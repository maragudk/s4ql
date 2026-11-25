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

### glue and goqite are a milestone, not a specification

s4ql's own design decisions take precedence over what glue and goqite happen to need.

- **Why:** Markus said on 2026-11-24, when asked whether `without rowid` stays required although none of glue's tables has it: "take care not to overfit to accomodate goqite and glue. They just serve as a milestone. If anything in their design conflicts with ours, let me know, but assume that our decisions here in s4ql take precedence."
- **How to apply:** decide what s4ql supports from its own rules: the spec, exact types, SQLite's answer or an error, and no compatibility quirks. Use glue and goqite to choose what comes next, not to decide how it works. When something in their design conflicts with s4ql's, tell Markus, and assume that they change (he owns both), not s4ql.

### Nothing is in use yet, so formats break freely

Do not build an upgrade or a migration for the commit format, the manifest, or the cache while s4ql is used only in its own tests.

- **Why:** on 2026-11-20 a builder had written an upgrade in place of an old cache to a new layout, and a review found a concurrency bug in it. Markus: "none of this is used outside our tests yet, so breaking changes to the commit object format are fine", and on the upgrade: "Yes, remove it."
- **How to apply:** when a format or the cache's layout changes, change it, raise the version where one exists, and let the old one be refused with the error that is there for it. Tell builders so in their briefs. This ends when something outside the tests uses s4ql, or at version 1 of the formats, whichever comes first.

### Low ceremony on email address and secrets

Do not spend effort scrubbing Markus's email address from documents, transcripts, or research notes committed here, and do not pause work over possible secret leaks.

- **Why:** on 2026-11-10 Markus said "I don't care about my email address leaking btw", and earlier the same day "Don't worry too much about leaking secrets, we can just rotate them if you find them leaked."
- **How to apply:** a pre-commit hook of git checks every commit for secrets, so do not check by hand before committing. Markus on 2026-11-19: "there's a git precommit hook for secrets now, you don't have to check anymore". Do not block, ask, or redact on account of the email address, and do not stop work on a likely false positive. If a real credential does get pushed, tell Markus at once so he can rotate it.

### Keep this file lean

`AGENTS.md` holds only guidance on how agents should work in this repository. Do not record decisions in it.

- **Why:** Markus said on 2026-11-10: "Stop putting decisions into AGENTS.md, we need a better spot for it. We want to keep that file lean, not a dumping ground."
- **How to apply:** agreed rules go in `docs/spec.md`, with Markus's explicit agreement on the wording. Things that are agreed but not built, and open design questions, go in the files of `docs/todo/`, which `docs/todo.md` indexes, and are deleted from them when built. A builder records the decisions and notes of its step in the file of its piece of work, under a heading for its step, and an open item in the file of its area, so that two branches on different pieces of work never write to the same file: for slices 1 to 15 that is the file of the slice, `docs/todo/slice-NN.md`, and from then on a file named after the feature of the piece of work, such as `docs/todo/expressions.md`. How things were done is read from the code, its tests, and its comments. Research goes in `docs/research/`.

### Keep the README lean too

`README.md` is not a dumping ground for all kinds of information. Things are added to it judiciously.

- **Why:** Markus said on 2026-11-11 that "the readme isn't a dumping ground for all kinds of info, we'll add things there judiciously", when he had the paragraph on cgo and the C compiler removed from it.
- **How to apply:** do not add to `README.md` as a side effect of other work. Details on building, internals, and design go next to the code they describe or in `docs/`. If something seems to belong in the README, propose it to Markus instead of writing it. The section "Status" is the exception: Markus asked for it on 2026-11-13, "we can keep it up to date without dumping everything there". Update it when a piece of the project is merged, and keep it to what is built, what is not, and what is next, in a few lines. It says what s4ql supports now, by feature, not slice by slice, and the slices and how each is checked are in `docs/roadmap.md`. Markus on 2026-11-20: "the status block should be condensed, so we don't have to write all slices out, but just what we support now". Details stay in `docs/todo/`.

