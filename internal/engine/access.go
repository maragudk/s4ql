package engine

import (
	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// How a statement with a WHERE reads the rows of its table. The engine picks the access by fixed
// rules, [chooseAccess], from the table, its indexes, and the conditions alone: it has no statistics,
// so the choice is the same on every instance and for every state of the rows. The filter above the
// scan tests every condition on every row that the access gives, so an access only leaves rows out,
// and the rows of a statement do not depend on the choice.

// access of a [scan] to the rows of its table. The zero value reads every row, in the order of the
// primary key.
type access struct {
	// none reads no row, because a condition of the filter is never true.
	none bool
	// point is the primary key of the one row to read, by a get, if it is not nil.
	point []*s4qlpb.Value
	// index is the name of the index that the scan reads by, or empty for the primary key.
	index string
	// equal holds the values of the leading columns of the key of the index, or of the primary key,
	// that the scan pins, in the order of the key. A NULL pins a column to NULL.
	equal []*s4qlpb.Value
	// lower and upper bound the column of the key after the pinned ones.
	lower, upper bound
	// backward makes the scan give its rows in the reverse of the order of the key. The rules do not
	// set it: see [reading].
	backward bool
}

// ranged reports whether the access bounds the column after its pinned ones.
func (a access) ranged() bool {
	return a.lower.value != nil || a.upper.value != nil
}

// full reports whether the access reads every row of the table.
func (a access) full() bool {
	return !a.none && a.point == nil && a.index == "" && len(a.equal) == 0 && !a.ranged()
}

// scan of the access, which is neither none nor a point, as the storage reads it.
func (a access) scan() core.IndexScan {
	return core.IndexScan{Index: a.index, Equal: a.equal, Lower: a.lower.core(), Upper: a.upper.core(), Backward: a.backward}
}

// chooseAccess to the rows of the table t, which has the indexes, in the order in which they were
// created, for a filter of the conditions. These are the rules, and the first that holds decides:
//
//  1. No row, if a condition is never true: a comparison with NULL, and a test for NULL of a column
//     of the primary key.
//  2. The one row of a primary key, if a condition with = pins every column of the primary key.
//  3. Otherwise each candidate is fitted to the conditions, as [fit] says: the primary key, and each
//     index, whose key is its columns and then the columns of the primary key that it does not
//     hold, as [indexKey] gives it. The candidate that pins the most leading columns of its key
//     wins, and of those that pin as many, one with a bound on the next column wins over one
//     without. Of those that are still the same, the primary key wins, and then the index that was
//     created first.
//  4. Every row, if the winner pins nothing and bounds nothing.
//
// The access for no row and for the row of a primary key does not depend on the indexes, so a caller
// that gets one of them for no indexes need not read the indexes of the table. The values of the
// conditions are of the types of their columns, as [checkType] checks them.
func chooseAccess(t *s4qlpb.CreateTable, indexes []*s4qlpb.CreateIndex, conditions []condition) (access, error) {
	if never(t, conditions) {
		return access{none: true}, nil
	}
	if point := pointOf(t, conditions); point != nil {
		return access{point: point}, nil
	}
	best, err := fit(t, "", primaryKey(t), conditions)
	if err != nil {
		return access{}, err
	}
	for _, ix := range indexes {
		a, err := fit(t, ix.GetIndex(), indexKey(t, ix), conditions)
		if err != nil {
			return access{}, err
		}
		if len(a.equal) > len(best.equal) || len(a.equal) == len(best.equal) && a.ranged() && !best.ranged() {
			best = a
		}
	}
	if len(best.equal) == 0 && !best.ranged() {
		return access{}, nil
	}
	return best, nil
}

// never reports whether a condition is never true of a row of the table t, as the rules know it: a
// comparison with NULL, which is neither true nor false, or a test for NULL of a column of the
// primary key. A test for NULL of another column that does not take NULL is never true either, and
// is left to the filter: it pins nothing, as [pinOf] says, so it changes no access.
func never(t *s4qlpb.CreateTable, conditions []condition) bool {
	inKey := make([]bool, len(t.GetColumns()))
	for _, position := range t.GetPrimaryKey() {
		inKey[position] = true
	}
	for _, c := range conditions {
		switch c.op {
		case stmt.OpIsNull:
			if inKey[c.position] {
				return true
			}
		case stmt.OpIsNotNull:
		default:
			if c.value.HasNull() {
				return true
			}
		}
	}
	return false
}

// pointOf the conditions on the table t: the primary key that they pin, with the value of the first
// condition with = for each of its columns, in key order, or nil if some column has none.
func pointOf(t *s4qlpb.CreateTable, conditions []condition) []*s4qlpb.Value {
	var point []*s4qlpb.Value
	for _, position := range t.GetPrimaryKey() {
		v := pinOf(t, int(position), conditions, false)
		if v == nil {
			return nil
		}
		point = append(point, v)
	}
	return point
}

// pinOf the column at the position of the table t by the conditions: the value of the first
// condition, in the order of the text, that pins it, which is one with =, or, if byNull, a test for
// NULL of a column that takes NULL, whose value is NULL; or nil if no condition pins it. No row has
// NULL in a column that does not take it, and the storage takes no scan for one, so a test for NULL
// of such a column pins nothing.
func pinOf(t *s4qlpb.CreateTable, position int, conditions []condition, byNull bool) *s4qlpb.Value {
	for _, c := range conditions {
		if c.position != position {
			continue
		}
		switch {
		case c.op == stmt.OpEqual:
			return c.value
		case c.op == stmt.OpIsNull && byNull && t.GetColumns()[position].GetNullable():
			return null()
		}
	}
	return nil
}

// primaryKey of the table t: the positions of its columns, in key order.
func primaryKey(t *s4qlpb.CreateTable) []int {
	key := make([]int, len(t.GetPrimaryKey()))
	for i, position := range t.GetPrimaryKey() {
		key[i] = int(position)
	}
	return key
}

// indexKey is the key that a scan by the index ix of the table t reads in the order of, as
// positions of the columns of t: the columns of the index, and then the columns of the primary key
// that the index does not hold, in key order, as SQLite stores an index of a table without a rowid.
func indexKey(t *s4qlpb.CreateTable, ix *s4qlpb.CreateIndex) []int {
	var key []int
	held := make([]bool, len(t.GetColumns()))
	for _, c := range ix.GetColumns() {
		key = append(key, int(c.GetColumn()))
		held[c.GetColumn()] = true
	}
	for _, position := range t.GetPrimaryKey() {
		if !held[position] {
			key = append(key, int(position))
		}
	}
	return key
}

// fit the conditions, none of which is never true, to the key of the index of the table t with the
// name, or of the primary key, if it is empty: the access that pins each leading column of the key that a condition
// pins, with = or with a test for NULL, as [pinOf] says, and that bounds the first column that none
// pins by the tightest bounds of the conditions that compare it with <, <=, >, or >=. It never sets
// none or point.
func fit(t *s4qlpb.CreateTable, index string, key []int, conditions []condition) (access, error) {
	a := access{index: index}
	for _, position := range key {
		if v := pinOf(t, position, conditions, true); v != nil {
			a.equal = append(a.equal, v)
			continue
		}
		var err error
		for _, c := range conditions {
			if c.position != position {
				continue
			}
			switch c.op {
			case stmt.OpLess, stmt.OpLessOrEqual:
				a.upper, err = tighter(a.upper, bound{value: c.value, open: c.op == stmt.OpLess}, true)
			case stmt.OpGreater, stmt.OpGreaterOrEqual:
				a.lower, err = tighter(a.lower, bound{value: c.value, open: c.op == stmt.OpGreater}, false)
			}
			if err != nil {
				return access{}, err
			}
		}
		break
	}
	return a, nil
}

// tighter of the bounds a and b, which are both upper bounds if upper, and both lower bounds
// otherwise: the one that leaves out more values. b has a value, and a may have none.
func tighter(a, b bound, upper bool) (bound, error) {
	if a.value == nil {
		return b, nil
	}
	n, err := compare(b.value, a.value)
	if err != nil {
		return bound{}, err
	}
	if upper {
		n = -n
	}
	if n > 0 || n == 0 && b.open {
		return b, nil
	}
	return a, nil
}

// path that the tests of this package force every statement to read its rows by, to show that
// every access that can serve a statement gives the same rows. The zero value forces nothing: the
// access is the one of [chooseAccess], and the direction of a query is drawn.
type path struct {
	// kind of the access that is forced.
	kind pathKind
	// index is the name of the index of a path of the kind pathIndex. A statement on a table that has
	// no index of the name reads as the rules say.
	index string
	// limited makes the access pin at most pins leading columns of the key, and then bound nothing
	// unless the conditions pin no more than that, so that every prefix of the key that can serve a
	// statement can be forced.
	limited bool
	pins    int
	// direction of the scan, which is drawn for a query, and forward for an update and a delete, if
	// it is directionDrawn.
	direction direction
}

// pathKind is the kind of access that a [path] forces.
type pathKind int

const (
	// pathRules forces no access.
	pathRules pathKind = iota
	// pathFull reads every row in the order of the primary key, and tests no condition for being
	// never true.
	pathFull
	// pathPrimary scans by the primary key, as [fit] fits the conditions to it, also where the rules
	// read one row by a get.
	pathPrimary
	// pathIndex scans by the index of the name, as [fit] fits the conditions to its key.
	pathIndex
)

// direction of a scan that a [path] forces.
type direction int

const (
	directionDrawn direction = iota
	directionForward
	directionBackward
)

// forcedAccess to the rows of the table t for a filter of the conditions, by the path p, whose kind
// is pathPrimary or pathIndex, and whether p can serve the table: an index of another table, or of
// none, cannot. It reads no row for conditions of which one is never true, as the rules do, since
// the storage takes no such scan.
func forcedAccess(t *s4qlpb.CreateTable, indexes []*s4qlpb.CreateIndex, conditions []condition, p path) (access, bool, error) {
	name, key := "", primaryKey(t)
	if p.kind == pathIndex {
		var found *s4qlpb.CreateIndex
		for _, ix := range indexes {
			if fold(ix.GetIndex()) == fold(p.index) {
				found = ix
			}
		}
		if found == nil {
			return access{}, false, nil
		}
		name, key = found.GetIndex(), indexKey(t, found)
	}
	if never(t, conditions) {
		return access{none: true}, true, nil
	}
	a, err := fit(t, name, key, conditions)
	if err != nil {
		return access{}, false, err
	}
	if p.limited && len(a.equal) > p.pins {
		a.equal, a.lower, a.upper = a.equal[:max(p.pins, 0)], bound{}, bound{}
	}
	return a, true, nil
}

// reading is how one statement reads the rows of its table: by the path that the tests force, if
// any, and backward or forward. The engine draws backward for each query, so that the order of the
// rows of a select without ORDER BY, which SQLite leaves unspecified, varies from one query to the
// next, for every access. An update and a delete read forward, since the order in which they find
// their rows shows nowhere.
type reading struct {
	path     path
	backward bool
}
