# Diary: the simulator page remembers Sound and Turbo

Markus: "Also, persist sound and turbo button toggle state". He reloads the simulator page (`/site/simulator/`) often and has to press Sound again each time. This diary is of the builder that makes the page remember the two toggles in the browser's `localStorage`.

## Step 1: The brief

**Author:** builder (persist toggles)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> 1. The state of the toggles Sound and Turbo is remembered in `localStorage`, so that it holds across reloads, tabs, and visits in the same browser. A first visit is as today: Turbo on, Sound off.
> 2. Storage is a convenience. Reading and writing are wrapped so that a browser that refuses storage (private window, blocked site data, a throwing accessor, a garbage value) gets the page exactly as it is today, with nothing in the log about it.
> 3. Turbo stored as off: the page starts with Turbo off, in the button and in the speed, before the first Play.
> 4. Sound stored as on: a browser does not start audio without a user gesture, so the page cannot sound at load. It starts sound with the visitor's first click or key on the page, whatever it is aimed at (typically Play), creating the context inside that gesture as `wire` does today, so that the sounds of that very click and everything after it are heard. Until the context runs, the button must not claim that sound is playing: it shows a third, visibly different state (for example the LED not lit steadily), and its accessible state says so too. Once the context runs it is the ordinary "on". If the visitor's first click is on the Sound button itself, decide what is least surprising and say what you chose; it must not need two clicks to get from "remembered on, waiting" to a definite state.
> 5. Reset resets the simulation and leaves both toggles as they are, in the page and in storage.
> 6. `sound.js` stays the owner of sound and of the Sound button's wiring; the dancer keeps to its rule (no dancing until the context runs). Under `prefers-reduced-motion` nothing changes. No new dependencies or network requests.
>
> Out of scope: persisting anything else; anything else in `site/`, `README.md`, `docs/roadmap.md`, `docs/spec.md`. Verify by extending the site check (first visit has the defaults; each toggle comes back after a reload; Sound remembered on shows the waiting state, makes no context before a gesture, and runs after the first click on something other than Sound; Reset keeps both; throwing storage gives the defaults), without timing and without leaks between steps through `localStorage`; `make site-check` and `go test -tags s4ql_browser ./internal/simulator/...`; screenshots at about 1440 px of the three Sound states and of Turbo off after a reload.

**Interpretation:** two items in `localStorage`, one read and written by `page.js` for Turbo and one by `sound.js` for Sound, each behind a `try`. A remembered Sound on puts the button in a third state, `aria-pressed="mixed"`, and a capture listener on the document turns sound on at the first click or key that is not aimed at the Sound button. The site check gets a way to count the page's audio contexts from before its scripts run.

**Inferred intent:** a page that comes back as Markus left it, without ever pretending to sound when the browser will not let it.

### What I did

Reset the worktree onto `worktree-agent-ae6852d627a72f174`, the branch with `wake` in `sound.js`, and read `AGENTS.md`, `/site/simulator/sound.js`, `/site/simulator/page.js`, the buttons in `/site/simulator/index.html`, `/site/simulator/bucket.js`, `/internal/simulator/sitecheck/check.html` and `main.go`, and the diaries of the sound bug and the dancer of today.

### Why

To find where each toggle is decided and how the check drives the page before changing either.

### What worked

The reading. Reset already leaves Turbo alone (`reset` in `page.js` never calls `setTurbo`) and never touches Sound; it only empties the tab's `sessionStorage`, so items in `localStorage` survive it without code.

### What didn't work

Nothing yet.

### What I learned

The check page and its frames share an origin, so they share `localStorage` as they share `sessionStorage`; the check must clear the two items between its steps. Chromium's profile is new for each run of sitecheck (`--user-data-dir`), but a check that clears at its start does not rely on that.

### What was tricky

The first click on the Sound button while it waits: a capture listener on the document runs before the button's own handler, so without care the listener would turn sound on and the handler would turn it off again. The same goes for Enter or Space on the button, whose keydown comes before the click it makes.

### What warrants review

Nothing yet.

### Future work

None yet.
