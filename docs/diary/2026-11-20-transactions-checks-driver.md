# Diary: Generated checks of transactions through `database/sql`

Slice 3 was merged as `0818823` with hand-written tests of the driver's transactions (`/tx.go`, `/tx_test.go`). The differential test of the driver against `mattn/go-sqlite3`, `/internal/sqlite/crosscheck/drivercheck`, only began a transaction and rolled it back at once. This task makes it run generated transactions, looks for a suspected race between `db.Close()` and a statement of a transaction whose rows are open, and judges two small faults reachable through `sql.Conn.Raw`. The work is on the branch `tx-checks-driver` from `origin/main` at `0818823`.

## Step 1: Generated transactions in `drivercheck`

**Author:** tx-checks-driver builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "`TestGenerated`, `TestSequences`, and `FuzzSequence` run sequences in which a generated run of calls happens inside a transaction, on both sides, and compare every outcome, the result of the commit or the rollback, and the tables after. [...] The oracle must begin its transactions as s4ql does (a writer from the beginning), so that both sides mean the same. Where the two legitimately differ by the roadmap [...] the case is checked against s4ql's rule and counted with a pin, never silently excused. Required outcome kinds in `TestGenerated` grow to cover each bullet above. Nothing that s4ql runs may escape the comparison."
**Interpretation:** Make a transaction a state of the pair of databases, with calls that begin, end, and run in it, and drive them from the generator, the hand-written sequences, and the fuzz target.
**Inferred intent:** The driver's transactions are compared with SQLite at random, as its statements and arguments already are.

### What I did

First a probe (a throwaway test in the package, deleted): mattn's `BeginTx` accepts `ReadOnly: true` and then lets an insert through, and accepts every isolation level, `IsolationLevel(99)` too. `_txlock=immediate` in the DSN makes its begin `begin immediate`: a second connection's insert gets `database is locked`. Under `pragma query_only = 1`, a create, an insert, and a drop get `attempt to write a readonly database`. `tx.StmtContext` works with a statement prepared on a `sql.Conn`. `Conn.ExecContext` on a `sql.Conn` whose transaction is open runs in it, and a second `Conn.BeginTx` gets `cannot start a transaction within a transaction`.

`/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`:

- A `runner` interface (what `sql.DB`, `sql.Conn`, and `sql.Tx` share) beside `queryer`, and `inTx`, a `sql.Tx` with the queryer it began on.
- New calls, appended to `calls` so that the old bytes of the fuzz target keep their meaning: `begin read-only`, `commit`, `rollback`, `prepare outside, exec in the transaction` and `... query ...` (Prepare on the `DB` or `Conn`, run through `tx.StmtContext`), `query, rows left open`, and `query beside the transaction`. `begin` now keeps its transaction open; its text names the isolation level.
- `pair.tx` holds the transaction of both sides. `pair.begin` begins on both (the oracle is opened with `?_txlock=immediate`, and a read-only one runs `pragma query_only = 1`); a level that s4ql refuses is checked against its message and not begun in SQLite; on a pinned connection whose transaction is open, `pair.beginAgain` requires s4ql's `s4ql: a transaction is open on the connection` (no sentinel, no `*s4ql.Error`) and SQLite's own refusal; through the pool, a begin with a transaction open commits it first, because s4ql's second write transaction would wait for the first. `pair.end` resets `query_only`, commits or rolls back both, compares the outcomes, and compares every table.
- `pair.where` sends a call into the transaction, except `query beside`: on a pinned `Conn` it runs on the `Conn` (inside the transaction), through the pool on the `DB` outside it, and only for a select, because a write beside a write transaction would wait.
- `pair.openRows` runs a query again on both sides after its comparison, reads the first row of each, and leaves the rows open. `pair.whileRowsOpen` then requires s4ql's refusal (`rowsOpen`) for a call that s4ql would run (`stmttest.Call.Runs` in every run), not at prepare; a text or argument error is compared as without open rows. An end with rows open is compared too (database/sql closes them first).
- `compare` takes the oracle's runner. Through the pool, mattn's Prepare outside the transaction can fail on a name that only the transaction has; then mattn's statement is prepared in the transaction, which is where database/sql prepares it again, and the case is pinned (`excusedOutside`).
- `tally.add` counts each outcome once plain and once under the scope (`write transaction: ...`, `read-only transaction: ...`, `beside a write transaction: ...`); `pair.end` counts each end with where it ran and what the transaction did. `TestGenerated` requires 60 more kinds.
- `pick` begins a transaction one time in ten outside one, ends it one time in seven inside (a roll back one time in three), and in a transaction picks among the new calls; `pickLevel` picks serializable or another level one time in eight each.
- Six hand-written sequences: a write transaction that commits (arguments, prepared statements in and outside, a failed statement in the middle), one that rolls back (create and drop inside), a read-only transaction, rows left open, isolation levels and a second begin, and a query beside.
- `TestPair` checks that the oracle's write transaction is the writer (`database is locked` with `busy_timeout = 0`); it fails with `_txlock=deferred`.

