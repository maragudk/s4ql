# To do

A working list of things that are agreed but not built, and of design questions that are still open. The agreed rules are in [spec.md](spec.md), and the SQL slices are in [roadmap.md](roadmap.md).

Each item is marked agreed or open. An agreed item carries the details that a builder needs who was not in the conversation. When an item is built, it is deleted from this list, and its reasoning moves next to the code as tests and doc comments. The code is then the source of truth for how it was done.

## The storage core

Status: built in `internal/core` on 2026-11-12 and 2026-11-13, and merged in pull request #12. The reasoning behind its design, with Markus's words, is in [internal/core/README.md](../internal/core/README.md). This item holds what is still open or not built. What its tests do not show yet is in [Tests of the storage core](#tests-of-the-storage-core).

- **Open: a manifest that is damaged on its way.** A commit object is checked against its name, and the manifest has no such check. A damaged manifest that still parses would be believed, and the swap that takes the lease would write it back. The store's contract promises content that is not changed. Whether the manifest needs a checksum of its own is not decided.
- **Agreed: `DB.Close` closes the cache whatever the store does.** Now `DB.Close` spends its whole context on releasing the lease before it closes the cache. With a short deadline and a store that cannot be reached, the caller gets an error, and the cache stays open, again at every retry. maragubot will have it changed so that closing the cache does not depend on the store. One way is to close the cache without waiting when no transaction holds a lock. Another is to give the release only a part of the context.
- **Not decided: catch-up downloads every missing commit object twice.** It reads each one on the walk back through the log, and again when it applies it. A spool of the walk could remove the second download.
- **Not decided: a connection whose close fails.** `DB.Close` marks the instance as closed before it knows that the connections closed, so a connection whose close fails is never closed. Since the fix of the scan, only an `iter.Pull` of a scan without its `stop` can cause that.
- **A possible later addition: S3's timestamps could shorten the wait after a crash.** They would be switched on only for a store that has been checked. They help how fast an instance serves, not safety. Why S3's clock is not safe enough to rest the lease on is in [research/s3-clock.md](research/s3-clock.md).
- **To decide with the public driver: OpenTelemetry, traces, and metrics.** The core logs one structured event for each unit of work through `log/slog`, and nothing else.

## Tests of the storage core

Status: open. What the tests of `internal/core` and the simulation harness do not show yet, and the runs that are not done.

