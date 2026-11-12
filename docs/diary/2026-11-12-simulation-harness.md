# Diary: simulation harness

Turn the spike in `/internal/sim` into the simulation harness that the storage core is built on. The spike showed on 2026-11-10 that plain blocking Go code can run one goroutine at a time under a seed, inside a `testing/synctest` bubble, and that a seed replays a run exactly (`/docs/diary/2026-11-10-simulation-spike.md`). The design was adopted as the working approach (`/docs/todo.md`, "Concurrency and the simulation harness"). The harness is the first of four pieces of the storage core without SQL: the harness, the storage formats, the object store, and the core itself.

## Step 1: Requirements and scope

**Author:** main

### Prompt Context

**Verbatim prompt:** Okay, what's next?
**Interpretation:** The three design questions that came before the storage core are settled (the lease and its clocks, a manifest swap with an unknown outcome, and the storage formats). Markus asks what to build now. I proposed the four pieces, and after a short exchange about the weekly usage limit I started the harness and the formats in parallel, each in its own worktree.
**Inferred intent:** Start building the storage core on what was agreed, without waiting for him on anything that is not a question of design.

### What I did

I read the spike's diary, the outline of `/internal/sim/sim.go`, the todo item, `/.github/workflows/ci.yml`, and the `Makefile`. I created the worktree `.claude/worktrees/sim-harness` on branch `worktree-sim-harness`, wrote this entry, and briefed one builder on Opus.

The requirements handed to the builder:

1. **A runtime interface for library code.** One small interface for everything that waits, in a package that library code imports, with a production implementation that maps each method onto ordinary Go. It covers: starting a goroutine and waiting for it, locks, sleeping, the clock, timeouts, yielding, and random numbers. The name of the package is the builder's choice, but not `runtime`.
   - **Two clocks.** The lease is checked against the wall clock and the monotonic clock, so the runtime gives both as separate readings, and a test can move one without the other.
   - **Timeouts.** `context.WithTimeout` does not work in simulated time, so the runtime offers its own, which every wait of the runtime honours.
   - **Random numbers.** In production they must be unpredictable enough that two processes never choose the same lease holder ID.
2. **The scheduler implements it,** and so does the free-running runtime.
3. **More than one instance in a run.** The scheduler hands out a runtime per instance. Each has its own wall clock, which the test can set forward and backward during the run, and its own monotonic clock, which the test can hold still.
4. **Crash and restart.** A test can crash an instance: every task of it ends at its next wait and never runs library code again, and nothing of it reaches the object store afterwards. What a crash in the simulation does differently from a real crash is written down.
5. **Buggify.** A method of the runtime that names a point in the code and returns whether to do the unusual thing there. It is always false in production. In simulation it follows FoundationDB's three gates (`/docs/research/foundationdb.md`, "Implementation today"): buggify is on or off for the run, each point is switched on once per run with probability 0.25, and a point that is on fires with probability 0.25 each time. The probabilities are options. Every decision comes from the seed and appears in the trace.
6. **A lint for library code.** It forbids `go` statements; the `time` package's clock, sleeps, timers, and tickers; `context.WithTimeout`, `context.WithDeadline`, and their `Cause` variants; `sync.Cond` and `sync.WaitGroup`; and `math/rand`, `math/rand/v2`, and `crypto/rand`. It covers every Go file that is not a test in the module, except the packages on an explicit list in the lint, such as the harness and the production runtime. It runs in `make lint` and in CI. It has a test with a planted violation for each rule.
7. **The conditions from the spike hold in CI, permanently:** every seed is run twice and the traces are compared; the free-running mode runs under the race detector; the tests of `testing/synctest` with cgo stay.
8. **The toy library stays as the harness's own workload,** in test code, until the core replaces it. It is extended to show the two things the spike did not: two instances that compete for the lease, with clocks that the test moves, and a crash followed by a restart. The planted bugs stay as tests of the checks, and each new ability gets at least one planted bug that only it can find.
9. **The package is no longer a spike.** Its documentation says what it is and lists the rules for code that runs under it.

Out of scope: the object store interface and the simulated store as packages (the toy keeps its own store in test code), the storage formats, the core, and real S3. The builder does not edit `/docs/spec.md`, `/docs/todo.md`, `/README.md`, or `/AGENTS.md`, and reports instead what in them is now out of date.

No telemetry: this is a test harness and a small interface, and the trace of a run is its own record.

How the goal is verified: `make test` and `make lint` pass; `go test -race` passes on the harness; with `S4QL_SIM_SEEDS=10000`, every seed run twice gives no pair of traces that differ, with several instances, crashes, and buggify on; every planted bug is found, and the count of seeds that find it is reported; the lint's own test fails for each planted violation.

