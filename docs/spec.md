# Spec

This document holds only what Markus and maragubot have explicitly agreed to write here. Nothing is added or changed without that agreement. Keep it concise.

## Terms

- **S3:** any S3-compatible object store that provides all three of the following.
  - **Strong consistency:** a read or listing always reflects every write and delete that has completed.
  - **Compare-and-swap:** a write can be made to succeed only if the object is unchanged since it was read.
  - **Create-if-absent:** a write can be made to succeed only if no object exists at that key.

## SQL

- **SQLite is the result oracle.** Results are judged against SQLite's.
- **A strict subset of SQLite's SQL.** Because SQLite is the oracle, the dialect is SQLite's, and everything supported is also valid SQLite.
- **Strict tables only.** Only `STRICT` tables are supported. Other restrictions may be added as they are found.

## Testing

- **Deterministic simulation.** The production code runs unchanged in a simulation where object storage, time, and randomness sit behind interfaces driven by one seed. The same seed always replays the same run, including its failures.
- **Buggify.** The production code contains marked points where, in simulation only, it does something legal but unusual, such as returning an error, adding a delay, or taking a rare branch. Each run switches on a random subset of these points, so every run stresses a different part of the code.
- **Simulation and real S3 have separate jobs.** The simulation, with a simulated object store, hunts for bugs. Real S3 checks that the simulated store behaves like the real one, and runs the integration tests.

## Constraints

- **Always a lease.** An instance must hold the database's lease to read or write, and there is no mode without it. The lease is recorded in S3 and has a timeout. Every write renews it, and so does a periodic renewal.
- **Reads come from the local cache.** While an instance holds the lease, it reads from its local cache without contacting S3. Writes go to S3.
- **One commit at a time.** Commits to a database happen strictly one after another, and the storage enforces this, not the lease or a clock. Only the lease holder writes, and any instance can become the holder.
