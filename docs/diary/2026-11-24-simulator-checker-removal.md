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