Pinned now: 1,051 errors at prepare (was 1,120), 12 tables started (13), 364 wrong numbers of arguments (412), 109 arguments not supported (103), and the new 67 isolation levels, 44 statements while rows are open, 28 second begins, and 43 statements prepared outside. 200 sequences make 499 transactions: 240 write commits, 102 write roll backs, 119 read-only commits, 38 read-only roll backs.

Commands: `go test -count=1 ./drivercheck` in `/internal/sqlite/crosscheck`; `go test -count=20 -run 'TestGenerated|TestSequences|TestPair|FuzzSequence' ./drivercheck` (66 s); the same with `-race -count=5`; `go test -run XXX -fuzz FuzzSequence -fuzztime 30s ./drivercheck` (5,482 inputs); `golangci-lint run ./...` (0 issues). Two mutants of `/tx.go` by hand, reverted: `Rollback` calling `c.e.Commit` failed 7 tests, and `BeginTx` ignoring `ReadOnly` failed 4.

### Why

Treating the transaction as state of the pair, and not as one call, lets the same calls run in and out of transactions, and lets the generator, the hand-written sequences, and the fuzz target share one path. Every call that s4ql runs still goes through `compare` or through a check against a rule with a pinned count.

### What worked

The comparison found no difference between s4ql and SQLite in any of the 200 sequences or the hand-written ones. The hand-written outcomes I wrote down from SQLite's rules (for example that a create table after a failed insert reports 0 changes) held on the first run; a deliberately wrong expectation failed, so they are checked.

### What didn't work

- The first run of `TestGenerated` failed in `pair.beginAgain`: `BeginTx with &{Isolation:Default ReadOnly:false} in a transaction: SQLite gives attempt to write a readonly database`. Under `pragma query_only`, SQLite refuses `begin immediate` as a write before it says that a transaction is open; the expected message now depends on the kind of the open transaction.
- Then `whileRowsOpen` failed: s4ql gave `UNIQUE constraint failed: detACH.of` where the rule says the rows are open. The open query (`select * from RELEASE`) had no rows, and `sql.Rows.Next` returning false closes the rows, so nothing was open. `openRows` now closes and forgets rows whose first `Next` finds none.
- `run` closed the statement of `tx.StmtContext` and failed with `Expected nil error, but got "no such table: t1"`: its `Close` returns the error of the prepare in the transaction that failed (the table was dropped there). The end of the transaction closes it, so the test no longer does.
- All `FuzzSequence` seeds after the first failed with `table t1 already exists` in SQLite: the oracle's DSN was `file:<TempDir>/oracle.db?...`, a URI, and the TempDir of `FuzzSequence/seed#N` holds a `#`, which ends a URI's path, so every seed opened the same file. The DSN is now a plain path with `?_txlock=immediate`, which mattn also parses; a probe showed the lock is still taken and the `#` kept.
- The Bash sandbox refused heredocs and a `cd` to `go list -m -f '{{.Dir}}'`; the probes were written with the Write tool instead, and I did not read mattn's source outside the worktree.

### What I learned

The fuzz target's progress shows bursts with seconds of `0/sec` in between; the same happens with the file from `main`, at a load average of about 15 on the machine, so it is not this change. One run of the seeds took 24 s with four seeds at 3 s and 7 s, and five later runs took 0.25 s each.

### What was tricky

