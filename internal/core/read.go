package core

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sqlite"
)

// Reader is a connection to the cache that runs read transactions, one at a time. [DB.Reader]
// opens it, and its owner closes it with [Reader.Close]. The read transactions of different readers
// run in parallel, so the readers that are open are the read transactions that can run at once, and
// nothing but their owners limits their number.
//
// A reader is not safe for concurrent use. A stop of the instance ends the read transaction of
// every reader, and [DB.Close] closes the connection of every reader, also one whose read
// transaction is open; [Reader.Close] of such a reader then has nothing left to close.
type Reader struct {
	d *DB
	c *conn
	// tx is the read transaction that is open, and closed whether [Reader.Close] has closed the
	// reader. [DB.mu] guards both.
	tx     *ReadTx
	closed bool
}

// Reader opens a connection to the cache for read transactions. It does not need the instance to
// serve, which [Reader.Read] does. It fails with the error of the stop on an instance that has
// stopped.
//
// A cache that cannot be opened is an error of the cache as any other while the instance runs: a
// local I/O failure, which includes a file that cannot be opened for want of a file descriptor and
// a file that is gone, stops the instance, and the error wraps [ErrStopped] and [ErrLocal].
func (d *DB) Reader(ctx context.Context) (*Reader, error) {
	// The connection opens without the mutex, which nothing holds across a call that can take
	// long. The count of the readers that are opening is what [DB.Close] waits for meanwhile.
	d.mu.Lock()
	if err := d.stopped; err != nil {
		d.mu.Unlock()
		return nil, err
	}
	d.opening++
	d.mu.Unlock()
	d.host.Yield(ctx, "core: a reader before its open")

	// Nothing waits between the open and the list, so that no task is ended between them with a
	// connection that nothing knows of. A reader opens the cache that the write connection made and
	// never makes one: a cache whose file is gone, such as one deleted under the instance, is a file
	// that cannot be opened, and not a new empty cache without the tables.
	c, err := openConn(sqlite.OpenExistingVFS, d.path, d.options)
	if err == nil {
		if err = c.exec("pragma query_only = on"); err != nil {
			_ = c.close()
		}
	}

	d.mu.Lock()
	stopped := d.stopped
	switch {
	case err != nil:
	case stopped != nil:
		// The instance stopped meanwhile, and its close does not know the connection: it is closed
		// here, before the count lets that close return.
		_ = c.close()
	default:
		d.readers = append(d.readers, c)
	}
	d.opening--
	if d.closing && d.opening == 0 {
		d.wakeLocked()
	}
	d.mu.Unlock()
	switch {
	case err != nil:
		return nil, d.cacheError(ctx, "open a reader", err)
	case stopped != nil:
		return nil, stopped
	}
	return &Reader{d: d, c: c}, nil
}

// ReadTx is a read transaction. It sees one consistent state of the cache: the state after one
// commit object, whatever is committed while it is open. Read transactions run in parallel, one
// on each [Reader]. It ends with [ReadTx.Close], and must end with it.
//
// A read transaction is not safe for concurrent use. If the instance stops for good while a read
// transaction is open, every call of the transaction fails.
type ReadTx struct {
	d        *DB
	rd       *Reader
	c        *conn
	position format.Name
	done     bool
}

