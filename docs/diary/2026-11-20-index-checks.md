# Diary: Slice 9 of the roadmap in the generated checks, indexes

The engine and the driver run the `create index` and `drop index` of slice 9 since the branch `index-engine` (pull request #105), and the engine picks the access of a `where` by fixed rules. Its generated checks never read through an index: `stmttest.Sequence` leaves `Generator.Indexes` off, so only the hand tests cover index reads, which the independent review of #105 found first. This task makes the sequences create, use, and drop indexes, runs every generated `where` through every path that can serve it, checks the choice against a model of the rules written apart from `chooseAccess`, checks with `explain query plan` that the cache searched what the engine asked for, plants three bugs, keeps golden plans, runs `pragma integrity_check` in the engine's simulation, adds fuzz targets, and counts what the workload reaches. The branch is `index-checks`, from `origin/index-engine` at `2cf5533`.

## Step 1: The requirements

**Author:** index-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "The independent reviewer's first finding is your job: today the engine's generated checks never read through an index, because `stmttest.Sequence` leaves `Generator.Indexes` off [...] 1. Generated sequences with indexes. [...] 2. Every path gives SQLite's rows. For every generated `select`, `update`, and `delete` with a `where`: besides the access the rules choose, run every access that can serve it (the full scan, the primary key, each index of the table, with each number of pins the path allows, forward and backward) and require the same rows as SQLite as a multiset, and on a forced path of a `select` the rows in the order of that path's key [...] choose and justify a sampling that still reaches every kind of path in every run, proven by tallies [...] 3. The choice against a model. [...] 4. SQLite did what was asked. [...] 5. Planted bugs. At least three [...] 6. Golden plans. [...] 7. `pragma integrity_check` [...] 8. Fuzz. `FuzzIndexes` and `FuzzIndexes_core` [...] 9. Tallies. [...] If a check finds a real difference from SQLite, a lost row, or a wrong write, that is the point [...] Never weaken a comparison to make a test pass. CI's time is watched [...] keep each package's run within about its present time plus a third"
**Interpretation:** Index statements in `stmttest.Sequence` with their rules; one device in the engine's tests that runs the forced paths beside SQLite; a model of the choice in the test code; `explain query plan` of the cache's statement from the engine's tests; three planted bugs with counted shares; golden plans of named queries; `pragma integrity_check` in the engine's simulation; two fuzz targets; least numbers for every kind of outcome.
**Inferred intent:** Slice 9 gets the trust of the slices before it, from the generated checks that found most of their bugs, before its mutation testing.

### What I did

Read the roadmap's slice 9 and how it is checked, the spec's rule on row order, the research note's section on verification, the decisions of the engine step in `docs/todo.md`, and the code of `chooseAccess`, `fit`, `forcedAccess`, `pair.bothWays`, `otherPaths`, `stmttest.Sequence`, and the planted bugs of the engine and the core.

### Why

The pattern of the generated checks of slice 8 is the pattern here.

### What worked

Nothing to run yet.

### What didn't work

- A merge of `origin/main` into the branch stopped on a conflict in `internal/sql/mutation/equivalent.txt`; I aborted it, since the builder who lands #105 merges `main` into `index-engine` and I merge that.

### What I learned

- `Sequence.next` picks the kind of each statement itself, so `Generator.Indexes` alone does nothing for a sequence: its kinds need the create index and the drop index.
- The core's simulation scans an index only whole, so a bound turned around on a descending column in the core is found by no check there today.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

- The sequence's index statements and rules, the device of the paths, the model, the plans, the bugs, the golden files, the fuzz targets, and the tallies: the next steps.
