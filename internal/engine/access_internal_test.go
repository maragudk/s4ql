package engine

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// accessTable of a test of the rules: a table t of the columns, each as its name, its type, and "?"
// after it if it takes NULL, with the primary key of the columns at the positions key.
func accessTable(key []uint32, columns ...string) *s4qlpb.CreateTable {
	ct := s4qlpb.CreateTable_builder{Table: new("t"), PrimaryKey: key}
	types := map[string]s4qlpb.Type{"int": s4qlpb.Type_TYPE_INT, "text": s4qlpb.Type_TYPE_TEXT, "real": s4qlpb.Type_TYPE_REAL, "blob": s4qlpb.Type_TYPE_BLOB}
	for _, c := range columns {
		name, typ, _ := strings.Cut(c, " ")
		column := s4qlpb.Column_builder{Name: new(name), Type: new(types[strings.TrimSuffix(typ, "?")])}
		if strings.HasSuffix(typ, "?") {
			column.Nullable = new(true)
		}
		ct.Columns = append(ct.Columns, column.Build())
	}
	return ct.Build()
}

// accessIndex of the name on the columns of ct that columns names, each with " desc" after it for a
// column in descending order.
func accessIndex(ct *s4qlpb.CreateTable, name string, columns ...string) *s4qlpb.CreateIndex {
	ci := s4qlpb.CreateIndex_builder{Index: new(name), Table: new(ct.GetTable())}
	for _, c := range columns {
		name, desc := strings.CutSuffix(c, " desc")
		column := s4qlpb.IndexColumn_builder{Column: new(uint32(accessPosition(ct, name)))}
		if desc {
			column.Descending = new(true)
		}
		ci.Columns = append(ci.Columns, column.Build())
	}
	return ci.Build()
}

func accessPosition(ct *s4qlpb.CreateTable, name string) int {
	for i, c := range ct.GetColumns() {
		if c.GetName() == name {
			return i
		}
	}
	panic("no column " + name)
}

// accessConditions of a text such as "a = 1 and b is null and c > 'x'": conditions on the columns of
// ct, with integers, texts in single quotes, reals with a point, and null for values.
func accessConditions(ct *s4qlpb.CreateTable, where string) []condition {
	ops := map[string]stmt.Operator{"=": stmt.OpEqual, "<>": stmt.OpNotEqual, "<": stmt.OpLess, "<=": stmt.OpLessOrEqual, ">": stmt.OpGreater, ">=": stmt.OpGreaterOrEqual}
	var conditions []condition
	for term := range strings.SplitSeq(where, " and ") {
		words := strings.Fields(term)
		c := condition{position: accessPosition(ct, words[0])}
		switch {
		case strings.HasSuffix(term, " is null"):
			c.op = stmt.OpIsNull
		case strings.HasSuffix(term, " is not null"):
			c.op = stmt.OpIsNotNull
		default:
			c.op = ops[words[1]]
			switch v := words[2]; {
			case v == "null":
				c.value = null()
			case strings.HasPrefix(v, "'"):
				c.value = s4qlpb.Value_builder{Text: new(strings.Trim(v, "'"))}.Build()
			case strings.Contains(v, "."):
				var r float64
				if _, err := fmt.Sscan(v, &r); err != nil {
					panic(err)
				}
				c.value = s4qlpb.Value_builder{Real: new(r)}.Build()
			default:
				var n int64
				if _, err := fmt.Sscan(v, &n); err != nil {
					panic(err)
				}
				c.value = s4qlpb.Value_builder{Integer: new(n)}.Build()
			}
		}
		conditions = append(conditions, c)
	}
	return conditions
}

// accessText of the access a, for the expectations of the tests: "none", "point" and the key, "full",
// or "primary" or the name of the index, "=" and the pinned values, and each bound.
func accessText(a access) string {
	value := func(v *s4qlpb.Value) string {
		switch v.WhichKind() {
		case s4qlpb.Value_Integer_case:
			return fmt.Sprint(v.GetInteger())
		case s4qlpb.Value_Real_case:
			return fmt.Sprintf("%.1f", v.GetReal())
		case s4qlpb.Value_Text_case:
			return "'" + v.GetText() + "'"
		case s4qlpb.Value_Null_case:
			return "null"
		}
		return "?"
	}
	list := func(vs []*s4qlpb.Value) string {
		texts := make([]string, len(vs))
		for i, v := range vs {
			texts[i] = value(v)
		}
		return strings.Join(texts, ",")
	}
	switch {
	case a.none:
		return "none"
	case a.point != nil:
		return "point " + list(a.point)
	case a.full():
		return "full"
	}
	text := "primary"
	if a.index != "" {
		text = a.index
	}
	if len(a.equal) > 0 {
		text += " = " + list(a.equal)
	}
	if a.lower.value != nil {
		text += map[bool]string{true: " > ", false: " >= "}[a.lower.open] + value(a.lower.value)
	}
	if a.upper.value != nil {
		text += map[bool]string{true: " < ", false: " <= "}[a.upper.open] + value(a.upper.value)
	}
	return text
}

// turnedWhere is the WHERE where, of comparisons `column op value` and tests for NULL joined by " and
// ", with the comparisons for which turn reports true, by their number among the comparisons from 0,
// written `value op' column`, where op' compares with its sides swapped as op does.
func turnedWhere(where string, turn func(n int) bool) string {
	swapped := map[string]string{"=": "=", "==": "==", "!=": "!=", "<>": "<>", "<": ">", "<=": ">=", ">": "<", ">=": "<="}
	conditions := strings.Split(where, " and ")
	n := 0
	for i, c := range conditions {
		fields := strings.Fields(c)
		if len(fields) != 3 || fields[1] == "is" {
			continue
		}
		if swapped[fields[1]] == "" {
			panic("no operator " + fields[1] + " to turn round in " + where)
		}
		if turn(n) {
			conditions[i] = fields[2] + " " + swapped[fields[1]] + " " + fields[0]
		}
		n++
	}
	return strings.Join(conditions, " and ")
}

