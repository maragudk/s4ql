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

The host has no way to wait for a condition, such as "until the instance serves". The toy's supervisor polls with `Sleep`.

## Why the scheduler owns the clock, the locks, and the randomness

The scheduler cannot leave them to Go:

- **Timers.** Timers that are due at the same instant fire in random order under `testing/synctest`, which `TestSynctestTimerOrder` records. The scheduler therefore keeps its own simulated clock, and no task waits on the bubble's clock.
- **Locks.** A lock made of a channel wakes the next holder directly, and two goroutines then run at once until each waits again. With the lock in the scheduler, a release only changes a counter, and the seed picks who gets the permit. A task also parks when a permit is free, for the same reason.
- **Timeouts.** Two timeouts that are due at the same instant would race, so the scheduler ends them between steps, in the order in which they were made. The seed decides between a timeout and a sleep that end at the same instant.
- **Randomness.** The library draws from the same sequence as the scheduler, so a run that takes a different turn anywhere draws different numbers from then on. With buggify off, a buggify point draws no number, so a run is the same with the point as without it.

Other choices follow from these:

- **Sleeps and timeouts follow the instance's monotonic clock,** because Go's timers do. It is also what makes a stopped clock dangerous: the renewal stalls, and the lease runs out in real time. Yields, releases, and the start of a task get their delay on simulated time, so a stopped clock does not freeze an instance that is not sleeping.
- **A context derived from a timeout reports `context.Canceled`,** as the package comment says. The reason is determinism. For a context type of its own, the context package starts a goroutine to pass the end on to derived contexts, and that goroutine runs outside the scheduler. One seed then gave two different runs. No type from outside the context package gets both the error value and determinism. The timeout therefore wraps a context of the context package, and the host's own waits hide the difference.
- **Each task records the ID of its goroutine,** read from its stack trace, and a wait from any other goroutine fails the run. The check costs about 1.6 times on the workloads: 138 to 152 runs per second with it against 192 to 259 without, for one instance, on a loaded machine. It stays on, with no switch, because the lint cannot see a goroutine that a dependency or test code starts.
- **`Free` shares nothing between goroutines that the race detector counts as synchronisation.** A seeded generator or a trace behind a mutex is a happens-before edge between every pair of goroutines that use it, and hides data races. Its random numbers therefore come from no seed, and it keeps no trace. It embeds the production host, so the race runs test the code that ships.
- **`testing/synctest` works with cgo.** A goroutine inside a SQLite call counts as running, so the scheduler waits for the call to return, and fake time does not move meanwhile. This is not documented, so `TestSynctestWithCgo` stays as a canary for Go upgrades.

## The lint

- **It is a `go/analysis` analyzer over typed syntax.** It resolves identifiers with the type checker, so it catches a renamed import, a dot import, a method value such as `now := time.Now`, and an embedded `sync.WaitGroup`. A test over the bare syntax tree would miss them.
- **It is a module of its own,** so that `golang.org/x/tools` stays out of the library's `go.mod` and does not raise minimum versions for every module that requires s4ql.
- **It skips test files by name,** because the command that runs it analyses test files by default.
- **It cannot see a goroutine that a dependency starts,** such as one of an S3 client. The scheduler's goroutine check catches one that reaches the host.

## The toy library

`toy_test.go` holds a toy library with the shape of s4ql, in test code. It is the harness's own workload until the storage core replaces it, and it is not a design for s4ql.

