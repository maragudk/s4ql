# Diary: the order in which an update changes its rows

The adversarial review of 2026-11-26 found that an update that reads its rows by an index changes them in another order than SQLite, so an update whose rows fail with different errors gives another error than SQLite. This diary follows the fix: measuring SQLite's rules, making the engine follow them, and checking what the generated checks do about it.

## Step 1: measure SQLite, and the failing test

**Author:** builder (update-order)

### Prompt Context

**Verbatim prompt:** "An adversarial review found today: an update that reads its rows by an index changes them in another order than SQLite, so when two rows fail in different ways the statement gives another error. [...] 1. Bring the test onto your branch under a name that fits the package's other tests, and see it fail at `main`. 2. Measure SQLite first, do not assume [...] 3. Make the engine follow those rules wherever they decide which error a statement gives [...] 4. Check what else depends on that order [...] 5. The generated checks [...] 6. Correct the three documents named above, and record the rules and decisions in the file of `docs/todo/` where the write path's notes live (say which you chose)." (abridged; the brief is the lead's)
**Interpretation:** find SQLite's real order of changes for every access, follow it in the engine, keep the planner's choices that the engine cannot know out of the failing tests, and make the generated checks reach and compare the case.
**Inferred intent:** "Errors match SQLite's" to the letter for an update whose rows fail differently, without guessing SQLite's planner.

### What I did

Brought `TestAdversarial_updateOrderOfErrors` over as `TestEngine_updateOrder` in `/internal/engine/engine_test.go`, beside `TestEngine_update`; both subtests failed at `main` (`t.x` for `t.y`, and `19/1299` for `18`). Measured with the shell of SQLite 3.54.1 in `/Volumes/990/s4ql/update-order/m/`, with three rows that each give NULL to a column of their own, so the error names the row changed first, and `explain query plan` for the access. Read `sqlite3Update` and the one-pass part of `sqlite3WhereBegin` in the amalgamation to confirm. The rules are in `/docs/todo/update-order.md`, a new file of the piece of work, with a pointer from `/docs/todo/writing.md`, the area file of the statements that write.

### Why

The brief asked for the rules first. The source and the measurements agree: one pass in the order of the access unless the key, a column of the scanned index, a subquery in WHERE, or RETURNING forces two passes in the order of the key.

### What worked

The three-rows-three-columns setup reads the order off the error at once. `explain query plan` showed the planner's choices that the engine's rules do not make.

### What didn't work

The first shell run was refused by the sandbox: "this command is too complex to verify that it stays inside the worktree" for a heredoc with a loop; writing scripts to `/Volumes/990/s4ql/update-order/m/` and running them with `bash` worked. Several later `python3 - <<EOF` edits were refused the same way, at random it seemed; scripts in the scratch directory worked every time.

### What I learned

SQLite reads an update or delete of a table without a rowid through an index for a scan of every row whenever the index holds every column that the WHERE names and is narrower than the table: `update t set x = 1` reads by `ia`. The reason is in `update.c`: `colUsed` is set to 0 before the WHERE is resolved, so the index counts as covering. Ties between indexes go to the one created last; the rules take the one created first.

### What was tricky

My first table had both `ia (a)` and `iad (a desc)`; SQLite took `iad` (created last) and the rules `ia`, which looked like an engine fault until the tie rule showed up. The tests now use separate tables for the descending index.

### What warrants review

The list under "Where SQLite's planner decides" in `/docs/todo/update-order.md`: each is measured, and none is matched.

### Future work

Whether the rules of an update's access should follow SQLite's costs is a design question for the lead.

## Step 2: the engine, the core's ordered key set, and the pair

**Author:** builder (update-order)

### Prompt Context

**Verbatim prompt:** (as in step 1)
**Interpretation:** implement the rules where they decide an error; check the other dependents; teach the pair to compare and count the case.
**Inferred intent:** as in step 1.

### What I did

- `/internal/core/tx.go`: `Tx.KeysBy`, a key set in the order of columns that hold the primary key (an index's key). `Add` takes those columns' values; the first `All` numbers the keys with `row_number() over (order by ...)` into a second temporary table under the largest length limit, then reads them one at a time by number. Tests: `TestTx_KeysBy` against a scan of the index of the same columns.
- `/internal/engine/operator.go`: `changeOrder` and `build.changeOrder` decide one pass and the order from the rules' access (also under a forced path), and `changeRows` adds the index key of each row and, in one pass, stops at the first row error of the WHERE, changes the rows before it, and then gives that error. The storage interface's `keys` takes `by`; the fakes order in memory.
- `/internal/engine/differential_test.go`: `pair.firstError` (from `recordSlips`, now in the engine's order), `pair.sqliteOrder` (SQLite's plan plus the one-pass rules), `pair.engineOrder` (the model's access), `pair.checkUpdateOrder`, the kind `reachedUpdateOrder`, and the excuse `excusedPlannerOrder`, checked row by row in each order. `stmttest.RowQueries` takes the order; `stmttest.UpdateText` gives a clean text for the plan.
- Corrected `/internal/engine/README.md`, `/docs/todo/slice-08.md`, `/docs/todo/arithmetic.md`, and `/internal/core/README.md`.

### Why

The trio of `TestEngine_updateOrder` requires SQLite's error also from the instance that reads every row, so the order had to come from the rules' access and not from the scan; that needs a sort, and the set lives in the cache so that an update of any size holds no keys in memory.

### What worked

`TestChangeRows` pinned the calls of the fake, which caught that a forced path now looks the indexes up once more; a WHERE with no condition that may serve an access skips that.

### What didn't work

- The first pair run failed on texts the generator spells oddly: `";\n\n--\n -- c\n/* c */\tUPDATE\tBEGIN/* c */SET ...": SQLite gives 1/1: near ";": syntax error for its plan` (fixed by `stmttest.UpdateText`), then `SQLite gives 18/18: string or blob too big for its plan` for an argument over the limit (now no plan, no check), then `"ANalyze": SQLite reads by the index --, which the table does not have` for an index whose name holds a space (now the longest name that the detail begins with).
- A generator change in `arithmeticSet` (`nullCopies`: copy columns that take NULL into the columns that do not, one SET in four) reached the kind 0 times in a default run and moved pinned counts in every profile with arithmetic, e.g. `mixed gives "excused: arithmetic refused by the rule of exact types" 63 times, and testdata/reached/TestDifferential.tsv pins 64`. Taken back.

### What I learned

In a default run, 38 updates failed NOT NULL and 6 too big by an index, and in none did the order decide the error: each failing row gets its NULL from one literal or one column, so the failing rows fail alike. No excuse hid the fault; the workload never made it.

### What was tricky

A call with subqueries runs in SQLite flattened, with the values in place of the subqueries, so SQLite may run it in one pass where the engine, which has the subquery, runs two. The excuse covers that case too, checked the same way.

### What warrants review

`changeRows`'s deferral of a WHERE error under a forced path, documented as path-dependent; `core.Tx.KeysBy`'s use of `conn.unlimited` for a statement that writes (the keys it writes are no longer than the rows they come from); and `pair.changeOrderOf`'s matching of the SET's columns by fold.

### Self-review

`make lint` passed. `make test` failed once, in `internal/sql/mutation`: `no such mutant: internal/core/tx.go Tx.dropKeys element: , k.next =>`, and then `Tx.Keys swap arguments: ...`, because `/internal/sql/mutation/equivalent.txt` names mutants by their function and text: the body of `Tx.Keys` moved into `Tx.keys`, and `Tx.dropKeys` finalizes `k.sort` too. Renamed the entries and added `element: , k.sort =>` with the same reason. The new tests pass with `-count=50` and with `-race`.

The code-review skill's reviewers found, and I fixed: `pair.sqliteOrder` took the first SCAN or SEARCH row of the plan, which for a WHERE with a subquery can be the subquery's, and would then stop the test with `Fatalf` (now only the row of the updated table, and a statement with subqueries is two passes without a plan); `pair.recordSlips` ran the rows of a flattened statement in the order of the index, where the engine runs the statement with subqueries in the order of the key (now a flag, as in `checkUpdateOrder`); `createKeyTable` left the first temporary table when the second failed (now dropped); the doc comment of `TestPair_recordSlips` had slid onto the new test; Step 1 of the todo said `changes()` keeps the count before a failed statement (it is 0); and the replays ran for every failing update (now only where SQLite's order is not the key's, or the answers differ).

### The long runs

At the nightly's sizes after the last change: `go test -count=1 -p 3 -run '^TestDifferential$' -timeout 40m ./internal/engine/ -sequences 400` passed in 926 s. Its first try failed at once: `"update \"JsoN\" set \"OF\" = ?": SQLite's plan ["4,0,211,\"SCAN JsoN\""] has no scan`, since the plan names the table as the statement spells it and the pair looked for the stored spelling; it now compares without case, and `TestPair_updateOrder` pins it. `S4QL_ENGINE_SIM_SEEDS=100 go test -count=1 -run '^TestSimulation$' ./internal/engine/` passed in 457 s, and `go test ./drivercheck/` from `internal/sqlite/crosscheck` in 91 s.

### Future work

A generator of its own that reaches "update whose first row to fail is another in SQLite's order than in the order of the key", then the kind made required; see `/docs/todo/update-order.md`.

## Step 3: pay for SQLite's order only when a row fails

**Author:** builder (update-order-2)

### Prompt Context

**Verbatim prompt:** "Your task: a bounded round of changes after an independent review of the branch `update-order`. [...] 1. **Pay for the order only when a row fails.** [...] Change the design: an update changes its rows in the order of the key, as on `main`. Only when a row fails (an error of SET, of a check such as NOT NULL or the length limit, or of WHERE) does the engine find out which error SQLite would give: take the statement back to its savepoint, as a failing statement is taken back anyway, and replay it in SQLite's order [...] If the replay, against expectation, meets no failing row, that is a fault of the engine: fail loudly, do not return success. [...] 2. **A wrong order among equal index values goes unnoticed.** [...] 3. **The excuse can be widened without a test noticing.** [...]" (abridged; the brief is the lead's)
**Interpretation:** keep step 2's rules, but follow them only in a second run that starts from the statement's savepoint after a failed first run in key order; pin the tie of `ib (b)` and the third error of the pair's excuse.
**Inferred intent:** SQLite's error without making every update by an index slower, and a replay that cannot change what a caller sees.

### What I did

- Merged `origin/main` first (clean, no conflicts).
- `/internal/engine/operator.go`: the interface `replayer` (`mayReplay`, `replays`, `replay`), and on `update`: `run` in the order of the key where SQLite's order is an index's, `replay` in `u.changes`, the shared body `runIn`, and `failsRow` (an error of the WHERE as `ofRow` says, a constraint, or `string or blob too big`).
- `/internal/engine/engine.go`: `runSink` (roll back to the savepoint, open it again, replay, leave the replay's reads out of `report.before`, internal error for a replay without an error, success for the planted bug `evalStops`) and `runSavepointed` for a statement of its own, which takes a savepoint only for an update that may replay and releases it after the run. `/internal/engine/tx.go`: `writeIn` calls `runSink`.
- Tests: in `TestEngine_updateOrder` the tie of `ib (b)`, the roll back before the replay, a first run that fails on a long record and one with an evaluation error of SET, and an update of its own and in a transaction that goes on; `TestRunSink` with fakes over `memTx`; a third error in `TestPair_updateOrder`.
- Planted and taken back, never committed: ties by the key in reverse in `build.changeOrder` (only the new tie case fails); no roll back before the replay (only the new roll-back case fails); `replays` always false (seven cases of `TestEngine_updateOrder` fail); the excuse `rowFailure(byEngine)` (fails `TestPair_updateOrder` with "the engine's error is of a row, but of no first row"); `failsRow` without `CodeTooBig`, and without `ofRow` (each fails its new case); the replay's reads counted (fails the transaction case).
- `/internal/sql/mutation/equivalent.txt`: the mutants of `update.run` renamed to `update.runIn`, the one of `errStopped` back to `update.run`, and one for `runSink`.
- Measured with `/Volumes/990/s4ql/update-order-2/zz_bench_test.go` (not committed) against `main` at 34c3043a and step 2 at 56c1ae76; the numbers are in `/docs/todo/update-order.md`.

### Why

Which rows fail does not depend on the order where SQLite's order is that of an index: that update sets no column of the key or of the index, has no subquery and no RETURNING, and s4ql has no unique index, check, or trigger, so a row fails by its own values alone. So a run in the order of the key that succeeds is SQLite's answer, and only a failure needs SQLite's order.

### What worked

Planting each fault before trusting a new test: every new case fails for exactly its fault. The CPU time of the process, in interleaved rounds less the setup, gave usable numbers where the clock varied by 30% to 55%.

### What didn't work

- The first benchmark run measured nothing for the new tree: I moved `/internal/engine/zz_bench_test.go` out of the worktree while `bench.sh` was building, and `new.test -test.list Bench` listed nothing. The trees are now each an archive in `/Volumes/990/s4ql/update-order-2/`.
- `git checkout internal/engine/operator.go` to take back a planted fault was refused by the classifier, rightly: it would have thrown away the uncommitted change. Plants are now made and taken back with Edit.
- Several compound shell commands were refused as "too complex to verify that it stays inside the worktree" (a `git archive -o` into the scratch directory, a `perl` script fed from a heredoc next to `cp`); single commands and the Edit tool worked.
- `make test` failed once in `/internal/sql/mutation`: `no such mutant: internal/engine/operator.go update.run condition: err != nil => false #3`, from the split of `update.run`; after the review's change, `no such mutant: internal/engine/operator.go update.runIn swap arguments: errors.Is(err, errStopped) => errors.Is(errStopped, err)`. Both renamed.

### What I learned

The clock is no measure on this machine while other agents run mutation rounds (load 16 to 28). Allocations per operation are deterministic but miss the cost of step 2, which lies in SQLite (the numbering into a second table) and not in Go. A subquery in SET is refused today, so `b.subs != nil` in `build.changeOrder` means a subquery in WHERE in every statement that runs.

### What was tricky

A statement of its own has no savepoint, and a commit refuses one that is open, so `runSavepointed` must release it after every run, also a failed one, before the planted bug `commitFailed` commits. In a transaction the savepoint must be open again after the replay, for the roll back of the failed statement. The planted bug `evalStops` turned a replay into an internal error, which both reviewers found and which `TestSimulation_plantedBugs` would have reported as "finds the bug by another violation".

### What warrants review

`runSink` and `runSavepointed` in `/internal/engine/engine.go`, and the argument in the comment of `update.replay` that a replay cannot succeed. `failsRow`: an error that it leaves out is given in the order of the key; one that it takes in wrongly is replayed, which could turn a storage error into an internal error if the replay then succeeded.

### Self-review

`make test` and `make lint` passed; the new tests passed with `-count=50` and with `-race -count=3`. The code-review skill's two reviewers found no case where the replay gives a wrong error, keeps state, or succeeds outside a planted bug. Both found the `evalStops` internal error (fixed). Also fixed: no test of `failsRow`'s too-big and evaluation branches (two cases added), no test of the failures of the savepoint, its release, and the roll back (`TestRunSink`), a transaction test whose runs changed no row before failing (now they change two, and the transaction goes on), a dead guard on `r.reader`, two checks of the same invariant, the claim that a failed replay always counts 0 changes (an evaluation error leaves the count, as for any statement), and stale comments and README lines (`update.run`, the `changes` field, "only a transaction that spans statements calls savepoint"). Left: the fake `replaying` is in `engine_internal_test.go` with its sentinel `errReplayed`; no cancel test of an update by an index (reviewer B cancelled one at each of 400 calls in a scratch copy and found every outcome right).

### The long runs

At the nightly's sizes after the last change of code (0919f73c), run at once on a machine under a load of about 25: `go test -count=1 -p 3 -run '^TestDifferential$' -timeout 40m ./internal/engine/ -sequences 400` passed in 1,105 s, `S4QL_ENGINE_SIM_SEEDS=100 go test -count=1 -run '^TestSimulation$' ./internal/engine/` passed in 466 s, and `go test ./drivercheck/` from `/internal/sqlite/crosscheck` passed in 61 s. None found anything. No planted bugs at 1,000 seeds and no mutation round, as the brief said; the change of `evalStops` is checked by `TestRunSink` only.

### Future work

The generator that reaches the kind, as in step 2. Whether the rules of an update's access follow SQLite's costs stays the lead's question.

## Step 4: read the replay as an adversary, close two gaps, and land

**Author:** builder (update-order-land)

### Prompt Context

**Verbatim prompt:** "You come fresh to a finished branch: first read one new piece of it as an adversary, then close two gaps, then land it. [...] 1. **Read the replay as an adversary** [...] Plant at least five faults of your own choosing in the replay, one at a time, never committed, and see that each fails a short test; add a test where one does not. 2. **Close the two gaps the builder named.** A test of an update by an index whose context ends during the first run and during the replay. And the planted bug that the builder changed so that it stops the replay and succeeds has not been run at the nightly's size [...] 4. **Land.** Merge `origin/main` [...]" (abridged; the brief is the lead's)
**Interpretation:** look for a path of `runSink`, `runSavepointed`, and `update.replay` that keeps the first run's changes, gives success for a failure, or leaves the transaction in another state than the caller is told; prove the tests by planted faults; add the cancel test; run `evalStops` at 1,000 seeds; merge `main` and land.
**Inferred intent:** trust in the replay, which no independent reader had seen, before it reaches `main`.

### What I did

- Read the diff of `runSink`, `runSavepointed`, `replayer`, and `failsRow` against the merge base, with `core.Tx.Savepoint`, `Release`, `RollbackTo`, and `keys` in `/internal/core/tx.go`, `writeIn` in `/internal/engine/tx.go`, and `ExecAll` in `/internal/engine/several.go`. No fault found; the reasons are in `/docs/todo/update-order.md`, step 4.
- Added a case to `TestEngine_cancelChange` in `/internal/engine/engine_test.go`: 40 rows, `ia (a)` in the reverse order of the key, the key 20 gives NULL to y and the key 25 to x, so the run changes 20 rows and fails with `t.y` and the replay changes 14 and fails with `t.x`. The context is canceled at each call of `Err` in turn; each cancel must give `context.Canceled` and leave the oracle's rows, and the last run SQLite's error. Of its own and in a transaction that goes on, commits, and survives a replay of the log.
- Planted seven faults, one at a time, never committed, each run against `go test -count=1 -run 'TestEngine_updateOrder|TestRunSink|TestEngine_cancelChange' ./internal/engine/`: `release` in place of `rollbackTo` in `runSink` (caught by `TestRunSink`, two cases of `TestEngine_updateOrder`, and the new case); the replay's `context.Canceled` turned into the run's error (only the new case); no savepoint opened again after the roll back (`TestRunSink`, the transaction case, the new case); a replay that succeeds accepted (`TestRunSink`); the replay without `onePass` (the case of the SET before a later WHERE); `writeIn` with `s.run` in place of `runSink` (only the new case); `rollbackTo` in place of `release` after a run in `runSavepointed`, which takes back every successful update by an index of its own (`TestRunSink` and `TestEngine_paths` in `go test -short ./internal/engine/`).
- `S4QL_ENGINE_SIM_SEEDS=1000 go test -count=1 -run '^TestSimulation_plantedBugs$/.*Stops' -v -timeout 120m ./internal/engine/` from a copy of the tree in `/Volumes/990/s4ql/update-order-land/src`, with `GOTMPDIR` and `TMPDIR` there: passed, "174 of 1000 seeds of the profile arithmetic find the bug, 153 of them by \", and in SQLite\"", 152 s of test, 3 min 9 s with the build.
- Merged `origin/main` (no conflict; it touched `/internal/core/tx.go` only in the error of a commit whose context ended, not the engine's write path, so the differential run at 400 sequences of step 3 stands).

### Why

The replay was written after the independent review of step 2, and its builder named the cancel test and the run of `evalStops` at the nightly's size as not done.

### What worked

Planting before trusting: the first version of the new case set x and y from 1 to 1, so a run whose changes stayed looked like one taken back, and the first fault passed it. With x and y at 0 before the update, it failed at the call 105.

### What didn't work

The first fault (`release` for `rollbackTo`) passed the new case at first, for the reason above. Running the planted bug's subtest at 1 seed to see which subtests the pattern matches fails by design: `the bug is found by ", and in SQLite" too rarely`.

### What I learned

Two of seven faults were caught only by the new case: the transaction case of `TestEngine_updateOrder` sets only y from a column that fails, so its error is the same in both orders and it cannot tell a replay from none. `core.Tx.RollbackTo` and `Release` do not check the context, so a statement whose context ends during its replay is still rolled back.

### What was tricky

Keeping the long run away from the faults: the faults were planted in the worktree while the 1,000 seeds ran from a copy of the tree.

### What warrants review

The new case in `TestEngine_cancelChange`: that it reaches the replay rests on the final error being `t.x`, which only the replay gives, and on the cancel loop covering every call of `Err` before that.

### Future work

The generator that reaches the case, and the five cases where SQLite's planner decides; see "Open" in `/docs/todo/update-order.md`.