// Read starts a read transaction on the reader. It does not wait: it fails at once with an error
// that wraps [ErrNoLease] or [ErrStopped] on an instance that does not serve, with the error of ctx
// if ctx is done, with [ErrReaderBusy] while the read transaction that the reader started before is
// open, with [ErrReaderClosed] on a reader that is closed, and with an error that wraps [ErrBusy]
// if the cache is locked.
//
// Before the state that the transaction sees is fixed, the lease is checked against both clocks of
// the host: less than the duration of the lease has passed since the instance sent its last
// renewal, by its wall clock and by its monotonic clock, and both must show it. At the moment of
// that check no other instance holds the lease, so the cache holds the newest state of the
// database, and the state that is fixed later is that state or a newer one. A check after the
// state is fixed would not do alone: an instance that fixed the state while it did not hold the
// lease can take the lease and catch up before the check. The lease is checked once more then, so
// that a read on an instance that has stopped serving meanwhile is refused and not served late.
func (rd *Reader) Read(ctx context.Context) (_ *ReadTx, err error) {
	const operation = "read"
	d, c := rd.d, rd.c
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.host.Yield(ctx, "core: a read before it starts")
	r := &ReadTx{d: d, rd: rd, c: c}
	d.mu.Lock()
	err = d.servingLocked()
	switch {
	case err != nil:
	case rd.tx != nil:
		err = ErrReaderBusy
	default:
		rd.tx = r
	}
	d.mu.Unlock()
	if err != nil {
		return nil, d.refused(ctx, operation, err)
	}
	defer func() {
		if err != nil {
			_ = r.end(ctx)
		}
	}()
	// A connection that is closed refuses the read: with the stop, or with [ErrReaderClosed].
	if err := d.use(c); err != nil {
		return nil, err
	}
	defer d.unuse(c)

	if d.bugs.unlockedCounter {
		// The planted data race.
		d.racy++
	}

	if err := c.c.Exec("begin"); err != nil {
		return nil, d.cacheError(ctx, operation, err)
	}
	d.host.Yield(ctx, "core: a read before the state of the cache is fixed")
	// SQLite fixes the state that a transaction sees at its first read, not when it begins.
	if r.position, err = c.position(); err != nil {
		return nil, d.cacheError(ctx, operation, err)
	}
	d.host.Yield(ctx, "core: a read before the check of the lease")
	d.mu.Lock()
	err = d.servingLocked()
	d.mu.Unlock()
	if err != nil {
		return nil, d.refused(ctx, operation, err)
	}
	return r, nil
}

// Close the reader: end its read transaction, if one is open, and close its connection to the
// cache. It gives the error of the end of that transaction, and of the close of the connection.
// After [DB.Close], which has closed the connection, it has nothing left to close. It fails with
// [ErrReaderInUse], and closes nothing, while a call of the read transaction of the reader runs,
// such as a loop over [ReadTx.Scan]. Closing a closed reader does nothing.
func (rd *Reader) Close(ctx context.Context) error {
	d, c := rd.d, rd.c
	d.mu.Lock()
	switch {
	case rd.closed:
		d.mu.Unlock()
		return nil
	case c.users > 0:
		d.mu.Unlock()
		return ErrReaderInUse
	}
	rd.closed = true
	tx := rd.tx
	d.mu.Unlock()

	var endErr error
	if tx != nil {
		endErr = tx.end(ctx)
	}

	// The connection closes under the mutex, as in [DB.closeIdleLocked], so that [DB.Close] does not
	// return while a read connection that it takes for closed is still closing, and so that no two
	// connections that have read the cache close at once: the close of the last one deletes the
	// write-ahead log only if it can lock the cache alone, and two closes at once can keep each
	// other from it. A connection that has not read the cache holds no lock on it, as one that
	// [DB.Reader] closes after a failed open.
	d.mu.Lock()
	d.readers = slices.DeleteFunc(d.readers, func(other *conn) bool { return other == c })
	var closeErr error
	if !c.closed {
		c.closed = true
		closeErr = c.close()
	}
	d.mu.Unlock()
	return errors.Join(endErr, d.cacheError(ctx, "close a reader", closeErr))
}

// Position of the cache in the state that the transaction sees: the name of the last commit object
// that the state holds, or [format.EmptyLog].
func (r *ReadTx) Position() format.Name {
	return r.position
}

