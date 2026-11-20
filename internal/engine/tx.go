package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
)

// transaction that spans statements, which is open on a [Conn] from [Conn.Begin] or
// [Conn.BeginReadOnly] to [Conn.Commit] or [Conn.Rollback].
type transaction struct {
	e *Engine
	// write is the write transaction of the core, or nil for a read-only transaction, whose read
	// transaction read is then.
	write writeTx
	read  readTx
	start time.Duration
	// statements counts the statements that the connection was given while the transaction was open,
	// and writes those whose changes it kept.
	statements, writes int
}

// kind of the transaction, as its event names it.
func (t *transaction) kind() string {
	if t.write != nil {
		return "write"
	}
	return "read_only"
}

// position of the state that the transaction began from.
func (t *transaction) position() format.Name {
	if t.write != nil {
		return t.write.position()
	}
	return t.read.position()
}

// reader of the transaction, which reads its state: a write transaction reads its own writes.
func (t *transaction) reader() reader {
	if t.write != nil {
		return t.write
	}
	return t.read
}

// serving checks that the instance serves now, before a statement of the transaction.
func (t *transaction) serving(ctx context.Context) error {
	if t.write != nil {
		return t.write.serving(ctx)
	}
	return t.read.serving(ctx)
}

// end the transaction without a commit: roll a write transaction back, and end a read
// transaction.
func (t *transaction) end(ctx context.Context) error {
	if t.write != nil {
		return t.write.rollback(ctx)
	}
	return t.read.end(ctx)
}

// ended is the error of the event of a transaction that ended without a commit with err: err, or
// for a write transaction that the instance ended because it lost its lease, whose roll back
// returns nil, the error of that end, so that the event says how the transaction ended.
func (t *transaction) ended(err error) error {
	if err == nil && t.write != nil {
		return t.write.lost()
	}
	return err
}

// log the event of the transaction, which ended by how with err.
func (t *transaction) log(ctx context.Context, how string, err error) {
	outcome, level := outcomeOf(err)
	if !t.e.log.Enabled(ctx, level) {
		return
	}
	position := t.position().String()
	attrs := []slog.Attr{slog.String(attrTxKind, t.kind()), slog.String(attrTxEnd, how),
		slog.Int(attrTxStatements, t.statements), slog.Int(attrTxWrites, t.writes), slog.String(attrTxPosition, position)}
	if t.write != nil {
		attrs = append(attrs, slog.String(attrParent, position))
	}
	attrs = append(attrs, slog.String(attrOutcome, outcome))
	if err != nil {
		attrs = append(attrs, slog.String(attrError, err.Error()))
	}
	attrs = append(attrs, slog.Duration(attrDuration, t.e.host.Monotonic()-t.start))
	t.e.log.LogAttrs(ctx, level, eventTransaction, attrs...)
}

// Begin a write transaction on the connection, which spans the statements of the connection until
// [Conn.Commit] or [Conn.Rollback]. It takes the place of the single writer of the database, as
// SQLite's `begin immediate` does, and holds it until it ends: it waits while another write
// transaction is open, as long as ctx allows, and gives the error of ctx if ctx ends first. It also
// fails as a statement that writes would: with an error that matches [core.ErrNoLease] or
// [core.ErrStopped] on an instance that does not serve. It fails with [ErrTxOpen] while a
// transaction is open on the connection, and with [ErrRowsOpen] while the rows of a query of the
// connection have not ended. When it fails, nothing has begun.
//
// The transaction sees the newest state that is committed, and its own writes, which nothing
// outside sees before its commit. See [Conn.Exec] for how a statement runs in it.
func (c *Conn) Begin(ctx context.Context) error {
	if err := c.canBegin(); err != nil {
		return err
	}
	start := c.e.host.Monotonic()
	w, err := c.begin(ctx)
	if err != nil {
		return err
	}
	c.tx = &transaction{e: c.e, write: w, start: start}
	return nil
}

// BeginReadOnly begins a read-only transaction on the connection, which spans the statements of the
// connection until [Conn.Commit] or [Conn.Rollback]. It is a read transaction of the reader of the
// connection: every statement in it sees the state of its beginning, whatever commits meanwhile, and
// it takes no place of a writer, so it waits for nothing and blocks no one. It fails as a query
// would: at once, with an error that matches [core.ErrNoLease] or [core.ErrStopped] on an instance
// that does not serve. It fails as [Conn.Begin] does on a connection with a transaction or rows
// open. When it fails, nothing has begun.
func (c *Conn) BeginReadOnly(ctx context.Context) error {
	if err := c.canBegin(); err != nil {
		return err
	}
	start := c.e.host.Monotonic()
	rt, err := c.read(ctx)
	if err != nil {
		return err
	}
	c.tx = &transaction{e: c.e, read: rt, start: start}
	return nil
}

