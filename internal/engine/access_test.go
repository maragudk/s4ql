package engine_test

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
)

// pathIndex is an index of the table of the tests of the paths: its name, and the columns of its key,
// which are its own and then the primary key, if it does not hold it, each with " desc" after a
// column in descending order.
type pathIndex struct {
	name string
	// own is the number of columns of key that are the index's own.
	own int
	key []string
}

// create is the statement that creates the index on the table.
func (x pathIndex) create(table string) string {
	return fmt.Sprint("create index ", x.name, " on ", table, " (", strings.Join(x.key[:x.own], ", "), ")")
}

// orderBy is the ORDER BY that gives the rows in the order of the key, or in its reverse if backward.
func orderBy(key []string, backward bool) string {
	terms := make([]string, len(key))
	for i, c := range key {
		name, desc := strings.CutSuffix(c, " desc")
		if desc != backward {
			name += " desc"
		}
		terms[i] = name
	}
	return " order by " + strings.Join(terms, ", ")
}

const pathTable = "create table p (k int primary key, i integer, r real, s text not null, b blob, n int) strict, without rowid"

// pathIndexes of the table p: on a column of each type, on columns in descending order, on several
// columns, on columns that take NULL, and with the primary key among its own columns.
var pathIndexes = []pathIndex{
	{name: "p_s", own: 1, key: []string{"s", "k"}},
	{name: "p_r", own: 1, key: []string{"r desc", "k"}},
	{name: "p_i_s", own: 2, key: []string{"i", "s desc", "k"}},
	{name: "p_b", own: 1, key: []string{"b", "k"}},
	{name: "p_n_r", own: 2, key: []string{"n", "r", "k"}},
	{name: "p_s_k", own: 2, key: []string{"s", "k desc"}},
	{name: "p_n_i_r", own: 3, key: []string{"n desc", "i", "r desc", "k"}},
}

// pathRows are the statements that fill the table p with 60 rows: few values in each column, so that
// many rows share the leading columns of each index, with NULL in every column that takes it, 0.0
// and -0.0, and the empty text and the empty blob.
func pathRows() []string {
	var rows []string
	for k := range 60 {
		i := []string{"null", "1", "2", "3", "-9223372036854775808", "9223372036854775807"}[k%6]
		r := []string{"null", "0.0", "-0.0", "1.5", "-2.5", "1e300", "1.5"}[k%7]
		s := []string{"''", "'a'", "'b'", "'c'", "'ab'"}[k%5]
		b := []string{"null", "x''", "x'00'", "x'ff'"}[k%4]
		n := []string{"null", "1", "2"}[k%3]
		rows = append(rows, fmt.Sprintf("insert into p values (%v, %v, %v, %v, %v, %v)", k-10, i, r, s, b, n))
	}
	return rows
}

// pathWheres of the selects, the updates, and the deletes of the tests of the paths, each with its
// arguments.
var pathWheres = []struct {
	where string
	args  []any
}{
	{where: "s = 'a'"},
	{where: "s > 'a' and s <= 'c'"},
	{where: "s >= ''"},
	{where: "s < ''"},
	{where: "s = 'a' and k > 5"},
	{where: "s = ? and k >= ? and k < ?", args: []any{"b", int64(-5), int64(40)}},
	{where: "s = 'c' and k = 13"},
	{where: "r > 1.5"},
	{where: "r >= 1.5 and r < 1e300"},
	{where: "r <= 0.0"},
	{where: "r = 0.0"},
	{where: "r = -0.0 and k > 0"},
	{where: "r is null"},
	{where: "r is not null"},
	{where: "i = 3 and s = 'b'"},
	{where: "i = 3 and s > 'a'"},
	{where: "i = ? and s < ? and s >= ?", args: []any{int64(2), "c", "a"}},
	{where: "i = 1 and s = 'a' and k <= 31"},
	{where: "i is null"},
	{where: "i is null and s = 'a'"},
	{where: "i is null and s <= 'b' and k > 0"},
	{where: "i > 3"},
	{where: "i < 1"},
	{where: "b = x'00'"},
	{where: "b > x''"},
	{where: "b >= x'' and b < x'ff'"},
	{where: "b is null"},
	{where: "b is null and k > 20"},
	{where: "b = ?", args: []any{[]byte{}}},
	{where: "n is null and r > 1.0"},
	{where: "n is null and r is null"},
	{where: "n is null and r is null and k > 3"},
	{where: "n = 1 and r is null and k > 3"},
	{where: "n = 2 and r = 1.5 and k >= 6 and k <= 30"},
	{where: "n = 1 and i = 3 and r > 0.0"},
	{where: "n is null and i is null and r is null"},
	{where: "n = 2 and i = 2 and r = 1.5 and k < 100"},
	{where: "n > 1"},
	{where: "n <> 2"},
	{where: "k = 7"},
	{where: "k = 7 and s = 'c'"},
	{where: "k = 7 and s = 'a'"},
	{where: "k = 1000"},
	{where: "k > 10 and k <= 20"},
	{where: "k >= -10 and k < -5 and s = 'a'"},
	{where: "k is null"},
	{where: "k is not null and n = 1"},
	{where: "s = null"},
	{where: "s = ? and i > ?", args: []any{"a", nil}},
	{where: "s is null"},
	{where: "s is null and i = 1"},
	{where: "s > 'c' and s < 'a'"},
	{where: "i = 1 and i = 2"},
	{where: "i = 1 and i is null"},
	{where: "i is null and i = 1"},
	{where: "n = 1 and n > 1"},
}

