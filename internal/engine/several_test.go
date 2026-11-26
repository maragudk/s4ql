package engine_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
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

// canceling is a logger that calls cancel when it logs the event of the statement number at, from
// 1, and counts the events of statements.
type canceling struct {
	mu         sync.Mutex
	at, events int
	cancel     func()
}

func (h *canceling) Enabled(context.Context, slog.Level) bool { return true }
func (h *canceling) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *canceling) WithGroup(string) slog.Handler            { return h }

func (h *canceling) Handle(_ context.Context, r slog.Record) error {
	if r.Message != engine.EventStatement {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events++
	if h.events == h.at {
		h.cancel()
	}
	return nil
}

// count of the events of statements that h has logged.
func (h *canceling) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.events
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

	t.Run("should refuse an argument for its statement, by its number among the arguments of its statement, after the statements before it", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		one := integers([]int64{1})[0]
		nan := s4qlpb.Value_builder{Real: new(math.NaN())}.Build()
		text := s4qlpb.Value_builder{Text: new("x")}.Build()
		two := t1 + "; insert into t1 (v1) values (?); insert into t1 (v1) values (?)"
		_, err := i.c.ExecAll(t.Context(), two, one, nan)
		is.True(t, errors.Is(err, stmt.ErrNotSupported))
		is.Equal(t, "s4ql: not supported: argument 1, which is a real that is not a number", err.Error())
		is.Equal(t, "t1 (v1 INT): 1\n", i.state())

		_, err = i.c.ExecAll(t.Context(), "insert into t1 (v1) values (?); select * from t1 where v1 = ? limit ?", integers([]int64{2})[0], one, text)
		is.Equal(t, "s4ql: not supported: argument 2, which is text, for LIMIT, which takes an integer from 0", err.Error())
		_, err = i.c.ExecAll(t.Context(), "insert into t1 (v1) values (?); insert into t1 (v1) values (?)", integers([]int64{3})[0], text)
		is.Equal(t, "s4ql: not supported: argument 1, which is text, for the column t1.v1 of type INT", err.Error())
		is.Equal(t, "t1 (v1 INT): 1 2 3\n", i.state())
	})

	t.Run("should check a text before any statement runs as ExecAll does", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		tests := []struct {
			src string
			n   int
		}{
			{src: t1 + "; insert into t1 (v1) values (?)", n: 2},
			{src: t1 + "; insert into t1 (v1) values (?)", n: 0},
			{src: t1 + "; insert into t1 (v1) values (?); selec", n: 3},
			{src: "insert into t1 (v1) values (?);", n: 2},
			{src: "insert into t1 (v1) values (?);", n: 0},
			{src: "selec; " + t1, n: 0},
			{src: t1 + "; ", n: 0},
			{src: " ; ", n: 0},
		}
		for _, test := range tests {
			want := engine.CheckAll(test.src, test.n)
			_, err := i.c.ExecAll(t.Context(), test.src, integers(make([]int64, test.n))...)
			if want != nil {
				is.Equal(t, want.Error(), err.Error(), test.src)
				is.Equal(t, "", i.state(), test.src)
				continue
			}
			// The statements before an error of the run ran.
			is.True(t, err == nil || i.state() != "", test.src)
			_, err = i.execAll("drop table if exists t1")
			is.NotError(t, err)
		}
	})

	t.Run("should refuse what Split refuses of the whole text, and a text without a statement, before any statement runs", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		for src, want := range map[string]string{
			t1 + ";\u00a0" + t1:      "s4ql: not supported: space after a statement that SQLite reads as part of a token (at offset 59)",
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

	t.Run("should run the statement after a cancel of the context as it runs alone with that context, keep the statements before it, and run none after it", func(t *testing.T) {
		for _, inTx := range []bool{false, true} {
			name := fmt.Sprint("in a transaction: ", inTx)
			// The logger cancels the context when it logs the event of the first insert, so the cancel
			// comes between two statements, and the test waits for nothing.
			h := &canceling{at: 2}
			i := newInstance(t, engine.Options{Logger: slog.New(h)})
			_, err := i.execAll(t1)
			is.NotError(t, err, name)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h.cancel = cancel
			if inTx {
				is.NotError(t, i.c.Begin(t.Context()), name)
			}
			_, err = i.c.ExecAll(ctx, "insert into t1 (v1) values (1); insert into t1 (v1) values (2); insert into t1 (v1) values (3)")
			is.Equal(t, 3, h.count(), name)
			_, alone := i.c.Exec(ctx, "insert into t1 (v1) values (2)")
			is.Error(t, context.Canceled, alone, name)
			is.Error(t, context.Canceled, err, name)
			is.Equal(t, alone.Error(), err.Error(), name)
			if inTx {
				is.NotError(t, i.c.Commit(t.Context()), name)
			}
			is.Equal(t, "t1 (v1 INT): 1\n", i.state(), name)
		}
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

	t.Run("should give what Exec gives for each text of one statement of the generated workload: its result, its error with its offset, and the state after it, also in transactions", func(t *testing.T) {
		compared := 0
		for _, p := range stmttest.Profiles {
			for k := range uint64(6) {
				seed := p.Seed(k)
				q, err := stmttest.NewSequence(seed)
				is.NotError(t, err)
				// Both draw the directions of their scans from the same seed, so that a statement whose
				// outcome depends on the rows that a scan meets first meets them alike.
				one, all := sameDirections(t, seed), sameDirections(t, seed)
				r := q.Stream("transactions")
				inTx := false
				for n := range 120 {
					c := q.Next()
					if parts, failed := stmt.Split(c.Src); c.Statements > 0 || len(parts) > 1 || len(parts) == 1 && failed != nil {
						// A text of several statements, which Exec refuses for its second statement.
						continue
					}
					switch {
					case !inTx && r.IntN(8) == 0:
						begin := (*engine.Conn).Begin
						if r.IntN(3) == 0 {
							begin = (*engine.Conn).BeginReadOnly
						}
						is.NotError(t, begin(one.c, t.Context()))
						is.NotError(t, begin(all.c, t.Context()))
						inTx = true
					case inTx && r.IntN(5) == 0:
						end := (*engine.Conn).Commit
						if r.IntN(3) == 0 {
							end = (*engine.Conn).Rollback
						}
						is.Equal(t, fmt.Sprint(end(one.c, t.Context())), fmt.Sprint(end(all.c, t.Context())))
						inTx = false
					}
					args := values(c.Args)
					wantRes, wantErr := one.c.Exec(t.Context(), c.Src, args...)
					gotRes, gotErr := all.c.ExecAll(t.Context(), c.Src, args...)
					what := fmt.Sprintf("seed %v, call %v: %.500q", seed, n, c.Src)
					is.Equal(t, fmt.Sprint(wantRes), fmt.Sprint(gotRes), what)
					is.Equal(t, errorText(wantErr), errorText(gotErr), what)
					if n%10 == 0 || wantErr == nil {
						is.Equal(t, one.state(), all.state(), what)
					}
					compared++
				}
				if inTx {
					is.NotError(t, one.c.Commit(t.Context()))
					is.NotError(t, all.c.Commit(t.Context()))
				}
				is.Equal(t, one.state(), all.state(), fmt.Sprint("seed ", seed))
				one.closeMemory()
				all.closeMemory()
			}
		}
		is.True(t, compared > 4000, fmt.Sprint(compared, " texts compared"))
	})
}

// sameDirections is an instance of the engine on a database in memory, whose engine draws the
// directions of its scans from seed.
func sameDirections(t *testing.T, seed uint64) *instance {
	t.Helper()
	i := &instance{t: t, seed: seed}
	i.e, i.memory = engine.NewMemory(sim.NewFree(sim.Buggify{}), i.directed(engine.Options{}))
	i.c = i.e.Conn()
	return i
}

// closeMemory closes the connection of an instance of [sameDirections], after which nothing holds it.
func (i *instance) closeMemory() {
	i.t.Helper()
	is.NotError(i.t, i.c.Close(context.WithoutCancel(i.t.Context())))
}

// errorText of err for a comparison of two errors: its type, its message, the offset of SQLite's
// error, and the sentinels of the engine that it matches, or "nil".
func errorText(err error) string {
	if err == nil {
		return "nil"
	}
	text := fmt.Sprintf("%T %v", err, err)
	if se, ok := errors.AsType[*sqlerr.Error](err); ok {
		text += fmt.Sprint(" at offset ", se.Offset)
	}
	for _, sentinel := range []error{engine.ErrArguments, engine.ErrNoStatement, stmt.ErrNotSupported, engine.ErrTxOpen, engine.ErrNoTx, engine.ErrRowsOpen} {
		if errors.Is(err, sentinel) {
			text += fmt.Sprint(", matches ", sentinel)
		}
	}
	return text
}
