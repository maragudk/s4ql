package core

import (
	"errors"
	"fmt"
	"iter"
	"strings"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sqlite"
)

// cacheVersion is the version of the layout of the cache that this package makes and reads: the
// table s4ql_cache, and one table for each table of the database.
const cacheVersion = 0

// conn is a connection to the cache, with the prepared statements and the tables that the
// transaction that uses it has looked up. It is used by one goroutine at a time.
type conn struct {
	c *sqlite.Conn
	// stmts that are prepared and at rest, by their SQL.
	stmts map[string]*sqlite.Stmt
	// tables that were looked up, by their folded name. A nil table does not exist.
	tables map[string]*table

	// users is the number of calls that use the connection now, and closed whether [DB.Close] has
	// closed it. [DB.mu] guards both.
	users  int
	closed bool
}

// openConn to the cache at path, with the busy timeout in milliseconds, after which SQLite reports
// a lock that it waited for as "busy".
func openConn(path string, busyTimeout int64) (*conn, error) {
	c, err := sqlite.Open(path)
	if err != nil {
		return nil, err
	}
	cn := &conn{c: c, stmts: map[string]*sqlite.Stmt{}, tables: map[string]*table{}}
	if err := cn.exec(fmt.Sprint("pragma busy_timeout = ", busyTimeout)); err != nil {
		_ = c.Close()
		return nil, err
	}
	return cn, nil
}

// prepare the statement sql, or take it from those at rest. The caller hands it back with
// [conn.rest].
func (c *conn) prepare(sql string) (*sqlite.Stmt, error) {
	if s, ok := c.stmts[sql]; ok {
		delete(c.stmts, sql)
		return s, nil
	}
	return c.c.Prepare(sql)
}

// rest the statement s of the SQL sql: reset it and keep it for the next [conn.prepare], or
// finalize it if one for the same SQL is at rest already. It returns the error of the statement's
// last step, if that failed.
func (c *conn) rest(sql string, s *sqlite.Stmt) error {
	if err := s.Reset(); err != nil {
		_ = s.Finalize()
		return err
	}
	if _, ok := c.stmts[sql]; ok {
		return s.Finalize()
	}
	c.stmts[sql] = s
	return nil
}

// forget every statement at rest and every table that was looked up, as after a change of the
// schema and at the end of a transaction.
func (c *conn) forget() {
	for sql, s := range c.stmts {
		_ = s.Finalize()
		delete(c.stmts, sql)
	}
	clear(c.tables)
}

// close the connection.
func (c *conn) close() error {
	c.forget()
	return c.c.Close()
}

