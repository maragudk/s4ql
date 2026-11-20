package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"slices"
	"strings"
	"time"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sqlite"
)

// MaxIndexColumns is the largest number of columns of an index. A scan pins the columns of the key
// of an index with one term of a condition each, the key of an index is its columns and then those
// of the primary key, of which there are at most 250, and SQLite refuses a condition that is nested
// deeper than a thousand levels.
const MaxIndexColumns = 250

// index of a table of the database, as the cache holds it: an index of SQLite on the columns, which
// is not unique.
type index struct {
	// name as the index was created.
	name string
	// columns of the index, in the order of the index.
	columns []KeyColumn
}

// KeyColumn is one column of the key of an index or of a primary key: the position of the column in
// the table, and whether the key holds it in descending order.
type KeyColumn struct {
	Position   int
	Descending bool
}

// IndexKey is the key that a scan by the index ix of the table t reads in the order of, as SQLite
// stores an index of a table without a rowid: the columns of the index, each in its direction, and
// then the primary-key columns that the index does not hold, in key order and ascending. The key of
// no index, a nil ix, is the primary key. It is the one definition of the key of an index: a scan
// pins and bounds the columns of this key, in this order. It takes a table and an index that
// [Tx.CreateTable] and [Tx.CreateIndex] accept.
func IndexKey(t *s4qlpb.CreateTable, ix *s4qlpb.CreateIndex) []KeyColumn {
	var columns []KeyColumn
	for _, c := range ix.GetColumns() {
		columns = append(columns, KeyColumn{Position: int(c.GetColumn()), Descending: c.GetDescending()})
	}
	primaryKey := make([]int, len(t.GetPrimaryKey()))
	for i, position := range t.GetPrimaryKey() {
		primaryKey[i] = int(position)
	}
	return indexKey(primaryKey, columns)
}

// indexKey of the index on the columns of a table whose primary key has the columns at the
// positions primaryKey, as [IndexKey] says.
func indexKey(primaryKey []int, columns []KeyColumn) []KeyColumn {
	key := slices.Clone(columns)
	for _, position := range primaryKey {
		if !slices.ContainsFunc(columns, func(c KeyColumn) bool { return c.Position == position }) {
			key = append(key, KeyColumn{Position: position})
		}
	}
	return key
}

// scanKey is the key that a scan by the index ix of the table reads in the order of, as
// [IndexKey] says. The key of no index is the primary key.
func (t *table) scanKey(ix *index) []KeyColumn {
	if ix == nil {
		return indexKey(t.key, nil)
	}
	return indexKey(t.key, ix.columns)
}

// index of the table with the given name, compared as SQLite compares names, or nil if the table
// has none. It fails as [table.checkIndexed] does.
func (t *table) index(name string) (*index, error) {
	if err := t.checkIndexed(); err != nil {
		return nil, err
	}
	folded := fold(name)
	for _, ix := range t.indexes {
		if fold(ix.name) == folded {
			return ix, nil
		}
	}
	return nil, nil
}

// createIndex that describes the index ix of the table, as a commit object holds it: a column in
// ascending order is without the field for its direction.
func (t *table) createIndex(ix *index) *s4qlpb.CreateIndex {
	ci := s4qlpb.CreateIndex_builder{Index: new(ix.name), Table: new(t.name)}
	for _, c := range ix.columns {
		column := s4qlpb.IndexColumn_builder{Column: new(uint32(c.Position))}
		if c.Descending {
			column.Descending = new(true)
		}
		ci.Columns = append(ci.Columns, column.Build())
	}
	return ci.Build()
}

// createIndexes that describe the indexes of the table, in the order in which they were created. It
// fails as [table.checkIndexed] does.
func (t *table) createIndexes() ([]*s4qlpb.CreateIndex, error) {
	if err := t.checkIndexed(); err != nil {
		return nil, err
	}
	cis := make([]*s4qlpb.CreateIndex, len(t.indexes))
	for i, ix := range t.indexes {
		cis[i] = t.createIndex(ix)
	}
	return cis, nil
}

