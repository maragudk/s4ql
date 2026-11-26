# Diary: hardening the storage core after the adversarial review

The adversarial review of 2026-11-26 (`/docs/research/adversarial-core-2026-11-26.md`) confirmed four faults of `internal/core`, each with a failing test on the branch `adversarial-core`. This task fixes three of them (a takeover on a shared cache, a read of a replaced cache, an overflow in the wait for a lease), adds the cheap hardening that the claims true on paper suggested, and leaves the fourth (two identical commit objects) to the product owner, since it needs a decision on the commit format.

## Step 1: the three faults

**Author:** core-hardening builder

### Prompt Context

**Verbatim prompt:** "Your ONE task: **fix three confirmed faults of the storage core** that an adversarial review found, and add the cheap hardening it suggested." (The brief goes on with the setup on the branch `adversarial-core`, the three faults with what to decide for each, the hardening items, the proof by tests and planted bugs, the night-sized runs, and the docs.)
**Interpretation:** fix faults 2, 3, and 4 at their cause by the spec's rules, turn their `TestAdversarial_*` tests into ordinary tests that fail without the fix, drop fault 1's test from this branch, and show for each fix whether the simulation can reach it.
**Inferred intent:** more trust in the core's correctness, without deciding anything that belongs in the spec.

### What I did

- Reset the worktree to `adversarial-core`, renamed the branch `core-hardening`, and merged `main`. The merge conflicted in `/docs/todo/storage-core-tests.md` on the item about Nightmare mode, and I took `main`'s version, which is newer.
- **Fault 4.** `Options.Watch` is now a year at the most (`withDefaults` in `/internal/core/core.go`), and the wait in `DB.acquire` is `watchWait` in `/internal/core/lease.go`, which compares the time left of the lease with `watch - margin` before it adds the margin, so no sum or difference leaves the range for any positive duration a manifest can hold. `Lease.Duration` is the only duration or time of the manifest that the core computes with: `Renewed` is only written back by the same holder. Tests in `TestLease_duration` in `/internal/core/lease_test.go`: `Open` refuses a `Watch` of more than a year, the scenario of the review with a `Watch` of a year, and a table of `WatchWait` at the edges, among them the largest lease and the largest watch.
- **Fault 2.** `replay` in `/internal/core/replay.go` returns `errCacheMoved` and not `ErrLogBroken` for a cache that is not at the parent of the commit object, after reading it to its end. The walk of a catch-up hands replay each commit object after the one before it, so that mismatch can only mean that the cache moved under the catch-up. `DB.catchUp` in `/internal/core/catchup.go` then starts again from the position the cache records (`catchUpFrom`), and trusts it only if the walk from the head finds it, as for the position that it started from: otherwise `ErrCacheNotInLog`. `fetchAgain` returns `errCacheMoved` at once instead of retrying. The scenario is `TestDB_catchUp` in the new `/internal/core/catchup_test.go`.
- **Fault 3.** The instance keeps `DB.cache`, a `cachePosition` in `/internal/core/read.go`: the position that the write connection last committed or read (`at`), the one it is committing (`next`), and a count of changes (`moves`). `Tx.Commit` calls `DB.moving` before the commit of the cache and `DB.moved` after it, and so does catch-up around each replay and after reading the position. `Reader.Read` takes the value at its first check and again at its second; if `moves` is unchanged, the position of the state must be `at` or `next`, and anything else stops the instance with `ErrLocal`. If `moves` changed, the read rolls back and fixes its state again. Tests in `TestReader_Read` in `/internal/core/read_test.go`: the scenario of the review, now a stop, and a read that fixes its state before a commit and is checked after it, through a `yieldHook` at "core: a read before the check of the lease".
- Planted bug `fixedOnce` (`/internal/core/bugs.go`): the read compares without fixing its state again. The hammer workload finds it in each of its first 4 seeds, and the single workload in 4 of 113.

### Why

- Fault 2: the old holder may still commit its own transaction to a cache it shares, and only that one: its write transaction holds SQLite's write lock from `DB.Begin` (`begin immediate`) to the commit of the cache, so it lands on the position that it began at and names a commit object that is in the log. So nothing changes in what a holder that lost its lease writes, and the fix is in the reader of the cache, replay.
- Fault 3: I chose the position over the identity of the file, because the core also runs in the browser, where `os.Stat` of the path means nothing, and because the position is read in every read already, so the check costs no statement.

