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
