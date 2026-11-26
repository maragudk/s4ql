# Commit identity after an ambiguous result

Date: 2026-11-26. Tags: **[V]** = read in a primary source in this run (documentation, a specification, source code, or an issue), with the reference. **[I]** = inference, or seen only in a search result's excerpt.

Question: when a writer does not know whether its commit landed, how do other systems find out, and how do they keep two distinct transactions from being taken for one? The trigger is codex 3 in [adversarial-core-2026-11-26.md](adversarial-core-2026-11-26.md): two instances of s4ql that start from the same parent, make the same change, and read the same wall clock write byte-identical commit objects with one name. The one that lost the lease finds "its" name in the log and reports success, so two transactions are acknowledged and one is applied. The options on the table are (a) a random 128-bit value in each commit's header, (b) the writer's instance ID in the header, and (c) leave it and document it.

Source revisions read: Delta Lake `master` at `25561a5d`, SlateDB `main` at `06717572`, Graft `main` at `b07d9312`, Apache Paimon `master` at `13b985df`, Apache Hudi `master` at `bf0e7ffd`, Dolt `main` at `d5a833f8`, CockroachDB `master` at `d30c905f`. Iceberg and FoundationDB files were read at `main` on 2026-11-26.

## Summary

- **Every system that answers "did my commit land?" by looking for something in the log or the catalog looks for a value that is unique to one attempt, and almost all of them make it random.** Delta Lake puts a random UUID `txnId` into the commit file and recognises its own commit by it. Iceberg names each metadata file with a random UUID and checks whether that name is current or in the history. SlateDB attaches a ULID from its seeded random source to every conditional put and compares it on `AlreadyExists`. FoundationDB attaches 16 random bytes. Paimon uses a random UUID per writer plus a sequence number. Kafka uses a producer ID from the broker, an epoch, and a sequence number. None of them identifies an attempt by its content alone.
- **The systems whose identity is content-derived are the ones that treat equal content as the same thing on purpose:** Git (two identical commits are one object), Graft (the same commit hash at the same LSN is taken as "Someone (including us) pushed the same commit"), and ClickHouse's insert deduplication (a block is identified by the hash of its data). ClickHouse is the documented case of s4ql's fault class: separate inserts of the same rows are silently dropped as duplicates, and the remedy it added in version 22.2 is `insert_deduplication_token`, an identity per insert chosen by the client.
- **No one reported s4ql's exact sequence** (two writers, one parent, one wall-clock reading, the loser taking the winner's object for its own), as far as these searches reach. Graft has the same mechanism and calls it benign, because it treats a commit as a state and not as an acknowledged event; that holds for Graft's model, and fails in s4ql's for the reason the adversarial review gives.
- **For s4ql:** (a) is what Delta, Iceberg, SlateDB, and FoundationDB do, and none of their known costs (an ID table that must expire, metadata a store may not keep) applies, because the value lives inside an immutable commit object that the walk reads anyway. SlateDB shows that it fits a deterministic simulation: its ULIDs come from `DbRand`, which a test seeds. (b) is sound too, because s4ql's instance ID is already 128 random bits per process and an instance resolves an unknown outcome before it commits again, but its soundness rests on that second rule as well. (c) leaves a lost update that a stopped or coarse clock makes reachable. A fourth option, the lease tenure in the header, is (b) with a narrower scope and no gain that matters here.

## Systems

### FoundationDB

