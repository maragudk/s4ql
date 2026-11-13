# AGENTS.md

Guidance for AI agents working in this repository. What the project is and what has been agreed is in [docs/spec.md](docs/spec.md); the SQL that is supported and what comes next is in [docs/roadmap.md](docs/roadmap.md); what is agreed but not built, and the design questions that are still open, are in [docs/todo.md](docs/todo.md); background is in `docs/inspiration.md` and `docs/research/`, and the conversations behind the decisions are in `docs/conversations/`.

## Memory

Do not use your harness's internal or automatic memory for this project. Anything about how to work in this repository that is worth remembering between sessions goes in this section, so that it is versioned, visible to Markus, and shared by every agent. Add, update, and remove entries here as things change.

### The project is named s4ql

The project is named s4ql, short for "simple storage service + structured query language". It was first named sql4s3.

- **Why:** Markus renamed it on 2026-11-10: "I updated the project name from sql4s3 to just s4ql". The module path is `maragu.dev/s4ql` and the repository is `maragudk/s4ql`. Markus also bought the domain s4ql.com that day, and for now it only redirects to the repository on GitHub.
- **How to apply:** write s4ql in everything new. The notes in `docs/research/` and the kickoff transcript in `docs/conversations/` still say sql4s3, and they mean this project.

### Be ambitious; do not argue from human effort

Do not use human-team effort or duration ("years of work", comparisons with how long a company took) as a reason to shrink the project's scope or to prefer a less ambitious design.

- **Why:** Markus said on 2026-11-10, after being told that writing our own SQL engine would be "years of work": "You are an AI. What you can do now wasn't possible a year ago, and progress is accelerating. I'm counting on 'years of work' not being true because of exponential AI progress. I want us to be ambitious for this project. My main concerns are trust in correctness, so that's what we're aiming for."
- **How to apply:** argue from correctness, verifiability, and risk, not from how much code or calendar time a human team would need. Relay findings about other projects' effort as facts about those projects, not as estimates for this one. Scope cuts are still fine when they improve trust or remove something that does not need to exist. Do not give unmeasured time estimates.

### Low ceremony on email address and secrets

Do not spend effort scrubbing Markus's email address from documents, transcripts, or research notes committed here, and do not pause work over possible secret leaks.

- **Why:** on 2026-11-10 Markus said "I don't care about my email address leaking btw", and earlier the same day "Don't worry too much about leaking secrets, we can just rotate them if you find them leaked."
- **How to apply:** a cheap automatic check for the S3 access key and secret before committing bulk content is still worth running. Do not block, ask, or redact on account of the email address, and do not stop work on a likely false positive. If a real credential does get pushed, tell Markus at once so he can rotate it.

### Keep this file lean

`AGENTS.md` holds only guidance on how agents should work in this repository. Do not record decisions in it.

- **Why:** Markus said on 2026-11-10: "Stop putting decisions into AGENTS.md, we need a better spot for it. We want to keep that file lean, not a dumping ground."
- **How to apply:** agreed rules go in `docs/spec.md`, with Markus's explicit agreement on the wording. Things that are agreed but not built, and open design questions, go in `docs/todo.md`, and are deleted from it when built. How things were done is read from the code, its tests, and its comments. Research goes in `docs/research/`.

### Keep the README lean too

`README.md` is not a dumping ground for all kinds of information. Things are added to it judiciously.

- **Why:** Markus said on 2026-11-11 that "the readme isn't a dumping ground for all kinds of info, we'll add things there judiciously", when he had the paragraph on cgo and the C compiler removed from it.
- **How to apply:** do not add to `README.md` as a side effect of other work. Details on building, internals, and design go next to the code they describe or in `docs/`. If something seems to belong in the README, propose it to Markus instead of writing it. The section "Status" is the exception: Markus asked for it on 2026-11-13, "we can keep it up to date without dumping everything there". Update it when a piece of the project is merged, and keep it to what is built, what is not, and what is next, in a few lines. Details stay in `docs/todo.md`.

