# Diary: Slice 5 of the roadmap in the engine and the driver, several columns

Slice 5 adds tables of several columns, inserts of several values with an optional list of columns, and selects of named columns. The statement pass reads them since pull request #72 (`columns-stmt`); this task makes the engine and the `database/sql` driver run them end to end, removes the temporary refusal (`checkColumns`) and the `checkText` wrappers of the tests, and tests every point of the slice against SQLite. The work is on the branch `columns-engine`, from `origin/columns-stmt` at `f9cc7be`.

## Step 1: The engine runs several columns, in SQLite's order of errors

**Author:** columns-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "You build the engine and driver part of "Slice 5: several columns" [...] Make it work end to end, through the engine and through `database/sql` [...] Find the order in which SQLite gives these, among themselves and against the errors of slice 4 (type refusal, read-only transaction, too big, a key that exists), by running SQLite, and give the same order. [...] Remove what was temporary: `checkColumns` and the `checkText` wrappers, and the second parse in the driver's `check` if nothing needs it. [...] Keep the pinned numbers of the existing generated tests [...] true with the generator as it is [...]; where one moves, explain why."
**Interpretation:** Plan, build, and run every statement of slice 5 in the engine, give SQLite's errors in SQLite's order, keep slice 4's rule for the type, take the refusal and the wrappers out, and explain each pinned number that moves.
**Inferred intent:** The engine half of slice 5 with the same trust as slices 1 to 4, so that the next builder can set `Generator.Columns` in the generated tests and find only gaps of the tests, not of the engine.

### What I did

The order of the errors, read from `sqlite3Insert` and `sqlite3GenerateConstraintChecks` in `/internal/sqlite/c/sqlite3.c` and checked with the sqlite3 shell of 3.54.1 (`.dbconfig dqs_dml off`): at prepare, `no such table`, then `has no column named` for the columns of the list in their order, then the number of values (`table t has N columns but M values were supplied` without a list, `M values for N columns` with one); at bind, `string or blob too big`; at `OP_Transaction`, `attempt to write a readonly database`; then in the constraint checks `NOT NULL constraint failed` for the first column without a value in the order of the table (the key too, since a WITHOUT ROWID table marks its key NOT NULL), then `OP_TypeCheck` (where s4ql gives its "not supported" for the type, the first column of another type in the order of the table), then `UNIQUE constraint failed`, then a record that is too long. A select gives `no such table`, then `no such column: x` for the first unknown name of its list, or `no such column: "x" - should this be a string literal in single-quotes?` for a name in double quotes (`lookupName`, line 111287).

