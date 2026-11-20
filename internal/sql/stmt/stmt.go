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
//   - A supported statement: a [*CreateTable], an [*Insert], a [*Select], an [*Update], a [*Delete],
//     a [*DropTable], a [*CreateIndex], or a [*DropIndex]. Its [Statement.Parameters] says how many
//     arguments it takes.
//   - No statement, for a text of only spaces, comments, and semicolons, which SQLite prepares into no
//     statement and no error.
//
// The supported statements are those of slices 1 and 2 of docs/roadmap.md, with the column types and
// the literals of slice 4, the columns of slice 5, the NULL of slice 6, the WHERE of slice 7, the
// UPDATE and DELETE of slice 8, and the CREATE INDEX and DROP INDEX of slice 9. The README next to
// this file defines them token for token, and lists the names that a "not supported" error gives.
//
// Names compare as SQLite compares them, by [Name.Fold]. What a statement means for a table, such as
// whether its columns exist, its values are as many as the columns, or a value has the type of its
// column, is not decided here, since it depends on the database.
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

	"maragu.dev/s4ql/internal/sql/sqlerr"
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

// Statement that s4ql supports: a [*CreateTable], an [*Insert], a [*Select], an [*Update], a [*Delete],
// a [*DropTable], a [*CreateIndex], or a [*DropIndex].
type Statement interface {
	// Bounds of the statement in the text, from its first token to the end of its semicolon, or of
	// its last token if it has none.
	Bounds() Span

	// Parameters of the statement: how many arguments it takes, as sqlite3_bind_parameter_count
	// gives it. Every parameter is a plain ?, so this is the number of them.
	Parameters() int

	setSpan(Span)
}

// CreateTable of one table, with one or more columns, of which exactly one is its primary key. The
// table is STRICT and WITHOUT ROWID. A column that is not the primary key takes NULL unless it is
// declared NOT NULL.
type CreateTable struct {
	Span  Span
	Table Name
	// Columns of the table, in the order of the text. Two of them may have one name, as
	// [Name.Fold] compares names, and there may be more than SQLite allows: SQLite refuses both
	// once it has found that no table of the name exists, with the error of
	// [CreateTable.ColumnsError].
	Columns []Column
	// SQL is the text that SQLite keeps for the table in sqlite_schema: "CREATE TABLE ", and the
	// text from the name of the table up to the semicolon of the statement, or to the end of the
	// text if it has none, with its spaces and comments.
	SQL string
}

// Column of a table, as CREATE TABLE declares it.
type Column struct {
	Name Name
	// Type of the column as it is declared: INT and INTEGER are two types here, as SQLite records
	// them, though they behave the same.
	Type Type
	// PrimaryKey reports whether the column is the primary key of its table.
	PrimaryKey bool
	// NotNull reports whether the column is declared NOT NULL. A column that is not the primary key
	// and is not declared NOT NULL takes NULL. The primary key may be declared NOT NULL too, which
	// changes nothing: the primary key of a WITHOUT ROWID table never takes NULL.
	NotNull bool
}

// maxColumns of a table in SQLite, SQLITE_MAX_COLUMN, which the binding in internal/sqlite keeps at
// its default.
const maxColumns = 2000

// Key of the table: the index in Columns of its primary key.
func (s *CreateTable) Key() int {
	for i, c := range s.Columns {
		if c.PrimaryKey {
			return i
		}
	}
	panic("stmt: a table without a primary key")
}

// ColumnsError of the statement: the error that SQLite gives for its columns once it has found that
// no table of the name exists, or nil. SQLite adds the columns in order, and refuses the first column
// after the 2000th with `too many columns on t1`, and before that the first column whose name an
// earlier column has, as [Name.Fold] compares them, with `duplicate column name: v1`, with the name
// of the later column. Each prints the value of the name, and has the offset -1, as SQLite gives
// for both. When a table of the name exists, SQLite gives `table t1 already exists` instead, with
// the name as written.
func (s *CreateTable) ColumnsError() *sqlerr.Error {
	seen := map[string]bool{}
	for i, c := range s.Columns {
		message := ""
		switch {
		case i == maxColumns:
			message = "too many columns on " + s.Table.Value
		case seen[c.Name.Fold()]:
			message = "duplicate column name: " + c.Name.Value
		}
		if message != "" {
			return &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError, Message: message, Offset: -1}
		}
		seen[c.Name.Fold()] = true
	}
	return nil
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