### Work without waiting for Markus, except on design

Commit agreed work and merge pull requests when you judge them ready, without asking Markus first. Involve him in high-level design, architecture, and goals.

- **Why:** Markus said on 2026-11-10: "If it's what we agreed you can just commit. Less friction, I don't want to be the bottleneck in this project", and later the same day: "merge whenever you like. We can always revert. But I'd like to stay involved in high-level design, architecture, goals, etc." On 2026-11-12, when asked to approve the wording of a comment in the schema: "Remember, you own the code. I want to be part of high-level design and architecture decisions (such as specs, data formats), you handle the details." On 2026-11-18, when asked which literals and errors the first slice of SQL includes: "I think you should decide on these low-level decisions. Use your best judgment." On 2026-11-25, when shown the names of new exported errors, the order in which a row's conditions are evaluated, and a refusal of one kind of update: "I don't want to decide on error names, that's details for you. The other stuff honestly looks like implementation details too".
- **How to apply:** check the work yourself against what was agreed, and commit and merge it if it matches and CI passes. Decide implementation details yourself and tell him what you chose. A detail inside a file he reviewed, such as a comment in the schema, is still a detail: fix it and do not ask. What a single statement, literal, or error does within an agreed piece of work is a detail too: decide it by SQLite's behaviour and the agreed rules, record it in the file of its piece of work in `docs/todo/` or the roadmap, and tell him. The names of exported errors, when an error of evaluation comes, and which single forms of a statement are refused are details in the same way: do not put them to him as questions, and keep what he is told about them to a line. Bring him questions of design, architecture, scope, and goals, and anything that deviates from what was agreed. Wording in `docs/spec.md` still needs his explicit agreement before it is written.

### On the public site, only the roadmap is kept up to date, at the level of milestones

Do not update the public site in `site/` (served at www.s4ql.com) as a side effect of other work, except the milestones in its "Roadmap" section.

- **Why:** Markus said on 2026-11-16: "you don't need to keep the public site updated automatically, I'll do that once in a while", and then allowed updates to the site's status "and the roadmap." On 2026-11-25 he had the front page's "Status" section removed and its roadmap cut from a row per slice to a few milestones: "In particular the roadmap is getting way too long. We want to present the project properly to a new user, not give a detailed status."
- **How to apply:** when a piece of the project is merged, update the "Status" section of `README.md` and `docs/todo/`. Update the section with the id `roadmap` in `site/index.html` only when a milestone changes its state, such as from next to built, or when its one-line summary would become wrong. Built work stays visible on the site as built milestones, and the slice-by-slice record stays in `docs/roadmap.md`; Markus on 2026-11-19: "don't delete the old slices entirely from the roadmap? It's nice to see progress". Leave everything else in `site/` alone unless Markus asks. Neither the lead nor the builder makes these updates: the lead spawns a small sub-agent for them after the work is merged, so that the lead's context stays clean and the builder concentrates on the code. Markus: "I want to keep your context clean. Delegate", and: "A small subagent after the work is done sounds better to me."

### On the site, design before words

Make usage self-evident through the design of a page, and keep its words to a minimum.

- **Why:** Markus said on 2026-11-24, when he had the note above the simulator's SQL box removed and the page's introduction cut to one line: "It's better to use the design to make usage self-evident where possible. Minimize words."
- **How to apply:** before adding an explanation, a hint, or a label to a page in `site/`, see whether the layout, a default value, or the state of a control can say it instead. Add text only where the design cannot carry it, and keep it short. This holds for the pages' interface, not for `docs/`.

### A new page on the site gets a preview image of its own

A page added to the public site in `site/` gets its own image for link previews, with its own headline, and does not reuse the image of another page.

