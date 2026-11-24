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

## Step 2: Build, check, and self-review

**Author:** builder (persist toggles)

### Prompt Context

**Verbatim prompt:** the brief of step 1; no new message.

**Interpretation:** build the two items, the waiting state of Sound, and the check, red first.

**Inferred intent:** as in step 1.

### What I did

- `/site/simulator/page.js`: Turbo is read from `localStorage` item `s4ql:turbo` at the top of `start`, before anything draws or plays (`setTurbo(turboRemembered())`), and written in the click of the button. Only `"off"` is off; anything else, or a throwing accessor, is on.
- `/site/simulator/sound.js`: Sound is item `s4ql:sound`; only `"on"` is on. The making of the context moved out of the click handler into `open`, unchanged. A remembered on sets `aria-pressed="mixed"`, `data-audio="waiting"`, and a `title`, "Sound starts at your first click or key on the page.", which is also the button's accessible description, and adds `first` as a capture listener for `click` and `keydown` on the document. `first` makes the context in that gesture, before the page's own handler runs, sets sound on, asks to resume, and plays the blip of sound on. The button stays `mixed` until the context's state is `running`; then `settle` makes it the ordinary on. A click or key aimed at the Sound button is left to the button, whose click from the waiting state turns sound on (`on = waiting || !on`), as from off. `broken` ends the wait.
- `/site/simulator/index.html`: `.btn[aria-pressed="mixed"] .led` draws a dot in the square, the button not inverted, and blinks it with `steps(1)`; the page's rule for reduced motion stops the animation, so there the dot holds.
- `/internal/simulator/sitecheck/main.go`: the variant `no-storage` now makes `localStorage` throw as well as `sessionStorage`, and a new variant `audio` serves the whole page with a script first in its head that counts `new AudioContext()` in `window.s4qlContexts`. The two scripts sit in a map, `heads`.
- `/internal/simulator/sitecheck/check.html`: `forget` removes both items at the start of the check and before `persistence`; a new step `kept` checks, on the `audio` variant: a first visit; Turbo off after a reload, in the button and in the speed of the first step; Sound on after a reload waiting for 30 frames with 0 contexts and a still figure; the three looks of the button differ; the first click on Play makes exactly one context synchronously in the click and one blip, and the button becomes on once the context runs, with the figure dancing; Reset keeps both, in the page and in storage; a keydown of Enter on the Sound button leaves it waiting and its click turns it on at once; a key elsewhere starts it, but Meta, Shift, Escape, and Tab do not; Turbo turned on again; garbage values (`"fast"`, `"true"`) give the defaults; and the `no-storage` variant gives the defaults, toggles that work, and no line about them in the log. It ends with `forget`.

Results: `make site-check` passed (both runs, without and with a version), `go test -tags s4ql_browser ./internal/simulator/...` passed, the site check passed six times in a row and four times in parallel. Screenshots are in `/private/tmp/claude-502/-Users-maragubot-Developer-s4ql/3a1f6409-2444-4c23-8123-ed5164204cd4/scratchpad/persist-toggles/`.

The code-review skill (medium) over `abdc10d...HEAD` reported three things. One was right and important: `first` took any keydown, so the Command of Command+Tab, which reaches the page, would use up the one-time start and make a context outside a gesture that the browser leaves suspended. `first` now ignores Shift, Control, Alt, AltGraph, Meta, OS, Fn, CapsLock, Escape, and Tab, and the check presses Meta, Shift, Escape, and Tab before a real key. With the list emptied, the check fails with "FAIL a key on the page starts Sound that waits, and Meta, Shift, Escape, and Tab before it do not (… 1 contexts …)". The two others I left: a resume that the browser refuses after a real gesture leaves the button waiting, but `wake` resumes at every later click or key, so it does not stay; and Turbo can show on for a moment before `page.js` runs, while the button is still disabled.

### Why

`localStorage` holds across tabs and visits, which `sessionStorage` (the tab's S3) does not. The capture phase on the document is the only place that sees a click before Play's handler, so that the context exists when the page sounds the click's events. `mixed` is the one value of `aria-pressed` beyond on and off, and the title says what it waits for, to a mouse and to a screen reader alike.

### What worked

Counting contexts from a script in the head of the served page, which runs before any module of the page, so "no context before the gesture" is a number. Importing `sound.js` through the frame's own `Function` (`new p.win.Function("u", "return import(u)")`) gives the page's module instance, and with it `stats`.

### What didn't work

- The first run of the new step: `FAIL the check could not go on: TypeError: toggles is not a function`. `check` has a local `const toggles` (the faults of an instance) that shadowed my function; I renamed it `kept`.
- Before the code, the check hung, as expected for red: "sitecheck: the check did not end within 3m0s", waiting for sound that never started.
- "FAIL the button Sound looks different off, waiting, and on (off: rgb(0, 0, 0) | none / on: rgb(0, 0, 0) | none / …)": the button's `transition: background-color 120ms steps(1)` was still running when the check read the style. The check now sets `transition: none` on the button while it reads, which gives the state's look without waiting on time.
- A first screenshot of the waiting state had the hover frame, because the mouse stayed on Sound; I took it again with the mouse away.

### What I learned

A capture listener on the document runs before the target's own listeners, so the button's own click must be left out of `first`, or one click would turn sound on and the button's handler off again. Enter and Space on a button come as a keydown first, so a key aimed at the button is left out too.

### What was tricky

The other builder's change to the line "S3 still holds" in `page.js` and in `check.html` near `persistence`: I kept my hunks away from both, with `forget()` called in `check` before `persistence` and not inside it.

### What warrants review

- `first`, `settle`, and `unwait` in `/site/simulator/sound.js`, and the click handler's `on = waiting || !on`.
- `kept` in `/internal/simulator/sitecheck/check.html`, and `heads` in `/internal/simulator/sitecheck/main.go`.
- By hand: `make site-serve`, turn Sound on, reload, see the blinking dot, click Play, hear the blip and the session.

### Future work

- If a browser refuses the resume after a real gesture, the button waits with sound already on in the module, and figures queue on a stopped clock until a later click resumes it; the player's limits bound what then sounds at once. Not seen in Chromium.
