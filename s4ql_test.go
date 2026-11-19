package s4ql_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql"
	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/sqlite"
	"maragu.dev/s4ql/internal/sqlite/faultvfs"
	"maragu.dev/s4ql/internal/store"
	"maragu.dev/s4ql/internal/store/simstore"
)

const (
	t1      = "create table t1 (v1 int primary key) strict, without rowid"
	insert1 = "insert into t1 (v1) values (1)"
	select1 = "select * from t1"
)

// readers is the number of queries that a test holds open at once, each on a connection of its
// own, and the bound of the pool of a test that fills its pool.
const readers = 4

// world is a store in memory and a host with real goroutines, on which the tests open instances
// through database/sql.
type world struct {
	host  *sim.Free
	store *simstore.Store
	dir   string
}

// newWorld, with a directory for the caches. When the test ends, after the cleanups that close the
// instances, it fails the test for each cache whose write-ahead log is left, which SQLite deletes
// when the last connection to a cache closes: an instance that is not closed, or the rows of a query
// that still hold a read. A test that has failed already is not checked.
func newWorld(t *testing.T) *world {
	t.Helper()
	h := sim.NewFree(sim.Buggify{})
	w := &world{host: h, store: simstore.New(h, simstore.Options{}), dir: t.TempDir()}
	t.Cleanup(func() {
		if t.Failed() {
			return
		}
		is.NotError(t, filepath.WalkDir(w.dir, func(path string, _ fs.DirEntry, err error) error {
			if strings.HasSuffix(path, "-wal") {
				t.Errorf("a connection to the cache %v is still open", path)
			}
			return err
		}))
	})
	return w
}

// options of the storage core for the tests. A slow machine must not make the cache busy or the
// lease run out, so the waits for both are far longer than any test takes, and an instance that
// has the lease keeps it until it closes. A spare looks often for a lease that was released, so that
// no test waits long for it.
func coreOptions() core.Options {
	return core.Options{BusyTimeout: time.Minute, Lease: time.Hour, Watch: 10 * time.Millisecond}
}

// client of the world's store, with a name of its own.
func (w *world) client() store.Store {
	return w.store.Client(fmt.Sprint("client-", w.host.Uint64()), nil)
}

// open an instance of the database "db" with the cache directory of the given name, which is closed
// when the test ends.
func (w *world) open(t *testing.T, cache string) *sql.DB {
	t.Helper()
	return w.openOn(t, cache, w.client(), coreOptions())
}

// openOn is [world.open] of an instance that reaches the store through s, with the options co of
// the storage core.
func (w *world) openOn(t *testing.T, cache string, s store.Store, co core.Options) *sql.DB {
	t.Helper()
	db, err := s4ql.OpenStore(t.Context(), w.host, s, w.options(cache), co)
	is.NotError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// options of an instance of the database "db" with the cache directory of the given name.
func (w *world) options(cache string) s4ql.Options {
	return s4ql.Options{Prefix: "db", CacheDir: filepath.Join(w.dir, cache), CloseTimeout: time.Minute}
}

// serve opens an instance and waits until it serves.
func (w *world) serve(t *testing.T, cache string) *sql.DB {
	t.Helper()
	db := w.open(t, cache)
	is.NotError(t, db.PingContext(t.Context()))
	return db
}

// failed requires that err is an error, and one that database/sql does not take for a broken
// connection, and gives it. No error of the driver may be or wrap [driver.ErrBadConn], whatever
// its kind, because database/sql runs a statement again after it. So every test passes each error
// that it expects through this function.
func failed(t *testing.T, err error) error {
	t.Helper()
	is.True(t, err != nil, "no error")
	is.True(t, !errors.Is(err, driver.ErrBadConn), fmt.Sprint("the error is driver.ErrBadConn: ", err))
	return err
}

// exec the statements on db, each of which must succeed.
func exec(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, s := range statements {
		_, err := db.ExecContext(t.Context(), s)
		is.NotError(t, err, s)
	}
}

// values of the one column of the rows of the query src on db, which must succeed.
func values(t *testing.T, db *sql.DB, src string) []int64 {
	t.Helper()
	vs, err := query(t.Context(), db, src)
	is.NotError(t, err)
	return vs
}

// query db for the values of the one column of the rows of src. It fails no test, so that a
// goroutine that is not the test's can call it.
func query(ctx context.Context, db *sql.DB, src string) ([]int64, error) {
	rows, err := db.QueryContext(ctx, src)
	if err != nil {
		return nil, err
	}
	vs := []int64{}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		vs = append(vs, v)
	}
	return vs, errors.Join(rows.Err(), rows.Close())
}

