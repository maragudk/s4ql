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

	// errPanicked is the error in the event of a statement that panicked in the engine. The panic goes
	// on to the caller, and the error is only in the event.
	errPanicked = fmt.Errorf("%w: the statement panicked", ErrInternal)
)

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