- **Three workloads run it.** The first has one instance. The second has everything: two instances that compete for the lease, clocks that move, a crash and a restart, buggify, and faults of the store. The third has callers that never pause. They are in `simulation_test.go`, and the doc comment of `workload.run` lists the invariants.
- **The checks compare data against a model, not counters.** Each commit is recorded with its writes before `Commit`, and then marked acknowledged, failed, or unknown. Each read is compared with the value that the log has at the read's commit.
- **A restart finds a copy of the files.** A crash copies the cache and its write-ahead log to a new path at the moment of the crash, and a new instance starts on the copy.
- **The toy's lease is the design of 2026-11-12,** which compares no clocks between instances; see [The lease and its clocks](../../docs/todo.md#the-lease-and-its-clocks). The toy first had the design of 2026-11-11, with an expiry on the holder's wall clock. The review of the build found stale reads with it in 3 of 100 seeds, as soon as a holder whose wall clock was set backward could still reach the store. That finding changed the lease's design, and the toy was then changed to test the new one (`docs/diary/2026-11-12-lease-without-clock-comparison.md`).
- **Two clock faults stay out of the workload,** because the design does not claim to survive them: both clocks of one instance wrong in the same direction, and a monotonic clock that runs fast on the instance that waits to take the lease. The comment on `clockFaults` in `simulation_test.go` says why. Every other move of a clock is in the workload, on instances that can reach the store.
- **The toy's store gives a version from the content,** as S3's ETag does, so that a renewal that writes the same bytes is invisible. With numbered versions, the salt in the manifest could not have been tested.

## What was measured

The spike ran 10000 seeds twice with no differing pair, and each seed gave a different interleaving.

The numbers below are from the toy with the lease of 2026-11-12. Each workload ran with `S4QL_SIM_SEEDS=10000`. No pair of traces differed, and no seed violated an invariant:

| Workload | Seeds, each run twice | What the runs held |
|---|---|---|
| One instance | 10000 | some instance served again at most 57 s after the workload |
| Everything | 10000 | 97456 commits, 489 of unknown outcome, 239048 reads, 10000 crashes and restarts, 35905 moves of a clock, 25029 buggify points that fired |
| Callers that never pause | 2500 | a quarter of the seeds, because its runs are several times longer |

The planted bugs, each over 10000 seeds. The control is the same workload without the fault that the bug needs. Each bug's workload also ran with the toy unchanged, and no seed failed then:

| Planted bug | Seeds that found it | Seeds of the control that found it |
|---|---|---|
| A renewal that writes no new salt | 813 | 0 |
| An instance that waits for its own duration, not the holder's | 1367 | 0 |
| A swap against the version read at the end of the wait | 6372 | 0 |
| A holder that counts its lease from the answer of the renewal | 696 | 0 |
| A read that checks the wall clock only | 2355 | 0 |
| A read that checks the monotonic clock only | 2090 | 0 |
| A renewal that swaps the manifest outside the lock | 1534 | no control |
| A commit that reaches the cache before the manifest | 1666 | no control |
| An instance that takes over a lease only after a release | 9917 | 0 |
| A commit that ignores a failure of the cache | 2001 | 0 |

The bugs of the lease need workloads of their own to be found by many seeds. Planted in the workload with everything, the renewal without a new salt was found by 0 of 2500 seeds, so a clean run of that workload says little about the salt.

What these runs do not show: the margin on the wait is untested, because the harness's clocks run at the true rate; and the toy's own store never applies a request after its sender saw it fail, which the simulated store in `internal/store/simstore` does.

More results:

- **Two instances found in 200 seeds a bug that 20000 runs with one instance could not.** It was a real bug in the toy, not a planted one. It needs another holder to move the manifest while a swap is unresolved.
- **Buggify keeps its two probabilities.** Over 200 seeds it switched on 207 of 800 points, and a point that was on fired in 5196 of 20700 visits, which is a quarter of each.

## Conditions for keeping the design

The spike set five conditions. Each is met as follows:

- **A lint forbids in library packages what the scheduler cannot run.** `internal/simlint` does, in `make lint` and in the CI job `simulation`. It forbids more than the spike's list, such as channel operations that block and `context.Background`. One rule has no check: an ordinary mutex must not be held across a wait. Neither the lint nor the scheduler detects that, and the test hangs, as `TestScheduler_mutexHeldAcrossWait` shows.
- **Every seed is run twice, permanently.** `TestSimulation` runs 200 seeds of each workload twice and compares the traces, in `make test` and in CI. The workload whose callers never pause runs 50.
- **The free-running mode runs under the race detector in CI.** The CI job `simulation` runs `TestFreeRunning` and `TestFree` with `-race` itself, whatever the reusable test workflow does.
- **The checks compare data against a model, and planted bugs stay as tests of the checks.** `TestSimulation_plantedBugs` holds the ten bugs above. A bug that needs one ability of the harness also has a control, the same workload without that ability, which must never find it. The reason for the condition is that a check can pass for the wrong reason. The spike's toy had a real bug that 20000 clean runs produced and no check looked for. In the review of the build, three checks still passed with the mechanism they guard removed.
- **Every lock acquire and release is a scheduling point, and yields are placed wherever shared state is touched between waits.** The semaphore of the scheduler meets the first half. The second half is a rule for library code, and nothing checks it.

## What the scheduled mode cannot do

- **It switches goroutines only at waits.** `TestScheduler_window` shows it. In the spike, a race with no wait in the window was found by 0 of 1000 seeds, and by 659 once a yield was added.
- **It hides data races from the race detector.** `TestPlantedDataRace` plants one: the detector reports it in free-running mode, and nothing in scheduled mode.
- **It never has two goroutines inside SQLite.** In `TestSchedulerWithSQLiteLocks`, a writer among eight readers never gets `SQLITE_BUSY` under the scheduler. In the spike, the same code got it 70 times in 10000 transactions with the scheduler off.
- **It finds only what the checks look for.**
- **It runs only under `go test`, and a seed is tied to the exact code.** A trace that prints text that depends on the build, such as an error of Protocol Buffers, replays only in the same binary.

The free-running mode has a limit of its own. Its tests run on the fake clock of a bubble, where time moves only when every goroutine is blocked, so a goroutine that wakes from a wait often runs alone. What the harness does not do yet is in [docs/todo.md](../../docs/todo.md#concurrency).

## Ideas to revisit later

Markus on 2026-11-11, on whether the free-running mode can go: "I'd rather keep the free-running mode for now, and we can revisit later." The ideas:

- Pass the baton between goroutines through something the race detector cannot see, such as a call into C, so that the detector finds data races in scheduled mode. Untested.
- For code without data races, only synchronisation operations are places where a switch can change the outcome. If every lock and atomic operation is a scheduling point, the scheduled mode then covers what production can do.
- Inject SQLite's contention effects, such as "busy", from the seed at the binding, and keep one small test with real parallelism whose only job is to check our assumptions about SQLite, as real S3 does for the simulated store.
- The alternative is FoundationDB's own way: one global lock, so that production also runs one goroutine at a time. It would make the SQL engine run one query at a time and weaken "reads run in parallel".

## Tests

By default, `TestSimulation` runs 200 seeds of each workload, each seed twice, and 50 for the workload whose callers never pause. `TestSimulation_plantedBugs` runs the workload of each bug until four seeds have found the bug, from at most 200 seeds, and fails if none does. With the shares in the table above, a check that works misses a bug in 200 seeds with a chance of less than one in a million. The control and the same workload without the bug run 100 seeds each. The tests of the workloads run in parallel, and each logs a hash of its traces that is the same with `-parallel 1`. On a Mac with 10 cores and a load of 11 to 16, the package took 58 seconds, and 87 seconds under the race detector.

The environment variable `S4QL_SIM_SEEDS` sets another number of seeds for every workload, a quarter of it for the one whose callers never pause, and makes the workload of each bug run all of them. The large runs above set it to 10000:

```sh
S4QL_SIM_SEEDS=10000 go test -timeout 4h -run '^TestSimulation(_plantedBugs)?$' ./internal/sim/
```

With 1000 seeds, these two tests took 8 minutes 20 seconds on the same Mac. The time grows with the number of seeds, so 10000 seeds need about ten times that, and the `-timeout` leaves room for a slower machine.

`TestSimulationSeed` runs one seed and logs its whole trace. Its doc comment names the environment variables that choose the seed, the workload, and a planted bug.
