# Diary: A panic in the close of rows, and a test of the hand-over

Pull request #63 made every call of the driver that reaches the engine hold the engine's connection from `conn.enter` to `conn.leave` (`/conn.go`), and `rows.shut` (`/rows.go`) hold it while the close of the connector closes rows. An independent review found the fix sound and left two things: a panic in the close of rows left the connection held, and no test forced the hand-over of the connection to a call that comes while `shut` closes rows. The work is on the branch `close-handover` from `origin/main` at `1cb26dc`.

## Step 1: Free the connection on a panic, and force the hand-over in a test

**Author:** close-handover builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "1. **A panic while rows are closed must not leave the connection held.** In `conn.leave`, the `r.mu.Unlock()` after `r.close()` is not deferred, and neither is the final `freeLocked`; in `rows.shut`, the `freeLocked` after `_ = r.close()` is not deferred either. [...] Make the unlock and the free run on every path, also a panic, and let the panic go on. Write a test that makes the close of rows panic and then shows that the connection can still be closed and `db.Close()` returns [...] 2. **A test that forces the hand-over.** [...] Write a test that forces it without depending on timing [...] The test must fail if the hand-over is removed (try it [...])." Also: an entry in `/docs/todo.md` on the hang of CI's job `browser` on pull request #63.
**Interpretation:** Defer the unlock of the rows and the free of the connection on the two paths that close rows while they hold the connection, prove the `leave` path with a red/green test, and write a test that puts a call into the wait in `enter` and releases it through `freeLocked`, with no timing.
**Inferred intent:** `db.Close()` and the close of a connection never wait for good, also after a panic, and the hand-over is covered by a test that does not rest on luck.

### What I did

Both tests are in `TestClose` in `/conn_test.go`, and both hold a goroutine at the event "statement" of the engine's log. `panickyLog` in `/rows_test.go` got `at(f)`, which calls a function of the test at that event, as `rowHost.at` does at the yield between two rows. `openLogged` now takes the host, and moved to `/conn_test.go`. No hook in `internal/engine` was needed.

"should hand the connection to a statement that comes while the close of the database closes rows of the connection, and answer it as closed": rows of a query on a `sql.Conn` are read to their first row, so a loop of the engine runs. `db.Close()` runs in a goroutine; `shut` takes the connection, and the close of the rows ends the loop, whose task logs the event of the query while `shut` waits for it. The log holds it there. A new host, `turnHost`, wraps the semaphore named "turn of the connection" and calls a function of the test at each `Acquire`; it is armed only after the connections are made, so the call it sees is the wait in `enter`. A query on the same `sql.Conn` starts in a goroutine; the test waits until it reaches that `Acquire`, then releases the log. The query must get "s4ql: the instance has stopped: closed" (`ErrStopped`), which holds in both orders of the query and the close of the instance: the connector is marked closed before `shut`, so a query that runs before the instance closes is refused by `connector.hold`, and one that runs after gets the stop. Then `db.Close()` must return nil, and the rows and the connection close.

"should free the connection when the close of rows that the close of the database left to a statement panics, and raise the panic": rows of a query on a `sql.Conn` that no loop reads, so their close logs the event of the query on the goroutine that closes them. An insert on the same connection is held at its own event, inside `enter`/`leave`, and the hook arms the log's panic for the next event. `db.Close()` then finds the connection busy, leaves the rows to the insert, and returns. The test releases the insert; its `leave` closes the rows, whose event panics, and the panic comes out of `conn.ExecContext`, where the test recovers it. Then `rows.Next`, `rows.Close`, and `conn.Close` must return, and a new instance must see both rows.

The fix: `conn.leave` closes the rows through a new `conn.closeShut`, which closes each with `r.mu` unlocked by a `defer`, and on a panic frees the connection in a `defer`, with the rows after the one that panicked put back on `conn.shut` for the next call (the close of the connection is one). `rows.shut` frees the connection in a `defer`.

### Why

A panic that leaves `busy` set or `rows.mu` locked makes every later call of the connection wait in `enter` for good, also the close of the connection by database/sql, and so `db.Close()` of an application that recovers the panic.

### What worked

The panic test was red before the fix: `go test -short -race -count=1 -timeout 60s -run 'TestClose/should_free_the_connection_when' .` gave "panic: test timed out after 1m0s", with the test's goroutine in `rows.Next` at `/rows.go:125` (`r.mu.Lock()`), left locked by `leave`. Green after it.

The hand-over test without the hand-over: with `c.turn.Release(ctx)` removed from `freeLocked`, `go test -short -race -count=1 -timeout 60s -run 'TestClose/should_hand' .` gave "panic: test timed out after 1m0s", with the query waiting in `conn.enter` at `/conn.go:241`, called from `/conn.go:322` (`QueryContext`). With the release back, it passes.

`go test -short -race -count=30 -run 'TestClose/should_(free_the_connection_when|hand)' .` passed.

### What didn't work

Nothing failed beyond the two planned red runs above.

### What I learned

A panic in a call of `database/sql` leaves its own state sound: `withLock` unlocks the driver connection in a `defer`, and `execDC` releases the connection in a `defer`. So the driver's own locks are what matter.

The engine logs the event of a statement inside the driver's call (between `enter` and `leave`), and the event of a query in the end of its rows, on whichever goroutine ends them. That makes the log a good place to hold a goroutine at a known point.

