# Diary: Slice 8 of the roadmap in the statement pass, `update` and `delete`

Slice 8 adds `update t set a = <value>, b = <value>` and `delete from t`, each with or without the WHERE of slice 7. This task is the statement pass, `internal/sql/stmt`, and its generator `stmttest`; the engine and the driver do not run an update or a delete yet, and refuse both as not supported, with the error that the pass gave them before, until their builder comes. The work is on the branch `update-delete-stmt`, from `4b07614` on `main`.

## Step 1: SQLite's behaviour, the typed form, the recognizer, the checks, the generator, and the refusal in the engine

**Author:** update-delete-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Task: the statement pass of slice 8. Only the package `internal/sql/stmt` and its test helper `internal/sql/stmt/stmttest`. [...] 1. New statement kinds `stmt.Update` and `stmt.Delete`, shaped like their neighbours [...] Reuse the recognition of terms and of `where` that is there; do not duplicate it. 2. Parameters `?` are numbered in the order of the text, so those in `set` come before those in `where`, exactly as SQLite numbers them. [...] 3. The rule of the package holds without exception [...] Find out from SQLite itself [...] what it does with: the same column twice in `set`; a column in `set` or `where` that does not exist [...]; a qualified table name (`main.t`) [...]; `update t set a = 1 where` with nothing after; trailing semicolons and comments; keywords as names; `rowid` and its aliases as a column in `set`. [...] 4. `stmttest`: extend the `Generator` so that generated sequences contain `update` and `delete` statements [...] switched off by default if that is how the neighbours do it, so that the engine's generated checks, which cannot run these statements yet, keep passing. [...] 5. Tests in the style of the package [...]"
**Interpretation:** Recognize the two statements token for token with the code of insert and select, check every supported text against SQLite by running it on a table that tells the conditions and the assignments apart, generate both statements behind an option and two hooks that the engine's tests leave off, and make the engine and the driver refuse them exactly as before.
**Inferred intent:** The front half of slice 8 with the same trust as slices 1 to 7, so that the engine's builder only has to run the statements.

### What I did

First SQLite itself, with the pinned shell built from `/internal/sqlite/c/shell.c` into the scratchpad, and the vendored source (`sqlite3Update` in `/internal/sqlite/c/sqlite3.c`, around line 160760, and the grammar action of rule 159):

- The same column twice in SET: SQLite takes the last value (`aXRef[j] = i` in the loop over the assignments). The pass refuses it as `column named twice`, at the later name, as for INSERT.
- A missing table: `no such table: u` first. Then the columns of SET, in the order of the text: the first missing one gives `no such column: z` with the value of the name, also for a name in double quotes (no `- should this be a string literal` suffix, since this is `sqlite3ErrorMsg(pParse, "no such column: %s", pChanges->a[i].zEName)`, not the resolver). Only then the WHERE, by the rule of slice 7, with that suffix for a name in double quotes. `update t set z = 1 where y is null` gives `z`; `update t set b = 2 where y is null and x = 1` gives `y`.
- `rowid`, `ROWID`, `"rowid"`, `oid`, and `_rowid_` in SET give `no such column: rowid` (as written) in a WITHOUT ROWID table, since `sqlite3IsRowid` is only asked when the table has a rowid. The pass supports them as names, as it does in WHERE.
- `main.t` works in SQLite; `temp.t` and `foo.t` give `no such table: temp.t`. The pass refuses a schema as `schema name`, as for INSERT and SELECT, through `tableName`.
- `update t set a = 1 where` and `delete from t where` give `incomplete input`, and with a semicolon `near ";": syntax error`.
- ORDER BY and LIMIT on either statement are syntax errors in this build (no `SQLITE_ENABLE_UPDATE_DELETE_LIMIT`): `near "limit": syntax error`. The parser agrees.
- `set b == 'y'` is accepted: `==` is `TK_EQ` too. The pass takes it. `set b <> 'y'` and `set b = default` are syntax errors.
- `update t set 'b' = 1` is accepted by SQLite (a single-quoted name in `nm`); the pass refuses it as `single-quoted name`. `update t set true = 1` gives `no such column: true`: in SET, TRUE is a name.
- `too many columns in set list` for more than 2,000 assignments, raised in the grammar action while SQLite parses (`sqlite3ExprListCheckLength(pParse, ..., "set list")`). The pass refuses the 2,001st as `SET beyond SQLite's limit`.
- RETURNING, `update ... from`, `set (a, b) = (...)`, `update or ...`, an alias, INDEXED BY, NOT INDEXED, and WITH are all accepted by SQLite.
- Facts for the engine: `update t set b = 1` on a STRICT TEXT column stores the text `'1'`, so the exact types must refuse it; `update t set a = 2 where a = 1` with a key 2 gives `UNIQUE constraint failed: t.a` (1555); `update t set c = null` gives `NOT NULL constraint failed: t.c`; `changes()` counts every row that WHERE matches, also when the value does not change.