### Why

The core is plain blocking Go that is only trustworthy if the simulation can run all of it under a seed. The spike left open the three abilities that the core's hardest cases need: two instances that compete for a lease, a crash at any point, and code that does something legal but unusual on purpose. The core should not be the first user of an unproven ability.

### What worked

The spike's diary states its conditions and its open points precisely, so the requirements could be taken from it almost directly.

### What didn't work

I first told Markus that the harness, the formats, and the object store could be built in parallel. The simulated store waits through the runtime interface, which this piece defines, so the store has to follow the harness. I corrected that before any builder started.

### What I learned

The lease design needs the harness to tell the wall clock from the monotonic clock, and to give each instance its own. The spike's scheduler has one clock for the whole run.

### What was tricky

Deciding how much of the two-instance and crash work belongs here and not in the core. I put the abilities here and their proof in the toy, so that the core starts on something shown to work.

### What warrants review

Whether crash and restart can be simulated faithfully enough. A task that is ended runs its deferred calls, and a real crash runs none.

### Future work

The object store interface and the simulated store follow this piece, and then the core. `/docs/todo.md` loses what this piece builds.

## Step 2: The host interface and the scheduler behind it

**Author:** harness-builder

### Prompt Context

**Verbatim prompt:**

> You are building the simulation harness of s4ql, a Go SQL database whose truth is a log in S3, with a local SQLite cache. The repository is `maragudk/s4ql`, module `maragu.dev/s4ql`.
>
> **Your worktree is `/Users/maragubot/Developer/s4ql/.claude/worktrees/sim-harness`, on branch `worktree-sim-harness`.** Run `pwd` and `git branch --show-current` first. If you are somewhere else, switch into that worktree before you write anything. Never work in `/Users/maragubot/Developer/s4ql` itself. Another builder works at the same time in a different worktree on a different feature (the storage formats); you share nothing with it.
>
> ## The goal
>
> Turn the research spike in `internal/sim` into the simulation harness that the storage core is built on. The spike is merged and works: a seeded scheduler that runs plain blocking Go code one goroutine at a time inside a `testing/synctest` bubble, a free-running runtime with the same methods, and a toy library in test code with planted bugs.
>
> ## Read first
>
> - `AGENTS.md`: how to work in this repository.
> - `docs/diary/2026-11-12-simulation-harness.md`, Step 1: **the requirements, numbered 1 to 9, the scope, and how the goal is verified. They are binding; read them completely.**
> - `docs/diary/2026-11-10-simulation-spike.md`: what the spike found, especially Step 5.
> - `docs/todo.md`, the items "Concurrency and the simulation harness", "The lease and its clocks", "A manifest swap whose outcome is unknown", and "Local I/O failure handling": what the core that will run on your harness has to do. You do not build the core, but the harness must be able to test it.
> - `docs/spec.md`, the sections "Testing" and "Concurrency".
> - `docs/research/foundationdb.md`, the part on buggify.
> - `internal/sim/`: the code you start from.
>
> ## What I want beyond the diary's list
>
> - Invoke the `fabrik:go` skill before you read or write Go code, and follow it.
> - Decide implementation details yourself and say in your report what you chose: the name and shape of the runtime package and interface, how the lint is built (a test over the syntax tree, a `go/analysis` analyzer, or something else), and how a crash is simulated.
> - Keep the interface small. It is the only thing the core will see of your work, so every method needs a reason.
> - The goroutine check of the spike (`goroutineID`) stays switched on, unless you measure that it matters and the lint covers what it covers. If you make it optional, it is on by default.
> - The checks of the toy compare data against a model, not counters. Keep that for everything you add.
> - Check whether the reusable workflow `maragudk/workflows/.github/workflows/test.yml@main`, which `.github/workflows/ci.yml` uses, runs the tests under the race detector. If not, add what is needed so that the free-running mode runs under it in CI.
> - Do not edit `docs/spec.md`, `docs/todo.md`, `README.md`, or `AGENTS.md`. Report what in them is out of date after your work.
> - No telemetry.
>
> ## How to work
>
> - Keep the diary: add a step to `docs/diary/2026-11-12-simulation-harness.md` for each meaningful chunk of work, with the `fabrik:diary` skill, under your name `harness-builder`. Record failures with the exact commands and errors, and record the numbers that verify the goal.
> - Commit as you go, on the branch, with the `fabrik:git` skill's conventions. Do not ask about GitHub issues; there are none to reference. No attribution lines in commit messages. Do not push and do not open a pull request; I do that.
> - When the implementation is done, self-review it as your standing instructions say, fix what is real, and run `make test`, `make lint`, and the race run again.
> - You cannot ask me anything while you run. If you hit a question of scope or design that you cannot settle from the documents, stop and put the question in your report; I will answer and continue you. Questions of implementation you settle yourself.
>
> ## Your report
>
> It is read by me, not by Markus. Give me: what you built and what you chose, the numbers from the verification, what the harness still cannot do, what a builder of the object store and of the core must know to use it, and what is now out of date in `docs/todo.md`.