// bind args to the parameters of s, from the first on: an int64, a string, a [*s4qlpb.Value], or
// nil for NULL.
func bind(s *sqlite.Stmt, args ...any) error {
	for i, arg := range args {
		var err error
		switch arg := arg.(type) {
		case nil:
			err = s.BindNull(i + 1)
		case int64:
			err = s.BindInt64(i+1, arg)
		case string:
			err = s.BindText(i+1, arg)
		case *s4qlpb.Value:
			switch arg.WhichKind() {
			case s4qlpb.Value_Integer_case:
				err = s.BindInt64(i+1, arg.GetInteger())
			case s4qlpb.Value_Null_case:
				err = s.BindNull(i + 1)
			default:
				err = invalidf("a value of kind %v cannot be stored", arg.WhichKind())
			}
		default:
			err = fmt.Errorf("cannot bind a %T", arg)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// exec the statement sql with args as [bind] takes them, and discard its rows.
func (c *conn) exec(sql string, args ...any) error {
	_, err := c.count(sql, args...)
	return err
}

// count the rows that the statement sql returns, with args as [bind] takes them.
func (c *conn) count(sql string, args ...any) (int, error) {
	s, err := c.prepare(sql)
	if err != nil {
		return 0, err
	}
	if err := bind(s, args...); err != nil {
		_ = c.rest(sql, s)
		return 0, err
	}
	var n int
	for {
		ok, err := s.Step()
		if err != nil {
			_ = s.Finalize()
			return 0, err
		}
		if !ok {
			return n, c.rest(sql, s)
		}
		n++
	}
}

// query the statement sql with args as [bind] takes them, and call row for each of its rows.
func (c *conn) query(sql string, row func(s *sqlite.Stmt) error, args ...any) error {
	s, err := c.prepare(sql)
	if err != nil {
		return err
	}
	if err := bind(s, args...); err != nil {
		_ = c.rest(sql, s)
		return err
	}
	for {
		ok, err := s.Step()
		if err != nil {
			_ = s.Finalize()
			return err
		}
		if !ok {
			return c.rest(sql, s)
		}
		if err := row(s); err != nil {
			_ = c.rest(sql, s)
			return err
		}
	}
}

// setUp the cache on the write connection: the write-ahead log, how hard SQLite syncs, and the
// table s4ql_cache with its one row. It is the first statement on the first connection to the
// cache, so it is what recovers the write-ahead log after a crash. It refuses a cache of another
// version with [ErrCacheVersion].
//
// With synchronous = normal, SQLite syncs the write-ahead log only at a checkpoint. After a power
// loss the cache can then lack its last commits, and is never corrupt
// (https://sqlite.org/pragma.html#pragma_synchronous). A cache that is behind is what catching up
// from the log is for, so the sync at every commit that synchronous = full adds buys nothing.
func (c *conn) setUp(syncOff bool) error {
	var mode string
	if err := c.query("pragma journal_mode = wal", func(s *sqlite.Stmt) error {
		mode = s.ColumnText(0)
		return nil
	}); err != nil {
		return err
	}
	if mode != "wal" {
		return fmt.Errorf("the journal mode of the cache is %q, not wal", mode)
	}
	synchronous := "pragma synchronous = normal"
	if syncOff {
		synchronous = "pragma synchronous = off"
	}
	if err := c.exec(synchronous); err != nil {
		return err
	}

	if err := c.exec(`
		create table if not exists main.s4ql_cache (
			id integer primary key check (id = 1),
			version integer not null,
			head text not null
		) strict`); err != nil {
		return err
	}
	// A cache gets its one row when it is new. A cache that has tables and no row is not one that
	// this package made, and giving it a position would be a guess.
	rows, err := c.count("select 1 from main.s4ql_cache")
	if err != nil {
		return err
	}
	if rows == 0 {
		tables, err := c.count("select 1 from main.sqlite_schema where name != 's4ql_cache'")
		if err != nil {
			return err
		}
		if tables > 0 {
			return fmt.Errorf("%w: it records no position, and holds %v tables and indexes", ErrCacheVersion, tables)
		}
		if err := c.exec("insert into main.s4ql_cache (id, version, head) values (1, ?, ?)", int64(cacheVersion), format.EmptyLog.String()); err != nil {
			return err
		}
	}
	if _, err := c.position(); err != nil {
		return err
	}
	return nil
}

// position that the cache records: the name of the last commit object that it has applied, or
// [format.EmptyLog].
func (c *conn) position() (format.Name, error) {
	var name format.Name
	var rows int
	err := c.query("select version, head from main.s4ql_cache", func(s *sqlite.Stmt) error {
		rows++
		if version := s.ColumnInt64(0); version != cacheVersion {
			return fmt.Errorf("%w: it has version %v, and this version of s4ql reads version %v", ErrCacheVersion, version, cacheVersion)
		}
		var err error
		if name, err = format.ParseName(s.ColumnText(1)); err != nil {
			return fmt.Errorf("%w: its position: %v", ErrCacheVersion, err)
		}
		return nil
	})
	if err != nil {
		return format.Name{}, err
	}
	if rows != 1 {
		return format.Name{}, fmt.Errorf("%w: it records %v positions", ErrCacheVersion, rows)
	}
	return name, nil
}

// setPosition that the cache records, inside the transaction that applies the commit object.
func (c *conn) setPosition(name format.Name) error {
	n, err := c.count("update main.s4ql_cache set head = ? where id = 1 returning 1", name.String())
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: it records %v positions", ErrCacheVersion, n)
	}
	return nil
}

// table of the database, as the cache holds it.
type table struct {
	// name as the table was created.
	name    string
	columns []column
	// key holds the positions in columns of the primary-key columns, in key order.
	key []int
	// inKey reports for each column whether it is a primary-key column.
	inKey []bool
}

type column struct {
	name string
	typ  s4qlpb.Type
}

// createTable that describes t.
func (t *table) createTable() *s4qlpb.CreateTable {
	ct := s4qlpb.CreateTable_builder{Table: new(t.name)}
	for _, c := range t.columns {
		ct.Columns = append(ct.Columns, s4qlpb.Column_builder{Name: new(c.name), Type: new(c.typ)}.Build())
	}
	for _, position := range t.key {
		ct.PrimaryKey = append(ct.PrimaryKey, uint32(position))
	}
	return ct.Build()
}

// newTable from the data of a create table, which [checkCreateTable] has accepted.
func newTable(ct *s4qlpb.CreateTable) *table {
	t := &table{name: ct.GetTable(), inKey: make([]bool, len(ct.GetColumns()))}
	for _, c := range ct.GetColumns() {
		t.columns = append(t.columns, column{name: c.GetName(), typ: c.GetType()})
	}
	for _, position := range ct.GetPrimaryKey() {
		t.key = append(t.key, int(position))
		t.inKey[position] = true
	}
	return t
}

// quote the name of a table or a column as an SQL identifier. The name is valid UTF-8 without the
// character NUL, and may hold anything else, so every character but the double quote stands for
// itself, and a double quote is doubled.
func quote(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// fold the ASCII letters of a name to lowercase, and nothing else, as SQLite does when it compares
// the names of tables and columns.
func fold(name string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, name)
}

// reserved reports whether the name of a table is one that the database may not use: SQLite keeps
// the names that start with "sqlite_" for itself, and the cache's own tables have names that start
// with "s4ql_", in any case of the letters.
func reserved(name string) bool {
	folded := fold(name)
	return strings.HasPrefix(folded, "s4ql_") || strings.HasPrefix(folded, "sqlite_")
}

// typeSQL is the declared type of a column in the cache.
func typeSQL(typ s4qlpb.Type) (string, error) {
	switch typ {
	case s4qlpb.Type_TYPE_INT:
		return "int", nil
	}
	return "", invalidf("a column of type %v", typ)
}

// createSQL is the statement that creates the table t in the cache: a strict table without a
// rowid, with the names and the columns of the database's table.
func (t *table) createSQL() (string, error) {
	var b strings.Builder
	b.WriteString("create table main.")
	b.WriteString(quote(t.name))
	b.WriteString(" (")
	for _, c := range t.columns {
		typ, err := typeSQL(c.typ)
		if err != nil {
			return "", err
		}
		b.WriteString(quote(c.name))
		b.WriteString(" ")
		b.WriteString(typ)
		b.WriteString(", ")
	}
	b.WriteString("primary key (")
	b.WriteString(t.columnList(t.key))
	b.WriteString(")) strict, without rowid")
	return b.String(), nil
}

// columnList of the columns at positions, quoted and separated by commas.
func (t *table) columnList(positions []int) string {
	names := make([]string, len(positions))
	for i, position := range positions {
		names[i] = quote(t.columns[position].name)
	}
	return strings.Join(names, ", ")
}

// keyCondition compares the primary-key columns of the table, under the alias, with the
// parameters from the first on, in key order.
func (t *table) keyCondition(alias string) string {
	terms := make([]string, len(t.key))
	for i, position := range t.key {
		terms[i] = fmt.Sprint(alias, ".", quote(t.columns[position].name), " = ?", i+1)
	}
	return strings.Join(terms, " and ")
}

// all positions of the columns of the table.
func (t *table) all() []int {
	positions := make([]int, len(t.columns))
	for i := range positions {
		positions[i] = i
	}
	return positions
}

// placeholders for n parameters, from the first on.
func placeholders(n int) string {
	marks := make([]string, n)
	for i := range marks {
		marks[i] = fmt.Sprint("?", i+1)
	}
	return strings.Join(marks, ", ")
}

// checkValue v for the column at position of the table: its storage class is the one of the
// column's type, or it is NULL in a column outside the primary key. A strict table of SQLite does
// not refuse every other value: it converts the text '1' and the real 1.0 to an integer.
func (t *table) checkValue(position int, v *s4qlpb.Value) error {
	c := t.columns[position]
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		if c.typ == s4qlpb.Type_TYPE_INT {
			return nil
		}
	case s4qlpb.Value_Null_case:
		if !t.inKey[position] {
			return nil
		}
		return invalidf("NULL in the primary-key column %q of the table %q", c.name, t.name)
	}
	return invalidf("a value of kind %v in the column %q of the table %q, whose type is %v", v.WhichKind(), c.name, t.name, c.typ)
}

// checkRow has a valid value for each column of the table.
func (t *table) checkRow(row []*s4qlpb.Value) error {
	if len(row) != len(t.columns) {
		return invalidf("a row of %v values for the table %q, which has %v columns", len(row), t.name, len(t.columns))
	}
	for position, v := range row {
		if err := t.checkValue(position, v); err != nil {
			return err
		}
	}
	return nil
}

// checkKey has a valid value for each primary-key column of the table, in key order.
func (t *table) checkKey(key []*s4qlpb.Value) error {
	if len(key) != len(t.key) {
		return invalidf("a key of %v values for the table %q, whose primary key has %v columns", len(key), t.name, len(t.key))
	}
	for i, v := range key {
		if err := t.checkValue(t.key[i], v); err != nil {
			return err
		}
	}
	return nil
}

// keyOf the row: the values of its primary-key columns, in key order.
func (t *table) keyOf(row []*s4qlpb.Value) []*s4qlpb.Value {
	key := make([]*s4qlpb.Value, len(t.key))
	for i, position := range t.key {
		key[i] = row[position]
	}
	return key
}

// values as the arguments of [bind].
func values(vs []*s4qlpb.Value) []any {
	args := make([]any, len(vs))
	for i, v := range vs {
		args[i] = v
	}
	return args
}

// readRow of n values from the current row of s, from the column at first on.
func readRow(s *sqlite.Stmt, first, n int) ([]*s4qlpb.Value, error) {
	row := make([]*s4qlpb.Value, n)
	for i := range row {
		switch typ := s.ColumnType(first + i); typ {
		case sqlite.TypeInteger:
			row[i] = s4qlpb.Value_builder{Integer: new(s.ColumnInt64(first + i))}.Build()
		case sqlite.TypeNull:
			row[i] = s4qlpb.Value_builder{Null: &s4qlpb.Null{}}.Build()
		default:
			return nil, fmt.Errorf("%w: the cache holds a value of type %v", ErrCacheVersion, typ)
		}
	}
	return row, nil
}

// lookUp the table of the database with the given name, compared as SQLite compares names, or nil
// if there is none.
func (c *conn) lookUp(name string) (*table, error) {
	folded := fold(name)
	if t, ok := c.tables[folded]; ok {
		return t, nil
	}
	t, err := c.load(name)
	if err != nil {
		return nil, err
	}
	c.tables[folded] = t
	return t, nil
}

// load the table with the given name from the schema of the cache, or nil if there is none. The
// cache's own tables are not tables of the database.
func (c *conn) load(name string) (*table, error) {
	if reserved(name) {
		return nil, nil
	}
	var t *table
	if err := c.query("select name from main.sqlite_schema where type = 'table' and name = ? collate nocase", func(s *sqlite.Stmt) error {
		t = &table{name: s.ColumnText(0)}
		return nil
	}, name); err != nil {
		return nil, err
	}
	if t == nil {
		return nil, nil
	}

	// The position of a column in the primary key, from 1, with its position in the table.
	keyed := map[int64]int{}
	if err := c.query("select name, type, pk from pragma_table_info(?, 'main') order by cid", func(s *sqlite.Stmt) error {
		if typ := s.ColumnText(1); fold(typ) != "int" {
			return fmt.Errorf("%w: the column %q of its table %q has the type %q", ErrCacheVersion, s.ColumnText(0), t.name, typ)
		}
		if pk := s.ColumnInt64(2); pk > 0 {
			keyed[pk] = len(t.columns)
		}
		t.columns = append(t.columns, column{name: s.ColumnText(0), typ: s4qlpb.Type_TYPE_INT})
		return nil
	}, t.name); err != nil {
		return nil, err
	}
	t.inKey = make([]bool, len(t.columns))
	for i := range len(keyed) {
		position, ok := keyed[int64(i+1)]
		if !ok {
			return nil, fmt.Errorf("%w: its table %q has no column %v of its primary key", ErrCacheVersion, t.name, i+1)
		}
		t.key = append(t.key, position)
		t.inKey[position] = true
	}
	if len(t.columns) == 0 || len(t.key) == 0 {
		return nil, fmt.Errorf("%w: its table %q has %v columns and a primary key of %v", ErrCacheVersion, t.name, len(t.columns), len(t.key))
	}
	return t, nil
}

// list the tables of the database, in the order of their names as bytes.
func (c *conn) list() ([]*table, error) {
	var names []string
	if err := c.query(`select name from main.sqlite_schema where type = 'table' order by name collate binary`, func(s *sqlite.Stmt) error {
		if name := s.ColumnText(0); !reserved(name) {
			names = append(names, name)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	tables := make([]*table, 0, len(names))
	for _, name := range names {
		t, err := c.lookUp(name)
		if err != nil {
			return nil, err
		}
		if t == nil {
			return nil, fmt.Errorf("%w: its table %q is listed and cannot be found", ErrCacheVersion, name)
		}
		tables = append(tables, t)
	}
	return tables, nil
}

// get the row of the table with the given primary key, which [table.checkKey] has accepted, or
// nil if there is none.
func (c *conn) get(t *table, key []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	var row []*s4qlpb.Value
	sql := fmt.Sprint("select * from main.", quote(t.name), " as t where ", t.keyCondition("t"))
	err := c.query(sql, func(s *sqlite.Stmt) error {
		var err error
		row, err = readRow(s, 0, len(t.columns))
		return err
	}, values(key)...)
	return row, err
}

// scan the rows of the table in the order of its primary key. The statement is given back also when
// a panic or [runtime.Goexit] in the loop over the scan unwinds through it: a statement that stays
// stepped would hold its transaction's snapshot and locks, and the connection could not close.
func (c *conn) scan(t *table) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		sql := fmt.Sprint("select * from main.", quote(t.name), " order by ", t.columnList(t.key))
		s, err := c.prepare(sql)
		if err != nil {
			yield(nil, err)
			return
		}
		// s is nil once it is given back.
		defer func() {
			if s != nil {
				_ = c.rest(sql, s)
			}
		}()
		for {
			ok, err := s.Step()
			if err != nil {
				_ = s.Finalize()
				s = nil
				yield(nil, err)
				return
			}
			if !ok {
				err := c.rest(sql, s)
				s = nil
				if err != nil {
					yield(nil, err)
				}
				return
			}
			row, err := readRow(s, 0, len(t.columns))
			if err != nil {
				_ = c.rest(sql, s)
				s = nil
				yield(nil, err)
				return
			}
			if !yield(row, nil) {
				return
			}
		}
	}
}

// upsert the row into the table: insert it, or replace the row with the same primary key. The
// table has no triggers and no foreign keys, so replacing a row runs nothing else.
func (c *conn) upsert(t *table, row []*s4qlpb.Value) error {
	sql := fmt.Sprint("insert or replace into main.", quote(t.name), " (", t.columnList(t.all()), ") values (", placeholders(len(t.columns)), ")")
	return c.exec(sql, values(row)...)
}

// delete the row of the table with the given primary key, and report whether there was one.
func (c *conn) delete(t *table, key []*s4qlpb.Value) (bool, error) {
	sql := fmt.Sprint("delete from main.", quote(t.name), " as t where ", t.keyCondition("t"), " returning 1")
	n, err := c.count(sql, values(key)...)
	return n > 0, err
}

// create the table in the cache.
func (c *conn) create(t *table) error {
	sql, err := t.createSQL()
	if err != nil {
		return err
	}
	// A statement that changes the schema is not kept, and those at rest are prepared again.
	c.forget()
	if err := c.c.Exec(sql); err != nil {
		return err
	}
	c.forget()
	return nil
}

// drop the table from the cache, with its rows.
func (c *conn) drop(t *table) error {
	c.forget()
	if err := c.c.Exec(fmt.Sprint("drop table main.", quote(t.name))); err != nil {
		return err
	}
	c.forget()
	return nil
}

// isSQLite reports whether err is an error from SQLite.
func isSQLite(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr)
}
