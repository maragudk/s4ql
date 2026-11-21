package engine_test

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

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

// orderedOf the select s, with the arguments args of its parameters as SQLite binds them: its select
// list, its table, its conditions as [stmttest.SelectText] writes them, with a ? for each value, its
// ORDER BY, and its LIMIT, as a literal or a ?, and the arguments of the parameters of that text.
// args must have as many arguments as s has parameters.
func orderedOf(s *stmt.Select, args []any) ordered {
	q := ordered{columns: "*", table: stmttest.Quote(s.Table.Value)}
	if s.Columns != nil {
		var names []string
		for _, c := range s.Columns {
			names = append(names, stmttest.Quote(c.Value))
		}
		q.columns = strings.Join(names, ", ")
	}
	whole, bound := stmttest.SelectText(&stmt.Select{Table: s.Table, Where: s.Where}, args, "")
	q.where = strings.TrimPrefix(strings.TrimPrefix(whole, "select * from "+q.table), " where ")
	q.args = bound
	q.by = strings.Join(orderTerms(s.OrderBy), ", ")
	switch {
	case s.Limit.Parameter > 0:
		q.limit = "?"
		q.args = append(q.args, args[s.Limit.Parameter-1])
	case s.Limited():
		q.limit = fmt.Sprint(int64(s.Limit.Value.(stmt.Integer)))
	}
	return q
}

// orderTerms of the columns of an ORDER BY: each name in double quotes, with " desc" if descending.
func orderTerms(columns []stmt.OrderColumn) []string {
	var terms []string
	for _, c := range columns {
		term := stmttest.Quote(c.Name.Value)
		if c.Descending {
			term += " desc"
		}
		terms = append(terms, term)
	}
	return terms
}

// unlimited is the select of q without its LIMIT.
func (q ordered) unlimited() ordered {
	if q.limit == "?" {
		q.args = slices.Clip(q.args[:len(q.args)-1])
	}
	q.limit = ""
	return q
}

// plain reports whether the select has neither ORDER BY nor LIMIT, so that its rows are compared as a
// multiset, as [answer.String] gives them.
func (q ordered) plain() bool {
	return q.by == "" && q.limit == ""
}

// inOrderOf the access a, whose key is key, of the table ct, which reads the rows of the select s, of
// which q holds the parts: q with an ORDER BY of the order in which a gives the rows, which orders
// every two rows, so that SQLite gives the rows that a must give, in its order, and as many as the
// LIMIT of q keeps. Where a delivers the order of the ORDER BY of s, or s has none, it is the order
// of key, or its reverse if a is backward. Where a sorts the rows, it is the ORDER BY of s, and among
// rows that tie in it the order of the primary key, or its reverse if a is backward.
func (q ordered) inOrderOf(s *stmt.Select, a engine.Access, key []keyColumn, ct *s4qlpb.CreateTable) ordered {
	if !a.Sorted {
		q.by = strings.TrimPrefix(orderOf(key, a.Backward), " order by ")
		return q
	}
	terms := orderTerms(s.OrderBy)
	for _, position := range ct.GetPrimaryKey() {
		name := ct.GetColumns()[position].GetName()
		if slices.ContainsFunc(s.OrderBy, func(c stmt.OrderColumn) bool { return c.Name.Fold() == fold(name) }) {
			continue
		}
		term := stmttest.Quote(name)
		if a.Backward {
			term += " desc"
		}
		terms = append(terms, term)
	}
	q.by = strings.Join(terms, ", ")
	return q
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
	return q.compare(t, o, got, o.run(t, q.src(), q.args...))
}

