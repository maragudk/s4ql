# Inspiration

This document lists good ideas from other projects and explains how each idea works in its project. It does not say which of them this project uses. Keep it concise, preferably bullet points only.

## Terms

- **WAL:** write-ahead log. Changes are appended to it before they are applied anywhere else.
- **LSN:** log sequence number. The position of a commit in a log, counted 1, 2, 3 and so on.
- **LSM tree:** log-structured merge-tree. Writes are collected in memory and flushed as immutable sorted files, which background compaction merges into larger ones.
- **SST:** sorted string table. An immutable file of key-value pairs sorted by key.
- **DDL:** data definition language. The SQL statements that change the schema, such as `CREATE TABLE`.
- **ETag:** an identifier that object storage returns for a version of an object. It changes when the content changes.
- **`If-None-Match: *`:** a condition on a PUT that makes it succeed only if no object exists at that key (create-if-absent).
- **`If-Match`:** a condition on a PUT that makes it succeed only if the object's ETag still equals the given one (compare-and-swap).
- **412:** the HTTP status "precondition failed", returned when one of those conditions does not hold.
- **ULID:** universally unique lexicographically sortable identifier. A random id that sorts by creation time.
- **NVMe:** non-volatile memory express. Fast local solid-state storage.
- **FIFO:** first in, first out.
- **VFS:** SQLite's virtual file system interface, the layer through which SQLite does all its file reads, writes, and locking. A plugin can replace it.
- **LTX:** Litestream's file format for a set of changed SQLite pages: a header with the range of transaction ids, the pages sorted by page number, a page index, and a checksum trailer.

## Graft

Graft is a storage engine in Rust that replicates a SQLite database, as 4 KiB pages, directly to object storage with no server in between.

**Storage:** not an LSM tree. An append-only log of commits, each adding one immutable segment of changed pages; a page is read from the newest commit that wrote it, and nothing is compacted. Local state lives in Fjall, an embedded LSM key-value store.

### Design

- **One conditional write commits.** A commit creates the object `logs/{log}/commits/{LSN}` with `If-None-Match: *`, where the LSN is the last known LSN plus one and LSNs have no gaps. Once the object exists the commit is durable and visible to every client; a writer that loses the race gets a 412, and the bucket holds no manifest, pointer, or lock object.
- **Self-contained commit object.** The commit object holds the log id, the LSN, the volume's page count, a content hash, and an index of the one segment it adds: the segment id, the set of pages in it, and the size of each compressed frame. The page set is a Splinter, a compressed bitmap of 32-bit numbers built as a 256-way tree over their bytes, which stays small for sparse sets and can be read without decoding; with it a reader finds the segment, frame, and byte range of any changed page from the commit alone.
- **Data before the commit.** A writer first uploads the changed pages as a segment under a fresh random id, `segments/{SegmentId}`, and only then creates the commit object that names it. A commit that fails or loses the race leaves an unreferenced segment and nothing to roll back.
- **A hash settles an ambiguous commit.** Before the conditional write, the client stores a pending record locally with the target LSN and the commit hash, a BLAKE3 hash over the log id, the LSN, the page count, and every page's number and contents. After a 412, any other error, or a restart, it reads the commit at that LSN: the same hash means its own write landed, a different hash means another writer won, and a missing object means the write never landed and can be retried.
- **Head found by probing.** A client learns of new commits by fetching the object at its last known LSN plus one and continuing upward until a GET returns 404. A client that is up to date spends one GET per check and never lists the bucket.
- **Lazy, partial page fetching.** A segment is a sequence of zstd frames of at most 64 pages each (256 KiB uncompressed), sorted by page number, and the commit lists each frame's compressed size, so summing sizes gives any frame's byte range. A client downloads commits but no pages up front; on a cache miss it fetches the one frame it needs with a single range GET and caches all of that frame's pages under (segment id, page number), and a page that no commit wrote reads as zeros.

