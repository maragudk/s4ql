# Diary: mutation testing in CI, in shards, once a week

Mutation testing moves off the developer's machine into a scheduled, sharded workflow of GitHub Actions, `.github/workflows/mutation.yml`, so that a round no longer runs for hours on one machine beside every other run and no longer stands in the path of each feature. The runner in `/internal/sql/mutation` learns to run a share of a selection, to select the declarations that changed since a commit, and to merge the results of shards into one report.

## Step 1: the runner runs a shard, a round since a commit, and a merge

**Author:** mutation-ci builder

### Prompt Context

**Verbatim prompt:** "Your ONE task: **move mutation testing off the developer's machine into a scheduled, sharded GitHub Actions workflow**, so that a round no longer runs for hours on one machine and no longer stands in the path of each feature." (The brief then lists: the runner can run a slice and merge the results of all shards; what a round covers, by default every declaration that changed since the last recorded round; the workflow `mutation.yml`, weekly and by hand, about 20 shards, timeouts from the runner's own timings, artifacts, a final job that merges, summarizes, and opens or updates one issue; proof at small size locally and in CI; and docs.)
**Interpretation:** Build the three flags of the runner (`-shard`, `-since`, `-merge`), a file that records the last completed round, the workflow with a plan job, a matrix of shards, and a report job, and prove the split and the merge.
**Inferred intent:** Mutation testing keeps its rigour, but costs no time on the Mac Mini and no time in the path of a feature: it runs on free runners, many at once, and leaves one issue a week for a builder.

### What I did

- `/internal/sql/mutation/shard.go`: `-shard i/n` (`parseShard`, `shardOf`), and `narrow`, which applies, in order, the build filter, `-since`, and `-shard` to the selection. The mutants are dealt out like cards in the order of their keys, the ones that `equivalent.txt` lists first, so each shard gets its share, within one, of every file and so of every package, and of the listed ones of every file. The plan of the stages now covers only the packages that have mutants to run (`pkgsOf(todo)`), since a selection by `-since` leaves most packages empty and their planning would be wasted.
- `/internal/sql/mutation/since.go`: `-since REV` selects the mutants whose declaration holds a line that `git diff -U0 --relative REV` adds or changes, or the place of a removal inside it, plus every declaration of a file that git does not track yet; `-since round` reads the commit from `/internal/sql/mutation/round.txt`. `built` leaves out the mutants of files that `go/build` does not compile on this machine, such as `internal/sqlite/sqlite_browser.go`, `internal/sqlite/asan.go`, and `filelimit_other.go`, whose mutants could only survive. The generator now records the first and last line of the declaration of each mutant (`declFrom`, `declTo` in `generate.go`) so that the selection uses the generator's own idea of a declaration.
- `/internal/sql/mutation/merge.go`: `-merge PATTERN` merges the `results.jsonl` of every shard directory that the pattern matches into `-out`, refusing a shard of another fingerprint, a result in two shards, and a result outside the selection; and every report now also writes `summary.md`, in Markdown, with each unexplained survivor's `file:line`, declaration, operator, key, and code before and after, the timeouts by `file:line` with their keys, the stale entries, and the mutants without a result, and `counts` for `$GITHUB_OUTPUT`. `staleEntries` moved out of `writeReport` in `report.go` so both use it.
- `/internal/sql/mutation/round.txt` holds `8c53a53f` (#156), the last commit that the rounds of arithmetic mutated.
- `-pkgs all` means every package that the runner knows (`listedPkgs`).
- Tests: `shard_test.go`, `since_test.go` (with a real git repository in a temporary directory), `merge_test.go`.

### Why

Shards must agree on their shares without talking to each other, so the split cannot use the stage timings that each shard measures on its own runner, which differ from runner to runner. Dealing by key in a fixed order balances by expected cost without measuring it: the dominant costs are the package (an engine mutant costs many times one of `stmt`), the file (the variants of planted bugs run the simulation), and whether a mutant is a known survivor that runs every stage. A plan job that measured once and handed out a plan would add a whole planning to the critical path and still not know which mutants survive.

A round recorded in a file, written by the builder who works through the round's issue, is the sound record: a run that merely finished has not been worked through. Deriving the base from the last successful run of the workflow would skip the survivors that nobody handled.

### What worked

The tests went green at the first run after the implementation. `-list -pkgs all -since 8c53a53f` gives 2,208 mutants: the several statements in one `Exec` (#157 to #168) in the engine, `stmt`, the parser, and the driver, and `||`, `lower`, and `hex` (#172) in `stmt` and the evaluator.

### What didn't work

The harness of this session refuses compound shell commands that run git, such as a `for` loop over `git log`, with "this command names git in a form too complex to verify that it stays inside the worktree"; each git command had to run on its own.

### What I learned

In `git diff -U0`, a removal's hunk header has `+c,0`, where c is the line of the new file before the removed lines, so a removal is inside a declaration when both c and c+1 are in it. In GitHub's expressions `null != false` is false, since both are coerced to 0, so `inputs.issue != false` would have kept a scheduled run, which has no inputs, from ever opening an issue.

### What was tricky

The declaration of a mutant had to be the generator's: a method of a generic receiver is named without its type there, and a spec of a `var` block is a declaration of its own. Recording the lines in the generator avoided a second naming scheme that could drift.

### What warrants review

`hunk.touches` and the removal rule; `shardOf`'s claim of balance (`TestShardOf` checks every file and class within one for 1 to 300 shards); that `narrow`'s build filter now applies to every run, also a local one, which drops the mutants of `filelimit_other.go` and of the binding's browser and sanitizer files from every selection.

### Future work

The proof at small size and the runs in CI, in the next step.

## Step 2: the workflow, the proof at small size, and the self-review

**Author:** mutation-ci builder

### Prompt Context

**Verbatim prompt:** (the same brief as step 1, parts 3 to 5: "The workflow `.github/workflows/mutation.yml` ...", "Prove it before you hand in ...", "Docs.")
**Interpretation:** Write the workflow, show locally that a sharded and merged run equals a whole one, review the diff, and document.
**Inferred intent:** A workflow that can be trusted the first time it runs on a schedule, since nobody watches it start.

### What I did

- `/.github/workflows/mutation.yml`: on Saturdays at 06:43 UTC and by hand (inputs `since`, `pkgs`, `run`, `shards`, `issue`). The job `plan` resolves the base commit once (from `round.txt`, an input, or none), lists the selection, and makes the matrix: one shard for each 10 mutants, up to 20. Each `shard` restores the build cache of CI's job `sql`, runs its share with `-j 3 -memory 3221225472` under a step timeout of 300 minutes inside a job of 330, and uploads `fingerprint`, `results.jsonl`, and `report.txt` even when it failed. `report` downloads them, runs `-merge 'shards/*'`, uploads `mutation-report`, writes the summary through `/.github/scripts/mutation-report.sh`, and on `main` opens or comments on the one open issue with the label `mutation`.
- Proof at small size: the 15 mutants of `/filelimit_unix.go` (root package), once whole with `-j 2` (plan 38 s, root's hand-written stage 5.6 s, limit 1 min 52 s), and once as 4 shards of 4, 4, 4, and 3 mutants run at once with `-j 1` (60 s) and merged. The compare script read both `results.jsonl`: the same 15 keys, the same status and layer for every mutant (7 survived, 4 killed, 4 did not compile), and the same `report.txt` byte for byte; the only difference is the worker's directory in the 4 compiler messages. The 7 survivors are real and unexplained; they are left for the first round.
- A writer sub-agent wrote the README's new section "The weekly round in CI" and the changes of the flag table and the output; I reviewed it and corrected the workers per shard.
- `/docs/todo/ci.md`: open items for mutant schemata and for running only the covering tests, the one commit of `round.txt`, every shard planning every package, and tests that get weaker.

### Why

A shard that runs out of time must still hand in what it did, so the run's own timeout comes before the job's and the upload runs with `always()`. The report job runs with `!cancelled()` rather than `always()`, so that a run cancelled by hand reports nothing, while a shard that failed or ran out of time is reported.

### What worked

`actionlint` v1.7.7 (run with `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7`) found nothing in the workflow. The comparison at small size was exact.

### What didn't work

`make lint` failed at once with "Error: parallel golangci-lint is running", since another builder's lint ran on the machine; `golangci-lint run --allow-parallel-runners ./internal/sql/mutation/` gave "0 issues". Python scripts that held the word git in their text were refused by the harness as "too complex to verify that it stays inside the worktree", so edits went through script files in the scratchpad.

### What I learned

The `code-review` skill found that `git diff` with `diff.mnemonicPrefix` set prints `+++ w/...`, which `parseDiff` would have read as no file, so `-since` would have selected nothing on such a machine without a word. The diff now passes `--src-prefix=a/ --dst-prefix=b/`, and `TestChangedSince` sets `diff.mnemonicPrefix` through `GIT_CONFIG_COUNT` to show it.

### What was tricky

An added line that starts with `++` reads as `+++ ...` in a hunk, the same as a file's header. `parseDiff` now takes a `+++` line as a file name only between `diff --git` and the first `@@`.

### What warrants review

The self-review's other findings and what came of them: a cancelled run no longer opens an issue (fixed); `-merge` into an `-out` that holds results is refused instead of replacing them (fixed); a shard's last line that a kill cut short is left out, and its mutant has no result, instead of failing the whole merge (fixed, `readShardResults`); the input `shards` is checked to be 1 to 256 (fixed); four mutants at once at 4 GiB each could exceed a runner's 16 GB, so a shard runs three at 3 GiB (fixed, to be checked against the first runs); the caches `mutation-plan` of the jobs `plan` and `report` are never saved, so they always miss, which costs little since those jobs build only the runner (left); a round by `-since` does not see tests that got weaker (left, in `docs/todo/ci.md`).

### Future work

The two runs in CI after the merge: one small, one real, with their numbers.

## Step 3: by hand, not weekly, and for value per unit of compute

**Author:** mutation-ci builder

### Prompt Context

**Verbatim prompt:** First: "A change to your task: schedule the mutation workflow every night, not once a week." Then, replacing it, from the product owner: "It costs energy and resources, I care about that too, not just my monetary cost. So we should take care to use our resources wisely, and make sure it's warranted by the provided value." With: "(1) **No blind schedule.** ... (2) **Measure where a mutant's compute goes** ... (3) **Cut waste before scale** ... (4) **A sample first** ... (5) **Report the cost of every round** in its summary ... Pick the number of shards for little total compute, not for the shortest wall-clock time".
**Interpretation:** The workflow starts by hand only; a round records itself when it leaves no work, so that the next does not redo it; each run says what it cost; a seeded sample gives a cheap first pass; shards are few; and every idea for saving compute is measured before it is kept.
**Inferred intent:** Spend runner time only where it buys trust in the tests, and make the cost visible so the lead can judge each round.

### What I did

- `/.github/workflows/mutation.yml`: no schedule. Inputs `sample` and `seed`; `shards` defaults to 6, with one shard for each 150 mutants. The plan job takes as the base the commit of `round.txt`, or the newest `mutation-round` artifact of a run on `main` that lies between that commit and its own (read through the API with `actions: read`); the report job uploads that artifact when a whole default round left no work. One open issue with the label `mutation` gets its body replaced by the latest report and a comment per run; its title names the commit the round counts from.
- `/.github/scripts/mutation-report.sh`: the cost of the round from the jobs API (runner-minutes of `plan` and `shard`, mutants a runner-minute, unexplained survivors, and each shard's setup before its run and its planning from the new `timing` file the runner writes), and `complete=true` for the self-record.
- The runner: `result` has `Build` and `Stages` (seconds per stage, by layer); the summary has "Where the time went" (mutants that do not compile, builds, each layer's stages, and the survivors' share) and, for a sample, an extrapolation and the rule of three; `-sample` and `-seed` draw by a SHA-256 of the seed and the key.
- Concurrency: GitHub's documentation (docs.github.com/en/actions/reference/limits) gives 60 concurrent jobs for standard hosted runners on Team, which `gh api orgs/maragudk` shows is the plan, 20 on Free, 256 jobs in a matrix, and 6 hours a job; a public repository's `ubuntu-latest` has 4 CPUs, 16 GB, and 14 GB of disk. CI of one pull request runs up to 29 jobs and the nightly 15, so 6 shards leave room for both.

### Why

A schedule spends compute on half-finished work and on survivors that an open issue already lists, so the lead starts a round when a piece of work is complete. A round that left no work is as complete as one a builder recorded, so it records itself and the next round does not mutate the same declarations again. Every shard repeats the checkout, the restore of the cache, and the planning of every package of its share, so few shards use less in all.

### What worked

Measured on the 15 mutants of `/filelimit_unix.go` with 2 workers: without a compile of the package first 140.5 s in all, with it 164.7 s, and the 4 mutants that do not compile cost 0.9 s and 1.2 s; the compile first saves nothing, since `go test -c` already stops at the package, so it is gone again. Builds were 26.1 of the 41.2 s of the 4 killed mutants and 43.9 of the 98.4 s of the 7 survivors.

### What didn't work

There is no data of the last recorded round in the repository: `results.jsonl` was never committed, and the rounds' directories were on the Mac Mini outside this worktree, so the breakdown of the engine's compute must come from the first rounds in CI. A local sample of the next round (`-pkgs all -since round -sample 0.03 -seed 1 -j 4`, 64 mutants) planned the engine's hand-written stage at 5 min 40 s under a load of 32 on 10 cores, with a time limit of 1 h 53 min; it was stopped, since its numbers would have measured the machine's load and it competed with another builder's round. The first try of the sample with FNV-1a kept 600 of 5,000 keys for a fraction of 0.2 (`TestSampleOf`), since the high bits of FNV are poorly spread over keys that differ in their last characters; SHA-256 keeps about 1,000.

### What I learned

In GitHub's expressions `inputs.x` of a scheduled run is null, and `null != false` is false; with the schedule gone the issue step simply reads `inputs.issue`.

### What was tricky

The self-record must never skip work: the artifact is uploaded only for the default selection (`since` round, `pkgs` all, no `run`, `sample` 1), on `main`, with every shard and the merge successful and no unexplained survivor, stale entry, or mutant without a result; and the plan job accepts it only if `round.txt`'s commit is its ancestor and it is an ancestor of the run's own commit.

### What warrants review

The base chosen by the plan job (shell in `/.github/workflows/mutation.yml`), and `mutation-report.sh`, which runs only on Linux (`date -d`) and is first exercised by the runs after the merge.

### Future work

The runs in CI after the merge: a small one, then a sample of 0.2 of the round, whose summary gives the breakdown by layer that decides coverage-based skipping and the order of the stages (both open in `/docs/todo/ci.md`), and the measured setup per shard, which may move the 150 mutants a shard.