// newIndex of the table from the data of a create index, or an error that says which of its columns
// the table does not have. The writer of commit objects accepts the create index: it has at least
// one column, and none twice.
func (t *table) newIndex(ci *s4qlpb.CreateIndex) (*index, error) {
	ix := &index{name: ci.GetIndex()}
	for _, c := range ci.GetColumns() {
		if c.GetColumn() >= uint32(len(t.columns)) {
			return nil, fmt.Errorf("column %v of the table %q, which has %v columns", c.GetColumn(), t.name, len(t.columns))
		}
		ix.columns = append(ix.columns, KeyColumn{Position: int(c.GetColumn()), Descending: c.GetDescending()})
	}
	return ix, nil
}

// createIndexSQL is the statement that creates the index ix of the table in the cache: an index of
// SQLite on the columns of ix, each in its direction, that is not unique, not partial, on no
// expression, and with no collation, which is all that [conn.loadIndexes] reads back.
func (t *table) createIndexSQL(ix *index) string {
	terms := make([]string, len(ix.columns))
	for i, c := range ix.columns {
		terms[i] = quote(t.columns[c.Position].name)
		if c.Descending {
			terms[i] += " desc"
		}
	}
	return fmt.Sprint("create index main.", quote(ix.name), " on ", quote(t.name), " (", strings.Join(terms, ", "), ")")
}

// loadIndexes of the table t from the schema of the cache, unless t has them: its indexes in the
// order in which they were created, and the name that SQLite gives the index of its primary key,
// which a scan names to read the table in the order of its key. It refuses an index that this
// package does not make with [ErrCacheVersion]: one that is unique or partial, on an expression,
// with a collation, with a column twice, or with a reserved name. A table that it refuses stays
// without its indexes, and the next call that needs them refuses it again.
//
// It runs when a call first needs the indexes of the table, which is before the call writes a row
// of the table, scans it, or reads an index of it: see [conn.lookUp]. A unique index that this
// package did not make would let the upsert of a row replace another row, so no row is written to
// a table whose indexes it has not read.
//
// The order of creation is the order of the rowids in sqlite_schema. SQLite gives a new row of a
// table the rowid after the largest one in the table (https://sqlite.org/autoinc.html), so an index
// that is created later than another that still exists has the larger rowid, also after other
// indexes and tables were dropped. The index of the primary key of a table without a rowid is the
// table itself, and has a name and no row there.
func (c *conn) loadIndexes(t *table) error {
	if t.indexed {
		return nil
	}
	var primary string
	var names []string
	if err := c.readSchema(`select l.name, l."unique", l.origin, l.partial, s.rowid from pragma_index_list(?1, 'main') as l
		left join main.sqlite_schema as s on s.type = 'index' and s.name = l.name order by s.rowid`, func(s *sqlite.Stmt) error {
		name, schemaless := s.ColumnText(0), s.ColumnType(4) == sqlite.TypeNull
		if s.ColumnText(2) == "pk" && schemaless && primary == "" {
			primary = name
			return nil
		}
		if s.ColumnText(2) != "c" || s.ColumnInt64(1) != 0 || s.ColumnInt64(3) != 0 || schemaless || reserved(name) {
			return fmt.Errorf("%w: its table %q has the index %q, which is unique, partial, of a constraint, or of a reserved name", ErrCacheVersion, t.name, name)
		}
		names = append(names, name)
		return nil
	}, t.name); err != nil {
		return err
	}
	if primary == "" {
		return fmt.Errorf("%w: its table %q has no index of its primary key", ErrCacheVersion, t.name)
	}

	var indexes []*index
	for _, name := range names {
		ix := &index{name: name}
		held := make([]bool, len(t.columns))
		if err := c.readSchema(`select cid, "desc", coll, key from pragma_index_xinfo(?1, 'main') order by seqno`, func(s *sqlite.Stmt) error {
			if s.ColumnInt64(3) == 0 {
				// A column of the primary key, which SQLite adds to the key of the index.
				return nil
			}
			position := s.ColumnInt64(0)
			switch {
			case position < 0 || position >= int64(len(t.columns)):
				return fmt.Errorf("%w: the index %q of its table %q is on an expression", ErrCacheVersion, name, t.name)
			case s.ColumnText(2) != "BINARY":
				return fmt.Errorf("%w: the index %q of its table %q has the collation %q", ErrCacheVersion, name, t.name, s.ColumnText(2))
			case held[position]:
				return fmt.Errorf("%w: the index %q of its table %q holds the column %q twice", ErrCacheVersion, name, t.name, t.columns[position].name)
			}
			held[position] = true
			ix.columns = append(ix.columns, KeyColumn{Position: int(position), Descending: s.ColumnInt64(1) != 0})
			return nil
		}, name); err != nil {
			return err
		}
		if len(ix.columns) == 0 {
			return fmt.Errorf("%w: the index %q of its table %q has no columns", ErrCacheVersion, name, t.name)
		}
		indexes = append(indexes, ix)
	}
	t.indexes, t.primary, t.indexed = indexes, primary, true
	return nil
}

