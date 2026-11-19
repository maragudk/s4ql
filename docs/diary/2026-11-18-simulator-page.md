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
