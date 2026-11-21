package engine_test

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// orderedOrLimited reports whether s is a select with ORDER BY or LIMIT, of slice 13.
func orderedOrLimited(s stmt.Statement) bool {
	sel, ok := s.(*stmt.Select)
	return ok && (sel.OrderBy != nil || sel.Limited())
}

// ordered is a select with ORDER BY or LIMIT, in its parts, so that a test can ask SQLite for the
// rows of the same select without its LIMIT, each with its rank under the ORDER BY, and compare the
// rows of the engine with them as [ordered.differs] says.
type ordered struct {
	// columns of the select list, table, and the conditions of WHERE, or empty for none.
	columns, table, where string
	// by holds the terms of ORDER BY, or is empty for none.
	by string
	// limit is the number of LIMIT as the text writes it, "?" for a parameter, or empty for none.
	limit string
	// args of the parameters of where, and then of limit.
	args []any
}

// src is the text of the select.
func (q ordered) src() string {
	src := "select " + q.columns + " from " + q.table
	if q.where != "" {
		src += " where " + q.where
	}
	if q.by != "" {
		src += " order by " + q.by
	}
	if q.limit != "" {
		src += " limit " + q.limit
	}
	return src
}

// differs says how the answer got of the engine to the select differs from what SQLite, o, gives for
// it, or "" if it does not:
//
//   - The error and the columns are SQLite's, and so is the number of rows.
//   - With ORDER BY, the rows are in SQLite's order. SQLite gives the rows of the select without its
//     LIMIT, each with its dense rank under the ORDER BY, so two rows have one rank exactly if they
//     tie. The row of the engine at each place must be a row of the rank that SQLite has at that
//     place: where no rows tie, that is the same row at the same place, and the rows of a rank are
//     compared as a multiset, of which a LIMIT that cuts a rank leaves any part.
//   - Without ORDER BY, the rows are rows of the select without its LIMIT, each no more often than
//     that select gives it: with a LIMIT, any of them are right, and without one, they are all of
//     them.
func (q ordered) differs(t testing.TB, o *oracle, got answer) string {
	t.Helper()
	want := o.run(t, q.src(), q.args...)
	if want.err != "" || got.err != "" {
		if want.err != got.err {
			return fmt.Sprintf("the engine gives the error %q (%v), and SQLite %q", got.err, got.message, want.err)
		}
		return ""
	}
	if got.columns != want.columns {
		return fmt.Sprintf("the engine gives the columns %q, and SQLite %q", got.columns, want.columns)
	}
	if len(got.rows) != len(want.rows) {
		return fmt.Sprintf("the engine gives %v rows, %q, and SQLite %v, %q", len(got.rows), got.rows, len(want.rows), want.rows)
	}
	args := q.args
	if q.limit == "?" {
		args = args[:len(args)-1]
	}
	all := "select " + q.columns
	if q.by != "" {
		all += ", dense_rank() over (order by " + q.by + ")"
	}
	all += " from " + q.table
	if q.where != "" {
		all += " where " + q.where
	}
	if q.by != "" {
		all += " order by " + q.by
	}
	ranked := o.run(t, all, args...)
	if ranked.err != "" {
		t.Fatalf("%v: SQLite gives %v", all, ranked.err)
	}
	// rows and ranks of the select without its LIMIT, in SQLite's order: one rank for all without
	// ORDER BY.
	rows, ranks := make([]string, len(ranked.rows)), make([]string, len(ranked.rows))
	for n, row := range ranked.rows {
		rows[n] = row
		if q.by != "" {
			cut := strings.LastIndex(row, ",")
			rows[n], ranks[n] = row[:cut], row[cut+1:]
		}
	}
	if len(got.rows) > len(rows) {
		return fmt.Sprintf("the engine gives %v rows, and the select without its LIMIT has %v in SQLite", len(got.rows), len(rows))
	}
	for start := 0; start < len(got.rows); {
		end := start
		left := map[string]int{}
		for end < len(rows) && ranks[end] == ranks[start] {
			left[rows[end]]++
			end++
		}
		for n := start; n < min(end, len(got.rows)); n++ {
			if left[got.rows[n]]--; left[got.rows[n]] < 0 {
				return fmt.Sprintf("the engine gives the row %v at the place %v, where SQLite has the rows %q; the engine's rows are %q", got.rows[n], n, rows[start:end], got.rows)
			}
		}
		start = end
	}
	return ""
}

