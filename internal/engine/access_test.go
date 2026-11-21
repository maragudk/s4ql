package engine_test

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
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
	t.Run("should give SQLite's rows for every select by every path that can serve it, in the order of the key of the path or in its reverse, with the values of its comparisons on either side", func(t *testing.T) {
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
				// Each WHERE as written, with every comparison of it written with its value first, and with
				// every second one so, which reads the rows of the WHERE as written by the same access.
				for n, columns := range []string{"*", "k", "s, n, k"} {
					src := fmt.Sprint("select ", columns, " from p where ", valueFirst(w.where, func(m int) bool { return n == 1 || n == 2 && m%2 == 0 }))
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

	t.Run("should change the rows that SQLite changes with every update and delete by every path that can serve it, with the values of its comparisons on either side", func(t *testing.T) {
		changes := []string{"update p set n = 7 where ", "update p set s = 'z', i = null where ", "update p set r = 9.5, b = x'07' where ", "delete from p where "}
		// Each path gets a fresh database for each statement, so only every eighth condition runs by
		// each path, a different eighth for each path.
		for n, path := range pathsOf() {
			for m, w := range pathWheres {
				if (n+m)%8 != 0 {
					continue
				}
				// The paths that run a condition are eight apart, and every second of them writes its
				// comparisons with their values first.
				src := changes[(n+m/8)%len(changes)] + valueFirst(w.where, func(int) bool { return n/8%2 == 1 })
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

// checkPlanOf the select src with args on the instance i, outside a transaction: the access that the
// rules choose for it on the table and the indexes that the core holds, and SQLite's plan for the
// statement that the cache runs for its scan, which must be the rows that [wantPlans] gives: the one
// row of [wantPlan], so that the rows that the cache reads cannot come from a scan of a whole index
// that SQLite filters, and after it, for an access whose rows are sorted, the one row of a sort of
// all the rows, and for every other access no row more, so that the cache sorts exactly where the
// engine says so.
func checkPlanOf(t *testing.T, i *instance, src string, args []any) {
	t.Helper()
	s, err := stmt.Check(src)
	is.NotError(t, err)
	ctx := t.Context()
	rd, err := i.d.Reader(ctx)
	is.NotError(t, err)
	defer func() { is.NotError(t, rd.Close(ctx)) }()
	r, err := rd.Read(ctx)
	is.NotError(t, err)
	var ct *s4qlpb.CreateTable
	tables, err := r.Tables(ctx)
	is.NotError(t, err)
	for _, table := range tables {
		if fold(table.GetTable()) == s.(*stmt.Select).Table.Fold() {
			ct = table
		}
	}
	is.True(t, ct != nil, "no table of "+src)
	cis, err := r.Indexes(ctx, ct.GetTable())
	is.NotError(t, err)
	is.NotError(t, r.Close(ctx))
	a, ok := engine.AccessOf(src, ct, cis, values(args))
	is.True(t, ok, src)
	if a.None || a.Point != nil {
		return
	}
	sql, plan := scanPlan(t, i, ct.GetTable(), a)
	is.EqualSlice(t, wantPlans(a, ct, cis), plan, src+", which the cache runs as "+sql)
}

func TestEngine_rowsRead(t *testing.T) {
	// read runs src with args on the instance, by the rules and then forward and backward, and gives
	// the access of its event, with "delivered" or "sorted" after it for a query with ORDER BY, and the
	// number of rows that it read from the cache, which must be the same in each direction, and the
	// number of rows that it gave.
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
			if event[engine.AttrOrder] != "" {
				a += " " + event[engine.AttrOrder]
			}
			if n > 0 {
				is.Equal(t, access, a, src)
				is.Equal(t, read, event[engine.AttrRowsRead], fmt.Sprintf("%v by the path %+v", src, path))
				is.Equal(t, rows, event[engine.AttrRows], src)
			}
			access, read, rows = a, event[engine.AttrRowsRead], event[engine.AttrRows]
		}
		i.e.SetPath(engine.Path{})
		checkPlanOf(t, i, src, args)
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

	t.Run("should read the rows of one queue for a query shaped like goqite's receive without its ORDER BY, as goqite writes it and with its column first, with goqite's index, and every row without it", func(t *testing.T) {
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
		// The receive as goqite writes its WHERE, with the value on the left of timeout, and with the
		// column first, read the same rows by the same access.
		receives := []string{"select id from goqite where queue = ? and ? >= timeout and received < ?", "select id from goqite where queue = ? and timeout <= ? and received < ?"}
		args := []any{"queue-1", "2026-11-20T15:00:00.000Z", int64(3)}
		readEach := func(want string) {
			t.Helper()
			for _, receive := range receives {
				access, rowsRead, rows := read(t, i, h, receive, args...)
				is.Equal(t, want, fmt.Sprint(access, " reads ", rowsRead, " gives ", rows), receive)
			}
		}

		readEach("full_scan reads 3000 gives 400")

		is.Equal(t, "", i.exec("create index goqite_queue_priority_created_idx on goqite (queue, priority desc, created)").err)
		readEach("index goqite_queue_priority_created_idx reads 1000 gives 400")

		// An index on the queue and the timeout pins as much and bounds the next column, so it wins.
		is.Equal(t, "", i.exec("create index goqite_queue_timeout on goqite (queue, timeout)").err)
		readEach("index goqite_queue_timeout reads 500 gives 400")
	})

	t.Run("should read one row for goqite's receive by goqite's index when a message is ready, also beside an index on the queue and the timeout, the rows of the queue when none is, and every row of the table for a sort without it", func(t *testing.T) {
		const now, past, future = "2026-11-20T15:00:00.000Z", "2026-11-20T12:00:00.000Z", "2026-11-20T18:00:00.000Z"
		args := []any{"queue-1", now, int64(3)}
		for _, test := range []struct {
			name string
			// timeout of every message.
			timeout string
			// want of the receive with no index, with goqite's index, and with both indexes, and then of
			// the receive without its LIMIT with both indexes.
			want [4]string
			// id of the message that the receive gives, or empty for none.
			id string
		}{
			{name: "every message ready", timeout: past, id: `"m_0010"`, want: [4]string{
				"full_scan sorted reads 3000 gives 1",
				"index goqite_queue_priority_created_idx delivered reads 1 gives 1",
				"index goqite_queue_priority_created_idx delivered reads 1 gives 1",
				"index goqite_queue_timeout sorted reads 1000 gives 1000",
			}},
			{name: "no message ready", timeout: future, want: [4]string{
				"full_scan sorted reads 3000 gives 0",
				"index goqite_queue_priority_created_idx delivered reads 1000 gives 0",
				"index goqite_queue_priority_created_idx delivered reads 1000 gives 0",
				"index goqite_queue_timeout sorted reads 0 gives 0",
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				h := &events{}
				i := newInstance(t, engine.Options{Logger: slog.New(h)})
				is.Equal(t, "", i.exec(goqiteTable).err)
				// 3,000 messages in 3 queues of 1,000 each, with 4 priorities, none of them received. The
				// message 10 is the oldest of the highest priority of its queue.
				fill(t, i, 3000, func(n int) (string, []any) {
					created := fmt.Sprintf("2026-11-20T10:%02d:%02d.%03dZ", n/60000%60, n/1000%60, n%1000)
					return "insert into goqite values (?, ?, ?, ?, ?, ?, ?, ?)",
						[]any{fmt.Sprintf("m_%04d", n), created, created, fmt.Sprint("queue-", n%3), []byte("body"), test.timeout, int64(0), int64(n / 3 % 4)}
				})
				readEach := func(want string) {
					t.Helper()
					for _, receive := range []string{goqiteReceive, goqiteReceiveTurned} {
						access, rowsRead, rows := read(t, i, h, receive, args...)
						is.Equal(t, want, fmt.Sprint(access, " reads ", rowsRead, " gives ", rows), receive)
						got := i.queryValues(receive, values(args))
						is.Equal(t, "", got.err)
						is.Equal(t, test.id, strings.Join(got.rows, " "), receive)
					}
				}
				readEach(test.want[0])
				is.Equal(t, "", i.exec(goqiteIndex).err)
				readEach(test.want[1])
				// SQLite's own planner reads by the index on the queue and the timeout here, and sorts. The
				// rules read by goqite's index, in its order, for the LIMIT.
				is.Equal(t, "", i.exec("create index goqite_queue_timeout on goqite (queue, timeout)").err)
				readEach(test.want[2])
				// Without the LIMIT every row that passes is read, so the bound wins, and its rows are sorted.
				access, rowsRead, rows := read(t, i, h, strings.TrimSuffix(goqiteReceive, " limit 1"), args...)
				is.Equal(t, test.want[3], fmt.Sprint(access, " reads ", rowsRead, " gives ", rows))
			})
		}
	})
}

var update = flag.Bool("update", false, "rewrite the golden files of TestAccessOf_golden with the plans that the engine chooses now")

// planSchema is a table and its indexes, with the queries whose plans TestAccessOf_golden keeps in
// the golden file testdata/plans/<name>.txt.
type planSchema struct {
	name string
	// statements are the create table and then the create index of each index, in the order in which
	// they are created.
	statements []string
	queries    []planQuery
}

// planQuery is a select, an update, or a delete with WHERE, and the arguments of its parameters.
type planQuery struct {
	name string
	src  string
	args []any
}

const (
	goqiteTable = "create table goqite (id text primary key, created text not null, updated text not null, queue text not null, " +
		"body blob not null, timeout text not null, received int not null, priority int not null) strict, without rowid"
	goqiteIndex = "create index goqite_queue_priority_created_idx on goqite (queue, priority desc, created)"
)

// goqiteReceive is the query with which goqite picks the next message of a queue, as goqite writes
// it, with the value on the left of timeout, and goqiteReceiveTurned the same with the column on the
// left.
const (
	goqiteReceive       = "select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1"
	goqiteReceiveTurned = "select id from goqite where queue = ? and timeout <= ? and received < ? order by priority desc, created limit 1"
)

// goqiteQueries are the statements of goqite, a queue on SQLite, and statements shaped like them.
// The receive is goqite's, once as goqite writes its WHERE, with `? >= timeout`, and once with
// `timeout <= ?`, which must have the same access, as [TestAccessOf_golden] checks; and each of the
// two without its ORDER BY and its LIMIT, and the first without its LIMIT.
var goqiteQueries = []planQuery{
	{name: "receive in order", src: goqiteReceiveTurned, args: []any{"default", "2026-11-20T12:00:00.000Z", int64(3)}},
	{name: "receive in order as goqite writes it", src: goqiteReceive, args: []any{"default", "2026-11-20T12:00:00.000Z", int64(3)}},
	{name: "receive in order without a limit", src: strings.TrimSuffix(goqiteReceive, " limit 1"), args: []any{"default", "2026-11-20T12:00:00.000Z", int64(3)}},
	{name: "receive", src: "select id from goqite where queue = ? and timeout <= ? and received < ?",
		args: []any{"default", "2026-11-20T12:00:00.000Z", int64(3)}},
	{name: "receive as goqite writes it", src: "select id from goqite where queue = ? and ? >= timeout and received < ?",
		args: []any{"default", "2026-11-20T12:00:00.000Z", int64(3)}},
	{name: "by id", src: "select body from goqite where id = ?", args: []any{"m_0001"}},
	{name: "by queue", src: "select id from goqite where queue = 'default'"},
	{name: "by queue and priority", src: "select id from goqite where queue = ? and priority = ?", args: []any{"default", int64(1)}},
	{name: "by queue and a range of priorities", src: "select id from goqite where queue = 'default' and priority >= 1 and priority < 3"},
	{name: "extend", src: "update goqite set timeout = ? where queue = ? and id = ?",
		args: []any{"2026-11-20T12:05:00.000Z", "default", "m_0001"}},
	{name: "delete", src: "delete from goqite where queue = ? and id = ?", args: []any{"default", "m_0001"}},
	{name: "delete a queue", src: "delete from goqite where queue = 'default'"},
}

// planSchemas whose plans TestAccessOf_golden keeps.
var planSchemas = []planSchema{
	{name: "goqite", statements: []string{goqiteTable}, queries: goqiteQueries},
	{name: "goqite_index", statements: []string{goqiteTable, goqiteIndex}, queries: goqiteQueries},
	{
		name:       "goqite_two_indexes",
		statements: []string{goqiteTable, goqiteIndex, "create index goqite_queue_timeout on goqite (queue, timeout)"},
		queries:    goqiteQueries,
	},
	{
		name: "glue_sessions",
		statements: []string{
			"create table sessions (token text primary key, data blob not null, expiry real not null) strict, without rowid",
			"create index sessions_expiry_idx on sessions (expiry)",
		},
		queries: []planQuery{
			{name: "delete expired", src: "delete from sessions where expiry < ?", args: []any{1763640000.5}},
			{name: "by token", src: "select data, expiry from sessions where token = ?", args: []any{"abc"}},
			{name: "by token and expiry", src: "select data from sessions where token = ? and expiry > ?", args: []any{"abc", 1763640000.5}},
			{name: "a range of expiries", src: "select token from sessions where expiry >= 1763640000.0 and expiry <= 1763650000.0"},
			{name: "update by token", src: "update sessions set expiry = ? where token = ?", args: []any{1763650000.5, "abc"}},
			{name: "the sessions that expire first", src: "select token from sessions order by expiry limit 10"},
			{name: "the sessions that expire last", src: "select token from sessions where expiry > ? order by expiry desc limit ?", args: []any{1763640000.5, int64(10)}},
		},
	},
	{
		name: "roadmap",
		statements: []string{
			"create table t1 (v1 int primary key, v2 text not null, v3 real) strict, without rowid",
			"create index t1_v2_v3 on t1 (v2, v3 desc)",
		},
		queries: []planQuery{
			{name: "example", src: "select v1 from t1 where v2 = 'a' and v3 > 1.5"},
			{name: "first example of slice 13", src: "select v1, v2 from t1 order by v2 desc, v1 limit 10"},
			{name: "second example of slice 13", src: "select v1 from t1 where v2 > ? order by v1 limit ?", args: []any{"a", int64(10)}},
		},
	},
	{
		name:       "readme",
		statements: []string{indexTable, "create index ia on t (a)", "create index iab on t (a, b)"},
		queries: []planQuery{
			{name: "a", src: "select * from t where a = 10"},
			{name: "a and b", src: "select * from t where a = 10 and b = 'x'"},
			{name: "a and a range of b", src: "select * from t where a = 10 and b > 'x'"},
			{name: "a and a range of k", src: "select * from t where a = 10 and k < 2"},
			{name: "b", src: "select * from t where b = 'x'"},
			{name: "k and a", src: "select * from t where k = 2 and a = 10"},
		},
	},
	{
		name: "rules",
		statements: []string{
			"create table r (k int primary key, a int not null, b text, c real, d blob) strict, without rowid",
			"create index rk on r (k desc)",
			"create index ra on r (a)",
			"create index rb on r (b)",
			"create index rbc on r (b, c desc)",
			"create index rd on r (d)",
		},
		queries: []planQuery{
			{name: "no row for a comparison with null", src: "select k from r where a = null"},
			{name: "no row for a comparison with an argument of null", src: "select k from r where b > ?", args: []any{nil}},
			{name: "no row for is null on the primary key", src: "select k from r where k is null"},
			{name: "a get", src: "select a from r where k = ?", args: []any{int64(3)}},
			{name: "a get where an index pins more", src: "select a from r where k = 3 and a = 1"},
			{name: "the primary key wins a tie with an index", src: "select a from r where k > 3"},
			{name: "the first index created wins a tie", src: "select k from r where b = 'x'"},
			{name: "a range wins over no range at the same pins", src: "select k from r where b = 'x' and c > 1.5"},
			{name: "a range of a descending column", src: "select k from r where b = ? and c > ? and c <= ?", args: []any{"x", 1.5, 9.5}},
			{name: "is null pins a nullable column", src: "select k from r where b is null"},
			{name: "the first condition pins", src: "select k from r where b is null and b = 'x'"},
			{name: "the first of two equal pins", src: "select k from r where a = 1 and a = 2"},
			{name: "is null pins nothing on a column without null", src: "select k from r where a is null"},
			{name: "the primary key at the end of an index key", src: "select b from r where a = 1 and k > 3"},
			{name: "the open bound is tighter", src: "select a from r where k >= 3 and k > 3 and k < 9 and k <= 9"},
			{name: "bounds that hold no value", src: "select a from r where k > 5 and k < 3"},
			{name: "not equal pins nothing", src: "select k from r where a != 1"},
			{name: "is not null pins nothing", src: "select k from r where b is not null"},
			{name: "a later column of a key pins nothing", src: "select k from r where c = 1.5"},
			{name: "a blob", src: "select k from r where d = ?", args: []any{[]byte{1, 2}}},
			{name: "an update", src: "update r set c = ? where b = ? and c < ?", args: []any{2.5, "y", 0.5}},
			{name: "an update by a get", src: "update r set a = 2 where k = 3"},
			{name: "a delete", src: "delete from r where a = ? and k <= ?", args: []any{int64(1), int64(10)}},
			{name: "a delete of no row", src: "delete from r where k is null"},
			{name: "a value on the left bounds its column as the operator turned round", src: "select a from r where 3 < k and 9 >= k"},
			{name: "a value on the left pins and bounds an index", src: "select k from r where 'x' = b and ? > c", args: []any{9.5}},
			{name: "a get by a value on the left", src: "update r set a = 2 where 3 = k"},
			{name: "no row for null on the left", src: "delete from r where null <= a"},
			{name: "the order of the primary key", src: "select a from r order by k"},
			{name: "the reverse of the primary key", src: "select a from r order by k desc limit 3"},
			{name: "an index that delivers the order wins over every row and a sort", src: "select k from r order by a"},
			{name: "a sort of every row", src: "select k from r order by c"},
			{name: "a sort of every row with a limit", src: "select k from r order by c desc, k limit 5"},
			{name: "the first index created that delivers the order", src: "select k from r order by b desc"},
			{name: "an index backward for the reverse of its key", src: "select k from r order by b desc, c limit 2"},
			{name: "mixed directions are sorted", src: "select k from r order by b, c"},
			{name: "the order after a pinned column", src: "select k from r where b = 'x' order by c desc"},
			{name: "the order of a pinned column and the next", src: "select k from r where b is null order by b desc, c, k desc"},
			{name: "a range wins over the order without a limit", src: "select k from r where k > 3 order by a"},
			{name: "the order wins over a range with a limit", src: "select k from r where k > 3 order by a limit 1"},
			{name: "a range and the order by one index", src: "select k from r where a >= ? order by a desc limit ?", args: []any{int64(1), int64(4)}},
			{name: "more pins win over the order, also with a limit", src: "select k from r where b = 'x' order by a limit 1"},
			{name: "a column after the whole key is sorted", src: "select k from r order by a, k, b"},
			{name: "a get delivers every order", src: "select a from r where k = 3 order by c limit 1"},
			{name: "no row delivers every order", src: "select a from r where c > null order by d"},
			{name: "a limit without an order changes nothing", src: "select k from r where b = 'x' and c > 1.5 limit 1"},
		},
	},
}

// TestAccessOf_golden keeps the plans of the queries of [planSchemas] as golden files in
// testdata/plans, so that a change of the rules of the choice of an access shows as a diff of them.
// The flag -update rewrites them.
func TestAccessOf_golden(t *testing.T) {
	dir := filepath.Join("testdata", "plans")
	names := map[string]bool{}
	for _, s := range planSchemas {
		names[s.name+".txt"] = true
		got := planText(t, s)
		file := filepath.Join(dir, s.name+".txt")
		if *update {
			is.NotError(t, os.MkdirAll(dir, 0o755))
			is.NotError(t, os.WriteFile(file, []byte(got), 0o644))
			continue
		}
		want, err := os.ReadFile(file)
		if errors.Is(err, fs.ErrNotExist) {
			t.Errorf("no golden file %v of the schema %v: write it with go test ./internal/engine -run TestAccessOf_golden -update", file, s.name)
			continue
		}
		is.NotError(t, err)
		if diff := planDiff(string(want), got); diff != "" {
			t.Errorf("the plans of the schema %v differ from %v, which go test ./internal/engine -run TestAccessOf_golden -update rewrites:\n%v", s.name, file, diff)
		}
	}

	// goqite's receive has the same access whichever side of timeout its value is on.
	for _, s := range planSchemas {
		accesses := map[string]string{}
		for _, block := range strings.Split(planText(t, s), "\n## ")[1:] {
			name, _, _ := strings.Cut(block, "\n")
			_, access, _ := strings.Cut(block, "\naccess: ")
			accesses[name] = access
		}
		if want, ok := accesses["receive"]; ok {
			is.Equal(t, want, accesses["receive as goqite writes it"], "the access of goqite's receive in the schema "+s.name)
			is.Equal(t, accesses["receive in order"], accesses["receive in order as goqite writes it"], "the access of goqite's receive in order in the schema "+s.name)
		}
		// goqite's receive reads by goqite's index, in its order, wherever the index is there, also
		// beside an index on the queue and the timeout, which bounds a range and would need a sort.
		if strings.Contains(strings.Join(s.statements, "\n"), goqiteIndex) {
			is.Equal(t, "index goqite_queue_priority_created_idx (queue, priority desc, created, id), pins queue = 'default'\norder: delivered, forward\n",
				accesses["receive in order as goqite writes it"], "the access of goqite's receive in order in the schema "+s.name)
		}
	}

	entries, err := os.ReadDir(dir)
	is.NotError(t, err)
	for _, e := range entries {
		if names[e.Name()] || e.IsDir() || filepath.Ext(e.Name()) != ".txt" {
			continue
		}
		if *update {
			is.NotError(t, os.Remove(filepath.Join(dir, e.Name())))
			continue
		}
		t.Errorf("the golden file %v belongs to no schema: -update removes it", filepath.Join(dir, e.Name()))
	}
}

// planText of the schema s: a header with its statements, and a block for each query, which starts
// with "## " and the name of the query.
func planText(t *testing.T, s planSchema) string {
	t.Helper()
	ct, indexes := planTable(t, s.statements)
	var b strings.Builder
	b.WriteString("# The plans of the queries on the schema " + s.name + ", which TestAccessOf_golden in access_test.go checks.\n")
	b.WriteString("# Rewrite with: go test ./internal/engine -run TestAccessOf_golden -update\n\n")
	for _, src := range s.statements {
		b.WriteString(src + "\n")
	}
	seen := map[string]bool{}
	for _, q := range s.queries {
		if seen[q.name] {
			t.Fatalf("two queries of the schema %v are named %q", s.name, q.name)
		}
		seen[q.name] = true
		args := values(q.args)
		a, ok := engine.AccessOf(q.src, ct, indexes, args)
		if !ok {
			t.Fatalf("the query %q of the schema %v has no access: %v", q.name, s.name, q.src)
		}
		b.WriteString("\n## " + q.name + "\n" + q.src + "\n")
		if len(args) > 0 {
			texts := make([]string, len(args))
			for i, v := range args {
				texts[i] = planValue(v)
			}
			b.WriteString("args: " + strings.Join(texts, ", ") + "\n")
		}
		b.WriteString("access: " + planAccess(ct, indexes, a) + "\n")
		switch {
		case a.Delivered:
			b.WriteString("order: delivered, " + map[bool]string{false: "forward", true: "backward"}[a.Backward] + "\n")
		case a.Sorted:
			terms := make([]string, len(a.By))
			for i, c := range a.By {
				terms[i] = ct.GetColumns()[c.Position].GetName() + map[bool]string{true: " desc"}[c.Descending]
			}
			b.WriteString("order: sorted by " + strings.Join(terms, ", ") + "\n")
		}
	}
	return b.String()
}

// planTable of the statements: the table of the create table, and the indexes of the create indexes
// on it, in their order.
func planTable(t *testing.T, statements []string) (*s4qlpb.CreateTable, []*s4qlpb.CreateIndex) {
	t.Helper()
	plan := func(src string) engine.Plan {
		s, err := stmt.Check(src)
		is.NotError(t, err, src)
		return engine.NewPlan(s)
	}
	table, ok := plan(statements[0]).(*engine.CreateTable)
	is.True(t, ok, statements[0])
	ct := s4qlpb.CreateTable_builder{Table: new(table.Table.Value)}
	positions := map[string]int{}
	for i, c := range table.Columns {
		column := s4qlpb.Column_builder{Name: new(c.Name.Value), Type: new(c.Type)}
		if c.Nullable {
			column.Nullable = new(true)
		}
		ct.Columns = append(ct.Columns, column.Build())
		positions[c.Name.Fold()] = i
	}
	for _, p := range table.PrimaryKey {
		ct.PrimaryKey = append(ct.PrimaryKey, uint32(p))
	}
	var indexes []*s4qlpb.CreateIndex
	for _, src := range statements[1:] {
		index, ok := plan(src).(*engine.CreateIndex)
		is.True(t, ok, src)
		ci := s4qlpb.CreateIndex_builder{Index: new(index.Index.Value), Table: new(table.Table.Value)}
		for _, c := range index.Columns {
			position, ok := positions[c.Name.Fold()]
			is.True(t, ok, src)
			column := s4qlpb.IndexColumn_builder{Column: new(uint32(position))}
			if c.Descending {
				column.Descending = new(true)
			}
			ci.Columns = append(ci.Columns, column.Build())
		}
		indexes = append(indexes, ci.Build())
	}
	return ct.Build(), indexes
}

// planAccess in words: "no row", "every row", "get" with the value of each column of the primary
// key, or a scan of the primary key or of an index, with the columns of its key, its pinned columns,
// and its bounds, such as "index rbc (b, c desc, k), pins b = 'x', bounds 1.5 < c <= 9.5".
func planAccess(ct *s4qlpb.CreateTable, indexes []*s4qlpb.CreateIndex, a engine.Access) string {
	name := func(position uint32) string { return ct.GetColumns()[position].GetName() }
	key := func(index string) []string {
		var columns []string
		held := map[uint32]bool{}
		for _, ci := range indexes {
			if ci.GetIndex() != index {
				continue
			}
			for _, c := range ci.GetColumns() {
				held[c.GetColumn()] = true
				column := name(c.GetColumn())
				if c.GetDescending() {
					column += " desc"
				}
				columns = append(columns, column)
			}
		}
		for _, p := range ct.GetPrimaryKey() {
			if !held[p] {
				columns = append(columns, name(p))
			}
		}
		return columns
	}
	// column i of the key columns, or "column i" for one past the end of the key, which is an access
	// to a longer key than this test makes of the index.
	column := func(columns []string, i int) string {
		if i >= len(columns) {
			return fmt.Sprint("column ", i, " of a key of ", len(columns))
		}
		return strings.TrimSuffix(columns[i], " desc")
	}
	pinned := func(columns []string, vs []*s4qlpb.Value) string {
		pins := make([]string, len(vs))
		for i, v := range vs {
			pins[i] = column(columns, i) + " = " + planValue(v)
		}
		return strings.Join(pins, ", ")
	}

	switch {
	case a.None:
		return "no row"
	case a.Full:
		return "every row"
	case a.Point != nil:
		var columns []string
		for _, p := range ct.GetPrimaryKey() {
			columns = append(columns, name(p))
		}
		return "get, " + pinned(columns, a.Point)
	}
	s := "primary key"
	columns := key("")
	if a.Index != "" {
		s = "index " + a.Index
		columns = key(a.Index)
	}
	s += " (" + strings.Join(columns, ", ") + ")"
	if len(a.Equal) > 0 {
		s += ", pins " + pinned(columns, a.Equal)
	}
	if a.Lower == nil && a.Upper == nil {
		return s
	}
	less := func(open bool) string {
		if open {
			return " < "
		}
		return " <= "
	}
	s += ", bounds "
	if a.Lower != nil {
		s += planValue(a.Lower) + less(a.LowerOpen)
	}
	s += column(columns, len(a.Equal))
	if a.Upper != nil {
		s += less(a.UpperOpen) + planValue(a.Upper)
	}
	return s
}

// planValue as a literal of SQL: an integer, a real with a point or an exponent and the shortest
// digits that give it, a text in single quotes, a blob as x'...', or NULL.
func planValue(v *s4qlpb.Value) string {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return fmt.Sprint(v.GetInteger())
	case s4qlpb.Value_Real_case:
		r := v.GetReal()
		format := byte('f')
		if a := math.Abs(r); a != 0 && (a < 1e-6 || a >= 1e21) {
			format = 'g'
		}
		s := strconv.FormatFloat(r, format, -1, 64)
		if !strings.ContainsAny(s, ".eIN") {
			s += ".0"
		}
		return s
	case s4qlpb.Value_Text_case:
		return "'" + strings.ReplaceAll(v.GetText(), "'", "''") + "'"
	case s4qlpb.Value_Blob_case:
		return blobText(v.GetBlob())
	case s4qlpb.Value_Null_case:
		return "NULL"
	}
	return fmt.Sprint("a value of kind ", v.WhichKind())
}

// planDiff of the golden text want and the text got, by block: the header with the statements of the
// schema, and each query by its name. It is empty if the two are equal.
func planDiff(want, got string) string {
	if want == got {
		return ""
	}
	wantHeader, wantBlocks := planBlocks(want)
	gotHeader, gotBlocks := planBlocks(got)
	var b strings.Builder
	lines := func(prefix, s string) {
		for line := range strings.Lines(s) {
			b.WriteString(prefix + line)
		}
	}
	if wantHeader != gotHeader {
		b.WriteString("the statements of the schema changed:\n")
		lines("- ", wantHeader)
		lines("+ ", gotHeader)
	}
	wanted := map[string]string{}
	for _, block := range wantBlocks {
		if _, ok := wanted[block[0]]; ok {
			fmt.Fprintf(&b, "the golden file has two queries named %q\n", block[0])
		}
		wanted[block[0]] = block[1]
	}
	gotten := map[string]bool{}
	for _, block := range gotBlocks {
		gotten[block[0]] = true
	}
	for _, block := range wantBlocks {
		if !gotten[block[0]] {
			fmt.Fprintf(&b, "the query %q is gone:\n", block[0])
			lines("- ", block[1])
		}
	}
	for _, block := range gotBlocks {
		old, ok := wanted[block[0]]
		switch {
		case !ok:
			fmt.Fprintf(&b, "the query %q is new:\n", block[0])
			lines("+ ", block[1])
		case old != block[1]:
			fmt.Fprintf(&b, "the plan of the query %q changed:\n", block[0])
			b.WriteString(lineDiff(slices.Collect(strings.Lines(old)), slices.Collect(strings.Lines(block[1]))))
		}
	}
	if b.Len() == 0 {
		b.WriteString("the queries are in another order, or the file differs in its spaces\n")
	}
	return b.String()
}

// lineDiff of the lines before and after, by their longest common subsequence. A line of both is
// prefixed with "  ", a line of before alone with "- ", and a line of after alone with "+ ".
func lineDiff(before, after []string) string {
	// common[i][j] is the length of the longest common subsequence of before[i:] and after[j:].
	common := make([][]int, len(before)+1)
	for i := range common {
		common[i] = make([]int, len(after)+1)
	}
	for i := len(before) - 1; i >= 0; i-- {
		for j := len(after) - 1; j >= 0; j-- {
			if before[i] == after[j] {
				common[i][j] = common[i+1][j+1] + 1
			} else {
				common[i][j] = max(common[i+1][j], common[i][j+1])
			}
		}
	}
	var b strings.Builder
	i, j := 0, 0
	for i < len(before) || j < len(after) {
		switch {
		case i < len(before) && j < len(after) && before[i] == after[j]:
			b.WriteString("  " + before[i])
			i, j = i+1, j+1
		case j == len(after) || (i < len(before) && common[i+1][j] >= common[i][j+1]):
			b.WriteString("- " + before[i])
			i++
		default:
			b.WriteString("+ " + after[j])
			j++
		}
	}
	return b.String()
}

// planBlocks of a text of [planText]: the header before the first block, and each block as its name
// and its text.
func planBlocks(s string) (string, [][2]string) {
	parts := strings.Split(s, "\n## ")
	var blocks [][2]string
	for _, part := range parts[1:] {
		name, _, _ := strings.Cut(part, "\n")
		blocks = append(blocks, [2]string{name, "## " + part})
	}
	return parts[0], blocks
}

func TestEngine_events_order(t *testing.T) {
	t.Run("should say in the event of a query with ORDER BY whether its order was delivered or sorted, with the drawn direction only where it decides something, and nothing of the order for a statement without ORDER BY", func(t *testing.T) {
		h := &events{}
		i := newInstance(t, engine.Options{Logger: slog.New(h)})
		for _, src := range []string{
			"create table t (k int primary key, a int not null, b text) strict, without rowid",
			"insert into t values (1, 2, 'x')", "insert into t values (2, 1, 'x')", "insert into t values (3, 1, null)",
			"create index ta on t (a)",
		} {
			is.Equal(t, "", i.exec(src).err, src)
		}
		for _, test := range []struct {
			src string
			// access, index, and order of the event, and whether it has a direction.
			access, index, order string
			direction            bool
		}{
			{src: "select k from t order by a desc limit 2", access: "index", index: "ta", order: "delivered"},
			{src: "select k from t order by k", access: "full_scan", order: "delivered"},
			{src: "select k from t where k = 1 order by b", access: "primary_key", order: "delivered"},
			{src: "select k from t where b = null order by b", access: "none", order: "delivered"},
			{src: "select k from t order by b", access: "full_scan", order: "sorted", direction: true},
			{src: "select k from t where a = 1 order by b desc limit 1", access: "index", index: "ta", order: "sorted", direction: true},
			{src: "select k from t where a = 1 limit 1", access: "index", index: "ta", direction: true},
			{src: "select k from t limit 1", access: "full_scan", direction: true},
			{src: "update t set b = 'y' where a = 1", access: "index", index: "ta"},
			{src: "delete from t where k > 5", access: "primary_key"},
		} {
			got := i.queryValues(test.src, nil)
			is.Equal(t, "", got.err, test.src)
			event, _ := h.last(t)
			is.Equal(t, test.access, event[engine.AttrAccess], test.src)
			is.Equal(t, test.index, event[engine.AttrIndex], test.src)
			is.Equal(t, test.order, event[engine.AttrOrder], test.src)
			_, has := event[engine.AttrDirection]
			is.Equal(t, test.direction, has, test.src)
		}

		// A query that fails before it has an access says nothing of its order, and a path with a
		// direction draws none.
		got := i.queryValues("select k from t order by x", nil)
		is.Equal(t, "1/1: no such column: x", got.err)
		event, _ := h.last(t)
		_, has := event[engine.AttrOrder]
		is.True(t, !has)
		i.e.SetPath(engine.Path{Backward: true})
		is.Equal(t, "", i.queryValues("select k from t order by b", nil).err)
		event, _ = h.last(t)
		is.Equal(t, "sorted", event[engine.AttrOrder])
		_, has = event[engine.AttrDirection]
		is.True(t, !has)
	})
}
