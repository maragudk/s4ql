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
// UPDATE and DELETE of slice 8, the CREATE INDEX and DROP INDEX of slice 9, the IF NOT EXISTS and
// IF EXISTS of slice 10, the DEFAULT of slice 11, the value on the left of a comparison of slice 12,
// the ORDER BY and LIMIT of a select of slice 13, the RETURNING of an insert, an update, and a
// delete of slice 14, and the scalar subquery in a WHERE of slice 15. The README next to this file
// defines them token for token, and lists the names that a "not supported" error gives.
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
// declared NOT NULL. A column may have a DEFAULT.
type CreateTable struct {
	Span Span
	// IfNotExists reports whether the statement says IF NOT EXISTS. With it, SQLite does nothing and
	// gives no error when a table of the name exists, whatever its columns are: not the error of
	// [CreateTable.ColumnsError] either. The errors that come before SQLite looks for the table, such
	// as `object name reserved for internal use`, stay, and so does every error of the plain
	// statement when no table has the name, such as `there is already an index named t1`.
	IfNotExists bool
	Table       Name
	// Columns of the table, in the order of the text. Two of them may have one name, as
	// [Name.Fold] compares names, and there may be more than SQLite allows: SQLite refuses both
	// once it has found that no table of the name exists, with the error of
	// [CreateTable.ColumnsError].
	Columns []Column
	// SQL is the text that SQLite keeps for the table in sqlite_schema: "CREATE TABLE ", and the
	// text from the name of the table up to the semicolon of the statement, or to the end of the
	// text if it has none, with its spaces and comments. It never holds IF NOT EXISTS, which comes
	// before the name.
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
	// Default of the column, which an INSERT whose list of columns leaves the column out stores, or
	// the zero Default if the column says no DEFAULT.
	Default Default
}

// Default of a [Column]: the literal after DEFAULT. The literal has the type of its column, or is
// NULL, which a column of every type may say, also one that is NOT NULL or the primary key: SQLite
// takes such a declaration, and an insert that leaves the column out gives `NOT NULL constraint
// failed`, as it does for the column without the DEFAULT.
type Default struct {
	// Value of the literal, as for a [Term], or nil if the column says no DEFAULT. DEFAULT NULL has
	// the value [Null].
	Value Value
	// Span of the literal in the text, its sign included. Its text is what SQLite keeps as the
	// default of the column, which pragma_table_info gives as dflt_value.
	Span Span
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
	// Returning of the RETURNING clause, which gives the row that the statement inserts, with its
	// values as the table stores them, or the zero Returning if the statement has none.
	Returning Returning
}

// Returning clause of an [Insert], an [Update], or a [Delete]: the statement gives a row for each row
// that it inserts, updates, or deletes, with the columns that the clause names, as a select gives the
// columns of its select list. An insert and an update give the values of the row after the change,
// and a delete the values of the row before it. The rows come in no order that the statement says.
type Returning struct {
	// Span of the clause in the text, from the keyword RETURNING to the end of its last column, or
	// the zero Span if the statement has no RETURNING; see [Returning.Present].
	Span Span
	// Columns that the clause names, in the order of the text, each as often as it names it, as the
	// select list of a [Select] does, or nil for *, which is every column in the order of the table.
	// Whether the table has them is not decided here.
	Columns []Name
}

// Present reports whether the statement has a RETURNING clause.
func (r Returning) Present() bool {
	return r.Span != Span{}
}

// Term of a row, of an [Assignment], or of a [Condition]: the literal Value, the argument of a
// parameter if Parameter is not 0, the value of a scalar subquery if Subquery is not 0, or the value
// of an arithmetic expression if Expression is present.
//
// A Term without an Expression is a plain value, as it was before expressions: a literal, with at
// most one - before a number, a plain ?, or a subquery. Only the value of an [Assignment] and the
// value of a comparison of a [Condition] can be an expression.
type Term struct {
	// Value of the literal, and nil for a parameter or a subquery. The literal NULL has the value
	// [Null], never nil.
	Value Value
	// Parameter is the number of the parameter that gives the value, from 1 as SQLite counts them, or
	// 0 if the value is a literal or a subquery. Every parameter is a plain ?, which SQLite numbers
	// one more than the one before it, in the order of the text, also inside a subquery.
	Parameter int
	// Subquery is the number of the scalar subquery that gives the value, from 1, in the Subqueries
	// of the statement, or 0 if the value is a literal or a parameter. Only the value of a condition
	// of the WHERE of a [Select], an [Update], or a [Delete] is one, never the value of a condition
	// of a subquery itself.
	Subquery int
	// Expression that gives the value, or the zero Expression for a plain value; see
	// [Expression.Present]. With one, Value is nil, Parameter 0, and Subquery 0. It is an operator,
	// or, as the value of an [Assignment] only, a column alone, as in `set a = b`.
	Expression Expression
	// Span of the value in the text, its sign included, or of the parameter, or of the subquery from
	// its opening parenthesis to the end of its closing one, or of the expression with the parentheses
	// around it.
	Span Span
}

