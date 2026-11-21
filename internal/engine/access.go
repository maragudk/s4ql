package engine

import (
	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// How a statement with a WHERE or an ORDER BY reads the rows of its table. The engine picks the
// access by fixed rules, [chooseAccess], from the table, its indexes, the conditions, the ORDER BY,
// and whether there is a LIMIT, alone: it has no statistics, so the choice is the same on every
// instance and for every state of the rows. The filter above the scan tests every condition on every
// row that the access gives, so an access only leaves rows out, and the rows of a statement do not
// depend on the choice. The order of the rows of a query with ORDER BY does not depend on it either,
// but for the rows that tie: an access either delivers the order, by the key that it reads in, or
// the storage sorts its rows, as [ordering] says.

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
	// backward makes the scan give its rows in the reverse of the order of the key, and, for an access
	// whose rows are sorted, the rows that tie in the reverse of the order of the primary key. The
	// rules set it for an access that delivers the order of an ORDER BY, to the direction that
	// delivers it, and not otherwise: see [reading].
	backward bool
	// order is how the rows come in the order of the ORDER BY of the statement.
	order ordering
	// by holds the columns of the ORDER BY, for an access of the ordering sorted, and is nil for every
	// other.
	by []core.KeyColumn
}

// ordering is how the rows of an [access] come in the order of the ORDER BY of their query.
type ordering int

const (
	// unordered is the ordering of a statement without ORDER BY: its rows come in the order of the
	// key of its access, or in its reverse, as the engine draws for each query.
	unordered ordering = iota
	// delivered is the ordering of an access whose key holds the rows in the order of the ORDER BY,
	// forward or backward, as [delivers] says: the scan reads them in that direction, nothing sorts,
	// and the rows that tie come in the order in which the key holds them. An access of no row, and of
	// the one row of a primary key, delivers every order.
	delivered
	// sorted is the ordering of an access whose rows the storage sorts, by the columns of the ORDER
	// BY, as [core.IndexScan] says for its Order: it reads every row of the access before it gives the
	// first. The rows that tie come in the order of the primary key, or in its reverse, as the engine
	// draws for each query.
	sorted
)

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
	return core.IndexScan{Index: a.index, Equal: a.equal, Lower: a.lower.core(), Upper: a.upper.core(), Backward: a.backward, Order: a.by}
}

// orderBy is the ORDER BY and the LIMIT of a query, as the rules of the access read them. The zero
// value is a statement without either, as an update and a delete are.
type orderBy struct {
	// columns of the ORDER BY, in the order of the text: the position of each in the table, and
	// whether it is descending. It is nil for a statement without ORDER BY.
	columns []core.KeyColumn
	// limited is whether the query has a LIMIT.
	limited bool
}

// delivers reports whether a scan in the order of the key, of an index or of the primary key of the
// table t, as [core.IndexKey] gives it, gives the rows that pass a filter of the conditions in the
// order of the columns by, of an ORDER BY, and whether it does so backward, in the reverse of the
// order of the key.
//
// A column that the conditions pin, with = or with a test for NULL, as [pinOf] says, has one value
// in every row that passes the filter, so it orders nothing, wherever it is: the rule takes the
// pinned columns out of the key and out of the ORDER BY. The key delivers the order if what is then
// left of the ORDER BY is the start of what is left of the key, column for column, with either every
// column in the direction that the key holds it in, which a scan forward gives, or every column in
// the other direction, which a scan backward gives. So:
//
//   - An ORDER BY that ends before the key does is delivered. One of pinned columns alone is
//     delivered by every key, forward.
//   - An ORDER BY that names a column that the key does not have next is not delivered, also where
//     the columns before it hold the whole primary key, after which no column orders anything.
//   - An ORDER BY with one column in the direction of the key and one in the other is not delivered.
//
// The order of NULL needs no rule of its own. A key holds NULL before every value in a column that
// it holds ascending, and after every value in one that it holds descending, which is where ORDER BY
// puts NULL for ASC and for DESC, and a scan backward turns both round together.
//
// Whether the scan pins the pinned columns itself does not matter: the filter drops every row with
// another value in them, and the rows that are left are in the order of the rest of the key.
func delivers(t *s4qlpb.CreateTable, key []core.KeyColumn, conditions []condition, by []core.KeyColumn) (ok, backward bool) {
	// at is the place in the key of the next column that the ORDER BY must name, and matched whether a
	// column has fixed the direction.
	at, matched := 0, false
	for _, c := range by {
		if pinOf(t, c.Position, conditions) != nil {
			continue
		}
		for at < len(key) && pinOf(t, key[at].Position, conditions) != nil {
			at++
		}
		if at == len(key) || key[at].Position != c.Position {
			return false, false
		}
		turned := key[at].Descending != c.Descending
		if matched && turned != backward {
			return false, false
		}
		backward, matched = turned, true
		at++
	}
	return true, backward
}

