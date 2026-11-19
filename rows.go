package s4ql

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"runtime/debug"
	"sync"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
)

// rows of a query, which hold its read transaction until they are closed, by [rows.Close] or by the
// close of their connector.
//
// The engine gives its rows by pushing them into a loop, and [rows.Next] is asked for one row at a
// time, so the loop runs in a task of its own, which waits between two rows; see [loop]. The read
// transaction ends when the loop does, so the close ends the loop.
//
// A panic in the loop, in a turn that Next gave the task, is raised again on the goroutine that
// calls Next, so that an application that recovers panics can recover it. The rows are closed then,
// and the read transaction has ended. A panic in a turn that a close gave the task, at the end of
// the loop, is not raised, because database/sql closes rows also from a goroutine of its own, when
// the context of their query ends, where a panic would end the process: Close gives it as an
// internal error, and the close by the connector drops it. The task logs every panic that it
// recovers, with the stack where it came, as the event "panic" at Error.
type rows struct {
	// ctx of the query.
	ctx context.Context
	c   *connector
	// conn is the connection of the query, whose connection of the engine the rows are of.
	conn *conn
	rows *engine.Rows

	// mu is held by Next, Close, and the close by the connector, which can come beside either.
	mu sync.Mutex
	// loop over the rows, from the first call of Next on.
	loop *loop
	// closed is set once the rows are closed, and panicked if Next raised a panic of the loop.
	closed, panicked bool
}

var (
	_ driver.Rows                           = (*rows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*rows)(nil)
)

// loop over the rows of a query, in a task of the host, which hands over one row at a time: the
// task gives a row and waits for its turn, and the one that reads the rows takes the row, and gives
// the task its turn when it wants the next. So only one of the two runs at any time, and the fields
// need no lock.
//
// It is a task and not a coroutine of [iter.Pull2], because the rows are read and closed from more
// than one goroutine, and the Go runtime ends the process when a coroutine is resumed from a
// goroutine whose lock to an operating system thread differs from that of the goroutine that
// created it.
type loop struct {
	// turn is released for the task to go on, and given by the task when it has a row or has ended.
	// Both start with their one permit taken.
	turn, given host.Semaphore
	// row and err are what the loop gave last.
	row []*s4qlpb.Value
	err error
	// ended is set once the loop has ended, and the read transaction with it.
	ended bool
	// stop tells the task to end the loop at its turn.
	stop bool
	// panic is the value of a panic in the loop, which ended it, if it panicked.
	panic any
}

// start the loop over the rows.
func (r *rows) start(ctx context.Context) *loop {
	h := r.c.host
	l := &loop{turn: h.NewSemaphore("turn of the rows", 1), given: h.NewSemaphore("row of the rows", 1)}
	// Neither wait can fail, here or later: the context cannot end, and that is all a wait fails on.
	_ = l.turn.Acquire(ctx)
	_ = l.given.Acquire(ctx)
	h.Go(ctx, fmt.Sprint("rows of query ", r.c.queries.Add(1)), func(ctx context.Context) {
		// The loop ends here also when it panics, and the panic goes to the one whose turn it is.
		// The engine has ended the read transaction then.
		defer func() {
			if v := recover(); v != nil {
				r.c.log.ErrorContext(ctx, "panic", "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				l.panic = v
			}
			l.row, l.err, l.ended = nil, nil, true
			l.given.Release(ctx)
		}()
		for row, err := range r.rows.All() {
			l.row, l.err = row, err
			l.given.Release(ctx)
			_ = l.turn.Acquire(ctx)
			if l.stop {
				break
			}
		}
	})
	return l
}

// Columns gives the names of the columns, as SQLite gives them for the query.
func (r *rows) Columns() []string {
	columns := r.rows.Columns()
	names := make([]string, len(columns))
	for i, c := range columns {
		names[i] = c.Name
	}
	return names
}

// ColumnTypeDatabaseTypeName gives the declared type of the column, as SQLite reports it, such as
// "INT". It is empty for a column that is not a column of a table.
func (r *rows) ColumnTypeDatabaseTypeName(index int) string {
	return r.rows.Columns()[index].DeclType
}

// Next row into dest, or [io.EOF] after the last. On rows that the connector has closed, it gives
// the error of an instance that was closed, and never the end of the rows. It raises a panic of the
// loop over the rows, after it has closed them, and gives an internal error after that.
func (r *rows) Next(dest []driver.Value) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.panicked {
		return r.c.public(r.ctx, fmt.Errorf("%w: the query panicked", engine.ErrInternal))
	}
	if r.closed {
		return r.c.public(r.ctx, errClosed)
	}

	// The waits of the loop do not end with the context of the query, which the engine watches.
	ctx := context.WithoutCancel(r.ctx)
	switch {
	case r.loop == nil:
		r.loop = r.start(ctx)
	case r.loop.ended:
		return io.EOF
	default:
		r.loop.turn.Release(ctx)
	}
	_ = r.loop.given.Acquire(ctx)

	if v := r.loop.panic; v != nil {
		r.loop.panic = nil
		r.panicked = true
		// The loop has ended, so the close only closes the rows of the engine, which have ended too.
		_ = r.close()
		panic(v)
	}
	row, err := r.loop.row, r.loop.err
	if r.loop.ended {
		return io.EOF
	}
	if err != nil {
		return r.c.public(r.ctx, err)
	}
	if len(row) != len(dest) {
		return r.c.public(r.ctx, fmt.Errorf("%w: a row has %v values for %v columns", engine.ErrInternal, len(row), len(dest)))
	}
	for i, v := range row {
		if dest[i], err = value(v); err != nil {
			return r.c.public(r.ctx, err)
		}
	}
	return nil
}

