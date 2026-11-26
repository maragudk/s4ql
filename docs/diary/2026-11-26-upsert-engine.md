# Diary: the engine and the driver step of upserts

The statement pass reads `insert ... on conflict (key) do nothing` and `insert ... on conflict (key) do update set ... [where ...]` into `stmt.Insert.Conflict` (#178). This step makes the engine run them, and removes the refusal `ON CONFLICT clause` and every excuse for it in the differential pairs of the engine and of the driver. The contract is `/docs/todo/upserts.md`.

## Step 1: the requirements, and the design of DO UPDATE

**Author:** builder (upsert-engine)

### Prompt Context

**Verbatim prompt:** The task: the engine and the driver step of upserts, and nothing else. The statement pass reads `insert ... on conflict (key) do nothing` and `insert ... on conflict (key) do update set ... [where ...]` into `stmt.Insert.Conflict` (#178 on `main`). The engine refuses them for now (`upsertRefusal` in `internal/engine/upsert.go`, called from `textRefusal`; `engine.NewPlan` panics for an insert with a clause; `stmttest.UpsertRefusal`; `excusedUpsert` in the engine's differential pair and in `internal/sqlite/crosscheck/drivercheck`). Your step makes the engine run them and removes the refusal and every excuse for it. (The brief goes on with seven points: the rows and counts of "Left to the engine step"; the target must be the key, else SQLite's errors; the errors at prepare in SQLite's order, pinned against SQLite, or a named refusal; exact types for `excluded.c` as for a column of its type, NOT NULL, and arithmetic; `pragma query_only`; the storage core's `Tx.Upsert` as `update` uses it, without a change to `internal/core`; and the driver through Exec, Query with RETURNING, Prepare, a transaction, and several statements in one Exec. Text stays refused as on `main`; the generators, the generated checks, mutation testing, and several rows per insert are out of scope.)

**Interpretation:** Build the sink of an upsert in the engine, with SQLite's errors of its names in SQLite's order and at SQLite's offsets, s4ql's rules of exact types for both rows of DO UPDATE, and SQLite's rows and counts; let the pairs judge upserts by s4ql's rules instead of excusing them; keep the text refusal for an upsert with text.

**Inferred intent:** Upserts are what glue and goqite need next for their writes; the engine step must be as trustworthy as the update it builds on, checked against SQLite case by case, so that the later builder of the generated checks only has to generate them.

### What I did

I read `/docs/todo/upserts.md`, the plan, the operators of insert and update, the filter, the evaluator in `/internal/expr/expr.go`, and the rules of `stmttest` that the differential pair uses. I measured SQLite 3.54.1 with the shell for the order of the errors of names, for `pragma query_only`, for the IS NULL rule in the WHERE of DO UPDATE, and for the length limit of the proposed row.

The design: DO UPDATE reads two rows, the row of the table and the row that the insert proposes. The engine reads them as one row of a *combined table*: the columns of the table, then one column for each of them under a name that starts with a NUL byte, which no statement can write. Once every name is checked with SQLite's messages, each `excluded.c` is rewritten into a column of that table, and DO UPDATE is bound, checked, and computed by the existing `update` (its `bind`, `checkTypes`, `newRow`) and the existing filter (`walk`, `values`, `types`, `constants`, `holds`). So `excluded.c` alone is copied as a column alone, compared as a column alone, and is a column of its type in arithmetic, by the rule of exact types, without a change to the evaluator.

The plan gets a `Conflict` on `Insert`; the sink `upsert` embeds the `insert`, whose check is the check of the proposed row; its run inserts the row with `insertNew`, which measures the record first as SQLite does, and on a conflict does nothing or reads the row, tests WHERE on the combined row, computes SET, checks NOT NULL, and writes with the storage's upsert.

### Why

Reusing update and filter keeps one implementation of the rules of types, copies, and arithmetic, which already have their differential tests; a second evaluator path for excluded would have to be checked again from nothing.

### What worked

The first sequence of twenty upserts, compared step by step with SQLite on the core and in memory, matched SQLite on the first run but for one case of mine that broke the rule of exact types (`0 < excluded.w` of a REAL column).

### What didn't work

`go test -run TestEngine_upsert ./internal/engine/` first failed with `Expected "columns "v INT, w REAL", rows ["null,real 0.25"]", but got "not supported"`: the case compared an integer literal with a REAL column, which s4ql refuses. The case was wrong, not the engine; it says `0.0` now.

### What I learned

SQLite resolves every column of the target before it matches the target to an index: `on conflict (v, z)` gives `no such column: z`, not the error of the target. A test for NULL of `excluded.c` follows the same rule as one of a column: its `no such column` waits for the next comparison or the end. SQLite builds the record of the proposed row before it looks for a conflict, so `string or blob too big` comes also with DO NOTHING.

### What was tricky

Messages: a refusal that names a column of the combined table must say `excluded.c`, so `columnName` knows the prefix, and `checkType` now names its column through `columnName`.

### What warrants review

`/internal/engine/upsert.go` as a whole, and the order of the checks in `build.doUpdate` against the order of an update in `build.change`.

### Future work

The pairs, the errors and offsets pinned against SQLite, the driver, and the documents follow in the next steps.
