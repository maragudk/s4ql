package core

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sqlite"
)

// cacheVersion is the version of the layout of the cache that this package makes and reads: the
// table s4ql_cache, one table for each table of the database, in which every column that does not
// take NULL is declared "not null" and a column with a default declares it in the form of
// [defaultSQL], and one index of SQLite for each index of the database.
const cacheVersion = 4

// maxLength is SQLite's default limit for the length of a text, a blob, and a row, in bytes, which
// is also the largest that SQLite takes: SQLITE_MAX_LENGTH.
const maxLength = 1_000_000_000

// conn is a connection to the cache, with the prepared statements and the tables that the
// transaction that uses it has looked up. It is used by one goroutine at a time.
type conn struct {
	c *sqlite.Conn
	// stmts that are prepared and at rest, by their SQL.
	stmts map[string]*sqlite.Stmt
	// tables that were looked up, by their folded name. A nil table does not exist.
	tables map[string]*table
	// limit is SQLite's limit on the connection for the length of a text, a blob, and a row.
	limit int
	// bugs planted in the instance of the connection: a copy of those of its [Options], which no
	// instance changes after its start.
	bugs bugs
	// schemaReads counts the statements with which [conn.load] and [conn.loadIndexes] have read
	// tables and their indexes from the schema of the cache, so that a test can pin what a call
	// reads of it.
	schemaReads int

	// users is the number of calls that use the connection now, and closed whether [DB.Close] or
	// [Reader.Close] has closed it. [DB.mu] guards both.
	users  int
	closed bool
}

