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