**Interpretation:** Requirements 1 to 5 of Step 1: a small interface that library code waits through, implemented for production, by the scheduler, and by the free-running runtime; instances with their own clocks; crash; buggify.
**Inferred intent:** Give the core one seam to the world that the simulation can take over completely, so that the core is never the first user of an unproven ability.

### What I did

I wrote `/internal/host/host.go` with `host.Host` and the production implementation `host.New`, and its tests in `/internal/host/host_test.go`. The interface has nine methods: `Go`, `NewSemaphore`, `Sleep`, `WithTimeout`, `Yield`, `Wall`, `Monotonic`, `Uint64`, and `Buggify`, with `host.Task` (`Wait`) and `host.Semaphore` (`Acquire`, `Release`). Production maps them onto a goroutine and a channel, a buffered channel, a timer, `context.WithTimeout`, nothing, `time.Now().Round(0)`, `time.Since` a start time, the generator of `math/rand/v2`, which the Go runtime seeds from the operating system in every process, and `false`.

I rewrote `/internal/sim/sim.go`. `*sim.Scheduler` implements `host.Host` as the host of the test itself, and `Scheduler.NewInstance(name)` returns a `*sim.Instance` for each simulated process, which implements `host.Host` too and adds `ShiftWall`, `StopMonotonic`, `StartMonotonic`, `Crash`, `Crashed`, and `Trace`. The free-running runtime moved to `/internal/sim/free.go`. Tests are in `/internal/sim/sim_test.go` and `/internal/sim/free_test.go`.

### Why

Each method of the interface has one reason. `Go`, because the lease task is the library's one goroutine. `NewSemaphore`, because the writer lock, the manifest lock, and the reader pool are a lock, a lock, and a semaphore. `Sleep`, for the renewal and for backoff. `WithTimeout`, because `context.WithTimeout` is a timer of the bubble's clock, which stands still. `Yield`, for the windows between waits. `Wall` and `Monotonic`, because the lease is checked against both, separately. `Uint64`, for the lease holder ID and every random choice. `Buggify`, for requirement 5. I left `Trace` out: library code needs nothing from a trace, and the simulated store, which is simulation code, gets it from the concrete type.

How the pieces work:

- **Clocks.** Every instance has a wall clock (simulated time plus an offset that `ShiftWall` moves) and a monotonic clock (simulated time since the instance started, minus the time it stood still). Sleeps and timeouts through an instance run on its monotonic clock, as Go's timers run on the runtime's monotonic clock, so they stand still with it. Yields, releases, and the start of a task get their jitter on simulated time, so a stopped clock does not freeze an instance that is not sleeping.
- **Timeouts.** `WithTimeout` returns a context of its own type. Its error after the timeout is `context.DeadlineExceeded`, as in production, also for contexts derived from it: it has its own done channel and an `AfterFunc` method, so the context package attaches children through that method, which hands them the right error. Built on `context.WithCancelCause` alone, it would report `context.Canceled`, with the deadline only as its cause. The scheduler ends timeouts between steps, in the order they were made, so that two that are due at the same instant cannot race.
- **Crash.** `Instance.Crash` marks the instance. Its tasks are never picked again and stay parked. When every task that did not crash has returned, the scheduler wakes the crashed ones one at a time, and each ends with `runtime.Goexit` inside its wait; a wait in a deferred call then ends it at once. A task of a crashed instance counts as returned for `Task.Wait`.
- **Membership.** A task waits only through the host of its own instance or through the scheduler, and uses a semaphore only through the host that made it. A crashed task could otherwise hold a permit that a live instance waits for.
- **Buggify.** `Options.Buggify` switches it on with FoundationDB's gates: each site is switched on or off with probability 0.25 the first time it is reached, and a site that is on fires with probability 0.25. Both probabilities are options. The decision for a site holds for the run, in every instance. With buggify off, no number is drawn, so the rest of the run is the same as without the calls.
- **Free.** `sim.NewFree` takes the buggify options and no seed. Its random numbers come from `math/rand/v2`, and its `Trace` does nothing, because a seeded generator or a trace behind a mutex is a happens-before edge between every pair of goroutines that use it, which hides data races from the race detector. Free has no instances: a test uses the one host for every process, and moves no clocks and crashes nothing.

