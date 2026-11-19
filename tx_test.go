package s4ql_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql"
	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/sqlite/faultvfs"
	"maragu.dev/s4ql/internal/store/simstore"
)

// readOnly are the options of a read-only transaction.
var readOnly = &sql.TxOptions{ReadOnly: true}

// begin a transaction on db with the options opts, which must succeed. It is rolled back when the
// test ends, unless it has ended.
func begin(t *testing.T, db interface {
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}, opts *sql.TxOptions) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), opts)
	is.NotError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// execIn the transaction tx the statements, each of which must succeed.
func execIn(t *testing.T, tx *sql.Tx, statements ...string) {
	t.Helper()
	for _, s := range statements {
		_, err := tx.ExecContext(t.Context(), s)
		is.NotError(t, err, s)
	}
}

// sqliteError requires that err is SQLite's error with the codes and the message, and no error of
// s4ql's own.
func sqliteError(t *testing.T, code, extended int, message string, err error) {
	t.Helper()
	se, ok := errors.AsType[*s4ql.Error](failed(t, err))
	is.True(t, ok, fmt.Sprint("not SQLite's error: ", err))
	is.Equal(t, code, se.Code)
	is.Equal(t, extended, se.ExtendedCode)
	is.Equal(t, message, se.Message)
	is.True(t, !errors.Is(err, s4ql.ErrNotSupported) && !errors.Is(err, s4ql.ErrStopped) && !errors.Is(err, s4ql.ErrNotServing), err.Error())
}

