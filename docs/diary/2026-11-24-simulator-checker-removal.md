# Diary: removing the invariant checker of the simulator page

The simulator page, `site/simulator/`, runs the engine of `internal/simulator` in the visitor's browser, and that engine had a checker of its own (`/internal/simulator/check.go`) that checked six invariants over each run and reported violations as events, which the page turned into a log line, a sound, and a dance move. Markus: "I'm not sure we need the checker." "It won't actually show to any users ever if we do our job properly, right?" and "Less code to carry mostly, especially for something that's essentially dead code". The storage code's verification is the simulations of `internal/core` and `internal/engine`, which this work does not touch. This diary is of the builder that removes the page's checker.

## Step 1: The brief

**Author:** builder (simulator checker removal)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> 1. The checker is gone from the Go code: the type, its file, its tests, its calls, the `check` event, and the counts of violations in the state and the summary. Nothing is left that only served it. The formats of events, state, and summary simply change; `AGENTS.md`: nothing is in use yet, so no compatibility.
> 2. What the app knows about its keys stays, in a small type of its own with a name that says what it is, holding only what choosing keys needs. The workload behaves as before: the same kinds of requests, the same rules for which key a read or a delete picks. [...] Say in your report whether a run of a given seed still makes the same requests as before, and how you compared.
> 3. `run.end` and anything else that existed to run the last checks: keep what still has a purpose and remove what does not. `summary.log`, which came from the checker: find what reads it; keep it by simpler means if something does, remove it otherwise.
> 4. Tests keep their worth. A test whose only point was "no violation" is deleted; a test that asserts other things keeps them and loses only the violation assertion. Do not leave a test that asserts nothing. List in your report which tests were deleted and which lost an assertion [...]
> 5. The page no longer knows of the checker: no log line for violations, no excused stale-read line, no `alarm` sound figure and no dance move for it, with their poses, if nothing else uses them [...] `halt()` and `#failure` work as before.
> 6. The site check loses the assertions that existed for the checker and keeps passing; steps that are about other things stay meaningful.
> 7. `internal/simulator/README.md` is true again [...] Say in one short paragraph, where a reader would look for it, that the simulator does not check results and that the storage code's verification is the simulations of `internal/core` and `internal/engine`.
> 8. Report two numbers before and after, measured the same way: the size of `engine.wasm` from `make site` (raw and gzipped), and the count of non-test lines of Go in `internal/simulator`.
>
> Hard boundary: change nothing outside `internal/simulator/`, `site/simulator/`, and the diary. Markus: "And obviously, don't touch the real non-browser checker".

**Interpretation:** delete `check.go` and `check_internal_test.go`; move `known`, `deletable`, `gone`, `learn`, `learnEnd`, `pickRead`, and `pickDelete` into a type of their own with their tests; strip the `check` event, `Invariant`, `Excused`, and the counts from the trace, the state, and the summary; strip the page's alarm tier, sound, and dance; and rewrite the README.

**Inferred intent:** less code to carry, and no claim of checking on the page that the project's real verification does not stand behind.

### What I did

Set up the worktree: `git reset --hard worktree-agent-a3310990279078fb7` (the walking stick figure) and `git merge main`, which gave the merge commit `ee2a74f`, the base of this branch, `worktree-agent-a5ab501670d23c7ab`.

Measured before anything changed. `make site` gave an `engine.wasm` of 14,559,324 bytes, 3,435,091 under `gzip -9` (3,489,860 under plain `gzip`). The non-test Go of `internal/simulator` is 5,592 lines with its subpackages (`find internal/simulator -name '*.go' ! -name '*_test.go' | xargs cat | wc -l`), 4,841 in the package itself.

To show afterwards that a seed makes the same requests, I wrote an uncommitted test, `/internal/simulator/zz_fingerprint_test.go`, which runs three requests (every fault with a crash and paused clocks; a heavy workload with half of the writes deletes; the client on a standby) for four seeds each, and writes the hash, the steps, the counts of calls and requests, and every event except those of kind `check` as deterministic JSON lines into a file of the scratchpad. The run before the change gave 47,807 lines, 21,279 of them events of calls.

Read `check.go` and every call of the checker. What decides the picks needs only how each write ended and, for a stored bucket, the rows at the head of its log; nothing in the picks reads the checker's record of the log.

### Why

The numbers before have to be measured on the same tree as the numbers after, and the fingerprint has to exist before the change to be any use.

### What worked

The fingerprint test ran in under three seconds.

### What didn't work

Nothing so far.

### What I learned

`Summary.Log` came from the checker (`logAtEnd`, the length of the chain at its last checks), and only one assertion in `/internal/simulator/state_test.go` reads it, against the state's own `Bucket.Log`. The demo page of `/internal/simulator/engine/index.html` prints `summary.violations` in its line at the end of a session.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: The keys of the app, the checker out of Go, and the page

**Author:** builder (simulator checker removal)

### Prompt Context

**Verbatim prompt:** (the brief of Step 1)

**Interpretation:** three commits: move what the app knows of its keys into a type of its own while the checker still runs, so that the fingerprint can show that the move alone changes nothing; then delete the checker and everything that only served it; then the page and the site check together, because the site check fed `check` events to the page's `teller` and listener and must change with them.

