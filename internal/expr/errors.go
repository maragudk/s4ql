package expr

import (
	"errors"
	"fmt"

	"maragu.dev/s4ql/internal/sql/stmt"
)

var (
	// ErrEvaluation is matched by every [*Error]: an operation of an expression whose operands give
	// no value in s4ql, where SQLite gives a value that s4ql refuses, a real for an integer overflow,
	// NULL for a division by zero, or an infinity; or where SQLite fails too, for a text longer than
	// the length limit.
	ErrEvaluation = errors.New("s4ql: evaluation error")

	// ErrOverflow is the kind of an [*Error] of an operation on integers whose result does not fit a
	// signed 64-bit integer, where SQLite computes the result as a real: also the negation of the
	// smallest integer, and its division by -1.
	ErrOverflow = errors.New("s4ql: integer overflow")

	// ErrDivisionByZero is the kind of an [*Error] of a division by the integer 0 or the real 0.0 or
	// -0.0, where SQLite gives NULL.
	ErrDivisionByZero = errors.New("s4ql: division by zero")

	// ErrNotFinite is the kind of an [*Error] of an operation on reals whose result is infinite, or
	// not a number, where SQLite gives the infinity, or NULL.
	ErrNotFinite = errors.New("s4ql: real result that is not finite")

	// ErrTooLong is the kind of an [*Error] of a concatenation or a call whose text is longer than
	// the length limit of [Program.Bind], where SQLite fails with SQLITE_TOOBIG, `string or blob too
	// big`, and of a literal longer than the limit, which SQLite refuses as well, or reads as NULL
	// for a blob.
	ErrTooLong = errors.New("s4ql: text or blob longer than the length limit")

	// ErrInternal is matched by the error of a fault of s4ql: something happened that the evaluator
	// rules out, such as a value of another type than its column in a row.
	ErrInternal = errors.New("s4ql: internal error")
)

// Error of an operation of an expression whose operands give no value that s4ql gives. It matches
// [ErrEvaluation] and its Kind with [errors.Is]. Its message names no value, since a value may come
// from a row or an argument.
type Error struct {
	// Kind of the error: [ErrOverflow], [ErrDivisionByZero], [ErrNotFinite], or [ErrTooLong].
	Kind error
	// Op is the operator that failed, [stmt.ExprCall] for a call, or [stmt.ExprValue] for a literal.
	Op stmt.ExpressionKind
	// Function of a call that failed, and 0 for an operator.
	Function stmt.Function
	// Span of the operator's node in the text of the statement, as [stmt.Expression] has it.
	Span stmt.Span
}

// Error gives the message of the kind, the operator or the function, and the offset of its node,
// such as "s4ql: integer overflow in + (at offset 17)" or "s4ql: text or blob longer than the length
// limit in hex (at offset 17)".
func (e *Error) Error() string {
	return fmt.Sprintf("%v in %v (at offset %v)", e.Kind, nodeName(e.Op, e.Function), e.Span.Start)
}

// Is reports whether target is [ErrEvaluation] or the Kind of e.
func (e *Error) Is(target error) bool {
	return target == ErrEvaluation || target == e.Kind
}

// internalError gives an error that matches [ErrInternal], with a message of format and args.
func internalError(format string, args ...any) error {
	return fmt.Errorf("%w: %v", ErrInternal, fmt.Sprintf(format, args...))
}
