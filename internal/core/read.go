package core

import (
	"context"
	"fmt"
	"iter"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
)

// ReadTx is a read transaction. It sees one consistent state of the cache: the state after one
// commit object, whatever is committed while it is open. Read transactions run in parallel, as
// many at once as the instance has read connections. It ends with [ReadTx.Close], and must end
// with it.
//
// A read transaction is not safe for concurrent use. If the instance stops for good while a read
// transaction is open, every call of the transaction fails.
type ReadTx struct {
	d        *DB
	c        *conn
	position format.Name
	done     bool
}

// Read starts a read transaction, waiting until a read connection is free. It fails at once, and
// whenever the instance stops serving while it waits, with an error that wraps [ErrNoLease] or
// [ErrStopped], with the error of the wait if ctx is done first, and with one that wraps [ErrBusy]
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
func (d *DB) Read(ctx context.Context) (_ *ReadTx, err error) {
	const operation = "read"
	if err := d.acquireServing(ctx, d.readerPool); err != nil {
		return nil, d.refused(ctx, operation, err)
	}
	d.mu.Lock()
	c := d.idle[len(d.idle)-1]
	d.idle = d.idle[:len(d.idle)-1]
	d.mu.Unlock()
	r := &ReadTx{d: d, c: c}
	defer func() {
		if err != nil {
			_ = r.end(ctx)
		}
	}()
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

// Position of the cache in the state that the transaction sees: the name of the last commit object
// that the state holds, or [format.EmptyLog].
func (r *ReadTx) Position() format.Name {
	return r.position
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
// wraps [ErrNoTable] if there is no such table, and with [ErrInvalid] for a key that is not one of
// the table.
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
	if err := t.checkKey(key); err != nil {
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

// Close the transaction, and give its connection back.
func (r *ReadTx) Close(ctx context.Context) error {
	if r.done {
		return ErrTxDone
	}
	return r.end(ctx)
}

// end the transaction: give back its connection and its permit.
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
	r.d.idle = append(r.d.idle, r.c)
	r.d.mu.Unlock()
	r.d.readerPool.Release(ctx)
	return err
}
