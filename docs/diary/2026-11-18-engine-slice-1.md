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

## Step 4: Self-review, and what it changed

**Author:** engine-slice-1-builder

### Prompt Context

**Verbatim prompt:** (the same brief) "Self-review before you report (the code-review skill is available)."
**Interpretation:** Two independent reviewers over the diff, then fix what they agree on and what is serious.
**Inferred intent:** Find what the author cannot see.

### What I did

Ran the code-review skill: two reviewers, each told to compete with the other, over `git diff origin/main...HEAD`, with the checks on comments that look outward and on constants used once. Both found the same two serious things and a long list of smaller ones, nearly all in agreement. I fixed:

- **A compiled binary in the history.** `go build ./internal/sql/mutation`, run in the root of the module to check that it compiles, wrote a 5.4 MB executable `mutation` there, and `git add -A` committed it in the commit of the mutation tool. `git filter-branch --index-filter` was refused in this worktree, so I rebuilt the branch: a new branch at the commit before, `git cherry-pick -n` of that commit, `git rm --cached mutation`, `git commit -C`, `git cherry-pick` of the rest, and `git branch -f`. No commit of the branch holds it now.
- **`stmt.StartedTable` and reserved names.** For `create table sqlite_x (`, SQLite gives `object name reserved for internal use: sqlite_x` before it looks the table up, and `StartedTable` reported a start; `FuzzStartedTable` would have failed on it. It now reports none for a name that starts with `sqlite_`, and takes the syntax error of `stmt.Check` as an argument, so a text is not parsed twice. Its test now creates, where it expects no start, a table for each of the first tokens of the text, so a missed start shows whatever the name.
- **The count of changes after a failed insert.** `Result` could not say that an insert that fails at `UNIQUE constraint failed` sets SQLite's count to 0; the differential test set its own count to 0 by hand. `Exec` now gives `Result{SetsChanges: true}` with the error of an insert that fails after its checks, and `Query` gives rows that carry it; the test now takes the count from the result alone.
- **Read transactions on every path.** A panic while a query was built, or while `started` looked a table up, left a read transaction open; both now end it in a deferred call, with a test. The error of the end after an early stop is now given by `Close`, once.
- **`Rows.Close` during a loop,** which a reviewer showed cannot end the transaction while an iterator is suspended in its body: documented on `Rows`, and in the todo for the driver.
- **`fromCore`:** `core.ErrInvalid` from `Tx.Commit` (a commit time that the commit object cannot hold) is the clock's and passes through; `core.ErrTxDone` is an internal error.
- **The join of the events is tested:** `TestEngine_events` now requires that the core's `commit` event has the same `commit.parent` as the engine's event of the insert.
- **Tests on real time:** the instances of the differential tests have a busy timeout of a minute and a lease of an hour, and `noneOpen` has no deadline: a transaction left open makes it wait until the test times out. The number of read connections is a constant of the test.
- **The simulation:** one query in four is not read to its end (a loop that stops, `Close` unread, a consumer that panics), and `Rows.All` yields to the scheduler between rows, so commits, closes, and crashes now come between the rows of a query. A statement that saw no state is let through only for the three messages of the parser. Each planted bug must be found by the violation it causes.
- Comments that named the importers of a package or the core's internals, a doc link to a function that does not exist, and a constant used in one place.

Not changed, by decision: the SQL text is in an event only at Debug, also for a statement that failed with an error of the core, because the brief rules it out at Info and above; the constants of the attribute names stay, because their names carry the documentation and the tests read them.

### Why

Both reviewers agreed on almost all of these; the binary and the leak of a read transaction on a panic are serious on their own.

### What worked

The rebuilt history: `git log origin/main..HEAD -- mutation` gives nothing.

### What didn't work

`git filter-branch -f --index-filter 'git rm --cached --ignore-unmatch -q mutation' origin/main..HEAD` was refused: "this command passes --index-filter to git, which runs a program that can't be verified".

### What I learned

`switch hostRand{n.inst}.IntN(12) {` does not parse: a composite literal in the head of a switch needs parentheses.

