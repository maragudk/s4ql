package s4ql_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/sim"
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

		_, err := db.ExecContext(t.Context(), "select * from t1 group by v1")
		is.Equal(t, "GROUP BY clause (at offset 17)", notSupported(t, err))
		_, err = db.QueryContext(t.Context(), "insert into t1 (v1) values (1), (2)")
		is.True(t, notSupported(t, err) != "")
		_, err = db.QueryContext(t.Context(), insert1+"; "+insert1)
		is.Equal(t, "second statement (at offset 32)", notSupported(t, err))
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

		// The text of a parameter of another form than ?, which SQLite's parser accepts.
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (?1)", 1)
		is.Equal(t, "numbered parameter (at offset 28)", notSupported(t, err))
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (:v)", sql.Named("v", 1))
		is.Equal(t, "named parameter (at offset 28)", notSupported(t, err))
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (-?)", 1)
		is.Equal(t, "parameter after a sign (at offset 29)", notSupported(t, err))

		// Transactions as SQL text, also in a transaction, which stays open.
		texts := []string{"begin", "begin immediate", "commit", "end", "rollback", "savepoint s1", "release s1"}
		for _, src := range texts {
			_, err = db.ExecContext(t.Context(), src)
			is.True(t, notSupported(t, err) != "", src)
		}
		tx, err := db.Begin()
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), insert1)
		is.NotError(t, err)
		for _, src := range texts {
			_, err = tx.ExecContext(t.Context(), src)
			is.True(t, notSupported(t, err) != "", src)
		}
		is.NotError(t, tx.Rollback())

		is.EqualSlice(t, []int64{}, values(t, db, select1))
	})

	t.Run("should judge the text before its arguments, through Exec, Query, and a prepared statement, and run none of it", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)

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
		argss := [][]any{{1}, {1, 2}, {"1", 2}, {sql.Named("v", 1)}, {struct{}{}}, {nil}, {[]string{"a"}}, {uint64(1 << 63)}}

		for name, call := range calls {
			for _, args := range argss {
				// A syntax error gets SQLite's error, also in SQL that s4ql does not support.
				for src, message := range map[string]string{
					"drop table":                       "incomplete input",
					"selec * from t1":                  `near "selec": syntax error`,
					"insert into t1 (v1) values (":     "incomplete input",
					"insert into t1 (v1) values (? ?)": `near "?": syntax error`,
					"select v1 from t1 where":          "incomplete input",
				} {
					err := call(src, args...)
					sqlite, ok := errors.AsType[*s4ql.Error](failed(t, err))
					is.True(t, ok, name, src, err.Error())
					is.Equal(t, s4ql.Error{Code: 1, ExtendedCode: 1, Message: message}, *sqlite, name, src)
				}

				// A text that s4ql does not support gets the "not supported" that it gets without arguments.
				for _, src := range []string{"select * from t1 where v1 = ? or v1 = 2", "select * from t1 group by v1", "insert into t1 (v1) values (?1)", "insert into t1 (v1) values (?), (?)", "begin", "", " ; -- nothing"} {
					err := call(src, args...)
					is.Equal(t, notSupported(t, call(src)), notSupported(t, err), name, src)
				}
			}
		}

		// A prepared statement checks its text when it is prepared, with SQLite's syntax error and
		// "not supported", and its arguments when it is run.
		_, err := db.PrepareContext(t.Context(), "insert into t1 (v1) values (? ?)")
		is.Equal(t, `near "?": syntax error`, failed(t, err).Error())
		_, err = db.PrepareContext(t.Context(), "insert into t1 (v1) values (?1)")
		is.Equal(t, "numbered parameter (at offset 28)", notSupported(t, err))

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
	t.Run("should give the rows in the order of the key or in its reverse, with the names and the declared types of the columns", func(t *testing.T) {
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
		// The query reads forward, which is the order of the key, or backward, as the engine draws it.
		want := []any{int64(-1), int64(2), int64(3)}
		if len(got) > 0 && got[0] == want[2] {
			slices.Reverse(want)
		}
		is.EqualSlice(t, want, got)
	})

	t.Run("should give one row to QueryRow, and sql.ErrNoRows for an empty table", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		var v int64
		is.Error(t, sql.ErrNoRows, db.QueryRowContext(t.Context(), select1).Scan(&v))
		exec(t, db, "insert into t1 (v1) values (7)", "insert into t1 (v1) values (8)")
		is.NotError(t, db.QueryRowContext(t.Context(), select1).Scan(&v))
		// The one row is the first that the query reads, forward or backward.
		is.True(t, v == 7 || v == 8, fmt.Sprint(v))
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
		is.Equal(t, "s4ql: wrong number of arguments: 1 for a statement with 0 parameters", failed(t, err).Error())
		_, err = query.QueryContext(t.Context(), "a", "b")
		is.Equal(t, "s4ql: wrong number of arguments: 2 for a statement with 0 parameters", failed(t, err).Error())
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
		_, err = db.PrepareContext(t.Context(), "select * from t1 where v1 = 1 or v1 = 2")
		is.Equal(t, "OR (at offset 30)", notSupported(t, err))
		_, err = db.PrepareContext(t.Context(), "select * from t1 where v1 = 1 group by v1")
		is.Equal(t, "GROUP BY clause (at offset 30)", notSupported(t, err))
		// A select with WHERE, ORDER BY, and LIMIT that s4ql supports prepares without the instance, and
		// one with a term of ORDER BY or LIMIT that s4ql does not support is refused from the text.
		for _, src := range []string{"select * from t1 where v1 = ? and v1 is not null", "select * from t1 where v1 > ? order by v1 desc limit ?", "select v1 from t1 limit 0"} {
			p, err := db.PrepareContext(t.Context(), src)
			is.NotError(t, err, src)
			is.NotError(t, p.Close())
		}
		_, err = db.PrepareContext(t.Context(), "select * from t1 order by 1")
		is.Equal(t, "ordering term other than a column (at offset 26)", notSupported(t, err))
		_, err = db.PrepareContext(t.Context(), "select * from t1 limit -1")
		is.Equal(t, "negative LIMIT (at offset 23)", notSupported(t, err))
		// A statement with RETURNING is prepared, and one with a column of RETURNING that the pass does
		// not support is refused for that column.
		p, err := db.PrepareContext(t.Context(), "insert into t1 (v1) values (?) returning v1")
		is.NotError(t, err)
		is.NotError(t, p.Close())
		_, err = db.PrepareContext(t.Context(), "delete from t1 returning v1 + 1")
		is.Equal(t, "result column other than a column or * (at offset 25)", notSupported(t, err))
		// A statement with an arithmetic expression is prepared, and one with a token of an expression
		// that the pass does not support is refused for that token.
		for _, src := range []string{"update t1 set v1 = v1 + 1", "select * from t1 where v1 * 2 = ?"} {
			p, err = db.PrepareContext(t.Context(), src)
			is.NotError(t, err, src)
			is.NotError(t, p.Close())
		}
		_, err = db.PrepareContext(t.Context(), "update t1 set v1 = v1 / 0")
		is.Equal(t, "division by zero (at offset 24)", notSupported(t, err))
		// A statement with a subquery is prepared, and one with a token of its subquery that the pass
		// does not support is refused for that token.
		p, err = db.PrepareContext(t.Context(), "select * from t1 where v1 = (select v1 from t1 where v1 > ?)")
		is.NotError(t, err)
		is.NotError(t, p.Close())
		_, err = db.PrepareContext(t.Context(), "delete from t1 where v1 = (select v1, v1 from t1)")
		is.Equal(t, "subquery of more than one column (at offset 38)", notSupported(t, err))
		_, err = db.PrepareContext(t.Context(), "insert into t1 (v1) values ($v)")
		is.Equal(t, "named parameter (at offset 28)", notSupported(t, err))
		_, err = db.PrepareContext(t.Context(), "create table t2 (v2 int(10) primary key) strict, without rowid")
		is.Equal(t, "column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 23)", notSupported(t, err))
		_, err = db.PrepareContext(t.Context(), "insert into t1 (v1) values (1e999)")
		is.Equal(t, "infinite real number (at offset 28)", notSupported(t, err))
		_, err = db.PrepareContext(t.Context(), "create table t2 (v1 int primary key, v2 text null) strict, without rowid")
		is.Equal(t, "NULL constraint (at offset 45)", notSupported(t, err))
		_, err = db.PrepareContext(t.Context(), "insert into t1 (v1) values (-null)")
		is.Equal(t, "NULL after a sign (at offset 29)", notSupported(t, err))
		_, err = db.PrepareContext(t.Context(), "insert into t1 (v1, V1) values (1, 2)")
		is.Equal(t, "column named twice (at offset 20)", notSupported(t, err))
		// An index that s4ql does not support is refused from the text alone, and one that it supports,
		// and its drop, prepare without the instance.
		_, err = db.PrepareContext(t.Context(), "create unique index i on t1 (v1)")
		is.Equal(t, "UNIQUE INDEX (at offset 7)", notSupported(t, err))
		for _, src := range []string{"create index i on t1 (v1)", "drop index i"} {
			p, err := db.PrepareContext(t.Context(), src)
			is.NotError(t, err, src)
			is.NotError(t, p.Close())
		}
		// The type of a column is looked up when the statement runs, and so are the columns.
		for _, src := range []string{
			"create table t2 (v2 real primary key) strict, without rowid", "insert into t1 (v1) values ('a')", "insert into t1 (v1) values (x'')",
			"create table t2 (v1 int primary key, v1 text not null) strict, without rowid", "insert into t1 values (1, 2, 3)", "select v2, v2 from t1",
			// NULL too, which the text alone does not refuse.
			"create table t2 (v1 int primary key, v2 text) strict, without rowid", "insert into t1 (v1) values (null)", "insert into t1 values (?, NULL)",
		} {
			prepared, err := db.PrepareContext(t.Context(), src)
			is.NotError(t, err, src)
			is.NotError(t, prepared.Close())
		}

		// A statement that is supported is prepared, and refused when it is run.
		prepared, err := db.PrepareContext(t.Context(), insert1)
		is.NotError(t, err)
		defer func() { is.NotError(t, prepared.Close()) }()
		_, err = prepared.ExecContext(t.Context())
		is.Error(t, s4ql.ErrNotServing, failed(t, err))
		_, err = prepared.QueryContext(t.Context())
		is.Error(t, s4ql.ErrNotServing, failed(t, err))

		// The arguments are judged before the instance, which refuses only what it would run.
		withParameter, err := db.PrepareContext(t.Context(), "insert into t1 (v1) values (?)")
		is.NotError(t, err)
		defer func() { is.NotError(t, withParameter.Close()) }()
		_, err = withParameter.ExecContext(t.Context())
		is.Equal(t, "s4ql: wrong number of arguments: 0 for a statement with 1 parameter", failed(t, err).Error())
		_, err = withParameter.ExecContext(t.Context(), true)
		is.Equal(t, "argument 1 of type bool", notSupported(t, err))
		_, err = withParameter.ExecContext(t.Context(), "1\x00")
		is.Equal(t, "argument 1, which is text that holds the character NUL", notSupported(t, err))
		// The type of the column needs the instance.
		for _, arg := range []any{1, "1", 1.5, []byte{}} {
			_, err = withParameter.ExecContext(t.Context(), arg)
			is.Error(t, s4ql.ErrNotServing, failed(t, err))
		}
	})
}

// insertParameter is the insert of slice 2, whose value is its one parameter.
const insertParameter = "insert into t1 (v1) values (?)"

// valuer is a [driver.Valuer] of an int64.
type valuer int64

func (v valuer) Value() (driver.Value, error) {
	return int64(v), nil
}

// bigValuer is a [driver.Valuer] of the int64 15, whose own value is an unsigned integer, which may
// be above the largest int64: the value is what Value gives, and not its own.
type bigValuer uint

func (bigValuer) Value() (driver.Value, error) {
	return int64(15), nil
}

// errNoValue is the error of a [failingValuer].
var errNoValue = errors.New("no value for the secret 4815162342")

// failingValuer is a [driver.Valuer] that fails.
type failingValuer struct{}

func (failingValuer) Value() (driver.Value, error) {
	return nil, errNoValue
}

// annotations of the commit objects of the database "db" in the world's store, in the order of the
// log: the text of each statement, which is all that an annotation holds.
func annotations(t *testing.T, w *world) []string {
	t.Helper()
	data, _, ok := w.store.Object("db/manifest.json")
	is.True(t, ok)
	m, err := format.ParseManifest(data)
	is.NotError(t, err)
	var notes []string
	for name := m.Head; name != format.EmptyLog; {
		key := name.String()
		data, _, ok := w.store.Object("db/commits/" + key[:2] + "/" + key)
		is.True(t, ok, key)
		r := format.NewCommitReader(bytes.NewReader(data), name)
		h, err := r.Header()
		is.NotError(t, err)
		var commit []string
		for {
			_, a, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			is.NotError(t, err)
			if a != nil {
				commit = append(commit, a.GetStatement().GetSql())
			}
		}
		notes = append(commit, notes...)
		name, err = format.NameFromBytes(h.GetParent())
		is.NotError(t, err)
	}
	return notes
}

func TestParameters(t *testing.T) {
	t.Run("should insert the argument of a parameter, directly and through a prepared statement run several times", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)

		res, err := db.ExecContext(t.Context(), insertParameter, 1)
		is.Equal(t, int64(1), affected(t, res, err))
		rows, err := db.QueryContext(t.Context(), insertParameter, 2)
		is.NotError(t, err)
		is.True(t, !rows.Next())
		is.NotError(t, rows.Close())

		prepared, err := db.PrepareContext(t.Context(), "INSERT INTO T1 (V1) VALUES ( /* the value */ ? );")
		is.NotError(t, err)
		defer func() { is.NotError(t, prepared.Close()) }()
		for _, v := range []int64{-9223372036854775808, 9223372036854775807, 0, -3} {
			res, err := prepared.ExecContext(t.Context(), v)
			is.Equal(t, int64(1), affected(t, res, err), v)
		}
		rows, err = prepared.QueryContext(t.Context(), 3)
		is.NotError(t, err)
		is.NotError(t, rows.Close())

		is.EqualSlice(t, []int64{-9223372036854775808, -3, 0, 1, 2, 3, 9223372036854775807}, values(t, db, select1))
		readersFree(t, db)
	})

	t.Run("should take an integer of any Go type that fits a signed 64-bit integer, as database/sql converts it", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		type id int16
		args := []any{int(1), int8(-2), int16(3), int32(-4), int64(5), uint(6), uint8(7), uint16(8), uint32(9),
			uint64(9223372036854775807), id(10), new(int64(11)), sql.NullInt64{Int64: 12, Valid: true}, valuer(13), new(uint(14)),
			uint(9223372036854775806), bigValuer(9223372036854775808)}
		for _, a := range args {
			_, err := db.ExecContext(t.Context(), insertParameter, a)
			is.NotError(t, err, fmt.Sprintf("%T", a))
		}
		is.EqualSlice(t, []int64{-4, -2, 1, 3, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 9223372036854775806, 9223372036854775807}, values(t, db, select1))
	})

	t.Run("should give SQLite's errors for a parameter as for a literal", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1, insert1)
		prepared, err := db.PrepareContext(t.Context(), insertParameter)
		is.NotError(t, err)
		defer func() { is.NotError(t, prepared.Close()) }()

		_, literal := db.ExecContext(t.Context(), insert1)
		sqlite, ok := errors.AsType[*s4ql.Error](failed(t, literal))
		is.True(t, ok)
		is.Equal(t, s4ql.Error{Code: 19, ExtendedCode: 1555, Message: "UNIQUE constraint failed: t1.v1"}, *sqlite)
		for _, call := range []func() error{
			func() error { _, err := db.ExecContext(t.Context(), insertParameter, 1); return err },
			func() error { _, err := db.QueryContext(t.Context(), insertParameter, 1); return err },
			func() error { _, err := prepared.ExecContext(t.Context(), 1); return err },
		} {
			err := call()
			got, ok := errors.AsType[*s4ql.Error](failed(t, err))
			is.True(t, ok, err.Error())
			is.Equal(t, *sqlite, *got)
		}

		for src, message := range map[string]string{
			"insert into t2 (v1) values (?)": "no such table: t2",
			"insert into t1 (v2) values (?)": "table t1 has no column named v2",
		} {
			_, err := db.ExecContext(t.Context(), src, 1)
			got, ok := errors.AsType[*s4ql.Error](failed(t, err))
			is.True(t, ok, err.Error())
			is.Equal(t, s4ql.Error{Code: 1, ExtendedCode: 1, Message: message}, *got)
		}
		is.EqualSlice(t, []int64{1}, values(t, db, select1))
	})

	t.Run("should refuse another number of arguments than parameters, directly and prepared, and run nothing", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		cases := []struct {
			src  string
			args []any
			want string
		}{
			{src: insertParameter, want: "s4ql: wrong number of arguments: 0 for a statement with 1 parameter"},
			{src: insertParameter, args: []any{1, 2}, want: "s4ql: wrong number of arguments: 2 for a statement with 1 parameter"},
			// The number is judged before what each argument is.
			{src: insertParameter, args: []any{"1", 2.5}, want: "s4ql: wrong number of arguments: 2 for a statement with 1 parameter"},
			{src: insert1, args: []any{1}, want: "s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
			{src: select1, args: []any{1}, want: "s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
			{src: select1, args: []any{"x"}, want: "s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
			{src: "drop table t1", args: []any{1, 2}, want: "s4ql: wrong number of arguments: 2 for a statement with 0 parameters"},
			{src: "create table t2 (v2 int primary key) strict, without rowid", args: []any{1, 2, 3}, want: "s4ql: wrong number of arguments: 3 for a statement with 0 parameters"},
			// SQLite would give that the table does not exist; the arguments are judged first.
			{src: "insert into t9 (v1) values (?)", want: "s4ql: wrong number of arguments: 0 for a statement with 1 parameter"},
		}
		for _, c := range cases {
			prepared, err := db.PrepareContext(t.Context(), c.src)
			is.NotError(t, err)
			for name, call := range map[string]func() error{
				"Exec":          func() error { _, err := db.ExecContext(t.Context(), c.src, c.args...); return err },
				"Query":         func() error { _, err := db.QueryContext(t.Context(), c.src, c.args...); return err },
				"QueryRow":      func() error { return db.QueryRowContext(t.Context(), c.src, c.args...).Scan(new(int64)) },
				"prepared Exec": func() error { _, err := prepared.ExecContext(t.Context(), c.args...); return err },
				"prepared Query": func() error {
					_, err := prepared.QueryContext(t.Context(), c.args...)
					return err
				},
				"prepared QueryRow": func() error { return prepared.QueryRowContext(t.Context(), c.args...).Scan(new(int64)) },
			} {
				err := call()
				is.Equal(t, c.want, failed(t, err).Error(), name, c.src)
				_, isSQLite := errors.AsType[*s4ql.Error](err)
				is.True(t, !isSQLite, name, c.src)
				for _, sentinel := range []error{s4ql.ErrNotSupported, s4ql.ErrNotServing, s4ql.ErrOutcomeUnknown, s4ql.ErrStopped} {
					is.True(t, !errors.Is(err, sentinel), name, c.src)
				}
			}
			is.NotError(t, prepared.Close())
		}
		// Nothing was committed after the create table of t1, which still holds no row.
		is.EqualSlice(t, []int64{}, values(t, db, select1))
		is.Equal(t, 1, len(commits(t, w)))
	})

	t.Run("should refuse an argument that no column holds, and a named argument, as not supported, and run nothing", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		prepared, err := db.PrepareContext(t.Context(), insertParameter)
		is.NotError(t, err)
		defer func() { is.NotError(t, prepared.Close()) }()
		type flag bool
		cases := []struct {
			arg  any
			want string
		}{
			// A nil []byte does not say whether it means NULL or the empty blob. database/sql hands the
			// driver one also for a pointer to one, and for a valid sql.Null of one.
			{arg: []byte(nil), want: "argument 1 of type []byte, which is nil"},
			{arg: new([]byte), want: "argument 1 of type []byte, which is nil"},
			{arg: sql.Null[[]byte]{Valid: true}, want: "argument 1 of type []byte, which is nil"},
			{arg: sql.RawBytes(nil), want: "argument 1 of type []byte, which is nil"},
			{arg: true, want: "argument 1 of type bool"},
			{arg: flag(true), want: "argument 1 of type bool"},
			{arg: time.Time{}, want: "argument 1 of type time.Time"},
			{arg: math.NaN(), want: "argument 1, which is a real that is not a number"},
			{arg: float32(math.Inf(1)), want: "argument 1, which is an infinite real"},
			{arg: math.Inf(-1), want: "argument 1, which is an infinite real"},
			{arg: "a\x00", want: "argument 1, which is text that holds the character NUL"},
			{arg: "\xff", want: "argument 1, which is text that is not valid UTF-8"},
			{arg: uint64(9223372036854775808), want: "argument 1 of type uint64, whose value does not fit a signed 64-bit integer"},
			// database/sql's conversion would make these a negative int64.
			{arg: uint(9223372036854775808), want: "argument 1 of type uint, whose value does not fit a signed 64-bit integer"},
			{arg: new(uint(18446744073709551615)), want: "argument 1 of type *uint, whose value does not fit a signed 64-bit integer"},
			{arg: uintptr(9223372036854775808), want: "argument 1 of type uintptr, whose value does not fit a signed 64-bit integer"},
			{arg: struct{}{}, want: "argument 1 of type struct {}, which database/sql cannot convert"},
			{arg: failingValuer{}, want: "argument 1 of type s4ql_test.failingValuer, which database/sql cannot convert"},
			{arg: sql.Named("v", 1), want: `the named argument "v"`},
		}
		for _, c := range cases {
			_, err := db.ExecContext(t.Context(), insertParameter, c.arg)
			is.Equal(t, c.want, notSupported(t, err), fmt.Sprintf("%T", c.arg))
			_, err = db.QueryContext(t.Context(), insertParameter, c.arg)
			is.Equal(t, c.want, notSupported(t, err), fmt.Sprintf("%T", c.arg))
			_, err = prepared.ExecContext(t.Context(), c.arg)
			is.Equal(t, c.want, notSupported(t, err), fmt.Sprintf("%T", c.arg))
		}

		// The error of a driver.Valuer is the cause, and its text is not in the message.
		_, err = db.ExecContext(t.Context(), insertParameter, failingValuer{})
		is.Error(t, errNoValue, err)
		is.Error(t, s4ql.ErrNotSupported, err)
		is.True(t, !strings.Contains(err.Error(), "4815162342"), err.Error())
		is.EqualSlice(t, []int64{}, values(t, db, select1))
		is.Equal(t, 1, len(commits(t, w)))
	})

	t.Run("should annotate the commit with the text alone, and give the same rows from an empty cache after a restart", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		_, err := db.ExecContext(t.Context(), insertParameter, -9223372036854775808)
		is.NotError(t, err)
		prepared, err := db.PrepareContext(t.Context(), "insert into t1 (v1) values ( ? ) -- after")
		is.NotError(t, err)
		for _, v := range []int64{7, 9223372036854775807} {
			_, err = prepared.ExecContext(t.Context(), v)
			is.NotError(t, err)
		}
		_, err = prepared.ExecContext(t.Context(), 7)
		is.Equal(t, "UNIQUE constraint failed: t1.v1", failed(t, err).Error())
		is.NotError(t, prepared.Close())
		exec(t, db, "insert into t1 (v1) values (1)")
		is.EqualSlice(t, []string{
			t1,
			"insert into t1 (v1) values (?)",
			"insert into t1 (v1) values ( ? )",
			"insert into t1 (v1) values ( ? )",
			"insert into t1 (v1) values (1)",
		}, annotations(t, w))
		want := values(t, db, select1)
		is.EqualSlice(t, []int64{-9223372036854775808, 1, 7, 9223372036854775807}, want)
		is.NotError(t, db.Close())

		db = w.serve(t, "b")
		is.EqualSlice(t, want, values(t, db, select1))
		_, err = db.ExecContext(t.Context(), insertParameter, 7)
		is.Equal(t, "UNIQUE constraint failed: t1.v1", failed(t, err).Error())
	})
}