### What worked

The scheduler's tests passed after the fixes below, including the crash test over 50 seeds: after the crash no task of the instance runs again, and its deferred calls run after the main task has returned. Buggify over 200 seeds switched on 207 of 800 sites and fired 5196 times in 20700 visits of a site that was on, which is a quarter of each.

### What didn't work

My first `next` treated every task in the map as parked, so the test of a task that blocks on a raw channel hung in the bubble:

```
goroutine 3751 [chan send (durable), synctest bubble 556]:
maragu.dev/s4ql/internal/sim.(*Scheduler).Run(0x41a9f58fa0c0, 0x100c3a510?)
	/Users/maragubot/Developer/s4ql/.claude/worktrees/sim-harness/internal/sim/sim.go:209 +0xa8
```

The failed run tried to wake the blocked task for unwinding, and it never receives. `next` again collects only parked tasks.

One test assumed that the order of the "resumes from start" lines gives the order in which the timeouts were made, and counted `main` among them: `Expected "timeout of main after 1s expires", but got "timeout of waiter-0 after 1s expires"`. The test now looks only at the waiters.

`golangci-lint run ./internal/sim/` reported `SA1029: should not use built-in type string as key for value` in a test, which uses a key type now.

### What I learned

- The context package attaches a derived context to a parent of a foreign type through an `AfterFunc` method if the parent has one, and otherwise starts a goroutine that waits on the parent's done channel. That goroutine would run outside the scheduler, so the method is what makes a custom context safe under it.
- A seeded generator in the free-running runtime is not harmless: the mutex around it synchronises every goroutine that draws, in the eyes of the race detector.

### What was tricky

Deciding which clock each kind of wait follows. Sleeps follow the instance's monotonic clock, because that is Go's behaviour, and because it is what makes a stopped clock dangerous: the renewal stalls, and the lease runs out in real time. Network latency in the simulated store follows simulated time, because it is not the instance's.

### What warrants review

- `/internal/sim/sim.go`, `Scheduler.next` and `Scheduler.parkAs`: the order of the checks, the end of a run when only crashed tasks are left, and that nothing depends on the order of goroutines.
- `/internal/sim/sim.go`, the `timeout` type: whether its error and its children's behave as those of `context.WithTimeout`. A context derived from it learns that the parent of the timeout was cancelled at the next scheduling point, not at once. I accepted that.

### Future work

None beyond the steps that follow.

## Step 3: The toy, with two instances, clocks, crashes, and buggify

**Author:** harness-builder

### Prompt Context

**Verbatim prompt:** The brief quoted in Step 2.
**Interpretation:** Requirement 8: extend the toy to show two instances that compete for the lease, with clocks that the test moves, and a crash followed by a restart, with a planted bug for each new ability that only that ability finds, and checks that compare data against a model.
**Inferred intent:** Prove each ability on a library with s4ql's shape before the core relies on it.

### What I did

`/internal/sim/toy_test.go` now follows the agreed design where an ability needs it. A lease task takes the lease if it is free or has expired by the instance's own wall clock plus a margin of two seconds, catches up the cache from the log (walking back from the manifest's head to the commit that the cache has), and only then serves. A read checks the lease against both clocks. A local failure after the manifest swap stops the instance, and `Commit` still returns nil. Each instance reaches the store through its own client, which a partition can cut off. The toy has two buggify points: the cache fails to commit after the swap (after a rollback or after the commit, at random), and the lease is renewed early.