// canBegin a transaction on the connection, or the reason why not.
func (c *Conn) canBegin() error {
	switch {
	case c.closed:
		return internalError("a transaction on a connection that is closed")
	case c.tx != nil:
		return ErrTxOpen
	case c.rowsOpen():
		return ErrRowsOpen
	}
	return nil
}

// rowsOpen reports whether the rows of the last query of the connection have not ended.
func (c *Conn) rowsOpen() bool {
	return c.rows != nil && !c.rows.done
}

// ready checks that a statement can run in the transaction of the connection, after its text and
// its arguments are checked: ctx has not ended, the instance serves now, as for a statement of its
// own, and no rows of an earlier query of the connection are open.
func (c *Conn) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.tx.serving(ctx); err != nil {
		return err
	}
	if c.rowsOpen() {
		return ErrRowsOpen
	}
	if c.e.bugs.readNewest && c.tx.write == nil {
		// The planted bug: the read-only transaction moves on to the newest state.
		if err := c.tx.read.end(ctx); err != nil {
			return err
		}
		rt, err := c.read(ctx)
		if err != nil {
			return err
		}
		c.tx.read = rt
	}
	return nil
}

// Commit the transaction of the connection. The transaction is over when Commit returns, also with
// an error.
//
// A write transaction commits as a statement of its own does: all its changes are one commit
// object, and every read that starts after Commit has returned nil sees them. A transaction that
// changed nothing writes no commit object. Commit fails with an error that matches [ErrTooLarge] for
// a commit object that is too large for the store, with one that matches [core.ErrOutcomeUnknown] if
// ctx ended before the instance knew whether the transaction is committed, and otherwise with the
// errors of a commit of the core, each of which means that nothing is committed. A read-only
// transaction just ends, and writes nothing.
//
// Rows of a query of the transaction that no loop reads are closed first. Rows that a loop reads end
// at the next row of the loop with [ErrTxDone], and the transaction ends then; a write transaction
// is not committed in that case, but rolled back, and Commit fails with an error that matches
// [ErrRowsOpen]. Commit fails with [ErrNoTx] on a connection without a transaction.
func (c *Conn) Commit(ctx context.Context) error {
	t := c.tx
	if t == nil {
		return ErrNoTx
	}
	c.tx = nil
	var err error
	switch {
	case t.write != nil && c.rowsRead():
		err = fmt.Errorf("%w, so the transaction is rolled back and not committed", ErrRowsOpen)
		c.leaveEnd(t, "commit", err)
		return err
	case c.leaveEnd(t, "commit", nil):
		return nil
	case t.write != nil:
		err = commitError(t.write.commit(ctx))
	default:
		err = t.read.end(ctx)
	}
	t.log(ctx, "commit", err)
	return err
}

// Rollback the transaction of the connection: nothing that it did is kept. It ends the transaction
// and frees the place of the writer whatever it returns. It returns nil on an instance that has lost
// its lease or has been closed; on an instance that a local failure has stopped, it can give the
// error of the roll back in the cache. It treats rows of the transaction as [Conn.Commit]
// does, and a transaction whose rows a loop reads ends at the next row of the loop. It fails with
// [ErrNoTx] on a connection without a transaction.
func (c *Conn) Rollback(ctx context.Context) error {
	t := c.tx
	if t == nil {
		return ErrNoTx
	}
	c.tx = nil
	if c.leaveEnd(t, "rollback", nil) {
		return nil
	}
	if c.e.bugs.rollbackKeeps && t.write != nil && t.writes == 1 {
		// The planted bug: the roll back commits, whatever the commit gives.
		t.log(ctx, "rollback", commitError(t.write.commit(ctx)))
		return nil
	}
	err := t.end(ctx)
	t.log(ctx, "rollback", t.ended(err))
	return err
}

// rowsRead reports whether a loop over the rows of the last query of the connection runs.
func (c *Conn) rowsRead() bool {
	return c.rowsOpen() && c.rows.reading
}

