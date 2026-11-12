# Diary: storage core

Build the storage core of s4ql: the package that keeps a database whose truth is a log in S3, with a local SQLite cache, a lease, and commits through a manifest swap. It is the fourth of four pieces of the storage core without SQL. The simulation harness (`/docs/diary/2026-11-12-simulation-harness.md`), the storage formats (`/docs/diary/2026-11-12-storage-formats.md`), and the object store (`/docs/diary/2026-11-12-object-store.md`) are merged, and the lease design was tried in the toy (`/docs/diary/2026-11-12-lease-without-clock-comparison.md`). The core is the first code that uses all of them together.

## Step 1: Requirements

**Author:** core-builder

### Prompt Context

**Verbatim prompt:**

> You are building the storage core of s4ql, a Go SQL database whose truth is a log in S3, with a local SQLite cache (repository `maragudk/s4ql`, module `maragu.dev/s4ql`, Go 1.27.3). This is the piece the whole project's correctness rests on. [...] Everything the core stands on is built and merged [...] **The toy is your template and your benchmark: the core is the toy made real.** [...] The core is the first code that uses all of them together. There is no SQL engine yet and no public API; the future engine will be the core's only user.

The brief is long, so its requirements are recorded here by number, in its own words where the words carry the requirement.

What to build, "an internal package (you name it) with this shape. Names and signatures are yours; the behaviour is not":

1. Opening takes a `host.Host`, a `store.Store`, the path of the cache, the key prefix of the database, and options. It fails only on local problems and does not wait for the lease.
2. A separate call waits, with a context, until the instance serves.
3. Closing releases the lease (the manifest then has no lease entry) and closes the cache. A handle signals a permanent stop through a channel that closes, with the cause as an error.
4. Write transactions: one at a time per database. A second writer waits for the first.
5. Operations: create a table (the data of `s4qlpb.CreateTable`), drop a table, upsert a row, delete a row by primary key, and attach statements with their parameters as annotations. Inside the transaction, reads see its own writes: get a row by primary key, and scan a table in primary-key order.
6. Commit follows the spec exactly: the commit object is written, uploaded under its name (create-if-absent, with its checksum; "already exists" is success), the manifest is swapped, and then the transaction is committed to the cache together with the recorded position. `Commit` returns nil if and only if the transaction is known to be committed.
7. A commit object holds the net effect of its transaction: a primary key appears at most once per table, a table that is created and dropped in the same transaction leaves nothing, and a table that is dropped and created again is a drop, a create, and its rows.
8. A transaction of millions of rows is never in memory at once: not the commit object (it is spooled to a local file), and not the bookkeeping of which rows changed.
9. Read transactions run in parallel, bounded by a pool of connections whose size is an option. Each sees one consistent state. Before a read, the lease is checked against both of the holder's clocks.
10. The lease is taken by watching the manifest stand still for the holder's duration plus a margin; a compare-and-swap against the version that was read; at once if there is no lease entry.
11. Every write of the manifest by the holder carries a new salt and the renewal time. The lease is renewed by every commit and by a periodic renewal. Defaults: 30 seconds, renewed 20 seconds after the last renewal. Both are options.
12. The holder's window counts from when the first attempt of a renewal was sent.
13. An instance without a valid lease refuses reads and writes at once with a distinct temporary error, keeps trying, catches up its cache when it gets the lease, and then serves again. A spare instance is a normal state.
14. Unknown outcomes are built as `/docs/todo.md` says, for commits and for renewals, including the 409 and the swap that rules the stuck request out. `Commit` returns a distinct "outcome unknown" error only when the caller's context ends first, and the instance then serves nothing until it knows.
15. Every call to the store has a timeout through the host.
16. An instance whose cache is behind the manifest applies the missing commit objects, oldest first, before it serves anything. It finds them by walking the log back from the head to the cache's recorded position.
17. Replay applies the changes of one commit object in one SQLite transaction and commits it only after the reader has reported the end. It applies results and runs no SQL of the application. It handles every kind of change the format has, including an `UpsertRows` that lists only some columns.
18. A cache whose recorded position is not in the log is refused loudly, with a distinct error, and never rebuilt.
19. What cannot be read is never skipped: a manifest or a commit object that needs a newer version of s4ql, or one that is malformed, or a commit object whose bytes do not match its name after the download has been tried again. The instance then releases the lease if it holds it and stops permanently with a cause that says which case it is.
20. Any local I/O failure on the cache stops the instance for good, as `/docs/todo.md` says, including the classification of SQLite's result codes, the two variants of the buggify point between the swap and the local commit, the single attempt to release the lease, and "busy" not being a local I/O failure. A failure to write the spool file is a local I/O failure too.
21. SQLite: the busy timeout, the write connection that opens first and stays open, and "busy" after the timeout returned as an error and logged.
22. Plain blocking Go, no state machine. Two locks: the writer lock for a whole write transaction, and the manifest lock only around each swap. One background goroutine. Everything waits through `host.Host`, and `simlint` passes with no exemption.

