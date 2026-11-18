package stmt_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sqlite"
)

// db is a new in-memory database of SQLite, for one check.
type db struct {
	t testing.TB
	c *sqlite.Conn
}

// open a new in-memory database, which is closed when the test ends.
func open(t testing.TB) *db {
	t.Helper()
	c, err := sqlite.Open(":memory:")
	is.NotError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return &db{t: t, c: c}
}

// exec the first statement of sql to its end, as SQLite reads it, and give its error.
func (d *db) exec(sql string) error {
	s, _, err := d.c.PrepareFirst(sql)
	if err != nil {
		return err
	}
	if s == nil {
		return errors.New("no statement")
	}
	for {
		ok, err := s.Step()
		if err != nil {
			_ = s.Finalize()
			return err
		}
		if !ok {
			return s.Finalize()
		}
	}
}

// must exec the first statement of sql without an error.
func (d *db) must(sql string) {
	d.t.Helper()
	if err := d.exec(sql); err != nil {
		d.t.Fatalf("%q: %v", sql, err)
	}
}

// query the rows of sql, with args bound as text, each value as its type and its text, and the names
// of the result columns.
func (d *db) query(sql string, args ...string) ([]string, [][]string) {
	d.t.Helper()
	s, _, err := d.c.PrepareFirst(sql)
	if err != nil {
		d.t.Fatalf("%q: %v", sql, err)
	}
	defer func() { _ = s.Finalize() }()
	for i, a := range args {
		is.NotError(d.t, s.BindText(i+1, a))
	}
	var names []string
	for i := range s.ColumnCount() {
		names = append(names, s.ColumnName(i))
	}
	var rows [][]string
	for {
		ok, err := s.Step()
		if err != nil {
			d.t.Fatalf("%q: %v", sql, err)
		}
		if !ok {
			return names, rows
		}
		var row []string
		for i := range s.ColumnCount() {
			row = append(row, fmt.Sprintf("%v:%v", s.ColumnType(i), s.ColumnText(i)))
		}
		rows = append(rows, row)
	}
}

// schema of the database as SQLite records it, apart from the text of each statement: every object
// of sqlite_schema, and of each table its options, its columns, and its indexes and their columns.
func (d *db) schema() string {
	d.t.Helper()
	var b strings.Builder
	_, objects := d.query("select type, name, tbl_name from sqlite_schema order by name")
	fmt.Fprintln(&b, "objects", objects)
	for _, o := range objects {
		if o[0] != "text:table" {
			continue
		}
		name := strings.TrimPrefix(o[1], "text:")
		_, list := d.query("select * from pragma_table_list where schema = 'main' and name = ?", name)
		_, columns := d.query("select * from pragma_table_xinfo(?)", name)
		_, indexes := d.query("select * from pragma_index_list(?)", name)
		fmt.Fprintln(&b, "table", list, "columns", columns, "indexes", indexes)
		for _, ix := range indexes {
			_, cols := d.query("select * from pragma_index_xinfo(?)", strings.TrimPrefix(ix[1], "text:"))
			fmt.Fprintln(&b, "index", ix[1], cols)
		}
	}
	return b.String()
}

// tables of the database: the names of the tables in sqlite_schema, in order.
func (d *db) tables() []string {
	d.t.Helper()
	_, rows := d.query("select name from sqlite_schema where type = 'table' order by name")
	var names []string
	for _, r := range rows {
		names = append(names, strings.TrimPrefix(r[0], "text:"))
	}
	return names
}

// quote a name in double quotes.
func quote(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// swapCase of the ASCII letters of s, which SQLite takes for the same name.
func swapCase(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case 'a' <= r && r <= 'z':
			return r - 'a' + 'A'
		case 'A' <= r && r <= 'Z':
			return r - 'A' + 'a'
		}
		return r
	}, s)
}

