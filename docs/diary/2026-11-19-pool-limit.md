# Diary: A default limit on the pool from the process's limit of file descriptors

Since pull request #48 each connection of the pool of `database/sql` owns a reader, one SQLite connection to the cache, and the pool has no limit by default. A burst of queries can use up the process's file descriptors, and a failure to open a reader stops the instance for good. Markus agreed that `s4ql.Open` sets a default limit on the pool, derived from the process's limit of file descriptors: the readers may use at most half of it. The work is on the branch `pool-limit`, which starts from `main` at `8294863`.

## Step 1: The brief, and a plan

**Author:** pool-limit builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Since pull request #48 each connection of the `database/sql` pool owns a reader, which is one SQLite connection to the cache. The pool is unbounded by default. A burst of queries can therefore use up the process's file descriptors, and a failure to open a reader is a local failure that stops the instance for good. [...] `s4ql.Open` sets a default limit on the pool, derived from the process's limit of file descriptors and not a number picked by hand: the readers may use at most half of it. An application can still change it with `SetMaxOpenConns`, as with any `*sql.DB`. [...] The default is the soft limit of open files of the process (`RLIMIT_NOFILE`), divided by four, since a reader costs up to two descriptors and the readers get half; at least 1. Read it at `Open`. If the limit is unlimited or cannot be read, or on a platform without it, set no limit [...] Put the arithmetic in one small pure function so that it can be tested with a table, and keep platform differences behind build constraints in the usual Go way [...] `Open` calls `SetMaxOpenConns` with it. `Register` with `sql.Open` by name cannot [...] document at `Register` that the application sets `SetMaxOpenConns` itself, and why it should. [...] Log the chosen limit in the driver's `open` event [...] Do not change what a failed open of a reader does (it still stops the instance), and add no public option."
**Interpretation:** A pure function turns the soft limit of open files into a bound of the pool; a function behind build constraints reads that limit on the platforms that have one. `Open` sets the bound on the `*sql.DB` that it makes, the open event logs it, and the documentation at `Open` and `Register` says what it is and why.
**Inferred intent:** An application that does nothing gets a pool that cannot use up the descriptors of its process through readers, without a number that nobody measured.

### What I did

Reset the worktree to `origin/main` at `8294863` and created the branch `pool-limit`. Read `/AGENTS.md`, `/s4ql.go`, `/conn.go`, `/export_test.go`, the helpers and `TestOpen`, `TestOptions_Logger`, `TestRegister`, and `TestOpen_s3` of `/s4ql_test.go`, the start of `/internal/core/read.go`, the driver's points in `/docs/todo.md`, the rules of `/internal/simlint/simlint.go`, and the `Makefile`.

Probed the limit with a small program in the scratchpad, which calls `syscall.Getrlimit(syscall.RLIMIT_NOFILE, ...)`: on this Mac it prints a soft limit of 61440 and a hard limit of 9223372036854775807, while `ulimit -Sn` in the shell says 1048576. Then vetted the program with `CGO_ENABLED=0` for twenty `GOOS/GOARCH` pairs of `unix`.

The plan:

1. `maxOpenConns(limit uint64, limited bool) int` in `/s4ql.go`, with a table test: `limit/4`, at least 1, at most `math.MaxInt`, and 0 for no limit, which is what `SetMaxOpenConns` and `DBStats.MaxOpenConnections` mean by 0.
2. `fileLimit() (uint64, bool)` in `/filelimit_unix.go` (`//go:build unix`) through `syscall.Getrlimit`, and in `/filelimit_other.go` (`//go:build !unix`) giving no limit.
3. `Open` and the `OpenStore` of the tests share one function that makes the `*sql.DB` and sets the bound; the connector learns the bound so that the event `open` logs it as `pool.max_open_conns`, 0 for `Register`.
4. The documentation at `Open`, `Register`, and the package's section on statements, and the point in `/docs/todo.md`.

### Why

The bound has to be set on the `*sql.DB`, which only `Open` holds, and the connector logs the event `open`, so the bound goes into the connector when it is made.

### What worked

The probe showed the arithmetic's inputs on this machine before any code: the process sees 61440, so the default here is 15360.

### What didn't work

The first form of the check for an unlimited limit, `uint64(r.Cur) == uint64(syscall.RLIM_INFINITY)`, does not compile on Linux, Android, Solaris, or illumos, where `RLIM_INFINITY` is an untyped negative constant:

```
linux/amd64: vet: ./main.go:13:57: constant -1 overflows uint64
solaris/amd64: vet: ./main.go:13:57: constant -3 overflows uint64
```

It compiled on Darwin, the BSDs, and AIX, where the constant is positive.

### What I learned

Go raises the soft limit of open files to the hard limit when a process starts, on Darwin capped at `kern.maxfilesperproc`, so the soft limit that the process reads is not the one that the shell shows: 61440 here against 1048576 in the shell. On Linux the soft limit a Go process sees is therefore its hard limit, unless the program lowers it again. `RLIM_INFINITY` is `-1` on Linux, `-3` on Solaris and illumos, and `1<<63-1` on Darwin and FreeBSD, and `syscall.Rlimit.Cur` is `uint64` on most platforms and `int64` on FreeBSD.

### What was tricky