// readersFree requires that no connection of the pool of db is held, and that none is left with a
// read: it takes every connection that the pool has, all at once, and runs a query on each. For
// that it bounds the pool to the connections that it has, so that taking them waits for those that
// database/sql is still closing the rows of, and would wait for good for one that is held. A
// connection whose read was left open fails the query.
func readersFree(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	stats := db.Stats()
	// Two at the least, so that the bound does not lower the number of idle connections that the
	// pool keeps.
	n := max(2, stats.OpenConnections)
	db.SetMaxOpenConns(n)
	defer db.SetMaxOpenConns(stats.MaxOpenConnections)
	for range n {
		conn, err := db.Conn(ctx)
		is.NotError(t, err, "a connection is held")
		defer func() { is.NotError(t, conn.Close()) }()
		rows, err := conn.QueryContext(ctx, select1)
		is.NotError(t, err, "a read is held")
		is.True(t, rows.Next() || rows.Err() == nil)
		is.NotError(t, rows.Close())
	}
}

func TestOpen(t *testing.T) {
	t.Run("should open without waiting for the lease, refuse statements until the instance serves, and serve after a ping", func(t *testing.T) {
		w := newWorld(t)
		// The store cannot be reached, so the instance cannot take the lease.
		s := &faulty{Store: w.client()}
		s.down(true)
		db := w.openOn(t, "a", s, coreOptions())

		_, err := db.ExecContext(t.Context(), t1)
		is.Error(t, s4ql.ErrNotServing, failed(t, err))
		is.True(t, strings.HasPrefix(err.Error(), "s4ql: "), err.Error())
		_, err = db.QueryContext(t.Context(), select1)
		is.Error(t, s4ql.ErrNotServing, failed(t, err))

		// A ping waits for the instance to serve, and ends with its context. database/sql answers a
		// context that has ended by itself, also one that ends before database/sql has called the
		// driver, so the driver is asked directly, with a context that has not ended.
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, conn.Close()) }()
		is.NotError(t, conn.Raw(func(driverConn any) error {
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			is.Error(t, context.DeadlineExceeded, failed(t, driverConn.(driver.Pinger).Ping(ctx)))
			return nil
		}))

		s.down(false)
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, t1, insert1)
		is.EqualSlice(t, []int64{1}, values(t, db, select1))
	})

	t.Run("should create the cache directory, and keep the cache for the next instance", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "deep/down/a")
		exec(t, db, t1, insert1)
		is.NotError(t, db.Close())
		_, err := os.Stat(filepath.Join(w.dir, "deep", "down", "a", "cache.db"))
		is.NotError(t, err)

		// The next instance on the cache directory finds the cache.
		is.EqualSlice(t, []int64{1}, values(t, w.serve(t, "deep/down/a"), select1))
	})

	t.Run("should give the same answers from an empty cache, after a restart", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1, insert1, "insert into t1 (v1) values (-2)", "create table t2 (k int primary key) strict, without rowid", "drop table t2")
		is.NotError(t, db.Close())

		db = w.serve(t, "b")
		is.EqualSlice(t, []int64{-2, 1}, values(t, db, select1))
		_, err := db.QueryContext(t.Context(), "select * from t2")
		is.Equal(t, "no such table: t2", failed(t, err).Error())
	})

	t.Run("should fail on local problems only", func(t *testing.T) {
		w := newWorld(t)
		file := filepath.Join(w.dir, "file")
		is.NotError(t, os.WriteFile(file, nil, 0o600))
		cases := []struct {
			name   string
			change func(o *s4ql.Options)
			want   string
		}{
			{name: "no cache directory", change: func(o *s4ql.Options) { o.CacheDir = "" }, want: "s4ql: open: the options name no cache directory"},
			{name: "a negative close timeout", change: func(o *s4ql.Options) { o.CloseTimeout = -time.Second }, want: "s4ql: open: the close timeout -1s is negative"},
			{name: "a cache directory that is a file", change: func(o *s4ql.Options) { o.CacheDir = file }, want: "s4ql: open: the cache directory: "},
			{name: "a prefix that cannot be part of a key", change: func(o *s4ql.Options) { o.Prefix = "a/../b" }, want: "s4ql: open: "},
		}
		for _, c := range cases {
			o := w.options("a")
			c.change(&o)
			_, err := s4ql.OpenStore(t.Context(), w.host, w.client(), o, coreOptions())
			is.True(t, strings.HasPrefix(failed(t, err).Error(), c.want), fmt.Sprint(c.name, ": ", err))
		}

		// A cache that is no SQLite database cannot be opened.
		o := w.options("broken")
		is.NotError(t, os.MkdirAll(o.CacheDir, 0o700))
		is.NotError(t, os.WriteFile(filepath.Join(o.CacheDir, "cache.db"), []byte(strings.Repeat("no database ", 100)), 0o600))
		_, err := s4ql.OpenStore(t.Context(), w.host, w.client(), o, coreOptions())
		is.True(t, strings.HasPrefix(failed(t, err).Error(), "s4ql: open the cache: "), err.Error())
	})

	t.Run("should come up as a spare while another instance holds the lease, and serve with its commits after it has closed", func(t *testing.T) {
		w := newWorld(t)
		first := w.serve(t, "a")
		exec(t, first, t1, insert1)

		second := w.open(t, "b")
		// Nothing tells the test when the spare has looked at the lease, so it asks only what holds
		// at any time while the first holds the lease: the spare does not serve.
		for i := range 3 {
			_, err := second.ExecContext(t.Context(), "insert into t1 (v1) values (2)")
			is.Error(t, s4ql.ErrNotServing, failed(t, err))
			_, err = second.QueryContext(t.Context(), select1)
			is.Error(t, s4ql.ErrNotServing, failed(t, err))
			exec(t, first, fmt.Sprint("insert into t1 (v1) values (", 10+i, ")"))
		}
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		is.Error(t, context.DeadlineExceeded, failed(t, second.PingContext(ctx)))
		is.Equal(t, 4, len(values(t, first, select1)))

		// The lease of the first lasts an hour, so the second serves only because the close of the
		// first releases it.
		is.NotError(t, first.Close())
		is.NotError(t, second.PingContext(t.Context()))
		is.Equal(t, 4, len(values(t, second, select1)))
		exec(t, second, "insert into t1 (v1) values (2)")
	})
}

