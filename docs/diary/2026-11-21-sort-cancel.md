# Diary: Interrupting a sort of the cache when the context of the query ends

Slice 13 (`order by`, `limit`) made the cache's SQLite sort the rows of an access where no index delivers the order. The first step of such a scan sorts every row inside one `sqlite3_step`, and the count of unread rows after an early stop is one step too, and the context was only checked between rows. A review measured 211 ms to return `context deadline exceeded` for a deadline of 5 ms on 300,000 rows. This task makes a step that reads stop when its context ends, after an amount of SQLite's work that does not grow with the table. The work is on the branch `sort-cancel`, from `452b6cb` on `main`.

## Step 1: The mechanism, and `StepContext` in the binding

**Author:** sort-cancel builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "When a query's context ends while SQLite is inside a step of a scan or of the unread count, the step is interrupted and the query returns the context's error, after an amount of SQLite's work that does not grow with the table. The mechanism is your choice; decide by correctness. Things to weigh: `sqlite3_interrupt` from another goroutine (for example `context.AfterFunc`) acts on every statement running on the connection, and a call that arrives late must never hit the next statement on that connection [...] `sqlite3_progress_handler` with a handler written in C that reads a flag the Go side sets (no call into Go per tick) has no such race, and makes the test deterministic in virtual-machine operations instead of time. Check its cost on an ordinary scan and state the measured cost in the pull request; pick the period so it is not measurable. [...] interrupt only steps that read (the scans and the count) [...]"

**Interpretation:** pick between `sqlite3_interrupt` and a progress handler, put it in `internal/sqlite`, keep writes out of reach, and measure the cost.

**Inferred intent:** a query whose deadline passes returns promptly, without any chance that the mechanism hits a statement it was not meant for, and without making the tests depend on timing.

### What I did

Read the binding, the core's scan path (`conn.step`, `conn.unread`, `ReadTx.scan`, `Tx.scan`), SQLite's own check sites in `/internal/sqlite/c/sqlite3.c`, and `internal/simlint`.

Both suggested mechanisms need a goroutine that learns that the context has ended while the step runs in C: `context.AfterFunc` for `sqlite3_interrupt` as much as for a C flag. `internal/simlint` forbids `context.AfterFunc` and channel receives in library code, and `internal/sim/README.md` says why: the simulation runs one goroutine at a time under a seed, and a goroutine of the context package runs outside its scheduler, so the run would depend on the Go scheduler. So I chose a third mechanism: a progress handler written in C that calls one exported Go function, `s4qlInterrupted` in `/internal/sqlite/progress.go`, which looks at `ctx.Err()` of the step. It runs on the goroutine of the step, so there is no goroutine, no channel, and no late arrival: the handler is installed and removed inside the one C call that steps (`s4ql_step_interruptible`), so it exists for that step and no other. Under the simulation, no other goroutine runs during a cgo call, so the context cannot end during a step there, and the replay does not change.

`Stmt.StepContext(ctx)` in `/internal/sqlite/sqlite.go` runs the step with the handler every `progressPeriod` = 10,000 instructions, and returns `ctx.Err()` when SQLite stops the step with SQLITE_INTERRUPT and the context has ended. A context with a nil `Done` steps as `Step` does, with no handler. The connection keeps a `cgo.Handle` to a small `progress` struct, made in `open` and deleted in `Close`, and the statement points at it.

The browser binding (`/internal/sqlite/sqlite_browser.go`) gets `StepContext` as plain `Step`, documented: SQLite runs on the page's one thread there.

For deterministic tests, `/internal/ctxtest` is a new test-only package with `EndAtErr(parent, n)`: a context that ends at the nth call of its `Err`. Since the handler calls `Err` once a period, the test controls in which period the step stops, in instructions and not in time. The binding exposes `VMSteps` (SQLITE_STMTSTATUS_VM_STEP) and `ProgressPeriod` to its tests through `/internal/sqlite/export_test.go`.

### Why

Correctness of the simulation is part of correctness here, and the handler that calls Go is the only one of the three that keeps it, as well as removing the race of a late interrupt by construction.

### What worked

