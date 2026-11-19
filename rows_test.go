package s4ql_test

import (
	"context"
	"database/sql"
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

// held opens as many queries on db as the instance has readers, each with the context that ctx
// gives it, and reads one row of each, so that every read of the instance is held.
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

	t.Run("should make a query wait while every read is held, until one is free or its context ends", func(t *testing.T) {
		_, db := three(t)
		all := held(t, db, t.Context)

		// A slower machine ends the context all the same.
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		_, err := db.QueryContext(ctx, select1)
		is.Error(t, context.DeadlineExceeded, failed(t, err))
		// A query that is run to its end by Exec needs a read too.
		ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		_, err = db.ExecContext(ctx, select1)
		is.Error(t, context.DeadlineExceeded, failed(t, err))

		// A write needs none.
		exec(t, db, "insert into t1 (v1) values (4)")

		// A query that waits gets the read that is freed, and sees what is committed then.
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
		is.EqualSlice(t, []int64{1, 2, 3, 4}, a.values)

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

	t.Run("should run several queries at once on one connection", func(t *testing.T) {
		_, db := three(t)
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, conn.Close()) }()
		first, err := conn.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		is.True(t, first.Next())
		second, err := conn.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		for _, rows := range []*sql.Rows{second, first} {
			var v int64
			for rows.Next() {
				is.NotError(t, rows.Scan(&v))
			}
			is.Equal(t, int64(3), v)
			is.NotError(t, rows.Err())
		}
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

	t.Run("should raise a panic at the end of the loop on the goroutine that calls Close, and log it with its stack", func(t *testing.T) {
		w := newWorld(t)
		log := &panickyLog{}
		db := openLogged(t, w, log)
		exec(t, db, t1, insert1, "insert into t1 (v1) values (2)")

		// The engine logs the event of the query when the loop over its rows has ended, in the turn
		// that the close gives the task.
		rows, err := db.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		is.True(t, rows.Next())
		log.arm(true)
		v, _ := closeRows(rows)
		is.Equal(t, any(errPanic), v)
		log.arm(false)
		panics := log.lines.of("panic")
		is.Equal(t, 1, len(panics))
		is.True(t, strings.HasPrefix(panics[0], `level=ERROR msg=panic db.prefix=db panic="a panic of the test" stack="goroutine `), panics[0])

		// The rows are closed, and raise it no more.
		is.True(t, !rows.Next())
		is.NotError(t, rows.Err())
		is.NotError(t, rows.Close())
		readersFree(t, db)
		exec(t, db, "insert into t1 (v1) values (3)")
		is.NotError(t, db.Close())
		is.EqualSlice(t, []int64{1, 2, 3}, values(t, w.serve(t, "b"), select1))
	})

	t.Run("should not raise a panic at the end of the loop when the close of the database closes the rows, and log it", func(t *testing.T) {
		w := newWorld(t)
		log := &panickyLog{}
		db := openLogged(t, w, log)
		exec(t, db, t1, insert1, "insert into t1 (v1) values (2)")

		rows, err := db.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		is.True(t, rows.Next())
		log.arm(true)
		is.NotError(t, db.Close())
		log.arm(false)
		is.Equal(t, 1, len(log.lines.of("panic")))
		is.EqualSlice(t, []string{"level=INFO msg=close db.prefix=db"}, log.lines.of("close"))

		// No later call raises it.
		is.True(t, !rows.Next())
		is.Error(t, s4ql.ErrStopped, failed(t, rows.Err()))
		is.NotError(t, rows.Close())
		is.EqualSlice(t, []int64{1, 2}, values(t, w.serve(t, "b"), select1))
	})
}

// openLogged opens an instance of the world with the cache directory "a", which logs to log, and
// waits until it serves.
func openLogged(t *testing.T, w *world, log *panickyLog) *sql.DB {
	t.Helper()
	o := w.options("a")
	o.Logger = slog.New(log.handler())
	db, err := s4ql.OpenStore(t.Context(), w.host, w.client(), o, coreOptions())
	is.NotError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	is.NotError(t, db.PingContext(t.Context()))
	return db
}

// closeRows is [sql.Rows.Close] of rows, with the value of a panic in it, if it panicked.
func closeRows(rows *sql.Rows) (v any, err error) {
	defer func() { v = recover() }()
	return nil, rows.Close()
}

// panickyLog is a log in the text format of log/slog, whose handler panics with [errPanic] at the
// event of a statement while it is armed.
type panickyLog struct {
	lines lines
	armed atomic.Bool
}

// arm the log, or disarm it.
func (p *panickyLog) arm(armed bool) {
	p.armed.Store(armed)
}

func (p *panickyLog) handler() slog.Handler {
	return &panickyHandler{log: p, Handler: slog.NewTextHandler(&p.lines, &slog.HandlerOptions{Level: slog.LevelDebug})}
}

type panickyHandler struct {
	log *panickyLog
	slog.Handler
}

func (h *panickyHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.log.armed.Load() && r.Message == "statement" {
		panic(errPanic)
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
