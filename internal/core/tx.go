package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sqlite"
	"maragu.dev/s4ql/internal/store"
)

// Tx is a write transaction. One runs at a time for each database: [DB.Begin] waits for the one
// before it. It changes the cache in a transaction of the cache's write connection, so it reads
// its own writes, and nothing else sees them until [Tx.Commit] has committed them. It ends with
// [Tx.Commit] or [Tx.Rollback], and must end with one of them.
//
// A transaction is not safe for concurrent use. If the instance stops serving while a transaction
// is open, every call of the transaction fails, and it is not committed. After a call fails with an
// error of the cache, the transaction can only be rolled back.
type Tx struct {
	d *DB
	c *conn
	// parent is the position of the cache when the transaction began: the head of the log that its
	// commit object follows.
	parent format.Name

	// tables that the transaction has changed, by their folded names, and in the order in which it
	// first changed them.
	tables map[string]*txTable
	order  []*txTable
	// temps counts the temporary tables that the transaction has made.
	temps int
	// notes holds the annotations, which a commit object holds after all its changes.
	notes       *spool
	annotations int

	// failed is the error after which the transaction can only be rolled back.
	failed error
	done   bool
}

// txTable is what a transaction has done to the table of one name, which is all that it needs to
// write the net effect of its changes to that table when it commits.
type txTable struct {
	// folded is the folded name.
	folded string
	// dropped is the name of the table that existed when the transaction began, if the transaction
	// dropped it, and empty otherwise.
	dropped string
	// created is the table that exists now, if the transaction created it. All its rows are new.
	created *s4qlpb.CreateTable
	// touched is the name of the temporary table that holds the primary keys of the rows that the
	// transaction upserted or deleted in a table that existed when it began, each with whether the
	// row existed then. It is empty if there is none. The keys are in the cache, not in memory, so
	// that a transaction can change any number of rows.
	touched string
}

// Begin a write transaction, waiting until no other is open. It fails at once, and whenever the
// instance stops serving while it waits, with an error that wraps [ErrNoLease] or [ErrStopped],
// with the error of the wait if ctx is done first, and with one that wraps [ErrBusy] if the cache
// is locked.
func (d *DB) Begin(ctx context.Context) (*Tx, error) {
	if err := d.acquireServing(ctx, d.writerLock); err != nil {
		return nil, d.refused(ctx, "begin", err)
	}
	if err := d.use(d.w); err != nil {
		d.writerLock.Release(ctx)
		return nil, err
	}
	defer d.unuse(d.w)
	if err := d.w.c.Exec("begin immediate"); err != nil {
		d.writerLock.Release(ctx)
		return nil, d.cacheError(ctx, "begin", err)
	}
	parent, err := d.w.position()
	if err != nil {
		_ = d.w.c.Exec("rollback")
		d.writerLock.Release(ctx)
		return nil, d.cacheError(ctx, "begin", err)
	}
	return &Tx{d: d, c: d.w, parent: parent, tables: map[string]*txTable{}}, nil
}

// Position of the cache when the transaction began: the name of the commit object whose state the
// transaction started from, or [format.EmptyLog].
func (tx *Tx) Position() format.Name {
	return tx.parent
}

// check that the transaction can go on with the given operation, and use its connection for the
// call, which gives it back with [Tx.leave].
func (tx *Tx) check(ctx context.Context, operation string) error {
	switch {
	case tx.done:
		return ErrTxDone
	case tx.failed != nil:
		return tx.failed
	}
	if err := tx.d.usable(); err != nil {
		return tx.d.refused(ctx, operation, err)
	}
	return tx.d.use(tx.c)
}

// leave a call of the transaction, which [Tx.check] began.
func (tx *Tx) leave() {
	tx.d.unuse(tx.c)
}

// fail the transaction for an error of the cache during the given operation: it can then only be
// rolled back. A local I/O failure also stops the instance.
func (tx *Tx) fail(ctx context.Context, operation string, err error) error {
	if err == nil {
		return nil
	}
	err = tx.d.cacheError(ctx, operation, err)
	if tx.failed == nil {
		tx.failed = fmt.Errorf("the transaction can only be rolled back, after: %w", err)
	}
	return err
}

// table with the given name, or an error that wraps [ErrNoTable].
func (tx *Tx) table(ctx context.Context, operation, name string) (*table, error) {
	t, err := tx.c.lookUp(name)
	if err != nil {
		return nil, tx.fail(ctx, operation, err)
	}
	if t == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoTable, name)
	}
	return t, nil
}

