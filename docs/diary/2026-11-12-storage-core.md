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
