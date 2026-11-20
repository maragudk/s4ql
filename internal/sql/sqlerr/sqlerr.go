// Package sqlerr holds the one type of SQLite's errors as s4ql gives them: an error that SQLite
// gives for the same statement, with SQLite's result code, extended result code, and message, all
// three exactly as SQLite gives them.
//
// The type is for errors that s4ql reproduces. It is never an error that the SQLite library itself
// returned at run time, which is an error of another kind.
package sqlerr

// The result codes that s4ql gives, from https://sqlite.org/rescode.html. An extended result code
// whose low eight bits are a primary code is that primary code with more detail; a primary code is
// also an extended code of its own.
const (
	// CodeError is SQLITE_ERROR, the code of a syntax error and of an error about the schema.
	CodeError = 1
	// CodeConstraint is SQLITE_CONSTRAINT, the primary code of a constraint that a statement broke.
	CodeConstraint = 19
	// CodeConstraintPrimaryKey is SQLITE_CONSTRAINT_PRIMARYKEY, the extended code of a primary key
	// that a statement would have twice.
	CodeConstraintPrimaryKey = CodeConstraint | 6<<8
	// CodeConstraintNotNull is SQLITE_CONSTRAINT_NOTNULL, the extended code of a column of NOT NULL
	// that a statement would leave without a value.
	CodeConstraintNotNull = CodeConstraint | 5<<8
	// CodeReadOnly is SQLITE_READONLY, the code of a write that the connection may not make, such as
	// one under `pragma query_only`.
	CodeReadOnly = 8
	// CodeTooBig is SQLITE_TOOBIG, the code of a text or a blob, or a row, that is longer than SQLite's
	// limit.
	CodeTooBig = 18
)

// Error of SQLite, with its result codes.
type Error struct {
	// Code is SQLite's primary result code.
	Code int

	// ExtendedCode is SQLite's extended result code.
	ExtendedCode int

	// Message of SQLite, such as `near "frm": syntax error` or `no such table: t1`.
	Message string

	// Offset in bytes from the start of the text of the token that the error is about, as SQLite
	// gives it, or -1 where it is not known. It is extra information: SQLite does not give the same
	// offset on every connection, and s4ql does not promise it.
	Offset int
}

// Error gives SQLite's message.
func (e *Error) Error() string {
	return e.Message
}
