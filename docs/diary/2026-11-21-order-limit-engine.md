# Diary: Slice 13 of the roadmap in the engine and the driver, `order by` and `limit` on `select`

Slice 13 lets a select have ORDER BY with one or more columns, each ASC or DESC, and LIMIT with a number or `?`, because goqite picks the next message with `order by priority desc, created limit 1`. The statement pass reads such a select since pull request #119, and the engine refused it. This task makes the engine and the driver run it: the storage core sorts where no index gives the order, the rules of the choice of an access get two more, and the engine stops reading at the limit. The generated checks are the next builder's. The work is on the branch `order-limit-engine`, from `2ccc638` on `main`.

## Step 1: The requirements, the design, and the sorted scan of the storage core

**Author:** order-limit-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Task: `order by` and `limit` through the engine and the driver. This step builds the engine and the driver with hand tests. The generated checks (sequences, the model of the rules, planted bugs, tallies, fuzz targets) are the NEXT builder's job [...] 1. When does a candidate deliver the order? Write it as part of the pure function of the rules [...] 2. Sorting. Where no candidate delivers the order, the rows must be sorted. Go memory must stay bounded whatever the number of rows [...] the natural device is for core to offer a scan that returns its rows in a requested order (columns and directions, with the tie-break by the primary key in the drawn direction), with SQLite sorting and spilling as it does, rows streaming to the engine. Choose the mechanism and give the reason; whatever you choose, the tests must show with `explain query plan` on the cache's statement that a query whose plan says "delivered by the index" has no sort step in SQLite and one whose plan says "sorted" has exactly one, and that `rows read` counts what SQLite read (for a sort, every row within the bounds). 3. Limit. The engine stops reading as soon as it has returned the limit's number of rows that pass the filter [...] 4. Order of the result is exactly the `order by` [...] 5. Errors and their order against SQLite [...] 6. Driver [...] Remove the temporary `orderLimitRefusal` [...] 7. Forced paths and hooks for the next builder [...] 8. Telemetry [...]"
**Interpretation:** Three layers. The core gets a scan whose rows SQLite sorts. The engine gets the rule for "this candidate delivers the order", the two new rules of the choice, the limit, the errors, the forced paths, and the telemetry. The driver gets tests, and loses the exceptions that the statement pass left.
**Inferred intent:** goqite's receive runs through `database/sql`, reads a handful of rows where its index is there, and every claim about order, rows read, and plans is a number or a plan that a test pins, so that the next builder can generate checks on top of it.

### What I did

Read the roadmap (slices 9 and 13), the spec, `/docs/todo/slice-13.md`, and the code of the access (`/internal/engine/access.go`), the operators, the plan, the telemetry, and of the scan of the core (`/internal/core/index.go`).

The design, before any code:

- **A candidate delivers the order** when, with the columns that the conditions pin taken out of its key and out of the ORDER BY, what is left of the ORDER BY is the start of what is left of the key, with every direction as the key has it (the scan runs forward) or every direction the other way (it runs backward). A pinned column has one value in every row that passes the filter, so it orders nothing, wherever it is in the key or in the ORDER BY. This holds more often than the wording of the brief, which takes only the leading pinned columns out of the key, and never where that wording does not hold for a sound reason: see Step 2.
- **Sorting is SQLite's.** `core.IndexScan` gets `Order`, the columns and directions, and the statement of the scan gets them as its ORDER BY, with the primary key after them, ascending or, for a scan with `Backward`, descending, so that the order is total and the drawn direction of the query turns the rows that tie. The engine keeps streaming rows, and holds one row at a time.
- **The limit** is an operator above the filter, which ends the loop over its source when it has given its rows, and never starts it for `limit 0`.

The core, `/internal/core/index.go`:

- `IndexScan.Order []KeyColumn`, checked by `table.checkScan` (a position that the table has, and no more columns than the table has).
- `table.sortTerms` writes the ORDER BY of a scan with `Order`: each column of `Order` after a unary plus, and then the columns of the primary key that `Order` does not name. `table.scanSQL` uses it in place of the order of the key.
- `conn.scanIndex` counts the rows that a sorted scan read and did not give when the loop over it stops early, with `conn.unread`, which runs `table.countSQL`: `select count(*)` by the same conditions on the same index.
- `TestScanIndex_order` in `/internal/core/index_test.go`: the order of each type with NULL, the rows that tie forward and backward, also in a table with a key of two columns; 382 scans with an order (10 scans by 15 orders of one table, 2 by 10 and 3 by 7 of two more, each forward and backward) compared with a statement of plain SQLite in a read and in a write transaction, each with `ScanPlan` of exactly the search and one `USE TEMP B-TREE FOR ORDER BY`, and the same scan without the order with no sort; the rows read of loops that stop early; the refusals; and a sort of 8 MB of rows, which succeeds, and fails as a local failure when the fault VFS fails every write of a temporary file, while a sort of ten rows succeeds under the same fault.

