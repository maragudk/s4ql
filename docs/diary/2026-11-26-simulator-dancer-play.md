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

## Step 2: Pokes, the drag, and the frames that hold the figure

**Author:** builder (simulator dancer play)

### Prompt Context

**Verbatim prompt:** as in step 1.

**Interpretation:** as in step 1.

**Inferred intent:** as in step 1.

### What I did

In `/site/simulator/page.js`, section "The dancer":

- 16 new poses in `POSES`, drawn and checked as ASCII from `figurePath` before they went in: `crouch`, `eek`, `trip`, `sit`, `dust`/`dust2`, `ball`, `upside`, `flourish`, `bow`, `cower` (and `shiver`, the same a pixel to the right, made in code), `cross`, `sulk`, `dangle`/`dangle2`.
- `POKES`: five by chance, never the one before: `startle` (crouch, a jump up to 7 pixels of the grid in `eek`, and down), `flop` (trips over backwards, lies flat, sits up with a hand to its head, gets up and dusts itself off), `flip` (a back flip, upside down at 8 pixels up, landing in `tada`), `bow` (a flourish and a deep bow), and `cower` (arms over its head, shivering, then a look round). `sulk` comes of 4 pokes within 3 s: arms crossed, then it sits with its back turned, hugging its knees, for 2.4 s. A poke has a lift per pose, shown as a `translate` up, so that it can jump without the grid growing. A poke with a front faces the way the figure faces; `sulk` faces away. A poke's poses do not count in `data-moves`.
- `#figure`, a `span` around the SVG, now carries the position and the transform, the pointer and key handlers, `role="button"`, and `aria-label="Poke the stick figure"`. The SVG keeps `id="dancer"` and its data attributes, which the site check reads.
- The drag: `pointerdown` captures the pointer; a move of 6 px picks the figure up, and it hangs by its head under the pointer, kicking (`dangle`/`dangle2` every 130 ms), placed by a transform from its frame, so that it never leaves the DOM while the pointer is captured. On release `dancerLand` picks the frame: the one its feet are in, or whose line is under its feet, the highest, and otherwise the nearest. The figure moves into that frame at once, keeps its place on the screen through a transform, and falls at 0.004 px/ms², sliding to the x where it lands, and crouches for 150 ms.
- `stretch` decides which frames can hold it, at the layout of the moment: the longest stretch of a frame's top line with nothing of `CROWD` (the frames, `.requests`, `.box`, the buttons of the edges to S3, the heading of SQL, the failure box) in the 36 px above it and 8 px beside it, if it is at least three figures long. The lines of the edges do not count, and the figure walks in front of them. `span` now takes the ends of that stretch, so that the stroll and `dancerPlace` keep it inside them. The frame of `.logs` always has its whole line. After a resize, a frame that has no room any more lands the figure on the nearest one, without a fall.
- `STAGES` is `.node, .s3, .logs, .ask-box`; `.requests` is left out because the sticky bar stands right above it. With the measuring, at 1440 px it can stand on both instances, S3, Log, and SQL (to the right of "SQL to a"); at 390 px on instance b, S3, Log, and SQL, while instance a, whose line has 31 px under the requests, does not hold it. The disk and the clocks never do: their edges to the instance are 24 px.
- Under reduced motion a poke is the one pose `still` of it for 1.2 s, the held figure does not kick, and a drop lands at once.
- `dance(now)` plays the hold, the fall, and a poke before anything else, also when the page has halted; `dancerHear` with sound off leaves them alone; `dancerRest` ends a poke.

In `/site/simulator/index.html`: the CSS of `.dancer` (the span now) with `cursor: grab`, `grabbing` while pressed or aloft, `touch-action: none`, no selection, a `::before` that widens what is hit to 44 by 44 px around and above the figure, not into the frame below, and `z-index: 10` while it is carried or falls; `.node, .s3, .ask-box` are positioned, so that they can hold it.

### Why

- Carrying the figure by a transform from the frame it stands on, and moving it to another frame only after the release, keeps the pointer captured all the way: moving an element in the DOM releases its pointer capture.
- Measuring the room above a frame, instead of listing frames by hand, gives the right frames at every width: a list by hand had to be wrong at either 1440 or 390 px.

### What worked