// Value of a literal: an [Integer], a [Real], a [Text], a [Blob], or [Null], each with the value that
// SQLite gives the literal.
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

// Null value, of the literal NULL in any case of its letters. It is a value of its own, so that the
// [Term] of a NULL, whose Value is Null, is never taken for that of a parameter, whose Value is nil.
type Null struct{}

func (Integer) isValue() {}
func (Real) isValue()    {}
func (Text) isValue()    {}
func (Blob) isValue()    {}
func (Null) isValue()    {}

// Insert of one row. Without a list of columns, the values are for the columns of the table, in the
// order of the table.
type Insert struct {
	Span  Span
	Table Name
	// Columns of the list of columns, in the order of the text, or nil if the statement has none. No
	// two of them have one name, as [Name.Fold] compares names.
	Columns []Name
	// Values of the row, in the order of the text. Their number may differ from that of the columns,
	// which SQLite refuses once it has found the table.
	Values []Term
}

// Term of a row, of an [Assignment], or of a [Condition]: the literal Value, or the argument of a parameter if Parameter
// is not 0.
type Term struct {
	// Value of the literal, and nil for a parameter. The literal NULL has the value [Null], never nil.
	Value Value
	// Parameter is the number of the parameter that gives the value, from 1 as SQLite counts them, or
	// 0 if the value is a literal. Every parameter is a plain ?, which SQLite numbers one more than
	// the one before it.
	Parameter int
	// Span of the value in the text, its sign included, or of the parameter.
	Span Span
}

// Select of the rows of one table that meet the conditions of its WHERE clause, or of every row, with
// the columns that it names, or every column.
type Select struct {
	Span  Span
	Table Name
	// Columns that the select list names, in the order of the text, each as often as the list names
	// it, or nil for *, which is every column in the order of the table.
	Columns []Name
	// Where holds the conditions of the WHERE clause, in the order of the text, which are joined by
	// AND, so that a row is read only if it meets every one, or nil if the statement has no WHERE.
	// Two of them may test one column.
	Where []Condition
}

// Condition of a WHERE clause of a select, an update, or a delete: a column compared with a value,
// or tested for NULL.
type Condition struct {
	// Column that the condition tests, which is on the left of its operator. Whether the table has it
	// is not decided here.
	Column Name
	// Operator of the condition.
	Operator Operator
	// Value that the column is compared with, a literal or a parameter, as a value of an insert is.
	// For [OpIsNull] and [OpIsNotNull] it is the zero Term, which is neither.
	Value Term
}

// Operator of a [Condition]. The zero value is no operator.
type Operator int

// The operators of a condition. SQLite reads = and == as one operator, and != and <> as one.
const (
	OpEqual Operator = iota + 1
	OpNotEqual
	OpLess
	OpLessOrEqual
	OpGreater
	OpGreaterOrEqual
	OpIsNull
	OpIsNotNull
)

// String of the operator as SQL writes it, such as = or IS NOT NULL, or "" for no operator.
func (o Operator) String() string {
	switch o {
	case OpEqual:
		return "="
	case OpNotEqual:
		return "!="
	case OpLess:
		return "<"
	case OpLessOrEqual:
		return "<="
	case OpGreater:
		return ">"
	case OpGreaterOrEqual:
		return ">="
	case OpIsNull:
		return "IS NULL"
	case OpIsNotNull:
		return "IS NOT NULL"
	}
	return ""
}

// Update of the rows of one table that meet the conditions of its WHERE clause, or of every row: each
// gets the values of the assignments of its SET clause.
type Update struct {
	Span  Span
	Table Name
	// Set holds the assignments of the SET clause, in the order of the text, one or more. No two of
	// them have one column, as [Name.Fold] compares names. Whether the table has the columns is not
	// decided here.
	Set []Assignment
	// Where holds the conditions of the WHERE clause, in the order of the text, as for a [Select], or
	// nil if the statement has no WHERE.
	Where []Condition
}