// parametersOf the term t of a statement whose subqueries are subqueries: 1 for a parameter, those of
// its select for a subquery, those of its expression, and 0 for a literal and for the zero Term.
func parametersOf(t Term, subqueries []Select) int {
	switch {
	case t.Parameter > 0:
		return 1
	case t.Subquery > 0:
		return subqueries[t.Subquery-1].Parameters()
	}
	return t.Expression.parameters()
}

// Expression of arithmetic: a literal, a parameter, a column, or an operator with its operands, each
// an Expression. It holds its operands by value, in a slice, so that a statement holds no pointer and
// is compared and printed by value.
//
// The tree has SQLite's precedence and associativity: * and / bind more tightly than + and -, each
// groups to the left, and the - of a negation binds most tightly. Parentheses are not nodes: they
// group, and the Span of the parent holds them.
//
// A - directly before a number is part of the literal, as SQLite codes it, so `-5` is the value -5
// and never the negation of 5, and `-9223372036854775808` is the smallest integer of 64 bits. Every
// other - before an operand is [ExprNegate].
type Expression struct {
	// Kind of the node, or 0 for no expression.
	Kind ExpressionKind
	// Value of the literal of [ExprValue]: an [Integer] or a [Real], with its sign. Never a [Text],
	// a [Blob], or [Null], which arithmetic does not take. Nil for every other kind.
	Value Value
	// Parameter of [ExprParameter]: its number, from 1, numbered with every other parameter of the
	// statement in the order of the text. 0 for every other kind.
	Parameter int
	// Column of [ExprColumn]: its name, bare or in double quotes. Whether the table has it is not
	// decided here. The zero Name for every other kind.
	Column Name
	// Operands of an operator, in the order of the text: one for [ExprNegate], and two, the left and
	// the right, for [ExprAdd], [ExprSubtract], [ExprMultiply], and [ExprDivide]. Nil for a literal, a
	// parameter, and a column.
	Operands []Expression
	// Span of the node in the text, from its first token to the end of its last, without parentheses
	// around the node itself and with those around its operands. That of a literal holds its sign.
	Span Span
}

// ExpressionKind of a node of an [Expression]. The zero value is no expression.
type ExpressionKind int

// The kinds of a node of an [Expression].
const (
	// ExprValue is a literal: an integer or a real.
	ExprValue ExpressionKind = iota + 1
	// ExprParameter is a plain ?.
	ExprParameter
	// ExprColumn is the value of a column of the row.
	ExprColumn
	// ExprNegate is - before its one operand, which is no literal.
	ExprNegate
	// ExprAdd is +.
	ExprAdd
	// ExprSubtract is -.
	ExprSubtract
	// ExprMultiply is *.
	ExprMultiply
	// ExprDivide is /. Its right operand is never the literal 0 or 0.0, with or without a sign.
	ExprDivide
)

// String of the kind: the operator as SQL writes it, such as + or - for both a negation and a
// subtraction, or "value", "parameter", or "column", or "" for no kind.
func (k ExpressionKind) String() string {
	switch k {
	case ExprValue:
		return "value"
	case ExprParameter:
		return "parameter"
	case ExprColumn:
		return "column"
	case ExprNegate, ExprSubtract:
		return "-"
	case ExprAdd:
		return "+"
	case ExprMultiply:
		return "*"
	case ExprDivide:
		return "/"
	}
	return ""
}

// Present reports whether e is an expression, and not the zero Expression.
func (e Expression) Present() bool {
	return e.Kind != 0
}

// HasColumn reports whether a node of e is a column. An expression without a column has one value
// for each run of its statement, which its literals and its arguments decide, whatever the row.
func (e Expression) HasColumn() bool {
	if e.Kind == ExprColumn {
		return true
	}
	for _, o := range e.Operands {
		if o.HasColumn() {
			return true
		}
	}
	return false
}

// parameters of e: how many of its nodes are a parameter.
func (e Expression) parameters() int {
	n := 0
	if e.Kind == ExprParameter {
		n++
	}
	for _, o := range e.Operands {
		n += o.parameters()
	}
	return n
}

