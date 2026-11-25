# Diary: the generated checks of arithmetic

Arithmetic (`+ - * /` and the negation over integers and reals, in SET and on either side of a WHERE comparison) was on `main` with table tests and a generated pair of its own (`TestPair_arithmetic`), but `stmttest.Sequence` wrote no expression, the shared rules of `stmttest` panicked on one (`stmttest.HasExpression`), and the driver check left every such statement out (`pair.leaveOut`). This task makes arithmetic an area of the generator with a profile, checks it in the engine's differential tests, the simulation with planted bugs, and the driver check against mattn, in the way the scalar subquery got its generated checks on 2026-11-24.

## Step 1: the area, the profile, the shared rules, the engine's differential tests and the simulation

**Author:** builder (arithmetic-checks)

### Prompt Context

**Verbatim prompt:** "You are the builder of the **generated checks for arithmetic** in s4ql (`maragu.dev/s4ql`) [...] Arithmetic must be checked by generated statements everywhere the other features are, with nothing left out in a named exception: 1. **Workload.** [...] 2. **The engine's differential test** against SQLite: reach kinds for arithmetic, shown reached [...] 3. **The simulation with planted bugs** [...] 4. **The driver's generated test** against mattn: remove `pair.leaveOut` [...] 5. **Docs** [...]"
**Interpretation:** follow the recipe "Add a feature area" of `/internal/sql/stmt/stmttest/README.md`; let the sequence's hooks give typed trees of arithmetic that the generator writes; move the rules of the engine's pair into `stmttest` so that the simulation and the driver check share them; count the cases of the brief as kinds and require them.
**Inferred intent:** trust in arithmetic rests on the same generated machinery as every other feature, with floors that fail when the generator stops reaching what matters.

### What I did

- `/internal/sql/stmt/stmttest/expression.go`: `Generator.given`, `givenTerm`, `givenOperand`, `givenValue`, `givenColumn` write a tree that a hook gives (columns by raw name, `stmt.ExprValue` leaves), with parentheses where SQLite needs them and one time in four elsewhere. A value is written as a `?` where a literal would be refused or read otherwise: NULL, text, a blob, a value after a negation, a zero or subnormal divisor. `/internal/sql/stmt/stmttest/stmttest.go`: `conditions`, `conditionValue`, and `update` take `Condition.Expression` and `Term.Expression` from the hooks; a comparison whose two sides both have a column keeps its sides (the pass tests the left one).
- `/internal/sql/stmt/stmttest/profile.go`: the area `arithmetic`, `Profile.Arithmetic`, the profile `Arithmetic` at slot 13, `Mixed` with `Arithmetic: 8`.
- `/internal/sql/stmt/stmttest/sequence.go`: `arithmeticHooks` wraps the hooks of WHERE, update, and delete; `arithmeticWhere`/`arithmeticCondition` and `arithmeticSet` turn one comparison or assignment in `Arithmetic` into arithmetic (constants equal to the drawn value, tested expressions, a column alone, column and number, values that fail for all rows, some, or none, both sides with a column, faults of the other class); `failingWhere` (`x != 10 / (x - s)` with `s` a stored value, or `x * 1e308`) and `keyWhere` (key equal to or between constants) replace the WHERE of a select now and then; `failingRow` makes `Sequence.order` give such a select a LIMIT of 1 or 2 three times in four; `ArithmeticRefused` are 18 texts the pass refuses, one call in `8*Arithmetic`.
- `/internal/sql/stmt/stmttest/arithmetic.go` (new): the rules moved from `/internal/engine/expression_test.go`: `ArithmeticRefusals`, `ColumnClass`, `ArgumentClass`, `ArithmeticSites`, `Operators`, `EvaluationKinds`, `Quirk`, `QuirkQuery`, `SiteGuard`, `Justify`, `EvaluationOf` (parses a message, for the driver check), `ArithmeticRefusal`.
- `/internal/engine/expression_test.go`: the pair uses them; `pair.reducedSelect`; `pair.reachOfArithmetic` and `pair.reachServed` count the cases of the brief. `/internal/engine/differential_test.go`: `pair.checkAccess` now checks a select with arithmetic against the model on the reduced select (it skipped them before); `typeRule` and `readOnlyRule` give "" for arithmetic; `sequencesOf` doubles `arithmetic`. `/internal/engine/tx_test.go`: commits and roll backs of a transaction with an evaluation error, twice as many sequences of `arithmetic`.
- `/internal/engine/simulation_test.go`: an evaluation error is `skipped` with its `*expr.Error`; `run.arithmetic` checks a refusal against `arithmeticRefusals` and justifies an evaluation error on the state the statement saw (own or in a transaction); the log and the succeeded statements of a transaction must have no arithmetic refusal; `arithmeticKinds` for the tally; cells of 8 and 12 seeds of `arithmetic`.
- Recounts of all four tests; new kinds appended to the four `.kinds` files at a third of their totals (script `/scratchpad/arithchecks/kinds.py`). Commit `e803505`.

