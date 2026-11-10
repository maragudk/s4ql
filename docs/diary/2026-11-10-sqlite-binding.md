# Diary: SQLite binding

The spec says the cache and the oracle use the official SQLite C code, at a version we pin, through a small cgo binding of our own. This task builds that binding, cross-checks it against two references that run the same C code, and settles a question left open in `/AGENTS.md`: whether two copies of SQLite's C code can be linked into one test binary. It is the first code in the repository.

## Step 1: Requirements and scope

**Author:** main

### Prompt Context

**Verbatim prompt:** Start 3 in parallel and let’s discuss 2. Use Opus builders.
**Interpretation:** "3" was the third open item in my summary of where the project stood: the unverified SQLite symbol clash. I had said a builder could start on the binding in parallel and that doing so would also settle the clash, so the task is the binding itself, with the clash as a question it must answer with evidence. "2" is the story for a local commit failing after the manifest swap, which Markus and I discuss separately while the builder works.
**Inferred intent:** Get the first real code moving on a piece that does not depend on the two open design questions, so that the design discussion does not block all progress.

### What I did

I read `/docs/spec.md`, `/docs/roadmap.md`, the notes for the binding in `/AGENTS.md`, and the version table in `/docs/research/sqlite-cache-and-oracle.md`. I created the worktree `.claude/worktrees/sqlite-binding` on branch `worktree-sqlite-binding`, wrote this entry, and briefed one builder on Opus.

The requirements handed to the builder:

- Vendor the official SQLite amalgamation at 3.54.1, with its source and checksum recorded, and a repeatable way to update it.
- A small cgo binding in an internal package: open and close a database, prepare, bind, step, read typed columns, reset and finalize, run transactions, and report errors with SQLite's extended result codes and messages. It also reports the SQLite version and compile options it was built with.
- Compile options are chosen by the builder, each with a written reason, and flagged for review, because they shape the oracle's behaviour.
- Tests run under the C address sanitizer, and there is a fuzz test.
- Cross-checks against the official `sqlite3` shell built from the same vendored source with the same options, and against `github.com/mattn/go-sqlite3` pinned to the same SQLite version. The tests fail if the versions drift apart.
- A written, evidence-backed answer to the symbol-clash question, and a test that guards whichever approach is chosen.
- A preference, not a rule: keep `mattn/go-sqlite3` out of the main module's `go.mod` if that can reasonably be done.

Out of scope: the SQL engine, the cache schema, the session extension and changesets, a `database/sql` driver, a connection pool, a custom VFS, anything touching S3, and returning several rows per call into C.

No telemetry is asked for. The binding is a library with no service code around it yet.

### Why

The binding has no dependency on the two open design questions (how the drivers run the state machine, and the local-commit-failure story), and both the cache and the oracle need it before slice 1 can be built.

### What worked

The research note already records that the latest SQLite release is 3.54.1 and that `mattn/go-sqlite3` v1.14.53 bundles the same version, so the pin and the reference agree without any work.

### What didn't work

Nothing has failed yet. No code has been written.

### What I learned

SQLite 3.54.1 (2026-10-16) is the latest release, and `mattn/go-sqlite3` v1.14.53 (2026-11-05) bundles exactly that version, according to `/docs/research/sqlite-cache-and-oracle.md`.

### What was tricky

Deciding how much of the compile-option choice to make up front. I left it to the builder to propose and justify, because the right set depends on details in the research notes and on what the cross-checks tolerate, and I would rather review a reasoned list than guess one.

### What warrants review

The interpretation of "3" as the whole binding and not only a spike on the symbol clash. Markus did not object when I proposed it, but he did not confirm it in so many words either.

### Future work

The compile options need Markus's agreement before they are treated as settled, and the pinned version and options may belong in the spec once agreed.

## Step 2: Vendor SQLite 3.54.1 and answer the symbol-clash question

