# SlateDB deep dive: design, verification, lessons for a Go SQL engine on S3

Date: 2026-11-10. Repo state read: `slatedb/slatedb` main @ `0b705dc8` (2026-11-07, = v0.18.1).
Local clone: `scratchpad/slatedb-src/slatedb`. Paths below are repo-relative.
Marking: [V] verified from a primary source I read (file/RFC/issue/PR text), [I] my inference,
[S] from a search snippet or secondary summary only.

Complements `research-architecture.md` (does not repeat its SlateDB basics).

---

## Part 1 - Design

### 1.1 On-bucket layout and mutability [V]

Source: `website/src/content/docs/docs/design/files.mdx`, `slatedb-txn-obj/src/object_store.rs`,
`slatedb/src/wal/slatedb/store.rs`, `schemas/*.fbs`.

| Object | Name | Written how | Mutable? |
|---|---|---|---|
| Manifest | `manifest/<u64 zero-padded 20>.manifest` | `PutMode::Create` (If-None-Match:*) at `latest+1` | Immutable; whole-DB snapshot per version (FlatBuffers `ManifestV2`) |
| Compactor state | `compactions/<u64>.compactions` | `PutMode::Create` at `latest+1` | Immutable per version |
| WAL SST | `wal/<u64>.sst` | `PutMode::Create` | Immutable; zero-byte object = writer **fence** |
| L0 + sorted-run SSTs | `compacted/<ULID>.sst` | plain PUT / multipart (unique ULID name) | Immutable |
| GC boundary | `gc/manifest.boundary`, `gc/compactions.boundary` | Create, then `PutMode::Update(etag)` (If-Match) | **The only mutable objects**; ASCII u64, monotonic, never deleted |

- Only the GC boundary files are overwritten in place. Everything else is write-once. [V]
- Every conditional PUT carries a ULID in user metadata (`slatedbputid`) so a retry can tell its
  own success from a competitor's (see 1.5). [V] `slatedb/src/retrying_object_store.rs`.
- SST blocks carry checksums; WAL/SST open uses `read_with_validation_retry`. [V]
  (`format/sst.rs` `CHECKSUM_SIZE`; `wal/slatedb/store.rs`).
- Default SST block size 4096 B (`format/sst.rs:731`). [V]

#### Exact commit points [V]
- **Normal write (WAL enabled):** creation of `wal/<N>.sst` with If-None-Match. The manifest is
  NOT touched per write. `await_durable()` resolves when that PUT succeeds
  (`wal/slatedb/store.rs::write_create`; `AlreadyExists` -> `SlateDBError::Fenced`).
- **WAL disabled (`wal_disable` feature):** L0 SST upload + manifest version create.
- **Metadata change (L0 flush, compaction result, checkpoint, epoch bump):** creation of
  `manifest/<N+1>.manifest`, then a boundary check that `N+1 > gc/manifest.boundary`. A create
  that lands at/below the boundary is treated as failed (`slatedb-txn-obj/src/lib.rs`
  `SequencedStorageProtocol::write`). RFC-0026 states: "The filename is the commit point".
- **Visibility to other processes:** a `DbReader` sees data only once it is durable (WAL SST or
  manifest) and only after its next poll (`DbReaderOptions::manifest_poll_interval` default
  **10 s**; writer `Settings::manifest_poll_interval` default 1 s). [V] `config.rs`.
- **Visibility inside the writer process:** immediately after the in-memory commit, before
  durability. `ReadOptions::durability_filter` defaults to `DurabilityLevel::Memory`; `Remote`
  restricts reads to durable data. `ReadOptions::dirty=false` hides seqs above
  `last_committed_seq`. [V] `config.rs:256-329`.

### 1.2 Manifest contents and update protocol [V]

`schemas/manifest.fbs` `ManifestV2` fields: `manifest_id`, `external_dbs` (clone parents with
`source_checkpoint_id`, `sst_ids`), `initialized` (clone 2-phase flag), `writer_epoch`,
`compactor_epoch`, `replay_after_wal_id` (WALs <= this are in L0; replay starts at +1),
`wal_id_last_seen`, `last_compacted_l0_sst_view_id`, `ssts` (physical SST table), `l0` (views),
`compacted` (sorted runs of views), `last_l0_clock_tick` (seeds monotonic clock on restart),
`checkpoints` (id, manifest_id, create/expire time, name, optional writer epoch), `last_l0_seq`,
`recent_snapshot_min_seq` (min seq any in-memory snapshot/txn still needs; compactor must keep
versions >= this), `sequence_tracker` (seq<->time map, RFC-0012), `segments` (RFC-0024).

Update protocol (`slatedb-txn-obj/src/lib.rs`):
1. Hold an in-memory copy of version N (`SimpleTransactionalObject`).
2. `prepare_dirty` -> mutate -> `update`: evaluate registered **invariants** on the dirty value
   (`manifest/invariants.rs`, e.g. RFC-0029 L0 ULID cutoff), then `PutMode::Create` at N+1, then
   boundary `check(N+1)`.
3. On `ObjectVersionExists` (lost race, or behind boundary): `refresh()` and re-run the mutator
   (`maybe_apply_update` loops forever). Writer, compactor, readers (checkpoints), admin and GC
   all write the same manifest namespace this way; each change is re-applied as a function of the
   latest state, so they merge rather than conflict.
4. `FenceableTransactionalObject` wraps this: `check_epoch()` before every `prepare_dirty`,
   `update`, and after every `refresh`; stored epoch > local -> `Fenced`; stored < local -> panic.

