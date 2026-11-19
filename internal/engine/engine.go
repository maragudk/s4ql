// Package engine is the SQL engine of s4ql: it runs a statement on a database of the storage core,
// with SQLite's results and SQLite's errors.
//
// A statement goes through three stages, each of which a test can call on its own:
//
//  1. [stmt.Check] reads the text into a typed statement, or gives SQLite's syntax error, "not
//     supported", or no statement.
//  2. [NewPlan] turns the statement into a [Plan]: plain data, which [Format] prints.
//  3. The engine runs the plan in a transaction of the core: it builds a tree of operators from the
//     plan, which look the names up in the transaction and give SQLite's errors about them, and
//     runs the tree. Each operator hands its rows to the one above it through a push-style
//     iterator.
//
// Each statement is its own transaction. A statement that changes the database runs in a write
// transaction, which is annotated with the statement's text and committed, and rolled back on any
// failure; a query runs in a read transaction, which ends when its rows are read to the end or
// closed.
//
// A statement runs on a [Conn], a connection of the engine, which is to the engine what a
// connection of a pool is to its database: it runs one statement at a time, and it owns one reader
// of the storage core, on which its read transactions run. So the connections that are open are
// the queries that can run at once, and nothing in the engine or in the core limits their number.
//
// An error of a statement is one of these:
//
//   - SQLite's error, as a [*sqlerr.Error], with SQLite's result code, extended result code, and
//     message: a syntax error, or an error about the tables, the columns, or a primary key.
//   - "Not supported", as a [*stmt.NotSupportedError] that matches [stmt.ErrNotSupported].
//   - [ErrNoStatement], for a text that holds no statement.
//   - An error of the storage core, which matches its sentinel in package core with [errors.Is], or
//     an error of the context.
//   - An internal error, which matches [ErrInternal]: a fault of s4ql.
//
// The engine logs one event for each statement through log/slog; see eventStatement in
// telemetry.go.
package engine