// textAccess of the select src, without parameters, on the table ct with the indexes, as the engine
// builds it from the statement, with the bugs b planted.
func textAccess(t *testing.T, ct *s4qlpb.CreateTable, indexes []*s4qlpb.CreateIndex, src string, b bugs) access {
	t.Helper()
	s, err := stmt.Check(src)
	is.NotError(t, err, src)
	built, err := buildQuery(t.Context(), tableOf{ct, indexes}, NewPlan(s).(*Query), nil, math.MaxInt, reading{bugs: b})
	is.NotError(t, err, src)
	scan := scanOf(built)
	is.True(t, scan != nil, src)
	return scan.access
}

func TestChooseAccess(t *testing.T) {
	// k is the primary key, a and e do not take NULL, and b, c, and d do.
	one := accessTable([]uint32{0}, "k int", "a int", "b text?", "c real?", "d int?", "e int")
	// The primary key is k1 and then k2.
	two := accessTable([]uint32{2, 0}, "k2 int", "a int", "k1 int", "b int?")
	// The table of goqite, with the columns that its receive compares.
	goqite := accessTable([]uint32{0}, "id text", "queue text", "timeout text", "received int", "priority int", "created text")

	tests := []struct {
		name    string
		table   *s4qlpb.CreateTable
		indexes [][]string
		where   string
		want    string
	}{
		{name: "should read every row for a condition on a column that is in no key", table: one, where: "a = 1", want: "full"},
		{name: "should read every row for a condition that an index has but not first", table: one, indexes: [][]string{{"i", "a", "e"}}, where: "e = 1", want: "full"},
		{name: "should read every row for <> on the first column of an index, which pins and bounds nothing", table: one, indexes: [][]string{{"i", "a"}}, where: "a <> 1", want: "full"},
		{name: "should read every row for IS NOT NULL on the first column of an index", table: one, indexes: [][]string{{"i", "b"}}, where: "b is not null", want: "full"},

		{name: "should read no row for a comparison with NULL", table: one, where: "a = null", want: "none"},
		{name: "should read no row for a comparison with NULL of any operator, also when the key is pinned", table: one, where: "k = 1 and b < null", want: "none"},
		{name: "should read no row for a comparison with NULL, also when an index fits", table: one, indexes: [][]string{{"i", "a"}}, where: "a = 1 and e <> null", want: "none"},
		{name: "should read no row for IS NULL of the primary key", table: one, where: "k is null", want: "none"},
		{name: "should read no row for IS NULL of a column of a primary key of two columns", table: two, where: "k1 = 1 and k2 is null", want: "none"},
		{name: "should read every row for IS NOT NULL of the primary key", table: one, where: "k is not null", want: "full"},

		{name: "should get the row of the primary key that = pins", table: one, where: "k = 1", want: "point 1"},
		{name: "should get the row of the first of two values that = pins the primary key to", table: one, where: "k = 2 and k = 1", want: "point 2"},
		{name: "should get the row of the primary key also when an index pins as much", table: one, indexes: [][]string{{"i", "a"}}, where: "a = 5 and k = 1", want: "point 1"},
		{name: "should get the row of the primary key also when an index pins more", table: one, indexes: [][]string{{"i", "a", "e"}}, where: "a = 5 and e = 6 and k = 1", want: "point 1"},
		{name: "should get the row of a primary key of two columns in key order", table: two, where: "k2 = 2 and k1 = 1", want: "point 1,2"},

		{name: "should scan the primary key from a lower bound", table: one, where: "k > 1", want: "primary > 1"},
		{name: "should scan the primary key to an upper bound", table: one, where: "k <= 9", want: "primary <= 9"},
		{name: "should scan the primary key between the tightest bounds, of which the open one is tighter at one value", table: one, where: "k >= 1 and k < 9 and k > 1 and k <= 9 and k < 12 and k > 0", want: "primary > 1 < 9"},
		{name: "should scan the primary key between bounds that hold no key as they are", table: one, where: "k > 5 and k < 3", want: "primary > 5 < 3"},
		{name: "should pin the first column of a primary key of two columns", table: two, where: "k1 = 1", want: "primary = 1"},
		{name: "should pin the first column of a primary key of two columns and bound the second", table: two, where: "k2 > 5 and k1 = 1 and k2 <= 8", want: "primary = 1 > 5 <= 8"},
		{name: "should read every row for a condition on the second column of a primary key alone", table: two, where: "k2 = 2", want: "full"},
		{name: "should bound the first column of a primary key of two columns, and not the second", table: two, where: "k1 > 1 and k2 > 2", want: "primary > 1"},

		{name: "should scan an index whose first column = pins", table: one, indexes: [][]string{{"i", "a"}}, where: "a = 1", want: "i = 1"},
		{name: "should pin a column of an index to the first of two values", table: one, indexes: [][]string{{"i", "a"}}, where: "a = 1 and a = 2", want: "i = 1"},
		{name: "should scan an index whose first column is bounded", table: one, indexes: [][]string{{"i", "a"}}, where: "a > 1 and a <= 7", want: "i > 1 <= 7"},
		{name: "should state the bounds of a descending column by value", table: one, indexes: [][]string{{"i", "a desc"}}, where: "a > 1 and a <= 7", want: "i > 1 <= 7"},
		{name: "should pin two columns of an index", table: one, indexes: [][]string{{"i", "a", "e"}}, where: "e = 2 and a = 1", want: "i = 1,2"},
		{name: "should pin the first column of an index and bound the second", table: one, indexes: [][]string{{"i", "a", "e desc"}}, where: "e < 2 and a = 1", want: "i = 1 < 2"},
		{name: "should not bound a column after one that is only bounded", table: one, indexes: [][]string{{"i", "a", "e"}}, where: "a > 1 and e > 2", want: "i > 1"},
		{name: "should not pin a column after one that is not pinned", table: one, indexes: [][]string{{"i", "a", "e"}}, where: "a > 1 and e = 2", want: "i > 1"},

		{name: "should bound the primary key at the end of the key of an index", table: one, indexes: [][]string{{"i", "a"}}, where: "a = 1 and k > 3", want: "i = 1 > 3"},
		{name: "should bound the first column of the primary key at the end of the key of an index", table: two, indexes: [][]string{{"i", "a"}}, where: "a = 1 and k1 >= 3 and k2 > 9", want: "i = 1 >= 3"},
		{name: "should pin into the columns of the primary key at the end of the key of an index", table: two, indexes: [][]string{{"i", "a"}}, where: "a = 1 and k1 = 3 and k2 > 9", want: "i = 1,3 > 9"},
		{name: "should not repeat a column of the primary key that the index holds", table: two, indexes: [][]string{{"i", "a", "k2"}}, where: "a = 1 and k2 = 2 and k1 < 4", want: "i = 1,2 < 4"},

		{name: "should pin a column that takes NULL with IS NULL", table: one, indexes: [][]string{{"i", "b"}}, where: "b is null", want: "i = null"},
		{name: "should pin with IS NULL and bound the next column", table: one, indexes: [][]string{{"i", "b", "c"}}, where: "c > 1.5 and b is null", want: "i = null > 1.5"},
		{name: "should pin with IS NULL and bound the primary key after it", table: one, indexes: [][]string{{"i", "b"}}, where: "b is null and k < 4", want: "i = null < 4"},
		{name: "should pin with the first of = and IS NULL", table: one, indexes: [][]string{{"i", "b"}}, where: "b is null and b = 'x'", want: "i = null"},
		{name: "should pin with = when it comes before IS NULL", table: one, indexes: [][]string{{"i", "b"}}, where: "b = 'x' and b is null", want: "i = 'x'"},
		{name: "should pin nothing with IS NULL of a column that does not take NULL, which no row has", table: one, indexes: [][]string{{"i", "a"}}, where: "a is null", want: "full"},
		{name: "should pin nothing with IS NULL of a column that does not take NULL after a pinned one", table: one, indexes: [][]string{{"i", "e", "a"}}, where: "e = 1 and a is null", want: "i = 1"},

		{name: "should take the candidate that pins the most columns", table: one, indexes: [][]string{{"i1", "a"}, {"i2", "e", "a"}}, where: "a = 1 and e = 2", want: "i2 = 2,1"},
		{name: "should take the candidate that pins the most columns also when it was created first", table: one, indexes: [][]string{{"i2", "e", "a"}, {"i1", "a"}}, where: "a = 1 and e = 2", want: "i2 = 2,1"},
		{name: "should take an index that pins one column over the primary key with a range", table: one, indexes: [][]string{{"i", "a"}}, where: "k > 1 and a = 1", want: "i = 1 > 1"},
		{name: "should take, of two that pin as many, the one with a range on the next column", table: one, indexes: [][]string{{"i1", "a"}, {"i2", "a", "e"}}, where: "a = 1 and e > 2", want: "i2 = 1 > 2"},
		{name: "should take, of two that pin as many, the one with a range also when it was created first", table: one, indexes: [][]string{{"i2", "a", "e"}, {"i1", "a"}}, where: "a = 1 and e > 2", want: "i2 = 1 > 2"},
		{name: "should take an index that pins more over a later one that pins less and bounds the next column", table: one, indexes: [][]string{{"i2", "a", "e"}, {"i1", "b"}}, where: "a = 1 and e = 2 and b > 'x'", want: "i2 = 1,2"},
		{name: "should take an index with a range over the primary key with nothing", table: one, indexes: [][]string{{"i", "a"}}, where: "a < 3", want: "i < 3"},
		{name: "should give a tie of ranges to the primary key", table: one, indexes: [][]string{{"i", "a"}}, where: "a < 3 and k < 4", want: "primary < 4"},
		{name: "should give a tie of pinned columns to the index that was created first", table: one, indexes: [][]string{{"i1", "a"}, {"i2", "a", "e"}}, where: "a = 1", want: "i1 = 1"},
		{name: "should give a tie of pinned columns to the index that was created first, whatever its columns", table: one, indexes: [][]string{{"i2", "a", "e"}, {"i1", "a"}}, where: "a = 1", want: "i2 = 1"},
		{name: "should give a tie of pinned columns and ranges to the index that was created first", table: one, indexes: [][]string{{"i1", "a", "e"}, {"i2", "a", "e desc"}}, where: "a = 1 and e >= 2", want: "i1 = 1 >= 2"},
		{name: "should give a tie of an index and the primary key of two columns to the primary key", table: two, indexes: [][]string{{"i", "k1"}}, where: "k1 = 2", want: "primary = 2"},
		{name: "should take an index that pins into the primary key at its end over the primary key, which pins less", table: two, indexes: [][]string{{"i", "a"}}, where: "a = 1 and k1 = 2", want: "i = 1,2"},

		{name: "should scan goqite's index by the queue for its receive without ORDER BY", table: goqite, indexes: [][]string{{"goqite_queue_priority_created_idx", "queue", "priority desc", "created"}},
			where: "queue = 'q' and timeout <= 't' and received < 3", want: "goqite_queue_priority_created_idx = 'q'"},
		{name: "should take an index on the queue and the timeout over goqite's for its receive without ORDER BY", table: goqite,
			indexes: [][]string{{"goqite_queue_priority_created_idx", "queue", "priority desc", "created"}, {"goqite_queue_timeout", "queue", "timeout"}},
			where:   "queue = 'q' and timeout <= 't' and received < 3", want: "goqite_queue_timeout = 'q' <= 't'"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var indexes []*s4qlpb.CreateIndex
			for _, ix := range test.indexes {
				indexes = append(indexes, accessIndex(test.table, ix[0], ix[1:]...))
			}
			conditions := accessConditions(test.table, test.where)

			a, err := chooseAccess(test.table, indexes, conditions, orderBy{}, bugs{})
			is.NotError(t, err)
			is.Equal(t, test.want, accessText(a))

			// The statement of the conditions chooses it too, also with every comparison written with its
			// value first, and with every second one so, which the statement pass reads as the conditions.
			for _, turn := range []func(int) bool{func(int) bool { return false }, func(int) bool { return true }, func(n int) bool { return n%2 == 1 }} {
				src := "select * from t where " + turnedWhere(test.where, turn)
				is.Equal(t, test.want, accessText(textAccess(t, test.table, indexes, src, bugs{})), src)
			}
			is.True(t, !a.backward, "the rules set no direction")

			// The first two rules do not depend on the indexes, which is why a statement that they decide
			// reads none.
			without, err := chooseAccess(test.table, nil, conditions, orderBy{}, bugs{})
			is.NotError(t, err)
			if a.none || a.point != nil || without.none || without.point != nil {
				is.Equal(t, accessText(a), accessText(without))
			}
		})
	}

	t.Run("should bound a column by a value on the left with the operator as the text writes it, with the planted bug unturnedBound, and by the other order as without it", func(t *testing.T) {
		planted := bugs{unturnedBound: true}
		ix := []*s4qlpb.CreateIndex{accessIndex(one, "i", "a", "e")}
		is.Equal(t, "primary < 5", accessText(textAccess(t, one, nil, "select * from t where 5 < k", planted)))
		is.Equal(t, "primary <= 9", accessText(textAccess(t, one, nil, "select * from t where 9 <= k", planted)))
		is.Equal(t, "i = 1 > 2 <= 7", accessText(textAccess(t, one, ix, "select * from t where 1 = a and 2 > e and e <= 7", planted)))
		is.Equal(t, "primary > 5", accessText(textAccess(t, one, nil, "select * from t where k > 5", planted)))
		is.Equal(t, "point 5", accessText(textAccess(t, one, nil, "select * from t where 5 = k", planted)))
	})

	t.Run("should fail with an internal error for two bounds of values of two types, which the check of the types refuses before", func(t *testing.T) {
		conditions := []condition{
			{position: 0, op: stmt.OpLess, value: s4qlpb.Value_builder{Integer: new(int64(1))}.Build()},
			{position: 0, op: stmt.OpLess, value: s4qlpb.Value_builder{Text: new("a")}.Build()},
		}

		_, err := chooseAccess(one, nil, conditions, orderBy{}, bugs{})
		is.True(t, errors.Is(err, ErrInternal))

		conditions[0].position, conditions[1].position = 1, 1
		_, err = chooseAccess(one, []*s4qlpb.CreateIndex{accessIndex(one, "i", "a")}, conditions, orderBy{}, bugs{})
		is.True(t, errors.Is(err, ErrInternal))
	})
}

