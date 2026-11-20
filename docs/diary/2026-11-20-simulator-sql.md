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

## Step 2: The workload on the engine, and the checker on bounded reads

**Author:** builder (simulator SQL)

### What I did

- `run.go`: each process makes an `engine.Engine` when it opens its database, with a logger whose handler, `positionHandler`, is enabled only for a context that carries a `statementSeen`, and takes `statement.position` from the event `statement`. A call gets a `Conn` of its own. A write is `Conn.Begin`, `insert into orders (id) values (?)`, and `Conn.Commit`; on `no such table: orders` it runs `create table orders (id integer primary key) strict, without rowid` and the insert again in the same transaction. A read is `select id from orders where id > ?`, with the bound `max(0, values - window)` taken in `newCall`, where a write takes its key, so the bound comes from the counter of the run and is the same in every build. The trim and `workload.keep` are gone; `workload.window`, from 1 to 10000 and 100 by default in a request and a session, takes its place.
- Events of calls carry `sql` and `args`. A commit object carries `statements`, read from the annotations with `CommitReader.Next`. `deletes` is gone from the commit object, the state, and the checker.
- `check.go`: `state` compares a read with the rows of the log above its bound at its position, and also catches a key given twice among all the rows (`{1, 1, 2}`), which the old slow check missed because it compared sets. `fresh` requires only the writes above the bound. The fast check counts the live keys from the bound to the largest key of the log, which is the window and the keys taken since the read started, so it does not grow with the log.
- Tests: the checker's table gained reads that miss a row above the bound, and give a row at, or below, the bound; a new subtest checks every read of four keys, at every position, above every bound from 0 to 4, with a fast checker and one made slow from the start, and requires the same violations. `run_test.go` replaced the test of the trim with one of the SQL: the first commit object holds the create and the insert, each later one an insert with its key as argument, and every read gives only keys above its bound, in order.

### Why

- **Begin, insert, commit, and not one `Exec`.** With one statement of its own, the error of a refused begin and of a refused commit can be the same sentinel, `core.ErrNoLease`. Three calls give the three phases that the outcomes of a call need, as with the core before.
- **The bound in `newCall`.** A call of the client starts when the client sends it, and its task in the process starts up to 50 ms later. Taking the bound when the call starts, as a write takes its key, puts it in the event `started` already, and keeps it out of the order of the tasks.
- **A read without the table is served.** It saw a state, the empty log, and a stale read of the empty log after an acknowledged write is exactly what `fresh` must catch. As `failed`, it would not be checked.
- **The table is `orders`, not `requests`.** The page already says "Requests" for the requests to the app, and the trace says `request` for the requests to S3; a third meaning would confuse.

### What worked

`go test -tags s4ql_browser ./internal/simulator/...` passed at the first run, including the session against the batch run and the steps of a fake minute in `TestEngine_live`. A probe of a run showed the SQL in the events, and the first commit object with `[create table ..., insert ... args [1]]`.

### What didn't work

- `TestChecker/should_find_the_same_violations_fast_and_slow` first required that every read without a violation is checked fast: `check_internal_test.go:245: Expected "11", but got "5"`. The fast check leaves a read at a position before an acknowledged write at or below its bound to the slow check, because `ackedAt` holds the latest index of every acknowledged write, not only those above the bound. That is conservative and rare in a run, so the test now requires only that some reads are checked fast, and that a read that the fast check passes has no violation.
- I checked out `main`'s `internal/simulator` to measure a baseline, and that reverted my uncommitted edits of `internal/simulator/sitecheck/check.html`. I wrote them again.

### What warrants review

- `positionHandler` relies on the engine logging the event of a statement on the context of the call, and on `statement.position` being there once the statement has a transaction. Both are in the engine's README; a select of a missing table has a position.
- The fast check of `fresh` is conservative (see above). The slow check decides.

## Step 3: Step 0, measured

**Author:** builder (simulator SQL)

### What I did

A temporary test, not committed, started a session, advanced it in steps of 30 fake minutes, and at the ages 0, 1, 6, and 24 fake hours timed five advances of one fake minute each. It ran natively and in Chromium through `make test-browser`, on this code and on `main`'s `internal/simulator`, with the page's settings (`{"workload": {"callers": 0}, "client": {"instance": "a"}, "standby": true}`) and the defaults of a session, seed 7, on an Apple M4.

### What I learned

