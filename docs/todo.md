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

Status: agreed with Markus on 2026-11-11, not built. The spec holds the rules under "Constraints"; this item holds the reasoning and the detail.

Markus on the frame for all of it: s4ql is "designed mostly for always-on server-side usage", and "I don't want to design around theoretical failures."

- **The manifest records an absolute expiry time.** Another instance takes the lease when its own wall clock has passed that time. This is only safe if the wall clocks of the machines agree. The alternative was a duration and a counter, which another instance watches stand still for a full lease period on its own monotonic clock, and which needs no wall clock. Markus chose the expiry time: "A is fine for now."
- **Clocks affect only reads.** Writes are correct whatever the clocks do, because S3 checks the manifest swap. The danger is a holder that has lost the lease without knowing it and answers a read from its cache, which lacks the new holder's commits.
- **A read checks the lease against both clocks.** It passes only if the wall clock is before the expiry and the monotonic clock shows less than the lease duration since the renewal was sent. A wall clock that is set backward is caught by the monotonic clock, and a monotonic clock that stood still is caught by the wall clock. If both fail, reads can be stale, and Markus accepts that: "if the server is in such bad a state that both clocks fail, stale reads are probably okay too."
- **One check when the read starts is enough.** Its order relative to the SQLite snapshot does not matter. The cache never holds another instance's commits, so a snapshot taken after a pause shows a state that was correct at the moment of the check.
- **An instance without a valid lease refuses and keeps trying.** Reads and writes fail at once with a distinct temporary error, and a write transaction that is open then is not committed. The instance keeps trying to get the lease. When it gets it, it applies the commit objects it is missing, with the same code as at startup, and serves again. This one rule covers three cases:
  - The instance cannot reach S3 and its lease runs out. If a later renewal succeeds, the manifest was unchanged and there is nothing to catch up. Markus: "this is where we'd return a HTTP 502 and let the clients retry."
  - Another instance has taken the lease.
  - The instance has just started.
- **A spare instance is a normal state.** Markus: "we can have a spare instance running, and it'll never serve anything until it gets the lease."
- **Opening does not wait for the lease.** It fails only on local problems, such as a cache that cannot be opened. A separate call waits, with a context, until the instance serves, as `Ping` does next to `Open` in `database/sql`. Wrong S3 settings therefore do not fail the open call; they show in the waiting call and in the error of every query.
- **A permanent stop is for local I/O failures only.**
- **A write transaction has no maximum duration, for now.** A waiting writer gives up when its own context expires.

Details chosen by maragubot:

- The lease duration counts from when the renewal was sent, not from when S3 answered.
- Another instance waits a small margin past the expiry before it takes the lease. The value is chosen when it is built.

## Storage formats

Status: agreed with Markus on 2026-11-11 and 2026-11-12, not built. Both formats are drafts until we settle on version 1. One question on the commit objects is open, and it is marked below.

The manifest, `manifest.json`:

```json
{
  "version": 0,
  "branches": {
    "main": {
      "head": "9f2c…e41a"
    }
  },
  "lease": {
    "holder": "5b0e…77c3",
    "expires": "2026-11-11T12:00:30.123Z"
  }
}
```

- `version` is the version of the manifest format. It is 0 while the format is a draft.
- `branches.main.head` is the name of the newest commit object, which is the SHA-256 checksum of its content. A branch entry is an object so that it can gain fields later.
- **An empty log has the name of 32 zero bytes.** The `head` of a new database is that name, written as 64 zeros, and so is the `parent` field of the first commit object. Both fields are required. A forgotten `parent` is then an error, and not a file that looks like the first one and silently cuts off the log before it. The first commit is also no special case: every commit sets `parent` to the `head` it read. Markus proposed the required pointer with a magic name on 2026-11-11.
- `lease.holder` is the random ID of the process that holds the lease, and `lease.expires` is the wall-clock time at which it expires. A released lease has no `lease` entry.
- An instance keeps the fields it does not know when it rewrites the manifest. Without this rule, an older version would delete the fields of a newer one at every renewal.
- There is no count of commits. Markus: "Drop the "commits", let's keep the essentials."

The commit objects. Markus reviewed the schema line by line in pull request #4 on 2026-11-12, which is for review only and is not merged:

- **The term is "commit object", and the pointer is `parent`.** "Commit" is the action, and "commit object" is the immutable object in S3 that the action stores, as in git. The term replaces "change file", which was our own invention. The spec and the research notes still say "change file", and the spec says so until its new wording is agreed.
- **Both formats are drafts until we settle on version 1.** Markus: "while we're developing, this is all still a draft, so breaking changes are fine until we're settling on a v1." A draft commit object and a draft manifest carry version 0, so that a later release can refuse them cleanly. The rule that field numbers and types never change holds from version 1 on.
- **What the format is optimised for.** Markus: "we don't optimize this for printing, but for robustness, consistency, speed, and other things. But printing to screen is not one of them."
- **The encoding is Protocol Buffers, with the standard tooling:** a `.proto` file as the schema, generated Go code, and `google.golang.org/protobuf`. Markus asked for "a binary format that is forwards and backwards compatible". The comparison with CBOR, Cap'n Proto, and others is in [research/change-file-encodings.md](research/change-file-encodings.md).
- **Why not Cap'n Proto,** which Markus leaned towards first: its strengths are reading single fields without decoding and RPC, and a log that is read once from front to back uses neither. Its byte format is a memory layout with pointers and segments, which is more to trust in a file that must stay readable forever.
- **Why the tooling and not a hand-written codec,** which maragubot recommended first: less code of ours, a decoder that is fuzzed continuously, the `.proto` file as the real schema, and a check in CI that a schema change does not break old files. Both ways write the same bytes, so the choice can be changed later without touching a stored file.
- **A commit object is checked against its SHA-256 name before it is decoded.**

What a commit object holds:

- **A header, a list of changes, and a list of annotations,** as three fields of `CommitObject`. It is written and read as a stream: the header first, then the changes and the annotations one at a time, so that a commit of millions of rows is never in memory at once.
- **The header says what the commit object is:** the format version, the required `parent` name, and the commit time. It is a field of `CommitObject` and not a kind of record, because it appears exactly once. Protocol Buffers has no way to mark a field as required, and it does not reject bytes where the header is missing, late, or doubled. The reader checks that the header is there and comes first.
- **The changes are applied in order,** and a commit object holds the net effect of its commit, so a primary key appears at most once per table. The kinds for the first slice are:
  - create a table, with its name, columns, and primary key as data and not as SQL text, so that replay needs no SQL parser
  - drop a table, by its name; the rows are not listed as deletes
  - put rows, each a full row that replaces any row with the same primary key
  - delete rows, each by its primary-key values
- **The kinds of change follow the roadmap.** A kind is defined when the slice that needs it is built: `alter table` and indexes each become a new kind. Every `insert`, `update`, and `delete` ends up as put and delete, so row changes need nothing new.
- **An unknown kind of change is refused, never skipped.** An instance that meets a kind it does not know stops replay with an error that says a newer version of s4ql is needed. Skipping a `drop table` would silently leave the cache different from the log.
- **Every kind of change has a message named after it, and each holds only what it needs.** `CreateTable` holds the table name, the columns with their types, and the primary key. `DropTable` holds the table name. `PutRows` and `DeleteRows` hold the table name and the rows. Markus asked for this in the review: "DropTable doesn't need columns nor primary key. That's my point." The field for the table name is `table_name` in all four, because SQLite's grammar calls it `table-name`. Markus: "I'd just like to be consistent and aligned with SQLite if possible."
- **Row changes carry no column names or types,** and replay does not compare them with the cache. Markus: "The precondition is that the cache is correct up to the parent, and with a deterministic change, the commit is safe and the postcondition is the same." Replay already checks that precondition through the cache's recorded position, so a column check could never fail. The silent failures that the research found ([research/change-file-contents.md](research/change-file-contents.md)) come from systems where the reader's tables are managed apart from the writer's, which is not our case. This reverses the "heading" with names and types that was agreed on 2026-11-11. Two checks remain without extra data: a row with the wrong number of values is refused, and the cache's strict tables reject a value of the wrong type.
- **Rows come in batches.** A `PutRows` or `DeleteRows` must be complete in memory before it is written, so it is limited to about a megabyte, and a commit that changes many rows of one table writes several.
- **The primary key is a list of column positions** in `CreateTable`, in key order, starting at 0. A position cannot disagree with a column name the way a second copy of the name could.
- **A value is in one of SQLite's five storage classes,** under SQLite's names: integer, real, text, blob, and null. Null is stored explicitly, because SQLite treats it as a storage class, and because a value with no kind set must stay distinct from it: that is either a kind from a newer version or a malformed value, and both are refused.
- **All text is valid UTF-8:** table names, column names, text values, and SQL text. They are `string` fields, which Protocol Buffers refuses to write or read otherwise. s4ql rejects a statement, a parameter, or an expression that would produce other text; see the restrictions in [roadmap.md](roadmap.md). Markus: "I have a hunch that's going to make life easier down the line."
- **The commit time is in the header.** Replay does not use it, and it comes from the holder's wall clock. It is included because a later feature such as restoring to a point in time needs it, and it cannot be added to old commit objects. The time that a statement read is a different thing: SQLite fixes the time per statement, not per transaction, so it will be a field of `Statement` when a slice needs it.
- **Annotations are what replay may skip.** An annotation is today always one statement: its SQL text and its parameters. Its non-deterministic inputs become fields when a slice first needs them. The parameters make it complete, so a tool can run a commit's statements through the engine again and compare the outcome with the recorded changes. The cost is that the data of an insert is stored roughly twice. Leaving them out can become a setting later.
- **Where something sits says whether it may be skipped.** Everything in `changes` must be understood, or the commit object is refused. Everything in `annotations` may be skipped, including a kind of annotation that the reader does not know. Markus chose the name "annotation" over "evidence".
- **The statements and the changes are not linked.** The commit object does not say which statement caused which change.

