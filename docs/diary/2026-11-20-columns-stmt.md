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

## Step 2: The pinned numbers, the documents, the limit of parameters, and the review

**Author:** columns-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "The pass's existing kinds of test, extended to the new forms [...] mutants and near misses with their pinned counts [...] Review your own work with the code-review skill before you report, and fix what it finds."
**Interpretation:** Explain and pin the numbers that moved, have a writer bring the README of the pass and `docs/todo.md` up to date, run every check, review the branch, and fix what matters.
**Inferred intent:** A branch whose numbers, documents, and code agree, ready for the lead.

### What I did

The pass's tests now set `Generator.Columns = 3`, so their statements are other texts and three pinned numbers moved. A throwaway test that grouped the excused texts by SQLite's message gave the reasons. `TestGenerate_mutants`: syntax errors where SQLite does not say 0 to 85 (83 mutants of a create table with a column named twice and a later syntax error, where SQLite stops at `duplicate column name`; 2 with `no such collation sequence`), not supported where SQLite does not say 343 to 310 (254 `unknown table option`, 51 `duplicate column name`, 3 collation, 2 AUTOINCREMENT). `TestGenerate_nearMisses`: 956 to 3,022, of which 2,080 are four new forms on each of the 520 create tables that give `table "t" has more than one primary key`. The corpus's excused numbers did not move; its supported texts went from 1,198 to 12,736, 11,950 of them INSERT, each checked in SQLite.

A writer sub-agent updated `/internal/sql/stmt/README.md`, `/docs/todo.md`, and a bullet of `/internal/engine/README.md`; I read its diff and corrected the todo's wording. It found that an insert can now hold more than 32,766 `?`, where SQLite gives `too many SQL variables` while it parses, whatever the database holds. `parameter` in `/internal/sql/stmt/recognize.go` now rejects the 32,767th as `parameter beyond SQLite's limit`, by the pass's rule for errors of grammar actions, and `TestCheck` checks 32,766 (supported, parses in SQLite) and 32,767 (not supported, SQLite stops early). It also found that a doc comment of `stmttest` said "one time in three" where the code gives NOT NULL on the key two times in three.

The review (`code-review high origin/main...columns-stmt`) gave five nits. Fixed: `insert` called `s.Parameters()` for each `?`, quadratic in a long row, and now keeps a count; the doc comment of `parameter`; the comment of `columnList`, which can write fewer names than values if the pool runs out. Documented: a create table with a column named twice and a second PRIMARY KEY is `second PRIMARY KEY`, where SQLite gives `duplicate column name`, by the same rule as `unknown datatype`. Left: `Prepare` parses a supported text twice, in `stmt.Check` and `engine.Check`, until the engine runs slice 5, as slice 4's pass did.

### Why

Every moved number is pinned only with its reason, as the README of the pass requires.

### What worked

`make test`, `make lint`, and `make check-browser` passed. `go test -count=1 ./internal/sql/... ./internal/engine/` and `cd internal/sqlite/crosscheck && go test -count=1 ./...` passed without `-short`, with the engine's and the driver's numbers unchanged. `go test -race -count=3 ./internal/sql/stmt/...` and `-count=20` of `TestCheck`, `TestCreateTable_Key`, `TestRecognize`, and `TestVocabulary` passed. Fuzzing found nothing: `FuzzGenerate` 664,827 executions in 60 s, `FuzzLiteral` 4,062,973 in 46 s, `FuzzCheck` 2,328,969 in 45 s. Coverage under `-short` is 98.8%. The test of `internal/sql/mutation` passed; no key of `equivalent.txt` moved.

### What didn't work

Nothing failed in this step beyond what the review found.

### What I learned

SQLite counts plain `?` in the grammar action of `expr ::= VARIABLE` (`sqlite3ExprAssignVarNumber`, line 114269 of `/internal/sqlite/c/sqlite3.c`), so `too many SQL variables` comes before any lookup of the table.

### What was tricky

The full run of the package now takes about 70 s, since the corpus has ten times the supported texts, each checked in SQLite.

### What warrants review

- `/internal/sql/stmt/README.md`, the sections the writer rewrote: CREATE TABLE, INSERT, SELECT, the errors of grammar actions, and "Results".
- The decision on `ColumnsError` in `/docs/todo.md`, for the lead to confirm.

### Future work

- Mutation testing and long fuzzing of the code of slice 5.
- The engine and the driver for slice 5, which remove `checkColumns`, the call in the driver's `check`, and the `checkText` wrappers.
