# Research notes: embedded ACID SQL engine with object storage as source of truth

Compiled 2026-11-10. Legend used throughout:

- **[V]** verified from a primary source (vendor docs, source code, RFCs, the speaker's own talk transcript). Quotes are verbatim where marked with quotation marks.
- **[S]** secondary source (third-party blog, forum, search summary). Treat with care.
- **[I]** my inference / synthesis, not stated by any source.

---

## 1. turbopuffer

Sources:
- Architecture: https://turbopuffer.com/docs/architecture
- Guarantees: https://turbopuffer.com/docs/guarantees
- Query (consistency param): https://turbopuffer.com/docs/query
- Write: https://turbopuffer.com/docs/write
- Limits: https://turbopuffer.com/docs/limits
- Launch blog (Jul 2024): https://turbopuffer.com/blog/turbopuffer
- Object-storage queue blog (Feb 2026): https://turbopuffer.com/blog/object-storage-queue
- FTS v2 postings blog (Jan 2026): https://turbopuffer.com/blog/fts-v2-postings
- CMU DB seminar talk w/ Andy Pavlo (Mar 2026): https://turbopuffer.com/blog/video-andy-pavlo-cmu
- Geek Narrator podcast (Nov 2024): https://turbopuffer.com/blog/podcast-database-from-first-principles

### Namespace layout
- [V] "Each namespace has its own prefix on object storage." Diagram: WAL at `s3://tpuf/{namespace_id}/wal` with files `01.bin, 02.bin, ...`; index data under `/{org_id}/{namespace}/index` (centroids.bin, clusters-N.bin, config). (architecture page)
- [V] Launch blog: "Each search namespace is simply a prefix on object storage." Any node can serve any namespace; requests are routed to the same node for cache locality.
- [V] Large namespaces are sharded into multiple internal indexes "that share the same WAL" (architecture page; write page: "A sharded namespace has a single write-ahead log").

### WAL, durability, group commit
- [V] "Every write adds a new file to the WAL directory inside the namespace's prefix. If a write returns successfully, data is guaranteed to be durably written to object storage. This means high write throughput (~10,000+ vectors/sec), at the cost of high write latency (p50=165ms for 500kB)."
- [V] "Each namespace can currently write 1 WAL entry per second. Concurrent writes to the same namespace are batched into the same entry. If a new batch is started within one second of the previous one, it will take up to 1 second to commit." Diagram labels: "mem buffer" -> "group commit (<= 1/s)" -> WAL; "CAS commit point"; "index cursor"; "~10ms for consistent read".
- [V] Object-storage-queue blog: group commit is "the same pattern" turbopuffer uses "for batching writes to the WAL"; it "decoupl[es] write rate from request rate." Replacing a file on object storage "can take up to 200ms", so serialized CAS on one object gives "~5 writes / second".
- [V] CMU talk (2026): "we write it directly to the write-ahead log on S3"; small writes are coalesced into a buffer then committed; "we commit a file and then commit it into a manifest with compare and swap." CAS described as "fetch an object, you get a tag back, and then you only write the object back to object storage if the tag is matching."
- [V] Podcast (Nov 2024): earlier/simplified description: WAL = prefix, "you put a file inside the WAL directory, and you just increment the number"; multi-writer safety via "a compare-and-swap kind of mechanism to make sure that you can only put it if it doesn't exist" (i.e. put-if-absent on WAL entry N+1); alternatives mentioned: single writer, ZooKeeper/etcd lock, UUIDv7 ordering.
- **Discrepancy [V]:** Podcast (2024) describes put-if-absent on sequentially numbered WAL files + LIST for freshness; CMU talk (2026) describes WAL file + manifest committed via If-Match CAS + conditional GET for freshness. Likely an evolution of the design (or the podcast simplified). Also the CMU talk says S3 got CAS "in December 2024"; actual AWS dates are Aug 20 2024 (If-None-Match) and Nov 25 2024 (If-Match) [I: from my knowledge of AWS announcements; AWS conditional-writes doc doesn't date it].
- [V] Write latency p50=165ms for 500kB (architecture). Podcast: "a write to object storage is generally around 100 milliseconds". Launch blog: "p90 to object storage is around 250ms for <1MB."

### Consistency
- [V] Strong by default: "if you perform a write, a subsequent query will immediately see the write."
- [V] How strong reads work: podcast: "every time a query comes in, you do a list call to S3 in the WAL directory before you do everything else ... Those list calls might take a couple of tens of milliseconds." CMU talk (newer): "round trip with a conditional GET (If-None-Match) to make sure that the caching node has the latest write" and "The list call is not one that I recommend ever using in production on a critical data path." Architecture page: "~10ms for consistent read"; "The roundtrip to object storage for consistency, which we can relax on request for eventually consistent sub 10ms queries."
- [V] Strong reads replay unindexed WAL on top of the index: "turbopuffer is strongly consistent and will replay the WAL entirely on top of the index read" (CMU).
- [V] Query param `consistency`: `{"level": "strong"}` (default) searches all unindexed writes; `{"level": "eventual"}` searches at most 128 MiB of unindexed writes, reuses node's cached view for up to `max_staleness_seconds` (default 60, max 3600).
- [V] Guarantees page: "Over 99.8% of queries return consistent data" (eventual mode); ~100ms staleness during scaling/failover; up to ~1 hour when >128 MiB outstanding writes.
- [V] CAP: favors consistency over availability when object storage is unreachable.
- [V] Isolation: no general read-write transactions. Conditional writes "behave as if run under Serializable isolation"; patch_by_filter/delete_by_filter run as Read Committed. Upserts are atomic ("All writes in an upsert are applied simultaneously").

### Indexing / compaction (async)
- [V] Committed WAL entries are indexed asynchronously; three states: indexed+committed, committed-unindexed, written-not-committed; index cursor tracks progress.
- [V] Storage engine is an LSM on object storage: "a bunch of metadata files, the WAL is growing, the LSM has a bunch of SSTables" (CMU); FTS blog: KV pairs "periodically compacted"; compaction "more or less the simplest tiered compaction that you can imagine" (CMU).
- [V] Indexing queue = single JSON file on object storage updated by CAS behind a stateless broker with group commit; "not part of the write path"; at-least-once; heartbeats for job ownership; the old broker learns it's deposed via a CAS failure.
- [V] Backpressure: HTTP 429 when unindexed data exceeds 2 GB or 1M rows (write page).
- [V] Limits: 10k writes/s @ 32 MB/s per namespace; 512 MB max write request.

### Cache hierarchy and latency
- [V] Memory cache + NVMe SSD cache on query nodes, object storage as source of truth. Cache is read-through and write-through; NVMe tier is "a FIFO ring buffer"; busy namespaces prioritized for warming (CMU).
- [V] Cold p50=874ms (1M docs), warm p50=14ms; "each roundtrip to object storage takes ~100ms. The 3-4 required roundtrips for a cold query often take as little as ~400ms." Launch blog p90: 1M 768-dim vectors cold 444ms / warm 10ms; BM25 1M docs cold 285ms / warm 18ms.
- [V] Design principle: minimize sequential roundtrips; planner trades extra roundtrips vs fetching more bytes per roundtrip; "a maximum of three roundtrips for sub-second cold latency."
- [V] Cost framing (launch blog): S3 ~$0.02/GB, SSD cache ~$0.1/GB, RAM $2+/GB.

---

## 2. SlateDB

Sources:
- Repo: https://github.com/slatedb/slatedb (latest release v0.18.1, 2026-11-07; v0.18.0 2026-11-02; v0.17.0 2026-09-29)
- RFC-0001 manifest: https://github.com/slatedb/slatedb/blob/main/rfcs/0001-manifest.md
- RFC-0008 synchronous commit: https://github.com/slatedb/slatedb/blob/main/rfcs/0008-synchronous-commit.md
- RFC-0009 separate WAL store: https://github.com/slatedb/slatedb/blob/main/rfcs/0009-separate-wal.md
- RFC-0011 transactions: https://github.com/slatedb/slatedb/blob/main/rfcs/0011-transaction.md
- RFC-0004 checkpoints, RFC-0026 GC boundary, RFC-0030 pluggable WAL (titles listed in repo `rfcs/`)
- Website docs source: https://github.com/slatedb/slatedb/tree/main/website/src/content/docs/docs (design/writes, files, gc, readers, checkpoints, consistency; operations/tuning; get-started/faq)
- Source: `slatedb/src/config.rs`, `slatedb/src/fence.rs`, `slatedb/src/manifest/store.rs`, `slatedb/src/wal/slatedb/store.rs`, `slatedb-txn-obj/src/object_store.rs`, `slatedb/src/transaction_manager.rs`
- Formal specs: `specs/fizzbee/{KeyValueStore,WalProtocol,SequencedMetadataBoundary}.fizz`
- Go binding: https://github.com/slatedb/slatedb/tree/main/bindings/go

### Layout [V] (design/files.mdx)
```
path/to/db/
  manifest/00000000000000000001.manifest ...   (FlatBuffers, full DB state each)
  wal/00000000000000000001.sst ...              (WAL written in SST format)
  compactions/000...1.compactions               (compactor state)
  compacted/<ULID>.sst                          (L0 + sorted runs)
  gc/manifest.boundary, compactions.boundary
```
- Manifest is updated by writer (new WAL/L0), readers (checkpoints), compactor (sorted runs). "The manifest with the highest ID is considered the current manifest" (RFC-0001).

### Commit / CAS mechanism [V from source]
- RFC-0001 (2024) designed a two-phase write + DynamoDB fallback because "S3 does not [support CAS], and we want to support S3" — **dated**; S3 now has conditional writes.
- Current code: manifest versions written with `PutMode::Create` (= `If-None-Match: *`) at next sequential ID (`slatedb-txn-obj/src/object_store.rs`), `AlreadyExists` -> `ObjectVersionExists` -> retry read-modify-write.
- WAL SST writes use `PutMode::Create`; `AlreadyExists` maps directly to `SlateDBError::Fenced` (`wal/slatedb/store.rs`). So: a zombie writer is fenced the first time it tries to write a WAL ID already claimed by the new writer.
- Epoch fencing: `writer_epoch` and `compactor_epoch` in manifest; new writer increments epoch via manifest CAS on startup (`FenceableManifest::init_writer`), then (RFC-0001) writes a fencing (empty) WAL SST at the next WAL id. Epoch is also stored in WAL SST metadata.
- GC boundary pitfall [V] (design/gc.mdx): with sequential put-if-absent naming, if GC deletes old IDs, a stalled process can later "create-if-absent ... reuse that ID and report a stale update as successful even though newer metadata has superseded it." SlateDB solves this with boundary files updated via `If-Match` (ETag CAS); writers check the boundary after creating a metadata file. Without If-Match support, `min_age` must exceed max lifetime of a stale process.

### Write path, durability, latency [V]
- `put()/write()/delete()` return a `WriteHandle` after the write reaches in-memory WAL buffer + memtable; `handle.await_durable()` or `db.flush()` waits for object storage (design/writes.mdx). Single batch-writer task assigns sequence numbers.
- Group commit: "multiple writes with `await_durable: true` will be batched into a single WAL write after `flush.interval` seconds or when the WAL buffer is full" (RFC-0008).
- Defaults (config.rs `impl Default for Settings`): `flush_interval` 100ms, `manifest_poll_interval` 1s, `manifest_update_timeout` 300s, `l0_sst_size_bytes` 64 MiB, `max_unflushed_bytes` 1 GiB, `l0_max_ssts` 8, `max_wal_flushes_before_l0_flush` 4096.
- Config doc comment: "A 100ms flush interval should result in $130/month in PUT costs on S3 standard"; "A 100ms flush interval will result in a 100ms + the time it takes to send the bytes to object storage."
- Tuning doc expected write latency: S3 Standard 50-100ms; S3 Express One Zone 5-10ms; GCS 50-100ms; ABS 50-100ms; MinIO 5-20ms. Prefix limits cited: 3,500 PUT/s, 5,500 GET/s.
- RFC-0009: "Even if `flush_interval` is set to an extremely low value, it can't overcome the inherent latency of the underlying object store" -> optional separate (low-latency) object store for WAL only (e.g. S3 Express).
- Visibility subtlety (RFC-0008): historically data written with `DurabilityLevel::Memory` was visible to readers before durable; RFC introduces committed vs persisted watermarks, `durability_filter` on reads. [I] For an ACID engine, ack-before-durable ("dirty" visibility) must be avoided or explicitly opted into.
- FAQ: why a separate WAL vs writing L0 directly: frequent small WAL flushes without exploding L0 SST count; WAL not used for reads, only recovery.

### Readers / snapshots / checkpoints [V]
- `DbReader`: read-only; opens a checkpoint on latest manifest (with lifetime, refreshed at half-life), replays newer WAL SSTs into immutable memtables (unless `skip_wal_replay`), polls manifest every `manifest_poll_interval`. Only returns data durable in object storage.
- Checkpoints pin a manifest (and its SSTs) against GC; clones build on checkpoints.
- Snapshots (in-process) pin state via sequence numbers.

### Transactions [V]
- `IsolationLevel::{Snapshot (default), SerializableSnapshot}` in `transaction_manager.rs`; SSI tracks read keys and read ranges (phantoms). Conflict checking in-memory against recently committed txns (Badger-style oracle). RFC-0011: "SlateDB is a single-writer database, and distributed transaction is not a goal."

### GC [V]
- Background GC per file type with `min_age` and interval; deletes files not referenced by any active manifest/checkpoint; WAL fence GC dry-run by default.

### Go availability / maturity [V]
- Official Go binding in-repo at `bindings/go` (module `slatedb.io/slatedb-go`, import `slatedb.io/slatedb-go/uniffi`), added 2026-03-19 (PR #1399). Generated by `uniffi-bindgen-go`; **requires cgo** and the `slatedb_uniffi` shared library on the loader path; starts a multi-threaded Tokio runtime in-process. API: DbBuilder, DbReaderBuilder, Db/DbReader/DbSnapshot, `Db.Begin(isolation)`.
- Pure-Go port `github.com/slatedb/slatedb-go` previously existed ("not yet ready for production use" per search summary [S]); `gh api repos/slatedb/slatedb-go` now returns 404 [V] — status unknown/moved.
- README: storage formats forward/backward compatible between adjacent versions, but "we do not currently guarantee API compatibility". Pre-1.0 (0.18.x). Lists Tigris among supported stores. Adopters listed include Dropbox, Prisma, Goldsky, Gadget, Responsive, ZeroFS (not stated as production).
- Open issue #1970 (Jul 2026): "Open can loop indefinitely in compactions-log load + compactor fencing CAS against a live previous compactor (20+ min observed)" — evidence that fencing/CAS edge cases are still being shaken out.

---

## 3. Commit protocols on object storage (other systems)

### Delta Lake [V] https://docs.delta.io/latest/delta-storage.html
- Log store requirements: atomic visibility, **mutual exclusion** ("Only one writer must be able to create (or rename) a file at the final destination"), consistent listing.
- Commit = create `_delta_log/<version>.json` exclusively (put-if-absent); losers re-read and run conflict detection, retry.
- Spark docs still say S3 single-cluster default; multi-cluster via `S3DynamoDBLogStore`; "Concurrent writes to the same Delta table on S3 storage from multiple Spark drivers can lead to data loss"; "only safe when all writers use this LogStore implementation as well as the same DynamoDB table". [S] delta-rs >= ~0.23 uses native S3 conditional writes by default (no DynamoDB). Pitfall: mixing writers with different locking mechanisms = data loss.

### Apache Iceberg [V] https://github.com/apache/iceberg/blob/main/format/spec.md
- "All changes to table state create a new metadata file and replace the old metadata with an atomic swap." "Writers create table metadata files optimistically ... commits by swapping the table's metadata file pointer from the base version to the new version." On conflict: "the writer must retry the update based on the new current version"; operations validate whether they can be re-applied (e.g. rewritten files still present).
- Atomic swap mechanism "is not standardized by this spec" (catalog: Hive/Glue/REST/JDBC; or rename for HDFS file-system tables). Sequence numbers reassigned on retry; manifests reusable across retries (inheritance) to make retries cheap.
- [I] Pitfalls widely known: commit contention under many concurrent writers (retry storms), metadata/manifest growth needing snapshot expiry + orphan-file cleanup; orphan cleanup must not delete files of in-flight commits (age threshold).

### WarpStream [V] https://docs.warpstream.com/warpstream/overview/architecture
- Stateless agents write data files directly to object storage; only metadata goes to WarpStream Cloud; metadata ops are "journaled to our strongly-consistent log storage system before being executed by a Virtual Cluster replica and acknowledged". Agents "make a few files per second"; background compaction of small files. Kafka operations within a virtual cluster are atomic across topics/partitions.
- [I] Lesson: data in object storage, but the commit/sequencing point lives in a separate low-latency strongly consistent metadata service, not object-store CAS.

### Neon (brief) [V] https://neon.com/docs/introduction/architecture-overview
- Commit = WAL quorum ack from safekeepers (Paxos); pageserver materializes pages; object storage holds long-term history and is "never accessed directly by the compute layer"; commit does not touch S3. [I] Neon explicitly avoids object storage on the commit path for latency.

### Litestream / LiteFS / SQLite VFS approaches
- [V] Litestream "Revamped" (2025) https://fly.io/blog/litestream-revamped/: LTX files = sorted page changesets, compacted like an LSM; "With conditional writes, we can implement a time-based lease" (single writer per replica destination, replaces LiteFS's Consul); FUSE (LiteFS) dropped as "a gross hack"; LiteVFS reads pages from S3.
- [S] v0.5.0 announcement https://fly.io/blog/litestream-v050-is-here/; release notes for v0.5.8 mention "distributed leasing with If-Match conditional writes". [S] Writable VFS (Feb 2026, per https://bex.co/blog/2026/08/07/litestream-writable-vfs-sqlite-s3-self-hosted-paas): writes buffered locally, synced ~every 1s => up to ~1s of acknowledged writes lost on crash ("eventual durability"); L0 uploaded every second, compacted to 30s/5min/1h levels.
- [S] Research issue https://github.com/djosh34/s3-smb/issues/573 (2026): Litestream "does not stop two writers and has no fence" (in the default replication mode); "A stale local DB at start becomes the newest state in S3"; open restore corruption bugs (#1164, #1220, #1309) cited.
- [V] Litestream issue #903 (Dec 2025): sets `X-Tigris-Consistent: true` for Tigris endpoints because "Without this header, Tigris may return eventually consistent results"; existing Tigris defaults: `SignPayload=true`, `RequireContentMD5=false`. Issue #1278: Tigris multipart upload defaults + request-canceled LIST flakiness found in soak tests.
- LiteFS: [S] beta, last release 2025-04; relies on Consul lease + FUSE; no S3 backend.

### Graft (orbitinghail) [V] https://github.com/orbitinghail/graft (docs/src/content/docs/docs/internals/index.md, concepts/consistency.mdx)
- Rust, SQLite VFS extension (libgraft). Page-based volumes. Local storage: Fjall LSM. Remote: `/segments/{SegmentId}` (zstd frames of up to 64 pages) and `/logs/{LogId}/commits/{LSN}` written "using a conditional write to detect conflicts"; also checkpoints.
- Push rolls up multiple local commits into one remote commit (dedupe to latest page version).
- Isolation: "Serializable Snapshot Isolation globally"; a commit must be based on the latest snapshot => strict serializable, but **any** concurrent commit to the same volume conflicts (whole-volume OCC). Default: commit locally then push asynchronously ("Optimistic Snapshot Isolation" locally — client may observe snapshots that never exist globally). Alpha quality [S]; graft-sqlite 0.2.1.

### Turbolite [V] https://github.com/russellromney/turbolite
- Rust SQLite VFS: pages grouped by B-tree type into page-group objects (default 256 pages, seekable zstd frames ~4 pages for range GETs); manifest maps page->object/offset; old groups never overwritten, `gc()` later. Durable mode uploads at checkpoint under SQLite EXCLUSIVE lock. Single writer; "Two machines writing to the same prefix will corrupt the manifest." Labeled experimental.
- Measured cold query latency, ~1.5GB DB, nothing cached: S3 Express (~4ms GET) vs Tigris (~25ms GET): point lookup+join 86 vs 172ms; 5-JOIN 251 vs 479ms; full scan 476 vs 532ms.

### Others [S]
- sqlite-s3vfs (Python): one S3 object per page, no locking -> concurrent writers corrupt.
- haqlite: S3 conditional PUT for leader election + WAL shipping; single writer w/ failover; experimental.
- Hudi lock provider on conditional writes (via https://alexr17.github.io/notes/the-sharp-edges-of-object-storage-conditional-writes.html).

### Known pitfalls (collected)
- [V] SDK retries turn your own win into a 412 ("412 usually means someone else won ... The client may have won it itself"); disabling SDK retries surfaces transient errors; lost responses require re-entering the protocol (alexr17 notes).
- [V] Timeouts are ambiguous; object stores "are not consensus systems"; use CAS as coarse batch-level coordination (cdouglas survey https://cdouglas.github.io/posts/2026/01/conditional).
- [V] Single-key CAS throughput (one 2KiB object, no retries, mid-2025): S3 14.7-15.3 op/s at 58-68ms; S3 Express 68.6-75.3 op/s at 14-26ms; GCS 0.8-1.4 op/s at 500-700ms (GCS documents 1 write/s/object); Azure Std 9-10 op/s; Premium 15-17 op/s. Under contention, successful writes invalidate all other in-flight CAS (S3X logged ~600 failed op/s).
- [V] Torn reads / truncated objects on interrupted streams observed in some stores (cdouglas) -> checksum every object.
- [V] Name reuse after GC with put-if-absent (SlateDB GC boundary).
- [V] Paused lease holder resumes believing it owns the lock -> protected resource must reject stale work (fencing tokens) (alexr17; SlateDB epochs).
- [V] Provider gaps: DigitalOcean Spaces lacks PutObject conditionals (alexr17); Garage has no conditional writes; MinIO has If-Match/If-None-Match on PUT only, not DELETE/COPY ([S] s3-smb #573).

---

## 4. S3 primitives (AWS)

Sources:
- Conditional writes: https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html
- Consistency model: https://docs.aws.amazon.com/AmazonS3/latest/userguide/Welcome.html#ConsistencyModel
- Conditional deletes (Sep 16 2025): https://aws.amazon.com/about-aws/whats-new/2025/09/amazon-s3-conditional-deletes-s3-general-purpose-buckets
- Performance: https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance.html

### Conditional writes [V]
- `If-None-Match: *` (only `*` accepted) and `If-Match: <etag>` on PutObject, CompleteMultipartUpload, CopyObject. Requires SigV4. If-Match needs s3:PutObject + s3:GetObject.
- If-None-Match: 200 if no object; 412 if exists. Versioned bucket: succeeds if no current version or current version is a delete marker; applies "only to the current version".
- "If multiple conditional writes or copies occur for the same object name, the first write operation to finish succeeds. Amazon S3 then fails subsequent writes with a 412."
- 409 Conflict possible "if a delete request to an object succeeds before a conditional write operation on that object completes"; PutObject may be retried after 409; CompleteMultipartUpload after 409 requires a brand-new multipart upload.
- If-Match: 412 on ETag mismatch; 409 possible for concurrent requests; 404 if concurrently deleted or current version is a delete marker.
- Multipart: "Conditional writes do not consider any in-progress multipart uploads"; condition is evaluated at CompleteMultipartUpload; if another client wrote the key meanwhile, Complete fails with 412.
- Bucket policies can enforce conditional writes (`s3:if-none-match`, `s3:if-match` condition keys).
- Conditional deletes (general purpose buckets, Sep 2025): `If-Match: <etag>` on DeleteObject/DeleteObjects -> 412 on mismatch; `If-Match: *` also allowed; enforceable with `s3:if-match`.
- [I] No multi-key atomicity; no conditional LIST; no "append" on general purpose buckets (S3 Express directory buckets support position-based append per cdouglas).

### Consistency [V]
- "strong read-after-write consistency for PUT and DELETE requests of objects ... in all AWS Regions"; "Any read (GET or LIST request) that is initiated following the receipt of a successful PUT response will return the data written"; LIST after write includes the object; LIST after delete excludes it.
- "Updates to a single key are atomic"; "There is no way to make atomic updates across keys."
- Concurrent unconditional PUTs: "the request with the latest timestamp wins" (last-writer-wins).
- Bucket configuration (e.g., enabling versioning) is eventually consistent (wait 15 min after enabling versioning).

### Performance / cost [V unless marked]
- At least 3,500 PUT/COPY/POST/DELETE and 5,500 GET/HEAD per second per partitioned prefix; scaling is gradual, 503 SlowDown while scaling.
- "consistent small object latencies (and first-byte-out latencies for larger objects) of roughly 100–200 milliseconds" (AWS guidance). Measured CAS PUT 58-68ms (cdouglas). SlateDB: 50-100ms writes on S3 Standard.
- S3 Express One Zone: "single-digit millisecond" access; single AZ; directory buckets; no versioning (RFC-0001 noted) .
- Request pricing S3 Standard (us-east-1): $0.005/1000 PUT/COPY/POST/LIST, $0.0004/1000 GET (quoted in SlateDB FAQ [V]; matches AWS pricing [I]). Storage ~$0.023/GB-mo.
- [I] Multipart basics (from AWS docs knowledge, not re-fetched): parts 5 MiB–5 GiB (last part may be smaller), max 10,000 parts, max object 5 TiB; incomplete uploads billed until aborted -> lifecycle rule `AbortIncompleteMultipartUpload` (recommended in conditional-writes doc [V]).
- Range GETs: supported everywhere (Tigris compat table lists "GetObject (range request)" ✅). [I] The cost is per request regardless of size, so small range GETs are cheap in bytes but not in requests.

---

## 5. Tigris (CRITICAL)

Sources:
- Conditionals (current): https://www.tigrisdata.com/docs/objects/conditionals/
- Conditionals (archived 2025-05-15): http://web.archive.org/web/20250515115918/https://www.tigrisdata.com/docs/objects/conditionals/
- Consistency (current): https://www.tigrisdata.com/docs/concepts/consistency/
- Consistency (archived 2025-04-30; URL now redirects to bucket locations): http://web.archive.org/web/20250430163009/https://www.tigrisdata.com/docs/objects/consistency/
- Bucket locations: https://www.tigrisdata.com/docs/buckets/locations/
- Architecture: https://www.tigrisdata.com/docs/concepts/architecture/
- S3 API compatibility: https://www.tigrisdata.com/docs/api/s3/
- Changelog: https://www.tigrisdata.com/docs/changelog/
- Multi/dual-region blog (Mar 24 2026): https://www.tigrisdata.com/blog/multi-region-dual-region-buckets/
- Go SDK headers: https://github.com/tigrisdata/storage-go/blob/main/tigrisheaders/tigrisheaders.go (pkg.go.dev v0.8.0, Aug 2026)
- Fly community threads: https://community.fly.io/t/tigris-consistency-questions/24046 , https://community.fly.io/t/please-define-the-delete-consistency-behaviour/24227
- Pricing: https://www.tigrisdata.com/pricing/
- Benchmarks: https://www.tigrisdata.com/docs/overview/benchmarks/

### Conditional PUTs — supported [V]
Current doc, verbatim:
- "`If-Match` | Request proceeds only if the object's ETag matches the provided value. Returns `412 Precondition Failed` otherwise."
- "`If-None-Match` | Request proceeds only if the object's ETag does not match. Use `If-None-Match: "*"` to write only if the object does not already exist. Returns `304 Not Modified` on a matching GET, or `412 Precondition Failed` on a matching PUT."
- Also If-Modified-Since / If-Unmodified-Since, and `X-Amz-Copy-Source-If-*` for CopyObject. Multiple conditions: all must hold.
- Use cases listed: create-if-not-exists (`If-None-Match: "*"`), compare-and-swap (`If-Match: "etag-from-previous-read"`), conditional GET (`If-None-Match: <etag>` -> 304).
- Not stated in current docs: behaviour of conditionals on CompleteMultipartUpload, 409 semantics on concurrent delete, conditional DELETE. The S3 compat table does not list conditional PutObject as a tested row (it lists GetObject If-* variants only). [I] => test these explicitly.

### Consistency is a function of bucket location type [V]
Current consistency doc, verbatim table:

| Location Type | Same-region | Cross-region | How it works |
|---|---|---|---|
| Global | Strong | Eventual | "Cross-region reads may serve a cached or on-demand copy that is eventually consistent." |
| Multi-region | Strong | Strong | "Any region can serve a strongly consistent read." |
| Dual-region | Strong | Eventual | "Reads from within the same region as the data are strongly consistent. Reads from outside are eventually consistent." |
| Single-region | Strong | Strong | "All requests, regardless of origin, are routed to one region, providing strong consistency globally." |

- Strong consistency includes: read-after-write, read-after-delete, read-after-update, **"List-after-write: A LIST immediately after a PUT includes the new object."**, and "Conditional operations: Compare-and-set (If-Match, If-None-Match) and other conditional operations always evaluate against the latest state."
- When eventual: "there is a brief window where a read may return a previous version of the object. The replication lag is typically sub-second."
- Conditionals doc, verbatim: "Conditional operations always evaluate against the latest state of the object within the consistency model defined by your bucket's location type." "Multi-region and Single-region buckets provide strong consistency globally. Conditional operations are guaranteed to evaluate against the most recent write, regardless of which region the request originates from." "Global and Dual-region buckets provide strong consistency within the same region and eventual consistency globally. For conditional operations that may span regions, use a Multi-region or Single-region bucket to ensure conditions are always evaluated against the latest state." Note: "If your workload requires conditional writes from multiple regions on the same objects, we recommend using a Multi-region or Single-region bucket ... without any additional configuration."
- **Global is the default location type** ("If you create a bucket without specifying a location type, it will be Global"). Global: "Single copy, distributed globally based on access patterns"; "Metadata pushed to all regions. Data pulled on demand and cached locally"; uploads stored "in the region nearest to the upload origin".
- Location set at creation via `LocationConstraint`: omit/`auto` = Global; `usa`/`eur` = Multi-region (USA candidates sjc, ord, iad; EUR ams, fra); `iad,sjc` = Dual-region; `iad` = Single-region. Multi/Dual-region launched 2026-03-11 (changelog) / blog 2026-03-24.

### X-Tigris-Consistent header — history and current status
- [V] Archived conditionals doc (May 2025): "Attach the `X-Tigris-Consistent:true` header in your conditional request to ensure Tigris performs the conditional operation on the leader. The same applies to reads, to ensure you get consistent read results." and "Don't forget to attach `X-Tigris-Consistent:true` header as that ensures the conditions are evaluated and conditional operation will be performed on leader."
- [V] Archived consistency doc (Apr 2025): "By default, Tigris offers strict read-after-write consistency within the same region and eventual consistency globally." Bucket-level global strong consistency option, or per request `X-Tigris-Consistent:true` — "include the `X-Tigris-Consistent:true` header in every request that requires global strong consistency ... Similarly, include the header in any subsequent operations, such as get or delete". "global strong consistency is achieved by routing all operations through a single leader"; leader region controllable via `X-Tigris-Regions` header or bucket region.
- [V] Changelog 2025-02-15: "Buckets can be created in strict consistency mode."
- [V] Fly community (Tigris staff "himank", 2025-03-20): put is "applied synchronously to the first region", other regions asynchronously; older `X-Tigris-CAS` header deprecated but still works; `X-Tigris-Consistent` routes reads to the leader that handled the write; a single header covers PUT/GET/LIST/DELETE.
- [V] Go SDK (storage-go, current main): `WithConsistentRead()` sets `X-Tigris-Consistent: true`; `WithCompareAndSwap()` sets `X-Tigris-CAS: true` ("skip the cache and read the object from its designated region ... only used on GET"); **`WithCreateObjectIfNotExists()` sets `If-Match: ""`** (Tigris-specific legacy create-only, per the 2025 doc: "`If-Match: ""` - (empty etag) creates only if object not exists"; "`If-None-Match: ""` - (empty etag) replace only if object exists"). These empty-ETag forms are non-standard relative to AWS.
- [V] Current (Nov 2026) docs no longer mention `X-Tigris-Consistent` anywhere I fetched (conditionals, consistency, locations, S3 API); the old `/docs/objects/consistency` URL redirects to bucket locations. **Unresolved [I]:** whether the header is still honored for Global/Dual-region buckets, or superseded by location types. Litestream (Dec 2025) still sends it for all Tigris requests.

### Architecture relevant to CAS [V]
- Metadata in FoundationDB ("multi-key strictly serializable transactions"); designed "to be transactional so that we can provide strong consistency guarantees and powerful semantics such as Compare-And-Set"; multiple FDB clusters replicated via a Tigris-built replication service (QuiCK-style queue). Small objects inlined in metadata records. Caching layer per region, cache-on-read default; cache invalidation on writes via the distribution framework.
- [I] Cross-region eventual consistency in Global buckets stems from asynchronous FDB-cluster-to-cluster replication; a conditional PUT evaluated in region B may not see a write committed in region A within the lag window -> two writers in different regions could both "win" put-if-absent on the same key. This is exactly what the doc warns about.

### Other deviations / notes
- [V] S3 compat table: 61/68 tests pass; PutBucketVersioning ❌ (Tigris has bucket snapshots/forks instead); GetBucketPolicy/PutBucketPolicy ❌ (=> cannot use bucket policy to enforce conditional writes); SSE headers accepted but ignored; only canned ACLs.
- [V] Litestream needed Tigris-specific client settings: `SignPayload=true`, `RequireContentMD5=false`; also hit Tigris multipart default and request-canceled LIST flakiness in soak tests (issues #903, #1278).
- [V] Pricing: Standard $0.02/GB-mo; Class A $0.005/1000; Class B $0.0005/1000; DELETE free; egress free.
- [V] Vendor benchmark: "Sub-10ms read latency", "Sub-20ms write latency" (small objects; "P90 latencies under 17 ms" for writes, Mar 2025 changelog). Independent [V via turbolite README]: ~25ms GET on Tigris vs ~4ms on S3 Express.
- [V] Tigris supports bucket snapshots and forks (changelog; SDK `WithEnableSnapshot`, `WithTakeSnapshot`, `WithSnapshotVersion` for ListObjectsV2/GetObject/HeadObject).

### Practical implications for a CAS commit protocol on Tigris [I]
1. Create the bucket as **Single-region** (or Multi-region) explicitly; do not rely on the Global default for any key that is CAS'ed.
2. Use standard `If-None-Match: *` / `If-Match: <etag>`; optionally also send `X-Tigris-Consistent: true` (harmless if ignored; required in older semantics).
3. Write a conformance test suite against the real endpoint: concurrent put-if-absent races, If-Match races, CAS after delete, CompleteMultipartUpload with conditions, LIST-after-PUT, 409/412 handling, ETag stability across multipart vs single PUT.
4. Avoid relying on bucket policies or versioning.

---

## 6. Design implications (facts + trade-offs)

### Single writer with fencing vs multi-writer OCC
- Single writer + epoch fencing (SlateDB, Litestream lease, turbolite/haqlite, turbopuffer-per-namespace node routing): commit point = put-if-absent of next sequential log object; a newer writer claims an epoch via manifest CAS and writes at the next log ID so a zombie's next put-if-absent fails [V SlateDB source]. Conflict detection is in memory => full SSI is cheap [V SlateDB, Badger]. Throughput limited by group-commit cadence, not CAS contention.
- Multi-writer OCC (Delta, Iceberg, Graft): each writer races put-if-absent on log N+1 (or If-Match on a pointer); losers must read the winners' entries and validate (or abort). Contention ceiling per log/pointer is roughly 1/RTT: ~15 op/s on S3, ~1/s on GCS [V cdouglas]. Graft aborts on any concurrent commit (whole-volume), giving strict serializability at low concurrency [V]. Iceberg/Delta re-apply if validation passes [V].
- Lease pitfalls: clock-based leases need fencing at the storage resource, not just the lease [V alexr17, SlateDB].

### Isolation achievable [I, grounded in V facts]
- Single-writer process: Serializable (SSI with read/write set + range tracking, as SlateDB does) or even strict serial execution of write txns; readers get snapshot reads from immutable objects.
- Multi-process writers via object-store OCC: Snapshot Isolation via write-set validation against intervening log entries; Serializable requires validating read sets/predicates against intervening commits (needs per-commit key/range summaries in log entries) or Graft-style "abort if anything committed since my snapshot".
- Strict serializability for readers requires a freshness round trip per transaction (turbopuffer pays ~10ms+ per strong read; eventual mode skips it) [V].

### Commit latency floor
- One durable round trip minimum if the log append IS the commit (SlateDB WAL put-if-absent) [V]. Two sequential round trips if data object + manifest CAS (turbopuffer CMU description) [V].
- Measured/claimed single-PUT latencies: S3 Standard 50-100ms (SlateDB), 58-68ms CAS (cdouglas), p50 165ms for 500kB (turbopuffer), AWS guidance 100-200ms; S3 Express 5-10ms / 14-26ms CAS; Tigris vendor sub-20ms writes (p90 <17ms small objects), independent ~25ms GET. Plus group-commit wait (SlateDB default 100ms flush interval; turbopuffer up to 1s).
- Cost floor: one PUT per group commit: at 10 commits/s on S3/Tigris Class A ≈ $0.005/1000 × 864,000/day ≈ $4.32/day ≈ $130/month (matches SlateDB's "$130/month" at 100ms) [V+I arithmetic].

### Storage layout options
- WAL + LSM/SSTs (SlateDB, turbopuffer, Litestream LTX ≈ LSM): immutable objects, batched writes, compaction rewrites; reads need block cache + bloom filters; recent WAL can be replayed for strong reads. Good fit for "PUT is expensive, objects immutable" [V].
- Page-based COW (Graft segments + page index; turbolite page groups + manifest; sqlite-s3vfs page-per-object): maps naturally to SQLite B-tree pages; each commit writes a segment of changed pages + a commit/manifest record; needs a page->object index and GC of superseded segments; page-per-object is too many requests and has no atomicity [V/S]. [I] A COW B-tree rewriting root-to-leaf paths per commit amplifies writes; batching all dirty pages of a group commit into one segment object amortizes it.
- Cold read latency is dominated by sequential round trips; designs cap round trips (turbopuffer: ≤3 for vector queries) and prefetch (turbolite) [V].

### GC
- Reference-based GC from active manifests/checkpoints with `min_age` grace (SlateDB) [V]; must handle name reuse with put-if-absent (boundary objects with If-Match) [V]; orphaned objects from failed/aborted commits require age-based sweeping [V Iceberg/SlateDB]; incomplete multipart uploads need lifecycle abort [V AWS]. Tigris DELETE requests are free [V].

### How readers find a consistent snapshot cheaply
- Options observed: (a) LIST the log prefix (turbopuffer 2024: tens of ms; advised against on critical path in 2026); (b) read the latest manifest then list/probe the WAL for contiguous entries (SlateDB RFC-0001); (c) conditional GET with `If-None-Match: <cached etag>` on a manifest/pointer object -> 304 if unchanged (turbopuffer CMU); (d) poll manifest periodically (SlateDB DbReader, 1s default) for bounded staleness; (e) pin a checkpoint so GC can't delete the snapshot's files (SlateDB) [V].
- [I] Probing `log/<N+1>` with HEAD/GET until 404 is an alternative to LIST that relies on read-after-write consistency for new keys; on Tigris only safe where consistency is strong (Single/Multi-region or same-region).
