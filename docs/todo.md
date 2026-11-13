# To do

A working list of things that are agreed but not built, and of design questions that are still open. The agreed rules are in [spec.md](spec.md), and the SQL slices are in [roadmap.md](roadmap.md).

Each item is marked agreed or open. An agreed item carries the details that a builder needs who was not in the conversation. When an item is built, it is deleted from this list, and its reasoning moves next to the code as tests and doc comments. The code is then the source of truth for how it was done.

## The SQL front end

Status: open. Nothing of the SQL half is built but the tokenizer, which is being built. The facts and the options behind every point here are in [research/sql-front-end.md](research/sql-front-end.md). Markus decides each open point before it is built.

- **Agreed, and not in the spec yet: `database/sql` is the main interface.** Markus agreed on 2026-11-10 that the driver is not registered by a blank import, that the open function builds a `driver.Connector` and passes it to `sql.OpenDB`, and that an explicit `Register` function exists for code that needs `sql.Open` by name. It is recorded only in `docs/conversations/2026-11-10-kickoff.txt`.
- **Open: the parser.** A parser for the grammar of the slices only, grown slice by slice, or a parser for all of SQLite's grammar with a pass after it that rejects what the roadmap does not list. Only the second can give SQLite's own syntax error for a statement outside the subset and tell it from "not supported". `sqlc-dev/meyer` is such a parser in Go; whether it is a reference, a starting point, or a dependency is part of the question.
- **Open: what an error promises to match.** The result code, the extended result code, and the message text, and for which statements. The codes alone do not tell `no such table` from `table already exists`.
- **Open: how "not supported" is marked.** A sentinel error, the result code it carries, and a message that cannot be one of SQLite's.
- **Open: whether an API of s4ql's own exists next to the driver, and whether it is public.** `database/sql` cannot carry the stop of an instance, a context on `Close`, or a context on `Commit`.
- **Open: single-quoted names.** SQLite accepts `'t1'` as a name in some places. The restrictions in the roadmap do not mention it.
- **Open: what "any integer value" in slice 1 includes.** A sign, hex literals, digit separators, and 9223372036854775808, which SQLite reads as a real and refuses for an `int` column.
- **Open: the errors of slice 1 that the roadmap does not list.** Its statement shapes can also give `table t1 has no column named v2`, `object name reserved for internal use`, `table sqlite_master may not be dropped`, `cannot store REAL value in INT column`, and `hex literal too big`.
- **Open: a bytecode machine or a tree of iterators for execution.** For slice 1 the two hardly differ.
- **A rule for the driver, whatever is decided: no error of an unknown commit outcome or of a lost lease wraps `driver.ErrBadConn`.** `database/sql` runs an operation again, up to three times, when the driver returns it.

## The storage core

Status: built in `internal/core` on 2026-11-12 and 2026-11-13, and merged in pull request #12. The reasoning behind its design, with Markus's words, is in [internal/core/README.md](../internal/core/README.md). This item holds what is still open or not built. What its tests do not show yet is in [Tests of the storage core](#tests-of-the-storage-core).

- **Open: a manifest that is damaged on its way.** A commit object is checked against its name, and the manifest has no such check. A damaged manifest that still parses would be believed, and the swap that takes the lease would write it back. The store's contract promises content that is not changed. Whether the manifest needs a checksum of its own is not decided.
- **Not decided: catch-up downloads every missing commit object twice.** It reads each one on the walk back through the log, and again when it applies it. A spool of the walk could remove the second download.
- **Not decided: a connection whose close fails.** `DB.Close` marks the instance as closed before it knows that the connections closed, so a connection whose close fails is never closed. Since the fix of the scan, only an `iter.Pull` of a scan without its `stop` can cause that.
- **A possible later addition: S3's timestamps could shorten the wait after a crash.** They would be switched on only for a store that has been checked. They help how fast an instance serves, not safety. Why S3's clock is not safe enough to rest the lease on is in [research/s3-clock.md](research/s3-clock.md).
- **To decide with the public driver: OpenTelemetry, traces, and metrics.** The core logs one structured event for each unit of work through `log/slog`, and nothing else.

## Tests of the storage core

Status: open. What the tests of `internal/core` and the simulation harness do not show yet, and the runs that are not done.

- **No test shows that the margin on the watcher's wait is enough.** The harness cannot make a clock run fast, so the fault that the margin is for stays out of the workloads.
- **The simulation does not reach every failure.** It does not have SQLite's "busy" or the order in which the connections open, because the scheduler runs one goroutine at a time. It also does not have a manifest that is damaged on its way, a cache that fails anywhere but at the commit after a manifest swap, a power loss, or clock faults on an instance that was restarted after a crash.
- **A real S3 client under simulation is not shown.**
- **A separate stress test on the real clock is not built.** It covers what only true parallelism reaches. The free-running mode runs on the fake clock of a `testing/synctest` test. Ideas to revisit later about the free-running mode are in [internal/sim/README.md](../internal/sim/README.md#ideas-to-revisit-later).
- **The planted bugs were counted over 1000 seeds each, not 10000.** Over 10000 seeds they need about five and a half hours on the machine of the build.
- **Six decisions of the core have no test that can reach them.** The mutation testing on the final code changed each of them, and no test failed; the list is in [internal/core/README.md](../internal/core/README.md). Four need a file system that fails in the middle of an operation: a spool file that fails during an upload, a local failure at the end of a read, a failure of the cache during replay, and a spool file whose length cannot be read. One needs a power loss, and one needs real parallelism after a crash. A file system that fails on demand would cover the first four.
- **The clean runs of the planted bugs repeat each other.** 40 of the 190 clean runs are the same as others, because several bugs share a workload. Each distinct clean workload could run once, or each bug could get its own seeds.

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

Status: built in `internal/store/s3store` on 2026-11-13. It passes the conformance suite against the project's Tigris bucket. What was found about Tigris, and what rests on AWS's documentation alone, is in [internal/store/s3store/README.md](../internal/store/s3store/README.md). This item holds what is still open.

Markus asked: "Maybe S3 already knows the hash of its objects?" It does. On upload, the client signs the request with the commit object's SHA-256 as the hash of its payload and sends it as the object's checksum, and the store refuses content that does not match it.

- **Open: the check on download is not the one that was agreed.** The agreed bullet was: "On download, the client compares the checksum that S3 returns with the name, before it reads the body." The builder of the client did not add a checksum to `store.Store.Get`. The client asks the store for the checksum that it holds, and the SDK compares it with the content as it is read, so a read of content that does not match it fails at its end. A comparison with the name before the body would need the name in the interface, and it would only catch an object stored with another checksum, which the check on upload rules out for every object that s4ql writes. The reader of a commit object reads to the end and checks the name anyway. Markus decides whether that is enough.
- **Not set up: the integration tests in CI.** They need a bucket for CI and its credentials as secrets of the repository.
- **Not checked: AWS, and other stores than Tigris.** There are no AWS credentials. The spec asks three things of a store, and checksums are not among them, so a store that does not check the payload's hash still works, without the second guard.

## Vendoring s4ql

Status: open, not fixed.

A module that vendors s4ql with `go mod vendor` cannot build, because Go vendors only directories that hold Go packages and so drops `internal/sqlite/c/`, which holds the SQLite C code. It is not fixed, because nothing depends on s4ql yet.
