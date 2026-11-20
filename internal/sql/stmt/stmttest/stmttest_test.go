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

func TestGenerator_Update(t *testing.T) {
	t.Run("should write the assignments and the conditions that the hook gives, with = and ==, and a ? now and then, numbered across both", func(t *testing.T) {
		pools, err := stmttest.NamePools()
		is.NotError(t, err)
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(3, 4)), pools)
		g.Parameters = 2
		values := []stmt.Value{stmt.Integer(-1), stmt.Real(1.5), stmt.Text("a'b"), stmt.Blob{1}, stmt.Null{}}
		var tables []string
		g.Update = func(table stmt.Name) ([]stmt.Assignment, []stmt.Condition) {
			tables = append(tables, table.Raw)
			var set []stmt.Assignment
			for i, v := range values {
				set = append(set, stmt.Assignment{Column: stmt.Name{Raw: fmt.Sprint(`"c`, i, `"`)}, Value: stmt.Term{Value: v}})
			}
			if len(tables)%4 == 0 {
				return set, nil
			}
			return set, []stmt.Condition{{Column: stmt.Name{Raw: "w"}, Operator: stmt.OpEqual, Value: stmt.Term{Value: stmt.Integer(5)}}}
		}
		parameters, without, doubled := 0, 0, 0
		for range 200 {
			src, s, args := g.CallOf(stmttest.KindUpdate)
			got, err := stmt.Check(src)
			is.NotError(t, err, src)
			is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
			u := s.(*stmt.Update)
			is.Equal(t, len(args), u.Parameters(), src)
			is.Equal(t, len(values), len(u.Set), src)
			for i, a := range u.Set {
				is.Equal(t, fmt.Sprint(`"c`, i, `"`), a.Column.Raw, src)
				if a.Value.Parameter > 0 {
					parameters++
					is.Equal(t, fmt.Sprintf("%#v", stmttest.Argument(values[i])), fmt.Sprintf("%#v", args[a.Value.Parameter-1]), src)
				} else {
					is.Equal(t, fmt.Sprintf("%#v", values[i]), fmt.Sprintf("%#v", a.Value.Value), src)
				}
			}
			if u.Where == nil {
				without++
				continue
			}
			if v := u.Where[0].Value; v.Parameter > 0 {
				is.Equal(t, fmt.Sprintf("%#v", stmttest.Argument(stmt.Integer(5))), fmt.Sprintf("%#v", args[v.Parameter-1]), src)
			}
			if strings.Contains(src, "==") {
				doubled++
			}
		}
		is.Equal(t, 200, len(tables))
		is.Equal(t, 50, without)
		is.True(t, parameters > 100, "too few parameters")
		is.True(t, doubled > 20, "too few ==")
	})
}

func TestGenerator_Delete(t *testing.T) {
	t.Run("should write the conditions that the hook gives", func(t *testing.T) {
		pools, err := stmttest.NamePools()
		is.NotError(t, err)
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(5, 6)), pools)
		g.Parameters = 2
		n := 0
		g.Delete = func(stmt.Name) []stmt.Condition {
			n++
			if n%2 == 0 {
				return nil
			}
			return []stmt.Condition{{Column: stmt.Name{Raw: "k"}, Operator: stmt.OpIsNotNull}, {Column: stmt.Name{Raw: "v"}, Operator: stmt.OpLess, Value: stmt.Term{Value: stmt.Text("a")}}}
		}
		without := 0
		for range 100 {
			src, s, args := g.CallOf(stmttest.KindDelete)
			got, err := stmt.Check(src)
			is.NotError(t, err, src)
			is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
			d := s.(*stmt.Delete)
			is.Equal(t, len(args), d.Parameters(), src)
			if d.Where == nil {
				without++
				continue
			}
			is.Equal(t, 2, len(d.Where), src)
			is.Equal(t, stmt.OpIsNotNull, d.Where[0].Operator, src)
		}
		is.Equal(t, 100, n)
		is.Equal(t, 50, without)
	})
}

func TestGenerator_Changes(t *testing.T) {
	t.Run("should write updates and deletes among statements of a kind at random only with Changes", func(t *testing.T) {
		pools, err := stmttest.NamePools()
		is.NotError(t, err)
		count := func(changes int) map[string]int {
			g := stmttest.NewGenerator(rand.New(rand.NewPCG(7, 8)), pools)
			g.Changes = changes
			g.Conditions = 3
			kinds := map[string]int{}
			for range 600 {
				src, s := g.Statement()
				got, err := stmt.Check(src)
				is.NotError(t, err, src)
				is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
				kinds[fmt.Sprintf("%T", s)]++
				switch s := s.(type) {
				case *stmt.Update:
					if s.Where != nil {
						kinds["update with WHERE"]++
					}
				case *stmt.Delete:
					if s.Where != nil {
						kinds["delete with WHERE"]++
					}
				}
			}
			return kinds
		}
		without := count(0)
		is.Equal(t, 0, without["*stmt.Update"]+without["*stmt.Delete"])
		with := count(3)
		for _, k := range []string{"*stmt.Update", "*stmt.Delete", "update with WHERE", "delete with WHERE"} {
			is.True(t, with[k] > 40, fmt.Sprint(k, with))
		}
		is.True(t, with["update with WHERE"] < with["*stmt.Update"], fmt.Sprint(with))
		is.True(t, with["delete with WHERE"] < with["*stmt.Delete"], fmt.Sprint(with))
	})
}
