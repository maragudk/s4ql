// Package s4ql is a SQL database whose data is in S3, or in an object store that is compatible with
// it, used through [database/sql]. The name is short for "simple storage service + structured query
// language".
//
// [Open] gives a [database/sql.DB] for one database, which is a key prefix in a bucket:
//
//	db, err := s4ql.Open(ctx, s4ql.Options{Bucket: "bucket", Prefix: "app", CacheDir: "/var/lib/app/cache"})
//	if err != nil {
//		return err
//	}
//	defer db.Close()
//	if err := db.PingContext(ctx); err != nil {
//		return err
//	}
//	_, err = db.ExecContext(ctx, "create table t1 (v1 int primary key) strict, without rowid")
//
// The package has no API of its own next to database/sql: it holds the [Options], the errors, [Open],
// and [Register].
//
// # Instances and the lease
//
// Each [database/sql.DB] from this package is one instance of the database, with a local cache of
// its own. An instance reads and writes only while it holds the lease of the database, which one
// instance at a time holds. [Open] does not wait for the lease, and fails only on local problems.
// [database/sql.DB.PingContext] waits until the instance serves, or until its context ends;
// [database/sql.DB.Ping] has no context, and waits without an end. Until the instance serves, and
// whenever it has lost the lease, a statement fails at once with an error that matches
// [ErrNotServing], and the instance keeps trying to get the lease. An instance that starts while
// another one holds the lease is a spare, which is a normal state: it serves when the other one has
// closed, or when the lease of the other one has run out.
//
// [database/sql.DB.Close] closes the instance and releases the lease, within
// [Options.CloseTimeout], so that the next instance serves without waiting. If the process ends
// without it, or the release fails, the lease runs out by itself, as after a crash, and the next
// instance waits for that. That is correct, only slower. An instance that is not closed keeps the
// lease for as long as its process runs.
//
// An instance stops for good when its cache fails or when it meets something in the object store
// that it cannot read. Every statement that reaches the instance, and every ping, then fails with
// an error that matches [ErrStopped] and carries the cause. What the text of a statement alone
// decides, a syntax error or "not supported", is answered as before. Nothing brings the instance
// back: the application closes the [database/sql.DB] and opens the database again.
//
// # Statements
//
// The SQL is a strict subset of SQLite's, and what a statement returns is what SQLite returns for
// it. Each statement is a transaction of its own, which is committed when the statement returns
// without an error. A statement is bounded by the context that it is given, and so is its commit.
// Parameters, [database/sql.DB.BeginTx], and several statements in one text are not supported.
// [database/sql.DB.PrepareContext] checks the text of the statement, and neither the tables that it
// names nor whether the instance serves.
//
// [database/sql.Result.RowsAffected] is SQLite's count of changes on the connection: an insert sets
// it, and a statement that sets none, such as CREATE TABLE, gives the count of the statement before
// it on the same connection. database/sql takes any connection of its pool for a statement, so the
// count of such a statement means something only on a [database/sql.Conn].
// [database/sql.Result.LastInsertId] is always 0, because no table has a rowid.
//
// One write runs at a time, and a second one waits for the first. Reads run in parallel, each on
// one consistent state of the database, and the pool of database/sql sets how many run at once:
// each connection of the pool runs one read at a time. [database/sql.DB.SetMaxOpenConns] therefore
// bounds the reads that run at once, and a further statement then waits for a connection, until one
// is free or its context ends: also one that writes, because the bound is on every statement.
// [Open] sets a bound from the limit of open files of the process, as it says, and the application
// can change it. The pool of a database of [Register] has no bound until the application sets one.
//
// A connection opens a connection to the cache of its own for its first read, which is two open
// files, and closes it when database/sql closes the connection, so the other settings of the pool,
// such as [database/sql.DB.SetMaxIdleConns] and [database/sql.DB.SetConnMaxIdleTime], decide how
// long it is kept. A process that has no file descriptor left for it has a cache that cannot be
// opened, which stops the instance as any failure of the cache does. The bound that [Open] sets
// keeps the readers of one instance to half of the file descriptors of the process.
//
// The read of a query runs until its [database/sql.Rows] are read to the end or closed, so rows
// that are neither hold their connection and its read, until the context of their query ends or
// the database is closed. A connection runs one read at a time: on a [database/sql.Conn], a
// statement that reads while the rows of an earlier query of that connection are open is not
// supported, and fails at once with an error that matches [ErrNotSupported]. A statement that
// changes the database runs there all the same. database/sql closes the rows of a query whose
// context has ended by itself, some time after, so the next statement on that connection is sure
// to find them closed only after [database/sql.Rows.Close].
//
// # Errors
//
// The error of the driver for a statement is one of these:
//
//   - SQLite's error, as an [*Error], with SQLite's result code, extended result code, and message.
//   - An error that matches [ErrNotSupported], for SQL that SQLite accepts and s4ql does not support.
//     It carries no result code, and its message starts with "s4ql: not supported: ".
//   - An error that matches [ErrNotServing], [ErrOutcomeUnknown], or [ErrStopped], for the state of
//     the instance.
//   - Another error, whose message starts with "s4ql: ", such as one that wraps the error of a
//     context that ended.
//
// Every one of them but one that matches [ErrOutcomeUnknown] means that the statement is not
// committed. The driver never returns [database/sql/driver.ErrBadConn], so database/sql never runs a
// statement a second time by itself.
//
// database/sql has errors of its own, which say nothing about a commit: the error of a context that
// has ended before the statement starts, as it is, that of a database that is closed, and
// [database/sql.ErrNoRows] from [database/sql.Row.Scan], also for a statement that is no query and
// is committed.
package s4ql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strings"
	"time"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/store/s3store"
)

