# Diary: the follow-ups of the reader of each connection

Pull request #48 gave each connection of the `database/sql` pool a reader of its own (`core.DB.Reader`, `core.Reader`, `engine.Conn`). An independent review judged it sound and named five follow-ups, which this branch, `reader-follow-ups`, takes from `main` at `8294863`.

## Step 1: The brief

**Author:** reader-follow-ups builder

### Prompt Context

**Verbatim prompt:** (from the lead's brief, shortened to its points) "1. **A reader must not create the cache.** A reader opens the cache with `SQLITE_OPEN_CREATE` [...]. Fix: readers open without the create flag (only the connection that makes the cache may create it), so that a missing file is SQLite's `CANTOPEN`, which the core already treats as a local failure that stops the instance. [...] 2. **Say the truth about file descriptors.** SQLite's unix VFS keeps the descriptor of the main database file while another connection of the process holds a lock on it [...]. Correct the claims in the root package's documentation [...] and in `docs/todo.md`, and say it in the core's README. Verify the behaviour yourself first with a small experiment and put the numbers in the diary. Add no test that depends on `lsof`. 3. **`Reader.Close` closes the SQLite connection while it holds `DB.mu`** [...]. Move the close out of the mutex in the same way as the open [...]. Only if it stays simple [...]; if it does not stay simple, leave the code and record it in `docs/todo.md` as known and why. 4. **The core's README contradicts itself** about the minimums of `TestSimulation` [...]. Correct the text and add that measurement. 5. **Record in `docs/todo.md`**, under the driver: (a) a second query on one pinned `sql.Conn` whose rows are still open is refused as "not supported" [...]; (b) Open, for Markus: with the pool unbounded by default, running out of file descriptors stops the instance for good [...]. Do not build either."
**Interpretation:** Five small follow-ups: one change to the binding and the core (a reader opens without `SQLITE_OPEN_CREATE`), one possible change to the core (the close of a reader without `DB.mu`), and three corrections or additions to the documentation. Tests on the real core and cache with the in-memory store, nothing that depends on timing.
**Inferred intent:** Close the gaps that the review of #48 found while they are fresh, and leave the two design questions (the default bound of the pool, and waiting on a pinned connection) recorded for the lead and Markus instead of decided.

### What I did

Reset the worktree to `origin/main` at `8294863` and made the branch `reader-follow-ups`. Read `/AGENTS.md`, the last steps of `/docs/diary/2026-11-19-reader-per-conn.md`, `/internal/core/read.go`, the parts of `/internal/core/README.md` and of the root package's documentation that speak of readers, and the driver's points in `/docs/todo.md`.

Plan, one commit for each:

1. The binding gets `OpenExistingVFS(path, vfs)`, which is `OpenVFS` without `SQLITE_OPEN_CREATE`, in `/internal/sqlite/sqlite.go` and `/internal/sqlite/sqlite_browser.go`; the browser's VFS in `/internal/sqlite/browser/vfs.js` already refuses a missing file without the flag. `DB.Reader` opens through it. Tests in the core and at the root.
2. The truth about file descriptors, after an experiment with `lsof`.
3. The close of a reader without `DB.mu`, if it stays simple.
4. and 5. The README and the todo.

### Why

The brief asks for a diary step from the brief before code changes.

### What worked

Nothing to report yet.

### What didn't work

Nothing yet.

### What I learned

`/internal/sqlite/browser/vfs.js` already answers an open of a missing file without `OPEN_CREATE` with `CANTOPEN`, so the browser's binding needs only the flag.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: A reader opens the cache that is there, and never makes one

**Author:** reader-follow-ups builder

### Prompt Context

**Verbatim prompt:** (from the lead's brief) "1. **A reader must not create the cache.** A reader opens the cache with `SQLITE_OPEN_CREATE` (see the open in `internal/sqlite/sqlite.go` and how the core calls it). If the cache files are deleted under a running instance, a new reader creates an empty `cache.db`, and every query on it fails with `s4ql: sqlite: no such table: main.s4ql_cache (code 1)` while inserts and pings succeed, without a stop. Fix: readers open without the create flag (only the connection that makes the cache may create it), so that a missing file is SQLite's `CANTOPEN`, which the core already treats as a local failure that stops the instance. Keep the binding's API small and in its present style, and do the same in the second implementation of the binding behind the build tag `s4ql_browser` if it has the same notion (look at how `make check-browser` builds it). Test: delete the cache files under a serving instance, open a new reader (through `database/sql` at the root, and in the core), and show the stop with its cause, that no empty cache file is left behind, and that a normal start afterwards rebuilds from the store as the spec says a start with a missing cache does (check what the spec and the core say about that; do not invent behaviour)."
**Interpretation:** One new open in both bindings, without `SQLITE_OPEN_CREATE`, which `DB.Reader` uses, and tests in the binding, the core, and the root package.
**Inferred intent:** A cache that disappears under a running instance stops it loudly, as every other local failure does, and a restart recovers.

### What I did

`/internal/sqlite/sqlite.go` has `OpenExistingVFS(path, vfs)`, which is `OpenVFS` without `SQLITE_OPEN_CREATE`; both call a new unexported `open` with the flags of `sqlite3_open_v2`. `/internal/sqlite/sqlite_browser.go` has the same, with the three flags as named constants in place of the one constant of the flags. The browser's VFS in `/internal/sqlite/browser/vfs.js` already returns `CANTOPEN` for a missing file without `OPEN_CREATE`, so nothing changed there.

In the core, `openConn` in `/internal/core/cache.go` takes the open as its first argument: `Open` passes `sqlite.OpenVFS` for the write connection, which makes a new cache, and `DB.Reader` passes `sqlite.OpenExistingVFS`. The doc comment of `DB.Reader` names a file that is gone among the local failures, and `/internal/core/README.md` has a bullet, "Only the write connection makes the cache", under the local I/O failures.

Tests:

- `TestOpenExistingVFS` in `/internal/sqlite/sqlite_test.go`: it opens a file that exists through `""` and `"unix"`, fails with code 14 and makes no file for a path without one, fails for a file that was deleted while another connection holds it open in a read transaction, and refuses a NUL byte. The same test, shorter, in `/internal/sqlite/sqlite_browser_test.go`.
- `TestDB_Reader` in `/internal/core/read_test.go`, "should stop the instance for good when the files of the cache are deleted under it, ...": a reader that is open reads, the three files are removed, the next `DB.Reader` fails with `ErrStopped` and `ErrLocal` and the text `open a reader: sqlite: unable to open database file (code 14)`, the open reader's next `Read` fails with `ErrLocal`, after `DB.Close` the directory is empty, and a new instance on the same path serves the row from the store.
- `TestStop` in `/conn_test.go`, "should stop the instance when the files of the cache are deleted under it, ...": through `database/sql`, with a held `sql.Conn` so that the next query takes a new connection. The query fails with exactly `s4ql: the instance has stopped: local I/O failure: open a reader: sqlite: unable to open database file (code 14)`, an insert, a query on the held connection, and a ping get the stop, the cache directory is empty after `db.Close()`, and a new instance gives the rows 1 and 2.

### Why

Only the write connection may make the cache, because only it sets the cache up (`conn.setUp`). A reader that makes a file makes a cache without `s4ql_cache`.

### What worked

With `DB.Reader` switched back to `sqlite.OpenVFS`, both new tests fail as the review described, which shows that they test the bug:

```
conn_test.go:1116: Expected "s4ql: the instance has stopped" (type *errors.errorString), but got "s4ql: sqlite: no such table: main.s4ql_cache (code 1)" (type *s4ql.failure)
read_test.go:312: Expected "the instance has stopped" (type *errors.errorString), but got "<nil>" (type <nil>)
```

Chrome is installed on this machine, so `make test-browser BROWSERPKGS=./internal/sqlite BROWSERFLAGS='-run TestOpenExistingVFS -v'` and the same for `TestDB_Reader` in `./internal/core` ran in headless Chromium, and pass.

The closed instance's write connection, which still had the deleted file open, closed without an error and left nothing in the directory, so the old instance and the new one do not meet as long as the old one is closed first, which a normal start is.

### What didn't work

The first version of the core test removed `a.db-shm` unconditionally, and failed in the browser:

```
read_test.go:307: Expected nil error, but got "remove /tmp/TestDB_Readershould_stop_the_instance_for_good_when_the_files_o4214972800/001/a.db-shm: No such file or directory" (type *fs.PathError)
```

The browser's VFS keeps the wal-index in memory and makes no `-shm` file. The test now accepts a missing `-shm`.

The first version also required `d.Readers()` to be 0 after the stop, and got 1: the helper `read` keeps its reader open until the test ends. The test now opens its reader itself and shows that it is refused after the stop.

### What I learned

A test that opens a reader through `read` or `startRead` holds that reader until the cleanup, so `DB.Readers` counts it.

### What was tricky

What a start does with a missing cache. The spec says "If the cache cannot be opened, the start fails, and the cache is not rebuilt automatically", and also "the cache can be rebuilt from it at any time". A missing file is no cache that cannot be opened: `Open` makes a new one, as for the first start, and catch-up replays the log from the empty position. That is what `TestOpen` already shows for a cache of another name, and what the new tests show for the same path.

### What warrants review

`openConn` with the open as an argument, in `/internal/core/cache.go`, and the two calls of it.

### Future work

None from this step.

## Step 3: The truth about the file descriptors of the readers

**Author:** reader-follow-ups builder

### Prompt Context

**Verbatim prompt:** (from the lead's brief) "2. **Say the truth about file descriptors.** SQLite's unix VFS keeps the descriptor of the main database file while another connection of the process holds a lock on it, which the write connection always does, and reuses it for the next reader. So when the pool closes a connection, the reader's SQLite connection is closed and its descriptor of the write-ahead log is given back, but the descriptor of `cache.db` stays until the instance closes: descriptors stay at the high-water mark of readers that were open at once. Measured by the reviewer with `lsof`: 50 queries open gave 51 and 51; after the pool had closed every connection, 51 and 1; after `db.Close()`, 0 and 0. Correct the claims in the root package's documentation (near \"closes it when database/sql closes the connection\") and in `docs/todo.md`, and say it in the core's README. Verify the behaviour yourself first with a small experiment and put the numbers in the diary. Add no test that depends on `lsof`."
**Interpretation:** Reproduce the reviewer's numbers, then correct three documents. No test.
**Inferred intent:** Nobody reads in the documentation that closing connections of the pool gives every descriptor back, when it does not.

### What I did

A throwaway test in the root package, `zz_fd_experiment_test.go`, deleted after the run and never committed, opened an instance through `w.serve`, created `t1` with one row, and counted the lines of `lsof -p <pid>` that end in `cache.db` and in `cache.db-wal` at each stage (`go test -count=1 -run TestZZFDExperiment -v .`):

```
serving, after one write: cache.db 1, cache.db-wal 1
50 queries open: cache.db 51, cache.db-wal 51
rows closed, pool keeps idle: cache.db 51, cache.db-wal 3
pool closed every connection: cache.db 51, cache.db-wal 1
20 queries open again: cache.db 51, cache.db-wal 21
pool closed every connection again: cache.db 51, cache.db-wal 1
after db.Close: cache.db 0, cache.db-wal 0
```

"Rows closed" is with the pool's default of two idle connections, so three logs: the write connection's and two readers'. "Pool closed every connection" is after `db.SetMaxIdleConns(0)`. The fifth line is new against the reviewer's numbers: 20 new readers after the close of the 50 opened no new descriptor of the cache, so SQLite reuses the ones it kept, and the count stays at the high-water mark and does not grow.

The root package's documentation in `/s4ql.go`, the driver's point of the reader of each connection in `/docs/todo.md`, and a new bullet in `/internal/core/README.md` after the one on one read transaction at a time, now say this with the numbers. The todo also says that `faultvfs.VFS.OpenFiles`, which `TestReaders` counts, counts the files that SQLite opens through the VFS and not the descriptors of the process, so the tests' "closed" is true at that level.

### Why

The documentation said that the pool's settings decide how long a reader's files are kept, which is true of the log and not of the cache.

### What worked

The experiment gave the reviewer's numbers exactly.

### What didn't work

The first build of the experiment failed, because the root package's tests have a helper `exec`:

```
./s4ql_test.go:128:6: exec already declared through import of package exec ("os/exec")
```

I imported `os/exec` under another name.

### What I learned

The descriptors are reused, so the cost of a burst of readers is paid once, not again at each burst.

### What was tricky

Nothing.

### What warrants review

The new paragraph of the package documentation in `/s4ql.go`, as `go doc .` shows it.

### Future work

None from this step. Whether to bound the pool by default is point 5 (b).

## Step 4: `Reader.Close` closes its connection without `DB.mu`

**Author:** reader-follow-ups builder

### Prompt Context

**Verbatim prompt:** (from the lead's brief) "3. **`Reader.Close` closes the SQLite connection while it holds `DB.mu`** (`internal/core/read.go`). The open was moved out of the mutex so that a slow disk holds nothing up; the close was not. Move the close out of the mutex in the same way as the open (the builder counted opening readers in `DB.opening` so that `DB.Close` waits for them; closing readers need the same care, so that `DB.Close` and a stop never return while a reader's connection is still closing, and never close it twice). Only if it stays simple and you can show with the present life-cycle tests under `-race`, plus one new test for the new waiting, that nothing broke; if it does not stay simple, leave the code and record it in `docs/todo.md` as known and why."
**Interpretation:** Mirror the count of `DB.opening` for closes, with a yield for a test that closes the instance in the middle.
**Inferred intent:** A slow close of a file does not hold up every call and the lease task.

### What I did

`Reader.Close` in `/internal/core/read.go` now, after it has ended the read transaction, marks the connection closed under `DB.mu` and counts itself in a new field, `DB.closingReaders`, if the connection was not closed already; it then yields at "core: a reader before its close" and closes the connection without the mutex, and finally, under the mutex, takes it off `DB.readers`, lowers the count, and wakes the waits if the instance is closing and the count is 0. `DB.waitClosed` in `/internal/core/core.go` waits for `closingReaders` as it waits for `opening`.

Never twice: the mark comes before the close, under the mutex, and `DB.closeIdleLocked` skips a connection that is marked. Never early: `DB.Close` waits for the count. The connection stays on the list until it is closed, so that a crash of the simulation that ends the task at the yield leaves the connection where `DB.CloseCache` closes it, which is why `DB.Reader` does not yield between its open and the list either. A stop alone closes no connection, so only `DB.Close` needed the wait.

The new test, `TestReader_Close` "should make the close of the instance wait for a reader that is closing its connection, and close that connection once", is the twin of the one for the open in `TestDB_Reader`: in a bubble of `testing/synctest`, with `faultvfs` counting files, the hook at the new yield starts `DB.Close`, sleeps a simulated minute, and finds the instance stopped, two files open (the reader's cache and log; the write connection is closed), and no answer from the close. After the reader's close, no file is open, and the close returns with no simulated time passed, so the reader woke it.

`/internal/core/README.md`: the bullet on the life cycle of a reader, the list of yields of the simulation, and the list of the tests of the life cycle say it.

### Why

The open was moved out of the mutex on the same branch for the same reason; the close was left.

### What worked

Two mutants by hand, each reverted after: without `|| d.closingReaders > 0` in `DB.waitClosed`, the new test fails with `read_test.go:406: Expected "0", but got "1"` (the close returned while the reader was closing) and `faultvfs: 2 files of s4ql-fault-0x10317e080 are open` at the end; without the wake, it fails with `close: a call still uses the cache, which is closed when the call ends: context deadline exceeded`.

The life-cycle tests (`TestDB_Reader`, `TestReader_*`, `TestReadTx_*`, `TestDB_Close*`) pass 50 times with `-shuffle on`, with and without `-race`. The whole of `/internal/core`, with `TestSimulation`, its traces compared, its minimums, and the planted bugs, passes with the new yield, and so do `go test -race -shuffle on -run '^TestFree' ./internal/sim/ ./internal/core/`, and the root package and the engine under `-race`.

### What didn't work

The first version of the test expected three open files before the close, and got four: the write connection has its write-ahead log open too.

### What I learned

With the new yield, every minimum of `TestSimulation` still holds, unlike after the yield of `DB.Reader` on the branch before; the callers of the simulation close their readers only when a node has opened its database again or when they end, so the yield is rare there. I did not count how far the counts moved.

### What was tricky

Where the yield may stand. Before the mark, a close of the instance at the yield closes the connection itself, and the test would show nothing new; between the list and the close, a crash would leave a connection that nothing knows of. Keeping the connection on the list until after its close resolves both.

### What warrants review

`Reader.Close` in `/internal/core/read.go`, against `DB.closeIdleLocked`, `DB.waitClosed`, and `DB.closeCache` in `/internal/core/core.go`.

### Future work

`DB.closeIdleLocked` still closes connections under `DB.mu`, in `DB.Close` and in `DB.unuse` while the instance is closing. That is the close of the instance, where nothing else is left to hold up but the lease task's end, and the brief named only `Reader.Close`.

## Step 5: The minimums of the simulation, and two points for the todo

**Author:** reader-follow-ups builder

### Prompt Context

**Verbatim prompt:** (from the lead's brief) "4. **The core's README contradicts itself** about the minimums of `TestSimulation`: one place says the change \"left the minimums ... passing as they were\", and a later paragraph says the minimums of one workload were derived again (30 seeds; \"reads refused by a clock\" is 4 where it was 5 over 10 seeds). The second is true. The review measured why, over 300 seeds: at the commit before the change that workload reached 129 such reads, which is 4.3 for each 10 seeds, below the old minimum of 5, so the old ten seeds were lucky. Correct the text and add that measurement. 5. **Record in `docs/todo.md`**, under the driver: (a) a second query on one pinned `sql.Conn` whose rows are still open is refused as \"not supported\" [...]; to look at again with transactions [...]; the review measured that a query right after cancelling the context of the one before it, without `Rows.Close`, was refused 1928 of 2000 times. (b) Open, for Markus: with the pool unbounded by default, running out of file descriptors stops the instance for good [...]; the choices are a default bound on the pool in `Open`, or failing only the statement when a reader cannot be opened for want of descriptors (which needs `sqlite3_system_errno` in the binding to tell `EMFILE` from a missing cache). Do not build either."
**Interpretation:** Two edits of documentation, with the reviewer's numbers as they were given.
**Inferred intent:** The README says one true thing about the minimums, and the two questions are where the lead and Markus look for open points.

### What I did

In `/internal/core/README.md`, the paragraph on the harness now says that every planted bug was still found and that the minimums held but for those of the workload whose callers never pause, which were derived again, and points to the paragraph on its seeds. That paragraph has the review's measurement over 300 seeds, and says that each new minimum is at least the old one except that of the reads refused by a clock, 4 over 30 seeds where it was 5 over 10.

In `/docs/todo.md`, under the driver, after the open point on rows that are neither read nor closed: a point to look at again with transactions, on the refusal of a second query on a pinned connection, with the 1928 of 2000; and an open point for Markus on running out of file descriptors, with the reproduction at a limit of 64 and the two choices. The second says that a cache that is gone must still stop the instance, which is why the binding would need `sqlite3_system_errno`: since step 2 both give `CANTOPEN`.

### Why

The brief asked for both, and for nothing to be built for point 5.

### What worked

The numbers are the reviewer's, as the brief gives them; I did not measure them again.

### What didn't work

Nothing.

### What I learned

Since step 2 a missing cache and a process without descriptors give a reader the same `CANTOPEN`, which makes choice (b) of point 5 depend on telling them apart.

### What was tricky

Nothing.

### What warrants review

The two new points of `/docs/todo.md`, which are for the lead and Markus.

### Future work

Point 5 (b) is Markus's decision.

## Step 6: The close of a reader goes back under `DB.mu`

**Author:** reader-follow-ups builder

### Prompt Context

**Verbatim prompt:** (from the lead's brief) "Only if it stays simple and you can show with the present life-cycle tests under `-race`, plus one new test for the new waiting, that nothing broke; if it does not stay simple, leave the code and record it in `docs/todo.md` as known and why."
**Interpretation:** The checks of the whole branch broke on step 4, so the condition of the brief decides.
**Inferred intent:** No change to the life cycle that makes the cache less tidy or the tests flaky.

### What I did

The checks of the whole branch found two failures, both from step 4.

`make test` failed in the simulator, in the run with the tag `s4ql_browser`:

```
--- FAIL: TestRun_faults (0.97s)
    --- FAIL: TestRun_faults/should_show_writes_held_back_after_a_crash (0.01s)
        run_test.go:409: Not true
        run_test.go:409: an event request/held
```

The case runs seed 5 alone, and the new yield moved the schedule. Over seeds 1 to 40, with a throwaway test, the event came in 24 seeds with the yield and in 19 without it, so it is a coin toss for each seed and seed 5 landed on the other side. I gave that case seed 7, which has the event either way, before the second failure made it moot.

The run under the address sanitizer of the packages that this branch touches, in Docker (`go test -asan -shuffle on ./internal/sqlite/ ./internal/core/ .` in `golang:1.26`), failed in the root package:

```
--- FAIL: TestClose (0.25s)
    --- FAIL: TestClose/should_refuse_a_query_that_comes_while_the_close_closes_the_rows_that_are_open,_before_the_instance_has_stopped (0.01s)
        s4ql_test.go:65: a connection to the cache /tmp/TestCloseshould_refuse_a_query_that_comes_while_the_close_close2838741886/001/a/cache.db-wal is still open
```

Natively, `go test -count=300 -run 'TestClose/should_refuse_a_query_that_comes_while' .` found the log left 21 times, and 3 of 100 under `-race`; with `read.go` and `core.go` of the commit before step 4, 0 of 300. In that test `database/sql` closes its idle connections, and with them their readers, while the connector closes the instance. With the close of a reader out of the mutex, it runs at the same time as the closes of `DB.closeIdleLocked`. SQLite deletes the write-ahead log at the close of the last connection only after it locks the database file exclusively, and two closes at once can each find the other's shared lock, so neither deletes it. Before, every close of a connection was under `DB.mu`, one at a time, so the last one always could.

A log that is left loses nothing: the next start recovers from it. But it makes the tests that take a log that is left for a connection that is left flaky, and a fix would need a lock of its own for the closes, which `DB.closeIdleLocked` would wait for while it holds `DB.mu`. That is not simple, so I took back the code of step 4: `read.go`, `core.go`, and `read_test.go` of `/internal/core` are as before it, the seed of the simulator's case is 5 again, and the three sentences of step 4 in `/internal/core/README.md` are gone. The comment in `Reader.Close` now gives the second reason for the close under the mutex, and the README's bullet on the life cycle of a reader points to a new point in `/docs/todo.md`, under the storage core, which records the close under the mutex as known, with what was tried and the measurements.

After it: 0 of 300 runs of that test leave a log, and `TestRun_faults` passes on seed 5.

### Why

The brief made the change conditional on its staying simple and breaking nothing.

### What worked

Running the sanitizer on the touched packages, which the brief asked for because of the binding, is what found it; the native `make test` passed that test on its one run.

### What didn't work

Step 4, as above.

### What I learned

The lifecycle tests and the simulation cannot see two closes at once: in the simulation one task runs at a time, and the life-cycle tests close the readers before the instance. Only the root package's tests, where `database/sql` closes from goroutines of its own, put two closes side by side.

### What was tricky

That a close under the mutex was doing a second job, which nothing said.

### What warrants review

The new point in `/docs/todo.md` under the storage core, and the comment in `Reader.Close`.

### Future work

The point in the todo, if a slow close ever matters.

## Step 7: The review of the branch, and the checks

**Author:** reader-follow-ups builder

### Prompt Context

**Verbatim prompt:** (from the lead's brief) "Checks before you report: your new and changed tests with `-count=50 -shuffle on` and with `-race`; `make test`; `make lint`; `make check-browser`. Because point 1 changes the binding, also run the sanitizer for the packages you touched only [...]. Review your own diff once more before you report."
**Interpretation:** The checks on the final state of the branch, and a review by the code-review skill of `main...reader-follow-ups`.
**Inferred intent:** The lead gets a branch whose weak spots are named.

### What I did

The code-review skill, at medium, on `main...reader-follow-ups`, found three things and nothing that blocks:

- The new comment in `Reader.Close` said that no two connections to the cache close at once, and `DB.Reader` closes its new connection without `DB.mu` when `pragma query_only = on` fails, which is code from before this branch. That close cannot get in the way: a connection that has not read the cache holds no lock on it, and neither `busy_timeout` nor `query_only` reads the schema (in `/internal/sqlite/c/sqlite3.c` both have `PragFlg_Result0` and no `PragFlg_NeedSchema`). The comment now says "no two connections that have read the cache", and why the close after a failed open does not count. No code changed.
- The README's bullet on who makes the cache said that a deletion stops the instance, which is so only at the next open of a reader. It now says that until then the write connection and the open readers go on with the deleted files, and that an instance whose readers are all open may never stop for it.
- The package documentation left out the descriptor of the write connection. It now says "plus the one of the connection for writes", which the 51 for 50 queries shows.

The checks on the branch before these three edits of comments and documentation, which `go vet`, `golangci-lint` of the two packages, and the tests of the reader then ran again:

- `TestOpenExistingVFS` in `/internal/sqlite`, `TestDB_Reader` and `TestReader_Close` in `/internal/core`, and `TestStop` and `TestClose` in the root package: 50 times with `-shuffle on`, and 50 times with `-race -shuffle on`. All pass.
- `make test`, `make lint`, and `make check-browser` pass.
- `go test -asan -shuffle on ./internal/sqlite/ ./internal/core/ .` in Docker, `golang:1.26`, passes.
- In headless Chromium, `TestOpenExistingVFS` of `/internal/sqlite` and `TestDB_Reader` of `/internal/core`, through `make test-browser` with `BROWSERPKGS` and `-run`, pass.

### Why

The brief asked for them.

### What worked

The sanitizer's run found the fault of step 4.

### What didn't work

Nothing in this step.

### What I learned

The flags of SQLite's pragma table say whether a pragma reads the schema.

### What was tricky

Nothing.

### What warrants review

Nothing beyond the earlier steps.

### Future work

The two open points of `/docs/todo.md` for the lead and Markus (the bound of the pool, and waiting on a pinned connection), and the known point of the close of a reader under `DB.mu`.
