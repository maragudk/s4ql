# To do

A working list of things that are agreed but not built, and of design questions that are still open. The agreed rules are in [spec.md](spec.md), and the SQL slices are in [roadmap.md](roadmap.md).

Each item is marked agreed or open. An agreed item carries the details that a builder needs who was not in the conversation. When an item is built, it is deleted from this list, and its reasoning moves next to the code as tests and doc comments. The code is then the source of truth for how it was done.

## Local I/O failure handling

Status: agreed with Markus on 2026-11-10, not built.

The spec holds the rules: a commit is acknowledged once S3 has it, a local I/O failure stops the instance, and recovery from a stop is a normal start. This item holds the reasoning and the detail.

**The motivating failure.** The manifest swap succeeded, so the transaction is committed in S3, and the commit to the local cache then fails. The commit is real, but the cache cannot show it, so the instance must not go on serving reads from the cache at the old state. Markus asked for a story that covers local I/O failure in general, not only a full disk, because local disk I/O can fail in many ways.

- **One rule for every local I/O failure.** Any I/O failure on the cache stops the instance, also where nothing was committed: a full disk during a write before the swap, a failed read, or a failure during catch-up. There is then one rule and one recovery path to test, at the cost of availability on transient failures. The rule holds for now. Markus: "We can relax later if needed."
- **No retry in place.** After a failed fsync, the local state cannot be trusted until it is reopened. PostgreSQL makes the same choice for the same reason; see the research note.
- **Stopped means a dead handle.** s4ql is a library and cannot crash the process, so the database handle dies instead. No new statement starts, lease renewal stops, and every later call returns a distinct stopped error that wraps the I/O cause. The handle also signals the stop directly, in the style of a context: a channel that closes when it stops, and the cause as an error. The application can then react at once, and not only on its next query.
- **`Commit` returns nil when the swap succeeded.** Its result keeps one meaning: nil if and only if the transaction is known to be committed. We considered and rejected an error that means "committed, but stopped". Almost all Go code treats any error from `Commit` as "not committed", so a job queue would redeliver and a user would resubmit after the restart, and the transaction would be written twice.
- **Running reads are aborted.** Reads already running when the instance stops fail. This is a lean, not a hard rule. Markus: "Sounds theoretical. Don't overfocus on it. Leaning towards abort." Letting them finish would break no rule, since they hold a snapshot from before the commit.
- **The instance stops, then releases the lease.** It stops serving first, then tries once to release the lease. If the release fails, the lease times out as after a crash.
- **The library does not reopen itself, for now.** The application closes the database and reopens it, so recovery from a stop is the same path as recovery from an application crash.
- **Recovery follows the cache's recorded position.** If the failed local commit did not land, startup applies the missing commit object. If it landed despite the error, there is nothing to apply. An I/O error from SQLite's `COMMIT` does not say whether the commit happened, so recovery never assumes either state. If the failure persists, `Open` fails. Nothing deletes or rebuilds the cache automatically: a person or the application chooses a rebuild, for example by pointing at an empty cache.
- **The simulation injects the failure.** A buggify point sits between the manifest swap and the local commit, with two variants:
  - The local commit is rolled back and reports an error.
  - The local commit goes through and still reports an error.

  The second variant is not distrust of SQLite. SQLite can legitimately return an I/O error from `COMMIT` after the transaction became durable. The variant catches a bug of ours: recovery that assumes an error means the cache did not get the transaction, and so applies the commit object twice.

  The invariant checked is that no read that starts after an acknowledged commit returns a state without that commit. The simulation also checks that reopening shows the transaction, and that the lease was released or timed out.
