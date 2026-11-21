package core

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

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
// is open, every call of the transaction fails, and it is not committed. If the instance has lost
// its lease, it ends the transaction unless it is committing, so that it can take the lease back:
// every later call then fails with an error that wraps [ErrTxLeaseLost], and Rollback returns nil.
// See [DB.endForLease]. After a call fails with an error of the cache, the transaction can only be
// rolled back.
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

	// droppedIndexes are the names of the indexes that existed when the transaction began and that
	// it dropped, in the order in which it dropped them, and createdIndexes the folded names of the
	// indexes that it created and that exist. An index that went with its table when the
	// transaction dropped the table is in neither.
	droppedIndexes []string
	createdIndexes map[string]bool

	// read counts the rows that the scans and the gets of the transaction have read from the cache.
	read int64

	// saved is the state at the savepoint that is open, or nil.
	saved *savepoint

	// failed is the error after which the transaction can only be rolled back.
	failed error
	done   bool

	// ending is set when [Tx.Commit] or [Tx.Rollback] begins, after which the lease task leaves the
	// transaction alone, and lease when the lease task has ended it. Both are guarded by [DB.mu].
	ending bool
	lease  *leaseEnd
	// marked is set when lease is, so that a scan looks at each row without the mutex.
	marked atomic.Bool
}

// leaseEnd is the end of a write transaction by the lease task, because the instance lost its
// lease: see [DB.endForLease]. It is guarded by [DB.mu].
type leaseEnd struct {
	// err is the error of every later call of the transaction, and reason why the instance did not
	// serve, in a word.
	err    error
	reason string
	// at is when the lease task ended the transaction, by the monotonic clock.
	at time.Duration
	// waited is whether a call of the transaction ran then, which ends the transaction when it
	// leaves, and done whether the end is under way or over: the roll back of the transaction of
	// the cache, and the release of the writer lock.
	waited, done bool
}

// savepoint is what [Tx.RollbackTo] restores of a transaction, beside the cache, which SQLite's
// savepoint restores: what it had done to each table, and the number of its annotations.
type savepoint struct {
	// tables are copies of the entries of [Tx.order], in their order.
	tables      []txTable
	annotations int
	// droppedIndexes and createdIndexes are copies of those of the transaction.
	droppedIndexes []string
	createdIndexes map[string]bool
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
	// touched and existed are the names of the two temporary tables that hold primary keys of the
	// rows that the transaction upserted or deleted in a table that existed when it began: touched
	// all of them, and existed those of the rows that existed then. Both are empty if there are
	// none. The keys are in the cache, not in memory, so that a transaction can change any number of
	// rows. A temporary table holds a key and nothing else, so that SQLite never refuses a key there
	// as too big when it took the row in the table: see [Tx.touch].
	touched, existed string
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
	if err := d.w.c.Exec("begin immediate"); err != nil {
		d.unuse(d.w)
		d.writerLock.Release(ctx)
		return nil, d.cacheError(ctx, "begin", err)
	}
	parent, err := d.w.position()
	if err != nil {
		_ = d.w.c.Exec("rollback")
		d.unuse(d.w)
		d.writerLock.Release(ctx)
		return nil, d.cacheError(ctx, "begin", err)
	}

	// The transaction is recorded, and the connection given back, in the critical section that finds
	// the instance serving, so that the lease task, which ends the transaction when the instance has
	// lost its lease, either finds it or finds no transaction that can still begin.
	tx := &Tx{d: d, c: d.w, parent: parent, tables: map[string]*txTable{}}
	d.mu.Lock()
	err = d.usableLocked()
	if err == nil {
		d.writing = tx
		d.w.users--
	}
	d.mu.Unlock()
	if err != nil {
		_ = d.w.c.Exec("rollback")
		d.unuse(d.w)
		d.writerLock.Release(ctx)
		return nil, d.refused(ctx, "begin", err)
	}
	return tx, nil
}

// Position of the cache when the transaction began: the name of the commit object whose state the
// transaction started from, or [format.EmptyLog].
func (tx *Tx) Position() format.Name {
	return tx.parent
}

// check that the transaction can go on with the given operation, and use its connection for the
// call, which gives it back with [Tx.leave].
func (tx *Tx) check(ctx context.Context, operation string) error {
	if err := tx.state(); err != nil {
		return err
	}
	if err := tx.d.usable(); err != nil {
		return tx.d.refused(ctx, operation, err)
	}
	return tx.use()
}

