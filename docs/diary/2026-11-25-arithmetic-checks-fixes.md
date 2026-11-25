# Diary: fixes after the review of the workload of arithmetic

An independent review of the generated workload of arithmetic (branch `arithmetic-checks`) found six gaps in `/internal/sql/stmt/stmttest/`: a tested expression was never compared with a plain value or a subquery, three refusals of classes were almost never generated, `expressionClass` gave bogus allowed refusals, `ArithmeticRefusal` disagreed with its comment, the masking of `turn` in `Generator.conditions` had no direct test, and arithmetic inside a subquery came only from `mixed`. This task closes each, proves the new forms with five planted faults, and recounts the reach files. The work is on the branch `arithmetic-checks-workload-fix`.

## Step 1: the rules, the generator, the reach kinds, the planted faults, and the recount

**Author:** builder (arithmetic-checks-workload-fix)

### Prompt Context

**Verbatim prompt:** "You are a builder in s4ql (`maragu.dev/s4ql`), a Go SQL database with its own SQL engine checked against SQLite. Your task is small and bounded: close the gaps that an independent review found in the **generated workload for arithmetic** in `internal/sql/stmt/stmttest/`. Do nothing beyond the list below. [...] 1. **A tested expression is never compared with a plain value, a parameter, or a subquery.** [...] 2. **Refusals that are almost never generated** [...] 3. **`expressionClass` in `arithmetic.go` (near line 284)** returns the class "" for a refused TEXT/BLOB leaf [...] 4. **`ArithmeticRefusal` in `arithmetic.go` (near line 340)** [...] 5. **`TestSequence_arithmetic`** runs only the `arithmetic` profile [...] 6. **Arithmetic inside a subquery** comes only from `mixed` [...] If the new statements show s4ql giving another answer than SQLite where it should not, that is a fault in s4ql: do not hide it; record it and report it plainly."
**Interpretation:** make the generator write the missing forms in `arithmetic` and `mixed`, count them as reach kinds of the engine's differential test, prove each with the five named planted faults under `-short`, fix the two rules of `arithmetic.go`, add a direct test of the masking, give `arithmetic` subqueries, and recount.
**Inferred intent:** every class check of arithmetic in the engine is reached by generated statements often enough that turning it off fails the quick test run.

### What I did