func TestTx(t *testing.T) {
	t.Run("should commit several statements as one commit object, unseen by others before the commit, and seen after it and from an empty cache after a restart", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		before := len(commits(t, w))

		tx := begin(t, db, nil)
		execIn(t, tx, insert1, "insert into t1 (v1) values (2)")
		_, err := tx.ExecContext(t.Context(), "insert into t1 (v1) values (?)", 3)
		is.NotError(t, err)

		// The transaction reads its own writes, and nothing outside sees them.
		is.EqualSlice(t, []int64{1, 2, 3}, values(t, tx, select1))
		is.EqualSlice(t, []int64{}, values(t, db, select1))
		is.Equal(t, before, len(commits(t, w)))

		is.NotError(t, tx.Commit())
		is.EqualSlice(t, []int64{1, 2, 3}, values(t, db, select1))
		is.Equal(t, before+1, len(commits(t, w)))
		is.EqualSlice(t, []string{t1, insert1, "insert into t1 (v1) values (2)", "insert into t1 (v1) values (?) with 3"}, annotations(t, w))

		is.NotError(t, db.Close())
		is.EqualSlice(t, []int64{1, 2, 3}, values(t, w.serve(t, "b"), select1))
	})

	t.Run("should keep nothing of a transaction that is rolled back, and write no commit object for it", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1, insert1)
		before := len(commits(t, w))

		tx := begin(t, db, nil)
		execIn(t, tx, "insert into t1 (v1) values (2)", "create table t2 (v2 int primary key) strict, without rowid", "insert into t2 (v2) values (1)")
		is.EqualSlice(t, []int64{1, 2}, values(t, tx, select1))
		is.NotError(t, tx.Rollback())

		is.EqualSlice(t, []int64{1}, values(t, db, select1))
		_, err := db.ExecContext(t.Context(), "select * from t2")
		sqliteError(t, 1, 1, "no such table: t2", err)
		is.Equal(t, before, len(commits(t, w)))
	})

	t.Run("should keep the statements before and after one that fails, and commit them without it", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)

		tx := begin(t, db, nil)
		execIn(t, tx, insert1)
		_, err := tx.ExecContext(t.Context(), insert1)
		sqliteError(t, 19, 1555, "UNIQUE constraint failed: t1.v1", err)
		_, err = tx.ExecContext(t.Context(), "insert into t2 (v2) values (1)")
		sqliteError(t, 1, 1, "no such table: t2", err)
		_, err = tx.ExecContext(t.Context(), "select * from t1 where v1 = 1")
		is.Equal(t, "WHERE clause (at offset 17)", notSupported(t, err))
		execIn(t, tx, "insert into t1 (v1) values (2)")
		is.NotError(t, tx.Commit())

		is.EqualSlice(t, []int64{1, 2}, values(t, db, select1))
		is.EqualSlice(t, []string{t1, insert1, "insert into t1 (v1) values (2)"}, annotations(t, w))
	})

	t.Run("should make create table and drop table part of the transaction, and undo them at a roll back", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1, insert1)
		t2 := "create table t2 (v2 int primary key) strict, without rowid"

		// A create and a drop that are rolled back.
		tx := begin(t, db, nil)
		execIn(t, tx, t2, "insert into t2 (v2) values (5)", "drop table t1")
		is.EqualSlice(t, []int64{5}, values(t, tx, "select * from t2"))
		_, err := tx.ExecContext(t.Context(), select1)
		sqliteError(t, 1, 1, "no such table: t1", err)
		// Outside, t1 is there and t2 is not.
		is.EqualSlice(t, []int64{1}, values(t, db, select1))
		_, err = db.ExecContext(t.Context(), "select * from t2")
		sqliteError(t, 1, 1, "no such table: t2", err)
		is.NotError(t, tx.Rollback())
		is.EqualSlice(t, []int64{1}, values(t, db, select1))
		_, err = db.ExecContext(t.Context(), "select * from t2")
		sqliteError(t, 1, 1, "no such table: t2", err)

		// The same, committed.
		tx = begin(t, db, nil)
		execIn(t, tx, t2, "insert into t2 (v2) values (5)", "drop table t1")
		is.NotError(t, tx.Commit())
		is.EqualSlice(t, []int64{5}, values(t, db, "select * from t2"))
		_, err = db.ExecContext(t.Context(), select1)
		sqliteError(t, 1, 1, "no such table: t1", err)

		// A create and a drop of the same table change nothing, and write no commit object.
		before := len(commits(t, w))
		tx = begin(t, db, nil)
		execIn(t, tx, t1, insert1, "drop table t1")
		is.NotError(t, tx.Commit())
		is.Equal(t, before, len(commits(t, w)))

		is.NotError(t, db.Close())
		is.EqualSlice(t, []int64{5}, values(t, w.serve(t, "b"), "select * from t2"))
	})

	t.Run("should run prepared statements with arguments in a transaction: prepared on it, and prepared on the database and taken into it", func(t *testing.T) {
		w := newWorld(t)
		var log lines
		o := w.options("a")
		o.Logger = slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
		db, err := s4ql.OpenStore(t.Context(), w.host, w.client(), o, coreOptions())
		is.NotError(t, err)
		defer func() { _ = db.Close() }()
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, t1)
		outside, err := db.PrepareContext(t.Context(), "insert into t1 (v1) values (?)")
		is.NotError(t, err)
		defer func() { is.NotError(t, outside.Close()) }()

		tx := begin(t, db, nil)
		inside, err := tx.PrepareContext(t.Context(), "insert into t1 (v1) values (?)")
		is.NotError(t, err)
		for _, v := range []int64{424201, 424202} {
			res, err := inside.ExecContext(t.Context(), v)
			is.Equal(t, int64(1), affected(t, res, err))
		}
		_, err = inside.ExecContext(t.Context(), 424201)
		sqliteError(t, 19, 1555, "UNIQUE constraint failed: t1.v1", err)
		_, err = inside.ExecContext(t.Context(), "x")
		is.Equal(t, "argument 1, which is text, for the column t1.v1 of type INT", notSupported(t, err))
		taken := tx.StmtContext(t.Context(), outside)
		res, err := taken.ExecContext(t.Context(), 424203)
		is.Equal(t, int64(1), affected(t, res, err))
		var v int64
		is.NotError(t, tx.QueryRowContext(t.Context(), select1).Scan(&v))
		is.Equal(t, int64(424201), v)
		query, err := tx.PrepareContext(t.Context(), select1)
		is.NotError(t, err)
		rows, err := query.QueryContext(t.Context())
		is.NotError(t, err)
		is.EqualSlice(t, []int64{424201, 424202, 424203}, rest(t, rows))
		is.NotError(t, tx.Commit())

		is.EqualSlice(t, []int64{424201, 424202, 424203}, values(t, db, select1))
		is.EqualSlice(t, []string{t1, "insert into t1 (v1) values (?) with 424201", "insert into t1 (v1) values (?) with 424202", "insert into t1 (v1) values (?) with 424203"}, annotations(t, w))
		// The statement prepared on the database still runs on it, outside the transaction.
		res, err = outside.ExecContext(t.Context(), 424204)
		is.Equal(t, int64(1), affected(t, res, err))

		// No value of an argument is in the log.
		is.NotError(t, db.Close())
		is.True(t, len(log.of("transaction")) > 0, "no event of the transaction")
		log.mu.Lock()
		text := log.b.String()
		log.mu.Unlock()
		is.True(t, !strings.Contains(text, "42420"), "a value of an argument is in the log")
	})

	t.Run("should read the state of its beginning in a read-only transaction while others commit, refuse a write in it with SQLite's error, and hold up no writer", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1, insert1)
		before := len(commits(t, w))

		ro := begin(t, db, readOnly)
		is.EqualSlice(t, []int64{1}, values(t, ro, select1))

		// A statement of its own and a write transaction commit meanwhile, without waiting.
		exec(t, db, "insert into t1 (v1) values (2)")
		writer := begin(t, db, nil)
		execIn(t, writer, "insert into t1 (v1) values (3)")
		is.NotError(t, writer.Commit())
		is.EqualSlice(t, []int64{1, 2, 3}, values(t, db, select1))
		is.EqualSlice(t, []int64{1}, values(t, ro, select1))

		// A write in it is refused as SQLite refuses it under query_only, after SQLite's errors of
		// the names, and the transaction stays usable.
		for _, src := range []string{"insert into t1 (v1) values (4)", "create table t2 (v2 int primary key) strict, without rowid", "drop table t1"} {
			_, err := ro.ExecContext(t.Context(), src)
			sqliteError(t, 8, 8, "attempt to write a readonly database", err)
		}
		_, err := ro.ExecContext(t.Context(), "insert into t1 (v1) values (?)", 4)
		sqliteError(t, 8, 8, "attempt to write a readonly database", err)
		_, err = ro.ExecContext(t.Context(), "insert into t9 (v1) values (4)")
		sqliteError(t, 1, 1, "no such table: t9", err)
		is.EqualSlice(t, []int64{1}, values(t, ro, select1))
		is.NotError(t, ro.Commit())

		is.EqualSlice(t, []int64{1, 2, 3}, values(t, db, select1))
		is.Equal(t, before+2, len(commits(t, w)))
	})

	t.Run("should begin both kinds with the default and the serializable isolation level, and refuse every other level as not supported, without beginning", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, conn.Close()) }()

		for _, readOnly := range []bool{false, true} {
			for _, level := range []sql.IsolationLevel{sql.LevelDefault, sql.LevelSerializable} {
				tx := begin(t, conn, &sql.TxOptions{Isolation: level, ReadOnly: readOnly})
				is.EqualSlice(t, []int64{}, values(t, tx, select1))
				is.NotError(t, tx.Commit())
			}
			for _, level := range []sql.IsolationLevel{sql.LevelReadUncommitted, sql.LevelReadCommitted, sql.LevelWriteCommitted, sql.LevelRepeatableRead, sql.LevelSnapshot, sql.LevelLinearizable, 99} {
				begun, err := conn.BeginTx(t.Context(), &sql.TxOptions{Isolation: level, ReadOnly: readOnly})
				if err == nil {
					// So that the close of the connection does not wait for it.
					_ = begun.Rollback()
				}
				is.Equal(t, "the isolation level "+level.String(), notSupported(t, err))
				// Nothing was begun on the connection, where a transaction now begins.
				tx := begin(t, conn, nil)
				is.NotError(t, tx.Rollback())
			}
		}
	})

	t.Run("should refuse a statement while the rows of an earlier query of the transaction are open, and run it after they are closed", func(t *testing.T) {
		for _, opts := range []*sql.TxOptions{nil, readOnly} {
			w := newWorld(t)
			db := w.serve(t, "a")
			exec(t, db, t1, insert1, "insert into t1 (v1) values (2)")

			tx := begin(t, db, opts)
			rows, err := tx.QueryContext(t.Context(), select1)
			is.NotError(t, err)
			is.True(t, rows.Next())
			const what = "a statement while the rows of a query of a transaction on its connection are open; close the rows first"
			_, err = tx.ExecContext(t.Context(), "insert into t1 (v1) values (3)")
			is.Equal(t, what, notSupported(t, err))
			_, err = tx.QueryContext(t.Context(), select1)
			is.Equal(t, what, notSupported(t, err))
			// The rows are not changed by the refusal.
			is.EqualSlice(t, []int64{2}, rest(t, rows))

			if opts == nil {
				execIn(t, tx, "insert into t1 (v1) values (3)")
			}
			is.EqualSlice(t, map[bool][]int64{true: {1, 2, 3}, false: {1, 2}}[opts == nil], values(t, tx, select1))
			is.NotError(t, tx.Commit())
			is.EqualSlice(t, map[bool][]int64{true: {1, 2, 3}, false: {1, 2}}[opts == nil], values(t, db, select1))
		}
	})

	t.Run("should give ErrTooLarge for a commit object that the store does not take, commit nothing, and go on serving", func(t *testing.T) {
		w := newWorld(t)
		w.store = simstore.New(w.host, simstore.Options{MaxSize: 4 << 10})
		db := w.serve(t, "a")
		exec(t, db, t1)
		db.SetMaxOpenConns(1)
		before := len(commits(t, w))

		tx := begin(t, db, nil)
		for i := range 100 {
			execIn(t, tx, fmt.Sprint("insert into t1 (v1) values (", i, ")"))
		}
		err := tx.Commit()
		is.Error(t, s4ql.ErrTooLarge, failed(t, err))
		is.True(t, strings.HasPrefix(err.Error(), "s4ql: the transaction is too large to commit: "), err.Error())
		for _, other := range []error{s4ql.ErrNotSupported, s4ql.ErrNotServing, s4ql.ErrOutcomeUnknown, s4ql.ErrStopped} {
			is.True(t, !errors.Is(err, other), fmt.Sprint(err, " matches ", other))
		}
		_, isSQLite := errors.AsType[*s4ql.Error](err)
		is.True(t, !isSQLite, err.Error())
		is.Error(t, sql.ErrTxDone, tx.Rollback())

		// Nothing is committed, and the connection of the transaction serves the next statements.
		is.Equal(t, before, len(commits(t, w)))
		is.EqualSlice(t, []int64{}, values(t, db, select1))
		exec(t, db, insert1)
		is.EqualSlice(t, []int64{1}, values(t, db, select1))
		readersFree(t, db)
	})
}

