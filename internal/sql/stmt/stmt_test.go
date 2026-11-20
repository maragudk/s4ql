package stmt_test

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
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
	d := openUnmanaged(t)
	t.Cleanup(func() { _ = d.c.Close() })
	return d
}

// openUnmanaged opens a new in-memory database, which the caller closes.
func openUnmanaged(t testing.TB) *db {
	t.Helper()
	c, err := sqlite.Open(":memory:")
	is.NotError(t, err)
	return &db{t: t, c: c}
}

// exec the first statement of sql to its end, as SQLite reads it, with args bound to its parameters
// by position, and give its error.
func (d *db) exec(sql string, args ...int64) error {
	s, _, err := d.c.PrepareFirst(sql)
	if err != nil {
		return err
	}
	if s == nil {
		return errors.New("no statement")
	}
	for i, a := range args {
		if err := s.BindInt64(i+1, a); err != nil {
			_ = s.Finalize()
			return err
		}
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

// must exec the first statement of sql, with args, without an error.
func (d *db) must(sql string, args ...int64) {
	d.t.Helper()
	if err := d.exec(sql, args...); err != nil {
		d.t.Fatalf("%q: %v", sql, err)
	}
}

// parameters of the first statement of sql, as SQLite counts them when it prepares the statement on
// this database: the largest index that sqlite3_bind_int64 takes, which refuses the next one with
// SQLITE_RANGE. sqlite3_bind_parameter_count gives the same, and the binding has no call for it.
func (d *db) parameters(sql string) int {
	d.t.Helper()
	s, _, err := d.c.PrepareFirst(sql)
	if err != nil {
		d.t.Fatalf("%q: %v", sql, err)
	}
	defer func() { _ = s.Finalize() }()
	for i := 1; ; i++ {
		err := s.BindInt64(i, 0)
		if err == nil {
			continue
		}
		var e *sqlite.Error
		if !errors.As(err, &e) || e.Code != 25 {
			d.t.Fatalf("%q: bind of parameter %v: %v", sql, i, err)
		}
		return i - 1
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
	return createTyped(table, column, stmt.TypeInt)
}

// createTyped a table named table with a column named column of the type typ.
func createTyped(table, column string, typ stmt.Type) string {
	return createColumns(table, []stmt.Column{{Name: stmt.Name{Value: column}, Type: typ, PrimaryKey: true}})
}

// createColumns of a table named table with the columns, by the values of their names, in the
// canonical form: every name in double quotes, and NOT NULL after PRIMARY KEY.
func createColumns(table string, columns []stmt.Column) string {
	var defs []string
	for _, c := range columns {
		def := quote(c.Name.Value) + " " + c.Type.String()
		if c.PrimaryKey {
			def += " primary key"
		}
		if c.NotNull {
			def += " not null"
		}
		defs = append(defs, def)
	}
	return fmt.Sprintf("create table %v (%v) strict, without rowid", quote(table), strings.Join(defs, ", "))
}

// quoteNames of names, each by its value in double quotes, joined by commas.
func quoteNames(names []stmt.Name) string {
	var quoted []string
	for _, n := range names {
		quoted = append(quoted, quote(n.Value))
	}
	return strings.Join(quoted, ", ")
}

// typeFor the value v: the type of a column that holds it as it is. A parameter, nil, is bound to an
// integer, and a column of every type holds a NULL.
func typeFor(v stmt.Value) stmt.Type {
	switch v.(type) {
	case stmt.Real:
		return stmt.TypeReal
	case stmt.Text:
		return stmt.TypeText
	case stmt.Blob:
		return stmt.TypeBlob
	}
	return stmt.TypeInt
}

// literal of the value v, in a canonical form: an integer in decimal, a real with an exponent and the
// fewest digits that give it back when they are rounded to the nearest float, a string in single
// quotes, and a blob in hex.
func literal(v stmt.Value) string {
	switch v := v.(type) {
	case stmt.Integer:
		return fmt.Sprint(int64(v))
	case stmt.Real:
		return strconv.FormatFloat(float64(v), 'e', -1, 64)
	case stmt.Text:
		return "'" + strings.ReplaceAll(string(v), "'", "''") + "'"
	case stmt.Blob:
		return "x'" + hex.EncodeToString(v) + "'"
	case stmt.Null:
		return "NULL"
	}
	panic(fmt.Sprintf("%T", v))
}

// show the value v with its type, exactly: a real by its 64 bits, so that two values show the same
// only if they are the same value of the same type.
func show(v stmt.Value) string {
	switch v := v.(type) {
	case stmt.Integer:
		return fmt.Sprintf("integer %v", int64(v))
	case stmt.Real:
		return fmt.Sprintf("real %v (%#016x)", float64(v), math.Float64bits(float64(v)))
	case stmt.Text:
		return fmt.Sprintf("text %q", string(v))
	case stmt.Blob:
		if v == nil {
			return "nil blob"
		}
		return fmt.Sprintf("blob %x", []byte(v))
	case stmt.Null:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

// value of the first column of the first row of sql, as the type that SQLite gives it, NULL as
// [stmt.Null].
func (d *db) value(sql string) stmt.Value {
	d.t.Helper()
	return d.valueAt(sql, 0)
}

// valueAt gives the value of the column i of the first row of sql, as [db.value] does.
func (d *db) valueAt(sql string, i int) stmt.Value {
	d.t.Helper()
	s, _, err := d.c.PrepareFirst(sql)
	if err != nil {
		d.t.Fatalf("%q: %v", sql, err)
	}
	defer func() { _ = s.Finalize() }()
	ok, err := s.Step()
	if err != nil || !ok {
		d.t.Fatalf("%q: no row: %v", sql, err)
	}
	switch s.ColumnType(i) {
	case sqlite.TypeInteger:
		return stmt.Integer(s.ColumnInt64(i))
	case sqlite.TypeFloat:
		return stmt.Real(s.ColumnDouble(i))
	case sqlite.TypeText:
		return stmt.Text(s.ColumnText(i))
	case sqlite.TypeBlob:
		return stmt.Blob(s.ColumnBlob(i))
	case sqlite.TypeNull:
		return stmt.Null{}
	}
	d.t.Fatalf("%q: a value of the type %v", sql, s.ColumnType(i))
	return nil
}

// canonical text of the statement s: the plain form, with every name in double quotes.
func canonical(s stmt.Statement) string {
	switch s := s.(type) {
	case *stmt.CreateTable:
		return createColumns(s.Table.Value, s.Columns)
	case *stmt.Insert:
		var values []string
		for _, v := range s.Values {
			if v.Parameter > 0 {
				values = append(values, "?")
			} else {
				values = append(values, literal(v.Value))
			}
		}
		list := ""
		if s.Columns != nil {
			list = " (" + quoteNames(s.Columns) + ")"
		}
		return fmt.Sprintf("insert into %v%v values (%v)", quote(s.Table.Value), list, strings.Join(values, ", "))
	case *stmt.Select:
		if s.Columns == nil {
			return "select * from " + quote(s.Table.Value)
		}
		return fmt.Sprintf("select %v from %v", quoteNames(s.Columns), quote(s.Table.Value))
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
	// The databases of one check are closed when it ends, since a test checks many statements.
	var opened []*db
	defer func() {
		for _, d := range opened {
			_ = d.c.Close()
		}
	}()
	open := func(t testing.TB) *db {
		d := openUnmanaged(t)
		opened = append(opened, d)
		return d
	}
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
		checkCreateTable(t, open, src, s)
	case *stmt.Insert:
		checkInsert(t, open, src, s)
	case *stmt.Select:
		checkSelect(t, open, src, s)
	case *stmt.DropTable:
		drop := func(sql string) []string {
			d := open(t)
			d.must(createFor(swapCase(s.Table.Value), "v"))
			d.must(createFor("other_"+s.Table.Value, "v"))
			is.Equal(t, d.parameters(sql), s.Parameters(), sql)
			d.must(sql)
			return d.tables()
		}
		got := drop(src)
		is.Equal(t, fmt.Sprint([]string{"other_" + s.Table.Value}), fmt.Sprint(got), src)
		is.Equal(t, fmt.Sprint(drop(canonical(s))), fmt.Sprint(got), src)
		sqliteError(t, src, open(t).exec(src), 1, "no such table: "+s.Table.Value)
	}
}

// checkCreateTable is [checkMeaning] of a CREATE TABLE: the schema that SQLite records for src is
// that of the canonical text, with the names, the types, the primary key, and the options of s; a
// second run gives `table <Raw> already exists`. For a statement with an error of its columns, SQLite
// gives that error on a database without the table, for src and for the canonical text, and `table
// <Raw> already exists` on a database with it.
func checkCreateTable(t testing.TB, open func(testing.TB) *db, src string, s *stmt.CreateTable) {
	t.Helper()
	is.Equal(t, 0, s.Parameters(), src)
	if e := s.ColumnsError(); e != nil {
		is.Equal(t, sqlerr.CodeError, e.Code, src)
		is.Equal(t, sqlerr.CodeError, e.ExtendedCode, src)
		sqliteError(t, src, open(t).exec(src), 1, e.Message)
		sqliteError(t, src, open(t).exec(canonical(s)), 1, e.Message)
		d := open(t)
		d.must(createFor(swapCase(s.Table.Value), "v"))
		sqliteError(t, src, d.exec(src), 1, "table "+s.Table.Raw+" already exists")
		return
	}
	d := open(t)
	is.Equal(t, d.parameters(src), s.Parameters(), src)
	d.must(src)
	got := d.schema()
	want := open(t)
	want.must(canonical(s))
	if got != want.schema() {
		t.Fatalf("%q: SQLite records\n%v\nbut for the canonical text\n%v", src, got, want.schema())
	}
	_, list := d.query("select * from pragma_table_list where schema = 'main' and name <> 'sqlite_schema'")
	is.Equal(t, fmt.Sprint([][]string{{"text:main", "text:" + s.Table.Value, "text:table", fmt.Sprint("integer:", len(s.Columns)), "integer:1", "integer:1"}}), fmt.Sprint(list), src)
	// A column is NOT NULL if it says so, and the primary key of a WITHOUT ROWID table is whether it
	// says so or not.
	var columns [][]string
	for i, c := range s.Columns {
		pk, notNull := "integer:0", "integer:0"
		if c.PrimaryKey {
			pk = "integer:1"
		}
		if c.PrimaryKey || c.NotNull {
			notNull = "integer:1"
		}
		columns = append(columns, []string{fmt.Sprint("integer:", i), "text:" + c.Name.Value, "text:" + c.Type.String(), notNull, "null:", pk, "integer:0"})
	}
	_, got2 := d.query("select * from pragma_table_xinfo(?)", s.Table.Value)
	is.Equal(t, fmt.Sprint(columns), fmt.Sprint(got2), src)
	sqliteError(t, src, d.exec(src), 1, "table "+s.Table.Raw+" already exists")
}

// checkInsert is [checkMeaning] of an INSERT: the value of each literal is the one that SQLite gives
// its text; on a table of the columns that src names, created with the case of the names swapped and
// in another order, src stores its values in the columns that it names, as the canonical text does; a
// second run gives `UNIQUE constraint failed` with the names as the table stores them; with a NULL,
// a table whose columns say NOT NULL gives `NOT NULL constraint failed`, as [checkNotNull] says; and
// SQLite gives `no such table`, `has no column named`, a wrong number of values, or `values for` with
// the names and the numbers of s. When every value is NULL, no column can be the primary key of a
// table that holds them, and only the errors are checked.
func checkInsert(t testing.TB, open func(testing.TB) *db, src string, s *stmt.Insert) {
	t.Helper()
	// The value of a literal is the value that SQLite gives the text of the literal, exactly.
	for _, v := range s.Values {
		if v.Parameter == 0 {
			lit := src[v.Span.Start:v.Span.End]
			if got, want := show(open(t).value("select "+lit)), show(v.Value); got != want {
				t.Fatalf("%q: the value is %v, SQLite gives %v for %q", src, want, got, lit)
			}
		}
	}
	table := swapCase(s.Table.Value)
	sqliteError(t, src, open(t).exec(src, parameterArgs(src, s)...), 1, "no such table: "+s.Table.Value)
	n := len(s.Values)

	// The columns of the table, by the values that they get, in the order of the values, with the
	// case of their names swapped. Without a list of columns, they are named c1, c2, and so on.
	var names []string
	if s.Columns == nil {
		for i := range n {
			names = append(names, fmt.Sprint("c", i+1))
		}
	} else {
		for _, c := range s.Columns {
			names = append(names, swapCase(c.Value))
		}
	}
	if s.Columns != nil && len(s.Columns) != n {
		// SQLite counts the values against the list once it has found the table and its columns.
		d := open(t)
		d.must(createColumns(table, intColumns(names, 0)))
		sqliteError(t, src, d.exec(src, parameterArgs(src, s)...), 1, fmt.Sprintf("%v values for %v columns", n, len(s.Columns)))
		return
	}

	// The values, with a parameter bound to an integer, and the columns of the table, of the type of
	// their value, with the primary key on the first value that is not NULL. A column of a NULL is of
	// any type, and takes NULL; every other column says NOT NULL.
	values, args := s.Values, parameterArgs(src, s)
	stored := make([]stmt.Value, n)
	columns := make([]stmt.Column, n)
	key := -1
	a := 0
	for i, v := range values {
		stored[i] = v.Value
		if v.Parameter > 0 {
			stored[i] = stmt.Integer(args[a])
			a++
		}
		_, null := stored[i].(stmt.Null)
		columns[i] = stmt.Column{Name: stmt.Name{Value: names[i]}, Type: typeFor(v.Value), NotNull: !null}
		if null {
			columns[i].Type = stmttest.Types[(len(src)+i)%len(stmttest.Types)]
		} else if key < 0 {
			key = i
			columns[i].PrimaryKey, columns[i].NotNull = true, false
		}
		// SQLite stores a real of a REAL column that has an integer value as an integer, which it
		// reads back as a real, so -0.0 comes back as 0.0: the stored value is equal as a number.
		if r, ok := stored[i].(stmt.Real); ok && r == 0 {
			stored[i] = stmt.Real(0)
		}
	}
	// With a list, the table has its columns in the reverse order, so that the values go by name.
	tableColumns := columns
	if s.Columns != nil {
		tableColumns = slices.Clone(columns)
		slices.Reverse(tableColumns)
	}
	nulls := map[string]bool{}
	for i, v := range stored {
		if _, ok := v.(stmt.Null); ok {
			nulls[names[i]] = true
		}
	}
	checkNotNull(t, open, src, table, tableColumns, nulls, args)
	if key < 0 {
		// Every value is NULL, and no column of the table can be its primary key.
		return
	}
	read := "select " + strings.Join(quoted(names), ", ") + " from " + quote(table)
	run := func(sql string) *db {
		d := open(t)
		d.must(createColumns(table, tableColumns))
		is.Equal(t, d.parameters(sql), s.Parameters(), sql)
		d.must(sql, args...)
		return d
	}
	d := run(src)
	_, rows := d.query(read)
	want := make([]string, n)
	for i := range n {
		want[i] = show(stored[i])
	}
	got := make([]string, n)
	for i := range n {
		got[i] = show(d.valueAt(read, i))
	}
	is.Equal(t, fmt.Sprint(want), fmt.Sprint(got), src)
	is.Equal(t, 1, len(rows), src)
	sqliteError(t, src, d.exec(src, args...), 1555, "UNIQUE constraint failed: "+table+"."+names[key])
	c := run(canonical(s))
	for i := range n {
		is.Equal(t, got[i], show(c.valueAt(read, i)), src)
	}

	// SQLite prints the name of the table as the statement writes it, and the table has it with the
	// case swapped.
	other := open(t)
	if s.Columns == nil {
		// One column more than the values.
		other.must(createColumns(table, intColumns(append(slices.Clone(names), "s4ql extra"), 0)))
		sqliteError(t, src, other.exec(src, args...), 1, fmt.Sprintf("table %v has %v columns but %v values were supplied", s.Table.Value, n+1, n))
		return
	}
	// A table without the first column of the list: SQLite names the first column that it does not
	// find, in the order of the list.
	other.must(createColumns(table, intColumns(append([]string{"s4ql other"}, names[1:]...), 0)))
	sqliteError(t, src, other.exec(src, args...), 1, "table "+s.Table.Value+" has no column named "+s.Columns[0].Value)
}

// checkNotNull of the insert src, with args, whose values give NULL to the columns of nulls, by the
// values of their names: on a table of the columns, in their order, where each column says NOT NULL
// or is the primary key, the first column if none of them is, SQLite refuses src with `NOT NULL
// constraint failed` for the first column in the order of the table that gets a NULL, with the names
// as the table stores them.
func checkNotNull(t testing.TB, open func(testing.TB) *db, src, table string, columns []stmt.Column, nulls map[string]bool, args []int64) {
	t.Helper()
	columns = slices.Clone(columns)
	if !slices.ContainsFunc(columns, func(c stmt.Column) bool { return c.PrimaryKey }) {
		columns[0].PrimaryKey = true
	}
	first := ""
	for i, c := range columns {
		columns[i].NotNull = !c.PrimaryKey
		if first == "" && nulls[c.Name.Value] {
			first = c.Name.Value
		}
	}
	if first == "" {
		return
	}
	d := open(t)
	d.must(createColumns(table, columns))
	sqliteError(t, src, d.exec(src, args...), 1299, "NOT NULL constraint failed: "+table+"."+first)
}

// checkSelect is [checkMeaning] of a SELECT: on a table created with the case of its names swapped,
// beside another table, src reads the rows of the named table in key order, with the columns that it
// names, by the names that the table stores, or every column in the order of the table for *, as the
// canonical text does; SQLite gives `no such table` for it without the table, and `no such column`
// for the first name of the list on a table without it.
func checkSelect(t testing.TB, open func(testing.TB) *db, src string, s *stmt.Select) {
	t.Helper()
	table := swapCase(s.Table.Value)
	// The columns of the table: those that s names, once each, as SQLite compares names, with the case
	// of their names swapped, and one more, after them; or for *, three, the key second.
	var names []string
	seen := map[string]int{}
	for _, c := range s.Columns {
		if _, ok := seen[c.Fold()]; !ok {
			seen[c.Fold()] = len(names)
			names = append(names, swapCase(c.Value))
		}
	}
	key := 0
	if s.Columns == nil {
		names = []string{"w", "v"}
		key = 1
	}
	names = append(names, "s4ql extra")
	columns := intColumns(names, key)
	// Three rows, with the keys 3, 1, and 2 inserted in that order, and the value 10 times the key
	// plus its position in every other column.
	valueOf := func(k, column int) int {
		if column == key {
			return k
		}
		return 10*k + column
	}
	var want [][]string
	for _, k := range []int{1, 2, 3} {
		var row []string
		if s.Columns == nil {
			for i := range columns {
				row = append(row, fmt.Sprint("integer:", valueOf(k, i)))
			}
		}
		for _, c := range s.Columns {
			row = append(row, fmt.Sprint("integer:", valueOf(k, seen[c.Fold()])))
		}
		want = append(want, row)
	}
	wantNames := names
	if s.Columns != nil {
		wantNames = nil
		for _, c := range s.Columns {
			wantNames = append(wantNames, names[seen[c.Fold()]])
		}
	}
	read := func(sql string) string {
		d := open(t)
		d.must(createColumns(table, columns))
		d.must(createFor("other_"+s.Table.Value, "v"))
		for _, k := range []int{3, 1, 2} {
			var values []string
			for i := range columns {
				values = append(values, fmt.Sprint(valueOf(k, i)))
			}
			d.must(fmt.Sprintf("insert into %v values (%v)", quote(table), strings.Join(values, ", ")))
		}
		d.must(fmt.Sprintf("insert into %v (v) values (7)", quote("other_"+s.Table.Value)))
		is.Equal(t, d.parameters(sql), s.Parameters(), sql)
		names, rows := d.query(sql)
		return fmt.Sprint(names, rows)
	}
	got := read(src)
	is.Equal(t, fmt.Sprint(wantNames, want), got, src)
	is.Equal(t, read(canonical(s)), got, src)
	sqliteError(t, src, open(t).exec(src), 1, "no such table: "+s.Table.Value)
	if s.Columns != nil {
		// For a name in double quotes, SQLite asks whether it should be a string, since its build reads
		// double quotes as names only.
		want := "no such column: " + s.Columns[0].Value
		if s.Columns[0].Raw[0] == '"' {
			want = `no such column: "` + s.Columns[0].Value + `" - should this be a string literal in single-quotes?`
		}
		d := open(t)
		d.must(createColumns(s.Table.Value, intColumns([]string{"s4ql extra"}, 0)))
		sqliteError(t, src, d.exec(src), 1, want)
	}
}

// intColumns of a table: an INT column for each of names, with the primary key at key, and NOT NULL.
func intColumns(names []string, key int) []stmt.Column {
	var columns []stmt.Column
	for i, n := range names {
		columns = append(columns, stmt.Column{Name: stmt.Name{Value: n}, Type: stmt.TypeInt, PrimaryKey: i == key, NotNull: i != key})
	}
	return columns
}

// quoted names, each in double quotes.
func quoted(names []string) []string {
	var q []string
	for _, n := range names {
		q = append(q, quote(n))
	}
	return q
}

// parameterArgs binds each parameter of the insert s of src to an integer at or near the limits of
// 64 bits, which the length of the text and the number of the parameter pick, and the arguments
// differ from each other, so that a value that goes to the wrong column is seen.
func parameterArgs(src string, s *stmt.Insert) []int64 {
	limits := []int64{math.MinInt64, -1, 0, 1, math.MaxInt64}
	var args []int64
	for i := range s.Parameters() {
		v := limits[(len(src)+i)%5]
		if i >= 5 {
			v = int64(i) * 1000
		}
		args = append(args, v)
	}
	return args
}

// checkPositions of the statement s in src: its span, against the tail that SQLite gives, and the raw
// text of its names at their offsets, and the span of each value of an insert.
func checkPositions(t testing.TB, src string, s stmt.Statement) {
	t.Helper()
	b := s.Bounds()
	if b.Start < 0 || b.Start >= b.End || b.End > len(src) || !onlySpace(src[:b.Start]) || !onlySpace(src[b.End:]) {
		t.Fatalf("%q: span %v", src, b)
	}
	// SQLite's tail is after the statement's semicolon, or at the end of the text if it has none. A
	// statement with an error of its columns stops SQLite's parse before its end.
	if ct, ok := s.(*stmt.CreateTable); !ok || ct.ColumnsError() == nil {
		tail := oracle.Judge(src).Tail
		if src[b.End-1] == ';' && tail != b.End || src[b.End-1] != ';' && tail != len(src) {
			t.Fatalf("%q: span %v, SQLite's tail %v", src, b, tail)
		}
	}
	var names []stmt.Name
	switch s := s.(type) {
	case *stmt.CreateTable:
		names = []stmt.Name{s.Table}
		if len(s.Columns) == 0 {
			t.Fatalf("%q: no column", src)
		}
		keys := 0
		for _, c := range s.Columns {
			names = append(names, c.Name)
			if c.PrimaryKey {
				keys++
			}
		}
		if keys != 1 {
			t.Fatalf("%q: %v primary keys", src, keys)
		}
	case *stmt.Insert:
		names = append([]stmt.Name{s.Table}, s.Columns...)
		if s.Columns != nil && len(s.Columns) == 0 || len(s.Values) == 0 {
			t.Fatalf("%q: %v columns and %v values", src, len(s.Columns), len(s.Values))
		}
		seen := map[string]bool{}
		for _, c := range s.Columns {
			if seen[c.Fold()] {
				t.Fatalf("%q: the column %q twice", src, c.Raw)
			}
			seen[c.Fold()] = true
		}
		parameters := 0
		end := b.Start
		for _, v := range s.Values {
			span := v.Span
			if span.Start < end || span.End > b.End || span.Start >= span.End {
				t.Fatalf("%q: value %v at %v", src, v.Value, span)
			}
			end = span.End
			// The value of a literal at its span is checked against SQLite in checkMeaning.
			if v.Parameter > 0 {
				parameters++
				if v.Parameter != parameters || v.Value != nil || src[span.Start:span.End] != "?" {
					t.Fatalf("%q: parameter %v with the value %v at %v", src, v.Parameter, v.Value, span)
				}
			}
			if v.Parameter == 0 && v.Value == nil {
				t.Fatalf("%q: no value and no parameter", src)
			}
		}
	case *stmt.Select:
		names = append([]stmt.Name{s.Table}, s.Columns...)
		if s.Columns != nil && len(s.Columns) == 0 {
			t.Fatalf("%q: an empty list of columns", src)
		}
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
		is.Equal(t, "abc_1ÆØÅé@[z`{", stmt.Name{Value: "AbC_1ÆØÅé@[Z`{"}.Fold())
		is.Equal(t, "\xff\xfe", stmt.Name{Value: "\xff\xfe"}.Fold())

		// SQLite takes ÆØÅ and æøå for two tables, and T1 and t1 for one.
		d := open(t)
		d.must(createFor("ÆØÅ", "v"))
		d.must(createFor("æøå", "v"))
		d.must(createFor("t1", "v"))
		sqliteError(t, "", d.exec("create table T1 (v int primary key) strict, without rowid"), 1, "table T1 already exists")
	})
}

func TestCreateTable_Key(t *testing.T) {
	t.Run("should give the index of the primary key", func(t *testing.T) {
		s := &stmt.CreateTable{Columns: []stmt.Column{{NotNull: true}, {PrimaryKey: true}, {NotNull: true}}}
		is.Equal(t, 1, s.Key())
	})

	t.Run("should panic for a table without a primary key, which the pass never gives", func(t *testing.T) {
		defer func() {
			is.True(t, recover() != nil)
		}()
		(&stmt.CreateTable{Columns: []stmt.Column{{NotNull: true}}}).Key()
		t.Fatal("no panic")
	})
}