func TestForcedAccess(t *testing.T) {
	one := accessTable([]uint32{0}, "k int", "a int", "b text?", "e int")
	indexes := []*s4qlpb.CreateIndex{accessIndex(one, "i", "a", "e"), accessIndex(one, "j", "b")}

	tests := []struct {
		name  string
		path  path
		where string
		want  string
		// other reports that the path cannot serve the table.
		other bool
	}{
		{name: "should scan the primary key with the key pinned where the rules get one row", path: path{kind: pathPrimary}, where: "k = 1", want: "primary = 1"},
		{name: "should scan the primary key with nothing where the rules take an index", path: path{kind: pathPrimary}, where: "a = 1", want: "full"},
		{name: "should scan the index of the name with what the conditions pin", path: path{kind: pathIndex, index: "I"}, where: "a = 1 and e = 2 and k > 3", want: "i = 1,2 > 3"},
		{name: "should scan the whole index of the name for conditions that it does not fit", path: path{kind: pathIndex, index: "j"}, where: "a = 1", want: "j"},
		{name: "should scan the index of the name where the rules get one row", path: path{kind: pathIndex, index: "i"}, where: "k = 1", want: "i"},
		{name: "should pin no more columns than the limit, and then bound nothing", path: path{kind: pathIndex, index: "i", limited: true, pins: 1}, where: "a = 1 and e = 2 and k > 3", want: "i = 1"},
		{name: "should pin nothing and bound nothing at the limit of no columns", path: path{kind: pathIndex, index: "i", limited: true}, where: "a = 1 and e > 2", want: "i"},
		{name: "should keep the bounds at a limit that the conditions do not pass", path: path{kind: pathIndex, index: "i", limited: true, pins: 1}, where: "a = 1 and e > 2", want: "i = 1 > 2"},
		{name: "should read no row for a condition that is never true", path: path{kind: pathIndex, index: "i"}, where: "a = 1 and e = null", want: "none"},
		{name: "should pin a column that takes NULL with IS NULL", path: path{kind: pathIndex, index: "j"}, where: "b is null and k <= 4", want: "j = null <= 4"},
		{name: "should not serve a table that has no index of the name", path: path{kind: pathIndex, index: "other"}, where: "a = 1", other: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, ok, err := forcedAccess(one, indexes, accessConditions(one, test.where), orderBy{}, test.path, bugs{})
			is.NotError(t, err)
			is.Equal(t, !test.other, ok)
			if ok {
				text := accessText(a)
				if a.full() && a.index == "" {
					text = "full"
				}
				is.Equal(t, test.want, text)
			}
		})
	}

	t.Run("should fail with an internal error for two bounds of values of two types, which the check of the types refuses before", func(t *testing.T) {
		conditions := []condition{
			{position: 1, op: stmt.OpGreater, value: s4qlpb.Value_builder{Integer: new(int64(1))}.Build()},
			{position: 1, op: stmt.OpGreater, value: s4qlpb.Value_builder{Text: new("a")}.Build()},
		}

		a, ok, err := forcedAccess(one, indexes, conditions, orderBy{}, path{kind: pathIndex, index: "i"}, bugs{})
		is.True(t, errors.Is(err, ErrInternal))
		is.True(t, !ok)
		is.True(t, a.index == "" && !a.ranged())
	})
}

