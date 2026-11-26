package expr_test

import (
	"errors"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/expr"
	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestError(t *testing.T) {
	t.Run("should name the kind, the operator, and the offset of its node", func(t *testing.T) {
		err := &expr.Error{Kind: expr.ErrOverflow, Op: stmt.ExprAdd, Span: stmt.Span{Start: 17, End: 22}}
		is.Equal(t, "s4ql: integer overflow in + (at offset 17)", err.Error())
		err = &expr.Error{Kind: expr.ErrDivisionByZero, Op: stmt.ExprDivide, Span: stmt.Span{Start: 3, End: 8}}
		is.Equal(t, "s4ql: division by zero in / (at offset 3)", err.Error())
		err = &expr.Error{Kind: expr.ErrNotFinite, Op: stmt.ExprMultiply, Span: stmt.Span{Start: 0, End: 8}}
		is.Equal(t, "s4ql: real result that is not finite in * (at offset 0)", err.Error())
	})

	t.Run("should name the function of a call, and || for a concatenation", func(t *testing.T) {
		err := &expr.Error{Kind: expr.ErrTooLong, Op: stmt.ExprCall, Function: stmt.FuncHex, Span: stmt.Span{Start: 17, End: 23}}
		is.Equal(t, "s4ql: text or blob longer than the length limit in hex (at offset 17)", err.Error())
		err = &expr.Error{Kind: expr.ErrTooLong, Op: stmt.ExprCall, Function: stmt.FuncLower, Span: stmt.Span{Start: 4, End: 12}}
		is.Equal(t, "s4ql: text or blob longer than the length limit in lower (at offset 4)", err.Error())
		err = &expr.Error{Kind: expr.ErrTooLong, Op: stmt.ExprConcat, Span: stmt.Span{Start: 0, End: 6}}
		is.Equal(t, "s4ql: text or blob longer than the length limit in || (at offset 0)", err.Error())
		is.True(t, errors.Is(err, expr.ErrTooLong))
		err = &expr.Error{Kind: expr.ErrTooLong, Op: stmt.ExprValue, Span: stmt.Span{Start: 5, End: 40}}
		is.Equal(t, "s4ql: text or blob longer than the length limit in a literal (at offset 5)", err.Error())
		is.True(t, errors.Is(err, expr.ErrEvaluation))
		is.True(t, !errors.Is(err, expr.ErrOverflow))
	})

	t.Run("should match its kind and the evaluation error, and no other kind", func(t *testing.T) {
		var err error = &expr.Error{Kind: expr.ErrNotFinite, Op: stmt.ExprMultiply}
		is.True(t, errors.Is(err, expr.ErrNotFinite))
		is.True(t, errors.Is(err, expr.ErrEvaluation))
		is.True(t, !errors.Is(err, expr.ErrOverflow))
		is.True(t, !errors.Is(err, expr.ErrDivisionByZero))
		is.True(t, !errors.Is(err, expr.ErrInternal))
		is.True(t, !errors.Is(err, stmt.ErrNotSupported))
	})
}