// state of the transaction: [ErrTxDone] if it has ended, the error of the end by the lease task, the
// error after which it can only be rolled back, or nil.
func (tx *Tx) state() error {
	if tx.done {
		return ErrTxDone
	}
	if err := tx.Lost(); err != nil {
		return err
	}
	return tx.failed
}

// Lost is nil unless the instance ended the transaction because it lost its lease. It is then the
// error that every call of the transaction gives until Commit or Rollback, which wraps
// [ErrTxLeaseLost], and Lost goes on giving it after them.
func (tx *Tx) Lost() error {
	tx.d.mu.Lock()
	defer tx.d.mu.Unlock()
	if tx.lease == nil {
		return nil
	}
	return tx.lease.err
}

// use the connection of the transaction for a call, unless the lease task has ended the
// transaction, in the same critical section in which the lease task looks whether a call uses it.
func (tx *Tx) use() error {
	tx.d.mu.Lock()
	defer tx.d.mu.Unlock()
	if tx.lease != nil {
		return tx.lease.err
	}
	return tx.d.useLocked(tx.c)
}

// useToEnd is [Tx.use] for [Tx.Commit] and [Tx.Rollback], which marks the transaction as ending
// first, so that the lease task does not end it under them. It fails with the error of the end by
// the lease task, before it marks anything, if there was one.
func (tx *Tx) useToEnd() error {
	tx.d.mu.Lock()
	defer tx.d.mu.Unlock()
	if tx.lease != nil {
		return tx.lease.err
	}
	tx.ending = true
	return tx.d.useLocked(tx.c)
}

// leave a call of the transaction, which [Tx.check] began. If the lease task has ended the
// transaction while the call ran, the last call to leave rolls it back in the cache and gives back
// the writer lock.
func (tx *Tx) leave(ctx context.Context) {
	d := tx.d
	d.mu.Lock()
	last := tx.lease != nil && !tx.lease.done && tx.c.users == 1
	if last {
		tx.lease.done = true
	}
	d.mu.Unlock()
	if last {
		tx.endForLease(ctx)
		return
	}
	d.unuse(tx.c)
}

// endForLease rolls back the transaction of the cache, gives back its connection, and frees the
// writer lock, for a transaction that the lease task has ended: see [DB.endForLease]. The caller
// uses the connection, as the only one, and has set [leaseEnd.done].
func (tx *Tx) endForLease(ctx context.Context) {
	d := tx.d
	tx.c.forget()
	err := d.cacheError(ctx, "rollback", tx.c.c.Exec("rollback"))
	d.mu.Lock()
	d.writing = nil
	e := *tx.lease
	d.mu.Unlock()
	d.unuse(tx.c)
	d.writerLock.Release(ctx)

	outcome, level := "rolled_back", slog.LevelInfo
	if err != nil {
		outcome, level = "failed", slog.LevelWarn
	}
	d.event(ctx, level, eventTxEnded, slog.String(attrOutcome, outcome), slog.String(attrReason, e.reason),
		slog.String(attrParent, tx.parent.String()), slog.Bool(attrTxWaited, e.waited),
		slog.Duration(attrDurationTotal, d.host.Monotonic()-e.at), errAttr(err))
}

// Serving returns nil if the instance serves now, as [DB.Begin] requires it: it holds the lease, its
// cache is caught up, and the lease is valid by both clocks. Otherwise it returns the error of the
// stop, or one that wraps [ErrNoLease]. It is the check before each statement of a transaction that
// spans statements, so that such a statement is refused as a statement of its own would be. The other
// calls of a transaction that is open do not look at the clocks, because its commit is judged by its
// manifest swap. It also fails with [ErrTxDone] on a transaction that has ended, and with the error
// after which the transaction can only be rolled back.
func (tx *Tx) Serving(ctx context.Context) error {
	if err := tx.state(); err != nil {
		return err
	}
	tx.d.mu.Lock()
	err := tx.d.servingLocked()
	tx.d.mu.Unlock()
	if err != nil {
		return tx.d.refused(ctx, "statement", err)
	}
	return nil
}

