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
// A statement is its own transaction, unless a transaction that spans statements is open on its
// connection. A statement of its own that changes the database runs in a write transaction, which is
// annotated with the statement's text and committed, and rolled back on any failure; a query runs in
// a read transaction, which ends when its rows are read to the end or closed.
//
// A transaction that spans statements begins with [Conn.Begin], which takes the place of the
// single writer as SQLite's `begin immediate` does, or with [Conn.BeginReadOnly], a read transaction
// that waits for nothing, and ends with [Conn.Commit] or [Conn.Rollback]; see tx.go. A statement
// that fails in it has no effect, and the transaction stays open, as in SQLite.
//
// A statement runs on a [Conn], a connection of the engine: it runs one statement at a time, and
// it owns one reader of the storage core, on which its read transactions run. So the connections
// that are open are the queries that can run at once, and nothing in the engine or in the core
// limits their number.
//
// An error of a statement is one of these:
//
//   - SQLite's error, as a [*sqlerr.Error], with SQLite's result code, extended result code, and
//     message: a syntax error, an error about the tables, the columns, or a primary key, or a text
//     or a blob that is too long.
//   - "Not supported", as a [*stmt.NotSupportedError] that matches [stmt.ErrNotSupported], or, for
//     an argument that no column holds and for a value of another type than its column, also one
//     that a condition compares with a column, an error that matches stmt.ErrNotSupported.
//   - [ErrNoStatement], for a text that holds no statement.
//   - An error that matches [ErrArguments], for another number of arguments than the statement has
//     parameters.
//   - [ErrRowsOpen], [ErrTxOpen], [ErrNoTx], or [ErrTxDone], for what the state of the transaction
//     of the connection does not allow, and an error that matches [ErrTooLarge] for a commit that
//     the store cannot take.
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
	"sync/atomic"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// Options of an [Engine]. The zero value of each option means its default.
type Options struct {
	// Logger of the events of the engine: one for each statement. The default discards them.
	Logger *slog.Logger

	// bugs planted by the tests of this package.
	bugs bugs
	// path makes every statement read the rows of its table by one access, where the rules would
	// choose one. The tests of this package set it, to show that every access gives the same rows.
	path path
	// Directions, if not nil, is the seed of the directions in which the queries of the engine read
	// their rows, forward or backward: see [Engine.reading]. With it the engine draws nothing from its
	// host for a query, and two engines with one seed read their n-th queries in the same direction,
	// so a run that must be had again from a seed, such as a simulation or a test, sets it. The
	// default draws each direction from the host.
	Directions *uint64
}

// Engine runs statements on one database, each on a [Conn] of the engine. It is safe for concurrent
// use, and a Conn is not: each statement is its own transaction of the database, and the database
// runs one write transaction at a time, and the read transactions of the connections in parallel.
type Engine struct {
	db   database
	host host.Host
	log  *slog.Logger
	bugs bugs
	// path that the tests force, as [Options.path] says.
	path path
	// seeded makes the engine draw the directions of its scans from draws, a sequence that its seed
	// fixes, and not from the host.
	seeded bool
	draws  atomic.Uint64
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
	e := &Engine{db: d, host: h, log: log, bugs: options.bugs, path: options.path}
	if options.Directions != nil {
		e.seeded = true
		e.draws.Store(*options.Directions)
	}
	return e
}

