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

## Step 2: Indexes in the sequence, every path, the model, the plan of the cache, a planted bug, and the pins

**Author:** index-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "1. Generated sequences with indexes. [...] 2. Every path gives SQLite's rows. [...] choose and justify a sampling that still reaches every kind of path in every run, proven by tallies [...] 3. The choice against a model. [...] 4. SQLite did what was asked. [...] 5. Planted bugs [...] (a) in the engine's fitting of conditions to an index [...] 9. Tallies."
**Interpretation:** Make the sequence write slice 9, one device in the engine's tests that runs the paths beside SQLite, a model in `stmttest`, the plan from the engine's tests, the bug in `fit`, and recount every pin that moved.
**Inferred intent:** The generated checks read through indexes as often as the hand tests, and a wrong rule or a wrong scan cannot pass unseen.

### What I did

- `stmttest.Sequence` (`internal/sql/stmt/stmttest/sequence.go`): a create index in about ten statements and a drop index in 29 (`next`); index names from a pool common to every position of a table and of an index, one of them among the names of a create table and a drop table, and one name of a table among those of a create index and a drop index; `Sequence.createIndex`, the hook `Generator.CreateIndex`, which gives columns of the shape, each once, descending one time in three, and one time in twelve a name that the shape does not have, one in forty a second; `Sequence.indexConditions`, one WHERE in two of a table on which a create index was drawn, which pins leading columns of the index with `=` or IS NULL and bounds the next; a create table makes its name likelier for a create index; `Sequence.Indexed`, which a third of the sequences of `TestDifferential` and `TestConn_differential` call, for more create indexes, of columns without the key, and more such WHERE; `Sequence.IndexNames`.
- `stmttest` rules (`rule.go`): `ChosenAccess` with `TableIndex` and `Access` (with `Same` and `String`), the model of the rules, written from the roadmap's text and the decisions in the todo; `IndexRule`, SQLite's errors of a create index and a drop index in SQLite's order; `SelectText`. Tests `TestChosenAccess`, `TestIndexRule`, `TestSequence_createIndex`.
- The device, in `internal/engine/differential_test.go`: `pair.everyPath` runs the WHERE of each select, update, and delete (as `stmttest.SelectOf` gives the select of a change) on the second engine of the pair before the call: by the rules, and by a sample of `candidates`, every path that can serve it (every row, the primary key, each index, each with every number of pins that `engine.Path.Limited` allows, forward and backward, each once by what it reads). Each must give SQLite's rows as a multiset, and a forced path SQLite's `ORDER BY` of its key exactly (`accessKey`, `orderOf`). One in four, and the access of the rules one time in four, go through `core.ReadTx.ScanPlan` outside a transaction (`pair.checkPlan`, `wantPlan`). `pair.bothWays` then runs the call itself by the next of `otherPaths` with the table's index names. `checkChoice` compares `engine.AccessOf` with `stmttest.ChosenAccess` for every WHERE; `reachIndex` checks each create index and drop index against `stmttest.IndexRule` and counts it. The states that the tests compare (`oracle.state`, `instance.state`, `memory.state`, `coreState`) include the indexes.
- `core.ReadTx.ScanPlan` moved from the core's test file into `index.go`, so that the engine's tests reach it; `engine.Access.Scan`. `TestEngine_rowsRead` checks the plan of each pinned query (`checkPlanOf`).
- The planted bug `pinByPlace` in `engine.fit`: the pin of the n-th column of a key is looked for among the conditions on the n-th column of the table. The bugs reach `fit` through `reading.bugs`. `TestDifferential_plantedBugs` plants it in both engines of the first 60 sequences, through `differentialFind`, which gives the first difference in place of failing, and `instance.planted`, which makes an internal error an answer.
- The simulation: every fourth engine of a node reads every row and the others of every second engine force a path of one of the sequence's index names; `tableOf` knows a create index, and a drop index counts as a write of a table that is there.
- `drivercheck`: `pair.checkQuoted` excuses a create index whose first column that the table does not have, in the order of the text, is in double quotes, which mattn's build with DQS on indexes as a string.
- Golden plans, by a sub-agent: `TestAccessOf_golden` in `access_test.go`, 7 schemas and 62 plans under `internal/engine/testdata/plans`, `-update`.

### Why

