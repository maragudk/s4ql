# Diary: a stick figure that dances to the simulator's sounds

The simulator page, `site/simulator/`, has sounds for the events of the session, off until the visitor turns them on (`sound.js`). Markus: "make a small stick figure that dances on the top line of the log/commit objects panel, to the sounds from the simulator. (Maybe it only dances with the sound on?)" This diary is of the builder that adds the figure.

## Step 1: The brief

**Author:** builder (simulator dancer)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> 1. A small stick figure stands on the top line of the frame around Log and Commit objects, feet on the line, drawn in the page's style: white strokes, crisp, about the height of a line or two of text. It is decoration: `aria-hidden`, not focusable, takes no pointer events, and does not move or resize anything else on the page.
> 2. It dances to the sounds: its moves are triggered by the same events that trigger the sounds in `sound.js`, so that a move lands with its sound, and different kinds of sound give visibly different moves. It is not a canned loop that merely runs while sound is on. [...] a bit of dry humour is welcome (a fault or a failure need not get the same move as a commit).
> 3. It dances only when sound is on. With sound off, paused, or before the first Play, it stands still on the line. When the sounds stop it returns to standing.
> 4. With Turbo on, sounds come fast: the figure must still read as dancing and not as flicker. Bound how often it changes pose.
> 5. Under `prefers-reduced-motion: reduce` it stands still whatever the sound does.
> 6. It works at phone width (no horizontal page scroll, no overlap with text that matters) and at desktop width.
> 7. No new dependencies, no new network requests; plain SVG, CSS, and the page's own JavaScript. Keep `sound.js` the owner of sound: if the figure needs to hear about sound events, give it a small, clear hook, not a second copy of the logic that decides when a sound plays. The sound itself must not change.
>
> Out of scope: anything else in `site/`, `README.md`, `docs/roadmap.md`, `docs/spec.md`; no changes to what the simulation does. Verify by extending the site check (figure exists, still with sound off while playing, changes pose with sound on while playing, stands again after pause), without timing; `make site-check` and `go test -tags s4ql_browser ./internal/simulator/...`; screenshots with playwright at about 390 and 1440 px.

**Interpretation:** an inline SVG in the frame `.logs` of `index.html`, positioned above its top border; a hook in `sound.js` that tells a listener each figure and tick that actually plays, with when it sounds; and in `page.js` a small dancer that maps each kind to a move, gated on sound on, playing, and motion allowed, with a least hold between poses.

**Inferred intent:** a bit of charm for a visitor who turns the sound on, that also makes the kinds of sound visible, without touching what the page or the simulation does.

### What I did

Read `AGENTS.md`, the two diaries of the page from 2026-11-23 and 2026-11-24, `sound.js`, the parts of `page.js` that drive playback and call `hear`, the CSS and markup of `.logs` in `index.html`, and the sound part of `internal/simulator/sitecheck/check.html`.

### Why

What the reading gave:

- A sound plays at the moment `hear` is called, at `ctx.currentTime` plus a lead of 10 ms and up to some `STAGGER` of 50 ms per figure that falls together. So the figure has to move when `player` decides to play, not at the fake time of the event.
- `player.play` and `player.tick` decide whether a sound plays (spacing, voices, budget per second), so the hook belongs right where they say yes; anything earlier would be a second copy of that logic.
- The page's visitor actions sound also while paused; the brief wants the figure still then, so the gate on playing lives in `page.js`.
- The site check never turns sound on, and an `AudioContext` that a script's click creates may stay suspended in headless Chromium, with `currentTime` stuck at 0, which makes the player drop nearly everything.

### What worked

Nothing built yet.

### What didn't work

Nothing tried yet.

### What I learned

