# Diary: Transactions that span statements, in the engine

Slice 3 offers transactions through `database/sql`. This diary covers the engine's part: `internal/engine` and what it needs from `internal/core`. The driver is the next builder's. The work is on the branch `tx-engine`, which starts from `origin/main` at `429dd5c`.

## Step 1: Savepoints and a check of serving in the core

**Author:** transactions builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Slice 3 is transactions that span statements, offered to applications through `database/sql` (`db.BeginTx`, `tx.Commit`, `tx.Rollback`). The product owner decided the design: **A transaction takes the single writer's place when it begins**, like SQLite's `begin immediate`, and holds it until it commits or rolls back. [...] **A transaction begun as read-only is a read transaction**: it sees one consistent state, the one at its beginning, never writes, takes no writer's place, and blocks no one. [...] **Your task is the engine only**: `internal/engine` (and `internal/core` where the engine needs something from it). [...] 2. **A statement that fails inside a write transaction** behaves as in SQLite: the statement has no effect, the transaction stays open and usable, and what earlier statements did is kept. [...] 4. [...] Each statement in it obeys the spec's rule for reads when the lease is not valid or the instance has stopped. [...] 6. [...] Statements in an open transaction then fail with the errors that single statements get today; commit fails; roll back returns nil and frees the writer's place."
**Interpretation:** The core already has a write transaction that holds any number of calls and a read transaction fixed at one position. What it lacks for statements in a transaction is a way to undo one failed statement, and a check of the lease before each statement.
**Inferred intent:** Make a statement in a transaction behave exactly as in SQLite, also where a failure comes after a write, and keep the spec's rule for reads for every statement.

### What I did

`/internal/core/tx.go` got `Tx.Savepoint`, `Tx.Release`, and `Tx.RollbackTo`, one savepoint at a time, on SQLite's `savepoint s4ql_statement`. SQLite restores the cache, the temporary tables of touched keys included. The core also keeps in memory what the transaction did to each table (`txTable`: created, dropped, the name of the temporary table of touched keys), which its commit object is written from, so `Savepoint` copies `tx.order` and `RollbackTo` rebuilds `tx.tables` and `tx.order` from the copies, and forgets what the connection had looked up and prepared. The spool file of annotations is append-only, so a rollback to a savepoint before an annotation fails the transaction (it can then only be rolled back). `Commit` with a savepoint open rolls back and fails with `ErrInvalid`. `Tx.Serving` and `ReadTx.Serving` (`/internal/core/read.go`) check `servingLocked`, with the clocks, as a new transaction would; the other calls of an open transaction keep checking only `usable`.

Tests: `TestTx_Savepoint` in `/internal/core/tx_test.go` (undo of upserts, deletes, a create and a drop since the savepoint, with the commit object and a replay; a release keeps; the record of touched keys made before the savepoint stays, and one made after it is gone; misuse; an annotation since the savepoint; the clocks; a stop), and `TestReadTx_Serving` in `/internal/core/read_test.go`.

### Why

In slices 1 and 2 no SQLite error of a statement comes after its write, but other failures do: a lease that is lost between the upsert and the annotation, a context, a panic of an operator. Without a statement savepoint, such a statement would leave its row in a transaction that then commits. Every later slice with statements of several rows needs it anyway.

### What worked

A probe in a throwaway test (deleted) showed what SQLite does: `rollback to` restores tables created in `temp` and `main`; a `rollback` while a `select` of the same connection is between two steps succeeds and aborts the select (`abort due to ROLLBACK (code 516)` at its next step and at its reset); a new `begin immediate` on that connection works while the aborted statement is not yet finalized. A mutation that dropped the restore of `tx.order` made two subtests of `TestTx_Savepoint` fail.

### What didn't work

A shell heredoc (`cat > file <<'EOF'`) was refused by the sandbox of the worktree ("this command is too complex to verify that it stays inside the worktree"); files were written with the editor tools or a Python heredoc instead.

### What I learned

SQLite refuses `DROP TABLE` while any statement of the connection reads, so a commit of the core, which drops its temporary tables, cannot run while a scan of the same transaction is suspended. That decided how commit treats open rows (step 2).

### What was tricky

Whether the per-statement check should look at the clocks for a write transaction. The core says that an open write transaction is judged by its swap; the brief says statements in an open transaction fail as single statements do. `Serving` is a separate call, used only before each statement, so the core's other calls keep their rule.

### What warrants review

`Tx.RollbackTo` in `/internal/core/tx.go`: the copy of `txTable` values and the rebuild of the map. `tx.temps` is not restored, on purpose: a temporary table's number is only used once more after a rollback.

### Future work

None for the core in this slice.

## Step 2: Transactions in the engine

