# Diary: an instance that has lost its lease ends the idle write transaction

Since slice 3, an application can hold a write transaction open across statements. The open `core.Tx` holds the writer lock of the core from `DB.Begin` to its end, and the lease task needs that lock to take the lease (`DB.acquire`) and to catch up (`DB.resume`). An instance that lost its lease while a write transaction sat idle therefore never served again until the application ended the transaction. Markus decided on 2026-11-19 that the instance fails such a transaction at once, so that it can take the lease back. This diary is of the builder on the branch `lease-ends-tx`.

## Step 1: the end for the lease in the core

**Author:** builder (lease-ends-tx)

### Prompt Context

**Verbatim prompt (excerpt of the brief):** "When the instance has lost its lease, it fails an open write transaction that is not committing, at once, so that it can get the lease again. The transaction is rolled back in the cache and the writer's place is freed. Its later statements and its commit get the temporary error that an instance without a valid lease gives (`core.ErrNoLease`, the public `ErrNotServing`), and its roll back returns nil. A commit that is under way is not touched: it goes on, and its manifest swap decides, as the spec says." And: "A call of the transaction that is running at that moment (an upsert, a scan, a savepoint) must not be cut in the middle in a way that breaks the cache or races: decide how the two are ordered (the lease task waits for the running call, or the call sees the end at a safe point), write the decision down, and test it. A scan whose rows are still being read is the hard case: the end must not leave a statement of SQLite open on the write connection, and the place must still be freed in bounded steps."
**Interpretation:** the lease task, before it waits for the writer lock in `acquire` and `resume`, ends the write transaction that holds it, unless that transaction is in its commit or its roll back.
**Inferred intent:** an instance with an idle transaction must not stay a spare for ever; nothing wrong can commit either way, so availability is what is at stake.

### What I did

- `/internal/core/core.go`: `DB.writing`, the write transaction that holds the writer lock, guarded by `DB.mu`; `DB.useLocked`.
- `/internal/core/tx.go`: `Tx.ending` and `Tx.lease *leaseEnd`, both guarded by `DB.mu`. `DB.Begin` records the transaction and gives back its use of the connection in one critical section that checks `usableLocked`, so the lease task either finds the transaction or finds no transaction that can still begin. Every call checks `Tx.lease` under the same mutex in which it counts itself on the connection (`Tx.use`). `Tx.leave` lets the last call that leaves a transaction that was ended do the end (`Tx.endForLease`: forget, `rollback`, clear `DB.writing`, give the connection back, release the writer lock, log the event). `Tx.Commit` and `Tx.Rollback` mark `ending` first (`Tx.useToEnd`), after which the lease task leaves the transaction alone. A scan looks at `Tx.Lost` before each row. `Tx.end` of a transaction that was ended releases nothing, because the end for the lease did. New exported `Tx.Lost`, for the engine's event.
- `/internal/core/lease.go`: `DB.endForLease`, called at the start of `acquire` and `resume`. If no call uses the connection, the lease task counts itself on it and does the end at once; otherwise it only marks the transaction, and the call that runs does the end when it leaves. Then the lease task waits for the writer lock, as before.
- `/internal/core/errors.go`: `ErrTxLeaseLost`, which wraps `ErrNoLease`, with the text "the transaction was ended because the instance lost its lease". The error of each call is `ErrTxLeaseLost: <reason>`, with the reason of `refusalLocked` at that moment, such as `not_holder`.
- `/internal/core/telemetry.go`: the event `transaction_ended` at Info (Warn if the roll back in the cache fails), with `outcome` (`rolled_back` or `failed`), `reason`, `commit.parent`, `transaction.waited_for_call`, `duration.total` from the mark to the end, and `error`.
- `/internal/core/bugs.go`, `/internal/core/export_test.go`: the planted bug `keepIdleTx`, which makes `endForLease` do nothing.
- `/internal/core/lease_test.go`: `TestLease_openTx`, on the fake clock of synctest, with the helper `idleWhileLost`, which is the review's sequence: a is cut off, b takes the lease and commits, a finds the loss. Cases: the idle transaction (every call gives the error, also once a serves; the read transaction open beside it is untouched; the next write transaction commits; the log holds nothing of the ended one; the event); the roll back returns nil; a loop over a scan whose body runs at the loss (the instance does not serve while it runs; the nested `Get` and the next row fail; it serves after the loop); rows of a scan held by `iter.Pull2` (no serving until `next` or `stop`); and the planted bug (no serving an hour after b closed, serving after the roll back).
- `/internal/core/tx_test.go`: the test of a commit under way when another instance takes the lease now also asserts that its error does not match `ErrTxLeaseLost`.

