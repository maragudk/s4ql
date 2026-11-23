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

## Step 2: The engine side

**Author:** builder (simulator SQL box)

### What I did

- `request.go`: the action `sql`, with the field `sql`, at most 1000 characters (`maxSQL`) of valid UTF-8, for no instance. A `sql` on another action is an error.
- `box.go` (new): `run.sql` starts the call in a task of the run, which goes to the client's target through the client's path (`callTarget`, split out of `clientCall`), so `down` and `lost` hold for it as for a request of the app. `namesApp` reads the text with `stmt.Check` and refuses every statement but a select whose table, or index name, folds to `barcodes`. `boxStatement` runs a select (and a text that the pass refuses) as `Conn.Query`, and every other statement as `Begin`, `Exec`, `Commit`. The event that ends the call has `columns`, the first 50 `rows` as typed cells, `count`, `affected`, and `commit`.
- `run.go`: op `sql`, caller `box`; `coreEvent` gives a write of the box the name of its commit object from the core's event `commit` on its context, unless the outcome is `empty`.
- `live.go`: `advance` takes `until: "sql"`, and `untilBox` waits on the box's task with the timeout of the advance, so a paused page can step exactly to the answer. The JS module's `advance(ms, until)` sends it.
- `check.go`: `txn` has a third identity, `commit`; every commit object is the write of its name (`writesOf`), and a write of the box is known by it alone. `indexOf` finds it by its position. Selects of the box go through `boxRead`: `fresh`, and the position part of `state`. New: `checker.writes` holds every write of the workload that a call started, or the stored log held; at the end, an insert or delete of `barcodes` in the log that no call made is a violation of `not_committed`.

### Why

- **The refusal by the statement pass.** `stmt.Check` exposes the table of each statement kind and the index names; nothing outside `internal/simulator` changed. A text the pass does not take cannot change a table, since the engine refuses it before reading the database, so it goes to the engine and the visitor gets the engine's own "not supported".
- **Which invariants, and the new half of `not_committed`.** Without it the checker did not notice a foreign delete of a row of `barcodes`: a read after it gives no row, and the log at its position has none, so `state` agrees. Knowing every write the app made closes that, and is what "still catches a change to barcodes that the app did not make" needs. I put it under `not_committed`, next to the write of a client that found its instance `down`, which was never made either.
- **A box write by its commit name.** The core's commit event and the engine's statement event are logged on one context, as the engine's README says, so the name is there without changing the engine. App writes keep their key identity, which also catches a key in the log twice.
- **`until` in the engine, not small steps in the page.** "No further" means stopping at the end of the call; the controller waiting on the box's task does exactly that. An advance with `until` is not part of the session-equals-batch comparison.
- **The box's timeout is the workload's (30 s).** A select yields between rows, about 12.5 ms of fake time a row (measured: 300 rows in about 3.8 s), so only a select of some 2,400 rows would time out; the cap of 2,000 commit objects keeps tables below that.

### What didn't work

- The first box test expected a delete of no row, and a `create table if not exists` of an existing table, to have no commit object, and the checker reported `the commit 3dbf4fe4 is acknowledged, and the log holds it 0 times`. The core logs `commit.name` also for the outcome `empty` (the commit is spooled, never written). `coreEvent` now ignores the name for `empty`. The checker caught this itself, which is what it is for.
- `select nothing from t` gives `near "nothing": syntax error` in SQLite (`nothing` is a keyword); the test uses `missing`.
- `create table if not exists barcodes (k integer primary key)` without `strict, without rowid` is not taken by the statement pass, so the simulator does not refuse it; the engine does ("not supported"). The test uses a supported form.
- The worktree's guard refused shell heredocs that hold backticks or `${}`; edit scripts went through files in the scratch area instead.

## Step 3: The page

**Author:** builder (simulator SQL box)

### What I did