// place the rows of the access a, a scan by the key of an index or of the primary key of the table t
// for a filter of the conditions, in the order of o: delivered, in the direction that delivers it,
// if the key delivers the order, as [delivers] says, and sorted otherwise. A statement without ORDER
// BY keeps a as it is.
func (o orderBy) place(t *s4qlpb.CreateTable, key []core.KeyColumn, conditions []condition, a access) access {
	if o.columns == nil {
		return a
	}
	if ok, backward := delivers(t, key, conditions, o.columns); ok {
		a.order, a.backward = delivered, backward
		return a
	}
	return o.sort(a)
}

// sort the rows of the access a by the columns of o, whatever its key delivers. An access of no row
// and of the one row of a primary key has nothing to sort, and delivers the order. A statement
// without ORDER BY keeps a as it is.
func (o orderBy) sort(a access) access {
	switch {
	case o.columns == nil:
	case a.none || a.point != nil:
		a.order = delivered
	default:
		a.order, a.by, a.backward = sorted, o.columns, false
	}
	return a
}

// better reports whether the access a of a candidate wins over the access best of the candidates
// before it, both fitted to the same conditions and placed in the order of o, as rule 3 of
// [chooseAccess] says.
func (o orderBy) better(a, best access) bool {
	if len(a.equal) != len(best.equal) {
		return len(a.equal) > len(best.equal)
	}
	byOrder := (a.order == delivered) != (best.order == delivered)
	byRange := a.ranged() != best.ranged()
	switch {
	case o.limited && byOrder, !byRange && byOrder:
		return a.order == delivered
	case byRange:
		return a.ranged()
	}
	return false
}