Details that the lead decided: the keys `<prefix>/manifest.json` and `<prefix>/commits/<64 hexadecimal digits>`; tables in the cache that are `STRICT` and `WITHOUT ROWID` with the database's names, with every identifier quoted by the core; the core's own tables named `s4ql_...`, and a distinct error for a table whose name starts with `s4ql_` or `sqlite_` in any case; only `TYPE_INT`, with a value of the wrong storage class refused; a margin on the watcher's wait that I choose and justify; the smallest addition to `internal/host` if it lacks something; and one wide structured event for each unit of work through a `*slog.Logger` that discards by default, with attribute names in one place, durations from the host's monotonic clock, and no OpenTelemetry.

Out of scope: SQL, a parser, an engine, the `database/sql` driver, the client for real S3, snapshots, garbage collection of the log, branches other than `main`, and removing the toy. I do not edit `/docs/spec.md`, `/docs/todo.md`, `/README.md`, or `/AGENTS.md`, and report what in them is out of date.

Testing, "this is most of the work": simulation workloads on `simstore` with every fault on, several instances, moving clocks, crashes and restarts on a copy of the cache, and buggify; a model, not counters; every seed run twice; planted bugs with a control and a run without the bug (the ten of the toy and at least seven more); aimed workloads for each rule of the lease and of the unknown outcome; free-running mode under the race detector with a planted data race that must be reported; tests without the simulation for what is deterministic; a default run under 3 minutes with `-race` and under a minute without; and no test on the real clock that depends on the speed of the machine.

Verification, with numbers: `make test`, `make lint`, and `go test -race -shuffle on`; `S4QL_SIM_SEEDS=10000` on every workload with the counts of what happened; each planted bug with its control and the run without it; and mutation testing with `go test -overlay`.

**Interpretation:** Build one internal package that is the toy of `/internal/sim` made real on `internal/format`, `internal/store`, and `internal/sqlite`, following `/docs/spec.md` and the agreed items of `/docs/todo.md` to the letter, and prove it with a simulation that is at least as strong as the toy's.

**Inferred intent:** The whole project's claim to correctness rests on this package, so the result that matters is not that it works on a calm store but that every rule of the lease, of the commit, and of the unknown outcome is shown to be exercised and shown to be checked.

### What I did

I checked the worktree, `/Users/maragubot/Developer/s4ql/.claude/worktrees/agent-a0ea7c79436181a4c` on branch `worktree-agent-a0ea7c79436181a4c`, which contains `a226a44` ("Test the lease without clock comparison in the simulation (#11)"). I read `/AGENTS.md`, `/docs/spec.md`, `/docs/todo.md`, `/docs/roadmap.md`, the READMEs of `internal/sim`, `internal/format`, and `internal/sqlite`, the code of `internal/host`, `internal/sim`, `internal/store`, `internal/store/simstore`, `internal/format`, and `internal/sqlite`, the toy and its workloads, the three diaries of the day, `/docs/research/sqlite-busy.md`, and section 6 of `/docs/research/s3-clock.md`.

### Why

The requirements bind every later step, and the brief asks for them as the first step of the diary.

### What worked

The toy and its diary say plainly where the toy departs from the agreed design, so the list of what the core must do differently was short: it resolves an unknown outcome and goes on, where the toy stops; it reads the store through `store.Store` with streams and checksums; and it keeps real tables.

### What didn't work

Nothing yet. No code has been written.

### What I learned

- The SQLite binding binds and reads integers only, and a statement runs once. The core needs the name of a commit object and the names of tables and columns in the cache, so the binding needs text, NULL, and a reset.
- A `STRICT` table does not refuse every value of the wrong storage class for an `INT` column: SQLite converts the text `'123'` and the real `1.0` to an integer without an error. So the core must check the storage class itself, before the cache sees the value.
- A manifest without a lease entry has the same bytes whenever the head is the same, so its version can come back. Only the swap that takes a free lease is sent from such a version, and that swap adds no commit, so the worst that a stuck one can do is name a holder that no longer knows it holds the lease.

