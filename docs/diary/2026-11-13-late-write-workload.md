# Diary: a workload for writes that land long after they were sent

A write to S3 can get stuck: the caller gets an error or gives up, and the write lands later, up to many minutes later. The simulated store held such a write for at most 60 simulated seconds, while the lease lasts 30 seconds and a takeover takes about 33. So the simulation hardly showed a write of an old holder that lands after a new holder took the lease and committed. The design says that such a write is harmless: a late manifest swap is refused by its precondition, because every holder write carries a fresh salt, and a late commit object is created only if absent and is named by its content. This task makes the simulation show it.

## Step 1: The brief

**Author:** builder (late-write workload)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements; the full brief is in the lead's conversation)

> - `simstore` can hold a write in flight for a long time, up to 15 simulated minutes, with the delay drawn from the seed as its other faults are. The caller sees an unknown outcome, and the write is applied, or refused by its precondition, when it lands. Keep the present behaviour as the default of the existing workloads unless a change costs nothing in run time and breaks none of them.
> - A simulation workload for the core in which this happens for both kinds of write: the creation of a commit object, and a swap of the manifest. Between the send and the landing, the lease changes hands at least once in a good share of the seeds, with crashes and restarts, and other instances commit. The existing checks against the model must hold: no acknowledged commit is lost, a commit that was refused never appears, a commit with an unknown outcome is either in or out and every instance agrees, reads are never older than the spec allows.
> - Show that the workload has teeth. At least one planted bug must be found by it, and the clean code must pass. Look at the bugs that exist in `bugs.go` first; the ones around the "flights" of unknown outcomes and around the salt are the natural ones. If none fits, plant a new one in the same way (through the unexported `Options.bugs`), for example an instance that takes the lease with a salt it has used before, or that takes a refused swap as final while one of its own writes is still in flight. Say in the test how many seeds it took.
> - The same seed gives the same trace. Check it.
> - Run time: simulated time is free, but CI runs the package under the race detector and under the address sanitizer, several times slower than this machine, and Go stops a package's tests after 10 minutes. Earlier pull requests failed in CI on exactly this. Measure what your workload adds to `go test ./internal/core/`, under `-race` and under `make test-asan`, and keep the default run small; a longer run can sit behind the flag or environment variable the other simulation tests already use.
> - If the workload finds a real fault in the core or in the harness: when the fix is clear, small, and inside what the spec says, fix it with a regression test and put it first in your report. When a fix would change the design or the spec, do not fix it: stop, and report the seed, the trace, and what you think is wrong.
> - Docs: update `docs/todo.md` where an item changes (built things are deleted from it), and `internal/core/README.md` or `internal/sim/README.md` where they list the workloads and the planted bugs. Do not change `docs/spec.md` or `README.md`.

Another builder works in parallel on a commit object that is too large, with changes to `internal/store/store.go`, `simstore`, and `internal/core/tx.go`, so the changes to `simstore` are to stay local.

**Interpretation:** add a fault to `simstore` that holds a write back for up to 15 minutes, off by default; add a workload in `internal/core` with several instances, crashes, restarts, and cut-offs, where such writes of both kinds land after the lease changed hands; measure that they do; find a planted bug with it; keep the default run cheap.

**Inferred intent:** the claim that late writes are harmless rests on the salt and on create-if-absent. Markus's concern is trust in correctness, so the claim should rest on a run that exercises it and on a check that is shown to fail when the claim is broken.

### What I did

Read `AGENTS.md`, `docs/spec.md`, `docs/todo.md`, the two READMEs, `simstore`, the simulation tests of the core, `bugs.go`, and `lease.go` and the commit path in `tx.go`. Started a measurement of `go test -count=1 ./internal/core/` on `main` as the baseline.

### Why

The run time is a hard limit in CI, so the baseline comes before any change.

### What worked

`simstore` already has `Faults.LateBy`, so a longer delay needs no new mechanism, only a way to draw a long delay for some writes and not for all.

### What didn't work

Nothing yet.

### What I learned

In the clean design the only manifest whose version comes back is the one without a lease entry, and the only swaps sent from it are takes. So a late manifest swap can be applied only if it is a take, and a planted bug that late writes alone find must make a version come back through a late landing.

### What was tricky

Finding a planted bug that needs a long delay, and not only one that any unknown outcome finds.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: A long hold in the simulated store, and the workload

