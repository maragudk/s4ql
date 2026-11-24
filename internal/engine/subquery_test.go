package engine_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

// subquerySetUp is the tables of the tests of subqueries, with rows: t of a column of each type, u
// with an index that serves a subquery by y in the order of x, u1 of one column, for `*`, and e,
// which is empty.
var subquerySetUp = []string{
	"create table t (k int primary key, v text, w int, r real, b blob) strict, without rowid",
	"create table u (k integer primary key, x text, y int not null) strict, without rowid",
	"create index uy on u (y, x)",
	"create table u1 (k int primary key) strict, without rowid",
	"create table e (k int primary key) strict, without rowid",
	"insert into t values (1, 'a', 0, 1.5, x'01')",
	"insert into t values (2, 'b', 0, 2.5, x'02')",
	"insert into t values (3, 'c', 1, null, null)",
	"insert into t values (4, 'b', 1, 2.5, x'02')",
	"insert into u values (1, 'b', 7)",
	"insert into u values (2, 'c', 7)",
	"insert into u values (3, null, 8)",
	"insert into u1 values (2)",
}

// subqueryTestCall is a statement of the tests, with the arguments of its parameters, which runs
// through Exec if exec, and through Query otherwise.
type subqueryTestCall struct {
	src  string
	args []any
	exec bool
}

// call of the statement for [pair.step].
func (c subqueryTestCall) call() stmttest.Call {
	return stmttest.Call{Src: c.src, Args: c.args, Query: !c.exec, Supported: true}
}

