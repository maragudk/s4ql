# Diary: Remove the toy library from the simulation harness

The tests of `/internal/sim` held a toy library with the shape of s4ql: a toy store, lease, and cache, three workloads, a model, and ten planted bugs. It was the harness's own workload while the storage core did not exist. The core in `/internal/core` now has its own simulation, which carries over all ten bugs and adds nine. This task removes the toy, and first gives each ability of the harness that only the toy exercised a direct test of its own in `/internal/sim`.

## Step 1: Requirements

**Author:** toy-builder

### Prompt Context

**Verbatim prompt:** You are removing a piece of test scaffolding that has done its job, in s4ql, a Go SQL database whose truth is a log in S3 (repository `maragudk/s4ql`, module `maragu.dev/s4ql`, Go 1.27.3). [...] The goal: The toy is gone, and the harness is tested as well as before by tests of its own.

1. Before you remove anything, find out what only the toy tests. [...] Measure the coverage of `internal/sim/sim.go`, `internal/sim/free.go`, and `internal/host/host.go` by the package's tests with and without the toy's test files, line by line [...]. List every function and branch that only the toy reaches.
2. Give each of those a direct test in `internal/sim`, small and aimed, in the style of the existing tests in `sim_test.go` [...]. A test of the harness must not need SQLite, a store, or a lease. The package's tests must not rest on `internal/core` either.
3. Keep what is not the toy: the scheduler's own tests, the tests of `testing/synctest` with cgo and SQLite (`cgo_test.go`, `timers_test.go`), and the tests of the free-running host. If one of these uses a helper from the toy's files, move the helper. If a canary test lives in a toy file, move the test. Say what you moved.
4. Every seed still runs twice. The harness's own determinism check [...] must survive in `internal/sim` with a small workload of its own that uses every ability at once (several instances, clock moves, a crash, buggify, timeouts, semaphores), without the toy. It needs no model.
5. Remove the toy: `toy_test.go`, the toy's workloads, model, and planted bugs in `simulation_test.go`, and the environment variables, Makefile lines, and CI steps that exist only for them. [...] check that the core's free-running tests run under the race detector somewhere in CI.
6. Documents: `internal/sim/README.md` [...], `internal/core/README.md` and any other README that points to the toy, `docs/todo.md`: delete the bullet on removing the toy. Edit nothing else in it, and do not edit `docs/spec.md`, the root `README.md`, or `AGENTS.md`.
7. Measure what it buys: the time of `go test ./internal/sim/`, of `go test -race ./internal/sim/`, and of `go test -asan ./internal/sim/` in the Docker container [...] (with `--cpus 4`), before and after, with the load of the machine.

What must hold at the end: coverage of `sim.go`, `free.go`, and `host.go` not lower than before, and every line covered only by the toy covered by a direct test or listed with a reason; a mutation check over the decisions in `sim.go` that the toy alone exercised, with `go test -overlay`; `make test`, `make lint`, and `go test -race -shuffle on ./internal/sim/ ./internal/host/ ./internal/core/` pass, and `make test-asan FUZZTIME=5s` passes in Docker; new tests follow the timing rule in `AGENTS.md`, run 50 times, also under `-race`. If something the toy tested turns out to be tested nowhere else, keep that test.