**Author:** transactions builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "On an `engine.Conn` (names are yours to choose; document them): Begin a write transaction: waits for the writer's place, until the context ends. Begin a read-only transaction: does not wait for it. `Exec` and `Query` on a Conn with an open transaction run inside it; without one, they behave exactly as today. Commit and roll back. [...] 5. **Open rows.** [...] Decide what commit and roll back do when rows of the transaction are still open, so that nothing is undefined, no rows can be read after the transaction ended, and a roll back always ends the transaction; write the choice down."
**Interpretation:** Add the four calls on `Conn`, route `Exec` and `Query` into the open transaction, and settle every corner of open rows.
**Inferred intent:** An engine the driver can map `BeginTx`, `Commit`, and `Rollback` onto without deciding anything about the semantics itself.

### What I did

`/internal/engine/tx.go` (new) holds `transaction`, `Conn.Begin`, `Conn.BeginReadOnly`, `Conn.Commit`, `Conn.Rollback`, `writeIn`, `queryIn`, and `commitError`. `Conn` got the fields `tx` and `rows` (the last rows of a query). `write` and `query` in `/internal/engine/engine.go` send a statement to `writeIn` or `queryIn` when a transaction is open. A write in a write transaction runs: `ready` (context, `serving`, rows open), `savepoint`, `buildSink`, `run`, `annotate`, `release`, with a deferred `rollbackTo` for every failure and a panic. A write in a read-only transaction builds its sink on `readOnly{t.read}` for SQLite's errors of the names, and then gives `attempt to write a readonly database` (8/8, `sqlerr.CodeReadOnly`). `Rows.tx` became an interface with only `end`, so that rows of a transaction end nothing (`inTx`), and rows whose transaction ended under a running loop end the transaction themselves. New errors: `ErrTooLarge` (wraps `core.ErrTooLarge`, also for statements of their own), `ErrRowsOpen`, `ErrTxOpen`, `ErrNoTx`, `ErrTxDone`. A new event `transaction` with `transaction.kind`, `.end`, `.statements`, `.writes`, `.position`, and `commit.parent`; the statement event gets `transaction.kind`.

The storage interfaces got `serving`, `savepoint`, `release`, `rollbackTo` on `writeTx` and `serving` on `readTx`; `fake` and `memory` in the internal test files got them, the fake's as no-ops that record nothing, so that no existing order of calls changed.

### Why

The open rows: a loop over `Rows.All` that is running or suspended holds a scan of SQLite on the transaction's connection, which only resuming the loop can finish. A commit cannot run then (the drop of temporary tables), and a rollback can, but the aborted statement would later be finished on the shared write connection while the next writer uses it, and `conn.rest` touches a Go map of that connection. So the end of the transaction is left to the rows: the loop ends at its next row with `ErrTxDone`, and the roll back or the end of the read transaction runs then, after the scan. The commit of a write transaction in that case rolls back and fails with `ErrRowsOpen`, because the brief says that a transaction is over when `Commit` returns. `database/sql` closes the rows of a `sql.Tx` before it calls the driver's `Commit`, so the driver does not meet it.

### What worked

The existing tests passed unchanged after the routing, under `-short`.

### What didn't work

My first Python edit of `engine.go` replaced the start of `Conn.Close` and then looked for the old text in the second replacement, which the first had changed: `AssertionError` from my own check, and nothing was written. One replacement for the whole function fixed it.

### What I learned

SQLite sets the count of changes to 0 after an insert that `pragma query_only` refuses, as after a broken constraint; a refused `create table` or `drop table` leaves it. The pinned test found it, and `pair.step` in `/internal/engine/differential_test.go` now accepts it.

### What was tricky

The order of the checks of a statement in a transaction: the text and the arguments first (as everywhere), then the context, then the lease by the clocks, then open rows. In a read-only transaction, SQLite gives `no such table` and the like when it prepares, before the refusal of the write when it steps, and the engine follows that.

### What warrants review

`Conn.leaveEnd` and the cut in `Rows.read` in `/internal/engine/engine.go`; `Conn.Close`, which now returns `ErrReaderInUse` from the rows' flag `reading` before it looks at the reader, and rolls back an open transaction; `pending`, which refuses every statement while rows hold the end of an ended transaction.

### Future work

The driver: see the section "Transactions" of `/docs/todo.md`.

## Step 3: Tests of transactions

**Author:** transactions builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Tests of the engine against SQLite through our binding [...] Tests of isolation with two or more Conns [...] Tests that read the commit objects [...] Tests for points 4, 5, 6, 8 and 10 [...] No test depends on timing: 'the second writer waits' is shown with the means the package already has for deterministic order [...] Run new tests with `-count=20`, also with `-race`."
**Interpretation:** New test files only, reusing the helpers of the differential test.
**Inferred intent:** The same trust as for slices 1 and 2: SQLite is the judge, and every decision of detail has a test.

### What I did

