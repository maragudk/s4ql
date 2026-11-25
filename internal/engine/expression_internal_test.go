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
