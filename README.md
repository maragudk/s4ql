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

As of 2026-11-21:

- **Works now,** through `database/sql`, with the package `maragu.dev/s4ql` as the driver:
  - `create table` and `drop table` of a strict table with one or more columns, exactly one of them the primary key.
  - The column types `int`, `integer`, `real`, `text`, and `blob`. A value must have the exact type of its column.
  - Columns that are `not null`, and columns that take null.
  - `insert` of one row, with literals and arguments, with or without a list of columns.
  - `select` of named columns or `*`, with a `where` of comparisons and of `is null` and `is not null`, joined by `and`. Rows without `order by` come in a varying order.
  - `update` and `delete`, with or without that `where`. An update may change the primary key, and `RowsAffected` is what SQLite reports.
  - `create index` and `drop index`, on one or more columns, each ascending or descending, not unique. The engine picks the index for a `where` by fixed rules.
  - `if not exists` on `create table` and `create index`, and `if exists` on `drop table` and `drop index`.
  - Arguments for a plain `?`, directly and through prepared statements.
  - Transactions, also read-only ones.
  - Storage: a database is a log of commits in S3, with a local SQLite cache. One instance at a time holds a lease on it. Commits, recovery after a crash, and replay of the log work.
- **How it is checked:** against SQLite with generated statements, in the engine and through `database/sql`, in a deterministic simulation with planted bugs, and with mutation testing and fuzzing. The storage is also tested against a real S3-compatible bucket in CI.
- **Not built:** `or` and other expressions in a `where`, `order by` and `limit`, more than one row per `insert`, defaults, and a statement while rows of the same transaction are open.
- **Next:** mutation testing of indexes and of slice 10, then slice 11 (`default`). After it comes the rest of what the first application needs (the roadmap's [milestones](docs/roadmap.md#milestones)).

The slices, and how each is checked, are in [docs/roadmap.md](docs/roadmap.md). What is open in detail is in [docs/todo.md](docs/todo.md).
