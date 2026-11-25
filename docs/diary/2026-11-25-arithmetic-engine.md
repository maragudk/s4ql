# Diary: the engine and the driver evaluate arithmetic

The third step of the piece of work "arithmetic": wire the evaluator of `internal/expr` into the planning and the execution of `internal/engine`, remove `engine.expressionRefusal`, and make arithmetic work through the driver, so that goqite's whole receive (`update goqite set timeout = ?, received = received + 1 where queue = ? and id = (select id ... limit 1) returning id, body, received`) runs. The statement pass (`docs/diary/2026-11-25-arithmetic-stmt.md`) and the evaluator (`docs/diary/2026-11-25-arithmetic-eval.md`) came before.

## Step 1: the requirements, and the engine evaluates arithmetic

**Author:** arithmetic-engine (builder)

### Prompt Context

**Verbatim prompt:** "You are the builder of the third step of the piece of work "arithmetic" on s4ql ... ## Your step: the engine and the driver evaluate arithmetic — Wire the evaluator into planning and execution, remove the refusals, and make it work through the driver: in `select`, `update`, and `delete`, with arguments, prepared statements run again with other arguments, transactions, `returning`, scalar subqueries (an expression inside a subquery's `where`, and beside one), and after a restart with an empty cache. goqite's whole receive must now run through the driver" (the brief is the lead's; its decisions are quoted where they are applied below and in `/docs/todo/arithmetic.md`).

**Interpretation:** the lead decided the semantics: an expression without a column is a constant of one run, computed after the checks and before the access, serving lookups and bounds as a `?` does; one with a column is computed per row and never serves the access; SET sees the row before the statement; the conditions of a row are evaluated in the order of the text and stop at the first that is not true; a failed statement changes nothing; types follow the exact-types rule of a literal or argument of the result type; a TEXT or BLOB column compared with arithmetic is refused; `set a = b` copies between equal types; `where a = b` may be lifted in the pass; negative zero must match SQLite everywhere; the driver must make an evaluation error tellable; the commit format does not change.

**Inferred intent:** goqite's receive through the driver, with the agreed rule "SQLite's answer or an error" held and checked against SQLite, and the places where the outcome depends on the access or a draw written down for Markus.

### What I did

Reset the worktree to `5451365`, merged `main` (already in), renamed the branch `arithmetic-engine`. Read `/docs/todo/expressions.md`, `/docs/todo/arithmetic.md`, the README of `/internal/expr`, and the engine's planning and operators (`plan.go`, `operator.go`, `subquery.go`, `access.go`, `engine.go`, `tx.go`).

Measured with the pinned shell (3.54.1): SQLite resolves the names of the value of an assignment before its column (`update t set z = x` gives `no such column: x`, `set z = 1, a = x` gives `z`), with the question about quotes for a quoted name (`set a = "x"`); WHERE after SET; a comparison with an expression after a missing column under IS NULL ends the walk with the missing column (`where x is null and a + y = 1` gives `x`); `row value misused` of an expression on the left of a subquery of `*` has the offset of its leftmost leaf (`a + 1`, `(a + 1) * 2`, `-a` at `a`, `-1.5 * a` at `1.5`); and a REAL column set to `-0.0 * 1` stores 0.0 and gives 0.0 through RETURNING (`atan2(r, -1)` is +pi in both), in a table WITHOUT ROWID and in a rowid table.

Then commit `9fb95b4`:

- `/internal/engine/plan.go`: `engine.Arithmetic` (an `Expr`), `Condition.Expression`, `term` maps a `Term.Expression`, `Format` prints arithmetic with every binary operator in parentheses (`formatArithmetic`). `NewPlan` no longer panics.
- `/internal/engine/expression.go`: replaced the refusal with the machinery: `missingName` (names in the order of the text), `leftmostOffset`, `bindArithmetic` (`expr.Check` + `Bind`, with `fromExpr` mapping `expr.ErrInternal` to the engine's internal error), the classes of the rule of exact types (`classInteger`, `classReal`, `classText`, `classBlob`, `classAny` for NULL), and `number`, a value compared without an allocation.
- `/internal/engine/operator.go`: `condition` has `test`, `per`, and `constant` (`*expr.Bound`); `condition.computed` evaluates both sides in the order of the text; `bounds` drops the per-row conditions from the access (`build.reach`) and from `never`; `build.walk` looks the names of expressions up; `build.types` binds and checks the classes; `build.constants` computes the constants (NULL in a `readOnly` build); `build.change` resolves SET's names before its column, binds SET after the lengths of SET and WHERE, computes SET's constants after WHERE's types, and builds an `update` with `computed` and `copied`; `update.newRow` computes the row from the old row and checks NOT NULL per row in the order of the table when a value is per row; `update.check` checks the classes of arithmetic and of a copied column.
- `/internal/engine/subquery.go`: `subqueries.types` takes the tested expression; `build.compute` computes the constants first; `build.value` computes a subquery's constants when it is computed.
- `/internal/engine/engine.go`: `check` no longer refuses; tests: `expressed` is now a plain test of the statement, `errText` knows `evaluation error`.

`go test ./internal/engine -short` passed (64 s). A smoke test (uncommitted, `zz_smoke_test.go`) showed `update t set a = a + 1`, the guard `k = 0 and 1 / (k - k) = 1` (no error), the overflow of a constant, and the refusals of `s = a + 1` and `r = a + 1`.

### Why

The evaluator was built and checked bit for bit; this step is only the wiring, and the order of the errors is what decides whether s4ql gives SQLite's error where SQLite has one.

### What worked

The existing stages of the build (names, lengths, types, compute, reach) took each piece of arithmetic in a natural place: the constants sit in `compute` beside the subqueries, which already had to come after every check and before the access.

### What didn't work

My first combined shell command (`cat > file <<EOF ... ; go test`) was refused by the harness: "this command is too complex to verify that it stays inside the worktree". Writing the file with the Write tool and running `go test` alone worked. The test helper `errText` failed the smoke test at first with `"update t set a = 9223372036854775807 + a where k = 2": the engine gives s4ql: integer overflow in + (at offset 17)`, until it learned `evaluation error`.

### What I learned

SQLite's order of names in an UPDATE is per assignment: the expression, then the column; the engine before only had columns there. A NULL argument gives `expr.TypeNull`, which meets every class, as a NULL argument meets every column today.

### What was tricky

Where the NOT NULL check of an update goes. Before, the values of SET were constants, checked once if the update finds a row, and then their types. With a value per row, NULL is known only per row, and SQLite checks NOT NULL in the order of the table on each row, over every column it sets; mixing a constant check with a per-row check would name the wrong column on the first row. So an update with any per-row value checks NOT NULL of every set column per row, and its static type refusals come before that.

### What warrants review

`build.change`'s new order (SET lengths, WHERE lengths, SET binds, WHERE types, SET constants, WHERE constants and subqueries, access), and `update.check` and `update.newRow`.

### Future work

Error mapping in `write`/`writeIn` and the driver, telemetry, the column compared with a column, the differential pair, the driver's tests, and the docs.

## Step 2: the driver, the column compared with a column, the differential pair, and the checks

**Author:** arithmetic-engine (builder)

### Prompt Context

**Verbatim prompt:** (the brief of step 1) "How the driver reports an evaluation error. ... make an evaluation error as tellable ... If that needs a new exported name in the root package, add it in the style of the existing ones"; "Also lift the statement pass's refusal of a column compared with a column (`where a = b`) for columns of the same declared type"; "Differential tests against SQLite in the style of `internal/engine/differential_test.go` (the pair skips statements with expressions today through `expressed`; make `pair.step` handle them)".

**Interpretation:** give the caller sentinels for the evaluation error and its kinds; lift `a = b` if small; make the pair judge statements with arithmetic by rules that are independent of the engine; write the unit tests, the driver's tests, and plant faults.

**Inferred intent:** that goqite's receive runs through the driver, and that every outcome of arithmetic is checked against SQLite.

### What I did

- Commit `7600180`: `s4ql.ErrEvaluation` with `s4ql.ErrOverflow`, `s4ql.ErrDivisionByZero`, `s4ql.ErrNotFinite` in `/errors.go` (`failure` got a second sentinel, `kind`), documented in `/s4ql.go`; `ran` in `/internal/engine/engine.go` leaves the count of changes after an evaluation error, in `write` and `writeIn`; `outcomeOf` gives `evaluation_error` at Debug. `TestArithmetic` in `/conn_test.go`: goqite's whole receive (counts `received` up, prepared, in a transaction rolled back, an empty queue, the annotation, a restart with an empty cache), evaluation errors through Exec, Query, QueryRow in and outside a transaction, and negative zero against SQLite, in the commit object (`committedReals`), and after a replay.
- Commit `3cbd763`: the pass takes `where a = b` as the condition on `a` with a `Term.Expression` of `ExprColumn` (`/internal/sql/stmt/recognize.go`); the engine compares the two columns (`condition.paired`) where they are of one class. It was small: seven pinned cases of `TestCheck` and three near misses changed (`v = v` and `v = w` became `v = (v)` and `v = (w)`).
- Commit `fda9d9d`: the pair. `stmttest.whereText` writes arithmetic (`stmttest.ExpressionText`, `stmttest.TermText`, `stmttest.BoundValue` exported); `subqueryCall.flatten`, `parametersIn`, `subqueryCandidates`, and `correlatedRule` read the parameters and columns of expressions; `pair.step` calls `pair.checkArithmetic` in place of `checkType` for a statement with arithmetic; `bothWays` reads a write with arithmetic by the rules and accepts an evaluation error by one path of a select if `pair.justify` justifies it; `reach`, `checkAccess`, `returnedInOrder` handle or leave out arithmetic. `TestPair_arithmetic` runs `arithmeticCases` (hand-written, on the core and in memory, with a restart) and `arithmeticCalls` (generated, 100 seeds of 40 calls). The generated run found a real bug at once: `b = ((? - r) * (-0.0 - 2.0))` with a NULL argument passed the type check (an expression whose every value is NULL meets every class) and then failed with `s4ql: internal error: a value of kind blob in a comparison of arithmetic`. I made arithmetic never meet text or blobs, whatever its arguments (`meetsArithmetic`), as the brief says, and `condition.computed` reads the column after the expressions.
- Commit `2fefed1`: `TestEquivalent` of `/internal/sql/mutation` failed with `no such mutant: internal/engine/operator.go build.bind operator: err != nil => err == nil #5`, because the end of `build.bind` moved to `build.computeAndReach`; the five entries now name it, with `#3`.
- Commit `6a2281d`: `make lint` found `func isColumn is unused` and SA4026 for `-0.0` in a Go literal of the test (`math.Copysign(0, -1)` now).
- `TestEngine_arithmetic` and `TestNewPlan_arithmetic` in `/internal/engine/expression_test.go` pin the plans, the access of constants (`primary_key reads 1`, `index t_a reads 2`) and of per-row conditions (`full_scan reads 4`), the guards of the text and of a bound, the LIMIT with the direction, the constants also without rows, the order of the names and the messages, NOT NULL per row, the count, and the event.

Runs: `go test ./internal/engine` (without -short) 155 s, ok; `go test ./internal/sql/...` ok; the root package ok but `TestOpen_realBucket`, which needs credentials that this worktree has not (not copied, as the brief says); `-race -count=50` of the new tests of the engine (60 s) and of the root package, ok; 600 seeds of the generated test, ok, once.

### Why

The pair had to keep SQLite and the instance in the same state: an evaluation error is not run in SQLite at all, which works because it leaves the count of changes and changes nothing.

### What worked

The planted faults, each found by a test (script `plant.py` in the scratchpad, each fault applied, tests run, file restored):
- per-row conditions given to the access: `TestPair_arithmetic`;
- a constant of `>` taken as NULL: all three;
- SET copied from the new row: `TestEngine_arithmetic`, `TestPair_arithmetic`;
- a comparison with NULL true: both of the engine;
- text meets arithmetic: `TestEngine_arithmetic` only;
- an evaluation error counted as a step error: both of the engine;
- no stop at the first condition that is not true: both;
- NOT NULL per row skipped: both;
- the value side always first: `TestPair_arithmetic`.

### What didn't work

Several shell commands with here-docs holding backticks were refused by the harness ("this command is too complex to verify that it stays inside the worktree"); writing the text to a scratch file and running python on it worked. A `git commit` followed by `go test` in one command was refused too.

### What I learned

The offset of an evaluation error is the start of the operator's node, its first operand, not the operator itself: `update t set a = a + 1` fails "at offset 17".

### What was tricky

The pair's notion of a row that "the documented rule evaluates": for a justification it may be any row that the conditions before the failing one hold for (a bound may or may not rule it out), and for a due error only rows that also every condition that may serve the access holds for. A select with LIMIT and a subquery have no due errors for rows, because their order decides how far they read.

### What warrants review

`pair.checkArithmetic`, `pair.justify`, `pair.due`, and `arithmeticRefusals` in `/internal/engine/expression_test.go`, which are the independent rules; `bothWays` keeping the other instance in the state of the instance.

### Future work

The generated workload in `stmttest`, the simulation's planted bugs, the driver's generated test against mattn, and mutation testing, as the brief says.

## Step 3: the documentation and the self-review

**Author:** arithmetic-engine (builder)

### Prompt Context

**Verbatim prompt:** (the brief of step 1) "Update `internal/engine/README.md`, the statement pass's README if you lift the column comparison, and `s4ql.go`'s package documentation where it speaks of errors (prose may go to a `fabrik:writer` sub-agent with `model: opus`)" and "Self-review your diff against this brief when done, and fix what you find."

**Interpretation:** have a writer put the decisions into the READMEs and the todo, check its text against the code, then review the whole diff and fix what is real.

**Inferred intent:** that the lead and Markus can read the exact rule of evaluation and every difference from SQLite without reading the code.

### What I did

A `fabrik:writer` (opus) wrote the section `## Arithmetic` of `/internal/engine/README.md`, the heading "The engine and the driver" of `/docs/todo/arithmetic.md`, the changes of `/internal/sql/stmt/README.md` for `where a = b`, and closed the built items of `/docs/todo/expressions.md`, `/docs/todo/sql-front-end.md`, and `/internal/expr/README.md` (commit `50ca156`). I corrected one line of the todo: the other instance of the pair reads a select with arithmetic by another path, and only a write by the rules. It reported a stale comment in `pair.everyPath`, which I rewrote.

The review (`code-review`, high, over `main...HEAD`) gave four findings, all fixed in `52cd4e2`:
- `row value misused` beside `-1.5 * a = (select * from u)` had the offset of the sign, where SQLite gives that of `1.5`: `leftmostOffset` now adds one for a negative literal, and `TestEngine_arithmetic` pins four offsets measured with the shell. With a space between the sign and the number it is still one short; SQLite's offsets are not promised.
- The constants of SET are computed in `build.change`, before `update.check`, so they fail before the refusal of a type of SET and before NOT NULL; the comment of `expression.go`, the README, and the todo said "after every check". The text now says which checks come before.
- The planted bug `noRowMatches` gave an internal error on a condition that tests an expression: it now clears the expression too.
- A stale reason in the comment of `recognizer.parenthesized`.

Then `make test` (all of it, the simulator's tests with `s4ql_browser` included) and `make lint` passed.

### Why

The rule of evaluation is the part Markus must know, and the review found the one place where the text and the code disagreed on it.

### What worked

Giving the writer the facts as a list, with the code to check them against; it found that the slower runs (`-race`, the sanitizer) also take 8 seeds, not only `-short`.

### What didn't work

Nothing failed in this step; the first `make test` of step 2 had stopped at `TestEquivalent`, so this was the first full run of the simulator's tests on the branch.

### What I learned

The equivalent mutants of `/internal/sql/mutation/equivalent.txt` are named by function and occurrence, so moving code between functions breaks them silently until `TestEquivalent` runs.

### What was tricky

Saying "after the checks" exactly: the update checks NOT NULL and the types of SET in its sink's `check`, after the build, while the constants are computed in the build.

### What warrants review

The section "When an evaluation error comes" of the engine's README against `build.change`, `build.compute`, and `filter.holds`.

### Future work

None beyond the generated checks of the brief and the two open items in `/docs/todo/arithmetic.md`.
