# Litestream and the Litestream VFS: deep dive

Compiled 2026-11-10. This builds on the Litestream paragraphs in `object-storage-architectures.md` and does not repeat them.

Legend:
- **[V]** verified from a primary source: repo source, the official docs, a release note, an issue or PR by a maintainer, or the authors' own blog.
- **[I]** my inference.

Sources read:
- Litestream source: https://github.com/benbjohnson/litestream at `main` = `af28505` (2026-11-09). The latest release is v0.5.18 (2026-11-03). File paths below are relative to this repo unless stated otherwise.
- LTX library and format spec: https://github.com/superfly/ltx at `main` = `ad7202a` (2026-10-29). Litestream pins `github.com/superfly/ltx v0.5.2`.
- Docs site source: https://github.com/benbjohnson/litestream.io at `6b6893d` (2026-07-30). The live page https://litestream.io/reference/vfs/ was fetched on 2026-11-10 and matches the repo text.
- Docs pages: https://litestream.io/reference/vfs/, https://litestream.io/how-it-works/, https://litestream.io/how-it-works/vfs/, https://litestream.io/guides/vfs/, https://litestream.io/guides/vfs-write-mode/, https://litestream.io/guides/vfs-hydration/, https://litestream.io/reference/config/, https://litestream.io/reference/ltx/, https://litestream.io/tips/.
- Fly.io blog, all by Ben Johnson:
  - "Litestream: Revamped", 2025-05-20, https://fly.io/blog/litestream-revamped/
  - "Litestream v0.5.0 is Here", 2025-10-02, https://fly.io/blog/litestream-v050-is-here/
  - "Litestream VFS", 2025-12-11, https://fly.io/blog/litestream-vfs/
  - "Litestream Writable VFS", 2026-01-29, https://fly.io/blog/litestream-writable-vfs/
  - Publication dates come from the posts' `datePublished` metadata and the blog's Atom feed. No other 2025 or 2026 post on the Fly.io blog is about Litestream. "SBD: We Have a Filesystem at Home" (2026-10-22, https://fly.io/blog/sprites-block-device/) mentions Litestream only in passing.
- Release notes: https://github.com/benbjohnson/litestream/releases (v0.5.0 to v0.5.18).
- Issues and PRs cited by number at https://github.com/benbjohnson/litestream/issues/NNN.

---

## 0. Timeline [V, from tags, release notes and blog posts]

- **2025-05-20:** "Litestream: Revamped" announces the redesign around LTX, compaction, a lease based on conditional writes, and a planned VFS read-replica layer.
- **2025-08-20:** PR #721 "Litestream VFS MVP" lands. Polling for new files follows on 2025-08-29 (#726).
- **2025-09-30:** v0.5.0 ships. It replaces generations and shadow WAL files with LTX files and TXIDs, allows one replica destination per database, and drops CGO (it uses `modernc.org/sqlite`). The blog post (2025-10-02) calls the VFS "the next major feature", with "a proof of concept working".
- **2025-11:** The pending index for read transactions (#828, 2025-11-06), time-based L0 retention (#821, 2025-11-05), the page cache (#838), PRAGMA time travel (#850) and the observability PRAGMAs (#856) land.
- **2025-12-11:** v0.5.3 is the first release with prebuilt VFS binaries, under the heading "VFS Extension (Experimental)". The "Litestream VFS" blog post is published the same day.
- **2026-01-09:** v0.5.6 adds VFS write mode (#953) and creating new databases in write mode (#972).
- **2026-01-29:** "Litestream Writable VFS" blog post.
- **2026-02-02:** v0.5.7 adds background hydration (#978), VFS compaction through a shared `Compactor` (#979), and `PRAGMA litestream_hydration_progress`.
- **2026-02-12:** v0.5.8 adds `s3/leaser.go`, "distributed leasing with If-Match conditional writes" (#1073).
- **2026-02-23:** v0.5.9 adds `PRAGMA litestream_write_enabled` and persistent hydration files with a `.meta` sidecar.
- **2026-07:** v0.5.14 publishes the extension as `litestream-vfs` on PyPI, npm and RubyGems.
- **2026-11-03:** v0.5.18 is the latest release. Its notes still label the VFS "Experimental".

---

## 1. What Litestream is, and how it captures changes

- [V] "Litestream is a streaming replication tool for SQLite databases. It runs as a separate background process and continuously copies write-ahead log pages from disk to a replica." (https://litestream.io/how-it-works/)
- [V] Since v0.5, each database replicates to one replica destination. (how-it-works; v0.5.0 post: "You only get a single replica destination per database.")
- [V] **It takes over checkpointing.** "It starts a long-running read transaction to prevent any other process from checkpointing and restarting the WAL file. Instead, it continually reads new WAL pages and manually calls out to SQLite to perform checkpoints as necessary." (how-it-works) In the source, `DB.acquireReadLock` opens a transaction and runs `SELECT COUNT(1) FROM _litestream_seq;` to hold the read lock (`db.go`).
- [V] **Monitor loop.** `DB.monitor` ticks every `DefaultMonitorInterval = 1s`. `DB.sync` opens the WAL at the last synced offset and verifies the salts and the previous frame. On a mismatch (`PrevFrameMismatchError`) it restarts from the WAL header and writes a snapshot. `WALReader.pageMap` builds a map from page number to the latest committed frame, and the pages are encoded into a local L0 file `LTXPath(0, txID, txID)` with `txID = pos.TXID + 1` (`db.go`, `wal_reader.go`).
- [V] One TXID covers a batch: "A TXID identifies the whole batch of WAL pages in that file, which may span one or more SQLite write transactions, so it is not a per-transaction identifier." A sync that finds no new committed WAL pages writes no file. (how-it-works)
- [V] `max-sync-wal-bytes` (default 64 MiB, since v0.5.15) splits a large catch-up into several files. The limit is checked only at commit boundaries, so one transaction is never split. (how-it-works, config reference)
- [V] **Copy before checkpoint:** `checkpointWithExecutor` first runs a full sync ("Copy end of WAL before checkpoint to copy as much as possible"), and only then executes the checkpoint. So frames are always in an LTX file before SQLite can overwrite them (`db.go`).
- [V] **Checkpoint policy** (`DB.checkpointIfNeeded`, `db.go`): a PASSIVE checkpoint once the WAL reaches `MinCheckpointPageN = 1000` pages, a time-based PASSIVE checkpoint every `CheckpointInterval = 1m` when changes were synced, and an emergency TRUNCATE checkpoint above `TruncatePageN = 121359` pages (about 500 MB at 4 KiB pages), tried first as PASSIVE.
- [V] LTX files are staged locally in a hidden directory next to the database (for example `/var/lib/.db-litestream` for `/var/lib/db`) and then uploaded. (how-it-works) The replica uploader runs every `DefaultSyncInterval = 1s` (`replica.go`).
- [V] The `litestream` daemon uses `modernc.org/sqlite`, which needs no CGO. The VFS needs CGO and `github.com/mattn/go-sqlite3`. (v0.5.0 post, `go.mod`, VFS reference)

## 2. The LTX file format [V: superfly/ltx `README.md`, `ltx.go`, `checksum.go`, `compactor.go`]

- The README documents format version 3. Versions 2 and 3 both use the magic `LTX1`, and "A reader cannot determine the format version from the file alone".
- **Four sections:** header, page block, page index, trailer. Integers are big-endian.
- **Header (100 bytes):** `Magic`, `Flags`, `PageSize`, `Commit` (database size in pages after applying the file), `MinTXID`, `MaxTXID`, `Timestamp` (ms since the epoch), `PreApplyChecksum`, `WALOffset`, `WALSize`, `WALSalt1`, `WALSalt2`, `NodeID`, and 20 reserved bytes.
- **Header flag `HeaderFlagNoChecksum` (0x2):** disables database checksum tracking. The pre- and post-apply checksums are then zero.
- **Page block:** frames in ascending page-number order. Each frame is `Pgno` (4 bytes), `Flags` (2), `CompressedSize` (4), then the LZ4 block-compressed page. A six-byte zero page header ends the block. Pages are compressed one by one, so a single page can be decompressed alone (also stated in the v0.5.0 post).
- **Page index:** one entry per page, sorted by page number, each three unsigned varints: page number, absolute byte offset of the frame, and the frame's encoded size. A zero page number ends the entries, followed by an 8-byte size of the index.
- **Trailer (16 bytes):** `PostApplyChecksum` and `FileChecksum`.
- **File checksum:** CRC-ISO-64 over the header, every page header with its **decompressed** data, the zero header, the page index, and the post-apply checksum. "it is not a byte-for-byte checksum of the file on disk."
- **Database checksum:** `ChecksumPage` is a CRC-64 of the page number and page data. The database checksum is the XOR of the checksums of all pages except the lock page (`ChecksumReader`). Because it is an XOR, it can be updated per page by XOR-ing out the old page and XOR-ing in the new one. [I]
- [V, Litestream source] Litestream v0.5 writes every LTX file with `HeaderFlagNoChecksum`: L0 files (`db.go` lines 2259 and 2996), compactions (`compactor.go`, `replica.go`), and VFS write-mode files (`vfs.go`). So the database checksums are not tracked, and only the file checksum protects each file.
- [I] A page frame has no checksum of its own. A page fetched alone by a range request can therefore not be verified against the file checksum. It is only checked for decompressing to the page size.
- **Snapshot:** a file with `MinTXID == 1` (`Header.IsSnapshot`). A snapshot must have a zero pre-apply checksum, and other files must have one, unless checksums are disabled.
- **Lock page:** the page at the 1 GiB `PENDING_BYTE` (`LockPgno`) is never stored. The Litestream testing guide calls this "The Lock Page Problem" and tests databases over 1 GB for it (`docs/TESTING_GUIDE.md`).
- **Filenames:** `ltx.FormatFilename` gives `%016x-%016x.ltx` from the min and max TXID, for example `0000000000000001-0000000000000005.ltx`.

## 3. Compaction levels, snapshots, retention [V: config reference, how-it-works, `compactor.go`, `compaction_level.go`, `store.go`]

- **L0:** one file per sync that has new pages ("uncompacted per-sync batches").
- **L1, L2, L3:** compacted every 30 s, 5 min and 1 h by default (`levels:` in the config). The position of an entry in the list sets the level number. L8 is the highest configurable level.
- **L9:** the snapshot level, a full copy of the database every `snapshot.interval` (default 24 h).
- **Time grid:** "Compaction is scheduled on an absolute time grid. Each level targets the next boundary of its interval, so a `5m` level aims at `:00`, `:05`, `:10`, and so on no matter when the daemon started." Each level also makes one attempt at startup.
- **Compaction step** (`Compactor.Compact`): it reads the max TXID of the destination level, lists source-level files after it, and merges them with `ltx.NewCompactor`, which writes each page from the newest input that has it (`writePageBuffer` iterates inputs from newest to oldest). The result is uploaded as one file `minTXID-maxTXID` at the destination level. Inputs must have equal page sizes and contiguous TXIDs. When available, the daemon reads its local copies of the source files instead of downloading them (`LocalFileOpener`).
- **Compaction verification:** `verify-compaction` (since v0.5.7) checks after each compaction that the destination level's TXIDs are contiguous.
- **L0 retention:** an L0 file is deleted only when it has been compacted into L1 **and** is older than `l0-retention` (default `5m`, checked every 15 s; 0 is rejected). The newest L0 file is always kept (`EnforceL0Retention`). The config reference gives the reason: retention "prevents race conditions where L0 files could be deleted before VFS has time to fetch newly created L1 files."
- **Snapshot retention:** snapshots older than `snapshot.retention` (default 24 h) are removed, but at least one is always kept. The minimum retained snapshot TXID is then applied as one cutoff to all levels L1 and up. Levels do not age out separately. Compacting L1 into L2 leaves the L1 files in place until that cutoff removes them.
- **Restore granularity:** "Litestream replays whole LTX files and never applies part of one, so your restore points are the boundaries of the files that still exist in the replica." A file is used only if its whole TXID range fits the target. After L0 expiry, the finest restore points are L1 boundaries. (how-it-works)
- [V, v0.5.0 post] Levels let you restore "to any point in time, using only a dozen or so files on average."
- [V, VFS post] Compaction, as the post explains it: when pages 1 to 5 were written in the order `1 2 3 5 3 5 4 5 5`, a restore wants only the rightmost copy of each page, and "the algorithm is trivial: reading backwards from the end of the sequence, skipping any page you already read."

## 4. Layout of a replica in a bucket [V: `s3/replica_client.go`, `s3/leaser.go`, `litestream.go`]

- S3 object keys are `<path>/<level as 4 hex digits>/<minTXID>-<maxTXID>.ltx`. For example: `db/0000/0000000000000007-0000000000000007.ltx` (L0), `db/0001/...` (L1), `db/0009/0000000000000001-00000000000003e8.ltx` (a snapshot). (`WriteLTXFile`: `key := c.Path + "/" + fmt.Sprintf("%04x/%s", level, filename)`. A test fixture lists `replica/0000/0000000000000001-0000000000000002.ltx`.)
- The file backend instead uses `<root>/ltx/<level>/<filename>` (`litestream.LTXFilePath`).
  - The docs say files are organized "by compaction level under an `ltx/` prefix", and the v0.5.0 post says "New LTX files go in an `ltx` directory on the replica". For S3 at `af28505` there is no `ltx/` prefix. [V source vs docs]
- Each S3 object carries its creation timestamp in user metadata (`MetadataKeyTimestamp`, RFC 3339 nano). Listing for a timestamp restore issues parallel `HeadObject` calls to read it (`fetchMetadataBatch`). Ordinary listing uses `LastModified`.
- The lease object, when used, is `<path>/lock.json` (see §10).
- There is no manifest object. Readers discover files by `ListObjectsV2` on a level prefix. The S3 iterator lists the whole level prefix and filters by TXID on the client (`newFileIterator`; the `seek` filter is applied in Go, with no `StartAfter`).
  - An open PR, #1173 "feat(s3): add manifest file to reduce LIST operations", motivates this: "A user observed ~100k LIST operations vs ~4k GET operations when Litestream polls S3 every second." It is not merged at `af28505`.

## 5. Restore plan [V: `CalcRestorePlan` in `replica.go`]

- Start from the newest L9 snapshot at or before the target (TXID or timestamp).
- Open one listing cursor per level, from 8 down to 0. Repeatedly pick, from any level, a file whose `MinTXID <= currentMax+1` and that extends the contiguous range the furthest. `restoreCandidateBetter` prefers a larger `MaxTXID`, then a smaller `MinTXID`, then a higher level, then an older file. Append it and advance.
- For a "latest" plan, if any level's next file starts after `currentMax+1`, fail with "non-contiguous ltx files". [V] how-it-works: "Because TXIDs form a contiguous sequence, Litestream can verify that no transactions are missing before restoring".
- A TXID target is inclusive. A timestamp target is exclusive: only files created strictly before it are used. (how-it-works)

---

## 6. The VFS: what it is and how it is loaded

- [V] "The Litestream VFS provides read-only access to a replicated SQLite database directly from object storage. It registers a VFS named `litestream`, fetches pages on-demand, and continuously polls the replica for new LTX files. The VFS is built separately from the main `litestream` binary and requires CGO." (VFS reference)
- [V] From the blog: "It runs SQLite hot off an object storage URL." "It's just a plugin for the SQLite you're already using." "Litestream VFS handles only the read side of SQLite. Litestream itself, running as a normal Unix program, still handles the 'write' side." (VFS post, 2025-12-11)
- [V] **Loadable extension:** built with `make vfs` (`-tags "vfs,SQLITE3VFS_LOADABLE_EXT"`, `-buildmode=c-archive`, then linked with `src/litestream-vfs.c`). The entry point is `sqlite3_litestreamvfs_init`. Usage: `.load ./dist/litestream-vfs sqlite3_litestreamvfs_init` then `.open 'file:replica.db?vfs=litestream'`. Since v0.5.14 it is also published as `litestream-vfs` on PyPI, npm and RubyGems for linux/darwin amd64/arm64.
- [V] **Environment configuration** (`cmd/litestream-vfs/main.go`, `LitestreamVFSRegister`): `LITESTREAM_REPLICA_URL` is required. It accepts `s3://`, `gs://`, `abs://`, `sftp://`, `file://`, `nats://`, `webdav://`, `webdavs://` and `oss://`. The URL is read when the extension loads, so "Setting it from inside the process ... is not reliably visible". Other variables: `LITESTREAM_LOG_LEVEL`, `LITESTREAM_LOG_FILE`, `LITESTREAM_S3_ENDPOINT`, `LITESTREAM_WRITE_ENABLED`, `LITESTREAM_SYNC_INTERVAL`, `LITESTREAM_BUFFER_PATH`, `LITESTREAM_HYDRATION_ENABLED`, `LITESTREAM_HYDRATION_PATH`.
- [V] **Per-database configuration** (`vfs_config.go`, #1157, 2026-08-29): `SetVFSConfig(dbName, cfg)`, and URI parameters `replica_url`, `write_enabled`, `sync_interval`, `buffer_path`, `hydration_enabled`, `hydration_path`, `poll_interval` and `cache_size`. With `replica_url` set, a connection gets its own replica client (`openMainDB`).
- [V] **Go usage:** `litestream.NewVFS(client, logger)` with any `ReplicaClient`. Set `vfs.PollInterval` (default 1 s) and `vfs.CacheSize` (default 10 MB), call `sqlite3vfs.RegisterVFS("litestream", vfs)` (github.com/psanford/sqlite3vfs), then `sql.Open("sqlite3", "file:prod.db?vfs=litestream")`. This needs the `vfs` build tag and mattn/go-sqlite3 (CGO). (VFS guide)
- [V] **PRAGMAs and functions:** these go through `xFileControl` with `SQLITE_FCNTL_PRAGMA` (`VFSFile.FileControl`). An auto-extension registers the SQL functions on every connection (`src/litestream-vfs.c`).
  - `litestream_txid`: the current TXID as 16 hex characters.
  - `litestream_lag`: seconds since the last successful poll, or -1 before the first.
  - `litestream_time`: get or set the point in time. It takes RFC 3339 or relative values such as `'5 minutes ago'` and `'yesterday'`, and `'latest'` resets it.
  - `litestream_hydration_progress` and `litestream_hydration_file`.
  - `litestream_write_enabled`.
  - Function forms: `litestream_txid()`, `litestream_lag()`, `litestream_time()`, `litestream_set_time(v)`.
- [V] SQL function time travel is per connection. The C side maps each `sqlite3*` to the VFS file id (`RegisterVFSConnection`, `vfsFileForConnection`).

## 7. The VFS read path

### Opening a database (cold start) [V: `VFSFile.Open`, `waitForRestorePlan`, `buildIndexMap`, `FetchPageIndex`]
1. **Restore plan:** `CalcRestorePlan(latest)` lists L9 and then levels 8 to 0, which is about 10 LIST requests. In read-only mode, if no files exist (`ErrTxNotAvailable`), it waits and retries every `PollInterval`. "The VFS waits for available backup files before serving reads." In write mode it does not wait: it creates a new database.
2. **Page size:** one range GET of the 100-byte header of the newest file in the plan. Page sizes 512 to 65536 are accepted.
3. **Page index:** for **each** file in the plan, `FetchPageIndex` sends one range GET for the last 32 KiB (`DefaultEstimatedPageIndexSize`). It reads the 8-byte index size just before the trailer. If the index is larger than what was fetched, it sends a second range GET from the start of the index. Then it sends one more 100-byte header GET to learn `Commit`, the database size in pages. The indexes are merged into one map from page number to `{Level, MinTXID, MaxTXID, Offset, Size}`, and a newer TXID wins (`mergePageIndexes`).
4. It starts the polling goroutine, and the write-sync goroutine and hydration if enabled.
- [I] Cold-start cost is therefore about 10 LISTs plus 1 + 2N (or up to 3N) small range GETs, where N is the number of files in the restore plan (a snapshot plus some L3, L2, L1 and L0 files; "a dozen or so files on average" per the v0.5.0 post). No page data is fetched until SQLite reads a page. Index fetches run one after another (a `for` loop), not in parallel.
- [V] The blog: each page index "occupies about 1% of its LTX file", and "we didn't have to download the whole database to run this query."

### Reading a page [V: `VFSFile.ReadAt`, `FetchPage`]
- The page number is `off/pageSize + 1`. "that byte offset is an illusion" (blog). The lookup order is:
  1. the write buffer, for dirty pages in write mode;
  2. the hydrated local file, once hydration is complete;
  3. the LRU cache (`hashicorp/golang-lru`, with `CacheSize / pageSize` entries);
  4. the index entry, followed by **one range GET of exactly that page frame** (`OpenLTXFile(level, min, max, offset, size)` → `Range: bytes=offset-(offset+size-1)`). The frame is decompressed and the page is added to the cache.
- [V] There is no read-ahead or batching of neighbouring pages. A sequential scan issues one GET per uncached page. The guide: "Sequential scans across large tables will generate sustained remote reads; indexed lookups and repeated queries benefit most from caching."
- [V] Retries: up to 6 attempts with a linear backoff of 15 ms × attempt. Retryable errors are context deadline or cancel, unexpected EOF, and **not-found** (for example a file deleted by compaction). After the last retry the VFS returns `SQLITE_BUSY`. Other errors fail at once (`isRetryablePageError`).
- [V] A page missing from the index returns the error "page not found" in read-only mode, and zeros in write mode (a new database).
- [V] **Header rewrite for page 1.** On every read at offset 0 the VFS sets bytes 18 and 19 (the file format read and write versions) to `0x01` and fills bytes 24 to 27 (the file change counter) with random bytes. The docs: "rewrites the header so SQLite reports `DELETE` journal mode". The VFS also reports `SectorSize` 0 and no device characteristics.
  - [V, SQLite file format doc https://www.sqlite.org/fileformat2.html] Values 1 at offsets 18 and 19 mean legacy rollback-journal mode, and 2 means WAL. The change counter at offset 24 is what other connections use to detect that the file changed and that their page cache is stale.
  - [I] A random counter makes SQLite treat the file as changed at the start of each read transaction and drop its own page cache, so pages that changed through polling are read again from the VFS. Issue #1608 is consistent with this: a hydrated replica kept its snapshot only while the table fit in SQLite's page cache.
- [V] Temporary files (journals, temp databases) are local files in a private directory created by the VFS (`openTempFile`). `Access` always reports that no `-wal` file exists, so SQLite never tries WAL mode on the VFS.

### Performance notes
- [V] "Placing the VFS client in the same region as the replica minimizes added latency (typically 5–50ms over local disk)." (how-it-works/vfs)
- [V] Blog: "Most databases have a small set of 'hot' pages — inner branch pages or the leftmost leaf pages for tables with an auto-incrementing ID field."
- [V] Guide cache sizing: 5 to 10 MB for databases under 100 MB, 10 to 50 MB for 100 MB to 1 GB, 50 to 100 MB or more above that.

## 8. Following new transactions [V: `monitorReplicaClient`, `pollReplicaClient`, `pollLevel`, `Lock`, `Unlock`]

- **Polling:** every `PollInterval` (default 1 s) the VFS lists L0 from its position and L1 from its last L1 TXID. For each new file it fetches the page index and header (2 or 3 range GETs). It never downloads page data at poll time.
  - Blog: "Because Litestream backs up (into the L0 layer) once per second, the VFS code can simply poll the S3 path, and then incrementally update its index. The result is a near-realtime replica."
- **L0 first, L1 as fallback:** a gap at L0, for example L0 files already deleted, is logged ("ltx gap detected at L0, deferring to higher levels") and L1 fills it. A gap at L1 is an error ("non-contiguous ltx file"). There is extra logic for an L1 file that straddles the last known L1 TXID (#1460, fixed).
- **Shrinking database:** if a new file's `Commit` is smaller than the previous one, for example after `VACUUM`, the whole index is replaced instead of merged (`replaceIndex`).
- **Two-index snapshot isolation:** when the connection holds a SHARED lock or higher (a read transaction is open), new entries go into a `pending` map instead of the main `index`. On `Unlock` to SHARED or NONE, pending is merged into the index (or replaces it) and the changed page numbers are evicted from the LRU cache. Docs: "a read transaction sees a consistent snapshot of the database as it existed when the transaction started."
  - Cost: pending entries grow with write rate × transaction length, and the docs give "~60,000 entries" for 1,000 writes/s over 60 s. "Currently, there is no direct PRAGMA to observe pending index size."
- **Cache invalidation:** when no reader is active, the changed pages are evicted from the cache immediately at poll time.
- **Staleness:** a reader is behind the primary by up to the primary's monitor and upload intervals (1 s + 1 s by default) plus the VFS poll interval (1 s) plus request latency. `litestream_lag` reports only the seconds since the last successful poll, not the distance to the primary. [V for the components, I for the sum]
- **Request cost of following:** at least 2 LISTs per second per open database, plus 2 to 3 GETs per new file. Each S3 LIST covers the whole level prefix (§4). [V source, I arithmetic]
- **Time travel pauses polling:** the poll tick is skipped while a target time is set.

## 9. Hydration [V: `Hydrator` in `vfs.go`, hydration guide, writable-VFS post]

- **Purpose:** "restores the entire database to a local file in the background while the VFS continues serving reads from the cache and remote storage. Once hydration completes, all reads are served from the local file". The blog says the trick was "shoplifted" from systems like `dm-clone`.
- **Mechanism:** `Hydrator.Restore` opens every file of the restore plan as a full GET stream and pipes them through `ltx.Compactor`, which keeps the newest version of each page. `ltx.Decoder.DecodeDatabaseTo` writes the result to the local file. Memory stays bounded ("A 10GB database can be hydrated with the same memory footprint as serving it read-only"). `CatchUp` then applies whole L0 files between the hydrated TXID and the current one. After that, `SetComplete` is called and the LRU cache is purged.
- **Steady state:** each poll applies the new winning pages to the hydrated file through `Hydrator.ApplyUpdates`, with **one range GET per changed page** under the hydrator mutex. Issue #1387 (open) reports "~600x request amplification and reader stalls on write-heavy workloads".
- **Persistence (v0.5.9):** with an explicit `LITESTREAM_HYDRATION_PATH`, the file survives restarts. A `<path>.meta` file holds the TXID and is written on clean close with temp file + fsync + rename + directory fsync. On open, if both files exist, hydration resumes from that TXID. If the remote TXID is behind the saved one ("TXID regression", for example after a reset), the file is truncated and restored from scratch. A `.meta` without a database file is deleted.
  - Issue #1614 (open, 2026-11-01): "VFS hydration saves a stale applied TXID on graceful close".
- **Without an explicit path** the hydration file is a temp file, deleted on close. Blog: "We can't trust that the database is using the latest state every time we start up, not without doing a full restore, so we just chuck the hydration file when we exit the VFS."
- **Time travel** disables hydrated reads: time-travel reads come from remote LTX files.
- **Isolation gap:** after hydration, reads go straight to the local file, and poll updates are written into it even while a read transaction is open. Issue #1608 (open, 2026-10-30): "Hydrated VFS does not preserve repeatable reads within an explicit read transaction". [V issue; the code path matches]

## 10. Time travel / point-in-time queries [V: `SetTargetTime`, `ResetTime`, `rebuildIndex`]

- `PRAGMA litestream_time = '<time>'` runs `CalcRestorePlan(timestamp)`, which uses only files created strictly before the time and reads the timestamps from object metadata. It then builds a fresh index from that plan and swaps it in under the mutex, purging the cache. `'latest'` rebuilds from the latest plan.
- It is per connection (per VFS file). Polling is skipped while a target time is set.
- How far back you can go depends on retention: "Time travel requires `l0-retention` to be configured on the primary so historical LTX files remain available." In general, the reachable times are the boundaries of the files that still exist (§3).
- Blog: "instantaneous point-in-time recovery (PITR), expressed simply in SQL and SQLite pragmas", and "Litestream holds backup files for every state your database has been in, with single-second resolution, for as long as you want it to."
- [I] "Single-second resolution" holds only while L0 files are retained. With the defaults, that is 5 minutes after their compaction into L1. After that, resolution is 30 s (L1 boundaries) until snapshot retention removes the L1 files.

## 11. Write mode [V: `vfs.go` write path, write-mode guide, writable-VFS post]

- **Why it exists:** Fly.io Sprites keep a block map in a SQLite database. After a cold start "its storage stack needs to serve writes, milliseconds after booting, without having a full copy of the 10MB block map." (writable-VFS post) The Sprites storage stack later moved to SBD, a block device with metadata in the bucket (SBD post, 2026-10-22). [V]
- **Enabling:** `LITESTREAM_WRITE_ENABLED=true`, the URI parameter `write_enabled`, or at runtime `PRAGMA litestream_write_enabled = 1`. The VFS always reports `OpenReadWrite` to SQLite so that this runtime switch works, and enforces read-only mode itself in `WriteAt`, `Truncate` and `Lock`.
- **Write buffer:** `WriteAt` reads the whole page (from the buffer, the cache or remote), applies the write, and stores the page in a local buffer file. A `dirty` map holds page number → buffer offset. A rewritten page is overwritten in place, and a new one is appended. The buffer holds raw pages with no header.
- **Sync:** every `syncInterval` (default 1 s) and on close, if there are dirty pages and no write transaction is open (`inTransaction` is set at RESERVED and cleared on unlock), `syncToRemoteWithLock` does this:
  1. `checkForConflict`: LIST L0 from `expectedTXID`. If any file's `MaxTXID > expectedTXID`, it returns `ErrConflict` ("remote has newer transactions than expected").
  2. It encodes all dirty pages, sorted and without the lock page, into one LTX file `pendingTXID-pendingTXID` at **L0** (with `HeaderFlagNoChecksum`), streamed through a pipe.
  3. `WriteLTXFile`: a **plain PUT with no condition** (no `If-None-Match`).
  4. It advances `expectedTXID` and `pendingTXID`, merges the new index entries, copies the pages to the cache (and to the hydrated file), clears `dirty` and truncates the buffer.
- **Group commit:** all local transactions committed during one interval become one remote TXID. [V code; I name]
- **SQLite's xSync** is a no-op during a transaction. Commits return as soon as the pages are in the local buffer. [V]
- **Durability promise:** "Nothing written through the VFS is truly durable until that sync happens." "All storage on a Sprite shares this 'eventual durability' property ... They probably don't make sense for your application." (blog) The guide: "Uncommitted changes in the buffer since the last sync may be lost on crash".
- **Crash recovery, docs vs. code:**
  - The docs and guide say the buffer "persists uncommitted changes. On restart, buffered writes can be recovered and synced."
  - The code truncates the buffer on open: `initWriteBufferWithLock` opens it with `O_TRUNC`, commented "Any existing buffer content is discarded since unsync'd changes are lost on restart". Test `TestVFS_WriteBufferDiscardedOnOpen` asserts that "unsynced data is lost after crash".
  - The buffer is never fsynced.
  - So at `af28505`, writes not yet uploaded are lost on any crash. [V code; the docs disagree]
- **Single writer:**
  - **In a process:** the `VFS` struct holds `writeFile`, the one file allowed RESERVED. A second connection gets `SQLITE_BUSY`. `lastSyncedTXID` per replica URL keeps pooled connections in step (#1140, #1544).
  - **Across processes:** only the conflict check. "Multiple concurrent writers trigger conflict detection (not prevention). Applications must handle conflicts." Suggested remedies are external locking, partitioning by writer, or retry.
  - [I] The check (LIST) and the upload (unconditional PUT) are separate steps. Two writers that both pass the check would write the same key `0000/<txid>-<txid>.ltx`, and the last PUT wins, silently losing the other's pages.
- **Polling in write mode:**
  - The blog says "the VFS in write-mode disables polling".
  - At `af28505`, `Open` starts `monitorReplicaClient` regardless of write mode, and the conflict check lists L0 on every sync. [V code; the blog disagrees]
- **New databases:** with write mode on and no remote files, the VFS starts empty with 4096-byte pages and TXID 0. The first sync uploads TXID 1. A missing page reads as zeros.
- **Compaction from the VFS:** with `CompactionEnabled` (Go API only), a write-mode VFS runs the same `Compactor` levels, snapshots and L0 retention as the daemon (`startCompactionMonitors`). Issue #1459 (open): the VFS keeps no local LTX copies, so a lost L0 object cannot be re-uploaded.

## 12. Preventing two writers: leases [V: `leaser.go`, `s3/leaser.go`, `docs/ARCHITECTURE.md`, issues]

- **Design intent** (Revamped post, 2025-05-20): "With conditional writes, we can implement a time-based lease", giving "essentially the same constraint Consul gave us" for LiteFS, so ephemeral nodes with overlapping lifetimes cannot corrupt each other's replicas.
- **Implementation** (v0.5.8, #1073): `Leaser` interface with `AcquireLease`, `RenewLease` and `ReleaseLease`. The S3 version:
  - The lease object is `<path>/lock.json`, JSON `{generation, expires_at, owner}`. The owner is `hostname:pid`, and the TTL defaults to 30 s.
  - **Acquire:** GET the lease. If it exists and has not expired, return `LeaseExistsError{Owner, ExpiresAt}`. Otherwise PUT a lease with `generation+1`, using `If-None-Match: *` if no object existed or `If-Match: <etag of the expired lease>`. A 412 means another process won.
  - **Renew:** PUT with `If-Match: <own etag>`. A 412 → `ErrLeaseNotHeld`.
  - **Release:** `DeleteObject` with `If-Match: <own etag>`. A 412 → `ErrLeaseNotHeld`, a 404 → `ErrLeaseAlreadyReleased`.
- **Not enforced anywhere:**
  - At `af28505` nothing outside tests calls `AcquireLease`. Neither `litestream replicate` nor the VFS takes the lease.
  - A maintainer wrote on 2026-02-16 (#1119): "Litestream doesn't currently coordinate between instances ... We have distributed leasing infrastructure merged (#1073) but the integration into the replication loop (so it's actually enforced) is still pending."
  - Issue #1301 (open, 2026-06-08) asks to "Expose distributed leasing in replicate -exec lifecycle".
- The tips page still says: "Multiple applications replicating into the same bucket & path can cause situations where you will be unable to restore. It is _your_ responsibility to ensure you do not have multiple applications replicating concurrently."
- [I] The lease also has no fencing token on the data path: LTX PUTs are unconditional and carry no generation. So even when held, a lease would exclude a stale writer only through timing (a TTL longer than a pause), not by rejecting its writes.

## 13. What consistency a VFS reader gets

- [V] **Within one read transaction** (non-hydrated): a snapshot as of the start of the transaction, through the pending index (§8). It is also bounded by retention: if a file the snapshot needs is deleted mid-transaction, page fetches retry on not-found and then return `SQLITE_BUSY` (§7). [V code, I scenario]
- [V] **Across transactions:** each new transaction sees the newest state the last successful poll indexed. It is eventually consistent and lags the primary by a few seconds (§8). There is no way to ask for a read at least as fresh as a given TXID. `litestream_txid` exposes the position, so an application can compare it itself. [I]
- [V] **Atomicity of what is seen:** a reader advances only by whole LTX files. Each file ends at a WAL commit boundary (§1), so a reader never sees half a transaction. It may jump several transactions at once. [V for the commit boundary, I for the reader effect]
- [V] **Hydrated mode** breaks repeatable reads within a transaction (#1608, §9).
- [V] **Write mode:** readers in other processes see writes only after the writer's next sync and their own next poll. "Commit visibility: Local commits are immediately visible to the same connection but not to remote readers until the next sync completes."
- **Object-store consistency assumptions:** [I] Discovery is by LIST, so a reader sees a new file only once LIST returns it. The design assumes LIST is consistent with completed PUTs, as it is on AWS S3. [V] For Tigris, Litestream sets `X-Tigris-Consistent: true` (#903; see `object-storage-architectures.md`).

## 14. Documented limitations and failure modes

From the docs [V]:
- Read-only by default. Writes fail with "attempt to write a readonly database".
- CGO only. SQLite clients must support loadable extensions. macOS system SQLite disables them, and some Node libraries cannot use URI filenames.
- "Contiguous LTX coverage: Missing L0 files (e.g., aggressive retention) will error until new files appear."
- "Initial snapshot required: The VFS waits for available backup files before serving reads."
- "Network latency: First-page reads incur network latency".
- "High concurrency: designed for modest fan-out; very high reader counts will increase object-store requests."
- Long read transactions grow the pending index.
- Write mode: no distributed lock, conflicts are detected but not prevented, a crash window, and the buffer needs disk.
- Hydration needs disk for the whole database, and short-lived processes may never finish it.
- Docs: "The VFS targets read-replica workloads with moderate query load, not high-concurrency OLTP scenarios." "Choose VFS for light-weight read replicas and rapid spin-up; choose restore for low-latency, write-capable workloads on local disk."

From open issues at 2026-11-10 [V]:
- #1608: hydrated reads are not repeatable within a transaction.
- #1614: a stale TXID is saved on graceful close.
- #1387: hydration applies updates one page per request.
- #1459: L0 gaps cannot self-heal in a VFS writer.
- #1271: "Writable VFS: FileSize race after sync flush causes 'database disk image is malformed'".
- #1018: "VFS write buffer race condition causes 'read page from buffer: EOF' errors".
- #1513: "Writable VFS: scope write state by database name".
- #1449 (2026-08-18): "vfs: test suite is red on main and CI never runs it".
- #1391: an open manifest design and non-manifest-aware writers.

## 15. How Litestream and the VFS are tested [V unless marked]

- **Unit tests** in the root package and the backend packages, with a mock `ReplicaClient` and real SQLite ("Use Real SQLite: Avoid mocking SQLite behavior"). The race detector is used throughout ("Always run with `-race` flag"). (`docs/TESTING_GUIDE.md`)
- **VFS tests:**
  - Root package (build tag `vfs`): `vfs_test.go`, `vfs_write_test.go`, `vfs_compaction_test.go`, `vfs_config_test.go`, `vfs_link_test.go`.
  - `cmd/litestream-vfs` package: a SQLite CLI auto-extension end-to-end test, hydration end-to-end tests, time-travel tests, `vfs_write_integration_test.go` and a stress test. The write tests include conflict detection, rollback restoring the original state, a discarded buffer on open, and read-your-writes.
  - Most of these run a real Litestream `DB` replicating to a **file** replica client in a temp directory, with 15 to 75 ms intervals, and open the VFS against it (`openReplicatedPrimary`).
- **Fuzzing (Go native):**
  - `FuzzVFSReplicaReadPatterns` maps fuzz bytes to six query kinds (point read, grouped count, ordered scan with offset, range sum, LIKE). It cross-checks group counts between primary and replica after the replica catches up, and it also runs on fixed seeds in plain `go test`.
  - `FuzzRestoreWithMissingCompactedFile` (`restore_fuzz_test.go`).
  - LTX has encoder golden tests and spill tests (superfly/ltx).
- **Chaos test** (`cmd/litestream-vfs/chaos_test.go`, tags `vfs,chaos`): a wrapping `chaosReplicaClient` (seed 99) does three things while a writer inserts, updates and deletes on the primary and readers query the replica for 3 s:
  - fails 5% of LISTs with `context.DeadlineExceeded`;
  - adds a 0 to 4 ms delay to every page or file open and fails 5% of them;
  - **truncates 5% of ranged reads to half their bytes**.
  - Readers tolerate `SQLITE_BUSY`, the replica must converge to the primary's row count, and the test fails if no fault was injected.
- **Soak tests:**
  - `TestVFS_LongRunningSoak`: 5 min by default, sustained reads and writes, file backend.
  - In `tests/integration` (tags `integration,soak`): `TestComprehensiveSoak` (2 h, 50 MB, 500 writes/s, file backend), `TestMinIOSoak` (2 h against MinIO in Docker), `TestSoakReplicateRestore` (stop → restore → `integrity_check` every 30 s against MinIO, a reproduction for #1164), and `TestOvernightS3Soak` (8 h against real AWS S3).
- **Behavioural assertions from logs** (`tests/integration/ltx_assertions.go`): a soak run's log is parsed into sync, snapshot, compaction and checkpoint events. The tests then assert:
  - snapshot cadence, and no excessive snapshots;
  - "no snapshot on checkpoint", a past regression;
  - L0 files hold only a handful of pages;
  - compaction timing per level is within tolerance;
  - WAL size stays bounded, including its sampled peak.
  - A short version runs on every commit ("LTX Behavioral Gate"), and longer profiles run nightly.
- **Real object stores:**
  - Every commit: replica-client tests against `moto` (an S3 mock), a MinIO integration test, and a NATS JetStream integration test.
  - A manual workflow runs the same tests against AWS S3, GCS, Azure Blob, Tigris and Cloudflare R2. `docs/PROVIDER_COMPATIBILITY.md` records provider quirks. For example, Tigris gets `sign-payload=true`, `require-content-md5=false`, `concurrency=2` and `part-size=16MiB`. Issues #903 and #1278 found Tigris problems during soak testing.
  - Integration tests also cover upgrades and compatibility (v0.3 to v0.5 migration, binary compatibility), the 1 GB lock-page boundary, different page sizes, and an S3 restore with dropped connections.
- **Nightly:** a race-detector sweep (`go test -race -count=3 ./...`), LTX behavioural soaks (30 min on weekdays, 2 h on Saturdays), a snapshot regression test and a MinIO soak.
- **Not found:** no deterministic simulation, no Antithesis, no model checking (TLA+ or similar), and no Jepsen-style multi-writer tests. A search of the repo for these terms finds nothing.
- **Gap:** CI's "VFS Unit Test" job runs `go test -tags vfs -race -count=1 .` on the root package only. Issue #1449 says the `cmd/litestream-vfs` suite was "red on main" with data races, and that "no CI job runs the VFS test suite — the VFS jobs only compile the extension."

## 16. What the authors report as hard

- [V] **Generations and lost WAL continuity** (Revamped): when Litestream is down it can miss changes and must start a new generation. Restoring by replaying raw WAL pages is slow for busy databases. Many generations make failover and read replicas hard. The fix was one writer per destination, enforced by a lease (which is still not wired in, §12).
- [V] **FUSE and Consul** (Revamped): LiteFS needed a FUSE filesystem ("a lot to ask of users") and Consul for leases. LiteVFS showed that FUSE is not needed.
- [V] **Writes in the VFS** (Writable VFS post): "Writes are harder." "multiple-writer distributed SQLite databases are the Lament Configuration and we are not explorers over great vistas of pain."
- [V] **Remote reads are slower:** "this approach isn't as efficient as a local SQLite database" (Revamped). For a cold start, reading from object storage "is a godsend ... but it's not fast enough for steady state" (Writable VFS post), which is why hydration was added.
- [V] **Retention racing readers:** the config reference says `l0-retention` exists to prevent "race conditions where L0 files could be deleted before VFS has time to fetch newly created L1 files."
- [V] **The SQLite lock page at 1 GiB** gets its own section in the testing guide.
- [I] From the issue list, recurring trouble spots are: gaps and straddling files at level boundaries (#1459, #1460), races between the sync goroutine and SQLite callbacks in write mode (#1018, #1271), and keeping the hydrated file consistent with the index (#1387, #1608, #1614).

## 17. Discrepancies between docs, blog and code (at `af28505`)

- **Write buffer crash recovery:** the docs say buffered writes "can be recovered and synced". The code discards the buffer on open and never fsyncs it (§11).
- **Polling in write mode:** the blog says write mode "disables polling". The code polls (§11).
- **`ltx/` prefix:** the docs and the v0.5.0 post say files sit under an `ltx/` prefix. S3 keys have none (§4).
- **Lease:** the Revamped post presents the lease as the way to keep one writer. The code ships the primitive only, and it is not enforced (§12).
- **Checksums:** the how-it-works page says Litestream "stores checksums alongside the pages". Only the per-file CRC is stored. Database checksums are disabled by `HeaderFlagNoChecksum` (§2).

## 18. Looked for and not found

- No per-page checksum or verification on single-page range reads (§2).
- No read-ahead, prefetching or batching of page fetches. The Revamped post mentioned "caching and prefetching", but the code fetches one page per request.
- No conditional PUT on any LTX upload. Conditional writes appear only in `s3/leaser.go`.
- No fencing token tying LTX files to a lease generation.
- No documented end-to-end latency figure from primary commit to VFS visibility, and no published benchmark of cold-start time or request counts.
- No VFS for non-SQLite consumers. The VFS needs a CGO SQLite (mattn) and does not work with the pure-Go `modernc.org/sqlite` that the daemon uses.
