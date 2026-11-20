# Diary: indexes in the commit format and the storage core

Slice 9 of `/docs/roadmap.md` adds `create index i on t (a, b desc)` and `drop index i`. This task brings indexes to the commit format (`/proto/s4ql/v1/commit.proto`, `/internal/format`, `/internal/s4qlpb`) and to the storage core (`/internal/core`): the definition of an index in a commit object, a real SQLite index in the cache, replay, and a scan by a named index. The engine, the planner, and the driver are the next builder's task. The branch is `index-core`, from `origin/index-stmt` at `870f427`, which holds the statement pass of slice 9 (pull request #92).

## Step 1: the requirements, SQLite's facts, and `CreateIndex` and `DropIndex` in the commit format

**Author:** builder (index-core)

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Task: indexes in the commit format and the storage core. Only `proto/`, `internal/format`, `internal/core` (and the binding `internal/sqlite` if a call is missing). [...] Agreed with Markus (2026-11-20): An index is `create index i on t (a, b desc)`: a name, a table, one or more columns of the table in order, each ascending or descending. Not unique. No partial index, no expression, no collation. The log holds only the definition. [...] The cache holds a real SQLite index [...] s4ql's engine picks the index; the cache's SQLite then scans exactly that index. Core therefore offers scans by a named index and never lets SQLite choose. The cache's layout version rises to 3. [...] 1. Commit format [...] creating and dropping an index, in the manner tables are created and dropped in a commit, with the index's name, its table, and its columns with their directions. Dropping a table drops its indexes (decide whether the commit names them or the reader derives it, and say why). Validation on read is as strict as for the rest [...] 2. Core write API [...] 3. Cache [...] 4. Replay and catch-up [...] 5. Core read API: a scan by a named index [...] 6. Counts [...] 7. Tests in the core's style [...] 8. Decisions that the agreement does not settle [...] record each in `docs/todo.md` [...]"
**Interpretation:** Two new kinds of change in the commit object, checked as strictly as the others; an index in the cache that `conn.load` reads back and checks; `Tx.CreateIndex` and `Tx.DropIndex` with the net effect in the commit object; replay of both; a scan by an index or the primary key with bounds on a prefix of the key and a direction; a count of the rows read; and tests against SQLite itself.
**Inferred intent:** The storage half of slice 9 with the same trust as the rest of the core, so that the engine's builder only picks an index and states bounds as values.

### What I did

First SQLite itself, with the pinned shell built from `/internal/sqlite/c/shell.c` and `/internal/sqlite/options.h` into the scratchpad:

- `sqlite_schema` holds one row for each index made with `create index`, and none for the index of the primary key of a table without a rowid. A new row gets the rowid after the largest one: after `i1`, `I 2`, `i3`, `i4` at rowids 2 to 5 and `drop index "I 2"`, `i5` got rowid 6. So the order of the rowids is the order of creation of the indexes that exist.
- `pragma_index_list` lists the newest index first, and the index of the primary key as `sqlite_autoindex_<table>_1` with origin `pk`. It reports `unique` and `partial`. `pragma_index_xinfo` gives a row for each column of the key of an index with `key` 1 for the index's own columns and 0 for the columns of the primary key that SQLite adds, `desc`, `coll`, and `cid`, which is -2 for an expression.
- The key of an index is its columns and then the columns of the primary key that it does not hold: `i3 (c desc, k2 desc)` on a table with the key `(k1, k2)` has the key `(c desc, k2 desc, k1)`.
- `explain query plan` of `select * from t indexed by i where a = ?1 and b > ?2 order by ...` gives `SEARCH t USING INDEX i (a=? AND b>?)`, `a is null` shows as `a=?`, a scan without bounds as `SCAN t USING INDEX i`, the primary key as `SEARCH t USING PRIMARY KEY (k1=? AND k2>?)` and `SCAN t`, and an index that holds every column of the table as `USING COVERING INDEX`.
- An `order by` of more than 63 terms is never satisfied by an index: SQLite sorts (`USE TEMP B-TREE FOR ORDER BY`), with 500 and with 800 key columns alike. A condition of 1,201 terms is refused (`Expression tree is too large (maximum depth 1000)`), and one of 501 terms is not.

Then the format. `/proto/s4ql/v1/commit.proto`: `Change` gets `create_index = 5` and `drop_index = 6`; `CreateIndex{index, table, columns}`, `IndexColumn{column, descending}`, where `column` is a position in the table's columns as in `UpsertRows`, and `DropIndex{index}`. `DropTable` says that it drops the indexes of the table too, and that a commit object does not list them. `/internal/format/check.go`: `checkCreateIndex` refuses no name, no table name, the name of its table as its own (compared as SQLite compares names), no columns, a column without a position, and a column twice; a drop index needs a name. `/internal/format/writer.go`: `CommitWriter.CreateIndex` and `CommitWriter.DropIndex`. The version of the format stays 0: a new kind of change is a new member of the `oneof`, which a reader of before refuses as one of a newer version, as the README of the format says a new kind is made.

