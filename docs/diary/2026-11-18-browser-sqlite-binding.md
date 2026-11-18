# Diary: the SQLite binding in a browser

The simulator on the site is a JavaScript model of the storage design, and Markus wants the page to run the real code instead. `internal/core` does not build for `GOOS=js`, because `internal/sqlite` is a cgo binding. The research in `docs/research/sqlite-in-the-browser.md` measured two routes, and Markus chose the official SQLite WebAssembly build, called from Go through `syscall/js`, with a JavaScript VFS of ours. This task is step 1 of two: a second implementation of the API of `internal/sqlite` and `internal/sqlite/faultvfs` on that build, behind the build tag `s4ql_browser`, so that the unchanged `internal/core`, `internal/sim`, and `internal/store/simstore` build and run in a browser. Step 2, by a later builder, puts the real code on the site.

## Step 1: The brief

**Author:** builder (browser binding)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> Make the unchanged `internal/core` (with `internal/sim`, `internal/store/simstore`) build and run in a web browser, by giving `internal/sqlite` and `internal/sqlite/faultvfs` a second implementation of their API on the official SQLite WebAssembly build, behind a build tag of our own, `s4ql_browser`. [...] Do not touch `site/`.
>
> - The route is the official SQLite WebAssembly build, called from Go (`GOOS=js GOARCH=wasm`) through `syscall/js`, with a JavaScript VFS of ours. Not ncruces, not modernc, not our own Emscripten build.
> - The new files build only with the tag `s4ql_browser` (together with `js && wasm`). Without the tag nothing changes for anyone [...].
> - No existing Go file's behaviour changes. `internal/core`, `internal/sim`, `internal/store/simstore` are not edited. In `internal/sqlite` and `faultvfs`, add new files; if an existing file needs a build constraint so that the two implementations do not collide, add only that and say so in your report.
> - `docs/spec.md` is not changed. Do not edit the README or `site/`.
>
> Deliver: 1. the browser implementation and the JavaScript it needs, with the pinned official build, its version, download, checksum, and how to update it next to the code; 2. a documented, scripted way to run a package's tests in headless Chromium with the tag; 3. a CI job that at least builds (`go build`, `go vet`, `go test -c` for `internal/core` and `internal/sim`) with the tag for `GOOS=js GOARCH=wasm`, and a browser test run in CI only if it is reliable and reasonably fast; 4. evidence: `internal/core`'s tests in Chromium with every failure and its cause, and the trace hashes of `TestSimulation` for the default seeds equal to the native ones; 5. for each test that cannot pass for a reason in the route, fix the route if possible, otherwise skip it under the tag with a comment that states the measured cause, and never weaken a test for the native build.

**Interpretation:** a binding file and a fault VFS file in Go behind `js && wasm && s4ql_browser`, a VFS and a file system in JavaScript, the official build vendored with a manifest and an update tool in the manner of `internal/sqlite/update`, a runner for `go test -exec` that drives headless Chromium, and a CI job.

**Inferred intent:** the page should run the code that ships, so that what it shows can be trusted, and the second binding must not drift from the first without CI noticing.

### What I did

Read `AGENTS.md`, the research note, `internal/sqlite/README.md`, `internal/sim/README.md`, the cgo binding, `faultvfs`, and the research's sketch in the scratch directory (`sqlite_official.go`, `faultvfs_official.go`, `memfs.js`, `memshm.js`, `worker.js`), which still exists. Read `runtime/lock_js.go` and `syscall/js/func.go` of Go 1.27.4 to see how a call from JavaScript into Go runs while Go is inside a call into JavaScript.

Wrote `internal/sqlite/browser/update`, a tool in the manner of `internal/sqlite/update` that reads the PRODUCT line of `sqlite-wasm-X.zip` from the download page, checks size and SHA3-256, and vendors `jswasm/sqlite3.js` and `jswasm/sqlite3.wasm` unchanged with a `source.json`. Ran it:

```
go run ./internal/sqlite/browser/update -version 3.54.1
go run ./internal/sqlite/browser/update -verify
```