- Checking poses as ASCII art from a copy of `figurePath` in the REPL before putting them in the page, which caught a tangled first `sit`, `bow`, and `sulk`.
- Verification with playwright (scripts run with `playwright-cli -s=dz run-code`), against `make site-serve SITEADDR=127.0.0.1:8137`:
  - Six pokes in a row at 1440 px gave bow, cower, bow, cower, bow, flip, and seven more gave startle, bow, flop, startle, flip, bow, cower: never one twice in a row. Four quick clicks gave `sulk`. Screenshots mid-poke, enlarged: `eek` in the air, `flourish`, `bow`, `flat`, `upside` in the air, `tada`, `cower`, `sulk`.
  - Drags at 1440 px to S3, both instances, SQL at both ends (it landed right of "SQL to a"), the disk (it fell to S3), the heading of the page (it fell to instance a), and back to Log: each landed with its feet on the line (0 px) and inside the frame. 45 s on SQL: 518 frames of walking, never closer than 10 px to "to a".
  - At 390 by 844, touch, via CDP `Input.dispatchTouchEvent`: a tap 5 px beside and 6 px above the figure pokes it; a touch drag kept `scrollY` at 1565 the whole way and the figure dangled; a swipe elsewhere scrolled from 1565 to 1984. Drags to S3, instance b, and SQL landed there; to instance a and the clocks it fell to instance b.
  - With sound on and playing: 6 dance poses in 3 s, 3 more after a poke, none while held, 7 more after the drop on S3, where it danced `nod` and `point`.
  - Reduced motion: three pokes showed `-sit`, `eek`, `-sit`, each followed by `stand`; a drag showed `dangle` and then `stand`, at once on S3, where it stayed.
  - Resize from 1440 to 390 px with the figure on instance a: it went to instance b, which has room.
  - Keyboard: Tab reaches it, with the page's focus ring; Space and Enter poke it, and Space does not scroll. Its accessible name is button "Poke the stick figure".
  - No errors in the console in any of these.
  - The lines that meet instance a and S3 are the same, pixel for pixel, before and after the frames became positioned.
- `make site-check`: `sitecheck: ok`. `go test -tags s4ql_browser ./internal/simulator/...`: ok.

### What didn't work

- My first touch drags in playwright landed in odd places: `scrollIntoView` scrolls smoothly on this page, so the boxes I measured right after it were mid-scroll. `scrollIntoView({block: "center", behavior: "instant"})` fixed the test.
- `page.screenshot({clip})` with a box above the viewport: "Clipped area is either empty or outside the resulting image". With `fullPage: true` and coordinates of the page it works.

### What I learned

- Moving an element in the DOM drops its pointer capture, and with it the rest of the press.
- `e.detail === 0` tells the click that a screen reader sends to a button from a click of a press, which has a detail of 1 or more.

### What was tricky

- The fall: the figure moves into its new frame at the release, so its transform has to start at the offset of the drop from the new frame's old place, and run to the place where it lands, by whole pixels of the grid.
- How much room is enough: 36 px above the line, at the layout of the moment, with the edges of the diagram not counting.

### What warrants review

- `stretch`, `dancerLand`, and `falling` in `/site/simulator/page.js`, and the choice of `STAGES` and `CROWD`.
- The poses: whether they read as what they are meant to be at real size.
- That a poke and a drag work also when the page has halted, by choice: they are the visitor's.

### Self-review

The code-review skill (medium, over `main...HEAD`) reported seven possible problems. Fixed:

- A screen reader activates a button with a click and no keys or pointer: a click with `detail` 0 now pokes.
- A press held while a resize moved the figure to another frame would lose its pointer capture and stick, so that the figure took nothing more: `dancerRestage` waits while a press is on, and `lostpointercapture` ends the press.
- `.box` was in `STAGES` but can never hold the figure: removed, and kept in `CROWD`.
- Focus was lost when a resize moved a focused figure: it is focused again, without a scroll.
- The carried figure slid off the pointer when the page scrolled under a still mouse: a scroll places it again.

Left, as not worth the code: if reduced motion is switched on in the middle of a visit, the figure stands where it is and does not go back to its old place; and a stretch is measured again only on a resize, so words that grow into the room above a line later (the failure box, a longer "to a") are not avoided until the next resize.

### Future work

None.