Open: what a reader does with a field it does not know outside the annotations, for example a later "not null" on a column. Either the reader refuses the commit object, or we raise the header's version for every such field. maragubot recommends the first.

The schema, as revised in pull request #4. It passes `buf lint` with the `STANDARD` and `COMMENTS` rules. The file's place in the repository is not decided:

```proto
// The format of an s4ql commit object.
//
// The format is a draft while s4ql is in development. Anything in it can
// change, including field numbers and types, and a commit object written
// in it carries version 0.
//
// From version 1 on, commit objects are kept forever, and every version of
// s4ql must read every one ever written. Field numbers and field types then
// never change, and a number that is no longer used is reserved. Names can
// change.
edition = "2024";

package s4ql.v1;

import "google/protobuf/timestamp.proto";

option go_package = "maragu.dev/s4ql/internal/s4qlpb";

// A commit object: everything one commit did.
//
// It is written and read as a stream: the header first, then the changes
// and the annotations, one at a time.
message CommitObject {
  // Exactly once, and first. A reader refuses a commit object without it.
  Header header = 1;
  // What the commit changed, in the order in which replay applies it.
  // A reader refuses the commit object if it does not know a change.
  repeated Change changes = 2;
  // What else is known about the commit. Replay does not use them, and a
  // reader may skip them.
  repeated Annotation annotations = 3;
}

// What the commit object is: its format, what came before it, and when it
// was made.
message Header {
  // The version of this format: 0 while the format is a draft. A reader
  // refuses a version it does not know. From version 1 on, the version is
  // raised when something is added that a reader must not ignore.
  uint32 version = 1;
  // The name of the commit object before this one: 32 bytes, all zero in
  // the first one.
  bytes parent = 2;
  // The wall-clock time of the commit, from the clock of the lease holder.
  // Replay does not use it.
  google.protobuf.Timestamp commit_time = 3;
}

// One change to the database.
//
// A Change holds no field outside `kind`, now or later. A reader that finds
// no known kind refuses the commit object: if the change holds an unknown
// field, a newer version of s4ql wrote it; if it holds nothing, it is
// malformed.
message Change {
  // What the change is.
  oneof kind {
    // Creates a table.
    CreateTable create_table = 1;
    // Drops a table and all its rows.
    DropTable drop_table = 2;
    // Puts rows into a table.
    PutRows put_rows = 3;
    // Deletes rows from a table.
    DeleteRows delete_rows = 4;
  }
}

// Creates a table.
message CreateTable {
  // The name of the table.
  string table_name = 1;
  // The columns, in their declared order.
  repeated Column columns = 2;
  // The primary-key columns, in key order, as positions in `columns`,
  // starting at 0.
  repeated uint32 primary_key = 3;
}

// One column of a table.
message Column {
  // The name of the column.
  string name = 1;
  // The declared type of the column.
  Type type = 2;
}

// The declared type of a column. More are added as the roadmap grows.
// A reader refuses a value it does not know.
enum Type {
  // Never written. A reader refuses it.
  TYPE_UNSPECIFIED = 0;
  // The STRICT type INT.
  TYPE_INT = 1;
}

// Drops a table and all its rows.
message DropTable {
  // The name of the table.
  string table_name = 1;
}

// Puts rows into a table. A row replaces any row with the same primary key.
//
// A PutRows is a batch that is small enough to hold in memory. A commit that
// puts many rows into one table writes several batches.
message PutRows {
  // The name of the table.
  string table_name = 1;
  // The rows. Each holds a value for every column, in the table's column
  // order.
  repeated Row rows = 2;
}

// Deletes rows from a table.
//
// A DeleteRows is a batch, like a PutRows.
message DeleteRows {
  // The name of the table.
  string table_name = 1;
  // The rows. Each holds only its primary-key values, in key order.
  repeated Row rows = 2;
}

// One row: its values, in the order that the change gives.
message Row {
  // The values.
  repeated Value values = 1;
}

// One SQLite value, in one of SQLite's five storage classes.
//
// A Value holds no field outside `kind`, now or later. A reader refuses a
// value with no known kind, as it does a change.
message Value {
  // The storage class of the value, and the value.
  oneof kind {
    // A 64-bit signed integer.
    sint64 integer = 1;
    // A 64-bit IEEE 754 floating-point number.
    double real = 2;
    // Text. It is always valid UTF-8.
    string text = 3;
    // A blob.
    bytes blob = 4;
    // NULL.
    Null null = 5;
  }
}

// The SQL NULL. It has no fields.
message Null {}

// One thing that is known about a commit beyond its changes.
//
// A reader skips a kind of annotation it does not know.
message Annotation {
  // What the annotation is.
  oneof kind {
    // One statement, as it ran.
    Statement statement = 1;
  }
}

// One statement, as it ran.
message Statement {
  // The SQL text. It is always valid UTF-8.
  string sql = 1;
  // The parameters, by position, starting with parameter 1.
  repeated Value parameters = 2;
  // Non-deterministic inputs, such as the time the statement read, become
  // fields here when a slice of the roadmap first needs them.
}
```