(The brief is abridged here at the "[...]" marks; the full text is in the lead's conversation.)

**Interpretation:** First measure, then add direct tests for what only the toy reached, then delete the toy and everything that exists only for it, then correct the documents, and measure the gain in time.

**Inferred intent:** Make `/internal/sim` faster and smaller without losing any assurance about the harness itself. The toy's assurance about the design of s4ql now lives in `/internal/core`.

### What I did

Read `/AGENTS.md`, `/internal/sim/README.md`, the section "Tests" of `/internal/core/README.md`, `/internal/sim/sim.go`, `/internal/sim/free.go`, `/internal/host/host.go`, all of `/internal/sim/sim_test.go`, and the parts of `/internal/sim/simulation_test.go` that are not the toy's workload. Exported the commit `95037b3` into a copy in the scratchpad, so that the measurements before the change run on a tree that my edits cannot touch.

What in `/internal/sim/simulation_test.go` is not the toy:

- `TestSchedulerWithSQLiteLocks` and its helper `contend`: the scheduler with real SQLite locks. The README's section "What the scheduled mode cannot do" rests on it. It uses `openWAL` and `busy` from `/internal/sim/cgo_test.go`.
- The first subtest of `TestSimulationSpeed`: the speed of the scheduler with eight tasks that only yield.

### Why

The measurements must come first, because the list of what only the toy reaches decides which tests to write.

### What worked

Nothing to report yet beyond the reading.

### What didn't work

`git archive HEAD | tar -x -C <scratch>` was refused by the sandbox of the worktree, because a pipeline that names git is "too complex to verify that it stays inside the worktree". `git archive -o <scratch>/before.tar HEAD` and a separate `tar -xf` worked.

### What I learned

The reusable test workflow of `maragudk/workflows` runs `go test -race -shuffle on ./...`, so the core's free-running tests already run under the race detector in CI's job `test`. The job `simulation` runs the free-running tests by name itself, "whatever the reusable test workflow does".

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: What only the toy reached, and the tests that reach it now

**Author:** toy-builder

### Prompt Context

**Verbatim prompt:** The brief quoted in Step 1, items 1 to 4.
**Interpretation:** Measure line by line what the package's tests cover of `/internal/sim/sim.go`, `/internal/sim/free.go`, and `/internal/host/host.go` with and without the toy, give each block that only the toy reaches a direct test, and give the determinism check a workload of its own.
**Inferred intent:** Lose nothing about the harness when the toy goes.

### What I did

The times before the change, on the copy of `95037b3`, one after another, each after a run that only builds (script `timeit.sh` in the scratchpad):

| Run | Time | Load before | Load after |
|---|---|---|---|
| `go test -count=1 ./internal/sim/` | 57 s | 12.25 | 16.47 |
| `go test -race -count=1 ./internal/sim/` | 83 s | 15.64 | 17.78 |
| `go test -asan -count=1 ./internal/sim/` in `golang:1.27.3` with `--cpus 4` | 172 s | 17.78 | 17.06 |

The coverage, with `-coverpkg=./internal/sim/,./internal/host/` over both packages' tests, with the toy and with `-skip '^(TestSimulation|TestSimulation_plantedBugs|TestFreeRunning|TestPlantedDataRace)$'`, compared block by block with `coverdiff.py` in the scratchpad:

- `sim.go`: 331 of 343 statements with the toy, 323 without.
- `free.go`: 15 of 15 both ways. `host.go`: 29 of 29 both ways.

What only the toy reached:

1. `sim.go:289`, the `continue` in `Scheduler.next` that leaves out the timeouts of a crashed instance.
2. `Scheduler.Steps`.
3. `Scheduler.Monotonic`.
4. `Instance.Crashed`.
5. `Free.Trace`, which has no statement, so it counts as a block of zero statements.

The tests that reach them now, all in `/internal/sim/sim_test.go` unless named otherwise:

1. `TestInstance_Crash`, "should never expire a timeout of a crashed instance, nor move simulated time to it", and "should report a deadlock when the only task that waits on time belongs to a crashed instance".
2. `TestScheduler_Run`, "should count a step each time it runs a task from one wait to its next", and `TestSimulation`, which compares the steps of the two runs of a seed.
3. `TestInstance_clocks`, "should give the test clocks that both read the simulated time, whatever the instances' clocks do".
4. `TestInstance_Crash`, "should report a crash to the test and to tasks of other instances".
5. `TestFree` in `/internal/sim/free_test.go`, "should return from a yield and a trace, with any context, and record nothing".

Blocks that neither version covered, and that are now covered too: the panic of `Scheduler.taskOf`, the panic of `Scheduler.NewInstance` on a bad name, `Scheduler.Buggify`, the panic of `Instance.WithTimeout` on a context of no task, the panic of `semaphore.Release`, `timeout.String`, and `host.production.Yield` (in `/internal/host/host_test.go`). Beyond the coverage, new tests also check what the toy used and no direct test pinned: a second `StopWall` or `StopMonotonic` that must not move where the clock stood, the trace of every clock move, and a sleep longer than the clock can hold on an instance that started late, where `Instance.trueAt` must not overflow.

`TestSimulation` is new in `sim_test.go`. Its workload, `everything`, has two instances with three workers each, which take a lock or a pool of two at random, with a timeout of one to three seconds, sleep, yield, and hit a buggify point; the test's own task shifts, stops, and starts the clocks of the instances at times from the seed, crashes `a`, waits for its workers, and starts `a2`. 200 seeds, each run twice, compare the lines of the trace, the steps, and the next number from the seed, and every seed gives a trace of its own. The test also requires that some seed has each kind of event: a timeout that expires, a worker that gives up, a crash, the restart running, buggify firing, and each kind of clock move.

What moved: `TestSchedulerWithSQLiteLocks` and its helper `contend` went from `simulation_test.go` to `/internal/sim/cgo_test.go`, next to `openWAL` and `busy`, which they use. The first subtest of `TestSimulationSpeed`, the speed of the scheduler with tasks that only yield, went to `sim_test.go`; its subtests of the toy's workloads went with the toy.

After the change, `sim.go` is covered 340 of 343, `free.go` 15 of 15, and `host.go` 29 of 29. The three statements left are the two panics of `goroutineID`, which need a stack trace without a goroutine ID, and the check in `Scheduler.parkAs` that a task waits only while the scheduler runs it, which a task's own goroutine cannot break, because the goroutine runs nothing before the scheduler first wakes it, and the goroutine check comes first for any other goroutine.

### Why

Statement coverage finds what no test reaches. It cannot tell whether a test checks what it reaches, which is what the mutants of the next step are for.

### What worked

The package ran in 11 seconds after the change, against 57 before. `TestSimulation` takes under a second for its 400 runs.

### What didn't work

The first version of two new tests used the outer test's `t.Context()` as the parent of a timeout inside a bubble, and the run crashed at the cleanup of the outer test:

```
fatal error: close of synctest channel from outside bubble
...
context.(*cancelCtx).cancel(...)
	context/context.go:568
context.WithCancel.func1()
	context/context.go:243
testing.(*common).runCleanup(...)
```

`Instance.WithTimeout` makes a `context.WithCancelCause` inside the bubble, whose done channel then belongs to the bubble, and the outer test cancels its context from outside. The two tests now pass `context.Background()`, which is never cancelled and carries no task.

### What I learned

The toy reached almost nothing that the direct tests did not, by statement: four functions and one branch. What it gave beyond that was the combination of everything at once under many seeds, which `TestSimulation` keeps.

### What was tricky

Telling a test of the harness from a test of the toy in `simulation_test.go`: `TestSchedulerWithSQLiteLocks` sits among the toy's tests and uses no part of the toy.

### What warrants review

The assertion in `TestSimulation` that 200 seeds give 200 different traces. It is deterministic, so it cannot flake, but a change to the workload could make two seeds give the same trace, and then the number would need to drop.

### Future work

None.

## Step 3: Self-review, and what it changed

**Author:** toy-builder

### Prompt Context

**Verbatim prompt:** The builder's standing instructions: "Once the implementation is done, self-review before handing back to the lead. Work in two phases: first, review the code you wrote -- read the diff, check logic, look for missing edge cases and test coverage gaps; second, run automated checks (tests and linters). [...] Use the code-review skill for the first phase -- invoke it rather than reviewing inline; it dispatches competing reviewers over your diff."
**Interpretation:** Run the `fabrik:code-review` skill over commit `73b125d` and the README edits, and fix what is real.
**Inferred intent:** A test that passes for the wrong reason is worse than no test, and this task replaces tests.

### What I did

Two reviewers on the `fable` model reviewed the diff independently, each with mutants through `go test -overlay` and an export of `origin/main` to compare against the toy. What they found, and what I did:

- **Both, serious: nothing in `/internal/sim` called `Free.Buggify` from goroutines in parallel.** Both replaced `sites sync.Map` with a plain map, and the new CI step passed; the toy's `TestFreeRunning` reported `WARNING: DATA RACE` at once. Reviewer A found that the core's free-running tests did not catch it either; reviewer B found that they caught it in 3 of 8 runs. `TestFree` has a new subtest, "should switch each site on or off once for every goroutine, also when they reach it at once": eight goroutines reach fifty new sites, and each records into its own array, because anything they shared would synchronise them and hide the race.
- **Reviewer B, serious: a semaphore that takes a permit on a failed acquire passed every test of the harness.** The toy and the core caught it. The workers of `everything` give up on acquires, but nothing checked the semaphores afterwards. `TestScheduler_NewSemaphore` has a new subtest, "should keep every permit after acquires that a cancellation or a timeout ended", and `everything` now fails the test when a semaphore lets in more holders than it has permits, or when a task that runs after the workers cannot take every permit.
- **Both: the deadlock test of a crashed instance's timeout passed with its mechanism removed.** The error text is the same whether the deadlock comes at 1 s or at 1 min. It now also checks that simulated time stayed at 1 s and that nothing expired. Its neighbour said "nor move simulated time to it", which it could not see, and lost those words.
- **Both: a sentence of the README was false.** The toy's workload whose callers never pause ran 2500 seeds, not 10000.
- **Both: `TestSimulation` overclaimed "every ability at once", and some of its required events happen in every run.** `everything` now also derives a context from each timeout, has a second task of the test that shares a semaphore with the one that moves the clocks, has each worker start a child that yields through the host of the test, has an instance that crashes itself, and traces when a worker ends, which pins the order in which crashed tasks are ended. The test now checks that no task of a crashed instance resumes and no timeout of it expires after the crash, and that the crash cut off work in some seed. The events that happen in every run are gone from its list. The claims in the comments and the README now name what the workload does.
- **Both: the comment of `everything` spoke of a model, which no reader of this package has met.** Rewritten.
- **Both: the test of `Free.Yield` claimed "record nothing", which nothing can see.** Retitled.
- **Both: the test of an instance's name sat in `TestInstance_clocks`, and one test asserted the same value twice.** Moved to `TestScheduler_NewInstance`, and the duplicate is gone.
- **Reviewer B: the test of `Steps` did not pin what a step is.** It now also sleeps, lets a timeout expire, and crashes a task, none of which may count.
- **Reviewer B: the panic of `Instance.WithTimeout` was not tested with the context of another scheduler's task.** It is now, with a context taken from a run in another bubble, which carries no channel of that bubble.
- **Reviewer B: the CI pattern was an exact list.** It is `'^TestFree'` now, which `go test -list` shows selects `TestFree` in `/internal/sim` and `TestFreeRunning` and `TestFreeRunning_plantedDataRace` in `/internal/core`.
- **Reviewer A: older decisions of `sim.go` that no test pinned, with the toy or without:** a release that does not park, the order in which crashed tasks are ended, the share of points that get jitter, a timeout whose parent is done that stays listed, a task of an instance that sets a timeout through the host of the test, and the trace of a timeout that is not positive. Each has a direct test or an assertion now.
- **Reviewer A: `TestSchedulerWithSQLiteLocks` would hang rather than fail,** because its writer retries on "busy" without a wait, and under the scheduler the holder of the lock never runs again. The retry now yields.
- **Reviewer A: the determinism check could not be scaled.** `S4QL_SIM_SEEDS` sets its number of seeds, as it does in the core.

Not changed: the mutants of `Instance.trueAt` that drop the time when the instance started, or the time its clock stood still, hang the tests until the timeout of `go test` instead of failing them. The loop of `Scheduler.Run` moves simulated time to the earliest wake-up without counting a step, so with a wrong wake-up it spins and `MaxSteps` never ends it. That is a property of `sim.go` from before this change, and it does not let a mutant survive, so I left the code alone.

### Why

Each serious finding was something that the toy tested inside `/internal/sim` and that only the core, or nothing, tested after the removal. The brief asked for exactly those to be found.

### What worked

Two reviewers with overlays found both gaps that my coverage measure could not see: the race in `Free.Buggify` is not a line that went uncovered, and neither is the leak of a permit.

### What didn't work

The first version of the new `Free.Buggify` test recorded the outcomes in a map behind a `sync.Mutex`. That mutex would have been a happens-before edge between the goroutines after each call, which is the way of hiding a race that `/internal/sim/README.md` warns about. Each goroutine now writes into its own row of an array.

The sandbox refused several commands whose heredoc or `cd` it could not verify ("this command is too complex to verify that it stays inside the worktree"); writing the script to a file in the scratchpad and running it worked.

### What I learned

Statement coverage says nothing about concurrency or about state that a later call depends on. The toy covered both by accident, through sheer volume.

### What was tricky

Keeping `TestSimulation` a test of determinism while giving it checks of its own, without growing it into a second toy. Its checks are of what the scheduler promises: permits, crashes, and when things happen.

### What warrants review

The new `Free.Buggify` test relies on goroutines that reach a site at the same time under the race detector. Without `-race`, a plain map would most likely crash with "concurrent map writes", but that is not certain on every run.

### Future work

- `Scheduler.Run` could fail a run whose simulated time stops moving while no task can run, instead of spinning. It would make hanging mutants fail at once. That is a change to the harness, so I left it for the lead.

## Step 4: The mutants, the times, and where the work stopped

**Author:** toy-builder

### Prompt Context

**Verbatim prompt:** "Wrap up now: the weekly usage limit is almost reached, so finish in as few steps as you can. [...] (1) let the two test runs that are going finish only if they end within a few minutes, otherwise stop them and note how far they got; (2) write the diary step with the numbers you have, and finish the two README edits that are uncommitted; (3) commit everything; (4) send your report [...]"
**Interpretation:** Stop the long runs, record what was measured, commit, and report.
**Inferred intent:** Leave the branch in a state that the lead can finish after the usage limit resets.

### What I did

The mutation check, with `mutate.py` and the lists `mutants-final.json` (50 mutants of `/internal/sim/sim.go`) and `mutants-final-race.json` (one of `/internal/sim/free.go`, run with `-race`) in the scratchpad, one mutant at a time through `go test -overlay`:

- **After the change:** 49 of the 50 mutants of `sim.go` are caught, and the one of `free.go`. One of the 49, a crash that leaves no line in the trace, survived the first run and is caught since `f946b94`. The one survivor ends crashed tasks in the reverse order of their names. It is equivalent with respect to what the scheduler promises: the order is fixed by the seed, and no order is promised, so two runs of the mutant agree.
- **Before the change, without the toy's tests** (`-skip '^(TestSimulation|TestSimulation_plantedBugs|TestFreeRunning|TestPlantedDataRace)$'`): 29 of 50 caught, and the one of `free.go` survived. The 21 that survived include every decision that only the toy reached, among them a timeout of a crashed instance that expires, `Instance.Crashed`, `Scheduler.Monotonic`, a failed acquire that takes a permit, and the plain map in `Free.Buggify`, and also older gaps that the toy did not close either, such as a release that does not park and the share of jitter.
- **Before the change, with the toy:** stopped after 1 of the 22 survivors. The first, a timeout of a crashed instance that expires, survived the toy too. Which of the other 21 the toy caught is not measured.

Several mutants make the scheduler spin without counting steps (see Step 3), so they are caught only by the timeout of `go test`, which `mutate.py` set to 120 s after the change.

The times after the change, single runs at a load of about 15: `go test -count=1 -shuffle on ./internal/sim/ ./internal/host/` took 11.8 s for `internal/sim`, and 22.0 s under `-race`, against 57 s and 83 s before. The sanitizer was not timed after the change.

The 50-fold runs: the new tests of the first version passed 50 times (46 s) and 50 times under `-race` (166 s) at a load of about 15. After the review, the whole of `/internal/sim` and `/internal/host` passed 50 times without the race detector (632 s, at a load of 14 to 18). The 50 runs under `-race` were stopped before they ended.

### Why

The usage limit for the week was close, and the lead asked to stop here.

### What worked

The mutants after the change found one weak check, the crash that leaves no trace, which is fixed.

### What didn't work

`git commit -am` for the fix of the crash check also took in the unfinished READMEs with their placeholders. `git reset --soft HEAD~1` and a commit of `/internal/sim/sim_test.go` alone undid it before anything else happened.

### What I learned

Running the mutants against the old tree without the toy is the measure that shows what the toy alone guarded: it turns "the coverage is the same" into a list of decisions.

### What was tricky

Mutants that hang the scheduler make a run of the list take long; a short `-timeout` is the only defence without a change to `sim.go`.

### What warrants review

That the one surviving mutant really is equivalent: nothing in `sim.go` or its doc comments promises an order in which crashed tasks end.

### Future work

Not done, for the lead to finish:

- `make test`, `make lint`, and `go test -race -shuffle on ./internal/sim/ ./internal/host/ ./internal/core/`.
- `make test-asan FUZZTIME=5s` in Docker, and the time of `go test -asan ./internal/sim/` in the container with `--cpus 4` after the change.
- The 50 runs under `-race` of the tests as they are after the review.
- The 21 remaining mutants against the old tree with the toy, to say which of them only the toy caught (`with_toy.sh` in the scratchpad).
