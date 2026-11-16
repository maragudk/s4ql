# SQLite in the browser, for the real internal/core

Date: 2026-11-16. Tags: **V** = verified in this run (measured, or read in source, with the reference). **I** = inferred. **NV** = not verified.

Question: how could s4ql's storage code, `internal/core`, run in a web browser, given that its cache needs SQLite? The simulator on the site (`site/simulator/`) is a JavaScript model of the storage design, and Markus asked how we can know that it behaves like the real code. The preferred answer is to compile the real Go code to WebAssembly and have the page only draw what it does. `internal/sim` already builds with `GOOS=js GOARCH=wasm`. `internal/core` does not, because `internal/sqlite` is a cgo binding, and Go's WebAssembly targets cannot link C. This note lists the routes to SQLite inside a browser for Go compiled to WebAssembly, with what was measured on each. It ranks them by how faithfully the browser would run the real `internal/core`. It does not decide.

**Setup.**

- Go 1.27.4, Node 26.13.1, and headless Chromium 157 (`HeadlessChrome/157.0.0.0`, through `playwright-cli` 1.59), on an Apple M4 with 10 cores, macOS.
- The project at commit `de9c77a`, with SQLite 3.54.1 vendored in `internal/sqlite/c/` and the options of `internal/sqlite/options.h`.
- The official SQLite WebAssembly build, from https://sqlite.org/2026/sqlite-wasm-3540100.zip (SHA3-256 `87e24f6a…0050`, which matches the PRODUCT line on https://sqlite.org/download.html), and the npm package `@sqlite.org/sqlite-wasm` 3.54.1-build1 (https://registry.npmjs.org/@sqlite.org/sqlite-wasm/-/sqlite-wasm-3.54.1-build1.tgz), whose `dist/node.mjs` loads in Node. Both report source ID `2026-10-16 14:45:20 555e07ef…ca76`, the same as the vendored amalgamation. Their `sqlite3.wasm` files differ in bytes.
- `github.com/ncruces/go-sqlite3` v0.37.0 (2026-11-07) with `github.com/ncruces/go-sqlite3-wasm/v6` v6.5.35401, and v0.32.0 (2026-03-13) with `github.com/tetratelabs/wazero` v1.11.0. `modernc.org/sqlite` v1.62.0 with `modernc.org/libc` v1.80.1.

**Method.** Throwaway programs in the scratch directory, outside the repository's module, listed at the end:

- *The probe workload*, the same for every route: open a database file, set `journal_mode = wal` and `synchronous = normal`, create a `STRICT` table, insert 5,000 rows in 50 transactions of 100 with one prepared statement, then read the 5,000 rows by key on a second connection in 50 read transactions. Last, a snapshot check: the second connection starts a read transaction and counts the rows, the first inserts one row and commits, and the second counts again before and after its commit. With a working wal-index the counts are 5,000, 5,000, 5,001. This check needs WAL with two connections, which is what `internal/core` uses: one write connection and `Options.Readers` read connections (`internal/core/core.go` 304-318).
- *A copy of the repository* with two files added in `internal/sqlite` and two in `internal/sqlite/faultvfs`, each behind a build tag, which give the API of the cgo binding over another SQLite (§3.3 and §2.4). With them the unchanged `internal/core` and its unchanged tests build for `GOOS=js`, and run in Node through `go_js_wasm_exec` and in Chromium in a Worker. Nothing of this is committed.
- *A browser harness*: a page that starts a Worker, which loads the official build, an optional file system in JavaScript (§3.2), and a Go program, and posts the program's output back to a small local server with the COOP and COEP headers.

Timings are the middle of three runs unless stated. In Node they varied by up to a factor of two between runs; in Chromium by less.

## Short answer

- **Two routes ran the real `internal/core` in Chromium**, with its own tests and its simulation, through a second implementation of the binding's API behind a build tag:
  1. *`ncruces/go-sqlite3` v0.37.0*. It no longer uses wazero: since v0.33.0 (2026-03-21) SQLite is compiled to WebAssembly and then machine-translated to Go source by `wasm2go`, so it is plain Go and builds for `GOOS=js` like any Go package. It ships SQLite 3.54.1, the pinned version. In Chromium, 42 of the 44 top-level tests of `internal/core` passed, and 209 of 211 subtests, in 3 min 25 s. Both failures come from the route, not from core (§3.4). The fault test of the cache passed with a Go port of `faultvfs`.
  2. *The official build*, called from Go through `syscall/js`, with a VFS of about 200 lines of JavaScript that keeps the files in the same in-memory file system that Go's `os` package sees and shares the wal-index between connections. 208 of 211 subtests passed in 4 min 37 s. The three failures are two compile options of the official build (page size 8192 and a 16 MB page cache) and one property of the bridge: while SQLite sleeps in its busy handler, no other goroutine can run (§2.4).