// openConn to the cache at path with open, [sqlite.OpenVFS] or [sqlite.OpenExistingVFS], through
// the SQLite VFS of [Options.VFS], with the busy timeout of [Options.BusyTimeout], after which
// SQLite reports a lock that it waited for as "busy", and the length limit of the options.
func openConn(open func(path, vfs string) (*sqlite.Conn, error), path string, options Options) (*conn, error) {
	c, err := open(path, options.VFS)
	if err != nil {
		return nil, err
	}
	cn := &conn{c: c, stmts: map[string]*sqlite.Stmt{}, tables: map[string]*table{}, limit: options.LengthLimit, bugs: options.bugs}
	if err := c.SetLengthLimit(cn.limit); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := cn.exec(fmt.Sprint("pragma busy_timeout = ", options.BusyTimeout.Milliseconds())); err != nil {
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

// bind args to the parameters of s, from the first on: an int64, a string, or a [*s4qlpb.Value]. A
// real that is -0.0 is bound as 0.0, which is what a column of the cache holds for it, so that no
// row, no key, and no temporary table of a transaction holds the sign.
func bind(s *sqlite.Stmt, args ...any) error {
	for i, arg := range args {
		var err error
		switch arg := arg.(type) {
		case int64:
			err = s.BindInt64(i+1, arg)
		case string:
			err = s.BindText(i+1, arg)
		case *s4qlpb.Value:
			switch arg.WhichKind() {
			case s4qlpb.Value_Integer_case:
				err = s.BindInt64(i+1, arg.GetInteger())
			case s4qlpb.Value_Real_case:
				r := arg.GetReal()
				if r == 0 {
					// -0.0 becomes 0.0.
					r = 0
				}
				err = s.BindDouble(i+1, r)
			case s4qlpb.Value_Text_case:
				err = s.BindText(i+1, arg.GetText())
			case s4qlpb.Value_Blob_case:
				err = s.BindBlob(i+1, arg.GetBlob())
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
	return c.queryWith((*sqlite.Stmt).Step, sql, row, args...)
}

// queryContext is [conn.query] of a statement that only reads, each of whose steps SQLite stops once
// ctx has ended, as [sqlite.Stmt.StepContext] says. It then fails with the error of ctx.
func (c *conn) queryContext(ctx context.Context, sql string, row func(s *sqlite.Stmt) error, args ...any) error {
	return c.queryWith(func(s *sqlite.Stmt) (bool, error) { return s.StepContext(ctx) }, sql, row, args...)
}

// queryWith is [conn.query] with step for each step of the statement.
func (c *conn) queryWith(step func(s *sqlite.Stmt) (bool, error), sql string, row func(s *sqlite.Stmt) error, args ...any) error {
	s, err := c.prepare(sql)
	if err != nil {
		return err
	}
	if err := bind(s, args...); err != nil {
		_ = c.rest(sql, s)
		return err
	}
	for {
		ok, err := step(s)
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

// readSchema is [conn.query] for a statement of [conn.load] or of [conn.loadIndexes], which it
// counts.
func (c *conn) readSchema(sql string, row func(s *sqlite.Stmt) error, args ...any) error {
	c.schemaReads++
	return c.query(sql, row, args...)
}

// setUp the cache on the write connection: the write-ahead log, how hard SQLite syncs, and the
// table s4ql_cache with its one row. It is the first statement on the first connection to the
// cache, so it is what recovers the write-ahead log after a crash. It refuses a cache of another
// version with [ErrCacheVersion], and one that records another identity than the given one with
// [ErrCacheDatabase].
//
// With synchronous = normal, SQLite syncs the write-ahead log only at a checkpoint. After a power
// loss the cache can then lack its last commits, and is never corrupt
// (https://sqlite.org/pragma.html#pragma_synchronous). A cache that is behind is what catching up
// from the log is for, so the sync at every commit that synchronous = full adds buys nothing.
func (c *conn) setUp(syncOff bool, identity string) error {
	if err := c.wal(); err != nil {
		return err
	}
	synchronous := "pragma synchronous = normal"
	if syncOff {
		synchronous = "pragma synchronous = off"
	}
	if err := c.exec(synchronous); err != nil {
		return err
	}

	// A cache that has its table is only read, so that an instance of the same database opens it
	// while another one writes to it. A new cache gets its table in a transaction that holds the
	// write lock from its start, and that looks again whether the table is there: of two instances
	// that open one new cache at once, exactly one makes it, and the other then reads what the first
	// recorded.
	made, err := c.made()
	if err != nil {
		return err
	}
	if !made {
		if err := c.c.Exec("begin immediate"); err != nil {
			return err
		}
		if err := c.make(identity); err != nil {
			_ = c.c.Exec("rollback")
			c.forget()
			return err
		}
		c.forget()
		if err := c.c.Exec("commit"); err != nil {
			_ = c.c.Exec("rollback")
			return err
		}
	}

	r, err := c.record()
	if err != nil {
		return err
	}
	if err := r.check(identity); err != nil {
		return err
	}
	return c.checkObjects()
}

// checkObjects of the schema of the cache: each is one that this package makes or that SQLite makes
// itself, and anything else is refused with [ErrCacheVersion]. A table of the database and its
// indexes are checked here only by their kind and their name, since [conn.load] and
// [conn.loadIndexes] check them in full when a transaction first looks the table up, before it
// reads or writes a row of it. What it accepts:
//   - the table s4ql_cache, declared exactly as [conn.make] declares it;
//   - a table whose name is not reserved, which is a table of the database;
//   - an index on such a table, which is an index of the database or SQLite's index of its key;
//   - a table whose name starts with "sqlite_", which only SQLite makes, such as sqlite_stat1.
//
// It refuses everything else: a view, a trigger, a table under another name that starts with
// "s4ql_", and an index on a table with a reserved name.
func (c *conn) checkObjects() error {
	return c.query("select type, name, tbl_name, sql from main.sqlite_schema", func(s *sqlite.Stmt) error {
		typ, name, owner := s.ColumnText(0), s.ColumnText(1), s.ColumnText(2)
		switch {
		case typ == "table" && name == "s4ql_cache":
			if s.ColumnText(3) != kept(cacheTableSQL) {
				return foreign("its table %q is not declared as s4ql declares it", name)
			}
			return nil
		case typ == "table" && strings.HasPrefix(fold(name), "sqlite_"):
			return nil
		case typ == "table" && !reserved(name):
			return nil
		case typ == "index" && !reserved(owner):
			return nil
		}
		return foreign("it holds the %v %q on the table %q, which s4ql does not make", typ, name, owner)
	})
}

// foreign is the error that wraps [ErrCacheVersion] for a cache that holds what this package does
// not make there, with the reason that format and args give, and the advice for it.
func foreign(format string, args ...any) error {
	return fmt.Errorf("%w: %v; something other than s4ql has changed the cache, so open the database with a new cache, or delete this one, with its files -wal and -shm", ErrCacheVersion, fmt.Sprintf(format, args...))
}

// kept is the text that SQLite keeps in sqlite_schema for sql, a statement of this package that
// creates a table or an index in the schema main: "CREATE TABLE " or "CREATE INDEX ", and then the
// statement from the name of the table or the index on, as it was written.
func kept(sql string) string {
	for _, prefix := range []string{"create table main.", "create index main."} {
		if rest, ok := strings.CutPrefix(sql, prefix); ok {
			return strings.ToUpper(strings.TrimSuffix(prefix, "main.")) + rest
		}
	}
	panic("core: no statement that creates a table or an index in main: " + sql)
}

// wal puts the cache into the journal mode wal, or finds it there.
//
// A new cache is in another journal mode, and the change to wal is a read that becomes a write.
// SQLite waits, with the busy timeout, for the readers of the cache, but not for another
// connection that holds the write lock, as one does that makes the same change at the same time:
// it reports "busy" at once (sqlite3BtreeBeginTrans calls the busy handler only from no
// transaction). So after "busy" it waits, with the busy timeout, for the write lock, and asks
// again; the cache is then in wal already, if the other connection made the same change. A try
// ends in "busy" again only if another connection took the write lock in between, so it gives up
// after five.
func (c *conn) wal() error {
	for try := 1; ; try++ {
		var mode string
		err := c.query("pragma journal_mode = wal", func(s *sqlite.Stmt) error {
			mode = s.ColumnText(0)
			return nil
		})
		if classify(err) == kindBusy && try < 5 {
			if err := c.c.Exec("begin immediate"); err != nil {
				return err
			}
			if err := c.c.Exec("rollback"); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if mode != "wal" {
			return fmt.Errorf("the journal mode of the cache is %q, not wal", mode)
		}
		return nil
	}
}

// made reports whether the cache has its table s4ql_cache.
func (c *conn) made() (bool, error) {
	n, err := c.count("select 1 from main.sqlite_schema where name = 's4ql_cache'")
	return n > 0, err
}

// cacheTableSQL is the statement that makes the table s4ql_cache.
const cacheTableSQL = `create table main.s4ql_cache (
			id integer primary key check (id = 1),
			version integer not null,
			head text not null,
			database text not null
		) strict`

// make the table s4ql_cache with its one row, which records the identity, if the cache has no such
// table, inside a transaction that holds the write lock. A cache that has tables and no table
// s4ql_cache is not one that this package made, and giving it a position would be a guess.
func (c *conn) make(identity string) error {
	made, err := c.made()
	if err != nil || made {
		return err
	}
	tables, err := c.count("select 1 from main.sqlite_schema")
	if err != nil {
		return err
	}
	if tables > 0 {
		return fmt.Errorf("%w: it records no position, and holds %v tables and indexes", ErrCacheVersion, tables)
	}
	if err := c.c.Exec(cacheTableSQL); err != nil {
		return err
	}
	return c.exec("insert into main.s4ql_cache (id, version, head, database) values (1, ?, ?, ?)", int64(cacheVersion), format.EmptyLog.String(), identity)
}

// position that the cache records: the name of the last commit object that it has applied, or
// [format.EmptyLog]. It refuses a cache of another version than this code makes with
// [ErrCacheVersion].
func (c *conn) position() (format.Name, error) {
	r, err := c.head()
	if err != nil {
		return format.Name{}, err
	}
	if r.version != cacheVersion {
		return format.Name{}, r.versionError()
	}
	return r.position()
}

// record of the cache, the one row of its table s4ql_cache.
type record struct {
	version  int64
	head     string
	database string
}

// record of the cache, with the identity of its database if it is of [cacheVersion]: a cache of
// another version is refused before its identity matters, and one of version 0 has no column for
// it. It reads the identity in a statement of its own, so outside a transaction the identity can
// be of a later state of the row than the version; no instance changes the identity of a cache.
func (c *conn) record() (record, error) {
	r, err := c.head()
	if err != nil {
		return record{}, err
	}
	if r.version != cacheVersion {
		return r, nil
	}
	err = c.query("select database from main.s4ql_cache", func(s *sqlite.Stmt) error {
		r.database = s.ColumnText(0)
		return nil
	})
	return r, err
}

// head is the record of the cache without the identity of its database: its version and its
// position. It refuses a cache that records no row, or more than one, with [ErrCacheVersion].
func (c *conn) head() (record, error) {
	var r record
	var rows int
	if err := c.query("select version, head from main.s4ql_cache", func(s *sqlite.Stmt) error {
		rows++
		r = record{version: s.ColumnInt64(0), head: s.ColumnText(1)}
		return nil
	}); err != nil {
		return record{}, err
	}
	if rows != 1 {
		return record{}, fmt.Errorf("%w: it records %v positions", ErrCacheVersion, rows)
	}
	return r, nil
}

// check that the record is of a cache that this code opens for the database with the given
// identity: of [cacheVersion], with a position that is a name, and of that database. It refuses
// another version, and a position that is no name, with [ErrCacheVersion], and another database
// with [ErrCacheDatabase].
func (r record) check(identity string) error {
	if r.version != cacheVersion {
		return r.versionError()
	}
	if _, err := r.position(); err != nil {
		return err
	}
	if r.database != identity {
		return fmt.Errorf("%w: it records the database %q, and this instance opens the database %q; open each database with a cache of its own, or delete the cache, with its files -wal and -shm, if the database has moved", ErrCacheDatabase, r.database, identity)
	}
	return nil
}

// versionError of a cache whose version this code does not read.
func (r record) versionError() error {
	return fmt.Errorf("%w: it has version %v, and this version of s4ql reads version %v; open the database with a new cache, or delete this one, with its files -wal and -shm", ErrCacheVersion, r.version, cacheVersion)
}

// position that the record holds, which is [ErrCacheVersion] if it is not a name.
func (r record) position() (format.Name, error) {
	name, err := format.ParseName(r.head)
	if err != nil {
		return format.Name{}, fmt.Errorf("%w: its position: %v", ErrCacheVersion, err)
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
	// indexed is whether [conn.loadIndexes] has read indexes and primary from the cache, and found
	// no index there that this package does not make. Until then both are empty and say nothing of
	// the table, and [table.checkIndexed] guards what reads them.
	indexed bool
	// indexes of the table, in the order in which they were created.
	indexes []*index
	// primary is the name that SQLite gives the index of the primary key.
	primary string
}

// checkIndexed is nil for a table whose indexes [conn.loadIndexes] has read, as [conn.lookUp] gives
// it. Its error wraps [ErrCacheVersion] for any other table, which only [conn.lookUpWithoutIndexes]
// and [newTable] give: a fault of this package, since everything that reads the indexes of a table,
// scans it, or writes a row to it takes the table from lookUp.
func (t *table) checkIndexed() error {
	if t.indexed {
		return nil
	}
	return fmt.Errorf("%w: the indexes of its table %q are used before they are read", ErrCacheVersion, t.name)
}

type column struct {
	name string
	typ  s4qlpb.Type
	// nullable is whether the column takes NULL. A primary-key column never does.
	nullable bool
	// dflt is the default of the column, NULL or a value of its type, or nil if it has none. The
	// cache stores nothing by it: every row that it is handed has a value for each column.
	dflt *s4qlpb.Value
}

// createTable that describes t.
func (t *table) createTable() *s4qlpb.CreateTable {
	ct := s4qlpb.CreateTable_builder{Table: new(t.name)}
	for _, c := range t.columns {
		column := s4qlpb.Column_builder{Name: new(c.name), Type: new(c.typ)}
		if c.nullable {
			column.Nullable = new(true)
		}
		// The clone of no default is none.
		column.Default = proto.CloneOf(c.dflt)
		ct.Columns = append(ct.Columns, column.Build())
	}
	for _, position := range t.key {
		ct.PrimaryKey = append(ct.PrimaryKey, uint32(position))
	}
	return ct.Build()
}

// newTable from the data of a create table, which [checkCreateTable] or [format.CommitReader] has
// accepted. It copies the defaults, so that a later change of ct does not change the table.
func newTable(ct *s4qlpb.CreateTable) *table {
	t := &table{name: ct.GetTable(), inKey: make([]bool, len(ct.GetColumns()))}
	for _, c := range ct.GetColumns() {
		// The clone of no default is none.
		dflt := proto.CloneOf(c.GetDefault())
		t.columns = append(t.columns, column{name: c.GetName(), typ: c.GetType(), nullable: c.GetNullable(), dflt: dflt})
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
	if name, ok := typeNames[typ]; ok {
		return name, nil
	}
	return "", invalidf("a column of type %v", typ)
}

// typeNames are the declared types of the columns of the cache, by their type in a commit object:
// the types of a strict table of SQLite but ANY, with both spellings of SQLite's integer type.
var typeNames = map[s4qlpb.Type]string{
	s4qlpb.Type_TYPE_INT:     "int",
	s4qlpb.Type_TYPE_INTEGER: "integer",
	s4qlpb.Type_TYPE_REAL:    "real",
	s4qlpb.Type_TYPE_TEXT:    "text",
	s4qlpb.Type_TYPE_BLOB:    "blob",
}

// typeOf a column whose declared type SQLite reports as declared, which it compares without regard
// to the case of the letters, or false if it is none of [typeNames].
func typeOf(declared string) (s4qlpb.Type, bool) {
	for typ, name := range typeNames {
		if fold(declared) == name {
			return typ, true
		}
	}
	return s4qlpb.Type_TYPE_UNSPECIFIED, false
}

// createSQL is the statement that creates the table t in the cache: a strict table without a
// rowid, with the names and the columns of the database's table. Every column outside the primary
// key that does not take NULL is declared "not null", so that SQLite refuses a NULL there too, and
// [conn.load] tells a column that takes NULL by its lack; a primary-key column of a table without a
// rowid never takes NULL. A column with a default declares it with DEFAULT and the literal of
// [defaultSQL], also DEFAULT NULL, and a column without one declares none, which is how
// [conn.load] tells the two apart. SQLite never stores a default itself, since every row that the
// cache writes names every column. A primary key of one column is declared with the column, so that
// its name is not in the text twice, and the text fits in the length limit as often as it can: see
// [conn.create].
func (t *table) createSQL() (string, error) {
	var b strings.Builder
	b.WriteString("create table main.")
	b.WriteString(quote(t.name))
	b.WriteString(" (")
	for i, c := range t.columns {
		typ, err := typeSQL(c.typ)
		if err != nil {
			return "", err
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(quote(c.name))
		b.WriteString(" ")
		b.WriteString(typ)
		switch {
		case len(t.key) == 1 && t.inKey[i]:
			b.WriteString(" primary key")
		case !c.nullable && !t.inKey[i]:
			b.WriteString(" not null")
		}
		if c.dflt != nil {
			b.WriteString(" default ")
			b.WriteString(defaultSQL(c.dflt))
		}
	}
	if len(t.key) > 1 {
		b.WriteString(", primary key (")
		b.WriteString(t.columnList(t.key))
		b.WriteString(")")
	}
	b.WriteString(") strict, without rowid")
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
// column's type, or it is NULL in a column that takes NULL, which no primary-key column does; a
// real is neither NaN nor infinite, and text is valid UTF-8 without the character NUL, which s4ql
// never stores; and a text or a blob is not longer than limit, SQLite's limit for the length of
// one, for which the error wraps [ErrTooBig]. A strict table of SQLite does not refuse every value
// of another storage class: it converts the text '1' and the real 1.0 to an integer, and the
// integer 1 to a real.
func (t *table) checkValue(position int, v *s4qlpb.Value, limit int) error {
	c := t.columns[position]
	var n int
	switch kind := v.WhichKind(); {
	case kind == s4qlpb.Value_Integer_case && (c.typ == s4qlpb.Type_TYPE_INT || c.typ == s4qlpb.Type_TYPE_INTEGER):
		return nil
	case kind == s4qlpb.Value_Real_case && c.typ == s4qlpb.Type_TYPE_REAL:
		if r := v.GetReal(); math.IsNaN(r) || math.IsInf(r, 0) {
			return invalidf("the real %v in the column %q of the table %q", r, c.name, t.name)
		}
		return nil
	case kind == s4qlpb.Value_Text_case && c.typ == s4qlpb.Type_TYPE_TEXT:
		text := v.GetText()
		if !utf8.ValidString(text) {
			return invalidf("text that is not valid UTF-8 in the column %q of the table %q", c.name, t.name)
		}
		if strings.IndexByte(text, 0) >= 0 {
			return invalidf("text that holds the character NUL in the column %q of the table %q", c.name, t.name)
		}
		n = len(text)
	case kind == s4qlpb.Value_Blob_case && c.typ == s4qlpb.Type_TYPE_BLOB:
		n = len(v.GetBlob())
	case kind == s4qlpb.Value_Null_case:
		switch {
		case t.inKey[position]:
			return invalidf("NULL in the primary-key column %q of the table %q", c.name, t.name)
		case !c.nullable:
			return invalidf("NULL in the column %q of the table %q, which does not take NULL", c.name, t.name)
		}
		return nil
	default:
		return invalidf("a value of kind %v in the column %q of the table %q, whose type is %v", kind, c.name, t.name, c.typ)
	}
	if n > limit {
		return fmt.Errorf("%w: a value of %v bytes in the column %q of the table %q, where the limit is %v", ErrTooBig, n, c.name, t.name, limit)
	}
	return nil
}

// checkRow has a valid value for each column of the table, as [table.checkValue] checks it.
func (t *table) checkRow(row []*s4qlpb.Value, limit int) error {
	if len(row) != len(t.columns) {
		return invalidf("a row of %v values for the table %q, which has %v columns", len(row), t.name, len(t.columns))
	}
	for position, v := range row {
		if err := t.checkValue(position, v, limit); err != nil {
			return err
		}
	}
	return nil
}

// checkKey has a valid value for each primary-key column of the table, in key order, as
// [table.checkValue] checks it.
func (t *table) checkKey(key []*s4qlpb.Value, limit int) error {
	if len(key) != len(t.key) {
		return invalidf("a key of %v values for the table %q, whose primary key has %v columns", len(key), t.name, len(t.key))
	}
	for i, v := range key {
		if err := t.checkValue(t.key[i], v, limit); err != nil {
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
		case sqlite.TypeFloat:
			row[i] = s4qlpb.Value_builder{Real: new(s.ColumnDouble(first + i))}.Build()
		case sqlite.TypeText:
			row[i] = s4qlpb.Value_builder{Text: new(s.ColumnText(first + i))}.Build()
		case sqlite.TypeBlob:
			// ColumnBlob gives an empty blob as an empty slice, which the builder takes for a blob;
			// nil would be no value.
			row[i] = s4qlpb.Value_builder{Blob: s.ColumnBlob(first + i)}.Build()
		case sqlite.TypeNull:
			row[i] = s4qlpb.Value_builder{Null: &s4qlpb.Null{}}.Build()
		default:
			return nil, fmt.Errorf("%w: the cache holds a value of type %v", ErrCacheVersion, typ)
		}
	}
	return row, nil
}

// lookUp the table of the database with the given name, compared as SQLite compares names, with its
// indexes, or nil if there is none. The indexes are read when a table is first looked up with them,
// and stay with the table until the connection forgets it: see [conn.loadIndexes], which refuses a
// table with an index that this package does not make. Everything that writes a row of a table,
// scans it, or reads its indexes looks the table up here, so that it runs after that check.
func (c *conn) lookUp(name string) (*table, error) {
	t, err := c.lookUpWithoutIndexes(name)
	if err != nil || t == nil {
		return nil, err
	}
	if err := c.loadIndexes(t); err != nil {
		return nil, err
	}
	return t, nil
}

// lookUpWithoutIndexes is [conn.lookUp] of the table alone: its columns and its primary key, which
// is all that reading the definition of a table or one row by its key takes, so that such a call
// reads nothing of the indexes from the schema. The table that it gives has its indexes only if a
// lookUp read them before, and the check of [conn.loadIndexes] may not have run on it, so it is
// for nothing that writes a row, scans, or reads an index: [table.checkIndexed] refuses those.
func (c *conn) lookUpWithoutIndexes(name string) (*table, error) {
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

// load the table with the given name from the schema of the cache, without its indexes, or nil if
// there is none. The cache's own tables are not tables of the database.
//
// It refuses with [ErrCacheVersion] a table with a trigger, and a table that is not exactly what
// [table.createSQL] writes for the table that it reads. It reads the columns from
// pragma_table_info, which hides much of what a statement can declare: a generated column, a
// collation, a check, a foreign key, a constraint of uniqueness, the conflict clause of a key, and
// whether the table has a rowid or is strict. So it then compares the text that SQLite keeps for
// the table in sqlite_schema with the text that createSQL writes for what it read. SQLite keeps the
// statement from the name of the table on, as it was written, and this package never alters a
// table, so a table that this package made always has that text, and anything else that a
// statement declares changes it.
func (c *conn) load(name string) (*table, error) {
	if reserved(name) {
		return nil, nil
	}
	var t *table
	var sql string
	var triggers []string
	if err := c.readSchema("select type, name, sql from main.sqlite_schema where tbl_name = ? collate nocase and type in ('table', 'trigger')", func(s *sqlite.Stmt) error {
		if s.ColumnText(0) == "table" {
			t, sql = &table{name: s.ColumnText(1)}, s.ColumnText(2)
		} else {
			triggers = append(triggers, s.ColumnText(1))
		}
		return nil
	}, name); err != nil {
		return nil, err
	}
	// A trigger refuses the name also when no table has it, as one on a view would: the core makes
	// no trigger, and [conn.checkObjects] refuses a view at the open.
	if len(triggers) > 0 {
		return nil, foreign("the table %q has the trigger %q", name, triggers[0])
	}
	if t == nil {
		return nil, nil
	}
	// A virtual table is refused before pragma_table_info, which fails for one whose module is not
	// there.
	if !strings.HasPrefix(sql, "CREATE TABLE ") {
		return nil, foreign("its table %q is not declared as s4ql declares a table", t.name)
	}

	// The position of a column in the primary key, from 1, with its position in the table.
	keyed := map[int64]int{}
	// A column outside the primary key takes NULL if it is not declared "not null". SQLite reports
	// the text of a default as it was declared, and no default as NULL, and a default is read only in
	// the form that createSQL writes it.
	if err := c.readSchema(`select name, type, pk, "notnull", dflt_value from pragma_table_info(?, 'main') order by cid`, func(s *sqlite.Stmt) error {
		name := s.ColumnText(0)
		typ, ok := typeOf(s.ColumnText(1))
		if !ok {
			return foreign("the column %q of its table %q has the type %q", name, t.name, s.ColumnText(1))
		}
		pk := s.ColumnInt64(2)
		notNull := s.ColumnInt64(3) != 0
		if pk > 0 && !notNull {
			// SQLite reports the key of a table with a rowid as a column that takes NULL.
			return foreign("the primary-key column %q of its table %q takes NULL", name, t.name)
		}
		var dflt *s4qlpb.Value
		if s.ColumnType(4) != sqlite.TypeNull {
			if dflt, ok = parseDefault(s.ColumnText(4), typ); !ok {
				return foreign("the column %q of its table %q has the default %q, which s4ql does not write for a column of type %v", name, t.name, s.ColumnText(4), typeNames[typ])
			}
		}
		if pk > 0 {
			keyed[pk] = len(t.columns)
		}
		t.columns = append(t.columns, column{name: name, typ: typ, nullable: pk == 0 && !notNull, dflt: dflt})
		return nil
	}, t.name); err != nil {
		return nil, err
	}
	t.inKey = make([]bool, len(t.columns))
	for i := range len(keyed) {
		position, ok := keyed[int64(i+1)]
		if !ok {
			return nil, foreign("its table %q has no column %v of its primary key", t.name, i+1)
		}
		t.key = append(t.key, position)
		t.inKey[position] = true
	}
	if len(t.columns) == 0 || len(t.key) == 0 {
		return nil, foreign("its table %q has %v columns and a primary key of %v", t.name, len(t.columns), len(t.key))
	}
	want, err := t.createSQL()
	if err != nil {
		return nil, err
	}
	if sql != kept(want) {
		return nil, foreign("its table %q is not declared as s4ql declares a table of its columns", t.name)
	}
	return t, nil
}

// list the tables of the database, in the order of their names as bytes, each without its indexes,
// as [conn.lookUpWithoutIndexes] gives it.
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
		t, err := c.lookUpWithoutIndexes(name)
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

// exists reports whether the table has a row with the given primary key, which [table.checkKey]
// has accepted.
func (c *conn) exists(t *table, key []*s4qlpb.Value) (bool, error) {
	n, err := c.count(fmt.Sprint("select 1 from main.", quote(t.name), " as t where ", t.keyCondition("t")), values(key)...)
	return n > 0, err
}

// Bound of a scan on one column of a key, as SQLite compares the values of the column: Value is the
// smallest value that a lower bound lets through and the largest that an upper bound does, and the
// rows whose value is Value are left out if Open. A Bound without a Value bounds nothing.
type Bound struct {
	Value *s4qlpb.Value
	Open  bool
}

// checkBounds of a scan of the table: a bound with a value is only for a table whose primary key is
// one column, and its value is a valid key of the table, as [table.checkKey] checks it.
func (t *table) checkBounds(limit int, bounds ...Bound) error {
	for _, b := range bounds {
		if b.Value == nil {
			continue
		}
		if err := t.checkKey([]*s4qlpb.Value{b.Value}, limit); err != nil {
			return err
		}
	}
	return nil
}

// scan every row of the table in the order of its primary key, as [conn.scanIndex] reads them.
func (c *conn) scan(ctx context.Context, t *table) iter.Seq2[[]*s4qlpb.Value, error] {
	return c.scanIndex(ctx, t, nil, IndexScan{}, nil)
}

// upsert the row into the table: insert it, or replace the row with the same primary key. The
// table has no triggers and no foreign keys, so replacing a row runs nothing else, and no unique
// index but that of its primary key, so it replaces no other row: [conn.loadIndexes] has refused a
// table with one, which [table.checkIndexed] makes sure of.
func (c *conn) upsert(t *table, row []*s4qlpb.Value) error {
	if err := t.checkIndexed(); err != nil {
		return err
	}
	sql := fmt.Sprint("insert or replace into main.", quote(t.name), " (", t.columnList(t.all()), ") values (", placeholders(len(t.columns)), ")")
	return c.exec(sql, values(row)...)
}

// insert the row into the table if the table has no row with the same primary key, and report
// whether it did. SQLite gives "string or blob too big" for a row that it cannot hold also when the
// key exists, since it builds the record of the row first. It fails as [table.checkIndexed] does.
func (c *conn) insert(t *table, row []*s4qlpb.Value) (bool, error) {
	if err := t.checkIndexed(); err != nil {
		return false, err
	}
	// "on conflict do nothing" skips the row only for its primary key, and a NULL in a column that is
	// declared "not null" still fails, where "insert or ignore" would skip that row too.
	sql := fmt.Sprint("insert into main.", quote(t.name), " (", t.columnList(t.all()), ") values (", placeholders(len(t.columns)), ") on conflict do nothing returning 1")
	n, err := c.count(sql, values(row)...)
	return n > 0, err
}

// delete the row of the table with the given primary key, and report whether there was one. It
// fails as [table.checkIndexed] does.
func (c *conn) delete(t *table, key []*s4qlpb.Value) (bool, error) {
	if err := t.checkIndexed(); err != nil {
		return false, err
	}
	sql := fmt.Sprint("delete from main.", quote(t.name), " as t where ", t.keyCondition("t"), " returning 1")
	n, err := c.count(sql, values(key)...)
	return n > 0, err
}

// create the table in the cache.
//
// It fails with an error that wraps [ErrInvalid] and [ErrTooLongForCache] for a table whose
// statement is longer than the length limit as SQLite keeps it in its schema, without "main.", since
// a connection under the limit cannot read a schema that holds a longer text.
func (c *conn) create(t *table) error {
	sql, err := t.createSQL()
	if err != nil {
		return err
	}
	if kept := len(sql) - len("main."); kept > c.limit {
		return fmt.Errorf("%w: %w: a table whose statement in the cache is %v bytes, where the limit is %v", ErrInvalid, ErrTooLongForCache, kept, c.limit)
	}
	return c.changeSchema(sql)
}

// drop the table from the cache, with its rows.
func (c *conn) drop(t *table) error {
	return c.changeSchema(fmt.Sprint("drop table main.", quote(t.name)))
}

// changeSchema with the statement sql, under SQLite's largest length limit. SQLite formats the
// statements with which it changes its schema under the limit, and they hold the name of the table
// and, for a create, the statement, so it refuses as too big a table whose statement is a little
// shorter than the limit. A statement that changes the schema is not kept, and those at rest are
// prepared again.
func (c *conn) changeSchema(sql string) error {
	c.forget()
	if err := c.c.SetLengthLimit(maxLength); err != nil {
		return err
	}
	err := c.c.Exec(sql)
	if err := errors.Join(err, c.c.SetLengthLimit(c.limit)); err != nil {
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