// lookUpIndex of the database with the given name, compared as SQLite compares names, with its
// table, or nil if there is none. A table and an index share their names, so at most one of
// [conn.lookUp] and lookUpIndex finds something under a name.
func (c *conn) lookUpIndex(name string) (*table, *index, error) {
	if reserved(name) {
		return nil, nil, nil
	}
	var owner string
	var found bool
	if err := c.query("select tbl_name from main.sqlite_schema where type = 'index' and name = ?1 collate nocase", func(s *sqlite.Stmt) error {
		owner, found = s.ColumnText(0), true
		return nil
	}, name); err != nil || !found {
		return nil, nil, err
	}
	t, err := c.lookUp(owner)
	if err != nil {
		return nil, nil, err
	}
	if t == nil {
		return nil, nil, fmt.Errorf("%w: its index %q is on the table %q, which cannot be found", ErrCacheVersion, name, owner)
	}
	ix, err := t.index(name)
	if err != nil {
		return nil, nil, err
	}
	if ix == nil {
		return nil, nil, fmt.Errorf("%w: its index %q is listed and cannot be found on its table %q", ErrCacheVersion, name, t.name)
	}
	return t, ix, nil
}

// indexNames of the database, in the order in which the indexes were created, as
// [conn.loadIndexes] reads it.
func (c *conn) indexNames() ([]string, error) {
	var names []string
	err := c.query("select name from main.sqlite_schema where type = 'index' order by rowid", func(s *sqlite.Stmt) error {
		if name := s.ColumnText(0); !reserved(name) {
			names = append(names, name)
		}
		return nil
	})
	return names, err
}

// createIndex ix of the table t in the cache, which builds it from the rows of the table. SQLite
// keeps the statement in its schema, without the name of the schema, and a connection under the
// length limit cannot load a schema with a longer text, so an index whose statement is longer than
// the limit is refused before anything changes, with an error that wraps [ErrInvalid], as
// [conn.create] refuses such a table. A statement within the limit runs as [conn.changeSchema]
// says, since SQLite formats the statement that writes the index into its schema under the limit
// too.
func (c *conn) createIndex(t *table, ix *index) error {
	sql := t.createIndexSQL(ix)
	if kept := len(sql) - len("main."); kept > c.limit {
		return invalidf("an index whose statement in the cache is %v bytes, where the limit is %v", kept, c.limit)
	}
	return c.changeSchema(sql)
}

// dropIndex ix from the cache.
func (c *conn) dropIndex(ix *index) error {
	return c.changeSchema(fmt.Sprint("drop index main.", quote(ix.name)))
}

// checkCreateIndex is one that a commit object can hold. Its error wraps [ErrInvalid].
func checkCreateIndex(ci *s4qlpb.CreateIndex, commitTime time.Time) error {
	// The writer of commit objects knows what a valid index is.
	w, err := format.NewCommitWriter(io.Discard, format.EmptyLog, commitTime)
	if err != nil {
		return err
	}
	if err := w.CreateIndex(ci); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(ci.GetColumns()) > MaxIndexColumns {
		return invalidf("an index of %v columns, where the limit is %v", len(ci.GetColumns()), MaxIndexColumns)
	}
	return nil
}

