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
