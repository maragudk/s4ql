# Diary: the simulator's stick figure walks along the line

The stick figure of the simulator page (`site/simulator/`), added in pull request #134 and described in `/docs/diary/2026-11-24-simulator-dancer.md`, dances to the sounds and otherwise stands still on the top line of the frame around Log and Commit objects. Markus: "make the stick figure be able to walk along the line when not dancing." This diary is of the builder that teaches it to walk.

## Step 1: The brief

**Author:** builder (simulator walker)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> 1. When it is not dancing, the figure strolls along the top line of the frame: it stands a while, walks a stretch, stands again, and turns around at the ends. The ends are the corners of the frame; no part of the figure goes past them, in any pose, and its feet stay on the line. A stroll, not pacing: more standing than walking, at a leisurely speed, with stretches and pauses of varying length.
> 2. Walking is drawn as walking: a walk cycle of its own in the figure's pixel style, facing the way it goes, with steps whose length matches the distance it moves, so that the feet do not slide. It moves by whole pixels of the figure's grid, so that it stays crisp.
> 3. "Not dancing" includes sound off, the page paused, and before the first Play: it strolls in all of them. This replaces the earlier rule that it stands still then.
> 4. Dancing wins. When a sound comes that it would dance to, it stops where it is, dances there, and afterwards carries on from that spot. A crash still drops it flat on the line where it stands, clamped so that the lying figure stays inside the ends. After a move it does not jump back to an old place.
> 5. Under `prefers-reduced-motion: reduce` it stands still at its old place, 40 px from the right end, as now. When the simulation has halted on a failure, decide what suits the page (standing still is the safe choice) and say what you chose.
> 6. It keeps being decoration: `aria-hidden`, no focus, no pointer events, nothing else on the page moves or resizes, no horizontal page scroll at any width. It works at phone width, where the line is short, and when the window is resized while it is anywhere on the line. Check that nothing else occupies the strip above the frame that it walks through, at phone, middle, and desktop widths.
> 7. It costs next to nothing: it rides on the page's existing animation frame, changes only a transform and the pose, and does no work while the tab is hidden.
> 8. The walk may use chance for its pauses and stretches, but the code must let the check drive it without waiting on chance (a seam for the source of randomness or for the next decision is fine; no test-only branches in the behaviour itself). No new dependencies or network requests. The sounds and `sound.js`'s behaviour do not change.
>
> Out of scope: new dance moves, anything else on the page or in `site/`, `README.md`, `docs/roadmap.md`, `docs/spec.md`. Verify by reworking the dancer's steps in the site check (with sound off or paused it never dances but walks when told to, turns at each end, never leaves the ends; with sound on and playing a sound stops the walk and starts a move at the spot; a crash leaves it flat inside the ends; under reduced motion it stands at its old place), without timing or chance; `make site-check` and `go test -tags s4ql_browser ./internal/simulator/...`; screenshots with playwright at about 390 and 1440 px of the walk cycle enlarged and of the figure at the ends, mid-walk, and flat after a crash near an end.

**Interpretation:** a stroller in the section "The dancer" of `page.js`, driven from `dance(now)` on the page's animation frame: a state of standing or walking, a position in whole pixels of the figure's grid kept as a transform, a walk cycle drawn from joints like the poses, and decisions of how long to stand and how far to walk from a function that the check can replace. The dance keeps its rules and takes over from the walk where the figure is.

**Inferred intent:** the figure is alive also for a visitor who never turns sound on, without becoming a distraction or a cost, and without weakening what the check can prove.

### What I did

Reset the branch onto `worktree-agent-a94110cb895296bf5` and merged `main` into it. Read `AGENTS.md`, the diary of the dancer, the section "The dancer" of `/site/simulator/page.js`, the CSS and markup of `.logs` and `#dancer` in `/site/simulator/index.html`, the steps `dancer` and `soundWakes` and the use of `#dancer` in `kept` in `/internal/simulator/sitecheck/check.html`, and the variants of `broken` in `/internal/simulator/sitecheck/main.go`.