func TestOpen_cacheDir(t *testing.T) {
	t.Run("should make a second instance of the same database on the same cache directory a spare, which serves from the cache when it gets the lease", func(t *testing.T) {
		w := newWorld(t)
		first := w.serve(t, "a")
		exec(t, first, t1, insert1)

		second := w.open(t, "a")
		_, err := second.QueryContext(t.Context(), select1)
		is.Error(t, s4ql.ErrNotServing, failed(t, err))
		exec(t, first, "insert into t1 (v1) values (2)")

		is.NotError(t, first.Close())
		is.NotError(t, second.PingContext(t.Context()))
		is.EqualSlice(t, []int64{1, 2}, values(t, second, select1))
		exec(t, second, "insert into t1 (v1) values (3)")
		is.EqualSlice(t, []int64{1, 2, 3}, values(t, second, select1))
	})

	t.Run("should record in a new cache the endpoint, the bucket, and the prefix of its database", func(t *testing.T) {
		w := newWorld(t)
		for i, c := range []struct {
			endpoint, bucket, prefix, want string
		}{
			{endpoint: "", bucket: "bucket", prefix: "", want: "s3://bucket/"},
			{endpoint: "", bucket: "bucket", prefix: "app/one", want: "s3://bucket/app/one"},
			{endpoint: "https://t3.storage.dev", bucket: "bucket", prefix: "app", want: "s3://bucket/app at https://t3.storage.dev"},
			{endpoint: "http://127.0.0.1:9000", bucket: "my.bucket", prefix: "a b/smørrebrød?x", want: "s3://my.bucket/a%20b/sm%C3%B8rrebr%C3%B8d%3Fx at http://127.0.0.1:9000"},
		} {
			o := w.options(fmt.Sprint(i))
			o.Endpoint, o.Bucket, o.Prefix = c.endpoint, c.bucket, c.prefix
			db, err := s4ql.OpenStore(t.Context(), w.host, w.client(), o, coreOptions())
			is.NotError(t, err)
			is.NotError(t, db.Close())
			is.Equal(t, c.want, cacheIdentity(t, o.CacheDir))
		}
	})

	t.Run("should refuse at open a cache directory of another database, whether another instance runs on it or not, and leave the cache as it is", func(t *testing.T) {
		w := newWorld(t)
		o := w.options("a")
		o.Endpoint, o.Bucket = "https://t3.storage.dev", "bucket"
		first, err := s4ql.OpenStore(t.Context(), w.host, w.client(), o, coreOptions())
		is.NotError(t, err)
		is.NotError(t, first.PingContext(t.Context()))
		exec(t, first, t1, insert1)

		for _, running := range []bool{true, false} {
			if !running {
				is.NotError(t, first.Close())
			}
			for _, c := range []struct {
				name   string
				change func(o *s4ql.Options)
				opens  string
			}{
				{name: "another prefix", change: func(o *s4ql.Options) { o.Prefix = "other" }, opens: "s3://bucket/other at https://t3.storage.dev"},
				{name: "another bucket", change: func(o *s4ql.Options) { o.Bucket = "other" }, opens: "s3://other/db at https://t3.storage.dev"},
				{name: "another endpoint", change: func(o *s4ql.Options) { o.Endpoint = "https://fly.storage.tigris.dev" }, opens: "s3://bucket/db at https://fly.storage.tigris.dev"},
				{name: "no endpoint", change: func(o *s4ql.Options) { o.Endpoint = "" }, opens: "s3://bucket/db"},
			} {
				other := o
				c.change(&other)
				_, err := s4ql.OpenStore(t.Context(), w.host, w.client(), other, coreOptions())
				want := fmt.Sprintf(`s4ql: open the cache: the cache belongs to another database: it records the database "s3://bucket/db at https://t3.storage.dev", and this instance opens the database %q; open each database with a cache of its own, or delete the cache, with its files -wal and -shm, if the database has moved`, c.opens)
				is.Equal(t, want, failed(t, err).Error(), c.name)
				is.True(t, !errors.Is(err, s4ql.ErrStopped), c.name)
			}
		}

		// The cache is as it was, and its own database opens on it.
		is.Equal(t, "s3://bucket/db at https://t3.storage.dev", cacheIdentity(t, o.CacheDir))
		again, err := s4ql.OpenStore(t.Context(), w.host, w.client(), o, coreOptions())
		is.NotError(t, err)
		defer func() { is.NotError(t, again.Close()) }()
		is.NotError(t, again.PingContext(t.Context()))
		is.EqualSlice(t, []int64{1}, values(t, again, select1))
	})

	t.Run("should open exactly one of two databases that open one new cache directory at once, many times", func(t *testing.T) {
		w := newWorld(t)
		for i := range 20 {
			prefixes := []string{fmt.Sprint("one", i), fmt.Sprint("two", i)}
			dbs := make([]*sql.DB, len(prefixes))
			errs := make([]error, len(prefixes))
			start := make(chan struct{})
			var wg sync.WaitGroup
			for j, prefix := range prefixes {
				o := w.options(fmt.Sprint(i))
				o.Prefix = prefix
				s := w.client()
				wg.Go(func() {
					<-start
					dbs[j], errs[j] = s4ql.OpenStore(t.Context(), w.host, s, o, coreOptions())
				})
			}
			close(start)
			wg.Wait()

			is.True(t, (errs[0] == nil) != (errs[1] == nil), fmt.Sprint("not exactly one opened: ", errs))
			winner := 0
			if errs[0] != nil {
				winner = 1
			}
			loser := 1 - winner
			is.True(t, strings.Contains(failed(t, errs[loser]).Error(), fmt.Sprintf(`it records the database "s3:///%v", and this instance opens the database "s3:///%v"`, prefixes[winner], prefixes[loser])), errs[loser].Error())
			db := dbs[winner]
			is.NotError(t, db.PingContext(t.Context()))
			exec(t, db, t1)
			is.NotError(t, db.Close())
		}
	})
}