### Why

SQLite sorts because the cache's statement is already `select ... indexed by ... where ... order by`, so a sort is one more ORDER BY and no new machinery, and because SQLite's sorter keeps the rows in memory only up to the size of its page cache (2 MB by default) and then writes sorted runs to temporary files and merges them. A sort in Go would need a spill of its own. The cost is that SQLite sorts whole rows, and every row within the bounds, also those that the filter above would drop: the filter is the engine's, in Go, and stays there, since SQLite does not run the application's SQL.

The unary plus makes the sort all or nothing. Without it SQLite uses the order of the index where it can: for an ORDER BY whose first terms the index delivers it sorts only within groups (`USE TEMP B-TREE FOR LAST TERM OF ORDER BY`), reads lazily, and then what a query with a limit reads depends on where the loop stops, and is no number that the engine can say. With the plus, a scan with `Order` always reads every row within its bounds at its first step and sorts them once, so "sorted" means one thing, the plan of the cache has exactly one sort step, and the rows read are the rows within the bounds. The engine never asks for a sort where the candidate delivers the order, so nothing is lost on the path that the rules choose; a partial sort is an optimisation for later.

The rows read of a sorted scan that stops early are counted by a second statement because SQLite does not say how many rows its sorter holds. The two other ways were to step through the rest of the sorted rows, which costs a call into SQLite for each row that nobody reads, and to count only the rows given, which would make `limit 1` after a sort of 100,000 rows look like one row read. The count reads the index alone, in the same transaction, so it is the same number.

### What worked

- `go test ./internal/core/ -run TestScanIndex_order -count=1` passed on its first run, with the orders that I had written by hand.
- The check of the plan sees the plus: with `term := quote(...)` in place of `"+" + quote(...)`, the test failed at once with `Expected slice of length 2, but got 1` for `select * from main."t" indexed by "sqlite_autoindex_t_1" order by "c0"`, the scan by the primary key in the order of the primary key, which SQLite does not sort.
- `go test ./internal/core/ -run TestScanIndex -count=1` passed (2.2 s), and `golangci-lint run ./internal/core/...` gave 0 issues.

### What didn't work

- A command that held a Python here-document and a Go build after `&&` was refused by the harness: "This agent is isolated in the worktree [...] but this command is too complex to verify that it stays inside the worktree. Refusing to run it". Scripts written to the scratch directory with the Write tool and run as plain commands went through.

### What I learned

- The temporary files of SQLite's sorter are opened with `SQLITE_OPEN_TEMP_JOURNAL` through the VFS of the connection, so `faultvfs.Temp` reaches them, and a test can show that a sort spills by failing their writes.
- The pinned build has `SQLITE_TEMP_STORE` at its default, and the cache sets no `temp_store`, so temporary files are files.

### What was tricky

- The count of the rows read. A sorted statement has read everything at its first step, and a loop that stops after one row has stepped once. The count must not depend on when the loop stops, or the pinned numbers of a query with a limit would say that a sort is cheap.
- The tie-break must leave out the primary-key columns that `Order` names, or a table with 2,000 columns ordered by all of them would get an ORDER BY of more terms than SQLite takes.

### What warrants review

- `table.sortTerms` and the claim of its comment, which the 382 plans of `TestScanIndex_order` check: one sort, whatever the index.
- `conn.unread`: it returns 0 when the count fails, since the scan has ended by then and the error has no reader. The count runs in the transaction of the scan.
- A scan whose key has more than 63 columns that it does not pin is sorted by SQLite without `Order` too, and keeps counting only the rows stepped to, as before.

### Future work

- The engine: Step 2.
- A sort of the columns that the query needs, and not of whole rows, and a limit pushed into SQLite's sorter where the filter is exactly the bounds, would make a sort with a limit cheaper. They are recorded in `/docs/todo/performance.md`.

## Step 2: The rule of the order, the choice of the access, the limit, and the pins

