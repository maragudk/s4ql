package s4ql_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql"
	"maragu.dev/s4ql/internal/sqlite/faultvfs"
)

// notSupported requires that err is "not supported", with a message that names what, and gives the
// message without its prefix.
func notSupported(t *testing.T, err error) string {
	t.Helper()
	is.Error(t, s4ql.ErrNotSupported, failed(t, err))
	_, isSQLite := errors.AsType[*s4ql.Error](err)
	is.True(t, !isSQLite, "not supported carries a result code of SQLite")
	what, ok := strings.CutPrefix(err.Error(), "s4ql: not supported: ")
	is.True(t, ok, err.Error())
	return what
}

// affected is the count of rows affected of res, whose last insert ID must be 0.
func affected(t *testing.T, res sql.Result, err error) int64 {
	t.Helper()
	is.NotError(t, err)
	id, err := res.LastInsertId()
	is.NotError(t, err)
	is.Equal(t, int64(0), id)
	n, err := res.RowsAffected()
	is.NotError(t, err)
	return n
}

func TestExec(t *testing.T) {
	t.Run("should run each statement of slice 1 as a transaction of its own", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1, insert1, "insert into t1 (v1) values (-9223372036854775808)", select1,
			`create table "T 2" ("K" int primary key) strict, without rowid`, `insert into "T 2" ("K") values (9223372036854775807);`)
		is.EqualSlice(t, []int64{-9223372036854775808, 1}, values(t, db, select1))
		is.EqualSlice(t, []int64{9223372036854775807}, values(t, db, `select * from "t 2"`))
		exec(t, db, "drop table t1")
		_, err := db.ExecContext(t.Context(), select1)
		is.Equal(t, "no such table: t1", failed(t, err).Error())
	})

	t.Run("should give the count of changes as SQLite does, for each connection, and no last insert ID", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, conn.Close()) }()
		run := func(query string) int64 {
			t.Helper()
			res, err := conn.ExecContext(t.Context(), query)
			return affected(t, res, err)
		}

		// A statement that sets no count leaves the one of the statement before, which is 0 on a new
		// connection.
		is.Equal(t, int64(0), run(t1))
		is.Equal(t, int64(1), run(insert1))
		is.Equal(t, int64(1), run("create table t2 (v1 int primary key) strict, without rowid"))
		is.Equal(t, int64(1), run(select1))
		is.Equal(t, int64(1), run("drop table t2"))

		// An insert that fails on its key sets the count to 0.
		_, err = conn.ExecContext(t.Context(), insert1)
		is.Equal(t, "UNIQUE constraint failed: t1.v1", failed(t, err).Error())
		is.Equal(t, int64(0), run(select1))

		// A statement that SQLite fails when it prepares it sets no count.
		is.Equal(t, int64(1), run("insert into t1 (v1) values (2)"))
		_, err = conn.ExecContext(t.Context(), "insert into t9 (v1) values (2)")
		is.Equal(t, "no such table: t9", failed(t, err).Error())
		_, err = conn.ExecContext(t.Context(), "insert into")
		is.Equal(t, "incomplete input", failed(t, err).Error())
		is.Equal(t, int64(1), run(select1))

		// An insert that is run as a query sets the count too, also one that fails.
		_, err = conn.QueryContext(t.Context(), insert1)
		is.Equal(t, "UNIQUE constraint failed: t1.v1", failed(t, err).Error())
		is.Equal(t, int64(0), run(select1))
		rows, err := conn.QueryContext(t.Context(), "insert into t1 (v1) values (3)")
		is.NotError(t, err)
		is.NotError(t, rows.Close())
		is.Equal(t, int64(1), run(select1))

		// Another connection has a count of its own.
		other, err := db.Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, other.Close()) }()
		res, err := other.ExecContext(t.Context(), select1)
		is.Equal(t, int64(0), affected(t, res, err))
		_, err = other.ExecContext(t.Context(), insert1)
		is.Equal(t, "UNIQUE constraint failed: t1.v1", failed(t, err).Error())
		is.Equal(t, int64(1), run(select1))
	})

	t.Run("should refuse what is not supported with an error of s4ql's own, and run none of it", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)

		_, err := db.ExecContext(t.Context(), "select * from t1 where v1 = 1")
		is.Equal(t, "WHERE clause (at offset 17)", notSupported(t, err))
		_, err = db.QueryContext(t.Context(), "insert into t1 (v1) values (1), (2)")
		is.True(t, notSupported(t, err) != "")
		_, err = db.ExecContext(t.Context(), insert1+"; "+insert1)
		is.True(t, notSupported(t, err) != "")
		_, err = db.ExecContext(t.Context(), "begin")
		is.True(t, notSupported(t, err) != "")

		// A text with no statement.
		for _, src := range []string{"", " ; -- nothing\n;"} {
			_, err = db.ExecContext(t.Context(), src)
			is.Equal(t, "a text that holds no statement", notSupported(t, err))
			_, err = db.QueryContext(t.Context(), src)
			is.Equal(t, "a text that holds no statement", notSupported(t, err))
			_, err = db.PrepareContext(t.Context(), src)
			is.Equal(t, "a text that holds no statement", notSupported(t, err))
		}

		// The text of a parameter is not supported, and SQLite's parser accepts it.
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (?)")
		is.True(t, notSupported(t, err) != "")

		// Transactions.
		_, err = db.BeginTx(t.Context(), nil)
		is.Equal(t, "transactions", notSupported(t, err))
		_, err = db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelSerializable})
		is.Equal(t, "transactions", notSupported(t, err))
		_, err = db.Begin()
		is.Equal(t, "transactions", notSupported(t, err))

		is.EqualSlice(t, []int64{}, values(t, db, select1))
	})

	t.Run("should judge the text before its arguments, through Exec, Query, and a prepared statement, and run none of it", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		insert, err := db.PrepareContext(t.Context(), insert1)
		is.NotError(t, err)
		defer func() { is.NotError(t, insert.Close()) }()
		query, err := db.PrepareContext(t.Context(), select1)
		is.NotError(t, err)
		defer func() { is.NotError(t, query.Close()) }()

		// Each call runs the text src with the arguments args, and gives its error.
		calls := map[string]func(src string, args ...any) error{
			"Exec": func(src string, args ...any) error {
				_, err := db.ExecContext(t.Context(), src, args...)
				return err
			},
			"Query": func(src string, args ...any) error {
				rows, err := db.QueryContext(t.Context(), src, args...)
				if err == nil {
					is.NotError(t, rows.Close())
				}
				return err
			},
			"QueryRow": func(src string, args ...any) error {
				return db.QueryRowContext(t.Context(), src, args...).Scan(new(int64))
			},
		}
		// Arguments of any kind and number, also where the text has no parameter.
		argss := [][]any{{1}, {"1", 2}, {sql.Named("v", 1)}, {struct{}{}}, {nil}, {[]string{"a"}}}

		for name, call := range calls {
			for _, args := range argss {
				// A syntax error gets SQLite's error, also in SQL that s4ql does not support.
				for src, message := range map[string]string{
					"drop table":                   "incomplete input",
					"selec * from t1":              `near "selec": syntax error`,
					"insert into t1 (v1) values (": "incomplete input",
					"select v1 from t1 where":      "incomplete input",
				} {
					err := call(src, args...)
					sqlite, ok := errors.AsType[*s4ql.Error](failed(t, err))
					is.True(t, ok, name, src, err.Error())
					is.Equal(t, s4ql.Error{Code: 1, ExtendedCode: 1, Message: message}, *sqlite, name, src)
				}

				// A text that s4ql does not support gets the "not supported" that it gets without arguments.
				for _, src := range []string{"select * from t1 where v1 = 1", "insert into t1 (v1) values (?)", "insert into t1 (v1) values (1), (2)", "begin", "", " ; -- nothing"} {
					err := call(src, args...)
					is.True(t, notSupported(t, err) != "parameters", name, src)
					is.Equal(t, notSupported(t, call(src)), notSupported(t, err), name, src)
				}

				// A statement that s4ql supports gets "not supported: parameters", and is not run: neither
				// an insert, nor a create table or a drop table, nor one that would fail if it ran.
				for _, src := range []string{insert1, select1, "drop table t1", "create table t2 (v2 int primary key) strict, without rowid", t1, "insert into t9 (v1) values (1)"} {
					is.Equal(t, "parameters", notSupported(t, call(src, args...)), name, src)
				}
			}
		}

		// A prepared statement checks its text when it is prepared, and gets "not supported: parameters"
		// when it is run with an argument.
		_, err = db.PrepareContext(t.Context(), "drop table")
		is.Equal(t, "incomplete input", failed(t, err).Error())
		_, err = db.PrepareContext(t.Context(), "select * from t1 where v1 = 1")
		is.Equal(t, "WHERE clause (at offset 17)", notSupported(t, err))
		for _, args := range argss {
			_, err = insert.ExecContext(t.Context(), args...)
			is.Equal(t, "parameters", notSupported(t, err))
			_, err = insert.QueryContext(t.Context(), args...)
			is.Equal(t, "parameters", notSupported(t, err))
			_, err = query.QueryContext(t.Context(), args...)
			is.Equal(t, "parameters", notSupported(t, err))
			is.Equal(t, "parameters", notSupported(t, query.QueryRowContext(t.Context(), args...).Scan(new(int64))))
		}

		// Nothing was committed after the create table of t1, which still holds no row.
		is.EqualSlice(t, []int64{}, values(t, db, select1))
		is.Equal(t, 1, len(commits(t, w)))
	})

	t.Run("should end with its context, also in its commit, with an error that is none of the instance", func(t *testing.T) {
		w := newWorld(t)
		s := &faulty{Store: w.client()}
		db := w.openOn(t, "a", s, coreOptions())
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, t1)
		ended := func(err error) {
			t.Helper()
			is.Error(t, context.Canceled, failed(t, err))
			is.True(t, strings.HasPrefix(err.Error(), "s4ql: "), err.Error())
			for _, sentinel := range []error{s4ql.ErrNotSupported, s4ql.ErrNotServing, s4ql.ErrOutcomeUnknown, s4ql.ErrStopped} {
				is.True(t, !errors.Is(err, sentinel), sentinel.Error())
			}
		}

		// The context ends while the commit uploads its commit object, before the manifest swap, so
		// the statement is known not to be committed.
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		s.beforeCreate(cancel)
		_, err := db.ExecContext(ctx, insert1)
		ended(err)
		is.True(t, ctx.Err() != nil, "the commit did not reach the store")

		// database/sql answers a context that has ended already by itself, so the driver is asked
		// directly.
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, conn.Close()) }()
		is.NotError(t, conn.Raw(func(driverConn any) error {
			_, err := driverConn.(driver.ExecerContext).ExecContext(ctx, insert1, nil)
			ended(err)
			_, err = driverConn.(driver.QueryerContext).QueryContext(ctx, select1, nil)
			ended(err)
			// A ping has nothing to wait for on an instance that serves.
			return driverConn.(driver.Pinger).Ping(ctx)
		}))

		// A ping of an instance that does not serve ends with its context, and with that error only.
		down := &faulty{Store: w.client()}
		down.down(true)
		spare, err := w.openOn(t, "b", down, coreOptions()).Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, spare.Close()) }()
		is.NotError(t, spare.Raw(func(driverConn any) error {
			ended(driverConn.(driver.Pinger).Ping(ctx))
			return nil
		}))

		is.NotError(t, db.PingContext(t.Context()))
		is.EqualSlice(t, []int64{}, values(t, db, select1))
		is.Equal(t, 1, len(commits(t, w)))
	})
}

