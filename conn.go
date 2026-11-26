package s4ql

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/store"
)

// connector of one instance of a database: every connection that it gives runs its statements on
// that instance. Its Close closes the instance.
//
// It logs what the driver decides itself, through the logger of the options: the event "open" when
// the instance is opened, at Info; "close" when it is closed, at Info, and at Warn if the close
// failed, which includes a lease that is not released; "stop_seen" the first time that it answers
// a call with [ErrStopped], at Warn; and "panic" for a panic in the loop over the rows of a query
// (see [rows]) and for one in the close of rows by Close, at Error, with its value as "panic" and
// the stack where it came as "stack". Each event has the prefix of the database as "db.prefix", and
// one of a failure has it as "error". The event "open" has the bound of the pool that the instance opens with as "pool.max_open_conns", 0
// for none, as [database/sql.DBStats.MaxOpenConnections] has it.
type connector struct {
	host   host.Host
	db     *core.DB
	engine *engine.Engine
	driver driver.Driver
	log    *slog.Logger
	// closeTimeout bounds Close.
	closeTimeout time.Duration
	// store that the connector made, and so closes after the instance, or nil for a store of
	// someone else.
	store io.Closer
	// stopSeen logs the first answer of a stop.
	stopSeen sync.Once
	// queries counts the queries whose rows were read, for the names of their tasks.
	queries atomic.Uint64

	mu sync.Mutex
	// closed is set when Close starts.
	closed bool
	// open are the rows of the queries that are not closed, which Close closes.
	open map[*rows]struct{}
}

// errClosed is the error of a call on an instance that was closed: a stop whose cause is the close.
var errClosed = fmt.Errorf("%w: %w", core.ErrStopped, core.ErrClosed)

var (
	_ driver.Connector = (*connector)(nil)
	_ io.Closer        = (*connector)(nil)
)

// newConnector of a new instance of the database under the prefix of the options o in the store s,
// which runs on the host h, with the cache in the cache directory of o, for the driver d. Of o it
// uses the prefix, the cache directory, the timeout of a close, the logger, and the endpoint, the
// bucket, and the prefix for the identity of the database; co are the options of the storage core,
// whose logger and identity it sets. maxOpen is the bound of the pool that the instance opens with,
// 0 for none, which it logs. It fails only on local problems.
func newConnector(ctx context.Context, h host.Host, s store.Store, o Options, co core.Options, d driver.Driver, maxOpen int) (*connector, error) {
	if o.CacheDir == "" {
		return nil, errors.New("s4ql: open: the options name no cache directory")
	}
	if o.CloseTimeout < 0 {
		return nil, fmt.Errorf("s4ql: open: the close timeout %v is negative", o.CloseTimeout)
	}
	if o.CloseTimeout == 0 {
		o.CloseTimeout = 10 * time.Second
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	if err := os.MkdirAll(o.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("s4ql: open: the cache directory: %w", err)
	}
	path := filepath.Join(o.CacheDir, "cache.db")
	co.Logger = o.Logger
	co.Identity = o.identity()
	db, err := core.Open(ctx, h, s, path, o.Prefix, co)
	if err != nil {
		return nil, fmt.Errorf("s4ql: %w", err)
	}
	log = log.With("db.prefix", o.Prefix)
	log.InfoContext(ctx, "open", "cache.path", path, "db.identity", co.Identity, "pool.max_open_conns", maxOpen)
	return &connector{
		host:         h,
		db:           db,
		engine:       engine.New(db, h, engine.Options{Logger: o.Logger}),
		driver:       d,
		log:          log,
		closeTimeout: o.CloseTimeout,
		open:         map[*rows]struct{}{},
	}, nil
}

// ownedStore is a store that the connector that is made on it owns.
type ownedStore interface {
	store.Store
	io.Closer
}

// ownedConnector is [newConnector] on a store that the connector owns: the close of the connector
// closes s after the instance, and s is closed at once if the connector cannot be made.
func ownedConnector(ctx context.Context, h host.Host, s ownedStore, o Options, co core.Options, d driver.Driver, maxOpen int) (*connector, error) {
	c, err := newConnector(ctx, h, s, o, co, d, maxOpen)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	c.store = s
	return c, nil
}

// Connect gives a connection, which runs its statements on the one instance, through a connection
// of the engine of its own. It never fails, opens nothing, and does not wait for the instance to
// serve, which a ping does.
func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	turn := c.host.NewSemaphore("turn of the connection", 1)
	// The permit is free, so the wait cannot fail.
	_ = turn.Acquire(context.WithoutCancel(ctx))
	return &conn{c: c, e: c.engine.Conn(), turn: turn}, nil
}