// subqueryReads are selects with a subquery, which change nothing.
var subqueryReads = func() []subqueryTestCall {
	cs := []subqueryTestCall{
		// No row, one row, several rows without an order, with an order that decides, and with rows that
		// tie under it, of another table and of the table of the statement.
		{src: "select k from t where k = (select k from u where k = 5)"},
		{src: "select k from t where k = (select k from e)"},
		{src: "select * from t where k = (select k from u where x = 'b')"},
		{src: "select k from t where k = (select k from u)"},
		{src: "select k from t where k = (select k from u order by k desc limit 1)"},
		{src: "select k from t where k = (select k from u where y = 7 order by y)"},
		{src: "select k from t where k = (select k from u where y >= 7 order by y desc limit 1)"},
		{src: "select k from t where v = (select v from t where k = 4)"},
		{src: "select k from t where k = (select k from t where w = 1)"},
		{src: "select k from t where w = (select w from t order by w limit 1)"},
		// By the index of u, which delivers its order, and against it.
		{src: "select k from t where v = (select x from u where y = ? order by x desc limit 1)", args: []any{int64(7)}},
		{src: "select k from t where v = (select x from u where y = ? order by x limit 1)", args: []any{int64(7)}},
		{src: "select k from t where v = (select x from u where y = 7 order by k limit 1)"},
		// NULL from a row, and NULL compared on either side.
		{src: "select k from t where v = (select x from u where k = 3)"},
		{src: "select k from t where (select x from u where k = 3) = v"},
		{src: "select k from t where r = (select r from t where k = 3)"},
		{src: "select k from t where r != (select r from t where k = 3)"},
		{src: "select k from t where b = (select b from t where k = 2)"},
		{src: "select k from t where r < (select r from t where k = 2)"},
		// NULL from the first row under the ORDER BY, where later rows hold values: NULL comes first in
		// an ascending order, and a row with NULL in the column comes first by another column.
		{src: "select k from t where v = (select x from u order by x limit 1)"},
		{src: "select k from t where (select x from u order by x) <= v"},
		{src: "select k from t where v = (select x from u order by y desc limit 1)"},
		{src: "select k from t where v != (select x from u where y >= ? order by y desc, k)", args: []any{int64(7)}},
		{src: "select k from t where r = (select r from t order by r limit 1)"},
		{src: "select k from t where b <= (select b from t order by w desc, k limit 1)"},
		// Parameters before, inside, and after the subquery, in the order of the text.
		{src: "select k from t where w = ? and k = (select k from u where y = ? and x = ?) and v = ?", args: []any{int64(0), int64(7), "b", "a"}},
		{src: "select k from t where (select k from u where x > ? limit ?) <= k limit ?", args: []any{"a", int64(1), int64(2)}},
		// Two subqueries.
		{src: "select k from t where k >= (select k from u where x = 'b') and k < (select k from u where y = 8)"},
		// `*` of one column, of several, and a table or a column that does not exist.
		{src: "select k from t where k = (select * from u1)"},
		{src: "select k from t where k = (select * from u)"},
		{src: "select k from t where (select * from u) > k"},
		{src: "select k from t where nope is null and k = (select * from u)"},
		{src: "select nope from t where k = (select * from u)"},
		{src: "select k from t where k = (select k from nope)"},
		{src: "select nope from t where k = (select k from nope)"},
		{src: "select k from t where k = (select nope from u)"},
		{src: "select k from t where k = (select k from u where nope = 1)"},
		{src: "select k from t where k = (select k from u where nope is null order by nope2)"},
		{src: "select k from t where k = (select k from u order by nope)"},
		{src: "select k from t where nope = (select nope2 from u)"},
		{src: "select k from t where (select nope2 from u) = nope"},
		{src: "select k from t where nope is null and k = (select nope2 from u)"},
		{src: "select k from t where k = (select nope from u) order by nope2"},
		{src: "select k from nope where k = (select k from nope2)"},
		// Correlated: a name of t in a subquery of u, after the errors that SQLite gives for the names.
		{src: "select k from t where k = (select v from u)"},
		{src: "select k from t where k = (select k from u where w = 0)"},
		{src: "select k from t where k = (select k from u order by r)"},
		{src: "select k from t where k = (select v from u where nope = 1)"},
		{src: "select k from t where k = (select v from u where w = 0 order by r)"},
		{src: "select k from t where k = (select k from u where w is null and nope is null)"},
		{src: "select k from t where k = (select v from u) order by nope"},
		// LIMIT of the subquery: 0, arguments of each kind, and those that s4ql refuses.
		{src: "select k from t where k = (select k from u order by k limit 0)"},
		{src: "select k from t where k = (select k from u order by k limit ?)", args: []any{int64(0)}},
		{src: "select k from t where k = (select k from u order by k limit ?)", args: []any{int64(2)}},
		{src: "select k from t where k = (select k from u order by k limit ?)", args: []any{int64(-1)}},
		{src: "select k from t where k = (select k from u order by k limit ?)", args: []any{"x"}},
		{src: "select k from t where k = (select k from u order by k limit ?)", args: []any{nil}},
		{src: "select k from t where k = (select k from u order by k limit ?)", args: []any{1.5}},
		// Values of another type than their column, outside the subquery and inside it.
		{src: "select k from t where v = (select k from u)"},
		{src: "select k from t where k = 'x' and k = (select k from u)"},
		{src: "select k from t where k = (select k from u where x = 1)"},
		{src: "select k from t where k = (select k from u where x = ?) and v = 2", args: []any{int64(1)}},
		// ORDER BY and LIMIT of the statement.
		{src: "select k, v from t where v >= (select x from u where k = 1) order by v, k limit 2"},
		{src: "select k from t where w = (select y from u where k = 9) order by k desc limit 1"},
		{src: "select * from t where w <= (select k from u1) order by w desc"},
		// A statement that can give no row, whose subquery the engine does not compute: LIMIT 0 of the
		// statement, as a literal and as an argument, and another condition that is never true.
		{src: "select k from t where k = (select k from u1) limit 0"},
		{src: "select k from t where k = (select k from u1) limit ?", args: []any{int64(0)}},
		{src: "select k from t where k = (select k from u1) order by k limit 0"},
		{src: "select k from t where k is null and k = (select k from u1) order by k"},
		{src: "select k from t where w = ? and k = (select k from u1) order by k limit 2", args: []any{nil}},
	}
	// Every operator, in each of its spellings, on both sides.
	for _, op := range []string{"=", "==", "!=", "<>", "<", "<=", ">", ">="} {
		cs = append(cs,
			subqueryTestCall{src: "select k from t where k " + op + " (select k from u where x = 'b')"},
			subqueryTestCall{src: "select k from t where (select k from u where x = 'b') " + op + " k"},
			subqueryTestCall{src: "select k from t where v " + op + " (select x from u where k = 3)"})
	}
	return cs
}()