- The sample: `TestDifferential` runs 1,138 WHERE by the device, 443 of them on a table with an index; 349 have more than four candidates and run four at random from the pair's own seed, the others all. Four keeps the package within its budget, and the tallies show every kind reached in every run, with least numbers (a third, or 1) that fail when one stops. The fuzz targets run every candidate.
- An update and a delete find their rows with the same scan as a select (`build.access`), so the select of their WHERE through every path checks what they find; the call itself then runs by one path in turn on the second engine, which checks the change, and the table after it.
- `pinByPlace` in place of a first try, `boundPastGap` (the bounds of a later column of the key taken for the column after the pins): with that one, 2 of 60 sequences found it, because the rules pick the primary key at a tie of a range; the pin taken from the wrong column is found by 6 of 60.
- The sequences of `TestDifferential` grew from 40 statements to 48, so that those of the earlier slices are about as many as before: at 40, least numbers of slices 5 to 8 failed, at 48 four did, which went to a third of their new counts.

### What worked

- No difference from SQLite at the first run with indexes, by any path, in any order, in any plan, and no difference between the model and the engine.
- `TestChosenAccess`, `TestIndexRule`, and the plans of `TestEngine_rowsRead` passed at once.

### What didn't work

- `TestDifferential` at the first run of the sequence with indexes reached few paths of an index: 375 of 3,600, and 1 with three pins. The share of create indexes, the WHERE of indexes, and `Sequence.Indexed` took it to about 950 of 4,300, with 21 of two pins and 3 of three; most create indexes fail on a table that is not there (640 of 1,700) or a name that is taken.
- The first simulation with indexes panicked in `tableOf` on a drop index: `panic: a statement of type *stmt.DropIndex`.
- `TestSimulation`'s idle workload failed `"selects with WHERE of their own that give rows, compared with SQLite"`, 0 of 50 seeds with at least 1 expected: the sequence draws otherwise; recounted in the next step.
- `drivercheck` failed on `create index "before" on BEGIN (REPLACE asc, """")`, where s4ql gives `no such column: """ - should this be a string literal in single-quotes?` and mattn creates the index; then on a create index whose index already existed, which the first version of the excuse took for the quoted column; then on one whose first missing column in the order of the text is in double quotes and a later one is not, which `stmttest.MissingColumn` gives in another order.
- `git worktree add` of the base commit into the scratchpad, to measure the time before, was refused by the permission system as a git command outside the worktree; I measured on a detached checkout of the base in this worktree instead.
- Python one-liners and heredocs of several edits were refused as too complex to verify; scripts in the scratchpad and the Edit tool worked.

### What I learned

- A planted bug in the fitting is found only where an index wins or a path forces it, so it must be in both engines of the pair; a skip of a WHERE whose rules-run fails hid it until only "not supported" was skipped.
- `strings.EqualFold` folds more than SQLite does (`été` and `ÉTÉ` are two names); `fold` of the tests is `stmt.Name.Fold`.

### What was tricky

- `Path.Limited` with zero pins is the same access as every row, and the candidates are deduplicated by what they read, so "the primary key with 0 pinned" never shows apart.

### What warrants review

- The sampling of four candidates, and whether the least numbers of rare kinds (two pins and a range: 1) are firm enough; the fuzz targets run every candidate.
- `checkChoice` against `stmttest.ChosenAccess`, whose bounds of equal value are compared by SQLite's comparison, not by identity (the engine keeps the later of two open bounds at one value, the model the first).
- Pinned numbers that moved, from the test's own output: `excusedArguments` 707 to 822, `excusedArgument` 300 to 326, `excusedType` 576 to 477, `excusedLong` 2 to 1, `excusedLongChange` 4 to 2; least numbers `insert into INT UNIQUE constraint failed` 50 to 14, `select * with NULL ok` 45 to 12, `IS NOT NULL on another column ok` 8 to 2, and `update WHERE that reads a scan to an upper bound ok, rows changed` (1, reached 0 times at 48 statements) removed.

### Future work

- The simulation's integrity checks, tallies, and recounted minimums, the core's bounded scans and its two planted bugs, the fuzz targets, and the driver check's pins: sub-agents, merged in the next step.

## Step 3: The work of the sub-agents, the reviews, the merges, and the checks

**Author:** index-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "5. Planted bugs. [...] (b) the key of an index without its primary key columns (in the one `core.IndexKey` once it is there); (c) in core, a bound turned around on a descending column [...] 7. `pragma integrity_check` on the cache after replay and after crashes in the engine's simulation [...] 8. Fuzz. `FuzzIndexes` and `FuzzIndexes_core` [...] Finish: `make test`, `make lint`, `make check-browser`, `make fuzz-shards` pass; `make test-asan` for the packages whose fuzz tests you changed. Self-review against these requirements."
**Interpretation:** Merge the branches of the four sub-agents, take in the reviews, merge `index-engine` and `main`, and run the checks.
**Inferred intent:** A branch that a reviewer can read as one piece of work.