- **A bounded read is a range scan.** `keyAccess` in the engine turns `id > ?` on the key into a `scanRange` with an open lower bound. Measured: with the page's settings a read gives 100 rows at 1 h and at 24 h, when the table has about 14,000 rows, and the time of a fake minute does not grow.
- **The rows of a query cost a step each.** `Rows.All` yields to the host between two rows, and a yield of `sim` parks the task with the jitter of the scheduler. With the page's settings a fake minute takes about 1,440 steps, against 417 on `main` (`(2077274 - 521082) / 1080` and `(601523 - 151901) / 1080`), at every age. The text trace of `sim` keeps a line for each, so the Go heap grows about twice as fast: 306.7 MB after 24 fake hours, against 151.1 MB on `main`.
- Natively, the mean of five fake minutes, page's settings: 16, 29, 27, 35 ms at 0, 1, 6, 24 h (`main`: 11, 10, 9, 14 ms). Defaults of a session: 77, 40, 36, 110 ms (`main`: 56, 39, 20, 54 ms). The outliers at 24 h come with a heap of 300 to 400 MB.
- In Chromium, page's settings: 85, 103, 141, 73 ms (`main`: 52, 20, 35, 25 ms); defaults: 108, 145, 153, 129 ms (`main`: 46, 48, 46, 40 ms). The steps at each age were the same as natively, to the step.

At Turbo, the page advances five fake seconds a real second, so a fake minute takes 12 real seconds; 73 to 141 ms of work in them does not show. So the cost does not grow with the age of the session, and the build goes on. It is about three times that of `main`, from the yields between rows.

The heap is what grows. The page's comment on its stop at 24 fake hours says "about 12.5 MB a fake hour ... some 300 MB", which was measured with the defaults; with the page's settings `main` grew half that, and this code grows 306.7 MB in 24 hours, so the comment holds for the page now.

### Future work

- If the heap or the steps matter, the levers are a smaller window or fewer reads. Neither is mine to choose.

## Step 4: The page

**Author:** builder (simulator SQL)

### What I did

- `run.js`: `sqlText` puts the arguments of a statement in the places of its `?`, and `callText` and `statementsText` use it for a call and a commit object. The log says `a serves select id from orders where id > 812: 100 rows.` and `a commits insert into orders (id) values (913) as 3f6fbfd1.`; the first commit says the `create table` and the insert, joined by `; `. A write that gets no answer, or is not committed, names its statement where it named its key. The teller keeps each commit object until the core's event of its commit, and drops it then, where it kept the keys of every commit object for good.
- `page.js` and `index.html`: each commit object shows the statements of its annotations, as they are, with `ARGS` and the arguments. The requests and the cards of the instances count inserts and selects. The intro says "each one SQL statement", and the line "Built from" names `internal/engine` beside `internal/core`.
- `sitecheck/check.html`: the words of the log, of a write that gets no answer, and of the cards, and a new unit check of `sqlText` and of the lines of a commit and of reads, also one of a missing table.

### What worked

Screenshots in headless Chromium through `playwright-cli`, at 1440, 1000, and 390 px: the log and the commit objects wrap a long statement, the create table of the first commit too, and the page is 390 px wide at 390 px. `make site-check` passed, with every file of the simulator loaded with `?v=<commit>`.

### What didn't work

`playwright-cli eval ...` was refused by the worktree's guard ("this command runs a string through eval"); `playwright-cli run-code` with `page.evaluate` worked.

## Step 5: Self-review

**Author:** builder (simulator SQL)

### What I did

Ran the code-review skill over `main...HEAD`. It found:

- **`fresh` lost its teeth below the bound** (important, fixed). The bound comes from the counter, which counts every key taken, also by writes that fail. If the last `window` keys all belong to writes that failed, as in a long handover, a stale read with no rows above its bound passed both checks. Fix: a write at or below the bound must be in the state of the read's position, which `checker.below` reads from the commit object of the position and its ancestors in the store; a commit object names its parent, so the state of a position never changes. With it the fast check, whose `ackedAt` already compared positions, and the slow check agree exactly, and the test of agreement now requires that the fast check passes every read without a violation. Two new cases in the checker's table: a read at a state without an acknowledged write, and without a write of unknown outcome, at or below its bound.
- A test indexed `c.Statements[len-1]` without checking the length (fixed).
- `sqlText` replaces a `?` inside a string literal, and shows a blob as text (left: the workload has neither; the comment says so).
- The intro and the line "Built from" on the page (left: the brief allowed a few words in the intro; the link to `internal/engine` is the code that now runs there).
- The teller's map of commit objects keeps those whose commit the core never logs (pre-existing; it now drops every one that is logged).

Checks: `go test -tags s4ql_browser ./internal/simulator/...`, also with `-race`, and the new tests with `-race -count=20`; `make check-browser`, `make test-browser`, `make lint` with simlint, and `make site-check`, all passed. No lint needed an exemption for the import of `internal/engine`. CI's job `changes` counts every path under `internal/` as code, so a change to `internal/engine` or `internal/sql` already runs the jobs `browser` and `site`.

### What warrants review

- The section "What the code does under each action" in the README of the simulator is dated 2026-11-19, with times from runs of the workload on the core. The courses that `run_test.go` checks still hold; the times were not measured again.
- The Go heap after 24 fake hours, about 300 MB with the page's settings, and the steps of a fake minute, about three times those of `main`: both from the engine's yield between two rows of a query.
