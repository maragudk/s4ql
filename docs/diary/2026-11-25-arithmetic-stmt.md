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