- `/internal/sql/stmt/stmttest/arithmetic.go`: `expressionClass` gives "" with any refused operand; `whereRefusals` and `setRefusals` skip the class comparison of a condition or an assignment with a refused operand; `ArithmeticRefusal` matches the refusals of a correlated subquery and of LIMIT. `/internal/sql/stmt/stmttest/arithmetic_test.go`: 15 new cases, and the check of `ArithmeticRefusal` on every refusal (red first: 7 failures, then green). Commit `40f5a8c`.
- `/internal/sql/stmt/stmttest/stmttest_test.go`: a subtest of `TestGenerator_ValueFirst` with `ValueFirst` 1 and five given comparisons; replacing `turn = false` by `_ = turn` fails it with 6 failures. Commit `8c1d642`.
- `/internal/sql/stmt/stmttest/sequence.go`: `testedOf`, `plainOf`, `nullExpression`, `integerExpression`; `arithmeticCondition` draws from 14 with two draws of a tested expression against a plain value, and a NULL expression for half of the faults of TEXT and BLOB; `arithmeticWhere` gives a comparison with a subquery a tested expression one in `max(1, Arithmetic/2)`, of another class one time in two; `arithmeticSet` gives NULL to TEXT and BLOB and an integer expression to REAL (one in eight) as faults. `/internal/sql/stmt/stmttest/profile.go`: `Arithmetic` has `Subquery: 5`. `TestSequence_arithmetic` counts both profiles now and requires the new forms.
- `/internal/engine/expression_test.go`: `reachCompared` and `classRefusals` count "arithmetic: expression compared with a plain value, ok", "... with a subquery, ok", and the refusals "a plain value compared with an expression", "a subquery compared with an expression", "an expression of type null compared with a TEXT or BLOB column", "... for a TEXT or BLOB column", and "an expression of type integer for a REAL column".
- `/internal/engine/subquery.go`: the fault of s4ql below. `/internal/engine/expression_test.go`: `pair.due` and `limitZero`, the gap of the pair below.
- Recounts with `/scratchpad/wlfix/recount.sh` (the previous builder's script pointed at this worktree), `/scratchpad/wlfix/lost.py` and `lostu.py` to list kinds that the new blocks lost, `/scratchpad/wlfix/note.py` for the comment of the lost ones, `S4QL_RECOUNT=1` of the driver check. `/scratchpad/wlfix/plants.py` plants each of the five faults in turn and runs the short differential test.
- Docs: `/docs/todo/arithmetic.md` "Fixes after the review of the workload", `/internal/sql/stmt/stmttest/README.md`, `/internal/engine/README.md` (the `row value misused` rule, a subquery with LIMIT 0 computes no constant).

### Why

The class checks of `build.types` (a plain value against a tested expression), `subqueries.types` (a subquery against one), `arithmeticClass`, `meetsArithmetic`, and the REAL check of `update.check` were reached by almost no generated statement, so a fault in any of them would pass every generated test.

### What worked

- All five planted faults fail `go test -short ./internal/engine/ -run 'TestDifferential$'`: the class check of a plain value off (`s4ql: internal error: a comparison of an real with a integer` on `of + ? = 1`), the class check of a subquery off, `TypeNull` mapped to `classInteger` and `meetsArithmetic` letting TEXT meet NULL (both on `for > (? * 3)` with NULL, `for` TEXT), and an integer accepted for REAL (on `set trigger = 3 * 1` of a REAL column).
- No difference from SQLite in 1,000 seeds of `arithmetic`, 800 of `mixed`, 300 transactions of `arithmetic` (scratch test), and no violation in 150 seeds of the simulation of `arithmetic`, after the two fixes below.
- `make test`, `make lint`, and the new tests 20 times under `-race` pass.

### What didn't work

- The first recount failed in `TestConn_differential` on seed 64000013: `update conflict set s4ql = ? where sqlite + 1 = ? + 1 and a1b2 == (select ...) and (select * from conflict limit 1) = s4ql + ?` gave `1/1: sub-select returns 3 columns - expected 1` in s4ql and `1/1: row value misused` in SQLite. A probe of 19 statements (scratch `zz_probe_test.go`) showed SQLite gives `row value misused` in an update or a delete for a subquery of `*` on the left of an arithmetic expression with every operator, and `sub-select returns` with it on the right. A fault of s4ql: `subqueries.widths` knew only `!=`. Fixed, pinned in `TestSubqueries` and `TestEngine_subquery` (red first: `Expected "1/1: row value misused", but got "1/1: sub-select returns 2 columns - expected 1"`). Commit `4b401db`.
- 400 and then 1,000 seeds of `arithmetic` in the scratch test found three seeds (5997, 9389, 9485): `the engine gives columns ..., rows [], and SQLite shows the quirk of s4ql: integer overflow of / at offset 155 for a row that the rule evaluates`, each a constant that fails in the WHERE of a subquery with `limit 0`. The engine computes nothing for such a subquery (`build.value` returns NULL before `b.constants`), on purpose; `pair.due` did not know it. A gap of the test, fixed in `pair.due`. The first fix panicked: `index out of range [-1]` in `stmttest.BoundValue` for a subquery without LIMIT; `limitZero` guards it.
- `S4QL_RECOUNT=1` after re-deriving the blocks: `the runs reach "statements with a subquery of another type than its column, refused" 86 times in all, and testdata/reached/TestSimulation.kinds requires 87`: lowered by hand, with a comment.
- Kinds that the old blocks required and the new ones lost could not be kept at a floor of 1: `no run reaches "reached: arithmetic with a column in WHERE of a delete, an evaluation error: s4ql: division by zero", which is required, three times`. They are named in a comment of `TestDifferential.kinds`.
- The short differential test did not catch the subquery fault at first: the first 8 seeds of `arithmetic` (a tenth under `-short`) had no subquery of another class than its expression. Raising the rate (every subquery comparison in `arithmetic`, wrong class one in two) and `Subquery` from 6 to 5 made it 2 there; `Subquery: 3` gave 3 but starved the access kinds more.
- The driver check failed twice: first `no profile reaches "reached: value on the left of != finds rows"` (mixed reached it once before, by luck, and never since), fixed by 70 sequences of `valuefirst`; then, in `make test`, `arithmetic gives "excused: an evaluation error of arithmetic, which SQLite's result of its operator justifies" 40 times, and testdata/reached/TestGenerated.tsv pins 39`, 41 in another run: the driver check opens s4ql through `s4ql.Open`, which draws the directions of its scans from the host, and a subquery with arithmetic now fails on a row or not by the direction. That excuse is no longer pinned exactly; the reached kinds of the justified errors keep their floors. Five runs with `-shuffle on` pass since.
- Shell commands with a heredoc and `git` or `cd` were refused by the harness ("too complex to verify that it stays inside the worktree"); scripts written with the Write tool and run with `python3 <file>` worked.

### What I learned

- Under `-short`, `TestDifferential` runs the first tenth of the seeds of each profile, so a planted fault must be reachable in 8 sequences of `arithmetic` and 20 of `mixed`; a rate that is enough for the full run can be zero there.
- A required kind must be reached three times by one run, so a rare kind cannot be kept at a floor of 1.

### What was tricky

- Balancing the new forms against the old kinds: subqueries and refusals take statements from the others, and access kinds of selects disappear with subqueries, since the model declines a select with one.

### What warrants review

- `subqueries.widths` in `/internal/engine/subquery.go`: the new rule rests on the 19 probes against SQLite; `TestEngine_subquery` pins seven.
- `pair.due` and the README's sentence on a subquery with LIMIT 0: whether s4ql should compute the constants of such a subquery is a rule of s4ql, and the engine's choice was kept.
- The exact pin removed from the driver check's excuse of an evaluation error (the commit message says "a least number"; it is no longer counted in the `.tsv` at all).

### Future work

- The lost kinds of the comment in `/internal/engine/testdata/reached/TestDifferential.kinds`, if they matter, need more sequences of `arithmetic`.
- The driver check could open s4ql with fixed directions, if `s4ql.Options` gets a way, so that its excuses are pinned exactly again.