// cacheIdentity is the identity of the database that the cache in the directory dir records.
func cacheIdentity(t *testing.T, dir string) string {
	t.Helper()
	c, err := sqlite.Open(filepath.Join(dir, "cache.db"))
	is.NotError(t, err)
	defer func() { is.NotError(t, c.Close()) }()
	s, err := c.Prepare("select database from s4ql_cache")
	is.NotError(t, err)
	ok, err := s.Step()
	is.NotError(t, err)
	is.True(t, ok, "the cache records no identity")
	identity := s.ColumnText(0)
	is.NotError(t, s.Finalize())
	return identity
}

func TestOptions_Logger(t *testing.T) {
	t.Run("should log the open, the close, and the first answer of a stop, with the events of the core and of the engine", func(t *testing.T) {
		w := newWorld(t)
		vfs, err := faultvfs.New()
		is.NotError(t, err)
		t.Cleanup(func() { is.NotError(t, vfs.Close()) })
		co := coreOptions()
		co.VFS = vfs.Name()
		var log lines
		o := w.options("a")
		o.Logger = slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
		db, err := s4ql.OpenStore(t.Context(), w.host, w.client(), o, co)
		is.NotError(t, err)
		defer func() { _ = db.Close() }()

		is.EqualSlice(t, []string{"level=INFO msg=open db.prefix=db cache.path=" + filepath.Join(o.CacheDir, "cache.db") + " db.identity=s3:///db"}, log.of("open"))
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, t1)
		is.Equal(t, 1, len(log.of("statement")))
		is.True(t, len(log.of("lease")) > 0, "no event of the core")
		is.Equal(t, 0, len(log.of("stop_seen")))

		// The cache fails at the insert, which stops the instance.
		vfs.Fail(faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.WAL})
		_, _ = db.ExecContext(t.Context(), insert1)
		for range 3 {
			_, err = db.ExecContext(t.Context(), insert1)
			is.Error(t, s4ql.ErrStopped, failed(t, err))
		}
		is.Error(t, s4ql.ErrStopped, failed(t, db.PingContext(t.Context())))
		seen := log.of("stop_seen")
		is.Equal(t, 1, len(seen))
		is.True(t, strings.HasPrefix(seen[0], `level=WARN msg=stop_seen db.prefix=db error="s4ql: `), seen[0])
		is.Equal(t, 1, len(log.of("stop")))

		is.NotError(t, db.Close())
		is.EqualSlice(t, []string{"level=INFO msg=close db.prefix=db"}, log.of("close"))
	})

	t.Run("should log a close that fails as a warning", func(t *testing.T) {
		w := newWorld(t)
		s := &faulty{Store: w.client()}
		var log lines
		o := w.options("a")
		o.Logger = slog.New(slog.NewTextHandler(&log, nil))
		o.CloseTimeout = 100 * time.Millisecond
		db, err := s4ql.OpenStore(t.Context(), w.host, s, o, coreOptions())
		is.NotError(t, err)
		is.NotError(t, db.PingContext(t.Context()))
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		s.down(true)
		_ = failed(t, db.Close())
		closes := log.of("close")
		is.Equal(t, 1, len(closes))
		is.True(t, strings.HasPrefix(closes[0], "level=WARN msg=close db.prefix=db error="), closes[0])

		// A statement on a closed instance is answered with a stop, which is no stop to report.
		_, err = conn.ExecContext(t.Context(), t1)
		is.Error(t, s4ql.ErrStopped, failed(t, err))
		is.NotError(t, conn.Close())
		is.Equal(t, 0, len(log.of("stop_seen")))
	})
}

