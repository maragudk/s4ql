# Diary: the simulator's stick figure reacts to a poke, and can be carried to another frame

The stick figure of the simulator page (`site/simulator/`) dances to the sounds and strolls along the top line of the frame around Log and Commit objects: see `/docs/diary/2026-11-24-simulator-dancer.md` and `/docs/diary/2026-11-24-simulator-walker.md`. Markus: "When I click/touch the stick figure, it does something cute or funny. Add different reactions. Maybe we can even drag the figure around on the panels?" This diary is of the builder that makes it react and lets the visitor carry it.

## Step 1: The brief

**Author:** builder (simulator dancer play)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> 1. Click or tap on the figure triggers a reaction. At least five distinct reactions, chosen at random, never the same one twice in a row. Each is a short pose sequence in the figure's own pixel style, made from new poses in the existing pose system [...] (several clicks within a short time escalate to a different reaction is a nice touch, optional). Keep the dry humour; no text bubbles or words (design before words).
> 2. A reaction interrupts whatever the figure does (stroll, stand, dance) and afterwards it goes back to its normal life; dancing to sound resumes as it does now.
> 3. Drag: pointer down on the figure and move picks it up (use Pointer Events so mouse, touch, and pen all work; distinguish a tap from a drag by a small movement threshold). While held, it shows a dangling/flailing pose and follows the pointer. On release it lands on the top line of whichever panel frame is nearest beneath/around the drop point (it may fall down to it). [...] find which frames exist on the page and allow all whose top line is a sensible standing place (at least the current one). It then strolls and dances on that panel's line, kept inside its ends as `dancerPlace` does now. A drop far from any panel returns it to the nearest one. Position need not persist across reloads.
> 4. Touch: dragging the figure must not scroll the page (touch-action on the figure), but the rest of the page scrolls as before. The figure needs a hit area large enough to tap on a phone (it is small), without changing how it looks.
> 5. Cursor hints the affordance (e.g. `grab`/`grabbing`). Keyboard users: the figure may be focusable and Enter/Space triggers a reaction; give it an accessible label. No visible text.
> 6. Reduced motion (`dancer.still`): reactions become a single still pose change or nothing jarring; drag still works but without falling animation.
> 7. No new dependencies, no build step changes [...] No new sound required [...]
> 8. Do not change other parts of the site. No README or site roadmap changes.
>
> Verify with playwright on desktop and a touch viewport: several pokes give different reactions (screenshots mid-reaction), a drag to each panel lands and strolls within the line, the page still scrolls on touch elsewhere, the figure still dances to sound after a reaction and after a drag, no console errors. Run the existing tests of the page. Self-review with the code-review skill.

**Interpretation:** new poses and short sequences of them, "pokes", played from the page's animation frame in front of the dance and the stroll; a press-and-move on an element around the SVG that carries it with a transform, and a drop that moves the element into the frame it lands on, where the stroll goes on with that frame's line.

**Inferred intent:** the figure turns from decoration into a small toy that rewards curiosity, without words, without costing anything while nobody touches it, and without weakening the checks of sound that use `#dancer`.

### What I did

Read `AGENTS.md`, the section "The dancer" of `/site/simulator/page.js`, the CSS and markup of `.logs` and `#dancer` in `/site/simulator/index.html`, the diary of the walker, and the uses of `#dancer` in `/internal/simulator/sitecheck/check.html` (`data-moves` in the steps `kept` and `soundWakes`). Served the site with `make site-serve SITEADDR=127.0.0.1:8137` and measured the frames of the page at 1440 and 390 px.

### Why

What the reading and the measuring gave:

- `dance(now)` runs from `frame` only once the engine is ready, so a poke before then has no frames to play in.
- `data-moves` counts dance poses for the check of sound; a poke's poses must not count.
- The framed boxes are `.requests`, the two `.node`s, the `.box`es of disk and clocks, `.s3`, `.logs`, and `.ask-box`. The room above them differs by width: 44 px above the nodes on a wide screen, 23 px above the disk and clocks, 48 px above S3 on a wide screen and 32 px on a phone, and the heading "SQL" stands above the left end of `.ask-box`. The figure is 36 px high. `.requests` has the sticky bar right above it.

### What worked

Nothing built yet.

### What didn't work

- `cd <scratchpad> && playwright-cli ... eval "..."` was refused by the worktree's guard ("this command runs a string through eval"); `playwright-cli -s=dz run-code "async page => ..."` run from the worktree works.

### What I learned

Nothing yet.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.
