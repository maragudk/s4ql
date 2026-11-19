package s4ql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/engine"
)

// tx is a transaction that spans statements, open on the connection of the engine of its
// connection from [conn.BeginTx] to [tx.Commit] or [tx.Rollback]. Every statement of the connection
// runs in it meanwhile, as database/sql runs every statement of a [sql.Tx] on the one connection.
type tx struct {
	conn *conn
	// ctx of the begin, which bounds the commit: database/sql gives the commit of a driver no
	// context of its own, and the context of [sql.DB.BeginTx] is used until the transaction ends.
	ctx context.Context
}

var _ driver.Tx = (*tx)(nil)

// errTxEnded is the error of a commit or a roll back of a transaction that is not the one open on
// its connection, which database/sql does not make.
var errTxEnded = fmt.Errorf("%w: the transaction has ended", engine.ErrInternal)

// BeginTx begins a transaction on the connection, with the options opts.
//
// A transaction that is not read-only is a write transaction: it takes the place of the single
// writer of the database, and holds it until it ends, so it waits while another write transaction is
// open, as long as ctx allows, and every other write of the instance waits for it meanwhile. A
// read-only transaction reads the state of its beginning, takes no place of a writer, and waits for
// nothing. Both accept the isolation levels [sql.LevelDefault] and [sql.LevelSerializable], which is
// what they give: writers run one after another, and a read-only transaction reads one state that is
// committed. Another level is not supported, and then nothing is begun, also on an instance that does
// not serve.
//
// It fails as a statement would on an instance that does not serve, with an error that matches
// [ErrNotServing] or [ErrStopped]; with "not supported" while the rows of an earlier query of the
// connection are open; and with an error that says so while a transaction is open on the connection,
// which a [sql.Conn] can make. When it fails, nothing has begun.
func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	switch level := sql.IsolationLevel(opts.Isolation); level {
	case sql.LevelDefault, sql.LevelSerializable:
	default:
		return nil, notSupported(fmt.Sprint("the isolation level ", level))
	}
	begin := c.e.Begin
	if opts.ReadOnly {
		begin = c.e.BeginReadOnly
	}
	c.enter(ctx)
	defer c.leave(ctx)
	if err := begin(ctx); err != nil {
		if errors.Is(err, engine.ErrRowsOpen) {
			return nil, &failure{is: ErrNotSupported, message: "s4ql: not supported: a transaction that begins on a connection whose rows of an earlier query are open; close the rows first", cause: err}
		}
		return nil, c.c.public(ctx, err)
	}
	c.tx = &tx{conn: c, ctx: ctx}
	return c.tx, nil
}

// Begin is [conn.BeginTx] of a write transaction, without a context, which it then waits for
// without an end.
func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// Commit the transaction, within the context of its begin. The transaction is over when Commit
// returns, also with an error.
//
// A write transaction commits all its changes as one commit object, and writes none if it changed
// nothing. Commit fails with an error that matches [ErrTooLarge] for a commit object that is too
// large for the store, with one that matches [ErrOutcomeUnknown] if the context ended before the
// instance knew whether the transaction is committed, and otherwise with an error that means that
// nothing is committed, such as one that matches [ErrNotServing] or [ErrStopped]. A read-only
// transaction just ends, and its commit returns nil, also on an instance that has stopped: it has
// nothing to commit, and each of its statements was refused or answered as the instance was then.
func (t *tx) Commit() error {
	c := t.conn
	if c.tx != t {
		return publicError(errTxEnded)
	}
	c.tx = nil
	c.enter(t.ctx)
	defer c.leave(t.ctx)
	err := c.e.Commit(t.ctx)
	if errors.Is(err, engine.ErrRowsOpen) {
		// database/sql closes the rows of a transaction before its commit, so only a call of the
		// driver's own interfaces meets this.
		return &failure{is: ErrNotSupported, message: "s4ql: not supported: a commit while the rows of a query of the transaction are read, so the transaction is rolled back and not committed", cause: err}
	}
	return c.c.public(t.ctx, err)
}

// Rollback the transaction: nothing that it did is kept, and the place of the writer is free. It
// returns nil also on an instance that has lost its lease or has stopped, where the transaction ends
// all the same and nothing is committed, so that a deferred Rollback after a Commit that failed is
// quiet. It gives another error of the roll back, which is a fault, after it has ended the
// transaction all the same.
func (t *tx) Rollback() error {
	c := t.conn
	if c.tx != t {
		return publicError(errTxEnded)
	}
	c.tx = nil
	// database/sql rolls the transaction back when the context of its begin ends.
	ctx := context.WithoutCancel(t.ctx)
	c.enter(ctx)
	defer c.leave(ctx)
	err := c.e.Rollback(ctx)
	if errors.Is(err, core.ErrStopped) || errors.Is(err, core.ErrNoLease) {
		return nil
	}
	return c.c.public(ctx, err)
}

// ResetSession has nothing to reset: [conn.IsValid] keeps a connection with a transaction out of
// the pool. With it and IsValid, database/sql keeps the connection of a transaction that it rolls
// back when the context of the begin ends.
func (c *conn) ResetSession(context.Context) error {
	return nil
}

// IsValid reports whether the connection can go back to the pool: not while a transaction is open
// on it, which database/sql then closes with the connection, and the close rolls the transaction
// back.
func (c *conn) IsValid() bool {
	return c.tx == nil
}
