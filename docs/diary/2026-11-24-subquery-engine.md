# Diary: Slice 15 of the roadmap in the engine and the driver, the scalar subquery

Slice 15 adds the scalar subquery of goqite's receive, `update goqite set timeout = ?, received = received + 1 where queue = ? and id = (select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1) returning id, body, received`, without the arithmetic in SET. The statement pass is done on `slice-15-stmt`: it reads the subquery into `stmt.Select.Subqueries` and `stmt.Term.Subquery`, and the engine and the driver refuse it. This task is the engine and the driver: plan and run the subquery, remove the refusals, and check it against SQLite. The work is on the branch `slice-15-engine`, from `slice-15-stmt` with `main` merged.

## Step 1: The brief, SQLite's behaviour, and the design

**Author:** subquery-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Make the engine plan and run a statement with a scalar subquery, remove the refusals, and make it work through the driver: `select`, `update`, and `delete`, with arguments, prepared statements, transactions, `returning`, and after a restart with an empty cache. [...] Once, before, same view. [...] No row gives null [...] Several rows give the first. Where the subquery's `order by` does not decide which row is first (none, or a tie), s4ql follows the spec's rule on unspecified order [...] Correlated subqueries are refused with a clear error [...] Exact types. [...] for every pair of types, s4ql gives SQLite's rows or refuses with a clear error. [...] Plans. The subquery is planned as a select is [...] It must not read or hold more than a select with `limit 1` does. [...] Telemetry. One event per statement, as today. Add what someone needs who asks "why was this receive slow or empty" [...]"
**Interpretation:** Build the subquery into the plan, the operators, and the telemetry of the engine, with SQLite's errors in SQLite's order, a draw for the first row where the order is not decided, and the refusals of the slice; then make the differential tests take a subquery, and check the driver with goqite's receive.
**Inferred intent:** goqite's receive running on s4ql with the same trust as the slices before.

### What I did

Read the roadmap's section of slice 15, `/docs/todo/slice-15.md`, the open items in `/docs/todo/sql-front-end.md`, the diary of the statement pass, the notes of the engine step of slice 14, and the engine's plan, operators, access, telemetry, and transactions. Built the pinned shell (3.54.1) in the scratchpad and measured the order of SQLite's errors with three scripts.

### Why

The order of the errors and where SQLite looks a name up decide the phases of the build of a subquery.

### What worked

The facts, which go into `/docs/todo/slice-15.md`:

- In a select, SQLite expands `*` of every subquery before it resolves a name, so `row value misused` for `*` of several columns comes at the comparison, before its column (`nope = (select * from u)` gives `row value misused`), before the deferred error of a test for NULL of a missing column (`nope is null and k = (select * from u)`), and before the names inside the subquery (`k = (select * from u where nope = 1)`), but after the select list (`select nope from t where k = (select * from u)` gives `no such column: nope`).
- Inside the subquery, the names come in its select list, its WHERE, and its ORDER BY, with the deferred error of a test for NULL as in a statement: `(select nope3 from u where nope is null order by nope2)` gives `nope3`, and `(select k from u where nope is null order by nope2)` gives `nope`.
- In an update and a delete, `sub-select returns N columns - expected 1` comes from SQLite's code generator, whose order follows its plan: `update t set v='z' where w = (select * from u3) and k = (select * from u)` gives `2 columns`, the second subquery, since the lookup by the key `k` is coded first. With one such subquery the message is decided; with two it depends on SQLite's plan.
- A correlated name resolves without an error in SQLite, and the names after it are looked up: `k = (select v from u where nope = 1)`, with `v` a column of `t` only, gives `no such column: nope`.

### What didn't work

A Bash command with a here-doc that wrote the SQL script and ran the shell was refused: "this command is too complex to verify that it stays inside the worktree". The scripts are written with the Write tool and run with the shell's input redirected.

### What I learned

SQLite's vector check of a comparison (`row value misused`) runs in `resolveExprStep` before the walk goes down into the operands, and the walk aborts after any node once `pParse->nErr` is set, which is the mechanism of the deferred error of a test for NULL that the engine already models.

### What was tricky

Where the subquery may be undecided, the pair's two instances and SQLite may each pick another row, and their states then part. The design: a test hook that observes the values that an instance computed, and on the pair's other instance uses the first instance's values where SQLite's candidates hold more than one value, and SQLite runs the statement with the subquery replaced by a parameter bound to that value.

### What warrants review

The decisions in `/docs/todo/slice-15.md` once written.

### Future work

The plan, the operators, the telemetry, the tests, the driver.

## Step 2: The plan, the operators, the event, and the checks against SQLite

**Author:** subquery-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Unit tests of planning and execution, and differential tests against SQLite in the style of the existing ones [...]: no row, one row, several rows with and without a deciding order, ties, same table and another table, every operator and both sides, null values, every pair of types, parameters inside and outside in text order, a subquery whose table or column does not exist, correlated, `*`, inside and outside a transaction, with `returning`, `update` and `delete` on the table the subquery reads, with and without an index that serves it. Through the driver (`conn_test.go`): goqite's receive without the arithmetic [...]"
**Interpretation:** Build the engine's half, make the pair of the differential tests take a subquery with SQLite's candidates, and pin each case of the brief against SQLite, in the engine and through the driver.
**Inferred intent:** Trust in the subquery equal to that of the slices before, before the generated checks reach it.