Avoiding deadlocks through the pool: a write outside a write transaction waits for the writer's place in s4ql and for SQLite's lock in mattn, so `query beside` runs outside only for a select, a begin with a transaction open commits it first, and the tables are compared only after a transaction ends.

### What warrants review

- `excusedOutside` in `compare`: through the pool, mattn's outer Prepare sees the committed state, and the comparison falls back to mattn's statement prepared in the transaction. That is what database/sql does after the outer Prepare, but it is an excuse; it is pinned at 43.
- `pair.where` for `query beside` through the pool: a non-select runs in the transaction.
- The rate of the new calls in `pick`, and that the counts of `while rows are open` stay small (44 in all).

### Future work

None from this step.

## Step 2: The race between `db.Close()` and a statement on a connection whose rows are open

**Author:** tx-checks-driver builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "The builder of the driver suspected, without a test: if `db.Close()` (or the stop of the instance) closes the open rows of a transaction while the application is running a statement on that same transaction, two goroutines touch the engine's state of the rows. Write a test in the root package that makes exactly that overlap many times, and run it under `-race`. Also the same for rows outside a transaction, if no existing test already makes that overlap (see the tests of `db.Close()` under load). If it is a real race, fix it and say what the fix costs; if it is not, say why not, in the test's comment."
**Interpretation:** Reproduce the overlap with the race detector, inside and outside a transaction, and fix the driver if it races.
**Inferred intent:** The close of a database under load is free of data races, as the rest of the driver is.

### What I did

Only `connector.Close` closes rows from outside database/sql (`rows.shut`); the stop of the instance closes none. No existing test ran a statement on the same connection as the rows that the close closes: `TestClose` "should refuse a query that comes while the close closes the rows" queries on another connection, and the many-goroutine test of `TestTx_instance` keeps no rows open beside a statement.

`closeBeside` in `/conn_test.go` opens a new world 20 times, opens rows on a connection (read to their first row every second time, so that a loop of the engine runs), starts a goroutine that runs statements on that connection until the close answers, and closes the database. Two tests use it: `TestTx_instance` "should close the rows of a transaction while the database is closed, beside statements of the transaction" (write and read-only, `tx.ExecContext` of an insert), and `TestClose` "should close the rows of a connection while the database is closed, beside statements on the connection" (a `sql.Conn`, an insert and a query by turns). Both reported `WARNING: DATA RACE` under `go test -short -race -run ...`: `engine.(*Rows).end()` (engine.go:591) wrote what `engine.(*Conn).rowsOpen()` (tx.go:149) read, from `connector.Close` or the loop's task on one side and `conn.ExecContext` on the other.

The fix is in the driver (`/conn.go`, `/rows.go`, `/tx.go`); the engine is unchanged. A call of the connection that reaches the engine (`ExecContext`, `QueryContext`, `BeginTx`, `Commit`, `Rollback`, `Close`) holds the engine's connection from `conn.enter` to `conn.leave`. `rows.shut` holds it while it closes rows. If a call holds it, `shut` appends the rows to `conn.shut` and returns, and the call closes them in `leave` before it frees the connection. A call that comes while `shut` closes rows waits on a host semaphore (`conn.turn`), which `shut` hands over. simlint forbids `sync.Cond`, hence the semaphore.

### Why

database/sql makes one call of a connection and its rows at a time, under its lock of the connection, but `connector.Close` runs outside that lock. The engine's `Conn` and `Rows` say they are not safe for concurrent use, so the exclusion belongs in the driver. The close must not wait for a call: a write outside a transaction waits for the place of the writer, and it wakes only at the stop of the instance, which `connector.Close` makes after it closes the rows. A plain mutex around both would make `db.Close()` wait for such a write, perhaps for good.

### What worked

With the fix, both tests pass under `-race`, and a probe (`println` in both branches of `shut`, removed) showed both reached: of 180 closes, 167 and 174 (two runs) found a call running and left the rows to it, and 13 and 6 closed them at once. `go test -short -race -count=20 .` passed (94 s), as did `golangci-lint run .` and simlint on the root package.

### What didn't work

My first version of the helper replaced the doc comment of `TestTx_raw` by mistake; moved the helper to `/conn_test.go` and restored the comment.

### What I learned

