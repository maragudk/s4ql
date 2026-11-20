package stmttest_test

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

func TestGenerator_Where(t *testing.T) {
	t.Run("should write the conditions that the hook gives, with every spelling of an operator and a ? now and then", func(t *testing.T) {
		pools, err := stmttest.NamePools()
		is.NotError(t, err)
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(1, 2)), pools)
		g.Parameters = 2
		ops := []stmt.Operator{stmt.OpEqual, stmt.OpNotEqual, stmt.OpLess, stmt.OpLessOrEqual, stmt.OpGreater,
			stmt.OpGreaterOrEqual, stmt.OpIsNull, stmt.OpIsNotNull}
		values := []stmt.Value{stmt.Integer(-1), stmt.Real(1.5), stmt.Text("a'b"), stmt.Blob{1}, stmt.Null{}, stmt.Integer(2), nil, nil}
		var tables []string
		g.Where = func(table stmt.Name) []stmt.Condition {
			tables = append(tables, table.Raw)
			if len(tables)%5 == 0 {
				return nil
			}
			var cs []stmt.Condition
			for i, op := range ops {
				cs = append(cs, stmt.Condition{Column: stmt.Name{Raw: fmt.Sprint(`"c`, i, `"`)}, Operator: op, Value: stmt.Term{Value: values[i]}})
			}
			return cs
		}
		spellings := map[string]bool{}
		parameters, without := 0, 0
		for range 200 {
			src, s, args := g.CallOf(stmttest.KindSelect)
			got, err := stmt.Check(src)
			is.NotError(t, err, src)
			is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
			where := s.(*stmt.Select).Where
			if where == nil {
				without++
				is.True(t, !strings.Contains(strings.ToLower(src), "where"), src)
				continue
			}
			is.Equal(t, len(ops), len(where), src)
			for i, c := range where {
				is.Equal(t, fmt.Sprint(`"c`, i, `"`), c.Column.Raw, src)
				is.Equal(t, ops[i], c.Operator, src)
				switch {
				case values[i] == nil:
					is.True(t, c.Value.Value == nil && c.Value.Parameter == 0, src)
				case c.Value.Parameter > 0:
					parameters++
					is.Equal(t, fmt.Sprintf("%#v", stmttest.Argument(values[i])), fmt.Sprintf("%#v", args[c.Value.Parameter-1]), src)
				default:
					is.Equal(t, fmt.Sprintf("%#v", values[i]), fmt.Sprintf("%#v", c.Value.Value), src)
				}
			}
			for _, op := range []string{"==", "!=", "<>", "<=", ">="} {
				if strings.Contains(src, op) {
					spellings[op] = true
				}
			}
			if strings.Contains(src, "=") && !strings.Contains(src, "==") {
				spellings["="] = true
			}
		}
		is.Equal(t, 200, len(tables))
		is.Equal(t, 40, without)
		is.True(t, parameters > 50, "too few parameters")
		is.Equal(t, 6, len(spellings), fmt.Sprint(spellings))
	})
}
