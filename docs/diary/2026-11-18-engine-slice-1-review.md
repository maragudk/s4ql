# Diary: The review round of the engine of slice 1

Two independent reviewers checked the branch `engine-slice-1`, which holds the first SQL engine in `/internal/engine`. They found no correctness bug, but found tests that prove less than they seem to, and Markus reversed one design choice: a syntax error never depends on the state of the database. This diary is the round of changes that answers the review, on the same branch. The diary of the engine itself is `/docs/diary/2026-11-18-engine-slice-1.md`.

## Step 1: The brief

**Author:** engine-review-builder

### Prompt Context

**Verbatim prompt:** "You are a builder for s4ql (`maragu.dev/s4ql`) [...] The branch `engine-slice-1` holds the first SQL engine (`internal/engine`), written by an earlier builder and not merged. [...] Two independent reviewers checked the branch. They found no correctness bug in the engine, confirmed every test number of the first builder by running it again, and caught all 11 faults they planted themselves. They also found that some tests prove less than they seem to, and one design choice was reversed by the product owner. Your job is this round of changes, and no more. [...] 1. Take `stmt.StartedTable` out of production. [...] 2. The simulation cannot see a read transaction left open. [...] 3. The differential fuzzing is seed fuzzing. [...] 4. A refused statement is never checked. [...] 5. Events on panic paths are wrong. [...] 6. Operators hold guesses about SQLite beyond slice 1. [...] 7. `internal/sql/stmt` logs, and does not assert, how many texts escape comparison. [...] 8. Assert what the simulation workloads reach. [...] 9. Small records. [...] The driver is not part of this work. The engine keeps push-style rows. The names of SQLite's built-in virtual tables stay "not supported"."

**Interpretation:** Nine points, each with its own proof: remove the lookup of a started table from the engine and keep its classifier as test code; make the end of a simulation run take every read connection and plant a bug that leaves a read open; add a fuzz target over SQL text on a database with tables; check the statements that the engine refuses; log one event with a fault outcome on every panic path; make the unreachable branches of the operators internal errors; assert the excused counts of `stmt`; assert minimum counts of what the simulation reaches, and make the default workload reach transactions more often if that is cheap; and three small records in the roadmap and the todo.

**Inferred intent:** Trust in correctness: every test should prove what it seems to prove, and a fault that a test claims to find should be shown to be found.

### What I did

Checked out `engine-slice-1` in my worktree and merged `origin/main` into it; the merge had no conflicts. Read `AGENTS.md`, the READMEs of `/internal/engine` and `/internal/sql/stmt`, the spec's rule on syntax errors, the roadmap, the section "The SQL front end" of `/docs/todo.md`, and the engine's code and its differential and simulation tests.

### Why

The changes reach into tests that the first builder wrote with care; they have to keep what those tests prove while closing the gaps.

### What worked

The merge of main was clean, and `go build ./...` and `go vet` pass on the merged branch.

### What didn't work

Nothing yet.

### What I learned

`stmttest` is already a package for tests only, which imports the SQLite binding, so it is a natural home for the classifier of a started table: the tests of `stmt` and of `engine` can both use it.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: Points 1, 2, 5, and 9: the syntax error, the read left open, the events of a panic, and the small records

**Author:** engine-review-builder

### Prompt Context

**Verbatim prompt:** see Step 1.
**Interpretation:** Points 1, 2, 5, and 9 of the brief.
**Inferred intent:** A syntax error that never depends on the database; a simulation that sees a read transaction left open; one event for every panic.

### What I did