// lines of a log in the text format of log/slog.
type lines struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// of the events with the given name, each without its time.
func (l *lines) of(event string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var events []string
	for line := range strings.Lines(l.b.String()) {
		_, rest, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		if strings.Contains(rest, " msg="+event+" ") || strings.HasSuffix(rest, " msg="+event) {
			events = append(events, rest)
		}
	}
	return events
}

// drivers counts the names that [driverName] has given.
var drivers atomic.Int64

// driverName that no test has registered a driver under: database/sql keeps every driver for the
// life of the process, and a test can run many times in it.
func driverName() string {
	return fmt.Sprint("s4ql-test-", drivers.Add(1))
}

func TestRegister(t *testing.T) {
	t.Run("should open an instance for each sql.Open with the name, and refuse a data source name", func(t *testing.T) {
		w := newWorld(t)
		name := driverName()
		s4ql.RegisterStore(name, w.host, w.client(), w.options("a"), coreOptions())

		_, err := sql.Open(name, "bucket/db")
		is.Equal(t, "s4ql: open: the data source name must be empty, because the options given to Register name the database", failed(t, err).Error())

		db, err := sql.Open(name, "")
		is.NotError(t, err)
		is.NotError(t, db.PingContext(t.Context()))
		exec(t, db, t1, insert1)
		is.EqualSlice(t, []int64{1}, values(t, db, select1))
		is.NotError(t, db.Close())

		// A second sql.Open is a second instance, which serves after the first has released the lease.
		db, err = sql.Open(name, "")
		is.NotError(t, err)
		defer func() { is.NotError(t, db.Close()) }()
		is.NotError(t, db.PingContext(t.Context()))
		is.EqualSlice(t, []int64{1}, values(t, db, select1))

		// The driver of a database opens a connector and no single connection.
		_, err = db.Driver().Open("")
		is.True(t, strings.HasPrefix(failed(t, err).Error(), "s4ql: "), err.Error())
	})

	t.Run("should fail on a local problem at sql.Open", func(t *testing.T) {
		name := driverName()
		s4ql.Register(name, s4ql.Options{CacheDir: t.TempDir()})
		_, err := sql.Open(name, "")
		is.Equal(t, "s4ql: open: the options name no bucket", failed(t, err).Error())
	})

	t.Run("should panic for a name that is registered, as sql.Register does", func(t *testing.T) {
		name := driverName()
		s4ql.Register(name, s4ql.Options{})
		defer func() {
			is.Equal(t, "sql: Register called twice for driver "+name, fmt.Sprint(recover()))
		}()
		s4ql.Register(name, s4ql.Options{})
	})

	t.Run("should register nothing by itself", func(t *testing.T) {
		for _, name := range sql.Drivers() {
			is.True(t, strings.HasPrefix(name, "s4ql-test-"), name)
		}
	})
}

