package engine

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

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

// checkArguments of a statement with the given number of parameters: the error of [countArguments],
// and then "not supported" for the first argument that is not a value that a column can hold, as
// [checkArgument] says. Whether it is of the type of its column is checked when the statement runs,
// after SQLite's errors of the names. No value of an argument is in an error.
func checkArguments(parameters int, args []*s4qlpb.Value) error {
	if err := countArguments(parameters, len(args)); err != nil {
		return err
	}
	return checkValues(args)
}

// checkValues args, the arguments of a statement, as [checkArgument] checks each, by its number.
func checkValues(args []*s4qlpb.Value) error {
	for i, a := range args {
		if err := checkArgument(i+1, a); err != nil {
			return err
		}
	}
	return nil
}

// checkArgument a, the argument of the given number: "not supported" for a value of no kind, which
// no column holds, for a real that is not a number or is infinite, which SQLite stores as NULL or as
// itself, and for text that is not valid UTF-8 or holds the character NUL, which s4ql never stores.
// An integer, a blob, NULL, and every other real and text are values that a column can hold.
func checkArgument(number int, a *s4qlpb.Value) error {
	var which string
	switch a.WhichKind() {
	case s4qlpb.Value_Integer_case, s4qlpb.Value_Blob_case, s4qlpb.Value_Null_case:
		return nil
	case s4qlpb.Value_Real_case:
		switch r := a.GetReal(); {
		case math.IsNaN(r):
			which = "is a real that is not a number"
		case math.IsInf(r, 0):
			which = "is an infinite real"
		default:
			return nil
		}
	case s4qlpb.Value_Text_case:
		switch text := a.GetText(); {
		case !utf8.ValidString(text):
			which = "is text that is not valid UTF-8"
		case strings.IndexByte(text, 0) >= 0:
			which = "is text that holds the character NUL"
		default:
			return nil
		}
	default:
		which = "has no value"
	}
	return fmt.Errorf("%w: argument %v, which %v", stmt.ErrNotSupported, number, which)
}

// checkLength of the value v: SQLite's error for a text or a blob that is longer than limit bytes,
// which SQLite gives when it binds such an argument.
func checkLength(v *s4qlpb.Value, limit int) error {
	var n int
	switch v.WhichKind() {
	case s4qlpb.Value_Text_case:
		n = len(v.GetText())
	case s4qlpb.Value_Blob_case:
		n = len(v.GetBlob())
	}
	if n > limit {
		return tooBig()
	}
	return nil
}

// checkType of the value v for the column at position of the table t: "not supported" unless the
// value has the type of the column, an integer for INT and INTEGER, a real for REAL, text for TEXT,
// and a blob for BLOB, also where SQLite would convert it or give an error of its own. NULL has no
// type, and is never refused: an insert checks the NOT NULL of a column before, and a comparison
// with NULL is neither true nor false. The value comes from the expression e, an argument or a
// literal, which the message names, and use says in the message how the value meets the column:
// "for" for a value of an insert, and "compared with" for one that a condition compares with it.
func checkType(t *s4qlpb.CreateTable, position int, v *s4qlpb.Value, e Expr, use string) error {
	c := t.GetColumns()[position]
	var holds bool
	switch c.GetType() {
	case s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INTEGER:
		holds = v.HasInteger()
	case s4qlpb.Type_TYPE_REAL:
		holds = v.HasReal()
	case s4qlpb.Type_TYPE_TEXT:
		holds = v.HasText()
	case s4qlpb.Type_TYPE_BLOB:
		holds = v.HasBlob()
	default:
		return internalError("a column of type %v", c.GetType())
	}
	if holds || v.HasNull() {
		return nil
	}
	var what string
	switch e := e.(type) {
	case *Parameter:
		what = fmt.Sprintf("argument %v, which is %v,", e.Number, kindName(v))
	case *Integer:
		what = "an integer literal"
	case *Real:
		what = "a real literal"
	case *Text:
		what = "a string literal"
	case *Blob:
		what = "a blob literal"
	default:
		return internalError("a value of an expression of type %T", e)
	}
	return fmt.Errorf("%w: %v %v the column %v.%v of type %v", stmt.ErrNotSupported, what, use, t.GetTable(), c.GetName(), typeName(c.GetType()))
}