**Author:** binding-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Vendored SQLite. The official amalgamation at version 3.54.1, from sqlite.org, with the download URL and checksum recorded and verified, and a repeatable, documented way to update to another version (a script or Makefile target). Include what is needed to build the official `sqlite3` shell from the same source." and "The symbol-clash question. `AGENTS.md` records as unverified that two copies of SQLite's C code normally cannot be linked into one binary. Answer it with evidence: try it, record exactly what happens (verbatim errors), and choose between compiling our copy with file-local symbols, running the reference in a separate process, or something better. Guard the chosen approach with a test."
**Interpretation:** Vendor the amalgamation with verifiable provenance and an update tool. Then link our copy and mattn's copy into one test binary, record what breaks, and choose and guard a way around it.
**Inferred intent:** Trust in the oracle depends on knowing exactly which C code runs, and on comparing it in-process with a reference built from the same code.

### What I did

I wrote the update tool `/internal/sqlite/update/main.go`. With `-version` it reads the URL, size and SHA3-256 from the machine-readable `PRODUCT` lines of https://sqlite.org/download.html and downloads the zip. It checks the size and the SHA3-256, then writes `sqlite3.c`, `sqlite3.h`, `sqlite3ext.h` and `shell.c` into `/internal/sqlite/c/`. The manifest `/internal/sqlite/c/source.json` records the URL, size, SHA3-256 and SHA-256 of the zip, and the SHA-256 of each file. `make sqlite-update VERSION=x.y.z` and `make sqlite-verify` wrap the tool. The download page gives the SHA3-256 of `2026/sqlite-amalgamation-3540100.zip` as `92281c86e2957f9b9305890f9ac04204caa7b677cc868dc326d942689df28e30`, and the downloaded zip matched it.

For the symbol clash I built a throwaway module. Its package `naive` compiles `sqlite3.c` the ordinary way, as a `.c` file in the package directory with global symbols. Its test imports both `naive` and `github.com/mattn/go-sqlite3` v1.14.53.

On macOS arm64 (Apple clang 21), `go test` failed at link time with 271 errors of this form, ending:

```
duplicate symbol '_sqlite3_vsnprintf' in:
    /private/var/folders/.../go-link-2266657057/000015.o
    /private/var/folders/.../go-link-2266657057/000011.o
...
ld: 271 duplicate symbols
clang: error: linker command failed with exit code 1 (use -v to see invocation)
FAIL	clash [build failed]
```

In `golang:1.27.3` on linux/arm64 (GCC 14.2.0), the same test failed with 271 errors of this form:

```
/src/internal/sqlite/crosscheck/naive/sqlite3.c:30682: multiple definition of `sqlite3_mutex_free'; /tmp/go-link-4272427079/000011.o:/go/pkg/mod/github.com/mattn/go-sqlite3@v1.14.53/sqlite3-binding.c:30683: first defined here
```

The note in `/AGENTS.md` is therefore right: two ordinary copies cannot be linked into one binary. I chose file-local symbols. The amalgamation is `#include`d into the cgo preamble of `/internal/sqlite/sqlite.go` after `#define SQLITE_API static`. Everything not marked `SQLITE_API` is already `static` in the amalgamation (`SQLITE_PRIVATE`), so all of SQLite becomes local to that one translation unit, and cgo's generated wrappers call the static functions directly. With that change the same two-copy test linked and passed. System `nm` on the test binary showed every one of our `sqlite3*` symbols as local (533 `t`, plus `b`, `d` and `s`); only mattn's were global.

There are two guards. `TestSymbols` in `/internal/sqlite/sqlite_test.go` builds a test binary with `go test -c` and reads its symbol table with `debug/macho` or `debug/elf`. It fails on any global `sqlite3*` symbol, and also on fewer than 100 local ones, so it cannot pass vacuously. As a mutation check I removed the `#define`, and the test failed with "Expected slice of length 0, but got 276". The second guard is the cross-check module (Step 3), which links only if the symbols stay local.

### Why

Running mattn in a separate process would have needed a small RPC protocol and a second binary for the comparison, and that is more glue to trust. File-local symbols keep the comparison in-process, add no code, and keep every unsafe call in one file.

### What worked