// Select of the rows of one table that meet the conditions of its WHERE clause, or of every row, with
// the columns that it names, or every column, in the order of its ORDER BY clause, and at most as
// many as its LIMIT clause says.
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
	// OrderBy holds the columns of the ORDER BY clause, in the order of the text, one or more, or nil
	// if the statement has no ORDER BY. The rows come in the order of the first column, those that
	// tie there in the order of the second, and so on, each column ordered as SQLite orders its
	// values: NULL before every other value in ascending order, and after every other in descending
	// order. The order of rows that tie on every column is unspecified. No two of the columns have
	// one name, as [Name.Fold] compares names, and a column need not be one that the select list
	// names. Whether the table has them is not decided here.
	OrderBy []OrderColumn
	// Limit of the LIMIT clause: the most rows that the select gives, as a literal [Integer] from 0
	// to the largest of 64 bits, or as a parameter, whose argument is taken after those of the
	// conditions. It is the zero Term, which is neither, if the statement has no LIMIT; see
	// [Select.Limited].
	Limit Term
	// Subqueries holds the scalar subqueries of the WHERE clause, in the order of the text, which the
	// values of its conditions name by their [Term.Subquery], or nil if it has none.
	//
	// The value of a condition whose Term.Subquery is k is the value that the k-th subquery gives:
	// the value of its one column in the first row that it gives, or NULL if it gives no row. SQLite
	// computes it once, from the rows as they are before the statement changes any, also when the
	// subquery reads the table that an update or a delete changes.
	//
	// A subquery is a Select of one table, with the WHERE, ORDER BY, and LIMIT of a select, and with
	// one result column: the name of a column, or *, which SQLite takes only for a table of one
	// column. Its conditions hold no subquery of their own, so its Subqueries is nil. Its parameters
	// are numbered with those of the statement, in the order of the text: those before the subquery
	// come before its own, and those after it after them. Its Span is the bytes from its SELECT to the
	// end of its last token, without the parentheses, which the Span of the Term holds.
	//
	// Whether a name of a subquery is one of its own table, or one of the table of the statement,
	// which SQLite takes it for when its own table does not have it, depends on the tables, so it is
	// not decided here.
	Subqueries []Select
}

// Limited reports whether the statement has a LIMIT clause.
func (s *Select) Limited() bool {
	return s.Limit.Value != nil || s.Limit.Parameter > 0
}

// OrderColumn of an ORDER BY: a column and its direction.
type OrderColumn struct {
	// Name of the column.
	Name Name
	// Descending reports whether the column says DESC. A column that says ASC, or neither, is in
	// ascending order.
	Descending bool
}

