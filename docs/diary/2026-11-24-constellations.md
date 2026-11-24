# Diary: constellations that follow the pointer on the simulator page

Markus asked for something on the simulator page that follows the mouse cursor and fits the page's vibe, and chose constellations: the few stars of the night sky nearest the pointer are joined by thin pixel lines. This diary is of the builder that adds them to `site/simulator/`.

## Step 1: The brief and the design

**Author:** builder (constellations)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> 1. While the pointer moves over the simulator page, the few stars nearest to it (three or four) are joined by thin lines into a small constellation. As the pointer moves on, old lines fade out and new ones appear. When the pointer rests, leaves the page, or is over the header, the lines fade away and the sky is as before.
> 2. The lines look like they belong to the sky: pixel-y (stepped, crisp, no anti-aliased diagonals), thin, and dimmer than the stars they join. Use the page's existing colour variables. Join only stars that are reasonably near the pointer and each other, so that no line runs across half the page.
> 3. The lines sit in the sky's layer, behind the page's content, and never take pointer events. They must not make any text harder to read: check the log, the commit objects, the SQL section, the "What runs here" section, and the footer. If a line behind text is distracting, make it not happen (for example, by not drawing lines that cross content boxes); you decide how.
> 4. Nothing in `header`, where there are no stars.
> 5. Off entirely where it makes no sense: devices without a hovering fine pointer (`(hover: hover) and (pointer: fine)` is the condition for on), `prefers-reduced-motion: reduce`, print, and forced colours.
> 6. It costs nothing when idle: no animation loop runs while the pointer is still or away. Pointer handling must not cause layout work per event; read star positions once and again only when the sky is laid out anew (resize). It must not slow the simulator, which runs a WebAssembly engine and updates the log on the same page.
> 7. No words: no label, hint, tooltip, or setting. Markus: "It's better to use the design to make usage self-evident where possible. Minimize words."
> 8. No new step in the site check (`internal/simulator/sitecheck`) for this. Markus does not want site-check steps for decoration. The existing site check and the simulator's tests must still pass; find the targets in the `Makefile` and run them.
> 9. Touch nothing else on the site, nothing in `docs/` apart from the diary, and nothing under `internal/` unless the build needs it. If the code fits better in a file of its own than in `page.js` or the head script, that is your call; note that unversioned files sit behind a 4-hour cache, so see how the page versions its script URLs and follow that.

**Interpretation:** the head script that places the stars already knows, at each layout, where every star is and where content is (its rectangles `out`, which keep stars clear of content). It publishes both on `#sky`, and a new module draws the constellations from them, so the pointer's handler reads no layout at all. The module is loaded by `page.js` with the deploy's version query, as `sound.js` and `bucket.js` are, so the site's stamping and its check of versioned requests cover it with no change under `internal/`.

**Inferred intent:** a quiet bit of play that rewards moving the mouse, in the page's pixel style, that never costs the simulation anything and never gets between a visitor and the words.

### What I did

Read the sky's CSS and head script in `/site/simulator/index.html`, the dancer's Bresenham routine in `/site/simulator/page.js`, how `internal/simulator/serve` stamps `page.js` and `run.js` with `?v=<version>`, and how `internal/simulator/sitecheck` checks that every `.js` under `/simulator/` is fetched with that query. Designed before writing:

- The head script's `draw` publishes `sky.layout`: each star's centre and reach in CSS px from the sky's top left, the rectangles `out`, the sky's place on the page, and the device pixel ratio with the pixel size `k`. It fires an event `layout` on `#sky`.
- A constellation is a function of one star, its root: the star nearest the pointer, if near enough. From it a Prim tree grows to at most four stars, among stars near the root, by edges that are short and do not cross any rectangle of `out`. So it changes only when the nearest star changes, with some hysteresis, and never runs behind content.
- Each constellation is a small SVG of its own, as big as its lines, drawn by Bresenham in pixels of `k` device pixels, with gaps at the stars. It fades in and out by stepped CSS animations of opacity, which the compositor runs; a timer removes it. A timer after the last move fades the lines when the pointer rests.

### Why

Publishing from the head script keeps the one place that knows the layout as the only place that reads it. A root-keyed constellation is stable under small moves and cheap to compute. Testing edges against `out` settles requirement 3 by construction.

### What worked

Nothing built yet in this step.

### What didn't work

Nothing has failed yet.

### What I learned

The site check already fails the page if any script under `/simulator/` is fetched without the version query, so a new module imported with `version`, as `sound.js` is, is versioned and checked without touching `internal/`.

### What was tricky

The header is not sticky, but the playback bar is: lines behind the stuck bar are hidden by its opaque background, as the stars are.

### What warrants review

The design choices above, in particular that the head script now publishes data on `#sky` for another module.

### Future work

None yet.

## Step 2: Building, checking by hand, and the self-review

**Author:** builder (constellations)

### Prompt Context

**Verbatim prompt:** (the same brief as step 1)

**Interpretation:** build the design of step 1, check it in a real browser with playwright, run the simulator's tests and the site check, and review the diff.

**Inferred intent:** a finished, reviewed branch that the lead can push.

### What I did