### What worked

- Each renamed test fails without its fix, checked by reverting the fix in place: `TestLease_duration` (two subtests), `TestDB_catchUp` (with the old `brokenf`, "the instance has stopped: the log is broken: the commit object 9d2f... has the parent ..."), and the replaced-cache subtest of `TestReader_Read`.
- The read test of the race also fails with `fixedOnce` planted, which shows that the test reaches the case.

### What didn't work

- The first run of the planted bug `fixedOnce` failed with "seed 24 gave another trace when run again": the error of the stop held the path of the cache, which is a temporary directory that differs from run to run. The error no longer names the path.
- `TestDB_catchUp` first panicked with "sim: the context does not belong to a task of this scheduler", because it read through `read(t, b)`, which uses `t.Context()` inside a task of the scheduler. It now reads with the task's context.

### What I learned

- In the scheduler, no goroutine runs between the SQLite commit of `Tx.commitCache` and `DB.moved`, so the field `next` matters only under real parallelism. The window that the simulation reaches is the yield between the read of the position and the second check, which is what the retry is for.
- The simulation cannot reach faults 2, 3, or 4: every node has a cache of its own, no workload deletes the files of a running node's cache, and no workload sets a `Watch` above minutes or writes a manifest that s4ql does not write. The scenario tests stand in.

### What was tricky

- Getting the set of positions that a read may see right without a history: with `moves` unchanged between the two checks, the state is `at` or `next`; with `moves` changed it can be an intermediate one, so the read starts again instead of guessing.

### What warrants review

- `Reader.Read`'s loop in `/internal/core/read.go`, and that every commit of the write connection that moves the position goes through `moving` and `moved`: `Tx.Commit` (also under the planted bug `cacheFirst`) and `catchUpFrom`.
- The restart of `catchUp` and its event, which reports the position it started from and every commit object it applied over all restarts.

### Future work

- Fault 1 stays open for Markus.

## Step 2: the hardening

**Author:** core-hardening builder

### Prompt Context

**Verbatim prompt:** "Each small, each with a test: when the upload of a commit object finds an object already there, check that object's checksum against what was to be written before treating it as the same; at `Open`, refuse a cache at the empty-log position that holds tables, and a cache whose text encoding is not UTF-8; check the limit of key columns in replay as the write path does; check for a stop between rows in `Tx.scan`; correct `docs/todo/s3-client.md` where it says a store that checks no payload hash \"still works\"."
**Interpretation:** five small changes, each red first.
**Inferred intent:** close the claims that were true on paper where it costs little.

### What I did

