package s4ql_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql"
	"maragu.dev/s4ql/internal/sim"
)

// three rows in t1 on a new instance.
func three(t *testing.T) (*world, *sql.DB) {
	t.Helper()
	w := newWorld(t)
	db := w.serve(t, "a")
	exec(t, db, t1, insert1, "insert into t1 (v1) values (2)", "insert into t1 (v1) values (3)")
	return w, db
}

// held opens [readers] queries on db, each with the context that ctx gives it, and reads one row of
// each, so that each holds a connection of the pool and the read of that connection.
func held(t *testing.T, db *sql.DB, ctx func() context.Context) []*sql.Rows {
	t.Helper()
	var all []*sql.Rows
	for range readers {
		rows, err := db.QueryContext(ctx(), select1)
		is.NotError(t, err)
		is.True(t, rows.Next())
		all = append(all, rows)
	}
	return all
}

func TestRows(t *testing.T) {
	t.Run("should free the read when the rows are closed early", func(t *testing.T) {
		_, db := three(t)
		for _, rows := range held(t, db, t.Context) {
			is.NotError(t, rows.Close())
			is.NotError(t, rows.Err())
			is.NotError(t, rows.Close())
		}
		readersFree(t, db)

		// Rows that are closed before their first row.
		for range readers {
			rows, err := db.QueryContext(t.Context(), select1)
			is.NotError(t, err)
			is.NotError(t, rows.Close())
		}
		readersFree(t, db)
	})

	t.Run("should free the read when the rows are read to the end, without a close", func(t *testing.T) {
		_, db := three(t)
		for _, rows := range held(t, db, t.Context) {
			n := 1
			for rows.Next() {
				n++
			}
			is.Equal(t, 3, n)
			is.NotError(t, rows.Err())
		}
		readersFree(t, db)
	})

	t.Run("should free the read when the context of the query ends, without a close", func(t *testing.T) {
		_, db := three(t)
		ctx, cancel := context.WithCancel(t.Context())
		all := held(t, db, func() context.Context { return ctx })
		cancel()
		// database/sql closes the rows by itself, some time after the context has ended. The check
		// waits for the reads, so a slower machine cannot make it fail.
		readersFree(t, db)
		for _, rows := range all {
			for rows.Next() {
			}
			is.Error(t, context.Canceled, failed(t, rows.Err()))
		}
	})

	t.Run("should run as many reads at once as the pool has connections, and make a further statement wait until a connection is free or its context ends", func(t *testing.T) {
		_, db := three(t)
		db.SetMaxOpenConns(readers)
		all := held(t, db, t.Context)
		is.Equal(t, readers, db.Stats().InUse)

		// A slower machine ends the context all the same.
		for _, run := range []func(ctx context.Context) error{
			func(ctx context.Context) error { _, err := db.QueryContext(ctx, select1); return err },
			// A query that is run to its end by Exec needs a connection too, and so does a write, which
			// is then not run.
			func(ctx context.Context) error { _, err := db.ExecContext(ctx, select1); return err },
			func(ctx context.Context) error {
				_, err := db.ExecContext(ctx, "insert into t1 (v1) values (9)")
				return err
			},
		} {
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			err := run(ctx)
			cancel()
			is.Error(t, context.DeadlineExceeded, failed(t, err))
		}
		is.Equal(t, readers, db.Stats().OpenConnections)

		// A query that waits gets the connection that is freed.
		type answer struct {
			values []int64
			err    error
		}
		got := make(chan answer, 1)
		go func() {
			var a answer
			a.values, a.err = query(t.Context(), db, select1)
			got <- a
		}()
		is.NotError(t, all[0].Close())
		a := <-got
		is.NotError(t, a.err)
		is.EqualSlice(t, []int64{1, 2, 3}, a.values)

		// A write commits on that connection, and the next query on it sees the write.
		exec(t, db, "insert into t1 (v1) values (4)")
		is.EqualSlice(t, []int64{1, 2, 3, 4}, values(t, db, select1))

		// The reads that are still held see the state of their start.
		for _, rows := range all[1:] {
			var rest []int64
			for rows.Next() {
				var v int64
				is.NotError(t, rows.Scan(&v))
				rest = append(rest, v)
			}
			is.NotError(t, rows.Err())
			is.EqualSlice(t, []int64{2, 3}, rest)
		}
		readersFree(t, db)
	})

	t.Run("should read and close rows from a goroutine that is locked to its thread, and from others", func(t *testing.T) {
		_, db := three(t)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		// database/sql closes the rows from a goroutine of its own when the context ends.
		ctx, cancel := context.WithCancel(t.Context())
		rows, err := db.QueryContext(ctx, select1)
		is.NotError(t, err)
		is.True(t, rows.Next())
		cancel()
		readersFree(t, db)

		// Rows that another goroutine has started to read.
		started := make(chan *sql.Rows, 1)
		go func() {
			rows, err := db.QueryContext(t.Context(), select1)
			if err == nil {
				rows.Next()
			}
			started <- rows
		}()
		rows = <-started
		is.True(t, rows != nil)
		is.True(t, rows.Next())
		is.NotError(t, rows.Close())

		// Rows that the close of the database closes, from this goroutine.
		held(t, db, t.Context)
		is.NotError(t, db.Close())
	})

	t.Run("should refuse a statement that reads on a connection whose rows are open as not supported, at once, and run it after the rows are closed", func(t *testing.T) {
		_, db := three(t)
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, conn.Close()) }()
		prepared, err := conn.PrepareContext(t.Context(), select1)
		is.NotError(t, err)
		first, err := conn.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		// The rows are closed before the connection also when the test fails, or the close of the
		// connection would wait for them.
		defer func() { _ = first.Close() }()
		is.True(t, first.Next())

		const want = "a statement that reads on a connection whose rows of an earlier query are open"
		_, err = conn.QueryContext(t.Context(), select1)
		is.Equal(t, want, notSupported(t, err))
		_, err = conn.ExecContext(t.Context(), select1)
		is.Equal(t, want, notSupported(t, err))
		_, err = prepared.QueryContext(t.Context())
		is.Equal(t, want, notSupported(t, err))
		is.Equal(t, want, notSupported(t, conn.QueryRowContext(t.Context(), select1).Scan(new(int64))))

		// A write needs no read, and the rows that are open do not see it. Another connection is not
		// refused.
		_, err = conn.ExecContext(t.Context(), "insert into t1 (v1) values (4)")
		is.NotError(t, err)
		is.EqualSlice(t, []int64{1, 2, 3, 4}, values(t, db, select1))
		rest := []int64{1}
		for first.Next() {
			var v int64
			is.NotError(t, first.Scan(&v))
			rest = append(rest, v)
		}
		is.NotError(t, first.Err())
		is.EqualSlice(t, []int64{1, 2, 3}, rest)

		// The rows have ended, and the connection reads again.
		second, err := conn.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		var n int
		for second.Next() {
			n++
		}
		is.NotError(t, second.Err())
		is.Equal(t, 4, n)
		is.NotError(t, prepared.Close())
	})

	t.Run("should close the rows that are left open when the database is closed, and fail them", func(t *testing.T) {
		w, db := three(t)
		all := held(t, db, t.Context)
		unread, err := db.QueryContext(t.Context(), "insert into t1 (v1) values (4)")
		is.NotError(t, err)
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)

		// The instance cannot close while a read is held, so the close would fail after its timeout
		// if the rows were left.
		is.NotError(t, db.Close())
		closed := func(err error) {
			t.Helper()
			is.Error(t, s4ql.ErrStopped, failed(t, err))
			is.Equal(t, "s4ql: the instance has stopped: closed", err.Error())
		}
		for _, rows := range all {
			is.True(t, !rows.Next())
			closed(rows.Err())
			is.NotError(t, rows.Close())
		}
		// The insert is committed, and its rows, which hold nothing, do not say otherwise.
		is.True(t, !unread.Next())
		is.NotError(t, unread.Err())
		is.NotError(t, unread.Close())

		// A connection that the application holds still reaches the instance, which is closed.
		_, err = conn.ExecContext(t.Context(), insert1)
		closed(err)
		_, err = conn.QueryContext(t.Context(), select1)
		closed(err)
		closed(conn.PingContext(t.Context()))
		is.NotError(t, conn.Close())

		// The lease is released, and the cache is closed, which the world checks when the test ends.
		is.EqualSlice(t, []int64{1, 2, 3, 4}, values(t, w.serve(t, "b"), select1))
	})

	t.Run("should raise a panic of the engine between two rows on the goroutine that calls Next, with the read freed, and go on serving", func(t *testing.T) {
		w := newWorld(t)
		h := &rowHost{Free: w.host}
		db, err := s4ql.OpenStore(t.Context(), h, w.client(), w.options("a"), coreOptions())
		is.NotError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, t1, insert1, "insert into t1 (v1) values (2)")

		rows, err := db.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		h.at(func() { panic(errPanic) })
		// The engine yields after it has given a row, so the first row comes, and the panic comes at
		// the second.
		ok, v := next(rows)
		is.True(t, ok && v == nil, fmt.Sprint(v))
		ok, v = next(rows)
		is.True(t, !ok)
		is.Equal(t, any(errPanic), v)
		h.at(nil)

		// The rows are closed, and say so.
		ok, v = next(rows)
		is.True(t, !ok && v == nil, fmt.Sprint(v))
		is.Equal(t, "s4ql: internal error: the query panicked", failed(t, rows.Err()).Error())
		is.NotError(t, rows.Close())

		// The read is free, the instance serves, and it closes in time and releases the lease.
		readersFree(t, db)
		exec(t, db, "insert into t1 (v1) values (3)")
		is.EqualSlice(t, []int64{1, 2, 3}, values(t, db, select1))
		is.NotError(t, db.Close())
		is.EqualSlice(t, []int64{1, 2, 3}, values(t, w.serve(t, "b"), select1))
	})

	t.Run("should give a panic at the end of the loop as an internal error of Close, and log it with its stack", func(t *testing.T) {
		w := newWorld(t)
		log := &panickyLog{}
		db := openLogged(t, w, w.host, log)
		exec(t, db, t1, insert1, "insert into t1 (v1) values (2)")

		// The engine logs the event of the query when the loop over its rows has ended, in the turn
		// that the close gives the task.
		rows, err := db.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		is.True(t, rows.Next())
		log.arm(true)
		v, err := closeRows(rows)
		log.arm(false)
		is.True(t, v == nil, fmt.Sprint(v))
		is.Equal(t, "s4ql: internal error: the query panicked: a panic of the test", failed(t, err).Error())
		panics := log.lines.of("panic")
		is.Equal(t, 1, len(panics))
		is.True(t, strings.HasPrefix(panics[0], `level=ERROR msg=panic db.prefix=db panic="a panic of the test" stack="goroutine `), panics[0])

		is.True(t, !rows.Next())
		is.NotError(t, rows.Close())
		readersFree(t, db)
		exec(t, db, "insert into t1 (v1) values (3)")
		is.NotError(t, db.Close())
		is.EqualSlice(t, []int64{1, 2, 3}, values(t, w.serve(t, "b"), select1))
	})

	t.Run("should not raise a panic at the end of the loop when database/sql closes the rows from its own goroutine, as the context of the query ends", func(t *testing.T) {
		w := newWorld(t)
		log := &panickyLog{}
		db := openLogged(t, w, w.host, log)
		exec(t, db, t1, insert1, "insert into t1 (v1) values (2)")

		ctx, cancel := context.WithCancel(t.Context())
		rows, err := db.QueryContext(ctx, select1)
		is.NotError(t, err)
		is.True(t, rows.Next())
		log.arm(true)
		cancel()
		// The task logs the panic before the close ends. The bound is far above what the close needs.
		waited, stop := context.WithTimeout(t.Context(), time.Minute)
		defer stop()
		for len(log.lines.of("panic")) == 0 && waited.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		log.arm(false)
		is.Equal(t, 1, len(log.lines.of("panic")))

		readersFree(t, db)
		is.True(t, !rows.Next())
		is.Error(t, context.Canceled, failed(t, rows.Err()))
		is.NotError(t, rows.Close())
		exec(t, db, "insert into t1 (v1) values (3)")
		is.EqualSlice(t, []int64{1, 2, 3}, values(t, db, select1))
	})

	t.Run("should count rows whose close panics as closed, so that their connection can go back to the pool", func(t *testing.T) {
		w := newWorld(t)
		log := &panickyLog{}
		db := openLogged(t, w, w.host, log)
		exec(t, db, t1, insert1)
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)

		is.NotError(t, conn.Raw(func(driverConn any) error {
			// No loop reads the rows, so their close logs the event of the query, and panics there.
			rows, err := driverConn.(driver.QueryerContext).QueryContext(t.Context(), select1, nil)
			is.NotError(t, err)
			is.True(t, !driverConn.(driver.Validator).IsValid())
			log.arm(true)
			v := func() (v any) {
				defer func() { v = recover() }()
				_ = rows.Close()
				return nil
			}()
			log.arm(false)
			is.Equal(t, any(errPanic), v)
			is.True(t, driverConn.(driver.Validator).IsValid())
			return nil
		}))
		is.NotError(t, conn.Close())
		is.EqualSlice(t, []int64{1}, values(t, db, select1))
	})

	t.Run("should not raise a panic at the end of the loop when the close of the database closes the rows, and log it", func(t *testing.T) {
		w := newWorld(t)
		log := &panickyLog{}
		db := openLogged(t, w, w.host, log)
		exec(t, db, t1, insert1, "insert into t1 (v1) values (2)")

		rows, err := db.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		is.True(t, rows.Next())
		log.arm(true)
		is.NotError(t, db.Close())
		log.arm(false)
		is.Equal(t, 1, len(log.lines.of("panic")))
		is.EqualSlice(t, []string{"level=INFO msg=close db.prefix=db"}, log.lines.of("close"))

		// No later call gives it.
		is.True(t, !rows.Next())
		is.Error(t, s4ql.ErrStopped, failed(t, rows.Err()))
		is.NotError(t, rows.Close())
		is.EqualSlice(t, []int64{1, 2}, values(t, w.serve(t, "b"), select1))
	})
}

