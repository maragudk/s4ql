# Diary: a lease that compares no clocks between machines, tried in the toy

The lease design of 2026-11-11 records an absolute expiry from the holder's wall clock in the manifest. On 2026-11-12 the simulation showed that it gives stale reads with one wrong wall clock (`/docs/diary/2026-11-12-simulation-harness.md`, Step 5, "What warrants review"). Markus has agreed to a design that compares no clocks between machines. This task changes the toy library in `/internal/sim` to that design and finds out with the simulation whether it holds.

## Step 1: Requirements

**Author:** lease-toy-builder

### Prompt Context

**Verbatim prompt:**

> You are testing a design decision in the simulation of s4ql, a Go SQL database whose truth is a log in S3 (repository `maragudk/s4ql`, module `maragu.dev/s4ql`, Go 1.27.3). You work in your own git worktree: run `pwd` and `git branch --show-current` first and stay in that directory. Never write in `/Users/maragubot/Developer/s4ql` itself or in another worktree under `.claude/worktrees/`. Run `git fetch origin` and check that your branch starts from the current `origin/main` (it must contain the commit "Fix three small things in the formats, the Makefile, and the lint"); if it does not, merge `origin/main` first.
>
> ## Background
>
> One process at a time holds a lease on a database. The lease is recorded in the object `manifest.json`, which is only ever replaced with a compare-and-swap on its version. The storage core that will implement this for real is not built yet. What exists is the simulation harness (`internal/host`, `internal/sim`) and, in the harness's test code, a toy library with s4ql's shape (`internal/sim/toy_test.go`, driven by `internal/sim/simulation_test.go`). The toy exists to prove the harness and to try designs.
>
> The toy implements the lease design of 2026-11-11: the manifest records an absolute expiry time from the holder's wall clock, and another instance takes the lease when its own wall clock has passed it. On 2026-11-12 the simulation showed that this design gives stale reads with a single wrong wall clock: a holder whose clock is behind writes an expiry that others see as past, a spare takes over and commits, and the holder's own checks still pass. Because of that, the workload today keeps an instance whose wall clock was set backward cut off from the store, and says why in a comment in `simulation_test.go`.
>
> The product owner, Markus, has since agreed to a new design that compares no clocks between machines. The spec and the todo are being updated in another branch; the design is this:
>
> - **Taking the lease.** An instance that wants the lease reads the manifest. If it has no lease entry, the instance takes the lease at once. Otherwise it waits for the lease's duration plus a margin on its own monotonic clock, and then takes the lease with a compare-and-swap against the version it read. If the manifest changed, the holder is alive, and the wait starts again from the new version.
> - **The manifest's lease entry** holds the holder's ID, the lease's duration, the holder's wall-clock time at the renewal (nothing reads it for a decision), and a salt of 64 random bits that is new at every write of the manifest by the holder. There is no expiry time.
> - **Why the salt:** the version of an object comes from its content, as S3's ETag does. A renewal that wrote the same bytes would be invisible to the instance that watches, and that instance's compare-and-swap would succeed against a live holder.
> - **The duration is in the manifest** because an instance that watches must use the holder's duration, not its own setting.
> - **The holder's check before a read:** less than the lease's duration has passed since it sent its last renewal, by its wall clock and by its monotonic clock, and both must show it. Each clock is compared only with itself. The duration counts from when the first attempt of a renewal was sent, not from a retry and not from the answer.
> - **A clean shutdown releases the lease** (no lease entry), so the next instance does not wait.
> - Unchanged: an instance without a valid lease refuses reads and writes and keeps trying; a swap whose outcome is unknown is resolved by walking the log back; a local failure stops the instance.
>
> ## The goal
>
> Change the toy to the new design, and find out with the simulation whether the design holds. This is an experiment with a result, not only a refactoring: the honest outcome may be "it holds", or a seed that shows it does not.
>
> 1. **The toy follows the new design** as described above. Read `docs/diary/2026-11-12-simulation-harness.md` first; it tells you what the toy, the workload, its model, and the planted bugs are.
> 2. **The toy's simulated store gives an object's version from its content,** so that two writes of the same bytes have the same version. If it numbers versions today, this is the change that makes the salt matter. Check what it does and say so.
> 3. **The restriction on backward wall clocks goes.** The workload moves the wall clock of any instance forward and backward, by small and large amounts, while the instance stays connected to the store, and the model still requires that no read misses an acknowledged commit. The monotonic clock is still stopped and started as today. What the design does not survive stays out of the workload only with a comment that says exactly which fault it is and why the design accepts it. I expect two such cases at most: an instance whose two clocks are both wrong in the same direction, and an instance whose monotonic clock runs at the wrong rate; if you find others, that is a finding, so report it.
> 4. **Planted bugs for the new design,** each found by many seeds and by none of the control without the ability it needs, as the existing ones are:
>    - a renewal that writes no new salt
>    - an instance that watches for its own configured duration when the holder's is longer
>    - an instance that takes the lease with a compare-and-swap against the version it reads at the end of the wait, not the one it read at the start
>    - a holder that counts its duration from the answer of the renewal, not from when it was sent
>    - a holder that checks only one of its two clocks (the two existing planted bugs `wallOnly` and `monotonicOnly` may already be these; keep or adapt them)
>    The existing planted bugs that still make sense stay. One that tested the old expiry design and has no meaning now is removed, and you say which.
> 5. **The liveness check stays:** some instance serves a real read within a bounded time after the workload, and you adjust the bound to the new design's wait and say what it is.
> 6. **Every seed run twice still gives the same trace.**
>
> Verification, with numbers in your report: `S4QL_SIM_SEEDS=10000` on the full workload with no violation and no differing pair; each planted bug with the count of seeds that find it and its control; `make test`, `make lint`, and `go test -race -shuffle on ./internal/sim/ ./internal/host/` pass.
>
> If the simulation finds a violation that is not a bug of the toy but a hole in the design, do not patch around it. Reduce it to the smallest seed and trace you can, describe the scenario in plain steps, and report it; that is the most valuable thing this task can produce.
>
> ## How to work
>
> - Invoke the `fabrik:go` skill before you read or write Go code. Read `AGENTS.md` (note `encoding/json/v2`).
> - You change test code in `internal/sim` only. If the harness itself (`sim.go`, `free.go`, `internal/host`) needs a change for this, make the smallest one and say why.
> - Start a new diary `docs/diary/2026-11-12-lease-without-clock-comparison.md` with the `fabrik:diary` skill, under your name `lease-toy-builder`. Its first step records the requirements of this brief. Record failures with exact commands and errors, and the numbers.
> - Update `internal/sim/README.md` only if it exists in your branch and says something about the toy's lease that your change makes false.
> - Do not edit `docs/spec.md`, `docs/todo.md`, `README.md`, or `AGENTS.md`.
> - Commit on your branch with the `fabrik:git` skill's conventions. Do not ask about GitHub issues; there are none. No attribution lines. Do not push and do not open a pull request.
> - When done, self-review as your standing instructions say and fix what is real.
> - You cannot ask me anything while you run. If you hit a question you cannot settle, stop and put it in your report.
>
> ## Your report
>
> Read by me, the lead, not by Markus. Give me: the name of your branch and the absolute path of your worktree, whether the design holds, every fault you had to keep out of the workload and why, the numbers, the planted bugs with their counts, what you changed in the harness if anything, and what a builder of the real core should take from this.

