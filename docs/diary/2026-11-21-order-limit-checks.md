# Diary: the generated checks of slice 13, ORDER BY and LIMIT

The statement pass (#119), the engine and the driver (#122), and the interrupt of a sort (#124) of slice 13 were merged, but no generated test wrote an ordered or limited select: `Generator.OrderBy` was off everywhere. This step turns ORDER BY and LIMIT into an area of the generator with a profile of its own, models the agreed choice of an access with an ORDER BY, compares every ordered and limited select with SQLite and through every path, checks the spec's rule that unspecified order varies, excuses SQLite's refusal of a sort under a lowered length limit, plants three bugs, and runs the profile in the simulation and the driver check.

## Step 1: the area, the model, the checks, the bugs

**Author:** builder (order-limit-checks)

### Prompt Context

**Verbatim prompt:** "You are a builder on s4ql (repository maragudk/s4ql, Go): a SQL database whose truth is a log in S3, with a local SQLite cache and its own SQL engine. [...] Slice 13 of `docs/roadmap.md` is `order by` (one or more columns, asc/desc) and `limit` (a non-negative integer or `?`) on `select`. [...] What is missing is the slice's generated checks [...]" followed by the ten points of "What to build" (the area, the model of the rules, answers, the spec's rule on order, the difference under a lowered length limit, kinds and least numbers, the simulation, planted bugs, fuzzing, faults found) and the limits (10% time, no mutation testing, no timing in tests, docs in `docs/todo/slice-13.md` and `docs/todo/generated-workload.md`).
**Interpretation:** follow the recipe "Add a feature area" of `/internal/sql/stmt/stmttest/README.md` for slice 13, and add the checks that the recipe does not cover: the model, the order-varies kinds, the excuse, the planted bugs.
**Inferred intent:** trust in ORDER BY and LIMIT comes from the same generated machinery that every earlier slice has, with floors that make a regression of the generator visible.

### What I did

- `/internal/sql/stmt/stmttest/profile.go`: the area `order`, the profile `Order` at slot 10, the fields `Order`, `Limit`, `Keys`, and `Shared`, and `Mixed` with Order 1 and Limit 4. `/internal/sql/stmt/stmttest/sequence.go`: the hooks `Sequence.order` and `Sequence.orderColumns` on the stream `rOrder`, the conditions of the select kept in `Sequence.selected`, a LIMIT argument that s4ql refuses drawn in `Sequence.next`, `keyOfStep` for `Keys`, and the shared first column of a create index for `Shared`. Only the digests of `mixed` and `order` moved.
- `/internal/sql/stmt/stmttest/rule.go`: `ChosenAccess` with the agreed ranking of slice 13, written from the rules in the brief; `Access` has `Delivered`, `Backward`, `Sorted`; `MissingColumn` names a column of ORDER BY after those of the select list and WHERE. Table tests with the README's examples and goqite's receive in `/internal/sql/stmt/stmttest/rule_test.go`.
- `/internal/engine/differential_test.go`: `reachOrder` (the kinds, and the choice against the model for every ordered select, also without WHERE), `pair.checkOrder` (the rows of each query in the order of its access, and the kinds of the spec's rule), `pair.sortTooBig` and `excusedSortTooBig`, `oracle.lengthLimit` and `oracle.unlimited`, `newInstanceDirected`, and `TestPair_sortTooBig`. `/internal/engine/tx_test.go`: `pair.wrote` for the kind of an ordered select after the writes of its transaction.
- `/internal/engine/simulation_test.go`: `differs` compares ordered and limited selects with `ordered.compare`, a refused LIMIT argument goes to `typeRule`, two cells of `order`, five kinds.
- `/internal/engine/bugs.go`, `/internal/engine/access.go`, `/internal/engine/operator.go`: the planted bugs `turnMixed`, `limitFirst`, `ascendLast`.
- `.kinds` files with a third of each total, recounts of the four tests, five golden plans in `/internal/engine/testdata/plans/rules.txt`, fuzz seeds of the profile.

### Why

The README's recipe only covers the generator. The model, the order check, and the bugs are what make "the rows are in SQLite's order" a claim with teeth: a sort is always right in its rows, so a wrong choice of access shows only against the model, and a wrong order only against SQLite in order.

### What worked

- The machinery of the engine builder (`ordered.compare`, `everyPath` with `Path.Sort`, `limitRule`) took the generated selects without a change. No generated test found a fault in s4ql.
- Planting the bugs found each by its own failure, "where SQLite has the rows", and no seed fails without the bug: 4, 14, and 21 of 30 sequences in `TestDifferential_plantedBugs`, 16, 32, and 61 of 200 seeds in `TestSimulation_plantedBugs`.
- The `.tsv` diffs against `main` touch only the rows of `mixed`, `order`, the environments that hold them, and the new kinds.

### What didn't work

- The first run failed in the test itself: `select *, dense_rank() over (order by ...) ... : SQLite gives 18/18: string or blob too big`. The ranked query of `ordered.compare` is a sort of SQLite's own under the lowered limit. `oracle.unlimited` runs such helper queries without the limit.
- The simulation took a refused LIMIT argument for a violation: `s4ql: not supported: argument 1, which is a real, for LIMIT, which takes an integer from 0`. It now takes it by `typeRule`, as a refusal of a type.
- The driver check failed on `order by "restrict"`, a name in double quotes that is no column: mattn's SQLite reads it as a string, where the pinned build gives `no such column`. `MissingColumn` did not look at ORDER BY, so `checkQuoted` did not excuse it.
- Recounts were not repeatable: the rows of the new direction kinds moved between two recounts with no change of code, because each instance of the tests drew the seed of its directions at random. `newInstanceDirected` derives it from the sequence seed, and two recounts in a row now give no diff.
- The goqite shape stayed at 0 for a long time. Most selects gave 0 or 1 row (keys repeat, so inserts fail with UNIQUE), and two kept indexes with the same first column were rare. `Keys` and `Shared` raised both.

### What I learned

- Under a LIMIT that cuts, the rows of SQLite and of s4ql may differ as multisets, so every place that compares answers by `answer.String` needs the ordered comparison: the simulation had two such places.
- A pinned direction draw is what makes the counts of point 4 a function of the seed; the `-directions` flag still overrides it for a replay.

### What was tricky

- Point 4 needs the direction to show in the rows. The check compares the rows of the other direction (by SQLite) and counts only where they differ, so a select of one row, or of rows equal in every selected column, counts nothing.
- The excuse of point 5 did not occur in the final default run (0 times; once in an earlier draft of the generator). `TestPair_sortTooBig` pins it on the engine builder's measured case, and shows it excuses nothing else.

### What warrants review

- `ChosenAccess` in `/internal/sql/stmt/stmttest/rule.go`, against the agreed ranking.
- `pair.checkOrder` and `pair.sortTooBig` in `/internal/engine/differential_test.go`.
- `newInstanceDirected`: the generated tests no longer explore new directions from run to run; they explore those of their seeds.

### Future work

- The goqite shape with a pin is reached once in the default run and is not required; see `/docs/todo/generated-workload.md`.