// Savepoint marks the state of the transaction, which [Tx.RollbackTo] restores and [Tx.Release]
// keeps, so that a statement in a transaction of several statements can fail without an effect, as
// SQLite's statement does. One savepoint is open at a time. It fails as the calls that change the
// transaction do, and with an error that wraps [ErrInvalid] while a savepoint is open.
func (tx *Tx) Savepoint(ctx context.Context) error {
	const operation = "savepoint"
	if err := tx.check(ctx, operation); err != nil {
		return err
	}
	defer tx.leave(ctx)
	if tx.saved != nil {
		return invalidf("a savepoint while one is open")
	}
	if err := tx.c.c.Exec("savepoint s4ql_statement"); err != nil {
		return tx.fail(ctx, operation, err)
	}
	sp := &savepoint{annotations: tx.annotations}
	tx.saveIndexes(sp)
	for _, e := range tx.order {
		sp.tables = append(sp.tables, *e)
	}
	tx.saved = sp
	return nil
}

// Release the savepoint that is open: the transaction keeps what it did since. It does not need the
// instance to serve, because nothing is read or written then. It fails with an error that wraps
// [ErrInvalid] if no savepoint is open, with [ErrTxDone] on a transaction that has ended, with the
// error after which the transaction can only be rolled back, and with the error of the stop once
// [DB.Close] has closed the connection; on an instance that has stopped and is not closed, it still
// runs. After an error of the cache, the transaction can only be rolled back.
func (tx *Tx) Release(ctx context.Context) error {
	const operation = "release"
	if err := tx.checkOpen(); err != nil {
		return err
	}
	defer tx.leave(ctx)
	if tx.saved == nil {
		return invalidf("a release without a savepoint")
	}
	tx.saved = nil
	return tx.fail(ctx, operation, tx.c.c.Exec("release s4ql_statement"))
}

// RollbackTo the savepoint that is open, and end it: the transaction is as it was at
// [Tx.Savepoint], in the cache and in what it commits. It does not need the instance to serve. It
// fails as [Tx.Release] does. An annotation cannot be taken back, so a rollback to a savepoint
// before which the transaction had fewer annotations fails the transaction, which can then only be
// rolled back.
func (tx *Tx) RollbackTo(ctx context.Context) error {
	const operation = "rollback to"
	if err := tx.checkOpen(); err != nil {
		return err
	}
	defer tx.leave(ctx)
	sp := tx.saved
	if sp == nil {
		return invalidf("a rollback to no savepoint")
	}
	tx.saved = nil
	if tx.annotations != sp.annotations {
		tx.failed = fmt.Errorf("the transaction can only be rolled back, after: %w", invalidf("a rollback to a savepoint before an annotation"))
		return tx.failed
	}
	// The schema of the cache may change back, so nothing that was looked up or prepared stays.
	tx.c.forget()
	if err := tx.c.c.Exec("rollback to s4ql_statement"); err != nil {
		return tx.fail(ctx, operation, err)
	}
	if err := tx.c.c.Exec("release s4ql_statement"); err != nil {
		return tx.fail(ctx, operation, err)
	}
	tx.c.forget()
	tx.tables = make(map[string]*txTable, len(sp.tables))
	tx.order = make([]*txTable, len(sp.tables))
	for i := range sp.tables {
		e := sp.tables[i]
		tx.tables[e.folded], tx.order[i] = &e, &e
	}
	tx.restoreIndexes(sp)
	return nil
}

