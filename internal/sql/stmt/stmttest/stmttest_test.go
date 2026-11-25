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

func TestGenerator_CreateIndex(t *testing.T) {
	t.Run("should write the columns that the hook gives, with ASC, DESC, or neither", func(t *testing.T) {
		pools, err := stmttest.NamePools()
		is.NotError(t, err)
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(9, 10)), pools)
		var names []string
		g.CreateIndex = func(index, table stmt.Name) []stmt.IndexColumn {
			names = append(names, index.Raw+" on "+table.Raw)
			return []stmt.IndexColumn{{Name: stmt.Name{Raw: `"c0"`}}, {Name: stmt.Name{Raw: "c1"}, Descending: true}, {Name: stmt.Name{Raw: "C2"}}}
		}
		asc := 0
		for range 100 {
			src, s, args := g.CallOf(stmttest.KindCreateIndex)
			got, err := stmt.Check(src)
			is.NotError(t, err, src)
			is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
			is.Equal(t, 0, len(args), src)
			c := s.(*stmt.CreateIndex)
			is.Equal(t, names[len(names)-1], c.Index.Raw+" on "+c.Table.Raw, src)
			is.Equal(t, 3, len(c.Columns), src)
			for i, raw := range []string{`"c0"`, "c1", "C2"} {
				is.Equal(t, raw, c.Columns[i].Name.Raw, src)
				is.Equal(t, i == 1, c.Columns[i].Descending, src)
			}
			if strings.Count(strings.ToLower(src), "asc") > strings.Count(strings.ToLower(c.Index.Raw+c.Table.Raw), "asc") {
				asc++
			}
		}
		is.Equal(t, 100, len(names))
		is.True(t, asc > 20 && asc < 100, fmt.Sprint(asc))
	})
}

func TestGenerator_Indexes(t *testing.T) {
	t.Run("should write create indexes and drop indexes among statements of a kind at random only with Indexes", func(t *testing.T) {
		pools, err := stmttest.NamePools()
		is.NotError(t, err)
		count := func(indexes int) map[string]int {
			g := stmttest.NewGenerator(rand.New(rand.NewPCG(11, 12)), pools)
			g.Indexes = indexes
			g.Columns = 3
			kinds := map[string]int{}
			for range 600 {
				src, s := g.Statement()
				got, err := stmt.Check(src)
				is.NotError(t, err, src)
				is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
				kinds[fmt.Sprintf("%T", s)]++
				if c, ok := s.(*stmt.CreateIndex); ok {
					kinds[fmt.Sprint("create index of ", len(c.Columns))]++
					for _, column := range c.Columns {
						if column.Descending {
							kinds["descending"]++
						}
					}
				}
			}
			return kinds
		}
		without := count(0)
		is.Equal(t, 0, without["*stmt.CreateIndex"]+without["*stmt.DropIndex"])
		with := count(3)
		for _, k := range []string{"*stmt.CreateIndex", "*stmt.DropIndex", "create index of 1", "create index of 2", "create index of 3", "descending"} {
			is.True(t, with[k] > 10, fmt.Sprint(k, with))
		}
		is.Equal(t, 0, with["create index of 4"], fmt.Sprint(with))
	})
}

func TestGenerator_Exists(t *testing.T) {
	t.Run("should write IF NOT EXISTS and IF EXISTS in creates and drops only with Exists", func(t *testing.T) {
		pools, err := stmttest.NamePools()
		is.NotError(t, err)
		count := func(exists int) map[string]int {
			g := stmttest.NewGenerator(rand.New(rand.NewPCG(13, 14)), pools)
			g.Exists = exists
			g.Indexes = 2
			kinds := map[string]int{}
			for range 800 {
				src, s := g.Statement()
				got, err := stmt.Check(src)
				is.NotError(t, err, src)
				is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
				clause := false
				switch s := s.(type) {
				case *stmt.CreateTable:
					clause = s.IfNotExists
				case *stmt.CreateIndex:
					clause = s.IfNotExists
				case *stmt.DropTable:
					clause = s.IfExists
				case *stmt.DropIndex:
					clause = s.IfExists
				}
				kinds[fmt.Sprintf("%T %v", s, clause)]++
			}
			return kinds
		}
		without := count(0)
		with := count(2)
		for _, k := range []string{"*stmt.CreateTable", "*stmt.CreateIndex", "*stmt.DropTable", "*stmt.DropIndex"} {
			is.Equal(t, 0, without[k+" true"], fmt.Sprint(k, without))
			is.True(t, with[k+" true"] > 20, fmt.Sprint(k, with))
			is.True(t, with[k+" false"] > 20, fmt.Sprint(k, with))
		}
	})
}