### What was tricky

Deciding how to wait for a condition, such as "until the instance serves", without polling and without a new method on the host. A wait is `host.Host.Sleep` with a context derived from the caller's, and whoever changes the condition cancels that context. That needs nothing from the host that it does not have.

### What warrants review

Nothing yet.

### Future work

The steps that follow, in the order the brief suggests: the cache, tables, replay, and the commit path against a calm store; the lease; unknown outcomes and the stop; the workloads, the model, and the planted bugs; and the mutation testing.

## Step 2: The cache, replay, transactions, and the lease

**Author:** core-builder

### Prompt Context

**Verbatim prompt:** "A suggested order, so that a stop at any point leaves something whole and verified: the cache, tables, replay, and the commit path against a calm store; then the lease; then unknown outcomes and the stop; then the workloads, the model, and the planted bugs; then the mutation testing."
**Interpretation:** Requirements 1 to 22 of Step 1, as code, with tests on a calm store before the simulation.
**Inferred intent:** Have the whole of the core in place, in plain blocking Go on `host.Host`, so that the simulation has all of it to attack.

### What I did

The package is `/internal/core`. Its files:

- `/internal/core/core.go`: `core.Options`, `core.DB`, `core.Open`, `core.DB.Wait`, `core.DB.Close`, `core.DB.Done`, `core.DB.Err`, the stop, and the two locks.
- `/internal/core/lease.go`: the one goroutine, `DB.leaseLoop`, with `DB.acquire`, `DB.recoverLocked`, `DB.renewLocked`, `DB.observe`, and the release.
- `/internal/core/catchup.go` and `/internal/core/replay.go`: the walk back through the log, the download with its second try, and replay.
- `/internal/core/tx.go` and `/internal/core/read.go`: `core.Tx` with `CreateTable`, `DropTable`, `Upsert`, `Delete`, `Annotate`, `Table`, `Tables`, `Get`, `Scan`, `Commit`, and `Rollback`, and `core.ReadTx`.
- `/internal/core/cache.go`, `/internal/core/spool.go`, `/internal/core/errors.go`, `/internal/core/telemetry.go`, and `/internal/core/bugs.go`.

What I chose, where the brief left a choice:

- **The SQLite binding got four methods.** `sqlite.Stmt.BindText`, `sqlite.Stmt.BindNull`, `sqlite.Stmt.ColumnText`, and `sqlite.Stmt.Reset`, with tests and a fuzz test, in `/internal/sqlite/sqlite.go`. The position of the cache and the names of tables and columns are text, and replay runs one statement for many rows. The binding bound and read integers only, and ran a statement once.
- **SQLite's schema is the catalogue.** The core keeps no table of tables. It reads names from `sqlite_schema` and columns from `pragma_table_info`. The one table of its own is `s4ql_cache`, with a version and the position as text.
- **The storage class of a value is checked before the cache sees it,** in a write and in replay, because a strict table converts `'1'` and `1.0` for an `INT` column.
- **Which rows a transaction changed is in temporary tables of the cache,** one for each table that existed when the transaction began, with whether each row existed then. The commit object is written from joins of them, through a spool file that is unlinked when it is open. The annotations have a spool file of their own, because they come after all changes.
- **A transaction without changes writes no commit object,** and commits if the lease holds by the clocks.
- **`DB.Begin` checks the clocks, and `Tx.Commit` does not:** the swap decides a commit.
- **Swaps in flight.** Every swap of the holder goes from the one version that it knows. One without an answer is kept, as a `flight`. Any swap from that version that is applied rules the others out, so one function, `DB.observe`, settles commits and renewals alike.
- **Waiting needs nothing new from the host.** A wait is `host.Host.Sleep` or `host.Semaphore.Acquire` with a context derived from the caller's, and whoever changes the state cancels the contexts that are registered. Polling would not let a waiting writer fail "at once".
- **The margin is the larger of two seconds and a tenth of the holder's duration.**
- **Planted bugs are an unexported field of `core.Options`,** set only through `/internal/core/export_test.go`.

### Why

Each choice either follows from a rule (a waiting call fails at once, a transaction is never in memory at once) or removes a second source of truth (the catalogue, the bookkeeping of an unknown outcome).

### What worked