func TestTx_wait(t *testing.T) {
	t.Run("should make a second write transaction wait for the first until it ends, and let reads outside go on meanwhile", func(t *testing.T) {
		for _, end := range []string{"commit", "rollback"} {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				db := w.serve(t, "a")
				exec(t, db, t1)

				first := begin(t, db, nil)
				execIn(t, first, insert1)
				type begun struct {
					tx  *sql.Tx
					err error
				}
				second := make(chan begun, 1)
				go func() {
					tx, err := db.BeginTx(t.Context(), nil)
					second <- begun{tx, err}
				}()
				synctest.Wait()
				select {
				case b := <-second:
					t.Fatalf("the second transaction began while the first is open: %v", b.err)
				default:
				}

				// Reads outside go on, and a read-only transaction begins.
				is.EqualSlice(t, []int64{}, values(t, db, select1))
				ro := begin(t, db, readOnly)
				is.EqualSlice(t, []int64{}, values(t, ro, select1))
				is.NotError(t, ro.Commit())

				want := []int64{}
				if end == "commit" {
					is.NotError(t, first.Commit())
					want = []int64{1}
				} else {
					is.NotError(t, first.Rollback())
				}
				b := <-second
				is.NotError(t, b.err)
				// The second sees what the first committed.
				is.EqualSlice(t, want, values(t, b.tx, select1))
				execIn(t, b.tx, "insert into t1 (v1) values (2)")
				is.NotError(t, b.tx.Commit())
				is.EqualSlice(t, append(want, 2), values(t, db, select1))
			})
		}
	})

	t.Run("should end the wait of a second transaction, and of a write outside, with their contexts, and begin nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t)
			db := w.serve(t, "a")
			exec(t, db, t1)

			first := begin(t, db, nil)
			execIn(t, first, insert1)

			// A write outside the transaction waits for it, also from the goroutine that holds it,
			// until its context ends, and is not committed.
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			_, err := db.BeginTx(ctx, nil)
			cancel()
			is.Error(t, context.DeadlineExceeded, failed(t, err))
			ctx, cancel = context.WithTimeout(t.Context(), time.Minute)
			_, err = db.ExecContext(ctx, "insert into t1 (v1) values (2)")
			cancel()
			is.Error(t, context.DeadlineExceeded, failed(t, err))
			for _, other := range []error{s4ql.ErrNotSupported, s4ql.ErrNotServing, s4ql.ErrOutcomeUnknown, s4ql.ErrStopped} {
				is.True(t, !errors.Is(err, other), fmt.Sprint(err, " matches ", other))
			}

			// The first goes on, and commits alone.
			execIn(t, first, "insert into t1 (v1) values (3)")
			is.NotError(t, first.Commit())
			is.EqualSlice(t, []int64{1, 3}, values(t, db, select1))

			// The place of the writer is free.
			tx := begin(t, db, nil)
			is.NotError(t, tx.Commit())
		})
	})
}

