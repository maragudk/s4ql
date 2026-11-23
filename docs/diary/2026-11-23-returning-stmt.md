# Diary: Slice 14 of the roadmap in the statement pass, `returning` on `insert`, `update`, and `delete`

Slice 14 lets an insert, an update, and a delete end with RETURNING and a list of column names or `*`, because goqite's send is `insert into goqite (queue, body, timeout, priority) values (?, ?, ?, ?) returning id`. This task is the SQL front end: the tokenizer and the parser already read RETURNING, so the work is the statement pass, `internal/sql/stmt`, and its generator `stmttest`. The engine refuses such a statement as not supported, with the error that the pass gave it before, until its builder comes. The work is on the branch `worktree-agent-a1197f7b295299ade`, from `9238075` on `main`.

## Step 1: SQLite's behaviour, the field, the recognizer, the checks, the generator, and the refusal in the engine

**Author:** returning-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Markus (the product owner) agreed on 2026-11-23 that slice 14 is `returning` on `insert`, `update`, and `delete`, with named columns and `*`. The exchange, for your diary's prompt context, verbatim: I proposed "I propose `returning` as slice 14, on `insert`, `update`, and `delete`, with named columns and `*`. [...] Shall I write slice 14 as `returning` into the roadmap and start the builder?" and Markus answered "yes". [...] Your task is the SQL front end: the tokenizer, the parser, and the pass after it (`internal/sql/...`, mainly `internal/sql/stmt`), so that a statement with `returning` comes out of the pass in the form that the engine will run, with the returned columns resolved against the table. [...] make every layer above the pass refuse a statement with `returning` with the project's "not supported" error until then, so that nothing runs it half-way. Do not drop `returning` silently anywhere."
**Interpretation:** Read RETURNING into the typed statements in a shape that the engine can run, support in it exactly what the select list supports, check every supported text against SQLite, generate the new form behind an option, and refuse it above the pass until the engine runs it.
**Inferred intent:** The front half of slice 14 with the trust of the earlier slices, so that the engine's builder has the field, the facts of SQLite, and the checks ready, and only has to remove a refusal.

### What I did

SQLite first, with the shell built from `/internal/sqlite/c/shell.c`, `/internal/sqlite/c/sqlite3.c`, and `/internal/sqlite/options.h` into the scratchpad (`cc -O1 -include options.h ...`), and SQL scripts written there with the Write tool: the names of the result columns, a column twice, a missing column and the order of the errors in each statement, `t.v`, `t.*`, `main.t.v`, an empty list, RETURNING on other statements, `*` with a column, `rowid`, `true`, keywords, failures of constraints, `query_only`, negative zero, and the limit of 2,000 result columns. The results are in `/docs/todo/slice-14.md`.

`/internal/sql/stmt/stmt.go`: the type `Returning`, with `Span` and `Columns`, nil for `*`, and `Returning.Present`, and a field `Returning` on `Insert`, `Update`, and `Delete`. `/internal/sql/stmt/recognize.go`: `recognizer.returning`, called at the end of `insert`, `update`, and `delete`, which reads each column with `recognizer.resultColumn`, the code of the select list. No new name in the vocabulary: every refusal inside RETURNING has the name that the select list gives.

