package shapetest

import "testing"

// Slow reports whether the tests run with -short, the race detector, or the address sanitizer, which
// make them several times slower: the tests on the corpus then use every tenth entry, and the other
// tests a smaller sample.
func Slow() bool {
	return testing.Short() || raceEnabled || asanEnabled
}

// Structure holds statements written by hand for the structure of statements, where a generator
// does not fit: which ON or USING belongs to which join, ORDER BY and LIMIT on compounds and in
// subqueries, VALUES, common table expressions, windows, FILTER, upserts, RETURNING, CASE, triggers,
// constraints, indexes, and numbers with digit separators.
var Structure = []string{
	// Joins of every kind, with ON and USING.
	"select * from t1 join t2 on t1.a = t2.a left join t3 on t2.b = t3.b",
	"select * from t1, t2 left outer join t3 using (a) inner join t4 on t4.b = t3.b",
	"select * from t1 right join t2 on t1.a = t2.a full outer join t3 on t3.a = t2.a",
	"select * from t1 cross join t2 join t3 on t3.a = t1.a natural join t4",
	"select * from (t1 join t2 on t1.a = t2.a) left join t3 on t3.b = t2.b",
	"select * from t1 left join (t2 join t3 on t2.b = t3.b) on t1.a = t2.a",
	"select * from t1 left join (t2 left join t3 on t2.b = t3.b) using (a)",
	"select * from t1 natural left join t2 natural right join t3",
	"select * from t1 as x join t1 as y on x.a = y.b join t1 z on z.a = y.a and z.b = x.b",
	"select * from t1 join t2 on t1.a = t2.a join t3 on t2.b = t3.b and t1.c = t3.c left join t4 on t4.a = t1.a",
	"select * from t1 indexed by i1 join t2 not indexed on t1.a = t2.a",
	// ORDER BY and LIMIT on compounds, in subqueries, and LIMIT with a comma.
	"select a from t1 union select b from t2 order by 1 limit 2 offset 1",
	"select a from t1 union all select b from t2 except select c from t3 intersect select a from t1 order by 1 desc limit 3",
	"select * from (select a from t1 order by a limit 2) order by 1 desc limit 1",
	"select (select a from t1 order by b limit 1), (select b from t1 order by a desc limit 1 offset 2)",
	"select a from t1 limit 5, 10",
	"select a from t1 limit 10 offset 5",
	"select a from t1 limit (select count(*) from t2), 1 + 1",
	"select a from t1 where a in (select b from t2 order by b limit 3) order by a nulls last",
	"select a, b from t1 order by a desc nulls first, b asc",
	// VALUES, alone, in compounds, and in common table expressions.
	"values (1, 2), (3, 4) union select a, b from t1",
	"select * from (values (1), (2)) union all values (3), (4)",
	"values (1), (2) union all select 3 order by 1",
	"with v(x) as (values (1), (2)) select x from v",
	"insert into t1(a, b) values (1, 2), (3, 4)",
	// Common table expressions: recursive, materialized, nested, and before DML.
	"with recursive c(x) as (select 1 union all select x + 1 from c where x < 10) select x from c",
	"with a as materialized (select 1 as x), b as not materialized (select x from a) select * from b",
	"with a(x) as (with b(y) as (select 2) select y from b) select x from a",
	"with c(x) as (select a from t1) insert into t2(a) select x from c",
	"with c as (select 1) update t1 set a = (select * from c)",
	"with c as (select a from t2) delete from t1 where a in c",
	// Windows, frames, named windows, FILTER, and ORDER BY in an aggregate.
	"select sum(a) over (partition by b order by a rows between 1 preceding and current row exclude ties) from t1",
	"select sum(a) over w, max(a) over (w order by b) from t1 window w as (partition by b)",
	"select count(*) over w2 from t1 window w1 as (partition by a), w2 as (w1 order by b range between unbounded preceding and unbounded following)",
	"select sum(a) filter (where b > 1) over (order by a groups 2 preceding exclude current row) from t1",
	"select first_value(a) over (order by b rows between current row and 2 following exclude no others) from t1",
	"select group_concat(a, ',' order by b desc) from t1",
	"select count(distinct a) filter (where a > 0) from t1",
	"select max(abs(a)) filter (where b > 0) over (order by c) from t1",
	// Upserts with several targets, and INSERT ... SELECT ... ON CONFLICT.
	"insert into t1(a, b) values (1, 2) on conflict (a) where a > 0 do update set b = excluded.b where b < 10 on conflict (b) do nothing on conflict do nothing",
	"insert into t1(a, b) select a, b from t2 where true on conflict (a) do update set b = 1",
	"insert into t1(a) values (1) on conflict do update set a = a + 1 returning a, b * 2 as c",
	"insert or replace into t1 default values",
	"replace into t1(a) select a from t2",
	// RETURNING.
	"update t1 set a = 1, (b, c) = (select 1, 2) where b = 2 returning *, a + 1",
	"update or ignore t1 set a = t2.b from t2 where t1.c = t2.c returning t1.a",
	"delete from t1 where a = 1 returning a",
	// CASE with and without an operand.
	"select case a when 1 then 'x' when 2 then 'y' else 'z' end, case when a = 1 then case when b then 1 end else 2 end from t1",
	// Triggers with several steps.
	"create trigger r after update of a on t1 for each row when new.a > old.a begin insert into t2(a) values (new.a); update t3 set b = new.a where a = old.a; delete from t2 where a = new.a; select raise(abort, 'x') where new.a < 0; end",
	"create trigger r before delete on t1 begin insert into t2 select * from t3 where t3.a = old.a; end",
	"create trigger r instead of insert on v1 begin insert into t1(a) values (new.x); end",
	// Column and table constraints with conflict clauses, DEFERRABLE, foreign key actions, and
	// generated columns.
	"create table t(a integer primary key asc on conflict replace autoincrement, b text not null on conflict fail unique on conflict ignore check (b <> '') default 'x' collate nocase, c references p(x) on delete cascade on update set null match full deferrable initially deferred, d as (a * 2) stored, e int generated always as (b || 'y') virtual)",
	"create table t(a, b, c, constraint c1 check (a > 0) constraint c2 unique (b, c) on conflict abort, foreign key (c) references p(x) on delete restrict not deferrable, primary key (a desc, b) on conflict rollback, check (b < c))",
	"create table t(a int default -1, b real default (1 + 2), c text default current_timestamp, d text default 'x', e int not null default 0) strict",
	"create table t(a primary key, b) without rowid",
	"create table t as select a, b + 1 as c from t1 where a > 0",
	"alter table t1 add column d int default 3 check (d > 0) references t2(a)",
	"alter table t1 rename column a to z",
	// Index definitions with expressions, COLLATE, order, and WHERE.
	"create unique index i on t1(a collate nocase desc, b + 1 asc, lower(c)) where a > 0 and b is not null",
	"create index if not exists i on t1(a) where a not between 1 and 2",
	// Views.
	"create view v(x, y) as select a, b * 2 from t1 where a > 0 order by 2 limit 1",
	// The text of an unnamed CHECK as SQLite takes it, dequoted and with comments; primary keys and
	// generated columns in the probes; INDEXED BY on a table that is not the first.
	"create table t(a check(\"a\" > 0), b check (b > 0 /* c */))",
	"create table t(a integer primary key, b, g as (b + 1))",
	"create trigger r after update on t1 begin update t2 set a = new.a; end",
	"select * from t1, t2 indexed by i where t1.a = t2.a",
	// Numbers with digit separators stay one literal.
	"select 1_000, 1_000.5_0e1_0, 0x1_F, a from t1 where a > 1_0",
}
