# The engine

`internal/engine` runs one statement of [slice 1](../../docs/roadmap.md#slice-1-one-integer-column), with the parameter of [slice 2](../../docs/roadmap.md#slice-2-parameters) and the five column types of [slice 4](../../docs/roadmap.md#slice-4-the-other-types), on a database of the [storage core](../core/README.md), a `*core.DB`, and gives SQLite's results and SQLite's errors. A statement is its own transaction, unless a transaction that spans statements is open on its connection; see [Transactions](#transactions). What a statement of slice 1 is, token for token, is in [the README of `stmt`](../sql/stmt/README.md#slice-1).

The interface:

| Name | What it does |
| --- | --- |
| `New(d, host, Options)` | Gives an `*Engine` for the database `d`. The engine reads the monotonic clock of the host, for the duration in its events. `Options.Logger` takes the events, and the default discards them. |
| `Engine.Conn()` | Gives a `*Conn`, a connection of the engine, which holds nothing until it runs a statement. See [Connections](#connections). |
| `Conn.Exec(ctx, src, args...) (Result, error)` | Runs the statement of the text `src`, with the arguments `args`, each an `*s4qlpb.Value`, for its parameters by position. A query is run to its end, and its rows are dropped. The result comes with an error too. See [Parameters](#parameters). |
| `Conn.Query(ctx, src, args...) (*Rows, error)` | Runs the statement with its arguments and gives its rows. A statement that is not a query is run as `Exec` runs it. Its rows have no columns and no rows, and `Rows.Result` gives its result; Query gives these rows also with its error. |
| `Conn.Begin(ctx) error` | Begins a write transaction on the connection. It waits for the place of the single writer as long as `ctx` allows. See [Transactions](#transactions). |
| `Conn.BeginReadOnly(ctx) error` | Begins a read-only transaction on the connection, which sees the state of its beginning. It waits for nothing. |
| `Conn.Commit(ctx) error` | Commits the transaction of the connection. The transaction is over when `Commit` returns, also with an error. |
| `Conn.Rollback(ctx) error` | Rolls the transaction of the connection back: nothing that it did is kept. |
| `Conn.Close(ctx) error` | Closes the connection, and its reader, if it has one. It rolls back a transaction that is open on the connection. |
| `Rows.Columns()` | The columns of each row, each a `Column` with the name as the table stores it and the declared type, such as `INT` or `TEXT`. |
| `Rows.All()` | The rows in order, as an `iter.Seq2[[]*s4qlpb.Value, error]`. The sequence ends at the first error, and can be used once. |
| `Rows.Close()` | Ends the rows. |
| `Rows.Result()` | The result of a statement that is not a query. |
| `Result{Changes, SetsChanges}` | See [Changes and the last insert rowid](#changes-and-the-last-insert-rowid). |
| `ErrNoStatement` | The error of a text that is empty or holds only spaces, comments, and semicolons. SQLite prepares such a text into no statement and gives no error. |
| `ErrArguments` | Matched by the error of a statement that is given another number of arguments than it has parameters. |
| `Check(src, n) error` | The errors of the text `src` and of the number `n` of its arguments, as `Exec` gives them, without the database and without an event. |
| `ErrTooLarge` | Matched by the error of a commit whose commit object the store refuses as too large. The error also matches `core.ErrTooLarge`. |
| `ErrRowsOpen` | The error of a begin, and of a statement in a transaction, while the rows of an earlier query of the connection have not ended. Matched by the error of a commit of a write transaction while a loop reads its rows. |
| `ErrTxOpen` | The error of a begin on a connection whose transaction is open. |
| `ErrNoTx` | The error of a commit or a roll back on a connection without a transaction. |
| `ErrTxDone` | The last error of rows whose transaction ended while a loop over them ran. |
| `ErrInternal` | Matched by the error of a fault of s4ql. See [Errors](#errors). |

An `Engine` is safe for concurrent use. A `Conn` is not, and `Rows` are not.

## Connections

A statement runs on a `Conn`, which is to the engine what a connection of a pool is to its database: it runs one statement at a time. The driver gives each connection of `database/sql` a `Conn` of its own.

- **A `Conn` owns one reader of the core,** a `*core.Reader`, on which every read transaction of the connection runs. It opens the reader for its first statement that reads, so a connection that only writes has none, and closes it in `Conn.Close`. So the connections that are open are the queries that can run at once, and nothing in the engine or in the core limits their number.
- **A connection runs one read at a time.** A statement that reads while the rows of an earlier query of the same connection have not ended fails at once, with an error that matches `core.ErrReaderBusy`. It does not wait for the rows, and [the README of the core](../core/README.md#concurrency) says why. A statement that changes the database needs no reader, and runs. This is the rule for a statement of its own. In a transaction, open rows refuse every statement; see [Rows in a transaction](#rows-in-a-transaction).
- **A reader that cannot be opened fails the statement that needed it,** and the next statement that reads tries again. If the cache cannot be opened, that is a local failure, which stops the instance as any other.
- **`Conn.Close` returns `core.ErrReaderInUse`, and closes nothing, while a loop over the rows of a query of the connection runs.** Rows of the connection that have not ended must not be used after its close: their read transaction has ended with the reader, and what they give then is an internal error. A statement on a closed `Conn` is an internal error, and so is a begin. Closing a closed `Conn` does nothing.
- **`Conn.Close` rolls back a transaction that is open on the connection,** which frees the place of the writer.
- **Why a `Conn`, and not a reader that is passed to each call.** A statement decides whether it reads only after it is parsed, so the engine must be the one to open the reader. A connection is also where the state of a connection belongs, such as a transaction that spans statements. maragubot chose the `Conn` on 2026-11-19, after Markus had agreed that each connection of the pool has a reader of its own. `Engine.Exec` and `Engine.Query` are gone.

## One statement, one transaction

A statement is its own transaction of the core, unless a transaction is open on its connection. This section is about a statement of its own. [Transactions](#transactions) is about a statement in a transaction that spans statements.

- **A statement that changes the database runs between `DB.Begin` and `Tx.Commit`.** The engine annotates the transaction with the text of the statement before the commit; see [Annotations](#annotations). Any failure rolls the transaction back, and so does a panic of an operator. A commit ends the transaction whatever it gives.
- **A query runs in a read transaction of the reader of its connection,** which `Reader.Read` starts. Its `Rows` hold the read transaction, and with it the reader, until they end. They end when `Rows.All` has given the last row or an error, when the loop over it stops early, when the loop panics, or at `Rows.Close`. One of these must happen.
- **`Rows.Close` during a loop over `Rows.All` ends the loop at its next row,** and the read transaction ends then. The loop holds the transaction also while it is suspended in its body, so an iterator that is suspended must be resumed or stopped for the transaction to end.
- **A failure of the end of the read transaction after a loop that stopped early is the error of the next `Rows.Close`.**
- **A panic while a query is built ends its read transaction too.**
- **`Rows.All` yields to the host between two rows,** so that under the simulation other tasks run while a query is open.
- **A failure of the end of the read transaction after the last row is the last error of `Rows.All`.**

## Transactions

A transaction that spans statements is open on a `Conn` from `Conn.Begin` or `Conn.BeginReadOnly` to `Conn.Commit` or `Conn.Rollback`. Every statement of the connection runs in it meanwhile. The code is in `tx.go`.

The product owner decided the design:

- **A write transaction takes the place of the single writer when it begins,** as SQLite's `begin immediate` does, and holds it until its commit or its roll back. It sees the newest state that is committed, and its own writes. Nothing outside sees its writes before its commit.
- **A read-only transaction is a read transaction.** It sees the state of its beginning, never writes, takes no place of a writer, and blocks no one.

The builder decided the details below.

### The begin

- **`Begin` waits for the place of the writer as long as its context allows.** It has no deadline of its own. If the context ends first, `Begin` gives the error of the context. On an instance that does not serve, it fails as a write of its own does, with an error that matches `core.ErrNoLease` or `core.ErrStopped`.
- **`BeginReadOnly` waits for nothing.** It starts a read transaction on the reader of the connection, and fails as a query does.
- **Both fail with `ErrTxOpen` while a transaction is open on the connection.**
- **Both fail with `ErrRowsOpen` while the rows of an earlier query of the connection have not ended,** also the rows of a statement of its own.
- **When a begin fails, nothing has begun,** and the engine logs no event for it.

### A statement in a transaction

- **The text and the arguments are checked first,** as everywhere. Three checks follow, in this order (`Conn.ready`):
  1. The context has not ended. If it has, the statement fails with the error of the context.
  2. The instance serves now, by the clocks, as a statement of its own needs it. The engine asks `Tx.Serving` or `ReadTx.Serving` of the core. If the instance does not serve, the error matches `core.ErrNoLease` or `core.ErrStopped`.
  3. No rows of an earlier query of the connection are open. If some are, the statement fails with `ErrRowsOpen`.
- **Only `Serving` looks at the clocks.** The other calls of an open transaction of the core do not, because the commit of a write transaction is judged by its manifest swap.
- **In a write transaction, a statement that changes the database runs between a savepoint and its release:** `Tx.Savepoint` and `Tx.Release` of the core, on SQLite's `savepoint s4ql_statement`. The engine annotates the statement before the release. A query takes no savepoint.
- **A statement that fails has no effect, and the transaction stays open.** When the statement fails, or an operator panics, the engine calls `Tx.RollbackTo`. That call restores the cache, and what the transaction had recorded of each table for its commit object, which is its net effect. The work of the statements before it is kept, as in SQLite.
- **The engine annotates last, because an annotation cannot be taken back** from its spool file. A roll back to a savepoint before an annotation makes the core fail the transaction. The engine never makes such a roll back.
- **After an error of the cache, the core lets the transaction only roll back.** Every later statement fails, the commit fails, and the roll back works.
- **`create table` and `drop table` are transactional.** The later statements of the transaction see them, and a roll back undoes them.
- **The texts `begin`, `commit`, `end`, `rollback`, `savepoint`, and `release` stay "not supported",** also inside a transaction, and the transaction stays open.

### A read-only transaction

- **Every statement sees the state of the beginning,** whatever commits meanwhile.
- **A statement that would write fails with SQLite's `attempt to write a readonly database`,** with the code 8 and the extended code 8, as in SQLite under `pragma query_only = 1`. SQLite's errors of the names come first: `no such table`, `has no column named`, and `already exists`. SQLite gives them when it prepares the statement, and refuses the write when it steps it. The engine looks the names up in the state of the transaction.
- **The transaction stays usable after such an error.**
- **`Commit` and `Rollback` end it, and write nothing.** On an instance that has stopped they return nil, because there is nothing to commit.

### The commit and the roll back

- **A write transaction commits as a statement of its own does.** All its changes are one commit object, and every read that starts after `Commit` has returned nil sees them.
- **The transaction is over when `Commit` returns,** with or without an error. `Rollback` ends it too, whatever it returns.
- **`Commit` and `Rollback` on a connection without a transaction give `ErrNoTx`.**
- **A transaction that changes nothing in the end writes no commit object, and its commit succeeds.** This is the core's rule. Examples are a create and a drop of the same table, a transaction whose statements all failed, and one that only read.
- **The commit object holds, in order, the statements of the transaction that succeeded and that write,** each with its arguments. It holds no statement that failed and no query. See [Annotations](#annotations).
- **A commit object that the store refuses as too large gives an error that matches `ErrTooLarge`.** The error also matches `core.ErrTooLarge`, and its message starts with `s4ql: the transaction is too large to commit: `. Nothing is committed. A statement of its own gets the same error. The tests set the limit of the store to 4 KiB with `simstore.Options.MaxSize`, which the commit object of 100 inserts exceeds.
- **An instance that has lost its lease ends the write transaction that is open,** unless it is committing, so that it can take the lease back; the core decides this (`DB.endForLease`). Every later statement and the commit of the transaction fail with an error that matches `core.ErrTxLeaseLost` and `core.ErrNoLease`, `Rollback` returns nil, and the connection is usable after either. Rows of a query of the transaction that a loop reads end at their next row with that error. The event `transaction` of such a transaction has the outcome `lease_lost`, at Debug, with the error, and `transaction.end` is what the application called; a statement refused so has the outcome `lease_lost` too. The transaction's writes are never in the log.
- **On an instance that has lost its lease or has stopped, the commit fails and the roll back works.** The statements fail as said above. `Commit` gives the error of the core. One that matches `core.ErrNoLease` or `core.ErrStopped` means that nothing is committed. One that matches `core.ErrOutcomeUnknown` means what it means for a statement of its own. `Rollback` frees the place of the writer, and returns nil, except after a stop by a local failure, where the roll back in the cache can give its error. The spec: "A write transaction that is already open commits only if its manifest swap still succeeds."

### Rows in a transaction

The rows of a query in a transaction read the state of the transaction. Their end does not end the transaction.

- **While the rows of a query of the transaction are open, every statement of the connection fails with `ErrRowsOpen`,** also a write. The scan holds a statement of SQLite on the connection of the transaction.
- **`Commit` and `Rollback` first end the rows that no loop reads.** A later `Rows.All` on them gives nothing.
- **Rows that a loop over `Rows.All` reads cannot be ended from outside.** A loop reads them while its body runs, and while its iterator is suspended. `Commit` and `Rollback` mark such rows. The loop then ends at its next row with `ErrTxDone`, and reads nothing more.
- **The end of the transaction runs when that loop ends,** after the scan of the core has ended. It is the roll back of a write transaction, or the end of a read transaction. `Rows.Close` after the loop gives the error of that end, which is nil when the end worked.
- **Until the loop ends, a write transaction keeps the place of the writer.** Every statement and every begin of the connection fail with `ErrRowsOpen`, and `Conn.Close` gives `core.ErrReaderInUse`.
- **A `Commit` of a write transaction does not commit in that case.** It rolls the transaction back, and fails with an error that matches `ErrRowsOpen` and ends with `so the transaction is rolled back and not committed`. A `Commit` of a read-only transaction ends it, and returns nil.
- **Why the end is left to the loop.** SQLite cannot commit while a statement of the connection reads: the commit of the core drops temporary tables, and SQLite refuses that then. A roll back can run then, but it aborts the statement, and the loop would finish the aborted statement later, on the write connection, while the transaction of the next writer runs there. So the roll back waits for the loop. A commit cannot be left to the loop in the same way, because the transaction is over when `Commit` returns: the transaction is rolled back, and the error of `Commit` says so.
- **A driver whose `Rows.Close` ends the loop does not meet this case.** `database/sql` closes the rows of a `sql.Tx` before it calls `Commit` or `Rollback` of the driver.

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

A plan is a `*CreateTable`, a `*DropTable`, an `*Insert`, or a `*Query`. An insert and a query read their rows from a tree of nodes: `*Scan`, `*Values`, and `*Project`. The expressions are `*Integer` and `*Parameter`, which `Format` prints as `?1` by its number.

**Why a tree of iterators, and not a bytecode machine like SQLite's.** The lead recommended it on 2026-11-18, and Markus left the choice to the lead: "I'll trust your recommendation." Each operator is tested alone, a plan is data that tests can read, and the oracle stays rows and errors compared with SQLite, which does not change between SQLite's releases. A bytecode machine that mirrors SQLite's would give `EXPLAIN` as a second oracle and SQLite's order of checks more cheaply, but it ties the engine to an instruction set that SQLite says is not an interface. Where SQLite's order of checks or of errors is visible, the engine reproduces it on purpose, and the differential tests find the places. The plan is the seam: an executor of another kind can replace the operators behind it without touching the parser or the pass after it. The trade-offs are in [research/sql-front-end.md](../../docs/research/sql-front-end.md) §6.3.

## The storage interface

The operators and the engine do not call the core. They call six interfaces in `storage.go`:

| Interface | Holds |
| --- | --- |
| `reader` | `table`, `scan`, and `get` of one transaction. `table` gives nil when there is no table of the name. |
| `writer` | A `reader`, with `createTable`, `dropTable`, and `upsert`. It reads its own writes. |
| `database` | `begin`, which gives a write transaction, and `reader`, which gives a reader that its owner closes. |
| `readConn` | A reader of the database: `read`, which gives a read transaction and fails while the one before it on the reader is open, and `close`. |
| `readTx` | A `reader`, with `position`, `serving`, and `end`. |
| `writeTx` | A `writer`, with `position`, `annotate`, `serving`, `savepoint`, `release`, `rollbackTo`, `commit`, and `rollback`. One savepoint is open at a time. |

Only a transaction that spans statements calls `serving`, `savepoint`, `release`, and `rollbackTo`; see [A statement in a transaction](#a-statement-in-a-transaction).

A name of a table is compared as SQLite compares names: without regard to the case of the ASCII letters, and every other character exactly.

The adapters `coreDB`, `coreTx`, `coreReader`, and `coreReadTx` put the interfaces on a `*core.DB`, a `*core.Tx`, a `*core.Reader`, and a `*core.ReadTx`. They change two things:

- **A lookup of a table that does not exist gives nil,** where the core gives an error that wraps `core.ErrNoTable` (`lookUp`). The operators turn the nil into SQLite's error.
- **A refusal of the core becomes an internal error** (`fromCore`). The refusals are `core.ErrTableExists`, `core.ErrNoTable`, `core.ErrReservedName`, and `core.ErrInvalid`. The engine checks for each before it calls the core, so one that the core still returns is a fault of the engine or of the pass after the parser. So are `core.ErrTxDone`, a call on a transaction that has ended, and `core.ErrReaderClosed`, a read on a reader that is closed. The error of `Tx.Commit` passes through as it is: its `core.ErrInvalid` is a commit time that the commit object cannot hold, which comes from the clock.

Every other error of the core passes through as it is.

The adapters pass `serving`, `savepoint`, `release`, and `rollbackTo` of a write transaction to `Tx.Serving`, `Tx.Savepoint`, `Tx.Release`, and `Tx.RollbackTo` of the core, and `serving` of a read transaction to `ReadTx.Serving`. Each goes through `fromCore`. So a refusal of a savepoint, such as a second one while the first is open, is an internal error.

The interfaces exist for the tests:

- **The operators are tested alone, against a fake.** `fake` in `operator_internal_test.go` keeps tables in memory, records every call, and fails or panics on demand. Its four calls of a transaction that spans statements do nothing and record nothing. `spanning` in `tx_internal_test.go` wraps it, records them, and fails them on demand.
- **The fuzz tests run on a database in memory too.** `memory` in `memory_internal_test.go` is fast enough for many more texts and sequences than the core is. Its savepoint is a copy of its tables.

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
5. Add the kind of the statement to `report.of` in `telemetry.go`. If the statement sets SQLite's count of changes, set `SetsChanges` for it in `write` of `engine.go`, which runs a statement of its own, and in `writeIn` of `tx.go`, which runs a statement in a transaction.
6. Extend the generator in [`stmttest`](../sql/stmt/stmttest/stmttest.go), and `Sequence` in [`stmttest`](../sql/stmt/stmttest/sequence.go), which the differential tests of the engine and of the driver share: the mix of statements in `Next`, and the near misses in `Others`.
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
| `attempt to write a readonly database` | 8, `SQLITE_READONLY` | 8 | None. |
| `string or blob too big` | 18, `SQLITE_TOOBIG` | 18 | None. |
| A syntax error, such as `near "frm": syntax error` | 1 | 1 | From the pass. |

The code 8 is the constant `sqlerr.CodeReadOnly`. The engine gives this error for a statement that would write in a read-only transaction, and `TestConn_Begin` pins it against SQLite under `pragma query_only = 1`.

Examples, each pinned against SQLite in `TestEngine_errors`:

- With a table `t1`, `create table "T1" (x int primary key) strict, without rowid` gives `table "T1" already exists`.
- `select * from "T""2"` gives `no such table: T"2`.
- With a table `t1`, `insert into "T1" ("V""2") values (1)` gives `table T1 has no column named V"2`.
- After `create table "Foo bar" ("Bar""x" int primary key) strict, without rowid`, a second insert of the same key through `"FOO BAR" ("BAR""X")` gives `UNIQUE constraint failed: Foo bar.Bar"x`.

The order of the errors is SQLite's:

- **A table that does not exist comes before a column that does not exist.** `insert into t2 (v2) values (1)` without `t2` gives `no such table: t2`.
- **In a read-only transaction, an error of a name comes before `attempt to write a readonly database`.** There, `insert into t2 (v1) values (1)` without `t2` gives `no such table: t2`, and `create table t1 …` with `t1` gives `table t1 already exists`. An insert of a key that exists gives `attempt to write a readonly database` there, and not `UNIQUE constraint failed`, as in SQLite.
- **A syntax error never depends on the database.** The spec: "Text with a syntax error gets the error that SQLite gives for it on an empty database." So `stmt.Check` alone decides it, before the engine begins a transaction, and a text with a syntax error gives its syntax error also on a closed instance, without the lease, and with a context that has ended. SQLite differs on a database that is not empty: it starts to create a table as soon as it has read `CREATE TABLE`, the name, and the `(` or `AS` after it, and if a table of the name exists, it gives `table %s already exists` and never meets the syntax error. With a table `t1`, SQLite gives `table t1 already exists` for `create table t1 (v1 int primary key) strict without rowid`, and the engine gives `near "without": syntax error`, as SQLite does on an empty database. This is the one difference from SQLite that the tests accept: `stmttest.ExistsAfterStart`, which the differential tests of the engine and of the driver share, accepts it where `stmttest.StartedTable` gives the name of the table that the text starts, SQLite gives that the table exists with the name as written, and the engine gives what SQLite gives on an empty database. The other texts where SQLite gives an error that is not the syntax error are in [the known gaps of `stmt`](../sql/stmt/README.md#known-gaps).

The errors that are not SQLite's:

- **"Not supported"** is the `*stmt.NotSupportedError` of the pass, unchanged. It matches `stmt.ErrNotSupported`.
- **A value of another type than its column is "not supported";** see [Types](#types).
- **A text without a statement** gives `ErrNoStatement`.
- **The state of the transaction of a connection has four errors:** `ErrTxOpen`, `ErrNoTx`, `ErrRowsOpen`, and `ErrTxDone`. Each is returned as it is, but for the commit of a write transaction while a loop reads its rows, whose error wraps `ErrRowsOpen`. See [Transactions](#transactions).
- **A commit that is too large gives an error that matches `ErrTooLarge`,** and that wraps the error of the core, which matches `core.ErrTooLarge`.
- **Every other error of the core passes through unchanged,** so that `errors.Is` finds its sentinel, such as `core.ErrStopped`, `core.ErrOutcomeUnknown`, or `core.ErrReaderBusy`. So does an error of the context. Neither is ever given as a `*sqlerr.Error`.
- **A refusal of the core becomes an error that matches `ErrInternal`** and wraps nothing else; see [The storage interface](#the-storage-interface). It is a fault of s4ql, and never SQLite's error.

## Changes and the last insert rowid

The engine gives what is needed to report `sqlite3_changes` and `sqlite3_last_insert_rowid` as SQLite 3.54.1 does. The differential test compares both with SQLite after every statement.

- **An insert that succeeds sets the count of changes to 1.** Its result has `Changes` 1 and `SetsChanges` true.
- **CREATE TABLE, DROP TABLE, and SELECT leave the count as the statement before left it.** Their result has `SetsChanges` false.
- **An insert that fails with `UNIQUE constraint failed` sets the count to 0.** SQLite gives this error when it steps the statement. The engine gives `Result{SetsChanges: true}` with the error of an insert that fails after its checks: in its operator, in its annotation, or in its commit. For an insert in a write transaction, the release of its savepoint takes the place of the commit.
- **An insert that a read-only transaction refuses sets the count to 0,** as SQLite does under `pragma query_only = 1`. Its result is `Result{SetsChanges: true}`. `TestConn_Begin` measured it against SQLite. A create or a drop that is refused there leaves the count.
- **An error that SQLite gives when it prepares the statement leaves the count:** `no such table`, `has no column named`, `already exists`, and a syntax error. A text without a statement leaves it too.
- **The last insert rowid never changes.** Every table is WITHOUT ROWID, and SQLite does not record an insert into such a table. The value is 0 on a new connection, and stays 0.

The engine keeps no count between statements. A connection that reports the counts as SQLite does keeps one number, its count of changes, which starts at 0. After a statement whose result has `SetsChanges`, also one that failed, the count is `Result.Changes`; after any other statement, the count stays.

Its last insert rowid is always 0.

## Annotations

- **Each write that is committed carries one annotation: the text of its statement,** from its first token to its end. The end is that of its semicolon, if it has one.
- **The commit object of a transaction that spans statements holds one annotation for each of its statements that succeeded and that write,** in their order. `TestConn_Commit` reads them from the log.
- **Comments and semicolons before the statement, and the text after it, are left out.** A comment inside the statement stays. `  /* before */ create table t1 (v1 int primary key) strict, without rowid; -- after` is annotated as `create table t1 (v1 int primary key) strict, without rowid;`.
- **An annotation holds the arguments of the statement,** by position, in `Statement.parameters` of the commit object: one for an insert of a parameter, and none otherwise.
- **A statement that fails commits nothing,** and leaves no annotation. A query leaves none either.
- **Every write of slice 1 that succeeds changes something.** The core writes no commit object for a transaction that changed nothing, and would then store its annotation nowhere. No statement of its own meets that case: a create, an insert of a new key, and a drop each change the database.
- **A transaction that spans statements can meet that case.** A create and a drop of the same table succeed and change nothing in the end. The transaction then writes no commit object, and its annotations are stored nowhere.

## Parameters

Slice 2 adds the plain `?` as the value of an insert; [the README of `stmt`](../sql/stmt/README.md#insert) defines it, and `Statement.Parameters()` gives how many a statement has.

- **The text is checked first, then the arguments,** in `prepare`, before the statement needs a transaction: a syntax error, "not supported", or `ErrNoStatement` comes whatever the arguments are, and the errors of the arguments come on a stopped instance and with a context that has ended too.
- **Another number of arguments than parameters** fails with an error that matches `ErrArguments`, such as `s4ql: wrong number of arguments: 2 for a statement with 1 parameter`. SQLite binds NULL to a parameter without an argument; the engine refuses it.
- **An argument that no column holds** is "not supported", in an error that matches `stmt.ErrNotSupported`, before the statement needs a transaction: NULL (`s4ql: not supported: argument 1, which is NULL`), a value of no kind (`which has no value`), a real that is not a number or is infinite (`which is a real that is not a number`, `which is an infinite real`), and text that is not valid UTF-8 or holds the character NUL (`which is text that is not valid UTF-8`, `which is text that holds the character NUL`). The core refuses each of these with `core.ErrInvalid`, which the engine would report as an internal error, so none may reach it. An integer, every other real and text, and a blob, also an empty one, are values; whether one is of the type of its column is checked later, when the table is known: see [Types](#types).
- **The insert binds the argument through `*Parameter`** of the plan, which `eval` reads from the arguments. A parameter without its argument is an internal error, which the check of the number rules out.
- **No value of an argument is in an error or an event.**

## Types

Slice 4 adds the column types `integer`, `real`, `text`, and `blob` beside `int`, and real, string, and blob literals and arguments; [the roadmap](../../docs/roadmap.md#slice-4-the-other-types) has what it includes.

- **A column keeps its type as declared.** `NewPlan` maps `stmt.Type` to the format's `Type`, so INT and INTEGER stay apart, in the commit object and after a replay, and `Rows.Columns` gives the declared type as SQLite reports it: `INT`, `INTEGER`, `REAL`, `TEXT`, or `BLOB`.
- **A value must have the type of its column:** an integer for INT and INTEGER, a real for REAL, text for TEXT, and a blob for BLOB. Any other value is "not supported", also where SQLite would convert it (the integer 1 for a REAL column, `'1'` for an INT column) and where SQLite gives an error of its own (`'a'` for an INT column, text for a BLOB column). The message names the value and the column, with the names as the table stores them, and no value: `s4ql: not supported: argument 1, which is text, for the column t1.v1 of type INT`, and `s4ql: not supported: a string literal for the column t1.v1 of type INT`, or `an integer literal`, `a real literal`, `a blob literal`. Such a refusal leaves the count of changes, as every "not supported" does.
- **The order of the errors of an insert is SQLite's,** where SQLite gives an error: the errors of the text and of the arguments, then the names (`no such table`, `has no column named`), which SQLite gives when it prepares the statement; then `string or blob too big` for a text or a blob longer than the limit, which SQLite gives when it binds such an argument; then, in a read-only transaction, `attempt to write a readonly database`, which SQLite gives at its `OP_Transaction`, before it looks at the value; then "not supported" for a value of another type than its column, where SQLite has its `OP_TypeCheck`; then `UNIQUE constraint failed`; and last `string or blob too big` for a row whose record is longer than the limit while its value is not, which only SQLite's write of the row finds. `buildSink` makes the checks up to the length, the sink's `check` makes that of the type, and `run` the rest. `TestEngine_tooBig` and `TestConn_Begin` pin the order against SQLite.
- **The limit is the core's,** `core.Options.LengthLimit`, 1,000,000,000 bytes by default, which `DB.LengthLimit` gives. Tests lower it to 1000, and compare with SQLite under `sqlite3_limit` at the same value. The engine checks the length of every value, a literal too, after the names; the core gives `core.ErrTooBig` for the record of a row, which `fromCore` turns into SQLite's error, and the transaction goes on after it. A literal longer than the limit is not compared with SQLite: SQLite meets one only with a limit below the length of the text that holds it, which `SQLITE_LIMIT_SQL_LENGTH`, also 1,000,000,000, rules out, and under the lowered limit alone it gives `string or blob too big` for a string literal only when it steps the statement, and turns a blob literal into NULL.
- **A real that is -0.0 is stored as 0.0,** by the core, as SQLite stores it, and read back as 0.0. It is the key of 0.0, so either after the other is `UNIQUE constraint failed`. The annotation keeps the argument as given, -0.0.
- **Keys compare as SQLite compares them,** which the cache does: integers and reals by their number, text and blobs byte by byte. `select *` gives the rows in that order. The test double in `memory_internal_test.go` compares them so too.

## Events

The engine logs one event for each statement, named `statement`, when the statement ends. For a query, that is when its rows end. It logs one event for each transaction that spans statements, named `transaction`, when the transaction ends; see [The event of a transaction](#the-event-of-a-transaction). The names are in `telemetry.go`.

| Attribute | Value | Present |
| --- | --- | --- |
| `statement.kind` | `create_table`, `drop_table`, `insert`, or `select`. | When the text is a supported statement. |
| `statement.table` | The name of the table, as its value. | When the text is a supported statement. |
| `statement.sql` | The text that the call was given. | When the level of the event is Debug. |
| `transaction.kind` | The kind of the transaction that spans statements: `write` or `read_only`. | For a statement that the connection is given while such a transaction is open. |
| `statement.position` | The last commit object of the state that the statement saw: the position of its read transaction, or the parent of its write transaction. In a transaction that spans statements, it is the state that the transaction began from. | Once the statement has a transaction. |
| `commit.parent` | The same value, under the core's name. | For a statement in a write transaction, also a query in a write transaction that spans statements. |
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
| Debug | `not_supported` | It is not supported. So is a statement that reads on a connection whose rows of an earlier query are open, which the core refuses with `ErrReaderBusy`: it is what the caller asked for, and nothing that went wrong. An error that matches `ErrRowsOpen` has this outcome too. |
| Debug | `tx_state` | It failed with `ErrTxOpen`, `ErrNoTx`, or `ErrTxDone`: the state of the transaction of the connection does not allow it. |
| Debug | `too_large` | Its commit failed with an error that matches `ErrTooLarge`. It is the application's transaction that is too large, and nothing that went wrong. |
| Debug | `no_statement` | The text holds no statement. |
| Debug | `wrong_arguments` | It was given another number of arguments than it has parameters. |
| Debug | `refused` | The core refused it with `ErrNoLease` or `ErrStopped`. The core logs that itself. |
| Debug | `canceled` | Its context ended. |
| Warn | `core_error` | It failed with another error of the core, such as a commit of unknown outcome. |
| Error | `internal_error` | It failed with an internal error, or panicked in the engine. |

- **A statement that panics in the engine logs one event,** with the outcome `internal_error` and the error `s4ql: internal error: the statement panicked`, and the panic goes on to the caller. This holds for a panic while the statement is checked, planned, or built, while a write runs, and while a source gives the rows of a query. A panic in the body of a loop over `Rows.All` is the application's, and ends the rows as a break does: the event has the outcome `ok` and the rows given until then.
- **The event of a write statement joins the core's event of its commit** by `commit.parent`, which is the same in both, and by the context, which both are logged with.
- **The SQL text is only in an event at Debug,** because it can hold the application's data. An event at Warn or Error has no text.
- **No value of a row is in any event.**
- **Of the three errors of `tx_state`, only `ErrTxDone` reaches an event now:** the event of a query whose loop the end of its transaction cut. A begin logs no event, and a commit or a roll back without a transaction logs none.

### The event of a transaction

The event `transaction` has the style of the event of a statement. Its level and its `outcome` follow the table of levels above, for the error of the commit or the roll back. A commit that succeeds and a roll back that succeeds are at Debug.

| Attribute | Value | Present |
| --- | --- | --- |
| `transaction.kind` | `write` or `read_only`. | Always. |
| `transaction.end` | `commit` or `rollback`. A close of the connection is a roll back. | Always. |
| `transaction.statements` | The number of statements that the connection was given while the transaction was open, also those that failed. | Always. |
| `transaction.writes` | The number of statements that changed the database and succeeded, each of which the transaction annotated. A commit object holds them only if the transaction commits and changes something in the end. | Always. |
| `transaction.position` | The last commit object of the state that the transaction began from. | Always. |
| `commit.parent` | The same value, under the core's name. | For a write transaction. |
| `outcome` | How the commit or the roll back ended, in a word. | Always. |
| `error` | The text of the error. | When the commit or the roll back failed. |
| `duration.total` | The duration from the call of the begin to the end, on the monotonic clock of the host. The wait of `Begin` for the place of the writer is in it. | Always. |

- **The event of a write transaction joins the core's event of its commit** by `commit.parent`, as the event of a write statement does.
- **The event of a statement of the transaction has the position of the transaction:** its `statement.position` is the `transaction.position`.
- **The event has no SQL text and no value.**
- **The event is logged when the transaction has ended.** That is when `Commit` or `Rollback` returns, or, if a loop reads rows of the transaction then, when the loop ends and the transaction ends with it. The event then has the error that `Commit` gave, or else the error of that later end, and its duration goes to that end.

## How it is tested

SQLite is the judge. `oracle` in `differential_test.go` runs each text on a database in memory through the binding in [`internal/sqlite`](../sqlite/README.md), as `sqlite3_prepare_v2` and `sqlite3_step` run it.

The layers:

1. **The differential test.** `TestDifferential` runs random sequences of statements on SQLite and on the engine, which runs on the real core with a simulated store. After each statement it compares the answer: the error as code, extended code, and message, or the columns and the rows. It also compares the count of changes, and requires a last insert rowid of 0. A statement that fails must leave SQLite's count as it was, with three exceptions that `pair.step` accepts, each of which sets the count to 0: a broken constraint, an insert that a read-only transaction refuses, and an insert whose record is too long while its value is not. The second occurs only in `TestConn_differential`, which runs the same step in transactions, and the third only under the lowered length limit. The generator marks each statement that is of slice 1, and the engine must support every one of them. A text that the engine does not support is not run in SQLite, which would change its database; SQLite prepares it on an empty database, and must give no syntax error, since a text that is not supported is one that SQLite accepts. The one difference from SQLite that the test accepts is that of [a syntax error after the start of a table that exists](#errors). Each call has the arguments that the generator gives: one insert in three has a `?` with its value as the argument, an `int64`, a `float64`, a `string`, or a `[]byte`, and one call in four of a statement with a parameter, and one in twenty of one without, has the cases around it, as [`docs/todo.md`](../../docs/todo.md#parameters) lists them. Among these cases are an `int`, an `int32`, a `uint64`, and a `float32` in place of the argument, and the arguments that no column holds: nil, a `bool`, a `time.Time`, a `uint64` above the largest `int64`, NaN, an infinity, text that holds a NUL or is not valid UTF-8, and a nil `[]byte`. A call that the engine refuses for its arguments is not run in SQLite, which binds NULL to a missing argument and binds every type; the test checks it against s4ql's rule instead, with the exact message: the error of the number of arguments, or "not supported" for the first argument that no column holds. Half of the sequences, the third and the fourth of every four, run with the length limit of the cache and of SQLite (`sqlite3_limit`) lowered to 1,000 bytes, `stmttest.LengthLimit`. An argument that is too long, and a row whose record is too long, are compared with SQLite. A literal that is too long is not run in SQLite, and must give `string or blob too big` after the names (`pair.checkLength`). An insert that SQLite prepares, whose value, a literal or an argument, is of another type than its column, is not run in SQLite either, and is checked against the rule of [Types](#types), with the names and the type of the column from SQLite. `typeRule` gives that rule, and the simulation shares it. The rule decides nothing in a read-only transaction, where SQLite and the engine refuse the write first, nor for an argument that is too long, which SQLite refuses when it binds it. Every other call runs in SQLite with the same arguments bound, each by its type, so that an argument of another type than its column meets SQLite's errors of the names as the engine does. A run with the default flags, and without `-short`, the race detector, or the address sanitizer, pins how many texts it excuses from the comparison, by kind: 21 texts of a table that exists, none of the known gaps of `stmt`, 644 calls with the wrong number of arguments, 141 with an argument that no column holds, 363 with a value of another type than its column, and 5 literals longer than the length limit, of the 12,000 statements. Before the generator wrote types, the first four counts were 24, 632, 50, and 10. The sequence draws other random numbers since it writes types, which moved the first two. The arguments that no column holds are of more kinds, and the cases around an argument replace a text or a blob by one of them, which took the third to 141. Every table has a type, and one value in ten takes any type, which took the fourth to 363. The test also requires an insert through a parameter that succeeds, that meets a key that exists, and that the rule refuses. Every second sequence restarts the instance at random points, with an empty cache, which replays the log, or with its own cache. After each restart and at the end, the test compares every table and every row.

   A sequence comes from `stmttest.Sequence`. Its statements meet at one or two tables and columns, each in several spellings: some that SQLite takes for the same name, and some that it takes for another. Each name of a table has a type of its own, one of the five, which nine create tables in ten declare and nine values in ten have; the tenth takes any of the five types, so that a value is now and then of another type than its column. Three values in four come from a small set of their type, so keys repeat, and the sets hold keys that SQLite takes for one, 0.0 and -0.0. The fourth comes from `stmttest.ValueOf`, with weight on the edges of its type. One text or blob in 16 is about 1,000 bytes long, at the lowered length limit or within a few bytes of it. One statement in twelve is a text that is not supported, a syntax error, or no statement. Some queries run through `Exec`, and some other statements through `Query`.

   `pair.reach` counts what the inserts reach, under "reached: " in the tally: each argument by its kind, as `stmttest.ArgumentKind` names it; each literal by its kind, as `stmttest.LiteralKind` names it, which is integer, real, real -0.0, text, empty text, blob, or empty blob; each text or blob of more than 900 bytes; each insert that SQLite prepares, by the type of its column and its outcome; and -0.0 that meets a key that exists. The full run with the default flags requires a least number of each kind, in `reachedDifferential`: for each of the five types an insert that succeeds, one that meets a key that exists, and one of a value of another type; -0.0 that meets a key that exists, as a literal and as an argument; `string or blob too big` for TEXT and for BLOB; and every kind of literal and of argument. Each least number is about a third of what the run gave on 2026-11-19, so that the test fails when the generator stops reaching a case. The run also requires one literal longer than the length limit.
2. **Fuzzing.** `FuzzSQL` gives the engine any SQL text, on a database with a fixed set-up of tables, some with rows, with names in different cases and in quotes (`fuzzSetUp`), and gives SQLite the same text on the same tables. It compares them as the differential test does: the error as code, extended code, and message, or the columns and the rows; the count of changes; and the tables after the text. It runs on the database in memory, and `FuzzSQL_core` runs the same on the core, which is about 45 times slower. Their seeds are statements of the sequence of the differential test, the texts of `stmttest.Others`, and every 500th text of the corpus. Beside the difference of a table that exists, they excuse the known gaps of `stmt` as its tests do, and count them apart: a syntax error where an error of a grammar action of SQLite came first, and a syntax error where SQLite gives an error about the database first, such as `no such table: main.X` for a trigger.

   `FuzzDifferential` explores seeds of the generator of the differential test, on the database in memory: its input is a seed and a length, not SQL text, so the guidance of the fuzzer by coverage has little to work with, and its count of executions is not a measure of the SQL text that it covers. `FuzzDifferential_core` runs such a sequence on the core, with or without restarts.
3. **The simulation.** `TestSimulation` runs statements under the scheduler of [`internal/sim`](../sim/README.md) while the store fails, hangs, and loses requests. The default workload has two instances that compete for the lease, two callers on each, one crash, and one close. Each caller is a `client` with a connection of the engine of its own, which it replaces when its node has opened its database again. A caller whose statement is refused for want of the lease waits up to two minutes for its instance to serve, and stops if it does not, as a client behind a check of health would turn to another instance; without that, the callers of the instance that does not hold the lease made more than half of all statements, and each was refused. The crash takes an instance that runs a statement, if one does within ten seconds, so that it often cuts one. After a crash, a new instance starts on a copy of the cache or on an empty one. The hammer workload has one instance with four callers that hardly wait, so that two statements often run at once. Each statement has a timeout, so a commit can end with its outcome unknown. Each statement carries an ID in a comment, which its annotation keeps, and its event gives the state that it saw. The first three seeds run twice, and the traces must be the same.

   In both workloads, one turn of a caller in four is a transaction that spans statements (`run.transaction`), on the caller's connection: a write transaction, or one time in four a read-only one, of two to six statements of the same sequence, with a wait of up to half a second before each. Its begin, each statement, and its end each have a timeout of up to the workload's, so a begin can give up its wait for the place of the writer, and a commit can end with its outcome unknown. It ends with a commit, or one time in four with a roll back. A statement that is refused for want of the lease or by a stop ends it early with a roll back, so these callers do not leave a transaction open. The idle workload adds the callers that do: it is the default workload with one idler on each instance (`run.idler`), whose two write transactions each make their statements and then sit idle while the instance is cut off from the store, half of the time for 30 to 60 seconds, long enough for the other instance to take the lease. The first runs one more statement after up to a minute and ends. The second is cut off for two minutes always, sits idle for three, and stays open after the end of the workload; the write of the end runs in it (`run.endKept`), and `run.readsFree`, which closes each instance in turn once its connections have run their query, requires that an instance where it is still open serves within ten minutes, with a violation of its own, "is open and idle, does not serve". An instance that lost its lease meanwhile must have ended the transaction. The core logs the end of a transaction for the lease, and `run.verifyOverlaps` takes it for the end of the place of the writer of that transaction, because the core frees the place before the caller ends the transaction. The engine cannot plant the core's bug `keepIdleTx`, so the check was shown by hand on 2026-11-19: with `DB.endForLease` made to do nothing, 89 of the first 100 seeds of the idle workload failed, each by that violation. Over 1,000 seeds the idle workload ended 1,348 transactions for the lease, 1,317 of them kept open, with no violation, and `TestSimulation` pins a quarter of both counts.

   `run.verify` checks each run against SQLite and the log:
   - Every commit object of the log holds the statements of one call. A statement of its own is of a call that succeeded, that had an unknown outcome, or whose instance crashed while it ran. A transaction that spans statements is a write transaction whose caller called its commit, and whose commit succeeded, had an unknown outcome, or did not return because its instance crashed; nothing of a transaction that was rolled back, or whose commit failed, is in the log. Its commit object holds exactly its statements that succeeded and that write, in order, and its parent is the state that its statements saw, so no commit landed while it was open. No statement and no transaction is in the log twice. The parameters of each statement are exactly the arguments that the caller gave, by position, kind, and value.
   - Every write that was acknowledged is in the log: a statement of its own when it succeeded, and the writes of a transaction when its commit succeeded. A transaction whose net effect is nothing writes no commit object, and its writes must not be in the log: one whose writes are all of tables that SQLite does not have at its beginning nor at its end.
   - The statements of the log, run in SQLite in their order with their parameters, each succeed.
   - Every insert of the log has a value of the type of its column in SQLite, where it runs, as `typeRule` says. One that does not is the violation "stores a value of another type than its column". SQLite converts some such values without an error, so the replay of the log alone does not find them.
   - A statement of its own that failed with SQLite's error gives that error in SQLite on the state that it saw. An insert that the engine refused for the type of its value is refused so by `typeRule` there. A query that succeeded gives its rows there, each value compared by its type. That state is the state after a commit of the log, and it holds every write that was acknowledged before the statement started.
   - The statements of a transaction that spans statements all saw one state, which holds every write that was acknowledged before the begin (`run.verifyTxs`). SQLite runs the log up to that state, under `pragma query_only = 1` for a read-only transaction, and then each statement of the transaction that succeeded or failed with SQLite's error, in order, and each must give what the engine gave, a write that succeeded included: for a write transaction, the state of its beginning and its own writes so far; for a read-only one, the state of its beginning, whatever committed meanwhile. An insert of a write transaction that the engine refused for the type of its value is refused so by `typeRule` on SQLite's state at that statement. A read-only transaction must refuse no insert for its type, since it refuses the write, with `attempt to write a readonly database`, before it looks at the value. So no caller sees a write of a transaction that has not committed, of its own transaction excepted.
   - Two write transactions of one engine never overlap (`run.verifyOverlaps`): no write transaction's begin returns between the return of another's begin and the call of its end, and no statement of its own that writes both starts and commits in that time. The calls are ordered by a clock of events of the run, and an engine is one node's between two opens of its database.
   - A text that the engine does not support is one that SQLite prepares on an empty database without a syntax error, as in the differential test.
   - A new instance with an empty cache replays the log to the state that SQLite has after it.

   A commit of a transaction whose outcome is unknown may be in the log or not, as for a statement of its own. One query in four is not read to its end: its loop stops at the first row, it is closed unread, or its consumer panics. Its rows are not compared, and the rest of the run must still hold. The callers run the calls of the differential test's generator with their arguments, and a call that the engine refuses for its arguments must be refused by the rule, as in the differential test. A refusal of an insert for the type of its value is checked by `typeRule` against SQLite on the state that the statement saw: in `run.verify` for a statement of its own, and in `run.verifyTxs` for a statement of a transaction. An insert of such a value that succeeds is found in the log, by the check of the type of each value there. A statement of slice 1 must not be refused as not supported. A statement that saw no state, because it failed before it had a transaction, is a syntax error, and must give what SQLite gives for it on an empty database.

   `TestSimulation` counts what each workload reaches: the statements by how they ended (succeeded, SQLite's error, unknown outcome, cut by a crash, not supported or without a statement, and refused for want of the lease, by a stop, or by the timeout), the queries not read to their end, the inserts refused for the type of their value, the acknowledged writes, the commits, the commits of more than one statement, the statements in a transaction, the write and the read-only transactions, the transactions by how they ended (committed, of unknown outcome, not committed, rolled back, and not begun), the crashes followed by an empty cache and by a copy of the cache, the closes, and the faults that the store injects, by kind, from the lines of the trace that the store writes for each. In a run of at least 50 seeds, each kind must occur at least as often as `reached` says for each seed, from a fifth to a third of what the first 1,000 seeds give, so that the test fails when a kind stops happening. The least numbers of "refused for the type" are 0.5, 0.1, and 1 for each seed in the default, the idle, and the hammer workload, where the first 200 seeds gave 505, 112, and 980. In such a run, at most the share `noLease` of the statements may be refused for want of the lease: 0.15 of them in the default workload and 0.12 in the hammer workload, where the first 1,000 seeds refused 0.08 and 0.07 before the workloads had transactions, and refuse 0.04 and 0.03 with them. A caller that does not wait for its instance to serve took the share to 0.60 and 0.31, measured before the workloads had transactions. The seeds are the same in every run, so the counts are too.

   A run also fails in these cases: a statement gives an error that the workload does not expect, as an internal error is; an instance stops; a close of an instance does not end within five minutes; a close of a connection fails; no instance runs a write within ten minutes after the store has calmed; a connection of a caller of a live instance has a read transaction left open after the workload; or a live instance does not run a query on each connection of its callers within ten minutes after that.

   `run.readsFree` makes the last two checks. Every connection of the callers of a live instance must run a query of a table that does not exist, which gives SQLite's `no such table` once the query has its read transaction. A connection whose read transaction was left open fails the query with `core.ErrReaderBusy`, and that is a violation at once: "… has a connection of a caller with a read transaction left open after the end of the workload". An instance that has not run the queries within ten minutes is a violation too: "… does not run a query on each connection of its callers ten minutes after the end of the workload". Only the instance that holds the lease can read, so each instance whose connections have all run their query is closed, which hands the lease to the next.
4. **Planted bugs.** `TestSimulation_plantedBugs` plants each of ten bugs in the engine, and runs seeds until two have found it by its own violation. A seed finds a bug only with a violation that the bug causes: a commit that fails in SQLite for `skipUnique` and `checkOutside`, and for `skipUnique` also an insert of a write transaction that succeeds where SQLite fails, an acknowledged statement that is not in the log for `ackUnknown`, no write after the workload, or an instance that does not run a query on each connection of its callers then, for `keepOpen`, whose open write transaction also keeps its instance from releasing the lease, for `keepRead` a connection with a read transaction left open after the workload, or a later query of the same connection that fails with "the reader has a read transaction open", and for `annotateNext`, which annotates each argument plus one, a commit whose parameters are not the arguments that its caller gave. Of the bugs of transactions, `keepFailed` is found by a commit object that holds other statements than its transaction's that succeeded and write, or a commit that fails in SQLite; `readNewest` by a statement of a read-only transaction that gives in SQLite another answer on the state of the beginning of its transaction, a read-only transaction whose statements saw more than one state, or an end of the transaction that fails with "the transaction has ended", because the transaction keeps the read that the bug ended when its read of the newest state fails; and `rollbackKeeps` by a transaction in the log that its caller rolled back. `convertReal` is found by an insert of the log that stores a value of another type than its column. `keepRead` also leaves open the rows of a query in a transaction, so a later statement of the connection fails with "the rows of an earlier query of the connection are open". The first of these for each bug is its own, and two seeds must find the bug by it: `keepOpen` by no write after the workload, and `keepRead` by the read transaction left open after the workload. Each seed that finds a bug runs again without it, and must find nothing. The bugs are fields of `bugs` in `bugs.go`, which only the test files can set, through `Options.Plant` in `export_test.go`.

   Of the bugs of statements of their own, a write in a transaction that spans statements (`writeIn`) plants only `skipUnique` and `convertReal`. `convertReal` is in `insert.check` in `operator.go`, which a statement of its own and a write in a transaction both run. Only the check of the log finds it, with "stores a value of another type than its column": SQLite converts the real of the log as the bug did, so the replay of the log and every comparison with SQLite agree with the engine. `ackUnknown`, `checkOutside`, `keepOpen`, and `annotateNext` are in `write`, which runs a statement of its own. `keepRead` is in `Rows.Close`, so it also leaves open the rows of a query in a transaction. The three bugs of transactions are in `tx.go`: `keepFailed` in `writeIn`, `readNewest` in `Conn.ready`, which every statement of a transaction passes, and `rollbackKeeps` in `Conn.Rollback`.

   The bug that the brief of the checks of transactions asked for first, a rollback to the savepoint that is skipped after a failed statement so that its change commits, cannot be found with the statements of slice 1, because no statement of slice 1 fails after it has changed anything. An insert writes one row, and checks its table, its column, and its key before it writes; a create checks the name, and a drop the table, before they change anything; the context is checked before the write and not after it; and nothing yields to the scheduler between the write and the annotation, where a stop of the instance would come. A failed statement therefore has nothing to roll back, and a release in place of the rollback to the savepoint leaves the same transaction. A probe on 2026-11-19 planted it as `skipRollbackTo`, which released the savepoint of a failed statement instead of rolling back to it, and 100 seeds of the default workload found nothing. `keepFailed` is the nearest bug that the statements of slice 1 make visible: it skips the rollback to the savepoint and keeps the failed statement, annotated, in the transaction. The rollback to the savepoint will matter once a statement can fail after a write, as an insert of several rows can, and `TestConn_writeIn` in `tx_internal_test.go` shows the order of its calls against a fake now.

   | Bug | The wrong decision | Workload |
   | --- | --- | --- |
   | `skipUnique` | An insert writes its row without the check of its key, so the row replaces one. | Default |
   | `ackUnknown` | A statement whose commit has an unknown outcome succeeds. | Default |
   | `checkOutside` | An insert checks its table, its column, and its key in a read transaction before its write transaction. | Hammer, because the bug needs two inserts of one key on one instance at once. |
   | `keepOpen` | A write statement that fails leaves its write transaction open. | Default |
   | `keepRead` | `Rows.Close` of rows that no loop has read leaves the read transaction open. | Default |
   | `annotateNext` | A statement annotates each argument plus one, while its row gets the argument. | Default |
   | `keepFailed` | A statement that fails in a write transaction after its savepoint is annotated, and its savepoint released, as if it had succeeded. | Default |
   | `readNewest` | Each statement of a read-only transaction reads the newest state that is committed, and not the state of the beginning of the transaction. | Hammer, because the bug needs commits between two statements of one transaction. |
   | `rollbackKeeps` | The roll back of a write transaction with one write commits it. | Default |
   | `convertReal` | An insert of a real that is a whole number into an INT or INTEGER column stores the integer, as SQLite converts it. | Default |
5. **The operators alone.** `operator_internal_test.go` runs each operator, `buildQuery`, and `buildSink` against `fake`, and `engine_internal_test.go` the engine's handling of transactions and its events: the order of the calls, an early stop, a context that ends, a failure at each step, a panic, and each planted bug. `TestConn` there has the reader of a connection: none for a connection that only writes, one from its first read until its close, one for each connection of an engine, a reader that cannot be opened, a close while the reader is in use, and a statement on a closed connection.
6. **The pinned cases.** `TestEngine_errors` pins each error against SQLite. `TestNewPlan` pins the plans, `TestEngine_annotations` the annotations as the log holds them, and `TestEngine_events` the events.
7. **No transaction left open.** `TestEngine_noneOpen` runs statements of a sequence, each through `Exec` or through `Query`. It consumes the rows of `Query` in one of four ways: to the end, with a loop that stops at once, with a close and no loop, or with a loop that panics. After each statement, a write transaction must begin, and a query on the connection of the instance must get its read transaction (`readFree`): it is a query of a table that does not exist, which gives SQLite's `no such table` once it has the transaction, and another error if a read transaction was left open. A second case plants `keepOpen`, and shows that a begin then waits.
8. **The names of the core.** `TestEngine_namesOfTheCore` runs the four statements on every name of the generator's pools and on random names in double quotes, of every kind of character. Wherever the pass supports the name, the core must take it, and the answer must be SQLite's.
9. **Concurrency.** `TestEngine_concurrent` runs inserts and queries from eight goroutines at once, each on a connection of its own, for the race detector. `TestConn` in `engine_test.go` runs the queries of twelve connections at once on the real core, each on the state of its start, and shows that a second query of a connection is refused while the rows of its first are open, that a write on that connection runs, and that a connection does not close inside the loop over the rows of its query.
10. **Transactions.** `tx_test.go` runs transactions that span statements on the real core with a simulated store, and `tx_internal_test.go` against a fake.

    | Test | What it shows |
    | --- | --- |
    | `TestConn_Begin` | Pinned sequences of statements in one transaction, on the engine and in SQLite. SQLite runs a write transaction between `begin immediate` and `commit` or `rollback`, and a read-only one under `pragma query_only = 1`, after `begin`. The test compares the answer and the count of changes after each statement, and the tables after the end. It compares the tables again after a replay of the log into an empty cache. |
    | `TestConn_differential` | 60 random sequences of 60 statements of the generator of the differential test, from seed 9,000,000. The statements run in write transactions, in read-only ones, and on their own, and each transaction ends with a commit or a roll back, at random. `pair.step` compares each statement as in `TestDifferential`. The test compares the tables after the end of each transaction, and after a replay of the log. |
    | `TestConn_isolation` | Two or more connections. No other connection sees the writes of a transaction before its commit, and every read that starts after the commit sees them. A read-only transaction keeps the state of its beginning while others commit rows and tables. A read-only transaction and a write transaction run at once, and neither waits. A second write transaction begins only after the first has ended. A begin whose context ends while it waits gives the error of the context, and begins nothing. |
    | `TestConn_Commit` | The commit object of a transaction and its annotations, as the log holds them. No commit object for a transaction that changes nothing. A commit that is too large. |
    | `TestConn_rows` | The rules of [Rows in a transaction](#rows-in-a-transaction), for both kinds of transaction. Also `ErrTxOpen`, `ErrNoTx`, a begin on a closed connection, and the roll back at the close of a connection. |
    | `TestConn_stop` | The statements, the commit, and the roll back of a transaction that is open on a closed instance, and on an instance that has lost its lease. The lease is lost for real: the test cuts the store client of the instance off with `simstore.Client.CutOff`, and another instance takes the lease and commits. |
    | `TestConn_events` | The event of a transaction, the attribute `transaction.kind` of its statements, a commit that the instance refuses, a commit that is too large, and no value of an argument in any event. |
    | `TestConn_writeIn`, in `tx_internal_test.go` | The order of the calls `serving`, `savepoint`, `release`, and `rollbackTo` against a fake: for a write that succeeds, one that fails in its operator, one that fails in its annotation, and one that panics. A statement that is refused before its savepoint, or by it, which leaves the transaction open. A commit of unknown outcome, which ends the transaction and is logged at Warn. |

    A test of a wait does not depend on real time. It runs in a bubble of `testing/synctest`, whose clock moves only while every goroutine of the bubble waits. So a second writer that has not begun after `time.Sleep(time.Hour)` is waiting, and a clock that has not moved shows that no call waited.
11. **Mutation testing.** [`internal/sql/mutation`](../sql/mutation/README.md) mutates this package too, and runs the simulation as a layer of its own.

Under `-short`, the race detector, or the address sanitizer, `TestDifferential`, `TestSimulation`, `TestEngine_noneOpen`, `TestEngine_namesOfTheCore`, and `TestConn_differential` run a tenth of their sequences, seeds, statements, and names, and the simulation at least two seeds.

**The simulation runs no crashes under the address sanitizer.** A crashed database stays open until the test binary ends, because only the core's own tests can close the cache of a crashed instance. The sanitizer reports what is left open.

The flags of the test binary:

- `-seed` is the first seed of the sequences of statements. The default is 0.
- `-sequences` is the number of sequences of `TestDifferential`. The default is 300.
- `-length` is the number of statements in each sequence. The default is 40.
- `-replay` makes `TestSimulation` replay that one seed, and nothing else, and log its whole trace. `-workload` chooses the workload: `default`, the default, or `hammer`.
- `-bug` plants the bug of that name in the replay.

A seed that fails in `TestSimulation` or `TestSimulation_plantedBugs` prints the command that replays it, with `-race` or `-asan` if the run that failed had one: the simulation runs no crashes under the address sanitizer, so a replay without it would be another run.

The environment variables:

- `S4QL_ENGINE_SIM_SEEDS` sets the number of seeds of each workload of `TestSimulation`, 50 by default. For `TestSimulation_plantedBugs` it sets the limit, 100 by default, and makes the test run every seed up to it, to count how many find each bug.

Examples of larger runs:

```sh
go test ./internal/engine/ -run '^TestDifferential$' -sequences 10000 -length 40
go test ./internal/engine/ -run '^$' -fuzz '^FuzzDifferential$' -fuzztime 10m
go test ./internal/engine/ -run '^$' -fuzz '^FuzzDifferential_core$' -fuzztime 10m
S4QL_ENGINE_SIM_SEEDS=1000 go test ./internal/engine/ -run '^TestSimulation$'
S4QL_ENGINE_SIM_SEEDS=1000 go test ./internal/engine/ -run '^TestSimulation_plantedBugs$'
go test -run '^TestSimulation$' -v ./internal/engine/ -replay 7 -bug skipUnique
```

## Results

On the code as it is now, on 2026-11-18:

| Layer | Run | Result |
| --- | --- | --- |
| `TestDifferential` | 10,000 sequences of 40 statements, from seed 2,000,000, half with restarts | 400,000 statements, none that differ from SQLite but the 985 accepted. By kind and outcome: CREATE TABLE 24,322 ok and 49,354 `already exists`; INSERT 65,978 ok, 25,683 `UNIQUE constraint failed`, 31,236 `has no column named`, and 60,490 `no such table`; SELECT 61,392 ok and 30,265 `no such table`; DROP TABLE 12,266 ok and 5,867 `no such table`; and of the other texts 24,506 not supported, each prepared in SQLite on an empty database without a syntax error, 2,861 `syntax error`, 2,867 `incomplete input`, and 2,913 without a statement. Of the syntax errors, 985 are after the start of a table that exists, where SQLite gives `already exists`: the accepted difference |
| `TestDifferential` | 3,000 sequences, from seed 1,000,000, on 2026-11-19, with the typed tables and values of the generator | no statement that differs from SQLite but those that the test excuses |
| `FuzzSQL` | 864,815 executions in 10 min on 4 workers, while other work loaded the machine | nothing found; 153 new inputs of interest |
| `FuzzSQL_core` | 66,355 executions in 10 min on 4 workers, while other work loaded the machine | nothing found; 97 new inputs of interest. Its first runs found the two known gaps of `stmt`, which `knownGap` now excuses, and whose inputs are in `testdata/fuzz` |
| `FuzzDifferential_core` | 65,263 executions in 5 min on 4 workers | nothing found |
| `TestSimulation`, default workload | 1,000 seeds, on 2026-11-19, with transactions | 93,300 statements: 29,878 that succeeded, 45,155 with SQLite's error, 562 commits of unknown outcome, 826 cut by a crash, 12,214 not supported, without a statement, or refused for their arguments, and 4,665 refused or ended by their timeout (5.0%): 3,470 for want of the lease, 208 by a stop, and 987 by the timeout. 50,335 of the statements ran in 14,176 transactions that span statements: 10,624 write transactions and 3,552 read-only ones, of which 8,577 committed, 276 had a commit of unknown outcome, 351 a commit that failed, 3,357 rolled back, and 1,488 did not begin. 11,516 commits, 1,481 of them of more than one statement; 518 crashes followed by an empty cache, 482 by a copy of the cache, and 1,308 closes; no violation |
| `TestSimulation`, hammer workload | 1,000 seeds, on 2026-11-19, with transactions | 184,769 statements: 56,773 that succeeded, 85,444 with SQLite's error, 1,013 of unknown outcome, 23,987 not supported, without a statement, or refused for their arguments, and 17,552 refused or ended by their timeout (9.5%): 5,373 for want of the lease and 12,179 by the timeout. 94,781 of the statements ran in 30,012 transactions: 22,565 write transactions and 7,447 read-only ones, of which 16,571 committed, 470 had a commit of unknown outcome, 701 a commit that failed, 6,042 rolled back, and 6,228 did not begin, most of them because their begin waited for the place of the writer longer than its timeout. 19,386 commits, 2,493 of them of more than one statement; no violation |
| `TestSimulation`, default workload | 1,000 seeds, on 2026-11-19, with typed tables and values | 94,727 statements: 28,628 that succeeded, 45,232 with SQLite's error, and 15,009 not supported, without a statement, or refused for their arguments or their type; no violation |
| `TestSimulation`, hammer workload | 1,000 seeds, on 2026-11-19, with typed tables and values | 186,044 statements: 54,726 that succeeded, 84,718 with SQLite's error, and 28,891 not supported, without a statement, or refused for their arguments or their type; no violation |
| `TestSimulation`, idle workload | 1,000 seeds, on 2026-11-19, with typed tables and values | 66,355 statements; no violation |
| `TestEngine_namesOfTheCore` | 2,832 names | 11,328 statements compared with SQLite, none that differ, and no internal error |

`FuzzDifferential` was not run at length on this code. Its executions are seeds of the generator, not SQL text.

Transactions, on 2026-11-19, with the arguments of the generator: `TestConn_differential` runs 60 sequences of 60 statements, 3,600 statements, and none differs from SQLite but the 6 accepted; 179 calls with the wrong number of arguments and 20 with an argument that is not an integer are checked against s4ql's rule instead. By kind and outcome: CREATE TABLE 149 ok, 432 `already exists`, 59 `attempt to write a readonly database`, and 40 with the wrong number of arguments; INSERT 430 ok, 184 `UNIQUE constraint failed`, 212 `has no column named`, 477 `no such table`, 218 `attempt to write a readonly database`, 87 with the wrong number of arguments, and 20 with an argument that is not an integer; SELECT 541 ok, 254 `no such table`, and 45 with the wrong number of arguments; DROP TABLE 76 ok, 48 `no such table`, 21 `attempt to write a readonly database`, and 7 with the wrong number of arguments; and of the other texts 223 not supported, 30 `syntax error`, 24 `incomplete input`, and 23 without a statement. The counts are of statements in a write transaction, in a read-only one, and on their own together. Since 2026-11-19 the simulation runs transactions, and three planted bugs are of transactions, as above. No fuzz test covers transactions yet; see [the todo](../../docs/todo.md#transactions).

The planted bugs:

| Bug | Seeds that found it | Seeds that found anything without the bug |
| --- | --- | --- |
| `skipUnique` | 168 of 200 | 0 |
| `ackUnknown` | 45 of 200 | 0 |
| `checkOutside` | 67 of 200, in the hammer workload | 0 |
| `keepOpen` | 194 of 200 | 0 |
| `keepRead` | 111 of 200 | 0 |
| `annotateNext` | 148 of 200 | 0 |
| `keepFailed` | 187 of 200 | 0 |
| `readNewest` | 172 of 200, in the hammer workload, 141 of them by its own violation, "(read-only), which began at commit" | 0 |
| `rollbackKeeps` | 108 of 200 | 0 |
| `convertReal` | 14 of 200 | 0 |

These counts are of 2026-11-19, with transactions in the workloads and with the typed tables and values of the generator, each of which changed the seeds' runs. Each count is of the seeds that found the bug by any of its violations; the first two seeds by its own violation are what the default run requires.

Mutation testing, on 2026-11-18 after the review: of 1,025 mutants, 308 do not compile, 703 are killed, none times out, and 12 are equivalent. The two that survived are killed by tests added after the run, which was checked by hand for each: the event of a syntax error through `Query`, and of a panic of a source before its first row. None is unexplained. An earlier run of this round left 10 survivors, each a gap in the tests, an equivalent swap of the arguments of `slices.Equal`, or code that a simpler form removed. See [`internal/sql/mutation`](../sql/mutation/README.md#the-pass-after-the-parser-and-the-engine). The code of slice 2 was mutated on 2026-11-19: of 77 mutants of the engine, 12 do not compile and 65 are killed; see [the parameters of slice 2](../sql/mutation/README.md#the-parameters-of-slice-2). The code of transactions was mutated the same day: of 628 mutants of `tx.go`, of the storage interface of transactions, and of the parts of `engine.go` that transactions changed, 128 do not compile, 469 are killed, 8 time out, and 23 are equivalent, after new tests of the events of transactions, of a statement whose context has ended, of a release that fails, and of a begin on a closed connection killed 14 survivors of the first run; see [the transactions of slice 3](../sql/mutation/README.md#the-transactions-of-slice-3).

### Run times

Measured on an Apple M4 with 10 cores, at a load of about 12 from other work:

- `go test ./internal/engine` takes about 8 s, `-short` about 2.5 s, and `-race` about 7 s. At a load of about 28, on 2026-11-18 after the review, it took 15 to 30 s.
- Under the address sanitizer, in a Linux container in Docker on the same machine, the tests of the engine took 89 s, and each fuzz test of `make fuzz` runs for `FUZZTIME` on top. `make test-asan FUZZTIME=10s` passed in 9 min 10 s while a mutation run used 6 cores.
- `TestDifferential` with 10,000 sequences took 266 s at a load of about 28, and `TestSimulation` with 1,000 seeds of each workload 233 s.
- On 2026-11-19, at a load of about 12 to 15, `TestSimulation` with its default 50 seeds of each workload took 3.2 to 3.3 s before the workloads had transactions and 3.9 to 4.1 s after, and 3.5 to 3.8 s before the idle workload and 5.2 to 5.5 s after it, at a load of about 12; `TestSimulation_plantedBugs` took 1.0 s with six bugs and 2.4 s with nine; and `TestSimulation` with 1,000 seeds of each workload took 75 s.
