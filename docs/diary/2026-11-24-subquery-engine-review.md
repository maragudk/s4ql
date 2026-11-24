# Diary: closing the review of the engine step of slice 15

An independent review of the branch `slice-15-engine` (the scalar subquery in the WHERE of a select, an update, and a delete, in `/internal/engine` and the driver) found no wrong result, but one crash of the differential harness and planted faults that every test passed. This task closes them with tests that fail on each planted fault, on the branch `slice-15-engine-review`.

## Step 1: Close the eight findings and their neighbours

**Author:** builder (sub-agent of the lead)

### Prompt Context

**Verbatim prompt:** (the findings of the brief, verbatim; the rest of the brief gave the rules of `AGENTS.md`, where to record, and the report)

> 1. **The differential harness panics on a select with a subquery whose outer `limit` is 0.** `select k from t where k = (select k from u1) limit 0`, and the same with `limit ?` bound to 0, give `index out of range [-1]` in `stmttest.boundValue`, reached from `orderedOf` (`internal/engine/orderby_test.go`, about line 45), called by `pair.step` (`internal/engine/differential_test.go`, about line 1200). The engine rightly computes no subquery there, so `p.computed` is nil and the statement is not flattened; `orderedOf` then writes the subquery term as a `?` with `Parameter == 0`. Fix the harness so the pair checks this path (the engine's answer, no rows, is right), and add the statements as cases. The generated workload will draw this shape in the next step.
> 2. **A subquery that takes the first row whose value is not null** passes every test (plant: in `build.value` in `internal/engine/subquery.go`, `if row[in.position].HasNull() { continue }` before the assignment). Add cases where the `order by` puts a row with null first while later rows hold values, such as `(select x from u order by x limit 1)` and one with `desc` on a column where null comes last, for `select`, and at least one `update` or `delete`.
> 3. **A cancelled context inside the subquery is not tested** (plant: `src.rows(ctx)` becomes `src.rows(context.Background())` in `build.value`). The existing test cancels before `Exec`, so the error comes before the subquery reads. Write a test in which the context ends while the subquery reads, without depending on timing: look at how the existing tests of a select end a context in the middle of a scan (a hook, a fake store, or a context that ends after a counted number of checks) and do the same.
> 4. **`statement.subqueries.found` is not tested for a subquery that reads rows and finds none** (plant: `if sub.found` becomes `if sub.rows > 0` in `report.subqueryAttrs`). Add the case, in the shape of an empty goqite receive: messages are queued, none is due, so the subquery reads index rows and its filter rejects them all. Check `found`, `rows_read`, and the other subquery attributes there.
> 5. **The `!=`/`<>` exception for a wide `*` in an update or delete is tested on one side only** (plant: `c.Operator == stmt.OpNotEqual && c.ValueFirst` loses `&& c.ValueFirst` in `subqueries.widths`). Add `update t set w = 1 where k != (select * from u)` and the `<>` and `delete` forms; SQLite gives `sub-select returns 3 columns - expected 1` there.
> 6. **A subquery without `where` or `order by` that always reads forward** passes (plant: `in.s.access.backward = false` in `build.value`). The tally of "another row than SQLite's first" is met only by subqueries with a filter. Require the tally per kind of subquery, so that one without `where` and `order by` must also give both.
> 7. **The length check of a subquery's `limit` argument is untested** (plant: drop it in `subqueries.lengths`). Add a direct test under a lowered length limit, in the way the existing tests of the length limit do it.
> 8. **The offset of `row value misused` in a select** can be swapped between the sides without a test failing. Offsets are not promised; add the check only if it is one line where the message is already checked, otherwise leave it and say so.
>
> Also look once for neighbours of these gaps (the same kind of gap one case over) and close what you find cheaply. Do not widen the feature. If closing a gap shows a real fault in the engine, fix it, and say so plainly in the report with the statement, s4ql's output, and SQLite's.

**Interpretation:** Change only tests (and the harness), one finding at a time. For each, plant the fault named, show that the new test fails on it, and revert the plant.

**Inferred intent:** Trust in the engine step rests on tests that catch faults. The lead wants the gaps shut before the generated workload starts drawing subqueries.

### What I did

All of the changes are in `/internal/engine/subquery_test.go`, `/internal/engine/differential_test.go`, and `/internal/engine/engine_internal_test.go`. No code of the engine changed.

1. In `pair.step`, a select that still has its subqueries after the engine ran without an error is one whose subqueries the engine did not compute. The pair now requires it to give no row, compares its rows with SQLite's rows of the unflattened text, and skips `orderedOf`, `checkOrder`, and `checkAccess` for it. It counts "subquery that the engine did not compute, of a select that gave no row", which `TestEngine_subquery` requires. The cases: LIMIT 0 as a literal, as `limit ?` bound to 0, with ORDER BY, and `k is null` and `w = ?` bound to NULL. The neighbours: an update and a delete with a condition that is never true.
2. Cases where NULL comes first in the column of the subquery: `order by x limit 1`, `order by x` with no LIMIT, `order by y desc` (the row with NULL x has the highest y), `order by r` on the table of the statement, and `order by w desc, k`. There are two updates and two deletes of the same shape at the start of `subqueryWrites`, where they change nothing.
3. A new subtest of `TestEngine_subquery` uses `newFlipContext` of `TestEngine_cancelChange`. It runs a select, an update, and a delete whose subquery reads table `s` (80 rows) by `v = 'v40'`, so it reads 40 rows or more in either direction. The context ends at each call of Err in turn. Each run must fail with `context.Canceled` and leave the state as it was. The statement must be canceled more than 40 times. The final state must equal SQLite's.
4. Three cases in the subquery-event subtest of `TestEngine_report`. Two read two rows of index `i` (`w = 20`) and reject both with `k != 2 and k != 4`, one of them with `order by w limit 1` in an update. The third is a delete whose full scan rejects all four rows.
5. `k != (select * from u)` and `k <> (...)` in an update and a delete, `(select * from u) = k` and `< k`, and `!=`/`<>` in a select.
6. `subqueryCall.check` also counts the two kinds of ties per shape (`subqueryShapes`: without WHERE and ORDER BY, with WHERE and without ORDER BY, with ORDER BY), and the round loop requires all six. Two new sorted cases with ties of other values (`(select k from t order by w limit 1)` and `(select v from t where w = 1 order by w desc)`) were needed for the ORDER BY shape.
7. A new subtest under a length limit of 1,000 bytes, after the one in `/internal/engine/orderby_test.go`. A text or blob argument of the subquery's LIMIT, or of a condition of the subquery, that is longer than the limit gives `18/18: string or blob too big` in s4ql and in SQLite. This holds for a select, an update, and a delete, and comes before the types and before the LIMIT of the statement.
8. Left open, as the brief allows: the record is in `/docs/todo/slice-15.md`.

Each plant was applied with `sed -i ''` on `/internal/engine/subquery.go` or `/internal/engine/telemetry.go`, the test was run, and the file was restored with `git checkout`. The new subtests ran with `-count=50` (57.6s, ok) and with `-race -count=5` (ok).

### Why

A plant that every test passes means a real fault of that shape would ship. Each new test is aimed at one plant and is checked against it.

### What worked

- The pair's own machinery: once a case is in `subqueryReads` or `subqueryWrites`, it is checked against SQLite on the core and in memory, in a transaction, and read-only, all for free.
- `newFlipContext` with a large enough gap between the counts with and without the plant. The subquery's rows give 41 or more checks, and the plant gives 6 to 13.

### What didn't work

- The first form of the context test compared the number of cancels of a near row (`v0`) with that of a far row (`v39`). It gave `9 times by its first row, and 9 by its last`, and later 11/9 and 9/11. Each run of the statement draws its direction anew from the seed of the instance, so the near row is sometimes read last. I made it independent of direction by putting the row in the middle of 80.
- A subquery that sorts (`(select w from s order by v desc limit 1)`) gave 9 cancels without the plant and 6 with it, and the delete gave 18 against 13. The storage sorts and checks the context through SQLite's progress handler, not at each row, so the count does not tell the plant apart with a margin. I dropped those cases.
- With the per-shape tally, the shape "with ORDER BY" never reached "another": the only subquery with ties under ORDER BY used index `uy`, which delivers the order, and ties then come in index order. This failed with `a subquery whose rows tie gave one of them only in 20 rounds`. Sorted cases fixed it.
- One `sed` and test command was refused by the sandbox because it was too complex ("this command is too complex to verify that it stays inside the worktree"). Writing the scratch file with the Write tool worked.

### What I learned

- `Conn.Exec` of a select runs the query, so its subqueries are computed under Exec too.
- In `subqueryReads`, a subquery is drawn forward or backward once per run, and the first run of a test can be either. Any count-based test of a scan has to work in both directions.

### What was tricky

- Keeping the uncomputed-select check strong. Flattening with NULLs would have made the comparisons work, but it would also have hidden an engine that wrongly skips computing. SQLite runs the real text, so its rows decide whether skipping was right. A plant that skips computing for any statement whose only condition has a subquery fails at once.

### What warrants review

- The `uncomputed` branch in `pair.step` in `/internal/engine/differential_test.go`, and that it cannot hide a select that gave rows. It returns a failure if the engine gave rows, and `differs` compares with SQLite otherwise.
- The threshold `canceled > 40` in the context subtest of `/internal/engine/subquery_test.go`.

### Future work

- Running the pair's subquery cases under a lowered length limit needs `pair.checkLength` to read the literals of a subquery. This is in `/docs/todo/generated-workload.md`.
- Pinning the offset of `row value misused` in a select would take a `pin` test of its own.