// limitCount is the number of rows of a LIMIT whose expression e, a literal or a parameter, has the
// value v: an integer from 0. Every other value is "not supported", by the rule of exact types, as
// [checkType] refuses a value of another type than its column: a negative integer, which SQLite
// reads as no limit; a real and a text, which SQLite converts to an integer where they hold one, and
// refuses with `datatype mismatch` where they do not; and a blob and NULL, which SQLite refuses with
// `datatype mismatch`. The statement pass has refused every such literal, so the value is the
// argument of a parameter, which the message names by its number, without its value.
func limitCount(e Expr, v *s4qlpb.Value) (int64, error) {
	var which string
	switch {
	case v.HasInteger() && v.GetInteger() >= 0:
		return v.GetInteger(), nil
	case v.HasInteger():
		which = "a negative integer"
	case v.HasNull():
		which = "NULL"
	default:
		which = kindName(v)
	}
	p, ok := e.(*Parameter)
	if !ok {
		return 0, internalError("a LIMIT of an expression of type %T, which is %v", e, which)
	}
	return 0, fmt.Errorf("%w: argument %v, which is %v, for LIMIT, which takes an integer from 0", stmt.ErrNotSupported, p.Number, which)
}

// kindName of the kind of the value v, in a sentence: "an integer", "a real", "text", or "a blob".
func kindName(v *s4qlpb.Value) string {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return "an integer"
	case s4qlpb.Value_Real_case:
		return "a real"
	case s4qlpb.Value_Text_case:
		return "text"
	case s4qlpb.Value_Blob_case:
		return "a blob"
	}
	return fmt.Sprint("a value of kind ", v.WhichKind())
}

// countArguments: [ErrArguments] if n arguments are not one for each of the given number of
// parameters, with both numbers in its message, and nil otherwise.
func countArguments(parameters, n int) error {
	if n == parameters {
		return nil
	}
	return fmt.Errorf("%w: %v for a statement with %v %v", ErrArguments, n, parameters, parameterNoun(parameters))
}

// internalError that matches [ErrInternal], and wraps nothing else.
func internalError(format string, args ...any) error {
	return fmt.Errorf("%w: %v", ErrInternal, fmt.Sprintf(format, args...))
}

// The errors of SQLite that the supported statements can give, with the names in the form that
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

// indexNamed is `there is already an index named %s`, for a create table under the name of an index,
// with the value of the name that the statement writes.
func indexNamed(value string) error {
	return &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError, Message: "there is already an index named " + value, Offset: -1}
}

// tableNamed is `there is already a table named %s`, for a create index under the name of a table,
// with the value of the name of the index that the statement writes.
func tableNamed(value string) error {
	return &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError, Message: "there is already a table named " + value, Offset: -1}
}

// indexExists is `index %s already exists`, with the value of the name of the index that the
// statement writes.
func indexExists(value string) error {
	return &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError, Message: "index " + value + " already exists", Offset: -1}
}

// noSuchIndex is `no such index: %s`, with the value of the name of the index that the statement
// writes.
func noSuchIndex(value string) error {
	return &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError, Message: "no such index: " + value, Offset: -1}
}

// noSuchColumn is `table %s has no column named %s`, with the values of the names that the statement
// writes.
func noSuchColumn(table, column string) error {
	return &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError, Message: fmt.Sprintf("table %v has no column named %v", table, column), Offset: -1}
}

// noSuchResultColumn is `no such column: %s`, for the name of a column of a select list or of a
// condition that the table does not have, with the value of the name. For a name in double quotes, SQLite, whose build
// takes double quotes for names only, adds that it may have been meant as a string: `no such column:
// "%s" - should this be a string literal in single-quotes?`.
func noSuchResultColumn(name stmt.Name) error {
	message := "no such column: " + name.Value
	if strings.HasPrefix(name.Raw, `"`) {
		message = `no such column: "` + name.Value + `" - should this be a string literal in single-quotes?`
	}
	return &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError, Message: message, Offset: name.Offset}
}

// valuesSupplied is `table %s has %d columns but %d values were supplied`, for an insert without a
// list of columns, with the value of the name of the table that the statement writes.
func valuesSupplied(table string, columns, values int) error {
	return &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError,
		Message: fmt.Sprintf("table %v has %v columns but %v values were supplied", table, columns, values), Offset: -1}
}

// valuesForColumns is `%d values for %d columns`, for an insert whose list of columns has another
// number of columns than it has values.
func valuesForColumns(values, columns int) error {
	return &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError,
		Message: fmt.Sprintf("%v values for %v columns", values, columns), Offset: -1}
}

// notNullFailed is `NOT NULL constraint failed: %s.%s`, for a column that does not take NULL and that
// an insert gives NULL or leaves out, with the names as the table stores them.
func notNullFailed(table, column string) error {
	return &sqlerr.Error{Code: sqlerr.CodeConstraint, ExtendedCode: sqlerr.CodeConstraintNotNull,
		Message: "NOT NULL constraint failed: " + table + "." + column, Offset: -1}
}

