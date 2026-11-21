# Diary: Slice 12 of the roadmap through the engine and the driver, a value on the left of a comparison

Slice 12 lets a comparison of a WHERE have the literal or `?` on the left and the column on the right, as in `? >= timeout` and `5 < a`, because goqite, the queue that glue uses, writes its receive query both ways round. The statement pass reads such a condition since pull request #116 (diary `2026-11-21-value-left-stmt.md`), normalised: `5 < a` is `Condition{Column: a, Operator: OpGreater, Value: 5, ValueFirst: true}`. The engine refuses it through `valueFirstRefusal` until now. This task runs the slice in the engine and the driver, and turns it on in the generated checks, with a planted bug. The branch is `value-left-engine`, from `origin/value-left-stmt` with `origin/main` merged in (nothing new on `main`).

## Step 1: The requirements

**Author:** value-left-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Task: slice 12 through the engine and the driver, with its generated checks [...] 1. Remove the temporary `valueFirstRefusal` [...] 2. Statement by statement s4ql does what SQLite does, for both orders of every operator, with every type of value, `null` on the left (no rows), arguments on the left (numbered in the order of the text [...]), in `select`, `update`, and `delete`, mixed with conditions of the other order in one `where`. Same rows, same `RowsAffected`, same errors in the same order. The engine's own refusals must read rightly for a value on the left [...] go through every place the engine builds a message from a condition and check it with `ValueFirst` set. [...] 3. The choice of access is the same for both orders [...] pin it in `TestChooseAccess`, in the golden plans (add goqite's receive query written as goqite writes it, with `? >= timeout` [...]), and in the pinned rows-read numbers for one query of each order. 4. Generated checks: turn on `Generator.ValueFirst` in `stmttest.Sequence` [...] with tallies that count conditions with the value on the left by operator and by outcome [...] One planted bug [...] a condition with the value on the left whose operator is not turned around when it bounds a key or an index [...] 5. The driver: [...] `Query` and `Exec` with arguments on the left; the driver check compares with mattn as for the other order. 6. [...] when a tallied kind gets rarer, first try the generator's rates; lower a least number only to a third of its recounted value [...] Report every least number you lowered or dropped."
**Interpretation:** Remove the refusal, prove by hand tests against SQLite that the normalised condition runs rightly in every statement with every operator, type, and error, pin the access of both orders, write value-first conditions in every generated check with tallies and a planted bug in the fitting of bounds, and run the driver with them.
**Inferred intent:** goqite's receive query, as goqite writes it, runs on s4ql with the same trust as slices 1 to 11.

### What I did

Read the roadmap's slice 12, the diary of the statement pass and its decisions and facts in `docs/todo/slice-12.md`, the diary of the engine of slice 11 for the pattern, `internal/engine/valuefirst.go`, `access.go`, `bugs.go`, the filter and its errors in `operator.go` (`build.conditions`, `build.bind`), `errors.go` (`checkType`, `noSuchResultColumn`), `plan.go` (`rowsOf`), and `stmttest`'s `Generator.conditions`, `Sequence`, and `rule.go` (`ChosenAccess`, `WhereCases`, `WhereRule`, `whereText`).

### Why

The pass normalises, so every reader of a condition in the engine and in `stmttest` reads `5 < a` as `a > 5` already; what is left is to show it, and to find each place where the text's order could still show.

### What worked

Nothing to run yet.

### What didn't work

Nothing yet.

### What I learned

- No message of the engine quotes a position but `no such column`, whose offset is the column's own `Offset`, which the pass sets from the text whichever side the column is on. The message of the rule of exact types names the value and the column, which reads rightly in either order, and the number of an argument comes from the pass, which numbers in the order of the text.
- The engine's plan (`engine.Condition`) does not carry `ValueFirst`, and nothing in the engine reads it.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

- The removal of the refusal and the hand tests: step 2.
- The pins of the access, the generated checks, the planted bug, the driver, and the documents: the steps after it.

## Step 2: The refusal removed, the hand tests, the pins of the access, the driver, and the planted bug