// Options of a database, for [Open] and [Register]. The zero value of an option that is not
// required means its default.
type Options struct {
	// Bucket that holds the database. It is required.
	Bucket string

	// Prefix of the keys of the database in the bucket. Each prefix is a database of its own, so one
	// bucket can hold many. An empty prefix means that the keys have none.
	Prefix string

	// Endpoint of the object store, an http or https URL such as https://t3.storage.dev for Tigris.
	// If it is empty, it comes from the environment variable AWS_ENDPOINT_URL_S3 or the shared AWS
	// configuration, and is otherwise Amazon S3 in the region.
	//
	// The cache records the endpoint as this option gives it, with the bucket and the prefix, as the
	// database that it belongs to: see [Open]. Another text for the same store, such as an empty
	// option in place of the URL, or a URL with a slash at its end, is therefore another database to
	// the cache, and needs another cache directory. A change of the environment or of the shared AWS
	// configuration is not seen.
	Endpoint string

	// Region of the bucket, such as eu-north-1, or auto for Tigris. If it is empty, it comes from
	// the environment variable AWS_REGION or the shared AWS configuration, and one of them must
	// give it.
	Region string

	// AccessKeyID and SecretAccessKey are the credentials for the object store. If both are empty,
	// they come from the default chain of the AWS SDK: the environment variables AWS_ACCESS_KEY_ID
	// and AWS_SECRET_ACCESS_KEY, the shared AWS configuration, or the role of the machine.
	AccessKeyID     string
	SecretAccessKey string

	// PathStyle puts the bucket into the path of each request instead of into the host name, which
	// some object stores need.
	PathStyle bool

	// CacheDir is the directory of the local cache of the database, which is created if it does not
	// exist. It is required. A cache directory belongs to one database, and is for one instance at a
	// time: see [Open]. The cache is kept when the instance closes, and the next instance on the
	// directory catches it up from where it stands. A cache that is lost is built again from the
	// object store.
	CacheDir string

	// CloseTimeout is how long [database/sql.DB.Close] waits for the instance to close and to
	// release the lease. The default is 10 seconds.
	CloseTimeout time.Duration

	// Logger of the events of the instance. The default discards them. A healthy instance logs
	// nothing above the level Debug while it serves, and its start and its close at Info. The SQL
	// text of a statement is logged at Debug only.
	Logger *slog.Logger
}

// Open the database of the options, and give a [database/sql.DB] for it, which is one instance of
// the database.
//
// It fails only on local problems: options that make no sense, and a cache that cannot be opened.
// The context bounds only that local work. Open does not contact the object store and does not
// wait for the lease: [database/sql.DB.PingContext] waits until the instance serves.
//
// The pool of the [database/sql.DB] has a bound, which [database/sql.DB.SetMaxOpenConns] sets: a
// quarter of the soft limit of open files of the process, RLIMIT_NOFILE, as Open reads it, and at
// least 1. A later change of the limit does not change the bound. Each connection of the pool has a
// reader, a connection to the cache, which holds up to two file descriptors, for the cache and its
// write-ahead log, so the readers of the instance use at most half of the descriptors of the
// process, and the rest are for the application and the other files of the instance. The bound is
// for each instance: an application that opens several instances in one process lowers the bound
// of each, so that their readers together keep to half. SQLite keeps the descriptor of the cache of
// a reader that has closed until the instance closes, and gives it to the next reader that opens,
// so the descriptors of the readers stay at the most readers that were open at once. Go raises the soft limit of a process when it starts, on most
// systems to the hard limit, so it is what the process can use unless the application lowers it. A
// process whose limit is unlimited or cannot be read, and one on a platform without the limit, such
// as Windows, gets no bound, as database/sql gives none by default. The application can change the
// bound with SetMaxOpenConns, as on any [database/sql.DB], and then makes sure itself that the
// process has the descriptors for it: a reader that cannot be opened stops the instance.
//
// A cache directory belongs to one database: the endpoint, the bucket, and the prefix of the
// options, as they are given, and with nothing else of the options. A new cache records them, and
// Open fails for a cache that records another database, also while an instance of that database
// runs on it. The error names both databases and starts with "s4ql: open the cache: the cache
// belongs to another database: ". Nothing rebuilds or takes over such a cache: give each database
// a cache directory of its own, or, if the database has moved, delete the files of the cache, which
// the next instance builds again from the object store. Of two instances of two databases that
// open one new cache directory at once, one opens and the other fails so.
//
// A cache directory is for one instance at a time. A second instance of the same database on the
// same cache directory is a spare, as any second instance is, and serves from the same cache when
// it gets the lease.
func Open(ctx context.Context, options Options) (*sql.DB, error) {
	return (&opener{connect: options.connector}).openDB(ctx)
}

