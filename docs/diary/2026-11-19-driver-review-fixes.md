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
