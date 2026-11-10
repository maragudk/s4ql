# Diary: simulation spike

A research spike to find out whether s4ql can get deterministic simulation in Go the way FoundationDB gets it: the real library, written as plain blocking code, run one goroutine at a time under a seed. The design is called "design B" in the conversation of 2026-11-10. It replaces the state machine that the spec asks for under "Concurrency", which Markus decided to drop that day. The spike decides whether design B is adopted.

## Step 1: Requirements and scope

**Author:** main

### Prompt Context

**Verbatim prompt:** Good point, we could control locking with locks inside the Go driver before calling out into C. That would work with synctest I think.
**Interpretation:** Markus had said "synctest doesn't play well with CGo AFAIK". I answered that the documentation does not mention cgo, that a goroutine inside a cgo call is treated as still running, and that the risk is a SQLite call that blocks on something another goroutine must do. I proposed making that the first question of a spike. His reply accepts the approach and adds the mitigation: take locks in Go before calling into C, so that SQLite itself never waits.
**Inferred intent:** Find out with a test, not by reading, whether design B works, starting with the cgo question, before anything is built on it.

### What I did

Earlier in the same conversation a research agent wrote a design brief with four designs for running the library deterministically. Markus then dropped the state machine ("Yes, no state machine"), and asked me to look again at how FoundationDB tests. FoundationDB runs plain coroutine code on one thread with a cooperative scheduler, and design B is that model rebuilt in Go on `testing/synctest`.

I read the `testing/synctest` documentation for Go 1.27.3 with `go doc testing/synctest`. It lists five operations that durably block a goroutine (channel operations on bubbled channels, `select` on them, `sync.Cond.Wait`, `sync.WaitGroup.Wait`, `time.Sleep`), says that locking a `sync.Mutex`, I/O, and system calls are not durably blocking, and does not mention cgo.

I created the worktree `.claude/worktrees/sim-spike` on branch `worktree-sim-spike`, wrote this entry, and briefed one builder on Opus. The questions handed to the builder, in order:

1. How `testing/synctest` behaves with real SQLite calls through `/internal/sqlite`, including a call that is deliberately made to wait on a lock.
2. Whether a seeded scheduler can run plain blocking code one goroutine at a time, with goroutines parking at simulated S3 calls, timers, and marked points in reads.
3. Whether a toy library with s4ql's shape (a simulated object store with a compare-and-swap manifest, one writer at a time, a lease renewal goroutine, a SQLite cache, parallel readers) gives the same trace when a seed is run twice, over many seeds.
4. Whether the same harness runs with the scheduler off under the race detector.
5. Whether a deliberately planted bug is found and then replayed from its seed.

### Why

Design B is the only option that replays the whole library, including the public API and the readers, and it matches the spec's "production code runs unchanged". It is unproven in Go with cgo, so nothing should be built on it before a test shows it works.

### What worked

Reading the documentation directly settled what it does and does not say about cgo, which the design brief had described from the runtime source.

### What didn't work

Nothing has failed yet. No code has been written.

### What I learned

The `testing/synctest` documentation is silent on cgo. By its definition a goroutine in a cgo call is not durably blocked, so `synctest.Wait` should wait for the call to return. That is inferred from the documentation, not tested.

### What was tricky

Keeping the spike a spike. It answers questions with evidence, and the toy library is not the start of the engine.

### What warrants review

Whether the five questions are the right ones, and whether a spike that passes is enough to adopt design B.

### Future work

If the spike passes: the spec's state machine sentence needs new wording agreed with Markus, and the driver item in `/docs/todo.md` needs rewriting. If it fails: the choice between a state machine core and giving up exact replay of ordering comes back.

## Step 2: `testing/synctest` with real SQLite calls

**Author:** spike-builder

### Prompt Context

**Verbatim prompt:**