func TestQuery(t *testing.T) {
	t.Run("should give the rows in the order of the key, with the names and the declared types of the columns", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, `create table "Foo" ("Bar x" int primary key) strict, without rowid`,
			"insert into foo (\"bar X\") values (3)", "insert into foo (\"bar X\") values (-1)", "insert into foo (\"bar X\") values (2)")

		rows, err := db.QueryContext(t.Context(), "select * from FOO")
		is.NotError(t, err)
		defer func() { is.NotError(t, rows.Close()) }()
		columns, err := rows.Columns()
		is.NotError(t, err)
		is.EqualSlice(t, []string{"Bar x"}, columns)
		types, err := rows.ColumnTypes()
		is.NotError(t, err)
		is.Equal(t, 1, len(types))
		is.Equal(t, "Bar x", types[0].Name())
		is.Equal(t, "INT", types[0].DatabaseTypeName())

		var got []any
		for rows.Next() {
			var v any
			is.NotError(t, rows.Scan(&v))
			got = append(got, v)
		}
		is.NotError(t, rows.Err())
		is.EqualSlice(t, []any{int64(-1), int64(2), int64(3)}, got)
	})

	t.Run("should give one row to QueryRow, and sql.ErrNoRows for an empty table", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		var v int64
		is.Error(t, sql.ErrNoRows, db.QueryRowContext(t.Context(), select1).Scan(&v))
		exec(t, db, "insert into t1 (v1) values (7)", "insert into t1 (v1) values (8)")
		is.NotError(t, db.QueryRowContext(t.Context(), select1).Scan(&v))
		is.Equal(t, int64(7), v)
		err := db.QueryRowContext(t.Context(), "select * from t2").Scan(&v)
		is.Equal(t, "no such table: t2", failed(t, err).Error())
		readersFree(t, db)
	})

	t.Run("should run a statement that is no query, and give no columns and no rows for it", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		for _, query := range []string{t1, insert1, "create table t2 (v1 int primary key) strict, without rowid", "drop table t2"} {
			rows, err := db.QueryContext(t.Context(), query)
			is.NotError(t, err)
			columns, err := rows.Columns()
			is.NotError(t, err)
			is.Equal(t, 0, len(columns))
			is.True(t, !rows.Next())
			is.NotError(t, rows.Err())
			is.NotError(t, rows.Close())
		}
		is.EqualSlice(t, []int64{1}, values(t, db, select1))
		is.Error(t, sql.ErrNoRows, db.QueryRowContext(t.Context(), "insert into t1 (v1) values (2)").Scan())
		is.EqualSlice(t, []int64{1, 2}, values(t, db, select1))
	})
}