// blobValuer is a [driver.Valuer] that gives its own bytes, which [driver.DefaultParameterConverter]
// passes on as they are.
type blobValuer []byte

func (v blobValuer) Value() (driver.Value, error) {
	return []byte(v), nil
}

// rawBytes is a type whose kind is a slice of bytes, which [driver.DefaultParameterConverter]
// converts to a []byte over the same array.
type rawBytes []byte

func TestArgument(t *testing.T) {
	t.Run("should give the blob that the insert stored in the row of its RETURNING, also when the caller changes the bytes of the argument before it reads the row", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, "create table r (k int primary key, b blob) strict, without rowid")

		b := []byte{1}
		rows, err := db.QueryContext(t.Context(), "insert into r values (1, ?) returning b", b)
		is.NotError(t, err)
		defer func() { _ = rows.Close() }()
		// The statement has run and is committed. The bytes are the caller's again, such as a buffer
		// that it fills for its next statement.
		b[0] = 2

		is.True(t, rows.Next(), "the insert gives a row")
		var returned []byte
		is.NotError(t, rows.Scan(&returned))
		is.NotError(t, rows.Close())

		var stored []byte
		is.NotError(t, db.QueryRowContext(t.Context(), "select b from r where k = 1").Scan(&stored))
		is.EqualSlice(t, []byte{1}, stored, "the row that is stored")
		is.EqualSlice(t, []byte{1}, returned, "the row that RETURNING gives")
	})

	t.Run("should give the rows of a query for the blob that its argument was when the query ran, directly, prepared, and in a transaction, also when the caller changes the bytes before it reads the rows", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, "create table q (k blob primary key, v int) strict, without rowid", "insert into q values (x'01', 10)", "insert into q values (x'02', 20)")
		const src = "select v from q where k = ?"
		prepared, err := db.PrepareContext(t.Context(), src)
		is.NotError(t, err)
		defer func() { is.NotError(t, prepared.Close()) }()

		for _, conv := range []struct {
			name string
			arg  func([]byte) any
		}{
			{"a []byte", func(k []byte) any { return k }},
			{"a type whose kind is a slice of bytes", func(k []byte) any { return rawBytes(k) }},
			{"a sql.RawBytes", func(k []byte) any { return sql.RawBytes(k) }},
			{"a driver.Valuer of a []byte", func(k []byte) any { return blobValuer(k) }},
		} {
			for _, how := range []struct {
				name string
				run  func(tx *sql.Tx, k any) (*sql.Rows, error)
			}{
				{"directly", func(_ *sql.Tx, k any) (*sql.Rows, error) { return db.QueryContext(t.Context(), src, k) }},
				{"prepared", func(_ *sql.Tx, k any) (*sql.Rows, error) { return prepared.QueryContext(t.Context(), k) }},
				{"in a transaction", func(tx *sql.Tx, k any) (*sql.Rows, error) { return tx.QueryContext(t.Context(), src, k) }},
				{"prepared in a transaction", func(tx *sql.Tx, k any) (*sql.Rows, error) {
					return tx.StmtContext(t.Context(), prepared).QueryContext(t.Context(), k)
				}},
			} {
				name := conv.name + " " + how.name
				tx := begin(t, db, nil)
				k := []byte{1}
				rows, err := how.run(tx, conv.arg(k))
				is.NotError(t, err, name)
				k[0] = 2

				var got []int64
				for rows.Next() {
					var v int64
					is.NotError(t, rows.Scan(&v), name)
					got = append(got, v)
				}
				is.NotError(t, rows.Err(), name)
				is.NotError(t, rows.Close(), name)
				is.NotError(t, tx.Rollback(), name)
				is.EqualSlice(t, []int64{10}, got, name)
			}
		}
	})

	t.Run("should commit the blobs that the arguments were when each write ran, to the cache and to the commit object, also when the caller changes the bytes before the commit", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, "create table r (k int primary key, b blob) strict, without rowid")
		prepared, err := db.PrepareContext(t.Context(), "insert into r values (?, ?)")
		is.NotError(t, err)
		defer func() { is.NotError(t, prepared.Close()) }()

		tx := begin(t, db, nil)
		b := []byte{1}
		_, err = tx.ExecContext(t.Context(), "insert into r values (1, ?)", b)
		is.NotError(t, err)
		b[0] = 9
		b = []byte{2}
		_, err = tx.StmtContext(t.Context(), prepared).ExecContext(t.Context(), 2, rawBytes(b))
		is.NotError(t, err)
		b[0] = 9
		b, c := []byte{3}, []byte{4}
		_, err = tx.ExecContext(t.Context(), "insert into r values (3, ?); insert into r values (4, ?)", sql.RawBytes(b), blobValuer(c))
		is.NotError(t, err)
		b[0], c[0] = 9, 9
		b = []byte{5}
		rows, err := tx.QueryContext(t.Context(), "insert into r values (5, ?) returning b", b)
		is.NotError(t, err)
		b[0] = 9
		is.True(t, rows.Next())
		var returned []byte
		is.NotError(t, rows.Scan(&returned))
		is.NotError(t, rows.Close())
		is.EqualSlice(t, []byte{5}, returned, "the row that RETURNING gives")
		is.NotError(t, tx.Commit())

		const want = "k INT, b BLOB: 1 [1]; 2 [2]; 3 [3]; 4 [4]; 5 [5]"
		is.Equal(t, want, rowsText(t, db, "select * from r"), "the cache")
		is.EqualSlice(t, []string{
			"create table r (k int primary key, b blob) strict, without rowid",
			"insert into r values (1, ?)",
			"insert into r values (?, ?)",
			"insert into r values (3, ?);",
			"insert into r values (4, ?)",
			"insert into r values (5, ?) returning b",
		}, annotations(t, w), "the commit object")
		is.NotError(t, db.Close())
		is.Equal(t, want, rowsText(t, w.serve(t, "b"), "select * from r"), "the cache of another instance")
	})
}

// typedTables are a table of each of the five types of a column, each named by its type with an "s".
var typedTables = []string{
	"create table ints (v int primary key) strict, without rowid",
	"create table integers (v integer primary key) strict, without rowid",
	"create table reals (v real primary key) strict, without rowid",
	"create table texts (v text primary key) strict, without rowid",
	"create table blobs (v blob primary key) strict, without rowid",
}

// rowKeys of the rows vs, each a row of one value, as database/sql gives it to an any: the texts of
// the values in ascending order, where the text of a value is its Go type and its Go syntax, which
// no two different values share: 0.0 and -0.0, nil and a text, and a blob and the text of its bytes
// each have two. Two lists of rows have the same keys only if they hold the same rows, each as
// often, so a test that compares the keys of two lists element by element compares the rows as a
// multiset, in which a wrong row, a missing row, and a row that is there twice all show. A query
// without ORDER BY gives its rows forward or backward, as the engine draws it, and a test of the
// driver cannot fix the direction.
func rowKeys(vs []any) []string {
	keys := make([]string, len(vs))
	for i, v := range vs {
		keys[i] = fmt.Sprintf("%T %#v", v, v)
	}
	slices.Sort(keys)
	return keys
}

// rowsOf the query src on db: each row as its one value, as database/sql gives it to an any, in the
// order in which the rows came.
func rowsOf(t *testing.T, db querier, src string) []any {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), src)
	is.NotError(t, err)
	vs := []any{}
	for rows.Next() {
		var v any
		is.NotError(t, rows.Scan(&v))
		vs = append(vs, v)
	}
	is.NotError(t, rows.Err())
	is.NotError(t, rows.Close())
	return vs
}

func TestTypes(t *testing.T) {
	t.Run("should insert and give values of each type, through literals and arguments of the Go types that database/sql converts", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, typedTables...)
		type name string
		type number float64
		type bytes []byte
		cases := []struct {
			table   string
			literal string
			args    []any
			want    []any
		}{
			{table: "ints", literal: "-9223372036854775808", args: []any{int8(1), uint32(2), 9223372036854775807}, want: []any{int64(-9223372036854775808), int64(1), int64(2), int64(9223372036854775807)}},
			{table: "integers", literal: "0", args: []any{-1, int64(7)}, want: []any{int64(-1), int64(0), int64(7)}},
			{table: "reals", literal: "1.5", args: []any{float32(0.5), number(-2.25), math.MaxFloat64, math.SmallestNonzeroFloat64, sql.NullFloat64{Float64: 3, Valid: true}},
				want: []any{-2.25, 4.9e-324, 0.5, 1.5, 3.0, math.MaxFloat64}},
			{table: "texts", literal: "'it''s'", args: []any{"", name("日本"), "á", sql.NullString{String: "b", Valid: true}, new("c")},
				want: []any{"", "á", "b", "c", "it's", "日本"}},
			{table: "blobs", literal: "x'00'", args: []any{[]byte{}, bytes{0, 0}, []byte("ab"), new(bytes("1"))},
				want: []any{[]byte{}, []byte{0}, []byte{0, 0}, []byte("1"), []byte("ab")}},
		}
		for _, c := range cases {
			exec(t, db, "insert into "+c.table+" (v) values ("+c.literal+")")
			prepared, err := db.PrepareContext(t.Context(), "insert into "+c.table+" (v) values (?)")
			is.NotError(t, err)
			for i, a := range c.args {
				var res sql.Result
				if i%2 == 0 {
					res, err = db.ExecContext(t.Context(), "insert into "+c.table+" (v) values (?)", a)
				} else {
					res, err = prepared.ExecContext(t.Context(), a)
				}
				is.Equal(t, int64(1), affected(t, res, err), fmt.Sprintf("%T", a))
			}
			is.NotError(t, prepared.Close())
			got := rowsOf(t, db, "select * from "+c.table)
			is.EqualSlice(t, rowKeys(c.want), rowKeys(got), c.table)
		}
		// Each type as declared, INT and INTEGER apart, also after a replay of the log into an empty
		// cache.
		check := func(db *sql.DB) {
			t.Helper()
			for _, typ := range []string{"INT", "INTEGER", "REAL", "TEXT", "BLOB"} {
				rows, err := db.QueryContext(t.Context(), "select * from "+typ+"s")
				is.NotError(t, err)
				types, err := rows.ColumnTypes()
				is.NotError(t, err)
				is.Equal(t, typ, types[0].DatabaseTypeName())
				is.NotError(t, rows.Close())
			}
			is.EqualSlice(t, rowKeys(cases[4].want), rowKeys(rowsOf(t, db, "select * from blobs")))
			is.EqualSlice(t, rowKeys(cases[2].want), rowKeys(rowsOf(t, db, "select * from reals")))
		}
		check(db)
		is.NotError(t, db.Close())
		check(w.serve(t, "b"))
	})

	t.Run("should refuse a value of another type than its column as not supported, after SQLite's errors of the names, and run nothing", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, typedTables...)
		cases := []struct {
			src  string
			args []any
			want string
		}{
			// SQLite would convert these.
			{src: "insert into ints (v) values ('1')", want: "a string literal for the column ints.v of type INT"},
			{src: "insert into integers (v) values (?)", args: []any{"1"}, want: "argument 1, which is text, for the column integers.v of type INTEGER"},
			{src: "insert into ints (v) values (1.0)", want: "a real literal for the column ints.v of type INT"},
			{src: "insert into reals (v) values (1)", want: "an integer literal for the column reals.v of type REAL"},
			{src: "insert into reals (v) values (?)", args: []any{1}, want: "argument 1, which is an integer, for the column reals.v of type REAL"},
			{src: "insert into texts (v) values (1.5)", want: "a real literal for the column texts.v of type TEXT"},
			{src: "insert into texts (v) values (?)", args: []any{2}, want: "argument 1, which is an integer, for the column texts.v of type TEXT"},
			// SQLite would give an error of its own for these.
			{src: "insert into ints (v) values ('a')", want: "a string literal for the column ints.v of type INT"},
			{src: "insert into blobs (v) values ('a')", want: "a string literal for the column blobs.v of type BLOB"},
			{src: "insert into blobs (v) values (?)", args: []any{"a"}, want: "argument 1, which is text, for the column blobs.v of type BLOB"},
			{src: "insert into texts (v) values (x'61')", want: "a blob literal for the column texts.v of type TEXT"},
			{src: "insert into texts (v) values (?)", args: []any{[]byte("a")}, want: "argument 1, which is a blob, for the column texts.v of type TEXT"},
			{src: `insert into "REALS" (V) values (?)`, args: []any{"1.5"}, want: "argument 1, which is text, for the column reals.v of type REAL"},
		}
		for _, c := range cases {
			_, err := db.ExecContext(t.Context(), c.src, c.args...)
			is.Equal(t, c.want, notSupported(t, err), c.src)
			_, err = db.QueryContext(t.Context(), c.src, c.args...)
			is.Equal(t, c.want, notSupported(t, err), c.src)
		}
		_, err := db.ExecContext(t.Context(), "insert into nosuch (v) values (?)", "a")
		sqliteError(t, 1, 1, "no such table: nosuch", err)
		_, err = db.ExecContext(t.Context(), "insert into ints (w) values ('a')")
		sqliteError(t, 1, 1, "table ints has no column named w", err)
		is.Equal(t, len(typedTables), len(commits(t, w)))
	})

	t.Run("should store -0.0 as 0.0, give it back as 0.0, take it for the key 0.0, and annotate the statement by its text", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, typedTables[2])
		_, err := db.ExecContext(t.Context(), "insert into reals (v) values (?)", math.Copysign(0, -1))
		is.NotError(t, err)
		var v float64
		is.NotError(t, db.QueryRowContext(t.Context(), "select * from reals").Scan(&v))
		is.True(t, v == 0 && !math.Signbit(v), fmt.Sprint(v))
		for _, src := range []string{"insert into reals (v) values (0.0)", "insert into reals (v) values (-0.0)"} {
			_, err = db.ExecContext(t.Context(), src)
			sqliteError(t, 19, 1555, "UNIQUE constraint failed: reals.v", err)
		}
		_, err = db.ExecContext(t.Context(), "insert into reals (v) values (?)", 0.0)
		sqliteError(t, 19, 1555, "UNIQUE constraint failed: reals.v", err)
		is.EqualSlice(t, []string{typedTables[2], "insert into reals (v) values (?)"}, annotations(t, w))
		is.NotError(t, db.Close())
		is.NotError(t, w.serve(t, "b").QueryRowContext(t.Context(), "select * from reals").Scan(&v))
		is.True(t, v == 0 && !math.Signbit(v), fmt.Sprint(v))
	})

	t.Run("should give SQLite's error of a key that exists for every type", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, typedTables...)
		for table, arg := range map[string]any{"ints": 1, "integers": -1, "reals": 1.5, "texts": "é", "blobs": []byte{}} {
			src := "insert into " + table + " (v) values (?)"
			_, err := db.ExecContext(t.Context(), src, arg)
			is.NotError(t, err)
			res, err := db.ExecContext(t.Context(), src, arg)
			sqliteError(t, 19, 1555, "UNIQUE constraint failed: "+table+".v", err)
			is.True(t, res == nil)
		}
	})

	t.Run("should give each application a []byte of its own", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, typedTables[4], "insert into blobs (v) values (x'6162')")
		var b []byte
		is.NotError(t, db.QueryRowContext(t.Context(), "select * from blobs").Scan(&b))
		b[0] = 'x'
		var raw any
		is.NotError(t, db.QueryRowContext(t.Context(), "select * from blobs").Scan(&raw))
		is.Equal(t, "ab", string(raw.([]byte)))
		raw.([]byte)[0] = 'y'

		rows, err := db.QueryContext(t.Context(), "select * from blobs")
		is.NotError(t, err)
		is.True(t, rows.Next())
		var r sql.RawBytes
		is.NotError(t, rows.Scan(&r))
		is.Equal(t, "ab", string(r))
		is.NotError(t, rows.Close())
	})

	t.Run("should give SQLite's error for a text or a blob that is too long, and keep the transaction usable", func(t *testing.T) {
		const limit = 1000
		w := newWorld(t)
		co := coreOptions()
		co.LengthLimit = limit
		db := w.openOn(t, "a", w.client(), co)
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, typedTables[3], typedTables[4])
		for _, arg := range []any{strings.Repeat("a", limit+1), make([]byte, limit+1), make([]byte, limit)} {
			_, err := db.ExecContext(t.Context(), "insert into blobs (v) values (?)", arg)
			sqliteError(t, 18, 18, "string or blob too big", err)
		}
		_, err := db.ExecContext(t.Context(), "insert into ints (v) values (?)", strings.Repeat("a", limit+1))
		sqliteError(t, 1, 1, "no such table: ints", err)

		tx := begin(t, db, nil)
		_, err = tx.ExecContext(t.Context(), "insert into texts (v) values ('a')")
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), "insert into texts (v) values (?)", strings.Repeat("b", limit+1))
		sqliteError(t, 18, 18, "string or blob too big", err)
		_, err = tx.ExecContext(t.Context(), "insert into texts (v) values (?)", strings.Repeat("c", limit))
		sqliteError(t, 18, 18, "string or blob too big", err)
		_, err = tx.ExecContext(t.Context(), "insert into texts (v) values (?)", strings.Repeat("d", limit-10))
		is.NotError(t, err)
		is.NotError(t, tx.Commit())
		is.EqualSlice(t, rowKeys([]any{"a", strings.Repeat("d", limit-10)}), rowKeys(rowsOf(t, db, "select * from texts")))
	})

	t.Run("should run values of every type in a read-only transaction, and undo them at a roll back", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, typedTables[3], "insert into texts (v) values ('a')")
		ro := begin(t, db, &sql.TxOptions{ReadOnly: true})
		is.EqualSlice(t, rowKeys([]any{"a"}), rowKeys(rowsOf(t, ro, "select * from texts")))
		for _, arg := range []any{"b", 1} {
			_, err := ro.ExecContext(t.Context(), "insert into texts (v) values (?)", arg)
			sqliteError(t, 8, 8, "attempt to write a readonly database", err)
		}
		is.NotError(t, ro.Commit())

		tx := begin(t, db, nil)
		_, err := tx.ExecContext(t.Context(), "insert into texts (v) values (?)", "b")
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), "insert into texts (v) values (?)", 1)
		is.Equal(t, "argument 1, which is an integer, for the column texts.v of type TEXT", notSupported(t, err))
		is.EqualSlice(t, rowKeys([]any{"a", "b"}), rowKeys(rowsOf(t, tx, "select * from texts")))
		is.NotError(t, tx.Rollback())
		is.EqualSlice(t, rowKeys([]any{"a"}), rowKeys(rowsOf(t, db, "select * from texts")))
	})
}

// t3 is the table of the example of slice 5: three columns of three types, with the key first.
const t3 = "create table t3 (v1 int primary key, v2 text not null, v3 real not null) strict, without rowid"

// rowsText of the rows of the query src on db, which must succeed: its column names, its declared
// types, and each row as its values, with the rows in the order of their texts, each as often as the
// query gives it: the rows as a multiset, since a query without ORDER BY gives its rows forward or
// backward, as the engine draws it, and a test of the driver cannot fix the direction.
func rowsText(t *testing.T, db querier, src string, args ...any) string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), src, args...)
	is.NotError(t, err, src)
	columns, err := rows.Columns()
	is.NotError(t, err)
	types, err := rows.ColumnTypes()
	is.NotError(t, err)
	var names []string
	for i, c := range columns {
		is.Equal(t, c, types[i].Name())
		names = append(names, c+" "+types[i].DatabaseTypeName())
	}
	var rs []string
	for rows.Next() {
		vs := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range vs {
			dest[i] = &vs[i]
		}
		is.NotError(t, rows.Scan(dest...))
		rs = append(rs, strings.TrimSuffix(fmt.Sprintln(vs...), "\n"))
	}
	is.NotError(t, rows.Err())
	is.NotError(t, rows.Close())
	slices.Sort(rs)
	return strings.Join(names, ", ") + ": " + strings.Join(rs, "; ")
}

