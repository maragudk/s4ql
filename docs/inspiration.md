# Inspiration

This document lists the ideas we take as inspiration from four prior projects, for both design and testing. Keep it concise, preferably bullet points only.

## Graft

A Rust SQLite extension that stores page-based volumes directly on object storage, with no server in between.

### Design

- **One immutable object per sequence number as the commit point.** A commit is the create-if-absent of `log/{seq}`, with no manifest, mutable pointer, or lock, and the object holds everything needed to read the commit. A lost race is a 412 with nothing to roll back, and none of Graft's known v2 bugs came from this protocol.
- **Data before the commit object.** Data objects are uploaded under unique names before the commit object, so a failed commit leaves only an orphan for garbage collection.
- **Resolve ambiguous outcomes by re-reading the slot.** Before the conditional PUT, durably record the target sequence number and a content hash of the commit. After a timeout, 412, or restart, read the slot: our hash means we won, another hash means someone else did, and an empty slot means try again.
- **Find the head by probing, not listing.** GET `log/{head+1}` until it returns 404, which costs one GET when the cache is current and relies only on read-after-write consistency.
- **One log, no layers.** A snapshot is a position in the single log on object storage plus the open transaction's own writes. Most of Graft's v2 bugs lived in code that layered a local log over the remote one.

### Testing

- **Bank workload with full transfer history.** Every transfer also inserts a row describing itself, so the validator re-derives each balance from history and catches lost or duplicated commits, not only a wrong total.
- **Named crash points with a reachability check per recovery branch.** The engine can crash before recording its intent, before the commit PUT, after it, and during recovery, and every recovery outcome must occur in some run.
- **Local state survives killed processes.** Each test process picks up the data directory a killed process left behind, so crash recovery is tested without a separate restart step.

## FoundationDB

A distributed, strictly serializable key-value store, known for testing its own code in deterministic simulation.

### Design

- **Definite outcome after ambiguity.** FoundationDB's `commit_unknown_result` guarantees that a transaction that did not commit never will. On object storage a delayed PUT can still land, so before concluding "not committed" we occupy the slot with our own conditional write.
- **Hard maximum transaction lifetime.** FoundationDB rejects transactions older than five seconds and splits long work into many short transactions. A fixed lifetime bounds how long a snapshot's objects must survive, which is what lets garbage collection delete safely.
- **Schema-generation fencing for DDL.** In the FoundationDB SQL Layer, every transaction reads a schema generation number and every DDL statement increments it. DDL therefore conflicts with every concurrent transaction, and no transaction runs against a stale cached schema.

### Testing

- **Deterministic simulation of the production code.** The engine runs unchanged, with object storage, time, and randomness behind interfaces driven by one seed, so every failure replays exactly. A sample of seeds runs twice and compares the final random draw to catch accidental nondeterminism.
- **BUGGIFY with per-site, per-run activation.** The code marks places where it may take a rare but legal path; each run enables a random quarter of those sites, so runs explore different regions instead of drowning in every fault at once. Tuning knobs are randomised the same way, so no tuning value becomes necessary for correctness.
- **Self-checking workloads.** Cycle keeps a ring of pointers that must stay one ring, Sideband hands a commit to another client out of band and requires it to be visible, and the serializability workload checks that A; B; C and (A+B); C give the same results. These oracles hold with many concurrent clients and with ambiguous commits.
- **Exactly-once under ambiguous commits.** Each logical transaction writes a client-generated ID and retries through injected ambiguous outcomes. At the end, each ID appears exactly once, or never if the call reported a definite failure.
- **Quiesce, recover, then check.** At the end of a run the faults stop, the system must recover, and only then do the consistency check and the workload checks run. Oracles that hold only without faults get their own calm runs.
- **Coverage probes for rare paths, enforced.** A probe counts how many runs reach a rare condition, such as a lost commit race. FoundationDB only reports unhit probes; we fail the test suite when one is never hit.
- **Every found bug becomes a named regression test.** Any code change alters the random sequence, so a failing seed does not survive the fix, and the scenario must be written down as its own test.

## SlateDB

An embedded key-value store, written in Rust, built as an LSM tree on object storage.

### Design

- **Guard every garbage-collected namespace with a boundary.** Before deleting sequence-numbered objects, GC advances a monotonic boundary marker with compare-and-swap, and a writer treats any create-if-absent at or below the boundary as failed, so a stalled writer cannot reuse a deleted name. SlateDB guarded only its metadata this way, and its unguarded WAL namespace could lose acknowledged writes.
- **Unique token on every conditional write.** Each conditional PUT carries a fresh random ID in object metadata. On a 412 after a retry, a HEAD that finds our ID shows that our earlier attempt succeeded.
- **Fail-stop on unresolved ambiguity.** When an outcome cannot be resolved, the handle closes and recovery re-reads the bucket. The caller gets an explicit unknown-outcome error, not a guess.
- **Invariant checks before every shared-state update.** Registered invariants run on each new manifest before its conditional write, and impossible states panic.
- **Garbage collection by reference, not by clock.** Delete only objects that a committed state once referenced and has since retired, and that no reader pins. SlateDB's time-based cutoffs assume bounded clock skew, and its GC deleted live files more than once.

### Testing

- **Formal models of the commit and GC protocol, run in CI.** SlateDB's FizzBee models, with ghost state separating "the object exists" from "the writer reported success", found real fencing and GC bugs. One bug needed four writers to appear, so we check ours in CI with at least four.
- **Faults on the response side.** The fault-injecting store can apply a PUT and then return an error, which is exactly the ambiguous case that request-side faults never produce.
- **Ban direct clock and randomness calls.** A lint forbids the standard clock, sleep, random, and ID-generation calls, so all time and randomness flow through injected sources that the simulation controls.
- **Round-trip encoders across the full value range.** A SlateDB encoding bug that corrupted the manifest after about 4.5 billion writes hid behind a property-test generator that limited its inputs. Generators for every stored format span the full range of their types.

## Turbopuffer

A vector and full-text search engine that keeps all data on object storage, with memory and NVMe caches in front.

- **Object storage is the only source of truth.** The caches are read-through and write-through, and any node can serve any namespace because all state lives in the bucket.
- **A manifest swap is the commit.** New data is uploaded as its own file and then committed into a manifest with compare-and-swap. A stale writer's swap fails because the manifest has changed, and the manifest is never deleted, so its name cannot be reused.
- **One JSON file as the coordination layer.** The indexing work queue is a single JSON file updated by compare-and-swap, with no other metadata service. A deposed worker learns it has been replaced when its swap fails.
- **Group commit.** Concurrent writes to a namespace are batched into one WAL entry, so the number of object-storage writes no longer follows the number of requests.
- **Acknowledge only when durable.** A write returns success only once it is durably written to object storage.
- **Strong reads by default, with one cheap freshness check.** Each strong query first makes a conditional GET, about 10 ms, to confirm that the cache holds the latest write, and never LISTs on the critical path. Eventual consistency with bounded staleness is an explicit opt-in.
- **Asynchronous indexing and compaction.** Committed WAL entries are indexed off the write path, and strong reads replay the entries not yet indexed on top of the index.
- **Backpressure on the unindexed backlog.** Writes are rejected once unindexed data exceeds a limit, which bounds how much a strong read must replay.
- **Bound object-storage round trips per cold query.** Each round trip costs about 100 ms, so the planner aims for at most three sequential round trips and trades extra round trips against larger reads.
- **Consistency over availability.** When object storage is unreachable, Turbopuffer gives up availability rather than consistency.