### Work without waiting for Markus, except on design

Commit agreed work and merge pull requests when you judge them ready, without asking Markus first. Involve him in high-level design, architecture, and goals.

- **Why:** Markus said on 2026-11-10: "If it's what we agreed you can just commit. Less friction, I don't want to be the bottleneck in this project", and later the same day: "merge whenever you like. We can always revert. But I'd like to stay involved in high-level design, architecture, goals, etc." On 2026-11-12, when asked to approve the wording of a comment in the schema: "Remember, you own the code. I want to be part of high-level design and architecture decisions (such as specs, data formats), you handle the details."
- **How to apply:** check the work yourself against what was agreed, and commit and merge it if it matches and CI passes. Decide implementation details yourself and tell him what you chose. A detail inside a file he reviewed, such as a comment in the schema, is still a detail: fix it and do not ask. Bring him questions of design, architecture, scope, and goals, and anything that deviates from what was agreed. Wording in `docs/spec.md` still needs his explicit agreement before it is written.

### Second opinions are allowed without asking

The second-opinion skill may be used whenever an independent view helps.

- **Why:** Markus said on 2026-11-10: "Feel free to use second opinion skill whenever you want, but take results judiciously."
- **How to apply:** use it for design decisions, stubborn bugs, and reviews. Treat the result as one input: check its claims before acting on them, and do not let it override what was agreed with Markus.

### Exported conversations go in `docs/conversations/`

When Markus exports a conversation, move the export into `docs/conversations/` and commit it, without asking.

- **Why:** Markus said on 2026-11-11: "every time I export a conversation, just move it (potentially overwriting another one if it's an update to a conversation) and commit."
- **How to apply:** name the file by the date the conversation started and a short topic, like `2026-11-10-kickoff.txt`. If the export is a newer version of a conversation that is already there, overwrite that file. Run the cheap check for the S3 access key and secret first.

### JSON is handled with `encoding/json/v2`

Use `encoding/json/v2` and `encoding/json/jsontext` from the standard library for all JSON. Do not import `encoding/json`.

- **Why:** Markus said on 2026-11-12: "Make sure to use the new json/v2 from Go stdlib". It rejects duplicate names and invalid UTF-8 and matches names exactly by default, which the storage formats need.
- **How to apply:** this holds for code and tests. Read `go doc encoding/json/v2` for its behaviour instead of assuming that of version 1.

### Each builder works in a worktree of its own

A builder gets a git worktree of its own when it is spawned, through the Agent tool's worktree isolation. The lead stays on `main` and does not create or enter worktrees for builders.

- **Why:** on 2026-11-12 the lead created the worktrees and switched between them while builders ran. One builder's working directory moved with the lead, and its writes and git commands in its own worktree were refused, so it could not commit its last diary step. Markus then said: "From now on maybe let builders create their worktrees?" A builder that got its own worktree the same day ran without friction.
- **How to apply:** spawn builders and writers with worktree isolation. The builder writes the first step of the feature's diary from the requirements in its brief. The lead reviews a builder's branch from `main` with `git diff main...<branch>`, pushes it, and opens the pull request. After the merge, remove the worktree and the branch.

### A test must not depend on timing

A test passes or fails for the same reason on every machine, however slow or loaded.

- **Why:** on 2026-11-12 and 2026-11-13 five tests failed only in CI's sanitizer job, which is several times slower than a developer's machine. One assumed that work finished within a busy timeout of five seconds. One told a fake store to hang only after the instance that reads from it had started. One reused a key that an earlier step may or may not have written. None showed a fault in the code under test.
- **How to apply:** set up every fake before the code that uses it starts. Where two orders of events are both legal, make the test accept both or force one. Bound real time only from the side that a slower machine cannot break. Before handing in a new test, run it many times, also under `-race` and with the machine under load (`go test -count=50`), and run `make test-asan` for anything that waits.
