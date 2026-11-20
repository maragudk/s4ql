# Diary: Slice 7 of the roadmap in the engine and the driver, `where` on `select`

Slice 7 adds a WHERE of comparisons of a column with a literal or a `?`, and `is null` and `is not null`, joined with AND, to a select. The statement pass reads it since pull request #80 (branch `where-stmt`, diary `2026-11-20-where-stmt.md`). This task is the engine and the driver: a filter of the conditions in the plan, SQLite's comparisons, the exact types, SQLite's errors in SQLite's order, and the use of the key. The work is on the branch `where-engine`, from `origin/where-stmt` at `8d150cc`.

## Step 1: The core's scan by the key, the filter, the errors, and the tests by hand

**Author:** where-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "The roadmap's section of slice 7 is the requirement. Make it work end to end, through the engine and through `database/sql` [...] **Use of the key:** decide how the plan reads rows. A scan of the whole table with the filter is correct. Where the conditions fix or bound the primary key [...] and the core gives a cheap way (a `Get` by key; a scan that starts or stops at a key, if the core has one or can get one with a small, well-tested change), use it [...] the result and its order must be the same as with the full scan, and a test must show that for every operator, with the generated differential tests able to run both ways. [...] **Remove what was temporary:** `checkWhere`, the `checkText` wrappers, and the second parse in the driver's `check`."
**Interpretation:** Run the conditions in the engine, with SQLite as the judge of every comparison and error, read by the key where the conditions allow it, and take away the refusal of slice 7 that the pass's builder put in.
**Inferred intent:** A select with WHERE that gives exactly SQLite's rows and errors, that does not read the whole table for a lookup by key, with the same trust as slices 1 to 6.

### What I did

The core: `core.Bound` (a value, open or closed, or none), and `ReadTx.ScanRange` and `Tx.ScanRange` in `/internal/core/read.go` and `/internal/core/tx.go`, for a table whose key is one column. `Scan` is `ScanRange` without bounds. `conn.scan` in `/internal/core/cache.go` adds `where k >= ?1 and k < ?2` to its query and binds the bounds; `table.checkBounds` checks a bound as `Get` checks its key (`ErrInvalid`, `ErrTooBig`). `TestScanRange` in `/internal/core/cache_test.go` compares every pair of bounds (none, closed, open, at each edge value of each type, -0.0 too) with a filter of the full scan by the model's order, in a read and in a write transaction, and the refusals.

The plan: `Filter{Source, Conditions}` and `Condition{Column, Operator, Value}` in `/internal/engine/plan.go`, below the projection; `Format` prints `filter v1 >= ?1 and v2 IS NOT NULL`. `NewPlan` no longer panics for WHERE.

The operators, `/internal/engine/operator.go`: `filter` and `condition.holds` (three-valued, a comparison with NULL on either side is false for the WHERE), `compare` (integers and reals by number with `cmp.Compare`, so 0.0 equals -0.0; text with `strings.Compare`, which is memcmp and the shorter first; blobs with `bytes.Compare`; another kind is an internal error), and `keyAccess`, which gives the scan its `access`: no row for a comparison with NULL or `key is null`; `get` by the first `key = value`; `scanRange` from the tightest lower and upper bounds (`tighter`); every row otherwise. The filter tests every condition on every row the scan gives, so the access can only leave out rows that a condition rejects. `buildQuery` takes the arguments, the length limit, and `scanAll`, and a `build` struct holds them. The projection resolves its names before the filter's (SQLite's order). `build.filter` resolves the conditions with SQLite's walk, then checks the length of each value, then the type of each in the order of the text (`checkCompared` in `/internal/engine/errors.go`: `s4ql: not supported: a string literal compared with the column t.a of type INT`, `argument 1, which is text, compared with the column ...`).

`Options.scanAll` (set through `Options.ScanAll` in `export_test.go`) makes every query read every row; `TestDifferential` and `FuzzDifferential_core` take `-scanall`.

Removed: `checkWhere`, its calls in `engine.Check` and `prepare`, the `checkText` wrappers in the engine's and `drivercheck`'s tests, and the call of `engine.Check` in the driver's `check` in `/conn.go`.

The rules of the generated tests: `stmttest.ConditionRule` and `stmttest.MissingColumn` in `/internal/sql/stmt/stmttest/rule.go`; the engine's `typeRule`, `typeRefusal`, `checkType` (a select is refused by the rule in a read-only transaction too), and the simulation use them; `drivercheck`'s `typeRefusal` and `checkQuoted` too, the latter now for a name in double quotes in a condition.

Tests: `TestEngine_where` (the edges of each type against every operator, by the key and with every row, against SQLite; every shape of `TestEngine_columns` with one to three conditions; the errors; the types; the arguments; transactions; a replay), `TestFilter` (the calls of the fake for each access), `TestNewPlan`, `TestWhere` of the driver, a sequence "selects with WHERE" of `drivercheck`'s `TestSequences`, and `TestConditionRule`.

### Why

The core had `Get` already, and a bounded scan is a `where` on the query that the cache already prepares, with SQLite's own comparison of keys, so it was a small change that a model test covers completely. Keeping the filter over every access makes the access an optimization that cannot add a row, and the tests with `scanAll` show that it removes none.

### What worked

- The facts of the pass's diary held when I ran them in SQLite (a throwaway test with the oracle): `x is null and y is null` gives `y`, `x is null and y = 1` gives `x`, `x is null and a is null and y is not null and a = 2` gives `y`; the message in double quotes holds in WHERE; `rowid` and `oid` are `no such column`; a too-long argument is `string or blob too big` after the names; a too-long string literal in WHERE is `string or blob too big` (SQLite steps it as a constant, also on an empty table), and a too-long blob literal is NULL, so no rows.
- Two planted mutants (`<=` read as `<` in `condition.holds`, and an upper bound of `<=` taken as open) each failed `TestEngine_where` 50 times; `!b.Open` turned around in `conn.scan` failed `TestScanRange`.
- `TestEngine_where` matched SQLite on every query the first time, apart from my own lower bound for the number of rows.