// checkOpen is [Tx.check] for a call that needs only the connection, and not the instance to serve.
func (tx *Tx) checkOpen() error {
	if err := tx.state(); err != nil {
		return err
	}
	return tx.use()
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

// table with the given name and its indexes, as [conn.lookUp] gives it, or an error that wraps
// [ErrNoTable].
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

// tableWithoutIndexes is [Tx.table] without the indexes of the table, as
// [conn.lookUpWithoutIndexes] gives it, for a call that reads the definition of the table or one row
// by its key, and writes nothing.
func (tx *Tx) tableWithoutIndexes(ctx context.Context, operation, name string) (*table, error) {
	t, err := tx.c.lookUpWithoutIndexes(name)
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
// [ErrTableExists] if a table has the name, and [ErrIndexExists] if an index has it, compared as
// SQLite compares names, with [ErrReservedName] for a name that starts with "s4ql_" or "sqlite_",
// and with [ErrInvalid] for a table that the database cannot hold, also with [ErrTooLongForCache]
// for one whose statement in the cache is longer than the length limit.
func (tx *Tx) CreateTable(ctx context.Context, ct *s4qlpb.CreateTable) error {
	const operation = "create table"
	if err := tx.check(ctx, operation); err != nil {
		return err
	}
	defer tx.leave(ctx)
	ct = withoutNegativeZero(ct)
	if err := checkCreateTable(ct, tx.d.host.Wall()); err != nil {
		return err
	}
	if err := tx.unused(ctx, operation, ct.GetTable()); err != nil {
		return err
	}
	t := newTable(ct)
	if err := tx.c.create(t); err != nil {
		if errors.Is(err, ErrInvalid) {
			// The cache refused the table before it changed anything.
			return err
		}
		return tx.fail(ctx, operation, err)
	}
	// The commit object holds the table as the cache gives it back, without a field that is set to
	// its zero value, such as a nullable of false, and with copies of the defaults.
	tx.changed(ct.GetTable()).created = t.createTable()
	return nil
}

// DropTable with the given name, with all its rows and all its indexes. It fails with an error that
// wraps [ErrNoTable] if there is none.
func (tx *Tx) DropTable(ctx context.Context, name string) error {
	const operation = "drop table"
	if err := tx.check(ctx, operation); err != nil {
		return err
	}
	defer tx.leave(ctx)
	t, err := tx.table(ctx, operation, name)
	if err != nil {
		return err
	}
	if err := tx.forgetIndexes(t); err != nil {
		return tx.fail(ctx, operation, err)
	}
	e := tx.changed(t.name)
	if err := tx.c.drop(t); err != nil {
		return tx.fail(ctx, operation, err)
	}
	if err := tx.dropTouched(e); err != nil {
		return tx.fail(ctx, operation, err)
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

// recording reports whether the transaction records the keys that it touches in the table t, which
// it does for a table that existed when it began.
func (tx *Tx) recording(t *table) bool {
	e, ok := tx.tables[fold(t.name)]
	return !ok || e.created == nil
}

// touch the row of the table t with the given primary key, after the transaction upserted or
// deleted it, if the table existed when the transaction began: the key is recorded as touched,
// unless it is already, and, the first time, also as one of a row that existed, if the row existed
// before the change. The first record of a key stays, so it says whether the row existed when the
// transaction began.
//
// A key is recorded after the change, and only when SQLite took the change, so that a change that
// SQLite refuses leaves no record. A temporary table holds the key alone, and SQLite's record of a
// key is never larger than the record of a row with that key, so SQLite takes the key whenever it
// took the change, and does not refuse it as too big where a row of SQLite's own table would fit.
func (tx *Tx) touch(t *table, key []*s4qlpb.Value, existed bool) error {
	e := tx.changed(t.name)
	if e.created != nil {
		return nil
	}
	if e.touched == "" {
		tx.temps++
		names := []string{fmt.Sprint("s4ql_touched_", tx.temps), fmt.Sprint("s4ql_existed_", tx.temps)}
		for _, name := range names {
			if err := tx.createKeyTable(t, name); err != nil {
				return err
			}
		}
		e.touched, e.existed = names[0], names[1]
	}
	first, err := tx.c.count(fmt.Sprint("insert or ignore into temp.", quote(e.touched), " values (", placeholders(len(t.key)), ") returning 1"), values(key)...)
	if err != nil || first == 0 || !existed {
		return err
	}
	return tx.c.exec(fmt.Sprint("insert into temp.", quote(e.existed), " values (", placeholders(len(t.key)), ")"), values(key)...)
}

// createKeyTable makes the temporary table of the given name that holds primary keys of the table
// t, and nothing else: a column k0, k1, and so on, of the type of each column of the key, in key
// order, which are its primary key.
func (tx *Tx) createKeyTable(t *table, name string) error {
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
	b.WriteString("primary key (")
	b.WriteString(keyColumns("", len(t.key)))
	b.WriteString(")) strict, without rowid")
	return tx.c.c.Exec(b.String())
}

// dropTouched drops the temporary tables of the keys that the transaction touched in the table of
// e, if there are any.
func (tx *Tx) dropTouched(e *txTable) error {
	if e.touched == "" {
		return nil
	}
	for _, name := range []string{e.touched, e.existed} {
		if err := tx.c.c.Exec(fmt.Sprint("drop table temp.", quote(name))); err != nil {
			return err
		}
	}
	e.touched, e.existed = "", ""
	return nil
}

// tooBig is the error of a write that SQLite refused with err, which wraps [ErrTooBig] if SQLite
// refused it as too big, and nil otherwise. SQLite refuses such a statement before it changes
// anything, so the transaction can go on.
func tooBig(err error) error {
	if isTooBig(err) {
		return fmt.Errorf("%w: %v", ErrTooBig, err)
	}
	return nil
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
// primary key. The row has a value for each column of the table, in their order: a value of the
// column's type, or NULL for a column that takes NULL. A real that is -0.0 is stored as 0.0, as
// SQLite stores it. It fails with an error that wraps [ErrNoTable] if
// there is no such table, with [ErrInvalid] for a row that the table cannot hold, and with
// [ErrTooBig] for a text or a blob, or a row, that is larger than SQLite takes, after which the
// transaction goes on without the row.
func (tx *Tx) Upsert(ctx context.Context, name string, row []*s4qlpb.Value) error {
	const operation = "upsert"
	if err := tx.check(ctx, operation); err != nil {
		return err
	}
	defer tx.leave(ctx)
	t, err := tx.table(ctx, operation, name)
	if err != nil {
		return err
	}
	if err := t.checkRow(row, tx.c.limit); err != nil {
		return err
	}
	key := t.keyOf(row)
	var existed bool
	if tx.recording(t) {
		if existed, err = tx.c.exists(t, key); err != nil {
			return tx.fail(ctx, operation, err)
		}
	}
	if err := tx.c.upsert(t, row); err != nil {
		if refused := tooBig(err); refused != nil {
			return refused
		}
		return tx.fail(ctx, operation, err)
	}
	return tx.fail(ctx, operation, tx.touch(t, key, existed))
}

// Insert a row into the table with the given name if the table has no row with the same primary
// key, and report whether it did. The row is as for [Tx.Upsert]. It fails as Upsert fails, also
// with [ErrTooBig] for a row that is larger than SQLite takes when the table has a row with its key:
// SQLite builds the record of a row of a WITHOUT ROWID table before it looks for its key, so it
// refuses such a row as too big before it finds the key. After ErrTooBig the transaction goes on
// without the row.
func (tx *Tx) Insert(ctx context.Context, name string, row []*s4qlpb.Value) (bool, error) {
	const operation = "insert"
	if err := tx.check(ctx, operation); err != nil {
		return false, err
	}
	defer tx.leave(ctx)
	t, err := tx.table(ctx, operation, name)
	if err != nil {
		return false, err
	}
	if err := t.checkRow(row, tx.c.limit); err != nil {
		return false, err
	}
	inserted, err := tx.c.insert(t, row)
	if err != nil {
		if refused := tooBig(err); refused != nil {
			return false, refused
		}
		return false, tx.fail(ctx, operation, err)
	}
	if !inserted {
		return false, nil
	}
	// The table had no row with the key, so none existed before the change.
	return true, tx.fail(ctx, operation, tx.touch(t, t.keyOf(row), false))
}

// Delete the row with the given primary key from the table with the given name, and report
// whether there was one. The key has a value for each primary-key column, in key order. It fails
// with an error that wraps [ErrNoTable] if there is no such table, with [ErrInvalid] for a key that
// is not one of the table, and with [ErrTooBig] for a text or a blob that is longer than SQLite
// takes, after which the transaction goes on.
func (tx *Tx) Delete(ctx context.Context, name string, key []*s4qlpb.Value) (bool, error) {
	const operation = "delete"
	if err := tx.check(ctx, operation); err != nil {
		return false, err
	}
	defer tx.leave(ctx)
	t, err := tx.table(ctx, operation, name)
	if err != nil {
		return false, err
	}
	if err := t.checkKey(key, tx.c.limit); err != nil {
		return false, err
	}
	found, err := tx.c.delete(t, key)
	if err != nil {
		return false, tx.fail(ctx, operation, err)
	}
	// The delete of a row that is not there changes no row, so no key is recorded. It still counts
	// as a change of the table, for the order of the tables in the commit object.
	if !found {
		tx.changed(t.name)
		return false, nil
	}
	if err := tx.touch(t, key, true); err != nil {
		return false, tx.fail(ctx, operation, err)
	}
	return true, nil
}

// KeySet is a set of primary keys of one table, which a write transaction holds in a temporary table
// of the cache, so that it can hold any number of keys. It lasts for the call of [Tx.Keys] that
// gives it.
type KeySet struct {
	tx *Tx
	t  *table
	// add, first, and next are the statements of [KeySet.Add] and [KeySet.All], each on the
	// temporary table of the set.
	add, first, next string
	// gone is set when the call of [Tx.Keys] ends, which drops the set.
	gone bool
}

// Keys runs f with an empty set of primary keys of the table with the given name, and drops the set
// when f returns. It gives the error of f, and otherwise the error of the drop. It fails before f
// with an error that wraps [ErrNoTable] if there is no such table, and as the calls that change the
// transaction do. f must not end the transaction, nor roll it back to a savepoint.
//
// The set is for a statement that changes the rows that it finds, as SQLite's UPDATE and DELETE do:
// the statement adds the keys of the rows that it finds while it scans the table, and then reads
// them back from the set and changes the row of each. SQLite leaves undefined what a cursor gives
// when its table changes under it, and this way no cursor is open on a table that changes: while
// the scan runs, only the set changes, which the scan does not read, and [KeySet.All] reads each key
// with a statement that has ended before the row of the key changes. The set is in the cache, so
// neither holds rows in memory, and the rows that the statement writes never come back to it.
func (tx *Tx) Keys(ctx context.Context, name string, f func(*KeySet) error) error {
	const operation = "keys"
	if err := tx.check(ctx, operation); err != nil {
		return err
	}
	t, err := tx.table(ctx, operation, name)
	if err != nil {
		tx.leave(ctx)
		return err
	}
	tx.temps++
	temp := fmt.Sprint("s4ql_keys_", tx.temps)
	if err := tx.createKeyTable(t, temp); err != nil {
		tx.leave(ctx)
		return tx.fail(ctx, operation, err)
	}
	tx.leave(ctx)

	columns := keyColumns("", len(t.key))
	from := fmt.Sprint(" from temp.", quote(temp))
	k := &KeySet{
		tx:    tx,
		t:     t,
		add:   fmt.Sprint("insert or ignore into temp.", quote(temp), " values (", placeholders(len(t.key)), ")"),
		first: fmt.Sprint("select ", columns, from, " order by ", columns, " limit 1"),
		next:  fmt.Sprint("select ", columns, from, " where (", columns, ") > (", placeholders(len(t.key)), ") order by ", columns, " limit 1"),
	}
	err = f(k)
	k.gone = true
	return cmp.Or(err, tx.dropKeys(ctx, temp, k))
}

// dropKeys drops the temporary table temp of the set k, and the statements on it that are at rest,
// unless the transaction can only be rolled back, which drops it then.
func (tx *Tx) dropKeys(ctx context.Context, temp string, k *KeySet) error {
	const operation = "drop keys"
	if err := tx.checkOpen(); err != nil {
		if tx.state() != nil {
			return nil
		}
		return err
	}
	defer tx.leave(ctx)
	for _, sql := range []string{k.add, k.first, k.next} {
		if s, ok := tx.c.stmts[sql]; ok {
			delete(tx.c.stmts, sql)
			if err := s.Finalize(); err != nil {
				return tx.fail(ctx, operation, err)
			}
		}
	}
	return tx.fail(ctx, operation, tx.c.c.Exec(fmt.Sprint("drop table temp.", quote(temp))))
}

// Add the key to the set, unless the set has it. The key has a value for each primary-key column of
// the table, in key order. It fails as [Tx.Delete] does, and with an error that wraps [ErrInvalid]
// once the call of [Tx.Keys] has ended.
func (k *KeySet) Add(ctx context.Context, key []*s4qlpb.Value) error {
	const operation = "add key"
	if k.gone {
		return invalidf("a key added to a set that is gone")
	}
	tx := k.tx
	if err := tx.check(ctx, operation); err != nil {
		return err
	}
	defer tx.leave(ctx)
	if err := k.t.checkKey(key, tx.c.limit); err != nil {
		return err
	}
	if err := tx.c.exec(k.add, values(key)...); err != nil {
		if refused := tooBig(err); refused != nil {
			return refused
		}
		return tx.fail(ctx, operation, err)
	}
	return nil
}

// All keys of the set, in the order of the primary key, as SQLite compares keys. Each is read by a
// statement of its own, which has ended when All gives the key: the least key of the set that is
// greater than the one before it. So the transaction can change any table, the set among them,
// between two keys, and All gives a key that is added then if it is greater than the last. The
// sequence ends with an error if a read fails, and with one that wraps [ErrInvalid] once the call of
// [Tx.Keys] has ended.
func (k *KeySet) All(ctx context.Context) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		var last []*s4qlpb.Value
		for {
			key, err := k.after(ctx, last)
			if err != nil {
				yield(nil, err)
				return
			}
			if key == nil || !yield(key, nil) {
				return
			}
			last = key
		}
	}
}

// after gives the least key of the set that is greater than last, or the least key if last is nil,
// or nil if there is none.
func (k *KeySet) after(ctx context.Context, last []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	const operation = "read keys"
	if k.gone {
		return nil, invalidf("keys read from a set that is gone")
	}
	tx := k.tx
	if err := tx.check(ctx, operation); err != nil {
		return nil, err
	}
	defer tx.leave(ctx)
	sql, args := k.first, []any(nil)
	if last != nil {
		sql, args = k.next, values(last)
	}
	var key []*s4qlpb.Value
	err := tx.c.query(sql, func(s *sqlite.Stmt) error {
		var err error
		key, err = readRow(s, 0, len(k.t.key))
		return err
	}, args...)
	return key, tx.fail(ctx, operation, err)
}

// Annotate the transaction with a statement that it ran: its SQL text and its parameters. The
// annotations go into the commit object after its changes, in the order in which they were made.
// Replay never uses them. It fails with an error that wraps [ErrInvalid] for text that is not
// valid UTF-8, and for a parameter that is no value, a real that is NaN or infinite, or text that
// is not valid UTF-8 or holds the character NUL. A parameter that is -0.0 keeps its sign.
func (tx *Tx) Annotate(ctx context.Context, sql string, parameters []*s4qlpb.Value) error {
	const operation = "annotate"
	if err := tx.check(ctx, operation); err != nil {
		return err
	}
	defer tx.leave(ctx)
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
	defer tx.leave(ctx)
	t, err := tx.tableWithoutIndexes(ctx, operation, name)
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
	defer tx.leave(ctx)
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
	defer tx.leave(ctx)
	t, err := tx.tableWithoutIndexes(ctx, operation, name)
	if err != nil {
		return nil, err
	}
	if err := t.checkKey(key, tx.c.limit); err != nil {
		return nil, err
	}
	row, err := tx.c.get(t, key)
	if row != nil {
		tx.read++
	}
	return row, tx.fail(ctx, operation, err)
}

// Scan the rows of the table with the given name, as the transaction sees them, in the order of
// the primary key. The sequence ends with an error if the scan fails, which wraps [ErrNoTable] if
// there is no such table. The transaction must not change the table while it scans it, and must
// not drop any table: SQLite refuses the drop, and the transaction can then only be rolled back.
func (tx *Tx) Scan(ctx context.Context, name string) iter.Seq2[[]*s4qlpb.Value, error] {
	return tx.ScanRange(ctx, name, Bound{}, Bound{})
}

// ScanRange is [Tx.Scan] of the rows whose primary key is within the bounds lower and upper, which
// it checks as [ReadTx.ScanRange] does.
func (tx *Tx) ScanRange(ctx context.Context, name string, lower, upper Bound) iter.Seq2[[]*s4qlpb.Value, error] {
	return tx.scan(ctx, name, func(t *table) (*index, IndexScan, error) {
		return nil, IndexScan{Lower: lower, Upper: upper}, t.checkBounds(tx.c.limit, lower, upper)
	})
}

// Rollback the transaction: nothing of it is kept. It returns nil on a transaction that the
// instance ended because it lost its lease, and on one whose connection [DB.Close] closed. On a
// transaction that the instance ended while a scan of it runs, such as from the body of a loop over
// the scan, the writer lock is given back only when the scan ends, so a [DB.Begin] in that body waits
// for itself.
func (tx *Tx) Rollback(ctx context.Context) error {
	if tx.done {
		return ErrTxDone
	}
	defer tx.end(ctx)
	if tx.useToEnd() != nil {
		// The lease task ended the transaction, or [DB.Close] closed the connection, which rolled the
		// transaction of the cache back.
		return nil
	}
	defer tx.leave(ctx)
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
	tx.notes.close()
	d := tx.d
	d.mu.Lock()
	lost := tx.lease != nil
	if !lost {
		d.writing = nil
	}
	d.mu.Unlock()
	if lost {
		// The end by the lease task gives back the connection and the writer lock, now or when the
		// last call that runs leaves.
		return
	}
	if d.use(tx.c) == nil {
		tx.c.forget()
		d.unuse(tx.c)
	}
	d.writerLock.Release(ctx)
}

// spooled is the commit object of a transaction in its spool file.
type spooled struct {
	spool *spool
	name  format.Name
	// changes is the number of tables and indexes created and dropped and of rows upserted and
	// deleted.
	changes int
}

// spoolCommit writes the commit object of the transaction to a spool file: the net effect of what
// the transaction did, and then its annotations.
//
// The net effect is, first, the drops of the indexes that were there when the transaction began
// and that it dropped, in the order in which it dropped them. An index that went with its table,
// which the transaction dropped, is not among them: the drop of the table drops it.
//
// Then, for each table name that the transaction changed, in the order in which it first changed
// them, the net effect is: the drop of the table that was there when the transaction began, if the
// transaction dropped it; then the table that is there now and all its rows, if the transaction
// created it; and otherwise the rows that the transaction changed and that are there now, and the
// keys of the rows that it changed, that were there when it began, and that are gone. So a primary
// key appears at most once for each table, a table that is created and dropped leaves nothing, and
// a table that is dropped and created again is a drop, a create, and its rows.
//
// Last come the indexes that the transaction created and that are there now, in the order in which
// it created them, which is the order that they have among the indexes of their table on every
// instance. An index that is created and dropped leaves nothing, and one that is dropped and
// created again is a drop and a create, and is then the last of its table's.
//
// Applied in this order to the state before the transaction, every change finds what it needs: a
// dropped index is there, because nothing before it has dropped its table; a name is free for a
// table, because the index or the table that had it was dropped before; and the table and the
// columns of a created index are there, with all their rows, so that the index is built once.
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

	for _, name := range tx.droppedIndexes {
		if err := w.DropIndex(name); err != nil {
			return nil, fail(err)
		}
		out.changes++
	}

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
				if err := w.UpsertRow(t.name, columns, tx.committed(t, row)); err != nil {
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

	created, err := tx.spoolCreatedIndexes(w)
	if err != nil {
		return nil, fail(err)
	}
	out.changes += created

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
	// The cross join makes the temporary table the outer loop, read in the order of its primary key,
	// so that SQLite sorts nothing: a sorter would hold the key and the row in one record, which
	// SQLite refuses as too big for a row near the length limit.
	upserts := fmt.Sprint("select t.* from temp.", quote(e.touched), " as k cross join main.", quote(t.name), " as t where ", match, " order by ", order)
	if err := tx.c.query(upserts, func(s *sqlite.Stmt) error {
		row, err := readRow(s, 0, len(t.columns))
		if err != nil {
			return err
		}
		row = tx.committed(t, row)
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
	deletes := fmt.Sprint("select ", order, " from temp.", quote(e.existed), " as k where not exists (select 1 from main.",
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
		if err := tx.dropTouched(e); err != nil {
			return err
		}
	}
	return nil
}

// committed is the row of the table t as the commit object holds it: as the cache holds it.
func (tx *Tx) committed(t *table, row []*s4qlpb.Value) []*s4qlpb.Value {
	bugs := tx.d.bugs
	if !bugs.blobCut && !bugs.nullZero {
		return row
	}
	changed := make([]*s4qlpb.Value, len(row))
	for i, v := range row {
		changed[i] = v
		switch {
		case bugs.blobCut && v.WhichKind() == s4qlpb.Value_Blob_case:
			// The planted bug: a blob is cut at its first zero byte, as by code that takes it for a
			// string of C.
			if j := bytes.IndexByte(v.GetBlob(), 0); j >= 0 {
				changed[i] = s4qlpb.Value_builder{Blob: v.GetBlob()[:j:j]}.Build()
			}
		case bugs.nullZero && v.WhichKind() == s4qlpb.Value_Null_case:
			// The planted bug: a NULL is written as the zero value of the column's type, as by code
			// that reads a column into a value of Go without asking whether it is NULL.
			changed[i] = zeroValue(t.columns[i].typ)
		}
	}
	return changed
}

// zeroValue of the given type of a column: 0, 0.0, the empty text, or the empty blob.
func zeroValue(typ s4qlpb.Type) *s4qlpb.Value {
	switch typ {
	case s4qlpb.Type_TYPE_REAL:
		return s4qlpb.Value_builder{Real: new(0.0)}.Build()
	case s4qlpb.Type_TYPE_TEXT:
		return s4qlpb.Value_builder{Text: new("")}.Build()
	case s4qlpb.Type_TYPE_BLOB:
		return s4qlpb.Value_builder{Blob: []byte{}}.Build()
	default:
		return s4qlpb.Value_builder{Integer: new(int64(0))}.Build()
	}
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
	// a stop, because it then commits or rolls back the transaction of the cache. From here on, the
	// lease task does not end the transaction for the lease: the manifest swap decides.
	if err := tx.useToEnd(); err != nil {
		return err
	}
	defer tx.leave(ctx)
	if tx.failed != nil {
		_ = tx.rollback(ctx)
		return tx.failed
	}
	if tx.saved != nil {
		_ = tx.rollback(ctx)
		return fmt.Errorf("commit: not committed: %w", invalidf("a commit while a savepoint is open"))
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