// CreateIndex from its name, its table, and its columns, as positions in the columns of the table,
// each with its direction. The index is not unique. It is built from the rows that the table
// holds, and holds every row that is written from then on.
//
// It fails with the first of these, which is the order of SQLite's errors for a create index: an
// error that wraps [ErrNoTable] if there is no such table, [ErrReservedName] for a name that
// starts with "s4ql_" or "sqlite_", [ErrTableExists] if a table has the name, and [ErrIndexExists]
// if an index has it, each compared as SQLite compares names, and then [ErrInvalid] for an index
// that the database cannot hold: one without a name or without columns, with a column twice, with
// more than 250 columns, with a column that the table does not have, or whose statement in the
// cache is longer than the length limit. After each of these the transaction goes on.
func (tx *Tx) CreateIndex(ctx context.Context, ci *s4qlpb.CreateIndex) error {
	const operation = "create index"
	if err := tx.check(ctx, operation); err != nil {
		return err
	}
	defer tx.leave(ctx)
	t, err := tx.table(ctx, operation, ci.GetTable())
	if err != nil {
		return err
	}
	if reserved(ci.GetIndex()) {
		return fmt.Errorf("%w: %q", ErrReservedName, ci.GetIndex())
	}
	if err := tx.unused(ctx, operation, ci.GetIndex()); err != nil {
		return err
	}
	if err := checkCreateIndex(ci, tx.d.host.Wall()); err != nil {
		return err
	}
	ix, err := t.newIndex(ci)
	if err != nil {
		return invalidf("an index on %v", err)
	}
	if err := tx.c.createIndex(t, ix); err != nil {
		if errors.Is(err, ErrInvalid) {
			// The cache refused the index before it changed anything.
			return err
		}
		return tx.fail(ctx, operation, err)
	}
	if tx.createdIndexes == nil {
		tx.createdIndexes = map[string]bool{}
	}
	tx.createdIndexes[fold(ix.name)] = true
	return nil
}

// unused checks that no table and no index has the given name, compared as SQLite compares names,
// which a table and an index share. Its error wraps [ErrTableExists] or [ErrIndexExists].
func (tx *Tx) unused(ctx context.Context, operation, name string) error {
	existing, err := tx.c.lookUp(name)
	if err != nil {
		return tx.fail(ctx, operation, err)
	}
	if existing != nil {
		return fmt.Errorf("%w: %q", ErrTableExists, existing.name)
	}
	_, ix, err := tx.c.lookUpIndex(name)
	if err != nil {
		return tx.fail(ctx, operation, err)
	}
	if ix != nil {
		return fmt.Errorf("%w: %q", ErrIndexExists, ix.name)
	}
	return nil
}

// DropIndex with the given name. Its table and the rows of its table stay. It fails with an error
// that wraps [ErrNoIndex] if there is none.
func (tx *Tx) DropIndex(ctx context.Context, name string) error {
	const operation = "drop index"
	if err := tx.check(ctx, operation); err != nil {
		return err
	}
	defer tx.leave(ctx)
	_, ix, err := tx.c.lookUpIndex(name)
	if err != nil {
		return tx.fail(ctx, operation, err)
	}
	if ix == nil {
		return fmt.Errorf("%w: %q", ErrNoIndex, name)
	}
	if err := tx.c.dropIndex(ix); err != nil {
		return tx.fail(ctx, operation, err)
	}
	if folded := fold(ix.name); tx.createdIndexes[folded] {
		// The transaction created the index, so dropping it leaves what was there before: nothing, or
		// the drop of the index that the transaction dropped first.
		delete(tx.createdIndexes, folded)
	} else {
		tx.droppedIndexes = append(tx.droppedIndexes, ix.name)
	}
	return nil
}

// saveIndexes of the transaction in the savepoint sp.
func (tx *Tx) saveIndexes(sp *savepoint) {
	sp.droppedIndexes, sp.createdIndexes = slices.Clone(tx.droppedIndexes), maps.Clone(tx.createdIndexes)
}

// restoreIndexes of the transaction from the savepoint sp, which ends.
func (tx *Tx) restoreIndexes(sp *savepoint) {
	tx.droppedIndexes, tx.createdIndexes = sp.droppedIndexes, sp.createdIndexes
}

// forgetIndexes of the table t, which the transaction is about to drop: those that it created are
// gone with the table, and leave nothing in its commit object. It fails as [table.checkIndexed]
// does.
func (tx *Tx) forgetIndexes(t *table) error {
	if err := t.checkIndexed(); err != nil {
		return err
	}
	for _, ix := range t.indexes {
		delete(tx.createdIndexes, fold(ix.name))
	}
	return nil
}

