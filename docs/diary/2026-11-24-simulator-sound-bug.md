# Diary: the sound of the simulator page that does not always work

Markus, on the live simulator page: "the sound doesn't always work (maybe only after a refresh)". Later he added that he sees it in Safari on macOS and does not know when it happens, and: "I think it started after we built persistent storage in the browser." This diary is of the builder that looks for the cause in `/site/simulator/sound.js` and the page around it.

## Step 1: Reproduce, and find the cause

**Author:** sound-bug builder

### Prompt Context

**Verbatim prompt:** "You are finding and fixing a bug in the sound of the s4ql simulator page (`site/simulator/`, served at www.s4ql.com/simulator/). [...] Markus, who uses the live page: "the sound doesn't always work (maybe only after a refresh)". That is all there is. It can be read two ways, and you should test both: the sound fails after a reload of the page, or the sound works only after a reload. [...] If you cannot reproduce it, or the cause is in a browser's policy that code cannot work around, or the fix would be more than a small change, do not guess a fix: stop and report what you found, what you ruled out, and what you would ask Markus (browser, device, the exact steps)." Two later messages from the lead: Markus sees it in Safari on macOS, so put WebKit first; and Markus thinks it started with the persistent storage, so compare a first visit with a reload on a stored database.

**Interpretation:** observe the `AudioContext` and the sound module's counters in headless WebKit and Chrome through the sequences of a visitor, find which ones leave the page saying sound is on while nothing plays, and fix it if the fix is small and clear.

**Inferred intent:** the sound should work whenever the button says it is on, and Markus wants to know why it does not.

### What I did

I built the site (`make site SITEVERSION=`), served it with `go run -tags s4ql_browser ./internal/simulator/serve -root site -isolated=false -addr 127.0.0.1:8247`, and drove it with scripts on the Playwright library that `playwright-cli` brings (`node repro.js webkit|chrome`, in the scratchpad). The scripts read the button's `aria-pressed` and `data-audio` (the context's state), and `stats` of `/simulator/sound.js`, which the page imports at the same URL, so it is the same module instance: the figures played and dropped, and the ticks.