func TestColumns(t *testing.T) {
	t.Run("should insert rows of several values, with and without a list of columns, and give named columns in any order, with the names as the table stores them", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t3, "insert into t3 values (2, 'b', 2.5)", "insert into T3 (V3, v1, \"V2\") values (1.5, 1, 'a')")
		res, err := db.ExecContext(t.Context(), "insert into t3 (v1, v2, v3) values (?, ?, ?)", 4, "d", 4.5)
		is.Equal(t, int64(1), affected(t, res, err))
		prepared, err := db.PrepareContext(t.Context(), "insert into t3 (v2, v3, v1) values (?, ?, ?)")
		is.NotError(t, err)
		_, err = prepared.ExecContext(t.Context(), "c", 3.5, 3)
		is.NotError(t, err)
		_, err = prepared.ExecContext(t.Context(), "z", 9.5, 3)
		sqliteError(t, 19, 1555, "UNIQUE constraint failed: t3.v1", err)
		is.NotError(t, prepared.Close())

		want := "v1 INT, v2 TEXT, v3 REAL: 1 a 1.5; 2 b 2.5; 3 c 3.5; 4 d 4.5"
		is.Equal(t, want, rowsText(t, db, "select * from t3"))
		is.Equal(t, "v2 TEXT, v1 INT, v2 TEXT: a 1 a; b 2 b; c 3 c; d 4 d", rowsText(t, db, `select V2, "V1", v2 from T3`))
		var v2 string
		var v3 float64
		is.NotError(t, db.QueryRowContext(t.Context(), "select v2, v3 from t3").Scan(&v2, &v3))
		// The one row is the first that the query reads, forward or backward.
		one := fmt.Sprint(v2, " ", v3)
		is.True(t, one == "a 1.5" || one == "d 4.5", one)

		is.EqualSlice(t, []string{
			t3, "insert into t3 values (2, 'b', 2.5)", `insert into T3 (V3, v1, "V2") values (1.5, 1, 'a')`,
			`insert into t3 (v1, v2, v3) values (?, ?, ?)`,
			`insert into t3 (v2, v3, v1) values (?, ?, ?)`,
		}, annotations(t, w))
		is.NotError(t, db.Close())
		is.Equal(t, want, rowsText(t, w.serve(t, "b"), "select * from t3"))
	})

	t.Run("should give SQLite's errors of several columns when the statement runs, also prepared, and set the count of changes as SQLite does", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t3, "insert into t3 values (1, 'a', 1.5)")
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, conn.Close()) }()
		cases := []struct {
			src     string
			code    int
			ext     int
			message string
		}{
			{"insert into t3 values (2, 'b')", 1, 1, "table t3 has 3 columns but 2 values were supplied"},
			{"insert into t3 (v1, v2) values (2, 'b', 2.5)", 1, 1, "3 values for 2 columns"},
			{"insert into t3 (v1, x) values (2)", 1, 1, "table t3 has no column named x"},
			{"insert into t3 (v2, v3) values ('b', 2.5)", 19, 1299, "NOT NULL constraint failed: t3.v1"},
			{"insert into t3 (v1, v3) values (2, 'x')", 19, 1299, "NOT NULL constraint failed: t3.v2"},
			{"insert into t3 values (1, 'b', 2.5)", 19, 1555, "UNIQUE constraint failed: t3.v1"},
			{"select v1, x from t3", 1, 1, "no such column: x"},
			{`select "x" from t3`, 1, 1, `no such column: "x" - should this be a string literal in single-quotes?`},
			{"create table t3 (v int primary key, v text not null) strict, without rowid", 1, 1, "table t3 already exists"},
			{"create table t4 (v int primary key, V text not null) strict, without rowid", 1, 1, "duplicate column name: V"},
		}
		for i, c := range cases {
			// The count of changes is 1 before each, and a constraint that fails sets it to 0, as in
			// SQLite.
			_, err := conn.ExecContext(t.Context(), fmt.Sprint("insert into t3 values (", 100+i, ", 'x', 0.5)"))
			is.NotError(t, err)
			prepared, err := conn.PrepareContext(t.Context(), c.src)
			is.NotError(t, err, "Prepare checks only the text: "+c.src)
			_, err = prepared.ExecContext(t.Context())
			sqliteError(t, c.code, c.ext, c.message, err)
			is.NotError(t, prepared.Close())
			res, err := conn.ExecContext(t.Context(), "select * from t3")
			want := int64(1)
			if c.code == 19 {
				want = 0
			}
			is.Equal(t, want, affected(t, res, err), c.src)
		}
		is.Equal(t, 1+len(cases), len(commits(t, w))-1)
	})

	t.Run("should keep a transaction usable after an insert of several values that fails, which changes nothing, and refuse a write in a read-only one after the errors of the names and the number of values", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t3)
		tx := begin(t, db, nil)
		_, err := tx.ExecContext(t.Context(), "insert into t3 values (?, ?, ?)", 1, "a", 1.5)
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), "insert into t3 (v1, v2) values (?, ?)", 2, "b")
		sqliteError(t, 19, 1299, "NOT NULL constraint failed: t3.v3", err)
		_, err = tx.ExecContext(t.Context(), "insert into t3 values (2, 'b', ?)", "c")
		is.Equal(t, "argument 1, which is text, for the column t3.v3 of type REAL", notSupported(t, err))
		is.Equal(t, "v3 REAL, v1 INT: 1.5 1", rowsText(t, tx, "select v3, v1 from t3"))
		is.NotError(t, tx.Commit())
		is.Equal(t, "v1 INT, v2 TEXT, v3 REAL: 1 a 1.5", rowsText(t, db, "select * from t3"))

		ro := begin(t, db, &sql.TxOptions{ReadOnly: true})
		_, err = ro.ExecContext(t.Context(), "insert into t3 values (2, 'b')")
		sqliteError(t, 1, 1, "table t3 has 3 columns but 2 values were supplied", err)
		_, err = ro.ExecContext(t.Context(), "insert into t3 (v1, y) values (2, 'b')")
		sqliteError(t, 1, 1, "table t3 has no column named y", err)
		for _, src := range []string{"insert into t3 (v1, v2) values (2, 'b')", "insert into t3 values (2, 'b', 'c')", "insert into t3 values (2, 'b', 2.5)"} {
			_, err = ro.ExecContext(t.Context(), src)
			sqliteError(t, 8, 8, "attempt to write a readonly database", err)
		}
		is.Equal(t, "v2 TEXT: a", rowsText(t, ro, "select v2 from t3"))
		is.NotError(t, ro.Commit())
	})
}

func TestNulls(t *testing.T) {
	const create = "create table n (k int primary key, t text, i integer, r real, b blob, s text not null) strict, without rowid"

	t.Run("should take every kind of nil that database/sql hands the driver as NULL, in a column of any type, directly and prepared, and annotate the statement by its text", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, create)
		type valuer struct{ sql.NullString }
		var nilValuer *nullValuer
		nils := []any{nil, (*int64)(nil), (*string)(nil), (*[]byte)(nil), sql.NullString{}, sql.NullInt64{}, sql.NullFloat64{}, sql.Null[[]byte]{}, sql.Null[string]{}, valuer{}, nullValuer{}, nilValuer}
		src := "insert into n (k, t, i, r, b, s) values (?, ?, ?, ?, ?, ?)"
		prepared, err := db.PrepareContext(t.Context(), src)
		is.NotError(t, err)
		for k, v := range nils {
			_, err := db.ExecContext(t.Context(), src, 2*k, v, v, v, v, "x")
			is.NotError(t, err, fmt.Sprintf("%T", v))
			_, err = prepared.ExecContext(t.Context(), 2*k+1, v, v, v, v, "x")
			is.NotError(t, err, fmt.Sprintf("%T", v))
		}
		is.NotError(t, prepared.Close())
		var rows []string
		for range 2 * len(nils) {
			rows = append(rows, "<nil> <nil> <nil> <nil> x")
		}
		is.Equal(t, "t TEXT, i INTEGER, r REAL, b BLOB, s TEXT: "+strings.Join(rows, "; "), rowsText(t, db, "select t, i, r, b, s from n"))
		is.Equal(t, `insert into n (k, t, i, r, b, s) values (?, ?, ?, ?, ?, ?)`, annotations(t, w)[1])
		is.NotError(t, db.Close())
		is.Equal(t, int64(2*len(nils)), rowCount(t, w.serve(t, "b"), "select k from n"))
	})

	t.Run("should take the literal NULL, and give NULL to a column that the list leaves out, and give NULL back as nil, which scans into a sql.Null, a pointer, and an any", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, create, "insert into n values (1, null, NULL, null, null, 'x')", "insert into n (s, k) values ('y', 2)", "insert into n (k, t, i, r, b, s) values (3, 'a', 4, 1.5, x'01', 'z')")
		is.Equal(t, "k INT, t TEXT, i INTEGER, r REAL, b BLOB, s TEXT: 1 <nil> <nil> <nil> <nil> x; 2 <nil> <nil> <nil> <nil> y; 3 a 4 1.5 [1] z", rowsText(t, db, "select * from n"))

		var (
			ns       sql.NullString
			ni       sql.NullInt64
			nf       sql.NullFloat64
			nb       sql.Null[[]byte]
			ps       *string
			pi       *int64
			anyValue any = "not nil"
		)
		// The row of the key 1, whose other columns but the last are NULL: without the condition, the one
		// row of QueryRow is the first that the query reads, forward or backward.
		is.NotError(t, db.QueryRowContext(t.Context(), "select t, i, r, b, t, i, t from n where k = 1").Scan(&ns, &ni, &nf, &nb, &ps, &pi, &anyValue))
		is.True(t, !ns.Valid && !ni.Valid && !nf.Valid && !nb.Valid, "a sql.Null of NULL is not valid")
		is.True(t, ps == nil && pi == nil, "a pointer of NULL is nil")
		is.True(t, anyValue == nil, "an any of NULL is nil")

		// A scan of NULL into a plain value is database/sql's own error.
		var s string
		var i int64
		err := db.QueryRowContext(t.Context(), "select t from n where k = 1").Scan(&s)
		is.True(t, err != nil && strings.Contains(err.Error(), "converting NULL to string is unsupported"), fmt.Sprint(err))
		err = db.QueryRowContext(t.Context(), "select i from n where k = 1").Scan(&i)
		is.True(t, err != nil && strings.Contains(err.Error(), "converting NULL to int64 is unsupported"), fmt.Sprint(err))
		_, ok := errors.AsType[*s4ql.Error](err)
		is.True(t, !ok, "the driver reports nothing wrong itself")

		is.NotError(t, db.Close())
		is.Equal(t, "k INT, t TEXT, i INTEGER, r REAL, b BLOB, s TEXT: 1 <nil> <nil> <nil> <nil> x; 2 <nil> <nil> <nil> <nil> y; 3 a 4 1.5 [1] z", rowsText(t, w.serve(t, "b"), "select * from n"))
	})

	t.Run("should give SQLite's NOT NULL constraint failed for a nil argument or NULL in the key or a column that does not take NULL, for the first in the order of the table, and set the count of changes to 0", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, create)
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, conn.Close()) }()
		cases := []struct {
			src     string
			args    []any
			message string
		}{
			{"insert into n (k, s) values (?, ?)", []any{nil, "x"}, "NOT NULL constraint failed: n.k"},
			{"insert into n (k, s) values (?, ?)", []any{sql.NullInt64{}, nil}, "NOT NULL constraint failed: n.k"},
			{"insert into n (k, s) values (?, ?)", []any{1, (*string)(nil)}, "NOT NULL constraint failed: n.s"},
			{"insert into n (k, t) values (?, ?)", []any{1, "x"}, "NOT NULL constraint failed: n.s"},
			{"insert into n (t, s) values (?, null)", []any{"x"}, "NOT NULL constraint failed: n.k"},
			{"insert into n (s, i) values (null, ?)", []any{"not an integer"}, "NOT NULL constraint failed: n.k"},
		}
		for j, c := range cases {
			_, err := conn.ExecContext(t.Context(), fmt.Sprint("insert into n (k, s) values (", 100+j, ", 'x')"))
			is.NotError(t, err)
			_, err = conn.ExecContext(t.Context(), c.src, c.args...)
			sqliteError(t, 19, 1299, c.message, err)
			res, err := conn.ExecContext(t.Context(), "select * from n")
			is.Equal(t, int64(0), affected(t, res, err), c.src)
		}
	})

	t.Run("should take NULL in transactions, also after a statement that failed, and refuse the write in a read-only one", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, create)
		tx := begin(t, db, nil)
		_, err := tx.ExecContext(t.Context(), "insert into n (k, t, s) values (?, ?, ?)", 1, nil, "x")
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), "insert into n (k, t, s) values (?, ?, ?)", 2, "a", nil)
		sqliteError(t, 19, 1299, "NOT NULL constraint failed: n.s", err)
		is.Equal(t, "k INT, t TEXT: 1 <nil>", rowsText(t, tx, "select k, t from n"))
		is.NotError(t, tx.Rollback())
		is.Equal(t, "k INT: ", rowsText(t, db, "select k from n"))

		tx = begin(t, db, nil)
		_, err = tx.ExecContext(t.Context(), "insert into n (k, t, s) values (?, ?, ?)", 1, nil, "x")
		is.NotError(t, err)
		is.NotError(t, tx.Commit())
		ro := begin(t, db, &sql.TxOptions{ReadOnly: true})
		_, err = ro.ExecContext(t.Context(), "insert into n (k, s) values (?, ?)", 2, nil)
		sqliteError(t, 8, 8, "attempt to write a readonly database", err)
		is.Equal(t, "t TEXT: <nil>", rowsText(t, ro, "select t from n"))
		is.NotError(t, ro.Commit())
		is.NotError(t, db.Close())
		is.Equal(t, "k INT, t TEXT, s TEXT: 1 <nil> x", rowsText(t, w.serve(t, "b"), "select k, t, s from n"))
	})
}

func TestWhere(t *testing.T) {
	const create = "create table w (k int primary key, s text, r real not null, b blob) strict, without rowid"
	fill := func(t *testing.T, db *sql.DB) {
		t.Helper()
		exec(t, db, create, "insert into w values (1, 'a', 1.5, x'01')", "insert into w values (2, null, 2.5, null)", "insert into w values (3, 'c', 3.5, x'')", "insert into w values (10, 'a', -0.0, x'0a')")
	}

	t.Run("should give the rows of the conditions, directly, prepared, several times with other arguments, and through QueryRow, and none for a comparison with a nil argument", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		fill(t, db)
		is.Equal(t, "k INT, s TEXT, r REAL, b BLOB: 1 a 1.5 [1]; 10 a 0 [10]", rowsText(t, db, "select * from w where s = ?", "a"))
		is.Equal(t, "s TEXT: a; c", rowsText(t, db, "select s from w where k >= 1 and k < 10 and s is not null"))
		is.Equal(t, "k INT: 2", rowsText(t, db, "select k from w where s is null"))
		is.Equal(t, "k INT: 10", rowsText(t, db, "select k from w where r = 0.0 and r == -0.0 and b > x'01'"))
		is.Equal(t, "k INT: 1; 10", rowsText(t, db, "select k from w where s <> 'c' and b != ?", []byte{}))

		p, err := db.PrepareContext(t.Context(), "select k from w where k > ? and k <= ? and s is not null")
		is.NotError(t, err)
		for args, want := range map[[2]int]string{{0, 10}: "k INT: 1; 3; 10", {1, 3}: "k INT: 3", {3, 1}: "k INT: ", {-1, 1}: "k INT: 1"} {
			rows, err := p.QueryContext(t.Context(), args[0], args[1])
			is.NotError(t, err)
			// The keys in ascending order, since the query reads forward or backward.
			var ks []string
			for _, k := range rest(t, rows) {
				ks = append(ks, fmt.Sprint(k))
			}
			is.Equal(t, want, "k INT: "+strings.Join(ks, "; "), fmt.Sprint(args))
		}
		is.NotError(t, p.Close())

		var s sql.NullString
		is.NotError(t, db.QueryRowContext(t.Context(), "select s from w where k = ?", 3).Scan(&s))
		is.Equal(t, "c", s.String)
		err = db.QueryRowContext(t.Context(), "select s from w where k = ?", 4).Scan(&s)
		is.True(t, errors.Is(err, sql.ErrNoRows), fmt.Sprint(err))

		for _, arg := range []any{nil, sql.NullString{}, (*string)(nil), sql.Null[[]byte]{}} {
			is.Equal(t, "k INT: ", rowsText(t, db, "select k from w where s = ? and s is not null", arg), fmt.Sprintf("%T", arg))
		}
	})

	t.Run("should give SQLite's errors of the names, and refuse a value of another type and an argument that no column holds as not supported", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		fill(t, db)
		for src, message := range map[string]string{
			"select * from nosuch where k = 1":                  "no such table: nosuch",
			"select x from w where y = 1":                       "no such column: x",
			"select * from w where k = 1 and x = 1":             "no such column: x",
			"select * from w where x is null and y is not null": "no such column: y",
			"select * from w where x is null and k = 1":         "no such column: x",
			`select * from w where "x" = 1`:                     `no such column: "x" - should this be a string literal in single-quotes?`,
			"select * from w where k = 'x' and x = 1":           "no such column: x",
		} {
			_, err := db.QueryContext(t.Context(), src)
			sqliteError(t, 1, 1, message, err)
		}
		for _, c := range []struct {
			src  string
			args []any
			want string
		}{
			{src: "select * from w where k = '1'", want: "a string literal compared with the column w.k of type INT"},
			{src: "select * from w where r > 1", want: "an integer literal compared with the column w.r of type REAL"},
			{src: "select * from w where k = 1 and s = ?", args: []any{1}, want: "argument 1, which is an integer, compared with the column w.s of type TEXT"},
			{src: "select * from w where b = ?", args: []any{"x"}, want: "argument 1, which is text, compared with the column w.b of type BLOB"},
			{src: "select * from w where k = ? and s = ?", args: []any{1.5, 1}, want: "argument 1, which is a real, compared with the column w.k of type INT"},
			{src: "select * from w where b = ?", args: []any{[]byte(nil)}, want: "argument 1 of type []byte, which is nil"},
			{src: "select * from w where s = ?", args: []any{true}, want: "argument 1 of type bool"},
			{src: "select * from w where s = ?", args: []any{"a\x00"}, want: "argument 1, which is text that holds the character NUL"},
		} {
			_, err := db.QueryContext(t.Context(), c.src, c.args...)
			is.Equal(t, c.want, notSupported(t, err), c.src)
			p, err := db.PrepareContext(t.Context(), c.src)
			is.NotError(t, err, c.src)
			_, err = p.QueryContext(t.Context(), c.args...)
			is.Equal(t, c.want, notSupported(t, err), c.src)
			is.NotError(t, p.Close())
		}
		_, err := db.QueryContext(t.Context(), "select * from w where k = ? and s = ?", 1)
		is.Equal(t, "s4ql: wrong number of arguments: 1 for a statement with 2 parameters", fmt.Sprint(err))
	})

	t.Run("should select in a write transaction from its own writes, in a read-only one from the state of its beginning, and after a restart with an empty cache", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		fill(t, db)
		before := len(commits(t, w))
		tx := begin(t, db, nil)
		_, err := tx.ExecContext(t.Context(), "insert into w values (?, ?, ?, ?)", 4, "d", 4.5, nil)
		is.NotError(t, err)
		is.Equal(t, "k INT, s TEXT: 3 c; 4 d", rowsText(t, tx, "select k, s from w where k > ? and k < ?", 2, 10))
		is.Equal(t, "k INT: ", rowsText(t, db, "select k from w where k = 4"))
		ro := begin(t, db, &sql.TxOptions{ReadOnly: true})
		is.NotError(t, tx.Commit())
		is.Equal(t, before+1, len(commits(t, w)))
		is.Equal(t, "k INT: ", rowsText(t, ro, "select k from w where k = 4"))
		is.Equal(t, "k INT: 3", rowsText(t, ro, "select k from w where k >= 3 and k <= 4"))
		is.NotError(t, ro.Commit())
		is.Equal(t, "k INT: 3; 4", rowsText(t, db, "select k from w where k >= 3 and k <= 4"))
		// A select writes nothing, also in a transaction.
		is.Equal(t, before+1, len(commits(t, w)))
		is.NotError(t, db.Close())
		is.Equal(t, "k INT, b BLOB: 2 <nil>; 4 <nil>", rowsText(t, w.serve(t, "b"), "select k, b from w where b is null"))
	})
}

func TestUpdateDelete(t *testing.T) {
	const create = "create table u (k int primary key, s text, r real not null) strict, without rowid"
	fill := func(t *testing.T, db *sql.DB) {
		t.Helper()
		exec(t, db, create, "insert into u values (1, 'a', 1.5)", "insert into u values (2, null, 2.5)", "insert into u values (3, 'c', 3.5)")
	}
	// affected runs src with args on db, and gives the rows that it affected.
	affected := func(t *testing.T, db interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	}, src string, args ...any) int64 {
		t.Helper()
		res, err := db.ExecContext(t.Context(), src, args...)
		is.NotError(t, err, src)
		n, err := res.RowsAffected()
		is.NotError(t, err)
		id, err := res.LastInsertId()
		is.NotError(t, err)
		is.Equal(t, int64(0), id)
		return n
	}

	t.Run("should update and delete, directly and prepared, with the arguments of SET before those of WHERE, and give every row that they find as affected", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		fill(t, db)
		is.Equal(t, int64(3), affected(t, db, "update u set r = 0.5"))
		is.Equal(t, int64(3), affected(t, db, "update u set r = 0.5"))
		is.Equal(t, int64(1), affected(t, db, "update u set s = ?, r = ? where k = ? and s is null", "b", 9.5, 2))
		is.Equal(t, int64(0), affected(t, db, "update u set s = ? where k = ?", "z", 9))
		is.Equal(t, "k INT, s TEXT, r REAL: 1 a 0.5; 2 b 9.5; 3 c 0.5", rowsText(t, db, "select * from u"))

		p, err := db.PrepareContext(t.Context(), "update u set k = ? where k = ?")
		is.NotError(t, err)
		for _, args := range [][2]int{{4, 1}, {1, 4}, {5, 3}} {
			res, err := p.ExecContext(t.Context(), args[0], args[1])
			is.NotError(t, err)
			n, err := res.RowsAffected()
			is.NotError(t, err)
			is.Equal(t, int64(1), n, fmt.Sprint(args))
		}
		is.NotError(t, p.Close())
		is.Equal(t, "k INT: 1; 2; 5", rowsText(t, db, "select k from u"))

		is.Equal(t, int64(1), affected(t, db, "delete from u where k = ?", 5))
		is.Equal(t, int64(0), affected(t, db, "delete from u where s = ?", nil))
		is.Equal(t, "k INT, s TEXT, r REAL: 1 a 0.5; 2 b 9.5", rowsText(t, db, "select * from u"))
		is.Equal(t, int64(2), affected(t, db, "delete from u"))
		is.Equal(t, "k INT: ", rowsText(t, db, "select k from u"))
		is.NotError(t, db.Close())
		is.Equal(t, "k INT: ", rowsText(t, w.serve(t, "b"), "select k from u"))
	})

	t.Run("should give SQLite's errors, refuse a value of another type as not supported, and change nothing", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		fill(t, db)
		for _, c := range []struct {
			src               string
			args              []any
			code, extended    int
			message           string
			notSupportedWhich string
		}{
			{src: "update nosuch set s = 'x'", code: 1, extended: 1, message: "no such table: nosuch"},
			{src: "delete from nosuch", code: 1, extended: 1, message: "no such table: nosuch"},
			{src: `update u set s = 'x', "z" = 1 where y = 1`, code: 1, extended: 1, message: "no such column: z"},
			{src: "update u set s = 'x' where y is null and k = 1", code: 1, extended: 1, message: "no such column: y"},
			{src: "delete from u where z = ?", args: []any{1}, code: 1, extended: 1, message: "no such column: z"},
			{src: "update u set r = ? where k = 1", args: []any{nil}, code: 19, extended: 1299, message: "NOT NULL constraint failed: u.r"},
			{src: "update u set k = null where k = 1", code: 19, extended: 1299, message: "NOT NULL constraint failed: u.k"},
			{src: "update u set k = 3 where k = 1", code: 19, extended: 1555, message: "UNIQUE constraint failed: u.k"},
			{src: "update u set k = ? where k > ?", args: []any{7, 1}, code: 19, extended: 1555, message: "UNIQUE constraint failed: u.k"},
			{src: "update u set s = 1 where k = 9", notSupportedWhich: "an integer literal for the column u.s of type TEXT"},
			{src: "update u set r = ? where k = 1", args: []any{"1.5"}, notSupportedWhich: "argument 1, which is text, for the column u.r of type REAL"},
			{src: "delete from u where k = ?", args: []any{"1"}, notSupportedWhich: "argument 1, which is text, compared with the column u.k of type INT"},
			{src: "update u set s = ?", args: []any{true}, notSupportedWhich: "argument 1 of type bool"},
		} {
			_, err := db.ExecContext(t.Context(), c.src, c.args...)
			if c.notSupportedWhich != "" {
				is.Equal(t, c.notSupportedWhich, notSupported(t, err), c.src)
			} else {
				sqliteError(t, c.code, c.extended, c.message, err)
			}
		}
		_, err := db.ExecContext(t.Context(), "update u set s = ? where k = ?", "x")
		is.Equal(t, "s4ql: wrong number of arguments: 1 for a statement with 2 parameters", fmt.Sprint(err))
		_, err = db.PrepareContext(t.Context(), "update u set s = 'x' returning s as x")
		is.Equal(t, "alias (at offset 33)", notSupported(t, err))
		is.Equal(t, "k INT, s TEXT, r REAL: 1 a 1.5; 2 <nil> 2.5; 3 c 3.5", rowsText(t, db, "select * from u"))
	})

	t.Run("should update and delete in a write transaction, which reads its own changes, keeps them after a statement that fails, and commits them as one", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		fill(t, db)
		before := len(commits(t, w))
		tx := begin(t, db, nil)
		is.Equal(t, int64(2), affected(t, tx, "update u set s = 'q' where k >= ?", 2))
		_, err := tx.ExecContext(t.Context(), "update u set k = 1 where k = 2")
		sqliteError(t, 19, 1555, "UNIQUE constraint failed: u.k", err)
		is.Equal(t, int64(1), affected(t, tx, "update u set k = 9 where k = 3"))
		is.Equal(t, int64(1), affected(t, tx, "delete from u where k = 1"))
		is.Equal(t, "k INT, s TEXT: 2 q; 9 q", rowsText(t, tx, "select k, s from u"))
		is.Equal(t, "k INT: 1; 2; 3", rowsText(t, db, "select k from u"))
		is.NotError(t, tx.Commit())
		is.Equal(t, before+1, len(commits(t, w)))
		is.NotError(t, db.Close())
		is.Equal(t, "k INT, s TEXT: 2 q; 9 q", rowsText(t, w.serve(t, "b"), "select k, s from u"))
	})

	t.Run("should refuse an update and a delete in a read-only transaction with SQLite's error, after the errors of the names", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		fill(t, db)
		ro := begin(t, db, &sql.TxOptions{ReadOnly: true})
		for _, src := range []string{"update u set s = 'x'", "update u set s = 'x' where k = 9", "delete from u", "delete from u where k = 9"} {
			_, err := ro.ExecContext(t.Context(), src)
			sqliteError(t, 8, 8, "attempt to write a readonly database", err)
		}
		_, err := ro.ExecContext(t.Context(), "update u set z = 1")
		sqliteError(t, 1, 1, "no such column: z", err)
		_, err = ro.ExecContext(t.Context(), "delete from v")
		sqliteError(t, 1, 1, "no such table: v", err)
		is.NotError(t, ro.Commit())
		is.Equal(t, "k INT: 1; 2; 3", rowsText(t, db, "select k from u"))
	})
}

