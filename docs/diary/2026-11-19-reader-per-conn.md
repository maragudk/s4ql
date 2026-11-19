# Diary: Each connection of the pool has a reader of its own

The spec says that the number of connections in the database pool sets how many reads run at once. As built, the core had a fixed number of read connections, `Options.Readers`, 4 by default, and the pool of `database/sql` has no limit by default, so `SetMaxOpenConns` could lower the number of reads at once and could not raise it above 4. Markus agreed that the code follows the spec's sentence: each connection of the pool has a reader of its own. The work is on the branch `reader-per-conn`, which starts from `main` at `9c5ba9b`.

## Step 1: The brief, and a plan

**Author:** reader-per-conn builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "The spec says: 'Reads run in parallel. Each read transaction sees one consistent state of the cache, even while a write is in progress. The number of connections in the database pool sets how many reads run at once.' As built, the core has a fixed number of reader connections (`Options.Readers`, 4 by default, a number nobody measured), and the pool of `database/sql` is unlimited by default. A fifth concurrent query waits for a free reader. [...] The code follows the spec's sentence: each connection of the pool of `database/sql` has a reader of its own, so the pool alone decides how many reads run at once. [...] A connection of the driver (`driver.Conn`) owns one reader connection to the cache. It is opened when the connection first needs it (a connection that only writes never opens one), and closed when `database/sql` closes the connection [...] The core hands out such readers and keeps track of them: a stop of the instance and a close end every reader, also one that a read transaction is using, exactly as they end the reads of today, and a reader that is given back after that is handled cleanly. A read through a reader keeps every check that `DB.Read` has today [...] One path, not two [...] `Options.Readers` goes away if nothing needs it. [...] How the engine's API takes the reader is yours to design; keep it small and say why. Keep what the public package exports unchanged [...] A failure to open a reader [...] is a local failure of that connection's statement. Decide by the spec's rule 'A local I/O failure stops the instance' whether it stops the instance or fails only the statement [...] Telemetry: no new events are asked for."
**Interpretation:** The core gets a type for one read connection that its user owns, opens, and closes; `DB.Read`, the pool of read connections, and `Options.Readers` go. The engine gets a connection of its own, which owns such a reader and opens it at its first read. A `driver.Conn` owns one connection of the engine. The simulations, the engine's tests, and the simulator give each caller a reader.
**Inferred intent:** The pool of `database/sql`, with its four settings, is the one place that decides how many reads run at once, and the number 4 that nobody measured is gone.

### What I did

Reset the worktree to `origin/main` at `9c5ba9b` and created the branch `reader-per-conn`. Read `/AGENTS.md`, the sections Concurrency and Constraints of `/docs/spec.md`, `/internal/core/README.md`, `/internal/core/core.go`, `/internal/core/read.go`, `/internal/core/cache.go`, `/internal/core/errors.go`, the start of `/internal/core/tx.go`, `/internal/core/export_test.go`, `/internal/core/read_test.go`, the parts of `/internal/core/simulation_test.go` and `/internal/core/workloads_test.go` that open instances, run callers, crash, and read, `/internal/engine/engine.go`, `/internal/engine/storage.go`, the fakes in `/internal/engine/engine_internal_test.go` and `/internal/engine/memory_internal_test.go`, the callers and `takeReaders` of `/internal/engine/simulation_test.go`, `/conn.go`, `/rows.go`, `/s4ql.go`, `/export_test.go`, the helpers of `/s4ql_test.go`, `TestRows` in `/rows_test.go`, `/internal/simulator/request.go` and the reads of `/internal/simulator/run.go`, the documentation of `/internal/sqlite/faultvfs`, the semaphore of `/internal/sim/sim.go`, `/internal/simlint/simlint.go`, `/.github/workflows/ci.yml`, and the `Makefile`.

The plan:

1. **Core.** `DB.Reader(ctx)` opens a read connection to the cache and gives a `*Reader`, which the instance keeps in the list of its read connections. `Reader.Read(ctx)` is what `DB.Read` was, on the connection of the reader, with a semaphore of one permit of the reader in place of the pool's: it goes through `DB.acquireServing` unchanged, so it keeps the check of the lease and the clocks before the read, the wait that ends when the instance stops serving, the second check after the state is fixed, and the stop. `Reader.Close(ctx)` ends the read transaction that is open, closes the connection, and takes it off the list; after `DB.Close` has closed the connection it does nothing. `DB.Read`, the idle list, the semaphore `readers`, and `Options.Readers` go.
2. **Engine.** `Engine.Conn()` gives a `*Conn` with `Exec`, `Query`, and `Close`, and `Engine.Exec` and `Engine.Query` go. A `Conn` opens its reader at its first read.
3. **Driver.** A `conn` owns a `*engine.Conn`, and its `Close` closes it.
4. **The simulations, the simulator, and the tests** give each caller a reader or a connection of the engine.
5. **Documentation.** `/docs/todo.md`, `/internal/core/README.md`, `/internal/engine/README.md`, `/internal/simulator/README.md`, and the package documentation in `/s4ql.go`.

Three decisions that the brief leaves to the builder, made before the code:

- **A second read on a reader that has a read transaction open waits,** until the first ends or its context ends. `rows_test.go` has a test "should run several queries at once on one connection", which holds the rows of one query open on a `sql.Conn` and runs a second query on it; with one reader for each connection that cannot go on as it was. The choices were to fail the second query at once, to make it wait, to open a further reader for it, or to let both share one state of the cache. Sharing a state breaks the spec's "A read that starts after a commit is acknowledged therefore always sees it". A further reader breaks "the pool alone decides". Failing at once makes a statement fail by timing where `database/sql` closes the rows of a query whose context ended from a goroutine of its own, and the application runs the next statement on the same `sql.Conn` before that close has run. Waiting is what `database/sql` itself does for a query on a pool with `SetMaxOpenConns(1)` whose one connection holds open rows, and what the fifth query did before, so it is the choice. It also keeps `DB.acquireServing`, and with it every check of the wait, on the path of a read.
- **A failure to open a reader goes through `DB.cacheError`,** as every error of the cache does while the instance runs: what `classify` calls a local I/O failure stops the instance, and that includes SQLite's `CANTOPEN`, which is what a process without a free file descriptor gets. The first read of a new connection opens the write-ahead log too, and a failure there already stopped the instance before this change, so an open that failed without a stop would make the outcome depend on which of the two files met the limit.
- **The open readers are counted through `faultvfs`,** which counts the files that are open through it, and not through a seam in `/export_test.go` of the root package: a test file of the root package cannot see the fields of the core, and a method of the core that only a test calls is what the brief rules out. The VFS counts what SQLite itself has open.

### Why

A reader that its user owns maps onto a `driver.Conn` one to one, so the four settings of the pool govern the readers with no code of ours: `database/sql` closes a connection, and the connection closes its reader. Keeping `DB.acquireServing` on the path means that no check of a read is written a second time.

### What worked

The core already counts the calls that use each connection (`DB.use`, `DB.unuse`) and closes every connection in `DB.closeIdleLocked`, over a list of the read connections. A list that grows and shrinks needs nothing new there.

### What didn't work

Nothing tried yet.

### What I learned

- The semaphore of the simulation parks its task at every acquire and release, also when a permit is free, so that the seed decides who goes on. A semaphore for each reader therefore keeps the points at which the scheduler can switch tasks in a read where they were.
- SQLite's unix VFS does not close the file descriptor of a connection that closes while another connection of the process holds a lock on the same file, which the write connection always does in the journal mode wal. It keeps the descriptor and uses it for the next connection. So the number of file descriptors of the process says nothing about the number of readers, and a test cannot count them that way.
- `internal/simulator` has a setting `readers` in its request, and rejects a member that it does not know. Nothing under `/site` sends `readers`.

### What was tricky

Finding a way for a test of the root package to count readers without a method in production code that only tests call.

### What warrants review

The three decisions above, and above all the first: a `sql.Conn` runs one read at a time, and a second query on it waits for the rows of the first.

### Future work

None yet.
