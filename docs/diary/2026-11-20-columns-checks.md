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
