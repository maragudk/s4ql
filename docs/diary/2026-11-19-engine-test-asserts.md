# Diary: Make the engine's tests assert what they count

A reviewer of the last round of changes to the SQL engine of slice 1 found it sound to merge, and found places where its tests assert less than they could, each shown by a planted fault. This work closes them, in tests only, on the branch `engine-test-asserts`.

## Step 1: The brief, and a plan

**Author:** engine-test-asserts builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "A reviewer of the engine's last round of changes found it sound to merge, and found places where the tests assert less than they could. Each was shown by planting a fault. Close them. This is a small task in tests only; do not change production code, and do not widen it. [...] 1. **Nothing asserts that most statements of the simulation reach a transaction.** [...] Add a ceiling on the share of statements that are refused for want of the lease, per workload, with room above what is seen. Show that the planted change now fails, then revert it. 2. **The minimums barely notice the store's faults being switched off.** [...] Count the faults that the simulated store actually injects, by kind, and assert a minimum for each kind per workload [...] 3. **`keepOpen` and `keepRead` share a signature.** [...] Assert that `keepOpen` is found by its own violation in at least some minimum number of the seeds [...] 4. **Excused texts in the engine's tests are counted and not asserted.** [...] In the full run with default flags [...] assert the counts exactly [...] 5. **The `excused` check in `internal/sql/stmt` asserts only a sum.** [...] Pin each outcome on its own. 6. **The simulation does not check a refused text against SQLite.** [...] Add the same check to the simulation's verification if it fits there simply [...] 7. **Two texts drifted from the code.** [...]"
**Interpretation:** Seven small changes to tests and two texts. The simulation's tally gains a ceiling on "failed: no lease" and minimums for the faults of the store by kind, counted from the markers in the trace; the planted-bug test counts the seeds that find `keepOpen` by "no instance runs a write"; the differential test pins its counts of excused texts in a full run; the `excused` check of `stmt` pins each of its two outcomes; the simulation's verification prepares each text that the engine refuses as not supported in SQLite on an empty database; and the README of `stmt`, the README of the engine, and `docs/todo.md` say what the code does.
**Inferred intent:** Each check fails for the fault that it is meant to catch, with a planted fault as proof where the brief asks for one.

### What I did

Read `AGENTS.md`, `/internal/engine/simulation_test.go`, `/internal/engine/differential_test.go`, the pins of `/internal/sql/parser/parser_test.go`, the `excused` check of `/internal/sql/stmt/check_test.go`, the markers of the store's faults in `/internal/core/workloads_test.go` and `/internal/store/simstore/client_test.go`, and the texts of the trace in `/internal/store/simstore/client.go`.

### Why

The markers that the core's workload test counts are the texts that `simstore` writes into the trace when it injects a fault, and the engine's simulation keeps the same trace in `simulation.lines`, so the faults can be counted there without any change to `simstore`.

### What worked

Nothing built yet.

### What didn't work

Nothing tried yet.

### What I learned

`simstore.AllFaults` switches on ten faults: lost requests and responses, hangs, conflicts, slow ways, broken reads, cut-offs, outages, late writes, and writes applied after a crash. It does not switch on `Long`. A write applied after its sender crashed needs a crash, which the hammer workload and the runs under the address sanitizer do not have.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: The seven points, the proofs, and the self-review

**Author:** engine-test-asserts builder

### Prompt Context

**Verbatim prompt:** (the same brief as step 1)
**Interpretation:** Build the plan of step 1, prove points 1 and 2 by planting the reviewer's faults, and check the result at the default seed counts, at 1,000 and 200 seeds, under `-race`, and repeated.
**Inferred intent:** Leave no check that a planted fault can pass.

### What I did

All in `/internal/engine/simulation_test.go` unless named otherwise:

1. `TestSimulation` has a ceiling `noLease` per workload on the share of statements refused for want of the lease: 0.15 for the default workload and 0.12 for the hammer workload. The first 1,000 seeds refuse 4,756 of 57,957 statements (0.08) and 7,823 of 120,000 (0.07). The tally now counts `statements`.
2. `tallyOf` takes the whole simulation and counts the faults that the store injected, by kind, from the lines of the trace that the store's request tasks write (`storeFaults`, `storeLine`): eleven kinds for the ten faults of `simstore.AllFaults`, since `Late` has two markers, a write applied after its caller gave up and a write stuck in the network. Each kind has a minimum per seed in `reached`, about a quarter of what the first 1,000 seeds reached. The hammer workload has no crash, so it has no minimum for a write applied after a crash.
3. `TestSimulation_plantedBugs` takes the first entry of `finds` as the bug's own violation and requires two seeds to find the bug by it; the loop runs until it has. Other entries are still accepted. Over 200 seeds `keepOpen` is found in 200, 195 of them by "no instance runs a write".
4. `/internal/engine/differential_test.go` counts excused texts under `excused: ` in the tally and, with the default flags and not `slow()`, pins each kind: 34 `already exists after the start of a table, accepted`, and 0 of any other kind, which covers each known gap without naming it. `pinned` and `atDefault` are copies of those in `/internal/sql/parser/parser_test.go`.
5. `excused` in `/internal/sql/stmt/check_test.go` takes the syntax errors and the texts that are not supported where SQLite does not say as two numbers, from the table of the README: 62/237, 78/178, 2,356/1,479, 2/375, and 0/1,046.
6. `statement` keeps "not supported" or "no statement" in the answer of a skipped attempt, and `run.verify` prepares each text that was not supported in SQLite on an empty database, through `syntaxOfNotSupported`, which `pair.step` now uses too.
7. `/internal/sql/stmt/README.md` says that the simulation compares a statement that saw no state with SQLite on an empty database and does not use `StartedTable`, and that each column of the excused texts is pinned on its own. `/internal/engine/README.md` says "from a fifth to a third", and describes the ceiling, the faults, the new check of `verify`, the pins of the differential test, and the own violation of each planted bug.

### Why

Each point closes a check that a planted fault passed, as the brief lists them.

### What worked

The proofs, each planted, run, and reverted:

- Point 1: `waitServing` made to return `true` at once. `go test ./internal/engine -run '^TestSimulation$'` failed with "50 seeds refuse 2958 of 4894 statements for want of the lease, 0.60 of them, and at most 0.15 were expected" and, for the hammer workload, "1910 of 6000 ... 0.32 ... at most 0.12". With `S4QL_ENGINE_SIM_SEEDS=1000`: 58,615 of 96,932 (0.60) and 36,645 of 120,000 (0.31).
- Point 2: `faults: simstore.AllFaults()` removed from `defaultWorkload`. The default run failed on every kind of fault at 0, such as `50 seeds give 0 of "fault: slow way", and at least 40 were expected`, beside the old `failed: timeout` at 11 of 12.
- Point 3: the violation "no instance runs a write" switched off with `if false &&`. `keepOpen` then failed: "2 of 100 seeds find the bug, 0 of them by \"no instance runs a write\"".
- Point 4: the pin set to 33 failed with "texts excused from the comparison with SQLite as already exists after the start of a table, accepted: 34, but the test pins 33".
- Point 5: 63/236 for the corpus failed on both numbers, where the sum alone would have passed.
- Point 6: `prepare` in `/internal/engine/engine.go` made to turn a syntax error into `stmt.ErrNotSupported`. `TestSimulation` failed in both workloads, such as `the engine does not support the statement 39, "select * frm outer", and SQLite gives near "frm": syntax error`.

`S4QL_ENGINE_SIM_SEEDS` at 50, 100, 200, and 1,000 passes `TestSimulation`; 200 passes `TestSimulation_plantedBugs`.

### What didn't work

A compound shell command with a heredoc into `python3` that also ran `gofmt` and `go test` was refused by the worktree guard: "this command is too complex to verify that it stays inside the worktree". Split commands and the Edit tool worked.

### What I learned

- The store writes a stuck write's later trace lines from a task named `store request N, stuck`, so a pattern for the store's lines must accept that suffix; the code review found it. With it, the conflicts of the first 1,000 seeds rise from 266 to 268 and from 862 to 867.
- With the check "no instance runs a write" off, `keepOpen` is found by the read connections in only 2 of 100 seeds, so its own violation is nearly all of how it is found.
- `/docs/todo.md:70`, which the brief names, is in "Tests of the storage core" and describes the minimums of the core's `TestSimulation` in `/internal/core/workloads_test.go`, which are a quarter by that file's own rule. It is true, so I left it.

### What was tricky

`TestSimulation_plantedBugs` lists `skipUnique` and `checkOutside` with the same violation, so the own violation is the first of the bug's list, not one that no other bug causes.

### What warrants review

- The minimums for the faults in `TestSimulation`, against the counts that the test logs at 1,000 seeds.
- The review asked to export the markers of the faults from `simstore`, which the core's tests copy too; that is production code, out of this task, and left. It also called the strings "not supported" and "no statement" in the answer fragile; they follow the convention of `answer.err` in the differential test, and stayed. Its point that the scan of every trace line costs time did not show: 1,000 seeds take about 60 s before and after.

### Future work

- `/internal/engine/README.md` says the fuzz tests "count them apart", the known gaps; they pass a nil tally and count nothing.
- The markers of the store's faults are in three test files; `simstore` could export them.
