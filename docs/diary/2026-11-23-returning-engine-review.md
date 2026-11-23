# Diary: the review of the engine step of slice 14

An independent review of the engine and the driver for slice 14, `returning` on `insert`, `update`, and `delete`, found three problems. The lead decided what to do about each one and gave the fixes to a builder, who worked on top of the branch `worktree-agent-a9579e82a7d8bcd14`.

## Step 1: fix the three findings of the review

**Author:** builder (sub-agent of the lead)

### Prompt Context

**Verbatim prompt:** The prompt is the lead's brief. There is no new message from Markus. The part that names the findings:

> 1. **A committed statement gets the "stopped" error when the connector closes during `Query`.** [...] **Fix:** the rows of a `returning` outside a transaction hold no read transaction, so they are never refused after the commit: once the commit is acknowledged, `Query` returns the rows without error, also when the connector is closed or closing. Write the test (deterministic, no timing: the close is started from inside the fake store's swap as the reviewer did, or by another forced order). Look for the same pattern on every path between the commit's acknowledgement and the return of the rows, in the engine and in the driver, not only at `hold`.
>
> 2. **A context that ends after the store applied the commit: `Exec` reports success, `QueryRow` reports "context canceled".** [...] **Fix:** the held rows of a `returning` do not check the context: they hold nothing and the work is done. This holds for the rows inside a transaction too, unless you find a reason there; say what you did. Change the test that pinned the old behaviour. [...] So also **document it** in the package doc of `s4ql.go`, in the section on errors [...]. Add a driver test that shows the two halves: `QueryContext` returning nil implies the change is there, also with a context that ends right after the swap is applied.
>
> 3. **`Exec` of a statement with `returning` holds every row in memory and a `delete` does one extra `get` per row, for rows that are dropped.** **Fix:** `Exec` does not collect the rows. Every error must stay the same as through `Query`, in particular `no such column` for a `returning` column and the order of the errors, and `RowsAffected` must not change. The telemetry of an `Exec` then counts fewer rows read for a `delete`; that is right, record it in the notes.

**Interpretation:** make three small fixes, each with a test that fails before it and passes after it, plus a paragraph in the package doc, notes in `/docs/todo/slice-14.md`, and an updated open item on memory.

**Inferred intent:** the package doc promises that every error except `ErrOutcomeUnknown` means the statement is not committed. A committed RETURNING must never report an error that breaks that promise. `Exec` must not pay for rows that it drops.

### What I did

I wrote the failing tests first. In `/conn_test.go`, `TestReturning` got two subtests. The store wrapper `faulty` in `/s4ql_test.go` got `afterSwap`, which runs a function once the next swap is applied and then gives the store's answer. In the first subtest, that function starts `db.Close()` in a goroutine and spins on `runtime.Gosched()` until `Connector.Closing()` reports that the connector's close has begun. `Connector.Closing()` and `OpenStoreConnector` are new in `/export_test.go`. Before the fix the subtest failed with `Expected nil error, but got "s4ql: the instance has stopped: closed"`. In the second subtest, `afterSwap` cancels the context of `QueryContext` and of `QueryRowContext`. In `/internal/engine/returning_test.go` I changed the subtest that pinned the old context behaviour so that it now requires every row, both outside a transaction and in one. A new subtest compares the errors of `Exec` and `Query`, and checks `statement.rows_read` of an `Exec` of a delete. Both engine subtests were red: `Expected nil error, but got "context canceled"` and `Expected "2", but got "4"`.

The fixes:
- **Finding 1:** `engine.Rows.Holds()` is new. It is false for the rows of a RETURNING outside a transaction, through a field `free` that `Conn.queryText` sets. `conn.QueryContext` in `/conn.go` calls `connector.hold` only for rows that hold something. I checked the other steps between the commit's acknowledgement and the return: `returnedOf`, `report.end`, the building of the `Rows`, `conn.count`, the check of the columns, `conn.leave`, and what `database/sql` does after the driver returns. None of them can fail.
- **Finding 2:** `returned.rows` in `/internal/engine/returning.go` no longer looks at the context. This holds in a transaction too. A paragraph in the "Errors" section of `/s4ql.go` says that once `QueryContext` of a RETURNING has returned nil, the statement is committed, and that a later error from `Rows.Err` or `Row.Scan` says nothing about the commit.
- **Finding 3:** `Conn.write` and `Conn.writeIn` take a new parameter `returns`, and `Exec` passes false. The sink is still built with its RETURNING, so the errors and their order do not change. Then `dropReturns` sets `ret` to nil before the run, so no row is held and a delete makes no extra `get`.

I updated the README of the engine and the doc of `attrRowsRead`. I added the heading "Review of the engine step" to `/docs/todo/slice-14.md`, with pointers from the old bullets that the fixes changed. The open item in `/docs/todo/storage-core.md` now says that `Exec` holds no rows.

### Why

Finding 1 and finding 2 broke the package's promise about errors for a statement that was committed. Finding 3 held memory and made reads for rows that nobody takes.

### What worked

The `afterSwap` hook forces the order with no timing. The close begins after the swap is applied and before the instance acknowledges the commit. The core goes on with a commit whose swap may be applied ("goes on finding out", in the doc of `core.DB.Close`), so the commit is acknowledged, and the test then reads the row from a second instance. Each new test passed 50 runs with and without `-race`, and a run with `-cpu 1,4` and `-count=50` of both packages at once also passed. `make test`, `make lint`, and `make test-sql` passed.

### What didn't work

The first version of the context test failed under `-race -count=50`: `conn_test.go:2635: Expected nil error, but got "context canceled"`. `database/sql` closed the rows between `rows.Next()` and `rows.Scan()`, and `Scan` then gives the error of the context. The test now accepts `context.Canceled` from `Scan` too. The behaviour belongs to `database/sql`, and the package doc describes it.

The first expectation of `statement.rows_read` for the `Exec` was wrong: I wrote 3, and the value is 2. The access is by the key with `k > 0`, so it reads only the two rows that it finds.

### What I learned

`database/sql.Rows.Scan` returns the error of the context once its own goroutine has closed the rows, also right after `Next` returned true. So no driver can promise that a context ending after a commit leaves the rows unharmed. The package doc is the only place to say it.

### What was tricky

I had to observe the start of the connector's close from inside the store's swap. `database/sql.DB.Close` gives no hook, so the test reaches the connector through a new export in `export_test.go`. The swap callback must run without the wrapper's lock, because the close of the core releases the lease through the same store.

### What warrants review

Two places in `/conn.go`: `r.Holds() && !c.c.hold(held)`. Rows that the connector does not hold still count in `conn.held`, and still remove themselves from the connector's map when they close, which does nothing for them. Also check the decision to stop ending the rows of a RETURNING with their context inside a transaction too. I found no reason against it: those rows hold the connection, but nothing that the context bounds. A loop over `Rows.All` that stops early still ends them, and the driver closes them when `database/sql` closes its rows.

### Future work

The rows of a RETURNING through `Query` are still held in memory without a bound, which is the open item in `/docs/todo/storage-core.md`.

## Step 2: self-review

**Author:** builder (sub-agent of the lead)

### Prompt Context

**Verbatim prompt:** the same brief, which asks for a self-review before the hand-back.
**Interpretation:** run the code-review skill over `696b0a4..HEAD`, then decide what to do about each finding.
**Inferred intent:** hand back a branch that the lead can land without another round.

### What I did

The review found four nits and no blocking bug. I fixed three of them:
- The doc of `connector.Close` claimed that the close closes every open rows. It now names the rows of a RETURNING outside a transaction, which the close leaves open.
- `faulty.Swap` returned early after `afterSwap`, which would skip a planned `loseNext` on the same swap. It now goes on to the loss.
- `Exec` of an update or a delete with RETURNING still logged `statement.direction` for rows that it never gives. The engine still makes the draw, so that the sequence of draws does not depend on Exec or Query, but it no longer records it, in `write` and in `writeIn`. The test of `Exec` pins that the attribute is absent. The doc of `attrDirection` and the README of the engine say so.

The fourth nit said that the rows of a RETURNING in a transaction no longer end with their context, and could hold the connection. I kept that behaviour and answered the nit in the notes: `Rows.All` ends the rows when a loop stops early, and the driver closes them when `database/sql` closes its rows.

### Why

Each fixed nit made a comment or an event say something false, or let the test helper skip a planned loss without anyone noticing.

### What worked

After the fixes, `make test`, `make lint`, and `make test-sql` passed, and the new tests passed 50 runs again, also under `-race` and under load.

### What didn't work

Nothing failed in this step.

### What I learned

The direction draw has two jobs. It keeps the sequence of draws for a seed, which must not depend on Exec or Query, and it explains the order of the rows in the event. Those two jobs can be separated.

### What was tricky

Nothing was tricky in this step.

### What warrants review

The changes to `r.drew(order)` in `Conn.write` (`/internal/engine/engine.go`) and in `Conn.writeIn` (`/internal/engine/tx.go`).

### Future work

None beyond the open item on memory.
