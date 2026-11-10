# 1. Start with `modernc.org/sqlite` as the SQLite binding

Date: 2026-11-10

## Decision

Use `modernc.org/sqlite` for both roles SQLite has in this project: the local cache and the result oracle. Keep it behind a small internal interface so another binding can replace it.

## Why

- It is pure Go, which Markus prefers. cgo is acceptable but not preferred.
- It tracks current SQLite (3.54.1 in v1.62.0, released 2026-11-07), which is past the WAL-reset corruption bug fixed after 3.51.2.
- It has a `database/sql` driver and is actively maintained.
- `github.com/ncruces/go-sqlite3`, the other maintained pure-Go option, changed its WebAssembly runtime in March 2026, and its maintainer flagged that as a stability risk.

## What could change it

- **Speed.** Benchmarks from March 2026 put the pure-Go bindings at about 1.6 to 2 times slower than cgo on inserts. Reads from the cache are on the hot path, so measure against `github.com/mattn/go-sqlite3` (cgo) once the engine can run a read workload, and switch if the difference matters.
- **Oracle fidelity.** `modernc.org/sqlite` is a machine translation of SQLite's C code, not the C code itself. Cross-check the oracle against a C build of the same SQLite version in CI, so a translation bug cannot hide behind the oracle.

Sources: `docs/research/sqlite-cache-and-oracle.md`.