// closeRows is [sql.Rows.Close] of rows, with the value of a panic in it, if it panicked.
func closeRows(rows *sql.Rows) (v any, err error) {
	defer func() { v = recover() }()
	return nil, rows.Close()
}

// panickyLog is a log in the text format of log/slog, whose handler panics with [errPanic] at the
// event of a statement while it is armed, and else calls a function of the test there while one is
// set.
type panickyLog struct {
	lines lines
	armed atomic.Bool
	on    atomic.Pointer[func()]
}

// arm the log, or disarm it.
func (p *panickyLog) arm(armed bool) {
	p.armed.Store(armed)
}

// at the event of a statement, call f from now on, or nothing if f is nil. f can set the next.
func (p *panickyLog) at(f func()) {
	if f == nil {
		p.on.Store(nil)
		return
	}
	p.on.Store(&f)
}

func (p *panickyLog) handler() slog.Handler {
	return &panickyHandler{log: p, Handler: slog.NewTextHandler(&p.lines, &slog.HandlerOptions{Level: slog.LevelDebug})}
}

type panickyHandler struct {
	log *panickyLog
	slog.Handler
}

func (h *panickyHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "statement" {
		if h.log.armed.Load() {
			panic(errPanic)
		}
		if f := h.log.on.Load(); f != nil {
			(*f)()
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h *panickyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &panickyHandler{log: h.log, Handler: h.Handler.WithAttrs(attrs)}
}

func (h *panickyHandler) WithGroup(name string) slog.Handler {
	return &panickyHandler{log: h.log, Handler: h.Handler.WithGroup(name)}
}

// errPanic is the value of the panics that the tests plant.
var errPanic = errors.New("a panic of the test")

// rowHost is the host of the world, which calls a function of the test at the yield between two
// rows of a query, while one is set.
type rowHost struct {
	*sim.Free
	between atomic.Pointer[func()]
}

// at the yield between two rows, call f from now on, or nothing if f is nil.
func (h *rowHost) at(f func()) {
	if f == nil {
		h.between.Store(nil)
		return
	}
	h.between.Store(&f)
}

func (h *rowHost) Yield(ctx context.Context, site string) {
	if f := h.between.Load(); f != nil && site == "engine: between two rows of a query" {
		(*f)()
	}
	h.Free.Yield(ctx, site)
}

// next is [sql.Rows.Next] of rows, and the value of a panic in it, if it panicked, as an application
// that recovers panics sees it.
func next(rows *sql.Rows) (ok bool, v any) {
	defer func() { v = recover() }()
	return rows.Next(), nil
}