func TestIndexes(t *testing.T) {
	const create = "create table x (k int primary key, s text, r real not null) strict, without rowid"
	fill := func(t *testing.T, db *sql.DB) {
		t.Helper()
		exec(t, db, create, "insert into x values (1, 'a', 1.5)", "insert into x values (2, null, 2.5)", "insert into x values (3, 'a', 3.5)")
	}

	t.Run("should create and drop an index, directly and prepared, with no last insert ID, and give the rows of a query through it", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		fill(t, db)

		// A create index and a drop index leave SQLite's count of changes as the statement before on
		// their connection left it, so only the last insert ID is pinned for them.
		res, err := db.ExecContext(t.Context(), "create index xs on x (s, r desc)")
		affected(t, res, err)
		is.Equal(t, "k INT, s TEXT, r REAL: 1 a 1.5; 3 a 3.5", rowsText(t, db, "select * from x where s = ?", "a"))
		is.Equal(t, "k INT: 3", rowsText(t, db, "select k from x where s = ? and r > ?", "a", 2.0))
		is.Equal(t, "k INT: 2", rowsText(t, db, "select k from x where s is null"))
		res, err = db.ExecContext(t.Context(), "update x set r = 9.5 where s = 'a'")
		is.Equal(t, int64(2), affected(t, res, err))
		res, err = db.ExecContext(t.Context(), "insert into x values (4, 'a', 0.5)")
		is.Equal(t, int64(1), affected(t, res, err))
		res, err = db.ExecContext(t.Context(), "drop index xs")
		affected(t, res, err)
		is.Equal(t, "k INT: 1; 3; 4", rowsText(t, db, "select k from x where s = ?", "a"))

		p, err := db.PrepareContext(t.Context(), "create index xr on x (r)")
		is.NotError(t, err)
		_, err = p.ExecContext(t.Context())
		is.NotError(t, err)
		_, err = p.ExecContext(t.Context())
		sqliteError(t, 1, 1, "index xr already exists", err)
		is.NotError(t, p.Close())
		is.Equal(t, "k INT: 1; 3", rowsText(t, db, "select k from x where r = 9.5"))

		// Another instance replays the index, and reads by it.
		is.NotError(t, db.Close())
		is.Equal(t, "k INT: 1; 3", rowsText(t, w.serve(t, "b"), "select k from x where r = 9.5"))
	})

	t.Run("should give SQLite's errors of a create index and of a drop index, and refuse what s4ql does not support", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		fill(t, db)
		exec(t, db, "create index xs on x (s)")

		for src, want := range map[string]string{
			"create index i on nosuch (s)": "no such table: main.nosuch",
			"create index x on x (s)":      "there is already a table named x",
			"create index XS on x (r)":     "index XS already exists",
			"create index i on x (s, z)":   "no such column: z",
			"drop index nosuch":            "no such index: nosuch",
			"drop index x":                 "no such index: x",
			"create table xs (k int primary key) strict, without rowid": "there is already an index named xs",
		} {
			_, err := db.ExecContext(t.Context(), src)
			sqliteError(t, 1, 1, want, err)
		}
		for src, want := range map[string]string{
			"create unique index i on x (s)":         "UNIQUE INDEX",
			"create index i on x (s) where s = 'a'":  "partial index",
			"select * from x indexed by xs":          "INDEXED BY",
			"select * from x not indexed":            "NOT INDEXED",
			"explain query plan select * from x":     "EXPLAIN QUERY PLAN",
			"reindex xs":                             "REINDEX",
			"analyze":                                "ANALYZE",
			"create index i on x (s collate nocase)": "COLLATE",
			"create index i on x (lower(s))":         "index on an expression",
		} {
			_, err := db.ExecContext(t.Context(), src)
			is.True(t, strings.HasPrefix(notSupported(t, err), want), fmt.Sprint(src, ": ", err))
		}
		is.Equal(t, "k INT: 1; 3", rowsText(t, db, "select k from x where s = 'a'"))
		got, err := rowsInOrder(t, db, "select k from x where s = 'a' order by k desc")
		is.NotError(t, err)
		is.Equal(t, "int64 3; int64 1", strings.Join(got, "; "))
	})

	t.Run("should create and drop an index in a transaction, undo both with a roll back, and refuse both in a read-only one", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		fill(t, db)
		exec(t, db, "create index kept on x (r)")

		tx, err := db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), "create index xs on x (s)")
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), "drop index kept")
		is.NotError(t, err)
		is.Equal(t, "k INT: 1; 3", rowsText(t, tx, "select k from x where s = 'a'"))
		is.NotError(t, tx.Rollback())
		_, err = db.ExecContext(t.Context(), "drop index xs")
		sqliteError(t, 1, 1, "no such index: xs", err)

		ro, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		is.NotError(t, err)
		_, err = ro.ExecContext(t.Context(), "create index xs on x (s)")
		sqliteError(t, 8, 8, "attempt to write a readonly database", err)
		_, err = ro.ExecContext(t.Context(), "drop index kept")
		sqliteError(t, 8, 8, "attempt to write a readonly database", err)
		_, err = ro.ExecContext(t.Context(), "drop index xs")
		sqliteError(t, 1, 1, "no such index: xs", err)
		is.NotError(t, ro.Commit())

		tx, err = db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), "drop index kept")
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), "create index xs on x (s desc, k)")
		is.NotError(t, err)
		is.NotError(t, tx.Commit())
		_, err = db.ExecContext(t.Context(), "drop index kept")
		sqliteError(t, 1, 1, "no such index: kept", err)
		is.Equal(t, "k INT: 1; 3", rowsText(t, db, "select k from x where s >= 'a'"))
	})
}

func TestExists(t *testing.T) {
	const create = "create table x (k int primary key, s text) strict, without rowid"

	t.Run("should run the statements of IF NOT EXISTS and IF EXISTS, directly and prepared, leave the count of changes as the plain statements do, and write nothing for one that does nothing", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, create, "create index xs on x (s)")
		res, err := db.ExecContext(t.Context(), "insert into x values (1, 'a')")
		is.Equal(t, int64(1), affected(t, res, err))
		before := len(commits(t, w))

		// SQLite's count of changes stays as the insert left it, whether a statement does something or
		// nothing, as it does for the plain statements.
		for _, src := range []string{
			"create table if not exists x (k int primary key) strict, without rowid", "create table if not exists X (z text primary key, z int) strict, without rowid",
			"drop table if exists nosuch", "drop table if exists xs", "create index if not exists xs on x (k)",
			"create index if not exists XS on x (nosuch)", "drop index if exists nosuch", "drop index if exists x",
		} {
			res, err := db.ExecContext(t.Context(), src)
			is.Equal(t, int64(1), affected(t, res, err), src)
		}
		is.Equal(t, before, len(commits(t, w)), "the commit objects after the statements that do nothing")

		p, err := db.PrepareContext(t.Context(), "create table if not exists y (k int primary key) strict, without rowid")
		is.NotError(t, err)
		for range 2 {
			res, err := p.ExecContext(t.Context())
			is.Equal(t, int64(1), affected(t, res, err))
		}
		is.NotError(t, p.Close())
		is.Equal(t, before+1, len(commits(t, w)), "one commit object of the create table that created")
		exec(t, db, "create index if not exists yk on y (k)", "drop index if exists xs", "drop table if exists x")
		is.Equal(t, before+4, len(commits(t, w)))

		_, err = db.ExecContext(t.Context(), "create table if not exists yk (k int primary key) strict, without rowid")
		sqliteError(t, 1, 1, "there is already an index named yk", err)
		_, err = db.ExecContext(t.Context(), "create index if not exists i on x (s)")
		sqliteError(t, 1, 1, "no such table: main.x", err)
		_, err = db.ExecContext(t.Context(), "create index if not exists y on y (k)")
		sqliteError(t, 1, 1, "there is already a table named y", err)
		_, err = db.ExecContext(t.Context(), "create index if not exists i on y (nosuch)")
		sqliteError(t, 1, 1, "no such column: nosuch", err)

		// Another instance replays the log.
		is.NotError(t, db.Close())
		b := w.serve(t, "b")
		is.Equal(t, "k INT: ", rowsText(t, b, "select k from y where k = 1"))
		_, err = b.ExecContext(t.Context(), "drop index yk")
		is.NotError(t, err)
		_, err = b.ExecContext(t.Context(), "select * from x")
		sqliteError(t, 1, 1, "no such table: x", err)
	})

	t.Run("should run them in a transaction, undo them with a roll back, and run those that do nothing in a read-only one", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, create, "insert into x values (1, 'a')")
		before := len(commits(t, w))

		tx, err := db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		for _, src := range []string{"drop table if exists x", "drop table if exists x", "create table if not exists x (k text primary key) strict, without rowid", "create table if not exists x (k int primary key) strict, without rowid"} {
			_, err = tx.ExecContext(t.Context(), src)
			is.NotError(t, err, src)
		}
		is.NotError(t, tx.Rollback())
		is.Equal(t, "k INT, s TEXT: 1 a", rowsText(t, db, "select * from x"))

		tx, err = db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		for _, src := range []string{"create table if not exists x (k text primary key) strict, without rowid", "drop index if exists nosuch"} {
			_, err = tx.ExecContext(t.Context(), src)
			is.NotError(t, err, src)
		}
		is.NotError(t, tx.Commit())
		is.Equal(t, before, len(commits(t, w)), "a transaction of statements that do nothing writes no commit object")

		ro, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		is.NotError(t, err)
		_, err = ro.ExecContext(t.Context(), "create table if not exists x (k int primary key) strict, without rowid")
		is.NotError(t, err)
		_, err = ro.ExecContext(t.Context(), "drop table if exists nosuch")
		is.NotError(t, err)
		_, err = ro.ExecContext(t.Context(), "drop table if exists x")
		sqliteError(t, 8, 8, "attempt to write a readonly database", err)
		is.NotError(t, ro.Commit())
		is.Equal(t, "k INT, s TEXT: 1 a", rowsText(t, db, "select * from x"))
	})
}

// nullValuer is a [driver.Valuer] that gives nil. database/sql takes a nil pointer to it for nil
// too, without a call.
type nullValuer struct{}

func (nullValuer) Value() (driver.Value, error) {
	return nil, nil
}

// rowCount of the query src on db.
func rowCount(t *testing.T, db querier, src string) int64 {
	t.Helper()
	return int64(len(rowsOf(t, db, src)))
}

func TestDefaults(t *testing.T) {
	const create = "create table d (k int primary key, n integer not null default 0, r real default -0.0, s text default 'it''s', b blob default x'0a', u text default null) strict, without rowid"

	t.Run("should create a table with defaults and store them for the columns that an insert leaves out, directly, prepared, and in a transaction, with the count of rows and no last insert ID", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		res, err := db.ExecContext(t.Context(), create)
		affected(t, res, err)
		res, err = db.ExecContext(t.Context(), "insert into d (k) values (?)", 1)
		is.Equal(t, int64(1), affected(t, res, err))
		p, err := db.PrepareContext(t.Context(), "insert into d (s, k) values (?, ?)")
		is.NotError(t, err)
		res, err = p.ExecContext(t.Context(), nil, 2)
		is.Equal(t, int64(1), affected(t, res, err))
		res, err = p.ExecContext(t.Context(), "x", 3)
		is.Equal(t, int64(1), affected(t, res, err))
		_, err = p.ExecContext(t.Context(), "y", 3)
		sqliteError(t, 19, 1555, "UNIQUE constraint failed: d.k", err)
		is.NotError(t, p.Close())
		tx, err := db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		res, err = tx.ExecContext(t.Context(), "insert into d (k, n, u) values (4, 5, 'u')")
		is.Equal(t, int64(1), affected(t, res, err))
		_, err = tx.ExecContext(t.Context(), "insert into d (k, n) values (5, null)")
		sqliteError(t, 19, 1299, "NOT NULL constraint failed: d.n", err)
		is.NotError(t, tx.Commit())
		const want = "k INT, n INTEGER, r REAL, s TEXT, b BLOB, u TEXT: 1 0 0 it's [10] <nil>; 2 0 0 <nil> [10] <nil>; 3 0 0 x [10] <nil>; 4 5 0 it's [10] u"
		is.Equal(t, want, rowsText(t, db, "select * from d"))
		is.Equal(t, "insert into d (k) values (?)", annotations(t, w)[1])

		// Another instance replays the table with its defaults, and fills them in.
		is.NotError(t, db.Close())
		db = w.serve(t, "b")
		is.Equal(t, want, rowsText(t, db, "select * from d"))
		exec(t, db, "insert into d (k) values (6)")
		is.Equal(t, "k INT, n INTEGER, r REAL, s TEXT, b BLOB, u TEXT: 6 0 0 it's [10] <nil>", rowsText(t, db, "select * from d where k = 6"))
	})

	t.Run("should refuse at once what s4ql does not support of a DEFAULT", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		for src, want := range map[string]string{
			"create table d (k int primary key, v real default 1) strict, without rowid":            "DEFAULT of another type than its column (at offset 50)",
			"create table d (k int primary key, v int default (1)) strict, without rowid":           "DEFAULT of an expression (at offset 49)",
			"create table d (k int primary key, v text default current_time) strict, without rowid": "current date or time (at offset 50)",
			"insert into d default values": "DEFAULT VALUES (at offset 14)",
		} {
			_, err := db.PrepareContext(t.Context(), src)
			is.Equal(t, want, notSupported(t, err), src)
		}
	})
}

func TestValueFirst(t *testing.T) {
	// The table of goqite's queue, as far as slices 1 to 12 take it.
	const create = "create table goqite (id text primary key, queue text not null, body blob not null, timeout text not null, " +
		"received integer not null default 0, priority integer not null default 0) strict, without rowid"
	fill := func(t *testing.T, db *sql.DB) {
		t.Helper()
		exec(t, db, create, "create index goqite_queue_timeout on goqite (queue, timeout)",
			"insert into goqite (id, queue, body, timeout) values ('m1', 'q', x'01', '2026-11-21T10:00:00.000Z')",
			"insert into goqite (id, queue, body, timeout, received) values ('m2', 'q', x'02', '2026-11-21T12:00:00.000Z', 3)",
			"insert into goqite (id, queue, body, timeout) values ('m3', 'q', x'03', '2026-11-21T14:00:00.000Z')",
			"insert into goqite (id, queue, body, timeout) values ('m4', 'r', x'04', '2026-11-21T10:00:00.000Z')")
	}
	// goqite's receive, without its ORDER BY and its LIMIT, which slice 13 brings, as goqite writes it,
	// and with its column first.
	const receive = "select id from goqite where queue = ? and ? >= timeout and received < ?"
	const columnFirst = "select id from goqite where queue = ? and timeout <= ? and received < ?"

	t.Run("should give the rows of a comparison with its value on the left, directly, prepared, several times with other arguments, and in a transaction, as with its column first", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		fill(t, db)
		for args, want := range map[[3]any]string{
			{"q", "2026-11-21T13:00:00.000Z", 3}: "id TEXT: m1",
			{"q", "2026-11-21T13:00:00.000Z", 4}: "id TEXT: m1; m2",
			{"q", "2026-11-21T09:00:00.000Z", 4}: "id TEXT: ",
			{"r", "2026-11-21T10:00:00.000Z", 1}: "id TEXT: m4",
		} {
			is.Equal(t, want, rowsText(t, db, receive, args[:]...), fmt.Sprint(args))
			is.Equal(t, want, rowsText(t, db, columnFirst, args[:]...), fmt.Sprint(args))
		}
		is.Equal(t, "id TEXT: m3; m4", rowsText(t, db, "select id from goqite where 'm2' < id and 'm4' >= id"))
		is.Equal(t, "id TEXT: ", rowsText(t, db, "select id from goqite where null = queue"))
		is.Equal(t, "id TEXT: ", rowsText(t, db, "select id from goqite where ? <> queue", nil))

		p, err := db.PrepareContext(t.Context(), receive)
		is.NotError(t, err)
		for received, want := range map[int]string{0: "", 1: "m1 m3", 4: "m1 m2 m3"} {
			rows, err := p.QueryContext(t.Context(), "q", "2026-11-21T15:00:00.000Z", received)
			is.NotError(t, err)
			var ids []string
			for rows.Next() {
				var id string
				is.NotError(t, rows.Scan(&id))
				ids = append(ids, id)
			}
			is.NotError(t, rows.Err())
			slices.Sort(ids)
			is.Equal(t, want, strings.Join(ids, " "), fmt.Sprint(received))
		}
		is.NotError(t, p.Close())

		var id string
		is.NotError(t, db.QueryRowContext(t.Context(), "select id from goqite where ? = id", "m3").Scan(&id))
		is.Equal(t, "m3", id)

		tx := begin(t, db, nil)
		_, err = tx.ExecContext(t.Context(), "insert into goqite (id, queue, body, timeout) values ('m5', 'q', x'05', '2026-11-21T11:00:00.000Z')")
		is.NotError(t, err)
		is.Equal(t, "id TEXT: m1; m5", rowsText(t, tx, receive, "q", "2026-11-21T13:00:00.000Z", 3))
		is.NotError(t, tx.Commit())
	})

	t.Run("should change and delete the rows of a comparison with its value on the left, with arguments numbered in the order of the text, and the count of rows, directly, prepared, and after a replay", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		fill(t, db)
		// goqite's extend and delete are shaped so, with the value of a condition on the left.
		res, err := db.ExecContext(t.Context(), "update goqite set timeout = ?, received = ? where ? = queue and id = ?", "2026-11-21T20:00:00.000Z", 1, "q", "m1")
		is.Equal(t, int64(1), affected(t, res, err))
		p, err := db.PrepareContext(t.Context(), "update goqite set priority = ? where ? > timeout and queue = ?")
		is.NotError(t, err)
		res, err = p.ExecContext(t.Context(), 7, "2026-11-21T13:00:00.000Z", "q")
		is.Equal(t, int64(1), affected(t, res, err))
		res, err = p.ExecContext(t.Context(), 8, "2026-11-21T23:00:00.000Z", "q")
		is.Equal(t, int64(3), affected(t, res, err))
		is.NotError(t, p.Close())
		res, err = db.ExecContext(t.Context(), "delete from goqite where ? <= timeout and 'q' = queue", "2026-11-21T14:00:00.000Z")
		is.Equal(t, int64(2), affected(t, res, err))
		const want = "id TEXT, priority INTEGER, received INTEGER: m2 8 3; m4 0 0"
		is.Equal(t, want, rowsText(t, db, "select id, priority, received from goqite"))
		is.Equal(t, `delete from goqite where ? <= timeout and 'q' = queue`, annotations(t, w)[len(annotations(t, w))-1])

		is.NotError(t, db.Close())
		db = w.serve(t, "b")
		is.Equal(t, want, rowsText(t, db, "select id, priority, received from goqite"))
		is.Equal(t, "id TEXT: m4", rowsText(t, db, "select id from goqite where 0 = received"))
	})

	t.Run("should give SQLite's errors of the names, and refuse a value of another type and an argument that no column holds, on the left too", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		fill(t, db)
		for src, message := range map[string]string{
			"select * from goqite where 1 = nope":                   "no such column: nope",
			`select * from goqite where 'q' = "nope"`:               `no such column: "nope" - should this be a string literal in single-quotes?`,
			"select * from goqite where nope is null and 1 < nope2": "no such column: nope",
			"update goqite set received = 1 where 'x' > nope":       "no such column: nope",
			"delete from goqite where 'q' = queue and x'00' = nope": "no such column: nope",
		} {
			_, err := db.ExecContext(t.Context(), src)
			sqliteError(t, 1, 1, message, err)
		}
		for _, c := range []struct {
			src  string
			args []any
			want string
		}{
			{src: "select * from goqite where 1 = queue", want: "an integer literal compared with the column goqite.queue of type TEXT"},
			{src: "select * from goqite where queue = ? and ? >= timeout", args: []any{"q", 5}, want: "argument 2, which is an integer, compared with the column goqite.timeout of type TEXT"},
			{src: "delete from goqite where ? < received", args: []any{true}, want: "argument 1 of type bool"},
			{src: "update goqite set received = 1 where 1.5 < received", want: "a real literal compared with the column goqite.received of type INTEGER"},
		} {
			_, err := db.ExecContext(t.Context(), c.src, c.args...)
			is.Equal(t, c.want, notSupported(t, err), c.src)
			p, err := db.PrepareContext(t.Context(), c.src)
			is.NotError(t, err, c.src)
			_, err = p.ExecContext(t.Context(), c.args...)
			is.Equal(t, c.want, notSupported(t, err), c.src)
			is.NotError(t, p.Close())
		}
		_, err := db.QueryContext(t.Context(), receive, "q", "2026-11-21T13:00:00.000Z")
		is.Equal(t, "s4ql: wrong number of arguments: 2 for a statement with 3 parameters", fmt.Sprint(err))
	})
}

// ordered is a select with ORDER BY or LIMIT, in its parts, so that a test can ask SQLite for the
// rows of the same select without its LIMIT, and their ranks under the ORDER BY, and compare the
// rows of s4ql with them as [ordered.differs] says.
type ordered struct {
	// columns of the select list, table, and the conditions of WHERE, or empty for none.
	columns, table, where string
	// by holds the terms of ORDER BY, or is empty for none.
	by string
	// limit is the number of LIMIT as the text writes it, "?" for a parameter, or empty for none.
	limit string
	// args of the parameters of where, and then of limit, each as SQLite binds it.
	args []any
}

// src is the text of the select.
func (q ordered) src() string {
	src := "select " + q.columns + " from " + q.table
	if q.where != "" {
		src += " where " + q.where
	}
	if q.by != "" {
		src += " order by " + q.by
	}
	if q.limit != "" {
		src += " limit " + q.limit
	}
	return src
}

// first is the select with a LIMIT of 1 in place of its own, which gives the row that a QueryRow of
// the select gives, if its own LIMIT is not 0.
func (q ordered) first() ordered {
	if q.limit == "?" {
		q.args = q.args[:len(q.args)-1]
	}
	q.limit = "1"
	return q
}

