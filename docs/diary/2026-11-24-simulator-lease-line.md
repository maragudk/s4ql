# Diary: the simulator's log says that a waits for the lease after a reload

After a reload of a tab with a stored database, the simulator page starts the session on it, and the new instance `a` waits for the lease that the stored manifest still names before it serves. The log said only "S3 still holds db-…: N commit objects." during the wait, so the page looked broken. This diary is of the builder that makes that line say the wait.

## Step 1: The line, the dead line in `run.js`, and the check

**Author:** lease-line builder

### Prompt Context

**Verbatim prompt:** "You are making a small change to the log of the s4ql simulator page [...] In his words: "Maybe in the 'S3 still holds db-4p2fj6: 72 commit objects.', it should say 'waiting for lease' too?" [...] 1. When the page starts on a stored database and an instance has to wait for a lease from before the reload, that log line also says so [...] 2. It must be true in every case. [...] 3. Find out why the line "learns that its lease from before has not run out yet" in `run.js` does not appear after a reload. [...] 4. The wait itself does not change, nor anything else the simulation does."

**Interpretation:** extend the one line of a reload so that it names who waits, for what, and the lease's length from the manifest; say nothing of a wait when there is none; explain the line of `run.js`; extend the site check.

**Inferred intent:** a visitor who reloads sees why nothing happens for half a minute.

### What I did

Read `acquire` in `/internal/core/lease.go`. An instance that does not hold the lease and finds a lease of another holder ID in the manifest waits until it has watched the manifest stand still for the lease's duration and `margin(duration)` (`max(2s, d/10)`, so 33 s for a lease of 30 s), by its own monotonic clock, rechecking every `Watch` (5 s). It never compares the lease's `renewed` with a clock, so a lease that ran out by the wall clock before the reload is waited out all the same. It sends no event while it waits. The simulator's fold seeds the stored manifest into the state (`seed` in `/internal/simulator/state.go`), and "nothing knows the process that wrote them", so the page cannot know which instance held the lease. Under `standby`, only `a` starts; `b` is `waiting` until `a` serves.

So `storedLine` in `/site/simulator/run.js` makes the line from the name, the count, the manifest of the state at the start, and the instances: each instance that is not `waiting` and whose holder ID is not the lease's waits. `begin` in `/site/simulator/page.js` now fetches the state before it writes the line. The line reads "S3 still holds db-g2ns98: 4 commit objects. a waits for the lease of 30 s from before the reload to run out by its own clock." The count now has its singular ("1 commit object").

The line "learns that its lease from before has not run out yet" (commit `9c6025f`) is said for a `lease` event with transition `lost` whose holder is an earlier holder ID of the same instance. The core sends `lost` only from `lose`, which runs only when the instance believed it held the lease and a read of the manifest shows otherwise. A new process, after a restart or a reload, never believed that, so it waits silently in `acquire`. After a reload the teller's `names` would not know the old holder ID either. The line was written for a restart, checked only with a synthetic event in `unit()`, and is not reachable in the reload case; I left it.

Checks: a unit check of `storedLine` in `/internal/simulator/sitecheck/check.html` with a lease of 30 s and of 12.5 s, no lease, no manifest, a lease under a's own holder ID, and two instances that start. In `persistence`, the check reads the stored manifest from sessionStorage before the reload and expects the line with the wait and the manifest's duration, or without it.

### Why

The length comes from the manifest, so the line stays true for any lease. The line names the lease's length, not the wait's, because the wait adds the core's margin, which the page would have to copy.

### What worked

Red first: `make site-check` failed with "FAIL the check could not go on: TypeError: R.storedLine is not a function". Then `make site-check` passed, both runs ("ok the line of the reload says that a waits for the lease from before the reload, of 30s in the stored manifest: 11:00.0S3 still holds db-fnn82v: 100 commit objects. a waits for the lease of 30 s from before the reload to run out by its own clock."), and `go test -tags s4ql_browser ./internal/simulator/...` passed.

### What didn't work

`playwright-cli eval "..."` in a compound command was refused by the worktree guard ("this command runs a string through eval, which can't be verified to stay inside the worktree"); `playwright-cli run-code` with a function worked. In `run-code`, `document` is not defined at the top level ("ReferenceError: document is not defined"); it is only inside `page.waitForFunction`.

### What I learned

In the browser, after a reload at 0:52.2, `a` took the lease at 1:25.5: 33.3 s, the lease and the margin. The log then says "a takes the lease over from ae273799.", with the old holder ID, because the page has no name for it.

### What was tricky

Saying the wait truthfully when the page cannot know who held the lease, nor whether it ran out: "the lease from before the reload" and "by its own clock" cover both.

### What warrants review

`storedLine` in `/site/simulator/run.js`, and that `begin` in `/site/simulator/page.js` now reads the state before it notes the line. Run `make site-serve`, play a few seconds, reload, Play, and read the log.

A self-review with the code-review skill (low) found two things. One said that `lease.duration / 1000` gives `NaN` if the state's duration is a string such as "30s", as the stored manifest has it. It is not: the state's manifest is the engine's `ManifestObject`, whose `Duration` is in ms (`millis(l.Duration)` in `/internal/simulator/run.go`), as `drawS3` already uses it, and the browser shows "30 s". I added a comment that says the unit. The other: any state but `waiting` counted as waiting for the lease, so a crashed or stopped instance would be named. None is at the start, but `storedLine` now counts only `starting` and `running`. `make site-check` passed again.

The lead then changed the wording, because "by its own clock" and "to run out" read as written for the code's reviewer: the second sentence is now "a waits out the 30 s lease from before the reload." ("a and b wait out …" for two). "Waits out" says that the full length is waited, as `acquire` does. The comment at `storedLine` keeps why it says "the lease" and not "its lease", and that the real wait is the duration and the margin, timed from the first read.

### Future work

- After the wait, "a takes the lease over from ae273799." names the old holder by its ID. It could say "from before the reload".
- A crash and restart of `a` in a session, with `b` not taking over, also waits silently for the lease of the process before the crash; the "learns that its lease from before" line was meant for that and never shows. A line there would need an event from `acquire` in the core.
