package engine_test

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// existsSetup is the database of the tests of IF NOT EXISTS and IF EXISTS: the table t, with an
// index i and rows, and the table u, without either, after an insert that leaves SQLite's count of
// changes at 1.
var existsSetup = []string{
	indexTable, "create index i on t (a)", "create table u (k int primary key) strict, without rowid",
	"insert into t values (1, 10, 'x', 1.5)", "insert into t values (2, 20, null, null)",
}

// existsCases are what SQLite gives for each statement of IF NOT EXISTS or IF EXISTS on the database
// of existsSetup, in their order, each on the database that the statements before it leave.
var existsCases = []struct{ src, want string }{
	// A table of the name, in any case: nothing, whatever the columns are.
	{"create table if not exists t (k int primary key) strict, without rowid", ""},
	{"create table if not exists T (x int primary key, x int) strict, without rowid", ""},
	{`CREATE TABLE If Not Exists "t" (k text primary key, y blob) strict, without rowid`, ""},
	// An index of the name still gives its error, and no table of the name the errors of the plain
	// statement.
	{"create table if not exists i (k int primary key) strict, without rowid", "1/1: there is already an index named i"},
	{"create table if not exists I (k int primary key, k int) strict, without rowid", "1/1: there is already an index named I"},
	{"create table if not exists w (k int primary key, k int) strict, without rowid", "1/1: duplicate column name: k"},
	{"create table if not exists w (k int primary key, v text) strict, without rowid", ""},
	{"create table if not exists W (z int primary key) strict, without rowid", ""},
	{"insert into w values (1, 'a')", ""},
	// No table of the name, also where an index has it: nothing.
	{"drop table if exists nosuch", ""},
	{"drop table if exists i", ""},
	{`drop table if exists "W"`, ""},
	{"drop table if exists w", ""},
	// An index of the name, on any table: nothing, also where the table lacks the columns. The errors
	// of the table come first.
	{"create index if not exists i on t (b desc)", ""},
	{"create index if not exists I on u (nosuch)", ""},
	{"create index if not exists i on nosuch (a)", "1/1: no such table: main.nosuch"},
	{"create index if not exists t on t (a)", "1/1: there is already a table named t"},
	{"create index if not exists U on t (nosuch)", "1/1: there is already a table named U"},
	{"create index if not exists j on t (nosuch)", "1/1: no such column: nosuch"},
	{"create index if not exists j on t (b, c desc)", ""},
	{"create index if not exists J on t (k)", ""},
	// No index of the name, also where a table has it: nothing.
	{"drop index if exists nosuch", ""},
	{"drop index if exists t", ""},
	{"drop index if exists j", ""},
	{"drop index if exists J", ""},
	// A drop table takes the indexes of its table along.
	{"drop table if exists t", ""},
	{"drop index if exists i", ""},
	{"create index if not exists i on t (a)", "1/1: no such table: main.t"},
	{"create table if not exists t (k int primary key, a int) strict, without rowid", ""},
	{"create index if not exists i on t (a)", ""},
}

