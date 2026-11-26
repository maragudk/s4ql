package engine_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
	"maragu.dev/s4ql/internal/sqlite"
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

// recorder is a logger that records the events of statements while it is on, each by its kind, its
// outcome, and the kind of its transaction, and is not enabled while it is off.
type recorder struct {
	mu     sync.Mutex
	on     bool
	events []string
}

func (h *recorder) Enabled(context.Context, slog.Level) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.on
}

func (h *recorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recorder) WithGroup(string) slog.Handler      { return h }

func (h *recorder) Handle(_ context.Context, r slog.Record) error {
	if r.Message != engine.EventStatement {
		return nil
	}
	m := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.on {
		h.events = append(h.events, eventOf(m[engine.AttrKind], m[engine.AttrOutcome], m[engine.AttrTxKind]))
	}
	return nil
}

// eventOf a statement of the kind, with the outcome, in a transaction of the kind tx, or "" for
// none, as [recorder] records it.
func eventOf(kind, outcome, tx string) string {
	return fmt.Sprintf("%q %v %q", kind, outcome, tx)
}

// start recording, from no event.
func (h *recorder) start() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.on, h.events = true, nil
}

// stop recording, and give the events recorded since [recorder.start].
func (h *recorder) stop() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.on = false
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

// The types of SQLite's tokens that [sqliteParts] reads, as [sqlite.Tokenize] gives them: a
// semicolon, a token of spaces, and a comment. SQLite numbers them anew for each version, so they
// are read from the tokens of texts that hold one each.
var sqliteSemi, sqliteSpace, sqliteComment = sqlite.Tokenize(";")[0].Type, sqlite.Tokenize(" ")[0].Type, sqlite.Tokenize("-- c")[0].Type

func TestSqliteParts(t *testing.T) {
	t.Run("should read the statements of a text as SQLite does, with the spaces, comments, and empty statements before each in its part", func(t *testing.T) {
		tests := []struct {
			src  string
			want string
		}{
			{src: "", want: ""},
			{src: " ; -- c\n", want: ""},
			{src: "select 1", want: `"select 1" "select 1" false`},
			{src: "select 1; select 2", want: `"select 1;" "select 1;" true | " select 2" "select 2" false`},
			{src: ";; select 1 ;\n-- c\n;select 2; -- d", want: `";; select 1 ;" "select 1 ;" true | "\n-- c\n;select 2; -- d" "select 2;" true`},
			{src: `insert into "a;b" values ('c;d'); /* e; */ select 1 -- f;`, want: `"insert into \"a;b\" values ('c;d');" "insert into \"a;b\" values ('c;d');" true | " /* e; */ select 1 -- f;" "select 1" false`},
			{src: "create trigger r after insert on t begin select 1; select 2; end; select 3",
				want: `"create trigger r after insert on t begin select 1; select 2; end;" "create trigger r after insert on t begin select 1; select 2; end;" true | " select 3" "select 3" false`},
			{src: "select 'a; select 2", want: `"select 'a; select 2" "select 'a; select 2" false`},
		}
		for _, test := range tests {
			var got []string
			for _, part := range sqliteParts(test.src) {
				got = append(got, fmt.Sprintf("%q %q %v", test.src[part.span.Start:part.span.End], test.src[part.statement.Start:part.statement.End], part.semi))
			}
			is.Equal(t, test.want, strings.Join(got, " | "), test.src)
		}
	})
}