### Why

What the reading gave:

- `dance(now)` runs from `frame`, which starts only once the engine is ready, and never while the tab is hidden, since the browser runs no animation frames then.
- `data-moves` counts every change of pose; `kept` and `dancer` use it to say that the figure has not danced. Once the figure walks, that count has to mean changes into a dance pose only.
- The page reads `prefers-reduced-motion` once, as a `MediaQueryList`, so the check cannot switch it from outside. A variant of `broken` that puts a script first in the page's head, as "audio" and "no-storage" do, can give the page a `matchMedia` that says reduce.

### What worked

Nothing built yet.

### What didn't work

Nothing tried yet.

### What I learned

The frame's `right: 40px` is the old place; keeping it in the CSS and moving the figure by a transform from there means that reduced motion needs no position of its own.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: The stroll, its walk cycle, and a check that leaves the decoration alone

**Author:** builder (simulator walker)

### Prompt Context

**Verbatim prompt:** (as in step 1, and then a change from the lead, mid-build) "Change of requirement from Markus: checks for decoration are overkill. So for the walker: Do not rework the dancer's step of the site check for the new rules and do not add checks of the walk. Remove the step `dancer` from the site check instead, with whatever in `main.go`'s package comment describes it. Drop requirement 8's seam for driving the walk from the check if nothing else needs it; chance may simply be `Math.random`. Remove hooks in the page that existed only for the dancer's check, such as `data-moves`, unless a remaining step uses them. The steps `soundWakes` and `kept` are not decoration [...] Keep them passing. Where one of them asserts that the figure stands still and the walk now breaks that, change that one assertion to what still holds [...] with the smallest edit that keeps the step meaningful. If you have already built the reworked checks, delete them [...] You still verify the walk yourself by hand with playwright (ends, turning, a sound stopping it, a crash near an end, reduced motion, resize, phone width) and say what you saw in the report, and `make site-check` must still pass."

**Interpretation:** build the stroll as planned, without a seam, and verify it by hand; cut the site check down to what checks sound and the toggles.

**Inferred intent:** the figure is charm, and its correctness is judged by eye, not by the check that guards the simulator.

### What I did