- **No test shows that the margin on the watcher's wait is enough.** The harness cannot make a clock run fast, so the fault that the margin is for stays out of the workloads.
- **The recovery in `Open` after a crash is not exercised.** A restarted instance in the simulation never opens a cache with a write-ahead log that the crash left, because the copy of the cache is checkpointed first (`cacheDump`).
- **The simulation does not reach every failure.** It does not have SQLite's "busy" or the order in which the connections open, because the scheduler runs one goroutine at a time. It also does not have a manifest that is damaged on its way, a cache that fails anywhere but at the commit after a manifest swap, a power loss, or clock faults on an instance that was restarted after a crash.
- **A real S3 client under simulation is not shown.**
- **A separate stress test on the real clock is not built.** It covers what only true parallelism reaches. The free-running mode runs on the fake clock of a `testing/synctest` test. Ideas to revisit later about the free-running mode are in [internal/sim/README.md](../internal/sim/README.md#ideas-to-revisit-later).
- **The planted bugs were counted over 1000 seeds each, not 10000.** Over 10000 seeds they need about five and a half hours on the machine of the build.
- **Six decisions of the core have no test that can reach them.** The mutation testing on the final code changed each of them, and no test failed; the list is in [internal/core/README.md](../internal/core/README.md). Four need a file system that fails in the middle of an operation: a spool file that fails during an upload, a local failure at the end of a read, a failure of the cache during replay, and a spool file whose length cannot be read. One needs a power loss, and one needs real parallelism after a crash. A file system that fails on demand would cover the first four.
- **The clean runs of the planted bugs repeat each other.** 40 of the 190 clean runs are the same as others, because several bugs share a workload. Each distinct clean workload could run once, or each bug could get its own seeds.
- **The toy library in `internal/sim` can be removed.** The core's tests now cover its ten planted bugs, its three workloads, the free-running mode, and the planted data race.

## The memory-mapped wal-index

Status: open. A local I/O failure on the cache stops the instance, as the spec says, and `internal/core` does that. The failure must arrive as an error for that.

- **Open: the wal-index is memory-mapped.** The build sets `SQLITE_MAX_MMAP_SIZE=0`, so SQLite never memory-maps the database file, and an I/O error there arrives as an error code. In WAL mode SQLite still memory-maps its shared index, the `-shm` file: "The wal-index is implemented using an ordinary file that is mmapped" (https://sqlite.org/wal.html). An I/O failure on a memory-mapped file arrives as a signal that kills the process, not as an error that SQLite can handle (https://sqlite.org/mmap.html). How to handle this is not designed.
- **A possible answer, parked: SQLite's `unix-excl` VFS.** Markus on 2026-11-11: "park it in TODO.md for now as a potential thing". It keeps the wal-index in ordinary memory and creates no `-shm` file, which would close the gap above. It also locks every other process out of the cache file while our process has it open, including the `sqlite3` shell, so a live cache cannot be inspected from outside. Several connections in one process still work. It does not change the file format, so it can be switched on or off at any time, and `sqlite.Open` would need a VFS name. To find out before deciding: what happens after a crash (whether the operating system releases the lock, and that the next start recovers from the WAL), and a test that no signal arrives on a disk error. It exists on macOS and Linux only. Details are in [research/sqlite-busy.md](research/sqlite-busy.md).

## Storage formats

Status: the manifest and the commit object are built in `internal/format`, with the schema in `proto/s4ql/v1/commit.proto`, and replay into the cache is built in `internal/core`. One check in CI is not built, and two questions are open. The reasons behind the formats are in [internal/format/README.md](../internal/format/README.md).

- **`buf breaking` under its `WIRE` rules is set up when version 1 is settled.** While the formats are drafts, a change to the schema may break old commit objects.
- **Nothing produces an `UpsertRows` with only some columns yet.** The format writes and reads it, the samples cover it, and replay applies it. It is first produced when a slice of the roadmap brings `update`.
- **Replay in later slices applies results only.** When a slice brings triggers, cascades, or foreign keys, replay must not run triggers or cascades. Foreign keys cannot be checked row by row at replay, because a commit object holds the net effect and not the order of the statements.
- **Open: whether `integer` as a declared type becomes a second value of `Type`,** or is treated as the same type as `int`. For s4ql the two behave the same, because every table is `WITHOUT ROWID`. The slice that adds `integer` decides.
- **Open: whether a new lease holder keeps the unknown fields of the old lease.** `Manifest` keeps the unknown fields of a `Lease` only while the same value is changed in place, and a new `Lease` has none. The core does not keep them: a new holder writes a new lease entry.

## The S3 client and S3's checksums

Status: agreed with Markus on 2026-11-12, not built. The interface `store.Store` and the simulated store are built in `internal/store`; the client for real S3 is the next piece.

Markus asked: "Maybe S3 already knows the hash of its objects?" It does, and s4ql uses it as a second guard next to its own check.

- **On upload, the client sends the commit object's SHA-256 with the request.** S3 computes its own and rejects the object if they differ, so a commit object is never stored under a name that does not match its bytes.
- **On download, the client compares the checksum that S3 returns with the name,** before it reads the body. S3 stores the checksum with the object and returns it in the response headers.
- **The reader still computes the checksum itself as it reads.** That check covers the whole way into our process, and it is the same against the simulated store and against a store that does not offer checksums. The spec asks three things of a store, and checksums are not among them. Which S3-compatible stores offer them is not checked.
- **This holds for commit objects up to 5 GB,** the limit of one `PutObject`. In a multipart upload, S3 offers SHA-256 only as a checksum of the checksums of the parts, which is not our name. Markus: "Don't worry too much about commit objects above 5GB for now."

Source: https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html

What the client must do, from building the simulated store. The sources are in `docs/diary/2026-11-12-object-store.md`:

- **It passes the conformance suite,** `storetest.Test`, against real S3, with a new and empty key prefix for each case.
- **It never lets the SDK retry a conditional write by itself,** or it reports the refusal of such a retry as an unknown outcome. Otherwise the first attempt can be what made the retry fail, and the client would report "not applied" for a write that was applied.
- **It maps S3's answers to the errors of `store.Store`:** 412 on a create is `ErrExists`; 412 and 409 on a swap are `ErrChanged`; 404 is `ErrNotFound`; `BadDigest` is `ErrChecksum`. Everything else on a write is an unknown outcome.
- **It needs the permission `s3:ListBucket`,** because S3 answers 403 and not 404 for a missing object without it.
- **It never sends an empty `If-Match`,** which Tigris treats as "create only".
- **On Tigris, the checksum goes in a trailer.** Tigris does not check a checksum that is sent as a header ([research/tigris-probe-results.md](research/tigris-probe-results.md)).
- **Open: the check on download.** `store.Store.Get` neither returns the checksum that the store holds nor takes one to compare. The builder of the client adds one of the two to the interface, the simulated store, and the suite, if it is cheap, and otherwise says why not.
- **Telemetry for requests to S3 is decided with this client.**

## Vendoring s4ql

Status: open, not fixed.

A module that vendors s4ql with `go mod vendor` cannot build, because Go vendors only directories that hold Go packages and so drops `internal/sqlite/c/`, which holds the SQLite C code. It is not fixed, because nothing depends on s4ql yet.