// subqueryWrites are updates and deletes with a subquery, in an order in which each changes what it
// is meant to.
var subqueryWrites = []subqueryTestCall{
	// NULL from the first row under the ORDER BY, where later rows hold values, which changes no row.
	{src: "update t set w = 7 where v = (select x from u order by x limit 1) returning k"},
	{src: "update t set w = 7 where v >= (select x from u order by y desc)", exec: true},
	{src: "delete from t where r = (select r from t order by r limit 1) returning k"},
	{src: "delete from t where (select b from t order by w desc, k limit 1) = b"},
	// The table that the subquery reads, as it is before the statement.
	{src: "update t set w = 9 where k = (select k from t where w = 0 order by k limit 1) returning *"},
	{src: "update t set w = 5 where w = (select w from t order by k limit 1)", exec: true},
	{src: "update t set v = ? where k = (select k from u where x = ?) returning v, k", args: []any{"z", "c"}},
	{src: "update t set r = ? where (select r from t where k = ?) = r", args: []any{9.5, int64(2)}, exec: true},
	// Rows that tie, with another value each.
	{src: "update t set w = 6 where k = (select k from u where y = 7) returning k"},
	// The errors: of `*`, of a type, of NOT NULL, of names, correlated, and of a missing table before
	// RETURNING.
	{src: "update t set w = 1 where k = (select * from u)"},
	{src: "update t set w = 1 where (select * from u) != k"},
	{src: "delete from t where (select * from u) <> k"},
	// `*` with != or <> on the right, and with another operator on the left, which are SQLite's
	// `sub-select returns 3 columns - expected 1`.
	{src: "update t set w = 1 where k != (select * from u)"},
	{src: "update t set w = 1 where k <> (select * from u)"},
	{src: "delete from t where k != (select * from u)"},
	{src: "delete from t where k <> (select * from u) returning k"},
	{src: "update t set w = 1 where (select * from u) = k"},
	{src: "delete from t where (select * from u) < k"},
	{src: "delete from t where k = (select * from u) and nope = 1"},
	{src: "delete from t where k = (select * from u) returning nope"},
	{src: "delete from t where k = (select * from u) and w = (select * from u)"},
	{src: "update t set w = 1 where v = (select k from u)"},
	{src: "update u set y = null where k = (select k from u where x = 'b')"},
	{src: "update u set y = null where k = (select k from u where x = 'nope')"},
	{src: "update t set nope = 1 where k = (select k from nope)"},
	{src: "update t set w = 1 where nope = 1 and k = (select k from nope)"},
	{src: "update t set w = 1 where k = (select k from nope) returning nope"},
	{src: "update t set w = 1 where (select k from nope) = nope"},
	{src: "update t set w = 1 where nope is null and k = (select nope2 from u)"},
	{src: "update t set w = 1 where k = (select v from u)"},
	{src: "update t set w = 1 where k = (select v from u) returning nope"},
	{src: "delete from t where k = (select k from u where w = 0)"},
	// A delete of the row that the subquery picks, of rows that tie, and of the table that it reads.
	{src: "delete from t where k = (select k from u where y = 7) returning k"},
	{src: "delete from t where k >= (select k from u1)"},
	{src: "delete from u where k = (select k from u order by k limit 1) returning *"},
}

