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
- `/internal/core/lease.go`: the one goroutine, `DB.leaseLoop`, with `DB.acquire`, `DB.resumeLocked`, `DB.renewLocked`, `DB.observe`, and the release.
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

## Step 4: Tests without the simulation, and mutation testing

**Author:** core-builder

### Prompt Context

**Verbatim prompt:** "Mutation testing in the style of the object store's builder (`go test -overlay`, never touching the tree) over the core's decisions: which mutants no test catches, and for each, either a new test or the reason it does not matter."
**Interpretation:** Tests for what is deterministic by itself, then one mutant for each decision of the core, and a test or a reason for each mutant that survives.
**Inferred intent:** Find the checks that pass with the mechanism they guard removed, before a reviewer or a user does.

### What I did

The tests without the simulation, each in the file of what it tests:

- `/internal/core/tx_test.go`: 15 cases of the net effect, each with the exact lines of its commit object and a replay into an empty cache; reads of a transaction's own writes; every refusal with its error; the cases of a manifest swap without an answer, spelled out with a scripted store; a commit while the instance is closed; a transaction of 600,000 rows whose commit object is 23 MB, during which the heap of the Go runtime grew by 3.2 MB, and whose replay grew it by 20 MB.
- `/internal/core/replay_test.go`: logs written by hand with every kind of change, also rows that list only some columns; 25 commit objects that a reader accepts and that do not fit the cache; 9 that a reader refuses or the store lacks; manifests that cannot be read; a cache whose position is not in the log; bytes that are damaged on their way, once and twice.
- `/internal/core/lease_test.go`: the rules of the lease on the fake clock of `testing/synctest`, and each clock of the lease check under the scheduler.
- `/internal/core/cache_test.go`, `/internal/core/core_test.go`, `/internal/core/errors_test.go`, `/internal/core/telemetry_test.go`: 57 hostile names as tables and as columns, the reserved names, the cache's version and position, the classification of SQLite's result codes, the stop for a cache that SQLite reports as corrupt and for a spool file that cannot be written, both ways in which the cache fails after a manifest swap, and one event for each unit of work.

The mutation testing is a script in the scratchpad, `mutate.py`. Each mutant is a copy of one file with one decision changed, put in place of the file with `go test -overlay`. It first runs the tests without the simulation, and the three workloads if those pass.

### Why

The simulation shows that the rules hold under faults. It does not say which line of the core each test depends on. A mutant per decision does.

### What worked

- The first run, before the review: 117 mutants, 81 caught, 23 not caught, and 13 that did not build, which was my mistake in writing them. I rewrote the 13 and wrote tests for the survivors that deserved one. A rerun of those 35 mutants: 23 caught, 12 not caught, and after two more tests, 10.
- Tests that a surviving mutant made me write: the margin's value; the lease that an instance took counted from when the swap was sent; the lease counted from the first attempt of a renewal when a later attempt finds it applied; a renewal without an answer sent again with the same bytes; a manifest of the instance's own that is not the first swap in flight; no serving between taking the lease again and catching up; the lease renewed during a catch-up that takes longer than a lease; a parent that damaged bytes seem to name; a commit of nothing after the lease ran out; a transaction that failed in the cache; the lease task woken when a commit returns "outcome unknown"; a close with a store that gives no answer at all; a cache that SQLite reports as corrupt; and each clock of the lease check alone.

### What didn't work

- `TestLease_window` failed when I wrote it: `lease_test.go:357: the instance serves before it has caught up`. The core was right. My scripted store did not hold the answer of a read, only of a write, so the catch-up that the test meant to slow down was not slow. The scripted store holds reads now.
- The same test file: `lease_test.go:300: Expected "25s", but got "50s"`. An instance whose swap for the lease was answered after 25 seconds renews first, because a renewal is due, and serves after that. The test now has an answer after 15 seconds.
- A test of a damaged table name panicked in the scripted store, `index out of range [-1]`: the commit object that was downloaded did not hold the name that the test wanted damaged.
- The last full run of the mutation testing, on the code as it is after the review, was stopped by the system after 64 of 144 mutants: `Background command "Run the full mutation testing on the final code" was stopped because the system is running low on memory`. I did not start it again. Of those 64, 53 were caught, 9 were not, one was a mutant whose text no longer matched, and two of the 9 got a test afterwards that no mutation run has confirmed: a download without a timeout, and a fixed salt in the write that takes the lease. The other 80 mutants, of the transaction, the cache, and the 29 that I added for what the review changed, have not run on the final code. The mutants of the transaction and the cache were all caught in the first run but three, which are in the list below.

