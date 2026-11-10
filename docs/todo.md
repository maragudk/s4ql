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
- **Recovery follows the cache's recorded position.** If the failed local commit did not land, startup applies the missing change file. If it landed despite the error, there is nothing to apply. An I/O error from SQLite's `COMMIT` does not say whether the commit happened, so recovery never assumes either state. If the failure persists, `Open` fails. Nothing deletes or rebuilds the cache automatically: a person or the application chooses a rebuild, for example by pointing at an empty cache.
- **The simulation injects the failure.** A buggify point sits between the manifest swap and the local commit, with two variants:
  - The local commit is rolled back and reports an error.
  - The local commit goes through and still reports an error.

  The second variant is not distrust of SQLite. SQLite can legitimately return an I/O error from `COMMIT` after the transaction became durable. The variant catches a bug of ours: recovery that assumes an error means the cache did not get the transaction, and so applies the change file twice.

  The invariant checked is that no read that starts after an acknowledged commit returns a state without that commit. The simulation also checks that reopening shows the transaction, and that the lease was released or timed out.
- **No fault-injecting layer under SQLite, for now.** SQLite tests its own I/O error handling, and using the official C code is how we inherit that.
- **Open: the wal-index is memory-mapped.** The build sets `SQLITE_MAX_MMAP_SIZE=0`, so SQLite never memory-maps the database file, and an I/O error there arrives as an error code. In WAL mode SQLite still memory-maps its shared index, the `-shm` file: "The wal-index is implemented using an ordinary file that is mmapped" (https://sqlite.org/wal.html). An I/O failure on a memory-mapped file arrives as a signal that kills the process, not as an error that SQLite can handle (https://sqlite.org/mmap.html). How to handle this is not designed.

What comparable systems tell the client, and what the node does, in the motivating failure is in [research/commit-acknowledgement.md](research/commit-acknowledgement.md).

## Concurrency and the simulation harness

Status: partly agreed, partly open. A spike is testing the simulation design. To be settled before anyone is briefed on engine work.

The concurrency model is agreed and in the spec under "Concurrency": one write transaction at a time per database, and reads in parallel bounded by the connection pool.

Agreed with Markus on 2026-11-10:

- **No state machine.** The library is plain blocking Go. Each caller's goroutine does its own work from top to bottom, a lock lets one write transaction through at a time, and one background goroutine renews the lease. That goroutine is the only concurrency the library creates; the rest comes from the callers. The spec used to ask for a state machine that does no I/O, and that sentence was deleted. The reasons: a state machine does not solve the hard part, which is the order goroutines run in; it fits interactive transactions poorly; and FoundationDB, whose testing this project follows, runs plain coroutine code on a deterministic scheduler and has no such state machine.
- **Locks are taken in Go before calling into SQLite,** so that SQLite never waits on a lock itself. SQLite's busy timeout stays off, so it returns "busy" at once and never sleeps. If the Go locks are right, that error never appears, and seeing it means a bug of ours.
- **The stop is synchronised with the readers.** After a local I/O failure (see [Local I/O failure handling](#local-io-failure-handling)), no read can start between the failure and `Commit` returning. The simulation tests this with readers racing the stop.

Being tested by a spike, and adopted only if it passes:

- The real library runs unchanged inside a `testing/synctest` test. S3 is a simulated object store, time is fake, and randomness comes from the seed. The test starts goroutines that play application callers.
- Wherever a goroutine meets the outside world, it parks: at an S3 call, at a wait on time, and at marked points in a read. A scheduler waits until every goroutine is parked, picks one by the seed, lets it run to its next parking point, and repeats. So one goroutine runs at a time, and the seed decides the order. This is FoundationDB's model rebuilt in Go.
- Every seed is run twice and the two runs are compared, so nondeterminism that leaks in fails the test.
- A second mode runs the same tests with the scheduler off under the race detector. It finds data races, but a failure there cannot be replayed from the seed.
- Known costs: all waiting on time goes through our clock interface, a lock held across a parking point cannot be a `sync.Mutex` (waiting on one does not count as blocked for `testing/synctest`), and parking points need stable names. The design cannot replay two SQLite calls that run at the same instant.

If the spike fails, the choice comes back between a state machine for the core logic and giving up exact replay of ordering.

Still open:

- How read connections check that the lease is valid without going through the write path, and which clocks that relies on.
- What a read does when the lease deadline has passed but the instance has not stopped.
- What the instance does after it loses the lease: stop, or try to take it back and catch up.
- Whether a write transaction has a maximum duration.
- The stop requirement above cannot be met exactly as worded, because a read can start between a failure inside SQLite and the moment the library sees it. The wording needs tightening to what the spec promises: a read that starts after a commit is acknowledged always sees it.

Relevant notes: [research/foundationdb.md](research/foundationdb.md), [research/slatedb.md](research/slatedb.md), [research/testing-strategies.md](research/testing-strategies.md).

## A manifest swap whose outcome is unknown

Status: open, not designed.

The manifest swap can end without telling the instance whether it succeeded, for example when the request times out. What `Commit` returns and what the instance does then is not designed yet.

## Vendoring s4ql

Status: open, not fixed.

A module that vendors s4ql with `go mod vendor` cannot build, because Go vendors only directories that hold Go packages and so drops `internal/sqlite/c/`, which holds the SQLite C code. It is not fixed, because nothing depends on s4ql yet.