The tests in `TestStmt_StepContext` pass: for 20,000 and 100,000 rows, and a context that ends at the 1st, 2nd, and 5th look, the step stops at between `at*10,000` and `at*10,000+100` instructions, whatever the table's size. An interrupted read inside `begin` keeps the transaction and its writes, which matches `sqlite3VdbeHalt` in the vendored source: a read-only statement stopped with SQLITE_INTERRUPT rolls nothing back.

Measured cost, `go test -run '^$' -bench BenchmarkStmt_StepContext -count 6 ./internal/sqlite/` on a loaded Apple M4: a seek of one row, 1.20 to 1.60 µs with `Step` and 0.77 to 1.22 µs with `StepContext`, within the noise; a sort of 20,000 rows in its first step, 6.0 to 11.4 ms against 7.8 to 11.3 ms, within the noise. With the period set to 1 for a moment, the sort took 14 to 17 ms against 3.4 to 3.9 ms, about 11 ms over roughly 150,000 calls, so about 75 ns a call; at 10,000 the sort makes about 15 calls, about 1 µs, under a tenth of a percent.

### What didn't work

The first run of the tests failed on my own helper: `sqlite_test.go:1267: Expected "0", but got "1" (type int64)`, because the helper that counts the rest of the rows began at v = 0 after the first row had already been read; and `Expected "1", but got "2"`, because `StepContext` calls `Err` once more for its error. Then `10 calls of Err in 200009 instructions` failed a bound of more than 10 looks: the first step of that sort runs about 100,000 of the statement's 200,000 instructions. All three were the tests' expectations, fixed in the tests.

### What I learned

SQLite runs `select count(*) from t` without WHERE as one instruction, `OP_Count`, which counts the whole b-tree and looks only at `sqlite3_interrupt`'s flag, never at the progress handler. `count(k)` of a column has no `SQLITE_FUNC_COUNT` flag (`isSimpleCount` in `sqlite3.c`), so it runs as a loop that the handler stops. The core's count of unread rows must therefore count a column of the primary key. The first look of the handler in a statement that ran before comes after `period - (prior steps % period)` instructions, since SQLite counts the steps since the prepare.

### What was tricky

Seeing that the brief's preferred C flag needs a goroutine to set it, and that the lint forbids exactly that.

### What warrants review

`/internal/sqlite/progress.go` and the preamble of `/internal/sqlite/sqlite.go`: the rule that `sqlite.go` has no `//export` holds, and the exported function's file declares only. `TestSymbols` still passes.

### Future work

The core uses `StepContext`: step 2.

## Step 2: The core, the engine, the driver, and the notes

**Author:** sort-cancel builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "`SQLITE_INTERRUPT` from a step becomes the context's error (`context.Canceled` or `context.DeadlineExceeded`, by `ctx.Err()`), never a local failure that stops the instance. [...] The connection is clean afterwards: the statement is reset or finalized, the length limit that `conn.step` raises is put back, the flag or handler state is cleared, and the next query on the same connection gives right results. Make the comment on `scan` in `internal/engine/operator.go` [...] true as written, or correct it to what now holds. Telemetry: no new span or attribute. [...] Three branches from the review that have no test yet [...] write them and delete that point [...] Through the driver: one test that a `QueryContext` with a context ended during a sort returns the context's error and that the `*sql.DB` works afterwards."

**Interpretation:** wire `StepContext` into the core's first step of a scan and its count, keep write transactions usable, test every branch named, and record the decisions in `/docs/todo/slice-13.md`.

**Inferred intent:** the review's finding is closed with tests that a fresh reader can trust.

### What I did

In `/internal/core/index.go`, `conn.scanIndex` takes the context and runs its first step through `conn.step(ctx, s, sorting)`, which now calls `StepContext` and still raises and puts back the length limit for a sorting scan; later steps use `Step`. `conn.unread` takes the context and runs the count through a new `conn.queryContext` in `/internal/core/cache.go`. `countSQL` counts the first column of the primary key, because SQLite runs a bare `count(*)` as one uninterruptible instruction. `Tx.scan` gives the error of the context from the scan, and drops it from the count, without `tx.fail`, through `core.ended` in `/internal/core/errors.go`. The commit's own scan runs with `context.WithoutCancel(ctx)`, which has no `Done`, so it is never stopped.

