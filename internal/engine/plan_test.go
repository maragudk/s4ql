package engine_test

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestNewPlan(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{src: `CREATE TABLE "T""1" (V1 INT PRIMARY KEY) WITHOUT ROWID, STRICT`, want: "create table \"T\"\"1\" (V1 INT, primary key (V1))\n"},
		{src: "drop table t1;", want: "drop table t1\n"},
		{src: "insert into t1 (\"v\") values (-  9223372036854775808)", want: "insert into t1 (\"v\")\n  values (-9223372036854775808)\n"},
		{src: "insert into t1 (v1) values ( ? )", want: "insert into t1 (v1)\n  values (?1)\n"},
		{src: "select * from été", want:"query\n  project *\n    scan été\n"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			s, err := stmt.Check(c.src)
			is.NotError(t, err)
			is.Equal(t, c.want, engine.Format(engine.NewPlan(s)))
		})
	}

	t.Run("should hold the names as the statement writes them, and the type and key of the column", func(t *testing.T) {
		s, err := stmt.Check(`create table "a b" (C int primary key) strict, without rowid`)
		is.NotError(t, err)
		p := engine.NewPlan(s).(*engine.CreateTable)
		is.Equal(t, stmt.Name{Raw: `"a b"`, Value: "a b", Offset: 13}, p.Table)
		is.Equal(t, 1, len(p.Columns))
		is.Equal(t, stmt.Name{Raw: "C", Value: "C", Offset: 20}, p.Columns[0].Name)
		is.Equal(t, s4qlpb.Type_TYPE_INT, p.Columns[0].Type)
		is.EqualSlice(t, []int{0}, p.PrimaryKey)
	})

	t.Run("should format a plan with several columns and rows", func(t *testing.T) {
		p := &engine.Insert{
			Table:   stmt.Name{Raw: "t"},
			Columns: []stmt.Name{{Raw: "a"}, {Raw: "b"}},
			Source:  &engine.Values{Rows: [][]engine.Expr{{&engine.Integer{Value: 1}, &engine.Integer{Value: 2}}, {&engine.Integer{Value: 3}, &engine.Integer{Value: 4}}}},
		}
		is.Equal(t, "insert into t (a, b)\n  values (1, 2), (3, 4)\n", engine.Format(p))
		c := &engine.CreateTable{
			Table:      stmt.Name{Raw: "t"},
			Columns:    []engine.ColumnDef{{Name: stmt.Name{Raw: "a"}, Type: s4qlpb.Type_TYPE_INT}, {Name: stmt.Name{Raw: "b"}, Type: s4qlpb.Type_TYPE_INT}},
			PrimaryKey: []int{1, 0},
		}
		is.Equal(t, "create table t (a INT, b INT, primary key (b, a))\n", engine.Format(c))
		q := &engine.Query{Root: &engine.Project{Source: &engine.Scan{Table: stmt.Name{Raw: "t"}}}}
		is.Equal(t, "query\n  project\n    scan t\n", engine.Format(q))
	})
}