// Driver of the connector, which opens a further instance of the same database for each connector.
func (c *connector) Driver() driver.Driver {
	return c.driver
}

// Close the instance, and release the lease, within the close timeout. It fails if the instance did
// not close in that time or if the lease is not released, which then runs out by itself.
//
// It first closes the rows that are open, but for the rows of a RETURNING outside a transaction,
// which hold nothing and are not closed. The loop over the rows of a query is a call on the
// instance, which the close of the instance waits for. The next row of such rows is then the error
// of an instance that was closed. A panic of a loop that it ends is logged and not raised, so that
// the close goes on. So is a panic of the close of rows itself, which then fails as an internal
// error with the value of the panic, unless the close of the instance fails, whose error it then
// gives.
//
// The close of the instance closes the reader of every connection, also of one that database/sql
// has not closed yet, such as one that an application holds. The close of such a connection then
// finds its reader closed.
//
// Last, it closes the store that it made, which closes the connections to the store once the last
// request of the instance has ended, also after a close that failed.
func (c *connector) Close() error {
	c.mu.Lock()
	c.closed = true
	open := make([]*rows, 0, len(c.open))
	for r := range c.open {
		open = append(open, r)
	}
	c.mu.Unlock()
	// database/sql gives no context here.
	var panicked error
	for _, r := range open {
		if err := c.shut(context.Background(), r); err != nil && panicked == nil {
			panicked = err
		}
	}

	ctx, cancel := c.host.WithTimeout(context.Background(), c.closeTimeout)
	defer cancel()
	err := c.db.Close(ctx)
	if c.store != nil {
		// The close of the store does not wait for a request that the instance left in flight, and
		// never fails.
		_ = c.store.Close()
	}
	if err != nil {
		c.log.WarnContext(ctx, "close", "error", err)
		return publicError(err)
	}
	c.log.InfoContext(ctx, "close")
	return publicError(panicked)
}

// shut the rows r with [rows.shut], with the context ctx, and recover a panic of their close, which
// it logs as the event "panic" and gives as an internal error, so that the close goes on.
func (c *connector) shut(ctx context.Context, r *rows) (err error) {
	defer func() {
		if v := recover(); v != nil {
			c.log.ErrorContext(ctx, "panic", "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			err = fmt.Errorf("%w: the query panicked: %v", engine.ErrInternal, v)
		}
	}()
	r.shut()
	return nil
}

// hold the rows r until they are closed, and report whether the connector is open. The rows of a
// connector that is closed are not held, and their query fails: a query that comes after the close
// has taken the rows that it closes, and before the instance has stopped, would hold a read that
// the close of the instance then waits for.
func (c *connector) hold(r *rows) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.open[r] = struct{}{}
	return true
}

// release the rows r, which are closed.
func (c *connector) release(r *rows) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.open, r)
}

// public error for err, an error of the engine or of the core, or nil: see [publicError].
func (c *connector) public(ctx context.Context, err error) error {
	err = publicError(err)
	// An instance that was closed has stopped too, which is nothing to report.
	if errors.Is(err, ErrStopped) && !errors.Is(err, core.ErrClosed) {
		c.stopSeen.Do(func() {
			c.log.WarnContext(ctx, "stop_seen", "error", err)
		})
	}
	return err
}