// drawHost is a host that gives the numbers of draws, in turn, and counts them.
type drawHost struct {
	host.Host
	draws []uint64
	drawn int
}

func (h *drawHost) Uint64() uint64 {
	h.drawn++
	return h.draws[(h.drawn-1)%len(h.draws)]
}

func TestEngine_reading(t *testing.T) {
	directions := func(e *Engine, query bool, n int) string {
		var b strings.Builder
		for range n {
			b.WriteString(map[bool]string{false: "f", true: "b"}[e.reading(query).backward])
		}
		return b.String()
	}

	t.Run("should draw the direction of each query from the host, backward for an odd number, and draw nothing for a statement that is no query", func(t *testing.T) {
		h := &drawHost{draws: []uint64{2, 7, 1 << 63, 1<<63 + 1, 0}}
		e := newEngine(nil, h, Options{})

		is.Equal(t, "fbfbf", directions(e, true, 5))
		is.Equal(t, 5, h.drawn)
		is.Equal(t, "fffff", directions(e, false, 5))
		is.Equal(t, 5, h.drawn)
	})

	t.Run("should draw the directions from its seed, and nothing from the host, the same for the same seed, both ways, and forward for a statement that is no query", func(t *testing.T) {
		h := &drawHost{draws: []uint64{1}}
		first := directions(newEngine(nil, h, Options{Directions: new(uint64(3))}), true, 64)

		is.Equal(t, first, directions(newEngine(nil, h, Options{Directions: new(uint64(3))}), true, 64))
		is.True(t, first != directions(newEngine(nil, h, Options{Directions: new(uint64(4))}), true, 64))
		is.True(t, strings.Count(first, "f") >= 16 && strings.Count(first, "b") >= 16, first)
		is.Equal(t, 0, h.drawn)
		e := newEngine(nil, h, Options{Directions: new(uint64(3))})
		is.Equal(t, strings.Repeat("f", 8), directions(e, false, 8))
		is.Equal(t, first, directions(e, true, 64), "a statement that is no query takes no draw of the seed")
	})

	t.Run("should draw the direction of the n-th query of a seed by the low bit of the n-th number of splitmix64 from that seed", func(t *testing.T) {
		// splitmix64 as Sebastiano Vigna gives it, at https://prng.di.unimi.it/splitmix64.c.
		state := uint64(3)
		var want strings.Builder
		for range 64 {
			state += 0x9e3779b97f4a7c15
			z := state
			z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
			z = (z ^ (z >> 27)) * 0x94d049bb133111eb
			want.WriteString(map[uint64]string{0: "f", 1: "b"}[(z^(z>>31))&1])
		}

		is.Equal(t, want.String(), directions(newEngine(nil, &drawHost{draws: []uint64{1}}, Options{Directions: new(uint64(3))}), true, 64))
	})

	t.Run("should take the direction of a path that has one, for a query and for another statement, and draw nothing", func(t *testing.T) {
		h := &drawHost{draws: []uint64{1}}
		e := newEngine(nil, h, Options{path: path{direction: directionBackward}})

		is.Equal(t, "bbbb", directions(e, true, 2)+directions(e, false, 2))
		e.path.direction = directionForward
		is.Equal(t, "ffff", directions(e, true, 2)+directions(e, false, 2))
		is.Equal(t, 0, h.drawn)
		is.Equal(t, pathRules, e.reading(true).path.kind)
	})
}