### Why

The order between the lease task and a call that runs is the hard part. SQLite's connection and the core's `conn` are used by one goroutine at a time; a roll back from the lease task while a scan holds a statement would abort that statement and let the scan step it later, maybe while the next writer's transaction runs on the connection, which the engine's README already names as the reason why the end of a transaction waits for a loop. So the lease task never touches the connection while a call uses it: it marks, and the call that runs ends the transaction when it leaves. The count of users and the mark are read and written under `DB.mu`, so exactly one of the two does the end.

### What worked

The tests went red without the change: with `endForLease` made to return at once, every subtest of `TestLease_openTx` failed (for example `lease_test.go:1094: Expected nil error, but got "context deadline exceeded" ... the instance does not serve again`). With it, the whole core package passed in 54 s, the simulation included.

### What didn't work

A shell command with a long heredoc of Python that also ran `go build` was refused by the harness of the worktree ("this command is too complex to verify that it stays inside the worktree"). I put the script in the scratchpad and ran it with a path argument. Its first run failed on an assertion because the loop of `Tx.Scan` is indented by two tabs, not three.

### What I learned

The resume path (`holder && !serving`) is reached only after a commit of unknown outcome, which is the transaction's own commit, so it cannot meet an idle transaction today. `endForLease` is called there too, as the brief asked, and does nothing then.

### What was tricky

`DB.Begin` used `defer d.unuse(d.w)`. If it had recorded the transaction before that unuse, the lease task could have seen one user, left the end to "the call that runs", and no call would have done it. The record and the unuse are now one critical section.

### What warrants review

- That every path that releases the writer lock does so exactly once: `Tx.endForLease` for a transaction that was ended, `Tx.end` otherwise.
- `Tx.state` checks the end for the lease before `failed`, so a transaction that failed in the cache and was then ended reports the lease.
- Rows of a scan that the application never reads again hold the writer lock until it reads or stops them. That is the one case where the instance does not serve again by itself.

### Future work

Mutation testing of `endForLease`, `Tx.leave`, `Tx.use`, and `Tx.useToEnd` (a later task).

## Step 2: the engine and the driver

**Author:** builder (lease-ends-tx)

### Prompt Context

**Verbatim prompt (excerpt of the brief):** "In `internal/engine`: a `Conn` whose write transaction was ended this way gives the error for every statement and for `Commit`, `Rollback` returns nil, and after either the Conn is usable again. The event of the transaction says how it ended. Nothing of the transaction is in the log." and "In the driver (repository root): statements and `tx.Commit` give an error that matches `ErrNotServing` and no other sentinel, `tx.Rollback` gives nil, and the connection goes back to the pool and works once the instance serves again. Adjust the package documentation's section on transactions with one or two sentences."
**Interpretation:** the core's errors already flow through; what the engine needs is the event, and the driver needs a test and the documentation.
**Inferred intent:** the behaviour is visible and pinned at every layer an application meets.

### What I did

- `/internal/engine/storage.go`: `writeTx.lost`, `coreTx.lost` calls `Tx.Lost`; the fakes of the tests return nil.
- `/internal/engine/tx.go`: `transaction.ended` gives the event of a roll back that returned nil the error of the end for the lease, in `Conn.Rollback` and in `ending.end`. So the event `transaction` keeps `transaction.end` as `commit` or `rollback`, what the application called, and its `outcome` is the new `lease_lost` with the error text.
- `/internal/engine/telemetry.go`: `outcomeOf` gives `lease_lost`, at Debug, for `core.ErrTxLeaseLost`, before the case of `ErrNoLease`. Statements refused that way get it too.
- `/internal/engine/tx_test.go`: `TestConn_leaseLost` with the helper `leaseLost`: for commit and for rollback, a serves again after b is closed while the transaction is still open on the connection; insert and query give `ErrTxLeaseLost`; the event; `ErrNoTx` after; the connection reads `rows 2` and commits a new transaction. A second case holds the rows of a query of the transaction in `iter.Pull2`: a does not serve an hour after b closed, the next row gives the error, and a serves after `rows.Close`.
- `/tx_test.go`: `TestTx_leaseLost` through `database/sql`, in synctest, with the store `faulty` down while b takes the lease: statements and `tx.Commit` match `ErrNotServing` and none of `ErrStopped`, `ErrOutcomeUnknown`, `ErrNotSupported`, `ErrTooLarge`; `tx.Rollback` is nil; `Stats` shows two open and two idle connections, so the connection of the transaction went back to the pool; the next transaction commits with `SetMaxOpenConns(1)`.
- `/s4ql.go`: two sentences in the section "Transactions" of the package documentation.
- `/internal/sql/mutation/equivalent.txt`: the entry of the mutant of `ending.end` names the new expression. `TestEquivalent` failed without it: `no such mutant: internal/engine/tx.go ending.end swap arguments: first(e.err, err) => first(err, e.err)`.

