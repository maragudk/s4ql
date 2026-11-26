# Diary: the driver owns the bytes of a blob argument

The adversarial review of 2026-11-26 found that the driver kept the caller's `[]byte` of an argument as the value of a parameter, so a query or a RETURNING answered for bytes that the caller changed after `Query` returned. This task fixes that and looks for the same class of fault in the other directions.

## Step 1: the copy, and the search for the same fault elsewhere

**Author:** builder (blob-argument)

### Prompt Context

**Verbatim prompt:** "An adversarial review found today that the driver keeps the caller's `[]byte` of an argument instead of a copy: a `select` or a `returning` answers for bytes that the caller changed after `Query` returned. [...] Fix it: an argument's bytes are the driver's own from the moment the call [...] receives them. Find where the copy belongs so that one place covers every path; do not copy more than once per argument. Look for the same class of fault in every direction, and write a test for each place before you decide it is sound or fix it" (abridged; the full brief names the write before `Commit` first, then the rows handed to the caller, then values kept in the engine's and the core's structures).
**Interpretation:** copy the bytes at the one place every path goes through, and show by tests where else caller-owned bytes could leak in or driver-owned bytes leak out.
**Inferred intent:** trust in correctness: no value that s4ql answers for or stores may depend on what the caller does with its buffers after a call.

### What I did

Brought `TestAdversarial_blobArgument` from the branch `adversarial-codebase` into `/conn_test.go` as the first two subtests of `TestArgument`, and widened the second to four forms of the argument (a `[]byte`, a type whose kind is a slice of bytes, a `sql.RawBytes`, a `driver.Valuer` of a `[]byte`) and four paths (directly, prepared, in a transaction, prepared in a transaction). Added a third subtest for the write before `Commit`: inside one transaction, an `Exec`, a prepared `Exec`, an `Exec` of two statements, and a `Query` with RETURNING, each with a buffer that the caller overwrites after the call returns; after `Commit` it checks the cache, the annotations of the commit object, and a second instance that replays the log. Added `TestRows_blob` in `/rows_test.go` for the rows handed out: a select and a RETURNING, in a transaction and outside one, scanned into a `[]byte`, a `sql.RawBytes`, and an `any`, with the `RawBytes` and the `any` overwritten by the caller, and the `[]byte` of each row checked after the next row, the close, and later updates.

All three subtests of `TestArgument` failed at `main` (`Expected "1", but got "2"` for the RETURNING, `Expected "10", but got "20"` for the select). The fix is `bytes.Clone(v)` in `argument` in `/conn.go`, which `arguments` calls once per argument for `Exec`, `Query`, both of a prepared statement, in a transaction and outside one; database/sql's own conversion (`driver.DefaultParameterConverter`) passes a `[]byte`, a `RawBytes`, and a slice kind over the same array, so all of them arrive there.

### Why

The engine reads the `*s4qlpb.Value` of a parameter lazily: the plan of a select evaluates its condition while the rows are read, and the rows of a RETURNING hold the argument's value until `Next`. SQLite copies at bind (`SQLITE_TRANSIENT`), which is why the stored row was right and the answered row was wrong.

### What worked

The write before `Commit` was already sound at `main`, before the fix: the core writes every change into SQLite (temporary tables of touched keys, read back at commit) and serializes an annotation into its spool in `Tx.Annotate`, during the call. The rows were already sound too: `value` in `/rows.go` copies each blob, and `internal/sqlite`'s `ColumnBlob` uses `C.GoBytes`. The engine's `Conn` and `transaction` keep no values across calls beyond the rows of the last query, and its events log no values.

### What didn't work

The first version of the select test hung: `go test -count=1 -run '^TestArgument$' .` ran past the 120 s timeout. Each "in a transaction" case began a write transaction with `begin` and left it open until the test's cleanup, so the next case's `BeginTx` waited for good. Before the fix the first case failed, so the hang only showed after it. Each case now rolls back its transaction. `TestRows_blob` hung the same way when a RETURNING outside a transaction ran beside an open write transaction; only the cases in a transaction begin one now. Then it failed with `Expected "1", but got "2"` for "a RETURNING": an update gives its rows in the order it draws them, so the test sorts the bytes before it compares.

### What I learned

The annotation's text of a statement of several in one `Exec` keeps its `;`: `insert into r values (3, ?); with x'03'`.

### What was tricky

Telling a fault of aliasing from a fault of the test: the unordered RETURNING looked like a changed byte until the values were sorted.

### What warrants review

`argument` in `/conn.go` and the comment on why it copies; the three subtests of `TestArgument` in `/conn_test.go`; `TestRows_blob` in `/rows_test.go`.

A review of the diff at the level low (`/code-review low main...HEAD`) found nothing: the nil `[]byte` is refused before the copy, and `bytes.Clone` of an empty slice that is not nil gives one that is not nil, so the empty blob stays a blob. `make test`, `make lint`, and the two new tests with `-count=50`, also under `-race`, passed.

### Future work

None found in `internal/core`.