// conn is a connection: a connection of the engine and a count of changes, on the instance of its
// connector. It is not safe for concurrent use, as [driver.Conn] says.
//
// The connection of the engine owns the reader of the connection: a connection to the cache, on
// which the read transactions of the connection run, one at a time. It is opened for the first
// statement of the connection that reads, and closed with the connection, when database/sql closes
// it. So the connections that database/sql keeps open are the reads that run at once, and the
// settings of its pool are the settings of the readers.
type conn struct {
	c *connector
	// e is the connection of the engine that the statements run on.
	e *engine.Conn
	// changes is SQLite's count of changes on the connection: what the last statement that set it
	// set it to.
	changes int64
	// tx is the transaction that is open on the connection, or nil; see [conn.BeginTx].
	tx *tx

	// The connection of the engine is not safe for concurrent use. database/sql makes one call of
	// the connection and its rows at a time, but the close of the connector closes rows from a
	// goroutine of its own. So a call of the connection that reaches the engine holds it from
	// [conn.enter] to [conn.leave], and so does the close of rows by the connector in [rows.shut].
	// mu guards busy, which is set while one of them holds the connection, waiting, which a call
	// sets while it waits for the close of rows to end, and shut, the rows that the close of the
	// connector found open while a call held the connection, which that call closes when it ends,
	// so that the close of the connector never waits for a call. turn hands the connection to a call
	// that waits; its one permit is taken when the connection is made.
	mu            sync.Mutex
	busy, waiting bool
	shut          []*rows
	turn          host.Semaphore
	// held counts the rows of the connection that are not closed, and closeLater is set when the
	// close of the connection was left to the close of the last of them; see [conn.Close]. mu
	// guards both.
	held       int
	closeLater bool
}

// enter the connection of the engine for a call of the connection, with the context ctx of the
// call, once the close of rows of the connection by the connector, if one runs, has ended.
func (c *conn) enter(ctx context.Context) {
	c.mu.Lock()
	if !c.busy {
		c.busy = true
		c.mu.Unlock()
		return
	}
	c.waiting = true
	c.mu.Unlock()
	// The wait cannot fail: the context cannot end.
	_ = c.turn.Acquire(context.WithoutCancel(ctx))
}

// leave the connection of the engine after a call, with the context ctx of the call, and first
// close the rows that the close of the connector found open meanwhile.
func (c *conn) leave(ctx context.Context) {
	for {
		c.mu.Lock()
		shut := c.shut
		c.shut = nil
		if len(shut) == 0 {
			c.freeLocked(ctx)
			return
		}
		c.mu.Unlock()
		c.closeShut(ctx, shut)
	}
}

// closeShut closes the rows shut, which the close of the connector left to a call, with the context
// ctx of the call. If a close panics, the panic goes on, and the connection of the engine is freed
// first, with the rows after those that panicked left to the next call, so that neither waits for
// the connection for good.
func (c *conn) closeShut(ctx context.Context, shut []*rows) {
	i := 0
	defer func() {
		if i == len(shut) {
			return
		}
		c.mu.Lock()
		c.shut = append(c.shut, shut[i+1:]...)
		c.freeLocked(ctx)
	}()
	for ; i < len(shut); i++ {
		func(r *rows) {
			r.mu.Lock()
			defer r.mu.Unlock()
			// As in [rows.shut], the error is dropped.
			_ = r.close()
		}(shut[i])
	}
}

// freeLocked frees the connection of the engine, with mu held, which it unlocks: it hands it to a
// call that waits, or else leaves it free.
func (c *conn) freeLocked(ctx context.Context) {
	if c.waiting {
		c.waiting = false
		c.mu.Unlock()
		c.turn.Release(ctx)
		return
	}
	c.busy = false
	c.mu.Unlock()
}

// release one of the rows of the connection, which are closed, with the context ctx of their query,
// and close the connection if its close was left to them and they were the last; see [conn.Close].
// It gives the error of that close.
func (c *conn) release(ctx context.Context) error {
	c.mu.Lock()
	c.held--
	later := c.closeLater && c.held == 0
	if later {
		c.closeLater = false
	}
	c.mu.Unlock()
	if !later {
		return nil
	}
	// database/sql closed the connection while these rows were read, and has let go of it. So
	// nothing else uses it, and the error of its close, such as that of the roll back of its
	// transaction, is the rows', unless they have one.
	return c.e.Close(context.WithoutCancel(ctx))
}

var (
	_ driver.Conn               = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.Pinger             = (*conn)(nil)
	_ driver.NamedValueChecker  = (*conn)(nil)
	_ driver.SessionResetter    = (*conn)(nil)
	_ driver.Validator          = (*conn)(nil)
)