- `/site/simulator/page.js`, section "The dancer": a walk cycle of 12 frames, `walk0` to `walk11`, made from the tables `LEG` (knee and foot by the phase of a leg) and `SWING` (the arms), with the head a pixel forward and the body a pixel lower in the two frames with both feet down; mirrored for walking left. `figurePath` now also gives the first and last lit column of a pose. A `walker` holds the place `x` in pixels of the grid from the old place (the CSS keeps `right: 40px`, and a `translateX` moves it), the facing, and the state of a stretch. `stroll(now)` decides with `strollChance` once a stand is over and walks a frame each `PACE` (110 ms, a pixel a frame, 18 px a second), a step being 6 pixels; it stops in a frame with both feet down when its steps are done or the next step would pass an end, and stands one pixel on. `dancerPlace` moves the figure in until every lit pixel of its pose is on the line and keeps it there. `dance(now)` stands still when halted or under reduced motion (at the old place), drops a dance while paused, lets `danceOn` dance, and strolls otherwise. A `ResizeObserver` on the frame gives the width of the line. `data-moves` now counts dance poses only, since `kept` and `soundWakes` use it.
- `/site/simulator/index.html`: the comment of `.dancer`.
- `/internal/simulator/sitecheck/check.html`: removed the step `dancer`; in `soundWakes` the figure has to show a dance pose and then stop dancing, instead of standing; one message of `kept` says dance poses.
- `/internal/simulator/sitecheck/main.go`: the package comment and the comment of the autoplay flag.
- Verified by hand in playwright, at 390 and 1440 px, with a `MutationObserver` that logged every pose and place (scratch helpers in the session's scratchpad, not committed).

The pacing of `strollChance`: after a walk or a dance it stands 3 to 12 s; after the page loads, 1.5 to 6 s; after a stand it walks 1 to 6 steps (6 to 36 pixels of the grid, 0.7 to 4 s), turning two times in five, or one time in five stands on.

### Why

- **A pixel a frame, a step of 6.** The foot on the line moves back by a pixel each frame against the figure, which moves forward by one, so it stays put on the line; feet slide only if the two disagree. Six pixels between the feet is a step of a third of the figure's height, a leisurely stride, and at 110 ms a frame it is 90 steps a minute.
- **Stopping only with both feet down.** Any other frame would need a jump of up to three pixels into the stand. So it turns up to one step short of an end: at 390 px it came within 7 px of the left corner and 1 px of the right.
- **Moving a pose in, and keeping it there.** Dance poses reach wider than the walk (the point to column 13, the fall from 0 to 14), so near an end they are moved in, and the figure stays where the pose put it, so that it never jumps back.
- **Standing still on a halt.** The page then shows its failure; a figure walking past a failure box would be odd, and standing where it is avoids a jump.
- **The place anchored to the right end.** The old place needs no position of its own, and a resize keeps the figure's distance from the right end, clamped at the left.

### What worked

The log of the `MutationObserver` made each claim checkable: no frame of the walk without a foot on the line in the same place as the frame before (0 of 175, and 0 of 282 in another run), the bottom of the SVG always on the line, and the lit pixels never past the corners. A sound while walking: `-walk6@125 point@125 stand@125 -walk1@125`, the same place throughout. A crash of b at the right end of 1440 px: `slump@1365 flat@1361`, moved in by two pixels of the grid, its last lit pixel 1 px inside the outer corner. Under emulated reduced motion, with sound on and playing for 6 s, it stood 40 px from the right end with no transform. Halted (storage that refuses writes, so the page stops as S3 full) it stood still and asked chance nothing for 15 s. Nothing else is in the 37 px above the frame at 390, 800, or 1440 px (`elementsFromPoint` over the band), and the page has no horizontal scroll at those widths. `make site-check` passed: `sitecheck: ok` twice, 1:14 for both runs.

### What didn't work

- Steering the page's `Math.random` with a fixed pattern of values to make long stretches got out of step with the calls of `strollChance` after a reduced-motion rest, and the figure stood for over 30 s: `TimeoutError: page.waitForFunction: Timeout 30000ms exceeded.` A constant `Math.random = () => 0.5` (4 steps, no turn, 7.5 s stands) worked.
- A crash of a while sound played gave the shrug of failing requests and no fall: both have rank 4 and the newer takes the waiting slot. That is the dance's rule from before; a crash of b, which serves no requests, falls.
- Screenshots: `playwright-cli eval` was refused by the worktree guard ("runs a string through eval"), so everything went through `run-code`; a crop came out under the page's sticky controls because the frame was scrolled to the top of the viewport; `page.screenshot: Clipped area is either empty or outside the resulting image` after a click had scrolled the page, and again because of smooth scrolling.
- The 390 px strip was cut to the width of the viewport at first, and was redrawn wrapped.

### What I learned

With Turbo on, sounds come so often that the figure dances all the time and never strolls; at 1x it dances most of the time too. The stroll is mostly for a visitor without sound, or paused, which is the case Markus asked about.

### What was tricky

Keeping the walk, the dance, and the place apart: the dance's least hold of 200 ms must not slow the walk's frames of 110 ms, so only dance poses set `dancer.since`; and every pose goes through `dancerPlace`, so that a pose that reaches past an end can never be shown there.

### What warrants review

- `/site/simulator/page.js`, `stroll`: the stop rule (`room(w.dir, STRIDE + 1)` in frames 0 and 6) and the start from frame 1 at the same place, which keep the feet from sliding at either end of a stretch.
- `span`: the arithmetic of the ends, from `right: 40px` and the 30 px box.
- The pacing of `strollChance`, which is taste.

### Future work

None.