- **The traces of the simulation are the same.** With the default seeds, the four workloads of `TestSimulation` gave the same four trace hashes natively (cgo, the vendored SQLite), on ncruces in Chromium, and on the official build in Chromium. With 25 seeds per workload, native and ncruces in Chromium agreed too. The traces record core's events, not SQLite's internals, and a broken lock in the first version of the JavaScript VFS did not change them; core's own tests found that bug (§2.4).
- **Speed is enough for a page that draws.** The real core in Chromium committed 5,000 rows in 50 commits at 75 to 80 µs a row on ncruces and 36 to 45 µs on the official build, against 4.7 to 5.5 µs natively, and read them at 8 to 9, 12 to 13, and 1.0 to 1.1 µs a row (§6). `TestSimulation` with its default seeds ran in about 30 s of the page's time.
- **What core needs, and the official build's own VFSes lack, is WAL with several connections in one instance.** `opfs`, `opfs-wl`, `opfs-sahpool`, `unix-none` (the default), `memdb`, and `kvvfs` either refuse WAL or need `locking_mode = exclusive`, which allows one connection; core then refuses to open, because it checks the journal mode (`internal/core/cache.go`, `setUp`). The build's `unix` VFS over Emscripten's in-memory file system does run WAL with several connections, measured, but its files are not visible to Go (§2.2).
- **`modernc.org/sqlite` does not build** for `js/wasm` or `wasip1/wasm`: its libc has no files for them (§4).
- **ncruces v0.32.0, the wazero version,** runs inside `GOOS=js` through wazero's interpreter, with SQLite 3.51.3: 92 µs a row to insert and 58 to read in Chromium, 5 to 15 times slower than v0.37.0, and older than the pin (§3.5).
- **For the spec.** Every route is a second binding beside the cgo one, for one build target. On ncruces, the SQLite code is SQLite's, but translated twice by tools of a third party, under a VFS rewritten in Go, with other compile options and three patches (§3.1). On the official build, the C is compiled by SQLite's developers with their options, under a VFS of ours in JavaScript. Building the vendored amalgamation with `options.h` ourselves, for either path, was not tried, because neither Emscripten nor wasi-sdk is installed here (§5).

## 1. What internal/core needs from SQLite (V)

- **The API.** Non-test code uses `OpenVFS`, `Conn.Prepare`, `Conn.Close`, and on `Stmt`: `BindInt64`, `BindText`, `BindNull`, `Step`, `ColumnInt64`, `ColumnText`, `ColumnType`, `Reset`, and `Finalize`, plus `Error` with its extended `Code` and the types `TypeInteger` and `TypeNull` (counted with `grep` over `internal/core/*.go`). The tests also use `sqlite.Open`, `Conn.Exec`, and `faultvfs` with `New`, `Fail`, `Failures`, `Name`, and `Close`. `PrepareFirst`, `Tokenize`, `DenyProcessPragmas`, and the version functions are for the parser's oracle, not for core.
- **The journal mode.** `setUp` runs `pragma journal_mode = wal` and refuses any other answer, then `pragma synchronous = normal` (`cache.go`). The write connection opens first, then `Options.Readers` read connections, each with `pragma query_only = on` and a busy timeout (`core.go` 300-318). So the cache needs WAL with a wal-index shared by several connections of one process.
- **Files beside the cache.** Spool files are made with `os.CreateTemp` next to the cache and removed at once (`spool.go` 36-40). The tests read and copy the cache's files with `os`: the simulation copies the database and its log at a crash (`simulation_test.go` 1113-1125), and other tests damage the file or check that the log is gone (`core_test.go` 70, 1075, 1142). In a browser, the cache must therefore be in a file system that both SQLite and Go's `os` reach.
- **The fault VFS.** `faultvfs` is C that wraps SQLite's default VFS by its function pointers, armed from Go. One test of core uses it, with `Fault{Ops: Write, Files: WAL}` (`core_test.go` 821-870).
- **The pinned build.** SQLite 3.54.1, with the ten options of `options.h`. The binding reports 46 compile options (`sqlite.CompileOptions`, run in the copy).
- **The simulation's assumption about calls into SQLite.** `internal/sim` relies on a goroutine inside a SQLite call counting as running, so that fake time does not move during the call (`internal/sim/README.md`, "testing/synctest works with cgo"). In `GOOS=js`, a call into JavaScript through `syscall/js` is synchronous, and a `js.Func` made inside a bubble keeps the bubble from going idle until it is released (`syscall/js/func.go` 45-75, which calls `internal/synctest.Acquire`). Calls that return through a JavaScript callback, as the file system calls of Go's `os` package do on `GOOS=js` (`syscall/fs_js.go` 531-559), are covered by that.

## 2. Route 1: the official SQLite WebAssembly build, bridged by JavaScript

### 2.1 What it is (V)

