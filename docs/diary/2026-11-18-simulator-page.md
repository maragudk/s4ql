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