### Why

The pair already judged arithmetic by rules independent of the engine; the simulation and the driver check need the same rules, and the only way to share them across packages of tests is `stmttest`.

### What worked

- No sequence of any profile found a difference from SQLite: `TestDifferential`, `TestConn_differential`, and `TestSimulation` passed with only counts to recount. `checkAccess` agreed with the model on 157 selects with arithmetic of `arithmetic` in 80 sequences.
- A scratch test (`/internal/engine/zz_scratch_test.go`, not committed) runs the sequences of one profile, optionally with a planted bug, and prints chosen kinds; `zz_sim_scratch_test.go` does the same for seeds of one simulation cell. Each change of the generator was measured in seconds.

### What didn't work

- The first run of the pair on the new sequences panicked: `panic: runtime error: index out of range [-1]` in `plainName` < `setRefusals`, for a SET with a plain NULL beside arithmetic; the old `plainName` had no case for `stmt.Null`. The moved `plainName` names NULL.
- The writer panicked on `-(-x)` (`stmttest: a negation of a negation`), from `Sequence.nested` negating a negation; `nested` divides by one there instead.
- `TestSequence_arithmetic` failed on "a tested expression on the right": the profile has `ValueFirst` 0, so that case is reached only in `mixed`.
- Recounts: `the runs reach "reached: subquery on the left of != in a select" 4 times in all, and testdata/reached/TestDifferential.kinds requires 5` and, in the simulation, `"statements with a subquery of another type than its column, refused" 87 times in all [...] requires 90`: both from `mixed`, whose subquery statements now get arithmetic or a replaced WHERE now and then; lowered by hand with a comment. `TestGenerated`: `no run reaches "reached: insert of NULL beside a value of another type NOT NULL constraint failed" [...] mixed 1`: taken out of `TestGenerated.kinds` with a comment; `TestDifferential` requires it.
- Shell commands that combined a heredoc, `sed`, and `go test`, or a `python3 - <<EOF` with certain contents, were refused by the harness ("too complex to verify that it stays inside the worktree"); scripts in `/scratchpad/arithchecks/` run with `python3 <file>` worked.

### What I learned

- Most selects of a focused profile find no row (small tables, narrow conditions), so "a LIMIT that gave rows where an unread row fails" needed a WHERE of its own (`failingWhere`) that holds for most rows.
- `TestGenerated` of the driver check is not deterministic in every count: "reached: select * with NULL ok" of `subquery` was 9, 8, and 10 in three runs of the same code. Its floor is 3, so it holds.

### What was tricky

- Keeping the typed form the generator returns equal to the pass's: which side a comparison tests, a negative literal, a negation of a literal, a divisor of zero. `TestSequence_arithmetic` compares `%#v` of both on 3,000 statements.

### What warrants review

- `Generator.given` and the masking of `turn` in `Generator.conditions`.
- `pair.reachOfArithmetic`: the kinds are what the brief asks to be shown reached.
- `run.arithmetic` and its call sites in `run.verify` and `run.verifyTxs`.

### Future work

- Planted bugs, the driver check, docs.

## Step 2: planted bugs, the driver check, recounts, and docs

**Author:** builder (arithmetic-checks)

### Prompt Context

**Verbatim prompt:** (the brief of step 1, parts 3 to 5) and, from the coordinator near the end: "Stop now at a committed, green state: your context is nearly used up, and the task was scoped too large. Finish only the unit you are in the middle of (or revert it if it is not close to green), commit, write the diary step, and report."
**Interpretation:** plant bugs in the engine's arithmetic, find each by the generated tests with full symptom lists checked on 1,000 seeds, make the driver check judge arithmetic instead of leaving it out, recount, document, and stop green.
**Inferred intent:** every check that meets arithmetic judges it, and a fault of arithmetic is found.

