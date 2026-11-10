# Spec

This document holds only what Markus and maragubot have explicitly agreed to write here. Nothing is added or changed without that agreement. Keep it concise.

## Terms

- **S3:** any S3-compatible object store that provides all three of the following.
  - **Strong consistency:** a read or listing always reflects every write and delete that has completed.
  - **Compare-and-swap:** a write can be made to succeed only if the object is unchanged since it was read.
  - **Create-if-absent:** a write can be made to succeed only if no object exists at that key.

## SQL

- **We write the SQL engine.** The engine parses, plans, and executes SQL itself. SQLite does not run the application's SQL.
- **SQLite is the result oracle.** Results are judged against SQLite's.
- **A strict subset of SQLite's SQL.** Because SQLite is the oracle, the dialect is SQLite's, and everything supported is also valid SQLite.
- **Strict tables only.** Only `STRICT` tables are supported. Other restrictions may be added as they are found.

Which SQL is supported today, and what comes next, is in the working document [roadmap.md](roadmap.md).

## Testing

- **Deterministic simulation.** The production code runs unchanged in a simulation where object storage, time, and randomness sit behind interfaces driven by one seed. The same seed always replays the same run, including its failures.
- **Buggify.** The production code contains marked points where, in simulation only, it does something legal but unusual, such as returning an error, adding a delay, or taking a rare branch. Each run switches on a random subset of these points, so every run stresses a different part of the code.
- **Simulation and real S3 have separate jobs.** The simulation, with a simulated object store, hunts for bugs. Real S3 checks that the simulated store behaves like the real one, and runs the integration tests.

## Concurrency

- **One write transaction at a time.** Per database, a second writer waits until the first has committed or rolled back.
- **Reads run in parallel.** Each read transaction sees one consistent state of the cache, even while a write is in progress. The number of connections in the database pool sets how many reads run at once.
- **State changes run on a single path.** Write transactions, the commit steps, lease renewal, and cache catch-up run on one path, written as a state machine that does no I/O itself: it receives events and answers with the next action. This is what lets the simulation replay a run exactly.

## Constraints

- **Many databases per bucket.** Each database is a key prefix, so one S3 bucket can hold many databases. This enables one database per user, and it suits write-heavy workloads by spreading them across databases. The common case is still one database, meaning one prefix.
- **ACID is per database.** Transactions are ACID within one database, and no transaction spans databases.
- **Always a lease.** An instance must hold the database's lease to read or write, and there is no mode without it. The lease is recorded in the database's manifest, the object `manifest.json` under its prefix, and has a timeout. Every write renews it, and so does a periodic renewal. By default the lease lasts 30 seconds and is renewed 20 seconds after the last renewal.
- **The lease belongs to a running process.** The holder is identified by a random ID that the process holds in memory. A clean shutdown releases the lease; otherwise it times out.
- **The holder keeps the lease.** While the holder keeps renewing the lease, no other instance can take it. Another instance gets the lease only after the holder has released it or it has timed out, and it waits or fails until then.
- **Reads come from the local cache.** While an instance holds the lease, it reads from its local cache without contacting S3. Writes go to S3.
- **The local cache is a SQLite database.** It holds the same tables and rows as the database, and the engine uses it as its local row store. S3 holds the truth in our own format, and the cache can be rebuilt from it at any time.
- **S3 holds a log.** The database in S3 is a log of immutable change files, one per commit, with no snapshots in the first design.
- **The log is a linked list.** Each change file is named by the SHA-256 checksum of its content, with no metadata in the name, and records the name of the change file before it. The manifest points only to the newest change file. As with a branch in git, that one pointer identifies the whole history, and a reader can verify every file against its name.
- **Change files are row-based.** A change file records results, not statements: the rows a commit inserted or changed, the keys of the rows it deleted, and its schema changes, as the net effect of the commit. Replaying the log applies these results and does not run SQL.
- **Statements are kept as metadata.** Next to the results, a change file stores the SQL statements that produced them and their non-deterministic inputs, such as the time and random seeds. They are evidence, not the truth, and replay never depends on them.
- **Storage formats are backward and forward compatible.** A format stored in S3 can gain fields without breaking older or newer readers, in the style of protocol buffers, though not necessarily with protocol buffers.
- **The manifest maps branch names to log pointers.** The map is named `branches` and has one entry, `main`. Other branches are not designed yet, but can be added later as further entries.
- **The manifest has a format version.** A `version` field says which version of the manifest format it uses.
- **The manifest swap is the commit.** A commit uploads the transaction's changes as a new file, then replaces `manifest.json` with a compare-and-swap so that it lists that file. The transaction is committed when the swap succeeds, and not before.
- **Our own SQLite binding.** The cache and the oracle use the official SQLite C code, at a version we pin, through a small cgo binding of our own. The module therefore needs cgo.
- **The cache is never ahead of S3.** A commit swaps the manifest first and commits to the cache second. The cache records the name of the last change file it has applied, in the same local transaction as the rows. An instance whose cache is behind the manifest applies the missing change files before it serves anything. A cache whose recorded position is not in the log is refused loudly, not rebuilt silently.
- **A commit is acknowledged once S3 has it.** `Commit` returns success if the manifest swap succeeded. Before it returns, the instance commits the transaction to the local cache, or stops if that fails. A read that starts after a commit is acknowledged therefore always sees it.
- **A local I/O failure stops the instance.** If reading or writing the local cache fails, the instance stops serving for good. Running reads and waiting writes fail, every later call returns an error that carries the cause, and the instance signals the stop to the application. It then tries once to release the lease. It does not retry, and it does not reopen itself.
- **Recovery from a stop is a normal start.** The application reopens the database, and the instance catches the cache up from its recorded position, as after a crash. If the cache cannot be opened, the start fails, and the cache is not rebuilt automatically.
- **One commit at a time.** Commits to a database happen strictly one after another, and the storage enforces this, not the lease or a clock. Only the lease holder writes, and any instance can become the holder.