Reading latest (RFC-0032, implemented in `object_store.rs::try_read_latest_unchecked`): cache
(id, bytes) of the newest seen version; probe GET N+1.. up to `MAX_PROBES` (4); first 404 means
"latest" because IDs are consecutive; otherwise LIST and take max. LIST/GET race with GC handled
by retry (issue #1215). Motivation quoted: an idle DB did "560 LIST requests" in 5 min, "$24.19
per-month"; goal "<$5 per month" idle.

Boundary reads use a cached value plus conditional GET (If-None-Match on the boundary ETag).
RFC-0026: "The boundary read must not be served from a stale object cache. Manifest and
compactions stores must not use `CachedObjectStore`." A boundary file that disappears after being
observed -> panic.

### 1.3 Who coordinates through the manifest [V]

- **Writer** (`Db`): bumps `writer_epoch` on open; L0 flushes update `l0`, `replay_after_wal_id`,
  `last_l0_seq`, `recent_snapshot_min_seq`; polls the manifest every 1 s to learn compaction
  results and fencing.
- **Compactor** (embedded by default, or standalone `Compactor`, or RFC-0025 coordinator +
  stateless workers): bumps `compactor_epoch` (manifest first, then `.compactions`), commits
  results to the manifest. Only the coordinator writes the manifest; workers only write
  `.compactions`.
- **Readers** (`DbReader`, `ManagedCheckpoint` mode default): create a checkpoint in the manifest
  (lifetime default 10 min, refreshed at half-life), delete it on close. So readers are manifest
  writers too. `FollowLatest` mode writes nothing and has no GC protection; `Checkpoint(id)` pins.
- **GC** (embedded by default or standalone): removes expired checkpoints from the manifest,
  advances boundaries, deletes files.
- **Admin**: detached checkpoints, clones, submit compactions; wraps its store in
  `RetryingObjectStore` because "an ambiguous failure after a successful write is detected rather
  than surfaced as a spurious error" (`admin.rs:775`).

### 1.4 Checkpoints, clones, snapshots [V]

`design/checkpoints.mdx`, `design/clones.mdx`, RFC-0004.
- Checkpoint = manifest entry pinning a manifest_id; GC keeps that manifest and all its SSTs.
  `CheckpointScope::All` flushes WAL+memtable first; `Durable` does not.
- `begin_checkpoint` returns a handle; "If `wait()` reports an unknown outcome, the checkpoint can
  still exist" -> documented recipe for recording a permanent checkpoint in another system:
  create with finite lifetime, wait, record id externally, then `refresh_checkpoint(id, None)`;
  "The two systems do not update together." (explicit ambiguous-outcome protocol).
- Compactor writes a temporary checkpoint (default 15 min, `CompactorOptions::checkpoint_lifetime`)
  before publishing a manifest that drops SST references; reads running longer can fail with a
  data error when GC removes the SST. Writer-side long scans have no own checkpoint: issue #319
  "Maintain a checkpoint from the writer client" is still OPEN, and the bank DST disables
  compacted-SST GC because of it (`slatedb-dst/tests/bank.rs` comment).
- Clone = new path whose manifest references parent SSTs via `external_dbs`; durable checkpoint
  created in each parent; WAL SSTs of the checkpoint are copied; two-phase
  `initialized=false -> true` makes clone creation retry-safe. Clones unsupported with a separate
  WAL store. Projection/union clones (RFC-0004 updates) are used for split/merge "rescaling".
- `DbSnapshot` = in-process seq-number view; not durable; pins via `recent_snapshot_min_seq`.

### 1.5 Fencing in full [V unless noted]

**Writer open** (`slatedb/src/fence.rs`, `wal/slatedb/writer_init.rs`, `manifest/store.rs`):
1. Read latest manifest; compute `empty_wal_id` = first missing WAL id after
   `replay_after_wal_id` (probe).
2. `FenceableManifest::init_writer`: write manifest N+1 with `writer_epoch+1` (CAS loop, bounded
   by `manifest_update_timeout` default 300 s). Optional `expected_epoch` claims an exact epoch.
3. If the old writer advanced `replay_after_wal_id` past `empty_wal_id` meanwhile, recompute and
   refresh (asserts after).
4. Write zero-byte **fence** `wal/<empty_wal_id>.sst` with Create. On `AlreadyExists` (old writer
   got there first): refresh manifest (fails with `Fenced` if a newer epoch exists), try id+1.
5. Replay WAL `replay_after_wal_id+1 ..= fence_id`.
6. **Since #2231 (v0.18.1, 2026-11-07): validate epoch ownership after replay**, before any read
   or write is served. Previously validated before replay (see bug 1.10-B1).

**What a zombie (stale) writer can still do:**
- Accept writes into its memtable and return `WriteHandle`s; `await_durable` fails with
  `Closed(Fenced)` when its next WAL PUT hits the fence (`AlreadyExists`) or its manifest
  refresh sees a higher epoch. The DST `DbFencerActor` asserts exactly this
  (`slatedb-dst/src/actors/fencer.rs`).
- A WAL PUT that lands **before** the new writer's fence is a legitimate durable write; the new
  writer replays it (fence is placed after all existing WALs). [I, consistent with spec]
- Serve **stale reads** from its memtable/cached manifest until it notices the fence (next WAL
  flush, <=100 ms with writes; manifest poll 1 s otherwise). Reads are not epoch-validated. [I]
- Upload SSTs (unique ULIDs) - harmless orphans for GC.
- Not: commit a manifest change after the epoch bump (create at N+1 collides; refresh sees new
  epoch -> `Fenced`); every manifest write is linearized against the epoch bump by CAS.

**Compactor epoch** works the same way on the manifest and `.compactions`. Hazard (RFC-0025): "If
a standalone compactor is started alongside a compactor embedded in the writer, the compactor's
epoch is rewritten by the new compaction process. This fences the Db, disabling all Db
operations." Multi-coordinator / zombie-coordinator handling is a stated non-goal: "deployments
are responsible for ensuring at most one coordinator is active at a time". Distributed workers
use heartbeats in `.compactions` (`worker_heartbeat_timeout`) and lose their claim on conflict.

**Boundary files - exact failure prevented** (RFC-0026, `object_store.rs:148-345`):
1. A reads manifest N, prepares N+1, stalls. 2. B creates N+1; others advance to N+k.
3. GC deletes N+1 (older than `min_age`, not latest, not checkpointed). 4. A resumes, creates
N+1 successfully because "create-if-absent only protects against objects that currently exist".
5. A reports a stale update as committed.
Fix: GC advances boundary B (If-Match CAS, monotonic) *before* deleting ids <= B; writers check
the boundary *after* create; ids <= B are failures. Latest-read also rejects ids <= B. If the
store lacks If-Match (e.g. `LocalFileSystem`), set `boundary_files_enabled=false` and rely on
`min_age` > max stale-process lifetime.

**WAL namespace has no boundary file.** Instead fence objects are kept: WAL fence GC is dry-run
by default. Config doc (`config.rs`, `wal_fence_options`): W1 computes slot 7; W2 fences 7;
min_age passes; GC deletes fence 7; "W1 writes its WAL entry at position 7 and returns success"
-> data loss. Original report: issue #352 (writer acks a write into a GC'd WAL id). The FizzBee
spec says the full fix (re-check manifest after each WAL write) was rejected "due to its added
latency and API costs. Users that want a full guarantee must leave WAL fence GC disabled."

### 1.6 Ambiguous outcomes (timeouts/retries on conditional PUTs) [V]

`slatedb/src/retrying_object_store.rs` (added PR #1164, 2026-01-05):
- All object-store calls go through `RetryingObjectStore`: exponential backoff 100 ms..1 s with
  jitter from `DbRand`, sleeping on the injected `SystemClock`; **unbounded by default**
  (`object_store_max_retries: None`).
- Non-retryable: `AlreadyExists`, `Precondition`, `NotModified`, `NotFound`, `NotImplemented`,
  `NotSupported`, or anything wrapped in `NonRetryable` (`slatedb-common/src/retry.rs`).
- For conditional PUTs (Create/Update) it attaches a fresh ULID in user metadata
  `slatedbputid`. If the final outcome is `AlreadyExists`/`Precondition`, it HEADs the object; if
  the stored ULID equals ours, it returns success ("a put operation times out after the file was
  successfully written, a retry would encounter an AlreadyExists"). Otherwise the conflict stands
  -> conflict/`Fenced`. Same for multipart `complete()`.
- If the store does not support attributes, it falls back to a put without ULID (ambiguity then
  unresolved). [V]
- If the HEAD itself fails, the original conflict error is returned (treated as lost race). [V]
- Higher layers: a fatal WAL flush error closes the writer ("The manager becomes unusable after
  encountering a fatal error", `wal/slatedb/writer.rs:49`); recovery = reopen (re-fence +
  replay), which resolves the ambiguity by reading the bucket. [V/I]
- Caveat [I]: the ULID check identifies *our* object, but if GC removed it between write and
  HEAD the answer is "not ours" (min_age makes this unlikely). The DST injects downstream
  `ResetPeer` on PUT, i.e. "applied but reported failed" (see 2.2), which exercises this path.

### 1.7 Transactions [V]

RFC-0011 (Accepted; Badger-style oracle), `transaction_manager.rs`, `db_transaction.rs`.
- `IsolationLevel::Snapshot` (default; write-write conflicts only) and `SerializableSnapshot`
  (also read keys + read ranges vs. later committed write keys = backward OCC validation,
  catches write skew and phantoms; not Cahill-style rw-antidependency tracking, so it aborts
  more than PostgreSQL SSI) [V for mechanism; I for the comparison].
- State per txn: `started_seq`, `write_keys: HashSet<Bytes>`, `read_keys`, `read_ranges`.
  Committed txns go to `recent_committed_txns: VecDeque` with write keys only; GC'd when every
  active txn's `started_seq` > its `committed_seq`. Plain (non-txn) write batches are also
  recorded as single-op txns while any txn is active. Conflict check is a linear scan of that
  deque, in memory, done in the single write-batch task (`batch_write.rs`) - "no IO overhead".
- Commit returns a `WriteHandle` **without waiting for durability**; `await_durable()` needed.
  Commit makes the writes visible to other in-process txns (`advance_committed_seq`) before they
  are durable. [V] Safe for write txns because WAL durability is prefix-ordered [I]; a read-only
  txn can observe non-durable data unless it reads with `DurabilityLevel::Remote`. [I]
- Limits: single writer process only ("SlateDB is a single-writer database, and distributed
  transaction is not a goal"); no pessimistic locks; RFC proposes txn/snapshot expiration to
  bound memory, but nothing in `transaction_manager.rs` implements expiry ("TODO: have a quota
  for max active transactions") -> a long txn grows `recent_committed_txns` and pins compaction
  via `recent_snapshot_min_seq`. Conflict state is lost on restart (fine: txns die with the
  process). `unmark_write` lets callers exclude keys from conflict tracking.
- Real bug: issue #1298 lost update under SI - `begin()` read `last_committed_seq` and registered
  the txn non-atomically, so a concurrent commit's record was dropped. Found by a user's
  multi-threaded bank-transfer test; that test became the bank DST. Now `new_transaction` reads
  the seq under the manager lock.

### 1.8 Garbage collection [V]

`design/gc.mdx`, `garbage_collector/*.rs`, `config.rs`, RFC-0026, RFC-0029.
- Runs in-process by default or standalone; per-type options (manifest, WAL, WAL fence,
  compacted, compactions, detach); defaults interval 600 s, `min_age` 300 s
  (`garbage_collector.rs:59-60`); WAL fence dry-run.
- Manifests: delete if not latest, not referenced by an active checkpoint, older than `min_age`
  (object `last_modified`), and <= boundary (advanced first).
- WAL SSTs: below `replay_after_wal_id` and older than `min_age`; fences excluded unless enabled.
- Compacted SSTs: unreferenced by latest manifest and checkpointed manifests AND ULID timestamp
  `< cutoff = min(now - min_age, compaction_low_watermark, newest_l0)` (RFC-0029). The extra terms
  protect uploaded-but-unpublished outputs of running compactions and in-flight L0 uploads.
  RFC-0029 (Draft) fixes a race where parallel L0 upload minted ULIDs out of publish order so GC
  deleted an uploaded-but-unpublished L0; "It was reproduced by a deterministic simulation test
  failure in PR #1758." Interim mitigation: manifest invariant `l0_ulid_cutoff` plus re-minting
  (server-side copy to a new ULID) in PR #1758. Clock skew is out of scope: "It assumes skew is
  bounded, and users set `min_age` for the margin".
- `GcFilter` hook lets an app veto deletions (e.g. until replicated elsewhere).
- Slow readers: protected only by checkpoints (reader 10 min lifetime refreshed at half-life;
  compactor 15 min). The bank DST skips bandwidth toxics on WAL paths because a slow WAL replay
  lets a reader checkpoint expire ("we only refresh checkpoint lifetimes _after_ replay").

### 1.9 Compaction [V]

`design/compaction.mdx`, RFC-0002/0013/0024/0025/0028/0035.
- Default size-tiered scheduler; pluggable `CompactionScheduler`; Tokio executor; optional
  trivial moves; subcompactions; segments (per-prefix LSMs); experimental incremental compaction
  (publishes partial output at most once a minute, with a checkpoint before each update).
- Runs embedded in the writer by default, as a standalone process, or as coordinator + N
  stateless workers (RFC-0025, "Accepted").
- Commit: output SSTs uploaded (ULID names) -> manifest version created with inputs replaced by
  outputs (preceded by a temporary checkpoint pinning the old manifest) -> `.compactions` updated
  (manifest first, then compactions). Recovery: `Compacted`/`Running` jobs are re-validated; if
  sources are already gone from the manifest the job is marked `Failed` ("This is safe").
  Progress persisted at each output SST boundary so a resumed job skips finished key ranges.
- Write back-pressure: L0 flush stalls when `l0_max_ssts` (8) / `l0_max_ssts_per_key` (8) would
  be exceeded; `max_unflushed_bytes` 1 GiB.

### 1.10 Caching and read cost [V]

`design/caching.mdx`, `db_cache/mod.rs`, RFC-0023/0027/0031/0034.
- Block cache (`DbCache`), default `SplitCache`: data blocks (512 MiB default) separate from
  index/filter/stats (128 MiB) so scans do not evict metadata. Foyer hybrid (memory+disk) option.
  Point reads populate; scans default `cache_blocks=false`; compaction/WAL replay do not populate.
  `BlockCachePolicy`: memtable flush inserts data+index+filters; compaction output inserts only
  index+filters.
- `CachedObjectStore` (4 MiB part files on disk) **deprecated in 0.18.1**: RFC-0034 says it "did
  more harm than good" (part granularity adds latency to small reads, 64 files per 256 MiB SST,
  dropped admissions). Replaced by `slatedb-mirror` `ObjectStoreMirror` (whole-file local mirror,
  write-through, "all compacted SST reads are from local files").
- Metadata: manifest/compactions latest-object cache + probing (RFC-0032); boundary cache with
  conditional GET. Manifest/WAL writes are never cached.
- Read path: memtable -> immutable memtables -> L0 SSTs (newest first) -> sorted runs; WAL never
  read except at recovery/reader replay. `max_concurrent_sources` (default 1) trades GETs for
  latency. Cold point read [I]: per candidate SST, filter + index blocks (cached after first
  touch) then one data-block range GET (~50-100 ms on S3 Standard per tuning doc); up to 8 L0s +
  1 per sorted run, bloom filters prune most. Warm: "< 1ms" (tuning doc).

### 1.11 Durability/latency knobs [V]

Defaults (`config.rs`): `flush_interval` 100 ms; `manifest_poll_interval` 1 s;
`manifest_update_timeout` 300 s; `l0_sst_size_bytes` 64 MiB; `max_wal_flushes_before_l0_flush`
4096; `l0_max_ssts` 8; `max_unflushed_bytes` 1 GiB; reader poll 10 s, reader checkpoint 10 min;
GC 600 s / 300 s. Separate WAL object store (RFC-0009, e.g. S3 Express) and pluggable WAL
(RFC-0030). Tuning doc: durable write latency S3 Standard 50-100 ms, S3 Express 5-10 ms, MinIO
5-20 ms. Published benchmarks: benchmark.slatedb.io (300 M records, ~120 GiB, YCSB + db_bench,
64 closed-loop clients, 5 min warmup, 15 min measure) - numbers in section 1.12.

### 1.12 Published performance numbers [V unless marked]

Release suite `github.com/slatedb/slatedb-benchmark` results/0.18.1 (runs 2026-11-08; raw JSON
copied to `scratchpad/bench/`): WarpBuild arm64 32 cores/128 GB; 300 M records, 20 B keys, 400 B
values (~117 GiB); 8 GiB block cache + 4 GiB meta cache, no disk cache; 64 closed-loop clients;
`flush_interval` 100 ms. **Writes use `await_durable=false`; "durable" = time until the durable
frontier covers the write. "The suite has no per-operation synchronous-write workload."**

| Workload | S3 us-east-1 | Tigris |
|---|---|---|
| balanced 50/50, zipf .99: throughput | ~6.5K get/s + 6.5K put/s | ~1.3K + 1.3K |
| balanced: durable p50/p99/p99.9 | 105 / 196 / 327 ms | 231 / 932 / 1235 ms |
| balanced: get p50/p99 | 31 us / 44.8 ms | 28 us / 510 ms |
| point-read-uniform (mostly cold): p50/p99, ops/s | 25 / 52.6 ms, 2.57K | 48 / 596 ms, 565 |
| point-read-missing (bloom) | 1.06 M ops/s, p99 19 us | - |
| range scan p50/p99 | 74 / 134 ms | - |
| update-heavy 95% put: durable p50/p99 | 155 / 339 ms (84K put/s) | - |
| sustained ingest | 287K put/s, ~115 MiB/s; durable p50 9.4 s (backlog) | ~54 MiB/s |
| txn contention (SSI, 5R+5W, 10K hot keys) | 27.8K commits/s, 7.7K conflicts/s; commit p50 1.6 ms, durable p50 241 ms / p99 2.9 s | - |
| idle | ~3.3 GET/s (~$3.4/month [I]) | same |

- Uncached-read throughput looks capped by GET rate: ~2.4K GET/s S3, ~560 GET/s Tigris [I].
- Benchmark blog (slatedb.io/blog/benchmarking-slatedb, 2026-08-12, Riccomini): idle DB "cost
  roughly $40 per-month in API polling fees" (fixed to "under $5"); 4 MiB cache parts caused P99
  spikes; public-internet routing to Tigris caused tail latency and "very sawtoothed ingestion
  charts as SlateDB throttled writes".
- Launch post (2026-06-30): "we still have some work to do to match the read performance ... p99
  latencies drag our throughput well below RocksDB." 10K ops/s directly on S3 as KV "would cost
  $70K/mo".
- Tigris-specific: Prisma hit #1970 (open livelock 20+ min) on Tigris; Tigris tails 3-5x S3's in
  the suite. [V]


### 1.13 Correctness bugs found in the wild (catalog)

Source: issue/PR bodies read via `gh` (full dump of 2,238 issues+PRs at
`scratchpad/sdb/all.jsonl`), plus the code. All [V] unless marked. "Found by" in brackets.

Fencing / WAL / durability
- **B1 #2225 -> #2231 (Nov 2026):** 4 competing writers; a delayed fence refills a WAL slot GC
  freed; another writer replays the fence instead of the lost data WAL and becomes READY missing
  committed data (stale reads until next manifest poll). Fix: validate epoch *after* replay.
  [External TLA+ spec by Jack Vanlightly, `github.com/Vanlightly/s3-wal-collection/slatedb`,
  violation found by the TLAPS-BENCH paper (arXiv 2611.02849) running it with 4 writers; he had
  only run 3. 62-step trace, "30 min to 4 hours" to hit. Then reproduced in FizzBee.] Vanlightly:
  "so unlikely and difficult to produce I imagine it would never be triggered even by DST."
- **#352 (2024-11) / #1622 (2026-05):** zombie writer acks a write into a WAL id that GC deleted
  after a new writer advanced `replay_after_wal_id` -> acknowledged write lost. Open 18 months.
  [#352 maintainer code reading + repro test; #1622 external researcher nvartolomei with a demo on
  GCS: "Because GC already run, SlateDB succeeded writing the WAL without getting a conflict"].
  Fix: #1672 keep 0-byte fences forever by default; #1635/#1657 boundary files for metadata.
- **#271 (2024-10):** fencing race allowed two writers open at once. [FizzBee spec PR #266]
- **#1682 (2026-05):** new writer's fence landed in a slot already below `replay_after_wal_id`.
  [FizzBee `WalProtocol.fizz` while writing #1672: "I was modeling our WAL protocol ... I found an
  issue"]
- **#1625 (2026-05):** crash during WAL replay dropped never-replayed WALs (boundary recorded too
  early). [bank DST: "The bank.rs DST failed a couple of nights ago"]
- **#1857 (2026-06):** WAL flush vs memtable freeze race set `replay_after_wal_id` past an
  unflushed write -> loss on hard crash. [maintainer repro]
- **#762:** write batch not atomic in the WAL (buffer could flush mid-batch). [code reading]
- **#1018/#1025:** WAL gap after a failed WAL flush; reader fails "WAL SST not found".
  [production user ZeroFS on Azure]
- **#433, #434, #1391, #1464:** WAL-disabled / close / manifest-durability ordering bugs. [review]

GC deleting live files
- **#604 (2025):** GC deleted needed compacted SSTs -> corrupted DB. [production, ZeroFS]
- **#1044:** standalone GC could delete new L0s / in-flight compaction outputs. [maintainer]
- **#1758 / RFC-0029:** uploaded-but-unpublished L0 deleted (mint order != publish order, or clock
  skew): "silent data loss". [segments DST]
- **#2073 (OPEN):** GC deletes SSTs under long `DbReader` scans; "Observed twice in production, on
  SSTs 26 and 48 minutes old."
- **#1215 -> #1230:** manifest deleted between LIST and GET; retrier retried a 404 forever.
  [nightly chaos test + ZeroFS user]
- **#1686:** boundary-file read race -> `InvalidObjectState`. [chaos test; Codex reproduced in
  Docker]
- **#2089:** GC had silently stopped for 6 days. [production]

Compaction / manifest
- **#1192, #1652, #1769, #1795, #2002, #1935, #2132, #2134, #2143, #2107, #2113**: ghost
  compactions after restart, codec dropped destination run, resumed compaction skipped merge
  entries (lost writes), CAS-retry resurrected a terminal compaction, union-clone view-ID
  collision -> data loss, clock tick reset to `i64::MIN`. Mix of DST (#1795, #2002), production
  (#2107, #2143), code review (#1652, #1769, #1935).
- **#1838 / #2191 / #2192 (OPEN):** duplicate-destination jobs panic the worker; DB "panics on
  every open"; job with missing input retries forever and writes stall.

Wrong read results
- **#517** stale block chosen by binary search; **#1367** stale reads when one key spans SSTs;
  **#1995/#2067** descending scans wrong order [hourly DST after scan options were randomized;
  check was disabled (#1996) until fixed]; **#2184** descending seek wrong entry [`tests/scan_model.rs`
  model test vs `BTreeMap`]; **#1054** snapshot retention ignored in compaction merge.

Transactions
- **#1298** lost update under SI [user's 16-thread bank-transfer invariant test]; **#1074**
  `seek()` not tracked for SSI; **#1711** SSI tracked only returned keys -> phantoms/write skew
  [contributor code reading]; **#1732** cancelling the commit future skipped conflict detection
  -> lost update; **#1593** conflict consumed a seqnum and hung the manifest writer [DST hang];
  **#2097 (OPEN)** non-repeatable reads in `DbSnapshot` with Remote durability.

Caches / encoding
- **#499, #1083, #1542:** block-cache keys not scoped per DB; WAL ids collide across DBs ->
  "cross-database data corruption on reads". **#1319:** shared disk cache path collisions.
- **#2024 -> #2181:** sequence-tracker Gorilla encoding truncated a delta-of-delta to 32 bits ->
  manifest corruption, panic on every restart, "in production use after about 4.5 billion
  writes"; an earlier fix PR was closed as esoteric/"AI stuff"; the existing proptest "explicitly
  limits deltas below that" (the generator's bounds hid the bug).

Liveness (open)
- **#1970:** open loops forever as fencing CAS keeps losing to a live previous compactor (Prisma,
  20+ min on Tigris); fix PR #2010 (bounded conflicts, `FencingConflictLimitExceeded`) unmerged.
- **#2151:** fences pile up when a DB is opened often but never written; opens slow without
  bound (4+ min on R2).

### 1.14 Ambiguous outcomes - history [V]
- **#766 (2025-08):** object_store maintainer: with `PutMode::Create`, on a timeout "the file was
  created but the response was lost ... The client would have no way to distinguish". Riccomini:
  in that case "the client has fenced itself. This behavior isn't catastrophic" (fails safe).
  Options weighed: random id, epoch in metadata, checksum. Shipped ULID-in-metadata (#1164).
- **#564 -> #570:** epoch bump succeeded but response lost -> `ManifestVersionExists`; fix:
  retry the bump (bumping again is harmless).
- **#1992 (OPEN):** lost response on multipart `complete` can surface as `NotFound`
  (NoSuchUpload) and is not treated as ambiguous yet.
- **#2230 (OPEN):** external fencing tokens: "if an open times out, the coordinator can't tell
  whether the epoch was bumped".

---

## Part 2 - Testing and verification

### 2.1 Deterministic simulation testing (`slatedb-dst/`) [V]

Structure (`slatedb-dst/README.md`, `src/harness.rs`, `src/actors/*`, `tests/*`):
- One root seed -> `DbRand`; derived seeds for the Tokio runtime (`tokio::runtime::RngSeed`,
  needs `--cfg tokio_unstable`), DB startup, the background clock task, the failure controller,
  and one per actor.
- **Single-threaded, seeded current-thread Tokio runtime**; actors are Tokio tasks
  (`Actor::run` called in a loop until a shared cancellation token fires, then `finish`).
- **Clock:** shared `MockSystemClock`; advanced by a harness task (random 1-5 ms steps) and by
  latency toxics; all SlateDB time goes through `SystemClock` (enforced by lint, 2.5).
  `ClockedObjectStore` stamps `last_modified` from the mock clock so GC `min_age` is simulated.
- **Object store:** `DeterministicLocalFilesystem` (sync file I/O on the current task, no
  `spawn_blocking`, synthesized stable `last_modified`, sorted LIST) or in-memory; wrapped by
  `FailingObjectStore` and `ClockedObjectStore`. Separate main and WAL stores.
- **Randomized config:** `utils::build_settings` randomizes flush interval, poll/update timeouts,
  bloom filters, L0 sizes/counts, unflushed bytes, compression, compactor options (incl.
  incremental compaction, trivial move), GC intervals and `min_age` (1 ms .. 900 s, boundary
  files always on), reader options, scan options.
- **Scenarios / oracles:**
  - `tests/bank.rs`: 6 transfer actors (3 put-based SI txns, 3 merge-operator txns, retry on
    `ErrorKind::Transaction`), 3 auditors (live `Db`, `DbSnapshot`, `DbReader`) asserting the
    sum of balances is invariant, a `DbFencerActor` that reopens the DB every 120 s and asserts
    the old handle fails with `Closed(Fenced)` on put+await_durable, a standalone compactor
    restarted every 250 ms, 10 random toxics. Compacted-SST GC disabled (issue #319).
  - `tests/determinism.rs`: workload + flusher + compactor actors; replays each seed N times and
    asserts identical next-RNG value and final mock-clock time ("If either the harness or SlateDB
    consumes randomness differently ... one of those post-run checks will diverge").
  - `WorkloadActor` oracle: per-actor key prefix; values embed a monotonically increasing
    version; asserts versions never go backward, no stale value after an observed delete, scans
    ordered. This is a weak monotonic-read oracle, not a full model. [V code; I assessment]
  - `tests/rescaling.rs`: split (projection clones) and merge (union clone); exact snapshot
    equality at the split/merge barriers.
  - `tests/segments.rs`: determinism with RFC-0024 segments.
- **Reproduction:** every run logs its seeds; `SLATEDB_DST_SEEDS=<s1,s2> RUSTFLAGS="--cfg dst
  --cfg tokio_unstable" cargo test -p slatedb-dst --test <name>` replays them (PR #1877).
  Scenario tests fan out one seed per CPU core.
- **Where/how long:** PR CI runs the DST suite (`pr.yaml`, nextest profile `dst`, 30 min cap).
  **Hourly** (`dst-hourly.yaml`, since #1868, 2026-06-30) with `--cfg slow`: bank 2,000,000 ms
  simulated time, determinism 2 sims x 4,000,000 ms, rescaling, segments; 60 min step timeout,
  failure notifications.
- **What they could not make deterministic / excluded:** the disk object-store cache ("uses local
  filesystem state and blocking-task wakeups outside the seeded current-thread runtime");
  `spawn_blocking` in the compactor executor (replaced by `spawn` only under `cfg(dst)`); write
  timestamps (`WriteOptions::now` forced under `cfg(dst)`, issue #719); multi-threaded
  scheduling; real network/S3 behaviour; process crash (the harness swaps DB handles but there is
  no torn-process/kill -9 model of in-flight requests beyond what toxics produce) [I].
- **History and honest assessment:** first DST July 2025 (#694, found two non-determinism
  sources: `spawn_blocking`, ULID clock). The April 2026 rewrite (#1596) says: "The old
  deterministic simulation tests that we had were inadequate ... I can't actually recall the last
  time their nightly test run caught anything useful." After the rewrite (interleaving, faults,
  transactions, fencing) DST found ~8-10 real bugs (#1593, #1609, #1625, #1687, #1758, #1795,
  #1955, #1995, #2002), mostly hangs, crash-recovery and GC races. A linearizability checker
  (Porcupine) was planned in #1596; not present in the repo.

### 2.2 Fault injection [V]

- `FailingObjectStore` (`slatedb-dst/src/failing_object_store.rs`): toxiproxy-style `Toxic`s with
  probability, operation filter (put/get/range/head/list/delete/copy/rename) and path-prefix
  filter (`wal/`, `manifest/`, `compacted/`, `compactions/`). Kinds: Latency+jitter, Bandwidth,
  ResetPeer, SlowClose. `HttpFailBefore` injects HTTP status errors before dispatch.
  - **Applied-but-reported-failed is modelled:** `put_like` runs request toxics, then the inner
    PUT, then *response* toxics; a `Downstream` `ResetPeer` therefore returns an error after the
    write was applied. `build_toxic` generates ResetPeer in either direction with toxicity
    0.5-3.5%.
  - Not modelled [V by reading]: torn/partial objects, wrong data, eventual consistency/stale
    LIST, ETag anomalies, 409 Conflict semantics, duplicated/delayed requests that land after a
    later request, clock skew between processes, disk-cache corruption.
- **Failpoints:** `fail-parallel` crate (a fork of `fail` keyed by a per-DB `FailPointRegistry`,
  so parallel tests do not share global failpoints). ~18 `fail_point!` sites (WAL write I/O error,
  WAL probe, dispatcher loop, clone manifest, memtable flusher, compactor heartbeat ...).
  `fail_point_send!` + `fail_point_channel` emit named events and support `pause`, used to force
  exact interleavings in unit tests, e.g. `fence.rs` tests pause the fencer at `LoadEmptyWalId`,
  `FenceManifest`, `ReloadEmptyWalId`, `WriteWalFence:1`, run a competing writer + flush + GC,
  resume, then assert WAL contiguity and no data loss.
- **Chaos (nightly, `nightly.yaml` job `chaos`):** LocalStack S3 behind Toxiproxy (TCP latency,
  1 kbps bandwidth, 15% resets, slow close, 3 s "timeoutish") and `lowdown` (Riccomini's HTTP
  fault proxy: 10% 503, 5% 429 fail-before); runs `test_concurrent_writers_and_readers`. Found
  #1215 and #1686.
- **Flaky-test hunt (nightly):** loops the full lib test suite for 15 minutes.

### 2.3 Formal methods [V]

`specs/fizzbee/` (FizzBee, a Python-like TLA+-style model checker):
- `WalProtocol.fizz`: writer fencing + WAL replay + GC. Assertions: no WAL id reported successful
  twice; live replayable WALs contiguous above `replay_after_wal_id`; epochs nondecreasing by id;
  replayed WALs exist; validated replay preserves committed data; boundary monotonic; deletes only
  behind boundary; latest never deleted. Bounds: 3 writers (comment: "Set NUM_WRITERS to 4 to
  reproduce the error in #2225"), 2 GCs, 3 writes/writer, 40 actions. WAL fence GC deliberately
  disabled in the model.
- `SequencedMetadataBoundary.fizz`: the RFC-0026 boundary protocol; create and boundary check are
  separate atomic actions so GC can interleave; ghost state distinguishes "object exists" from
  "writer returned success"; assertion `CommitsOnlyAboveBoundary`, `CommittedHistoryIsDurable`,
  `ReadsOnlyAboveBoundary`.
- `KeyValueStore.fizz` (2024, by FizzBee author JP): basic write/flush/read with liveness
  ("always eventually") assertions.
- **Kept in sync?** Partly. Spec commits accompany protocol PRs (#1635, #1672, #1703, #1709,
  #1718, #2231), but the specs are **not run in CI** (no fizz step in `.github/workflows`) and are
  hand-maintained abstractions. They found #271 and #1682 and validated the #2231 fix.
- **External TLA+:** Vanlightly's `s3-wal-collection` models SlateDB's WAL (plus a CAS-manifest
  variant) alongside Chroma wal3, S2C, OSWALD, objwal, Cursor, Walgit, OpenData Buffer. This is the
  most directly reusable formal artifact for a single-writer WAL on S3.
- No P, no TLA+ in-repo, no TLAPS proofs in-repo.

### 2.4 Antithesis / Jepsen [V]
None. Antithesis appears once, as a suggestion in issue #267 (2024); Jepsen never. No Elle /
Porcupine history checking.

### 2.5 Other techniques [V]
- **Determinism lints:** `.config/clippy.toml` `disallowed-types`/`disallowed-methods` ban OS RNGs
  (`OsRng`, `ThreadRng`, `StdRng`, ChaCha/Xoshiro...), `uuid`/`ulid` generators (use
  `IdGenerator`), `std::time::{Instant, SystemTime}`, `chrono::Utc::now`, `tokio::time::{sleep,
  interval, timeout, Instant::now}` (use `SystemClock`), `std::thread::spawn/sleep`,
  `tokio::task::spawn_blocking`, `Option/Result::unwrap` (use `expect`), unbounded `send` (use
  `create_safe_channel`). Crates `#![deny(clippy::disallowed_types, clippy::disallowed_methods)]`.
- **Runtime invariants on manifest writes** (`manifest/invariants.rs`, `with_invariants`): checked
  before every CAS; Rust `panic!` on impossible states (stored epoch < local epoch; missing
  boundary file).
- **Property-based tests:** proptest in ~20 modules (iterators, memtable, db_state, wal_replay,
  transaction_manager, filters...). No notable correctness bug credited to proptest; in #2189 the
  generator bounds hid a production bug.
- **Model-based tests:** `slatedb/tests/scan_model.rs` compares `Db` scans/seeks against a
  `BTreeMap` with tiny blocks/SSTs to force versions across SST boundaries (found #2184). The 2025
  DST stored expected state in SQLite (#778) before the rewrite.
- **Format evolution checks:** `scripts/check_flatbuffers_evolution.sh` in PR CI; docs promise
  storage format compatibility between adjacent versions.
- **Fuzzing, loom, miri, shuttle, turmoil, madsim:** none found.
- **Test object stores:** `object_store::memory::InMemory` (most unit tests),
  `DeterministicLocalFilesystem` (DST), `LocalFileSystem` (no If-Match -> boundary disabled),
  LocalStack S3 (chaos), plus users on real S3/GCS/Azure/R2/Tigris. No real-S3 or Tigris job in CI.
- **AI in the loop:** many repro/fix PRs credit Codex/GPT; `CLAUDE.md`/`AGENTS.md` set writing
  rules; FizzBee ships agent skills (`fizz install-skills`).

### 2.6 What found real bugs (summary) [V]
- Formal models (FizzBee, external TLA+): the subtle multi-writer fencing/GC-reuse bugs (#271,
  #1682, #2225). DST would likely never hit #2225 (62-step trace).
- DST (post-rewrite) + chaos: crash-recovery, hangs, GC races, scan ordering, compaction CAS
  retries.
- Production users (ZeroFS, Prisma, others): GC deleting live SSTs, cache key collisions,
  encoding overflow after 4.5 B writes, liveness under contention on Tigris/R2.
- Code review (human + AI): most transaction-isolation and compaction-state bugs.
- Model-based scan test: seek/scan ordering.
- Proptest: essentially nothing credited.

---

### 2.7 Notes on conflicting sources
- Open issues #722/#724/#725/#727 (filed 2025-08) say DST lacks DbReader, fencing, network and
  filesystem faults. They are stale: the current code (post-#1596) has a `DbFencerActor`, a
  reader auditor, and network toxics. Filesystem faults (disk cache) are still absent. [V code]
- Antithesis: only hearsay in #573 (Riccomini, 2025-05-30): "I'm told it's gone through
  antithesis DST testing at some of them [companies]". No case study. [V]
- Open gaps filed 2026-11-07: DST for incremental compaction (#2235) and the mirror cache
  (#2234). [V via agent]

### 2.8 Maintainer statements on design limits [V]
- #352 thread (2024-11..2026-05): the fencing fixes are "in some sense, time dependent"; "to truly
  make this bullet proof (i.e. not clock dependent), we need to read after write" (Riccomini);
  "every file we write has this problem right now"; "I think nearly everyone is doing
  await_durable=false anyway"; Gavra: OK with "some generous time boundary after which point a
  zombie writer can ack a write (e.g. 24h)". A post-write manifest HEAD was estimated at ~$10/month
  at 100 ms flush but was not adopted. Moving to an overwrite-style manifest was rejected partly
  because GCS limits writes to one object name to ~1/s.
- Launch post next steps = current gaps: pluggable WAL for latency, promote read replicas,
  predictive cache, "Multi-writer support".
- No explicit "what we'd do differently" retrospective found. `CLEAN_SLATE.md`: "SlateDB is a
  transactional database, so ACID semantics are paramount ... simple, incremental, and empirical";
  "We default to copying RocksDB, LevelDB, Badger, or Pebble"; cites TigerBeetle's Tiger Style.

---

## Part 3 - Lessons for a Go embedded SQL engine on S3/Tigris

### 3.1 Projects that put SQL or other layers on SlateDB [V unless marked]
- **DuckDB extension `duckdb_object_storage`** (dentiny.github.io/posts/duckdb-on-object-storage,
  2026-10-09): the only SQL engine on SlateDB found. Stores DuckDB's files as 256 KiB chunks in
  one SlateDB per DB (a mini file system); FileSync -> one durable write batch. TPC-H: reads 1.08x
  native (local) / 1.20x (cold S3); writes 0.19x (~5x slower). Pain points: **double logging**
  ("logged by DuckDB and then again by SlateDB"), in-process compaction/GC competing for CPU,
  serial manifest/index loading at cold start, one S3 round trip per commit, second writer gets an
  I/O error but is not fail-fast. Wishlist: single WAL, block manager, strong-read mode, snapshot
  pinning.
- **ZeroFS** (NFS/9P/NBD on S3): v2.0.0 (2026-07-02) moved file data out of SlateDB; SlateDB now
  holds only metadata. Reasons: "LSM compaction repeatedly downloaded and re-uploaded file
  contents as SSTs merged", "per-fsync WAL churn", memory/CPU of memtables, caches, filters.
  "The WAL is gone, permanently." Their HA post: crash failover ~9 s; "a crash before a flush loses
  acknowledged writes". ZeroFS runs Jepsen in its own CI. Largest source of SlateDB production
  bug reports (#591, #604, #1025, #1215, #1859).
- **s2-lite** (s2.dev): single node, vertical scaling; two instances on one bucket -> "SL8 will
  fence the older writer ... potential for stale reads until the fencing happens".
- **OpenData** Log/Vector/Timeseries: batching and an external buffer to hide write latency;
  Vector acks ~1 s (one SlateDB write/s for PUT cost) or ~100 ms with Buffer but without
  read-your-writes. Log: poll latency p50 ~30 ms / p99 ~300 ms at 25 MiB/s.
- **Responsive RS3** (Kafka Streams state), **Embucket**/Rustberg (Iceberg catalog metadata) [S].
- Prisma, Dropbox, Gadget, Goldsky: user list only; Prisma runs a fork carrying the #1970 fix.

### 3.2 Transfers directly (adopt)
1. **Write-once, sequentially numbered objects with `If-None-Match:*` as the commit point**, plus
   whole-state manifests re-derived by a mutator on conflict (`maybe_apply_update`). Simple,
   provable, cheap to read with cached probing (GET N+1 until 404). [V]
2. **GC boundary file advanced with `If-Match` before deleting any sequenced id, checked after
   every create and on every latest-read** - apply it to *every* put-if-absent namespace you GC,
   including the WAL. SlateDB did not for WAL and paid with #352/#1622/#1682/#2225; they keep WAL
   fences forever instead. If you GC WAL ids, give the WAL a boundary too, or never reuse/delete
   commit-point names. [V; I for recommendation]
3. **Validate ownership (epoch) after recovery/replay and before serving anything** (#2231). [V]
4. **Idempotency token in object metadata on every conditional PUT**, HEAD on 412/AlreadyExists to
   resolve "my retry vs. someone else". Verify Tigris preserves `x-amz-meta-*` on conditional PUT
   and multipart complete; treat attribute-unsupported as fatal rather than silently falling back.
   Also handle `NoSuchUpload` on multipart complete as ambiguous (#1992). [V; I]
5. **Fail-stop on unresolved ambiguity**: a fatal WAL/manifest error closes the handle; recovery
   (re-fence + replay) re-reads the bucket. Return "unknown outcome" explicitly to callers (as
   `CheckpointHandle::wait` does). [V]
6. **Determinism discipline in Go**: inject Clock, RNG, ID generator, object store, and
   scheduler; ban `time.Now`, `time.Sleep`, `time.After`, `math/rand` global, `crypto/rand` for
   ids, `uuid.New`, raw `go` statements outside the scheduler, via a custom `go/analysis` linter
   (the equivalent of their clippy `disallowed-*` lists). [V for theirs; I for Go]
7. **DST harness shape**: one seed -> derived seeds; actors; mock clock advanced by harness and by
   latency faults; object-store wrapper with request-side and **response-side** faults (applied
   but reported failed); randomized config per seed (including tiny GC `min_age`); a
   determinism self-test (same seed twice -> same RNG/clock end state); `SEEDS=` env replay; run
   on every PR plus hourly. [V]
8. **Invariant-style oracles that survive faults**: bank-transfer sum invariant across live,
   snapshot and reader views; "old handle must be fenced" after takeover; exact equality at
   quiesced barriers; monotonic version reads. Add a real history checker (Elle/Porcupine-like,
   e.g. `anishathalye/porcupine` is Go-native) - SlateDB planned it but never shipped it. [V; I]
9. **Formal model of the fencing/GC/recovery protocol** (FizzBee or TLA+), with ghost state
   separating "object exists" from "writer reported success", **at >= 4 writer incarnations**:
   the #2225 counterexample needed 4. Start from Vanlightly's `s3-wal-collection` specs. Run the
   model in CI (SlateDB does not). [V; I]
10. **Runtime invariants checked before every manifest CAS** and panics on impossible states.
    [V]
11. **Model-based tests against an in-memory reference** with tiny blocks/files to force edge
    layouts (found #2184). Make property-test generators span full integer ranges (#2189). [V]
12. **Chaos job against a real S3 API through toxiproxy + an HTTP fault proxy** (`lowdown` is
    Riccomini's). For you: run it against Tigris itself, nightly; Tigris had the worst tails and
    the #1970 livelock. [V; I]

### 3.3 Does not transfer, or needs change
- **Tokio seeded current-thread runtime** has no Go equivalent; Go's scheduler is not
  controllable. You must own concurrency: a single-goroutine event loop per component with an
  explicit simulated scheduler/task queue in tests (FoundationDB/TigerBeetle style), or accept
  non-deterministic tests. Budget for this early; it shapes the code. [I]
- **`fail-parallel` failpoints**: in Go use an injected hook registry (no global state) with
  pause/notify channels; same idea. [I]
- **Ack-before-durable default** (`await_durable=false`, reads see `Memory` data, commit returns
  before durable). For ACID SQL, COMMIT must wait for the WAL PUT, and read-only transactions must
  either read only durable state or wait for the durable frontier to cover their snapshot before
  returning. Otherwise a crash can retract data a client already saw. [V/I]
- **Time-based safety** (GC `min_age`, ULID-timestamp cutoffs, checkpoint lifetimes) rests on
  bounded clock skew and bounded pauses; RFC-0029 and #352 admit it. Prefer reference-based GC
  (deletes only of names the manifest has retired, plus boundaries) over timestamp cutoffs; use
  Tigris/S3 server `Last-Modified` rather than local clocks where a time is unavoidable. [V; I]
- **Zombie stale reads**: SlateDB serves reads from a fenced writer until it notices (up to a
  WAL flush or manifest poll). For strict serializability, a read-only txn must prove it is
  still the owner at or after its read point (e.g. a conditional GET/HEAD on the latest manifest
  or a lease), or you document that reads are only snapshot-consistent. [V/I]
- **Compaction/GC in-process** competes with queries (DuckDB report) and starting a second
  compactor fences the DB (RFC-0025). Keep one owner per role and make role takeover explicit. [V]
- **LSM KV under SQL**: double logging if the SQL layer has its own WAL (DuckDB). Use the KV
  WAL as the only log; map rows/index entries to keys. [V/I]

### 3.4 What the SQL/transaction layer changes
- Conflict detection: SlateDB's OCC keeps read sets/ranges per txn in memory and scans a deque
  of committed write sets linearly. A SQL layer generates large read ranges (scans, index range
  lookups) - expect many false conflicts under SSI-as-OCC and memory growth; add txn age/size
  limits (RFC-0011 proposed expiry, never built) and coarser range tracking. [V/I]
- Ordering: one writer task assigns seqnums; commit = in-memory validation + WAL append. Keep that:
  serializability is decided in one place, durability in another (the WAL PUT). Group commit
  amortizes the 50-230 ms PUT. [V]
- DDL/schema and catalog changes are just transactional writes, but schema caches in readers need
  invalidation driven by the manifest/WAL position they replay to. [I]
- Multi-process readers get snapshot (not linearizable) reads, stale by poll interval (10 s
  default); expose the read point. [V/I]
- Large transactions: SlateDB batches must fit the WAL buffer/memtable; a SQL bulk load needs a
  path that writes SSTs directly and commits them via manifest (like their WAL-disabled path). [I]

### 3.5 Looked for and not found
- Antithesis/Jepsen results for SlateDB (only hearsay); fuzzing; loom-class concurrency testing;
  a linearizability/history checker; FizzBee in CI; an explicit maintainer retrospective; a
  synchronous `await_durable=true` latency benchmark; any Postgres/SQLite-on-SlateDB project; talk
  transcripts (P99 Conf 2024, Data Council 2025, QCon SF 2026-11-17 is upcoming); GitHub
  Discussions (disabled; much debugging is on Discord, not accessible).
