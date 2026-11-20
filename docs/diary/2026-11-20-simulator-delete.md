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