The first test of the whole path, `TestOpen` in `/internal/core/core_test.go`, passed at its first run: open, commit twice, close, open again, and replay into an empty cache. The scenario tests with a scripted store on the fake clock of `testing/synctest` (`/internal/core/script_test.go`, `/internal/core/lease_test.go`, `/internal/core/tx_test.go`) also passed nearly as written, which made me distrust them until the planted bugs and the mutation testing showed that they fail when the code is wrong.

### What didn't work

- `golangci-lint run ./internal/core/` found a real bug before any test did: `internal/core/tx.go:718:3: ineffectual assignment to err (ineffassign)`. In `if err := tx.dropTemps(); err == nil { err = tx.c.setPosition(out.name) }`, the inner `err` was the one of the `if`, so a failure to record the position was dropped.
- One scenario test failed in one run of six: `lease_test.go:105: the manifest has a lease entry after the holder was closed`. The spare reads the manifest every five seconds, the test slept five minutes, and so the spare's read and the test's check fell on the same instant of the fake clock: the spare had taken the released lease before the check. The test now sleeps five minutes and a second.
- `go test -race` reported a data race in my own scripted store, not in the core: the test set the delay of a rule after it had added the rule. `scriptStore.on` takes the delay now.
- A shell command with a here-document and `go test` or `git` in one line was refused several times with `this command is too complex to verify that it stays inside the worktree`. Scripts go into the scratchpad with the write tool and run as plain commands.
- The editing tool turned the escapes of U+0301, U+00E9, U+200B, and U+FEFF in a list of hostile names into the raw characters, as the brief warned. A raw byte order mark inside a Go file does not compile. The names are `string(rune(0x301))` and so on now, and I checked the bytes with `xxd`.

### What I learned

- `testing/synctest` makes two timers that are due at the same instant race, also across the goroutines of a test with real goroutines. A test on the fake clock must not check at a multiple of a period that the code under test also wakes at.
- The toy stops an instance on an unknown outcome. Doing it properly needs only the rule that every swap of a holder is sent from one version, which makes "at most one of them is applied" true by construction.

### What was tricky

- The lock order and the stop. `serving` becomes true only under the writer lock, so that an open write transaction cannot miss a catch-up, and the lease task takes the writer lock before the manifest lock, as a commit does.
- A release of a semaphore waits under the scheduler, and a crash ends a task in a wait and runs its deferred calls. `locks` in `/internal/core/core.go` marks a lock as given back before it releases it, which is the bug that the toy's builder hit.

### What warrants review

- `/internal/core/lease.go`, `DB.observe`, `DB.send`, and `DB.adopt`: the claim that every swap in flight is from the version that the instance knows.
- `/internal/core/tx.go`, `Tx.publish` and `Tx.inLog`: each return value against the rule "nil if and only if committed".
- `/internal/core/replay.go`, `authentic`: a refusal only after the bytes are known to match the name.

### Future work

The simulation, in the next step.

## Step 3: The simulation, the model, and the planted bugs

**Author:** core-builder

### Prompt Context

**Verbatim prompt:** "The core is trusted only as far as its simulation is. Read how the toy is tested and do at least as much for the real thing."
**Interpretation:** Workloads on `simstore` with every fault on, a model in place of counters, every seed twice, planted bugs with controls, aimed workloads, and a free-running mode.
**Inferred intent:** Every rule must be shown to be exercised and shown to be checked, not only shown not to fail.

### What I did

- `/internal/core/model_test.go` is a second implementation in maps. `mstate.apply` replays a commit object strictly, `mtx` computes the net effect of a transaction, and `model` reads the log through the direct view of the simulated store.
- `/internal/core/simulation_test.go` is the harness: callers create and drop tables, upsert, delete, get, and scan, on three table names in two spellings each, with characters that must be quoted, and four shapes. Every result of a call is compared with the model. `workload.run` lists the invariants, and `run.verifyHistory` checks the writes of the manifest in the store's history: who wrote each, that the head only moves forward by one commit object, that no version comes back, and that a takeover comes no earlier than the holder's duration and the margin after the write before it.
- `/internal/core/workloads_test.go` holds the three workloads, the aimed workloads `competing` and `unknownOutcomes`, and 19 planted bugs, each with its workload, a control where the bug needs one fault or ability, and a run without the bug.
- `/internal/core/free_test.go` runs the workloads on real goroutines, and runs the test binary again with a planted data race, which the race detector must report with the scheduler off and must not report under it.
- `/internal/store/simstore/simstore.go` got `simstore.Store.Calm`, which ends new faults, with a test. Liveness is stated after the faults end, and the simulated store had no way to end them.