### Testing

- **Bank workload that records its own history.** Setup creates 100,000 accounts with a balance of 1,000 each; every transaction makes 1 to 100 transfers between random accounts and, in the same transaction, inserts a row per transfer into a `transfers` table. The validator checks that the balances sum to 100,000,000 and that every account equals its initial balance minus what the table says it sent plus what it received, which catches lost or duplicated transactions that still conserve the total.
- **Named crash points.** The commit path contains named fault points (before the pending record is stored, before the conditional write, after it, and at the start of recovery), and a point that trips kills the process. Under Antithesis, a deterministic testing platform, each point trips with probability one half, drawn from the platform's random source so the run can be replayed; a unit test instead forces a named point to trip a set number of times, for example once right after the commit lands, and asserts that recovery adopts the existing commit.
- **Reachability checks.** Graft's assertion library, precept, registers every assertion call site in a catalog at startup, so the testing platform also knows about assertions that never ran. Graft marks each outcome of commit recovery (success, diverged, push failed) as a point that must be reached, so a branch that no run exercised is reported instead of passing silently.
- **A killed process's state is reused.** Each invocation of the test client picks at random an existing client data directory that it can lock with a non-blocking file lock, or creates a new one. A later process therefore inherits the local state of a killed one, including any pending commit record, and has to recover from it, which tests crash recovery without a dedicated restart step.

## FoundationDB

FoundationDB is a distributed, transactional, ordered key-value store; the SQL Layer and the Record Layer are relational layers built on top of it.

**Storage:** a replicated transaction log, separate from storage servers that apply it to a pluggable local engine. The classic `ssd` engine is a B-tree derived from SQLite; Redwood (a versioned B+tree) and RocksDB (an LSM tree) are alternatives. Storage servers also keep the last 5 seconds of versions in memory.

### Design

- **An unknown commit result comes with a guarantee.** When the connection to the commit proxy is lost or the proxy fails during a commit, the client gets `commit_unknown_result`, and at that moment the transaction has either committed or never will: it is no longer in flight. A retry can therefore settle the outcome by reading, and the documented pattern is to generate a unique id outside the retry loop and, inside the loop, return early if the transaction's own side effect under that id is already present.
- **Five-second transaction lifetime.** A transaction lives at most 5 seconds: after that its reads fail with `transaction_too_old` and it cannot commit. The limit lets the resolvers and storage servers keep only 5 seconds of old versions, in memory, and long jobs run as many short transactions, with Record Layer scans stopping at a record, byte, or time limit and returning an opaque continuation to resume from.
- **Schema generation key.** The SQL Layer stores the schema next to a `generation` key. Every transaction reads that key to pick its schema version, and every DDL statement increments it in the transaction that changes the schema, so optimistic conflict detection makes a schema change conflict with every concurrent transaction.
- **Online index builds.** In the Record Layer a new index starts write-only: every record write maintains it in the same transaction, but queries cannot use it. A background indexer then scans the existing records in many small transactions (by default at most 100 records, 900,000 bytes, or 4 seconds each), stores the key ranges it has finished, and finally marks the index readable.

### Testing

