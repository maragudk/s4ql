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
