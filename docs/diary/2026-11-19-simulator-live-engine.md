# Diary: A live session in the engine of the simulator, and new actions

The engine of the simulator, `internal/simulator`, runs a request to its end and returns the whole trace, so that a run could be replayed from a link. Markus has dropped that ("random seed, no link. Doesn't need to be replayable") and asked why there is a time limit at all: "Just keep going. Refreshing the page means starting over." He also asked for new actions on each instance: "Crash, Stop, Hang network, Disconnect network, Hang disk, Error disk, and clock jumps forward and backward and pause." This is the engine side. A later builder changes the page.

## Step 1: The brief

**Author:** builder (simulator live engine)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> Part 1: live sessions. Add a live mode to the engine: a session that has no end. The page starts a session (two instances by default, the workload on, no random faults, a seed). The page asks the engine to **advance** fake time by some amount; the engine runs the simulation that far and returns the events of that stretch. Between two advances fake time stands still, however long the visitor waits. The page sends an **action** for an instance (or for S3); it takes effect at the current fake time. The page can ask for the **state** now. The cost of advancing one fake minute must not grow with the age of the session, and the engine must not keep the whole trace: keep the state folded as you go. Measure and report what still grows.
>
> Keep the existing batch API (`run`, `state`) working as it is. **A live session that receives the same actions at the same fake times as a batch request's schedule gives the same events and the same trace hash.**
>
> Part 2: the actions. Crash and restart; Stop (check that the trace shows the clean release of the lease); Hang network; Disconnect network; Error disk, through `faultvfs`; Hang disk, which may not be feasible; Clock: jump the wall clock forward and backward, pause both clocks and resume them. For each action, report what the real code does under it, from runs.
>
> The one change to `internal/core` that is allowed: the VFS option as a field of `core.Options`. No change to `internal/sim`, `internal/store/simstore`, `docs/spec.md`, `site/`, or `README.md`. Tests natively and in Chromium; no test may depend on timing.

**Interpretation:** a second mode of the engine in which one bubble lives across many messages, driven by a controller task that waits for the page between advances; new actions in the engine's own code, usable in a batch schedule and in a session alike; and an honest account of what grows and what the code does.

**Inferred intent:** a page on which a visitor watches the real code run for as long as they like, and breaks its environment in the ways Markus listed, with every claim of the page backed by the code's own behaviour.

### What I did

Read `AGENTS.md`, the README and the code of `internal/simulator`, the diary of the engine, `internal/sim` (`sim.go` and its README), `simstore`'s client, `faultvfs` in both builds, `vfs.js` and the README of `internal/sqlite/browser`, and the tests of `internal/core` that use `faultvfs`.

Found that `core.Options.VFS` is already a field on `main`, from commit `192cba6` ("Make the VFS of the cache a field of `core.Options`, as the other options are"), so the brief's one change to `core` is done.

### Why

The plan, from the reading:

- **The mechanism.** The first task of the run, `main`, becomes a controller: it sleeps through the scheduler for each advance, and between advances it waits for the next command on a channel made outside the bubble. Such a wait is not durable, so `synctest.Wait` in the scheduler's loop does not return, no task runs, and fake time stands still. The scheduler sees the controller as the task that runs, as it sees `main` in a batch run while it does an action.
- **Equivalence.** A batch run's `main` does `sleepUntil(at)` and then the action, for each action. A session that advances to the same times and sends the same actions makes the same calls of the scheduler, so it takes the same steps and draws the same numbers. An advance whose end has no action wakes the controller once more than a batch run would, which is one step and one draw from the seed, so from there the run takes another turn.

### What worked

Nothing to run yet.

### What didn't work

Nothing yet.

### What I learned

- `sim` keeps every line of its text trace in memory, and `simstore` keeps every applied write with its content and a handle of every task that carried a request. Neither can be changed here, so they grow with a session.
- A crashed instance's tasks stay in the scheduler until the run ends, and `next()` sorts the names of every parked task at each step, so each crash makes every later step a little dearer.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

See the later steps.

## Step 2: The session, the actions, and the measurements

**Author:** builder (simulator live engine)

### Prompt Context

**Verbatim prompt:** (continues from Step 1)

**Interpretation:** build what Step 1 planned, keep every batch run byte for byte what it was, and measure in Chromium.

**Inferred intent:** a page builder who needs nothing but the README to switch the page to a session.

### What I did