func TestSpaceRule(t *testing.T) {
	t.Run("should refuse where Go's TrimSpace removes what SQLite reads as part of a token, as stmt.Split does", func(t *testing.T) {
		for _, src := range []string{
			"select 1; select 2", "select 1;\t\v select 2", "select 1; select 2 ", "select 1; select 2", "select 1; select 2 ",
			"select 1; select 2; /* ", "select 1; select 'abc  ", "select 1; select 2 -- c\u0085", "select 1;\vselect 2", "selec 1; select 2",
			"select 1", "select 1 ", "select 1; 　", "select 1 -- c\n;\n ",
		} {
			parts, err := stmt.Split(src)
			what, at := spaceRule(src, sqliteParts(src))
			want := ""
			if what != "" {
				want = fmt.Sprintf("s4ql: not supported: %v (at offset %v)", what, at)
			}
			got := ""
			if err != nil && len(parts) == 0 && strings.Contains(err.Error(), "SQLite reads as part of a token") {
				got = err.Error()
			}
			is.Equal(t, want, got, fmt.Sprintf("%q", src))
		}
	})
}

// sqlitePart of a text of several statements, as SQLite reads the statement and the oracle runs it
// alone.
type sqlitePart struct {
	// span of the part of the text that runs as the statement alone: from the end of the part before,
	// or the start of the text, to the end of its semicolon, and for the last part to the end of the
	// text, so that the spaces, comments, and empty statements before a statement belong to it.
	span stmt.Span
	// statement is the span of the statement: from its first token to the end of its semicolon, or of
	// its last token if it ends the text without one.
	statement stmt.Span
	// semi reports whether the statement ends in a semicolon.
	semi bool
}

// sqliteParts of the text src, as SQLite reads its statements one after another: on the tokens of
// SQLite's own tokenizer, a statement ends at the first semicolon after which sqlite3_complete takes
// the text from the statement's first token on for complete, so that a semicolon within the body of a
// CREATE TRIGGER ends none; spaces, comments, and empty statements are in no statement. The oracle
// reads the statements so, and not as [stmt.Split] reads them.
func sqliteParts(src string) []sqlitePart {
	var parts []sqlitePart
	from, first, last, offset := 0, -1, 0, 0
	for _, tok := range sqlite.Tokenize(src) {
		if tok.Len == 0 {
			break
		}
		end := offset + tok.Len
		switch {
		case tok.Type == sqliteSpace || tok.Type == sqliteComment:
		case tok.Type == sqliteSemi && first < 0:
		case tok.Type == sqliteSemi && sqlite.Complete(src[first:end]):
			parts = append(parts, sqlitePart{span: stmt.Span{Start: from, End: end}, statement: stmt.Span{Start: first, End: end}, semi: true})
			from, first = end, -1
		default:
			if first < 0 {
				first = offset
			}
			last = end
		}
		offset = end
	}
	switch {
	case first >= 0:
		parts = append(parts, sqlitePart{span: stmt.Span{Start: from, End: len(src)}, statement: stmt.Span{Start: first, End: last}})
	case len(parts) > 0:
		parts[len(parts)-1].span.End = len(src)
	}
	return parts
}

// spaceRule is the oracle's rule of the spaces of a text of several statements, read with SQLite's
// tokens: what s4ql's refusal says, and where, of a text that it must refuse before any statement
// runs, or "" and 0. mattn hands SQLite each statement after the first as the text after the semicolon
// of the statement before it with Go's strings.TrimSpace applied, so where the trim removes what SQLite
// reads as part of a token, mattn and SQLite read the text differently, and s4ql refuses it: at the
// start of the text after a statement, and, if text follows the first statement, at the end of the
// text. The statements are those of parts, up to the first that the rules of a statement alone refuse
// or that ends without a semicolon, as the rule of the driver check reads them on s4ql's tokens.
func spaceRule(src string, parts []sqlitePart) (string, int) {
	trimmed := len(strings.TrimRightFunc(src, unicode.IsSpace))
	for j, part := range parts {
		if s, err := stmt.Check(src[part.statement.Start:part.statement.End]); err != nil || s == nil || !part.semi {
			return "", 0
		}
		at := part.statement.End
		if j == 0 && at < len(src) {
			if end, ok := trimmedEnd(src, trimmed); ok {
				return "space at the end of the text that SQLite reads as part of a token", end
			}
		}
		if at >= trimmed {
			return "", 0
		}
		// The spaces that the trim removes at the start of the text after the statement must be SQLite's
		// tokens of spaces.
		spaces := len(src) - len(strings.TrimLeftFunc(src[at:], unicode.IsSpace))
		for _, tok := range sqlite.Tokenize(src[at:spaces]) {
			if tok.Len > 0 && tok.Type != sqliteSpace {
				return "space after a statement that SQLite reads as part of a token", at
			}
			at += tok.Len
		}
	}
	return "", 0
}