### Why

The engine and the driver pass the core's error on unchanged, and `publicError` maps `ErrNoLease` to `ErrNotServing`, so no new sentinel reaches the application.

### What worked

`go test -short ./...` passed after the entry of `equivalent.txt`; the repository builds with the tag `s4ql_browser`, natively and for `GOOS=js GOARCH=wasm`.

### What didn't work

`TestEquivalent` of `/internal/sql/mutation`, as above, until the entry named the mutant as the generator now makes it.

### What I learned

The driver needed no code: `Rollback` already returned nil for an error of the engine that matches `core.ErrNoLease`, and the engine's roll back returns nil anyway.

### What was tricky

The new mutant of `ending.end` is no longer quite unreachable: a commit refused while a loop reads, of a transaction that the instance ended, has both errors. Only the event differs, and I left the decision whether to pin it to the later mutation testing, as the entry says.

### What warrants review

That `lease_lost` as an outcome of the engine's events, at Debug, is the right word and level.

### Future work

None beyond the mutation testing.

## Step 3: the simulations

**Author:** builder (lease-ends-tx)

### Prompt Context

**Verbatim prompt (excerpt of the brief):** "Add callers that keep a write transaction open and idle across faults, in the engine's simulation and, if its workload has transactions that hold, the core's. Add a check that would have caught the problem: an instance that could get the lease again does get it, in bounded simulated time, while a transaction sits idle. Plant one bug for it in the way the existing planted bugs are done (the lease task not ending the idle transaction), and show that the simulation finds it within its seeds by a violation of its own. Pin the minimum of transactions ended for the lease that each workload must reach."
**Interpretation:** idlers in both simulations, a check at the end of the workload where "could get the lease" is certain (the store is calm and the other instances are closed), and the planted bug in the core.
**Inferred intent:** the simulations must have caught the bug that the review found by hand.

### What I did

- `/internal/engine/simulation_test.go`: the workload `idleWorkload` ("idle"), the default workload with `idlers: 1` and `idle: time.Minute`. `run.idler` runs two write transactions through `run.transactionOf`: after their statements they sit idle while `n.client.CutOff` cuts the instance off, half of the time for 30 to 60 s; the second is cut off for 2 minutes always, sleeps 3 minutes, and is kept open (`node.kept`). The write of the end runs in the kept transaction (`run.endKept`), which rolls it back when the instance ended it; `run.readsFree` calls `endKept` first and gives the violation "<node>, where the write transaction <id> is open and idle, does not serve ten minutes after the end of the workload" for a node that still has it. The core of each engine gets a logger (`coreHandler`) that records the tick of each `transaction_ended`, and `run.verifyOverlaps` ends the place of a write transaction there. Counts: "transactions ended for the lease" and "kept transactions ended for the lease", pinned at 0.33 per seed each, with the other minimums of the idle workload at about a quarter of the first 1,000 seeds.
- `/internal/core/simulation_test.go`, `/internal/core/workloads_test.go`: the workload `idling` ("idle"), `competing` with `idlers: 1`; `run.idler` as in the engine, with `node.cut`, the simulated client under the wrapper; `run.verifyIdle` closes every other live node and requires the node of the first kept transaction to serve within `workload.liveness`, then rolls every kept transaction back. The planted bug `keepIdleTx`, with the control `instances = 1`, and the symptom "is open and idle, does not serve". `idleReaches` from the minimums that the 20 default seeds printed.

