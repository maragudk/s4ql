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

// Declared holds statements written by hand for the rules by which SQLite records what a CREATE
// TABLE, an ALTER TABLE ... ADD, a CREATE INDEX, or a CREATE VIEW declares, which the check
// compares with what the tree declares.
var Declared = []string{
	// GENERATED and ALWAYS as words of a type, and a type of 16 bytes or more that loses them.
	"create table t(a int generated always as (1), b)",
	"create table t(a intgenerated always as (1), b)",
	"create table t(a generated always as (1), b)",
	"create table t(a always generated always as (1), b)",
	"create table t(e int                         Always default(5), f int Always, g x generated)",
	"create table t(a int(5) generated always as (1), b)",
	"create table t(a int as (1), b text always as (a) stored, c real generated always as (b), d)",
	// Conflict clauses, on the constraint before them.
	"create table t(a not null on conflict ignore unique, b primary key on conflict replace not null, c unique on conflict fail check (c > 0), d, unique (d) on conflict rollback, check (d > 0))",
	// The text of a type, dequoted as sqlite3AddColumn does, and the standard types.
	"create table t(a [int] x, b 'x' y, c \"in\"\"t\", d '', f [int], g \"integer\", h \"\", i Integer, j any)",
	"create table t(a varchar ( 10 , -2 ), b double precision, c unsigned big int, d decimal(10,5), e int(+3))",
	"create table t(a big /* c */ int, b char -- d\n(4))",
	// Defaults, as their text.
	"create table t(a default - 1, b default ( 1 + 2 ) , c default \"abc\", d default x'01', e default -0x1_0, f default true, g default null, h default NULL)",
	"create table t(i default +1.5, j default current_time, k default 'it''s', l default (  'x'  ), m default (/* c */ 1), n default 1 default 2)",
	// Primary keys: the rowid, indexes, order, AUTOINCREMENT, WITHOUT ROWID, and STRICT.
	"create table t(a integer primary key desc, b)",
	"create table t(a integer, b, primary key(a desc))",
	"create table t(a integer, b, primary key(a desc)) without rowid",
	"create table t(a int primary key, b)",
	"create table t(a integer primary key autoincrement, b)",
	"create table t(a, b, primary key(b, a, b)) without rowid",
	"create table t(a, b, primary key(b, a, b))",
	"create table t(a, b, primary key(a, b collate nocase, b)) without rowid",
	"create table t(a text primary key collate nocase, b unique collate rtrim unique)",
	"create table t(a unique, b, unique(a), unique(a collate nocase), primary key(a))",
	"create table t(a integer primary key, b, unique(a)) without rowid",
	"create table t(a int primary key, b text, c any not null, d integer) strict",
	"create table t(a int primary key, b integer) without rowid, strict",
	"create table t('a', \"b\", [c], primary key('a' collate nocase, \"b\"))",
	"create table t(a, b, unique(b desc, a), unique (b, a))",
	// CONSTRAINT names: a name names every CHECK after it, up to the next column or a comma between
	// table constraints, and the one after the last column names the first table constraint.
	"create table t(a constraint c1 check(a > 0) check(a < 5), b check(b > 0))",
	"create table t(a constraint c1, check(a > 0), check(a < 9))",
	"create table t(a, constraint c1 check(a > 0) check(a < 5), constraint c2, check(a <> 3))",
	"create table t(a check( \"a\" ), b check( 'y' > 0 ))",
	"create table t(a constraint \"q\" check(a), constraint [r] constraint s check(a))",
	"create table t(a constraint c1 not null constraint c2 default 1 check(a > 0))",
	// Foreign keys: actions, MATCH, and DEFERRABLE, which applies to the foreign key made last.
	"create table t(a references p, b references p(x) on delete cascade on update set null on delete restrict match simple, c references p(y) deferrable initially deferred, d deferrable initially immediate, e references p not deferrable initially deferred)",
	"create table t(a, b, foreign key(a, b) references p(x, y) on update cascade deferrable initially deferred, foreign key(b) references p on insert set default)",
	"create table t(A references p, foreign key(\"a\") references p(x) on delete no action on update set default)",
	"create table t(a references p deferrable initially deferred, b, foreign key(b) references p deferrable)",
	// Collations and generated columns.
	"create table t(a collate nocase collate rtrim, b as (a) stored, c as (a || 'x') virtual, d)",
	"create table t(a text unique, b collate \"nocase\", c collate [rtrim])",
	// Options, TEMP, another database, and IF NOT EXISTS.
	"create table t(a int), strict",
	"create temp table t(a unique)",
	"create table aux.t(a references p, b primary key)",
	"create table if not exists t(a)",
	// ALTER TABLE ... ADD.
	"alter table t1 add column d int default 3 check (d > 0) references t2(a) deferrable initially deferred",
	"alter table t1 add e text collate nocase not null default 'x'",
	"alter table t1 add f int generated always as (1)",
	"alter table t1 add constraint c9 check (a > 0)",
	"alter table t1 add check ( a > 0 )",
	// CREATE INDEX and CREATE VIEW.
	"create unique index i on t1(a collate nocase desc, b + 1 asc, lower(c), 'b') where a > 0",
	"create index i on t1((a) collate rtrim, \"b\" desc, c collate binary collate nocase)",
	"create view v(x, \"y\", [z]) as select 1, 2, 3",
}
