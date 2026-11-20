# Diary: Slice 7 of the roadmap in the generated checks, `where` on `select`

The engine and the driver run the WHERE of slice 7 since the branch `where-engine` (pull request #81, in review), with hand-written and table-driven comparisons with SQLite. `stmttest.Sequence`, which the engine's differential test, the driver's generated comparison with mattn's driver, and the engine's simulation draw from, made no WHERE beyond two fixed texts of `stmttest.Others`. This task makes it write WHERE, compares the selects with SQLite in the engine and with mattn in the driver, reads every generated select both ways in the engine, pins what each test reaches, runs the simulation over them with a planted bug that only a WHERE shows, and adds a fuzz target of WHERE. The branch is `where-checks`, from `origin/where-engine` at `6728627`.

## Step 1: WHERE in the sequence, both ways in the engine, the cross-check of the key access, the planted bug, and `FuzzWhere`

**Author:** where-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "1. **`where` in `stmttest.Sequence`:** selects with one to several conditions joined by `and`, on the key and on other columns, with every operator and `is null` / `is not null`; values drawn so that a condition is often true for some rows and false for others [...] 2. **The engine's differential test and the driver's generated test** compare these with SQLite [...] 3. **Both ways of reading rows, always:** the engine's differential test runs every generated `select` with a `where` on an engine that uses the key and on one with `scanAll`, in the same test run [...] 4. **The simulation of the engine** runs selects with `where` [...] Add a planted bug that only a `where` can show, in the key access, where a fault would be silent [...] 5. **Fuzz:** a fuzz target that builds a `where` from arbitrary bytes [...]"
**Interpretation:** Give the sequence a hook `Generator.Where` that draws conditions from what it inserted, count every case of slice 7 that the generated tests reach, run every generated select with WHERE twice in the engine, plant a silent bug of the key access, and add a fuzz target of WHERE from bytes.
**Inferred intent:** The generated checks, which found most bugs of the earlier slices, cover slice 7 with the same trust, and CI reads rows both ways without a flag.

### What I did

- `/internal/sql/stmt/stmttest/sequence.go`: `Sequence.where` is the hook `Generator.Where`. One select in two has a WHERE; one in five of those is a range of the key (a lower and an upper bound, open or closed, in either order, the lower first three times in four where the values compare) with up to two more conditions, the others have one or two conditions, or one time in eight four to seven. A condition's column is an earlier condition's one time in four, a name that the shape lacks one time in ten (in double quotes for some spellings), the key one time in three, and any column of the shape otherwise. IS NULL and IS NOT NULL come one time in twelve each. `conditionValue` gives NULL one time in twenty, a value of another type one time in twenty, a value that an insert gave the column two times in five (`Sequence.store` keeps the last twelve per column, without a random number, so the inserts draw as before), a neighbour of one (`neighbour`: the next integer or real, a prefix, the least value above, the last character or byte one more or less) one time in eight, an edge (`edgeOf`: the limits of 64 bits, 0.0, -0.0, the largest and smallest reals, the empty text and blob, ...) one time in ten, and `valueOf` otherwise. One WHERE in eight gets the inverse of one of its conditions, which contradicts it. The generator writes a value as a `?` one time in three, so NULL comes by the literal and by a nil argument, and `Sequence.Next` puts rejected arguments around one call in four.
- `/internal/sql/stmt/stmttest/rule.go`: `WhereCases` (the cases of a select with WHERE for the counts: each operator on the key or another column, each kind of value, the faults, how the plan reads) and `KeyAccess` (the roadmap's rule of the key access, as the engine's README details it). Tests in `rule_test.go` and `sequence_internal_test.go`.
- `/internal/engine/differential_test.go`: `pair.bothWays` runs each call that holds a select with WHERE a second time with `engine.Engine.SetScanAll(true)` (new in `/internal/engine/export_test.go`; a query reads the field when it starts) and requires the same answer, message, and count of changes. It is in `pair.step`, so `TestDifferential`, `TestConn_differential` (in transactions too), `FuzzDifferential`, `FuzzDifferential_core`, `FuzzSQL`, `FuzzTypes`, and the new `FuzzWhere` all read both ways. `reachWhere` counts the cases and cross-checks `stmttest.KeyAccess` against `engine.AccessOf` (new in `export_test.go`: the access that `buildQuery` gives the scan, on a reader of one table that reads no rows), so that a plan that reads more rows than the rule says fails, which no comparison of rows sees. `pair.longCondition` and the excuse `excusedLongCondition`: a literal in a condition longer than the length limit is skipped by the check of the type and excused only where SQLite gives rows and s4ql `string or blob too big` (the known difference).
- The planted bug `openBound` (`/internal/engine/bugs.go`): `bugs.reader` wraps the reader of a query in `openBounds`, whose `scanRange` reads a closed bound as open. `engine.go` and `tx.go` call `buildQuery` with `c.e.bugs.reader(...)`; I did not change the signature of `buildQuery`, because `operator_internal_test.go` calls it eight times and another builder works in the tests of the engine.
- `/internal/engine/simulation_test.go`: every second engine of a node (`n.gen` odd before the open) reads every row; `run.countWhere` counts the selects with WHERE that `verify` and `verifyTxs` compare with SQLite, those on an engine that reads every row, and those that give rows; `openBound` is in `TestSimulation_plantedBugs`.
- `drivercheck`: `pair.reachWhere` counts the cases of each run, as the engine's test does.
- `FuzzWhere` and `FuzzWhere_core`: a table of one of the five types, with a key and a nullable v, rows at the edges of the type; the bytes build up to eight conditions on k, v, or x (also in double quotes), with every spelling of the operators, and values of a row, of the bytes, NULL, or of another type, as literals or arguments.

### Why

The comparison of rows with SQLite cannot tell a plan that reads every row from one that reads by the key, and a fault of the key access that drops a row shows only where a condition's value meets a stored key, so the values come from the inserts, and the access is checked against the rule on every generated select.

### What worked

- The first full run of `TestDifferential` with WHERE found no difference from SQLite and no difference between the two ways of reading, and neither did `TestGenerated` against mattn, `TestConn_differential`, or `TestSimulation`.
- A temporary mutant that dropped the `get` of `keyAccess` (`a.key = nil`) failed `TestDifferential` at once by the cross-check: `the plan reads "a scan from a lower bound", and the rule "a get of one key"`.
- `openBound` was found in 2 of the first 23 seeds of the default workload, by `, and in SQLite`.

### What didn't work

- My first `stmttest.WhereCases` panicked in `TestDifferential`: `panic: interface conversion: interface {} is nil, not []uint8`, from `compareValues` on a comparison with NULL in the check of contradicting conditions. It compares only values that are not NULL now.
- A heredoc to append to `rule.go` was refused: "This agent is isolated in the worktree ..., but this command is too complex to verify that it stays inside the worktree." The Edit tool worked.
- My first baseline timing ran while I edited, and failed to build: `q.where undefined (type *Sequence has no field or method where)`. I took it again at `6728627` with a detached checkout.
- Adding a parameter for the planted bug to `buildQuery` broke eight calls in `operator_internal_test.go` (`not enough arguments in call to buildQuery`); I reverted it for the wrapper of the reader.
- My test of `WhereCases` expected no contradiction for `k >= 3 and k > 3 and k <= 3`; `> 3` and `<= 3` are inverses, so the test was wrong.

### What I learned

- With a table of a few rows, most WHERE give no row: IS NULL of the key, a contradiction, an empty range, and a comparison with NULL all give none, so the count of selects that give some rows needs the range form with values of the inserts.

### What was tricky

- Running a select a second time must not change what the tests see: a select leaves the count of changes, and in a transaction it reads the same snapshot, so the second run on the same connection gives the same answer, and a test that toggles the engine's field between queries on one goroutine has no race.

### What warrants review

- `Sequence.where` and `conditionValue` in `/internal/sql/stmt/stmttest/sequence.go`; `WhereCases` and `KeyAccess` in `rule.go`.
- `pair.bothWays`, `reachWhere`, `pair.longCondition`, and `engine.AccessOf`.
- `openBounds` in `/internal/engine/bugs.go`.

### Future work

- The pinned numbers, the least numbers of the simulation, the counts of the planted bugs, the fuzz runs, and the documents: step 2.

## Step 2: The pinned numbers, the simulation's counts, the fuzz runs, the documents, and the review

**Author:** where-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Pin, with minimums, how often each case of slice 7 is reached [...] Say how many of 200 seeds find each planted bug, old and new. [...] Run it for about a minute. 6. Update the pinned numbers that move and explain each in your report. [...] Say in the report how long the changed packages take before and after [...] Review your own work with the code-review skill before you report, and fix what it finds."
**Interpretation:** Pin every excuse that moved with its reason, set least numbers for slice 7 at a third (generated tests) or a quarter of 1,000 seeds (simulation), count the planted bugs over 200 seeds, fuzz each new target for a minute, have a writer bring the documents up to date, review, and run every check.
**Inferred intent:** A branch whose numbers, documents, and code agree, ready for the lead.

### What I did

- Pins, with the reasons beside them in the code: `TestDifferential` acceptedStartedTable 18 to 19, excusedArguments 703 to 718, excusedArgument 220 to 248, excusedType 350 to 478, excusedLong 8 to 4, and the new excusedLongCondition at 0 (the run reaches 2 such selects, which SQLite refuses too); `TestGenerated` excusedPrepare 1063 to 1178, excusedStartedTable 10 to 11, excusedArguments 426 to 411, excusedArgument 200 to 260, excusedType 169 to 231, excusedLevel 74 to 69, excusedRowsOpen 46 to 24, excusedTxOpen 29 to 24, excusedOutside 53 to 58, excusedQuoted 48 to 69. Least numbers of slice 7 in both, a third of the run; `TestConn_differential` requires 146 selects read both ways, 32 of them in a read-only transaction.
- `S4QL_ENGINE_SIM_SEEDS=1000 go test -v -run '^TestSimulation$' ./internal/engine/` (235 s): selects with WHERE compared with SQLite of their own 3,078, 2,449, and 6,968 in the default, idle, and hammer workloads, in a transaction 4,254, 2,875, and 8,052; `TestSimulation` requires a quarter for each seed.
- `S4QL_ENGINE_SIM_SEEDS=200 go test -v -run '^TestSimulation_plantedBugs$' ./internal/engine/` (299 s): skipUnique 165 (158 by its own violation), ackUnknown 47, checkOutside 61, keepOpen 199, keepRead 80 (37), annotateNext 156, keepFailed 180, readNewest 178 (154), rollbackKeeps 109, convertReal 16 (12), listOrder 118 (39), zeroLeftOut 39 (36), openBound 20 (20); none without its bug.
- `go test -run '^$' -fuzz '^FuzzWhere_core$' -fuzztime 60s ./internal/engine/`: 11,619 executions; `FuzzWhere`: 222,955; nothing found.
- A writer sub-agent updated `/docs/todo.md`, `/internal/engine/README.md`, and `/internal/sql/stmt/README.md`; I read the diff, fixed three sentences that the review had made stale (`-scanall`, `openBound`'s one-sided bounds, the long literal), and committed it. Its report found that the doc comment of `Sequence.where` said "another column of the shape" where the code draws any column; I fixed the comment.
- The review (`code-review high` over `6728627...HEAD`) found one real fault and four nits, all fixed: under `-scanall`, `pair.bothWays` returned at once, so the new least numbers of "read both ways" failed and the second run was off; it now runs the other way of the engine's setting (`Engine.ScansAll`). A long blob literal in a condition that s4ql stopped refusing would give no rows on both sides unseen; `pair.step` now requires `string or blob too big` where SQLite gives no error. `whereOf` could draw INT for an INTEGER table as "another type". The comments of `openBound` said "between bounds" where one bound is enough. The todo and the README were stale (the writer's work).
- `go test -count=20` of `TestDifferential`, `TestConn_differential`, `TestSimulation`, `TestSimulation_plantedBugs`, and the seeds of `FuzzWhere` and `FuzzWhere_core` passed (762 s), and with `-race` (1,046 s); `stmttest` with and without `-race`; drivercheck's `TestGenerated` `-count=20` (107 s) and `-race -count=5` (74 s).
- Run times, `go test -count=1` of each package alone, on a machine with a load average of 22 from other sessions: `internal/engine` 42.9 s before and 46.0 s after (`TestDifferential` 3.9 s to 4.6 s, `TestSimulation` 6.3 s to 8.0 s, `TestSimulation_plantedBugs` 5.7 s to 9.8 s, most of it `openBound`, which needs about 20 seeds for its two); `internal/sql/stmt` 51.1 s and 45.8 s (noise; its code did not change); drivercheck 11.7 s and 10.1 s.

### Why

A moved pin needs its reason, and a least number that is a share of a long run fails when a case stops, not by chance.

### What worked

- Two temporary mutants confirmed the new checks: dropping the `get` of `keyAccess` failed `TestDifferential` by the cross-check, and skipping `checkLength` of the values of conditions failed it at seed 266.

### What didn't work

- The review showed that my first `pair.bothWays` returned at once under `-scanall`, so `go test -run 'TestDifferential$|TestConn_differential$' ./internal/engine/ -scanall` failed with `the run reaches "read both ways" 0 times, and at least 444 were expected`.
- `make lint` found `QF1002: could use tagged switch on n` and `QF1001: could apply De Morgan's law` in `sequence.go`.

### What I learned

- `openBound` is found by 20 of 200 seeds: it shows only where a closed bound of the key equals a stored key and the query runs on an engine that reads by the key, which every second engine of a node does not.

### What was tricky

- Measuring run times on a machine loaded by other sessions: the package that I did not change moved by 5 s, so the differences below that are noise.

### What warrants review

- The decisions in `/docs/todo.md` under "Decided by the builder of the generated checks for slice 7", for the lead to confirm.
- Whether every second engine of a node in the simulation should read every row: it checks the path of `scanAll` under crashes, at the price of fewer seeds that find `openBound`.

### Future work

- Mutation testing of the code of slice 7, as `/docs/todo.md` says. `drivercheck` cannot read both ways without a knob in the driver.
