# The engine

`internal/engine` runs one statement of [slice 1](../../docs/roadmap.md#slice-1-one-integer-column) on a database of the [storage core](../core/README.md), a `*core.DB`, and gives SQLite's results and SQLite's errors. What a statement of slice 1 is, token for token, is in [the README of `stmt`](../sql/stmt/README.md#slice-1).

The interface:

| Name | What it does |
| --- | --- |
| `New(d, host, Options)` | Gives an `*Engine` for the database `d`. The engine reads the monotonic clock of the host, for the duration in its events. `Options.Logger` takes the events, and the default discards them. |
| `Engine.Exec(ctx, src) (Result, error)` | Runs the statement of the text `src`. A query is run to its end, and its rows are dropped. The result comes with an error too. |
| `Engine.Query(ctx, src) (*Rows, error)` | Runs the statement and gives its rows. A statement that is not a query is run as `Exec` runs it. Its rows have no columns and no rows, and `Rows.Result` gives its result; Query gives these rows also with its error. |
| `Rows.Columns()` | The columns of each row, each a `Column` with the name as the table stores it and the declared type, `INT`. |
| `Rows.All()` | The rows in order, as an `iter.Seq2[[]*s4qlpb.Value, error]`. The sequence ends at the first error, and can be used once. |
| `Rows.Close()` | Ends the rows. |
| `Rows.Result()` | The result of a statement that is not a query. |
| `Result{Changes, SetsChanges}` | See [Changes and the last insert rowid](#changes-and-the-last-insert-rowid). |
| `ErrNoStatement` | The error of a text that is empty or holds only spaces, comments, and semicolons. SQLite prepares such a text into no statement and gives no error. |
| `ErrInternal` | Matched by the error of a fault of s4ql. See [Errors](#errors). |

An `Engine` is safe for concurrent use. `Rows` are not.

## One statement, one transaction

Each statement is its own transaction of the core.

- **A statement that changes the database runs between `DB.Begin` and `Tx.Commit`.** The engine annotates the transaction with the text of the statement before the commit; see [Annotations](#annotations). Any failure rolls the transaction back, and so does a panic of an operator. A commit ends the transaction whatever it gives.
- **A query runs in `DB.Read`.** Its `Rows` hold the read transaction until they end. They end when `Rows.All` has given the last row or an error, when the loop over it stops early, when the loop panics, or at `Rows.Close`. One of these must happen.
- **`Rows.Close` during a loop over `Rows.All` ends the loop at its next row,** and the read transaction ends then. The loop holds the transaction also while it is suspended in its body, so an iterator that is suspended must be resumed or stopped for the transaction to end.
- **A failure of the end of the read transaction after a loop that stopped early is the error of the next `Rows.Close`.**
- **A panic while a query is built ends its read transaction too.**
- **`Rows.All` yields to the host between two rows,** so that under the simulation other tasks run while a query is open.
- **A failure of the end of the read transaction after the last row is the last error of `Rows.All`.**

## The stages

A statement goes through three stages. A test can call each on its own.

1. **`stmt.Check`** reads the text into a typed statement, or gives SQLite's syntax error, "not supported", or no statement.
2. **`NewPlan`** turns the statement into a `Plan`. A plan is plain data, and `Format` prints it. It holds each name as a `stmt.Name`, as the statement writes it, and nothing of the catalogue: whether a table or a column exists is not known when the plan is made.
3. **Execution** runs the plan in a transaction of the core. `buildQuery` and `buildSink` turn the plan into a tree of operators inside the transaction. They look the names up there, and give SQLite's errors about them in SQLite's order. The engine then runs the tree.

The plans of the four statements, as `Format` prints them:

```
CREATE TABLE "T""1" (V1 INT PRIMARY KEY) WITHOUT ROWID, STRICT

create table "T""1" (V1 INT, primary key (V1))
```

```
drop table t1;

drop table t1
```

```
insert into t1 ("v") values (-  9223372036854775808)

insert into t1 ("v")
  values (-9223372036854775808)
```

```
select * from été

query
  project *
    scan été
```

A plan is a `*CreateTable`, a `*DropTable`, an `*Insert`, or a `*Query`. An insert and a query read their rows from a tree of nodes: `*Scan`, `*Values`, and `*Project`. The one expression is `*Integer`.

**Why a tree of iterators, and not a bytecode machine like SQLite's.** The lead recommended it on 2026-11-18, and Markus left the choice to the lead: "I'll trust your recommendation." Each operator is tested alone, a plan is data that tests can read, and the oracle stays rows and errors compared with SQLite, which does not change between SQLite's releases. A bytecode machine that mirrors SQLite's would give `EXPLAIN` as a second oracle and SQLite's order of checks more cheaply, but it ties the engine to an instruction set that SQLite says is not an interface. Where SQLite's order of checks or of errors is visible, the engine reproduces it on purpose, and the differential tests find the places. The plan is the seam: an executor of another kind can replace the operators behind it without touching the parser or the pass after it. The trade-offs are in [research/sql-front-end.md](../../docs/research/sql-front-end.md) §6.3.

## The storage interface

The operators and the engine do not call the core. They call five interfaces in `storage.go`:

| Interface | Holds |
| --- | --- |
| `reader` | `table`, `scan`, and `get` of one transaction. `table` gives nil when there is no table of the name. |
| `writer` | A `reader`, with `createTable`, `dropTable`, and `upsert`. It reads its own writes. |
| `database` | `begin` and `read`, which give the transactions. |
| `readTx` | A `reader`, with `position` and `end`. |
| `writeTx` | A `writer`, with `position`, `annotate`, `commit`, and `rollback`. |

A name of a table is compared as SQLite compares names: without regard to the case of the ASCII letters, and every other character exactly.

The adapters `coreDB`, `coreTx`, and `coreReadTx` put the interfaces on a `*core.DB`, a `*core.Tx`, and a `*core.ReadTx`. They change two things:

- **A lookup of a table that does not exist gives nil,** where the core gives an error that wraps `core.ErrNoTable` (`lookUp`). The operators turn the nil into SQLite's error.
- **A refusal of the core becomes an internal error** (`fromCore`). The refusals are `core.ErrTableExists`, `core.ErrNoTable`, `core.ErrReservedName`, and `core.ErrInvalid`. The engine checks for each before it calls the core, so one that the core still returns is a fault of the engine or of the pass after the parser. So is `core.ErrTxDone`, a call on a transaction that has ended. The error of `Tx.Commit` passes through as it is: its `core.ErrInvalid` is a commit time that the commit object cannot hold, which comes from the clock.

Every other error of the core passes through as it is.

The interfaces exist for the tests:

- **The operators are tested alone, against a fake.** `fake` in `operator_internal_test.go` keeps tables in memory, records every call, and fails or panics on demand.
- **The fuzz test runs on a database in memory.** `memory` in `memory_internal_test.go` is fast enough for many more sequences than the core is.

## The operators

Operators are of two kinds. A source gives rows to the operator above it. A sink runs a statement that changes the database, and gives the number of rows that it changed.

| Operator | Kind | What it does |
| --- | --- | --- |
| `scan` | source | Gives the rows of a table in the order of its primary key. It checks the context before each row, and ends with the error of the context when the context has ended. |
| `values` | source | Gives rows of constants. It checks the context before each row too. |
| `project` | source | Gives the columns of each row of its source at its positions. For `*`, they are all the columns, in the order of the table. |
| `insert` | sink | Writes each row of its source into a table as a new row. |
| `createTable` | sink | Creates a table. |
| `dropTable` | sink | Drops a table, by the name that the table stores. |

- **The operators build only the plans of slice 1.** A plan of another shape fails with an internal error: a query of anything but `*` from a table, an insert of anything but one row of one value, a row without a value for a column, a key of several columns, and a table of several columns. What SQLite gives for these has not been compared with SQLite, so the engine gives no answer of SQLite's for them. A slice that adds such a shape replaces its internal error with what the differential test has compared with SQLite.
- **`insert` checks each key itself.** The core has only an upsert, which replaces a row with the same key. So `insert` calls `get` for the key of each row, inside the write transaction, before it calls `upsert`. If the table has the key, also from an earlier row of the same statement, `insert` fails with `UNIQUE constraint failed`.
- **A source is a push-style iterator,** an `iter.Seq2`. The operator above ranges over the rows, and ends the source by leaving the loop. Everything runs on the goroutine of the caller.
- **No operator uses `iter.Pull`.** It starts a goroutine, and [`simlint`](../simlint/simlint.go) forbids it in code that runs under the simulation.

## How to add a statement of the next slice

1. Add a type or a field to `stmt`. Its README has [the steps](../sql/stmt/README.md#how-to-add-the-next-slice), which start with the agreement on the slice in the roadmap.
2. Add a plan node to `plan.go`, with a case in `NewPlan` and in `Format`.
3. Add an operator to `operator.go`, and build it in `buildNode`, `buildValues`, or `buildSink`. Make the lookups there in the order in which SQLite gives their errors. Replace the internal error of each shape of a plan that the slice adds.
4. Add a constructor to `errors.go` for each new error of SQLite, and pin its codes, its message, and the form of each name against SQLite in `TestEngine_errors`.
5. Add the kind of the statement to `report.of` in `telemetry.go`. If the statement sets SQLite's count of changes, set `SetsChanges` for it in `Exec` and `Query`.
6. Extend the generator in [`stmttest`](../sql/stmt/stmttest/stmttest.go), and `sequence` in `differential_test.go`: the mix of statements in `next`, and the near misses in `others`.
7. Extend `fake` and `memory` to what the new operator calls.
8. Update this README.

## Errors

SQLite's errors have one type, `*sqlerr.Error` of [`internal/sql/sqlerr`](../sql/sqlerr/sqlerr.go), which the syntax errors of the pass have too. It holds `Code`, `ExtendedCode`, and `Message`, each exactly as SQLite gives it. `Offset` is extra information, and s4ql does not promise it.

| Message | Code | Extended code | The names in the message |
| --- | --- | --- | --- |
| `table %s already exists` | 1 | 1 | `Name.Raw` of the statement: the name as written, with its quotes. |
| `no such table: %s` | 1 | 1 | `Name.Value` of the statement: the name without its quotes. |
| `table %s has no column named %s` | 1 | 1 | `Name.Value` of the statement, for both. |
| `UNIQUE constraint failed: %s.%s` | 19 | 1555, `SQLITE_CONSTRAINT_PRIMARYKEY` | The names as the table stores them, which are those of its CREATE TABLE. |
| A syntax error, such as `near "frm": syntax error` | 1 | 1 | From the pass. |

Examples, each pinned against SQLite in `TestEngine_errors`:

- With a table `t1`, `create table "T1" (x int primary key) strict, without rowid` gives `table "T1" already exists`.
- `select * from "T""2"` gives `no such table: T"2`.
- With a table `t1`, `insert into "T1" ("V""2") values (1)` gives `table T1 has no column named V"2`.
- After `create table "Foo bar" ("Bar""x" int primary key) strict, without rowid`, a second insert of the same key through `"FOO BAR" ("BAR""X")` gives `UNIQUE constraint failed: Foo bar.Bar"x`.

The order of the errors is SQLite's:

- **A table that does not exist comes before a column that does not exist.** `insert into t2 (v2) values (1)` without `t2` gives `no such table: t2`.
- **A syntax error never depends on the database.** The spec: "Text with a syntax error gets the error that SQLite gives for it on an empty database." So `stmt.Check` alone decides it, before the engine begins a transaction, and a text with a syntax error gives its syntax error also on a closed instance, without the lease, and with a context that has ended. SQLite differs on a database that is not empty: it starts to create a table as soon as it has read `CREATE TABLE`, the name, and the `(` or `AS` after it, and if a table of the name exists, it gives `table %s already exists` and never meets the syntax error. With a table `t1`, SQLite gives `table t1 already exists` for `create table t1 (v1 int primary key) strict without rowid`, and the engine gives `near "without": syntax error`, as SQLite does on an empty database. This is the one difference from SQLite that the tests accept: `startedTable` in `differential_test.go` accepts it where `stmttest.StartedTable` gives the name of the table that the text starts, SQLite gives that the table exists with the name as written, and the engine gives what SQLite gives on an empty database. The other texts where SQLite gives an error that is not the syntax error are in [the known gaps of `stmt`](../sql/stmt/README.md#known-gaps).

The errors that are not SQLite's:

- **"Not supported"** is the `*stmt.NotSupportedError` of the pass, unchanged. It matches `stmt.ErrNotSupported`.
- **A text without a statement** gives `ErrNoStatement`.
- **An error of the core passes through unchanged,** so that `errors.Is` finds its sentinel, such as `core.ErrStopped` or `core.ErrOutcomeUnknown`. So does an error of the context. Neither is ever given as a `*sqlerr.Error`.
- **A refusal of the core becomes an error that matches `ErrInternal`** and wraps nothing else; see [The storage interface](#the-storage-interface). It is a fault of s4ql, and never SQLite's error.

## Changes and the last insert rowid

The engine gives what is needed to report `sqlite3_changes` and `sqlite3_last_insert_rowid` as SQLite 3.54.1 does. The differential test compares both with SQLite after every statement.

- **An insert that succeeds sets the count of changes to 1.** Its result has `Changes` 1 and `SetsChanges` true.
- **CREATE TABLE, DROP TABLE, and SELECT leave the count as the statement before left it.** Their result has `SetsChanges` false.
- **An insert that fails with `UNIQUE constraint failed` sets the count to 0.** SQLite gives this error when it steps the statement. The engine gives `Result{SetsChanges: true}` with the error of an insert that fails after its checks: in its operator, in its annotation, or in its commit.
- **An error that SQLite gives when it prepares the statement leaves the count:** `no such table`, `has no column named`, `already exists`, and a syntax error. A text without a statement leaves it too.
- **The last insert rowid never changes.** Every table is WITHOUT ROWID, and SQLite does not record an insert into such a table. The value is 0 on a new connection, and stays 0.

The engine keeps no count between statements. A connection that reports the counts as SQLite does keeps one number, its count of changes, which starts at 0. After a statement whose result has `SetsChanges`, also one that failed, the count is `Result.Changes`; after any other statement, the count stays.

Its last insert rowid is always 0.

## Annotations

- **Each write that is committed carries one annotation: the text of its statement,** from its first token to its end. The end is that of its semicolon, if it has one.
- **Comments and semicolons before the statement, and the text after it, are left out.** A comment inside the statement stays. `  /* before */ create table t1 (v1 int primary key) strict, without rowid; -- after` is annotated as `create table t1 (v1 int primary key) strict, without rowid;`.
- **An annotation has no parameters.** Slice 1 has none.
- **A statement that fails commits nothing,** and leaves no annotation. A query leaves none either.
- **Every write of slice 1 that succeeds changes something.** The core writes no commit object for a transaction that changed nothing, and would then store its annotation nowhere. No statement of slice 1 meets that case: a create, an insert of a new key, and a drop each change the database.

## Events

The engine logs one event for each statement, named `statement`, when the statement ends. For a query, that is when its rows end. The names are in `telemetry.go`.

| Attribute | Value | Present |
| --- | --- | --- |
| `statement.kind` | `create_table`, `drop_table`, `insert`, or `select`. | When the text is a supported statement. |
| `statement.table` | The name of the table, as its value. | When the text is a supported statement. |
| `statement.sql` | The text that the call was given. | When the level of the event is Debug. |
| `statement.position` | The last commit object of the state that the statement saw: the position of its read transaction, or the parent of its write transaction. | Once the statement has a transaction. |
| `commit.parent` | The same value, under the core's name. | For a statement in a write transaction. |
| `statement.rows` | The number of rows that the statement returned or changed. | Always. |
| `outcome` | How the statement ended, in a word; see below. | Always. |
| `sqlite.code`, `sqlite.extended_code` | The result codes. | For SQLite's error. |
| `error` | The text of the error. | When the statement failed. |
| `duration.total` | The duration from the call to the end, on the monotonic clock of the host. | Always. |

The level follows the conventions of the core, so a healthy instance logs nothing at the default level, Info:

| Level | `outcome` | When |
| --- | --- | --- |
| Debug | `ok` | The statement succeeded. |
| Debug | `sqlite_error` | It failed with SQLite's error, which is an answer to the application. |
| Debug | `not_supported` | It is not supported. |
| Debug | `no_statement` | The text holds no statement. |
| Debug | `refused` | The core refused it with `ErrNoLease` or `ErrStopped`. The core logs that itself. |
| Debug | `canceled` | Its context ended. |
| Warn | `core_error` | It failed with another error of the core, such as a commit of unknown outcome. |
| Error | `internal_error` | It failed with an internal error, or panicked in the engine. |

- **A statement that panics in the engine logs one event,** with the outcome `internal_error` and the error `s4ql: internal error: the statement panicked`, and the panic goes on to the caller. This holds for a panic while the statement is checked, planned, or built, while a write runs, and while a source gives the rows of a query. A panic in the body of a loop over `Rows.All` is the application's, and ends the rows as a break does: the event has the outcome `ok` and the rows given until then.
- **The event of a write statement joins the core's event of its commit** by `commit.parent`, which is the same in both, and by the context, which both are logged with.
- **The SQL text is only in an event at Debug,** because it can hold the application's data. An event at Warn or Error has no text.
- **No value of a row is in any event.**

## How it is tested

SQLite is the judge. `oracle` in `differential_test.go` runs each text on a database in memory through the binding in [`internal/sqlite`](../sqlite/README.md), as `sqlite3_prepare_v2` and `sqlite3_step` run it.

The layers:

1. **The differential test.** `TestDifferential` runs random sequences of statements on SQLite and on the engine, which runs on the real core with a simulated store. After each statement it compares the answer: the error as code, extended code, and message, or the columns and the rows. It also compares the count of changes, and requires a last insert rowid of 0. A statement that the engine does not support is not given to SQLite. Every second sequence restarts the instance at random points, with an empty cache, which replays the log, or with its own cache. After each restart and at the end, the test compares every table and every row.

   A sequence comes from `sequence`. Its statements meet at one or two tables and columns, each in several spellings: some that SQLite takes for the same name, and some that it takes for another. The values come from a small set, so keys repeat. One statement in twelve is a text that is not supported, a syntax error, or no statement. Some queries run through `Exec`, and some other statements through `Query`.
2. **Fuzzing.** `FuzzDifferential` runs the sequence that its input chooses on the database in memory. `FuzzDifferential_core` runs it on the core, with or without restarts.
3. **The simulation.** `TestSimulation` runs statements under the scheduler of [`internal/sim`](../sim/README.md) while the store fails, hangs, and loses requests. The default workload has two instances that compete for the lease, two callers on each, one crash, and one close. After a crash, a new instance starts on a copy of the cache or on an empty one. The hammer workload has one instance with four callers that hardly wait, so that two statements often run at once. Each statement has a timeout, so a commit can end with its outcome unknown. Each statement carries an ID in a comment, which its annotation keeps, and its event gives the state that it saw. The first three seeds run twice, and the traces must be the same.

   `run.verify` checks each run against SQLite and the log:
   - Every commit object of the log holds one statement. It is of a call that succeeded, that had an unknown outcome, or whose instance crashed while it ran. No statement is in the log twice.
   - Every write that was acknowledged is in the log.
   - The statements of the log, run in SQLite in their order, each succeed.
   - A statement that failed with SQLite's error gives that error in SQLite on the state that it saw. A query that succeeded gives its rows there. That state is the state after a commit of the log, and it holds every write that was acknowledged before the statement started.
   - A new instance with an empty cache replays the log to the state that SQLite has after it.

   One query in four is not read to its end: its loop stops at the first row, it is closed unread, or its consumer panics. Its rows are not compared, and the rest of the run must still hold. A statement that saw no state, because it failed before it had a transaction, is a syntax error, and must give what SQLite gives for it on an empty database.

   A run also fails in five more cases: a statement gives an error that the workload does not expect, as an internal error is; an instance stops; a close does not end within five minutes; no instance runs a write within ten minutes after the store has calmed; or a live instance does not take all its read connections at once within ten minutes after that, which a read transaction left open prevents. Only the instance that holds the lease can read, so each instance that has taken its read connections is closed, which hands the lease to the next.
4. **Planted bugs.** `TestSimulation_plantedBugs` plants each of five bugs in the engine, and runs seeds until two have found it. A seed finds a bug only with the violation that the bug causes: a commit that fails in SQLite for `skipUnique` and `checkOutside`, an acknowledged statement that is not in the log for `ackUnknown`, no write after the workload for `keepOpen`, and an instance that cannot take its read connections after the workload for `keepRead`. Each seed that finds a bug runs again without it, and must find nothing. The bugs are fields of `bugs` in `bugs.go`, which only the test files can set, through `Options.Plant` in `export_test.go`.

   | Bug | The wrong decision | Workload |
   | --- | --- | --- |
   | `skipUnique` | An insert writes its row without the check of its key, so the row replaces one. | Default |
   | `ackUnknown` | A statement whose commit has an unknown outcome succeeds. | Default |
   | `checkOutside` | An insert checks its table, its column, and its key in a read transaction before its write transaction. | Hammer, because the bug needs two inserts of one key on one instance at once. |
   | `keepOpen` | A write statement that fails leaves its write transaction open. | Default |
   | `keepRead` | `Rows.Close` of rows that no loop has read leaves the read transaction open. | Default |
5. **The operators alone.** `operator_internal_test.go` runs each operator, `buildQuery`, and `buildSink` against `fake`, and `engine_internal_test.go` the engine's handling of transactions and its events: the order of the calls, an early stop, a context that ends, a failure at each step, a panic, and each planted bug.
6. **The pinned cases.** `TestEngine_errors` pins each error against SQLite. `TestNewPlan` pins the plans, `TestEngine_annotations` the annotations as the log holds them, and `TestEngine_events` the events.
7. **No transaction left open.** `TestEngine_noneOpen` runs statements of a sequence, each through `Exec` or through `Query`. It consumes the rows of `Query` in one of four ways: to the end, with a loop that stops at once, with a close and no loop, or with a loop that panics. After each statement, a write transaction and four read transactions, one for each read connection of the instance, must all begin. A second case plants `keepOpen`, and shows that a begin then waits.
8. **The names of the core.** `TestEngine_namesOfTheCore` runs the four statements on every name of the generator's pools and on random names in double quotes, of every kind of character. Wherever the pass supports the name, the core must take it, and the answer must be SQLite's.
9. **Concurrency.** `TestEngine_concurrent` runs inserts and queries from eight goroutines at once, for the race detector.
10. **Mutation testing.** [`internal/sql/mutation`](../sql/mutation/README.md) mutates this package too, and runs the simulation as a layer of its own.

Under `-short`, the race detector, or the address sanitizer, `TestDifferential`, `TestSimulation`, `TestEngine_noneOpen`, and `TestEngine_namesOfTheCore` run a tenth of their sequences, seeds, statements, and names, and the simulation at least two seeds.

**The simulation runs no crashes under the address sanitizer.** A crashed database stays open until the test binary ends, because only the core's own tests can close the cache of a crashed instance. The sanitizer reports what is left open.

The flags of the test binary:

- `-seed` is the first seed of the sequences of statements. The default is 0.
- `-sequences` is the number of sequences of `TestDifferential`. The default is 300.
- `-length` is the number of statements in each sequence. The default is 40.

The environment variables:

- `S4QL_ENGINE_SIM_SEEDS` sets the number of seeds of each workload of `TestSimulation`, 50 by default. For `TestSimulation_plantedBugs` it sets the limit, 100 by default, and makes the test run every seed up to it, to count how many find each bug.
- `S4QL_ENGINE_SIM_SEED` makes `TestSimulationSeed` run that one seed of the default workload and log its whole trace.
- `S4QL_ENGINE_SIM_BUG` plants the bug of that name in the run of `TestSimulationSeed`.

Examples of larger runs:

```sh
go test ./internal/engine/ -run '^TestDifferential$' -sequences 10000 -length 40
go test ./internal/engine/ -run '^$' -fuzz '^FuzzDifferential$' -fuzztime 10m
go test ./internal/engine/ -run '^$' -fuzz '^FuzzDifferential_core$' -fuzztime 10m
S4QL_ENGINE_SIM_SEEDS=1000 go test ./internal/engine/ -run '^TestSimulation$'
S4QL_ENGINE_SIM_SEEDS=1000 go test ./internal/engine/ -run '^TestSimulation_plantedBugs$'
S4QL_ENGINE_SIM_SEED=7 S4QL_ENGINE_SIM_BUG=skipUnique go test ./internal/engine/ -run '^TestSimulationSeed$' -v
```

## Results

On the code as it is now, on 2026-11-18:

| Layer | Run | Result |
| --- | --- | --- |
| `TestDifferential` | 10,000 sequences of 40 statements, from seed 2,000,000, half with restarts | 400,000 statements, none that differ from SQLite. By kind and outcome: CREATE TABLE 24,322 ok and 49,354 `already exists`; INSERT 65,978 ok, 25,683 `UNIQUE constraint failed`, 31,236 `has no column named`, and 60,490 `no such table`; SELECT 61,392 ok and 30,265 `no such table`; DROP TABLE 12,266 ok and 5,867 `no such table`; and of the other texts 21,687 not supported, 7,647 `syntax error`, 1,397 `incomplete input`, 985 `already exists` after the start of a table, and 1,431 without a statement |
| `FuzzDifferential` | 50,000,000 executions in 2 h 57 min on 8 workers, while other work loaded the machine | nothing found; 15 new inputs of interest in the run, none in its last hour |
| `FuzzDifferential_core` | 90,288 executions in 5 min on 6 workers | nothing found |
| `TestSimulation`, default workload | 1,000 seeds | 95,196 statements: 13,318 that succeeded, 20,385 with SQLite's error, 55,405 refused or ended by their timeout, 572 commits of unknown outcome, 29 cut by a crash, and 5,487 not supported or without a statement; 8,364 commits; no violation |
| `TestSimulation`, hammer workload | 1,000 seeds | 120,000 statements: 28,001 that succeeded, 40,610 with SQLite's error, 43,360 refused or ended by their timeout, 1,078 of unknown outcome, and 6,951 not supported or without a statement; 15,155 commits; no violation |
| `TestEngine_namesOfTheCore` | 2,832 names | 11,328 statements compared with SQLite, none that differ, and no internal error |

The planted bugs:

| Bug | Seeds that found it | Seeds that found anything without the bug |
| --- | --- | --- |
| `skipUnique` | 124 of 200 | 0 |
| `ackUnknown` | 47 of 200 | 0 |
| `checkOutside` | 73 of 200, in the hammer workload | 0 |
| `keepOpen` | 193 of 200 | 0 |

Mutation testing: of 955 mutants, 312 do not compile, 632 are killed, none times out, and 11 are equivalent; none is unexplained. The score is 98.29%, and 100% without the equivalent ones. The first run found 74 survivors, each a gap in the tests but the equivalent ones; see [`internal/sql/mutation`](../sql/mutation/README.md#the-pass-after-the-parser-and-the-engine).

### Run times

Measured on an Apple M4 with 10 cores, at a load of about 12 from other work:

- `go test ./internal/engine` takes about 8 s, `-short` about 2.5 s, and `-race` about 7 s.
- Under the address sanitizer, in a Linux container in Docker on the same machine while a fuzz run used 8 of its cores, the tests took 56 s, and each fuzz test of `make fuzz` runs for `FUZZTIME` on top. `make test-asan FUZZTIME=10s` passed in 10 min 45 s.