// uniqueFailed is `UNIQUE constraint failed: %s.%s`, for a primary key of one column, with the names
// as the table stores them.
func uniqueFailed(table, column string) error {
	return &sqlerr.Error{Code: sqlerr.CodeConstraint, ExtendedCode: sqlerr.CodeConstraintPrimaryKey,
		Message: "UNIQUE constraint failed: " + table + "." + column, Offset: -1}
}

// tooBig is `string or blob too big`, which SQLite gives for a text or a blob, and a row, that is
// longer than its limit.
func tooBig() error {
	return &sqlerr.Error{Code: sqlerr.CodeTooBig, ExtendedCode: sqlerr.CodeTooBig, Message: "string or blob too big", Offset: -1}
}

// computedTooBig is SQLite's `string or blob too big` of a text that an expression computes, which
// SQLite gives when it steps the statement, also where the engine gives it before it reads a row, so
// a failed update or delete sets the count of changes to 0 then, as [built] says. It matches
// [*sqlerr.Error] with [errors.As].
type computedTooBig struct {
	err error
}

func (e *computedTooBig) Error() string { return e.err.Error() }

func (e *computedTooBig) Unwrap() error { return e.err }

// built is the result of a statement whose sink's build failed with err: that of a statement that
// changed nothing and leaves the count of changes, as SQLite's errors of preparing and of binding
// leave it, but for a text of an expression that is too long, which SQLite gives when it steps the
// statement, and which sets the count to 0 for a statement that sets it, as the plan p says.
func built(p Plan, err error) Result {
	if _, ok := errors.AsType[*computedTooBig](err); ok {
		return Result{SetsChanges: setsChanges(p)}
	}
	return Result{}
}

// schemaTooBig reports whether SQLite refuses the create table of the table with the name, which
// keeps the text sql, as too big under the length limit. SQLite writes the table into its schema
// with an UPDATE of sqlite_schema whose text it formats under the limit, with the name twice and
// sql, each in single quotes and with every single quote doubled, and it gives `string or blob too
// big` when it prepares the statement if that text is limit bytes or longer. The register numbers
// in the text, #2 and #1, are those of every create table that s4ql supports.
func schemaTooBig(name, sql string, limit int) bool {
	quoted := func(s string) int { return len(s) + strings.Count(s, "'") + 2 }
	const rest = len("UPDATE 'main'.sqlite_master SET type='table', name=, tbl_name=, rootpage=#2, sql= WHERE rowid=#1")
	return rest+2*quoted(name)+quoted(sql) >= limit
}

// indexTooBig reports whether SQLite refuses the create index of the index with the name on the
// table, whose name is given as the table stores it, which keeps the text sql, as too big under the
// length limit. SQLite writes the index into its schema with an INSERT into sqlite_schema whose text
// it formats under the limit, with the two names and sql, each in single quotes and with every single
// quote doubled, and it gives `string or blob too big` when it prepares the statement if that text is
// limit bytes or longer. The register number in the text, #1, is that of every create index that
// s4ql supports.
func indexTooBig(name, table, sql string, limit int) bool {
	quoted := func(s string) int { return len(s) + strings.Count(s, "'") + 2 }
	const rest = len("INSERT INTO 'main'.sqlite_master VALUES('index',,,#1,);")
	return rest+quoted(name)+quoted(table)+quoted(sql) >= limit
}

// tooLongForCache is s4ql's "not supported" for a table or an index, what, of the name, that SQLite
// takes under the length limit and whose statement in the cache is longer than the limit, which the
// storage refuses. The cache writes every name in double quotes, NOT NULL after every column outside
// the key that does not take NULL, and each default in its canonical form, so its statement can be
// longer than the text of the statement by an amount that grows with the columns and the defaults,
// and a statement under the limit can meet it, where SQLite gives no error and creates the table or
// the index.
func tooLongForCache(what, name string) error {
	return fmt.Errorf("%w: the %v %v, whose statement in the cache is longer than the length limit", stmt.ErrNotSupported, what, name)
}

// readOnlyDatabase is `attempt to write a readonly database`, which SQLite gives for a statement that
// would write on a connection under `pragma query_only = 1`, when it steps the statement.
func readOnlyDatabase() error {
	return &sqlerr.Error{Code: sqlerr.CodeReadOnly, ExtendedCode: sqlerr.CodeReadOnly, Message: "attempt to write a readonly database", Offset: -1}
}

// noSuchSetColumn is `no such column: %s`, for a column of the SET of an update that the table does
// not have, with the value of the name, also for a name in double quotes: SQLite's update gives it
// itself, and not its resolver of names, which adds the question about quotes.
func noSuchSetColumn(name stmt.Name) error {
	return &sqlerr.Error{Code: sqlerr.CodeError, ExtendedCode: sqlerr.CodeError, Message: "no such column: " + name.Value, Offset: -1}
}