func TestGenerator_Defaults(t *testing.T) {
	pools, err := stmttest.NamePools()
	is.NotError(t, err)
	// count the columns of the create tables of a generator with defaults, by whether they say
	// DEFAULT, of what kind of value, on the key or on a column that is NOT NULL, and where the
	// DEFAULT is among the constraints of its column.
	count := func(g *stmttest.Generator) map[string]int {
		kinds := map[string]int{}
		for range 600 {
			src, s := g.StatementOf(stmttest.KindCreateTable)
			got, err := stmt.Check(src)
			is.NotError(t, err, src)
			is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
			for _, c := range s.(*stmt.CreateTable).Columns {
				if c.Default.Value == nil {
					kinds["none"]++
					continue
				}
				kinds[stmttest.LiteralKind(c.Default.Value)]++
				if c.PrimaryKey {
					kinds["on the key"]++
				}
				if c.NotNull {
					kinds["on NOT NULL"]++
				}
				if after := strings.ToLower(src[c.Default.Span.End:]); strings.Contains(strings.SplitN(after, ",", 2)[0], "key") || strings.Contains(strings.SplitN(after, ",", 2)[0], "not") {
					kinds["before another constraint"]++
				}
			}
		}
		return kinds
	}

	t.Run("should write a DEFAULT only with Defaults", func(t *testing.T) {
		for _, columns := range []int{0, 3} {
			g := stmttest.NewGenerator(rand.New(rand.NewPCG(15, 16)), pools)
			g.Columns, g.Nulls = columns, 3
			kinds := count(g)
			is.Equal(t, 1, len(kinds), fmt.Sprint(kinds))
		}
	})

	t.Run("should write a DEFAULT of every kind of value, anywhere among the constraints, with Defaults", func(t *testing.T) {
		for _, columns := range []int{0, 3} {
			g := stmttest.NewGenerator(rand.New(rand.NewPCG(15, 16)), pools)
			g.Columns, g.Nulls, g.Defaults = columns, 3, 2
			kinds := count(g)
			for _, k := range []string{"none", "integer", "real", "text", "blob", "null", "on the key", "before another constraint"} {
				is.True(t, kinds[k] > 10, fmt.Sprint(columns, " ", k, " ", kinds))
			}
			if columns > 0 {
				is.True(t, kinds["on NOT NULL"] > 10, fmt.Sprint(kinds))
			}
		}
	})

	t.Run("should write the DEFAULT that Create gives, whatever Defaults is", func(t *testing.T) {
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(15, 16)), pools)
		g.Columns = 2
		g.Create = func(stmt.Name) []stmt.Column {
			return []stmt.Column{
				{Name: stmt.Name{Raw: "k"}, Type: stmt.TypeText, PrimaryKey: true, Default: stmt.Default{Value: stmt.Text("it's")}},
				{Name: stmt.Name{Raw: "v"}, Type: stmt.TypeInt, NotNull: true, Default: stmt.Default{Value: stmt.Integer(0)}},
				{Name: stmt.Name{Raw: "w"}, Type: stmt.TypeReal},
			}
		}
		for range 50 {
			src, s := g.StatementOf(stmttest.KindCreateTable)
			got, err := stmt.Check(src)
			is.NotError(t, err, src)
			is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
			columns := s.(*stmt.CreateTable).Columns
			is.Equal[stmt.Value](t, stmt.Text("it's"), columns[0].Default.Value, src)
			is.Equal[stmt.Value](t, stmt.Integer(0), columns[1].Default.Value, src)
			is.True(t, columns[2].Default.Value == nil, src)
		}
	})
}