### What was tricky

Deciding what `Result` should say for a failed statement. SQLite's count after a failed statement depends on whether it failed when prepared or when stepped, which `database/sql` never shows, but a connection that keeps the count must know it.

### What warrants review

The rules of `Result.SetsChanges` in `/internal/engine/engine.go`, and the doc of `Rows`.

### Future work

Bridging `Rows.All` to a pull-style `driver.Rows` is the driver's to design, with the rule that a suspended iterator must be stopped.

## Step 5: Mutation testing, and the long runs

**Author:** engine-slice-1-builder

### Prompt Context

**Verbatim prompt:** (the same brief) "6. Mutation testing of the engine's package with the repository's tool (`internal/sql/mutation`; extend it to take a package outside `internal/sql` if needed): no survivor left unexplained. Also rerun it for `internal/sql/stmt` if you change it." and "Run the fuzzing long on several cores until quiet (at least 50 million executions) and report the count"
**Interpretation:** Run the tool on `internal/engine` and `internal/sql/stmt`, close every gap it shows, and list the rest as equivalent with a reason; then the long runs on the final code.
**Inferred intent:** Show that the tests notice a wrong decision anywhere in the new code.

### What I did

The tool reads the files of the module while it runs, so I ran it on a copy of the tree from `git archive HEAD` in the scratchpad, and kept working in the worktree:

```
go run ./internal/sql/mutation -out $S/mut -pkgs internal/engine,internal/sql/stmt -j 6
```

2,102 mutants in about 35 minutes. 110 survived: 74 in the engine, 34 in `stmt.StartedTable`, one in the recognizer (the guard `r.isNext(t)` of the empty name), and the known equivalent one. I wrote tests for every survivor that a test can see, ran the affected files again on a new copy (`-run 'internal/engine/|started.go|recognize.go recognizer.name'`, 1,208 mutants, 10 minutes with 9 workers), closed the last gaps in the events, ran `telemetry.go` once more, and listed 25 equivalent mutants in `/internal/sql/mutation/equivalent.txt`.

### Why

A survivor is a decision that no test checks.

### What worked

The gaps were real and small: no test looked at the event of a statement on its paths of failure (outcome, rows, position, duration), at the offsets of the errors, at `Rows.Close` during a loop on the fake (on the core it passed for the wrong reason: the rows still stopped, but the transaction ended under a running scan), at the end of a read after a loop that stopped early, or at plans that no statement of slice 1 gives. Two pieces of code went away instead of getting tests: the check of IF NOT EXISTS in `StartedTable`, which its later checks cover, and a second `r.reading = false` in `Rows.All`.

The long runs on the final code: `TestDifferential` with 10,000 sequences (400,000 statements), `TestSimulation` with 1,000 seeds of each workload, and `TestSimulation_plantedBugs` with 200 seeds of each bug. Nothing differed from SQLite, no seed found a violation without a bug, and each bug was found by its own violation.

### What didn't work

- The first long differential run failed after some thousands of sequences with `open the cache: local I/O failure: sqlite: unable to open database file (code 14)`. Each sequence's instance was closed only by `t.Cleanup`, at the end of the test, so 10,000 instances held their files open at once. `differential` now closes the instance and the oracle at the end of each sequence.
- The first long fuzz runs ran on code that the review then changed, so they were stopped (10 million and 3 million executions, nothing found) and run again on the final code.

### What I learned

Most of the shape checks of `StartedTable` are covered by its last check, of the offset of the syntax error: SQLite's grammar lets only a dot, `(`, or AS follow the name of CREATE TABLE, so the parser fails at any other token there.

### What was tricky

Telling an equivalent mutant from a gap: `return "core_error"` removed from the case of `ErrOutcomeUnknown` ends the switch at its last return, which gives the same, but the case itself matters, which a test now shows with an error that wraps both `ErrOutcomeUnknown` and `context.DeadlineExceeded`.

### What warrants review

The reasons in `/internal/sql/mutation/equivalent.txt` for the engine and `StartedTable`.

### Future work

None from this step.