// trimmedEnd is where SQLite reads the end of src from the offset trimmed on, which Go's TrimSpace
// removes, otherwise than it reads src without it, and true: at trimmed, if a token that is no
// comment that stays one runs past it, such as a string without its closing quote, or a block
// comment of which the trim leaves `/*`, which SQLite reads as a slash; or at the first token after
// trimmed that is not one of SQLite's spaces. It reports false if SQLite reads both alike.
func trimmedEnd(src string, trimmed int) (int, bool) {
	offset := 0
	for _, tok := range sqlite.Tokenize(src) {
		start := offset
		offset += tok.Len
		switch {
		case tok.Len == 0 || offset <= trimmed:
		case start < trimmed && tok.Type == sqliteComment:
			if src[start:trimmed] == "/*" {
				return trimmed, true
			}
		case start < trimmed:
			return trimmed, true
		case tok.Type != sqliteSpace:
			return start, true
		}
	}
	return 0, false
}

// atOffset is the offset that the message of an error of s4ql other than SQLite's says, as a
// [*stmt.NotSupportedError] and an evaluation error write it.
var atOffset = regexp.MustCompile(`\(at offset (\d+)\)$`)

// shifted answer a of a statement that ran alone as the part of a text that starts at the offset
// start: with the offset of SQLite's error, and the offset that the message of s4ql's own error says,
// in the whole text.
func shifted(a answer, start int) answer {
	if strings.Contains(a.err, "/") && a.offset >= 0 {
		// SQLite's error, at an offset, or at -1 for none.
		a.offset += start
	}
	if m := atOffset.FindStringSubmatchIndex(a.message); m != nil {
		n, err := strconv.Atoi(a.message[m[2]:m[3]])
		if err != nil {
			panic(err)
		}
		a.message = a.message[:m[2]] + fmt.Sprint(n+start) + a.message[m[3]:]
	}
	return a
}

// parameterNoun for n parameters, as the messages of s4ql's rules write it.
func parameterNoun(n int) string {
	if n == 1 {
		return "parameter"
	}
	return "parameters"
}

// refusedBefore is the answer of s4ql's rules that refuse the text src of several statements, whose
// statements are parts, with n arguments, before any statement runs, in the order of
// [engine.CheckAll], and why: "spaces", for its spaces, as [spaceRule] says; "none", for no
// statement; and "arguments", for more arguments than parameters, if the rules of a statement alone
// accept every statement. It gives the zero answer and "" for a text that they do not refuse.
func refusedBefore(src string, n int, parts []sqlitePart) (answer, string) {
	total, checked := 0, true
	for _, part := range parts {
		s, err := stmt.Check(src[part.statement.Start:part.statement.End])
		if err != nil || s == nil {
			checked = false
			break
		}
		total += s.Parameters()
	}
	what, at := spaceRule(src, parts)
	switch {
	case what != "":
		return answer{err: "not supported", message: fmt.Sprintf("s4ql: not supported: %v (at offset %v)", what, at)}, "spaces"
	case len(parts) == 0:
		return answer{err: "no statement", message: engine.ErrNoStatement.Error()}, "none"
	case len(parts) > 1 && checked && n > total:
		return answer{err: wrongArguments, message: fmt.Sprintf("s4ql: wrong number of arguments: %v for a text whose %v statements have %v %v", n, len(parts), total, parameterNoun(total))}, "arguments"
	}
	return answer{}, ""
}

