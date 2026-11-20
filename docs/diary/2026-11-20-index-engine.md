# Diary: indexes through the engine and the driver

Slice 9 of `/docs/roadmap.md` adds `create index i on t (a, b desc)` and `drop index i`. The statement pass reads both since pull request #92 (diary `2026-11-20-index-stmt.md`), and the commit format and the storage core hold indexes since pull request #97 (diary `2026-11-20-index-core.md`). This task is the engine and the driver: the two statements, the choice of an access by fixed rules, the order of rows that varies, a way for the tests to force every access, the counts of rows read, and two fixes in the core from the review of #97. The branch is `index-engine`, from `origin/index-core` at `2e01c6b`, with `origin/main` at `093b646` merged in.

## Step 1: the requirements, the storage interface, the rules, and the two statements

**Author:** builder (index-engine)

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Task: indexes through the engine and the driver. 1. The statements. `create index i on t (a, b desc)` and `drop index i` run through the engine and the driver, in and outside transactions, with rollback, with the errors SQLite gives in SQLite's order [...] `drop table` drops the table's indexes. `create table` under an index's name gives SQLite's error. Core's limit of 250 columns per index, where SQLite takes 2,000: answer \"not supported\" with a name of its own. Remove the temporary refusal (`indexRefusal`), the driver's `engine.Check` call, and the `checkText` wrappers [...] 2. The choice of access, by fixed rules (agreed with Markus; no statistics; the choice depends only on the statement and the schema, so it is the same on every instance). Generalise `keyAccess`/`tighter` of slice 7 from the primary key to every index of the table [...] Write the rules as one small pure function from (schema, conditions) to an access, with its own table-driven tests [...] 3. Row order varies (the spec's rule, built in this slice): for a `select` without `order by`, each query runs its scan forward or backward, chosen at random per query [...] every comparison of a `select`'s rows with SQLite [...] compares the rows as a multiset, never by order [...] 4. Forcing a path, for the checks. Generalise the test option that forces a full scan (`scanAll`, `Engine.SetScanAll`) into one that forces any access that can serve the query [...] 5. Counts. Pin \"rows read from the cache\" [...] 6. Two fixes in core from the independent review of #97 [...] 7. Telemetry [...] 8. Driver [...]"
**Interpretation:** Run the two statements with SQLite's errors, replace the access by the key of slice 7 with one function of the rules over the primary key and every index, draw a direction for each query, give the tests a path to force and a seed, and make every comparison of rows with SQLite a comparison of multisets.
**Inferred intent:** Slice 9 end to end, with the choice of an index as code that a model, planted bugs, and mutation testing can check later, and with no test left that depends on an order that SQLite does not promise.

### What I did

The base: `git checkout -b index-engine origin/index-core`, then `git merge origin/main`. Only `/internal/core/README.md` conflicted, in two bullets that both sides added, and both stay.

The storage interface of the engine, `/internal/engine/storage.go`: `reader.scan` takes a `core.IndexScan` and replaces `scan` and `scanRange`; `reader.indexes`, `reader.index`, and `reader.rowsRead`; `writer.createIndex` and `writer.dropIndex`. `fromCore` takes `core.ErrIndexExists` and `core.ErrNoIndex` for refusals that the engine checks first.

The rules, a new file `/internal/engine/access.go`: `access` (none, the point of a primary key, or a scan by the primary key or an index with pinned values, bounds, and a direction), `chooseAccess(t, indexes, conditions)`, and its parts `never`, `pointOf`, `pinOf`, `primaryKey`, `indexKey`, `fit`, and `tighter`, which moved from `operator.go`. `path`, `forcedAccess`, and `reading` are the test's way to force an access and the direction of one statement. `build.access` in `/internal/engine/operator.go` calls the rules first without the indexes, and reads the indexes of the table only if the answer is neither "no row" nor the row of a primary key.

The statements: the plans `CreateIndex` and `DropIndex` in `/internal/engine/plan.go`, the sinks `createIndex` and `dropIndex`, `buildCreateIndex` and `buildDropIndex` with SQLite's errors (`tableNamed`, `indexExists`, `noSuchIndex`, `indexNamed`, `indexTooBig` in `/internal/engine/errors.go`), and a create table under the name of an index. `indexRefusal` and its test are gone, and so is the call of `engine.Check` in the driver's `check` in `/conn.go`.

The pass: `stmt.CreateIndex.SQL`, the text that SQLite keeps for the index, which the engine needs to tell whether SQLite refuses the statement as too big under a lowered length limit; `checkCreateIndex` of the pass's tests compares it with `sqlite_schema`.

The engine: `Options.path`, `Options.seed`, and `Engine.reading`, which draws the direction of each query from the host, or from a splitmix64 sequence of the engine's own seed; the attributes `statement.access`, `statement.index`, and `statement.rows_read` of the statement's event, and the kinds `create_index` and `drop_index`.

The tests' fakes: `memTx` sorts the rows of a table by the key of an index for a scan, a second way to the rows that the fuzz tests then compare with SQLite; `fakeTx` records an index scan and a look-up of the indexes among its calls. `answer.String` in `/internal/engine/differential_test.go` gives the rows sorted, so every comparison of two answers is of multisets.

### Why

One function of the table, its indexes, and the conditions is what the brief asks for, and what a model can be compared with. The rules are called twice so that the common point lookup reads no index of its table: the first two rules do not depend on the indexes.

### What worked

- The merge of `origin/main` conflicted in one README only: the other files that the brief named had merged on `index-core` already.
- The engine built after the change of the interface with the compiler naming every place.

### What didn't work

- A Bash command with a here-doc of a Python script and a `go vet` after it was refused: "this command is too complex to verify that it stays inside the worktree". A script written with the Write tool to the scratchpad and run by its path worked, as the diaries before say.
- The first text of an index that the pass kept ended at the closing parenthesis, and `FuzzCheck/seed#6414` failed: SQLite keeps `"...desc\n /*-*/ )\n"`, up to the semicolon, as for a table (`sqlite3CreateIndex` reads `pParse->sLastToken`).
- The first rule for "no row" took a test for NULL of every column that does not take NULL. `stmttest.KeyAccess`, the model of slice 7, says "a scan of every row" for such a test of a column that is not the key, and its counts are pinned. The rule is slice 7's again, and such a test pins nothing.
- After the change, `go test -short ./internal/engine/` failed in 60 and more tests whose expected rows are in the order of the key, since each query now reads forward or backward.

### What I learned

- SQLite keeps the text of an index from its name to the semicolon or the end of the text, as it keeps a table's.
- The library code must draw randomness only from `host.Host.Uint64`, which `internal/simlint` checks, so a seed of the engine's own is a counter and a mix, with no import of `math/rand`.

### What was tricky

A test for NULL of a column that does not take NULL: the storage refuses a scan that pins such a column to NULL, so the rules must never pin one. `pinOf` asks the table.

### What warrants review

- `chooseAccess`, `fit`, `pinOf`, and `forcedAccess` in `/internal/engine/access.go`.
- `build.access` in `/internal/engine/operator.go`: that it reads the indexes only when the choice depends on them.
- `answer.String` in `/internal/engine/differential_test.go`: the claim that a sorted list of quoted rows cannot hide a wrong, a missing, or a doubled row.

### Future work

- The tests by hand that pin rows in the order of the key, the tests of the rules, of the statements, of the forced paths, of the order, and of the counts: the next steps.
- The lazy reading of the indexes in the core, which a sub-agent builds in a worktree of its own.

## Step 2: the core's lazy reading of indexes, the tests of the rules, of the statements, of every path, of the order, and of the counts

**Author:** builder (index-engine)

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "4. Forcing a path, for the checks. [...] With it, add to the engine's table tests a check that every forced path gives the same rows as SQLite for a set of shapes (all five types, null, descending columns, several columns, bounds reaching into the trailing primary key columns, `is null` pins). [...] 5. Counts. Pin \"rows read from the cache\" (`RowsRead` of core) in tests for a few named queries on a table of some thousand rows, with and without an index [...] The numbers must be exact and not depend on the direction chosen or on timing. [...] 6. Two fixes in core from the independent review of #97, on your branch: `conn.load`/`conn.loadIndexes` read a table's indexes at every first look-up of the table in a transaction [...] Read the indexes only when a call needs them [...] The check that refuses a foreign index in the cache must still run before anything is written to or read from that table through an index. [...] The doc of `RowsRead` and of `ScanIndex` says a scan reads no more rows than are within its bounds; where SQLite sorts [...] it reads every row in the bounds before the first step. Say so in the doc. [...] 9. Tests in the packages' style, compared with SQLite statement by statement; through the log: an index created by one instance is used by a second after replay, with the same choice of access."
**Interpretation:** Tests of each new piece beside SQLite, with SQLite as the judge of every row, error, and offset; the two fixes of the core in a branch of a sub-agent, merged here.
**Inferred intent:** Every claim of step 1 checked by a test that a wrong bound, a wrong direction, or a wrong choice fails.

### What I did

The core, by a sub-agent in a worktree of its own (branch `index-engine-core-lazy`, merged): `table.indexed`, `conn.lookUpWithoutIndexes` for `Get`, `Table`, and `Tables`, `table.checkIndexed` as the guard of everything that reads the indexes or writes a row, `conn.schemaReads` with `TestSchemaReads`, the foreign indexes of `TestOpen_cache_indexes` shown for each call, and the sort case in the doc comments of `ScanIndex` and `RowsRead`. I read its diff of `cache.go`, `read.go`, and `tx.go` before the merge. Then, myself: `conn.createIndex` and `conn.dropIndex` go through `conn.changeSchema`, under SQLite's largest length limit, and an index whose statement in the cache is longer than the limit is refused with `ErrInvalid` before anything changes, with a subtest of `TestTx_tooBig`.

The tests of the engine, each in a file of its own:

- `/internal/engine/access_internal_test.go`: `TestChooseAccess`, 61 cases of the rules on a table of one key column, one of two, and goqite's, written as text (`a = 1 and e > 2`) and expected as text (`i2 = 1 > 2`); each case also asks the rules without the indexes, and requires the same answer where either is "no row" or a point. `TestForcedAccess`, 11 cases. `TestEngine_reading`, with a host that counts its draws.
- `/internal/engine/index_test.go`: `TestEngine_indexes` with `indexPair`, the engine and SQLite side by side: each statement must give SQLite's answer and count of changes, and outside a transaction SQLite's rows and SQLite's indexes (`oracle.indexes` from `sqlite_schema` and `pragma_index_xinfo`, `instance.indexes` from the core). Twelve subtests: the statements on a table with rows; the order of creation through a drop, a replay, and a restart; 20 errors in SQLite's order; the offsets of seven errors on a fresh connection each; `drop table`; transactions with a roll back and a commit; a read-only transaction; open rows; 250 and 251 columns; the length limit; the events; and a replay with the same choice of access.
- `/internal/engine/access_test.go`: `TestEngine_paths`, `TestEngine_order`, and `TestEngine_rowsRead`.

The generated tests: every instance of the tests has a seed of the directions, which it logs when its test fails (`instance.seeded`, the flag `-directions`); the simulation derives the seed of each engine from the seed of the run; the second instance of a `pair` reads by the next of `otherPaths()`; the `checkText` wrappers are gone from the engine's tests and from the driver check.

The merges: `origin/index-core` again, which had merged `main`; `origin/update-delete-checks`, the branch of pull request #94, before it was merged, with three conflicts (`operator.go`, `tx.go`, `export_test.go`) that took my side and then its `bugs` field and its two planted bugs; and `origin/main` after #94 was merged, which merged cleanly.

### Why

A path that the tests can force, with a direction, lets SQLite's own `order by` of the key of the path say what the order of the rows must be, which is a stronger check than a multiset and needs no model of the order.

### What worked

- `TestEngine_indexes` passed on its first run after one fault of the test's own, the length limit among it: the register number `#1` of SQLite's INSERT was a guess from the source, and SQLite agreed at every length from 400 to 480 bytes in seven forms.
- `TestEngine_paths`, `TestEngine_order`, and `TestEngine_rowsRead` passed on their first run. So I planted faults by hand: `>=` read as open in `fit` failed `TestEngine_paths` and `TestChooseAccess`; one pinned column at most failed six cases of `TestChooseAccess`; the direction turned around in the core's `scanSQL` failed `TestEngine_paths` at once.
- The counts of `TestEngine_rowsRead` were the ones that I had worked out by hand: 22 rows of `a = 42 and b >= 3`, and 400 of 1,000 for the receive.
- The full `TestDifferential`, `TestConn_differential`, and the fuzz seeds passed with every pinned number as it was.

### What didn't work

- `TestChooseAccess` failed one case of my own: `Expected "primary = 2", but got "i = 1,2"`. An index on `(a)` of a table with the key `(k1, k2)` pins two columns for `a = 1 and k1 = 2`, and the primary key one, so the index wins; the case for the tie is now an index on `(k1)`.
- `oracle.indexes` first wrote the name of a table into its SQL in double quotes: `no such column: "t" - should this be a string literal in single-quotes?`. It binds the name now.
- The full `TestSimulation` failed after the first version: `50 seeds give 0 of "selects with WHERE of their own that give rows, compared with SQLite", and at least 1 were expected`, in the idle workload. The engine drew each direction from the host of the simulation, which moved every later draw of the scheduler. The simulation gives each engine a seed now, and the counts are those of before.
- `go test -short ./internal/sql/mutation/` failed: `no such mutant: internal/engine/bugs.go openBounds.scanRange condition: lower.open => true`. A script took 20 entries of functions that are gone out of `equivalent.txt`.
- That script first failed with a traceback through `scratchpad/operator.py`: a file of mine of that name in the scratchpad shadowed Python's `operator` module for every later script there. I deleted my scripts and ran the new one with `python3 -I`.

### What I learned

- A file named like a module of Python's standard library in the directory of a script breaks every script beside it.
- SQLite's count of changes and its offsets for the errors of a create index are those of a create table: -1 but for `no such column`.

### What was tricky

The second instance of a `pair` and the direction. Before, it read every row where the first read by the key, and both gave the rows in one order. Now each draws, and the pair compares multisets; the paths that it forces have a direction, so a failure names the path and can be had again.

### What warrants review

- `TestEngine_paths` in `/internal/engine/access_test.go`: the claim that SQLite's `order by` of the key of a path is the order of a scan by that path, also for NULL and for a descending column.
- `conn.createIndex` in `/internal/core/index.go`, and the merged `conn.lookUp`, `conn.lookUpWithoutIndexes`, and `table.checkIndexed` in `/internal/core/cache.go`.
- `run.open` in `/internal/engine/simulation_test.go`: the seed of each engine.

### Future work

- The tests by hand that pin rows in an order, the driver check, and the READMEs: three sub-agents, each in a worktree of its own.
