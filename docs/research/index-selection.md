# How engines pick an index

Date: 2026-11-20. Tags: **V** = verified in this run (read in source or docs with the reference, or run). **I** = inferred.

Question: s4ql gets secondary indexes. The log holds only the definition, and the cache holds a real SQLite index. Who picks the index for a query: s4ql's engine by fixed rules, with SQLite then running the scan on the named index, or the cache's SQLite? If rules, which rules? How do other engines do it, and what did they learn?

Short answer: rules in s4ql's engine, executed with `indexed by`. SQLite in s4ql's cache has no statistics, so its choice already comes from fixed guesses: 10 rows per `=`, a quarter of the rows per range bound, a million rows per table. Those guesses are invisible to s4ql's tests, change with SQLite's version, and pick the slow plan in the measured case of goqite below. The lead's rules are sound as a base. They need three additions: the primary key columns at the end of every index key, an `order by` rule, and a check that SQLite ran the scan that s4ql asked for. Every rule-based engine shares the bad cases: the rules cannot tell which of two filters is more selective, or whether reading in order beats filtering first. The choice never shows in results, so the rules can change later without a format change.

Setup: SQLite 3.54.1, the vendored amalgamation `internal/sqlite/c/sqlite3.c`. The shell `shell.c` was built in a scratch directory with `internal/sqlite/options.h`, so it is the build that the cache uses. That build does not set `SQLITE_ENABLE_STAT4` (`internal/sqlite/options.h`). Web sources were read through a fetch tool that summarises pages, so quotes from web pages were not diffed against the raw HTML.

## 1. SQLite

### How it chooses (V)

