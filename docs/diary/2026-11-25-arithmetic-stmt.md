# Diary: Arithmetic in the statement pass

The piece of work "arithmetic" is the first step into expressions: s4ql evaluates them with an evaluator of its own, and gives SQLite's answer or an error for each. Its whole scope is the evaluator and arithmetic with `+`, `-`, `*`, `/` in SET and in WHERE, on integers and reals. This task is its first step, the statement pass, `internal/sql/stmt`: read arithmetic expressions into the typed statements, check them against SQLite, and make the engine and the driver refuse a statement with one until the evaluator runs it. The work is on the branch `arithmetic-stmt`, from `15993be` on `main`.

## Step 1: The brief, SQLite's behaviour, and the shape of the typed form

**Author:** arithmetic-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Make the recognizer read arithmetic expressions and carry them in the statement values, with tests. The engine does not evaluate them yet: make the engine (and so the driver) refuse a statement that carries an expression with a clear "not supported yet" error, as slice 15's statement pass did for the subquery [...] An expression is built from: a column name (bare or double-quoted), an integer or real literal, a `?`, a parenthesised expression, the binary operators `+`, `-`, `*`, `/`, and a unary `-`. Precedence and associativity are SQLite's. [...] Where an expression may stand: as the right side of `=` in the `set` of an `update`, and as either operand of a comparison in a `where` [...] The typed form is the contract for the engine: design one expression type in `stmt` that later pieces can extend with more operators, functions, and boolean logic without a second redesign [...] What the statement pass can refuse from the text alone, it refuses with an error that says what [...]"
**Interpretation:** Read an arithmetic expression wherever SET takes a value and wherever a comparison of WHERE takes an operand, as a tree in the typed form, refuse from the text what SQLite would answer with a quirk, check every supported text against SQLite, also its structure, put expressions into the generator behind an option, and refuse them above the pass.
**Inferred intent:** The front half of arithmetic, so that the builder of the evaluator has a typed form to evaluate, the facts of SQLite, and only a refusal to remove; `received = received + 1` of goqite's receive becomes possible.

### What I did

Read `/AGENTS.md`, `/docs/todo/expressions.md`, the statement pass of slice 15 in `/docs/todo/slice-15.md` and `/docs/diary/2026-11-24-subquery-stmt.md`, `/internal/sql/stmt/stmt.go`, `/internal/sql/stmt/recognize.go`, `/internal/sql/stmt/vocabulary.go`, the typed form and the names of the README of the pass, and the commit `4ad32da` that refused the subquery above the pass. Built the pinned shell into the scratchpad (`cc -O1 -include internal/sqlite/options.h -Iinternal/sqlite/c internal/sqlite/c/shell.c internal/sqlite/c/sqlite3.c -o sqlite3 -lm`, 3.54.1) and measured with a small Python driver of the shell that maps each error to its line.

### Why

The typed form is the contract for the evaluator, and what SQLite does with the texts decides what the pass refuses from the text alone and what it leaves to the engine.

### What worked

The shell answered every question:

- `-9223372036854775808` is the integer -9223372036854775808, also as `-(9223372036854775808)`, since SQLite's parser drops parentheses. `- -9223372036854775808` is the real 9.2233720368547758e+18 (the negation overflows), and `1 - 9223372036854775808` is a real, since the literal after a binary `-` does not fit. `1 - -5` is 6, and `1--5` is 1: `--5` is a comment.
- `5 / 0`, `5 / 0.0`, `null / 0`, and `1 / -0.0` are NULL; `9223372036854775807 + 1` and `-9223372036854775808 / -1` are reals; `7 / 2` is 3 and `-7 / 2` is -3; `'1' + 1` is 2, `'a' + 1` is 1, `x'31' + 1` is 2, `null + 1` is NULL.
- The depth: a literal, a column, and a `?` are 1 deep, a `-` before a literal 2 (SQLite keeps the minus as a node), every operator one more than its deeper operand, and parentheses add nothing. `a = 1 + 1 + ... ` takes 998 `+` and refuses 999 (`Expression tree is too large (maximum depth 1000)`), the same on the left; `-1 * 1 * ...` and `1 * -1 * ...` take one fewer; SET takes 999 `+` and refuses 1,000, independently of the WHERE.
- SQLite's parser stack: `(1 + (1 + ...))` of 830 levels parses, 831 give `Recursion limit`, about three entries of the stack for each level. The parser of s4ql reads up to 2,499 levels, so the pass must refuse such texts itself.