func TestEngine_subquery(t *testing.T) {
	t.Run("should give what SQLite gives for a subquery, with its errors, on the core and in memory, and draw among the rows that tie", func(t *testing.T) {
		ties := []string{
			"reached: subquery with rows that tie for the first, whose value was that of SQLite's first",
			"reached: subquery with rows that tie for the first, whose value was that of another",
		}
		y := tally{}
		// The calls run on new instances until the engine has given both of a subquery whose rows tie,
		// each a draw of the engine from the sequence of its seed.
		for round := 0; round < 2 || slices.ContainsFunc(ties, func(k string) bool { return y[k] == 0 }); round++ {
			if round == 20 {
				t.Fatalf("a subquery whose rows tie gave one of them only in 20 rounds: %v", y)
			}
			for _, i := range []*instance{newInstance(t, engine.Options{}), newMemoryInstance(t)} {
				p := newPair(t, i, y)
				for _, src := range subquerySetUp {
					is.Equal(t, "", p.step(stmttest.Call{Src: src, Supported: true}), src)
				}
				for _, c := range append(slices.Clone(subqueryReads), subqueryWrites...) {
					if failure := p.step(c.call()); failure != "" {
						t.Fatal(failure)
					}
				}
				is.Equal(t, p.o.state(t), i.state())
			}
		}
		for _, k := range []string{"reached: subquery whose value was NULL", "reached: subquery whose value was that of its one first row", "reached: subquery that the engine did not compute, of a select that gave no row", "excused: " + excusedType} {
			t.Log(k, y[k])
			is.True(t, y[k] > 0, k)
		}
	})

	t.Run("should run a subquery in a transaction on its state, keep its change with the commit, and take it back with the roll back", func(t *testing.T) {
		for _, commit := range []bool{true, false} {
			i := newInstance(t, engine.Options{})
			p := newPair(t, i, tally{})
			for _, src := range subquerySetUp {
				is.Equal(t, "", p.step(stmttest.Call{Src: src, Supported: true}), src)
			}
			for _, c := range []*engine.Conn{i.c, p.other.c} {
				is.NotError(t, c.Begin(t.Context()))
			}
			for _, src := range oracleBegin(false) {
				is.Equal(t, "", p.o.run(t, src).err, src)
			}
			p.inTx = true
			// A change of the transaction, which the subqueries after it read.
			is.Equal(t, "", p.step(stmttest.Call{Src: "insert into u values (4, 'a', 9)", Supported: true}))
			for _, c := range append(slices.Clone(subqueryReads), subqueryWrites...) {
				if failure := p.step(c.call()); failure != "" {
					t.Fatal(failure)
				}
			}
			for _, c := range []*engine.Conn{i.c, p.other.c} {
				if commit {
					is.NotError(t, c.Commit(t.Context()))
				} else {
					is.NotError(t, c.Rollback(t.Context()))
				}
			}
			for _, src := range oracleEnd(false, commit) {
				is.Equal(t, "", p.o.run(t, src).err, src)
			}
			p.inTx = false
			is.Equal(t, p.o.state(t), i.state())
			// After a restart with an empty cache, the log gives the same rows, and the subqueries read
			// them.
			i.restart(true, engine.Options{})
			p.other.restart(true, engine.Options{})
			is.Equal(t, p.o.state(t), i.state())
			for _, c := range subqueryReads {
				if failure := p.step(c.call()); failure != "" {
					t.Fatal(failure)
				}
			}
		}
	})

	t.Run("should read a subquery in a read-only transaction, and refuse an update or a delete with one after the errors of its names, as SQLite does", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		p := newPair(t, i, nil)
		for _, src := range subquerySetUp {
			is.Equal(t, "", p.step(stmttest.Call{Src: src, Supported: true}), src)
		}
		for _, c := range []*engine.Conn{i.c, p.other.c} {
			is.NotError(t, c.BeginReadOnly(t.Context()))
		}
		for _, src := range oracleBegin(true) {
			is.Equal(t, "", p.o.run(t, src).err, src)
		}
		p.inTx, p.readOnly = true, true
		for _, c := range append(slices.Clone(subqueryReads), subqueryWrites...) {
			if failure := p.step(c.call()); failure != "" {
				t.Fatal(failure)
			}
		}
	})

	t.Run("should give SQLite's rows for every pair of types of a column and of the column of its subquery, or refuse the pair as of other types", func(t *testing.T) {
		types := []string{"int", "integer", "real", "text", "blob"}
		// Values that SQLite's affinities would make equal across the types: 1, 1.0, '1', and x'31'.
		values := []string{"1", "1", "1.0", "'1'", "x'31'"}
		setUp := []string{
			"create table a (k int primary key, i int, n integer, r real, s text, b blob) strict, without rowid",
			"create table a2 (k int primary key, i int, n integer, r real, s text, b blob) strict, without rowid",
			"insert into a values (1, " + strings.Join(values, ", ") + ")",
			"insert into a values (2, 2, 2, 2.0, '2', x'32')",
			"insert into a values (3, null, null, null, null, null)",
			"insert into a2 values (1, " + strings.Join(values, ", ") + ")",
			"insert into a2 values (2, null, null, null, null, null)",
		}
		columns := []string{"i", "n", "r", "s", "b"}
		y := tally{}
		p := newPair(t, newInstance(t, engine.Options{}), y)
		for _, src := range setUp {
			is.Equal(t, "", p.step(stmttest.Call{Src: src, Supported: true}), src)
		}
		for x, outer := range columns {
			for z, inner := range columns {
				for _, src := range []string{
					"select k from a where " + outer + " = (select " + inner + " from a2 where k = 1)",
					"select k from a where (select " + inner + " from a2 where k = ?) <= " + outer,
					"select k from a where " + outer + " != (select " + inner + " from a2 where k = 2)",
					// Row 2 has the values that the update gives it.
					"update a set r = 2.0 where k = 2 and " + outer + " >= (select " + inner + " from a2 order by k limit 1) returning k",
				} {
					args := []any{}
					if strings.Contains(src, "?") {
						args = []any{int64(1)}
					}
					if failure := p.step(stmttest.Call{Src: src, Args: args, Query: true, Supported: true}); failure != "" {
						t.Fatal(failure)
					}
					same := storageClass(strings.ToUpper(types[x])) == storageClass(strings.ToUpper(types[z]))
					is.Equal(t, same, p.last.err == "", src, p.last.err)
					if !same {
						is.Equal(t, fmt.Sprintf("s4ql: not supported: subquery 1, of the column a2.%v of type %v, compared with the column a.%v of type %v",
							inner, strings.ToUpper(types[z]), outer, strings.ToUpper(types[x])), p.last.message, src)
					}
				}
			}
		}
		is.Equal(t, 4*(25-7), y["excused: "+excusedType])
	})

	t.Run("should end a subquery with the error of its context, and change nothing", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		for _, src := range subquerySetUp {
			is.Equal(t, "", i.exec(src).err, src)
		}
		before := i.state()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		for _, src := range []string{"select k from t where k = (select k from u)", "update t set w = 3 where k = (select k from u order by k limit 1)", "delete from t where k >= (select k from u1)"} {
			_, err := i.c.Exec(ctx, src)
			is.True(t, errors.Is(err, context.Canceled), src)
		}
		is.Equal(t, before, i.state())
	})

	t.Run("should end a subquery with the error of its context when it ends while the subquery reads, and change nothing", func(t *testing.T) {
		// s has 80 rows, each with w = 1, and its subquery by v = 'v40' reads 40 rows at least, in
		// either direction, and looks at its context at each, before it gives 1. The statement reads the
		// 4 rows of t at most.
		setUp := slices.Clone(subquerySetUp)
		setUp = append(setUp, "create table s (k int primary key, v text, w int) strict, without rowid")
		for k := range 80 {
			setUp = append(setUp, fmt.Sprintf("insert into s values (%v, 'v%v', 1)", k, k))
		}
		for _, src := range []string{
			"select k from t where w = (select w from s where v = 'v40')",
			"update t set r = 0.5 where w = (select w from s where v = 'v40')",
			"delete from t where (select w from s where v = 'v40') = w",
		} {
			i, o := newInstance(t, engine.Options{}), newOracle(t)
			t.Cleanup(o.close)
			for _, src := range setUp {
				is.Equal(t, "", i.exec(src).err, src)
				is.Equal(t, "", o.run(t, src).err, src)
			}
			before := i.state()
			// The context is canceled at each call of its Err in turn, until the statement no longer
			// calls it as often, and succeeds.
			canceled := 0
			for n := int64(0); ; n++ {
				_, err := i.c.Exec(newFlipContext(t.Context(), n), src)
				if err == nil {
					break
				}
				is.True(t, errors.Is(err, context.Canceled), fmt.Sprint(src, ": the error of a canceled statement: ", err))
				is.Equal(t, before, i.state(), fmt.Sprint(src, ": the state after a cancel at the call ", n))
				canceled++
				is.True(t, n < 100_000, "the statement succeeds once its context is no longer canceled")
			}
			is.True(t, canceled > 40, fmt.Sprint(src, ": the statement is canceled at each of its calls of Err, which are more than the 40 rows that its subquery reads: ", canceled))
			is.Equal(t, "", o.run(t, src).err, src)
			is.Equal(t, o.state(t), i.state(), src)
		}
	})

	t.Run("should run goqite's receive without its arithmetic on goqite's table, two in a row picking two messages, and none on an empty queue", func(t *testing.T) {
		p := newPair(t, newInstance(t, engine.Options{}), tally{})
		for _, src := range []string{goqiteTable, goqiteIndex} {
			is.Equal(t, "", p.step(stmttest.Call{Src: src, Supported: true}), src)
		}
		receive := "update goqite set timeout = ?, received = ? where queue = ? and id = (select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1) returning id, body, received"
		now, later := "2026-11-24T12:00:00.000Z", "2026-11-24T12:01:00.000Z"
		args := []any{later, int64(1), "default", "default", now, int64(3)}
		is.Equal(t, "", p.step(stmttest.Call{Src: receive, Args: args, Query: true, Supported: true}))
		is.Equal(t, 0, len(p.last.rows))
		send := "insert into goqite (id, created, updated, queue, body, timeout, received, priority) values (?, ?, ?, ?, ?, ?, 0, ?)"
		// m_2 and m_3 have the highest priority, and m_3 the earlier time; m_4 ties with m_3 on both.
		for _, m := range []struct {
			id, created string
			priority    int64
		}{{"m_1", "2026-11-24T11:50:00.000Z", 0}, {"m_2", "2026-11-24T11:52:00.000Z", 1}, {"m_3", "2026-11-24T11:51:00.000Z", 1}, {"m_4", "2026-11-24T11:51:00.000Z", 1}, {"o_1", "2026-11-24T11:40:00.000Z", 5}} {
			queue := "default"
			if m.id == "o_1" {
				queue = "other"
			}
			is.Equal(t, "", p.step(stmttest.Call{Src: send, Args: []any{m.id, m.created, m.created, queue, []byte("body " + m.id), now, m.priority}, Supported: true}))
		}
		var received []string
		for range 5 {
			is.Equal(t, "", p.step(stmttest.Call{Src: receive, Args: args, Query: true, Supported: true}))
			received = append(received, strings.Join(p.last.rows, " "))
		}
		is.True(t, received[0] == `"m_3",x'626f6479206d5f33',1` || received[0] == `"m_4",x'626f6479206d5f34',1`, received[0])
		is.True(t, received[1] == `"m_3",x'626f6479206d5f33',1` || received[1] == `"m_4",x'626f6479206d5f34',1`, received[1])
		is.True(t, received[0] != received[1])
		is.EqualSlice(t, []string{`"m_2",x'626f6479206d5f32',1`, `"m_1",x'626f6479206d5f31',1`, ""}, received[2:])
		is.Equal(t, p.o.state(t), p.i.state())
	})
}

