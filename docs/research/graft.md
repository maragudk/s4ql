# Graft: deep dive (design, testing, lessons)

Compiled 2026-11-10. This builds on `research-architecture.md` and does not repeat it.

Legend:
- **[V]** verified from a primary source: repo source, docs, RFC, issue or PR text by the author, a release note, or the author's own blog.
- **[S]** secondary source.
- **[I]** my inference.

Repo: https://github.com/orbitinghail/graft. Cloned at `main` = `b07d931` (2026-06-17). Branches `feat/grove` and `v1` were also read. File paths below are relative to the repo root.

---

## 0. Status and history (read this first)

### Timeline [V, from git log, tags, releases and issues]
- **2024-10 to 2025-06: v1, a service architecture.**
  - The services are a MetaStore and a PageStore, combined into the crate `graft-server`, plus a `graft-client` library.
  - Releases v0.1.0 to v0.1.5. v0.1.5 shipped on 2025-06-20.
- **2025-01-08:** Antithesis testing starts, with commits "antithesis initial setup" and "working antithesis workload".
- **2025-01-27:** `precept` is extracted, with the commit "consolidiate antithesis code in precept".
- **2025-05-14:** RFC 0001 "Direct Storage Architecture" (PR #131) proposes removing the MetaStore and PageStore.
- **2025-11-07:** PR #221 "Graft sqlite v2" switches to the new architecture. Its description says: "Antithesis and integration tests have been temporarily disabled."
- **2025-12-03:** v0.2.0 ships. Release note: "Graft V2 is here! ... I dedicated myself to building a version of Graft that worked without any middleman servers between the client and object storage." Under "What's Changed" it says only "Everything." The latest release is v0.2.1 (2025-12-04).
- **2025-12-09:** PR #267 re-enables Antithesis for v2 and closes #261. Three snapshot bugs follow within two weeks (see §6).
- **2026-01 to 2026-02:** On the `feat/grove` branch (unmerged), Carl writes design plans for the next version: `plans/grove*.md`, `plans/graft3.md`, `plans/thoughts.md`.
- **2026-03 to 2026-06:** `main` gets only dependency updates and small fixes. The last commit on main is 2026-06-17.
- **2026-07-25:** Carl closes three externally reported bugs (#322, #323, #324) as "not planned", each with the same comment: "This issue is fixed in the next design of graft. **I'm not currently working on graft.** When I get back to it, this will be resolved."
- **Maintainer:** Carl Sverre's GitHub profile lists his company as `@antithesishq`. His recent public activity is on `antithesishq/snouty` and `antithesis-plugin`. So he now appears to work at Antithesis. [V for the profile, I for the job]
- **README:** "Graft should be considered **Alpha** quality software... contact @carlsverre before using it in production."

**Practical status as of 2026-11:** Graft is dormant alpha. The v2 code on `main` is the last shipped design. The author has stated that it is superseded by an unimplemented "next design". Treat v2 as a study object, not a reference implementation.

### v1 architecture [V: `v0.1.5:docs/src/content/docs/docs/internals/{index,metastore,pagestore}.md`]
- **MetaStore:** "Stores metadata for each Volume, including the log of Segments, Snapshot history, and current LSN. Also responsible for coordination tasks like garbage collection, authentication, and graft tracking."
  - It stored state in object storage: `/volumes/[VolumeId]/[LSN]` → CommitHeader `{vid, cid (client id), lsn, checkpoint_lsn, page_count, timestamp}` plus a list of Segment `{sid, size, graft: Splinter}`.
  - Commit log entries used fixed-width sortable LSN keys.
  - On a pull, it computed the "graft", the changed-page bitset between two versions, so clients could fast-forward in one round trip.
- **PageStore:** Stored pages in segments at `/segments/[SegmentId]`. Segment layout was Pages | Index (Volume index plus PageIdx list) | 32-byte footer.
  - A segment could hold pages from **many volumes**, which amortized PUT cost across tenants.
  - It acted as a caching tier (memory, then NVMe, then S3) hosted on Fly.io. [S: talk notes at tinfoil-knight.github.io]
- **Client:** Had local snapshots and a sync task, and a Leap-algorithm prefetcher (the `LeapOracle` still exists in `crates/graft/src/oracle.rs`).
- **Commit:** The client sent a commit request to the MetaStore. The MetaStore appended `{vid}/{lsn}`. "Only a single writer commits on a particular snapshot version." [S, talk notes]
- **v1 bug [V: commit `281cf05`, 2025-01-27]:** "fixed race in metastore/commit that could allow two commits to go through on the same base lsn."
  - The diff changes `store.put(...)` to `put_opts(..., PutMode::Create)`. In other words, the commit object was originally written **unconditionally**, so last-writer-wins.
  - This landed during the first Antithesis month. Antithesis found it or it surfaced while debugging under Antithesis. [I]

### Why the redesign [V: RFC 0001, `v0.1.5:docs/src/content/docs/docs/rfcs/0001-direct-storage-architecture.mdx`]
- **v1 advantages listed:**
  - The MetaStore can roll up commits to fast-forward clients ("instant read replicas").
  - The PageStore is a smart cache.
  - The PageStore "is able to collocate writes to multiple Volumes in the same Segment which can reduce the cost and overhead of small transactions."
- **v1 disadvantages listed:**
  - "There is little isolation between data in different Volumes. Graft will need to roll out a comprehensive encryption + authorization layer to work for production workloads. This is a huge cost in terms of testing and engineering."
  - "Users must run two services to take full advantage of Graft."
- **Other motivations:**
  - Datasette's (Simon Willison and Alex Garcia's) needs: rollback, read replicas, and composability with IAM prefix scoping. "data does not cross 'tenant' boundaries."
  - A prospective user was wary of the indirection, because "S3 is already proven to handle extremely high scale."
  - Zero-service new-user experience.
- **Acknowledged cost:** "the biggest drawback is that there won't be a metastore to perform client fast forwarding. This will make fetches after long periods of being offline a bit less optimal." The proposed fix was an optional stateless `graft-proxy` that offers a "virtual overlay of commits and segments". It was never built.
- **What was learned:** Graft is not a service. The commit protocol fits in a single conditional PUT, so the server added only auth surface and operational burden. Locality and rollup were the server's real value, and v2 gave them up. [I]

---

## 1. On-bucket layout (v2, as shipped) [V: `crates/graft/src/remote.rs`]

```
{prefix}/
  logs/{LogId}/commits/{CBE64-hex LSN}   -> Commit (bilrost-encoded)   IMMUTABLE, created with If-None-Match: *
  segments/{SegmentId}                    -> Segment (zstd frames)       IMMUTABLE, plain PUT (multipart writer)
```

- **There are only two object types.** There is no manifest, no mutable pointer, no tag or checkpoint file, and no lock object.
  - RFC 0001 planned `control`, `forks/`, and `checkpoints` (CAS-updated) objects. They were dropped.
  - Issue #277 "inline checkpoint management into the commit log" was closed.
- **LogId and SegmentId** are 16-byte GIDs: 1-byte type prefix, 48-bit ms timestamp, duplicate prefix, 64 random bits, encoded as 24-char base58 (`docs/.../internals/gid.md`).
- **SegmentIds are random**, not content-addressed. The planned next design makes them blake3 hashes.
- **LSN encoding:** `CBE64` = ones'-complement big-endian, written as 16 hex characters. Lexicographic order is therefore **descending** LSN, so a LIST or forward KV scan returns the newest entry first (RFC 0001). LSNs are u64 in `[1, 2^64)`, gap-free and monotonic, and 0 is a sentinel.
- **Commit message** (`crates/graft/src/core/commit.rs`):
  ```
  Commit { log: LogId, lsn: LSN, page_count: PageCount,
           commit_hash: Option<CommitHash>,      // always set on remote commits
           segment_idx: Option<SegmentIdx>,      // absent if only page_count changed (truncate/extend)
           checkpoints: Vec<LSN> }               // LSNs "upgraded into Checkpoint Commits"; contains own LSN if this commit is a checkpoint
  SegmentIdx { sid: SegmentId, pageset: Splinter (PageSet), frames: Vec<SegmentFrameIdx{frame_size: u64, last_pageidx}> }
  ```
  The whole page index for the pages a commit touched is **inline in the commit object**. A reader needs no other metadata object to find the segment, the frame, and the byte range.
- **Segment format** (`crates/graft/src/remote/segment.rs`):
  - A sequence of zstd frames. Pages are ordered by PageIdx, at most 64 pages per frame (256 KiB raw), zstd level 3, with the zstd content checksum on.
  - Byte offsets come from summing `frame_size` values in the commit's frame index. There is no footer or header.
  - A frame cannot be decoded without its commit. The grove plans complain about exactly this and propose per-frame checksums and a trailer.
- **CommitHash** (`core/commit_hash.rs`) = blake3 over the following, with the first byte replaced by `'C'`:
  - the magic bytes `68 A4 19 30`
  - LogId
  - CBE64(LSN)
  - volume page_count (u32 BE)
  - commit page count
  - (pageidx BE, 4096-byte page) for every page, in order
  It is deterministic in the logical content and position of the commit.
- **Page size** is fixed at 4 KiB. Per-volume page size is open in #258.
- **Encoding:** Commits use bilrost, a protobuf-like Rust format. Issue #296 (open) plans a switch to Protobuf or Fory so third-party tools can read the format.

### The exact commit point [V: `crates/graft/src/rt/action/remote_commit.rs`]
`RemoteCommit::run` does the following steps in order:
1. `attempt_recovery`. If the local Volume has a `pending_commit`, fetch the remote log and resolve it (see §3).
2. `plan_commit`. If the remote log has advanced past `sync.remote`, return `VolumeDiverged`. Otherwise target LSN = `sync.remote + 1`.
3. `build_segment`. Collect the newest version of every page across all local commits since `local_watermark`. This rolls N local commits up into one remote commit. Pages are also written into the local cache under the new SegmentId.
4. `put_segment`. PUT `segments/{sid}` (OpenDAL writer, 5-way concurrent multipart).
5. `remote_commit_prepare`. Persist `PendingCommit{local LSN, remote LSN, commit_hash}` in the local Volume record (Fjall).
6. `put_commit`. **PUT `logs/{log}/commits/{lsn}` with `if_not_exists: true`.** This single object creation is the commit point. Once it exists, the transaction is durable and visible to any client that fetches the log.
7. On success, `remote_commit_success`: write the remote commit locally and set `SyncPoint{remote: lsn, local_watermark: local}` atomically in one Fjall batch.

- A **checkpoint** is implicit. A commit whose segment contains every page (`segment_idx.page_count() == page_count`) lists its own LSN in `checkpoints`. No code rewrites old commits. The "re-fetch upgraded checkpoints" path in `FetchLog` is there for a future rewrite-in-place checkpointer. That checkpointer would make commit objects mutable, which contradicts "immutable". [V code, I intent]

---

## 2. Core concepts and how they relate [V: `docs/.../internals/index.md`, `glossary.md`, `concepts/volumes.md`, source]

- **Volume:** a sparse set of 4 KiB pages, with PageIdx starting at 1. Locally it is a record `{vid, local: LogId, remote: LogId, sync: Option<SyncPoint>, pending_commit: Option<PendingCommit>}` (`crates/graft/src/volume.rs`). Volumes and Tags (name → VolumeId) are **local only**. Nothing about a volume is stored remotely except its remote log.
- **Log:** an ordered list of commits under one LogId. Each volume has a **local log**, which exists only in Fjall and is never uploaded as such, and a **remote log**, which is the source of truth in object storage.
- **SyncPoint `{remote: LSN, local_watermark: Option<LSN>}`:** says that the volume is attached to remote LSN `remote`, and that local LSNs up to `local_watermark` are already contained in the remote.
- **Snapshot:** a list of `(LogId, LSN range)` layers plus a page count. The latest snapshot (`fjall_storage.rs::snapshot`) is:
  - `[local (watermark+1 ..= latest_local)] ++ [remote (1 ..= sync.remote)]` if there are unpushed local commits,
  - otherwise `[remote (1 ..= sync.remote)]`.
  - This two-layer composition is where most of the v2 bugs lived (§6).
- **Local storage = Fjall** (an LSM KV store) with these keyspaces: `tags`, `volumes`, `checkpoints`, `log` (LogRef → Commit), `page_versions` (LogId, PageIdx, LSN, stored in reverse order), and `pages` ((SegmentId, PageIdx) → Page, with KV separation). Pages are cached by segment, not by volume.
- **Push** = RemoteCommit (above). It squashes all local commits since the watermark into **one** remote commit, so the remote log does not keep local commit granularity.
- **Fetch** = download new commits of a log into local storage without applying them, like `git fetch`.
- **Pull** = fetch, then `recover_pending_commit` if needed, then `sync_remote_to_local`, which fast-forwards `SyncPoint.remote`. This works only when there are no local changes. Otherwise it returns `VolumeDiverged`.
- **Autosync** (`rt/task/autosync.rs`): every N seconds it fetches each volume's remote log, then pushes (if there are local changes) or pulls. If a volume has both local and remote changes, it does nothing ("volume has diverged and requires user/app intervention").
- **Splinter:** a compressed u32 bitmap (orbitinghail/splinter-rs). It is a 256-way tree over the big-endian bytes, with node classes tree, vec, bitmap, and run, and zero-copy `SplinterRef` reads. It is smaller than Roaring for small sparse sets. Graft uses it for every commit's page set.
- **Not implemented** (marked open in issues):
  - remote compaction (#260)
  - local GC (#259)
  - multi-process (#293)
  - a dedicated page store replacing Fjall (#294, "proven to cause significant overhead on both the read and write paths")
  - per-volume page size (#258)
  - a conflict-pages API (#297)
  - force push (#285)
  - formal verification (#59)

---

## 3. Concurrency, isolation, durability, ambiguous outcomes

### Claimed isolation [V: `docs/.../concepts/consistency.mdx`]
- The docs claim this: "Graft provides **Serializable Snapshot Isolation** globally... A write transaction must be based on the latest snapshot to commit. Assuming a compliant Graft client, this enforces Strict Serializable."
- Locally, a client that resets and replays experiences "Optimistic Snapshot Isolation": "a client may observe a snapshot which never exists in the global timeline." The docs give a worked example: a CHECK constraint fails on replay after a read had already shown balance 5.
- The rejected client has three options:
  - fork the volume
  - reset and replay
  - merge (not implemented; it would degrade the guarantee to SI)

### How conflicts are detected
- **Local conflicts (same process):** Detected at two points:
  - SQLite `RESERVED` lock acquisition checks `snapshot_is_latest`. If the check fails, the result is `SQLITE_BUSY_SNAPSHOT` (`crates/graft-sqlite/src/file/vol_file.rs`).
  - Local `commit` re-checks `is_latest_snapshot` under a global Fjall read-write mutex. If that fails, the result is `VolumeConcurrentWrite`, also mapped to `SQLITE_BUSY_SNAPSHOT`.
  Writers within one process are also serialized by a per-tag reserved mutex.
- **Remote conflicts:** There are two ways to lose:
  - The remote log already advanced before the push. `plan_commit` sees `latest_remote > sync.remote` and returns `VolumeDiverged`.
  - The race is lost at the PUT. The conditional PUT returns `ConditionNotMatch`, and recovery then detects the different hash.
- **What conflicts mean:** The unit of conflict is the **whole volume**. Any concurrent remote commit makes the client diverge, even when the page sets are disjoint. There is no page-level or row-level validation, no rebase, and no retry. Recovery is left to the application. The test harness handles divergence by opening a **new** volume on the remote log, which discards all unpushed local transactions (`crates/graft-test/src/bin/test_client.rs`). [V]
- **Multi-writer:** It is "supported" only in the sense that the CAS keeps the remote log linear. Throughput collapses under contention, and local work is thrown away. [I]

### Acknowledgement and durability modes [V source + fjall docs; I for consequences]
- A SQLite `COMMIT` returns when `VolumeWriter::commit` has written a Fjall batch, at `unlock(Shared)` in `vol_file.rs`. Nothing on the commit path touches object storage.
- Remote durability happens only through explicit `pragma graft_push`, `Runtime::volume_push`, or `autosync = N seconds`.
- There is no synchronous "commit means durable in S3" mode in v2.
  - The v1 talk said the client write was not acknowledged until the data was in object storage, with a 1 s flush. [S]
  - The v1 blog says "pushes block until the data is committed to object storage". That applies to the push, not the SQL COMMIT.
- **Graft never calls Fjall `persist`/fsync.** `grep -rn persist|fsync crates` finds nothing. Fjall's default is "write batches or transactions automatically flush data to the operating system", not fsync (docs.rs fjall `DatabaseBuilder::manual_journal_persist`, default false).
- **Consequences** [I]:
  - A locally acknowledged transaction survives a process crash.
  - It can be lost on power loss or kernel crash.
  - It is discarded on divergence.
  - The `PendingCommit` marker itself is not fsynced. If a power failure loses the marker after the remote PUT succeeded, the next push sees a remote commit it does not recognize and reports divergence. The data still exists remotely.
- Graft's injected "crash" fault is `std::process::exit(0)` (`crates/graft/src/fault.rs`). That simulates process death, not loss of the OS page cache. Antithesis node termination may cover that loss. [I]

### Reads and staleness [V]
- Readers see the local snapshot as of the last pull. There is no per-transaction freshness check against the remote. Strict serializability is a property of the remote commit order, not of reads.

### Ambiguous outcomes (timeout or retry on the conditional PUT) [V: `remote_commit.rs`, `fjall_storage.rs::recover_pending_commit`]
- The S3 operator stacks OpenDAL `RetryLayer::new()` (`remote.rs`). A conditional PUT whose first attempt actually landed but whose response was lost can be retried. The retry then gets `412 / ConditionNotMatch`.
- Graft does not interpret the 412 directly. It re-runs recovery, which works like this:
  - `PendingCommit{remote LSN, commit_hash}` was persisted **before** the PUT.
  - On 412, on any error, or on restart, it runs `FetchLog` on the remote and reads the commit at the pending LSN.
  - **Case 1:** the hash at that LSN equals the pending hash. "Someone (including us) pushed the same commit", so it marks success and advances the SyncPoint.
  - **Case 2:** the hash is different. It drops the pending commit and returns `VolumeDiverged`.
  - **Case 3:** there is no commit at that LSN. The push never landed, so it drops the pending commit and returns Ok, and the next push retries.
  - Code comment: "if any other error occurs, we leave the pending_commit in place and fail the job. this allows the `recover_pending_commit` job to run at a later point which will attempt to figure out if the commit was successful."
- **Why this works:** the content hash plus the immutable, never-deleted commit object together act as an idempotency key. A deterministic hash also means a *different* client that produced bit-identical content at the same LSN would be treated as our success. The states are identical, so this is benign. [I]
- **Precondition the method relies on:** LSN objects are never deleted or reused. Graft has no GC, so this holds trivially. SlateDB's GC boundary file exists precisely because GC breaks this precondition (see `research-architecture.md`). [I]
- The planned design adds client provenance (client id and watermark) to the commit for the same purpose (`plans/grove.md`, "Pushing").

---

## 4. Reads [V: `fjall_storage.rs`, `rt/runtime.rs`, `rt/action/*.rs`]

- **Locating a page for a snapshot:** `search_page` iterates the snapshot's layers. For each `(log, lsn range)` it does a reverse range scan on `page_versions` keyed `(log, pageidx, lsn)` to find the newest LSN ≤ the snapshot that touched the page. It then loads that commit's `SegmentIdx`. That is one local index probe per layer, at most two layers. This index (#276) replaced scanning the commit chain.
- **Cache check:** it looks up `pages[(sid, pageidx)]`. On a miss, `frame_for_pageidx` computes the byte range of the 64-page frame, and `FetchSegment` does **one range GET**. The frame is decompressed, and **all** pages in it are cached in Fjall.
- **Sparse pages:** If the page is not found in any layer, it returns an empty (zero) page.
- **Hydrate:** `HydrateSnapshot` finds missing frames for the whole snapshot (`find_missing_frames`), coalesces adjacent frames of the same segment into one range, and fetches with 5-way concurrency.
- **Prefetch:** `LeapOracle` (from the USENIX ATC'20 Leap paper) exists in `oracle.rs`, but v2's `read_page` does not call it. It was used in v1. [V by reading `runtime.rs::read_page`]
- **Cold start cost:**
  - `FetchLog` starts at `latest_local + 1` or LSN 1 and streams **every commit** of the log. The first request is one LSN, then chunks of 5 concurrent GETs. It stops at the first 404.
  - There is no LIST and no checkpoint-based truncation. `checkpoint_for` exists but is unused on the fetch path.
  - Cold open of a log with N commits therefore costs about N GETs in about N/5 round-trip batches, then one range GET per missing frame. [V code, I cost]
- **Finding the head:** done by probing GET `lsn+1` until 404. That is one GET per poll when up to date. [V]
- **Locality:** a pushed segment contains every page changed since the last push, sorted by PageIdx. Locality across commits is therefore poor until a full rewrite (checkpoint) happens. Compaction (#260) was meant to fix this. [I]
- **SQLite header tricks** (`vol_file.rs`):
  - Graft overwrites the file change counter (offset 24) with a hash of the snapshot, so SQLite's page cache invalidates correctly.
  - It ignores writes to page 1 that change only the change counter or the version-valid-for number (#35).
  - A related v1 bug ("stale SQLite page cache when independent clients had matching file change counters") is in the Antithesis blog post.
- **Performance numbers:** none are published in the docs, the README, the release notes, or the posts I read. The future-work page says "Currently Graft provides high-latency writes at low cost" and suggests S3 Express or a consensus buffer for lower latency, plus request hedging per the AnyBlob paper. [V]

---

## 5. GC, checkpointing, slow readers, zombies

- **GC is not implemented**, either remote or local (#259, #260 open). Orphan segments from failed pushes are never deleted. Commits are never deleted.
- **Planned (RFC 0001):**
  - Checkpoint = rewrite a commit to reference a segment containing all pages, and record it in a CAS-updated CheckpointSet.
  - GC would use a `checkpoint_ts` so "relevant checkpoints have lived 'long enough' to shadow truncated data. I.e. long enough that hopefully all the clients have picked them up."
  - That is a wall-clock grace period, not a reader registry.
  - Remote GC must checkpoint forks before truncating history they reference.
- **Planned (grove):**
  - Per-log manifest `/logs/{LogId}/manifest` updated by CAS. It holds checkpoints and the parent LogRef.
  - Checkpoint commits go to a separate path `/logs/{LogId}/checkpoints/{LSN}`, so commits stay immutable.
  - "GC will be a separate operation from checkpointing... The mark phase can add watermarks to the manifests, allowing for a sweep phase to happen later."
  - A later revision (grove3) moves to `/volumes/{name}` manifests with "Every write... must be conditional on the version that was read", plus image logs linked by provenance.
- **Zombie writers:** a zombie can only ever attempt `If-None-Match` at an LSN that someone else already took. It gets 412 and recovers into divergence. Because nothing is deleted, there is no ABA problem. [I]
- **Slow readers:** irrelevant today because nothing is deleted. [I]

---

## 6. Known correctness bugs and root causes

| When | Where found | Bug | Root cause | Source |
|---|---|---|---|---|
| 2025-01 (v1) | during Antithesis bring-up [I] | Two commits accepted on the same base LSN | MetaStore wrote commit with unconditional PUT | commit `281cf05` [V] |
| 2025 (v1) | Antithesis | Incorrect rollback from chained faults | Crash mid-commit, then a network timeout during recovery, reset `pending_sync`/`last_sync`, and the client believed recovery was done | sqlsync.dev/posts/antithesis-driven-testing [V] |
| 2025 (v1) | Antithesis | Segment corruption on abrupt termination | Client disconnect released internal locks early, and overlapping writes corrupted data. Seen in 64 of 149,784 timelines (~0.04%) | same post [V] |
| 2025 (v1) | Antithesis | Stale SQLite page cache | Independent clients with equal file change counters let SQLite reuse cached pages after sync | same post [V] |
| 2025-12-15 (v2) | Antithesis run (PR mentions Antithesis debuggability) | Snapshots "could erroneously pick up changes to the local log" | Off-by-one: the local layer started at `watermark` instead of `watermark+1`, which re-included an already-pushed commit | PR #279, commits `2514bab`, `bd82781` [V] |
| 2025-12-21 (v2) | Unit test, linked to #286 | Snapshot wrong after pull then local write | Same layering logic | PR #290 [V] |
| 2025-12-22 (v2) | **"Found by Antithesis test run 2025-12-22 07:15 UTC"** | `is_latest_snapshot` checked only the snapshot head, not the remote layer, so a writer could commit on a stale remote base (a lost-update risk) | The check compared only the top log, not the full layer set against the SyncPoint. The fix "creates a feature regression. Writers have a higher chance of failing with a concurrency error when executed alongside a concurrent volume push/pull" | PR #291, commit `d97fa5f` [V] |
| 2025-12-19 | Antithesis-era debugging | Checksum calculation bug | — | commit `7d1cafb` [V] |
| 2025-12 | External user running fio on a Graft block device | "BUG: missing page", "Invalid PageSet: Splinter contains PageIdx 0" | Probably the Splinter encoding bug below [I] | #286 [V] |
| 2026-03 | External user (Discord) | Commit decode fails for a dense page set 1..=7936 | **splinter-rs encode/decode round-trip bug** at a dense boundary, which depended on the construction path. Splinter has proptests (`proptest-regressions/`), but they missed it | #311, splinter-rs #83 [V] |
| 2026-07 | External reporter `logical-misha`, sequential core-API traces | (a) push between soft-truncate and regrow loses pages; (b) `SQLITE_BUSY_SNAPSHOT` leaks the reserved mutex, so the tag is write-blocked for the VFS lifetime; (c) fetch before first push panics on `assert_eq!(latest_remote, None)` | State-machine edge cases in local/remote layering and in the lock/unlock state machine. Each came with a minimal repro | #322, #323, #324 [V], closed "not planned" |

- **Carl's own diagnosis** in #295: "a recent bug discovered in the snapshot code means my solution for concurrent writes was fundamentally wrong anyway... if you try to read only from the local log: 1..=8 -> this is corrupt." This motivated the branch/staging-log redesign.
- **The pattern** [I]: the object-store commit protocol (one immutable object per LSN, plus hash-based recovery) produced **no** v2 bugs that I found. Every v2 correctness bug was in:
  - the composition of local and remote snapshots (two logs and a watermark), or
  - the SQLite lock state machine, or
  - truncation semantics, or
  - a dependency's encoder.
- **Other notable fixes:**
  - `6aa082a`: a global 60 s request timeout broke large segment uploads. It was replaced with `tcp_user_timeout`, which leaves no overall deadline on PUTs.
  - `e1cfb69`: "dont pull the volume when recovering from divergence".

---

## 7. The next design (unmerged `feat/grove` plans, Jan to Feb 2026) [V: `git show origin/feat/grove:plans/*.md`]

The plans are drafts and partly contradict each other. Key ideas:

- **"Grove: a forest of logs."** Branching is cheap via `branched_from: LogRef` in LSN 1 of a new log.
- **Three logs per writer: `local` (write) → `staged` → `origin/main`.** "we can squash outstanding changes from the local log into a staging log and attempt to push the staging log to the remote. This decouples local and remote writes from the push operation."
- **Provenance replaces merges.** `Commit.provenance: Option<LogRef>` asserts "the logical state of the Volume represented by this commit is exactly the same as the state represented by the provenance commit". It is used to canonicalize read paths (`c5==b2==a4`). "provenance is an optimization, not required for correctness."
- **Image commits** (full checkpoints) live in separate image logs, and reads terminate at the nearest reachable image.
- **Remote layout (grove3):** `/logs/{LogId}/{LSN bucket}/{LSN}`, with 4096 commits per bucket "to ensure that we don't run into filesystem/remote storage listing limitations". Also `/volumes/{name}` → VolumeManifest (CAS), and `/segments/{hash prefix}/{hash suffix}`, content-addressed by blake3.
- **Safety invariants written down:**
  - conditional manifest writes only
  - deterministic manifest merge (set union, sorted)
  - "All commits to a Log must be written via atomic exclusive create at `LSN(tip + 1)`"
  - "Shared Main Log Is Allowed... Correctness relies on atomic append semantics... not exclusive ownership of main."
- **Commit filters:** every 32 commits, write a Splinter or a `xorf::BinaryFuse8` over the PageIdxs touched, to skip 32 commits per probe. A filter may give false positives but never false negatives. Studied against git's commit-graph.
- **Local WAL modeled on SQLite's wal2:** two WAL files, a shared-memory WALState, seven locks, and fsync of the WAL at commit. This gives multi-process support.
- **Segments:** sparse local files, mmapped, with a presence bitmap and a per-frame checksum trailer.
- **"How do we validate correctness?"** (grove_airplane): considers an incremental whole-volume checksum per commit. It rejects the idea because truncate and sparsity make it costly, and settles on per-frame checksums stored in the SegmentIdx.
- **An abandoned idea:** fully content-addressed git-style DAG with version vectors (`plans/graft3.md`), dropped because pulling requires server-side set computation. Its section header reads "ABORT: Problems on problems".

---

## 8. Testing and verification

### 8.1 Antithesis harness (v2) [V: `tests/antithesis/**`, `antithesis.Dockerfile`, `justfile`, `.github/workflows/nightly.yml`]
- **Topology:** docker-compose with one `client` container (sleep infinity, runs commands) and **MinIO** as the S3 endpoint (`AWS_ENDPOINT_URL=http://minio:9000`). Real S3 or Tigris is never used under Antithesis.
- **Build:**
  - Rust with `-Cpasses=sancov-module ... -sanitizer-coverage-trace-pc-guard`, linked to `libvoidstar.so` (checked into the repo) for coverage guidance.
  - A debug profile build of the `test_client` binary only.
  - Symbols are linked into `/symbols`.
- **Test composer commands** (`tests/antithesis/workloads/v1/bank/`):
  - `first_setup.sh` → `test_client bank-setup`: 100,000 accounts × 1,000, then push. Faults are disabled during setup.
  - `parallel_driver_tx.sh` → `test_client bank-tx`. It runs many copies concurrently, each potentially a separate "client".
  - `anytime_enable_faults.sh` / `anytime_disable_faults.sh` toggle a `/faults-disabled` file. When present, the next driver runs with `--disable-faults`. Antithesis thereby explores timelines with and without in-process faults.
  - `anytime_validate.sh`, `eventually_validate.sh`, `finally_validate.sh` → `bank-validate`.
  - Antithesis semantics: `eventually_` kills other commands and stops all fault injection. `finally_` runs only when every command completed. [V: antithesis.com docs]
- **Client identity across process kills:**
  - Each invocation picks a random existing `clients/{name}` data dir it can lock with a non-blocking file lock, or creates a new one.
  - So a killed process's local Fjall state, including any `pending_commit`, is picked up by a later invocation. This tests crash recovery without a dedicated restart step. [V `test_client.rs`]
- **Workload `bank_tx`** (`crates/graft-test/src/workload.rs`):
  - It runs a random 1..=100 transfers. Each transfer reads two random accounts, moves min(50, balance) from the larger to the smaller, and inserts a `transfers` row with `cid` (client) and `pragma graft_snapshot`.
  - Inside the loop, `precept::sometimes_fault!` may call `volume_push` or `volume_pull` in the **middle of an open SQL transaction**.
  - It pushes at the end.
  - Errors are classified: `VolumeConcurrentWrite`, `NeedsRecovery`, `Diverged`, temporary or persistent object-store errors, `SQLITE_BUSY`, and `SQLITE_IOERR` are retried. Divergence triggers "reopen volume on remote and continue".
- **Properties checked:**
  - `bank_validate`: pull, hydrate, and checksum, then:
    - `expect_always_or_unreachable!(SUM(balance) == 100_000_000, "validate: bank is balanced")`
    - `"validate: account balances match transfers"`: every account equals initial − sent + received, from the `transfers` table. This checks that history and state agree, which catches lost or duplicated transactions, not only conservation.
    - `"validate: sqlite database is not corrupt"`: `PRAGMA integrity_check == ["ok"]`.
  - `"bank tx always pushes some valid txns"`.
  - `"volume is not diverged post recovery"`.
  - `expect_unreachable!("unhandled error in test client")` at the top level, so any non-retryable error is a failure.
  - Reachability (`expect_reachable!`, a "sometimes" property) for: `"volume diverged"`, `"pull_if_empty triggers"` and `"... doesn't trigger"`, and each recovery branch: `"recover pending commit: success" / "diverged" / "push failed"`.
- **In-process fault points** (`#[cfg(feature="precept")]` in `remote_commit.rs`):
  - `"RemoteCommit: before prepare"`, `"before commit"`, `"after commit"`, `"attempting recovery"` → `fault_crash()`, which calls `process::exit(0)` (or panics with `PowerlossPanic` in unit tests).
  - `"RemoteCommit: skipping segment cache"` → skips caching pushed pages locally, which forces a later remote read.
- **Platform faults** from the Antithesis blog post: network loss, latency, and partitions; node hangs, throttling, and termination; thread pausing; CPU modulation; clock jitter. The posts do not list a v2-specific configuration.
- **Cadence:** nightly at 07:00 UTC via `antithesishq/antithesis-trigger-action`, default `antithesis.duration=420` minutes. `just antithesis-run` defaults to 120. Reports are emailed. The tenant is `orbitinghail`.
- **How access was obtained** [V, blog]: the author was an Antithesis customer and on its Customer Advisory Board. The post does not describe an open-source arrangement. He now appears to be an Antithesis employee [I]. Antithesis also runs an OSS giveaway program (antithesis.com/blog/oss_pledge, applications open per that page) [S].
- **v1 harness** [V: `v0.1.5:crates/graft-test/src/workload/*.rs`]:
  - `SimpleWriter` mutates random pages in 1..=128 while maintaining an **index page at PageIdx 129 holding the hash of every page**. Every write first verifies that the page matches its recorded hash.
  - `SimpleReader` validates snapshots against the same tracker. It checks "snapshot never empty after non-empty" and "page should be empty as it has never been written to".
  - This is a self-validating data pattern: the data carries its own oracle.

### 8.2 precept [V: github.com/orbitinghail/precept, cloned at 0.4.1]
- **What it is:** "A testing utility for fuzzing and fault injection to discover erroneous and interesting states." It is modeled on the Antithesis Rust SDK and uses "expect" instead of "assert". About 1,600 lines.
- **Catalog:**
  - Every `expect_*!` call site defines a `static CatalogEntry` in a `linkme::distributed_slice`. It records the expectation kind, the property name, `Location::caller()`, the module, and the function.
  - At `init`, every entry is emitted as a "RegisterEntry" so the guidance system knows about assertions that **never fire**. This is what makes `Sometimes` and `Reachable` meaningful.
  - Each entry emits only on the **first** pass and first fail.
- **Expectation kinds:** `Always`, `AlwaysOrUnreachable`, `Sometimes`, `Reachable`, `Unreachable`.
- **Fault catalog:**
  - `sometimes_fault!(name, expr, details)` defines a `static FaultEntry{enabled: AtomicBool (default true), pending_trips: AtomicU32}` in another distributed slice. Duplicate names panic at init.
  - `trip()` returns true if a pending forced trip exists (decremented atomically). Otherwise, if the fault is enabled, it uses `dispatch::choose(&[true,false])`, a 50% chance from the **dispatcher's RNG**.
  - The API offers `enable_all`, `disable_all`, `get_fault_by_name`, and `set_pending(n)`.
- **Dispatchers:**
  - `AntithesisDispatch`: loads `libvoidstar` via dlopen, or else writes JSONL to the Antithesis output dir. Randomness comes from Antithesis, so faults are deterministic and replayable.
  - `TestDispatch`: panics on a failed `Always`, `AlwaysOrUnreachable`, or `Unreachable`. Randomness is `rand::random()`.
  - `NoopDispatch`.
- **`precept::random::rng()`** returns an RNG backed by the dispatcher, so workload choices are also guided and replayable under Antithesis.
- **Compile-out:** with `enabled` off, the catalogs are empty arrays. Since 0.4.1, macros still type-check their arguments but never evaluate them.
- **0.4.0 (2026-07-13):**
  - `observe!(|| ...)` property blocks constrained by an `Fn` closure bound, so "my property accidentally mutated the system" becomes a compile error.
  - `GhostState<T>` (auxiliary or reference-model state, mutable only via `GhostState::mutate`, readable only in `observe!`, zero-sized when disabled).
- **Outside Antithesis:**
  - Faults still work as random 50% trips, and `set_pending` gives deterministic, targeted fault tests. Graft's `crates/graft-test/tests/remote_commit_faults.rs` uses it, for example to force `"RemoteCommit: after commit"` once and then assert that recovery reuses the remote commit.
  - The **sometimes/reachable accounting is lost**: `TestDispatch` ignores `RegisterEntry` and never reports unreached `Sometimes`. Every fault point fires 50% of the time when enabled, with no guidance. A run is not reproducible from a seed unless you plug in your own seeded Dispatch.
- **Mirroring in Go** [I]:
  - **Registry:** a package-level `map[string]*Fault` populated in `init()` via `fault.Define("name")`, or `var fRemoteAfterCommit = fault.Define(...)` at package scope. The catalog is then known before any code runs, which is equivalent to linkme.
  - **Fault state:** each `Fault` has `enabled atomic.Bool` and `pending atomic.Int32`. `Trip()` consults a pluggable `Dispatcher` interface `{ Emit(Event); Random() uint64 }`.
  - **Compile-out:** guard with a build tag (`//go:build faults`) and a no-op twin file so production binaries carry nothing.
  - **Expectations:** register `expect.Always(cond, "name", details)` call sites lazily (keyed by name and caller via `runtime.Caller`). Pre-registration needs a generator or a `go:generate` scan of call sites. The Antithesis Go SDK ships `antithesis-go-generator` for exactly this, as far as I know (not verified this session).
  - **End-of-run report:** at the end of a test, fail if any `Sometimes` or `Reachable` property never held, and if any `Always` property was never reached. precept's TestDispatch does not do this. It is cheap to add and recovers most of the value outside Antithesis.
  - **Seeded RNG:** use a seeded `math/rand/v2` PCG dispatcher for reproducible local runs, and print the seed on failure.

### 8.3 Other test layers [V]
- **Unit tests:** inline `#[cfg(test)]` covering segment build and iteration, commit-hash golden values (table test with fixed expected hashes), frame lookup, Splinter, PageSet, GID, and LSN.
- **Integration tests** (`crates/graft-test/tests/`):
  - `runtime_sanity`: two runtimes on one in-memory remote, using `tokio` `start_paused(true)` so autosync time advances deterministically.
  - `sqlite.rs`: sync-and-reset divergence, export, and others.
  - `verify_snapshot.rs` and `verify_soft_truncate.rs`: regression tests added for the Antithesis bugs.
  - `workload_sanity.rs`: the bank workload end to end on a memory remote.
  - `remote_commit_faults.rs`: forced faults.
- **SQL golden tests:** `tests/sql/test_*.sql` with `.expected` output, run through the `sqlite3` CLI loading the extension (`tasks/sqlite/test`). There are three files: sanity, lock basics, and info pragmas.
- **Remotes used:** OpenDAL `Memory` (unit, integration, CI), `Fs`, and MinIO (Antithesis). CI (`.github/workflows/ci.yml`) runs fmt, build-all, nextest, doctests, SQL tests, and clippy. CI has **no** real S3 or Tigris test.
- **Absent:**
  - no property-based tests or fuzzing in Graft itself (no proptest, quickcheck, cargo-fuzz, or bolero)
  - no deterministic simulation (no turmoil, madsim, or loom)
  - no model-based testing against a reference model
  - SQLite's TCL test suite or sqllogictest is not run through the VFS
  - no formal spec: #59 "Formal verification of the Graft sync protocol" is open; Carl leaned to P or FizzBee "due to their alignment with distributed systems and usage in similar systems level projects (S3, SlateDB)"
  - `splinter-rs` has proptests, but the round-trip bug in §6 escaped them.

### 8.4 What found real bugs (summary)
- **Antithesis with the bank invariant workload and in-process crash faults:**
  - v1: three bugs in the blog post, plus the likely unconditional-PUT race.
  - v2: PR #291 explicitly, and PR #279 with high likelihood.
  - All were multi-step fault or concurrency interleavings: crash at commit, then timeout in recovery; abrupt disconnect; concurrent push/pull during a transaction.
- **External users and reviewers writing minimal sequential repros:** #286 and #311 (Splinter), and #322 to #324. These were **sequential, single-client** edge cases (truncate then push then regrow; fetch before first push; error-path lock release) that the Antithesis bank workload never exercised. It never truncates, never fetches without pulling, and retries `SQLITE_BUSY` instead of asserting liveness. A model-based or property-based test over the public API (random op sequences checked against an in-memory reference volume) would likely have caught all three. [I]

---

## 9. Lessons for a pure-Go engine with its own SQL layer

### Transfer directly [I unless marked]
1. **Use one immutable object per LSN as the only commit point** (`If-None-Match: *` on `log/{lsn}`), with all metadata needed to read that commit inline, including the page-set bitmap and frame index. There is no manifest on the hot path. A lost race is a 412, and there is nothing to roll back except an orphaned data object. [V design]
2. **Write data before the commit object.** Data objects get unique names, and orphans are harmless (to be GC'd later). [V]
3. **Resolve ambiguity with a persisted intent plus a deterministic content hash in the commit object.** Before the PUT, durably record `{target LSN, hash}`. On 412, timeout, or restart, read `log/{lsn}` and compare hashes. This distinguishes "I won earlier" from "someone else won" without any server. Requirements:
   - (a) the intent record must be **fsynced**, which Graft does not do;
   - (b) LSN keys must never be deleted and recreated, or a GC boundary is needed as in SlateDB;
   - (c) the hash should include a writer or txn id if you want to tell apart identical concurrent commits. [V mechanism, I requirements]
4. **Check the remote head by probing `GET log/{head+1}` until 404.** This is cheap: one GET when up to date. It gives a strongly consistent freshness check on S3, Tigris, or MinIO with no LIST. Use it at transaction start if reads must be strictly serializable. Graft does not do this. [V probe, I usage]
5. **Range-readable compressed frames** (zstd, ≤64 × 4 KiB, offsets from the commit's frame index) with a local page cache keyed by (segment, page). Add an explicit per-frame checksum. Graft relies on zstd's internal checksum, and its own plans call that "a bit dangerous". [V]
6. **Testing structure to copy:**
   - Name crash points around every remote step: before prepare, before PUT, after PUT, and during recovery.
   - Add a reachability property for **each recovery branch** (success, diverged, absent).
   - Write a bank workload whose transfers table lets the validator re-derive every balance from history, which catches lost or duplicated commits.
   - Run the engine's integrity check after every run.
   - Reuse client data dirs across killed processes.
   - Gate faults with a toggle so runs include fault-free periods.
   - Add `unreachable("unhandled error")` at the top level.
   - Use a deterministic virtual clock for background sync. In Go that is `testing/synctest` (Go 1.25+); Graft uses tokio's paused clock. [V for Graft's practice]
7. **Add what Graft lacked, since its escaped bugs came from there:**
   - A **model-based property test** over the public API: random sequences of {begin, read, write, truncate, commit, push, pull, fetch, crash/restart, inject 412 / timeout-after-success}, compared against a simple in-memory reference.
   - Hegel (Antithesis's PBT library) supports Go.
   - Round-trip fuzzing of every on-disk or on-bucket encoder with dense and boundary inputs (Splinter #83).
   - A fault-injecting object-store wrapper that can **apply a PUT and then return an error**. That is the exact ambiguous case, and Graft only covers it indirectly via OpenDAL retries plus process exit.

### Do not transfer [I]
1. **Acknowledging before object-store durability.** Graft's SQL `COMMIT` acks after a local, non-fsynced Fjall write. Remote push is separate, and divergence silently discards locally acked transactions. That contradicts ACID with object storage as the only durable store. Ack only after the commit object PUT returns 200, or after an ambiguous outcome has been resolved as "ours". Group commit can amortize latency.
2. **The local-log plus remote-log plus watermark layering.** It caused every v2 snapshot bug, and the author called it "fundamentally wrong". With object storage as the only truth, keep a single log namespace: a local cache of remote commits plus one in-memory pending transaction.
3. **Whole-volume conflict detection with abort-to-application.** It is acceptable for Graft's edge or offline niche but wrong for a multi-writer SQL engine. Either serialize writers through one leader per database, or validate read and write sets (pages or keys) against the commits that landed between snapshot and target LSN, then **retry automatically at the next LSN**. Graft's model does not prevent this: the commit objects already carry page sets, so validation needs only fetching the intervening commits.
4. **Fetching the log from LSN 1 on cold start.** You need checkpoints (image commits) discoverable without scanning, through a CAS manifest or a fixed-path pointer, before any GC. Graft never built this.
5. **Depending on an embedded LSM (Fjall) as the local cache.** The author says it caused "significant overhead on both the read and write paths" (#294) and blocks multi-process use (#293). A cache of immutable frames is better as plain files.

### Page-oriented (Graft) vs WAL + LSM (SlateDB) on object storage [I, using SlateDB facts from `research-architecture.md`]

| Dimension | Graft-style page log | SlateDB-style WAL + LSM |
|---|---|---|
| **Commit cost** | About 2 sequential PUTs (segment, then conditional commit). Payload is whole 4 KiB pages for every touched B-tree page (write amplification is roughly tree height × pages per txn), squashed per push. No manifest write. | 1 conditional WAL SST PUT per group-commit interval (default 100 ms) carries many txns; the manifest CAS happens only on L0 flush or compaction. Payload is only changed KVs. Cheaper per txn at high rates; latency is bounded by the flush interval. |
| **Commit protocol risk** | Minimal: one immutable object per LSN; conflict = 412; no fencing epochs. | More parts: WAL id fencing, writer and compactor epochs in the manifest, manifest CAS retries, and GC boundary files. |
| **Read cost (point lookup)** | Local page index → 1 range GET per cold frame. A B-tree descent needs about tree-height sequential round trips cold unless inner pages are cached or prefetched. Inner pages are hot, so warm reads are about 0–1 GET. | Bloom and index per SST; a cold read may probe L0 SSTs plus each sorted run. Unflushed WAL must be replayed by readers. Indexes and filters are cached. |
| **Read cost (scan)** | Good locality after a checkpoint or compaction; poor before it, because pages are scattered across many small push segments. | Sorted runs give good scan locality; compaction keeps it good. |
| **Snapshot / MVCC** | Free: a snapshot is (log, LSN). Old page versions stay addressable until GC. PITR and branching are natural. | Sequence-number MVCC; old versions disappear at compaction unless checkpoints pin them. |
| **GC complexity** | Needs image checkpoints, then deletion of commits and segments wholly shadowed by them. A segment is deletable only when no retained commit references it. Reader protection needs a time grace or a reader registry. Unimplemented in Graft. | Implemented in SlateDB: compaction rewrites, the manifest tracks live SSTs, checkpoints pin readers, and GC uses `min_age` and boundary files. More code, but done and documented. |
| **Correctness risk overall** | Low on the bucket side; risk shifts to page-level B-tree code (your own), checkpoint/GC (unbuilt), and any local layering. | Higher on the bucket side (more protocols), lower in the tree layer (LSM merges are simpler than B-tree page splits under crash). |

**Bottom line** [I]:
- For "correctness > reliability > speed" with your own SQL layer, Graft's **commit protocol** is the part worth copying almost verbatim. It is the simplest correct use of conditional writes I have seen, and it had zero bugs found in v2.
- Its **storage model** (4 KiB-page B-tree with copy-on-write pages per commit) is reasonable if you are willing to build checkpoints and GC.
- A WAL + LSM costs fewer PUTs per transaction and has a proven GC design (SlateDB), at the price of more coordination objects.
- A hybrid is possible: a Graft-style immutable per-LSN commit object holding *logical* changes (a WAL), with periodic image checkpoints of a page or SST structure. It keeps the single-object commit point and avoids page write amplification. This is not a design Graft or SlateDB use as such.

---

## 10. What I looked for and could not find
- **Talk transcripts:** SyncConf 2025, Vancouver Systems 2024, and High Performance SQLite 2024 are videos and not reviewed. I used only a third-party note of one 2025 talk [S].
- **Podcast:** the Database School episode (2025-04-18), with show notes only through mirrors. The transcript was not accessible.
- **Redesign blog post:** none found. The only primary sources for the redesign rationale are RFC 0001 and the v0.2.0 release note.
- **Performance numbers:** no published latency, throughput, or cost figures for v1 or v2.
- **Antithesis specifics:** no v2-specific fault configuration, run statistics, or full bug list. The April 2025 blog post covers v1 only. I could not confirm whether the v1 metastore PUT race was found by Antithesis.
- **Antithesis access:** no statement of terms, beyond "customer" and "Customer Advisory Board".
- **Other gaps:** no formal specification, no deterministic simulation, and no property-based or fuzz tests in Graft. The "next design" exists only as unmerged plan documents. There is no code and no date.

## Sources
- Repo: https://github.com/orbitinghail/graft
  - main @ `b07d931`
  - branch `feat/grove`, files `plans/graft3.md`, `plans/grove.md`, `plans/grove2.md`, `plans/grove3.md`, `plans/grove_airplane.md`, `plans/thoughts.md`
  - tag `v0.1.5` (v1 docs and RFC 0001)
- Code files:
  - `crates/graft/src/remote.rs`
  - `crates/graft/src/remote/segment.rs`
  - `crates/graft/src/core/commit.rs`
  - `crates/graft/src/core/commit_hash.rs`
  - `crates/graft/src/rt/action/{remote_commit,fetch_log,fetch_segment,hydrate_snapshot}.rs`
  - `crates/graft/src/local/fjall_storage.rs`
  - `crates/graft/src/volume.rs`
  - `crates/graft/src/rt/runtime.rs`
  - `crates/graft/src/rt/task/autosync.rs`
  - `crates/graft/src/fault.rs`
  - `crates/graft-sqlite/src/file/vol_file.rs`
  - `crates/graft-sqlite/src/vfs.rs`
  - `crates/graft-test/src/{workload.rs,bin/test_client.rs,lib.rs}`
  - `crates/graft-test/tests/*.rs`
  - `tests/antithesis/**`
  - `antithesis.Dockerfile`
  - `justfile`
  - `.github/workflows/{ci,nightly}.yml`
- Docs:
  - https://graft.rs/docs/internals/
  - https://graft.rs/docs/concepts/consistency/
  - https://graft.rs/docs/internals/future/
  - https://graft.rs/docs/rfcs/0001-direct-storage-architecture
- Issues and PRs: #59, #221, #259, #260, #267, #276, #277, #279, #286, #290, #291, #293, #294, #295, #296, #311, #322, #323, #324. Release v0.2.0. splinter-rs #83.
- precept: https://github.com/orbitinghail/precept at 0.4.1. Files `src/{fault,catalog,dispatch,random,macros,ghost}.rs`, `src/dispatch/{test,antithesis}.rs`, `CHANGELOG.md`.
- Blog posts:
  - https://sqlsync.dev/posts/stop-syncing-everything/ (Mar 2025)
  - https://sqlsync.dev/posts/antithesis-driven-testing/ (Apr 2025)
- Antithesis test composer reference: https://antithesis.com/docs/test_templates/test_composer_reference
- Fjall persistence default: https://docs.rs/fjall/3.0.0/fjall/struct.DatabaseBuilder.html
- [S] Talk notes: https://tinfoil-knight.github.io/notes/storing-small-things-in-big-places-2025
- [S] Podcast: https://share.transistor.fm/s/6dc15ef3
- [S] Antithesis OSS: https://antithesis.com/blog/oss_pledge/