// reading of the next statement of the engine: by the path that the tests force, if any, and, for a
// query, forward or backward as the engine draws it, one draw for each query. The draw comes from the
// host, or, for an engine with a seed of its own, [Options.Directions], from the sequence of that
// seed, which is splitmix64, so that the n-th query of two engines with one seed reads in the same
// direction. A path with a direction draws nothing.
func (e *Engine) reading(query bool) reading {
	how := reading{path: e.path, bugs: e.bugs}
	switch {
	case how.path.direction != directionDrawn:
		how.backward = how.path.direction == directionBackward
	case !query:
	case e.seeded:
		how.drawn = true
		z := e.draws.Add(0x9e3779b97f4a7c15)
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		how.backward = (z^(z>>31))&1 == 1
	default:
		how.drawn = true
		how.backward = e.host.Uint64()&1 == 1
	}
	return how
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
// reader, and runs. A statement of its own of IF NOT EXISTS or IF EXISTS reads first, on the reader
// of the connection, or, while rows hold it, on a reader of its own for the time of its look; see
// [Conn.nothingToWrite].
//
// A transaction that spans statements is open on the connection from [Conn.Begin] or
// [Conn.BeginReadOnly] to [Conn.Commit] or [Conn.Rollback], and every statement of the connection
// runs in it meanwhile. While the rows of a query of the connection have not ended, no transaction
// begins, and no statement runs in a transaction: each fails at once with [ErrRowsOpen].
//
// A reader that cannot be opened fails the statement that needed it, and the next statement that
// reads tries again. If the cache cannot be opened, that is a local failure, which stops the
// instance as any other.
//
// A Conn is not safe for concurrent use. It must be closed, and only after the rows of its queries
// have ended.
type Conn struct {
	e *Engine
	// rd is the reader of the connection, from its first read on.
	rd     readConn
	closed bool
	// tx is the transaction that spans statements and is open on the connection, or nil.
	tx *transaction
	// rows of the last query of the connection, which may not have ended.
	rows *Rows
}

// Conn gives a new connection of the engine, which holds nothing until it runs a statement.
func (e *Engine) Conn() *Conn {
	return &Conn{e: e}
}

// Close the connection, and its reader, if it has one, and roll back its transaction, if one is
// open. It fails, with an error that matches [core.ErrReaderInUse], and closes nothing, while a loop
// over the rows of a query of the connection runs. Rows of the connection that have not ended must not be used after its close:
// their read transaction has ended with the reader, and what they give then is an internal error.
// A statement on a connection that is closed fails with an internal error too. Closing a closed
// connection does nothing.
func (c *Conn) Close(ctx context.Context) error {
	if c.rows != nil && c.rows.reading {
		return core.ErrReaderInUse
	}
	var err error
	if c.tx != nil {
		err = c.Rollback(ctx)
	}
	if c.rd != nil {
		closeErr := c.rd.close(ctx)
		if errors.Is(closeErr, core.ErrReaderInUse) {
			return closeErr
		}
		err = errors.Join(err, closeErr)
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

// Exec runs the statement of the text src, with args for its parameters, by position, and gives its
// result. A query is run to its end, in its read transaction, and its rows are dropped. The result
// is given with an error too: a failed insert sets the count of changes.
//
// The text is checked first, and then the arguments, before the statement needs the database: there
// must be one argument for each parameter, as [Check] checks it, and each must be a value that a
// column can hold: an integer, a real that is a finite number, valid UTF-8 text without the
// character NUL, a blob, or NULL.
//
// An insert then fails, in this order, with SQLite's errors of the names and of the number of its
// values, with SQLite's `string or blob too big` for a text or a blob that is longer than the limit
// of the database, which SQLite gives when it binds such an argument, with SQLite's error of a
// read-only transaction, with SQLite's `NOT NULL constraint failed` for the first column in the
// order of the table, the key among them, that does not take NULL and that it gives NULL or leaves
// out, with "not supported" for a value other than NULL of another type than its column, with
// `string or blob too big` for a row that is longer than the limit, also when its key exists, and
// with SQLite's error of a key that exists. A real that is -0.0 is stored as 0.0, as SQLite stores
// it, and its key is the key of 0.0.
//
// A select fails, in this order, with SQLite's `no such table`, with `no such column` for a column of
// its select list, and then for one of its conditions, as SQLite looks them up, with `string or blob
// too big` for a value of a condition that is longer than the limit, and with "not supported" for the
// first value of a condition, in the order of the text, that is not NULL and is of another type than
// its column. It gives the rows of the table for which every condition is true, with SQLite's
// comparisons; a comparison with NULL is never true. The rows come in the order of the key of the
// access that the engine chooses for the conditions, the primary key or an index, or in its reverse,
// as the engine draws it for each query: SQLite leaves the order unspecified.
//
// An update and a delete change the rows that a select with their WHERE would give, each once, in
// the order of the key, and count each, also a row whose values an update does not change. They
// fail, in this order, with SQLite's `no such table`, with `no such column` for a column of SET, the
// first in the order of the text, and then for one of WHERE, as for a select, with `string or blob
// too big` for a value of SET and then of WHERE that is longer than the limit, with "not supported"
// for a value of WHERE of another type than its column, with SQLite's error of a read-only
// transaction, with SQLite's `NOT NULL constraint failed` for the first column of SET, in the order
// of the table, that does not take NULL and gets it, if the update finds a row, with "not supported"
// for the first value of SET, in the order of the table, of another type than its column, and then,
// row by row, with `string or blob too big` for a row that is longer than the limit and with
// SQLite's error of a key that exists, for an update that gives a row the key of another.
//
// A create table with IF NOT EXISTS whose name a table has, a create index with IF NOT EXISTS whose
// name an index has, a drop table with IF EXISTS whose name no table has, and a drop index with IF
// EXISTS whose name no index has, does nothing, as SQLite does: it writes nothing, is not annotated,
// and succeeds also in a read-only transaction. A create index gives SQLite's errors of its table
// and of a table of its name first, and a create table that of an index of its name; see
// [buildSink].
//
// In a transaction that spans statements, the statement then fails with the error of ctx if ctx has
// ended, with the refusal of a statement of its own if the instance does not serve now, and with
// [ErrRowsOpen] while the rows of an earlier query of the connection are open. A statement that
// fails in a write transaction has no effect, and what the statements before it did is kept; the
// transaction stays open, unless the core lets it only roll back, as after a failure of the cache.
// In a read-only transaction, a statement that would write fails with SQLite's error under `pragma
// query_only = 1`, after SQLite's errors of the names that it gives when it prepares the statement.
//
// SQLite's last insert rowid is not changed by any statement of s4ql, because every table is WITHOUT
// ROWID, and SQLite does not record an insert into such a table.
func (c *Conn) Exec(ctx context.Context, src string, args ...*s4qlpb.Value) (Result, error) {
	r := c.newReport(src)
	// Every statement of Exec has logged its event when exec returns, so one without is a panic.
	defer r.panicked(ctx)
	return c.exec(ctx, r, src, args)
}

// exec is [Conn.Exec] of src with args for the report r.
func (c *Conn) exec(ctx context.Context, r *report, src string, args []*s4qlpb.Value) (Result, error) {
	p, text, err := prepare(r, src, args)
	if err != nil {
		r.end(ctx, err)
		return Result{}, err
	}
	if q, ok := p.(*Query); ok {
		rows, err := c.query(ctx, r, q, args)
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
	// The rows of a RETURNING are dropped.
	res, _, err := c.write(ctx, r, p, text, args)
	r.end(ctx, err)
	return res, err
}

// Query runs the statement of the text src, with args for its parameters, and gives its rows. The
// text and the arguments are checked as [Conn.Exec] checks them. A statement that is not a query is
// run as Exec runs it, and gives rows with no columns and no rows, and its result in [Rows.Result],
// also when it fails: Query then gives those rows with the error. An insert, an update, or a delete
// with RETURNING gives its rows, with its result in Rows.Result, once it has succeeded; see
// [Conn.write].
//
// The rows of a query are read in its read transaction, which is open until the rows end; see
// [Rows]. Until then, the reader of the connection is taken, and a statement of the connection that
// reads fails.
func (c *Conn) Query(ctx context.Context, src string, args ...*s4qlpb.Value) (*Rows, error) {
	r := c.newReport(src)
	returned := false
	defer func() {
		if !returned {
			r.panicked(ctx)
		}
	}()
	rows, err := c.queryText(ctx, r, src, args)
	returned = true
	return rows, err
}

// queryText is [Conn.Query] of src with args for the report r.
func (c *Conn) queryText(ctx context.Context, r *report, src string, args []*s4qlpb.Value) (*Rows, error) {
	p, text, err := prepare(r, src, args)
	if err != nil {
		r.end(ctx, err)
		return nil, err
	}
	if q, ok := p.(*Query); ok {
		return c.query(ctx, r, q, args)
	}
	res, ret, err := c.write(ctx, r, p, text, args)
	r.end(ctx, err)
	if ret == nil {
		return &Rows{done: true, result: res}, err
	}
	// The statement has ended, and its event is logged: the rows read nothing, and their end ends
	// nothing. In a transaction, they are the rows of the connection, as those of a query are.
	rows := &Rows{ctx: ctx, tx: inTx{}, src: ret, columns: ret.columns(), report: r, result: res}
	if c.tx != nil {
		c.rows = rows
	}
	return rows, nil
}

// newReport of a statement of the text src on the connection, which counts it in the transaction of
// the connection, if one is open.
func (c *Conn) newReport(src string) *report {
	r := c.e.newReport(src)
	if c.tx != nil {
		c.tx.statements++
		r.tx = c.tx.kind()
	}
	return r
}

// Check the text src, as [Conn.Exec] and [Conn.Query] check it, for a run with n arguments, without
// the database and without an event: the text's error, as [check] gives it, [ErrNoStatement],
// or the error of [ErrArguments] for another number of arguments than the statement has parameters;
// otherwise nil. It does not check what the arguments are.
func Check(src string, n int) error {
	s, err := check(src)
	if err != nil {
		return err
	}
	return countArguments(s.Parameters(), n)
}

// check the text src, as [Conn.Exec], [Conn.Query], and [Check] check it: its statement, or its
// error, as [stmt.Check] gives it, or [ErrNoStatement].
func check(src string) (stmt.Statement, error) {
	s, err := stmt.Check(src)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, ErrNoStatement
	}
	return s, nil
}

// setsChanges reports whether the plan p sets SQLite's count of changes: an insert, an update, and a
// delete do.
func setsChanges(p Plan) bool {
	switch p.(type) {
	case *Insert, *Update, *Delete:
		return true
	}
	return false
}

// prepare the text src with the arguments args for the report r: check the text, and then the
// arguments, and give the plan of its statement and the text of the statement. A syntax error is
// the one that SQLite gives for src on an empty database, which [stmt.Check] alone decides, whatever
// the database holds.
func prepare(r *report, src string, args []*s4qlpb.Value) (Plan, string, error) {
	s, err := check(src)
	if err != nil {
		return nil, "", err
	}
	r.of(s)
	if err := checkArguments(s.Parameters(), args); err != nil {
		return nil, "", err
	}
	span := s.Bounds()
	return NewPlan(s), src[span.Start:span.End], nil
}

// write runs the plan p of a statement that changes the database, whose text is sql, with the
// arguments args, in a write transaction of its own, and gives its result. The transaction is
// annotated with the text and the arguments, and committed if the statement succeeds and writes,
// and rolled back otherwise, also when an operator panics. A statement of IF NOT EXISTS or IF EXISTS
// that does nothing on the newest state succeeds in a read transaction, as [Conn.nothingToWrite]
// says, without the write transaction; one that does nothing in the write transaction succeeds there
// without an annotation, and the transaction is rolled back.
//
// An insert, an update, or a delete with RETURNING also gives the rows of its RETURNING, which the
// engine holds, once its commit has succeeded, and none if the statement or its commit fails: a row
// that it gives belongs to a change that is committed. The rows come in the order in which the
// statement changed them, or in its reverse, as the engine draws it for the statement, as it draws
// the direction of a query; see [returned].
func (c *Conn) write(ctx context.Context, r *report, p Plan, sql string, args []*s4qlpb.Value) (_ Result, _ *returned, err error) {
	if c.tx != nil {
		return c.writeIn(ctx, r, p, sql, args)
	}
	if c.pending() {
		return Result{}, nil, ErrRowsOpen
	}
	e := c.e
	if nothing, err := c.nothingToWrite(ctx, r, p, args); nothing {
		return Result{}, nil, err
	}
	_, insert := p.(*Insert)
	if insert && e.bugs.checkOutside {
		// The planted bug: the checks of the insert, in a read transaction of their own.
		if err := c.checkOutside(ctx, r, p, args); err != nil {
			return Result{}, nil, err
		}
	}
	tx, err := c.begin(ctx)
	if err != nil {
		return Result{}, nil, err
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
	order := e.order(p)
	s, err := buildSink(ctx, tx, p, args, b, e.db.lengthLimit(), e.reading(false))
	if err != nil {
		return Result{}, nil, err
	}
	r.reads(tx, s)
	r.drew(order)
	if r.doesNothing(s) {
		// The statement does nothing, which [Conn.nothingToWrite] could not tell, or which a commit
		// since its read transaction made so. The transaction is rolled back above, and nothing of it
		// is in the log. It wrote nothing, so its event has no parent.
		r.write = false
		return Result{}, nil, nil
	}
	// From here on, a failed insert, update, or delete sets the count of changes to 0, but for "not
	// supported".
	failed := Result{SetsChanges: setsChanges(p)}
	if err := s.check(ctx); err != nil {
		return checked(failed, err), nil, err
	}
	n, err := s.run(ctx)
	if err != nil {
		return failed, nil, err
	}
	annotated := args
	if e.bugs.annotateNext {
		// The planted bug: the annotation holds each argument plus one.
		annotated = nil
		for _, a := range args {
			annotated = append(annotated, s4qlpb.Value_builder{Integer: new(a.GetInteger() + 1)}.Build())
		}
	}
	if err := tx.annotate(ctx, sql, annotated); err != nil {
		return failed, nil, err
	}
	// The transaction ends with its commit, whatever the commit gives.
	ended = true
	if err := commitError(tx.commit(ctx)); err != nil {
		if e.bugs.ackUnknown && errors.Is(err, core.ErrOutcomeUnknown) {
			// The planted bug: a commit whose outcome is unknown counts as committed.
			r.rows = n
			return Result{Changes: n, SetsChanges: setsChanges(p)}, returnedOf(s, order), nil
		}
		return failed, nil, err
	}
	r.rows = n
	return Result{Changes: n, SetsChanges: setsChanges(p)}, returnedOf(s, order), nil
}

// order of the rows of the RETURNING of the plan p, as [Engine.reading] draws it for a query: the
// engine draws it for an insert, an update, and a delete with RETURNING, one draw for each, and for
// no other plan.
func (e *Engine) order(p Plan) reading {
	var r *Returning
	switch p := p.(type) {
	case *Insert:
		r = p.Returning
	case *Update:
		r = p.Returning
	case *Delete:
		r = p.Returning
	}
	if r == nil {
		return reading{}
	}
	return e.reading(true)
}

// returnedOf the sink s, which has run, with the order of its rows: the rows of its RETURNING, or nil
// for a statement without one.
func returnedOf(s sink, order reading) *returned {
	ret := returnsOf(s)
	if ret == nil {
		return nil
	}
	return newReturned(ret, order)
}

// nothingToWrite reports whether the plan p of a statement of its own, with the arguments args, is
// one of IF NOT EXISTS or IF EXISTS that does nothing on the newest state, which it looks up in a
// read transaction of the connection, or, while the rows of an earlier query of the connection hold
// its reader, of a reader of its own, and gives the error of the end of that transaction then.
// SQLite only reads the schema for such a statement, so it waits for no writer; here too it neither
// waits for the place of the writer nor takes it. It reports false, and the statement runs in a
// write transaction, which looks again and decides, for another plan, for one that does something or
// fails there, and where the read transaction cannot begin.
func (c *Conn) nothingToWrite(ctx context.Context, r *report, p Plan, args []*s4qlpb.Value) (bool, error) {
	switch p := p.(type) {
	case *CreateTable:
		if !p.IfNotExists {
			return false, nil
		}
	case *CreateIndex:
		if !p.IfNotExists {
			return false, nil
		}
	case *DropTable:
		if !p.IfExists {
			return false, nil
		}
	case *DropIndex:
		if !p.IfExists {
			return false, nil
		}
	default:
		return false, nil
	}
	rt, err := c.read(ctx)
	if errors.Is(err, core.ErrReaderBusy) {
		// The rows of an earlier query of the connection hold its reader, so the statement looks on a
		// reader of its own, which it closes when it has looked.
		rd, rdErr := c.e.db.reader(ctx)
		if rdErr != nil {
			return false, nil
		}
		defer func() { _ = rd.close(ctx) }()
		rt, err = rd.read(ctx)
	}
	if err != nil {
		return false, nil
	}
	ended := false
	defer func() {
		// Also when an operator panics.
		if !ended {
			_ = rt.end(ctx)
		}
	}()
	// The planted bug replaceExisting must reach the write transaction, where it replaces the table.
	s, err := buildSink(ctx, readOnly{rt}, p, args, bugs{replaceExisting: c.e.bugs.replaceExisting}, c.e.db.lengthLimit(), c.e.reading(false))
	if _, ok := s.(nothing); err != nil || !ok {
		return false, nil
	}
	r.at(rt.position(), false)
	r.doesNothing(s)
	ended = true
	return true, rt.end(ctx)
}

// checked is the result of a statement whose sink's check failed with err, where failed is the result
// of a statement that fails after it has started to write. SQLite gives its errors of the check,
// such as `NOT NULL constraint failed`, when it steps the statement, so they set the count of changes
// as failed does; "not supported" for a value of another type leaves the count, as every "not
// supported" does.
func checked(failed Result, err error) Result {
	if _, ok := errors.AsType[*sqlerr.Error](err); ok {
		return failed
	}
	return Result{}
}

// checkOutside is the planted bug that checks an insert in a read transaction before its write
// transaction: the table, the column, and the key, without a write.
func (c *Conn) checkOutside(ctx context.Context, r *report, p Plan, args []*s4qlpb.Value) error {
	rt, err := c.read(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = rt.end(ctx) }()
	r.at(rt.position(), true)
	s, err := buildSink(ctx, readOnly{rt}, p, args, bugs{}, c.e.db.lengthLimit(), c.e.reading(false))
	if err != nil {
		return err
	}
	if err := s.check(ctx); err != nil {
		return err
	}
	i, ok := s.(*insert)
	if !ok {
		return internalError("the checks outside of a sink of type %T", s)
	}
	name, key := i.table.GetTable(), i.key
	for _, row := range i.src.data {
		existing, err := rt.get(ctx, name, []*s4qlpb.Value{row[key]})
		if err != nil {
			return err
		}
		if existing != nil {
			return uniqueFailed(name, i.table.GetColumns()[key].GetName())
		}
	}
	return nil
}

// readOnly is a [writer] of a read transaction that writes nothing, for the sinks that are built and
// not run: for the planted bug checkOutside, and for the lookups of a write in a read-only
// transaction, which then refuses the write.
type readOnly struct {
	readTx
}

func (readOnly) createTable(context.Context, *s4qlpb.CreateTable) error { return nil }
func (readOnly) dropTable(context.Context, string) error                { return nil }
func (readOnly) createIndex(context.Context, *s4qlpb.CreateIndex) error { return nil }
func (readOnly) dropIndex(context.Context, string) error                { return nil }
func (readOnly) upsert(context.Context, string, []*s4qlpb.Value) error  { return nil }
func (readOnly) insertNew(context.Context, string, []*s4qlpb.Value) (bool, error) {
	return false, nil
}
func (readOnly) delete(context.Context, string, []*s4qlpb.Value) (bool, error) { return false, nil }
func (readOnly) keys(context.Context, string, func(keySet) error) error        { return nil }

// query runs the plan q, with args for its parameters, in a read transaction of its own, and gives
// its rows, which hold the transaction until they end. The transaction ends if the rows cannot be
// built, also when that panics; the event of a panic is then left to [Conn.Exec] and [Conn.Query].
func (c *Conn) query(ctx context.Context, r *report, q *Query, args []*s4qlpb.Value) (_ *Rows, err error) {
	if c.tx != nil {
		return c.queryIn(ctx, r, q, args)
	}
	if c.pending() {
		r.end(ctx, ErrRowsOpen)
		return nil, ErrRowsOpen
	}
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
	how := c.e.reading(true)
	src, err := buildQuery(ctx, c.e.bugs.reader(tx), q, args, c.e.db.lengthLimit(), how)
	if err != nil {
		return nil, err
	}
	r.reads(tx, src)
	r.drew(how)
	rows := &Rows{ctx: ctx, tx: tx, src: src, columns: src.columns(), report: r}
	built = true
	c.rows = rows
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
	ctx context.Context
	// tx is what the end of the rows ends: the read transaction of a query of its own, nothing for a
	// query in a transaction that spans statements, or that transaction, once it has ended while a
	// loop over the rows ran.
	tx interface {
		end(ctx context.Context) error
	}
	src     source
	columns []Column
	report  *report
	// reading is set while a loop over All runs, closing when Close was called during it, and done
	// once the rows have ended.
	reading, closing, done bool
	// endErr is the error of the end of the read transaction after an early end of the loop, which
	// Close gives.
	endErr error
	// cut is the error that a loop over All ends with at its next row, because the transaction of the
	// rows has ended, or nil.
	cut error
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
		if !yield(row, nil) {
			return
		}
		if r.cut != nil {
			*failed = r.cut
			yield(nil, r.cut)
			return
		}
		if r.closing {
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
	if _, held := r.src.(*returned); !held && r.report != nil && r.report.e.bugs.keepRead {
		// The planted bug: the rows are closed, and their read transaction is left open. The rows of
		// a RETURNING have none.
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

// pending reports whether the rows of the last query of the connection hold the end of a
// transaction that has ended, which then waits for a loop over the rows to end: no statement runs on
// the connection until it has.
func (c *Conn) pending() bool {
	return c.rowsOpen() && c.rows.cut != nil
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
