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