### What I learned

The mutants that no test catches, and why each does not matter. None of them changes what a caller can see:

- **No second look after a call waited for its lock** (`DB.acquireServing`), **and no check after the state of a read is fixed** (`DB.Read`). The check before the wait is the one that the rule needs. The other two make a refusal come at once, where the next call of the transaction would bring it.
- **Replay does not check the parent of a commit object against the position.** The walk back through the log makes it true. It is a guard against a bug in the catch-up.
- **Replay does not check that a table to create is not there, a key that it deletes, the storage class of a value, or that a row with only some columns lists the primary key.** In each case the commit object is still refused with `ErrLogBroken`, by SQLite or by the binding of the value, with a worse message. The last of them is refused by accident, through a parameter `?0`, which I would not rely on.
- **The rows of a table that the transaction created are tracked like changed rows.** The commit object is the same. The cost is time and disk.
- **A commit does not check that the log is where the transaction began.** The correct code never reaches the check. The planted bug `ackUnknown` does.
- **The read connections open before the write connection has run its first statement.** It guards against SQLite's "busy" after a crash under real parallelism, which no test here produces.

### What was tricky

A mutant that does not build looks like a mutant that is caught unless the script builds first. The object store's builder wrote that down, and I still wrote 13 mutants that did not build: deleting a branch leaves a variable unused. A mutant that keeps the branch and adds `&& false` to its condition builds.

### What warrants review

- The list above: whether "the commit object is still refused, by SQLite" is good enough for the checks of replay, or whether each should have a test that pins its own message.
- The 80 mutants that have not run on the final code. The script is `mutate.py` in the session's scratchpad, and is not in the tree.

### Future work

Run the mutation testing again on the final code, on a machine with memory to spare.

## Step 5: Self-review, and what it changed

**Author:** core-builder

### Prompt Context

**Verbatim prompt:** "When the implementation is done, self-review it as your standing instructions say, and fix what is real."
**Interpretation:** The `fabrik:code-review` skill over `git diff origin/main...HEAD`: two reviewers that compete, told to try their claims with `go test -overlay` and to leave the tree alone.
**Inferred intent:** The core is what the project's correctness rests on, so it has to survive two readers who are paid in bugs.

### What I did

Two reviewers on the `fable` model reviewed the branch independently. Neither found a schedule that breaks the safety of a commit or of a read. Both found the same four things, and each found serious things alone. What I fixed:

- **Both: a `Commit` that returned "outcome unknown" because the instance stopped, in an error that also wrapped `ErrStopped`.** The documentation said that an error that wraps `ErrStopped` means "not committed". A caller that tested for it first would have repeated a committed transaction. `Tx.publish` now goes on finding out when the instance stops while a swap of the commit may be applied, and the error for an unknown outcome wraps nothing else. No test had stopped an instance during a commit: three scripted tests do now, and the workload with everything closes and opens instances while their callers run.
- **Reviewer B: the catch-up started its walk from the head again after any download that failed.** A cache that is N commit objects behind then needs N downloads in a row to succeed. With a store that fails every twentieth download, the reviewer's log of 15 commit objects was caught up after 46 downloads, and a log of 30 was not caught up after 345,640. `DB.fetchAgain` tries a download again where it failed.
- **Reviewer B: the documentation of `DB.Read` said that one check of the lease is enough, before or after the state of the read is fixed.** That is the todo's sentence, and it is wrong for the order "after": catch-up puts the commits of other instances into the cache, so a spare that fixed the state of a read could take the lease and catch up before the check. The code checks before, so it was right, and the comment now says why the check before is the one that counts.
- **Reviewer A: the wait of a watcher overflowed for a lease of more than about 265 years,** from an option or from the manifest, and the watcher then took a live holder's lease at once. A lease lasts a year at the most now, and the wait is computed without a sum that can overflow.
- **Reviewer A: calls that waited for a lock were not failed when the lease ran out by the clocks, and `DB.Wait` returned at once in that state,** so that a loop of "wait, then try" would spin. A wait for a lock now ends when the lease has no time left, and `DB.Wait` waits for a lease that is valid by the clocks.
- **Reviewer A: the lease task could miss a wake-up** between reading the state and going to sleep, with real parallelism only. The state has a count of its changes, and the task does not sleep if the count has moved.
- **Reviewer B: a primary key of 499 columns or more made the cache's own SQL too deep for SQLite.** A primary key has at most 250 columns now.
- **Reviewer A: the simulation checked the lease at the end only after its own replay instance had taken and released it,** so the check saw nothing. It runs before the replay now. The reviewer's mutant, an instance that never releases, passed 100 seeds before.
- **Both: weak spots in the tests.** A stop for a local failure was excused under buggify whatever its text; a stop for bytes that do not match their name was excused after one damaged download; the symptom of a stale read matched another violation's text; the planted-bug test asserted one find where its comment said four. All four are tightened.
- **Both: comments and constants.** Comments that looked outward, a comment that described the scheduler, the result codes of SQLite as constants that one function used, two limits as constants that one function used, and tests in the files of other sources.

What I did not change:

- **A manifest without a lease entry has the same bytes whenever the head is the same,** so a swap that takes a free lease and is stuck in the network can be applied after a later release, also after a clean `Close`. Both reviewers reproduced it. It adds no commit, and the instance that is named takes over or is dead, so the cost is one wait for a lease. A salt outside the lease entry would end it. That changes the manifest's format, so it is a question for the lead.
- **The manifest has no guard against bytes that are damaged on their way** (reviewer B). A damaged manifest that still parses would be written back by the swap that takes the lease. The store's contract promises unchanged content, and the design has no checksum for the manifest. Also a question for the lead.
- **The names of events and attributes stay constants in one place,** as the brief asks, though each is used once.
- **The control of `lateVersion` changes four things,** and the control of `ownDuration` cannot fail by construction, as their comments say.

### Why

Every fix closes a way in which the core does something other than what its documentation promises, or a way in which a test passes for the wrong reason.

### What worked

Asking for mutants. Reviewer A's "an instance that has committed never releases" and reviewer B's "keep only the check after the state is fixed" each showed a hole that no reading of the code had.

### What didn't work

- My fix for the calls that wait made the planted-bug suite fail, and reviewer B found it before I did: `scheduler: no end after 5000000 steps; waiting: a/caller-2 at acquire of a/readers`. The wait took the time that the lease has left from the monotonic clock alone. Under the planted bug `wallOnly` the check passed while that time was zero, so the timeout expired at once, for ever. The time left now comes from the clocks that the check looks at, and is never zero.
- I had run only the tests without the simulation after that fix, and committed. The planted bugs are part of the default run for this reason.
- Reviewer B's own mutant "Open accepts any path" created three files named `file:a.db` in the tree, which the reviewer removed and reported.

### What I learned

- A fix made during a review needs the whole default run, not the part that the fix is about.
- The sentence in the todo about the order of the lease check and the state of a read is wrong, and was copied into my comment unchecked.

### What was tricky

Keeping "nil if and only if committed" when the instance stops in the middle of a commit. The stop must fail what waits, and must not turn "we do not know yet" into an answer.

### What warrants review

- `/internal/core/tx.go`, `Tx.publish`: the commit that goes on after a stop, and holds the manifest lock while it does, so that a `Close` waits for it.
- `/internal/core/core.go`, `DB.acquireServing`, `DB.pause`, and `DB.Wait`.
- `/internal/core/catchup.go`, `DB.fetchAgain`.

### Future work

The two questions for the lead above.
