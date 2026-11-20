# Diary: Slice 10 of the roadmap in the statement pass, `if not exists` and `if exists`

Slice 10 adds `create table if not exists t (...)`, `drop table if exists t`, `create index if not exists i on t (...)`, and `drop index if exists i`. When the object is not there (for a create) or is there (for a drop), the statement does what the plain form does; otherwise it does nothing, gives no error, and writes nothing to the log. The reason is glue's migration tool, which creates its migrations table with `create table if not exists`. This task is the statement pass, `internal/sql/stmt`, and its generator `stmttest`; the engine and the driver do not run the clauses yet, and refuse them as not supported, with the error that the pass gave them before, until their builder comes. The work is on the branch `if-exists-stmt`, from `359dcef` on `main`.

## Step 1: SQLite's behaviour, the fields, the recognizer, the checks, the generator, and the refusal in the engine

**Author:** if-exists-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Task: the statement pass of slice 10. Only `internal/sql/stmt` and its test helper `internal/sql/stmt/stmttest`. Not the engine, the driver, or core [...] 1. The four statement kinds [...] carry whether the clause was written (a field each, named alike, for example `IfNotExists` and `IfExists`) [...] find out from SQLite what it stores in `sqlite_schema` for a statement written with `if not exists` [...] 2. The rule of the package holds without exception [...] Find out from SQLite itself, not from memory: the clause in the wrong place [...], half a clause [...], the clause with `temp`, with a schema (`main.t`), with keywords as names (a table named `if`, `not`, `exists`), twice. 3. For the engine builder after you, measure in SQLite and record in the diary and in `docs/todo.md` [...] 4. `stmttest`: the `Generator` writes the clauses, valid and invalid, behind an option that is off by default [...] show that with the option off the generator's output is byte-identical to before [...] 5. [...] make the engine answer 'not supported' for a statement that carries a clause, at one place, with a test that pins it, in the manner the statement pass of indexes did with `indexRefusal`; nothing more in the engine."
**Interpretation:** Take the clauses token for token in the four supported statements, keep every other refusal, find each fact of SQLite with the pinned shell and pin it in tests, generate the clauses behind `Generator.Exists`, and refuse them in the engine with the old error so that the engine and the driver answer as before.
**Inferred intent:** The front half of slice 10 with the same trust as slices 1 to 9, and the facts that the engine's builder needs to decide the order of its errors.

### What I did

SQLite first, with the shell built from `internal/sqlite/c/shell.c` and `options.h` into the scratchpad, and the vendored source (`sqlite3StartTable`, `sqlite3CreateIndex`, `sqlite3DropTable`, `sqlite3DropIndex`, `sqlite3ForceNotReadOnly`):

- SQLite keeps the text from the name on, so `sqlite_schema` never has the clause: `create TABLE IF /* x */ not EXISTS "t2" (...)` is kept as `CREATE TABLE "t2" (...)`, and `CREATE INDEX If Not Exists /*c*/ i2 on t (v desc)` as `CREATE INDEX i2 on t (v desc)`. The existing `SQL` of both statements is already that text, since it starts at the name.
- `create table if not exists if (...)` parses, and then SQLite cannot parse its own kept text `CREATE TABLE if (...`: `malformed database schema (if) - near "(": syntax error`, and nothing is created. The same for an index (`near "on"`). The plain `create table if (...)` is `near "(": syntax error`, since IF starts the clause there.
- With the object there, the clause stops SQLite before the rest of the declaration: no `duplicate column name`, `too many columns on t`, `has more than one primary key`, `unknown datatype`, or `no such column`. Errors before the lookup or in the parser stay: `there is already an index named i`, `there is already a table named t`, `no such table: main.x`, `unsupported use of NULLS FIRST`, `object name reserved for internal use`, `unknown table option`. `drop table if exists i` of an index, `drop index if exists t` of a table, and `drop table if exists nosuch.t` give nothing.
- `changes()` is left as it was by every one of the four statements, with the clause or without, doing something or nothing. Under `pragma query_only` a statement that does nothing succeeds; one that would write gives `attempt to write a readonly database`, after the errors of the declaration.
- Syntax errors: `create if not exists table` (`near "if"`), `drop table t if exists` (`near "if"`), `create table if exists` (`near "exists"`), `drop table if not exists` (`near "not"`), `if not t`, `if t`, the clause twice, a name `not` or `exists`.

The code. `/internal/sql/stmt/stmt.go`: `CreateTable.IfNotExists`, `CreateIndex.IfNotExists`, `DropTable.IfExists`, `DropIndex.IfExists`, each a bool, documented with what SQLite does. `/internal/sql/stmt/recognize.go`: `ifExists` takes the clause, `notIf` rejects the bare name IF after IF NOT EXISTS as `bare name IF after IF NOT EXISTS`. `/internal/sql/stmt/vocabulary.go`: the field names `IF NOT EXISTS` and `IF EXISTS` are gone, and the README's vocabulary with them; the new name is in the README.