// accessOrder of the ORDER BY by, such as "a desc, k", on the table: its columns, as the rules read
// them.
func accessOrder(ct *s4qlpb.CreateTable, by string) []core.KeyColumn {
	var columns []core.KeyColumn
	for term := range strings.SplitSeq(by, ", ") {
		name, desc := strings.CutSuffix(term, " desc")
		columns = append(columns, core.KeyColumn{Position: accessPosition(ct, name), Descending: desc})
	}
	return columns
}

// orderedText is [accessText] of the access a, and how its rows come in the order of an ORDER BY:
// "delivered forward", "delivered backward", or "sorted", and nothing for an access without an
// order.
func orderedText(a access) string {
	text := accessText(a)
	switch a.order {
	case delivered:
		text += " delivered " + map[bool]string{false: "forward", true: "backward"}[a.backward]
	case sorted:
		text += " sorted"
		if a.backward {
			text += " backward"
		}
	}
	if (a.order == sorted) != (a.by != nil) {
		text += " with the wrong columns to sort by"
	}
	return text
}

func TestDelivers(t *testing.T) {
	// The table one has the key k. The table two has the key k1, k2. The table goqite is goqite's.
	one := accessTable([]uint32{0}, "k int", "a int", "b text?", "c real?", "d blob")
	two := accessTable([]uint32{0, 1}, "k1 int", "k2 int", "a int", "b text?")
	goqite := accessTable([]uint32{0}, "id text", "created text", "updated text", "queue text", "body blob", "timeout text", "received int", "priority int")
	const receive = "queue = 'q' and timeout <= 'now' and received < 3"

	tests := []struct {
		name  string
		table *s4qlpb.CreateTable
		// index holds the columns of the index, and is nil for the primary key.
		index []string
		where string
		by    string
		// want is "forward", "backward", or "no".
		want string
	}{
		{name: "the primary key in its order", table: one, by: "k", want: "forward"},
		{name: "the primary key in its reverse", table: one, by: "k desc", want: "backward"},
		{name: "a column that the primary key does not have", table: one, by: "a", want: "no"},
		{name: "a column after the whole primary key, which orders nothing and is still not delivered", table: one, by: "k, a", want: "no"},
		{name: "a pinned column alone", table: one, where: "a = 1", by: "a", want: "forward"},
		{name: "a pinned column alone, descending, which has no direction", table: one, where: "a = 1", by: "a desc", want: "forward"},
		{name: "a pinned column before the key", table: one, where: "a = 1", by: "a, k desc", want: "backward"},
		{name: "a pinned column after the key", table: one, where: "a = 1", by: "k, a", want: "forward"},
		{name: "a pinned column whose direction is the other than that of the key", table: one, where: "a = 1", by: "a desc, k", want: "forward"},
		{name: "a bounded column, which is not pinned", table: one, where: "a > 1", by: "a", want: "no"},
		{name: "a column that IS NOT NULL tests, which is not pinned", table: one, where: "b is not null", by: "b, k", want: "no"},
		{name: "a column of a test for NULL that takes none, which is not pinned", table: one, where: "a is null", by: "a, k", want: "no"},
		{name: "a column pinned to NULL", table: one, where: "b is null", by: "b, k", want: "forward"},

		{name: "an index in its order", table: one, index: []string{"a"}, by: "a", want: "forward"},
		{name: "an index in its reverse", table: one, index: []string{"a"}, by: "a desc", want: "backward"},
		{name: "an index and the primary key at its end", table: one, index: []string{"a"}, by: "a, k", want: "forward"},
		{name: "an index and the primary key at its end, both turned", table: one, index: []string{"a"}, by: "a desc, k desc", want: "backward"},
		{name: "an index with the primary key turned alone", table: one, index: []string{"a"}, by: "a, k desc", want: "no"},
		{name: "an index turned with the primary key not", table: one, index: []string{"a"}, by: "a desc, k", want: "no"},
		{name: "the primary key by an index whose column is not pinned", table: one, index: []string{"a"}, by: "k", want: "no"},
		{name: "the primary key by an index whose column is pinned", table: one, index: []string{"a"}, where: "a = 1", by: "k", want: "forward"},
		{name: "the primary key in reverse by an index whose column is pinned", table: one, index: []string{"a"}, where: "a = 1", by: "k desc", want: "backward"},
		{name: "the pinned column of an index, turned, and the primary key", table: one, index: []string{"a"}, where: "a = 1", by: "a desc, k", want: "forward"},
		{name: "another column by an index whose column is pinned", table: one, index: []string{"a"}, where: "a = 1", by: "b", want: "no"},
		{name: "an index whose column takes NULL, ascending", table: one, index: []string{"b"}, by: "b", want: "forward"},
		{name: "an index whose column takes NULL, descending", table: one, index: []string{"b"}, by: "b desc", want: "backward"},
		{name: "the primary key by an index whose column is pinned to NULL", table: one, index: []string{"b"}, where: "b is null", by: "k", want: "forward"},

		{name: "an index with a descending column in its order", table: one, index: []string{"a", "b desc"}, by: "a, b desc", want: "forward"},
		{name: "an index with a descending column in its reverse", table: one, index: []string{"a", "b desc"}, by: "a desc, b", want: "backward"},
		{name: "both columns ascending by an index with a descending one", table: one, index: []string{"a", "b desc"}, by: "a, b", want: "no"},
		{name: "both columns descending by an index with a descending one", table: one, index: []string{"a", "b desc"}, by: "a desc, b desc", want: "no"},
		{name: "the whole key of an index", table: one, index: []string{"a", "b desc"}, by: "a, b desc, k", want: "forward"},
		{name: "the whole key of an index in reverse", table: one, index: []string{"a", "b desc"}, by: "a desc, b, k desc", want: "backward"},
		{name: "the whole key of an index with its last column turned", table: one, index: []string{"a", "b desc"}, by: "a, b desc, k desc", want: "no"},
		{name: "the start of the key of an index", table: one, index: []string{"a", "b desc"}, by: "a", want: "forward"},
		{name: "a later column of an index whose first is not pinned", table: one, index: []string{"a", "b desc"}, by: "b desc", want: "no"},
		{name: "the next column after a pinned one, as the index holds it", table: one, index: []string{"a", "b desc"}, where: "a = 1", by: "b desc", want: "forward"},
		{name: "the next column after a pinned one, turned", table: one, index: []string{"a", "b desc"}, where: "a = 1", by: "b", want: "backward"},
		{name: "the next two columns after a pinned one, turned", table: one, index: []string{"a", "b desc"}, where: "a = 1", by: "b, k desc", want: "backward"},
		{name: "the next two columns after a pinned one, one of them turned", table: one, index: []string{"a", "b desc"}, where: "a = 1", by: "b, k", want: "no"},
		{name: "the primary key after two pinned columns", table: one, index: []string{"a", "b desc"}, where: "a = 1 and b = 'x'", by: "k", want: "forward"},
		{name: "the columns around a pinned column in the middle of the key", table: one, index: []string{"a", "b desc"}, where: "b = 'x'", by: "a, k", want: "forward"},
		{name: "the columns around a pinned column in the middle of the key, with it", table: one, index: []string{"a", "b desc"}, where: "b = 'x'", by: "a, b, k", want: "forward"},
		{name: "the columns around a column pinned to NULL, one of them turned", table: one, index: []string{"a", "b desc"}, where: "b is null", by: "a, k desc", want: "no"},
		{name: "the columns around a column pinned to NULL, both turned", table: one, index: []string{"a", "b desc"}, where: "b is null", by: "a desc, k desc", want: "backward"},
		{name: "a column that the key has after one that is neither pinned nor named", table: one, index: []string{"a", "b desc"}, where: "a > 1", by: "b desc", want: "no"},
		{name: "a descending column of an index, as the index holds it", table: one, index: []string{"c desc"}, by: "c desc", want: "forward"},
		{name: "a descending column of an index, ascending", table: one, index: []string{"c desc"}, by: "c", want: "backward"},
		{name: "a descending column of an index, ascending, and the primary key descending", table: one, index: []string{"c desc"}, by: "c, k desc", want: "backward"},
		{name: "a descending column of an index, ascending, and the primary key ascending", table: one, index: []string{"c desc"}, by: "c, k", want: "no"},
		{name: "an index that holds the primary key, which its key does not hold again", table: one, index: []string{"k desc", "a"}, by: "k desc, a", want: "forward"},
		{name: "an index that holds the primary key, with the primary key named again after it", table: one, index: []string{"k desc", "a"}, by: "k desc, a, k", want: "no"},

		{name: "a primary key of two columns", table: two, by: "k1, k2", want: "forward"},
		{name: "a primary key of two columns in reverse", table: two, by: "k1 desc, k2 desc", want: "backward"},
		{name: "the first column of a primary key of two", table: two, by: "k1", want: "forward"},
		{name: "the second column of a primary key of two", table: two, by: "k2", want: "no"},
		{name: "the second column of a primary key whose first is pinned", table: two, where: "k1 = 1", by: "k2 desc", want: "backward"},
		{name: "the first column of a primary key whose second is pinned", table: two, where: "k2 = 1", by: "k1", want: "forward"},
		{name: "a primary key of two columns at the end of an index whose column is pinned", table: two, index: []string{"a"}, where: "a = 1", by: "k1, k2", want: "forward"},
		{name: "an index and the first column of a primary key of two", table: two, index: []string{"a"}, by: "a, k1", want: "forward"},
		{name: "an index and the second column of a primary key of two", table: two, index: []string{"a"}, by: "a, k2", want: "no"},
		{name: "an index that holds the second column of the primary key, and then the first", table: two, index: []string{"k2"}, by: "k2, k1", want: "forward"},

		{name: "goqite's receive by goqite's index", table: goqite, index: []string{"queue", "priority desc", "created"}, where: receive, by: "priority desc, created", want: "forward"},
		{name: "goqite's receive turned round by goqite's index", table: goqite, index: []string{"queue", "priority desc", "created"}, where: receive, by: "priority, created desc", want: "backward"},
		{name: "goqite's receive with both columns descending", table: goqite, index: []string{"queue", "priority desc", "created"}, where: receive, by: "priority desc, created desc", want: "no"},
		{name: "goqite's receive by the priority alone", table: goqite, index: []string{"queue", "priority desc", "created"}, where: receive, by: "priority desc", want: "forward"},
		{name: "goqite's receive by the time alone", table: goqite, index: []string{"queue", "priority desc", "created"}, where: receive, by: "created", want: "no"},
		{name: "goqite's receive with the id at the end", table: goqite, index: []string{"queue", "priority desc", "created"}, where: receive, by: "priority desc, created, id", want: "forward"},
		{name: "goqite's receive with the queue first", table: goqite, index: []string{"queue", "priority desc", "created"}, where: receive, by: "queue, priority desc, created", want: "forward"},
		{name: "goqite's receive of one priority", table: goqite, index: []string{"queue", "priority desc", "created"}, where: receive + " and priority = 1", by: "priority desc, created", want: "forward"},
		{name: "goqite's receive of every queue", table: goqite, index: []string{"queue", "priority desc", "created"}, where: "received < 3", by: "priority desc, created", want: "no"},
		{name: "goqite's receive by an index on the queue and the timeout", table: goqite, index: []string{"queue", "timeout"}, where: receive, by: "priority desc, created", want: "no"},
		{name: "goqite's receive by the primary key", table: goqite, where: receive, by: "priority desc, created", want: "no"},
		{name: "the timeout by an index on the queue and the timeout", table: goqite, index: []string{"queue", "timeout"}, where: receive, by: "timeout", want: "forward"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var ix *s4qlpb.CreateIndex
			if test.index != nil {
				ix = accessIndex(test.table, "i", test.index...)
			}
			var conditions []condition
			if test.where != "" {
				conditions = accessConditions(test.table, test.where)
			}
			ok, backward := delivers(test.table, core.IndexKey(test.table, ix), conditions, accessOrder(test.table, test.by))
			got := "no"
			switch {
			case ok && backward:
				got = "backward"
			case ok:
				got = "forward"
			}
			is.Equal(t, test.want, got)
			if !ok {
				is.True(t, !backward, "an order that is not delivered has no direction")
			}
		})
	}
}

