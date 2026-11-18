package engine

import (
	"errors"
	"fmt"
	"strings"

	"maragu.dev/s4ql/internal/sql/sqlerr"
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

	// errPanicked is the error in the event of a statement that panicked in the engine. The panic goes
	// on to the caller, and the error is only in the event.
	errPanicked = fmt.Errorf("%w: the statement panicked", ErrInternal)
)

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

// uniqueFailed is `UNIQUE constraint failed: %s.%s`, for each column of the primary key, with the
// names as the table stores them.
func uniqueFailed(table string, columns []string) error {
	terms := make([]string, len(columns))
	for i, c := range columns {
		terms[i] = table + "." + c
	}
	return &sqlerr.Error{Code: sqlerr.CodeConstraint, ExtendedCode: sqlerr.CodeConstraintPrimaryKey,
		Message: "UNIQUE constraint failed: " + strings.Join(terms, ", "), Offset: -1}
}
