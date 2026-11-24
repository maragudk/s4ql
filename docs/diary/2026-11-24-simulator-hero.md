# Diary: a one-line hero, a description at the foot, and stars over the whole simulator page

Markus asked for the top of the simulator page to shrink to one line with the stars behind it, for the longer description to move above the footer, for the sky to cover the whole page except the navigation, and for the note above the SQL box to go. This diary is of the builder that reshapes `site/simulator/index.html` for it.

## Step 1: The brief

**Author:** builder (simulator hero)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to Markus's words and the requirements)

> In his words:
> - "Update the whole header section to just something like: 'Try the real s4ql compiled to WebAssembly.', and it should also have the stars as background. Then add a longer description just above the footer instead. And the stars background should be everywhere on the sim page except header."
> - "Also, drop this: 'One statement of what s4ql supports so far, see the roadmap. The app's table barcodes is there to read; make tables of your own to write.'. It's better to use the design to make usage self-evident where possible. Minimize words."
> - About the stars as they are now: "I like the stars".
>
> 1. The hero. `section.intro` shrinks to its headline alone: the `h1` reads "Try the real s4ql, compiled to WebAssembly." [...] It keeps the weight of a hero (the big type, room around it) and has the sky behind it, with stars in its empty space.
> 2. The description. The two paragraphs and the "Built from" line move, with their links and the element `#build` that the page's script fills, into a section of their own directly above the footer, below the SQL section, under a short heading of two or three words in the page's voice. Keep the text as it is [...] change only what the move makes wrong [...]
> 3. Stars everywhere but the navigation. [...] Everything Markus likes about them stays [...] Stars still never sit behind or against words, numbers, bar codes, controls, or the figure's line [...] the boxes with dense content [...] and the sticky bar stay opaque. Bands that are mostly empty space, such as the hero, become transparent so that the sky shows. Keep the cap on how many stars twinkle low [...]
> 4. The note above the SQL box (`#sql-note`) goes, with the `aria-describedby` that points at it. [...]
> 5. Because the headline changes, the page's link preview changes with it [...] make it again the same way, 1200 by 630, in the same style, with the new headline. [...]
> 6. Add this entry to the section "Memory" of `AGENTS.md` [...] "On the site, design before words" [...]
> 7. Everything keeps working: the links in the moved text, the script that fills `#build`, the stick figure, the sound, reduced motion (stars stand still), print and forced colours (no sky), phone width (16 px gutters, no horizontal scroll). The front page `site/index.html` is not changed [...]
>
> Out of scope: New copy beyond the headline and the section's heading. Changes to the stars' look. The panel, the SQL box's behaviour, the footer's content. `README.md`, `docs/roadmap.md`, `docs/spec.md`.

Another builder is removing the invariant checker at the same time, in `run.js`, `sound.js`, the `alarm` move in `page.js`, the CSS of `li.alarm`, and the site check's assertions on it; this work stays away from those.

**Interpretation:** all of the page's changes are in `/site/simulator/index.html`: the intro's markup and CSS, a new section after `#sql`, the sky's keep-out rules in its script (no longer everything above `.panel`, but `header`, the sticky bar, and the content boxes and text), the note and its `aria-describedby`, and the head's metadata. Plus a new `/site/simulator/og.png` drawn the way step 9 of `/docs/diary/2026-11-18-simulator-page.md` describes, the site check's assertion on the note, and the entry in `/AGENTS.md`.

**Inferred intent:** a page that opens with one confident line under the stars and lets the simulation speak, with the precise account of what is real kept for the reader who scrolls to the end.

### What I did

Reset the worktree to `worktree-agent-a2bb79ffd29a314ac` (the newest page, with the strolling figure), read `/AGENTS.md`, `/site/simulator/index.html`, the stars' diary, step 9 of the simulator page's diary on the preview image, and the site check's assertions on `#build` and `#sql-note`.

### Why

To know what the sky keeps out today, how the old image was made, and which checks name what moves.

### What worked

Nothing built yet.

### What didn't work

Nothing tried yet.

### What I learned

- The card page that made `/site/simulator/og.png` is not in the repository; it has to be written again from the diary's recipe, copying the front page's `rng` and `layout`.
- `impeccable`, which embedded the image's provenance, is not on this machine's `PATH`.
- The site check reads `#sql-note` and its link to the roadmap; that assertion has to change.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: The hero, the band "What runs here", the sky over the whole page, and the new card

**Author:** builder (simulator hero)

### Prompt Context

**Verbatim prompt:** (as in step 1)

**Interpretation:** build what step 1 planned, sweep the stars at many widths, look at the page at three, and draw the card again.

**Inferred intent:** as in step 1.

### What I did

In `/site/simulator/index.html`:

- `section.intro` holds only the `h1` "Try the real s4ql, compiled to WebAssembly." at `clamp(30px, 5.6vw, 80px)`, `max-width: 25ch` and `text-wrap: balance`, so that it breaks after "s4ql," on wide screens and in three lines on a phone, with `clamp(48px, 6.5vw, 96px)` above and below. The `h1` is `width: fit-content`, so that the sky keeps clear of its words and not of the whole band. Its old grid and the rules for its paragraphs are gone.
- A new `section.about` after `#sql`, heading "What runs here" in the style of "SQL" (32 px, bold), then the two paragraphs and the "Built from" line, word for word, with `#build` and their links. Nothing in them pointed "below", and the link "Send one of your own" goes to `#sql` as before, so no word changed. It has a rule at its foot and the padding of `.ask`; its heading and paragraphs are `width: fit-content` too.
- `#sql-note` and the `aria-describedby` that pointed at it are gone. The box keeps its hidden label, its statement, Run, and the keys; the form just starts with the statement, which looks whole.
- The opaque backgrounds are now on `.requests, .node, .box, .s3, .logs, .ask-box`: `.intro` and `.ask` became transparent. `header` and `.bar` were opaque already.
- The sky's script: instead of "everything above `.panel`" and "the band of `.ask`", stars keep clear of `header` (the whole band down to its rule), of the sticky bar where it stands before it sticks (from the hero's bottom for the bar's `offsetHeight`, so it is right whatever the scroll when a redraw comes), of the rules at the top and bottom of `RULED` (`.intro, .ask, .about`, read from their computed border widths), of `BOXES` (`#failure, .requests, .fork, .inst, .s3, .logs, .ask-box`), and of `WORDS` (`h1, .ask-head > *, .about h2, .about p, footer p, footer li`), each by its box and also by the box of a range over its text. The `ResizeObserver` watches the words too, since `#sql-to` and `#build` change their width alone.
- The cap stays at 12 that blink and 3 that glint. At 1440 by 900 four of them are in the first screen, three in the hero; below 960 px there are fewer candidates than the cap anyway (2 to 13).
- The head: `<title>` and `og:title` quoted the old headline ("the storage code under faults"), and are now "s4ql simulator: try the real s4ql, compiled to WebAssembly". `og:image:alt` follows the new card. The meta description and `og:description` still say what the page is, so they stay; `twitter:card` has no text.

`/site/simulator/og.png`, 64,751 bytes: a card page in the scratchpad, as step 9 of `/docs/diary/2026-11-18-simulator-page.md` describes, with the front page's `rng` and `layout` copied from `/site/index.html` (seed `0x7334716c`, density 1.1), the mark of "s4ql" in bars of 2 px, the headline in three lines at 64 px bold, "In your browser, with / S3 and time simulated." at 34 px, the address, and the panel of "a serves", "b stands by", and three commit objects whose bar codes are SHA-256 of "s4ql commit 1" to "3" (so their names differ from the old card's). Served with `python3 -m http.server 8264 -d <card dir>`, captured with `playwright-cli -s=ogcard run-code` at 1200 by 630, `scale: "css"`. `impeccable` is not on this machine, so the provenance went in as the same `tEXt` chunk `impeccable:prompt` that the old image had, written by a few lines of Python, with the new headline in it.

`/internal/simulator/sitecheck/check.html`: the assertion on the note became "the box has a label and keys".

### Why

Markus: "Update the whole header section to just something like: 'Try the real s4ql compiled to WebAssembly.' [...] the stars background should be everywhere on the sim page except header", and "Minimize words."

### What worked

- A sweep over 19 widths from 320 to 2560 px (a script in the scratchpad that collects every pixel of every star, every text range, every element with a background, a pseudo-element, or a drawing, and each border as a line of its own): every star is 10 px or more from all of them, none is in `header`, and `scrollWidth` equals the window's at every width.
- Scrolling 1440 px wide in steps of 20 px, every twinkling star inside the stuck bar's rectangle is under the bar (`elementFromPoint` gives the bar; 74 of 74).
- `page.emulateMedia`: under `reducedMotion: 'reduce'` 0 animations, 15 twinklers and the three paths still there; back to `no-preference`, 21 animations. Under `print` and `forcedColors: 'active'` the sky is `display: none`, and `block` again after.
- `make site-check`: exit 0, `sitecheck: ok` twice, no `FAIL`. `go test -tags s4ql_browser ./internal/simulator/...`: ok.

### What didn't work

- The first sweep counted the bordered sections `.intro`, `.ask`, and `.about` as solid boxes, so every star in them failed; the sweep now takes each border as a line.
- The second sweep found stars 7 to 9.7 px from the `h1` and the `h2`s, at 360, 480, 900, 960, 1280, 1440, 1600, 1920, and 2560 px: with a line height of 1.04 the glyphs reach about 3 px out of the element's box. The script now also keeps clear of the box of a range over each element of `WORDS`.
- The first card had every commit's bar code at the top left of the card: the card's `canvas { position: absolute }` meant for the field caught the bar codes too.

### What I learned

The sweep measures what a reader sees: text by its glyph boxes, not by the boxes of its elements. The sky's script must do the same wherever text is large or its lines are tight.

### What was tricky

The sticky bar. Its rectangle moves with the scroll once it sticks, and a redraw can come at any scroll, so the keep-out is computed from the hero's bottom, where the bar stands before it sticks; elsewhere the bar is opaque and covers the stars it passes.

### What warrants review

- `/site/simulator/index.html`, `BOXES`, `WORDS`, and `RULED` in the sky's script: new content on the page needs adding there.
- The card's second line, "In your browser, with S3 and time simulated.", is the old card's second line without "The real Go code", which the headline now says.

### Future work

- The card page that makes `og.png` is still not in the repository; this is the second time it was written from the diary's recipe.
