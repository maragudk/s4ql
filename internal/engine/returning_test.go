package engine_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
	"maragu.dev/s4ql/internal/store/simstore"
)

// returningSetUp is the table of the tests of RETURNING: a column of each type, NULL in all but the
// key and s, a default on s and on d, and an index on s.
var returningSetUp = []string{
	"create table t (k int primary key, i integer, r real, s text not null default 'x', b blob, d int default 7) strict, without rowid",
	"create index ts on t (s)",
}

// returningCall is a statement of [returningCalls], with the arguments of its parameters, which runs
// through Exec if exec, and through Query otherwise.
type returningCall struct {
	src  string
	args []any
	exec bool
}

// call of the statement for [pair.step].
func (c returningCall) call() stmttest.Call {
	return stmttest.Call{Src: c.src, Args: c.args, Query: !c.exec, Supported: true}
}

// returningCalls are statements with RETURNING of each kind, in their order, which the tests run
// against SQLite.
var returningCalls = []returningCall{
	// An insert of every column, of the columns that the list names with the defaults of the others,
	// of NULL, of -0.0, by arguments, and of each type.
	{src: "insert into t (k, i, r, s, b, d) values (1, 10, 1.5, 'a', x'01', 3) returning *"},
	{src: "insert into t (k) values (2) returning k, d, s, k"},
	{src: "insert into t (k, i, r, b) values (3, null, null, null) returning *"},
	{src: "insert into t (k, r) values (4, -0.0) returning r, K"},
	{src: "insert into t (k, i, r, s, b) values (?, ?, ?, ?, ?) returning *", args: []any{int64(5), int64(-9223372036854775808), math.Copysign(0, -1), "é", []byte{}}},
	{src: "insert into t values (?, ?, ?, ?, ?, ?) returning b, i", args: []any{int64(6), nil, 2.5, "b", []byte{0, 1}, nil}},
	{src: `insert into t (k, s) values (7, 'c') returning "d", S`},
	{src: "insert into t (k) values (8) returning d", exec: true},
	// The failures of an insert: a key that exists, NULL in the key, NULL in a column that takes none,
	// a value of another type, and a column of RETURNING that the table does not have, after the
	// errors of the rest of the statement.
	{src: "insert into t (k) values (1) returning *"},
	{src: "insert into t (k) values (null) returning k"},
	{src: "insert into t (k, s) values (9, null) returning k"},
	{src: "insert into t (k, s) values (9, 1) returning k"},
	{src: "insert into t (k) values (9) returning nope"},
	{src: `insert into t (k) values (9) returning k, "nope"`},
	{src: "insert into t (nope) values (9) returning nope2"},
	{src: "insert into t (k, i) values (9) returning nope"},
	{src: "insert into nope (k) values (9) returning nope"},
	// An update of one row, of the key, of an indexed column, with the values that the row has, of
	// every row, of the rows of the index, and of no row.
	{src: "update t set s = 'y' where k = 1 returning *"},
	{src: "update t set s = 'y' where k = 1 returning s, k"},
	{src: "update t set k = 100 where k = 1 returning k, s"},
	{src: "update t set k = ? where k = ? returning *", args: []any{int64(1), int64(100)}},
	{src: "update t set s = 'z' where s = 'x' returning k, s"},
	{src: "update t set r = 0.5 returning k, r"},
	{src: "update t set r = -0.0, i = null where k >= 2 returning r, i, r"},
	{src: "update t set b = ? where s > ? returning *", args: []any{[]byte{9}, "b"}},
	{src: "update t set d = 1 where k > 1000 returning *"},
	{src: "update t set d = 1 where k is null returning k"},
	{src: "update t set d = 2 where d = 7 returning k", exec: true},
	// The failures of an update: a key that another row has, NULL in a column that takes none, a value
	// of another type, and a column of RETURNING that the table does not have, after those of SET and
	// of WHERE.
	{src: "update t set k = 2 where k = 1 returning k"},
	{src: "update t set k = 9 where k > 1 returning k"},
	{src: "update t set s = null where k = 1 returning k"},
	{src: "update t set s = 1 where k = 1 returning k"},
	{src: "update t set s = 'q' where k = 1 returning nope"},
	{src: "update t set nope = 'q' where k = 1 returning nope2"},
	{src: "update t set s = 'q' where nope = 1 returning nope2"},
	{src: "update t set s = 'q' where nope is null returning nope2"},
	// A delete of one row, of the rows of the index, of no row, and of every row.
	{src: "delete from t where k = ? returning *", args: []any{int64(6)}},
	{src: "delete from t where s = 'z' returning k, s, k"},
	{src: "delete from t where k = 1000 returning *"},
	{src: "delete from t where k = 3 returning k", exec: true},
	{src: "delete from t where nope = 1 returning k"},
	{src: "delete from t where k = 1 returning nope"},
	{src: "delete from t returning d, k"},
	{src: "delete from t returning *"},
}

