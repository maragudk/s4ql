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