func TestPrepare(t *testing.T) {
	t.Run("should run a prepared statement of each kind, any number of times", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		prepare := func(query string) *sql.Stmt {
			t.Helper()
			s, err := db.PrepareContext(t.Context(), query)
			is.NotError(t, err)
			t.Cleanup(func() { _ = s.Close() })
			return s
		}
		// A statement is prepared before its table exists: the check reads no table.
		create, insert, query, drop := prepare(t1), prepare(insert1), prepare(select1), prepare("drop table t1")

		_, err := query.QueryContext(t.Context())
		is.Equal(t, "no such table: t1", failed(t, err).Error())
		for range 2 {
			res, err := create.ExecContext(t.Context())
			affected(t, res, err)
			res, err = insert.ExecContext(t.Context())
			is.Equal(t, int64(1), affected(t, res, err))
			_, err = insert.ExecContext(t.Context())
			is.Equal(t, "UNIQUE constraint failed: t1.v1", failed(t, err).Error())

			var v int64
			is.NotError(t, query.QueryRowContext(t.Context()).Scan(&v))
			is.Equal(t, int64(1), v)
			rows, err := query.QueryContext(t.Context())
			is.NotError(t, err)
			is.True(t, rows.Next())
			is.True(t, !rows.Next())
			is.NotError(t, rows.Err())
			is.NotError(t, rows.Close())
			_, err = query.ExecContext(t.Context())
			is.NotError(t, err)

			_, err = drop.ExecContext(t.Context())
			is.NotError(t, err)
		}

		_, err = insert.ExecContext(t.Context(), 1)
		is.Equal(t, "parameters", notSupported(t, err))
		_, err = query.QueryContext(t.Context(), "a", "b")
		is.Equal(t, "parameters", notSupported(t, err))
		for _, s := range []*sql.Stmt{create, insert, query, drop} {
			is.NotError(t, s.Close())
		}
		exec(t, db, t1)
		readersFree(t, db)
	})

	t.Run("should give SQLite's syntax error and not supported when the statement is prepared, also on an instance that does not serve", func(t *testing.T) {
		w := newWorld(t)
		s := &faulty{Store: w.client()}
		s.down(true)
		db := w.openOn(t, "a", s, coreOptions())

		_, err := db.PrepareContext(t.Context(), "selec * from t1")
		sqlite, ok := errors.AsType[*s4ql.Error](failed(t, err))
		is.True(t, ok, err.Error())
		is.Equal(t, s4ql.Error{Code: 1, ExtendedCode: 1, Message: `near "selec": syntax error`}, *sqlite)
		_, err = db.PrepareContext(t.Context(), "select * from t1 where v1 = 1")
		is.Equal(t, "WHERE clause (at offset 17)", notSupported(t, err))
		_, err = db.PrepareContext(t.Context(), "insert into t1 (v1) values (?)")
		is.True(t, notSupported(t, err) != "")

		// A statement that is supported is prepared, and refused when it is run.
		prepared, err := db.PrepareContext(t.Context(), insert1)
		is.NotError(t, err)
		defer func() { is.NotError(t, prepared.Close()) }()
		_, err = prepared.ExecContext(t.Context())
		is.Error(t, s4ql.ErrNotServing, failed(t, err))
		_, err = prepared.QueryContext(t.Context())
		is.Error(t, s4ql.ErrNotServing, failed(t, err))
	})
}

