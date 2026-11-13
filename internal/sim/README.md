# Simulation harness

`internal/sim` is the simulation harness of s4ql. It runs the library, which is plain blocking Go, one goroutine at a time under a seed, so that a seed replays a run exactly, with its failures. Two other packages belong to it: `internal/host` is the interface through which library code reaches the world, and `internal/simlint` is the lint that keeps library code inside that interface.

What the harness does, and how to use it, is in the code:

- the package comment in `sim.go`, with the scheduler, the instances and their clocks, crashes and how they differ from real ones, buggify, the trace, and the rules for code that runs under the scheduler
- the doc comment of `Free` in `free.go`
- the package comment and the doc comments of `Host` in [internal/host/host.go](../host/host.go)
- the package comment in [internal/simlint/simlint.go](../simlint/simlint.go), with everything the lint reports and why

This file holds the reasons behind the design, what was measured, and what the harness cannot do. A spike on 2026-11-10 showed that the design works. Markus, on 2026-11-11, on building on it: "I think we should just try it out." The harness was built from the spike on 2026-11-12, in pull request #6. The two diaries are [2026-11-10-simulation-spike.md](../../docs/diary/2026-11-10-simulation-spike.md) and [2026-11-12-simulation-harness.md](../../docs/diary/2026-11-12-simulation-harness.md).

The research behind it is in three notes: [foundationdb.md](../../docs/research/foundationdb.md), [slatedb.md](../../docs/research/slatedb.md), and [testing-strategies.md](../../docs/research/testing-strategies.md).

## Two modes

- **Scheduled mode.** The real library runs unchanged inside a `testing/synctest` test, with test goroutines playing application callers. One goroutine runs at a time. It runs until it waits, and the seed then picks which goroutine goes next. This is FoundationDB's model rebuilt in Go.
- **Free-running mode.** The same code runs with the scheduler off, on `Free`, with real parallelism and no replay. It exists for the race detector, which finds nothing in scheduled mode.

## The host interface

`host.Host` is the only thing that library code sees of the harness, so every method has one reason:

- **`Go`,** because the lease task is the library's one goroutine.
- **`NewSemaphore`,** because the writer lock, the manifest lock, and the reader pool are a lock, a lock, and a semaphore.
- **`Sleep`,** for the renewal and for backoff.
- **`WithTimeout`,** because `context.WithTimeout` is a timer of the bubble's clock, which stands still.
- **`Yield`,** for the windows between waits.
- **`Wall` and `Monotonic`,** because the lease is checked against both, separately.
- **`Uint64`,** for the lease holder ID and every random choice.
- **`Buggify`,** for the points that the spec asks for under "Testing".

There is no method for the trace. Library code needs nothing from a trace, and simulation code, such as a simulated object store, gets it from the concrete type.