### What I did

- Merged, from sub-agents in worktrees of their own: the golden plans; the core's simulation, which scans by an index or the primary key with pins and bounds against the model's own filter (`mtable.keyOf`, `mtable.scanned`), with the planted bugs `turnDescBound` (in `scanConditions`, which takes the key) and `shortKey`; the driver check's pins at 48 statements; the fuzz targets `FuzzIndexes` and `FuzzIndexes_core`, with `pair.allPaths` and `TestFuzzIndexes_seeds`; and the engine's simulation, with `run.checkCache` (`pragma integrity_check` on a copy of a cache after each crash, of each live node at the end, and of the replay of `run.verify`), the tallies of slice 9, `run.dropUnder` (an index dropped on another connection while rows that read by it are open), `pinByPlace` with its own violation, every odd seed's sequence `Indexed`, and the least numbers recounted from 1,000 seeds.
- Merged `origin/index-engine` twice (the second time with `core.IndexKey` and the direction in the event; conflicts in `access.go`, the field of `reading`, and `simulation_test.go`, the entry of `openBound`) and `origin/main` (#105, #104).
- Moved `shortKey` into `core.indexKey`, the one definition of the key, which `core.IndexKey` calls without the bugs and `table.scanKey` with them.
- From the reviews: `FuzzChanges` sets `pair.inTx`; `checkPlanOf` requires its table and closes its reader in a defer; `Sequence.IndexNames` gives the values of the names without quotes, since a path compares values, so that the forced index paths of the simulation read by their index; `TestDifferential_plantedBugs` runs each finding sequence again without the bug; a guard of `pinByPlace` that always held is gone; the docs of the shares of `Sequence`.

### Why

- `shortKey` in `indexKey` and not in `IndexKey`: the core's planted bugs are a field of its connection, and `IndexKey` is a pure function of the engine's too, which must stay right so that the engine asks for a scan of the right key and the core's wrong key shows.
- The core's bugs are found by the core's simulation, since only the core's tests can plant them.

### What worked

- `make test`, `make lint`, `make check-browser`, and `make fuzz-shards` for the groups c (4 and 6 shards, 37 fuzz tests) and go pass.
- The counted shares: `pinByPlace` by 7 of the first 60 sequences of `TestDifferential_plantedBugs`, and by 19 of 200 seeds of the engine's simulation, all by its own violation, "the storage core refused a call that the engine had checked"; `turnDescBound` by 33 and `shortKey` by 29 of 200 seeds of the core's simulation; none by a seed without its bug.
- `go test -race -count=50` of the new tests of `stmttest`, and `-count=10` of `TestDifferential_plantedBugs` and `TestEngine_rowsRead`.

### What didn't work

- The review of the fuzz targets found that `Sequence.IndexNames` gave spellings in double quotes, so about 40% of the engines of the simulation that should read by an index did not; the simulation's sub-agent recounted after the fix.
- `TestSimulation_plantedBugs/pinByPlace` failed at seed 26 of the default workload with `seed 26 finds the bug by another violation: [statement 11, "...UPDATE ... GROUPS SET ..." : s4ql: internal error: the storage core refused a call that the engine had checked: invalid: a value of kind ...`: the bug pins a column to a value of another column's type. That is now the bug's own violation.
- A forked review over the whole branch reported findings in the code of others, which the sub-agents relayed; each is taken in above or noted below.

### What I learned

- An engine that a node opens for the fourth time is rare in the simulation, so a choice by the number of the engine left the forced index paths unrun; the simulation draws it from the engine's seed.

### What was tricky

- Three sub-agents changed `differential_test.go`, `simulation_test.go`, and the README of the engine at once; the merges needed only the README by hand.

### What warrants review

- `core.ReadTx.ScanPlan` is exported in the code of the core for tests, and caches the prepared `explain query plan` of each distinct scan on the connection as every statement of the cache is; a review called it a nit, and it stays.
- `reachIndex` counts a "not supported" of a create index without checking why; `pair.step` already fails a statement of the generator that the engine does not support.

### Future work

- The mutation testing of the code of slice 9 in the engine and the core, in the todo.
