# Diary: the other types in the storage format and the storage core

Slice 4 of `/docs/roadmap.md` adds the column types `integer`, `real`, `text`, and `blob`, beside `int`, with exact types. This task brings them to the storage format (`/proto/s4ql/v1/commit.proto`, `/internal/format`, `/internal/s4qlpb`) and the storage core (`/internal/core`), with the tests of the core for every type. The statement pass is another builder's task; the engine and the driver come after both. The base is the branch `binding-types` (pull request #67), which adds `BindDouble`, `ColumnDouble`, `BindBlob`, and `ColumnBlob` to `/internal/sqlite`.

## Step 1: the types and the refusals of values in the storage format

**Author:** builder (types-core)

### Prompt Context

**Verbatim prompt:** You are a builder on s4ql (`maragu.dev/s4ql`): a Go SQL database whose truth is a log of commit objects in S3, with a local SQLite cache (pinned 3.54.1, own cgo binding), its own SQL engine, and a `database/sql` driver as the only public API. [...] **Your task is the storage format and the storage core.** [...] 1. **Format** (`proto/s4ql/v1/commit.proto`, `internal/format`, the generated code in `internal/s4qlpb`): `Type` gets values for `integer`, `real`, `text`, and `blob`, beside `int`. `int` and `integer` stay apart, so that replay declares a column as it was declared. [...] 2. **What the format's reader refuses in a value**, beyond what it refuses today (a NaN, text that is not valid UTF-8): text with a NUL byte, and a real that is infinite. [...] Decide, and write down in the README, what holds for `-0.0` [...] 3. **Core**: tables with one column of any of the five types. [...] 4. **Tests of the core for every type** [...] 5. An older cache, made before this change, holds only `int` tables and must still open. Decide whether the cache's version needs to change, and say why.
**Interpretation:** add four values to `enum Type`, refuse NUL text, infinite reals, and -0.0 where the format must, then make the core store, check, order, commit, and replay values of all five types, and test it all, including a planted bug for types.
**Inferred intent:** what a commit object says changed must be exactly what the cache holds, bit for bit, for every type, so that a replay into an empty cache reaches the same state.

### What I did

`/proto/s4ql/v1/commit.proto`: `TYPE_INTEGER = 2`, `TYPE_REAL = 3`, `TYPE_TEXT = 4`, `TYPE_BLOB = 5`, and comments on `Value.real` and `Value.text` that say what is never stored. `make generate` rewrote `/internal/s4qlpb/commit.pb.go`; `make lint-proto` passes.

`/internal/format/check.go`: `checkValue`, which runs on every value of a change and on every parameter that the writer is handed, now also refuses an infinite real and text that holds NUL. A new `checkStored` runs on the values of a row and of a primary key, and also refuses a real that is -0.0. A parameter of an annotation keeps -0.0 with its sign. The doc comments of `CommitReader` and `CommitWriter` say so, and the README of the package has three new points under "Keys and values": why `INT` and `INTEGER` are two values, why NUL, NaN, and infinities are refused, and the rule for -0.0.

Tests: the reader refuses each new case in a row and in a key, the writer refuses them in an upsert, a delete, and a parameter, the writer keeps -0.0 in a parameter, and the reader hands out a parameter that is -0.0, infinite, or text with NUL as it is (it does not check annotations). The sample `commit-v0-all.bin` gained a table `every` with a column of each type, two rows of edge values, a delete, and a parameter -0.0, written again with `-update`. A new subtest of `TestCommitSamples` fails if no sample holds some value of an enum. The fuzz input of the writer makes its values valid under the new rules.

### Why

The product owner decided that s4ql never stores text with NUL, a NaN, or an infinity, so a reader must never put one into a cache. -0.0 is refused in a row and a key because SQLite stores it in a `REAL` column as 0.0: a row or key in a commit object is what the cache holds, and a commit object with -0.0 would agree with a replayed cache only by SQLite's conversion.

### What worked

The existing table-driven tests of refusals took the new cases with one line each.

### What didn't work

Two tests used `s4qlpb.Type(2)` as a type that the reader does not know, and failed once 2 became `TYPE_INTEGER`:

```
--- FAIL: TestCommitReader/should_refuse_a_type_it_does_not_know_as_newer (0.00s)
--- FAIL: TestCommitWriter/should_refuse_a_column_of_a_type_it_does_not_know (0.00s)
```

They use `s4qlpb.Type(6)` now.

### What I learned

The reader does not check annotations at all, so the refusals of NUL and infinities in a parameter are the writer's only.

### What was tricky

Nothing beyond the one choice of where -0.0 is refused: in a row and a key, not in a parameter.

### What warrants review

- `/internal/format/check.go`: `checkStored` versus `checkValue`, and that the writer's annotation check uses only `checkValue`.
- The README's point on -0.0, which records a decision of detail.

### Future work

None from this step.

## Step 2: five types in the storage core

**Author:** builder (types-core)

### Prompt Context

**Verbatim prompt:** see step 1.
**Interpretation:** the core's integer-only places (`typeSQL`, `load`, `table.checkValue`, `bind`, `readRow`, the temporary tables of a commit) take every type, with exact types checked by the core, -0.0 stored as 0.0, and a value that is too long refused with an error of its own while the transaction goes on.
**Inferred intent:** the same as step 1.

### What I did

