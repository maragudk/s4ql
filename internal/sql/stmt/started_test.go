package stmt_test

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/parsetest"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
	"maragu.dev/s4ql/internal/sql/token"
	"maragu.dev/s4ql/internal/sqlite"
)

// checkStarted checks [stmt.StartedTable] on src against SQLite, if src is a syntax error. If it
// gives a name, SQLite gives `table %s already exists` with the name as StartedTable gives it, on a
// database with that table. If it gives none, SQLite does not give that a table exists, on a database
// with a table for each of the first tokens of src as a name. It reports whether StartedTable gave a
// name.
func checkStarted(t testing.TB, src string) bool {
	t.Helper()
	_, err := stmt.Check(src)
	var se *sqlerr.Error
	if !errors.As(err, &se) {
		return false
	}
	n, ok := stmt.StartedTable(src, se)
	d := open(t)
	names := []string{n.Value}
	if !ok {
		names = tokenNames(src)
	}
	for _, name := range names {
		d.must("create table " + stmttest.Quote(name) + " (v int primary key) strict, without rowid")
	}
	var sqliteErr *sqlite.Error
	if !errors.As(d.exec(src), &sqliteErr) {
		t.Fatalf("%q: SQLite gives no error", src)
	}
	if ok && sqliteErr.Message != "table "+n.Raw+" already exists" {
		t.Fatalf("%q, with the table %q: SQLite gives %q, and the pass reads the start of the table %v", src, n.Value, sqliteErr.Message, n.Raw)
	}
	// Where the pass reads no start of a table, SQLite gives the syntax error, or an error of its
	// grammar actions, which the pass does not give, but not that a table exists.
	if !ok && strings.HasSuffix(sqliteErr.Message, " already exists") {
		t.Fatalf("%q, with the tables %q: SQLite gives %q, and the pass reads no start of a table", src, names, sqliteErr.Message)
	}
	return ok
}

// tokenNames are the texts of the first tokens of src that are not spaces or comments, each without
// its quotes, as names of tables that SQLite takes: not empty, without NUL, and not reserved.
func tokenNames(src string) []string {
	seen := map[string]bool{}
	var names []string
	for _, tok := range token.Tokens(src) {
		if len(names) == 12 {
			break
		}
		if tok.Kind == token.Space || tok.Kind == token.Comment {
			continue
		}
		name := tok.Text(src)
		if len(name) >= 2 && strings.ContainsRune("\"'`[", rune(name[0])) {
			name = name[1 : len(name)-1]
		}
		lower := strings.ToLower(name)
		if name == "" || seen[lower] || strings.ContainsRune(name, 0) || !utf8.ValidString(name) || strings.HasPrefix(lower, "sqlite_") {
			continue
		}
		seen[lower] = true
		names = append(names, name)
	}
	return names
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
		{src: "create table sqlite_x ("},
		{src: "create table main.SQLite_x (garbage"},
		{src: "create table zzz (", name: "zzz", raw: "zzz"},
		{src: "create table"},
		{src: "create table main."},
		{src: "create table main.t1"},
		{src: "create table if not exists"},
		{src: "values ("},
		{src: `create table "" (`, name: "", raw: `""`},
		{src: "select ("},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			var n stmt.Name
			var ok bool
			if _, err := stmt.Check(c.src); err != nil {
				var se *sqlerr.Error
				if errors.As(err, &se) {
					n, ok = stmt.StartedTable(c.src, se)
				}
			}
			is.Equal(t, c.raw != "", ok)
			is.Equal(t, c.name, n.Value)
			is.Equal(t, c.raw, n.Raw)
			if ok {
				is.Equal(t, c.raw, c.src[n.Offset:n.Offset+len(n.Raw)])
			}
			checkStarted(t, c.src)
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
			src, _ := g.StatementOf(stmttest.KindCreateTable)
			texts := parsetest.Mutants(src, *seed+uint64(i), 3)
			for _, cut := range []int{len(src) / 3, len(src) / 2, len(src) - 1} {
				texts = append(texts, src[:cut])
			}
			for _, m := range texts {
				if checkStarted(t, m) {
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
		checkStarted(t, src)
	})
}
