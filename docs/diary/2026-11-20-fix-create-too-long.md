# Diary: A create table that is too big for SQLite's schema under the length limit

CI's job `asan-fuzz (1)` failed on pull request #92 (run 44743824025) with a finding of `FuzzDifferential_core` in `internal/engine`: a create table with two names of 200 bytes, under the length limit of 1000 that the target lowers, for which the engine gave SQLite's raw `sqlite: string or blob too big (code 18)` from the core. The fault was on `main` too. The branch is `fix-create-too-long`, from `origin/main`.

## Step 1: Find the cause, measure SQLite, and fix it in the pass, the engine, the core, and the binding

**Author:** fix-create-too-long builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Fix the cause, not the test: s4ql must answer such a text as SQLite does (find out from SQLite itself what it gives for a declaration, a table name, and a column name over the limit, with the limit lowered as the target lowers it), in every layer the text passes (statement pass, engine, core, cache, replay: a table whose declaration the cache refuses must not reach the log). If the open todo item is this fault, the fix closes the item: delete it from `docs/todo.md`. Add the failing input to the target's seed corpus [...], a table test that pins the case at each layer you touched, and run the fuzz target locally for some minutes afterwards".
**Interpretation:** Reproduce from the log, find out what SQLite's rule for a create table under the limit is, give that answer in the engine, and make the cache hold every table that SQLite takes, or refuse it cleanly.
**Inferred intent:** The fuzz targets of CI pass again, and #92 can land, with s4ql answering as SQLite does.

### What I did

The log printed the corpus file in full (`uint64(342)`, `byte('9')`, `bool(true)`, `bool(true)`: a seed of the generator, not a text), so `/internal/engine/testdata/fuzz/FuzzDifferential_core/188a29aaa6925c1d` is that file, and it failed on `origin/main` as on `index-stmt`.

The cause. SQLite takes the text of the input: with `.limit length 1000` the sqlite3 shell creates it, and keeps 494 bytes in `sqlite_schema`. The cache does not: its own statement, `create table main."X…" ("T1" integer not null, "x…" int, primary key ("x…")) strict, without rowid`, is longer, and `sqlite3EndTable` writes a table into the schema with `UPDATE %Q.sqlite_master SET type='%s', name=%Q, tbl_name=%Q, rootpage=#%d, sql=%Q WHERE rowid=#%d`, which `sqlite3NestedParse` formats with `sqlite3VMPrintf` under `SQLITE_LIMIT_LENGTH` and refuses with `SQLITE_TOOBIG` if the text does not fit. So the name twice and the statement must fit in the limit, not only the statement.

SQLite's rule, measured with the binding on Linux in Docker (`golang:1.27.3`): for a long table name, a long column name, single quotes in the name, a comment at the end with and without a semicolon, and other shapes, SQLite refuses exactly when that UPDATE is 1000 bytes or longer: 96 bytes, the name twice, and the kept text, each in quotes, with a single quote doubled. The kept text is `CREATE TABLE ` and the text from the name of the table to the semicolon, or to the end of the text when there is none (`pParse->sLastToken` of a table with options).

The fixes:

- `/internal/sql/stmt`: `CreateTable.SQL`, the text that SQLite keeps, from `recognize.go`; the generator of `stmttest` writes it for its statements; `checkCreateTable` of the pass's tests compares it with `select sql from sqlite_schema` for every create table that the corpus, the grammar, and the generator check.
- `/internal/engine`: the plan's `CreateTable.SQL`, and `schemaTooBig` in `errors.go`, which `buildSink` checks after `columnsError`, where SQLite meets it, at the end of the parse. The README's order of the errors says so.
- `/internal/core/cache.go`: `conn.create` refuses with `ErrInvalid` a table whose statement in the cache is longer than the limit, and otherwise runs it under SQLite's largest limit and lowers the limit again. `createSQL` declares a primary key of one column with the column, so that its name is not in the text twice. `Tx.CreateTable` gives `ErrInvalid` without failing the transaction, as its doc comment promised. Replay goes through `conn.create` too.
- `/internal/sqlite/options.h`: `SQLITE_WITHOUT_ZONEMALLOC`. See below.

Tests: `TestConn_SetLengthLimit` in `/internal/sqlite/sqlite_test.go` pins the boundary for three shapes; `TestTx_tooBig` in `/internal/core/tx_test.go` pins `ErrInvalid` for three statements longer than the limit, and three tables that the old code refused, after a restart and a replay; `TestEngine_tooBig` in `/internal/engine/engine_test.go` compares 86 texts around the boundary with SQLite, the order of the errors, and the longest table name after a restart and a replay.

### Why

The core's refusal came from the cache's own statement, not from the user's, so a fix in the engine alone would have refused a table that SQLite takes. And the engine must give `string or blob too big` where SQLite does, or the generated tests, whose names reach 200 bytes, find that next.

### What worked

