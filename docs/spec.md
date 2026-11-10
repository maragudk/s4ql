# Spec

This document holds only what Markus and maragubot have explicitly agreed to write here. Nothing is added or changed without that agreement. Keep it concise.

## Constraints

- **One commit at a time.** Commits to a database happen strictly one after another, and the storage enforces this, not a lock or a clock. Any process may write; there is no designated writer.
- **Retries over waiting.** A transaction that loses a race to another commit is retried by the engine where possible, and otherwise fails with a retryable error. Correctness never depends on the retry.
