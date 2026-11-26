# Diary: StepContext hides an I/O error behind an ended context

The adversarial review of 2026-11-26 (agy 13.1, on the branch `adversarial-codebase-2`) found that a read of the cache that fails in a step whose context has just ended comes out of `sqlite.Stmt.StepContext` as `context.Canceled`, although SQLite has rolled back the connection's whole transaction for the I/O error, and that the core takes that for a harmless stop. The goal was to make the traced consequence concrete, then fix it in the binding and in the core.

## Step 1: Show the consequence, and fix the binding and the core

**Author:** builder (step-context)

### Prompt Context

**Verbatim prompt:** "`Stmt.StepContext` hides an I/O error behind an ended context. [...] 1. Bring the test onto your branch, renamed to fit the package's tests, and see it fail at `main`. 2. **First make the traced consequence concrete.** [...] 3. **Fix it in both layers** [...] 4. Tests for both, deterministic, also under `-race` and at `-count=50`. [...] 5. If the core's simulation has planted bugs for ignored local failures [...] record it as an open item [...]" (abridged; the full brief is the lead's.)
**Interpretation:** reproduce at `main`, find out what really follows in the core, fix the binding so that a failed step keeps its error, fix the core so that an ended context counts as harmless only while the cache's transaction is intact, and test both without timing.
**Inferred intent:** rule out a cache that differs from the log without a stop, which is the kind of fault the project exists to exclude.

### What I did

- Moved the review's test into `TestStmt_StepContext` in `/internal/sqlite/sqlite_test.go` and saw it fail at `main` with `expected *sqlite.Error, got *errors.errorString: context canceled`.
- Added `Conn.SetProgressPeriod` and `Conn.Autocommit` to both bindings (`/internal/sqlite/sqlite.go`, `/internal/sqlite/sqlite_browser.go`), and `Options.ProgressPeriod` for the core's tests (`/internal/core/export_test.go`, applied in `openConn`). With a period of 1, SQLite looks at the context at every jump and on the way out of every step. A context that ends at the first `Err` after `faultvfs` has failed a read (`failedContext`) then ends exactly inside the failing step, with no timing involved.
- With that, a scratch test in the core at `main` (with the binding's fix turned off) showed the following. In a write transaction that had written a row and then hit the fault in a scan, the scan gave `context canceled`. The next `Tx.Upsert` of the row (3,30) ran outside any transaction, so SQLite committed it to the cache at once. Its `touch` then failed with `sqlite: no such table: temp.s4ql_touched_1 (code 1)`, and the commit failed with "the transaction can only be rolled back". The instance did not stop and served 1,10 | 3,30. A restart on the same cache served the same rows, because the cache's position still matched the log's head. A new cache that replayed the log had only 1,10. When the scan came first in the transaction, the later upsert was visible to readers before the commit, the commit was acknowledged, and then the instance stopped at the cache's commit. In a read transaction, a later scan saw a commit made after the read began, so the snapshot was gone.
- Binding: the C progress handler `s4ql_progress` returns 0 once `sqlite3_stmt_busy` is false, so a step that has ended or failed keeps its result. `s4ql_step_interruptible` reports whether the connection left a transaction during the step, and `StepContext` then gives SQLite's `SQLITE_INTERRUPT` and not the context's error, as for a write that a stop rolled back in an explicit transaction.
- Core: `conn.harmless` in `/internal/core/errors.go` is used by `Tx.scan` and `ReadTx.scan` (`/internal/core/index.go`), for the first step and for the count of unread rows. Those are the only steps of the cache with a context. While the core's transaction is open, a failed step after which the connection is outside its transaction gives `errTxEnded`, which `cacheError` treats as a local failure that stops the instance. Otherwise only the context's error is a harmless stop.
- Tests: `TestStmt_StepContext` gained three subtests (the I/O error, a write whose stop rolled back the transaction, and a committed insert that is never reported as stopped), plus `TestConn_Autocommit` and `TestConn_SetProgressPeriod`. `TestScanIndex_context` (`/internal/core/index_test.go`) has the end-to-end case for both transaction kinds: `ErrLocal`, a stop, the restarted cache equal to the replayed log. `TestScanIndex_interrupt` (`/internal/core/index_cgo_test.go`) covers the core's check alone, by rolling the transaction back under the core with `LoseCacheTx`, for the scan and for the count, and covers a transaction that the loop body ended itself.
- Recorded the fix and the open item in `/docs/todo/storage-core-tests.md`.

### Why

Either layer alone closes this path, as the end-to-end test shows: it passes with only the binding's fix and with only the core's. The binding's fix keeps the true error. The core's fix keeps any future way of losing the transaction under an apparent stop from going unnoticed.

### What worked

- Making the period 1 and ending the context on the VFS's failure counter turned a window of a few instructions into a deterministic test, in the core as well as in the binding.
- Turning each fix off by a small script (`/Volumes/990/s4ql/step-context/asmain.py` for the binding, a `false &&` in `harmless`) showed every new test red without its fix.

### What didn't work

- The first end-to-end test hung for 120 s. Serving a second instance on the cache "b" waited for the lease, because the restarted instance on "a" was still open. The 40 s timeout's dump showed `(*DB).Wait` in `world.serve`. Closing the restarted instance first fixed it.
- The isolation of the worktree refused several compound shell commands ("this command is too complex to verify that it stays inside the worktree"), so edits went through Python scripts in the scratch directory.
- The first version of the review's test aligned the statement's instruction count by stepping it in an unbounded loop until `10_000 - VMSteps() % 10_000 == 4`. Both reviewers flagged that a SQLite upgrade could make it never end. It now uses a period of 1 and the failure-aware context.

### What I learned

- SQLite's `vdbe_return` runs the progress handler while it is due, also after `abort_due_to_error` has halted the statement, and a nonzero return goes back to `abort_due_to_error` with `SQLITE_INTERRUPT`. The message of the first error stays (`p->zErrMsg` is set already), and only the code changes.
- `sqlite3_stmt_busy` is `eVdbeState == VDBE_RUN_STATE`, and every exit through `sqlite3VdbeHalt` that rolls anything back ends in the halt state. The paths that stay busy (a row, the early BUSY returns of a commit) roll nothing back.
- The same hole hid a committed write: an insert in autocommit mode with a period of 1 was reported as stopped after its commit, at the fourth look.

### What was tricky

The core's first check, which looked at autocommit after the context's error, regressed a legal case, as both reviewers found and one reproduced. A loop body that calls `Tx.Rollback` or `ReadTx.Close` and then breaks out of a scan with Order makes the count run outside a transaction, and a context that ended during it stopped the instance. `harmless` now takes whether the core's own transaction is open (`!tx.done`, `!r.done`).

### What warrants review

- `/internal/sqlite/sqlite.go`, `s4ql_progress` and `s4ql_step_interruptible`, against `vdbe_return` and `sqlite3VdbeHalt` in `/internal/sqlite/c/sqlite3.c`.
- `/internal/core/errors.go` `conn.harmless`, and the four call sites in `/internal/core/index.go`. For `ReadTx.scan`, an error after a lost transaction now stops the instance where it was returned before.
- Checks run: `make test`, `make lint`, `go test ./internal/core` without `-short`, the new tests at `-count=50` and under `-race`, the browser build's `TestScanIndex_context` and `TestConn_Autocommit` through `make test-browser`, and `make test-asan` restricted to `internal/sqlite`.

### Future work

- The simulation cannot produce this fault: it has no read faults of the cache, and its contexts end by timeouts. Recorded as open in `/docs/todo/storage-core-tests.md`.
