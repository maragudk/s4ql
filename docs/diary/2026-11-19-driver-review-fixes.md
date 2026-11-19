# Diary: The follow-ups of the review of the driver

The driver of package `s4ql` was merged in `66f8463` after an independent review that judged it sound and named follow-ups. This task is those follow-ups and nothing more. The work is on the branch `driver-review-fixes`, which starts from `main` at `66f8463`.

## Step 1: The brief, and a plan

**Author:** driver-review-fixes builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "The driver was merged after an independent review that judged it sound and named follow-ups. Your task is those follow-ups, and nothing more: 1. **A panic in the loop over a query's rows ends the whole process.** In `rows.go`, `rows.start` runs the loop over `engine.Rows.All` in a task of the host (`host.Go`). A panic there unwinds a goroutine that is not the caller's, so an application that recovers panics (as `net/http` does) cannot catch it, and the lease is not released. [...] Fix: recover in the task, hand the value over through the loop, and raise it again on the goroutine that calls `Next` or `Close` (decide what the connector's own close does with it, so that `db.Close()` still closes everything; document it). [...] 2. **Tests of closing the database while statements run.** [...] 3. **The refusal in the connector's `hold` is untested** [...] Write a test that reaches it deterministically if that is possible without a test-only change to production code beyond what `export_test.go` can give; if it is not, say so in a comment at the check and in your report, and do not bend the code. 4. **Comments that overstate.** In `s4ql_test.go`, near the ping with a cancelled context and the ping with a context of one millisecond [...] 5. **Record in `docs/todo.md`**, under the driver: the first phase of the connector's close [...] waits for a running `Next` and is not under `CloseTimeout` [...] 6. **Optional, your judgment:** `core.Options.UseVFS` is a setter; a plain field would match the other options."
**Interpretation:** Six small units: a fix in `/rows.go` with a test that fails before it, tests of a close that runs beside statements, a test of the refusal in `connector.hold` or a comment that says why there is none, honest comments in the test of `Open`, a point in `/docs/todo.md`, and maybe a field in place of `core.Options.UseVFS`.
**Inferred intent:** The driver never kills the process of an application that recovers panics, and the behaviour of the close under concurrency is pinned by tests that do not depend on timing.

### What I did

Reset the worktree to `origin/main` at `66f8463` and created the branch `driver-review-fixes`. Read `/AGENTS.md`, `/s4ql.go`, `/conn.go`, `/rows.go`, `/errors.go`, `/export_test.go`, the tests of the root package, steps 4 and 5 of `/docs/diary/2026-11-19-driver.md`, the driver's section of `/docs/todo.md`, the reviewer's scratch tests, `engine.Rows.All` in `/internal/engine/engine.go`, and `/internal/host/host.go`.

What the engine does with a panic inside its loop: `Rows.All` has a deferred function that sees that the panic came neither from the body of the loop nor after the loop returned, logs the statement's event with `errPanicked` (an `ErrInternal`, outcome `internal_error`), and ends the read transaction. It does not recover: the panic goes on into the caller of `All`, which in the driver is the task of the host. So the read transaction has ended when the panic reaches the task, and what escapes is the panic itself, on a goroutine that no application can recover on.

The plan:

1. The task recovers, records the value in the `loop`, and hands over as at the end of the loop. `rows.Next` sees it, closes the rows (the engine's rows are done, so the close only releases them from the connector), and panics with the value on its own goroutine. `rows.Close` from the application raises it too, if `Next` has not. The close by the connector raises nothing, so that `db.Close()` closes the rest and the instance.
2. The reviewer's tests, made free of timing, in `/rows_test.go` and `/s4ql_test.go`.
3. See whether anything that `export_test.go` can give reaches the window between `closed = true` and the core's stop.
4. Steps 4 to 6 as the brief says.

### Why

The brief names the units; the order puts the fault first.

### What worked

The reviewer's scratch file was still in the scratchpad.

### What didn't work

Nothing tried yet.

### What I learned

The engine already ends the transaction and logs on a panic in its own loop, so the driver's fix is only about where the panic is raised.

### What was tricky

Nothing yet.

### What warrants review

The plan for the connector's close: it swallows the panic of rows that it closes.

### Future work

The steps of the plan.

## Step 2: A panic in the loop over the rows is raised on the goroutine of the reader

**Author:** driver-review-fixes builder

### Prompt Context

**Verbatim prompt:** (from the lead's brief) "Fix: recover in the task, hand the value over through the loop, and raise it again on the goroutine that calls `Next` or `Close` (decide what the connector's own close does with it, so that `db.Close()` still closes everything; document it). The read transaction must have ended, and the rows must be closed and released, when the panic reaches the caller. [...] write a test in that way that fails before the fix and passes after, and that asserts that a `recover` around the caller's `rows.Next()` catches it, that the reader is free afterwards, and that the database still serves and closes cleanly."
**Interpretation:** Recover in the task of `rows.start`, put the value into the `loop`, and let whoever waits for that turn raise it.
**Inferred intent:** A fault in s4ql can be recovered by an application, as a panic on its own goroutine can, and does not take the lease and the cache down with the process.

### What I did

- Wrote the test first, in `/rows_test.go`: a host `panicky`, which is the world's `sim.Free` with a `Yield` that panics at `engine: between two rows of a query` while it is armed, as the reviewer's scratch test had it. Before the fix, `go test -run 'TestRows/should_raise' .` ended the process with `panic: a panic of the test`, from `maragu.dev/s4ql.(*rows).start.func1` down through `engine.(*Rows).All.1`.
- `/rows.go`: the task has a deferred function that recovers, logs the event `panic` at Error with the value and `debug.Stack()`, stores the value in `loop.panic`, and then does what the end of the loop did before: sets `ended` and gives the turn back. `rows.Next` takes the value after its wait, marks the rows as `panicked`, closes them (which releases them from the connector), and panics with it. A later `Next` gives `s4ql: internal error: the query panicked`. `rows.Close` raises a value that its own turn met, after the close. `rows.shut`, the close by the connector, drops it, so that `db.Close()` closes the rest and the instance, and no later call raises it. The documentation of `rows`, `connector`, and `connector.Close` says so.
- Two more tests reach the other two turns through the logger of the options: a `slog.Handler` that panics at the event `statement` while armed. The engine logs that event when its rows end, which after an early stop is in the turn that the close gives the task. One test recovers the panic around `rows.Close()` and checks the event `panic` with its stack; the other closes the database with the rows open and checks that the close succeeds, logs one `panic` and an Info `close`, and that the rows raise nothing afterwards.
- Planted five faults in `/rows.go` with a script, one at a time: `Next` keeps the value (caught), `shut` keeps it (caught, the next `Close` panicked), `Close` drops it (caught), no internal error after the panic (caught), and `Next` raises before it closes the rows (not caught; see below).

### Why

The panic now arrives where an application can recover it, and the lease and the cache are released by the close as for any other rows.

### What worked

The engine already does the work that matters for the transaction: its `Rows.All` ends the read transaction and logs `internal_error` when it panics, so `readersFree` passes right after the recover.

### What didn't work

- The first draft of the helper `closeRows` returned only `any` and was used as an error: `rows_test.go:283:18: cannot use closeRows(rows) (value of interface type any) as error value in argument to is.NotError`. It returns the value of the panic and the error now.
- `golangci-lint run .` then said `rows_test.go:276:6: ineffectual assignment to err (ineffassign)`; the error of a close that panicked is dropped with `_`.

### What I learned

- A panic in `driver.Rows.Close` leaves `database/sql`'s connection unreleased, because `sql.Rows.close` calls `releaseConn` after the driver's `Close`. The connection of this driver holds nothing, so that costs nothing here.
- The panic in `Next` reaches the application through `sql.Rows.Next`, whose locks are released by deferred calls, so the `sql.Rows` can be used and closed afterwards.

### What was tricky

Which call raises a panic of the turn that the connector's close gave: `shut` must drop it, or the application's next `rows.Close()`, often in a `defer` after `db.Close()`, panics during shutdown.

### What warrants review

- That `Next` closes the rows before it raises is not shown by a test: the only difference is that the rows are released from the connector's set at once instead of at the application's `Close`, which `database/sql` makes when the context ends anyway. Nothing outside the package can see the set.
- The new event `panic` at Error, which keeps the stack of the original panic, since the panic that is raised again carries the stack of `Next` or `Close`.

### Future work

None.
