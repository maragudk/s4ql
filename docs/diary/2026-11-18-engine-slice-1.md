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
