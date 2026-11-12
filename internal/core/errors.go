package core

import (
	"errors"
	"fmt"

	"maragu.dev/s4ql/internal/sqlite"
)

var (
	// ErrNoLease is wrapped by the error of a read or a write that the instance refuses because it
	// has no valid lease: it does not hold the lease, it holds it and its cache is not caught up, a
	// commit of it has an outcome that it does not know yet, or its clocks say that the lease has run
	// out. The error is temporary: the instance keeps trying to get the lease, so the same call can
	// succeed later, and it can succeed now on another instance.
	ErrNoLease = errors.New("no valid lease")

	// ErrOutcomeUnknown is wrapped by the error of [Tx.Commit] when the context of the caller ended
	// before the instance knew whether the transaction is committed. The transaction may be
	// committed, may not be, and may still become committed. It is the one error of a commit that
	// does not mean "not committed".
	ErrOutcomeUnknown = errors.New("the outcome of the commit is unknown")

	// ErrStopped is wrapped by the error of every call on an instance that has stopped for good, and
	// by [DB.Err]. The same error wraps the cause: [ErrClosed], [ErrLocal], [ErrCacheNotInLog],
	// [ErrLogBroken], or one of the three refusals of [maragu.dev/s4ql/internal/format].
	ErrStopped = errors.New("the instance has stopped")

	// ErrClosed is the cause of the stop of an instance that was closed.
	ErrClosed = errors.New("closed")

	// ErrLocal is wrapped by the cause of the stop of an instance whose cache or spool file failed:
	// a local I/O failure. It also wraps the error of [Open] for a cache that cannot be opened.
	ErrLocal = errors.New("local I/O failure")

	// ErrCacheNotInLog is wrapped by the cause of the stop of an instance whose cache records a
	// position that is not in the log that the manifest points to. The cache belongs to another
	// database, or to a log that is gone. Nothing rebuilds it: a person or the application chooses
	// that, for example by opening the database with an empty cache.
	ErrCacheNotInLog = errors.New("the position of the cache is not in the log")

	// ErrLogBroken is wrapped by the cause of the stop of an instance that finds the log incomplete
	// or in disagreement with itself: a commit object that the log names is not in the store, or a
	// commit object does not fit the tables and rows that the commit objects before it leave.
	ErrLogBroken = errors.New("the log is broken")

	// ErrCacheVersion is wrapped by the error of [Open] for a cache that a newer version of s4ql
	// made, or that s4ql did not make.
	ErrCacheVersion = errors.New("the cache is not one that this version of s4ql reads")

	// ErrBusy is wrapped by the error of a call for which SQLite reported that the cache is locked,
	// after waiting for the busy timeout. Another process has the cache open, or s4ql has a bug. It
	// is not a local I/O failure, and the instance goes on.
	ErrBusy = errors.New("the cache is busy")

	// ErrReservedName is wrapped by the error of [Tx.CreateTable] for a table whose name starts
	// with "s4ql_" or "sqlite_", in any case of the letters.
	ErrReservedName = errors.New("the name is reserved")

	// ErrTableExists is wrapped by the error of [Tx.CreateTable] for a table that exists.
	ErrTableExists = errors.New("the table exists")

	// ErrNoTable is wrapped by the error of a call that names a table that does not exist.
	ErrNoTable = errors.New("no such table")

	// ErrInvalid is wrapped by the error of a call whose table, row, key, or annotation is not one
	// that the database can hold: a table without columns, a row with the wrong number of values, a
	// value of the wrong storage class for its column, or NULL in a primary key.
	ErrInvalid = errors.New("invalid")

	// ErrTxDone is the error of a call on a transaction that was committed, rolled back, or closed.
	ErrTxDone = errors.New("the transaction has ended")
)

// errLeaseLost is the reason for [ErrNoLease] when another instance has taken the lease.
var errLeaseLost = fmt.Errorf("%w: not_holder", ErrNoLease)

// SQLite's primary result codes that the core tells apart, from https://sqlite.org/rescode.html.
const (
	sqlitePerm     = 3
	sqliteBusy     = 5
	sqliteNoMem    = 7
	sqliteReadOnly = 8
	sqliteIOErr    = 10
	sqliteCorrupt  = 11
	sqliteFull     = 13
	sqliteCantOpen = 14
	sqliteProtocol = 15
	sqliteNoLFS    = 22
	sqliteNotADB   = 26
)

// errorKind of an error from the cache.
type errorKind int

const (
	// kindOther is an error that is neither of the two below: the statement or its data is wrong.
	kindOther errorKind = iota
	// kindBusy is SQLite's "busy", after the busy timeout.
	kindBusy
	// kindLocal is a local I/O failure, which stops the instance.
	kindLocal
)

// classify an error from the cache by SQLite's primary result code. A local I/O failure is an I/O
// error, a full disk, a file that cannot be opened, a corrupt file, a file that is not a database,
// no large-file support, a denied permission, and a cache that is read-only though it was opened for
// writing. Out of memory and SQLite's protocol error count as one too, for simplicity. "Busy" is
// its own kind, and never a local I/O failure.
func classify(err error) errorKind {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return kindOther
	}
	switch sqliteErr.Code.Primary() {
	case sqliteBusy:
		return kindBusy
	case sqliteIOErr, sqliteFull, sqliteCantOpen, sqliteCorrupt, sqliteNotADB, sqliteNoLFS, sqlitePerm, sqliteReadOnly, sqliteNoMem, sqliteProtocol:
		return kindLocal
	}
	return kindOther
}

// invalidf is an error that wraps [ErrInvalid].
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %v", ErrInvalid, fmt.Sprintf(format, args...))
}

// brokenf is an error that wraps [ErrLogBroken].
func brokenf(format string, args ...any) error {
	return fmt.Errorf("%w: %v", ErrLogBroken, fmt.Sprintf(format, args...))
}