// differs says how the rows got, which s4ql gave for the select in the order in which they came, as
// [rowText] gives them, differ from what SQLite, o, gives for it, or "" if they do not:
//
//   - The number of rows is SQLite's.
//   - With ORDER BY, the rows are in SQLite's order. SQLite gives the rows of the select without its
//     LIMIT, and separately the dense rank of each under the ORDER BY, in the same order, so that two
//     rows have one rank exactly if they tie. The row of s4ql at each place must be a row of the rank
//     that SQLite has at that place: where no rows tie, that is the same row at the same place, and
//     the rows of a rank are compared as a multiset, of which a LIMIT that cuts a rank leaves any
//     part. The ranks at each place do not depend on the order in which SQLite gives tied rows, so
//     the two queries need not give those in one order.
//   - Without ORDER BY, the rows are rows of the select without its LIMIT, each no more often than
//     that select gives it: with a LIMIT, any of them are right, and without one, they are all of
//     them.
func (q ordered) differs(t *testing.T, o *oracle, got []string) string {
	t.Helper()
	want, err := o.rows(t, q.src(), q.args...)
	is.True(t, err == nil, fmt.Sprint(q.src(), ": SQLite gives ", err))
	if len(got) != len(want) {
		return fmt.Sprintf("%v: s4ql gives %v rows, %q, and SQLite %v, %q", q.src(), len(got), got, len(want), want)
	}
	args := q.args
	if q.limit == "?" {
		args = args[:len(args)-1]
	}
	unlimited := q
	unlimited.limit, unlimited.args = "", args
	rows, err := o.rows(t, unlimited.src(), args...)
	is.True(t, err == nil, fmt.Sprint(unlimited.src(), ": SQLite gives ", err))
	ranks := make([]string, len(rows))
	if q.by != "" {
		ranked := unlimited
		ranked.columns = "dense_rank() over (order by " + q.by + ")"
		ranks, err = o.rows(t, ranked.src(), args...)
		is.True(t, err == nil, fmt.Sprint(ranked.src(), ": SQLite gives ", err))
		is.Equal(t, len(rows), len(ranks))
	}
	if len(got) > len(rows) {
		return fmt.Sprintf("%v: s4ql gives %v rows, and the select without its LIMIT has %v in SQLite", q.src(), len(got), len(rows))
	}
	for start := 0; start < len(got); {
		end := start
		left := map[string]int{}
		for end < len(rows) && ranks[end] == ranks[start] {
			left[rows[end]]++
			end++
		}
		for n := start; n < min(end, len(got)); n++ {
			if left[got[n]]--; left[got[n]] < 0 {
				return fmt.Sprintf("%v: s4ql gives the row %v at the place %v, where SQLite has the rows %q; s4ql's rows are %q", q.src(), got[n], n, rows[start:end], got)
			}
		}
		start = end
	}
	return ""
}

// rowQuerier is where a query of one row runs: a [sql.DB], a [sql.Conn], or a [sql.Tx].
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// inOrder requires that the select q gives on db, through Query and through QueryRow, the rows that
// SQLite, o, gives for it, as [ordered.differs] says, and gives the rows of the Query.
func inOrder(t *testing.T, db interface {
	querier
	rowQuerier
}, o *oracle, q ordered) []string {
	t.Helper()
	got, err := rowsInOrder(t, db, q.src(), q.args...)
	is.NotError(t, err, q.src())
	is.Equal(t, "", q.differs(t, o, got))

	width := strings.Count(q.columns, ",") + 1
	if q.columns == "*" {
		width = len(strings.Split(o.columnsOf(t, q.table), ","))
	}
	values := make([]any, width)
	dest := make([]any, width)
	for i := range values {
		dest[i] = &values[i]
	}
	err = db.QueryRowContext(t.Context(), q.src(), q.args...).Scan(dest...)
	if len(got) == 0 {
		is.Error(t, sql.ErrNoRows, err, q.src())
		return got
	}
	is.NotError(t, err, q.src())
	is.Equal(t, "", q.first().differs(t, o, []string{rowText(values)}))
	return got
}

// columnsOf the table in SQLite: their names, joined by commas.
func (o *oracle) columnsOf(t *testing.T, table string) string {
	t.Helper()
	rows, err := o.rows(t, "select group_concat(name) from pragma_table_info(?)", table)
	is.True(t, err == nil && len(rows) == 1, fmt.Sprint(table, ": ", err))
	names, ok := strings.CutPrefix(rows[0], `string "`)
	is.True(t, ok, rows[0])
	return strings.TrimSuffix(names, `"`)
}

// orderTable is the table of the tests of ORDER BY and LIMIT, with a column of each type, rows that
// tie in each column but the key, NULL in the columns that take it, 0.0 and -0.0, the largest and
// the smallest integer, the empty text and the empty blob, and text that sorts by its bytes.
var orderTable = []string{
	"create table o (k int primary key, i integer, r real, s text, b blob, n int not null) strict, without rowid",
	"insert into o values (1, 3, 1.5, 'm', x'01', 1)",
	"insert into o values (2, null, 2.5, null, x'02', 2)",
	"insert into o values (3, 0, null, 'a', null, 0)",
	"insert into o values (4, 3, 0.5, 'z', x'', 1)",
	"insert into o values (5, -7, 2.5, 'm', x'ff', 2)",
	"insert into o values (6, null, -0.0, null, x'00', 0)",
	"insert into o values (7, 0, 1.5, 'b', null, 1)",
	"insert into o values (8, 9223372036854775807, 0.0, 'a', x'0100', 2)",
	"insert into o values (9, -9223372036854775808, null, 'z', x'01', 0)",
	"insert into o values (10, 3, 3.5, '', x'7f', 1)",
	"insert into o values (11, 0, -1.5, 'b', x'80', 2)",
	"insert into o values (12, null, 2.5, 'M', x'02', 0)",
}

func TestOrderBy(t *testing.T) {
	// The terms of ORDER BY: each column ascending and descending, two and three columns, and a
	// column that ties in most rows, alone, which leaves its ties in any order.
	bys := []string{"k", "k desc", "i", "i desc", "r", "r asc", "r desc", "s", "s desc", "b", "b desc", "n", "n desc",
		"n, s desc", "n desc, r", "i, r desc, k", "s, b desc", "b desc, i desc, k"}
	// The limits, as the text writes them: none, 0, 1, a few, more than all rows, and a parameter.
	limits := []string{"", "0", "1", "3", "100", "?"}
	// The shapes of the select that the terms and limits go with.
	shapes := []ordered{
		{columns: "k"},
		{columns: "*"},
		{columns: "s, k", where: "n = ?", args: []any{int64(1)}},
		{columns: "b, r", where: "s is not null"},
		{columns: "n", where: "r > 0.0"},
	}

	t.Run("should give the rows of each type, with NULL, in the order of ORDER BY, ascending and descending, by several columns, with ties, and as many as LIMIT says, through Query and QueryRow, as SQLite gives them", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, orderTable...)
		o := newOracle(t, orderTable...)

		for _, shape := range shapes {
			for _, by := range append([]string{""}, bys...) {
				for _, limit := range limits {
					q := shape
					q.table, q.by, q.limit = "o", by, limit
					q.args = slices.Clone(shape.args)
					if limit == "?" {
						q.args = append(q.args, int64(2))
					}
					inOrder(t, db, o, q)
				}
			}
		}

		// The rows of an order without ties, pinned, also of a column of each type and with NULL.
		for src, want := range map[string]string{
			"select k from o order by i desc, k limit 4":         "int64 8; int64 1; int64 4; int64 10",
			"select k from o order by r, k limit 4":              "int64 3; int64 9; int64 11; int64 6",
			"select s from o where n = 0 order by s desc":        `string "z"; string "a"; string "M"; <nil> <nil>`,
			"select b from o where k < 5 order by b limit 3":     "<nil> <nil>; []uint8 []byte{}; []uint8 []byte{0x1}",
			"select k from o order by n desc, s desc, b limit 2": "int64 5; int64 11",
			"select k from o order by k desc limit 0":            "",
		} {
			got, err := rowsInOrder(t, db, src)
			is.NotError(t, err, src)
			is.Equal(t, want, strings.Join(got, "; "), src)
		}
	})

	t.Run("should take a LIMIT argument of an integer from 0 of each Go type that database/sql converts to one, and refuse every other, the driver's types before the names and the engine's after them", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, orderTable...)
		o := newOracle(t, orderTable...)
		type count uint16
		// numbered is the message want with the number of the argument of LIMIT, n, for its %v.
		numbered := func(want string, n int) string {
			return strings.ReplaceAll(want, "%v", fmt.Sprint(n))
		}
		for _, c := range []struct {
			arg any
			n   int64
		}{
			{arg: int64(2), n: 2}, {arg: int64(0), n: 0}, {arg: int(1), n: 1}, {arg: int8(3), n: 3}, {arg: int16(4), n: 4},
			{arg: int32(5), n: 5}, {arg: uint(6), n: 6}, {arg: uint8(7), n: 7}, {arg: uint16(8), n: 8}, {arg: uint32(9), n: 9},
			{arg: uint64(100), n: 100}, {arg: int64(math.MaxInt64), n: math.MaxInt64}, {arg: uint64(math.MaxInt64), n: math.MaxInt64},
			{arg: count(2), n: 2}, {arg: new(int64(3)), n: 3}, {arg: valuer(1), n: 1},
			{arg: sql.NullInt64{Int64: 4, Valid: true}, n: 4}, {arg: bigValuer(1 << 63), n: 15},
		} {
			for _, by := range []string{"k", "n desc"} {
				q := ordered{columns: "k", table: "o", by: by, limit: "?", args: []any{c.n}}
				got, err := rowsInOrder(t, db, q.src(), c.arg)
				is.NotError(t, err, fmt.Sprintf("%T", c.arg))
				is.Equal(t, "", q.differs(t, o, got), fmt.Sprintf("%T", c.arg))
			}
		}

		// Each refusal is s4ql's "not supported". The engine refuses a value that is no integer from 0
		// when the statement runs, after the errors of the names; the driver refuses a type that no
		// column holds, and an argument that database/sql does not convert, before the engine, so also
		// where the table does not exist.
		for _, c := range []struct {
			arg    any
			want   string
			driver bool
		}{
			{arg: 2.0, want: "argument %v, which is a real, for LIMIT, which takes an integer from 0"},
			{arg: float32(2), want: "argument %v, which is a real, for LIMIT, which takes an integer from 0"},
			{arg: "2", want: "argument %v, which is text, for LIMIT, which takes an integer from 0"},
			{arg: []byte("2"), want: "argument %v, which is a blob, for LIMIT, which takes an integer from 0"},
			{arg: []byte{}, want: "argument %v, which is a blob, for LIMIT, which takes an integer from 0"},
			{arg: nil, want: "argument %v, which is NULL, for LIMIT, which takes an integer from 0"},
			{arg: (*int64)(nil), want: "argument %v, which is NULL, for LIMIT, which takes an integer from 0"},
			{arg: sql.NullInt64{}, want: "argument %v, which is NULL, for LIMIT, which takes an integer from 0"},
			{arg: nullValuer{}, want: "argument %v, which is NULL, for LIMIT, which takes an integer from 0"},
			{arg: int64(-1), want: "argument %v, which is a negative integer, for LIMIT, which takes an integer from 0"},
			{arg: int(-3), want: "argument %v, which is a negative integer, for LIMIT, which takes an integer from 0"},
			{arg: int64(math.MinInt64), want: "argument %v, which is a negative integer, for LIMIT, which takes an integer from 0"},
			{arg: []byte(nil), want: "argument %v of type []byte, which is nil", driver: true},
			{arg: true, want: "argument %v of type bool", driver: true},
			{arg: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC), want: "argument %v of type time.Time", driver: true},
			{arg: uint64(1 << 63), want: "argument %v of type uint64, whose value does not fit a signed 64-bit integer", driver: true},
			{arg: uint(1 << 63), want: "argument %v of type uint, whose value does not fit a signed 64-bit integer", driver: true},
			{arg: failingValuer{}, want: "argument %v of type s4ql_test.failingValuer, which database/sql cannot convert", driver: true},
			{arg: struct{}{}, want: "argument %v of type struct {}, which database/sql cannot convert", driver: true},
			{arg: sql.Named("n", 2), want: `the named argument "n"`, driver: true},
		} {
			what := fmt.Sprintf("%T %v", c.arg, c.arg)
			for _, src := range []string{"select k from o order by k limit ?", "select * from o limit ?", "select k from o where k = ? order by n desc limit ?"} {
				args := []any{c.arg}
				if strings.Count(src, "?") == 2 {
					args = []any{int64(1), c.arg}
				}
				want := numbered(c.want, len(args))
				_, err := db.QueryContext(t.Context(), src, args...)
				is.Equal(t, want, notSupported(t, err), what, src)
				err = db.QueryRowContext(t.Context(), src, args...).Scan(new(any))
				is.Equal(t, want, notSupported(t, err), what, src)
				_, err = db.ExecContext(t.Context(), src, args...)
				is.Equal(t, want, notSupported(t, err), what, src)
				p, err := db.PrepareContext(t.Context(), src)
				is.NotError(t, err)
				_, err = p.QueryContext(t.Context(), args...)
				is.Equal(t, want, notSupported(t, err), what, src)
				is.NotError(t, p.Close())
			}

			_, err := db.QueryContext(t.Context(), "select k from nope order by k limit ?", c.arg)
			if c.driver {
				is.Equal(t, numbered(c.want, 1), notSupported(t, err), what)
			} else {
				sqliteError(t, 1, 1, "no such table: nope", err)
			}
			_, err = db.QueryContext(t.Context(), "select k from o order by x limit ?", c.arg)
			if c.driver {
				is.Equal(t, numbered(c.want, 1), notSupported(t, err), what)
			} else {
				sqliteError(t, 1, 1, "no such column: x", err)
			}
		}
		exec(t, db, t1)
		readersFree(t, db)
	})

	t.Run("should prepare a select with ORDER BY and LIMIT ?, and run it several times with other limits, also in a write transaction that wrote rows and in a read-only transaction", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, orderTable...)
		o := newOracle(t, orderTable...)

		const src = "select k, s from o where n = ? order by s desc, k limit ?"
		p, err := db.PrepareContext(t.Context(), src)
		is.NotError(t, err)
		defer func() { is.NotError(t, p.Close()) }()
		// run the prepared statement p, or its statement in tx, with n and limit, and compare with SQLite.
		run := func(t *testing.T, tx *sql.Tx, n, limit int64) {
			t.Helper()
			s := p
			if tx != nil {
				s = tx.StmtContext(t.Context(), p)
			}
			rows, err := s.QueryContext(t.Context(), n, limit)
			is.NotError(t, err)
			got := []string{}
			for rows.Next() {
				var k, v any
				is.NotError(t, rows.Scan(&k, &v))
				got = append(got, rowText([]any{k, v}))
			}
			is.NotError(t, rows.Err())
			is.NotError(t, rows.Close())
			q := ordered{columns: "k, s", table: "o", where: "n = ?", by: "s desc, k", limit: "?", args: []any{n, limit}}
			is.Equal(t, "", q.differs(t, o, got))
		}
		for _, limit := range []int64{0, 1, 2, 3, 100, 1, 0} {
			for n := range int64(3) {
				run(t, nil, n, limit)
			}
		}

		// In a write transaction, after writes of its own, which the statement reads.
		writes := []string{
			"insert into o values (13, 1, 1.0, 'y', x'03', 1)",
			"insert into o values (14, 1, 1.0, null, x'04', 1)",
			"update o set s = 'zz' where k = 7",
			"delete from o where k = 4",
		}
		tx := begin(t, db, nil)
		execIn(t, tx, writes...)
		o.exec(t, writes...)
		for _, limit := range []int64{2, 0, 100, 1} {
			run(t, tx, 1, limit)
		}
		inOrder(t, tx, o, ordered{columns: "*", table: "o", by: "r desc, k", limit: "?", args: []any{int64(5)}})
		tp, err := tx.PrepareContext(t.Context(), "select k from o order by b desc limit ?")
		is.NotError(t, err)
		for _, limit := range []int64{3, 1, 20} {
			rows, err := tp.QueryContext(t.Context(), limit)
			is.NotError(t, err)
			var got []string
			for rows.Next() {
				var k any
				is.NotError(t, rows.Scan(&k))
				got = append(got, rowText([]any{k}))
			}
			is.NotError(t, rows.Err())
			is.NotError(t, rows.Close())
			is.Equal(t, "", ordered{columns: "k", table: "o", by: "b desc", limit: "?", args: []any{limit}}.differs(t, o, got))
		}
		is.NotError(t, tp.Close())
		is.NotError(t, tx.Commit())
		run(t, nil, 1, 3)

		// In a read-only transaction.
		ro := begin(t, db, readOnly)
		for _, limit := range []int64{1, 3, 0, 100} {
			run(t, ro, 1, limit)
			run(t, ro, 2, limit)
		}
		inOrder(t, ro, o, ordered{columns: "k, i", table: "o", by: "i desc", limit: "?", args: []any{int64(4)}})
		is.NotError(t, ro.Commit())
		exec(t, db, t1)
		readersFree(t, db)
	})

	t.Run("should receive with goqite's statement the message with the highest priority and then the oldest, with and without goqite's index, as SQLite does", func(t *testing.T) {
		const table = "create table goqite (id text primary key, created text not null, updated text not null, queue text not null, " +
			"body blob not null, timeout text not null, received int not null, priority int not null) strict, without rowid"
		const index = "create index goqite_queue_priority_created_idx on goqite (queue, priority desc, created)"
		const receive = "select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1"
		const extend = "update goqite set timeout = ?, received = ? where queue = ? and id = ?"
		const now = "2026-11-21T12:00:00.000Z"

		// message of goqite's table, as far as the receive reads it.
		type message struct {
			id, created, queue, timeout string
			received, priority          int64
		}
		messages := []message{
			{id: "m01", created: "2026-11-21T10:00:05.000Z", queue: "q", timeout: "2026-11-21T10:00:05.000Z", priority: 0},
			{id: "m02", created: "2026-11-21T10:00:01.000Z", queue: "q", timeout: "2026-11-21T10:00:01.000Z", priority: 0},
			{id: "m03", created: "2026-11-21T10:00:09.000Z", queue: "q", timeout: "2026-11-21T10:00:09.000Z", priority: 2},
			{id: "m04", created: "2026-11-21T10:00:03.000Z", queue: "q", timeout: "2026-11-21T10:00:03.000Z", priority: 2},
			{id: "m05", created: "2026-11-21T10:00:02.000Z", queue: "q", timeout: "2026-11-21T13:00:00.000Z", priority: 5},
			{id: "m06", created: "2026-11-21T10:00:04.000Z", queue: "q", timeout: "2026-11-21T10:00:04.000Z", priority: 1, received: 3},
			{id: "m07", created: "2026-11-21T10:00:00.000Z", queue: "r", timeout: "2026-11-21T10:00:00.000Z", priority: 9},
			{id: "m08", created: "2026-11-21T10:00:07.000Z", queue: "q", timeout: "2026-11-21T10:00:07.000Z", priority: 1, received: 1},
			{id: "m09", created: "2026-11-21T10:00:06.000Z", queue: "q", timeout: "2026-11-21T10:00:06.000Z", priority: -1},
			{id: "m10", created: "2026-11-21T10:00:08.000Z", queue: "q", timeout: now, priority: 2},
		}
		var inserts []string
		for _, m := range messages {
			inserts = append(inserts, fmt.Sprintf("insert into goqite values ('%v', '%v', '%v', '%v', x'00', '%v', %v, %v)",
				m.id, m.created, m.created, m.queue, m.timeout, m.received, m.priority))
		}
		// next is the message that the receive must give, as the test computes it: of the queue q, due
		// at now, received fewer than 3 times, with the highest priority, and then the oldest.
		next := func(messages []message) string {
			var due []message
			for _, m := range messages {
				if m.queue == "q" && now >= m.timeout && m.received < 3 {
					due = append(due, m)
				}
			}
			slices.SortFunc(due, func(a, b message) int {
				if a.priority != b.priority {
					return int(b.priority - a.priority)
				}
				return strings.Compare(a.created, b.created)
			})
			if len(due) == 0 {
				return ""
			}
			return due[0].id
		}

		for _, indexed := range []bool{false, true} {
			t.Run(fmt.Sprint("indexed: ", indexed), func(t *testing.T) {
				w := newWorld(t)
				db := w.serve(t, "a")
				statements := append([]string{table}, inserts...)
				if indexed {
					statements = append(statements, index)
				}
				exec(t, db, statements...)
				o := newOracle(t, statements...)
				messages := slices.Clone(messages)

				p, err := db.PrepareContext(t.Context(), receive)
				is.NotError(t, err)
				defer func() { is.NotError(t, p.Close()) }()
				q := ordered{columns: "id", table: "goqite", where: "queue = ? and ? >= timeout and received < ?", by: "priority desc, created", limit: "1",
					args: []any{"q", now, int64(3)}}
				is.Equal(t, receive, q.src())

				// Receive until no message is due, and extend each received message past now, as goqite does,
				// once in a transaction.
				var order []string
				for round := 0; ; round++ {
					want := next(messages)
					var id string
					err := p.QueryRowContext(t.Context(), "q", now, 3).Scan(&id)
					if want == "" {
						is.Error(t, sql.ErrNoRows, err)
						break
					}
					is.NotError(t, err)
					is.Equal(t, want, id, fmt.Sprint("round ", round))
					is.Equal(t, "", q.differs(t, o, []string{rowText([]any{id})}))
					got := inOrder(t, db, o, q)
					is.Equal(t, rowText([]any{want}), got[0])
					order = append(order, id)

					i := slices.IndexFunc(messages, func(m message) bool { return m.id == id })
					messages[i].timeout, messages[i].received = "2026-11-21T12:30:00.000Z", messages[i].received+1
					args := []any{messages[i].timeout, messages[i].received, "q", id}
					if round == 2 {
						tx := begin(t, db, nil)
						var again string
						is.NotError(t, tx.QueryRowContext(t.Context(), receive, "q", now, 3).Scan(&again))
						is.Equal(t, id, again)
						res, err := tx.ExecContext(t.Context(), extend, args...)
						is.Equal(t, int64(1), affected(t, res, err))
						// The transaction reads its own extend, and receives the next message.
						err = tx.QueryRowContext(t.Context(), receive, "q", now, 3).Scan(&again)
						if n := next(messages); n == "" {
							is.Error(t, sql.ErrNoRows, err)
						} else {
							is.NotError(t, err)
							is.Equal(t, n, again)
						}
						is.NotError(t, tx.Commit())
					} else {
						res, err := db.ExecContext(t.Context(), extend, args...)
						is.Equal(t, int64(1), affected(t, res, err))
					}
					o.exec(t, fmt.Sprintf("update goqite set timeout = '%v', received = %v where queue = 'q' and id = '%v'", args[0], args[1], id))
				}
				is.Equal(t, "m04 m10 m03 m08 m02 m01 m09", strings.Join(order, " "))
			})
		}
	})

	t.Run("should give SQLite's error for a column of ORDER BY that the table does not have, after the errors of the select list and of WHERE, through Query and a prepared statement", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, orderTable...)
		o := newOracle(t, orderTable...)
		for src, message := range map[string]string{
			"select k from o order by x":                       "no such column: x",
			"select k from o order by x desc limit 0":          "no such column: x",
			`select k from o order by "x" desc limit 1`:        `no such column: "x" - should this be a string literal in single-quotes?`,
			"select k from o order by n, x, y":                 "no such column: x",
			"select k from o where n = ? order by x limit ?":   "no such column: x",
			"select y from o where z = 1 order by x":           "no such column: y",
			"select k from o where z is null order by x":       "no such column: z",
			"select k from nope order by x":                    "no such table: nope",
			"select k from o where n = 1 order by x, k desc":   "no such column: x",
			"select * from o where k > 0 order by K, S, nope2": "no such column: nope2",
		} {
			var args []any
			for range strings.Count(src, "?") {
				args = append(args, int64(1))
			}
			_, want := o.rows(t, src, args...)
			is.True(t, want != nil, src)
			is.Equal(t, s4ql.Error{Code: 1, ExtendedCode: 1, Message: message}, *want, src)

			_, err := db.QueryContext(t.Context(), src, args...)
			sqliteError(t, 1, 1, message, err)
			err = db.QueryRowContext(t.Context(), src, args...).Scan(new(any))
			sqliteError(t, 1, 1, message, err)
			p, err := db.PrepareContext(t.Context(), src)
			is.NotError(t, err, src)
			_, err = p.QueryContext(t.Context(), args...)
			sqliteError(t, 1, 1, message, err)
			_, err = p.ExecContext(t.Context(), args...)
			sqliteError(t, 1, 1, message, err)
			is.NotError(t, p.Close())
		}
		exec(t, db, t1)
		readersFree(t, db)
	})
}