Point 1: moved `StartedTable` from `/internal/sql/stmt/started.go` to `/internal/sql/stmt/stmttest/started.go`, which is code for tests only; `TestStartedTable` and `FuzzStartedTable` stay in `/internal/sql/stmt/started_test.go`, since they use the helpers of that package's tests. The engine's `prepare` gives the error of `stmt.Check` as it is, and `Engine.started` is gone. `startedTable` in `/internal/engine/differential_test.go` accepts exactly the one difference: the engine's syntax error equals SQLite's on a fresh empty database, and SQLite on the real one gives `table <Raw> already exists` for the name that `StartedTable` gives. The tally counts these apart. The simulation now compares every statement that saw no state with SQLite on an empty database, instead of checking that it has one of three messages. New tests: `TestEngine_syntaxError` (a fake whose begin and read fail) and two subtests of `TestEngine_errors` on the real core (a table that exists, and a closed instance and a cancelled context). Removed the 14 equivalent mutants of `StartedTable` from `/internal/sql/mutation/equivalent.txt`. Checked the reviewer's sibling cases with the sqlite3 shell 3.54.1 and wrote them into `/docs/todo.md` and the known gaps of `/internal/sql/stmt/README.md`.

Point 2: at the end of a run, `run.takeReaders` makes each live instance take its two read connections at once, with the workload's timeout on the simulated clock, and closes each instance that did, which hands the lease to the next. An instance that has not done it in ten minutes is a violation. Planted `keepRead`: `Rows.Close` outside a loop skips the end of the transaction, the change the reviewer made by hand.

Point 5: `Exec` and `Query` are wrappers with a `returned` flag, and call `report.panicked` when they did not return, which logs `errPanicked` (it wraps `ErrInternal`) unless the event is logged already. `Rows.All` tells a panic of a source from a panic of the consumer by a flag set around `yield`. A consumer's panic stays an early stop with the outcome `ok`.

Point 9: the empty name in the restrictions of `/docs/roadmap.md`, and the two decisions of the lead in `/docs/todo.md`.

### Why

The spec's rule makes the lookup wrong, and the reviewer showed that the simulation could not see a read left open.

### What worked

The reviewer's exact change (`return nil` in place of `return r.end(nil)` at the end of `Rows.Close`) now fails `TestSimulation` at seed 1 of the default workload: `a does not take its 2 read connections ten minutes after the end of the workload: context deadline exceeded`. With the planted bug, `TestSimulation_plantedBugs/keepRead` found it in 2 of 9 seeds. 1,000 sequences of `TestDifferential` accepted the difference 102 times and found nothing else.

### What didn't work

A Bash call that ran a Python heredoc with an edit of `differential_test.go` was refused by the sandbox as "too complex to verify that it stays inside the worktree"; scripts written to the scratchpad and run by path work.

### What I learned

Only the instance that holds the lease can read (`DB.Read` fails at once with `ErrNoLease` on a spare), so the check of the read connections must hand the lease from instance to instance by closing them in turn.

Every sibling case of the reviewer gives its non-syntax error also on an empty database, so all of them stay a gap against the spec's rule; only the `already exists` of `CREATE TABLE` is covered by it.

### What was tricky

In `query`, the deferred end of a read transaction that was not built must not log on a panic, or the event would say `ok`; it logs only when `buildQuery` returned an error, and the wrappers log the panic.

### What warrants review

`run.takeReaders` in `/internal/engine/simulation_test.go`: whether ten minutes is safe for an instance to take the lease after the others close. The default and hammer workloads pass on the seeds run so far; the 1,000-seed runs come later.

### Future work

None.

## Step 3: Points 3, 4, 6, 7, and 8: text fuzzing, refused statements, the operators, the excused counts, and the workloads

**Author:** engine-review-builder

### Prompt Context

**Verbatim prompt:** see Step 1.
**Interpretation:** The rest of the code of the brief.
**Inferred intent:** Tests that prove what they seem to prove, with numbers that fail when something stops happening.

### What I did

Point 6: the operators build only plans of slice 1. `values` lost its names of columns and is a `rowSource`, which has no `columns`; `buildValues` takes one row of one value; a projection needs `*`; `insert` needs a value for every column and a key of one column; `CreateTable` needs one column with the key `[0]`; `uniqueFailed` takes one column. Everything else is an internal error, pinned in `TestInsert` and `TestBuild`.