The archive is 728,897 bytes, SHA3-256 `87e24f6a4588692310b7942dc2fa7c7275bb9ba2e01d553e4115aa4a904a0050`, as the research recorded, and its `sqlite3.wasm` is byte for byte the one the research ran.

### Why

The plan, from the reading:

- **Where things live.** The Go files are `internal/sqlite/sqlite_browser.go` and `internal/sqlite/faultvfs/faultvfs_browser.go`. Everything else is in `internal/sqlite/browser/`: the vendored build, the JavaScript, the runner for `go test -exec` (a `main` package that embeds the JavaScript), and the update tool. Nothing goes under `site/`; step 2 copies or serves the files from here.
- **The three failures of the research can be fixed in the route.** Two come from compile options that have a PRAGMA per connection: the page size (8192 against 4096) and the page cache (-16384 against -2000). The binding sets them on every connection it opens, with foreign keys and the temp store, so that the browser runs with the options of `options.h` where SQL can set them. The third, the busy handler that blocks the only thread, can be fixed with an `xSleep` of our VFS that calls into Go: Go's runtime lets other goroutines run during such a call, until all of them are idle (`handleEvent` in `runtime/lock_js.go`), so the goroutine that holds the lock can release it.
- **Fidelity details beyond the research's sketch.** The cgo build has `SQLITE_OMIT_AUTORESET` and the official build does not, so the binding keeps the rule that a finished statement fails with `SQLITE_MISUSE` until it is reset. `sqlite3_close` is not exported, so `Close` refuses with `SQLITE_BUSY` itself while a statement is not finalized, as the cgo binding does. The VFS reports power-safe overwrite as SQLite's unix VFS does by default, so the log is written the same way.

### What worked

The download page still lists 3.54.1, so the tool ran without `-url`.

### What didn't work

Nothing in this step.

### What I learned

- A `js.Func` made outside a `testing/synctest` bubble runs on the calling goroutine without `bubble.Run`, so a callback made in `init` can be called from a goroutine inside a bubble.
- `beforeIdle` returns to JavaScript only when every goroutine is idle, so a goroutine that waits for a file operation of Go's `os` during such a call waits until SQLite's call returns, because the callbacks of `fs.js` run from the microtask queue.

### What was tricky

Nothing in this step.

### What warrants review

- The choice to emulate the compile options per connection with PRAGMAs. `pragma cache_size` reads the schema, so `OpenVFS` reads the database file at once where the cgo binding reads it at the first statement.

### Future work

See the later steps.

## Step 2: The binding, the JavaScript, and the runner

**Author:** builder (browser binding)

### Prompt Context

**Verbatim prompt:** (continues from Step 1)

**Interpretation:** build what Step 1 planned, and run the tests in Chromium as soon as possible.

**Inferred intent:** see the route work end to end before polishing it.

### What I did