The PRODUCT lines mean the update tool does not have to scrape HTML. Making `SQLITE_API` static needed nothing else. `SQLITE_EXTERN` is used only under `SQLITE_ENABLE_IOTRACE`, and `sqlite3_version[]` is defined only once in the amalgamation, because the second definition is under `#ifndef SQLITE_AMALGAMATION`.

mattn v1.14.53 bundles SQLite 3.54.1 with the source ID `2026-10-16 14:45:20 555e07ef33f69a0957d24d8e555b702464a131d6f1607f4c3c1e4551e8e6ca76`, the same as ours. Its `sqlite3-binding.c` and `sqlite3-binding.h` are byte-identical to our files apart from an `#ifndef USE_LIBSQLITE3` wrapper. I checked this with `diff`, and `TestSource` in the cross-check module now checks it on every run.

### What didn't work

The naive link failed, as recorded above. The session's sandbox also refused some compound shell commands in the scratchpad ("this command names git in a form too complex to verify"). I therefore ran the experiment inside the worktree, in what became `/internal/sqlite/crosscheck/`, and deleted the experiment files afterwards.

### What I learned

In Mach-O binaries, `go tool nm` prints `T` for local symbols as well as global ones, so it cannot tell them apart. System `nm` and `debug/macho` (through the `N_EXT` bit) can. With the amalgamation in the preamble and its symbols static, the compiler drops unreferenced code, so a tiny probe compiled in about a second.

### What was tricky

Because the symbols are static, only the Go file whose preamble includes the amalgamation can call SQLite. Any `//export` in that file would make cgo copy the preamble into `_cgo_export.c`, a second translation unit with a second private copy of SQLite. The preamble comment states both rules.

### What warrants review

The preamble in `/internal/sqlite/sqlite.go`, and whether Markus accepts in-process comparison with file-local symbols as the answer to the open note in `/AGENTS.md`. Two copies of SQLite in one process must never open the same database file, because POSIX locks belong to the process. The package doc now says so, and the cross-checks use `:memory:` only.

### Future work

The note in `/AGENTS.md` under "Notes for building the SQLite binding" still says the clash is not verified. It can now point to what is recorded here.

## Step 3: The minimal binding, compile options, and cross-checks

**Author:** binding-builder

### Prompt Context

**Verbatim prompt:** Markus, relayed by the lead in the middle of the build: "Builder should just build the minimal binding we need for now, we'll add more later as needed. But maybe it's simple because of our restricted use case."
**Interpretation:** The lead translated this to slice 1 of `/docs/roadmap.md`: open and close, prepare, bind and read integers, step, finalize, run a statement without results, and errors with the extended code and message, plus the version and compile-option report. No float, text, blob or null binding or access. Anything already built beyond that is removed.
**Inferred intent:** Less memory-unsafe surface. Everything else in the brief (vendoring, options, sanitizer, fuzzing, cross-checks, symbol clash, CI) stays, scaled to the smaller surface.

### What I did

When the scope change arrived, I had written tests for a wider API (text, blob and float binding and reads, reset, and `sqlite3_exec`). I rewrote them for the minimum before writing any implementation. The binding in `/internal/sqlite/sqlite.go` exposes:

- `Open(path)` (read-write, create, extended result codes on), `Conn.Close`, `Conn.Prepare` (exactly one statement), and `Conn.Exec` (Prepare, Step to the end, Finalize, written in Go)
- `Stmt.BindInt64`, `Stmt.Step`, `Stmt.ColumnCount`, `Stmt.ColumnName`, `Stmt.ColumnType`, `Stmt.ColumnInt64` and `Stmt.Finalize`
- `Error`, with `Code` (the extended `ResultCode`, with `Primary()`) and `Message`
- `Version`, `SourceID` and `CompileOptions`

I kept `ColumnType` and `ColumnName`, although they go beyond the strict list. Without `ColumnType`, an integer read cannot tell a real integer from NULL or from a converted value. `select *` results have column names that the oracle should compare. There is no reset, no `sqlite3_exec`, and no explicit transaction API. Slice 1 runs each statement in its own transaction, which SQLite does by default.

