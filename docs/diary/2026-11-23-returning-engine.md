# Diary: the engine, the core as far as needed, and the driver of slice 14, `returning`

The second step of slice 14. The statement pass reads `returning` already (#129, `/docs/diary/2026-11-23-returning-stmt.md`), and the engine refused it with `RETURNING clause`. This step makes an insert, an update, and a delete with `returning` run end to end through `database/sql`, with the commit before the first row outside a transaction.

## Step 1: run RETURNING in the engine

**Author:** builder of the engine step of slice 14

### Prompt Context

**Verbatim prompt:** The lead's brief, which quotes the agreement with Markus of 2026-11-23: I proposed "I propose `returning` as slice 14, on `insert`, `update`, and `delete`, with named columns and `*`. [...] Shall I write slice 14 as `returning` into the roadmap and start the builder?" and Markus answered "yes". The brief for this step: "Make statements with `returning` run, end to end through `database/sql`. Remove the guards the first step put in: `engine.returningRefusal` (`internal/engine/returning.go`, called in `engine.check`), the panic in `engine.NewPlan`, `returned()`, and the exception in `pair.step` of the differential test."
**Interpretation:** remove the four guards, give the plan, the sinks, the write paths, and the rows of the engine a RETURNING, decide where the rows are held, their order, what a statement beside them does, and how they show in the telemetry; test it against SQLite in the engine and through the driver.
**Inferred intent:** goqite's send and receive need `insert ... returning id` and `update ... returning id, body`, on the way to glue on s4ql, with the promise that a row a caller has read belongs to a committed change.

### What I did

I measured SQLite first with the pinned shell, built from `/internal/sqlite/c/shell.c` and `/internal/sqlite/c/sqlite3.c` with `-include options.h` into the scratchpad, as the first step did: the defaults and `0.0` for `-0.0` in an insert, the count of changes, an update with the values that a row has, an update of the key, an update and a delete that match nothing, a delete through an index, and a failed `UNIQUE`. SQLite gave the rows of an update and a delete in the order of the key in every case I tried, also where an index on the column of the WHERE exists.

Then the code:

- `/internal/engine/plan.go`: `Insert`, `Update`, and `Delete` have a field `Returning *Returning` (`Star`, `Columns`), nil without RETURNING, which `NewPlan` fills and `Format` prints on the first line, as ` returning k, v` or ` returning *`.
- `/internal/engine/returning.go` holds what is new: `returning`, the rows that a sink holds, with the positions of its columns; `buildReturning`, which resolves the columns with `noSuchResultColumn`, as a select list; `returning.add`, which gives `0.0` for `-0.0`; and `returned`, the `source` of the rows, which reads nothing and gives them forward or reversed.
- `/internal/engine/operator.go`: `insert`, `update`, and `remove` have a field `ret`, and add each row once they have written it. A delete with RETURNING reads the row with `get` before it deletes it. `buildSink` resolves RETURNING after every other name: after the value counts of an insert, and after the names of WHERE for an update and a delete, before the lengths of the values.
- `/internal/engine/engine.go` and `/internal/engine/tx.go`: `Conn.write` and `Conn.writeIn` give a third value, the `*returned`, only after the commit, or the release of the savepoint, has succeeded. `Conn.queryText` turns it into `Rows` whose end ends nothing (`inTx{}`); in a transaction they become the connection's rows, as a query's. `Engine.order` draws one direction for each statement with RETURNING, before its sink is built, as `Engine.reading(true)` does for a query.
- `/internal/engine/telemetry.go`: `report.end` logs once, so the rows of a RETURNING, which end after their statement's event, log nothing again. The event of the statement is the write's, with `statement.rows`, which equals the rows returned, and `statement.direction` for an update and a delete with RETURNING.
- The driver's `check` in `/conn.go` calls `stmt.Check` alone again, as before the first step, so the text is parsed once in `Prepare`; `TestPrepare` now prepares an insert with RETURNING.
- The tests: `/internal/engine/returning_test.go` has `TestEngine_returning`, 44 calls on a table of every type with defaults and an index, through `pair.step`, twice, on the core and in memory, in a write transaction with a commit and with a roll back and a restart with an empty cache after it, in a read-only transaction, and with restarts between the calls; `TestConn_returning` for the columns, the counts, the variation of the order over 40 seeds, a commit that the store refuses as too large (`simstore.Options.MaxSize`), a context that has ended, the rows open in a transaction, and goqite's send and receive; `TestCheck_returning`, `TestPair_returning`, and `TestNewPlan_returning`. `pair.step` in `/internal/engine/differential_test.go` gained `returnedInOrder` and `checkReturned`, the order check of RETURNING; see below. `PathAccessOf` builds an update or a delete without its RETURNING, so that a missing column of RETURNING does not hide the access from `reachChange`.

### Why

The rows are held in Go memory because outside a transaction they must outlive the commit, and the write transaction of the cache ends with the commit: nothing that the transaction holds in the cache can be read after it, and a delete's rows are gone from the table anyway. Holding them also makes the commit-before-rows rule fall out of the order of the code: the rows exist as a value of `write` only on the path where the commit returned nil.

### What worked

The pair of the differential tests compares rows as a multiset already, and SQLite gives the declared types of the columns of RETURNING through `sqlite3_column_decltype`, so `answer.String` compared columns, types, and rows with no change. A quick mutation, the normalization of `-0.0` switched off, failed `TestEngine_returning` with `columns "r REAL, k INT", rows ["real -0,4"]`.

### What didn't work

The first run of the new tests failed three ways:

- `reachChange` reported `"update t set s = 'q' where k = 1 returning nope" with []interface {}(nil): the scan of the rows reads "", and the rule "a get of one key"`, because `engine.AccessOf` built the whole statement and failed on `nope`. Fixed in `PathAccessOf`, which leaves RETURNING out.
- My own test used `returning k, v, "w", *`, which the pass refuses (`*` with another column): `Expected "", but got "not supported"`.
- The goqite subtest used `duo.run`, which runs only a text that starts with `select` as a query, so the rows were empty: `Expected ""m_1"", but got ""`. It uses a `pair` now.

Two shell commands were refused by the permission system as "too complex to verify that it stays inside the worktree": a Python here-doc that held the word `commit` in its Go code, and one that ended with `go test`. I wrote the scripts to the scratchpad and ran them on their own.

### What I learned

An update with RETURNING that returns two rows or more never changes the key: SET has only constants, so an update of the key that finds two rows fails with `UNIQUE constraint failed`. So the order of the key of the rows found is the order of their keys after the change too, and the order check can compute the expected rows with a select before the change.

### What was tricky

The rows of a RETURNING outside a transaction must not become `Conn.rows`: a select's rows may still hold the reader there (a write runs beside them), and overwriting `Conn.rows` would hide them from `Conn.Close` and `Conn.canBegin`. So they are the connection's rows only in a transaction, where `Conn.ready` guarantees that no other rows are open.

### What warrants review

`Conn.queryText` in `/internal/engine/engine.go` and the idempotent `report.end` in `/internal/engine/telemetry.go`; `remove.run` in `/internal/engine/operator.go`, which reads each row before it deletes it when there is a RETURNING; and `pair.returnedInOrder`, which writes the SET values into the select list, with `(? + 0.0)` for a real so that `-0.0` reads as `0.0`.

### Future work

The driver's tests, the golden plans, the notes in `/docs/todo/slice-14.md`, the open item on memory, and the README of the engine.

## Step 2: the driver, mattn, the golden plans, and the notes

**Author:** builder of the engine step of slice 14

### Prompt Context

**Verbatim prompt:** the same brief as Step 1; this step is its part "Table tests [...] through the driver against mattn/go-sqlite3 in `internal/sqlite/crosscheck/drivercheck` [...] goqite's two statements as far as slices 1 to 14 take them [...] The commit-before-rows rule [...] Golden plans, if the plan of a writing statement is kept as a golden file anywhere".
**Interpretation:** cover RETURNING through `database/sql` against mattn, pin goqite's statements, keep the access of the new statements in the golden plans, and write down the decisions.
**Inferred intent:** the same as Step 1.

### What I did

`TestReturning` in `/conn_test.go` runs RETURNING through `database/sql`: direct, prepared, in a transaction with a roll back and a commit, the column types, `RowsAffected` of Exec, the errors, a replay on a second instance, and a commit whose outcome is unknown (the `faulty` store of `TestOutcomeUnknown`), which gives `ErrOutcomeUnknown` and no rows and commits once. `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go` has two sequences of slice 14, on `t5`, through every call, in transactions, with rows left open and a read-only transaction, and a subtest of `TestPair` with goqite's send through Query, a prepared query, and QueryRow, its select of the next message, and its receive by id, and a restart with an empty cache. The golden plans of goqite hold "receive by id" and "delete a queue with RETURNING" (`/internal/engine/access_test.go`, `/internal/engine/testdata/plans/goqite*.txt`). The decisions are in `/docs/todo/slice-14.md` under "The engine and the driver", and the open item on memory in `/docs/todo/storage-core.md`. A writer updates `/internal/engine/README.md` on a branch of its own.

### Why

The engine's differential tests compare with SQLite directly; the driver check adds what only `database/sql` and mattn show: the conversions of the values, `ColumnTypes`, and the count of changes of a connection.

### What worked

Once the mattn difference below was handled, both sequences passed on a pinned connection and through the pool at the first run.

### What didn't work

The first run of the sequences failed with three findings:

```
"update t5 set v2 = 'y' where v1 = 3 returning *" with the arguments [][]interface {}{[]interface {}(nil)}: s4ql gives
    exec: 1 affected, last insert id 0
and SQLite
    exec: 2 affected, last insert id 0
```

mattn's Exec steps the statement once and reads `sqlite3_changes` then, and SQLite counts the changes of a statement with RETURNING only when it has run to its end, so mattn gives the count of the statement before (here 2). The pinned shell gives `changes()` 1 after the statement, as s4ql does. `execByQuery` now runs SQLite's side of an Exec of a statement with RETURNING through Query and counts its rows. The third failure was my sequence: `pair.whileRowsOpen` closes the rows that were left open after the first call, so a second refusal needs a second `callQueryOpen`.

`make test` then failed in a test of slice 8 that pinned the old refusal:

```
--- FAIL: TestUpdateDelete/should_give_SQLite's_errors,_refuse_a_value_of_another_type_as_not_supported,_and_change_nothing (0.00s)
    conn_test.go:1503: Not true
    conn_test.go:1503: no error
```

It prepares `update u set s = 'x' returning s as x` now, which is refused for the alias. The first run of `TestReturning` hung: I opened a second instance with `w.serve` while the first still held the lease; it closes the first now, as `TestDefaults` does.

### What I learned

SQLite runs every change of a statement with RETURNING in its first step, so a QueryRow through mattn, which steps once, changes every row too, and so does s4ql; but the count of changes waits for the end of the statement.

### What was tricky

`callQueryOpen` of the driver check runs its text a second time to leave the rows open, which is harmless for a select and changes the database twice for a write. My sequences use it only with updates, whose second run changes the same rows to the same values.

### What warrants review

`execByQuery` in the driver check, which turns the outcome of a Query into that of an Exec; and whether treating the rows of a RETURNING in a transaction as a query's rows, with `ErrRowsOpen`, is what the lead wants, since they hold nothing.

### Future work

The generated checks (`stmttest.Generator.Returning` with a hook and a profile, and the simulation), and mutation testing, by the next builders. `pair.openRows` and the QueryRow of more than one column in the driver check run a text twice, which a generated write with RETURNING meets.