Then the code. `/internal/sql/stmt/stmt.go`: `stmt.Update{Span, Table, Set []Assignment, Where []Condition}`, `stmt.Assignment{Column Name, Value Term}`, `stmt.Delete{Span, Table, Where []Condition}`, with `Bounds`, `Parameters` (the assignments, then the conditions), and `setSpan`. `/internal/sql/stmt/recognize.go`: `update` and `delete`, which use `tableName`, `name`, and `term` as INSERT does, and `where`, the WHERE of SELECT taken out of `selectCmd`; `conditions` takes the number of parameters before it, so the `?` of WHERE follow those of SET. `/internal/sql/stmt/vocabulary.go`: `Update.Or` is `UPDATE OR`, `Assignment.LParen` is `SET of a list of columns`; the rest has the names of its nodes already: `RETURNING clause`, `FROM clause`, `alias`, `INDEXED BY or NOT INDEXED`, `WITH clause`.

The engine: `check` in `/internal/engine/engine.go` gives `&stmt.NotSupportedError{What: "UPDATE", Offset: s.Bounds().Start}` and the same for DELETE, the exact error that the pass gave before, so `engine.Check`, `Exec`, and `Query` answer as before. The driver's `check` in `/conn.go` calls `engine.Check` after `stmt.Check`, as it did in slice 7, so `Prepare` refuses at once. The generated tests of the engine and the driver failed then on the texts of the fuzz seeds and of `stmttest.Others` that the pass supports now: `"UPDATE techo SET a = 2": the engine does not support a statement that the pass supports: s4ql: not supported: UPDATE (at offset 0)`. So, as in `ff2bf40` for slice 7, `checkText` wrappers in `/internal/engine/differential_test.go` (used by `/internal/engine/simulation_test.go` too) and `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go` take the place of every `stmt.Check` there, and take a text that the engine refuses as not supported for one that is not supported. They go when the engine runs slice 8.

Tests: `checkChange` in `/internal/sql/stmt/stmt_test.go` runs src and the canonical text on an `anyTable` (the table of `checkWhere`, taken out into a type with `create`, `rows`, `refuses`, `noSuchColumn`, and `checkMissing`), and compares the rows that are left and `changes()`; it checks that each row is the old row or the old row with the assignments, that `changes()` lies between the rows that differ and those that have the assigned values, `no such table`, `no such column` for the first and the last assigned column, and the missing columns of WHERE after the assigned ones. `checkPositions` checks the spans and the numbers of the `?` across SET and WHERE (`checkTerm`, `checkConditions`, `checkOnce`, shared with INSERT and SELECT). About 120 cases in `/internal/sql/stmt/check_test.go`, three subtests (keywords as names in the five new places, the 2,000 assignments, and the depth of WHERE in both statements), 105 near misses, `difference` for both, `FuzzLiteral` for SET and the WHERE of DELETE, and the allow-list.

`/internal/sql/stmt/stmttest/stmttest.go`: positions `UpdateTable`, `UpdateColumn`, and `DeleteTable`; kinds `KindUpdate` and `KindDelete`; `Generator.Changes` (one statement of a kind at random in Changes is an update or a delete), and the hooks `Generator.Update` (assignments and conditions) and `Generator.Delete` (conditions). The writing of WHERE is `Generator.conditions`, shared by the three statements. `Sequence` takes the names of its tables and columns from the new positions too; their pools are the same as those of INSERT. The pass's tests set `Changes = 3`.

### Why

The brief's list of what stays not supported, each under a name; SQLite's behaviour decided the details that the roadmap leaves open.

### What worked

- A dump of 51,000 texts, statements, and arguments of `Generator` (four settings of Columns, Nulls, and Conditions) and `Sequence` from 30 seeds, written before the change of the generator and after it, was byte for byte the same (`cmp old.txt new.txt && echo SAME`), also after `Sequence` took the new positions into its common pools.
- Planted mutants in the recognizer each failed `verify` at once, without the pinned statements: the `?` of WHERE numbered from 1 again, and the last assignment dropped (`sqlite: no such column: b`).
- The corpus has 1,124 supported texts more, 534 deletes and 590 updates, and its pinned numbers did not move.

### What didn't work

- A Bash heredoc with a Python script that held backticks was refused: "This agent is isolated in the worktree [...] but this command is too complex to verify that it stays inside the worktree." Scripts written with the Write tool to the scratchpad worked.
- `git checkout internal/sql/stmt/recognize.go`, to undo a planted mutant, was refused by the permission system ("Destructive or Unwanted Actions"); it would have thrown away the uncommitted recognizer too. I undid the mutant with `sed` on the one line.
- `TestCheck` panicked on `update t set (v, w) = (1, 2)`: `name` reads the text of an absent token. `update` now calls `unexpected` when the assignment has no column, which names the parenthesis.
- `TestGenerate` failed on an update whose condition reused the assigned column `CURRENT_DATE`: a name after SET and a value in WHERE. A condition at random reuses only the assigned columns that are in the pool of `SelectColumn`.

### What I learned

- SQLite resolves the columns of SET in `sqlite3Update` itself, before the WHERE, and prints their value without the suffix about single quotes that the resolver adds for a name in double quotes.
- The pools of names of the new positions are those of INSERT exactly, and the keywords that SQLite reads as a column in the WHERE of an update or a delete are those of SELECT.