Tests in `/internal/format/reader_test.go` and `/internal/format/writer_test.go`: three refusals as newer, sixteen as malformed, ten refusals of the writer, both samples written again with a create index and a drop index, the round trip of the writer, and two more operations in `FuzzCommitWriter` with a seed, and a seed for `FuzzCommitReader`.

### Why

The format holds the definition and nothing else, as agreed. A position cannot disagree with the table the way a second copy of a column's name could, which is the reason the README gives for the primary key and for `UpsertRows`. A commit object stands alone, so the format checks what one change shows in itself, and replay checks the rest against the cache.

### What worked

`go test ./internal/format/` passes. `FuzzCommitWriter` ran 86,091 inputs and `FuzzCommitReader` 711,931 in 20 seconds each without a failure.

### What didn't work

- A Bash command with a Python heredoc of many lines was refused: "this command is too complex to verify that it stays inside the worktree". A script written with the Write tool to the scratchpad and run with the path of the worktree as its argument worked.
- Two tests of the reader used the field numbers 5 and 6 of `Change` as fields that it does not know. They are 7 and 8 now.
- `TestCommitWriter` pinned 6 changes and 9 writes of the example; they are 8 and 11 with the two new changes.

### What I learned

- SQLite gives the index of the primary key of a table without a rowid a name and no row in `sqlite_schema`.
- An index cannot serve an `order by` of more than 63 terms, whatever the terms are.

### What was tricky

Nothing in this step.

### What warrants review

- The comments of `CreateIndex`, `IndexColumn`, `DropIndex`, and `DropTable` in `/proto/s4ql/v1/commit.proto`.
- `checkCreateIndex` in `/internal/format/check.go`.

### Future work

- The cache, the write API, and replay: step 2. The scan and the counts: step 3.

## Step 2: the index in the cache, `Tx.CreateIndex` and `Tx.DropIndex`, the net effect, and replay

**Author:** builder (index-core)

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "2. Core write API, in the manner of `Tx.CreateTable`/`Tx.DropTable`: create an index and drop an index inside a transaction, with rollback and the statement savepoint working as for tables, and the errors SQLite gives [...] exposed so the engine can return SQLite's messages and codes in SQLite's order. Creating an index on a table that has rows builds it from them. The schema that core hands the engine [...] also lists the table's indexes, in the order they were created, and that order is the same on every instance and after replay [...] 3. Cache: the index is a real SQLite index in the cache file, with exactly the columns and directions. On open, the cache's indexes are read back and checked against what the layout allows, as tables are (`conn.load`) [...] The order of creation must survive in the cache (find out whether SQLite's schema table keeps it and how to read it reliably; if not, store it). 4. Replay and catch-up apply index commits to a cache [...] `pragma integrity_check` on the cache passes after replay, after a crash in the middle of applying such a commit, and after a rolled-back transaction; add that check to the core's tests where they already crash and replay."
**Interpretation:** An index struct beside the table's columns, loaded and checked with the table; two calls of `Tx` that keep what the commit object needs; an order of the changes in a commit object that replay can always apply; the model and the simulation taught indexes, so that every existing check of crash and replay covers them.
**Inferred intent:** Indexes get the trust that tables have, by the tests that tables have.

### What I did

A new file `/internal/core/index.go`. `index` and `keyColumn`; `table` got `indexes`, in the order of creation, and `primary`, SQLite's name of the index of the primary key. `conn.loadIndexes`, which `conn.load` calls, reads `pragma_index_list` joined with `sqlite_schema` in the order of the rowids, and `pragma_index_xinfo` for each index, and refuses with `ErrCacheVersion` an index that is unique, partial, of a constraint, of a reserved name, on an expression, with a collation, or with a column twice. `conn.lookUpIndex` finds an index by its name through `sqlite_schema` and its table. `cacheVersion` is 3.

`Tx.CreateIndex` checks in SQLite's order: the table (`ErrNoTable`), the reserved name (`ErrReservedName`), a table of the name (`ErrTableExists`), an index of the name (the new `ErrIndexExists`), and then the writer of the format, the limit of 250 columns, and the positions (`ErrInvalid`). `Tx.DropIndex` fails with the new `ErrNoIndex`. `Tx.CreateTable` asks `Tx.unused` now, so it fails with `ErrIndexExists` under the name of an index. `Tx.Indexes`, `Tx.Index`, `ReadTx.Indexes`, and `ReadTx.Index` give `s4qlpb.CreateIndex`.