- `live.go`: the session. `startSession` runs a bubble on a goroutine of its own; its first task, `serve`, is the controller. The commands are `start`, `advance` (`by` or `to`), `act`, `now`, and `end`, and each answers with a `Live`: the events since the last answer, the time, the steps, and on `end` the summary and the hash. `handoff` holds the channels, because `simlint` forbids channel waits in the engine, and it is exempt with its reason.
- `state.go`: `StateAt` is now a `fold` that takes one event at a time, forgets a request once its caller has its answer and the store side has ended, and keeps the log as it grows. A session folds as it records, and keeps no events.
- `check.go`: a fast path that checks a read and an acknowledged write without walking the log, kept while nothing has been found wrong; the first violation turns it off, and from then on the checker does what it did before. A read checked fast keeps three numbers, not the keys it saw.
- `trace.go`: the recorder counts the summary as it goes, and in a session hands its events over and forgets them.
- `run.go`: the actions `hangNetwork`, `disconnectNetwork`, `failDisk`, `pauseClocks`, `resumeClocks`, in the engine's client of the store and through a VFS of `faultvfs` for each instance (`disk.go`, a pool across runs); the supervisor opens a database again after an open that failed; `workload.keep`, which trims the table to its newest rows in each write, with `deletes` in commit objects and in the checker's model.
- `simulator.js`, `worker.js`, and the demo page: the session's calls, and a live panel.
- Outside the engine: an exception in `.golangci.yml` so that `internal/simulator` may import `faultvfs`, and the exemption of `internal/simulator/handoff` in `internal/simlint/simlint.go`.

### Why

- **`advance` with `to`.** The answer to `start` comes at a time above 0, because the scheduler delays the start of the first task (1.537405 ms with seed 1, in a batch run too), so an advance by the gap between two actions lands elsewhere than the batch run's sleep. An advance to a time sleeps as `sleepUntil` does.
- **`keep`, trimmed by the oldest keys.** A read scans the whole table, so without a bound the cost of a fake minute grows with the session. Deleting the key `k - keep` in the write of `k` did not bound it: the spare's writes take keys and are refused, so half the keys never delete anything, and the bytes of events per minute grew by half in an hour. Trimming to the newest `keep` rows does.

### What worked

- Every batch run is the same as before, byte for byte except the echoed request, which gains `workload.keep: 0`: a scratch test ran 16 requests (every fault, buggify, crashes, clocks) against a copy of the old package and compared hashes, events, steps, summaries, and the states at five times. All agreed.
- The session against the batch run: the same events, steps, summary, and hash for three seeds, with every action, every fault, buggify, and `keep` 20, natively and in Chromium.
- A fake minute after an hour costs what it did at minute 10: 611 against 626 steps, 442 against 458 events, and 120,319 against 128,781 bytes of events.

### What didn't work

- The first run of the comparison differed from the old package in every request. The cause was the `context.WithCancel` that a disconnect needs: the engine cancelled it when `Get` returned, and the content of a get is read later with the same context, so every download failed. The context now lives until the content is closed.
- `make lint` failed twice: depguard forbids `faultvfs` outside tests ("it makes the files of SQLite fail on demand, which only tests may do"), and `simlint` reported the channel operations and the `go` statement of the session ("a channel send waits outside the host"). The first got an exception, the second the package `handoff`.
- The test that a disconnect ends a request on its way found none at 30 s in a run of one instance; it now takes the time of a request from a run without the disconnect, which is the same up to that time.

### What I learned

- The comparison with the old package is the cheapest strong test of a refactor here: copy the package under another name in the worktree, run both on the same requests, and diff the JSON.
- `sim` keeps every line of its trace, so the Go heap of a session grows by about 12.5 MB per fake hour natively, most of it the trace.

### What was tricky

- The harness refused many compound shell commands and heredocs; edits went through Python scripts in the scratch directory, with a small `rep.py` that applies exact replacements.

### What warrants review

- The fast path of the checker against its slow path: they must find the same violations. With `keep` 0 the batch runs agree with the old checker; the checker's own tests cover the slow path.
- `fold.forget`: a request is dropped from the fold once it is done and nobody waits for it. The argument that nothing lists it again is in its comment.

### Self-review

The code-review skill at medium over `main...HEAD`, and the writer of the README, who checked every claim in runs. What I did with what they found:

- Fixed, found by the writer: a write whose key was smaller than the `keep` largest deleted its own row in `trim`, so its commit object held no insert of its key, which the checker would report as an acknowledged write that the log lacks. A write now never deletes its own row; the new test with `keep` 2 and six callers fails without the fix.
- Fixed: the first violation, also an excused one, made the checker slow for the rest of the run, so a session after `pauseClocks` cost more per fake minute. Only what breaks the model of the log does now: a head that is not a child of the head, an acknowledged write that the log lacks, a key inserted or deleted twice.
- Fixed: `slow` left the reads it converted marked fast; a read with a row twice passed the fast check; an acknowledged write missing from the log put a made-up index into `ackedAt`.
- Fixed: a disconnect did not end the wait of a call whose network hung; it does now, with its own test. The calls of a crashed process are dropped from the instance's calls on the network.
- Fixed: a refused command now answers with only `error`, as the README says; the comments that said the answer to `start` is at time 0; the doc comment of `Workload.Keep`, and its limit is now 10000, since each write reads the whole table.
- Left: `trim` reads the table in each write, which costs as much as a read; it is bounded by `keep`.

### Future work

- A disk that hangs: see the README.
- An option of `sim` to hash its trace as it goes instead of keeping it, and `core.DB.CloseCache` as a normal method, would remove the two largest growths of a session.
