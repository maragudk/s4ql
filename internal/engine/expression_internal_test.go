package engine

import (
	"fmt"
	"testing"

	"maragu.dev/is"

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