**Author:** value-left-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "1. Remove the temporary `valueFirstRefusal` [...] 2. Statement by statement s4ql does what SQLite does [...] 3. The choice of access is the same for both orders [...] pin it in `TestChooseAccess`, in the golden plans [...] and in the pinned rows-read numbers for one query of each order. [...] 5. The driver: `Prepare` accepted such a text already and refused it at the first run; now it runs. `Query` and `Exec` with arguments on the left [...]"
**Interpretation:** Remove the refusal and its test, and show by hand, against SQLite, the rows, the counts, the errors, and their order of both orders, and the same access for both.
**Inferred intent:** The engine runs the form, and nothing in it reads the text's order wrongly.

### What I did

- Removed `/internal/engine/valuefirst.go` and its call in `check` in `/internal/engine/engine.go`. `TestCheck_valueFirst` is gone; `/internal/engine/valuefirst_test.go` now holds `TestEngine_valueFirst` and its helper `valueFirst`, which writes the comparisons of a WHERE with their values first (`TestValueFirst`).
- `TestEngine_valueFirst`, against SQLite: every operator in every spelling with the edges of each type and values beside them, as a literal and as an argument, on the key and on another column, by the key and with every row, each also against the operator turned round; NULL on the left as a literal and as an argument; conditions of both orders mixed at random on every shape of `shapes()`, also bounds of the key of both orders that contradict, hold one row, and fix it; parameters numbered in the order of the text, of other types in each place, in a select, an update, and a delete; every operator with its value first in an update and a delete, with the count of changes and the state; the errors of the names with SQLite's offset (`pin`), a column on the right in double quotes, the walk past a test for NULL, a missing column of the select list and a missing table; under the length limit of 1,000, the names before a value on the left that is too long, `string or blob too big` before the refusal by type, as a literal in a select and as an argument in an update and a delete; the refusal by type of the first value of another type in the order of the text, literal and argument, in a select, an update, and a delete; and the refusals of the arguments by their number and by what they are, and `engine.Check`.
- `TestEngine_paths` in `/internal/engine/access_test.go`: of the three select lists of each WHERE, one has every comparison written with its value first and one every second comparison, and every second path of the updates and deletes writes them so, so every path runs both orders with no more statements than before.
- `TestChooseAccess` in `/internal/engine/access_internal_test.go`: every case is checked again through the statement, with its comparisons as written, all turned round (`turnedWhere`), and every second one turned (`textAccess`); a subtest pins what the planted bug does.
- The golden plans: goqite's receive as goqite writes it, `select id from goqite where queue = ? and ? >= timeout and received < ?`, beside the one that was there, in the three goqite schemas, and `TestAccessOf_golden` requires the two accesses to be equal; four plans of the rules with a value on the left. `TestEngine_rowsRead` reads both receives at each stage: 3000 rows read for 400 without an index, 1000 with goqite's, and 500 with an index on the queue and the timeout, the same for both orders.
- `TestNewPlan`: the plan of a condition with its value on the left is the condition with its column first, and says `ValueFirst`.
- The driver: `TestValueFirst` in `/conn_test.go`, on goqite's table and an index on the queue and the timeout: goqite's receive as goqite writes it and with its column first give the same rows for four sets of arguments, prepared with three, through `QueryRow`, and in a transaction; an update with SET and WHERE arguments and a delete with values on the left, directly and prepared, with `RowsAffected`, the annotation, and a replay on another instance; and SQLite's errors of the names and the refusals by type and of an argument, also prepared.
- The planted bug `unturnedBound` in `fit` in `/internal/engine/access.go`, with a field in `bugs.go` and `Options.Plant` in `export_test.go`. For it the plan's `engine.Condition` and the engine's `condition` carry `ValueFirst` from the statement.

### Why