// maxOpenConns is the bound of the pool of a process whose limit of open files is limit, if it has
// one: a quarter of the limit, at least 1, and at most the largest int. A process without a limit
// gets 0, which is no bound to [database/sql.DB.SetMaxOpenConns]. A reader holds up to two file
// descriptors, and the readers get half of the limit.
func maxOpenConns(limit uint64, limited bool) int {
	if !limited {
		return 0
	}
	return int(min(max(limit/4, 1), math.MaxInt))
}

// Register a driver for database/sql under the name, which opens the database of the options. It is
// for code that needs [database/sql.Open] with the name of a driver, such as a migration tool;
// other code uses [Open]. The package registers no driver by itself.
//
// Each call of [database/sql.Open] with the name opens one instance of the database, as [Open]
// does, and fails on the same local problems. Its data source name must be empty, because the
// options name the database.
//
// The pool of a [database/sql.DB] of [database/sql.Open] has no bound, because database/sql makes
// the DB, and the driver never sees it. The application sets one with
// [database/sql.DB.SetMaxOpenConns], such as the bound that [Open] sets: each connection of the pool
// holds up to two file descriptors, and without a bound a burst of queries can use up those of the
// process, which stops the instance.
//
// Register panics if a driver is registered under the name already, as [database/sql.Register]
// does.
func Register(name string, options Options) {
	sql.Register(name, &opener{connect: options.connector})
}

// connector of a new instance of the database of the options, on the production host and on S3,
// for the driver d, whose pool has the bound maxOpen.
func (o Options) connector(ctx context.Context, d driver.Driver, maxOpen int) (*connector, error) {
	if o.Bucket == "" {
		return nil, errors.New("s4ql: open: the options name no bucket")
	}
	s, err := s3store.New(ctx, s3store.Options{
		Bucket:          o.Bucket,
		Endpoint:        o.Endpoint,
		Region:          o.Region,
		AccessKeyID:     o.AccessKeyID,
		SecretAccessKey: o.SecretAccessKey,
		PathStyle:       o.PathStyle,
		Logger:          o.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("s4ql: open: %w", err)
	}
	return newConnector(ctx, host.New(), s, o, core.Options{}, d, maxOpen)
}

// identity of the database of the options, which the cache records: an s3 URL of the bucket and the
// prefix, followed by " at " and the endpoint if the options give one. The bucket and each segment
// of the prefix are escaped as segments of a path, which escapes the slash, the space, and the
// question mark, so that two databases never give one identity.
func (o Options) identity() string {
	segments := strings.Split(o.Prefix, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	identity := "s3://" + url.PathEscape(o.Bucket) + "/" + strings.Join(segments, "/")
	if o.Endpoint != "" {
		identity += " at " + o.Endpoint
	}
	return identity
}

// opener is the [driver.Driver] of this package: it opens an instance with connect for each
// connector that it is asked for.
type opener struct {
	// connect gives the connector of a new instance, for the driver d, whose pool has the bound
	// maxOpen, 0 for none.
	connect func(ctx context.Context, d driver.Driver, maxOpen int) (*connector, error)
}

var _ driver.DriverContext = (*opener)(nil)

// openDB opens a new instance, as [Open] does, and gives the [sql.DB] of it, whose pool has the
// bound of [maxOpenConns] for the limit of open files of the process.
func (d *opener) openDB(ctx context.Context) (*sql.DB, error) {
	maxOpen := maxOpenConns(fileLimit())
	c, err := d.connect(ctx, d, maxOpen)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(maxOpen)
	return db, nil
}

// Open is not how database/sql opens a connection of this driver: it asks for a connector, because
// the driver is a [driver.DriverContext]. A connection alone would be an instance of its own.
func (d *opener) Open(string) (driver.Conn, error) {
	return nil, errors.New("s4ql: the driver opens a database through its connector, as sql.Open does, and not a single connection")
}

// OpenConnector of a new instance, whose pool has no bound: database/sql makes the [sql.DB] of
// it, which the driver never sees. The data source name must be empty.
func (d *opener) OpenConnector(name string) (driver.Connector, error) {
	if name != "" {
		return nil, errors.New("s4ql: open: the data source name must be empty, because the options given to Register name the database")
	}
	// database/sql gives no context here.
	c, err := d.connect(context.Background(), d, 0)
	if err != nil {
		return nil, err
	}
	return c, nil
}
