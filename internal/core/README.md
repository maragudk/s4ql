# Storage core

`internal/core` is the storage core of s4ql. A `DB` is one instance of one database: a cache in SQLite, a lease in the manifest, and commits through a manifest swap. `Open` does not wait for the lease, and `DB.Wait` does. `DB.Begin` gives a write transaction, a `Tx`, and `DB.Read` gives a read transaction, a `ReadTx`. The package starts one goroutine, `DB.leaseLoop`.

What the package does in each case is in the code:

- the package comment and the doc comments of `Options` and `DB` in `core.go`
- the doc comments of `Tx.Commit` and of `Tx.spoolCommit`, with the net effect of a transaction, in `tx.go`
- the doc comment of `DB.Read` in `read.go`, with the check of the lease before a read
- the doc comments of `DB.acquire`, `DB.resumeLocked`, `DB.renewLocked`, `DB.observe`, and `flight` in `lease.go`, and of `margin` in `core.go`, for the lease and for a swap whose outcome is unknown
- the doc comments of `DB.catchUp` and `DB.fetch` in `catchup.go`, and of `replay` in `replay.go`
- the errors and the doc comment of `classify` in `errors.go`
- the names of the events and of their attributes in `telemetry.go`
- the planted bugs in `bugs.go`

This file holds the reasons behind the design, what was chosen and what was rejected, what was measured, and what the package cannot do. The rules that the package follows are in [docs/spec.md](../../docs/spec.md) under "Concurrency" and "Constraints". The reasoning behind those rules is in four items of [docs/todo.md](../../docs/todo.md): [Local I/O failure handling](../../docs/todo.md#local-io-failure-handling), [Concurrency](../../docs/todo.md#concurrency), [The lease and its clocks](../../docs/todo.md#the-lease-and-its-clocks), and [A manifest swap whose outcome is unknown](../../docs/todo.md#a-manifest-swap-whose-outcome-is-unknown). This file repeats neither.

The core was built on 2026-11-12. It is the first code that uses the simulation harness ([internal/sim/README.md](../sim/README.md)), the storage formats ([internal/format/README.md](../format/README.md)), and the object store in `internal/store` together. The diary is [2026-11-12-storage-core.md](../../docs/diary/2026-11-12-storage-core.md).

## The interface

The future SQL engine is the only user of the package, and the interface is made for it.

- **Every call that can fail takes a context.** A call that waits ends when its context ends, and a call logs its events with its context.
- **A table is an `s4qlpb.CreateTable`, and a value is an `s4qlpb.Value`.** They are the messages of the commit object, so nothing is converted between the engine and the log.
- **A scan is an `iter.Seq2`.** It runs on the goroutine of the caller, so a scan starts no goroutine.
- **The names of tables are compared as SQLite compares them:** ASCII letters without regard to case, and every other character exactly (`fold`). The cache is SQLite, and the oracle is SQLite. A table keeps the spelling of its create.
- **A primary key has at most 250 columns.** A row is found by a condition with a term for each column of its key, and SQLite refuses a condition that is nested deeper than a thousand levels, which about 500 terms are. SQLite itself allows a key of 2000 columns, so this is a restriction of the build.
- **The core checks the storage class of every value itself,** in a write and in replay (`table.checkValue`). A `STRICT` table does not refuse every wrong value: for an `INT` column, SQLite converts the text `'1'` and the real `1.0` to an integer without an error.

## The cache

- **SQLite's own schema is the catalogue.** The core keeps no table of tables. It reads the names from `sqlite_schema` and the columns from `pragma_table_info`. There is then one source of truth, and a person with the `sqlite3` shell sees the tables of the database.
- **The cache has one table of its own, `s4ql_cache`.** It holds the version of the cache's layout and the position, which is the name of the last commit object that the cache has applied. The position is text, 64 hexadecimal digits, so that a person can compare it with the manifest.
- **`synchronous` is `normal`, not `full`.** A cache that loses its last commits in a power loss is behind, which catch-up handles, and it is never corrupt. A commit is acknowledged from S3, so the sync at every commit that `full` adds buys nothing. The doc comment of `conn.setUp` names the source.
- **The SQLite binding got four methods for this package:** `Stmt.BindText`, `Stmt.BindNull`, `Stmt.ColumnText`, and `Stmt.Reset`, in `internal/sqlite`. The position and the names of tables and columns are text, a value can be NULL, and replay runs one statement for many rows.

## A write transaction

- **Which rows a transaction changed is kept in the cache, not in memory.** For each table that existed when the transaction began, a temporary table holds the primary key of every row that the transaction upserted or deleted, and whether the row existed then. A key goes in with `insert or ignore`, so the first record of a key stays. `Tx.Commit` writes the commit object from joins of those tables with the real ones. A transaction can therefore change any number of rows.
- **A table that the transaction created needs no temporary table,** because all its rows are new.
- **The commit object and the annotations go through spool files.** The files are next to the cache. Each is unlinked as soon as it is open, so a crash leaves none behind. A failure of a spool file is a local I/O failure, and it stops the instance.
- **A transaction that changed nothing writes no commit object,** also when it has annotations, which are then stored nowhere. It commits if the lease is valid by the clocks, as a read does. A commit object without changes would only lengthen the log, and without a manifest swap nothing but the clocks can judge the commit. This is a choice of the build, not an agreed rule.
- **An error of the cache in a call of a transaction leaves the transaction fit only for a rollback.** One way to get there is to drop a table while a scan of the transaction is running, which SQLite refuses.
- **`DB.Begin` looks at the clocks, and `Tx.Commit` does not.** `DB.Begin` refuses at once when the lease has run out by a clock, as the spec says. In `Tx.Commit` the manifest swap decides, and a swap that is applied also renews the lease. This is a choice of the build too.

## The commit

- **The commit object is uploaded before the manifest lock is taken.** The lease task can then renew the lease while a large commit object is on its way.
- **The position is written into the cache's transaction before the swap.** After the swap, all that the cache has to do is to commit.
- **The two locks are taken in one order:** the writer lock, then the manifest lock.
- **`serving` becomes true only under the writer lock.** A write transaction that saw the instance serve therefore cannot miss a catch-up.
- **One ordinary mutex, `DB.mu`, guards the flags of the instance.** It is never held across a wait. That is the rule for library code that neither the lint nor the scheduler checks; see [Conditions for keeping the design](../sim/README.md#conditions-for-keeping-the-design).

## The lease

- **An instance without the lease reads the manifest every `Options.Watch`,** which is 5 seconds by default. It then sees a release within that time. When a holder dies, the instance has already watched the manifest since about the holder's last renewal.
- **The wait counts from after the read,** because a read shows the manifest as it was at some moment before the answer came.
- **The margin is the larger of two seconds and a tenth of the holder's duration.** It is for a monotonic clock that runs fast on the instance that waits. The doc comment of `margin` gives the numbers.
- **A new holder writes a new lease entry.** The fields of the old holder's entry that this version does not know are not kept. One holder keeps them from renewal to renewal. The todo lists this as an open question under [Storage formats](../../docs/todo.md#storage-formats), and this is what the build chose.
- **The retries of the lease task back off,** from one second to `Options.Watch`, while the store gives no answer.
- **The lease is checked before the state of a read is fixed, and that check is the one that counts.** At its moment no other instance holds the lease, so the cache holds the newest state, and the state that the read fixes later is that state or a newer one. The todo says that the order does not matter, "The cache never holds another instance's commits". That is not so: catch-up puts the commits of other instances into the cache, so an instance that fixed the state of a read while it was a spare could take the lease and catch up before a check that came only afterwards. The review of the build found this. `DB.Read` checks a second time after the state is fixed, which only keeps a read from being served late.
- **A call that waits for its lock waits no longer than the lease has left** (`DB.acquireServing`). Nothing wakes a caller when the lease runs out by a clock, so the wait ends then and looks again. `DB.Wait` returns only when the lease is valid by the clocks too, and is woken when a renewal makes it valid again.
- **A lease lasts a year at the most,** so that no sum of a time and a lease leaves the range of a duration. The duration of another holder comes from the manifest and can be any, so the wait for it is computed without such a sum.

## A swap whose outcome is unknown

The rules are in the todo item. One mechanism serves them, for commits and for renewals:

- **Every swap that the holder sends is a swap from the one version that it knows.** A swap that ends without an answer is kept as a `flight`.
- **At most one flight is ever applied,** because all flights are swaps from the same version. Any swap from that version that is applied rules all the others out for good. A version that a holder wrote never comes back, because of the salt.
- **`DB.observe` reads the manifest and tells three cases.** The manifest is unchanged. Or it is the manifest of one of the flights, which the instance then adopts. Or it is another manifest, and the lease is lost.

What follows from the mechanism:

- **A commit can be sent while the outcome of a renewal is unknown.** If the renewal turns out to be applied, the commit is sent again as a new swap from the new version.
- **After a refusal with an unchanged manifest, the same swap is sent again.** This is the 409 of S3.
- **After the lease is lost, a commit with an unknown attempt is looked for in the log** (`Tx.inLog`). The walk goes back from the new head to the parent of the transaction.
- **While `Tx.Commit` still waits, it pushes the commit:** it sends the same swap again.
- **After the caller gave up, the lease task does not push the commit.** It reads the manifest. If the manifest is unchanged, the lease task renews the lease without the commit, and that renewal is the swap that rules the stuck request out.
- **`Tx.Commit` goes on finding out when the instance stops meanwhile.** The caller is owed an answer for as long as it waits, and the answer comes from the store. The error for an unknown outcome wraps nothing but `ErrOutcomeUnknown`, so that a caller that tests for `ErrStopped` first is not told "not committed" for a commit that may be in the log.
- **One more case gives "outcome unknown" without the caller giving up:** the lease is lost, the commit must be looked for in the log, and the log holds a commit object that can never be read. Nothing can tell then. The spec says "only if the caller gives up first", so this is a departure that the build owns up to.
- **The transaction of the cache is rolled back when `Tx.Commit` returns "outcome unknown".** If the commit object turns out to be in the log, catch-up applies it from the store. There is then one path, and it is the path of every start.

## Catching up and replay

- **Catch-up downloads every missing commit object twice.** It walks the log back from the head to the cache's position, and it reads each commit object on the way to its end, so that it trusts a parent only when the bytes match the name. It then applies the commit objects, oldest first, and downloads each again. A spool of the walk could remove the second download later.
- **The holder renews its lease during a long catch-up** (`DB.keepLease`): between commit objects, and between the changes of one.
- **A download that fails is tried again where it failed** (`DB.fetchAgain`). The first build started the walk from the head again after any failure, so a cache that was N commit objects behind needed N downloads in a row to succeed, and with a store that fails one download in twenty, a log of 30 commit objects was never caught up. The review of the build found this.
- **Replay is strict.** A commit object that the reader accepts and that does not fit the cache stops the instance with `ErrLogBroken`. Examples are a table that is there already, a row to delete that is not there, and a value of the wrong storage class. The cache is right up to the parent, so the commit object is wrong.
- **Before replay refuses a commit object, it reads the commit object to its end** (`authentic`). Bytes that were damaged on their way must give "does not match its name" and a second download, not a stop.
- **A commit object that the store does not have is `ErrLogBroken` too.**

## Stopping

- **`DB.stop` is for three things:** a local I/O failure, something in the store that the instance cannot read, and `DB.Close`.
- **The lease task makes the one attempt to release the lease** (`DB.releaseOnce`).
- **`DB.Close` then tries again** (`DB.release`). It reads the manifest and releases the lease while the manifest names the instance, for at most as long as the lease lasts. A clean shutdown therefore releases the lease also after a failed first try.
- **The SQLite codes that are a local I/O failure are those of the list in the todo, exactly.** SQLite's `INTERNAL` is not in that list, and the core does not treat it as one.

## Waiting

- **A wait for a condition needs nothing new from the host.** The wait is `host.Host.Sleep`, or the `Acquire` of a semaphore, with a context that is derived from the caller's. Whoever changes `stopped`, `holder`, or `serving` cancels the contexts that are registered (`DB.waitLocked`, `DB.wakeLocked`).
- **Three things wait this way.** `DB.Wait` waits until the instance serves. A writer or a reader that waits for its lock fails at once when the instance stops serving. The lease task is woken from its pause.
- **Polling was rejected,** because a call that waits must fail "at once". The toy of the harness polls with `Sleep`; see [The host interface](../sim/README.md#the-host-interface). The build was free to add a method to the host, and none was needed.

## Planted bugs

The reason for planted bugs is in [Conditions for keeping the design](../sim/README.md#conditions-for-keeping-the-design): a check can pass for the wrong reason.

- **The tests plant wrong decisions in the real code.** `Options` has an unexported field, `bugs`. Each field of it switches on one wrong decision, at the one place where the right decision is made. Nineteen fields are for the simulation, and one is a data race for the race detector.
- **A program that uses the package cannot switch a bug on.** Only the package's own test files can set the field, through `export_test.go`.
- **The cost is that the wrong branches are compiled into the package,** each behind a test of one boolean.

## Events

- **The instance logs one wide event for each unit of work,** through `Options.Logger`, which is a `*slog.Logger`. The names of the events and of the attributes are in one place, `telemetry.go`.
- **There is no OpenTelemetry, no trace, and no metric yet.** They are decided with the public driver.

## What the package does not do, and what to know

- **A catch-up downloads every missing commit object twice.** See [Catching up and replay](#catching-up-and-replay).
- **Replay holds one batch of rows decoded.** That is about twenty mebibytes for a batch of one mebibyte, whatever the size of the commit object.
- **No test shows that the margin is enough.** The tests check that an instance waits for the holder's duration and the margin (`run.verifyHistory`). The harness cannot make a clock run fast, so the fault that the margin is for stays out of the workloads; see the comment of `clockFaults` in `simulation_test.go`.
- **The version of a manifest without a lease entry can come back.** Such a manifest has the same bytes whenever the head is the same. A swap that takes a free lease and is stuck in the network can therefore be applied after a later release. It adds no commit. The cost is that the manifest names a holder that does not know it, and the next instance waits for that lease to time out.
- **The simulation does not reach SQLite's "busy".** The write connection opens first, and every connection has a busy timeout, as the todo says. Both guard against "busy" under real parallelism, which the scheduler never produces; see [What the scheduled mode cannot do](../sim/README.md#what-the-scheduled-mode-cannot-do).
- **The manifest has no guard against bytes that are damaged on their way.** A commit object is checked against its name and downloaded a second time. A manifest that is damaged and no longer parses stops the instance at the first download. One that is damaged and still parses would be believed, and the swap that takes the lease would write it back, because the swap is from the version that the store gave, not from the bytes. The store's contract promises content that is not changed, and the design has no checksum for the manifest.
- **There are no snapshots and no garbage collection.** Nothing deletes a commit object of the log, or one that was uploaded and never committed.
- **There is no branch other than `main`.**

## Tests

`model_test.go` holds a second, simpler implementation, in maps. `mstate.apply` replays a commit object strictly: a key that appears twice is an error, and so is a delete of a row that is not there. `mtx` computes the net effect of a transaction. `model` reads the log through the direct view of the simulated store.

`simulation_test.go` holds the harness. Callers create and drop tables, upsert, delete, get, and scan, and every result is compared with the model. The tables have three names in two spellings each, with characters that must be quoted, and four shapes. The doc comment of `workload.run` lists the invariants, and the doc comment of `run.verifyHistory` lists the checks of the store's history.

`workloads_test.go` holds the three workloads, `singleWorkload`, `fullWorkload`, and `hammerWorkload`. It also holds the two aimed workloads: `competing` for the rules of the lease, and `unknownOutcomes` for a swap whose outcome is unknown. The list `plantedBugs` gives each bug its workload, and a control where the bug needs one ability of the harness or one fault.

`free_test.go` runs the workloads on real goroutines, for the race detector. `TestFreeRunning_plantedDataRace` runs the test binary again, and requires the race detector to report a planted data race with the scheduler off.

`script_test.go` holds a store that does to chosen requests what a test spells out. The scenario tests in `lease_test.go`, `tx_test.go`, `replay_test.go`, and `core_test.go` use it, and they run on the fake clock of `testing/synctest`. The tests of each clock of the lease check (`TestLease_clocks`) and of the cache that fails after the manifest swap (`TestDB_stop`) run one instance under the scheduler, because only it can move one clock and fire one buggify point.

By default, `TestSimulation` runs 40 seeds of the workload with one instance, 30 of the workload with everything, and 10 of the workload whose callers never pause. Every seed runs twice, and the traces are compared. The events of the instances are part of the trace. `TestSimulation_plantedBugs` runs the workload of each bug until four seeds have found the bug, from at most 200 seeds. It runs the control and the same workload without the bug for 24 seeds each.

The environment variables:

- `S4QL_SIM_SEEDS` sets the number of seeds for every workload and every bug, and the number of free runs. The workload of each bug then runs all of them, to count how many find the bug.
- `S4QL_SIM_SEED` makes `TestSimulationSeed` run one seed and log its whole trace. `S4QL_SIM_WORKLOAD` chooses the workload: `single`, `full`, or `hammer`.
- `S4QL_SIM_BUG` makes `TestSimulationSeed` run the workload of the planted bug that it names. `S4QL_SIM_CONTROL` then runs the control of that bug, and `S4QL_SIM_CLEAN` runs the workload without the bug.
- `S4QL_CORE_ROWS` sets the number of rows of `TestTx_Commit_large`.

The full run:

TODO(core-builder): the command with its -timeout.

### What was measured

TODO(core-builder): the numbers of the 10000-seed runs, the planted bugs, and the mutation testing.
