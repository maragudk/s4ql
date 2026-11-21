# Diary: Fault tests of the commit and the replay of indexes in the storage core

The mutation testing of slice 9 (#120) left 18 mutants of `internal/core` open: each ignores the error of a read of the cache or of a write of the spool file at the commit or the replay of a `create index` or a `drop index`, and no test made those reads and writes fail. This task writes the fault tests that close them, in the style of `TestOpen_cache`, and runs the mutation tester on the 18. The branch is `index-commit-faults`, from `origin/index-mutation`.

## Step 1: The tests, why the first try missed, and the 18 mutants

**Author:** index-commit-faults builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Fault tests in the core's style [...]: for a transaction that creates an index, one that drops an index, one that does both, one that creates a table and an index on it, and one with row changes beside them, make every cache read and every spool write of the commit fail in turn (one fault per run, at each position), and likewise for the replay of such commits on a second instance. For each fault the outcome must be one of the defined ones: the commit fails with the local error and nothing is published; or it is published complete. Never a commit object that lacks what the caller was told happened, never a cache that holds an index the log does not, never a log that a second instance cannot replay. [...] Run the mutation tester on the 18 mutants [...] and show that all 18 are now killed. If one still survives, it is either equivalent [...] or the test is missing a position [...]"
**Interpretation:** Sweep the position of one fault over the reads of the cache and the writes of the spool files at the commit, and over the reads of the open and the replay on a second instance, and check every outcome against the log, a fresh replay, and SQLite's integrity check.
**Inferred intent:** Trust that a failing disk under an index change can only cost the commit, never the data that the caller was told of.

### What I did

- `spool_test.go`: `spoolFaults` fails writes too (`failWrite(after)`: the write after `after` writes fails once with `EIO`).
- `index_test.go`: `indexFaults` (the five transactions), `indexFaultBase` (a table `t` with rows and an index, and eight tables of 30 columns so that `sqlite_schema` takes more than its first page), `shrinkPageCache`, `faultStep`, and two tests:
  - `TestTx_Commit_indexFaults`: for each transaction and each kind (read of any file through `faultvfs`, write of a spool file), for each position from 0 until a run where no fault lands, a new world with the base, `shrinkPageCache`, an instance on the fault VFS, the steps, the armed fault, and `Commit`. A failed commit must have had a fault, publish nothing, wrap `ErrLocal`, and not `ErrCacheVersion`. A commit that succeeds must be published with exactly the lines of the commit object of the same transaction without a fault, and must have stopped the instance if a fault landed. Then the cache opened again, `cacheState` (with `pragma integrity_check`), and a new cache that replays the whole log must all show the state of the log.
  - `TestReplay_indexFaults`: the instance `b` caught up with the base, `a` commits the transaction, and each run opens a copy of `b`'s cache on the fault VFS and waits. A failure must have had a fault, wrap `ErrLocal`, and none of `ErrLogBroken`, `ErrCacheVersion`, `ErrCacheNotInLog`; a success must have had no failed read. The log must be unchanged, and the copy, opened again without faults, must hold the state after the transaction, also by `cacheState`.
- The mutation tester on the 18 (their keys from `-list` at the lines of the brief's functions, `condition: err != nil => false` and `remove statement`, none of them in `equivalent.txt`), with `-first` of the two tests, 4 workers, and a build cache in the scratchpad: 14 killed by the new tests, 4 survived, in 7 min 41 s.
- The 4 survivors are in a new section at the end of `internal/sql/mutation/equivalent.txt`; `-report` on the results gives 0 unexplained.
- Removed the item from `docs/todo/storage-core-tests.md`; the core's row in `internal/sql/mutation/README.md` is now 738 killed, 76 equivalent, 0 unexplained, 26 killed by the new tests; a section in `docs/todo/slice-09.md`.

### Why

- One fault in each run, swept from the first position, is what `TestOpen_cache` does, and it needs no knowledge of which read is whose: the position that lands in `conn.indexNames` is among them.
- The comparison with the commit object of the same transaction without a fault is what catches the mutant that loses data (`Tx.spoolCommit` ignoring the error of `Tx.spoolCreatedIndexes`): its commit succeeds, without the index.
- "A failed read never passes unseen" is what catches the replay mutants that ignore a lookup's error: SQLite's read error is one-off, so the next statement works and the replay ends right, only without having noticed the failure.

### What worked

- The first full run passed: 2.2 s for both tests without `-race` and 7.3 s with it; under `-short` 0.8 s and 3.2 s.
- The kills: `Tx.spoolCreatedIndexes` at `indexNames` and `lookUpIndex` by "not a cache of another version"; `Tx.spoolCommit` at `spoolCreatedIndexes` by the published commit object without the index; `apply`, `applyCreateIndex`, and `applyDropIndex` by a refusal as a broken log.

### What didn't work

- Nothing failed in the code. The first sketch of the row transaction had 80 rows of 1 KB, which with the one-page cache took 974 read positions and 12 s; one row of 70 KiB gives the same writes of the spool file before the end of the commit object (its buffer is 64 KiB) in 60 positions.

### What I learned

- Why the round's first try missed: with SQLite's default page cache, the pages of `sqlite_schema` are read from the file once, when the connection loads the schema, and the queries of `conn.indexNames` and `conn.lookUpIndex` find them in memory, so no fault of a read can reach them. `pragma default_cache_size = 1` is kept in the header of the file (the build has no `SQLITE_OMIT_DEPRECATED`), so every connection that the core opens on that cache later has a page cache of one page and reads nearly every page from the file, with no change to the core.
- The browser's official SQLite has `SQLITE_OMIT_DEPRECATED`, ignores the PRAGMA, and sets its own page cache at open, so the tests reach fewer positions there; their assertions hold either way.

### What was tricky

- Telling the 4 survivors apart from a missing position: `format.CommitWriter.fail` keeps the first error in `w.err`, and `startChange`, `Annotate`, and `Close` return it, so a write error that `CreateIndex` or `DropIndex` returned and the caller ignored still fails the commit, at the next method or at the close, with the same error and the same classification by `fail`. No position could tell them apart.

### What warrants review

- The reasons in `equivalent.txt` for the 4.
- Whether "a commit that succeeds after a failed read has stopped the instance" is the right rule: it is what the doc comment of `Tx.Commit` says of a cache that fails after the swap.

### Future work

- None for these mutants.