The constant: it has to pass through a variable of a signed type before it becomes a `uint64`, because a conversion of a negative constant to `uint64` is a compile error.

### What warrants review

That the build constraints cover every platform the root package builds for: `unix` and `!unix` are complementary, so every platform gets exactly one `fileLimit`.

### Future work

None yet.

## Step 2: The bound, its tests, and the self-review

**Author:** pool-limit builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Through `database/sql`: after `Open`, `db.Stats().MaxOpenConnections` is what the function gives for the limit that the test process has at that moment; an application's `SetMaxOpenConns` afterwards wins [...] Do not lower the descriptor limit of the test process itself, because other tests in the same process would be hit [...] Checks before you report: your new and changed tests with `-count=50 -shuffle on` and with `-race`; `make test`; `make lint`; `make check-browser`."
**Interpretation:** Build the plan of step 1 test first, run the checks of the brief, and review the diff with the code-review skill.
**Inferred intent:** The bound is shown through the public API, on the real core and cache, without a test that depends on the machine's limit being low.

### What I did

Wrote the tests first in `/s4ql_test.go` and `/filelimit_unix_test.go`, with `MaxOpenConns` and `FileLimit` exported in `/export_test.go`, and saw them fail to build. Then wrote `maxOpenConns` and `opener.openDB` in `/s4ql.go`, which `Open` and the tests' `OpenStore` share, `fileLimit` in `/filelimit_unix.go` and `/filelimit_other.go`, and the field `pool.max_open_conns` of the event `open` in `/conn.go`. `opener.connect`, `Options.connector`, and `newConnector` take the bound, which `OpenConnector` passes as 0. The documentation of the package, `Open`, `Register`, `OpenConnector`, and `connector` says what the bound is. Then the point in `/docs/todo.md`; this tree had no open point on file descriptors, so it is a new point.

The tests: `TestMaxOpenConns` is the table (no limit, 0 to 8, 11, 1024, 61440, 1048576, and `math.MaxUint64`); `TestFileLimit` compares `fileLimit` with `syscall.Getrlimit`; a subtest of `TestOpen` checks `Stats().MaxOpenConnections` after `OpenStore` and that `SetMaxOpenConns(4)` and `SetMaxOpenConns(0)` win, with queries after each; `TestOpen_s3` checks the bound of the real `Open` and that `Register` has none; a subtest of `TestRegister` checks the pool without a bound and `pool.max_open_conns=0` in its log; `TestOptions_Logger` has the new field in the line of `open`. A further statement that waits for a connection until its context ends is already in `TestRows`, so it is not repeated.

Vetted copies of the two platform files for 27 platforms, and ran the probe of step 1 in `golang:1.27` containers.

Checks: the changed tests with `-count=50 -shuffle on`, and with `-race -count=50 -shuffle on` while `make test` ran beside them, both passed; `make test`, `make lint`, and `make check-browser` passed.

The code-review skill found six points. I fixed three: the documentation said that the readers use at most half of the descriptors, which holds only for one instance in a process, so it now says that the bound is for each instance; it now says that a later change of the limit is not seen; and `TestFileLimit` required a finite limit, so it now accepts an unlimited one too, with no bound. I left three: an unlimited limit on a BSD whose kernel caps the descriptors of a process gets no bound, as the brief asks; a small limit gives a small bound that writes wait behind, as the brief's "at least 1" gives; and a temporary file of SQLite would be a third descriptor of a reader, which I did not check. All three are in `/docs/todo.md`.

### Why

`Open` is the one place that holds the `*sql.DB`, and the connector logs the event `open`, so the bound is computed in `openDB` and handed to the connector.

### What worked

Sharing `openDB` between `Open` and `OpenStore` makes every test that opens through `OpenStore` run with the default bound, so the whole suite runs as an application would. `TestOpen_s3` covers the real `Open`, which the other tests do not reach.

### What didn't work

The first form of `rlimInfinity`, `int64(syscall.RLIM_INFINITY)`, passed for 26 platforms and failed on `linux/loong64`, where the constant is `1<<64-1`:

```
linux/loong64: vet: ./filelimit_unix.go:18:20: constant 18446744073709551615 overflows int64
```

`uint64(syscall.RLIM_INFINITY & math.MaxUint64)` compiles for all of them: constant arithmetic on untyped integers is exact, and the mask gives the two's complement of a negative constant.

### What I learned

In a Linux container with a soft limit of 1024 and a hard limit of 65536, Go raised the soft limit to 65535 and not to 65536, so the number that the process sees is not always the hard limit. With Docker's default of 1048576 it is 1048576. The root package does not build with `CGO_ENABLED=0` on any platform, because `internal/core` needs the cgo binding, so the platform files were vetted as copies in a package of their own.

### What was tricky

That the log field had to be on the connector while the bound is set on the `*sql.DB`: `Register` goes through `OpenConnector`, which never sees a `*sql.DB`, so it logs 0, which is what `DBStats.MaxOpenConnections` calls no bound.

### What warrants review

The point "for the lead to confirm" in `/docs/todo.md`: the bound is for each instance, and two instances in one process can use every descriptor. A shared budget would be a second mechanism. Also the wording of the documentation of `Open` in `/s4ql.go`.

### Future work

If an application opens several instances in one process, a budget that they share; and a check whether a read of the engine can make SQLite open a temporary file.
