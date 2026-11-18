package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
	"maragu.dev/s4ql/internal/store/simstore"
)

const t1 = "create table t1 (v1 int primary key) strict, without rowid"

// pin runs the statements of setup and then src on a new instance of the engine and on SQLite, and
// requires the same answer of both, and the answer want: an error as "code/extended: message", or
// "columns ...; rows ..." for a query, or "" for a statement that changes the database.
func pin(t *testing.T, setup []string, src, want string) {
	t.Helper()
	i := newInstance(t, engine.Options{})
	o := newOracle(t)
	for _, s := range setup {
		is.Equal(t, "", i.exec(s).err, s)
		is.Equal(t, "", o.run(t, s).err, s)
	}
	query := strings.Contains(strings.ToLower(src), "select")
	var got answer
	if query {
		got = i.query(src)
	} else {
		got = i.exec(src)
	}
	sqlite := o.run(t, src)
	if !query {
		sqlite.columns, sqlite.rows = "", nil
	}
	is.Equal(t, sqlite.String(), got.String(), "SQLite and the engine")
	text := got.err
	if got.err == "" && query {
		text = fmt.Sprintf("columns %v; rows %v", got.columns, strings.Join(got.rows, " "))
	}
	is.Equal(t, want, text)
}

func TestEngine_errors(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		src   string
		want  string
	}{
		{name: "should give that a table exists, with its name as written", setup: []string{t1},
			src: `create table "T1" (x int primary key) strict, without rowid`, want: `1/1: table "T1" already exists`},
		{name: "should give that a table exists for a name in another case", setup: []string{t1},
			src: "CREATE TABLE T1 (x int primary key) without rowid, strict", want: "1/1: table T1 already exists"},
		{name: "should give the syntax error when no table of the name exists", setup: []string{t1},
			src: "create table t2 (v1 int primary key) strict without rowid", want: `1/1: near "without": syntax error`},
		{name: "should give that there is no table to read, with the name without its quotes",
			src: `select * from "T""2"`, want: `1/1: no such table: T"2`},
		{name: "should give that there is no table to write",
			src: "insert into T2 (v1) values (1)", want: "1/1: no such table: T2"},
		{name: "should give that there is no table to drop",
			src: `drop table "t 2"`, want: "1/1: no such table: t 2"},
		{name: "should give that there is no table before that there is no column",
			src: "insert into t2 (v2) values (1)", want: "1/1: no such table: t2"},
		{name: "should give that there is no column, with the names as the statement writes them", setup: []string{t1},
			src: `insert into "T1" ("V""2") values (1)`, want: `1/1: table T1 has no column named V"2`},
		{name: "should give that a key exists, with the names as the table stores them",
			setup: []string{`create table "Foo bar" ("Bar""x" int primary key) strict, without rowid`, `insert into "foo BAR" ("bar""X") values (-0)`},
			src:   `insert into "FOO BAR" ("BAR""X") values (0)`, want: `19/1555: UNIQUE constraint failed: Foo bar.Bar"x`},
		{name: "should take names of letters outside ASCII in another case for other names",
			setup: []string{`create table "été" (v int primary key) strict, without rowid`},
			src:   `insert into "ÉTÉ" (v) values (1)`, want: "1/1: no such table: ÉTÉ"},
		{name: "should give the rows in the order of the key, with the column as created",
			setup: []string{"create table t (Kéy int primary key) strict, without rowid", "insert into t (kéy) values (3)", "insert into t (kéy) values (-9223372036854775808)", "insert into t (kéy) values (9223372036854775807)", "insert into t (kéy) values (0)"},
			src:   "select * from T", want: "columns Kéy INT; rows -9223372036854775808 0 3 9223372036854775807"},
		{name: "should give no rows for an empty table", setup: []string{t1},
			src: "select * from t1", want: "columns v1 INT; rows "},
		{name: "should give the syntax error of SQLite",
			src: "selec * from t1", want: `1/1: near "selec": syntax error`},
		{name: "should drop a table that a name in another case names", setup: []string{t1, "insert into t1 (v1) values (1)"},
			src: "drop table T1", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pin(t, c.setup, c.src, c.want)
		})
	}

	t.Run("should give the syntax error that SQLite gives on an empty database, after the start of a table that exists", func(t *testing.T) {
		for _, src := range []string{"create table t1 (v1 int primary key) strict without rowid", "create table main.T1 as selec"} {
			i := newInstance(t, engine.Options{})
			o := newOracle(t)
			want := o.run(t, src).err
			is.True(t, strings.HasSuffix(want, ": syntax error"), want)
			is.Equal(t, "", i.exec(t1).err)
			is.Equal(t, "", o.run(t, t1).err)
			is.Equal(t, want, i.exec(src).err, src)
			is.Equal(t, want, i.query(src).err, src)
			// SQLite gives that the table exists, which is the one difference between SQLite and s4ql
			// that the tests accept.
			is.True(t, strings.HasSuffix(o.run(t, src).err, " already exists"), src)
		}
	})

	t.Run("should give the syntax error on a closed instance and with a context that has ended", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(t1).err)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		for _, src := range []string{"create table t1 (v1 int primary key) strict without rowid", "selec * from t1", "insert into t1 (v1) values ("} {
			want := newOracle(t).run(t, src).err
			_, err := i.e.Exec(ctx, src)
			is.Equal(t, want, errText(t, src, err), src)
			_, err = i.e.Query(ctx, src)
			is.Equal(t, want, errText(t, src, err), src)
		}
		i.close()
		for _, src := range []string{"create table T1 (", "selec * from t1"} {
			want := newOracle(t).run(t, src).err
			_, err := i.e.Exec(t.Context(), src)
			is.Equal(t, want, errText(t, src, err), src)
			_, err = i.e.Query(ctx, src)
			is.Equal(t, want, errText(t, src, err), src)
		}
	})

	t.Run("should give not supported, and no statement, as errors of their own", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		_, err := i.e.Exec(t.Context(), "select * from t1 where v1 = 1")
		is.True(t, errors.Is(err, stmt.ErrNotSupported))
		var se *sqlerr.Error
		is.True(t, !errors.As(err, &se))
		_, err = i.e.Exec(t.Context(), `create table "" (v int primary key) strict, without rowid`)
		is.True(t, errors.Is(err, stmt.ErrNotSupported))
		_, err = i.e.Query(t.Context(), " ; -- nothing")
		is.True(t, errors.Is(err, engine.ErrNoStatement))
		_, err = i.e.Exec(t.Context(), "")
		is.True(t, errors.Is(err, engine.ErrNoStatement))
	})

	t.Run("should pass an error of the core through as it is", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		i.close()
		_, err := i.e.Exec(t.Context(), t1)
		is.True(t, errors.Is(err, core.ErrStopped))
		_, err = i.e.Query(t.Context(), "select * from t1")
		is.True(t, errors.Is(err, core.ErrStopped))
		var se *sqlerr.Error
		is.True(t, !errors.As(err, &se))
	})

	t.Run("should pass the error of a context that ended through", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := i.e.Exec(ctx, t1)
		is.True(t, errors.Is(err, context.Canceled))
	})
}

