# Diary: Slice 7 of the roadmap in the statement pass, `where` on `select`

Slice 7 adds a WHERE of comparisons of a column with a literal or a `?`, and `is null` and `is not null`, joined with AND, to a select. This task is the statement pass, `internal/sql/stmt`, and its generator `stmttest`; the engine and the driver do not run a WHERE yet, and refuse it as not supported until their builder comes. The work is on the branch `where-stmt`, from `8a6dae5`, the head of `null-checks` (pull request #79, merged into `main` as `5f5cb40` while this ran).

## Step 1: The typed form, the recognizer, the generator, the checks, and the refusal in the engine

**Author:** where-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "The statement pass (`internal/sql/stmt`) reads the `where` of slice 7 on a `select`. [...] **The statement types:** `stmt.Select` carries the conditions in order, each with its column name, its operator (keep `=` and `==` apart only if SQLite gives them apart anywhere that s4ql shows; otherwise one operator), and its value as a `stmt.Term` like a value of an `insert` [...], or that it is an `is null` / `is not null` test. [...] the engine refuses a `select` with a `where` with an error that matches `stmt.ErrNotSupported`, in `engine.Check` too, so that `Prepare` in the driver refuses at once. [...] Confirm with a dump, as the earlier builders did, that the generator draws exactly as before with the option off."
**Interpretation:** Extend the recognizer and the typed form to WHERE, check every form against SQLite in the pass's tests, put the new forms behind an option and a hook of `stmttest` that the engine's and the driver's tests leave off, and make the engine refuse a select with WHERE as `checkNulls` refused NULL in slice 6.
**Inferred intent:** The front half of slice 7 with the same trust as slices 1 to 6, so that the engine's builder only has to run the conditions.

### What I did

`/internal/sql/stmt/stmt.go`: `Select.Where []Condition`, nil without WHERE; `Condition{Column Name, Operator Operator, Value Term}`; `Operator` with `OpEqual`, `OpNotEqual`, `OpLess`, `OpLessOrEqual`, `OpGreater`, `OpGreaterOrEqual`, `OpIsNull`, `OpIsNotNull`, and `String`. `=` and `==` are one token kind in the tokenizer (`token.EQ`), as `!=` and `<>` are (`token.NE`), as in SQLite, and nothing that s4ql shows prints an operator, so each pair is one operator. The Value of an IS test is the zero Term. `Select.Parameters()` counts the `?` of the conditions.

`/internal/sql/stmt/recognize.go`: `selectCmd` takes WHERE and calls `conditions`, which walks the ORs down to their leftmost operand, accepts the left spine of ANDs, reads the conditions in the order of the text, and rejects `OR` at the first OR after them. `condition` reads a comparison (`leftColumn`, then the operator, then `term`, which `insert` now shares) or `isCondition` an IS. New names: `value on the left of a comparison`, `column compared with a column`, `column with the name of a table`, `OR`, `IS DISTINCT FROM`, `IS with a value other than NULL`, and `WHERE deeper than SQLite's limit`; `NOT operator` and `TRUE or FALSE` cover WHERE too. The rest falls out of `describe`: `parentheses`, `binary operator`, `function call`, `IN`, `BETWEEN`, `LIKE, GLOB, REGEXP, or MATCH`, `COLLATE`, `NULL test`, `IS`, `ORDER BY clause`, `LIMIT clause`.

`/internal/sql/stmt/stmttest/stmttest.go`: `Generator.Conditions` (one select in two gets 1 to Conditions conditions, a column from the pool of `SelectColumn` or one time in three an earlier one, any operator in any spelling, a value from `Generator.value`) and the hook `Generator.Where`. `Generator.term` is shared with the insert. The pass's tests set `Conditions = 4`; `Sequence` sets neither.

Tests: `checkWhere` in `/internal/sql/stmt/stmt_test.go` runs src and the canonical text on a table of ANY columns that hold the values around each condition's value, of every type, and NULL (`around`, `whereRows`, up to 100 rows), with typed arguments (`whereArgs`), and compares names and rows exactly; and checks `no such table` and `no such column`. About 120 cases in `/internal/sql/stmt/check_test.go`, two subtests (keywords as a column of WHERE, the depth limit), 31 near misses for SELECT, `difference` for mutants, and `FuzzLiteral` checks the value of a condition too.

The engine: `checkWhere` in `/internal/engine/errors.go`, called by `engine.Check` and `prepare`, and `NewPlan` panics for a select with WHERE. The driver's `check` in `/conn.go` calls `engine.Check` again. `checkText` wrappers in `/internal/engine/differential_test.go` (used by `/internal/engine/simulation_test.go` too) and `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`, at every former `stmt.Check`, as in `e50dba1`. `TestEngine_where`, and `TestPrepare` and `TestSequences` pin the refusal; `TestExec`, `TestTx`, and one step of `TestSequences` use ORDER BY now for a text that the pass refuses.

### Why

The brief's list of what stays not supported, each under a name of its own; the forms that `describe` names already got no name of their own.

### What worked

- A dump of 63,000 texts and arguments of `Generator` (Columns 0 and 3, Nulls 0 and 5) and `Sequence` from 30 seeds, from the new code and from `internal/sql/stmt` of `8a6dae5` checked out under a WIP commit, was byte for byte the same (`cmp old.txt new.txt && echo SAME`).
- Planted mutants in the recognizer (`<` read as `<=`, IS NOT NULL read as IS NULL, the last condition dropped) each failed `checkWhere` at once.
- `drivercheck` passed with every pinned number unchanged.

### What didn't work

- A Bash heredoc that wrote a scratch program was refused: "this command is too complex to verify that it stays inside the worktree". The Write tool worked.
- `git checkout origin/null-checks -- internal/sql/stmt` failed with `fatal: invalid reference: origin/null-checks`: the branch was deleted after its merge. `8a6dae5` worked.
- `TestGenerate` first failed with `SQLite gives sqlite: no such column: oid (code 1), want 1 "no such column: OID"`, which led to the fact below.

### What I learned

- SQLite resolves the select list before WHERE, and WHERE in the order of the text. But a missing column under IS NULL or IS NOT NULL does not stop it: `resolveExprStep` walks the operand of TK_ISNULL itself and ignores its abort, so the walk goes on, a later IS test of a missing column replaces the message, and the next comparison stops it (`return pParse->nErr ? WRC_Abort : WRC_Continue`) without looking up its column. `a is null and b is null` gives `no such column: b`; `a is null and b = 1` gives `a`.
- SQLite refuses an expression deeper than 1000 (`SQLITE_MAX_EXPR_DEPTH`) while it parses, with `Expression tree is too large (maximum depth 1000)`. A condition is 2 deep, 3 with a sign before its value, and an AND one more than its deeper side: 999 conditions are the most, 998 with a sign in one of the first two.
- The 84 keywords that SQLite reads as a column in the select list are exactly those it reads as a column in WHERE.
- Rows come in key order: SQLite searches the primary key for a condition on it and scans in key order otherwise (`EXPLAIN QUERY PLAN`).

### What was tricky

- AND is left-deep, so the first condition is at the bottom of the tree: every AND must be accepted before the first condition is read, or a token that stops the recognizer there is named `binary operator` for the AND.
- `1and` is one bad token, so the generator writes a gap before AND always.

### What warrants review

- `conditions`, `condition`, `isCondition`, and `leftColumn` in `/internal/sql/stmt/recognize.go`, and the names and offsets of the new rejects.
- `checkWhere` and its rule for `no such column` in `/internal/sql/stmt/stmt_test.go`.

### Future work

- The README of the pass, `docs/todo.md`, and the roadmap's slice 7: step 2.
- The engine and the driver for slice 7, which remove `checkWhere`, the call in the driver's `check`, and the `checkText` wrappers.