// spoolCreatedIndexes writes the indexes that the transaction created and that are there, in the
// order in which it created them, and returns their number.
func (tx *Tx) spoolCreatedIndexes(w *format.CommitWriter) (int, error) {
	if len(tx.createdIndexes) == 0 {
		return 0, nil
	}
	names, err := tx.c.indexNames()
	if err != nil {
		return 0, err
	}
	var written int
	for _, name := range names {
		if !tx.createdIndexes[fold(name)] {
			continue
		}
		t, ix, err := tx.c.lookUpIndex(name)
		if err != nil {
			return 0, err
		}
		if ix == nil {
			return 0, fmt.Errorf("%w: its index %q is listed and cannot be found", ErrCacheVersion, name)
		}
		if err := w.CreateIndex(t.createIndex(ix)); err != nil {
			return 0, err
		}
		written++
	}
	if written != len(tx.createdIndexes) {
		return 0, fmt.Errorf("%w: it holds %v of the %v indexes that the transaction created", ErrCacheVersion, written, len(tx.createdIndexes))
	}
	return written, nil
}

// Indexes of the table with the given name, as the transaction sees them, in the order in which
// they were created, which is the same on every instance. It fails with an error that wraps
// [ErrNoTable] if there is no such table.
func (tx *Tx) Indexes(ctx context.Context, name string) ([]*s4qlpb.CreateIndex, error) {
	const operation = "indexes"
	if err := tx.check(ctx, operation); err != nil {
		return nil, err
	}
	defer tx.leave(ctx)
	t, err := tx.table(ctx, operation, name)
	if err != nil {
		return nil, err
	}
	cis, err := t.createIndexes()
	return cis, tx.fail(ctx, operation, err)
}

// Index with the given name, as the transaction sees it, or an error that wraps [ErrNoIndex].
func (tx *Tx) Index(ctx context.Context, name string) (*s4qlpb.CreateIndex, error) {
	const operation = "index"
	if err := tx.check(ctx, operation); err != nil {
		return nil, err
	}
	defer tx.leave(ctx)
	t, ix, err := tx.c.lookUpIndex(name)
	if err != nil {
		return nil, tx.fail(ctx, operation, err)
	}
	if ix == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoIndex, name)
	}
	return t.createIndex(ix), nil
}

// Indexes of the table with the given name, in the order in which they were created, which is the
// same on every instance. It fails with an error that wraps [ErrNoTable] if there is no such table.
func (r *ReadTx) Indexes(ctx context.Context, name string) ([]*s4qlpb.CreateIndex, error) {
	const operation = "indexes"
	if err := r.check(); err != nil {
		return nil, err
	}
	defer r.leave()
	t, err := r.table(ctx, operation, name)
	if err != nil {
		return nil, err
	}
	cis, err := t.createIndexes()
	return cis, r.d.cacheError(ctx, operation, err)
}

// Index with the given name, or an error that wraps [ErrNoIndex].
func (r *ReadTx) Index(ctx context.Context, name string) (*s4qlpb.CreateIndex, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	defer r.leave()
	t, ix, err := r.c.lookUpIndex(name)
	if err != nil {
		return nil, r.d.cacheError(ctx, "index", err)
	}
	if ix == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoIndex, name)
	}
	return t.createIndex(ix), nil
}

// applyCreateIndex of a commit object to the cache.
func applyCreateIndex(c *conn, ci *s4qlpb.CreateIndex) (int, error) {
	if reserved(ci.GetIndex()) {
		return 0, brokenf("it creates the index %q, whose name is reserved", ci.GetIndex())
	}
	t, err := existingTable(c, ci.GetTable(), "creates an index on")
	if err != nil {
		return 0, err
	}
	existing, err := c.lookUp(ci.GetIndex())
	if err != nil {
		return 0, err
	}
	if existing != nil {
		return 0, brokenf("it creates the index %q, and the table %q exists", ci.GetIndex(), existing.name)
	}
	_, other, err := c.lookUpIndex(ci.GetIndex())
	if err != nil {
		return 0, err
	}
	if other != nil {
		return 0, brokenf("it creates the index %q, and the index %q exists", ci.GetIndex(), other.name)
	}
	if len(ci.GetColumns()) > MaxIndexColumns {
		return 0, brokenf("it creates the index %q of %v columns, where the limit is %v", ci.GetIndex(), len(ci.GetColumns()), MaxIndexColumns)
	}
	ix, err := t.newIndex(ci)
	if err != nil {
		return 0, brokenf("it creates the index %q on %v", ci.GetIndex(), err)
	}
	return 1, c.createIndex(t, ix)
}

