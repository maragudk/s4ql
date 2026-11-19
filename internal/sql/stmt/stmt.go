// Package stmt is the pass after the parser: it reads SQL text into a statement that s4ql supports,
// or says why it cannot.
//
// [Check] gives one of four answers for a text:
//
//   - SQLite's syntax error, as a [*sqlerr.Error] with the result codes 1 and 1, when the text is a
//     syntax error in SQLite, also when it is SQL that s4ql does not support.
//   - "Not supported", as a [*NotSupportedError] that matches [ErrNotSupported], when SQLite's parser
//     accepts the text and it is not, token for token, a statement that s4ql supports. Text that is
//     not valid UTF-8, or that has a NUL byte, is not supported either, and is rejected before it is
//     parsed.
//   - A supported statement: a [*CreateTable], an [*Insert], a [*Select], or a [*DropTable]. Its
//     [Statement.Parameters] says how many arguments it takes.
//   - No statement, for a text of only spaces, comments, and semicolons, which SQLite prepares into no
//     statement and no error.
//
// The supported statements are those of slices 1 and 2 of docs/roadmap.md, with the column types and
// the literals of slice 4. The README next to this file defines them token for token, and lists the
// names that a "not supported" error gives.
//
// A statement is built only from what the pass has recognized. The pass reads the syntax tree of
// internal/sql/parser in the order of the text and takes each token that the supported form expects
// there, one after the other. A statement is supported only if every token of the tree was taken, so a
// node, a clause, or a keyword that the pass does not know makes the statement not supported without a
// check of its own.
package stmt

import (
	"errors"
	"fmt"
)

// ErrNotSupported is matched by every [*NotSupportedError], with [errors.Is].
var ErrNotSupported = errors.New("s4ql: not supported")

// NotSupportedError says what of a text that SQLite's parser accepts s4ql does not support, and where.
// It carries no result code of SQLite.
type NotSupportedError struct {
	// What is not supported: a short description from the vocabulary in the README, such as
	// "WHERE clause".
	What string

	// Offset in bytes from the start of the text: of the first token that is not part of a supported
	// statement, or of where a part that the statement lacks would go.
	Offset int
}

// Error gives "s4ql: not supported: " and what is not supported, with its offset.
func (e *NotSupportedError) Error() string {
	return fmt.Sprintf("s4ql: not supported: %v (at offset %v)", e.What, e.Offset)
}

// Is reports whether target is [ErrNotSupported].
func (e *NotSupportedError) Is(target error) bool {
	return target == ErrNotSupported
}

// Span of bytes [Start, End) of the text.
type Span struct {
	Start, End int
}

// Name of a table or a column as a statement writes it. A name is bare, or in double quotes with ""
// for a quote inside.
type Name struct {
	// Raw text of the name as written, with its quotes. SQLite prints it so in `table "t1" already
	// exists`.
	Raw string

	// Value of the name without its quotes: what SQLite stores, compares, and prints in `no such
	// table: t1`, `table t1 has no column named v2`, and, for the names that CREATE TABLE stores,
	// `UNIQUE constraint failed: t1.v1`.
	Value string

	// Offset of the name's token in the text.
	Offset int
}

// Fold of the name: its value with the ASCII letters in lower case. SQLite takes two names for the
// same name when their folds are equal; it folds no other letter, so `ÆØÅ` and `æøå` are two names.
func (n Name) Fold() string {
	return asciiLower(n.Value)
}

// asciiLower of s: s with A to Z in lower case and every other byte as it is.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// Statement that s4ql supports: a [*CreateTable], an [*Insert], a [*Select], or a [*DropTable].
type Statement interface {
	// Bounds of the statement in the text, from its first token to the end of its semicolon, or of
	// its last token if it has none.
	Bounds() Span

	// Parameters of the statement: how many arguments it takes, as sqlite3_bind_parameter_count
	// gives it. Every parameter is a plain ?, so this is the number of them.
	Parameters() int

	setSpan(Span)
}

// CreateTable of one table, with one column of the type Type that is its primary key. The table is
// STRICT and WITHOUT ROWID.
type CreateTable struct {
	Span   Span
	Table  Name
	Column Name
	// Type of the column as it is declared: INT and INTEGER are two types here, as SQLite records
	// them, though they behave the same.
	Type Type
}

// Type of a column, as a STRICT table of SQLite declares it.
type Type int

// The types of a column. The zero value is no type.
const (
	TypeInt Type = iota + 1
	TypeInteger
	TypeReal
	TypeText
	TypeBlob
)

// String of the type as SQLite records it, in upper case, such as INT, or "" for no type.
func (t Type) String() string {
	switch t {
	case TypeInt:
		return "INT"
	case TypeInteger:
		return "INTEGER"
	case TypeReal:
		return "REAL"
	case TypeText:
		return "TEXT"
	case TypeBlob:
		return "BLOB"
	}
	return ""
}

// Value of a literal: an [Integer], a [Real], a [Text], or a [Blob], each with the value that SQLite
// gives the literal.
type Value interface {
	isValue()
}

// Integer value, of a decimal integer literal that fits a signed 64-bit integer.
type Integer int64

// Real value, of a real literal, as SQLite reads it: bit for bit the same 64-bit float, negative zero
// included. It is never infinite and never NaN.
type Real float64

// Text value, of a string literal: the bytes between its quotes, with two quotes read as one. It is
// valid UTF-8 without a NUL byte, as the text of every statement is.
type Text string

// Blob value, of a blob literal: the bytes that its hex digits give. It is never nil, also when it is
// empty.
type Blob []byte

func (Integer) isValue() {}
func (Real) isValue()    {}
func (Text) isValue()    {}
func (Blob) isValue()    {}

// Insert of one row with one value, into the one column that it names. The value is the literal
// Value, or the argument of a parameter if Parameter is not 0.
type Insert struct {
	Span   Span
	Table  Name
	Column Name
	// Value of the literal, and nil for a parameter.
	Value Value
	// Parameter is the number of the parameter that gives the value, from 1 as SQLite counts them, or
	// 0 if the value is a literal.
	Parameter int
	// ValueSpan of the value in the text, its sign included, or of the parameter.
	ValueSpan Span
}

// Select of every column and every row of one table.
type Select struct {
	Span  Span
	Table Name
}

// DropTable of one table.
type DropTable struct {
	Span  Span
	Table Name
}

// Bounds of the statement in the text.
func (s *CreateTable) Bounds() Span { return s.Span }

// Bounds of the statement in the text.
func (s *Insert) Bounds() Span { return s.Span }

// Bounds of the statement in the text.
func (s *Select) Bounds() Span { return s.Span }

// Bounds of the statement in the text.
func (s *DropTable) Bounds() Span { return s.Span }

// Parameters of the statement, which has none.
func (s *CreateTable) Parameters() int { return 0 }

// Parameters of the statement: 1 if its value is a parameter, and 0 otherwise.
func (s *Insert) Parameters() int {
	if s.Parameter > 0 {
		return 1
	}
	return 0
}

// Parameters of the statement, which has none.
func (s *Select) Parameters() int { return 0 }

// Parameters of the statement, which has none.
func (s *DropTable) Parameters() int { return 0 }

func (s *CreateTable) setSpan(sp Span) { s.Span = sp }
func (s *Insert) setSpan(sp Span)      { s.Span = sp }
func (s *Select) setSpan(sp Span)      { s.Span = sp }
func (s *DropTable) setSpan(sp Span)   { s.Span = sp }
