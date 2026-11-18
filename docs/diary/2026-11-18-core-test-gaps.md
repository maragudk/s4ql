# Diary: Close the gaps that an audit found in the tests of the storage core

An independent audit of the tests of `/internal/core` found no dishonest test and no fault in the core, but found places where a check proves less than its name says. Each was proven by planting a fault or changing a setting and seeing the tests still pass. This work closes them, in tests and test support only.

## Step 1: The brief

**Author:** builder (core test gaps)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> 1. The "everything" workload asserts nothing about what it reached (`workloads_test.go:245`, `if n >= 40`). With `fullWorkload` set to `crashes: 0, closes: 0, clocks: 0, faulty: 0, damaged: 0, buggify: false`, `TestSimulation` still passed. Assert a minimum count for each kind of event per workload from the tally, well below what is seen, so the assertion fails when a kind of fault is switched off and never by chance.
> 2. A seed that finds a planted bug is mostly never run without the bug. Rerun each seed that finds a bug with the bug unplanted (`Unplanted()`) and require no violation.
> 3. Two controls (`leaseFromAnswer`, `lostMeansFailed`) set `instances = 1`, which removes every takeover. Mark them honestly in the test and in the README table, or remove them if point 2 makes them pointless.
> 4. `TestTx_Commit_large` measures the Go heap only. Also bound the memory SQLite itself uses (`sqlite3_memory_highwater` or `sqlite3_status64`; add the binding to `internal/sqlite` with a test of its own if missing). Show that `pragma temp_store = memory` and a 4 GB `cache_size` in `conn.setUp` now fail the test, then revert.
> 5. Seven replay refusal cases (`replay_test.go:229-234`) pass without the check they name, because `replay.go` relabels any non-I/O error as `ErrLogBroken`. Assert the reason, so that removing the `checkValue` loop in `applyUpsertRows` fails the test. If the reason is not visible without changing production code, say so and propose the smallest change.
> 6. Two tests depend on the real clock: `tx_test.go:1224` (`elapsed < 8*time.Second`) and `tx_test.go:1186-1206` (busy wait exercised only if the main goroutine wins a 200 ms race). Force the order of events instead.
> 7. Small: `S4QL_SIM_SEEDS=0` passes with zero runs, and a value below 200 silently drops the "bug is found" assertion; `readOnly` (`core_test.go:563`) skips three spool-failure tests as root (the Docker route of `make test-asan`).
>
> Change tests and test support only; stop and report if production code in `internal/core` must change. Checks: `make test`, `make lint`, `make test-asan`; `-count=20`, also under `-race` and load. Update `/internal/core/README.md` and the section on the tests of the storage core in `/docs/todo.md`.

**Interpretation:** seven independent fixes to the tests of `/internal/core`, each shown to bite by planting the fault the audit used.

**Inferred intent:** every test in the core proves what its name says, so that trust in correctness rests on checks that would fail.

### What I did

Created the branch `core-test-gaps` from `origin/main` (4f77693) and read `/internal/core/workloads_test.go` and `seeds` in `/internal/core/simulation_test.go`.

### Why

The diary lets another builder take over at any step.

### What worked

Nothing to report yet.

### What didn't work

Nothing yet.

### What I learned

`seeds` already fails on a value that cannot be parsed (`is.NotError` on `strconv.Atoi`); only zero and negative values slip through.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

The seven points, one step each.