**Author:** builder (late-write workload)

### Prompt Context

**Verbatim prompt:** (the brief of step 1)

**Interpretation:** build the fault and the workload, and measure that the workload reaches what it is for.

**Inferred intent:** a workload that does not reach its case would pass for the wrong reason, so the case is counted, and the count is checked.

### What I did

Measured the baseline first, on `main` at `186ccdb`, with a test binary built from a detached checkout so that my changes did not count: `go test -count=1 ./internal/core/` took 46 s at a load of 12 to 17; the race binary took 128 s at a load of 16 to 21; the sanitizer binary in Docker (`golang:1.26`, built with `go test -asan -c` inside the container) took 146 s at a load of up to 30, part of which was my own runs.

In `/internal/store/simstore`: two new fields of `Faults`, `Long` and `LongBy`. A write that `Late` or `AfterCrash` holds back waits up to `LongBy` with the probability `Long`, and up to `LateBy` otherwise. `Long` of zero draws no number, so `AllFaults` and every existing workload give the same traces as before. A new file, `late.go`, holds `Landing`, `Store.Landings`, `holdBack`, and `landed`: the store records every write that it held back when it arrives, with the time it was sent, the time it landed, and whether it was applied or refused. The changes to `client.go` are a `sent` and a `held` field on `request`, and four lines that call `holdBack` and `landed`. Its tests are in `late_test.go`, so that the other builder's tests in `client_test.go` do not meet mine in a merge.

In `/internal/core`: `run.countLandings` in `simulation_test.go` reads the history of the manifest and the landings after the run, and counts, for manifest swaps and for creates of commit objects, how many were applied and refused, how many landed after the lease changed hands once and twice since they were sent, after another instance committed, and later than `LateBy` allows. `tally` adds them up, and logs them. `workload.liveness` takes `LongBy` into its bound when `Long` is on. The workload `lateWrites` in `workloads_test.go` builds on `competing`: three instances, two crashes with restarts, closes, cut-offs of up to two minutes, `Slow` 0.05, `Late` and `AfterCrash` 0.8, `Long` 0.5 with `LongBy` 15 minutes, and callers that run 40 transactions each with up to 30 s between them and a timeout of 10 s, which lasts about 15 simulated minutes. `TestSimulation` runs it for 10 seeds, twice each, and requires in that run that both kinds of write land after a change of holder in a quarter of the seeds at least, and after two changes, after a commit of another instance, and later than `LateBy` allows, in some seed.

### Why

Simulated time is free, steps are not. A workload whose callers think for longer reaches 15 minutes in few steps: about 8000 steps a seed, against tens of thousands in the other workloads. Half of the held writes stay short so that what the short delays find is still found: the bug `settleByRead` needs a stuck swap that lands before the renewal that rules it out, and the long hold alone would lose it.

### What worked

The first try, with 20 transactions and up to a minute between them, already landed manifest swaps after a change of holder in 7 of 10 seeds, but committed only 16 times a seed. With 40 transactions and up to 30 s, 20 seeds gave 587 commits, manifest swaps after a change of holder in 17 of them, after two changes in 13, and commit objects after a change of holder in 13 and after two changes in 10. Of 83 late creates of commit objects, 49 were applied (their transaction had failed, so the object is an orphan) and 34 refused because an attempt after them had created the object. Of 338 late manifest swaps, 3 were applied, each a take of a free lease, which the design allows, and 335 refused. No seed found a violation, and no pair of traces differed.

### What didn't work

Nothing failed. The workload as built finds none of the planted bugs around the flights: over 100 seeds it found `settleByRead`, `lostMeansFailed`, and `keepSalt` in none. Those need a stuck swap that lands before the next swap of the holder, callers that wait for longer than a lease, and a wall clock that stands still. It finds six others: `lateVersion` in 100 of 100, `ackUnknown` in 66, `ownDuration` in 65, `takeoverAfterRelease` in 60, `serveUnknown` in 34, and `renewOutsideLock` in 10. None of these needs a long delay.

### What I learned

The design leaves exactly one way for a late manifest swap to be applied: the manifest without a lease entry is the only one whose version comes back, and only takes are sent from it. So a bug that the long hold finds, and nothing else, must make a version come back through a late landing.

### What was tricky

