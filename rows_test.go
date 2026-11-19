package s4ql_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql"
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
		got := make(chan []int64)
		go func() { got <- values(t, db, select1) }()
		is.NotError(t, all[0].Close())
		is.EqualSlice(t, []int64{1, 2, 3, 4}, <-got)

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
		for _, rows := range append(all, unread) {
			is.True(t, !rows.Next())
			closed(rows.Err())
			is.NotError(t, rows.Close())
		}

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
}
