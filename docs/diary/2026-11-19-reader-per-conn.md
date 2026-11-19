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

## Step 3: A connection of the engine, and the simulator

**Author:** reader-per-conn builder

### Prompt Context

**Verbatim prompt:** (from the lead's brief) "How the engine's API takes the reader is yours to design; keep it small and say why. [...] The engine and the core run under the deterministic simulation, where there is no `database/sql`; decide how the simulation, the engine's tests, and the differential tests get their readers (for example each simulated client owns one, as a connection does), and keep the simulation deterministic and its coverage at least what it is (it asserts minimum event counts and finds planted bugs; all of that must still pass, and the planted bugs must still be found)."
**Interpretation:** The engine gets a type that stands for one connection, and its tests, its simulation, and the simulator use it or a reader of the core.
**Inferred intent:** One way to read, from the driver down to the core, which the simulations exercise as the driver does.

### What I did

In `/internal/engine/engine.go`: `Engine.Conn()` gives a `*Conn`, with `Exec`, `Query`, and `Close`. `Engine.Exec` and `Engine.Query` are gone, and their bodies are methods of `Conn`. A `Conn` opens its reader in `Conn.read`, at its first statement that reads, and keeps it until `Conn.Close`. A statement on a closed `Conn` is an internal error. `Conn.Close` passes `core.ErrReaderInUse` on and stays open.

In `/internal/engine/storage.go`: the interface `database` has `reader(ctx)` in place of `read(ctx)`, and a `readConn` has `read` and `close`. `coreReader` is the one of a `*core.Reader`. `fromCore` takes `core.ErrReaderClosed` for a fault of the engine. `core.ErrReaderBusy` passes through as it is: it is the error of a statement that reads while the rows of an earlier query of its connection are open, which is what the caller did and no fault of the engine.

Why a `Conn`, and not a reader as an argument of `Exec` and `Query`: only the engine knows whether a statement reads, after it has parsed it, so the engine must be the one that opens the reader, or a connection that only writes would open one. And a connection is where the state of a connection will live when a transaction spans statements.

The tests:

- The fakes, `fake` in `/internal/engine/operator_internal_test.go` and `memory` in `/internal/engine/memory_internal_test.go`, are their own readers. `fake` counts the readers that are opened and closed.
- `TestConn` in `/internal/engine/engine_internal_test.go`, on the fake: no reader for a connection that only writes, one from the first read on, and none after the close; a reader for each connection; a reader that cannot be opened, which fails the statement with one event, and the next statement, which opens one; the close while the reader is in use, and with another error; and the statements on a closed connection.
- `TestConn` in `/internal/engine/engine_test.go`, on the core: twelve connections with a query each, each started after one more insert, each on its own state; a second query and a `select` through `Exec` on a connection whose rows are open, refused with `core.ErrReaderBusy` and not as an internal error, while an insert on it runs; and the rows and the statements of a connection whose instance is closed, and its close after that.
- The `instance` of the tests, in `/internal/engine/differential_test.go`, has one connection, which it closes after its database, so every test closes a connection whose reader the close of the instance has closed. `noneOpen` requires that a write transaction begins and that a query on the connection gets its read transaction (`readFree`). `TestEngine_concurrent` gives each goroutine a connection.
- The simulation, in `/internal/engine/simulation_test.go`: each caller is a `client` with a connection of its own, which it replaces when its node has opened its database again. `run.readsFree` replaces `run.takeReaders`: after the workload, every connection of the callers of a live instance must run a query of a table that does not exist. A connection with a read transaction left open fails it with `core.ErrReaderBusy`, which is a violation at once.

In `/internal/simulator`: a read opens a reader and closes it. The setting `readers` of the request is gone, with its limit and its row in the README.

In `/internal/sql/mutation/equivalent.txt`: the keys of the two equivalent mutants of `Engine.write` and `Engine.checkOutside` name `Conn.write` and `Conn.checkOutside`.

### Why

A connection of the engine maps onto a `driver.Conn` and onto a caller of a simulation in the same way, so the simulations run the path that the driver runs.

### What worked

Making the fakes their own readers kept the calls of the internal tests as they were, apart from the helpers, which now give a connection of a new engine.

### What didn't work

The first run of the engine's tests after the change failed in two tests that run a statement on an instance that they have closed, because the helper `instance.close` closed the connection first, and a statement on a closed connection is an internal error:

```
--- FAIL: TestEngine_errors/should_pass_an_error_of_the_core_through_as_it_is (0.00s)
    engine_test.go:161: Not true
--- FAIL: TestEngine_events/should_log_one_event_for_each_statement,_with_its_kind,_table,_outcome,_rows,_and_position,_and_the_text_at_Debug_only (0.00s)
    engine_test.go:473: Expected "DEBUG", but got "ERROR" (type slog.Level)
```

`instance.close` now closes only the database, and the connection is closed after it, at the next open or at the end of the test.

`TestSimulation_plantedBugs` matches each bug to the text of the violation that finds it, and the texts changed twice, once for each version of what a busy reader does:

```
simulation_test.go:1167: seed 3 finds the bug by another violation: [statement 15, " -- c\n; ;    sEleCt /* id 15 */   /**/\n*  froM -- c\n\"Desc\";;": the reader has a read transaction open [...] b2 has a connection of a caller with a read transaction left open after the end of the workload: the reader has a read transaction open]
```

`keepRead` is now found by "a read transaction left open after the end of the workload", and by the later queries of the same connection, which fail with "the reader has a read transaction open". Before, it was found only at the end, when an instance could not take all its read connections.

`make test` failed in a package that this work did not touch:

```
--- FAIL: TestEquivalent/should_name_only_mutants_that_the_generator_makes (0.19s)
    generate_test.go:203: no such mutant: internal/engine/engine.go Engine.write condition: err == nil => false
```

The list of equivalent mutants names each mutant by its function, and the two functions are methods of `Conn` now.

### What I learned

- The planted bug `keepRead` shows at the next query of its connection now, and no longer only when every read connection of the instance is taken. With a reader for each connection, a read transaction that is left open is the problem of one connection.
- The engine is under mutation testing, and `/internal/sql/mutation/equivalent.txt` holds the names of its functions.

### What was tricky

The check after the workload in the simulation. An instance runs a query only while it holds the lease, so the check still closes each instance whose connections are free, to hand the lease to the next, and still waits ten minutes for one that does not serve. A connection that is busy will stay busy, so that instance is not asked again.

### What warrants review

- `Conn.Close` in `/internal/engine/engine.go`: after any error of the close of its reader but `core.ErrReaderInUse`, the connection counts as closed.
- `run.readsFree` in `/internal/engine/simulation_test.go`.
- The request of the simulator has lost a field. A request that still gives `readers` is refused, as any member that is no field is. Nothing under `/site` gives it.

### Future work

None.

## Step 4: The driver

**Author:** reader-per-conn builder

### Prompt Context

**Verbatim prompt:** (from the lead's brief) "A connection of the driver (`driver.Conn`) owns one reader connection to the cache. It is opened when the connection first needs it (a connection that only writes never opens one), and closed when `database/sql` closes the connection, so that idle connections that the pool drops give their reader back, and the pool's settings (`SetMaxOpenConns`, `SetMaxIdleConns`, `SetConnMaxIdleTime`, `SetConnMaxLifetime`) govern the readers too. [...] At the root, through `database/sql`: more queries than the old fixed number run at once [...]; with `SetMaxOpenConns(k)` at most k reads run at once and the next waits and ends with its context; a connection that the pool closes gives its reader back [...]; `db.Close()` and a stop end every reader with rows open on several connections; nothing is left open after close".
**Interpretation:** `conn` in `/conn.go` owns a `*engine.Conn` and closes it in `conn.Close`. The package documentation says what the pool decides. The tests go through `database/sql` and count what is open in SQLite.
**Inferred intent:** The spec's sentence is true of the driver, and an application reads in the package documentation what that means for it.

### What I did

In `/conn.go`: `connector.Connect` gives a `conn` with a connection of the engine, and `conn.Close` closes it. In `/s4ql.go`: the production wiring passes `core.Options{}`, and the paragraph of the package documentation that said "up to four at once" now says that the pool sets how many reads run at once, that a connection opens its connection to the cache at its first read, that this is two open files, that a cache that cannot be opened for it stops the instance, and that a statement that reads on a connection whose rows are open is not supported. In `/errors.go`: `publicError` gives `core.ErrReaderBusy` as an error that matches `ErrNotSupported`, with the message "s4ql: not supported: a statement that reads on a connection whose rows of an earlier query are open". `go doc -all .` prints the same names as before.

In `/internal/sqlite/faultvfs`: `VFS.OpenFiles()`, in the native build and in the browser's, with a test.

The tests:

- `TestReaders` in `/conn_test.go`, with the cache opened through a VFS of `faultvfs`: twenty pinned connections with a query each, each started after one more insert, each on its own state while a further insert commits, with two files more open for each; a connection that writes, pings, and prepares, which opens nothing, and then reads, which opens two files that stay open while the pool keeps the connection and close with `SetMaxIdleConns(0)`, and a pool with no and with one idle connection; and `db.Close()` with rows open on twelve connections and a connection held, after which no file is open, before the application has closed the rows or the connection.
- `TestRows` in `/rows_test.go`: the test of the fifth query that waits is now the test of the pool, with `SetMaxOpenConns(4)`: a query, a `select` through `Exec`, and an insert each wait for a connection and end with their context, and a query that waits gets the connection that is freed. The test "should run several queries at once on one connection" is replaced by the refusal.
- `TestStop` in `/conn_test.go`: rows on five connections fail at their next row after the stop, and no file is open after the close; and a new subtest for the stop when the cache cannot be opened for the reader of a connection.
- `readersFree` in `/s4ql_test.go` bounds the pool to the connections that it has, takes them all, and runs a query on each, so it waits for the connections whose rows `database/sql` is still closing, and fails for a connection that is held or whose read was left open.
- `/internal/store/s3store/s3store_test.go` opens readers in its two helpers that read. Those tests need a real store and were not run.

The new and the changed tests ran 50 times with `-shuffle on`, and 50 times under `-race`, in the root package, the core, and the engine, and passed.

### Why

`database/sql` already closes a connection in each of the cases that the brief names, so the one thing the driver adds is that a connection has something to close.

### What worked

Counting files through the VFS. The counts are exact at every step: two for the write connection, two more for each reader that has read, and none after the close of the database.

### What didn't work

`TestConn_Raw` failed after 60 seconds with the new `readersFree`:

```
--- FAIL: TestConn_Raw/should_run_the_calls_of_the_driver's_interfaces_that_database/sql_makes_no_more,_as_it_runs_those_with_a_context (60.01s)
    conn_test.go:468: Expected nil error, but got "context deadline exceeded" (type context.deadlineExceededError)
    conn_test.go:468: a connection is held
```

The test called `readersFree` while it still held its `sql.Conn`, which the old check did not mind. It now closes the connection first, and the check then also covers the reader of that connection.

The first version of the new subtest of `TestStop` expected the rows that were open at the stop to fail, and they ended without an error:

```
conn_test.go:1086: Not true
conn_test.go:1086: no error
```

The table had one row, which the test had read. A scan that comes to its end after a stop ends without an error, as it did before this work; only a further row brings the stop. The table has two rows now.

### What I learned

- `database/sql` holds the mutex of a `driverConn` during `Rows.Next`, `Rows.Close`, and every statement, so a driver must never wait, inside a statement, for something that only a call on the same connection can end. That is what rules out a second query that waits for the rows of the first.
- `DB.SetMaxIdleConns` closes the idle connections that are too many before it returns, and a connection that is released to a pool that is full of idle connections is closed in the call that releases it. Both let a test count without waiting.
- A query of 100 rows took 212 µs on a connection that the pool kept and 434 µs on a new connection, whose reader is opened for it and closed after it, on the machine of the build while other tests ran (`testing.Benchmark` in a throwaway test, with the default of two idle connections and with none). `database/sql` keeps two idle connections by default, so an application that runs more than two queries at once opens and closes readers unless it raises `SetMaxIdleConns`.

### What was tricky

What `readersFree` should mean without a fixed number of readers. A read that is left open is now the problem of one connection of the pool, so the check has to reach every connection, and it has to wait for the rows that `database/sql` closes by itself after their context has ended.

### What warrants review

- The package documentation in `/s4ql.go`, which an application reads.
- That a second query on one `sql.Conn` is now refused, where it ran before. An application that loops over the rows of a query on a `sql.Conn` and runs a query on the same `sql.Conn` inside the loop gets "not supported". On a `*sql.DB` nothing changes, because each query takes a connection of its own.
- `SetConnMaxIdleTime` and `SetConnMaxLifetime` have no test of their own, because one would depend on time. They close a connection through the same `driver.Conn.Close` as `SetMaxIdleConns`.

### Future work

Whether `Open` should raise the number of idle connections that the pool keeps is a question for the lead: the default of two means that a third query at once pays for a new reader. Nothing was changed.
