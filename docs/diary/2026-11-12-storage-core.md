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

## Step 6: Verification

**Author:** core-builder

### Prompt Context

**Verbatim prompt:** "`S4QL_SIM_SEEDS=10000` on every workload: no violation, no pair of traces that differ. Give the counts of what happened in those runs (commits, unknown outcomes, takeovers, crashes, each fault), so that I can see the faults really occurred. Each planted bug: how many seeds find it, its control, and the run without the bug."
**Interpretation:** The large runs on the code as it is after the review, with the numbers, and the default run's times.
**Inferred intent:** Numbers that someone else can check, on the code that is handed over.

### What I did

- `S4QL_SIM_SEEDS=10000 go test -timeout 6h -run '^TestSimulation$' ./internal/core/`: each of the three workloads, 10000 seeds, each seed twice. No violation and no pair of traces that differed. The workload with one instance ran on commit `512466d` (23 minutes), and the workload with everything (30 minutes) and the workload whose callers never pause (39 minutes) on `d9fad45`, after the fix below. In the workload with everything: 110225 commits acknowledged, 473 of unknown outcome, 38763 leases taken, 2706 leases lost to another instance, 10000 crashes with a restart, 17702 instances closed and opened again while callers ran, 7082 stops for a cache that failed after the manifest swap, 167174 moves of a clock, and every fault of the store between 4273 and 87789 times. The longest time until an instance served after a workload was 2 minutes 8 seconds, where the bound is 5 minutes 29 seconds. The tables are in `/internal/core/README.md`, "What was measured".
- `S4QL_SIM_SEEDS=1000 go test -timeout 3h -run '^TestSimulation_plantedBugs$' ./internal/core/`: every planted bug, its control, and its workload without the bug, 1000 seeds each, in 33 minutes. Every bug was found, by 53 to 1000 seeds. No seed found anything without the bug. I did not run the planted bugs over 10000 seeds: that is about five and a half hours on this machine.
- The default run: `go test -count=1 -shuffle on ./internal/core/` in 53 seconds, and with `-race` in 112 to 123 seconds, at a load of 13 to 17 on 10 cores from other sessions. `make test` passes in 78 seconds, with `internal/core` at 70 seconds beside the other packages, and `make lint` passes, with `simlint` and no exemption for the core.
- `TestFreeRunning` under the race detector: 30 runs of each workload on real goroutines, with no report, and `TestFreeRunning_plantedDataRace` gets its report of the planted race with the scheduler off and none under it.

### Why

The claims of the report rest on these runs.

### What worked

The 10000-seed run did what it is for. It found a fault that 600 seeds and the default run had not.

### What didn't work

- The first 10000-seed run of the workload with everything failed in 5 of the first 2700 seeds, each with a run that never ended: `seed 668: scheduler: no end after 5000000 steps; waiting: b/lease of 7e8cc65308108648a574c34f7a38bab2 at release of b/manifest`. It was my harness and not the core. The task that closes and opens an instance while its callers run, which I had added after the review, and the supervisor that opens an instance again after a stop could both open a database for the same node. The one that lost was never closed, and its lease task ran for ever. One task at a time does it now, behind a flag, and the five seeds pass.
- My first fix of that added a check that a close ends, and the check failed for seed 2369: `the close of b did not end: close: release of the lease: gave up after 27 attempts: simstore: request 77: context deadline exceeded`. The close had ended. Its error wrapped the timeout of one request, which my check took for the timeout of the close. The check looks at the context of the close now.
- Two controls found their bug in the count over 1000 seeds: `1 of 1000 seeds found the bug without what it needs` for the lease counted from the answer, and 3 of 1000 for the replay that commits early, such as `seed 986 found the bug without what it needs: [the cache of b2 holds at commit 3 the state "TBL 2;--(select*0): 0 | 1 | 3\nt0(select*0): ", and the log has "TBL 2;--(select*0): 0 | 1 | 3"]`. Neither control took away everything that its bug needs. An instance also learns late of a swap whose answer a cut-off lost, and the content of a download also breaks off when its client is cut off while replay renews the lease. The first control has one instance now, the second has no cut-offs and no outages, and both were counted again: 0 of 1000.

### What I learned

- The default run of 20 seeds of a control says little: both wrong controls passed it, and one of them passed 60 seeds earlier in the day.
- A harness fault can look like the worst kind of fault of the code under test, a run that never ends. What told them apart was the list of tasks that still waited: only a lease task, of an instance that no test task held any more.

### What was tricky

Running the large runs on a machine that 13 sessions share. The system stopped one background job of mine for lack of memory, and the times are from a load of 13 to 18.

### What warrants review

- The numbers in `/internal/core/README.md`, "What was measured", against the logs of the runs, which are in the session's scratchpad and not in the tree.
- The two controls that changed: `leaseFromAnswer` and `replayCommitsEarly` in `/internal/core/workloads_test.go`.

### Future work

- The planted bugs over 10000 seeds, and the mutation testing on the final code, on a machine with time and memory to spare.
- What the simulation still does not show: the margin on the watcher's wait, because no clock runs fast; SQLite's "busy" and the order in which the connections open, because the scheduler runs one goroutine at a time; a manifest that is damaged on its way; a cache that fails anywhere but at the commit after a manifest swap; a power loss; and clock faults on an instance that was restarted after a crash.