### Why

During the workload, "the instance could get the lease again" is hard to tell: another holder may be alive, the store faulty. At the end of the workload it is certain once the other instances are closed, which is what the review's sequence does too. So the kept transaction carries the check past the end, where the existing checks already close instances in turn.

### What worked

- Engine: 1,000 seeds of the idle workload passed in 31 s, with 1,348 transactions ended for the lease, 1,317 of them kept. With `DB.endForLease` made to return at once (by hand, then restored), 89 of the first 100 seeds failed, all by the new violation.
- Core: `keepIdleTx` was found in 97 of 200 seeds, 0 of 200 of the control, 0 without the bug (`S4QL_SIM_SEEDS=200 go test -run 'TestSimulation_plantedBugs/.*idle_write' ./internal/core/`). The 20 default seeds of `idling` passed, deterministically.
- Run times, each binary built from `74a7ac8` and from the branch, run in turns at a load of 12 to 19: the engine's `TestSimulation` 3.84 s and 3.49 s before, 5.45 s and 5.17 s after; the core's `TestSimulation` with `TestSimulation_plantedBugs` 41.04 s and 40.65 s before, 43.64 s and 43.08 s after.
- The new tests passed with `-count=20`, and with `-race -count=20`; the engine's simulation passed under `-race`.

### What didn't work

The first try put the idlers into the engine's default workload. It passed with no violation, but four minimums failed: `50 seeds give 5 of "unknown", and at least 7 were expected`, `0 of "fault: write applied after a crash"`, `120 of "commits"` against 125, and `2 of "transactions unknown"` against 3. The idlers' cut-offs and the writer place that their transactions hold change the default workload, so they got a workload of their own and the default one is as it was.

### What I learned

The engine's simulation cannot plant a bug of the core: `core.Options.bugs` is set only through the core's `export_test.go`. So the planted bug lives in the core's simulation, and the engine's check was shown by a hand-made mutant.

### What was tricky

`run.verifyOverlaps` of the engine would have reported every transaction that the core ended for the lease, since the core frees the place before the caller calls its roll back. Only one write transaction of an engine is open at a time, so the first `transaction_ended` of that engine after the begin is the end of that transaction's place.

### What warrants review

- The cut-off of the kept transaction is aimed: the idler cuts its own instance off for two minutes. That makes the case common; whether the reviewer thinks it is a fault of the harness like a crash or a close, or too scripted.
- `run.verifyIdle` in the core closes the other live nodes earlier than before; the final close loop skips them.

### Future work

None beyond the mutation testing.

## Step 4: the documents

**Author:** builder (lease-ends-tx)

### Prompt Context

**Verbatim prompt (excerpt of the brief):** "`docs/todo.md`: the open question on this in \"Transactions\" becomes the decision (Markus agreed on 2026-11-19 that the instance fails the transaction at once so that it can take the lease back), with your decisions of detail. The READMEs of the core and the engine describe the behaviour where they describe the lease and transactions."
**Interpretation:** the todo records the decision and the details; each README says what its package does and how it is tested.
**Inferred intent:** a reader finds the behaviour where the lease and the transactions are described.

### What I did

`/docs/todo.md`: the item "Open, with the product owner" became the decision, with the details of when, what the transaction gives, the order with a call that runs, the rows that nobody reads, the record in `DB.Begin`, the events, and the tests. `/internal/core/README.md`: two bullets under "A write transaction", the paragraph on `idling` with its numbers, the count of planted bugs, the seeds of the default run. `/internal/engine/README.md`: a bullet under "The commit and the roll back", the paragraph on the idle workload, and the run times.

### Why

The brief asks for it, and `docs/spec.md` is left to the lead and Markus.

### What worked

Nothing to report beyond the edits.

### What didn't work

Nothing failed.

### What I learned

Nothing new.

### What was tricky

The 23 ends in the 20 default seeds of the core's `idling` are what the event counted; that at least 20 were of kept transactions follows from the minimum of 5 that the run printed, a quarter rounded down, so the README says "at least 20" and not a number I did not measure.

### What warrants review

The spec still says "A write transaction that is already open commits only if its manifest swap still succeeds", which the lead rewords with Markus.

### Future work

The reworded sentence of the spec, by the lead.