// pathsOf the table p: nothing forced, every row, the primary key, and each index, each forward and
// backward, and each with every number of pinned columns that its key allows.
func pathsOf() []engine.Path {
	paths := []engine.Path{{}, {Forward: true}, {Backward: true}}
	for _, p := range []engine.Path{{Full: true}, {Primary: true}} {
		forward, backward := p, p
		forward.Forward, backward.Backward = true, true
		paths = append(paths, forward, backward)
	}
	paths = append(paths, engine.Path{Primary: true, Limited: true, Forward: true})
	for _, x := range pathIndexes {
		paths = append(paths, engine.Path{Index: x.name, Forward: true}, engine.Path{Index: x.name, Backward: true})
		for pins := range len(x.key) {
			paths = append(paths, engine.Path{Index: x.name, Limited: true, Pins: pins, Forward: pins%2 == 0, Backward: pins%2 == 1})
		}
	}
	return paths
}

// keyOf the path p of the table p: the columns that its scan reads in the order of, or nil for a
// path that forces no access.
func keyOf(p engine.Path) []string {
	switch {
	case p.Full, p.Primary:
		return []string{"k"}
	case p.Index != "":
		for _, x := range pathIndexes {
			if x.name == p.Index {
				return x.key
			}
		}
	}
	return nil
}

