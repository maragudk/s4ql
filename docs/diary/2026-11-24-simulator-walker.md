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