Tests: `TestScanIndex_context` in `/internal/core/index_test.go` (interrupted sort at the 1st, 2nd, and 3rd look in a read and a write transaction, with the length limit of 1,000 bytes; an interrupted count; the count skipped when the context ended before the loop stopped; a count that fails on a read fault), `TestConn_step` in `/internal/core/index_internal_test.go` (the limit after a sort step that succeeds, fails on a temp-file write, or is stopped), a subtest of `TestEngine_events` in `/internal/engine/engine_test.go` (the event of an interrupted query is `canceled` with `context canceled`, as for a cancel between rows), and a subtest of `TestRows` in `/rows_test.go` (every call of `Err` up to the first row, the sort's included, ends the query with `context.Canceled`, and the next query gives the row).

Docs: the comment on `scan` in `/internal/engine/operator.go`, the engine's and the core's READMEs, the section "Interrupting a sort" in `/docs/todo/slice-13.md`, the item deleted from `/docs/todo/storage-core.md`, and a new open item there: the stop of an instance does not stop a running sort.

### Why

Each branch that the review named now has a test that fails without the code: I checked by hand that putting back `count(*)`, failing the transaction on the context's error, or skipping the limit's restore on error each fails a test.

### What worked

The probes with `ctxtest.EndAtErr(ctx, math.MaxInt)` made the core and driver tests independent of how often other code calls `Err`: a test first counts the calls up to a point, then ends the context at each of them. `go test -count=50` and `-race` passed for every new test.

### What didn't work

`go vet` refused a `context.WithCancel` in a test without `defer cancel()`: `the cancel function is not used on all paths (possible context leak)`. The first driver test expected the key 4479 for the largest v; it is 2321 (2321 × 7919 = 18,379,999), and the test now states 2321. The first version of that test asserted more than 10 calls of `Err` before the first row and got 9, so it now compares the calls of the sorted query with those of a query by the key: the difference is the sort's looks. In the engine test, `probe.Calls()` after the whole query counted calls after the first row, so a context ending there gave no error; the test records the calls when the first row arrives. `readersFree` reads `t1`, which the driver test had not made: `no such table: t1`. Two commands of mine with heredocs and inline Python were refused by the worktree's guard ("this command is too complex to verify that it stays inside the worktree"); I used the edit tool and scripts in the scratchpad instead.

### What I learned

The core's earlier README said that no test could fail the count because the index's pages stay in the page cache after a sort; rows of 400 bytes, 4 MB in all, are larger than the page cache, and a read fault then reaches the count.

### What was tricky

A failed count stops the instance on both paths, since a read fault is a local failure, so that test needs an instance of its own per path.

### What warrants review

`core.ended`, and that no path returns a context error from `scanIndex` but the interrupted step. The open item on the stop of an instance in `/docs/todo/storage-core.md`.

### Future work

The stop of an instance could stop a running sort too, through a context that the stop cancels. Not decided.

## Step 3: Self-review and the checks

**Author:** sort-cancel builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Run new tests with `go test -count=50` and under `-race`. Because you change `internal/sqlite`, run the sanitizer for the packages you touch only [...]. Run `make test` and `make lint` before you hand in."

**Interpretation:** review the diff with the code-review skill, fix what matters, and run every check.

**Inferred intent:** the branch is ready for the lead's review without surprises.

### What I did

Ran `/code-review medium main...sort-cancel`. It found nothing important, and three smaller points. (1) Pre-existing: a scan whose key has more than 63 unpinned columns sorts in its first step under the connection's small length limit, since `conn.step` raises the limit only for `Order`. Not this task's; I made the comment on `conn.step` say "a scan with Order" and recorded it as a known item in `/docs/todo/storage-core.md`. (2) The count of a column costs more than `OP_Count`: I measured it on 300,000 rows of about 200 bytes, `count(*)` 13 to 16 ms, `count(k)` 19 ms, the sort before it 49 to 53 ms, and recorded the numbers in `/docs/todo/slice-13.md`; the trade is kept, since the 15 ms of `count(*)` could not be stopped. (3) The `progress` struct is shared by the statements of a connection: a comment at the field now says so; a `Conn` was already not safe for concurrent use.

Checks: `make lint` with 0 issues, simlint included; `make test`, exit 0; the sanitizer in Docker with `make asan-test ASANPKGS="maragu.dev/s4ql/internal/sqlite maragu.dev/s4ql/internal/core"`, `ok` for both (core 265 s, sqlite 63 s); every new test with `-count=50` and with `-race`.

### Why

The measured cost of the count was the one claim of the branch without a number.

### What worked

The review agreed with the reasoning on transactions, the handle's lifetime, and the count's equality.

### What didn't work

A one-shot benchmark written with a heredoc was refused by the worktree guard; I wrote it with the edit tool as `internal/sqlite/zz_count_test.go`, ran it, and deleted it.

### What I learned

On a file database `count(*)` of 300,000 rows takes about 15 ms, which is a third of the sort's time: the uninterruptible count was not negligible before this change either.

### What was tricky

Nothing beyond step 2.

### What warrants review

The two open items in `/docs/todo/storage-core.md`.

### Future work

None beyond those items.

## Step 4: The browser build, which does not interrupt a step

**Author:** sort-cancel browser builder

### Prompt Context

**Verbatim prompt:** "#124 lets a running sort be cancelled: `sqlite.Stmt.StepContext(ctx)` installs SQLite's progress handler for one step in the cgo binding. In the browser build (tag `s4ql_browser`, wasm in headless Chrome) `StepContext` is plain `Step`, so nothing is interrupted there. CI job `browser-test (4)` (covers `internal/core`) fails in `TestScanIndex_context` [...] 1. Decide by reading the browser binding in `internal/sqlite` (the files built under wasm) whether interruption can be built there simply and correctly. [...] If not (likely), do not build it [...] 2. In that case the subtests that assert an interruption inside a step move to test files that are not built for the browser [...] 3. While you are in `TestScanIndex_context`: the count subtest ends its context at `looks+2`, which assumes exactly one call of `Err` between the first row and the unread count [...] Make the test fail when the count was not the step that was interrupted [...] 4. In `docs/todo/storage-core.md`, at the item about a sort that an instance's stop or close does not interrupt, add: a review measured the cost of `StepContext` at about 47 ns on the first step of a scan, with no allocation per step." (abridged; the full brief also asks for the checks, this diary step, and landing pull request #124)

**Interpretation:** make CI's browser jobs pass on #124 without weakening what the cgo build tests, decide whether the browser can interrupt, and harden the count subtest so that it cannot pass without an interrupted count.

**Inferred intent:** land #124 with the tests telling the truth about each build.

### What I did

I read `/internal/sqlite/sqlite_browser.go` and the vendored build. `sqlite3.wasm` exports `sqlite3_progress_handler`, and `sqlite3.js` has `wasm.installFunction`, and the binding already calls Go from SQLite through `syscall/js` (`s4qlYield`, from the VFS). So a handler could be built. I did not build it: Go's runtime, on every call from JavaScript into Go, runs every goroutine that is ready before it returns (`handleEvent` parks the callback's goroutine until all others wait). A handler that calls Go every 10,000 instructions would so run the simulation's other goroutines in the middle of a step, and they could call SQLite there, which has `SQLITE_THREADSAFE=0` in that build. That changes the interleavings of the simulation and is not simple. The doc comment of the browser's `StepContext` now says that it does not interrupt and why, and `/docs/todo/storage-core.md` has the open item.

The tests that assert an interruption inside a step moved to two new files with `//go:build cgo`, which is where the cgo binding, whose `StepContext` installs the handler, is built: `/internal/core/index_cgo_test.go` (`TestScanIndex_interrupt`: the interrupted sort and the interrupted count, in a read and a write transaction) and `/internal/core/index_cgo_internal_test.go` (`TestConn_stepContext`). `TestScanIndex_context` in `/internal/core/index_test.go` keeps the context ended between rows and the count that fails on a read of the cache, and `TestConn_step` keeps the limit put back after a step that gives a row and one that fails on a temporary file. Their setup became helpers shared by both files: `fillBig`, `beginBig`, `bigScanner`, `bigSorted`, `bigRows` in `index_test.go`, `scanBig` in the cgo file (lint found it unused in the browser build), and `stepSort` in `index_internal_test.go`. `TestStmt_StepContext` in `/internal/sqlite/sqlite_test.go`, the engine's event subtest, and the driver's `TestRows` subtest needed nothing: `sqlite_test.go` has `!s4ql_browser`, and `internal/engine` and the root package are not in `BROWSERPKGS`.

For the count, `ctxtest.Context` got `EndedBy`, the name of the function whose call of `Err` ended the context (from `runtime.Callers` at that call). The count subtest now ends the context at each look after the first row in turn, asserts for each that nothing was counted, stops at the first look made by `maragu.dev/s4ql/internal/sqlite.s4qlInterrupted`, and fails if no look was. The sort subtest also asserts that its ends at the 1st, 2nd, and 3rd look are the handler's, and `TestConn_stepContext` the same. The 47 ns of the review went into the item on the stop that does not interrupt a sort, and the test names in `/docs/todo/slice-13.md` were brought up to date.

### Why

The browser build is the site's simulator, where a context that ends mid-sort does not matter, and a build constraint keeps the cgo build's tests whole without a skip. `EndedBy` turns "the count was interrupted" from an assumption about how many times the cache calls `Err` into something the test observes.

### What worked

A planted fault showed that the moved tests catch the browser's behaviour natively: with `StepContext` changed to `if true { return s.Step() }` in `/internal/sqlite/sqlite.go`, `go test -count=1 -run 'TestScanIndex_interrupt|TestConn_step' ./internal/core` failed with "SQLite looked at the context 0 times in the sort", "SQLite's progress handler looked at the context in no count", and, at the next look, `Expected "1", but got "10001"` with "ended at the look 3 by maragu.dev/s4ql/internal/core.ended", the same failures CI had in the browser. With the binding back, everything passed: `go test -count=20` and `-race -count=5` of the four tests and of `internal/ctxtest`, `make test`, `make lint`, `make check-browser`, and `make test-browser BROWSERSHARD=4/6` (`internal/core`, 56 s) and `BROWSERSHARD=5/6` (`internal/sqlite` and the rest).

### What didn't work

`make lint` failed first on the browser's build: `internal/core/index_test.go:2472:6: func scanBig is unused (unused)`, because only the cgo file uses it, so it moved there. `go vet` then gave `"maragu.dev/s4ql/internal/ctxtest" imported and not used` in `index_test.go`. A combined shell command that spliced the test file was refused by the worktree guard as too complex to verify; plain separate commands worked.

### What I learned

In the browser build, a callback from JavaScript into Go is a scheduling point for every goroutine, not only the callback's. Any hook inside SQLite that calls Go is therefore a yield.

### What was tricky

The count's first look is after the cache's own calls of `Err` (one in `ReadTx.scan` before the count, and `ended` in the loop of `Tx.scan` before the first row), and its number differs by path. Searching for the handler's first look, instead of computing it, avoids depending on that number.

### What warrants review

The constant `progressHandler` in `/internal/core/index_cgo_test.go` names the unexported `s4qlInterrupted` of `internal/sqlite`; a rename there fails these tests loudly, it cannot make them pass falsely. The constraint `cgo`, not `!s4ql_browser`, is the exact condition under which `StepContext` interrupts, also natively with the tag `s4ql_browser`.

The self-review (`/code-review medium`) found no bug in the change. It gave two nits that I fixed: `/docs/todo/slice-13.md` still named `TestScanIndex_context` for the write transaction that goes on after an interrupted sort, now `TestScanIndex_interrupt`, and the test of `EndedBy` compared the compiler's name of a closure, `TestEndAtErr.func2.1`, which a new subtest would renumber; it now calls a named helper, `lookAt`. Its two other findings are known and recorded in `/docs/todo/storage-core.md`: the scan with more than 63 unpinned key columns sorts under the lowered length limit, and a stop or close does not interrupt a sort.

### Future work

The open item on the browser build in `/docs/todo/storage-core.md`.
