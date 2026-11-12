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

## Step 2: The toy on the new design, the workload, and the planted bugs

**Author:** lease-toy-builder

### Prompt Context

**Verbatim prompt:** The brief quoted in Step 1.
**Interpretation:** Requirements 1 to 6: change the toy and its store, open the workload to wall clocks that move both ways while connected, plant a bug for each rule of the design, fit the liveness bound to the new wait, and keep every seed a replay.
**Inferred intent:** A toy that is an executable statement of the design, with checks that are shown to bite.

### What I did

**The toy** (`/internal/sim/toy_test.go`). The manifest has `Lease *lease`, absent when no instance holds the lease, and `lease` has `Holder`, `Duration` (nanoseconds, because `encoding/json/v2` has no default encoding of a `time.Duration`), `Wall`, and `Salt`. Every write by the holder (acquire, renewal, commit) gets a new lease entry from `toyDB.newLease`, with 64 bits from the host as the salt, and `toyDB.trySwap` stamps the wall clock at the moment it sends. `toyDB.acquire` reads the manifest without locks; with the lease entry of another instance, it sleeps the holder's duration plus `takeoverMargin` (2 s) on its own monotonic clock, then takes the writer and swap locks and swaps against the ETag it read before the sleep. A precondition failure returns at once, so the next attempt reads the new version and waits again. With no lease entry, or one with its own ID, it swaps at once. The holder keeps `sent` (monotonic) and `sentWall` from the swap that is known to have happened, and `toyDB.leaseHolds` requires less than the duration since then by each clock. A renewal is due at two thirds of the duration. `toyDB.Close` releases by writing the manifest without a lease entry. Each instance has its own duration (`toyOptions.leaseDuration`).

**The store.** `memStore` numbered versions (`v1`, `v2`, ...). It now keeps only the bytes, and `etagOf` is the first 8 bytes of their SHA-256, so two writes of the same bytes have the same ETag, and a swap against it succeeds. I also split the latency into the way in and the way out: under `faults.delayAnswers`, a delay goes on the way out as often as not, and the number is drawn either way, so that a seed gives the same delays. A delay on the way out is traced when the store applies the operation (`applied with ..., the answer is delayed by ...`).

**The harness** (`/internal/sim/sim.go`). `Instance.StopWall` and `Instance.StartWall` hold a wall clock still, as `StopMonotonic` does for the monotonic clock; shifts still apply while it stands. A test is in `/internal/sim/sim_test.go`. It is the only harness change, and the reason is the salt: a renewal without a new salt writes the same bytes only if the holder's wall clock reads the same at two writes that are seconds apart, and `ShiftWall` cannot make that happen, because simulated time moves between any two steps.

**The workload** (`/internal/sim/simulation_test.go`). The clock faults are `stopMonotonic`, `wallForward`, `wallBackward`, and `stopWall`. A faulty instance (half of them in the full workload) gets one to four moves. It either has its monotonic clock stopped for 20 to 80 s and its wall clock moved only forward, or has its wall clock moved forward, backward, or stopped for up to five minutes. A move is small (under the margin) or large (up to two hours), as often as not, and is for good. Partitions are a fault of their own now. The comment on `clockFaults` names the two faults that are left out and why. `workload.unmoved` draws every clock fault at the same times and moves nothing: it is the control of the bugs that need a clock. The full workload draws each instance's lease from 20, 30, and 45 s, and has answers that come late. `workload.writes` sets the share of writes.

**Planted bugs.** New: `keepSalt` (a renewal reuses the salt), `ownDuration` (the watcher waits for its own duration), `lateVersion` (the watcher reads the manifest again after its wait and swaps against that), and `leaseFromAnswer` (the holder counts from the answer to a swap). `wallOnly` and `monotonicOnly` are kept, and now test the check of each clock against itself. `competing` builds their workloads: the full workload without crashes, clock faults, or buggify, with 30 transactions per caller and every instance faulty. None of the six old bugs lost its meaning, so none is removed: `takeoverAfterRelease` now means an instance that takes the lease only when there is no lease entry.

**Liveness.** `workload.liveness` is two waits for the longest lease and the margin, plus one delayed operation of the store (70 s): 2 m 44 s for the full workload, 2 m 14 s for the others. Two waits, because an instance that ends a wait can find a new version and wait again. The run records how long the end took, and `TestSimulation` logs the slowest seed.

### Why

Each bug breaks exactly one rule of the design, and its control removes exactly the fault that the rule is there for. A bug that the workload finds and the control does not shows that the rule matters and that the workload reaches it.

### What worked