> You are running a research spike for s4ql. Your name is `spike-builder`. The output is evidence and a clear verdict, with code that demonstrates it. It is not the start of the engine.
>
> ## Where you work
>
> Work only in the worktree `/Users/maragubot/Developer/s4ql/.claude/worktrees/sim-spike` (branch `worktree-sim-spike`). Do not `cd` to the main checkout. Use the go skill before touching Go and the git skill for commits (no attribution lines; no GitHub issue to reference, so do not ask). Commit on the branch as you go. Do not push and do not open a pull request.
>
> ## Read first
>
> - `AGENTS.md`, `docs/spec.md`, `docs/todo.md` (short).
> - `docs/diary/2026-11-10-simulation-spike.md`: the diary for this spike, with my Step 1 (uncommitted so far; commit it with your first commit). Add your own steps with the diary skill (author `spike-builder`), recording failures verbatim, and commit the diary with the code. Do not edit my step.
> - `internal/sqlite/README.md` and `internal/sqlite/sqlite.go`: our cgo binding to SQLite. It is minimal (open, prepare, bind and read integers, step, finalize, `Exec`).
> - `go doc testing/synctest` for the Go version in `go.mod` (1.27.3).
> - Optional background: a design brief from a research agent at `/private/tmp/claude-502/-Users-maragubot-Developer-s4ql/f1343515-adca-4ca0-9eb0-f507768f8fc7/scratchpad/driver-design-brief.md` (its "design B" is what you are testing; its state machine core has since been dropped), and `docs/research/foundationdb.md` section 1.1 for the model being copied.
>
> ## Context
>
> s4ql is a Go library: a SQL database whose truth is a log in S3, with a local SQLite cache that serves reads. Its spec requires deterministic simulation: the production code runs unchanged where object storage, time, and randomness sit behind interfaces driven by one seed, and the same seed replays the same run.
>
> The product owner (Markus) decided today to drop the state machine the spec describes and to write plain blocking Go instead: each caller's goroutine does its own work, a lock lets one write transaction through at a time, and one background goroutine renews the lease. The question is whether such code can be made to replay exactly in Go, the way FoundationDB does it with one thread and cooperative coroutines.
>
> The design under test:
>
> - The library runs unchanged inside a `testing/synctest` bubble.
> - Wherever a goroutine meets the outside world (a call to the simulated object store, a wait on time, a marked yield point in a read, a step of a test caller), it parks under a stable name and tells a scheduler.
> - The scheduler waits until every goroutine in the bubble is parked (`synctest.Wait`), picks one by the seed, lets it run to its next parking point, and repeats. So one goroutine runs at a time, and the seed decides the order.
> - SQLite calls run to completion inside whichever goroutine is running. Markus's point: locks are taken in Go before calling into C, so that SQLite itself never waits on a lock. A lock that is held across a parking point cannot be a `sync.Mutex`, because waiting on one is not durably blocking; it must be built on a channel or similar.
> - Every seed is run twice and the two traces are compared, to catch nondeterminism.
> - A second mode runs the same code with the scheduler off, under the race detector, with real parallelism and no replay.
>
> Markus's own doubt, to take seriously: "synctest doesn't play well with CGo AFAIK". Question 1 exists to settle that with a test.
>
> ## Questions to answer, in this order
>
> Stop and report as soon as an answer makes the rest pointless.
>
> 1. **`testing/synctest` with cgo.** The documentation does not mention cgo; by its definition a goroutine in a cgo call is not durably blocked. Test what actually happens with real SQLite calls through `internal/sqlite` from several goroutines in a bubble, on a file database in WAL mode with one connection per goroutine:
>    - Does `synctest.Wait` wait for a running SQLite call to return?
>    - What happens when a SQLite call is deliberately made to wait: two connections that want the write lock, with `pragma busy_timeout` set, and with it at 0? Does the bubble hang, does fake time interfere, is a deadlock detected?
>    - Anything else that goes wrong between the two (finalizers, cleanup, C threads, the race detector, the address sanitizer build).
>    Record exact outputs.
> 2. **The scheduler.** Build the smallest seeded scheduler that runs plain blocking code one goroutine at a time as described. Decide how waits on time work (the brief claims timers due at the same instant fire in random order under synctest; check that, and if so keep a timer queue in the scheduler). Provide a lock that is safe to hold across a parking point.
> 3. **A toy library with s4ql's shape**, in plain blocking Go, as test code: an in-memory object store behind an interface with a compare-and-swap manifest and injected faults (errors, delays, an operation that succeeds but reports failure); one writer at a time; a commit that uploads, swaps the manifest, then commits to a SQLite cache; a background goroutine that renews a lease on a timer through the same manifest; readers on their own SQLite connections in parallel; several caller goroutines running a simple workload. Keep it small. It exists to exercise the scheduler, not to be right about s4ql's design.
> 4. **Run twice and compare.** For many seeds, run each seed twice and compare a trace of the ordered operations and their results. Report how many seeds you ran, whether any pair differed, and how many distinct traces the seeds produced (to show that the seed really varies the interleaving). If pairs differ, find the cause; that is a finding, not a failure to hide.
> 5. **Free-running mode.** Run the same toy library with the scheduler off under `-race`. Does it work without changes to the library code? Does the race detector behave in both modes?
> 6. **A planted bug.** Plant one realistic concurrency bug in the toy library (for example: a read that starts after a commit is acknowledged can see the state before it; or renewal and commit both swapping the manifest so one fails). Show that some seed finds it, and that the same seed finds it again on every rerun.
>
> ## What to report on, beyond the six answers
>
> - The rules the library code must follow for this to work, as you found them in practice, and how each can be checked or enforced (a lint, a test, a runtime check) instead of being left to discipline.
> - Where nondeterminism leaked in and how you found it.
> - Measured numbers only: how many simulated runs per second on this machine, and what dominates. Give no estimates you did not measure.
> - What the design cannot do, stated plainly.
> - Your verdict: adopt, adopt with named conditions, or reject, argued from correctness, verifiability, and risk. Do not argue from how much effort a human team would need.
>
> ## Constraints
>
> - Do not change `internal/sqlite` unless a question cannot be answered otherwise; if you must, keep it minimal and say so.
> - Put the code in a new internal package (for example `internal/sim`) with the toy library in its tests. No changes to public API, the spec, or `docs/todo.md`.
> - No telemetry.
> - `make test` and `make lint` must pass at the end, and the spike's tests must run under `go test -race`. Say plainly what you could not run.
>
> ## Final report
>
> The six answers with evidence, the rules, the measurements, the limits, and the verdict. Also say what you would do differently if this became the real simulation harness. If you hit a question of scope or product intent you cannot resolve, stop and end your run with the question.

