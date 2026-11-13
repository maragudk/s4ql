# s4ql

> [!WARNING]
> This project is currently an experiment. It is not ready for use, and anything in it may change.

[![Docs](https://pkg.go.dev/badge/maragu.dev/s4ql)](https://pkg.go.dev/maragu.dev/s4ql)
[![CI](https://github.com/maragudk/s4ql/actions/workflows/ci.yml/badge.svg)](https://github.com/maragudk/s4ql/actions/workflows/ci.yml)

s4ql ("simple storage service + structured query language") is an embedded SQL database engine for Go, in the spirit of SQLite, but with all data stored in S3. It is ACID per database, speaks a strict subset of SQLite's SQL, and serves reads from a local SQLite cache that can be rebuilt from S3 at any time.

What has been agreed is in [docs/spec.md](docs/spec.md), and the supported SQL is in [docs/roadmap.md](docs/roadmap.md).

Made with ✨sparkles✨ by [maragu](https://www.maragu.dev/): independent software consulting for cloud-native Go apps & AI engineering.

[Contact me at markus@maragu.dk](mailto:markus@maragu.dk) for consulting work, or perhaps an invoice to support this project?

## Status

As of 2026-11-13:

- **Built: the storage.** A database is a log of commits in S3, with a local SQLite cache. One instance at a time holds a lease on it. Commits, recovery after a crash, and replay of the log work. They are tested in a deterministic simulation and against a real S3-compatible bucket.
- **Not built: the SQL.** There is no parser, no engine, and no public API yet, so the database cannot be used.
- **Next:** the first slice of SQL in the [roadmap](docs/roadmap.md): `create table`, `insert`, `select`, and `drop table` for one integer column, checked against SQLite.

What is open in detail is in [docs/todo.md](docs/todo.md).