func TestEngine_returning(t *testing.T) {
	t.Run("should give the rows of RETURNING that SQLite gives, with its columns and its errors, in the order of the key or in its reverse, on the core and in memory", func(t *testing.T) {
		for _, i := range []*instance{newInstance(t, engine.Options{}), newMemoryInstance(t)} {
			y := tally{}
			p := newPair(t, i, y)
			for _, src := range returningSetUp {
				is.Equal(t, "", p.step(stmttest.Call{Src: src, Supported: true}), src)
			}
			// The calls run again until the rows of an update and those of a delete have each come in both
			// directions, each a draw of the engine from the sequence of its seed, so that the number of
			// rounds is a function of the seed.
			var directions []string
			for _, kind := range []string{"update", "delete"} {
				for _, order := range []string{"order", "reverse of the order"} {
					directions = append(directions, "reached: "+kind+" with RETURNING whose rows came in the "+order+" of the key")
				}
			}
			for round := 0; round < 2 || slices.ContainsFunc(directions, func(d string) bool { return y[d] == 0 }); round++ {
				if round == 20 {
					for _, d := range directions {
						t.Logf("%v times: %v", y[d], d)
					}
					t.Fatal("the rows of RETURNING of an update or of a delete came in one direction only in 20 rounds")
				}
				for _, c := range returningCalls {
					if failure := p.step(c.call()); failure != "" {
						t.Fatal(failure)
					}
				}
				is.Equal(t, p.o.state(t), i.state())
			}
		}
	})

	t.Run("should give the rows in a transaction, keep them with its commit, and take them back with its roll back", func(t *testing.T) {
		for _, commit := range []bool{true, false} {
			i := newInstance(t, engine.Options{})
			p := newPair(t, i, nil)
			for _, src := range returningSetUp {
				is.Equal(t, "", p.step(stmttest.Call{Src: src, Supported: true}), src)
			}
			for _, c := range []*engine.Conn{i.c, p.other.c} {
				is.NotError(t, c.Begin(t.Context()))
			}
			for _, src := range oracleBegin(false) {
				is.Equal(t, "", p.o.run(t, src).err, src)
			}
			p.inTx = true
			for _, c := range returningCalls {
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
			// After a restart with an empty cache, the log gives the same rows.
			i.restart(true, engine.Options{})
			is.Equal(t, p.o.state(t), i.state())
		}
	})

	t.Run("should refuse a statement with RETURNING in a read-only transaction after the errors of its names, as SQLite does", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		p := newPair(t, i, nil)
		for _, src := range returningSetUp {
			is.Equal(t, "", p.step(stmttest.Call{Src: src, Supported: true}), src)
		}
		is.Equal(t, "", p.step(stmttest.Call{Src: "insert into t (k) values (1)", Supported: true}))
		for _, c := range []*engine.Conn{i.c, p.other.c} {
			is.NotError(t, c.BeginReadOnly(t.Context()))
		}
		for _, src := range oracleBegin(true) {
			is.Equal(t, "", p.o.run(t, src).err, src)
		}
		p.inTx, p.readOnly = true, true
		for _, src := range []string{"insert into t (k) values (2) returning k", "update t set s = 'y' returning *", "delete from t returning k",
			"delete from t returning nope", "update t set s = 'q' where k = 1 returning nope"} {
			if failure := p.step(stmttest.Call{Src: src, Query: true, Supported: true}); failure != "" {
				t.Fatal(failure)
			}
		}
	})

	t.Run("should keep the rows of RETURNING after a restart with an empty cache, and with its cache", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		p := newPair(t, i, nil)
		for _, src := range returningSetUp {
			is.Equal(t, "", p.step(stmttest.Call{Src: src, Supported: true}), src)
		}
		for n, c := range returningCalls {
			if failure := p.step(c.call()); failure != "" {
				t.Fatal(failure)
			}
			if n%7 == 6 {
				i.restart(n%2 == 0, engine.Options{})
				is.Equal(t, p.o.state(t), i.state())
			}
		}
	})
}

