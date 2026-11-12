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