func TestEngine_exists(t *testing.T) {
	t.Run("should do what the plain statement does where it would, and nothing otherwise, with SQLite's errors in SQLite's order", func(t *testing.T) {
		p := newIndexPair(t, 0, existsSetup...)

		for _, c := range existsCases {
			is.Equal(t, c.want, p.run(c.src).err, c.src)
		}
	})

	t.Run("should write nothing to the log for a statement that does nothing, of its own or in a transaction, and replay the rest", func(t *testing.T) {
		p := newIndexPair(t, 0, existsSetup...)
		commits := len(logAnnotations(t, p.i.store))

		for _, src := range []string{
			"create table if not exists t (k int primary key) strict, without rowid", "drop table if exists nosuch",
			"create index if not exists i on t (b)", "drop index if exists nosuch", "drop index if exists t", "drop table if exists i",
		} {
			is.Equal(t, "", p.run(src).err, src)
		}
		is.Equal(t, commits, len(logAnnotations(t, p.i.store)), "the commit objects after the statements that do nothing")

		// A transaction of statements that do nothing writes no commit object, and one with a write holds
		// only the annotation of that write.
		for _, commit := range []bool{true, false} {
			p.begin(false)
			is.Equal(t, "", p.run("create table if not exists u (x int primary key) strict, without rowid").err)
			is.Equal(t, "", p.run("drop index if exists nosuch").err)
			p.end(false, commit)
		}
		is.Equal(t, commits, len(logAnnotations(t, p.i.store)), "the commit objects after the transactions that do nothing")
		p.begin(false)
		is.Equal(t, "", p.run("create index if not exists i on u (k)").err)
		is.Equal(t, "", p.run("create table if not exists w (k int primary key) strict, without rowid").err)
		is.Equal(t, "", p.run("drop table if exists nosuch").err)
		is.Equal(t, "", p.run("create table if not exists w (k int primary key, v text) strict, without rowid").err)
		is.Equal(t, "", p.run("drop index if exists i").err)
		p.end(false, true)
		annotations := logAnnotations(t, p.i.store)
		is.Equal(t, commits+1, len(annotations))
		is.Equal(t, fmt.Sprint([]string{"create table if not exists w (k int primary key) strict, without rowid", "drop index if exists i"}), fmt.Sprint(annotations[len(annotations)-1]))

		// The log replayed into the empty cache of a new instance, and a restart on the same cache.
		p.i.restart(true, engine.Options{})
		p.same("a replay of the log into an empty cache")
		p.i.restart(false, engine.Options{})
		p.same("a restart on the same cache")
		is.Equal(t, commits+1, len(logAnnotations(t, p.i.store)))
	})

	t.Run("should run the statements in a write transaction, undo them with its roll back, and keep the transaction open after one that fails", func(t *testing.T) {
		p := newIndexPair(t, 0, existsSetup...)

		for _, commit := range []bool{false, true} {
			p.begin(false)
			for _, c := range existsCases {
				is.Equal(t, c.want, p.run(c.src).err, c.src)
			}
			p.end(false, commit)
		}
		p.i.restart(true, engine.Options{})
		p.same("a replay of the log into an empty cache")
	})

	t.Run("should run a statement that does nothing in a read-only transaction, and refuse one that would write with SQLite's error after the errors of its names and columns", func(t *testing.T) {
		p := newIndexPair(t, 0, existsSetup...)

		p.begin(true)
		for _, c := range []struct{ src, want string }{
			{"create table if not exists t (x int primary key, x int) strict, without rowid", ""},
			{"create table if not exists i (k int primary key) strict, without rowid", "1/1: there is already an index named i"},
			{"create table if not exists w (k int primary key, k int) strict, without rowid", "1/1: duplicate column name: k"},
			{"create table if not exists w (k int primary key) strict, without rowid", "8/8: attempt to write a readonly database"},
			{"drop table if exists nosuch", ""},
			{"drop table if exists i", ""},
			{"drop table if exists u", "8/8: attempt to write a readonly database"},
			{"create index if not exists i on u (nosuch)", ""},
			{"create index if not exists j on nosuch (a)", "1/1: no such table: main.nosuch"},
			{"create index if not exists t on t (a)", "1/1: there is already a table named t"},
			{"create index if not exists j on t (nosuch)", "1/1: no such column: nosuch"},
			{"create index if not exists j on t (b)", "8/8: attempt to write a readonly database"},
			{"drop index if exists nosuch", ""},
			{"drop index if exists t", ""},
			{"drop index if exists i", "8/8: attempt to write a readonly database"},
		} {
			is.Equal(t, c.want, p.run(c.src).err, c.src)
		}
		p.end(true, true)
	})

	t.Run("should run a statement of its own that does nothing without the place of the writer, as SQLite only reads the schema for it", func(t *testing.T) {
		p := newIndexPair(t, 0, existsSetup...)
		other := p.i.e.Conn()
		defer func() { is.NotError(t, other.Close(t.Context())) }()
		// The write transaction of another connection holds the place of the writer, so a statement that
		// waited for it would not return before the commit below.
		is.NotError(t, other.Begin(t.Context()))
		for _, src := range []string{
			"create table if not exists t (k int primary key) strict, without rowid", "drop table if exists nosuch",
			"create index if not exists i on t (b)", "drop index if exists nosuch",
		} {
			_, err := p.i.c.Exec(t.Context(), src)
			is.NotError(t, err, src)
		}
		_, err := other.Exec(t.Context(), "drop table u")
		is.NotError(t, err)
		is.NotError(t, other.Commit(t.Context()))
		is.Equal(t, "", p.o.run(t, "drop table u").err)
		p.same("the commit of the other connection")

		// With rows of a query of the connection open, which hold its reader, the statement looks in a
		// write transaction, and does nothing there.
		rows, err := p.i.c.Query(t.Context(), "select * from t")
		is.NotError(t, err)
		commits := len(logAnnotations(t, p.i.store))
		for _, src := range []string{"create table if not exists t (k int primary key) strict, without rowid", "drop index if exists nosuch"} {
			_, err := p.i.c.Exec(t.Context(), src)
			is.NotError(t, err, src)
		}
		is.NotError(t, rows.Close())
		is.Equal(t, commits, len(logAnnotations(t, p.i.store)))
		p.same("the statements beside the rows")
	})

	t.Run("should refuse a statement that would do nothing while rows of its transaction are open, as every statement in a transaction", func(t *testing.T) {
		p := newIndexPair(t, 0, existsSetup...)
		for _, readOnly := range []bool{false, true} {
			if readOnly {
				is.NotError(t, p.i.c.BeginReadOnly(t.Context()))
			} else {
				is.NotError(t, p.i.c.Begin(t.Context()))
			}
			rows, err := p.i.c.Query(t.Context(), "select * from t")
			is.NotError(t, err)
			for _, src := range []string{
				"create table if not exists t (k int primary key) strict, without rowid", "drop table if exists nosuch",
				"create index if not exists i on t (a)", "drop index if exists nosuch",
			} {
				_, err := p.i.c.Exec(t.Context(), src)
				is.Error(t, engine.ErrRowsOpen, err, src)
			}
			is.NotError(t, rows.Close())
			_, err = p.i.c.Exec(t.Context(), "drop table if exists nosuch")
			is.NotError(t, err)
			is.NotError(t, p.i.c.Commit(t.Context()))
		}
		p.same("the transactions")
	})

	t.Run("should do nothing where SQLite does, before the length limit, and refuse otherwise as the plain statement", func(t *testing.T) {
		const limit = 1000
		long := strings.Repeat("y", 600)
		p := newIndexPair(t, limit, existsSetup...)

		for _, c := range []struct{ src, want string }{
			{"create table if not exists t (k int primary key, " + long + " int, " + long + "x int) strict, without rowid", ""},
			{"create table if not exists t (k int primary key) strict, without rowid -- " + strings.Repeat("z", 1200), ""},
			{"create table if not exists " + long + " (k int primary key) strict, without rowid", "18/18: string or blob too big"},
			{"create table if not exists v (k int primary key, " + long + " int, " + long + "x int) strict, without rowid", "18/18: string or blob too big"},
			{"create index if not exists i on t (a) -- " + strings.Repeat("z", 1200), ""},
			{"create index if not exists i on t (" + long + ")", ""},
			{"create index if not exists " + long + " on t (a)", "18/18: string or blob too big"},
			{"drop table if exists " + long, ""},
			{"drop index if exists " + long, ""},
		} {
			is.Equal(t, c.want, p.run(c.src).err, c.src)
		}
	})

	t.Run("should do nothing for an index of more than 250 columns that exists, as SQLite does, and refuse one that does not as not supported", func(t *testing.T) {
		columns := make([]string, 251)
		for i := range columns {
			columns[i] = fmt.Sprint("c", i)
		}
		wide := "create table w (" + strings.Join(columns, " int, ") + " int, k int primary key) strict, without rowid"
		p := newIndexPair(t, 0, wide, "create index taken on w (c0)")
		list := strings.Join(columns, ", ")

		is.Equal(t, "", p.run("create index if not exists taken on w ("+list+")").err)
		is.Equal(t, "", p.run("create index if not exists TAKEN on w ("+list+", nosuch)").err)
		src := "create index if not exists j on w (" + list + ")"
		_, err := p.i.c.Exec(t.Context(), src)
		is.Error(t, stmt.ErrNotSupported, err)
		ns, ok := errors.AsType[*stmt.NotSupportedError](err)
		is.True(t, ok)
		is.Equal(t, "index of more than 250 columns", ns.What)
		is.Equal(t, strings.Index(src, "c250"), ns.Offset)
		is.Equal(t, "", p.o.run(t, src).err, "SQLite takes it")
	})

	t.Run("should give the offset of SQLite's errors of the statements", func(t *testing.T) {
		for _, src := range []string{
			"create table if not exists i (k int primary key) strict, without rowid",
			"create table if not exists w (k int primary key, k int) strict, without rowid",
			"create index if not exists j on nosuch (a)", "create index if not exists t on t (a)", "create index if not exists j on t (a,  nosuch)",
		} {
			// A fresh connection to SQLite, since SQLite does not give the same offset on every one.
			p := newIndexPair(t, 0, existsSetup...)
			want := p.o.run(t, src)
			got := p.i.exec(src)
			is.Equal(t, want.String(), got.String(), src)
			is.Equal(t, want.offset, got.offset, src)
		}
	})

	t.Run("should say in the event of a statement of the clauses whether it did nothing", func(t *testing.T) {
		h := &events{}
		i := newInstance(t, engine.Options{Logger: slog.New(h)})
		for _, src := range existsSetup {
			is.Equal(t, "", i.exec(src).err, src)
		}
		event := func(src string) string {
			_ = i.exec(src)
			e, _ := h.last(t)
			nothing, ok := e[engine.AttrNothing]
			if !ok {
				nothing = "none"
			}
			return fmt.Sprint(e[engine.AttrKind], " ", nothing, " ", e[engine.AttrOutcome])
		}

		for _, c := range []struct{ src, want string }{
			{"create table if not exists t (k int primary key) strict, without rowid", "create_table true ok"},
			{"create table if not exists w (k int primary key) strict, without rowid", "create_table false ok"},
			{"create table if not exists i (k int primary key) strict, without rowid", "create_table none sqlite_error"},
			{"create table v (k int primary key) strict, without rowid", "create_table none ok"},
			{"drop table if exists nosuch", "drop_table true ok"},
			{"drop table if exists w", "drop_table false ok"},
			{"create index if not exists i on t (b)", "create_index true ok"},
			{"create index if not exists j on t (b)", "create_index false ok"},
			{"create index if not exists j on nosuch (b)", "create_index none sqlite_error"},
			{"drop index if exists nosuch", "drop_index true ok"},
			{"drop index if exists j", "drop_index false ok"},
			{"drop index i", "drop_index none ok"},
		} {
			is.Equal(t, c.want, event(c.src), c.src)
		}

		// A statement that would write in a read-only transaction went on to do its work.
		is.NotError(t, i.c.BeginReadOnly(t.Context()))
		is.Equal(t, "drop_table false sqlite_error", event("drop table if exists u"))
		is.Equal(t, "drop_table true ok", event("drop table if exists nosuch"))
		is.NotError(t, i.c.Rollback(t.Context()))
	})
}