### What was tricky

- The moved pinned numbers. `TestGenerate_mutants` went from 54 to 64 syntax errors and from 284 to 196 not supported where SQLite does not say, and `TestGenerate_nearMisses` from 3,003 to 2,054. A throwaway test grouped the excused texts by SQLite's message: all are of CREATE TABLE (`duplicate column name`, `unknown table option`, `no such collation sequence`, and in the near misses `table "t" has more than one primary key` and the two others of before), none of an update or a delete. They moved because a third of the statements are updates and deletes now, so 349 of the 2,000 near-miss statements are CREATE TABLE where 515 were.

### What warrants review

- `update` and `delete` in `/internal/sql/stmt/recognize.go`, and the names and offsets of the new rejects.
- `checkChange` and `anyTable` in `/internal/sql/stmt/stmt_test.go`, and that `checkWhere` checks what it did.
- `Generator.update` and `Generator.conditions` in `/internal/sql/stmt/stmttest/stmttest.go`.

### Future work

- The README of the pass, `docs/todo.md`, and the decisions: step 2.
- The engine and the driver for slice 8, which remove the refusal in `check` and the call of `engine.Check` in the driver's `check`.

## Step 2: The documents, the review, and the checks

**Author:** update-delete-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Where SQLite's behaviour is a detail that the roadmap does not settle, decide by SQLite's behaviour and the agreed rules, and record the decision in `docs/todo.md` in a group \"decisions of the statement pass of slice 8, for the lead to confirm\" [...] Finish: `make test`, `make lint`, `make check-browser` pass [...] Self-review the diff against these requirements."
**Interpretation:** Record the decisions and SQLite's facts in the todo, bring the README of the pass up to date, review the branch with the code-review skill, fix what matters, and run every check.
**Inferred intent:** A branch whose code, numbers, and documents agree, ready for the lead and for the builder of the engine.

### What I did

`/docs/todo.md`: the group "Decisions of the statement pass of slice 8", and two items for fresh builders: the engine and the driver for slice 8, and mutation testing and long fuzz runs of the code of slice 8 in the pass. A writer sub-agent updated `/internal/sql/stmt/README.md`: the typed form, sections UPDATE and DELETE, the errors of grammar actions, "How it is tested", and "Results" with the numbers of the full run of step 1. I read its diff. Its breakdown of the excused near misses is more exact than my brief: 2,054 = 1,396 `table "t" has more than one primary key` + 349 `generated columns cannot be part of the PRIMARY KEY` + 291 `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY` + 18 `non-deterministic functions prohibited in index expressions`, all of CREATE TABLE.

`checkChange` gained a check of the order of SQLite's errors: on a table without the last assigned column and without every column of the conditions, SQLite names the assigned column.

The review (`code-review high 4b07614...HEAD`) found no bug and four nits. Fixed: the case "syntax error in a statement that is not supported" used an update, which the pass supports now, and uses `create view v as select 1 + where`; the todo said that the engine answers every update and delete as before, which holds only for those that the pass supports (`update t set v = 1 returning *` gives `RETURNING clause (at offset 19)` where it gave `UPDATE (at offset 0)`); `FuzzLiteral` shadowed its `d *db` with a `*stmt.Delete`. Left: the driver's `check` parses a supported text twice, through `engine.Check`, until the engine runs slice 8, as in slice 7.

### Why

The decisions are the lead's to confirm, and a moved number or a changed message needs its reason written down.

### What worked

`make test`, `make lint`, and `make check-browser` passed. `go test -asan ./internal/sql/stmt/...` passed in a `golang:1.27.3` container, since the fuzz targets `FuzzLiteral` and `FuzzGenerate` changed. `-race -count=10` of `TestCheck`, `TestGenerator_*`, `TestStatement`, and `TestAllowList`, and `-count=50` of `TestGenerator_*`, passed. `golangci-lint run ./internal/sql/stmt/... ./internal/engine/ .` gave 0 issues. The engine's `TestDifferential` and `TestSimulation` without `-short` passed with every pinned number unchanged, and so did `drivercheck`. Coverage of the pass under `-short` is 99.4%, with `update` and `delete` covered completely.

### What didn't work

Nothing failed in this step.

### What I learned

The roadmap's slice 8 lists ORDER BY and LIMIT on an update and a delete as "Rejected as not supported, where SQLite would accept it", but the pinned build of SQLite refuses both with a syntax error. The pass gives the syntax error; the todo says so, and the roadmap's text is the lead's.

### What was tricky

Nothing beyond step 1.

### What warrants review

- The group "Decisions of the statement pass of slice 8" in `/docs/todo.md`, for the lead to confirm.
- The sections UPDATE and DELETE, "How it is tested", and "Results" of `/internal/sql/stmt/README.md`.

### Future work

- The engine and the driver for slice 8; the todo has the list of what to remove and add.
- Mutation testing and long fuzz runs of the code of slice 8 in the pass.
- The roadmap's slice 8 is not marked built; the status of the README, the site, and the todo are for the lead's sub-agent after the merge.
