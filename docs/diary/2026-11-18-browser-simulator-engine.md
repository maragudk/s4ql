# Diary: The engine of the simulator in a browser

The simulator on the site, `site/simulator/`, is a JavaScript model of the storage design. Markus wants it replaced by the real code: the unchanged `internal/core` in the browser, on the official SQLite WebAssembly build that step 1 (#31) bound behind the tag `s4ql_browser`. This is step 2a: the engine, a Go program for the browser that a page drives. Step 2b, by a later builder, is the page on the site and its deploy.

## Step 1: The brief

**Author:** builder (simulator engine)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> You build step 2a: **the engine**, a Go program for the browser that a page can drive. A later builder (2b, not you) builds the page on the site and its deploy. Do not touch `site/` or the Pages workflow.
>
> - The page shows how the **current code** reacts to **faults in the environment**: S3 losing, delaying, hanging, or failing requests, writes that land late, instances that crash and restart, clocks that move, file-system faults through `faultvfs`, and whatever else `simstore`, `sim`, and `faultvfs` already offer. **No planted bugs**: do not expose `core`'s `bugs`, and add no file to `internal/core`.
> - A run is deterministic and shareable: a **seed** plus the **faults and actions the visitor chose, each with the fake time it happens at**, give the same trace every time.
> - No change to `internal/core`, `internal/sim`, `internal/store/simstore`, `docs/spec.md`, or the README. Everything new builds only with `s4ql_browser`.
> - The simulation runs inside `testing/synctest`, which needs a `*testing.T`; a program gets one by starting through the `testing` package.
>
> Design I recommend: run-to-completion, not a live loop. [...] "Injecting a fault while it runs" is then: the page pauses at fake time T, adds the fault to the schedule at T, and asks for the run again.
>
> Deliver: 1. the engine package and the JavaScript glue for a Worker; 2. a documented request and trace format (JSON through `encoding/json/v2`) in a README for the 2b builder, with what the old page offers that the real code cannot, and the reverse; 3. whether invariants can be checked in the page; 4. tests in Chromium through `make test-browser`: the same request gives a byte-identical trace twice, schedules that agree up to T give traces that agree up to T, each fault kind has its effect, a malformed request fails clearly without killing the engine, nothing depends on timing; 5. a minimal demo page; 6. numbers: the size of the `.wasm`, the time to engine ready, the time of a typical run.

**Interpretation:** a library package that runs one request in one `synctest` bubble and returns structured events, a test binary that serves requests from a Worker, the JavaScript around it, a README of the format, and tests that run natively and in Chromium.

**Inferred intent:** the page shows what the shipped code does, so what it shows can be trusted, and every run a visitor shares replays exactly.

### What I did

Read `AGENTS.md`, `internal/sqlite/browser/README.md`, its `worker.js` and `main.go`, the diary of step 1, `internal/sim/README.md` and the API of `sim`, `simstore`, `store`, `format`, and `core`, and how `internal/core/simulation_test.go` and `workloads_test.go` drive the core under the scheduler. A sub-agent read `site/simulator/index.html` and `model.js` and listed what the page lets visitors do and what it shows.

### Why

The plan, from the reading:

- **Where things live.** `internal/simulator` is the engine as a library, so that its code is linted by `simlint` and its tests run natively as well as in Chromium. `internal/simulator/engine` is the program: a package with one test, `TestEngine`, which serves requests from the page, so the program is its test binary (`go test -c`), and it gets its `*testing.T` from the `testing` package without `testing.MainStart`, whose `testDeps` is not meant to be implemented outside the standard library.
- **Structured events without changing `simstore`.** The store traces through the host it is given. The engine gives it the scheduler wrapped in a type whose `Trace` also turns each line into an event, by its format string, which is fixed at each call site. The client of each process wraps the client of `simstore` and puts a record of the call in the context; the store's carriers keep the values of the caller's context, so every line of a request finds its call.
- **The instance's lease ID and buggify** come from the host that the core is given: the scheduler's instance, wrapped to note the name of the task "lease of ID" and every point that fires.
- **The events of the core** come from `Options.Logger`, with a handler that keeps the attributes as values.

### What worked

Nothing to run yet.

### What didn't work

Nothing yet.

### What I learned

- `faultvfs` is out of reach: a connection uses a fault VFS only when it is opened through its name, and the only way to give `core.Open` a VFS is `Options.UseVFS`, which is in `export_test.go`. `Open` refuses a path that starts with `file:`, so a URI with `?vfs=` is no way around it either.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

See the later steps.

## Step 2: The engine, the program, and the tests

**Author:** builder (simulator engine)

### Prompt Context

**Verbatim prompt:** (continues from Step 1)

**Interpretation:** build what Step 1 planned, and get the whole path into Chromium early, since the Worker is the part that native tests cannot reach.

**Inferred intent:** a page that a later builder can write from the README and the module alone.

### What I did

- `internal/simulator`: `request.go` (the request, its defaults, and its checks), `trace.go` (the events, and the reading of the store's lines by their format strings), `run.go` (one run: the instances and their processes, the callers, the actions of the schedule, the client that wraps the store's, and the end), `check.go` (six invariants over what the calls saw and the store holds), `state.go` (the state at a time, folded from the events), and `simulator.go` (`Engine.Handle` for the messages "run" and "state").
- `internal/simulator/engine`: `doc.go`, `engine_test.go` with `TestEngine`, which serves the page, `worker.js`, `simulator.js` (the module for a page), and `index.html` (the demo).
- `internal/simulator/serve`: a command that builds the engine with `go test -c`, puts every file a page needs into one directory, and serves it. `make simulator` and `make simulator-demo` run it.
- Tests: `request_test.go`, `simulator_test.go` (the same bytes twice and on a second engine, ten seeds with every fault twice each, the same events before two schedules differ and before an earlier end, malformed requests, unknown kinds, the state message), `run_test.go` (each action and each fault of the store with its effect, and that every line of the store is classified), `state_test.go` (the position of each cache by the core's events against what the reads saw, the log, requests and calls in flight), and `check_internal_test.go` (each invariant on input made for it).
- `Makefile`: `BROWSERPKGS` has the two packages, `SIMULATORPKGS` the three; `make test` and `make lint` run the simulator natively with the tag, and the simulation's lint over it with `GOFLAGS=-tags=s4ql_browser`. CI's job `browser` does the same, and lints the two packages for the browser.

Commands that showed the path working:

```
go test -tags s4ql_browser ./internal/simulator/...
GOOS=js GOARCH=wasm go test -tags s4ql_browser -exec <runner> -count=1 ./internal/simulator/ ./internal/simulator/engine
go run -tags s4ql_browser ./internal/simulator/serve -out <dir> -addr 127.0.0.1:18431
playwright-cli open http://127.0.0.1:18431/
```

### Why

- **Run to completion**, as the brief recommends: a goroutine of the bubble that waits for the visitor is durably blocked, so fake time would run on.
- **At the end** the engine stops recording, starts every monotonic clock that stands still, and lets each process close its database. A first run with a stopped monotonic clock ended with `deadlock: no task can run: b/caller-0 at sleep, b/caller-1 at sleep, b/lease of 03e6...ffacf7 at sleep, b/supervisor at sleep`, because the sleeps of an instance follow its monotonic clock.
- **The lifecycle of a database** (open, close, reopen after a stop, the close at the end) holds a semaphore of the process, and a close happens only once for each database, so that a close and an open at the same time never leave two databases on one cache.
- **A caller pauses at least a millisecond** before each call, so that a refused call, which returns without waiting, cannot spin without fake time moving.
- **The position of a cache** follows the core's events: `commit` with the outcome `committed` or `committed_and_stopped` gives `commit.name`, and `catch_up` gives `catch_up.to`, or, after a failure, the commit object that is `catch_up.commits` after `catch_up.from` on the way to the head. Served reads give it too. The test reads it from the core's events alone and compares it with what each read saw.
- **No snapshots during a run.** Reading the cache with a connection of the engine's own, or waking a task to take a snapshot, would change the run. The state at a time is folded from the events after the run.

### What worked

- The first native run of a request ran 2 minutes of three instances in 41 ms, with no violation.
- In Chromium the hashes equal the native ones: `95308c33cd30a785...` for the demo's request and `b82b8ab6e0336832...` for a run with a crash, natively and in the runner.
- The checker found nothing in ten seeds with every fault and buggify, and finds each invariant broken on input made for it.

### What didn't work

- In the demo page the second run never ended. A log in the Worker showed `submit 2 state true` after `done 2`: a call from JavaScript into Go returns only once every goroutine waits, so `TestEngine` handled the message and called `done` inside `submit`, and `pump` then marked the engine busy for good. `pump` now marks it busy before it calls `submit`.
- `sqlite3_memory_used` is not among the exports of the official build, so the Worker reports the size of SQLite's WebAssembly memory instead.
- Four first versions of tests failed for the seed, not the code: an outage that the last request before its end outlives, a renewal that comes at 20 s exactly, a spare that reads the manifest only when it does not hold the lease, and conflicts, which need another write to the manifest in flight and so a write stuck in the network.

### What I learned

- `simlint` ignores its `-tags` flag ("no effect (deprecated)"), and `GOFLAGS=-tags=s4ql_browser` makes it see the files behind the tag; a probe file with `time.Now` showed it.
- The context of a call reaches every line that the store traces for it, also from its carriers and from a write stuck in the network, because `sim` passes the values of the context to a task.

### What was tricky

- The harness refuses shell commands with heredocs that it cannot check, and an `rm -rf` inside a Makefile recipe in a Python heredoc; edits went through scripts in the scratch directory.

### What warrants review

- `storeLine` in `trace.go` reads the lines of `simstore` by their format strings. A change of a format there turns its line into an event without `what`, and the test "should classify every line of the store under every fault" fails.
- The engine's own faults in the client: `loseAnswer`, `failRequest`, and `damaged`. They are network faults that the store's contract allows, made outside `simstore`, as the tests of the core do with `damagingStore`.

### Future work

See the later steps.