`/internal/engine/plan.go`: `NewPlan` takes every column and `s.Key()`; an insert holds all its values in one row of `Values`, with `Columns` nil without a list; `Project` has `Columns`, `Star` for `*`. `Format` prints `insert into t` without a list and `project v2, v1, v2`. `/internal/engine/operator.go`: `buildNode` resolves the names of a select after the scan; `buildSink` checks, after the table, `columnsError` (the pass's `CreateTable.ColumnsError`, built from the plan) for a create, and the columns of the list, the key of the table, and the number of values for an insert; `buildValues` places each value at the position of its column, with nil for a column left out; `insert.check` gives NOT NULL first and then the type, each in the order of the table; `insert.run` reads the key at its position. `/internal/engine/errors.go`: `noSuchResultColumn`, `valuesSupplied`, `valuesForColumns`, `notNullFailed`, and the new `sqlerr.CodeConstraintNotNull` (1299); `checkColumns` is gone. `/internal/engine/engine.go` and `/internal/engine/tx.go`: `checked` gives the result of a sink whose check failed, so that NOT NULL sets the count of changes to 0, as SQLite does at step, and the type's "not supported" leaves it. `/conn.go`: `check` calls `stmt.Check` alone again.

Tests: the `checkText` wrappers in `/internal/engine/differential_test.go` and `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go` are gone, their calls are `stmt.Check`; `messageKind` of both knows the new messages; the driver's excuse of an error that SQLite gives at prepare takes the new errors of the names (`prepareKinds`). `TestBuild` and `TestNewPlan` have the shapes of slice 5. Then the new tests: `TestEngine_columns` (13 shapes of two and three columns of mixed types, the key at each position, about 45 statements each, compared with SQLite: answer, column names and types, rows in order, count of changes, the tables, and the tables after a replay; plus a table of 2000 columns), 17 new pinned cases in `TestEngine_errors`, an annotation of several arguments and its replay, the order of the number of values, a too-long argument, NOT NULL, and a key that exists in `TestEngine_tooBig`, three cases of `TestConn_Begin` (write, roll back, read-only). In the driver: `TestColumns` in `/conn_test.go` (rows, `Rows.Columns`, `ColumnTypes`, prepared statements, annotations, a restart, every error with its codes and the count of changes, transactions), two refusals of `Prepare`, and in drivercheck four hand-written sequences and `TestColumns`, which runs the same 13 shapes through every call against mattn/go-sqlite3, pinned and through the pool.

### Why

The engine reproduces SQLite's order of checks on purpose, where the order is visible. NOT NULL is SQLite's error, given before SQLite looks at the types, so by slice 4's rule it comes before s4ql's "not supported" for the type.

### What worked

- Every new test passed against SQLite at its first run, the 17 pinned cases of `TestEngine_errors` included, which I wrote from the shell's answers.
- `go test -count=20` and `-race -count=20` of the new and changed tests of the engine and the driver, and `-count=20` and `-race -count=5` of drivercheck's `TestColumns` and `TestSequences`, passed. `make lint` and `make check-browser` passed.

### What didn't work

- A heredoc that appended the test to `/internal/engine/engine_test.go` with `cat >>` was refused: "this command is too complex to verify that it stays inside the worktree". The Edit tool worked.
- `sed -i '' 's/...\(want\|got\).../'` changed nothing: BSD sed has no `\|` in a basic expression. The Edit tool worked.
- `go test -count=1 .` failed with `TestOpen_realBucket: s3test: the tests against a real bucket cannot run: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are set neither in the environment nor in .../.env`, as it should without credentials; `make test` runs with `-short`.
- drivercheck's sequence with `select "x" from t3` failed: mattn's build reads `"x"` as a string, and gives the row `x`, where the pinned build of the engine's oracle has DQS=0. The step is out of the sequence, with a comment; the engine's tests compare it with the pinned build.
- drivercheck's sequence with `insert into k (b, a) values (3, ?)` and a wrong type failed: `typeRefusal` there knows only an insert of one value into the key. Out of the sequence; the root `TestColumns` checks such a refusal.

### What I learned

- `table T has no column named d` and `table T has 3 columns but 1 values were supplied` print the table's name as the statement writes it (its value), `NOT NULL constraint failed: t.c` as the table stores it.
- With a column left out and a value of another type in another column, SQLite gives NOT NULL: it tests every NOT NULL before its affinity and `OP_TypeCheck`.

### What was tricky

- NOT NULL is found in the sink's `check`, after the read-only refusal, and it is SQLite's error at step, while the type's refusal in the same `check` is s4ql's "not supported", which leaves the count. `checked` tells them apart by the type of the error.
- The pinned numbers moved only through the two texts of `stmttest.Others` that the engine refused and now runs, `insert into %v values (%v)` and `select %v from %v`: the engine's `excusedType` 363 to 371, and the driver's `excusedPrepare` 1010 to 1012, `excusedType` 173 to 180, `excusedRowsOpen` 37 to 38, `excusedOutside` 48 to 49. The generator draws exactly as before.

### What warrants review

- `buildSink` and `insert.check` in `/internal/engine/operator.go`: the order of the checks against the list above.
- `checked` in `/internal/engine/engine.go`, and its two callers.
- `shape.steps` in `/internal/engine/engine_test.go`: what each step means to reach.

### Future work

- For the builder of the generated checks: the helpers of the rule of the type, `typeRule`, `valueKind`, `insertType`, `oracle.column`, `pair.reach`, and `pair.checkLength` in `/internal/engine/differential_test.go`, and `typeRefusal`, `reach`, and `columnType` in drivercheck, read `Values[0]` and the key column; with `Generator.Columns` they must take every value and the first column of another type in the order of the table, after NOT NULL. The simulation's checks of the type use `typeRule` too. And a name in double quotes that is no column differs between mattn and the pinned build.