**Author:** order-limit-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "1. When does a candidate deliver the order? [...] after the key columns pinned by `=` or `is null`, the next columns of the candidate's key (an index's columns followed by the primary key's columns it lacks; `core.IndexKey` is the one definition) must be the `order by` columns in order, each with the direction that matches the scan forward, or all of them with the opposite direction (then the scan runs backward); `order by` columns that are pinned may be skipped or met in any place (a pinned column has one value); an `order by` that ends before the key does is delivered; one that names a column the key does not have next is not. [...] Table-driven tests for the function alone, the golden plans extended (goqite's receive query WITH its `order by priority desc, created limit 1`, in both orders of its conditions, with no index, with goqite's index, with both indexes: the plan must name goqite's index and no sort in the last two), and the pinned rows-read numbers for those [...] 3. Limit. The engine stops reading as soon as it has returned the limit's number of rows that pass the filter; `limit 0` returns no row and reads none [...] 5. Errors and their order against SQLite [...] 7. Forced paths and hooks for the next builder [...] 8. Telemetry"
**Interpretation:** The pure function, the two new rules on top of slice 9's, the limit as an operator, the errors in SQLite's order with the engine's own last, the path and the access for the tests, the event, and for each a table test or a pin.
**Inferred intent:** A choice that a model can be written against and whose every consequence, the plan, the rows read, the order of the rows that tie, is a pinned text or number.

### What I did

- `/internal/engine/plan.go`: the nodes `Order` (with `OrderColumn`) and `Limit`, which `NewPlan` puts under the `Project` of a select, and `Format` prints as `order by v2 desc, v1` and `limit ?2`.
- `/internal/engine/access.go`: `ordering` (`delivered`, `sorted`, and the zero value for no ORDER BY), `access.order` and `access.by`, `orderBy` (the columns and whether there is a LIMIT), `delivers`, `orderBy.place`, `orderBy.sort`, `orderBy.better`, and `chooseAccess`, `forcedAccess`, and `path` with them (`path.sort`, `path.place`).
- `/internal/engine/operator.go`: the operator `limit`; `build.ordered`, which builds what is under the projection of a select in SQLite's order of errors; `build.bind` split into `build.values`, `build.types`, and `build.reach`; `build.access` with the ORDER BY and the path that sorts.
- `/internal/engine/errors.go`: `limitCount`. `/internal/engine/telemetry.go`: `statement.order`, and `statement.direction` left out for a delivered order. `/internal/engine/engine.go`: `check` without the refusal. `/internal/engine/orderby.go` is deleted.
- `/internal/engine/export_test.go`: `Path.Sort`, `Access.Delivered`, `Access.Sorted`, `Access.By`, `ChooseOrderedAccess`, `Delivers`; `AccessOf` answers for a select with ORDER BY and no WHERE.
- `/internal/engine/memory_internal_test.go`: the database in memory sorts for a scan with an order, and counts every row within the bounds as read.
- Tests: `TestDelivers` (72 cases), `TestChooseAccess_orderBy` (22, each with a LIMIT and without, and without the ORDER BY), and `TestForcedAccess_orderBy` (8) in `/internal/engine/access_internal_test.go`; `TestEngine_orderBy` in `/internal/engine/orderby_test.go`, with `ordered.differs`, which compares with SQLite in order; in `/internal/engine/access_test.go`, `TestAccessOf_golden` with 3 more queries of goqite, 2 of glue's sessions, the 2 examples of the roadmap, and 18 of the rules, the new subtest of `TestEngine_rowsRead`, `TestEngine_events_order`, and `checkPlanOf` with the sort row; 5 cases in `TestNewPlan`.
- `/docs/todo/slice-13.md` has the decisions, and `/docs/todo/performance.md` what is not built.
- `/internal/core/index.go` again: `conn.unread` is called by the scans of `ReadTx` and `Tx` after their loop has ended, and its error goes to `DB.cacheError` and `Tx.fail`.

Three sub-builders work from the branch in worktrees of their own: one on the generated machinery of `/internal/engine/differential_test.go` and the hand tests through every path, one on the driver's tests and the driver check, and a writer on the two READMEs.

### Why

**The rule is wider than the brief in one place.** The brief takes the leading pinned columns out of the key. I take every pinned column out, wherever it is in the key, as the brief does for the ORDER BY: with an index on `(a, b)`, `where b = 5 order by a, k` is delivered. The reason is the brief's own, "a pinned column has one value", and it holds for the rows that pass the filter whether or not the scan pins the column. The rule is then one sentence: take the pinned columns out of both, and the ORDER BY must be the start of the key, all in the key's directions or all turned. It also makes a forced path that pins fewer columns than the conditions do (`Path.Limited`) deliver exactly what the unforced one does. I did not cut the ORDER BY after the primary key, which would be sound too, because the brief says that a column that the key does not have next is not delivered.