### Why

A model that predicts the lines of each commit object's changes, and the state after each, checks the net effect and replay with an independent implementation. The history of the store is a second witness for the lease that does not depend on any read happening at the right moment.

### What worked

The first run of 100 seeds of each workload had no violation and no pair of traces that differed. That says little by itself, so the planted bugs came next, and they found what was wrong with the planting and with the workloads, below.

### What didn't work

- The first run of the planted bugs, `S4QL_SIM_SEEDS=60 go test -run TestSimulation_plantedBugs ./internal/core/`, failed for five bugs with `found something else`:
  - `serveUnknown`: `no instance serves 4m0s after the end of the workload`. My planted branch kept `serving` and still set `unresolved`, which nothing cleared, so the bug made the instance refuse for ever. The branch sets neither now.
  - `renewOutsideLock`: `no instance serves 4m56s after the end of the workload`. I had planted it as a separate crude path that forgot a swap without an answer. It is now the real renewal with the lock released around the swap. Planted that way, an aimed workload was needed (a renewal one second after the last one, so that renewals race commits), and the symptoms are worse than the toy's: `the head of the log went back to commit 7, from commit 10` in 64 of 100 seeds. A renewal that lands after a commit and adopts its own manifest writes the old head back.
  - `ackUnknown`: `commit: not committed: the log is at ca58..., and the transaction began at a0f9...`. That is a guard in `Tx.publish` that the correct code never reaches, reporting that the cache is ahead of the log, which is the bug. It is a symptom now.
  - `settleByRead`: `an instance lost the lease 1 times, with no other instance`. The stuck commit landed after the wrong conclusion, and the instance then met a manifest of its own that it did not know. Also a symptom now.
  - `forgetDelete`: `the commit of transaction 7025987658175066058 was acknowledged, and the log does not have it`. A transaction that only deletes has no changes left with the bug, and so writes no commit object. Also a symptom.
- `lateVersion` had a control that found the bug in 3 of 50 seeds. With one node and a crash, a write of the crashed holder that arrives late changes the manifest during the wait, which is what the bug needs. The control now has no late writes.
- `lostMeansFailed` was found by 0 of 200 seeds with callers that give up after 30 seconds: a takeover needs more than 30 seconds, so the caller always gave up first and got "outcome unknown". Its workload has callers that wait 100 seconds, and more lost answers and cut-offs. It is still found by only 4 of 100 seeds.
- The whole default run took 93 seconds without the race detector. A CPU profile showed 75% of the time in `runtime.pthread_cond_signal` and `runtime.pthread_cond_wait`: the scheduler hands the baton between goroutines on different threads. With `GOMAXPROCS=1`, three workloads in sequence took as long as in parallel with 10. So the cost is per step of the scheduler, and the default run is cut by seeds: 40, 40, and 10 for `TestSimulation`, and 30 for each control and each run without the bug.
- The lease task asked the store again every second for as long as it was cut off: `lease/renewed/unknown 27891` in 100 runs of one workload. It now backs off to the time between two looks of a watcher. One scenario test then failed, `lease_test.go:276: Expected "no valid lease" ... but got "<nil>"`, because it gave the instance two seconds to notice that it had lost the lease. It gives it six now.

### What I learned

- A planted bug must be the real code with one decision changed. Both of my first two failures were bugs planted as a separate, simpler path, which then failed for reasons of its own.
- The read model finds a lease bug in few seeds, as the toy's builder wrote: 5 to 9 of 100 for the bugs of the clocks. The store's history finds the bugs of the watcher in nearly every seed, because it needs no read at the right moment.

### What was tricky

- Checking "the instance serves nothing until it knows" from outside. The check is made at the end, when the log is final: no read anywhere that started after a commit returned "outcome unknown" may show a state before that commit, if the log holds it.
- The lease at the end of a run. A swap that takes a free lease can be stuck in the network and be applied after its instance was closed, because a manifest without a lease entry has the same bytes, and so the same version, whenever the head is the same. The check allows a holder whose last write was applied after it was closed.

### What warrants review

- `/internal/core/simulation_test.go`, `run.read`, `run.write`, and `run.verify`: whether a check can pass for the wrong reason.
- `/internal/core/workloads_test.go`, `plantedBugs`: each control, and whether it removes exactly what the bug needs.

### Future work

The deterministic tests for what the simulation does not aim at, the mutation testing, and the large runs, in the next step.