The ticks (a scan per insert, a void per delete, a select, a miss, an error tone, the box's key) come at most every 40 ms, and the figures at most four a second, so under turbo the stream of ticks is what the figure must not follow one by one.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: The figure, its hook, and its check

**Author:** builder (simulator dancer)

### Prompt Context

**Verbatim prompt:** (as in step 1)

**Interpretation:** build what step 1 planned, look at it in a browser, and extend the site check.

**Inferred intent:** as in step 1.

### What I did

- `/site/simulator/sound.js`: `player(ctx, dest, played)` calls `played(kind, at)` when `play` or `tick` says yes, after all its limits; the page's player gets `tell`, which passes the kind and the delay in ms until the sound starts to the function that `watch(fn)` was given, only while the context runs, and `fn(null)` when sound goes off, by the toggle or by a fault. A fault in the watcher is caught there and never reaches the sound. What sounds did not change; the check's offline renders call `player` without the third argument.
- `/site/simulator/page.js`: a section "The dancer". Poses are joints on a grid of 15 by 18 pixels, drawn as one-pixel lines by Bresenham's algorithm and a ring for the head, into one path of 1 by 1 squares, so the figure is as crisp as the bar codes. Each kind of sound has a move of one or more poses with a rank; a move of one pose alternates with its mirror. `dancerHear` keeps the one move that waits; `dance`, from the page's animation frame, starts it once the pose has held `HOLD` (200 ms), drops it after `STALE` (300 ms) late, lets a move cut into another only from as high a rank, and goes back to standing after `REST` (450 ms). It stands at once while the page is paused or halted, under `prefers-reduced-motion: reduce`, and when sound goes off.
- `/site/simulator/index.html`: the SVG `#dancer` (30 by 36 px, `aria-hidden`, `focusable="false"`, `pointer-events: none`) is absolutely placed in `.logs`, now `position: relative`, 40 px from the right, with its bottom on the outer edge of the frame's top border, in the 48 px margin above the frame. It takes no grid cell and moves nothing.
- `/internal/simulator/sitecheck/check.html`: `dancer(p)`, after "Pause holds fake time": the figure is decoration and stands with its feet on the line; it stood still while the page played with sound off; with sound on and paused, still for 30 frames; Play, then wait until it has changed pose twice; Pause, wait until it stands, and still for 30 frames; sound off again. `/internal/simulator/sitecheck/main.go`: Chromium starts with `--autoplay-policy=no-user-gesture-required`, and the package comment says what the check now covers.

The moves, by sound: an insert's beep is the disco point; a delete's void brushes the item off the counter; a select nods; a select that finds nothing puts a hand to the brow and looks for the row; the error tone is a facepalm; the box's statement waves at the visitor; an instance that opens is a ta-da with both arms up; a crash buckles the knees and falls flat on the line; a stop is the raised hand of a traffic stop; a takeover shoves sideways and then points both thumbs at its own chest; requests that start to fail get a shrug; requests that work again get a fist pump with a knee up; a blip is a toe tap; the alarm is panic, arms flailing and jumping.

### Why

- **The hook after the player's limits.** The figure must move with what is heard, and the player is what decides that. A hook on `hear` would have had to copy the spacing, the voices, and the budget of the ticks.
- **The gate on playing in `page.js`.** `sound.js` plays the visitor's own actions also while paused; the brief wants the figure still then, and only the page knows whether it plays.
- **Only while the context runs.** A context that the browser holds suspended plays nothing, so nothing is heard to dance to.
- **A least hold of 200 ms, one waiting move, and ranks.** Under turbo the ticks come every 40 ms or so. With one waiting slot that the higher rank, or the newer of one rank, takes, a burst of scans becomes a point to the left, a point to the right, at most five poses a second, and a crash still gets its fall.
- **In `page.js`, not a module of its own.** A new file would be a new request, which the brief rules out.

### What worked

The pose preview in a scratch page (`preview.html`, served with `python3 -m http.server`) made the poses quick to tune before they went into the page. `make site-check` passed with the new lines, and six runs of the check at once, under that load, all passed. In playwright, under emulated reduced motion, the count of moves stayed the same for 3 s while the page played with sound on, and went on once motion was allowed again.

### What didn't work

- `playwright-cli open file:///…/preview.html` failed: `Error: Access to "file:" URL is blocked. Allowed protocols: http:, https:, about:, data:.` The preview went through a local server instead.
- The worktree's guard refused a shell command that wrote into the scratchpad with heredocs and `cd`: "this command is too complex to verify that it stays inside the worktree". The files went through the Write tool.
- The first crops of the figure at desktop width came out cut off: Playwright scrolled the page to click the buttons of a, and the crop's coordinates were from before. A viewport 2600 px tall, so that nothing scrolls, fixed it.
- One crop is labelled with a pose and shows the next one: the screenshot came after the pose had changed. It is a race of the capture, not of the page.

### What I learned

A script's `click()` on the Sound button creates an `AudioContext` that headless Chromium holds suspended without the autoplay flag; with it the context runs, and the player plays as it does for a visitor.

### What was tricky

The rank of a move that is resting in its last pose: if a resting move blocked lower ranks, a stream of scans after a ta-da went stale and the figure stood about; if every sound could cut in at once, turbo's ticks cut the fall of a crash short. Each pose now has a least time; after the last pose's least time any move may follow, and only `REST` later does it stand.

### What warrants review

- `/site/simulator/sound.js`: that `tell` adds no path by which the sound can change or fail.
- `/site/simulator/page.js`, `dance`: the order of the checks (hold, stale, start, next pose, rest).
- The check needs a running audio context in headless Chromium on CI's Linux; on this Mac it runs. If CI's Chromium cannot run audio, the line "sound is on, and its audio context runs" fails with the state it saw.

### Future work

None.