import (
	"context"
	"errors"
	"iter"
	"log/slog"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// Options of an [Engine]. The zero value of each option means its default.
type Options struct {
	// Logger of the events of the engine: one for each statement. The default discards them.
	Logger *slog.Logger

	// bugs planted by the tests of this package.
	bugs bugs
}

// Engine runs statements on one database, each on a [Conn] of the engine. It is safe for concurrent
// use, and a Conn is not: each statement is its own transaction of the database, and the database
// runs one write transaction at a time, and the read transactions of the connections in parallel.
type Engine struct {
	db   database
	host host.Host
	log  *slog.Logger
	bugs bugs
}

// New engine for the database d, whose instance runs on the host h, which the engine reads the
// time from.
func New(d *core.DB, h host.Host, options Options) *Engine {
	return newEngine(coreDB{d: d}, h, options)
}

func newEngine(d database, h host.Host, options Options) *Engine {
	log := options.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Engine{db: d, host: h, log: log, bugs: options.bugs}
}

// Result of a statement, for SQLite's count of changes, which sqlite3_changes gives after the
// statement.
type Result struct {
	// Changes is the count of changes that the statement sets, if SetsChanges.
	Changes int64

	// SetsChanges reports whether the statement sets SQLite's count of changes. An insert sets it:
	// to the number of rows that it inserted, and to 0 if it fails after it has started to write,
	// as with `UNIQUE constraint failed`, which SQLite gives when it steps the statement. CREATE
	// TABLE, DROP TABLE, SELECT, and a statement that fails before it starts to write, as SQLite
	// fails it when it prepares it, leave the count as the statement before left it.
	SetsChanges bool
}

// Conn is a connection of an [Engine]: it runs the statements that it is given, one at a time. It
// owns one reader of the database, which it opens for its first statement that reads, so a
// connection that only writes has none, and closes in [Conn.Close]. Every read transaction of the
// connection runs on that reader, one at a time: a statement that reads while the rows of an
// earlier query of the connection have not ended fails at once, with an error that matches
// [core.ErrReaderBusy], and does not wait for them. A statement that changes the database needs no
// reader, and runs.
//
// A reader that cannot be opened fails the statement that needed it, and the next statement that
// reads tries again. If the cache cannot be opened, that is a local failure, which stops the
// instance as any other.
//
// A Conn is not safe for concurrent use. It must be closed, after the rows of its queries have
// ended.
type Conn struct {
	e *Engine
	// rd is the reader of the connection, from its first read on.
	rd     readConn
	closed bool
}

// Conn gives a new connection of the engine, which holds nothing until it runs a statement.
func (e *Engine) Conn() *Conn {
	return &Conn{e: e}
}

// Close the connection, and its reader, if it has one. A read transaction of the connection that
// is open ends with it, and the rows that hold it give an error from then on. It fails, and closes
// nothing, while a loop over the rows of a query of the connection runs. A statement on a
// connection that is closed fails with an internal error. Closing a closed connection does
// nothing.
func (c *Conn) Close(ctx context.Context) error {
	var err error
	if c.rd != nil {
		if err = c.rd.close(ctx); errors.Is(err, core.ErrReaderInUse) {
			return err
		}
	}
	c.closed, c.rd = true, nil
	return err
}

// read starts a read transaction on the reader of the connection, which it opens if the connection
// has none.
func (c *Conn) read(ctx context.Context) (readTx, error) {
	if c.closed {
		return nil, internalError("a statement on a connection that is closed")
	}
	if c.rd == nil {
		rd, err := c.e.db.reader(ctx)
		if err != nil {
			return nil, err
		}
		c.rd = rd
	}
	return c.rd.read(ctx)
}

// begin a write transaction of the database.
func (c *Conn) begin(ctx context.Context) (writeTx, error) {
	if c.closed {
		return nil, internalError("a statement on a connection that is closed")
	}
	return c.e.db.begin(ctx)
}

// Exec runs the statement of the text src, and gives its result. A query is run to its end, in its
// read transaction, and its rows are dropped. The result is given with an error too: a failed
// insert sets the count of changes.
//
// SQLite's last insert rowid is not changed by any statement of s4ql, because every table is WITHOUT
// ROWID, and SQLite does not record an insert into such a table.
func (c *Conn) Exec(ctx context.Context, src string) (Result, error) {
	r := c.e.newReport(src)
	// Every statement of Exec has logged its event when exec returns, so one without is a panic.
	defer r.panicked(ctx)
	return c.exec(ctx, r, src)
}

// exec is [Conn.Exec] of src for the report r.
func (c *Conn) exec(ctx context.Context, r *report, src string) (Result, error) {
	p, text, err := prepare(r, src)
	if err != nil {
		r.end(ctx, err)
		return Result{}, err
	}
	if q, ok := p.(*Query); ok {
		rows, err := c.query(ctx, r, q)
		if err != nil {
			return Result{}, err
		}
		for _, err := range rows.All() {
			if err != nil {
				return Result{}, err
			}
		}
		return Result{}, nil
	}
	res, err := c.write(ctx, r, p, text)
	r.end(ctx, err)
	return res, err
}

// Query runs the statement of the text src, and gives its rows. A statement that is not a query is
// run as [Conn.Exec] runs it, and gives rows with no columns and no rows, and its result in
// [Rows.Result], also when it fails: Query then gives those rows with the error.
//
// The rows of a query are read in its read transaction, which is open until the rows end; see
// [Rows]. Until then, the reader of the connection is taken, and a statement of the connection that
// reads fails.
func (c *Conn) Query(ctx context.Context, src string) (*Rows, error) {
	r := c.e.newReport(src)
	returned := false
	defer func() {
		if !returned {
			r.panicked(ctx)
		}
	}()
	rows, err := c.queryText(ctx, r, src)
	returned = true
	return rows, err
}

// queryText is [Conn.Query] of src for the report r.
func (c *Conn) queryText(ctx context.Context, r *report, src string) (*Rows, error) {
	p, text, err := prepare(r, src)
	if err != nil {
		r.end(ctx, err)
		return nil, err
	}
	if q, ok := p.(*Query); ok {
		return c.query(ctx, r, q)
	}
	res, err := c.write(ctx, r, p, text)
	r.end(ctx, err)
	return &Rows{done: true, result: res}, err
}

// prepare the text src for the report r: check it, and give the plan of its statement and the text
// of the statement. A syntax error is the one that SQLite gives for src on an empty database, which
// [stmt.Check] alone decides, whatever the database holds.
func prepare(r *report, src string) (Plan, string, error) {
	s, err := stmt.Check(src)
	if err != nil {
		return nil, "", err
	}
	if s == nil {
		return nil, "", ErrNoStatement
	}
	r.of(s)
	span := s.Bounds()
	return NewPlan(s), src[span.Start:span.End], nil
}

// write runs the plan p of a statement that changes the database, whose text is sql, in a write
// transaction of its own, and gives its result. The transaction is committed if the statement
// succeeds, and rolled back otherwise, also when an operator panics.
func (c *Conn) write(ctx context.Context, r *report, p Plan, sql string) (_ Result, err error) {
	e := c.e
	_, insert := p.(*Insert)
	if insert && e.bugs.checkOutside {
		// The planted bug: the checks of the insert, in a read transaction of their own.
		if err := c.checkOutside(ctx, r, p); err != nil {
			return Result{}, err
		}
	}
	tx, err := c.begin(ctx)
	if err != nil {
		return Result{}, err
	}
	r.at(tx.position(), true)
	ended := false
	defer func() {
		// The planted bug keepOpen leaves the transaction of a statement that failed open.
		if !ended && (err == nil || !e.bugs.keepOpen) {
			_ = tx.rollback(ctx)
		}
	}()
	b := e.bugs
	b.skipUnique = b.skipUnique || b.checkOutside
	s, err := buildSink(ctx, tx, p, b)
	if err != nil {
		return Result{}, err
	}
	// From here on, a failed insert sets the count of changes to 0.
	failed := Result{SetsChanges: insert}
	n, err := s.run(ctx)
	if err != nil {
		return failed, err
	}
	if err := tx.annotate(ctx, sql); err != nil {
		return failed, err
	}
	// The transaction ends with its commit, whatever the commit gives.
	ended = true
	if err := tx.commit(ctx); err != nil {
		if e.bugs.ackUnknown && errors.Is(err, core.ErrOutcomeUnknown) {
			// The planted bug: a commit whose outcome is unknown counts as committed.
			r.rows = n
			return Result{Changes: n, SetsChanges: insert}, nil
		}
		return failed, err
	}
	r.rows = n
	return Result{Changes: n, SetsChanges: insert}, nil
}

// checkOutside is the planted bug that checks an insert in a read transaction before its write
// transaction: the table, the column, and the key, without a write.
func (c *Conn) checkOutside(ctx context.Context, r *report, p Plan) error {
	rt, err := c.read(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = rt.end(ctx) }()
	r.at(rt.position(), true)
	s, err := buildSink(ctx, readOnly{rt}, p, bugs{})
	if err != nil {
		return err
	}
	_, err = s.run(ctx)
	return err
}

// readOnly is a [writer] of a read transaction that writes nothing, for the planted bug checkOutside.
type readOnly struct {
	readTx
}

func (readOnly) createTable(context.Context, *s4qlpb.CreateTable) error { return nil }
func (readOnly) dropTable(context.Context, string) error                { return nil }
func (readOnly) upsert(context.Context, string, []*s4qlpb.Value) error  { return nil }

// query runs the plan q in a read transaction of its own, and gives its rows, which hold the
// transaction until they end. The transaction ends if the rows cannot be built, also when that
// panics; the event of a panic is then left to [Conn.Exec] and [Conn.Query].
func (c *Conn) query(ctx context.Context, r *report, q *Query) (_ *Rows, err error) {
	tx, err := c.read(ctx)
	if err != nil {
		r.end(ctx, err)
		return nil, err
	}
	r.at(tx.position(), false)
	built := false
	defer func() {
		if built {
			return
		}
		endErr := tx.end(ctx)
		// The rows are not built when buildQuery fails, with an error, or panics, with none.
		if err != nil {
			err = first(err, endErr)
			r.end(ctx, err)
		}
	}()
	src, err := buildQuery(ctx, tx, q)
	if err != nil {
		return nil, err
	}
	rows := &Rows{ctx: ctx, tx: tx, src: src, columns: src.columns(), report: r}
	built = true
	return rows, nil
}

// Rows of a query, which hold its read transaction until they end: when [Rows.All] has given the last
// row or an error, when the loop over it stops early or panics, or when [Rows.Close] is called
// outside a loop over All. Rows are not safe for concurrent use.
//
// A loop over All holds the transaction while it runs, also while it is suspended in the body of
// the loop, so Close during the loop cannot end it: it makes the loop end at its next row, and the
// transaction ends then. An iterator that is suspended must be resumed, or stopped, for the
// transaction to end.
type Rows struct {
	ctx     context.Context
	tx      readTx
	src     source
	columns []Column
	report  *report
	// reading is set while a loop over All runs, closing when Close was called during it, and done
	// once the rows have ended.
	reading, closing, done bool
	// endErr is the error of the end of the read transaction after an early end of the loop, which
	// Close gives.
	endErr error
	// result of a statement that is not a query.
	result Result
}

// Result of the statement, if it is not a query, as [Conn.Exec] gives it. A query sets no count of
// changes.
func (r *Rows) Result() Result {
	return r.result
}

// Columns of each row: their names as SQLite gives them for the query, and their declared types.
func (r *Rows) Columns() []Column {
	return r.columns
}

// All rows, in order, each with a value for each of [Rows.Columns]. The sequence ends at the first
// error, and can be used once: on rows that have ended, it gives nothing. When it ends, by its last
// row, an error, a loop that stops early, or a panic in the loop, the read transaction of the query
// ends.
func (r *Rows) All() iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		if r.done || r.reading {
			return
		}
		r.reading = true
		var err error
		// returned is set when the loop below returns, and consuming while the body of the loop over
		// All runs. A panic at any other time is in the engine.
		returned, consuming := false, false
		// The read transaction ends here when the loop stops early, panics, or meets an error. A
		// failure of that end is the error of Close.
		defer func() {
			r.reading = false
			if !returned && !consuming {
				err = errPanicked
			}
			r.endErr = r.end(err)
		}()
		consume := func(row []*s4qlpb.Value, err error) bool {
			consuming = true
			more := yield(row, err)
			consuming = false
			return more
		}
		r.read(consume, &err)
		returned = true
	}
}