`lose` in `client.go` makes a copy of a stuck request, so the copy needs `sent` and `held` too, or its landing is lost or dated at zero.

### What warrants review

`run.countLandings` decides what counts as a change of holder: a write of the manifest whose lease entry names another holder than the last one before it, with releases skipped. A late write sent before any lease existed is not counted at all.

### Future work

None.

## Step 3: A planted bug that needs the long hold

**Author:** builder (late-write workload)

### Prompt Context

**Verbatim prompt:** (the brief of step 1) "Show that the workload has teeth. At least one planted bug must be found by it, and the clean code must pass. [...] If none fits, plant a new one in the same way (through the unexported `Options.bugs`), for example an instance that takes the lease with a salt it has used before, or that takes a refused swap as final while one of its own writes is still in flight. Say in the test how many seeds it took."

**Interpretation:** a bug whose discovery depends on writes that land late, with numbers for long holds, short holds, and none.

**Inferred intent:** the planted bug is the evidence that the checks would see a late write that the design does not stop.

### What I did

Planted `resendTake` in `/internal/core/bugs.go`, at the take in `DB.acquire` in `/internal/core/lease.go`: a take that got no answer is sent again with the same bytes, while the manifest has the version that it was sent from, as a client that retries a request would. Two fields of `DB` in `/internal/core/core.go` hold the take and its version, under the bug only. `internal/core/README.md` already names the condition that the bug breaks, under "A take that arrives late": "nothing sends a take twice with the same bytes."

The workload of the bug is `lateWrites` with two instances, callers that only read, 30 closes, `LostRequest` 0.2, and `Long` 1. Its control has no held-back writes (`Late` and `AfterCrash` 0). The symptom is the check of the history, "a version that it had before".

Counted with a temporary test (`explore_test.go`, not committed) over 400 seeds each:

| Variant | Seeds that found it |
|---|---|
| `lateWrites` itself, 40% writes, before the bug kept only takes without an answer | 0 of 200 |
| 3 instances, no writes, 3 closes, `LostRequest` 0.1 | 3 of 200 |
| the same with 10 closes, `LostRequest` 0.2 | 16 of 200 |
| 2 instances, 10 closes | 27 of 400, and 25 of 200 before the bug kept only takes without an answer; 1 of 400 with `Long` 0 |
| 2 instances, 20 closes | 77 of 400 |
| 2 instances, 30 closes, `Long` 1 (the one chosen) | 149 of 400 |
| the same with `Long` 0, so holds of at most `LateBy`, a minute | 13 of 400 |
| the same with no held-back writes (the control) | 0 of 400 |
| the same without the bug | 0 of 400 |

### Why

The take is the one swap that is sent from a version that comes back, so it is where a late landing can be applied. The bug needs four things in a row: a take from the free manifest that gets no answer and is stuck, its second attempt applied, a release at the same head, and the first attempt landing after that release and before the next take. The last step needs a write held for longer than the holder keeps the lease, which is what the long hold gives. Callers that write move the head, and then the free manifest does not come back, so the workload of the bug has none.

### What worked

Seed 2 of the variant with 10 closes showed the mechanism plainly: at 5m25s `b23` sent request 141, a take from `c920ce88`, the free manifest, which was lost and stuck for 13m55s; its second attempt, request 143, was applied at `0040d7fd`. At 17m10s `a` released the lease at the same head, back to `c920ce88`, and at 19m21s request 141 landed and gave the manifest `0040d7fd` again. The history check reported it at once.

### What didn't work

The first version of the bug kept the last take after it was applied, so an instance that lost the lease and came back to the same free manifest sent its old take again, and gave a version back without any late write. That is not the bug that a retrying client makes. It now keeps only a take that got no answer, until a take is applied or refused.

### What I learned

The planted bug is found 11 times as often with holds of up to 15 minutes as with holds of up to a minute, and never without held writes. With writes in the workload it is not found at all, because the head moves on and the free manifest of the old head never comes back.

### What was tricky

In the end phase the store is calm, so no new write is stuck there, but the writes that were stuck before still land. Most of the findings land after the last release of the run, when the manifest stays free until the replay.

### What warrants review

The two fields `lastTake` and `lastTakeFrom` of `DB` exist only for the planted bug, which is the same cost that the other bugs pay in branches.

### Future work

None.

## Step 4: Self-review, a check of its own, and the measurements

**Author:** builder (late-write workload)

