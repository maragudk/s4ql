package shapetest_test

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/shapetest"
)

func TestPrint(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"should put every binary operation in parentheses, by its grouping", "select 1 + 2 * 3 - 4", "select ( ( 1 + ( 2 * 3 ) ) - 4 )"},
		{"should group comparisons as SQLite does", "select a = b < c", "select ( a = ( b < c ) )"},
		{"should put NOT around NOT NULL", "select not x not null", "select ( not ( x not null ) )"},
		{"should put a unary operator in parentheses with its operand", "select -x collate nocase", "select ( ( - x ) collate nocase )"},
		{"should end BETWEEN at its AND", "select x between a and b and c", "select ( ( x between a and b ) and c )"},
		{"should write IS NOT DISTINCT FROM as one operator", "select a is not distinct from b", "select ( a is not distinct from b )"},
		{"should write LIKE with its ESCAPE", "select a not like b escape c", "select ( a not like b escape c )"},
		{"should write the postfix forms", "select a isnull, b notnull, c not null", "select ( a isnull ) , ( b notnull ) , ( c not null )"},
		{"should write IN with a list, a select, and a table", "select a in (1, 2), b not in (select 1), c in t, d in f(1)", "select ( a in ( 1 , 2 ) ) , ( b not in ( select 1 ) ) , ( c in t ) , ( d in f ( 1 ) )"},
		{"should put CASE in parentheses", "select case a when 1 then 2 else 3 end", "select ( case a when 1 then 2 else 3 end )"},
		{"should not put a name, literal, or call in parentheses", "select a, 1, 'x', ?1, f(a), cast(a as int), (select 1), exists (select 1)", "select a , 1 , 'x' , ?1 , f ( a ) , cast ( a as int ) , ( select 1 ) , exists ( select 1 )"},
		{"should keep the parentheses of a row value", "select (a, b) = (1, 2)", "select ( ( a , b ) = ( 1 , 2 ) )"},
		{"should keep parentheses around a name", "select (a), ((b))", "select ( a ) , ( b )"},
		{"should not double the parentheses around an operation", "select ((a + b)) * c", "select ( ( a + b ) * c )"},
		{"should write a function call with DISTINCT, ORDER BY, FILTER, and OVER", "select count(distinct a order by b) filter (where c) over (partition by d order by e rows between 1 preceding and current row exclude ties)",
			"select count ( distinct a order by b ) filter ( where c ) over ( partition by d order by e rows between 1 preceding and current row exclude ties )"},
		{"should write a qualified name without spaces around its dots", "select s.t.c, t.*", "select s.t.c , t.*"},
		{"should add AS before an alias", "select a b from t x", "select a AS b from t AS x"},
		{"should keep LIMIT with a comma as written", "select 1 limit 1 + 1, 2", "select 1 limit ( 1 + 1 ) , 2"},
		{"should write each ON after its join", "select * from a join b on a.x = b.x left join c using (y)", "select * from a join b on ( a.x = b.x ) left join c using ( y )"},
		{"should write a join in parentheses", "select * from (a join b) join c", "select * from ( a join b ) join c"},
		{"should write a compound with ORDER BY and LIMIT on its last part", "select 1 union all select 2 order by 1 limit 1", "select 1 union all select 2 order by 1 limit 1"},
		{"should write a WITH clause", "with recursive c(x) as not materialized (select 1) select x from c", "with recursive c ( x ) as not materialized ( select 1 ) select x from c"},
		{"should write VALUES", "values (1, 2), (3, 4)", "values ( 1 , 2 ) , ( 3 , 4 )"},
		{"should write an upsert", "insert into t(a) values (1) on conflict (a) where a > 0 do update set a = a + 1 where a < 9 on conflict do nothing returning *",
			"insert into t ( a ) values ( 1 ) on conflict ( a ) where ( a > 0 ) do update set a = ( a + 1 ) where ( a < 9 ) on conflict do nothing returning *"},
		{"should write the steps of a trigger with a semicolon after each", "create trigger r after insert on t when new.a > 0 begin select 1; delete from t; end",
			"create trigger r after insert on t when ( new.a > 0 ) begin select 1 ; delete from t ; end"},
		{"should keep the spacing of a declared type, which SQLite keeps", "create table t(a varchar ( 10 ), b unsigned  big int)", "create table t ( a varchar ( 10 ) , b unsigned  big int )"},
		{"should write a comma between table constraints only where one was written", "create table t(a, constraint c check (a) unique (a), check (a))",
			"create table t ( a , constraint c check ( a ) unique ( a ) , check ( a ) )"},
		{"should write an empty first table option", "create table t(a), strict", "create table t ( a ) , strict"},
		{"should write a DEFAULT in parentheses with the expression in its own", "create table t(a default (1 + 2), b default -1)", "create table t ( a default ( ( 1 + 2 ) ) , b default - 1 )"},
		{"should write the arguments of a virtual table as tokens", "create virtual table t using m(a b, (c, d), )", "create virtual table t using m ( a b , ( c , d ) , )"},
		{"should keep EXPLAIN", "explain query plan select 1", "explain query plan select 1"},
		{"should keep a number with digit separators as one literal", "select 1_000", "select 1_000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stmt, _, err := parser.Parse(test.src)
			is.NotError(t, err)
			is.Equal(t, test.want, shapetest.Print(stmt))
		})
	}
}
