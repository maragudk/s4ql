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
