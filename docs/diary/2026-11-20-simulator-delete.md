# Diary: deletes in the simulator

The simulator on the site, `internal/simulator` served by `site/simulator`, runs inserts and selects on `create table barcodes (code integer primary key) strict, without rowid`. When SQL came to the simulator, Markus said: "we'll add delete once that's done." `delete` is built in the engine (slice 8, #91). This diary is of the builder that adds `delete from barcodes where code = ?` to the simulator's requests.

## Step 1: The brief, and the engine side

**Author:** builder (simulator delete)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> The simulator's requests gain `delete from barcodes where code = ?`, as a visible request beside insert and select. Not `update`. No changes to the Go code outside the simulator; one simulator and one code path; no planted bugs; the page keeps its design, with few words.
>
> - Which requests: by shares from the seed; keep the share of writes against reads, and make about one write in four a delete (an option beside `workload.writes`). A delete removes one barcode that the application knows is in the store, chosen from the seed, not always the newest, and leaves the newest key alone when it is the only one. While nothing can be deleted, the call is an insert.
> - Transactions and outcomes as for an insert. A delete whose outcome is unknown leaves its barcode in limbo: neither selected nor deleted again.
> - Selects: as now, plus about one in eight for a barcode whose delete was acknowledged recently (one of the last ten or so), expecting no row. The event says what the application expects.
> - The checker: a commit object carries the keys it deletes; the state at a position is inserts minus deletes. `acknowledged`, `not_committed` and `once` for deletes as for inserts; `state` and `fresh` with deletes, minding unknown outcomes. Fast and slow path in agreement, with a test; constructed traces for an acknowledged delete missing from the log, a delete applied twice, a deleted row that a later select returns, a row missing that was never deleted.
> - After a refresh: the known barcodes are the state of the stored log. If the stored shape changes, raise the simulator's part of the version.
> - Page: log lines, commit objects, the Requests strip and the cards count deletes; sound: a delete that commits is a scanner voiding an item, two-part square-wave figure, quieter than the insert. Budgets and mix limits stay.
> - Tests natively and in Chromium, the cost of a fake minute measured at 0 h and at the cap, README true after the change.

**Interpretation:** a third op in the workload, a checker whose model has deletes again, and the page and sound for it.

**Inferred intent:** the visitor sees an application that also removes data, and the checker proves that a deleted row stays deleted through crashes, replays, and reloads.

### What I did

- Merged `origin/worktree-agent-a6701f8212f3ee0a6` (the persistent bucket, PR #96) into this branch first, as the brief said.
- `request.go`: `workload.deletes`, 0 to 100, default 25: the share of write transactions that delete. `opOf` in `run.go` takes the op from one number of the seed: `u%100 < writes` for a write, then `u/100%100 < deletes` for a delete. One draw, as before, so the number of draws per call does not change.
- `run.go`: `call.write` became `call.op` (`write`, `delete`, `read`); events of a delete have `op: "delete"`, `sql` of the delete, and its key in `args`, and no `value`. `write` runs an insert or a delete in `Begin`, the statement, `Commit`, with the outcomes as before. `readCommit` reads `DeleteRows` of `barcodes` into `CommitObject.Deletes` (`deletes` in JSON). A read's events have `expect`, `row` or `none`.
- `check.go`: a write is a `txn{key, delete}`. A key is inserted by one transaction (its value from the counter) and deleted by one at most, which is what lets the checker tell the transactions apart. What the application knows moved into the checker beside the log: `known` (ordered, for the newest picks), `deletable`, and `gone` (the last 10 acknowledged deletes). `pickDelete` takes a key uniformly from `deletable` and removes it from `known` at once, so no read selects it while the delete runs; it refuses while fewer than two keys are known. `learnEnd`: an acknowledged delete goes to `gone`; `unknown` and `lost` leave the key in limbo; any other end puts the key back in `known`, but never in `deletable`. `pickRead` takes one read in eight (`goneReads`) from `gone`.
- The fast path keeps `deleted` beside `inserted`; `live(k, at)` is inserted before `at` and not deleted before it. `extend` goes slow on a second insert, and on a delete of a row that is not live. The slow path's `fresh` is now by position for every write, also of the read's own key: `writesAt(position)` gives the inserts and deletes of the ancestors. `final` counts each `txn` in the log, and `once` also reports a delete of a row that the log does not hold there.
- `bucket.go`: `readLog` refuses a log that deletes a row that it does not hold; `storageShape` is 2. `checker.seed` makes the acknowledged writes of the stored log, inserts and deletes, and the known rows the state at the head, in the order of their inserts.
- Tests: the checker's table gained the four cases of the brief and more (an acknowledged delete missing, fast and slow; a failed delete in the log; a delete twice; a delete of a row never held; a delete before its insert; a deleted row returned at its position, fast and slow; a read before an acknowledged delete of its key, of another key, excused with both clocks wrong; a delete of unknown outcome that the log holds; a missing row never deleted; two on a stored log that deletes). The agreement test runs every read of five keys at every position also for a log with a delete, acknowledged and of unknown outcome, and an acknowledged insert after it. Two tests of the picks. `run_test.go`'s SQL test checks the commit objects of deletes and every read against what it expects; the bucket test checks that the first session deletes and that no call of the second picks a deleted key. `TestSeed_readLog` refuses three bad logs.

### Why

- **Fresh by position for the own key.** Before, `fresh` checked the read's own key by its row. With deletes, "saw the insert of k" can no longer be told from the answer (no row is right after the delete). The position says it exactly, and `state` already compares the answer with the state at the position. A read at a right position with a wrong answer is now a violation of `state` alone, and the fast and the slow path still agree exactly.
- **A key is deleted once at most.** A delete that ended `not_committed` is checked to be absent from the log at the end. If the same key could be deleted again later, the log could hold a delete of the key from the second transaction, and the checker could not tell which one. So a key that a delete picked is never picked by a delete again; it stays selectable if the delete did not happen.
- **The shape raised to 2.** An old engine cannot read a log with deletes, so its bucket must be dropped cleanly rather than refused for a reason that looks like damage.

### What worked

`go test -tags s4ql_browser -shuffle on ./internal/simulator/...` passed after two fixes (below), in 49 s. Every existing run test with every fault, the sessions against the batch runs, the bucket tests with crashes and lost answers, and the steps-per-minute check in `TestEngine_live` now run with deletes, since the default share is 25, and found no violation.

### What didn't work

- `TestRun_faults/should_show_writes_held_back_after_a_crash` failed: `run_test.go:480: an event request/held`. With seed 1, no write is on its way at the crash at 20 s any more. Seeds 2 and 4 to 11 show one; the case uses seed 2.
- The agreement test first failed with `check_internal_test.go:408: a read that the fast check passes has no violation`: with the delete of unknown outcome as the last commit object, a read at the position before it passes the fast check (the fast check does not look at writes of unknown outcome), and `final` then reports `fresh` for it. That is right; the test needed an acknowledged write after the unknown one, as the log without deletes has (insert 3 after insert 2). The variants now have a fifth commit object, which inserts 4, acknowledged.
- `golangci-lint run` answered `Error: parallel golangci-lint is running`: another agent's lint. To be run again before the report.
- A shell here-doc into the scratchpad was refused by the worktree guard ("this command is too complex to verify that it stays inside the worktree"); scripts go through the Write tool.

### What I learned

The core's replay refuses a delete of a row that is not there (`internal/core/replay.go`), so the engine writes a `DeleteRows` only for a row that it found. A delete that finds no row commits an empty transaction, which the checker then sees as an acknowledged delete that the log does not hold. For this workload that is right: it deletes only keys that it knows are there.

### What was tricky

The bookkeeping of the application's keys has to happen once per call, also when a call of the client ends with `lost` after its task ended it: `callEnded` already guards that with `call.ended`, so `learnEnd` sits behind it in `writeEnded`.

### What warrants review

- `checker.learnEnd` and `checker.pickDelete`: the rule that a key is deleted by one transaction at most is what makes `txn` unique.
- The slow path's `fresh` by position, in `checker.readEnded`, against the comment of `invariantFresh`.

### Future work

None yet; the page and the sound come next.

## Step 2: The cost, the page, the sound, and the README

**Author:** builder (simulator delete)

### Prompt Context

**Verbatim prompt:** (as in step 1: the page, the sound, the cost of a fake minute, and the README)

**Interpretation:** measure that a fake minute costs what it did, then show deletes on the page with few words and the sound of a voided scan.

**Inferred intent:** a visitor sees and hears the deletes, and nothing else on the page changes.

### What I did

- **Cost.** A throwaway test (not committed) started a session with the page's settings and seed 7, timed 30 advances of one fake minute at about 0.5 h, advanced by 10 fake minutes until the log had 2,000 commit objects, and timed 30 more. I ran it on this branch and on the base (`git checkout 6ed8e33 -- internal/simulator` after a commit, and back), as prebuilt test binaries alternating three times each, natively.
- **Hash.** A throwaway batch run (seed 11, 10 fake minutes, the client and `standby`, faults, a crash and a restart of `a`) gave `a35f81952929f552...` with 9,143 steps natively and in Chromium through `make test-browser`; its summary had 59 committed deletes, 2 `not_committed`, 18 `down`, 7 refused, and no violation.
- **Page.** `run.js`: `commitKeysText` labels a commit object of a delete "delete" and its key, where an insert says "key"; a read that expects no row and finds none says "0 rows, as deleted"; a delete that gets no answer has its outcome unknown, as a write has (`e.op !== "read"`). The log line of a committed delete needed nothing: the teller already prints the statements of the commit object, so it says `a commits delete from barcodes where code = 5791086233578 as b47838e9.` `page.js` counts committed deletes in the strip and on the cards ("82 inserts, 22 deletes, 153 selects"). `index.html`: a cell Deletes in the strip, four columns of counts where there were three, padding of 10 px in the cells below 720 px, and two rows of counts below 380 px.
- **Sound.** `sound.js`: a tick `void`, two notes of a square wave, the note of the key for 35 ms and an octave down for 30 ms, 6 ms apart, at level 0.7 against the scan's 0.85. `figure` now keeps the ratios of a figure's notes to its first note when it is given a pitch, which leaves the one-note ticks as they were.
- **Site check.** Unit checks of the delete's log lines, of the label of its commit object, of a delete that gets no answer, of its tick at the note of its key, and of the void's level against the scan and the select; the burst gives the void every pitch of the scale.
- **README.** A writer sub-agent updated `/internal/simulator/README.md` from a brief: the workload, what a delete's end teaches the application, `expect`, `deletes` on a commit object, the bucket's shape 2 and its new refusal, the invariants, the tests, and the cost. I read its diff; it matches the code. It noted that a delete of a caller whose process crashes never ends and leaves its key in limbo, which it wrote down, and that "both answers legal" holds only for a delete whose outcome is still open: for one of unknown outcome that the log does not hold at the end, `state` still wants the row. Both are right.

### Why

- Below 380 px four cells of 13 px caps no longer fit: "INSERTS" broke inside the word at 390 px with the old padding, and at 360 px and 320 px with the new. Two rows of counts there keep every word whole; 390 px and up keep one row.

### What worked

- Cost: the steps of a fake minute are the same, 431 at 0.5 h and 433 at the cap (2,324 commit objects) against 425 and 431 on the base. The wall time was noise from other agents on the machine: medians of three runs 27.6 and 27.5 ms here, 24.6 and 34.7 ms on the base, with single runs from 18 to 104 ms on both. A CPU profile of the whole probe: 3.77 s here, 3.91 s on the base, both mostly in the core's file operations and cgo.
- Sound levels, rendered offline in Chromium by the site check: a void peaks at −38.0 dBFS at the loudest note, below the scan at −36.3 dBFS and above the select at −42.0 dBFS; the error tone −41.0 dBFS, a figure −34.9 dBFS. The burst with every tick, the void among them, peaks at −26.0 dBFS (limit −24) and −21.7 dBFS with the alarm (limit −20), the same as before, with 26 ticks in 3 s.
- Screenshots at 1440 px and 390 px of the strip, the log with deletes and reads "as deleted", and the commit objects labelled "delete"; at every width from 320 to 1440 px each label and count of the strip is one line and the page is no wider than the window.

### What didn't work

- `make site-check` failed in 3 of 9 runs with `FAIL a reload keeps the database, its name, and its 93 commit objects, and the time goes on from 11:00.0`. I made the message say every part of the condition, and the next failure showed it: `660017.3810030001 to 660017.381003 ms of history`. After a reload the page's history is the offset of the seed plus the time of the session, a sum of floats that can come out one rounding below the sum that was stored, and the check required `>=`. That check came with the persistent bucket (#96), not with this change. It now allows 1e-6 ms; two runs of `make site-check` (four passes of the check) passed after it.
- Element screenshots of `#requests` at 390 px came out under the sticky bar; a style that made `.bar` static for the shot fixed it.
- `timeout 900 make ...` failed with `(eval):1: command not found: timeout` (macOS); ran without it.

### What I learned

The teller of the log builds a commit's line from the statements in its commit object, so a new statement in the workload shows in the log with no change to the teller.

### What was tricky

The cards' "Served" line wraps to two lines at 390 px once the counts grow ("82 inserts, 22 deletes, 153 selects"). It wraps between words, so I left it.

### What warrants review

- `/site/simulator/index.html`, the strip's media queries at 720 px and 380 px.
- At 320 px the heading "Requests" touches "To" in the strip's first row; that row is unchanged by this branch.

### Future work

None that this change needs.

## Step 3: Self-review and checks

**Author:** builder (simulator delete)

### Prompt Context

**Verbatim prompt:** (the brief's "Checks before you report", and the builder's self-review)

**Interpretation:** review the diff with competing reviewers, fix what matters, and run every check of the brief.

**Inferred intent:** hand the lead a branch that can be merged as it is.

### What I did

Ran the code-review skill at medium over `6ed8e33...HEAD`. It found no bug that must be fixed, and six small things:

1. The SQL test required every read that expects its row to find it, but a delete of the key can start while the read runs and commit before the read fixes its state. Fixed: such a read may find no row if its key's delete committed.
2. A write that ends `down` reads "write outcome unknown" on the page. Left: `run.js` says on purpose that `down` and `lost` look the same from outside the app.
3. A delete that ends before its commit (`refused`, `abandoned`, ...) is not checked to be absent from the log. Left: the same holds for inserts, and the README says so.
4. `pickDelete` copies `known` once per delete. Left: a few thousand keys at the cap, and the measured steps and times did not move.
5. A commit object that deletes and then inserts the same key would be read as insert then delete. Left: the workload never writes one.
6. After a reload no read selects a key deleted before it. Left: the brief accepts that.

Checks: `go test -tags s4ql_browser ./internal/simulator/...`, also with `-race`, and the new and changed tests with `-race -count=20`; `make check-browser`; `make test-browser`, all seven packages, with no hang; `make lint` (0 issues, simlint clean); `make site`; `make site-check`.

### Why

The reviewers saw the checker's delete handling cold, which is where a mistake would cost the most trust.

### What worked

Every check passed. The same batch request gave the same hash natively and in Chromium (step 2).

### What didn't work

`make test-browser` took about 10 minutes, most of it `internal/core` (363 s); nothing hung.

### What I learned

The reviewers traced the fast and the slow path of the checker with deletes and found them in agreement, as the agreement test says.

### What was tricky

Nothing beyond the steps above.

### What warrants review

- The new paragraph of the README on what a delete's end teaches the application.

### Future work

None.