- **Deterministic simulation.** Network, disk, time, and randomness sit behind interfaces, so the production code of a whole cluster, together with the test clients, runs as coroutines on one simulated thread, and one seed with one binary always replays the same run. To catch accidental nondeterminism, a run ends by printing the next number from its random generator, and the harness reruns 5% of runs with the same seed and compares the two numbers, which differ after any extra or missing random draw.
- **BUGGIFY.** A `buggify()` call in production code returns true only in simulation and makes the code do something legal but unusual: return an error from an operation that usually succeeds, add a delay, take a rare branch, or contact only a minimal quorum. Buggify is on in 80% of runs, each call site is switched on or off once per run with probability 0.25, and an active site fires with probability 0.25 per evaluation, so every run stresses a different subset of sites.
- **Randomized knobs.** Each tuning parameter is defined with its default and an unusual value that a simulation run may pick instead, through a buggify call at the definition; about 350 definitions do this. Running with odd values shows whether any tuning value has become necessary for correctness.
- **Cycle workload.** N keys form a ring of next-pointers, and each transaction follows three pointers from a random key and rewrites three pointers so that two adjacent nodes swap places. Every read must find the next key, and the final check must find exactly one cycle of length N, which exposes lost updates, partial commits, and stale reads; a transaction retried after an unknown commit result is harmless because it reads again and makes another valid swap.
- **Sideband workload.** One client commits a unique key and sends the key and its commit version to a checker client over a separate channel; the checker then starts a new transaction and must see the key. A miss is a causal consistency error, so the workload checks strict serializability across clients.
- **Faults stop before the final checks.** After the workload phase the simulator sets a flag that every destructive fault injection checks, the simulated environment returns to a state in which recovery is possible, and the cluster must recover. Only then does a consistency check compare all replicas of every shard and each workload run its own final check.
- **Unexpected errors fail the run.** Any trace event of error severity, anywhere in the simulated cluster, fails the run whatever the workloads report. An error that carries an injected fault is downgraded automatically, so only errors the test did not cause count.
- **Coverage probes for rare paths.** `CODE_PROBE(condition, "comment")` marks a situation that the tests ought to reach, such as a full buffer, and records its first hit in each run. The harness adds up the hits per probe across a batch of runs and reports the probes that were missed, which shows where a workload, a fault, or a buggify call is lacking.

## SlateDB

SlateDB is an embedded key-value store in Rust, a log-structured merge-tree that keeps all its files in object storage.

**Storage:** an LSM tree. Writes go to WAL objects and an in-memory table, are flushed as level-0 SSTs, and are merged by compaction into sorted runs; a manifest lists the SSTs that make up the current state.

### Design

- **Durable when the WAL object exists.** Writes collect in memory and go out together as one WAL object, `wal/<N>.sst`, created with `If-None-Match: *` at the next number; that creation is the commit point, and the manifest is not touched per write. Each write returns a handle whose `await_durable()` resolves when the PUT succeeds.
- **Epochs fence old writers.** The manifest, written as numbered immutable versions that are each created with `If-None-Match: *`, holds a writer epoch and a compactor epoch. A writer that opens the database publishes a manifest version with the writer epoch plus one, creates a zero-byte fence object at the first free WAL number, replays the WAL up to the fence, and confirms that it still owns the epoch before it serves anything; the old writer's next WAL write collides with the fence, its next manifest update sees the higher epoch, and both fail as fenced.
- **Boundary files stop name reuse.** Create-if-absent protects only against objects that exist, so once garbage collection has deleted manifest version N+1, a stalled process could create N+1 again and report a stale update as committed. The collector therefore advances a number in `gc/manifest.boundary` with an `If-Match` write before it deletes any version at or below that number, and every writer reads the boundary after its create and treats a version at or below it as failed.
- **A unique token settles a retried write.** Every conditional PUT carries a fresh ULID in the object's user metadata. If a retried PUT ends in "already exists" or "precondition failed", the client sends a HEAD and compares the stored ULID with its own: a match means its earlier attempt landed and the write succeeded, and anything else is a real conflict.
- **Closing the handle on an unsettled outcome.** A fatal WAL flush error closes the writer, and the handle rejects all further use. Reopening the database fences the old handle and replays the WAL, so the new handle learns the outcome from the bucket.
- **Checkpoints and clones.** A checkpoint is a manifest entry that names a manifest version and an expiry time; garbage collection keeps that version and every file it references, and an open reader holds one (10 minutes by default, refreshed at half-life). A clone is a new database whose manifest points at the parent's SSTs through a checkpoint in the parent, so only that checkpoint's WAL objects are copied.
- **Invariants checked before every manifest update.** Registered invariant functions run against the new manifest value before each conditional create. States that should be impossible, such as a stored epoch lower than the writer's own or a boundary file that has vanished, panic the process.

