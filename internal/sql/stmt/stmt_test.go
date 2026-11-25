package stmt_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"math/big"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
	"maragu.dev/s4ql/internal/sql/token"
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
// The default of a column is the value that SQLite gives the text it keeps for it, which differs
// from one spelling of the literal to another.
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
		for _, c := range columns {
			if text, ok := strings.CutPrefix(c[4], "text:"); ok {
				c[4] = "default " + show(d.value("select "+text))
			}
		}
		_, indexes := d.query("select * from pragma_index_list(?)", name)
		fmt.Fprintln(&b, "table", list, "columns", columns, "indexes", indexes)
		for _, ix := range indexes {
			_, cols := d.query("select * from pragma_index_xinfo(?)", strings.TrimPrefix(ix[1], "text:"))
			fmt.Fprintln(&b, "index", ix[1], cols)
		}
	}
	return b.String()
}

// snapshot of the database: its [db.schema] and the text that sqlite_schema keeps of each object, so
// that two snapshots are the same only if a statement between them changed nothing.
func (d *db) snapshot() string {
	d.t.Helper()
	_, kept := d.query("select type, name, sql from sqlite_schema order by name")
	return d.schema() + fmt.Sprintln("kept", kept)
}

// nothing requires that src runs on d without an error and changes nothing of its schema, as SQLite
// does for IF NOT EXISTS when the object exists and for IF EXISTS when it does not.
func (d *db) nothing(src string) {
	d.t.Helper()
	before := d.snapshot()
	d.must(src)
	if after := d.snapshot(); after != before {
		d.t.Fatalf("%q: SQLite changes\n%v\nto\n%v", src, before, after)
	}
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
// canonical form: every name in double quotes, NOT NULL after PRIMARY KEY, and DEFAULT after both,
// with the canonical literal of its value.
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
		if c.Default.Value != nil {
			def += " default " + literal(c.Default.Value)
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

// canonical text of the statement s: the plain form, with every name in double quotes, and IF NOT
// EXISTS or IF EXISTS if s says it.
func canonical(s stmt.Statement) string {
	switch s := s.(type) {
	case *stmt.CreateTable:
		if s.IfNotExists {
			return strings.Replace(createColumns(s.Table.Value, s.Columns), "create table ", "create table if not exists ", 1)
		}
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
		return fmt.Sprintf("insert into %v%v values (%v)%v", quote(s.Table.Value), list, strings.Join(values, ", "), canonicalReturning(s.Returning))
	case *stmt.Select:
		list := "*"
		if s.Columns != nil {
			list = quoteNames(s.Columns)
		}
		return fmt.Sprintf("select %v from %v%v%v", list, quote(s.Table.Value), canonicalWhere(s.Where, s.Subqueries), canonicalOrder(s))
	case *stmt.Update:
		var set []string
		for _, a := range s.Set {
			set = append(set, quote(a.Column.Value)+" = "+canonicalTerm(a.Value))
		}
		return fmt.Sprintf("update %v set %v%v%v", quote(s.Table.Value), strings.Join(set, ", "), canonicalWhere(s.Where, s.Subqueries), canonicalReturning(s.Returning))
	case *stmt.Delete:
		return fmt.Sprintf("delete from %v%v%v", quote(s.Table.Value), canonicalWhere(s.Where, s.Subqueries), canonicalReturning(s.Returning))
	case *stmt.DropTable:
		if s.IfExists {
			return "drop table if exists " + quote(s.Table.Value)
		}
		return "drop table " + quote(s.Table.Value)
	case *stmt.CreateIndex:
		var columns []string
		for _, c := range s.Columns {
			column := quote(c.Name.Value)
			if c.Descending {
				column += " desc"
			}
			columns = append(columns, column)
		}
		clause := ""
		if s.IfNotExists {
			clause = "if not exists "
		}
		return fmt.Sprintf("create index %v%v on %v (%v)", clause, quote(s.Index.Value), quote(s.Table.Value), strings.Join(columns, ", "))
	case *stmt.DropIndex:
		if s.IfExists {
			return "drop index if exists " + quote(s.Index.Value)
		}
		return "drop index " + quote(s.Index.Value)
	}
	panic(fmt.Sprintf("%T", s))
}

// canonicalTerm of a value: a ? for a parameter, the canonical expression of an expression, and the
// canonical literal otherwise.
func canonicalTerm(v stmt.Term) string {
	switch {
	case v.Parameter > 0:
		return "?"
	case v.Expression.Present():
		return canonicalExpression(v.Expression)
	}
	return literal(v.Value)
}

// canonicalExpression of e, with every name in double quotes, and parentheses only around an operand
// that SQLite would otherwise group with another operator: a left operand that binds less tightly than
// its operator, a right operand that binds less tightly or as tightly, and an operator after a
// negation. So the text has no more parentheses open at once than any text of e.
func canonicalExpression(e stmt.Expression) string {
	group := func(x stmt.Expression, tighter bool) string {
		if x.Operands != nil && (precedence(x.Kind) < precedence(e.Kind) || !tighter && precedence(x.Kind) == precedence(e.Kind)) {
			return "(" + canonicalExpression(x) + ")"
		}
		return canonicalExpression(x)
	}
	switch e.Kind {
	case stmt.ExprValue:
		return literal(e.Value)
	case stmt.ExprParameter:
		return "?"
	case stmt.ExprColumn:
		return quote(e.Column.Value)
	case stmt.ExprNegate:
		return "-" + group(e.Operands[0], false)
	}
	return group(e.Operands[0], true) + " " + e.Kind.String() + " " + group(e.Operands[1], false)
}

// precedence of the operator of the kind k, higher for one that binds more tightly: a negation more
// tightly than * and /, and those more tightly than + and -.
func precedence(k stmt.ExpressionKind) int {
	switch k {
	case stmt.ExprNegate:
		return 3
	case stmt.ExprMultiply, stmt.ExprDivide:
		return 2
	case stmt.ExprAdd, stmt.ExprSubtract:
		return 1
	}
	return 4
}

// parenthesized expression e, with every name in double quotes and every operator in parentheses of
// its own, so that SQLite reads the tree of e from it whatever the precedence and the grouping of the
// operators.
func parenthesized(e stmt.Expression) string {
	switch e.Kind {
	case stmt.ExprValue, stmt.ExprParameter, stmt.ExprColumn:
		return canonicalExpression(e)
	case stmt.ExprNegate:
		return "(-" + parenthesized(e.Operands[0]) + ")"
	}
	return "(" + parenthesized(e.Operands[0]) + " " + e.Kind.String() + " " + parenthesized(e.Operands[1]) + ")"
}

// canonicalWhere of the conditions where, whose subqueries are subqueries: " where " and the
// conditions joined by " and ", each with its two sides in the order of the text, a subquery as its
// canonical select in parentheses, or "" for no conditions.
func canonicalWhere(where []stmt.Condition, subqueries []stmt.Select) string {
	var conditions []string
	for _, c := range where {
		tested := quote(c.Column.Value)
		if c.Expression.Present() {
			tested = canonicalExpression(c.Expression)
		}
		if c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull {
			conditions = append(conditions, tested+" "+c.Operator.String())
			continue
		}
		var value string
		if c.Value.Subquery > 0 {
			value = "(" + canonical(&subqueries[c.Value.Subquery-1]) + ")"
		} else {
			value = canonicalTerm(c.Value)
		}
		switch {
		case c.ValueFirst:
			// In the order of the text, so that the parameters of the two sides keep their numbers.
			conditions = append(conditions, value+" "+c.Operator.Swapped().String()+" "+tested)
		default:
			conditions = append(conditions, tested+" "+c.Operator.String()+" "+value)
		}
	}
	if conditions == nil {
		return ""
	}
	return " where " + strings.Join(conditions, " and ")
}

// canonicalReturning of the RETURNING clause r: " returning " and its columns, or *, or "" if the
// statement has none.
func canonicalReturning(r stmt.Returning) string {
	switch {
	case !r.Present():
		return ""
	case r.Columns == nil:
		return " returning *"
	}
	return " returning " + quoteNames(r.Columns)
}

// canonicalOrder of the select s: " order by " and its columns, each with " desc" if it is
// descending, and " limit " and its limit, or "" for each that s does not have.
func canonicalOrder(s *stmt.Select) string {
	var b strings.Builder
	for i, c := range s.OrderBy {
		if i == 0 {
			b.WriteString(" order by ")
		} else {
			b.WriteString(", ")
		}
		b.WriteString(quote(c.Name.Value))
		if c.Descending {
			b.WriteString(" desc")
		}
	}
	if s.Limited() {
		b.WriteString(" limit " + canonicalTerm(s.Limit))
	}
	return b.String()
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

	if checkExpressions(t, open, src, s) || checkSubqueries(t, open, src, s) {
		return
	}
	switch s := s.(type) {
	case *stmt.CreateTable:
		checkCreateTable(t, open, src, s)
	case *stmt.Insert:
		checkInsert(t, open, src, s)
	case *stmt.Select:
		checkSelect(t, open, src, s)
	case *stmt.Update:
		checkChange(t, open, src, s, s.Table, s.Set, s.Where, s.Returning)
	case *stmt.Delete:
		checkChange(t, open, src, s, s.Table, nil, s.Where, s.Returning)
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
		if !s.IfExists {
			sqliteError(t, src, open(t).exec(src), 1, "no such table: "+s.Table.Value)
			break
		}
		// Without the table, IF EXISTS does nothing, also when an index has the name.
		open(t).nothing(src)
		d := open(t)
		d.must(createFor("s4ql t", "v"))
		d.must(`create index ` + quote(swapCase(s.Table.Value)) + ` on "s4ql t" ("v")`)
		d.nothing(src)
	case *stmt.CreateIndex:
		checkCreateIndex(t, open, src, s)
	case *stmt.DropIndex:
		checkDropIndex(t, open, src, s)
	}
}

// indexes of the database: the name of each index in sqlite_schema and the name of its table, in
// order.
func (d *db) indexes() [][]string {
	d.t.Helper()
	_, rows := d.query("select name, tbl_name from sqlite_schema where type = 'index' order by name")
	return rows
}

// noSuchColumn is SQLite's message for a column named n that the table does not have, in a WHERE or
// in the columns of an index.
func noSuchColumn(n stmt.Name) string {
	if n.Raw[0] == '"' {
		// SQLite asks whether it should be a string, since its build reads double quotes as names only.
		return `no such column: "` + n.Value + `" - should this be a string literal in single-quotes?`
	}
	return "no such column: " + n.Value
}

// checkCreateIndex is [checkMeaning] of a CREATE INDEX. The table has a column for each column of the
// index, of its name with the case swapped, in the other order, and a primary key of its own, "s4ql
// key", first. On it, src makes the index that the canonical text makes: of the name of s, on the
// table, not unique and not partial, with the columns of s in their order, each descending if it says
// so, and then the primary key, as SQLite records it; a second run gives `index <Value> already
// exists`, and so does an index of the name with the case swapped on another table. SQLite gives
// `no such table: main.<Value>` without the table, also with a table of the name of the index; `there
// is already a table named <Value>` with a table of the name of the index, with the case swapped,
// which the table of src is when the two names are one name; and `no such column` for the first and
// the last column of s on a table without it, and for the first on a table without any of them, but
// for the error of an index of the name, which comes first. SQLite looks the names up in this order.
// With IF NOT EXISTS, SQLite does nothing where it gives `index <Value> already exists` without it,
// also when the table lacks the columns, and gives every other error as without it.
func checkCreateIndex(t testing.TB, open func(testing.TB) *db, src string, s *stmt.CreateIndex) {
	t.Helper()
	is.Equal(t, 0, s.Parameters(), src)
	table := swapCase(s.Table.Value)
	// with the table of the columns of the index but those of without, by their indexes in s.Columns.
	with := func(without map[int]bool) *db {
		columns := []stmt.Column{{Name: stmt.Name{Value: "s4ql key"}, Type: stmt.TypeInt, PrimaryKey: true}}
		for i := len(s.Columns) - 1; i >= 0; i-- {
			if !without[i] {
				columns = append(columns, stmt.Column{Name: stmt.Name{Value: swapCase(s.Columns[i].Name.Value)}, Type: stmt.TypeInt, NotNull: true})
			}
		}
		d := open(t)
		d.must(createColumns(table, columns))
		return d
	}
	all := map[int]bool{}
	for i := range s.Columns {
		all[i] = true
	}
	alreadyTable := "there is already a table named " + s.Index.Value
	alreadyIndex := "index " + s.Index.Value + " already exists"

	if s.Index.Fold() == s.Table.Fold() {
		// The index has the name of its table, which SQLite never creates.
		sqliteError(t, src, with(nil).exec(src), 1, alreadyTable)
		sqliteError(t, src, with(all).exec(src), 1, alreadyTable)
		sqliteError(t, src, with(nil).exec(canonical(s)), 1, alreadyTable)
	} else {
		d := with(nil)
		is.Equal(t, d.parameters(src), s.Parameters(), src)
		d.must(src)
		want := with(nil)
		want.must(canonical(s))
		if d.schema() != want.schema() {
			t.Fatalf("%q: SQLite records\n%v\nbut for the canonical text\n%v", src, d.schema(), want.schema())
		}
		is.Equal(t, fmt.Sprint([][]string{{"text:" + s.Index.Value, "text:" + table}}), fmt.Sprint(d.indexes()), src)
		_, kept := d.query("select sql from sqlite_schema where type = 'index' and name = ?", s.Index.Value)
		is.Equal(t, fmt.Sprint([][]string{{"text:" + s.SQL}}), fmt.Sprint(kept), src)
		// The index of the primary key, of the origin pk, is the table itself.
		_, list := d.query(`select name, "unique", origin, partial from pragma_index_list(?) where origin <> 'pk'`, table)
		is.Equal(t, fmt.Sprint([][]string{{"text:" + s.Index.Value, "integer:0", "text:c", "integer:0"}}), fmt.Sprint(list), src)
		var key [][]string
		for _, c := range s.Columns {
			desc := "integer:0"
			if c.Descending {
				desc = "integer:1"
			}
			key = append(key, []string{"text:" + swapCase(c.Name.Value), desc, "text:BINARY", "integer:1"})
		}
		key = append(key, []string{"text:s4ql key", "integer:0", "text:BINARY", "integer:0"})
		_, xinfo := d.query("select name, desc, coll, key from pragma_index_xinfo(?)", s.Index.Value)
		is.Equal(t, fmt.Sprint(key), fmt.Sprint(xinfo), src)
		// already gives SQLite's answer for an index of the name on d: `index <Value> already exists`,
		// or nothing with IF NOT EXISTS.
		already := func(d *db) {
			if s.IfNotExists {
				d.nothing(src)
			} else {
				sqliteError(t, src, d.exec(src), 1, alreadyIndex)
			}
		}
		already(d)

		other := with(all)
		other.must(createFor("s4ql other", "v"))
		other.must(`create index ` + quote(swapCase(s.Index.Value)) + ` on "s4ql other" ("v")`)
		already(other)
		named := with(all)
		named.must(createFor(swapCase(s.Index.Value), "v"))
		sqliteError(t, src, named.exec(src), 1, alreadyTable)
	}

	missingTable := "no such table: main." + s.Table.Value
	sqliteError(t, src, open(t).exec(src), 1, missingTable)
	sqliteError(t, src, open(t).exec(canonical(s)), 1, missingTable)
	if s.Index.Fold() != s.Table.Fold() {
		d := open(t)
		d.must(createFor(swapCase(s.Index.Value), "v"))
		sqliteError(t, src, d.exec(src), 1, missingTable)

		first, last := s.Columns[0].Name, s.Columns[len(s.Columns)-1].Name
		sqliteError(t, src, with(map[int]bool{0: true}).exec(src), 1, noSuchColumn(first))
		sqliteError(t, src, with(map[int]bool{len(s.Columns) - 1: true}).exec(src), 1, noSuchColumn(last))
		sqliteError(t, src, with(all).exec(src), 1, noSuchColumn(first))
	}
}

// checkDropIndex is [checkMeaning] of a DROP INDEX: on a table with an index of the name of s, with
// the case swapped, and another index, src drops the index of the name and leaves the other, as the
// canonical text does. SQLite gives `no such index: <Value>` without the index, also with a table of
// the name of the index. With IF EXISTS, SQLite does nothing there.
func checkDropIndex(t testing.TB, open func(testing.TB) *db, src string, s *stmt.DropIndex) {
	t.Helper()
	other := "other_" + s.Index.Value
	drop := func(sql string) [][]string {
		d := open(t)
		d.must(createFor("s4ql t", "v"))
		d.must(`create index ` + quote(swapCase(s.Index.Value)) + ` on "s4ql t" ("v")`)
		d.must(`create index ` + quote(other) + ` on "s4ql t" ("v")`)
		is.Equal(t, d.parameters(sql), s.Parameters(), sql)
		d.must(sql)
		return d.indexes()
	}
	got := drop(src)
	is.Equal(t, fmt.Sprint([][]string{{"text:" + other, "text:s4ql t"}}), fmt.Sprint(got), src)
	is.Equal(t, fmt.Sprint(drop(canonical(s))), fmt.Sprint(got), src)
	missing := func(d *db) {
		if s.IfExists {
			d.nothing(src)
		} else {
			sqliteError(t, src, d.exec(src), 1, "no such index: "+s.Index.Value)
		}
	}
	missing(open(t))
	d := open(t)
	d.must(createFor(swapCase(s.Index.Value), "v"))
	missing(d)
}

// checkCreateTable is [checkMeaning] of a CREATE TABLE: the schema that SQLite records for src is
// that of the canonical text, with the names, the types, the primary key, and the options of s; a
// second run gives `table <Raw> already exists`. For a statement with an error of its columns, SQLite
// gives that error on a database without the table, for src and for the canonical text, and `table
// <Raw> already exists` on a database with it. With IF NOT EXISTS, SQLite does nothing where it gives
// `already exists` without it, also on a table of the name with other columns, and gives `there is
// already an index named <Value>` on a database with an index of the name.
func checkCreateTable(t testing.TB, open func(testing.TB) *db, src string, s *stmt.CreateTable) {
	t.Helper()
	is.Equal(t, 0, s.Parameters(), src)
	// already gives SQLite's answer for a table of the name on d.
	already := func(d *db) {
		if s.IfNotExists {
			d.nothing(src)
		} else {
			sqliteError(t, src, d.exec(src), 1, "table "+s.Table.Raw+" already exists")
		}
	}
	if s.IfNotExists {
		d := open(t)
		d.must(createFor("s4ql t", "v"))
		d.must(`create index ` + quote(swapCase(s.Table.Value)) + ` on "s4ql t" ("v")`)
		sqliteError(t, src, d.exec(src), 1, "there is already an index named "+s.Table.Value)
	}
	if e := s.ColumnsError(); e != nil {
		is.Equal(t, sqlerr.CodeError, e.Code, src)
		is.Equal(t, sqlerr.CodeError, e.ExtendedCode, src)
		err := open(t).exec(src)
		sqliteError(t, src, err, 1, e.Message)
		if se, _ := errors.AsType[*sqlite.Error](err); se.Offset != e.Offset {
			t.Fatalf("%q: SQLite gives the offset %v, and ColumnsError %v", src, se.Offset, e.Offset)
		}
		sqliteError(t, src, open(t).exec(canonical(s)), 1, e.Message)
		d := open(t)
		d.must(createFor(swapCase(s.Table.Value), "v"))
		already(d)
		return
	}
	d := open(t)
	is.Equal(t, d.parameters(src), s.Parameters(), src)
	d.must(src)
	_, kept := d.query("select sql from sqlite_schema where name = ?", s.Table.Value)
	is.Equal(t, fmt.Sprint([][]string{{"text:" + s.SQL}}), fmt.Sprint(kept), src)
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
		// SQLite keeps the text of the literal of a DEFAULT, with its sign, as the default.
		dflt := "null:"
		if c.Default.Value != nil {
			dflt = "text:" + src[c.Default.Span.Start:c.Default.Span.End]
		}
		columns = append(columns, []string{fmt.Sprint("integer:", i), "text:" + c.Name.Value, "text:" + c.Type.String(), notNull, dflt, pk, "integer:0"})
	}
	_, got2 := d.query("select * from pragma_table_xinfo(?)", s.Table.Value)
	is.Equal(t, fmt.Sprint(columns), fmt.Sprint(got2), src)
	checkDefaults(t, open, src, s)
	already(d)
	if s.IfNotExists {
		other := open(t)
		other.must(createFor(swapCase(s.Table.Value), "s4ql other"))
		already(other)
	}
}

// checkDefaults of the create table s, which src holds and which has no error of its columns: on the
// table that src creates, and on the one that its canonical text creates, an insert with a list of
// columns that leaves out a column with a DEFAULT, and gives the other columns a value each, stores
// the value of the DEFAULT, as SQLite gives it to the text of its literal. A real 0 comes back as 0.0
// of either sign, since SQLite stores a real of a REAL column that has an integer value as an
// integer. For DEFAULT NULL, the insert stores NULL, or gives `NOT NULL constraint failed` for a
// column that is NOT NULL or the primary key, as for a column without a DEFAULT; and for a primary
// key with a DEFAULT that is not NULL, a second insert gives `UNIQUE constraint failed`.
func checkDefaults(t testing.TB, open func(testing.TB) *db, src string, s *stmt.CreateTable) {
	t.Helper()
	values := map[stmt.Type]string{stmt.TypeInt: "7", stmt.TypeInteger: "8", stmt.TypeReal: "2.5", stmt.TypeText: "'b'", stmt.TypeBlob: "x'02'"}
	for i, c := range s.Columns {
		if c.Default.Value == nil {
			continue
		}
		if got, want := show(open(t).value("select "+src[c.Default.Span.Start:c.Default.Span.End])), show(c.Default.Value); got != want {
			t.Fatalf("%q: the default of %v is %v, SQLite gives %v for its text", src, c.Name.Value, want, got)
		}
		var names, given []string
		for j, o := range s.Columns {
			if j != i {
				names, given = append(names, quote(o.Name.Value)), append(given, values[o.Type])
			}
		}
		insert := "insert into " + quote(s.Table.Value) + " default values"
		if names != nil {
			insert = fmt.Sprintf("insert into %v (%v) values (%v)", quote(s.Table.Value), strings.Join(names, ", "), strings.Join(given, ", "))
		}
		for _, create := range []string{src, canonical(s)} {
			d := open(t)
			d.must(create)
			_, null := c.Default.Value.(stmt.Null)
			if null && (c.PrimaryKey || c.NotNull) {
				sqliteError(t, src, d.exec(insert), 1299, "NOT NULL constraint failed: "+s.Table.Value+"."+c.Name.Value)
				continue
			}
			d.must(insert)
			want := c.Default.Value
			if r, ok := want.(stmt.Real); ok && r == 0 {
				want = stmt.Real(0)
			}
			got := d.value("select " + quote(c.Name.Value) + " from " + quote(s.Table.Value))
			if show(got) != show(want) {
				t.Fatalf("%q: %q stores %v in %v, want %v", src, insert, show(got), c.Name.Value, show(want))
			}
			if c.PrimaryKey {
				sqliteError(t, src, d.exec(insert), 1555, "UNIQUE constraint failed: "+s.Table.Value+"."+c.Name.Value)
			}
		}
	}
}

// checkInsert is [checkMeaning] of an INSERT: the value of each literal is the one that SQLite gives
// its text; on a table of the columns that src names, created with the case of the names swapped and
// in another order, src stores its values in the columns that it names, as the canonical text does; a
// second run gives `UNIQUE constraint failed` with the names as the table stores them; with a NULL,
// a table whose columns say NOT NULL gives `NOT NULL constraint failed`, as [checkNotNull] says; and
// SQLite gives `no such table`, `has no column named`, a wrong number of values, or `values for` with
// the names and the numbers of s. When every value is NULL, no column can be the primary key of a
// table that holds them, and only the stored values are not checked.
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

	// The columns of RETURNING, once each, in the order of the text. Without a list, the table has as
	// many columns as the statement has values, so it holds the columns of RETURNING only if they are
	// not more.
	returned := distinct(s.Returning.Columns)
	unfit := s.Returning.Present() && s.Columns == nil && len(returned) > n

	// The columns of the table, by the values that they get, in the order of the values, with the
	// case of their names swapped. Without a list of columns, they are named c1, c2, and so on, or for
	// a statement with RETURNING, the first by the columns of RETURNING and the others s4ql c2, s4ql c3,
	// and so on.
	var names []string
	if s.Columns == nil {
		for i := range n {
			switch {
			case s.Returning.Present() && !unfit && i < len(returned):
				names = append(names, swapCase(returned[i].Value))
			case s.Returning.Present():
				names = append(names, fmt.Sprint("s4ql c", i+1))
			default:
				names = append(names, fmt.Sprint("c", i+1))
			}
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
	// With a list, the table has its columns in the reverse order, so that the values go by name, and
	// after them the columns of RETURNING that the list does not name, each with a DEFAULT of its own,
	// which the insert stores in it.
	tableColumns := slices.Clone(columns)
	if s.Columns != nil {
		slices.Reverse(tableColumns)
		listed := map[string]bool{}
		for _, c := range s.Columns {
			listed[c.Fold()] = true
		}
		for i, r := range returned {
			if !listed[r.Fold()] {
				tableColumns = append(tableColumns, stmt.Column{Name: stmt.Name{Value: swapCase(r.Value)}, Type: stmt.TypeInt,
					Default: stmt.Default{Value: stmt.Integer(1000 + i)}})
			}
		}
	}
	nulls := map[string]bool{}
	for i, v := range stored {
		if _, ok := v.(stmt.Null); ok {
			nulls[names[i]] = true
		}
	}
	// A table that lacks a column of RETURNING gives SQLite's error for it before any other, which
	// checkReturningMissing checks.
	if !unfit {
		checkNotNull(t, open, src, table, tableColumns, nulls, args)
	}
	// When every value is NULL, no column of the table can be its primary key, and no table stores the
	// row.
	if key >= 0 && !unfit {
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
		if s.Returning.Present() {
			checkInsertReturning(t, open, src, s, createColumns(table, tableColumns), table, args)
		}
	}
	checkReturningMissing(t, open, src, s, table, names, args)

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

// distinct names of names, the first of each by its fold, in the order of names.
func distinct(names []stmt.Name) []stmt.Name {
	var once []stmt.Name
	seen := map[string]bool{}
	for _, n := range names {
		if !seen[n.Fold()] {
			seen[n.Fold()] = true
			once = append(once, n)
		}
	}
	return once
}

// returningList of the RETURNING clause r, as a select list of the canonical text writes it: the
// columns by the values of their names in double quotes, or *.
func returningList(r stmt.Returning) string {
	if r.Columns == nil {
		return "*"
	}
	return quoteNames(r.Columns)
}

// checkInsertReturning of the insert s of src with RETURNING, with args, on a new database of the
// table that create creates: src gives one row, with the columns of RETURNING, or every column for *,
// by the names that the table stores and with the values that a select of the same columns reads
// after it, as the canonical text does. So an insert gives the values as the table stores them, and
// its DEFAULT in a column that the list leaves out.
func checkInsertReturning(t testing.TB, open func(testing.TB) *db, src string, s *stmt.Insert, create, table string, args []int64) {
	t.Helper()
	read := func(sql string) (string, string, int) {
		d := open(t)
		d.must(create)
		names, rows, err := d.run(sql, integers(args))
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		selected, after, err := d.run("select "+returningList(s.Returning)+" from "+quote(table), nil)
		is.NotError(t, err)
		return fmt.Sprint(names, "\n", showRows(rows)), fmt.Sprint(selected, "\n", showRows(after)), len(rows)
	}
	got, want, n := read(src)
	is.Equal(t, 1, n, src)
	is.Equal(t, want, got, src)
	canonicalGot, _, _ := read(canonical(s))
	is.Equal(t, got, canonicalGot, src)
}

// checkReturningMissing of the insert s of src, with args: SQLite looks for the columns of RETURNING
// after the table, the columns of the list, and the number of the values, in the order of the text.
// On a table of the columns of the list, by names, or without a list of as many columns as values,
// none of which RETURNING names, SQLite gives `no such column` for the first column of RETURNING that
// the table does not have, with the question about a string literal for a name in double quotes.
func checkReturningMissing(t testing.TB, open func(testing.TB) *db, src string, s *stmt.Insert, table string, names []string, args []int64) {
	t.Helper()
	if s.Returning.Columns == nil || s.Columns != nil && len(s.Columns) != len(s.Values) {
		return
	}
	columns := names
	if s.Columns == nil {
		columns = nil
		for i := range s.Values {
			columns = append(columns, fmt.Sprint("s4ql c", i+1))
		}
	}
	has := map[string]bool{}
	for _, c := range columns {
		has[stmt.Name{Value: c}.Fold()] = true
	}
	for _, r := range s.Returning.Columns {
		if !has[r.Fold()] {
			d := open(t)
			d.must(createColumns(table, intColumns(columns, 0)))
			_, _, err := d.run(src, integers(args))
			sqliteError(t, src, err, 1, noSuchColumn(r))
			return
		}
	}
}

// integers of args, as values.
func integers(args []int64) []stmt.Value {
	var values []stmt.Value
	for _, a := range args {
		values = append(values, stmt.Integer(a))
	}
	return values
}

// checkSelect is [checkMeaning] of a SELECT: on a table created with the case of its names swapped,
// beside another table, src reads the rows of the named table in key order, with the columns that it
// names, by the names that the table stores, or every column in the order of the table for *, as the
// canonical text does; SQLite gives `no such table` for it without the table, and `no such column`
// for the first name of the list on a table without it.
func checkSelect(t testing.TB, open func(testing.TB) *db, src string, s *stmt.Select) {
	t.Helper()
	if s.Where != nil || s.OrderBy != nil || s.Limited() {
		checkWhere(t, open, src, s)
		return
	}
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

// whereReads counts the selects with WHERE that [checkWhere] ran, by whether they read no row of the
// table, some, or every row, so that a test can see that the conditions choose rows.
var whereReads [3]atomic.Int64

// changeReads counts the updates and the deletes with WHERE that [checkChange] ran, by whether they
// changed no row of the table, some, or every row, as [whereReads] counts the selects.
var changeReads [3]atomic.Int64

// returningReads counts the updates and the deletes with WHERE and RETURNING that
// [checkChangeReturning] ran, by whether they gave no row, some, or a row for every row of the table,
// so that a test can see that the check compares rows.
var returningReads [3]atomic.Int64

// count in reads a statement that read or changed n of the all rows of a table: none, some, or every
// one.
func count(reads *[3]atomic.Int64, n, all int) {
	switch n {
	case 0:
		reads[0].Add(1)
	case all:
		reads[2].Add(1)
	default:
		reads[1].Add(1)
	}
}

// checkLiterals of src: the value of each literal of terms is the one that SQLite gives its text.
func checkLiterals(t testing.TB, open func(testing.TB) *db, src string, terms []stmt.Term) {
	t.Helper()
	for _, v := range terms {
		if v.Parameter == 0 && v.Value != nil {
			lit := src[v.Span.Start:v.Span.End]
			if got, want := show(open(t).value("select "+lit)), show(v.Value); got != want {
				t.Fatalf("%q: the value is %v, SQLite gives %v for %q", src, want, got, lit)
			}
		}
	}
}

// conditionValues of where: the value of each condition, which is the zero Term for a test for NULL.
func conditionValues(where []stmt.Condition) []stmt.Term {
	var terms []stmt.Term
	for _, c := range where {
		terms = append(terms, c.Value)
	}
	return terms
}

// anyTable is a table for the check of a statement with WHERE: a column for each name that the
// statement names, once each as SQLite compares names, with the case of the name swapped, and a
// primary key of its own, "s4ql key", in the middle. The columns are of the type ANY, which SQLite
// compares with a value without converting either, and which stores a value as it is.
type anyTable struct {
	name string
	// names of the columns, in the order of the table, the key at key.
	names []string
	// index of each column in names, by the fold of the name that the statement writes.
	index map[string]int
	key   int
}

// newAnyTable named name, with a column for each of names.
func newAnyTable(name string, names []stmt.Name) anyTable {
	a := anyTable{name: name, index: map[string]int{}}
	for _, n := range names {
		if _, ok := a.index[n.Fold()]; !ok {
			a.index[n.Fold()] = len(a.names)
			a.names = append(a.names, swapCase(n.Value))
		}
	}
	a.key = len(a.names) / 2
	a.names = slices.Insert(a.names, a.key, "s4ql key")
	for f, i := range a.index {
		if i >= a.key {
			a.index[f] = i + 1
		}
	}
	return a
}

// create the table with the columns of names, which are those of the table or some of them.
func (a anyTable) create(names []string) string {
	var defs []string
	for _, n := range names {
		if n == "s4ql key" {
			defs = append(defs, quote(n)+" int primary key")
		} else {
			defs = append(defs, quote(n)+" any")
		}
	}
	return fmt.Sprintf("create table %v (%v) strict, without rowid", quote(a.name), strings.Join(defs, ", "))
}

// rows of the table for the conditions where of the statement src with args, at least least of them:
// the values of each column of a condition are those of [anyTable.pools], as [whereRows] combines
// them.
func (a anyTable) rows(src string, where []stmt.Condition, args []stmt.Value, least int) [][]stmt.Value {
	return whereRows(src, a.pools(where, args), a.key, least)
}

// pools of the values of the columns of the table for the conditions where with args: for each column
// of a condition, those around the value of each of its conditions, and NULL and the least value of
// each type but the real, and nil for every other column.
func (a anyTable) pools(where []stmt.Condition, args []stmt.Value) [][]stmt.Value {
	pools := make([][]stmt.Value, len(a.names))
	for _, c := range where {
		i := a.index[c.Column.Fold()]
		if pools[i] == nil {
			pools[i] = []stmt.Value{stmt.Null{}, stmt.Integer(math.MinInt64), stmt.Text(""), stmt.Blob{}}
		}
		switch {
		case c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull:
		case c.Value.Parameter > 0:
			pools[i] = append(pools[i], around(args[c.Value.Parameter-1])...)
		default:
			pools[i] = append(pools[i], around(c.Value.Value)...)
		}
	}
	return pools
}

// refuses requires SQLite's error want when src runs with args on the table without the columns of
// missing, by their folds.
func (a anyTable) refuses(t testing.TB, open func(testing.TB) *db, src string, args []stmt.Value, missing map[string]bool, want string) {
	t.Helper()
	var columns []string
	for _, c := range a.names {
		if !missing[stmt.Name{Value: c}.Fold()] {
			columns = append(columns, c)
		}
	}
	d := open(t)
	d.must(a.create(columns))
	_, _, err := d.run(src, args)
	sqliteError(t, src, err, 1, want)
}

// noSuchColumn requires SQLite's error for the name n of the select list or of a condition when src
// runs with args on the table without the columns of missing, by their folds.
func (a anyTable) noSuchColumn(t testing.TB, open func(testing.TB) *db, src string, args []stmt.Value, n stmt.Name, missing map[string]bool) {
	t.Helper()
	a.refuses(t, open, src, args, missing, noSuchColumn(n))
}

// checkMissing of the conditions where of src, with args, on the table: SQLite's error on the table
// without the first column of the conditions that listed does not have, by the folds of their names,
// and on the table without all of them. SQLite looks for the names of the conditions after those of
// listed, and for those in the order of the text. It stops at a comparison whose column is missing.
// It goes on after a test for NULL whose column is missing, as it does not stop the walk over the
// conditions, and a later test for NULL whose column is missing replaces its message, until a
// comparison stops it, whose column it then does not look for.
func (a anyTable) checkMissing(t testing.TB, open func(testing.TB) *db, src string, args []stmt.Value, where []stmt.Condition, listed map[string]bool) {
	t.Helper()
	reported := func(missing map[string]bool) stmt.Name {
		var n stmt.Name
		for _, c := range where {
			test := c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull
			if n.Raw != "" && !test {
				break
			}
			if missing[c.Column.Fold()] {
				n = c.Column
				if !test {
					break
				}
			}
		}
		return n
	}
	first, all := map[string]bool{}, map[string]bool{}
	for _, c := range where {
		if !listed[c.Column.Fold()] {
			if len(first) == 0 {
				first[c.Column.Fold()] = true
			}
			all[c.Column.Fold()] = true
		}
	}
	if len(first) > 0 {
		a.noSuchColumn(t, open, src, args, reported(first), first)
		a.noSuchColumn(t, open, src, args, reported(all), all)
	}
}

// checkWhere is [checkSelect] of a SELECT with WHERE, ORDER BY, or LIMIT. The value of each literal
// of a condition is the one that SQLite gives its text. The table is an [anyTable] of the columns
// that src names, those of the select list first, and holds the values around those of the
// conditions, of every type, and NULL, as [anyTable.rows] gives them, and for a column of ORDER BY
// without a condition those of [orderValues], with ties. On it, src reads the same rows, by the same
// names, as the canonical text does, with the same arguments, which differ from each other, but for
// that of a ? of LIMIT, an integer from [limitArg]; with * and without ORDER BY, in key order. The
// ORDER BY and LIMIT of src mean what the statement says, as [checkOrder] checks. SQLite gives `no
// such table` for src without the table, `no such column` for the first name of the select list on a
// table without it, `no such column` for the columns of the conditions that the list does not name,
// as [anyTable.checkMissing] says, and then for the first column of ORDER BY that neither names.
func checkWhere(t testing.TB, open func(testing.TB) *db, src string, s *stmt.Select) {
	t.Helper()
	checkLiterals(t, open, src, conditionValues(s.Where))
	args := whereArgs(src, s.Parameters())
	if s.Limit.Parameter > 0 {
		args[s.Limit.Parameter-1] = limitArg(src)
	}
	named := slices.Clone(s.Columns)
	for _, c := range s.Where {
		named = append(named, c.Column)
	}
	for _, c := range s.OrderBy {
		named = append(named, c.Name)
	}
	a := newAnyTable(swapCase(s.Table.Value), named)
	least := 1
	if s.OrderBy != nil {
		least = 30
	}
	rows := a.orderedRows(src, s, args, least)
	// readWith the arguments of the text sql: its names and its rows.
	readWith := func(sql string, args []stmt.Value) ([]string, [][]stmt.Value) {
		d := open(t)
		d.must(a.create(a.names))
		d.insert(a.name, rows)
		is.Equal(t, d.parameters(sql), len(args), sql)
		names, got, err := d.run(sql, args)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		return names, got
	}
	read := func(sql string) ([]string, [][]stmt.Value) {
		return readWith(sql, args)
	}
	is.Equal(t, s.Parameters(), len(args), src)
	gotNames, got := read(src)
	wantNames := a.names
	if s.Columns != nil {
		wantNames = nil
		for _, c := range s.Columns {
			wantNames = append(wantNames, a.names[a.index[c.Fold()]])
		}
	}
	is.Equal(t, fmt.Sprint(wantNames), fmt.Sprint(gotNames), src)
	canonicalNames, want := read(canonical(s))
	is.Equal(t, fmt.Sprint(gotNames), fmt.Sprint(canonicalNames), src)
	is.Equal(t, showRows(want), showRows(got), src)
	if s.Columns == nil && s.OrderBy == nil {
		for i := 1; i < len(got); i++ {
			if got[i-1][a.key].(stmt.Integer) >= got[i][a.key].(stmt.Integer) {
				t.Fatalf("%q: the rows are not in key order: %v", src, showRows(got))
			}
		}
	}
	if s.Where != nil {
		count(&whereReads, len(got), len(rows))
	}
	if s.OrderBy != nil || s.Limited() {
		checkOrder(t, src, s, a, args, got, func(sql string, args []stmt.Value) [][]stmt.Value {
			_, rows := readWith(sql, args)
			return rows
		})
	}

	_, _, err := open(t).run(src, args)
	sqliteError(t, src, err, 1, "no such table: "+s.Table.Value)
	listed := map[string]bool{}
	if s.Columns != nil {
		a.noSuchColumn(t, open, src, args, s.Columns[0], map[string]bool{s.Columns[0].Fold(): true})
		for _, n := range s.Columns {
			listed[n.Fold()] = true
		}
	}
	a.checkMissing(t, open, src, args, s.Where, listed)
	// SQLite looks for the columns of ORDER BY after those of the select list and the conditions, in
	// the order of the text.
	for _, c := range s.Where {
		listed[c.Column.Fold()] = true
	}
	var missing []stmt.Name
	for _, c := range s.OrderBy {
		if !listed[c.Name.Fold()] {
			missing = append(missing, c.Name)
		}
	}
	if missing != nil {
		a.noSuchColumn(t, open, src, args, missing[0], map[string]bool{missing[0].Fold(): true})
		all := map[string]bool{}
		for _, n := range missing {
			all[n.Fold()] = true
		}
		a.noSuchColumn(t, open, src, args, missing[0], all)
	}
}

// orderValues of a column of ORDER BY without a condition: NULL and values of every type, two of
// which compare equal, so that the rows tie on them.
var orderValues = []stmt.Value{stmt.Null{}, stmt.Integer(math.MinInt64), stmt.Integer(-1), stmt.Integer(2), stmt.Real(-1.5),
	stmt.Real(2), stmt.Real(math.Copysign(0, -1)), stmt.Text(""), stmt.Text("B"), stmt.Text("a"), stmt.Text("é"), stmt.Blob{},
	stmt.Blob{0}, stmt.Blob{0xff}}

// orderedRows of the table for the select s of src with args, at least least of them: those of
// [anyTable.rows], with the values of [orderValues] in each column of ORDER BY that no condition
// tests.
func (a anyTable) orderedRows(src string, s *stmt.Select, args []stmt.Value, least int) [][]stmt.Value {
	tested := map[string]bool{}
	for _, c := range s.Where {
		tested[c.Column.Fold()] = true
	}
	pools := a.pools(s.Where, args)
	for _, c := range s.OrderBy {
		if i := a.index[c.Name.Fold()]; !tested[c.Name.Fold()] {
			pools[i] = orderValues
		}
	}
	return whereRows(src, pools, a.key, least)
}

// limitArg of a ? of LIMIT in the statement src: an integer that the length of the text picks, none,
// one, a few, more than any table of the checks has rows, or the largest of 64 bits.
func limitArg(src string) stmt.Value {
	limits := []stmt.Value{stmt.Integer(0), stmt.Integer(1), stmt.Integer(3), stmt.Integer(1000), stmt.Integer(math.MaxInt64)}
	return limits[len(src)%len(limits)]
}

// checkOrder of the select s of src, whose rows with args on the table a are got: the canonical text
// of s with * in place of its select list reads the rows in the order of the columns of ORDER BY,
// each in its direction, as [compareValues] orders values, and as many as LIMIT says, but no more than
// the rows that the text without LIMIT reads; those rows are the first ones of the text without
// LIMIT, by their values in the columns of ORDER BY; and got is those rows with the columns of the
// select list. read reads the rows of a text with the arguments of its parameters.
func checkOrder(t testing.TB, src string, s *stmt.Select, a anyTable, args []stmt.Value, got [][]stmt.Value, read func(string, []stmt.Value) [][]stmt.Value) {
	t.Helper()
	star := *s
	star.Columns = nil
	all := read(canonical(&star), args)
	positions := make([]int, len(s.OrderBy))
	for i, c := range s.OrderBy {
		positions[i] = a.index[c.Name.Fold()]
	}
	keys := func(row []stmt.Value) string {
		var parts []stmt.Value
		for _, p := range positions {
			parts = append(parts, row[p])
		}
		return showRows([][]stmt.Value{parts})
	}
	for i := 1; i < len(all); i++ {
		for j, c := range s.OrderBy {
			cmp := compareValues(all[i-1][positions[j]], all[i][positions[j]])
			if c.Descending {
				cmp = -cmp
			}
			if cmp < 0 {
				break
			}
			if cmp > 0 {
				t.Fatalf("%q: the rows are not in the order of ORDER BY at row %v:\n%v", src, i, showRows(all))
			}
		}
	}
	if s.Limited() {
		limit := s.Limit.Value
		if s.Limit.Parameter > 0 {
			limit = args[s.Limit.Parameter-1]
		}
		unlimited := star
		unlimited.Limit = stmt.Term{}
		// The ? of LIMIT is the last parameter.
		every := read(canonical(&unlimited), args[:unlimited.Parameters()])
		n := min(int64(len(every)), int64(limit.(stmt.Integer)))
		if int64(len(all)) != n {
			t.Fatalf("%q: %v rows with LIMIT %v, of %v without it", src, len(all), show(limit), len(every))
		}
		for i := range all {
			if keys(all[i]) != keys(every[i]) {
				t.Fatalf("%q: the row %v with LIMIT is %v, without it %v", src, i, showRows(all[i:i+1]), showRows(every[i:i+1]))
			}
		}
	}
	if len(got) != len(all) {
		t.Fatalf("%q: %v rows, and %v with *", src, len(got), len(all))
	}
	for i, row := range all {
		want := row
		if s.Columns != nil {
			want = nil
			for _, c := range s.Columns {
				want = append(want, row[a.index[c.Fold()]])
			}
		}
		if showRows([][]stmt.Value{want}) != showRows(got[i:i+1]) {
			t.Fatalf("%q: the row %v is %v, with * %v", src, i, showRows(got[i:i+1]), showRows(all[i:i+1]))
		}
	}
}

// compareValues a and b as SQLite orders values without a collation: NULL first, then the numbers,
// an integer and a real by their exact values, then the texts by their bytes, then the blobs by
// their bytes. It gives a negative number if a comes first, a positive one if b does, and 0 if they
// tie.
func compareValues(a, b stmt.Value) int {
	rank := func(v stmt.Value) int {
		switch v.(type) {
		case stmt.Null:
			return 0
		case stmt.Integer, stmt.Real:
			return 1
		case stmt.Text:
			return 2
		}
		return 3
	}
	if ra, rb := rank(a), rank(b); ra != rb {
		return ra - rb
	}
	number := func(v stmt.Value) *big.Float {
		if i, ok := v.(stmt.Integer); ok {
			return new(big.Float).SetInt64(int64(i))
		}
		return big.NewFloat(float64(v.(stmt.Real)))
	}
	switch a := a.(type) {
	case stmt.Integer, stmt.Real:
		return number(a).Cmp(number(b))
	case stmt.Text:
		return strings.Compare(string(a), string(b.(stmt.Text)))
	case stmt.Blob:
		return bytes.Compare(a, b.(stmt.Blob))
	}
	return 0
}

// checkChange is [checkMeaning] of an UPDATE, whose assignments are set, or of a DELETE, for which
// set is nil, of the table with the conditions where. The value of each literal is the one that
// SQLite gives its text. The table is an [anyTable] of the columns that src names, those of the
// assignments first, with the rows of [anyTable.rows], three at least. On it, src leaves the same
// rows as the canonical text does, with the same arguments, which differ from each other, and changes
// as many rows, as SQLite counts them. Each row that is left is the row before, or, for an update,
// the row before with the values of the assignments; an update without WHERE changes every row, and
// a delete without WHERE leaves none. SQLite gives `no such table` for src without the table, `no such
// column` for the first column of the assignments on a table without it, and for the last on a table
// without it and without the columns of the conditions, with the value of the name, also of one in
// double quotes, and `no such column` for the columns of the conditions that no assignment names, as
// [anyTable.checkMissing] says.
func checkChange(t testing.TB, open func(testing.TB) *db, src string, s stmt.Statement, table stmt.Name, set []stmt.Assignment, where []stmt.Condition, returning stmt.Returning) {
	t.Helper()
	var terms []stmt.Term
	var named []stmt.Name
	for _, as := range set {
		terms = append(terms, as.Value)
		named = append(named, as.Column)
	}
	checkLiterals(t, open, src, append(terms, conditionValues(where)...))
	for _, c := range where {
		named = append(named, c.Column)
	}
	named = append(named, returning.Columns...)
	args := whereArgs(src, s.Parameters())
	a := newAnyTable(swapCase(table.Value), named)
	rows := a.rows(src, where, args, 3)
	run := func(sql string) (int, [][]stmt.Value) {
		d := open(t)
		d.must(a.create(a.names))
		d.insert(a.name, rows)
		is.Equal(t, d.parameters(sql), s.Parameters(), sql)
		if _, _, err := d.run(sql, args); err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		changes := int(d.value("select changes()").(stmt.Integer))
		_, left, err := d.run("select * from "+quote(a.name), nil)
		is.NotError(t, err)
		return changes, left
	}
	changes, got := run(src)
	canonicalChanges, want := run(canonical(s))
	is.Equal(t, showRows(want), showRows(got), src)
	is.Equal(t, canonicalChanges, changes, src)

	// Each row that is left is the row of its key before, or that row with the values of the
	// assignments.
	before := map[stmt.Integer][]stmt.Value{}
	for _, row := range rows {
		before[row[a.key].(stmt.Integer)] = row
	}
	updated := func(row []stmt.Value) []stmt.Value {
		row = slices.Clone(row)
		for _, as := range set {
			v := as.Value.Value
			if as.Value.Parameter > 0 {
				v = args[as.Value.Parameter-1]
			}
			row[a.index[as.Column.Fold()]] = v
		}
		return row
	}
	differ, could := 0, 0
	for _, row := range got {
		old, ok := before[row[a.key].(stmt.Integer)]
		if !ok {
			t.Fatalf("%q: a row that was not there: %v", src, showRows([][]stmt.Value{row}))
		}
		same := showRows([][]stmt.Value{old}) == showRows([][]stmt.Value{row})
		changed := set != nil && showRows([][]stmt.Value{updated(old)}) == showRows([][]stmt.Value{row})
		if !same && !changed {
			t.Fatalf("%q: the row %vwas %v", src, showRows([][]stmt.Value{row}), showRows([][]stmt.Value{old}))
		}
		if !same {
			differ++
		}
		if changed {
			could++
		}
	}
	if set == nil {
		is.Equal(t, len(rows)-len(got), changes, src)
	} else {
		is.Equal(t, len(rows), len(got), src)
		// A row that an update changes can keep its values, if the assignments give it those it had.
		if changes < differ || changes > could {
			t.Fatalf("%q: SQLite changed %v rows, of which %v differ, and %v have the values of the assignments", src, changes, differ, could)
		}
	}
	if where == nil {
		is.Equal(t, len(rows), changes, src)
	} else {
		count(&changeReads, changes, len(rows))
	}

	_, _, err := open(t).run(src, args)
	sqliteError(t, src, err, 1, "no such table: "+table.Value)
	listed := map[string]bool{}
	if set != nil {
		// SQLite looks for the columns of the assignments before those of the conditions, in the
		// order of the text, and prints the value of the name, also of one in double quotes.
		first, last := set[0].Column, set[len(set)-1].Column
		a.refuses(t, open, src, args, map[string]bool{first.Fold(): true}, "no such column: "+first.Value)
		for _, as := range set {
			listed[as.Column.Fold()] = true
		}
		// Without the columns of the conditions too, SQLite names the column of the assignment.
		missing := map[string]bool{last.Fold(): true}
		for _, c := range where {
			if !listed[c.Column.Fold()] {
				missing[c.Column.Fold()] = true
			}
		}
		a.refuses(t, open, src, args, missing, "no such column: "+last.Value)
	}
	a.checkMissing(t, open, src, args, where, listed)
	if !returning.Present() {
		return
	}
	checkChangeReturning(t, open, src, s, a, rows, args, set, where, returning)
	// SQLite looks for the columns of RETURNING after those of the assignments and the conditions, in
	// the order of the text.
	for _, c := range where {
		listed[c.Column.Fold()] = true
	}
	var missing []stmt.Name
	for _, c := range returning.Columns {
		if !listed[c.Fold()] {
			missing = append(missing, c)
		}
	}
	if missing != nil {
		a.noSuchColumn(t, open, src, args, missing[0], map[string]bool{missing[0].Fold(): true})
		all := map[string]bool{}
		for _, n := range missing {
			all[n.Fold()] = true
		}
		a.noSuchColumn(t, open, src, args, missing[0], all)
	}
}

// checkChangeReturning of the update or the delete s of src with RETURNING, with args, on the table a
// with the rows: src gives a row for each row that the conditions where choose, as many as SQLite
// counts as changed, with the columns of RETURNING, or every column for *, by the names that the
// table stores. An update gives the values of each such row after it, and a delete those before it,
// as a select of the same columns reads them, in some order. The canonical text gives the same rows,
// in the same order.
func checkChangeReturning(t testing.TB, open func(testing.TB) *db, src string, s stmt.Statement, a anyTable, rows [][]stmt.Value, args []stmt.Value, set []stmt.Assignment, where []stmt.Condition, returning stmt.Returning) {
	t.Helper()
	// The arguments of the conditions come after those of the assignments.
	assigned := 0
	for _, as := range set {
		if as.Value.Parameter > 0 {
			assigned++
		}
	}
	read := func(sql string) (string, int) {
		d := open(t)
		d.must(a.create(a.names))
		d.insert(a.name, rows)
		_, chosen, err := d.run(`select "s4ql key" from `+quote(a.name)+canonicalWhere(where, nil), args[assigned:])
		is.NotError(t, err)
		var keys []string
		for _, row := range chosen {
			keys = append(keys, literal(row[0]))
		}
		selectChosen := func() string {
			names, rows, err := d.run("select "+returningList(returning)+" from "+quote(a.name)+` where "s4ql key" in (`+strings.Join(keys, ", ")+")", nil)
			is.NotError(t, err)
			return fmt.Sprint(names, "\n", sortedRows(rows))
		}
		want := ""
		if set == nil {
			want = selectChosen()
		}
		names, got, err := d.run(sql, args)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		is.Equal(t, int(d.value("select changes()").(stmt.Integer)), len(got), sql)
		if set != nil {
			want = selectChosen()
		}
		is.Equal(t, want, fmt.Sprint(names, "\n", sortedRows(got)), sql)
		return fmt.Sprint(names, "\n", showRows(got)), len(got)
	}
	got, n := read(src)
	canonicalGot, _ := read(canonical(s))
	is.Equal(t, got, canonicalGot, src)
	if where != nil {
		count(&returningReads, n, len(rows))
	}
}

// subqueryReads counts the subqueries that [checkSubqueries] ran, by whether they gave no row, and so
// NULL, or a value, so that a test can see that the check compares both.
var subqueryReads [2]atomic.Int64

// checkSubqueries is [checkMeaning] of a select, an update, or a delete with a scalar subquery, and
// reports whether s is one. Each table of the statement is an [anyTable] of the columns that the
// statement names of it, by the fold of the name of the table, so that a subquery of the table of
// the statement reads that table. It holds the values around those of the conditions on it, as
// [anyTable.rows] gives them, and NULL and the least value of each type but the real in a column of
// a condition with a subquery and in the column of a subquery. On it, src does what the text does in
// which each subquery is a ? whose argument is the value that SQLite gives the select of the
// subquery, as the span of the select holds it, run alone with its arguments, on the tables as they
// are before src: of its column in its first row, or NULL without a row. So the parentheses, the
// span of each select, and the numbers of the parameters are those of the text, and the value of a
// subquery is that of its first row before the statement changes any. The canonical text does the
// same. A select gives the same rows, by the same names, and an update or a delete the same count of
// changes, the same rows of RETURNING, and the same rows left in its table. SQLite gives `no such
// table` for src without any table, and for the table of a subquery that is not the table of the
// statement without that table. A subquery of * of a table of more than one column gives `row value
// misused` in a select, and `sub-select returns 3 columns - expected 1`, with the number of the
// columns, in an update and a delete, but for != with the subquery on the left of its column, which
// gives `row value misused` there too; and the check ends there.
func checkSubqueries(t testing.TB, open func(testing.TB) *db, src string, s stmt.Statement) bool {
	t.Helper()
	var table stmt.Name
	var named []stmt.Name
	var where []stmt.Condition
	var subqueries []stmt.Select
	var terms []stmt.Term
	switch s := s.(type) {
	case *stmt.Select:
		table, where, subqueries = s.Table, s.Where, s.Subqueries
		named = slices.Clone(s.Columns)
		for _, c := range s.OrderBy {
			named = append(named, c.Name)
		}
		terms = append(terms, s.Limit)
	case *stmt.Update:
		table, where, subqueries = s.Table, s.Where, s.Subqueries
		for _, a := range s.Set {
			named = append(named, a.Column)
			terms = append(terms, a.Value)
		}
		named = append(named, s.Returning.Columns...)
	case *stmt.Delete:
		table, where, subqueries = s.Table, s.Where, s.Subqueries
		named = slices.Clone(s.Returning.Columns)
	}
	if subqueries == nil {
		return false
	}
	terms = append(terms, conditionValues(where)...)

	// The tables, the one of the statement first, by the folds of their names.
	type tableOf struct {
		name  stmt.Name
		named []stmt.Name
		where []stmt.Condition
		// selected are the columns of the subqueries of the table.
		selected []stmt.Name
	}
	var folds []string
	tables := map[string]*tableOf{}
	add := func(n stmt.Name, named []stmt.Name, where []stmt.Condition) *tableOf {
		tb := tables[n.Fold()]
		if tb == nil {
			tb = &tableOf{name: n}
			tables[n.Fold()] = tb
			folds = append(folds, n.Fold())
		}
		tb.named = append(tb.named, named...)
		for _, c := range where {
			tb.named = append(tb.named, c.Column)
		}
		tb.where = append(tb.where, where...)
		return tb
	}
	add(table, named, where)
	for _, q := range subqueries {
		named := slices.Clone(q.Columns)
		for _, c := range q.OrderBy {
			named = append(named, c.Name)
		}
		tb := add(q.Table, named, q.Where)
		tb.selected = append(tb.selected, q.Columns...)
		terms = append(terms, q.Limit)
		terms = append(terms, conditionValues(q.Where)...)
	}
	checkLiterals(t, open, src, terms)

	args := whereArgs(src, s.Parameters())
	if sel, ok := s.(*stmt.Select); ok && sel.Limit.Parameter > 0 {
		args[sel.Limit.Parameter-1] = limitArg(src)
	}
	for _, q := range subqueries {
		if q.Limit.Parameter > 0 {
			args[q.Limit.Parameter-1] = limitArg(src)
		}
	}
	type built struct {
		a    anyTable
		rows [][]stmt.Value
	}
	all := map[string]built{}
	for _, f := range folds {
		tb := tables[f]
		a := newAnyTable(swapCase(tb.name.Value), tb.named)
		var plain []stmt.Condition
		for _, c := range tb.where {
			if c.Value.Subquery == 0 {
				plain = append(plain, c)
			}
		}
		pools := a.pools(plain, args)
		others := slices.Clone(tb.selected)
		for _, c := range tb.where {
			if c.Value.Subquery > 0 {
				others = append(others, c.Column)
			}
		}
		for _, n := range others {
			if i := a.index[n.Fold()]; pools[i] == nil {
				pools[i] = []stmt.Value{stmt.Null{}, stmt.Integer(math.MinInt64), stmt.Text(""), stmt.Blob{}}
			}
		}
		all[f] = built{a: a, rows: whereRows(src+" "+f, pools, a.key, 3)}
	}
	// build a database of the tables, without the one of the fold without.
	build := func(without string) *db {
		d := open(t)
		for _, f := range folds {
			if f != without {
				d.must(all[f].a.create(all[f].a.names))
				d.insert(all[f].a.name, all[f].rows)
			}
		}
		return d
	}
	_, isSelect := s.(*stmt.Select)
	for _, c := range where {
		if c.Value.Subquery == 0 {
			continue
		}
		q := subqueries[c.Value.Subquery-1]
		if n := len(all[q.Table.Fold()].a.names); q.Columns == nil && n > 1 {
			// SQLite names the fault in a select, and the number of the columns in an update and a
			// delete, but for != with the subquery on the left, where it names the fault too.
			want := "row value misused"
			if !isSelect && (!c.ValueFirst || c.Operator != stmt.OpNotEqual) {
				want = fmt.Sprintf("sub-select returns %v columns - expected 1", n)
			}
			_, _, err := build("").run(src, args)
			sqliteError(t, src, err, 1, want)
			return true
		}
	}

	// The parameters of the statement, by the offsets of their ?, to find those before a subquery.
	var offsets []int
	for _, tk := range token.Tokens(src) {
		if tk.Kind == token.Variable {
			offsets = append(offsets, tk.Offset)
		}
	}
	is.Equal(t, len(args), len(offsets), src)
	spans := make([]stmt.Span, len(subqueries))
	for _, c := range where {
		if k := c.Value.Subquery; k > 0 {
			spans[k-1] = c.Value.Span
		}
	}
	text, textArgs := src, slices.Clone(args)
	for k := len(subqueries) - 1; k >= 0; k-- {
		q, sp := subqueries[k], spans[k]
		before := 0
		for _, o := range offsets {
			if o < sp.Start {
				before++
			}
		}
		own := q.Parameters()
		_, rows, err := build("").run(src[q.Span.Start:q.Span.End], args[before:before+own])
		if err != nil {
			t.Fatalf("%q: the subquery %q: %v", src, src[q.Span.Start:q.Span.End], err)
		}
		var v stmt.Value = stmt.Null{}
		if len(rows) > 0 {
			v = rows[0][0]
			subqueryReads[1].Add(1)
		} else {
			subqueryReads[0].Add(1)
		}
		text = text[:sp.Start] + "?" + text[sp.End:]
		textArgs = slices.Concat(textArgs[:before], []stmt.Value{v}, textArgs[before+own:])
	}

	outer := all[table.Fold()].a
	run := func(sql string, args []stmt.Value) string {
		d := build("")
		is.Equal(t, d.parameters(sql), len(args), sql)
		names, rows, err := d.run(sql, args)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		got := fmt.Sprint(names, "\n", sortedRows(rows))
		if !isSelect {
			_, left, err := d.run("select * from "+quote(outer.name), nil)
			is.NotError(t, err)
			got += fmt.Sprint("\n", show(d.value("select changes()")), "\n", showRows(left))
		}
		return got
	}
	is.Equal(t, s.Parameters(), len(args), src)
	got := run(src, args)
	is.Equal(t, run(text, textArgs), got, src)
	is.Equal(t, run(canonical(s), args), got, src)

	_, _, err := open(t).run(src, args)
	sqliteError(t, src, err, 1, "no such table: "+table.Value)
	for _, q := range subqueries {
		if q.Table.Fold() != table.Fold() {
			_, _, err := build(q.Table.Fold()).run(src, args)
			sqliteError(t, src, err, 1, "no such table: "+q.Table.Value)
		}
	}
	return true
}

// sortedRows of a result, as [showRows] shows them, in the order of their text.
func sortedRows(rows [][]stmt.Value) string {
	var lines []string
	for _, row := range rows {
		lines = append(lines, showRows([][]stmt.Value{row}))
	}
	slices.Sort(lines)
	return strings.Join(lines, "")
}

// whereArgs gives each of the n parameters of the statement src a value of a type that the length of
// the text and the number of the parameter pick, NULL among them, and the values differ from each
// other, so that a value that goes to the wrong condition or the wrong column is seen.
func whereArgs(src string, n int) []stmt.Value {
	values := []stmt.Value{stmt.Integer(math.MinInt64), stmt.Real(-0.5), stmt.Text("a"), stmt.Blob{1}, stmt.Null{}, stmt.Integer(7), stmt.Text("")}
	var args []stmt.Value
	for i := range n {
		v := values[(len(src)+i)%len(values)]
		if i >= len(values) {
			v = stmt.Integer(int64(i) * 1000)
		}
		args = append(args, v)
	}
	return args
}

// around the value v: v, and the values of its type just below and just above it, which a comparison
// with v tells apart from it. NULL has none.
func around(v stmt.Value) []stmt.Value {
	vs := []stmt.Value{v}
	switch v := v.(type) {
	case stmt.Integer:
		if v > math.MinInt64 {
			vs = append(vs, v-1)
		}
		if v < math.MaxInt64 {
			vs = append(vs, v+1)
		}
	case stmt.Real:
		for _, f := range []float64{math.Nextafter(float64(v), math.Inf(-1)), math.Nextafter(float64(v), math.Inf(1))} {
			if !math.IsInf(f, 0) {
				vs = append(vs, stmt.Real(f))
			}
		}
	case stmt.Text:
		vs = append(vs, v+"a")
		if _, n := utf8.DecodeLastRuneInString(string(v)); n > 0 {
			vs = append(vs, v[:len(v)-n])
		}
	case stmt.Blob:
		vs = append(vs, append(slices.Clone(v), 0))
		if len(v) > 0 {
			vs = append(vs, v[:len(v)-1])
		}
	}
	return vs
}

// whereRows of a table whose columns have the values of pools, and whose primary key is at key: every
// combination of the values of the pools that are not nil, or 100 at random, from a seed of src, if
// they are more, and at least least rows, of which those after the combinations repeat them. A column
// without a pool has a number of its own in each row, and the key counts down from the number of
// rows, so that the rows are inserted in the reverse of key order.
func whereRows(src string, pools [][]stmt.Value, key, least int) [][]stmt.Value {
	n := 1
	for i, p := range pools {
		if i != key && p != nil && n <= 100 {
			n *= len(p)
		}
	}
	random := n > 100
	n = max(min(n, 100), least)
	h := fnv.New64a()
	_, _ = h.Write([]byte(src))
	r := rand.New(rand.NewPCG(h.Sum64(), 0x3a1e))
	var rows [][]stmt.Value
	for k := range n {
		row := make([]stmt.Value, len(pools))
		m := k
		for i, p := range pools {
			switch {
			case i == key:
				row[i] = stmt.Integer(n - k)
			case p == nil:
				row[i] = stmt.Integer(1000*k + i)
			case random:
				row[i] = p[r.IntN(len(p))]
			default:
				row[i] = p[m%len(p)]
				m /= len(p)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// insert the rows into the table, each value bound as its type.
func (d *db) insert(table string, rows [][]stmt.Value) {
	d.t.Helper()
	sql := fmt.Sprintf("insert into %v values (%v)", quote(table), strings.TrimSuffix(strings.Repeat("?, ", len(rows[0])), ", "))
	s, err := d.c.Prepare(sql)
	is.NotError(d.t, err)
	defer func() { _ = s.Finalize() }()
	for _, row := range rows {
		is.NotError(d.t, bind(s, row))
		_, err := s.Step()
		is.NotError(d.t, err, sql)
		is.NotError(d.t, s.Reset())
	}
}

// bind the values to the parameters of s, by position, each as its type.
func bind(s *sqlite.Stmt, values []stmt.Value) error {
	for i, v := range values {
		var err error
		switch v := v.(type) {
		case stmt.Integer:
			err = s.BindInt64(i+1, int64(v))
		case stmt.Real:
			err = s.BindDouble(i+1, float64(v))
		case stmt.Text:
			err = s.BindText(i+1, string(v))
		case stmt.Blob:
			err = s.BindBlob(i+1, v)
		case stmt.Null:
			err = s.BindNull(i + 1)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// run the first statement of sql with args bound by position, as [bind] binds them, and give the
// names of its result columns, its rows, and its error.
func (d *db) run(sql string, args []stmt.Value) ([]string, [][]stmt.Value, error) {
	s, _, err := d.c.PrepareFirst(sql)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = s.Finalize() }()
	if err := bind(s, args); err != nil {
		return nil, nil, err
	}
	var names []string
	for i := range s.ColumnCount() {
		names = append(names, s.ColumnName(i))
	}
	var rows [][]stmt.Value
	for {
		ok, err := s.Step()
		if err != nil || !ok {
			return names, rows, err
		}
		row := make([]stmt.Value, s.ColumnCount())
		for i := range row {
			switch s.ColumnType(i) {
			case sqlite.TypeInteger:
				row[i] = stmt.Integer(s.ColumnInt64(i))
			case sqlite.TypeFloat:
				row[i] = stmt.Real(s.ColumnDouble(i))
			case sqlite.TypeText:
				row[i] = stmt.Text(s.ColumnText(i))
			case sqlite.TypeBlob:
				row[i] = stmt.Blob(s.ColumnBlob(i))
			default:
				row[i] = stmt.Null{}
			}
		}
		rows = append(rows, row)
	}
}

// showRows of a result, each value as [show] shows it.
func showRows(rows [][]stmt.Value) string {
	var b strings.Builder
	for _, row := range rows {
		for _, v := range row {
			b.WriteString(show(v) + "; ")
		}
		b.WriteString("\n")
	}
	return b.String()
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
		checkOnce(t, src, s.Columns)
		parameters := 0
		end := b.Start
		for _, v := range s.Values {
			names = append(names, checkTerm(t, src, b, &end, &parameters, v, nil)...)
		}
		names = append(names, checkReturning(t, src, b, end, s.Returning)...)
		is.Equal(t, parameters, s.Parameters(), src)
	case *stmt.Select:
		parameters := 0
		names = checkSelectPositions(t, src, b, s, &parameters, &subqueryState{all: s.Subqueries})
	case *stmt.Update:
		names = []stmt.Name{s.Table}
		if len(s.Set) == 0 {
			t.Fatalf("%q: no assignment", src)
		}
		// Each assignment comes after the table and the assignment before it, its column first, and a ?
		// has the number after that of the ? before it, also in the conditions after them.
		var columns []stmt.Name
		parameters := 0
		end := s.Table.Offset + len(s.Table.Raw)
		for _, a := range s.Set {
			if a.Column.Offset < end {
				t.Fatalf("%q: assignment of %q at %v", src, a.Column.Raw, a.Column.Offset)
			}
			end = a.Column.Offset + len(a.Column.Raw)
			columns = append(columns, a.Column)
			names = append(names, checkTerm(t, src, b, &end, &parameters, a.Value, nil)...)
		}
		checkOnce(t, src, columns)
		names = append(names, columns...)
		subs := &subqueryState{all: s.Subqueries}
		names = append(names, checkConditions(t, src, b, end, &parameters, s.Where, subs)...)
		subs.done(t, src)
		names = append(names, checkReturning(t, src, b, conditionsEnd(end, s.Where), s.Returning)...)
		is.Equal(t, parameters, s.Parameters(), src)
	case *stmt.Delete:
		parameters := 0
		end := s.Table.Offset + len(s.Table.Raw)
		subs := &subqueryState{all: s.Subqueries}
		names = append([]stmt.Name{s.Table}, checkConditions(t, src, b, end, &parameters, s.Where, subs)...)
		subs.done(t, src)
		names = append(names, checkReturning(t, src, b, conditionsEnd(end, s.Where), s.Returning)...)
		is.Equal(t, parameters, s.Parameters(), src)
	case *stmt.DropTable:
		names = []stmt.Name{s.Table}
	case *stmt.CreateIndex:
		// The name of the index comes before the table, and the table before the columns, each after
		// the one before it.
		names = []stmt.Name{s.Index, s.Table}
		if len(s.Columns) == 0 || len(s.Columns) > 2000 {
			t.Fatalf("%q: %v columns", src, len(s.Columns))
		}
		var columns []stmt.Name
		for _, c := range s.Columns {
			columns = append(columns, c.Name)
		}
		checkOnce(t, src, columns)
		names = append(names, columns...)
		for i := 1; i < len(names); i++ {
			if names[i].Offset < names[i-1].Offset+len(names[i-1].Raw) {
				t.Fatalf("%q: name %q at %v", src, names[i].Raw, names[i].Offset)
			}
		}
	case *stmt.DropIndex:
		names = []stmt.Name{s.Index}
	}
	for _, n := range names {
		if n.Offset < b.Start || n.Offset+len(n.Raw) > b.End || src[n.Offset:n.Offset+len(n.Raw)] != n.Raw {
			t.Fatalf("%q: name %q at %v", src, n.Raw, n.Offset)
		}
	}
}

// checkSelectPositions of the select s of src in the bounds b, after as many parameters, whose
// subqueries are those of subs, or of a subquery, without subqueries of its own, for a nil subs: its
// conditions come after its table, the columns of ORDER BY after them, each after the one before it,
// and the LIMIT after them, with a ? numbered after those of the conditions and of their subqueries.
// It moves parameters past those of s, and gives the names of s, those of its subqueries among them.
func checkSelectPositions(t testing.TB, src string, b stmt.Span, s *stmt.Select, parameters *int, subs *subqueryState) []stmt.Name {
	t.Helper()
	start := *parameters
	names := append([]stmt.Name{s.Table}, s.Columns...)
	if s.Columns != nil && len(s.Columns) == 0 {
		t.Fatalf("%q: an empty list of columns", src)
	}
	if subs == nil && (len(s.Columns) > 1 || s.Subqueries != nil) {
		t.Fatalf("%q: a subquery of %v columns and %v subqueries", src, len(s.Columns), len(s.Subqueries))
	}
	names = append(names, checkConditions(t, src, b, s.Table.Offset+len(s.Table.Raw), parameters, s.Where, subs)...)
	if subs != nil {
		subs.done(t, src)
	}
	end := conditionsEnd(s.Table.Offset+len(s.Table.Raw), s.Where)
	if s.OrderBy != nil && len(s.OrderBy) == 0 {
		t.Fatalf("%q: an empty ORDER BY", src)
	}
	var columns []stmt.Name
	for _, c := range s.OrderBy {
		if c.Name.Offset < end {
			t.Fatalf("%q: ORDER BY %q at %v", src, c.Name.Raw, c.Name.Offset)
		}
		end = c.Name.Offset + len(c.Name.Raw)
		columns = append(columns, c.Name)
	}
	checkOnce(t, src, columns)
	names = append(names, columns...)
	if s.Limited() {
		checkTerm(t, src, b, &end, parameters, s.Limit, nil)
		if v, ok := s.Limit.Value.(stmt.Integer); s.Limit.Parameter == 0 && (!ok || v < 0) {
			t.Fatalf("%q: LIMIT %v", src, s.Limit.Value)
		}
	} else if fmt.Sprintf("%#v", s.Limit) != fmt.Sprintf("%#v", stmt.Term{}) {
		t.Fatalf("%q: no LIMIT, and the term %+v", src, s.Limit)
	}
	is.Equal(t, *parameters-start, s.Parameters(), src)
	return names
}

// subqueryState of the subqueries of a statement in [checkPositions]: all of them, and how many of
// them the terms before have named.
type subqueryState struct {
	all  []stmt.Select
	used int
}

// done requires that the terms of the statement named every subquery of it.
func (s *subqueryState) done(t testing.TB, src string) {
	t.Helper()
	if s.used != len(s.all) {
		t.Fatalf("%q: %v subqueries, of which the conditions name %v", src, len(s.all), s.used)
	}
	if s.all != nil && len(s.all) == 0 {
		t.Fatalf("%q: an empty list of subqueries", src)
	}
}

// conditionsEnd gives the end of the conditions where, which come after end, or end if there are
// none.
func conditionsEnd(end int, where []stmt.Condition) int {
	for _, c := range where {
		end = max(end, c.Column.Offset+len(c.Column.Raw), c.Expression.Span.End, c.Value.Span.End)
	}
	return end
}

// checkReturning of the RETURNING clause r of a statement of src in the bounds b, after the end of
// what comes before it: a statement without RETURNING has the zero Returning. The span of a clause
// starts at the keyword RETURNING, after end, and ends at the end of its last column, or of its * for
// a clause without columns, after which only a semicolon of the statement may come. Its columns come
// after the keyword, each after the one before it, and there is one at least if Columns is not nil.
// It gives the names of the columns.
func checkReturning(t testing.TB, src string, b stmt.Span, end int, r stmt.Returning) []stmt.Name {
	t.Helper()
	if !r.Present() {
		if r.Columns != nil {
			t.Fatalf("%q: no RETURNING, and the columns %v", src, r.Columns)
		}
		return nil
	}
	sp := r.Span
	if sp.Start < end || sp.End > b.End || sp.Start+len("returning") > sp.End || !strings.EqualFold(src[sp.Start:sp.Start+len("returning")], "returning") {
		t.Fatalf("%q: RETURNING at %v, after %v", src, sp, end)
	}
	if !onlySpace(src[sp.End:b.End]) {
		t.Fatalf("%q: RETURNING at %v, before %q", src, sp, src[sp.End:b.End])
	}
	if r.Columns == nil {
		if src[sp.End-1] != '*' || !onlySpace(src[sp.Start+len("returning"):sp.End-1]) {
			t.Fatalf("%q: RETURNING * at %v", src, sp)
		}
		return nil
	}
	if len(r.Columns) == 0 {
		t.Fatalf("%q: RETURNING of no column", src)
	}
	end = sp.Start + len("returning")
	for _, c := range r.Columns {
		if c.Offset < end {
			t.Fatalf("%q: RETURNING %q at %v", src, c.Raw, c.Offset)
		}
		end = c.Offset + len(c.Raw)
	}
	if end != sp.End {
		t.Fatalf("%q: RETURNING at %v, its last column ends at %v", src, sp, end)
	}
	return r.Columns
}

// checkOnce that no two of names are one name, as [stmt.Name.Fold] compares them.
func checkOnce(t testing.TB, src string, names []stmt.Name) {
	t.Helper()
	seen := map[string]bool{}
	for _, c := range names {
		if seen[c.Fold()] {
			t.Fatalf("%q: the column %q twice", src, c.Raw)
		}
		seen[c.Fold()] = true
	}
}

// checkTerm v of a statement of src in the bounds b, after the end of what comes before it and after
// as many parameters, where the statement has the subqueries of subs, or none for a nil subs: its
// span is after end and in b, and it is either a ? with the number after parameters, a literal, or
// the next subquery of subs, whose select is inside its parentheses with only spaces and comments
// around it, and whose positions [checkSelectPositions] checks. It moves end and parameters past v,
// and gives the names of its subquery.
func checkTerm(t testing.TB, src string, b stmt.Span, end, parameters *int, v stmt.Term, subs *subqueryState) []stmt.Name {
	t.Helper()
	if v.Span.Start < *end || v.Span.End > b.End || v.Span.Start >= v.Span.End {
		t.Fatalf("%q: value %v at %v", src, v.Value, v.Span)
	}
	*end = v.Span.End
	if v.Subquery > 0 {
		if subs == nil || v.Value != nil || v.Parameter != 0 || v.Subquery != subs.used+1 || v.Subquery > len(subs.all) {
			t.Fatalf("%q: the subquery %v with the value %v and the parameter %v at %v", src, v.Subquery, v.Value, v.Parameter, v.Span)
		}
		subs.used++
		q := &subs.all[v.Subquery-1]
		sp := q.Span
		if src[v.Span.Start] != '(' || src[v.Span.End-1] != ')' || sp.Start <= v.Span.Start || sp.End >= v.Span.End ||
			!onlySpace(src[v.Span.Start+1:sp.Start]) || !onlySpace(src[sp.End:v.Span.End-1]) || !strings.EqualFold(src[sp.Start:sp.Start+len("select")], "select") {
			t.Fatalf("%q: the subquery at %v, its select at %v", src, v.Span, sp)
		}
		return checkSelectPositions(t, src, sp, q, parameters, nil)
	}
	if v.Expression.Present() {
		if v.Value != nil || v.Parameter != 0 {
			t.Fatalf("%q: the expression at %v with the value %v and the parameter %v", src, v.Span, v.Value, v.Parameter)
		}
		e := v.Expression
		if !operatorsOnly(src[v.Span.Start:e.Span.Start], "") || !operatorsOnly(src[e.Span.End:v.Span.End], "") {
			t.Fatalf("%q: the expression at %v in the term at %v", src, e.Span, v.Span)
		}
		*end = v.Span.Start
		names := checkExpression(t, src, v.Span, end, parameters, e)
		*end = v.Span.End
		return names
	}
	// The value of a literal at its span is checked against SQLite in checkMeaning.
	if v.Parameter > 0 {
		*parameters++
		if v.Parameter != *parameters || v.Value != nil || src[v.Span.Start:v.Span.End] != "?" {
			t.Fatalf("%q: parameter %v with the value %v at %v", src, v.Parameter, v.Value, v.Span)
		}
	}
	if v.Parameter == 0 && v.Value == nil {
		t.Fatalf("%q: no value and no parameter", src)
	}
	return nil
}

// checkConditions of a WHERE of a statement of src in the bounds b, after the end of what comes
// before them and after as many parameters, with the subqueries of subs, or none for a nil subs: each
// condition comes after the one before it, its column first, or its value first if it says
// ValueFirst, which only a comparison says, and a ? has the number after that of the ? before it,
// also in a subquery. It gives the names of their columns, and those of their subqueries.
func checkConditions(t testing.TB, src string, b stmt.Span, end int, parameters *int, where []stmt.Condition, subs *subqueryState) []stmt.Name {
	t.Helper()
	if where != nil && len(where) == 0 {
		t.Fatalf("%q: an empty list of conditions", src)
	}
	var names []stmt.Name
	column := func(c stmt.Condition) {
		if c.Expression.Present() {
			if c.Column != (stmt.Name{}) || c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull || !c.Expression.HasColumn() {
				t.Fatalf("%q: the condition on %q and the expression at %v, %v", src, c.Column.Raw, c.Expression.Span, c.Operator)
			}
			names = append(names, checkExpression(t, src, b, &end, parameters, c.Expression)...)
			return
		}
		names = append(names, c.Column)
		if c.Column.Offset < end {
			t.Fatalf("%q: condition on %q at %v", src, c.Column.Raw, c.Column.Offset)
		}
		end = c.Column.Offset + len(c.Column.Raw)
	}
	for _, c := range where {
		// The two sides of a comparison are never both without a column.
		if c.Operator != stmt.OpIsNull && c.Operator != stmt.OpIsNotNull && !c.Expression.Present() && c.Column == (stmt.Name{}) {
			t.Fatalf("%q: a comparison without a column", src)
		}
		switch c.Operator {
		case stmt.OpIsNull, stmt.OpIsNotNull:
			column(c)
			if v := c.Value; v.Value != nil || v.Parameter != 0 || v.Span != (stmt.Span{}) || c.ValueFirst {
				t.Fatalf("%q: %v with the value %+v, value first %v", src, c.Operator, v, c.ValueFirst)
			}
			continue
		case stmt.OpEqual, stmt.OpNotEqual, stmt.OpLess, stmt.OpLessOrEqual, stmt.OpGreater, stmt.OpGreaterOrEqual:
		default:
			t.Fatalf("%q: the operator %v", src, c.Operator)
		}
		if c.ValueFirst {
			names = append(names, checkTerm(t, src, b, &end, parameters, c.Value, subs)...)
			column(c)
			continue
		}
		column(c)
		names = append(names, checkTerm(t, src, b, &end, parameters, c.Value, subs)...)
	}
	return names
}

// checkExpression e of a statement of src in the bounds b, after the end of what comes before it and
// after as many parameters: its span is after end and in b, its operands come in the order of the
// text inside its span, with only parentheses, spaces, and comments around them and its operator
// between them, a ? has the number after parameters, and a name has its raw text at its offset. It
// moves end and parameters past e, and gives the names of its columns.
func checkExpression(t testing.TB, src string, b stmt.Span, end, parameters *int, e stmt.Expression) []stmt.Name {
	t.Helper()
	sp := e.Span
	if sp.Start < *end || sp.End > b.End || sp.Start >= sp.End {
		t.Fatalf("%q: the expression %v at %v, after %v", src, e.Kind, sp, *end)
	}
	var names []stmt.Name
	switch e.Kind {
	case stmt.ExprValue:
		_, integer := e.Value.(stmt.Integer)
		_, real := e.Value.(stmt.Real)
		if !integer && !real || e.Operands != nil || e.Parameter != 0 || e.Column != (stmt.Name{}) {
			t.Fatalf("%q: the literal %+v", src, e)
		}
	case stmt.ExprParameter:
		*parameters++
		if e.Parameter != *parameters || src[sp.Start:sp.End] != "?" || e.Operands != nil || e.Value != nil || e.Column != (stmt.Name{}) {
			t.Fatalf("%q: the parameter %+v, after %v", src, e, *parameters-1)
		}
	case stmt.ExprColumn:
		if e.Column.Offset != sp.Start || src[sp.Start:sp.End] != e.Column.Raw || e.Operands != nil || e.Value != nil || e.Parameter != 0 {
			t.Fatalf("%q: the column %+v", src, e)
		}
		names = append(names, e.Column)
	case stmt.ExprNegate:
		if len(e.Operands) != 1 || e.Value != nil || e.Parameter != 0 || e.Column != (stmt.Name{}) ||
			e.Operands[0].Kind == stmt.ExprValue || e.Operands[0].Kind == stmt.ExprNegate {
			t.Fatalf("%q: the negation %+v", src, e)
		}
		x := e.Operands[0]
		if !operatorsOnly(src[sp.Start:x.Span.Start], "-") || !operatorsOnly(src[x.Span.End:sp.End], "") {
			t.Fatalf("%q: the negation at %v of the operand at %v", src, sp, x.Span)
		}
		*end = sp.Start
		names = checkExpression(t, src, sp, end, parameters, x)
	case stmt.ExprAdd, stmt.ExprSubtract, stmt.ExprMultiply, stmt.ExprDivide:
		if len(e.Operands) != 2 || e.Value != nil || e.Parameter != 0 || e.Column != (stmt.Name{}) {
			t.Fatalf("%q: the operator %+v", src, e)
		}
		x, y := e.Operands[0], e.Operands[1]
		if !operatorsOnly(src[sp.Start:x.Span.Start], "") || !operatorsOnly(src[x.Span.End:y.Span.Start], e.Kind.String()) ||
			!operatorsOnly(src[y.Span.End:sp.End], "") {
			t.Fatalf("%q: the operator %v at %v of the operands at %v and %v", src, e.Kind, sp, x.Span, y.Span)
		}
		*end = sp.Start
		names = checkExpression(t, src, sp, end, parameters, x)
		names = append(names, checkExpression(t, src, sp, end, parameters, y)...)
		if e.Kind == stmt.ExprDivide && y.Kind == stmt.ExprValue && compareValues(y.Value, stmt.Integer(0)) == 0 {
			t.Fatalf("%q: a division by zero", src)
		}
	default:
		t.Fatalf("%q: the expression of the kind %v", src, e.Kind)
	}
	*end = sp.End
	return names
}

// operatorsOnly reports whether text, between two operands or around one, holds only parentheses,
// spaces, and comments, and the one token op if it is not "".
func operatorsOnly(text, op string) bool {
	var others []string
	for _, tk := range token.Tokens(text) {
		switch tk.Kind {
		case token.Space, token.Comment, token.LP, token.RP:
		default:
			others = append(others, tk.Text(text))
		}
	}
	if op == "" {
		return others == nil
	}
	return len(others) == 1 && others[0] == op
}

// statementParts of the statement s: its table, its conditions, and its subqueries, for a select, an
// update, or a delete, and nothing for another statement.
func statementParts(s stmt.Statement) (stmt.Name, []stmt.Condition, []stmt.Select) {
	switch s := s.(type) {
	case *stmt.Select:
		return s.Table, s.Where, s.Subqueries
	case *stmt.Update:
		return s.Table, s.Where, s.Subqueries
	case *stmt.Delete:
		return s.Table, s.Where, s.Subqueries
	}
	return stmt.Name{}, nil, nil
}

// expressionsOf the statement s: every expression of its assignments and its conditions, and those of
// the conditions of its subqueries, each with the name of the table whose rows it reads.
func expressionsOf(s stmt.Statement) ([]stmt.Expression, []stmt.Name) {
	table, where, subqueries := statementParts(s)
	var expressions []stmt.Expression
	var tables []stmt.Name
	if u, ok := s.(*stmt.Update); ok {
		for _, a := range u.Set {
			if a.Value.Expression.Present() {
				expressions, tables = append(expressions, a.Value.Expression), append(tables, table)
			}
		}
	}
	add := func(table stmt.Name, where []stmt.Condition) {
		for _, c := range where {
			for _, e := range []stmt.Expression{c.Expression, c.Value.Expression} {
				if e.Present() {
					expressions, tables = append(expressions, e), append(tables, table)
				}
			}
		}
	}
	add(table, where)
	for _, q := range subqueries {
		add(q.Table, q.Where)
	}
	return expressions, tables
}

// nodesOf e: e and every node under it, each before its operands.
func nodesOf(e stmt.Expression) []stmt.Expression {
	nodes := []stmt.Expression{e}
	for _, o := range e.Operands {
		nodes = append(nodes, nodesOf(o)...)
	}
	return nodes
}

// expressionSamples are the values of the columns and the parameters in [checkExpressions]: small
// numbers of both types that tell the groupings of the operators apart, zero, an edge of 64 bits,
// NULL, and a text that SQLite reads as a number.
var expressionSamples = []stmt.Value{stmt.Integer(7), stmt.Integer(-3), stmt.Real(2.5), stmt.Integer(11), stmt.Integer(0),
	stmt.Real(-0.25), stmt.Integer(2), stmt.Integer(math.MaxInt64), stmt.Null{}, stmt.Text("12"), stmt.Integer(5)}

// checkExpressions of the supported statement s that src holds, if it has an arithmetic expression,
// and report whether it has one; such a statement has no other check of its meaning. Each literal of
// an expression has the value that SQLite gives its text. Each expression gives in SQLite, for every
// row of a table of sample values and with sample arguments, what its text gives in which every
// operator has parentheses of its own, so that SQLite reads the expression of src with the tree of s.
// And src gives on such tables the result of the canonical text of s, the same changes, and the same
// rows left in the table of the statement.
func checkExpressions(t testing.TB, open func(testing.TB) *db, src string, s stmt.Statement) bool {
	t.Helper()
	expressions, tables := expressionsOf(s)
	if expressions == nil {
		return false
	}
	for _, e := range expressions {
		for _, n := range nodesOf(e) {
			if n.Kind == stmt.ExprValue {
				lit := src[n.Span.Start:n.Span.End]
				if got, want := show(open(t).value("select "+lit)), show(n.Value); got != want {
					t.Fatalf("%q: the value is %v, SQLite gives %v for %q", src, want, got, lit)
				}
			}
		}
	}

	// A table for each table that the statement reads, with a column of the type ANY for each name
	// that the statement says anywhere, so that each name is a column of each table.
	table, where, subqueries := statementParts(s)
	all := []stmt.Name{table}
	var named []stmt.Name
	switch s := s.(type) {
	case *stmt.Select:
		named = append(slices.Clone(s.Columns), orderNames(s.OrderBy)...)
	case *stmt.Update:
		for _, a := range s.Set {
			named = append(named, a.Column)
		}
		named = append(named, s.Returning.Columns...)
	case *stmt.Delete:
		named = slices.Clone(s.Returning.Columns)
	}
	conditions := slices.Clone(where)
	for _, q := range subqueries {
		all = append(all, q.Table)
		named = append(append(named, q.Columns...), orderNames(q.OrderBy)...)
		conditions = append(conditions, q.Where...)
	}
	for _, c := range conditions {
		if c.Column != (stmt.Name{}) {
			named = append(named, c.Column)
		}
	}
	for _, e := range expressions {
		for _, n := range nodesOf(e) {
			if n.Kind == stmt.ExprColumn {
				named = append(named, n.Column)
			}
		}
	}
	type built struct {
		a    anyTable
		rows [][]stmt.Value
	}
	var folds []string
	byFold := map[string]built{}
	for _, tb := range all {
		if _, ok := byFold[tb.Fold()]; ok {
			continue
		}
		a := newAnyTable(swapCase(tb.Value), named)
		pools := make([][]stmt.Value, len(a.names))
		for i := range pools {
			if i != a.key {
				// Each column starts at a sample of its own, so that two columns differ in a row.
				k := i % len(expressionSamples)
				pools[i] = slices.Concat(expressionSamples[k:], expressionSamples[:k])
			}
		}
		folds = append(folds, tb.Fold())
		byFold[tb.Fold()] = built{a: a, rows: whereRows(src+" "+tb.Fold(), pools, a.key, 20)}
	}
	build := func() *db {
		d := open(t)
		for _, f := range folds {
			d.must(byFold[f].a.create(byFold[f].a.names))
			d.insert(byFold[f].a.name, byFold[f].rows)
		}
		return d
	}

	var args []stmt.Value
	for i := range s.Parameters() {
		args = append(args, expressionSamples[(len(src)+3*i)%len(expressionSamples)])
	}
	selects := slices.Clone(subqueries)
	if sel, ok := s.(*stmt.Select); ok {
		selects = append(selects, *sel)
	}
	for _, q := range selects {
		if q.Limit.Parameter > 0 {
			args[q.Limit.Parameter-1] = limitArg(src)
		}
	}

	// The tree: each expression of src and its canonical text give the same value in each row, with
	// the arguments of its own parameters, which come in the same order in the two texts.
	d := build()
	for i, e := range expressions {
		var own []stmt.Value
		for _, n := range nodesOf(e) {
			if n.Kind == stmt.ExprParameter {
				own = append(own, args[n.Parameter-1])
			}
		}
		sql := fmt.Sprintf("select %v, %v from %v", src[e.Span.Start:e.Span.End], parenthesized(e), quote(byFold[tables[i].Fold()].a.name))
		_, rows, err := d.run(sql, slices.Concat(own, own))
		if err != nil {
			t.Fatalf("%q: %q: %v", src, sql, err)
		}
		if len(rows) == 0 {
			t.Fatalf("%q: %q gives no row", src, sql)
		}
		for _, row := range rows {
			if show(row[0]) != show(row[1]) {
				t.Fatalf("%q: %q gives %v for the text and %v for the tree", src, sql, show(row[0]), show(row[1]))
			}
		}
	}

	// The statement and its canonical text, which give the same error for a subquery of *.
	_, isSelect := s.(*stmt.Select)
	star := slices.ContainsFunc(subqueries, func(q stmt.Select) bool { return q.Columns == nil })
	run := func(sql string) string {
		d := build()
		if !star {
			is.Equal(t, d.parameters(sql), len(args), sql)
		}
		names, rows, err := d.run(sql, args)
		if err != nil && star {
			// A subquery of * reads a table of more than one column here.
			return err.Error()
		}
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		got := fmt.Sprint(names, "\n", sortedRows(rows))
		if !isSelect {
			_, left, err := d.run("select * from "+quote(byFold[table.Fold()].a.name), nil)
			is.NotError(t, err)
			got += fmt.Sprint("\n", show(d.value("select changes()")), "\n", showRows(left))
		}
		return got
	}
	is.Equal(t, run(canonical(s)), run(src), src)
	return true
}

// orderNames of the columns of an ORDER BY.
func orderNames(columns []stmt.OrderColumn) []stmt.Name {
	var names []stmt.Name
	for _, c := range columns {
		names = append(names, c.Name)
	}
	return names
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

func TestOperator_String(t *testing.T) {
	t.Run("should give each operator as SQL writes it, and nothing for no operator", func(t *testing.T) {
		var got []string
		for o := stmt.Operator(0); o <= stmt.OpIsNotNull+1; o++ {
			got = append(got, o.String())
		}
		is.Equal(t, fmt.Sprint([]string{"", "=", "!=", "<", "<=", ">", ">=", "IS NULL", "IS NOT NULL", ""}), fmt.Sprint(got))
	})
}

func TestOperator_Swapped(t *testing.T) {
	t.Run("should swap < and >, <= and >=, keep = and !=, and give no operator for a test for NULL or no operator", func(t *testing.T) {
		var got []string
		for o := stmt.Operator(0); o <= stmt.OpIsNotNull+1; o++ {
			got = append(got, o.Swapped().String())
		}
		is.Equal(t, fmt.Sprint([]string{"", "=", "!=", ">", ">=", "<", "<=", "", "", ""}), fmt.Sprint(got))
	})

	t.Run("should give the operator back when swapped twice", func(t *testing.T) {
		for o := stmt.OpEqual; o <= stmt.OpGreaterOrEqual; o++ {
			is.Equal(t, o, o.Swapped().Swapped())
		}
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