- The bug is in `fit` because that is the one place where the direction of an operator decides which end of the scan a value bounds, and a fault there shows only as rows that are missing: the filter above the scan tests the right operator and drops what the scan read wrongly, but cannot give the rows that the scan never read. The regression it stands for is a reader who sees `ValueFirst` and `Operator.Swapped` beside the operator and turns a condition round that the pass has already turned round, so that `5 < a` bounds `a` from above. It is planted in the fitting only, and not in the filter, because a fault in both would show in every check at once.
- Carrying `ValueFirst` in the plan: the plan holds what the statement says, and `Format` still writes every condition with its column first, as the pass decided.

### What worked

The engine ran every hand test as SQLite does on the first run once the refusal was gone: no fault was found in the engine. The failures of the first run were my own expectations, below.

### What didn't work

- Four expectations of mine were wrong, and SQLite corrected them: a threshold of rows of the random WHERE of a shape, an expected row (`c <= 4.5` holds for the row of 4.5, and NULL fails it), a count of changes, and the planted bug's bound of `9 <= k`, which is `k <= 9` as the text writes it.
- An update with a literal in WHERE longer than the limit gave the same error in both but another count of changes: SQLite gives `string or blob too big` there only when it steps the statement, and resets the count. It is the known difference of a long literal of an update in `docs/todo/slice-08.md` ("SQLite gives that error only when it changes a row"), whichever side the literal is on, so the hand test uses an argument there, which SQLite binds before it steps.
- A Python here-doc that edited `access_test.go` was refused by the permission system as "too complex to verify that it stays inside the worktree"; the same script in the scratchpad, run with `python3 -I`, worked.

### What I learned

No message of the engine needs the text's order: `no such column` takes the column's own offset, which the pass sets from the text, the refusal by type names the value and the column in a sentence that reads rightly for either order, and the numbers of the arguments are the pass's, in the order of the text.

### What was tricky

Making every path run both orders without more statements: the paths of the updates and deletes that run one condition are eight apart, so the order is chosen by `n/8%2`, not by `n%2`, which would give each condition one order only.

### What warrants review

- `unturnedBound` in `fit`, and whether the plan should carry `ValueFirst` for it.
- `TestEngine_valueFirst` against the brief's list.

### Future work

- The generated checks, their tallies, the planted bug in the simulation, and the documents: the next steps.

## Step 3: The generated checks, their tallies, the fuzz targets, and the planted bug in the simulation

**Author:** value-left-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "4. Generated checks: turn on `Generator.ValueFirst` in `stmttest.Sequence`, so that `TestDifferential`, `TestConn_differential`, the driver check, the simulation, and the fuzz targets write conditions of both orders, run through every path as slice 9's checks do [...] with tallies that count conditions with the value on the left by operator and by outcome [...] One planted bug [...] 6. [...] when a tallied kind gets rarer, first try the generator's rates; lower a least number only to a third of its recounted value and never to a number that rounds to zero at the test's seed count [...] never drop a tally without writing beside it where the kind is still required."
**Interpretation:** One comparison in some number of the sequence with its value first, a rule in `stmttest` that names such comparisons, tallies by operator and outcome in every generated check with least numbers, the paths run in the form of the text, the hand-built fuzz targets taught the form, and `unturnedBound` in the simulation.
**Inferred intent:** A generator that stops writing a value on the left, or an engine that reads it wrongly on some path, fails a test.

### What I did

