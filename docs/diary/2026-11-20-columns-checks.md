# Diary: Slice 5 of the roadmap in the generated checks, several columns

The engine and the driver run tables of several columns since the branch `columns-engine` (pull request #73), with hand-written and table-driven comparisons with SQLite. The generated checks still drew one-column statements, with `stmttest.Generator.Columns` at 0, and their helpers of the rule of the type read only the first value. This task makes `stmttest.Sequence` write tables of one to three columns, runs the engine's differential test, the driver's generated comparison with mattn's driver, and the engine's simulation over them, pins what each reaches, and plants bugs that only several columns show. The branch is `columns-checks`, from `origin/columns-engine`.

## Step 1: A fix of the order of the errors, and several columns in the sequence and the differential test

**Author:** columns-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "1. **Several columns in the generated tests of the engine and the driver.** Set `Generator.Columns` and extend the generator where needed, so that sequences hold: tables of one to several columns of mixed types with the key at any position; an `insert` with and without a list, the list in any order; a `select` of names in any order with repeats, and `*`; and the faulty forms that slice 5 defines, each reached often enough [...] 2. **The helpers for the type rule** read every value and expect what the engine's README says [...]". And, mid-task, from the lead: "for a row whose values each fit the length limit but whose record does not, and whose key already exists, SQLite gives `18/18: string or blob too big` and the engine gives `19/1555: UNIQUE constraint failed`. [...] measure the record before the check of the key [...] do not estimate the size in the engine; ask the core, or order the calls so that the core's own refusal is what the engine reports."
**Interpretation:** First fix the order of `string or blob too big` and `UNIQUE constraint failed` with a hand-written test, then give the sequence shapes of tables of several columns, and make the differential test's rule of the type read every value.
**Inferred intent:** The generated checks, which found the most bugs in earlier slices, cover slice 5 with the same trust.

### What I did

The fix. SQLite builds the record of a row of a WITHOUT ROWID table before it looks for its key, so a row too long for its record gets `string or blob too big` also where its key exists; I checked that `insert or ignore` behaves so too in the sqlite3 shell of 3.54.1 (`.limit length 1000`, then an insert of an existing key with 600 bytes of text and 600 of blob gives `string or blob too big` for `insert` and for `insert or ignore`, and `insert or ignore` of a key that exists with a short row changes 0 rows). `/internal/core/tx.go` has a new `Tx.Insert`, which inserts a row only if its key is new and reports whether it did, through `conn.insert` in `/internal/core/cache.go`, `insert or ignore ... returning 1`, so that SQLite itself measures the record. The engine's `writer` has `insertNew` (`/internal/engine/storage.go`), and `insert.run` in `/internal/engine/operator.go` calls it only when `get` found the key, so the path of a new key keeps its calls (`get`, `upsert`) and the fakes of the other builder's tests keep theirs. Tests: a subtest of `TestTx_tooBig` in `/internal/core/tx_test.go` (a new key, a key that exists, at each length around the boundary against SQLite's own table, and the log), a subtest of `TestEngine_tooBig` in `/internal/engine/engine_test.go` (with and without a list, in a transaction and not, against SQLite, with the count of changes), and a subtest of `TestInsert` in `/internal/engine/operator_internal_test.go`. The doc comment of `Conn.Exec`, the engine's README, slice 5 of `/docs/roadmap.md`, and the entry in `/docs/todo.md` give the new order.

The generator. `/internal/sql/stmt/stmttest/stmttest.go` has three hooks, `Generator.Create`, `Generator.Insert`, and `Generator.Select`, which give the columns of a create table, the list and the values of an insert, and the select list; with them nil the generator draws exactly as before, so the pinned numbers of `/internal/sql/stmt/generate_test.go` did not move. `/internal/sql/stmt/stmttest/sequence.go`: each name of a table has a shape of one to three columns, with distinct names from at least four folds of names of columns, mixed types (each column after the first takes the type of an earlier one half of the time, so that two columns of one type are common), and the key at any position. Create tables declare the shape nine times in ten (one in ten of these retypes a column), and the tenth draws columns at random, whose names can repeat (`duplicate column name`). An insert names the shape's columns in a list, shuffled two times in three, or has no list one time in three; one list in eight leaves a column out, one in twelve swaps a name for another that is no other of the list, one insert in sixteen has a value more and one a value less, one in ten has a value of any type, and a third of those a second. A select is `*` half of the time, otherwise one to four names, one in four a repeat, one in twelve any name of the sequence. The names of columns now come from the names that SQLite reads as a name in every position of a column, the select list too. `/internal/sql/stmt/stmttest/rule.go` is new: `TableColumn`, `Positions`, `ColumnOf`, `TypeRule`, `ValueKind`, and `Faults`, shared by the engine's tests and drivercheck, which is a module of its own and cannot share the engine's test helpers.

The differential test, `/internal/engine/differential_test.go`: `typeRule` reads the columns of the table in SQLite (`oracle.columns`, `pragma_table_info` in the order of `cid`) and asks `stmttest.TypeRule`, which gives "" where a column has no value (SQLite gives NOT NULL first) and otherwise the refusal of the first column of another type in the order of the table; `valueKind`, `insertType`, `oracle.column`, and `columnHolds` are gone; `checkLength` takes the longest literal; `reach` counts every literal and argument, the key's type, -0.0 in the key, and the forms and faults of slice 5. The simulation's `coreState` wrote only the first value of each row; it writes every value now.

### Why

The rule of the type must name the first column of another type in the order of the table, and only after NOT NULL; without the list in its positions, the test would excuse wrong refusals or fail right ones.

### What worked

- The first full run of `TestDifferential` over the new sequences found no difference from SQLite, and neither did 3,000 more sequences from seed 1,000,000 (`go test ./internal/engine/ -run '^TestDifferential$' -seed 1000000 -sequences 3000`, 42 s).
- `TestSimulation` and `TestSimulation_plantedBugs` passed at once once `coreState` wrote every value.

### What didn't work

- A shell heredoc and a `python3` heredoc that edited files were refused: "this command is too complex to verify that it stays inside the worktree". The Edit tool worked.
- My first hook for the select wrote `from` and the table twice: `SELECT /**/ANALYZE ... from OIDfrom "oid"`, which the pass refused as `alias`. The hook's branch now only picks the table, and the common code writes it.
- `slices.Delete(order, r.IntN(len(order)), 1+r.IntN(len(order)))` panicked with `slice bounds out of range [2:1:]`; a typo of two draws.
- `the engine does not support a statement that the pass supports: s4ql: not supported: result column other than a column or * (at offset 24)` for `SELECT "  ", CURRENT_DATE, ...`: the names of columns came from the pools of the create table and the insert, where SQLite reads `current_date` as a name, but in a select list it is a value. They now come from the names common to the three positions of a column.
- `TestSimulation` failed on seed 0 with `the replay of the log holds no (range TEXT, saVePoInT TEXT): "a" "it's" "é"`, where SQLite had two values in each row: `coreState` read `row[0]`. A gap of the check.

### What I learned

- SQLite measures the record of a WITHOUT ROWID row before every conflict check, also under `insert or ignore`, which makes `insert or ignore` an exact probe of whether SQLite takes a row.
- The pins of the excuses moved, and why: `acceptedStartedTable` 21 to 17, `excusedArguments` 644 to 718, `excusedArgument` 140 to 197, `excusedType` 371 to 336, `excusedLong` 5 to 3. Inserts have up to four values, so more parameters and more arguments around them; one insert in ten has a value of any type where one value in ten had; the sequence draws other numbers.

### What was tricky

- Keeping the generator's draws for nil hooks identical, so that the statement pass's pinned numbers stay; the select branch must pick its table before the list only with a hook.

### What warrants review

- `insert.run` in `/internal/engine/operator.go` and `Tx.Insert` in `/internal/core/tx.go`: the touch of a key with `existed` false after an insert of a new key.
- `stmttest.TypeRule` and `stmttest.Faults` in `/internal/sql/stmt/stmttest/rule.go`, against the order in the engine's README.
- `reachedDifferential`: each least number is about a third of the run of 2026-11-20.

### Future work

- The driver's generated test, the mattn limit of names in double quotes, and the simulation's check of the rows of the log and its planted bugs, in the next steps.

## Step 2: The driver's generated test, the simulation's check of the rows of the log, `listOrder`, and the review

**Author:** columns-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "3. **Known limit of the driver's oracle:** mattn's SQLite reads a double-quoted unknown name as a string, where the pinned build gives `no such column`. Keep such a `select` out of the comparison with mattn in a way that is counted [...] 4. **The simulation of the engine** runs with tables of several columns, and checks the log against them: every row in the log has one value per column of its table, each of its column's type. Add a planted bug that only several columns can show [...] Say how many of 200 seeds find each planted bug, old and new. 5. Update the pinned numbers that move and explain each in your report."
**Interpretation:** Make drivercheck read every value, count the mattn excuse, check the rows of the log, plant a bug of the order of a list, and pin and explain what moves.
**Inferred intent:** The same trust for the driver and the simulation as for the differential test.

### What I did

`/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`: `typeRefusal` asks `stmttest.TypeRule` with the columns of the table where the call runs (`pair.columns`, `pair.columnsIn`); `columnType` is gone; `reach` counts every literal, the key's type, the forms and faults of an insert of several columns, and the forms of a select, on the tables of the runner where the call runs. A `QueryRow` scanned one value, which failed with `sql: expected 4 destination arguments in Scan, not 1`; it now runs again with as many values as that error names. `pair.checkQuoted` keeps a select whose first name that the table does not have is in double quotes out of the comparison with mattn, checks each run's error against the pinned build's `no such column: "x" - should this be a string literal in single-quotes?`, and counts it as `excusedQuoted` (32 in the default run). Two hand-written steps that the builder of the engine had left out are back, a select of `"x"` and the refusal of a value of another type in a list of several columns, with one of NOT NULL before it. The pins moved: `excusedPrepare` 1012 to 1072, `excusedStartedTable` 12 to 9, `excusedArguments` 395 to 396, `excusedArgument` 126 to 189, `excusedType` 180 to 161, `excusedLevel` 78 to 70, `excusedRowsOpen` 38 to 29, `excusedTxOpen` 22 to 30, `excusedOutside` 49 to 68, `excusedQuoted` new at 32. A throwaway run of 1,500 other seeds (from 100,000) found no difference from mattn.

`/internal/engine/simulation_test.go`: `readLog` keeps the changes of each commit, and `run.verifyRows` checks every upserted row against the table that the log has there: for every column in order, one value per column, each of its column's type. "rows of several columns" is counted and required: the first 1,000 seeds gave 4,591, 715, and 8,380 in the default, idle, and hammer workloads, and the least numbers are 1.1, 0.17, and 2 for each seed. The planted bug `listOrder` (`/internal/engine/bugs.go`, `buildSink` in `/internal/engine/operator.go`, a case in `TestBuild`) gives the values of a list to the columns in the order of the list; it is planted in statements of their own, since `writeIn` hands `buildSink` only two bugs and a change of that line would rename keys of `/internal/sql/mutation/equivalent.txt`. 200 seeds of each bug, seeds that found it, against the counts of 2026-11-19: skipUnique 168 (168), ackUnknown 67 (45), checkOutside 68 (67), keepOpen 198 (194), keepRead 98 (111), annotateNext 165 (148), keepFailed 186 (187), readNewest 180 (172), rollbackKeeps 100 (108), convertReal 14 (14), listOrder 125, 39 of them by its own violation, "the replay of the log holds". None found anything without its bug. 1,000 seeds of each workload found no violation.

A writer sub-agent updated `/internal/engine/README.md`, `/docs/todo.md`, and `/internal/sql/stmt/README.md`; I read its diff, and fixed two facts in the doc comment of `stmttest.Sequence` that its report found wrong (one insert in three has no list, not two; one value in three is a `?`, not one insert).

The review (`code-review high`, over `92438c6...columns-checks`, since `origin/columns-engine` was merged and gone) found no bug and five nits. Fixed: drivercheck's `reach` read the tables of the transaction also for a call beside it, and now takes the runner of the call; the doc comment of `Conn.Exec` was not rewrapped; the least numbers of rows of several columns were not quite a quarter (1.2 and 0.18, now 1.1 and 0.17); and `/docs/todo.md` now says that the core's own model and simulation tests drive `Upsert` and not the new `Tx.Insert`. Left: the nit that the engine's `reachInsert` counts a call with a named argument as reaching the forms; the engine's test hands the engine the value of a named argument, as `values` says, so the engine runs such a call, and the count is right there.

### Why

The comparison with mattn must not hide a select, so each one it leaves out is checked against the pinned build and counted. The replay of the log in SQLite runs its statements, not its rows, so only a check of the rows sees a row whose values are in other columns than its statement names.

### What worked

- `go test -count=20` and `-race -count=20` of `TestDifferential`, `TestConn_differential`, `TestSimulation`, and `TestSimulation_plantedBugs` passed (347 s and 367 s at a load of about 23), and so did `-count=20` and `-race -count=20` of the new tests of the core and the engine, and `-race -count=5` of drivercheck's `TestSequences` and `TestGenerated`.
- `make test` (3 min 9 s), `make lint`, and `make check-browser` passed.

### What didn't work

- The first 200 seeds of `TestSimulation_plantedBugs` failed for `convertReal`: `seed 41 finds the bug by another violation: [... gives "s4ql: not supported: a string literal for the column WITh.Ｔ１ of type INTEGER", and the rule for the type of a value "s4ql: not supported: a real literal for the column WITh.OTHERS of type INTEGER"]`. With several columns the real that the bug converts can be the first value of another type, so the engine refuses a later one. That violation is the bug's, and is in its list now.
- `s4qlpb.Value_Kind_case` is not exported (`undefined: s4qlpb.Value_Kind_case`); `holds` uses `HasInteger` and the others.
- Some compound shell commands were refused as "too complex to verify that it stays inside the worktree"; plain commands and the Edit tool worked.

### What I learned

- `database/sql`'s `Row` gives no columns; its error `sql: expected N destination arguments in Scan, not 1` is the only way to learn the width of a row of `QueryRow` without another query.

### What was tricky

- Choosing the own violation of `listOrder`: a key in the wrong column shows first as a later insert of the log that fails in SQLite, and a value of another type in the wrong column as a refusal that the rule does not give; the replay of the log, which writes the rows, is the violation that only a row in the wrong order causes.

### What warrants review

- `pair.checkQuoted` in drivercheck: the condition that the first unknown name is quoted, which is where mattn and the pinned build differ.
- `run.verifyRows` in `/internal/engine/simulation_test.go`.
- The planted bug `listOrder` in `buildSink`.

### Future work

- Mutation testing of the code of slice 5, including the core's `Tx.Insert`, and fuzz targets of several columns, as `/docs/todo.md` says.
- The core's model and simulation tests could drive `Tx.Insert`.
