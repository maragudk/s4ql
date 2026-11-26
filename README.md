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

As of 2026-11-26:

- **Works now,** through `database/sql`, with the package `maragu.dev/s4ql` as the driver:
  - `create table` and `drop table` of a strict table with one or more columns, exactly one of them the primary key, and a `default` with a literal on a column.
  - The column types `int`, `integer`, `real`, `text`, and `blob`. A value must have the exact type of its column.
  - Columns that are `not null`, and columns that take null.
  - `insert` of one row, with literals and arguments, with or without a list of columns. A column that the list leaves out gets its default.
  - `select` of named columns or `*`, with a `where` of comparisons and of `is null` and `is not null`, joined by `and`. A comparison may have its value on either side. Rows without `order by` come in a varying order.
  - `order by` of one or more columns, each ascending or descending, and `limit` with a number or `?`, on `select`.
  - A scalar subquery in a `where`, on the right or the left of a comparison, in a `select`, an `update`, and a `delete`. It is not correlated, and runs once before the statement changes anything.
  - Arithmetic: `+`, `-`, `*`, `/`, and a unary `-` on integers and reals, in the `set` of an `update` and in a `where`. An overflow, a division by zero, and an infinite result are errors. A column may be compared with a column of the same type.
  - Text: `||`, `lower`, and `hex`, where arithmetic runs.
  - Upserts: `insert ... on conflict (key) do nothing` and `do update set ... [where ...]`, with `excluded.<column>`, also in arithmetic and text.
  - `update` and `delete`, with or without that `where`. An update may change the primary key, and `RowsAffected` is what SQLite reports.
  - `returning` at the end of an `insert`, an `update`, and a `delete`, with column names or `*`. Outside a transaction, the statement commits before it gives its first row.
  - `create index` and `drop index`, on one or more columns, each ascending or descending, not unique. The engine picks the index for a `where` by fixed rules.
  - `if not exists` on `create table` and `create index`, and `if exists` on `drop table` and `drop index`.
  - Arguments for a plain `?`, directly and through prepared statements.
  - Transactions, also read-only ones.
  - Several statements in one `Exec`, run one after another. `Query` and `Prepare` take one.
  - Storage: a database is a log of commits in S3, with a local SQLite cache. One instance at a time holds a lease on it. Commits, recovery after a crash, and replay of the log work.
- **How it is checked:** against SQLite with generated statements, in the engine and through `database/sql`, in a deterministic simulation with planted bugs, and with mutation testing and fuzzing. The storage is also tested against a real S3-compatible bucket in CI.
- **Not built:** `or`, other functions and operators in a `where`, arithmetic and text outside `set` and `where`, `offset`, more than one row per `insert`, and a statement while rows of the same transaction are open.
- **Next:** what is decided next is in the [roadmap](docs/roadmap.md#at-a-glance) by area: more operators and functions, and expressions in more places, grouping and joining, and a limit on the memory of an operator.

The roadmap by area, and the slices that were built with how each is checked, are in [docs/roadmap.md](docs/roadmap.md). What is open in detail is in [docs/todo.md](docs/todo.md).
