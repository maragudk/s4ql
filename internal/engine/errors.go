package engine

import (
	"errors"
	"fmt"

	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
)

var (
	// ErrNoStatement is the error of a text that holds no statement: one that is empty or holds only
	// spaces, comments, and semicolons. SQLite prepares such a text into no statement and gives no
	// error, so this error is not SQLite's, and is not a failure of the text either.
	ErrNoStatement = errors.New("s4ql: the text holds no statement")

	// ErrInternal is matched by the error of a fault of s4ql: something happened that the engine
	// rules out, such as a refusal of the storage core for a table that the engine had checked. It
	// is never SQLite's error.
	ErrInternal = errors.New("s4ql: internal error")

	// ErrArguments is matched by the error of a statement that is given another number of arguments
	// than it has parameters. Its message says both numbers. SQLite binds NULL to a parameter that is
	// given no argument, and refuses an argument for a parameter that the statement does not have;
	// s4ql refuses both, and runs nothing. It is not SQLite's error, and not "not supported".
	ErrArguments = errors.New("s4ql: wrong number of arguments")

	// ErrTooLarge is matched by the error of a commit whose commit object is larger than the store
	// takes in one write. Nothing of the transaction is committed, and the same changes would give a
	// commit object of the same size again. The error also matches [core.ErrTooLarge].
	ErrTooLarge = errors.New("s4ql: the transaction is too large to commit")

	// ErrRowsOpen is the error of a statement in a transaction that spans statements, and of a begin
	// of such a transaction, on a connection whose rows of an earlier query have not ended; and of a
	// commit of a write transaction while a loop over the rows of one of its queries runs, which then
	// rolls the transaction back. A connection runs one query at a time.
	ErrRowsOpen = errors.New("s4ql: the rows of an earlier query of the connection are open")

	// ErrTxOpen is the error of a begin on a connection whose transaction is open.
	ErrTxOpen = errors.New("s4ql: a transaction is open on the connection")

	// ErrNoTx is the error of a commit or a roll back on a connection without a transaction.
	ErrNoTx = errors.New("s4ql: no transaction is open on the connection")

	// ErrTxDone is the last error of rows whose transaction ended while a loop over them ran: the
	// loop ends at its next row with it, and reads nothing more.
	ErrTxDone = errors.New("s4ql: the transaction of the rows has ended")

	// errPanicked is the error in the event of a statement that panicked in the engine. The panic goes
	// on to the caller, and the error is only in the event.
	errPanicked = fmt.Errorf("%w: the statement panicked", ErrInternal)
)

// checkTypes of the statement s, which [stmt.Check] supports: "not supported" for a column of
// another type than INT, and for a literal that is not an integer. The engine holds integers only,
// so far. No value of a literal is in an error.
func checkTypes(s stmt.Statement) error {
	switch s := s.(type) {
	case *stmt.CreateTable:
		if s.Type != stmt.TypeInt {
			return fmt.Errorf("%w: column type %v", stmt.ErrNotSupported, s.Type)
		}
	case *stmt.Insert:
		switch s.Value.(type) {
		case stmt.Integer, nil:
		case stmt.Real:
			return fmt.Errorf("%w: a real literal, which is not an integer", stmt.ErrNotSupported)
		case stmt.Text:
			return fmt.Errorf("%w: a string literal, which is not an integer", stmt.ErrNotSupported)
		case stmt.Blob:
			return fmt.Errorf("%w: a blob literal, which is not an integer", stmt.ErrNotSupported)
		}
	}
	return nil
}

// checkArguments of a statement with the given number of parameters: the error of [countArguments],
// and then "not supported" for the first argument that is not an integer. No value of an argument
// is in an error.
func checkArguments(parameters int, args []*s4qlpb.Value) error {
	if err := countArguments(parameters, len(args)); err != nil {
		return err
	}
	for i, a := range args {
		if !a.HasInteger() {
			return fmt.Errorf("%w: argument %v, which is not an integer", stmt.ErrNotSupported, i+1)
		}
	}
	return nil
}

// countArguments: [ErrArguments] if n arguments are not one for each of the given number of
// parameters, with both numbers in its message, and nil otherwise.
func countArguments(parameters, n int) error {
	if n == parameters {
		return nil
	}
	noun := "parameters"
	if parameters == 1 {
		noun = "parameter"
	}
	return fmt.Errorf("%w: %v for a statement with %v %v", ErrArguments, n, parameters, noun)
}

// internalError that matches [ErrInternal], and wraps nothing else.
func internalError(format string, args ...any) error {
	return fmt.Errorf("%w: %v", ErrInternal, fmt.Sprintf(format, args...))
}

// The errors of SQLite that the statements of slice 1 can give, with the names in the form that
// SQLite prints in each.

// tableExists is `table %s already exists`, with the name of the table as the statement writes it,
// quotes and all, as SQLite prints its token.
func tableExists(raw string, offset int) error {
	return &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError, Message: fmt.Sprintf("table %v already exists", raw), Offset: offset}
}

// noSuchTable is `no such table: %s`, with the value of the name of the table that the statement
// writes.
func noSuchTable(value string) error {
	return &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError, Message: "no such table: " + value, Offset: -1}
}

// noSuchColumn is `table %s has no column named %s`, with the values of the names that the statement
// writes.
func noSuchColumn(table, column string) error {
	return &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError, Message: fmt.Sprintf("table %v has no column named %v", table, column), Offset: -1}
}

// uniqueFailed is `UNIQUE constraint failed: %s.%s`, for a primary key of one column, with the names
// as the table stores them.
func uniqueFailed(table, column string) error {
	return &sqlerr.Error{Code: sqlerr.CodeConstraint, ExtendedCode: sqlerr.CodeConstraintPrimaryKey,
		Message: "UNIQUE constraint failed: " + table + "." + column, Offset: -1}
}

// readOnlyDatabase is `attempt to write a readonly database`, which SQLite gives for a statement that
// would write on a connection under `pragma query_only = 1`, when it steps the statement.
func readOnlyDatabase() error {
	return &sqlerr.Error{Code: sqlerr.CodeReadOnly, ExtendedCode: sqlerr.CodeReadOnly, Message: "attempt to write a readonly database", Offset: -1}
}