The sequences, each in WebKit (Playwright's) and Chrome:

| Sequence | State | Figures and ticks |
| --- | --- | --- |
| First visit: Sound, then Play 3 s | running | blip, up at once; 5 ticks |
| First visit: Play, then Sound | running | blip; ticks at once |
| Sound toggled eight times in a row, with gaps of 0, 20, and 80 ms | running when on, suspended when off; `aria-pressed` always matches | plays |
| Sound off, other tab in front, back | suspended, as it should be off | none, as off |
| Reload after a session: Sound, Play | **running** | **only the blip for 30 s of fake time**, then up, then ticks |
| The same after Reset | running | blip, up, ticks at once |
| Back/forward to the page | the page loads fresh (not from the cache), sound off | as a reload |
| Skip an hour (566 commit objects), reload, Sound, Play | **running** | **only the blip for 30 s of fake time**, then up and failing, then the error tone at most every 1.5 s for about 70 s more of fake time, then normal |

So the failing sequence is: a reload of a tab that has a stored database, then Sound and Play. The button says Sound on, the context is `running`, and nothing sounds: `stats.played` holds only the blip, and `stats.ticks` is empty. The log has one line, "S3 still holds db-…", for the whole time, and the requests show 0/0/0. Turbo is on by default (`let turbo = true`, `/site/simulator/page.js` line 101), so 30 s of fake time is 6 s of real time; with turbo off it is 30 s. With a larger database, the catch-up that follows adds about 0.13 s of fake time per commit object, in which every request fails with "cache behind" and the only sound is the error tone, at most once every `ERROR_SPACING` (1.5 s) and at 0.5 of `MASTER`, about −41 dBFS at 330 Hz: on a laptop's speakers, close to silence. In the run with 566 commit objects, that is about 20 s of real time in turbo, and about 100 s without, before the first ordinary tick.

The cause is not in `sound.js`. The sound module plays each figure and tick it is given; it is given none, because nothing happens. On a reload, `begin` in `/site/simulator/page.js` (line 781, `answer = await e.start(stored ? { ...settings, bucket: stored } : settings)`) starts the session on the stored database, whose manifest still names the lease of the process `a` of the session before the reload. The new `a` cannot know that process is gone, so it waits for that lease to run out, 30 s by its clocks, and then catches up from S3, one commit object after another, before it serves. The diary of the persistent bucket says so: "`a` waits for the old lease (30 s by its own clocks), then catches up sequentially" (`/docs/diary/2026-11-20-simulator-persistent-bucket.md`, line 83). That is the storage design simulated honestly, and the site check checks it ("a catches up from S3 and serves … s of fake time after the reload").

The storage arrived in `4672829` (2026-11-20, "Keep the simulator page's S3 in the tab's sessionStorage, with Reset"). Before it, `start` always began with `e.start({ seed: R.newSeed(), workload: { callers: 0 }, client: { instance: "a" }, standby: true })`, with no bucket (`git show 4672829^:site/simulator/page.js`, line 603), so every load was a first visit: `a` takes the lease at 0:00.1 and the first sounds come at once. I did not build and run the page at `4672829^`; the first visit of today's page takes the same path, and it sounds at once in both browsers. That matches Markus's "I think it started after we built persistent storage" and "maybe only after a refresh".

I made no change to the code. The fix is not small and it is a question of design: what the page should do, or say, or sound, while `a` waits for the lease of the process from before the reload.

### Why

The brief says not to guess a fix when it would be more than a small change. Every fix of this one changes what the page does after a reload: a line in the log, a sound for the wait, or a page that does not leave a stale lease behind. Each is the lead's and Markus's to choose.

### What worked

Reading `stats` of the sound module from the page, by importing `/simulator/sound.js` in `page.evaluate`, which gives the page's own module instance because the local site has no version in its URLs. With it, "the context runs but nothing is played" is a number, not a guess.

### What didn't work

Playwright's headless browsers keep `document.visibilityState` at `visible` when another page of the context is brought to the front, so the sequence "tab hidden and shown" did not hide the tab, and Safari's `interrupted` state could not be reached. A compound shell command with `cd` and `git` was refused by the worktree guard ("this command names git in a form too complex to verify that it stays inside the worktree"); plain commands one at a time worked.

### What I learned

After a reload, the page is silent for 30 s of fake time for any database, however small, because of the old lease alone, and longer the larger the database. Reset ends it at once, because Reset empties the database, so there is no old lease.

### What was tricky

The bug looks like one of audio, and every hypothesis of the brief about the `AudioContext` (made outside a gesture, `suspended`, closed, replaced by Reset) turned out false in both browsers: the context is `running` in every sequence where the button says on. The sound is missing because the events are.

### What warrants review

The table above, against a run of `/private/tmp/claude-502/-Users-maragubot-Developer-s4ql/3a1f6409-2444-4c23-8123-ed5164204cd4/scratchpad/sound-bug/reload.js` and `big.js` (`node reload.js webkit`, `node big.js chrome http://127.0.0.1:8247/simulator/ 3600000 45`) with the site served as above. By hand: `make site-serve`, Play for a few seconds, reload, Sound, Play, and listen.

### Future work

- Decide with Markus what the page does while `a` waits after a reload. Options: a line in the log, such as "a waits for the lease from before the reload to run out", which the log does not have now (the line "learns that its lease from before has not run out yet" exists in `/site/simulator/run.js` but did not appear in these runs); a sound for the wait; or no stale lease after a reload.
- Not observed, because headless WebKit is not Safari: Safari can put a context into `suspended` or `interrupted` by itself, for instance when the output device changes, and `sound.js` resumes the context only in the click of Sound (`ctx.resume()` in `wire`). If Markus still hears nothing on a first visit or after Reset, a small hardening is to resume the context in `blip` and `hear` when sound is on and the state is not `running`.