// subqueried reports whether s is a select, an update, or a delete with a scalar subquery.
func subqueried(s stmt.Statement) bool {
	switch s := s.(type) {
	case *stmt.Select:
		return s.Subqueries != nil
	case *stmt.Update:
		return s.Subqueries != nil
	case *stmt.Delete:
		return s.Subqueries != nil
	}
	return false
}

// subqueryCall is what a [pair] knows of the scalar subqueries of a call before it runs, from SQLite
// on the state before it: for each subquery, the texts of the values that it may give, as
// [oracle.run] writes values, and the text of the value of the row that SQLite's select of the
// subquery alone gives first.
//
// The value of a subquery is that of the first row that it gives, and where its ORDER BY does not
// decide which row that is, none or one whose rows tie, any row that a legal order puts first may
// give it: every row that the subquery's WHERE finds without ORDER BY, and every row that ties with
// the first under it. The engine draws among them as for the order of a select, and SQLite takes the
// first of the order of its plan. So the pair checks that the value of each instance is among the
// candidates, and where a subquery has more than one candidate, the call is undecided: the other
// instance uses the values of the instance in place of its own, as [pair.force] says, and SQLite runs
// the statement with each subquery replaced by a parameter bound to the value of the instance, as
// [subqueryCall.flatten] gives it, so that the three hold the same state after it. Where every
// subquery has one candidate, SQLite's value is that one, so the flattened statement runs in SQLite
// too, and the comparisons of the rows of a select with ORDER BY or LIMIT read a statement without a
// subquery.
type subqueryCall struct {
	src        string
	s          stmt.Statement
	subs       []stmt.Select
	args       []any
	candidates [][]string
	first      []string
}

