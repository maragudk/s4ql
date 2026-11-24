# Diary: Slice 15 of the roadmap in the statement pass, the scalar subquery

Slice 15 adds the scalar subquery of goqite's receive, `update goqite set timeout = ?, received = received + 1 where queue = ? and id = (select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1) returning id, body, received`, without the arithmetic in SET. This task is the statement pass, `internal/sql/stmt`: read the subquery into the typed statements, check it against SQLite, and make the engine and the driver refuse a statement with one as not supported until the engine runs it. The work is on the branch `slice-15-stmt`, from `2273d72` on `main`.

## Step 1: The brief, SQLite's behaviour, and the shape of the typed form

**Author:** subquery-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Slice 15, agreed with Markus on 2026-11-24, adds the scalar subquery of that statement. Arithmetic in `set` (`received + 1`) is NOT in this slice. [...] Make the recognizer accept a scalar subquery and carry it in the statement values, with tests. The engine does not run it yet: as slice 14's statement pass did for `returning`, make the engine (and so the driver) refuse a statement that carries a subquery with a clear "not supported yet" error, so that `main` stays consistent until the engine step. [...] A scalar subquery is a `select` in parentheses that stands where one value stands. Accept it in the `where` of `select`, `update`, and `delete`, as an operand of a comparison, in the positions where a `?` may stand today (think of it as a parameter that the statement computes itself). [...] Refuse, each with an error that says what is refused: a subquery inside a subquery; `in (select ...)`; `exists (select ...)`; a subquery in `from`; a subquery anywhere else (select list, `set`, `values`, `order by`, `limit`, `returning`, a default in `create table`); a compound select (`union` and so on) inside the parentheses. A correlated subquery [...] is the engine step's; write that down as an open point for it."
**Interpretation:** Read `(select ...)` as the value of a comparison of a WHERE, on either side of the column, with the inner select of the forms that a select has today on one table, number its parameters in the text with the rest, refuse every other place and form by name, check every supported text against SQLite, and refuse it above the pass until the engine runs it.
**Inferred intent:** The front half of slice 15 with the trust of the earlier slices, so that the engine's builder has a typed form to plan from, the facts of SQLite, and only a refusal to remove.

### What I did

Read `/docs/todo/slice-14.md`, `/docs/diary/2026-11-23-returning-stmt.md`, the recognizer, the typed form, the README of the pass, and the tests that compare the pass with SQLite (`verify` and `checkMeaning` run on every supported text of the corpus, the grammar, the mutants, the generator, and `FuzzCheck`). Built the pinned shell into the scratchpad (`cc -O1 -include internal/sqlite/options.h -Iinternal/sqlite/c internal/sqlite/c/shell.c internal/sqlite/c/sqlite3.c -o sqlite3 -lm`, 3.54.1) and measured the subquery with SQL scripts there.

### Why

The typed form is the contract for the engine's builder, and what SQLite does with a subquery decides what the pass may accept and what it must leave to the engine.

### What worked

The shell answered every question. The facts, which go into `/docs/todo/slice-15.md`:

- A subquery of two columns in a comparison with one value gives `row value misused`, not `sub-select returns 2 columns - expected 1` (that message is IN's), also `*` of a table of two columns, and also when the inner list names a column that the table lacks. A missing table gives `no such table` first.
- No row gives NULL, so the comparison chooses nothing; several rows give the first.
- In a select, SQLite finds every table of the statement, the outer one first and those of the subqueries in the order of the text, before any column: `select * from t where nope = (select k from nope2)` gives `no such table: nope2`. In an update and a delete it finds the inner table only when its walk over WHERE reaches the subquery: `update t set v = 1 where nope = 1 and k = (select k from nope2)` gives `no such column: nope`.
- The subquery is evaluated once, on the state before the statement: `update t set w = 9 where k = (select k from t where w = 0 order by k limit 1)` changes one row.
- It is evaluated lazily: with `limit ?` bound to NULL, which is `datatype mismatch`, a select on an empty table, or one whose key lookup finds nothing first, gives no error.
- SQLite rewrites the LIMIT X of a scalar subquery as `LIMIT (X <> 0)`: `limit ?` with `'x'`, `-1`, `1.5`, or `x'00'` gives the first row, with 0 or 0.0 none, and with NULL `datatype mismatch`.
- A name of the inner select that its table lacks is looked up in the outer table: `select * from t where k = (select k from u where x = v)` runs, with `v` a column of `t`.
- The depth of a subquery counts: `k = (select k from u where <n conditions>)` is `Expression tree is too large (maximum depth 1000)` from n = 998, so the comparison is two deeper than the inner WHERE.

### What didn't work

`.param set ?1 'x'` in the shell bound nothing the first time, because the shell read `'x'` as a word; `.param set ?1 "'x'"` bound the text. And without `.bail off` the shell stopped at the first error of a script piped into it.

### What I learned

A scalar subquery in SQLite is not a select with its own LIMIT: its LIMIT becomes a boolean, so the engine cannot run it as the select it reads like for every argument of a `?`.

### What was tricky

The statements are compared and printed by value in the tests (`%#v`), and the package says that no statement holds a pointer. A subquery is a select inside a select, which a value type cannot hold without a pointer. So the subqueries go in a list of the statement, `Subqueries []stmt.Select`, and the value of a condition names one by its number, `Term.Subquery`, as a parameter is named by `Term.Parameter`.

### What warrants review

The decisions in `/docs/todo/slice-15.md` once written, in particular the refusal of a list of two columns.

### Future work

The recognizer, the checks, the generator option, and the refusal in the engine and the driver: the next steps.

## Step 2: The recognizer, the checks against SQLite, the refusal above the pass, and the generator

**Author:** subquery-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Checks in this step: unit tests of the recognizer in the style of the existing ones (accepted shapes, each refusal, spans, parameter numbering, case and whitespace and comments and quoting as the existing tests cover them for other clauses), and whatever existing test in the package compares the recognizer with SQLite's own parser or runs fuzzing over it, extended so the new shapes are in its reach (look for how slice 14 did this)."
**Interpretation:** Build the typed form and the recognizer, give every supported text with a subquery a check of its meaning in SQLite, put the new shapes into the generator behind an option so that the mutants, the near misses, and the fuzzers reach them, and refuse the statements in the engine and the driver.
**Inferred intent:** The same trust for slice 15 as for slice 14, with a typed form that the engine's builder can plan from.

### What I did

`/internal/sql/stmt/stmt.go`: `Term.Subquery`, `Select.Subqueries`, `Update.Subqueries`, `Delete.Subqueries`, and `Parameters()` counting the parameters of the subqueries. `/internal/sql/stmt/recognize.go`: one counter of the parameters of the statement, `recognizer.parameters`, in place of the `before` that every function took; `recognizer.selectOf`, which `selectCmd` and a subquery share; `recognizer.subquery`, `recognizer.conditionValue`, and `recognizer.statementWhere`; the heights of the conditions returned by `recognizer.condition` and `recognizer.conditions`; and a subquery after IS named `IS with a value other than NULL`.

Tests. `/internal/sql/stmt/check_test.go`: a section of slice 15 of 66 cases (13 supported: goqite's receive without the arithmetic, both sides, every operator and spelling, the numbering of parameters before, inside, and after, `*`, case and comments and quoting, the table of the statement in the subquery, tests for NULL, `limit 0`, keywords; 48 not supported; 5 syntax errors), and two subtests, the depth at both edges against SQLite with the tables, and the facts of SQLite for the engine. `/internal/sql/stmt/stmt_test.go`: `canonicalWhere` writes a subquery; `checkPositions` checks the parentheses, the span of the inner select, its names, and the numbers of its parameters (`checkSelectPositions`, `subqueryState`); `checkSubqueries` checks the meaning of every supported text with a subquery against SQLite, by the text in which each subquery is a `?` whose argument is the value that SQLite gives the inner select alone, on the tables before the statement. `/internal/sql/stmt/generate_test.go`: `newGenerator` sets `Subquery` to 4, `difference` tells subqueries apart, 56 near misses with subqueries, and `TestGenerate` logs and requires the subqueries that gave a row and those that gave none. `/internal/sql/stmt/vocabulary_test.go`: the node `Subquery` and its fields are allowed.

`/internal/sql/stmt/stmttest/stmttest.go`: `Generator.Subquery`, `Generator.conditionValue`, and `Generator.subquery`. `TestGenerator_Subquery` in `/internal/sql/stmt/stmttest/stmttest_test.go`.

Above the pass: `subqueryRefusal` in `/internal/engine/subquery.go`, called in `check` in `/internal/engine/engine.go`; the panic in `NewPlan` in `/internal/engine/plan.go`; `engine.Check` again in the driver's `check` in `/conn.go`; and in `/internal/engine/differential_test.go` the skip of the rules of values and of `pair.everyPath` for a statement with a subquery. `TestCheck_subquery`, `TestConn_subquery`, and `TestPair_subquery` in `/internal/engine/subquery_test.go`, and `TestPrepare` in `/conn_test.go`. Notes in `/docs/todo/slice-15.md`, its line in `/docs/todo.md`, and four open items in `/docs/todo/sql-front-end.md`.

### Why

A list of subqueries on the statement and a number in the term keep the statements free of pointers, so every test that prints a statement with `%#v` and compares it still works, and the engine reads a subquery as it reads a parameter: by its number. One counter of parameters is the rule SQLite has, the order of the text, and it holds inside a subquery without a second rule.

The check of the meaning substitutes the subquery by its value, because that is the definition of a scalar subquery, and it checks at once the parentheses, the span of the inner select, the numbering of its parameters, and that the value comes from the rows before the statement.

### What worked

- Of the first 65 cases all but two were right on their first run. The two: `select * from t where v = (select 1)`, an old case, now gives the error of the result column `1` inside the subquery, and `default (select 1)` is SQLite's syntax error `near "select": syntax error`, so the case is `default ((select 1))`.
- Two planted bugs, each reverted: dropping the LIMIT of every subquery failed `verify` on "a subquery with limit 0, which gives NULL" by its rows (`Expected "[s4ql key K]\n integer 1; blob ; \n", but got "[s4ql key K]\n"`), and not counting the parameters of a subquery for those after it failed `checkPositions` (`parameter 2 with the value <nil> at {103 104}`).
- With `Subquery` 0 the generator is unchanged: a dump of 10,320 lines was the same before and after (`cmp before.txt after.txt && echo SAME`).
- `TestGenerate` checked 1,535 subqueries against SQLite in its default run, 383 of which gave no row.
- `TestPair_subquery` found a real fault before it was fixed: `pair.everyPath` panicked in `stmttest.SelectText` on `select * from t1 where v1 = (select v1 from t1)`, which FuzzSQL could have reached.

### What didn't work

- My first `checkSubqueries` expected `row value misused` for every subquery of `*` on a table of two columns. `TestGenerate` failed: `SQLite gives sqlite: sub-select returns 7 columns - expected 1 (code 1), want 1 "row value misused"`. The shell then showed that an update and a delete give `sub-select returns N columns - expected 1`, and a select `row value misused`; then `TestGenerate_mutants` failed on `(seLeCT*/**/from oid ...) <> "a""b"`, and a table of every operator on both sides showed the one exception, `!=` with the subquery on the left in an update or a delete.
- My first rule of the depth followed only the parser: 997 conditions in the WHERE of a subquery. `checkSubqueries` ran the text on the tables and failed with `sqlite: Expression tree is too large (maximum depth 1000) (code 1)`, though the oracle said it parses. SQLite adds the heights of the two WHEREs when it resolves the names (`pParse->nHeight` in `sqlite3ResolveExprNames`); the shell put the most at 498. `recognizer.statementWhere` refuses more.
- `x = v` as a correlated case in the facts was refused by the pass as `column compared with a column`, which I had forgotten; the correlated cases are `select v from u` and `order by v` in the subquery.
- `make test` failed in `TestEquivalent` of `/internal/sql/mutation`: `no such mutant: stmt/recognize.go recognizer.conditions swap arguments: max(height, depth(ands[i].Y)) => max(depth(ands[i].Y), height)`. The entry now names the new text, `max(height, h)`.
- `make lint`: `QF1001: could apply De Morgan's law (staticcheck)` on the condition of that exception; rewritten.
- Bash commands that ran Python with a here-doc, or `sed` and then `git`, were refused as too complex for the worktree check; edit scripts written to the scratchpad with the Write tool and run as `python3 -I rep.py <file> <edits>` worked, and so did the Edit tool.

### What I learned

SQLite's errors for a subquery of the wrong width depend on the statement, the operator, and the side. And the limit of the depth of an expression is checked twice, at parse time per node and at name resolution over nested selects, so a statement can parse and still be too deep once its tables exist.

### What was tricky

- Which offset to give an error that SQLite raises after the parse. The pass gives the error of the depth at resolution at the subquery, with an explicit offset, once it has read the whole WHERE, since only then is the height of the WHERE known.
- The tables of the check of meaning: a subquery of the table of the statement must read that same table, so the tables are built by the fold of their names, with the columns of the statement and of its subqueries together.

### What warrants review

- `recognizer.subquery`, `recognizer.conditionValue`, `recognizer.statementWhere`, and the doc comments of `Term.Subquery` and `Select.Subqueries` in `/internal/sql/stmt/stmt.go`, the contract for the engine's builder.
- `checkSubqueries` in `/internal/sql/stmt/stmt_test.go`.
- The exceptions in `/internal/engine/differential_test.go` and the call of `engine.Check` in `/conn.go`.

### Future work

- The README of the pass for slice 15, by a writer: step 3.
- The engine and the driver of slice 15: remove `subqueryRefusal`, `subqueried`, and the exceptions; decide the open items in `/docs/todo/sql-front-end.md` (a correlated subquery, the row of a subquery without order, the argument of `limit ?` in a subquery); give SQLite's errors for `*` of more than one column.
- The simulator's box (`/internal/simulator/box.go`) sends an update or a delete with a subquery down its write path, which begins a write transaction before the engine refuses it, as slice 14's statement pass noted for RETURNING.