- **Why:** the simulator's page went live with the front page's `og.png`, so its preview showed the front page's headline under the simulator's title. Markus said on 2026-11-19: "The simulator has no OpenGraph images etc. like the front page, it should have", and then: "make a memory note that new pages on the site need new images."
- **How to apply:** when a page is added, make an image of 1200 by 630 pixels in the style of `site/og.png`, with the page's own headline, and point the page's `og:image` tags and their alt text at it, beside the title, description, `twitter:card`, favicon, and touch icon that every page has. Check the result with a card extractor after the deploy. When a page's headline changes, its image changes with it.

### maragubot posts project updates on Bluesky without asking

maragubot reports interesting updates in this project on Bluesky, as @maragubot.com, and decides itself when to post.

- **Why:** Markus said on 2026-11-16: "Your job is now to report on interesting updates in this project. Feel free to post whenever you think it's a good idea", "Make sure to monitor project progress", and "Avoid Claudisms: https://claudisms.ai".
- **How to apply:** post with the `bsky` CLI when something is merged that a reader outside the project would care about, such as a piece of the project that is built, a slice of the roadmap, or a test result with numbers. Skip small fixes, documentation changes, and changes to the site in `site/` (Markus on 2026-11-16: "Ignore changes to the website"). Check every post against https://claudisms.ai/claudisms.md before posting: no em dashes, no "not X, but Y", no announcing that something matters. Every number in a post comes from the code, a commit, or a pull request. Decide for each update whether it starts a new post or replies to an earlier one; Markus on 2026-11-16: "Doesn't all need to be in the same thread, up to you". The first post about s4ql is at://did:plc:ccrzw4q3x5cknyo5uqsti74x/app.bsky.feed.post/3n2gxgjffxb2d.

### Second opinions are allowed without asking

The second-opinion skill may be used whenever an independent view helps.

- **Why:** Markus said on 2026-11-10: "Feel free to use second opinion skill whenever you want, but take results judiciously."
- **How to apply:** use it for design decisions, stubborn bugs, and reviews. Treat the result as one input: check its claims before acting on them, and do not let it override what was agreed with Markus. The bugs that it reports are often true on paper and do not matter in practice. Markus on 2026-11-18: "be wary of theoretical-only bugs. Codex has a tendency towards that", and: "The bugs are often true, but insignificant. True on paper, but don't really matter. So make a judgment call for each reported issue." Decide for each reported issue whether it matters enough to fix, and leave the ones that do not.

### Exported conversations go in `docs/conversations/`

When Markus exports a conversation, move the export into `docs/conversations/` and commit it, without asking.

- **Why:** Markus said on 2026-11-11: "every time I export a conversation, just move it (potentially overwriting another one if it's an update to a conversation) and commit."
- **How to apply:** name the file by the date the conversation started and a short topic, like `2026-11-10-kickoff.txt`. If the export is a newer version of a conversation that is already there, overwrite that file.

### The lead's conversations with Markus can be read in `docs/conversations/`

The exports in `docs/conversations/` are the full record of the lead's conversations with Markus, up to the moment of each export.

- **Why:** a compacted session and a new session start from a summary, and a summary leaves things out. Markus asked on 2026-11-18 for a note that the conversations can always be found there, "in case the compacted session leaves out details".
- **How to apply:** when a detail is missing after a compaction or in a new session, such as the exact wording of a decision or the reason for it, search the exports before guessing or asking Markus again. Spawn a small sub-agent for the search and have it report only the answer, with the quote that supports it. Do not read an export into your own context, because they are large. Markus on 2026-11-18: "That's also a good use case for a small subagent." What is in `docs/spec.md`, `docs/roadmap.md`, and `docs/todo/` is newer than a conversation where they differ.

### JSON is handled with `encoding/json/v2`

Use `encoding/json/v2` and `encoding/json/jsontext` from the standard library for all JSON. Do not import `encoding/json`.

- **Why:** Markus said on 2026-11-12: "Make sure to use the new json/v2 from Go stdlib". It rejects duplicate names and invalid UTF-8 and matches names exactly by default, which the storage formats need.
- **How to apply:** this holds for code and tests. Read `go doc encoding/json/v2` for its behaviour instead of assuming that of version 1.