// changed is what the transaction has done to the table with the given name, which it is about to
// change.
func (tx *Tx) changed(name string) *txTable {
	folded := fold(name)
	e, ok := tx.tables[folded]
	if !ok {
		e = &txTable{folded: folded}
		tx.tables[folded] = e
		tx.order = append(tx.order, e)
	}
	return e
}

// checkCreateTable is one that a commit object can hold and the cache can make. Its error wraps
// [ErrInvalid] or [ErrReservedName].
func checkCreateTable(ct *s4qlpb.CreateTable, commitTime time.Time) error {
	// The writer of commit objects knows what a valid table is.
	w, err := format.NewCommitWriter(io.Discard, format.EmptyLog, commitTime)
	if err != nil {
		return err
	}
	if err := w.CreateTable(ct); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if reserved(ct.GetTable()) {
		return fmt.Errorf("%w: %q", ErrReservedName, ct.GetTable())
	}
	// SQLite's default limit.
	if len(ct.GetColumns()) > 2000 {
		return invalidf("a table of %v columns, where the limit is 2000", len(ct.GetColumns()))
	}
	// A row is found by a condition with a term for each column of its primary key, and SQLite
	// refuses a condition that is nested deeper than a thousand levels, which a few hundred terms
	// are.
	if len(ct.GetPrimaryKey()) > 250 {
		return invalidf("a primary key of %v columns, where the limit is 250", len(ct.GetPrimaryKey()))
	}
	for _, c := range ct.GetColumns() {
		if _, err := typeSQL(c.GetType()); err != nil {
			return err
		}
	}
	return nil
}

// CreateTable from its name, its columns, and its primary key. It fails with an error that wraps
// [ErrTableExists] if a table has the name, compared as SQLite compares names, with [ErrReservedName]
// for a name that starts with "s4ql_" or "sqlite_", and with [ErrInvalid] for a table that the
// database cannot hold.
func (tx *Tx) CreateTable(ctx context.Context, ct *s4qlpb.CreateTable) error {
	const operation = "create table"
	if err := tx.check(ctx, operation); err != nil {
		return err
	}
	defer tx.leave()
	if err := checkCreateTable(ct, tx.d.host.Wall()); err != nil {
		return err
	}
	existing, err := tx.c.lookUp(ct.GetTable())
	if err != nil {
		return tx.fail(ctx, operation, err)
	}
	if existing != nil {
		return fmt.Errorf("%w: %q", ErrTableExists, existing.name)
	}
	if err := tx.c.create(newTable(ct)); err != nil {
		return tx.fail(ctx, operation, err)
	}
	tx.changed(ct.GetTable()).created = proto.CloneOf(ct)
	return nil
}

// DropTable with the given name, and all its rows. It fails with an error that wraps [ErrNoTable]
// if there is none.
func (tx *Tx) DropTable(ctx context.Context, name string) error {
	const operation = "drop table"
	if err := tx.check(ctx, operation); err != nil {
		return err
	}
	defer tx.leave()
	t, err := tx.table(ctx, operation, name)
	if err != nil {
		return err
	}
	e := tx.changed(t.name)
	if err := tx.c.drop(t); err != nil {
		return tx.fail(ctx, operation, err)
	}
	if e.touched != "" {
		if err := tx.c.c.Exec(fmt.Sprint("drop table temp.", quote(e.touched))); err != nil {
			return tx.fail(ctx, operation, err)
		}
		e.touched = ""
	}
	if e.created != nil {
		// The transaction created the table, so dropping it leaves what was there before: nothing,
		// or the drop of the table that the transaction dropped first.
		e.created = nil
	} else {
		e.dropped = t.name
	}
	return nil
}

// touch the row of the table t with the given primary key, before the transaction changes it: if
// the table existed when the transaction began, the key is recorded, with whether the row exists.
// The first record of a key stays, so it says whether the row existed when the transaction began.
func (tx *Tx) touch(t *table, key []*s4qlpb.Value) error {
	e := tx.changed(t.name)
	if e.created != nil {
		return nil
	}
	if e.touched == "" {
		tx.temps++
		name := fmt.Sprint("s4ql_touched_", tx.temps)
		var b strings.Builder
		b.WriteString("create table temp.")
		b.WriteString(quote(name))
		b.WriteString(" (")
		for i, position := range t.key {
			typ, err := typeSQL(t.columns[position].typ)
			if err != nil {
				return err
			}
			fmt.Fprint(&b, "k", i, " ", typ, ", ")
		}
		b.WriteString("existed int not null, primary key (")
		b.WriteString(keyColumns("", len(t.key)))
		b.WriteString(")) strict, without rowid")
		if err := tx.c.c.Exec(b.String()); err != nil {
			return err
		}
		e.touched = name
	}
	sql := fmt.Sprint("insert or ignore into temp.", quote(e.touched), " values (", placeholders(len(t.key)),
		", exists (select 1 from main.", quote(t.name), " as t where ", t.keyCondition("t"), "))")
	return tx.c.exec(sql, values(key)...)
}

