# Diary: Randomized checks of transactions in the core and the engine

Slice 3 was merged as `0818823` with hand-written tests of transactions and a differential test against SQLite. This task adds a property test of the core's savepoints, transactions in the engine's simulation, planted bugs for transactions, and mutation testing of the code of transactions, on the branch `tx-checks-engine` from `origin/main` at `0818823`.

## Step 1: A property test of savepoints in the core

**Author:** tx-checks-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "**A property test of savepoints in the core** (`internal/core`). An independent review wrote one as a scratch file and deleted it; it should exist for good. Random transactions of random steps: `CreateTable` and `DropTable` on a few names, also in mixed case; `Upsert` and `Delete` on a few keys; `Savepoint`, `Release`, and `RollbackTo` in random places, with a savepoint still open at the end rolled back; then commit or roll back. After each run, the cache of the instance that committed must equal a replay of the log into an empty cache, and equal a simple model in the test that applies the same steps. It must reach these cases, and say how often it did: drop then create of one name with a rollback to a savepoint between; the first touch of a table's key inside a savepoint that is rolled back; a drop inside a savepoint of a table that existed before it; a rollback to a savepoint in which nothing changed. Seeded and deterministic, with a number of seeds that runs in a few seconds, and a way to replay one seed that follows the package's existing conventions."
**Interpretation:** Reuse the core's model of a transaction (`mtx` in `model_test.go`), give it a copy for a savepoint, and drive the real `Tx` and the model with the same random steps from a seed.
**Inferred intent:** A savepoint's restore of what the transaction records for its commit object, which the hand-written cases cover only in a few orders, is checked in many orders against an account that does not share its code.

### What I did

`/internal/core/model_test.go`: `mtx.clone`, a deep copy of the model's transaction, which is the model's savepoint.

`/internal/core/tx_test.go`: `TestTx_Savepoint_random` and `randomSavepoints`. Each seed opens one instance on a simulated store and runs two to six write transactions of 1 to 30 steps: creates (two shapes) and drops of `t1`/`T1`, `t2`/`T2`, `tX`/`Tx`, upserts and deletes of keys 1 to 4, annotations outside a savepoint, and savepoints, releases, and rollbacks to, also where the core must refuse them with `ErrInvalid`. Every result is checked against the model (`ErrTableExists`, `ErrNoTable`, the `found` of a delete). After each step, a dump of what the transaction reads must equal the model's state. Before the end, the instance must serve the committed state. A savepoint open at the end is rolled back to; then one transaction in four rolls back, and the rest commit. After a commit, the head's commit object must be the model's `effect()` followed by the annotations, and its parent the old head; a transaction whose effect is empty must leave the head. At the end of the seed, the model's strict replay of the log (`newModel(...).sync()`, `mstate.apply`) and a new instance on an empty cache must give the committed state.

The counts: the four cases of the brief, plus commits, rollbacks, rollbacks to, and savepoints open at the end. 400 seeds take about 2 s; under `-short`, `-race`, or the address sanitizer 40 seeds, without the minimums, as `TestSimulation` does it ("NOT CHECKED"). On 2026-11-20 the 400 seeds reached: 1,598 transactions, 1,179 commits, 419 rollbacks, 1,772 savepoints, 625 releases, 1,147 rollbacks to, 552 open at the end, 28 creates after a drop with a rollback to between, 168 rollbacks that undid a first touch of a key, 263 drops in a savepoint of a table that existed before it, 580 rollbacks to a savepoint in which nothing changed. The minimums are a quarter of those.

The replay uses the core's flag `-replay`, whose doc in `/internal/core/workloads_test.go` now names this test too; with it, the test runs that one seed and logs its steps. A failed seed prints `go test -run '^TestTx_Savepoint_random$' -v ./internal/core/ -replay N`, with `-race` or `-asan` as `replayCommand` does.

`/internal/core/README.md`, section "Tests", describes it.

### Why

The model already computes the net effect of a transaction in maps, and its strict `apply` already refuses a commit object that is no net effect, so the test needed only a copy for the savepoint.

### What worked

Two mutants of `Tx.RollbackTo` through `go test -overlay`: one that returns before it restores `tx.tables` and `tx.order` failed 206 of 400 seeds, and one that clears `dropped` of each restored entry failed 103.

### What didn't work

With steps of 1 to 16, the 150 seeds of the first version reached the drop then create with a rollback to between 0 times; with 1 to 30 steps, 10 in 150, and 28 in 400. A first mutant that I wrote crashed the test with a nil pointer in `spoolCommit` rather than making it fail an assertion, which says nothing about the test; I replaced it.

A shell heredoc that appended to the test file was refused by the worktree's sandbox: "this command is too complex to verify that it stays inside the worktree". I used the editor tool instead.

### What I learned

The test found no fault in the core. The case of a drop then a create across a rollback to needs the drop before the savepoint, since a rollback to that undoes the drop leaves the table there and the create then fails; so it is the rarest case.