func TestGenerator_ValueFirst(t *testing.T) {
	pools, err := stmttest.NamePools()
	is.NotError(t, err)
	// count the conditions of the selects, updates, and deletes of g, which must each be the statement
	// that the pass gives for its text, by whether the value is on the left, and of those by kind of
	// statement, operator, and whether the value is a ?.
	count := func(g *stmttest.Generator) map[string]int {
		kinds := map[string]int{}
		for i := range 900 {
			src, s, _ := g.CallOf([]stmttest.Kind{stmttest.KindSelect, stmttest.KindUpdate, stmttest.KindDelete}[i%3])
			got, err := stmt.Check(src)
			is.NotError(t, err, src)
			is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
			var where []stmt.Condition
			switch s := s.(type) {
			case *stmt.Select:
				where = s.Where
			case *stmt.Update:
				where = s.Where
			case *stmt.Delete:
				where = s.Where
			}
			for _, c := range where {
				if !c.ValueFirst {
					kinds["column first"]++
					continue
				}
				kinds["value first"]++
				kinds[fmt.Sprintf("%T", s)]++
				kinds[c.Operator.String()]++
				if c.Value.Parameter > 0 {
					kinds["?"]++
				}
				// The text has the value, then the operator swapped, then the column.
				is.True(t, c.Value.Span.End <= c.Column.Offset, src)
			}
		}
		return kinds
	}

	t.Run("should write a value on the left only with ValueFirst", func(t *testing.T) {
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(17, 18)), pools)
		g.Parameters, g.Columns, g.Nulls, g.Conditions = 3, 3, 3, 4
		kinds := count(g)
		is.Equal(t, 0, kinds["value first"], fmt.Sprint(kinds))
		is.True(t, kinds["column first"] > 500, fmt.Sprint(kinds))
	})

	t.Run("should write a value on the left with every operator, in every kind of statement with WHERE, with ValueFirst", func(t *testing.T) {
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(17, 18)), pools)
		g.Parameters, g.Columns, g.Nulls, g.Conditions, g.ValueFirst = 3, 3, 3, 4, 2
		kinds := count(g)
		for _, k := range []string{"column first", "value first", "*stmt.Select", "*stmt.Update", "*stmt.Delete", "=", "!=", "<", "<=", ">", ">=", "?"} {
			is.True(t, kinds[k] > 10, fmt.Sprint(k, " ", kinds))
		}
		is.Equal(t, 0, kinds["IS NULL"]+kinds["IS NOT NULL"], fmt.Sprint(kinds))
	})

	t.Run("should write the value on the left where the hook says so, whatever ValueFirst is", func(t *testing.T) {
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(17, 18)), pools)
		g.Parameters = 2
		g.Where = func(stmt.Name) []stmt.Condition {
			return []stmt.Condition{
				{Column: stmt.Name{Raw: "a"}, Operator: stmt.OpLess, Value: stmt.Term{Value: stmt.Integer(5)}, ValueFirst: true},
				{Column: stmt.Name{Raw: "b"}, Operator: stmt.OpGreaterOrEqual, Value: stmt.Term{Value: stmt.Text("x")}},
				{Column: stmt.Name{Raw: "c"}, Operator: stmt.OpIsNull},
			}
		}
		for range 50 {
			src, s, _ := g.CallOf(stmttest.KindSelect)
			got, err := stmt.Check(src)
			is.NotError(t, err, src)
			is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
			where := s.(*stmt.Select).Where
			is.Equal(t, 3, len(where), src)
			is.True(t, where[0].ValueFirst && where[0].Operator == stmt.OpLess, src)
			is.True(t, !where[1].ValueFirst && !where[2].ValueFirst, src)
		}
	})

	t.Run("should keep the sides of a comparison of two sides with a column, where neither or both are a column alone, and turn the others, with ValueFirst", func(t *testing.T) {
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(17, 18)), pools)
		g.Parameters, g.ValueFirst = 2, 1
		column := func(raw string) stmt.Expression { return stmt.Expression{Kind: stmt.ExprColumn, Column: stmt.Name{Raw: raw}} }
		value := func(v stmt.Value) stmt.Expression { return stmt.Expression{Kind: stmt.ExprValue, Value: v} }
		add := func(a, b stmt.Expression) stmt.Expression { return stmt.Expression{Kind: stmt.ExprAdd, Operands: []stmt.Expression{a, b}} }
		g.Where = func(stmt.Name) []stmt.Condition {
			return []stmt.Condition{
				// Kept: an expression of a column compared with an expression of a column.
				{Expression: add(column("a"), value(stmt.Integer(1))), Operator: stmt.OpLess, Value: stmt.Term{Expression: add(column("b"), value(stmt.Integer(2)))}},
				// Kept: a column compared with a column alone.
				{Column: stmt.Name{Raw: "a"}, Operator: stmt.OpGreater, Value: stmt.Term{Expression: column("b")}},
				// Turned: an expression of a column compared with a value.
				{Expression: add(column("a"), value(stmt.Integer(1))), Operator: stmt.OpLessOrEqual, Value: stmt.Term{Value: stmt.Integer(5)}},
				// Turned: a column compared with an expression without a column.
				{Column: stmt.Name{Raw: "a"}, Operator: stmt.OpGreaterOrEqual, Value: stmt.Term{Expression: add(value(stmt.Integer(1)), value(stmt.Integer(2)))}},
				// Turned: an expression of a column compared with an expression without a column.
				{Expression: add(column("a"), value(stmt.Integer(1))), Operator: stmt.OpNotEqual, Value: stmt.Term{Expression: add(value(stmt.Integer(1)), value(stmt.Integer(2)))}},
			}
		}
		for range 50 {
			src, s, _ := g.CallOf(stmttest.KindSelect)
			got, err := stmt.Check(src)
			is.NotError(t, err, src)
			is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
			where := s.(*stmt.Select).Where
			is.Equal(t, 5, len(where), src)
			is.True(t, !where[0].ValueFirst && where[0].Expression.Present() && where[0].Operator == stmt.OpLess, src)
			is.True(t, !where[1].ValueFirst && where[1].Column.Value == "a" && where[1].Operator == stmt.OpGreater, src)
			is.True(t, where[2].ValueFirst && where[2].Expression.Present() && where[2].Operator == stmt.OpLessOrEqual, src)
			is.True(t, where[3].ValueFirst && where[3].Column.Value == "a" && where[3].Operator == stmt.OpGreaterOrEqual, src)
			is.True(t, where[4].ValueFirst && where[4].Expression.Present() && where[4].Operator == stmt.OpNotEqual, src)
		}
	})
}

