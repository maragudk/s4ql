# Diary: Text in expressions, the engine and the driver

The third step of the piece of work "text in expressions": the engine and the driver run statements with `||`, `lower`, and `hex`, which the statement pass reads since pull request #172 and the evaluator computes since #177. Until this step the engine refused every statement with text (`textRefusal`). The work is on the branch `text-engine`, from `07cc9a2e` on `main`.

## Step 1: SQLite's rules for names and lengths, and the engine runs text

**Author:** text-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Your ONE task is the third step of the piece **text in expressions**: the ENGINE AND DRIVER run statements with `||`, `lower`, and `hex`. [...] Statements with `||`, `lower`, and `hex` run through the engine and the driver, in the `set` of an `update` and on either side of a `where` comparison, in subqueries where arithmetic runs there, and in the texts of several statements: 1. Types and refusals [...] 2. SQLite's order of errors [...] 3. The length limit [...] compute constant subtrees once, before rows, and compare with SQLite on empty tables. 4. The access [...] 5. The helpers that assume numbers [...] 6. The driver [...] 7. goqite's expression"
**Interpretation:** Remove the refusal, type text by exact types, copy SQLite's order of names inside calls, give SQLite's own too-big error where SQLite gives it, let text constants serve the access, and prove each against SQLite.
**Inferred intent:** goqite's id expression through the driver, with "SQLite's answer or an error" held for text, and each place where s4ql cannot match SQLite written down.

### What I did

Read `/docs/todo/text-functions.md`, the two diaries of the piece, the engine step of arithmetic (todo, diary, `/internal/engine/expression.go`, `operator.go`, `subquery.go`). Measured SQLite with the pinned shell (`/opt/homebrew/opt/sqlite/bin/sqlite3`, 3.54.1) under `.limit length 100` with arguments of 60 bytes, scripts in `/Volumes/990/s4ql/text-engine/sql/`, and read `resolveExprStep`, `sqlite3ExprCodeRunJustOnce`, and `sqlite3ExprCodeTemp` in `/internal/sqlite/c/sqlite3.c`.

Rewrote `/internal/engine/expression.go`: `textRefusal` gone; `resolver` (the names, with a pending column inside a call); `bindExpression` (was `bindArithmetic`, now with the length limit); `hoisted`, `bindHoisted`, `computeHoisted`; `fromExpr` maps `expr.ErrTooLong` to `tooBig()`; the classes gain `classNumber`, `expressionClass` replaces `arithmeticClass`, and `comparable` replaces `meetsArithmetic`; `scalar` (was `number`) holds text. In `/internal/engine/operator.go`: `condition` gains `hoisted`, `lazy`, `early`, `failed`; `build.walk` and `build.change` resolve names with `resolver`; `build.constants` defers a too-long constant with a call where `mayServe` says it serves no access; `build.hoist`; `update.hoisted`, and a too-long text constant of SET is computed for each row. `/internal/engine/subquery.go`: `subqueries.pendingNames`, `subqueries.all`, and `build.compute` hoists the subqueries too. `/internal/engine/engine.go` and `several.go` no longer refuse text. Tests: `TestEngine_text` rewritten in `/internal/engine/expression_test.go`, internal tests in `expression_internal_test.go`. Commit `f866e3da`.

### Why

The evaluator was ready; the engine had to place each computation where SQLite places it, since `string or blob too big` is the first error of evaluation that SQLite gives itself, so when it comes decides whether s4ql gives SQLite's answer.

### What worked

The duo of `/internal/engine/defaults_test.go`, which runs each statement on the engine and on SQLite and compares answers, counts, and tables, made every rule cheap to check: the names test runs 34 statements and the too-long test about 50, on an empty table, one row, and rows that no condition reaches.

### What didn't work