// Ping waits until the instance serves, or until ctx ends. It fails with the stop of an instance
// that has stopped.
func (c *conn) Ping(ctx context.Context) error {
	return c.c.public(ctx, c.c.db.Wait(ctx))
}

// ExecContext runs the statements of the text query one after another, with args for their
// parameters, by position, as [engine.Conn.ExecAll] runs them: each statement takes as many of the
// arguments as it has parameters, in order, and runs as a transaction of its own, or in the
// transaction of the connection, if one is open. A statement that fails ends the call with its
// error, and keeps those before it. What comes before any statement runs is judged first, as
// [engine.CheckAll] judges it, then whether each argument is one that s4ql supports, as [arguments]
// says, and then the instance runs the statements; an argument that s4ql does not support runs none
// of them. The result is SQLite's count of changes on the connection after the last statement that
// ran.
func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.exec(ctx, query, args, engine.CheckAll, c.e.ExecAll)
}

// exec the text query with args: judge it with judge, as [arguments] says, and run it with run.
func (c *conn) exec(ctx context.Context, query string, args []driver.NamedValue, judge func(string, int) error,
	run func(context.Context, string, ...*s4qlpb.Value) (engine.Result, error)) (driver.Result, error) {
	values, err := arguments(query, args, judge)
	if err != nil {
		return nil, err
	}
	c.enter(ctx)
	defer c.leave(ctx)
	res, err := run(ctx, query, values...)
	// The engine gives the result with an error too: an insert that fails sets the count to 0.
	c.count(res)
	if err != nil {
		return nil, c.c.public(ctx, err)
	}
	return result{changes: c.changes}, nil
}

// QueryContext runs the statement of the text query, with args for its parameters, and gives its
// rows. The text and the arguments are judged as [conn.ExecContext] judges a text of one statement,
// and a text of several statements is not supported, as a second statement. A statement that is no
// query gives no columns and no rows. Outside a transaction, a statement with RETURNING gives its
// rows once it is committed, and without an error from then on.
func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	values, err := arguments(query, args, engine.Check)
	if err != nil {
		return nil, err
	}
	c.enter(ctx)
	defer c.leave(ctx)
	r, err := c.e.Query(ctx, query, values...)
	if r != nil {
		c.count(r.Result())
	}
	if err != nil {
		return nil, c.c.public(ctx, err)
	}
	if len(r.Columns()) == 0 {
		// A statement that is no query has run to its end, and holds nothing.
		return noRows{}, nil
	}
	held := &rows{ctx: ctx, c: c.c, conn: c, rows: r}
	// The rows of a RETURNING outside a transaction hold nothing, and their statement is committed,
	// so they are given also when the connector has closed meanwhile, which does not close them.
	if r.Holds() && !c.c.hold(held) {
		_ = r.Close()
		return nil, c.c.public(ctx, errClosed)
	}
	c.mu.Lock()
	c.held++
	c.mu.Unlock()
	return held, nil
}

// count the changes of a statement with the result res, if it sets the count.
func (c *conn) count(res engine.Result) {
	if res.SetsChanges {
		c.changes = res.Changes
	}
}

// PrepareContext checks the text query, and gives a statement that runs it. It fails with SQLite's
// syntax error and with "not supported", which the text alone decides: the check reads no table,
// and does not need the instance to serve. A text of several statements is not supported, as a
// second statement: a prepared statement holds one.
func (c *conn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	return c.Prepare(query)
}

// Prepare is [conn.PrepareContext] without a context, which it does not need.
func (c *conn) Prepare(query string) (driver.Stmt, error) {
	if err := check(query); err != nil {
		return nil, err
	}
	return &prepared{conn: c, query: query}, nil
}

// check the text query with [stmt.Check], and then as the engine checks it: SQLite's syntax error,
// "not supported", also for a statement that the pass supports and the engine does not run, or nil
// for a statement that s4ql supports. The text alone decides, so the check reads no table, and does
// not need the instance to serve.
func check(query string) error {
	s, err := stmt.Check(query)
	if err == nil && s == nil {
		err = engine.ErrNoStatement
	}
	if err == nil {
		// With as many arguments as the statement has parameters, only the text can fail the check.
		err = engine.Check(query, s.Parameters())
	}
	return publicError(err)
}