**Interpretation:** Answer six questions in order with tests that stay in the repository as evidence, starting with whether `testing/synctest` and cgo work together at all, because a "no" there ends the spike.
**Inferred intent:** Decide whether plain blocking Go can replace the spec's state machine without losing exact replay, on evidence and not on reading.

### What I did

I wrote `/internal/sim/cgo_test.go` (`TestSynctestWithCgo`) and `/internal/sim/timers_test.go` (`TestSynctestTimerOrder`). Every case opens a file database in `t.TempDir()` in WAL mode through `/internal/sqlite`, inside `synctest.Test`. A slow call is a recursive query that counts to three million inside one `sqlite3_step`, about 230 ms on this machine. Real time is measured around `synctest.Test` from outside the bubble, because inside it `time.Now` is fake.

I ran the tests plain, under `-race`, and under the address sanitizer in a Linux container:

```
docker run --rm -v "$PWD":/src -v s4ql-gomod:/go/pkg/mod -v s4ql-gocache:/root/.cache/go-build -w /src golang:1.27.3 go test -asan -count=1 -v -run 'TestSynctestWithCgo|TestSynctestTimerOrder' ./internal/sim/
```

### Why

The whole design rests on `synctest.Wait` meaning "nothing is running", and on SQLite calls being steps that start and finish inside one turn of one goroutine. If `Wait` returned while C code ran, or if a bubble hung on a SQLite call, there would be no design to test.

### What worked

Plain run, output of `go test ./internal/sim/ -run TestSynctestWithCgo -v -count=1` (line numbers as they were at the time):

```
cgo_test.go:86: run 0: Wait returned after the call; the bubble took 230ms of real time
cgo_test.go:86: run 1: Wait returned after the call; the bubble took 228ms of real time
cgo_test.go:86: run 2: Wait returned after the call; the bubble took 232ms of real time
cgo_test.go:86: run 3: Wait returned after the call; the bubble took 230ms of real time
cgo_test.go:86: run 4: Wait returned after the call; the bubble took 233ms of real time
cgo_test.go:110: an hour of fake time passed only after the call returned; the bubble took 232ms of real time
cgo_test.go:135: the waiter got SQLITE_BUSY after 1.636s of real time and no fake time
cgo_test.go:173: the waiter gave up after 1.645s of real time; fake time could not advance to wake the holder
cgo_test.go:191: the waiter got SQLITE_BUSY at once; the bubble took 1ms of real time
cgo_test.go:239: synctest.Test panicked with: deadlock: all goroutines in bubble are blocked
```

So, for Go 1.27.3 and SQLite 3.54.1:

- `synctest.Wait` does wait for a running SQLite call. In five of five runs it returned only after the 230 ms call had.
- Fake time does not advance while a goroutine is in C. An hour-long `time.Sleep` in another goroutine returned only after the call.
- A SQLite call that waits for a lock (`pragma busy_timeout = 1500`, second connection wants the write lock) sleeps in C in real time. The bubble does not hang and reports no deadlock. It stalls for the timeout, 1.6 s of real time and zero fake time, and the call returns `SQLITE_BUSY`.
- If the lock holder is asleep on fake time, the waiter can never get the lock, however short the holder's sleep: fake time cannot move while the waiter is in C. The holder slept 1 ms, the waiter had 1500 ms, and the waiter still got `SQLITE_BUSY`. Outside a bubble it would have got the lock.
- With `busy_timeout` at 0 the waiter gets `SQLITE_BUSY` at once.
- A waiter does get the lock if the holder is running, not parked (holder busy in C for 230 ms, then commits). That is ordinary SQLite behaviour and it is not replayable.
- The bubble's deadlock detection still works after SQLite calls: `deadlock: all goroutines in bubble are blocked`.
- `t.TempDir` and `t.Cleanup` (closing connections) work inside the bubble. The binding registers no finalizers or cleanups, and no test needed a C thread, so nothing came up there.
- The same tests pass under `-race` and under `-asan` on Linux (`ok maragu.dev/s4ql/internal/sim 20.142s` in the container).

Timers: in 300 runs each, four goroutines that `time.Sleep` to the same instant woke in all 24 possible orders, four `time.AfterFunc` timers ran in all 24, and a `select` over four timers chose in all 24. The claim in the design brief holds, so the scheduler keeps its own clock.

### What didn't work

Two things, and both are findings.

First, a single writer among parallel readers gets `SQLITE_BUSY`, although nothing else writes. The test had one writer connection, eight reader connections, WAL mode, and `busy_timeout = 0` everywhere, and I expected zero:

```
cgo_test.go:300: begin immediate: sqlite: database is locked (code 5)
cgo_test.go:312: 3000 writes and 107427 reads in 279ms of real time; SQLITE_BUSY on setting WAL mode: 0, on reads: 0, on writes: 18
cgo_test.go:315: Expected "0", but got "18" (type int64)
```

Over five more runs the writer's `begin immediate` failed 27, 95, 31, 13, and 28 times in 3000 transactions. With `busy_timeout = 1000` on the writer it failed 0 times in each of five runs. The cause that fits is in `/internal/sqlite/c/sqlite3.c`, `walIndexReadHdr` (line 69656): "If the first attempt failed, it might have been due to a race with a writer. So get a WRITE lock and try again." A reader that sees the wal-index header change under it takes the WAL write lock for a moment, and a writer that begins just then fails at line 70682 ("Only one writer allowed at a time. Get the write lock. Return SQLITE_BUSY if unable."). I read this in the source and did not prove it by instrumenting SQLite.

Second, the race detector reported a race in my own test:

```
WARNING: DATA RACE
Read at 0x00c0000144e8 by goroutine 11:
  maragu.dev/s4ql/internal/sim_test.TestSynctestWithCgo.func2.1()
      /Users/maragubot/Developer/s4ql/.claude/worktrees/sim-spike/internal/sim/cgo_test.go:107 +0x248
...
Previous write at 0x00c0000144e8 by goroutine 12:
  maragu.dev/s4ql/internal/sim_test.TestSynctestWithCgo.func2.1.1()
      /Users/maragubot/Developer/s4ql/.claude/worktrees/sim-spike/internal/sim/cgo_test.go:102 +0xa4
```

One goroutine wrote a variable after its SQLite call, and the test read it after `time.Sleep(time.Hour)`. The fake clock guarantees that order, but the race detector does not count it as synchronisation. It does count `synctest.Wait`: the same pattern with `Wait` in the first test is not reported. I made the variable atomic.

### What I learned

- Markus's rule, "take the lock in Go before calling into C, so that SQLite never waits", is right for the simulation and not sufficient for production. Under the scheduler only one goroutine is ever in SQLite, so no SQLite lock is ever contended. With real parallel readers, SQLite's own readers take the WAL write lock briefly, and the one writer can get `SQLITE_BUSY` from `begin immediate`. Production code needs a busy timeout or a retry there, and the deterministic mode can never exercise that path. Only the free-running mode can.
- A busy timeout in a simulated run is harmless only because it never triggers. If it ever did, it would burn real time and then fail, because the lock holder cannot run while the waiter is in C.
- An ordering that only the fake clock provides is a data race to the race detector.