### What I did

`/internal/engine/plan.go`: `Query.Subqueries`, `Update.Subqueries`, `Delete.Subqueries`, the expression `*Subquery{Number, Offset}`, `queryOf`, `subqueriesOf`, and `Format` of both. `/internal/engine/subquery.go`, new: `subqueries` and `inner`, the phases of a subquery's build (`tables`, `names`, `widths`, `uncorrelated`, `lengths`, `types`), `build.compute` and `build.value`, and SQLite's two new errors. `/internal/engine/operator.go`: `build.walk` in place of the body of `build.conditions`, with the subquery's names in it; `orderColumns`; the calls of the phases in `build.node`, `build.ordered`, `build.bind`, `build.values`, `build.types`, and `build.change`. `/internal/engine/access.go`: `reading.e`, `reading.observe`, `reading.inner`. `/internal/engine/telemetry.go`: six attributes `statement.subqueries*`, and `statement.rows_read` with the subqueries' rows. `/internal/engine/engine.go`: no refusal in `check`; `Engine.observe`. `/conn.go`: `check` with `stmt.Check` alone.

Tests: `TestEngine_subquery` in `/internal/engine/subquery_test.go`, with 86 selects and 25 updates and deletes run through `pair.step` on the core and in memory, in a write transaction committed and rolled back, after a restart with an empty cache, in a read-only transaction, a grid of the 25 pairs of types, a cancelled context, and goqite's receive on goqite's table. The pair's `subqueryCall` (candidates from SQLite, `flatten`, `check`), `subqueryRule`, and the changes to `pair.step`, `pair.bothWays`, `typeRule`, `readOnlyRule`, and `returnedInOrder` in `/internal/engine/differential_test.go`; `Engine.ObserveSubqueries` in `/internal/engine/export_test.go`. A subtest of `TestEngine_report` in `/internal/engine/engine_internal_test.go` for the event; cases of `TestNewPlan`; `pin` in `/internal/engine/engine_test.go` decides a query by the pass. `TestSubquery` in `/conn_test.go`. `TestEngine_box` in `/internal/simulator/box_test.go` expects the update with a subquery `committed` now, the smallest fix.

### Why

The phases follow SQLite's order of errors as measured in step 1; the value is computed after every check and before the access of the statement, because the access of `id = (subquery)` is the lookup by the key of the value. The pair flattens the statement once the engine has computed it, because every later comparison of the pair (the order of the rows, RETURNING, every path) builds selects from the typed statement and would drop a subquery.

### What worked

- The calls agreed with SQLite on their first full run, errors, offsets, and rows included.
- Faults planted by hand, each reverted, and what found them: reading the last row in place of the first (`TestEngine_subquery`: "the engine gives the subquery 1 the value 1, and SQLite one of [3]"); no refusal of the types (an internal error of a comparison of text with an integer); no `row value misused` in a select; the subquery on the left looked up after the column (a panic); no `row value misused` for `!=` on the left of an update; LIMIT 0 read as no limit; the LIMIT of a subquery not refused; an access chosen as without a LIMIT (only by the event's test: `subqueries.access:primary_key subqueries.order:sorted` where `index` was wanted).
- `go test -race -count=50` of the new tests of the engine (135 s) and of the driver passed. `make lint` gave 0 issues.

### What didn't work

- `pin` took any text with "select" in it for a query, so `update ... where k = (select ...)` was run through Query: `Expected "", but got "columns ; rows "`. It now asks the pass.
- `returnedInOrder` panicked in `stmttest.boundValue` (`index out of range [-1]`) on an update with RETURNING whose subquery failed before it was computed, which the pair does not flatten; it skips such a statement now.
- `sqliteError` exists in `/conn_test.go` already; mine is `sqliteMessage`.
- My expectations, not the engine, were wrong three times: the real 1.5 is `real 1.5` in an answer, the offset of the second subquery of a plan was 62, and goqite's receive at 12:01:30 picks m_2, of the higher priority.
- The planted fault "the last correlated name, not the first" survived: no call had two correlated names. A call with three is in the list now.
- A Bash command with a here-doc of Go code, and one with a `for` loop around `git`, were refused as too complex for the worktree; the code went to the scratchpad with the Write tool and in with a small Python script.

### What I learned

The fake of the engine's internal tests counts the rows that a sorted scan gives, not those that it sorts, so the rows read of a sorted subquery are compared with those of the select of its text with LIMIT 1 on the same fake, and not with a number of the storage.

### What was tricky

The undecided subquery: the instance, the other instance, and SQLite may each take another of the rows that tie, and an update then leaves three states. The observe hook lets the other instance take the instance's values only where SQLite has several candidates, and SQLite runs the flattened statement with them.

### What warrants review

`build.walk` and the phases in `/internal/engine/subquery.go` against the order of the errors in `/docs/todo/slice-15.md`; `subqueryCall` and the flattening in `pair.step`; the static rule of types.

### Future work

The review, the README and the notes by a writer, `make test`.