// TestOpen_s3 is [s4ql.Open] and [s4ql.Register] as an application calls them, on a server that
// answers every request with an error, because the tests use no real bucket: it shows what the
// options reach the store with, and that an instance whose store fails is a spare.
func TestOpen_s3(t *testing.T) {
	isolate(t)
	// requests to the server, of which the test reads the first of each instance. It holds more
	// than the instances of the test send before they are closed, and a request beyond it is dropped.
	requests := make(chan string, 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization"):
		default:
		}
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	options := func(prefix string) s4ql.Options {
		return s4ql.Options{
			Bucket: "bucket", Prefix: prefix, Endpoint: srv.URL, Region: "eu-north-1", AccessKeyID: "AKIDOFOPTIONS", SecretAccessKey: "secret",
			PathStyle: true, CacheDir: t.TempDir(),
		}
	}

	t.Run("should open on a store that fails, as a spare, and ask the store for the manifest of the database with the credentials of the options", func(t *testing.T) {
		for i, open := range []func(o s4ql.Options) (*sql.DB, error){
			func(o s4ql.Options) (*sql.DB, error) { return s4ql.Open(t.Context(), o) },
			func(o s4ql.Options) (*sql.DB, error) {
				name := driverName()
				s4ql.Register(name, o)
				return sql.Open(name, "")
			},
		} {
			// A prefix of its own for each instance, so that no request of the other one is taken for
			// one of this.
			prefix := fmt.Sprint("app/db", i)
			db, err := open(options(prefix))
			is.NotError(t, err)
			_, err = db.ExecContext(t.Context(), t1)
			is.Error(t, s4ql.ErrNotServing, failed(t, err))
			_, err = db.ExecContext(t.Context(), "selec")
			is.Equal(t, `near "selec": syntax error`, failed(t, err).Error())

			// The instance reads the manifest as soon as it has started. No request of an instance
			// that was closed is left, because the close waits for the instance's requests.
			first := <-requests
			is.True(t, strings.HasPrefix(first, "GET /bucket/"+prefix+"/manifest.json AWS4-HMAC-SHA256 Credential=AKIDOFOPTIONS/"), first)
			is.True(t, strings.Contains(first, "/eu-north-1/s3/aws4_request"), first)
			is.NotError(t, db.Close())
		}
	})

	t.Run("should fail on options that cannot reach a store", func(t *testing.T) {
		cases := []struct {
			name   string
			change func(o *s4ql.Options)
			want   string
		}{
			{name: "no bucket", change: func(o *s4ql.Options) { o.Bucket = "" }, want: "s4ql: open: the options name no bucket"},
			{name: "an endpoint that is no URL", change: func(o *s4ql.Options) { o.Endpoint = "store" }, want: "s4ql: open: "},
			{name: "no region", change: func(o *s4ql.Options) { o.Region = "" }, want: "s4ql: open: "},
			{name: "an access key without its secret", change: func(o *s4ql.Options) { o.SecretAccessKey = "" }, want: "s4ql: open: "},
			{name: "no cache directory", change: func(o *s4ql.Options) { o.CacheDir = "" }, want: "s4ql: open: the options name no cache directory"},
		}
		for _, c := range cases {
			o := options("app/db")
			c.change(&o)
			_, err := s4ql.Open(t.Context(), o)
			is.True(t, strings.HasPrefix(failed(t, err).Error(), c.want), fmt.Sprint(c.name, ": ", err))
		}
	})
}