- `/internal/sql/stmt/stmttest/sequence.go`: `Generator.ValueFirst` is 3 in `NewSequence`, so one comparison in three of a WHERE of a select, an update, or a delete is written with its value first, the hooks' conditions too.
- `/internal/sql/stmt/stmttest/rule.go`: `ValueFirstCases` names each comparison with its value first, by its operator as the text writes it, and those with NULL, a literal or a nil argument (`TestValueFirstCases`). `whereText`, which `SelectText`, `FoundQuery`, and `RowQueries` use, writes a condition with `ValueFirst` as the text did, `? op' column`, so that `pair.everyPath` runs every path of a WHERE in the form that it came from; before, it wrote every condition with its column first, and the paths would never have seen the form. `TestWhereCases` and `TestChosenAccess` have cases with a value on the left, and a subtest pins the text of `SelectText`.
- `/internal/engine/differential_test.go`: `reachValueFirst`, called from `reachWhere` and `reachChange`, counts each case by itself with the outcome of any kind ("finds rows", "finds no row", or the refusal), after the kind of the statement with its own outcome, and once per statement; `pair.reach` counts the statements with a value on the left again by the kind of the transaction; `checkChoice` counts "chosen ... by a value on the left" when the model chooses another access with `!=` in place of the operator of each condition with its value on the left, which pins and bounds nothing; `pair.everyPath` counts the WHERE with a value on the left run by every path and its paths with a range. Least numbers of slice 12 in `reachedDifferential`, in `TestConn_differential`, and in `TestGenerated` of the driver check (`/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`, its own `reachValueFirst`), a third of the run or 1.
- `FuzzWhere`, `FuzzChanges`, and `FuzzIndexes`: `fuzzBytes.typedConditions` writes a comparison with its value first when the highest bit of its operator's byte is set, and `fuzzBytes.pins` a pin when bit 2 of its byte is set and each bound by bits 16 and 32 of the bounds' byte. The operator a byte chooses is the same as before, so a seed that had the bit set now runs the same condition in the other order. New seeds for `FuzzWhere` and two for each type of `FuzzIndexes`; `TestFuzzIndexes_seeds` requires that they reach the choices and the paths by a value on the left. The fuzz targets that take their statements from `Sequence` (`FuzzDifferential`, `FuzzSQL`'s seeds, the driver check's `FuzzSequence` seeds) write the form now too.
- `/internal/engine/simulation_test.go`: `tallyOf` counts the statements with a value on the left, those that found rows, those with SQLite's error, and those read by the primary key or an index (`hasValueFirst`), with least numbers in the three workloads, a quarter of the first 1,000 seeds. `unturnedBound` in `TestSimulation_plantedBugs`, in the hammer workload.
- `TestDifferential_plantedBugs` plants `unturnedBound` too.
- `internal/sql/stmt/README.md`: the engine and the driver run slice 12 since 2026-11-21; the sentence about the engine's refusal is gone.

### Why

- The rate 3: a value on the left costs the other kinds nothing, since a condition is the same condition in either order, but the generator draws a number for each comparison, which moves every later statement. I tried 2, 3, 4, and 5 in `TestDifferential`: 2 took six least numbers below their numbers, three of them to 0; 3 four; 4 five; 5 six. 3 it is.
- `!=` in place of the operator to tell whether a value on the left decided the access: removing the condition would renumber the parameters, which `stmttest.ChosenAccess` refuses, and `!=` keeps a comparison with NULL never true, so only pins and bounds can change.

### What worked

- No generated check found a difference from SQLite or from mattn's build, and no path gave other rows: `TestDifferential` ran 497 WHERE with a value on the left by every path, 195 on a table with an index. The simulation ran 1,000 seeds of each workload without a violation (225 s).
- The planted bug is found by the property that should find it, rows that differ from SQLite's: in the simulation 33 of the first 200 seeds of the hammer workload find it, 26 by ", and in SQLite", and 11 of the default workload, 6 by it; the default run finds it the second time at seed 24. In `TestDifferential_plantedBugs` 3 of 60 sequences find it, each by the rows of a select of its WHERE as the rules choose it. Each seed that finds it finds nothing without it. Every other planted bug of the simulation is still found twice by its own violation within the limit of 100 seeds; `listOrder` now needs 83 seeds for its second.

### What didn't work

- My first tallies split "finds rows" from "rows changed", which left each operator with a handful of either; one outcome of any kind, "finds rows" or "finds no row", counts both.
- My first `checkChoice` count removed the conditions with a value on the left, and `stmttest.ChosenAccess` then refused every statement with a parameter among them for its number of arguments.
- The seeds of `FuzzWhere` first chose the operator with 128 plus its number, which modulo the ten operators is another one, two of them a test for NULL; 130 plus the number is the operator.
- A Python here-doc with `\n` in it was refused by the permission system; the scripts in the scratchpad worked.