The transaction keeps `droppedIndexes`, the names of the indexes that were there at its start and that it dropped by name, and `createdIndexes`, the folded names of those that it created and that exist; a savepoint copies both, and `Tx.DropTable` takes the indexes of its table out of the second. `Tx.spoolCommit` writes the drops of indexes first, then the tables as before, then the created indexes in the order of `sqlite_schema` (`Tx.spoolCreatedIndexes`). Its doc comment says why replay can always apply that order. `apply` in `/internal/core/replay.go` got `applyCreateIndex` and `applyDropIndex`, and a create table under the name of an index is a broken log.

The tests' model: `mtable.indexes`, `mstate.index`, `mtx.createIndex`, `mtx.dropIndex`, `mtx.taken`, the lines of indexes in `mtx.effect`, and `mstate.apply`, which refuses a drop of an index after a change of a table and any change after a create index. `dump` and `cacheDump` put the indexes of a table, in their order, into its description (`t1(k*0,v) +i1(1) +i2(0 desc)`), and `cacheDump` runs `pragma integrity_check` first and reads the order of the indexes from `pragma_index_list`, reversed, which is another way to it than the core's. The simulation's `run.statement` creates an index 6 times in 100 and drops one 4 times in 100, under three names of which one is a name of the tables, and scans a whole table by one of its indexes, forward or backward, against `mtable.sortedBy`.

