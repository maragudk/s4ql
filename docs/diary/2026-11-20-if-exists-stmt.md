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

## Step 2: The documents, the review, and the checks

**Author:** if-exists-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "6. Decisions on details the agreement does not settle: by SQLite's behaviour and the rule above; record each in `docs/todo.md` [...] Finish: `make test`, `make lint`, `make check-browser`, `make fuzz-shards` pass; `make test-asan` only for the packages whose fuzz tests you changed. Self-review against these requirements."
**Interpretation:** Record the decisions and SQLite's facts in the todo, bring the README of the pass up to date, review the branch with the code-review skill, fix what matters, and run every check.
**Inferred intent:** A branch whose code, numbers, and documents agree, ready for the lead and for the builder of the engine.

### What I did

`/docs/todo.md`: the groups "Decisions of the statement pass of slice 10", "Facts of SQLite for the builder of the engine of slice 10", "Known differences from SQLite of slice 10", what the engine's builder removes, and an item for mutation testing and long fuzz runs. A writer sub-agent updated `/internal/sql/stmt/README.md`: the typed form, the grammar and the facts of the four statements, "How it is tested", and "Results", with the numbers of a run of its own (the corpus has 113 more supported texts, 92 of them DROP TABLE IF EXISTS). I read its diff. It found that I had the order of SQLite wrong in the doc comment of `CreateTable.IfNotExists` and in the todo: `sqlite3StartTable` looks for the table first and for an index of the name after it, so `there is already an index named t` is an error of the plain statement when no table has the name, not one before the lookup. I fixed both from the source.

The review (`code-review high origin/main...HEAD`) found no bug and four nits, all fixed: the README still said the clauses of the index statements were not supported (the writer's work was not done yet); `existsRefusal`'s comment and the todo claimed every text with a clause gets the old error, which holds only for the texts that the pass supports, so a refused text with a clause now gets the error of its later token (`column type other than ...` at 32 for an unknown type, where it was `IF NOT EXISTS` at 13), which the todo now says; the comment of `stmttest.Others` counted six texts that the pass supports, eight now; and the two throwaway tests of the dump and of the counts, which return when their flag is missing, were deleted before any commit.

### Why

The decisions are the lead's to confirm, and a moved number or a changed message needs its reason written down.

### What worked

`make test` (with the crosscheck module), `make lint` (0 issues), `make check-browser`, and `make fuzz-shards FUZZGROUP=c FUZZSHARDS=4` and `FUZZGROUP=go FUZZSHARDS=2` passed. `make asan-test ASANPKGS="maragu.dev/s4ql/internal/sql/stmt maragu.dev/s4ql/internal/sql/stmt/stmttest"` in the `golang` image passed: the seeds of `FuzzCheck` and `FuzzGenerate` changed with the cases and the generator, though their code did not. `go test ./internal/sql/stmt/...` without `-short` passed in about 80 s; the engine's `TestDifferential` and `TestSimulation` without `-short`, and the crosscheck module, passed. `-race -count=20` of `TestGenerator_*` passed. The dump of the generator with `Exists` at 0 was also compared with one written by the code of `origin/main` (checked out into the worktree for the run and restored): 64,440 lines, byte for byte the same once the new `IfNotExists:false` and `IfExists:false` fields of `%#v` are stripped.

### What didn't work

`make asan-test ASANPKGS=./internal/sql/stmt/...` failed with `asan-test: ./internal/sql/stmt/... is not a package that links C`; it takes import paths. `make fuzz-shards` alone fails with `FUZZSHARDS is a number of shards, such as 4, not ''`; CI passes the group and the number.

### What I learned

The writer's reading of `sqlite3StartTable` was right where my summary was not: a measured behaviour ("the index error stays") can be described with the wrong cause ("it comes before the lookup"), and the source settles it.

### What was tricky

Nothing beyond step 1.

### What warrants review

- The groups of slice 10 in `/docs/todo.md`, above all the refusal of the bare name IF and the known differences, which keep "not supported" for texts where SQLite would do nothing.
- The sections of the four statements and "Results" in `/internal/sql/stmt/README.md`.

### Future work

- The engine and the driver for slice 10; the todo has what to remove.
- Mutation testing and long fuzz runs of the code of slice 10 in the pass.
- The status of the README, the site, and the roadmap are for the lead's sub-agent after the merge.