// several runs the call c of a text of several statements through [engine.Conn.ExecAll] on the other
// instance of the pair, and requires what running its statements one after another gives: the
// statements of [sqliteParts], each with as many of the arguments as it has parameters, in order, run
// alone by [pair.step] on the instance and in SQLite, which judges each by the rules of a statement
// alone, up to the first that fails. Before any statement runs, the text must be refused as s4ql's
// rules say: for its spaces, as [spaceRule] says, and for more arguments than its statements have
// parameters, if the rules of a statement alone accept every statement. Of the call it requires the
// error of the statement at which the walk stopped, with its offset in the whole text, or the error
// of fewer arguments than that statement has parameters as s4ql's rule writes it; the result of the
// last statement that set the count of changes; the tables after it, also after a failed call, read
// in the transaction if one is open; and the commits that the other instance writes, with their
// changes and their annotations, which must be the instance's commits of the statements alone: one
// for each statement that changed something outside a transaction, and none in a transaction.
func (p *pair) several(c stmttest.Call) string {
	t, i, other := p.t, p.i, p.other
	t.Helper()
	src := c.Src
	fail := func(format string, args ...any) string {
		return fmt.Sprintf("the text of several statements %q with %#v: %v", src, c.Args, fmt.Sprintf(format, args...))
	}
	if !utf8.ValidString(src) || strings.ContainsRune(src, 0) {
		return fail("the oracle has no rule of a text that is not valid UTF-8 or holds NUL")
	}
	parts := sqliteParts(src)
	logged := i.store != nil && other.store != nil
	var iLog, otherLog int
	if logged {
		iLog, otherLog = len(logChanges(t, i.store)), len(logChanges(t, other.store))
	}
	want, why := refusedBefore(src, len(c.Args), parts)
	refused := why != ""
	switch why {
	case "spaces":
		p.count("several statements: refused for a space that mattn trims")
	case "arguments":
		p.count("several statements: more arguments than parameters")
	}
	// The walk: each statement alone on the instance and in SQLite, up to the first that fails.
	stopped, ran := -1, 0
	// stopStatement is the statement at which the walk stopped, as the rules of a statement alone read
	// it, with the arguments stopArgs.
	var stopStatement stmt.Statement
	var stopArgs []any
	sets := i.sets
	// before is the count of changes before the last statement that ran, and created and changed hold
	// the tables, folded, that a statement of the text that ran before created, or changed rows of.
	before := i.changes
	created, changed := map[string]bool{}, map[string]bool{}
	// The events of the statements that the instance logs in the walk, and the statements that it
	// counts in the transaction that is open, which the other instance must log and count in ExecAll.
	recorded := i.events != nil && other.events != nil
	var walked []string
	iTx := i.c.TxStatements()
	if refused {
		// One event of the whole text, without a kind, as that of a text that fails its check.
		walked = []string{eventOf("", outcomeOf(want.err), p.txKind())}
		if iTx >= 0 {
			iTx--
		}
	}
	if !refused {
		p.walking = true
		if recorded {
			i.events.start()
		}
		used := 0
		for j, part := range parts {
			alone := src[part.span.Start:part.span.End]
			s, err := stmt.Check(alone)
			take := c.Args[used:]
			n := 0
			if err == nil && s != nil && len(parts) > 1 {
				// A text of one statement counts its arguments as Exec does, and a statement that the
				// rules refuse is refused before its arguments.
				n = s.Parameters()
				take = c.Args[used:min(len(c.Args), used+n)]
			}
			before = i.changes
			setsBefore := i.sets
			if j >= len(c.Supports) {
				p.walking = false
				return fail("the walk reaches its statement %v of %v, and the generator wrote %v texts, so the oracle cannot tell whether s4ql supports it", j+1, len(parts), len(c.Supports))
			}
			// Each statement is judged by whether it itself is one that s4ql supports, as a text of one.
			failure := p.step(stmttest.Call{Src: alone, Args: take, Supported: c.Supports[j]})
			if failure != "" {
				p.walking = false
				return fail("its statement %v of %v alone: %v", j+1, len(parts), failure)
			}
			if p.last.err != "" {
				stopped, want = j, shifted(p.last, part.span.Start)
				stopStatement, stopArgs = s, take
				if want.err == wrongArguments && len(parts) > 1 {
					want.message = fmt.Sprintf("s4ql: wrong number of arguments: %v for a text whose statements up to the one at offset %v have %v %v", len(c.Args), part.statement.Start, used+n, parameterNoun(used+n))
					p.count("several statements: fewer arguments than parameters")
				}
				break
			}
			used += len(take)
			ran++
			p.reachDependent(s, created, changed, i.sets > setsBefore && i.changes > 0)
		}
		p.walking = false
		if recorded {
			walked = i.events.stop()
		}
	}
	iTx = i.c.TxStatements() - iTx
	// The other instance reads as the rules choose, and computes its own subqueries, which no
	// statement of the walk forces.
	other.e.SetPath(engine.Path{})
	other.e.ObserveSubqueries(func(_ context.Context, vs []*s4qlpb.Value) []*s4qlpb.Value { return vs })
	otherTx := other.c.TxStatements()
	if recorded {
		other.events.start()
	}
	res, err := other.c.ExecAll(t.Context(), src, values(c.Args)...)
	var execAllEvents []string
	if recorded {
		execAllEvents = other.events.stop()
	}
	otherTx = other.c.TxStatements() - otherTx
	if res.SetsChanges {
		other.changes = res.Changes
	}
	got := answer{}
	if err != nil {
		got = other.errAnswer(src, err)
	}
	p.last = got
	if got.err == evaluationError && want.err == evaluationError && got.message != want.message {
		// Which row of a statement with arithmetic fails first depends on the directions of the scans,
		// which the instances draw each of their own: the error of the other instance must be one of the
		// statement at which the walk stopped, and be justified as [pair.justify] says, on SQLite's rows
		// before that statement.
		part := parts[stopped]
		ee := *got.eval
		ee.Span.Start -= part.span.Start
		ee.Span.End -= part.span.Start
		if got.eval.Span.Start < part.statement.Start || got.eval.Span.Start >= part.statement.End {
			return fail("ExecAll gives %q, which is not of the statement at which the statements one after another stopped, with %q", got.message, want.message)
		}
		if why := p.justify(stopStatement, oracleArgs(stopArgs), &ee); why != "" {
			return fail("ExecAll gives %q, which %v, and the statements one after another %q", got.message, why, want.message)
		}
		p.count("several statements: an evaluation error of another row than the statement alone met, justified")
		want = got
	}
	if got.err != want.err || got.message != want.message || got.offset != want.offset {
		return fail("ExecAll gives\n%v (%q at offset %v)\nand the statements one after another\n%v (%q at offset %v)\nafter %v of them ran", got, got.message, got.offset, want, want.message, want.offset, ran)
	}
	wantRes := engine.Result{}
	if i.sets > sets {
		wantRes = engine.Result{Changes: i.changes, SetsChanges: true}
	}
	if res != wantRes {
		return fail("ExecAll gives the result %+v, and the statements one after another %+v", res, wantRes)
	}
	// Each statement runs once, in order, as alone: its event, by its kind, its outcome, and its
	// transaction, and its count in the transaction that is open, also where a second run of it would
	// change nothing else, as a select does.
	if recorded && fmt.Sprint(execAllEvents) != fmt.Sprint(walked) {
		return fail("ExecAll logs the events of statements\n%v\nand the statements one after another\n%v", execAllEvents, walked)
	}
	if otherTx != iTx {
		return fail("ExecAll counts %v statements in the transaction, and the statements one after another %v", otherTx, iTx)
	}
	if failure := p.tablesAfter(src, parts); failure != "" {
		return fail("%v", failure)
	}
	if logged {
		iCommits, otherCommits := logChanges(t, i.store)[iLog:], logChanges(t, other.store)[otherLog:]
		iNotes, otherNotes := logAnnotations(t, i.store)[iLog:], logAnnotations(t, other.store)[otherLog:]
		switch {
		case p.inTx && len(otherCommits) > 0:
			return fail("ExecAll in a transaction writes the commits %v", otherCommits)
		case fmt.Sprint(otherCommits) != fmt.Sprint(iCommits):
			return fail("ExecAll writes the commits\n%v\nand the statements one after another\n%v", otherCommits, iCommits)
		case fmt.Sprint(otherNotes) != fmt.Sprint(iNotes):
			return fail("ExecAll annotates its commits with\n%v\nand the statements one after another with\n%v", otherNotes, iNotes)
		}
		switch {
		case p.inTx:
		case len(otherCommits) == 1:
			p.count("several statements: one commit of a text outside a transaction")
		case len(otherCommits) > 1:
			p.count("several statements: a commit of each of two statements or more of a text outside a transaction")
		}
	}
	p.reachSeveral(src, parts, stopped, want, ran, i.sets > sets, before)
	return ""
}