func TestConn_returning(t *testing.T) {
	t.Run("should give the columns of RETURNING as the table stores them, also where the statement changes no row", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec("create table T (K int primary key, V text, W real) strict, without rowid").err)
		for _, c := range []struct{ src, columns string }{
			{"insert into t (k, v) values (1, 'a') returning k, v, \"w\", v", "K INT, V TEXT, W REAL, V TEXT"},
			{"insert into t (k, v) values (2, 'a') returning *", "K INT, V TEXT, W REAL"},
			{"update t set v = 'b' where k = 3 returning w", "W REAL"},
			{"delete from t where k = 3 returning *", "K INT, V TEXT, W REAL"},
		} {
			got := i.queryValues(c.src, nil)
			is.Equal(t, "", got.err, c.src)
			is.Equal(t, c.columns, got.columns, c.src)
		}
	})

	t.Run("should count the changes of a statement with RETURNING as without it, through Exec and Query", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec("create table t (k int primary key, v text) strict, without rowid").err)
		for k := range 4 {
			res, err := i.c.Exec(t.Context(), "insert into t (k, v) values (?, 'a') returning k", s4qlpb.Value_builder{Integer: new(int64(k))}.Build())
			is.NotError(t, err)
			is.Equal(t, engine.Result{Changes: 1, SetsChanges: true}, res)
		}
		res, err := i.c.Exec(t.Context(), "update t set v = 'a' where k > 0 returning *")
		is.NotError(t, err)
		is.Equal(t, engine.Result{Changes: 3, SetsChanges: true}, res)
		rows, err := i.c.Query(t.Context(), "delete from t where k < 2 returning v")
		is.NotError(t, err)
		is.Equal(t, engine.Result{Changes: 2, SetsChanges: true}, rows.Result())
		n := 0
		for _, err := range rows.All() {
			is.NotError(t, err)
			n++
		}
		is.Equal(t, 2, n)
	})

	t.Run("should log one event for a statement with RETURNING when it has ended, as the write it is, with the rows that it returned and the direction that it drew", func(t *testing.T) {
		h := &events{}
		i := newInstanceLogged(t, engine.Options{Logger: slog.New(h)}, nil)
		is.Equal(t, "", i.exec("create table t (k int primary key, v text) strict, without rowid").err)
		for k := range 3 {
			is.Equal(t, "", i.exec("insert into t (k, v) values (?, 'a')", int64(k)).err)
		}
		for _, c := range []struct {
			src, kind, access, rows, read string
			direction                     bool
		}{
			{"insert into t (k, v) values (3, 'b') returning *", "insert", "", "1", "", false},
			{"update t set v = 'c' where k > 0 returning k", "update", "primary_key", "3", "6", true},
			{"delete from t returning v", "delete", "full_scan", "4", "8", true},
			{"delete from t where k = 9 returning v", "delete", "primary_key", "0", "0", true},
		} {
			n := len(h.records)
			rows, err := i.c.Query(t.Context(), c.src)
			is.NotError(t, err, c.src)
			// The event is logged before the rows are read, and not again when they end.
			is.Equal(t, n+1, len(h.records), c.src)
			e, _ := h.last(t)
			is.Equal(t, c.kind, e[engine.AttrKind], c.src)
			is.Equal(t, "ok", e[engine.AttrOutcome], c.src)
			is.Equal(t, c.rows, e[engine.AttrRows], c.src)
			is.Equal(t, c.access, e[engine.AttrAccess], c.src)
			is.Equal(t, c.read, e[engine.AttrRowsRead], c.src)
			is.True(t, e[engine.AttrParent] != "", c.src)
			_, has := e[engine.AttrDirection]
			is.Equal(t, c.direction, has, c.src)
			for _, err := range rows.All() {
				is.NotError(t, err)
			}
			is.NotError(t, rows.Close())
			is.Equal(t, n+1, len(h.records), c.src)
		}
	})

	t.Run("should vary the order of the rows of RETURNING of an update and of a delete from one statement to the next, by the seed of the engine", func(t *testing.T) {
		orders := map[string]map[string]int{"update": {}, "delete": {}}
		for seed := range uint64(40) {
			i := newInstance(t, engine.Options{Directions: &seed})
			is.Equal(t, "", i.exec("create table t (k int primary key, v text) strict, without rowid").err)
			for k := range 3 {
				is.Equal(t, "", i.exec("insert into t (k, v) values (?, 'a')", int64(k)).err)
			}
			// The update first, so that the delete finds the three rows that it changed.
			for _, c := range []struct{ kind, src string }{{"update", "update t set v = 'b' returning k"}, {"delete", "delete from t where v = 'b' returning k"}} {
				got := i.queryValues(c.src, nil)
				is.Equal(t, "", got.err, c.src)
				orders[c.kind][strings.Join(got.rows, " ")]++
			}
		}
		for kind, o := range orders {
			is.Equal(t, 2, len(o), kind)
			is.True(t, o["0 1 2"] >= 5, kind)
			is.True(t, o["2 1 0"] >= 5, kind)
		}
	})

	t.Run("should give the error of a commit that fails and no rows, and leave nothing of the change", func(t *testing.T) {
		// The commit object of each statement holds a text of 5,000 bytes, as a value or in the text of
		// the statement, which the store refuses as too large.
		i := newInstanceOn(t, simstore.Options{MaxSize: 4 << 10})
		is.Equal(t, "", i.exec("create table t (k int primary key, v text) strict, without rowid").err)
		is.Equal(t, "", i.exec("insert into t (k, v) values (1, 'a')").err)
		long := strings.Repeat("x", 5000)
		for _, c := range []struct {
			src  string
			args []*s4qlpb.Value
		}{
			{"insert into t (k, v) values (2, ?) returning *", []*s4qlpb.Value{s4qlpb.Value_builder{Text: new(long)}.Build()}},
			{"update t set v = ? returning *", []*s4qlpb.Value{s4qlpb.Value_builder{Text: new(long)}.Build()}},
			{"delete from t where v != '" + long + "' returning *", nil},
		} {
			rows, err := i.c.Query(t.Context(), c.src, c.args...)
			is.Error(t, engine.ErrTooLarge, err)
			is.Error(t, core.ErrTooLarge, err)
			is.True(t, rows == nil || len(rows.Columns()) == 0, c.src)
			res, err := i.c.Exec(t.Context(), c.src, c.args...)
			is.Error(t, engine.ErrTooLarge, err)
			is.Equal(t, engine.Result{SetsChanges: true}, res)
			got := i.queryValues("select * from t", nil)
			is.Equal(t, "", got.err)
			is.Equal(t, `1,"a"`, strings.Join(got.rows, " "), c.src)
		}
		is.Equal(t, 2, len(logAnnotations(t, i.store)))
	})

	t.Run("should give the error of a context that ends before the statement and no rows, and change nothing, and give every row where it ends after the change", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec("create table t (k int primary key, v text) strict, without rowid").err)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := i.c.Query(ctx, "insert into t (k, v) values (1, 'a') returning k")
		is.True(t, errors.Is(err, context.Canceled))
		got := i.queryValues("select * from t", nil)
		is.Equal(t, "", got.err)
		is.Equal(t, 0, len(got.rows))

		// A context that ends after the commit does not end the rows, which hold nothing, and the
		// change stays.
		ctx, cancel = context.WithCancel(t.Context())
		rows, err := i.c.Query(ctx, "insert into t (k, v) values (1, 'a') returning k")
		is.NotError(t, err)
		cancel()
		var keys []int64
		for row, err := range rows.All() {
			is.NotError(t, err)
			keys = append(keys, row[0].GetInteger())
		}
		is.EqualSlice(t, []int64{1}, keys)
		is.NotError(t, rows.Close())
		is.Equal(t, `1,"a"`, strings.Join(i.queryValues("select * from t", nil).rows, " "))

		// Nor in a transaction, where the change is made too.
		is.NotError(t, i.c.Begin(t.Context()))
		ctx, cancel = context.WithCancel(t.Context())
		rows, err = i.c.Query(ctx, "update t set v = 'b' returning v")
		is.NotError(t, err)
		cancel()
		n := 0
		for _, err := range rows.All() {
			is.NotError(t, err)
			n++
		}
		is.Equal(t, 1, n)
		is.NotError(t, rows.Close())
		is.NotError(t, i.c.Commit(t.Context()))
		is.Equal(t, `1,"b"`, strings.Join(i.queryValues("select * from t", nil).rows, " "))
	})

	t.Run("should drop the rows of RETURNING in Exec, with the errors and the count of Query, and read no deleted row again", func(t *testing.T) {
		h := &events{}
		i := newInstanceLogged(t, engine.Options{Logger: slog.New(h)}, nil)
		is.Equal(t, "", i.exec("create table t (k int primary key, v text) strict, without rowid").err)
		for k := range 3 {
			is.Equal(t, "", i.exec("insert into t (k, v) values (?, 'a')", int64(k)).err)
		}
		for _, src := range []string{
			"delete from t where k = 9 returning nope",
			"update t set v = 1 returning nope",
			"insert into t (k, v) values (1, 'b') returning nope",
			"insert into t (k, v) values (1, 'b') returning k",
		} {
			_, queryErr := i.c.Query(t.Context(), src)
			is.True(t, queryErr != nil, src)
			_, execErr := i.c.Exec(t.Context(), src)
			is.Equal(t, queryErr.Error(), fmt.Sprint(execErr), src)
		}
		res, err := i.c.Exec(t.Context(), "delete from t where k > 0 returning v")
		is.NotError(t, err)
		is.Equal(t, engine.Result{Changes: 2, SetsChanges: true}, res)
		e, _ := h.last(t)
		is.Equal(t, "2", e[engine.AttrRows])
		// The access by the key reads each row that it finds once, and the delete reads none again to
		// give its values, as Query does.
		is.Equal(t, "2", e[engine.AttrRowsRead])
		// No row shows the direction that the engine drew.
		_, has := e[engine.AttrDirection]
		is.True(t, !has)
		is.Equal(t, `0,"a"`, strings.Join(i.queryValues("select * from t", nil).rows, " "))
	})

	t.Run("should refuse a statement in a transaction while the rows of a RETURNING of it are open, and run one outside a transaction", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec("create table t (k int primary key, v text) strict, without rowid").err)
		rows, err := i.c.Query(t.Context(), "insert into t (k, v) values (1, 'a') returning k")
		is.NotError(t, err)
		// Outside a transaction the rows hold nothing.
		is.Equal(t, "", i.exec("insert into t (k, v) values (2, 'b')").err)
		is.Equal(t, 2, len(i.queryValues("select * from t", nil).rows))
		is.NotError(t, i.c.Begin(t.Context()))
		is.NotError(t, rows.Close())
		rows, err = i.c.Query(t.Context(), "insert into t (k, v) values (3, 'c') returning k")
		is.NotError(t, err)
		_, err = i.c.Exec(t.Context(), "insert into t (k, v) values (4, 'd')")
		is.True(t, errors.Is(err, engine.ErrRowsOpen))
		is.NotError(t, rows.Close())
		is.Equal(t, "", i.exec("insert into t (k, v) values (4, 'd')").err)
		is.NotError(t, i.c.Rollback(t.Context()))
		is.Equal(t, 2, len(i.queryValues("select * from t", nil).rows))
	})

	t.Run("should hold nothing with the rows of a RETURNING outside a transaction, which a begin and a statement beside them do not wait for, and hold the connection with them in one", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec("create table t (k int primary key, v text) strict, without rowid").err)

		rows, err := i.c.Query(t.Context(), "insert into t (k, v) values (1, 'a') returning k")
		is.NotError(t, err)
		is.True(t, !rows.Holds(), "rows of a RETURNING outside a transaction")
		// Nothing waits for them, also a begin while they are open.
		is.NotError(t, i.c.Begin(t.Context()))
		tx, err := i.c.Query(t.Context(), "insert into t (k, v) values (2, 'b') returning k")
		is.NotError(t, err)
		is.True(t, tx.Holds(), "rows of a RETURNING in a transaction")
		is.NotError(t, tx.Close())
		is.True(t, !tx.Holds(), "rows of a RETURNING in a transaction once closed")
		is.NotError(t, i.c.Commit(t.Context()))
		var got [][]*s4qlpb.Value
		for row, err := range rows.All() {
			is.NotError(t, err)
			got = append(got, row)
		}
		is.Equal(t, 1, len(got))
		is.Equal(t, int64(1), got[0][0].GetInteger())

		none, err := i.c.Query(t.Context(), "insert into t (k, v) values (3, 'c')")
		is.NotError(t, err)
		is.True(t, !none.Holds(), "rows of a statement without RETURNING")
		query, err := i.c.Query(t.Context(), "select k from t")
		is.NotError(t, err)
		is.True(t, query.Holds(), "rows of a query")
		is.NotError(t, query.Close())
	})

	t.Run("should run goqite's send and receive as far as the slices take them, as SQLite does", func(t *testing.T) {
		p := newPair(t, newInstance(t, engine.Options{}), nil)
		for _, src := range []string{goqiteTable, goqiteIndex} {
			is.Equal(t, "", p.step(stmttest.Call{Src: src, Supported: true}), src)
		}
		// goqite's send, with the id from the caller, since the default of the id is an expression.
		send := "insert into goqite (id, created, updated, queue, body, timeout, received, priority) values (?, ?, ?, ?, ?, ?, 0, ?) returning id"
		now := "2026-11-23T12:00:00.000Z"
		// Each message is created a minute after the one before, so that no two tie in the order of the
		// receive.
		for n, id := range []string{"m_1", "m_2", "m_3"} {
			created := fmt.Sprintf("2026-11-23T11:5%v:00.000Z", n)
			is.Equal(t, "", p.step(stmttest.Call{Src: send, Args: []any{id, created, created, "default", []byte("body " + id), now, int64(n % 2)}, Query: true, Supported: true}))
			is.Equal(t, `"`+id+`"`, strings.Join(p.last.rows, " "))
		}
		// goqite's receive, with the id that its select of the next message gives, since a scalar
		// subquery and arithmetic are not supported.
		next := "select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1"
		is.Equal(t, "", p.step(stmttest.Call{Src: next, Args: []any{"default", now, int64(3)}, Query: true, Supported: true}))
		is.Equal(t, `"m_2"`, strings.Join(p.last.rows, " "))
		receive := "update goqite set timeout = ?, received = ? where id = ? returning id, body"
		is.Equal(t, "", p.step(stmttest.Call{Src: receive, Args: []any{"2026-11-23T12:01:00.000Z", int64(1), "m_2"}, Query: true, Supported: true}))
		is.Equal(t, `"m_2",x'626f6479206d5f32'`, strings.Join(p.last.rows, " "))
		is.Equal(t, "", p.step(stmttest.Call{Src: next, Args: []any{"default", now, int64(3)}, Query: true, Supported: true}))
		is.Equal(t, `"m_1"`, strings.Join(p.last.rows, " "))
		is.Equal(t, "", p.step(stmttest.Call{Src: "delete from goqite where queue = ? and id = ?", Args: []any{"default", "m_2"}, Supported: true}))
		is.Equal(t, p.o.state(t), p.i.state())
	})
}