The engine's loop over rows runs in a task of the host, so the close of rows by the connector runs engine code on that task while the connector waits for it, not on the connector's goroutine.

### What was tricky

Choosing between "the close waits for the call" and "the call closes the rows": only the second keeps `db.Close()` from depending on calls that wait for the stop that comes after it.

### What warrants review

- `conn.enter`, `conn.leave`, `conn.freeLocked`, and `rows.shut`: the hand-over, and that no path takes `rows.mu` while it waits for the connection (`shut` never waits for it, and `leave` takes `rows.mu` only while it holds it).
- The cost: a mutex taken twice for each call, which database/sql makes one at a time, so it is not contended, and a wait only for a call that comes while the close of the connector closes rows of its connection. The branch where a call waits (`waiting`) is not reached by a test in a way I can show; it is the hand-over of a free semaphore.

### Future work

None.

## Step 3: `IsValid` and the close of a connection while a loop reads, through `sql.Conn.Raw`

**Author:** tx-checks-driver builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Two smaller things from the review, both reachable only through `sql.Conn.Raw`, recorded in `docs/todo.md`: after a commit or a rollback while a loop reads rows, `IsValid` is true while the engine still holds the writer's place; and the engine's `Conn.Close` gives `ErrReaderInUse` and rolls nothing back while a loop reads. Judge each: fix it if a fix is small and sure, otherwise leave the entry in todo with your reasoning."
**Interpretation:** Decide whether the two are worth fixing in the driver, and fix them if the fix is small.
**Inferred intent:** A connection never goes back to the pool in a state that refuses the next user, and nothing is left held by no one.

### What I did

A test first, `TestTx_raw` "should keep a connection out of the pool while rows of its that a loop reads are open, and close it when they end, after the end of their transaction", for a commit, a roll back, and no end. It failed: `IsValid` was true after the commit and the roll back (tx_test.go:994 `Not true`), and with no end the transaction was never rolled back, so the next begin waited until its bound: `Expected nil error, but got "s4ql: context deadline exceeded"`.

The two are one fix, because fixing the first alone makes database/sql close such connections, and the second then leaves their reader and transaction to no one. The connection counts its rows that are not closed (`conn.held`). `IsValid` is false while a transaction is open or `held` is above 0. `conn.Close` keeps the engine's error, but when it is `core.ErrReaderInUse` while rows are held it sets `closeLater` and returns nil; `rows.close` then closes the engine's connection when the last rows close, which rolls back a transaction that is still open and closes the reader. The test passes, and the whole root package passes with `-short` and with `-race -count=20`.

`/docs/todo.md`: the entry "Known, through the driver's own interfaces only" is now the decision of what the driver does; a new entry records the race fix of step 2; the entry of `drivercheck`'s `begin` describes the transactions of step 1; and "Generated comparisons of transactions through `database/sql`" is deleted from "Not built". `/docs/roadmap.md`: only the sentence about the generated comparison through `database/sql` in the status of slice 3, with the numbers of step 1.

### Why

Both are small, and the engine's documented rule (a close fails while a loop reads, and must come after the rows end) says what the driver must do: close later. database/sql never reaches either path, so normal use is unchanged; `IsValid` after a normal end sees `held` at 0, because database/sql closes the rows before it gives the connection back.

### What worked

The red test showed both faults at once, and the fix made it green without changing any other test.

### What didn't work

My first version of the test expected the rows to end at their next row in every case, and failed for no end: `tx_test.go:1006: Not true`. With the transaction still open, the loop reads its second row; the test now reads to the end and counts the rows. A check that a begin with a cancelled context fails proved nothing, because database/sql checks the context before it asks the driver, and is gone.

### What I learned

`sql.Conn.Close` calls `putConn`, which calls `IsValid`, so a connection taken with `db.Conn` reaches the same check as one of the pool.

### What was tricky

That fixing only `IsValid` would have made things worse: before, a connection given back with open rows stayed usable once the application closed them; with `IsValid` false and no later close, its reader would have stayed open until the instance closed.

### What warrants review

- `rows.close` closes the engine's connection when `closeLater` is set and the last rows close, and drops the error: database/sql has let go of the connection.
- That `held` is decremented exactly once per rows (`rows.close` returns early once `closed` is set).

### Future work

None.
