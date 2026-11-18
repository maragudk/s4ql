package stmt_test

import (
	"errors"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/parsetest"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
	"maragu.dev/s4ql/internal/sqlite"
)

// checkStarted checks [stmt.StartedTable] on src against SQLite, if src is a syntax error: on a
// database with the table that it names, or with the table of the name fallback if it names none,
// SQLite gives `table %s already exists` with the name as StartedTable gives it if it gives one, and
// otherwise the syntax error of [stmt.Check]. It reports whether StartedTable gave a name.
func checkStarted(t testing.TB, src, fallback string) bool {
	t.Helper()
	_, err := stmt.Check(src)
	var se *sqlerr.Error
	if !errors.As(err, &se) {
		_, ok := stmt.StartedTable(src)
		is.True(t, !ok, src)
		return false
	}
	n, ok := stmt.StartedTable(src)
	d := open(t)
	name := fallback
	if ok {
		name = n.Value
	}
	d.must("create table " + stmttest.Quote(name) + " (v int primary key) strict, without rowid")
	var sqliteErr *sqlite.Error
	if !errors.As(d.exec(src), &sqliteErr) {
		t.Fatalf("%q: SQLite gives no error", src)
	}
	want := se.Message
	if ok {
		want = "table " + n.Raw + " already exists"
	}
	// Where the pass reads no start of a table, SQLite gives the syntax error, or an error of its
	// grammar actions, which the pass does not give, but not that the table exists.
	if ok && sqliteErr.Message != want || !ok && sqliteErr.Message != want && strings.HasSuffix(sqliteErr.Message, "already exists") {
		t.Fatalf("%q, with the table %q: SQLite gives %q, and the pass reads %q", src, name, sqliteErr.Message, want)
	}
	return ok
}

func TestStartedTable(t *testing.T) {
	cases := []struct {
		src  string
		name string
		raw  string
	}{
		{src: "create table t1 (", name: "t1", raw: "t1"},
		{src: "create table T1 (v int primary key) strict without rowid", name: "T1", raw: "T1"},
		{src: "create table t1 (v 1_)", name: "t1", raw: "t1"},
		{src: "explain create table t1 (", name: "t1", raw: "t1"},
		{src: "explain query plan create table t1 (", name: "t1", raw: "t1"},
		{src: ";; /* x */ create table t1 (", name: "t1", raw: "t1"},
		{src: "create table 't''1' (", name: "t'1", raw: "'t''1'"},
		{src: "create table [t1] (", name: "t1", raw: "[t1]"},
		{src: "create table `T``1` (", name: "T`1", raw: "`T``1`"},
		{src: `create table "a b" as selec`, name: "a b", raw: `"a b"`},
		{src: "create table main.t1 (", name: "t1", raw: "t1"},
		{src: `create table "MAIN".t1 (`, name: "t1", raw: "t1"},
		{src: "create table abort (", name: "abort", raw: "abort"},
		{src: "create table if ("},
		{src: `create table "if" (`, name: "if", raw: `"if"`},
		{src: "create virtual table t1 using fts5(", name: "t1", raw: "t1"},
		{src: "create table t1 (v int primary key) strict, without rowid x", name: "t1", raw: "t1"},
		{src: "create table t1 x"},
		{src: "create table t1"},
		{src: "create table t1 ;"},
		{src: "create table temp.t1 ("},
		{src: "create temp table t1 ("},
		{src: "create table if not exists t1 ("},
		{src: "create table zz.t1 ("},
		{src: "create table t1 #1 ("},
		{src: "create table select ("},
		{src: "create virtual table t1 using fts5 x"},
		{src: "create virtual table t1 x"},
		{src: "create view t1 as selec"},
		{src: "create table t1 (v int primary key) strict, without rowid"},
		{src: "selec * from t1"},
		{src: ""},
		{src: `create table "t1 (`},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			n, ok := stmt.StartedTable(c.src)
			is.Equal(t, c.name != "", ok)
			is.Equal(t, c.name, n.Value)
			is.Equal(t, c.raw, n.Raw)
			checkStarted(t, c.src, "t1")
		})
	}

	t.Run("should agree with SQLite on mutants and prefixes of statements of the generator", func(t *testing.T) {
		n := *generatedCount / 10
		if slow() {
			n /= 10
		}
		g := newGenerator(t, *seed+4)
		started := 0
		for i := range n {
			src, s := g.StatementOf(stmttest.KindCreateTable)
			fallback := s.(*stmt.CreateTable).Table.Value
			texts := parsetest.Mutants(src, *seed+uint64(i), 3)
			for _, cut := range []int{len(src) / 3, len(src) / 2, len(src) - 1} {
				texts = append(texts, src[:cut])
			}
			for _, m := range texts {
				if checkStarted(t, m, fallback) {
					started++
				}
			}
		}
		t.Logf("%v of %v texts start a table before their syntax error", started, n*6)
		is.True(t, started > n/2, "too few texts start a table")
	})
}

// FuzzStartedTable checks [stmt.StartedTable] against SQLite on any text.
func FuzzStartedTable(f *testing.F) {
	for _, c := range []string{"create table t1 (", "create table main.t1 as", "create virtual table t1 using m(", "create table t1 x"} {
		f.Add(c)
	}
	f.Fuzz(func(t *testing.T, src string) {
		checkStarted(t, src, "t1")
	})
}
