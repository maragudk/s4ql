# SQLite as local cache and as correctness oracle

Compiled 2026-11-10. This maps the design space for the idea "target SQLite compatibility, and use SQLite both for correctness testing and for the local cache". It does not recommend an architecture until the last section.

Legend:
- **[V]** verified from a primary source: SQLite source or documentation, a project's repository, docs, issue tracker, or release notes. Also used for results measured in the scratch experiments described in §0.2.
- **[I]** inferred.

Sources were read at these versions:
- SQLite: latest release 3.54.1 (2026-10-16). The GitHub mirror trunk is at `8d769ae` (2026-11-10, `VERSION` = 3.55.0). [V https://sqlite.org/changes.html, https://github.com/sqlite/sqlite]
- Go bindings and prior art: see §4 and §5, which give each version.

Two delegated research agents gathered most of §4 (Go bindings) and §5 (prior art). I spot-checked their decisive claims: ncruces's mandatory Go VFS, modernc's hook API, and rqlite #1338. Everything else in those sections carries the agents' tags.

---

## 0. Ground rules and method

### 0.1 What "without replacing SQLite's storage layer" excludes
The brief excludes three things: a custom or shim VFS, WAL shadowing, and reading SQLite's journal or WAL files. Three mechanisms sit on the boundary, and each place that uses them flags it:
- **Virtual tables** (`sqlite3_create_module`). This is SQLite's documented extension point for table storage. It replaces the storage of the tables it serves, but not the VFS. See architecture V in §1.
- **`sqlite_dbpage`**. This reads and writes raw pages through the pager, inside transactions. It is an SQL interface, not a VFS. See §2.8.
- **`sqlite3_deserialize`**. This uses SQLite's built-in `memdb` VFS, not one of ours. See §2.6.

### 0.2 Scratch experiments (measured, not taken from docs)
- **Setup:** SQLite 3.54.1, the Homebrew build on macOS arm64, compiled with `SQLITE_ENABLE_SESSION`, `PREUPDATE_HOOK`, `DBPAGE_VTAB` and `DBSTAT_VTAB` (checked with `sqlite_compileoption_used`).
- **Harness:** about 100 lines of C, used only for these experiments and not project code. It does the following steps in order:
  1. Opens database A and database B, both with the same schema.
  2. Runs `BEGIN IMMEDIATE` on A.
  3. Creates a session on all tables, with `SQLITE_SESSION_OBJCONFIG_ROWID` on.
  4. Runs a workload script.
  5. Calls `sqlite3session_changeset()` while A's transaction is still open.
  6. Runs `COMMIT` or `ROLLBACK` on A.
  7. Applies the changeset to B with `sqlite3changeset_apply_v2`. The conflict handler logs each conflict and returns OMIT.
- **Comparison:** the two databases are compared with `.dump --preserve-rowids`. That dump includes `sqlite_sequence`, `sqlite_stat*`, FTS5 shadow tables and hidden rowids.
- **Second program:** a separate C program prints the events of the pre-update, commit and rollback hooks.
- **Status:** these are single runs on one machine. Timing numbers are indicative only.

---

## 1. The design space

Every architecture below keeps our own format in S3 as the source of truth. "Rebuild" means making a lost or stale local cache current again.

### A. Stock SQLite executes all SQL against a local file; S3 holds a logical change log

The application's SQL runs on stock SQLite against a normal local database file in WAL mode. A commit coordinator does the following in order:
1. Captures the transaction's row changes.
2. Uploads them as the next log entry.
3. Swaps the manifest with a conditional write.
4. Lets SQLite commit locally only after step 3 succeeds.

The local file is a materialization of the log. "SQLite compatibility" here means that the SQL engine *is* SQLite. Compatibility gaps come only from what the change-capture mechanism cannot carry, and from features we choose to forbid. Variants A1 to A6 differ in what goes to S3 and how changes are captured.

#### A1. Session changesets plus SQLite-file checkpoints
- **Executes reads and writes:** stock SQLite. Reads never touch S3.
- **Capture:** one session object per write transaction, attached to all tables (`sqlite3session_attach(s, NULL)`). The changeset is taken before `COMMIT` (§2.1, §3).
- **DDL:** shipped as statement text in the same log entry (§2.1.4).
- **In S3:**
  - Log entries, each holding a changeset blob, any DDL text, the LSN and metadata.
  - The manifest.
  - Periodic checkpoints that are **SQLite database files**: produced by `VACUUM INTO` or a consistent page read, chunked, and optionally deduplicated by page hash, as `sqlite3_rsync` does (§2.8).
- **Rebuild:**
  - Full: download the latest checkpoint file, then apply the changesets after it.
  - Incremental: apply the missing log entries to the existing file.
  - Lazy: impossible. SQLite needs the whole file locally unless a VFS fetches pages, and a VFS is excluded. [I]
- **Compatibility:**
  - Everything stock SQLite does, including FTS5 and R\*Tree, which persist in real shadow tables and are captured (§2.1.3).
  - Minus: tables without an `INTEGER PRIMARY KEY` or `WITHOUT ROWID` key (hidden rowids diverge), NULL primary keys, `AUTOINCREMENT` sequence state, `ANALYZE` statistics (unless DDL-like handling is added), virtual tables that are not shadow-table backed, mixing some DDL with DML in one transaction, and `VACUUM` between checkpoints. See §2.1.3 for the measured cases.
  - Most of these can be closed by policy (reject at DDL time) or by extra capture (diffing `sqlite_sequence` and `sqlite_stat1` at commit). [I]
- **Engine code to write:** no SQL engine. The parts to write [I]:
  - the commit coordinator and lease
  - the S3 log, manifest and checkpoint formats
  - DDL capture and the DDL policy
  - the catch-up applier
  - checkpoint upload and download
  - a verifier
  - a `database/sql` or custom API wrapper that owns `BEGIN`/`COMMIT`
  
  Order of magnitude: thousands to low tens of thousands of lines of Go.
- **Main correctness risks:**
  - Silent gaps in capture: apply returns `SQLITE_OK` while dropping rows whose table is missing or incompatible (§2.1.3, E5).
  - Replay that is not identical (rowids, sequences).
  - The checkpoint format is SQLite's file format, so the S3 format is partly SQLite's, not ours. This needs an explicit decision. [I]

#### A2. Session changesets plus logical checkpoints in our own format
- Same as A1, but a checkpoint is our own row-level snapshot: per-table sorted row files. The S3 format is then entirely ours and contains no SQLite pages. [I]
- **Rebuild:**
  - Full: bulk-load every table, which is slower than downloading a file because indexes must be rebuilt.
  - Measured indication: 1,000,000 rows of about 100 bytes, with one secondary index, loaded in 2.8 s, a 136 MB file. At that rate a 5 GB database is roughly 100 s on this machine. [V measured, I extrapolated]
  - Incremental: as A1.
  - **Lazy at table granularity** is possible. `sqlite3_set_authorizer` reports every table and column a statement reads, at prepare time. A table that has not been hydrated yet can be fetched and loaded before the statement runs. [V authorizer API exists, I design]
- **Compatibility:** as A1. Hidden rowids and `sqlite_sequence` must be carried in the snapshot explicitly. [I]
- **Engine code:** A1 plus a snapshot writer and loader. **Risk:** as A1, plus the cost of the bulk load at cold start.

#### A3. Statement log (rqlite-style)
- **Executes:** SQLite. **In S3:** the SQL text and parameters of each write transaction, plus checkpoints.
- **Rebuild:** replay statements.
- **Compatibility:** every statement must be deterministic on replay. SQLite's own non-deterministic built-ins are `random()`, `changes()`, `last_insert_rowid()`, `sqlite_version()`, and date/time functions that use `'now'`, `localtime` or `utc`. [V https://sqlite.org/deterministic.html] Also:
  - rowid choice is random once the maximum rowid is reached [V https://sqlite.org/autoinc.html];
  - `INSERT ... SELECT` without `ORDER BY` assigns rowids in plan order, and the plan depends on statistics and version [I];
  - triggers, defaults and generated columns can contain non-determinism [I].
- **rqlite's experience** [V via agent]:
  - It rewrites `RANDOM()` and `'now'` in the AST, but not inside `ORDER BY`, DDL, defaults, triggers or views.
  - It silently passes statements it cannot parse.
  - It has had recent rewriter bugs (#2592, #2965).
  - Its author has an open proposal (#1338, since 2023-07-11) to replicate session changesets instead: "No more concerns about RANDOM, TIMESTAMP, and AUTOINCREMENT".
- **Engine code:** an SQL rewriter (a parser of the SQLite dialect). **Risk:** high and open-ended. [I]

#### A4. Trigger-captured change tables (cr-sqlite or PowerSync style capture)
- **Executes:** SQLite. **Capture:** generated `AFTER INSERT/UPDATE/DELETE` triggers write row images into a change table inside the same transaction. The coordinator reads and clears that table before `COMMIT`.
- **Pros:**
  - Works through plain `database/sql` with any binding, because no session API is needed.
  - Statement-level and savepoint rollbacks undo the change rows too. [I]
- **Cons:**
  - Doubles write I/O: cr-sqlite reports inserts "2.5x slower" [V via agent].
  - Triggers cannot exist on virtual tables [V https://sqlite.org/vtab.html], so FTS5 needs capture on its shadow tables.
  - Every DDL must regenerate the triggers.
  - Trigger recursion settings interact with application triggers. [I]
- **Engine code:** a trigger generator, plus A1's parts. **Risk:** trigger maintenance across schema changes. cr-sqlite needs `crsql_begin_alter`/`commit_alter`. [V via agent]

#### A5. Our own capture through `sqlite3_preupdate_hook`
- **Executes:** SQLite. **Capture:** a Go callback records row images as they change. Marmot v2 does this [V via agent].
- **Pro:** the full change is in Go memory before `COMMIT`. The S3 step can therefore run inside the commit hook, which cannot run SQL (§3), so autocommit statements work without rewriting.
- **Deal-breaker unless undo is modelled:**
  - The hook fires for changes that are later undone by a statement abort or by `ROLLBACK TO`, and SQLite gives no callback for either undo (measured, §2.2).
  - A capture built on the hook alone records changes that never happened.
  - The session extension avoids this because it rebuilds the changeset from the database's final state (§2.1.1).
- **Fix:** track SQLite's statement and savepoint boundaries, or combine the hook with a final-state read, which is what the session extension already does. [I]

#### A6. A1 or A2 with several writers (OCC on changesets)
- **Context:** `docs/spec.md` says "Any process may write; there is no designated writer". The brief describes one lease holder.
- **How it would work:** a process that does not hold the lease executes on its own replica, which may be stale. It then applies its changeset to the latest state with a conflict handler that **aborts on any conflict**. DATA, NOTFOUND and CONFLICT then act as row-level write-write conflict detection. [V conflict semantics §2.1.2, I design]
- **Limit:** SQLite exposes no row-level read set, so this gives at most snapshot-isolation-like checking. The authorizer gives a table and column read set at prepare time, which allows serializable checking only at table granularity. [I]
- **Note:** this tension between spec and brief has to be resolved for every architecture in family A. [I]

### V. Stock SQLite executes SQL over our storage through virtual tables (boundary case)
- **Executes:** the SQLite planner and VDBE. Our Go module serves rows from our own storage, cached locally in our own files.
- **In S3:** entirely our own format. **Rebuild:** lazy at any granularity we choose.
- **Commit:** `xSync` runs on every virtual table before any `xCommit`, and "If any of the xSync methods fail, the entire transaction is rolled back". This is a natural hook for the S3 step. [V https://sqlite.org/vtab.html]
- **Compatibility is poor** [V https://sqlite.org/vtab.html]:
  - "One cannot create a trigger on a virtual table."
  - "One cannot create additional indices."
  - No `ALTER TABLE ... ADD COLUMN`.
  - The declared schema's constraints "are ignored", so we must enforce NOT NULL, UNIQUE, CHECK and FOREIGN KEY ourselves, plus affinity and defaults.
  - "SQLite does not enforce the PRIMARY KEY for a WITHOUT ROWID virtual table."
  - Applications would write `CREATE VIRTUAL TABLE`, or we would rewrite their DDL.
- **Performance:** each row crosses the module boundary (cgo or transpiled calls). [I]
- **Engine code:** a storage engine with secondary indexes exposed through `xBestIndex`, plus constraint enforcement. **Risk:** that layer re-implements half of SQLite's table semantics and can diverge silently. This arguably also "replaces SQLite's storage" for these tables. [I]

### B. Our engine executes SQL; SQLite is at most a container for cached data
#### B1. Our engine; local cache stored inside SQLite files
- Our Go engine parses, plans and executes the SQLite dialect over our own storage format. The local cache (pages, SST blocks or rows) is kept in SQLite tables, such as `blocks(key PRIMARY KEY, data BLOB)`, to get crash-safe local transactions.
- **In S3:** our format. **Rebuild:** anything we design, including lazy fetch at block level.
- **Compatibility:** only as complete as our engine. Stock SQLite is the test oracle.
- **Engine code:** a complete SQL engine. For scale:
  - The SQLite library is 155.8 KSLOC of C (as of 3.42.0) [V https://sqlite.org/testing.html].
  - Turso, a Rust rewrite of SQLite, started 2023-08, has about 390 contributors, and as of 2026-11 still "tracks SQLite version 3.50.4" and is doing "ongoing work to pass the full SQLite TCL test suite" [V https://github.com/tursodatabase/turso COMPAT.md, GitHub API].
- **Risk:** semantic divergence everywhere: affinity, collation, NULL handling, printf and float formatting, date functions, JSON, FTS5. SQLite as the cache adds little over plain files. Graft's author found an embedded store used as a page cache to be "significant overhead" (graft #294, see `graft.md`). [I]

#### B2. Our engine; our own cache files; SQLite only as test oracle (control case)
This is B1 without SQLite in production. It is listed because it is the "SQLite for correctness testing" half of the idea taken alone. Its code size and risk are the same as B1's, minus the SQLite dependency. [I]

### H. Hybrids
#### H1. SQLite executes reads; our engine executes writes and applies row operations to SQLite and S3
- Our engine would compute DML effects itself: constraints, conflict clauses, upsert, triggers, FK actions, defaults, affinity, generated columns and `RETURNING`.
- **Risk:** this re-implements the write half of SQLite, and any divergence corrupts the cache relative to SQLite's own semantics. If our engine instead lets SQLite compute the write's effects, H1 collapses into A. [I]

#### H2. Dual execution as a production oracle
- Either A with a shadow execution of our own engine, or B with a shadow SQLite replica that applies the same transactions. The results or resulting states are compared at runtime.
- **Cost:** two engines, and twice the work on writes. **Value:** divergence detection on real traffic. This is useful as a staging mode, not as the product. [I]

### Summary table [I unless marked]

| Arch | Executes SQL | In S3 | Cache rebuild | Engine code we write | Main risk |
|---|---|---|---|---|---|
| A1 | stock SQLite | changeset log + SQLite-file checkpoints | full download + log tail; no lazy | coordinator, formats, DDL capture, applier | capture gaps (silent drops), rowid/sequence divergence; S3 format partly SQLite's |
| A2 | stock SQLite | changeset log + own logical snapshots | bulk load + tail; lazy per table via authorizer | A1 + snapshot loader | as A1 + slow cold start |
| A3 | stock SQLite | statement log | replay | A1 + SQL rewriter | non-determinism, never complete (rqlite) |
| A4 | stock SQLite | trigger-captured rows | as A1/A2 | trigger generator | trigger and schema maintenance; 2x writes |
| A5 | stock SQLite | pre-update-hook rows | as A1/A2 | capture + undo modelling | records undone changes (measured) |
| A6 | stock SQLite, many writers | as A1/A2 | as A1/A2 | + conflict-abort merge | no read sets: SI at most |
| V | SQLite VDBE over our vtabs | our format | lazy, any granularity | storage + indexes + constraints | lost triggers/indexes/constraints; boundary of "no storage hooking" |
| B1/B2 | our engine | our format | anything, lazy | a whole SQL engine | semantic divergence; years of work (Turso) |
| H1 | SQLite reads, ours writes | our format | as A | DML semantics | write-semantics divergence |
| H2 | both | either | either | both | cost |

---

## 2. Mechanisms for learning what a transaction changed and applying it elsewhere

### 2.1 Session extension (changesets and patchsets)
**Source:** `ext/session/sqlite3session.h` at trunk `8d769ae`, and https://sqlite.org/sessionintro.html (updated 2025-05-31). **Build flags:** `-DSQLITE_ENABLE_SESSION -DSQLITE_ENABLE_PREUPDATE_HOOK`, off by default. [V]

#### 2.1.1 Capture semantics
- **Built on the pre-update hook.** An application cannot also register a pre-update hook on a connection that has sessions, and the reverse is also not allowed. [V header]
- **How records accumulate:**
  - "For each unique primary key value, data is only recorded once": the original values, recorded the first time the row is touched.
  - When the changeset is generated, "the requested changeset is created using both the accumulated records and **the current contents of the database file**".
  - So an insert followed by a delete yields nothing, and a delete followed by a reinsert yields an UPDATE. [V header]
- **Consequence:** rolled-back statements and savepoints leave no trace in the changeset, because generation reads the final state. [I from V semantics, consistent with E1]
- **Generated before COMMIT:** `sqlite3session_changeset()` works inside an open transaction and sees that connection's uncommitted changes. Measured in E1: the changeset was taken inside an open transaction, A was then rolled back, B applied the changeset, and B ended in the post-transaction state. [V measured]
- **Ordering:** changes are grouped per table, in attach order. "The order in which the changes related to a single table are stored is undefined." [V header]
- **Primary key changes:** an UPDATE of a PK column is recorded as DELETE plus INSERT. [V header]
- **Patchset:** omits old values, so it is smaller. No DATA conflicts can be detected, and it cannot be inverted. [V header]
- **Other functions:**
  - `sqlite3session_changeset_size()` gives an upper bound, but only when it is enabled with `SQLITE_SESSION_OBJCONFIG_SIZE` before the first attach.
  - `sqlite3session_memory_used()` reports the heap use.
  - The `_strm` variants stream changesets. [V header]
- **Sticky errors:** an internal failure sets a sticky `pSession->rc` that `changeset()` then returns. Example: `SQLITE_SCHEMA` after a column count shrinks. [V `sqlite3session.c` `sessionPreupdateOneChange`, `sessionReinitTable`]

#### 2.1.2 Apply and conflict semantics [V header]
- **Savepoint:** `sqlite3changeset_apply`, `_v2` and `_v3` (`_v3` added in 3.51.0, 2025-11-04) wrap all changes in a savepoint. `SQLITE_CHANGESETAPPLY_NOSAVEPOINT` removes it.
- **Compatible table:** same name, at least as many columns, PK columns in the same positions. Columns are matched **by position, not name**.
- **Incompatible or missing table:** "it is not an error, but none of the changes associated with the table are applied". Only an `sqlite3_log()` warning is issued.
- **Conflict types:**
  - DELETE: requires the old PK and all old non-PK values to match. Otherwise DATA or NOTFOUND.
  - UPDATE: requires the PK and the modified columns' old values to match.
  - INSERT: a PK clash is CONFLICT. Another constraint failure is CONSTRAINT.
  - Deferred FK violations are reported once as FOREIGN_KEY.
- **Handler returns:** OMIT, REPLACE (only for DATA or CONFLICT) or ABORT. ABORT rolls back the whole apply.
- **Flags:** `INVERT`, `IGNORENOOP`, `FKNOACTION` (FKs behave as `NO ACTION` during the apply), `NOUPDATELOOP`.
- **Rebaser:** `sqlite3rebaser_*` is experimental and rebases a local changeset over remote conflict resolutions.
- **Changegroups:** `sqlite3changegroup_*` concatenate changesets. 3.53.0 (2026-04-09) added "interfaces ... to add changes one at a time". [V https://sqlite.org/changes.html]
- **Apply does not disable triggers.** The code only sets `PRAGMA defer_foreign_keys = 1` around the apply (`sqlite3session.c` lines 5724 and 5862). [V code]
- **A divergence detector for free:** with a single writer, applying the leader's changeset to a correct replica should never conflict. A handler that ABORTs on any conflict therefore checks the cache against the log on every catch-up. [I]

#### 2.1.3 What is not captured or not reproduced: measured cases (E-numbers refer to the scratch runs)

| Case | Doc statement | Measured result |
|---|---|---|
| E1: mixed DML, `INSERT OR REPLACE` conflict delete, PK update, `randomblob()` value, WITHOUT ROWID table, table without PK (with OBJCONFIG_ROWID) | — | dumps identical; the random value was carried as data [V] |
| Tables without a declared PK | ignored, unless `SQLITE_SESSION_OBJCONFIG_ROWID` (3.42.0, 2023-05-16) treats them as having `_rowid_ INTEGER PRIMARY KEY` [V header, changes.html] | captured, keyed by rowid (E1) [V] |
| E4: table without PK, then `VACUUM` on the origin | "VACUUM may change the ROWIDs of entries in any tables that do not have an explicit INTEGER PRIMARY KEY" [V https://sqlite.org/lang_vacuum.html] | `VACUUM` renumbered rowids 4,5 to 2,3; it produced no changes; the next UPDATE got NOTFOUND on B, and with OMIT the replica silently diverged [V] |
| E14: rowid table with a non-INTEGER PK (`a TEXT PRIMARY KEY`) | NULL PK rows "ignored" [V sessionintro] | the NULL-PK row was silently absent on B, and the hidden rowid of the kept row differed (2 on A, 1 on B) [V] |
| E3: `AUTOINCREMENT` | the pre-update hook does not fire for `sqlite_sequence` [V sqlite.h.in] | insert 2, insert 3, delete 3: A's `sqlite_sequence`=3, B's=2, so a later insert after failover reuses id 3 [V] |
| E5: DDL (CREATE TABLE, ALTER ADD COLUMN, CREATE INDEX) | the session docs do not mention DDL | no DDL in the changeset; the rows for the new table and the 3-column INSERT into the altered table were **silently dropped** by apply, which returned rc=0 [V] |
| E12: `ALTER TABLE DROP COLUMN` inside the captured transaction | — | `sqlite3session_changeset` returned 17 (`SQLITE_SCHEMA`), a loud failure [V] |
| E13: `RENAME COLUMN` inside the transaction | — | data captured; schema not; applied by position [V] |
| E6: trigger writing to an audit table | trigger changes are captured as indirect [V header] | apply re-fired B's trigger and produced 2 CONFLICTs; the result happened to be identical because the ids coincided. With `SQLITE_DBCONFIG_ENABLE_TRIGGER`=0 on B there were 0 conflicts and the result was identical [V] |
| E7: `ON DELETE CASCADE` | — | B's cascade ran, then the captured child deletes got NOTFOUND (OMIT gives an identical result); with `FKNOACTION`, 0 conflicts [V] |
| E8: FTS5 | "no support for virtual tables" [V sessionintro] | the FTS5 *shadow tables* (`_content`, `_docsize`, `_data`, `_idx`; `_config` was untouched here) are real tables with PKs and were captured; dumps identical; MATCH and `integrity-check` correct on a fresh connection [V] |
| E8c: FTS5 queried on the applying connection | — | after the apply, the same connection's MATCH failed with `fts5: missing row 1 from content table` (a stale structure cache). A separate long-lived connection (E8d) returned correct results [V] |
| E14: R\*Tree | — | captured through its shadow tables; identical [V] |
| E9: `ANALYZE` | `sqlite_stat1` is captured with special handling [V header] | `sqlite_stat1` and `sqlite_stat4` were missing on B: stat1 because B has no `sqlite_stat1` table (its creation is DDL), stat4 because it is not captured [V] |
| Generated columns | supported since 3.49.0 (2025-02-06) [V changes.html] | hidden columns are skipped via `table_xinfo` [V code] |

What follows from the table [I]:
- **Logical identity holds** for ordinary rowid tables with `INTEGER PRIMARY KEY`, WITHOUT ROWID tables with non-NULL keys, FTS5 and R\*Tree, provided that:
  - DDL is shipped and replayed first;
  - the applier disables triggers and uses `FKNOACTION`;
  - the applier connection is dedicated and recycled.
- **Identity does not hold**, without extra work, for:
  - hidden rowids;
  - `sqlite_sequence`;
  - statistics tables;
  - custom virtual tables;
  - anything a `VACUUM` renumbers.
- **Closing the gaps:**
  - Policy: require every table to be WITHOUT ROWID or to have an `INTEGER PRIMARY KEY`; reject NULL-able PKs (for example with STRICT and NOT NULL); forbid `VACUUM` or treat it as a checkpoint event.
  - Extra capture: diff `sqlite_sequence` and `sqlite_stat1/4` at every commit, since they are tiny.

#### 2.1.4 DDL capture options [I unless marked]
- **Detect DDL:**
  - The authorizer reports `SQLITE_CREATE_*`, `SQLITE_DROP_*` and `SQLITE_ALTER_TABLE` at prepare time.
  - `PRAGMA schema_version` changes when the schema changes.
  - The commit hook also fires for DDL-only transactions (measured in the pre-update program). [V measured]
- **Ship DDL:** as the statement text, applied before the changeset.
- **Policy:** forbid mixing DDL with DML in one transaction, or allow only additive DDL first. Reason: column shrink fails the session (E12), and column changes are positional (E13).
- **Turso has the same class of bug in its logical sync:** #5640 "silent data corruption" after DROP and ADD COLUMN, because records are positional. [V via agent]

#### 2.1.5 Cost (E10, one run)
- **1,000,000-row INSERT (about 136 MB database):**
  - The workload took 2.82 s with the session on. Without it, a separate CLI run took 2.91 s including commit, so the overhead is small.
  - Generating the changeset took 0.34 s. The changeset was 111 MB, and the session used 65 MB of heap.
  - Applying it to B took 3.01 s.
- **100,000-row UPDATE:** 0.21 s workload, 0.10 s generation, 9.9 MB changeset, 0.46 s apply.
- **Single-row UPDATE:** 29-byte changeset, under 1 ms.
- [V measured]
- **Large transactions** cost memory proportional to the rows touched, plus a changeset about the size of the new data (§2.1.1). Streaming APIs avoid one in-memory copy. [V header, I]

### 2.2 `sqlite3_preupdate_hook` [V `src/sqlite.h.in`, measured]
- **Fires** before each INSERT, UPDATE and DELETE on real tables. It does **not** fire for virtual tables, `sqlite_sequence` or `sqlite_stat1`.
- **Values:** gives old and new values, the column count, and the trigger depth.
- **rowid arguments:** undefined for WITHOUT ROWID tables.
- **Blob writes:** `sqlite3_blob_write` appears as DELETE, with `sqlite3_preupdate_blobwrite()` set.
- **REPLACE conflict deletes:** they get an explicit no-op OP_Delete so the hook sees them (`src/insert.c` ~2398). [V code]
- **Truncate optimization:** disabled while a pre-update hook is set (`src/delete.c:476`). [V code]
- **No undo notification (measured):**
  - `INSERT INTO t VALUES(1,'a'),(2,'b'),(1,'dup')` fired the hook for rows 1 and 2, then failed with a UNIQUE error and rolled back. The hook gives no undo event.
  - Inside a transaction, `SAVEPOINT s; INSERT 6; ROLLBACK TO s` fired the hook for row 6, and nothing reported that row 6 was undone.
  - The rollback hook fires only for full-transaction rollbacks. [V measured]
- **Build tags:** compiled only with `SQLITE_ENABLE_PREUPDATE_HOOK`. Bindings differ, see §4.

### 2.3 Update, commit and rollback hooks [V `src/sqlite.h.in`, `src/vdbeaux.c`]
- **Update hook:** fires for rowid tables only. Not for WITHOUT ROWID, `sqlite_sequence`, rows deleted by REPLACE conflict resolution, or the truncate optimization. It gives only the rowid, no values. **Unsuitable for capture.**
- **Commit hook:**
  - Non-zero from the callback converts the COMMIT into a rollback (`SQLITE_CONSTRAINT_COMMITHOOK`).
  - The callback "must not do anything that will modify the database connection". Even `prepare` or `step` of a SELECT counts, so `sqlite3session_changeset()` cannot be called inside it.
  - In `vdbeCommit` the hook runs **after** each write database's `sqlite3PagerExclusiveLock` and **before** `sqlite3BtreeCommitPhaseOne` and `PhaseTwo`.
  - Deferred FK checks run before `vdbeCommit`: measured, a deferred FK violation failed the COMMIT without invoking the hook and left the transaction open (`autocommit=0`). [V measured]
- **Rollback hook:** not invoked when a connection closes with an open transaction. [V doc]

### 2.4 Backup API [V `src/sqlite.h.in`]
- Copies pages between databases with the same page size, in steps.
- The source is locked only during each step.
- A write to the source by another connection restarts the backup. A write by the same connection is propagated.
- It copies the whole file; it is not a change mechanism. **Use:** take a local checkpoint image without blocking writers for long. [I]

### 2.5 `VACUUM INTO` [V https://sqlite.org/lang_vacuum.html]
- Produces "a consistent snapshot of the original database" with "the same logical content ... fully vacuumed".
- It is not a write operation, so another connection's write lock does not block it.
- The target must not exist or must be empty. An interrupted run can leave an incomplete or corrupt output.
- **Rowids:** plain `VACUUM` may renumber rowids of tables without an INTEGER PK. Whether `VACUUM INTO` output does the same is not stated; treat it as yes. [I]
- **Measured:** 0.20 s for the 136 MB E10 database. [V measured]

### 2.6 `sqlite3_serialize` / `sqlite3_deserialize` [V `src/sqlite.h.in`]
- **Serialize:** copies the database into one contiguous memory buffer.
- **Deserialize:** reopens a schema as an in-memory database (the built-in `memdb` VFS) over a buffer.
  - It must not be in WAL mode (`SQLITE_CANTOPEN`); patch header bytes 18 and 19 to work around this.
  - It fails with `SQLITE_BUSY` during a read transaction.
- **Assessment:** whole-database and memory-resident. Fine for tests and small databases, not for multi-gigabyte caches. [I]

### 2.7 `dbstat` [I]
`dbstat` reports per-page and per-table space use. It is not a change mechanism, but it is useful for verifying checkpoints and for sizing.

### 2.8 `sqlite_dbpage` and `sqlite3_rsync` [V `src/dbpage.c`, `tool/sqlite3_rsync.c`]
- **`sqlite_dbpage`:** an eponymous virtual table, `(pgno INTEGER PRIMARY KEY, data BLOB, schema HIDDEN)`. It reads pages "through the pager", so uncommitted and WAL-resident content is seen correctly.
  - It is writable: UPDATE or INSERT of a page-sized blob, or an INSERT with NULL to truncate.
  - Writes are refused under `SQLITE_DBCONFIG_DEFENSIVE` ("read-only"). The table is `SQLITE_VTAB_DIRECTONLY`.
- **`sqlite3_rsync`** (tool, 2024-09) copies a live database by comparing `hash(data)` of `sqlite_dbpage` pages. The origin must be in WAL mode.
- **Per-commit use is infeasible [I]:** nothing reports which pages a transaction dirtied. A full hash scan is the only option, measured at 33,167 pages in 0.29 s, about 470 MB/s on one core, or roughly 11 s for 5 GB. [V measured, I extrapolated]
- **Checkpoint use is feasible [I]:** upload only pages whose hash changed since the last checkpoint, or install a downloaded image page by page inside one transaction.
- **Replicas are not byte-identical [I]:** two replicas built by applying the same changesets are logically, not physically, identical. So page-image checkpoints must always come from one lineage: an image, plus the changesets generated after it.

### 2.9 Statement logging
See A3. Non-determinism is in built-ins (`random`, `randomblob`, `'now'`, `changes`, `last_insert_rowid`), rowid selection at the maximum value, plan-dependent ordering, application functions, and version-dependent behaviour. rqlite's rewriter is the most mature, and it is still incomplete. [V/I as in A3]

### 2.10 Other relevant APIs
- **`sqlite3_set_authorizer`:** table and column read sets at prepare time, and DDL detection. [V API]
- **`SQLITE_DBSTATUS_DEFERRED_FKS`:** "returns zero ... if and only if all foreign key constraints (deferred or immediate) have been resolved". It can be checked **before** the S3 step when the S3 step cannot run in the commit hook. [V `src/sqlite.h.in` line 9291]
- **`sqlite3session_diff`:** generates the changeset that turns one attached table into another. Use it to verify that a rebuilt cache equals a reference, and to produce a repair changeset. [V header]
- **`SQLITE_DBCONFIG_ENABLE_TRIGGER`:** off for the applier connection (E6). [V measured]
- **`PRAGMA data_version` and `schema_version`:** detect external changes and DDL. [I]

### 2.11 Does replaying captured changes on a snapshot always give a logically identical database?
**No, not unconditionally.** It does, as measured, under all of these conditions [V measured cases, I generalization]:
1. Every table has an explicit `INTEGER PRIMARY KEY` or is WITHOUT ROWID, with non-NULL PK values.
2. DDL is replayed in order before the changeset, and is not mixed with shrinking column changes in one transaction.
3. The applier disables triggers and uses `FKNOACTION`, or the conflict handler aborts so that any deviation is visible.
4. `sqlite_sequence` and `sqlite_stat1/4` are carried explicitly.
5. No `VACUUM` runs between the snapshot and later changesets, unless only INTEGER-PK and WITHOUT ROWID tables exist.
6. Virtual tables are limited to shadow-table modules such as FTS5 and R\*Tree, and the applier connection is recycled.
7. The same SQLite version applies the change, because FTS5 and R\*Tree shadow content is written as opaque blobs, which is fine. Collations and user functions must be registered identically.

---

## 3. Commit ordering and failure

### 3.1 Can stock SQLite hold a transaction open, hand over its changes, and commit or roll back on an external result?
**Yes, in two ways** [V measured or doc]:
- **(a) The wrapper owns COMMIT.**
  1. `BEGIN IMMEDIATE`, then the application's statements.
  2. `sqlite3session_changeset()` (works in-transaction, E1).
  3. Check `SQLITE_DBSTATUS_DEFERRED_FKS` == 0.
  4. The S3 steps.
  5. `COMMIT` or `ROLLBACK`.
- **(b) The S3 step inside the commit hook.** The hook blocks for the round trip and returns 0 to commit, or non-zero to roll back. It runs after the deferred-FK check and with the write lock held. It cannot run SQL, so the changeset must be in memory before COMMIT is issued: either (a)'s changeset, or A5's hook capture.

### 3.2 Order of steps (A1, A2, A4, A6) [I unless marked]
1. **Execute.** The leaseholder runs the transaction in SQLite, WAL mode, single writer connection. `BEGIN IMMEDIATE` makes write-write conflicts appear at BEGIN.
2. **Capture.** Generate the changeset, collect DDL text, read the `sqlite_sequence` and stat diffs, and add our own metadata. Also write the new LSN into a local metadata table *inside the same SQLite transaction*. The local file then records atomically which LSN it reflects. Exclude that table from the session with `sqlite3session_table_filter`.
3. **Data upload.** PUT the log entry under a unique name.
4. **Commit point.** Conditional write that replaces the manifest (`If-Match`), naming the entry and the lease. The body must contain a unique token, because Tigris ETags are MD5s and allow ABA (`tigris-probe-results.md` §1.3).
5. **Local commit.** On 200, run `COMMIT` locally and return success to the application.
   - In WAL mode, other connections keep reading the last committed snapshot during steps 3 to 5. So readers never see data that is not durable in S3, and reads need no S3 contact. [V WAL readers semantics, I mapping]
   - In rollback-journal mode, the commit hook runs after the EXCLUSIVE lock (§2.3), so readers would block for the round trip. Use WAL.

### 3.3 Failures
- **412 at step 4** (lost lease or a concurrent commit):
  - `ROLLBACK` locally and report a retryable error.
  - The cache is not ahead of the truth.
  - Before the next transaction, catch up from the log.
- **Ambiguous outcome at step 4** (timeout, connection reset after the request was sent):
  - **Never** `COMMIT` locally on a guess, because a cache ahead of the truth is corrupt.
  - Option 1: keep the SQLite transaction open (the write lock stays held; readers continue) and resolve by reading the manifest and comparing the token. Our token means `COMMIT`; any other means `ROLLBACK`.
  - Option 2: if S3 stays unreachable, `ROLLBACK`, return "outcome unknown" (like FoundationDB's `commit_unknown_result`), and mark the instance as needing catch-up before its next write.
- **Process dies between step 4 and step 5:**
  - On restart the local metadata LSN is N-1 while the manifest says N.
  - Fetch entry N, apply it (triggers off, `FKNOACTION`, ABORT on conflict), and continue.
  - This makes the originating host itself a "replayed" replica, so §2.11's identity conditions matter even with a single host.
- **Local COMMIT fails after S3 succeeded:**
  - With approach (a), the remaining causes are I/O errors and a full disk, because the deferred-FK check was done beforehand. In WAL mode the write lock is already held; in rollback mode `SQLITE_BUSY` is possible while upgrading to EXCLUSIVE.
  - Treat this as a lost cache: reopen and catch up, or rebuild.
- **Stale instance:** it cannot commit because its manifest CAS fails. It can still *read* stale data until it learns it lost the lease. That is a lease-timing question that is the same in every architecture.

### 3.4 Throughput consequence [I]
- SQLite allows one writer, and the write lock is held across the PUT and the CAS: about 60 ms + 60 ms on Tigris (`tigris-probe-results.md` Q12). That gives about 8 commits per second serially.
- **Group commit is possible with stock SQLite:**
  - Run each application transaction as a `SAVEPOINT` inside one outer transaction on the single writer connection.
  - Roll back only the savepoint of a transaction that fails its own constraints.
  - Upload one changeset for the group, and release the outer transaction on success.
  - Every transaction in the group fails together on a 412 or an ambiguous outcome.
- Readers are unaffected in WAL mode.

### 3.5 V and B
- **V:** the S3 step goes in `xSync`, and a failure there rolls back everything [V vtab doc]. Local state is ours to order.
- **B:** we own the whole sequence. Use the ordering above without the SQLite-specific caveats.

---

## 4. Go bindings (from the delegated agent; spot-checked) — state as of 2026-11-10

| Binding | Latest (date) | SQLite | cgo | database/sql | Session API in Go | Pre-update | Commit/rollback hook | Backup | Serialize | dbpage / dbstat |
|---|---|---|---|---|---|---|---|---|---|---|
| mattn/go-sqlite3 | v1.14.53 (2026-11-05) | 3.54.1 | yes | yes | no (PR #1334 open since 2025-04-16) | tag `sqlite_preupdate_hook` | yes | yes | yes | no / tag `sqlite_dbstat` |
| modernc.org/sqlite | v1.62.0 (2026-11-07) | 3.54.1 | no (transpiled) | yes (extras via `Conn.Raw`) | **no** (compiled in, not exposed) | yes (`RegisterPreUpdateHook`) | yes | yes | yes | yes / yes |
| ncruces/go-sqlite3 | v0.37.0 (2026-11-07), wasm2go since v0.33 (2026-03) | 3.54.1 | no | yes | no (not compiled) | yes (v0.35.0+) | yes | yes | emulated | no / no |
| zombiezen.com/go/sqlite | v1.4.2 (2025-05-23) | 3.49.x (pins modernc v1.37.1) | no | **no** (direct API) | **yes** (session, changeset, changegroup; no rebaser) | no | no | yes | yes | via modernc |
| crawshaw.io/sqlite | v0.3.2, last commit 2022-06 | 3.38.5 | yes | no | yes | no | no | yes | no | no |
| tailscale/sqlite | untagged, 2026-09-10 | 3.53.4 | yes | yes | no | not exposed | no | — | — | — / yes |
| eatonphil/gosqlite | v0.10.0 (2024-08) | 3.46.0 | yes | no | yes | not exposed | yes | yes | — | no |

[V via agent: repositories, tags, go.mod files and source files named in the agent's report. Spot-checked: the ncruces README says it "replaces the SQLite OS Interface (aka VFS) with a pure Go implementation"; modernc `pre_update_hook.go` defines `RegisterPreUpdateHook`, `RegisterCommitHook` (callback returns `int32`) and `RegisterRollbackHook`.]

Key facts:
- **ncruces always replaces SQLite's OS layer with its own Go VFS.** Under this project's "no custom VFS" rule that is at least questionable. It also lacks the session extension and `sqlite_dbpage`. [V README]
- **No binding offers the session API through `database/sql`.** [V via agent]
- **Combinations [I]:**
  - zombiezen's session API on modernc's engine. zombiezen imports `modernc.org/sqlite/lib`, so Go's minimal version selection may let an application require a newer modernc, subject to modernc's exact `modernc.org/libc` pin (GitLab #177, [V via agent]).
  - Or call `lib.Xsqlite3session_*` on modernc with the connection handle, which modernc does not export.
- **The WAL-reset corruption bug is present in SQLite 3.7.0 to 3.51.2, fixed in 3.51.3 (2026-03-13).** It needs two or more connections writing or checkpointing at the same instant. [V https://sqlite.org/wal.html §11] zombiezen's pinned 3.49.x is in the affected range unless modernc is upgraded. [I]
- **Performance:**
  - modernc's own figures (September 2026, against C 3.53.4): 2.0x C time for unindexed ORDER BY+LIMIT, 1.9x for GROUP BY, 1.3x for a correlated subquery. Before libc v1.75.7 these were 3.0x, 2.2x and 1.6x. [V via agent `doc.go`]
  - cvilsmeier/go-sqlite-bench, 2026-03-23, i7-1165G7, read-heavy "Many N=1000", ms: mattn 819, modernc 692, ncruces 837, **zombiezen 267**, crawshaw 380, eatonphil 451, sqinn 320. These predate modernc's September speedup. [V via agent]
  - So on read-heavy work, `database/sql` overhead dominates the engine choice. [I]
- **Concurrency:**
  - modernc: a `Raw` connection is not safe for concurrent use; each pooled connection has its own page cache.
  - ncruces: one Wasm memory per connection, capped at 256 MB by default.
  - [V via agent]
- **Licences:**
  - SQLite: public domain. "Every byte of the public-domain code can be traced back to the original authors." Build scripts are BSD and do not reach `sqlite3.c`. [V `LICENSE.md`]
  - Bindings: mattn MIT, modernc BSD-3-Clause, zombiezen ISC, ncruces MIT. [I, not rechecked]

---

## 5. Prior art (from the delegated agent; spot-checked where noted)

### 5.1 Logical or non-storage-hooking sync

| Project | Replicates | Bootstrap and catch-up | Ack vs remote durability | Non-determinism | Reported hard |
|---|---|---|---|---|---|
| rqlite v10.7.1 (2026-11-09) | rewritten SQL statements via Raft | full SQLite file + compacted WAL chain ("incremental snapshots"), then log | after Raft quorum | AST rewriter; holes listed in A3 | #2592, #2965, open #1338 (verified) to move to changesets; v10 snapshot store redesign because snapshot streaming blocked new snapshots |
| cr-sqlite v0.16.3 (2024-01), dormant | per-column CRDT changes via triggers and clock tables | app-supplied | local, multi-master | n/a | PK required; no unique indexes except PK; no checked FKs; NOT NULL needs DEFAULT; no AUTOINCREMENT; ALTER via `crsql_begin_alter`; inserts 2.5x slower; #433 DROP TABLE corruption |
| Turso Sync (tursodb 0.9.0-pre.4) | push: logical CDC mutations; pull: physical pages or MVCC log | bootstrap from remote, chunked or partial | local first; remote on `push()` | n/a | #5640 positional CDC causes "silent data corruption" after DROP+ADD COLUMN; #9430; #9529 RENAME not replayed; #8725 |
| PowerSync core 0.5.4 (2026-10-30) | server ops into `ps_oplog`, applied only at a complete, checksum-valid checkpoint; local writes via triggers into `ps_crud` | per-bucket download; re-download on checksum mismatch | local immediate; upload FIFO | n/a | no new checkpoint applied while uploads are pending |
| Marmot v2.10.0-beta (2026-09-30) | row CDC via the go-sqlite3 pre-update hook | snapshot + anti-entropy log | per-write ONE/QUORUM/ALL | n/a | "Rows may sync out of order"; avoid concurrent DDL |
| LiteSync (commercial) | transactions re-executed | full copy to a new secondary | — | blocks `random()`, `date('now')`; no AUTOINCREMENT | — |
| Bedrock (Expensify) | journal of SQL text + hash; Paxos only for leader election | missing journal rows | leader does not wait for followers | not documented | commits lost if the leader crashes before replicating |
| ElectricSQL | legacy SQLite client used trigger oplogs; 2024-07-17 rewrite is read-path Shapes only | — | — | — | old stack's complexity was "a wide surface for bugs" |
| litesql/go-ha v0.16.2 (Go) | session changesets over NATS JetStream | snapshotter | modes: leaderless LWW, leader redirect, 2PC | n/a | — |
| sqlpipe (C++ core, Go wrapper) | session changesets | bucket-hash comparison | — | n/a | requires a single INTEGER PK; rejects WITHOUT ROWID |

All rows [V via agent; sources listed there]. rqlite #1338 spot-checked [V].

### 5.2 Hold the acknowledgement until remote durability
- **Cloudflare Durable Objects (Storage Relay Service).** This hooks the VFS and WAL, so it is listed for the protocol, not the mechanism. Its output gates hold "the response until all storage writes relevant to the response have been confirmed". On failure, "the response will be replaced with an error and the Durable Object itself will restart". [V via agent, blog 2024-09-26]
- **D1 read replicas.** They receive WAL entries before confirmation and "must keep them hidden". [V via agent]
- **Mapping to us [I]:** local COMMIT after the manifest CAS is our output gate. WAL-mode isolation hides the unconfirmed transaction from readers, and "error plus restart" corresponds to "ROLLBACK and catch up".

### 5.3 Storage-hooking family (contrast only)
- Litestream (WAL shipping, LTX), LiteFS (FUSE, Consul lease, async), Graft (page segments plus a commit log, a VFS), mvSQLite (FoundationDB VFS), dqlite (Raft of WAL pages through a custom VFS). [V via agent, `graft.md`]
- Another agent covers Litestream VFS.

### 5.4 Pattern across prior art [I]
- Bootstrap is almost always a full snapshot plus a log tail.
- The recurring bugs are positional schema handling, DDL replay and non-determinism (statement replication), not the transport.

---

## 6. SQLite as the correctness oracle

### 6.1 Assets [V unless marked]
- **TCL test suite.** Public domain (`LICENSE.md`). The `test/` directory has 1,205 `*.test` files at trunk, plus more under `ext/`.
  - testing.html: 51,445 distinct cases in 1,390 files, 27.2 KSLOC of C harness, "millions" of tests with parameterization. Page updated 2026-04-21.
  - It runs only under `testfixture`, a TCL interpreter linked with test-only C hooks (`AGENTS.md`, `testing.html`).
  - My grep over `test/*.test`: 445 files use test-only machinery (`sqlite3_test_control`, `faultsim`, `testvfs`, crash simulation, `db_config`, `db func`). About 206 files avoid both those and journal, WAL and page-size pragmas. [V grep, I classification is rough]
  - Turso is "working to pass the full SQLite TCL test suite" [V COMPAT.md], so porting is possible but laborious.
- **sqllogictest.**
  - The about page says "concerned only with correct results. No attention is paid to ... transactional behavior, or concurrency" [V about.wiki mirror]. testing.html gives 7.2 million queries and 1.12 GB.
  - Code licence: GPL, BSD, MIT or CC0 at the user's choice (`sqllogictest.c` header) [V].
  - SQLite's own results are the reference, so for architecture B the whole corpus is usable as is. For A it tests nothing new. [I]
  - Runners and corpora are covered in `external-test-suites.md` §2.
- **Fuzz corpora.** `test/fuzzdata1.db` to `fuzzdata8.db` (about 1.8 to 17 MB each), `dbfuzz2-seed1.db` and `sessionfuzz-data1.db`, rerun by `fuzzcheck`. Public domain. [V local listing, `testing.html`]
- **Session tests and fuzzing.** `ext/session/` has about 45 `session*.test` files, `changesetfuzz1.test`, `changesetfuzz.c` and `sessionfuzz.c`. [V listing]
- **`dbsqlfuzz`:** "proprietary fuzzer". **TH3:** "proprietary and requires a license" (page updated 2026-10-01). Neither is available. [V]
- **SQLancer:** has a SQLite provider (TLP, NoREC and others); see `external-test-suites.md` §4.

### 6.2 Differential setups per architecture [I]
- **Family A:** SQL semantics come from SQLite itself, so the oracle tests **the replication layer**.
  - **Replay equivalence.** After every commit in a randomized workload, rebuild a fresh cache from S3 alone (checkpoint plus log) and compare it with the originator's file. Compare logical state with `.dump --preserve-rowids` or `sqlite3session_diff`, including `sqlite_sequence` and stats, plus `PRAGMA integrity_check` and FTS5 `integrity-check`.
  - **Control.** Run the same workload on a plain SQLite file with no sql4s3. Query results and final state must match. This catches capture gaps, ordering bugs and commit and recovery bugs.
  - **Crash and fault injection.** Kill points at steps 2 to 5 of §3.2, plus S3 faults ("apply, then fail the response"). Then check replay equivalence and that no acknowledged commit is lost (the bank workload in `graft.md`).
  - **Workload generation.** Generate SQL that exercises §2.1.3's edge cases: DDL, triggers, cascades, FTS5, AUTOINCREMENT, REPLACE, PK updates, savepoint rollbacks, statement aborts.
  - **Limit:** the oracle cannot catch SQLite's own bugs. It also shares blind spots with SQLite: where both sides use SQLite to read state, a bug in the comparison path is invisible.
- **V:** differential SQL results against stock SQLite over the same data in normal tables. This catches constraint, index and `xBestIndex` mistakes, plus the replication tests above.
- **B:** classic differential testing. Run the same SQL on our engine and on stock SQLite, comparing results, errors and final state. Use sqllogictest (full corpus), the TCL suite subset, SQLancer-style TLP and NoREC in Go, and random workload generators.
  - It cannot catch behaviour where SQLite's answer is unspecified, such as unordered results or floating-point edge cases.
  - Turso does this: a DST simulator with a differential mode against SQLite, plus Antithesis, SQLancer, SQLRight, fuzzing and TLA+. [V via agent]

### 6.3 How prior art tests itself [V via agent]
- **jepsen.io** has no analysis of rqlite, dqlite, Turso, libSQL, Cloudflare, ElectricSQL, PowerSync, LiteFS or Bedrock.
- **rqlite:** unit, system and end-to-end tests; a third-party student Jepsen study of rqlite 7.3.1, which passed with strong reads; used as an Antithesis demo (2026-03-26).
- **cr-sqlite:** Python property tests (Hypothesis).
- **Marmot:** Go cluster tests (partition, crash, convergence).
- **Graft:** Antithesis with a bank workload (`graft.md`).
- **Turso:** DST, Antithesis and TLA+.

---

## 7. Risks and deal-breakers

### 7.1 For the idea as a whole [I unless marked]
- **The S3 format question.** "Own the S3 format entirely" fits A2, V and B. A1's checkpoints are SQLite files, so the format would contain SQLite's file format. That format is stable and public domain, but it is not ours.
- **Cold start (family A).**
  - Lazy page fetch is impossible without a VFS. A new host must download a full checkpoint (A1) or bulk-load one (A2, about 100 s per 5 GB on this machine), then apply the log tail.
  - Download time depends on S3 throughput and was not measured here. At 100 to 500 MB/s aggregate, 5 GB takes 10 to 50 s.
  - A2's per-table lazy hydration via the authorizer softens this only when queries touch few tables.
- **Write throughput.** Under one writer with a round trip of about 125 ms, group commit via savepoints is needed for more than about 8 commits per second (§3.4).
- **Compatibility restrictions in A** (policy or extra capture required, §2.11):
  - tables must have an INTEGER PK or be WITHOUT ROWID;
  - no NULL PKs;
  - DDL rules;
  - no `VACUUM` between checkpoints, or `VACUUM` becomes a checkpoint;
  - custom virtual tables unsupported;
  - application functions and collations must be registered on the applier too.
- **Binding gap.** No Go binding gives session capture plus commit hooks through `database/sql` in one maintained package (§4).
  - The likely path is modernc plus zombiezen-style session access, or a thin wrapper of our own around `modernc.org/sqlite/lib`.
  - Pure Go costs 1.3x to 2x on query execution. cgo (mattn) needs a C toolchain and lacks session in Go.
- **Version pinning.** Changeset bytes, FTS5 shadow blobs and planner behaviour belong to one SQLite version. Pin the SQLite version, and include it in every log entry. Upgrades need a test that applies old entries with the new version. The WAL-reset bug (fixed in 3.51.3) shows why pinning matters. [V bug]

### 7.2 Per architecture
- **A1, A2:** silent drops in `changeset_apply` for incompatible tables (E5). Mitigation: an ABORT-on-anything conflict handler, plus a schema-hash check before apply. FTS5 stale cache on the applying connection (E8c). Mitigation: a dedicated applier connection, recycled after apply.
- **A3:** an open-ended rewriter (rqlite's history). Likely a deal-breaker given "correctness first".
- **A4:** trigger maintenance and double writes. Workable, but the session extension does the same job natively.
- **A5:** a deal-breaker as stated (records undone changes, measured) unless savepoint and statement undo is modelled.
- **A6:** serializability across writers is unachievable at row level with SQLite's APIs.
- **V:** loses triggers, secondary indexes declared in SQL, and constraint enforcement. Not "SQLite compatible" in the sense applications expect.
- **B:** effort and divergence. Turso's three years with hundreds of contributors is the reference point. The oracle story is strongest here, and the product risk is largest.
- **H1:** reimplements DML semantics. No advantage over A or B.

---

## 8. What I looked for and could not find
- **S3 GET throughput** for multi-gigabyte downloads from Tigris. Not measured; `tigris-probe-results.md` has only per-request latencies.
- **`VACUUM INTO` and rowids.** Whether `VACUUM INTO` output preserves rowids of tables without an INTEGER PK; the docs are silent.
- **Session extension and `SQLITE_DBCONFIG_DEFENSIVE`.** No documentation on applying changesets to FTS5 shadow tables when defensive mode is on. Inferred that it fails, because shadow tables become read-only; not tested.
- **`SQLITE_DBSTATUS_DEFERRED_FKS` in bindings.** Whether any Go binding exposes it. Not verified.
- **Published sizes and costs.** No published benchmark of session-extension overhead or changeset size; §2.1.5 is my single run.
- **jepsen.io analyses.** None for any project in §5.
- **sqllogictest licence.** Only the source file headers state it. The Fossil site blocked automated reads; I used the GitHub mirror.

---

## 9. Which I would choose
**A2, with A1's checkpoint format optional.** Stock SQLite executes all SQL against a local WAL file. The session extension captures each transaction before COMMIT. The coordinator commits locally only after the manifest conditional write. The S3 format holds our own changeset-log entries and logical snapshots. The §2.11 conditions are enforced as policy, and replay equivalence against a cache rebuilt from S3 is the main test oracle.

The reason: this keeps correctness risk in a small replication layer that can be tested exhaustively. SQLite's SQL semantics come unchanged instead of being re-implemented. Every mechanism the design needs (in-transaction changesets, a blocking commit hook, conflict-checked apply) is verified above. Its costs are a full-snapshot cold start and about 8 serial commits per second without savepoint group commit; both are visible and bounded. [I]

---

## Sources
- **SQLite source** (GitHub mirror, trunk `8d769ae`, 2026-11-10): `ext/session/sqlite3session.h`, `ext/session/sqlite3session.c`, `ext/session/*.test`, `ext/session/changeset.c`, `src/sqlite.h.in`, `src/vdbeaux.c` (`vdbeCommit`), `src/delete.c`, `src/insert.c`, `src/dbpage.c`, `tool/sqlite3_rsync.c`, `LICENSE.md`, `AGENTS.md`, `test/`. https://github.com/sqlite/sqlite
- **SQLite docs:**
  - https://sqlite.org/sessionintro.html
  - https://sqlite.org/changes.html
  - https://sqlite.org/deterministic.html
  - https://sqlite.org/autoinc.html
  - https://sqlite.org/lang_vacuum.html
  - https://sqlite.org/vtab.html
  - https://sqlite.org/testing.html
  - https://sqlite.org/th3.html
  - https://sqlite.org/wal.html
- **sqllogictest:**
  - https://github.com/gregrahn/sqllogictest (mirror; `about.wiki`, `src/sqllogictest.c`)
  - https://sqlite.org/sqllogictest
- **Turso:**
  - https://github.com/tursodatabase/turso (`COMPAT.md`)
  - issues #5640, #9430, #9529, #8725
- **rqlite:** https://github.com/rqlite/rqlite/issues/1338 and the agent's rqlite sources
- **Go bindings:**
  - https://github.com/mattn/go-sqlite3
  - https://gitlab.com/cznic/sqlite (`pre_update_hook.go`, `doc.go`, `CHANGELOG.md`)
  - https://github.com/ncruces/go-sqlite3 (README; discussion #361; PR #362)
  - https://github.com/zombiezen/go-sqlite
  - https://github.com/crawshaw/sqlite
  - https://github.com/tailscale/sqlite
  - https://github.com/eatonphil/gosqlite
  - https://github.com/cvilsmeier/go-sqlite-bench
- **Prior art:**
  - https://rqlite.io/docs/api/non-deterministic/
  - https://github.com/vlcn-io/cr-sqlite
  - https://vlcn.io/docs/cr-sqlite/constraints
  - https://turso.tech/blog/introducing-databases-anywhere-with-turso-sync
  - https://docs.turso.tech/features/embedded-replicas/introduction
  - https://blog.cloudflare.com/sqlite-in-durable-objects/
  - https://blog.cloudflare.com/durable-objects-easy-fast-correct-choose-three/
  - https://blog.cloudflare.com/d1-read-replication-beta/
  - https://github.com/maxpert/marmot
  - https://github.com/powersync-ja/powersync-sqlite-core
  - https://docs.powersync.com/architecture/consistency
  - https://electric.ax/blog/2024/07/17/electric-next
  - https://github.com/Expensify/Bedrock/tree/main/docs
  - https://litesync.io/en/sqlite-synchronization.html
  - https://pkg.go.dev/github.com/litesql/go-ha
  - https://github.com/marcelocantos/sqlpipe
  - https://canonical.com/dqlite/docs/explanation/replication
  - https://jepsen.io/analyses
  - https://github.com/wildarch/jepsen.rqlite
- **Repository notes:** `graft.md`, `tigris-probe-results.md`, `external-test-suites.md`, `testing-strategies.md`, `object-storage-architectures.md`.
