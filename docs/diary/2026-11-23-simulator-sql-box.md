# Diary: a box for SQL on the simulator page

The simulator on the site, `internal/simulator` served by `site/simulator`, runs an app whose requests are SQL statements on `barcodes`. Markus: "I'd also like someone looking at the simulator to try out the storage engine. Maybe a small SQL box and a results table below it?" It was agreed that the box is part of the running simulation, not a separate engine. This diary is of the builder that adds the box.

## Step 1: The brief

**Author:** builder (simulator SQL box)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> 1. A box for one SQL statement with a Run button, and below it the result: a table of rows for a select, the number of rows affected for a write, or the engine's error in the engine's own words.
> 2. The statement is a request in the same session, sent to the instance that the app's requests go to. It shows in the log like the app's requests, a write is committed as its own commit object, and it fails or waits under the faults the visitor has switched on, with the same outcomes kept apart as for the app's writes: refused, abandoned, outcome unknown, not committed.
> 3. The visitor may create, insert into, update, delete from, index, and drop tables of their own. `barcodes` is read-only for the box: `select` on it is allowed, and every other statement that names it is refused by the simulator before it reaches the engine, with a short plain message. Find the table from what `internal/engine` or its statement pass exposes. No Go code outside `internal/simulator` changes.
> 4. While paused, Run steps fake time just far enough for the answer to arrive, and no further. If S3 hangs, the statement waits, visibly, until the visitor lets time move. While playing it simply goes in. One statement at a time; a reset ends a pending one.
> 5. The visitor's tables persist with the bucket across a refresh, and their commits count toward the cap of 2,000. When `sessionStorage` refuses a write, the page ends in the existing "S3 is full … Reset to start over." stop. Cap the statement's length and say so when it is exceeded.
> 6. The checker accepts the visitor's commits: its six invariants keep holding for the app's workload, with no false violation from a visitor's commit, and it still catches a change to `barcodes` that the app did not make. Decide which invariants apply to the box, and write it in the README. Fast and slow paths agree, and the exhaustive test covers logs with visitor commits.
> 7. Results: at most the first 50 rows, with the total when there are more ("50 of 1,204 rows"); null, text, integer, real, and blob told apart; long values cut with an ellipsis.
> 8. One line near the box says what SQL works, links to `/#roadmap`, and names `barcodes`. A working example in the box at first load; remembering the box's text across a refresh is a nice touch.
> 9. Sound: the box's statements sound, with a short sound of their own, square waves, no louder than a select tick or insert beep; the mix limits hold.
> 10. Design: the page's tokens, type, and controls, nothing new. Findable without pushing the diagram and log off the first screen. Checked with playwright at 1280 and 390 px, with an empty result, an error, a long table, and a pending statement. Cmd/Ctrl+Enter runs; labels and visible focus.
>
> Tests: `go test -tags s4ql_browser ./internal/simulator/...` and the site check pass, and the site check runs a select on `barcodes`, creates a table, inserts, selects with `order by` and `limit`, gets an engine error, gets the refusal for `barcodes`, runs a statement while paused, and reloads and finds the visitor's table. No timing in tests, `-count=50` and `-race` on new tests. No migration of the stored shape; no planted bugs; `site/index.html` unchanged; no `make test-asan`.

**Interpretation:** a new action of the engine, `sql`, that sends one statement of the visitor to the client's target as a call of its own, the checker extended to commits of the box, an advance that can stop when the box's call ends, and on the page a section with a text box, a Run button, and a result table.

**Inferred intent:** a visitor can poke the real engine and see that their statements go through the same storage, under the same faults, as the app's.

### What I did

Read `AGENTS.md`, the README of `internal/simulator`, the three diaries of the SQL, the bucket, and the deletes, `run.go`, `client.go`, `live.go`, `check.go`, `bucket.go`, `request.go`, `trace.go`, the page's `page.js`, `run.js`, `bucket.js`, `sound.js`, `index.html`, and the site check, and `go doc` of `internal/engine` and `internal/sql/stmt`.

### Why

What the reading gave:

- `stmt.Check` of `internal/sql/stmt` gives the typed statement, with `Table` on every kind but `DropIndex`, and `CreateIndex.Index` and `DropIndex.Index` for the index; `Name.Fold` compares names as SQLite does. That is enough to find whether a statement names `barcodes`, before the engine sees it, without changing the engine.
- `readCommit` already keeps only the keys of `barcodes` in `CommitObject.Keys` and `Deletes`, so a commit of another table carries no write of the app, and the bucket's checks on reload hold as they are.
- The engine's README says that the event of a write statement and the core's event of its commit are logged on one context. So a write of the box can learn the name of its commit object from the core's event `commit` on its own context, as a read learns its position from the engine's event. The checker can then know a box's write by the name of its commit object.
- The page's storage drops the stored database and goes on in memory when a write fails; the brief wants the "S3 is full" stop instead.