Details chosen by maragubot, from the research in [research/change-file-contents.md](research/change-file-contents.md) and [research/protobuf-practices.md](research/protobuf-practices.md):

- **Each commit object stands alone.** No state carries over from an earlier one.
- **An update of a primary key is a delete of the old key and a put of the new row.**
- **New kinds can be added to a `oneof` later,** as a new member with a new number. Old commit objects stay valid, and `buf breaking` accepts the addition. Moving an existing field into a `oneof` is unsafe.
- **`Change` and `Value` never get a field outside their `oneof`.** Any unknown field in them then means a kind from a newer version, and the reader finds it with `ProtoReflect().GetUnknown()`. One with no kind and no unknown field is malformed.
- **What the reader must do:**
  - refuse a `Type` value it does not know, which Go keeps as a plain number without an error
  - never set `DiscardUnknown`
  - raise the size limit of `protodelim`, whose default of 4 MiB is below SQLite's limit of 1 GB for one value
- **The schema uses `edition = "2024"`,** not `proto3`. Every field then records whether it was set, and the generated Go code is the kind the documentation recommends. The bytes are the same, so this can be changed.
- **CI checks the schema** with `buf lint` and with `buf breaking` under its `WIRE` rules, checks that the committed generated code matches the schema, and reads sample commit objects from every format version.
- **There are no before-images and no count of changes, for now.** Both can be added later as new fields.
- **For later slices:** replay applies results only, so it must not run triggers or cascades, and foreign keys cannot be checked row by row, because a commit object holds the net effect and not the order of the statements. Adding a column with a default is one change, with no rows repeated.

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

## Vendoring s4ql

Status: open, not fixed.

A module that vendors s4ql with `go mod vendor` cannot build, because Go vendors only directories that hold Go packages and so drops `internal/sqlite/c/`, which holds the SQLite C code. It is not fixed, because nothing depends on s4ql yet.