### Each builder works in a worktree of its own

A builder gets a git worktree of its own when it is spawned, through the Agent tool's worktree isolation. The lead stays on `main` and does not create or enter worktrees for builders.

- **Why:** on 2026-11-12 the lead created the worktrees and switched between them while builders ran. One builder's working directory moved with the lead, and its writes and git commands in its own worktree were refused, so it could not commit its last diary step. Markus then said: "From now on maybe let builders create their worktrees?" A builder that got its own worktree the same day ran without friction.
- **How to apply:** spawn builders and writers with worktree isolation. The builder writes the first step of the feature's diary from the requirements in its brief. The lead reviews a builder's branch from `main` with `git diff main...<branch>`, pushes it, and opens the pull request. After the merge, remove the worktree and the branch.

### The lead delegates heavily, also small jobs

The lead hands off not only building but the routine around it, to keep its own context for the conversation with Markus and for decisions.

- **Why:** on 2026-11-20, after a long session in which the lead itself reviewed diffs, opened pull requests, waited for CI, merged, cleaned up, and checked each deploy of the site, Markus said: "Remember to delegate to subagents heavily to conserve your context. Doesn't have to be whole builders each time."
- **How to apply:** give a small sub-agent, on a small model where that is enough, each job whose steps are known: taking a finished branch through its pull request, its checks, the merge, the cleanup, and the check of the deploy; a text change of a few lines; a search; reading a log. Its brief names the rules of this file that bind it, and asks for a report of a few lines. The lead keeps what needs judgement: what to build, whether a result is what Markus asked for, and anything that deviates from what was agreed.

### A builder's task fits well inside its context

Split the work so that no builder comes near the limit of its context.

- **Why:** on 2026-11-18 the builder of the engine for slice 1 had used 700,000 of its 1,000,000 tokens before it was done. Its brief held the code and every kind of test at once: differential tests, simulation with planted bugs, mutation testing, and fuzzing. Markus: "It might make sense to split work up more in the future?"
- **How to apply:** the first builder writes the code and the tests that show it works, and its branch is merged. Each heavy check after that, such as simulation with planted bugs, mutation testing, or a long fuzz run, goes to a fresh builder that starts from the merged code. Where two pieces of code do not depend on each other, give them to two builders. Tell every builder to commit and to write its diary step as it goes, so that a fresh builder can take over if it runs out.

### Large scratch files go on the larger disk of the Mac Mini

This rule applies only on the Mac Mini, which has a second, larger disk. Other machines, such as the Raspberry Pi 5 (`pi3`), have one large disk, so there large scratch files go in `~/scratch/s4ql/` on that disk. Not in the session's scratchpad or `/tmp`: on the Raspberry Pi `/tmp` is a `tmpfs` of 4 GB that lives in memory. The rest of the rule holds everywhere: each agent uses a subdirectory of its own and deletes only that.

On the Mac Mini, scratch files that can grow large go in `/Volumes/990/s4ql/`, each agent in a subdirectory of its own named after its branch. This covers the working copies of mutation testing runs, fuzz corpora under construction, and benchmark data. They do not go in the session's scratchpad or under `/tmp`. Go's build cache and module cache are on that disk already (`go env GOCACHE GOMODCACHE`), so a run uses the default ones and sets no cache of its own. An agent deletes its own subdirectory when its work is merged, and never anything outside it.

