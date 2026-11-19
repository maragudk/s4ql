package s4ql

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/store"
)

// connector of one instance of a database: every connection that it gives runs its statements on
// that instance. Its Close closes the instance.
//
// It logs what the driver decides itself, through the logger of the options: the event "open" when
// the instance is opened, at Info; "close" when it is closed, at Info, and at Warn if the close
// failed, which includes a lease that is not released; "stop_seen" the first time that it answers
// a call with [ErrStopped], at Warn; and "panic" for a panic in the loop over the rows of a query,
// at Error, with its value as "panic" and the stack where it came as "stack" (see [rows]). Each
// event has the prefix of the database as "db.prefix", and one of a failure has it as "error".
type connector struct {
	host   host.Host
	db     *core.DB
	engine *engine.Engine
	driver driver.Driver
	log    *slog.Logger
	// closeTimeout bounds Close.
	closeTimeout time.Duration
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
// uses the prefix, the cache directory, the timeout of a close, and the logger; co are the options
// of the storage core, whose logger it sets. It fails only on local problems.
func newConnector(ctx context.Context, h host.Host, s store.Store, o Options, co core.Options, d driver.Driver) (*connector, error) {
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
	db, err := core.Open(ctx, h, s, path, o.Prefix, co)
	if err != nil {
		return nil, fmt.Errorf("s4ql: %w", err)
	}
	log = log.With("db.prefix", o.Prefix)
	log.InfoContext(ctx, "open", "cache.path", path)
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

// Connect gives a connection, which is no more than a count of changes of its own: every
// connection runs its statements on the one instance. It never fails, and does not wait for the
// instance to serve, which a ping does.
func (c *connector) Connect(context.Context) (driver.Conn, error) {
	return &conn{c: c}, nil
}

// Driver of the connector, which opens a further instance of the same database for each connector.
func (c *connector) Driver() driver.Driver {
	return c.driver
}

// Close the instance, and release the lease, within the close timeout. It fails if the instance did
// not close in that time or if the lease is not released, which then runs out by itself.
//
// It first closes the rows that are open. The loop over the rows of a query is a call on the
// instance, which the close of the instance waits for. The next row of such rows is then the error
// of an instance that was closed. A panic of a loop that it ends is logged and not raised, so that
// the close goes on.
func (c *connector) Close() error {
	c.mu.Lock()
	c.closed = true
	open := make([]*rows, 0, len(c.open))
	for r := range c.open {
		open = append(open, r)
	}
	c.mu.Unlock()
	for _, r := range open {
		r.shut()
	}

	// database/sql gives no context here.
	ctx, cancel := c.host.WithTimeout(context.Background(), c.closeTimeout)
	defer cancel()
	if err := c.db.Close(ctx); err != nil {
		c.log.WarnContext(ctx, "close", "error", err)
		return publicError(err)
	}
	c.log.InfoContext(ctx, "close")
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

// conn is a connection: a count of changes, on the instance of its connector. It is not safe for
// concurrent use, as [driver.Conn] says.
type conn struct {
	c *connector
	// changes is SQLite's count of changes on the connection: what the last statement that set it
	// set it to.
	changes int64
}

var (
	_ driver.Conn               = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.Pinger             = (*conn)(nil)
	_ driver.NamedValueChecker  = (*conn)(nil)
)

// Ping waits until the instance serves, or until ctx ends. It fails with the stop of an instance
// that has stopped.
func (c *conn) Ping(ctx context.Context) error {
	return c.c.public(ctx, c.c.db.Wait(ctx))
}

// ExecContext runs the statement of the text query. It is not supported with arguments.
func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if len(args) > 0 {
		return nil, notSupported("parameters")
	}
	res, err := c.c.engine.Exec(ctx, query)
	// The engine gives the result with an error too: an insert that fails sets the count to 0.
	c.count(res)
	if err != nil {
		return nil, c.c.public(ctx, err)
	}
	return result{changes: c.changes}, nil
}

// QueryContext runs the statement of the text query, and gives its rows. It is not supported with
// arguments. A statement that is no query gives no columns and no rows.
func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if len(args) > 0 {
		return nil, notSupported("parameters")
	}
	r, err := c.c.engine.Query(ctx, query)
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
	held := &rows{ctx: ctx, c: c.c, rows: r}
	if !c.c.hold(held) {
		_ = r.Close()
		return nil, c.c.public(ctx, errClosed)
	}
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
// and does not need the instance to serve.
func (c *conn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	return c.Prepare(query)
}

// Prepare is [conn.PrepareContext] without a context, which it does not need.
func (c *conn) Prepare(query string) (driver.Stmt, error) {
	s, err := stmt.Check(query)
	if err == nil && s == nil {
		err = engine.ErrNoStatement
	}
	if err != nil {
		return nil, publicError(err)
	}
	return &prepared{conn: c, query: query}, nil
}

// BeginTx is not supported: each statement is a transaction of its own.
func (c *conn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return nil, notSupported("transactions")
}

// Begin is not supported, as [conn.BeginTx] is not.
func (c *conn) Begin() (driver.Tx, error) {
	return nil, notSupported("transactions")
}

// CheckNamedValue takes every argument as it is, so that a statement with an argument of any type
// is answered with "not supported", and not with an error about the type of the argument.
func (c *conn) CheckNamedValue(*driver.NamedValue) error {
	return nil
}

// Close does nothing: a connection holds nothing. The rows of its queries hold their reads.
func (c *conn) Close() error {
	return nil
}

// prepared is a prepared statement: a text that was checked, and that each run checks again and
// runs.
type prepared struct {
	conn  *conn
	query string
}

var (
	_ driver.Stmt             = (*prepared)(nil)
	_ driver.StmtExecContext  = (*prepared)(nil)
	_ driver.StmtQueryContext = (*prepared)(nil)
)

// NumInput is -1, so that database/sql does not count the arguments, and the driver answers any
// argument with "not supported".
func (p *prepared) NumInput() int {
	return -1
}

func (p *prepared) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return p.conn.ExecContext(ctx, p.query, args)
}

func (p *prepared) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return p.conn.QueryContext(ctx, p.query, args)
}

// Exec is [prepared.ExecContext] without a context.
func (p *prepared) Exec(args []driver.Value) (driver.Result, error) {
	return p.ExecContext(context.Background(), make([]driver.NamedValue, len(args)))
}

// Query is [prepared.QueryContext] without a context.
func (p *prepared) Query(args []driver.Value) (driver.Rows, error) {
	return p.QueryContext(context.Background(), make([]driver.NamedValue, len(args)))
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