- It costs every candidate and keeps the cheapest. "SQLite tries to estimate the total amount of work needed to perform the query using each option" (https://sqlite.org/optoverview.html, section 8). For a join it keeps the N best paths at each step ("N Nearest Neighbors", https://sqlite.org/queryplanner-ng.html).
- An index is usable for `=` (or `in`, `is`) on its leading columns, then one range on the next column. "The right-most column that is used can employ inequalities", and "there cannot be gaps in the columns of the index that are used" (optoverview.html, section 2.1).
- The cost formulas are in the comment above `whereLoopAddBtree` in `sqlite3.c`: a full scan costs `nRow * 3.0`, and an index scan `nSeek * (log(nRow) + (K+3.0) * nVisit)` when it is not covering.
- The model is biased toward indexes on purpose: "scoring is designed to pick plans that 'do the least harm' if the estimates are inaccurate ... since the worst-case performance of using an index is far better than the worst-case performance of a full table scan" (same comment).
- A single-table query with `=` on a whole unique key skips the costing: "This routine attempts to plan those simple cases using much less ceremony" (`whereShortCut`). It is skipped under `indexed by` and `not indexed`.

### Without `analyze` (V)

- `sqlite3DefaultRowEst` fills in guesses: a million rows in the table, then "a[1] is 10, a[2] is 9, a[3] is 8, a[4] is 7, a[5] is 6 and each subsequent value (if any) is 5". These are the rows per value of the first one, two, three... columns of an index.
- A range bound cuts the rows to a quarter, and both bounds to 1/64: "an open-ended range query (e.g. col > ?) is assumed to match 1/4 of the rows ... a closed range ... 1/64" (`whereRangeScanEst`, `whereRangeAdjust`).
- A full scan pays 3.0 per row "to discourage the use of full table scans, since index lookups have better worst-case performance if our stat guesses are wrong" (`whereLoopAddBtree`).
- Sorting costs `N log N` with `N` cut to the `limit`. If the first terms of the `order by` come in order from the index, only the rest are sorted in blocks (`whereSortingCost`).
- Skip-scan needs statistics. The code requires `pProbe->hasStat1!=0`, and the docs say "a skip-scan is never used on a database that has not been analyzed" (optoverview.html, section 6). s4ql's cache never has statistics, so skip-scan never happens there.
- The guesses are fixed. Without `analyze`, the cost model is a fixed function of the statement and the schema (I, from the code above). The values bound to parameters enter only through stat4 (`stat4ValueFromExpr` is under `SQLITE_ENABLE_STAT4`), which the cache does not build.

### Ties (V, run)

- Of two indexes that cost the same, SQLite takes the one created last. With `create index u1 on u (a); create index u2 on u (a)`, `where a = ?` uses `u2`. With `tab (a, b)` then `ta (a)`, `where a = ?` uses `ta`, and with the opposite order it uses `tab`. The same held after the file was closed and opened again. The cause is inferred: `create index` puts the new index first in the table's list, and `whereLoopInsert` keeps the first of equal plans.

### `analyze`, stat1, and stat4 (V)

- `analyze` writes statistics to `sqlite_stat1`, and with stat4, samples to `sqlite_stat4`. "Running ANALYZE can cause SQLite to choose different query plans for subsequent queries", and "one can construct pathological cases where running ANALYZE could make some subsequent queries run slower" (https://sqlite.org/lang_analyze.html).
- For plans that stay fixed, the advice is to "never run a full ANALYZE nor the 'PRAGMA optimize' command in the application". Instead, copy a `sqlite_stat1` made on test data into the deployed databases (same page).
- With stat4, the full-scan penalty drops from 3.0 to 2.75 (`whereLoopAddBtree`), and a range is estimated from samples of the values (`whereRangeScanEst`).

### The planner stability guarantee (V)

- "SQLite will always pick the same query plan for any given SQL statement as long as" the schema does not change in significant ways, `analyze` is not rerun, and "the same version of SQLite is used". "The QPSG is disabled by default" (https://sqlite.org/queryplanner-ng.html). Turning it on stops SQLite from preparing the statement again for the bound values (`exprCompareVariable`, `SQLITE_DBCONFIG_ENABLE_QPSG` in `sqlite3.h`).
- So a new pinned version of SQLite may change plans. The guarantee holds only within one version.

### `indexed by` and `not indexed` (V)

- "The INDEXED BY phrase forces the SQLite query planner to use a particular named index". "If index-name does not exist or cannot be used for the query, then the preparation of the SQL statement fails". It "is not a hinting mechanism", and is meant "to raise a run-time error if a schema change ... changes the plan" (https://sqlite.org/lang_indexedby.html).
- In the code, `indexed by` puts the named index in place of the list of candidates (`whereLoopAddBtree`, the loop `pProbe=(pSrc->fg.isIndexedBy ? 0 : pProbe->pNext)`). A full scan of that index is allowed. `indexed by goqite_body` with `where queue = ?`, where `body` has nothing to do with it, gave `SCAN goqite USING INDEX goqite_body` with no error. So "cannot be used" is rare. Run.
- The primary key of a `without rowid` table can be named too. `indexed by sqlite_autoindex_goqite_1` gave `SCAN goqite`, the table's own b-tree. Run.
- `not indexed` does nothing on a `without rowid` table. `select * from t not indexed where a = ?` gave `SEARCH t USING INDEX tab (a=?)`. The docs say "no index shall be used", but the code reads the table's indexes in either case for a table without a rowid (`whereLoopAddBtree`: `else if( !HasRowid(pTab) ){ pProbe = pTab->pIndex; }`), and checks `notIndexed` only on the rowid path. Run and read. To force the table's own b-tree, name its autoindex, or put a unary `+` before the column (`+queue = ?` gave `SCAN goqite`).

### Indexes on a `without rowid` table (V, run)

- A secondary index holds the primary key columns after its own, and SQLite uses them. With `ta (a)` and primary key `k`, `where k > ? and a = ?` gave `SEARCH t USING INDEX ta (a=? AND k>?)`. The goqite index is therefore `(queue, priority desc, created, id)` to the planner. `where queue = ? and priority = ? and created = ? and id > ?` used all four columns.
- An index scan that is not covering seeks the primary key b-tree for each row. A covering one does not (`COVERING INDEX` in the output above).

### `order by` and `limit` (V)

- "SQLite attempts to use an index to satisfy the ORDER BY clause of a query when possible" (optoverview.html, section 10). `indexMightHelpWithOrderBy` adds the index scan as a candidate, and `wherePathSatisfiesOrderBy` checks the order.
- Directions: on `(queue, priority desc, created)`, with `queue = ?`, `order by priority desc, created` came in order forward, and `order by queue desc, priority, created desc, id desc` backward, with no sort in either. A mix, `order by priority desc, created desc`, gave `USE TEMP B-TREE FOR LAST TERM OF ORDER BY`, a sort in blocks. Run.
- `limit` lowers the cost of a sort (`whereSortingCost`), not the cost of a scan that can stop early. Run: with indexes `t2a (a)` and `t2b (b)`, `where a > ? order by b limit 10` scans `t2b` in order, and so does the same query without `limit`. `where a = ? order by b limit 10` searches `t2a` and sorts. A range is guessed at a quarter of the table, an `=` at 10 rows.

### Measured: goqite with a second index (V, run)

goqite's receive query runs `select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1` (goqite v0.4.0 `goqite.go`, `ReceiveTx`), with the index `(queue, priority desc, created)`. The table was made `strict, without rowid` and filled with 100,000 rows in 2 queues and 3 priorities.

- With only goqite's index, SQLite uses it and sorts nothing: `SEARCH goqite USING INDEX goqite_queue_priority_created_idx (queue=?)`. `not indexed` gives the same, as above.
- An added index `(queue, timeout)` wins: `SEARCH goqite USING INDEX goqite_queue_timeout (queue=? AND timeout<?)` and `USE TEMP B-TREE FOR ORDER BY`. That is the guess at work: one `=` plus a range is cheaper than one `=` and a sort cut to one row. After `analyze` the plan stays the same. `sqlite_stat1` then holds `100000 50000 1`, and stat1 says nothing about `timeout`.
- Time per query, forced each way with `indexed by` on the same file:

| Messages ready in the queue | `(queue, timeout)`, then a sort | `(queue, priority desc, created)`, in order |
|---|---|---|
| all 50,000 | 19.4 ms | 0.048 ms |
| none | 0.062 ms | 20.8 ms |

Each index has a bad case of about 400 times. In both, the bad index reads every row of the queue (I, from the plans). SQLite's guesses pick the first column of the table whatever the data, both before and after `analyze`.

## 2. PostgreSQL and MySQL (V, docs)

- PostgreSQL costs plans from statistics that `analyze` samples, and they "are always approximate even when freshly updated" (https://www.postgresql.org/docs/current/planner-stats.html). An estimate "can change after each ANALYZE command, because the statistics produced by ANALYZE are taken from a randomized sample of the table" (https://www.postgresql.org/docs/current/using-explain.html).
- Without statistics, PostgreSQL also guesses: `DEFAULT_EQ_SEL 0.005`, `DEFAULT_INEQ_SEL 0.3333333333333333`, `DEFAULT_RANGE_INEQ_SEL 0.005` (`src/include/utils/selfuncs.h`, https://github.com/postgres/postgres).
- Prepared statements: "the first five executions are done with custom plans", and later ones use the generic plan if it costs not much more (https://www.postgresql.org/docs/current/sql-prepare.html). So the plan of one statement can change after its fifth run.
- The `order by ... limit` failure is known. Tom Lane, on an "abort-early plan disaster": the estimate "effectively assumes that they're perfectly uniformly distributed; which is a good average-case assumption but can be horribly wrong in the worst case" (https://www.postgresql.org/message-id/13982.1412031637%40sss.pgh.pa.us, 2014). That is the goqite table above with the ready rows at the end of the order.
- Core PostgreSQL has no index hints: "We are not interested in implementing hints in the exact ways they are commonly implemented on other databases" (https://wiki.postgresql.org/wiki/OptimizerHintsDiscussion).
- InnoDB samples 20 pages by default (`innodb_stats_persistent_sample_pages`). Before persistent statistics, "different estimates could be produced when recalculating statistics leading to different choices in query execution plans and variation in query performance". They were stored so that "the optimizer is more likely to make consistent choices each time for a given query" (https://dev.mysql.com/doc/refman/8.0/en/innodb-persistent-stats.html).
- MySQL prefers an index that gives the order when there is a `limit`. The docs admit the rule "cannot handle every conceivable case (due in part to the assumption that the distribution of data is always more or less uniform)". Since 8.0.21 the rule can be turned off with the switch `prefer_ordering_index` (https://dev.mysql.com/doc/refman/8.0/en/switchable-optimizations.html).
- "An ORDER BY query with and without LIMIT may return rows in different orders", and tied rows may come "differently depending on the overall execution plan" (https://dev.mysql.com/doc/refman/8.0/en/limit-optimization.html).
- MySQL has `use`, `force`, and `ignore index`, for "if EXPLAIN shows that MySQL is using the wrong index" (https://dev.mysql.com/doc/refman/8.0/en/index-hints.html).

Lesson: statistics help on average and make plans move. Each of these engines later added a way to hold a plan still.

## 3. CockroachDB, TiDB, and Spanner (V, docs and blogs)

- CockroachDB's first planner ranked indexes by their structure: "the more restrictive our search ranges are, the better". Covering was "not the most important factor", and an index could be chosen because it gives "the results in the order we want". Its choices "don't depend on the data itself – only on its structure", and the post gives index hints as the way out when "the index algorithms don't correctly identify the best index" (https://www.cockroachlabs.com/blog/index-selection-cockroachdb-2/, 2016).
- The planning RFC names "a primitive Cost model during index selection" and argues for costs and statistics (https://github.com/cockroachdb/cockroach/blob/master/docs/RFCS/20171213_sql_query_planning.md). The cost-based optimizer shipped in 2.1 (https://www.cockroachlabs.com/blog/building-cost-based-sql-optimizer/, 2018). The reasons, as the fetch tool paraphrased the blog: the hand-written rules came to conflict, every new rule had to be checked against the old ones, and joins have crossover points that rules miss. Today statistics are made "when tables are created and after schema changes" (https://www.cockroachlabs.com/docs/stable/cost-based-optimizer).
- TiDB prunes candidate indexes by dominance before it costs them ("skyline pruning", "a heuristic filtering rule for indexes"). The dimensions are "How many access conditions are covered by the indexed columns", "Whether it needs to retrieve rows from a table", and "whether the index satisfies a certain order". A unique index matched in full by `=` that needs no lookup is chosen without cost (https://docs.pingcap.com/tidb/stable/choose-index).
- Spanner chooses by cost, but "in a few cases, Spanner might choose an index that causes query latency to increase", and "Google advises you to use FORCE_INDEX directives in your SQL statements for more consistent performance" (https://cloud.google.com/spanner/docs/secondary-indexes). Each optimizer version is a numbered release. A database can pin one, "to minimize any potential for performance regression when the query optimizer or statistics change", and so can a single query, with `@{OPTIMIZER_VERSION=9}` (https://cloud.google.com/spanner/docs/query-optimizer/manage-query-optimizer).

Lesson: CockroachDB left fixed rules for joins and crossover points, not for the single-table choice that s4ql has. TiDB still keeps rules in front of its costs. Spanner, with costs, tells users to name the index.

## 4. Rule-based planners and the caller's choice (V, docs and source)

- Oracle's rule-based optimizer ranked 15 access paths, from "Single Row by Rowid" to "Full Table Scan". "If there is more than one way to execute a SQL statement, then the RBO always uses the operation with the lower rank" (https://docs.oracle.com/cd/A97385_01/server.920/a96533/rbo.htm). Rank 8, "Composite Index", is above rank 10, "Bounded Range Search on Indexed Columns", which is above rank 14, "ORDER BY on Indexed Column". An `=` beats a range, and a range beats the order.
- It was retired in 10.1: "The rule-based optimizer is no longer supported in release 10.1" (https://docs.oracle.com/cd/B13789_01/server.101/b10763/compat.htm). The pages read give no reason beyond the cost-based optimizer being the default. Oracle later added SQL plan management, which "prevents performance regressions caused by plan changes" (https://docs.oracle.com/en/database/oracle/oracle-database/19/tgsql/overview-of-sql-plan-management.html).
- The FoundationDB Record Layer plans by fixed scores, not statistics. `RecordQueryPlanner.java` compares candidates as "better if higher score (for indexes the number of sargables)", then by fewer residual filters, and says "index is safer without knowing any cardinalities" (https://github.com/FoundationDB/fdb-record-layer). It refuses an `order by` that no index gives: `UnableToPlanException` "if there is no index that matches the sort", unless an in-memory sort is allowed. The paper says ordered queries work "only when there is an available index supporting the requested sort order" (https://arxiv.org/abs/1901.04452, section 3.1).
- MongoDB races the candidates. "The winning plan is the query plan that produces the most results during the trial period while performing the least amount of work", the winner is cached by query shape, and a cached plan that stops meeting its mark becomes inactive (https://www.mongodb.com/docs/manual/core/query-plans/). `hint()` overrides the choice.
- DynamoDB lets the caller choose: a query names its index (`IndexName`) and needs "a single value" for the partition key. A scan is a separate call (https://docs.aws.amazon.com/amazondynamodb/latest/APIReference/API_Query.html). Firestore refuses a query that no index fits: "you receive an error", with a link to create the index (https://cloud.google.com/firestore/docs/query-data/indexing).

Lesson: rules without statistics are in use where the workload is known and the index was made for it. Racing plans would make the plan depend on timing, and refusing would make SQL that SQLite accepts fail (I).

## 5. Systems on SQLite (V where cited)

- Systems that use SQLite as a replica or cache send it the application's SQL and let it plan. That holds for the systems in [sqlite-cache-and-oracle.md](sqlite-cache-and-oracle.md) (I, since none of them has a planner of its own).
- Turso, a rewrite of SQLite in Rust, has its own planner. It costs plans, "uses `sqlite_stat1` data after `ANALYZE`", and "without statistics, the model uses fixed fallback values. The main fallback assumes one million rows per table" (https://github.com/tursodatabase/turso, `core/translate/optimizer/OPTIMIZER.md`). It copies SQLite's guesses and adds `EXPLAIN QUERY PLAN FORMAT=JSON` with the estimates.

## 6. For s4ql

### Rules in s4ql, or let the cache's SQLite pick

The spec settles most of it: "The engine parses, plans, and executes SQL itself. SQLite does not run the application's SQL." Letting SQLite pick means sending it the application's conditions and, for the order, its `order by`.

Letting the cache's SQLite pick:

- Correctness risk: low for rows, if s4ql still filters every row. But s4ql's tests cannot see the choice without reading `explain query plan` text. The choice depends on SQLite's version (the QPSG holds within one) and on the order in which the indexes were created (newest wins). It would also depend on statistics if any were ever gathered. If s4ql does not know whether the rows come in order, it must sort them again, or let SQLite sort, which runs the application's `order by` in SQLite.
- Performance risk: SQLite's guesses without stat4 picked the plan that was 400 times slower in the goqite case, with and without `analyze`. SQLite's guesses are rules too, only rules that s4ql cannot see or pin.

s4ql picks by rules and names the index with `indexed by`:

- Correctness risk: s4ql turns conditions into bounds itself. Here a bug can drop rows: a bound on a `desc` column turned around, an open bound read as closed, the primary key suffix forgotten. A bound that is too wide is harmless, because the filter tests every condition again (as in slice 7). This risk is the same kind that slice 7 already has, and it is testable path by path (below). SQLite may still run the named index as a full scan, so a test must check the bounds it used.
- Performance risk: the bad cases of every rule set, listed below. They are the same as SQLite's own without statistics, but known and visible.
- Gain: the plan is a function of the statement and the schema in the log. It is the same on every instance and every pinned SQLite, and the simulation and mutation testing can check it as code.

Mimicking SQLite's exact choice and checking it against `explain query plan` is a third way. It would copy fudge factors, row sizes, and the newest-wins tie, and it would copy the goqite choice above. As a report of differences it is useful. As a check that fails it would be brittle (I).

### The rules

Terms: the key of an index is its columns followed by the primary key columns that it does not already hold, each with its direction. The primary key is a candidate with its own columns as its key. A column is pinned by `=` or by `is null`. SQLite uses `is` as an equality for an index (optoverview.html, section 2), and a `null` is a value in the index.

1. A condition that is never true (a comparison with null, the key `is null`) reads nothing. This is slice 7's rule.
2. `=` on every primary key column: a point lookup by the primary key.
3. For each candidate, count `p`: how many leading key columns are pinned. Note `r`: whether the next column has a range. Note `o`: whether the rest of the key, after the pinned columns, gives the `order by`. That means each `order by` column is the next key column, or is pinned, and every direction matches, or every direction is reversed.
4. The largest `p` wins.
5. With equal `p`: if the statement has `limit` and `order by`, a candidate with `o` beats one with `r`. Without `limit`, `r` beats `o`. The reason: with the same `=` prefix, both read at most the rows of that prefix. The ordered one can stop after `limit` rows and holds none for a sort. Without `limit`, it reads every row of the prefix, and the range reads fewer.
6. With `p = 0` and no `r`: a candidate with `o` beats a full scan of the table. A sort would hold every row, and the index holds none. SQLite does the same (`order by b` gave `SCAN t USING INDEX tb`).
7. Ties: the primary key first, since it needs no second seek. Then a covering index. Then the index created first. The order of creation is in the log, so it is the same on every instance. Ties are rare, and this rule can change freely.
8. Nothing usable: a full scan of the table, as now.

The cache then runs `select ... from t indexed by i where <bounds> order by <key columns, in the direction of the scan>`.

For goqite, rule 4 alone picks its index. With an added `(queue, timeout)` index, rule 5 keeps goqite's index, where SQLite would take the other.

Tied rows still have to vary (spec, "Unspecified row order varies"). When the index gives the order, a backward scan reverses the `order by` too, so it does not vary ties. The variation has to come from the key columns after the `order by` columns. SQLite can sort that last part in blocks (`USE TEMP B-TREE FOR LAST TERM OF ORDER BY` for `order by priority desc, created, id desc`, run), or the engine can reverse each tied group. Either way, only one group is held at a time. This is decided when slice 12 meets indexes.

### The bad cases of rules

- Two filters that rules cannot rank: `where a = ? and b = ?` with one index on `a` and one on `b`. If `a` has two values and `b` is nearly unique, the choice of `a` reads half the table. A composite index fixes it.
- An `=` on a column with few values against a range on a selective column: rule 4 takes the `=`.
- Order against filter, with the same prefix: the table in section 1. Rule 5 takes the order, so its bad case is a queue with nothing ready. For goqite, that read is bounded by the rows of the queue.
- An index on a column where most rows share the value. Each row costs a second seek, so the scan is slower than a full scan by a constant and a log factor, not by the size of the table. That is SQLite's "least harm" argument for preferring an index (I, from `whereLoopAddBtree`).
- No plan flips: a rule never changes its choice because data or statistics changed. That is the bad case of every cost-based engine above, and rules avoid it.

### How a developer finds out

- Now: the engine's statement event (`internal/engine/telemetry.go`) gets an attribute for the access, such as `index goqite_queue_priority_created_idx (queue=?) ordered`. That shows in the log and the traces and binds nothing.
- Later, if wanted: `explain query plan`. By the rule "whatever the engine accepts behaves exactly as in SQLite", its output would have to be SQLite's. s4ql could get SQLite's text for s4ql's own plan by running `explain query plan` on the cache's statement with `indexed by`. That text would still differ from what SQLite plans for the original statement where the choices differ. So this needs a decision of its own.
- `indexed by` and `not indexed` in the application's SQL: rejected as not supported for now. If they come later, SQLite's own behaviour comes with them, including the quirk that `not indexed` does nothing on a `without rowid` table.

### What binds now, and what is free

Binds a format or public behaviour:

- The definition in the log: name, table, columns in order, direction per column. Agreed already. Index names share SQLite's namespace with tables, and the reserved prefixes `sqlite_` and `s4ql_` apply.
- The errors of `create index` and `drop index`, by SQLite.
- Whether `indexed by`, `not indexed`, and `explain query plan` are accepted. Rejecting them now keeps every choice open.

Free to change later:

- The rules and their tie-breaks. By the spec, the order of unordered and tied rows varies, so no result can show the choice. A change of rules changes only speed. Speed is not promised, but a change makes some query slower, so a rule change should be noted in the release.
- The SQL that the engine sends to the cache, and the cache's layout. The cache never gets statistics: s4ql does not support `analyze`, and the cache can be rebuilt from the log.

### How to verify it

- Every path, every query. The generated tests and the fuzz targets make schemas with several indexes, including `desc` columns and indexes that share leading columns. For each `select`, they run every candidate: the full scan, the primary key range, and each index with each usable prefix, forward and backward. Each must give the rows that SQLite gives, as a set, and in order where `order by` fixes the order. This extends slice 7's test, which runs each select by the key and by a full scan.
- The rule against a model. A small, separate model of the rules in the tests says which candidate wins, and the plan must match it. Slice 7 already checks that "the plan reads by the key as the rule says, which no comparison of rows can see".
- That SQLite ran the scan as asked. In the tests, the cache's statement goes through `explain query plan`. The output must name the index and the expected constraint, such as `(queue=? AND priority>?)`, and must have no `USE TEMP B-TREE` where s4ql relies on the order. A count of the rows that the cache gives against the rows that match finds a scan that read too much.
- Planted bugs and mutants. Plant a bound turned around on a `desc` column, an index key without its primary key suffix, and a rule that ignores `limit`, and require the simulation to find each. Mutation testing of the rule code follows, as for every slice.
- The index matches the table. The simulation runs `pragma integrity_check` on the cache after replay and after crashes. It checks every index against its table.
- Golden plans. A pinned list of queries, goqite's receive among them, with the chosen access. A rule change then shows as a diff.

## 7. Recommendation

1. s4ql's engine picks the index by fixed rules, and the cache runs the scan on it with `indexed by`. Do not let the cache's SQLite pick. Without statistics, SQLite's choice is also fixed guesses, but hidden, tied to its version, and wrong in the measured goqite case. Letting it pick also goes against the spec's rule that SQLite does not run the application's SQL.
2. Use the lead's rules with these changes. Count the primary key columns at the end of each index key. Count `is null` as a pin. With the same `=` prefix, an index that gives the order beats a range when there is a `limit`, and loses to it without one. With no prefix, an index that gives the order beats a full scan. Ties go to the primary key, then a covering index, then the index created first.
3. Gather no statistics, and keep `analyze` out of the cache.
4. Reject `indexed by`, `not indexed`, and `explain query plan` in the application's SQL for now. Show the chosen access in the statement's telemetry.
5. Verify by running every candidate path on every generated query against SQLite. Check the choice against a model of the rules, and check with `explain query plan` that SQLite ran the bounds that s4ql asked for.

## Files read

- `internal/sqlite/c/sqlite3.c`: `sqlite3DefaultRowEst`, `whereRangeAdjust`, `whereRangeScanEst`, `whereLoopAddBtreeIndex` (skip-scan), `whereLoopAddBtree`, `indexMightHelpWithOrderBy`, `whereSortingCost`, `whereShortCut`, `sqlite3IndexedByLookup`, `exprCompareVariable`, `estLog`
- `internal/sqlite/options.h`
- `internal/engine/operator.go` (`keyAccess`), `internal/engine/telemetry.go`, `internal/core/read.go` (`ScanRange`), `internal/core/cache.go` (`conn.scan`)
- `/Volumes/990/go/mod/maragu.dev/goqite@v0.4.0/goqite.go` and `schema_sqlite.sql`
- `docs/spec.md`, `docs/roadmap.md` (slices 7, 8, 11, 12), `docs/research/glue-on-s4ql.md`
