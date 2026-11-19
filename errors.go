package s4ql

import (
	"errors"
	"strings"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// Error of SQLite: the error that SQLite gives for the same statement, with SQLite's result code,
// extended result code, and message. An error of a statement is one if [errors.As] finds a *Error
// in it. The codes are listed at https://sqlite.org/rescode.html.
//
// It is never an error of s4ql's own: "not supported", and the errors of the instance, carry no
// result code.
type Error struct {
	// Code is SQLite's primary result code, such as 19 for SQLITE_CONSTRAINT.
	Code int

	// ExtendedCode is SQLite's extended result code, such as 1555 for SQLITE_CONSTRAINT_PRIMARYKEY.
	ExtendedCode int

	// Message of SQLite, such as `no such table: t1`.
	Message string
}

// Error gives SQLite's message.
func (e *Error) Error() string {
	return e.Message
}

// The errors of s4ql's own, which an error of the driver matches with [errors.Is]. An error matches
// at most one of them. Its message says more, such as what is not supported or why the instance
// stopped.
var (
	// ErrNotSupported is matched by the error of SQL that SQLite accepts and s4ql does not support,
	// and of a call of database/sql that s4ql does not support. The message starts with
	// "s4ql: not supported: " and names what is not supported. The statement was not run.
	ErrNotSupported = errors.New("s4ql: not supported")

	// ErrNotServing is matched by the error of a statement that the instance refused because it
	// does not hold a valid lease of the database now: it has just started, another instance holds
	// the lease, or the object store cannot be reached. The statement is not committed. The error is
	// temporary: the instance keeps trying to get the lease, and [database/sql.DB.PingContext] waits
	// until it serves.
	ErrNotServing = errors.New("s4ql: the instance does not serve now")

	// ErrOutcomeUnknown is matched by the error of a statement whose context ended before the
	// instance knew whether its commit was applied, and of one whose commit the instance has to look
	// for in a log that it cannot read. The statement may be committed, may not be, and may still
	// become committed. It is the one error of a statement that does not mean "not committed", so a
	// statement that is run again after it can be applied twice. The instance serves nothing until
	// it has found out.
	ErrOutcomeUnknown = errors.New("s4ql: the outcome of the commit is unknown")

	// ErrStopped is matched by the error of every statement that reaches an instance that has
	// stopped for good, and of every ping of it: its cache failed, it met something in the object
	// store that it cannot read, or it was closed. The message carries the cause. Nothing brings the
	// instance back: the application closes the [database/sql.DB] and opens the database again.
	ErrStopped = errors.New("s4ql: the instance has stopped")
)

// failure is an error of s4ql's own: one that matches is with [errors.Is], if is is not nil, and
// that unwraps to its cause, if it has one.
type failure struct {
	is      error
	message string
	cause   error
}

func (f *failure) Error() string {
	return f.message
}

func (f *failure) Is(target error) bool {
	return f.is != nil && target == f.is
}

func (f *failure) Unwrap() error {
	return f.cause
}

// notSupported error of what, which names what is not supported.
func notSupported(what string) error {
	return &failure{is: ErrNotSupported, message: "s4ql: not supported: " + what}
}

// publicError of err, an error of the engine or of the storage core, or nil: SQLite's error as an
// [*Error], and every other error as one whose message starts with "s4ql: " and which matches the
// public error for its state, if there is one.
//
// No error that it gives is or wraps [database/sql/driver.ErrBadConn], because nothing below the
// driver gives that error. database/sql runs a statement again when a driver returns it, and a
// write whose outcome is unknown must never run again by itself.
func publicError(err error) error {
	if err == nil {
		return nil
	}
	if se, ok := errors.AsType[*sqlerr.Error](err); ok {
		return &Error{Code: se.Code, ExtendedCode: se.ExtendedCode, Message: se.Message}
	}
	if ns, ok := errors.AsType[*stmt.NotSupportedError](err); ok {
		// The message of the error itself, which starts with "s4ql: not supported: ".
		return &failure{is: ErrNotSupported, message: ns.Error(), cause: err}
	}
	if errors.Is(err, stmt.ErrNotSupported) {
		// An argument that the engine does not support, whose message starts with "s4ql: not
		// supported: ".
		return &failure{is: ErrNotSupported, message: err.Error(), cause: err}
	}
	if errors.Is(err, engine.ErrNoStatement) {
		return &failure{is: ErrNotSupported, message: "s4ql: not supported: a text that holds no statement", cause: err}
	}
	if errors.Is(err, core.ErrReaderBusy) {
		// A connection runs one read at a time, on its one reader.
		return &failure{is: ErrNotSupported, message: "s4ql: not supported: a statement that reads on a connection whose rows of an earlier query are open", cause: err}
	}

	f := &failure{message: err.Error(), cause: err}
	if !strings.HasPrefix(f.message, "s4ql: ") {
		f.message = "s4ql: " + f.message
	}
	switch {
	case errors.Is(err, core.ErrOutcomeUnknown):
		f.is = ErrOutcomeUnknown
	case errors.Is(err, core.ErrStopped):
		f.is = ErrStopped
	case errors.Is(err, core.ErrNoLease):
		f.is = ErrNotServing
	}
	return f
}
