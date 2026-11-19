package s4ql

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"iter"
	"sync"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/s4qlpb"
)

// rows of a query, which hold its read transaction until they are closed. database/sql closes them
// after the last row, after an error, when the context of the query ends, and when the application
// does. The connector closes those that are left when it is closed.
//
// The engine gives its rows by pushing them into a loop, and database/sql pulls them one at a time,
// so the loop runs in a coroutine of [iter.Pull2], which is suspended between two rows. The read
// transaction ends when the loop does, so the close stops the coroutine.
type rows struct {
	// ctx of the query, for the events of the driver.
	ctx  context.Context
	c    *connector
	rows *engine.Rows

	// mu is held by every call. database/sql calls Next and Close one at a time, and the close of
	// the connector can come beside either.
	mu sync.Mutex
	// next and stop are the coroutine of the loop over the rows, from the first call of Next on.
	next func() ([]*s4qlpb.Value, error, bool)
	stop func()
	// closed is set once the rows are closed.
	closed bool
}

var (
	_ driver.Rows                           = (*rows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*rows)(nil)
)

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
// the error of an instance that was closed, and never the end of the rows.
func (r *rows) Next(dest []driver.Value) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.c.error(r.ctx, errClosed)
	}
	if r.next == nil {
		r.next, r.stop = iter.Pull2(r.rows.All())
	}
	row, err, ok := r.next()
	if !ok {
		return io.EOF
	}
	if err != nil {
		return r.c.error(r.ctx, err)
	}
	if len(row) != len(dest) {
		return r.c.error(r.ctx, fmt.Errorf("%w: a row has %v values for %v columns", engine.ErrInternal, len(row), len(dest)))
	}
	for i, v := range row {
		if dest[i], err = value(v); err != nil {
			return r.c.error(r.ctx, err)
		}
	}
	return nil
}

// Close the rows, and end the read transaction of the query, unless it has ended. It gives the
// error of the end of the transaction.
func (r *rows) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.c.error(r.ctx, r.close())
}

// shut is the close of the rows by the connector.
func (r *rows) shut() {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.close()
}

// close the rows, unless they are closed, with mu held.
func (r *rows) close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.stop != nil {
		// The loop holds the transaction while it is suspended, and ends it when it is stopped.
		r.stop()
	}
	r.c.release(r)
	return r.rows.Close()
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