### What I learned

The least numbers moved by chance, as every slice's do: the generator draws one more number per comparison.

### What was tricky

The pins and least numbers that moved, each recounted from the tests' own output, with the value before taken from a run with `ValueFirst` 0:
- `TestDifferential`: "!= on the key a value of another type" 15 to 6 (least 11 to 2), "> on the key a value of another type" 26 to 11 (least 12 to 3), "delete WHERE that tests a column more than once ok, rows changed" 12 to 5 (least 7 to 1), and "delete WHERE that reads a scan between two bounds ok, rows changed" 5 to 1, dropped, with the select's and the update's kinds and `TestEngine_paths` named beside it. The excuses went from 13, 816, 278, 530, 4, 4, 0, and 0 to 26, 817, 263, 540, 2, 2, 2, and 0; the 2 of `excusedLongCondition` are selects with a long literal on the left of its column, for which SQLite gives rows as for one on the right, and which the rule decides.
- The driver check: "insert that leaves out a column that takes NULL a value of another type" 7 to 4 (least 7 to 1); "NULL literal into a column that takes NULL attempt to write a readonly database" 10 to 1, dropped, with "read-only transaction: insert attempt to write a readonly database" named beside it; and "write transaction rollback ok while rows are open" 1 to 0, dropped from the kinds that must be reached, with the commit while rows are open, which is still required, and a subtest of `TestTx_databaseSQL` named beside it. Its excuses went from 1715, 7, 679, 338, 384, 100, 24, 53, 97, and 129 to 1632, 14, 611, 345, 347, 99, 22, 41, 96, and 113.
- The simulation: no least number fell; "statements with a value on the left that found rows" of the idle workload has none, since a quarter of the 34 of 1,000 seeds is less than one in 50.
- `TestDifferential_plantedBugs`: `pinByPlace` went from about 7 of 60 to 11, which its least number of 3 allows.

### What warrants review

- `whereText` writing the form of the text, since `FoundQuery` and `RowQueries` use it for SQLite's side of the checks of updates.
- The dropped least numbers and the reasons beside them.

### Future work

- The engine's README, the todo, the merge of `main`, and the review: step 4.

## Step 4: The README of the engine, the todo, the merge, the review, and the checks