// subqueriesOf the call c of the pair, on SQLite's state before it: what [subqueryCall] says, or nil
// for a call without a subquery, with another number of arguments than parameters, or with an
// argument that no column holds. The candidates of a subquery that SQLite does not run, or that
// s4ql refuses for its names or its LIMIT, are nil.
func (p *pair) subqueriesOf(c stmttest.Call) *subqueryCall {
	t, o := p.t, p.o
	t.Helper()
	s, err := stmt.Check(c.Src)
	if err != nil || s == nil || !subqueried(s) {
		return nil
	}
	args := oracleArgs(c.Args)
	if len(c.Args) != s.Parameters() || slices.ContainsFunc(c.Args, func(a any) bool { return refusal(a) != "" }) {
		return nil
	}
	sc := &subqueryCall{src: c.Src, s: s, subs: subqueriesIn(s), args: args}
	for _, q := range sc.subs {
		candidates, first := subqueryCandidates(t, o, c.Src, q, args)
		sc.candidates, sc.first = append(sc.candidates, candidates), append(sc.first, first)
	}
	return sc
}

// subqueriesIn the select, the update, or the delete s.
func subqueriesIn(s stmt.Statement) []stmt.Select {
	switch s := s.(type) {
	case *stmt.Select:
		return s.Subqueries
	case *stmt.Update:
		return s.Subqueries
	case *stmt.Delete:
		return s.Subqueries
	}
	return nil
}

// subqueryCandidates of the subquery q of the statement of the text src, with the arguments args of
// the statement, on the database of o: the texts of the values that it may give, each once, and the
// text of the value that SQLite's select of its text alone gives first; nil and "" for a subquery
// whose table SQLite does not have, of `*` of more than one column, with a name that its table does
// not have, or with an argument of LIMIT that s4ql refuses.
func subqueryCandidates(t testing.TB, o *oracle, src string, q stmt.Select, args []any) ([]string, string) {
	t.Helper()
	_, columns := o.columns(t, q.Table.Value)
	if columns == nil || q.Columns == nil && len(columns) != 1 {
		return nil, ""
	}
	result := q.Columns
	if result == nil {
		result = []stmt.Name{{Value: columns[0].Name}}
	}
	names := slices.Clone(result)
	for _, c := range q.Where {
		names = append(names, c.Column)
	}
	for _, c := range q.OrderBy {
		names = append(names, c.Name)
	}
	if slices.ContainsFunc(names, func(n stmt.Name) bool { return stmttest.ColumnOf(columns, n) < 0 }) || limitRule(&q, args) != "" {
		return nil, ""
	}
	var bound []any
	for _, p := range parametersIn(q) {
		bound = append(bound, args[p-1])
	}
	alone := o.run(t, src[q.Span.Start:q.Span.End], bound...)
	if alone.err != "" {
		// SQLite refuses the subquery, as for a value of another type than its column, which s4ql
		// refuses, and SQLite may not.
		return nil, ""
	}
	first := "null"
	if len(alone.rows) > 0 {
		first = alone.rows[0]
	}
	if q.Limited() && (q.Limit.Value == stmt.Integer(0) || q.Limit.Parameter > 0 && args[q.Limit.Parameter-1] == int64(0)) {
		return []string{"null"}, first
	}
	list, orderBy := slices.Clone(result), ""
	for i, c := range q.OrderBy {
		list = append(list, c.Name)
		if i == 0 {
			orderBy += " order by "
		} else {
			orderBy += ", "
		}
		orderBy += stmttest.Quote(c.Name.Value)
		if c.Descending {
			orderBy += " desc"
		}
	}
	text, bound := stmttest.SelectText(&stmt.Select{Table: q.Table, Columns: list, Where: q.Where}, args, orderBy)
	rows := o.run(t, text, bound...)
	if rows.err != "" {
		t.Fatalf("%q: SQLite gives %v for %q", src, rows.err, text)
	}
	if len(rows.rows) == 0 {
		return []string{"null"}, first
	}
	var candidates []string
	top := splitValues(t, rows.rows[0])
	for _, row := range rows.rows {
		values := splitValues(t, row)
		if slices.Equal(values[1:], top[1:]) && !slices.Contains(candidates, values[0]) {
			candidates = append(candidates, values[0])
		}
	}
	return candidates, first
}