// outcomeOf the event of a text that the rules refuse before any statement runs with the error err,
// as an [answer] names it.
func outcomeOf(err string) string {
	switch err {
	case "not supported":
		return "not_supported"
	case wrongArguments:
		return "wrong_arguments"
	case "no statement":
		return "no_statement"
	}
	panic("no outcome of a refusal before any statement runs is " + err)
}

// txKind of the transaction of the pair that is open, as the event of a statement in it names it, or
// "" if none is open.
func (p *pair) txKind() string {
	switch {
	case !p.inTx:
		return ""
	case p.readOnly:
		return "read_only"
	}
	return "write"
}

// reachDependent counts in the tally a statement s of a text of several statements that ran on a
// table that an earlier statement of the text created, or changed rows of, which created and changed
// hold, and records in them the table of s if s created it, or if it changed rows of it, as wrote
// says.
func (p *pair) reachDependent(s stmt.Statement, created, changed map[string]bool, wrote bool) {
	var table stmt.Name
	switch s := s.(type) {
	case *stmt.CreateTable:
		created[s.Table.Fold()] = true
		return
	case *stmt.Insert:
		table = s.Table
	default:
		table, _ = tableWhere(s)
	}
	if table.Value == "" {
		return
	}
	if created[table.Fold()] {
		p.count("several statements: a statement that ran on a table that an earlier statement of the text created")
	}
	if changed[table.Fold()] {
		p.count("several statements: a statement that ran on a table whose rows an earlier statement of the text changed")
	}
	if wrote {
		changed[table.Fold()] = true
	}
}