## Step 7: The leak under the address sanitizer, and the time of the tests

**Author:** leak-builder

### Prompt Context

**Verbatim prompt:**

> You are fixing a CI failure of the storage core of s4ql, a Go SQL database whose truth is a log in S3 (repository `maragudk/s4ql`, module `maragu.dev/s4ql`, Go 1.27.3). You work in your own git worktree: run `pwd` and `git branch --show-current` first and stay in that directory. Never write in `/Users/maragubot/Developer/s4ql` itself or in another worktree under `.claude/worktrees/`.
>
> **Your work builds on the branch of pull request #12, not on `main`.** First run `git fetch origin` and `git merge --ff-only origin/storage-core`, and check with `git log --oneline -1` that you are on the commit `8ff0a57` or a later one of that branch.
>
> Read `AGENTS.md`. Invoke the `fabrik:go` skill before you read or write Go code. Read `internal/core/README.md`, and `internal/sqlite/README.md` for how the SQLite binding works and how it is tested under the sanitizer.
>
> ## The failure
>
> `internal/core` is new: the storage core, with a SQLite cache through our own cgo binding in `internal/sqlite`. The CI job `asan` runs `make test-asan` (see the `Makefile` and `.github/workflows/ci.yml`): all tests under the C address sanitizer, on Linux. On pull request #12 it failed in the core's tests:
>
> ```
> ==6387==ERROR: LeakSanitizer: detected memory leaks
> Indirect leak of 887264 byte(s) in 604 object(s) allocated from:
>     #0 ... in malloc ../../../../src/libsanitizer/asan/asan_malloc_linux.cpp:67
>     #1 ... (/tmp/go-build.../b001/core.test+0xaed496)
> Indirect leak of 13728 byte(s) in 36 object(s) allocated from: ...
> Indirect leak of 2400 byte(s) in 40 object(s) allocated from: ...
> ... (ten groups in all)
> SUMMARY: AddressSanitizer: 907040 byte(s) leaked in 748 allocation(s).
> FAIL	maragu.dev/s4ql/internal/core	585.611s
> ```
>
> The stacks have no symbols. Every other package passed, among them `internal/sqlite` and `internal/sim`, whose tests also open many SQLite connections, so the binding itself and the harness's toy do not leak. Two things are wrong:
>
> 1. **Memory that C allocated is not freed when the test binary exits.** It is almost certainly SQLite's: connections or prepared statements that are never closed. Either the tests leave them open (for example a simulated crash that abandons an instance and never closes its connections; the toy in `internal/sim` closes a crashed instance's connections after the run, so look at how it does it), or the core itself fails to close something on some path (a statement that is not finalized, a connection of the pool that is not closed in `Close` or after a stop, a spool, an early return). **The second would be a real bug in the core, and finding out which it is matters more than making the job green.**
> 2. **The core's tests took 585 seconds under the sanitizer,** against Go's limit of 600 seconds for one package. Under the race detector in CI the whole test job took 8 minutes 23 seconds. Both are too close.
>
> ## The goal
>
> 1. **Reproduce it.** Go supports `-asan` on Linux only; the `Makefile`'s `test-asan` target runs in Docker elsewhere. Run only the core under the sanitizer (copy the `docker run` line of the Makefile and run `go test -asan ./internal/core/` in it, with `-run` to narrow down), and get symbols if you can (`ASAN_OPTIONS`, `ASAN_SYMBOLIZER_PATH`, or `addr2line` on the test binary built with `go test -c -asan`). Find which tests leak, by running them one at a time or by halves.
> 2. **Find the cause and say which kind it is,** with the evidence: a test that leaves something open, or the core.
>    - If it is the core: write a test that shows the leak without the sanitizer if that is possible (for example by counting open statements or connections through the binding, if the binding can tell, or by a check in `Close`), then fix the core. Look for the same mistake on the other paths: every error return, the stop after a local failure, `Close` after a stop, a rollback, a scan that is abandoned half way, a read transaction that is not closed, catch-up that fails in the middle.
>    - If it is the tests: fix the tests so that every connection that a test opens is closed, also after a simulated crash, as the toy does.
>    - If the sanitizer reports memory that SQLite keeps on purpose for the life of the process, show that from SQLite's documentation before you suppress anything, and suppress only that.
> 3. **Bring the core's time under the sanitizer well below the limit:** find out which tests take the time under `-asan` (it may differ from the plain run), and cut the default run as `docs/diary/2026-11-12-lease-without-clock-comparison.md`, Step 4, did for the harness's tests, without losing what the tests prove: every planted bug is still found in the default run and the test fails if one is not, every seed that runs still runs twice, and the full run stays behind `S4QL_SIM_SEEDS`. Target: `internal/core` under `-asan` in the Docker container with `--cpus 4` in under 5 minutes, and under `go test -race` on this machine in under 3 minutes. Say what you measured before and after, and what the load of the machine was.
> 4. **Another builder is adding tests to this package at the same time,** on the same base, to finish a mutation run. Keep your changes small and where they belong, so that the two branches merge easily. Do not reorganise files.
>
> Verify at the end: `make test` and `make lint` pass; `make test-asan FUZZTIME=5s` passes in Docker (limit the container to 4 CPUs in a copy of the command, not in the Makefile); `go test -race -shuffle on ./internal/core/` passes, with the time.
>
> ## How to work
>
> - Add a step to the diary `docs/diary/2026-11-12-storage-core.md` with the `fabrik:diary` skill, under your name `leak-builder`, with the failure quoted above, how you found the cause, and the measurements. Update `internal/core/README.md` where it describes the default test run, if you change it.
> - Commit on your branch with the `fabrik:git` skill's conventions. Do not ask about GitHub issues; there are none. No attribution lines. Do not push and do not open a pull request.
> - Do not edit `docs/spec.md`, `docs/todo.md`, the root `README.md`, or `AGENTS.md`.
> - You cannot ask me anything while you run. If you hit a question you cannot settle, stop and put it in your report.
>
> ## Your report
>
> Read by me, the lead, not by the product owner. Give me: the name of your branch and the absolute path of your worktree; the cause of the leak and whether it was in the core or in the tests, with the evidence; what you changed; the times before and after; and the verification results.