func TestEngine_paths(t *testing.T) {
	t.Run("should give SQLite's rows for every select by every path that can serve it, in the order of the key of the path or in its reverse", func(t *testing.T) {
		p := newIndexPair(t, 0, pathTable)
		for _, x := range pathIndexes {
			is.Equal(t, "", p.run(x.create("p")).err)
		}
		for _, src := range pathRows() {
			is.Equal(t, "", p.run(src).err, src)
		}
		// some counts, for each path, the selects that gave some rows and not all of them.
		some := map[string]int{}
		for _, path := range pathsOf() {
			p.i.e.SetPath(path)
			for _, w := range pathWheres {
				for _, columns := range []string{"*", "k", "s, n, k"} {
					src := fmt.Sprint("select ", columns, " from p where ", w.where)
					what := fmt.Sprintf("%v with %#v by the path %+v", src, w.args, path)
					want := p.o.run(t, src, oracleArgs(w.args)...)
					is.Equal(t, "", want.err, what)

					rows, err := p.i.c.Query(t.Context(), src, values(w.args)...)
					is.NotError(t, err, what)
					a, ok := rows.Access()
					is.True(t, ok, what)
					is.NotError(t, rows.Close())
					got := p.i.queryValues(src, values(w.args))
					is.Equal(t, want.String(), got.String(), what)

					// The path reached the scan: the comparison of the rows cannot see that.
					key := keyOf(path)
					switch {
					case path.Full:
						is.True(t, a.Full, what)
					case path.Primary:
						is.True(t, a.None || a.Index == "" && a.Point == nil, what)
					case path.Index != "":
						is.True(t, a.None || a.Index == path.Index, what)
					}
					if path.Limited {
						is.True(t, a.Pinned() <= path.Pins, what)
					}
					if path.Forward || path.Backward {
						is.Equal(t, path.Backward, a.Backward, what)
					}
					if key != nil && (path.Forward || path.Backward) {
						// SQLite's own ORDER BY of the key of the path says what its order is.
						ordered := p.o.run(t, src+orderBy(key, path.Backward), oracleArgs(w.args)...)
						is.Equal(t, "", ordered.err, what)
						is.Equal(t, fmt.Sprintf("%q", ordered.rows), fmt.Sprintf("%q", got.rows), "the order of the rows of "+what)
					}
					if len(want.rows) > 0 && len(want.rows) < 60 {
						some[fmt.Sprintf("%+v", path)]++
					}
				}
			}
		}
		for _, path := range pathsOf() {
			is.True(t, some[fmt.Sprintf("%+v", path)] >= 30, fmt.Sprintf("the path %+v gave some rows for %v selects", path, some[fmt.Sprintf("%+v", path)]))
		}
	})

	t.Run("should change the rows that SQLite changes with every update and delete by every path that can serve it", func(t *testing.T) {
		changes := []string{"update p set n = 7 where ", "update p set s = 'z', i = null where ", "update p set r = 9.5, b = x'07' where ", "delete from p where "}
		// Each path gets a fresh database for each statement, so only every eighth condition runs by
		// each path, a different eighth for each path.
		for n, path := range pathsOf() {
			for m, w := range pathWheres {
				if (n+m)%8 != 0 {
					continue
				}
				src := changes[(n+m/8)%len(changes)] + w.where
				what := fmt.Sprintf("%v with %#v by the path %+v", src, w.args, path)
				var options engine.Options
				options.Path(path)
				i, o := newInstance(t, options), newOracle(t)
				setup := []string{pathTable}
				for _, x := range pathIndexes {
					setup = append(setup, x.create("p"))
				}
				// One transaction, so that the setup is one commit.
				is.NotError(t, i.c.Begin(t.Context()))
				for _, s := range append(setup, pathRows()...) {
					is.Equal(t, "", i.exec(s).err, s)
					is.Equal(t, "", o.run(t, s).err, s)
				}
				is.NotError(t, i.c.Commit(t.Context()))

				want := o.run(t, src, oracleArgs(w.args)...)
				got := i.execValues(src, values(w.args))

				is.Equal(t, want.String(), got.String(), what)
				changed, _ := o.changes(t)
				is.Equal(t, changed, i.changes, "the count of changes of "+what)
				is.Equal(t, o.state(t), i.state(), "the state after "+what)
			}
		}
	})
}