**Inferred intent:** small steps that each leave the tests and the site check passing.

### What I did

`a441d20` adds `/internal/simulator/keys.go` with `knownKeys`: `known`, `deletable`, `gone`, the constants `recentKeys`, `recentDeletes`, and `goneReads`, and the methods `stored` (the rows at the head of a stored log, in the order of their inserts, as `checker.seed` learned them), `learn`, `ended` (what `checker.learnEnd` did), `pickRead`, and `pickDelete`, all unchanged in their rules. `run.keys` is a value of it; `newCall` picks from it and `callEnded` tells it how an insert or a delete ended. The two pick tests and the seed's assertion of `known` moved from `check_internal_test.go` into `/internal/simulator/keys_internal_test.go`, `TestKnownKeys`, with one new subtest: only an acknowledged insert makes a row known. The fingerprint after this commit was byte for byte the one before.

`4a6cb56` deletes `check.go` and `check_internal_test.go`; the `check` event and the fields `Event.Invariant` and `Event.Excused`; `State.Violations` and `State.Excused` and their fold; `Summary.Log`, `Summary.Violations`, and `Summary.Excused`; `run.check`; the type `txn` and `call.txn`; the calls at manifests, commit objects, reads, the box's selects, the end of a write, and the seed; the fields `wallStoppedEver`, `monotonicStoppedEver`, and `wallBackEver` of `process`, with `process.bothClocks`, which only excused stale reads; and the parameters that only fed the checker (`ctx` of `run.end`, `run.callEnded`, and `run.object`, and `p` of `run.boxStatement`). `short` moved to `bucket.go`, its last user. `run.end` keeps its other work: it sets `ending`, stops the recorder, and starts the monotonic clocks so that the run can end. The demo page of `/internal/simulator/engine/index.html` no longer prints `summary.violations`, and a comment of `/internal/simulator/engine/simulator.js` no longer promises the last checks. The fingerprint after this commit was again byte for byte the one before.

`e416081` takes the checker out of the page: the `check` case of `teller` in `/site/simulator/run.js`, with the excused stale-read line and the tier `alarm` in its comment; the figure `alarm` of `/site/simulator/sound.js`, the note G6 that only it used, its case in `listener`, and the exception for it in `player.play` (it skipped the limits of voices and of `PER_SECOND`, and faded the oldest voice to make room), with the comments on its louder peak; the move `alarm` of the dancer in `/site/simulator/page.js`, its poses `panic` and `jump`, and the `up` of a pose, which only `jump` used; and the CSS of `li.alarm` in `/site/simulator/index.html`. The box `#failure`, whose class is `alarm`, and `halt()` stay: neither played the sound or used the tier. In `/internal/simulator/sitecheck/check.html` the step of the tiers of a stale read and a violation goes, the sounds lose their two `check` events and `alarm`, the burst is measured once without ticks and once with, against 0.06 only, and the two assertions of `s.violations === s.excused` go; the one after the reload becomes an assertion that `a` serves, so that the step still reports how long the catch-up took.

### Why

Requirement 1 asks that nothing be left that only served the checker, which is why the clock flags, `bothClocks`, the `up` of a pose, and the alarm's branch in the player went too.

### What worked

The fingerprint, three requests by four seeds, every event but `check` as JSON, compared with `cmp`: identical after each commit. The hash is of the scheduler's text trace and its next random number, and the checker drew nothing from the seed, so the same hash and the same events of calls mean the same requests.

### What didn't work

The first `python3` script of edits for `run.go` stopped at `run.go: missing '\tr.mu.Unlock()\n\ts.Log = r.check.logAtEnd\n\treturn &s'`: `summarize` unlocks `r.rec.mu`, not `r.mu`. Its second run stopped at `missing 'p.wallStopped, p.wallStoppedEver, p.monotonicStopped, p.monotonicStoppedEver = true, true, true, true'`, because an earlier pair of the same script had already rewritten a substring of it. The script writes nothing when a pair is missing, so both runs left `run.go` untouched.

The first full test run failed with `zz_fingerprint_test.go:18: FINGERPRINT`: the uncommitted fingerprint test was still in the package and fails without its variable, as it should. It lives in the scratchpad now and is copied in only to take a fingerprint.

`gofmt -l` listed `/internal/simulator/bucket_test.go` after the second commit: a deletion of a line by its text, with two tabs, left the third tab of a more indented copy in front of the next line. I fixed it with `gofmt -w` and amended the commit, which was not pushed.

### What I learned

The page's player let the alarm through every limit, so the limits of the mix are now simply what the comment says, for every figure.

### What was tricky

Telling what only the checker used: `p.wallStopped` and `p.monotonicStopped` stay, because the `clock` event carries them; the `...Ever` flags went. The `call.commit` field stays, because the box's event names its commit object.

### What warrants review

That `callEnded` tells `knownKeys` of exactly the calls that `checker.writeEnded` learned from with `learnEnd`: inserts and deletes of the workload, not reads and not the box, whose writes `learnEnd` ignored.

### Future work

None.