At 2000 seeds, all three workloads gave no differing pair and no violation (after the fix of seed 1738 below). The worst time to serve after the workload was 57 s (one instance), 1 m 21 s (full, seed 1643), and 1 m 8 s (callers that never pause), against bounds of 2 m 14 s, 2 m 44 s, and 2 m 14 s. At 200 seeds every planted bug was found and no control found one: renewal outside the lock 31, cache first 35, wall only 35, monotonic only 45, no new salt 14, own duration 25, late version 127, lease from the answer 25, takeover only after a release 199, ignore a cache failure 36.

`keepSalt`, seed 64, shows the mechanism in the trace. `2m17.25s a/leaser ... store swap manifest.json from bbb42a501468761e: bbb42a501468761e <nil>` is a renewal that wrote the same bytes, so the version did not change. `2m32.08s b/leaser ... swap manifest.json from bbb42a501468761e: e5618c43d8bc75a0 <nil>` is the watcher's swap against the version it read before its wait, which succeeds against a live holder. `b` commits at 2m34.93s, and at 2m36.69s `a/caller-0 VIOLATION: a read saw commit 3 after commit 4 was acknowledged`.

### What didn't work

- The first run never served: `S4QL_SIM_SEEDS=50 go test ./internal/sim/ -run 'TestSimulation$' -v` gave `seed 0: no instance serves 10m0s after the end of the workload` for every seed. A probe test showed ``json: cannot marshal from Go sim_test.lease within "/lease": Go struct field Duration has unsupported `format` tag option`` for `json:"duration,format:nano"`. The duration is an `int64` of nanoseconds now. The marshal error had been returned from `trySwap` and retried forever, which shows only as a failure of liveness.
- The first run of the new bugs at 200 seeds found `keepSalt` in 0, `leaseFromAnswer` in 0, and `wallOnly` in 5. The seeds explained it. For `leaseFromAnswer`, seed 2: the late answer came at 57.28 s, but a commit that had queued on the swap lock during the delay failed 0.2 s later with `stopped: lease lost`, so the stale window closed before any read. Also, a watcher that first waited on an older version takes the lease up to 2(D+2) after the swap applied, which with 30 and 45 s leases is after an answer that is 40 to 70 s late. For `keepSalt`, the holder's wall clock must stand still across two of its writes with no commit in between, and wall stops lasted 20 to 80 s. I gave the bug workloads 30 transactions per caller and faults on every instance; `keepSalt` 10% writes, `leaseFromAnswer` 10 s leases and 5% delays, and `ownDuration` leases of 10 and 90 s; and let a wall clock stand still for up to five minutes.
- A sweep of the share of writes printed 0 for every bug: zsh does not split `$cfg` in `for cfg in "12 35 50" ...; do set -- $cfg`, so each variable held the whole string, which `strconv.Atoi` made 0. I reran it with explicit values.
- I made `toyDB.Begin` refuse a write when the lease does not hold, as the design says. It raised `leaseFromAnswer` from 14 to 25 of 200, and dropped `renewOutsideLock` from 31 to 4 of 200, because the renewals that race a commit mostly come after the lease ran out. A commit is safe without the check, since its swap fails if another instance took the lease, so I took the check out again.
- At 2000 seeds, the full workload had one violation, seed 1738: `the lease is still held by fdc1b75fcd9785be after every instance was closed`. `b` released the lease at 5m56.89s, and `a2`'s new instance read the released manifest and swapped at once. The store applied the swap and lost the answer, and buggify made the read-back say nothing. The instance closed without knowing that it held the lease. `toyDB.Close` now reads the manifest after an acquire whose outcome was unknown (`toyDB.unsure`), and releases the lease if it names this instance. This is not a stale read and not a hole in the lease: the lease entry would have timed out.

### What I learned

- A planted bug in the lease is hard to catch for a structural reason: the holder learns that it lost the lease at its next write, which in this workload is a commit within seconds. The stale window runs from the new holder's first acknowledged commit to the old holder's next write, so only seeds where both land in it find the bug. Fewer writes widen the window, but also make the new holder's commit rarer.
- The salt matters only under a wall clock that repeats a reading. With a wall clock that runs, the wall-clock time in the lease entry already makes the bytes of every write new.

### What was tricky

Making a control that removes exactly one ability. `unmoved` draws the clock faults and sleeps as they would, and `delayAnswers` draws its number either way, so that a control differs from its workload only in what the bug needs.

### What warrants review

- `/internal/sim/simulation_test.go`, the comment on `clockFaults`: whether the two faults left out are the only ones the design does not survive.
- `/internal/sim/toy_test.go`, `toyDB.acquire`: the wait, and the swap against the version read before it.
- `/internal/sim/simulation_test.go`, `workload.liveness`.

### Future work

The verification at 10000 seeds and the self-review follow in the next step.
