# Diary: Remove the in-place upgrade of the cache from version 1

Slice 6 raised the version of the cache's layout to 2, and its builder wrote an upgrade in place of a cache of version 1 in `internal/core/cache.go`. A review then found a concurrency bug in it, which a second change fixed. Markus decided on 2026-11-20 that none of this is used outside the tests, so formats break freely, and on the upgrade: "Yes, remove it." This task removes the upgrade and its tests, and has a cache of version 1 refused with `ErrCacheVersion` like any other version. The branch is `remove-cache-upgrade`, from `main` after the merge of the mutation testing of slices 5 and 6 (#85).

## Step 1: Remove the upgrade, its test hook, and its tests

**Author:** remove-cache-upgrade builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Remove the upgrade: `conn.upgrade`, `conn.checkNoNull` if only the upgrade uses it, the upgrade-only parts of `setUp`, the test hook `Options.versionRead`/`OnVersionRead` if only the upgrade's tests need it, and the tests of the upgrade (including the version 1 half of the open-reads test and the forced-order concurrency test of the upgrade). A cache of version 1 (or any version other than 2) is refused with the existing `ErrCacheVersion`, as any unknown version already is. Keep or add one test that a version 1 cache is refused with that error and left unchanged. The cache version stays 2. Remove the upgrade's entries from `internal/sql/mutation/equivalent.txt`, and check that no remaining entry went stale because line content it keys on changed. Update `internal/core/README.md` (or wherever the upgrade is described) and `docs/todo.md`: delete the upgrade's items; mark the `nullable` field of `Column` in `commit.proto` as confirmed by Markus on 2026-11-20 [...]. Do not touch `README.md` or `site/`."
**Interpretation:** Take the open of a cache back to what it was before the upgrade, with version 2 as the only version that it reads; keep the tests of the open that do not need a cache of version 1, on a cache of version 2.
**Inferred intent:** Less code in the open of the cache, and no concurrency to trust in code that nothing outside the tests needs.

### What I did

In `/internal/core/cache.go`:

- `conn.upgrade` and `conn.checkNoNull` are gone; only the upgrade called `checkNoNull`.
- `conn.setUp` takes `syncOff` and `identity` again, as it did before the hook, and ends with `c.record()` and `r.check(identity)`.
- `conn.record` reads the identity only for a cache of `cacheVersion`, and `record.check` refuses every version but `cacheVersion` with `versionError`, so a cache of version 1 gets `it has version 1, and this version of s4ql reads version 2`.
- The comment of `cacheVersion` no longer says that version 1 is raised in place.

`Options.versionRead` in `/internal/core/core.go` and `Options.OnVersionRead` in `/internal/core/export_test.go` are gone; only the tests of the upgrade used them.

In `TestOpen_cache` of `/internal/core/core_test.go`:

- Removed: the open and upgrade of a cache of version 1 with a table of each type; the two forced orders of the upgrade with another instance (`OnVersionRead`), one with the write lock held after it; the changes by another process after the read of the version; the failed commit of the upgrade; 20 rounds of 8 instances that open one cache of version 1 at once; and the refusal of a cache of version 1 that holds NULL. The helpers `versionOne`, `newVersionOne`, and `versionOne.write` went with them.
- Changed: the test that fails each read of the open now runs on a cache of version 2 only, which has a table with two rows, so that its open reads more than an empty cache does. The test of an open while another connection holds the write lock keeps only its half for version 2, on a cache of its own.
- New: "should refuse a cache of version 1, made before NULL, and leave it as it is". The test makes a cache of version 2 with a table of two `int` columns and two rows, and sets only the version that it records to 1. The open reads the version before it looks at a table, so this cache is refused as one of version 1, whose columns lack `not null`, would be. The open must fail with `ErrCacheVersion` and the message of the version, and the cache must still record version 1, dump the same tables and rows, and have the same bytes in its main file.

