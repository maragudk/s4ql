# Diary: object store

Build the interface through which s4ql talks to S3, and a simulated store behind it that runs under the simulation harness. It is the third of four pieces of the storage core without SQL: the simulation harness (`/docs/diary/2026-11-12-simulation-harness.md`) and the storage formats (`/docs/diary/2026-11-12-storage-formats.md`) are merged, and the core itself follows. The client for real S3 is a piece of its own after this one.

## Step 1: Requirements and scope

**Author:** main

### Prompt Context

**Verbatim prompt:** Okay, what's next?
**Interpretation:** Markus asked this at the start of the day's building, and I proposed four pieces. The harness and the formats were built and merged the same day as pull requests #6 and #5. This piece had to wait for the harness, because the simulated store waits through the harness's `host.Host`.
**Inferred intent:** Keep building the storage core on what was agreed, without waiting for him on anything that is not a question of design.

### What I did

I read the reports of the harness builder and of the builder who merged the two branches, created the worktree `.claude/worktrees/object-store` on branch `worktree-object-store`, wrote this entry, and briefed one builder on Opus.

The requirements handed to the builder.

The interface:

1. **One small interface for what the core needs of S3, and nothing more:** read an object, write an object only if none exists at the key, and write an object only if it is unchanged since it was read. There is no listing and no delete, because nothing in the design uses them yet.
2. **A read gives the content as a stream and a version.** The version is what a later conditional write is checked against. A key with no object is a distinct error.
3. **A write takes its content as a reader, not as bytes in memory,** so that a large commit object is never held in memory. A commit object is complete before its upload starts, because its key is the checksum of its content, so no pipe is needed.
4. **A write can carry the SHA-256 checksum of its content.** A store that has it checks the content against it and refuses on a mismatch, as S3 does (`/docs/todo.md`, "The S3 client and S3's checksums").
5. **The errors tell the cases apart:** no object at the key; an object is already at the key; the object changed since it was read; the content does not match its checksum. Every other error of a write means that the outcome is unknown: the write may have been applied, or may still be applied later. The documentation of the interface says this in so many words, because the design of the core rests on it (`/docs/todo.md`, "A manifest swap whose outcome is unknown").
6. **Library code.** The interface and anything next to it that is not for tests follow the rules of the lint, `simlint`.

The simulated store:

7. **It implements the interface in memory and runs under the scheduler.** It waits only through `host.Host`, draws every random number from the seed, and writes every request and its outcome into the trace, so that a seed replays a run exactly.
8. **It behaves like S3 where the design depends on it:**
   - Strong consistency: a read reflects every write that has been applied.
   - The version of an object comes from its content, as S3's ETag does for a simple upload. Two writes of the same bytes give the same version. A simulated store that gave every write a new version would hide a real bug: a lease renewal that writes the same bytes is invisible to a compare-and-swap.
   - A write is applied at one instant, completely or not at all.
9. **It never does what S3 never does:** it loses no write that it has acknowledged, serves no stale read, and changes no content.
10. **Each instance of the simulation gets its own client,** so that the store knows who sends a request, and a test can cut one client off.
11. **Faults, each from the seed, each with its own switch and probability:**
    - a request that is lost before it is applied, with an error
    - a response that is lost after the write was applied, with an error
    - a write whose caller gave up, by its context, and that is applied afterwards, or never
    - slow requests, with simulated network time before and after the store applies them
    - a read that fails in the middle of its content
    - a client that is cut off for a time, and a store that nobody reaches for a time
    - the crash of an instance while its request is on the way: the request is applied or not, by the seed
12. **A test can look at the store directly,** without faults and without scheduling points: every object, and the history of applied writes in order, with the client and the simulated time of each.
13. **Calm runs are possible:** with every fault off, no request fails.

Testing:

14. **A conformance suite** tests the interface as a function over a constructor of a store, so that the client for real S3 runs the same suite later. It covers every method and every distinct error, and conditional writes that race.
15. **Deliberately wrong stores stay as tests of the suite:** at least a compare-and-swap that ignores the version, a create-if-absent that overwrites, and a read that is stale. The suite fails for each.
16. **Tests of the simulated store itself:** every seed run twice gives the same trace; each fault is seen to happen and is seen in the trace; with a fault off it never happens; the direct view agrees with what the interface returns.
17. **A small workload under the scheduler** with several clients and all faults on: clients swap a counter object with compare-and-swap and create content-named objects, and a model checks that no acknowledged swap is lost, that every swap with an unknown outcome was applied at most once, and that the final state is one that the applied writes explain.