- **Why:** in the night of 2026-11-20 to 2026-11-21 the machine's main disk filled to zero bytes with the scratch directories of mutation testing rounds, about 40 GB in three directories. No command could start, so every agent stopped until a run happened to free space. Then two builders that had been told to clean up their scratch files deleted the whole shared scratchpad, which killed another builder's runs twice. Markus on 2026-11-21: "Sorry about the disk. Maybe check space once in a while. There's a larger disk you can use at /Volumes/990/s4ql/ ." On 2026-11-23, about the Raspberry Pi: "there's just one big disk here, so add that it only applies to the Mac Mini".
- **How to apply:** the lead names the directory in the brief of every builder whose task runs mutation testing, long fuzzing, or anything else that copies the source tree many times, and tells every agent to delete only a subdirectory of its own. On the Mac Mini, the lead checks free space on both disks with `df -h / /Volumes/990` before each larger round of sub-agents and when a builder reports odd I/O errors, and treats results recorded while a disk was full as void. Small scratch files still go in the session's scratchpad. If `/Volumes/990` is not mounted on the Mac Mini, say so to Markus and fall back to an untracked directory inside the agent's own worktree, kept under 5 GB.

### A pull request is merged only after its checks have run and passed

Before merging, confirm that the pull request has checks and that every one of them passed. Confirm after merging that it is merged, and only then clean up.

- **Why:** on 2026-11-19 the lead merged pull request #41 before its CI had run. The watch on its checks returned at once with "no checks reported", because GitHub had not registered them yet, and a pipe in the merge command hid the exit code that should have stopped it. CI passed afterwards. On 2026-11-18 a merge that the API refused was followed by a cleanup that deleted the branch and closed the pull request. `main` has no required status checks, because they would also block direct pushes; Markus: "I just want to avoid too much friction while still supporting correctness."
- **How to apply:** count the checks with `gh pr checks <n> --json bucket`, and merge only if there are as many as CI has jobs and none is in another bucket than `pass` or `skipping`. The job `changes` skips the jobs that a change cannot affect, so a skipped job is fine, and `changes` itself always runs and must pass. Put no pipe between a check and the command that depends on it. An empty list of checks means "not run yet", never "passed". After `gh pr merge`, read the state with `gh pr view <n> --json state` before removing the branch or the worktree. A pull request that changes a workflow file is merged locally and pushed, because the API refuses it. One exception, for the simulator of the site (`internal/simulator`, `site/simulator`): a job that does not build or run what the pull request changes need not be waited for, such as `asan`, which runs without the tag `s4ql_browser`. Markus on 2026-11-19: "Feel free to merge early again for CI jobs that don't make sense for the simulator." Check in the Makefile that the job leaves the changed code out before relying on this, and wait for every job that covers it.

### A test never skips itself, except under `-short`

A test that cannot run fails. It does not skip.

- **Why:** a skipped test looks like a passing one, so a missing credential or variable turns a check off without anyone seeing it. Markus said on 2026-11-19, about the tests against a real bucket: "the tests shouldn't skip but fail on missing creds. That goes for all tests btw, never skip tests unless it's because of -short."
- **How to apply:** `testing.Short()` is the only reason for `t.Skip`. A test that needs something from outside, such as a bucket and its credentials, fails with a message that says what is missing, and skips under `-short`. Something that only a person runs with a variable, such as the replay of one seed, is not written as a test that skips when the variable is missing.

### A test must not depend on timing

A test passes or fails for the same reason on every machine, however slow or loaded.

- **Why:** on 2026-11-12 and 2026-11-13 five tests failed only in CI's sanitizer job, which is several times slower than a developer's machine. One assumed that work finished within a busy timeout of five seconds. One told a fake store to hang only after the instance that reads from it had started. One reused a key that an earlier step may or may not have written. None showed a fault in the code under test.
- **How to apply:** set up every fake before the code that uses it starts. Where two orders of events are both legal, make the test accept both or force one. Bound real time only from the side that a slower machine cannot break. Before handing in a new test, run it many times, also under `-race` and with the machine under load (`go test -count=50`). The whole run under the sanitizer and the fuzz tests are CI's work, where they run in parallel jobs: run `make test-asan` locally only for a change to the SQLite binding (`internal/sqlite`) or to a fuzz test, and then only the packages that the change touches. Markus asked on 2026-11-19 whether all builders wait on the long fuzz tests locally as well; they did, once or more per task.
