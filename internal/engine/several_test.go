package engine_test

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// execAll src with args through [engine.Conn.ExecAll] on the connection of i, and keep SQLite's count
// of changes as its result says, as [instance.exec] does.
func (i *instance) execAll(src string, args ...int64) (engine.Result, error) {
	i.t.Helper()
	res, err := i.c.ExecAll(i.t.Context(), src, integers(args)...)
	if res.SetsChanges {
		i.changes = res.Changes
	}
	return res, err
}

func TestConn_ExecAll(t *testing.T) {
	t.Run("should run the statements one after another, each as a transaction of its own, with the arguments in order", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		res, err := i.execAll(t1+"; insert into t1 (v1) values (?); update t1 set v1 = ? where v1 = ?;\n-- c\ninsert into t1 (v1) values (?); select * from t1 where v1 = ?; ;", 1, 2, 1, 3, 3)
		is.NotError(t, err)
		is.Equal(t, engine.Result{Changes: 1, SetsChanges: true}, res)
		is.Equal(t, "t1 (v1 INT): 2 3\n", i.state())
		is.Equal(t, fmt.Sprint([][]string{
			{t1 + ";"},
			{"insert into t1 (v1) values (?); with 1"},
			{"update t1 set v1 = ? where v1 = ?; with 2 with 1"},
			{"insert into t1 (v1) values (?); with 3"},
		}), fmt.Sprint(logAnnotations(t, i.store)))
	})

	t.Run("should give the count of changes of the last statement that sets it, and none if none does", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		res, err := i.execAll(t1 + "; insert into t1 (v1) values (1); insert into t1 (v1) values (2); update t1 set v1 = v1; " +
			"create table t2 (v1 int primary key) strict, without rowid; select * from t1")
		is.NotError(t, err)
		is.Equal(t, engine.Result{Changes: 2, SetsChanges: true}, res)

		res, err = i.execAll("drop table t2; select * from t1")
		is.NotError(t, err)
		is.Equal(t, engine.Result{}, res)

		res, err = i.execAll("delete from t1 returning *; create table t2 (v1 int primary key) strict, without rowid")
		is.NotError(t, err)
		is.Equal(t, engine.Result{Changes: 2, SetsChanges: true}, res)
	})

	t.Run("should keep the statements before one that fails and run none after it, with the result of the failed insert", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		res, err := i.execAll(t1 + "; insert into t1 (v1) values (1); insert into t1 (v1) values (1); insert into t1 (v1) values (3)")
		is.Equal(t, "UNIQUE constraint failed: t1.v1", err.Error())
		is.Equal(t, engine.Result{Changes: 0, SetsChanges: true}, res)
		is.Equal(t, "t1 (v1 INT): 1\n", i.state())

		res, err = i.execAll("insert into t1 (v1) values (2); insert into t9 (v1) values (1); insert into t1 (v1) values (3)")
		is.Equal(t, "no such table: t9", err.Error())
		is.Equal(t, engine.Result{Changes: 1, SetsChanges: true}, res)
		is.Equal(t, "t1 (v1 INT): 1 2\n", i.state())
	})

	t.Run("should run the statements before the first that does not check, and then give its error, with the offset in the whole text", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		_, err := i.execAll(t1 + "; insert into t1 (v1) values (1); selec * from t1; insert into t1 (v1) values (2)")
		var se *sqlerr.Error
		is.True(t, errors.As(err, &se))
		is.Equal(t, sqlerr.Error{Code: 1, ExtendedCode: 1, Message: `near "selec": syntax error`, Offset: 92}, *se)
		is.Equal(t, "t1 (v1 INT): 1\n", i.state())

		res, err := i.execAll("insert into t1 (v1) values (2); select * from t1 group by v1; insert into t1 (v1) values (3)")
		is.Equal(t, "s4ql: not supported: GROUP BY clause (at offset 49)", err.Error())
		is.Equal(t, engine.Result{Changes: 1, SetsChanges: true}, res)
		is.Equal(t, "t1 (v1 INT): 1 2\n", i.state())
	})

	t.Run("should refuse more arguments than the statements have parameters before any statement runs, and run the statements before one that has too few", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		_, err := i.execAll(t1+"; insert into t1 (v1) values (?); insert into t1 (v1) values (?)", 1, 2, 3)
		is.True(t, errors.Is(err, engine.ErrArguments))
		is.Equal(t, "s4ql: wrong number of arguments: 3 for a text whose 3 statements have 2 parameters", err.Error())
		is.Equal(t, "", i.state())

		_, err = i.execAll(t1+"; insert into t1 (v1) values (?); update t1 set v1 = ? where v1 = ?", 1, 2)
		is.True(t, errors.Is(err, engine.ErrArguments))
		is.Equal(t, "s4ql: wrong number of arguments: 2 for a text whose statements up to the one at offset 92 have 3 parameters", err.Error())
		is.Equal(t, "t1 (v1 INT): 1\n", i.state())

		// A text of one statement is judged as Exec judges it.
		for _, args := range [][]int64{nil, {1, 2}} {
			_, err = i.execAll("insert into t1 (v1) values (?);", args...)
			_, want := i.c.Exec(t.Context(), "insert into t1 (v1) values (?);", integers(args)...)
			is.Equal(t, want.Error(), err.Error())
		}
		is.Equal(t, "t1 (v1 INT): 1\n", i.state())
	})

	t.Run("should refuse an argument that no column holds for its statement, by its number in the call, after the statements before it", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		nan := s4qlpb.Value_builder{Real: new(math.NaN())}.Build()
		_, err := i.c.ExecAll(t.Context(), t1+"; insert into t1 (v1) values (?); insert into t1 (v1) values (?)", integers([]int64{1})[0], nan)
		is.True(t, errors.Is(err, stmt.ErrNotSupported))
		is.Equal(t, "s4ql: not supported: argument 2, which is a real that is not a number", err.Error())
		is.Equal(t, "t1 (v1 INT): 1\n", i.state())
	})

	t.Run("should refuse what Split refuses of the whole text, and a text without a statement, before any statement runs", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		for src, want := range map[string]string{
			t1 + "; " + t1:           "s4ql: not supported: space after a statement that SQLite reads as part of a token (at offset 59)",
			t1 + "; drop table t1\v": "s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 73)",
			t1 + "; \x00":            "s4ql: not supported: NUL byte (at offset 60)",
		} {
			_, err := i.execAll(src)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), src)
			is.Equal(t, want, err.Error(), src)
		}
		_, err := i.execAll(" ; -- c")
		is.Error(t, engine.ErrNoStatement, err)
		is.Equal(t, "", i.state())
	})

	t.Run("should run the statements in the transaction of the connection, which keeps the statements before one that fails and stays open", func(t *testing.T) {
		h := &events{}
		i := newInstance(t, engine.Options{Logger: slog.New(h)})
		_, err := i.execAll(t1)
		is.NotError(t, err)
		is.NotError(t, i.c.Begin(t.Context()))
		_, err = i.execAll("insert into t1 (v1) values (1); insert into t1 (v1) values (1); insert into t1 (v1) values (2)")
		is.Equal(t, "UNIQUE constraint failed: t1.v1", err.Error())
		_, err = i.execAll("insert into t1 (v1) values (3); insert into t1 (v1) values (4)")
		is.NotError(t, err)
		is.NotError(t, i.c.Commit(t.Context()))
		is.Equal(t, "t1 (v1 INT): 1 3 4\n", i.state())
		e, _ := h.last(t)
		is.Equal(t, "transaction", e["msg"])
		is.Equal(t, "4", e["transaction.statements"])
		is.Equal(t, fmt.Sprint([][]string{
			{t1},
			{"insert into t1 (v1) values (1);", "insert into t1 (v1) values (3);", "insert into t1 (v1) values (4)"},
		}), fmt.Sprint(logAnnotations(t, i.store)))
	})

	t.Run("should log an event for each statement, with the text of its part, and one for the whole text that fails before any statement runs", func(t *testing.T) {
		h := &events{}
		i := newInstance(t, engine.Options{Logger: slog.New(h)})
		src := t1 + "; insert into t1 (v1) values (1);\n insert into t1 (v1) values (1); -- c"
		_, err := i.execAll(src)
		is.Error(t, err, err)
		is.Equal(t, 3, len(h.records))
		var got []string
		for _, e := range h.records {
			got = append(got, e[engine.AttrKind]+" "+e[engine.AttrOutcome]+" "+e[engine.AttrSQL])
		}
		is.EqualSlice(t, []string{
			"create_table ok " + t1 + ";",
			"insert ok  insert into t1 (v1) values (1);",
			"insert sqlite_error \n insert into t1 (v1) values (1); -- c",
		}, got)

		_, err = i.execAll("drop table t1; insert into t1 (v1) values (?)", 1, 2)
		is.True(t, errors.Is(err, engine.ErrArguments))
		e, _ := h.last(t)
		is.Equal(t, 4, len(h.records))
		is.Equal(t, "", e[engine.AttrKind])
		is.Equal(t, "wrong_arguments", e[engine.AttrOutcome])
		is.Equal(t, "drop table t1; insert into t1 (v1) values (?)", e[engine.AttrSQL])

		_, err = i.execAll("insert into t1 (v1) values (2); selec")
		is.Error(t, err, err)
		is.Equal(t, 6, len(h.records))
		e, _ = h.last(t)
		is.Equal(t, "sqlite_error", e[engine.AttrOutcome])
		is.Equal(t, " selec", e[engine.AttrSQL])
	})
}