### What didn't work

- A heredoc in Bash and a Python script were refused: "this command is too complex to verify that it stays inside the worktree". The Edit tool worked.
- `TestConn_differential` failed with `the engine does not support a statement that the pass supports: s4ql: not supported: argument 1, which is an integer, compared with the column left.viRtuAl of type REAL`: `pair.checkType` skipped the rule in every read-only transaction, which holds for an insert only.
- `drivercheck`'s `TestGenerated` failed with `no such column: "T1" - should this be a string literal in single-quotes?` against mattn's rows: mattn reads a name in double quotes as a string in WHERE too, so `checkQuoted` needed the name of SQLite's error, which `stmttest.MissingColumn` gives.

### What I learned

SQLite gives `string or blob too big` for a string literal in WHERE that is longer than a lowered limit even on an empty table, and turns a blob literal into NULL; s4ql gives the error for both, as for an insert.

### What was tricky

- The order of the names: SQLite resolves the select list before the WHERE, but the plan has the filter below the projection, so `build.node` of a `*Project` builds the scan under the filter first, then its own names, then the filter.
- The walk of a missing column under IS NULL: a test for NULL of an existing column after a missing one does not stop the walk; only a comparison does.

### What warrants review

- `keyAccess` and `tighter` in `/internal/engine/operator.go`, and `conn.scan` in `/internal/core/cache.go`.
- `build.filter`: the order of the errors.
- The pinned numbers that moved (step 2).

### Future work

- The documents and the full checks: step 2.

## Step 2: The pinned numbers, the documents, the checks, and the review

**Author:** where-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Keep the pinned numbers of those tests true; where one moves, explain why. [...] `make check-browser` must pass [...] Run `make test`, `make lint`, and `make check-browser`. [...] Review your own work with the code-review skill before you report, and fix what it finds."
**Interpretation:** Pin the numbers that moved with their reasons, have the documents written, run every check, and review the branch with two reviewers.
**Inferred intent:** A branch whose code, numbers, and documents agree, ready for the lead.

### What I did

Pinned numbers, with the generator drawing as before (only the two texts with WHERE of `stmttest.Others` reach the engine): `TestDifferential` `excusedType` 339 to 350; `drivercheck`'s `TestGenerated` `excusedPrepare` 1060 to 1063, `excusedType` 161 to 169, `excusedQuoted` 45 to 48. The comments beside the pins say why. The simulation's minimums held.

A writer sub-agent updated `/internal/engine/README.md`, `/docs/todo.md` (status, the decisions of slice 7, what is not built, the known difference of a long literal), and refinements under slice 7 in `/docs/roadmap.md` (status line untouched). I read the diff and committed it.

The review (two reviewers on `git diff 8d150cc`): neither found a wrong row or error; one ran 16,000 random selects and 20,000 random error cases, the other 24,000 and 20,000, all equal to SQLite. Fixed: the doc of `core.Bound` and `bound`, which described only a lower bound (both); `scan.rows` built the full scan before its switch, and `keyAccess` cleared the bounds for a key with no effect (both); `whereProbes` and `operators` were package-level and used in one test (both); the core's README named the engine (both); the `get` path's error was untested (one reviewer: removing `yield(nil, err)` passed every test) and `access.none` was unpinned (one): `fake` has `getFails` and `scanned` now, and `TestFilter` checks both; a too-long string literal as the upper bound of the key gives no rows in SQLite when the search reaches no row (one reviewer, which I confirmed), now a known difference in `docs/todo.md`; the vacuous `ErrTooBig` loop of `TestScanRange`; stale comments in `errors.go`, `simulation_test.go`, and the README of the pass. Left: `checkCompared` duplicates `checkType` (both reviewers) because another builder is mutation-testing the code of slice 4 to 6 and keys its equivalent mutants by function, so I did not move `checkType`'s body; the comment on `Options.scanAll` names the tests, as the one on `bugs` does; `-scanall` is in no CI job, now in the todo for the builder of the generated checks.

### Why

A moved pin needs its reason; the review's findings were cheap and, for the `get` error and the long literal, real.

### What worked

`make test`, `make lint`, and `make check-browser` passed. The new tests passed with `-count=20` and with `-race -count=20` (`TestEngine_where`, `TestFilter`, `TestNewPlan`, `TestScanRange`, `TestConditionRule`, `TestWhere`). After the fixes, `TestDifferential`, `TestSimulation`, `TestConn_differential`, and `drivercheck` passed again.

### What didn't work

`TestOpen_realBucket` fails without `-short` for want of credentials, as it must; `make test` runs with `-short`.

### What I learned

SQLite evaluates the upper bound of a search of the primary key only when the search reaches a row, so `k < 'long'` on an empty table gives no rows where `v < 'long'` gives `string or blob too big`.

### What was tricky

Seeing that the key access cannot add a row (the filter tests every condition) but can drop one, so the tests must compare it with a scan of every row, which `scanAll` gives.

### What warrants review

- The decisions of slice 7 in `/docs/todo.md`, and the refinements in `/docs/roadmap.md`.
- Whether `checkCompared` should merge with `checkType` once the mutation testing of slices 5 and 6 is merged.

### Future work

- WHERE in the generated tests, with `-scanall` in CI; the simulation with a planted bug for WHERE; mutation testing of slice 7; fuzz targets with WHERE.
