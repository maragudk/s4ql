package engine

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/expr"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestArgumentLengths(t *testing.T) {
	t.Run("should give the internal error of a parameter that the arguments do not have, which no statement gives", func(t *testing.T) {
		args := []*s4qlpb.Value{integer(1), integer(2)}
		for _, n := range []int{0, 3} {
			e := stmt.Expression{Kind: stmt.ExprAdd, Operands: []stmt.Expression{{Kind: stmt.ExprParameter, Parameter: 1}, {Kind: stmt.ExprParameter, Parameter: n}}}
			err := argumentLengths(e, args, 1000)
			is.Equal(t, fmt.Sprintf("s4ql: internal error: the parameter %v of a statement with 2 arguments", n), err.Error())
		}
		for _, n := range []int{1, 2} {
			is.NotError(t, argumentLengths(stmt.Expression{Kind: stmt.ExprParameter, Parameter: n}, args, 1000))
		}
	})
}

func TestFromExpr(t *testing.T) {
	t.Run("should give a fault of the evaluator as an internal error of the engine, and every other error as it is", func(t *testing.T) {
		fault := fmt.Errorf("%w: a row without its column", expr.ErrInternal)
		err := fromExpr(fault)
		is.True(t, errors.Is(err, ErrInternal))
		is.Equal(t, "s4ql: internal error: "+fault.Error(), err.Error())

		refusal := fmt.Errorf("%w: column of type TEXT in arithmetic", stmt.ErrNotSupported)
		is.Equal(t, refusal, fromExpr(refusal))
	})
}

func TestClass_String(t *testing.T) {
	t.Run("should name each class", func(t *testing.T) {
		for c, want := range map[class]string{classAny: "null", classInteger: "integer", classReal: "real", classText: "text", classBlob: "blob"} {
			is.Equal(t, want, c.String())
		}
	})
}

func TestCompareNumbers(t *testing.T) {
	t.Run("should compare integers and reals by their number, and give the internal error of two classes or of a class that is no number, which the checks of the types rule out", func(t *testing.T) {
		n, err := compareNumbers(number{class: classInteger, i: 1}, number{class: classInteger, i: 2})
		is.NotError(t, err)
		is.Equal(t, -1, n)
		n, err = compareNumbers(number{class: classReal, f: 0}, number{class: classReal, f: math.Copysign(0, -1)})
		is.NotError(t, err)
		is.Equal(t, 0, n)
		for _, c := range []struct {
			a, b number
			err  string
		}{
			{a: number{class: classInteger, i: 1}, b: number{class: classReal, f: 1}, err: "a comparison of values of the classes integer and real"},
			{a: number{class: classReal, f: 1}, b: number{class: classInteger, i: 1}, err: "a comparison of values of the classes real and integer"},
			{a: number{}, b: number{}, err: "a comparison of values of class null"},
			{a: number{class: classText}, b: number{class: classText}, err: "a comparison of values of class text"},
		} {
			n, err := compareNumbers(c.a, c.b)
			is.Equal(t, 0, n, c.err)
			is.Equal(t, "s4ql: internal error: "+c.err, fmt.Sprint(err))
		}
	})
}

func TestBindArithmetic(t *testing.T) {
	t.Run("should give the internal error of a column that the table does not have, which the walk over the statement rules out", func(t *testing.T) {
		table := newFake().with("t", []string{"k", "a"}).tables["t"].ct
		e := stmt.Expression{Kind: stmt.ExprAdd, Operands: []stmt.Expression{
			{Kind: stmt.ExprColumn, Column: stmt.Name{Value: "a"}},
			{Kind: stmt.ExprColumn, Column: stmt.Name{Value: "b"}},
		}}
		_, err := bindArithmetic(table, e, nil)
		is.Equal(t, "s4ql: internal error: an expression of the column b, which the table t does not have", err.Error())
	})
}