### What was tricky

Measuring real time around a bubble: `time.Now` inside is fake, so the stopwatch has to be outside `synctest.Test`.

The session's sandbox refuses shell commands that chain several steps ("this command is too complex to verify that it stays inside the worktree"), so files are written with the editor tools and each shell command is a single step.

### What warrants review

`/internal/sim/cgo_test.go`: whether the cases cover what "plays well with cgo" has to mean, and the explanation of the `SQLITE_BUSY` finding. The first of the two parallel cases logs its count and asserts nothing about it, because the count varies and is sometimes zero.

### Future work

How production handles `SQLITE_BUSY` on the write connection (a busy timeout, or a retry in Go) has to be decided when the engine is built. The spec's "a local I/O failure stops the instance" should then say whether `SQLITE_BUSY` counts as one.

## Step 3: The scheduler

**Author:** spike-builder

### Prompt Context

**Verbatim prompt:** The brief quoted in Step 2. Its question 2: "Build the smallest seeded scheduler that runs plain blocking code one goroutine at a time as described. Decide how waits on time work (the brief claims timers due at the same instant fire in random order under synctest; check that, and if so keep a timer queue in the scheduler). Provide a lock that is safe to hold across a parking point."
**Interpretation:** A package with a seeded scheduler, its own clock, and a lock, small enough to read in one sitting, with tests that show replay and the checks it can make.
**Inferred intent:** Find out what the library must do differently to run under such a scheduler, and which of those rules a machine can check.

### What I did

I wrote `/internal/sim/sim.go` and `/internal/sim/sim_test.go`. The package has two runtimes with the same methods: `Go` (start a task), `Task.Wait`, `Yield(site)`, `Sleep`, `Now`, `NewSemaphore` (`Acquire` and `Release`; one permit makes a lock), `Uint64` (randomness), and `Trace`.

`sim.Scheduler` is the deterministic one. Each task is a goroutine that, at every one of those calls, records where it waits and blocks on a channel of its own. The loop in `Scheduler.Run` calls `synctest.Wait`, collects the waiting tasks sorted by name, keeps those that may run (their time has come, or their condition is true), picks one with the seeded generator, and sends on its channel. If none may run, it moves its own clock to the earliest wake time. Conditions (a permit is free, a task has returned, a context is done) are plain functions that the loop calls while nothing runs, so no task ever wakes another directly.

`sim.Free` maps the same methods to plain Go: `go`, `runtime.Gosched`, a timer in a `select` with the context, a buffered channel as the semaphore.

A task's identity travels in its `context.Context`, set by `Go`.

### Why

Three decisions, each with its reason.

- **The scheduler owns the clock.** Step 2 showed that timers due at the same fake instant fire in every possible order. So `Sleep` is a wait with a wake time in the scheduler, and the bubble's fake clock is never used. A task that calls `time.Sleep` is reported as a broken rule.
- **Locks belong to the scheduler.** A channel lock would be durably blocking and so legal under synctest, but its release wakes the next holder directly, and then two goroutines run at once until each waits again. With the lock in the scheduler, a release only changes a counter, and the seed picks who gets the permit.
- **Identity in the context, not in the goroutine.** Go has no goroutine-local storage, and names must be stable across runs for the sort before the seeded pick. The context is already passed to everything that does I/O.

### What worked

- Same seed, same trace: 200 seeds, each run twice, all pairs equal, 200 distinct traces.
- Tasks due at the same simulated instant wake in an order the seed decides: 300 seeds gave all 24 orders of four tasks, and every seed gave the same order twice.
- The scheduler can check three rules at run time, and each has a test:
  - A task blocked on something the scheduler does not know (a raw channel, `time.Sleep`): after `synctest.Wait` the task it released is neither waiting in the scheduler nor finished. Error: `task main is blocked outside the scheduler, after before`.
  - A `go` statement inside a task: the new goroutine arrives at a wait under its parent's name while the scheduler has not run it. Error: `task main reached ... without the scheduler having run it`.
  - A deadlock, with the tasks and where they wait: `deadlock: every task waits on a condition that is false: worker-0 at acquire b, worker-1 at acquire a`.