// createFor a table named table with a column named column, as slice 1 creates it.
func createFor(table, column string) string {
	return fmt.Sprintf("create table %v (%v int primary key) strict, without rowid", quote(table), quote(column))
}

// canonical text of the statement s: the plain form, with every name in double quotes.
func canonical(s stmt.Statement) string {
	switch s := s.(type) {
	case *stmt.CreateTable:
		return createFor(s.Table.Value, s.Column.Value)
	case *stmt.Insert:
		return fmt.Sprintf("insert into %v (%v) values (%v)", quote(s.Table.Value), quote(s.Column.Value), s.Value)
	case *stmt.Select:
		return "select * from " + quote(s.Table.Value)
	case *stmt.DropTable:
		return "drop table " + quote(s.Table.Value)
	}
	panic(fmt.Sprintf("%T", s))
}

// sqliteError requires that err is an error of SQLite with the result code code and the message
// message.
func sqliteError(t testing.TB, src string, err error, code sqlite.ResultCode, message string) {
	t.Helper()
	var e *sqlite.Error
	if !errors.As(err, &e) || e.Code != code || e.Message != message {
		t.Fatalf("%q: SQLite gives %v, want %v %q", src, err, code, message)
	}
}

// checkMeaning of the supported statement s that src holds: that SQLite, when it runs src, does what
// s says, that the canonical text of s does the same, and that the names of s are those that SQLite
// prints in the errors that s can cause.
func checkMeaning(t testing.TB, src string, s stmt.Statement) {
	t.Helper()
	checkPositions(t, src, s)

	// The canonical text of s is supported and gives the same statement.
	c, err := stmt.Check(canonical(s))
	if err != nil {
		t.Fatalf("%q: the canonical text %q of the statement is not supported: %v", src, canonical(s), err)
	}
	if canonical(c) != canonical(s) {
		t.Fatalf("%q: the canonical text %q gives %q", src, canonical(s), canonical(c))
	}

	switch s := s.(type) {
	case *stmt.CreateTable:
		d := open(t)
		d.must(src)
		got := d.schema()
		want := open(t)
		want.must(canonical(s))
		if got != want.schema() {
			t.Fatalf("%q: SQLite records\n%v\nbut for the canonical text\n%v", src, got, want.schema())
		}
		_, list := d.query("select * from pragma_table_list where schema = 'main' and name <> 'sqlite_schema'")
		is.Equal(t, fmt.Sprint([][]string{{"text:main", "text:" + s.Table.Value, "text:table", "integer:1", "integer:1", "integer:1"}}), fmt.Sprint(list), src)
		_, columns := d.query("select * from pragma_table_xinfo(?)", s.Table.Value)
		is.Equal(t, fmt.Sprint([][]string{{"integer:0", "text:" + s.Column.Value, "text:INT", "integer:1", "null:", "integer:1", "integer:0"}}), fmt.Sprint(columns), src)
		sqliteError(t, src, d.exec(src), 1, "table "+s.Table.Raw+" already exists")

	case *stmt.Insert:
		// The table is created with the case of its ASCII letters swapped, which SQLite takes for the
		// same names, and the error of a second insert prints the names as the table stores them.
		table, column := swapCase(s.Table.Value), swapCase(s.Column.Value)
		read := "select " + quote(column) + ", typeof(" + quote(column) + ") from " + quote(table)
		d := open(t)
		d.must(createFor(table, column))
		d.must(src)
		_, rows := d.query(read)
		is.Equal(t, fmt.Sprint([][]string{{fmt.Sprintf("integer:%v", s.Value), "text:integer"}}), fmt.Sprint(rows), src)
		sqliteError(t, src, d.exec(src), 1555, "UNIQUE constraint failed: "+table+"."+column)

		want := open(t)
		want.must(createFor(table, column))
		want.must(canonical(s))
		_, wantRows := want.query(read)
		is.Equal(t, fmt.Sprint(wantRows), fmt.Sprint(rows), src)

		sqliteError(t, src, open(t).exec(src), 1, "no such table: "+s.Table.Value)
		other := open(t)
		other.must(createFor(s.Table.Value, s.Column.Value+"_other"))
		sqliteError(t, src, other.exec(src), 1, "table "+s.Table.Value+" has no column named "+s.Column.Value)

	case *stmt.Select:
		table := swapCase(s.Table.Value)
		read := func(sql string) string {
			d := open(t)
			d.must(createFor(table, "v"))
			d.must(createFor("other_"+s.Table.Value, "v"))
			for _, v := range []int{3, 1, 2} {
				d.must(fmt.Sprintf("insert into %v (v) values (%v)", quote(table), v))
			}
			d.must(fmt.Sprintf("insert into %v (v) values (7)", quote("other_"+s.Table.Value)))
			names, rows := d.query(sql)
			return fmt.Sprint(names, rows)
		}
		got := read(src)
		is.Equal(t, fmt.Sprint([]string{"v"}, [][]string{{"integer:1"}, {"integer:2"}, {"integer:3"}}), got, src)
		is.Equal(t, read(canonical(s)), got, src)
		sqliteError(t, src, open(t).exec(src), 1, "no such table: "+s.Table.Value)

	case *stmt.DropTable:
		drop := func(sql string) []string {
			d := open(t)
			d.must(createFor(swapCase(s.Table.Value), "v"))
			d.must(createFor("other_"+s.Table.Value, "v"))
			d.must(sql)
			return d.tables()
		}
		got := drop(src)
		is.Equal(t, fmt.Sprint([]string{"other_" + s.Table.Value}), fmt.Sprint(got), src)
		is.Equal(t, fmt.Sprint(drop(canonical(s))), fmt.Sprint(got), src)
		sqliteError(t, src, open(t).exec(src), 1, "no such table: "+s.Table.Value)
	}
}

