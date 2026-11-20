# Diary: `default` with a literal in the commit format and the storage core

Slice 11 of `/docs/roadmap.md` lets a column carry `default <literal>`, the literal an integer, a real, a text, a blob, or NULL. The statement pass is built (pull request #109, `default-stmt`). This task carries the default through the commit format (`/proto/s4ql/v1/commit.proto`, `/internal/format`, `/internal/s4qlpb`) and the storage core (`/internal/core`): in the schema, faithfully, everywhere the schema lives. The engine fills in a left-out column with its default and hands the core whole rows, as it does for NULL; that is the next builder's, from this branch. The branch is `default-core`, from `origin/default-stmt` with `origin/main` merged in (nothing new on `main`).

## Step 1: the field `default` of a column, and its refusals in the format

**Author:** default-core builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Task: defaults in the commit format and the storage core. Only `proto/`, `internal/format`, `internal/core`. [...] 1. Commit format (`proto/s4ql/v1/commit.proto`, `internal/format`): `Column` gets an optional default that is a value of the kind rows hold (reuse the value message), with presence, so that \"no default\" and \"default null\" differ. Validation on read as strict as the rest: a default whose type is not the column's type is refused as malformed [...]; decide and say what holds for a null default on a column that does not take null and on a key column [...], for a real that is infinite or NaN, for a text that is not valid UTF-8, and for a value over the length limit. Negative zero: SQLite reads `-0.0` back as 0.0 after an insert; decide what the format stores and say why. 2. The cache: the schema that the cache gives back after a close and open, and that a second instance gets by replay, has every column's default exactly (presence, type, value). [...] The cache's layout version rises; `conn.load` refuses a table whose default it cannot read back. 3. `Tx.CreateTable` takes the default with the column; `Tx.Table`/`Tables` and whatever the engine reads the schema through give it back [...]. 4. Core does not apply defaults to rows [...]. 5. Tests in the packages' style [...]. 7. Decisions that the agreement does not settle: by SQLite's behaviour and the agreed rules, each recorded in `docs/todo.md` in a group \"decisions of the core step of slice 11, for the lead to confirm\"."
**Interpretation:** a field `Value default = 4` on `Column`, checked by the reader and the writer as a stored value of the column's storage class or NULL; the cache declares it in SQLite in a canonical form that it parses back strictly, with nullability from `notnull`; layout version 4; tests at every place the schema lives.
**Inferred intent:** a table that says DEFAULT reads back from the log and from the cache exactly as it was created, with no way for the cache or a replay to lose the difference between no default and DEFAULT NULL.

### What I did

`/proto/s4ql/v1/commit.proto`: `Value default = 4` on `Column`, with a comment that says what a reader refuses and what is allowed. A message field has presence in edition 2024, so `HasDefault` tells no default from DEFAULT NULL. `make generate` and `make lint-proto` passed; `default` is a field name that buf and protoc-gen-go take (`GetDefault`, `HasDefault`, `SetDefault`, `ClearDefault`, `Column_builder.Default`).

`/internal/format/check.go`: `checkCreateTable` calls the new `checkDefault` for a column with a default; it runs `checkStoredValue` (split out of `checkStored`, which a row and a key use) and then refuses a value of another storage class than the column's type, NULL apart. The doc comment of `CommitReader` in `/internal/format/reader.go` lists the new refusals.

The decisions, recorded in `/internal/format/README.md`:

- A default of another storage class than its column is malformed; NULL is a default of any column, also one that does not take NULL and a primary-key column, as SQLite takes both and the pass supports them.
- A real that is NaN, infinite, or -0.0, and text with NUL, are refused as in a row; text that is not valid UTF-8 is refused everywhere already.
- -0.0: refused, since a default is a value that a column stores, and SQLite stores it as 0.0. The core writes a -0.0 that it is handed as 0.0 (step 2).
- Length: the format has no limit on a value, in a row or as a default; the core refuses a table whose statement in the cache is over its limit, and that statement holds the default.

Tests in `/internal/format/reader_test.go`: two refusals as newer (a default of an unknown kind, a default with an unknown field), and twelve as malformed: no kind, each pair of a wrong storage class (real for INT, text for INTEGER, integer for REAL, blob for TEXT, text for BLOB), NaN, infinite, -0.0, text with NUL, and text that is not valid UTF-8 (raw bytes). The sample `commit-v0-all.bin` now holds `kv.c`, which takes NULL, with DEFAULT NULL, and the table `every` with a default for each column, `-1`, `0`, `0.5`, `'it''s'`, and NULL on the BLOB column that does not take NULL (`everyDefault`); it was written again with `go test ./internal/format -run TestCommitSamples -update`, and the test that every field is held by some sample sees `Column.default`. `/internal/format/writer_test.go`: four refusals, a round trip of three tables with every kind of default (the integer edges, the smallest subnormal as a negative real, a text with quotes, a double quote, a newline, a tab, and multi-byte characters, a blob with 0x00, 0xff, and a quote, the empty blob, the empty text, NULL on a key column and on a column that does not take NULL, DEFAULT NULL beside no default on columns that take NULL), and `FuzzCommitWriter` gives each column a default by the bits of a byte, of any kind, with the column's type made the value's.

### Why

The field reuses the message of a value so that a default is checked by the same code as a row, and so that the core and the engine pass it on as a value. Presence comes for free with a message field.

### What worked

`go test ./internal/format` passed after the update of the samples, and `go test ./internal/format -run '^$' -fuzz FuzzCommitWriter -fuzztime 20s` ran 96,053 executions without a failure.

### What didn't work

My first messages expected the kind as a number (`kind 2`); `%v` of the generated case type prints its name, and the error says `is a value of kind real, which a column of type TYPE_INT does not hold`. The tests take the names.

The generated case type of `Value.kind` (`case_Value_Kind`) is unexported, so a variable of it cannot be declared by name; `checkDefault` starts from `want := s4qlpb.Value_Null_case` and lets the type be inferred.

### What I learned

`default` is a keyword of the protobuf language only in a field option (`[default = ...]`), and is a valid field name.

### What was tricky

Nothing in this step.

### What warrants review

- `checkDefault` and `checkStoredValue` in `/internal/format/check.go`.
- The three new points of `/internal/format/README.md` under "Keys and values", which hold the decisions for the lead.

### Future work

- The core: step 2.