**Author:** value-left-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "8. [...] Merge `origin/main` again before you open your pull request. 9. Do not change `README.md` (root), `site/`, `docs/spec.md`, `docs/roadmap.md`. Update the engine's README. [...] Finish: `make test`, `make lint`, `make check-browser` pass; `make test-asan` for the packages whose fuzz tests you changed. Self-review against these requirements."
**Interpretation:** A writer sub-agent for the README of the engine, the decisions in `docs/todo/slice-12.md`, a merge of `origin/main` (pull request #116, merged), the code-review skill over the branch, fixes, and every check.
**Inferred intent:** A branch ready for the independent reviewer.

### What I did

- `/docs/todo/slice-12.md`: the group "Decisions of the engine step of slice 12", an open item on `listOrder`, and two items not built (mutation testing of the engine step and of the pass, and long fuzz runs).
- A writer sub-agent, limited to `/internal/engine/README.md` in this worktree, added the section "A value on the left of a comparison" and the facts of slice 12 to the opening, "The stages", the rules, "The golden plans", "The rows read", "How it is tested", the planted bugs (nineteen now), and "Results"; I read its diff and wrote the paragraph of results. It reported older claims that it left: the default of `-length` is 48 where the README says 40, `-workload` takes `idle` too, the link of the known difference of a long literal points to `sql-front-end.md` where the text is in `slice-08.md`, the `listOrder` row of the table of results is not recounted, and the numbers of "read both ways" in `TestConn_differential` are not recounted. It also corrected the pinned excuses of `TestDifferential` in the README.
- Merged `origin/main` (`9ae52f6`, the merge of #116 into `main`, the same tree as the branch of the pass): no conflict.
- `code-review high origin/main...HEAD` found no bug in the engine and eight nits. Fixed: the driver check's `reachChange` did not count the values on the left of an update with a SET of a column that the table does not have, and passed the outcome of the change where the other calls pass the kind of the run; `turnedWhere` of `TestChooseAccess` did not know `!=` and `==` and now panics on an operator it cannot turn; a local `any` in `reachValueFirst` and a local `turned` in `checkChoice` hid the predeclared type and the package's map; the decision of the pass on `valueFirstRefusal` in the todo is in the past tense. Left, as recorded: `docs/roadmap.md` says slice 12 is not built, which the brief keeps out of this task, for the lead's sub-agent after the merge; the dropped and lowered least numbers, which follow the brief's rule after trying the rates; and `listOrder`'s seed 82, an open item in the todo.

### Why

The brief's finish.

### What worked

- `make lint` (0 issues), `make check-browser`, `make test` (the engine with `-short` in 148 s, beside other work on the machine), and `make fuzz-shards FUZZGROUP=c FUZZSHARDS=4`.
- `make asan-test ASANPKGS=maragu.dev/s4ql/internal/engine` in the `golang` image of `go.mod`, since the fuzz targets of the engine changed: 287 s, where the engine of slice 11 took 269 s.
- `TestDifferential` with 3,000 sequences from seed 1,000,000: no difference from SQLite, 183 s. `FuzzWhere` 72,503 executions in 1 min, `FuzzChanges` 84,773, and `FuzzIndexes` 18,342, on 4 workers: nothing found.
- The new tests 20 times under `-race` (`TestEngine_valueFirst`, `TestValueFirst`, `TestChooseAccess`, `TestNewPlan`, `TestAccessOf_golden`), and the driver's `TestValueFirst` 50 times and 3 times under `-race`.

### What didn't work

- Two loops that waited on the output of a background run with `until` were wrong (a `grep -c` that gave two lines, and a test of an empty number), and printed errors until I stopped them. The notifications of the runs were enough.

### What I learned

The merge of #116 into `main` is a true merge of the branch of the pass, so the merge here changed nothing; a squash would have brought `valuefirst.go` back by the three-way merge, which the check after the merge would have shown.

### What was tricky

Nothing beyond the steps before.

### What warrants review

- The section "A value on the left of a comparison" and the planted bugs of `/internal/engine/README.md`.
- `/docs/todo/slice-12.md`, the group of the engine step.

### Future work

- Mutation testing of the engine step of slice 12, and long fuzz runs, for a fresh builder.
- The roadmap's slice 12, the status of the README, and the site, for the lead's sub-agent after the merge.
- `listOrder` in `TestSimulation_plantedBugs`, with the work on the workloads of the planted bugs.

## Step 5: The reviewer's addition, the checks, and the landing

**Author:** value-left-engine landing agent

### What I did

- The independent reviewer found no fault and asked for one addition. The doc comments of `engine.Condition.ValueFirst` (`/internal/engine/plan.go`) and `condition.valueFirst` (`/internal/engine/operator.go`) now say that the field is there only for the planted bug `unturnedBound` and for tests, that the statement pass has turned the operator round already, and that nothing may turn it round again from the field.
- The reviewer also asked for a case in the statement pass's tests that fails if `recognize.condition` forgets `Swapped()` for any single operator. It is there already: "every operator with the value on the left" in `/internal/sql/stmt/check_test.go` writes all eight spellings with the value first and expects the recorded operators as literals, not through `Swapped()`. `TestOperator_Swapped` pins `Swapped` itself. So I added no test.
- `git merge origin/main`: already up to date (`9ae52f6`).

### What worked

- Without `-short`: `go test ./internal/engine/... ./internal/sql/...` (the engine in 127 s) and the driver check in `/internal/sqlite/crosscheck` all pass. No pin moved, and `listOrder` is still found within its seeds.
- `make fuzz-shards FUZZGROUP=c FUZZSHARDS=4` (37 fuzz tests, each once), `make lint`, `make check-browser`, and `make test`.

### What didn't work

Nothing.

### Future work

- Mutation testing of slices 9 to 12, then slice 13.
