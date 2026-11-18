# SQLite in a browser

`internal/sqlite/browser` holds what a Go program needs to run `internal/sqlite` in a web browser: the official SQLite WebAssembly build, the JavaScript between that build and Go, and a runner that executes such a program in headless Chromium.

The Go side is a second implementation of the binding's API, in `../sqlite_browser.go` and `../faultvfs/faultvfs_browser.go`. It is built with `GOOS=js GOARCH=wasm` and the tag `s4ql_browser`.

It exists so that the real storage core, `internal/core` with `internal/sim` and `internal/store/simstore`, can run as a simulator on the project's site. The tag is for that simulator only. Browser support for users of the library is not promised.

## Layout

- `sqlite3.js` and `sqlite3.wasm` are the official build, unchanged from the directory `jswasm/` of https://sqlite.org/2026/sqlite-wasm-3540100.zip. It is SQLite 3.54.1, the version of the vendored amalgamation in `../c`. The archive has 728,897 bytes and the SHA3-256 `87e24f6a4588692310b7942dc2fa7c7275bb9ba2e01d553e4115aa4a904a0050`, as its PRODUCT line on https://sqlite.org/download.html says.
- `source.json` records the download URL, size, SHA3-256, and SHA-256 of the archive, and the SHA-256 of each of the two files.
- `bridge.js` loads the build and gives Go the helpers that copy bytes and integers between the two memories.
- `vfs.js` is the VFS that SQLite uses.
- `fs.js` is the in-memory file system that both Go's `os` package and the VFS use.
- `main.go` is the runner, with its page `index.html` and its Worker `worker.js`.
- `update/` is the tool that vendors and verifies the build.

## How it works

- Go and SQLite are two WebAssembly instances with separate memories. The Go binding calls the C functions that the build exports through `syscall/js`, and copies every string and out parameter between the memories.
- All bytes pass through one transfer array, a `Uint8Array` that Go keeps, with the helpers `copyIn` and `copyOut` of `bridge.js`. Every helper returns a number or nothing. The reason: `syscall/js` keeps each JavaScript object that reaches Go in a table until Go's garbage collector frees it, and a view of SQLite's memory per call filled the table's 2^24 entries in a loop of SQLite calls.
- A 64-bit integer passes as a Number when it is exact as one, and as decimal text otherwise, because `syscall/js` has no BigInt.
- `bridge.js` registers the VFS of `vfs.js` as SQLite's default VFS, under the name `s4ql`. None of the build's own VFSes lets several connections in one instance share a database in WAL mode with files that Go can see.
- The VFS keeps its files in the file system of `fs.js`, so a file that SQLite writes is one that `os.ReadFile` reads.
- File locks are kept per file, by connection, with the rules of SQLite's unix VFS inside one process. The wal-index lives in SQLite's own heap, in regions that every connection to the same database maps, and the last connection to unmap frees it.
- The VFS reports what the unix VFS reports by default: power-safe overwrite, and sectors of 4096 bytes.
- `xSleep` waits in real time and calls the Go function `s4qlYield` about once a millisecond. SQLite's C code holds the page's one thread while it sleeps in its busy handler, and during that call Go's runtime runs the goroutines that are ready, so that one of them can release the lock that SQLite waits for.
- The permission bits of `fs.js` bind as they bind a user who is not root: a directory takes and loses entries only with its owner's write bit, and a file opens only with its owner's read or write bit.
- `fs.js` calls every callback from the microtask queue, after the call returns. A callback that JavaScript calls during a call from Go runs on the calling goroutine, and inside a `testing/synctest` bubble that panics with "goroutine is already bubbled". One consequence: a file operation of Go's `os` package finishes only once the JavaScript stack is empty, so never while Go is inside a call into SQLite.
- `faultvfs.New` registers one more VFS of the same kind, named `s4ql-fault-N`. It works on the same files and locks, and has a fault state of its own that makes operations fail as the C VFS of `faultvfs` does.

## Differences from the cgo build

The official build is compiled with other options than `../options.h`. Where a statement can see the difference, the binding makes up for it where it can:

- Every connection runs PRAGMAs when it is opened: a page size of 4096 bytes, a page cache of `-2000` (2000 KiB) for the main and the temporary database, foreign keys on, and temporary files on disk. The official build defaults to 8192 bytes, 16384 KiB, foreign keys off, and temporary files in memory.
- The cgo build has `SQLITE_OMIT_AUTORESET`, so a statement that finished or failed must be reset before it steps again. `Stmt.Step` keeps that rule itself.
- The official build exports `sqlite3_close_v2` and no `sqlite3_close`, so `Conn.Close` itself refuses, with `SQLITE_BUSY`, to close a connection that has a statement that is not finalized, as `sqlite3_close` does.

These differences remain:

- The official build has `SQLITE_THREADSAFE=0`, `SQLITE_OMIT_UTF16`, `SQLITE_OMIT_DEPRECATED`, `SQLITE_USE_URI`, and more extensions, such as FTS5 and R*Tree.
- The VFS is the one in `vfs.js`, where the cgo build uses SQLite's unix VFS.
- The PRAGMA for the page cache reads the schema, so a file that is not a database fails at `Open`. In the cgo build it fails at the first statement.
- The API is the subset that the storage core, its tests, and `faultvfs` use. `Tokenize`, `DenyProcessPragmas`, `AddTestFunctions`, `FindVFS`, `RegisterVFS`, and `UnregisterVFS` are missing.

`make check-browser` compiles the tests of every package that runs in a browser, so the subset cannot drift from the API of the cgo binding unnoticed.

## What a page provides

A Go program built with the tag runs in a Worker. The Worker does the following, in this order:

1. Load `sqlite3.js`, `vfs.js`, `bridge.js`, `fs.js`, and Go's own `lib/wasm/wasm_exec.js`, with `importScripts`. The `wasm_exec.js` must come from the GOROOT of the Go that built the program. `fs.js` comes before it, because `wasm_exec.js` keeps a `globalThis.fs` that is already there.
2. Await `s4qlInitSQLite()`. It loads the build, registers the VFS, and sets `globalThis.s4qlSQLite`, which the Go binding reads. The binding panics if it is not set.
3. Instantiate the Go module with the `importObject` of a `new Go()`, and run it.

`sqlite3.wasm` is served next to `sqlite3.js`.

A page that sets `globalThis.s4qlOutput(fd, bytes)` receives what the program writes to standard output (`fd` 1) and standard error (`fd` 2). Without it, the output goes to `console.log` line by line.

A page served with the headers `Cross-Origin-Opener-Policy: same-origin` and `Cross-Origin-Embedder-Policy: require-corp` is cross-origin isolated, and the VFS then sleeps with `Atomics.wait`. Without them it spins.

The program's working directory is the root of the file system of `fs.js`, which is empty but for `/tmp`. Nothing in it outlives the Worker.

`worker.js` is the example.

## Tests

`make check-browser` builds and vets `BROWSERPKGS` for the browser and compiles their tests. It needs no browser.

`make test-browser` runs the tests of `BROWSERPKGS` in headless Chromium. It builds the runner and passes it to `go test` as the `-exec` program. Its variables:

- `BROWSERPKGS` are the packages, by default `./internal/sqlite`, `./internal/sqlite/faultvfs`, `./internal/core`, `./internal/sim`, and `./internal/store/simstore`.
- `BROWSERFLAGS` go to `go test`, such as `BROWSERFLAGS='-run TestSimulation -v'`.
- `BROWSERTIMEOUT` is the timeout of each test binary, by default `30m`.
- `BROWSERSKIP` are the tests to skip, described below.

To run one package by hand:

```sh
go build -o /tmp/s4ql-browser ./internal/sqlite/browser
GOOS=js GOARCH=wasm go test -tags s4ql_browser -exec /tmp/s4ql-browser ./internal/core
```

The runner serves the page, the program, and the JavaScript on a port of 127.0.0.1, starts Chromium on the page, writes the program's output to its own, and exits with the program's exit code, or with 3 if the page fails. With `-test.timeout` among the arguments, it stops Chromium a minute after that timeout, because the program cannot stop itself while SQLite holds the only thread.

Two environment variables choose the browser:

- `S4QL_CHROMIUM` is the path of Chromium. Without it, the runner takes the first of `chromium`, `chromium-browser`, `google-chrome`, `google-chrome-stable`, and `chrome` on the PATH, and on macOS then Google Chrome or Chromium in `/Applications`.
- `S4QL_CHROMIUM_FLAGS` adds flags, separated by spaces, such as `--no-sandbox` where Chromium's sandbox cannot start.

The program gets the runner's other environment variables whose names start with `S4QL_`, and `GODEBUG` and `GOTRACEBACK`. It gets no others.

`BROWSERSKIP` defaults to `TestSynctestWithCgo/among_parallel_readers`, which matches two canaries in `internal/sim/cgo_test.go`. Each runs eight goroutines that read in a loop that never blocks, beside a writer. Go's js/wasm port has one thread and no preemption, so the first such goroutine runs alone for good and the test hangs. A canary about calls into C and real parallelism has nothing to measure there. A run of `./internal/sim` by hand needs the same `-skip`.

`make lint` also runs `golangci-lint` over the browser's build of `BROWSERPKGS`, whose files a native run does not see.

CI's job `browser` runs `make check-browser`, lints the browser's build, and runs `make test-browser` in the headless Chrome of the runner image, with `S4QL_CHROMIUM_FLAGS=--no-sandbox`.

## Measurements

Measured on an Apple M4 with Chrome 158, headless:

- `make test-browser` passed every test of the five packages, in 3 min 1 s. `internal/core` took 177 s, with 44 top-level tests and 213 subtests passed. Three of its top-level tests skip, as they do natively by default: `TestSimulationSeed` and `TestPlantedDataRace` without their environment variables, and `TestFreeRunning_plantedDataRace` without the race detector, which js/wasm does not have. `internal/store/simstore` took 25 s, `internal/sim` 17 s, and `internal/sqlite` and `internal/sqlite/faultvfs` 2 s each.
- The trace hashes of `TestSimulation` for the default seeds equal the native ones, for each of the four workloads.

Sizes, with `gzip -9` in parentheses:

- `sqlite3.wasm`: 879,430 bytes (406,762)
- `sqlite3.js`: 439,732 bytes (93,665)
- the test binary of `internal/core`: 14.0 MB (3.3 MB)

## Updating the build

Update the build together with the amalgamation in `../c`, as [the binding's README](../README.md#updating-sqlite) describes. `TestVersion` in `../sqlite_browser_test.go` requires that both have the same version. Run these steps from the repository root:

1. Run `make sqlite-browser-update VERSION=x.y.z`. It reads the URL, the size, and the SHA3-256 from the PRODUCT lines on https://sqlite.org/download.html, downloads the archive, checks it, writes `sqlite3.js` and `sqlite3.wasm` unchanged, and rewrites `source.json`.
2. Run `make check-browser`, `make test-browser`, and `make lint`.

For a version that is no longer on the download page, replace step 1 with:

```sh
go run ./internal/sqlite/browser/update -version x.y.z -url URL -sha3 HASH
```

`make sqlite-browser-verify` downloads the archive again, checks it against `source.json`, and against the download page while the version is listed there, and compares the two vendored files with it byte for byte. CI runs it, with `make sqlite-verify`, only on changes to the vendored files, `source.json`, or `update/`.