- Emscripten 5.0.5 (header of `dist/node.mjs`), SQLite 3.54.1, `sqlite3.wasm` of 879,430 bytes (406,762 gzip) with 439,732 bytes of JavaScript in `sqlite3.js`. It exposes the C API as `sqlite3.capi`, the raw exports as `sqlite3.wasm.exports`, an object API `oo1`, and a Worker API (https://sqlite.org/wasm/doc/trunk/api-index.md). The C API is synchronous; even the OPFS VFS makes the asynchronous OPFS API synchronous for SQLite (https://sqlite.org/wasm/doc/trunk/vfs.md).
- **The VFSes registered**, read with `sqlite3_vfs_find` and `pNext`: in Node `unix-none` (the default), `memdb`, `kvvfs`, `unix-excl`, `unix-dotfile`, `unix`; in a Chromium Worker also `opfs` and `opfs-wl`, and `opfs-sahpool` after `installOpfsSAHPoolVfs()`. The `unix` family is SQLite's own `os_unix.c` over Emscripten's in-memory file system.
- **WAL, as documented:** "As of version 3.47, it is possible to activate WAL mode for OPFS-hosted databases", but "Because the WASM build does not have shared memory APIs, activating WAL requires that a client specifically activate exclusive-locking mode", and "the requirement for exclusive locking eliminates all concurrency support from the" `opfs` and `opfs-wl` VFSes. The same page says: "Never use two database handles to the same db file within the same thread" (https://sqlite.org/wasm/doc/trunk/persistence.md). `opfs` needs a Worker and the COOP and COEP headers; `opfs-sahpool` needs a Worker only.
- **Custom VFSes.** "It is possible to create custom sqlite3_vfs implemenations entirely in JavaScript" (vfs.md). `sqlite3.vfs.installVfs` takes a `sqlite3_vfs` and a `sqlite3_io_methods` struct with JavaScript methods (`dist/node.mjs` 12793-12845). The struct wrapper has all members of version 3, `xShmMap` to `xUnfetch` included (V, listed from `structInfo`). The constants `SQLITE_SHM_UNLOCK` and `SQLITE_SHM_EXCLUSIVE` are missing from `capi` (both `undefined`, measured), so a VFS must use the numbers.
- **Compile options**, 59 reported. Against the 46 of the project's build: the official build lacks `DEFAULT_FOREIGN_KEYS`, `OMIT_AUTORESET`, and `THREADSAFE=1`, has `DEFAULT_PAGE_SIZE=8192` (project 4096), `DEFAULT_CACHE_SIZE=-16384` (project -2000), `TEMP_STORE=2` (project 1), `OMIT_UTF16`, `OMIT_DEPRECATED`, `THREADSAFE=0`, and adds FTS5, R*Tree, the session extension, four virtual tables (`ENABLE_BYTECODE_VTAB`, `ENABLE_DBPAGE_VTAB`, `ENABLE_DBSTAT_VTAB`, `ENABLE_STMTVTAB`), and `ENABLE_COLUMN_METADATA`, `ENABLE_OFFSET_SQL_FUNC`, `ENABLE_PERCENTILE`, `ENABLE_PREUPDATE_HOOK`, and `ENABLE_UNKNOWN_SQL_FUNCTION`. Of these, foreign keys and the page size can be set per connection with a PRAGMA; `OMIT_AUTORESET`, `OMIT_UTF16`, and the threading mode cannot (I, from https://sqlite.org/compile.html).

### 2.2 The probe on each of its VFSes (V)

The probe workload from Go through `syscall/js` (§2.3), in a Chromium Worker:

| VFS | `journal_mode = wal` gives | Snapshot check | Insert, µs a row | Read, µs a row |
|---|---|---|---|---|
| `opfs` | `delete` | `database is locked` | 37.2 | 7.9 |
| `opfs-wl` | `delete` | `database is locked` | 40.5 | 8.7 |
| `opfs-sahpool` | `delete` | `database is locked` | 31.0 | 6.0 |
| `memdb` | `memory` | `database is locked` | 7.1 | 4.3 |
| `unix-none` (default) | `delete` | counts right, without locks | 8.4 | 4.3 |
| `memshm` (ours, §2.4) | `wal` | 5,000, 5,000, 5,001 | 10.4 | 5.4 |

- In rollback mode the reader's open transaction blocks the writer's commit, so the writer gets `SQLITE_BUSY`, as SQLite documents for that mode. The OPFS VFSes also log `NoModificationAllowedError: ... Access Handles cannot be created if there is another open Access Handle` while they retry the lock.
- `unix-none` takes no locks at all, so its right counts show nothing about isolation.
- The OPFS files persist across page loads: the second and third runs on the same path met the rows of the first and failed with `UNIQUE constraint failed`. Only the first run of each OPFS VFS is in the table.
- In Node, the same bridge on the `unix` and `unix-excl` VFSes gave `wal` and the right counts, and `unix-dotfile` gave `delete`. A second `begin immediate` on `unix` got `SQLITE_BUSY`, and with `busy_timeout = 300` it waited 375 ms before it did. Emscripten's `fcntl` makes `F_SETLK` and `F_SETLKW` succeed and `F_GETLK` report no lock (`dist/node.mjs` 2097-2125), so the exclusion comes from SQLite's own bookkeeping of locks per process in `os_unix.c`, which is all that one instance needs (I). These files are in Emscripten's file system, which the build does not export, so Go's `os` cannot read or copy them.

### 2.3 Go and SQLite as two instances (V)

- **The bridge.** A Go program for `GOOS=js` reaches the official build through `syscall/js`: it calls `sqlite3.wasm.exports.sqlite3_prepare_v3` and the others by name. The two instances have separate linear memories. Every SQL text, bound text, and column text is copied with `js.CopyBytesToJS` or `js.CopyBytesToGo` into or out of memory from `sqlite3_malloc`, and every out parameter (the `sqlite3*`, the `sqlite3_stmt*`, the tail) is read back from SQLite's memory. The sketch in the scratch directory is 304 lines of Go for the API of §1 (`sqlite_official.go`) and a few lines of JavaScript helpers in the page's Worker.
- **Integers.** `syscall/js` turns an `int64` into a JavaScript `Number`, which holds 53 bits, and has no `BigInt` conversion. The build takes and returns `BigInt` for 64-bit values. The sketch passes them as decimal strings through two JavaScript helpers. Core's values reach 2^50 and more (`tx_test.go`, `large`), so this is needed for correctness, not only for range.
- **The exports differ from the C API's symbols.** `sqlite3_close` is not exported; `sqlite3_close_v2` is. The sketch uses it, so a close with statements still open does not fail with `SQLITE_BUSY` as in the cgo binding, but leaves a zombie connection.
- **Cost of a call.** One million calls of `sqlite3_libversion_number` took 807 ns each through `syscall/js`, and 6 ns each through `//go:wasmimport sqlite3 sqlite3_libversion_number` with the build's exports passed as the import module `sqlite3` when Go's module is instantiated (Node). `go:wasmimport` builds for `GOOS=js` in Go 1.27 (V). Through it, `int64` parameters are WebAssembly `i64` and need no strings. Strings still need a copy by JavaScript, because Go cannot address the other instance's memory. A binding on `go:wasmimport` was not written.
- **Size.** The probe through the bridge is 2.88 MB of Go (818 KB gzip), plus the build's 1.32 MB (500 KB gzip). The real core probe (`cmd/coreprobe`, §6) is 10.2 MB (2.5 MB gzip).

### 2.4 The real core on the official build (V)

- **The VFS.** None of the build's VFSes gives both WAL with several connections and files that Go's `os` sees (§2.2). `memshm.js`, written for this run, is a VFS in JavaScript of 210 lines: its files are the nodes of the same JavaScript file system that Go's `os` uses (§3.2); file locks are kept per node; `xShmMap` allocates the wal-index regions with `sqlite3.wasm.alloc` in SQLite's heap, shared by every connection of the instance; `xShmLock` keeps shared and exclusive holders per slot. Each `faultvfs.New` registers one more such VFS on the same files, with its own fault state, which Go arms through `syscall/js` (`faultvfs_official.go`, 85 lines). The binding opens through `memshm` when the VFS name is empty.
- **The first version had a bug.** It read the lock flags from `capi.SQLITE_SHM_UNLOCK` and `capi.SQLITE_SHM_EXCLUSIVE`, which are `undefined` (§2.1), so every shm lock was taken as shared and two connections could both write. The probe's snapshot check passed with it. The trace hashes of the simulation were the same as native with it. Core's tests found it: `TestDB_Begin` expected "the cache is busy" and got no error. After the fix, a second `begin immediate` gets `SQLITE_BUSY`, and waits out a busy timeout of 300 ms (Node, `locktest.mjs`).
- **Core's tests in Chromium, after the fix.** `go test -c -tags s4ql_official ./internal/core`, run with `-test.v`: 42 of 44 top-level tests and 208 of 211 subtests passed, in 4 min 37 s. The three failures:
  - *"should stop for good when SQLite reports that the cache is corrupt"*: the test damages the file from byte 8,192 on, after a checkpoint, and expects the read to meet the damage in its scan. Here `DB.Read` itself failed with `database disk image is malformed`. With the build's page size of 8,192 bytes, byte 8,192 starts the second page instead of the third (I: the page size is measured, the cause is inferred).
  - *"should stop for good with a local failure ... when the file system fails while replay applies a change"*: the fault was armed and writes to the log failed, but "replay read 4413519 of 4413519 bytes". The test depends on SQLite's page cache of 2 MB spilling before the commit (`internal/core/README.md`); the build's cache is 16 MB (`DEFAULT_CACHE_SIZE=-16384`), so nothing spilled before the commit (I, from the option and the message).
  - *"should wait for a lock on the cache that another connection holds for a while"*: a goroutine is to release the lock after 200 ms while core waits in SQLite's busy handler. SQLite's busy handler sleeps inside the SQLite instance, which blocks the only thread, so the goroutine never ran, and the wait ended with `SQLITE_BUSY` after the busy timeout of one minute. With cgo, the goroutine runs on another thread; on ncruces, the busy handler is Go code (§3.1).
- **The simulation.** The four trace hashes of `TestSimulation` with the default seeds were the same as native (§7).

## 3. Route 2: github.com/ncruces/go-sqlite3

### 3.1 What it is now (V)

- **No wazero since v0.33.0.** From the go.mod of each release on proxy.golang.org: v0.28.0 to v0.32.0 require `github.com/tetratelabs/wazero`; v0.33.0 (2026-03-21) and later require `github.com/ncruces/go-sqlite3-wasm` instead. v0.37.0's README: "It wraps a Wasm build of SQLite, and uses wasm2go to translate it to Go." The translation is one generated file, `sqlite3.go`, of 4.6 MB ("Code generated by wasm2go v0.4.20. DO NOT EDIT.").
- **The build** (`go-sqlite3-wasm/v6/build/`): `download.sh` fetches `sqlite-autoconf-3540100.tar.gz` and checks its SHA3-256, plus five extensions from `ext/misc` at the tag `version-3.54.1`, then applies three patches. `build.sh` compiles with wasi-sdk's clang (`--target=wasm32 -ffreestanding -nostdlib -Oz`, `-DSQLITE_CUSTOM_INCLUDE=sqlite_opt.h`), optimizes with binaryen's `wasm-opt`, and translates with `wasm2go`. `SQLITE_OS_OTHER` is set: SQLite's `os_unix.c` is not used, and the VFS is the Go package `vfs`.
- **The patches.** `busy_timeout.patch` replaces SQLite's default busy callback with one in Go, "so Go can handle, and interrupt, sqlite3_busy_timeout"; `clearbinds.patch` backports https://sqlite.org/src/info/535a77cf5a into `sqlite3_clear_bindings`; `vfs_find.patch` removes SQLite's VFS registry, because "Go handles VFS registration."
- **Compile options**, 61 reported. Against the project's: ncruces lacks `OMIT_AUTORESET` and has `THREADSAFE=0`, `MAX_MMAP_SIZE` of 1 GiB (project 0), `DEFAULT_WAL_SYNCHRONOUS=1` (project 2), `ENABLE_STAT4`, `TRUSTED_SCHEMA=0` (not in the list, set in `sqlite_opt.h`), `LIKE_DOESNT_MATCH_BLOBS`, `OMIT_DEPRECATED`, `OMIT_AUTOINIT`, `OMIT_DESERIALIZE`, `ENABLE_SETLK_TIMEOUT`, `ENABLE_PREUPDATE_HOOK`, `ENABLE_COLUMN_METADATA`, `ENABLE_ATOMIC_WRITE`, `ENABLE_BATCH_ATOMIC_WRITE`, `SOUNDEX`, and `USE_URI`. The page size (4096), the page cache (-2000), `DQS=0`, foreign keys on by default, and the math functions are the same as the project's.
- **The VFS.** "Since this is a from scratch reimplementation, there are naturally some ways it deviates from the original. It's also not as battle tested as the original." (`vfs/README.md`). Locks and shared memory depend on the platform. For `GOOS=js`, which is not `unix` to Go's build constraints, the default build has no file locking and no shared memory (`vfs.SupportsFileLocking` and `vfs.SupportsSharedMemory` are both false, measured), and WAL then needs exclusive locking mode. With the build tag `sqlite3_dotlk`, locks are dot-files and directories, and the wal-index is "in-process, memory sharing ... through copying" (`vfs/README.md`, `vfs/os_dotlk.go`, `vfs/shm_copy.go`). The `vfs` package also lets a VFS be written in Go (`vfs.VFS`, `vfs.File`, `vfs.FileSharedMemory` in `vfs/api.go`).
- **Its tests.** The README lists 20 OS and CPU combinations run on every commit, SQLite's `mptest` for the VFS, and `speedtest1` for the translation. `js` and `wasip1` are not among them.

### 3.2 Building and running it for GOOS=js (V)

- It builds for `GOOS=js GOARCH=wasm` and for `GOOS=wasip1 GOARCH=wasm` with no change. The probe is 14.05 MB (3.87 MB gzip, 2.76 MB brotli).
- **In Node**, through Go's `wasm_exec_node.js`, which passes Node's `fs` to Go: without `sqlite3_dotlk`, opening a file gave `disk I/O error` at the first statement; `memdb` worked in rollback mode, and the snapshot check got `database is locked`. With `sqlite3_dotlk`, the probe on a real file gave `wal` and the right counts: 55 to 63 µs a row to insert and 45 to 52 to read, against 0.5 to 1.1 and 0.3 natively for the same program and 0.4 and 0.3 for the project's cgo binding.
- **In a browser, Go's `os` package has no file system.** `wasm_exec.js` installs a `globalThis.fs` whose calls fail with `ENOSYS`, except writes to standard output, and Go's `syscall` calls `globalThis.path.resolve`, which a browser lacks (`lib/wasm/wasm_exec.js` 8-72, `syscall/fs_js.go` 18-124). `memfs.js`, written for this run, is an in-memory file system of 134 lines with the Node callback API that `syscall/fs_js.go` calls (`open`, `read`, `write`, `fstat`, `ftruncate`, `mkdir`, `readdir`, `rename`, `unlink`, and the rest), loaded before `wasm_exec.js`. With it, the dotlk probe in Chromium gave `wal` and the right counts, at 19.5 µs a row to insert and 7.5 to read.
- **A trap with synctest.** The first `memfs.js` called each callback before returning. Inside a `testing/synctest` bubble that panics with `goroutine is already bubbled`, because a callback that JavaScript calls during a call from Go runs on the calling goroutine (`syscall/js/func.go`: "Other wrapped functions which are triggered during a call from Go to JavaScript get executed on the same goroutine"), and `FuncOf` wraps it in `bubble.Run`. Calling the callbacks from `queueMicrotask`, as Node calls them later, fixed it.

### 3.3 The real core on ncruces (V)

- **The binding.** `sqlite_js.go`, 156 lines behind `//go:build js && !s4ql_official`, gives the API of §1 over ncruces's `Conn` and `Stmt`, and `sqlite.go` gets `//go:build !js`. A VFS other than the default is chosen with a URI, `file:PATH?vfs=NAME`. Extended result codes come from `sqlite3.Error.ExtendedCode`.
- **The fault VFS.** `faultvfs_js.go`, 236 lines, gives the API of `faultvfs` as a Go VFS that wraps ncruces's default VFS, fails the same operations with the same result codes, and passes `SharedMemory` on, so that WAL works through it.
- **In Node**, `GOOS=js GOARCH=wasm go test -tags sqlite3_dotlk ./internal/core` with `go_js_wasm_exec` on the path: everything passed except the two tests below, in 1,636 s, against 72.6 s natively. The machine was running other work for part of that time.
- **In Chromium**, the test binary (24.9 MB) in a Worker with `memfs.js`: 42 of 44 top-level tests and 209 of 211 subtests passed, in 3 min 25 s. `TestDB_stop_cache`, with the fault test, passed in 3.9 s.

### 3.4 The two failures on ncruces (V)

- **`TestSpool`, "should keep no spool file under a name"**: the directory of the cache holds `a.db.lock`, the lock directory of `sqlite3_dotlk`, which the test does not expect (the name was printed in a run with a log line added).
- **`TestTx_Commit_large`**: "the heap grew by 11571856 bytes for a commit object of 23389389 bytes", over the limit of 8 MiB (`tx_test.go` 933). The translated SQLite keeps its memory in a Go slice, so SQLite's page cache and allocations count as Go heap, which the test measures; with cgo they are in C's heap (I, from `sqlite3.go`, whose `Module` holds `memory *[]byte`). On the official build the same test passed, with 3.8 MB of growth, because SQLite's memory is in its own instance.

### 3.5 ncruces v0.32.0, the wazero version (V)

The question was whether WebAssembly can run WebAssembly: Go compiled to `js/wasm`, with wazero inside it running SQLite. It can. wazero's compiler targets only some native CPUs, so in `js/wasm` it interprets. v0.32.0 embeds SQLite 3.51.3 (source ID `2026-03-13 10:38:09 737ae4a3…d618`), older than the pin. The probe is 8.6 MB (2.4 MB gzip). Natively it gave 0.8 and 0.5 µs a row. In Node with `sqlite3_dotlk`: opening took 940 to 975 ms, and the rows 310 µs to insert and 227 to read. In Chromium with `memfs.js`: opening 214 to 229 ms, and 92 and 58 µs a row, with WAL and the right counts. It is superseded by the translated version of §3.1.

## 4. Route 3: modernc.org/sqlite (V)

`modernc.org/sqlite` v1.62.0 is SQLite 3.54.1 translated from C to Go by ccgo, once for each GOOS and GOARCH. Its `doc.go` lists 21 combinations: darwin, freebsd, illumos, linux, netbsd, openbsd, and windows on various CPUs. `GOOS=js GOARCH=wasm go build` and `GOOS=wasip1 GOARCH=wasm go build` both fail with `modernc.org/libc/errno: build constraints exclude all Go files` and the same for `modernc.org/libc/limits`. A port would mean a new target in ccgo and `modernc.org/libc`. The project dropped `modernc.org/sqlite` before for the oracle (`internal/sqlite/README.md`, "Why our own binding").

## 5. Other routes

- **Building the vendored amalgamation ourselves (NV).** Both translated routes start from a compiled SQLite, so one built from `internal/sqlite/c/sqlite3.c` with `options.h` would have the pinned code and options exactly. Two toolchains exist: Emscripten, as the official build uses (SQLite's `ext/wasm` in the canonical source tree), and wasi-sdk with `wasm2go`, as ncruces's `build.sh` does. The C of `faultvfs` would compile into the same module and wrap the default VFS by its function pointers, as it does natively. With Emscripten the default VFS would be `os_unix.c` over Emscripten's file system, which §2.2 shows runs WAL with several connections; the file system would need to be exported and then given to Go's `os` as `globalThis.fs` (I). Apple clang here compiles C for `wasm32`, but there is no `wasm-ld`, Emscripten, or wasi-sdk, and installing them was outside this run.
- **wasip1.** ncruces builds for `GOOS=wasip1` (14.0 MB), and `go:wasmimport` works there too. In a browser it needs a WASI shim in JavaScript for the files, and gives Go nothing that `GOOS=js` lacks for this use. Not run (NV).
- **TinyGo.** It "embeds the Clang compiler to parse `import "C"` blocks", with "some features of Cgo ... still unsupported", and its `reflect` is a reimplementation of which "some parts are not yet fully supported" (https://tinygo.org/docs/reference/lang-support/). Whether it compiles `internal/sqlite` with the amalgamation in its preamble, protobuf-go, and `testing/synctest` is not known. TinyGo is not installed here (NV).
- **wa-sqlite** (https://github.com/rhashimoto/wa-sqlite): another Emscripten build of SQLite, made for VFSes written in JavaScript, with synchronous, Asyncify, and JSPI variants and VFSes on IndexedDB and OPFS, one of them named `OPFSWriteAheadVFS`. Its version and WAL support were not checked (NV). As a route it is the official build's route with another build of the C.
- **sql.js**: an Emscripten build with an in-memory database only (NV).
- **Rewrites of SQLite** in other languages, such as Turso's, are not SQLite's C code, and so not candidates for running the real cache.

## 6. Measured numbers (V)

The probe workload, µs a row, inserts then reads on the second connection, middle of three runs:

| Where | Route | VFS | WAL, snapshot right | Insert | Read |
|---|---|---|---|---|---|
| native | project's cgo binding | `unix` | yes | 0.4 | 0.3 |
| native | ncruces v0.37.0 | Go | yes | 0.6 | 0.3 |
| Chromium | ncruces v0.37.0, dotlk | Go, over `memfs.js` | yes | 19.5 | 7.5 |
| Chromium | ncruces v0.37.0 | `memdb` | no | 13.8 | 4.0 |
| Chromium | ncruces v0.32.0 (wazero), dotlk | Go, over `memfs.js` | yes | 92.6 | 58.6 |
| Chromium | official, `syscall/js` | `memshm` | yes | 10.4 | 5.4 |
| Chromium | official, `syscall/js` | `opfs-sahpool` | no | 31.0 | 6.0 |
| Node | ncruces v0.37.0, dotlk | Go, over Node's `fs` | yes | 63.1 | 50.3 |
| Node | official, from JavaScript (no Go) | `unix-none` | no | 13.8 | 3.9 |

The real core (`cmd/coreprobe`: `core.Open` on the simulated store with the free-running host, then 5,000 upserts in 50 commits and 5,000 `Get` in 50 read transactions), µs a row, three runs:

| Where | Route | Upsert | Get | Binary |
|---|---|---|---|---|
| native | cgo binding | 4.7 to 5.5 | 1.0 to 1.1 | 10.6 MB |
| Chromium | ncruces v0.37.0 | 74.9 to 79.7 | 7.8 to 9.1 | 21.2 MB, 5.3 MB gzip, 3.8 MB brotli |
| Chromium | official build | 36.5 to 45.3 | 11.9 to 13.1 | 10.2 MB, 2.5 MB gzip, plus 1.3 MB of SQLite |

Opening took 115 to 123 ms on ncruces and 23 to 27 ms on the official build. Chromium compiled and instantiated each Go module in 5 to 41 ms.

All tests of `internal/core`: 72.6 s natively (subtests in parallel on 10 cores), 3 min 25 s in Chromium on ncruces, 4 min 37 s in Chromium on the official build (one thread), 27 min in Node on ncruces with real files.

## 7. The simulation's traces (V)

`TestSimulation` prints a hash of all traces of each workload (`workloads_test.go` 243). Each workload runs every seed twice and fails if the traces differ.

| Workload (runs) | native, cgo | Chromium, ncruces | Chromium, official |
|---|---|---|---|
| one instance (20) | `474a8a3e…b46c` | `474a8a3e…b46c` | `474a8a3e…b46c` |
| writes that land late (20) | `a063e0fc…03c7` | `a063e0fc…03c7` | `a063e0fc…03c7` |
| instances that compete (60) | `9e400673…7809` | `9e400673…7809` | `9e400673…7809` |
| callers that never pause (80) | `5367e1c2…7438` | `5367e1c2…7438` | `5367e1c2…7438` |

With `S4QL_SIM_SEEDS=3` and `S4QL_SIM_SEEDS=25`, native and Chromium on ncruces also gave equal hashes for all four workloads (`82fd1fae…`, `110cba33…`, `53e8afda…`, `4d64cf3b…` for 25 seeds). The official build was not run with those counts.

What this shows: for these seeds, the steps of the scheduler and the lines the instances and tests write are the same whichever SQLite runs below. What it does not show: that SQLite behaves the same inside. The broken shm locks of §2.4 gave the same hashes, because under the scheduler two connections never contend (`internal/sim/README.md`: "It never has two goroutines inside SQLite").

`TestFreeRunning`, which runs real goroutines with the scheduler off, passed on both routes, but in a browser there is one thread, so its goroutines interleave and never run in parallel. `-race` is not available for `GOOS=js`.

## 8. What each route means for the spec and for faultvfs

The spec says: "Our own SQLite binding. The cache and the oracle use the official SQLite C code, at a version we pin, through a small cgo binding of our own. The module therefore needs cgo." (`docs/spec.md` 54). The binding's README gives the reasons: the oracle is the real C code, the project chooses the version and the options, and no third-party code sits between the engine and SQLite.

- **Any browser route adds a second implementation of the binding's API for one target**, behind a build tag, so the rule would need a sentence for that target. In this run the second implementations were 156 lines (ncruces) and 304 lines (official), and the cgo binding is 620.
- **ncruces.** The SQLite code is the pinned release, but what runs is SQLite compiled by wasi-sdk, rewritten by `wasm2go`, and compiled again by Go, under ncruces's own VFS in Go, with its options and three patches. That is third-party code between core and SQLite, at two levels. The version follows ncruces's releases, not the pin, unless the project rebuilds the translation itself with ncruces's tools and `options.h` (NV). `faultvfs` becomes a second implementation in Go (236 lines here), so the fault test checks a different fault VFS in the browser.
- **The official build.** The C is SQLite's, compiled by SQLite's developers, at a version that matches the pin today (3.54.1 on both). The options are theirs, and three of core's tests already depend on the difference (§2.4). The VFS under the cache is ours, in JavaScript (210 lines here), and the fault injection is in it, so the fault test checks that code. Calls cross two instances through JavaScript, at about 0.8 µs each through `syscall/js`, or 6 ns through `go:wasmimport` with copies for strings.
- **Our own build of the amalgamation (§5).** It would keep the pinned C and `options.h`, and could keep `faultvfs.c` and `os_unix.c` unchanged. The new code would be the build, the bridge to Go, and, with Emscripten, a shim from Emscripten's file system to `globalThis.fs`. Not tried.

## 9. The routes, ranked by how faithfully the browser would run the real internal/core

1. **ncruces v0.37.0 with a Go port of `faultvfs` and a JavaScript file system for Go.** Measured: core and its tests ran unchanged in Chromium, 209 of 211 subtests passed, the two failures come from the route, and the traces equal native. It runs SQLite 3.54.1, the pin, under a VFS that is not SQLite's, with other options and three patches, translated by third-party tools.
2. **Our own build of the vendored amalgamation, bridged to Go.** Not built. It is the only route that would run the pinned C with `options.h`, SQLite's own VFS, and the C `faultvfs`, so it would be first on fidelity of the SQLite side if it worked. Its risks are the new bridge and file system shim, which this run measured only in the official build's form.
3. **The official build with a VFS of ours and the `syscall/js` bridge.** Measured: 208 of 211 subtests in Chromium, and the traces equal native. It runs SQLite's C as SQLite's developers build it, under our JavaScript VFS, with options that differ from the pin in ways that three of core's tests detect. It is below ncruces here because of those option differences and because the VFS under the cache is new code of this run, which already had one bug that only core's tests caught.
4. **ncruces v0.32.0 on wazero.** Works with WAL, 5 to 15 times slower than v0.37.0, with SQLite 3.51.3. Superseded by 1.
5. **The official build on its own VFSes** (`opfs`, `opfs-wl`, `opfs-sahpool`, `memdb`, `kvvfs`, `unix-none`). Core refuses to open the cache without WAL with several connections. Only for a design without read connections.
6. **modernc.org/sqlite, and TinyGo, wasip1, wa-sqlite, sql.js.** modernc does not build for WebAssembly; the others were not measured.

## Not verified

- A build of the vendored amalgamation with `options.h` for WebAssembly, with Emscripten or with ncruces's tools (§5).
- Firefox and Safari. Only headless Chromium 157 was used.
- The official build's OPFS VFSes with core. They cannot serve it as it is (§2.2), and the simulator has no need to persist across page loads.
- TinyGo, wa-sqlite, sql.js, and running the `wasip1` builds.
- A binding on `go:wasmimport` beyond the one function timed in §2.3.
- The cause of the corruption test's failure on the official build, beyond the page size (§2.4).

## Where the material is

In the scratch directory `/private/tmp/claude-502/-Users-maragubot-Developer-s4ql/68c0d47f-ada8-4603-995b-2482fe6fef3a/scratchpad/`: `progs/` (the probe workload and the ncruces, modernc, bridge, and `go:wasmimport` programs, in module `example.com/probe`), `progs-wazero/` (the probe on ncruces v0.32.0), `s4ql-js/` (the copy of the repository with `internal/sqlite/sqlite_js.go`, `sqlite_official.go`, `faultvfs/faultvfs_js.go`, `faultvfs/faultvfs_official.go`, `cmd/coreprobe`, and `cmd/cgobench`), `www/` (the page, `worker.js`, `memfs.js`, `memshm.js`, and the built `.wasm` files), `js/` (the Node hosts, the server, and the scripts), `logs/` (the full output of every browser run, `all.log` for ncruces and `allo2.log` for the official build), `core-js-test.log` (the Node run), `bench-browser.txt` and `bench-node.txt`, and the downloads in `dl-sqlite-wasm/` and `dl-npm-sqlite-wasm/`.
