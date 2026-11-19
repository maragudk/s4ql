# Diary: Slice 5 of the roadmap in the statement pass, several columns

Slice 5 adds tables of several columns, inserts of several values with an optional list of columns, and selects of named columns. This task is the statement pass, `internal/sql/stmt`, and its generator `stmttest`; the engine and the driver keep running one column, and refuse the new forms as not supported until their builder comes. The work is on the branch `columns-stmt`, from `origin/main` at `f842ffb` (pull request #71, the generated checks of slice 4, merged while this task started; its branch `types-checks` had the same tree).

## Step 1: The typed form, the recognizer, the generator, and the refusal in the engine

**Author:** columns-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "The statement pass (`internal/sql/stmt`) reads the statements of slice 5. [...] `create table` with one or more columns, each of one of the five types, `strict, without rowid`. Exactly one column is the primary key [...] A column that is not the primary key and has no `not null` is rejected as not supported [...] `insert into t values (...)` with one or more values, and `insert into t (c1, c2) values (...)` [...] A column named twice in the list is rejected as not supported [...] `select c1, c2 from t` [...] and `select * from t`. [...] What the pass cannot decide without the table stays with the engine [...] What SQLite decides from the text alone on an empty database, such as a column named twice in a `create table` or more columns than SQLite allows, follows the pass's existing rule for such errors. [...] the engine refuses a statement of the new forms that it cannot run yet [...] in `engine.Check` too, so that `Prepare` in the driver refuses at once."
**Interpretation:** Extend the typed statements and the recognizer to the forms of slice 5, test each form against SQLite as the pass's tests do, extend `stmttest` without changing what the engine's and the driver's tests draw, and make the engine and the driver refuse the new forms at the check of the text.
**Inferred intent:** The front half of slice 5 with the same trust as slices 1 to 4, so that the engine's builder can build on a typed form whose every field is checked against SQLite.

### What I did

`/internal/sql/stmt/stmt.go`: `CreateTable.Columns []Column` (`Name`, `Type`, `PrimaryKey`, `NotNull`) with `Key()` and `ColumnsError()`; `Insert.Columns []Name` (nil without a list) and `Insert.Values []Term` (`Value`, `Parameter`, `Span`); `Select.Columns []Name` (nil for `*`). `/internal/sql/stmt/recognize.go`: `createTable` loops over the columns and takes the commas; `column` takes PRIMARY KEY and NOT NULL in any order, each once; `insert` takes an optional `IDList` and every value of the row, numbering the `?` from 1; `selectCmd` takes `*` alone or bare names through `resultColumn`. New names of rejects: `column without PRIMARY KEY or NOT NULL`, `column without NOT NULL`, `second PRIMARY KEY`, `table without PRIMARY KEY`, `column named twice`, `* and another result column`, `result column other than a column or *`; `ResultColumn.As` and `.Alias` are `alias`. Gone: `second column`, `column without PRIMARY KEY`, `INSERT without a column list`, `result column other than *`, and the field names `second value` and `second result column`, which no text reaches now.

`/internal/sql/stmt/stmttest/stmttest.go`: `Generator.Columns`; with 0 the generator draws exactly as before (checked, see below), with more it writes the forms of slice 5. A new position `SelectColumn` with its pool, whose keywords are those for which SQLite gives `no such column: kw` on a table without such a column.

The engine: `checkColumns` in `/internal/engine/errors.go` refuses a table of more than one column, NOT NULL, an insert without a list or of more than one column or value, and a select of named columns, with `s4ql: not supported: <what>, which the engine does not run yet`. `engine.Check` and `prepare` call it; `NewPlan` panics for those forms. The driver's `check` in `/conn.go` calls `engine.Check` for a supported statement, as slice 4's pass did for a while. The tests of the engine and the driver read texts through a `checkText` that takes such a refusal for "not supported".

Tests: `checkMeaning` in `/internal/sql/stmt/stmt_test.go` is split into `checkCreateTable`, `checkInsert`, `checkSelect`; about 70 hand-written cases in `/internal/sql/stmt/check_test.go`, among them tables of 2000 and 2001 columns; about 60 new near misses in `/internal/sql/stmt/generate_test.go`; the allow-list in `/internal/sql/stmt/vocabulary_test.go` gains `NullConstraint`, `ColumnRef`, and the commas; a test that every keyword SQLite reads as a column name in the select list is supported, and no other.

### Why

A column named twice in a CREATE TABLE and more than 2000 columns are errors of SQLite's grammar actions, raised in `sqlite3AddColumn` after `sqlite3StartTable` has looked for the table. With a table of the name, SQLite gives `table t already exists` instead (checked with the sqlite3 shell of 3.54.1). So whether SQLite gives `duplicate column name` depends on the database, and the spec only accepts a difference from SQLite for syntax errors. The pass therefore supports such a statement and gives the error in `ColumnsError()`, for the engine to give after its check of the table; the tests check `ColumnsError()` against SQLite on a database with and without the table. Two primary keys and no primary key stay "not supported", by the pass's rule for errors of SQLite that come with a construct the slice rejects.

### What worked

- A dump of 27,000 texts and arguments of `Generator` and `Sequence` from 30 seeds, written once on `205a1ed` and once on the new code through a temporary WIP commit, was byte for byte the same (`cmp ... && echo SAME`), so the engine's and the driver's generated tests draw the same calls.
- `go test -count=1 ./internal/engine/` and `cd internal/sqlite/crosscheck && go test -count=1 ./...` passed with their pinned numbers unchanged, before any test of the pass compiled.
- `TestCheck` passed against SQLite for every new form at the first run after the expected answers were set.

### What didn't work

- Bash with Python heredocs that edited files, and a long chain of `head`/`cat` into a file, were refused: "this command is too complex to verify that it stays inside the worktree". The Edit tool, `perl -pi -e`, and `sed -i ''` worked.
- `git checkout origin/types-checks -- internal/sql/stmt` failed with `fatal: invalid reference: origin/types-checks`: the branch had been deleted when #71 merged. `205a1ed` and `origin/main` have the same tree, so I used the commit and then moved the branch onto `origin/main` with `git reset --soft origin/main`.
- `TestRecognize/should_pass_on_a_panic_that_is_not_its_own` failed with `no panic`: its tree, a CREATE TABLE without columns, panicked on `c.Columns[0]`, which the loop no longer reads. It now uses an INSERT of two columns without the comma.
- `TestCheck` failed for `select "a b", A, a from t` with `SQLite gives sqlite: no such column: "a b" - should this be a string literal in single-quotes? (code 1), want 1 "no such column: a b"`: SQLite's message for a name in double quotes differs (`lookupName`, line 111287 of `/internal/sqlite/c/sqlite3.c`). The check expects it now.
- The pinned numbers moved; see step 2.

### What I learned

- SQLite names a result column of a plain column reference by the column's name as the table stores it, not as the select writes it: `select B from t` with a column `b` gives the name `b`.
- `table t has 2 columns but 1 values were supplied`, `has no column named`, and `no such column` print the table and column names as the statement writes them; `UNIQUE constraint failed` as the table stores them.
- `insert into t (a, b, a) values (3, 'z', 4)` succeeds in SQLite with `a` = 3.
- `select true from t` gives 1 where `t` has no column `true`; `select "true" from t` is a column reference.

### What was tricky

- The guards in `createTable` and `column`: a reject such as `second PRIMARY KEY` or `table constraint` must only be given when its token is next, or a token that an earlier node left untaken would get the wrong name (`create table t (a int primary key not null on conflict abort, b int primary key)` must say `ON CONFLICT clause`).
- Others of `stmttest.Sequence` holds `insert into %v values (%v)` and `select %v from %v`, which the pass now supports and the engine refuses; the tests of the engine and the driver had to take the engine's refusal for "not supported" to keep their numbers.

### What warrants review

- `ColumnsError` in `/internal/sql/stmt/stmt.go` against `sqlite3AddColumn`: the check of the number comes before that of the name, and the name printed is the later column's value.
- The decision above, that a column named twice is supported with an error for the engine, not rejected by the pass.
- `checkInsert` and `checkSelect` in `/internal/sql/stmt/stmt_test.go`: the tables they build and what they expect.

### Future work

- The engine and the driver for slice 5: then `checkColumns` and the `checkText` wrappers go, and `stmttest`'s `Columns` is set in their generated tests.
