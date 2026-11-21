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
}