// applyDropIndex of a commit object to the cache.
func applyDropIndex(c *conn, name string) (int, error) {
	_, ix, err := c.lookUpIndex(name)
	if err != nil {
		return 0, err
	}
	if ix == nil {
		return 0, brokenf("it drops the index %q, which does not exist", name)
	}
	return 1, c.dropIndex(ix)
}

// IndexScan says what a scan by an index, or by the primary key, reads: the rows whose values in
// the leading columns of the key are Equal, and whose value in the column after those is within
// Lower and Upper.
//
// The key of an index is its columns and then the primary-key columns that it does not hold, as
// SQLite stores an index, so every key is of one row, and Equal and the bounds can reach into the
// primary-key columns at its end. The key of the primary key is its columns.
//
// Every value is compared as SQLite compares the values of the column. The bounds are stated by
// value, whatever the direction of the column in the index: Lower is the smallest value that the
// scan lets through, and Upper the largest, also for a column that the index holds in descending
// order. A row whose value in the bounded column is NULL is within no bound, as in SQL, and a
// column is pinned to NULL by a NULL in Equal, which finds the rows whose value is NULL.
type IndexScan struct {
	// Index is the name of the index, or empty for the primary key.
	Index string
	// Equal are the values of the leading columns of the key, in the order of the key: none, some,
	// or all of them.
	Equal []*s4qlpb.Value
	// Lower and Upper bound the column of the key after those of Equal. A bound without a value
	// bounds nothing.
	Lower, Upper Bound
	// Backward gives the rows in the reverse of the order of the key.
	Backward bool
}

// checkScan of the table by the given key: Equal pins no more columns than the key has, with a
// valid value for each, as [table.checkValue] checks it, so NULL only for a column that takes it;
// and a bound is on a column of the key, and its value is valid for it and not NULL, which no value
// is above or below.
func (t *table) checkScan(key []KeyColumn, scan IndexScan, limit int) error {
	if len(scan.Equal) > len(key) {
		return invalidf("a scan that pins %v columns of a key of the table %q, which has %v", len(scan.Equal), t.name, len(key))
	}
	for i, v := range scan.Equal {
		if err := t.checkValue(key[i].Position, v, limit); err != nil {
			return err
		}
	}
	for _, b := range []Bound{scan.Lower, scan.Upper} {
		switch {
		case b.Value == nil:
			continue
		case len(scan.Equal) == len(key):
			return invalidf("a scan with a bound after the last of the %v columns of a key of the table %q", len(key), t.name)
		case b.Value.WhichKind() == s4qlpb.Value_Null_case:
			return invalidf("a scan with a bound that is NULL, on the column %q of the table %q", t.columns[key[len(scan.Equal)].Position].name, t.name)
		}
		if err := t.checkValue(key[len(scan.Equal)].Position, b.Value, limit); err != nil {
			return err
		}
	}
	return nil
}

// scanConditions of a scan on the columns of its key, whose quoted names are given in the order of
// the key, with the arguments of their parameters: a term for each value of Equal, and one for each
// bound on the column after them. The terms compare values, so the direction of a column in the key
// changes none of them: SQLite turns a bound on a descending column into the right end of the
// range of the index.
func scanConditions(names []string, scan IndexScan) (conditions []string, args []any) {
	for i, v := range scan.Equal {
		if v.WhichKind() == s4qlpb.Value_Null_case {
			conditions = append(conditions, names[i]+" is null")
			continue
		}
		args = append(args, v)
		conditions = append(conditions, fmt.Sprint(names[i], " = ?", len(args)))
	}
	for _, b := range []struct {
		Bound
		op string
	}{{scan.Lower, ">"}, {scan.Upper, "<"}} {
		if b.Value == nil {
			continue
		}
		op := b.op
		if !b.Open {
			op += "="
		}
		args = append(args, b.Value)
		conditions = append(conditions, fmt.Sprint(names[len(scan.Equal)], " ", op, " ?", len(args)))
	}
	return conditions, args
}