- `TestScheduler_window` shows the limit that matters most. A check-then-act race with no waiting point between the check and the act was found by 0 of 1000 seeds. With a `Yield` in the window, 659 of 1000 seeds found it, each of them on all three reruns. The free runtime found the version without the yield in 1 of 1000 runs.

### What didn't work

A task that blocks outside the scheduler cannot be unwound by it, and the bubble then reports its own deadlock on top of the scheduler's error. My first test of the `time.Sleep` rule died with:

```
panic: deadlock: main bubble goroutine has exited but blocked goroutines remain [recovered, repanicked]
...
goroutine 3410 [sleep (durable), synctest bubble 453]:
time.Sleep(0x3b9aca00)
```

The cause is in the synctest documentation: "Time stops advancing when the root goroutine of the bubble exits." The test now sleeps on the fake clock after `Run` has returned its error, so that the stray sleeper can finish.

My first test for "something other than the scheduler wakes a task" used a raw channel between two tasks and died the same way (`goroutine 3437 [chan receive (durable), synctest bubble 454]`), because the rule above fires first: a task cannot be woken from outside unless it first blocked outside. I replaced it with the `go` statement case, which is the one way to get two running goroutines without blocking first. Getting that case to fail cleanly needed a change: a wait that finds the run failed panics with a value that the task wrapper recovers, and a finished task closes its channel so that a second goroutine waiting under the same name is not left behind.

### What I learned

- `synctest.Wait` gives the scheduler more than quiescence. Together with "which task did I release", it tells the scheduler whether that task followed the rules. The check costs nothing extra.
- The seed must also drive how long a yield takes. With zero-length yields, a task that yields always runs again before simulated time moves, so a task waiting on time can never get in between. `Options.Jitter` gives half of the yields a random simulated delay.
- The design switches tasks only at waits. Go in production switches anywhere and runs in parallel. FoundationDB does not have this gap, because its production code is cooperative too. Here the simulation explores a strict subset of what production can do, and the subset is set by where the waits are.

### What was tricky

Failing a run cleanly. The scheduler has to end every goroutine in the bubble or synctest panics, so a failed run wakes the waiting tasks one at a time and each panics out of its wait, running its deferred calls.

### What warrants review

`/internal/sim/sim.go`, `Scheduler.next` and `Scheduler.park`: these two functions are the whole mechanism. Check that nothing in them depends on the order in which goroutines arrive, only on names and on the generator.

### Future work

A real harness needs more primitives than the spike has: a broadcast ("stopped") that waiters observe, and a pool. Both are conditions polled by the loop, like the semaphore.

## Step 4: The toy library, run twice, run free, and with planted bugs

**Author:** spike-builder

### Prompt Context

**Verbatim prompt:** The brief quoted in Step 2, questions 3 to 6.
**Interpretation:** Build a small library with s4ql's moving parts on top of the runtime from Step 3, run it under the scheduler twice per seed and compare, run the same code with the scheduler off under the race detector, and plant a bug that only an interleaving finds.
**Inferred intent:** See whether replay survives contact with code that has real locks, a background task, faults, and SQLite, and learn what each of the two modes can and cannot find.

### What I did

`/internal/sim/toy_test.go` holds the toy library and the simulated store. `/internal/sim/simulation_test.go` holds the workload, its invariant checks, and the tests.

The toy library (`toyDB`) is plain blocking Go that knows the world only through an `env` interface, which both `*sim.Scheduler` and `*sim.Free` satisfy:

- `Begin` takes the writer lock and runs `begin immediate` on the one write connection. `Set` writes a row. `Commit` uploads a change file named by its SHA-256, swaps the manifest under a second lock (the swap lock), and then commits the SQLite transaction. The writer lock is held for the whole interactive transaction, the swap lock only around the upload and swap, so that the renewal can run while a transaction is open.
- `renewLoop` is a task that sleeps until 20 seconds after the last successful swap, then swaps the manifest under the swap lock. The lease lasts 30 seconds, measured from when the swap was sent.
- `Read` takes a permit and a connection from a pool, begins, yields, reads the commit number (which fixes the snapshot), checks the lease deadline, yields, and reads the value.
- A swap whose result is an error other than "precondition failed" is resolved by reading the manifest back.

The simulated store (`memStore`) waits a random 1 to 50 ms before applying an operation and again before answering, and injects three faults: an error without applying (3% of operations), applying but reporting an error (3%), and a delay of 40 to 70 seconds (1%), which is longer than the lease.