### What was tricky

The panic in `rows.shut` comes out of `connector.Close`, and so out of `db.Close()`, before the instance is closed; database/sql then counts the database as closed, and a second `db.Close()` does nothing. So a test of the `shut` path cannot end with a closed instance, and the world's check that no `-wal` file is left would fail. The `defer` in `shut` is therefore not covered by a test; the `leave` path is.

### What warrants review

- `conn.closeShut` in `/conn.go`: the put-back of the rows after the one that panicked, and that `freeLocked` runs with `mu` held on that path.
- That the answer of the query in the hand-over test is the same in both orders (see above).

### Self-review

The code-review skill (medium) reported three things. One is fixed: a panic in `rows.close` (in the engine's `Rows.Close`) skipped `conn.held--` and the close that `conn.Close` left to the last rows, so the connection counted the rows as open for good, `IsValid` said false, and a pending `closeLater` never closed the engine's connection. That count and close moved into `conn.release`, which `rows.close` calls in a `defer`. The new test `TestRows` "should count rows whose close panics as closed, so that their connection can go back to the pool" (`/rows_test.go`) closes rows of the driver's own interface with the log's panic armed and checks `IsValid`; it failed before the move ("rows_test.go:382: Not true") and passes after. Left as they are: the panic of `shut` that comes out of `db.Close()` (see Future work), and that the rows after one whose close panicked in `closeShut` wait for the next call of their connection, which the close of the connection is.

`make test`, `make lint`, and `go build -tags s4ql_browser ./...` passed, and `go test -short -race -count=30 -run 'TestRows/should_count_rows|TestClose/should_(free_the_connection_when|hand)' .` passed.

### Future work

- A panic in the close of rows by `connector.Close` (the `shut` path) still leaves the instance unclosed, because the panic goes on out of `db.Close()`. `connector.Close` already logs and drops a panic of a loop that it ends; it could do the same for a panic of the engine's `Rows.Close` there, so that the instance still closes. Not done, because the brief said to let the panic go on.

## Step 2: `db.Close()` recovers a panic in its own close of rows

**Author:** close-handover builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's answer) "yes, `connector.Close` recovers it, in the same way it already treats a panic from a loop that it ends. [...] recover the panic of the close of rows on the `shut` path, log it as the event `panic` at Error with the stack as the existing code does, go on with the other open rows and with the close of the instance (so the lease is released and the cache is closed), and return from `db.Close()` an error `s4ql: internal error: ...` in the wording of the existing error for a panic of a query, with the value. If the close of the instance fails too, join or prefer the errors in the way `connector.Close` already does for several errors. The panic on the `leave` path stays as you built it [...] Write the test that was not possible before [...] Record the decision in `docs/todo.md` where the driver's decisions on panics are."
**Interpretation:** Wrap each `rows.shut` in `connector.Close` with a recover that logs and remembers the panic, close the instance anyway, and give the panic as the error of the close.
**Inferred intent:** `db.Close()` always closes the instance and releases the lease, whatever the engine does while it closes rows.

### What I did

`connector.shut` in `/conn.go` calls `rows.shut` and recovers a panic: it logs `panic` at Error with the value and `debug.Stack()`, as the task of the loop does, and gives `fmt.Errorf("%w: the query panicked: %v", engine.ErrInternal, v)`. `connector.Close` keeps the first such error, goes on with the other rows and the close of the instance, and gives it through `publicError` after the close event at Info. `connector.Close` had only one error before, so for "several errors" I chose to prefer the error of the close of the instance, which says that the lease may not be released; the panic is in the log then. The doc comments of `connector` and `connector.Close`, and the entry on panics in `/docs/todo.md`, say so.

The test, `TestClose` "should close the instance when the close of rows by the close of the database panics, log the panic, and give it as an internal error" in `/conn_test.go`: two queries whose rows no loop reads, so their close logs the event of the query on the goroutine of `db.Close()`. The log's hook panics once, at the first of them, and clears itself, so the other closes as usual whichever order the map of open rows gives. `db.Close()` must not panic, must give "s4ql: internal error: the query panicked: a panic of the test", log one `panic` event with the stack and `close` at Info; both rows then give `ErrStopped`, a new instance sees the row, and the world finds no `-wal` left.

### Why

The lease and the cache must not depend on the engine's close of one set of rows.

### What worked

Red before the change: "conn_test.go:1387: Not true" and "a panic of the test" (the panic came out of `db.Close()`). Green after it. `go test -short -race -count=30` over the four new tests, `make test`, `make lint`, and `go build -tags s4ql_browser ./...` passed.

### What didn't work

My first version of the test armed the log's panic for every event and read one of the two rows to its first row. Then the close of those rows panicked in the task of their loop too, which the task recovers and logs, so there could be two `panic` events, in an order that the map decides. Both rows are now unread and the hook panics once.

### What I learned

`internal/engine` has no exported counterpart of `ErrInternal` in the root package, so the tests of the root package check an internal error by its message.

### What was tricky

Choosing between joining and preferring the errors when both the panic and the close of the instance fail: the existing code had no case of several errors, so I picked the one that matters more to the caller, and logged the other.

### What warrants review

- `connector.shut` and the choice of error in `connector.Close` in `/conn.go`.

### Future work

None.
