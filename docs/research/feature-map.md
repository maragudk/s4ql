# A map of SQL features

Date: 2026-11-24. Read: `main` at commit `2175351`: [internal/sql/stmt/README.md](../../internal/sql/stmt/README.md), [internal/engine/README.md](../../internal/engine/README.md), [the roadmap](../roadmap.md), [the spec](../spec.md), [the todo](../todo.md) and its files, and [the glue note](glue-on-s4ql.md).

Question: which SQL does s4ql support, which does it lack, and which missing pieces of the engine do most of the missing features wait on? The map groups features by area, as the basis for deciding the core gaps one at a time.

How to read the tables:

- **State:** `built`; `building` (the scalar subquery: its statement pass is built, and its engine step is under way); `open` (not built, and undecided); `rejected` (rejected for now, with where that was decided).
- **Glue:** the places in glue that need the feature, production / tests, as [the glue note](glue-on-s4ql.md) counts them. **[A]** marks a place that a change in glue or its dependencies could remove instead. "no" means glue does not need it.
- **Gap:** the core gap below that the feature waits on.

## The core gaps

Ordered by how many rows of the tables wait on each.

### 1. Expressions (23 rows)

- **Today:** the engine has no expression evaluator. An expression in a plan is a literal of five kinds or a `?` (`Expr` in `internal/engine/plan.go`), and `eval` reads its value. A `where` is comparisons of one column with one such value, joined by `and`. A `set`, a `values`, a `default`, and a `limit` each take one such value.
- **Glue:** `+` 1; `count(*)` 0/5; expression defaults 5 [A]; `strftime` 5 [A]; `julianday` 4 [A]; `lower`, `hex`, `randomblob`, and `||` 1 [A]; `select 1` 1/1, [A] in production.
- **Decide:**
  - Where expressions are evaluated: (a) an evaluator of our own in Go, judged against SQLite; (b) in the cache's SQLite, in statements that s4ql writes, as the cache sorts rows today. Option (b) touches the spec's rule "SQLite does not run the application's SQL."
  - Which operators and functions come first: (a) what glue needs; (b) SQLite's core scalar functions; (c) another set.
  - How exact types apply inside an expression: (a) refuse mixed types, such as `'1' + 1`; (b) convert as SQLite does.
  - Time and randomness. Commits are row-based, so replay never runs a function again ([spec](../spec.md)). Open: where `'now'` and random bytes come from (the host, whose clock and randomness the simulation drives), and whether `'now'` is one value per statement, as SQLite gives it within one step. The spec says that the annotation keeps "the time and random seeds", and `commit.proto` has no field for them yet. Options: (a) take both from the host and record them in the annotation; (b) refuse `'now'`, `current_*`, `random()`, and `randomblob`, and leave times and ids to the application, as glue could [A]; (c) support one of the two.

### 2. Query shapes (18 rows)