// Condition of a WHERE clause of a select, an update, or a delete: a column, or an arithmetic
// expression with a column, compared with a value, which may be a scalar subquery or an arithmetic
// expression, or a column tested for NULL.
//
// A condition always reads as Column, Operator, Value, whichever side of the operator the text
// writes the column on: `5 < a` is the condition `a > 5`, with ValueFirst set. SQLite gives the two
// the same rows, for every operator and every type, NULL included, so code that runs a condition can
// read every condition one way and never needs ValueFirst. The text's own order is kept for messages
// and spans: in ValueFirst, and in the offsets of Column and of Value.
//
// The side that the condition tests is the side with a column. Where both sides have one, it is the
// side that is a column alone, or the left side if neither is, so that Expression is present only
// where neither side is a column alone. The two sides of a comparison never are both a column alone,
// and never are both without a column.
type Condition struct {
	// Column that the condition tests, or the zero Name if Expression is present. Whether the table
	// has it is not decided here.
	Column Name
	// Expression that the condition tests in place of Column: an operator with a column, as in `a + 1
	// = 5`, or the zero Expression if the condition tests Column; see [Expression.Present].
	Expression Expression
	// Operator of the condition, as it compares Column with Value. When ValueFirst is set, the text
	// writes [Operator.Swapped] of it.
	Operator Operator
	// Value that the column is compared with, a literal or a parameter, as a value of an insert is,
	// or a scalar subquery, or an arithmetic expression, with a column or without one, on either side
	// of the column. For [OpIsNull] and [OpIsNotNull] it is the zero Term, which is none of them.
	Value Term
	// ValueFirst reports whether the text writes the value on the left of the operator and the column
	// on the right, as in `5 < a`. It is never set for [OpIsNull] and [OpIsNotNull], whose column is
	// always on the left.
	ValueFirst bool
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

// Swapped gives the operator that compares as o does with its two sides swapped: > for <, >= for
// <=, and = and != themselves. A test for NULL has one side, and gives no operator, as no operator
// gives.
func (o Operator) Swapped() Operator {
	switch o {
	case OpEqual, OpNotEqual:
		return o
	case OpLess:
		return OpGreater
	case OpLessOrEqual:
		return OpGreaterOrEqual
	case OpGreater:
		return OpLess
	case OpGreaterOrEqual:
		return OpLessOrEqual
	}
	return 0
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
	// Returning of the RETURNING clause, after the WHERE, which gives each row that the statement
	// updates, with the values of the row after the update, or the zero Returning if the statement has
	// none.
	Returning Returning
	// Subqueries holds the scalar subqueries of the WHERE clause, as for a [Select], or nil if it has
	// none. Their parameters come after those of the assignments.
	Subqueries []Select
}

// Assignment of a SET clause: a column and the value that it gets.
type Assignment struct {
	// Column that gets the value.
	Column Name
	// Value of the column, a literal or a parameter, as a value of an insert is, or an arithmetic
	// expression, which may be a column alone and may name the columns of the row before the update.
	Value Term
}

// Delete of the rows of one table that meet the conditions of its WHERE clause, or of every row.
type Delete struct {
	Span  Span
	Table Name
	// Where holds the conditions of the WHERE clause, in the order of the text, as for a [Select], or
	// nil if the statement has no WHERE.
	Where []Condition
	// Returning of the RETURNING clause, after the WHERE, which gives each row that the statement
	// deletes, with the values that the row had, or the zero Returning if the statement has none.
	Returning Returning
	// Subqueries holds the scalar subqueries of the WHERE clause, as for a [Select], or nil if it has
	// none.
	Subqueries []Select
}

// DropTable of one table.
type DropTable struct {
	Span Span
	// IfExists reports whether the statement says IF EXISTS. With it, SQLite does nothing and gives
	// no error when no table of the name exists, also when an index has the name.
	IfExists bool
	Table    Name
}

// CreateIndex of one index, which is not unique, on one table, by one or more of its columns.
type CreateIndex struct {
	Span Span
	// IfNotExists reports whether the statement says IF NOT EXISTS. With it, SQLite does nothing and
	// gives no error when an index of the name exists, on any table, whatever its columns are: not
	// `no such column` either. The errors that come before SQLite looks for the index stay: `no such
	// table` and `there is already a table named i`.
	IfNotExists bool
	// Index is the name of the index.
	Index Name
	// Table is the name of the table, after ON.
	Table Name
	// Columns of the index, in the order of the text, one or more and at most as many as SQLite
	// allows in an index. No two of them have one name, as [Name.Fold] compares names. Whether the
	// table has them is not decided here.
	Columns []IndexColumn
	// SQL is the text that SQLite keeps for the index in sqlite_schema: "CREATE INDEX ", and the
	// text from the name of the index up to the semicolon of the statement, or to the end of the
	// text if it has none, with its spaces and comments. It never holds IF NOT EXISTS, which comes
	// before the name.
	SQL string
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
	Span Span
	// IfExists reports whether the statement says IF EXISTS. With it, SQLite does nothing and gives
	// no error when no index of the name exists, also when a table has the name.
	IfExists bool
	Index    Name
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

// Parameters of the statement: the number of the values of its conditions that are a parameter, with
// those of its subqueries, and one more if its LIMIT is a parameter, which comes after them in the
// text.
func (s *Select) Parameters() int {
	n := conditionParameters(s.Where, s.Subqueries)
	if s.Limit.Parameter > 0 {
		n++
	}
	return n
}

// Parameters of the statement: the number of the values of its assignments and of its conditions
// that are a parameter, with those of its subqueries and its expressions. Those of the assignments
// come first, as SQLite numbers them in the order of the text.
func (s *Update) Parameters() int {
	n := 0
	for _, a := range s.Set {
		n += parametersOf(a.Value, nil)
	}
	return n + conditionParameters(s.Where, s.Subqueries)
}

// Parameters of the statement: the number of the values of its conditions that are a parameter, with
// those of its subqueries.
func (s *Delete) Parameters() int {
	return conditionParameters(s.Where, s.Subqueries)
}

// conditionParameters of the conditions where, whose subqueries are subqueries: the number of their
// values that are a parameter, and of the parameters of the subqueries and the expressions of them.
func conditionParameters(where []Condition, subqueries []Select) int {
	n := 0
	for _, c := range where {
		n += c.Expression.parameters() + parametersOf(c.Value, subqueries)
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