func TestConn_Raw(t *testing.T) {
	t.Run("should run the calls of the driver's interfaces that database/sql makes no more, as it runs those with a context", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, conn.Close()) }()

		is.NotError(t, conn.Raw(func(driverConn any) error {
			c := driverConn.(driver.Conn)
			_, err := c.Begin() //nolint:staticcheck // The call is deprecated, and the driver has it.
			is.Equal(t, "transactions", notSupported(t, err))
			_, err = c.Prepare("selec")
			is.Equal(t, `near "selec": syntax error`, failed(t, err).Error())

			insert, err := c.Prepare(insert1)
			is.NotError(t, err)
			is.Equal(t, -1, insert.NumInput())
			res, err := insert.Exec(nil) //nolint:staticcheck // As above.
			is.Equal(t, int64(1), affected(t, res, err))
			_, err = insert.Exec([]driver.Value{int64(1)}) //nolint:staticcheck // As above.
			is.Equal(t, "parameters", notSupported(t, err))
			is.NotError(t, insert.Close())

			query, err := c.Prepare(select1)
			is.NotError(t, err)
			_, err = query.Query([]driver.Value{int64(1)}) //nolint:staticcheck // As above.
			is.Equal(t, "parameters", notSupported(t, err))
			rows, err := query.Query(nil) //nolint:staticcheck // As above.
			is.NotError(t, err)
			is.EqualSlice(t, []string{"v1"}, rows.Columns())
			dest := make([]driver.Value, 1)
			is.NotError(t, rows.Next(dest))
			is.Equal(t, driver.Value(int64(1)), dest[0])
			is.Error(t, io.EOF, rows.Next(dest))
			is.NotError(t, rows.Close())
			return query.Close()
		}))
		readersFree(t, db)
	})
}