Point 7: `excused` in `/internal/sql/stmt/check_test.go` requires exact counts in a full run with the default flags. I found that "not supported" texts where the oracle says `NotSaid` escape the comparison too, so they are an outcome of their own now, `notSupportedNotSaid`, and the count is the sum of both: 299 for the corpus, 256 for its mutants, 3,835 for the grammar, 377 for the generator's mutants, and 1,046 for the near misses.

Point 4: `sequence.next` gives whether the statement is of slice 1. `pair.step` fails when the engine refuses one; for any other refused text, SQLite must prepare it on an empty database without a syntax error (texts with a NUL byte or invalid UTF-8 are skipped, since the roadmap rejects them before parsing). The simulation counts a refused statement of slice 1 as a violation.

Point 3: the comparison of one statement moved from `differentialOn` into `pair.step`. `FuzzSQL` (on `memory`) and `FuzzSQL_core` (on the core) run any text after a fixed set-up, `fuzzSetUp`, and compare it as the differential test does, and the tables after it.

Point 8: added a reason to each failed attempt, and found that nearly all refusals are `ErrNoLease`, from the callers of the instance that does not hold the lease. A caller so refused now waits up to two minutes in `DB.Wait`, and stops if its instance still does not serve. The crasher now takes an instance that runs a statement. `TestSimulation` asserts least counts per seed, in runs of at least 50 seeds.

### Why

Each point closes a gap that a reviewer showed.

### What worked

Planted for point 4: a prepare that refused every insert with a negative value made `TestDifferential` fail at once ("the engine does not support a statement of slice 1") and `TestSimulation` at seed 0; a prepare that turned every syntax error into "not supported" failed with `the engine does not support it, and SQLite gives near "%": syntax error`. Both reverted.

Point 8, on 1,000 seeds of the default workload: refused or timed out 5,691 of 47,010 statements (12.1%, from 58%); succeeded 14,868, SQLite's error 21,725, unknown 594, cut by a crash 905 (from 18), commits 9,117. Hammer: 17,347 of 120,000 refused or timed out (14.5%, from 36%). The planted bugs with 200 seeds: `skipUnique` 123, `ackUnknown` 59, `checkOutside` 101, `keepOpen` 138 (from 193), `keepRead` 53, none found without the bug.

`FuzzSQL_core` made about 125 executions a second under a load of 28; `FuzzSQL` on `memory` about 5,800, so both exist.

### What didn't work

The first fuzz run of `FuzzSQL` found `CreAte tABle""("")A,0` within seconds: the engine gives `near "0": syntax error`, SQLite `unknown table option: A`. Then `CREATE TRIGGER A INSERT ON A BEGIN`: the engine `incomplete input`, SQLite `no such table: main.A`. Both are the known gaps of `stmt`, so `knownGap` excuses them as `stmt`'s tests do: the oracle does not say, or the oracle, which creates the tables a text names, gives the engine's syntax error. The two inputs stay in `/internal/engine/testdata/fuzz/`.

Running the compiled test binary with `-test.fuzz` failed with `testing: -test.fuzzcachedir must be set if -test.fuzz is set`.

A pre-existing fault of the test: the forms of `others` without a verb (`begin`, `""`, `" ; -- nothing"`) went through `Sprintf`, which appended `%!(EXTRA ...)`, so they were never the texts they claim to be. Fixed in `sequence.next`.

### What I learned

Only `ErrNoLease` made the default workload's refusals; timeouts are a small part.

### What was tricky

The least counts must not fail by chance. The seeds are fixed, so the counts are deterministic, but a rare kind (a statement cut by a crash was 18 in 1,000 seeds) cannot be asserted in a run of 50; choosing a victim that runs a statement made it common enough (47 in 50 seeds).

### What warrants review

`knownGap` in `/internal/engine/differential_test.go`, which widens what the differential comparison excuses to the two known gaps of `stmt`. It cannot excuse a text that SQLite parses, since the oracle would then not say `SyntaxError`.

The waits of the callers in `/internal/engine/simulation_test.go` change what the default workload does: fewer statements (47,010 against 95,196 in 1,000 seeds), more commits.

### Future work

None.