func TestEngine_Exec(t *testing.T) {
	t.Run("should count one change for an insert, and none set by a create table, a drop table, or a query", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		ctx := t.Context()
		res, err := i.e.Exec(ctx, t1)
		is.NotError(t, err)
		is.Equal(t, engine.Result{}, res)
		res, err = i.e.Exec(ctx, "insert into t1 (v1) values (1)")
		is.NotError(t, err)
		is.Equal(t, engine.Result{Changes: 1, SetsChanges: true}, res)
		res, err = i.e.Exec(ctx, "select * from t1")
		is.NotError(t, err)
		is.Equal(t, engine.Result{}, res)
		res, err = i.e.Exec(ctx, "drop table t1")
		is.NotError(t, err)
		is.Equal(t, engine.Result{}, res)
	})

	t.Run("should run a query to its end and give its error", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		_, err := i.e.Exec(t.Context(), "select * from t1")
		is.Equal(t, "no such table: t1", err.Error())
	})
}

func TestEngine_Query(t *testing.T) {
	t.Run("should run a statement that is not a query, with its result and no rows", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		ctx := t.Context()
		_, err := i.e.Exec(ctx, t1)
		is.NotError(t, err)
		rows, err := i.e.Query(ctx, "insert into t1 (v1) values (7)")
		is.NotError(t, err)
		is.Equal(t, engine.Result{Changes: 1, SetsChanges: true}, rows.Result())
		is.Equal(t, 0, len(rows.Columns()))
		for _, err := range rows.All() {
			_ = err
			t.Fatal("a row")
		}
		is.NotError(t, rows.Close())
		is.Equal(t, `columns "v1 INT", rows ["7"]`, i.query("select * from t1").String())
	})

	t.Run("should end the read transaction when the loop stops early, at Close, and at Close during the loop", func(t *testing.T) {
		i := newInstance(t, engine.Options{Logger: slog.New(slog.DiscardHandler)})
		ctx := t.Context()
		_, err := i.e.Exec(ctx, t1)
		is.NotError(t, err)
		for v := range 10 {
			_, err := i.e.Exec(ctx, fmt.Sprint("insert into t1 (v1) values (", v, ")"))
			is.NotError(t, err)
		}
		// The instance has [readers] read connections, so a read transaction that
		// is left open shows after a few queries: the next one waits for a connection.
		for range 10 {
			rows, err := i.e.Query(ctx, "select * from t1")
			is.NotError(t, err)
			for _, err := range rows.All() {
				_ = err
				break
			}
			is.NotError(t, rows.Close())

			rows, err = i.e.Query(ctx, "select * from t1")
			is.NotError(t, err)
			is.NotError(t, rows.Close())
			for _, err := range rows.All() {
				_ = err
				t.Fatal("a row after Close")
			}

			rows, err = i.e.Query(ctx, "select * from t1")
			is.NotError(t, err)
			n := 0
			for _, err := range rows.All() {
				is.NotError(t, err)
				n++
				is.NotError(t, rows.Close())
			}
			is.Equal(t, 1, n)
		}
		noneOpen(t, i)
	})

	t.Run("should end the read transaction when the loop panics", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		ctx := t.Context()
		_, err := i.e.Exec(ctx, t1)
		is.NotError(t, err)
		_, err = i.e.Exec(ctx, "insert into t1 (v1) values (1)")
		is.NotError(t, err)
		for range 10 {
			rows, err := i.e.Query(ctx, "select * from t1")
			is.NotError(t, err)
			func() {
				defer func() { is.Equal(t, "the consumer", recover()) }()
				for _, err := range rows.All() {
					_ = err
					panic("the consumer")
				}
			}()
		}
		noneOpen(t, i)
	})
}