- `index.html`: a section `#sql` in the grid of the log: on wide screens at the top of the right column, above the commit objects, with the log running down the left as tall as both; below 960 px it comes first, then the log and the commit objects. A line with the link to `../#roadmap` that names `barcodes`, a textarea with the example `select code from barcodes order by code desc limit 5`, Run, the hint `Ctrl+Enter` (`⌘ Enter` on a Mac), a status line (inverted on an error, as the requests' status is), and a table of at most 50 rows. The intro says "Send one of your own" with a link to `#sql`. Only the page's tokens, `.btn`, `.caps`, `.log-head`, and rules; NULL is in small capitals at opacity 0.55, text in quotes, a blob as `x'…'`, a real always with a decimal point; long cells cut by CSS with an ellipsis and a title, and the engine's own cut ends in `…`.
- `page.js`: `runSQL` queues the action and, while paused, a step `{until: true}` that advances with `until: "sql"`; `boxStep` skips it and shows "Waiting: the connection of a to S3 hangs. Play to let time move." (or the clocks standing still) instead. Run is disabled while a statement is pending; Reset clears it. The box's text is kept in `sessionStorage` under `s4ql:box`, which a reset of the database keeps. The box's calls are not counted in the requests strip or the cards.
- Storage: a refused write (`keep` returns `full` or `failed`) now halts the page with "S3 is full. The browser's storage for this tab takes no more, at N commit objects. Reset to start over." where it went on in memory before.
- `run.js`: lines for the box ("You send … to a.", "a serves …: N rows.", errors in the engine's words, "nothing to commit"); a commit is the core's line as for the app. Statements in the log are cut at 200 characters (`boxText`), also in the core's commit line. A commit object of the box shows its number of changes.
- `sound.js`: a tick `box`, E6 then A6, 18 and 22 ms, level 0.44, the select's; an error tone for a failed or refused statement. The Run action blips as every action does.
- Site check: unit checks of the lines and ticks, the box's peak against the select's, and `sqlBox` on the paused page (the example, a table of its own, three inserts by Run and Ctrl+Enter, `order by` and `limit` with each type, an empty result, an engine error, the refusal, the length limit, the wait under a hang until Play); after a reload, the text and the table are back; a refused storage write stops the page.

### Why

- **The place.** At 1280 by 800 the diagram already runs past the first screen and the log starts at about 1075 px, so anything above the log or the diagram pushes them down. Beside the log, where the visitor's statements and their commits show, it pushes nothing; the link in the intro points to it.
- **Run while paused stops stepping on a hang or paused clocks** because the call could only end by its timeout there, which is the "waits, visibly, until the visitor lets time move" of the brief. Other faults end the call by themselves (a disconnect fails fast and retries to the timeout), so the step goes on to the answer.
- **One theme.** The page has only its dark theme (and the inverted head of the instance that serves); there is no light theme to check.

### What didn't work

- Port 8093 was taken by another process, and my first page load hit it; the site was served on 8197 instead.
- The check compared `data-t`, which is floored, with the end of the call; it reads `s4qlPage.t` now.
- Run stayed enabled on the stopped page, because `halt` did not draw the box; it does now.
- `navigator.userAgentData.platform` is `macOS`, so the Mac hint needed a case-insensitive match.

### How to review

`go test -tags s4ql_browser ./internal/simulator/...`, then `make site SITEVERSION= && go run -tags s4ql_browser ./internal/simulator/sitecheck -root site`. By hand: `make site-serve`, press Play for a few seconds, and run statements in the box; hang `a` while paused and run an insert to see it wait.

## Step 4: Self-review

**Author:** builder (simulator SQL box)

### What I did

Ran the code-review skill over `main...simulator-sql-box`. It found five things:

1. **A statement still on its way when the page pauses stayed pending for good** (fixed). The step until the answer was queued only by a Run on a paused page. Pause now queues it too, and the site check sends a statement while playing and pauses at once.
2. **The event of the action could name another instance than the call went to** (fixed). The task read the client's target again when it ran, so a switch of the client at the same fake time moved the call. `callTarget` now takes the instance, which `run.sql` reads with the action. A test switches the requests right after the action.
3. **The box's text was stored at any length** (fixed). A pasted text of megabytes could fill `sessionStorage`, and as Reset keeps `s4ql:box`, the page would stop as S3 full again after it. Only a text within the limit of 1,000 characters is kept now.
4. **A write of the box whose process crashes before the core logs its commit** ends `lost` without a name, so the checker learns nothing of it, also if the commit object lands later (left). The README lists it among what is not checked; a crashed process logs nothing more, so the name cannot be had without changing the core.
5. **An empty statement** (checked, left): the engine answers `s4ql: the text holds no statement`, its own words, which a test now holds.

Checks: `go test -tags s4ql_browser ./internal/simulator/...`, also with `-race`; `TestEngine_box` and `TestChecker` with `-count=50` and with `-race -count=20`; `make test-browser BROWSERPKGS=./internal/simulator`; `golangci-lint run --build-tags s4ql_browser ./internal/simulator/...`, also with `GOOS=js GOARCH=wasm`; simlint over the simulator with the tag; `make site-check`. All passed.

A writer sub-agent updated `internal/simulator/README.md` from a brief: the action `sql`, the section "The box", the fields of the call's event and its cells, `until`, and the invariants for the box. It noted that an empty select has `rows` as `[]` and no `count`, and a `lost` write without a name, both now in the README.

### Future work

- A select of more than about 2,400 rows would reach the timeout of 30 s; the cap keeps tables below that today.
- The site has one theme, the dark one, so there was no other theme to check.