// leaveEnd ends the rows of the transaction t, which ends by how, and reports whether it left the
// end of t to them. Rows that no loop reads end now. Rows that a loop reads hold the scan of a
// statement of t until the loop ends, so the end of t is theirs: they end at the next row of the
// loop with [ErrTxDone], and end t after the scan has ended, and log its event then, with err, or
// with the error of that end.
func (c *Conn) leaveEnd(t *transaction, how string, err error) bool {
	r := c.rows
	if r == nil || r.done {
		return false
	}
	if !r.reading {
		_ = r.end(nil)
		return false
	}
	r.closing, r.cut, r.tx = true, ErrTxDone, ending{t: t, how: how, err: err}
	return true
}

// ending is the end of the transaction t, which ended by how with err while a loop over its rows
// ran, and which those rows run when the loop ends.
type ending struct {
	t   *transaction
	how string
	err error
}

func (e ending) end(ctx context.Context) error {
	err := e.t.end(ctx)
	e.t.log(ctx, e.how, first(e.err, e.t.ended(err)))
	return err
}

// commitError is the error of a commit, with [ErrTooLarge] for a commit object that the store
// cannot take.
func commitError(err error) error {
	if errors.Is(err, core.ErrTooLarge) {
		return fmt.Errorf("%w: %w", ErrTooLarge, err)
	}
	return err
}

// writeIn runs the plan p of a statement that changes the database, whose text is sql, with the
// arguments args, in the transaction of the connection. In a write transaction, the statement runs
// between a savepoint and its release, and is annotated before the release; if it fails, the
// transaction rolls back to the savepoint, so that the statement has no effect and what the
// statements before it did is kept, as in SQLite. In a read-only transaction, it fails with the
// error that SQLite gives under `pragma query_only = 1`, after SQLite's errors of its names.
func (c *Conn) writeIn(ctx context.Context, r *report, p Plan, sql string, args []*s4qlpb.Value) (Result, error) {
	t := c.tx
	r.at(t.position(), t.write != nil)
	if err := c.ready(ctx); err != nil {
		return Result{}, err
	}
	_, insert := p.(*Insert)
	if t.write == nil {
		// SQLite looks the names up when it prepares the statement, and refuses the write when it
		// steps it.
		if _, err := buildSink(ctx, readOnly{t.read}, p, args, bugs{}, c.e.db.lengthLimit()); err != nil {
			return Result{}, err
		}
		return Result{SetsChanges: insert}, readOnlyDatabase()
	}

	w := t.write
	if err := w.savepoint(ctx); err != nil {
		return Result{}, err
	}
	kept := false
	defer func() {
		// Also when an operator panics. A rollback to the savepoint that fails leaves a transaction
		// that the core lets only roll back.
		switch {
		case kept:
		case c.e.bugs.keepFailed:
			// The planted bug: the statement is kept as if it had succeeded.
			_ = w.annotate(ctx, sql, args)
			_ = w.release(ctx)
		default:
			_ = w.rollbackTo(ctx)
		}
	}()
	s, err := buildSink(ctx, w, p, args, bugs{skipUnique: c.e.bugs.skipUnique, convertReal: c.e.bugs.convertReal}, c.e.db.lengthLimit())
	if err != nil {
		return Result{}, err
	}
	failed := Result{SetsChanges: insert}
	if err := s.check(); err != nil {
		return checked(failed, err), err
	}
	n, err := s.run(ctx)
	if err != nil {
		return failed, err
	}
	if err := w.annotate(ctx, sql, args); err != nil {
		return failed, err
	}
	// The release ends the savepoint whatever it gives, and a release that fails leaves a
	// transaction that the core lets only roll back.
	kept = true
	if err := w.release(ctx); err != nil {
		return failed, err
	}
	t.writes++
	r.rows = n
	return Result{Changes: n, SetsChanges: insert}, nil
}

// queryIn runs the plan q in the transaction of the connection, and gives its rows, which read the
// state of the transaction. Their end does not end the transaction.
func (c *Conn) queryIn(ctx context.Context, r *report, q *Query) (*Rows, error) {
	t := c.tx
	r.at(t.position(), t.write != nil)
	if err := c.ready(ctx); err != nil {
		r.end(ctx, err)
		return nil, err
	}
	src, err := buildQuery(ctx, t.reader(), q)
	if err != nil {
		r.end(ctx, err)
		return nil, err
	}
	rows := &Rows{ctx: ctx, tx: inTx{}, src: src, columns: src.columns(), report: r}
	c.rows = rows
	return rows, nil
}

// inTx is the end of the rows of a query in a transaction that spans statements, which ends nothing:
// the transaction ends with its commit or roll back.
type inTx struct{}

func (inTx) end(context.Context) error { return nil }