// keyColumns of a temporary table of touched keys: k0, k1, and so on, each after the prefix.
func keyColumns(prefix string, n int) string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprint(prefix, "k", i)
	}
	return strings.Join(names, ", ")
}

// Upsert a row into the table with the given name: insert it, or replace the row with the same
// primary key. The row has a value for each column of the table, in their order. It fails with an
// error that wraps [ErrNoTable] if there is no such table, and with [ErrInvalid] for a row that the
// table cannot hold.
func (tx *Tx) Upsert(ctx context.Context, name string, row []*s4qlpb.Value) error {
	const operation = "upsert"
	if err := tx.check(ctx, operation); err != nil {
		return err
	}
	defer tx.leave()
	t, err := tx.table(ctx, operation, name)
	if err != nil {
		return err
	}
	if err := t.checkRow(row); err != nil {
		return err
	}
	if err := tx.touch(t, t.keyOf(row)); err != nil {
		return tx.fail(ctx, operation, err)
	}
	return tx.fail(ctx, operation, tx.c.upsert(t, row))
}

// Delete the row with the given primary key from the table with the given name, and report
// whether there was one. The key has a value for each primary-key column, in key order. It fails
// with an error that wraps [ErrNoTable] if there is no such table, and with [ErrInvalid] for a key
// that is not one of the table.
func (tx *Tx) Delete(ctx context.Context, name string, key []*s4qlpb.Value) (bool, error) {
	const operation = "delete"
	if err := tx.check(ctx, operation); err != nil {
		return false, err
	}
	defer tx.leave()
	t, err := tx.table(ctx, operation, name)
	if err != nil {
		return false, err
	}
	if err := t.checkKey(key); err != nil {
		return false, err
	}
	if err := tx.touch(t, key); err != nil {
		return false, tx.fail(ctx, operation, err)
	}
	found, err := tx.c.delete(t, key)
	return found, tx.fail(ctx, operation, err)
}

// Annotate the transaction with a statement that it ran: its SQL text and its parameters. The
// annotations go into the commit object after its changes, in the order in which they were made.
// Replay never uses them. It fails with an error that wraps [ErrInvalid] for text that is not
// valid UTF-8 and for a parameter that is no value.
func (tx *Tx) Annotate(ctx context.Context, sql string, parameters []*s4qlpb.Value) error {
	const operation = "annotate"
	if err := tx.check(ctx, operation); err != nil {
		return err
	}
	defer tx.leave()
	a := s4qlpb.Annotation_builder{Statement: s4qlpb.Statement_builder{Sql: new(sql), Parameters: parameters}.Build()}.Build()
	// The writer of commit objects knows what a valid annotation is.
	w, err := format.NewCommitWriter(io.Discard, format.EmptyLog, tx.d.host.Wall())
	if err != nil {
		return err
	}
	if err := w.Annotate(a); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	if tx.notes == nil {
		if tx.notes, err = tx.d.newSpool("annotations"); err != nil {
			return tx.spoolFailed(ctx, err)
		}
	}
	if err := tx.notes.writeAnnotation(a); err != nil {
		return tx.spoolFailed(ctx, err)
	}
	tx.annotations++
	return nil
}

// spoolFailed stops the instance for a failure of a spool file, and fails the transaction.
func (tx *Tx) spoolFailed(ctx context.Context, err error) error {
	err = tx.d.spoolError(ctx, err)
	if tx.failed == nil {
		tx.failed = err
	}
	return err
}

// Table with the given name, as the transaction sees it, or an error that wraps [ErrNoTable].
func (tx *Tx) Table(ctx context.Context, name string) (*s4qlpb.CreateTable, error) {
	const operation = "table"
	if err := tx.check(ctx, operation); err != nil {
		return nil, err
	}
	defer tx.leave()
	t, err := tx.table(ctx, operation, name)
	if err != nil {
		return nil, err
	}
	return t.createTable(), nil
}

// Tables of the database, as the transaction sees them, in the order of their names as bytes.
func (tx *Tx) Tables(ctx context.Context) ([]*s4qlpb.CreateTable, error) {
	const operation = "tables"
	if err := tx.check(ctx, operation); err != nil {
		return nil, err
	}
	defer tx.leave()
	tables, err := tx.c.list()
	if err != nil {
		return nil, tx.fail(ctx, operation, err)
	}
	return createTables(tables), nil
}