// noneOpen requires that the instance i holds no transaction: a write and every read connection can
// begin. A transaction that is left open makes it wait until the test times out.
func noneOpen(t *testing.T, i *instance) {
	t.Helper()
	ctx := t.Context()
	tx, err := i.d.Begin(ctx)
	is.NotError(t, err)
	var reads []*core.ReadTx
	for range readers {
		r, err := i.d.Read(ctx)
		is.NotError(t, err)
		reads = append(reads, r)
	}
	for _, r := range reads {
		is.NotError(t, r.Close(ctx))
	}
	is.NotError(t, tx.Rollback(ctx))
}

// logAnnotations of the database in the store s, in the order of the log, each commit object as its
// annotations.
func logAnnotations(t testing.TB, s *simstore.Store) [][]string {
	t.Helper()
	data, _, ok := s.Object("db/manifest.json")
	if !ok {
		return nil
	}
	m, err := format.ParseManifest(data)
	is.NotError(t, err)
	var commits [][]string
	for name := m.Head; name != format.EmptyLog; {
		key := name.String()
		data, _, ok := s.Object("db/commits/" + key[:2] + "/" + key)
		is.True(t, ok, key)
		r := format.NewCommitReader(bytes.NewReader(data), name)
		h, err := r.Header()
		is.NotError(t, err)
		var notes []string
		for {
			_, a, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			is.NotError(t, err)
			if a != nil {
				is.Equal(t, 0, len(a.GetStatement().GetParameters()))
				notes = append(notes, a.GetStatement().GetSql())
			}
		}
		commits = append(commits, notes)
		name, err = format.NameFromBytes(h.GetParent())
		is.NotError(t, err)
	}
	slices.Reverse(commits)
	return commits
}

