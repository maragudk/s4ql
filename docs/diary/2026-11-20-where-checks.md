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