func TestCheck_returning(t *testing.T) {
	t.Run("should check an insert, an update, or a delete with RETURNING as without it, and give the pass's error for a column of RETURNING that it does not support", func(t *testing.T) {
		for _, c := range []struct {
			src string
			n   int
		}{
			{"insert into t1 (v1, v2) values (?, ?) returning v1", 2},
			{"update t1 set v2 = ? where v1 = ? returning v1, v2", 2},
			{"delete from t1 where v1 = ? returning *", 1},
			{" ; DELETE FROM t /* returning */ RETURNING \"returning\";", 0},
			{"insert into goqite (queue, body, timeout, priority) values (?, ?, ?, ?) returning id", 4},
		} {
			is.NotError(t, engine.Check(c.src, c.n), c.src)
			is.Error(t, engine.ErrArguments, engine.Check(c.src, c.n+1), c.src)
		}
		err := engine.Check("delete from t returning v + 1", 0)
		is.Error(t, stmt.ErrNotSupported, err)
		is.Equal(t, "s4ql: not supported: result column other than a column or * (at offset 24)", err.Error())
	})
}

func TestPair_returning(t *testing.T) {
	t.Run("should hold a statement with RETURNING to the rules of its values and of its arguments, through Exec and Query", func(t *testing.T) {
		for _, src := range []string{"insert into t1 (v1) values ('x') returning v1", "update t1 set v1 = 'x' returning v1", "insert into t1 (v1) values (2) returning v1",
			"delete from t1 where v1 = 'x' returning *", "delete from t1 where v1 = ? returning *"} {
			for _, exec := range []bool{true, false} {
				y := tally{}
				p := newPair(t, newMemoryInstance(t), y)
				for _, s := range fuzzSetUp {
					is.Equal(t, "", p.step(stmttest.Call{Src: s, Supported: true}), s)
				}
				is.Equal(t, "", p.step(stmttest.Call{Src: src, Query: !exec, Supported: true}), src)
			}
		}
	})

	t.Run("should excuse a statement whose RETURNING names a column twice, which SQLite refuses under the lowered length limit, by SQLite's answer without the limit", func(t *testing.T) {
		for _, src := range []string{"delete from t where k = 1 returning b, b", "update t set k = 2 where k = 1 returning b, k, b", "insert into t (k, b) values (3, ?) returning b, b"} {
			y := tally{}
			p := newPair(t, newInstanceLimited(t, engine.Options{}, nil, stmttest.LengthLimit), y)
			// A blob of 85 hundredths of the limit, which fits a row, and twice in a row of RETURNING does not.
			long := []byte(strings.Repeat("v", 85*stmttest.LengthLimit/100))
			for _, c := range []stmttest.Call{{Src: "create table t (k int primary key, b blob) strict, without rowid", Supported: true},
				{Src: "insert into t (k, b) values (1, ?)", Args: []any{long}, Supported: true}} {
				is.Equal(t, "", p.step(c), c.Src)
			}
			var args []any
			if strings.Contains(src, "?") {
				args = []any{long}
			}
			is.Equal(t, "", p.step(stmttest.Call{Src: src, Args: args, Query: true, Supported: true}), src)
			is.Equal(t, 1, y["excused: "+excusedReturningTooBig], src)
			is.Equal(t, p.o.state(t), p.i.state(), src)
		}
	})
}

func TestNewPlan_returning(t *testing.T) {
	t.Run("should plan RETURNING on an insert, an update, and a delete, and none on a statement without it", func(t *testing.T) {
		for _, c := range []struct{ src, want string }{
			{"insert into t (k, v) values (1, ?) returning k, V, k", "insert into t (k, v) returning k, V, k\n  values (1, ?1)\n"},
			{"insert into t values (1) returning *", "insert into t returning *\n  values (1)\n"},
			{"update t set v = 1 where k = 2 returning *", "update t set v = 1 returning *\n  filter k = 2\n    scan t\n"},
			{`delete from t returning "v"`, "delete from t returning \"v\"\n  scan t\n"},
			{"delete from t", "delete from t\n  scan t\n"},
		} {
			s, err := stmt.Check(c.src)
			is.NotError(t, err, c.src)
			is.Equal(t, c.want, engine.Format(engine.NewPlan(s)), c.src)
		}
	})
}
