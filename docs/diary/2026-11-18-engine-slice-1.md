# Diary: The SQL engine for slice 1

The engine joins the two halves of s4ql that had never met: the SQL front end in `internal/sql`, which turns SQL text into a typed statement of slice 1, and the storage core in `internal/core`, which holds tables and rows in a log of commits in S3 with a SQLite cache. The engine runs a statement of slice 1 on the core, with SQLite's exact results and errors. It is the first slice of the engine, so its structure is what later slices grow from. The public driver is the next builder's task, on top of this one.

## Step 1: The brief

**Author:** engine-slice-1-builder

### Prompt Context

**Verbatim prompt:** "You are the builder for the feature "The SQL engine for slice 1" in s4ql [...] Your task is the piece that joins them: the engine that runs a slice-1 statement on the core, with SQLite's exact results and errors. It is the first slice of the engine, so its structure is what later slices grow from. There is no public API and no `database/sql` driver yet; that is the next builder's task, on top of yours." The brief then lists the requirements: the stages (`stmt.Check`, a plan that is plain data, a tree of push-style operators over a small storage interface of the engine's own), the semantics of each statement as its own transaction, annotations, the number of rows changed and "last insert rowid" as SQLite reports them; SQLite's errors with all three of code, extended code, and message, in one Go type; the core's errors passed through, and its `ErrTableExists`, `ErrNoTable`, `ErrReservedName`, and `ErrInvalid` never; the names that the core refuses made "not supported" at the pass; one wide event per statement; and seven layers of tests, with SQLite as the judge: differential sequences and a fuzz test, restarts with an empty cache, the simulation with faults and planted bugs, operators alone, no transaction left open, mutation testing, and lint and the sanitizer.
**Interpretation:** Build `internal/engine` on `*core.DB`, with an API that a `database/sql` driver can stand on, find out by experiment what SQLite does in every case, and hold the engine to SQLite and to a model under the simulation.
**Inferred intent:** Trust in correctness: every answer of the engine is one that SQLite gives for the same sequence of statements, also across restarts and faults, and the structure is the one the next slices extend.

### What I did

Read `AGENTS.md`, `docs/spec.md`, `docs/roadmap.md`, `docs/todo.md`, the READMEs of `internal/sql/stmt`, `internal/core`, `internal/sim`, and `internal/sql/mutation`, the API of the core, the binding, and the simulation, the core's tests of the simulation, and §3 and §6 of `docs/research/sql-front-end.md`. Created the branch `engine-slice-1` on `fe40bbd`.

### Why

The engine sits between two halves that each have strong conventions, and the brief asks to follow them.

### What worked

The sqlite3 shell of the machine is 3.54.1, the pinned version, so first facts about `changes()` could be had at once: `changes()` is left as it was by `create table`, `drop table`, and `select`, set to 1 by an insert, set to 0 by an insert that fails with `UNIQUE constraint failed`, and left as it was by an error at prepare (`no such table`, `already exists`). `last_insert_rowid()` is not changed by an insert into a `WITHOUT ROWID` table. The tests will pin these through the binding.

### What didn't work

Nothing yet.

### What I learned

`database/sql` itself starts a goroutine in `sql.OpenDB`, so the public driver cannot run under the scheduler of `internal/sim`; the engine can, and must not use `iter.Pull`.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: The engine, its errors, and the pass's gate on names

**Author:** engine-slice-1-builder

### Prompt Context

**Verbatim prompt:** (the same brief) "A new internal package (name and layout are yours, for example `internal/engine`) with an API that the driver will be built on [...] Give SQLite's errors one Go type that carries the three values [...] Names that the core refuses. [...] Change `internal/sql/stmt` so that it rejects them"
**Interpretation:** Build the stages as separate, callable steps; one error type outside `stmt`; and find every rule on names in which the core is stricter than the pass.
**Inferred intent:** One gate (the pass) decides what is supported, and the engine never has to translate a refusal of the core.

### What I did

- `/internal/sql/sqlerr/sqlerr.go`: `sqlerr.Error{Code, ExtendedCode, Message, Offset}`, which replaces `stmt.SyntaxError`. `stmt.Check` returns it for syntax errors.
- `/internal/sql/stmt/recognize.go`: the empty name `""` is "not supported" (`empty name`) in every position. It is the only rule of the core (`checkIdentifier` in `/internal/format/check.go`, `reserved` in `/internal/core/cache.go`) that the pass did not already cover: valid UTF-8 and no NUL are checked before parsing, and the reserved prefixes are reserved names already.
- `/internal/sql/stmt/stmttest/stmttest.go`: the generator of slice 1, moved out of `generate_test.go` so that the engine's tests build on it, with the same stream of random numbers, a `Rand` interface instead of `*rand.Rand`, pools that the caller gives, `StatementOf(kind)`, and a hook for values.
- `/internal/sqlite/sqlite.go`: `Stmt.ColumnDeclType`, so that the oracle can give the declared type that SQLite reports for `select *`. SQLite gives `INT` for `int`, `Int`, and `INT`: it upper-cases the standard type names, and gives others as written (`Varchar ( 10 )`).
- `/internal/engine`: `engine.go` (Exec, Query, Rows, Result, the write and read transactions), `plan.go` (plain-data plans and `Format`), `operator.go` (scan, values, project, insert, create, drop, and the build steps with SQLite's checks in SQLite's order), `storage.go` (the interfaces and the adapters for the core), `errors.go`, `telemetry.go`, `bugs.go`.
- `/internal/sql/stmt/started.go`: `StartedTable`, see below.

### Why

The plan holds names as written and nothing of the catalogue, so that the lookups happen in the transaction that runs the statement, which is where SQLite's errors about names must come from for a statement that is its own transaction.

### What worked

The differential test passed on its first run: 800 statements of 20 sequences, with every error kind. The error forms of `/internal/sql/stmt/README.md` were right in every case.

### What didn't work

On the second run of the differential test, with more texts that are not slice 1, it failed:

```
"create table T1 (qUERY int primary key) strict without rowid": the engine gives
1/1: near "without": syntax error
and SQLite
1/1: table T1 already exists
```

SQLite looks a table up in `sqlite3StartTable` while it parses, as soon as it has read `CREATE TABLE name (` or `AS`, and gives `table %s already exists` before any later syntax error. The pass judges syntax on an empty database, so it cannot know. I probed the sqlite3 shell (3.54.1, the pinned version) on 30 texts: also after `explain`, `explain query plan`, empty statements, names in brackets, backticks, and single quotes (printed as written, looked up dequoted), `main.` before the name, and `CREATE VIRTUAL TABLE name USING m (`; not after TEMP, `temp.`, IF NOT EXISTS, or another schema (`unknown database zz`). `stmt.StartedTable` gives the name for such a text, and the engine looks it up and gives SQLite's error. The same kind of error in `CREATE TRIGGER` (`no such table: main.t2`, `cannot create INSTEAD OF trigger on table: t1`) is not handled, and is in the todo.

My first `StartedTable` took `create table if (` for the start of a table named `if`; SQLite gives `near "(": syntax error` there, because after IF the grammar wants NOT. The check that the pass's syntax error comes after the `(` fixed it, and a cut in the middle of a multi-byte character showed that `StartedTable` must ask `stmt.Check` for the syntax error rather than the parser.

### What I learned

`sqlite3_changes` is left as it was by CREATE TABLE, DROP TABLE, SELECT, and by an error at prepare, and set to 0 by an error at step (`UNIQUE constraint failed`). So the count is a property of the connection, and the engine's `Result.SetsChanges` says whether a statement sets it.

### What was tricky

A query's read transaction must end on every path: the last row, an error, a loop that stops, a panic in the loop, and `Close`, also `Close` during the loop. `Rows.All` ends it in a deferred call, and `Close` during a loop only marks the rows, which end at the next row.

### What warrants review

`/internal/sql/stmt/started.go` against SQLite's grammar, and the order of the checks in `buildSink` in `/internal/engine/operator.go`.

### Future work

The trigger case above.

## Step 3: The test layers

**Author:** engine-slice-1-builder

### Prompt Context

**Verbatim prompt:** (the same brief) "4. Tests: SQLite is the judge [...] 1. Differential against SQLite, statement by statement. [...] 2. Restart. [...] 3. Under the simulation with faults. [...] 4. Operators alone [...] 5. No transaction is left open [...] 6. Mutation testing [...] 7. `make lint` [...]"
**Interpretation:** One harness, `differential`, that holds the engine to SQLite after every statement, reused by the fuzz test; and a simulation whose checks come from the log and SQLite.
**Inferred intent:** Every answer of the engine is shown to be SQLite's, also under faults, and the checks are shown to fail when the engine is wrong.

### What I did

- `/internal/engine/differential_test.go`: sequences of statements from `stmttest`, with names from a few base names in several spellings (ASCII case changes that name the same table, upper case of other letters that names another, with and without quotes), values from a small set so that keys repeat, and one statement in twelve from a list of texts that are not supported, are syntax errors, or hold no statement. After each statement it compares the answer, the count of changes, and the last insert rowid; at the end and after each restart, every table. Half of the sequences restart the instance at random points, with an empty cache or its own.
- `FuzzDifferential` runs the same harness on a database in memory (`/internal/engine/memory_internal_test.go`), because on the core a sequence takes about 25 ms and 50 million would take days; `FuzzDifferential_core` runs it on the core.
- `/internal/engine/simulation_test.go`: the workload under the scheduler, with `simstore.AllFaults()`, two instances, a crash with a restart on a copy of the cache or an empty cache, a close and reopen, and statement timeouts of up to 5 s so that commits end with an unknown outcome. Each statement carries `/* id N */` after its first token, which its annotation keeps. `run.verify` reads the log from the store and checks it against the attempts and SQLite: each commit is one statement of an attempt that succeeded, is unknown, or was cut by a crash; each acknowledged write is in the log; each statement of the log succeeds in SQLite in log order; each SQLite error and each query gives the same answer in SQLite on the state that it saw (from the `statement.position` of its event), which holds every write acknowledged before it started; and an instance with an empty cache replays the log to SQLite's state.
- Operators alone against a fake in `/internal/engine/operator_internal_test.go`; plans in `/internal/engine/plan_test.go`; hand-written errors pinned against SQLite, events, annotations, no transaction left open, the names of the core, and concurrency in `/internal/engine/engine_test.go`.
- `/internal/sql/mutation`: the tool lists test packages under `internal/...`, fingerprints all of `internal`, includes `internal/engine` by default, and has a stage `simulation`.

### Why

The log is the truth, so the simulation checks the engine against SQLite through the log, which needs no model of the engine of its own.

### What worked

The four planted bugs are found: `skipUnique` by a commit that fails in SQLite (`UNIQUE constraint failed: OUTER.OFFSET`), `ackUnknown` by an acknowledged statement that is not in the log, `checkOutside` by a commit that fails in SQLite in the hammer workload, and `keepOpen` by the check that an instance runs a write after the workload.

### What didn't work

- The first simulation had each caller draw its own pool of names, so callers of different instances never met at a table. One sequence for the run fixed it.
- With timeouts of 30 s no commit had an unknown outcome in 20 seeds; with timeouts of up to 5 s, 12 did.
- `checkOutside` was first "found" for the wrong reason: its pre-check gave `UNIQUE constraint failed` from a read transaction that recorded no position, which the check reported as a statement that saw no state. With the position recorded, the default workload found it in 0 of 100 seeds; it needs two inserts of one key on one instance at once, which the hammer workload gives.
- `golangci-lint` flagged `for range rows.All()` (errcheck drops the error of a `Seq2`) and an ignored error of `Rows.end`.

### What I learned

A heredoc in a shell command is refused in this worktree; edits go through files.

### What was tricky

Crashed instances keep their caches open, and only the core's own tests can close them (`core.DB.CloseCache` is in its `export_test.go`). The address sanitizer reports what is left open, so the simulation runs no crashes under it.

### What warrants review

`run.verify` in `/internal/engine/simulation_test.go`: whether each check can fail.

### Future work

An exported way to close the cache of a crashed instance for the tests of other packages, so that crashes can run under the address sanitizer too.