`Prepare` refuses SQL with anything but whitespace after the first statement, including comments and a NUL byte. It also refuses SQL without a statement. Without these checks, SQLite would silently run only part of the SQL. For the same reason, `Open` refuses a path with a NUL byte. Go ints that do not fit a C int are refused before they reach SQLite, where they would wrap to another index.

The compile options are in `/internal/sqlite/options.h`, each with its reason. The rule is that an option must implement a roadmap restriction, make misuse detectable or impossible, or be needed by the build. Options that only tune performance are left out. The options are `SQLITE_DQS=0`, `SQLITE_DEFAULT_FOREIGN_KEYS=1`, `SQLITE_THREADSAFE=1` (the default, stated explicitly), `SQLITE_ENABLE_API_ARMOR`, `SQLITE_OMIT_AUTORESET` (added in Step 5), `SQLITE_OMIT_SHARED_CACHE` and `SQLITE_OMIT_LOAD_EXTENSION`.

Cross-checks:

- **Against the shell,** in `/internal/sqlite/sqlite_test.go`. The tests build `sqlite3` once per test binary from `/internal/sqlite/c/shell.c` and `/internal/sqlite/c/sqlite3.c`, with `-include options.h` and the compiler from `go env CC`. They assert that the shell reports the same version and source ID as the binding and exactly the same `pragma compile_options` list. The list includes `COMPILER=`, so the same compiler is checked too. They also assert the same results for slice-1 statements and the same error messages. For results, the shell runs the query inside a common table expression that encodes each value in SQL (`typeof(c) || ':' || c`), so the comparison does not depend on how the shell formats its output.
- **Against mattn,** in the nested module `/internal/sqlite/crosscheck/` (module `maragu.dev/s4ql/internal/sqlite/crosscheck`, with `replace maragu.dev/s4ql => ../../..`):
  - `TestLink` shows that the two copies are distinct. On a new connection, `pragma foreign_keys` is 1 in ours and 0 in mattn's, and only ours reports `DQS=0`.
  - `TestVersion` compares the version and the source ID.
  - `TestSource` compares mattn's bundled C code with ours byte for byte.
  - `TestBindInt64`, `FuzzBindInt64`, `TestSlice1` and `FuzzSlice1` compare binding, typed reads, column names, extended codes and messages for slice-1 scripts. A subtest with fixed expectations stops the error comparison from passing vacuously.

### Why

For the scope: memory-unsafe surface is the cost, and slice 1 needs very little. For the cross-checks: any difference from a reference that runs the same C code is a bug in the glue. The nested module keeps mattn out of the main `/go.mod`. Go's `internal` rule still lets the nested module import `maragu.dev/s4ql/internal/sqlite`, because the rule is about import paths, not modules.

### What worked

The mutation check worked: when I built the shell without `-include options.h`, the compile-option test failed with "Expected slice of length 43, but got 38", and so did the foreign-key case. The cross-check module needed no workarounds to import the internal package.

### What didn't work

My first option list had `SQLITE_OMIT_DEPRECATED`, with the reason that it removes the deprecated flag PRAGMAs. A test showed that reason was wrong: `pragma count_changes` and `pragma full_column_names` still return a row. In the source, those PRAGMAs are guarded only by `SQLITE_OMIT_FLAG_PRAGMAS`. `OMIT_DEPRECATED` removes `PRAGMA default_cache_size` and the deprecated C functions. It also changes how the default cache size is chosen, because it ignores the size stored in the file header. That does not meet the rule, so I dropped the option.

A test that `create table ... default "abc"` fails under `SQLITE_DQS=0` failed. SQLite's grammar reads any identifier after `DEFAULT`, quoted or not, as a string (`ccons ::= DEFAULT scantok id`). The test now pins that behaviour, and `options.h` notes the exception. The engine's parser has to reject it itself.

`FuzzStmt_BindInt64` failed on its first native run with "Expected nil error, but got "sqlite: UNIQUE constraint failed: t1.v1 (code 1555)"". The fault was in the test, not the binding. As documented, `Finalize` after a failed step reports the step's error again, and the fuzz body expected nil. The fuzz bodies now open a new connection for each input (Step 5).