`/internal/sim/simulation_test.go` has three workloads: one instance (the spike's), the full workload (two instances, a clock fault for each instance as often as not, a crash and a restart, buggify, and store faults), and callers that never pause. The model is per attempt: each commit is recorded with its writes before `Commit`, then marked acknowledged, failed, or unknown, and stays pending if its instance crashed. At the end, the log must hold every acknowledged commit with its writes and no failed one, and every cache must equal the log up to its own commit. A supervisor task per instance closes and reopens a stopped database, as an application would. A crash copies the cache and its write-ahead log, without the shared-memory index, to a new path at the moment of the crash, and starts a new instance on the copy.

Clock faults are limited to what the design claims to survive: at most one clock wrong per instance at a time, and a wall clock set backward only while the instance is cut off from the store. A holder that renews with a wall clock behind the others writes an expiry that is already past for them, which is outside the design ("the wall clocks of the instances must therefore agree").

The planted bugs for the new abilities, each with a control that runs the same workload without the ability:

- `wallOnly`, a read that checks only the wall clock, needs the wall clock set backward.
- `monotonicOnly`, a read that checks only the monotonic clock, needs the monotonic clock stopped.
- `takeoverAfterRelease`, an instance that takes over a lease only after a release, needs a crash. A new check finds it: some instance must serve within two minutes of the end of the workload.
- `ignoreCacheError`, a commit that goes on serving after the cache failed, needs buggify.

### Why

A bug that only one ability can produce is the proof that the ability works and that a check looks at it. The control shows the "only".

### What worked

With 200 seeds and no planted bug, the three workloads gave no differing pair and no violation, and every planted bug was found: in 30, 27, 26, 38, 198, and 33 of 200 seeds, and in 0 of 200 in every control. The trace of `wallOnly`, seed 4, shows the mechanism: `a` is cut off and its wall clock is set back by 1m17.526s, `b` takes over the expired lease at 1m42.5s and acknowledges commit 8, and at 1m53.3s `a/caller-0 VIOLATION: a read saw commit 7 after commit 8 was acknowledged`.

### What didn't work

The first run of the planted bugs found a real bug in the toy, which the spike's single instance could not have produced:

```
simulation_test.go:963: seed 190 found something else: [the log has commit 3 with the writes [[4 3]], which no caller made, or whose commit failed]
```

`S4QL_SIM_SEED=190 S4QL_SIM_BUG=3 go test ./internal/sim/ -run TestSimulationSeed -v` showed it. The swap for transaction 3 was applied and its answer lost (`store swap manifest.json from v5: applied with v7 <nil>, reported as an injected error`). The read-back that resolves it was delayed 53 seconds, the lease ran out meanwhile, and `b` took it over and caught up, commit 3 included. The toy then saw a manifest that was not its own and reported `commit of transaction 3: stopped: lease lost`, a definite failure, for a committed transaction. The agreed design for a swap whose outcome is unknown says what to do: walk the log back from the new head and look for the commit. The toy does that now, and returns nil from `Commit` with the instance stopped.

The first run also found `wallOnly` in 0 of 200 seeds. A holder that was cut off stopped at its first renewal, whose outcome was unknown, so it never served a stale read. The agreed design resolves an unknown renewal later and keeps serving while the clocks say that the lease holds, which is the window that the wall clock check guards. The toy now keeps such a renewal pending and resolves it before the next swap.

My first crash-only bug, `trustCache` (catch up only into an empty cache), was not crash-only: an instance that the supervisor reopens after losing its lease has a cache with data too, so the control would find it. I replaced it before running it.

### What I learned

- The two-instance workload found in 200 seeds a bug that 20000 runs with one instance could not, because it needs another holder to move the manifest while a swap is unresolved.
- A planted bug that the checks do not find can be a wrong toy and not a weak check. Both of today's misses were the toy departing from the agreed design.

### What was tricky

Keeping the clock faults inside the design's assumptions. The first scheme, a wall clock set backward for good, breaks the assumption that wall clocks agree as soon as the instance renews, and would have produced violations that the design accepts.

### What warrants review

- `/internal/sim/simulation_test.go`, `workload.run` and `outcome.verify`: whether a check can pass for the wrong reason, and whether the liveness check of two minutes is tight enough to mean something.
- `/internal/sim/simulation_test.go`, `workload.moveClocks`: whether the faults are the ones that the design claims to survive, and no others.
- `/internal/sim/toy_test.go`, `toyDB.resolveSwap` and `toyDB.resolvePending`.

### Future work

The toy stops on a commit whose outcome is unknown, where the design refuses and keeps trying. The core must do it properly, and its simulation needs the same check for commits that are in the log without an acknowledgement.

## Step 4: The lint

**Author:** harness-builder

### Prompt Context

**Verbatim prompt:** The brief quoted in Step 2.
**Interpretation:** Requirement 6: forbid the listed constructs in every non-test Go file of the module outside an explicit list, in `make lint` and in CI, with a test that has a planted violation for each rule. Also check whether CI runs the free-running mode under the race detector.
**Inferred intent:** Turn the rules that the scheduler sees only when the code runs, or never, into a check that runs on every change.

### What I did

`/internal/simlint/simlint.go` is a `go/analysis` analyzer, and `/internal/simlint/cmd/simlint/main.go` runs it with `singlechecker`. It resolves identifiers with the type checker, so a renamed import, a dot import, a method value such as `now := time.Now`, and an embedded `sync.WaitGroup` are all caught. It reports go statements; `time.Now`, `Since`, `Until`, `Sleep`, `After`, `AfterFunc`, `NewTimer`, `NewTicker`, `Tick`, `Timer`, and `Ticker`; `context.WithTimeout`, `WithDeadline`, and their `Cause` variants; `context.AfterFunc`, which starts a goroutine and is beyond the list in Step 1; `sync.Cond`, `NewCond`, and `WaitGroup`; and imports of `math/rand`, `math/rand/v2`, and `crypto/rand`. It skips `_test.go` files and exempts `internal/host`, `internal/sim`, and `internal/sqlite/update`, each with its reason. The test, `/internal/simlint/simlint_test.go`, runs `analysistest` over `/internal/simlint/testdata/src/library`, which breaks every rule once, a test file there that breaks rules and must not be reported, and a stand-in for the exempt `internal/host`.

`make lint` runs it, and `/.github/workflows/ci.yml` has a new job, `simlint`, that runs it. The reusable test workflow already runs `go test -race -shuffle on ./...` (I read `maragudk/workflows/.github/workflows/test.yml` with `gh api`), so the free-running mode runs under the race detector in CI without a change.

### Why

An analyzer over typed syntax is the standard way to do this in Go, and `analysistest` gives the planted violations for free. A test over the bare syntax tree would miss renamed and dot imports. The cost is a dependency on `golang.org/x/tools` v0.52.0, which also brings `golang.org/x/mod` and `golang.org/x/sync` into `/go.mod` as indirect requirements. Nothing outside the lint imports them.

### What worked

`go run ./internal/simlint/cmd/simlint ./...` reports nothing on the module. A file planted in `/internal/sqlite` with `go time.Sleep(time.Second)` gave two reports, `a go statement starts a goroutine outside the host; use host.Host.Go` and `time.Sleep waits on the real clock; use host.Host.Sleep`, and exit status 3. I deleted the file again.

To show that the test fails for each planted violation, a script removed one rule at a time from the analyzer and ran the test with `go test -overlay`: all 25 mutations made it fail (22 identifiers and imports, the go statement, the skip of test files, and the exemption).

### What didn't work

Running the command with `GOFLAGS=-overlay=...` to plant the file without touching the tree failed with `internal/sqlite/planted.go:1: no such file or directory`, because `go/packages` reads the files itself. I copied the file into the tree for one run instead.

### What I learned

`singlechecker` analyses test files by default (`-test=true`), so the analyzer skips them by name rather than relying on a flag.

### What was tricky

Nothing beyond the choice of approach.

### What warrants review

- The exempt list in `/internal/simlint/simlint.go`. The lint does not see the cross-check module in `/internal/sqlite/crosscheck`, which is a separate module of test code.
- Whether `context.AfterFunc` belongs in the list.

### Future work

The lint cannot see a goroutine that a dependency starts, such as one of an S3 client. The scheduler's goroutine check is what catches one that reaches the host.


## Step 5: Self-review, and what it changed

**Author:** harness-builder

### Prompt Context

**Verbatim prompt:** The brief quoted in Step 2, and the builder's standing instructions: "Once the implementation is done, self-review before handing back to the lead. Work in two phases: first, review the code you wrote -- read the diff, check logic, look for missing edge cases and test coverage gaps; second, run automated checks (tests and linters). Be honest about real issues; if everything looks good, say so rather than inventing problems. Use the code-review skill for the first phase -- invoke it rather than reviewing inline; it dispatches competing reviewers over your diff. Address what you find, then report a summary of your review and any follow-up work back to the lead."
**Interpretation:** Run the code-review skill over `git diff main...HEAD`, fix what is real, and verify again, because the first verification was of code that the review then changed.
**Inferred intent:** The harness is only as trustworthy as its checks, so they have to survive someone who tries to make them pass for the wrong reason.

### What I did

Before the review, I ran the verification on the code of Steps 2 to 4. With `S4QL_SIM_SEEDS=10000 go test ./internal/sim/ -run 'TestSimulation$' -v`, all three workloads gave 0 differing pairs and no violation, and every planted bug was found with no control finding it. I also measured the goroutine check (below).

Then I ran the `fabrik:code-review` skill. Two reviewers on the `fable` model reviewed the branch independently, told that they competed, and tried their claims through `go test -overlay` without touching the tree. They agreed on most findings, and I fixed all of those, and the serious single ones:

- **A timeout derived through a value made the context package start a goroutine, and one seed gave two runs.** Both reviewers demonstrated it (reviewer A: one seed, 299 against 1 traces). My `AfterFunc` method helps only when the timeout is the direct parent, and `Instance.Go` wraps every task context in `context.WithValue`. The timeout now embeds a `context.WithCancelCause` copy of its parent and has no done channel of its own, so every derived context attaches to the copy at once. The cost is that a derived context reports `context.Canceled` with `context.DeadlineExceeded` as its cause, and the host's waits return `context.DeadlineExceeded`. This also fixed an error that was not nil while `Done` was open, and a timeout of a parent that is already done reporting the deadline. A new test asserts that no goroutine is started.
- **A timeout always beat a sleep that ended at the same instant.** The seed now decides: 49 against 51 in 100 seeds.
- **Read values were never compared with the model.** With `Read` returning 0 as the value, everything passed. Each read is now compared with the value that the log has at the read's commit.
- **The liveness check read a flag that was never cleared.** With renewal removed, everything passed. The check is now a read that must succeed.
- **Nothing checked the files that a restart finds.** With the copy turned off, everything passed. The crash now compares the copy with the crashed cache.
- **Commits were matched by their writes, which two transactions can share.** Each change file now carries a transaction ID, and the model matches by it.
- **The toy never used a timeout, and almost never reached a commit of unknown outcome** (6 in 2000 seeds). Every call of a caller now has a timeout of 30 seconds, and a buggify point makes the read-back of a swap say nothing.
- **A task started by a deferred call of a crashed task ran with its waits ignored,** and **a timeout could be set through another instance's host.** Both fail the run now.
- **Lint holes:** channel receives, sends, ranges, and selects without a default; `context.Background` and `TODO`; `iter.Pull` and `Pull2`; `runtime.SetFinalizer` and `AddCleanup`; `hash/maphash`; and `golang.org/x/sync/errgroup`. All are reported now, each with a planted violation.
- **`golang.org/x/tools` in the library's `go.mod`** would have raised minimum versions for every module that requires s4ql. The lint is a module of its own now, like `/internal/sqlite/crosscheck`, built there and run on the root by `make lint` and by CI.
- **`Free` copied the production host,** so the race runs never tested what ships. It embeds `host.New()` now and overrides `Go`, `Yield`, `Buggify`, and `Trace`.
- **Production randomness relied on undocumented seeding of `math/rand/v2`.** It is `crypto/rand` now.
- **The written crash differences** missed that a request in flight is never delivered, and claimed wrongly that deferred calls cannot reach what the run checks. Both are corrected in the package documentation.
- **The `wallOnly` control** removed both the partition and the shift. A new fault, `partition`, draws the same numbers as `wallBackward` without the shift, and is the control now.
- **The requirement that CI runs the free-running mode under the race detector "permanently"** rested on the reusable workflow. The new CI job `simulation` runs it itself.
- **Comments and constants:** `host` named its importer and the internals of `math/rand/v2`; the `timeout` comment described the context package's internals; single-use constants and errors (`allClockFaults`, `errLeaseHeld`, `errClosed`) are inlined; the dead `toyTx.Get` and `Instance.Name` are gone; `expected` checks each error of a joined one; `TestSimulationSpeed` runs only with `S4QL_SIM_SPEED` set.

I kept the lint's three rule tables as package-level variables, although each is used once: their names say what they are, and inlining them would bury the rules in `run`.

### Why

Each fix closes a way for a check to pass for the wrong reason, or for a seed not to be a replay.

### What worked

Six mutations that the reviewers had shown to pass now fail `TestSimulation` at 100 seeds: a read that returns 0, no renewal, no copy of the files at a crash, no copy of the write-ahead log, catch-up newest first, and catch-up that drops the oldest missing commit. Without the write-ahead log, the copy has no `meta` table at all, because the schema was never checkpointed into the database file. Of 39 mutations of the lint, one rule at a time, all 39 make its test fail.

The verification after the fixes, with `S4QL_SIM_SEEDS=10000` (the workload whose callers never pause runs a quarter of that):

```
one instance:   10000 seeds, each run twice: 0 pairs differed; 10000 distinct traces, and 10000 distinct orders of tasks
                133547 commits, 14608 failed, 305534 reads of which 2018 overtaken, 4236 refused for an expired lease, 6582 timeouts that expired
everything:     10000 seeds, each run twice: 0 pairs differed; 10000 distinct traces, and 10000 distinct orders of tasks
                106553 commits, 18106 failed, 487 of unknown outcome, 611 cut off by a crash, 254588 reads
                18713 buggify points that fired, 16722 moves of a clock, 10000 crashes and 10000 restarts, 10435 stops, 9280 timeouts that expired
never pause:    2500 seeds, each run twice: 0 pairs differed; 2500 distinct traces
```

No seed of any workload violated an invariant. The planted bugs, over 10000 seeds each, and their controls over the same 10000:

```
renewal outside the lock                1572 seeds   (no control)
cache before the manifest               1750 seeds   (no control)
wall clock only                         1352 seeds   control, partition without the shift: 0
monotonic clock only                    1841 seeds   control, no clock fault: 0
takeover only after a release           9915 seeds   control, no crash: 0
ignore a failure of the cache           2068 seeds   control, buggify off: 0
```

Each of the first three seeds that found a bug gave the same trace hash and the same violations five times over. `make test` and `make lint` pass, and `go test -race -shuffle on ./internal/sim/ ./internal/host/` passes in 152 s. With `S4QL_SIM_PLANT_RACE=free go test -race -run TestPlantedDataRace ./internal/sim/`, the race detector reports `WARNING: DATA RACE` at `/internal/sim/toy_test.go:834`; with `S4QL_SIM_PLANT_RACE=scheduler`, it reports nothing, as in the spike.

### What didn't work

The goroutine check costs more than the spike measured. An A/B comparison with `go test -overlay`, where `goroutineID` returns a constant, interleaved three times on a machine with a load average of about 33:

```
                        with the check            without
scheduler alone         98000 to 122000 steps/s   436000 to 508000 steps/s
one instance            138 to 152 runs/s         192 to 259 runs/s
everything              59 to 68 runs/s           103 to 113 runs/s
```

It matters, about 1.6 times on the workloads, but the lint does not cover what it covers: a goroutine that a dependency or test code starts. It stays on, with no option to switch it off. A CPU profile of the scheduler alone put 84% of the time in `pthread_cond_signal` and `pthread_cond_wait`, the hand-over between goroutines, and under 5% in the package's own code. All speed numbers in this diary are from a loaded machine, and other runs were going on at the same time.

Moving the lint to its own module broke my mutation script's `go test ./internal/simlint/` from the root, which I fixed by running it in the module's directory.

The first attempt at `expected` for joined errors also treated `fmt.Errorf("%w: %w", errStopped, cause)` as a joined error, because it too has `Unwrap() []error`, so a stopped error would have needed its cause in the list. A stopped error is checked first now.

At the very end, the tooling moved this session's working directory to the other builder's worktree, `/.claude/worktrees/storage-formats`, and refused writes and git commands in `/.claude/worktrees/sim-harness`: `This session is isolated in the worktree /Users/maragubot/Developer/s4ql/.claude/worktrees/storage-formats, so its writes are limited to that folder.` I did not write there, so this step was handed to the lead to append and commit.

### What I learned

- The context package's fast path depends on the parent's done channel being the done channel of the nearest cancelable context in its chain. A context type of its own breaks it through any wrapper, and the fallback is a goroutine.
- Removing a mechanism is a good test of a check. Three of the reviewers' strongest findings were "remove this, and everything still passes".

### What was tricky

Choosing between the error value of a derived context and determinism. No implementation from outside the context package gets both, so I chose determinism, and made the host's own waits hide the difference.

### What warrants review

- A design question for Markus, which both reviewers raised and which I did not settle: a wall clock set backward on a holder that can still reach the store gives stale reads with only one clock wrong. The holder renews with an expiry that is already past for the others, another instance takes the lease, and both of the holder's checks still pass. Reviewer B showed it with the partition removed and no planted bug: seeds 4, 31, and 47 of 100 report `a read saw commit 6 after commit 7 was acknowledged`. Reviewer A saw 19 of 200. `/docs/todo.md` says "A wall clock that is set backward is caught by the monotonic clock", which holds only while the holder writes no expiry from the wrong clock. The workload keeps such an instance cut off from the store, and `/internal/sim/simulation_test.go` says so.
- `/internal/sim/sim.go`, the `timeout` type and `waitErr`.
- `/internal/sim/simulation_test.go`, `valueAt` and the crash's check of the copy.

### Future work

- The host has no way to wait for a condition, such as "until the instance serves". The toy's supervisor polls with `Sleep`. The core will need to decide whether to poll too or to have the host grow a primitive.
- A request in flight at a crash is never delivered. A simulated store that should deliver it late has to carry requests on a task of the test.
- Two single-reviewer findings left as they are: the jitter of a release and of a task's start has no test of its own, and the planted-bug test asserts that a bug is found only at 200 seeds or more.
