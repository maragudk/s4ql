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
