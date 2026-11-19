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