// reachSeveral counts in the tally the outcome of the text src of several statements, whose statements
// of parts ran up to the one of the number stopped, from 0, which gave want, or to the end for -1,
// ran of them, as [pair.several] judged it: a text that ran to its end, by its number of statements,
// and one whose last statement has RETURNING, also where a statement of the text set the count of
// changes, as set says, and the count before the last statement, before, differs from the count after
// it; and a text that stopped at a statement, by the position of the statement, first, in the middle,
// or last, and by its cause. It counts a text in a transaction by the kind of the transaction too.
func (p *pair) reachSeveral(src string, parts []sqlitePart, stopped int, want answer, ran int, set bool, before int64) {
	if p.y == nil {
		return
	}
	in := map[bool]string{true: "read-only", false: "write"}[p.readOnly]
	if p.inTx {
		p.count("several statements: a text in a " + in + " transaction")
		p.severalFailed = p.severalFailed || want.err != ""
	}
	if want.err == "" {
		p.count(fmt.Sprint("several statements: a text of ", len(parts), " statements that ran to its end"))
		last := parts[len(parts)-1].statement
		if s, _ := stmt.Check(src[last.Start:last.End]); s != nil && returning(s) {
			p.count("several statements: a last statement with RETURNING that ran")
			if set && p.i.changes != before {
				p.count("several statements: a last statement with RETURNING that ran, after another count of changes")
			}
		}
		return
	}
	if stopped < 0 {
		return
	}
	position := "a middle"
	switch stopped {
	case 0:
		position = "the first"
	case len(parts) - 1:
		position = "the last"
	}
	cause := "SQLite's error"
	switch {
	case strings.HasPrefix(want.err, fmt.Sprint(sqlerr.CodeConstraint, "/")):
		cause = "a constraint"
	case want.err == evaluationError:
		cause = "an evaluation error"
	case want.err == "not supported":
		cause = "not supported"
	case want.err == wrongArguments:
		cause = "too few arguments"
	}
	p.count("several statements: stopped at " + position + " statement: " + cause)
	if ran > 0 && p.inTx {
		p.count("several statements: stopped after a statement ran, in a " + in + " transaction")
	}
}