func TestClose(t *testing.T) {
	t.Run("should release the lease, so that the next instance serves without waiting for it to run out", func(t *testing.T) {
		w := newWorld(t)
		first := w.serve(t, "a")
		exec(t, first, t1)
		is.NotError(t, first.Close())
		is.NotError(t, first.Close())

		// The lease of the first lasts an hour.
		second := w.serve(t, "b")
		exec(t, second, insert1)
	})

	t.Run("should fail when the lease cannot be released, within the close timeout", func(t *testing.T) {
		w := newWorld(t)
		s := &faulty{Store: w.client()}
		// The instance tries to release the lease for half of the close timeout. A slower machine
		// makes the close fail too.
		o := w.options("a")
		o.CloseTimeout = 100 * time.Millisecond
		db, err := s4ql.OpenStore(t.Context(), w.host, s, o, coreOptions())
		is.NotError(t, err)
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, t1)

		s.down(true)
		err = failed(t, db.Close())
		is.True(t, strings.HasPrefix(err.Error(), "s4ql: close: the lease is not released"), err.Error())
		is.True(t, !errors.Is(err, s4ql.ErrStopped) && !errors.Is(err, s4ql.ErrNotServing), err.Error())

		// The database is closed all the same.
		_, err = db.ExecContext(t.Context(), insert1)
		is.Equal(t, "sql: database is closed", failed(t, err).Error())
		is.NotError(t, db.Close())
	})

	t.Run("should commit an insert if and only if it was acknowledged, when the database is closed while writes and queries run in many goroutines", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)

		const writers, queriers = 4, 8
		var mu sync.Mutex
		// acked are the inserts that succeeded.
		acked := map[int64]bool{}
		var count atomic.Int64
		// some is closed once some inserts are acknowledged, and writing once the writers have all
		// returned, so that the test waits for neither if the other does not come.
		some, writing := make(chan struct{}), make(chan struct{})
		var once sync.Once
		// Each goroutine sends one error at the most.
		errs := make(chan error, writers+queriers)
		var writes, all sync.WaitGroup
		for range writers {
			writes.Go(func() {
				for {
					v := count.Add(1)
					_, err := db.ExecContext(t.Context(), fmt.Sprint("insert into t1 (v1) values (", v, ")"))
					if err != nil {
						if !closedError(err) {
							errs <- fmt.Errorf("the insert of %v: %w", v, err)
						}
						return
					}
					mu.Lock()
					acked[v] = true
					n := len(acked)
					mu.Unlock()
					if n >= 5 {
						once.Do(func() { close(some) })
					}
				}
			})
		}
		all.Go(func() {
			writes.Wait()
			close(writing)
		})
		for g := range queriers {
			all.Go(func() {
				// left are the rows that the goroutine has left open, which it closes before its next
				// query, so that it holds one read at the most.
				var left *sql.Rows
				for {
					if left != nil {
						if err := left.Close(); err != nil {
							errs <- fmt.Errorf("the rows of a query: %w", err)
							return
						}
						left = nil
					}
					rows, err := db.QueryContext(t.Context(), select1)
					if err != nil {
						if !closedError(err) {
							errs <- fmt.Errorf("a query: %w", err)
						}
						return
					}
					last := int64(-1)
					for n := 1; rows.Next(); n++ {
						var v int64
						if err := rows.Scan(&v); err != nil || v <= last {
							errs <- fmt.Errorf("the rows of a query are not in order, or cannot be scanned: %v after %v: %w", v, last, err)
							return
						}
						last = v
						// Every other goroutine stops early.
						if g%2 == 0 && n == 2 {
							break
						}
					}
					err = rows.Err()
					// Every third leaves its rows open, for the close of the database if it comes now.
					if g%3 == 0 {
						left = rows
					} else {
						err = errors.Join(err, rows.Close())
					}
					if err != nil {
						if !closedError(err) {
							errs <- fmt.Errorf("the rows of a query: %w", err)
						}
						return
					}
				}
			})
		}

		select {
		case <-some:
		case <-writing:
		}
		closeErr := db.Close()
		writes.Wait()
		all.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		is.NotError(t, closeErr)
		is.True(t, len(acked) >= 5, fmt.Sprint(len(acked), " inserts are acknowledged"))

		var want []int64
		for v := range acked {
			want = append(want, v)
		}
		slices.Sort(want)
		is.EqualSlice(t, want, values(t, w.serve(t, "b"), select1))
	})

	t.Run("should end the rows that goroutines read while the database is closed, each at its last row or with the error of an instance that was closed", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		for i := range 50 {
			exec(t, db, fmt.Sprint("insert into t1 (v1) values (", i, ")"))
		}

		var wg sync.WaitGroup
		// Each goroutine sends one error at the most.
		errs := make(chan error, readers)
		for range readers {
			rows, err := db.QueryContext(t.Context(), select1)
			is.NotError(t, err)
			wg.Go(func() {
				n := 0
				for rows.Next() {
					n++
				}
				err := errors.Join(rows.Err(), rows.Close())
				switch {
				case err == nil && n != 50:
					errs <- fmt.Errorf("the rows ended without an error after %v of 50 rows", n)
				case err != nil && (!errors.Is(err, s4ql.ErrStopped) || errors.Is(err, driver.ErrBadConn)):
					errs <- fmt.Errorf("the rows of a query: %w", err)
				}
			})
		}
		is.NotError(t, db.Close())
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	})

	t.Run("should refuse a query that comes while the close closes the rows that are open, before the instance has stopped", func(t *testing.T) {
		w := newWorld(t)
		h := &rowHost{Free: w.host}
		db, err := s4ql.OpenStore(t.Context(), h, w.client(), w.options("a"), coreOptions())
		is.NotError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, t1, insert1, "insert into t1 (v1) values (2)")
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, conn.Close()) }()

		// A Next that waits between two rows, which the close waits for before it closes the rows,
		// and so before it closes the instance.
		rows, err := db.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		is.True(t, rows.Next())
		reached, gate := make(chan struct{}), make(chan struct{})
		h.at(func() {
			h.at(nil)
			close(reached)
			<-gate
		})
		read := make(chan error, 1)
		go func() {
			for rows.Next() {
			}
			read <- errors.Join(rows.Err(), rows.Close())
		}()
		<-reached
		closed := make(chan error, 1)
		go func() { closed <- db.Close() }()

		// Queries on the connection that the application holds succeed until the close has begun,
		// and are refused from then on. The bound is far above what the close needs to begin.
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		for {
			r, err := conn.QueryContext(ctx, select1)
			if err != nil {
				is.Error(t, s4ql.ErrStopped, failed(t, err))
				is.Equal(t, "s4ql: the instance has stopped: closed", err.Error())
				break
			}
			is.NotError(t, r.Close())
		}
		// The instance has not stopped: the refusal is the connector's.
		is.NotError(t, conn.PingContext(t.Context()))

		close(gate)
		is.NotError(t, <-closed)
		if err := <-read; err != nil {
			is.Error(t, s4ql.ErrStopped, failed(t, err))
		}
		is.Error(t, s4ql.ErrStopped, failed(t, conn.PingContext(t.Context())))
	})

	t.Run("should commit no insert whose commit waits in the store while the database closes, whether it goes on during the close or after the close has timed out", func(t *testing.T) {
		for _, after := range []bool{false, true} {
			// The clocks are fake, so the close times out only when nothing else can happen.
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				s := &faulty{Store: w.client()}
				db := w.openOn(t, "a", s, coreOptions())
				is.NotError(t, db.PingContext(t.Context()))
				exec(t, db, t1)

				// The insert uploads its commit object and waits there, before the manifest swap.
				reached, gate := make(chan struct{}), make(chan struct{})
				s.beforeCreate(func() {
					close(reached)
					<-gate
				})
				inserted := make(chan error, 1)
				go func() {
					_, err := db.ExecContext(t.Context(), insert1)
					inserted <- err
				}()
				<-reached

				var closeErr error
				if after {
					closeErr = db.Close()
					close(gate)
				} else {
					closed := make(chan error, 1)
					go func() { closed <- db.Close() }()
					// The close has stopped the instance and waits for the insert.
					synctest.Wait()
					close(gate)
					closeErr = <-closed
				}
				err := <-inserted
				is.Error(t, s4ql.ErrStopped, failed(t, err))
				is.True(t, !errors.Is(err, s4ql.ErrOutcomeUnknown), err.Error())
				if after {
					is.True(t, strings.HasPrefix(failed(t, closeErr).Error(), "s4ql: close: a call still uses the cache"), closeErr.Error())
				} else {
					is.NotError(t, closeErr)
				}

				// The lease is released either way. The cache is closed when the insert has ended, which
				// the world checks when the test ends.
				is.EqualSlice(t, []int64{}, values(t, w.serve(t, "b"), select1))
			})
		}
	})
}