### What I learned

When `nByte` is given, `sqlite3_prepare_v3` copies the SQL into a NUL-terminated buffer and maps the tail back to the caller's buffer. The tail offset is therefore correct even for a Go string with an embedded NUL. The parser stops at the NUL, which is why `Prepare` reports a NUL as trailing SQL.

### What was tricky

`sqlite3_errmsg` is meaningful only for the most recent failed call. Step 5 records a case where the message did not belong to the failure.

### What warrants review

The option list and the rule behind it are for Markus to decide. These were left out on purpose:

- `SQLITE_LIKE_DOESNT_MATCH_BLOBS`: SQLite recommends it, but it changes the result of `LIKE` on blobs compared with a stock build.
- `SQLITE_ENABLE_MATH_FUNCTIONS`: the official CLI enables it. A later slice decides.
- `SQLITE_DEFAULT_WAL_SYNCHRONOUS`, `SQLITE_DEFAULT_MEMSTATUS=0`, `SQLITE_OMIT_DECLTYPE` and the other performance options.
- `SQLITE_TRUSTED_SCHEMA=0`.
- `SQLITE_OMIT_DEPRECATED`, for the reasons above.

Also review the decision to panic, and not return an error, on column misuse and on out of memory in `ColumnName`.

### Future work

Statement reuse is not cross-checked against mattn, because the binding has no reset. That comes back when a slice needs reset.

## Step 4: Address sanitizer, fuzzing, and CI

**Author:** binding-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Memory safety. The binding is memory-unsafe code that Go's race detector does not cover. Its tests must run under the C address sanitizer, with a documented command (Makefile target). If the sanitizer cannot run on this machine (macOS arm64), find the nearest working way (for example a Linux container or CI) and say plainly what was and was not run. Add a fuzz test of the binding and run it for a bounded time." and "CI. Look at `.github/workflows` and make sure the new tests run there, including cgo, and the sanitizer run if feasible."
**Interpretation:** A Makefile target for ASan that works from this Mac, bounded fuzz runs, and CI jobs for the parts that the reusable workflows do not cover.
**Inferred intent:** Evidence, and not only intent, that the glue is memory-safe.

### What I did

On this machine, `go test -asan` fails with "-asan is not supported on darwin/arm64". When not on Linux, `make test-asan` (`/Makefile`) therefore runs itself in `golang:1.27.3` with Docker. On Linux it runs `go test -asan` for both modules and then `make fuzz FUZZFLAGS=-asan`. `make fuzz` finds every `Fuzz*` test with `go test -list` and runs each one for `FUZZTIME`.

I checked that the container's toolchain really detects errors. A cgo program with `volatile char *p = malloc(4); p[4] = 1;`, run with `go run -asan`, printed "ERROR: AddressSanitizer: heap-buffer-overflow". A first probe without `volatile` printed nothing, because the compiler optimized the access away. Natively on macOS, `CGO_CFLAGS="-fsanitize=address" CGO_LDFLAGS="-fsanitize=address"` also instruments the C code and caught the same probe. In that mode, though, Go's heap is not poisoned, so the Linux run is the one that counts.

CI (`/.github/workflows/ci.yml`): the reusable `test` workflow already runs `go test -race ./...` at the root with cgo, which covers the binding and the shell cross-check. I added three jobs:

- `crosscheck`, for the nested module: tidy check, `go test -race` and golangci-lint.
- `asan`: runs `make test-asan FUZZTIME=30s` and prints any fuzz input that failed.
- `sqlite-verify` (Step 5).

All of these runs passed:

- Before the review, `make test-asan FUZZTIME=60s` in Docker ran all tests under `-asan` and then each fuzz target for 60s under `-asan`: FuzzConn_Prepare 222,894 execs, FuzzStmt_BindInt64 435,916, FuzzBindInt64 82,238 and FuzzSlice1 28,672.
- Natively without ASan, each target ran for 60s: FuzzStmt_BindInt64 8.6M execs, FuzzBindInt64 1.8M and FuzzSlice1 108,792. FuzzConn_Prepare also passed.
- After the review fixes came the final `make test-asan FUZZTIME=60s` run, in Step 5.