func TestTx_leaseLost(t *testing.T) {
	for _, end := range []string{"commit", "rollback"} {
		t.Run("should serve again while a write transaction is open and idle as the instance loses its lease, refuse its statements and its commit with ErrNotServing, return nil from its roll back, and put its connection back in the pool: "+end, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				options := core.Options{BusyTimeout: time.Minute}
				s := &faulty{Store: w.client()}
				a := w.openOn(t, "a", s, options)
				is.NotError(t, a.PingContext(t.Context()))
				exec(t, a, t1)
				tx := begin(t, a, nil)
				execIn(t, tx, insert1)

				// a cannot reach the store while b takes the lease and commits, and b is closed then.
				s.down(true)
				b := w.openOn(t, "b", w.client(), options)
				is.NotError(t, b.PingContext(t.Context()))
				exec(t, b, "insert into t1 (v1) values (2)")
				s.down(false)
				time.Sleep(time.Minute)
				is.NotError(t, b.Close())

				// a serves again, on another connection, while the transaction is still open.
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				is.NotError(t, a.PingContext(ctx))
				is.EqualSlice(t, []int64{2}, values(t, a, select1))

				notServing := func(err error) {
					t.Helper()
					is.Error(t, s4ql.ErrNotServing, failed(t, err))
					for _, other := range []error{s4ql.ErrStopped, s4ql.ErrOutcomeUnknown, s4ql.ErrNotSupported, s4ql.ErrTooLarge} {
						is.True(t, !errors.Is(err, other), fmt.Sprint(err, " matches ", other))
					}
					is.True(t, strings.Contains(err.Error(), "the transaction was ended because the instance lost its lease"), err.Error())
				}
				_, err := tx.ExecContext(t.Context(), "insert into t1 (v1) values (3)")
				notServing(err)
				_, err = tx.QueryContext(t.Context(), select1)
				notServing(err)
				if end == "commit" {
					notServing(tx.Commit())
				} else {
					is.NotError(t, tx.Rollback())
				}
				is.Error(t, sql.ErrTxDone, tx.Rollback())

				// The connection of the transaction went back to the pool, beside the one of the ping.
				is.Equal(t, 2, a.Stats().OpenConnections)
				is.Equal(t, 2, a.Stats().Idle)
				a.SetMaxOpenConns(1)
				next := begin(t, a, nil)
				execIn(t, next, "insert into t1 (v1) values (4)")
				is.NotError(t, next.Commit())
				is.EqualSlice(t, []int64{2, 4}, values(t, a, select1))
			})
		})
	}
}