func createTables(tables []*table) []*s4qlpb.CreateTable {
	cts := make([]*s4qlpb.CreateTable, len(tables))
	for i, t := range tables {
		cts[i] = t.createTable()
	}
	return cts
}

// Get the row with the given primary key from the table with the given name, as the transaction
// sees it, or nil if there is none. It fails as [Tx.Delete] does.
func (tx *Tx) Get(ctx context.Context, name string, key []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	const operation = "get"
	if err := tx.check(ctx, operation); err != nil {
		return nil, err
	}
	defer tx.leave()
	t, err := tx.table(ctx, operation, name)
	if err != nil {
		return nil, err
	}
	if err := t.checkKey(key); err != nil {
		return nil, err
	}
	row, err := tx.c.get(t, key)
	return row, tx.fail(ctx, operation, err)
}

// Scan the rows of the table with the given name, as the transaction sees them, in the order of
// the primary key. The sequence ends with an error if the scan fails, which wraps [ErrNoTable] if
// there is no such table. The transaction must not change the table while it scans it, and must
// not drop any table: SQLite refuses the drop, and the transaction can then only be rolled back.
func (tx *Tx) Scan(ctx context.Context, name string) iter.Seq2[[]*s4qlpb.Value, error] {
	const operation = "scan"
	return func(yield func([]*s4qlpb.Value, error) bool) {
		if err := tx.check(ctx, operation); err != nil {
			yield(nil, err)
			return
		}
		defer tx.leave()
		t, err := tx.table(ctx, operation, name)
		if err != nil {
			yield(nil, err)
			return
		}
		for row, err := range tx.c.scan(t) {
			if err != nil {
				yield(nil, tx.fail(ctx, operation, err))
				return
			}
			if !yield(row, nil) {
				return
			}
		}
	}
}

// Rollback the transaction: nothing of it is kept.
func (tx *Tx) Rollback(ctx context.Context) error {
	if tx.done {
		return ErrTxDone
	}
	defer tx.end(ctx)
	if tx.d.use(tx.c) != nil {
		// [DB.Close] closed the connection, which rolled the transaction of the cache back.
		return nil
	}
	defer tx.leave()
	return tx.rollback(ctx)
}

// rollback the transaction of the cache.
func (tx *Tx) rollback(ctx context.Context) error {
	tx.c.forget()
	return tx.d.cacheError(ctx, "rollback", tx.c.c.Exec("rollback"))
}

// end the transaction: give back what it holds, and let the next one in.
func (tx *Tx) end(ctx context.Context) {
	if tx.done {
		return
	}
	tx.done = true
	if tx.d.use(tx.c) == nil {
		tx.c.forget()
		tx.leave()
	}
	tx.notes.close()
	tx.d.writerLock.Release(ctx)
}

// spooled is the commit object of a transaction in its spool file.
type spooled struct {
	spool *spool
	name  format.Name
	// changes is the number of tables created and dropped and of rows upserted and deleted.
	changes int
}

