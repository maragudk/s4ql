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
