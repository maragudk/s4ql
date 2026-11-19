# Diary: A differential test of the driver against `mattn/go-sqlite3`

A differential test at the level of `database/sql`: the same texts run through the `*sql.DB` of s4ql and through the `*sql.DB` of SQLite, and everything that an application can observe is compared. The engine is compared with SQLite below the driver already, so this test is about what the driver adds or could break. The work is on the branch `driver-differential`, which starts from `main` at `66f8463`, the merge of the driver.

## Step 1: The brief, and a plan

**Author:** driver-differential builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "A differential test at the level of `database/sql`: the same sequence of SQL texts runs through `*sql.DB` of s4ql and through `*sql.DB` of SQLite, and everything an application can observe is compared. The engine is already compared with SQLite below the driver; this test is about what the driver adds or could break: `sql.Result` (`RowsAffected`, `LastInsertId`), the rows as `database/sql` scans them (values and Go types through `any`), column names, `ColumnTypes` (the database type name, and whatever else both sides report), the errors (for a SQLite error: code, extended code, and message through `*s4ql.Error` against the oracle's error; for "not supported": that SQLite accepts the text, so it is no syntax error, and that s4ql's error matches `s4ql.ErrNotSupported` and carries no SQLite code), and which calls fail (`Exec` against `Query` against `Prepare` and then `Exec`/`Query` on the statement, and `QueryRow`). [...] What to build: 1. A table-driven differential test with hand-written sequences for slice 1 [...] 2. A generated differential test [...] reporting counts by outcome so that the test fails if a class of outcome is never reached [...] 3. A fuzz target over a sequence of SQL texts [...] 4. If the test finds a difference between s4ql and SQLite, do not fix the engine or the driver unless the fix is small and plainly right: write a minimal reproducing test, mark it as you find the project does for known faults, and report it."
**Interpretation:** Choose an oracle and a home for the test, then build three layers: hand-written sequences, generated sequences with counts by outcome, and a fuzz target. Share the engine's classification of known differences instead of copying it, and pin whatever escapes comparison by exact count.
**Inferred intent:** Trust that what an application sees through `database/sql` is what it would see from SQLite, beyond what the engine's own tests show.

### What I did

Read `/AGENTS.md`, `/docs/spec.md`, `/docs/roadmap.md`, the driver's points in `/docs/todo.md`, the root package, `/internal/engine/README.md`, `/internal/engine/differential_test.go`, `/internal/sql/stmt/stmttest`, `/internal/sqlite/crosscheck`, `/internal/store/s3store/fake_test.go`, `/Makefile`, and `/.github/workflows/ci.yml`. Read the source of `mattn/go-sqlite3` v1.14.53 for what its driver does with results, rows, column types, and errors.

The plan:

1. **The oracle is `mattn/go-sqlite3`, and the test lives in the cross-check module,** in a package of its own, `/internal/sqlite/crosscheck/driver`. What was settled there: mattn bundles the same SQLite, 3.54.1, and `TestVersion` and `TestSource` in the module fail unless its C code is byte for byte the vendored amalgamation; the binding's SQLite has only local symbols, so both copies link into one binary, which `TestLink` shows. So the messages are the pinned SQLite's, and no excuse for versions is needed. mattn is a `database/sql` driver that we did not write, so it judges what the driver adds: `RowsAffected` from `sqlite3_changes`, `LastInsertId`, column names, `sqlite3_column_decltype`, the Go types of values, and which call fails. A driver over our own binding would have judged our driver with our own reading of `database/sql`.
2. **s4ql is opened as an application opens it,** with `s4ql.Open` on a fake S3 server over plain HTTP in the test: the real `s3store`, core, engine, and cache, and no seam in production code. The seam in `/export_test.go` is not visible from another module. The production defaults of the core then hold: a lease of 30 seconds, renewed after 20, and a busy timeout of a second, which only another process on the cache or a bug of s4ql can reach. Each sequence opens an instance of its own and closes it, so a sequence would have to take 20 seconds for the lease to matter.
3. **The compile options of mattn differ from the binding's,** as `TestCompileOptions` lists: double-quoted strings, foreign keys, the math functions, and `mmap_size`. None should be reachable in slice 1: a double-quoted string as a value is not supported by s4ql whatever the tables hold, and a text that s4ql does not support is not run in the oracle.
4. **The engine's sequence and its classification move to `stmttest`,** so that both tests share them: the generator of sequences with its spellings and its `others`, the known gaps of `stmt`, and the accepted difference of a syntax error after the start of a table that exists. The engine's tests keep their pinned count of 34.
5. **A connection's state.** A sequence runs on a `*sql.Conn` on each side, where `RowsAffected` is compared after every statement, and through the pool, where it is compared only for a statement that sets it.

### Why

The brief asks for the oracle and the home of the test to be chosen and recorded, and for the engine's classification to be shared rather than copied.

### What worked

Reading mattn's source answered most questions before any code: its `Query` takes the first step at once, so a failing insert through `Query` fails at `Query`, as s4ql's does; its `Prepare` runs `sqlite3_prepare_v2`, which looks tables up, where s4ql's `Prepare` checks only the text.

### What didn't work

Nothing yet.

### What I learned

mattn's `Prepare` fails with `no such table`, `already exists`, and `has no column named`, because SQLite raises them while it prepares; s4ql's `Prepare` checks the text only, as the lead decided, and the same error comes from `Exec` or `Query` on the statement. That is a difference of which call fails, decided on purpose, and the test must count it apart and pin it.

### What was tricky

mattn's `Exec` runs every statement of a text, and its `ColumnTypeScanType` and `ColumnTypeNullable` are its own choices, not SQLite's. s4ql does not support a second statement, and implements only `ColumnTypeDatabaseTypeName`, so neither is compared.

### What warrants review

The choice of the oracle, and the claim that no compile option of mattn's is reachable in slice 1.

### Future work

None yet.
