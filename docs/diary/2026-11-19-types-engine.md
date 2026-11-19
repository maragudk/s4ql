# Diary: Slice 4 of the roadmap in the engine and the driver, the other types

Slice 4 adds the column types `integer`, `real`, `text`, and `blob`, and real, string, and blob literals and arguments. The binding, the commit format and the storage core (pull request #68), and the statement pass (pull request #69, branch `types-stmt`) hold them already. This task makes `create table`, `insert`, and `select` work for all five types through the engine (`/internal/engine`) and the driver (the root package), and removes the temporary refusal of the types in the engine. The branch is `types-engine`, from `origin/types-stmt`.

## Step 1: The types in the engine and the driver, and the tests that compare them with SQLite

**Author:** types-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Make `create table`, `insert`, and `select` work end to end, through the engine and through `database/sql`, for all five column types [...] **Exact types:** the value must have the type of the column [...] Any other pairing is rejected with an error of s4ql's own that matches `stmt.ErrNotSupported` [...] Decide where this check sits relative to SQLite's own errors by this rule: an error that SQLite gives before it looks at the value (no such table, a syntax error) comes first. [...] **Too long:** `core.ErrTooBig` becomes SQLite's error for it: result code 18 [...] Tests must not allocate a gigabyte: find a way to lower the limit in tests of the engine and the driver [...] **Remove what was temporary:** `checkTypes` and the second `engine.Check` in the driver's `check` [...] Keep the pinned numbers of the existing generated tests [...] true; where one moves, explain why in your report."
**Interpretation:** Plan and run the five types and the four kinds of literal, take reals, text, and blobs as arguments, check exact types after the names, give SQLite's `string or blob too big`, refuse the values that the core would refuse, and test it all against SQLite by hand and in a table of edge values, keeping the generated tests true.
**Inferred intent:** The engine and the driver hold the types of slice 4 with the same trust as slices 1 to 3, so that the next builder can extend the generator and the heavier checks on top.

### What I did

`/internal/core/core.go`: the unexported `lengthLimit` of `core.Options` is now the exported `LengthLimit`, checked to be from 30 (SQLite's smallest) to 1,000,000,000, and `DB.LengthLimit` gives it. `SetLengthLimit` in `/internal/core/export_test.go` is gone; the core's tests set the field.

`/internal/engine/plan.go`: `Real`, `Text`, and `Blob` expressions, `columnType` from `stmt.Type` to the format's type (INT and INTEGER apart), `typeName` for all five, and `Format` of each literal. `/internal/engine/errors.go`: `checkTypes` is gone. `checkArguments` refuses, before the database, NULL, a value of no kind, NaN, an infinity, text that is not valid UTF-8 or holds NUL (`s4ql: not supported: argument 1, which is a real that is not a number`, and so on). `checkLength` gives `tooBig()` (18/18 `string or blob too big`), and `checkType` gives `s4ql: not supported: argument 1, which is text, for the column t1.v1 of type INT` or `s4ql: not supported: a string literal for the column t1.v1 of type INT`. `/internal/engine/operator.go`: the sink interface has a `check` step; `buildSink` checks the length of every value after the names; `insert.check` checks the types. `/internal/engine/engine.go` and `/internal/engine/tx.go` call `check` after `buildSink` and before the count of changes is set, and not at all in a read-only transaction, which refuses first. `/internal/engine/storage.go`: `fromCore` turns `core.ErrTooBig` into `tooBig()`, for a row whose record is above the limit while its value is not; `database` has `lengthLimit()`.

The driver: `/conn.go` takes `float64`, `string`, and a non-nil `[]byte`, refuses a nil `[]byte` as `argument 1 of type []byte, which is nil`, and `check` calls only `stmt.Check`. `/rows.go` copies every blob, so an empty blob is `[]byte{}` and the application owns its bytes.

Tests: `/internal/engine/engine_test.go` has `TestEngine_types` (the edges of each type, by literal and by argument, each twice, against SQLite, then a replay; INT and INTEGER apart; -0.0; the blob copy) and `TestEngine_tooBig` (with the cache and SQLite at a limit of 1000: every length from 988 to 1002 by argument, lengths up to the limit by literal, the order of the errors also in a read-only transaction, and a transaction that goes on). `/internal/engine/tx_test.go` has three `pinTx` cases with typed values. The test double `/internal/engine/memory_internal_test.go` holds every type, with keys compared as SQLite compares them. `/conn_test.go` has `TestTypes`. `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go` has `TestTypes`: the edges of each of the five types through literals, arguments, and a prepared statement run many times, pinned and through the pool, against mattn, with a replay into an empty cache.

The generated tests had to learn the new order: an argument of type string, float, or blob is no longer refused before the database. The engine's `checkArguments` rule (`/internal/engine/differential_test.go`) now refuses only what no column holds, and a new `pair.checkType` checks a value of another type than its column against the rule, after SQLite has prepared the text; everything else, such as `no such table` for such an argument, is compared with SQLite, which now binds the argument as its type. The driver's check has `argumentRefusal`, `reaches`, `pair.checkRefused`, and `pair.typeRefusal` in the same way. The engine's simulation accepts the type refusal of an argument and requires that no such argument succeeds, since `stmttest`'s tables are INT.

### Why

The rule of the brief puts the check of the type after the errors of the names. SQLite's own order for an insert is: the names when it prepares, the length of an argument when it binds, `pragma query_only` at `OP_Transaction`, then the type at `OP_TypeCheck`, then the record and the key. The engine follows it, so wherever SQLite gives an error of its own that s4ql reproduces, it is the same error.

### What worked

The table of edges passed against SQLite on its first run, in the engine and through the driver, so the core's handling of the types and -0.0 is right as the engine uses it.

### What didn't work

- The first run of `TestDifferential` after moving the check failed with `SQLite counts 1 changes, and the engine's results 0`: the type check was inside `insert.run`, after the result was set to count 0 changes. A "not supported" must leave the count as every other refusal does, so the check became the sink's own step before the count.
- `TestSimulation` failed on `xsqlite_ (database INT)` against `xsqlite_ (database INTEGER)`: `coreState` printed every column as INT; the INTEGER tables of `stmttest.Others` now are created.
- Bash commands with a heredoc or a multi-line `sed` replacement were refused by the sandbox ("this command is too complex to verify that it stays inside the worktree"); the Edit tool worked.
- `json.RawMessage` as a named `[]byte` in a test would need `encoding/json`, which the project forbids; a local named type took its place.

### What I learned

- SQLite turns a blob literal that is longer than `SQLITE_LIMIT_LENGTH` into NULL silently (`OP_Blob` ignores the result of `sqlite3VdbeMemSetStr`), and gives `string or blob too big` for a string literal only when it steps. With the default limits a literal can never be longer than the limit, because `SQLITE_LIMIT_SQL_LENGTH` is the same 1,000,000,000 and the text holds the literal, so SQLite gives `statement too long` first. s4ql has no limit on the length of the text.
- `sqlite3_limit` lowers `SQLITE_LIMIT_LENGTH` alone; `SQLITE_LIMIT_SQL_LENGTH` stays.

### What was tricky

- The pinned numbers of the driver's check moved for two reasons at once; a throwaway count of the excused calls that had a string, float, or blob argument separated them (see "What warrants review").
- `s4qlpb.Value_Kind_case` is not exported, so the engine compares kinds through `HasInteger` and the like.

### What warrants review

- The order of the checks in `buildSink`, `insert.check`, and `writeIn`'s read-only branch; `TestEngine_tooBig` pins it against SQLite.
- Pinned numbers that moved. `TestDifferential` of the engine: "an argument that is not an integer" (85) is now "an argument that no column holds" (50) and "a value of another type than its column" (10); the other 25 calls with a string, real, or blob argument met SQLite's errors of the names and are compared with SQLite. `TestGenerated` of the driver: `excusedArgument` 109 to 85, new `excusedType` 8, `excusedPrepare` 1051 to 1064, `excusedOutside` 43 to 46, `excusedRowsOpen` 44 to 45. Of the 24 typed calls, 8 are type refusals, 7 are `excusedPrepare`, 1 `excusedOutside`, 1 `excusedRowsOpen`, and 7 are compared with SQLite. The other 6 of `excusedPrepare` and 2 of `excusedOutside` come from the INTEGER create tables of `stmttest.Others`, which before were "not supported" and now create tables that later statements find.

### Future work

- Types in `stmttest`, the generated differential tests over all types, the simulation with a planted bug for types, mutation testing, and fuzz targets, as the brief leaves them.
- s4ql gives no `statement too long` for a text above 1,000,000,000 bytes, where SQLite does.
