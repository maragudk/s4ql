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