- **The ambiguity.** `commit_unknown_result` means the commit may or may not have happened, but "it is guaranteed that the transaction is not in-flight anymore", unlike `transaction_timed_out`. [V, already in [foundationdb.md](foundationdb.md), from https://apple.github.io/foundationdb/developer-guide.html]
- **The identity.** With the option `AUTOMATIC_IDEMPOTENCY` the client "sets the transaction's idempotency id to 16 random bytes"; with `IDEMPOTENCY_ID` the user supplies one of up to 255 bytes. [V https://github.com/apple/foundationdb/blob/main/design/idempotency_ids.md]
- **Where it is stored.** Beside the data, in the system keyspace: the key is `\xff\x02/idmp/` plus the 8-byte commit version and a batch index, and the value holds the IDs of the transactions in that batch. [V same]
- **After an ambiguous result.** `fdb_database_commit_result` commits a dummy transaction that conflicts with any transaction of the same ID, so none is still in flight, and then searches for the ID in the versions where it could have committed. [V same]
- **Costs found.** "Additional storage space on the cluster for the idempotency ids", a "Potential write hot spot", and expiry: a cleaner removes the oldest IDs, and a lookup of an ID that may have expired fails with "a new, non-retriable error". The author measured "about 1% overhead for the worst case workload". The feature "is currently experimental and not recommended for use in production". [V same; https://apple.github.io/foundationdb/automatic-idempotency.html]

### Delta Lake

- **The commit.** A commit is the file `NNN.json` for the next version, written so that an existing entry is never overwritten. [V `PROTOCOL.md`]
- **Two identities.** `txn` holds an `appId` ("a unique identifier for the process that is modifying the table") and a `version` ("an indication of how much progress has been made by that application"), so that a streaming sink can skip a batch it already wrote; "Delta only ensures that the latest `version` for a given `appId` is available". `commitInfo` may hold a `txnId`, "A unique identifier for the transaction that produced this commit", and must hold one when the feature `catalogManaged` is on. [V https://github.com/delta-io/delta/blob/master/PROTOCOL.md, sections Transaction Identifiers and Commit Provenance Information]
- **How the `txnId` is made.** `val txnId: String = UUID.randomUUID().toString`, once per transaction. [V `spark/src/main/scala/org/apache/spark/sql/delta/OptimisticTransaction.scala` line 587]
- **After an ambiguous result.** On `FileAlreadyExistsException` the writer checks the winning commit for conflicts, and `detectIdempotentSelfCommit` "Detects whether the winning transaction is this transaction's own commit by comparing the transaction IDs of the winning and losing transactions"; on a match it records `delta.commit.idempotentSelfCommitDetected` and treats the commit as landed. It checks only the first conflicting version, and only when `commit.idempotencyCheck.enabled` is set, an internal option whose default is false and whose doc says it "Prevents duplicating data on retry after a transient commit-response loss". [V same file, lines 3885 to 3910; `spark/src/main/scala/org/apache/spark/sql/delta/sources/DeltaSQLConf.scala` lines 589 to 597] So Delta's own writer has the random ID in every commit but leaves the check off by default.
- **Without it.** Delta Kernel throws `CommitStateUnknownException` when a first attempt fails with a retryable error and a second hits a conflict, because it "cannot determine whether a previous commit attempt succeeded", and leaves the check of the history to the user. [V https://docs.delta.io/api/latest/java/kernel/io/delta/kernel/exceptions/commitstateunknownexception]
- **Time.** An in-commit timestamp is the larger of the writer's wall clock and one millisecond after the previous commit's. [V `PROTOCOL.md`, In-Commit Timestamps] That makes times increase along the log, but two writers on the same parent still get the same value, so it would not separate a and b.

### Apache Iceberg

- **Snapshot IDs are random:** `UUID uuid = UUID.randomUUID()`, its two halves XORed and the sign bit cleared. [V `core/src/main/java/org/apache/iceberg/SnapshotIdGeneratorUtil.java`]
- **Metadata files are named with a random UUID:** `String.format(Locale.ROOT, "%05d-%s%s", newVersion, UUID.randomUUID(), fileExtension)`. [V `core/src/main/java/org/apache/iceberg/BaseMetastoreTableOperations.java`]
- **After an unknown commit state** the catalog reloads the table and checks `currentMetadataFileLocation.equals(newMetadataLocation) || metadata.previousFiles().stream().anyMatch(log -> log.file().equals(newMetadataLocation))`, with retries and backoff. A location that is not found is reported as `UNKNOWN` and not as a failure, "because possible pending retries might still commit the change". [V same file; `BaseMetastoreOperations.java`]
- **For s4ql** this is the closest shape to `Tx.inLog`: look for one's own name in the current pointer and its history. The name is unique by construction, because a random UUID is part of it.

### Apache Paimon

- **The identity is derived:** a `commitUser` per writer, which is `UUID.randomUUID()` with an optional prefix [V `paimon-api/src/main/java/org/apache/paimon/CoreOptions.java` `createCommitUser`], and a `commitIdentifier` per commit, which a Flink sink takes from its checkpoint [I]. Both are stored in each snapshot.
- **After an ambiguous result** the commit walks the snapshots from the last one it knew to the latest, and returns success if one has its `commitUser`, its `commitIdentifier`, and its kind. `filterCommitted` drops committables whose identifier is at or below the latest snapshot of the same user. [V `paimon-core/src/main/java/org/apache/paimon/operation/FileStoreCommitImpl.java` lines 326 to 365 and 1350 to 1370]
- **For s4ql** this is the walk of `Tx.inLog` with an identity that is the writer's random ID plus a sequence. It is option (b) with a sequence number, and the sequence is what lets one writer have several commits that it may need to look for.

### Apache Hudi

- An instant time is the wall clock in milliseconds. Within one JVM, `createNewInstantTime` loops until the new time is greater than the last one it handed out (`LAST_INSTANT_TIME`). [V `hudi-common/src/main/java/org/apache/hudi/common/table/timeline/HoodieInstantTimeGenerator.java` lines 75 to 84] Uniqueness across writers rests on the lock and the `TimeGenerator` around it [I]; it was not read further. Hudi's lock on object storage is in [s3-clock.md](s3-clock.md).

### SlateDB

- **Fencing.** A writer bumps `writer_epoch` in the manifest when it opens, and a stale writer finds out at its next write. [V, already in [slatedb.md](slatedb.md) and [s3-clock.md](s3-clock.md)]
- **The ambiguous put.** Issue #766: with `PutMode::Create` and a timeout, "the file was created but the response was lost", and a retry's "file already exists" may be the writer's own earlier write. The options discussed were a ULID "as a custom metadata field", a checksum (rejected, since an ETag "is not always MD5"), and wrapping the store in SlateDB. [V https://github.com/slatedb/slatedb/issues/766] PR #1164, "Add ULID-based precondition handling to RetryingObjectStore", merged on 2026-01-05. [V https://github.com/slatedb/slatedb/pull/1164]
- **How it works now.** Every conditional put carries the metadata `slatedbputid`, whose value is `self.rand.rng().gen_ulid(self.clock.as_ref())`. On `AlreadyExists` or a failed precondition after a retry, `verify_put_succeeded` reads the object's head and treats the put as done if the ID matches. `self.rand` is a `DbRand`, which tests construct with `DbRand::new(seed)`, so the IDs are reproducible in simulation. If the store does not support attributes, the put falls back to one without an ID. [V `slatedb/src/retrying_object_store.rs` lines 29 to 33, 123 to 157, 383 to 420, 853 to 892]
- **For s4ql** this is option (a), with the value beside the object instead of in its content, and it settles the question of random values and deterministic simulation: draw them from the seeded source.

### Graft

- **The identity is content-derived.** The commit hash covers the log ID, the LSN, the page counts, and the pages, "to ensure uniqueness and integrity verification". [V `crates/graft/src/core/commit_hash.rs` lines 148 to 163] No writer ID and no time go in.
- **After an ambiguous result** a failed precondition leads to recovery, with the comment "The commit already exists on the remote. This could be because: 1. Someone (including us) pushed the same commit (idempotency) 2. Someone (including us) pushed a DIFFERENT commit (divergence)". Equal hashes count as success. [V `crates/graft/src/rt/action/remote_commit.rs` lines 130 to 140; details in [graft.md](graft.md) section 3]
- **This is s4ql's mechanism.** Two clients that read a page holding 0 and both write 1 produce one hash, and both pushes succeed. Graft acknowledges a transaction locally before the push, and a different remote commit is divergence, not a merge, so equal content meaning one state is consistent with its model [I]. [graft.md](graft.md) recorded a plan to add "client provenance (client id and watermark)" to the commit from `plans/grove.md`; that file is no longer on `main` and could not be checked in this run.

### Litestream and LTX

- An LTX file carries `MinTXID` and `MaxTXID`, "Database checksum before applying the file" (`PreApplyChecksum`), "Database checksum after applying the file" (`PostApplyChecksum`), and `NodeID`, "Creator node ID; zero if unset". [V https://github.com/superfly/ltx/blob/main/README.md; [litestream-vfs.md](litestream-vfs.md)] The TXID is a position and the checksums are of the state, so two identical transactions on one state would differ only in `NodeID` and `Timestamp` [I]. The README does not describe a check for one's own file after an ambiguous write.

### turbopuffer and "S3 as a log" writeups

- turbopuffer's architecture page shows numbered WAL entries and a "CAS commit point", and says only "If a write returns successfully, data is guaranteed to be durably written to object storage". It says nothing about ambiguous results or IDs. [V https://turbopuffer.com/docs/architecture]
- Avinash Sajjanshetty's log on S3 names each record by its offset and writes it with `IfNoneMatch: "*"`, and stores a SHA-256 of the offset and the data. It does not handle a timeout after a write that landed, and it names zombie writers as an open problem. [V https://avi.im/blag/2024/s3-log/]

### Git, Dolt, and other content-addressed stores

- **Git.** A commit's name is the hash of its tree, parents, author and committer with their times in whole seconds, and message, so two identical commits made in the same second are one object. Junio Hamano described an empty commit that "received the same name" as the one before because it "had the exact same contents", as reproducibility and not a bug [I, from a search excerpt of https://public-inbox.org/git/7vhaqc7in6.fsf@alter.siamese.dyndns.org/; the archive refused automated reads]. Scripts that commit twice in a second meet this, and the usual answer is to make the commits differ. Git never has s4ql's problem, because a push of a commit that is already there means that the history already holds that exact state, and nothing was promised per push.
- **Dolt.** A commit's metadata holds name, email, description, and a committer and author time in milliseconds (`commitMetaTimestampKey`, `commitMetaUserTSKey`). [V `go/store/datas/commit_meta.go`] Equal metadata and an equal root would give equal commits [I]. No issue about this was found.
- **Irmin, Noms, ZFS:** not examined.

### Kafka

- **The identity is derived:** "Every new producer will be assigned a unique PID during initialization", with an epoch that a new instance with the same `transactional.id` bumps "so that the any previous zombie instance of the producer is fenced off", and "one sequence number per topic partition". [V https://cwiki.apache.org/confluence/display/KAFKA/KIP-98+-+Exactly+Once+Delivery+and+Transactional+Messaging]
- **After an ambiguous result** the producer resends with the same sequence number, and "Messages with a lower sequence number result in a duplicate error, which can be ignored by the producer". [V same]
- **Costs found.** The broker must keep the state per producer, and it expires: `transactional.id.timeout.ms` defaults to 7 days. [V same]

### Client request tokens and other ambiguous commits

- **DynamoDB `ClientRequestToken`:** "multiple identical calls have the same effect as one single call". The token is chosen by the client, is "valid for 10 minutes after the first request that uses it is completed", and a reuse with other parameters fails with `IdempotentParameterMismatch`. [V https://docs.aws.amazon.com/amazondynamodb/latest/APIReference/API_TransactWriteItems.html]
- **CockroachDB:** every transaction has a random UUID (`id is a unique UUID value which identifies the transaction`, [V `pkg/storage/enginepb/mvcc3.proto`]), but an ambiguous commit is handed to the client as `AmbiguousResultError`, "the response was not received and the final result is ambiguous". [V `pkg/kv/kvpb/errors.proto`; [commit-acknowledgement.md](commit-acknowledgement.md)]
- **etcd:** the client does not retry a `Put`, `Delete`, or `Txn` after a failure on an established connection, to keep at-most-once semantics, and has no request ID [I, from search excerpts of https://etcd.io/docs/v3.5/learning/design-client/ and `client/v3/retry.go`]. Jepsen found transactions in jetcd that ran more than once and lost updates. [I, https://jepsen.io/analyses/jetcd-0.8.2]
- **Datomic:** a timeout leaves the outcome unknown, and the advice is to read a recent database value, and to make retries idempotent with a `:db.unique/identity` attribute. [I, from search excerpts of https://docs.datomic.com/cloud/transactions/transaction-processing.html and https://forum.datomic.com/t/resolve-actual-ids-from-temporary-ids-in-case-of-communication-failures-during-transaction-executing/2495]

### ClickHouse: content identity and its fault

- **Inserts.** In a replicated MergeTree, "each block is assigned a unique `block_id`, which is a hash of the data in that block", and a block whose ID is in the deduplication log is not inserted. The log holds a bounded number of IDs (`replicated_deduplication_window`). "ClickHouse doesn't use the hash sum of the data when the token is provided", meaning `insert_deduplication_token`. [V https://clickhouse.com/docs/guides/developer/deduplicating-inserts-on-retries]
- **The fault.** Altinity's knowledge base shows four separate `insert into test_insert values(1);` that leave one row: "only one row has been inserted, the other rows were deduplicated". The remedies are to turn deduplication off or, since 22.2, an explicit token. [V https://kb.altinity.com/altinity-kb-schema-design/insert_deduplication/] This is s4ql's fault class in its plainest form: an identity taken from the content merges two operations that the clients meant as two.
- **The opposite fault.** Issue #112133 (open, opened 2026-07-27): a mutation whose response is lost is applied twice on retry, because "Mutations are not deduplicated by content (unlike INSERT, which carries a dedup token so retries are safe)". [V https://github.com/ClickHouse/ClickHouse/issues/112133]

## Table

| System | Identity of an attempt | Where it is stored | Use after an ambiguous result | Source |
|-|-|-|-|-|
| FoundationDB | random, 16 bytes (or user-chosen) | beside the data, `\xff\x02/idmp/`, expires | conflicting dummy commit, then look the ID up | `design/idempotency_ids.md` |
| Delta Lake | random UUID `txnId`; `appId` + `version` for applications | in the commit file (`commitInfo`, `txn`) | on a lost race, compare the winner's `txnId` with its own (off by default) | `PROTOCOL.md`; `OptimisticTransaction.scala` 3885 |
| Iceberg | random UUID in the metadata file's name; random snapshot ID | in the name | is the new location current or in `previousFiles` | `BaseMetastoreTableOperations.java` |
| Paimon | random UUID per writer + commit identifier | in each snapshot | walk the snapshots for (user, identifier, kind) | `FileStoreCommitImpl.java` 1350 |
| SlateDB | ULID from seeded `DbRand` per put; `writer_epoch` | object metadata, beside the object | HEAD on `AlreadyExists`, compare the ULID | `retrying_object_store.rs` |
| Graft | content hash (log, LSN, pages) | in the commit | equal hash at the LSN is success | `remote_commit.rs` 130 |
| LTX | TXID (position) + checksums of the state; `NodeID` | in the file's header and trailer | not described | ltx `README.md` |
| Git | content hash including whole-second times | the name | identical commits are one object by intent | [I] mailing list |
| Kafka | broker-assigned PID + epoch + sequence | broker state, expires (7 days) | resend; lower sequence is a duplicate | KIP-98 |
| DynamoDB | client token, up to 36 characters | service state, 10 minutes | resend with the same token | `TransactWriteItems` |
| CockroachDB | random UUID per transaction | transaction record | not resolved: `AmbiguousResultError` to the client | `errors.proto` |
| ClickHouse | content hash of the block, or an explicit token | Keeper, bounded window | resend; equal hash is dropped as duplicate | ClickHouse docs |
| s4ql today | content hash (parent, time, changes, statements) | the name | walk from head to parent, look for the name | `tx.go` 1658 |

## Did anyone have this exact fault?

- **Graft has the same mechanism** and takes equal content at the same position for its own success by design. Nothing in its tracker reports it as a fault.
- **ClickHouse has the fault class,** documented and long known: distinct inserts with equal content are merged into one, and an explicit token per insert was added to stop it.
- **Git has it by intent,** and it is harmless there because a commit is a state, not an acknowledged event.
- **No report of the exact sequence** (two writers, one parent, one clock reading, the loser of the lease acknowledging the winner's object) was found in the trackers of Delta, Iceberg, SlateDB, Graft, Dolt, or ClickHouse, in the searches made here.

## What follows for s4ql

The rule of the spec, "The transaction is committed if and only if its commit object is in the log", needs a name that identifies one transaction. Content does not, as Graft and ClickHouse show, and every system that resolves an ambiguous commit by looking in its own log or history looks for a value that only that attempt carries.

**(a) A random 128-bit value in each commit's header.**

- It is the practice of Delta (`txnId` in the commit file), Iceberg (a UUID in the name), SlateDB (a ULID per put), and FoundationDB (16 random bytes). Putting it in the content keeps the spec's rule that the name is the checksum of the content, with no metadata in the name, and the name inherits the uniqueness.
- The costs that others pay do not apply. FoundationDB's IDs and Kafka's producer state must be stored apart and expire; DynamoDB's tokens last 10 minutes; SlateDB's metadata may be unsupported by a store and then is skipped. An s4ql commit object is immutable, is never deleted while it is in the log, and is what the walk of `Tx.inLog` reads anyway, so the value never expires and needs no store feature.
- Determinism: SlateDB draws its ULIDs from `DbRand`, which tests seed. s4ql's `host.Host.Uint64` is the same arrangement: one PCG stream per simulation (`internal/sim/sim.go` line 176), crypto/rand in production (`internal/host/host.go` line 170). Two instances in one simulation draw from one stream and so get different values, as two processes do from crypto/rand, and a seed replays them exactly.
- It also narrows codex 4: with a unique value, `ErrExists` on upload can only be the writer's own earlier attempt or a foreign object, never another transaction's commit.
- What it costs: 16 bytes or so per commit, and the end of byte-identical commit objects for identical transactions, which nothing in s4ql relies on except the faulty case. The simulation's workloads, which put a random ID into every annotation, would then no longer hide the case.

**(b) The writer's instance ID in the header.**

- It is the shape of Paimon (`commitUser`) and Kafka (PID), Litestream's `NodeID`, and Delta's `appId`. Each of them pairs the writer's ID with a sequence number or epoch, because one writer may have several attempts in flight or to look for.
- In s4ql the instance ID is already 128 random bits per process (`internal/core/core.go` line 301), so (b) is a random value too, drawn once per process instead of once per commit.
- It is sound only together with a second rule: an instance resolves an unknown outcome before it commits again (the spec: "The instance then serves nothing until it knows"). If that ever loosened, or if two transactions of one instance with the same parent and content could both be looked for, the names would collide again. Paimon and Kafka need their sequence numbers for exactly this.
- It also puts an identifier of a process into every commit object forever, which (a) does not, and it gains nothing over (a) for the check.

**(c) Leave it and document it.**

- Graft and Git take this road, and for both it is right because equal content means one state there. In s4ql a commit is also an acknowledgement to a client, so this leaves a lost update. It needs two clocks that read the same instant, which a stopped clock, a clock of coarse resolution, or a restored VM can give; the format does not rule it out. ClickHouse's experience is that a content identity that merges operations is found by users and then needs a token.

**A fourth option: the lease tenure in the header.** An epoch or token per acquisition of the lease, as SlateDB's `writer_epoch` and Kafka's epoch are for fencing. a and b always hold the lease in different tenures, so their names would differ. It is (b) with a narrower scope, still needs the rule that an instance resolves before it commits again, and would need a counter or token in the manifest that s4ql's lease does not have today. It brings fencing information into the commit, which may be useful for diagnosis, but it does nothing for this fault that (a) does not.

**What does not help.** A monotonic commit time in Delta's style (the larger of the wall clock and the parent's time plus one) or a sequence number from the parent are equal for two writers on one parent. A check of the existing object's checksum on `ErrExists` (codex 4) does not separate two objects with the same bytes.

## What could not be determined

- Whether Graft added client provenance to its commits: `plans/grove.md`, cited in [graft.md](graft.md), is not on `main`, and the current commit hash has no writer identity.
- The exact wording of Junio Hamano's message and the etcd and Datomic sources: the mail archives (lore.kernel.org, public-inbox.org) refused automated reads, and the etcd and Datomic claims rest on search excerpts.
- Why Delta ships its idempotency check off by default; no design note or issue for it was found.
- How Hudi keeps instant times unique across writers, beyond the loop within one JVM.
- Irmin, Noms, ZFS, and WarpStream were not examined.