### What was tricky

Defining "first touch" the way the core records it: a key of a table that existed when the transaction began and that the transaction has not upserted or deleted since; a table created by the transaction records no keys.

### What warrants review

- `first`, `rollbackTo`, and the counting in `randomSavepoints` in `/internal/core/tx_test.go`: that each count means the case the brief names.
- That the model and the core agree on the order of `changed` entries, which the comparison of the commit object depends on.

### Future work

None.

## Step 2: Transactions in the engine's simulation

**Author:** tx-checks-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "**The simulation of the engine with transactions.** Callers also run transactions: write transactions of several statements that commit or roll back, with failed statements inside and with `create table` and `drop table`, and read-only transactions, mixed with the single statements of today, under the faults and schedules the simulation already has (lost leases, stops, crashes, unknown outcomes). The checks, extended accordingly: [...] Each seed must reach a pinned minimum of committed transactions with more than one statement, as the existing workloads pin their minimums."
**Interpretation:** Give each caller a chance per turn to run a transaction of statements from the run's sequence, record each transaction, and replace the check "one statement per commit" with checks per transaction.
**Inferred intent:** The rules of slice 3 that matter most, that a commit object is exactly what the transaction did and that nothing of a transaction is seen before its commit, are checked under faults, against SQLite.

### What I did

`/internal/engine/simulation_test.go`:

- `workload.txs`: one turn in `txs` (4 in both workloads) is `run.transaction`: one in four read-only, two to six statements of the run's sequence, a wait of up to 500 ms before each, a timeout of up to the workload's for the begin, each statement, and the end; a commit, or one in four a roll back; a statement refused for want of the lease or by a stop ends it early with a roll back. So no caller leaves a transaction open, and the open question of the idle write transaction that keeps a lost lease from being taken back is never met.
- `txRecord` holds a transaction's statements, the count of acknowledged writes at its begin, its end, and the numbers of a clock of events (`run.tick`) for the begin's call and return, the end's call and return. `attempt` got `tx`, `gen`, `start`, `stop`; `node.gen` counts the opens of a node's database, so that the engines of one node are told apart. A statement of a transaction takes the transaction's count of acknowledged writes, and its writes are acknowledged by the commit, not by the statement.
- `run.verify`: a commit object holds the statements of one call. For a transaction: all of the one transaction, a write one, whose caller called the commit, which succeeded, had an unknown outcome, or did not return; its IDs exactly the transaction's statements that succeeded and write, in order; its parent the state that the transaction's statements saw. Parameters checked for every statement of a commit. Every note of a commit runs in SQLite.
- `run.verifyTxs`: all statements of a transaction saw one state, which holds what was acknowledged before the begin; a fresh oracle runs the log up to that state, `pragma query_only = 1` for a read-only transaction, and each statement that succeeded or failed with SQLite's error, in order, comparing every answer, a write that succeeded included. It also gives the writes of a committed transaction whose net effect is nothing (every write of a table absent at the beginning and at the end, in SQLite), which must not be in the log; the check of acknowledged writes skips them.
- `run.verifyOverlaps`: no write transaction's begin returns between another's begin return and its end call on one engine, and no write of its own both starts and commits in that window.
- `tallyOf` counts transactions by kind and end, statements in a transaction, and commits of more than one statement; `TestSimulation` pins minimums for them, from a fifth to a quarter of what 1,000 seeds gave: 0.4 commits of more than one statement for each seed of the default workload and 0.6 of the hammer workload, where 1,000 seeds gave 1.48 and 2.49.

`/internal/engine/README.md` describes the transactions of the workloads and the new checks, and has the results of 1,000 seeds.

### Why

The parent check of a committed transaction is the strongest form of "no two write transactions overlap" that does not depend on time: the log is a chain, so a commit whose parent is the state of its beginning had no commit between. The clock of events covers transactions that rolled back or did not write.

### What worked

1,000 seeds of each workload passed with no violation, in 75 s: in the default workload 14,176 transactions, 1,481 commits of more than one statement; in the hammer workload 30,012 transactions, 2,493 commits of more than one statement. No fault was found in the merged code.

### What didn't work

The first run failed on seeds 0 and 1: `the acknowledged statement 28, "creATE /* id 28 */ ...", is not in the log`. The transaction had created and dropped the same table, so its net effect was nothing and it wrote no commit object, which is the core's rule. The check now excuses exactly those transactions, judged in SQLite.

A first `tables` of the oracle through `oracle.run` would have failed the test, because `run` refuses a value that is not an integer; it reads `sqlite_schema` with its own statement.

A Python script given inline to Bash was refused by the sandbox once ("this command is too complex to verify that it stays inside the worktree") when it was followed by other commands; scripts in the scratchpad, run with the path as an argument, worked.

### What I learned

A begin of a write transaction often gives up its wait in the hammer workload: 6,228 of 22,565 write transactions did not begin within their timeout, because four callers share one writer.

