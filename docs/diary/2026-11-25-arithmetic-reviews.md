# Diary: closing the reviews of the evaluator and of the engine step of arithmetic

Two independent reviews of the piece of work "arithmetic", one of `/internal/expr` and one of the engine step (`/internal/engine`), found no wrong value, but code that no test protects and two small issues of behaviour. This diary is of the builder that closed them on the branch `arithmetic-engine-review`, made from `arithmetic-engine` with `main` merged. The decisions and the closing of each finding are in `/docs/todo/arithmetic.md` under "Reviews of the evaluator and of the engine step".

## Step 1: plant each fault, pin it, fix the length error

**Author:** arithmetic-engine-review (builder)

### Prompt Context

**Verbatim prompt:** "The branch `arithmetic-engine` makes the engine and the driver evaluate arithmetic expressions ... Two independent reviews (one of `internal/expr`, one of the engine step) found no wrong value. They found code that no test protects, and two small behaviour issues. Close them. For each test gap: plant the reviewer's fault yourself, show that your new test fails on it, and revert it." The brief then lists eleven findings (the key changed by an expression, arithmetic in a read-only transaction, which column NOT NULL names, a long argument inside arithmetic, a comparison with NULL computed per row beside a subquery, the log level of an evaluation error, three order pins, the pair's lenient judgement, the order of two refused arguments in `expr`, boundary kinds reached by luck, and a stale benchmark time), and says: "If closing a gap shows a real fault in s4ql (a wrong result, or a wrong error where SQLite also errors), put the smallest statements with s4ql's and SQLite's output first in your report, and fix it only if the fix is small and clear."

**Interpretation:** for each finding, write the test first, plant the reviewer's fault by hand, see the test fail, revert; change code only for item 4 (and for a real fault if one shows up and the fix is small); record everything in the todo of arithmetic, and the leniencies that stay in the todo of the generated workload.

**Inferred intent:** that every rule of the README of arithmetic is held by a test that fails when the rule is broken, before the branch is merged.

### What I did

Reset to `arithmetic-engine` (`cf5c017`), merged `main`, renamed the branch. Then, commit by commit:

- `TestOutcomeOf` in `/internal/engine/telemetry_internal_test.go` got the row of an `*expr.Error` (item 6). Planted `slog.LevelError` in `outcomeOf`: `Expected "DEBUG", but got "ERROR"`.
- Item 4, the one planned code change: `engine.argumentLengths` in `/internal/engine/expression.go`, called in `build.values` for the tested side and the value of each condition in the order of the text, and in `build.change` for each expression of SET. A new subtest of `TestEngine_arithmetic` uses the `duo` of `/internal/engine/defaults_test.go` under a limit of 1000 and failed first with `Expected "18/18: string or blob too big", but got "not supported"`.
- Item 9: a subtest of `TestProgram_Bind` in `/internal/expr/expr_test.go` with `? + ?` bound to a text and a blob both ways. The planted fault in `typeOf` (hold the text refusal, return a later blob one) failed it with `got "argument 2 in arithmetic, which is a blob"`, and nothing else in the package.
- Item 10: tallies in `reference.compute` of `/internal/expr/differential_test.go` for an integer result exactly at a bound, a subnormal real that is not zero, the smallest integer divided by a negative integer other than -1, and an error on the right of NULL; the kinds appended to `/internal/expr/testdata/reached/TestDifferential.kinds` with a third of their totals; the generator's divisor edge takes `-2 .. -1001` one time in four (`/internal/expr/generator_test.go`), as that kind was reached only 11 times (179 after). Pinned cases in `TestBound_Eval`. Planted faults: `lo >= math.MaxInt64` in the product's overflow check, `b < 0` in the quotient's, and flushing subnormals above the smallest to zero; each failed `TestBound_Eval`, `TestDifferential`, and some seeds of `FuzzExpr`. The generator without its new divisor failed the required kind: `seed 1 reaches "the smallest integer / a negative integer other than -1" 2 times, and at least 9 were expected`.
- Item 11: `BenchmarkBound_Eval` five times, 73.93 to 78.33 ns on the M4; the expression has 18 nodes, not 12.
- Items 3, 5, 7, and the pinned half of 2: subtests of `TestEngine_arithmetic`. Each planted fault failed its subtest: `newRow` in the order of the text (`got "... t.b"`), the SET constants after WHERE's (`got "s4ql: integer overflow in * (at offset 33)"`), WHERE's constants in reverse (`at offset 40`), the two sides of a comparison swapped in `condition.computed` (`got "s4ql: division by zero in / (at offset 48)"`), and both read-only guards removed (`got "evaluation error"`).
- Item 1: `keyChanges` (240 updates of the key on the tables `m` and `q`, in `arithmeticSetUp`), `k` in the SET of `arithmeticCalls`, and a pinned subtest. `!u.bugs.keepOldKey && !u.perRow()` failed both subtests of `TestPair_arithmetic`; an update that changes its rows in the reverse of the order of the key (a planted `slices.Reverse` in `changeRows`) failed them and the pinned subtest.
- Item 2 in the pair: `begin read only` in the runner of `TestPair_arithmetic`, a read-only section of the hand cases, one generated transaction in two read-only, and `pair.checkArithmetic` refuses an evaluation error of a write in a read-only transaction.
- Item 8: `pair.modelGuard` gives `pair.due` the range of `stmttest.ChosenAccess` for a select without a subquery or a LIMIT; the runner runs a write with RETURNING through `Query`.

### Why

A rule that no test protects can break unseen; a planted fault that passes every test is the proof of the gap, and one that fails the new test is the proof that the gap is closed.

### What worked

The `duo` helper of the defaults tests compares one statement with SQLite and pins the expected error in one line, so each pin is checked against SQLite as well as against the stated text. Planting faults with the Edit tool and reverting with `git checkout <file>` after each commit kept the tree clean.

### What didn't work

- A `python3` here-doc that replaced code containing `eval(` was refused by the harness: "this command is too complex to verify that it stays inside the worktree". The word "eval" in the code was enough; the Edit tool worked.
- A `sed` with two substitutions to swap `if c.valueFirst` and `if !c.valueFirst` applied both to the first line and left the two `if`s equal; I reverted and swapped them with two Edit calls.
- My first read-only pin passed `nil` as an argument of `update t set a = a + 1`, which has no parameter: `Expected "25/25: column index out of range", but got "wrong number of arguments"`.
- The first planted reorder fault of item 8 (plain conditions before per-row ones in `filter.holds`) passed `TestPair_arithmetic` even with the model's guard. A probe showed the pair catching it in isolation; in the list, `update n set i = ? + 1` with NULL had set `i` to NULL on every row before the overflow section, so its cases of `i` computed nothing.

### What I learned

- SQLite changes the rows of an UPDATE that changes the key of a table WITHOUT ROWID in the order of the key, whatever the index that found them: `update m set k = k + 1 where b <= -1` with an index on `b = -k` fails with `UNIQUE constraint failed` in both.
- The pair's text of a real does show the sign of zero (`realText` writes `real -0`); what hid -0.0 through RETURNING was that a write ran only through `Exec`.

### What was tricky

The read-only coverage found a real fault (see step 2), which made one of the two read-only faults of the review equivalent. And the reorder fault of item 8 needed three things together: the model's guard, the restored rows, and a case whose plain condition does not serve the access.

### What warrants review

`pair.modelGuard` in `/internal/engine/expression_test.go`: how it turns `stmttest.Access` back into conditions (pins, `IS NULL` pins, open and closed bounds, the key of an index followed by the columns of the primary key it lacks, `None` as `key IS NULL`). And the restructured `arithmeticSetUp` and `arithmeticCases` (`slices.Concat` with `arithmeticRows`).

### Future work

The leniencies of the pair that stay are open in `/docs/todo/generated-workload.md`.

## Step 2: the read-only fault, and the self-review

**Author:** arithmetic-engine-review (builder)

### Prompt Context

**Verbatim prompt:** (the brief of step 1) "If closing a gap shows a real fault in s4ql ... fix it only if the fix is small and clear", and "`make test` and the linter before you hand in."

**Interpretation:** fix the read-only fault if small, then run every check and the review.

**Inferred intent:** a branch the lead can merge without reading every line again.

### What I did

The generated read-only transactions failed at once: `"update n set k = j, i = (r - s) where i > (-9223372036854775808 + 9223372036854775807) returning *" with the arguments []interface {}(nil): the engine gives "s4ql: not supported: column of type TEXT in arithmetic (at offset 29)", and the rules []`. A probe in a read-only transaction gave:

- `update t set a = s + 1`: s4ql `not supported: column of type TEXT in arithmetic (at offset 17)`, SQLite `attempt to write a readonly database`;
- `update t set a = a + ?` with `'1'`: s4ql `not supported: argument 1 in arithmetic, which is text (at offset 21)`, SQLite the read-only error;
- `update t set a = 'x'`, `= ?` with `'x'`, `= s`, and `set s = a + 1`: the read-only error in both.

So `build.change` now calls `update.bind` only outside a read-only transaction, which also makes the later `!lookups` guard on the constants of SET redundant; it is gone. The pinned read-only subtest has the four statements, and `update t set a = s + 1 where s = k + 1` still gives the refusal of WHERE first, as for a plain value. The README of the engine says both, and how an argument inside arithmetic gets its length error.

Checks: `go test ./internal/engine -run 'TestPair_arithmetic|TestEngine_arithmetic' -count=20` (100 s), the same with `-race -count=5` (16 s), `go test ./internal/expr -race -count=3`, `go test ./internal/sql/mutation -run TestEquivalent`, `make lint` (0 issues), `make test`.

### Why

SQLite has an error there, and s4ql gave its own refusal in its place, unlike for every plain value of SET; the fix is one guard.

### What worked

The pair's own rule (`arithmeticRefusals` leaves the refusals of SET out in a read-only transaction) was already right; running the pair read-only was enough to show the engine disagreed.

### What didn't work

Nothing else failed in this step.

### What I learned

Every error of `buildSink` comes before the read-only error of `Conn.writeIn` (`/internal/engine/tx.go`), so whatever the build checks is "before the write"; a check that should come after it belongs in the sink's `check`, which a read-only build never runs.

### What was tricky

Deciding which side to move: the WHERE refusals come before the read-only error by an earlier rule (`readOnlyRule`), while the plain values of SET come after it. The expressions of SET now follow the plain values of SET.

### What warrants review

The guard on `u.bind` in `build.change` of `/internal/engine/operator.go`, and that nothing in a read-only build reads `u.computed` or `u.copied`.

### Future work

None beyond the open items of the todos.