### Testing

- **Formal models of the protocol.** Two specifications in FizzBee, a model checker, describe the protocols: one covers writer fencing, WAL replay, and garbage collection, and the other covers boundary files, with the create and the boundary check as separate atomic steps so that collection can run between them. Ghost state in the boundary model records what a writer reported as successful, apart from which objects exist, and the checker asserts properties such as "no WAL number is reported successful twice" and "commits happen only above the boundary".
- **Deterministic simulation harness.** One root seed derives the seeds for a single-threaded runtime, the clock task, the fault controller, and every actor; the actors (transfer clients, auditors that check the sum of balances, a compactor that keeps restarting, and a fencer that reopens the database and asserts that the old handle now fails as fenced) run as tasks against a mock clock and a fault-injecting object store. Configuration, from the flush interval to the garbage collection age, is drawn from the seed as well, and a failing run is replayed by passing its seed.
- **Response-side faults.** The fault-injecting object store applies faults to the request, performs the real PUT, and then applies faults to the response. A connection reset on the response side therefore returns an error for a write that was applied, which is the ambiguous outcome that the token check has to settle.
- **Lint rules keep time and randomness injectable.** Clippy's lists of disallowed types and methods ban operating-system random generators, UUID and ULID generators, the standard and Tokio clocks, sleeps and timeouts, and thread and blocking-task spawns, and the crates deny those lints. Code passes only if it takes time from the injected `SystemClock`, randomness from `DbRand`, and ids from `IdGenerator`, which is what lets the simulation control all three.

## Turbopuffer

Turbopuffer is a search service for vectors and full text, built on object storage.

**Storage:** an LSM tree on object storage. A WAL takes the writes, and background indexing turns them into SSTables and index files, merged by a simple tiered compaction.

- **Object storage is the only source of truth.** Each namespace is a prefix in object storage that holds its WAL and its index files. Query nodes put a memory cache and an NVMe cache (a FIFO ring buffer) in front of it, both read-through and write-through, so any node can serve any namespace, and requests for one namespace are routed to the same node to keep its cache warm.
- **A manifest compare-and-swap commits a write.** The node writes the new WAL file to object storage and then commits it into a manifest with compare-and-swap: it fetches the manifest with its ETag and writes the manifest back only if the ETag still matches.
- **Group commit.** Concurrent writes to a namespace collect in a memory buffer and are committed together as one WAL entry, at most one entry per second per namespace. The rate of writes to object storage is thereby decoupled from the rate of incoming requests.
- **Acknowledge only when durable.** A write returns success only after its data is durably written to object storage.
- **Strong reads through one conditional request.** Before it answers a query, the node that caches a namespace sends one conditional GET to object storage (`If-None-Match` with the ETag it holds) to confirm that it has the latest write, a round trip of about 10 ms. A query that asks for eventual consistency skips the round trip and reuses the node's cached view for up to 60 seconds by default.
- **Asynchronous indexing with replay.** Committed WAL entries are indexed in the background, and an index cursor records how far indexing has come. A strongly consistent query reads the index and replays every unindexed WAL entry on top of it, so a write is searchable as soon as it is committed.
- **A JSON file as the indexing queue.** The queue of indexing jobs is a single JSON file on object storage, updated with compare-and-swap by a stateless broker that group-commits changes to it. Workers keep ownership of a job by heartbeat, delivery is at-least-once, and a broker that has been replaced finds out when its next compare-and-swap fails.
- **Backpressure.** The write API returns HTTP 429 once a namespace's unindexed data exceeds 2 GB or 1 million rows, which holds writers back while indexing is behind.
- **Bounded round trips for cold queries.** A round trip to object storage takes about 100 ms, so the query planner counts sequential round trips as the main cost and fetches more bytes per trip when that saves a trip. The design target is at most three round trips for a cold query, which keeps it under one second.
- **Consistency over availability.** When object storage is unreachable, Turbopuffer gives up availability, not consistency.