func TestChooseAccess_orderBy(t *testing.T) {
	one := accessTable([]uint32{0}, "k int", "a int", "b text?", "c real?", "d blob")
	tests := []struct {
		name string
		// indexes are the indexes of the table, each as its name, a colon, and its columns, in the order
		// in which they were created.
		indexes []string
		where   string
		by      string
		// want is the access of the query without a LIMIT, and limited the access with one, if it is
		// another.
		want, limited string
	}{
		{name: "a sort of every row for a column without an index", by: "a", want: "full sorted"},
		{name: "the primary key forward for its order", by: "k", want: "full delivered forward"},
		{name: "the primary key backward for its reverse", by: "k desc", want: "full delivered backward"},
		{name: "an index that delivers the order with nothing pinned, over every row and a sort", indexes: []string{"ia: a"}, by: "a", want: "ia delivered forward"},
		{name: "an index backward", indexes: []string{"ia: a"}, by: "a desc", want: "ia delivered backward"},
		{name: "the primary key over an index that delivers the order as well", indexes: []string{"ik: k desc"}, by: "k", want: "full delivered forward"},
		{name: "the first index created of two that deliver the order", indexes: []string{"ia: a", "iab: a, b"}, by: "a", want: "ia delivered forward"},
		{name: "the index that delivers the order of two with nothing pinned", indexes: []string{"ia: a", "iab: a, b desc"}, by: "a desc, b", want: "iab delivered backward"},
		{name: "a sort where the directions are mixed against every index", indexes: []string{"ia: a", "iab: a, b desc"}, by: "a, b", want: "full sorted"},
		{name: "a bound over the order without a LIMIT, and the order over a bound with one", indexes: []string{"ia: a"}, where: "k > 5", by: "a",
			want: "primary > 5 sorted", limited: "ia delivered forward"},
		{name: "a bound of an index over the order of the primary key without a LIMIT, and the order with one", indexes: []string{"ia: a"}, where: "a > 5", by: "k",
			want: "ia > 5 sorted", limited: "full delivered forward"},
		{name: "a bound and the order together", indexes: []string{"ia: a"}, where: "a > 5", by: "a", want: "ia > 5 delivered forward"},
		{name: "more pins over the order", indexes: []string{"ia: a", "ib: b"}, where: "b = 'x'", by: "a", want: "ib = 'x' sorted"},
		{name: "more pins over the order, and then the order", indexes: []string{"ia: a", "ib: b", "iba: b, a"}, where: "b = 'x'", by: "a",
			want: "iba = 'x' delivered forward"},
		{name: "the order over no order at the same pins", indexes: []string{"ia: a", "iac: a, c desc"}, where: "a = 1", by: "c desc", want: "iac = 1 delivered forward"},
		{name: "a bound over the order at the same pins without a LIMIT, and the order over the bound with one", indexes: []string{"ia: a", "iac: a, c desc"},
			where: "a = 1 and k > 3", by: "c", want: "ia = 1 > 3 sorted", limited: "iac = 1 delivered backward"},
		{name: "a bound and the order over the order alone", indexes: []string{"iac: a, c desc", "ia: a"}, where: "a = 1 and k > 3", by: "k",
			want: "ia = 1 > 3 delivered forward"},
		{name: "an index that pins over the primary key that delivers", indexes: []string{"ia: a"}, where: "a = 1", by: "k", want: "ia = 1 delivered forward"},
		{name: "an ORDER BY of a pinned column", indexes: []string{"ia: a"}, where: "a = 1", by: "a desc", want: "ia = 1 delivered forward"},
		{name: "a pinned column in the middle of the key", indexes: []string{"iab: a, b desc"}, where: "b is null", by: "a desc, k desc", want: "iab delivered backward"},
		{name: "no row, which delivers every order", indexes: []string{"ia: a"}, where: "a = null", by: "b", want: "none delivered forward"},
		{name: "the row of a primary key, which delivers every order", indexes: []string{"ia: a"}, where: "k = 1", by: "b desc", want: "point 1 delivered forward"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var indexes []*s4qlpb.CreateIndex
			for _, ix := range test.indexes {
				name, columns, _ := strings.Cut(ix, ": ")
				indexes = append(indexes, accessIndex(one, name, strings.Split(columns, ", ")...))
			}
			var conditions []condition
			if test.where != "" {
				conditions = accessConditions(one, test.where)
			}
			by := accessOrder(one, test.by)
			a, err := chooseAccess(one, indexes, conditions, orderBy{columns: by}, bugs{})
			is.NotError(t, err)
			is.Equal(t, test.want, orderedText(a))
			limited := test.limited
			if limited == "" {
				limited = test.want
			}
			a, err = chooseAccess(one, indexes, conditions, orderBy{columns: by, limited: true}, bugs{})
			is.NotError(t, err)
			is.Equal(t, limited, orderedText(a), "with a LIMIT")

			// Without the ORDER BY, the access is the one of slice 9, with a LIMIT and without, and its
			// rows have no order.
			plain, err := chooseAccess(one, indexes, conditions, orderBy{}, bugs{})
			is.NotError(t, err)
			onlyLimited, err := chooseAccess(one, indexes, conditions, orderBy{limited: true}, bugs{})
			is.NotError(t, err)
			is.Equal(t, accessText(plain), orderedText(plain))
			is.Equal(t, accessText(plain), orderedText(onlyLimited))
		})
	}
}

