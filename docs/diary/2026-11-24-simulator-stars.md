# Diary: pixel stars that twinkle behind the simulator page

Markus: "Can we also add some pixel-y twinkling stars on the background on the sim page only?" This diary is of the builder that adds the night sky to `site/simulator/`.

## Step 1: The brief

**Author:** builder (simulator stars)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> 1. The simulator page, and only it, gets a night sky behind its content: sparse stars that are single pixels or tiny pixel shapes (a few pixels, a small plus), white or near white on the black, in the page's pixel style, crisp at any device pixel ratio.
> 2. They twinkle the pixel way: in steps (on, dim, off, or a plus that grows and shrinks by a pixel), not in smooth fades, each star in its own rhythm, most of them slow, so that the sky is quiet and something glints now and then. No two loads need to look the same, but the sky must not look like noise or like a pattern that repeats visibly.
> 3. They never get in the way of reading. No star shows behind text, numbers, bar codes, the log, tables, buttons, or the figure's line: boxes and bands that hold content are opaque over the sky, and the stars live in the margins and gutters. The stars are dimmer than the page's rules and text, so that the eye goes to the content first. Check this at phone, middle, and desktop widths [...]
> 4. It is decoration: `aria-hidden`, no pointer events, no focus, behind everything, no effect on layout, scrolling, or the page's width. Decide whether the sky scrolls with the page or stays fixed to the window by what looks right and costs least, and say what you chose.
> 5. Under `prefers-reduced-motion: reduce` the stars are there and do not twinkle. In print, and in forced-colours mode, there are none.
> 6. It costs next to nothing [...] prefer CSS that the compositor can run by itself (stepped animations of opacity or transform on a handful of layers) over script that repaints [...] Measure it: say what the page's frame time or CPU share is with and without the sky while the simulation plays in Turbo.
> 7. No new dependencies, no images or other network requests. Nothing changes in what the simulation does, in the sounds, or on the front page (`site/index.html`).
>
> Out of scope: shooting stars, a moon, parallax, stars that react to sound or to the simulation, and anything else in `site/`.

A later message from the lead: "Change of requirement from Markus: checks for decoration are overkill. Do not add a step for the sky to the site check [...] you still verify them yourself by hand with playwright and say so in the report. `make site-check` must still pass".

**Interpretation:** one decorative layer `#sky` in `index.html`, with its own block of CSS and a small inline script that places the stars where there is no content, so no new request and nothing in `page.js`, where two other builders work. Most stars are drawn once into one SVG; a few are small elements of their own whose opacity steps in CSS animations that the compositor runs. Verified by hand in playwright, not by the site check.

**Inferred intent:** a bit of charm for the page, in its pixel style, that never competes with the simulation for the eye or for the CPU.

### What I did

Read `AGENTS.md`, the diary of the dancer, `site/simulator/index.html`, the site check and its `main.go`, and the targets `site`, `site-serve`, and `site-check` of the `Makefile`.

### Why

To learn where content sits on the page, what has a background already, and how the page is served and checked.

### What worked

Nothing built yet.

### What didn't work

Nothing tried yet.

### What I learned

- The page already has a global rule under `prefers-reduced-motion: reduce` that sets `animation: none !important` on every element, so CSS animations of the stars stop there by themselves.
- `.bar` is sticky, so its box moves with scrolling; but everything above `.panel` is a band (header, intro, bar, alarm), so the sky can leave out all of it at once.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.