func TestTx_databaseSQL(t *testing.T) {
	t.Run("should roll the transaction back when the context of its begin ends, and free the place of the writer and the connection", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		db.SetMaxOpenConns(1)

		ctx, cancel := context.WithCancel(t.Context())
		tx, err := db.BeginTx(ctx, nil)
		is.NotError(t, err)
		execIn(t, tx, insert1)
		cancel()

		// database/sql rolls back from a goroutine of its own, so the commit finds the transaction
		// either rolled back or with its context ended, and commits nothing either way.
		err = tx.Commit()
		is.True(t, errors.Is(err, sql.ErrTxDone) || errors.Is(err, context.Canceled), fmt.Sprint(err))
		// The next transaction gets the one connection and the place of the writer once the roll back
		// has freed them.
		next := begin(t, db, nil)
		is.EqualSlice(t, []int64{}, values(t, next, select1))
		is.NotError(t, next.Commit())
		is.EqualSlice(t, []int64{}, values(t, db, select1))
		is.EqualSlice(t, []string{t1}, annotations(t, w))
		readersFree(t, db)
	})

	t.Run("should close the rows of the transaction before its commit or its roll back, which then ends it", func(t *testing.T) {
		for _, end := range []string{"commit", "rollback"} {
			for _, opts := range []*sql.TxOptions{nil, readOnly} {
				w := newWorld(t)
				db := w.serve(t, "a")
				exec(t, db, t1, insert1, "insert into t1 (v1) values (2)")
				db.SetMaxOpenConns(1)

				tx := begin(t, db, opts)
				if opts == nil {
					execIn(t, tx, "insert into t1 (v1) values (3)")
				}
				rows, err := tx.QueryContext(t.Context(), select1)
				is.NotError(t, err)
				is.True(t, rows.Next())
				if end == "commit" {
					is.NotError(t, tx.Commit())
				} else {
					is.NotError(t, tx.Rollback())
				}
				// The rows were closed: they give no more.
				is.True(t, !rows.Next())
				is.NotError(t, rows.Close())

				want := []int64{1, 2}
				if opts == nil && end == "commit" {
					want = append(want, 3)
				}
				is.EqualSlice(t, want, values(t, db, select1))
				next := begin(t, db, nil)
				is.NotError(t, next.Commit())
				readersFree(t, db)
			}
		}
	})

	t.Run("should give sql.ErrTxDone for a commit or a roll back after the end, and put the connection back in the pool, usable", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		db.SetMaxOpenConns(1)

		tx := begin(t, db, nil)
		execIn(t, tx, insert1)
		is.NotError(t, tx.Rollback())
		is.Error(t, sql.ErrTxDone, tx.Commit())
		is.Error(t, sql.ErrTxDone, tx.Rollback())
		_, err := tx.ExecContext(t.Context(), insert1)
		is.Error(t, sql.ErrTxDone, err)

		tx = begin(t, db, readOnly)
		is.NotError(t, tx.Commit())
		is.Error(t, sql.ErrTxDone, tx.Commit())
		is.Error(t, sql.ErrTxDone, tx.Rollback())

		// The one connection runs statements of their own after the transactions.
		exec(t, db, insert1)
		is.EqualSlice(t, []int64{1}, values(t, db, select1))
		is.Equal(t, 1, db.Stats().OpenConnections)
		readersFree(t, db)
	})

	t.Run("should begin through a pinned connection as through the pool, and run the statements of the connection in its transaction", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, conn.Close()) }()

		tx := begin(t, conn, nil)
		execIn(t, tx, insert1)
		// A statement on the connection runs in the transaction, which database/sql allows.
		_, err = conn.ExecContext(t.Context(), "insert into t1 (v1) values (2)")
		is.NotError(t, err)
		is.EqualSlice(t, []int64{1, 2}, values(t, conn, select1))
		is.EqualSlice(t, []int64{}, values(t, db, select1))
		// A second transaction on the connection is refused, and the first stays open.
		_, err = conn.BeginTx(t.Context(), nil)
		is.Equal(t, "s4ql: a transaction is open on the connection", failed(t, err).Error())
		for _, sentinel := range []error{s4ql.ErrNotSupported, s4ql.ErrNotServing, s4ql.ErrOutcomeUnknown, s4ql.ErrStopped, s4ql.ErrTooLarge} {
			is.True(t, !errors.Is(err, sentinel), fmt.Sprint(err, " matches ", sentinel))
		}
		is.NotError(t, tx.Commit())
		is.EqualSlice(t, []int64{1, 2}, values(t, db, select1))

		// A read-only one, and one that is rolled back.
		ro := begin(t, conn, readOnly)
		exec(t, db, "insert into t1 (v1) values (3)")
		is.EqualSlice(t, []int64{1, 2}, values(t, ro, select1))
		is.NotError(t, ro.Rollback())
		tx = begin(t, conn, nil)
		execIn(t, tx, "insert into t1 (v1) values (4)")
		is.NotError(t, tx.Rollback())
		is.EqualSlice(t, []int64{1, 2, 3}, values(t, conn, select1))

		// A transaction does not begin while rows of the connection are open.
		rows, err := conn.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		is.True(t, rows.Next())
		for _, opts := range []*sql.TxOptions{nil, readOnly} {
			_, err = conn.BeginTx(t.Context(), opts)
			is.Equal(t, "a transaction that begins on a connection whose rows of an earlier query are open; close the rows first", notSupported(t, err))
		}
		is.NotError(t, rows.Close())
		tx = begin(t, conn, nil)
		is.NotError(t, tx.Commit())
	})

	t.Run("should keep a connection whose transaction is open out of the pool, and roll the transaction back with its close", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)

		// The driver's own interface begins a transaction that database/sql does not know of.
		is.NotError(t, conn.Raw(func(driverConn any) error {
			tx, err := driverConn.(driver.Conn).Begin() //nolint:staticcheck // The call is deprecated, and the driver has it.
			is.NotError(t, err)
			is.True(t, tx != nil)
			_, err = driverConn.(driver.ExecerContext).ExecContext(t.Context(), insert1, nil)
			return err
		}))
		is.NotError(t, conn.Close())

		// The pool closed the connection, which rolled the transaction back and freed the place of
		// the writer.
		tx := begin(t, db, nil)
		is.EqualSlice(t, []int64{}, values(t, tx, select1))
		is.NotError(t, tx.Commit())
		is.EqualSlice(t, []int64{}, values(t, db, select1))
		readersFree(t, db)
	})

	t.Run("should roll back and not commit a commit of the driver's own interface while a loop reads the rows of the transaction", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1, insert1, "insert into t1 (v1) values (2)")
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, conn.Close()) }()

		is.NotError(t, conn.Raw(func(driverConn any) error {
			tx, err := driverConn.(driver.ConnBeginTx).BeginTx(t.Context(), driver.TxOptions{})
			is.NotError(t, err)
			_, err = driverConn.(driver.ExecerContext).ExecContext(t.Context(), "insert into t1 (v1) values (3)", nil)
			is.NotError(t, err)
			rows, err := driverConn.(driver.QueryerContext).QueryContext(t.Context(), select1, nil)
			is.NotError(t, err)
			dest := make([]driver.Value, 1)
			is.NotError(t, rows.Next(dest))
			err = tx.Commit()
			is.Equal(t, "a commit while the rows of a query of the transaction are read, so the transaction is rolled back and not committed", notSupported(t, err))
			is.True(t, !errors.Is(err, s4ql.ErrOutcomeUnknown))
			// The rows end, and so does the transaction with them.
			is.True(t, rows.Next(dest) != nil)
			is.NotError(t, rows.Close())
			return nil
		}))
		is.EqualSlice(t, []int64{1, 2}, values(t, db, select1))
		tx := begin(t, db, nil)
		is.NotError(t, tx.Commit())
	})
}