### What didn't work

The first probe piped a script into the shell and read its output: the errors went to stderr unbuffered and the labels to stdout buffered, so the errors came out of order, and I read `where 998` as refused until the line numbers of the errors showed it was 999. The driver in the scratchpad now maps each `error near line N` to its statement.

### What I learned

SQLite counts the minus of a negative literal as a node of the depth, but codes it as one literal: a negative literal is one value with a height of 2.

### What was tricky

The statements hold no pointer and are compared with `%#v`. A tree of values does not need a pointer: a slice of its own type is allowed in a Go struct and prints by value, so `stmt.Expression` holds its operands as `[]stmt.Expression`.

### What warrants review

The decisions, once written in `/docs/todo/arithmetic.md`: the refusal of parentheses around a single value and of a second sign, the limit of 100 on nested parentheses, and how a condition with columns on both sides is normalised.

### Future work

The recognizer, its tests, the refusal above the pass, the checks against SQLite, and the generator option: the next steps.

## Step 2: The recognizer, the checks against SQLite, the refusal above the pass, and the generator

**Author:** arithmetic-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Checks in this step: unit tests of the recognizer in the style of the existing ones (accepted shapes, precedence and associativity shown by the typed form, each refusal, spans, parameter numbering, whitespace, comments, quoting); the existing test that compares the pass with SQLite (`TestCheck` and its helpers) extended so that every accepted text with an expression is checked against SQLite, including that SQLite parses it with the same structure [...]; the near misses and the generator of statements (`internal/sql/stmt/stmttest`): a generator option that writes expressions, off by default, as `Generator.Subquery` was added, so that existing digests and generated sequences do not change."
**Interpretation:** Build the typed form and the recognizer, give every supported text with an expression a check of its tree and its meaning in SQLite, refuse such statements in the engine and the driver, and put expressions into the generator behind an option that changes nothing when it is 0.
**Inferred intent:** The same trust for arithmetic as for slice 15, with a typed form that the builder of the evaluator can evaluate without a redesign.

### What I did

`/internal/sql/stmt/stmt.go`: `stmt.Expression`, `stmt.ExpressionKind` with eight kinds, `Expression.Present` and `Expression.HasColumn`, `Term.Expression`, `Condition.Expression`, and `Parameters()` counting the parameters of expressions. `/internal/sql/stmt/recognize.go`: `recognizer.condition` reads both sides with `recognizer.operand` and decides afterwards which side the condition tests; `recognizer.setValue`, `plainValue`, `recognizer.expression`, `recognizer.unary`, `recognizer.parenthesized`, `isZero`, and `maxParentheses`; `depth` is gone, since the heights now come from the operands.

Tests. `/internal/sql/stmt/check_test.go`: a section of 96 cases (36 supported, 58 not supported, 2 syntax errors), 12 older cases removed as supported now and 3 with new names, and three subtests: the depth at both edges in SET, in WHERE on either side, after an AND, after a negative literal, and in a subquery with the tables; 100 and 101 parentheses of the shape that holds the most of SQLite's parser stack, and 831 levels that SQLite refuses with `Recursion limit`; and goqite's receive with `received = received + 1`. `/internal/sql/stmt/stmt_test.go`: `checkExpression` in `checkPositions` (spans, operators between operands, parameter numbers, names), `canonicalExpression` with only the parentheses that SQLite needs, and `checkExpressions` in `checkMeaning`, which checks each literal, each expression against `parenthesized` on a table of sample values row for row, and the statement against its canonical text. `/internal/sql/stmt/vocabulary_test.go`: `Paren` is allowed. `/internal/sql/stmt/generate_test.go`: near misses that are supported now replaced, 7 new ones, `Expression = 4` in `newGenerator` but for the near misses, and the pins of `TestGenerate_mutants` at 34 and 165.