The workload is four callers with twelve transactions each, 35% writes, and up to 15 simulated seconds of thinking between transactions. It checks: a read that starts after a commit is acknowledged sees it; a read never sees a commit that the manifest does not have; the instance never loses its lease (there is no other instance); and at the end the manifest and the cache have exactly the acknowledged commits. A second workload, eight callers that never pause, is there to make tasks overlap.

Three planted bugs sit behind flags in `toyDB`: `renewOutsideLock` (the renewal reads the ETag under the swap lock, releases the lock, then swaps), `cacheFirst` (the commit reaches the cache before the manifest), and `unlockedCounter` (a plain data race in `Read`).

### Why

The toy has to contain the things that make s4ql hard to replay: two locks held across I/O, a timer-driven background task that contends with callers, cancellation, real SQLite on several connections, and failures whose outcome is unknown. The planted bugs are the kinds the brief names: one is a pure ordering bug with no data race, so only an interleaving can find it.

### What worked

Run twice (`TestSimulation`), with `S4QL_SIM_SEEDS=10000`:

```
10000 seeds, each run twice: 0 pairs differed, 10000 distinct traces
20000 runs in 54.155s, 369 runs per second, 134852 steps per second
```

No run violated an invariant. With the default 200 seeds the test also prints one hash over all traces, and that hash was identical in every setting I tried: plain on macOS, under `-race` on macOS, and under `-asan` on Linux arm64 in a container (`390db6fb36cda6a174adf9da9eb47106d58fa1db6c0ead6ee04bbdd0b3fa4fc2`, and `d2b6425918f00b85a9741bdc80824e79e69c023dea159217f7ab81e331d9939f` for the workload that never pauses). So the same seed gives the same run across processes, with and without race instrumentation, and across two operating systems.

The runs are not trivial. In 200 seeds: 2781 commits, 255 failed commits, 326 rollbacks, 6188 reads of which 34 were overtaken by a commit while they ran, and 50 reads refused for an expired lease. In the workload that never pauses, 2719 of 10320 reads were overtaken.

Planted bugs, over 10000 seeds:

```
1241 of 10000 seeds found the bug; the first are [2 19 22 28 34 39 51 54 57 60]     (renewOutsideLock)
8383 of 10000 seeds found the bug; the first are [0 2 4 5 6 7 8 9 10 11]          (cacheFirst)
```

Each of the first seeds that found a bug was rerun five times and gave the same trace hash and the same violations every time. The trace of seed 2 shows the renewal bug as it happens:

```
1m55.179893193s renewer store swap manifest.json from v19: v21 <nil>
1m55.194807999s caller-3 store swap manifest.json from v19:  precondition failed
...
1m55.238048994s caller-3 VIOLATION: commit of transaction 10: stopped: lease lost
```

Free-running mode (`TestFreeRunning`): the toy library runs with `sim.Free` without any change to its code, inside a bubble for the fake clock, and passes under `-race` with no violations.

The race detector in the two modes (`TestPlantedDataRace`, `unlockedCounter`):

- `S4QL_SIM_PLANT_RACE=scheduler go test -race ./internal/sim/ -run TestPlantedDataRace`: 50 seeds, `PASS`. The race is not reported, because every handover between tasks goes through the scheduler's channels and mutex, which the detector sees as synchronisation.
- `S4QL_SIM_PLANT_RACE=free go test -race ...`: `WARNING: DATA RACE` at `toy_test.go:513` in the first run.

Speed (`TestSimulationSpeed`, this machine, 10 cores, Apple silicon):

```
scheduler alone:                    320180 steps in 263ms, 1217657 steps per second
toy library:                        300 runs in 824ms, 364 runs per second, 133251 steps per second, 366 steps per run
toy library, synchronous = off:     300 runs in 654ms, 459 runs per second
```

Under `-race`: 134 runs per second, and the scheduler alone 139129 steps per second. Under `-asan` on Linux in the container: 71 runs per second. A CPU profile of 3000 runs (8.26 s of samples): 52% in `runtime.cgocall` (SQLite: opening and setting up the cache 21%, commits 17%, closing 7%, reads 6%), 35% in `pthread_cond_signal` and `pthread_cond_wait` (handing over between goroutines), 9% in system calls for the temporary directory. So SQLite and its files dominate, and the scheduler's own handover is the second cost.

### What didn't work

