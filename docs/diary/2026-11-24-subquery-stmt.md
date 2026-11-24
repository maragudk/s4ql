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