func TestReturning(t *testing.T) {
	const create = "create table r (k int primary key, n integer, x real default 0.5, s text not null default 'x', b blob) strict, without rowid"

	t.Run("should give the rows of RETURNING through Query, directly, prepared, and in a transaction, with the columns and types of a select of them, and drop them in Exec", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, create, "create index rs on r (s)")
		is.Equal(t, "k INT, n INTEGER, x REAL, s TEXT, b BLOB: 1 <nil> 0.5 x <nil>", rowsText(t, db, "insert into r (k) values (?) returning *", 1))
		is.Equal(t, "s TEXT, k INT, s TEXT: a 2 a", rowsText(t, db, "insert into r (k, s, x) values (2, ?, -0.0) returning s, K, s", "a"))
		res, err := db.ExecContext(t.Context(), "insert into r (k, b) values (3, ?) returning b", []byte{1})
		is.Equal(t, int64(1), affected(t, res, err))

		p, err := db.PrepareContext(t.Context(), "update r set n = ? where k >= ? returning k, n, x")
		is.NotError(t, err)
		rows, err := p.QueryContext(t.Context(), 7, 2)
		is.NotError(t, err)
		types, err := rows.ColumnTypes()
		is.NotError(t, err)
		is.Equal(t, "INT INTEGER REAL", types[0].DatabaseTypeName()+" "+types[1].DatabaseTypeName()+" "+types[2].DatabaseTypeName())
		var got []int64
		for rows.Next() {
			var k, n int64
			var x float64
			is.NotError(t, rows.Scan(&k, &n, &x))
			is.Equal(t, int64(7), n)
			got = append(got, k)
		}
		is.NotError(t, rows.Err())
		is.NotError(t, rows.Close())
		is.True(t, inKeyOrder(got), fmt.Sprint(got))
		is.Equal(t, 2, len(got))
		// The same values again count as a change of each row, and give each row.
		res, err = p.ExecContext(t.Context(), 7, 2)
		is.Equal(t, int64(2), affected(t, res, err))
		is.NotError(t, p.Close())
		// No row: the columns are known.
		is.Equal(t, "k INT: ", rowsText(t, db, "delete from r where k > 100 returning k"))

		tx, err := db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		is.Equal(t, "k INT, s TEXT: 4 y", rowsText(t, tx, "insert into r (k, s) values (4, 'y') returning k, s"))
		is.Equal(t, "k INT, s TEXT: 1 z; 2 z; 3 z; 4 z", rowsText(t, tx, "update r set s = 'z' returning k, s"))
		is.NotError(t, tx.Rollback())
		is.Equal(t, "k INT, s TEXT: 1 x; 2 a; 3 x", rowsText(t, db, "select k, s from r"))
		tx, err = db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		is.Equal(t, "k INT, n INTEGER: 1 <nil>", rowsText(t, tx, "delete from r where s = ? and n is null returning k, n", "x"))
		is.NotError(t, tx.Commit())
		is.Equal(t, "k INT, s TEXT: 2 a; 3 x", rowsText(t, db, "select k, s from r"))

		// Another instance replays the changes of the statements with RETURNING.
		is.NotError(t, db.Close())
		other := w.serve(t, "b")
		is.Equal(t, "k INT, n INTEGER, x REAL, s TEXT, b BLOB: 2 7 0 a <nil>; 3 7 0.5 x [1]", rowsText(t, other, "select * from r"))
	})

	t.Run("should give SQLite's errors of a statement with RETURNING, and no rows", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, create, "insert into r (k) values (1)")
		_, err := db.QueryContext(t.Context(), "insert into r (k) values (1) returning k")
		sqliteError(t, 19, 1555, "UNIQUE constraint failed: r.k", failed(t, err))
		_, err = db.QueryContext(t.Context(), "update r set s = null returning k")
		sqliteError(t, 19, 1299, "NOT NULL constraint failed: r.s", failed(t, err))
		_, err = db.QueryContext(t.Context(), "delete from r returning nope")
		sqliteError(t, 1, 1, "no such column: nope", failed(t, err))
		_, err = db.QueryContext(t.Context(), "update r set s = 1 returning k")
		is.Equal(t, "an integer literal for the column r.s of type TEXT", notSupported(t, err))
		tx, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		is.NotError(t, err)
		_, err = tx.QueryContext(t.Context(), "delete from r returning k")
		sqliteError(t, 8, 8, "attempt to write a readonly database", failed(t, err))
		is.NotError(t, tx.Rollback())
		is.Equal(t, "k INT: 1", rowsText(t, db, "select k from r"))
	})

	t.Run("should give no rows where the outcome of the commit is unknown, and commit the statement once", func(t *testing.T) {
		w := newWorld(t)
		s := &faulty{Store: w.client()}
		db := w.openOn(t, "a", s, coreOptions())
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, t1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		s.loseNext(cancel)
		rows, err := db.QueryContext(ctx, "insert into t1 (v1) values (1) returning v1")
		is.Error(t, s4ql.ErrOutcomeUnknown, failed(t, err))
		is.True(t, rows == nil)
		s.down(false)
		is.NotError(t, db.PingContext(t.Context()))
		is.EqualSlice(t, []int64{1}, values(t, db, select1))
		is.Equal(t, 2, len(commits(t, w)))
	})

	t.Run("should give the rows of a committed statement with RETURNING when the database closes between the commit and the end of Query", func(t *testing.T) {
		w := newWorld(t)
		s := &faulty{Store: w.client()}
		db, c, err := s4ql.OpenStoreConnector(t.Context(), w.host, s, w.options("a"), coreOptions())
		is.NotError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, t1)
		closed := make(chan error, 1)
		// The close begins once the commit is applied, and before the instance knows it: the
		// connector is closed when Query has its rows from the engine.
		s.afterSwap(func() {
			go func() { closed <- db.Close() }()
			for !c.Closing() {
				runtime.Gosched()
			}
		})
		rows, err := db.QueryContext(t.Context(), "insert into t1 (v1) values (1) returning v1")
		is.NotError(t, err)
		var got []int64
		for rows.Next() {
			var v int64
			is.NotError(t, rows.Scan(&v))
			got = append(got, v)
		}
		is.NotError(t, rows.Err())
		is.NotError(t, rows.Close())
		is.EqualSlice(t, []int64{1}, got)
		is.NotError(t, <-closed)
		other := w.serve(t, "b")
		is.EqualSlice(t, []int64{1}, values(t, other, select1))
	})

	t.Run("should commit a statement with RETURNING whose Query returns no error, also when its context ends once the commit is applied, whatever its rows then give", func(t *testing.T) {
		w := newWorld(t)
		s := &faulty{Store: w.client()}
		db := w.openOn(t, "a", s, coreOptions())
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, t1)

		ctx, cancel := context.WithCancel(t.Context())
		s.afterSwap(cancel)
		rows, err := db.QueryContext(ctx, "insert into t1 (v1) values (1) returning v1")
		is.NotError(t, err)
		// database/sql closes the rows of a context that has ended, at a time of its own, so they give
		// the row or the error of the context, also between Next and Scan, which says nothing about
		// the commit.
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				is.Error(t, context.Canceled, err)
				continue
			}
			is.Equal(t, int64(1), v)
		}
		if err := rows.Err(); err != nil {
			is.Error(t, context.Canceled, err)
		}
		_ = rows.Close()
		is.EqualSlice(t, []int64{1}, values(t, db, select1))

		ctx, cancel = context.WithCancel(t.Context())
		s.afterSwap(cancel)
		var v int64
		if err := db.QueryRowContext(ctx, "insert into t1 (v1) values (2) returning v1").Scan(&v); err != nil {
			is.Error(t, context.Canceled, err)
		} else {
			is.Equal(t, int64(2), v)
		}
		is.EqualSlice(t, []int64{1, 2}, values(t, db, select1))
		is.Equal(t, 3, len(commits(t, w)))
	})
}

func TestSubquery(t *testing.T) {
	const (
		table = "create table goqite (id text primary key, created text not null, updated text not null, queue text not null, " +
			"body blob not null, timeout text not null, received integer not null, priority integer not null) strict, without rowid"
		index   = "create index goqite_queue_priority_created_idx on goqite (queue, priority desc, created)"
		send    = "insert into goqite (id, created, updated, queue, body, timeout, received, priority) values (?, ?, ?, ?, ?, ?, 0, ?) returning id"
		receive = "update goqite set timeout = ?, received = ? where queue = ? and id = (select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1) returning id, body, received"
		now     = "2026-11-24T12:00:00.000Z"
		later   = "2026-11-24T12:01:00.000Z"
	)
	// sendAll sends the messages m_1, of priority 0, m_2 and m_3, of priority 1, the earlier of which is
	// m_3, and o_1 to another queue.
	sendAll := func(t *testing.T, db querier) {
		t.Helper()
		for _, m := range []struct {
			id, queue, created string
			priority           int
		}{{"m_1", "default", "2026-11-24T11:50:00.000Z", 0}, {"m_2", "default", "2026-11-24T11:52:00.000Z", 1}, {"m_3", "default", "2026-11-24T11:51:00.000Z", 1}, {"o_1", "other", "2026-11-24T11:40:00.000Z", 5}} {
			is.Equal(t, "id TEXT: "+m.id, rowsText(t, db, send, m.id, m.created, m.created, m.queue, []byte("body "+m.id), now, m.priority))
		}
	}

	t.Run("should run goqite's receive without its arithmetic, each receive picking the next message, and none on an empty queue, directly, prepared, and in a transaction", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, table, index)
		is.Equal(t, "id TEXT, body BLOB, received INTEGER: ", rowsText(t, db, receive, later, 1, "default", "default", now, 3))
		sendAll(t, db)
		is.Equal(t, "id TEXT, body BLOB, received INTEGER: m_3 [98 111 100 121 32 109 95 51] 1", rowsText(t, db, receive, later, 1, "default", "default", now, 3))

		p, err := db.PrepareContext(t.Context(), receive)
		is.NotError(t, err)
		var id string
		var body []byte
		var received int64
		is.NotError(t, p.QueryRowContext(t.Context(), later, 1, "default", "default", now, 3).Scan(&id, &body, &received))
		is.Equal(t, "m_2 body m_2 1", fmt.Sprint(id, " ", string(body), " ", received))

		tx, err := db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		is.NotError(t, tx.StmtContext(t.Context(), p).QueryRowContext(t.Context(), later, 1, "default", "default", now, 3).Scan(&id, &body, &received))
		is.Equal(t, "m_1", id)
		// The queue is empty in the transaction, and the message is back after its roll back.
		err = tx.StmtContext(t.Context(), p).QueryRowContext(t.Context(), later, 1, "default", "default", now, 3).Scan(&id, &body, &received)
		is.Error(t, sql.ErrNoRows, err)
		is.NotError(t, tx.Rollback())
		res, err := p.ExecContext(t.Context(), later, 1, "default", "default", now, 3)
		is.Equal(t, int64(1), affected(t, res, err))
		res, err = p.ExecContext(t.Context(), later, 1, "default", "default", now, 3)
		is.Equal(t, int64(0), affected(t, res, err))
		is.NotError(t, p.Close())
		// The other queue has its message, and a message whose timeout has passed is received again.
		is.Equal(t, "id TEXT, body BLOB, received INTEGER: o_1 [98 111 100 121 32 111 95 49] 1", rowsText(t, db, receive, later, 1, "other", "other", now, 3))
		is.Equal(t, "id TEXT: m_3", rowsText(t, db, "update goqite set timeout = ?, received = ? where queue = ? and id = (select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1) returning id",
			"2026-11-24T12:02:00.000Z", 2, "default", "default", later, 3))

		// Another instance, with an empty cache, replays the receives, and runs a receive on them.
		is.NotError(t, db.Close())
		other := w.serve(t, "b")
		is.Equal(t, "id TEXT, received INTEGER, timeout TEXT: m_1 1 2026-11-24T12:01:00.000Z; m_2 1 2026-11-24T12:01:00.000Z; m_3 2 2026-11-24T12:02:00.000Z; o_1 1 2026-11-24T12:01:00.000Z",
			rowsText(t, other, "select id, received, timeout from goqite"))
		is.Equal(t, "id TEXT, body BLOB, received INTEGER: m_2 [98 111 100 121 32 109 95 50] 2", rowsText(t, other, receive, "2026-11-24T12:03:00.000Z", 2, "default", "default", "2026-11-24T12:01:30.000Z", 3))
	})

	t.Run("should run a select and a delete with a subquery, with arguments before, inside, and after it", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, table, index)
		sendAll(t, db)
		is.Equal(t, "id TEXT: m_2; m_3", rowsText(t, db, "select id from goqite where priority = (select priority from goqite where queue = ? order by priority desc limit 1) and queue = ?", "default", "default"))
		is.Equal(t, "id TEXT: ", rowsText(t, db, "select id from goqite where priority = (select priority from goqite where queue = ?)", "nope"))
		res, err := db.ExecContext(t.Context(), "delete from goqite where queue = ? and id = (select id from goqite where queue = ? order by created limit 1)", "default", "default")
		is.Equal(t, int64(1), affected(t, res, err))
		is.Equal(t, "id TEXT: m_2; m_3; o_1", rowsText(t, db, "select id from goqite"))
	})

	t.Run("should give SQLite's errors and s4ql's refusals of a statement with a subquery", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, table, index)
		_, err := db.ExecContext(t.Context(), "delete from goqite where id = (select id from nope)")
		is.Equal(t, "no such table: nope", sqliteMessage(t, err))
		_, err = db.ExecContext(t.Context(), "delete from goqite where id = (select * from goqite)")
		is.Equal(t, "sub-select returns 8 columns - expected 1", sqliteMessage(t, err))
		_, err = db.QueryContext(t.Context(), "select id from goqite where id = (select * from goqite)")
		is.Equal(t, "row value misused", sqliteMessage(t, err))
		_, err = db.QueryContext(t.Context(), "select id from goqite where priority = (select received from goqite where id = ?)", 1)
		is.Equal(t, "argument 1, which is an integer, compared with the column goqite.id of type TEXT", typeRefused(t, err))
		_, err = db.QueryContext(t.Context(), "select id from goqite where queue = (select priority from goqite)")
		is.Equal(t, "subquery 1, of the column goqite.priority of type INTEGER, compared with the column goqite.queue of type TEXT", typeRefused(t, err))
	})
}

func TestArithmetic(t *testing.T) {
	const (
		table = "create table goqite (id text primary key, created text not null, updated text not null, queue text not null, " +
			"body blob not null, timeout text not null, received integer not null, priority integer not null) strict, without rowid"
		index   = "create index goqite_queue_priority_created_idx on goqite (queue, priority desc, created)"
		send    = "insert into goqite (id, created, updated, queue, body, timeout, received, priority) values (?, ?, ?, ?, ?, ?, 0, ?) returning id"
		receive = "update goqite set timeout = ?, received = received + 1 where queue = ? and id = (select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1) returning id, body, received"
		now     = "2026-11-25T12:00:00.000Z"
	)
	// at is the time of the n-th minute after now.
	at := func(n int) string {
		return fmt.Sprintf("2026-11-25T12:%02d:00.000Z", n)
	}

	t.Run("should run goqite's whole receive, which counts received up until the message is received as often as allowed, directly, prepared, in a transaction, and after a restart with an empty cache", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, table, index)
		is.Equal(t, "id TEXT, body BLOB, received INTEGER: ", rowsText(t, db, receive, at(1), "default", "default", now, 3))
		for _, m := range []struct {
			id, created string
			priority    int
		}{{"m_1", "2026-11-25T11:50:00.000Z", 0}, {"m_2", "2026-11-25T11:51:00.000Z", 1}} {
			is.Equal(t, "id TEXT: "+m.id, rowsText(t, db, send, m.id, m.created, m.created, "default", []byte("body "+m.id), now, m.priority))
		}

		// m_2 has the higher priority; once received, it is not visible until its timeout.
		is.Equal(t, "id TEXT, body BLOB, received INTEGER: m_2 [98 111 100 121 32 109 95 50] 1", rowsText(t, db, receive, at(1), "default", "default", now, 3))
		p, err := db.PrepareContext(t.Context(), receive)
		is.NotError(t, err)
		var id string
		var body []byte
		var received int64
		is.NotError(t, p.QueryRowContext(t.Context(), at(1), "default", "default", now, 3).Scan(&id, &body, &received))
		is.Equal(t, "m_1 1", fmt.Sprint(id, " ", received))
		err = p.QueryRowContext(t.Context(), at(2), "default", "default", now, 3).Scan(&id, &body, &received)
		is.Error(t, sql.ErrNoRows, err)

		// After the timeout, m_2 is received again, and its count goes up, in a transaction too, and back
		// after its roll back.
		tx, err := db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		is.NotError(t, tx.StmtContext(t.Context(), p).QueryRowContext(t.Context(), at(2), "default", "default", at(1), 3).Scan(&id, &body, &received))
		is.Equal(t, "m_2 2", fmt.Sprint(id, " ", received))
		is.NotError(t, tx.Rollback())
		for _, want := range []string{"m_2 2", "m_1 2"} {
			is.NotError(t, p.QueryRowContext(t.Context(), at(2), "default", "default", at(1), 3).Scan(&id, &body, &received))
			is.Equal(t, want, fmt.Sprint(id, " ", received))
		}
		res, err := p.ExecContext(t.Context(), at(3), "default", "default", at(2), 3)
		is.Equal(t, int64(1), affected(t, res, err))
		is.Equal(t, "id TEXT, received INTEGER: m_1 2; m_2 3", rowsText(t, db, "select id, received from goqite"))
		// m_2 has been received three times, and is received no more.
		is.Equal(t, "id TEXT, body BLOB, received INTEGER: m_1 [98 111 100 121 32 109 95 49] 3", rowsText(t, db, receive, at(4), "default", "default", at(3), 3))
		is.Equal(t, "id TEXT, body BLOB, received INTEGER: ", rowsText(t, db, receive, at(5), "default", "default", at(4), 3))
		is.NotError(t, p.Close())
		notes := annotations(t, w)
		is.Equal(t, receive, notes[len(notes)-1])

		// Another instance, with an empty cache, replays the receives, which the commits hold as rows.
		is.NotError(t, db.Close())
		other := w.serve(t, "b")
		is.Equal(t, "id TEXT, received INTEGER, timeout TEXT: m_1 3 2026-11-25T12:04:00.000Z; m_2 3 2026-11-25T12:03:00.000Z", rowsText(t, other, "select id, received, timeout from goqite"))
		is.Equal(t, "id TEXT: m_1; m_2", rowsText(t, other, "select id from goqite where received + 0 = 3 and received * 2 > ? - 1", 5))
	})

	t.Run("should give an evaluation error that matches ErrEvaluation and its kind through Exec, Query, and QueryRow, change nothing, and leave a transaction usable", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, "create table t (k int primary key, a int, r real) strict, without rowid", "insert into t values (1, 9223372036854775807, 1e308)", "insert into t values (2, 0, 2.5)")
		cases := []struct {
			src  string
			args []any
			kind error
			msg  string
		}{
			{"update t set a = a + 1 returning k", nil, s4ql.ErrOverflow, "s4ql: integer overflow in + (at offset 17)"},
			{"update t set a = 1 / a returning k", nil, s4ql.ErrDivisionByZero, "s4ql: division by zero in / (at offset 17)"},
			{"update t set r = r * ? where k = 1 returning k", []any{10}, s4ql.ErrNotFinite, "s4ql: real result that is not finite in * (at offset 17)"},
			{"delete from t where k = 1 + ? returning k", []any{int64(math.MaxInt64)}, s4ql.ErrOverflow, "s4ql: integer overflow in + (at offset 24)"},
			{"select k from t where k > 0 and k / a > 0", nil, s4ql.ErrDivisionByZero, "s4ql: division by zero in / (at offset 32)"},
			{"select k from t where k = (select k from t where a - ? < 0)", []any{int64(math.MinInt64)}, s4ql.ErrOverflow, "s4ql: integer overflow in - (at offset 49)"},
		}
		state := "k INT, a INT, r REAL: 1 9223372036854775807 1e+308; 2 0 2.5"
		check := func(how, src string, err error, kind error, msg string) {
			t.Helper()
			is.Error(t, s4ql.ErrEvaluation, err, how+": "+src)
			is.Error(t, kind, err, how+": "+src)
			is.Equal(t, msg, err.Error(), how+": "+src)
			_, isSQLite := errors.AsType[*s4ql.Error](err)
			is.True(t, !isSQLite, how+": "+src)
			for _, other := range []error{s4ql.ErrNotSupported, s4ql.ErrOverflow, s4ql.ErrDivisionByZero, s4ql.ErrNotFinite} {
				is.True(t, other == kind || !errors.Is(err, other), how+": "+src)
			}
		}
		for _, c := range cases {
			_, err := db.ExecContext(t.Context(), c.src, c.args...)
			check("Exec", c.src, err, c.kind, c.msg)
			_, err = rowsInOrder(t, db, c.src, c.args...)
			check("Query", c.src, err, c.kind, c.msg)
			var k int64
			check("QueryRow", c.src, db.QueryRowContext(t.Context(), c.src, c.args...).Scan(&k), c.kind, c.msg)

			tx, err := db.BeginTx(t.Context(), nil)
			is.NotError(t, err)
			_, err = tx.ExecContext(t.Context(), "insert into t values (3, -3, 3.5)")
			is.NotError(t, err)
			_, err = tx.ExecContext(t.Context(), c.src, c.args...)
			check("Exec in a transaction", c.src, err, c.kind, c.msg)
			check("QueryRow in a transaction", c.src, tx.QueryRowContext(t.Context(), c.src, c.args...).Scan(&k), c.kind, c.msg)
			// The transaction is usable, and holds its insert and nothing of the failed statements.
			res, err := tx.ExecContext(t.Context(), "update t set a = a - 1 where k = 3")
			is.Equal(t, int64(1), affected(t, res, err))
			is.Equal(t, state+"; 3 -4 3.5", rowsText(t, tx, "select * from t"))
			is.NotError(t, tx.Rollback())
			is.Equal(t, state, rowsText(t, db, "select * from t"))
		}
	})

	t.Run("should store -0.0 from arithmetic in a REAL column as 0.0, as SQLite does, in the cache, in the commit object, after a replay, and through RETURNING", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		setUp := []string{"create table t (k int primary key, r real) strict, without rowid", "insert into t values (1, 1.5)"}
		exec(t, db, setUp...)
		o := newOracle(t, setUp...)
		for _, src := range []string{"update t set r = -0.0 * 1 returning r", "select r from t", "select r from t where r = 0.0 * -1"} {
			want, se := o.rows(t, src)
			is.True(t, se == nil, src)
			got, err := rowsInOrder(t, db, src)
			is.NotError(t, err, src)
			is.EqualSlice(t, want, got, src)
			is.EqualSlice(t, []string{"float64 0"}, got, src)
		}
		var r float64
		is.NotError(t, db.QueryRowContext(t.Context(), "update t set r = r * -1 returning r").Scan(&r))
		is.True(t, r == 0 && !math.Signbit(r))
		is.EqualSlice(t, []string{"real 0"}, committedReals(t, w))
		is.NotError(t, db.Close())
		other := w.serve(t, "b")
		is.NotError(t, other.QueryRowContext(t.Context(), "select r from t").Scan(&r))
		is.True(t, r == 0 && !math.Signbit(r))
	})
}