My first assertion for the `cacheFirst` bug was too narrow. It expected the stale-cache symptom, and some seeds showed the other symptom of the same bug:

```
simulation_test.go:336: seed 4 found something else: [12 commits were acknowledged, the manifest has 13, and the cache has 13]
```

The test now lists both symptoms.

I expected the free-running mode to be unable to repeat a failure, and named a test "gives no seed that finds it again". The measurement said otherwise:

```
46 of 300 free runs found the bug; run again with the same seeds, 45 of those 46 found it
```

Inside a bubble, the fake clock orders most of a free run. With callers that pause, 4 to 7 of 200 seeds gave two different traces when run twice. With callers that never pause, 50 of 50 did. So free running on the fake clock usually repeats itself for a workload spread out in time, and never for one that is not. I renamed the test.

I wanted the toy to show the `SQLITE_BUSY` finding from Step 2 with the scheduler off, and wrote a comment that explained why the fake clock would hide it and the real clock would show it. The measurement did not support the explanation, so I removed it:

```
with no busy timeout, in 2000 runs each: SQLITE_BUSY 0 times under the scheduler, 0 times with the scheduler off on the fake clock, and 0 times in 0 runs with the scheduler off on the real clock
```

The toy workload does not get `SQLITE_BUSY` in any mode. Only the tight loop in `TestSynctestWithCgo` does.

A test of mine that the run-twice check catches map iteration order asserted that more than 40 of 50 seeds differ, and got exactly 41. A small Go map has few iteration orders, so about one pair in eight agrees by chance. The test now asserts that at least one pair differs.

A `sync.Mutex` held while its holder waits hangs the run, and nothing reports it but the test timeout. `S4QL_SIM_HANG=1 go test ./internal/sim/ -run TestScheduler_mutexHeldAcrossWait -timeout 5s`:

```
panic: test timed out after 5s
	running tests:
		TestScheduler_mutexHeldAcrossWait (5s)
...
goroutine 7 [synctest.Wait (durable), synctest bubble 1]:
internal/synctest.Wait()
...
goroutine 10 [sync.Mutex.Lock, synctest bubble 1]:
internal/sync.runtime_SemacquireMutex(0x330e9dfc8960?, 0x8?, 0x330e9dec1ea8?)
```

That confirms what the design brief had only inferred.

### What I learned

- No pair of runs differed at any point while I built the toy, so I have no leak of nondeterminism to report from the scheduled mode. The choices that kept it out were made before the first run: the scheduler owns the clock and the locks, picks from names in sorted order, and hands out the only randomness. To see that the run-twice check works at all, I gave it a task that ranges over a map, and it failed as it should.
- The scheduled mode hides data races from the race detector completely. The free mode is needed for that, and it found the planted one at once.
- The free mode is weaker than I assumed at producing real overlap. On the fake clock, time moves only when every goroutine is blocked, so a goroutine that wakes from a wait often runs alone. It did not reproduce the one parallel-only effect I know of (`SQLITE_BUSY` on the write connection), on either clock. A tight stress test did.
- The order in which the scheduler logs steps reads oddly at a release: the waiter can resume from its acquire before the releaser resumes from its release, because a release adds the permit and then waits. I changed the wording of trace lines to "resumes from" to make that clear.

### What was tricky

Deciding where the lock for manifest swaps sits. An interactive write transaction holds the writer lock across the caller's own steps, and the renewal must still run, so the swap needs its own lock. That is also where the realistic bug is.

Writing invariants that hold in both modes. "A read that starts after an acknowledged commit sees it" is checked with a high-water mark that the committer stores after `Commit` returns and the reader loads before `Read` starts, which is valid with real parallelism too.

### What warrants review

- `/internal/sim/toy_test.go`, `toyDB.renew` and `toyTx.publish`: whether `renewOutsideLock` is a fair example of a realistic bug.
- `/internal/sim/simulation_test.go`, `workload.run`: the invariants, and whether a check could pass for the wrong reason.
- The speed numbers are from one machine and include `-shuffle` and coverage only where stated. Rerun with `go test ./internal/sim/ -run TestSimulationSpeed -v -count=1`.
- To repeat the large runs: `S4QL_SIM_SEEDS=10000 go test ./internal/sim/ -run 'TestSimulation/' -count=1 -timeout 900s -v`.

### Future work

The free-running mode needs a workload built to overlap, on the real clock, if it is to find parallel-only problems. The spike's does not.
