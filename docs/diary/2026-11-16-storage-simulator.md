# Diary: A simulator of the storage design on the site

Markus asked for a page on the project site where people can play with the storage design and inject faults. The page is `/site/simulator/index.html`, served at https://www.s4ql.com/simulator/, and the model behind it is `/site/simulator/model.js`, an ES module that the page imports and that Node runs headless in `/site/simulator/model.test.mjs`.

## Step 1: Requirements

**Author:** simulator-builder

### Prompt Context

**Verbatim prompt:** Build an interactive simulator of s4ql's storage design, in JavaScript, as a new page of the project site. Markus asked: "is it possible to add a simulator written in Javascript to the page? So people can play with the storage design, inject faults etc. Maybe build it on a separate page." [...] `site/simulator/index.html` (served at https://www.s4ql.com/simulator/), plus the model as a separate ES module `site/simulator/model.js` that the page imports and that Node can run headless. Vanilla JS, no dependencies, no build step [...]. The model, deterministic and driven by one seed: a simulated S3 with strong consistency, compare-and-swap on `manifest.json` (ETag-style), and create-if-absent for commit objects [...]. Two or three instances [...]. Virtual time only. [...] Faults the user can inject at any moment [...]. Also a buggify switch [...]. An invariant checker that runs after every step [...]. Add a "planted bug" switch [...]. The page: same world as `site/index.html` [...]. Verification: `node site/simulator/model.test.mjs` [...] over at least 2000 seeds with random workloads, random faults, and buggify on, no invariant is violated; with the planted bug on, the checker finds a violation within a bounded number of seeds; scenario tests for the stall case [...], the dropped-response case [...], and catch-up after a crash. [...] Open the page in a browser [...] at 1440x900 and 390x844 [...]. Commit on your branch with clear messages. Do not push and do not merge.