Above the pass: `engine.expressionRefusal` in `/internal/engine/expression.go`, called in `engine.check` and in `engine.NewPlan`, which panics; `engine.Check` again in the driver's `check` in `/conn.go`; `expressed` and the skips in `pair.step` and `pair.everyPath` in `/internal/engine/differential_test.go`; `TestCheck_expression`, `TestConn_expression`, `TestPair_expression`, and two cases of `TestPrepare` in `/conn_test.go`.

The generator: `/internal/sql/stmt/stmttest/expression.go` and `Generator.Expression` in `/internal/sql/stmt/stmttest/stmttest.go`, and `TestGenerator_Expression`. Notes in `/docs/todo/arithmetic.md`, its line in `/docs/todo.md`, two open items in `/docs/todo/expressions.md`, one in `/docs/todo/sql-front-end.md`.

### Why

A tree that holds its operands in a slice of its own type keeps the statements free of pointers and printable with `%#v`, and a later operator or function is one more kind. Reading both sides of a comparison before deciding which one the condition tests makes the rule symmetric, and keeps the plain column and the plain value exactly as they were, so the planning of lookups in the engine does not move.

### What worked

The first run of `TestCheck` with the 96 cases passed: the offsets had been counted by hand, and the meaning check against SQLite passed on each. A planted fault, `parenthesized` without its parentheses, made 5 cases fail with "gives X for the text and Y for the tree", so the tree check sees a wrong grouping. A dump of 15,608 lines of the generator's texts and arguments with `Expression` 0 was the same before and after the change (`cmp` of the two files). Short fuzz runs of `FuzzGenerate`, `FuzzCheck`, and `FuzzSQL_core` (45, 45, and 40 seconds) found nothing.

### What didn't work

- `go vet ./internal/sql/stmt` after the new field: `stmt_test.go:2433:12: invalid operation: s.Limit != (stmt.Term{}) (struct containing stmt.Expression cannot be compared)`. A `Term` with a slice is no longer comparable; the test compares `%#v` now.
- The first canonical text put every operator in parentheses, and the pass refused it: `the canonical text "update \"t\" set \"v\" = (((((...` of the statement is not supported: s4ql: not supported: parentheses deeper than 100 (at offset 121)`, for a chain of 999 `+`. The canonical text now has only the parentheses that SQLite needs, and the tree check uses a text of its own, which is never checked by the pass.
- The parentheses subtest first nested `-(a)` innermost, which the pass refuses as `parentheses (at offset 1016)`; the innermost is `a + 1` now.
- `TestGenerate` with expressions: `gives <nil> near "/": syntax error`, for `(with / ...`: after an opening parenthesis SQLite reads `with` as the start of a subquery. The generator now draws a column again after `(` while it is `select`, `with`, or `values`.
- The generator panicked in `nameOf` from `Generator.order`, since a condition that tests an expression has no column, and three places collected the columns of the conditions for ORDER BY and RETURNING. They skip the empty name now.
- `TestPair_expression` failed on my own detection of a `?` in the text (`src[len(src)-11]`), which missed `-v1 = ? returning *`; it uses `strings.Contains` now.

### What I learned

The near misses take the text of the first value of SET as a value of their forms, so an expression there turns forms like `(%[3]v)` into supported texts; the near misses keep `Expression` at 0, as they keep `Parameters` at 0.

### What was tricky

The rule for which side a condition tests when both have a column. Choosing the side that is a column alone keeps `Condition.Column` for every condition that has one, so `Condition.Expression` appears only where neither side is a column alone, and the generator can write such conditions without knowing the pass's rule beyond that.

### What warrants review

`recognizer.condition` and its two pre-checks, which keep the names of slice 12 (`value compared with a value` and `column compared with a column` before the right side is read); the limit of 100 parentheses; and the choice of `expression` as the name of the engine's refusal, where slice 15 restored the pass's old name: here the old names varied by form (`binary operator`, `column reference`, `unary operator`, `parameter after a sign`).

### Future work

The README of the pass, by a writer, and `make test` and the linter over everything.