// spoolCommit writes the commit object of the transaction to a spool file: the net effect of what
// the transaction did, and then its annotations.
//
// For each table name that the transaction changed, in the order in which it first changed them,
// the net effect is: the drop of the table that was there when the transaction began, if the
// transaction dropped it; then the table that is there now and all its rows, if the transaction
// created it; and otherwise the rows that the transaction changed and that are there now, and the
// keys of the rows that it changed, that were there when it began, and that are gone. So a primary
// key appears at most once for each table, a table that is created and dropped leaves nothing, and
// a table that is dropped and created again is a drop, a create, and its rows.
func (tx *Tx) spoolCommit(ctx context.Context) (_ *spooled, err error) {
	d := tx.d
	sp, err := d.newSpool("commit")
	if err != nil {
		return nil, d.spoolError(ctx, err)
	}
	defer func() {
		if err != nil {
			sp.close()
		}
	}()
	// fail tells an error of the cache from a refusal of the writer of the commit object and from
	// an error of the spool file.
	fail := func(err error) error {
		switch {
		case isSQLite(err) || errors.Is(err, ErrCacheVersion):
			return d.cacheError(ctx, "commit", err)
		case errors.Is(err, format.ErrMalformed):
			// The writer refuses a commit time that it cannot write, which is the wall clock's.
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		return d.spoolError(ctx, err)
	}
	w, err := format.NewCommitWriter(sp, tx.parent, d.host.Wall())
	if err != nil {
		return nil, fail(err)
	}
	out := &spooled{spool: sp}

	for _, e := range tx.order {
		if e.dropped != "" {
			if err := w.DropTable(e.dropped); err != nil {
				return nil, fail(err)
			}
			out.changes++
		}
		switch {
		case e.created != nil:
			if err := w.CreateTable(e.created); err != nil {
				return nil, fail(err)
			}
			out.changes++
			t, err := tx.c.lookUp(e.created.GetTable())
			if err != nil {
				return nil, fail(err)
			}
			if t == nil {
				return nil, fail(fmt.Errorf("%w: the table %q that the transaction created is gone", ErrCacheVersion, e.created.GetTable()))
			}
			columns := positions(len(t.columns))
			for row, err := range tx.c.scan(t) {
				if err != nil {
					return nil, fail(err)
				}
				if err := w.UpsertRow(t.name, columns, row); err != nil {
					return nil, fail(err)
				}
				out.changes++
			}

		case e.touched != "":
			n, err := tx.spoolTouched(w, e)
			if err != nil {
				return nil, fail(err)
			}
			out.changes += n
		}
	}

	if tx.notes != nil {
		if err := tx.notes.annotations(w.Annotate); err != nil {
			return nil, fail(err)
		}
	}
	if out.name, err = w.Close(); err != nil {
		return nil, fail(err)
	}
	if err := sp.w.Flush(); err != nil {
		return nil, fail(err)
	}
	return out, nil
}

// positions from 0 to n-1, as a commit object lists columns.
func positions(n int) []uint32 {
	ps := make([]uint32, n)
	for i := range ps {
		ps[i] = uint32(i)
	}
	return ps
}

// spoolTouched writes the net effect on a table that existed when the transaction began and that
// it did not drop: the rows whose keys it touched and that are there, and the keys that it touched
// of rows that were there and are gone. It returns the number of both.
func (tx *Tx) spoolTouched(w *format.CommitWriter, e *txTable) (int, error) {
	d := tx.d
	// The folded name of the table finds it, whatever case the transaction used.
	t, err := tx.c.lookUp(e.folded)
	if err != nil {
		return 0, err
	}
	if t == nil {
		return 0, fmt.Errorf("%w: a table that the transaction changed is gone", ErrCacheVersion)
	}

	on := make([]string, len(t.key))
	for i, position := range t.key {
		on[i] = fmt.Sprint("t.", quote(t.columns[position].name), " = k.k", i)
	}
	match := strings.Join(on, " and ")
	order := keyColumns("k.", len(t.key))
	columns := positions(len(t.columns))

	var changes int
	upserts := fmt.Sprint("select t.* from temp.", quote(e.touched), " as k join main.", quote(t.name), " as t on ", match, " order by ", order)
	if err := tx.c.query(upserts, func(s *sqlite.Stmt) error {
		row, err := readRow(s, 0, len(t.columns))
		if err != nil {
			return err
		}
		changes++
		if d.bugs.keyTwice {
			// The planted bug.
			if err := w.UpsertRow(t.name, columns, row); err != nil {
				return err
			}
		}
		return w.UpsertRow(t.name, columns, row)
	}); err != nil {
		return 0, err
	}

	if d.bugs.forgetDelete {
		// The planted bug.
		return changes, nil
	}
	deletes := fmt.Sprint("select ", order, " from temp.", quote(e.touched), " as k where k.existed = 1 and not exists (select 1 from main.",
		quote(t.name), " as t where ", match, ") order by ", order)
	if err := tx.c.query(deletes, func(s *sqlite.Stmt) error {
		key, err := readRow(s, 0, len(t.key))
		if err != nil {
			return err
		}
		changes++
		return w.DeleteRow(t.name, key)
	}); err != nil {
		return 0, err
	}
	return changes, nil
}

// dropTemps drops the temporary tables of the transaction, before the cache commits.
func (tx *Tx) dropTemps() error {
	tx.c.forget()
	for _, e := range tx.order {
		if e.touched == "" {
			continue
		}
		if err := tx.c.c.Exec(fmt.Sprint("drop table temp.", quote(e.touched))); err != nil {
			return err
		}
		e.touched = ""
	}
	return nil
}

// Commit the transaction. It returns nil if and only if the transaction is known to be committed.
//
// It writes the commit object of the transaction to a local file, uploads it under its name, which
// is its checksum, swaps the manifest for one whose head is that name, and then commits the
// transaction of the cache, which records that name as the position of the cache. The swap is the
// commit: the transaction is committed when the swap is applied, and not before. If the cache then
// fails to commit, Commit still returns nil, and the instance stops, so that no read starts on a
// cache that lacks the transaction. A transaction that changed nothing writes no commit object,
// and commits if the lease is valid by the clocks, as a read does.
//
// If the swap ends without an answer, the instance asks the store until it knows: it sends the
// same swap again, and after a refusal it reads the manifest, and looks for the commit object in
// the log if another instance has taken the lease. It goes on with that when the instance stops
// meanwhile. It returns an error that wraps [ErrOutcomeUnknown], and no other error, only if ctx
// is done before it knows, or if it has to look for the commit object in a log that holds a commit
// object that can never be read. The instance then serves nothing until it has found out, and
// finds out without the caller: see [DB.resumeLocked]. Every other error means that the
// transaction is not committed: it wraps [ErrNoLease] if the instance did not hold the lease any
// more, [ErrStopped] if the instance has stopped, and [ErrTooLarge] if the store refuses the commit
// object as too large, which it does at once, without trying again.
func (tx *Tx) Commit(ctx context.Context) (err error) {
	d := tx.d
	if tx.done {
		return ErrTxDone
	}
	defer tx.end(ctx)

	clock := d.stopwatch()
	outcome := "not_committed"
	report := commitReport{parent: tx.parent}
	attempts := 0
	defer func() {
		// A commit that succeeded is routine, and its event is only for debugging.
		level := slog.LevelDebug
		if err != nil || outcome == "committed_and_stopped" {
			level = slog.LevelWarn
		}
		if d.enabled(ctx, level) {
			d.event(ctx, level, eventCommit, report.attrs(outcome, attempts, clock.total(), err)...)
		}
	}()

	// The commit uses the connection until it knows its outcome, also while it finds that out after
	// a stop, because it then commits or rolls back the transaction of the cache.
	if err := d.use(tx.c); err != nil {
		return err
	}
	defer tx.leave()
	if tx.failed != nil {
		_ = tx.rollback(ctx)
		return tx.failed
	}
	if err := d.usable(); err != nil {
		_ = tx.rollback(ctx)
		return d.refused(ctx, "commit", err)
	}

	out, err := tx.spoolCommit(ctx)
	if err != nil {
		_ = tx.rollback(ctx)
		return fmt.Errorf("commit: %w", err)
	}
	defer out.spool.close()
	report.spooled, report.name, report.changes, report.annotations, report.bytes = true, out.name, out.changes, tx.annotations, out.spool.size
	report.phase(clock.phase(attrDurationSpool))

	if out.changes == 0 {
		if err := tx.rollback(ctx); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		d.mu.Lock()
		err := d.servingLocked()
		d.mu.Unlock()
		if err != nil {
			return d.refused(ctx, "commit", err)
		}
		outcome = "empty"
		return nil
	}

	// All that the cache still has to do after the swap is to commit.
	cacheErr := tx.dropTemps()
	if cacheErr == nil {
		cacheErr = tx.c.setPosition(out.name)
	}
	if cacheErr != nil {
		cacheErr = d.cacheError(ctx, "commit", cacheErr)
		_ = tx.rollback(ctx)
		return fmt.Errorf("commit: %w", cacheErr)
	}
	if d.bugs.cacheFirst {
		// The planted bug: the cache has the transaction before the log does.
		if err := tx.c.c.Exec("commit"); err != nil {
			return d.cacheError(ctx, "commit", err)
		}
	}

	// The upload needs no lock, so the lease is renewed while a large commit object is on its way.
	if err := tx.upload(ctx, out, &attempts); err != nil {
		_ = tx.rollback(ctx)
		return fmt.Errorf("commit: not committed: %w", err)
	}
	report.phase(clock.phase(attrDurationUpload))

	result, err := tx.publish(ctx, out.name, &attempts)
	report.phase(clock.phase(attrDurationSwap))
	switch result {
	case published:
		d.host.Yield(ctx, "core: a commit between the manifest swap and the cache")
		outcome = "committed"
		if !d.bugs.cacheFirst {
			if err := tx.commitCache(ctx); err != nil && !d.bugs.ignoreCacheError {
				_ = tx.c.c.Exec("rollback")
				_ = d.stop(ctx, fmt.Errorf("%w: the commit of the cache after the manifest swap: %w", ErrLocal, err))
				outcome = "committed_and_stopped"
			}
		}
		report.phase(clock.phase(attrDurationCache))
		return nil

	case unknown:
		_ = tx.rollback(ctx)
		outcome = "unknown"
		// The error wraps nothing else: every other error of a commit means "not committed".
		return fmt.Errorf("commit: %w: %v", ErrOutcomeUnknown, err)
	}
	_ = tx.rollback(ctx)
	return fmt.Errorf("commit: not committed: %w", err)
}

// commitReport is what the event of a commit says, kept as plain values until the event is logged,
// so that a commit whose event the logger drops builds no attributes.
type commitReport struct {
	parent format.Name
	// spooled is whether the commit object was written, and the fields up to bytes then say what it
	// holds.
	spooled              bool
	name                 format.Name
	changes, annotations int
	bytes                int64
	// phases that have ended, each as its duration.
	phases []slog.Attr
}

// phase that has ended.
func (r *commitReport) phase(a slog.Attr) {
	r.phases = append(r.phases, a)
}

// attrs of the event of the commit, which ended with the outcome after the attempts.
func (r *commitReport) attrs(outcome string, attempts int, total slog.Attr, err error) []slog.Attr {
	attrs := []slog.Attr{slog.String(attrParent, r.parent.String())}
	if r.spooled {
		attrs = append(attrs, slog.String(attrCommit, r.name.String()), slog.Int(attrChanges, r.changes),
			slog.Int(attrAnnotations, r.annotations), slog.Int64(attrBytes, r.bytes))
	}
	attrs = append(attrs, r.phases...)
	return append(attrs, slog.String(attrOutcome, outcome), slog.Int(attrAttempts, attempts), total, errAttr(err))
}

// spoolReader reads the spool file of a commit object for an upload, and keeps the first error of
// the file, so that a failure of the file is told from a failure of the upload.
type spoolReader struct {
	s   *spool
	err error
}

func (r *spoolReader) Read(p []byte) (int, error) {
	n, err := r.s.f.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && r.err == nil {
		r.err = err
	}
	return n, err
}

func (r *spoolReader) Seek(offset int64, whence int) (int64, error) {
	n, err := r.s.f.Seek(offset, whence)
	if err != nil && r.err == nil {
		r.err = err
	}
	return n, err
}

// upload the commit object under its name, with its checksum, if no object is there. An object
// that is there already can only be this commit object, left by an upload that reported a failure,
// so it counts as success. An upload that ends without an answer is sent again until ctx is done,
// which is harmless, because the object is named by its content and created only if absent. An
// upload that the store refuses as too large fails at once with [ErrTooLarge].
func (tx *Tx) upload(ctx context.Context, out *spooled, attempts *int) error {
	d := tx.d
	sum := store.Checksum(out.name)
	wait := retrySoon
	var mismatches int
	for {
		if err := d.usable(); err != nil {
			return err
		}
		if err := out.spool.rewind(); err != nil {
			return d.spoolError(ctx, err)
		}
		content := &spoolReader{s: out.spool}
		*attempts++
		rctx, cancel := d.host.WithTimeout(ctx, d.options.ObjectTimeout)
		_, err := d.store.Create(rctx, d.commitKey(out.name), content, &sum)
		cancel()
		switch {
		case content.err != nil:
			return d.spoolError(ctx, content.err)
		case err == nil, errors.Is(err, store.ErrExists):
			return nil
		case errors.Is(err, store.ErrTooLarge):
			// Every attempt sends the same bytes, so none can succeed.
			return fmt.Errorf("%w: %v", ErrTooLarge, err)
		case errors.Is(err, store.ErrChecksum):
			// The bytes may have been damaged on their way once. Twice, it is the file.
			if mismatches++; mismatches == 2 {
				return d.spoolError(ctx, fmt.Errorf("the commit object read back from it does not have its checksum: %w", err))
			}
		}
		if sleepErr := d.host.Sleep(ctx, wait); sleepErr != nil {
			return fmt.Errorf("upload of the commit object %v: %w", out.name, after(sleepErr, err))
		}
		wait = min(2*wait, retryAfter)
	}
}

// publishing is what became of the manifest swap of a commit.
type publishing int

const (
	// unpublished: the commit object is not in the log, and never will be.
	unpublished publishing = iota
	// published: the commit object is in the log.
	published
	// unknown: the caller gave up, or the instance stopped, before the instance knew.
	unknown
)

// publish the commit object with the given name, which is uploaded: swap the manifest for one
// whose head it is, and find out what became of the swap. It holds the manifest lock from the
// first attempt until it knows, so no renewal is sent in between, and reads go on only for as long
// as the clocks say that the lease holds.
func (tx *Tx) publish(ctx context.Context, name format.Name, attempts *int) (publishing, error) {
	d := tx.d
	held := locks{d: d}
	defer held.release(ctx)
	if err := held.acquireManifest(ctx); err != nil {
		return unpublished, err
	}

	var f *flight
	var last error
	wait := retrySoon
	for {
		// An instance that has stopped sends no commit. One whose commit is sent and may be in the
		// log goes on finding out, because the caller is owed an answer for as long as it waits.
		if err := d.stoppedError(); err != nil && (f == nil || !f.unknown) {
			return unpublished, err
		}
		if f == nil {
			if !d.holds() {
				return unpublished, errLeaseLost
			}
			if d.man.Head != tx.parent {
				return unpublished, fmt.Errorf("the log is at %v, and the transaction began at %v", d.man.Head, tx.parent)
			}
			var err error
			if f, err = d.newFlight(name, true); err != nil {
				return unpublished, err
			}
		}
		if ctx.Err() != nil {
			if !f.unknown {
				// Every attempt was refused, or none was sent.
				return unpublished, after(context.Cause(ctx), last)
			}
			if d.bugs.ackUnknown {
				// The planted bug.
				return published, nil
			}
			// The instance serves nothing until it knows, or the application could look for the
			// transaction in a cache that lacks it, conclude that it is not committed, and repeat
			// it, while the request that is stuck commits the first one.
			d.mu.Lock()
			if !d.bugs.serveUnknown {
				d.serving, d.unresolved = false, true
			}
			d.wakeLocked()
			d.mu.Unlock()
			return unknown, after(context.Cause(ctx), last)
		}

		*attempts++
		result, err := d.send(ctx, f, nil)
		switch result {
		case sendApplied:
			return published, nil
		case sendUnknown:
			// The same swap is sent again. If that is applied, the transaction is committed,
			// whether the first attempt arrived or not.
			last = err
		case sendRefused:
			// A refusal does not say that an earlier attempt was applied: the store may refuse a
			// swap from the right version while another write to the manifest is on its way. So
			// the manifest is read.
			*attempts++
			o, landed, m, err := d.observe(ctx)
			switch o {
			case observedNothing:
				last = err
			case observedUnchanged:
				// Nothing was applied. The same swap is sent again.
			case observedLanded:
				if landed == f {
					return published, nil
				}
				// A renewal of this instance was applied, from the version that every attempt of
				// the commit was sent from, so none of them can be applied any more. The instance
				// still holds the lease, so the commit is sent again from the new version, as a new
				// swap.
				f = nil
				continue
			case observedLost:
				if !f.unknown || d.bugs.lostMeansFailed {
					// Every attempt was refused. (With lostMeansFailed, the planted bug.)
					return unpublished, errLeaseLost
				}
				// An attempt may have been applied before the other instance took the lease, and
				// that instance may have committed on top of it, so the manifest alone does not
				// tell: the log does.
				return tx.inLog(ctx, m.Head, name)
			}
		}
		_ = d.host.Sleep(ctx, wait)
		wait = min(2*wait, retryAfter)
	}
}

// inLog finds out whether the commit object with the given name is in the log that ends at head,
// by walking the log back from head to the parent of the transaction. The instance no longer
// holds the lease. If the log holds a commit object that can never be read, nothing can tell, and
// the outcome stays unknown.
func (tx *Tx) inLog(ctx context.Context, head, name format.Name) (publishing, error) {
	d := tx.d
	wait := retrySoon
	for at := head; ; {
		switch at {
		case name:
			return published, nil
		case tx.parent, format.EmptyLog:
			return unpublished, errLeaseLost
		}
		var parent format.Name
		err := d.download(ctx, at, func(body io.Reader) error {
			var err error
			parent, err = parentOf(at, body)
			return err
		})
		if err == nil {
			at, wait = parent, retrySoon
			continue
		}
		if unreadable(err) {
			d.mu.Lock()
			d.unresolved = true
			d.mu.Unlock()
			return unknown, err
		}
		if ctx.Err() != nil {
			d.mu.Lock()
			d.unresolved = true
			d.mu.Unlock()
			return unknown, after(context.Cause(ctx), err)
		}
		_ = d.host.Sleep(ctx, wait)
		wait = min(2*wait, retryAfter)
	}
}

// after is the error err, with the error that came before it, if there is one.
func after(err, before error) error {
	if before == nil {
		return err
	}
	return fmt.Errorf("%w, after: %w", err, before)
}

// commitCache commits the transaction of the cache, after the manifest swap. Under buggify it
// fails as a cache can: after rolling back, or after committing, because SQLite can report an I/O
// error from a commit that is durable.
func (tx *Tx) commitCache(ctx context.Context) error {
	d := tx.d
	// SQLITE_IOERR.
	const ioError = 10
	if d.host.Buggify(ctx, "core: the cache fails to commit after the manifest swap") {
		if d.host.Uint64()%2 == 0 {
			_ = tx.c.c.Exec("rollback")
			return &sqlite.Error{Code: ioError, Message: "injected: the commit was rolled back"}
		}
		if err := tx.c.c.Exec("commit"); err != nil {
			return err
		}
		return &sqlite.Error{Code: ioError, Message: "injected: the commit went through and reported an error"}
	}
	return tx.c.c.Exec("commit")
}
