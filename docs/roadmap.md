# SQL roadmap

A working document on which SQL s4ql supports, what is decided and not built, and what is undecided or rejected, by area. The rules that do not change are in [spec.md](spec.md). The details of what is agreed and not built are in the files of [the todo](todo.md).

A piece of work is named by its feature, such as "upserts", and no longer by a number. The slices 1 to 15 at the end are the record of what was built until 2026-11-24, in order.

## At a glance

| Area | Supported now | Decided next |
| --- | --- | --- |
| [Expressions and functions](#expressions-and-functions) | Literals, `null`, and `?`. Arithmetic: `+`, `-`, `*`, `/`, and a unary `-` on integers and reals, in `set` and in `where`, with generated checks, seven planted bugs, a driver test, and mutation testing of the statement pass, the evaluator, and the engine. | More operators and functions, and expressions in more places. The evaluator is s4ql's own, in Go. SQLite's answer or an error. Time and randomness from the host. |
| [Reading](#reading) | `select` from one table, with `where`, `order by`, `limit`, and a scalar subquery in the `where`. | Operators of s4ql's own for grouping and joining. |
| [Writing](#writing) | `insert` of one row, `update`, `delete`, and `returning`. | `on conflict (key) do nothing` and `do update`. Several rows per `insert`. |
| [Schema](#schema) | `strict, without rowid` tables with one key column, defaults, indexes, and `if not exists`. | `strict, without rowid` with a primary key stays required. |
| [The driver](#the-driver) | `s4ql.Open`, `?`, five kinds of argument, transactions, and several statements in one `Exec`, with hand-written tests, a generated workload, the driver's generated comparison with mattn, the engine's differential tests, and the simulation with eight planted bugs. | Mutation testing of several statements. |
| [Limits of the core](#limits-of-the-core) | A commit that is too large fails. The rows of a `returning` are held in memory without a bound. | A limit on the memory of an operator, with a clear error. Spooling later. |

## Principles

- **Everything is opt-in.** Nothing is supported until it is listed here, all the way down to `select 1`. Anything else is rejected as not supported.
- **Reject, never reinterpret.** Whatever the engine accepts behaves exactly as in SQLite. Legacy behaviour is left behind by refusing it, not by changing what it means.
- **Small pieces.** Support grows one small piece of work at a time. Each piece goes through every layer and is checked against the oracle before the next one starts. The slices were checked by table tests against SQLite, by generated sequences in the engine's differential tests and in the driver's against mattn/go-sqlite3, by the simulation with planted bugs, by independent reviews, and by mutation testing.

## Restrictions

These hold for everything that s4ql supports, unless this document says otherwise.

- All tables are `STRICT`.
- All tables are `WITHOUT ROWID`. Every table therefore has a primary key, and no integer keys are generated.
- Foreign keys are always enforced.
- Double quotes mean identifiers only. A double-quoted string is rejected, and so are `[brackets]` and backticks around names.
- Single quotes mean strings only. A single-quoted string where a name is expected is rejected.
- No `ANY` column type.
- A name is never empty. `""` as the name of a table or a column is rejected, and SQLite accepts it.
- No settings that change behaviour, for now.
- A table whose name starts with `s4ql_` or `sqlite_`, in any case of the letters, is rejected. SQLite reserves `sqlite_` itself, and `s4ql_` is for the tables that s4ql keeps in its cache.
- A table with the name of one of SQLite's built-in virtual tables, such as `json_each` or `pragma_table_info`, is rejected. SQLite reads those tables without a `create table`.
- UTF-8 only. SQL text, identifiers, and text values are always valid UTF-8. A statement whose text is not valid UTF-8 is rejected before it is parsed, and so is a parameter or an expression that would produce text that is not valid UTF-8. SQLite accepts such text.
- No NUL byte in SQL text. A statement whose text has a NUL byte is rejected before it is parsed. SQLite reads such text up to the NUL byte and ignores the rest.

Rejected for now, to be decided later: join variants such as comma joins and `NATURAL JOIN`. Upserts were here until 2026-11-24; what is decided for them is under [Writing](#writing).

## Milestones

A milestone is a goal that several pieces of work lead to. It helps to choose what comes next. It is not a specification: s4ql's own decisions take precedence over what a milestone needs, and a conflict with its design is reported to Markus. Markus on 2026-11-24: "take care not to overfit to accomodate goqite and glue. They just serve as a milestone."

- **The first application on s4ql: [glue](https://www.maragu.dev/glue).** `maragu.dev/glue` runs on s4ql in place of SQLite. Markus on 2026-11-19: "a nice second milestone: get maragu.dev/glue working on this storage instead of SQLite." The SQL that it needs and s4ql does not have yet is in [the research note on glue](research/glue-on-s4ql.md), and by area in [the feature map](research/feature-map.md). Slices 5 to 15 led towards it. Its known conflicts with s4ql's design:
  - None of glue's six tables is `strict, without rowid`.
  - The table `migrations` of maragu.dev/migrate has no primary key.
  - goqite, the queue that glue uses, has a trigger.
  - The session store that glue uses, scs, writes with `REPLACE INTO` and `$1` parameters.
  - One test reads `sqlite_master`.

## Expressions and functions

**Supported now:**

- Literals: a decimal integer that fits 64 bits, with at most one `-` in front; a real, such as `1.5`, `.5`, or `1e3`, read to the same float as SQLite reads it, bit for bit; a string in single quotes; a blob, such as `x'6162'`; and `null` (slices 1, 4, and 6).
- A plain `?` in place of a value in `values`, `set`, `where`, and `limit` (slices 2, 7, 8, and 13).
- Exact types: a value that is not null has the type of its column (slice 4).
- Arithmetic, built on 2026-11-25, with the notes in [todo/arithmetic.md](todo/arithmetic.md):
  - An expression is built from columns, integer and real literals, a plain `?`, parentheses, `+`, `-`, `*`, `/`, and a unary `-`. It may stand as the value of an assignment in `set` and as either side of a comparison in a `where`. Nowhere else yet, and no function.
  - An integer overflow, a division by zero, and a real result that is not finite are errors, and the statement then changes nothing. In the driver they match `s4ql.ErrEvaluation`, with `s4ql.ErrOverflow`, `s4ql.ErrDivisionByZero`, or `s4ql.ErrNotFinite`.
  - Text and blobs never meet arithmetic. An integer against a real across a comparison or a `set` is refused.
  - A column may be compared with, or copied from, a column of the same type.
  - Checked with the evaluator against SQLite with generated expressions and a fuzz test, the engine's differential tests, and independent reviews. Its generated workload, simulation with planted bugs (seven), and driver test are built (2026-11-25); the statement pass and the evaluator are mutation-tested (2026-11-26: 645 and 649 mutants, none unexplained), and so is the engine (2026-11-26: 2,488 mutants, none unexplained).
- No function. A `where` compares values and expressions; see [Reading](#reading).

**Decided,** with the details in [todo/expressions.md](todo/expressions.md):

- Expressions are evaluated by an evaluator of s4ql's own, in Go, which is checked against SQLite with generated expressions. The cache's SQLite does not evaluate them. Agreed by Markus on 2026-11-24.
- For every expression, s4ql gives SQLite's answer or an error, never a different answer. An integer with a real, such as `1 + 0.5`, works as in SQLite. So an application written for s4ql also runs unchanged on SQLite. Agreed by Markus on 2026-11-24.
- Time and randomness come from the host. `'now'` is the host's clock, one value for each statement (also inside a transaction, where each statement reads the clock again), and random bytes come from the host's source. Agreed by Markus on 2026-11-24.
- Which operators and functions come first is decided by what is needed next, and the others follow by need. Decided by the lead on 2026-11-24.

**Undecided:**

- How the annotation of a statement records the time and the random bytes that it used. The spec asks for "the time and random seeds", and the commit format has no field for them yet.
- Other literals, such as `+1`, `0x10`, `1_000`, and `true`.

**Rejected,** where SQLite's answer is a quirk (agreed by Markus on 2026-11-24): arithmetic on text, such as `'1' + 1` and `'abc' + 1`; an integer overflow, where SQLite silently gives a float; a division by zero, where SQLite gives null; and a comparison of values of different types by SQLite's order of types.

## Reading

**Supported now:**

- `select` of named columns, in any order and also more than once, or of `*`, from one table (slice 5).
- `where` with comparisons of a column, or an expression of arithmetic, with a literal, a `?`, a scalar subquery, or a column of the same type, with the value on either side, by `=`, `==`, `!=`, `<>`, `<`, `<=`, `>`, and `>=`; `is null` and `is not null`; all joined by `and` (slices 7, 12, and 15).
- `order by` one or more columns, each `asc` or `desc`, and `limit` with a number or a `?` (slice 13).
- The scalar subquery is a `select` of one column from one table, in the `where` of a `select`, an `update`, or a `delete` (slice 15). It is built in the statement pass, the engine, and the driver. Its generated workload, simulation with planted bugs, driver test, and mutation testing are done.
- A read uses the primary key or an index, chosen by fixed rules, and scans the whole table otherwise (slices 7, 9, and 13). Rows without `order by` come in an order that varies, as [the spec](spec.md) says.

**Decided,** with the details in [todo/operators.md](todo/operators.md):

- Grouping and joining get operators of s4ql's own, in Go, on top of the ordered scans and the lookups by key that the cache gives. Sorting stays an ordered scan of the cache. Agreed by Markus on 2026-11-24.
- An operator that holds rows has a limit on its memory; see [Limits of the core](#limits-of-the-core).

**Undecided:** which shapes come, and in which order. Among them are `count(*)` and the other aggregates, `group by`, the forms of `join`, `distinct`, `offset`, aliases, a `select` without `from`, `exists`, `in (select ...)`, `union`, and `or`, `not`, and parentheses in a `where`.

**Rejected,** for now: comma joins and `natural join` (see [Restrictions](#restrictions)).

## Writing

**Supported now:**

- `insert` of one row, with or without a list of columns. A column that the list leaves out gets its default, or null (slices 1, 5, 6, and 11).
- `update ... set` with literals, `null`, `?`, arithmetic, and a column of the same type, also of the primary key, and `delete`, each with or without a `where`. `RowsAffected` counts as SQLite counts (slice 8).
- `returning` a list of columns or `*`, on `insert`, `update`, and `delete` (slice 14).
- A statement that would give a key twice fails with SQLite's error and changes nothing (slices 1 and 8).

**Decided,** with the details in [todo/writing.md](todo/writing.md):

- Upserts. An `insert` without a clause fails on a conflict and changes nothing, as today, which is SQLite's `abort`. `insert ... on conflict (key) do nothing` and `insert ... on conflict (key) do update set ...` are built when they are next. Agreed by Markus on 2026-11-24.
- Several rows per `insert`, and `insert ... select`, are details, built when they are next. Decided by the lead on 2026-11-24.

**Undecided:** `insert ... default values`, `update ... from`, and `set (a, b) = (...)`.

**Rejected:** `replace into`, and the spellings `insert or abort`, `or fail`, `or rollback`, `or ignore`, and `or replace`. A `replace` deletes the old row and inserts a new one, so a column that the statement does not name falls back to its default. Agreed by Markus on 2026-11-24: "b sounds like the least surprising right choice here."

## Schema

**Supported now:**

- `create table` of a `strict, without rowid` table, with one primary-key column at any position, and columns of the types `int`, `integer`, `real`, `text`, and `blob`, each `not null` or taking null, with or without a `default` that is a literal (slices 1, 4, 5, 6, and 11).
- `drop table`; `create index` on one or more columns, each `asc` or `desc`, and `drop index`. An index is not unique (slices 1 and 9).
- `if not exists` and `if exists` on all four (slice 10).

**Decided:** `strict, without rowid` with a primary key stays required. Markus on 2026-11-24: "a, definitely, for the reasons we agreed on earlier when discussing that design."

**Undecided:**

- Triggers are deferred. Markus on 2026-11-24: "Let's defer triggers, not sure yet". When they come, replay must not run them ([todo/storage-formats.md](todo/storage-formats.md)).
- Reading the schema, `unique`, `check`, foreign keys, and `alter table` stay undecided until one of them is next. Decided by the lead on 2026-11-24.
- Also open: a key of several columns, views, a partial index or one on an expression, and `create table ... as select`.

**Rejected:** a table that is not `strict` or that has a rowid, and so also a table without a primary key and `autoincrement` (see [Restrictions](#restrictions)).

## The driver

**Supported now:**

- `s4ql.Open`, which returns a `*sql.DB`, and `s4ql.Register`, for `sql.Open` by name ([todo/driver.md](todo/driver.md)).
- A plain `?`, directly and through prepared statements (slice 2).
- Arguments: Go integers, a `float64` or `float32`, a `string`, a `[]byte` that is not nil, and nil, also through a `driver.Valuer` (slices 2, 4, and 6).
- Transactions with `db.BeginTx`: a write transaction, which waits for the single writer, and a read-only one, at `sql.LevelDefault` or `sql.LevelSerializable`. A statement outside a transaction is a transaction of its own (slice 3).
- Several statements in one `Exec`, built on 2026-11-25, with the notes in [todo/several-statements.md](todo/several-statements.md): they mean the same as running them one after another, inside a transaction as part of it, and outside one each committing on its own, as in SQLite and mattn's driver (agreed by Markus on 2026-11-24). `Query` and `Prepare` take one statement. Checked with hand-written tests and a comparison with mattn's driver, and since 2026-11-26 with a generated workload of texts of several statements, which the driver's generated test runs in one `Exec` on both sides and compares with mattn's own `Exec`, and since 2026-11-26 the split is compared with SQLite by a differential test and a fuzz target, and the engine's differential tests run each text in one `ExecAll` and compare it with its statements run one after another (built 2026-11-26), and the engine's simulation runs the texts through `ExecAll` with crashes between their statements and finds eight planted bugs of its loop and commit path (built 2026-11-26). Mutation testing is still to come.

**Decided,** with the details in [todo/driver.md](todo/driver.md):

- A statement while rows of the same transaction are open stays refused until an application needs it. Decided by the lead on 2026-11-24.
- The other forms of a parameter, such as `$1` and `:name`, and Go types such as `time.Time` are details for when they are next. Decided by the lead on 2026-11-24.

**Undecided:** the SQL texts `begin`, `commit`, `rollback`, `savepoint`, and `release`, and the other isolation levels.

**Rejected:** a nil `[]byte`, because it does not say whether it means null or the empty blob (slice 4).

## Limits of the core

**Today:**

- A transaction whose commit object is larger than the store takes fails at its commit with `s4ql.ErrTooLarge` (slice 3).
- A text or a blob holds up to 1,000,000,000 bytes, SQLite's limit (slice 4).
- A sort that no index gives runs in the cache's SQLite, which spills to a temporary file ([research/memory-bounds.md](research/memory-bounds.md)).
- The rows of a `returning` are held in memory without a bound ([todo/operators.md](todo/operators.md)).
- A stop of the instance does not stop a sort that runs ([todo/storage-core.md](todo/storage-core.md)), and the first phase of `db.Close()` has no bound ([todo/driver.md](todo/driver.md)).

**Decided,** with the details in [todo/operators.md](todo/operators.md): an operator that holds rows gets a limit with a clear error first, so that a statement that would hold more than a set amount fails and changes nothing. Spooling to the cache comes later, one operator at a time, starting with the rows of a `returning`. A spool that fills the disk stops the whole instance ([research/memory-bounds.md](research/memory-bounds.md)). Agreed by Markus on 2026-11-24.

**Undecided:** the size and the form of the limit.

**Rejected:** nothing.

## What was built, slice by slice

Until 2026-11-24 the work was planned as numbered slices. Slices 1 to 15 below are the record of what was built, in order. A built slice stays here as it was built, and is never removed or folded into another.

### Slice 1: one integer column

Status: built. The engine is checked against SQLite, and the `database/sql` driver runs these statements, without parameters, which slice 2 added, and without `db.BeginTx`, which slice 3 added.

```sql
create table t1 (v1 int primary key) strict, without rowid;

select * from t1;

insert into t1 (v1) values (1);

select * from t1;

drop table t1;
```

Included, without adding syntax:

- Any table name, column name, and integer value, in exactly these statement shapes: one `int primary key` column, `select *` from one table, a single-row `insert` that names the column, and `drop table` with one table name. An integer value is a decimal integer literal, with at most one `-` in front, that fits a signed 64-bit integer. A `+` sign, a hex literal, a digit separator, and a number outside 64 bits are not supported yet.
- Several rows and several tables. `select *` returns rows in primary-key order, as SQLite does for these tables.
- The errors these statements can produce, matching SQLite's: inserting a key twice, creating a table that exists, reading, writing, or dropping a table that does not exist, and inserting into a column that does not exist.
- Each statement is its own transaction.
- Surviving a restart: with an empty cache, replaying the log from S3 gives the same answers.

Everything else is rejected, including a second column, `integer` spelled out, `where`, `begin`, and `drop table if exists`.

### Slice 2: parameters

Status: built. The statement pass and the engine are checked against SQLite, and the `database/sql` driver runs an insert with an argument, directly and through a prepared statement. The generated tests make arguments: one insert in three has a `?`, and some calls have none, one too many, one of a type that is not supported, or one with a name. The engine's differential test compares every other call with SQLite and checks those 717 of its 12,000 statements against s4ql's rule; the driver's compares 8,000 calls with mattn/go-sqlite3, also prepared statements run several times, and checks 515 against the rule. The simulation checks that each commit holds the arguments that its caller gave, and finds a planted bug that annotates a wrong one. Mutation testing of the new code left no mutant unexplained.

```go
db.ExecContext(ctx, "insert into t1 (v1) values (?)", 1)
```

Included, without adding syntax:

- A plain `?` in the place where slice 1 has the integer literal of an `insert`. Nowhere else: the other statements of slice 1 have no value.
- An argument that is a Go integer and fits a signed 64-bit integer: what `database/sql` turns into an `int64`.
- Both directly, with `db.ExecContext` and its arguments, and through a prepared statement, with `db.PrepareContext` and then `stmt.ExecContext`, any number of times with different arguments.
- The number of arguments must equal the number of `?` in the text. Any other number is an error, the same for a direct call and a prepared one, and the statement does not run. This also holds for a text with no `?` that is given an argument. SQLite would bind NULL for a missing one; s4ql does not. The error is an ordinary error of s4ql's, whose message starts with `s4ql: ` and says both numbers, and not "not supported".
- The commit object keeps the arguments in the annotation of the statement, by position, next to its text.
- The errors of slice 1, matching SQLite's: a key inserted twice through a parameter gives the same error as with a literal.
- Surviving a restart: with an empty cache, replaying the log gives the same rows for rows inserted through parameters.

Rejected as not supported, where SQLite would accept it:

- The other forms of a parameter: `?1`, `:name`, `@name`, `$name`, and an argument made with `sql.Named`.
- An argument of any other type: nil, a string, a float, a bool, a `[]byte`, a `time.Time`. The message says which type.
- A parameter inside an expression, such as `values (-?)`, or in any other place.

The text is judged first, then the arguments, then the instance: a syntax error is SQLite's whatever the arguments are, and an error of the arguments does not depend on whether the instance serves.

### Slice 3: transactions

Status: built. The engine is checked against SQLite: pinned and generated sequences of statements run in transactions on both sides, SQLite's between `begin immediate` and `commit` or `rollback`, or under `pragma query_only`. The `database/sql` driver runs transactions, tested through `database/sql`. The differential test of the driver, `internal/sqlite/crosscheck/drivercheck`, runs generated transactions through `database/sql` on both sides, against SQLite through `mattn/go-sqlite3`, whose transactions begin with `begin immediate` and whose read-only ones run under `pragma query_only`: 200 sequences of 40 calls make 499 transactions, 342 write and 157 read-only, of which 359 commit and 140 roll back, on pinned connections and through the pool. The result of each commit and roll back and every table after it are compared with SQLite, and so is every call in them, except the 139 that s4ql refuses by its own rule (67 isolation levels, 44 statements while rows of the transaction are open, and 28 second begins on a connection), which are checked against the rule, with their numbers pinned. The engine's simulation runs transactions: one turn of a caller in four is a write or a read-only transaction of two to six statements, under the faults of the store, crashes, and closes. It checks that a commit holds exactly the statements of its transaction that succeeded and that write, in order, that nothing of a transaction that rolled back is in the log, that every statement gives what SQLite gives on the state of its transaction's beginning with the transaction's own writes, and that two write transactions never overlap. 1,000 seeds of each workload made 3,974 commits of more than one statement, with no violation, and the simulation finds three planted bugs of transactions. The core has a property test of its savepoints, against a model and a replay of the log. Mutation testing of the code of transactions, 628 mutants of the engine and 101 of the savepoints and checks of serving of the core, left no mutant unexplained: 14 survivors of the first run were gaps in the tests, which new tests close, and 33 mutants are equivalent or out of the reach of any test.

```go
tx, err := db.BeginTx(ctx, nil)
tx.ExecContext(ctx, "insert into t1 (v1) values (?)", 1)
tx.ExecContext(ctx, "insert into t1 (v1) values (?)", 2)
err = tx.Commit()
```

Included, without adding syntax:

- `db.BeginTx(ctx, nil)` and `db.Begin()` begin a write transaction. It takes the place of the single writer when it begins, as SQLite's `begin immediate` does, and holds it until its commit or its roll back. `BeginTx` waits for that place as long as its context allows.
- `db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})` begins a read-only transaction. It reads the state of its beginning, whatever commits meanwhile, and holds up no one.
- The isolation levels `sql.LevelDefault` and `sql.LevelSerializable`, for both kinds of transaction. They are what the transactions give: writers run one after another, and a read-only transaction reads one committed state.
- Everything of slices 1 and 2 inside a transaction: `tx.ExecContext`, `tx.QueryContext`, `tx.QueryRowContext`, `tx.PrepareContext`, and `tx.StmtContext` with a statement prepared on the `db`, with the arguments of slice 2. `tx.Commit` and `tx.Rollback` end the transaction. `conn.BeginTx` on a `sql.Conn` works the same way.
- A write transaction reads its own changes, and nothing outside it sees them before its commit.
- A statement that fails leaves the transaction usable, with the effects of the statements before it, as in SQLite.
- `create table` and `drop table` are transactional: the later statements of the transaction see them, and a roll back undoes them.
- One commit object for each transaction. It holds the statements of the transaction that write and that succeeded, in order, each with its arguments. A transaction that changes nothing writes none.
- A write in a read-only transaction gets SQLite's error `attempt to write a readonly database`, with the code 8 and the extended code 8.
- A commit object that is too large for the store fails the commit with an error that matches the new `s4ql.ErrTooLarge`, and nothing is committed.
- Surviving a restart: with an empty cache, replaying the log gives the same rows and tables for what transactions committed.

Rejected as not supported, where SQLite would accept it:

- The SQL texts `begin`, `commit`, `end`, `rollback`, `savepoint`, and `release`, also inside a transaction, which stays open.
- Every other isolation level, such as `sql.LevelReadCommitted`. Nothing is begun.
- A statement in a transaction while the rows of an earlier query of the transaction are open. For now, this rules out a write inside a loop over the rows of a query: close the rows first.

### Slice 4: the other types

Status: built. The statement pass, the engine, and the `database/sql` driver hold all five types, with literals, arguments, and exact types. The pass reads a real literal by a port of SQLite's own conversion, and agrees with SQLite bit for bit on 100,000 texts that are hard to round (81,351 reals and 18,649 infinities), on 6,998 of which Go's `strconv.ParseFloat` gives another float. The generated tests make tables of all five types, with literals and arguments of every kind, weighted to the edges, values of another type than their column, and arguments that no column holds. The engine's differential test compares 12,000 statements with SQLite, half of its sequences with the length limit lowered to 1,000 bytes, and checks 1,152 of them against s4ql's rule (644 with the wrong number of arguments, 140 with an argument that no column holds, 363 with a value of another type than its column, and 5 with a literal longer than the limit); the driver's compares 8,000 calls with mattn/go-sqlite3 and checks 694 against the rule. Both pin how often each case is reached. The simulation runs with typed tables and checks that every insert in the log holds a value of the type of its column, and it finds a planted bug, `convertReal`, which stores a whole real in an `int` column as an integer; no comparison with SQLite can see that bug, since SQLite converts the value in the same way. Mutation testing of the code of slice 4 made 2,627 mutants in five packages: 781 in `stmt`, 1,010 in the engine, 628 in the core, 90 in the format, and 118 in the driver. Of them 513 did not compile, 1,987 were killed, 16 timed out, and 105 are equivalent, each listed with its reason. Six are left open, all in the core: they drop the error of the temporary tables of a transaction, and [todo.md](todo.md) records the test to write. No mutant showed a fault in the code. The fuzz targets `FuzzTypes` and `FuzzTypes_core` give the engine and the core the bytes of their input as the value of an insert into a table of each type, in each form of literal and argument; the second also lowers the length limit and replays the log.

```sql
create table t1 (v1 text primary key) strict, without rowid;

insert into t1 (v1) values ('a');

select * from t1;
```

```go
db.ExecContext(ctx, "insert into t1 (v1) values (?)", "b")
```

Included:

- The column types `int`, `integer`, `real`, `text`, and `blob`, for the one column of a table, which is its primary key.
- Literals: an integer as in slice 1; a real number, such as `1.5`, `.5`, and `1e3`, with at most one `-` in front; a string in single quotes, with `''` for a quote in it; and a blob as `x'6162'`. s4ql reads every real literal to the same 64-bit float as SQLite, bit for bit, by a port of SQLite's own conversion. It rejects none for that reason, only a literal that SQLite reads as an infinity.
- Arguments: a Go integer, a `float64` or `float32`, a `string`, and a `[]byte` that is not nil.
- Exact types. A value must have the type of its column: an integer for `int` and `integer`, a real for `real`, text for `text`, a blob for `blob`. A value of another type is rejected as not supported. This holds also where SQLite converts the value, such as the integer `1` for a `real` column, the text `'1'` for an `int` column, and the real `1.5` for a `text` column, and where SQLite gives an error of its own, such as text for a `blob` column. Markus on 2026-11-19: "Let's stay strict and maybe loosen later".
- Keys are compared as SQLite compares them: text and blobs byte by byte, reals by their number. `select *` returns rows in that order. `0.0` and `-0.0` are the same key, and `-0.0` is stored as `0.0`, as in SQLite.
- The error of a key that exists, matching SQLite's, for every type.
- A text or a blob of up to 1,000,000,000 bytes, SQLite's limit. A longer argument gets SQLite's error. A literal that long gets `string or blob too big` from s4ql, where SQLite gives `statement too long`, which [todo/sql-front-end.md](todo/sql-front-end.md) records as an open difference.
- The commit object names the type of a column as it was declared, so `int` and `integer` stay apart, and replay declares the column in the same way.
- Everything of slices 1 to 3 with these types: arguments, prepared statements, transactions, and a restart with an empty cache.

Rejected as not supported, where SQLite would accept it:

- A text value with a NUL byte. SQLite stores it, and its own functions stop at the NUL. A blob takes any bytes.
- A text argument that is not valid UTF-8, as the restrictions say.
- A real that is not a number, and an infinite one, as an argument or as a literal that is too large, such as `1e999`. SQLite stores a NaN as NULL.
- `null` and a nil argument, and a nil `[]byte`: a table has no column yet that takes NULL.
- An argument of type `bool` or `time.Time`.
- An integer literal outside 64 bits, which SQLite reads as a real, a hex literal, and a digit separator, as before.

### Slice 5: several columns

Status: built. The statement pass, the engine, and the `database/sql` driver hold tables of one or more columns, `insert` with and without a list of columns, and `select` of names and of `*`, with SQLite's errors in SQLite's order. The engine's `TestEngine_columns` compares 13 shapes of tables, of two and three columns of mixed types with the key at each position, with SQLite, and a table of 2,000 columns, SQLite's limit. The generated tests make tables of one to three columns with the key at any position; inserts with a list in the order of the table and in another, with a column left out, a column that the table does not have, a wrong number of values, a value of another type, and two faults at once; and selects of `*`, of names in another order, of a name twice, and of a name that the table does not have. The engine's differential test compares 12,000 statements with SQLite, half of its sequences with the length limit lowered to 1,000 bytes, and checks 1,254 of them against s4ql's rule (718 with the wrong number of arguments, 197 with an argument that no column holds, 336 with a value of another type than its column, and 3 with a literal longer than the limit); the driver's `TestGenerated` in `internal/sqlite/crosscheck/drivercheck` compares 8,000 calls with mattn/go-sqlite3 and checks 746 against the rule (396 with the wrong number of arguments, 189 with an argument that the driver does not support, and 161 with a value of another type than its column). Both pin how often each case is reached. The simulation checks that every row in the log has one value for each column of its table, of the type of that column, and it finds a planted bug, `listOrder`, which gives the values of an insert to the columns in the order of its list. The replay of the log, which the bug breaks, also finds it. The fuzz targets `FuzzColumns` and `FuzzColumns_core` in `internal/engine` cover tables of several columns. Mutation testing of the new code was done on 2026-11-20; `internal/sql/mutation/README.md` has the record.

```sql
create table t1 (v1 int primary key, v2 text not null, v3 real not null) strict, without rowid;

insert into t1 values (1, 'a', 1.5);

insert into t1 (v3, v1, v2) values (2.5, 2, 'b');

select v2, v1, v2 from t1;

select * from t1;
```

```go
db.ExecContext(ctx, "insert into t1 (v1, v2, v3) values (?, ?, ?)", 3, "c", 3.5)
```

Included:

- A table with one or more columns, each of one of the five types of slice 4. Exactly one column is the primary key. It is declared on the column, and it can be at any position. Every table is still `strict, without rowid`.
- Every column that is not the primary key is declared `not null`. The primary-key column can also say `not null`. SQLite accepts it, and it changes nothing.
- `insert into t1 values (...)` gives one value for each column, in the order of the table. `insert into t1 (v2, v1) values (...)` names the columns in any order. Each value is a literal or a `?`, with the exact type of its column, as in slice 4.
- A column that is left out of the list of an `insert` has no value. It gets SQLite's error `NOT NULL constraint failed: t1.v2`.
- The other errors of these statements match SQLite's: a wrong number of values, an unknown column, a column named twice in a table, and more columns than SQLite allows.
- The errors of an `insert` come in SQLite's order. The names and the number of values come first, then a text or a blob that is too long, then the refusal of a read-only transaction. After these comes `NOT NULL constraint failed`, for the first column without a value in the order of the table. Next is "not supported" for a value of another type than its column, for the first such column in the order of the table. A row that is too long comes next, also when its key exists, because SQLite builds the record of the row before it looks for its key. A key that exists comes last.
- `select` names columns in any order, and it can name a column more than once. `select *` gives all columns in the order of the table. An unknown column gets SQLite's error.
- Rows come in the order that SQLite gives them.
- The commit format does not change: a row already holds several values.
- Everything of slices 1 to 4 with several columns: arguments, prepared statements, transactions, and a restart with an empty cache.

Rejected as not supported, where SQLite would accept it:

- A column that is not the primary key and is not declared `not null`. `null` comes in slice 6.
- A column named twice in the column list of an `insert`, such as `insert into t1 (v1, v2, v2)`. SQLite accepts it and uses the first value.
- `null` as a value.
- A default value for a column.
- A table constraint, such as `primary key (v1, v2)`, and a primary key of several columns.
- `where`, `update`, and `delete`.
- An expression or an alias in the select list, such as `v1 + 1` or `v1 as a`.

### Slice 6: `null`

Status: built. The statement pass, the engine, the storage core, and the `database/sql` driver hold columns that take null, with null given by the literal, by a nil argument, and by a column that an `insert` leaves out, and with SQLite's errors in SQLite's order. The commit format got the field `nullable` on a column, and the cache went to version 2. The engine's `TestEngine_columns` compares 44 shapes of tables with SQLite, of two and three columns with the key at each position and with none, some, or all of the other columns taking null, and a table of 2,000 columns. The generated tests make tables in which some, all, or no columns outside the key take null, and null as a literal and as a nil argument, also beside a key that exists and a value of another type, in the key and in a column that does not take it, and for a column that a list leaves out. The engine's differential test compares 12,000 statements with SQLite, half of its sequences with the length limit lowered to 1,000 bytes, and checks 1,270 of them against s4ql's rule (703 with the wrong number of arguments, 220 with an argument that no column holds, 339 with a value of another type than its column, and 8 with a literal longer than the limit); the driver's `TestGenerated` in `internal/sqlite/crosscheck/drivercheck` compares 8,000 calls with mattn/go-sqlite3 and checks 787 against the rule (426 with the wrong number of arguments, 200 with an argument that the driver does not support, and 161 with a value of another type than its column). It gives nil in ten kinds that `database/sql` turns into nil, such as a nil pointer and a `sql.Null*` value that is not valid, and reads each row into an `any`, into `sql.Null*` values, and into pointers. Both pin how often each case is reached. The simulation of the engine requires null values in the log of each workload, and checks that each is in a column that takes null. It finds a planted bug, `zeroLeftOut`, which gives a column that takes null and that a list leaves out the zero value of its type, mostly by a query that gives 0 where SQLite gives null. The simulation of the core finds a planted bug, `nullZero`, which writes each null of a row in the commit object as the zero value of its column's type. Mutation testing of the code of slices 5 and 6 was done on 2026-11-20; `internal/sql/mutation/README.md` has the record.

```sql
create table t1 (v1 int primary key, v2 text) strict, without rowid;

insert into t1 values (1, null);

insert into t1 (v1) values (2);

select * from t1;
```

```go
db.ExecContext(ctx, "insert into t1 (v1, v2) values (?, ?)", 3, nil)
```

Included:

- A column that is not the primary key can be declared without `not null`. It then takes null.
- The primary-key column is never null, because the table is `without rowid`. A null for it gets SQLite's error `NOT NULL constraint failed: t1.v1`.
- The literal `null` as a value in an `insert`.
- A nil argument: a Go `nil`, and whatever `database/sql` turns into nil before it reaches the driver, such as a nil pointer and a `sql.Null*` value that is not valid. `database/sql` hands the driver nil for a nil pointer of any type, also `(*[]byte)(nil)`, for a `driver.Valuer` that gives nil, such as a `sql.NullString` or a `sql.Null[T]` that is not valid, and for a nil pointer to a `Valuer` with a value receiver; it hands a nil `[]byte`, which stays rejected, for `[]byte(nil)`, a pointer to one, a valid `sql.Null[[]byte]` whose value is nil, and `sql.RawBytes(nil)`.
- A column that is left out of the list of an `insert` gets null if it takes null.
- A null for a column that is declared `not null` gets SQLite's error `NOT NULL constraint failed: t1.v2`. The error names the first column, in the order of the table, that does not take null and gets one. A null that the `insert` gives and a column that it leaves out are one case there. The error comes before the refusal of a value of another type, and before the error of a key that exists.
- A null goes into a column of any type. The exact types of slice 4 hold for every value that is not null.
- A null comes back as nil. It scans into a `sql.Null*` value, a pointer, and an `any`.
- The commit format gets the field `nullable` on a column, which says that the column takes null. It already has a null value.
- Everything of slices 1 to 5 with null: arguments, prepared statements, transactions, and a restart with an empty cache.

Rejected as not supported, where SQLite would accept it:

- A nil `[]byte`, as in slice 4. It does not say whether it means null or the empty blob.
- `default`.
- `is null` and `is not null`. They come with `where`, in slice 7.
- A null after a sign, `-null` or `+null`. SQLite reads both as null.
- The column constraint `null`, as in `v2 text null`. SQLite reads it as no constraint, so the column takes null with it as without it.
- A null anywhere but as a value of an `insert`, such as `select null from t1`.

### Slice 7: `where` on `select`

Status: built. The statement pass, the engine, and the `database/sql` driver run a `where` on a `select`. The plan reads by the primary key where the conditions allow it: no row at all for a comparison with null or for `key is null`, a point lookup for `key = value`, a scan between the bounds that `<`, `<=`, `>`, and `>=` put on the key, and a scan of every row otherwise. The filter tests every condition on every row that the scan gives, so the use of the key can only leave out rows that a condition rejects. The generated tests write selects with a `where`, with values that the inserts gave and values beside them. The engine's differential test runs every such select both by the key and with a scan of every row, compares both with SQLite, and checks that the plan reads by the key as the rule says, which no comparison of rows can see. The fuzz targets `FuzzWhere` and `FuzzWhere_core` build a `where` from their input, and read and compare it both ways too. The driver's generated test compares the selects with mattn/go-sqlite3, by the key only, because the driver has no option to read every row. In the simulation, every second engine of a node reads every row, so the comparisons with SQLite see both ways, also across crashes and closes. It finds a planted bug, `openBound`, which reads a closed bound of the key as open and leaves out the row whose key is the bound. Mutation testing of the code of slice 7 was done on 2026-11-20; `internal/sql/mutation/README.md` has the record.

```sql
select * from t1 where v1 = 1;

select v2 from t1 where v1 >= 2 and v1 < 10 and v2 is not null;
```

```go
db.QueryContext(ctx, "select * from t1 where v2 = ?", "a")
```

Included:

- `select ... from t1 where <condition>`.
- A condition compares a column with a literal or a `?`, with `=`, `==`, `!=`, `<>`, `<`, `<=`, `>`, or `>=`. The column is on the left.
- The value of a comparison is a literal of slice 4, `null`, or a `?`.
- `=` and `==` mean the same, and so do `!=` and `<>`.
- The conditions `v2 is null` and `v2 is not null`.
- Conditions joined with `and`. Several conditions can test the same column, as in `v1 >= 2 and v1 < 10`.
- Exact types. The value has the type of its column, as in slice 4. A value of another type is rejected as not supported. This holds also where SQLite converts the value, such as the text `'1'` for an `int` column and the integer `1` for a `real` column.
- A comparison with null, such as `v2 = null` or `v2 = ?` with a nil argument, is unknown, as in SQLite, and gives no rows.
- Values compare as SQLite compares them. Text compares by its bytes, which is SQLite's `binary` collation.
- Rows come in the order that SQLite gives them.
- Everything of slices 1 to 6 with `where`: arguments, prepared statements, transactions, and a restart with an empty cache.

Further details are decided when the slice is built, and recorded here. The forms of the `where`, token for token, are in [internal/sql/stmt/README.md](../internal/sql/stmt/README.md).

- The errors of a `select` with `where` come in SQLite's order. An unknown table comes first, then an unknown column of the select list, then the columns of the conditions, in the order of the text. After these comes `string or blob too big` for a value of a condition that is too long. Last is "not supported" for a value of another type than its column, for the first such condition in the order of the text.
- The first comparison of an unknown column gets SQLite's error, such as `no such column: v8`. An unknown column under `is null` or `is not null` does not stop SQLite at once: a later such test replaces the name, and the next comparison ends the search with it. So `v8 is null and v9 is null` names `v9`, and `v8 is null and v9 = 1` names `v8`, in a table that has neither.
- The refusal of a value of another type names the value and the column: `s4ql: not supported: a string literal compared with the column t1.v1 of type INT`, or `s4ql: not supported: argument 1, which is text, compared with the column t1.v1 of type INT`. A null has no type, and is never refused.
- A `select` by the key, such as `v1 = 1`, reads only the row of that key. One by a range of the key, such as `v1 >= 2 and v1 < 10`, reads only the rows of that range. A `where` that does not bound the key reads every row of the table.
- The rows are the same however they are read, and they come in the order of the key.

Rejected as not supported, where SQLite would accept it:

- `update` and `delete`, with or without `where`. They come in slice 8.
- A literal or a `?` on the left of a comparison, such as `1 = v1`.
- `or`, `not`, and parentheses.
- A column compared with another column.
- A column with the name of its table, such as `t1.v1 = 1`.
- Expressions and functions, such as `v1 + 1 = 2` and `lower(v2) = 'a'`.
- `in`, `between`, `like`, and `glob`.
- `collate`.
- `is` with anything but `null`, such as `v1 is 1`, `v1 is true`, and `v1 is distinct from 1`.
- `isnull`, `notnull`, and `not null` after a column. SQLite reads them as `is null` and `is not null`.
- `order by` and `limit`.
- A `where` deeper than SQLite's limit of 1000 levels for an expression, which SQLite refuses while it parses. The most is 999 conditions, or 998 if one of the first two has a sign before its value.

### Slice 8: `update` and `delete`

Status: built. The statement pass, the engine, and the `database/sql` driver run `update` and `delete`, each with or without a `where`, with literals, `null`, and arguments as values, with exact types, with a change of the primary key, and with `RowsAffected` as SQLite reports it. They are checked statement by statement against SQLite in table tests, and an independent review ran its own random comparison with SQLite. The generated tests write updates and deletes, with and without a `where`, from the values that the inserts gave. The engine's differential test runs every update and delete with a `where` both by the key and with a scan of every row, and compares the rows afterwards and the count of changes with SQLite. The driver's generated test compares `RowsAffected` and the tables afterwards with mattn/go-sqlite3. The simulation compares the count of changes of every update and delete with SQLite's, and finds two planted bugs, `changeNothing`, which changes no row, and `keepOldKey`, which leaves the old row of an update of the key. The fuzz targets `FuzzChanges` and `FuzzChanges_core` build updates and deletes from their input. Its generated checks and its mutation testing were done on 2026-11-20; `internal/sql/mutation/README.md` has the record.

```sql
create table t1 (v1 int primary key, v2 text, v3 real not null) strict, without rowid;

update t1 set v2 = 'b', v3 = 2.5 where v1 = 1;

update t1 set v2 = null;

delete from t1 where v1 >= 10;

delete from t1;
```

```go
res, err := db.ExecContext(ctx, "update t1 set v2 = ? where v1 = ?", "c", 2)
n, err := res.RowsAffected()
```

Included:

- `update t1 set v2 = <value>, v3 = <value>` and `delete from t1`, each with or without the `where` of slice 7.
- A value in `set` is a literal, `null`, or a `?`. A value that is not null has the exact type of its column, as in slice 4.
- `null` only for a column that takes null. For any other column, it gets SQLite's error `NOT NULL constraint failed: t1.v2`.
- An `update` can change the primary key, as in SQLite. If a key then exists twice, the statement gets SQLite's error of a key that exists, and it changes nothing.
- An unknown column in `set` gets SQLite's error.
- `RowsAffected` gives the number of rows that SQLite reports as changed. That is every row that the `where` matches, also when an `update` sets a value that the row already has.
- The commit format does not change: it already has rows to write and keys to delete.
- Everything of slices 1 to 7 with `update` and `delete`: arguments, prepared statements, transactions, and a restart with an empty cache.

Further details are decided when the slice is built, and recorded here.

Rejected as not supported, where SQLite would accept it (except the last item):

- An expression in `set`, such as `v1 = v1 + 1`.
- `returning`.
- `update ... from`.
- `set (v2, v3) = (...)`.
- `or replace` and the other conflict clauses.
- `limit` and `order by` on `update` and `delete`. The pinned SQLite build gives a syntax error for them, and so does s4ql.

### Slice 9: indexes

Status: built, with the generated checks done on 2026-11-20 and the mutation testing on 2026-11-21; `internal/sql/mutation/README.md` has the record. The statement pass, the commit format and storage core, the engine, and the `database/sql` driver run `create index` and `drop index`, and the engine picks the index for a `where` by the fixed rules. It is checked by table tests against SQLite, by two independent reviews, and by pinned counts of rows read. Generated sequences with indexes run every query through every path that can serve it, the engine's choice is compared with a model of the rules, and the query plan of the cache is checked. The simulations find planted bugs of indexes, golden plans pin the plans, and fuzz targets run every path.

```sql
create table t1 (v1 int primary key, v2 text not null, v3 real) strict, without rowid;

create index t1_v2_v3 on t1 (v2, v3 desc);

select v1 from t1 where v2 = 'a' and v3 > 1.5;

drop index t1_v2_v3;
```

Included:

- `create index i on t1 (v2, v3 desc)`: an index on one or more columns of the table, each `asc` or `desc`. When neither is written, it is `asc`. An index is not unique.
- `drop index i`.
- The log holds only the definition of an index. A commit says that a table has an index of a name on its columns with their directions, or that the index is dropped. No index entries go to S3. The commit format changes for this.
- The cache holds a real SQLite index, which SQLite keeps up to date on every write. Each instance builds it when it replays the commit that defines it. The cache's layout version rises to 3, and an older cache is refused, with no upgrade, because nothing uses s4ql yet.
- s4ql's engine picks the index by fixed rules, with no statistics and no `analyze`, and the cache's SQLite then scans exactly that index. The choice depends only on the statement and the schema, so it is the same on every instance. Markus on 2026-11-20: "Agree, and we can revisit later". The rules can change later without a change of format, because results cannot show the choice.
- The rules, for the `where` of slice 7. The key of an index is its columns followed by the columns of the primary key, as SQLite stores an index of a `without rowid` table. A column is pinned by `=` or by `is null`.
  - A `where` that pins the whole primary key with `=` uses the primary key.
  - Otherwise the index, or the primary key, that has the most leading columns of its key pinned wins. With as many pinned, one with a range on the next column wins.
  - A tie goes to the primary key, then to the index created first.
  - When nothing fits, the engine scans the whole table.
  - The rules for `order by` with `limit` come with slice 13.
- The rule on row order of [the spec](spec.md) is built in this slice. From here on the order of rows without `order by` depends on the index used, so each query runs its scan forward or backward, chosen at random. Tests and the simulation give the engine a seed, so a failure can be replayed. The checks against SQLite compare such rows as a set. Any order of such rows is one SQLite could give too.
- A column named twice in an index, the name of an index that is the name of a table, what `drop table` does to the indexes of the table, and `create index` in a transaction that rolls back are decided when the slice is built, by SQLite's behaviour, and recorded in [the todo](todo/slice-09.md).
- Everything of slices 1 to 8 with indexes: arguments, prepared statements, transactions, and a restart with an empty cache.
- The reason: glue has two indexes, goqite's on `(queue, priority desc, created)` and the sessions table's on `(expiry)`, and this slice covers both.

Further details are decided when the slice is built, and recorded here.

Rejected as not supported, where SQLite would accept it:

- `create unique index`. Unique indexes come later, in a slice of their own.
- A partial index, with `where`.
- An index on an expression.
- `collate` in an index.
- `create index if not exists` and `drop index if exists`. They come with slice 10.
- `reindex` and `analyze`.
- In the application's SQL: `indexed by`, `not indexed`, and `explain query plan`.

How it is checked:

- Every generated query runs through every path that can serve it: the full scan, the primary key, and each index, forward and backward. Each path must give the rows that SQLite gives.
- The chosen path is checked against a small model of the rules, because no comparison of rows can see it.
- The statement that the engine sends to the cache is checked with `explain query plan`: it must use the index and the bounds that s4ql asked for. An `indexed by` with an index that does not fit makes SQLite scan the whole index without an error, so the rows alone do not show it.
- The simulation finds planted bugs: a bound turned around on a `desc` column, and an index key without the columns of its primary key.
- `pragma integrity_check` runs on the cache after replay and after crashes. It checks every index against its table.
- The plans of pinned queries, goqite's receive among them, are kept as golden files, so a change of the rules shows as a diff.
- Counts that are exact are pinned in tests and fail on a regression: the rows read from the cache per query, for the pinned queries, goqite's receive among them, with and without the index. So the gain of an index shows as a number that does not depend on the machine or on timing.

Background: [the research note on how engines pick an index](research/index-selection.md).

### Slice 10: `if not exists` and `if exists`

Status: built, with the generated checks and the mutation testing done on 2026-11-21. The statement pass, the engine, and the `database/sql` driver run `create table if not exists`, `create index if not exists`, `drop table if exists`, and `drop index if exists`. A statement that does nothing writes nothing to the log, is not annotated, and runs also in a read-only transaction. One of its own that does nothing waits for no writer, as in SQLite. It is checked by table tests against SQLite, statement by statement and in transactions. Generated sequences with the clauses run in the engine's differential tests and in the driver's against mattn/go-sqlite3. The simulation finds a planted bug, `replaceExisting`, and an independent review checked the branch.

```sql
create table if not exists t1 (v1 int primary key, v2 text) strict, without rowid;

drop table if exists t1;
```

Included:

- `create table if not exists t1 (...)`: when no table `t1` exists, it does what `create table` does. When a table `t1` exists, the statement does nothing and gives no error, whatever the columns of the existing table are, as in SQLite.
- `drop table if exists t1`: when no table `t1` exists, nothing happens and there is no error. When it exists, it does what `drop table` does.
- The two forms also hold for indexes: `create index if not exists i on t1 (...)` and `drop index if exists i`.
- A statement that does nothing writes nothing to the log.
- An error that the declaration would give is still given by SQLite's rules when the table exists, for example a syntax error in the column list. What SQLite does with a declaration that is valid in syntax but not supported by s4ql, when the table exists, is decided when the slice is built, by SQLite's behaviour and the rule of the statement pass, and recorded in [the todo](todo/slice-10.md).
- The commit format does not change.
- Everything of slices 1 to 9 with the two forms: transactions, prepared statements, and a restart with an empty cache.
- The reason: the migration tool of glue creates its migrations table with `create table if not exists`.

Further details are decided when the slice is built, and recorded here.

Rejected as not supported, where SQLite would accept it:

- `create temp table`.
- `create table ... as select`.
- Everything else that slices 1 to 9 reject in a declaration.

### Slice 11: `default` with a literal

Status: built, with the generated checks and the mutation testing done on 2026-11-21. The statement pass, the commit format and storage core, the engine, and the `database/sql` driver run a `default` with a literal on a column of `create table`, and an `insert` with a list of columns gives each column that it leaves out its default, or null where the column has none. It is checked by table tests against SQLite, with goqite's schema among them as far as slices 1 to 11 take it. Generated sequences with defaults run in the engine's differential tests and in the driver's against mattn/go-sqlite3. The simulation finds a planted bug, `nullDefault`, and two independent reviews checked the branches.

```sql
create table t1 (v1 int primary key, v2 int not null default 0, v3 text default 'a') strict, without rowid;

insert into t1 (v1) values (1);
```

Included:

- A column declaration may carry `default <literal>`, where the literal is one of the literals that `insert` takes, or `null`.
- An `insert` with a column list that leaves the column out stores the default. Until now such a column gets `null` if it takes null, and the statement is refused if it is `not null`.
- The commit format changes: a column carries its default. The cache's layout version rises to 4, and an older cache is refused, with no upgrade, because nothing uses s4ql yet.
- A default whose type is not the column's type, a default on the primary key column, `not null default null`, signed numbers, and `true` and `false` are decided when the slice is built, by SQLite's behaviour and the rule of exact types, and recorded in [the todo](todo/slice-11.md).
- Everything of slices 1 to 10 with a default: transactions, prepared statements, and a restart with an empty cache.
- The reason: the schema of goqite, the queue that glue uses, declares two columns with `default 0`.

Further details are decided when the slice is built, and recorded here.

Rejected as not supported, where SQLite would accept it:

- A default that is an expression, such as `default (1 + 1)`.
- `current_time`, `current_date`, and `current_timestamp`.
- `insert ... default values`.

### Slice 12: a value on the left of a comparison

Status: built, with the generated checks and the mutation testing done on 2026-11-21. The statement pass, the engine, and the `database/sql` driver run a comparison with its literal or `?` on the left and its column on the right, in the `where` of a `select`, an `update`, and a `delete`, and the engine uses the primary key and the indexes for it as for the other order. It is checked by table tests against SQLite. Generated conditions of both orders run through every path that can serve them, in the engine's differential tests and in the driver's against mattn/go-sqlite3, and the engine's choice is compared with the model of the rules. The golden plans hold goqite's receive query in both orders. The simulation finds a planted bug, `unturnedBound`, and an independent review checked the branch with a random test of its own.

```sql
select v1 from t1 where 5 < v1;

select v1 from t1 where ? >= v2;
```

Included:

- In a `where`, a comparison may have the literal or `?` on the left and the column on the right, such as `? >= timeout` and `5 < a`. Slice 7 allows only the column on the left.
- It means what SQLite says it means. The rule of exact types holds as for the other order, and the primary key is used for lookups and ranges as in slice 7, and an index by the rules of slice 9: `5 < a` bounds `a` from below.
- The commit format does not change.
- Everything of slices 1 to 11 with the new order: arguments, prepared statements, transactions, and a restart with an empty cache.
- The reason: goqite, the queue that glue uses, writes its receive query both ways round, `? >= timeout` and `received < ?`.

Further details are decided when the slice is built, and recorded here.

Rejected as not supported, where SQLite would accept it:

- A column on both sides of a comparison.
- A value on both sides of a comparison.
- `is null` and `is not null` with anything but a column on the left.

### Slice 13: `order by` and `limit` on `select`

Status: built, with the generated checks done on 2026-11-21 and the mutation testing on 2026-11-23. The statement pass, the engine, and the `database/sql` driver run `order by` and `limit` on `select`.

```sql
select v1, v2 from t1 order by v2 desc, v1 limit 10;

select v1 from t1 where v2 > ? order by v1 limit ?;
```

Included:

- `order by` takes one or more column names, each with `asc` or `desc`. When neither is written, it is `asc`.
- `limit` takes a number or `?`.
- Rows that tie under `order by` follow the rule on row order of [the spec](spec.md). Where s4ql sorts, the order of tied rows varies from one query to the next: the sort gets a tie-break whose direction is drawn per query. Where an index delivers the order, tied rows come in the order of that index, because varying them would mean reading a whole group of tied rows before the first is returned, which defeats the index (with `limit 1` and a thousand ties, a thousand rows read to return one). That order is not a promise either: it depends on the index the engine picks, and the rules for picking may change. Agreed by Markus on 2026-11-21.
- Two rules for picking an index come with this slice. With the same leading columns pinned and a `limit`, an index that gives the order beats one that only narrows a range. On goqite's receive query with 100,000 rows, that is about 400 times faster when all messages are ready, as [the research note](research/index-selection.md) measured. With nothing pinned, an index that gives the order beats a full scan with a sort.
- `null` sorts where SQLite sorts it: first in `asc`, last in `desc`.
- The commit format does not change.
- A negative `limit`, `limit 0`, a `limit` argument that is not an integer, a column named twice in `order by`, and a column in `order by` that is not in the list of selected columns are decided when the slice is built, by SQLite's behaviour, and recorded in [the todo](todo.md).
- Everything of slices 1 to 12 with `order by` and `limit`: arguments, prepared statements, transactions, and a restart with an empty cache.
- The reason: goqite picks the next message with `order by priority desc, created limit 1`.

Further details are decided when the slice is built, and recorded here.

Rejected as not supported, where SQLite would accept it:

- `offset`, also as `limit x, y`.
- Ordering by an expression or by position, such as `order by 1`.
- `nulls first` and `nulls last`.
- `collate`.
- `order by` and `limit` on `update` and `delete`. The pinned SQLite build gives a syntax error for them, and so does s4ql.

### Slice 14: `returning` on `insert`, `update`, and `delete`

Status: built, with the generated checks and the mutation testing done on 2026-11-23. The statement pass, the engine, and the `database/sql` driver run `returning` on `insert`, `update`, and `delete`. It is checked by table tests against SQLite, and through the driver against mattn/go-sqlite3, with goqite's send and its receive by id among them. Generated sequences with `returning` run in the engine's differential tests and in the driver's against mattn/go-sqlite3. The simulation checks that the rows a caller read belong to a committed change, and it finds the planted bugs. Two independent reviews checked the branches.

```sql
insert into t1 (v1, v2) values (?, ?) returning v1;

update t1 set v2 = ? where v1 = ? returning v1, v2;

delete from t1 where v1 = ? returning *;
```

```go
rows, err := db.QueryContext(ctx, "update t1 set v2 = ? where v1 = ? returning v1, v2", "b", 1)
```

Included:

- `returning` at the end of an `insert`, an `update`, or a `delete`, with a list of column names of the statement's table, or `*` for all columns in the order of the table.
- The statement gives one row for each row that it inserts, updates, or deletes. An `insert` and an `update` give the values after the change. A `delete` gives the values that the row had.
- SQLite does not specify the order of the returned rows, so they follow the rule on row order of [the spec](spec.md), as the rows of a `select` without `order by` do.
- Outside a transaction, the statement commits before it gives its first row. If the commit fails, the statement gives the error and no rows. So a row that a caller has read belongs to a change that is committed. Inside a transaction, the statement gives the rows at once, and the change belongs to the transaction like any other. Agreed by Markus on 2026-11-24.
- `Exec` runs a statement with `returning` and drops its rows. `RowsAffected` gives the same count as without `returning`. `Query` gives the rows.
- The commit format does not change.
- A column named twice in the list, a statement that changes no row, and whether an `update` that sets a row to the values it already has returns that row are decided when the slice is built, by SQLite's behaviour, and recorded in [the todo](todo.md).
- Everything of slices 1 to 13 with `returning`: arguments, prepared statements, transactions, and a restart with an empty cache.
- The reason: goqite, the queue that glue uses, sends a message with `insert ... returning id` and receives one with `update ... returning id, body`.

Further details are decided when the slice is built, and recorded here.

Rejected as not supported, where SQLite would accept it:

- An expression in the list, such as `returning v1 + 1` or a function call.
- An alias, such as `returning v1 as x`.
- A column with the name of its table, such as `returning t1.v1`, and `t1.*`.

### Slice 15: a scalar subquery in `where`

Status: built, with the generated checks done on 2026-11-24 and the mutation testing on 2026-11-25; `internal/sql/mutation/README.md` has the record. It was agreed by Markus on 2026-11-24 and is built in the statement pass, the engine, and the driver. It is checked by their tests, by the engine's differential tests against SQLite, and by independent reviews. It is also checked by a new area of the generated workload, by the simulation with planted bugs, and by the driver's generated test against mattn/go-sqlite3.

```sql
select v1 from t1 where v2 = (select v2 from t2 where v1 = ?);

update t1 set v2 = ? where v1 = (select v1 from t1 where v2 < ? order by v2 desc limit 1) returning v1;

delete from t1 where v1 = (select v1 from t1 order by v2 limit 1);
```

Included:

- A scalar subquery is a `select` in parentheses that stands where one value stands. It gives one value: the one column of its first row.
- It may stand in the `where` of a `select`, an `update`, or a `delete`, as an operand of a comparison, wherever a `?` may stand there today. The case that Markus agreed, and that goqite needs, is the right of `=`.
- The inner `select` is a `select` as s4ql supports it today on one table: one result column, a `where`, an `order by`, a `limit`, and `?` parameters. It may read the table of the outer statement or another table.
- The `?` parameters are numbered in the order of the text over the whole statement, the subquery's included. In goqite's receive, the `?` of `queue = ?` inside the subquery is the third argument.
- A subquery that finds no row gives `null`, so the comparison matches no row. One that finds several rows gives the first, as in SQLite. Without an `order by` that says which row is first, it gives whichever row comes first, as SQLite documents.
- The subquery runs once, before the outer statement changes anything, and inside the same transaction. In an `update` or a `delete`, it sees the rows as they were before the statement.
- The errors of a statement with a subquery, their order, and how the rule of exact types applies to the value of a subquery are decided, and recorded in [the todo of slice 15](todo/slice-15.md).
- Everything of slices 1 to 14 with a subquery: arguments, prepared statements, transactions, `returning`, and a restart with an empty cache.
- The reason: goqite, the queue that glue uses, picks the message that it receives with `id = (select id from goqite where ... order by priority desc, created limit 1)`. The same statement has `received = received + 1` in its `set`. That arithmetic is not in this slice. It comes after, as a design question on how s4ql evaluates expressions.

Further details are decided when the slice is built, and recorded here.

Rejected as not supported, where SQLite would accept it:

- A subquery that names a column of the outer statement, which is a correlated subquery.
- A subquery inside a subquery.
- `in (select ...)` and `exists (select ...)`.
- A subquery in `from`.
- A subquery anywhere else: in the select list, in `set`, in `values`, in `order by`, in `limit`, and in `returning`.
- A subquery whose column has another declared type than the column it is compared with (`int` and `integer` count as one type), also when it finds no row.
- An argument of the subquery's `limit` that is not an integer from 0.

Refused as well, as SQLite refuses them: a subquery with more than one result column, such as `v1 = (select v1, v2 from t2)`, and a subquery in a default. Which error s4ql gives for each is recorded in [the todo of slice 15](todo/slice-15.md).