### What was tricky

What "acknowledged" means for a statement of a transaction: the time that matters for "what it must see" is the begin of its transaction, not the statement's start, or a read-only transaction would be required to see what committed after it began.

The close of a node while a transaction is open: the next engine of the same node must not count as the same writer, hence `gen`.

### What warrants review

- `run.transaction`, and the classification of the end: a roll back that returns an error is a violation, and a commit's error other than unknown is "not committed".
- `verifyTxs`: the test of a net effect of nothing, `tableOf`, and that a transaction whose statements saw more than one state is still compared on the state of the first.
- `verifyOverlaps`: that each window is one where the writer's place is surely held.

### Future work

None.

## Step 3: Planted bugs of transactions

**Author:** tx-checks-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "**Planted bugs for transactions**, in the way the existing planted bugs are done, each found by the simulation within its seeds by a violation of its own: (a) a rollback to the savepoint that is skipped after a failed statement, so a failed statement's change commits; (b) a read-only transaction that reads the newest state for each statement instead of the state at its beginning; (c) a rollback of a transaction that keeps one of its statements. If one of them cannot be found because slice 1's statements cannot make the difference visible (an insert has one row and checks its key before it writes), say so with the reasoning, and plant the nearest bug that can be found."
**Interpretation:** Three new fields of `bugs`, each at the place of its decision in `/internal/engine/tx.go`, and three entries of `TestSimulation_plantedBugs`; probe (a) first.
**Inferred intent:** Show that the new checks can fail, each for the reason it is there.

### What I did

`/internal/engine/bugs.go`, `/internal/engine/export_test.go`, `/internal/engine/tx.go`:

- (a) as asked, `skipRollbackTo`, released the savepoint of a failed statement instead of rolling back to it. 100 seeds of the default workload found nothing, with nothing else wrong either. No statement of slice 1 fails after it has changed anything: an insert checks its table, its column, and its key before its one row; a create and a drop check their names first; the context is checked before the write; and no yield to the scheduler sits between the write and the annotation, where a stop could come (`grep Yield internal/core/tx.go` shows only the one after the manifest swap). So the bug was removed again, and the nearest one planted: `keepFailed`, which skips the rollback to the savepoint and annotates and releases the failed statement, as if it had succeeded.
- (b) `readNewest`, in `Conn.ready`: a statement of a read-only transaction ends the transaction's read and starts a new one, after its event has recorded the old position.
- (c) `rollbackKeeps`, in `Conn.Rollback`: a write transaction with one write commits instead, and the roll back returns nil.

`/internal/engine/simulation_test.go`: entries for the three, with `readNewest` on the hammer workload, which commits between the statements of one transaction more often. Two existing entries got further symptoms that the transactions now show: `skipUnique` also by an insert of a write transaction that succeeds where SQLite fails, and `keepRead` by "the rows of an earlier query of the connection are open", because rows of a query in a transaction stay open under it.

With 200 seeds each (`S4QL_ENGINE_SIM_SEEDS=200`): `keepFailed` 191, `readNewest` 185 (168 by a statement whose answer differs from SQLite's on the state of the beginning), `rollbackKeeps` 110; the old bugs `skipUnique` 179, `ackUnknown` 44, `checkOutside` 72, `keepOpen` 199, `keepRead` 112, `annotateNext` 151. Every seed that found a bug found nothing without it. The default run of `TestSimulation_plantedBugs` took 2.4 s, against 1.0 s before.

### Why

A bug that no check can find shows nothing about the checks; the probe records why the rollback to the savepoint is not yet observable, so that a later slice with statements that can fail after a write knows to plant it again.

### What worked

Each bug was found by its own violation in the first seeds.

### What didn't work

The first run of the planted bugs with 100 seeds failed on three entries, none a fault of the code: `skipUnique` seed 94 `finds the bug by another violation: [the statement 29, ... of the transaction 4 (write), which began at commit 5, gives columns "", rows [], and in SQLite 19/1555: UNIQUE constraint failed: SQLITE.FOR]`; `keepRead` seed 0 by `s4ql: the rows of an earlier query of the connection are open`; and `rollbackKeeps` seed 8 by `the end of the transaction 1: the instance has stopped: closed`, because the first version returned the error of the commit it made. The first two are symptoms of their bugs, now listed; the third now returns nil, as a roll back does.

### What I learned

A read-only transaction that moves on is seen in two ways: a statement whose answer differs from SQLite's on the state of the beginning, and the later statements' events that report another state. The first needs a commit that changes what the statement reads; the second only a commit.

### What was tricky

Keeping the messages of the violations apart, so that "own" means one thing: the result of a statement of a transaction now names its kind in parentheses, "(read-only), which began at commit".

### What warrants review

- The three bugs in `/internal/engine/tx.go`, and the reasoning about (a) in the README's section on planted bugs.

### Future work

Plant `skipRollbackTo` again when a statement can fail after a write, such as an insert of several rows.
