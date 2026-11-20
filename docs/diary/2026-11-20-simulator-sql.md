# Diary: SQL in the simulator

The simulator on the site, `internal/simulator` served by `site/simulator`, runs the real `internal/core` in the browser, and its workload called the core directly, with `tx.Upsert` and `tx.Scan`. s4ql now has an SQL layer, `internal/engine` on `internal/sql/...`. Markus: "Should we introduce some SQL in the simulator, now that we support some? So it's not just the core storage engine." This diary is of the builder that moves the workload onto the engine.

## Step 1: The brief

**Author:** builder (simulator SQL)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> - A write is an SQL insert and a read is an SQL select, both run through `internal/engine`, the code that the `database/sql` driver runs on.
> - No trim. The table grows for the whole session. A read is bounded by its `where`: it selects only the keys above a bound, about 100 below the newest key.
> - The page shows the SQL.
> - `delete` is slice 8 of the roadmap and is not built. Do not build anything for it now.
>
> Standing constraints: "No changes to the Go codebase otherwise." Only `internal/simulator/**`, `site/simulator/**`, and where needed the `Makefile`, the workflows, `internal/simlint`, `.golangci.yml`, `.impeccable/config.json`. "I don't want two slightly different simulators": one workload path, so the direct calls on the core and the trim (`workload.keep` with its deletes) go away. No planted bugs. A session has no time limit apart from the page's stop (24 fake hours or 1 GiB of SQLite memory), and the cost of a fake minute must not grow with the age of the session.
>
> Step 0, by measurement, before building: (1) is a bounded read cheap at any age: range scan or full scan with a filter, and the wall time of a fake minute at about 0, 1, 6 and 24 fake hours, natively and in Chromium; stop and report if it grows so that the page would slow down visibly. (2) Can the simulator still get the position of each read, and tell a write's outcome apart (committed, refused, failed, unknown), through `engine.Conn` without changing the engine; stop and report if not.
>
> Then: an `engine.Engine` for each process; the workload, the client, and the visitor's `write` and `read` through `engine.Conn`; one insert per write and one select per read; a bound that is the same natively and in the browser; events with the SQL and its arguments, and for a commit object its annotation; the checker's `state` and `fresh` on the rows above the bound, with the fast path kept and in agreement with the slow one, and tests with constructed traces; the same events and hash natively and in Chromium, and for a session and a batch run; `internal/simulator/README.md` updated. On the page: the SQL in the log, in the "Requests" strip and in the detail of a commit object where it helps, without changing the design. CI's path filters must run the `browser` and `site` jobs for a change to `internal/engine` or `internal/sql`.

**Interpretation:** replace the workload's calls on the core with SQL through the engine, without touching the engine, and keep every property of the simulator that held before: determinism, a session equal to a batch run, a checker with a fast path, and a cost per fake minute that does not grow.

**Inferred intent:** a visitor sees an application's SQL, not calls of a storage API, and the page still shows only what the code does.

### What I did

Read `AGENTS.md`, the README and code of `internal/simulator`, the README of `internal/engine`, and the page.

### Why

The plan, from the reading:

- **Range scan.** The README of the engine says that `scan` reads by the key where the conditions allow it: `k > ?` on the primary key is a `scanRange` with an open lower bound, which the cache runs as a `where` on the key. So a bounded read should cost the same at any age; Step 0 measures it.
- **The rows of a query yield.** `Rows.All` calls `host.Yield` between two rows, and the yield of an instance of `sim` parks the task with the jitter of the scheduler, up to 50 ms half of the time. A read of 100 rows is then about 100 more steps and about a second of fake time. Step 0 measures what that costs.
- **Position.** The engine logs the event `statement` with `statement.position`, on the context of the call, to `Options.Logger`. A handler of the simulator can take it from the context of the call.
- **Outcome.** The engine passes every error of the core through unchanged, so `errors.Is` finds `core.ErrNoLease`, `core.ErrStopped`, `core.ErrBusy`, and `core.ErrOutcomeUnknown`. With `Conn.Begin`, `Conn.Exec`, and `Conn.Commit` the three phases of a write are three calls, as they were with the core.

### What worked

Nothing to run yet.

### What didn't work

Nothing yet.

### What I learned

Nothing yet.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.