func TestText(t *testing.T) {
	const table = "create table goqite (id text primary key, queue text not null, body blob not null) strict, without rowid"
	// id is goqite's id of the 16 bytes of the blob b, as its default computes it from
	// randomblob(16).
	id := func(b []byte) string {
		return "m_" + hex.EncodeToString(b)
	}
	first, second := []byte("0123456789abcdef"), []byte{0, 1, 2, 0xab, 0xcd, 0xef, 0xff, 9, 10, 11, 12, 13, 14, 15, 16, 17}

	t.Run("should compute goqite's id, 'm_' || lower(hex(?)) of 16 random bytes, in SET and in WHERE, as SQLite does, directly, prepared, in a transaction, and after a restart with an empty cache", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		setUp := []string{table, "insert into goqite values ('m_', 'default', x'00')"}
		exec(t, db, setUp...)
		o := newOracle(t, setUp...)
		same := func(src string, args ...any) []string {
			t.Helper()
			want, se := o.rows(t, src, args...)
			is.True(t, se == nil, src)
			got, err := rowsInOrder(t, db, src, args...)
			is.NotError(t, err, src)
			is.EqualSlice(t, want, got, src)
			return got
		}
		is.EqualSlice(t, []string{fmt.Sprintf("string %q", id(first))}, same("update goqite set id = 'm_' || lower(hex(?)) where id = 'm_' returning id", first))
		is.Equal(t, 1, len(same("select id, body from goqite where id = 'm_' || lower(hex(?))", first)))
		is.EqualSlice(t, []string{}, same("select id from goqite where id = 'm_' || lower(hex(?))", second))

		p, err := db.PrepareContext(t.Context(), "update goqite set id = 'm_' || lower(hex(?)) where id = 'm_' || lower(hex(?))")
		is.NotError(t, err)
		res, err := p.ExecContext(t.Context(), second, first)
		is.Equal(t, int64(1), affected(t, res, err))
		_, se := o.rows(t, "update goqite set id = 'm_' || lower(hex(?)) where id = 'm_' || lower(hex(?))", second, first)
		is.True(t, se == nil)
		res, err = p.ExecContext(t.Context(), second, first)
		is.Equal(t, int64(0), affected(t, res, err))
		is.NotError(t, p.Close())
		same("select id from goqite")
		is.Equal(t, "id TEXT: "+id(second), rowsText(t, db, "select id from goqite"))

		// In a transaction, rolled back, and with the arguments of the driver's types: a string for
		// lower, and nil, which || gives as NULL and hex as the empty text.
		tx := begin(t, db, nil)
		var got string
		is.NotError(t, tx.QueryRowContext(t.Context(), "update goqite set queue = lower(?) || queue where id = ? returning queue", "OTHER_", id(second)).Scan(&got))
		is.Equal(t, "other_default", got)
		_, err = tx.ExecContext(t.Context(), "update goqite set queue = ? || queue", nil)
		sqliteError(t, 19, 1299, "NOT NULL constraint failed: goqite.queue", err)
		is.NotError(t, tx.QueryRowContext(t.Context(), "update goqite set queue = hex(?) returning queue", nil).Scan(&got))
		is.Equal(t, "", got)
		is.NotError(t, tx.Rollback())
		is.Equal(t, "id TEXT, queue TEXT: "+id(second)+" default", rowsText(t, db, "select id, queue from goqite"))

		// The commits hold the rows, which another instance replays into an empty cache.
		is.NotError(t, db.Close())
		other := w.serve(t, "b")
		is.Equal(t, "id TEXT: "+id(second), rowsText(t, other, "select id from goqite where id = 'm_' || lower(hex(?))", second))
	})

	t.Run("should refuse text against a number, and a number in text, with ErrNotSupported, through Prepare, Exec, and Query", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, table, "create table n (k int primary key, s text) strict, without rowid")
		p, err := db.PrepareContext(t.Context(), "update goqite set id = 'm_' || lower(hex(body)) where queue = ?")
		is.NotError(t, err)
		is.NotError(t, p.Close())
		_, err = db.ExecContext(t.Context(), "update n set k = lower(s)")
		is.Equal(t, "an expression of type text for the column n.k of type INT", typeRefused(t, err))
		_, err = rowsInOrder(t, db, "select k from n where lower(s) = ?", 1)
		is.Equal(t, "argument 1, which is an integer, compared with an expression of type text", typeRefused(t, err))
		_, err = rowsInOrder(t, db, "select k from n where s = hex(?)", 1.5)
		is.Equal(t, "argument 1 in a call of hex, which is a real (at offset 30)", typeRefused(t, err))
		_, err = db.ExecContext(t.Context(), "delete from n where k || s = 'a'")
		is.Equal(t, "column of type INT in a concatenation (at offset 20)", typeRefused(t, err))
		_, err = db.ExecContext(t.Context(), "delete from nosuch where lower(queue) = ?", "default")
		is.Equal(t, "no such table: nosuch", sqliteMessage(t, err))
	})

	t.Run("should give SQLite's string or blob too big for text longer than the limit, change nothing, and keep the transaction usable", func(t *testing.T) {
		const limit = 1000
		w := newWorld(t)
		co := coreOptions()
		co.LengthLimit = limit
		db := w.openOn(t, "a", w.client(), co)
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, "create table t (k int primary key, s text) strict, without rowid", "insert into t values (1, 'a')", "insert into t values (2, 'b')")
		_, err := rowsInOrder(t, db, "select k from t where s = hex(?)", make([]byte, limit/2))
		sqliteError(t, 18, 18, "string or blob too big", err)
		_, err = db.ExecContext(t.Context(), "update t set s = ? || ? || s where k = 99", strings.Repeat("a", 600), strings.Repeat("b", 600))
		sqliteError(t, 18, 18, "string or blob too big", err)
		tx := begin(t, db, nil)
		_, err = tx.ExecContext(t.Context(), "update t set s = ? where k = 2", strings.Repeat("c", 600))
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), "update t set s = s || s")
		sqliteError(t, 18, 18, "string or blob too big", err)
		is.Equal(t, "k INT, s TEXT: 1 a; 2 "+strings.Repeat("c", 600), rowsText(t, tx, "select * from t"))
		is.NotError(t, tx.Commit())
	})
}

// committedReals of the last commit object of the log of w: each real of each row that it upserts, as
// "real" and its value, with the sign of a zero, in the order of the commit object.
func committedReals(t *testing.T, w *world) []string {
	t.Helper()
	data, _, ok := w.store.Object("db/manifest.json")
	is.True(t, ok)
	m, err := format.ParseManifest(data)
	is.NotError(t, err)
	key := m.Head.String()
	data, _, ok = w.store.Object("db/commits/" + key[:2] + "/" + key)
	is.True(t, ok, key)
	r := format.NewCommitReader(bytes.NewReader(data), m.Head)
	_, err = r.Header()
	is.NotError(t, err)
	var reals []string
	for {
		c, _, err := r.Next()
		if errors.Is(err, io.EOF) {
			return reals
		}
		is.NotError(t, err)
		for _, row := range c.GetUpsertRows().GetRows() {
			for _, v := range row.GetValues() {
				if v.HasReal() {
					reals = append(reals, "real "+strconv.FormatFloat(v.GetReal(), 'g', -1, 64))
				}
			}
		}
	}
}

// sqliteMessage of err, which must be SQLite's error: its message.
func sqliteMessage(t *testing.T, err error) string {
	t.Helper()
	se, ok := errors.AsType[*s4ql.Error](err)
	is.True(t, ok, fmt.Sprint(err))
	return se.Message
}

// typeRefused of err, which must be s4ql's "not supported" of a value of another type than its
// column: its message without the prefix.
func typeRefused(t *testing.T, err error) string {
	t.Helper()
	is.Error(t, s4ql.ErrNotSupported, err)
	return strings.TrimPrefix(err.Error(), "s4ql: not supported: ")
}

