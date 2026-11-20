# Diary: Slice 6 of the roadmap in the generated checks, `null`

The engine and the driver run the NULL of slice 6 since the branch `null-engine` (pull request #78, in review), with hand-written and table-driven comparisons with SQLite over 44 table shapes. `stmttest.Sequence`, which the engine's differential test, the driver's generated comparison with mattn's driver, and the engine's simulation draw from, made no column that takes NULL and no NULL. This task makes it write them, compares them with SQLite in the engine and with mattn in the driver, pins what each reaches, runs the simulation over them with a planted bug that only NULL shows, and lets `FuzzTypes` draw NULL. The branch is `null-checks`, from `origin/null-engine`.

## Step 1: NULL in the sequence, the differential test, the driver's test, the planted bug, and `FuzzTypes`

**Author:** null-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "1. **Null in `stmttest.Sequence`:** table shapes in which some, all, or none of the columns outside the key take null; an `insert` that gives null by the literal, by a nil argument (in the driver's test also by the other kinds of nil that `database/sql` turns into nil: a nil pointer, a `sql.Null*` value that is not valid), and by leaving a nullable column out of the list; and the faults, each reached often enough [...] 2. **The engine's differential test and the driver's generated test** compare these with SQLite [...] In the driver's test, read rows into `any` and also into `sql.Null*` and pointers, so that the scan of a null is compared too. 3. **The simulation of the engine** runs with nullable columns and null values. [...] Add a planted bug that only null can show [...] 4. **`FuzzTypes`** (and `FuzzTypes_core`) [...] let them also draw null as a value and a nullable column, if that is small"
**Interpretation:** give the shapes of the sequence a NOT NULL per column outside the key, write NULL through the existing hooks, count every case of slice 6 that the tests reach with least numbers, compare the scans into typed destinations in the driver, count NULL in the log, plant a bug of a left-out column, and add a form and tables of NULL to `FuzzTypes`.
**Inferred intent:** the generated checks, which found the most bugs in earlier slices, cover slice 6 with the same trust as slices 4 and 5.

### What I did

- `/internal/sql/stmt/stmttest/sequence.go`: each shape of more than one column has, for its columns outside the key, none that take NULL, all, or each one time in two (a third each). A create table turns whether one column takes NULL one time in ten, and its columns at random take NULL one time in two. An insert gives NULL one time in four to a column that takes NULL, its list leaves such a column out one time in three, and one insert in twelve has NULL for any column, the key too. A call around a NULL argument has a nil `[]byte` in its place. Nil is out of `Unsupported` (`/internal/sql/stmt/stmttest/values.go`), since it is NULL now.
- `/internal/sql/stmt/stmttest/stmttest.go`: with `Generator.Create`, the generator writes NOT NULL after a column outside the key only if the hook's `NotNull` says so; `Nulls` stays for the pass's own tests and does not apply to a hook's columns. The pass's pinned numbers did not move.
- `/internal/sql/stmt/stmttest/rule.go`: `Faults` names `NULL in the key` apart from `NULL in a column that does not take it`, and the new `NullCases` gives the cases of NULL that an insert reaches, shared by the engine's test and `drivercheck`. Tests in `rule_test.go`.
- `/internal/engine/differential_test.go`: `answer.null`; `reach` counts the create tables by how many columns take NULL, each case of `NullCases` by outcome, and selects of `*` and of names with NULL among their rows. `reachedDifferential` has least numbers for each, a third of the run.
- `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`: `withNils` makes each nil argument one of `nils` (nil, nil pointers of five types, a nil `*sql.NullString`, and four `sql.Null*` that are not valid) and each nil `[]byte` one of `nilBytes` (direct, `new([]byte)`, `sql.Null[[]byte]{Valid: true}`); `argumentKind` counts each kind. `read` scans each row again into a `sql.Null*` value and into a pointer per column (`typedDest`, `typedText`), kept in `outcome.scans`, which `outcome.full` adds to the text that `compare` and `compareTables` compare; `String` stays as it was, so the hand-written outcomes of `TestSequences` are unchanged. Least numbers for each case of slice 6.
- `/internal/engine/bugs.go`, `buildSink` in `/internal/engine/operator.go`: the planted bug `zeroLeftOut` gives a column that takes NULL and that a list leaves out the zero value of its type; a subtest of `TestBuild`; an entry of `TestSimulation_plantedBugs`, found by ", and in SQLite" (a query gives 0 where SQLite has NULL) and by the replay of the log. 52 of 200 seeds find it, 44 by its own violation.
- `/internal/engine/simulation_test.go`: `run.verifyRows` counts the NULL values in the log, as "NULL values in the log".
- `FuzzTypes`: five tables whose first column takes NULL, and the form `formNull`, which gives NULL by the length of the input as the literal, as a nil argument, or by leaving the first column out.

Commits: `e8dbf27`, the format fix after it, the commit of `NullCases`, the commit of `drivercheck`, and the commit of the planted bug and `FuzzTypes`.

### Why

The differential tests are where the earlier slices found their bugs, so every rule of slice 6 must be reached there, and counted so that a change of the generator that stops reaching it fails.

### What worked

- The first full run of `TestDifferential` with NULL found no difference from SQLite, and neither did 4,000 more sequences from seed 1,000,000 (`go test -count=1 -run '^TestDifferential$' ./internal/engine/ -seed 1000000 -sequences 4000`, 34 s), nor `TestGenerated` against mattn.
- A temporary mutant that added a word to s4ql's scans of a row with NULL failed `TestGenerated` at seed 5, so the scans into `sql.Null*` and pointers are compared.

### What didn't work

- Python heredocs, and `cat > file <<'EOF'` into the scratchpad, were refused: "This agent is isolated in the worktree ..., but this command is too complex to verify that it stays inside the worktree." Scripts written with the Write tool and run with `python3 -I script file` worked.
- My first scans into typed destinations went into the rows of the outcome, which broke four hand-written outcomes of `TestSequences`, such as `Expected "query: columns "v1" of types "INT", rows int64:1", but got "... rows int64:1,sql.NullInt64:{Int64:1 Valid:true},*int64:1"`. They moved to `outcome.scans`.
- A case of my test of `NullCases` expected no fault for `insert into t (b, k, c) values (?, null, 1)`, where `c` is a blob: the integer is of another type. The test was wrong.

### What I learned

- `database/sql` lets `Rows.Scan` run more than once on one row, so a row can be compared as an `any` and as typed destinations without a second query.

### What was tricky

- The hook `Generator.Create` meant "the generator writes NOT NULL as for columns at random"; making it read `NotNull` changed nothing for the pass's tests, which set no hook, but every shape of the sequence had to set it.

### What warrants review

- `Sequence.insert` and `Sequence.create` in `/internal/sql/stmt/stmttest/sequence.go`, and `NullCases` in `rule.go`.
- `withNils`, `typedDest`, and `outcome.full` in drivercheck.
- `zeroLeftOut` in `buildSink`.

### Future work

- The documents, the minimums of NULL in the simulation, the counts of the planted bugs, and the fuzz runs: step 2.