func TestGenerator_OrderBy(t *testing.T) {
	pools, err := stmttest.NamePools()
	is.NotError(t, err)
	// count the selects of g, which must each be the statement that the pass gives for its text, with
	// the argument of a ? of LIMIT, by what their ORDER BY and LIMIT have.
	count := func(g *stmttest.Generator) map[string]int {
		kinds := map[string]int{}
		for range 600 {
			src, s, args := g.CallOf(stmttest.KindSelect)
			got, err := stmt.Check(src)
			is.NotError(t, err, src)
			is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
			sel := s.(*stmt.Select)
			is.Equal(t, len(args), sel.Parameters(), src)
			kinds[fmt.Sprint(len(sel.OrderBy), " ordering columns")]++
			named := map[string]bool{}
			for _, n := range sel.Columns {
				named[n.Fold()] = true
			}
			for _, c := range sel.Where {
				named[c.Column.Fold()] = true
			}
			for _, c := range sel.OrderBy {
				kinds[fmt.Sprint("descending ", c.Descending)]++
				kinds[fmt.Sprint("named before ", named[c.Name.Fold()])]++
			}
			lower := strings.ToLower(src)
			if strings.Contains(lower, " asc") || strings.Contains(lower, "\nasc") || strings.Contains(lower, "/asc") {
				kinds["asc"]++
			}
			switch {
			case sel.Limit.Parameter > 0:
				kinds["limit ?"]++
				// The ? of LIMIT is the last one, and its argument an int64 from 0.
				is.Equal(t, sel.Parameters(), sel.Limit.Parameter, src)
				is.True(t, args[len(args)-1].(int64) >= 0, src)
			case sel.Limited():
				v := sel.Limit.Value.(stmt.Integer)
				is.True(t, v >= 0, src)
				kinds[map[bool]string{true: "limit small", false: "limit large"}[v <= 5]]++
				if src[sel.Limit.Span.Start] == '0' && v != 0 {
					kinds["limit with leading zeros"]++
				}
			default:
				kinds["no limit"]++
			}
		}
		return kinds
	}

	t.Run("should write ORDER BY and LIMIT only with OrderBy", func(t *testing.T) {
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(19, 20)), pools)
		g.Parameters, g.Columns, g.Nulls, g.Conditions, g.ValueFirst = 3, 3, 3, 4, 3
		kinds := count(g)
		is.Equal(t, 600, kinds["0 ordering columns"], fmt.Sprint(kinds))
		is.Equal(t, 600, kinds["no limit"], fmt.Sprint(kinds))
	})

	t.Run("should write ORDER BY of one or more columns in every direction, and LIMIT of every kind, with OrderBy", func(t *testing.T) {
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(19, 20)), pools)
		g.Parameters, g.Columns, g.Nulls, g.Conditions, g.ValueFirst, g.OrderBy = 3, 3, 3, 4, 3, 2
		kinds := count(g)
		for _, k := range []string{"0 ordering columns", "1 ordering columns", "2 ordering columns", "3 ordering columns", "4 ordering columns",
			"descending true", "descending false", "asc", "named before true", "named before false",
			"no limit", "limit ?", "limit small", "limit large", "limit with leading zeros"} {
			is.True(t, kinds[k] > 5, fmt.Sprint(k, " ", kinds))
		}
	})

	t.Run("should write the ORDER BY and the LIMIT that the hook gives, whatever OrderBy is", func(t *testing.T) {
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(19, 20)), pools)
		g.Parameters = 2
		var tables []string
		g.Order = func(table stmt.Name) ([]stmt.OrderColumn, stmt.Term) {
			tables = append(tables, table.Raw)
			if len(tables)%4 == 0 {
				return nil, stmt.Term{}
			}
			return []stmt.OrderColumn{{Name: stmt.Name{Raw: "priority"}, Descending: true}, {Name: stmt.Name{Raw: `"created"`}}},
				stmt.Term{Value: stmt.Integer(1)}
		}
		without, parameters := 0, 0
		for range 100 {
			src, s, args := g.CallOf(stmttest.KindSelect)
			got, err := stmt.Check(src)
			is.NotError(t, err, src)
			is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
			sel := s.(*stmt.Select)
			if sel.OrderBy == nil {
				without++
				is.True(t, !sel.Limited() && !strings.Contains(strings.ToLower(src), "order"), src)
				continue
			}
			is.Equal(t, 2, len(sel.OrderBy), src)
			is.True(t, sel.OrderBy[0].Name.Raw == "priority" && sel.OrderBy[0].Descending, src)
			is.True(t, sel.OrderBy[1].Name.Raw == `"created"` && !sel.OrderBy[1].Descending, src)
			if sel.Limit.Parameter > 0 {
				parameters++
				is.Equal(t, fmt.Sprintf("%#v", int64(1)), fmt.Sprintf("%#v", args[sel.Limit.Parameter-1]), src)
			} else {
				is.Equal(t, fmt.Sprintf("%#v", stmt.Integer(1)), fmt.Sprintf("%#v", sel.Limit.Value), src)
			}
		}
		is.Equal(t, 100, len(tables))
		is.Equal(t, 25, without)
		is.True(t, parameters > 10, "too few parameters")
	})
}