## Litestream

Litestream is a tool in Go that replicates a SQLite database to object storage from its WAL. The Litestream VFS is a SQLite plugin that queries such a replica directly in object storage.

**Storage:** compacted like an LSM tree. Each sync turns the newly committed WAL pages into an immutable LTX file for the next transaction id. Background compaction merges these files level by level, by default every 30 seconds, 5 minutes, and 1 hour, and a daily full snapshot sits above the levels.

- **Capture by owning the checkpoint.** Litestream keeps a read transaction open so that no other connection can checkpoint and restart the WAL. Every second it copies the newly committed WAL frames into one LTX file numbered with the next transaction id, and it checkpoints only after copying, so no frame is lost to a checkpoint, the ids have no gaps, and every file ends at a commit.
- **Page index at the end of the file.** An LTX file stores its pages sorted by page number and compressed one by one, followed by an index of each page's offset and size and, just before the trailer, the index's 8-byte length. A reader fetches the last 32 KiB with one range GET, reads the length, and so has the whole index, with a second GET only when the index is larger.
- **Open by reading indexes only.** To open a database the VFS makes a restore plan, the newest snapshot followed by, from any level, each next file that extends the gap-free range of transaction ids furthest, and fetches only the page index and header of each file, about 1% of its size. Merged with the newest transaction id winning, these form an in-memory map from page number to file, offset, and size, and no page data is downloaded until SQLite reads a page.
- **One range GET per page, then a cache.** The VFS turns the offset of each SQLite read into a page number, looks it up in the map, and fetches exactly that compressed page with one range GET. Pages go into a least-recently-used cache sized in bytes, 10 MB by default, which keeps hot pages such as inner B-tree pages local.
- **Compaction on a clock grid.** Level 0 holds one file per sync, levels 1, 2, and 3 merge the files of the level below at fixed clock boundaries (by default every 30 seconds, 5 minutes, and 1 hour) and keep only the newest version of each page, and level 9 holds full snapshots. A level 0 file is deleted only after it is merged into level 1 and is at least 5 minutes old, which gives polling readers time to find the level 1 file that replaces it.
- **Polling for new files.** Every second the VFS lists levels 0 and 1 for files past its last transaction id, fetches only their page indexes and headers, merges them into its map, and evicts the changed pages from its cache. A gap at level 0 is filled from level 1, and a file whose database page count is smaller than before, as after a `VACUUM`, replaces the map instead of being merged into it.
- **A pending index keeps read transactions stable.** While a connection holds SQLite's shared lock, index entries from new files go into a separate pending map. When the lock is released, the pending map is merged into the main one and the changed pages are evicted, so a read transaction sees the database as it was when the transaction started.
- **Time travel by swapping the index.** `PRAGMA litestream_time = '5 minutes ago'`, or a timestamp, computes a restore plan from the files created before that time, builds a fresh map from it, and swaps it in for that connection while polling pauses. `'latest'` swaps back, and the reachable points are the boundaries of the files that retention still keeps.
- **Background hydration.** The VFS can stream every file of the restore plan through LTX compaction into a full local database file while it keeps serving reads remotely, then switch reads to that file and write newly polled pages into it. A companion `.meta` file records the transaction id the file is at, so a restart resumes from there unless the remote id is now lower, in which case the VFS starts over.
- **Write mode with batched uploads.** In write mode, SQLite's page writes go to a local buffer file and a map of dirty pages, and every second, when no write transaction is open, all dirty pages are uploaded as one level 0 file with the next transaction id. Before the upload the VFS lists level 0 and returns a conflict error if the remote id has moved past the one it expects, and in one process only one connection at a time may hold the write lock.