**Interpretation:** Reproduce the leak of `internal/core` under `-asan`, find which test leaks and whether the core or the test is at fault, fix it, look for the same mistake elsewhere, and cut the default run under the sanitizer well below Go's 600 seconds without losing what the tests prove.

**Inferred intent:** Make pull request #12 mergeable, and know whether the core can leak connections in production.

### What I did

Worktree `/Users/maragubot/Developer/s4ql/.claude/worktrees/agent-a323fc251db2ed494`, branch `worktree-agent-a323fc251db2ed494`, fast-forwarded to `origin/storage-core` at `8ff0a57`. The Mac has 10 cores, and other sessions kept its load between 11 and 19 the whole time.

**Reproducing.** `go test -c -asan -o /out/core.test ./internal/core/` in `golang:1.27.3` with `--cpus 4`, run with `ASAN_OPTIONS=symbolize=1:external_symbolizer_path=/usr/bin/addr2line`, gives symbolized stacks: `go test -c` keeps the DWARF that `go test` strips. The first full run reported no leak at all, because two tests failed (below), and Go runs the leak check only when the binary exits with 0 (`os_beforeExit` in `/runtime/proc.go`: `if exitCode == 0 && asanenabled ...`). With those two skipped, the group of scenario tests leaked `907136 byte(s) in 748 allocation(s)`, the 748 of CI; `TestSimulation`, `TestSimulation_plantedBugs`, `TestFreeRunning`, and `TestTx_Commit_large` each alone leaked nothing. Each scenario test alone also leaked nothing, until I ran them with `GOGC=1`: then `TestLease_clocks` alone leaked `226784 byte(s) in 187 allocation(s)`, exactly a quarter of the total. LeakSanitizer scans the Go heap as roots, so memory is reported only once the Go object that points to it is collected. The stacks were SQLite's: `sqlite3_open_v2` (12 objects, the three connections of four instances), `pcache1AllocPage` under `sqlite3BtreeBeginTrans`, `sqlite3VdbeCreate` under `sqlite3Pragma` and `sqlite3Select`.

**The cause, in the tests.** `clocked` in `/internal/core/lease_test.go` cuts the client off for an hour, runs the test, and closes the instance with a context of one simulated second. `DB.Close` spends that second on releasing the lease, then fails to take the writer lock, and leaves the cache open, as its doc comment says it does when the context ends first. Logging the error showed `close: release of the lease: context deadline exceeded, after: simulated fault: request 17 of a did not reach the store, which was unreachable\nclose: context deadline exceeded`. Four calls of `clocked`, four instances of three connections each. The core did what it promises; the test gave it too little time and threw the error away. The fix gives the close five simulated minutes, as the workloads do, and checks that the close ended in time.