// Assignment of a SET clause: a column and the value that it gets.
type Assignment struct {
	// Column that gets the value.
	Column Name
	// Value of the column, a literal or a parameter, as a value of an insert is.
	Value Term
}

// Delete of the rows of one table that meet the conditions of its WHERE clause, or of every row.
type Delete struct {
	Span  Span
	Table Name
	// Where holds the conditions of the WHERE clause, in the order of the text, as for a [Select], or
	// nil if the statement has no WHERE.
	Where []Condition
}

// DropTable of one table.
type DropTable struct {
	Span  Span
	Table Name
}

// CreateIndex of one index, which is not unique, on one table, by one or more of its columns.
type CreateIndex struct {
	Span Span
	// Index is the name of the index.
	Index Name
	// Table is the name of the table, after ON.
	Table Name
	// Columns of the index, in the order of the text, one or more and at most as many as SQLite
	// allows in an index. No two of them have one name, as [Name.Fold] compares names. Whether the
	// table has them is not decided here.
	Columns []IndexColumn
}

// IndexColumn of a [CreateIndex]: a column of the table and its order in the index.
type IndexColumn struct {
	// Name of the column.
	Name Name
	// Descending reports whether the column says DESC. A column that says ASC, or neither, is in
	// ascending order.
	Descending bool
}

// DropIndex of one index.
type DropIndex struct {
	Span  Span
	Index Name
}

// Bounds of the statement in the text.
func (s *CreateTable) Bounds() Span { return s.Span }

// Bounds of the statement in the text.
func (s *Insert) Bounds() Span { return s.Span }

// Bounds of the statement in the text.
func (s *Select) Bounds() Span { return s.Span }

// Bounds of the statement in the text.
func (s *Update) Bounds() Span { return s.Span }

// Bounds of the statement in the text.
func (s *Delete) Bounds() Span { return s.Span }

// Bounds of the statement in the text.
func (s *DropTable) Bounds() Span { return s.Span }

// Bounds of the statement in the text.
func (s *CreateIndex) Bounds() Span { return s.Span }

// Bounds of the statement in the text.
func (s *DropIndex) Bounds() Span { return s.Span }

// Parameters of the statement, which has none.
func (s *CreateTable) Parameters() int { return 0 }

// Parameters of the statement: the number of its values that are a parameter.
func (s *Insert) Parameters() int {
	n := 0
	for _, v := range s.Values {
		if v.Parameter > 0 {
			n++
		}
	}
	return n
}

// Parameters of the statement: the number of the values of its conditions that are a parameter.
func (s *Select) Parameters() int {
	return conditionParameters(s.Where)
}

// Parameters of the statement: the number of the values of its assignments and of its conditions
// that are a parameter. Those of the assignments come first, as SQLite numbers them in the order of
// the text.
func (s *Update) Parameters() int {
	n := 0
	for _, a := range s.Set {
		if a.Value.Parameter > 0 {
			n++
		}
	}
	return n + conditionParameters(s.Where)
}

// Parameters of the statement: the number of the values of its conditions that are a parameter.
func (s *Delete) Parameters() int {
	return conditionParameters(s.Where)
}

// conditionParameters of the conditions where: the number of their values that are a parameter.
func conditionParameters(where []Condition) int {
	n := 0
	for _, c := range where {
		if c.Value.Parameter > 0 {
			n++
		}
	}
	return n
}

// Parameters of the statement, which has none.
func (s *DropTable) Parameters() int { return 0 }

// Parameters of the statement, which has none.
func (s *CreateIndex) Parameters() int { return 0 }

// Parameters of the statement, which has none.
func (s *DropIndex) Parameters() int { return 0 }

func (s *CreateTable) setSpan(sp Span) { s.Span = sp }
func (s *Insert) setSpan(sp Span)      { s.Span = sp }
func (s *Select) setSpan(sp Span)      { s.Span = sp }
func (s *Update) setSpan(sp Span)      { s.Span = sp }
func (s *Delete) setSpan(sp Span)      { s.Span = sp }
func (s *DropTable) setSpan(sp Span)   { s.Span = sp }
func (s *CreateIndex) setSpan(sp Span) { s.Span = sp }
func (s *DropIndex) setSpan(sp Span)   { s.Span = sp }