// checkPositions of the statement s in src: its span, and the raw text of its names at their
// offsets.
func checkPositions(t testing.TB, src string, s stmt.Statement) {
	t.Helper()
	b := s.Bounds()
	if b.Start < 0 || b.Start >= b.End || b.End > len(src) {
		t.Fatalf("%q: span %v", src, b)
	}
	var names []stmt.Name
	switch s := s.(type) {
	case *stmt.CreateTable:
		names = []stmt.Name{s.Table, s.Column}
	case *stmt.Insert:
		names = []stmt.Name{s.Table, s.Column}
		v := s.ValueSpan
		if v.Start < b.Start || v.End > b.End || v.Start >= v.End || !strings.HasSuffix(src[v.Start:v.End], strings.TrimPrefix(fmt.Sprint(s.Value), "-")) {
			t.Fatalf("%q: value %v at %v", src, s.Value, v)
		}
	case *stmt.Select:
		names = []stmt.Name{s.Table}
	case *stmt.DropTable:
		names = []stmt.Name{s.Table}
	}
	for _, n := range names {
		if n.Offset < b.Start || n.Offset+len(n.Raw) > b.End || src[n.Offset:n.Offset+len(n.Raw)] != n.Raw {
			t.Fatalf("%q: name %q at %v", src, n.Raw, n.Offset)
		}
	}
}

func TestName_Fold(t *testing.T) {
	t.Run("should fold the ASCII letters only, as SQLite compares names", func(t *testing.T) {
		is.Equal(t, "abc_1ÆØÅé", stmt.Name{Value: "AbC_1ÆØÅé"}.Fold())

		// SQLite takes ÆØÅ and æøå for two tables, and T1 and t1 for one.
		d := open(t)
		d.must(createFor("ÆØÅ", "v"))
		d.must(createFor("æøå", "v"))
		d.must(createFor("t1", "v"))
		sqliteError(t, "", d.exec("create table T1 (v int primary key) strict, without rowid"), 1, "table T1 already exists")
	})
}