func TestGenerator_Returning(t *testing.T) {
	pools, err := stmttest.NamePools()
	is.NotError(t, err)
	// count the inserts, updates, and deletes of g, which must each be the statement that the pass
	// gives for its text, by what their RETURNING has.
	count := func(g *stmttest.Generator) map[string]int {
		kinds := map[string]int{}
		for _, k := range []stmttest.Kind{stmttest.KindInsert, stmttest.KindUpdate, stmttest.KindDelete} {
			for range 300 {
				src, s, args := g.CallOf(k)
				got, err := stmt.Check(src)
				is.NotError(t, err, src)
				is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
				is.Equal(t, len(args), s.Parameters(), src)
				var r stmt.Returning
				var named []stmt.Name
				switch s := s.(type) {
				case *stmt.Insert:
					r, named = s.Returning, s.Columns
				case *stmt.Update:
					r = s.Returning
					for _, a := range s.Set {
						named = append(named, a.Column)
					}
					for _, c := range s.Where {
						named = append(named, c.Column)
					}
				case *stmt.Delete:
					r = s.Returning
					for _, c := range s.Where {
						named = append(named, c.Column)
					}
				}
				switch {
				case !r.Present():
					kinds["none"]++
					is.True(t, !strings.Contains(strings.ToLower(src), "returning"), src)
					continue
				case r.Columns == nil:
					kinds["*"]++
				}
				kinds[fmt.Sprint(len(r.Columns), " columns")]++
				seen := map[string]bool{}
				for _, c := range r.Columns {
					before := false
					for _, n := range named {
						before = before || n.Fold() == c.Fold()
					}
					kinds[fmt.Sprint("named before ", before)]++
					kinds[fmt.Sprint("again ", seen[c.Fold()])]++
					seen[c.Fold()] = true
				}
			}
		}
		return kinds
	}

	t.Run("should write RETURNING only with Returning", func(t *testing.T) {
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(21, 22)), pools)
		g.Parameters, g.Columns, g.Nulls, g.Conditions, g.ValueFirst = 3, 3, 3, 4, 3
		kinds := count(g)
		is.Equal(t, 900, kinds["none"], fmt.Sprint(kinds))
	})

	t.Run("should write RETURNING of * and of one or more columns, named before or not, with Returning", func(t *testing.T) {
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(21, 22)), pools)
		g.Parameters, g.Columns, g.Nulls, g.Conditions, g.ValueFirst, g.Returning = 3, 3, 3, 4, 3, 2
		kinds := count(g)
		for _, k := range []string{"none", "*", "1 columns", "2 columns", "3 columns", "4 columns", "named before true", "named before false",
			"again true", "again false"} {
			is.True(t, kinds[k] > 5, fmt.Sprint(k, " ", kinds))
		}
	})

	t.Run("should write RETURNING of * or one column without Columns", func(t *testing.T) {
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(21, 22)), pools)
		g.Returning = 1
		kinds := count(g)
		is.Equal(t, 0, kinds["none"], fmt.Sprint(kinds))
		is.Equal(t, 900, kinds["*"]+kinds["1 columns"], fmt.Sprint(kinds))
	})
}

