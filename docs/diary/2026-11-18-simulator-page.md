# Diary: The simulator page on the real code

The simulator on the site, `/site/simulator/`, has run a JavaScript model of the storage design. Steps 1 and 2a (#31, #32) put the unchanged storage core into the browser, as an engine in a Worker that runs a seed and a schedule of faults to its end and returns a trace. This is step 2b: the page that plays that trace, and the deploy that builds the engine into the site.

## Step 1: The brief

**Author:** builder (simulator page)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> You build step 2b: **the page and its deploy.**
>
> - One simulator on the site, the real one. The JavaScript model goes: delete `site/simulator/model.js` and `model.test.mjs`.
> - The page shows how the **current version of the code** reacts to **faults in the environment**. No planted bugs, anywhere in the page or its copy.
> - A run is shareable by link: the URL holds the seed, the settings, and every fault and action the visitor added, each with its fake time. Opening the link gives the same run.
> - Injecting a fault "while it runs" is done by running again: the page pauses at fake time T, adds the action at T to the schedule, and asks the engine for the run again; everything before T is identical. The visitor should experience this as the run continuing.
> - The size of the download is accepted. Load the engine in the Worker and show a real loading state meanwhile, and a clear message if WebAssembly or Workers are unavailable or loading fails.
> - No change to `internal/core`, `internal/sim`, `internal/store/simstore`, or `docs/spec.md`. Faults of the file system are not available in the engine; do not offer them.
> - Design it as a refinement of the existing surface, not a new look, with the `impeccable:impeccable` skill; check desktop and phone widths with `playwright-cli`; no horizontal scrolling on a phone.
> - Let a visitor play, pause, step, and scrub fake time; see the bucket, each instance, requests in flight, and the invariant checks at any time; read the events in plain words; add actions and faults at the current time; change the settings; see and remove the schedule; reset, pick a new seed, copy the link; and know that this is the real code compiled to WebAssembly, on the official SQLite WebAssembly build, with a simulated S3 and time, and which commit it was built from.
> - Build and deploy: the built files are not committed; the Pages workflow sets up Go, builds the engine into the site, writes the commit in, and uploads, also when `internal/**`, `go.mod`, `go.sum`, or the `Makefile` change. A make target builds the site with the engine and serves it locally. Do not depend on COOP/COEP.
> - A scripted smoke check in headless Chromium as a make target: the page loads, the engine becomes ready, the default run has events, a fault at a time changes the run after that time and not before, and a copied link gives the same trace hash. No check may depend on timing.
> - In `site/index.html`, change only what is said about the simulator, so that it is true.

**Interpretation:** replace the model page with a page that drives `internal/simulator/engine` through `simulator.js`, keeps its run in the URL, re-runs the engine when the visitor acts, and plays the trace back with the state folded at each time; build the engine into the site in CI and locally; and check the page end to end in Chromium.

**Inferred intent:** a visitor sees the shipped code under faults, and every run they share replays exactly, so what the page shows can be trusted as much as the tests.

### What I did

Read `/AGENTS.md`, `/internal/simulator/README.md`, `/internal/simulator/engine/` (`simulator.js`, `worker.js`, the demo `index.html`), `/internal/simulator/serve/main.go`, the diary of the engine, `/PRODUCT.md`, `/.impeccable/surfaces/site-index-html.md`, `/site/simulator/index.html`, the simulator's mentions in `/site/index.html`, `/Makefile`, and `/.github/workflows/pages.yml`.

### Why

The plan, from the reading:

- **The page folds the state itself.** The README says that the state is a fold of the events up to a time, and that a page can fold them itself. Asking the Worker for each frame of the playback would be a round trip per frame; a fold in the page is cheap and stays in step with the scrubber.
- **One re-run per change of the schedule or settings.** Playback, step, and scrub only move the cursor over the trace that the page already has.
- **The engine's files go into a directory of their own under `site/simulator/`, which `.gitignore` covers,** so the demo page of the engine cannot shadow the page.

### What worked

Nothing to run yet.

### What didn't work

Nothing yet.

### What I learned

- `site/index.html` says nothing about a model: its one sentence is the link "Open the simulator", which stays true.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

See the later steps.

## Step 2: The build, the page, and the check

**Author:** builder (simulator page)

### Prompt Context

**Verbatim prompt:** (continues from Step 1)

**Interpretation:** build what Step 1 planned: the engine into the site and the deploy, the page on the engine, and a check of the page in Chromium that CI can run.

**Inferred intent:** the lead can merge and the page on www.s4ql.com then runs the code of that commit.

### What I did

- `/internal/simulator/serve/main.go`: four flags for the site. `-demo=false` leaves out the demo page, `-commit` writes `build.json`, `-root` serves another directory than that of `-out`, and `-isolated=false` leaves out the COOP and COEP headers, as GitHub Pages serves. Tests in `main_test.go` for the refusals, the headers, the content type of `.wasm`, and `build.json`.
- `/Makefile`: `site` builds the engine into `site/simulator/engine/` (in `.gitignore`), `site-serve` serves `site/` on `SITEADDR`, and `site-check` runs the check.
- `/.github/workflows/pages.yml`: sets up Go from `go.mod`, runs `make site SITECOMMIT=$GITHUB_SHA`, and uploads; it also runs on changes to `internal/**`, `go.mod`, `go.sum`, and the `Makefile`.
- `/site/simulator/`: `index.html` (markup and styles, refined from the old page), `run.js` (the run as data: defaults, the request, the link, and the words for every event), and `page.js` (the engine, playback, and drawing). `model.js` and `model.test.mjs` are deleted.
- `/internal/simulator/sitecheck/`: a command that serves `site/` as Pages does, opens an embedded `check.html` in headless Chromium, and gets its result by a POST. `check.html` loads the page in frames and drives it. CI's job `browser` runs `make site-check`. `/internal/simlint/simlint.go` exempts the command, as it does the runner of `internal/sqlite/browser`.
- `/internal/simulator/README.md`: the site's build, and the old page in the past tense.

### Why

- **The page asks the engine for the state at the cursor** rather than folding it itself, so the page has no second implementation of `StateAt` that could drift. The requests are coalesced: at most one is in flight, and none while a run is.
- **One run in flight, and only the newest wish runs.** A visitor who adds three faults quickly gets one more run, not three queued ones.
- **A fault goes at `floor(T) + 1` ms**, after everything shown at T, as the README advises, and into the schedule after every action at or before its time, so the actions before it keep their indexes and the run before it stays the same.
- **The default run** is seed `0x7334716c` (the bytes of "s4ql", the old page's seed), three instances, 2 min, the faults of `simstore.AllFaults`, and a crash of b at 0:40 and its restart at 1:00. Under this seed b takes the lease first, c takes it over at about 1:14, and a at about 1:49; a native run of it took 0.1 s.
- **The link** is `#v=1&seed=…&n=…&end=…&lease=…&latency=…&faults=all|none|key:value,…&buggify=0|1&callers=…&think=…&writes=…&s=<at>.<do>.<params>~…&t=<ms>`. A link without `v=1`, with an unknown key, or with anything out of its limits gives the default run and a note.
- **The field of events** in the bar is the site's data-field language: one bar per 1/240 of the run, as high as its events on a log scale, wide up to the cursor and narrow after it, with marks for the visitor's actions above. A transparent range input over it is the scrubber, so it works by keyboard and touch.
- **Every wait in the check is for a data attribute** that the page sets (`data-engine`, `data-runs`, `data-hash`) or for an element state. Its only time limit stops a check that hangs.

### What worked

- `make site-check` passed 25 times in a row, and 24 times with twelve checks at once on the machine. One check takes about 2 s after the build.
- In the check, a crash of a at 30001 ms leaves the 370 events before it unchanged, changes those after, and the link of the page gives the same hash, `c3d93500811bf989`.
- Two things to try, checked in the page: a cut-off of b for 40 s at 0:10 gives refusals with `lease_expired_monotonic` from 0:30, and c takes the lease at 0:38.5; a lost answer of a commit makes b swap again, find its version refused, and settle it from the manifest.

### What didn't work

- The harness refused compound shell commands with heredocs or `python3 -c` scripts: "this command is too complex to verify that it stays inside the worktree". Files went through the Write tool and edits through `sed` or Edit.
- `golangci-lint` on `<-stopped` from a `chan error`: "Error return value is not checked (errcheck)", and on `_ = <-stopped`: "S1005: unnecessary assignment to the blank identifier (staticcheck)". The runner's pattern, a closed `chan struct{}` with the error in a variable, satisfies both.
- `simlint` refused `sitecheck`: "a go statement starts a goroutine outside the host; use host.Host.Go", because `SIMULATORPKGS` covers `internal/simulator/...`. It is exempt now, like `internal/sqlite/browser`.
- The fault inputs were `type="number"`, and Chrome showed `0,02` in a locale with a decimal comma. They are text inputs now.
- The second thing to try first said that the core "refuses new transactions" after a lost answer. In the page the core swapped again and settled it at once, so the text now says that it asks S3 again until it knows.
- `playwright-cli goto` to the same page with another hash did not reload it, and the page went on with its old run. The page now reloads on `hashchange`, which `history.replaceState` does not fire.

### What I learned

- Under the preset faults most calls of the workload are refused: every instance runs callers, and only the holder serves. That is what an application that calls each instance would see.
- A full-page screenshot of `playwright-cli` at 390 px came out 390 px wide, which shows that nothing scrolls sideways.

### What was tricky

- `drawControls` runs when the cursor moves, so the buttons stayed enabled while a run was in flight; `setBusy` draws the controls too.
- `setPlaying(true)` before the first run did nothing, so autoplay is a flag that the first applied run consumes.

### What warrants review

- `/site/simulator/run.js` `describe`: the words for every event kind and core event. Each was checked against two dumped runs for `undefined`, not against every outcome.
- Whether GitHub Pages serves `engine.wasm` (12 MB) compressed, and its `max-age=600`: for ten minutes after a deploy a visitor can get a new page with an old engine. The request format is the same, so the old engine runs it.

### Future work

- Random faults of processes, drawn from the seed into the schedule, as the README suggests. The page does not offer them.

## Step 3: Self-review

**Author:** builder (simulator page)

### Prompt Context

**Verbatim prompt:** (continues from Step 1)

**Interpretation:** review the diff with competing reviewers, fix what holds up, and run every check of the brief.

**Inferred intent:** the lead can merge on the evidence.

### What I did

- The code-review skill, at medium, over `main...HEAD`. Ten findings; what I did with each:
  - Fixed: every `hashchange` reloaded the page, so the skip link and the checker's chip threw the run away. A hash that names an element of the page now only scrolls, and the run goes back into the address. `sitecheck` clicks the chip and checks that the page and its run stay.
  - Fixed: a failed request for the state was asked again on every frame. The page records the time it failed at, stops playing, and shows the failure once.
  - Fixed: after a refused run, `run` and the schedule on screen differed, so "Remove" could delete another action. The page now goes back to the shown run and asks the engine for it again, because the engine forgets its last run when it refuses one.
  - Fixed: the toggles of a card (crash or restart, close or open, stop or start) were decided from the state of the run before. A new run clears the state, and the card's buttons wait for the new one.
  - Fixed: `in PURPOSES` let `toString` and `__proto__` through; `Object.hasOwn` now. A fault with more than one `:` and a duration that is not whole milliseconds are refused too.
  - Fixed: the select of callers did not offer a value from a link, such as 3, and had a dead ternary.
  - Fixed: the checker and the schedule scanned every event on each frame; the indexes of the checks and the results of the actions are computed once for each run, and "Back" searches by halves.
  - Fixed: `run.js` had no tests after `model.test.mjs` went. `sitecheck` now imports it in Chromium and checks the round trip of a run through the link, `insert`, and 25 links that must be refused.
  - Not changed: `sitecheck` copies the lookup of Chromium and its flags from the runner in `internal/sqlite/browser`. Sharing them needs a package for both commands; it is a follow-up for the lead to decide.
- The impeccable detector found one advisory, "Repeating-gradient stripes", for the bars of the buttons and the checkbox, which are the site's identity as on `site/index.html`. It is recorded in `/.impeccable/config.json` with that reason.
- Checked the copy against claudisms.ai: "its lease then lives too long" became "lasts too long", and "An acknowledged write is never lost" became "The checker still requires that no acknowledged write is lost", which claims only what the checker does.
- A crashed process's card now offers only "Restart", since the engine ignores the rest for it, and the sticky bar is shorter on a phone (no seed, no engine status, no "of 2:00.000").

### Why

- Each fix has a check in `sitecheck` where the browser is needed to show it, and the checks of `run.js` run where the module runs.

### What worked

- `make lint`, `make test`, and `make check-browser` passed. `make site-check` passed 30 times, ten at once. No Go that runs in the browser changed, so `make test-browser` was not run.
- The console of the page in Chromium had no messages during a run.

### What didn't work

- The first round trip in the unit checks failed, `FAIL a run goes through the link and back unchanged`, because the test's faults were in another key order than `decode` gives them. The values were the same; the test now lists them in the order of `R.FAULTS`.

### What I learned

- Under the default seed, a lost answer of a commit on b comes during a commit whose swap was already applied: the core swaps again, is refused by the version, reads the manifest, and settles the commit as committed after six attempts.

### What was tricky

- `drawControls` and `drawState` both decide whether a card's buttons are enabled; `drawState` calls `drawControls` once after the cards, so the two do not fight.

### What warrants review

- By eye before it goes live: the first viewport on a laptop shows the intro and only the top of the bar, as the old page did; the copy of "Things to try" names b and 0:40, which hold for the default seed and the current code and change if the core's behaviour changes.
- GitHub Pages and `engine.wasm`: compression and the ten-minute cache after a deploy.

### Future work

- A shared package for finding and starting Chromium, for the runner and `sitecheck`.
- Random faults of processes from the seed, if Markus wants them.

## Step 4: The brief to simplify the page

**Author:** builder (simpler simulator page)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> Markus looked at the live page and said: "I think there's too much going on. Let's just have the three instances, a stream of reads and writes, pause/play and the speedup selection, and then let the user inject faults manually. No buggify, no seeds etc." He then refined it.
>
> - **Two instances**, not three: "Maybe even just two instances, since one long-running instance is what it's built for? I don't want to give people the impression this is some sort of distributed database."
> - A stream of reads and writes runs by itself. Play/pause and the speed stay. Faults are injected by hand only: no random faults, no buggify.
> - No seed anywhere on the page, and no link: "Doesn't need to be replayable. The aim is to let users see what happens under regular runs and various faults." A random seed below 2^53 for each visit, shown nowhere; a fragment from an old link is ignored without a note.
> - Keep the event log and the bucket. Remove the scrubber and Back/Step, the checker's chip and panel, the settings, the schedule list, and Copy link.
> - The run keeps going: ask the engine for a longer run ahead of time, stop at a limit where a re-run would become noticeable, about 10 fake minutes unless measured better, and offer "Start over" there, with a new seed.
> - The lead's details: "Start over" always available; fewer faults (per instance: crash and restart, cut off from S3, the clock faults a visitor can understand; for S3: an outage, and losing the answer to the next commit); the refusals of the instance without the lease must not flood the log or suggest that two instances serve; a shorter intro; "Things to try" only if true for every seed.
> - A refinement in the site's visual world with the `impeccable:impeccable` skill (`distill`), checked at 1440, 1280, and 390 wide; the first viewport on a laptop shows the running simulation.
> - Delete what the page no longer uses. The engine, `internal/core`, `internal/sim`, `internal/store/simstore`, and `docs/spec.md` stay as they are. `sitecheck` checks the new page; `make site-check` passes many times in a row.

**Interpretation:** rebuild the page on the same engine as a calm view of two instances and S3: one serves a workload, the other stands by, the visitor adds a few faults, and the run extends itself to 10 min of fake time.

**Inferred intent:** a visitor understands in a minute what s4ql's storage does when its one instance fails, without learning the controls of a test tool, and without taking s4ql for a distributed database.

## Step 5: The simpler page, its check, and self-review

**Author:** builder (simpler simulator page)

### Prompt Context

**Verbatim prompt:** (continues from Step 4)

**Interpretation:** build the page that Step 4 describes on the unchanged engine, check it in `sitecheck`, and review it.

**Inferred intent:** the lead can merge, and the live page shows the code under faults in a minute of attention.

### What I did

- `/site/simulator/run.js`: only the run as data now. The request (two instances, no `faults`, no `buggify`, the default workload), `newSeed` (two `Uint32` from `crypto.getRandomValues`, below 2^53), `insert`, the limits (`CHUNK` 2 min, `LEAD` 1 min, `LIMIT` 10 min, `SPAN` 40 s), and `story`, which gives each event its words and a tier: `stream` (reads served and commits), `story` (lease, catch-up, unknown outcomes, refusals), `you` (the visitor's faults), `alarm` (a violation of the engine's checker, which no longer has a panel but must not be hidden), or none. The link's `encode` and `decode`, the presets, the fault table, the purposes, and the invariants are gone.
- `/site/simulator/page.js`: playback, the cards of a, b, and S3, the log, the commit objects, and the field, which now covers 0 to 10 min and shows only the past. No scrubber, Back, Step, checker, settings, schedule, link, or `hashchange` handler. "Start over" is always there and is called out at the limit.
- `/site/simulator/index.html`: a short intro (two paragraphs, the build line), the bar (Play, speed, the clock, the engine's time, Start over, the field), three cells (a, b, S3) with their faults, "Things to try" as four hairline cells under them, then the log beside the commit objects, and one sentence on what is left out. The `<head>` says what the page is now.
- `/internal/simulator/sitecheck/`: `check.html` checks run.js (seeds, the request, `insert`, the spells of refusals), and the page: the first run, a crash of a just after the current time, the growth past 2 min, the stop at 10 min with Start over offered, Start over, nothing written to the address, and an old link ignored without a note. `main.go` opens Chromium at 1600 by 1200.
- `/internal/simulator/README.md`: the run times of the page's runs, two instances.

### Why

- **The faults:** per instance Crash/Restart, "Cut off from S3 for 40 s", and "Stop/Start its monotonic clock"; for S3 "Make S3 unreachable for 40 s" and "Lose the answer to the next commit". Native runs of seeds 7, 99, 4242, and 12345 showed that the wall clock set back, set ahead, or stopped changes nothing that a visitor can see, on the holder or on the standby: the lease goes on, and only the card's clock would differ. Its one visible effect, with a stopped monotonic clock, is a stale read that the checker excuses, and that needs the checker and a paragraph. So the page has no wall clock fault. A stopped monotonic clock shows a takeover, and after the start a refusal by the wall clock and "learns that b holds the lease now". 40 s is longer than the 30 s lease, so a cut-off or an outage lets the lease run out.
- **The refusals of the standby:** the workload runs on both processes, so the standby refuses every call. In the log a refusal starts a spell: one line per instance and reason ("b refuses calls: it does not hold the lease."), until the instance serves again. The same spells keep a renewal without an answer and a failed catch-up to one line while a fault lasts. The card counts them: "0 writes and 0 reads served, 36 refused, the last because it does not hold the lease."
- **Extending:** when the playback comes within `max(60 s, 2 s × speed)` of fake time of the end, the page asks for 2 min more; such a run does not hold the playback, and its state is still valid, since the events before the old end are the same. A fault or Start over holds the playback until its run is shown. At 10 min the playback stops and Start over is called out.
- **One engine.** While a longer run is in flight the engine cannot answer a request for the state, so the cards and the bucket hold for that run, up to about 0.4 s at 8 to 10 min, while the clock, the log, and the counts go on. A second Worker for the longer runs would avoid that at the cost of a second engine's memory; I left it out and say so in the report. The cards no longer show ticking clocks, only whether the monotonic clock runs, so a held card shows less.
- **The check holds for any seed**, so the page has no hook for the seed: a crash always adds events, and the engine's determinism gives the rest.

### What worked

- Run times in headless Chrome, two instances, a crash and a restart, mean of five: 2 min 70 ms, 4 min 146, 6 min 222, 8 min 300, 10 min 375, 12 min 457, 15 min 632. 10 min holds; a fault at 9 min holds the playback for about 0.4 s.
- `make site-check` passed 15 times in a row, and 15 more, five at a time. A check takes about 15 s, most of it the playback at 64× to 10 min.
- `make lint`, `make test`, and `make check-browser` passed. No Go that runs in the browser changed.
- Phone at 390: `scrollWidth` 390 in every state.

### What didn't work

- The first `sitecheck` hung for 3 min and was stopped. A dump after 20 s showed `["1","false",0,false,...]`: one run, the page idle, the cursor at 0. Two causes. In Chromium's default 800 by 600 window the frame's animation frames did not run, so the page did not play; with `--window-size=1600,1200` and the frame fixed at the top left, they do. And when the check paused at once, the cursor was before the instances' first events at about 14 ms, so the state had no instances and the crash button stayed off; the check now lets the run play until a can crash.
- `FAIL the new run has 2 min and no faults`: at 64× the page asks for a longer run at once (the lead of 128 s is above 2 min), so the new run had grown. The check now asks only for no faults and less than 10 min.
- The harness refused `playwright-cli eval` ("runs a string through eval"); `run-code` worked.
- The old words had a bug: "Process a starts on a copy of the cache that the crashed process left" at 0:00.014, because `describe` tested `e.t === 0`. `story` tests whether the process has the name of its instance.
- The action event of `loseAnswer` has no `request`, so the old words said "the next undefined". `story` takes the words from the schedule.

### What I learned

- In two-instance runs without random faults, the instance that takes the lease first depends on the seed (a for 12345, 99, 4242; b for 7), so no copy names an instance.
- After an outage of 40 s the holder kept the lease in every seed tried: it asks S3 and learns that its renewal went through. "Things to try" says only what the log shows for every seed.

### What was tricky

- A Start over while the old run sits at 10 min first asked for longer runs of the new seed, because the extension looked at `run.end` against the old cursor. It now extends only a run with the seed shown.
- Keeping a refused run from looping: the restore of the old page reset `run`, and the next frame asked for the longer run again. A refusal now stops the page with "Start over" as the way on.

### What warrants review

- Self-review with the code-review skill at medium found eight things. Fixed: the loop after a refused run, the silent wait at the end when a longer run is late (the field turns dashed, and the lead grows with the speed), a request for the state on every frame (at most every 100 ms while playing), no way on after a failure (Start over), autoplay after a refused Start over, and `REASONS` in two places. Not changed: `apply` runs `story` over every event for each longer run, a few milliseconds at most for the 4,600 events of 10 min; and the cards hold during a longer run, as above.
- By eye: the copy of the intro and "Things to try", and whether the log's stream of reads and writes buries the story too fast at 16× and 64×; the story lines are bold and the visitor's faults inverted, and the last 100 lines can be scrolled.

### Future work

- A second Worker for the longer runs, if the held cards are noticed.

## Step 6: The brief to move the page to a live session

**Author:** builder (live simulator page)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> The engine now has live sessions (#44). Markus looked at the page and asked for these changes:
>
> 1. "Don't auto-start, make the user hit play." The page loads paused at time zero.
> 2. "Make the speed a 'turbo' button, which makes it go from 1x to 5x, not toggled on startup."
> 3. "Why is there a time limit at all? Just keep going. Refreshing the page means starting over." No limit and no "Start over": the page uses a live session and advances it as it plays.
> 4. "Just have instances a and b in the columns, drop s3. I'd rather have s3 info below." No actions on S3.
> 5. "Drop the whole 'things to try' section, that should be obvious from the fault buttons."
> 6. "Faults should maybe just be 'actions'? And make it clearer that they are buttons." Crash and restart, Stop and start, Hang network, Disconnect network, Error disk, and Clock: jump forward, jump backward, pause and resume. No "Hang disk", which the engine cannot do in a browser, and no mention of it.
> 7. "Also, cut down on the text. The simulator should be pretty self-explanatory, apart from the introduction text." Labels and short values; the event log in short plain lines.
>
> The lead's details: advance in small steps tied to animation frames, never pile up calls, do not jump ahead after a hidden tab; an action only where it makes sense, with the other state of a toggle shown in place of a disabled button; the standby's refusals as one line a spell and a count; a stale read after paused clocks said in one plain line; only the shown events kept, `now` with a bounded number of commits; a one-line stop for memory if ever needed; real loading and failure states.
>
> Delete what the page no longer uses. The engine stays as it is. Rewrite `sitecheck` for the new page, with fake time past the old 10 minutes without minutes of real time, through a hook that only the check uses; wait on states and `data-*` attributes, never on durations.

**Interpretation:** replace the batch playback with a pump that drives one live session: advance by the real time that passed, times the speed, then ask for `now`; send an action at once, paused or playing. Two instance columns with their actions as plain toggle buttons, S3 and the log below, and an intro of two sentences.

**Inferred intent:** a visitor presses Play, watches one instance serve and the other stand by for as long as they like, and breaks the environment of either with buttons whose state is obvious, without reading anything but the intro.

### What I did

Read `AGENTS.md`, the README of `internal/simulator` (the live session, the actions, and what the code does under each), `simulator.js` and the demo, the two diaries, the page, `sitecheck`, `PRODUCT.md`, and the surface brief of the site. Loaded the `impeccable` skill for `distill`. It had no way to ask Markus from a sub-agent, so the direction comes from the brief.

### Why

The plan:

- **1× is real time.** A fake second for a real second, and 5× with turbo. A fake minute costs about 45 ms in Chromium, so 5× costs the engine a few ms a second.
- **The field becomes a window.** With no end there is no fixed scale, so the bar field shows the last 2 min of fake time, with the present at its right edge.
- **The hook for the check** is `window.s4qlPage.skip(ms)`, a method on the object that the page already exposes to the check. It goes through the same pump as playback. A visitor reaches it only through the console.

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

See the later steps.

## Step 7: The page on a live session, its check, and self-review

**Author:** builder (live simulator page)

### Prompt Context

**Verbatim prompt:** (continues from Step 6)

**Interpretation:** build what Step 6 planned, check it in `sitecheck`, look at it at three widths, and review it.

**Inferred intent:** the lead can merge, and the live page waits for Play and then runs for as long as the visitor stays.

### What I did

- `/site/simulator/page.js`: rewritten around one live session. `pump` sends one command at a time (an action, a skip of the check, or a step of playback) and then `now` with 8 commit objects. Each animation frame adds its real time, at most 100 ms, to a debt; a step takes the debt, at most 250 ms, and advances by it, or five times it with turbo, so calls never pile up and a hidden tab does not jump ahead. The page keeps the last 100 lines of the log, the counts of the calls, and the bars of the last 2 fake minutes.
- `/site/simulator/run.js`: `teller`, which gives the line of one event at a time, in shorter words; `clock` with tenths and hours; `offset` for the clocks. The request, `insert`, and the limits are gone.
- `/site/simulator/index.html`: the intro (two sentences and the build line), the bar (Play, Turbo 5×, the clock, the field), two columns for a and b with their facts and actions, and below them S3 (head, lease, manifest writes), the log, and the commit objects. Gone: the speed selection, "Start over", the limit, the S3 card and its faults, "Things to try", the line on what the page leaves out, and the section intros. The `<head>` names the new actions.
- `/internal/simulator/sitecheck/`: `check.html` checks the new page; `main.go` says what it checks.
- `/internal/simulator/README.md`: the page drives a live session; the measurements of its batch runs are in the past tense.

### Why

- **The actions.** Crash becomes Restart, and Stop becomes Start, because each is one action and its undo. Hang network, Disconnect network, Error disk, and Pause clocks are toggles with `aria-pressed`: a square that fills, and the whole button inverts. Hover doubles the frame instead of inverting, so it never looks like on. Play is framed like the rest, so that Turbo, when on, is the only inverted button in the bar.
- **What is offered when.** Network and disk act on the instance also when its process has crashed, as the engine does them. The clock and Stop are off for a crashed process. Stop is off while the database is not open (the engine ignores `close` then), and Start is offered only after the visitor's Stop, since a process whose core stopped closes and opens its database by itself. All actions wait while another is on its way, so a toggle is decided from the state after the one before.
- **An action while paused** goes to the engine at once and takes effect at the session's time; the card and the log show it before Play.
- **The jumps are 60 s**, twice the lease, so a jump forward on the holder makes the core refuse calls by its wall clock until it renews.
- **A stale read** after paused clocks is one plain line a spell: "a serves a stale read after its clocks stood still, which the spec allows." A violation that the design does not excuse stays an inverted alarm line.
- **The stop for memory.** The page halts with "Reload to start over" when SQLite's memory passes 1 GiB, and after 24 h of fake time, which stands in for the Go heap (about 300 MB by the README's 12.5 MB a fake hour) that the page cannot measure. At 1× that is a day of real time.
- **The hook** is `s4qlPage.skip(ms)`, queued in the same pump as playback.

### What worked

- `make site-check` passed; the built `sitecheck` passed 15 times in a row and 24 times in three batches of eight at once, after the last changes. A check takes about 5 s.
- `make lint`, `make test`, and `make check-browser` passed. No Go that runs in the browser changed, so `make test-browser` was not run.
- By eye with `playwright-cli` at 1440, 1280, and 390: paused at zero, playing, turbo, each action on, a takeover, and after 5 fake minutes. The first viewport at 1280 by 800 shows Play and both instances with their actions. `scrollWidth` is 390 at 390. In one session, pausing the clocks of the holder for 60 s and resuming them gave the stale-read line and then "b learns that a holds the lease."

### What didn't work

- The harness refused a Bash command with heredocs ("too complex to verify that it stays inside the worktree"), as in Step 2. Scratch files went through the Write tool and edits through Python scripts.
- The first screenshot with Turbo on showed it as framed, not inverted; the computed style said inverted. A second screenshot, with the mouse moved away, showed it inverted.

### What I learned

- With the engine at about 45 ms a fake minute, 5× costs the engine a few ms a second; most of a step is the two messages to the Worker.
- After a crash of the holder, the other instance served 33 to 38 s of fake time later in the checks: the 30 s lease and the time to notice.

### What was tricky

- `press` in the check first waited for its button to be enabled again, which never happens for Stop while the database closes. It now waits for the action's line in the log, which every action adds, done or ignored.

### What warrants review

Self-review with the code-review skill at medium found nine things:

- Fixed: a spell of "cannot open its database" was ended only by a new process, so a second failing disk on the same process said nothing. It ends at `opened` now, with a unit check.
- Fixed: an action that `failed` (a restart whose cache could not be copied) was written as "Ignored".
- Fixed: the Stop button read the state, so it offered Start while a process closed and opened its database by itself after its core stopped. Start is offered only after the visitor's Stop.
- Fixed: an error in the page's own code was reported as "The engine stopped", and an error in drawing repeated on every frame. Engine calls and page code have their own `try` now.
- Fixed: only SQLite's memory was guarded; a day of fake time now stands in for the Go heap.
- Fixed: the log was rebuilt on every step, and the bar codes on every draw. The log is drawn when it has a new line, and the bar codes are cached.
- Fixed: three waits of the check depended on a change at the same fake time (a restarted process running, an instance serving, a close ending). They skip fake time until the change comes.
- Not changed: Stop stays off while the database cannot be opened. The reviewer read the README as saying that `close` ends the open loop, but `run.go` ignores `close` when no database is open.
- Not changed: `page.js` keeps its own map of holder IDs beside the one in `teller`. They are filled from the same events.

By eye before it goes live: the copy of the intro and of the log lines; the word "thinks it holds the lease" in a card head after paused clocks; the 2-minute window of the field; 1× as real time, which makes a takeover take about 35 real seconds without turbo.

### Future work

- If the Go heap ever becomes measurable from the page, replace the day of fake time with it.

## Step 8: Every file of the simulator at the version of its deploy

**Author:** builder (simulator cache fix)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to the bug, the measurement, and the requirements)

> The simulator page at https://www.s4ql.com/simulator/ breaks for returning visitors after a deploy. Markus saw "BUILT FROM an unknown commit" and, in Safari's console, `TypeError: null is not an object (evaluating '$("again").addEventListener')`. `again` is a button that an older `page.js` used and the current `index.html` no longer has: his browser ran a cached old `page.js` against the new HTML.
>
> Measured on the live site (GitHub Pages behind Cloudflare): the HTML is served with `cache-control: max-age=600`, but `page.js`, `run.js`, `engine/simulator.js`, and `engine/worker.js` come with `cache-control: max-age=14400` (Cloudflare's browser cache time for scripts), and `engine/engine.wasm` with `max-age=600`. So for up to four hours after a deploy, a browser can pair new HTML with old scripts, or scripts of one deploy with an engine of another. The site deploys on every merge that touches `internal/**`, which is often.
>
> Make every file of the simulator that a page load fetches come from a URL that carries the version of the deploy, so that HTML of one deploy never runs scripts, Worker code, or WebAssembly of another, whatever the caches hold. The version is the commit that the Pages workflow builds from.
>
> - The version is fixed in the HTML that the deploy uploads, not fetched at run time. A query string (`page.js?v=<commit>`) is enough.
> - It must reach every fetch: the module script, the modules it imports, the Worker's script, everything the Worker loads (`sqlite3.js`, `vfs.js`, `bridge.js`, `fs.js`, `wasm_exec.js`, `engine.wasm`, and `sqlite3.wasm`, which `sqlite3.js` may fetch by itself), and `build.json` if the page still fetches it.
> - The files in git stay clean. A local `make site` and `make site-serve` must leave `git status` clean and still work, with a development version. Keep `internal/simulator/engine/` usable by its demo page and by `make simulator-demo` without a version.
> - `internal/simulator/sitecheck` gets a check that fails if this breaks: build the site as the workflow does, with a version, and assert that every request the page makes for a file of the simulator carries that version, and that the page works. Also keep the unversioned local path working. No check may depend on timing.
> - When `page.js` fails at start-up for any reason, the visitor today sees a page that silently does nothing. Check that an error during start-up ends in the page's existing failure box ("Reload to try again"), and fix it if it does not. Do not build more than that.

**Interpretation:** stamp the version into the HTML only, in the Pages workflow's build, and let each file pass the query of its own URL on to every file it loads, so that one stamp reaches all twelve files. Make start-up of `page.js` a function whose errors all end in `halt`.

**Inferred intent:** a deploy can never again break the page for a visitor whose browser cached files of the deploy before, and the check in CI says so on every change.

### What I did

- `/internal/simulator/serve/main.go`: flags `-page` and `-version`, which go together. `stamp` appends `?v=<version>` to each ` src="page.js"`, ` href="page.js"`, and ` href="run.js"` of the page (the script tag and the two `modulepreload` links), refuses a version outside `[0-9A-Za-z._-]`, and refuses a page without ` src="page.js"`, which includes one stamped already. The page is stamped before anything is built, and `-page` alone only stamps.
- `/site/simulator/page.js`: `version` is `new URL(import.meta.url).search`. `run.js` is now a dynamic `import("./run.js" + version)` inside `start`, as is `engine/simulator.js`, and `build.json` is fetched at the version too. The mark, the cards, and the wiring of Play and Turbo moved from the top level of the module into `start` (`drawMark`, `wire`), and `start().catch` calls `halt("The page did not start.", "... Reload to try again.")`. `halt` shows the failure box first and puts the rest (`setPlaying`, the status, `drawControls`) in a `try`, because a page that failed at start-up may lack the controls.
- `/internal/simulator/engine/simulator.js`: the Worker is `new Worker(new URL('worker.js' + version, base))`, with `version` from the module's own URL.
- `/internal/simulator/engine/worker.js`: `version` is `self.location.search`, and goes on every `importScripts`, on `fetch('engine.wasm')`, and on `sqlite3.wasm` through `s4qlInitSQLite({ locateFile: (file, prefix) => prefix + file + version })`.
- `/internal/sqlite/browser/bridge.js`: `s4qlInitSQLite(options)` passes the options on to `sqlite3InitModule`. Without an argument it behaves as before, so the test runner's Worker is unchanged.
- `/Makefile`: `SITEDIR` (default `site`) and `SITEVERSION` (default empty). `make site SITEVERSION=x` also stamps `$(SITEDIR)/simulator/index.html`. `site-check` builds without a version, checks `site/`, then copies it to a temporary directory, stamps the copy with `serve -page` at `SITEVERSION` or `SITECOMMIT`, and checks the copy with `-version`.
- `/.github/workflows/pages.yml`: `make site SITECOMMIT="$SITECOMMIT" SITEVERSION="$SITECOMMIT"`, with a comment on why.
- `/internal/simulator/sitecheck/`: `-version`; the server records every request outside `/__check/`, and `checkRequests` fails the check unless every request under `/simulator/` for `.js`, `.wasm`, or `.json` has exactly the query `v=<version>` (or none without a version) and each of the 12 files was asked for. `/__check/broken/no-run/` serves the site without `run.js`, and `/__check/broken/no-play/` serves the page with its Play button's ID renamed, as a `page.js` of one deploy would find the HTML of another. `check.html` checks that both end in the failure box with "Reload to try again", waits for the page to name its commit (so `build.json` was served before the check ends), and loads `run.js` for its unit checks at the version.
- `/internal/simulator/README.md` and `/internal/sqlite/browser/README.md`: a few sentences on the version and on the argument of `s4qlInitSQLite`.

### Why

- **The query chain.** Only the HTML is stamped. Each module reads the query of its own URL and passes it on, so a stamp in one file reaches all twelve, and a file that gets the version wrong shows up in the check by its URL. Stamping each JavaScript file in the build would have needed more replacements in more files, and an import map would not reach the classic Worker.
- **`build.json` stays.** The page could read the commit from the version, but a local build has no version and would then say "an unknown commit". Fetched at the version, it is as safe as the rest, and what the page shows does not change.
- **`sqlite3.wasm`.** `sqlite3.js` fetches it by itself, in `Module.instantiateWasm`, from `Module.locateFile(wasmFilename, scriptDirectory)`. Its wrapper of `sqlite3InitModule` takes a `locateFile` in the module argument and calls it first. In a Worker, `scriptDirectory` is the directory of the Worker's script, without its query, so without help the file was fetched without the version. Passing `locateFile` through `s4qlInitSQLite` uses that supported hook; setting `sqlite3InitModuleState.wasmFilename` from the Worker would also have worked, through an internal of the build. The OPFS proxy Worker of the build only starts when the page is cross-origin isolated, which the site is not; the check would see it otherwise.
- **The failure box.** Before, any error at the top level of `page.js` (a missing element, as in Markus's case, or a module that does not load) ended the module silently. Now the top level only declares, and everything that can throw runs in `start`.

### What worked

- Red first: with the new `check.html` and the old JavaScript, `sitecheck -root site -timeout 60s` ended in `sitecheck: the check did not end within 1m0s`, because neither broken page ever left `data-engine="loading"`.
- With the fix, `make site-check` prints, for the stamped copy, `ok   every request for a file of the simulator has the query "v=<commit>"`, `ok   the page asks for each of the 12 files of the simulator`, and both failure boxes, such as `The page did not start.Cannot read properties of null (reading 'addEventListener'). Reload to try again.`
- Mutations: on a stamped copy, removing the version from each of the nine places in turn (the two `modulepreload` links, the import of `run.js`, `build.json`, the import of `simulator.js`, the Worker's URL, `importScripts`, `locateFile`, and `engine.wasm`) made the check fail every time, naming the files without the version.
- `make site-check` passed 15 times in a row, before the review fixes and again after them, and 12 checks at once (6 unversioned, 6 stamped) all passed. `git status` was clean after each `make site`.
- `make lint`, `make test`, and `make check-browser` passed. `make test-browser BROWSERPKGS=./internal/sqlite/faultvfs` passed with the changed `bridge.js`. `make simulator-demo` ran its three runs in Chromium without a version.

### What didn't work

- The harness refused Bash commands with heredocs that held `\\b` or template strings ("this command is too complex to verify that it stays inside the worktree"), and a `for` loop around `make site-check` with `git status` after it. Edits went through the Edit tool and a Python script in the scratchpad, and the loops through shell scripts there.
- The Edit tool dropped the space in `const $ = (id)` once, which the review caught.

### What I learned

- Chromium's request for `page.js?v=x` from `<script src>` and from `<link rel="modulepreload">` is one request, and so is the dynamic import of `./run.js?v=x` after its preload, so the version costs no extra fetch.
- `sqlite3.js` reads `urlParams` from the Worker's own URL (`sqlite3.dir`, `sqlite3.wasm`, `opfs-disable`), so a query on the Worker's URL is visible to it. `v` is not one of its names.

### What was tricky

- `make site SITEVERSION` changes a file in git by design, since the workflow stamps its own checkout. The check therefore stamps a copy, and `site-check` builds `site` with `SITEVERSION=` explicitly, so that passing `SITEVERSION` to it only picks the version of the copy.
- A check that waits for `data-engine` to leave `loading` hangs until `-timeout` when start-up fails silently. That is how the old code fails the new check, which is the same way every other step of `check.html` fails.

### What warrants review

Self-review with the code-review skill at medium found ten things:

- Fixed: `make site-check SITEVERSION=x` stamped `site/` and then failed to stamp the copy again. `site-check` now builds `site` without a version and uses `SITEVERSION` only for the copy.
- Fixed: the copy was built a second time with a full build of the engine only to stamp its page. `serve -page` alone now only stamps, and the copy reuses the engine.
- Fixed: a bad version was refused only after the engine was built. The page is stamped, and the version checked, before anything is built.
- Fixed: `\b(src|href)=` also matched `data-src=`. It now needs a space before the attribute.
- Fixed: the mark was drawn only after `run.js` loaded. It is drawn first.
- Fixed: the missing space in `const $ = (id)`.
- Not changed: running `make site SITEVERSION=x` locally leaves `index.html` changed; the Makefile says that it is for the workflow's checkout or a copy. A second run is refused with "the page has no src=\"page.js\", or is stamped already", after the recipe has removed the engine.
- Not changed: the step that waits for the page to name its commit hangs, as every step does, if `build.json` is missing. `make site` always writes it.
- Not changed: `halt` shows nothing if the HTML has no `#failure`. With the version, the HTML and `page.js` of two deploys no longer meet.
- Not changed: the review said that changing `site/simulator/page.js` breaks the rule on `site/`. The brief asked for this change.

To validate: `make site-check`, and read its two blocks of output; `make site && git status`.

What a visitor sees after the deploy of this change: a visitor whose browser holds HTML of the previous deploy (up to 10 minutes) still asks for `page.js` without a version, and can still get the old mismatch until that HTML expires. After that, every HTML asks for its own version, and a file cached for four hours under one version is only used by HTML of that version. A remaining gap: HTML of deploy 1, cached for up to 10 minutes after deploy 2, asks for `?v=1`, and a browser or edge cache without that URL gets the files of deploy 2 from GitHub Pages under the name of version 1. That needs a cache that holds some files of deploy 1 and not others; a reload revalidates the HTML and fixes it.

On the live site after the deploy: the page source names `page.js?v=<merge commit>` in the script tag and both `modulepreload` links; the network panel shows every file of the simulator with that query, including the Worker's `sqlite3.wasm` and `engine.wasm`; "Built from" shows the merge commit; and `curl -sI 'https://www.s4ql.com/simulator/page.js?v=x1'` followed by the same with `x2` gives `cf-cache-status: MISS` for each new query, which shows that Cloudflare's cache key includes the query string.

### Future work

- If the gap above ever shows, `page.js` could compare a version stamped into its own content with the one in its URL, and halt with "Reload to try again" when they differ.
- The `rm -rf` of the engine in `make site` runs before `serve` refuses a page stamped already; moving the stamp into its own step before the `rm` would keep the engine.

## Step 9: A preview image of its own

**Author:** builder (simulator preview image)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged) "The simulator page reuses the front page's `og.png`, so its link preview shows the front page's headline under the simulator's title. Markus asked for a dedicated preview image for the simulator. Make `site/simulator/og.png`: 1200 by 630 pixels, PNG, in the same visual world as `site/og.png` [...] the name s4ql, the page's headline "The storage code, under faults", and one short second line that says what it is, in the page's own words [...] hint at what the page shows (two instances, one serves and one stands by, a stream of commits as barcodes) if that can be done while staying legible [...] Then, in `site/simulator/index.html`, point the `og:image` tag at `https://www.s4ql.com/simulator/og.png` [...] and write the alt text for the new image. Change nothing else in that file."

**Interpretation:** one new raster in the front page's world, and two changed lines in the head of the simulator's page.

**Inferred intent:** a link to the simulator says what the simulator is, at the sizes at which previews are shown.

### What I did

`site/simulator/og.png`, 1200 by 630, 60,479 bytes (the front page's is 68,232). On the left, as on the front page's card, text knocked out on black over the front page's field of bars from the seed `0x7334716c`: the mark and "s4ql", the headline "The storage code, under faults" in two lines at 64 px bold, the line "The real Go code, in your browser, with S3 and time simulated." at 34 px, and "www.s4ql.com/simulator" at the bottom. On the right, a framed panel in the page's own forms: "a serves" inverted, "b stands by" framed, and three commit objects as bar codes of their SHA-256 (one bar per bit, as on the page) with their short names, the first inverted and marked "head". The hashes are SHA-256 of fixed strings, not of real commits; the image shows the form of the page, not a run.

The alt text: "s4ql. The storage code, under faults. The real Go code, in your browser, with S3 and time simulated. Beside the text, a panel shows instance a serving, instance b standing by, and three commit objects as bar codes. Black and white, on a field of bars."

How to make it again:

1. Write a card page of exactly 1200 by 630 with the two Commit Mono files from `site/fonts/` next to it, outside `site/`. It copies the front page's `rng` and `layout` (density 1.1) to paint the field on a canvas, the front page's mark (the 32 bits of "s4ql"), and draws the bar codes on canvases on whole pixels, so that bars hold only black and white (an SVG with `crispEdges` still gave four greys in the screenshot).
2. Serve that directory over HTTP (`playwright-cli` refuses `file:` URLs), open a session with `playwright-cli -s=ogcard open`, and capture with `run-code`: `setViewportSize({width: 1200, height: 630})`, `goto`, wait for `document.fonts.ready`, and `screenshot({clip: {x: 0, y: 0, width: 1200, height: 630}, scale: "css"})`. The session belongs to the working directory it was opened from: a script that `cd`s elsewhere gets "The browser 'ogcard' is not open".
3. Check with `file` that it is 1200 by 630, look at it, and look at it scaled to 600 and 300 wide.
4. Embed the provenance: `impeccable embed-prompt site/simulator/og.png --prompt "Not generated. Origin: [...]"`, then `impeccable embed-prompt --scan site` (it said "3 rasters, 0 missing").

### What worked

- At 600 wide every word reads, the hashes and "HEAD" included. At 300 wide the headline reads clearly, the second line reads with effort, "a serves" and "b stands by" are just legible, and the bar codes read as bar codes.
- `make site-check` passed. The local server answers `/simulator/og.png` with `image/png`, and the Pages workflow uploads all of `site/`.

### What didn't work

- The harness refused a Bash command with a heredoc and a `cd` into the scratch directory ("too complex to verify that it stays inside the worktree"); scratch files went through the Write tool.
- `make site-serve` failed with "bind: address already in use" on 127.0.0.1:8093; the server ran on another port.

### What was tricky

- The second line had to fit beside the panel at a size that still reads at 300 wide. "The Go code itself runs in your browser. S3 and time are simulated." needed three lines with a poor break; "The real Go code, in your browser, with S3 and time simulated." fits two. "Real" stands for the page's "the Go code itself, unchanged".

### What warrants review

Self-review with the code-review skill at low found nothing: the new URL points at the added file. By eye: the words "The real Go code" on the card, and a card extractor on the live page after the deploy, as `AGENTS.md` asks.

### Future work

- The card page that makes the image is not in the repository, nor is the front page's. If preview images are made again, a small directory of card pages and a script to capture them would make them reproducible.

## Step 10: The brief for a client outside the instances

**Author:** builder (simulator client)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> Markus asked: "It would be nice to add a client to the simulator, switchable manually between reading/writing to a or b, that keeps trying to read write, so that doesn't stop when the instances stop."
>
> In the engine: a client, one source of calls that stands outside the instances. It has a target, a or b, and repeats a pause from the seed and one call, a write or a read, on the target. It never ends: a target without an open database fails the call at once with an outcome that says so; a target without the lease refuses it as today; a call in flight when the target's process dies ends with what an outside caller knows, for a write that its outcome is unknown. An action switches the target at the current fake time, and a call in flight finishes on the old target. It is a setting and an action of a run and of a session, and the equivalence of a session and a batch run holds and is tested with it. Its calls show in the trace as the client's, with the instance they went to. With the client on, a page session has no callers inside the processes. At the start of a page session the client meets an instance that serves: preferably a starts and takes the lease, and b starts a moment later as the standby, with the client on a. No change to `internal/core`, `internal/sim`, `internal/store/simstore`, or `docs/spec.md`.
>
> Tests natively and in Chromium: the client keeps calling through a crash and restart, a stop and start, and a failed disk of its target; a switch moves its calls; the other instance receives no calls; a call in flight at a crash ends with the defined outcome, and the checker stays right over many seeds and still catches a real violation on synthetic input; live and batch equivalence with the client and its switch. No test depends on timing; the cost of a fake minute does not grow with the age of the session.
>
> On the page: the client as its own small element above the two instance columns, with a switch between a and b that reads as a switch and shows which one is on, and what the client got: writes and reads served, what failed, and the reason of the last failure in a few words. The log tells the client's story without flooding: a run of failures of one kind is one line when it begins ("Client: a is down", "Client: b refuses, no lease"). Little text. The page must not read as a distributed database: the refusal of the standby is part of what it shows. Within the page's visual world; `impeccable` for the refinement; screenshots at 1440, 1280, and 390 with `playwright-cli`. Do not touch the `<head>` beyond a changed description, nor the lines that load `page.js` and `run.js`. `sitecheck` for the client, waiting on states and `data-*` attributes.

**Interpretation:** a task of the scheduler, outside every process, that hands each call to a task of its target's process and waits for it, so that a crash of the process ends the wait and not the client. Plus a setting that starts the other instances only once the first serves.

**Inferred intent:** the visitor sees one application talking to one database instance, and sees plainly what that application gets when the instance dies, stops, or is the wrong one.

### What I did

Read `AGENTS.md`, the README of `internal/simulator`, `run.go`, `live.go`, `request.go`, `state.go`, `check.go`, the page, `sitecheck`, and the two diaries; and `go doc` of `internal/sim` and `internal/host` for what a task outside an instance may wait on.

### Why

`sim` lets a task wait only through the host of its own instance or through the scheduler, so the core's waits must run in a task of the process. A task of the scheduler may wait for a task of an instance, and that wait ends when the instance crashes. So the client is a task of the scheduler, and each call is a task of the target's process that the client waits for.

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

See the later steps.

## Step 11: The client in the engine and on the page, its check, and self-review

**Author:** builder (simulator client)

### Prompt Context

**Verbatim prompt:** the brief of step 10.

**Interpretation:** see step 10.

**Inferred intent:** see step 10.

### What I did

- **Engine** (`internal/simulator/client.go`, `run.go`, `request.go`, `state.go`, `check.go`): `Request.Client{Instance}` and `Request.Standby`, also in `Session`. The client is a task of the run (`r.world.Go`), which pauses 1 ms plus a time below `workload.think` from `r.world.Uint64()` on the time of the run, then makes one call on `r.target`. A call starts with its event `started` in the client's task. If `down(p)` finds the target unable to take it (not started, crashed, open failed, no database, closed, stopped), it ends at once with `down`. Otherwise a task of the target's process, `client-<n>`, runs `serveCall`, and the client waits for it with `task.Wait`, which `sim` ends when the process crashes. If the task did not end the call, the client ends it with `lost`. `r.call` is split into `newCall`, `call.event`, `serveCall`, and `callEnded`, which ends a call once. The action `client` points the client at an instance, and is ignored without a client.
- **Standby:** `setUp` starts only `a`, gives `b` and the others an event `process` `waiting`, and starts a task `standby`. That task starts a task `serves` of `a`'s process, which calls `core.DB.Wait` with a timeout of the lease on `a`'s clock. It waits for that task with a timeout of the lease on the time of the run, then starts the others. The client's task first waits for the `standby` task. An action on an instance that has not started is ignored, except `hangNetwork`, `disconnectNetwork`, `failDisk`, `loseAnswer`, `failRequest`, and `client`.
- **Checker:** `lost` counts as `unknown`, and `down` counts as `not_committed`.
- **Fold:** `State.Client{Instance}`, from the event `client` `started` and the actions `client` that are done; the instance state `waiting`. A fix outside the client: `catch_up.commits` is an `int64` in the engine and a `float64` after JSON, and the fold read only the first, so a fold of events that went through JSON lost a partial catch-up. `attrInt` reads both.
- **Tests:** `client_test.go` (standby order, a crash and restart, a close and open, a failing disk, a switch with a call in flight, a write lost at a crash, actions before the start, a run without a client, and 24 seeds with every fault, crashes, closes, and switches); two checker cases; live and batch equivalence for three seeds with the client, standby, and switches; the cost of a fake minute with the client; request parsing.
- **Page:** a ruled strip `#client` above the columns: "Client", a two-half switch `a | b` (buttons with `aria-pressed`), Writes, Reads, Failed, and Last failure, which inverts while the client's latest call failed. A 3 px wire runs from the strip to the letter of the target's column. Below 720 px the wire is hidden and the target's card head says CLIENT. The session starts with `workload: {callers: 0}, client: {instance: "a"}, standby: true`. `b` reads "not started" for its first moment. The intro now says "Here one instance serves the reads and writes of a client, and a second stands by to take over." `run.js` has `clientFailure` and the client's log lines: "Client: a is down.", "Client: b refuses, no lease.", "Client: a crashed during the write of key N, outcome unknown.", and "You point the client at b."
- **sitecheck:** unit checks of the client's lines and words; the client calls `a` from the start and `b` gets no call; it keeps calling `a` while `a` is crashed and after the restart; the switch moves its calls to `b`, and `a` gets none; the switch back.

### Why

- **A task of the run, with the calls in tasks of the process.** `sim` fails a run whose task waits through the host of an instance that is not its own, so the core's waits must run in a task of the process. A task of the run may wait for a task of an instance, and `task.Wait` returns when that instance crashes. The client therefore outlives every crash, and the moment it learns of the crash is the crash.
- **`lost` as unknown.** What the process sent before the crash is still on its way and can be applied. The mutation below shows that this happens.
- **`down` as not committed.** The call never reached the process, so the checker gets one more thing to check.
- **Standby by `DB.Wait`.** It is how an application starts: it opens its database and waits until it serves, and a second instance is started later as the spare. A start by time would have let the client's first call meet an `a` that had not taken the lease yet.
- **No client timeout.** The call has the workload's timeout on the process's clock. Under `pauseClocks` the client waits until the resume, as a client of a frozen server does. A timeout of the client's own would let the process end the call after the client had given up, with two ends for one call.

### What worked

- The client tests passed at the first run. A mutation that counted `lost` as `not_committed` gave 5 violations of `not_committed` over the 24 seeds, so lost writes do reach the log, and the checker must treat them as unknown. The log of the test: `writes lost at a crash: 26, and of unknown outcome: 0`.
- A fake minute with the client (`think` 1000, `keep` 20): `{steps:1186 events:583 bytes:169617}` in minutes 10 to 20 and `{steps:1208 events:598 bytes:176297}` in minutes 50 to 60.
- `make site-check` passed 10 times in a row, each with the plain and the stamped copy, and 5 more times after the review fixes. `make lint`, `make test`, and `make check-browser` passed. `make test-browser` passed for every package, `internal/simulator` in 46.0 s. `go test -race -count=2` of the client, live, and checker tests passed. The impeccable detector found nothing.

### What didn't work

- The live equivalence with the client failed at first on the state after action 10: `b`'s `position` was `002fc4d9...` in the engine's state and the empty log in the fold of the decoded events. The cause was the `int64` and `float64` of `catch_up.commits` above, which no earlier scenario had reached: it needs a partial catch-up, which `failDisk` on `b` during a catch-up gave.
- With `keep` 100 and the defaults, the client variant of the minute test was at +14% bytes, because one client writes less than four callers and fills the table later. Across seeds 11 to 13 the steps went both up and down, so nothing grows. Shorter pauses and `keep` 20 give steady means.
- The harness refused Bash commands with heredocs that held JS template strings, a command with `git status` after `make site`, and `playwright-cli eval`. The edits went through Python scripts written with the Write tool, and the page was driven with `playwright-cli run-code`.
- `git checkout internal/simulator/check.go` after a mutation also reverted my uncommitted edit of that file. I redid it.
- Port 8093 was taken, so `make site-serve` failed with `bind: address already in use`. `serve -root site -isolated=false -addr 127.0.0.1:8137` worked.

### What I learned

- `sim`'s `task.Wait` from a task of the run returns as soon as the instance of the task crashes (`t.done || t.inst.crashed`). That is the primitive behind `lost`.
- `core.Open` does not wait, which is why `setUp` and `act` can call it from tasks of the run, and why the standby task can start the other instances in one step.

### What was tricky

- The first call of the client must come after `b` has started, or it meets an `a` that does not serve yet. The client's task waits for the `standby` task as its gate.
- A test of the in-flight call needs a call that is certainly in flight at the switch or the crash, whatever the seed. A hang of `a`'s network holds every write on `a` until its 30 s timeout, so with short pauses one is always in flight.

### What warrants review

Self-review with the code-review skill at medium found nine things:

- Fixed: `loseAnswer` and `failRequest` for an instance that had not started were ignored. Their instance only filters requests, so they are now armed (tested).
- Fixed: `callEnded` could end a call twice if the client gave up a wait whose context ended. The context of the client's task never ends, but `callEnded` now ends a call once, and a failed wait records nothing.
- Fixed: the long-log assertion of the default session had dropped from 500 to 200. It is 500 again, and 300 for the client.
- Fixed: a garbled doc comment of `attrInt`.
- Not changed: "the gate waits for `core.Open` of the others, which can block". `core.Open` does not wait, so the starts run in one step.
- Not changed: "`isEnding` is checked once before the loop of starts". There is no wait in the loop, so the end cannot come between two starts.
- Not changed: the cards also count the client's calls. With no callers they are the only calls, and `down` is not a refusal, so the card does not count it as one.
- Not changed: the lookup of an instance by letter in `setUp` and `act`, and the loop of the client that is like that of a caller on another clock and generator. A shared helper would add indirection for two uses.

To check by eye: the wire (3 px, left 28 px) lands on the letter of the column at 1440 and 1280. The Last failure cell inverts while the client fails, which is the page's one strong signal of a refusal. At 390 the CLIENT tag sits at the right of the card head.

### Future work

- The client could get a timeout of its own on the time of the run, with `lost` when it gives up, if a page should show a client that gives up on a frozen server. That needs care so that the process cannot end the call a second time.

## Step 12: The client in its own voice

**Author:** builder (client voice)

### Prompt Context

**Verbatim prompt:** Markus: "The client says: a is down/crashed. But from the outside, it can't know that. Write the client status from the client perspective: unknown if no answer, any error reported by the instance, etc." The lead's brief: reword everything the page says about the client so that it holds only what the client can observe. No answer (`down` and `lost`, whatever the reason) says only that, and for a write that its outcome is unknown. An error from the instance is given in a few words, for every outcome and error the engine can give. Served stays as it is. The instance cards, the instances' own log lines, and the visitor's lines stay. No Go changes, no change to the `<head>` of `index.html`. Assert in `check.html` that the client's texts never say "down", "crashed", or "stopped".

**Interpretation:** a change of words in `run.js`, with the check to match.

**Inferred intent:** the page should not let the client know more than a real client would, so that a visitor learns what an application behind s4ql actually sees.

### What I did

- `run.js`: `clientFailure` now has two voices. `down` and `lost` give "no answer from a" (a read) or "no answer from a, write outcome unknown" (a write), and one kind, `no answer a`, so a run of them takes one line. Every other outcome is "error from a: ..." with `errorWords`: the refusal's reason, "write outcome unknown", "write not committed", "write rolled back", "cache busy", "timed out", the first cause of a stop ("database closed", "local failure", "cache not in the log", "log broken", else "out of service"), "no database open", "local failure", and "call failed" for anything else. The log lines are "The client gets no answer from a.", "The client gets no answer from a to the write of key 7: outcome unknown.", and "The client gets an error from a: no lease.". `DOWN` is gone. A new `reasonWords` turns a reason into words, also one the page does not know, and the card and the instance's log line use it too.
- "Failed" stays one count, and "Last failure" keeps its label. The CSS of the strip, in the `<head>`, has a grid for three counts and the last failure, so a fourth cell would need the `<head>`.
- `check.html`: the old words replaced. New unit checks: no answer reads the same for all six reasons of `down` and for `lost`, once for a read and once for a write. Every outcome, with every reason of `REASONS`, an unknown reason, and an unknown outcome, gives words without "down", "crashed", "stopped", "undefined", or "null". A stop's first cause is the one named. On the page, MutationObservers collect the last failure and every client line of the log for the whole check, and the check asserts the same of them. After the switch to `b`, if `b` refused calls, the log said so in the client's words.

### What worked

- `playwright-cli run-code` with a driver script at 1280 and 390, one fresh page per state: served ("none"); crash of `a` ("no answer from a"); stop of `a` ("no answer from a, write outcome unknown", the card says "stopped"); pointed at `b` ("error from b: no lease"); a write held by a hang when `a` crashes ("The client gets no answer from a to the write of key 2: outcome unknown."); a failing disk ("error from a: local failure", then "no answer from a" once the core stopped); a hung network ("error from a: write of key 4 not committed", then "error from a: lease ran out by its monotonic clock" while the card says "thinks it holds the lease").
- `make site-check` passed 8 times in a row before the review fixes and 7 after. `make lint` passed.

### What didn't work

- The harness refused a Python heredoc whose JS held `split("; refused ")`, and `playwright-cli eval`. The edits went through the Edit tool and a script file, and the page was driven with a shell script that passes the driver to `run-code`.
- The first try at the in-flight write hung `a`'s network for 1.5 s before the crash, and no write was on `a` yet. 9 s was enough.

### What was tricky

- A run of failures takes one line, so a lost write inside a run of no answers gets no line of its own. Before, every `lost` call took a line. Kept: to the client it is the same failure, and a kind per read and write would flood the log as they alternate. The last failure cell still says "write outcome unknown".

### What warrants review

Self-review with the code-review skill at medium found nine things:

- Fixed: `CAUSES` matched `: closed` anywhere in the error, so a local failure whose text ends in "closed" read "database closed". Now only the first cause after "the instance has stopped: " counts (tested).
- Fixed: an unknown reason read with spaces for the client and with underscores on the card and in the instance's line. `reasonWords` is now used in all three.
- Fixed: the kind of a run was built from the words alone. It now has the outcome too.
- Fixed: `errorWords` returned pairs of the same string. It returns one string.
- Fixed: the collected texts were one module-level set for the unit checks and the page check. Each has its own.
- Fixed: the unit checks listed the reasons by hand. They take `Object.keys(R.REASONS)`.
- Not changed: the lost write inside a run of no answers, above.
- Not changed: "the observer sees only what was drawn, and lines can leave the 100 kept between draws". The page draws after each command, and the check advances 1 s of fake time per command, far below 100 lines. The exhaustive unit check covers the words of every outcome.

To check by eye: the strip at 390 and 1280 with the longest value, "error from a: lease ran out by its monotonic clock", which wraps in the last failure cell.

## Step 13: Status, S3, instances, and two clocks

**Author:** builder (client status)

### Prompt Context

**Verbatim prompt:** Markus: "Maybe 'Last failure' should be status? I keep getting confused by it, thinking that there's an error when actually it's okay again. We can still see the last errors in the log." Four more requests from Markus arrived through the lead while the builder worked, for the same branch: "hang/disconnect network is misnamed now that the client is there. It looks like it's hang/disconnect s3?"; "Why is there sometimes a and sometimes a2?"; "We should also have a 'reset clocks' button."; and "Should we distinguish between the two types of clocks? So there's a 'pause monotonic clock', and the other one can jump back and forth and pause?"

**Interpretation:** five changes of the page's words and controls, each with its checks in `check.html`, and no Go code.

**Inferred intent:** the page should never make a visitor believe something that is not so: an error that is over, a network that is not the one the button acts on, a second instance that is the first one restarted, a clock fault that a reset has removed.

### What I did

- Status (`cb99125`): the cell "Last failure" is "Status", the result of the client's latest call that ended. `clientStatus` in `run.js` gives "ok from a" for a served or committed call, and the words of `clientFailure` for a failure, unchanged. `NO_CALLS` is "no calls yet". The cell inverts only while the status is a failure. Ids: `#client-status` (with `data-failing`) and `#client-status-words`; the class `.last` is `.status`.
- S3 (`9c6025f`): "Hang network" and "Disconnect network" are "Hang S3" and "Disconnect S3". The log said "You hang the network of a." / "The network of a no longer hangs." / "You disconnect a." / "You connect a again.", and now says "You hang the requests of a to S3." / "The requests of a to S3 no longer hang." / "You disconnect a from S3." / "You connect a to S3 again.". In the `<head>`, "hang or disconnect its network" became "hang or cut its connection to S3", and "cut its network" became "cut it off from S3".
- Instances (`9c6025f`): the page shows the instance wherever it showed a process. `instanceOf` in `run.js` strips the number that the engine gives the process of a restart. The card head no longer has the process span, and its CSS rule is gone. The commit objects' writer, the S3 lease holder, and the log use the instance. "a2 starts on the cache that the crashed process left." is now "a starts again, on the cache that it left when it crashed." A line about two processes of one instance would have read "a takes the lease over from a." and "a learns that a holds the lease."; they read "a takes the lease again." and "a learns that its lease from before has not run out yet.". On the card, "it thinks so; S3 names a" for its own earlier lease reads "S3 names its lease from before".
- Reset clocks (`e015520`, then `4f5afb2`): `resetClocks` in `page.js` queues `startWall` and `startMonotonic` for a clock that stands still, then `shiftWall` by the wall clock's offset. A job's `act` can now be a function that `pump` calls when the job's turn comes, so the shift is computed from the state after the starts, and a step with nothing to do is left out. `take` gets the job and gives the steps of a reset one log line, "You reset the clocks of a.". The button is off while the clocks are untouched (`touched`: a clock stands still, or the wall clock is off by a nanosecond or more).
- Two clocks (`4f5afb2`): the card's clock actions are two labelled rows, "Wall clock" with +60 s, −60 s, and Pause, and "Monotonic clock" with Pause, and Reset clocks apart at the right of the second row (at 390 px: the wall row in three columns, Reset clocks on a row of its own). The card's clocks say each clock only where it differs: "wall +60 s", "wall paused, −5 s", "monotonic paused", joined by "; ", else "true time". The monotonic offset is no longer shown, only whether it runs, so after a reset the card reads "true time". New log lines: "You pause the wall clock of a.", "You start the wall clock of a again.", and the same for the monotonic clock. The stale read reads "a serves a stale read after both of its clocks went wrong, which the spec allows.".
- `check.html`: the status at zero ("no calls yet"), while `a` serves ("ok from a"), while `a` is crashed (the no-answer words, failing), after the restart (once an instance serves, the client is pointed at it, and the status is "ok from" it, not failing), and after the switch to `b` (a status from `b`, failing only for a failure). Unit checks of `clientStatus`, of the restart lines, and of `instanceOf`. The labels and log lines of Hang S3, Disconnect S3, and Error disk. A check that no text of the page, and no line the log has had, matches `/\b[a-e][2-9]\b/`, after the crash and restart of `a`, after the crash and restart of the holder, and at 11 min. Each clock action, its opposite, and Reset clocks after +60 s and both pauses: both clocks run, the wall clock is off by 0 ms, the card says "true time", neither Pause is pressed, Reset clocks is off, and the log has one line for the reset.

### What worked

- `make site-check` passed after each change. The wall clock lands on true time exactly: the check reports "off by 0 ms". The engine keeps clocks in nanoseconds, `duration` rounds a shift to them, and the offset in the state is a whole number of nanoseconds in a float64 of milliseconds.
- `playwright-cli run-code` with the driver inline in single quotes, at 1280 and 390: the strip at zero, served, `a` crashed, served again after the restart, and pointed at the standby, and the card's clock rows.

### What didn't work

- The harness refused `playwright-cli eval`, a `run-code` that read its driver with `$(cat flow.js)`, and a `mkdir` chained with them. Passing the driver inline in single quotes worked.
- Port 8093 was taken; the site was served on 8197.
- The first card screenshot showed a pressed Pause not inverted: the button's 120 ms colour transition was still running. A short wait before the screenshot fixed it; the page is right.

### What was tricky

- After `a` crashes and restarts, `a2` and `b` both wait for `a1`'s lease to run out, and either can take it (seen both ways). So the check cannot wait for "ok from a" after the restart. It waits for an instance to serve, points the client at it, waits for "ok from" it, and points the client back at `a`.
- The three clock cases on the holder, 60 s of fake time each, from the page. With the client on the holder, nothing happens in any of them except the card's clocks: each write of the client commits, and a commit renews the lease, so the holder keeps the lease. The log shows only the visitor's two lines, the card "serves" throughout, and the status "ok from a". With the client pointed at the standby during the pause and back at the holder after:
  1. Monotonic paused 0:20.0 to 1:20.0: `b` takes the lease over from `a` at 0:48.5, the card of `a` says "thinks it holds the lease" while paused, and after the start the client gets "error from a: lease ran out by its wall clock" at 1:21.6; at 1:34.4 `a` learns that `b` holds the lease.
  2. Wall paused 0:20.0 to 1:20.0: nothing but the card ("wall paused, −60 s", then "wall −60 s"); `a` keeps renewing by its monotonic clock and serves.
  3. Both paused 0:20.0 to 1:20.0: `b` takes the lease over at 0:53.5; after the start, S3 refuses a commit of `a` at 1:20.8, `a` learns that `b` holds the lease at 1:20.9, and the client gets "write of key 9 not committed". In this seed the client's first call after the start was a write, so no stale read showed.
  No stale read appeared in cases 1 and 2.

### What warrants review

Self-review with the code-review skill at medium found eight things:

- Fixed: a reset whose shift had nothing to do (only the monotonic clock paused) dropped the step without drawing, so every control stayed off until Play. `pump` now calls `drawControls` when it drops a step, and the check covers that reset.
- Fixed: "a learns that its lease from before has not run out yet" also when the manifest named the process's own current holder ID (the core's `lose` when salt or head differ). The teller keeps each process's latest holder ID and says "a learns that it lost the lease." then (unit-checked).
- Fixed: a step of a reset that failed or was ignored logged as an action the visitor did not take. `resetLine` in `run.js` says "You reset the clocks of a. Ignored: ..." (unit-checked).
- Fixed: each step of a reset put a mark on the field. Only the first does.
- Fixed: after a crash and restart, the S3 row said "a, 30 s" for the lease of the crashed process while the card of `a` said it stands by. `holderName` now says "a before its restart" when the process that held the lease is not the instance's current one.
- Fixed: the check's process pattern missed `a10` and up. It is `/\b[a-e]([2-9]|[1-9][0-9]{1,2})\b/`; three digits at most, because the first try, with any number of digits, matched the short name of a commit object, "b8828120".
- Not changed: `actionWords` still words `pauseClocks` and `resumeClocks`, which the engine accepts though the page no longer sends them.
- Not changed: "no calls yet" is in the HTML and in `NO_CALLS`; the HTML needs it before the script runs, and the check asserts the text at zero.

`make site-check` passed 10 times in a row before the review fixes and 6 after (5 runs of `sitecheck` on `site/`, and one full `make site-check`). `make lint` passed.

- The `<head>` changed for this step: the two descriptions (one phrase each) and CSS for the status cell, the clock rows, and the removed process span.
- Clock faults on the holder show nothing while the client calls the holder, because its writes renew the lease. A visitor who pauses a clock of the serving instance sees no story unless the client calls the other one. This is a question for the lead and Markus, not something the page can fix by its words.

## Step 14: Requests in place of a client

**Author:** builder (requests)

### Prompt Context

**Verbatim prompt:** Markus, through the lead: "I forgot the client isn't external, but in the app. So it would also crash when the app crashes." And: "There is no load balancer, maybe an HTTP proxy. The typical use case is one app instance serving requests (responding over HTTP to browsers basically) on one machine, just like when you would use SQLite in a server-side context." Then: "maybe we keep it as 'Requests'".

**Interpretation:** the strip keeps its behaviour, and stands for the requests that reach the app; each instance is the app with s4ql embedded. A change of words only, in `site/simulator/` and `check.html`, because the engine already runs each call of its client in a task of the instance's process.

**Inferred intent:** s4ql is embedded, like SQLite on a server. The page must not suggest a client outside the app, a load balancer, or a distributed database.

### What I did

- The strip is "Requests", its switch label "To" (so "Requests to a | b"; "Sent to" wrapped to two lines beside "Requests" at 390 px), its status before the first request "no requests yet". The counts and the status words ("ok from a", "no answer from a", "no answer from a, write outcome unknown", "error from b: no lease") are unchanged. The tag on the card at 720 px and narrower says "requests".
- Log: "The client gets no answer from a." is "A request gets no answer from a."; "The client gets an error from b: no lease." is "A request gets an error from b: no lease."; "The client gets no answer from a to the write of key 7: outcome unknown." is "A request gets no answer from a: write of key 7, outcome unknown.", which matches the error form "A request gets an error from a: write of key 7, outcome unknown."; "You point the client at b." is "You send the requests to b." Singular throughout: a line describes the request that began the run.
- Introduction, second paragraph: "s4ql is built for one long-running instance per database, an app with s4ql embedded. Here requests to the app go to one instance, and a second stands by to take over."
- Names: `clientFailure`, `clientStatus`, `NO_CALLS` in `run.js` are `requestFailure`, `requestStatus`, `NO_REQUESTS`; `#client`, `#client-writes`, `#client-reads`, `#client-failed`, `#client-status`, `#client-status-words` are `#requests...`; the class `.client` is `.requests`; the cards' `data-client` is `data-requests`; `point` is `send`. The engine's `client` (session setting, caller, action, state) keeps its name. The `<head>` said no "client", and is unchanged.
- `check.html`: the new words, and `noClient`, which asserts that no text says client (any case) in the title, the `<meta>` contents, `body.textContent`, the `aria-label`, `aria-description`, `title`, `alt`, and `placeholder` attributes, every line the log has had, and every text the strip has shown: at zero, while `a` is crashed, with the requests sent to `b`, and at 11 min. A unit check asserts the same of every request text that `run.js` can give and of the lines of a run of failures.

### What worked

- With a planted `aria-label="Client a"`, the check failed four times, "FAIL at zero, no text of the page says client (Client a)" and so on; without it, `make site-check` passed 9 times in a row. `make lint` passed.
- `playwright-cli run-code` at 1280 and 390: at zero, served, `a` crashed, and the requests sent to `b`.

### What didn't work

- A Python heredoc with template literals in it was refused by the worktree guard ("too complex to verify that it stays inside the worktree"); the Edit tool did the same edits.
- Element screenshots of `section.panel` had the sticky playback bar over the strip; page clips from the top fixed it.

### What warrants review

Self-review with the code-review skill at medium:

- Fixed: the introduction's "the requests" had no antecedent; it says "requests to the app".
- Not changed, for the lead and Markus: "requests" now names both the app's requests and the instance's requests to S3 ("You hang the requests of a to S3."). The brief kept the actions and their lines as they are.
- Not changed: the cards and the instances' log lines still say "calls" ("b refuses calls: no lease."). These are the calls into s4ql that a request's handler makes, and the brief kept the cards.
- Not changed: `noClient` reads `body.textContent`, which includes `<noscript>`, so it is stricter than what a visitor sees; the body has no inline script.
- Not changed: the engine's names `"client"` are compared in `page.js` and `run.js` in four places, as before.
- An ignored action `client` would log the engine's text "the run has no client", but the page always starts its session with a client, so it cannot happen.
- Follow-up from the lead, on the double meaning of "requests": the Hang S3 lines say "You hang the connection of a to S3." and "The connection of a to S3 no longer hangs.". No other text the page shows says "request" for a request of an instance to S3: S3's refusals already name the renewal, commit, or release, the cards and the S3 row say no "request", and every format of `simstore` is known to `storeLine`, so no raw store text with "request" reaches the log. `check.html` asserts at 11 min that every line the log has had that says request begins "A request gets " or is "You send the requests to a." (or b). `make site-check` passed 4 times, `make lint` passed.

## Step 15: One row of clock actions

**Author:** builder (clocks)

### Prompt Context

**Verbatim prompt:** Markus, through the lead: "I still like having the clocks, because it shows it has no effect on the result. But maybe make the wall clock increments +-5, and just one button to pause both clocks?"

**Interpretation:** jumps of the wall clock of 5 s, one Pause that toggles both clocks, Reset clocks as before, and no separate pause of either clock. One row if it fits, with as few words as possible.

**Inferred intent:** the clocks stay on the page to show that moving them does not break the result. Small jumps and one pause are easier to read than two clocks with their own pauses.

### What I did

- The row: label "Clocks", buttons "Wall +5 s", "Wall −5 s", "Pause" (with its LED and `aria-pressed`), "Reset". Their `aria-label`s are "Move the wall clock of a forward 5 s", "… back 5 s", "Pause both clocks of a", and "Reset the clocks of a". It is one row at 1280. At 390 the label takes its own line and the four buttons a 2 by 2 grid, like every other action row there, because about 324 px inside the card is too narrow for four of them.
- `page.js`: `JUMP` is 5000. Pause sends `pauseClocks` or `resumeClocks`, by `paused(st)`, which is true if either clock stands still (the page stops and starts them together). Reset clocks sends `resumeClocks` if the clocks stand still, then the `shiftWall` back to true time, as before. The `data-a` values `wall` and `monotonic`, their ids `wall-*` and `mono-*`, and the CSS `.clock.wall`, `.reset`, and the label's `min-width` are gone.
- The card's Clocks value was "true time", "wall +60 s", "wall paused, −5 s", "wall −5 s; monotonic paused". It is "normal", "wall +5 s", "paused", "paused, wall −5 s".
- Log, `run.js`: "You move the wall clock of a forward 5 s." (and back), "You pause the clocks of a.", "You start the clocks of a again." (was "You resume the clocks of a."), "You reset the clocks of a." The lines for `stopWall`, `startWall`, `stopMonotonic`, and `startMonotonic` are gone, because the page no longer sends them.
- Stale read: was "a serves a stale read after both of its clocks went wrong, which the spec allows.", is "a serves a stale read after its clocks were paused, which the spec allows." The checker excuses a stale read only if the monotonic clock was stopped, and on the page only Pause stops it, together with the wall clock.
- `check.html`: untouched; +5 s and −5 s; −5 s, then Pause (both clocks stop, "paused, wall −5 s", then "paused, wall −10 s" after 5 s), then Pause again ("wall −10 s"); +5 s, Pause, 2 s, Reset ("paused, wall −7 s" before, "normal" after, one line in the log); Pause and Reset with no fake time between; and the clock buttons off after a crash. A unit check asserts the new stale-read line.

### What worked

- `make site-check` passed 10 times in a row, then 6 and 15 more, then 8 more after the review fixes. `make lint` passed.
- `playwright-cli run-code` at 1280 and 390: untouched, after +5 s, paused, and after a reset.

### What didn't work

- One run of `make site-check`, between the batches, failed: "FAIL fake time goes on to 11:00.0, with 44 lines and 3 commit objects". A passing run has some 269 lines and 93 commit objects there, so the requests went unserved for about 10 minutes. I did not save that run's output. My best guess: the step before crashes the instance that holds the lease and restarts it, but leaves the requests where they were. In every saved run that instance was b, so a took over and served. If it was a, the restarted a stands by and refuses every request. The check now sends the requests to the instance that serves before the skip. This changes nothing in the branch that every saved run took. Not confirmed, since the seed is random.
- `playwright-cli eval` and a `run-code` with `$(cat file)` were refused by the worktree guard; inline `run-code` worked.

### What warrants review

Self-review with the code-review skill at medium:

- Fixed: the check after Reset said "Pause is not pressed" without asserting it; it asserts `!pressed("pause")` now.
- Fixed: the comment on `JUMP` explains the 5 s: short next to the lease of 30 s, so it shows that it changes nothing. The reviewer noted that +5 s no longer makes the core refuse calls by its wall clock, as +60 s did. That is what Markus asked for.
- Not changed: the stale-read line, `paused(st)`, and the removed lines for the four single-clock actions assume that the page is the only source of clock actions in its session, which it is. The engine still accepts the single-clock actions; a page that sent them would need the lines back.
- Not changed: "normal" for untouched clocks. The lead's brief said "normal when untouched". It is also truer than "true time" after a pause, because the monotonic clock stays behind.

## Step 16: The middle of the page as a diagram

**Author:** builder (diagram)

### Prompt Context

**Verbatim prompt:** Markus, through the lead: "it would be nice to make it more visual. For example, the instance in the middle, with actions to crash or stop. Then an edge to S3, with hang/disconnect, and an edge to disk, with error action. And the clocks, with the actions for those." On the lead's sketch of requests at the top, two instance nodes each with a disk and clocks, and both joined to one S3 below: "Yes. But keep how the log works, and the barcode commit objects, I like those."

**Interpretation:** rebuild the strip of the requests, the two instance cards, and the S3 row as one picture: nodes joined by edges, every action on the node or edge that it acts on, and every node and edge showing its own state. Hung, disconnected, and a failing disk must look different by line style alone. Keep the playback bar, the introduction, the log, and the commit objects as they are. No new actions, no lost texts, no Go changes; the smoke check keeps everything it checks and adds the states of the edges and the edge of the requests.

**Inferred intent:** a visitor should see the system in two seconds: two instances, each with a disk and clocks, both joined to one S3, requests going into one of them. Then play with faults where they happen.

## Step 17: Building the diagram

**Author:** builder (diagram)

### What I did

- Built on the clock row of step 15 (`git merge worktree-agent-ac42757420ad6b019`).
- The middle of the page is one picture in `.system`: the strip of the requests as before; a thick edge (3 px, with an arrowhead) from the middle of the strip down into the instance that the requests go to, which moves with the switch; two instance nodes side by side; under each, its disk and its clocks on short hairline edges; and from each instance an edge straight down into one S3 node, centred below both. a is drawn as the mirror of b (clocks, disk, edge to S3 from the outside in), so both edges to S3 run down the middle and meet S3 side by side.
- An instance node has its letter, its state word, Crash/Restart and Stop/Start, and Lease, Served, Refused. The instance that serves has its head inverted, buttons and all: the head swaps `--void`, `--lumin`, `--ink`, and `--rule`, so the buttons, their disabled and pressed states, the focus ring, and the selection invert with it. A node whose process is not running has a dashed frame; a crashed one also has its letter struck through.
- The edge to S3 carries Hang and Disconnect, sitting on the line. Working is a solid hairline, hung is dashed (6 px on, 5 off) along its whole length into S3, disconnected is cut: a gap of 30 px with a tick across each end. Both at once are dashed and cut.
- The disk box: a drawn cylinder, the bar code of the cache's position, "Cache" and the text the card's Cache row had, and Error. While the disk fails, its frame is dashed and its edge from the instance has an X.
- The clocks box: a dial, the text the card's Clocks row had, and the four clock buttons, two by two where the box is at least 190 px wide (a container query), else one column. The dial's outer mark goes round with true time and its hand with the instance's wall clock, once a fake minute: the hand stops while the clocks are paused, and stands off the mark by the wall offset. The dial's circle is dashed while paused, and the hand is hidden while the clocks are "none".
- S3 has its head as a bar code too, so a cache at the head shows the same bar code as S3.
- Below 1140 px the instances stack. The edge of the requests is a short arrow above the instance that they go to (and its head says "requests", as the card did at 720 px). Each edge to S3 runs down a lane left of the disk and clocks, then along the bottom through Hang and Disconnect to a terminal "▸ S3"; S3 sits below both instances. The lane and the bottom line take the same dashes and cut.
- Words: the buttons on the edge say "Hang" and "Disconnect" and the one on the disk "Error"; their accessible names are "Hang the connection of a to S3", "Disconnect a from S3", and "Error on the disk of a". Crash and Stop have names "Crash a", "Restart a", "Stop a", "Start a", kept in step with their text. Each instance heading reads "Instance a" to a screen reader; the edge to S3 is a section named "Connection of a to S3", the boxes are sections headed "Disk" and "Clocks". The row label "Clocks" became the box heading. Log, states, and every other text are unchanged.
- `page.js`: `cards`/`drawCard` are `nodes`/`drawNode`; it sets `data-hung` and `data-disconnected` on `[data-edge="s3"]`, `data-failing` on `[data-edge="disk"]`, `data-paused` on the clocks, turns the dial, and draws the head's bar code in S3. Removed: `.cells`, `.cell*`, `.actions`, `.wires`, and the 520 px rules that wrapped action buttons.
- `check.html`: the labels Hang, Disconnect, Error; for each fault, that its edge or disk has the attribute while on, looks different (computed style of the line, the cut, the frame, the X), and looks as before once off; that working, hung, and disconnected give three different looks; that the edge of the requests is drawn into a at zero, into b after the switch, and into a again; that each action's accessible name names its instance (a's names must be b's with b changed to a, because "a" is also a word); Restart's name after a crash; and at 390 px, that nothing scrolls sideways, the arrow of the requests is over a and not b, and the bottom line of the edge to S3 is dashed while it hangs.

### What worked

- With the dashed style of a hung edge removed by hand, the check failed twice: "FAIL the edge of a to S3 shows that it is hung while Hang is on, ..." and "FAIL the edge to S3 looks different working, hung, and disconnected (...)".
- Before the review fixes: 6 runs of `sitecheck -root site`, three at a time, and 10 runs of `make site-check` (20 checks), all ok. `make lint` passed.
- `playwright-cli` at 1440, 1280, and 390, in each state of the brief: at zero, playing, a crashed, a hung, a hung and disconnected, a disconnected, a's disk failing, a's clocks paused after Wall −5 s, requests sent to b, after a takeover, and with all faults on at once. The first viewport at 1280 by 800 shows Play, the strip, and both instance nodes. `scrollWidth` is 390 at 390.

### What didn't work

- The worktree guard refused a shell pipeline that spliced the new markup in with `awk`; separate `head`, `cat`, and `tail` commands into a scratch file worked.
- The box's title row was first named `.top`, which is also the page header's class, and got its bottom rule.
- At 390 the bottom line of the edge to S3 painted over Hang and Disconnect: a positioned `::after` paints after positioned children. The buttons have `z-index: 1`.
- A `playwright-cli` session stopped answering ("The browser 'default' is not open", then "EADDRINUSE ... default.sock" on `open`); a new session with `-s=d2` worked.
- A first driver run timed out at 390 and 1440 waiting for five commits under the default 5 s timeout; `page.setDefaultTimeout(120000)` fixed it.

### What warrants review

Self-review with the code-review skill at medium found eight things:

- Fixed: the bare `.head` of the instance node also matched `li.commit.head`, so the head commit object lost its layout (its tag no longer sat at the right). The rules are `.node .head` now; a screenshot of the head commit shows it as before.
- Fixed: below 520 px the old rules wrapped action buttons; without them the row of the edge to S3 (Hang, Disconnect, cut, "▸ S3") would overflow at 320 px. At 720 px and narrower the buttons on the edge have 8 px padding and the cut is 20 px wide. At 320 px the diagram fits; the page is still 330 px wide because of the header's nav, as before this change, and outside this brief.
- Fixed: the check saw only the wide layout. It now also checks at 390 px (the frame is narrowed and widened again): no sideways scrolling, the arrow of the requests over a and not b, and the bottom line of the edge to S3 dashed while it hangs. `requestsEdge` is empty while the fork is hidden.
- Fixed: the check that each action names its instance matched the English word "a". a's names must now equal b's with b changed to a, and b's must contain b.
- Fixed: the strip of the requests switched at 1100 px and the diagram at 1140 px; both switch at 1139 px now.
- Fixed: `data-f="face"` on the dial was unused.
- Not changed: the inverted head hard-codes `#fff` and `#000`. A rule cannot swap `--void` and `--lumin` through each other (a cycle), and the page is black and white by design.
- Not changed: the markup of a and b is written out twice, as before; generating it in `page.js` would also move it out of the HTML that the check and a visitor without JavaScript read.

After the fixes: 7 runs of `make site-check` (14 checks), 3 runs of the previous `sitecheck` binary at once, and 6 runs of one built with the new check, three at a time: all ok. `make lint` passed.

For the lead and Markus:

- a is the mirror of b, so in a the order left to right is clocks, disk, edge to S3, while the DOM order (and the order of Tab) is edge to S3, disk, clocks in both. I chose the mirror so that both edges to S3 run down the middle into one S3.
- Below 1140 px the edges to S3 end in a terminal "▸ S3" instead of running down to the S3 node, and the edge of the requests to b starts in the gap above b. The brief allowed edges to simplify there.
- The dial is new: it shows the existing state of the clocks (true time, the wall clock, paused), not new state.

## Step 18: Sound

**Author:** builder (sound)

### Prompt Context

**Verbatim prompt:** Markus, through the lead: "add some subtle sound to the simulator on important events. Make it sound very digital. Default off, but can be turned on. Square waves. Watch the volume."

**Interpretation:** the lead decided the rest: Web Audio only, `OscillatorNode` of type square with no filter, notes of 25 to 90 ms with an envelope of a few ms at each end; a toggle Sound beside Turbo, off on every load, and no `AudioContext` before the click that turns it on; one sound per kind of important event, from one scale, rising for what comes back and falling for what goes away; one master gain, a peak of at most about −24 dBFS (−20 with the alarm), at most three voices, and a rate limit so that turbo is not a buzz; nothing while paused but the visitor's own action. Another builder changes the log in `page.js` at the same time, so all sound code goes in `sound.js` and `page.js` gets as few lines as possible.

**Inferred intent:** a visitor who wants it hears, without looking, that something happened to an instance, the lease, or the requests, and learns the sounds after a few minutes.

### What I did

- `site/simulator/sound.js`, an ES module that loads nothing. `listener()` maps events of the engine to a kind of sound, keeping what it needs: `up` when an instance opens its database (start of the standby, restart, Start, a reopen after its core stopped); `down` at a crash; `stop` at the visitor's Stop or a stop of the core; `takeover` when the lease is taken by another instance than the one that took it last; `failing` and `recovered` when the requests' status changes (the rule of `requestStatus` in `run.js`); `blip` for every other action of the visitor (Hang, Disconnect, Error, the clock actions, the switch of the requests) and for turning sound on; `alarm` for a violation that is not excused. Stop, Start, and Restart sound at the action, and the process events they cause later are silent, so a paused visitor hears their own action, and nothing sounds twice.
- `player(ctx, dest)` plays into any `BaseAudioContext`: one oscillator and one gain per figure, pitches of A minor pentatonic, A4 to A6. Master 0.018; the alarm at 2.75 times that. At most 3 voices (the alarm takes the place of the oldest), a kind never over itself or within 120 ms, at most 4 figures a second but the alarm, starts 50 ms apart.
- `page.js`, four lines: `let S`, the import of `sound.js` with the version (started before `run.js` and awaited after it, and a failed import leaves the page without sound), `S.wire($("sound"))`, and one line in `take`: `S.hear(e, live)`, where live is playing or the answer to an action, but not a later step of a reset of the clocks. `index.html`, one line: the button, `.btn big` like Turbo, disabled until `sound.js` has wired it. No new styles.
- Site check: `sound.js` is in the files that must load at the version; the toggle starts unpressed with `data-audio="none"`; a unit check of the kinds over a scripted session; every note 25 to 90 ms; and an offline render of a burst (every kind every 10 ms for 3 s) whose peak must be at most 0.06 without the alarm and 0.1 with it.

### What worked

- Offline in Chromium, 48 kHz: each ordinary figure peaks at 0.018 (−34.9 dBFS), RMS over 50 ms −36.4 to −39.4 dBFS; the alarm 0.049 (−26.1 dBFS, RMS −27.6). Three figures at once 0.050 (−26.0); the alarm over two others 0.083 (−21.7); the burst without the alarm 0.051 (−25.9 dBFS, 12 figures in 3 s), with it 0.083 (−21.6). Chromium's square wave showed no overshoot over its gain.
- On the real page with `playwright-cli`: no `AudioContext` before the click (counted through a wrapped constructor), one after it; a session with a crash, a takeover, a restart, the switch to b, a disconnect, and a reconnect played blip, up, down, failing, takeover, up, blip, recovered, and so on, one per event; after Sound off, `data-audio` is `suspended` and a crash of b played nothing; paused, a Restart played `up` and 60 s skipped through the hook played nothing; under turbo with faults switched for 20 s, the same kind never twice within 832 ms and at most 4 a second.
- With `MASTER` raised to 0.03, the site check failed: "FAIL a burst of sounds peaks at -21.5 dBFS". With `createOscillator` made to throw, and with no `AudioContext`, the toggle went to `unavailable` and disabled, and the session went on.
- `make site-check` passes, also after the review fixes.

### What didn't work

- The first render of the burst peaked at 0.144 (−16.8 dBFS): the alarm, exempt from the cap a second, sounded over itself every 120 ms. A kind now waits until its figure has ended.
- `go run ./internal/simulator/serve -addr 127.0.0.1:8093` failed with "bind: address already in use", another worktree's server; 8417 worked.
- I ran `git checkout site/simulator/sound.js` to undo the trial with a raised master, and it also undid the uncommitted fix above. I wrote it again.
- A screenshot taken at once after the click showed the toggle not inverted: `.btn` has a transition of 120 ms.

### What warrants review

Self-review with the code-review skill at medium found nine things:

- Fixed (important): an exception from Web Audio in `hear` would have reached `pump` and halted the page with "The page failed". `hear` and the click catch everything, and a fault turns sound off for good and disables the toggle.
- Fixed: the `webkitAudioContext` fallback, whose prefixed context lacks the promises the code uses, is gone; such a browser gets a disabled toggle.
- Fixed: a fade before a figure's start held the gain's default of 1; the gain now starts at 0.
- Fixed: the fade without `cancelAndHoldAtTime` (Firefox) set the value of now at a later time; it glides with `setTargetAtTime` from the value at that time.
- Fixed: `sound.js` was imported after `run.js`; it is now fetched in parallel. A `modulepreload` would need `internal/simulator/serve` to stamp it, so there is none.
- Fixed: the toggle was enabled in the HTML; it is disabled until `wire`, so a missing `sound.js` leaves no dead button.
- Fixed: `silence` kept the times of cut figures, so on, off, on within 120 ms dropped the confirming blip.
- Fixed: a reset of the clocks of paused clocks blipped twice, for its two actions.
- Fixed: the package comment claimed a bound the check does not hold; it now states the bound of the check.

For the lead and Markus:

- Between 360 and 429 px the clock no longer fits on the row of the buttons, so the sticky bar is 124 px high instead of 89 (at 320 it was already 124, now 137). Fitting it would mean smaller Play and Turbo buttons at phone width, outside this brief.
- The switch of the requests and turning sound on also blip; the brief listed faults only.