// orderSetup is the table of the tests of ORDER BY and LIMIT, with a column of each type, rows that
// tie in each column but the key, NULL in the columns that take it, and 0.0 and -0.0, and three
// indexes, of which tac holds a column descending.
var orderSetup = []string{
	"create table t (k int primary key, a int not null, b text, c real, d blob) strict, without rowid",
	"insert into t values (1, 1, 'm', 1.5, x'01')",
	"insert into t values (2, 2, null, 2.5, x'02')",
	"insert into t values (3, 0, 'a', null, null)",
	"insert into t values (4, 1, 'z', 0.5, x'')",
	"insert into t values (5, 2, 'm', 2.5, x'ff')",
	"insert into t values (6, 0, null, -0.0, x'00')",
	"insert into t values (7, 1, 'b', 1.5, null)",
	"insert into t values (8, 2, 'a', 0.0, x'0100')",
	"insert into t values (9, 0, 'z', null, x'01')",
	"insert into t values (10, 1, null, 3.5, x'7f')",
	"insert into t values (11, 2, 'b', -1.5, x'80')",
	"insert into t values (12, 0, 'm', 2.5, x'02')",
	"create index ta on t (a)",
	"create index tb on t (b)",
	"create index tac on t (a, c desc)",
}

func TestEngine_orderBy(t *testing.T) {
	// start an instance and SQLite with the statements of orderSetup.
	start := func(t *testing.T, options engine.Options) (*instance, *oracle) {
		t.Helper()
		i, o := newInstance(t, options), newOracle(t)
		t.Cleanup(o.close)
		for _, src := range orderSetup {
			is.Equal(t, "", i.exec(src).err, src)
			is.Equal(t, "", o.run(t, src).err, src)
		}
		return i, o
	}
	// keys of the rows of an answer whose first column is k.
	keys := func(a answer) string {
		var ks []string
		for _, row := range a.rows {
			k, _, _ := strings.Cut(row, ",")
			ks = append(ks, k)
		}
		return strings.Join(ks, " ")
	}

	t.Run("should give SQLite's rows in SQLite's order, by the access and with the sort or without that the rules choose, read no more rows than it must, and have the cache sort exactly where its event says sorted", func(t *testing.T) {
		h := &events{}
		i, o := start(t, engine.Options{Logger: slog.New(h)})
		for _, test := range []struct {
			q ordered
			// want is the access of the event, how the rows came in order, and the rows read and given.
			want string
		}{
			{ordered{columns: "k", table: "t", by: "k"}, "full_scan delivered reads 12 gives 12"},
			{ordered{columns: "k", table: "t", by: "k desc", limit: "3"}, "full_scan delivered reads 3 gives 3"},
			// An index that delivers the order wins over every row and a sort, with NULL first.
			{ordered{columns: "k, b", table: "t", by: "b"}, "index tb delivered reads 12 gives 12"},
			// The index backward, with NULL last.
			{ordered{columns: "k, b", table: "t", by: "b desc"}, "index tb delivered reads 12 gives 12"},
			{ordered{columns: "k, b", table: "t", by: "b desc", limit: "4"}, "index tb delivered reads 4 gives 4"},
			{ordered{columns: "b", table: "t", by: "b", limit: "5"}, "index tb delivered reads 5 gives 5"},
			// No index holds d, so every row is read and sorted, also for a LIMIT.
			{ordered{columns: "k, d", table: "t", by: "d"}, "full_scan sorted reads 12 gives 12"},
			{ordered{columns: "k", table: "t", by: "d desc", limit: "2"}, "full_scan sorted reads 12 gives 2"},
			{ordered{columns: "k", table: "t", by: "d", limit: "0"}, "full_scan sorted reads 0 gives 0"},
			{ordered{columns: "*", table: "t", by: "c desc, b, k desc", limit: "100"}, "full_scan sorted reads 12 gives 12"},
			// Both indexes on a pin it, and tac holds the rows of one a in the order of c.
			{ordered{columns: "k", table: "t", where: "a = 1", by: "c desc", limit: "2"}, "index tac delivered reads 2 gives 2"},
			{ordered{columns: "k", table: "t", where: "a = 1", by: "c", limit: "1"}, "index tac delivered reads 1 gives 1"},
			{ordered{columns: "k, c", table: "t", where: "a = 1", by: "c"}, "index tac delivered reads 4 gives 4"},
			{ordered{columns: "k", table: "t", where: "a = ?", by: "c desc", limit: "?", args: []any{int64(1), int64(2)}}, "index tac delivered reads 2 gives 2"},
			// The filter drops the first row of the index, so the query reads three rows to give two.
			{ordered{columns: "k", table: "t", where: "a = 1 and b is not null", by: "c desc", limit: "2"}, "index tac delivered reads 3 gives 2"},
			// Without a LIMIT the bound on k wins, and its three rows are sorted; with one, the order wins.
			{ordered{columns: "k", table: "t", where: "a = 1 and k > 1", by: "c desc"}, "index ta sorted reads 3 gives 3"},
			{ordered{columns: "k", table: "t", where: "a = 1 and k > 1", by: "c desc", limit: "1"}, "index tac delivered reads 1 gives 1"},
			// One column in the direction of tac and one against it: no index delivers that.
			{ordered{columns: "k", table: "t", by: "a, c"}, "full_scan sorted reads 12 gives 12"},
			{ordered{columns: "k", table: "t", by: "a desc, c"}, "index tac delivered reads 12 gives 12"},
			{ordered{columns: "k", table: "t", by: "a desc, c", limit: "7"}, "index tac delivered reads 7 gives 7"},
			// The pinned column of the ORDER BY orders nothing.
			{ordered{columns: "k", table: "t", where: "a = 2", by: "a desc, k desc", limit: "3"}, "index ta delivered reads 3 gives 3"},
			{ordered{columns: "k", table: "t", where: "b is null", by: "b, k"}, "index tb delivered reads 3 gives 3"},
			// A get and no row deliver every order.
			{ordered{columns: "k", table: "t", where: "k = 5", by: "b"}, "primary_key delivered reads 1 gives 1"},
			{ordered{columns: "k", table: "t", where: "k = 5", by: "b", limit: "0"}, "primary_key delivered reads 0 gives 0"},
			{ordered{columns: "k", table: "t", where: "a = null", by: "b"}, "none delivered reads 0 gives 0"},
			// A LIMIT without ORDER BY reads the rows that it gives, and a LIMIT above the rows all.
			{ordered{columns: "k", table: "t", limit: "5"}, "full_scan reads 5 gives 5"},
			{ordered{columns: "k", table: "t", limit: "0"}, "full_scan reads 0 gives 0"},
			{ordered{columns: "k", table: "t", limit: "9223372036854775807"}, "full_scan reads 12 gives 12"},
			{ordered{columns: "k", table: "t", where: "a = 2", limit: "100"}, "index ta reads 4 gives 4"},
			{ordered{columns: "k", table: "t", where: "a = 2", limit: "?", args: []any{int64(3)}}, "index ta reads 3 gives 3"},
			{ordered{columns: "k", table: "t", where: "c > 2.0", limit: "2"}, "full_scan reads 5 gives 2"},
		} {
			src := test.q.src()
			for _, path := range []engine.Path{{}, {Forward: true}, {Backward: true}} {
				i.e.SetPath(path)
				got := i.queryValues(src, values(test.q.args))
				is.Equal(t, "", test.q.differs(t, o, got), fmt.Sprintf("%v by the path %+v", src, path))
				event, _ := h.last(t)
				read := event[engine.AttrAccess]
				for _, attr := range []string{engine.AttrIndex, engine.AttrOrder} {
					if event[attr] != "" {
						read += " " + event[attr]
					}
				}
				read += " reads " + event[engine.AttrRowsRead] + " gives " + event[engine.AttrRows]
				// The rows that a query without ORDER BY reads depend on the direction where a filter drops
				// some of them before the LIMIT.
				if test.q.by != "" || test.q.where == "" || path == (engine.Path{Forward: true}) {
					is.Equal(t, test.want, read, fmt.Sprintf("%v by the path %+v", src, path))
				}
			}
			i.e.SetPath(engine.Path{})
			if test.q.by != "" || test.q.where != "" {
				checkPlanOf(t, i, src, test.q.args)
			}
		}
	})

	t.Run("should put the rows that tie in a sort in the order of the primary key or in its reverse, as the query draws, and leave those of a delivered order as the key of the access holds them", func(t *testing.T) {
		var options engine.Options
		options.Seed(7)
		i, o := start(t, options)
		run := func(q ordered) string {
			t.Helper()
			got := i.queryValues(q.src(), values(q.args))
			is.Equal(t, "", q.differs(t, o, got), q.src())
			return keys(got)
		}

		// No index holds c: NULL first, then -1.5, 0.0 with -0.0, 0.5, 1.5, 2.5, and 3.5.
		sortedQuery := ordered{columns: "k", table: "t", by: "c"}
		i.e.SetPath(engine.Path{Forward: true})
		is.Equal(t, "3 9 11 6 8 4 1 7 2 5 12 10", run(sortedQuery))
		i.e.SetPath(engine.Path{Backward: true})
		is.Equal(t, "9 3 11 8 6 4 7 1 12 5 2 10", run(sortedQuery))
		// The index ta holds the rows of one a in the order of k, and a path with a direction does not
		// turn them.
		forward, backward := ordered{columns: "k", table: "t", by: "a"}, ordered{columns: "k", table: "t", by: "a desc"}
		for _, path := range []engine.Path{{Forward: true}, {Backward: true}} {
			i.e.SetPath(path)
			is.Equal(t, "3 6 9 12 1 4 7 10 2 5 8 11", run(forward))
			is.Equal(t, "11 8 5 2 10 7 4 1 12 9 6 3", run(backward))
		}
		// A path that sorts turns the rows that tie by its direction, also where the index delivers.
		i.e.SetPath(engine.Path{Sort: true, Backward: true})
		is.Equal(t, "12 9 6 3 10 7 4 1 11 8 5 2", run(forward))
		i.e.SetPath(engine.Path{Index: "ta", Sort: true, Forward: true})
		is.Equal(t, "2 5 8 11 1 4 7 10 3 6 9 12", run(backward))

		// By the rules, 40 queries with a sort give both orders of the rows that tie, and 40 with a
		// delivered order one.
		i.e.SetPath(engine.Path{})
		seen := map[string]int{}
		for range 40 {
			seen[run(sortedQuery)]++
			is.Equal(t, "3 6 9 12 1 4 7 10 2 5 8 11", run(forward))
		}
		is.Equal(t, 2, len(seen), fmt.Sprint(seen))
		is.True(t, seen["3 9 11 6 8 4 1 7 2 5 12 10"] >= 5 && seen["9 3 11 8 6 4 7 1 12 5 2 10"] >= 5, fmt.Sprint(seen))
	})

	t.Run("should give SQLite's errors for a column of ORDER BY that the table does not have, after those of the select list and of WHERE, and then its own for an argument of LIMIT that is no integer from 0", func(t *testing.T) {
		// With a length limit of 1,000 bytes, as SQLite has it for the same connection.
		const limit = 1000
		i, o := newInstanceLimited(t, engine.Options{}, nil, limit), newOracleLimited(t, limit)
		t.Cleanup(o.close)
		for _, src := range orderSetup {
			is.Equal(t, "", i.exec(src).err, src)
			is.Equal(t, "", o.run(t, src).err, src)
		}
		long := strings.Repeat("x", limit+1)
		for _, test := range []struct {
			src  string
			args []any
			// want is the error of the engine: SQLite's, which SQLite must give too, or the engine's own,
			// which starts with "not supported: ".
			want string
		}{
			{src: "select k from t order by x", want: "1/1: no such column: x"},
			{src: `select k from t order by "x" desc limit 1`, want: `1/1: no such column: "x" - should this be a string literal in single-quotes?`},
			{src: "select k from t order by a, x, y", want: "1/1: no such column: x"},
			{src: "select k from nope order by x", want: "1/1: no such table: nope"},
			{src: "select y from t where z = 1 order by x", want: "1/1: no such column: y"},
			{src: "select k from t where z = 1 order by x", want: "1/1: no such column: z"},
			{src: "select k from t where z is null order by x", want: "1/1: no such column: z"},
			{src: "select k from t where z is null and a = 1 order by x", want: "1/1: no such column: z"},
			{src: "select k from t where a = 1 order by x limit 0", want: "1/1: no such column: x"},
			// The errors of the names come before those of the arguments.
			{src: "select k from t where a = ? order by x limit ?", args: []any{"text", 1.5}, want: "1/1: no such column: x"},
			{src: "select k from t where b = ? order by x limit ?", args: []any{long, 1.5}, want: "1/1: no such column: x"},
			// An argument that is too long is refused as SQLite refuses it when it binds it, the one of a
			// condition before the one of LIMIT, and both before the type of any.
			{src: "select k from t where b = ? order by k limit ?", args: []any{long, 1.5}, want: "18/18: string or blob too big"},
			{src: "select k from t where a = ? order by k limit ?", args: []any{"text", long}, want: "18/18: string or blob too big"},
			{src: "select k from t limit ?", args: []any{[]byte(long)}, want: "18/18: string or blob too big"},
			// The type of a value of a condition comes before the argument of LIMIT, in the order of the text.
			{src: "select k from t where a = ? order by k limit ?", args: []any{"text", 1.5},
				want: "not supported: argument 1, which is text, compared with the column t.a of type INT"},
			{src: "select k from t where a = ? order by k limit ?", args: []any{int64(1), 1.5},
				want: "not supported: argument 2, which is a real, for LIMIT, which takes an integer from 0"},
			{src: "select k from t limit ?", args: []any{2.0}, want: "not supported: argument 1, which is a real, for LIMIT, which takes an integer from 0"},
			{src: "select k from t limit ?", args: []any{"2"}, want: "not supported: argument 1, which is text, for LIMIT, which takes an integer from 0"},
			{src: "select k from t limit ?", args: []any{[]byte("2")}, want: "not supported: argument 1, which is a blob, for LIMIT, which takes an integer from 0"},
			{src: "select k from t limit ?", args: []any{nil}, want: "not supported: argument 1, which is NULL, for LIMIT, which takes an integer from 0"},
			{src: "select k from t limit ?", args: []any{int64(-1)}, want: "not supported: argument 1, which is a negative integer, for LIMIT, which takes an integer from 0"},
			{src: "select k from t order by b limit ?", args: []any{int64(-9223372036854775808)},
				want: "not supported: argument 1, which is a negative integer, for LIMIT, which takes an integer from 0"},
			// Also on a table without rows that pass, and for a get.
			{src: "select k from t where a = 9 limit ?", args: []any{"x"}, want: "not supported: argument 1, which is text, for LIMIT, which takes an integer from 0"},
			{src: "select k from t where k = ? limit ?", args: []any{int64(1), nil}, want: "not supported: argument 2, which is NULL, for LIMIT, which takes an integer from 0"},
		} {
			got := i.queryValues(test.src, values(test.args))
			if own, ok := strings.CutPrefix(test.want, "not supported: "); ok {
				is.Equal(t, "not supported", got.err, test.src)
				is.Equal(t, "s4ql: not supported: "+own, got.message, test.src)
				continue
			}
			is.Equal(t, test.want, got.err, test.src)
			is.Equal(t, test.want, o.run(t, test.src, test.args...).err, "SQLite: "+test.src)
		}
		// An integer from 0 runs, up to the largest.
		for _, n := range []int64{0, 1, 12, 13, 9223372036854775807} {
			q := ordered{columns: "k", table: "t", by: "b, k", limit: "?", args: []any{n}}
			got := i.queryValues(q.src(), values(q.args))
			is.Equal(t, "", q.differs(t, o, got), fmt.Sprint(q.src(), " with ", n))
			is.Equal(t, int(min(n, 12)), len(got.rows))
		}
	})
}