Out of scope: the client for real S3, the lease, the manifest logic, anything that uses `internal/format`, and replacing the store inside the toy of `/internal/sim`, which stays until the core replaces the toy. The builder does not edit `/docs/spec.md`, `/docs/todo.md`, `/README.md`, or `/AGENTS.md`, and reports instead what in them is now out of date.

No telemetry: the interface has no behaviour, and the simulated store's trace is its own record. Telemetry for requests to real S3 is decided with the real client.

How the goal is verified: `make test` and `make lint` pass; `go test -race` passes on the new packages; with `S4QL_SIM_SEEDS=10000`, the workload of point 17 run twice per seed gives no pair of traces that differ and no violation; the suite fails for each wrong store of point 15; and switching off the store's handling of any one fault's bookkeeping makes a test fail.

### Why

The core's hardest cases are all conversations with the store: a swap whose answer is lost, a request that lands after its sender gave up, a lease taken by another instance. The core can only be trusted if the simulated store can produce each of them on purpose, and if the same interface is later shown to behave the same against real S3.

### What worked

The harness builder's report listed what a simulated store must respect: it waits through the scheduler's host for network time, gives each instance its own client, and carries a request on a task of the test if the request is to survive the crash of its sender.

### What didn't work

Nothing has failed yet. No code has been written.

### What I learned

S3's ETag for a simple upload is a checksum of the content, so writing the same bytes twice does not change it. The lease design under discussion with Markus on this day needs a renewal to be visible, so the simulated store has to copy this property, not improve on it.

### What was tricky

Deciding what the store may do wrong. Every fault in point 11 is something a network or a crashed process really does. Silent corruption of content is left out, because S3 with TLS does not do it, and the reader of a commit object checks the content against its name anyway.

### What warrants review

Whether point 8 models S3 closely enough. The client for real S3 will run the conformance suite, and that is the check.

### Future work

The client for real S3, with the checksums of `/docs/todo.md`, and then the core.

## Step 2: The interface, the simulated store, and the conformance suite

**Author:** store-builder

### Prompt Context

**Verbatim prompt:** You are building the object store layer of s4ql, a Go SQL database whose truth is a log in S3, with a local SQLite cache. [...] Two things: the small interface through which the storage core will talk to S3, and a simulated store behind it that runs under the simulation harness and can do on purpose everything that S3 and a network really do. Plus a conformance suite that the client for real S3 will run later. You do not build the real client, and you do not build anything that uses the store. [...] The simulated store is the thing the correctness of the core will be judged against. Think about what a wrong simulated store would do: it would be more forgiving than S3, and bugs in the core would pass. Where S3's behaviour is something you are not sure of, check AWS's documentation, cite it in a comment, and choose the less forgiving behaviour when the documentation leaves it open. [...]
**Interpretation:** Requirements 1 to 17 of Step 1: an interface, a simulated store under the scheduler with every fault of point 11, a direct view, a conformance suite with wrong stores that it catches, tests of each fault, and a workload checked against a model.
**Inferred intent:** Give the core a seam that real S3 can be held to exactly, and a simulated store that is never kinder than S3, so that a bug in the core cannot hide behind it.

### What I did

Three packages, all library code under `simlint` with no exemption:

- `/internal/store/store.go`: `store.Store` with `Get(ctx, key) (io.ReadCloser, Version, error)`, `Create(ctx, key, content io.ReadSeeker, checksum *Checksum) (Version, error)`, and `Swap(ctx, key, from Version, content io.ReadSeeker, checksum *Checksum) (Version, error)`. `Version` is an opaque string. `Checksum` is an alias of `[32]byte`, passed as a pointer that may be nil. The four sentinel errors `ErrNotFound`, `ErrExists`, `ErrChanged`, and `ErrChecksum` each mean "not applied", and the package documentation says that every other error of a write means the outcome is unknown, also one that wraps a context error.
- `/internal/store/simstore/`: `simstore.New(host, Options)` on the host of the test itself, and `Store.Client(name, sender)` for each instance, where the sender is anything with `Crashed() bool`, such as a `*sim.Instance`, or nil. Every request is carried by a task of the test, named `store request N`, so that it survives its caller giving up and its sender crashing. The caller waits for that task with its own context. The carrier draws the faults, sleeps the way there, decides a request whose caller gave up or whose sender crashed by `Faults.Late` or `Faults.AfterCrash`, checks cut-offs and outages, applies the request at one instant under the mutex, sleeps the way back, and decides the response. A version is an HMAC-SHA256 of the content, with a key drawn from the seed, so the same content gets the same version at every key, and nothing outside can compute it. The direct view is `Store.Object`, `Store.Keys`, and `Store.History`, which copy what they return. `Store.Wait` waits for every carrier, including late ones. `Client.CutOff` and `Store.Outage` let a test cut a client or every client off by hand, and `Faults.CutOff` and `Faults.Outage` do it from the seed. `simstore.AllFaults` is a default with every fault on. Every error that a fault causes wraps `simstore.ErrFault`.
- `/internal/store/storetest/storetest.go`: `storetest.Test(t, harness)` runs 17 cases, `storetest.Cases` lists them, and each case reports through a small `storetest.T` interface with `Helper` and `Errorf`, and no `FailNow`, because a case runs in a task of a host and must not call `FailNow` there. A `Harness` runs a case once against a new, empty store, as a task of a host, so the same cases run under the scheduler and, later, against real S3 with `host.New()`. Racing cases start eight tasks through the host.

The tests: `/internal/store/storetest/storetest_test.go` runs the suite against five wrong stores, which wrap a correct simulated client: a swap that ignores its version, a create that overwrites, a read that serves what it read first, a write that drops its checksum, and a swap that creates where no object is. Each is caught by the case named for it. `/internal/store/simstore/client_test.go` runs the suite over 20 seeds, and has a probe (instances `a`, `b`, `c`, each with a caller of 25 operations, with or without short timeouts, and with or without a crash of `c`) with which each of the eight faults is seen through the interface and in the trace with only it on, and never with it off and every other fault on. It also checks a calm run, replay of 50 seeds, and, in every run of the fault tests, that no write of a client is applied, and no call of it succeeds, inside a window in which the trace says it was cut off. `/internal/store/simstore/simstore_test.go` tests the direct view, versions, `Wait`, `Outage`, and the workload of point 17.

The workload has instances `a`, `b`, and `c` with two callers each, and 15 operations per caller under timeouts that are long or short at random, with `simstore.AllFaults()`. One of the three crashes at a random time, and instance `d` joins. Callers swap a counter whose content is `n id`, create objects named by the SHA-256 of their content with retries while the outcome is unknown, one in ten with a wrong checksum, and read. The model replays the history and checks that every write was attempted; that each fits create-if-absent and compare-and-swap; that the counter goes up by one per write; that the same content has the same version and other content another; that an acknowledged write was applied exactly once, with the version it returned; that the writes of a content are no more than the attempts that may have made them, so a write with an unknown outcome is applied at most once; that a create of an object never returns `ErrExists` unless an earlier attempt wrote it; that the store's objects equal the model; and that each read returns a state that its key had at some moment between the read being sent and its content being read, or a prefix of it if the content broke off.

### Why

The interface is the seam that real S3 must match exactly, so it has only what the requirements need. The simulated store is the judge of the core, so wherever S3 leaves a choice open, it takes the harsher side.

### What worked

The suite passed against the simulated store at the first run, and the tests of the suite then showed it has teeth. Mutation testing with `go test -overlay`, from a script in the scratchpad that never touches the tree, broke one piece of the store at a time: 25 of 25 mutations make `go test ./internal/store/...` fail. Run with `-run TestStore_workload` alone, the workload catches the 10 mutations that break a promise of S3 (a swap that ignores its version, a create that overwrites, a dropped checksum, a stale read, an unrecorded write, and the five that change when a late or crashed write is applied). It cannot catch the 15 that only make the store kinder, such as a lost response that never happens, and the fault tests catch those.

### What didn't work

