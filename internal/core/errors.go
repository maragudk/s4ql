package core

import (
	"context"
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
	// does not mean "not committed", and an error that wraps it wraps no other error.
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
	// position that is not in the log that the manifest points to. The cache belongs to a log that is
	// gone, such as that of a database that was deleted and made again, or to another database that
	// was opened with the same [Options.Identity]. Nothing rebuilds it: a person or the application chooses
	// that, for example by opening the database with an empty cache.
	ErrCacheNotInLog = errors.New("the position of the cache is not in the log")

	// ErrLogBroken is wrapped by the cause of the stop of an instance that finds the log incomplete
	// or in disagreement with itself: a commit object that the log names is not in the store, or a
	// commit object does not fit the tables and rows that the commit objects before it leave.
	ErrLogBroken = errors.New("the log is broken")

	// ErrCacheVersion is wrapped by the error of [Open] for a cache that a newer version of s4ql
	// made, or that s4ql did not make. An instance that finds such a cache while it runs stops, and
	// the cause of the stop wraps it under [ErrLocal].
	ErrCacheVersion = errors.New("the cache is not one that this version of s4ql reads")

	// ErrCacheDatabase is wrapped by the error of [Open] for a cache that records the identity of
	// another database than [Options.Identity]. Nothing rebuilds or adopts it: a person or the
	// application chooses another cache for the database, or deletes the cache if the database has
	// moved and so has a new identity.
	ErrCacheDatabase = errors.New("the cache belongs to another database")

	// ErrBusy is wrapped by the error of a call for which SQLite reported that the cache is locked,
	// after waiting for the busy timeout. Another process has the cache open, or s4ql has a bug. It
	// is not a local I/O failure, and the instance goes on.
	ErrBusy = errors.New("the cache is busy")

	// ErrTooLongForCache is wrapped, beside [ErrInvalid], by the error of [Tx.CreateTable] and of
	// [Tx.CreateIndex] for a table or an index whose statement in the cache is longer than the length
	// limit. The cache writes every name in double quotes and each default in its canonical form, so
	// its statement can be longer than a text that SQLite takes under the same limit.
	ErrTooLongForCache = errors.New("the statement in the cache is longer than the length limit")

	// ErrReservedName is wrapped by the error of [Tx.CreateTable] and of [Tx.CreateIndex] for a
	// table or an index whose name starts with "s4ql_" or "sqlite_", in any case of the letters.
	ErrReservedName = errors.New("the name is reserved")

	// ErrTableExists is wrapped by the error of [Tx.CreateTable] for a table that exists, and of
	// [Tx.CreateIndex] for an index whose name a table has.
	ErrTableExists = errors.New("the table exists")

	// ErrIndexExists is wrapped by the error of [Tx.CreateIndex] for an index that exists, and of
	// [Tx.CreateTable] for a table whose name an index has.
	ErrIndexExists = errors.New("the index exists")

	// ErrNoIndex is wrapped by the error of a call that names an index that does not exist, or that
	// the table of the call does not have.
	ErrNoIndex = errors.New("no such index")

	// ErrNoTable is wrapped by the error of a call that names a table that does not exist.
	ErrNoTable = errors.New("no such table")

	// ErrInvalid is wrapped by the error of a call whose table, row, key, or annotation is not one
	// that the database can hold: a table without columns, a row with the wrong number of values, a
	// value of another storage class than its column's type, a real that is NaN or infinite, text
	// that is not valid UTF-8 or holds the character NUL, or NULL in a primary key.
	ErrInvalid = errors.New("invalid")

	// ErrTooBig is wrapped by the error of a call whose text or blob is longer than the cache's
	// limit for one value, 1,000,000,000 bytes, and of a write whose row is larger than that limit,
	// as SQLite counts the bytes of a row: SQLite refuses both with SQLITE_TOOBIG, "string or blob
	// too big". The call changes nothing, and the transaction can go on.
	ErrTooBig = errors.New("string or blob too big")

	// ErrTooLarge is wrapped by the error of [Tx.Commit] when the store refuses the commit object of
	// the transaction because it is larger than the store takes in one write. The transaction is not
	// committed. The error is not temporary: the same changes give a commit object of the same size
	// again. The instance goes on serving.
	ErrTooLarge = errors.New("the commit object is too large for the store")

	// ErrTxLeaseLost is wrapped by the error of every call of a write transaction that the instance
	// ended because it lost its lease, until the caller ends the transaction with [Tx.Commit], which
	// fails with it too, or with [Tx.Rollback], which returns nil; a call after that gives
	// [ErrTxDone]. It wraps [ErrNoLease],
	// so the error is temporary: the transaction is not committed and never will be, and the same
	// work can succeed in a new transaction. An instance that has lost its lease ends the write
	// transaction that is open and not committing, so that it can take the lease back: see
	// [DB.endForLease].
	ErrTxLeaseLost = fmt.Errorf("%w: the transaction was ended because the instance lost its lease", ErrNoLease)

	// ErrTxDone is the error of a call on a transaction that was committed, rolled back, or closed.
	ErrTxDone = errors.New("the transaction has ended")

	// ErrReaderClosed is the error of [Reader.Read] on a reader that [Reader.Close] has closed.
	ErrReaderClosed = errors.New("the reader is closed")

	// ErrReaderBusy is the error of [Reader.Read] on a reader whose read transaction is open: a
	// reader runs one at a time.
	ErrReaderBusy = errors.New("the reader has a read transaction open")

	// ErrReaderInUse is the error of [Reader.Close] while a call of the read transaction of the
	// reader runs. Nothing is closed then.
	ErrReaderInUse = errors.New("a call uses the reader")
)

// errLeaseLost is the reason for [ErrNoLease] when another instance has taken the lease.
var errLeaseLost = fmt.Errorf("%w: not_holder", ErrNoLease)

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
	// The primary result codes are those of https://sqlite.org/rescode.html.
	switch sqliteErr.Code.Primary() {
	case 5: // SQLITE_BUSY
		return kindBusy
	case 10, // SQLITE_IOERR
		13, // SQLITE_FULL
		14, // SQLITE_CANTOPEN
		11, // SQLITE_CORRUPT
		26, // SQLITE_NOTADB
		22, // SQLITE_NOLFS
		3,  // SQLITE_PERM
		8,  // SQLITE_READONLY
		7,  // SQLITE_NOMEM
		15: // SQLITE_PROTOCOL
		return kindLocal
	}
	return kindOther
}

// ended reports whether err is the error of ctx, which has ended, as a step of the cache that SQLite
// stopped for ctx fails: see [conn.step].
func ended(ctx context.Context, err error) bool {
	ctxErr := ctx.Err()
	return err != nil && ctxErr != nil && errors.Is(err, ctxErr)
}

// isTooBig reports whether err is SQLite's SQLITE_TOOBIG, "string or blob too big".
func isTooBig(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code.Primary() == 18
}

// invalidf is an error that wraps [ErrInvalid].
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %v", ErrInvalid, fmt.Sprintf(format, args...))
}

// brokenf is an error that wraps [ErrLogBroken].
func brokenf(format string, args ...any) error {
	return fmt.Errorf("%w: %v", ErrLogBroken, fmt.Sprintf(format, args...))
}