func TestEngine_annotations(t *testing.T) {
	t.Run("should annotate each commit with the text of its statement, from its first token to its end, and commit nothing for a statement that fails", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		texts := []string{
			"  /* before */ create table t1 (v1 int primary key) strict, without rowid; -- after",
			"insert into t1 (v1) /* inside */ values (1)",
			"insert into t1 (v1) values (1)",
			"select * from t1",
			"drop table nosuch",
			";drop table T1;;",
		}
		for _, src := range texts {
			_, _ = i.e.Exec(t.Context(), src)
		}
		is.Equal(t, fmt.Sprint([][]string{
			{"create table t1 (v1 int primary key) strict, without rowid;"},
			{"insert into t1 (v1) /* inside */ values (1)"},
			{"drop table T1;"},
		}), fmt.Sprint(logAnnotations(t, i.store)))
	})
}

// events records the events of a logger.
type events struct {
	mu      sync.Mutex
	records []map[string]string
	levels  []slog.Level
}

func (e *events) Enabled(context.Context, slog.Level) bool { return true }
func (e *events) WithAttrs([]slog.Attr) slog.Handler       { return e }
func (e *events) WithGroup(string) slog.Handler            { return e }

func (e *events) Handle(_ context.Context, r slog.Record) error {
	m := map[string]string{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = append(e.records, m)
	e.levels = append(e.levels, r.Level)
	return nil
}

// last event of the engine, with its level.
func (e *events) last(t *testing.T) (map[string]string, slog.Level) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	is.True(t, len(e.records) > 0)
	return e.records[len(e.records)-1], e.levels[len(e.levels)-1]
}