### Why

The glue's main risks are pointer and lifetime mistakes at the cgo boundary, and only a sanitizer sees those.

### What worked

Docker Desktop's Linux VM is arm64, so the container runs natively and fast enough. `go test -asan -fuzz` works for all four targets.

### What didn't work

`go test -asan` on darwin, and the probe without `volatile`, as recorded above.

### What I learned

Go's fuzzer measures coverage of Go code only. For the C code, fuzzing is close to random mutation of the seeds, so the seeds matter.

### What was tricky

The sanitizer cannot see inside SQLite's lookaside allocator. Step 5 fixes this.

### What warrants review

Whether the `sqlite-verify` CI job is acceptable. It downloads from sqlite.org on every run, so it fails whenever sqlite.org is down.

### Future work

None beyond Step 5.

## Step 5: Self-review and fixes

**Author:** binding-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "You have reviewed your own diff."
**Interpretation:** Run the code-review skill with two competing reviewers, fix what they find, and run the checks again.
**Inferred intent:** Find the bugs before the lead does.

### What I did

Before the review I found one problem myself. The `Step` doc said that a finished statement is not stepped again. Since 3.7.0, though, SQLite resets a finished statement automatically on the next step, so a stray extra `Step` would run an insert again. I added `SQLITE_OMIT_AUTORESET`, with tests. With it, such a step returns `SQLITE_MISUSE`, except after `SQLITE_BUSY` or `SQLITE_LOCKED`, where SQLite still restarts the statement.

Two reviewers then read the diff independently. Both found the following, and verified it by experiment:

- **Serious: the Go build cache did not track the amalgamation.** The go command hashes only the files in a package's own directory, and `/internal/sqlite/c/` is a subdirectory. A reviewer changed `SQLITE_VERSION` in a copy of `c/sqlite3.c`, and `go build` still produced a binary that reported 3.54.1. After `make sqlite-update`, the tests could therefore have run against the old SQLite. Fix: the update tool now also writes `/internal/sqlite/source.h`, with the version and the SHA-256 of each vendored file, and the preamble includes it. `TestVendored` checks it against the files and `source.json`, and `-verify` checks it too.
- **ASan could not see use after finalize,** because statements are allocated from each connection's lookaside slab. Fix: in a binary built with `-asan`, `Open` turns lookaside off with `SQLITE_DBCONFIG_LOOKASIDE`. Go sets the `asan` build tag for such builds, which `/internal/sqlite/asan.go` and `/internal/sqlite/noasan.go` use. Lookaside is a runtime setting, so the compile options and the shell comparison do not change, and production builds keep lookaside. I checked the fix with a temporary canary that prepares a statement, finalizes it, and then calls `sqlite3_column_count` on the freed statement. Under `-asan` the canary now fails with "ERROR: AddressSanitizer: heap-use-after-free on address 0x512000000888". Without the sanitizer it reads "column count read after finalize: 3" and nothing reports it. Before the fix, a reviewer saw the same 3, with no report, even under `-asan`. I removed the canary.
- **A `SQLITE_MISUSE` carried a stale message,** such as "no more rows available (code 21)", because SQLite does not set the connection's error for every failure. Fix: `errorFromConn` uses `sqlite3_errmsg` only when `sqlite3_extended_errcode` equals the returned code, and `sqlite3_errstr` otherwise. The tests now assert the messages.
- **Column reads without a current row, or out of range, returned a plausible NULL or 0,** which `sqlite3.h` calls undefined. Fix: `Stmt` tracks whether the last `Step` returned a row. `ColumnType` and `ColumnInt64` panic without a row, and all column methods panic for an index out of range or a finalized statement. SQLite is therefore never called in the undefined cases.
- **`-verify` trusted the manifest it was verifying.** Fix: it requires an `https://sqlite.org/` URL with the right file name for the version, compares with the PRODUCT line while the version is listed, and checks `source.h`. A `sqlite-verify` CI job runs it. The checks are split into tested functions (`checkURL`, `checkArchive`, `checkVendored` and `header`), and files are written to temporary names and then renamed.
- **The fuzz targets shared one connection,** and some PRAGMAs act when they are prepared. `pragma foreign_keys=off` turns enforcement off without a step, and `pragma hard_heap_limit=1` broke every later `Open` in the process. Fix: each input gets a new connection, and inputs that mention `heap_limit` or `directory` (settings for the whole process) are skipped. `Prepare`'s doc now says that some PRAGMAs take effect when prepared, even when the SQL after them is refused.