- `Tx.upload` in `/internal/core/tx.go`: on `store.ErrExists` it reads the object (`Tx.uploaded`, at most the spool's size and a byte) and counts it as the commit object only if the length and the SHA-256 match. `TestTx_Commit_exists` in `/internal/core/tx_test.go` uses a store, `squatter`, that puts other bytes at the key first.
- `conn.setUp` in `/internal/core/cache.go` checks `pragma encoding` before it makes the table `s4ql_cache` (`checkEncoding`), and `checkObjects` refuses a table of the database in a cache at `EmptyLog`. Tests in `TestOpen_cache_foreign` in `/internal/core/cache_test.go`.
- Replay refuses a key of more than 250 columns (`maxKeyColumns`, shared with `checkCreateTable`); a case of `TestReplay`.
- `Tx.scan` in `/internal/core/index.go` looks at the stop between rows; `TestTx_scan` in `/internal/core/index_test.go` failed first with "Expected "1", but got "3"".
- `/docs/todo/s3-client.md` now says what a store without checksums costs.

### Why

Each closes a case that a program other than s4ql, or a collision, would need; none needed a rule of the spec.

### What worked

The tests were red first where the fault could be shown: the scan read all three rows, and the squatter's object was committed.

### What didn't work

- The first version of the read in `Tx.upload` failed the commit at the first mismatch. `TestSimulation_plantedBugs` then failed in the workload of `skipUnreadable`, which damages 200 in 1000 downloads: "seed 0 found something else: [commit of transaction 9057639001382112477: commit: not committed: the object at the key tenant/db/commits/b5/... is not the commit object ...]". A read can be damaged on its way like any download.
- The encoding test first required the file to be unchanged, and failed with "the instance changed the main file of the cache UTF-16le": `conn.wal` puts the file into the journal mode wal before anything else. I left that order alone, because another builder changes `setUp`, and the test checks that no table was made.

### What I learned

The simulation's damaged downloads reach every `Get`, also one that the commit path adds, so every new read of a commit object must take damage as the norm.

### What was tricky

Choosing how often to read again: the catch-up stops after two damaged downloads, which the harness explains with `damagedTwice`, but a commit that fails is a violation in the harness. See step 3 for where it ended.

### What warrants review

`Tx.upload`'s loop with `exists`, and `checkObjects`' second read of the position.

### Future work

None.

## Step 3: self-review

**Author:** core-hardening builder

### Prompt Context

**Verbatim prompt:** "Run the `fabrik:code-review` skill over your diff and fix what matters."
**Interpretation:** two competing reviewers on the diff, then fix what is real.
**Inferred intent:** catch what I missed before the lead reviews.

### What I did

Two reviewers read `git diff main...HEAD -- internal/`. Both found no serious fault in the three fixes, and both checked on paper that every commit of the write connection is bracketed by `moving` and `moved`, that a shared cache gives no false stop, and that `watchWait` cannot overflow. What I fixed:

- **Both:** the upload re-sent the whole commit object and downloaded the foreign one until the context ended, with no bound. It now remembers that the object exists, only reads it again, and fails as not committed after ten reads in a row without the checksum (one in 10^7 under the simulation's heaviest damage). The subtest reads nine damaged copies and counts three creates.
- **Both:** the scenario with a `Watch` of a year passed before the fix too; only the table of `WatchWait` shows the overflow. The comment of the test says so now. The comment in `withDefaults` gave a wrong reason for the bound, and is corrected.
- **Both:** `replayCommitsEarly` can now leave a cache at `EmptyLog` with a table, which the next open refuses; its symptoms list "it records no commit object".
- **Both:** the catch-up's restart had one scenario only. `TestDB_catchUp_moved` in `/internal/core/catchup_test.go` moves the cache at the second download of c1, once to c1 (the catch-up goes on from the middle of the log) and once to a name that is not in the log (`ErrCacheNotInLog`).
- **One, a real race:** `setUp` reads the position and then the schema in two statements, so an instance that opens a shared cache while another commits its first table would refuse the cache. `checkObjects` now reads the position again before it refuses.
- Comments: replay's doc and `errCacheMoved` no longer reason from the catch-up, which `catchUpFrom` now explains; `fetchAgain`'s doc names `errCacheMoved`; the doc of `Reader.Read` no longer links an unexported type; `ErrLocal`'s doc names the replaced file; the test comment no longer names the root package; the comment of `setUp` is back on the call it describes; `watchWait`'s parameter no longer shadows the type `watch`; the stop message names `next` too.

Left, with reasons: no deterministic test reaches `next` (no yield sits between the commit of SQLite and `DB.moved`, and adding a yield site to `Tx.Commit` only for this changes every trace of the simulation); `ignoreCacheError`'s variant that commits and reports an error leaves `at` stale, but only under that planted bug and only in a window the scheduler never opens; the retry loop of a read has no bound, since each turn needs a whole commit of the write connection.

### Why

Both reviewers' agreements are the strong signal; the race at `Open` was serious enough to fix on one reviewer's word.

### What worked

The reviewers agreed on the main points without seeing each other's report.

### What didn't work

My first count of creates in the new upload subtest was 2, and the test said "Expected "2", but got "3"": the attempt after the lost answer is a create too, and it is the one that finds the object.

### What I learned

A reviewer that reads the planted bugs against a new refusal at `Open` finds new symptoms that no run had shown yet.

### What was tricky

Nothing beyond the count above.

### What warrants review

`checkObjects`' second read, and the bound of ten reads in `Tx.upload`.

### Future work

None beyond the report.