// closedError reports whether err is the error of a statement on a database that is closed: that of
// the instance, or that of database/sql. It is never driver.ErrBadConn.
func closedError(err error) bool {
	if errors.Is(err, driver.ErrBadConn) {
		return false
	}
	return errors.Is(err, s4ql.ErrStopped) && strings.HasSuffix(err.Error(), "the instance has stopped: closed") || err.Error() == "sql: database is closed"
}

func TestStop(t *testing.T) {
	t.Run("should answer every statement and every ping with the stop and its cause, after a local failure of the cache", func(t *testing.T) {
		w := newWorld(t)
		vfs, err := faultvfs.New()
		is.NotError(t, err)
		// The VFS is closed after the instance, whose cleanup is registered later.
		t.Cleanup(func() { is.NotError(t, vfs.Close()) })
		co := coreOptions()
		co.VFS = vfs.Name()
		db := w.openOn(t, "a", w.client(), co)
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, t1, insert1, "insert into t1 (v1) values (2)")

		// What is open when the instance stops: the rows of a query, a connection, and a prepared
		// statement.
		rows, err := db.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		is.True(t, rows.Next())
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		prepared, err := db.PrepareContext(t.Context(), "insert into t1 (v1) values (4)")
		is.NotError(t, err)

		// Every write to the write-ahead log of the cache fails from now on. The insert meets the
		// failure: before its commit, and it fails, or in the commit of the cache after the store has
		// taken the commit, and it succeeds. The instance stops either way.
		vfs.Fail(faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.WAL})
		_, insertErr := db.ExecContext(t.Context(), "insert into t1 (v1) values (3)")
		is.True(t, vfs.Failures() > 0, "no write failed")

		stopped := func(err error) {
			t.Helper()
			is.Error(t, s4ql.ErrStopped, failed(t, err))
			is.True(t, strings.HasPrefix(err.Error(), "s4ql: "), err.Error())
			is.True(t, strings.Contains(err.Error(), "the instance has stopped: local I/O failure"), err.Error())
			_, isSQLite := errors.AsType[*s4ql.Error](err)
			is.True(t, !isSQLite, err.Error())
			is.True(t, !errors.Is(err, s4ql.ErrNotServing) && !errors.Is(err, s4ql.ErrOutcomeUnknown) && !errors.Is(err, s4ql.ErrNotSupported), err.Error())
		}
		if insertErr != nil {
			stopped(insertErr)
		}
		for range 2 {
			_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (5)")
			stopped(err)
			_, err = db.ExecContext(t.Context(), select1)
			stopped(err)
			_, err = db.QueryContext(t.Context(), select1)
			stopped(err)
			stopped(db.QueryRowContext(t.Context(), select1).Scan(new(int64)))
			_, err = db.ExecContext(t.Context(), t1)
			stopped(err)
			_, err = db.ExecContext(t.Context(), "drop table t1")
			stopped(err)
			_, err = prepared.ExecContext(t.Context())
			stopped(err)
			_, err = prepared.QueryContext(t.Context())
			stopped(err)
			_, err = conn.ExecContext(t.Context(), insert1)
			stopped(err)
			_, err = conn.QueryContext(t.Context(), select1)
			stopped(err)
			stopped(conn.PingContext(t.Context()))
			stopped(db.PingContext(t.Context()))
			stopped(db.Ping())
		}
		// The rows that were open fail at their next row.
		is.True(t, !rows.Next())
		stopped(rows.Err())
		_ = rows.Close()

		// What the text alone decides does not depend on the instance.
		_, err = db.ExecContext(t.Context(), "selec")
		is.Equal(t, `near "selec": syntax error`, failed(t, err).Error())
		_, err = db.ExecContext(t.Context(), "")
		notSupported(t, err)
		_, err = db.BeginTx(t.Context(), nil)
		notSupported(t, err)
		again, err := db.PrepareContext(t.Context(), select1)
		is.NotError(t, err)
		_, err = again.QueryContext(t.Context())
		stopped(err)

		// Recovery is a normal start: the application closes the database and opens it again, and
		// the insert is there if and only if it was acknowledged.
		is.NotError(t, conn.Close())
		_ = db.Close()
		vfs.Fail(faultvfs.Fault{})
		want := []int64{1, 2}
		if insertErr == nil {
			want = append(want, 3)
		}
		is.EqualSlice(t, want, values(t, w.serve(t, "a"), select1))
	})
}