// parametersIn the subquery q: the numbers of its parameters, in the order of the text.
func parametersIn(q stmt.Select) []int {
	var ps []int
	for _, c := range q.Where {
		if c.Value.Parameter > 0 {
			ps = append(ps, c.Value.Parameter)
		}
	}
	if q.Limit.Parameter > 0 {
		ps = append(ps, q.Limit.Parameter)
	}
	return ps
}

// splitValues of a row as [oracle.run] writes it: the text of each value.
func splitValues(t testing.TB, row string) []string {
	t.Helper()
	var values []string
	for row != "" {
		var value string
		if row[0] == '"' {
			quoted, err := strconv.QuotedPrefix(row)
			is.NotError(t, err, row)
			value = quoted
		} else {
			value, _, _ = strings.Cut(row, ",")
		}
		values, row = append(values, value), strings.TrimPrefix(row[len(value):], ",")
	}
	return values
}

// undecided reports whether a subquery of the call has more than one candidate.
func (sc *subqueryCall) undecided() bool {
	return sc != nil && slices.ContainsFunc(sc.candidates, func(c []string) bool { return len(c) > 1 })
}

// check the values that an instance of the engine, who, computed for the call: each must be among
// the candidates of its subquery. It counts with count what the values reach, and gives what differs,
// or "".
func (sc *subqueryCall) check(t testing.TB, who string, computed []*s4qlpb.Value, count func(string)) string {
	t.Helper()
	if computed == nil {
		return ""
	}
	if sc == nil || len(computed) != len(sc.subs) {
		return fmt.Sprintf("%v computed %v subqueries of a call whose subqueries SQLite does not run", who, len(computed))
	}
	for j, v := range computed {
		text := valueText(t, v)
		candidates := sc.candidates[j]
		if !slices.Contains(candidates, text) {
			return fmt.Sprintf("%q with %#v: %v gives the subquery %v the value %v, and SQLite one of %v", sc.src, sc.args, who, j+1, text, candidates)
		}
		switch {
		case len(candidates) > 1 && text == sc.first[j]:
			count("subquery with rows that tie for the first, whose value was that of SQLite's first")
		case len(candidates) > 1:
			count("subquery with rows that tie for the first, whose value was that of another")
		case text == "null":
			count("subquery whose value was NULL")
		default:
			count("subquery whose value was that of its one first row")
		}
	}
	return ""
}

// flatten the call with the values of its subqueries: the text of its statement with each subquery
// replaced by a parameter, and the arguments of the parameters of that text, in the order of the
// text, with the value of each subquery in its place.
func (sc *subqueryCall) flatten(values []*s4qlpb.Value) (string, []any) {
	var terms []stmt.Term
	switch s := sc.s.(type) {
	case *stmt.Select:
		for _, c := range s.Where {
			terms = append(terms, c.Value)
		}
		terms = append(terms, s.Limit)
	case *stmt.Update:
		for _, a := range s.Set {
			terms = append(terms, a.Value)
		}
		for _, c := range s.Where {
			terms = append(terms, c.Value)
		}
	case *stmt.Delete:
		for _, c := range s.Where {
			terms = append(terms, c.Value)
		}
	}
	src := sc.src
	var args []any
	for i := len(terms) - 1; i >= 0; i-- {
		switch term := terms[i]; {
		case term.Subquery > 0:
			src = src[:term.Span.Start] + "?" + src[term.Span.End:]
			args = append(args, goValue(values[term.Subquery-1]))
		case term.Parameter > 0:
			args = append(args, sc.args[term.Parameter-1])
		}
	}
	slices.Reverse(args)
	return src, args
}

// goValue of a value of the engine, as SQLite binds it.
func goValue(v *s4qlpb.Value) any {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return v.GetInteger()
	case s4qlpb.Value_Real_case:
		return v.GetReal()
	case s4qlpb.Value_Text_case:
		return v.GetText()
	case s4qlpb.Value_Blob_case:
		return v.GetBlob()
	}
	return nil
}

