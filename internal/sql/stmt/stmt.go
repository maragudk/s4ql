// Package stmt is the pass after the parser: it reads SQL text into a statement that s4ql supports,
// or says why it cannot.
//
// [Check] gives one of four answers for a text:
//
//   - SQLite's syntax error, as a [*SyntaxError], when the text is a syntax error in SQLite, also when
//     it is SQL that s4ql does not support.
//   - "Not supported", as a [*NotSupportedError] that matches [ErrNotSupported], when SQLite's parser
//     accepts the text and it is not, token for token, a statement that s4ql supports. Text that is
//     not valid UTF-8, or that has a NUL byte, is not supported either, and is rejected before it is
//     parsed.
//   - A supported statement: a [*CreateTable], an [*Insert], a [*Select], or a [*DropTable].
//   - No statement, for a text of only spaces, comments, and semicolons, which SQLite prepares into no
//     statement and no error.
//
// The supported statements are those of slice 1 of docs/roadmap.md. The README next to this file
// defines them token for token, and lists the names that a "not supported" error gives.
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

// SyntaxError of SQLite: one of the three messages of its parser, with its result codes.
type SyntaxError struct {
	// Code is SQLite's primary result code, which is always 1, SQLITE_ERROR.
	Code int

	// ExtendedCode is SQLite's extended result code, which is always 1 as well.
	ExtendedCode int

	// Message of SQLite, such as `near "frm": syntax error`.
	Message string

	// Offset of the error in bytes from the start of the text, as SQLite gives it, or -1 where it gives
	// none. It is extra information: SQLite does not give the same offset on every connection.
	Offset int
}

// Error gives SQLite's message.
func (e *SyntaxError) Error() string {
	return e.Message
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
	setSpan(Span)
}

// CreateTable creates a table with one column, of type INT, that is its primary key. The table is
// STRICT and WITHOUT ROWID.
type CreateTable struct {
	Span   Span
	Table  Name
	Column Name
}

// Insert inserts one row with one value, into the one column that it names.
type Insert struct {
	Span   Span
	Table  Name
	Column Name
	Value  int64
	// ValueSpan of the value in the text, its sign included.
	ValueSpan Span
}

// Select reads every column and every row of one table.
type Select struct {
	Span  Span
	Table Name
}

// DropTable drops one table.
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

func (s *CreateTable) setSpan(sp Span) { s.Span = sp }
func (s *Insert) setSpan(sp Span)      { s.Span = sp }
func (s *Select) setSpan(sp Span)      { s.Span = sp }
func (s *DropTable) setSpan(sp Span)   { s.Span = sp }
