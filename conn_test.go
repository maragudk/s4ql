package s4ql_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
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

		_, err := db.ExecContext(t.Context(), "select * from t1 order by v1")
		is.Equal(t, "ORDER BY clause (at offset 17)", notSupported(t, err))
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
				for _, src := range []string{"select * from t1 where v1 = ? or v1 = 2", "select * from t1 order by v1", "insert into t1 (v1) values (?1)", "insert into t1 (v1) values (?), (?)", "begin", "", " ; -- nothing"} {
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
		_, err = db.PrepareContext(t.Context(), "select * from t1 where v1 = 1 order by v1")
		is.Equal(t, "ORDER BY clause (at offset 30)", notSupported(t, err))
		// A select with WHERE that s4ql supports prepares without the instance.
		p, err := db.PrepareContext(t.Context(), "select * from t1 where v1 = ? and v1 is not null")
		is.NotError(t, err)
		is.NotError(t, p.Close())
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
		// The pass supports an index, and the engine refuses it until it runs one, from the text alone.
		_, err = db.PrepareContext(t.Context(), "create index i on t1 (v1)")
		is.Equal(t, "CREATE INDEX (at offset 0)", notSupported(t, err))
		_, err = db.PrepareContext(t.Context(), "drop index i")
		is.Equal(t, "DROP INDEX (at offset 5)", notSupported(t, err))
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
// log: the text of each statement, and " with " and the value of each of its parameters.
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
				note := a.GetStatement().GetSql()
				for _, p := range a.GetStatement().GetParameters() {
					switch {
					case p.HasInteger():
						note += fmt.Sprint(" with ", p.GetInteger())
					case p.HasReal():
						note += " with real " + strconv.FormatFloat(p.GetReal(), 'g', -1, 64)
					case p.HasText():
						note += " with " + strconv.Quote(p.GetText())
					case p.HasBlob():
						note += fmt.Sprintf(" with x'%x'", p.GetBlob())
					case p.HasNull():
						note += " with null"
					default:
						t.Fatalf("%q: a parameter of kind %v", note, p.WhichKind())
					}
				}
				commit = append(commit, note)
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

	t.Run("should keep the arguments in the annotation of the commit, and give the same rows from an empty cache after a restart", func(t *testing.T) {
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
			"insert into t1 (v1) values (?) with -9223372036854775808",
			"insert into t1 (v1) values ( ? ) with 7",
			"insert into t1 (v1) values ( ? ) with 9223372036854775807",
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

	t.Run("should store -0.0 as 0.0, give it back as 0.0, take it for the key 0.0, and annotate the argument as given", func(t *testing.T) {
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
		is.EqualSlice(t, []string{typedTables[2], "insert into reals (v) values (?) with real -0"}, annotations(t, w))
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
			`insert into t3 (v1, v2, v3) values (?, ?, ?) with 4 with "d" with real 4.5`,
			`insert into t3 (v2, v3, v1) values (?, ?, ?) with "c" with real 3.5 with 3`,
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

	t.Run("should take every kind of nil that database/sql hands the driver as NULL, in a column of any type, directly and prepared, and annotate it as NULL", func(t *testing.T) {
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
		is.Equal(t, `insert into n (k, t, i, r, b, s) values (?, ?, ?, ?, ?, ?) with 0 with null with null with null with null with "x"`, annotations(t, w)[1])
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
		_, err = db.PrepareContext(t.Context(), "update u set s = 'x' returning *")
		is.Equal(t, "RETURNING clause (at offset 21)", notSupported(t, err))
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
			"create unique index i on x (s)":           "UNIQUE INDEX",
			"create index i on x (s) where s = 'a'":    "partial index",
			"select * from x indexed by xs":            "INDEXED BY",
			"select * from x not indexed":              "NOT INDEXED",
			"explain query plan select * from x":       "EXPLAIN QUERY PLAN",
			"create index if not exists i on x (s)":    "IF NOT EXISTS",
			"drop index if exists xs":                  "IF EXISTS",
			"reindex xs":                               "REINDEX",
			"analyze":                                  "ANALYZE",
			"create index i on x (s collate nocase)":   "COLLATE",
			"create index i on x (lower(s))":           "index on an expression",
			"select * from x where s = 'a' order by k": "ORDER BY",
		} {
			_, err := db.ExecContext(t.Context(), src)
			is.True(t, strings.HasPrefix(notSupported(t, err), want), fmt.Sprint(src, ": ", err))
		}
		is.Equal(t, "k INT: 1; 3", rowsText(t, db, "select k from x where s = 'a'"))
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