// subqueryRule of the select, the update, or the delete s of the text src, with scalar subqueries,
// with the arguments args as SQLite binds them, on the database of o: s4ql's "not supported", or "",
// in the order in which the engine checks them. For a statement that SQLite does not prepare, it is
// the refusal of two subqueries of `*` of more than one column of an update or a delete, whose error
// SQLite gives for one that its plan chooses, and otherwise "". For one that SQLite prepares, it is
// the refusal of the first correlated name, as the engine looks the names up, and then, condition by
// condition in the order of the text, the rule of the type of a value as [stmttest.WhereRule] gives
// it, and for a subquery the rule of its conditions on its table, of its argument of LIMIT, as
// [limitRule] gives it, and of the type of its column, which must be the type of the column that it
// is compared with, INT and INTEGER being one; and then for a select the rule of its LIMIT and for an
// update the rule of its SET, as [stmttest.UpdateRule] gives them, but in a read-only transaction,
// where SQLite refuses the write first.
func subqueryRule(t testing.TB, o *oracle, src string, s stmt.Statement, args []any, readOnly bool) string {
	t.Helper()
	if len(args) != s.Parameters() {
		return ""
	}
	table, where := tableWhere(s)
	subs := subqueriesIn(s)
	if err := o.prepare(t, src); err != nil {
		if _, isSelect := s.(*stmt.Select); isSelect || err.Message != "row value misused" && !strings.HasPrefix(err.Message, "sub-select returns ") {
			return ""
		}
		var wide []stmt.Term
		for _, c := range where {
			if q := c.Value.Subquery; q > 0 {
				if _, columns := o.columns(t, subs[q-1].Table.Value); subs[q-1].Columns == nil && len(columns) > 1 {
					wide = append(wide, c.Value)
				}
			}
		}
		if len(wide) < 2 {
			return ""
		}
		return fmt.Sprintf("s4ql: not supported: second subquery of * of more than one column (at offset %v)", wide[1].Span.Start)
	}
	name, columns := o.columns(t, table.Value)
	for _, c := range where {
		q := c.Value.Subquery
		if q == 0 {
			continue
		}
		_, inner := o.columns(t, subs[q-1].Table.Value)
		var names []stmt.Name
		names = append(names, subs[q-1].Columns...)
		for _, c := range subs[q-1].Where {
			names = append(names, c.Column)
		}
		for _, c := range subs[q-1].OrderBy {
			names = append(names, c.Name)
		}
		for _, n := range names {
			if stmttest.ColumnOf(inner, n) < 0 {
				return fmt.Sprintf("s4ql: not supported: correlated subquery (at offset %v)", n.Offset)
			}
		}
	}
	for _, c := range where {
		q := c.Value.Subquery
		if q == 0 {
			if rule := stmttest.WhereRule(name, columns, []stmt.Condition{c}, args); rule != "" {
				return rule
			}
			continue
		}
		sub := subs[q-1]
		innerName, inner := o.columns(t, sub.Table.Value)
		if rule := stmttest.WhereRule(innerName, inner, sub.Where, args); rule != "" {
			return rule
		}
		if rule := limitRule(&sub, args); rule != "" {
			return rule
		}
		position := 0
		if sub.Columns != nil {
			position = stmttest.ColumnOf(inner, sub.Columns[0])
		}
		in, out := inner[position], columns[stmttest.ColumnOf(columns, c.Column)]
		if storageClass(in.Type) != storageClass(out.Type) {
			return fmt.Sprintf("s4ql: not supported: subquery %v, of the column %v.%v of type %v, compared with the column %v.%v of type %v", q, innerName, in.Name, in.Type, name, out.Name, out.Type)
		}
	}
	switch s := s.(type) {
	case *stmt.Select:
		return limitRule(s, args)
	case *stmt.Update:
		if readOnly {
			return ""
		}
		set := &stmt.Update{Table: s.Table, Set: s.Set}
		return stmttest.UpdateRule(name, columns, set, args[:set.Parameters()], func() bool {
			found, bound := foundQuery(src, table, where, args[set.Parameters():])
			return len(o.run(t, found, bound...).rows) > 0
		})
	}
	return ""
}

// foundQuery of the conditions where of a statement of the text src on the table, with the arguments
// args of their parameters: a select that gives a row if the conditions hold for a row of the table,
// with each value as the text writes it, a subquery among them, and the arguments of its parameters.
func foundQuery(src string, table stmt.Name, where []stmt.Condition, args []any) (string, []any) {
	var conditions []string
	for _, c := range where {
		value := src[c.Value.Span.Start:c.Value.Span.End]
		switch {
		case c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull:
			conditions = append(conditions, stmttest.Quote(c.Column.Value)+" "+c.Operator.String())
		case c.ValueFirst:
			conditions = append(conditions, value+" "+c.Operator.Swapped().String()+" "+stmttest.Quote(c.Column.Value))
		default:
			conditions = append(conditions, stmttest.Quote(c.Column.Value)+" "+c.Operator.String()+" "+value)
		}
	}
	return "select 1 from " + stmttest.Quote(table.Value) + " where " + strings.Join(conditions, " and ") + " limit 1", args
}

// storageClass of the declared type of a column of a STRICT table: the kind of its values, which is
// one for INT and INTEGER.
func storageClass(typ string) string {
	if typ == "INT" {
		return "INTEGER"
	}
	return typ
}