- **No fault-injecting layer under SQLite, for now.** SQLite tests its own I/O error handling, and using the official C code is how we inherit that.
- **Which SQLite errors count as a local I/O failure.** Chosen by maragubot on 2026-11-11 and told to Markus: I/O error, disk full, cannot open, corrupt, not a database, no large-file support, permission denied, and read-only on a cache that was opened for writing. Out of memory and SQLite's protocol error also stop the instance, for simplicity. "Busy" does not: see the next item. The full classification is in [research/sqlite-busy.md](research/sqlite-busy.md).
- **Open: the wal-index is memory-mapped.** The build sets `SQLITE_MAX_MMAP_SIZE=0`, so SQLite never memory-maps the database file, and an I/O error there arrives as an error code. In WAL mode SQLite still memory-maps its shared index, the `-shm` file: "The wal-index is implemented using an ordinary file that is mmapped" (https://sqlite.org/wal.html). An I/O failure on a memory-mapped file arrives as a signal that kills the process, not as an error that SQLite can handle (https://sqlite.org/mmap.html). How to handle this is not designed.
- **A possible answer, parked: SQLite's `unix-excl` VFS.** Markus on 2026-11-11: "park it in TODO.md for now as a potential thing". It keeps the wal-index in ordinary memory and creates no `-shm` file, which would close the gap above. It also locks every other process out of the cache file while our process has it open, including the `sqlite3` shell, so a live cache cannot be inspected from outside. Several connections in one process still work. It does not change the file format, so it can be switched on or off at any time, and `sqlite.Open` would need a VFS name. To find out before deciding: what happens after a crash (whether the operating system releases the lock, and that the next start recovers from the WAL), and a test that no signal arrives on a disk error. It exists on macOS and Linux only. Details are in [research/sqlite-busy.md](research/sqlite-busy.md).

What comparable systems tell the client, and what the node does, in the motivating failure is in [research/commit-acknowledgement.md](research/commit-acknowledgement.md).

## Concurrency and the simulation harness

Status: the design is adopted as the working approach and is to be judged by building on it. Markus on 2026-11-11: "I think we should just try it out." Parts are still open, listed at the end.

The concurrency model is agreed and in the spec under "Concurrency": one write transaction at a time per database, and reads in parallel bounded by the connection pool.

Agreed with Markus on 2026-11-10 and 2026-11-11:

- **No state machine.** The library is plain blocking Go. Each caller's goroutine does its own work from top to bottom, and one background goroutine renews the lease. That goroutine is the only concurrency the library creates; the rest comes from the callers. The spec used to ask for a state machine that does no I/O, and that sentence was deleted. The reasons: a state machine does not solve the hard part, which is the order goroutines run in; it fits interactive transactions poorly; and FoundationDB, whose testing this project follows, runs plain coroutine code on a deterministic scheduler and has no such state machine.
- **Two locks.** A writer lock is held for a whole write transaction, so one runs at a time. A separate manifest lock is held only around each swap, so lease renewal can run while a write transaction sits open.
- **One small runtime interface for everything that waits:** starting a goroutine, locks, sleeping and timers, the clock, timeouts, and yielding. In production it maps onto ordinary Go. In simulation the scheduler implements it.
- **The stop is synchronised with the readers.** After a local I/O failure (see [Local I/O failure handling](#local-io-failure-handling)), no read can start between the failure and `Commit` returning. The simulation tests this with readers racing the stop.

The simulation, as shown to work by a spike on 2026-11-10 (in `internal/sim` once merged; the diary is `docs/diary/2026-11-10-simulation-spike.md`):

- **Scheduled mode.** The real library runs unchanged inside a `testing/synctest` test, with test goroutines playing application callers. One goroutine runs at a time. It runs until it waits (at an S3 call, a sleep, taking or releasing a lock, or an explicit yield), and the seed then picks which goroutine goes next. This is FoundationDB's model rebuilt in Go.
- **The scheduler owns the clock, the locks, and the randomness.** It cannot leave them to Go: timers due at the same instant fire in random order under `testing/synctest`, and a channel-based lock wakes the next holder directly, so two goroutines would run at once. Half of all yields get a random simulated delay, or timer-driven work such as renewal never gets in between.
- **Every seed is run twice and the traces are compared,** so nondeterminism that leaks in fails the test. The spike ran 10000 seeds twice with no differing pair, and each seed gave a different interleaving.
- **Free-running mode.** The same code runs with the scheduler off under the race detector, with real parallelism and no replay. A separate stress test on the real clock covers what only true parallelism reaches.
- **`testing/synctest` works with cgo.** A goroutine inside a SQLite call counts as running, so the scheduler waits for the call to return, and fake time does not move meanwhile. This is not documented, so the spike's tests of it stay as canaries for Go upgrades.

Conditions for keeping the design, from the spike:

- A lint forbids, in library packages, `go` statements, the real clock and timers, `context.WithTimeout` and `context.WithDeadline`, `sync.Cond`, and `sync.WaitGroup`. An ordinary mutex must not be held across a wait; the scheduler cannot detect that, and the test hangs.
- Every seed is run twice in CI, permanently.
- The free-running mode runs under the race detector in CI.
- The checks compare data against a model, and planted bugs stay as tests of the checks. The spike's toy library had a real bug that 20000 clean runs produced and no check looked for.
- Every lock acquire and release is a scheduling point, and yields are placed wherever shared state is touched between waits.

What the scheduled mode cannot do:

- It switches goroutines only at waits. In the spike, a race with no wait in the window was found by 0 of 1000 seeds, and by 659 once a yield was added.
- It hides data races from the race detector, and it never has two goroutines inside SQLite.
- It finds only what the checks look for.
- It runs only under `go test`, and a seed is tied to the exact code.
- Not yet shown: crash and restart, two instances competing for the lease, and a real S3 client under simulation.

SQLite's "busy", chosen by maragubot on 2026-11-11 after an investigation and told to Markus ([research/sqlite-busy.md](research/sqlite-busy.md)):

- **A lock in Go does not keep SQLite from reporting "busy".** A reader that starts while the writer commits can take the WAL write lock for a moment, and the writer's next `begin immediate` then fails at once. It happens with a single reader. It never happens in scheduled mode, only with real parallelism.
- **Every connection gets a busy timeout of about one second.** SQLite then waits out the window itself. In the investigation a timeout of one millisecond already removed every failure, and the wait was about 1.5 ms on fewer than 1% of begins.
- **The write connection opens first, runs one statement, and stays open for the instance's lifetime.** Recovery of the WAL after a crash and the cleanup when the last connection closes also report "busy", and this keeps both away from the reader pool.
- **"Busy" after the timeout is returned to the caller as an error and logged loudly.** It is not a local I/O failure and does not stop the instance. In this design it then means another process has the cache open, or a bug of ours.

To revisit later. Markus on 2026-11-11, on whether the free-running mode can go: "I'd rather keep the free-running mode for now, and we can revisit later." The ideas:

- Pass the baton between goroutines through something the race detector cannot see, such as a call into C, so that the detector finds data races in scheduled mode. Untested.
- For code without data races, only synchronisation operations are places where a switch can change the outcome. If every lock and atomic operation is a scheduling point, the scheduled mode then covers what production can do.
- Inject SQLite's contention effects, such as "busy", from the seed at the binding, and keep one small test with real parallelism whose only job is to check our assumptions about SQLite, as real S3 does for the simulated store.
- The alternative is FoundationDB's own way: one global lock, so that production also runs one goroutine at a time. It would make the SQL engine run one query at a time and weaken "reads run in parallel".

Still open:

- The stop requirement above cannot be met exactly as worded, because a read can start between a failure inside SQLite and the moment the library sees it. The wording needs tightening to what the spec promises: a read that starts after a commit is acknowledged always sees it.

Relevant notes: [research/foundationdb.md](research/foundationdb.md), [research/slatedb.md](research/slatedb.md), [research/testing-strategies.md](research/testing-strategies.md).

## The lease and its clocks

Status: agreed with Markus on 2026-11-11, changed on 2026-11-12, not built. The spec holds the rules under "Constraints"; this item holds the reasoning and the detail.

Markus on the frame for all of it: s4ql is "designed mostly for always-on server-side usage", and "I don't want to design around theoretical failures."

The design:

- **Another instance takes the lease after it has watched the manifest stand still.** An instance that wants the lease reads the manifest. If the manifest has no `lease` entry, the instance takes the lease at once. Otherwise it waits for the lease's duration plus a margin, on its own monotonic clock, and then takes the lease with a compare-and-swap against the version it read. If the manifest has changed, the holder is alive, and the wait starts again from the new version.
- **The compare-and-swap makes the wait safe.** If it succeeds, nothing has replaced the version that the instance read. The holder therefore sent its last renewal before that read, and its own time for reads ran out before the wait ended.
- **No instance compares its clock with another's.** Each machine only has to measure a duration correctly. The store needs nothing beyond the compare-and-swap that the spec already requires.
- **The cost is a wait after a crash.** An instance that starts after a crash waits the full time, about 35 to 40 seconds with the defaults, before it serves, even if the crash was long ago. A clean shutdown releases the lease, so a normal restart does not wait. Markus asked: "we agree that regular shutdowns still don't have to wait, right?" A spare that is already running takes over about as fast as before, because it has watched the manifest since the holder's last renewal.
- **Every renewal must change the manifest's bytes.** S3's ETag comes from the content. A renewal that wrote the same bytes would be invisible to the instance that watches, and that instance's compare-and-swap would succeed against a live holder. The manifest therefore gets a value that exists to change.
- **That value is a random salt, next to the time of the renewal.** maragubot recommended a counter. Markus proposed to keep writing the wall-clock time without using it, and then: "Or the clock + a random salt". On the counter: "But then we have to handle counter overflows and all that stuff. And I like that there's no state to keep track of." The time alone was not enough. Safety would then rest on the wall clock never repeating a value, and the simulation sets clocks backward on purpose.
- **The manifest's lease entry holds four fields,** and `expires` is gone:

  ```json
  "lease": {
    "holder": "5b0e…77c3",
    "duration": "30s",
    "renewed": "2026-11-12T12:00:30.123456789Z",
    "salt": "c41d07a9e2b35f68"
  }
  ```

  - `holder` is the ID of the process that holds the lease.
  - `duration` is how long the lease lasts, in Go's duration format, such as `30s` or `1m30s`. Only the spelling that Go itself writes is read, so that one duration has one spelling. maragubot proposed milliseconds as a number. Markus: "\"duration\" is fine, and make it seconds", and then: "Maybe even use the Go time.Duration format?"
  - `renewed` is the holder's wall-clock time at the renewal. Nothing reads it for a decision.
  - `salt` is 64 random bits as 16 hexadecimal digits, new at every write of the manifest by the holder.
- **The duration is in the manifest** because an instance that watches must use the holder's duration and not its own setting. With a shorter setting of its own, it would take the lease too early.
- **Clocks affect only reads.** Writes are correct whatever the clocks do, because S3 checks the manifest swap. The danger is a holder that has lost the lease without knowing it and answers a read from its cache, which lacks the new holder's commits.
- **A read checks the lease against both of the holder's clocks.** It passes only if less than the lease's duration has passed since the holder sent its last renewal, by the wall clock and by the monotonic clock. Each clock is compared only with itself. Both stay because Go's monotonic clock on Linux stands still while a machine is suspended, and the wall clock catches that. A wall clock that is set backward is caught by the monotonic clock. If both fail, reads can be stale, and Markus accepts that: "if the server is in such bad a state that both clocks fail, stale reads are probably okay too."
- **One check when the read starts is enough.** Its order relative to the SQLite snapshot does not matter. The cache never holds another instance's commits, so a snapshot taken after a pause shows a state that was correct at the moment of the check.
- **An instance without a valid lease refuses and keeps trying.** Reads and writes fail at once with a distinct temporary error, and a write transaction that is open then is not committed. The instance keeps trying to get the lease. When it gets it, it applies the commit objects it is missing, with the same code as at startup, and serves again. This one rule covers three cases:
  - The instance cannot reach S3 and its lease runs out. If a later renewal succeeds, the manifest was unchanged and there is nothing to catch up. Markus: "this is where we'd return a HTTP 502 and let the clients retry."
  - Another instance has taken the lease.
  - The instance has just started.
- **A spare instance is a normal state.** Markus: "we can have a spare instance running, and it'll never serve anything until it gets the lease."
- **Opening does not wait for the lease.** It fails only on local problems, such as a cache that cannot be opened. A separate call waits, with a context, until the instance serves, as `Ping` does next to `Open` in `database/sql`. Wrong S3 settings therefore do not fail the open call; they show in the waiting call and in the error of every query.
- **A permanent stop is for local I/O failures only.**
- **A write transaction has no maximum duration, for now.** A waiting writer gives up when its own context expires.

How the design came about, on 2026-11-12 unless a date is given:

- **On 2026-11-11 the manifest was to record an absolute expiry time.** Another instance was to take the lease when its own wall clock had passed that time, which is only safe if the wall clocks of the machines agree. The alternative was a duration and a value that another instance watches stand still, and it was passed over. Markus: "A is fine for now."
- **The simulation harness showed that one wrong wall clock is enough for stale reads.** The holder's wall clock is behind. The holder renews, and writes an expiry that is already past for every other instance. A spare takes the lease, correctly by its own clock, and commits. The holder's two checks both pass, so it serves reads from its cache until its next renewal fails. A spare whose clock is ahead does the same. This item used to say "A wall clock that is set backward is caught by the monotonic clock", which held only while the holder wrote no expiry from the wrong clock.
- **maragubot proposed to take the time from S3:** compare the manifest's `Last-Modified` with the `Date` of S3's response. Markus: "Ooh, of course. S3 should be our clock, I can't believe I didn't think of that."
- **The research found S3's clock better than the wall clocks, but not safe enough to rest the lease on.** On Tigris a client can set `Last-Modified` with a request header, and MinIO has the same hole through a header of its own. The two timestamps usually come from different machines. The only store that documents a bound on the skew between its own clocks is MinIO, and its bound is 15 minutes. No system was found that compares the two timestamps. The idea swaps "our machines' clocks agree" for "the store's clocks agree", which cannot be checked either. The note is [research/s3-clock.md](research/s3-clock.md).
- **The research pointed to the rule that Kubernetes and the DynamoDB Lock Client use, which compares no clocks.** It is the alternative of 2026-11-11. Markus: "didn't we discard this design already? But I think it sounds good."

Details chosen by maragubot:

- The duration counts from when the first attempt of a renewal was sent, not from a retry and not from the answer. Section 6 of the research note says why.
- The margin's value is chosen when it is built.

A possible later addition:

- **S3's timestamps could shorten the wait after a crash.** They would be switched on only for a store that has been checked. They help how fast an instance serves, not safety.

## Storage formats

Status: the manifest and the commit object are built in `internal/format`, with the schema in `proto/s4ql/v1/commit.proto`. Replay into the cache and one check in CI are not built, and two questions are open. The reasons behind the formats are in [internal/format/README.md](../internal/format/README.md).

- **The manifest's lease entry changes** to the one that [The lease and its clocks](#the-lease-and-its-clocks) describes: `expires` goes, and `duration`, `renewed`, and `salt` come. This is being built.
- **`buf breaking` under its `WIRE` rules is set up when version 1 is settled.** While the formats are drafts, a change to the schema may break old commit objects.
- **Nothing produces an `UpsertRows` with only some columns yet.** The format writes and reads it, and the samples cover it. It is first produced when a slice of the roadmap brings `update`.
- **Replay of commit objects into the cache is not built.** It applies the changes of one commit object inside one SQLite transaction, and commits that transaction only after `CommitReader` has reported the end with `io.EOF`. It refuses what the format cannot check because it needs the table: a column position outside the table's columns, and a row with the wrong number of values for the table. For later slices, replay applies results only, so it must not run triggers or cascades, and foreign keys cannot be checked row by row, because a commit object holds the net effect and not the order of the statements.
- **Open: whether `integer` as a declared type becomes a second value of `Type`,** or is treated as the same type as `int`. For s4ql the two behave the same, because every table is `WITHOUT ROWID`. The slice that adds `integer` decides.
- **Open: whether a new lease holder keeps the unknown fields of the old lease.** `Manifest` keeps the unknown fields of a `Lease` only while the same value is changed in place, and a new `Lease` has none.

## A manifest swap whose outcome is unknown

Status: agreed with Markus on 2026-11-11, not built. The spec holds the rule; this item holds the reasoning and the detail.

Markus on the state after "outcome unknown": "it's kind of the same as when the lease expired? We return a temporary error to readers and writers." It is the same state: the instance does not know whether its cache is current, so it refuses and keeps trying.

The manifest swap can end without telling the instance whether it succeeded, for example when the connection drops before the answer arrives. The transaction is then either committed or not, and the instance cannot tell which.

- **The instance asks S3 again.** It retries the same swap. If the first attempt never arrived, the retry succeeds and the transaction is committed. If the first attempt did arrive, S3 rejects the retry, because the manifest has changed.
- **After a rejection, the instance reads the manifest.** The transaction is committed if and only if its commit object is in the log that the manifest points to. Another instance may have taken the lease and committed on top in the meantime, so the check walks the log back from the newest commit object and does not look at that file alone.
- **A request stuck in the network cannot commit later.** Once the manifest has changed, S3 rejects the old request.
- **`Commit` returns only when the instance knows,** with nil if the transaction is committed and an error if it is not.
- **If S3 stays unreachable, `Commit` waits until the caller's context ends,** and then returns a distinct "outcome unknown" error. An application must check its own data then, or make its transactions safe to repeat. FoundationDB does the same with `commit_unknown_result` ([research/foundationdb.md](research/foundationdb.md)). This is the one case where `Commit` returns an error for a transaction that may be committed.

Details chosen by maragubot:

- **After "outcome unknown", the instance serves nothing until it knows.** It refuses reads and writes with the temporary error, as an instance without a valid lease does. Otherwise the application could check its data on a cache that lacks the transaction, conclude that it was not committed, and repeat it, while the stuck request commits the first one.
- **In the background the instance no longer pushes the commit.** It reads the manifest. If the commit object is in the log, it catches up the cache, which applies the transaction. If the manifest is unchanged, it renews the lease without the commit. When that renewal succeeds, the stuck request can no longer be applied, and the transaction is not committed. When the renewal is rejected, the instance reads the manifest again.
- **While `Commit` is still waiting, reads go on** for as long as the lease is valid by the clocks. They see the state before the transaction, which is allowed, because the commit is not acknowledged.
- **A renewal with an unknown outcome is resolved the same way,** with no caller involved.
- **An upload of a commit object with an unknown outcome is harmless.** The file is named by its content and created only if absent, so the instance retries, and "already exists" means success.

## The S3 client and S3's checksums

Status: agreed with Markus on 2026-11-12, not built. It belongs to the client for real S3, which comes after the simulated object store.

Markus asked: "Maybe S3 already knows the hash of its objects?" It does, and s4ql uses it as a second guard next to its own check.

- **On upload, the client sends the commit object's SHA-256 with the request.** S3 computes its own and rejects the object if they differ, so a commit object is never stored under a name that does not match its bytes.
- **On download, the client compares the checksum that S3 returns with the name,** before it reads the body. S3 stores the checksum with the object and returns it in the response headers.
- **The reader still computes the checksum itself as it reads.** That check covers the whole way into our process, and it is the same against the simulated store and against a store that does not offer checksums. The spec asks three things of a store, and checksums are not among them. Which S3-compatible stores offer them is not checked.
- **This holds for commit objects up to 5 GB,** the limit of one `PutObject`. In a multipart upload, S3 offers SHA-256 only as a checksum of the checksums of the parts, which is not our name. Markus: "Don't worry too much about commit objects above 5GB for now."

Source: https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html

## Vendoring s4ql

Status: open, not fixed.

A module that vendors s4ql with `go mod vendor` cannot build, because Go vendors only directories that hold Go packages and so drops `internal/sqlite/c/`, which holds the SQLite C code. It is not fixed, because nothing depends on s4ql yet.