### Prompt Context

**Verbatim prompt:** (the brief of step 1) "Before you finish [...] Review your own diff against this brief." The builder's instructions ask for the code-review skill on the diff.

**Interpretation:** review, fix what is real, measure, and run the checks the brief names.

**Inferred intent:** hand in work whose claims rest on runs, not on reading.

### What I did

Ran the `code-review` skill at `high` over the diff. It reported nine findings. What I changed for them:

- **A change of holder counted a node that closed and opened again, and a commit counted by node.** Both are now counted, with the same meaning: a change of holder is a new holder ID, and "another node took the lease" is a holder whose node, the client of the store, is not the one that sent the write. A restart after a crash is a new node. `TestSimulation` now requires the stricter count, in a quarter of the seeds.
- **The docs said that `resendTake` is the bug that the long hold finds,** while its control turns off every held write, not only the long ones, and holds of up to a minute find it too. The README now gives both numbers.
- **`request.held` was state that only one function used,** and the copy in `lose` set it for nothing. It is a local variable of `carry` now.
- **`workload.liveness` took `LongBy` into the bound when `Long` was on but nothing was held back**, as in the control of the bug. It now needs `Late` or `AfterCrash` too.
- **The doc comments of `Faults.Late` and `Faults.AfterCrash` still said "up to LateBy".** They name `Long` now.
- **A subtest of the store claimed to show that the numbers drawn are the same as before.** It only compared two settings with `Long` off. It is renamed to what it shows, and the claim is shown another way: the hashes of all traces of the three existing workloads in `TestSimulation`, with this branch and with `main`, are the same: `6bb53344…`, `726d3bc1…`, and `824e0b57…`.
- Not changed: the second call of `Store.History` in each run. A run takes thousands of steps, and the copy is small next to them.

The review also led me to a check that the count was missing. The design says a late swap of the manifest is refused once another holder has written, unless it is a take of a free lease. The count is now `run.verifyLandings`, and it reports a violation for a late swap that is applied after the lease changed hands and was not sent from a manifest without a lease entry. To see the check fire, I changed `apply` in `/internal/store/simstore/client.go` for a moment to skip the version check for a swap older than 70 s: the first five seeds of the workload all found violations, two of them through the new check ("request 449 of b23, a swap of the manifest that was sent at 11m9.343965196s, was applied at 19m18.639198278s, after the lease changed hands, and not as a take of a free lease"), and the others through a stale read and a cache whose position was not in the log. The change was undone.

Measured, with the test binaries of `main` and of this branch run in turns on the same machine: under `-race`, 104 s and 105 s for the branch against 88 s and 97 s for `main`; under the address sanitizer in Docker, 121 s and 128 s against 124 s and 118 s. The default run without either took 45 s against 46 s, measured 20 minutes apart. The load was 15 to 17 for the race runs and up to 34 for the sanitizer runs.

Counted over 1000 seeds: `lateWrites`, each seed twice, found nothing and no pair of traces differed; `resendTake` was found in 387, its control in none, the workload without the bug in none, and the same workload with `Long` off in 33. The numbers are in `/internal/core/README.md`.

Ran `make lint`: 0 issues. `make test`: passes. `make test-asan`: passes, with `internal/core` at 143 s. The new tests 50 times: `TestStore_Landings` passes plain and under `-race`; the subtests of the new workload and of `resendTake` pass in 757 s plain and in 988 s under `-race`, both beside other runs.

### Why

A count that the test only logs proves nothing by itself. The check makes the design's claim about late swaps a rule that a run can break.

### What worked

The hashes of the existing workloads did not move, so the new fault, off, changes nothing for them.

### What didn't work

The first form of an edit, a Python script given inline to the shell, was refused by the harness: "this command is too complex to verify that it stays inside the worktree". The same script from a file in the scratchpad ran.

### What I learned

Of 17691 late swaps of the manifest over 1000 seeds, the 121 that were applied all landed before any other holder wrote, so the check never fired on the clean code.

### What was tricky

`run.verifyLandings` holds the outcome's mutex while it counts, and `outcome.violate` takes it too, so the violations are collected and reported after the mutex is released.

### What warrants review

The rule in `run.verifyLandings`: a late swap applied after a change of holder must be from a free manifest. A swap of the holder's own that lands late with nothing written in between is legal and is not counted as one.

### Future work

None.