// arguments of the text query: the value of each of args, in order, as [argument] gives it, after
// the conversion of [conn.CheckNamedValue]. judge gives the errors of the text with a number of
// arguments that the engine gives, when it runs the text, before it runs any statement of it, such
// as [engine.Check] and [engine.CheckAll].
//
// The errors come in this order, the first that holds, so that such an error of the text never
// depends on the arguments: the error that judge gives, of the text and of the number of arguments;
// and "not supported" for the first argument with a name or of a type that no column holds, which
// says which type. The engine makes the checks of judge again when it runs the text, so they are
// made here only for arguments that are not supported, and the text is parsed once on the path where
// every argument is. The engine then refuses an argument whose value no column holds, such as a NaN
// or text with the character NUL, and, once it has found the table, a value of another type than
// its column.
func arguments(query string, args []driver.NamedValue, judge func(string, int) error) ([]*s4qlpb.Value, error) {
	values := make([]*s4qlpb.Value, 0, len(args))
	for _, a := range args {
		v, err := argument(a)
		if err != nil {
			if err := publicError(judge(query, len(args))); err != nil {
				return nil, err
			}
			return nil, err
		}
		values = append(values, v)
	}
	return values, nil
}

// argument a as a value: an int64 as an integer, a float64 as a real, a string as text, a []byte
// that is not nil as a blob, which an empty one is too, and nil as NULL. nil is what
// [conn.CheckNamedValue] gives for a nil pointer and for a [driver.Valuer] that gives nil, such as
// a [database/sql.NullString] that is not valid. A blob is a copy of the bytes. It gives "not supported" for an argument with a
// name, for a []byte that is nil, which does not say whether it means NULL or the empty blob, and
// for one of another type, such as a bool or a time.Time. The message names the argument by its
// position, and the type, never the value.
func argument(a driver.NamedValue) (*s4qlpb.Value, error) {
	if a.Name != "" {
		return nil, notSupported(fmt.Sprintf("the named argument %q", a.Name))
	}
	switch v := a.Value.(type) {
	case int64:
		return s4qlpb.Value_builder{Integer: new(v)}.Build(), nil
	case float64:
		return s4qlpb.Value_builder{Real: new(v)}.Build(), nil
	case string:
		return s4qlpb.Value_builder{Text: new(v)}.Build(), nil
	case []byte:
		if v == nil {
			return nil, notSupported(fmt.Sprintf("argument %v of type []byte, which is nil", a.Ordinal))
		}
		// The bytes are the caller's again once the call returns, and the engine can read the value
		// later, such as the rows of a query or of a RETURNING when they are read, so the value
		// holds a copy.
		return s4qlpb.Value_builder{Blob: bytes.Clone(v)}.Build(), nil
	case nil:
		return s4qlpb.Value_builder{Null: &s4qlpb.Null{}}.Build(), nil
	case unconverted:
		// The error of the conversion is the cause, and not in the message: the error of a
		// [driver.Valuer] is the application's, and can hold a value.
		what := fmt.Sprintf("argument %v of type %T, which database/sql cannot convert", a.Ordinal, v.value)
		if v.err == errOverflow {
			what = fmt.Sprintf("argument %v of type %T, whose value does not fit a signed 64-bit integer", a.Ordinal, v.value)
		}
		return nil, &failure{is: ErrNotSupported, message: "s4ql: not supported: " + what, cause: v.err}
	default:
		return nil, notSupported(fmt.Sprintf("argument %v of type %T", a.Ordinal, v))
	}
}

// unconverted is an argument that database/sql's default conversion refused, with its error, which
// [argument] reports after the text is judged, as the cause of its "not supported".
type unconverted struct {
	value any
	err   error
}

// errOverflow is the error of an unsigned integer above the largest int64.
var errOverflow = errors.New("s4ql: an unsigned integer that does not fit a signed 64-bit integer")

// overflows reports whether v is an unsigned integer above the largest int64, or a pointer to one,
// which [driver.DefaultParameterConverter] would turn into a negative int64 for every unsigned kind
// but uint64. A [driver.Valuer] gives its own value, and is not one.
func overflows(v any) bool {
	for {
		if _, ok := v.(driver.Valuer); ok {
			return false
		}
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.Pointer:
			if rv.IsNil() {
				return false
			}
			v = rv.Elem().Interface()
		case reflect.Uint, reflect.Uint64, reflect.Uintptr:
			return rv.Uint() > math.MaxInt64
		default:
			return false
		}
	}
}