**A check without the sanitizer.** SQLite deletes the write-ahead log when the last connection to a database closes ("The WAL file exists for as long as any database connection has the database open", https://sqlite.org/wal.html). `leftOpen` in `/internal/core/core_test.go` lists the caches of a directory whose log is left. `cacheDir` gives the scenario tests a temporary directory and fails a test, at its end, for each such cache; `simulate` and `free` record each one as a violation of the run, with its seed. Before the fix of `clocked`, plain `go test` failed exactly the three subtests of `TestLease_clocks`, and nothing else in the default run.

**The self-review found two more,** with two reviewers on `fable` over the diff. Both found the same two things:

- **A crashed instance with a read in flight left its read connection open, also in C memory.** `DB.closeCache`, which `CloseCache` calls after the run for a crashed instance, closed only the idle connections and the write connection. A read connection that a crashed caller held was not closed. The default seeds never hit it; seeds 470, 946, and 1254 of 2000 of the workload with everything did, and seed 22 of the workload of `takeoverAfterRelease`. A reviewer ran seed 470 under the sanitizer with the check off and got a LeakSanitizer report, so the CI failure would have come back the day a default seed crashed an instance in a read. `DB` now keeps all its read connections in `readers`, and `closeCache` closes them all. With `closeCache` back on the idle connections, `S4QL_SIM_SEEDS=23 go test -run 'TestSimulation_plantedBugs$/takes_over_a_lease'` fails with `seed 22 found something without the bug: [a connection to the cache a.db is still open after the run]`; with the fix, 2000 seeds of the workload with everything pass (224 s), and so does `S4QL_SIM_SEEDS=200 go test -run '^TestSimulation_plantedBugs$' ./internal/core/`, every bug, control, and run without the bug over 200 seeds (355 s).
- **The core leaked the statement of a scan that a panic or `runtime.Goexit` ends.** `conn.scan` in `/internal/core/cache.go` gave its statement back only on paths that return normally. A panic in the loop over `ReadTx.Scan` left the statement stepped; after a recover, `ReadTx.Close` returned nil, and the connection went back to the pool with its old snapshot: the next read on it served the state before a later acknowledged commit. After the same in `Tx.Scan`, the next commit failed with `commit: sqlite: database table is locked (code 6)`. In both, `DB.Close` could not close the connection (`unable to close due to unfinalized statements`). `TestScan` in `/internal/core/cache_test.go` shows both: before the fix it failed with `Expected "t1(k*0): 1 | 2", but got "t1(k*0): 1"` and `commit: sqlite: database table is locked (code 6)`. A deferred call now gives the statement back. That is a real bug of the core, but not the one CI saw.

I looked at the other paths the task names. The error returns of `Open` close through `closeCache`; `Close` after a stop, a rollback, catch-up and replay, and a read transaction that ends with an error, reset or finalize what they hold; both reviewers found the same. `conn.query` and `conn.count` call back only into the core's own code, which waits for nothing, so a crash cannot stop them half way.

**The root user in Docker.** Two subtests of `TestDB_stop` make the directory of the cache read-only with `chmod`, which does not bind root, and the `golang` container runs as root: `core_test.go:484: Expected "the instance has stopped" ..., but got "<nil>"`, and the second one hung for 60 seconds. So `make test-asan` on macOS failed, and hid every leak by the exit code. `readOnly` now skips a test whose directory still takes a file after the `chmod`, and restores the mode in a cleanup. CI runs `make test-asan` on the runner, not as root, so it still runs both.

**The time.** Under `-asan` in the container with four CPUs, at a load of 14 to 16, each group in its own process: `TestSimulation_plantedBugs` 133 s, `TestTx_Commit_large` 67 s, `TestSimulation` 21 s, `TestFreeRunning` 13 s, the rest 8 s. `TestTx_Commit_large` and `TestFreeRunning` are sequential, so they add to the wall time in full. In the planted bugs, the controls and the runs without the bug were about 700 of about 1090 runs. A CPU profile of one planted bug under `-asan` showed a quarter of the time in C, an eighth in `asancall`, a tenth in `sim.goroutineID` through `runtime.Stack`, and much in waking goroutines: nothing that a change of the core's tests could remove. Three changes:

- the control and the run without the bug of each planted bug run 10 seeds by default, not 20; the workload with the bug still runs until four seeds have found it;
- `TestFreeRunning` and its subtests are parallel, so they overlap the simulations;
- under the sanitizer, `TestTx_Commit_large` runs a tenth of its rows and does not check the heap, as it already did under the race detector. The heap check then ran nowhere in CI, whose test job uses the race detector, so the `asan` job in `/.github/workflows/ci.yml` runs `go test -run '^TestTx_Commit_large$' ./internal/core/` first, without either.

| `internal/core` | Before | After |
|---|---|---|
| `-asan`, container with `--cpus 4`, the package alone | 240 s (300 s with the 60 s hang of `TestDB_stop`), load 14 to 16 | 126 s, load 13 to 16 |
| `-asan`, container with `--cpus 4`, inside `go test -asan ./...` | 265 s, load 14 to 15 | 158 s, load 16 to 18, in `make test-asan FUZZTIME=5s` |
| `go test -race`, this machine | 136 s, load 16 to 18 | 98 s, load 16 |
| `GOMAXPROCS=4 go test -race`, this machine | 164 s, load 14 to 19 (with the seed cut only) | 133 s, load 16 to 18 |
| `go test` | 55 s | 46 s |

Every bug's workload found the same four seeds under `-asan` after the cut as before (for example `[5 31 39 80]` for the bug that needs 81 seeds), and every control and run without the bug found 0 of 10.

### Why

The leak had to be pinned to one test before anything was changed, because the task cared more about whether the core leaks than about a green job. The WAL check makes the same question answerable in every plain run, on every platform, without depending on when the collector ran. The time went into the planted bugs' controls and the large transaction, so that is where the cut went.

### What worked

- Running each group of tests in its own process, and then each test with `GOGC=1`. The sum of four quarters was the whole leak.
- The leftover write-ahead log as the sign of a connection that is left. It found the same leak as the sanitizer, then the crash in a read that no default seed reaches.
- Two reviewers who ran the large seed counts against the new check.

### What didn't work

- The first full run under `-asan` printed no leak, only `--- FAIL: TestDB_stop (60.09s)`: Go skips the leak check when the binary fails. I spent a run on that before I read `os_beforeExit`.
- Running each scenario test alone, without `GOGC=1`, found nothing: `TestLease_clocks exit=0` with no `SUMMARY`.
- `sh -c 'time ...'` in the `golang` image: `sh: 1: time: not found`. `bash -c` works.
- An inline `python3` heredoc to append the test to `cache_test.go` was refused: "this command is too complex to verify that it stays inside the worktree". I used the Edit tool.
- My first time cut was 20 to 10 seeds and the parallel `TestFreeRunning` only. A reviewer measured it at 7%, 176 s to 164 s on ten CPUs, because `TestTx_Commit_large` stayed sequential at a minute. My own before and after compared a run beside `internal/sim` with a run alone, which is not the same thing.
- The first version of the check failed with `t.Errorf` inside the bubble of `simulate`. `synctest.Test` then ends the outer test with `FailNow`, so the message named no seed and the rest of the seeds never ran. It is a violation of the run now.

### What I learned

- LeakSanitizer in a Go binary reports C memory only when the binary passes, and only once nothing on the Go heap points to it, so a leak can come and go with the timing of the collector.
- `go test` strips DWARF from the test binary, so its sanitizer stacks have no symbols; `go test -c` keeps it.
- A connection that has not read a database keeps no write-ahead log, so the check misses a connection that was opened and never used.

### What was tricky

Telling a leak of the tests from one of the core. `DB.Close` leaving the cache open when its context ends is documented, so the leak that CI saw is the test's. The same check then found one of the harness and one of the core. The crash that leaves a read connection open is the harness's, because a real crash frees everything, but the leak was real C memory.

### What warrants review

- `/internal/core/cache.go`, `conn.scan`: the deferred call, and that every path that gives the statement back first sets `s` to nil.
- `/internal/core/core.go`, `DB.readers` and `DB.closeCache`: it closes connections that a transaction holds, which is right only for an instance that nothing uses any more.
- `/internal/core/core_test.go`, `cacheDir`, `leftOpen`, `readOnly`; `/internal/core/simulation_test.go`, `outcome.checkClosed`.
- What the default run does less of: the controls and the runs without the bug run 10 seeds, not 20; `TestTx_Commit_large` checks the heap only in the plain run, and in CI only in the new step of the `asan` job.

### Future work

- **A question of design for the lead:** `DB.Close` spends its context on the release of the lease first, so a caller with a short deadline and an unreachable store gets an error and an open cache, again on every retry. The cache could be closed without waiting when no transaction holds a lock, or the release could get only part of the context.
- `DB.Close` sets `closed` before it knows that the connections closed, so a connection whose close fails is never closed. With the fix of the scan, only an `iter.Pull` of a scan without its `stop` can still cause that.
- The clean runs of `cacheFirst`, `keyTwice`, and `forgetDelete` are the same runs, and so are those of `ackUnknown`, `settleByRead`, and `serveUnknown`: 40 of the 190 clean runs repeat others. Running each distinct clean workload once, or giving each bug its own seeds, would buy back more coverage than the cut took.
- A restarted instance never opens a cache with a write-ahead log left by the crash, because `cacheDump` on the copy recovers and checkpoints it first (found by a reviewer), so the recovery in `Open` after a crash is not exercised by the simulation.

## Step 8: Mutation testing on the final code

**Author:** mutation-builder

### Prompt Context

**Verbatim prompt:** "Every mutant of the core's decisions has run against the final code, and every one that no test catches has either a new test that catches it or a written reason why it does not matter."
**Interpretation:** Find the builder's script and list, run every mutant one after another without running out of memory, extend the list where decisions had no mutant, and for each survivor write a test or a one-line reason, without changing the core.
**Inferred intent:** The claim that the tests check the core rested on a run that the system killed after 64 of 144 mutants. It should rest on a run that finished.

### What I did

The builder's script, `mutate.py`, and its list were in the session's scratchpad. The list held 134 mutants, not the 144 that Step 4 counts, and every one of them still applied to the final code. I kept the list, and moved it to a runner of my own, `mf/run.py` in the same scratchpad:

- One mutant at a time, each through `go test -overlay` on a snapshot of `HEAD` made with `git archive`, so that the tree stayed free for new tests.
- `GOMAXPROCS=3`, `-p 1`, `-parallel 2`, and `-failfast`, in three phases: the tests without the simulation, then the three workloads, the free runs, and the large transaction, then the planted bugs.
- Each result written to a file when it is known, and a mutant that is in the file skipped, so that a kill loses one mutant.
- A guard that kills the process group of a test whose memory goes above 2 GB, and counts it as a failure.

I added 111 mutants for decisions that the list did not cover, mostly in `read.go`, `spool.go`, `errors.go`, `cache.go`, the options, and the release, and one control without a change. The run took about three hours at a load of 12 to 28 on 10 cores. 188 of 245 were caught, 57 were not, and the control passed every test.

For the 57 I wrote 21 new subtests and extended four. Each failed with its mutant and passed without, and I ran all 57 again against the tree with the new tests: 25 caught, and 32 survivors, each with a reason below.

Two reviewers on the `fable` model read the new tests. Besides the two wrong tests below, both found: an assertion that pinned the floor of 100 milliseconds on a wait, which no doc comment promises; a test that pinned the wording of an error of replay; a loop over a scan that could pass with no rows; a copy of the helper `world.openOn`; and a comment that was hard to read. I removed the two assertions, which leaves `left-no-floor` and `replay-partial-without-key` as survivors, fixed the rest, and ran the 57 again. Both found the tests of `catchup.go` in `lease_test.go` and `core_test.go`, where the older tests of the catch-up are; I left them there.

The default run afterwards, at a load of 13 to 16 on 10 cores: `go test -count=1 ./internal/core/` in 55 seconds, and `go test -count=1 -race -shuffle on ./internal/core/` in 127 seconds, 140 with the build. `make test` and `make lint` pass.

### Why

A survivor is a decision that no test checks. Some are redundant on purpose, and some are holes. Only a test per survivor, or a reason per survivor, tells which.

### What worked

- The snapshot: the main run could not see a half-written test, and the tests could be written while it ran.
- The control: it shows that the memory guard and the timeouts do not catch correct code.
- Many survivors pointed at a promise of a doc comment that no test checked: the default of each option, an option below zero, the end of the lease to the nanosecond, the first of the two clocks ending a wait, a cache that another process changes while the instance runs, a manifest that is gone, a swap refused for its checksum, a close that cannot read the manifest or must not release the lease of another instance, a commit that looks in a log that cannot be read, a catch-up that tries a download again for longer than the lease, and every call of a read transaction after it ended or the instance stopped.

### What didn't work

- `stop-no-wake` made its test binary grow to 4 GB in two minutes: `ps` showed `4051088` kB resident for `core.test`. Four such runs in parallel are what killed the builder's run. The memory guard came after that, and caught seven mutants: `upload-no-timeout`, `upload-exists-is-failure`, `mismatch-downloaded-forever`, `adopt-keeps-flights`, and three of mine. Each is a test that never ends on the fake clock, and allocates for every step of it.
- I stopped the first runner with `pkill -f "mf/mutants/.*/overlay.json"`, which also killed the `go test` of the mutant that was running. The runner recorded it as `no-refusal-at-once  caught  5s ... | ?`. I removed the line, and the mutant ran again: caught by a timeout.
- A later command that used `pkill -f core.test` was refused: `Permission for this action was denied by the Claude Code auto mode classifier. Reason: [Interfere With Workloads]`. It would have killed the main run's tests too. A small script, `mf/probe.py`, runs one command in its own process group with the same guard.
- One of my mutants did not build: `../mutants-snap/set-position-unchecked/cache.go:258:2: declared and not used: n`. It got `&& false` in its condition and ran again.
- My test for a download tried again did not end under its mutant, and grew to 1.4 GB, because the bubble's fake time went on for ever. It waits with a timeout now, and fails in a moment.
- The code review found that two of my tests were wrong. The test of the default busy timeout checked only that the wait is shorter than two seconds, so a busy timeout of zero passed it. It measures the wait now. The test of a look in an unreadable log asserted `ErrNoLease` after the commit returned, while the lease task could take the free lease and stop the instance in parallel. The manifest it publishes now has a lease of another holder.

### What I learned

- The reason "no caller can see it" was wrong for the two second looks at the lease that Step 4 lists. A `Begin` whose lock comes free after the lease ran out by the clocks, and a `Read` whose lease runs out between fixing its state and the check, are both refused, as the doc comments say. Each has a test now.
- `manifest-any-size` was not redundant either: a manifest followed by a mebibyte of spaces parses when its first mebibyte is read. The check of the size is what refuses it.
- A test that kills a mutant only because of a timing detail of the correct code pins that detail. The review removed one such assertion, which is why `left-no-floor` survives.

### What was tricky

Telling a hang from a kill. A mutant that makes a test never end looks like a test that uses too much memory, or one that waits for its timeout. Both are caught, and the control is what shows that neither happens without a mutant.

### What warrants review

The 32 survivors and their reasons, by the id in `mf/mutants_list.py` and `mf/mutants_more.py` in the session's scratchpad:

- `replay-no-parent-check`: the walk back through the log makes the parent the position.
- `replay-create-existing`, `replay-delete-key-unchecked`, `replay-values-unchecked`, `replay-partial-without-key`: SQLite or the binding still refuses the commit object with `ErrLogBroken`, with a worse message; the last only by accident, through a parameter `?0`.
- `create-type-unchecked`: the writer of commit objects refuses every type but `TYPE_INT` first.
- `open-memory-path`: SQLite refuses the write-ahead log for an in-memory database, so `Open` still fails.
- `commit-no-usable-check`: the upload makes the same check before its first attempt.
- `set-position-unchecked`: the write lock is held from the read of the position at `Begin`, so the row is there.
- `resume-no-recheck`: `DB.keepLease` refuses an instance that no longer holds the lease.
- `observe-salt-only`, `observe-head-only`: at most one renewal and one commit are in flight, and they differ in both.
- `close-twice-closes`: the binding's close is idempotent, and nothing can hold a lock after the first close.
- `readers-writable`: the read connections run only the core's own reads.
- `created-table-touched`: the same commit object, at the cost of a temporary table.
- `back-off-never-resets`: only how soon the lease task asks again, at most the watch interval.
- `renew-no-second-look`: one renewal more.
- `send-duplicate-flights`: only the count of attempts in an event.
- `lose-keeps-version`: one refused release and its event.
- `adopt-keeps-unsure`: a successful release clears it.
- `refusal-exists-unknown`: a create of the manifest that finds one waits a second, not a tenth, for the read that follows either way.
- `spool-commit-unflushed`: the upload's rewind flushes.
- `scan-unordered`: SQLite scans a table without a rowid in the order of its key.
- `left-no-floor`: the check passes only while time is left, so the wait is never zero.
- `acquire-after-stop`: an instance that stopped while it waited for the locks takes a free lease and releases it at once, and serves nothing.
- `newflight-fresh-lease`: the holder's own lease entry has no fields that this version does not know.
- `readers-before-writer`: it guards against "busy" after a crash under real parallelism, which no test here produces.
- `setup-sync-off`: only a power loss shows it.
- `upload-spool-error-retried`, `read-end-swallows-local`, `replay-cache-error-as-broken`, `spool-length-error-ends`: a local failure in the middle of an operation, which needs a file system that fails. These four change what matters, and are not tested.

### Future work

- A file system that fails on demand, for the four local failures that no test produces.
- The runner and the lists are in the scratchpad. If mutation testing is to be repeated, they belong in the tree.

## Step 9: Five decisions of 2026-11-13

**Author:** decisions-builder

### Prompt Context

**Verbatim prompt:** "You are making five agreed changes to the storage core of s4ql [...] **1. The keys of commit objects are sharded by the first two hexadecimal digits of the name.** [...] **2. The levels of the events.** [...] **3. Pin the rule that every attempt to take the lease carries a new salt.** [...] **4. `DB.Close` closes the cache whatever the store does.** [...] **5. The simulation opens a cache with a write-ahead log that a crash left.** [...]" (the lead's brief, with the decisions that maragubot made with Markus on 2026-11-13; it is quoted here only in part, because it is long)
**Interpretation:** Five changes, one commit each, with tests, the reasoning in `/internal/core/README.md`, and the todo items deleted that they build. For change 3, check the reasoning against the code before writing it down, and stop if it has a hole.
**Inferred intent:** Close the points that were open or agreed but not built in the storage core, without changing anything else about it, and keep the simulation's guarantees (every planted bug found, every seed run twice, 1000 seeds clean).

### What I did

Five commits on the branch `worktree-agent-a74c7cf17c1097f36`:

1. **Sharded keys.** `DB.commitKey` in `/internal/core/core.go` writes `<prefix>/commits/<first two digits>/<name>`. The tests build keys through a new helper `commitKey` in `/internal/core/model_test.go`. The key test in `/internal/core/tx_test.go` checks the shard.
2. **Levels.** The doc comment of the events in `/internal/core/telemetry.go` lists the levels. A commit that succeeded, a renewal that succeeded, and a refusal are Debug. A change of the lease and a catch-up are Info. Failures are Warn, and a stop for any cause but a close is Error. The commit event keeps plain values in a `commitReport` and builds its attributes only after `DB.enabled`. A renewal and a refusal check `enabled` before they build anything. The recorder in `/internal/core/telemetry_test.go` got a minimum level and records a value of the context. `TestEvents` checks the level of each kind of event, that a healthy instance logs nothing at Info after it has started (ten rounds of commits, reads, and renewals), and the context of a commit, of a refused `Read`, of a refused `Begin`, of the lease task (the context of `Open`), and of a close.
3. **Salt.** I wrote the reasoning as a comment in `DB.acquire` and as the new section "A take that arrives late" in the README. Two tests are in `TestLease_unsure`: three takes from one version carry three salts, and the scenario. In the scenario, a take sticks, the second take is applied, and a commit of the holder sticks and is ruled out by a renewal. The holder closes, and the release writes the manifest without a lease entry back to the same bytes and the same version. Then the stuck take is applied and the stuck commit is refused with `store.ErrChanged`. The head is unchanged, and the next instance waits 30 s and the margin.
4. **Close.** Each connection now counts the calls that use it (`conn.users`, `DB.use`, `DB.unuse`). `Close` stops the instance, sets `closing`, and ends the lease task in one critical section of `DB.mu`. It closes every connection that no call uses, then releases the lease with half of the time that ctx has left (`DB.releaseContext`), then waits for the lease task and for the connections that calls still use (`DB.waitClosed`). The last call to leave a connection of a closing instance closes it. Every call of `Tx` and `ReadTx`, `Begin`, `Read`, and the lease task's `acquire` and `resume` mark their use.
5. **Recovery in the simulation.** The crasher in `/internal/core/simulation_test.go` no longer reads the copy before the restarted instance opens it. It reads the copy after `Open` and compares it with the crash and with the model. It also counts the restarts where a copy of the file alone, without the log (`fileOnly`), lacks commits that the cache had, and the frames of their logs (`walFrames`). `/internal/core/workloads_test.go` logs the counts.

### Why

The five were decided with Markus on 2026-11-13. Each removes an open point or an agreed point of `/docs/todo.md`, except the levels and the shard, which are new.

### What worked

- The 1000-seed runs: no violation and no pair of traces that differ in any of the three workloads. Of the 1000 restarts in the workload with everything, 761 recovered commits that only the log held, from logs of 5064 frames in all and 36 in the largest. The run on the final code, after the self-review, gave the same counts, at a load of 15 to 19.
- Two controls for change 5. Copying the cache without its log made the new check fail at once: `seed 0: the cache of a2 is at 00000000 after the restart, and was at aa3ce840 at the crash`. With the log, the check passes, and the file alone was behind in 761 restarts. So `Open` really recovered those commits from the log.
- Two mutants for change 2 and change 3. A commit at Info fails the test of a quiet instance. A constant salt in `DB.acquire` fails the new salt test, with `Expected "3", but got "1"`.
- The planted bugs in the default run: each of the 19 found four times within 81 seeds, as before.

### What didn't work

- I reverted my own uncommitted work. To undo a mutant in `tx.go`, I ran `git checkout internal/core/tx.go`, which also threw away the uncommitted `commitReport` change. I applied it again from the same script. Later mutants I undid with `sed` on a marker.
- The scenario test of change 3 does not fail under a constant salt. The take's `Renewed` time differs between the two attempts in the bubble, so the bytes differ anyway. The salt test is what pins the rule. In production, a wall clock that stands still would make the bytes equal.
- The worktree guard refused some compound shell commands: "this command is too complex to verify that it stays inside the worktree". I moved the large edits into a small script in the scratchpad (`sub.py` with a spec file of exact replacements) and split the git commands.
- One run of `go test -count=1 -race -shuffle on ./internal/core/` failed at a load of 17 to 22, while the reviewers of the code review also ran. I kept only `tail -3` of it (`FAIL maragu.dev/s4ql/internal/core 122.992s`), so the failing test is not known. Five more race runs passed, three of them with `-v` and the full log kept. I could not reproduce it.

### What I learned

- The reasoning of change 3 holds, and it rests on one thing that the README now names: no take is sent twice with the same bytes. The core never sends one twice. The client of the store must not either, which the todo already asks of the S3 client for another reason. If a take were sent twice, its version could come back, and a stuck commit from that version could then be applied after it was settled as not committed. That would be a correctness bug, so this condition matters.
- The old simulation never recovered anything because the harness's own `cacheDump` of the copy was the last connection to close it. Closing that connection checkpointed the copy and deleted its log.
- After the stop, every call of a transaction fails at its next check anyway. So waiting for an idle transaction in `Close` gains nothing, and closing its connection loses nothing.

### What was tricky

- `use` returns the error of the stop when a connection is closed. That is only safe if a connection is never closed before the stop. The first version of `Close` stopped first and set `closing` in a second critical section. The review found that the lease task could start its try to release the lease in that gap, which `Close` then cancelled mid-request. The stop, `closing`, and the end of the lease task are one critical section now (`DB.stopLocked`).
- Defers run last first. In `Tx.Commit` and `DB.Read`, the deferred `unuse` must run before `end`, which uses the connection again, so it is registered after `end`.
- The time left before a deadline can only be measured by the wall clock of the host. That is the only clock the deadline shares, and the review flagged it. A step of the clock changes only how long the release tries, and the doc comment of `releaseContext` says so.

### What warrants review

- The new contract of `DB.Close` in `/internal/core/core.go`. It closes the connection under an open, idle transaction (rolling a write transaction back), and `Tx.Rollback` and `ReadTx.Close` then return nil. The brief left this to me, and it changes what `Close` did before, which was to wait for open transactions. If this needs Markus, it is the one point.
- `DB.use` and `DB.unuse` in every call of `/internal/core/tx.go` and `/internal/core/read.go`. A call that forgets `leave` keeps a connection open, which `leftOpen` would catch in the tests that cover that call.
- The levels in `/internal/core/telemetry.go`. Three choices go beyond the brief's list. A commit whose cache fails after the swap is Warn, though it returns nil. A catch-up that fails is Warn, except when a close ended it, which is Info. A take, renewal, or release that fails is Warn.

Self-review, with the `code-review` skill at the level high over the branch. Ten findings:

- Fixed: the gap between the stop and `closing`, described above.
- Fixed: a release in `Close` that failed logged no event, because the lease task's try, which logged one, no longer runs on a close. `DB.release` logs a Warn `lease released outcome=failed` now, and the test of an unreachable store checks it.
- Fixed: `unuse` dropped the errors of the closes that it made. They are kept in `DB.closeErrs`, and `Close` returns them.
- Fixed: a catch-up that a close cancelled was logged as a Warn failure. It is Info now.
- Fixed: `commitReport` kept its phases in an array of four, and a fifth phase would have panicked. It is a slice now.
- Documented: `releaseContext` measures by the wall clock (see "What was tricky").
- Left, and in the report: the release gets half of the deadline even when nothing waits on the cache. The brief asks for a bounded part, and giving the release all of it when no call runs would be a small refinement.
- Left: each call of a transaction takes `DB.mu` three times. Merging the check into `use` would save one lock and unlock per call.
- Wrong: the shard breaks reading of the old layout. The spec says "The storage formats are drafts until version 1", and the brief says that nothing migrates.
- Reported to the lead: the change of the contract of `Close`, which the brief left to the builder.

### Future work

- `/internal/core/README.md` says under "A swap whose outcome is unknown" that the second case of "outcome unknown" is an open question in the todo. The todo no longer holds it, since the commit "Name the second case of "outcome unknown" in the spec". I left the line, because it was outside the five changes.
- The race failure that I could not reproduce.