func TestTx_instance(t *testing.T) {
	t.Run("should answer with the stop and its cause in a transaction, at its begin, and at its commit, and roll back quietly", func(t *testing.T) {
		w := newWorld(t)
		vfs, err := faultvfs.New()
		is.NotError(t, err)
		t.Cleanup(func() { is.NotError(t, vfs.Close()) })
		co := coreOptions()
		co.VFS = vfs.Name()
		db := w.openOn(t, "a", w.client(), co)
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, t1, insert1)

		write := begin(t, db, nil)
		execIn(t, write, "insert into t1 (v1) values (2)")
		ro := begin(t, db, readOnly)
		is.EqualSlice(t, []int64{1}, values(t, ro, select1))
		ro2 := begin(t, db, readOnly)
		write2 := make(chan error, 1)
		go func() {
			// It waits for the place of the writer until the stop.
			_, err := db.BeginTx(t.Context(), nil)
			write2 <- err
		}()

		// The cache cannot be opened for the reader of a new connection, which stops the instance as
		// any local failure of the cache does: a write transaction is open, and the insert of a
		// statement of its own would wait for it.
		vfs.Fail(faultvfs.Fault{Ops: faultvfs.Open, Files: faultvfs.MainDB, Times: 1})
		_, err = db.QueryContext(t.Context(), select1)
		stopped := func(err error) {
			t.Helper()
			is.Error(t, s4ql.ErrStopped, failed(t, err))
			is.True(t, strings.Contains(err.Error(), "the instance has stopped: local I/O failure"), err.Error())
			for _, other := range []error{s4ql.ErrNotSupported, s4ql.ErrNotServing, s4ql.ErrOutcomeUnknown, s4ql.ErrTooLarge} {
				is.True(t, !errors.Is(err, other), fmt.Sprint(err, " matches ", other))
			}
		}
		stopped(err)
		is.Equal(t, 1, vfs.Failures())
		stopped(<-write2)

		_, err = write.ExecContext(t.Context(), "insert into t1 (v1) values (4)")
		stopped(err)
		stopped(write.Commit())
		is.Error(t, sql.ErrTxDone, write.Rollback())
		_, err = ro.QueryContext(t.Context(), select1)
		stopped(err)
		// A read-only transaction has nothing to commit.
		is.NotError(t, ro.Commit())
		// A roll back is quiet.
		is.NotError(t, ro2.Rollback())
		for _, opts := range []*sql.TxOptions{nil, readOnly} {
			_, err = db.BeginTx(t.Context(), opts)
			stopped(err)
		}
		// What the options alone decide does not depend on the instance.
		_, err = db.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		is.Equal(t, "the isolation level Read Committed", notSupported(t, err))

		_ = db.Close()
		vfs.Fail(faultvfs.Fault{})
		is.EqualSlice(t, []int64{1}, values(t, w.serve(t, "a"), select1))
	})

	t.Run("should refuse a transaction and its statements while the instance does not serve, and commit a transaction whose outcome was unknown once", func(t *testing.T) {
		w := newWorld(t)
		s := &faulty{Store: w.client()}
		db := w.openOn(t, "a", s, coreOptions())
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, t1)
		ro := begin(t, db, readOnly)
		is.EqualSlice(t, []int64{}, values(t, ro, select1))
		ro2 := begin(t, db, readOnly)

		// The store applies the manifest swap of the commit and loses its answer, the context of the
		// transaction ends at that moment, and the store cannot be reached from then on.
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		tx, err := db.BeginTx(ctx, nil)
		is.NotError(t, err)
		_, err = tx.ExecContext(ctx, insert1)
		is.NotError(t, err)
		_, err = tx.ExecContext(ctx, "insert into t1 (v1) values (?)", 2)
		is.NotError(t, err)
		s.loseNext(cancel)
		err = tx.Commit()
		is.Error(t, s4ql.ErrOutcomeUnknown, failed(t, err))
		for _, other := range []error{context.Canceled, s4ql.ErrNotSupported, s4ql.ErrNotServing, s4ql.ErrStopped, s4ql.ErrTooLarge} {
			is.True(t, !errors.Is(err, other), fmt.Sprint(err, " matches ", other))
		}
		is.Error(t, sql.ErrTxDone, tx.Rollback())

		// The instance serves nothing until it knows: no begin, and no statement in a transaction.
		notServing := func(err error) {
			t.Helper()
			is.Error(t, s4ql.ErrNotServing, failed(t, err))
			is.True(t, !errors.Is(err, s4ql.ErrStopped) && !errors.Is(err, s4ql.ErrOutcomeUnknown), err.Error())
		}
		for _, opts := range []*sql.TxOptions{nil, readOnly} {
			_, err = db.BeginTx(t.Context(), opts)
			notServing(err)
		}
		_, err = ro.QueryContext(t.Context(), select1)
		notServing(err)
		is.NotError(t, ro.Commit())
		is.NotError(t, ro2.Rollback())

		// It finds out by itself: the transaction is committed, once.
		s.down(false)
		is.NotError(t, db.PingContext(t.Context()))
		is.EqualSlice(t, []int64{1, 2}, values(t, db, select1))
		is.Equal(t, 2, len(commits(t, w)))
		is.EqualSlice(t, []string{t1, insert1, "insert into t1 (v1) values (?) with 2"}, annotations(t, w))
		tx = begin(t, db, nil)
		is.NotError(t, tx.Commit())
	})

	t.Run("should end every transaction when the database is closed or the instance stops while transactions run in many goroutines, and commit a transaction if and only if it was acknowledged", func(t *testing.T) {
		for _, how := range []string{"close", "stop"} {
			t.Run(how, func(t *testing.T) {
				w := newWorld(t)
				vfs, err := faultvfs.New()
				is.NotError(t, err)
				t.Cleanup(func() { is.NotError(t, vfs.Close()) })
				co := coreOptions()
				co.VFS = vfs.Name()
				db := w.openOn(t, "a", w.client(), co)
				is.NotError(t, db.PingContext(t.Context()))
				exec(t, db, t1)

				ended := func(err error) bool {
					if how == "close" {
						return closedError(err)
					}
					return !errors.Is(err, driver.ErrBadConn) && errors.Is(err, s4ql.ErrStopped)
				}
				// complete reports whether the values hold v exactly when they hold -v: each transaction
				// inserts both.
				complete := func(vs []int64) bool {
					for _, v := range vs {
						if _, found := slices.BinarySearch(vs, -v); !found {
							return false
						}
					}
					return true
				}

				const writers, reading = 4, 4
				var mu sync.Mutex
				acked := map[int64]bool{}
				var next int64
				some, writing := make(chan struct{}), make(chan struct{})
				var once sync.Once
				errs := make(chan error, writers+reading)
				var writes, all sync.WaitGroup
				for g := range writers {
					writes.Go(func() {
						for i := 0; ; i++ {
							mu.Lock()
							next++
							v := next
							mu.Unlock()
							tx, err := db.BeginTx(t.Context(), nil)
							if err != nil {
								if !ended(err) {
									errs <- fmt.Errorf("a begin: %w", err)
								}
								return
							}
							err = func() error {
								// A roll back is quiet, also after the end of the instance.
								defer func() {
									if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
										errs <- fmt.Errorf("a roll back: %w", err)
									}
								}()
								for _, v := range []int64{v, -v} {
									if _, err := tx.ExecContext(t.Context(), "insert into t1 (v1) values (?)", v); err != nil {
										return err
									}
								}
								// Every other transaction of every other writer is rolled back.
								if g%2 == 0 && i%2 == 0 {
									return nil
								}
								if err := tx.Commit(); err != nil {
									return err
								}
								mu.Lock()
								acked[v] = true
								n := len(acked)
								mu.Unlock()
								if n >= 5 {
									once.Do(func() { close(some) })
								}
								return nil
							}()
							if err != nil {
								if !ended(err) {
									errs <- fmt.Errorf("the transaction of %v: %w", v, err)
								}
								return
							}
						}
					})
				}
				all.Go(func() {
					writes.Wait()
					close(writing)
				})
				for g := range reading {
					all.Go(func() {
						for {
							var q querier = db
							var tx *sql.Tx
							if g%2 == 0 {
								var err error
								if tx, err = db.BeginTx(t.Context(), readOnly); err != nil {
									if !ended(err) {
										errs <- fmt.Errorf("a read-only begin: %w", err)
									}
									return
								}
								q = tx
							}
							first, err := query(t.Context(), q, select1)
							if err == nil && tx != nil {
								// A read-only transaction reads one state.
								var again []int64
								if again, err = query(t.Context(), q, select1); err == nil && !slices.Equal(first, again) {
									err = fmt.Errorf("a read-only transaction read %v and then %v", first, again)
								}
								err = errors.Join(err, tx.Commit())
							}
							if err != nil {
								if !ended(err) {
									errs <- fmt.Errorf("a read: %w", err)
								}
								return
							}
							if !complete(first) {
								errs <- fmt.Errorf("a read saw part of a transaction: %v", first)
								return
							}
						}
					})
				}

				select {
				case <-some:
				case <-writing:
				}
				var closeErr error
				if how == "close" {
					closeErr = db.Close()
				} else {
					// Every write to the write-ahead log of the cache fails from now on, so the next commit
					// stops the instance: a commit of a writer, or this insert, which waits for the place of
					// the writer. A commit that meets the failure after the store has taken it succeeds.
					vfs.Fail(faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.WAL})
					if _, err := db.ExecContext(t.Context(), "insert into t1 (v1) values (0)"); err == nil {
						mu.Lock()
						acked[0] = true
						mu.Unlock()
					} else {
						is.Error(t, s4ql.ErrStopped, failed(t, err))
					}
				}
				writes.Wait()
				all.Wait()
				close(errs)
				for err := range errs {
					t.Error(err)
				}
				is.NotError(t, closeErr)
				is.True(t, len(acked) >= 5, fmt.Sprint(len(acked), " transactions are acknowledged"))
				is.True(t, vfs.Failures() > 0 || how == "close", "no write failed")
				if how == "stop" {
					// Every statement fails with the stop, which a write met.
					_, err := db.ExecContext(t.Context(), insert1)
					is.Error(t, s4ql.ErrStopped, failed(t, err))
					_ = db.Close()
					vfs.Fail(faultvfs.Fault{})
				}

				var want []int64
				for v := range acked {
					want = append(want, v)
					if v != 0 {
						want = append(want, -v)
					}
				}
				slices.Sort(want)
				is.EqualSlice(t, want, values(t, w.serve(t, "b"), select1))
			})
		}
	})

	t.Run("should close the rows of a transaction while the database is closed, beside statements of the transaction, and end the transaction", func(t *testing.T) {
		for _, kind := range []*sql.TxOptions{nil, readOnly} {
			closeBeside(t, func(t *testing.T, db *sql.DB) (*sql.Rows, func() (bool, error), func() error) {
				tx := begin(t, db, kind)
				rows, err := tx.QueryContext(t.Context(), select1)
				is.NotError(t, err)
				statement := func() (bool, error) {
					_, err := tx.ExecContext(t.Context(), "insert into t1 (v1) values (4)")
					switch {
					case err == nil, closedError(err):
						return false, nil
					case errors.Is(err, s4ql.ErrNotSupported) && strings.Contains(err.Error(), "close the rows first"):
						return true, nil
					case kind != nil && strings.Contains(err.Error(), "attempt to write a readonly database"):
						return false, nil
					}
					return false, err
				}
				return rows, statement, tx.Rollback
			})
		}
	})
}

