package engine_test

import (
	"testing"

	"maragu.dev/s4ql/internal/sql/stmt"
)

// subquerySetUp is the tables of the tests of subqueries: t and u, each with a column of each type
// that a comparison meets, and goqite's table with its index.
var subquerySetUp = []string{
	"create table t (k int primary key, v text, w int, r real, b blob) strict, without rowid",
	"create table u (k integer primary key, x text, y int not null) strict, without rowid",
	"create table u1 (k int primary key) strict, without rowid",
	"insert into t values (1, 'a', 0, 1.5, x'01')",
	"insert into t values (2, 'b', 0, 2.5, x'02')",
	"insert into t values (3, 'c', 1, null, null)",
	"insert into u values (1, 'b', 7)",
	"insert into u values (2, 'c', 7)",
	"insert into u1 values (2)",
}

func TestEngine_subquerySmoke(t *testing.T) {
	cases := []struct{ src, want string }{
		{"select * from t where k = (select k from u where x = 'b')", `columns k INT, v TEXT, w INT, r REAL, b BLOB; rows 1,"a",0,real 1.5,x'01'`},
		{"select k from t where v = (select x from u where k = 2)", "columns k INT; rows 3"},
		{"select k from t where k = (select k from u where k = 5)", "columns k INT; rows "},
		{"select k from t where k = (select * from u1)", "columns k INT; rows 2"},
		{"select k from t where k = (select * from u)", "1/1: row value misused"},
		{"select k from t where nope = (select k from nope2)", "1/1: no such table: nope2"},
		{"select k from t where k = (select nope from u)", "1/1: no such column: nope"},
		{"select k from t where (select k from u where y = 7 order by x desc limit 1) < k", "columns k INT; rows 3"},
		{"select k from t where k = (select k from u order by k limit 0)", "columns k INT; rows "},
	}
	for _, c := range cases {
		pin(t, subquerySetUp, c.src, c.want)
	}
	pin(t, subquerySetUp, "update t set w = 9 where k = (select k from t where w = 0 order by k limit 1)", "")
	pin(t, subquerySetUp, "update t set w = 9 where w = (select w from t order by k limit 1)", "")
	pin(t, subquerySetUp, "delete from t where k = (select * from u)", "1/1: sub-select returns 3 columns - expected 1")
	pin(t, subquerySetUp, "delete from t where (select * from u) != k", "1/1: row value misused")
}

// subqueried reports whether s is a select, an update, or a delete with a scalar subquery.
func subqueried(s stmt.Statement) bool {
	switch s := s.(type) {
	case *stmt.Select:
		return s.Subqueries != nil
	case *stmt.Update:
		return s.Subqueries != nil
	case *stmt.Delete:
		return s.Subqueries != nil
	}
	return false
}
