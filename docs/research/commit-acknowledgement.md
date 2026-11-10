# Commit acknowledgement when the authoritative step succeeded and the local step failed

Date: 2026-11-10. Tags: **[V]** = read the primary source in this run (source file or official doc, quoted or paraphrased closely). **[I]** = inference or memory, not verified in this run.

Question: what do comparable systems tell the client, and what does the node do, when a commit is durable in the authoritative place and the local apply then fails?

For s4ql: after the S3 manifest swap (the commit) succeeds and the local SQLite cache commit fails with an I/O error, what should `Commit` return, and what should the handle do? The proposal under review was that `Commit` returns success, the handle goes into a terminal stopped state, and a reopen catches the cache up from the log. The design agreed afterwards is in [../todo.md](../todo.md#local-io-failure-handling) until it is built.

Source: the raw source files fetched for grepping (not published). Links to the originals are given below.

## Short answer

- In the closest analogies (FoundationDB, CockroachDB, Neon, etcd), applying the committed entry to the local store is **not on the acknowledgement path**. The client gets success once the authoritative log is durable. A failure to apply locally later is **fatal for that node or role**, and recovery is replay or rebuild from the log. So "the client was told success, and the node then stops" is the normal outcome there. It comes from ordering (ack first, apply later), not from a decision taken after seeing the failure.
- I found **no system that, after detecting the local failure and before replying, deliberately replies with plain success.** The nearest is PostgreSQL's synchronous-replication cancel path. There the server knows the commit is durable locally but the promised second step did not finish. PostgreSQL returns the commit with a WARNING, because an ERROR "would lead the client to believe that the transaction aborted, which is not true".
- Servers that hit a fatal error **before** replying (PostgreSQL PANIC, MySQL `ABORT_SERVER`, etcd, rqlite) die without replying. The client sees a lost connection, which client libraries surface as "outcome unknown" (CockroachDB `AmbiguousResultError`, FoundationDB `commit_unknown_result`). Nobody returns an error that means "not committed" once the authoritative record is durable.
- An embedded library cannot "die without replying". Its nearest equivalent is an "outcome unknown" error. s4ql knows the outcome, so "unknown" would be less truthful than success. Success plus a stopped handle is consistent with the precedents. A loud, observable stopped state is the analogue of PostgreSQL's WARNING.

## Per system

### PostgreSQL

- **Sync replication wait cancelled** [V `src/backend/replication/syncrep.c`, `SyncRepWaitForLSN`, https://github.com/postgres/postgres/blob/master/src/backend/replication/syncrep.c]
  - Situation: the commit record is durable locally (the authoritative place for the primary), and the wait for the standby (the promised second step) is interrupted.
  - Client told:
    - On query cancel: WARNING "canceling wait for synchronous replication due to user request", DETAIL "The transaction has already committed locally, but might not have been replicated to the standby." The commit then completes normally (success).
    - On terminate (`ProcDiePending`): WARNING "... terminating connection due to administrator command" with the same DETAIL, then `whereToSendOutput = DestNone`, so no commit ack is sent, and the backend dies.
    - On postmaster death: no output, and the process exits.
  - Stated reasoning (code comment): "we can neither acknowledge the commit nor raise ERROR or FATAL. The latter would lead the client to believe that the transaction aborted, which is not true: it's already committed locally. The former is no good either: the client has requested synchronous replication, and is entitled to assume that an acknowledged commit is also replicated, which might not be true. So in this case we issue a WARNING (which some clients may be able to interpret) and shut off further output."
  - For s4ql: this argues against returning an error (it would mean "aborted"). It also says a plain ack is wrong if the ack promises something that did not happen. In s4ql the ack promises durability in S3, which did happen. What it may also promise, read-your-writes on this handle, is kept only because the handle stops.
- **Errors inside the commit critical section** [V `src/backend/access/transam/xact.c`, `RecordTransactionCommit`, https://github.com/postgres/postgres/blob/master/src/backend/access/transam/xact.c]
  - Writing the commit record and updating `pg_xact` runs under `START_CRIT_SECTION()`, so any ERROR there is promoted to PANIC (the whole server restarts and runs WAL recovery). [V for the critical section; I for "ERROR in a critical section becomes PANIC", a well-known PostgreSQL rule not re-read here]
  - `RecordTransactionAbort` has `elog(PANIC, "cannot abort transaction %u, it was already committed")`: once committed, the only way out is a crash, never an abort.
- **fsync failure on data files** [V https://www.postgresql.org/docs/current/runtime-config-error-handling.html, `data_sync_retry`]
  - Default `off`: "PostgreSQL will raise a PANIC-level error on failure to flush modified data files to the file system. This causes the database server to crash."
  - Reasoning: "the status of data in the kernel's page cache is unknown after a write-back failure ... the only way to avoid data loss is to recover from the WAL after any failure is reported."
  - For s4ql: this is the closest match to "the local store's I/O failed, and the log is the truth". PostgreSQL treats it as a crash and recovers from the log. It never retries in place.

### MySQL / InnoDB with the binary log

- **`binlog_error_action`** [V https://dev.mysql.com/doc/refman/8.4/en/replication-options-binary-log.html]
  - Covers errors writing, flushing or syncing the binary log. The default is `ABORT_SERVER`: "the server halt logging and shut down ... On restart, recovery proceeds as in the case of an unexpected server halt." `IGNORE_ERROR` keeps going without binlogging (legacy compatibility).
  - Client told: the server shuts down, so nothing (connection lost). [I that the in-flight commit gets no reply]
- **Binlog synced, then engine commit** [V `sql/binlog.cc`, `process_commit_stage_queue`, `finish_transaction_in_engines`, https://github.com/mysql/mysql-server/blob/trunk/sql/binlog.cc]
  - After the binlog flush and sync stage, the engine commit stage calls `finish_transaction_in_engines`, which returns `void`. The code treats the engine commit after binlog sync as infallible: no path reports an engine commit failure to the client as a commit error. The comment says "flush or sync errors are handled by the leader of the group (using binlog_error_action). Hence treat only COMMIT_ERRORs as errors", and `CE_COMMIT_ERROR` is set only when binlog rotation fails.
  - What InnoDB does on an I/O error at that point: [I] InnoDB treats redo-log write failures as fatal and aborts the server. On restart, crash recovery commits prepared transactions whose XID is in the binlog. I did not verify this in source in this run.

### CockroachDB

- **Ack before apply** [V `pkg/kv/kvserver/apply/doc.go` and `apply/task.go` `AckCommittedEntriesBeforeApplication`, https://github.com/cockroachdb/cockroach/blob/master/pkg/kv/kvserver/apply/task.go]
  - "a proposal through raft can be known to have succeeded as soon as it is durably replicated to a quorum of replicas (i.e. has committed in the raft log). The proposal does not need to wait for the effects of the proposal to be applied ... This is because the raft log is the provider of atomicity and durability for replicated writes, not ... the replicated state machine itself."
  - Transactional writes are acked before they are applied, after a deterministic rejection check on an ephemeral batch. Latches are held until the write applies, so conflicting commands cannot see the state before it.
- **Apply failure is fatal** [V `pkg/kv/kvserver/replica_raft.go` `maybeFatalOnRaftReadyErr`, `store_raft.go`]
  - Any error from `handleRaftReady` (which includes `ApplyCommittedEntries`), other than replica removal, is `log.Fatalf`: the node process exits. `replica_app_batch.go` calls part of application "a large (infallible, i.e. all errors are fatal for this Replica) critical section".
- **Disk stalls** [V https://docs.cockroachlabs.com/docs/stable/cluster-setup-troubleshooting]
  - A WAL sync stalled for 20 s (`COCKROACH_ENGINE_MAX_SYNC_DURATION`) terminates the process. "the mitigation in almost all cases is to restart the node with the stalled disk." "Do not raise the stall thresholds to mask hardware issues."
- **`AmbiguousResultError`** [V `pkg/kv/kvpb/errors.proto`]
  - "indicates that a request may have succeeded or failed, but the response was not received and the final result is ambiguous." This is what a client sees when the node that would have replied dies.
- For s4ql: the strongest precedent for "success, then the node stops". The client may already hold a success when the apply fails, and the node then exits and recovers from the log.

### etcd

- [V `server/etcdserver/server.go` `applyEntryNormal`] The client's waiter is triggered (`s.w.Trigger(id, ar)`) after the entry is applied to the backend's batch transaction.
- [V `server/storage/backend/backend.go` `run`, `batch_tx.go`] The bbolt transaction is committed on a timer (`batchInterval`), so later than the ack. On commit failure, `t.backend.lg.Fatal("failed to commit tx", ...)`: the process exits. On restart etcd replays the raft WAL from its consistent index.
- [I] So a client can hold a success for a write whose local bbolt commit later fails, and the node then dies. It is the same shape as CockroachDB: durable in the raft WAL means committed, and a local apply failure means a crash.

### rqlite

- [V `store/store.go` FSM `Apply`, `store/command_processor.go`] Per-statement SQL errors go back to the client inside the result. A whole-request error from `db.Execute` or `db.Request` leads to `s.logger.Fatalf("fatal error applying command at index %d - aborting to prevent divergence between nodes: %s", ...)`.
- The client gets the response from the FSM after apply, so on a fatal apply error the client gets no response (connection lost). [I] I did not verify whether an SQLite I/O error is classed as a per-statement error or as a request error.
- Stated reasoning: "to prevent divergence between nodes".

### hashicorp/raft (Consul, Nomad, rqlite)

- [V `fsm.go`] "Apply is called once a log entry is committed by a majority of the cluster. ... Apply must be deterministic". Its return value "is returned to the client as the ApplyFuture.Response". The interface has no error path for a failed apply. The FSM implementation must handle it, and rqlite (above) exits.

### TiKV

- [V `components/raftstore/src/store/fsm/apply.rs`] `write_opt(...).unwrap_or_else(|e| panic!("failed to write to engine: {:?}", e))`: a failed write to the local KV engine during apply panics the process.
- [I] I did not verify whether TiKV replies before or after apply for normal writes.

### FoundationDB

- **When success is sent** [V https://apple.github.io/foundationdb/architecture.html] "Once the mutations are durable on the logs, the commit proxy responds with success to the user." "Storage servers pull mutations from transaction logs and write them into the storage engine." Storage servers are not on the ack path.
- **Storage disk error** [V `fdbserver/worker/worker.cpp` `workerHandleErrors` and the storage recruitment path] A role that fails with `io_error` ends. A transaction-log role with `io_error` or `io_timeout` rethrows and takes down the worker. A storage server that stopped on `io_error` or `io_timeout` leaves disk files that are cleaned up once data distribution "already repairs the team". So the storage server is **rebuilt from replicas, not repaired in place**.
- **`commit_unknown_result`** [V https://apple.github.io/foundationdb/developer-guide.html] Raised when the client loses the connection to the commit proxy, or the proxy fails mid-commit. Guarantee: "the transaction is not in-flight anymore". It is retryable in the standard loop, so non-idempotent transactions can double-apply; the guide prescribes idempotency keys.
- For s4ql: FoundationDB uses "unknown" only when the client genuinely does not know. It never tells a client "unknown" or "failed" because a storage server could not apply a committed mutation.

### Neon

- [V https://neon.com/docs/introduction/architecture-overview] "A transaction is considered committed once a quorum of safekeepers has acknowledged the WAL record." On pageserver failure, "no durable state is lost. Another pageserver can be deployed and it can reconstruct pages using WAL and object storage."
- Shape: the authoritative log (safekeepers) is the ack point, and the materialised store (pageserver) is rebuilt, not consulted, when it fails.

### Cloudflare Durable Objects (output gates)

- [V https://blog.cloudflare.com/durable-objects-easy-fast-correct-choose-three/] Outgoing messages are held until the storage write completes. "If the write ultimately fails, the outgoing network messages will be discarded and replaced with errors", and "the Durable Object will be shut down and restarted from scratch".
- Here the **authoritative** write failed, so the error is truthful. The part that carries over to s4ql is the reaction to a storage failure: reset the object and restart from durable state, and never keep serving from memory.

### SlateDB

- [V project note [slatedb.md](slatedb.md) §1.6, citing `wal/slatedb/writer.rs:49`] A fatal WAL flush error closes the writer ("The manager becomes unusable after encountering a fatal error"). Recovery is reopen, which re-fences and replays. This is a terminal-handle precedent in an object-store-first embedded library. [I] Not re-read in this run.

### SQLite itself

- **COMMIT can return an error after the commit point** [V `src/pager.c`, https://github.com/sqlite/sqlite/blob/master/src/pager.c]
  - Rollback-journal mode: the commit point is finalizing the journal (delete, truncate, or zero the header). After it, `pager_end_transaction` can still return an I/O error. The comment after zeroing the journal says "At this point the transaction is committed but the write lock is still held". The journal-size-limit truncate that follows can fail, and so can `pager_truncate` of the database file ("At this point the journal has been finalized and the transaction successfully committed"). Those `rc` values reach the caller.
  - WAL mode [V `src/wal.c` `walFrames`; I for the consequence]: the frames, including the commit frame, are written and then `sqlite3OsSync` runs (line ~4313). `walIndexAppend` and the wal-index header update come after it (~4339 to 4358). If the sync fails, the error is returned before the wal-index is updated, but the commit frame may be on disk with valid checksums. WAL recovery after a reopen would then treat the transaction as committed. [I]
- **Docs** [V https://www.sqlite.org/lang_transaction.html] SQLITE_FULL, SQLITE_IOERR, SQLITE_INTERRUPT and SQLITE_NOMEM may roll back the statement or the whole transaction. Use `sqlite3_get_autocommit()` or `sqlite3_txn_state()` to tell which. The docs do not say whether a failed COMMIT may have committed.
- For s4ql: after the local COMMIT fails, s4ql cannot know whether the cache has the transaction. Recovery must read the cache's recorded last-applied change file and catch up from there. It must not assume either answer. The spec already records that position in the same local transaction as the rows, which makes this work. In-place retry is unsafe for the same reason PostgreSQL gives for `data_sync_retry`.

### Not covered or not applicable

- LiteFS, Litestream, Graft: these commit locally first and ship later, the opposite order. Graft acks a local, non-fsynced write (project note [graft.md](graft.md)). No precedent for this question.
- mvSQLite, turbopuffer, WarpStream, Aurora, dqlite, Kafka: not checked in this run. [I] Aurora acks on a 4-of-6 storage quorum, and individual segment failures are repaired by peers, which is the same shape as FoundationDB and Neon.

## Arguments against "return success"

1. **Most servers never reply in this state.** PostgreSQL PANIC, MySQL `ABORT_SERVER`, rqlite and etcd die, so the client sees a lost connection and treats it as unknown. Counter: an embedded library cannot drop the connection, and s4ql knows the outcome. Choosing "unknown" would make the application do an idempotency check that cannot fail.
2. **PostgreSQL refuses a plain ack when the ack promises more than happened.** If s4ql's "committed" promises that this handle will show the write, success breaks that promise unless the handle stops. Stopping repairs it. A plain success with no visible signal is weaker than PostgreSQL's WARNING. Suggestion: make the stopped state loud and observable. For example, every later call returns a distinct `ErrStopped` that wraps the local I/O cause, it is logged at error level, and a health check reports it.
3. **The spec's acknowledgement rule.** The precedents, especially CockroachDB's comment that "the raft log is the provider of atomicity and durability ..., not ... the replicated state machine itself", support amending it: the ack means "durable in S3". The cache is a derived view that is never served stale, because the handle stops.
4. **Retry hazard if an error is returned.** FoundationDB's guide documents that retrying after an ambiguous commit double-applies non-idempotent transactions. That is the stated risk of returning an error here, and it supports the proposal.