### What I did

- `/internal/engine/bugs.go`, `operator.go`, `tx.go`, `export_test.go`: seven planted bugs, `setFromNew`, `nullCompares`, `evalStops`, `errorSkipsRow`, `copyAcross`, `copySelf`, `constantsServeNothing` (replaced `skipNullSide`, which only a due check could see: 0 of 30 sequences). Entries in `TestDifferential_plantedBugs` and `TestSimulation_plantedBugs` (`arithmeticFinds`).
- Tuned the profile so the bugs and kinds are reached: `Columns: 3`, `EveryRow: 3`, a SET that reads a column it sets before (and one more such assignment one SET in four), a column alone of another column.
- `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`: removed `pair.leaveOut`; `pair.checkEvaluation`, `pair.arithmeticRefusals`, `boundArgs`, `evaluated`, `conditionNames`, `expressionNames`; `checkRefused`, `checkQuoted`/`quoted`, `namesOf`, `subqueryRefusal`, and `reachSubquery` read arithmetic; `s4qlText` takes an evaluation error; the hand sequence "arithmetic, and a column compared with a column"; `generatedOf` 40 for `arithmetic`; three kinds in `TestGenerated.kinds`.
- `/internal/sql/stmt/stmttest/`: `CorrelatedRefusal` and `LimitRefusal` joined `ArithmeticRefusals`; `EvaluationOf` gives an `*expr.Error` (staticcheck ST1008); `MissingColumn`, `SelectOf`, `SetCases` read arithmetic; `arithmetic_test.go`.
- Recounted all four tests; re-derived the arithmetic blocks of the `.kinds` files (`/scratchpad/arithchecks/recount.sh`). A writer updated `/internal/sql/stmt/stmttest/README.md` and `/internal/engine/README.md`. `/docs/todo/arithmetic.md` "Generated checks"; `/docs/todo/generated-workload.md`.

### Why

The rule is "SQLite's answer or an error"; the bugs show that a wrong value, a missing error, a hidden error, a wrong copy, and a wrong access are each found.

### What worked

- `TestSimulation_plantedBugs` with `S4QL_ENGINE_SIM_SEEDS=1000`, one bug per process from a built binary (`/scratchpad/arithchecks/planted1000.sh`): all six pass; 334, 262, 202, 287, 222, 174 seeds find them, 278, 244, 168, 271, 119, 136 by their own violation. A first pass with a scratch test and a classifier (`classify.py`) of every violation found the one symptom outside the generic list, for `copyAcross`.
- `TestGenerated` compared 21,650 calls with mattn, and no difference needed an excuse beyond the two of arithmetic.

### What didn't work

- `copyAcross` on seed 11085 gave `s4ql: internal error: a comparison of a value of kind blob with one of kind integer`: the copy into the key compares the new key of another kind with the old one in `update.run`; added to its list.
- The driver check panicked first in `checkQuoted` (`index out of range [0]`, `MissingColumn` gave a zero name for a tested expression), then in `checkQuotedSubquery` (`namesOf`), then `SetCases` (`stmttest: a literal of type <nil>`), then `WhereRule` from `subqueryRefusal` and `reachSubquery`; each now reads arithmetic.
- `TestSequences`: `select v2 from t2 where v2 = (select v2 from t2 where i2 * 2 = 2)` gave rows once and `integer overflow in * (at offset 54)` the next run: the subquery reads to its first row in the drawn direction; the step takes `oneOf` both.
- `make lint`: `ST1008: error should be returned as the last argument` for `EvaluationOf`.

### What I learned

- The driver check's other kinds read `Condition.Column` in many places; any new kind of condition needs a grep for `.Column` there.

### What was tricky

- Symptom lists: the planted-bug test fails a seed only if none of its violations matches, and stops at the first, so the lists were derived from all violations of 1,000 seeds first.

### What warrants review

- `pair.checkEvaluation` (per-run handling of a prepared statement run several times).
- The code-review skill was not run: the coordinator stopped the task for context. `make test` and `make lint` pass; the new stmttest tests and the driver's hand sequence pass 20 times under `-race`.

### Future work

- Code review of the branch; mutation testing; the open items in `/docs/todo/generated-workload.md`.