Tests in `/internal/core/index_test.go`: `TestTx_CreateIndex` (20 refusals in SQLite's order, the build from rows, rollback, the savepoint, the index of 250 columns), `TestTx_DropIndex`, `TestTx_Commit_indexes` (14 net effects, each replayed and applied by the model), `TestIndexes_order`, `TestReplay_indexes` (the order of application, 25 commit objects that stop replay, a commit object that breaks off after it created an index), and `TestOpen_cache_indexes` (a cache of version 2, nine foreign indexes).

### Why

SQLite's schema is the catalogue of the cache, so the order of creation is read from it and not stored a second time. The net effect is by name for tables, and an index depends on a table of another name, so the changes of indexes cannot sit in the per-name order: drops of indexes go before everything and creates after everything.

### What worked

- The 84 subtests of the first draft of the write and replay tests passed on their first run, after one hang of my own making was removed.
- The model found nothing in the core. Every violation of the first run with indexes in the simulation was a fault of the test.
- All 26 changes that I made by hand to the new code, one at a time, made a test fail: the operators of the bounds, open and closed, the NULL pin, both directions, the primary-key columns at the end of a key, the index named in the statement, the order of new indexes in a commit, the drop of a created index, each refusal of a foreign index, the counts, the savepoint, and the version.

### What didn't work

- The first run of the new tests hung: a test served a second instance while the first held the lease. `go test` was moved to the background after 180 s. The test closes the first instance now.
- The first run of the short suite with indexes in the simulation failed `TestFreeRunning` and nine planted bugs with `create table "t0", which exists: the index exists: "T0"`: the statement of the simulation still expected `ErrTableExists` for every name that is taken. It asks `mtx.taken` now.
- Then `TestSimulation` failed once: `the workload reached "deletes of the key -0.0" 0 times, and must reach it 2 times at least`, in the full workload. The index statements move every draw of the workloads. I took the entry out of `fullReaches`, where the single and the hammer workloads keep theirs, and added four entries for indexes to each workload, a quarter of what the default seeds reach.
- `core_test.go` pinned the text "reads version 2" of the error of an old cache.

### What I learned

- `pragma_index_list` lists the indexes of a table from the newest to the oldest, on a fresh connection too, because the connection reads the schema in the order of the rowids and puts each index before the others.
- A table and an index of SQLite share their names, so `Tx.CreateTable` needed a check that it never had.

### What was tricky

The order of a commit object. A transaction can drop a table, give its name to an index of another table, drop an index and give its name to a table, and drop an index by name and then its table. With the changes of one name together, as for tables, `drop table x; create table t; create index x on t` would put the index before its table. With drops of indexes after their table, the drop would find nothing. The order "drops of indexes, tables, creates of indexes" has neither fault, and `TestTx_Commit_indexes` has a case for each of these.

### What warrants review

- The doc comment of `Tx.spoolCommit` in `/internal/core/tx.go`, and `Tx.spoolCreatedIndexes`, `Tx.forgetIndexes`, and `Tx.DropIndex` in `/internal/core/index.go`: is there a transaction whose net effect replay cannot apply, or that leaves another order of indexes than the writer has?
- `conn.loadIndexes`: the claim that the rowids of `sqlite_schema` give the order of creation.
- `mstate.apply` and `mtx.effect` in `/internal/core/model_test.go`.

### Future work

- The scan and the counts: step 3.

## Step 3: the scan by an index, the counts, the generated comparison with SQLite, and the fuzz test

**Author:** builder (index-core)

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "5. Core read API: a scan by a named index, in the manner of `ScanRange`: the caller gives the index (or the primary key), bounds on a prefix of its key (equal values for the leading columns, then an optional lower and upper bound on the next, each open or closed), and a direction, forward or backward, and gets whole rows in the order of the index (or its reverse). [...] Descending columns must bound correctly [...] define the API so the engine states bounds in terms of values, not of positions in the index, and core turns them around; this is where a planted bug will go later, so make it one small function. Core must make SQLite use exactly the named index with exactly those bounds. [...] So the tests check with `explain query plan` [...] 6. Counts: core counts the rows it reads from the cache per scan [...] 7. Tests in the core's style [...] a generated comparison of index scans with a plain SQLite database holding the same rows and the same index [...] Extend the core's fuzz target or add one for index scans"
**Interpretation:** One statement for every scan, which names its index and holds the bounds as comparisons of values; a count of steps; a check of every shape against SQLite's rows and against SQLite's plan.
**Inferred intent:** The engine states what it wants in values, and no fault of direction or bound can hide behind SQLite's choice of a plan.

### What I did

`IndexScan{Index, Equal, Lower, Upper, Backward}` and `ScanIndex` on `Tx` and `ReadTx`, in `/internal/core/index.go`. `table.scanKey` is the key of an index: its columns and then the primary-key columns that it does not hold. `table.checkScan` refuses more pinned columns than the key has, a bound after the last column, a bound that is NULL, and a value that `table.checkValue` refuses. `scanConditions` is the one small function for the bounds: `=` or `is null` for each pinned column and `>`, `>=`, `<`, `<=` for the bounds, all on values. `table.scanSQL` writes `select * from main.t indexed by i where ... order by ...`, with SQLite's name of the index of the primary key for the table itself, and the columns that are not pinned in the `order by`, each in its direction or all in the other. `conn.scanIndex` steps it and counts. `conn.scan` calls it, so `Scan`, `ScanRange`, and the scan of a commit name their index too, and `Tx.ScanRange` and `ReadTx.ScanRange` share one loop each with `ScanIndex` (`Tx.scan`, `ReadTx.scan`). `RowsRead` on both.

`ReadTx.ScanPlan` in `/internal/core/index_internal_test.go` gives SQLite's plan for the statement of a scan on the connection of the transaction. Tests: `TestScanIndex` (42 scans by hand in a read and a write with the count of rows read, 22 refusals, the counts, the stop of the instance and a panic in the loop, and a key of 70 columns), `checkIndexScans` with `TestScanIndex_generated` and `FuzzScanIndex`. `/internal/core/lease_test.go` asks the new calls after the end of a transaction by the lease, and reads the next row of a `ScanIndex` after the loss. `internal/core` is in `FUZZDIRS` of the Makefile.

### Why

A statement that compares values needs no turning of bounds for a descending column: SQLite does it, and the test against SQLite's own rows shows it. The `order by` makes the order a promise of SQL and not a habit of the scan, and the plan shows that it costs no sort.

### What worked

- `TestScanIndex_generated`: 400 seeds, 52,988 scans, each with the rows of SQLite's own database in the same order, a plan of one row that names the index and the constraints, and the count. 17,833 of them give some rows and not all; of those, 737 pin a column to NULL, 4,057 bound a descending column, 2,745 reach into the primary-key columns at the end of a key, and 5,973 read a covering index. It takes about 5 s.
- `make fuzz FUZZDIRS=internal/core FUZZTIME=20s` ran 4,303 inputs of `FuzzScanIndex` without a failure.

### What didn't work

- The first plan check failed: `Expected "SCAN wide USING COVERING INDEX wide_i", but got "SCAN main.wide USING COVERING INDEX wide_i"`. SQLite prints the schema that the statement names.
- A Bash command with `sed -i` and a second command was refused twice as "too complex to verify that it stays inside the worktree"; the Edit tool did the change.

### What I learned

- `explain query plan` does not tell `>` from `>=`: both print `b>?`. Only the rows tell an open bound from a closed one.
- SQLite calls an index of a table without a rowid a covering index when the key of the index holds every column of the table.

### What was tricky

The `order by` of a scan. With every column of the key in it, a key of more than 63 columns made SQLite sort the rows (`USE TEMP B-TREE FOR ORDER BY`), also when most columns were pinned. The statement orders by the columns that are not pinned, which is the same order, and a test pins the plan with 64 such columns, where SQLite still searches the right index by the right bounds and then sorts.

### What warrants review

- `scanConditions`, `table.scanSQL`, `table.scanKey`, and `table.checkScan` in `/internal/core/index.go`.
- `wantPlan`, `plainScan`, and `checkIndexScans` in `/internal/core/index_test.go`: the statement of the plain database is written apart from the core's, with `is` for the pinned columns and the whole key in its `order by`.

### Future work

- The documents, the review, and the checks: step 4.
