package engine_test

import (
	"math"
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
		{src: "create table t (v integer primary key) strict, without rowid", want: "create table t (v INTEGER, primary key (v))\n"},
		{src: "create table t (v real primary key) strict, without rowid", want: "create table t (v REAL, primary key (v))\n"},
		{src: "create table t (v text primary key) strict, without rowid", want: "create table t (v TEXT, primary key (v))\n"},
		{src: "create table t (v blob primary key) strict, without rowid", want: "create table t (v BLOB, primary key (v))\n"},
		{src: "insert into t (v) values (1e300)", want: "insert into t (v)\n  values (1e+300)\n"},
		{src: "insert into t (v) values (-0.0)", want: "insert into t (v)\n  values (-0.0)\n"},
		{src: "insert into t (v) values (2.)", want: "insert into t (v)\n  values (2.0)\n"},
		{src: "insert into t (v) values ('it''s')", want: "insert into t (v)\n  values ('it''s')\n"},
		{src: "insert into t (v) values (X'0aFF')", want: "insert into t (v)\n  values (x'0aff')\n"},
		{src: "select * from été", want: "query\n  project *\n    scan été\n"},
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

	t.Run("should keep each type of a column as declared, INT and INTEGER apart", func(t *testing.T) {
		for src, want := range map[string]s4qlpb.Type{
			"create table t (v int primary key) strict, without rowid":     s4qlpb.Type_TYPE_INT,
			"create table t (v INTEGER primary key) strict, without rowid": s4qlpb.Type_TYPE_INTEGER,
			"create table t (v real primary key) strict, without rowid":    s4qlpb.Type_TYPE_REAL,
			"create table t (v text primary key) strict, without rowid":    s4qlpb.Type_TYPE_TEXT,
			"create table t (v Blob primary key) strict, without rowid":    s4qlpb.Type_TYPE_BLOB,
		} {
			s, err := stmt.Check(src)
			is.NotError(t, err)
			is.Equal(t, want, engine.NewPlan(s).(*engine.CreateTable).Columns[0].Type, src)
		}
	})

	t.Run("should hold a literal of each type as its value", func(t *testing.T) {
		for src, want := range map[string]engine.Expr{
			"insert into t (v) values (1.5)":     &engine.Real{Value: 1.5},
			"insert into t (v) values (-0.0)":    &engine.Real{Value: math.Copysign(0, -1)},
			"insert into t (v) values ('a''')":   &engine.Text{Value: "a'"},
			"insert into t (v) values (x'')":     &engine.Blob{Value: []byte{}},
			"insert into t (v) values (x'6100')": &engine.Blob{Value: []byte("a\x00")},
		} {
			s, err := stmt.Check(src)
			is.NotError(t, err)
			got := engine.NewPlan(s).(*engine.Insert).Source.(*engine.Values).Rows[0][0]
			is.Equal(t, engine.Format(&engine.Insert{Source: &engine.Values{Rows: [][]engine.Expr{{want}}}}), engine.Format(&engine.Insert{Source: &engine.Values{Rows: [][]engine.Expr{{got}}}}), src)
			if r, ok := got.(*engine.Real); ok {
				is.Equal(t, math.Float64bits(want.(*engine.Real).Value), math.Float64bits(r.Value), src)
			}
			if b, ok := got.(*engine.Blob); ok {
				is.True(t, b.Value != nil, src)
			}
		}
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