func TestConn_Raw(t *testing.T) {
	t.Run("should run the calls of the driver's interfaces that database/sql makes no more, as it runs those with a context", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)

		is.NotError(t, conn.Raw(func(driverConn any) error {
			c := driverConn.(driver.Conn)
			tx, err := c.Begin() //nolint:staticcheck // The call is deprecated, and the driver has it.
			is.NotError(t, err)
			is.NotError(t, tx.Rollback())
			_, err = c.Prepare("selec")
			is.Equal(t, `near "selec": syntax error`, failed(t, err).Error())

			insert, err := c.Prepare(insert1)
			is.NotError(t, err)
			is.Equal(t, -1, insert.NumInput())
			res, err := insert.Exec(nil) //nolint:staticcheck // As above.
			is.Equal(t, int64(1), affected(t, res, err))
			_, err = insert.Exec([]driver.Value{int64(1)}) //nolint:staticcheck // As above.
			is.Equal(t, "s4ql: wrong number of arguments: 1 for a statement with 0 parameters", failed(t, err).Error())
			is.NotError(t, insert.Close())

			// The arguments go to the statement by position, with no name.
			insertParameter, err := c.Prepare("insert into t1 (v1) values (?)")
			is.NotError(t, err)
			_, err = insertParameter.Exec([]driver.Value{int64(1)}) //nolint:staticcheck // As above.
			is.Equal(t, "UNIQUE constraint failed: t1.v1", failed(t, err).Error())
			_, err = insertParameter.Query([]driver.Value{true}) //nolint:staticcheck // As above.
			is.Equal(t, "argument 1 of type bool", notSupported(t, err))
			_, err = insertParameter.Query([]driver.Value{"1"}) //nolint:staticcheck // As above.
			is.Equal(t, "argument 1, which is text, for the column t1.v1 of type INT", notSupported(t, err))
			is.NotError(t, insertParameter.Close())

			query, err := c.Prepare(select1)
			is.NotError(t, err)
			_, err = query.Query([]driver.Value{int64(1)}) //nolint:staticcheck // As above.
			is.Equal(t, "s4ql: wrong number of arguments: 1 for a statement with 0 parameters", failed(t, err).Error())
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
		// The connection goes back to the pool, where its read must be free.
		is.NotError(t, conn.Close())
		readersFree(t, db)
	})
}

// counted opens an instance on a new world, with its cache opened through a VFS that counts the
// files that are open through it, and waits until it serves. A connection to the cache that has
// read it has two files open, the cache and its write-ahead log: the write connection of the
// instance, from its start on, and each reader that has read.
func counted(t *testing.T) (*world, *sql.DB, *faultvfs.VFS) {
	t.Helper()
	w := newWorld(t)
	vfs, err := faultvfs.New()
	is.NotError(t, err)
	// The VFS is closed after the instance, whose cleanup is registered later.
	t.Cleanup(func() { is.NotError(t, vfs.Close()) })
	co := coreOptions()
	co.VFS = vfs.Name()
	db := w.openOn(t, "a", w.client(), co)
	is.NotError(t, db.PingContext(t.Context()))
	return w, db, vfs
}

// rest of the values of the one column of rows, to their end, in ascending order, as [query] gives
// the values of all rows.
func rest(t *testing.T, rows *sql.Rows) []int64 {
	t.Helper()
	vs := []int64{}
	for rows.Next() {
		var v int64
		is.NotError(t, rows.Scan(&v))
		vs = append(vs, v)
	}
	is.NotError(t, rows.Err())
	slices.Sort(vs)
	return vs
}

// from the row that rows is at to their end: the value of the one column of that row and of each
// row after it, in ascending order. A test that has read one row of a query cannot tell which it
// was, the first or the last in the order of the key, so it checks that row with those after it.
func from(t *testing.T, rows *sql.Rows) []int64 {
	t.Helper()
	var v int64
	is.NotError(t, rows.Scan(&v))
	vs := append(rest(t, rows), v)
	slices.Sort(vs)
	return vs
}

func TestReaders(t *testing.T) {
	t.Run("should run a query on each of twenty connections at once, each with a reader of its own and on the state of its start, while writes commit", func(t *testing.T) {
		_, db, vfs := counted(t)
		exec(t, db, t1)
		is.Equal(t, 2, vfs.OpenFiles())

		// Each connection starts its query after one more insert than the connection before it, and
		// has run that insert itself, which opened no reader.
		const n = 20
		var conns []*sql.Conn
		var open []*sql.Rows
		var want []int64
		for k := range n {
			exec(t, db, fmt.Sprint("insert into t1 (v1) values (", k, ")"))
			want = append(want, int64(k))
			is.Equal(t, 2+2*k, vfs.OpenFiles())
			conn, err := db.Conn(t.Context())
			is.NotError(t, err)
			rows, err := conn.QueryContext(t.Context(), select1)
			is.NotError(t, err)
			conns, open = append(conns, conn), append(open, rows)
			is.Equal(t, 2+2*(k+1), vfs.OpenFiles())
		}
		is.Equal(t, n, db.Stats().InUse)

		// A write commits while the twenty reads are open, on one more connection, which opens no
		// reader for it.
		exec(t, db, fmt.Sprint("insert into t1 (v1) values (", n, ")"))
		is.Equal(t, 2+2*n, vfs.OpenFiles())

		for k, rows := range open {
			is.EqualSlice(t, want[:k+1], rest(t, rows))
			is.NotError(t, rows.Close())
		}
		// The rows have ended, and each connection reads again, on the reader that it has, what is
		// committed now.
		want = append(want, n)
		for _, conn := range conns {
			rows, err := conn.QueryContext(t.Context(), select1)
			is.NotError(t, err)
			is.EqualSlice(t, want, rest(t, rows))
			is.NotError(t, rows.Close())
		}
		is.Equal(t, 2+2*n, vfs.OpenFiles())
		for _, conn := range conns {
			is.NotError(t, conn.Close())
		}
		readersFree(t, db)
	})

	t.Run("should open the reader of a connection at its first read, keep it while the pool keeps the connection, and close it when the pool closes the connection", func(t *testing.T) {
		_, db, vfs := counted(t)
		exec(t, db, t1, insert1)
		is.Equal(t, 2, vfs.OpenFiles())

		// A connection that writes, pings, and prepares opens no reader.
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		_, err = conn.ExecContext(t.Context(), "insert into t1 (v1) values (2)")
		is.NotError(t, err)
		is.NotError(t, conn.PingContext(t.Context()))
		prepared, err := conn.PrepareContext(t.Context(), select1)
		is.NotError(t, err)
		_, err = conn.ExecContext(t.Context(), "selec")
		is.True(t, err != nil)
		is.Equal(t, 2, vfs.OpenFiles())

		// Its first read opens one, which it keeps for its later reads.
		for range 3 {
			rows, err := prepared.QueryContext(t.Context())
			is.NotError(t, err)
			is.Equal(t, 4, vfs.OpenFiles())
			is.EqualSlice(t, []int64{1, 2}, rest(t, rows))
			is.NotError(t, rows.Close())
			_, err = conn.ExecContext(t.Context(), select1)
			is.NotError(t, err)
			is.Equal(t, 4, vfs.OpenFiles())
		}
		is.NotError(t, prepared.Close())

		// The pool keeps the connection, and the connection its reader.
		is.NotError(t, conn.Close())
		is.Equal(t, 4, vfs.OpenFiles())
		is.EqualSlice(t, []int64{1, 2}, values(t, db, select1))
		is.Equal(t, 4, vfs.OpenFiles())

		// The pool closes its idle connections, and the reader closes with its connection.
		db.SetMaxIdleConns(0)
		is.Equal(t, 0, db.Stats().OpenConnections)
		is.Equal(t, 2, vfs.OpenFiles())

		// A pool that keeps no idle connection closes each connection after its statement.
		rows, err := db.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		is.Equal(t, 4, vfs.OpenFiles())
		is.EqualSlice(t, []int64{1, 2}, rest(t, rows))
		is.NotError(t, rows.Close())
		is.Equal(t, 2, vfs.OpenFiles())

		// A pool that keeps one idle connection keeps one reader of those of its connections.
		db.SetMaxIdleConns(1)
		all := held(t, db, t.Context)
		is.Equal(t, 2+2*readers, vfs.OpenFiles())
		for _, rows := range all {
			is.NotError(t, rows.Close())
		}
		is.Equal(t, 1, db.Stats().OpenConnections)
		is.Equal(t, 4, vfs.OpenFiles())
		readersFree(t, db)

		is.NotError(t, db.Close())
		is.Equal(t, 0, vfs.OpenFiles())
	})

	t.Run("should close the reader of every connection when the database is closed, under the rows that are open and the connections that are held", func(t *testing.T) {
		w, db, vfs := counted(t)
		exec(t, db, t1, insert1, "insert into t1 (v1) values (2)")

		// Rows that are open on twelve connections, and a connection that is held with its reader.
		const n = 12
		var open []*sql.Rows
		for range n {
			rows, err := db.QueryContext(t.Context(), select1)
			is.NotError(t, err)
			is.True(t, rows.Next())
			open = append(open, rows)
		}
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		_, err = conn.ExecContext(t.Context(), select1)
		is.NotError(t, err)
		is.Equal(t, 2+2*(n+1), vfs.OpenFiles())

		// The close of the database leaves no connection to the cache, before the application has
		// closed the rows and the connection.
		is.NotError(t, db.Close())
		is.Equal(t, 0, vfs.OpenFiles())
		for _, rows := range open {
			is.True(t, !rows.Next())
			is.True(t, closedError(failed(t, rows.Err())), rows.Err().Error())
			is.NotError(t, rows.Close())
		}
		_, err = conn.QueryContext(t.Context(), select1)
		is.True(t, closedError(failed(t, err)), err.Error())
		is.NotError(t, conn.Close())
		is.Equal(t, 0, vfs.OpenFiles())

		// The lease is released, and the next instance serves from the cache.
		is.EqualSlice(t, []int64{1, 2}, values(t, w.serve(t, "a"), select1))
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

	t.Run("should close a store that it owns once, after the instance, also when the close fails", func(t *testing.T) {
		for _, fails := range []bool{false, true} {
			w := newWorld(t)
			s := &closing{faulty: faulty{Store: w.client()}}
			o := w.options("a")
			if fails {
				// The close fails at the timeout, which a slower machine cannot change.
				o.CloseTimeout = 100 * time.Millisecond
			}
			db, err := s4ql.OpenOwnedStore(t.Context(), w.host, s, o, coreOptions())
			is.NotError(t, err)
			is.NotError(t, db.PingContext(t.Context()))
			exec(t, db, t1)
			is.Equal(t, int32(0), s.closes.Load())

			// A store that is down makes the release of the lease fail, and so the close.
			s.down(fails)
			err = db.Close()
			is.Equal(t, fails, err != nil, fmt.Sprint(err))
			is.Equal(t, int32(1), s.closes.Load())
			is.NotError(t, db.Close())
			is.Equal(t, int32(1), s.closes.Load())
		}
	})

	t.Run("should close a store that it owns when the instance cannot be opened", func(t *testing.T) {
		w := newWorld(t)
		s := &closing{faulty: faulty{Store: w.client()}}
		o := w.options("a")
		o.CacheDir = ""
		_, err := s4ql.OpenOwnedStore(t.Context(), w.host, s, o, coreOptions())
		is.True(t, err != nil)
		is.Equal(t, int32(1), s.closes.Load())
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
					// The rows come in the order of the key or in its reverse, as the engine draws it for the
					// query.
					var got []int64
					for n := 1; rows.Next(); n++ {
						var v int64
						err := rows.Scan(&v)
						got = append(got, v)
						if err != nil || !inKeyOrder(got) {
							errs <- fmt.Errorf("the rows of a query are not in the order of the key or in its reverse, or cannot be scanned: %v: %w", got, err)
							return
						}
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
		// The refused query left no rows open on the connection, so the next is refused alike, and not
		// for the rows of the one before.
		_, err = conn.QueryContext(ctx, select1)
		is.Equal(t, "s4ql: the instance has stopped: closed", failed(t, err).Error())
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

	t.Run("should close the rows of a connection while the database is closed, beside statements on the connection", func(t *testing.T) {
		closeBeside(t, func(t *testing.T, db *sql.DB) (*sql.Rows, func() (bool, error), func() error) {
			conn, err := db.Conn(t.Context())
			is.NotError(t, err)
			rows, err := conn.QueryContext(t.Context(), select1)
			is.NotError(t, err)
			// A write, which needs no reader and runs beside the rows, and a query, which needs the
			// reader of the connection, by turns, with a new key for each write.
			n := 3
			statement := func() (bool, error) {
				n++
				var err error
				if n%2 == 0 {
					_, err = conn.ExecContext(t.Context(), "insert into t1 (v1) values (?)", n)
				} else {
					var r *sql.Rows
					if r, err = conn.QueryContext(t.Context(), select1); err == nil {
						err = r.Close()
					}
				}
				switch {
				case err == nil:
					return true, nil
				case closedError(err):
					return false, nil
				case errors.Is(err, s4ql.ErrNotSupported) && strings.Contains(err.Error(), "whose rows of an earlier query are open"):
					return true, nil
				}
				return false, err
			}
			return rows, statement, conn.Close
		})
	})

	t.Run("should hand the connection to a statement that comes while the close of the database closes rows of the connection, and answer it as closed", func(t *testing.T) {
		w := newWorld(t)
		log := &panickyLog{}
		h := &turnHost{Free: w.host}
		db := openLogged(t, w, h, log)
		exec(t, db, t1, insert1, "insert into t1 (v1) values (2)")
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		rows, err := conn.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		is.True(t, rows.Next())

		// The close of the rows ends the loop over them, whose task logs the event of the query
		// while the close holds the connection: the log holds the close there until the statement
		// waits for the connection.
		held, release := make(chan struct{}), make(chan struct{})
		log.at(func() {
			log.at(nil)
			close(held)
			<-release
		})
		closedDB := make(chan error, 1)
		go func() { closedDB <- db.Close() }()
		<-held

		waits := make(chan struct{})
		h.at(func() {
			h.at(nil)
			close(waits)
		})
		answered := make(chan error, 1)
		go func() {
			var v int64
			answered <- conn.QueryRowContext(t.Context(), select1).Scan(&v)
		}()
		<-waits
		close(release)

		// The close of the rows hands the connection to the statement, which then runs on an
		// instance that is closing.
		err = <-answered
		is.Error(t, s4ql.ErrStopped, failed(t, err))
		is.Equal(t, "s4ql: the instance has stopped: closed", err.Error())
		is.NotError(t, <-closedDB)
		is.True(t, !rows.Next())
		is.Error(t, s4ql.ErrStopped, failed(t, rows.Err()))
		is.NotError(t, rows.Close())
		is.NotError(t, conn.Close())
	})

	t.Run("should free the connection when the close of rows that the close of the database left to a statement panics, and raise the panic", func(t *testing.T) {
		w := newWorld(t)
		log := &panickyLog{}
		db := openLogged(t, w, w.host, log)
		exec(t, db, t1, insert1)
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		// No loop reads the rows, so their close logs the event of the query on the goroutine that
		// closes them.
		rows, err := conn.QueryContext(t.Context(), select1)
		is.NotError(t, err)

		// The log holds the insert at its event, while it holds the connection, so that the close
		// of the database leaves the close of the rows to it. The close of the rows panics.
		held, release := make(chan struct{}), make(chan struct{})
		log.at(func() {
			log.at(nil)
			log.arm(true)
			close(held)
			<-release
		})
		raised := make(chan any, 1)
		go func() {
			defer func() { raised <- recover() }()
			_, _ = conn.ExecContext(t.Context(), "insert into t1 (v1) values (2)")
		}()
		<-held
		is.NotError(t, db.Close())
		close(release)
		is.Equal(t, any(errPanic), <-raised)
		log.arm(false)

		// Neither the rows nor the connection are held.
		is.True(t, !rows.Next())
		is.Error(t, s4ql.ErrStopped, failed(t, rows.Err()))
		is.NotError(t, rows.Close())
		is.NotError(t, conn.Close())
		is.EqualSlice(t, []int64{1, 2}, values(t, w.serve(t, "b"), select1))
	})

	t.Run("should close the instance when the close of rows by the close of the database panics, log the panic, and give it as an internal error", func(t *testing.T) {
		w := newWorld(t)
		log := &panickyLog{}
		db := openLogged(t, w, w.host, log)
		exec(t, db, t1, insert1)
		// No loop reads the rows, so their close by the close of the database logs the event of the
		// query on its goroutine. The close of the first of them panics there, and the other closes
		// as usual.
		rows, err := db.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		other, err := db.QueryContext(t.Context(), select1)
		is.NotError(t, err)

		log.at(func() {
			log.at(nil)
			panic(errPanic)
		})
		v, err := closeDB(db)
		is.True(t, v == nil, fmt.Sprint(v))
		is.Equal(t, "s4ql: internal error: the query panicked: a panic of the test", failed(t, err).Error())
		panics := log.lines.of("panic")
		is.Equal(t, 1, len(panics))
		is.True(t, strings.HasPrefix(panics[0], `level=ERROR msg=panic db.prefix=db panic="a panic of the test" stack="goroutine `), panics[0])
		is.EqualSlice(t, []string{"level=INFO msg=close db.prefix=db"}, log.lines.of("close"))

		for _, rows := range []*sql.Rows{rows, other} {
			is.True(t, !rows.Next())
			is.Error(t, s4ql.ErrStopped, failed(t, rows.Err()))
			is.NotError(t, rows.Close())
		}
		// The lease is released, and the cache is closed, which the world checks when the test ends.
		is.EqualSlice(t, []int64{1}, values(t, w.serve(t, "b"), select1))
	})
}

// closeDB is [sql.DB.Close] of db, with the value of a panic in it, if it panicked.
func closeDB(db *sql.DB) (v any, err error) {
	defer func() { v = recover() }()
	return nil, db.Close()
}

// openLogged opens an instance of the world w on the host h with the cache directory "a", which
// logs to log, and waits until it serves.
func openLogged(t *testing.T, w *world, h host.Host, log *panickyLog) *sql.DB {
	t.Helper()
	o := w.options("a")
	o.Logger = slog.New(log.handler())
	db, err := s4ql.OpenStore(t.Context(), h, w.client(), o, coreOptions())
	is.NotError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	is.NotError(t, db.PingContext(t.Context()))
	return db
}

// turnHost is the host of the world, which calls a function of the test when a connection of the
// driver waits for its turn, while one is set.
type turnHost struct {
	*sim.Free
	waits atomic.Pointer[func()]
}

// at a wait for the turn of a connection, call f from now on, or nothing if f is nil. f can set the
// next.
func (h *turnHost) at(f func()) {
	if f == nil {
		h.waits.Store(nil)
		return
	}
	h.waits.Store(&f)
}

func (h *turnHost) NewSemaphore(name string, n int) host.Semaphore {
	s := h.Free.NewSemaphore(name, n)
	if name != "turn of the connection" {
		return s
	}
	return &turnSemaphore{Semaphore: s, h: h}
}

// turnSemaphore is the semaphore of the turn of a connection on a [turnHost].
type turnSemaphore struct {
	host.Semaphore
	h *turnHost
}

func (s *turnSemaphore) Acquire(ctx context.Context) error {
	if f := s.h.waits.Load(); f != nil {
		(*f)()
	}
	return s.Semaphore.Acquire(ctx)
}

// closeBeside closes a database many times over, each time a new one that holds t1 with three rows,
// while the rows of a query on one of its connections are open, and while another goroutine runs
// statements on that connection. The close of the database closes those rows from its own goroutine,
// as it closes all rows that are open, so under -race the test shows whether that close and the
// statements touch the state of the connection without an order between them. It does not depend on
// how the two goroutines interleave: the race detector sees accesses without an order between them
// whenever they come.
//
// open gives the rows, of a query on the connection, the statement, which runs on the connection
// and gives whether to run it again, and its error, if it is one that the test does not expect, and
// end, which ends what open began. The goroutine runs the statement once before the close begins,
// and then until it is not to run again, and then closes the rows and calls end. Every second time,
// the rows are read up to their first row first, so that the close ends a loop of the engine over
// them; the other times no loop reads them.
func closeBeside(t *testing.T, open func(t *testing.T, db *sql.DB) (rows *sql.Rows, statement func() (bool, error), end func() error)) {
	t.Helper()
	for i := range 20 {
		db := newWorld(t).serve(t, "a")
		exec(t, db, t1, insert1, "insert into t1 (v1) values (2)", "insert into t1 (v1) values (3)")
		rows, statement, end := open(t, db)
		if i%2 == 0 {
			is.True(t, rows.Next())
		}
		started, done := make(chan struct{}), make(chan error, 1)
		go func() {
			var err error
			for again, n := true, 0; again && err == nil; n++ {
				again, err = statement()
				if n == 0 {
					close(started)
				}
			}
			done <- errors.Join(err, rows.Close(), end())
		}()
		<-started
		is.NotError(t, db.Close())
		is.NotError(t, <-done)
	}
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

		// What is open when the instance stops: the rows of a query, the rows of further queries on
		// connections of their own, a connection, and a prepared statement.
		rows, err := db.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		is.True(t, rows.Next())
		more := held(t, db, t.Context)
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
			is.True(t, !errors.Is(err, s4ql.ErrNotServing) && !errors.Is(err, s4ql.ErrOutcomeUnknown) && !errors.Is(err, s4ql.ErrNotSupported) && !errors.Is(err, s4ql.ErrTooLarge), err.Error())
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
			_, err = db.BeginTx(t.Context(), nil)
			stopped(err)
			_, err = conn.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			stopped(err)
		}
		// The rows that were open fail at their next row, on every connection.
		for _, rows := range append(more, rows) {
			is.True(t, !rows.Next())
			stopped(rows.Err())
			_ = rows.Close()
		}

		// What the text alone decides does not depend on the instance.
		_, err = db.ExecContext(t.Context(), "selec")
		is.Equal(t, `near "selec": syntax error`, failed(t, err).Error())
		_, err = db.ExecContext(t.Context(), "")
		notSupported(t, err)
		again, err := db.PrepareContext(t.Context(), select1)
		is.NotError(t, err)
		_, err = again.QueryContext(t.Context())
		stopped(err)

		// What the text and the arguments decide does not either, and a statement with arguments that
		// reaches the instance gets the stop.
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (? ?)", 1)
		is.Equal(t, `near "?": syntax error`, failed(t, err).Error())
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (?)", 1, 2)
		is.Equal(t, "s4ql: wrong number of arguments: 2 for a statement with 1 parameter", failed(t, err).Error())
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (?)", math.NaN())
		is.Equal(t, "argument 1, which is a real that is not a number", notSupported(t, err))
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (?)", 6)
		stopped(err)
		// The type of the column needs the instance.
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (?)", 1.5)
		stopped(err)

		// Recovery is a normal start: the application closes the database and opens it again, and
		// the insert is there if and only if it was acknowledged.
		is.NotError(t, conn.Close())
		_ = db.Close()
		// The close has closed the cache, with the reader of every connection.
		is.Equal(t, 0, vfs.OpenFiles())
		vfs.Fail(faultvfs.Fault{})
		want := []int64{1, 2}
		if insertErr == nil {
			want = append(want, 3)
		}
		is.EqualSlice(t, want, values(t, w.serve(t, "a"), select1))
	})

	t.Run("should stop the instance when the cache cannot be opened for the reader of a connection, as after any local failure of the cache", func(t *testing.T) {
		w, db, vfs := counted(t)
		exec(t, db, t1, insert1, "insert into t1 (v1) values (2)")
		rows, err := db.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		is.True(t, rows.Next())

		// The next open of the file of the cache fails, as it does for a process that has no file
		// descriptor left: the first read of a new connection.
		vfs.Fail(faultvfs.Fault{Ops: faultvfs.Open, Files: faultvfs.MainDB, Times: 1})
		stopped := func(err error) {
			t.Helper()
			is.Error(t, s4ql.ErrStopped, failed(t, err))
			is.True(t, strings.HasPrefix(err.Error(), "s4ql: the instance has stopped: local I/O failure: open a reader: "), err.Error())
		}
		_, err = db.QueryContext(t.Context(), select1)
		stopped(err)
		is.Equal(t, 1, vfs.Failures())

		// The stop is for good, for every connection, though the cache opens again.
		_, err = db.QueryContext(t.Context(), select1)
		stopped(err)
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (3)")
		stopped(err)
		stopped(db.PingContext(t.Context()))
		// The rows that were open fail at their next row.
		is.True(t, !rows.Next())
		stopped(rows.Err())
		is.NotError(t, rows.Close())
		is.Equal(t, 1, vfs.Failures())

		// Recovery is a normal start.
		is.NotError(t, db.Close())
		is.Equal(t, 0, vfs.OpenFiles())
		is.EqualSlice(t, []int64{1, 2}, values(t, w.serve(t, "a"), select1))
	})

	t.Run("should stop the instance when the files of the cache are deleted under it, at the first read of a new connection, and make no empty cache", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1, insert1, "insert into t1 (v1) values (2)")
		// A connection that is held has its reader, so the next query takes a new connection.
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		rows, err := conn.QueryContext(t.Context(), select1)
		is.NotError(t, err)
		is.NotError(t, rows.Close())

		dir := filepath.Join(w.dir, "a")
		for _, suffix := range []string{"", "-wal", "-shm"} {
			is.NotError(t, os.Remove(filepath.Join(dir, "cache.db"+suffix)))
		}

		_, err = db.QueryContext(t.Context(), select1)
		is.Error(t, s4ql.ErrStopped, failed(t, err))
		is.Equal(t, "s4ql: the instance has stopped: local I/O failure: open a reader: sqlite: unable to open database file (code 14)", err.Error())
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (3)")
		is.Error(t, s4ql.ErrStopped, failed(t, err))
		_, err = conn.QueryContext(t.Context(), select1)
		is.Error(t, s4ql.ErrStopped, failed(t, err))
		is.Error(t, s4ql.ErrStopped, failed(t, db.PingContext(t.Context())))

		is.NotError(t, conn.Close())
		is.NotError(t, db.Close())
		entries, err := os.ReadDir(dir)
		is.NotError(t, err)
		for _, e := range entries {
			t.Errorf("the file %v is left", e.Name())
		}

		// Recovery is a normal start, which makes a cache and replays the log into it.
		is.EqualSlice(t, []int64{1, 2}, values(t, w.serve(t, "a"), select1))
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

// inKeyOrder reports whether the values vs of a key of integers are in the order of the key or in
// its reverse, with none twice: the two orders in which a query without ORDER BY gives the rows that
// it reads by the primary key, forward or backward, as the engine draws it.
func inKeyOrder(vs []int64) bool {
	up, down := true, true
	for i := 1; i < len(vs); i++ {
		up, down = up && vs[i-1] < vs[i], down && vs[i-1] > vs[i]
	}
	return up || down
}

// some values of t1 on db, in the order in which the rows came: the first limit of them, with the
// rows closed after those, or all of them if limit is negative.
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
	t.Run("should run writes and queries from many goroutines, each write once and each query with its rows in the order of the key or in its reverse", func(t *testing.T) {
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
			// As many queries at once as writes, each on a connection of the pool.
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
					if !inKeyOrder(got) {
						errs <- fmt.Errorf("the rows of a query are not in the order of the key or in its reverse: %v", got)
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

// statementEvents records the outcome and the text of each event of a statement of the engine.
type statementEvents struct {
	mu     sync.Mutex
	events []string
}

func (h *statementEvents) Enabled(context.Context, slog.Level) bool { return true }
func (h *statementEvents) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *statementEvents) WithGroup(string) slog.Handler            { return h }

func (h *statementEvents) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "statement" {
		return nil
	}
	var sql, outcome string
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "statement.sql":
			sql = a.Value.String()
		case "outcome":
			outcome = a.Value.String()
		}
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, outcome+": "+sql)
	return nil
}

func TestExec_several(t *testing.T) {
	t.Run("should run the statements of a text one after another, each committed by itself, and keep them after a restart with an empty cache", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		// goqite's schema, as far as s4ql runs it: a table, and an index on it.
		schema := "create table goqite (\n  id text primary key,\n  queue text not null,\n  body blob not null\n) strict, without rowid;\n\n" +
			"-- The index of the receive.\ncreate index goqite_queue_idx on goqite (queue);\n"
		res, err := db.ExecContext(t.Context(), schema)
		is.Equal(t, int64(0), affected(t, res, err))
		res, err = db.ExecContext(t.Context(), "insert into goqite (id, queue, body) values (?, ?, ?);\ninsert into goqite (id, queue, body) values (?, 'q', x'02')",
			"m1", "q", []byte{1}, "m2")
		is.Equal(t, int64(1), affected(t, res, err))
		is.EqualSlice(t, []string{
			"create table goqite (\n  id text primary key,\n  queue text not null,\n  body blob not null\n) strict, without rowid;",
			"create index goqite_queue_idx on goqite (queue);",
			`insert into goqite (id, queue, body) values (?, ?, ?);`,
			`insert into goqite (id, queue, body) values (?, 'q', x'02')`,
		}, annotations(t, w))
		is.Equal(t, 4, len(commits(t, w)))
		want := rowsText(t, db, "select id, queue, body from goqite where queue = 'q' order by id")
		is.NotError(t, db.Close())

		db = w.serve(t, "b")
		is.Equal(t, want, rowsText(t, db, "select id, queue, body from goqite where queue = 'q' order by id"))
	})

	t.Run("should keep the statements before one that fails, committed, run none after it, and give the error of the one that failed", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		_, err := db.ExecContext(t.Context(), t1+"; "+insert1+"; "+insert1+"; insert into t1 (v1) values (2)")
		e, ok := errors.AsType[*s4ql.Error](failed(t, err))
		is.True(t, ok)
		is.Equal(t, "UNIQUE constraint failed: t1.v1", e.Message)
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (3); selec; insert into t1 (v1) values (4)")
		e, ok = errors.AsType[*s4ql.Error](failed(t, err))
		is.True(t, ok)
		is.Equal(t, `near "selec": syntax error`, e.Message)
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (5); begin; insert into t1 (v1) values (6)")
		is.Equal(t, "BEGIN (at offset 32)", notSupported(t, err))
		is.EqualSlice(t, []int64{1, 3, 5}, values(t, db, select1))
		is.NotError(t, db.Close())

		db = w.serve(t, "b")
		is.EqualSlice(t, []int64{1, 3, 5}, values(t, db, select1))
	})

	t.Run("should run the statements in a transaction as statements of it, keep those before one that fails in it, and undo them all with a roll back", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		tx, err := db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), insert1+"; "+insert1+"; insert into t1 (v1) values (2)")
		is.Equal(t, "UNIQUE constraint failed: t1.v1", failed(t, err).Error())
		is.EqualSlice(t, []int64{1}, values(t, tx, select1))
		is.NotError(t, tx.Rollback())
		is.EqualSlice(t, []int64{}, values(t, db, select1))

		tx, err = db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), insert1+"; insert into t1 (v1) values (2)")
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), "insert into t1 (v1) values (3); "+insert1)
		is.Equal(t, "UNIQUE constraint failed: t1.v1", failed(t, err).Error())
		is.NotError(t, tx.Commit())
		is.EqualSlice(t, []int64{1, 2, 3}, values(t, db, select1))
		is.EqualSlice(t, []string{t1, insert1 + ";", "insert into t1 (v1) values (2)", "insert into t1 (v1) values (3);"}, annotations(t, w))
	})

	t.Run("should give each statement the arguments of its parameters in order, refuse more arguments before any statement runs, and too few at the statement that lacks them", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		_, err := db.ExecContext(t.Context(), "insert into t1 (v1) values (?); update t1 set v1 = ? where v1 = ?; insert into t1 (v1) values (?)", 1, 2, 1, 3)
		is.NotError(t, err)
		is.EqualSlice(t, []int64{2, 3}, values(t, db, select1))

		two := "insert into t1 (v1) values (?); insert into t1 (v1) values (?)"
		_, err = db.ExecContext(t.Context(), two, 4, 5, 6)
		is.Equal(t, "s4ql: wrong number of arguments: 3 for a text whose 2 statements have 2 parameters", failed(t, err).Error())
		_, err = db.ExecContext(t.Context(), two, 4)
		is.Equal(t, "s4ql: wrong number of arguments: 1 for a text whose statements up to the one at offset 32 have 2 parameters", failed(t, err).Error())
		is.EqualSlice(t, []int64{2, 3, 4}, values(t, db, select1))

		// An argument that s4ql does not support runs nothing, after the errors that come before any
		// statement runs.
		_, err = db.ExecContext(t.Context(), two, 5, true)
		is.Equal(t, "argument 2 of type bool", notSupported(t, err))
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (?); selec", 5, true)
		is.Equal(t, "argument 2 of type bool", notSupported(t, err))
		_, err = db.ExecContext(t.Context(), two, true)
		is.Equal(t, "argument 1 of type bool", notSupported(t, err))
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (?); selec", true)
		is.Equal(t, "space after a statement that SQLite reads as part of a token (at offset 31)", notSupported(t, err))
		_, err = db.ExecContext(t.Context(), two, 5, true, 6)
		is.Equal(t, "s4ql: wrong number of arguments: 3 for a text whose 2 statements have 2 parameters", failed(t, err).Error())
		is.EqualSlice(t, []int64{2, 3, 4}, values(t, db, select1))

		// More arguments than parameters run the statements before one that s4ql refuses, which may
		// take them.
		_, err = db.ExecContext(t.Context(), "insert into t1 (v1) values (?); selec", 5, 6)
		is.Equal(t, `near "selec": syntax error`, failed(t, err).Error())
		is.EqualSlice(t, []int64{2, 3, 4, 5}, values(t, db, select1))
	})

	t.Run("should give SQLite's count of changes after the last statement that sets it, also after a statement that sets none", func(t *testing.T) {
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
		is.Equal(t, int64(0), run(t1+"; "+select1))
		is.Equal(t, int64(1), run(insert1+"; insert into t1 (v1) values (2); delete from t1 where v1 = 2"))
		is.Equal(t, int64(1), run("insert into t1 (v1) values (2); create table t2 (v1 int primary key) strict, without rowid; "+select1))
		// mattn/go-sqlite3 gives 0 for these two, for the empty statement at the end.
		is.Equal(t, int64(1), run("insert into t1 (v1) values (3); -- c"))
		is.Equal(t, int64(1), run("insert into t1 (v1) values (4);;"))
		is.Equal(t, int64(4), run("update t1 set v1 = v1 + 10 returning v1; "+select1))
		// mattn gives the count of the statement before a last statement with RETURNING, 1.
		is.Equal(t, int64(5), run("insert into t1 (v1) values (5); delete from t1 returning v1"))
		is.Equal(t, int64(5), run("drop table t2"))
	})

	t.Run("should refuse several statements in Query, QueryRow, and Prepare, and run a prepared statement that a comment follows", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		src := insert1 + "; insert into t1 (v1) values (2)"
		_, err := db.QueryContext(t.Context(), src)
		is.Equal(t, "second statement (at offset 32)", notSupported(t, err))
		var v int64
		err = db.QueryRowContext(t.Context(), select1+"; "+select1).Scan(&v)
		is.Equal(t, "second statement (at offset 18)", notSupported(t, err))
		_, err = db.PrepareContext(t.Context(), src)
		is.Equal(t, "second statement (at offset 32)", notSupported(t, err))
		is.EqualSlice(t, []int64{}, values(t, db, select1))

		// A prepared statement runs its one statement, as mattn's does, whatever comment follows it.
		prepared, err := db.PrepareContext(t.Context(), insert1+"; /* ")
		is.NotError(t, err)
		_, err = prepared.ExecContext(t.Context())
		is.NotError(t, err)
		is.NotError(t, prepared.Close())
		is.EqualSlice(t, []int64{1}, values(t, db, select1))
	})

	t.Run("should refuse a text whose spaces mattn/go-sqlite3 and SQLite read differently, before any statement runs", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1)
		for src, want := range map[string]string{
			insert1 + ";\u00a0insert into t1 (v1) values (2)":  "space after a statement that SQLite reads as part of a token (at offset 31)",
			insert1 + ";\vinsert into t1 (v1) values (2)":      "space after a statement that SQLite reads as part of a token (at offset 31)",
			insert1 + "; insert into t1 (v1) values (2)\u00a0": "space at the end of the text that SQLite reads as part of a token (at offset 62)",
			insert1 + "; /* ": "space at the end of the text that SQLite reads as part of a token (at offset 34)",
		} {
			_, err := db.ExecContext(t.Context(), src)
			is.Equal(t, want, notSupported(t, err), src)
		}
		is.EqualSlice(t, []int64{}, values(t, db, select1))
	})

	t.Run("should log an event for each statement, with the text of its part, as if it ran alone", func(t *testing.T) {
		w := newWorld(t)
		h := &statementEvents{}
		o := w.options("a")
		o.Logger = slog.New(h)
		db, err := s4ql.OpenStore(t.Context(), w.host, w.client(), o, coreOptions())
		is.NotError(t, err)
		defer func() { is.NotError(t, db.Close()) }()
		is.NotError(t, db.PingContext(t.Context()))
		_, err = db.ExecContext(t.Context(), t1+";\n"+insert1+"; "+insert1+"; insert into t1 (v1) values (2)")
		is.Equal(t, "UNIQUE constraint failed: t1.v1", failed(t, err).Error())
		h.mu.Lock()
		defer h.mu.Unlock()
		is.EqualSlice(t, []string{"ok: " + t1 + ";", "ok: \n" + insert1 + ";", "sqlite_error:  " + insert1 + ";"}, h.events)
	})
}
