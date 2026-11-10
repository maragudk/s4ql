# Spec

This document holds only what Markus and maragubot have explicitly agreed to write here. Nothing is added or changed without that agreement. Keep it concise.

## SQL

- **SQLite is the result oracle.** Results are judged against SQLite's.
- **A strict subset of SQLite's SQL.** Because SQLite is the oracle, the dialect is SQLite's, and everything supported is also valid SQLite.
- **Strict tables only.** Only `STRICT` tables are supported. Other restrictions may be added as they are found.

## Constraints

- **One commit at a time.** Commits to a database happen strictly one after another, and the storage enforces this, not a lock or a clock. Any process may write; there is no designated writer.
- **Retries over waiting.** A transaction that loses a race to another commit is retried by the engine where possible, and otherwise fails with a retryable error. Correctness never depends on the retry.