In `/internal/core/cache.go`: `typeNames` maps each `Type` to the declared type of the cache (`int`, `integer`, `real`, `text`, `blob`), and `typeOf` maps back for `load`, without regard to case, since SQLite reports the type of a strict column in capitals. `bind` binds reals, text, and blobs, and binds -0.0 as 0.0. `readRow` reads reals, text, and blobs. `table.checkValue` takes the length limit and checks exact types: an integer for `int` and `integer`, a finite real for `real`, valid UTF-8 without NUL for `text`, a blob for `blob`; a text or blob longer than the limit gets the new `ErrTooBig` from `/internal/core/errors.go`. `openConn` takes the `Options` and sets SQLite's length limit on every connection from the new unexported option `lengthLimit` (default 1,000,000,000), which the tests lower with `Options.SetLengthLimit` from `/internal/core/export_test.go`.

In `/internal/core/tx.go`: `Upsert` asks whether the row exists, writes it, and only then records the key; an `SQLITE_TOOBIG` from the write becomes `ErrTooBig` and does not fail the transaction. `Delete` deletes first and records the key only if the row was there. The temporary tables of a transaction changed from one table of keys with a column `existed` to two tables of keys alone: `s4ql_touched_N` with every key that the transaction touched, and `s4ql_existed_N` with those of rows that existed when it began. `spoolTouched` reads the touched keys with a `cross join`, so that SQLite sorts nothing. `Tx.committed` is where a row goes from the cache to the commit object, and where the new planted bug `blobCut` cuts a blob at its first zero byte.

The test helpers: `text` writes each kind exactly (`r-0` for -0.0, Go quotes for text, `x'..'` for blobs), `describe` names a type other than `int`, `cacheDump` reads every type, the model's `compareKeys` orders each kind as SQLite does, and the model stores -0.0 as 0.0 (`stored`). `edges` in `/internal/core/model_test.go` holds edge values for each type.

New tests: `TestScan_order` in `/internal/core/cache_test.go` compares the order of a scan, for each type, with the order of SQLite's own table of the same keys, and with the model's. `TestTx_types` in `/internal/core/tx_test.go` upserts, gets, deletes, commits, and replays rows of every type, with a column of every type beside the key, and compares the commit object with the model's net effect and both caches bit for bit with `cacheDump`; it checks -0.0, and the refusals of every wrong kind and of NaN, infinities, invalid UTF-8, and NUL. `TestTx_tooBig` runs with a limit of 1000 and tries every length from 980 to 1002 in four shapes of table, in a table that the transaction created and in one that existed before it, and requires `ErrTooBig` exactly where SQLite's own table of the same shape, under the same limit, refuses the row.

### Why

The core's own check of exact types is needed because a strict table of SQLite converts: the integer 1 into a `real` column becomes 1.0. The order of the write and the record of the key, and the tables of keys alone, are needed so that the core refuses a row as too big exactly where SQLite does, and so that a refused row leaves no trace in the commit object.

### What worked

`TestTx_tooBig` found the one real problem of this step (below) on its first run.

### What didn't work

The first version kept one temporary table with the column `existed`. That column makes the record of a key one byte larger than the record of the row in a one-column table, so a row at the limit would be taken by SQLite's own table and refused by the core. Moving to two tables of keys, with a `union all` in `spoolTouched`, made the commit fail with:

```
tx_test.go:2258: Expected nil error, but got "commit: sqlite: string or blob too big (code 18)" (type *fmt.wrapError)
tx_test.go:2258: a text key, in a table that existed before: true
```

The `union all` with `order by` makes SQLite sort, and a sorter holds the key and the row in one record, which exceeds the limit for a row near it. The design with `s4ql_touched_N` and `s4ql_existed_N` and the `cross join` reads the keys in the order of their table and sorts nothing.

Recording the key only after a successful write first broke the order of the tables in the commit object: the model counts a delete of a row that is not there as a change of the table, and the commit object then listed tables in another order than the model.

```
workloads_test.go:512: seed 5: the log has commit 12 of transaction 330111375477072285 with the changes ["drop TBL 2;--" "upsert t0 [0] 0"], and the caller made changes with the net effect ["upsert t0 [0] 0" "drop TBL 2;--"]
```

`Delete` now marks the table as changed also when it finds no row.

The test of a cache that another process changed used a `text` column as the type that s4ql does not make, and expected a stop; it uses `any` now.

### What I learned

- SQLite reports the type of a strict column in capitals in `pragma_table_info`, as `cacheDump` already assumed for `INT`.
- `insert or ignore ... returning 1` gives a row only when it inserted one, which `Tx.touch` uses to tell the first touch of a key.
- In the builder of `s4qlpb.Value`, a nil `Blob` is no value, so an empty blob must be an empty slice that is not nil; `ColumnBlob` gives one.

### What was tricky

The length limit counts the whole record of a row, header included, so the boundary is a few bytes below the limit and depends on the shape of the table. Matching SQLite there needs every record that the core writes to be no larger than one that SQLite's own table writes for the same row, and that holds for the temporary tables only because they hold the key alone.

### What warrants review

- `/internal/core/tx.go`: `Tx.Upsert`, `Tx.Delete`, `Tx.touch`, and `Tx.spoolTouched`. The `cross join` is what keeps the query from sorting; `TestTx_tooBig` fails without it.
- That a statement that SQLite refuses with `SQLITE_TOOBIG` leaves the transaction usable: `TestTx_tooBig` refuses replacements of an existing row and checks that the old row stays.
- `bind` maps -0.0 to 0.0; SQLite stores 0.0 anyway, so this is a second guard and no test can tell the two apart.

### Future work

None yet; the property test and the simulation come next.
