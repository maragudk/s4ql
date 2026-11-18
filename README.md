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

As of 2026-11-19:

- **Built: the storage and the SQL engine for slice 1.** A database is a log of commits in S3, with a local SQLite cache. One instance at a time holds a lease on it. Commits, recovery after a crash, and replay of the log work. They are tested in a deterministic simulation, and also run by hand against a real S3-compatible bucket. The engine runs `create table`, `insert`, `select *`, and `drop table` for one integer column. It is checked against SQLite statement by statement, in a simulation with faults, and by fuzzing. The SQL parser agrees with SQLite on all 138,926 statements on which SQLite gives a clear verdict, of a corpus of 139,234.
- **Not built: the public API.** The `database/sql` driver does not exist yet, so an application cannot use the database.
- **Next:** the driver. It completes slice 1 of the [roadmap](docs/roadmap.md).

What is open in detail is in [docs/todo.md](docs/todo.md).