// returning reports whether the insert, the update, or the delete s has RETURNING.
func returning(s stmt.Statement) bool {
	switch s := s.(type) {
	case *stmt.Insert:
		return s.Returning.Present()
	case *stmt.Update:
		return s.Returning.Present()
	case *stmt.Delete:
		return s.Returning.Present()
	}
	return false
}

// sameLog of the instance and the other instance of the pair, which run the same calls, the other
// instance each text of several statements in one call and the instance its statements alone: the
// same commits, with the same changes and annotations, in the same order. It gives what differs, or
// "", also for instances that keep no log.
func (p *pair) sameLog() string {
	t, i, other := p.t, p.i, p.other
	t.Helper()
	if i.store == nil || other.store == nil {
		return ""
	}
	if got, want := fmt.Sprint(logChanges(t, other.store)), fmt.Sprint(logChanges(t, i.store)); got != want {
		return fmt.Sprintf("the other instance has the commits\n%v\nand the instance\n%v", got, want)
	}
	if got, want := fmt.Sprint(logAnnotations(t, other.store)), fmt.Sprint(logAnnotations(t, i.store)); got != want {
		return fmt.Sprintf("the other instance has the annotations\n%v\nand the instance\n%v", got, want)
	}
	return ""
}

// tablesAfter the text src of several statements, whose statements are those of parts: every table
// of the other instance must hold SQLite's columns and rows, read in the transaction if one is open.
// In a transaction, the tables of SQLite are read, and the tables that a create table or a drop
// table of the text names, which the other instance must lack where SQLite does. It gives what
// differs, or "".
func (p *pair) tablesAfter(src string, parts []sqlitePart) string {
	t, other, o := p.t, p.other, p.o
	t.Helper()
	if !p.inTx {
		if got, want := other.state(), o.state(t); got != want {
			return fmt.Sprintf("after it, the other instance holds\n%v\nand SQLite\n%v", got, want)
		}
		return ""
	}
	tables := o.run(t, "select name from sqlite_schema where type = 'table' order by name collate binary")
	is.Equal(t, "", tables.err)
	var names []string
	for _, quoted := range tables.rows {
		names = append(names, unquoted(t, quoted))
	}
	for _, part := range parts {
		switch s, _ := stmt.Check(src[part.statement.Start:part.statement.End]); s := s.(type) {
		case *stmt.CreateTable:
			names = append(names, s.Table.Value)
		case *stmt.DropTable:
			names = append(names, s.Table.Value)
		}
	}
	for _, name := range names {
		read := "select * from " + stmttest.Quote(name)
		if got, want := other.queryValues(read, nil), o.run(t, read); got.String() != want.String() || got.columns != want.columns {
			return fmt.Sprintf("after it, in the transaction, %v gives\n%v (%v)\nand in SQLite\n%v (%v)", read, got, got.columns, want, want.columns)
		}
	}
	return ""
}