func TestOutcomeUnknown(t *testing.T) {
	t.Run("should say that the outcome of a commit is unknown when the context ends before the store has answered, and never run the statement again", func(t *testing.T) {
		w := newWorld(t)
		s := &faulty{Store: w.client()}
		db := w.openOn(t, "a", s, coreOptions())
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, t1)

		// The store applies the manifest swap of the commit and loses its answer, the context of the
		// statement ends at that moment, and the store cannot be reached from then on, so the instance
		// cannot find out.
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		s.loseNext(cancel)
		_, err := db.ExecContext(ctx, insert1)
		is.Error(t, s4ql.ErrOutcomeUnknown, failed(t, err))
		is.True(t, strings.HasPrefix(err.Error(), "s4ql: "), err.Error())
		// It matches no other error: every other error means "not committed".
		is.True(t, !errors.Is(err, context.Canceled) && !errors.Is(err, s4ql.ErrNotServing) && !errors.Is(err, s4ql.ErrStopped), err.Error())
		is.True(t, ctx.Err() != nil)

		// The instance serves nothing until it knows.
		_, err = db.QueryContext(t.Context(), select1)
		is.Error(t, s4ql.ErrNotServing, failed(t, err))
		_, err = db.ExecContext(t.Context(), insert1)
		is.Error(t, s4ql.ErrNotServing, failed(t, err))

		// It finds out by itself: the statement is committed, once. Had database/sql run it again,
		// as it does after driver.ErrBadConn, the store would have refused the second run, and the
		// error would not have been that of an unknown outcome.
		s.down(false)
		is.NotError(t, db.PingContext(t.Context()))
		is.EqualSlice(t, []int64{1}, values(t, db, select1))
		// The commit objects are those of the table and of the one insert.
		is.Equal(t, 2, len(commits(t, w)))
	})
}