// TestTx_raw checks what the driver gives for calls of its own interfaces that database/sql does
// not make.
func TestTx_raw(t *testing.T) {
	t.Run("should give an internal error for the end of a transaction that has ended", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, conn.Close()) }()

		is.NotError(t, conn.Raw(func(driverConn any) error {
			tx, err := driverConn.(driver.ConnBeginTx).BeginTx(t.Context(), driver.TxOptions{})
			is.NotError(t, err)
			is.NotError(t, tx.Rollback())
			for _, end := range []func() error{tx.Commit, tx.Rollback} {
				err := end()
				is.Equal(t, "s4ql: internal error: the transaction has ended", failed(t, err).Error())
			}
			// It does not end the transaction that is open on the connection now.
			again, err := driverConn.(driver.ConnBeginTx).BeginTx(t.Context(), driver.TxOptions{})
			is.NotError(t, err)
			is.True(t, tx.Commit() != nil)
			_, err = driverConn.(driver.ExecerContext).ExecContext(t.Context(), insert1, nil)
			is.NotError(t, err)
			return again.Rollback()
		}))
		is.EqualSlice(t, []int64{}, values(t, db, select1))
	})

	// The rows of the driver's interface that a loop reads end their transaction when they end, so a
	// connection that is given back while they are open must not go back to the pool, whose next user
	// would meet their refusal, and its close must not leave its transaction and its reader to no one.
	for _, end := range []string{"commit", "rollback", "none"} {
		t.Run("should keep a connection out of the pool while rows of its that a loop reads are open, and close it when they end, after the end of their transaction: "+end, func(t *testing.T) {
			w := newWorld(t)
			db := w.serve(t, "a")
			exec(t, db, t1, insert1)
			conn, err := db.Conn(t.Context())
			is.NotError(t, err)
			open := db.Stats().OpenConnections

			var rows driver.Rows
			dest := make([]driver.Value, 1)
			is.NotError(t, conn.Raw(func(driverConn any) error {
				tx, err := driverConn.(driver.ConnBeginTx).BeginTx(t.Context(), driver.TxOptions{})
				is.NotError(t, err)
				_, err = driverConn.(driver.ExecerContext).ExecContext(t.Context(), "insert into t1 (v1) values (2)", nil)
				is.NotError(t, err)
				rows, err = driverConn.(driver.QueryerContext).QueryContext(t.Context(), select1, nil)
				is.NotError(t, err)
				is.NotError(t, rows.Next(dest))
				switch end {
				case "commit":
					is.True(t, tx.Commit() != nil)
				case "rollback":
					is.NotError(t, tx.Rollback())
				}
				is.True(t, !driverConn.(driver.Validator).IsValid())
				return nil
			}))
			// database/sql closes the connection, which the engine closes when the rows end.
			is.NotError(t, conn.Close())
			is.Equal(t, open-1, db.Stats().OpenConnections)

			// The rows hold the place of the writer until they end, and the transaction with it, which
			// the close of the connection then rolls back, if it is still open. After the end of the
			// transaction, the next row is its end.
			n := 1
			for rows.Next(dest) == nil {
				n++
			}
			want := 1
			if end == "none" {
				want = 2
			}
			is.Equal(t, want, n)
			is.NotError(t, rows.Close())

			// The bound is far above what the begin needs, and keeps a fault from hanging the test.
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			tx, err := db.BeginTx(ctx, nil)
			is.NotError(t, err)
			is.EqualSlice(t, []int64{1}, values(t, tx, select1))
			is.NotError(t, tx.Commit())
			readersFree(t, db)
		})
	}
}
