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

Commits: `e8dbf27`, `9a7a700`, `8749c4d`, `b73b4b3`, `de79354`, and in step 2 `8425817`, `764e9e6`, `889bcea`.

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

## Step 2: The counts of the simulation, the fuzz runs, the documents, and the review

**Author:** null-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "`run.verifyRows` already accepts a null only in a nullable column; make sure it is reached (count and require it). [...] Say how many of 200 seeds find each planted bug, old and new. [...] run each for about a minute. 5. Update the pinned numbers that move and explain each in your report. Fix the stale entry on the generator in the slice 2 part of `docs/todo.md` [...] Review your own work with the code-review skill before you report, and fix what it finds."
**Interpretation:** measure the NULL of the log over 1,000 seeds of each workload, count the planted bugs over 200 seeds, fuzz `FuzzTypes` and `FuzzTypes_core` for a minute each, have a writer bring the documents up to date, review, and run every check.
**Inferred intent:** a branch whose numbers, documents, and code agree, ready for the lead.

### What I did

- `S4QL_ENGINE_SIM_SEEDS=1000 go test -v -run '^TestSimulation$' ./internal/engine/` (118 s) gave 1,581, 316, and 2,694 NULL values in the log in the default, idle, and hammer workloads; `TestSimulation` requires a quarter: 0.4, 0.08, and 0.67 for each seed. The default 50 seeds give 61, 16, and 87.
- `S4QL_ENGINE_SIM_SEEDS=200 go test -v -run '^TestSimulation_plantedBugs$' ./internal/engine/` (150 s): skipUnique 163 (156), ackUnknown 45, checkOutside 65, keepOpen 198, keepRead 98 (54), annotateNext 160, keepFailed 188, readNewest 185 (164), rollbackKeeps 104, convertReal 5 (5), listOrder 112 (44), zeroLeftOut 52 (44), and none without its bug. The default run of `convertReal` now needs 63 seeds for its two.
- `go test -run '^$' -fuzz '^FuzzTypes$' -fuzztime 60s ./internal/engine/`: 114,686 executions; `FuzzTypes_core`: 5,001; nothing found, on 10 workers.
- A writer sub-agent updated `/docs/todo.md` (the status of slice 6, the stale entry of `Generator.Nulls`, a group of my decisions, what is left, and the stale entry of slice 2 on the generator), `/internal/engine/README.md`, and `/internal/sql/stmt/README.md`. Its report found one fact of mine wrong: the arguments that are a nil `[]byte` in `TestDifferential` went from 26 to 54, not from 9 (9 was the old least number); I fixed the comment in the test. I fixed the doc comment of `Sequence.next` that it found stale.
- The review (`code-review high` over `39fee85...HEAD`) found no bug and six nits. Fixed: `firstRow`, which compares the rows that `callQueryOpen` leaves open, now scans into `sql.Null*` values and pointers too; the doc comments of drivercheck's `pair.reach` and of `stmttest.Others`. Left: that `formNull` before `formSQL` renumbers the forms of cached fuzz inputs (no corpus of `FuzzTypes` is committed, and adding a form changes `forms` either way); that `docs/todo.md` records built details and moved numbers, which is how every builder's group of decisions there is written; and that the sequence no longer writes an extra nil argument for a statement without a parameter, since nil left `Unsupported` (see below).
- `make test` (1 min 52 s), `make lint`, and `make check-browser` pass. `go test -count=20` of `TestDifferential`, `TestSimulation`, `TestSimulation_plantedBugs`, `TestBuild`, and the seeds of `FuzzTypes` and `FuzzTypes_core` passed (400 s), and so did the same with `-race` (744 s), the `stmttest` tests with and without `-race`, drivercheck `-count=20` (112 s), and drivercheck `-race -count=5` (95 s).
- Run times of the changed packages, `go test -count=1`, before and after: `internal/engine` 20.8 s and 25.1 s, most of it `TestSimulation_plantedBugs` with the new bug and the 63 seeds that `convertReal` now takes; `internal/sql/stmt` 32.1 s and 32.5 s; drivercheck 5.6 s and 5.8 s.

### Why

The least numbers of the simulation are a quarter of 1,000 seeds, as for the rows of several columns, so that they fail when a case stops and not by chance.

### What worked

The writer's report checked my brief against the code and found the one wrong number.

### What didn't work

- The fix of the nit about an extra nil argument, `anyValue` giving nil one time in six, moved the random numbers of the simulation, and `TestSimulation/default` failed an old least number that has nothing to do with NULL: `50 seeds give 1 of "failed: stopped", and at least 2 were expected`. The first 1,000 seeds give 191 of them, so the 50 seeds were unlucky with a rare outcome that comes in clusters. I reverted the change rather than lower a least number of another slice to fit; the case it would cover is the error of the number of arguments, which comes before any value is looked at.
- A `-race -count=20` run of the engine's tests in one `go test` call hit the default timeout: `panic: test timed out after 10m0s`, with no test failed before it. It ran again with `-timeout 60m`.
- Loops of mine that waited for a file in the shell broke on `[ $(grep -c ... || echo 0) -ge 4 ]`, which gives two numbers when grep counts 0: `(eval):1: condition expected: 0`.

### What I learned

- The least numbers of `TestSimulation` over 50 seeds hold for the seeds they were set with; any change of the sequence's random numbers can trip one of a rare outcome, so a change of the generator should leave its draws alone where it can.

### What was tricky

- Keeping the review's fix from moving every count again: each draw of the sequence moves the pins of both differential tests and the run of every seed of the simulation.

### What warrants review

- The new group of decisions in `/docs/todo.md`, for the lead to confirm.
- `convertReal` is found by 5 of 200 seeds now, where it was 14; the default run passes at seed 62.

### Future work

- Mutation testing of the code of slices 5 and 6, as `/docs/todo.md` says.