// scanSQL is the statement of a scan of the table by its index ix, or by its primary key if ix is
// nil, which [table.checkScan] has accepted, with the arguments of its parameters. It fails as
// [table.checkIndexed] does.
//
// SQLite must read exactly that index, and seek exactly the bounds. The statement therefore names
// the index with "indexed by", which leaves SQLite no other, and names the index of the primary
// key to read the table itself: "not indexed" does nothing on a table without a rowid. Its only
// conditions are those of the scan, on a prefix of the key of the index without a gap, which SQLite
// uses in full to seek. It orders by the columns of the key that the scan does not pin, each in its
// direction in the key, or each in the other for a scan backward, which is the order in which
// SQLite reads the index, so it sorts nothing: unless those columns are more than 63, which is the
// most that SQLite compares with the order of an index, and it then reads the same rows and sorts
// them.
func (t *table) scanSQL(ix *index, scan IndexScan) (string, []any, error) {
	if err := t.checkIndexed(); err != nil {
		return "", nil, err
	}
	key := t.scanKey(ix)
	names := make([]string, len(key))
	for i, c := range key {
		names[i] = quote(t.columns[c.Position].name)
	}
	by := t.primary
	if ix != nil {
		by = ix.name
	}
	conditions, args := scanConditions(names, scan)
	sql := fmt.Sprint("select * from main.", quote(t.name), " indexed by ", quote(by))
	if len(conditions) > 0 {
		sql += " where " + strings.Join(conditions, " and ")
	}
	var order []string
	for i := len(scan.Equal); i < len(key); i++ {
		term := names[i]
		if key[i].Descending != scan.Backward {
			term += " desc"
		}
		order = append(order, term)
	}
	if len(order) > 0 {
		sql += " order by " + strings.Join(order, ", ")
	}
	return sql, args, nil
}