- Reading `sqlite3EndTable`, `sqlite3NestedParse`, and `sqlite3StrAccumEnlarge` in `/internal/sqlite/c/sqlite3.c` gave the formula, and the measurement on Linux matched it to the byte for six shapes (smallest refused 1000, largest taken 999 or below).
- After the fix, `go test -run '^$' -fuzz '^FuzzDifferential_core$' -fuzztime 5m ./internal/engine` on macOS ran 58,713 executions without a failure. `make test`, `make lint`, and `make check-browser` passed. In Docker on Linux, `go test -asan -short ./internal/sqlite ./internal/engine` and `make fuzz FUZZDIRS=internal/engine FUZZFLAGS=-asan FUZZTIME=30s`, the ten fuzz targets of the engine, passed.
- The check of `CreateTable.SQL` against SQLite fires: with the end moved past the semicolon, 15 checks of `internal/sql/stmt` failed.

### What didn't work

- On macOS the boundary was not exact: SQLite took texts of up to 1023 bytes, and the smallest it refused was between 1015 and 1029, by shape. With SQLite's default allocator on Apple, the zone allocator, `sqlite3StrAccumEnlarge` takes the size of the allocation from `malloc_size`, which rounds up to 16 bytes, and to 512 above 1008, so the buffer can hold more than the limit without a new allocation and its check. My simulation of that growth still disagreed with SQLite in 33 of about 5,000 cases, so it cannot be predicted from outside. On Linux SQLite uses its own wrapper of malloc with a size rounded to 8 bytes, and with a limit that is a multiple of 8 the check is exact. `SQLITE_WITHOUT_ZONEMALLOC` gives macOS the same wrapper; with it, the engine's test of 86 texts passed on macOS, where without it the fuzz input itself failed as `18/18: string or blob too big` from the engine and a table from SQLite. SQLite does not report the option, so `TestCompileOptions` skips it, with a comment.
- A schema row longer than the limit cannot be read: a cache made with a statement of 3,112 bytes, opened again under a limit of 1000, gives `string or blob too big` for every statement. So raising the limit only for the create is not enough; the statement itself must fit, which is why `conn.create` refuses a longer one.
- My first test of a key of two columns of 300 bytes had a statement of 1,282 bytes in the cache, and `ErrInvalid`; two of 200 fit.
- In the engine's test, the name for the drop was cut out of the text before the check for the fuzz input, which starts with a comment: `slice bounds out of range`. The longest table name that SQLite takes is 280 bytes on Linux; 288 was the macOS value.

### What I learned

SQLite's answer to a create table under a lowered limit depends on its allocator, and the binding now fixes the allocator. The cache's statement is longer than any short text of the user's, so under a lowered limit some tables that SQLite holds cannot be in the cache: `docs/todo.md` has that as an open item.

### What was tricky

The fault looked like the todo item, a statement of the cache longer than the limit, but the statement was 690 bytes: it was the UPDATE around it that was too long.

### What warrants a second pair of eyes

- `schemaTooBig` hard-codes the register numbers `#2` and `#1` of the UPDATE. They are those of every create table that the pass supports; a constraint or a default that the pass accepts later could change them.
- `SQLITE_WITHOUT_ZONEMALLOC` is a change of the binding on macOS only; the build of CI on Linux does not change.
- `conn.changeSchema` raises the limit for one statement and lowers it again, and gives both errors if both fail. `SetLengthLimit` fails only on a closed connection.

The review with the code-review skill (medium) found seven possible issues. Fixed: the drop of a table with a name nearly as long as the limit could be refused as too big too, since SQLite formats `DELETE FROM 'main'.sqlite_master WHERE tbl_name=%Q ...` under the limit, so `conn.drop` and `conn.create` now share `conn.changeSchema`, under the largest limit; the check of the length counted `main.`, which SQLite does not keep, so a statement 1 to 5 bytes over was refused (a new subtest of `TestTx_tooBig` creates, restarts, writes, and drops a table whose kept statement is exactly the limit, and refuses one a byte longer); the restore of the limit lost its error; the header of `options.h` said that SQLite reports every option. Left: the register numbers of `schemaTooBig` (above); an instance with a lower limit than the writer's declares the log broken when it replays a table that it cannot hold, which it did before through SQLite's error too, and which only tests with two limits meet; and the open item of `docs/todo.md`.

### Future work

- The open item in `docs/todo.md`: a table that SQLite holds and whose statement in the cache is longer than a lowered limit gets an internal error from the engine. The generated tests do not reach it.

### Code review instructions

- Start with `schemaTooBig` in `/internal/engine/errors.go` and `conn.create` in `/internal/core/cache.go`, then `CreateTable.SQL` in `/internal/sql/stmt/recognize.go`.
- `go test ./internal/sqlite -run TestConn_SetLengthLimit`, `go test ./internal/core -run TestTx_tooBig`, `go test ./internal/engine -run 'TestEngine_tooBig|FuzzDifferential_core'`.

### Technical details

The UPDATE that SQLite formats, for the table `t` with the kept text `CREATE TABLE t (v int primary key) strict, without rowid`:

```
UPDATE 'main'.sqlite_master SET type='table', name='t', tbl_name='t', rootpage=#2, sql='CREATE TABLE t (v int primary key) strict, without rowid' WHERE rowid=#1
```
