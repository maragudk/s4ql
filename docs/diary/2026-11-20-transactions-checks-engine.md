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
