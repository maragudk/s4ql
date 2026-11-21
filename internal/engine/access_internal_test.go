package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

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
		{name: "should take an index with a range over the primary key with nothing",table: one, indexes: [][]string{{"i", "a"}}, where: "a < 3", want: "i < 3"},
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

			a, err := chooseAccess(test.table, indexes, conditions, bugs{})
			is.NotError(t, err)
			is.Equal(t, test.want, accessText(a))
			is.True(t, !a.backward, "the rules set no direction")

			// The first two rules do not depend on the indexes, which is why a statement that they decide
			// reads none.
			without, err := chooseAccess(test.table, nil, conditions, bugs{})
			is.NotError(t, err)
			if a.none || a.point != nil || without.none || without.point != nil {
				is.Equal(t, accessText(a), accessText(without))
			}
		})
	}

	t.Run("should fail with an internal error for two bounds of values of two types, which the check of the types refuses before", func(t *testing.T) {
		conditions := []condition{
			{position: 0, op: stmt.OpLess, value: s4qlpb.Value_builder{Integer: new(int64(1))}.Build()},
			{position: 0, op: stmt.OpLess, value: s4qlpb.Value_builder{Text: new("a")}.Build()},
		}

		_, err := chooseAccess(one, nil, conditions, bugs{})
		is.True(t, errors.Is(err, ErrInternal))

		conditions[0].position, conditions[1].position = 1, 1
		_, err = chooseAccess(one, []*s4qlpb.CreateIndex{accessIndex(one, "i", "a")}, conditions, bugs{})
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
			a, ok, err := forcedAccess(one, indexes, accessConditions(one, test.where), test.path, bugs{})
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

		a, ok, err := forcedAccess(one, indexes, conditions, path{kind: pathIndex, index: "i"}, bugs{})
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