// chooseAccess to the rows of the table t, which has the indexes, in the order in which they were
// created, for a filter of the conditions, and for the ORDER BY and the LIMIT of o. These are the
// rules, and the first that holds decides:
//
//  1. No row, if a condition is never true: a comparison with NULL, and a test for NULL of a column
//     of the primary key.
//  2. The one row of a primary key, if a condition with = pins every column of the primary key.
//  3. Otherwise each candidate is fitted to the conditions, as [fit] says, and placed in the order
//     of the ORDER BY, as [orderBy.place] says: the primary key, and each index, whose key is its
//     columns and then the columns of the primary key that it does not hold, as [core.IndexKey]
//     gives it. The candidate that pins the most leading columns of its key wins. Of those that pin
//     as many:
//     - for a statement without ORDER BY, one with a bound on the next column wins over one without;
//     - for a query with ORDER BY and LIMIT, one that delivers the order wins over one that does not,
//     since it reads only up to the last row that the query gives, and of those that are the same in
//     that, one with a bound wins over one without;
//     - for a query with ORDER BY and without LIMIT, one with a bound wins over one without, since
//     the query reads every row of its access, and a bound leaves rows out, and of those that are
//     the same in that, one that delivers the order wins over one that does not, since it saves the
//     sort.
//     Of those that are still the same, the primary key wins, and then the index that was created
//     first.
//  4. Every row, in the order of the primary key, if the winner pins nothing, bounds nothing, and is
//     the primary key. An index wins over the primary key by pinning more, by a bound, or by
//     delivering the order: with nothing pinned and nothing bounded, an index that delivers the
//     order wins over the scan of every row with a sort.
//
// The rows of the winner are sorted, as [ordering] says, unless it delivers the order.
//
// Rules 1 and 2 do not depend on the indexes: the access is no row, or the row of a primary key, for
// no indexes exactly when it is for any. They deliver every order. The values of the conditions are
// of the types of their columns, as [checkType] checks them.
func chooseAccess(t *s4qlpb.CreateTable, indexes []*s4qlpb.CreateIndex, conditions []condition, o orderBy, b bugs) (access, error) {
	if never(t, conditions) {
		return o.sort(access{none: true}), nil
	}
	if point := pointOf(t, conditions); point != nil {
		return o.sort(access{point: point}), nil
	}
	best, err := fit(t, "", primaryKey(t), conditions, b)
	if err != nil {
		return access{}, err
	}
	best = o.place(t, core.IndexKey(t, nil), conditions, best)
	for _, ix := range indexes {
		a, err := fit(t, ix.GetIndex(), indexKey(t, ix), conditions, b)
		if err != nil {
			return access{}, err
		}
		if a = o.place(t, core.IndexKey(t, ix), conditions, a); o.better(a, best) {
			best = a
		}
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
// condition with = for each of its columns, in key order, as [pinOf] gives it, or nil if some column
// has none. No column of the primary key takes NULL, so a test for NULL pins none of them.
func pointOf(t *s4qlpb.CreateTable, conditions []condition) []*s4qlpb.Value {
	var point []*s4qlpb.Value
	for _, position := range t.GetPrimaryKey() {
		v := pinOf(t, int(position), conditions)
		if v == nil {
			return nil
		}
		point = append(point, v)
	}
	return point
}

// pinOf the column at the position of the table t by the conditions: the value of the first
// condition, in the order of the text, that pins it, which is one with =, or a test for NULL of a
// column that takes NULL, whose value is NULL; or nil if no condition pins it. No row has NULL in a
// column that does not take it, and the storage takes no scan for one, so a test for NULL of such a
// column pins nothing.
func pinOf(t *s4qlpb.CreateTable, position int, conditions []condition) *s4qlpb.Value {
	for _, c := range conditions {
		if c.position != position {
			continue
		}
		switch {
		case c.op == stmt.OpEqual:
			return c.value
		case c.op == stmt.OpIsNull && t.GetColumns()[position].GetNullable():
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
// positions of the columns of t, in the order of [core.IndexKey], which defines it.
func indexKey(t *s4qlpb.CreateTable, ix *s4qlpb.CreateIndex) []int {
	var key []int
	for _, c := range core.IndexKey(t, ix) {
		key = append(key, c.Position)
	}
	return key
}

// fit the conditions, none of which is never true, to the key of the index of the table t with the
// name, or of the primary key, if it is empty: the access that pins each leading column of the key
// that a condition pins, with = or with a test for NULL, as [pinOf] says, and that bounds the first
// column that none pins by the tightest bounds of the conditions that compare it with <, <=, >, or
// >=. It never sets none or point. The bugs b are those planted in it.
func fit(t *s4qlpb.CreateTable, index string, key []int, conditions []condition, b bugs) (access, error) {
	a := access{index: index}
	for i, position := range key {
		pinned := position
		if b.pinByPlace {
			// The planted bug: the pin of a column of the key is looked for among the conditions on the
			// column of the table at its place in the key.
			pinned = i
		}
		if v := pinOf(t, pinned, conditions); v != nil {
			a.equal = append(a.equal, v)
			continue
		}
		var err error
		for _, c := range conditions {
			if c.position != position {
				continue
			}
			op := c.op
			if b.unturnedBound && c.valueFirst {
				// The planted bug: a condition with its value on the left bounds its column by the operator
				// as the text writes it, as if the condition had not been turned round.
				op = op.Swapped()
			}
			switch op {
			case stmt.OpLess, stmt.OpLessOrEqual:
				a.upper, err = tighter(a.upper, bound{value: c.value, open: op == stmt.OpLess}, true)
			case stmt.OpGreater, stmt.OpGreaterOrEqual:
				a.lower, err = tighter(a.lower, bound{value: c.value, open: op == stmt.OpGreater}, false)
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
	// it is directionDrawn. An access that delivers the order of an ORDER BY reads in the direction
	// that delivers it, whatever the path says, and for one whose rows are sorted, the direction is
	// that of the rows that tie.
	direction direction
	// sort makes the storage sort the rows of every query with ORDER BY, also where the key of the
	// access delivers the order. Without it, a forced access delivers the order if its key does, as
	// [delivers] says, and is sorted otherwise. With the kind pathRules, the access is the one that
	// the rules choose for the statement without its ORDER BY.
	sort bool
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
	// directionDrawn leaves the direction to the engine, which draws it for a query.
	directionDrawn direction = iota
	// directionForward reads in the order of the key.
	directionForward
	// directionBackward reads in the reverse of the order of the key.
	directionBackward
)

// forcedAccess to the rows of the table t for a filter of the conditions and the ORDER BY of o, by the
// path p, whose kind is pathPrimary or pathIndex, and whether p can serve the table: an index of
// another table, or of none, cannot. It reads no row for conditions of which one is never true, as
// the rules do, since the storage takes no such scan. Its rows come in the order of o as
// [path.place] says.
func forcedAccess(t *s4qlpb.CreateTable, indexes []*s4qlpb.CreateIndex, conditions []condition, o orderBy, p path, b bugs) (access, bool, error) {
	name, key, ordered := "", primaryKey(t), core.IndexKey(t, nil)
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
		name, key, ordered = found.GetIndex(), indexKey(t, found), core.IndexKey(t, found)
	}
	if never(t, conditions) {
		return o.sort(access{none: true}), true, nil
	}
	a, err := fit(t, name, key, conditions, b)
	if err != nil {
		return access{}, false, err
	}
	if p.limited && len(a.equal) > p.pins {
		a.equal, a.lower, a.upper = a.equal[:max(p.pins, 0)], bound{}, bound{}
	}
	return p.place(o, t, ordered, conditions, a), true, nil
}

// place the rows of the access a, which the path p forces, a scan by the key, in the order of o:
// sorted, if the path says so, and otherwise as [orderBy.place] says, delivered where the key
// delivers the order.
func (p path) place(o orderBy, t *s4qlpb.CreateTable, key []core.KeyColumn, conditions []condition, a access) access {
	if p.sort {
		return o.sort(a)
	}
	return o.place(t, key, conditions, a)
}

// reading is how one statement reads the rows of its table: by the path that the tests force, if
// any, and backward or forward. The engine draws backward for each query, so that the order of the
// rows that SQLite leaves unspecified varies from one query to the next, for every access: of all
// the rows of a select without ORDER BY, whose scan reads backward then, and of the rows that tie
// under an ORDER BY whose rows are sorted, which come in the reverse of the order of the primary
// key then. A query whose access delivers the order of its ORDER BY reads in the direction that
// delivers it, and its rows that tie come as the key of the access holds them: the draw decides
// nothing for it. An update and a delete read forward, since the order in which they find their rows
// shows nowhere.
type reading struct {
	path     path
	backward bool
	// drawn is whether the engine drew backward, which it does for a query whose path forces no
	// direction.
	drawn bool
	// bugs planted in the choice of the access, for the tests of this package.
	bugs bugs
}