func TestForcedAccess_orderBy(t *testing.T) {
	one := accessTable([]uint32{0}, "k int", "a int", "b text?", "c real?", "d blob")
	indexes := []*s4qlpb.CreateIndex{accessIndex(one, "iab", "a", "b desc")}
	tests := []struct {
		name  string
		path  path
		where string
		by    string
		want  string
	}{
		{name: "an index that delivers the order", path: path{kind: pathIndex, index: "iab"}, where: "a = 1", by: "b", want: "iab = 1 delivered backward"},
		{name: "an index that delivers the order, sorted", path: path{kind: pathIndex, index: "iab", sort: true}, where: "a = 1", by: "b", want: "iab = 1 sorted"},
		{name: "an index that does not deliver the order", path: path{kind: pathIndex, index: "iab"}, where: "a = 1", by: "c", want: "iab = 1 sorted"},
		{name: "an index that pins less than the conditions, which delivers the order all the same", path: path{kind: pathIndex, index: "iab", limited: true},
			where: "a = 1", by: "b desc, k", want: "iab delivered forward"},
		{name: "the primary key that delivers the order", path: path{kind: pathPrimary}, where: "a = 1", by: "k desc", want: "full delivered backward"},
		{name: "the primary key, sorted", path: path{kind: pathPrimary, sort: true}, where: "k > 1", by: "k desc", want: "primary > 1 sorted"},
		{name: "the primary key in place of a get, which sorts one row at most", path: path{kind: pathPrimary}, where: "k = 1", by: "b", want: "primary = 1 sorted"},
		{name: "no row, also where the path sorts", path: path{kind: pathPrimary, sort: true}, where: "k is null", by: "b", want: "none delivered forward"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, ok, err := forcedAccess(one, indexes, accessConditions(one, test.where), orderBy{columns: accessOrder(one, test.by)}, test.path, bugs{})
			is.NotError(t, err)
			is.True(t, ok)
			is.Equal(t, test.want, orderedText(a))
		})
	}
}