// Close the rows, and end the read transaction of the query, unless it has ended. It gives the
// error of the end of the transaction, and an internal error if the loop over the rows panicked in
// that end.
func (r *rows) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.c.public(r.ctx, r.close())
}

// shut is the close of the rows by the connector. It drops the error of the close: the task has
// logged a panic.
//
// database/sql makes no call of the rows while a call of their connection runs, but the connector
// closes them from a goroutine of its own, and the close touches the state of the connection of the
// engine. So shut holds that connection while it closes the rows, as a call does; see [conn]. If a
// call holds it, shut leaves the close to the call, which closes the rows when it ends, and does
// not wait: the call can wait for the stop of the instance, which the connector makes after shut.
func (r *rows) shut() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	c := r.conn
	c.mu.Lock()
	if c.busy {
		c.shut = append(c.shut, r)
		c.mu.Unlock()
		return
	}
	c.busy = true
	c.mu.Unlock()
	_ = r.close()
	c.mu.Lock()
	c.freeLocked(r.ctx)
}

// close the rows, unless they are closed, with mu held. It gives the error of the end of the
// transaction, and an internal error if the loop panicked in the turn that it gives the task.
func (r *rows) close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if l := r.loop; l != nil && !l.ended {
		// The task waits for its turn, and holds the transaction until the loop ends.
		ctx := context.WithoutCancel(r.ctx)
		l.stop = true
		l.turn.Release(ctx)
		_ = l.given.Acquire(ctx)
	}
	r.c.release(r)
	err := r.rows.Close()
	if l := r.loop; l != nil && l.panic != nil {
		err = fmt.Errorf("%w: the query panicked: %v", engine.ErrInternal, l.panic)
	}
	c := r.conn
	c.mu.Lock()
	c.held--
	later := c.closeLater && c.held == 0
	if later {
		c.closeLater = false
	}
	c.mu.Unlock()
	if later {
		// database/sql closed the connection while these rows were read, and has let go of it; see
		// [conn.Close]. So nothing else uses it, and nothing can get the error of its close.
		_ = c.e.Close(context.WithoutCancel(r.ctx))
	}
	return err
}

// noRows are the rows of a statement that is no query: no columns and no rows. They hold nothing.
type noRows struct{}

func (noRows) Columns() []string {
	return nil
}

func (noRows) Next([]driver.Value) error {
	return io.EOF
}

func (noRows) Close() error {
	return nil
}

// value of v for database/sql: an int64, a float64, a string, a []byte, or nil for NULL.
func value(v *s4qlpb.Value) (driver.Value, error) {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return v.GetInteger(), nil
	case s4qlpb.Value_Real_case:
		return v.GetReal(), nil
	case s4qlpb.Value_Text_case:
		return v.GetText(), nil
	case s4qlpb.Value_Blob_case:
		return v.GetBlob(), nil
	case s4qlpb.Value_Null_case:
		return nil, nil
	}
	return nil, fmt.Errorf("%w: a value of no storage class", engine.ErrInternal)
}