- The first run of the tests of the suite never ended for the stale store: `Expected nil error, but got "no end after 1000000 steps; waiting: counter-0 at wait for store request 249996, ...`. A racer that never sees the swaps of the others retries forever. The counter case now gives up after twice as many failed swaps as can succeed in all, which also bounds it against real S3.
- The stale store was first caught only by side cases, because the case meant for it read through a fresh client, whose wrapper had no cache yet. I added a case of its own, "should reflect in a read of any client every write that returned before it", in which two clients take turns writing and reading.
- `TestClient_replay` failed at once: `Expected "1.441706663s c/caller store: request 9: get objects/&{0x50af02e2e2d0 c 0x50af02b50460 0}-1 from c: sent", but got "... get objects/&{0x50af02f6e0f0 ...`. My probe put a pointer into a key with `fmt.Sprint("objects/", c, ...)`. The replay test did its job on my own test code.
- The "slow is off" test failed for seed 0, because an operation on the counter makes two requests and my bound allowed for one. The bound is now four ways.
- `Faults.AfterCrash` was seen in 1 of 30 seeds, because the crash came at up to 40 s, often after `c` had finished. It now comes within 15 s, and is seen in 4 of 30 seeds and traced in 9.
- The first workload over 200 seeds reported `seed 32: a create with a wrong checksum of objects/1e98f5ce... returned <nil>`. A debug test showed the attempt had never returned: its instance had crashed, so its error was never set. The check now requires the attempt to have returned.
- The first mutation run counted "the checksum is ignored" as caught, with no test named, because the mutant did not compile (`sha256` unused). The script now reports a mutant that does not build. It also missed "a broken read never breaks off": a break moved to the end of the content still fails the read, and no test asked for a break in the middle. A new test requires breaks over 20 seeds, some of them in the middle.

### What I learned

- What the AWS documentation says, with sources, is in Step 4.
- A shell command with a here-document was refused with `This session is isolated in the worktree ..., but this command is too complex to verify that it stays inside the worktree`. Debug files go in the tree and are deleted after, and scripts run from the scratchpad with plain commands.

### What was tricky

- Telling the faults apart in the "never happens" tests with every other fault on. A write whose caller gave up can land after its sender crashed, and a write that arrived before a crash, after its caller gave up, can land after the crash too. Each off-check excludes exactly the case that the other fault explains.
- Keeping each check from being a check of a counter: the cut-off invariant reads the windows from the trace and compares them with the history and with the calls that succeeded.

### What warrants review

- `/internal/store/store.go`: whether the contract is what the core and a real client can both keep, in particular `io.ReadSeeker` for the content and the claim on versions.
- `/internal/store/simstore/client.go`, `Store.carry`: the order of decisions, and that a request of a crashed sender is decided at arrival only.
- `/internal/store/simstore/simstore_test.go`, `world.check`: whether a check can pass for the wrong reason.

### Future work

- The client for real S3 runs `storetest.Test` with a harness of its own.
- Only a planted bug in a consumer of the store, such as the core, shows that the faults reach the consumer. The workload here shows that they reach a model of the store.

## Step 3: Self-review, and what it changed

**Author:** store-builder

### Prompt Context

**Verbatim prompt:** When the implementation is done, self-review it as your standing instructions say, fix what is real, and run `make test`, `make lint`, and the race run again.
**Interpretation:** Run the code-review skill over `git diff main...HEAD`, with two competing reviewers, fix what they found that is real, and verify again, since the review changes the code that the first verification ran.
**Inferred intent:** The simulated store is the judge of the core, so it has to survive someone who tries to make it kinder than S3 without a test noticing.

### What I did

I ran the `fabrik:code-review` skill. Two reviewers on the `fable` model reviewed the branch independently. They were told that they competed and that the store must never be kinder than S3, and they tried their claims with `go test -overlay` from the scratchpad. Both reran the 10000 seeds and the race run on the unchanged tree, and both passed.

Findings that both reported, which I fixed:

- **After any error that was not the caller's own context, the outcome of a write was settled.** This was the serious one. A lost request and an unreachable store meant "never applied", and a lost response meant "applied before the error". The contract says such a write may still land later, and a real network does that after a reset or a timeout of the transport. Reviewer A counted 3422 writes that failed with `ErrFault` over 300 seeds, 0 of them applied after the call returned. Now a write that is lost or does not reach the store is, by `Faults.Late`, stuck in the network: a task of its own carries a copy of it, which arrives up to `LateBy` later, and is applied unless the store is unreachable by then. Both reviewers noted that requirement 11 as worded allowed the old behaviour, so the gap was in the requirement as well.
- **No call blocked until its context ended.** A core that calls the store without a timeout would never be caught. The new `Faults.Hang` makes a call that a fault made fail get no answer at all, so it returns only when its context ends. A call without a deadline then hangs the run, which the scheduler reports.
- **S3 documents a third outcome for racing swaps.** The conditional-writes page says of `If-Match`: "You can also receive a `409 Conflict` response in the case of concurrent requests." The new `Faults.Conflict` refuses a swap from the right version with `store.ErrChanged` while another write to its key is in flight. The contract now says that `ErrChanged` can mean this too. The suite's racing swaps accept zero winners if the object is then unchanged, and the counter case allows ten times as many failed swaps as can succeed. My earlier citation came from the `If-None-Match` paragraph, and the package documentation now quotes both.
- **The contract's sentence on unknown outcomes was wrong.** "Only a later read, or a conditional write that the stuck request would make fail, tells what happened" suggested that a read settles the outcome, but the write can land after the read. The package documentation now says that only a conditional write that succeeds rules a stuck write out: a create rules out every create of the key, and a swap rules out swaps from the version it replaced, for as long as that version does not come back.
- **The SDK's own retries break the four errors.** A real client that sends a conditional write again by itself, after a first attempt whose response was lost, gets 412 for a write that this very call applied. The contract now says that such a refusal is an unknown outcome.
- **The ETag claim was imprecise.** SSE-KMS gives an ETag that is not MD5 too, also with a key that AWS manages. The comment now says "not encrypted with SSE-KMS or SSE-C", with the source.
- **Mechanisms that could be removed with every test passing:** the delay of a late write (`LateBy`), the client and request of each write in the history, slowness on each way separately, the key check, the panic on a swap from an empty version, the refusal of closed content, the panic on a duplicate client name, and `Store.Wait` waiting for requests sent while it waits. Each has a test now.
- **The "never happens" checks of cut-offs and outages were the trace marker twice.** Each fault now has two such checks: the marker, with every other fault on, and the effect through the interface and the history, with the other faults that look the same switched off too. For example, a lost response and a stuck request look the same to a caller.
- **The workload's IDs were not unique.** Both callers of an instance used `name-i`. They are `name-caller-i` now.
- **`go test -race` saw nothing.** Every test ran under the scheduler, one goroutine at a time. `TestClient_free` runs the conformance suite and eight parallel callers with every fault on, on `sim.Free`.
- **Comments looked outward**, to the lease, to "the correctness of s4ql", to the simulation's instances and the host of the test, and to the client for real S3. **Single-use package-level variables:** `wrongStores` and the two regular expressions of `probed.breach` are inlined.

Findings of one reviewer that I fixed for their seriousness:

- A task that calls `Crash` on its own instance runs on until it waits, and its request was still delivered, which a dead process never sends. A deferred call to the store from a crashed task failed the whole run, with `task store request 1 starts while the run ends`. A client whose sender has crashed now sends nothing and returns an error at once. A request that is never applied now gives a live caller an error, where it gave `nil` before (reviewer B).
- The check of the workload traced violations while it ranged over maps, so a run that found a bug could give two traces. Reviewer A found 4 such seeds of 195 with a planted bug. The check now ranges in sorted order (reviewer A).
- Which switch decides a write whose caller gave up and whose sender crashed before it arrived was not pinned. `TestClient_crash` pins it to `AfterCrash` (reviewer A).
- Reading content took no simulated time, and nothing could touch it after the response. Content now breaks off if its client is cut off while it is read. Reading still takes no simulated time (reviewer A).
- The test that cuts a client off while a response is on its way polled every millisecond and passed by the luck of the seed. Reviewer A found 2 of 2000 seeds where it fails. Both ways are now slow by up to an hour, and the poll runs once a second (reviewer A).
- Two stores on one host collided on the names of their tasks. `Options.Name` sets the name (reviewer B).
- The keys case required `a`, `a/`, and `a/b` side by side, which a store that keeps objects as files cannot hold. It no longer does (reviewer B).
- The workload counted "applied late" by a trace line that is written before the write can still be refused. It now counts from the history (reviewer A).

What I did not change:

- With `Late` or `AfterCrash` at zero, a write whose caller gave up, or whose sender crashed, is never applied. Reviewer A pointed out that this is the kind side for a calm run. I kept it. Each switch is a probability between the two outcomes, `AllFaults` has both at 0.5, and the workload sees both. "Off" means that the fault (applied after the caller has stopped waiting) never happens, which requirement 16 asks for.
- Reading content still takes no simulated time and is no scheduling point. A body may be read by another task than the one that called `Get`, and a wait in `Read` would bind it to the caller's task.
- Both reviewers noted that `Get` cannot carry the download check of `/docs/todo.md` ("On download, the client compares the checksum that S3 returns with the name"). Changing the interface for it is a question for the lead, and I did not.
- `chance` draws no random number for a probability of zero, so a fault that is off leaves the random numbers as they were. No test pins that, because no test needs it yet.

