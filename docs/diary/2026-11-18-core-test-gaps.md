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

## Step 2: SQLite's own memory in `TestTx_Commit_large` (point 4)

**Author:** builder (core test gaps)

### Prompt Context

**Verbatim prompt:** see step 1, point 4.
**Interpretation:** bound the memory that SQLite allocates, through its own counter, next to the Go heap.
**Inferred intent:** the name "never holds a large transaction in memory" is true of the whole process, not only of Go.

### What I did

Added `sqlite.MemoryUsed(reset bool) (used, highwater int64)` over `sqlite3_status64(SQLITE_STATUS_MEMORY_USED)` in `/internal/sqlite/sqlite.go`, and the same in `/internal/sqlite/sqlite_browser.go` (the browser binding must keep the API, because `make check-browser` compiles the core's tests for js/wasm; the official build exports `sqlite3_status64`, and the two 64-bit outputs are read as 32-bit halves with `peek32`). A `TestMemoryUsed` in both test files. In `/internal/core/tx_test.go`, `heapWatch` became `memoryWatch`, which resets SQLite's high-water mark at its start; the test asserts the growth below `sqliteLimit` (8 MiB) for the commit and for the replay, also under the race detector and the sanitizer.

### Why

The Go heap does not see SQLite's page cache or in-memory temporary files.

### What worked

Measured on 2026-11-18: SQLite grew by 5,102,656 bytes while the transaction was written, both with 300,000 and 1,000,000 rows per table (`S4QL_CORE_ROWS=1000000`), and by 2,956,352 bytes during the replay: a fixed amount, the default page caches. With `pragma temp_store = memory` and `pragma cache_size = -4000000` planted after `pragma synchronous` in `conn.setUp` (`/internal/core/cache.go`), it grew by 24,226,880 and 18,697,280 bytes, and the test failed: `the memory of SQLite grew by 24226880 bytes for a commit object of 23389389 bytes`. The plant was reverted. In headless Chromium (`make test-browser`), 4,316,032 and 3,119,016.

### What didn't work

My first combined shell command was refused by the worktree guard ("this command names git in a form too complex to verify"); git commands have to run alone.

### What I learned

SQLite's counter is process-wide, so it is fair only because `TestTx_Commit_large` is not parallel, and Go runs no parallel top-level test beside a sequential one. The comment of `watchMemory` says so.

### What was tricky

Nothing else.

### What warrants review

That an 8 MiB bound against a fixed 5.1 MB is enough room, and that asserting it under the sanitizer and race detector (30,000 rows) is right: there it cannot catch the plant, but it cannot fail by chance either.

### Future work

None.

## Step 3: The reason of each replay refusal (point 5)

**Author:** builder (core test gaps)

### Prompt Context

**Verbatim prompt:** see step 1, point 5.
**Interpretation:** each refusal case in `TestReplay` must fail when the check that its name points to is gone.
**Inferred intent:** no case passes because some other layer happens to refuse the same input.

### What I did

The map of cases in `/internal/core/replay_test.go` now holds `changes` and `reason`, and each case asserts that the error holds `the log is broken: ` + reason. All 25 cases, not only the seven.

### Why

`replay.go` wraps every non-I/O error as `ErrLogBroken`, so the kind of error says nothing about which check refused. The reason is in the text of the error already, so no production change was needed.

### What worked

With the `checkValue` loop of `applyUpsertRows` removed, six cases failed (text, real, and blob into an integer column, text and NULL into the primary key, and text into a row that is there); the message showed the binding's refusal, `invalid: a value of kind real cannot be stored`. With the `checkKey` call of the delete removed, four delete cases failed. Both reverted.

### What didn't work

My first reason for the case of 2001 columns, `too many columns on t3`, missed the `sqlite: ` prefix; the case of the reserved table `s4ql_cache` is refused as `which does not exist`, because `lookUp` does not see the cache's own table.

### What I learned

The case "creates a table with more columns than the cache can hold" has no check of the core: SQLite's own refusal is the reason, and the case now says so.

### What was tricky

Nothing.

### What warrants review

The reasons are full sentences of the error, so a rewording of a message in `replay.go` or `cache.go` fails these cases. That is the price of naming the check.

### Future work

None.

## Step 4: `TestDB_Begin` without a race against the clock (point 6)

**Author:** builder (core test gaps)

### Prompt Context

**Verbatim prompt:** see step 1, point 6.
**Interpretation:** the wait for the lock must be reached on every machine, and the default timeout shown with no bound from above on real time.
**Inferred intent:** follow the rule in `/AGENTS.md`: no test depends on timing.

### What I did

`/internal/sqlite/faultvfs` counts the calls of `xSleep` and the microseconds they ask for (`VFS.Slept`), in the C VFS and in `/internal/sqlite/browser/vfs.js` with `/internal/sqlite/faultvfs/faultvfs_browser.go`, with `TestVFS_Slept`. In `/internal/core/tx_test.go`, `sleepCounter` opens the cache through such a VFS. The first test commits in a goroutine and releases the other connection's lock only once `Slept` reports a sleep, which SQLite's busy handler does only after it found the lock held. The second asserts that the handler asked to sleep exactly one second in all, and that at least a second passed.

### Why

SQLite's default busy handler (`sqliteDefaultBusyCallback`) sleeps through the connection's VFS, and the delays it asks for sum to the busy timeout exactly, in both of its variants. So the count is deterministic, and a slow machine can only make the real time longer, which the lower bound allows.

### What worked

Passed natively, under `-race`, and in headless Chromium, where `xSleep` yields to Go so the polling goroutine can release the lock.

### What didn't work

`make test-browser BROWSERFLAGS='-run A|B'` failed with `/bin/sh: TestDB_Begin: command not found`; the pattern needs inner quotes.

### What I learned

The polling loop (`for sleeps == 0 { time.Sleep(time.Millisecond) }`) waits for a condition, not for a time, so it is allowed by the rule.

### What was tricky

The VFS must close after the instance: the cleanup of `sleepCounter` is registered before `w.serve`, so it runs after the instance's.

### What warrants review

That another connection of the instance cannot sleep first: the readers are idle while the lock is held, and nothing else in the instance writes the cache on the real clock in the time of the test.

### Future work

None.

## Step 5: What the workloads reach, clean reruns, and the two controls (points 1, 2, 3, 7a)

**Author:** builder (core test gaps)

### Prompt Context

**Verbatim prompt:** see step 1, points 1, 2, 3, and the first part of 7.
**Interpretation:** each workload of `TestSimulation` asserts minimums of what it reached; each seed that finds a planted bug is run without it; the two one-instance controls go or are marked; `S4QL_SIM_SEEDS` of 0 fails, and a dropped assertion shows.
**Inferred intent:** a fault kind that is switched off, or a bug attributed by a loose substring, cannot pass unnoticed.

### What I did

`tally.reached` names every count of the tally, every marker of the traces, and every event as `event <kind>`. `singleReaches`, `fullReaches`, `hammerReaches`, and `lateReaches` in `/internal/core/workloads_test.go` hold a quarter of what the default seeds reached on 2026-11-18, for each kind reached four times at least, generated from a logged run and written into the code; `singleReaches` keeps `failed commits: 1`, which the old assertion had. They are asserted when the run has at least the default seeds, because seeds 0 to n-1 of a larger run include the default ones; otherwise the test logs `NOT CHECKED`. The check that no two seeds give the same trace now holds for every workload. In `TestSimulation_plantedBugs`, the clean workload runs the first 10 seeds and every finding seed above them, and a counting run of fewer than 200 seeds logs `NOT CHECKED`. `seeds` fails for a value below 1. The controls of `leaseFromAnswer` and `lostMeansFailed` are removed: with one instance there is no takeover, so they could not find their bug, and the clean rerun of every finding seed is the control that shows the workload finds nothing else.

### Why

A run depends only on the code and its seed (the hash of all traces was the same under `-race`), so a minimum fails only when the code changes, never by chance.

### What worked

With `fullWorkload` set to `damaged: 0, clocks: 0, faulty: 0, crashes: 0, closes: 0, buggify: false`, the subtest of the full workload failed 14 minimums, among them `the workload reached "crashes" 0 times, and must reach it 7 times at least`, `"moves of a clock" 0 ... 124`, `"buggify points that fired" 0 ... 19`, `"restarts that recovered from WAL" 0 ... 5`, `"event stop/local_failure" 0 ... 4`, and `"damaged downloads" 0 ... 1`. Reverted. `S4QL_SIM_SEEDS=0` fails `TestSimulation`, `TestSimulation_plantedBugs`, and `TestFreeRunning` with `S4QL_SIM_SEEDS is 0, and must be 1 or more`; `abc` failed before and still does. The planted bugs run took 53 s alone, and no clean rerun found anything.

### What didn't work

A Python heredoc that held a Go function literal with `yield` was refused by the worktree guard as "too complex"; the edit went through the Edit tool instead.

### What I learned

`TestFreeRunning` also calls `seeds`, so it gets the check of zero too.

### What was tricky

Choosing the minimums: they had to fail when a fault kind is off and never otherwise. A quarter of a deterministic count meets both, and the kinds seen fewer than four times are left out, because a minimum of 1 for a count of 1 would fail on any harmless change.

### What warrants review

That dropping the two controls is the right call (the alternative was to keep and label them); the README table now says "no control, see below" for both.

### Future work

The minimums are a snapshot; `/docs/todo.md` says how to set them again.

## Step 6: A spool that fails as root (point 7b)

**Author:** builder (core test gaps)

### Prompt Context

**Verbatim prompt:** see step 1, point 7, second part.
**Interpretation:** the tests of a spool file that cannot be made must run as root, or fail loudly.
**Inferred intent:** `make test-asan` in Docker runs as root and must not skip them in silence.

### What I did

`readOnly` in `/internal/core/core_test.go` became `moveAway`: it renames the directory of the cache to `<dir>-away`, checks that a file cannot be made by the old path, and renames it back by the returned function or at the end of the test. The open files of SQLite stay open by their descriptors. The first test moves it back before `d.Close`, so that the last connection deletes its write-ahead log by its path and `leftOpen` finds none.

### Why

Permissions do not bind root; a missing directory binds everyone. I chose this over failing loudly, because failing would break `make test-asan` in Docker.

### What worked

Passed natively and in headless Chromium (where the old version would have skipped, as the browser's file system does not enforce permissions). Two call sites use it; the brief counted three tests, and I found two.

### What didn't work

Nothing.

### What I learned

The stop for a local failure does not close the connections; `Close` does, so the rename can stay in place until then.

### What was tricky

The order of cleanups: `moveAway` is registered after `w.serve`, so it moves the directory back before the instance closes.

### What warrants review

That nothing in the instance opens a new file in the directory of the cache between the move and the move back.

### Future work

None.

## Step 7: Self-review and the checks

**Author:** builder (core test gaps)

### Prompt Context

**Verbatim prompt:** see step 1, "Checks before you report".
**Interpretation:** review the diff, and run `make test`, `make lint`, `make test-asan`, and the changed tests many times, under `-race`, and under load.
**Inferred intent:** hand in only what holds on every machine.

### What I did

Ran the `code-review` skill at medium over the branch. It made ten findings. I fixed four. The first was that the polling loop of `TestDB_Begin` could spin until the timeout of `go test` if the transaction ended without a sleep, so it now selects on the result and fails at once. The second was that the test of the default timeout did not check that nothing had slept before the timed wait. The third was that the minimums of a workload were hard to set again, so a run that falls short now logs the minimums from its own counts as Go map entries. The fourth was that the comment with the rule of the minimums was floating, and it now heads a `var` group. I left six. The memory check during the replay is not disturbed by the first instance, because `d` is closed before the replay is watched. `TestMemoryUsed` is copied into both test files of `/internal/sqlite`, because each of them carries a build tag, and the package has no untagged test file. The probe of `moveAway` proves only the rename, and that is what it is for. The reason of the case of 2001 columns is SQLite's own text, because that is the check. The other two were small.

### Why

The two findings about `TestDB_Begin` were real: a regression would have hung the test, not failed it.

### What worked

`make test` passed (the core in 94.6 s, coverage 92.7%). `make lint` passed. `make test-asan` passed in Docker, the core in 241 s. A Docker run of `id -u` printed 0, and there the two spool tests PASS instead of skipping. With `-count=20` and `-shuffle on`, `TestDB_Begin`, `TestDB_stop`, and `TestReplay` passed in 44 s, and in 365 s under `-race`. `TestMemoryUsed` and `TestVFS_Slept` passed 20 times each with and without `-race`. Everything ran at a load of 25 to 35 from other agents. The traces of `TestSimulation` had the same four hashes under `-race` as without it.

### What didn't work

The first `make test-asan` (with `FUZZTIME=5s`) failed. Three packages timed out after 10 minutes: `internal/core` in `TestSimulation_plantedBugs`, and also `internal/sim` and `internal/sql/shapetest`, which this branch does not touch. My timing runs and a load of 35 were on the machine at the same time. Run alone in Docker, the core passed under `-asan` in 175 s, and the second full `make test-asan` passed.

### What I learned

The default run of the core changed by little in work: the clean reruns of the seeds that found a bug above seed 9 add 22 runs, and the two controls that were removed take away 20. The wall clock of the test binary, measured in turns with one built from `origin/main`, was 78.7 s before against 83.8 s after, and 109.9 s against 132.9 s, at a load of 25 to 35. At that load the numbers mostly show noise.

### What was tricky

Measuring time on a machine that other agents load.

### What warrants review

The choices in step 5 (dropping the two controls) and step 6 (rename instead of a loud failure).

### Future work

None beyond `/docs/todo.md`.