Tests. `/internal/sql/stmt/check_test.go`: a section of slice 14 of 63 cases, six of them the old cases that refused RETURNING, which are supported now or refuse a column of it (16 supported: the three statements of the roadmap, goqite's send, `*`, a column three times, a column that the insert does not list, `"*"` as a name, negative zero, keywords and `rowid`, comments in every gap; 33 not supported; 14 syntax errors); two subtests, the keywords that SQLite reads as a column in RETURNING in four places (84, those of the select list and of the pool of `SelectColumn`), and the order of SQLite's errors for 20 texts. `/internal/sql/stmt/stmt_test.go`: `canonical` writes RETURNING (`canonicalReturning`); `checkPositions` checks the span and the columns of RETURNING (`checkReturning`) and `Parameters` of an insert, an update, and a delete; `checkInsert` gives the table of the check the columns of RETURNING, by name without a list when they fit and as columns with a DEFAULT of their own after those of the list, and calls `checkInsertReturning` (the row that the text gives is what a select of the same columns reads after it, and the canonical text gives the same) and `checkReturningMissing` (`no such column` for the first column of RETURNING that a table lacks, after the errors of the list); `checkChange` adds the columns of RETURNING to its table, calls `checkChangeReturning` (a row for each row that the WHERE chooses, as many as `changes()`, with the values after an update and before a delete, as a select of the chosen keys reads them), and requires `no such column` for the first column of RETURNING that neither SET nor WHERE names. `returningReads` counts the updates and deletes with RETURNING by the rows they gave. `/internal/sql/stmt/vocabulary_test.go`: the allow list has the node `Returning` and its fields. `/internal/sql/stmt/generate_test.go`: `newGenerator` sets `Returning` to 3, `difference` tells RETURNING apart, and the six near misses with RETURNING that are supported now gave way to 30 near misses of RETURNING.

`/internal/sql/stmt/stmttest/stmttest.go`: `Generator.Returning` and `Generator.returning`. `TestGenerator_Returning` in `/internal/sql/stmt/stmttest/stmttest_test.go`.

Above the pass: `returningRefusal` in `/internal/engine/returning.go`, called once in `check` in `/internal/engine/engine.go`, gives `RETURNING clause` at the RETURNING; `TestCheck_returning` and `TestConn_returning` in `/internal/engine/returning_test.go` pin it, and `returned` there is the exception of `pair.step` in `/internal/engine/differential_test.go`. The driver's `check` in `/conn.go` now calls `engine.Check` after `stmt.Check`, so `Prepare` refuses such a text too; `TestPrepare` in `/conn_test.go` pins it. Notes in `/docs/todo/slice-14.md`, its line in `/docs/todo.md`, and two open items in `/docs/todo/sql-front-end.md`.

### Why

RETURNING takes a select list in SQLite's grammar (`RETURNING selcollist`), so reading it with the code of the select list makes the two agree by construction, and the brief's rule "treat `returning` the same as `select`" holds without a second set of decisions. The shape follows `Select.Columns` for the columns and `Select.Limit` for the absence: no pointer, the zero value for none, and a method that says whether there is one. The span is kept, unlike the offsets of ORDER BY and LIMIT, because `*` has no name with an offset, and the refusal of the engine and any later message need the place of the clause. A column twice stays supported, unlike in the lists of slices 5, 8, 9, and 13, because a later column of RETURNING is a column of the result, as in a select list, and does not go unused.

The check of the meaning compares each text with SQLite, and against something that does not depend on the pass: a select of the same columns after an insert, and a select of the chosen keys after an update or before a delete. The canonical text alone would agree with a text also where both are wrong.

### What worked

- Every new case of the typed form was right on its first run but one offset, which I had counted wrong (`returning *; select 1`: 27, not 26).
- With `Returning` 0 the generator is unchanged: a dump of 32,400 lines (`%#v` of `Generator.Statement` and `Generator.CallOf` of every kind in three settings, one with hooks, and of `Sequence.Next`, over 30 seeds), written with the generator before the change and after it, was the same (`cmp before.txt after.txt && echo SAME`).
- Two planted bugs: swapping the first two columns of RETURNING in the recognizer failed `TestGenerate` and the positions check, and giving the second column the value of the first failed `TestCheck_corpus` in `checkInsertReturning`, on the corpus entry `INSERT INTO t1(b,c) VALUES(5,99) RETURNING b,c,a,rowid`: `Expected "[B B A ROWID] integer 5; integer 5; ..." but got "[B C A ROWID] integer 5; integer 99; ..."`. Both reverted.
- `go test -count=1 ./internal/sql/stmt/...` passed, and `go test -short ./...` passed.

### What didn't work

- `TestGenerate_mutants` and `TestGenerate_nearMisses` failed on their pinned numbers: "45 texts are syntax errors where SQLite does not say [...] but the test pins 41", "167 [...] pins 131", and "1983 texts are not supported where SQLite does not say [...] but the test pins 2089". With `Returning` 0 they passed with the old pins, and a throwaway tally by SQLite's message, hooked into `verify` and reverted with `git checkout`, found only errors of grammar actions in create tables and two selects with `a JOIN clause is required before ON`, and no excused text with RETURNING. They are pinned at the new values.
- My first `TestConn_returning` expected the row `(1, 'a')`; the test helpers print it as `1,"a"`.
- Bash here-docs that ran Python with `git` or `go` in the same command were refused: "this command is too complex to verify that it stays inside the worktree". Scripts written to the scratchpad with the Write tool and run as `python3 -I script.py <file>` worked. A Python triple-quoted string turned `"\n"` of Go code into a real newline once: `stmt_test.go:1796:29: string literal not terminated`; fixed by a second script.

### What I learned

SQLite names a result column of RETURNING by the column as the table stores it, as in a select: `returning k` on a table with a column `K` gives `K`. It resolves RETURNING after the whole rest of the statement, so even a missing column under IS NULL in WHERE, which does not stop the walk over WHERE, gives its error before RETURNING. It takes `t.v` in RETURNING but refuses `t.*` with `RETURNING may not use "TABLE.*" wildcards`. A select, or a RETURNING, of more than 2,000 columns gives `too many columns in result set`, and s4ql runs such a select today: a throwaway test of the engine gave the rows where SQLite gives the error.

### What was tricky

- The table of the check of an insert must hold every column of RETURNING, or SQLite gives `no such column` before every check of the values. Without a list, the table has exactly as many columns as values, so the columns of RETURNING take their places only when they are not more; otherwise the check of the values is left out and `checkReturningMissing` checks the error.
- The keys that an update chooses must be read before the update, since the update can change what its WHERE tests, and the arguments of the WHERE in a select of its own are those after the arguments of SET.

### What warrants review

- `recognizer.returning` in `/internal/sql/stmt/recognize.go`, and the doc comments of `stmt.Returning` and of the three fields in `/internal/sql/stmt/stmt.go`, which are the contract for the engine's builder.
- `checkInsert`, `checkInsertReturning`, `checkReturningMissing`, `checkChange`, and `checkChangeReturning` in `/internal/sql/stmt/stmt_test.go`.
- The changes outside the pass: `/internal/engine/returning.go`, the exception in `/internal/engine/differential_test.go`, and the call of `engine.Check` in `check` of `/conn.go`, which parses the text a second time when a statement is prepared.

### Future work

- The README of the pass for slice 14: step 2.
- The engine and the driver for slice 14: remove `returningRefusal`, `returned`, and the exception, decide the driver's `check`, add the hook of the generator with the area and the profile of the generated checks, and give `too many columns in result set` for a select and a RETURNING of more than 2,000 columns.
- Mutation testing and long fuzz runs of `recognizer.returning` and `Generator.returning`.

## Step 2: The README, the review, the fixes it found, and the checks

**Author:** returning-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Run `make test` before you finish (and `make lint` if the Makefile has it). [...] Review your own work when the code is done, and fix what you find."
**Interpretation:** Bring the README of the pass up to slice 14, review the branch with competing reviewers, fix what matters, and run the checks of the project.
**Inferred intent:** A branch whose code, documents, and numbers agree, ready for the lead and for the builder of the engine.

### What I did

A writer sub-agent, in a worktree of its own, updated `/internal/sql/stmt/README.md` (the typed form, a section `### RETURNING`, the grammar of INSERT, UPDATE, and DELETE, "How it is tested", and "Results" from a full `-v` run); I cherry-picked its commit. It found six things in my notes and code, and I fixed five: the near misses that RETURNING made supported are six, not seven; the excused selects with ON are two; the corpus kept its excused numbers but its supported texts rose by 22; `*` on a wide table cannot pass 2,000 columns, since a table has at most 2,000 and the pass takes no `*` with another column; and `returningReads` was counted and never read, so it now counts only texts with WHERE and `TestGenerate` logs it and requires that more give some rows than none (seed 0: 346 none, 498 some, 0 every row). It also found two sentences of slice 13 in the README that said the engine still refuses ORDER BY and LIMIT and that `Sequence` writes none; both now say what the code does.

The review (`code-review high 9238075...HEAD`) gave five points. Fixed: `pair.step` in `/internal/engine/differential_test.go` checked an insert or an update with RETURNING against the rule of the type and of the length of its values, which applies now that the pass takes such a text, so FuzzSQL could fail on `insert into t1 (v1) values ('x') returning v1` (the reviewer confirmed it with a throwaway test, and `TestPair_returning` in `/internal/engine/returning_test.go` reproduced it before the fix: "the engine gives not supported ("s4ql: not supported: RETURNING clause (at offset 33)"), and the rule "s4ql: not supported: a string literal for the column t1.v1 [...]""); a statement for which `returned` holds now skips both rules. Fixed: `engine.NewPlan` panics for a statement with RETURNING, so that no path that skips `engine.check` runs its change and drops its rows. Fixed by the writer: the README. Left for the lead: the simulator's box (`/internal/simulator/box.go`) sends a text to the path of a write when the pass takes it, so a statement with RETURNING now begins a write transaction before the engine refuses it, and its event says "abandoned" where it said "failed"; the brief keeps `internal/simulator` out of this step. Left: the driver's `check` parses the text twice when a statement is prepared, once in `stmt.Check` and once in `engine.Check`; it keeps the refusal in one place, and the engine's builder removes it with the refusal.

### Why

The rules of the differential tests describe what s4ql does with a statement that it runs, and until the engine runs RETURNING, the refusal of the text comes before any value, so the rules do not hold for it.

### What worked

`make test` passed (also the module `internal/sqlite/crosscheck`, whose driver check runs in full), `make lint` gave 0 issues in each of its six runs, and `make test-sql`, the full run of `./internal/sql/...` and `./internal/engine/...` that checks the counts of `testdata/reached/`, passed with no `.tsv` file changed: `Sequence` writes no RETURNING. `FuzzCheck`, `FuzzGenerate`, and `FuzzSQL` ran 30 s each and found nothing.

### What didn't work

Nothing beyond the failure of `TestPair_returning` before its fix, quoted above.

### What I learned

The rules of the differential tests read a text through `stmt.Check` on their own, so every form that the pass takes before the engine runs it needs its exception there too, not only in the branch of "not supported".

### What was tricky

Nothing beyond step 1.

### What warrants review

- The skip of the rules in `pair.step` and the panic of `engine.NewPlan`, both of which the engine's builder removes.
- The section `### RETURNING` and "Results" of `/internal/sql/stmt/README.md`.

### Future work

- The simulator's box and RETURNING, for the lead, until the engine runs it.
- The engine and the driver for slice 14, as step 1 lists, and with them the skip in `pair.step` and the panic of `NewPlan`.
