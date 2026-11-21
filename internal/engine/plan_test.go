package engine_test

import (
	"fmt"
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
		{src: "insert into t (v) values (1.25)", want: "insert into t (v)\n  values (1.25)\n"},
		{src: "insert into t (v) values ('it''s')", want: "insert into t (v)\n  values ('it''s')\n"},
		{src: "insert into t (v) values (X'0aFF')", want: "insert into t (v)\n  values (x'0aff')\n"},
		{src: "select * from été", want: "query\n  project *\n    scan été\n"},
		{src: "create table t1 (v1 int not null, v2 text primary key, v3 real not null) strict, without rowid", want: "create table t1 (v1 INT, v2 TEXT, v3 REAL, primary key (v2))\n"},
		{src: "create table t1 (v1 int primary key not null, v2 blob not null) strict, without rowid", want: "create table t1 (v1 INT, v2 BLOB, primary key (v1))\n"},
		{src: "insert into t1 values (1, 'a', ?)", want: "insert into t1\n  values (1, 'a', ?1)\n"},
		{src: `insert into t1 (v3, "V1", v2) values (?, 2.5, ?)`, want: "insert into t1 (v3, \"V1\", v2)\n  values (?1, 2.5, ?2)\n"},
		{src: `select v2, "V1", v2 from t1`, want: "query\n  project v2, \"V1\", v2\n    scan t1\n"},
		{src: "select v from t", want: "query\n  project v\n    scan t\n"},
		{src: "create table t1 (v1 int, v2 text primary key, v3 real not null, v4 blob) strict, without rowid", want: "create table t1 (v1 INT NULL, v2 TEXT, v3 REAL, v4 BLOB NULL, primary key (v2))\n"},
		{src: "insert into t1 values (NULL, ?, null)", want: "insert into t1\n  values (null, ?1, null)\n"},
		{src: "select * from t1 where v1 = 1", want: "query\n  project *\n    filter v1 = 1\n      scan t1\n"},
		{src: `select v2 from t1 where v1 >= ? and "V1" <> -2.5 and v2 is not null and v1 == ? and v3 is null and v4 < x'00' and v4 > 'a''' and v4 <= null`,
			want: "query\n  project v2\n    filter v1 >= ?1 and \"V1\" != -2.5 and v2 IS NOT NULL and v1 = ?2 and v3 IS NULL and v4 < x'00' and v4 > 'a''' and v4 <= null\n      scan t1\n"},
		{src: "update t1 set v2 = 'b', V3 == 2.5", want: "update t1 set v2 = 'b', V3 = 2.5\n  scan t1\n"},
		{src: `update "T1" set v1 = ?, v2 = null where v1 = ? and v2 is not null`, want: "update \"T1\" set v1 = ?1, v2 = null\n  filter v1 = ?2 and v2 IS NOT NULL\n    scan \"T1\"\n"},
		{src: "delete from t1", want: "delete from t1\n  scan t1\n"},
		{src: "DELETE FROM t1 WHERE v1 >= 10 AND v2 < ?", want: "delete from t1\n  filter v1 >= 10 and v2 < ?1\n    scan t1\n"},
		{src: `CREATE INDEX "I 1" ON t1 (v2 DESC, "V1" asc, v3)`, want: "create index \"I 1\" on t1 (v2 desc, \"V1\", v3)\n"},
		{src: "create index i on t (v)", want: "create index i on t (v)\n"},
		{src: `DROP INDEX "I 1"`, want: "drop index \"I 1\"\n"},
		{src: "create table t (k int default - /* c */ 1 primary key, a integer not null default 00012, b real default .5, c real default -0.0, d text default 'it''s', e blob default X'aBcD', f int default NULL, g text not null default null, h int) strict, without rowid",
			want: "create table t (k INT DEFAULT -1, a INTEGER DEFAULT 12, b REAL NULL DEFAULT 0.5, c REAL NULL DEFAULT -0.0, d TEXT NULL DEFAULT 'it''s', e BLOB NULL DEFAULT x'abcd', f INT NULL DEFAULT null, g TEXT DEFAULT null, h INT NULL, primary key (k))\n"},
		{src: "create index i on t (a, b desc)", want: "create index i on t (a, b desc)\n"},
		{src: "drop index I", want: "drop index I\n"},
		{src: `CREATE TABLE If Not Exists "t" (v int primary key) strict, without rowid`, want: "create table if not exists \"t\" (v INT, primary key (v))\n"},
		{src: "drop table IF EXISTS t1", want: "drop table if exists t1\n"},
		{src: "create index if not exists i on t (a desc)", want: "create index if not exists i on t (a desc)\n"},
		{src: "drop index if exists i", want: "drop index if exists i\n"},
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

	t.Run("should hold the conditions of a WHERE in a filter below the projection, in the order of the text, with no value for a test for NULL", func(t *testing.T) {
		s, err := stmt.Check("select b from t where a is null and B = ? and a != 'x'")
		is.NotError(t, err)
		p := engine.NewPlan(s).(*engine.Query).Root.(*engine.Project)
		f := p.Source.(*engine.Filter)
		is.Equal(t, "t", f.Source.(*engine.Scan).Table.Value)
		is.Equal(t, 3, len(f.Conditions))
		is.Equal(t, "a IS NULL <nil>", fmt.Sprint(f.Conditions[0].Column.Value, " ", f.Conditions[0].Operator, " ", f.Conditions[0].Value))
		is.Equal(t, "B = 1", fmt.Sprint(f.Conditions[1].Column.Raw, " ", f.Conditions[1].Operator, " ", f.Conditions[1].Value.(*engine.Parameter).Number))
		is.Equal(t, "a != x", fmt.Sprint(f.Conditions[2].Column.Raw, " ", f.Conditions[2].Operator, " ", f.Conditions[2].Value.(*engine.Text).Value))
		s, err = stmt.Check("select * from t")
		is.NotError(t, err)
		_, ok := engine.NewPlan(s).(*engine.Query).Root.(*engine.Project).Source.(*engine.Scan)
		is.True(t, ok, "a select without WHERE has no filter")
	})

	t.Run("should hold every column in order, with the key at its position, and the same plan with and without NOT NULL on the key", func(t *testing.T) {
		with, err := stmt.Check("create table t (a text not null, b int not null primary key, c blob not null) strict, without rowid")
		is.NotError(t, err)
		without, err := stmt.Check("create table t (a text not null, b int primary key, c blob not null) strict, without rowid")
		is.NotError(t, err)
		p := engine.NewPlan(with).(*engine.CreateTable)
		is.Equal(t, 3, len(p.Columns))
		for i, want := range []string{"a", "b", "c"} {
			is.Equal(t, want, p.Columns[i].Name.Value)
			is.True(t, !p.Columns[i].Nullable, want)
		}
		is.EqualSlice(t, []int{1}, p.PrimaryKey)
		is.Equal(t, engine.Format(p), engine.Format(engine.NewPlan(without)))
	})

	t.Run("should hold a column that is not the key and does not say NOT NULL as one that takes NULL, of any type and at any position, and the literal NULL as its own expression", func(t *testing.T) {
		s, err := stmt.Check("create table t (a text, b int not null, c blob primary key, d real, e integer) strict, without rowid")
		is.NotError(t, err)
		p := engine.NewPlan(s).(*engine.CreateTable)
		var nullable []bool
		for _, c := range p.Columns {
			nullable = append(nullable, c.Nullable)
		}
		is.EqualSlice(t, []bool{true, false, false, true, true}, nullable)
		s, err = stmt.Check("insert into t (a) values (null)")
		is.NotError(t, err)
		_, ok := engine.NewPlan(s).(*engine.Insert).Source.(*engine.Values).Rows[0][0].(*engine.Null)
		is.True(t, ok)
	})

	t.Run("should hold the columns of an insert as nil without a list, and of a select as nil for *", func(t *testing.T) {
		s, err := stmt.Check("insert into t values (1, 2)")
		is.NotError(t, err)
		is.True(t, engine.NewPlan(s).(*engine.Insert).Columns == nil)
		s, err = stmt.Check("select * from t")
		is.NotError(t, err)
		p := engine.NewPlan(s).(*engine.Query).Root.(*engine.Project)
		is.True(t, p.Star)
		is.True(t, p.Columns == nil)
		s, err = stmt.Check("select a, a from t")
		is.NotError(t, err)
		p = engine.NewPlan(s).(*engine.Query).Root.(*engine.Project)
		is.True(t, !p.Star)
		is.Equal(t, 2, len(p.Columns))
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