**Interpretation:** Six numbered requirements: the toy follows the new lease design; the simulated store versions objects by content; wall clocks move both ways while connected; five kinds of planted bug for the new design, each with a control; the liveness check with a bound fitted to the new wait; and replay of every seed. The result to report is whether the design holds, with the numbers.

**Inferred intent:** Find out before the storage core is built whether a lease that compares no clocks between machines is safe under the faults that the old design failed, and leave the toy as an executable statement of the design.

### What I did

I checked the worktree: `/Users/maragubot/Developer/s4ql/.claude/worktrees/agent-a0f97f77ab75dfb21`, branch `worktree-agent-a0f97f77ab75dfb21`, which contains `origin/main` at `e116bdc` ("Fix three small things in the formats, the Makefile, and the lint (#7)"). I read `/AGENTS.md`, the harness diary, `/internal/sim/toy_test.go`, `/internal/sim/simulation_test.go`, and the clock code of `/internal/sim/sim.go`. There is no `/internal/sim/README.md` in this branch.

What the store does today: `memStore` numbers versions. Every `Put` and `Swap` takes the next value of a counter, `v1`, `v2`, and so on, so two writes of the same bytes get different ETags, and a salt could not matter.

### Why

The requirements are binding for the steps that follow, and the brief asks for them as the first step.

### What worked

The harness diary describes the toy, the model, and each planted bug with its control, so the places to change were clear.

### What didn't work

Nothing yet.

### What I learned

The wall-clock time in the lease entry is part of the manifest's bytes. With a wall clock read to the nanosecond, two renewals almost never write the same bytes even without a salt. The salt matters only when the holder's wall clock shows the same reading at two writes that are far apart, which a wall clock that stands still does.

### What was tricky

Seeing in advance which fault each new planted bug needs. Four of them need an ability that the workload does not have yet: a wall clock that stands still (no new salt), instances with different lease durations (own duration), and a store that delays an answer rather than a request (duration from the answer).

### What warrants review

Nothing yet.

### Future work

None beyond the steps that follow.