func TestEngine_order(t *testing.T) {
	setup := []string{"create table t (k int primary key, a int not null) strict, without rowid", "create index ta on t (a desc)"}
	for k := range 10 {
		setup = append(setup, fmt.Sprintf("insert into t values (%v, %v)", k, k%3))
	}
	forward := map[string]string{
		"select k from t":                       "[0 1 2 3 4 5 6 7 8 9]",
		"select k from t where k > 2 and k < 8": "[3 4 5 6 7]",
		"select k from t where a = 1":           "[1 4 7]",
		"select k from t where a >= 1":          "[2 5 8 1 4 7]",
	}
	queries := slices.Sorted(func(yield func(string) bool) {
		for q := range forward {
			if !yield(q) {
				return
			}
		}
	})
	backward := func(q string) string {
		var rows []string
		for _, row := range strings.Fields(strings.Trim(forward[q], "[]")) {
			rows = append([]string{row}, rows...)
		}
		return fmt.Sprint(rows)
	}
	// orders of 40 rounds of the queries on a new engine with the seed: for each query "f" or "b", and
	// each order is that of the key of the access, or its reverse, and what the rows say of
	// themselves.
	orders := func(t *testing.T, seed uint64) string {
		t.Helper()
		var options engine.Options
		options.Seed(seed)
		i := newInstance(t, options)
		for _, s := range setup {
			is.Equal(t, "", i.exec(s).err, s)
		}
		var b strings.Builder
		for range 40 {
			for _, q := range queries {
				rows, err := i.c.Query(t.Context(), q)
				is.NotError(t, err)
				a, ok := rows.Access()
				is.True(t, ok)
				var got []string
				for row, err := range rows.All() {
					is.NotError(t, err)
					got = append(got, fmt.Sprint(row[0].GetInteger()))
				}
				switch fmt.Sprint(got) {
				case forward[q]:
					is.True(t, !a.Backward, q)
					b.WriteString("f")
				case backward(q):
					is.True(t, a.Backward, q)
					b.WriteString("b")
				default:
					t.Fatalf("%v gives %v, which is neither the order of its key, %v, nor its reverse", q, got, forward[q])
				}
			}
		}
		return b.String()
	}

	t.Run("should read each query forward or backward, both of them for each access, and in the same directions again for the same seed", func(t *testing.T) {
		first := orders(t, 1)

		for n := range queries {
			var f, b int
			for round := range 40 {
				if first[round*len(queries)+n] == 'f' {
					f++
				} else {
					b++
				}
			}
			is.True(t, f >= 5 && b >= 5, fmt.Sprintf("%v ran forward %v times and backward %v times", queries[n], f, b))
		}
		is.Equal(t, first, orders(t, 1), "the same seed")
		is.True(t, first != orders(t, 2), "another seed")
	})

	t.Run("should start the directions again from a seed that is set later, and draw none for a path with a direction", func(t *testing.T) {
		var options engine.Options
		options.Seed(7)
		i := newInstance(t, options)
		for _, s := range setup {
			is.Equal(t, "", i.exec(s).err, s)
		}
		run := func() string {
			var b strings.Builder
			for range 20 {
				rows, err := i.c.Query(t.Context(), "select k from t")
				is.NotError(t, err)
				a, _ := rows.Access()
				is.NotError(t, rows.Close())
				b.WriteString(map[bool]string{false: "f", true: "b"}[a.Backward])
			}
			return b.String()
		}

		first := run()
		second := run()
		i.e.SetSeed(7)
		is.Equal(t, first, run())
		i.e.SetPath(engine.Path{Backward: true})
		is.Equal(t, strings.Repeat("b", 20), run())
		i.e.SetPath(engine.Path{})
		is.Equal(t, second, run(), "the forced queries drew nothing")
	})
}