### Why

Every fix closes a way for the core to pass against the simulated store and fail against S3, or a way for a test to pass for the wrong reason.

### What worked

The mutation script now has 45 mutants, one for each mechanism above and the earlier 25. All 45 make `go test ./internal/store/...` fail. The script now also reports a mutant that does not build, so that a compile error is not counted as caught.

### What didn't work

- After the rework, the "never happens" checks of a lost response and of a write after a crash failed for seeds 0 and 3, as they should: a lost request can now be stuck and land later. That led to the split into a check by marker and a check by effect with the look-alikes off.
- The new conflict was seen in 8 of 30 seeds and traced in none: `client_test.go:606: 8 of 30 seeds saw it, and 0 traced it`. The trace line of a refused write named only the sentinel error, not why. It names the reason now.
- `TestStore_Wait` failed with `task main waits at wait for store request 2 in a goroutine that is not its own`. My closure used `main`'s context inside another task, and the scheduler caught it.
- The parallel run on `sim.Free` with `faultsOn()` applied no write at all. Outages of up to 10 s start on 5% of requests there, and the callers had timeouts of 1 s. It uses `simstore.AllFaults()` now.
- Two mutants were missed after the rework: a stuck write that arrives with no delay, and a `Wait` that does not wait for requests sent while it waits. The first needed `LostRequest` in the run that shows `Late`. The second test passed the mutant by the luck of seed 0, and now runs 10 seeds.
- The editing tools refused a shell command that wrote with a here-document and then ran `grep`, with `this command is too complex to verify that it stays inside the worktree`. Scripts for bulk edits now sit in the scratchpad and run as plain commands.

### What I learned

- Writing the "never happens" check by effect forces the question of which faults look the same to a caller. That question has an answer for each fault, and the answer is the list of faults to switch off with it.
- A reviewer's count of "applied after the call returned, by the error it returned" was the clearest way to see the serious gap. No single test had looked at the store from that side.

### What was tricky

- The copy of a stuck write. If the stuck task applied the caller's own request, a late result could overwrite what the caller was about to read: a lost write would then report success. The stuck task applies a copy, which also takes over the original's place among the writes in flight.
- A caller of a stuck write with `Hang` waits until its context ends, while its write may land in the meantime. The checks by effect must allow for it.

### What warrants review

- `/internal/store/simstore/client.go`, `Store.carry` and `Store.lose`: the order of the decisions, and which of `Late` and `AfterCrash` decides each case.
- `/internal/store/store.go`: the package documentation on unknown outcomes and on retries, which the core's design rests on.
- `/internal/store/simstore/client_test.go`, `TestClient_faults`: the `alike` list of each fault.

### Future work

- Whether `store.Store.Get` should carry the stored checksum, for the download check of `/docs/todo.md`.

## Step 4: Verification, and what S3 does

**Author:** store-builder

### Prompt Context

**Verbatim prompt:** Record failures with the exact commands and errors, and record the numbers that verify the goal. [...] Where S3's behaviour is something you are not sure of, check AWS's documentation, cite it in a comment, and choose the less forgiving behaviour when the documentation leaves it open. Do not rely on memory for S3's behaviour.
**Interpretation:** Run the verification of Step 1 on the code as it is after the review, and write down what the AWS documentation says that the store and the contract rest on.
**Inferred intent:** Let the reader check the claim that the goal is met, and let the builder of the client for real S3 start from what is known.

### What I did

The verification of Step 1, on commit `ccd1b3b`:

- `make test` and `make lint` pass. `simlint` reports nothing on `internal/store`, `internal/store/simstore`, or `internal/store/storetest`, none of which is exempt.
- `go test -race -count=1 -shuffle on ./internal/store/...` passes in 66 s. `TestClient_free` runs the suite and eight parallel callers on `sim.Free`, so the race detector sees real parallelism.
- `S4QL_SIM_SEEDS=10000 go test ./internal/store/simstore/ -run TestStore_workload -v` passes in 4 min 2 s: 10000 seeds, each run twice, 0 pairs differed, 10000 distinct traces, and no violation. Of 680517 writes attempted in the first runs, 487087 were acknowledged, 69591 failed for sure, 120462 had an unknown outcome, and 3377 were cut off by the crash. 535382 writes were applied, 15808 of them after every attempt of them had returned, and 670 after the crash that cut their attempt off. 4963 of 309573 reads broke off. Every fault happened: 25587 lost requests, 31622 writes stuck in the network, 23685 lost responses, 31162 calls with no answer, 10845 conflicts, 52677 slow ways, 12113 broken reads, 7132 cut-offs, 2895 outages, 12563 writes applied after their caller gave up, and 955 after their sender crashed.
- The suite fails for each of the five wrong stores of `TestCases`, each in the case named for it.
- Mutation testing: 45 of 45 mutants of the store make `go test ./internal/store/...` fail. The workload's model alone catches 14 of them, all of which break a promise of S3 or change which writes land late. The other 31 make the store kinder, or concern a guard that the workload never touches, and the tests of each fault and each guard catch them.

What the AWS documentation says, as the store and the contract use it:

- Strong read-after-write consistency, atomic updates of one key, and a read concurrent with a write gives "either the old data or the new data, but never partial or corrupt data" (https://docs.aws.amazon.com/AmazonS3/latest/userguide/Welcome.html, "Amazon S3 data consistency model"). Also: "Amazon S3 never adds partial objects" (https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html).
- Conditional writes (https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html): with `If-None-Match`, an existing object gives `412 Precondition Failed`, and "If multiple conditional writes or copies occur for the same object name, the first write operation to finish succeeds". With `If-Match`, a different ETag gives 412, "You can also receive a `409 Conflict` response in the case of concurrent requests", and with no current object "the operation fails with a `404 Not Found` error". The PutObject reference adds that on a 409 "you should fetch the object's ETag and retry the upload". So the condition is evaluated when the upload finishes, which the store follows by checking at arrival, and a 404 for a swap where no object is is `store.ErrNotFound`.
- ETags (https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html): an object written with `PutObject` in plaintext or with SSE-S3 has the MD5 of its content as ETag. One encrypted with SSE-C or SSE-KMS does not, and neither does one from a multipart upload. Whether SSE-KMS gives the same content the same ETag is not documented, so the contract promises neither, and the simulated store takes the harsher side: the same content, the same version.
- Checksums (same page): "If the values don't match, the request is rejected", with `BadDigest`, and nothing is stored. `PutObject` checksums are full-object checksums, and for multipart uploads SHA-256 is only composite.
- Content length: the Go SDK v2 needs a body that implements `io.Seeker` to find the length and to hash the payload, or else `ContentLength` and an unsigned payload, because "Amazon S3 requires the content length to be provided for all object's uploaded to a bucket" (https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/sdk-utilities-s3.html, "Unseekable Streaming Input"). That is why the content of a write is an `io.ReadSeeker`.
- A missing object gives 404 only with the `s3:ListBucket` permission, and 403 without it (https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html).
- Keys are UTF-8 of at most 1024 bytes, and keys with `.` or `..` segments are normalized by some tools (https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-keys.html). The simulated store panics on them.

From `/docs/research/tigris-probe-results.md` and `/docs/research/s3-fault-injection.md`, not from AWS: Tigris does not check a checksum sent as a header, and the SDK retries a conditional `PutObject` by itself after a reset, after which the retry gets 412 for a write that the first attempt applied.

### Why

Step 1 set the numbers that verify the goal, and the brief asked for sources for S3's behaviour.

### What worked

The verification passed on the first run after the review.

### What didn't work

Nothing failed in this step.

### What I learned

The 409 for racing swaps came from reading the page that I had cited for something else. Rereading a source in full is worth it when one quote from it carries a decision.

### What was tricky

Nothing beyond what Step 3 records.

### What warrants review

- Whether 45 mutants are the mechanisms that matter. The script is in the session's scratchpad, not in the tree.

### Future work

- The client for real S3 maps a 404 and a 403 for a missing object to `store.ErrNotFound` according to the bucket's permissions, sends the checksum so that it is checked (by a trailer on Tigris), sends no conditional write again by itself, or reports the refusal of a retry as unknown, and maps a 409 on a swap to `store.ErrChanged`.