// scanIndex reads the rows of the table by its index ix, or by its primary key if ix is nil, as
// scan says, which [table.checkScan] has accepted, in the order of the key or its reverse. It adds
// one to read, if that is not nil, for each row that the cache gives. The statement is given back
// also when a panic or [runtime.Goexit] in the loop over the scan unwinds through it: a statement
// that stays stepped would hold its transaction's snapshot and locks, and the connection could not
// close.
func (c *conn) scanIndex(t *table, ix *index, scan IndexScan, read *int64) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		sql, args, err := t.scanSQL(ix, scan)
		if err != nil {
			yield(nil, err)
			return
		}
		s, err := c.prepare(sql)
		if err != nil {
			yield(nil, err)
			return
		}
		if err := bind(s, args...); err != nil {
			_ = c.rest(sql, s)
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
			if read != nil {
				*read++
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

// scanned is the table with the given name and its index that scan names, or nil for the primary
// key, for a scan that [table.checkScan] accepts on a connection with the given limit. Its error
// wraps [ErrNoIndex] if the table has no such index, and [ErrInvalid] or [ErrTooBig] as checkScan
// says.
func scanned(t *table, scan IndexScan, limit int) (*index, error) {
	var ix *index
	if scan.Index != "" {
		var err error
		if ix, err = t.index(scan.Index); err != nil {
			return nil, err
		}
		if ix == nil {
			return nil, fmt.Errorf("%w: %q on the table %q", ErrNoIndex, scan.Index, t.name)
		}
	}
	return ix, t.checkScan(t.scanKey(ix), scan, limit)
}

// ScanIndex is [ReadTx.Scan] by an index of the table, or by its primary key, of the rows that scan
// says, in the order of the key of the index, or its reverse: see [IndexScan]. The cache reads
// exactly that index, and seeks the bounds in it, so the scan reads no more rows than are within
// them: [ReadTx.RowsRead] counts them. For a key with more than 63 columns that the scan does not
// pin, SQLite sorts the rows, and reads every row within the bounds before it gives the first, so a
// loop over the sequence that stops early has still read them all. The sequence ends with an error
// that wraps [ErrNoTable] if there is no such table, [ErrNoIndex] if the table has no such index,
// [ErrInvalid] for a scan that pins more columns than the key has, for a bound after the last
// column of the key or that is NULL, and for a value that the column does not hold, and
// [ErrTooBig] for a text or a blob that is longer than SQLite takes.
func (r *ReadTx) ScanIndex(ctx context.Context, name string, scan IndexScan) iter.Seq2[[]*s4qlpb.Value, error] {
	return r.scan(ctx, name, func(t *table) (*index, IndexScan, error) {
		ix, err := scanned(t, scan, r.c.limit)
		return ix, scan, err
	})
}

// scan the table with the given name by the index, or the primary key, and as the scan says that
// plan gives for the table, or end the sequence with the error of plan.
func (r *ReadTx) scan(ctx context.Context, name string, plan func(t *table) (*index, IndexScan, error)) iter.Seq2[[]*s4qlpb.Value, error] {
	const operation = "scan"
	return func(yield func([]*s4qlpb.Value, error) bool) {
		if err := r.check(); err != nil {
			yield(nil, err)
			return
		}
		defer r.leave()
		t, err := r.table(ctx, operation, name)
		if err != nil {
			yield(nil, err)
			return
		}
		ix, scan, err := plan(t)
		if err != nil {
			yield(nil, err)
			return
		}
		for row, err := range r.c.scanIndex(t, ix, scan, &r.read) {
			if err == nil {
				// A read that is running when the instance stops is aborted.
				err = r.d.stoppedError()
			}
			if err != nil {
				yield(nil, r.d.cacheError(ctx, operation, err))
				return
			}
			if !yield(row, nil) {
				return
			}
		}
	}
}

// RowsRead is the number of rows that the transaction has read from the cache so far: one for each
// row that a scan stepped to, whether or not the loop over the scan went on to use it, and one for
// each row that [ReadTx.Get] found. It depends only on the rows of the database and on the calls,
// so a test can pin how many rows a query reads.
//
// It counts the rows that SQLite gave, and not the entries of an index that SQLite looked at to
// find them. The two are the same while SQLite seeks every bound of a scan, which the statement of
// [table.scanSQL] is written for and which the tests check with SQLite's plan of it.
//
// A scan whose key has more than 63 columns that the scan does not pin is the exception: SQLite
// sorts its rows, as [table.scanSQL] says, and reads every row within the bounds of the scan
// before it gives the first. A loop over such a scan that stops early has made SQLite read them
// all, and RowsRead counts only the rows that the loop stepped to.
func (r *ReadTx) RowsRead() int64 {
	return r.read
}

// ScanIndex is [Tx.Scan] by an index of the table, or by its primary key, as the transaction sees
// the rows, and as [ReadTx.ScanIndex] reads and fails, also where SQLite sorts the rows and so
// reads every row within the bounds before it gives the first. The transaction must not create or
// drop an index or a table while it scans.
func (tx *Tx) ScanIndex(ctx context.Context, name string, scan IndexScan) iter.Seq2[[]*s4qlpb.Value, error] {
	return tx.scan(ctx, name, func(t *table) (*index, IndexScan, error) {
		ix, err := scanned(t, scan, tx.c.limit)
		return ix, scan, err
	})
}

// scan the table with the given name by the index, or the primary key, and as the scan says that
// plan gives for the table, or end the sequence with the error of plan.
func (tx *Tx) scan(ctx context.Context, name string, plan func(t *table) (*index, IndexScan, error)) iter.Seq2[[]*s4qlpb.Value, error] {
	const operation = "scan"
	return func(yield func([]*s4qlpb.Value, error) bool) {
		if err := tx.check(ctx, operation); err != nil {
			yield(nil, err)
			return
		}
		defer tx.leave(ctx)
		t, err := tx.table(ctx, operation, name)
		if err != nil {
			yield(nil, err)
			return
		}
		ix, scan, err := plan(t)
		if err != nil {
			yield(nil, err)
			return
		}
		for row, err := range tx.c.scanIndex(t, ix, scan, &tx.read) {
			if err != nil {
				yield(nil, tx.fail(ctx, operation, err))
				return
			}
			// A scan is a call that the caller can keep running for long, so it looks at each row
			// whether the lease task has ended the transaction.
			if tx.marked.Load() {
				yield(nil, tx.Lost())
				return
			}
			if !yield(row, nil) {
				return
			}
		}
	}
}

// RowsRead is the number of rows that the transaction has read from the cache so far for its
// caller: by its scans and by [Tx.Get], and not by its writes or its commit. It counts them as
// [ReadTx.RowsRead] does, also for a scan whose rows SQLite sorts, of which it counts only the rows
// that the loop stepped to.
func (tx *Tx) RowsRead() int64 {
	return tx.read
}
