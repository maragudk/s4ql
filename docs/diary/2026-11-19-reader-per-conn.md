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

## Step 2: The core hands out readers

**Author:** reader-per-conn builder

### Prompt Context

**Verbatim prompt:** (from the lead's brief) "The core hands out such readers and keeps track of them: a stop of the instance and a close end every reader, also one that a read transaction is using, exactly as they end the reads of today, and a reader that is given back after that is handled cleanly. A read through a reader keeps every check that `DB.Read` has today (the lease and the clocks before a read, the catch-up before serving, the refusal on an instance that does not serve, the stop). Do not weaken any of them, and show with tests that each still holds on the new path."
**Interpretation:** A type `Reader` in `/internal/core/read.go`, with its life cycle and the checks of a read, and no pool next to it.
**Inferred intent:** The owner of a reader, and nothing in the core, decides how many there are, and the core still ends every one of them itself.

### What I did

In `/internal/core/read.go`:

- `DB.Reader(ctx)` opens a connection to the cache, sets `pragma query_only = on`, and puts the connection on the list `DB.readers`, under `DB.mu`, unless the instance has stopped meanwhile. It refuses on a stopped instance, and does not need the instance to serve. A failure of the open goes through `DB.cacheError`.
- `Reader.Read(ctx)` is the body of the old `DB.Read`. It checks the context, yields at a new site, "core: a read before it starts", and then, in one critical section, refuses for the stop or for an instance that does not serve by `DB.servingLocked`, for a reader that is closed (`ErrReaderClosed`), and for a reader whose read transaction is open (`ErrReaderBusy`). The rest is unchanged: `begin`, the yield, the position, which fixes the state, the yield, and the second `servingLocked`.
- `Reader.Close(ctx)` ends the read transaction that is open, closes the connection under `DB.mu`, and takes it off the list. After `DB.Close` it finds the connection closed and only takes it off. While a call of the read transaction runs, it returns `ErrReaderInUse` and changes nothing. A second close does nothing.
- `ReadTx.end` clears `Reader.tx`.

In `/internal/core/core.go`: `Options.Readers`, the semaphore `readerPool`, and the list `idle` are gone, `Open` opens only the write connection, and `DB.use` gives `ErrReaderClosed` for a connection that a reader closed while the instance runs. `DB.closeIdleLocked`, `DB.waitClosed`, and `DB.closeCache` go over `DB.readers` as before, which now grows and shrinks.

In `/internal/core/export_test.go`: `DB.Readers()`, the length of the list, for the tests.

The tests, in `/internal/core/read_test.go` unless another file is named:

- `TestReader_Read`: twenty readers with a read transaction each, each started after one more commit, each on its own state while a further write commits; a second read on a reader refused at once, and started after the first has ended; a context that is done; a reader on a spare, refused with `ErrNoLease`, which reads the holder's commits at the head of the log once the spare has the lease and has caught up; and the two that were there, one state under a commit, and the lease that runs out after the state is fixed.
- `TestDB_Reader`: no reader on an instance that is closed; and the stop when the cache cannot be opened, through `faultvfs`, with the read transaction of another reader ended by that stop.
- `TestReader_Close`: the close with a read transaction open; `ErrReaderInUse` inside a scan, and the close after it; nothing left to close after `DB.Close` with eight readers, four of them in a read transaction, and the write-ahead log gone; and six readers in read transactions on an instance that stops for a failed write of the write-ahead log, where every call fails with the stop, three readers are closed before the close of the instance and three after.
- `TestLease_clocks` in `/internal/core/lease_test.go` starts its reads through `startErr`, so the check of each clock is on the new path, as is every use of `readErr` in the tests of the lease.
- The simulation, in `/internal/core/simulation_test.go`: each caller is a `caller`, which opens a reader at its first read of a database and closes it when the node has opened its database again or when the caller ends. A close of a reader that fails is a violation. The callers of an instance that crashes never close theirs, and `DB.CloseCache` closes them after the run.

`go test -shuffle on ./internal/core/` passes, with the minimums of `TestSimulation` and every planted bug found, unchanged.

### Why

A reader is one SQLite connection, and its owner is the only one that knows when it is needed. The checks of a read are those of `DB.servingLocked`, at the same two places.

### What worked

Keeping the count of users of a connection (`DB.use`) as the one thing that decides when a connection may close: `Reader.Close` refuses while it is above zero, and `DB.Close` waits for it.

### What didn't work

The first version made a second read on a reader wait, with a semaphore of one permit for each reader, and went through `DB.acquireServing`, as step 1 planned. It worked in the core. It cannot work in the driver: `database/sql` holds the mutex of a connection during every call of the driver on it, also during `Rows.Close` and `Rows.Next`. A second query on a `sql.Conn` that waited inside the driver for the rows of the first would hold that mutex, so nothing could close or read those rows, and the wait could only end with its context. With `context.Background()` it would never end. So a read on a reader that is busy fails at once, with `ErrReaderBusy`, and the semaphore is gone.

The test of the simulator "should list a call from its start until its end" failed after the change:

```
--- FAIL: TestStateAt/should_list_a_call_from_its_start_until_its_end (0.01s)
    state_test.go:117: Not true
    state_test.go:117: listed at its start
```

It took the fourth call of a run and assumed that it lasts. The run is another one now, and its fourth call, a read that was served, starts and ends at the same simulated time, because each of its three yields drew no jitter. The test now takes the first call that ends later than it starts.

### What I learned

- A read no longer parks at a semaphore, where the scheduler of the simulation could switch tasks. The yield before the read stands in for the acquire. Nothing stands in for the release: the caller's next step is a call of the host anyway.
- `Reader.Close` must close the SQLite connection under `DB.mu`. If it released the mutex first, `DB.Close` could take the connection for closed and return while SQLite was still closing it, and a test that looks for the write-ahead log right after the close could find it.

### What was tricky

The order of the refusals in `Reader.Read`. The stop comes first, so that a reader whose read transaction a stop has ended, and whose owner has not closed that transaction yet, answers with the stop and not with `ErrReaderBusy`.

### What warrants review

- `/internal/core/read.go`, all of it.
- That `DB.Reader` opens the connection without `DB.mu` and looks at `stopped` again before it puts the connection on the list. A connection that it then closes itself has read nothing, so it holds no lock and no write-ahead log.
- The tree does not build between this commit and the one of the driver, because the engine, the simulator, and the driver still call `DB.Read`.

### Future work

`DB.acquireServing` has one caller left, `DB.Begin`.