- **Today:** a statement reads one table. The operators are `scan`, `filter`, `limit`, `values`, and `project`. No operator sorts, groups, or joins: the cache's SQLite sorts the rows of a scan (engine README, "The operators"). The only subquery is the scalar one in a `where`, which is being built.
- **Glue:** the scalar subquery 1 (building); `exists` 1 [A]; a subquery in `from` with `union` and `in` 0/2 [A]; `select` without `from` 1/1, [A] in production; `as` aliases 0/2; `count(*)` 0/5.
- **Decide:**
  - Which shapes come, and in which order: (a) those that glue needs: `exists`, `select 1`, aliases, `count(*)`, and `union`; (b) also those of an ordinary application: joins, `group by`, `offset`, `distinct`, and `in (select ...)`.
  - Where sorting, grouping, and joining run: (a) operators of our own in Go; (b) the cache's SQLite, as sorting runs today.
  - Joins: comma joins and `natural join` are rejected for now ([roadmap](../roadmap.md#restrictions)). Which forms of `join` come is undecided.

### 3. Schema (13 rows)

- **Today:** `create table` and `drop table` of a strict, without-rowid table with one key column, and indexes that are not unique, on columns. No other constraint than `not null` and the key, no trigger, no view, and no `alter table`. The spec says "Strict tables only"; `without rowid` is a restriction of the roadmap. Replay applies results only, so it must not run triggers or cascades, and it cannot check foreign keys row by row ([storage-formats.md](../todo/storage-formats.md)).
- **Glue:** 6 tables that are not `strict, without rowid` [A]; no primary key on `migrations` 1 [A, in migrate]; a trigger 1 [A]; `sqlite_master` 0/1 [A].
- **Decide:**
  - Does `without rowid` stay a restriction: (a) yes, and glue changes its tables; (b) no, and s4ql takes tables with a rowid.
  - Triggers: (a) the engine runs them, and the commit records their effects as rows; (b) rejected.
  - Reading the schema: (a) `sqlite_schema`, read-only; (b) `pragma table_info` and its kin; (c) neither.
  - Which constraints (`unique`, `check`, foreign keys) and which forms of `alter table` come, and in which order.

### 4. Writes (6 rows)

- **Today:** an insert writes exactly one row. A plan of several rows or of none is an internal error (engine README, "The operators"). The storage already has `upsert`, which inserts a row or replaces the row of its key; `update` uses it.
- **Glue:** `REPLACE INTO`, in 3 statements with `$1` [A].
- **Decide:**
  - Several rows per `insert`, `insert ... select`, and `default values`: which come.
  - Upserts, rejected for now ([roadmap](../roadmap.md#restrictions)): (a) keep them rejected; (b) `on conflict ... do update` and `do nothing` only, the kickoff's idea of "upsert only"; (c) also `replace` and `insert or ...`.

### 5. Driver (5 rows)

- **Today:** one statement per text, and a second is "not supported" (decided by the lead on 2026-11-18, "for now"). Only the plain `?`. Arguments are integers, floats, strings, a `[]byte` that is not nil, and nil. While rows of a transaction are open, every statement of the transaction is refused, and so is a read on a `sql.Conn` with open rows: `database/sql` holds the connection's mutex during a call, and a scan holds a statement of SQLite between rows ([driver.md](../todo/driver.md), [slice-03.md](../todo/slice-03.md)).
- **Glue:** several statements in one text 3+ [A only if migrate and the fixture loader split them]; `$1` 3 [A].
- **Decide:**
  - Several statements per text: (a) run them in order, and say what a failure halfway leaves; (b) keep one.
  - Which other forms of a parameter come: `?1`, `:name`, `@name`, `$name`, `sql.Named`.
  - Statements while rows are open: (a) a scan that holds no statement of SQLite between rows and seeks again by its last key, which [slice-03.md](../todo/slice-03.md) records for the lease, not built; (b) keep refusing.
  - `bool` and `time.Time`: which come, and as which column type, under exact types.

### 6. Limits of the core (no rows of their own)

- **Today,** from [the todo](../todo.md): the rows of a `returning` are held in memory without a bound ([storage-core.md](../todo/storage-core.md)); a stop does not stop a sort that runs ([storage-core.md](../todo/storage-core.md)); the first phase of `db.Close()` has no bound ([driver.md](../todo/driver.md)); rows of a scan that nobody reads keep the writer's place after the lease is lost ([slice-03.md](../todo/slice-03.md)).
- **Waits:** the features that hold or compute many rows: aggregates, joins, a sort or a grouping in Go, and a large `returning`.
- **Glue:** no. goqite's statements change one row each.
- **Decide:** whether a bound comes before such features or with them: (a) spool rows in the cache, as `KeySet` holds keys; (b) a documented limit in memory; (c) leave the work to the cache's SQLite.

## Data definition

| Feature | State | Glue | Gap |
| --- | --- | --- | --- |
| `create table t (k int primary key, v text) strict, without rowid`, columns `not null` or taking null | built | 14 / 10, with selects of named columns | |
| Types `int`, `integer`, `real`, `text`, `blob`; a value has the exact type of its column | built | yes | |
| `default 0`, a literal | built | 2 / 0 | |
| `default (strftime(...))`, `default current_timestamp` | open | 5 / 0 [A] | expressions |
| `create table if not exists`, `drop table if exists`, and the same for indexes | built | 1 / 0 | |
| `create index i on t (a, b desc)`, `drop index i` | built | 2 / 0 | |
| `drop table t` | built | 0 / 1 | |
| A table that is not `strict`, or that has a rowid | rejected: spec ("Strict tables only"), [roadmap](../roadmap.md#restrictions) (`without rowid`) | 6 tables [A] | schema |
| A table without a primary key | rejected: follows from `without rowid` | 1 [A, in migrate] | schema |
| `autoincrement`, `rowid`, generated integer keys | rejected: follows from `without rowid` | no | |
| A key of several columns, `primary key (a, b)` | open | no | schema |
| `unique`, `create unique index` | open; the roadmap says later, in a slice of its own | no | schema |
| `check (v > 0)` | open | no | schema, expressions |
| Foreign keys, `references u (k)` | open; when they come, always enforced ([roadmap](../roadmap.md#restrictions)) | no | schema |
| A partial index, an index on an expression, `collate` | open | no | schema, expressions |
| `alter table` add, rename, or drop a column | open | no | schema |
| Triggers | open | 1 [A] | schema, expressions |
| Views | open | no | schema, query shapes |
| Reading the schema from `sqlite_schema` or `sqlite_master` | rejected: names that start with `sqlite_` ([roadmap](../roadmap.md#restrictions)) | 0 / 1 [A] | schema |
| Reading the schema with `pragma table_info(t)` or `pragma_table_info` | open; the `pragma_*` table names are refused by a decision of the builder of the pass, not confirmed ([sql-front-end.md](../todo/sql-front-end.md)) | no | schema |
| `create temp table`, `create table ... as select` | open | no | schema, query shapes |
| `pragma` that changes behaviour | rejected: no settings, for now ([roadmap](../roadmap.md#restrictions)) | no | |

## Reading

| Feature | State | Glue | Gap |
| --- | --- | --- | --- |
| `select a, b from t`, `select * from t` | built | 14 / 10, with the columns | |
| `where a = ? and ? >= b and c is not null`: `=`, `!=`, `<`, `<=`, `>`, `>=`, `is null`, `is not null`, `and`, the value on either side | built | 7 / 3 | |
| `order by a desc, b limit ?` | built | yes, in goqite's receive | |
| A scalar subquery in the `where` of a select, update, or delete: `where id = (select id from t where ... limit 1)` ([slice 15](../roadmap.md#slice-15-a-scalar-subquery-in-where)) | building | 1 / 0 | query shapes |
| `or`, `not`, parentheses | open | no | expressions |
| `in (?, ?)`, `between`, `like`, `glob` | open | 0 / 2 [A], the places of `union` | expressions |
| `a = b`, a column compared with a column | open | no | expressions |
| `t.a`, a column with the name of its table | open | no | query shapes |
| `order by a + 1`, `order by 1`, `nulls first`, `collate nocase` | open | no | expressions |
| `select 1`, a select without `from` | open | 1 / 1, [A] in production | expressions, query shapes |
| `select a + 1`, `select 'x'`: computed result columns | open | 0 / 1 | expressions |
| `select a as x`, `from t as u`: aliases | open | 0 / 2 | query shapes |
| `limit 10 offset 5`, `limit 5, 10` | open | no | query shapes |
| `distinct` | open | no | query shapes |
| `exists (select ...)` | open | 1 [A] | query shapes, expressions |
| `in (select ...)`, a subquery in `from`, a correlated subquery, a subquery in a subquery | open | 0 / 2 [A] | query shapes |
| `count(*)`, `sum`, `min`, `max`, `group by`, `having` | open | 0 / 5 | query shapes, expressions |
| `join ... on`, `left join` | open | no | query shapes |
| Comma joins, `natural join` | rejected: [roadmap](../roadmap.md#restrictions) | no | query shapes |
| `union`, `union all`, `except`, `intersect` | open | 0 / 2 [A] | query shapes |
| `with`, common table expressions | open | no | query shapes |

## Expressions and functions

| Feature | State | Glue | Gap |
| --- | --- | --- | --- |
| Literals of five kinds, `null`, and `?` as values | built | yes | |
| Other literals: `+1`, `0x10`, `1_000`, `true`, `-null` | open | no | expressions |
| `+`, `-`, `*`, `/`, `%` | open | 1 / 0 | expressions |
| `\|\|` | open | 1 [A] | expressions |
| `case`, `cast`, `coalesce`, `ifnull`, `nullif` | open | no | expressions |
| `is` with a value, `is distinct from` | open | no | expressions |
| `lower`, `upper`, `length`, `substr`, `replace`, `trim`, `hex` | open | 1 [A] | expressions |
| `abs`, `round`, and the like | open | no | expressions |
| `random()`, `randomblob(16)` | open | 1 [A] | expressions |
| `strftime`, `julianday`, `datetime`, `current_timestamp`, with a value or `'now'` | open | `strftime` 5 [A], `julianday` 4 [A] | expressions |
| Window functions, recursive common table expressions, FTS, JSON functions, virtual tables | not considered | no | |

## Writing

| Feature | State | Glue | Gap |
| --- | --- | --- | --- |
| `insert into t (a, b) values (?, ?)` and `insert into t values (...)`; a column left out gets its default | built | 2 / 4; without a list 1 [A] | |
| `update t set a = ?, b = null where ...`, `delete from t where ...`, with or without `where` | built | update 4, delete 3 / 0 | |
| `returning a, b`, `returning *` | built | 2 [A] | |
| `set a = a + 1` | open | 1 / 0 | expressions |
| `returning a + 1`, `returning a as x` | open | no | expressions, query shapes |
| `values (1, 'a'), (2, 'b')`: several rows | open | no | writes |
| `insert into t select ...` | open | no | writes, query shapes |
| `insert into t default values` | open | no | writes |
| `insert ... on conflict (k) do update set ...`, `do nothing` | rejected: [roadmap](../roadmap.md#restrictions) ("upserts") | no | writes |
| `replace into`, `insert or replace`, `insert or ignore`, `update or ...` | rejected, as the glue note reads the roadmap's "upserts"; the kickoff of 2026-11-10 grouped them there | in 3 statements with `$1` [A] | writes |
| `update ... from`, `set (a, b) = (...)` | open | no | writes, query shapes |

## Transactions

| Feature | State | Glue | Gap |
| --- | --- | --- | --- |
| `db.BeginTx` for a write transaction, which waits for the single writer, and a read-only one; `LevelDefault`, `LevelSerializable` | built | yes | |
| A statement on its own as its own transaction | built | yes | |
| The texts `begin`, `commit`, `rollback`, `savepoint`, `release` | open | no | |
| Other isolation levels, such as `sql.LevelReadCommitted` | open | no | |
| A statement while rows of the same transaction are open, such as a write in a loop over rows | open; to look at again ([driver.md](../todo/driver.md)) | no: sqlx reads every row first | driver |

## The driver

| Feature | State | Glue | Gap |
| --- | --- | --- | --- |
| `s4ql.Open`, or `s4ql.Register` and `sql.Open` | built | glue must accept a `*sql.DB` or a driver name [A, required] | |
| A plain `?`, directly and through prepared statements | built | yes | |
| Arguments: Go integers, floats, strings, a `[]byte` that is not nil, nil, `driver.Valuer`, named types | built | yes | |
| `?1`, `:name`, `@name`, `$name`, `sql.Named` | open | `$1` 3 [A] | driver |
| Arguments of type `bool` or `time.Time` | open | no: glue passes times as text | driver |
| Several statements in one `Exec` | open; one per text "for now", decided by the lead on 2026-11-18 ([sql-front-end.md](../todo/sql-front-end.md)) | 3+ [A only if split; a trigger's `begin ... end;` makes a split on `;` unsafe] | driver |
| A read on a `sql.Conn` whose rows are open | open; refused since 2026-11-19 ([driver.md](../todo/driver.md)) | no | driver |

## What glue still needs

From the tables, checked against what is built now. The glue note's other gaps are built.

Waits on s4ql, with no change in glue named:

- Expressions: `received = received + 1` in goqite's receive (1 / 0); `count(*)` (0 / 5).
- Query shapes: the scalar subquery (1 / 0, building); a select without `from` in a test (0 / 1); `as` aliases (0 / 2); `count(*)`, as above.
- Driver: several statements in one text (3+), unless migrate and the fixture loader split them.

Could be removed by a change in glue or its dependencies [A]:

- Expressions: expression defaults (5), `strftime` (5), `julianday` (4), `lower`, `hex`, `randomblob`, and `||` (1), if goqite makes ids and times in Go; `select 1` in `Ping` (1), with `PingContext`.
- Query shapes: `exists` (1); a subquery in `from` with `union` and `in` (0 / 2).
- Schema: `strict, without rowid` on 6 tables; a primary key on `migrations` (1, in migrate); the trigger (1), if goqite sets `updated` in its 2 updates; `sqlite_master` (0 / 1).
- Writes and driver: `REPLACE INTO` and `$1` (3 statements), if glue writes its own `scs.Store`.
- Not SQL: glue accepts a `*sql.DB` or a driver name. This one is required. Also, glue's `Connect` gives the ping 10 s, and after a crash an instance serves only after one lease duration, 30 s by default ([spec](../spec.md)).