The six entries of `conn.upgrade` are gone from `/internal/sql/mutation/equivalent.txt`. To check the others, `go run ./internal/sql/mutation -list` with all the packages of the list ran on the tree before and after, and a throwaway script in the scratchpad compared, for each of the 273 keys left, the line that the key names in both trees, the six lines around it, and its function. Every key names a line with the same text. Only `conn.setUp` changed among their functions, by its signature and its end, and the reasons of its ten entries (the pragma of `synchronous`, and the make of a new cache) are about code before that end.

`/internal/core/README.md` says that a cache of version 1 is refused and left as it is, without an upgrade, and why. In `/docs/todo.md`, the item of the decisions of slice 6 in the core no longer says that a cache of version 1 is raised in place, and the item of `nullable` is "Decided by the lead and confirmed by Markus on 2026-11-20". `/internal/format/README.md` said "for Markus to confirm" of the field too, and now says that he confirmed it.

The fault test of the reads of the open had killed the two mutants of `conn.head` that the mutation testing of slices 5 and 6 closed, at read 3 of a cache of version 1. A run of the mutation tester on an `rsync` copy of the tree, `go run ./internal/sql/mutation -out DIR -pkgs internal/core -run 'internal/core/cache.go (conn\.head|conn\.record|record\.check) ' -j 6`, gave 51 mutants: 9 do not compile and 42 are killed, 3 of `conn.head` by the new form of that test, so none survives. A first run that also held `conn.setUp` was stopped after 10 of its 91 mutants, at a load of about 29, because each survivor of `setUp` runs every stage for about 400 s. Of those 10, six survived: four are in `equivalent.txt`, and `syncOff => true` and `syncOff => false` were not. They are equivalent for the reason of `syncOff => !syncOff`, the pragma of `synchronous` that shows only after a power loss, and `equivalent.txt` lists them now with that reason.

Self-review with the code-review skill found no bug and two stale comments. The comment of `conn.record` gave version 0 as the only reason to skip the identity; it now says that a cache of another version than `cacheVersion` is refused before its identity matters. The record of the run of slices 5 and 6 in `/internal/sql/mutation/README.md` says that `conn.upgrade` and `conn.checkNoNull` were left out because the upgrade was to be removed; it is a dated record of that run, and stays.

`make test`, `make lint`, and `make check-browser` pass. `go test ./internal/core/... -count=1 -shuffle on` passes, and `TestOpen_cache` passes 50 times under `-race` while the mutation run loaded the machine.

### Why

A cache is only a copy of the log, so a refused cache costs a new cache and a catch-up from the log, and nothing outside the tests has a cache of version 1. The upgrade had to be right under concurrent opens, which took a review and a fix; without it, the open of a cache reads the record once and decides.

### What worked

The test of the refusal failed as it should with the check of the version put back to accept version 1: `Expected "the cache is not one that this version of s4ql reads" (type *errors.errorString), but got "<nil>"`.

### What didn't work

- The first run of the mutation tester wrote into a directory of the scratchpad that held results of another run, and refused it: `mutation: the results in .../scratchpad/mut are of other code or another SQLite or Go; remove the directory or use another -out`. A new `-out` fixed it.
- The run that held `conn.setUp` was too slow at that load, as above.

### What I learned

A test that is removed can take kills of mutants with it in code that stays. The fault test of the reads of the open was the one that killed the two mutants of `conn.head`, and the run above shows that its form for version 2 still kills them.

### What was tricky

Keys of `equivalent.txt` with `#2` or `#4` count equal mutants in the order of the file, so a change earlier in `conn.setUp` could move them onto other statements without `TestEquivalent` noticing, because the keys still exist. The removed lines were all after the keyed ones, and the comparison of the lines in both trees shows it.

### What warrants review

- The new test compares the bytes of the main file before and after the refused open. That holds because the open only reads, and SQLite checkpoints nothing when the write-ahead log is empty.
- `conn.record` still reads the identity in a statement of its own after the version, as before the upgrade.

### Future work

None.
