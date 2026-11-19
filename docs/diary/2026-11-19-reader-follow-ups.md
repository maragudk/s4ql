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