func TestGenerator_Subquery(t *testing.T) {
	pools, err := stmttest.NamePools()
	is.NotError(t, err)
	// count the selects, updates, and deletes of g, which must each be the statement that the pass
	// gives for its text, by what their subqueries have.
	count := func(g *stmttest.Generator) map[string]int {
		kinds := map[string]int{}
		for _, k := range []stmttest.Kind{stmttest.KindSelect, stmttest.KindUpdate, stmttest.KindDelete} {
			for range 300 {
				src, s, args := g.CallOf(k)
				got, err := stmt.Check(src)
				is.NotError(t, err, src)
				is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
				is.Equal(t, len(args), s.Parameters(), src)
				var table stmt.Name
				var where []stmt.Condition
				var subqueries []stmt.Select
				switch s := s.(type) {
				case *stmt.Select:
					table, where, subqueries = s.Table, s.Where, s.Subqueries
				case *stmt.Update:
					table, where, subqueries = s.Table, s.Where, s.Subqueries
				case *stmt.Delete:
					table, where, subqueries = s.Table, s.Where, s.Subqueries
				}
				if subqueries == nil {
					kinds["none"]++
					continue
				}
				kinds[fmt.Sprint(len(subqueries), " subqueries")]++
				for _, c := range where {
					if c.Value.Subquery > 0 {
						kinds[fmt.Sprint("value first ", c.ValueFirst)]++
					}
				}
				for _, q := range subqueries {
					kinds[fmt.Sprint("table of the statement ", q.Table.Fold() == table.Fold())]++
					kinds[fmt.Sprint("* ", q.Columns == nil)]++
					kinds[fmt.Sprint("where ", q.Where != nil)]++
					kinds[fmt.Sprint("order by ", q.OrderBy != nil)]++
					kinds[fmt.Sprint("limit ", q.Limited())]++
					kinds[fmt.Sprint("parameters ", q.Parameters() > 0)]++
					is.True(t, q.Subqueries == nil, src)
				}
			}
		}
		return kinds
	}

	t.Run("should write a subquery only with Subquery", func(t *testing.T) {
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(23, 24)), pools)
		g.Parameters, g.Columns, g.Nulls, g.Conditions, g.ValueFirst, g.OrderBy = 3, 3, 3, 4, 3, 2
		kinds := count(g)
		is.Equal(t, 900, kinds["none"], fmt.Sprint(kinds))
	})

	t.Run("should write subqueries on either side of a column, of the table of the statement or another, with the clauses of a select, with Subquery", func(t *testing.T) {
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(23, 24)), pools)
		g.Parameters, g.Columns, g.Nulls, g.Conditions, g.ValueFirst, g.OrderBy, g.Subquery = 3, 3, 3, 4, 3, 2, 2
		kinds := count(g)
		for _, k := range []string{"none", "1 subqueries", "2 subqueries", "value first true", "value first false", "table of the statement true",
			"table of the statement false", "* true", "* false", "where true", "where false", "order by true", "limit true", "parameters true"} {
			is.True(t, kinds[k] > 5, fmt.Sprint(k, " ", kinds))
		}
	})
}