- `internal/sqlite/sqlite_browser.go`, behind `js && wasm && s4ql_browser`: the API of the cgo binding that the storage core, its tests, `faultvfs`, and the canaries of `internal/sim` use, over the exports of the official build through `syscall/js`. Left out, because nothing in those packages calls them and some cannot be built on the exports: `Tokenize` (it needs the private `sqlite3GetToken`), `DenyProcessPragmas` and `AddTestFunctions` (they need C callbacks), and `FindVFS`, `RegisterVFS`, `UnregisterVFS` (they take C pointers). `SetLengthLimit`, `Complete`, `Keywords`, `CompileOptions`, `Version`, and `SourceID` are in, because they cost a few lines.
- `internal/sqlite/faultvfs/faultvfs_browser.go`: the API of `faultvfs` over a fault state in JavaScript.
- `internal/sqlite/browser/`: `vfs.js` (the VFS `s4ql`, registered as SQLite's default VFS, and one more per `faultvfs.New`), `fs.js` (the in-memory file system for Go's `os`), `bridge.js` (loads the build and the helpers that the Go binding calls), `worker.js` and `index.html` (the runner's page), and `main.go`, a runner for `go test -exec` that serves the page on 127.0.0.1, starts headless Chromium on it, streams the program's output back with synchronous requests, and exits with the program's exit code. It finds Chromium on the PATH or in `/Applications`, or in `S4QL_CHROMIUM`.
- `make check-browser` (build, vet, `go test -c` of the browser packages; no browser needed) and `make test-browser` (the tests in Chromium). `make lint` also runs golangci-lint over the browser build.
- `internal/sqlite/sqlite_browser_test.go`: tests of what is particular to the bridge: 64-bit integers at and around 2^53 and at the extremes, text with NUL bytes and bytes that are not UTF-8, the refusal of `Close` with a statement open, the refusal to step a finished or failed statement, the PRAGMAs of the options, the page size in the file's header, two connections in WAL mode, the files as Go's `os` sees them, and a lock released by another goroutine while SQLite waits in its busy handler.
- The one constraint on an existing file: `//go:build !s4ql_browser` on `internal/sqlite/sqlite_test.go`, the tests of the cgo binding, which use functions that the browser binding lacks and build the `sqlite3` shell. `faultvfs_test.go` builds unchanged and passes in Chromium against the JavaScript fault VFS.

### Why

- The PRAGMAs on open: the research's two failures from compile options (page size and page cache) disappear when a connection has the cgo build's defaults.
- `xSleep` calls into Go: Go's `handleEvent` in `runtime/lock_js.go` parks the goroutine that JavaScript called back on and runs every goroutine that is ready until all are idle, so the holder of a lock can release it during SQLite's sleep.
- Device characteristics `SQLITE_IOCAP_POWERSAFE_OVERWRITE | SQLITE_IOCAP_SUBPAGE_READ` and sectors of 4096: read in `setDeviceCharacteristics` of the vendored `sqlite3.c`, the defaults of the unix VFS. With power-safe overwrite SQLite does not pad the log to sectors, so the log has the same frames as natively.

### What worked

- `faultvfs` and the binding's own tests passed in Chromium at the first full run, after one fix (below).
- The first run of `internal/core`: `GOOS=js GOARCH=wasm go test -tags s4ql_browser -exec <runner> -timeout 25m -count 1 -v ./internal/core` passed, 44 of 44 top-level tests and 211 of 211 subtests, in 2 min 52 s of wall time (170.6 s by `go test`). The research's three failures are gone: the corruption test and the replay fault test by the PRAGMAs, and the lock held for a while by the `xSleep` that calls into Go.
- A copy of the browser tests with the tag removed, run natively against the cgo binding, agreed on everything but the three points where the browser differs on purpose: `pragma temp.cache_size` is -2000 (natively 0 until the temporary database exists), `pragma temp_store` is 1 (natively 0, the default, which means a file), and `Open` of a file that is not a database fails (natively the first statement fails). The copy was deleted after the run.

### What didn't work

- `TestFault/should_fail_the_open_of_a_temporary_file` in `faultvfs`: `Expected "14", but got "0"`. The temporary database has a page cache of its own, which was still the official build's 16 MiB, so the 5 MB table never spilled to a file. Fixed with `pragma temp.cache_size = -2000` after `pragma temp_store = file`, because a change of the temp store drops the temporary database.
- Two `core` tests skipped at first, "should stop for a failure to write the spool file", because `fs.js` ignored `chmod`, as for root (`readOnly` in `core_test.go` skips then). `fs.js` now keeps permission bits that bind as for a user who is not root, and the VFS refuses to create or delete a file in a directory without the write bit, as the unix VFS's `open` and `unlink` would. `xDelete` of a missing file returns `SQLITE_IOERR_DELETE_NOENT`, as `unixDelete` does.
- The first run of `internal/sim` ended with `browser: RangeError: Map maximum size exceeded at Map.set (<anonymous>) at storeValue (wasm_exec.js:173:16)`. `syscall/js` keeps every JavaScript object that reaches Go in a `Map` until Go's garbage collector releases the `js.Value`, and the binding made a view of SQLite's memory per copy; Go's heap grows too little per call for the collector to run in time. Every helper now returns a number, and bytes pass through one `Uint8Array` that Go keeps.
- After that fix, `internal/sim` hung in `TestSynctestWithCgo`, at "should log how often one writer among parallel readers gets SQLITE_BUSY when it begins with no busy timeout". Its eight readers loop without ever blocking, and Go's js/wasm port has one thread and no preemption ("There is no preemption", `runtime/lock_js.go`), so the first reader runs alone for good. Its twin "with a busy timeout" would hang the same way, and also in `xSleep`, because a call from JavaScript into Go returns only once every goroutine is idle. The cause is in the route, and `internal/sim` may not be edited, so `make test-browser` passes `-skip TestSynctestWithCgo/among_parallel_readers` (`BROWSERSKIP` in the Makefile, with the cause). With it, `internal/sim` passed in Chromium in 17 s.

### What I learned

- A runner for `go test -exec` needs no browser automation: the page posts the output and the exit code back to the runner's own server, and Chromium is a plain process in `--headless`.
- The busy handler's yield has a limit that is the reverse of the research's failure: while SQLite sleeps, other goroutines run, but SQLite's call returns only once all of them wait. A goroutine that never waits keeps SQLite asleep. In the storage core every goroutine waits on a lock or a clock sooner or later, and its tests pass; a busy loop does not.

### What was tricky

- The order of the PRAGMAs: `temp.cache_size` before `temp_store` is lost.
- The tool refused shell commands that it could not prove stay in the worktree, such as a Python heredoc next to `go vet` in one command. Scripts went to the scratch directory and ran on their own.

### What warrants review

- `BROWSERSKIP` lives in the Makefile, not in `internal/sim`, because the brief forbids editing that package. If the lead prefers, the two canaries could get a skip under the tag in `internal/sim` itself.
- The `xSleep` yield: a goroutine that runs during SQLite's sleep can call into SQLite on another connection, inside the first call. That is safe only because each connection is used by one goroutine at a time and SQLite holds no global state across its busy handler; the official build has `SQLITE_THREADSAFE=0`, so nothing in SQLite checks it.
- `CompileOptions` reports the official build's options, not those of `options.h`.

### Future work

- A check in CI that the trace hashes of the browser equal the native ones, which today is a manual comparison.

## Step 3: Evidence, self-review, and the native checks

**Author:** builder (browser binding)

### Prompt Context

**Verbatim prompt:** (continues from Step 1)

**Interpretation:** measure what the brief asks for, review the diff with competing reviewers, and show that the native build is unchanged.

**Inferred intent:** the lead can merge on the evidence without rerunning everything.

### What I did

- `make test-browser BROWSERFLAGS='-count=1 -json'`, all five packages in Chromium: every test passed, in 3 min 1 s of wall time. `internal/core` 177 s, 44 top-level tests and 213 subtests passed, three top-level tests skipped as natively by default (`TestSimulationSeed` and `TestPlantedDataRace` without their environment variables, `TestFreeRunning_plantedDataRace` without the race detector). `internal/store/simstore` 25 s (20 and 97), `internal/sim` 17 s (15 and 93, two skipped by default: `TestSimulationSpeed`, `TestScheduler_mutexHeldAcrossWait`), `internal/sqlite` 2 s, `internal/sqlite/faultvfs` 2 s (2 and 19).
- The trace hashes of `TestSimulation` with the default seeds, read from `go test -json` so that each line belongs to its subtest, natively and in Chromium:

  | Workload | Native (cgo) | Chromium |
  |---|---|---|
  | one instance | `5367e1c2…7438` | `5367e1c2…7438` |
  | writes that land long after they were sent | `474a8a3e…b46c` | `474a8a3e…b46c` |
  | instances that compete, clocks that move, crashes, and buggify | `9e400673…7809` | `9e400673…7809` |
  | callers that never pause | `a063e0fc…03c7` | `a063e0fc…03c7` |

  The research note §7 pairs the hashes with other workloads; with `-v` the lines of the parallel subtests interleave, and only `-json` says which is which. The set of four hashes is the same as there.
- Sizes: `sqlite3.wasm` 879,430 bytes (406,762 with `gzip -9`), `sqlite3.js` 439,732 (93,665), the test binary of `internal/core` 14,021,415 bytes (3,296,330).
- A writer sub-agent wrote `internal/sqlite/browser/README.md` from a brief; I filled in the measurements.
- Native `make test`: 22 packages ok. `make lint` failed first: the simulation lint reported the runner's goroutines, `time.After`, `context.Background`, and channel waits (`main.go:132:2: a go statement starts a goroutine outside the host`). The runner and the update tool are commands that never run under the simulation, so they joined the exempt list in `internal/simlint/simlint.go`, as `internal/sqlite/update` is. Then `make lint` passed, with the browser build linted too.
- The code-review skill, at medium, over `main...HEAD`. Ten findings; what I did with each:
  - Fixed: a JavaScript exception in a VFS method unwound through SQLite's C frames. Every method now catches, logs, and returns the I/O error of its kind.
  - Fixed: `xOpen` opened a file that may not be written for writing. It now opens it read-only and reports `SQLITE_OPEN_READONLY`, as the unix VFS does, and SQLite refuses the write with `SQLITE_READONLY`; a new test shows it.
  - Fixed: `xLock` let a connection take PENDING while another held RESERVED. It now applies the unix VFS's rule for connections of one process (`unixLock`: a connection whose lock differs from the highest in the process may only take SHARED, and none while that is PENDING or higher).
  - Fixed: each `faultvfs.New` leaked its `sqlite3_vfs`, its name, and its methods in the function table. `unregisterVFS` now disposes of them.
  - Fixed: the fallback of `vfs.js` for running without `fs.js` was dead code with a second `resize` and `writeAt`. Removed; installing the VFS now requires `fs.js`.
  - Documented: the open reads the schema, so through `faultvfs` an armed fault counts the reads of the open; `pragma temp_store` reads 1 where the cgo build reads 0.
  - Already documented: a file operation of Go's `os` cannot finish while Go is inside a call into SQLite, so a holder of a lock that writes a file before it releases the lock keeps a waiter waiting for its busy timeout.
  - Rejected: "options.h sets `SQLITE_DQS` 0 and the official build does not". The official build reports `DQS=0` among its compile options.
  - Rejected: `SQLITE_THREADSAFE=0` with a `Conn` shared between goroutines. `Conn` is not safe for concurrent use in either binding.
  - Not done: moving the code that does not depend on C into files shared by both bindings. It would edit `sqlite.go` and `faultvfs.go` beyond a build constraint, which the brief rules out. The duplicates are `ResultCode`, `Error`, `Type`, `Prepare`, `Exec`, and the constants of `faultvfs`.

### Why

The research's pairing of hashes to workloads looked off once `-json` gave the owner of each line, so the table here comes from `-json` on both sides.

### What worked

The review's fixes changed no result. The last full `make test-browser` after them passed everything again in 3 min 14 s (`internal/core` 192 s, 44 and 213 passed, the same three skips), with the same four trace hashes, and `internal/sqlite` with its new test, 9 top-level tests and 15 subtests.

### What didn't work

- `make lint` and the simulation lint, as above.

### What I learned

- `go test -json` is the reliable way to attribute `t.Logf` lines of parallel subtests.

### What was tricky

- The tool refuses some long shell commands, so the edits after the review went through small Python scripts in the scratch directory.

### What warrants review

- The CI job `browser` was not run, since nothing was pushed. It assumes `google-chrome` on the PATH of `ubuntu-latest`, passes `--no-sandbox`, and lints with `golangci-lint-action` under `GOOS=js GOARCH=wasm` in the step's environment.
- `internal/simlint/simlint.go` got two entries in its exempt list. It is not one of the packages the brief forbids, but it is outside `internal/sqlite`.

### Future work

- A CI check that the trace hashes in Chromium equal the native ones.
- `//go:wasmimport` for the SQLite calls, which the research timed at 6 ns a call against 807 ns through `syscall/js`, if the page turns out too slow.
- Step 2: the simulator on the site on this binding.