// compare the answer got of the engine to the select with want, SQLite's answer on the database of o
// to the text of q or to another text of the same select, as [ordered.differs] says.
func (q ordered) compare(t testing.TB, o *oracle, got, want answer) string {
	t.Helper()
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
	args := q.unlimited().args
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
	ranked := o.unlimited(t, all, args...)
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

func TestEngine_orderBy(t *testing.T) {
	// orderSetup is the table of the tests of ORDER BY and LIMIT, with a column of each type, rows that
	// tie in each column but the key, NULL in the columns that take it, and 0.0 and -0.0, and three
	// indexes, of which tac holds a column descending.
	orderSetup := []string{
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

// pathsSetUp is the table of [TestEngine_orderBy_paths]: a column of each of the five types beside
// the key, NULL in each that takes it, rows that tie in each column but the key, 0.0 and -0.0, and
// five indexes, of which two hold a column descending and one holds three columns.
var pathsSetUp = []string{
	"create table u (k int primary key, i int, n integer not null, r real, s text, b blob) strict, without rowid",
	"insert into u values (1, 1, 2, 1.5, 'm', x'01')",
	"insert into u values (2, null, 1, null, 'a', x'00')",
	"insert into u values (3, 2, 2, -0.0, null, x'')",
	"insert into u values (4, 1, 0, 0.0, 'z', null)",
	"insert into u values (5, 0, 1, 2.5, 'm', x'ff')",
	"insert into u values (6, null, 2, 1.5, 'b', x'0100')",
	"insert into u values (7, 2, 0, null, null, x'01')",
	"insert into u values (8, 1, 1, -1.5, 'a', null)",
	"insert into u values (9, 0, 2, 2.5, 'É', x'7f')",
	"insert into u values (10, 2, 1, 0.5, 'm', x'80')",
	"insert into u values (11, -5, 0, 1.5, '', x'00')",
	"insert into u values (12, 1, 2, 3.5, 'b', x'01')",
	"insert into u values (13, null, 1, -0.0, 'z', x'')",
	"insert into u values (14, 9223372036854775807, 0, 1e300, 'é', x'02')",
	"insert into u values (-3, 0, 1, 0.5, 'm', null)",
	"create index ui on u (i)",
	"create index uns on u (n, s desc)",
	"create index ur on u (r desc)",
	"create index ub on u (b, i)",
	"create index uirs on u (i, r, s desc)",
}

// pathsWrites are the changes of the table of [pathsSetUp] that a transaction of
// [TestEngine_orderBy_paths] makes before it reads: a row that ties with another in every column but
// the key, a row of NULL, and changes of rows that the indexes hold.
var pathsWrites = []string{
	"insert into u values (20, 1, 2, 1.5, 'm', x'01')",
	"insert into u values (-7, null, 0, null, null, null)",
	"update u set i = 2, s = 'b' where n = 1",
	"delete from u where k = 3",
	"insert into u values (21, 0, 1, -0.0, 'a', x'')",
}

// pathsQueries of [TestEngine_orderBy_paths]: for each of its shapes, a WHERE or none and an ORDER BY
// or none, the select of k, of several columns, or of *, without LIMIT if it has an ORDER BY, and
// with a LIMIT of 0, 1, some, more than the rows, or a ? with such an argument, in turn, and for one
// shape in three a second time with a LIMIT of a ?.
func pathsQueries() []ordered {
	shapes := []struct {
		where, by string
		args      []any
	}{
		// Each type ascending and descending, with NULL and ties.
		{by: "k"}, {by: "k desc"}, {by: "i"}, {by: "i desc"}, {by: "n"}, {by: "n desc"}, {by: "r"}, {by: "r desc"},
		{by: "s"}, {by: "s desc"}, {by: "b"}, {by: "b desc"},
		// Several columns, in the directions of an index, against them, and of no index.
		{by: "n, s desc"}, {by: "n desc, s"}, {by: "n, s"}, {by: "i, r, s desc"}, {by: "i desc, r desc, s"}, {by: "b, i"},
		{by: "b desc, i"}, {by: "r desc, k"}, {by: "s, b desc, i"},
		// Pins and then the order.
		{where: "i = 1", by: "r"}, {where: "i = 1", by: "r desc, s"}, {where: "i = ?", by: "r, s desc", args: []any{int64(1)}},
		{where: "n = 2", by: "s desc"}, {where: "n = 1", by: "s"}, {where: "n = ?", by: "s, k desc", args: []any{int64(0)}},
		{where: "s is null", by: "r"}, {where: "i is null", by: "n desc"}, {where: "i is not null", by: "i"},
		{where: "n = 0 and i = 2", by: "r"}, {where: "b = x'01'", by: "i desc"}, {where: "k = 5", by: "s"}, {where: "i = null", by: "s"},
		// A range and the order.
		{where: "n > 0", by: "n desc"}, {where: "r >= 0.5", by: "r"}, {where: "r >= ?", by: "b", args: []any{0.5}},
		{where: "s < 'm'", by: "s desc, k"}, {where: "i > 0 and i < 3", by: "i desc, r"}, {where: "k > 3", by: "k desc"},
		{where: "b > x'00'", by: "b"}, {where: "n = 2 and s > 'a'", by: "s"},
		// A LIMIT without ORDER BY.
		{where: "i = 1"}, {where: "r > 0.0"}, {where: "n >= 1 and s is not null"},
		// Pins of every column of the key of an index, which holds the primary key.
		{where: "i = 1 and k = 4", by: "s"}, {where: "b = x'01' and i = 1 and k = 12", by: "r desc"},
	}
	limits := []string{"0", "1", "3", "100", "?"}
	limitArgs := []any{int64(0), int64(2), int64(5), int64(1000)}
	var qs []ordered
	for j, shape := range shapes {
		q := ordered{columns: []string{"k", "k, i, n, r, s, b", "*"}[j%3], table: "u", where: shape.where, by: shape.by, args: shape.args}
		if q.by != "" {
			qs = append(qs, q)
		}
		with := func(limit string) {
			l := q
			l.limit, l.args = limit, slices.Clone(q.args)
			if limit == "?" {
				l.args = append(l.args, limitArgs[(j/5)%len(limitArgs)])
			}
			qs = append(qs, l)
		}
		with(limits[j%len(limits)])
		if j%3 == 0 {
			with("?")
		}
	}
	return qs
}

// eventAccess of the access a, as the event of its statement names it, with the name of its index.
func eventAccess(a engine.Access) string {
	switch {
	case a.None:
		return "none"
	case a.Full:
		return "full_scan"
	case a.Index != "":
		return "index " + a.Index
	}
	return "primary_key"
}

func TestEngine_orderBy_paths(t *testing.T) {
	// start SQLite, and run the statements of pathsSetUp on it and on the instance i.
	start := func(t *testing.T, i *instance) *oracle {
		t.Helper()
		o := newOracle(t)
		t.Cleanup(o.close)
		for _, src := range pathsSetUp {
			is.Equal(t, "", i.exec(src).err, src)
			is.Equal(t, "", o.run(t, src).err, src)
		}
		return o
	}
	// write the changes of pathsWrites on the instance i and on SQLite o.
	write := func(t *testing.T, i *instance, o *oracle) {
		t.Helper()
		for _, src := range pathsWrites {
			is.Equal(t, "", i.exec(src).err, src)
			is.Equal(t, "", o.run(t, src).err, src)
		}
	}
	// some paths, of which those that a table has no index of read as the rules choose.
	some := []engine.Path{{}, {Forward: true}, {Backward: true}, {Sort: true}, {Full: true, Backward: true},
		{Primary: true, Sort: true, Forward: true}, {Index: "uns", Forward: true}, {Index: "ub", Sort: true, Backward: true}}
	// runSome runs each query of pathsQueries on the instance i by each of some, compares the rows with
	// those of SQLite o, and gives the number of runs.
	runSome := func(t *testing.T, i *instance, o *oracle) int {
		t.Helper()
		defer i.e.SetPath(engine.Path{})
		runs := 0
		for _, q := range pathsQueries() {
			for _, path := range some {
				i.e.SetPath(path)
				got := i.queryValues(q.src(), values(q.args))
				is.Equal(t, "", q.differs(t, o, got), fmt.Sprintf("%v with %v by the path %+v", q.src(), q.args, path))
				runs++
			}
		}
		return runs
	}

	t.Run("should give SQLite's rows in the order of the access by every path that can serve a query, with a sort and without, and say how in its event and in the plan of the cache, and read no more rows for a LIMIT of a sort than without it, and none for LIMIT 0", func(t *testing.T) {
		h := &events{}
		i := newInstance(t, engine.Options{Logger: slog.New(h)})
		o := start(t, i)
		table, columns := o.columns(t, "u")
		ct := createTableOf(table, columns)
		cis, _ := o.tableIndexes(t, table, columns)
		defer i.e.SetPath(engine.Path{})
		queries, runs := 0, map[string]int{}
		for _, q := range pathsQueries() {
			src, vals := q.src(), values(q.args)
			s, err := stmt.Check(src)
			is.NotError(t, err, src)
			sel := s.(*stmt.Select)
			want := o.run(t, src, q.args...)
			is.Equal(t, "", want.err, src)
			// Every forced path, and the rules in each direction, with a sort and without.
			cs := candidates(src, ct, cis, vals)
			for _, path := range []engine.Path{{Forward: true}, {Backward: true}, {Sort: true, Forward: true}, {Sort: true, Backward: true}} {
				a, ok := engine.PathAccessOf(src, ct, cis, vals, path)
				is.True(t, ok, src)
				cs = append(cs, candidate{path: path, access: a})
			}
			for _, k := range cs {
				what := fmt.Sprintf("%v with %v by the path %+v, which reads by %v", src, q.args, k.path, k.access)
				i.e.SetPath(k.path)
				got := i.queryValues(src, vals)
				is.Equal(t, "", q.compare(t, o, got, want), what)
				if !k.access.None {
					inOrder := q.inOrderOf(sel, k.access, accessKey(k.access, ct, cis), ct)
					in := o.run(t, inOrder.src(), inOrder.args...)
					is.Equal(t, "", in.err, inOrder.src())
					is.EqualSlice(t, in.rows, got.rows, what+", as "+inOrder.src())
				}

				event, _ := h.last(t)
				is.Equal(t, eventAccess(k.access), strings.TrimSpace(event[engine.AttrAccess]+" "+event[engine.AttrIndex]), what)
				order := ""
				switch {
				case sel.OrderBy == nil:
					is.True(t, !k.access.Delivered && !k.access.Sorted, what)
				case k.access.Delivered:
					is.True(t, !k.access.Sorted, what)
					order = "delivered"
				default:
					is.True(t, k.access.Sorted, what)
					order = "sorted"
				}
				is.Equal(t, order, event[engine.AttrOrder], what)
				runs[map[string]string{"": "without ORDER BY"}[order]+order]++

				if !k.access.None && k.access.Point == nil {
					sql, plan := scanPlan(t, i, ct.GetTable(), k.access)
					is.EqualSlice(t, wantPlans(k.access, ct, cis), plan, what+", which the cache runs as "+sql)
				}

				read := event[engine.AttrRowsRead]
				switch {
				case q.limit == "0" || q.limit == "?" && q.args[len(q.args)-1] == int64(0):
					is.Equal(t, "0", read, what)
					runs["limit 0"]++
				case q.limit != "" && k.access.Sorted:
					all := q.unlimited()
					is.Equal(t, "", all.differs(t, o, i.queryValues(all.src(), values(all.args))), what+", without its LIMIT")
					event, _ := h.last(t)
					is.Equal(t, event[engine.AttrRowsRead], read, what)
					runs["sorted with a limit"]++
				}
			}
			queries++
			runs["all"] += len(cs)
		}
		// The runs of each kind, so that a path that drops out shows.
		is.Equal(t, 107, queries)
		is.Equal(t, "map[all:1948 delivered:322 limit 0:337 sorted:1554 sorted with a limit:640 without ORDER BY:72]", fmt.Sprint(runs))
	})

	t.Run("should give SQLite's rows in order in a write transaction, which reads the rows that it wrote, and after its rollback", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		o := start(t, i)
		is.NotError(t, i.c.Begin(t.Context()))
		for _, src := range oracleBegin(false) {
			is.Equal(t, "", o.run(t, src).err, src)
		}
		write(t, i, o)
		is.Equal(t, 856, runSome(t, i, o))
		is.NotError(t, i.c.Rollback(t.Context()))
		for _, src := range oracleEnd(false, false) {
			is.Equal(t, "", o.run(t, src).err, src)
		}
		is.Equal(t, 856, runSome(t, i, o))
	})

	t.Run("should give SQLite's rows in order in a read-only transaction", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		o := start(t, i)
		write(t, i, o)
		is.NotError(t, i.c.BeginReadOnly(t.Context()))
		for _, src := range oracleBegin(true) {
			is.Equal(t, "", o.run(t, src).err, src)
		}
		is.Equal(t, 856, runSome(t, i, o))
		is.NotError(t, i.c.Commit(t.Context()))
		for _, src := range oracleEnd(true, true) {
			is.Equal(t, "", o.run(t, src).err, src)
		}
	})

	t.Run("should give SQLite's rows in order after a restart with an empty cache", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		o := start(t, i)
		write(t, i, o)
		i.restart(true, engine.Options{})
		is.Equal(t, o.state(t), i.state())
		is.Equal(t, 856, runSome(t, i, o))
	})

	t.Run("should give SQLite's rows in order on a database in memory", func(t *testing.T) {
		i := newMemoryInstance(t)
		o := start(t, i)
		write(t, i, o)
		is.Equal(t, 856, runSome(t, i, o))
	})
}