The host has no way to wait for a condition, such as "until the instance serves". The core waits for one with `Sleep`, or the `Acquire` of a semaphore, and a context that whoever changes the condition cancels; see [Waiting](../core/README.md#waiting) in `internal/core/README.md`.

## Why the scheduler owns the clock, the locks, and the randomness

The scheduler cannot leave them to Go:

- **Timers.** Timers that are due at the same instant fire in random order under `testing/synctest`, which `TestSynctestTimerOrder` records. The scheduler therefore keeps its own simulated clock, and no task waits on the bubble's clock.
- **Locks.** A lock made of a channel wakes the next holder directly, and two goroutines then run at once until each waits again. With the lock in the scheduler, a release only changes a counter, and the seed picks who gets the permit. A task also parks when a permit is free, for the same reason.
- **Timeouts.** Two timeouts that are due at the same instant would race, so the scheduler ends them between steps, in the order in which they were made. The seed decides between a timeout and a sleep that end at the same instant.
- **Randomness.** The library draws from the same sequence as the scheduler, so a run that takes a different turn anywhere draws different numbers from then on. With buggify off, a buggify point draws no number, so a run is the same with the point as without it.

Other choices follow from these:

- **Sleeps and timeouts follow the instance's monotonic clock,** because Go's timers do. It is also what makes a stopped clock dangerous: the renewal stalls, and the lease runs out in real time. Yields, releases, and the start of a task get their delay on simulated time, so a stopped clock does not freeze an instance that is not sleeping.
- **A context derived from a timeout reports `context.Canceled`,** as the package comment says. The reason is determinism. For a context type of its own, the context package starts a goroutine to pass the end on to derived contexts, and that goroutine runs outside the scheduler. One seed then gave two different runs. No type from outside the context package gets both the error value and determinism. The timeout therefore wraps a context of the context package, and the host's own waits hide the difference.
- **Each task records the ID of its goroutine,** read from its stack trace, and a wait from any other goroutine fails the run. The check cost about 1.6 times on the workloads of the toy library that the harness once had (see below): 138 to 152 runs per second with it against 192 to 259 without, for one instance, on a loaded machine. It stays on, with no switch, because the lint cannot see a goroutine that a dependency or test code starts.
- **`Free` shares nothing between goroutines that the race detector counts as synchronisation.** A seeded generator or a trace behind a mutex is a happens-before edge between every pair of goroutines that use it, and hides data races. Its random numbers therefore come from no seed, and it keeps no trace. It embeds the production host, so the race runs test the code that ships.
- **`testing/synctest` works with cgo.** A goroutine inside a SQLite call counts as running, so the scheduler waits for the call to return, and fake time does not move meanwhile. This is not documented, so `TestSynctestWithCgo` stays as a canary for Go upgrades.

## The lint

- **It is a `go/analysis` analyzer over typed syntax.** It resolves identifiers with the type checker, so it catches a renamed import, a dot import, a method value such as `now := time.Now`, and an embedded `sync.WaitGroup`. A test over the bare syntax tree would miss them.
- **It is a module of its own,** so that `golang.org/x/tools` stays out of the library's `go.mod` and does not raise minimum versions for every module that requires s4ql.
- **It skips test files by name,** because the command that runs it analyses test files by default.
- **It cannot see a goroutine that a dependency starts,** such as one of an S3 client. The scheduler's goroutine check catches one that reaches the host.

## The toy library, which is gone

Before the storage core existed, the harness's own tests held a toy library with the shape of s4ql: a store behind a manifest, a lease, and a SQLite cache, with three workloads, a model, and ten planted bugs. It proved the harness, and it tested the design of the lease before the core was built on it. It was removed on 2026-11-13. What it found:

- **Two instances found in 200 seeds a bug that 20000 runs with one instance could not.** It was a real bug in the toy, not a planted one. It needed another holder to move the manifest while a swap was unresolved.
- **The first design of the lease had a hole.** The design of 2026-11-11 put an expiry from the holder's wall clock in the manifest. The review of the build found stale reads with it in 3 of 100 seeds, as soon as a holder whose wall clock was set backward could still reach the store. That finding changed the design to one that compares no clocks between instances; see [How the design of the lease came about](../core/README.md#how-the-design-of-the-lease-came-about) in `internal/core/README.md`.
- **The salt in the manifest earns its place.** The toy's store gave a version from the content, as S3's ETag does. A renewal that wrote no new salt was then invisible to an instance that waited to take the lease, and the planted bug was found by 813 of 10000 seeds, against none of the control. The bug needed a wall clock that stands still, which is why `Instance.StopWall` exists. Planted in the workload with everything, it was found by 0 of 2500 seeds: the bugs of the lease need workloads of their own.

The toy is in the diaries [2026-11-12-simulation-harness.md](../../docs/diary/2026-11-12-simulation-harness.md) and [2026-11-12-lease-without-clock-comparison.md](../../docs/diary/2026-11-12-lease-without-clock-comparison.md), and its removal in [2026-11-13-remove-the-toy.md](../../docs/diary/2026-11-13-remove-the-toy.md). The toy's work now lives in the tests of the core, which carry over its ten planted bugs and add ten, and run the same three kinds of workload and one for writes that land long after they were sent, the free-running mode, and a planted data race; see [Tests](../core/README.md#tests) in `internal/core/README.md`. The harness's own abilities are tested directly in `sim_test.go` and `free_test.go`.

## What was measured

- **Determinism.** The spike ran 10000 seeds twice with no differing pair, and each seed gave a different interleaving. The toy's workloads then ran twice for each seed, 10000 seeds of the workload with one instance and of the workload with two instances, clocks that move, a crash and a restart, and buggify, and 2500 of the workload whose callers never pause. No pair of traces differed.
- **Buggify keeps its two probabilities.** Over 200 seeds it switched on 207 of 800 points, and a point that was on fired in 5196 of 20700 visits, which is a quarter of each.
- **Speed.** In the spike, the scheduler alone took 1217657 steps per second with tasks that only yield, and 253130 once each task checked the ID of its goroutine. `TestSimulationSpeed` logs the number on the machine at hand.

## Conditions for keeping the design

The spike set five conditions. Each is met as follows:

- **A lint forbids in library packages what the scheduler cannot run.** `internal/simlint` does, in `make lint` and in the CI job `simulation`. It forbids more than the spike's list, such as channel operations that block and `context.Background`. One rule has no check: an ordinary mutex must not be held across a wait. Neither the lint nor the scheduler detects that, and the test hangs, as `TestScheduler_mutexHeldAcrossWait` shows.
- **Every seed is run twice, permanently.** `TestSimulation` in this package runs 200 seeds twice and compares the traces, with a workload that uses the abilities of the scheduler together. The core's `TestSimulation` runs every seed of its workloads twice too. Both run in `make test` and in CI.
- **The free-running mode runs under the race detector in CI.** The CI job `simulation` runs `TestFree` of this package and the free-running tests of the core with `-race` itself, whatever the reusable test workflow does.
- **The checks compare data against a model, and planted bugs stay as tests of the checks.** The simulation of the core compares every result with the model in `internal/core/model_test.go`, and its `TestSimulation_plantedBugs` holds twenty planted bugs. A bug that needs one ability of the harness also has a control, the same workload without that ability, which must never find it. The reason for the condition is that a check can pass for the wrong reason. The spike's toy had a real bug that 20000 clean runs produced and no check looked for. In the review of the build, three checks still passed with the mechanism they guard removed.
- **Every lock acquire and release is a scheduling point, and yields are placed wherever shared state is touched between waits.** The semaphore of the scheduler meets the first half, and `TestScheduler_NewSemaphore` shows that a task that waits for a permit can run before the task that released it goes on. The second half is a rule for library code, and nothing checks it.

## What the scheduled mode cannot do

- **It switches goroutines only at waits.** `TestScheduler_window` shows it. In the spike, a race with no wait in the window was found by 0 of 1000 seeds, and by 659 once a yield was added.
- **It hides data races from the race detector.** `TestFreeRunning_plantedDataRace` in `internal/core` plants one: the detector reports it in free-running mode, and nothing in scheduled mode.
- **It never has two goroutines inside SQLite.** In `TestSchedulerWithSQLiteLocks`, a writer among eight readers never gets `SQLITE_BUSY` under the scheduler. In the spike, the same code got it 70 times in 10000 transactions with the scheduler off.
- **It finds only what the checks look for.**
- **It runs only under `go test`, and a seed is tied to the exact code.** A trace that prints text that depends on the build, such as an error of Protocol Buffers, replays only in the same binary.

The free-running mode has a limit of its own. Its tests run on the fake clock of a bubble, where time moves only when every goroutine is blocked, so a goroutine that wakes from a wait often runs alone. What the harness does not do yet is in [docs/todo.md](../../docs/todo.md#tests-of-the-storage-core).

## Ideas to revisit later

Markus on 2026-11-11, on whether the free-running mode can go: "I'd rather keep the free-running mode for now, and we can revisit later." The ideas:

- Pass the baton between goroutines through something the race detector cannot see, such as a call into C, so that the detector finds data races in scheduled mode. Untested.
- For code without data races, only synchronisation operations are places where a switch can change the outcome. If every lock and atomic operation is a scheduling point, the scheduled mode then covers what production can do.
- Inject SQLite's contention effects, such as "busy", from the seed at the binding, and keep one small test with real parallelism whose only job is to check our assumptions about SQLite, as real S3 does for the simulated store.
- The alternative is FoundationDB's own way: one global lock, so that production also runs one goroutine at a time. It would make the SQL engine run one query at a time and weaken "reads run in parallel".

## Tests

The tests of the harness use no store, no lease, and no database, except the canaries below. `sim_test.go` tests the scheduler, its instances, their clocks, crashes, timeouts, semaphores, buggify, and the trace, each with a small and aimed test. `free_test.go` tests `Free`.

`TestSimulation` runs 200 seeds twice and compares the traces, the number of steps, and the next number from the seed. Its workload, `everything`, uses the abilities of the scheduler together: instances whose workers each share a lock and a pool, wait with timeouts and contexts derived from them, hit a buggify point, and start tasks that wait through the host of the test; tasks of the test that share a semaphore; clocks that the test moves; a crash followed by a restart; and an instance that crashes itself. The test also checks that no semaphore lets in too many holders or loses a permit, and that nothing of an instance runs after its crash. It checks the scheduler, not a database, so nothing in it compares data. The environment variable `S4QL_SIM_SEEDS` sets another number of seeds.

Three tests are canaries for Go upgrades. They record what `testing/synctest` does with timers and with calls into C, which the design rests on and the documentation of Go does not promise: `TestSynctestTimerOrder` in `timers_test.go`, and `TestSynctestWithCgo` and `TestSchedulerWithSQLiteLocks` in `cgo_test.go`, which use SQLite.

On a Mac with 10 cores and a load of 12 to 18 from other work, the package took 12 seconds, and 22 seconds under the race detector. With the toy, it took 57 and 83 seconds, and 172 seconds under the address sanitizer in a Docker container with four CPUs.

Two tests run only when an environment variable is set. `S4QL_SIM_SPEED` makes `TestSimulationSpeed` log how fast the scheduler is. `S4QL_SIM_HANG` makes `TestScheduler_mutexHeldAcrossWait` show a run that hangs, and needs a short `-timeout`.