// CheckNamedValue converts the argument nv as database/sql converts one for a driver without this
// method, with [driver.DefaultParameterConverter]: an integer of any size and a type whose kind is an
// integer become an int64, a float32 and a type whose kind is a float a float64, a type whose kind
// is a string a string, a type whose kind is a slice of bytes a []byte, a [driver.Valuer] gives its
// value, and a pointer gives what it points to. It never fails, because database/sql gives its error before the driver judges the text. An
// argument that the conversion refuses is kept with the error, which [arguments] gives after the
// text, as the cause of a "not supported". So is an unsigned integer above the largest int64, which
// the conversion takes for a negative int64 when its type is not uint64, as uint(1<<63) is.
func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	if overflows(nv.Value) {
		nv.Value = unconverted{value: nv.Value, err: errOverflow}
		return nil
	}
	v, err := driver.DefaultParameterConverter.ConvertValue(nv.Value)
	if err != nil {
		nv.Value = unconverted{value: nv.Value, err: err}
		return nil
	}
	nv.Value = v
	return nil
}

// Close the connection: its connection of the engine, and with it its reader, if it has opened
// one, and its transaction, if one is open, which the close rolls back.
//
// database/sql closes a connection after the rows of its queries are closed. Through the driver's
// own interfaces, rows of the connection that a loop reads can still be open, which the engine
// does not close the connection beside: they hold the scan of their query, and the end of their
// transaction, if it ended while they were read. Then the close is left to the close of the last of
// those rows, and Close returns nil; see [rows.close].
func (c *conn) Close() error {
	// database/sql gives no context here.
	ctx := context.Background()
	c.enter(ctx)
	defer c.leave(ctx)
	err := c.e.Close(ctx)
	if errors.Is(err, core.ErrReaderInUse) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.held > 0 {
			c.closeLater = true
			return nil
		}
	}
	return c.c.public(ctx, err)
}

// prepared is a prepared statement: a text that was checked, and that each run checks again, with
// its arguments, and runs.
type prepared struct {
	conn  *conn
	query string
}

var (
	_ driver.Stmt             = (*prepared)(nil)
	_ driver.StmtExecContext  = (*prepared)(nil)
	_ driver.StmtQueryContext = (*prepared)(nil)
)

// NumInput is -1, so that database/sql does not count the arguments itself: the driver counts them,
// with the same error for a prepared statement as for a direct call.
func (p *prepared) NumInput() int {
	return -1
}

// ExecContext runs the statement with args, as [conn.ExecContext] runs a text of one statement.
func (p *prepared) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return p.conn.exec(ctx, p.query, args, engine.Check, p.conn.e.Exec)
}

// QueryContext runs the statement with args, and gives its rows, as [conn.QueryContext] runs its text.
func (p *prepared) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return p.conn.QueryContext(ctx, p.query, args)
}

// Exec is [prepared.ExecContext] without a context.
func (p *prepared) Exec(args []driver.Value) (driver.Result, error) {
	return p.ExecContext(context.Background(), named(args))
}

// Query is [prepared.QueryContext] without a context.
func (p *prepared) Query(args []driver.Value) (driver.Rows, error) {
	return p.QueryContext(context.Background(), named(args))
}

// named arguments of args, by position, without names.
func named(args []driver.Value) []driver.NamedValue {
	nv := make([]driver.NamedValue, len(args))
	for i, v := range args {
		nv[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return nv
}

// Close does nothing: a prepared statement holds nothing.
func (p *prepared) Close() error {
	return nil
}

// result of a statement: the count of changes of its connection after it.
type result struct {
	changes int64
}

// LastInsertId is always 0: every table is WITHOUT ROWID, and SQLite records no insert into such a
// table.
func (r result) LastInsertId() (int64, error) {
	return 0, nil
}

// RowsAffected is SQLite's count of changes on the connection after the statement.
func (r result) RowsAffected() (int64, error) {
	return r.changes, nil
}