Smaller fixes:

- The shell now runs with `-noinit`, because a user's `sqliterc` could change its output.
- Shell errors are compared exactly after "near line N: ", and the message must not be empty.
- The options test parses `options.h`, so SQLite must report every option defined there.
- The `crosscheck` CI job now points `setup-go` at the nested module's `go.mod` and `go.sum`, because paths in `uses:` steps ignore `working-directory`.
- Docker runs mount module and build caches.
- `FuzzSlice1` draws values from a small set and has real seeds (the slice-1 example, the error script, and extreme values). Its old seeds only ever created one table.
- Comments that looked outward, naming the cache, the oracle, mattn or slice 1, are rewritten.
- Package-level values used in one place are inlined or moved into their test.
- `TestStmt_Column` is split into one test per method.
- `-lm` is linked on every system except Darwin.
- `Open("")`, which opens a temporary database, is documented and tested.

Not changed, and why:

- **`go mod vendor` drops `/internal/sqlite/c/`.** One reviewer found and verified this with a consumer module: a downstream module that vendors s4ql cannot build. A fix would move the amalgamation into the package directory under a name that cgo does not compile, or something similar. Nothing depends on s4ql yet, so I left it for a decision.
- **SQLite updates are gated on mattn's releases.** `TestVersion` and `TestSource` fail until mattn bundles the same version. That is the drift guard working as asked, but it ties our update schedule to mattn's, which is a product decision.
- **Result-code constants.** Those used once are inlined with a comment. The four that several tests use stay as constants.

After the fixes, `make test` and `make lint` passed, with "0 issues." in both modules. The result of the final `make test-asan FUZZTIME=60s` run is in the final report.

### Why

The review found real correctness problems in exactly the area the project worries about: an oracle build that could go stale without notice, and a sanitizer that could not see the main hazard.

### What worked

Asking two independent reviewers for verified findings. Each found the build-cache problem and the lookaside problem on its own, with experiments behind them, so neither was in doubt.

### What didn't work

While editing, I twice addressed the main checkout's `/.github/workflows/ci.yml` by mistake. The read returned the main checkout's unchanged file, and the worktree isolation refused the edit, so nothing outside the worktree changed. I redid both on the worktree copy.

### What I learned

Go hashes only a package directory's own files into a build's action ID, so any `#include` that reaches outside the directory needs a tracked stand-in inside it. SQLite's lookaside allocator hides lifetime bugs from ASan unless lookaside is turned off.

### What was tricky

The sanitizer had to get stricter without changing the compile options, which the shell comparison requires to be identical. A runtime `sqlite3_db_config` call made only under `-asan` does both.

### What warrants review

- `Open` in `/internal/sqlite/sqlite.go`: in `-asan` builds, lookaside is turned off right after opening, and a failure there fails `Open`.
- The new panics on column misuse.
- `/internal/sqlite/source.h` and its check in `TestVendored`.

### Future work

- Support for `go mod vendor`.
- A decision on what happens when mattn lags behind a SQLite release.
- A permanent ASan canary would need a C entry point in `/internal/sqlite/sqlite.go`, which is production code, so this diary records the check instead.
- `SQLITE_DEFAULT_PCACHE_INITSZ` makes the page cache allocate in bulk, which ASan cannot see either. The glue does not touch pages, so I left it.