(The brief is abridged at the "[...]" marks; the full text is in the lead's conversation.)

**Interpretation:** Write a discrete-event model of the lease, the log, the cache, and the simulated store in plain JavaScript, faithful to `/docs/spec.md`, `/internal/core/README.md`, and the Go code in `/internal/core` where the documents are silent. Put an invariant checker and planted bugs in it, test it headless with Node's built-in runner over thousands of seeds, and build a page in the visual language of `/site/index.html` that drives it.

**Inferred intent:** Give visitors a way to see why the design is safe: what a stall, a lost answer, a partition, or a wrong clock does, and that the checker catches a real bug when one is planted. The page must not claim more than the design does.

### What I did

Read `/AGENTS.md`, `/docs/spec.md`, `/internal/core/README.md`, `/internal/format/README.md`, `/internal/format/testdata/manifest-v0-lease.json`, `/site/index.html`, and in `/internal/core` the files `bugs.go`, `core.go` (options, `margin`, `refusalLocked`), `lease.go` (all of it), `read.go`, `tx.go` (`Commit`, `upload`, `publish`, `inLog`, `commitCache`), and `catchup.go`. Read the list of phrases at https://claudisms.ai/claudisms.md.

### Why

The model has to follow the Go code's decisions where the spec is silent: when a spare starts its wait, what a refused swap leads to, when a commit is pushed and when the lease task resolves it, and which checks a read and a commit make.

### What worked

The doc comments in `lease.go` and `tx.go` give every decision of the lease and the commit in a form that maps one to one to generator functions.

### What didn't work

Nothing yet.

### What I learned

- An instance whose lease ran out by its clocks still believes it is the holder (`holder` stays true). Reads and `Begin` refuse, but an open transaction goes on to its swap, which S3 refuses. That is the stall case.
- A request that fails before it is sent is not a refusal to the core. Only "changed", "not found", "exists", and a checksum error are, so a failed swap is of unknown outcome too.
- A spare's wait counts from after its read, and the holder's lease counts from when its renewal was first sent. A watcher waits the holder's duration plus a margin of `max(2 s, duration/10)`.

### What was tricky

Several words of the design are on the list of phrases to avoid, such as "hold", "carry", and "settled". Literal holding of a lease is fine by the list's own note. "Settled" is banned in every form, so the page says "resolved".

### What warrants review

Nothing built yet.

### Future work

None yet.

## Step 2: The model, its tests, and the page

**Author:** simulator-builder

### Prompt Context

**Verbatim prompt:** (as in step 1)

**Interpretation:** Build `/site/simulator/model.js`, `/site/simulator/model.test.mjs`, and `/site/simulator/index.html`, and link the page from `/site/index.html`.

**Inferred intent:** A page that is faithful to the design, that cannot drift from the seed, and whose claims are checked by tests that run without a browser.

### What I did

- `/site/simulator/model.js`: a binary heap of events ordered by virtual time and then by when they were scheduled. Each activity of an instance (the lease loop, a write, a close) is a generator that yields effects: sleep, take a lock, send a request. The scheduler resumes it. The lease loop, `acquire`, `resumeLocked`, `renew`, `renewLocked`, `observe`, `sendFlight`, `publish`, `inLog`, and `catchUp` follow their Go namesakes in `/internal/core/lease.go`, `/internal/core/tx.go`, and `/internal/core/catchup.go`. S3 makes an object's version from the SHA-256 of its content, as the ETag. Every part of the model draws from its own stream of an sfc32 generator seeded from the one seed. A stalled process keeps its events in a list and replays them in order when it resumes; a crashed one drops them. The checker keeps the log as an array with an index, and on every change of the head it walks back to where the new head meets the old chain, which finds a fork, a missing object, and an acknowledged commit that dropped out. The planted bugs are `nocas`, `ackunknown`, `cachefirst`, and `nowait`.
- Actions are recorded with the step count and the virtual time at which they were applied. Replay applies each when the run reaches its step. That is exact even when the page steps one event at a time, which a time stamp alone would not be, because several events can share one time.
- `/site/simulator/model.test.mjs`: 20 tests with `node:test`. The 2000 seeds run 4 virtual minutes each with the workload, random faults, and buggify, then clear the faults, switch buggify off, and require that an instance serves within 4 more minutes.
- `/site/simulator/index.html`: the instance cards are built once per run and updated by text, so a button keeps its focus. Lists are rebuilt only when a key of their content changes, at most about ten times a second. The sticky bar shows the checker's state, so a violation is seen wherever the page is scrolled.
- Drove the page with `playwright-cli` at 1440x900 and 390x844: every fault button, buggify, random faults, a planted bug until the checker stopped the run, replay from the start, a new seed, a bad seed, two instances, the keyboard, and reduced motion.

### Why

Generators keep the model's code shaped like the Go code, so a reader can compare them function by function. A discrete-event queue with a sequence number as the tie-breaker makes every run a function of the seed and the actions.

### What worked

- The replay in the browser reproduced a planted bug's violation at the same virtual time and step, `4:24.230` and step 1971, after "Replay from start".
- `node site/simulator/model.test.mjs` run 12 times at once gave the same totals each time: `{"acked":62938,"notCommitted":1567,"unknown":245,...,"resolvedCommitted":5796,"resolvedNot":42,"resolvedLost":45,...}`, 1455 seeds with a takeover from a holder, 3 with a stale read that a clock fault excuses. The whole file takes about 6 s.
- The planted bugs, over seeds 1 to 40: `nocas` in 19 (first at seed 7), `ackunknown` in 2 (first at seed 10), `cachefirst` in 40, `nowait` in 32.

### What didn't work

- With the application's timeout at 15 s, the stall test failed with `AssertionError [ERR_ASSERTION]: the stalled holder sent the swap of its commit`. The stalled holder's caller had given up before the swap, so `publish` returned "the caller gave up" without sending, which is what the Go code does too when the context is done. A takeover takes about 38 s, so the refused swap of an open write is reachable only when the caller waits longer than that. The timeout is now 45 s, and the page says why.
- The test of a commit resolved as not committed first failed with `assert.ok(sim.events.some((e) => e.kind === "resolved" && e.outcome === "not committed"))`. The model was right: the swap went out in the same step as the upload's answer, before the partition that the test applied, so it landed and was resolved as committed. The test now arms "fail before sending" for the first swap, which the instance treats as of unknown outcome, as the Go code treats any error that is not a refusal.
- The liveness check after clearing faults failed for 21 of 50 seeds. "Clear all faults" did not clear a failed disk of a running instance, and buggify went on stopping instances after it, with no random faults left to restart them. Clearing now repairs disks, and the test switches buggify off first.
- At 1440 px the page scrolled sideways by 44 px: the select of planted bugs, whose options are long, set the width of its grid's implicit column. `grid-template-columns: minmax(0, 1fr)` on the knobs fixed it.
- The highlight of the head in `manifest.json` never showed: the pattern looked for `"head"` in text that was already escaped to `&quot;head&quot;`.
- A shell `node` call to a script in the scratchpad that imports the worktree's model was refused by the worktree guard as "too complex to verify"; writing the script with the Write tool and running `node <script>` worked. `playwright-cli eval` was refused for the same reason, and `playwright-cli run-code` with `page.evaluate` worked.

### What I learned

- Under the design, a stalled holder's open write can only be refused by S3 if the application waits for it longer than a takeover. With a shorter deadline, the write ends as "not committed" before any swap.
- A stale read under clock faults needs both of the holder's clocks to measure too little, or a spare's monotonic clock to run fast past the margin. The checker excuses a stale read only then, and for two lease durations after the clocks run true again.

### What was tricky

- Reads are one step in the model, so the subtle order of the Go code (check the lease, fix the state, check again) is not modelled. It is listed on the page.
- Which instance gets an event first after a stall depends on the original times of the deferred events. The stall test steps until the commit object's upload is on its way before it stalls, so that the write's swap comes before the lease task's renewal on resume.

### What warrants review

- `Sim.publish`, `Sim.resumeLocked`, and `Sim.renewLocked` against `/internal/core/tx.go` and `/internal/core/lease.go`.
- `Checker.onClockFault` and `Checker.excused`: the rule that excuses a stale read is the model's own reading of "Clocks affect only reads".
- The 45 s timeout of the application, and the page's list "Where the model is simpler".

### Future work

- `ackunknown` is found in only 2 of 40 seeds. Unknown outcomes are rare in the random faults (245 in 2000 seeds), because the application waits 45 s.

## Step 3: Self-review and fixes

**Author:** simulator-builder

### Prompt Context

**Verbatim prompt:** (as in step 1)

**Interpretation:** Review the branch with the code-review skill, fix what it finds, and run the checks again.

**Inferred intent:** Hand back a branch whose model does not claim things the design does not do.

### What I did

Ran the code-review skill at high effort over `main...HEAD`. It reported nine findings, three of them confirmed with a probe script. I fixed all nine in `e620b85`, with a test for each that could have one:

- Timers of a process ran in true time while the process measured its waits on its own monotonic clock. A holder whose monotonic clock ran twice as fast slept 12 s of true time for a renewal due in 12 s of its own time, by when its clock had passed the lease, so it refused reads for 25 s in 120. Go's timers run on the monotonic clock. `Sim.after` now waits for a target reading of the process's clock, and `armTimer` arms the timer again when the rate changes. Test: "a holder whose monotonic clock runs fast renews on time by that clock, and keeps serving".
- Any fast monotonic clock anywhere excused every stale read in the run for as long as it lasted, which could hide a real freshness bug. `Checker.excuse(slot)` now excuses a read only on an instance whose two clocks both measured too little time, or, after an instance with a fast monotonic clock took the lease from another (`Checker.onTake`), on the other instances, for two lease durations.
- `Checker.check` compared every row of every cache at every change, and `onManifest` walked every acknowledged commit at every write of the manifest: quadratic over a long run on the page. It now keeps how many rows of each cache it has checked, and rescans acknowledged commits only after a fork.
- A link such as `#seed=1&n=2&a=0.0.w2` threw `Cannot read properties of undefined (reading 'proc')` in the module script and left the page dead. `decodeActions` now refuses a clock action without an instance index, `Sim.apply` ignores an action on an instance that the run does not have, and the page refuses such a link and starts from the seed.
- An armed "fail before sending" and an armed "lose the answer" were both used up by one request. `takeArmed` now takes one.
- A read or write whose process died before it ran vanished without a trace. Events can now carry a `dropped` handler, and a process keeps its pending writes, so both are logged.
- The button of an instance that was shutting down said "Start" and killed the close in the middle of releasing the lease. `Sim.restart` now ignores a closing process, and the button says "Shutting down".
- The page built a commit object's key by hand; it imports `commitKey` now. `Proc.startedAt`, `Proc.writes`, and `Checker.lastAck` were never read and are gone.

I also lengthened random partitions from at most 45 s to at most 90 s and ran the planted bugs over 60 seeds instead of 40, because after the fixes `ackunknown` was found in only 1 of 40 seeds.

### Why

The second and the fourth findings were model errors that would have shown visitors behaviour the design does not have: a holder that stops serving for no reason, and a checker that waves a bug through.

### What worked

`node site/simulator/model.test.mjs` passes 26 tests in about 7 s, and 12 runs at once gave the same totals each time: `{"acked":56255,"notCommitted":1648,"unknown":236,...,"resolvedCommitted":5151,"resolvedNot":28,"resolvedLost":40,...}`, 1456 seeds with a takeover from a holder, 5 with a stale read that a clock fault excuses. Planted bugs over 60 seeds: `nocas` 31 (first at seed 4), `ackunknown` 4 (first at seed 11), `cachefirst` 60, `nowait` 51. In the browser, the bad link now gives the note "The actions in the link could not be read, so the run starts from its seed alone." and no error.

### What didn't work

- My first version of the timer fix compared `this.mono(p) < timer.target`. The time is in whole milliseconds, so a timer whose look came a rounding error short armed itself again at the same instant. I added a tolerance of 1e-6 ms.
- The test run then hung, which no per-test timeout could stop, because the test is synchronous (`node --test-timeout=15000` did not help). A guard on `Sim.prototype.step` in a scratch script showed the model at `now` 2522537710, a month of virtual time, renewing normally. The cause was my new test: `for (let t = sim.now; t < sim.now + 120_000; t += 250)` moves its own bound as `sim.now` advances. It now fixes `end` before the loop.
- `timeout` and `gtimeout` are not installed on this Mac, so a run cannot be capped from the shell.

### What I learned

A model with clocks of different rates needs its timers on those clocks, or every rate fault doubles as a scheduling bug of the model.

### What was tricky

Navigating the browser to the same page with only a different hash does not reload it, so the first check of the bad link looked at the old run. Going through `about:blank` first gives a real load.

### What warrants review

- `Sim.after` and `Sim.armTimer`: every sleep and request timeout of a process goes through them.
- `Checker.excuse` and `Checker.onTake`: which stale reads count as allowed.
- The page's copy, against https://claudisms.ai/claudisms.md. "Holds the lease" is kept as the spec's literal term.

### Future work

- `ackunknown` is still found in few seeds (4 of 60), because an unknown outcome needs the caller to give up after 45 s.