// Serving returns nil if the instance serves now, as [Reader.Read] requires it before it starts a
// read transaction: it holds the lease, its cache is caught up, and the lease is valid by both
// clocks. Otherwise it returns the error of the stop, or one that wraps [ErrNoLease]. It is the
// check before each statement of a read transaction that spans statements, so that such a
// statement is refused as a read of its own would be, and still sees the state that the transaction
// fixed when it started. The other calls of a read transaction do not look at the clocks. It fails
// with [ErrTxDone] on a transaction that has ended.
func (r *ReadTx) Serving(ctx context.Context) error {
	if r.done {
		return ErrTxDone
	}
	r.d.mu.Lock()
	err := r.d.servingLocked()
	r.d.mu.Unlock()
	if err != nil {
		return r.d.refused(ctx, "read", err)
	}
	return nil
}

// check that the transaction can go on, and use its connection for the call, which gives it back
// with [ReadTx.leave].
func (r *ReadTx) check() error {
	if r.done {
		return ErrTxDone
	}
	if err := r.d.stoppedError(); err != nil {
		return err
	}
	return r.d.use(r.c)
}

// leave a call of the transaction, which [ReadTx.check] began.
func (r *ReadTx) leave() {
	r.d.unuse(r.c)
}

// table with the given name, or an error that wraps [ErrNoTable].
func (r *ReadTx) table(ctx context.Context, operation, name string) (*table, error) {
	t, err := r.c.lookUp(name)
	if err != nil {
		return nil, r.d.cacheError(ctx, operation, err)
	}
	if t == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoTable, name)
	}
	return t, nil
}

// Table with the given name, or an error that wraps [ErrNoTable].
func (r *ReadTx) Table(ctx context.Context, name string) (*s4qlpb.CreateTable, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	defer r.leave()
	t, err := r.table(ctx, "table", name)
	if err != nil {
		return nil, err
	}
	return t.createTable(), nil
}

// Tables of the database, in the order of their names as bytes.
func (r *ReadTx) Tables(ctx context.Context) ([]*s4qlpb.CreateTable, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	defer r.leave()
	tables, err := r.c.list()
	if err != nil {
		return nil, r.d.cacheError(ctx, "tables", err)
	}
	return createTables(tables), nil
}

// Get the row with the given primary key from the table with the given name, or nil if there is
// none. The key has a value for each primary-key column, in key order. It fails with an error that
// wraps [ErrNoTable] if there is no such table, with [ErrInvalid] for a key that is not one of the
// table, and with [ErrTooBig] for a text or a blob that is longer than SQLite takes.
func (r *ReadTx) Get(ctx context.Context, name string, key []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	const operation = "get"
	if err := r.check(); err != nil {
		return nil, err
	}
	defer r.leave()
	t, err := r.table(ctx, operation, name)
	if err != nil {
		return nil, err
	}
	if err := t.checkKey(key, r.c.limit); err != nil {
		return nil, err
	}
	row, err := r.c.get(t, key)
	return row, r.d.cacheError(ctx, operation, err)
}

// Scan the rows of the table with the given name, in the order of the primary key. The sequence
// ends with an error if the scan fails, which wraps [ErrNoTable] if there is no such table.
func (r *ReadTx) Scan(ctx context.Context, name string) iter.Seq2[[]*s4qlpb.Value, error] {
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
		for row, err := range r.c.scan(t) {
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

// Close the transaction, so that its reader can start the next.
func (r *ReadTx) Close(ctx context.Context) error {
	if r.done {
		return ErrTxDone
	}
	return r.end(ctx)
}

// end the transaction, and free its reader for the next.
func (r *ReadTx) end(ctx context.Context) error {
	if r.done {
		return nil
	}
	r.done = true
	var err error
	// [DB.Close] may have closed the connection, which ended the transaction of the cache.
	if r.d.use(r.c) == nil {
		r.c.forget()
		// A transaction that only read loses nothing when it is rolled back. If it never began,
		// there is nothing to roll back, and SQLite says so with an error that is no failure of the
		// cache.
		err = r.c.c.Exec("rollback")
		if classify(err) == kindOther {
			err = nil
		}
		err = r.d.cacheError(ctx, "read", err)
		r.leave()
	}
	r.d.mu.Lock()
	r.rd.tx = nil
	r.d.mu.Unlock()
	return err
}
