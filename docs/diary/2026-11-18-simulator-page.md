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
