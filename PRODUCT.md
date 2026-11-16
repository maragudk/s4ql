# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Stack

Static HTML/CSS for the project site, in `site/`, with no build step, deployed to GitHub Pages by a GitHub Actions workflow at www.s4ql.com. Confirmed by Markus on 2026-11-16.

## Users

- Go developers curious about an embedded SQL database whose only infrastructure is S3. Nothing is usable yet, so their job is to understand the idea and the state of the work, and to follow the repository.
- People interested in building software with AI agents: how to get high trust in code through testing and verification while no human reads the code.

## Product Purpose

s4ql ("simple storage service + structured query language") is an embedded SQL database engine for Go, in the spirit of SQLite, with all data stored in S3. There is no server: the Go application talks to S3 directly through the module. It is ACID per database, speaks a strict subset of SQLite's SQL, and reads from a local SQLite cache that can be rebuilt from S3 at any time. One bucket holds many databases, one per key prefix, which enables one database per user.

Success for the project is trust in correctness. Success for the site is that a visitor understands what s4ql is, why it exists, how far it has come, and follows the repository.

## Positioning

- The storage layer is s4ql's own, designed for S3's constraints (strong consistency, compare-and-swap, create-if-absent), not a SQLite VFS or page replication.
- s4ql writes its own SQL engine and parser. SQLite is the result oracle: whatever the engine accepts behaves exactly as in SQLite, and everything else is rejected, never reinterpreted.
- The code is written and owned by AI agents (maragubot). No human reviews it. Trust comes from deterministic simulation with buggify, SQLite as the oracle, property-based tests, fuzzing, mutation testing, and integration tests against real S3 (Tigris). Markus wants this said plainly.

## Operating Context

The repository is github.com/maragudk/s4ql, module `maragu.dev/s4ql`. Agreed rules live in `docs/spec.md`, the SQL slices in `docs/roadmap.md`, open work in `docs/todo.md`, research in `docs/research/`, and the conversations behind decisions in `docs/conversations/`.

## Capabilities and Constraints

- Built: the storage core. A database is a linked list of immutable commit objects in S3, named by SHA-256, with a manifest swapped by compare-and-swap. One instance at a time holds a lease. Commits, crash recovery, and replay work, tested in deterministic simulation and against a real S3-compatible bucket.
- Not built: the SQL. Only the tokenizer exists. No parser, no engine, no public API, so the database cannot be used.
- Next: slice 1 of the roadmap, `create table`, `insert`, `select`, and `drop table` for one integer column, checked against SQLite.
- The project is an experiment. Anything may change.
- Status changes as work merges; the README's "Status" section is the source.

## Brand Commitments

- Name: s4ql, lowercase. Domain s4ql.com.
- Made by maragu (www.maragu.dev), Markus Wüstenberg's company. The README's line: "Made with ✨sparkles✨ by maragu".
- Plain, precise English. No hype.

## Evidence on Hand

- README.md, docs/spec.md, docs/roadmap.md, docs/todo.md.
- No users, benchmarks, testimonials, or release exist. Do not invent them.

## Product Principles

- Correctness before features: claim only what tests show.
- Honest status: say what is built and what is not.
- Reject, never reinterpret.
- Ambition argued from correctness and verification, not from effort.