func TestGenerator_Expression(t *testing.T) {
	pools, err := stmttest.NamePools()
	is.NotError(t, err)
	// count the selects, updates, and deletes of g, which must each be the statement that the pass
	// gives for its text, by where their expressions are and what they hold.
	count := func(g *stmttest.Generator) map[string]int {
		kinds := map[string]int{}
		var walk func(e stmt.Expression)
		walk = func(e stmt.Expression) {
			kinds[e.Kind.String()+" node"]++
			for _, o := range e.Operands {
				walk(o)
			}
		}
		for _, k := range []stmttest.Kind{stmttest.KindSelect, stmttest.KindUpdate, stmttest.KindDelete} {
			for range 300 {
				src, s, args := g.CallOf(k)
				got, err := stmt.Check(src)
				is.NotError(t, err, src)
				is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", got), src)
				is.Equal(t, len(args), s.Parameters(), src)
				var where []stmt.Condition
				switch s := s.(type) {
				case *stmt.Select:
					where = s.Where
				case *stmt.Update:
					where = s.Where
					for _, a := range s.Set {
						if a.Value.Expression.Present() {
							kinds["in set"]++
							walk(a.Value.Expression)
						}
					}
				case *stmt.Delete:
					where = s.Where
				}
				for _, c := range where {
					if c.Expression.Present() {
						kinds[fmt.Sprint("tested, value first ", c.ValueFirst)]++
						walk(c.Expression)
					}
					if e := c.Value.Expression; e.Present() {
						kinds[fmt.Sprint("value with a column ", e.HasColumn(), ", value first ", c.ValueFirst)]++
						walk(e)
					}
				}
			}
		}
		return kinds
	}

	t.Run("should write an expression only with Expression", func(t *testing.T) {
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(25, 26)), pools)
		g.Parameters, g.Columns, g.Nulls, g.Conditions, g.Changes, g.ValueFirst = 3, 3, 3, 4, 3, 3
		is.Equal(t, 0, len(count(g)))
	})

	t.Run("should write expressions in set and on either side of a comparison, with every kind of node, with Expression", func(t *testing.T) {
		g := stmttest.NewGenerator(rand.New(rand.NewPCG(25, 26)), pools)
		g.Parameters, g.Columns, g.Nulls, g.Conditions, g.Changes, g.ValueFirst, g.Subquery, g.Expression = 3, 3, 3, 4, 3, 3, 4, 2
		kinds := count(g)
		for _, k := range []string{"in set", "tested, value first true", "tested, value first false", "value with a column true, value first true",
			"value with a column true, value first false", "value with a column false, value first true", "value with a column false, value first false",
			"value node", "parameter node", "column node", "+ node", "- node", "* node", "/ node"} {
			is.True(t, kinds[k] > 5, fmt.Sprint(k, " ", kinds))
		}
	})
}