// read the rows of the query, and give each to yield, up to the first error, which read sets in
// failed before it gives it. After the last row, it ends the read transaction, and a failure of
// that end is the last error.
func (r *Rows) read(yield func([]*s4qlpb.Value, error) bool, failed *error) {
	for row, err := range r.src.rows(r.ctx) {
		if err != nil {
			*failed = err
			yield(nil, err)
			return
		}
		r.report.rows++
		if !yield(row, nil) || r.closing {
			return
		}
		r.report.e.host.Yield(r.ctx, "engine: between two rows of a query")
	}
	if err := r.end(nil); err != nil {
		yield(nil, err)
	}
}

// Close the rows, and end the read transaction of the query, unless it has ended; see [Rows]. It
// gives the error of the end of the transaction, also of an end after a loop that stopped early.
func (r *Rows) Close() error {
	if r.reading {
		r.closing = true
		return nil
	}
	if err := r.endErr; err != nil {
		r.endErr = nil
		return err
	}
	if r.report != nil && r.report.e.bugs.keepRead {
		// The planted bug: the rows are closed, and their read transaction is left open.
		return nil
	}
	return r.end(nil)
}

// end the rows of a query that ended with err, and its read transaction, and log its event.
func (r *Rows) end(err error) error {
	if r.done {
		return nil
	}
	r.done = true
	// The event is logged below, unless the end of the transaction panics.
	defer r.report.panicked(r.ctx)
	endErr := r.tx.end(r.ctx)
	r.report.end(r.ctx, first(err, endErr))
	return endErr
}

// first of the errors that is not nil, or nil.
func first(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