func TestEngine_events(t *testing.T) {
	t.Run("should log one event for each statement, with its kind, table, outcome, rows, and position, and the text at Debug only", func(t *testing.T) {
		h := &events{}
		ch := &events{}
		i := newInstanceLogged(t, engine.Options{Logger: slog.New(h)}, slog.New(ch))
		ctx := t.Context()

		_, err := i.e.Exec(ctx, t1)
		is.NotError(t, err)
		e, level := h.last(t)
		is.Equal(t, slog.LevelDebug, level)
		is.Equal(t, engine.EventStatement, e["msg"])
		is.Equal(t, "create_table", e[engine.AttrKind])
		is.Equal(t, "t1", e[engine.AttrTable])
		is.Equal(t, "ok", e[engine.AttrOutcome])
		is.Equal(t, "0", e[engine.AttrRows])
		is.Equal(t, t1, e[engine.AttrSQL])
		is.Equal(t, format.EmptyLog.String(), e[engine.AttrParent])
		is.Equal(t, e[engine.AttrParent], e[engine.AttrPosition])
		_, ok := e[engine.AttrDuration]
		is.True(t, ok)

		_, err = i.e.Exec(ctx, "insert into T1 (v1) values (5)")
		is.NotError(t, err)
		e, _ = h.last(t)
		is.Equal(t, "insert", e[engine.AttrKind])
		is.Equal(t, "T1", e[engine.AttrTable])
		is.Equal(t, "1", e[engine.AttrRows])
		parent := e[engine.AttrParent]
		// The event of the commit that the insert caused has the same parent, under the same name.
		commit, _ := ch.last(t)
		is.Equal(t, "commit", commit["msg"])
		is.Equal(t, parent, commit[engine.AttrParent])
		is.Equal(t, "committed", commit[engine.AttrOutcome])

		_, err = i.e.Exec(ctx, "insert into t1 (v1) values (5)")
		is.True(t, err != nil)
		e, level = h.last(t)
		is.Equal(t, slog.LevelDebug, level)
		is.Equal(t, "sqlite_error", e[engine.AttrOutcome])
		is.Equal(t, "19", e[engine.AttrCode])
		is.Equal(t, "1555", e[engine.AttrExtendedCode])
		is.Equal(t, "UNIQUE constraint failed: t1.v1", e[engine.AttrError])
		is.Equal(t, "0", e[engine.AttrRows])
		is.True(t, e[engine.AttrParent] != parent, "the parent of the insert after a commit")

		rows, err := i.e.Query(ctx, "select * from t1")
		is.NotError(t, err)
		for _, err := range rows.All() {
			is.NotError(t, err)
		}
		e, _ = h.last(t)
		is.Equal(t, "select", e[engine.AttrKind])
		is.Equal(t, "1", e[engine.AttrRows])
		_, ok = e[engine.AttrParent]
		is.True(t, !ok, "a query has no parent")
		is.Equal(t, e[engine.AttrPosition] != "", true)

		_, err = i.e.Exec(ctx, "select * from t1 where 1")
		is.True(t, err != nil)
		e, _ = h.last(t)
		is.Equal(t, "not_supported", e[engine.AttrOutcome])
		_, ok = e[engine.AttrKind]
		is.True(t, !ok, "not supported has no kind")

		_, err = i.e.Exec(ctx, "")
		is.True(t, err != nil)
		e, _ = h.last(t)
		is.Equal(t, "no_statement", e[engine.AttrOutcome])

		i.close()
		_, err = i.e.Exec(ctx, "insert into t1 (v1) values (6)")
		is.True(t, err != nil)
		e, level = h.last(t)
		is.Equal(t, slog.LevelDebug, level)
		is.Equal(t, "refused", e[engine.AttrOutcome])
	})

	t.Run("should log no text and no event of a healthy instance at Info", func(t *testing.T) {
		var b strings.Builder
		i := newInstance(t, engine.Options{Logger: slog.New(slog.NewTextHandler(&b, nil))})
		for _, src := range []string{t1, "insert into t1 (v1) values (1)", "insert into t1 (v1) values (1)", "select * from t1", "selec"} {
			_, _ = i.e.Exec(t.Context(), src)
		}
		is.Equal(t, "", b.String())
	})
}

func TestEngine_internal(t *testing.T) {
	t.Run("should never give an error that wraps a refusal of the core", func(t *testing.T) {
		for _, err := range []error{core.ErrTableExists, core.ErrNoTable, core.ErrReservedName, core.ErrInvalid} {
			got := engine.FromCore(fmt.Errorf("x: %w", err))
			is.True(t, errors.Is(got, engine.ErrInternal))
			is.True(t, !errors.Is(got, err))
		}
		is.True(t, errors.Is(engine.FromCore(core.ErrNoLease), core.ErrNoLease))
		is.True(t, engine.FromCore(nil) == nil)
	})
}