`/internal/engine/tx_test.go`: `TestConn_Begin` (pinned sequences, compared with SQLite under `begin immediate` or `pragma query_only = 1` and `begin`, with the count of changes after each statement, the state after the end and after a replay), `TestConn_differential` (60 random sequences of 60 statements, 6 under `-short`, in write, read-only, and no transactions), `TestConn_isolation`, `TestConn_Commit` (annotations, no commit object, too large with `simstore.Options.MaxSize` of 4 KiB), `TestConn_rows`, `TestConn_stop` (a closed instance, and a lease lost through `simstore.Client.CutOff` while a second instance takes the lease and commits), `TestConn_events`. `/internal/engine/tx_internal_test.go`: `TestConn_writeIn`, against a fake whose transactions record the calls of savepoints. `/internal/engine/tx_export_test.go` exports the names of the new event's attributes. The waits are shown in `testing/synctest` bubbles, as the core's tests show them: a second `Begin` that has not returned after `time.Sleep(time.Hour)` is waiting, and a clock that has not moved shows that nothing waited.

Commands: `go test ./internal/engine -run 'TestConn_' -count=20` (12.8 s) and the same with `-race` (9.1 s) passed; `go test ./internal/engine` passed in 14.5 s; `make lint` and `make check-browser` passed.

### Why

The lease-lost case needs a real second instance, because the spec's rule is about the manifest swap; the fakes of the core's tests are not reachable from the engine's package, and `CutOff` of the simulated store gives the same effect.

### What worked

The random differential test passed at once on the first run after the fix of the count of changes: 60 sequences, among them 485 inserts that succeeded, 160 refused as read-only, and 9 texts excused as the known difference of a table that exists.

### What didn't work

Two expectations of mine were wrong, not the code: `Rows.Close` after a loop that ended with `ErrTxDone` returns nil (it gives the error of the end of the transaction, as before), and the fake records `table t1` before each lookup. `make lint` found three unchecked receives and loops in the tests (`errcheck`).

### What I learned

Nothing beyond the steps above.

### What was tricky

`newInstance` works inside a synctest bubble, with its lease of an hour; the lease test needed the default lease of 30 seconds, so it opens its instances with `core.Open` itself.

### What warrants review

That `TestConn_stop`'s lost lease does not depend on how soon the first instance learns that it lost the lease: its statements fail by the clocks after 30 seconds either way, and its commit is sent after the cut-off has ended, so it is refused and not of unknown outcome.

### Future work

The generators of the differential tests (`stmttest`, `drivercheck`) and the simulation do not make transactions yet.

## Step 4: Documentation and self-review

**Author:** transactions builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "`internal/engine/README.md` describes transactions. `docs/todo.md` gets a section \"Transactions\" with every decision of detail you made, and with what is open for the driver."
**Interpretation:** Hand the long-form documentation to a writer with the decisions, review it against the code, and run a review of the diff.
**Inferred intent:** A next builder of the driver reads the README and the todo, not this branch's history.

### What I did

A writer sub-agent wrote the section "Transactions" of `/internal/engine/README.md`, the event of a transaction, the layer of tests, and the section "Transactions" of `/docs/todo.md`, from a list of facts. It checked them against the code and found seven places where the code was narrower than my facts; the ones that mattered: `duration.total` of the event started after the wait of `Begin` (I moved the start before the wait, as the brief's "duration" suggests), the event of a transaction whose end is left to its rows was logged before that end, and `keepRead` also acts on the rows of a transaction (documented, left as it is). I added two bullets to `/internal/core/README.md` for the savepoints and `Serving`.

The review of the diff (`/code-review high tx-engine`) found no bug that blocks, and six nits. Fixed: the event of a transaction whose end waits for a loop is now logged by that end (the type `ending` in `/internal/engine/tx.go`), with the commit's error or the end's, and `TestConn_events` shows it; the doc of `Conn.Rollback` no longer promises nil after a stop by a local failure, where the roll back in the cache can fail; the docs of `Tx.Release` and `Tx.RollbackTo` say that they fail with the stop only once `DB.Close` has closed the connection; `pair.step` decides that a text is an insert through `stmt.Check` and not by the word in the text; the doc of `transaction.writes` says what it counts; and `/docs/todo.md` says that a write is refused while rows of a transaction are open where SQLite runs it, also in the older item "To look at again with transactions".

`make test` passed (exit 0), `make lint` passed, and `go test ./internal/engine -run 'TestConn_' -count=20` passed with and without `-race` after the fixes.

### Why

The review's points were each true; none changed a result of SQL.

### What worked

Briefing the writer with a numbered list of facts, and asking it to check each against the code, found real mismatches.

### What didn't work

My first version of the new event test took the last event for the transaction's; the end of the rows logs the transaction's event first and the query's after it (`Rows.end` ends its transaction before it logs the query's event): `Expected "transaction", but got "statement"`.

### What I learned

Nothing new beyond the above.

### What was tricky

Keeping the docs and the code in step while two sub-agents worked over the same branch; the writer did not commit, and I reviewed its diff before committing.

### What warrants review

The writer's prose in `/internal/engine/README.md`, section "Transactions", against `/internal/engine/tx.go`.

### Future work

The driver, and the items under "Not built" in the section "Transactions" of `/docs/todo.md`.
