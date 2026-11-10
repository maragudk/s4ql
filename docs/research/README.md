# Research notes

These are raw research notes gathered on 2026-11-10 while planning the project. AI research agents wrote them from the cited sources. The notes tag claims as verified or inferred. They are a snapshot and are not kept up to date.

The distilled ideas are in [../inspiration.md](../inspiration.md). Agreed decisions are in [../spec.md](../spec.md).

- [object-storage-architectures.md](object-storage-architectures.md): survey of turbopuffer, SlateDB, Delta Lake and Iceberg, plus S3 and Tigris semantics.
- [testing-strategies.md](testing-strategies.md): testing techniques and Go tooling.
- [external-test-suites.md](external-test-suites.md): external SQL correctness suites, benchmarks, and a Postgres wire adapter.
- [s3-fault-injection.md](s3-fault-injection.md): tools for injecting faults into S3.
- [slatedb.md](slatedb.md): notes on SlateDB.
- [graft.md](graft.md): notes on Graft.
- [foundationdb.md](foundationdb.md): notes on FoundationDB.
- [litestream-vfs.md](litestream-vfs.md): notes on Litestream, LTX, and the Litestream VFS.
- [tigris-probe-results.md](tigris-probe-results.md): measured behaviour of a real Tigris bucket.
- [sqlite-cache-and-oracle.md](sqlite-cache-and-oracle.md): design space for SQLite as local cache and as test oracle, change-capture mechanisms, Go bindings, and prior art.
- [sqlite-best-practices.md](sqlite-best-practices.md): best practice for SQLite in server-side apps, the legacy behaviours to reject or match, the costs of STRICT plus WITHOUT ROWID, and migration practice.
- [sqlite-docs-inventory.md](sqlite-docs-inventory.md): inventory of the SQLite 3.54.1 SQL documentation (features, legacy flags, STRICT and WITHOUT ROWID rules, oracle configuration, a proposed slice ladder, and hard semantic areas).
- [commit-acknowledgement.md](commit-acknowledgement.md): what comparable systems tell the client, and what the node does, when a commit is durable in the authoritative place and the local apply then fails.
