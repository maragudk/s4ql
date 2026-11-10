# 1. Write our own cgo binding to the official SQLite C code

Date: 2026-11-10

## Decision

Write a small cgo binding of our own to the official SQLite amalgamation, vendored in this repository at a version we pin and compiled with options we choose. Use it for both roles SQLite has in this project: the local cache and the result oracle. Keep it behind a small internal interface.

This replaces an earlier decision, made the same day, to start with `modernc.org/sqlite`.

## Why

- **The oracle is the real thing.** Results are judged against SQLite's own C code, not a machine translation of it.
- **We update on our own terms.** The SQLite version and its compile-time options are ours to choose and to change.
- **We need little of it.** The engine needs to open a database, prepare, bind, step, read columns, and run transactions. A binding that does only that is small enough to test thoroughly.
- **The interface can fit the engine.** For example, it can return several rows per call into C, which a general-purpose `database/sql` driver cannot.
- **No third-party dependency** sits between the engine and SQLite. SQLite's source is in the public domain.

## Costs and risks

- **Users of the module need cgo:** a C compiler at build time, and cross-compiling becomes harder. Markus accepts cgo, with pure Go preferred where it costs nothing.
- **The binding is memory-unsafe code** that Go's race detector and memory safety do not cover. Keep it small, run its tests under the C address sanitizer, fuzz it, and cross-check it in tests against `modernc.org/sqlite` at the same SQLite version: two independent routes to SQLite must give the same answers.

## Alternatives considered

- `modernc.org/sqlite`: pure Go and current, but a translation of the C code, with its own release schedule.
- `github.com/mattn/go-sqlite3`: cgo and mature, but bundles its own copy of SQLite and far more surface than the engine needs.
- `github.com/ncruces/go-sqlite3`: pure Go, but its maintainer flagged the runtime change of March 2026 as a stability risk.

Sources: `docs/research/sqlite-cache-and-oracle.md`.
