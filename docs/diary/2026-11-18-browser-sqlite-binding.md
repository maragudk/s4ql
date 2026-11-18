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

Nothing yet.

### What I learned

- A `js.Func` made outside a `testing/synctest` bubble runs on the calling goroutine without `bubble.Run`, so a callback made in `init` can be called from a goroutine inside a bubble.
- `beforeIdle` returns to JavaScript only when every goroutine is idle, so a goroutine that waits for a file operation of Go's `os` during such a call waits until SQLite's call returns, because the callbacks of `fs.js` run from the microtask queue.

### What was tricky

Nothing yet.

### What warrants review

- The choice to emulate the compile options per connection with PRAGMAs. `pragma cache_size` reads the schema, so `OpenVFS` reads the database file at once where the cgo binding reads it at the first statement.

### Future work

None yet.
