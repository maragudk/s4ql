# AGENTS.md

Guidance for AI agents working in this repository. What the project is and what has been agreed is in [docs/spec.md](docs/spec.md); the SQL that is supported and what comes next is in [docs/roadmap.md](docs/roadmap.md); decisions are in `docs/decisions/`, background in `docs/inspiration.md` and `docs/research/`.

## Memory

Do not use your harness's internal or automatic memory for this project. Anything worth remembering between sessions goes in this section, so that it is versioned, visible to Markus, and shared by every agent. Add, update, and remove entries here as things change.

### Be ambitious; do not argue from human effort

Do not use human-team effort or duration ("years of work", comparisons with how long a company took) as a reason to shrink the project's scope or to prefer a less ambitious design.

- **Why:** Markus said on 2026-11-10, after being told that writing our own SQL engine would be "years of work": "You are an AI. What you can do now wasn't possible a year ago, and progress is accelerating. I'm counting on 'years of work' not being true because of exponential AI progress. I want us to be ambitious for this project. My main concerns are trust in correctness, so that's what we're aiming for."
- **How to apply:** argue from correctness, verifiability, and risk, not from how much code or calendar time a human team would need. Relay findings about other projects' effort as facts about those projects, not as estimates for this one. Scope cuts are still fine when they improve trust or remove something that does not need to exist. Do not give unmeasured time estimates.

### Low ceremony on email address and secrets

Do not spend effort scrubbing Markus's email address from documents, transcripts, or research notes committed here, and do not pause work over possible secret leaks.

- **Why:** on 2026-11-10 Markus said "I don't care about my email address leaking btw", and earlier the same day "Don't worry too much about leaking secrets, we can just rotate them if you find them leaked."
- **How to apply:** a cheap automatic check for the S3 access key and secret before committing bulk content is still worth running. Do not block, ask, or redact on account of the email address, and do not stop work on a likely false positive. If a real credential does get pushed, tell Markus at once so he can rotate it.

### Open: how the drivers run the state machine

The concurrency model is settled and in the spec under "Concurrency": one write transaction at a time per database, reads in parallel bounded by the connection pool, and all state changes on a single path written as a state machine that does no I/O itself.

Still undesigned, and to be worked out before anyone is briefed on engine work:

- how the production driver runs the state machine: goroutines, channels, and how blocking S3 calls and cgo SQLite calls report back as events
- how the simulation driver schedules events and parallel readers from a seed, given that Go cannot fix the order in which goroutines run
- what `testing/synctest` contributes
- how read connections check that the lease is valid without going through the write path

Relevant notes: `docs/research/foundationdb.md`, `docs/research/slatedb.md`, `docs/research/testing-strategies.md`.

### Open: a local commit failing after the manifest swap

The project needs a well-thought-out story for one failure: the manifest swap succeeded, so the transaction is committed in S3, but the commit to the local SQLite cache then fails. Markus asked on 2026-11-10 for a good story, and said not to overfocus on a full disk: local disk I/O can fail in many ways, and the story must cover local I/O failure in general.

- **Why it matters:** the spec says the cache is never ahead of S3 and that a commit is acknowledged only after the cache has it. In this failure the commit is real but the cache cannot show it, so the instance must not carry on serving reads from the cache at the old state.
- **Proposed, not yet agreed:** treat any local I/O failure like a crash. The instance stops serving, and recovery is the normal startup path (reopen and catch the cache up from the log). Two refinements over a real crash: `Commit` returns success if the swap succeeded, because the transaction did commit, and the instance releases the lease on the way out. Returning success would need the spec's acknowledgement rule amended to "after the cache has it, or after the instance has stopped serving".
- **Questions the story must answer:** what the committing caller is told; what happens to other in-flight transactions and readers in the process; whether the lease is released or kept; how the instance recovers if the local failure persists, and how loudly that is surfaced; whether the cache is caught up in place or rebuilt; and how the simulation injects this failure (a buggify point after the swap and before the local commit).