func TestEngine_noneOpen(t *testing.T) {
	// run a statement of the sequence on i, and consume the rows of a query in the way mode says:
	// to the end, a loop that stops at once, a close without a loop, or a loop that panics.
	run := func(t *testing.T, i *instance, src string, mode int) {
		t.Helper()
		if mode == 0 {
			_, _ = i.e.Exec(t.Context(), src)
			return
		}
		rows, err := i.e.Query(t.Context(), src)
		if err != nil {
			return
		}
		switch mode {
		case 1:
			for _, err := range rows.All() {
				_ = err
			}
		case 2:
			for _, err := range rows.All() {
				_ = err
				break
			}
		case 3:
			is.NotError(t, rows.Close())
		case 4:
			func() {
				defer func() { _ = recover() }()
				for _, err := range rows.All() {
					_ = err
					panic("the consumer")
				}
			}()
		}
	}

	t.Run("should hold no transaction after any statement, error, early stop, or panic", func(t *testing.T) {
		n := 300
		if slow() {
			n /= 10
		}
		i := newInstance(t, engine.Options{})
		r := rand.New(rand.NewPCG(*seed, 0x0be))
		q := newSequence(t, r)
		for range n {
			src, _ := q.next()
			run(t, i, src, r.IntN(5))
			noneOpen(t, i)
		}
	})

	t.Run("should find a transaction left open with the planted bug", func(t *testing.T) {
		var options engine.Options
		options.Plant("keepOpen")
		i := newInstance(t, options)
		_, err := i.e.Exec(t.Context(), "drop table t")
		is.True(t, err != nil)
		// The transaction is open, so no machine is slow enough for the begin to succeed.
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		_, err = i.d.Begin(ctx)
		is.True(t, errors.Is(err, context.DeadlineExceeded))
	})
}

func TestEngine_namesOfTheCore(t *testing.T) {
	t.Run("should take every name that the pass supports, as SQLite does, in every statement", func(t *testing.T) {
		pools, err := stmttest.NamePools()
		is.NotError(t, err)
		names := slices.Concat(pools...)
		r := rand.New(rand.NewPCG(*seed, 0x4a3e))
		// Random names of letters, marks, symbols, spaces, quotes, and runes from every plane.
		runes := []rune{'a', 'Z', '_', ' ', '"', '\'', '`', '[', ']', ';', '-', '/', '*', '\t', '\n', 'é', 'Ω', 'ı', 'ſ', 0x301, 0x200b, 0xfeff, '\U0001F600', '\U0010FFFD', 0x7f, 0x1}
		n := 2000
		if slow() {
			n /= 10
		}
		for range n {
			b := make([]rune, 1+r.IntN(8))
			for i := range b {
				b[i] = runes[r.IntN(len(runes))]
			}
			names = append(names, stmttest.Quote(string(b)))
		}
		i := newInstance(t, engine.Options{})
		o := newOracle(t)
		checked := 0
		for _, n := range names {
			for _, src := range []string{
				"create table " + n + " (" + n + " int primary key) strict, without rowid",
				"insert into " + n + " (" + n + ") values (1)",
				"select * from " + n,
				"drop table " + n,
			} {
				got := i.query(src)
				if got.err == "not supported" {
					continue
				}
				checked++
				is.Equal(t, o.run(t, src).String(), got.String(), src)
			}
		}
		t.Logf("%v statements of %v names", checked, len(names))
	})
}

func TestEngine_concurrent(t *testing.T) {
	t.Run("should run statements from several goroutines at once, for the race detector", func(t *testing.T) {
		i := newInstance(t, engine.Options{Logger: slog.New(&events{})})
		_, err := i.e.Exec(t.Context(), t1)
		is.NotError(t, err)
		var wg sync.WaitGroup
		for g := range 8 {
			wg.Go(func() {
				for v := range 20 {
					src := fmt.Sprint("insert into t1 (v1) values (", v%5, ")")
					if v%2 == 1 {
						src = "select * from t1"
					}
					rows, err := i.e.Query(t.Context(), src)
					var se *sqlerr.Error
					if err != nil && !errors.As(err, &se) {
						t.Errorf("goroutine %v: %v", g, err)
						return
					}
					if err == nil {
						for _, err := range rows.All() {
							if err != nil {
								t.Errorf("goroutine %v: %v", g, err)
							}
						}
					}
				}
			})
		}
		wg.Wait()
		is.Equal(t, `columns "v1 INT", rows ["0" "1" "2" "3" "4"]`, i.query("select * from t1").String())
		noneOpen(t, i)
	})
}