// isolate the test from the AWS configuration of the machine that runs it: no shared files, and no
// settings of the SDK from the environment.
func isolate(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	for _, name := range []string{
		"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_S3",
		"AWS_CA_BUNDLE", "AWS_DEFAULTS_MODE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
	} {
		t.Setenv(name, "")
	}
}

// faulty is a store that a test makes fail: it can be down, so that every request fails without
// reaching the store, and it can lose the answer of one swap.
type faulty struct {
	store.Store

	mu sync.Mutex
	// isDown makes every request fail.
	isDown bool
	// lose, if it is not nil, is called when the next swap has been applied, whose answer is then
	// lost, and the store is down from then on.
	lose func()
	// create, if it is not nil, is called before the next create is sent.
	create func()
}

var errFault = errors.New("fault of the test")

// down sets whether every request fails.
func (f *faulty) down(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isDown = down
}

// loseNext makes the store apply the next swap, lose its answer, call then, and be down.
func (f *faulty) loseNext(then func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lose = then
}

// beforeCreate makes the store call then before it sends the next create.
func (f *faulty) beforeCreate(then func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.create = then
}

func (f *faulty) check() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.isDown {
		return errFault
	}
	return nil
}

func (f *faulty) Get(ctx context.Context, key string) (io.ReadCloser, store.Version, error) {
	if err := f.check(); err != nil {
		return nil, "", err
	}
	return f.Store.Get(ctx, key)
}

func (f *faulty) Create(ctx context.Context, key string, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	if err := f.check(); err != nil {
		return "", err
	}
	f.mu.Lock()
	then := f.create
	f.create = nil
	f.mu.Unlock()
	if then != nil {
		then()
	}
	return f.Store.Create(ctx, key, content, checksum)
}

func (f *faulty) Swap(ctx context.Context, key string, from store.Version, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	if err := f.check(); err != nil {
		return "", err
	}
	v, err := f.Store.Swap(ctx, key, from, content, checksum)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil && f.lose != nil {
		f.lose()
		f.lose, f.isDown = nil, true
		return "", errFault
	}
	return v, err
}