func TestEngine_rowsRead(t *testing.T) {
	// read runs src with args on the instance, by the rules and then forward and backward, and gives
	// the access of its event and the number of rows that it read from the cache, which must be the
	// same in each direction, and the number of rows that it gave.
	read := func(t *testing.T, i *instance, h *events, src string, args ...any) (access string, read, rows string) {
		t.Helper()
		for n, path := range []engine.Path{{}, {Forward: true}, {Backward: true}} {
			i.e.SetPath(path)
			got := i.queryValues(src, values(args))
			is.Equal(t, "", got.err, src)
			event, _ := h.last(t)
			a := event[engine.AttrAccess]
			if event[engine.AttrIndex] != "" {
				a += " " + event[engine.AttrIndex]
			}
			if n > 0 {
				is.Equal(t, access, a, src)
				is.Equal(t, read, event[engine.AttrRowsRead], fmt.Sprintf("%v by the path %+v", src, path))
				is.Equal(t, rows, event[engine.AttrRows], src)
			}
			access, read, rows = a, event[engine.AttrRowsRead], event[engine.AttrRows]
		}
		i.e.SetPath(engine.Path{})
		return access, read, rows
	}
	// fill runs the statements of rows on the instance in transactions of 500.
	fill := func(t *testing.T, i *instance, rows int, row func(n int) (string, []any)) {
		t.Helper()
		for n := range rows {
			if n%500 == 0 {
				is.NotError(t, i.c.Begin(t.Context()))
			}
			src, args := row(n)
			is.Equal(t, "", i.execValues(src, values(args)).err, src)
			if n%500 == 499 || n == rows-1 {
				is.NotError(t, i.c.Commit(t.Context()))
			}
		}
	}

	t.Run("should read from the cache the rows that the access must, for a point lookup, an equality, and an equality with a range, with and without an index", func(t *testing.T) {
		h := &events{}
		i := newInstance(t, engine.Options{Logger: slog.New(h)})
		is.Equal(t, "", i.exec("create table t (k int primary key, a int not null, b int not null, c text) strict, without rowid").err)
		// 4,000 rows: 40 for each value of a, and b from 0 to 6.
		fill(t, i, 4000, func(n int) (string, []any) {
			return "insert into t values (?, ?, ?, ?)", []any{int64(n), int64(n % 100), int64(n % 7), fmt.Sprint("row ", n)}
		})
		queries := []struct {
			name, src string
			args      []any
		}{
			{name: "a point lookup", src: "select * from t where k = ?", args: []any{int64(1234)}},
			{name: "an equality", src: "select * from t where a = ?", args: []any{int64(42)}},
			{name: "an equality and a range", src: "select * from t where a = ? and b >= ?", args: []any{int64(42), int64(3)}},
			{name: "a range of the key", src: "select * from t where k >= ? and k < ?", args: []any{int64(1000), int64(1100)}},
		}
		pins := []struct {
			indexes string
			create  []string
			want    []string
		}{
			{indexes: "no index", want: []string{
				"primary_key reads 1 gives 1", "full_scan reads 4000 gives 40", "full_scan reads 4000 gives 22", "primary_key reads 100 gives 100"}},
			{indexes: "an index on (a)", create: []string{"create index t_a on t (a)"}, want: []string{
				"primary_key reads 1 gives 1", "index t_a reads 40 gives 40", "index t_a reads 40 gives 22", "primary_key reads 100 gives 100"}},
			{indexes: "indexes on (a) and on (a, b)", create: []string{"create index t_a_b on t (a, b)"}, want: []string{
				"primary_key reads 1 gives 1", "index t_a reads 40 gives 40", "index t_a_b reads 22 gives 22", "primary_key reads 100 gives 100"}},
		}
		for _, pin := range pins {
			for _, src := range pin.create {
				is.Equal(t, "", i.exec(src).err, src)
			}
			for n, q := range queries {
				access, rowsRead, rows := read(t, i, h, q.src, q.args...)
				is.Equal(t, pin.want[n], fmt.Sprint(access, " reads ", rowsRead, " gives ", rows), q.name+" with "+pin.indexes)
			}
		}
	})

	t.Run("should read the rows of one queue for a query shaped like goqite's receive without its ORDER BY, with goqite's index, and every row without it", func(t *testing.T) {
		h := &events{}
		i := newInstance(t, engine.Options{Logger: slog.New(h)})
		is.Equal(t, "", i.exec(`create table goqite (id text primary key, created text not null, updated text not null, queue text not null,
			body blob not null, timeout text not null, received int not null, priority int not null) strict, without rowid`).err)
		// 3,000 messages in 3 queues of 1,000 each, with 4 priorities. In each queue, 1 message in 2 has
		// a timeout that has passed, and 1 in 10 was received 3 times.
		fill(t, i, 3000, func(n int) (string, []any) {
			timeout := "2026-11-20T12:00:00.000Z"
			if n%2 == 1 {
				timeout = "2026-11-20T18:00:00.000Z"
			}
			received := int64(0)
			if n%10 == 4 {
				received = 3
			}
			created := fmt.Sprintf("2026-11-20T10:%02d:%02d.%03dZ", n/60000%60, n/1000%60, n%1000)
			return "insert into goqite values (?, ?, ?, ?, ?, ?, ?, ?)",
				[]any{fmt.Sprintf("m_%04d", n), created, created, fmt.Sprint("queue-", n%3), []byte("body"), timeout, received, int64(n / 3 % 4)}
		})
		receive := "select id from goqite where queue = ? and timeout <= ? and received < ?"
		args := []any{"queue-1", "2026-11-20T15:00:00.000Z", int64(3)}

		access, rowsRead, rows := read(t, i, h, receive, args...)
		is.Equal(t, "full_scan reads 3000 gives 400", fmt.Sprint(access, " reads ", rowsRead, " gives ", rows))

		is.Equal(t, "", i.exec("create index goqite_queue_priority_created_idx on goqite (queue, priority desc, created)").err)
		access, rowsRead, rows = read(t, i, h, receive, args...)
		is.Equal(t, "index goqite_queue_priority_created_idx reads 1000 gives 400", fmt.Sprint(access, " reads ", rowsRead, " gives ", rows))

		// An index on the queue and the timeout pins as much and bounds the next column, so it wins.
		is.Equal(t, "", i.exec("create index goqite_queue_timeout on goqite (queue, timeout)").err)
		access, rowsRead, rows = read(t, i, h, receive, args...)
		is.Equal(t, "index goqite_queue_timeout reads 500 gives 400", fmt.Sprint(access, " reads ", rowsRead, " gives ", rows))
	})
}