// some values of t1 on db: the first limit of them, with the rows closed after those, or all of them
// if limit is negative.
func some(ctx context.Context, db *sql.DB, limit int) ([]int64, error) {
	rows, err := db.QueryContext(ctx, select1)
	if err != nil {
		return nil, err
	}
	var got []int64
	for len(got) != limit && rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		got = append(got, v)
	}
	return got, errors.Join(rows.Err(), rows.Close())
}

// commits of the database "db" in the store of w: the keys of its commit objects.
func commits(t *testing.T, w *world) []string {
	t.Helper()
	var keys []string
	for _, key := range w.store.Keys() {
		if key != "db/manifest.json" {
			keys = append(keys, key)
		}
	}
	return keys
}

func TestConcurrent(t *testing.T) {
	t.Run("should run writes and queries from many goroutines, each write once and each query with its rows in the order of the key", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)

		const writers, each = 6, 15
		var wg sync.WaitGroup
		// Each goroutine sends one error at the most.
		errs := make(chan error, 2*writers)
		for g := range writers {
			wg.Go(func() {
				for i := range each {
					if _, err := db.ExecContext(t.Context(), fmt.Sprint("insert into t1 (v1) values (", g*each+i, ")")); err != nil {
						errs <- err
						return
					}
				}
			})
			// More queries at once than the instance has readers.
			wg.Go(func() {
				for range each {
					// Every other goroutine closes its rows early.
					limit := -1
					if g%2 == 0 {
						limit = 3
					}
					got, err := some(t.Context(), db, limit)
					if err != nil {
						errs <- err
						return
					}
					if !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != len(got) {
						errs <- fmt.Errorf("the rows of a query are not in order: %v", got)
						return
					}
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}

		want := make([]int64, writers*each)
		for i := range want {
			want[i] = int64(i)
		}
		is.EqualSlice(t, want, values(t, db, select1))
		readersFree(t, db)
	})
}