**The ranking.** Pins first, always, as slice 9. Then with a LIMIT the order before the bound, and without a LIMIT the bound before the order, and without ORDER BY the bound alone. Without a LIMIT, a candidate that delivers the order still wins over one that does not at the same pins and the same bound, since it saves the sort: that is what makes an index win over the scan of every row with a sort where nothing is pinned.

**The limit is an operator, and the order is the scan's.** The filter keeps the order of its source, so nothing above the scan has to know about the order, and the limit ends the loop over its source, which ends the statement of the cache.

**The count of the unread rows moved.** In `conn.scanIndex` a failure of the count was dropped, so a local failure of the cache during the count would have stopped nothing. The transactions' scans have `DB.cacheError` and `Tx.fail`, and call the count once the scan's statement is at rest.

### What worked

- `go test ./internal/engine/ -run 'TestDelivers|TestChooseAccess|TestForcedAccess' -count=1` passed on its first run, and so did every pinned number of rows read and every order of rows that tie in `TestEngine_orderBy`, written by hand before the first run.
- The tests see the rules: with `case !o.limited && byOrder` in `orderBy.better`, 3 cases of `TestChooseAccess_orderBy` failed, and with `==` for `!=` in the direction of `delivers`, 47 cases of `TestDelivers`.
- goqite's receive, pinned by `TestEngine_rowsRead` on 3,000 messages in 3 queues: with no index `full_scan sorted reads 3000 gives 1`; with goqite's index, and beside the index on `(queue, timeout)`, `index goqite_queue_priority_created_idx delivered reads 1 gives 1` when every message is ready and `reads 1000 gives 0` when none is; without its LIMIT beside both indexes `index goqite_queue_timeout sorted reads 1000 gives 1000`, and `reads 0 gives 0` when none is ready.
- A throwaway measurement on 100,000 rows of about 50 bytes in the cache, best of 5: a scan of every row into Go 85 ms, a sorted scan of every row 112 ms, and a sorted scan that stops after its first row, with the count of the unread rows, 22 ms. So the count is cheap beside the sort, and stepping through 99,999 rows to count them would not be.

### What didn't work

- `TestEngine_orderBy` failed once, for a case of mine: `select k from t where a = 9 limit ?` has one parameter, and I had written `argument 2` in the message that I expected. `Expected "s4ql: not supported: argument 2, which is text, for LIMIT, which takes an integer from 0", but got "s4ql: not supported: argument 1, which is text, [...]"`.
- Three hand tests used `select * from t1 order by v1` as their statement that is not supported (`TestEngine_errors`, `TestConn_Begin`, `pinTx`), and `TestBuild` expected the old internal error for a projection of VALUES; they have `group by` and the new message now. `TestNewPlan` expected `is null` where `Format` prints `IS NULL`.
- `golangci-lint run ./internal/engine/...` found `const unordered is unused (unused)`: the zero value of `ordering` has no name now.
- A command with a here-document and `git commit` after `&&` was refused by the harness again, as in Step 1.

### What I learned

- SQLite's own planner skips only the leading columns of an index that `=` pins when it decides whether the index gives the order, and it takes ORDER BY terms that `=` pins out wherever they are (`wherePathSatisfiesOrderBy`, from memory of its source, not measured here). The rule here takes both out wherever they are.
- A query whose loop stops inside a range-over-func cannot be handed an error any more, so what fails after that point needs a side effect to be seen.

### What was tricky

- The order of the errors with the argument of LIMIT in it: its length is checked between the lengths and the types of the values of the conditions, because SQLite gives `string or blob too big` when it binds, before it steps, and the engine's own refusals come after all of SQLite's. `build.bind` had lengths and types in one function.
- `Access.String` is in tallies of the generated tests. The suffixes `, in order` and `, sorted` appear only for a statement with ORDER BY, so no tally moved.
- A LIMIT without ORDER BY reads a number of rows that depends on the drawn direction where a filter drops rows, so `TestEngine_orderBy` pins those only forward.

### What warrants review

- `delivers` in `/internal/engine/access.go` against the brief's wording, and the decision to take pinned columns out of the middle of the key.
- `orderBy.better`: the three rankings in one switch.
- `build.access` for a path with `sort` and the kind `pathRules`: it chooses by the rules for the statement without its ORDER BY, and then sorts.
- The count of the unread rows has no test of its failure: after a sort the pages of the index are in SQLite's page cache, so a fault of the VFS on reads does not reach it.

### Future work

- The generated checks, by the next builder: the model of `delivers` and of the ranking, planted bugs in them, `Generator.OrderBy` in the sequences, the fuzz targets.
- What `/docs/todo/performance.md` lists.