- `/site/simulator/index.html`: the head script's `draw` collects each shown star as `{x, y, reach}` in CSS px from the sky's top left (the centre of its middle pixel, or of the 2×2 block, and how far its pixels reach from there), and after drawing sets `sky.layout = { stars, out, left, top, dpr, k }` and fires `layout` on `#sky`. New CSS for `.sky .con`: `fill: var(--lumin)`, `opacity: 0.2`, a stepped animation in (`con-in`, 0.2 s, `steps(3, jump-none)`) and out (`con-out`, 0.8 s, `steps(4, jump-none)`), and `display: none` under `(hover: none), (pointer: coarse), (pointer: none), (prefers-reduced-motion: reduce)`. Print and forced colours already hide `.sky` whole.
- `/site/simulator/constellations.js`, new: the module as designed, with `MOST = 4`, `FEWEST = 3`, `REACH = 120`, `NEAR = 130`, `EDGE = 110`, `SWAY = 8`, `GAP = 4` px, `REST = 900` and `OUT = 800` ms. JS gate: `(hover: hover) and (pointer: fine) and (prefers-reduced-motion: no-preference)`, and touch pointer events are ignored. A pointer over `header` or `.bar` (opaque, and the bar sticks) counts as away.
- `/site/simulator/page.js`: `start` imports `./constellations.js` + `version` beside `sound.js`, and calls `start($("sky"))`.
- Served with `make site-serve SITEADDR=127.0.0.1:8197` and drove it with `playwright-cli` (`run-code`), at 1440×900 dpr 1, and in fresh contexts at dpr 2 (1300 then 860 wide) and with `hasTouch: true`.

### Why

The data flows one way, from the only code that reads layout to a module that never does, so pointer handling is a scan of about 250 stars from `e.pageX`/`e.pageY` in one animation frame per burst of moves, and DOM work only when the constellation changes.

### What worked

- Lines appear near the pointer and join three or four stars, pixel-stepped and crisp, dimmer than the stars; old ones step out as new ones come.
- Counted `.sky .con` elements: 1 shown while moving, 0 shown 1.1 s after the last move, none at all after 1.9 s; 0 shown over the header and over the stuck bar; 0 shown right after a `pointerout` with no `relatedTarget`, none after 0.9 s.
- `page.emulateMedia({reducedMotion: 'reduce'})` cleared a shown constellation at once and no new one came while moving; back to `no-preference`, they came again. A context with `hasTouch: true` reports `(pointer: coarse)`, and no constellation came from mouse moves or a tap. `emulateMedia({media: 'print'})` and `forcedColors: 'active'` hide the sky and with it the lines.
- At dpr 2, before and after a resize from 1300 to 860 px wide, zoomed screenshots show the lines' ends aimed at their stars with even gaps.
- Sweeping the pointer around the log and commit objects, the SQL box, "What runs here", and the footer: no line crosses a box or text, since edges that cross a rectangle of `out` are never made. The simulator played in Turbo the whole time (fake time and log lines went on), with no console messages.
- `go test -tags s4ql_browser -shuffle on ./internal/simulator/...` passed, and `make site-check` passed, including "every request for a file of the simulator has the query" with the new module fetched at the version. No change under `internal/`.

### What didn't work

`playwright-cli -s=con eval "..."` was refused by the harness of this sub-agent ("this command runs a string through eval, which can't be verified to stay inside the worktree"); `playwright-cli -s=con run-code "async page => ..."` with `page.evaluate` did the same job. A first `run-code` that referred to `document` outside `page.evaluate` failed with `ReferenceError: document is not defined`.

### What I learned

During eight seconds of play at 1440×900 the sky laid itself out zero times: the log and the commit objects scroll inside boxes of fixed height, so the simulation does not move the stars.

### What was tricky

An edge between two close stars can lie wholly inside their gaps and leave no pixels, which would make an SVG with an infinite size; edges shorter than both reaches plus both gaps plus 4 px are never made.

### What warrants review

The self-review (the `code-review` skill, at medium, over `main...HEAD`) found one important issue and six nits. Fixed: a relayout that changed only the rectangles `out` cleared a shown constellation at once with no step-out (important, though in practice play caused no relayouts); now a relayout with the same stars keeps the constellation if its root's tree is unchanged and steps it out otherwise. Fixed: a root that comes back while its constellation steps out drew a second SVG over the first; it now brings the first back. Fixed: the nearest star with no constellation (all its edges blocked) hid a neighbour's; the root is now the nearest star within `REACH` that has one. Fixed: the empty `.catch` in `page.js` hid errors thrown by `start`; now only a failed import is ignored. Fixed: this step of the diary. Left: line pixels snap to multiples of `k` device pixels from the sky's origin while stars may sit one device pixel off that grid at dpr 2; with the gaps the lines never touch the stars, and the zoomed screenshots show no visible offset. Left: after a wheel scroll without a move the lines scroll with their stars until the next (synthetic) move or the rest timer, which is consistent with the sky scrolling.

### Future work

None needed. Not verified: Firefox and Safari (only Chromium was driven), a real touch device, and dpr 1.5, where a star pixel is 2 device pixels of 1.33 px.