Tests: 81 cases in `/internal/sql/stmt/check_test.go` (supported forms with their `SQL`, the refusals after the clause, the syntax errors), a subtest that checks every keyword after each clause against SQLite and the pools, and a subtest that pins SQLite's behaviour on a database with the objects and without, also under `pragma query_only`, with the count of changes and the pass's answer for each text. `canonical` writes the clause; `checkCreateTable`, `checkCreateIndex`, `checkDropIndex`, and the DROP TABLE case of `checkMeaning` check that SQLite does nothing where the plain form gives `already exists` or `no such`, with `db.snapshot` and `db.nothing`. The allow-list has the six new fields; the near misses have 33 forms of the clause in the wrong place or with what stays not supported, in place of the four that are now supported.

`/internal/sql/stmt/stmttest/stmttest.go`: `Generator.Exists`, and `Generator.exists`, which draws only when Exists is not 0. The pass's tests set it to 3.

The engine: `existsRefusal` in `/internal/engine/exists.go`, called once at the end of `check` in `/internal/engine/engine.go`, gives `IF NOT EXISTS` or `IF EXISTS` at the third token, the IF, which is what the pass gave; `TestCheck_exists` in `/internal/engine/exists_test.go` pins it. The driver's `check` in `/conn.go` calls `engine.Check` again, as in slice 8, because `Prepare` checks with `stmt.Check` alone and `drivercheck` pins `prepare failed with ... IF EXISTS (at offset 11)`. `stmttest.Others` holds `create table if not exists ...` and `drop table if exists ...`, which the pass now supports and the engine refuses, so the engine's and the driver's generated tests failed with "the engine does not support a statement that the pass supports"; the `checkText` wrappers of slice 8 came back in `/internal/engine/differential_test.go` (used by the simulation and `tx_test.go` too) and in `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`.

### Why

The brief's list of the forms and of what stays not supported; SQLite's behaviour decided the details, above all the refusal of the bare name IF, where SQLite accepts the text and then fails on its own schema.

### What worked

- A dump of 64,440 lines, statements, calls, and arguments of `Generator` in six settings (none, Parameters, Columns, Nulls and Conditions, Changes, Indexes) and of `Sequence`, from 30 seeds, written before and after the change of the generator with `Exists` at 0, was byte for byte the same (`cmp before.txt after.txt && echo SAME`).
- Every supported case passed its meaning check against SQLite on the first run, the kept `SQL` among them; only two spans of my own counting were wrong.
- The engine's `TestDifferential` and `TestSimulation` without `-short` passed with every pinned number unchanged, once the wrappers were back.

### What didn't work

- A Python heredoc fed to `python3 /dev/stdin` wrote nothing and said nothing, so the first dump comparison compared the old generator with itself. `git diff --stat` showed it; scripts written to the scratchpad and run with `python3 script.py` worked. Heredocs and pipes with backticks were refused by the permission system as too complex to verify, as in earlier slices.
- `go test -short ./...` failed in `internal/engine` (`TestDifferential`, `TestSimulation`, `FuzzSQL`, `FuzzDifferential_core`) with `"create table if not exists ROWID (...)": the engine does not support a statement that the pass supports: s4ql: not supported: IF NOT EXISTS (at offset 13)`, until the `checkText` wrappers.

### What I learned

SQLite's IF NOT EXISTS is decided in `sqlite3StartTable` and `sqlite3CreateIndex` by leaving `pParse->pNewTable` null, so every later action of the declaration returns early; it is not a check after the parse. That is why the errors of the columns vanish, and why only the errors before the lookup stay.

### What was tricky

The pinned numbers. `TestGenerate_mutants` went from 39 to 29 syntax errors and from 153 to 151 not supported where SQLite does not say, and `TestGenerate_nearMisses` from 1,531 to 1,760. A throwaway test grouped the excused texts by SQLite's message and form: the mutants are all of CREATE TABLE (29 `duplicate column name`; 132 `unknown table option`, 17 `duplicate column name`, 1 `no such collation sequence`, 1 `AUTOINCREMENT ...`), moved because the generator draws more numbers with `Exists` on. The near misses are 258 for each of five forms of `table "t" has more than one primary key`, one of them the new `create table if not exists ... w int primary key`, 258 `generated columns cannot be part of the PRIMARY KEY`, 202 `AUTOINCREMENT ...`, and 10 `non-deterministic functions ...`: 258 CREATE TABLE statements of 2,000, where 261 were.

### What warrants review

- `ifExists` and `notIf` in `/internal/sql/stmt/recognize.go`, and the refusal of the bare name IF.
- The subtest "should answer for IF NOT EXISTS and IF EXISTS what SQLite does ..." in `/internal/sql/stmt/check_test.go`, which is the table for the engine's builder.
- `existsRefusal` in `/internal/engine/exists.go`, the driver's `check`, and the `checkText` wrappers.

### Future work

- The README of the pass and the review: step 2.
- The engine and the driver for slice 10, which remove `existsRefusal`, the wrappers, and the call of `engine.Check` in the driver's `check`.