- `go test -short ./internal/engine/` first panicked: `interface conversion: engine.Expr is *engine.Integer, not *engine.Arithmetic` in `build.change`, where I asserted the type before the nil check of `u.computed[i]`.
- The first runs of the too-long tests passed for the wrong reason: the statements used `?1` and `?2`, which the pass refuses (`numbered parameter`), and the duo returns early on "not supported". Every statement there now asserts that it was not refused.
- `newDuo(t, 100, ...)` failed with `open: the length limit 100 is not from 1000 to 1000000000`; the tests use 1,000 and 600 bytes.
- `select 1 as a from t ...` is "not supported" in the engine (`result column other than a column or *`); the tests select `k`.
- The duo found three things that I had wrong: `delete from t where lower(nosuch1) <> (select nosuch3 from w)` gives `nosuch1` in SQLite (an update or a delete finds the subquery's table and resolves nothing more after an error, where a select looks its select list up); `select k from t where k = 99 and u = (select s from t where s = ? || ?)` gives no error in SQLite with an index on `s`, since a key of the subquery is computed with the subquery; and a statement that fails in a transaction left SQLite's earlier rows changed (Step 2).
- The harness refused shell commands with here-documents ("this command is too complex to verify that it stays inside the worktree"); the edit scripts went to `/Volumes/990/s4ql/text-engine/` through the Write tool.

### What I learned

SQLite computes a constant without a function in the start of the statement and one with a function behind `OP_Once` where the first row reaches it, unless the access uses it as a key, which computes it before the search; and a constant of a subquery that is no key is computed in the start of the whole statement, also when the subquery never runs. A missing column inside a call, like one under IS NULL, is swallowed by its node's callback, so the walk goes on.

### What was tricky

`k + 1 = s` looked like a fault of `main` (a TEXT column alone as the value, with `arithmeticClass` giving it the class of NULL), but the pass always makes a column alone the tested side, so it never reached there; I removed the dead branch I had written for it.

### What warrants review

`build.constants`, `mayServe`, and `build.hoist` in `/internal/engine/operator.go`, against the list "When the too-long error comes" in `/docs/todo/text-functions.md`; `resolver` and `subqueries.pendingNames`.

### Future work

The transaction question of Step 2, and the generated checks of text.

## Step 2: the driver, the shared rules, the docs, and SQLite's partial statement

**Author:** text-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "The driver: `Prepare`, `Exec`, `Query` with such statements [...] goqite's expression: `'m_' || lower(hex(?))` with a 16-byte blob argument, in a `set` and in a `where`, through the driver, as a test [...] the shared rules (`internal/sql/stmt/stmttest`: `HasExpression`, `ArithmeticRefusals`, `Justify`, `TextRefusal`) must keep judging what they judge; where a rule would now be wrong for a text statement that a hand-written test sends through it, correct the rule and say so."
**Interpretation:** Prove the driver path and goqite's expression against SQLite, and bring the shared rules up to text.
**Inferred intent:** That the differential tests and the driver check judge text correctly before the generators write it.

### What I did

`/internal/sql/stmt/stmttest/arithmetic.go`: `TextRefusal` removed; `expressionClass` types text with `textRefusals`; `isArithmetic` limits the rule of numbers to arithmetic; `ArithmeticRefusal` knows the refusals inside text; `Quirk` gives text no quirk. `/internal/sql/stmt/stmttest/rule.go`: `MissingColumn` walks as the engine's `resolver`. `pair.checkArithmetic` of the engine no longer asks for `TextRefusal`. `/conn_test.go`: `TestText` (goqite's id in SET and WHERE, prepared, in a transaction, after a restart, refusals, too long under a limit of 1,000), replacing the subtest of `TestArithmetic` that pinned the refusal. `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`: the hand sequence "text in expressions", with `update t3 set w3 = 'a' where lower(w3) = 'a'` prepared and then run. `/internal/sql/mutation/equivalent.txt`: removed the entries of `meetsArithmetic` and the five of `columnClass` and `valueClass` whose reason was that text and blobs were refused alike; renamed the entry of `build.arithmetic`. The todo has the section "The engine and the driver". A writer sub-agent updated `/internal/engine/README.md`. Commit `0598bdea` and those after.

### Why

The generated checks of text come next; their rules had to be right first, and the FuzzSQL seeds of text already run through them.

### What worked

`go test -short` of the root package, the engine, `internal/sql/...`, `internal/expr`, and `internal/sqlite/crosscheck/...` passed after the fixes below.

### What didn't work

- `TestEquivalent` failed: `no such mutant: internal/engine/expression.go meetsArithmetic swap arguments: comparable(c, a) => comparable(a, c)`.
- `TestSubqueries` failed: `Expected "s4ql: internal error: the subquery 2 of a statement with 1", but got "s4ql: internal error: a subquery 1 without its table"`; the new hoist loop now passes over a subquery without a table, which the loop after it refuses.
- `TestText` first failed on `s4ql: no valid lease: not_holder` (a database opened with `openOn` needs a ping first) and on `select ? || queue`, which the select list refuses.
- In a write transaction, `update t set s = s || s` over the rows `'a'` and 600 bytes of `'b'` under a limit of 1,000 fails in both, and SQLite's first row is `'aa'` after it: SQLite takes the statement back only where it holds a statement journal, which it opens for a statement that may abort on a constraint. Measured in the shell: an update of a column that takes NULL keeps the change, with or without an index and in a rowid table; one of a NOT NULL column takes it back; outside a transaction everything is taken back. s4ql keeps the rule of arithmetic, that a failed statement changes nothing; `TestEngine_text` pins both sides, and the todo puts it to the lead as a question of design.

### What I learned

SQLite's atomicity of a statement in a transaction is not a promise for `SQLITE_TOOBIG`: it depends on its statement journal, which depends on the constraints of the statement.

### What was tricky

The record that is too long, the older case of `string or blob too big` in an update, does not show the same partial statement in the shell: SQLite did not measure the record of the second row at all (the known "record slip").

### What warrants review

The rules of `stmttest` against the engine's messages (`TestArithmeticRefusals`), and the question of the partial statement in `/docs/todo/text-functions.md`.

### Future work

The generators of text, its planted bugs, the driver's generated test of text, mutation testing of the new code, and the lead's answer on the partial statement.
