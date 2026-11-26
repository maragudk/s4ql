# Diary: the annotation of a statement is its text only

Markus agreed on 2026-11-26 that the annotation of a statement in a commit holds the statement's text and nothing else: no parameters, no time, no random bytes. The rows of a commit are the truth, and replay never reads an annotation. This task takes the field `parameters` out of the commit format and moves every test that judged the annotation's arguments to a sound link from the annotation to the call that made it.

## Step 1: the format, the code, and the tests

**Author:** builder (annotation-text)

### Prompt Context

**Verbatim prompt:** Your ONE task: **the annotation of a statement in a commit becomes its text only**: the parameters go out of the commit format. [...] 1. **The format:** `Statement` in `proto/s4ql/v1/commit.proto` loses its field `parameters` (reserve its number and name) [...] 2. **The code that writes and reads annotations:** `Tx.Annotate` in `internal/core/tx.go`, its callers in `internal/engine` [...] 3. **The tests keep every check they have, with the arguments from another place.** [...] Find the smallest sound link [...] Do not put an ID into the commit format for the tests' sake [...] Prove with planted faults of your own, one at a time and reverted, that the tests still catch: a statement's text annotated in the wrong commit; two statements' annotations swapped; a statement that ran with another call's arguments. 4. **`internal/simulator`** [...] keep it compiling and its tests green with the smallest change [...] 5. **Docs** [...]

**Interpretation:** Remove `Statement.parameters` from the schema (field 2 reserved), make `Tx.Annotate(ctx, sql)` take the text alone, drop the argument plumbing from the engine's annotate path and its planted bugs, and keep the simulation's, the differential tests', and the planted bugs' checks meaningful without arguments in the log.

**Inferred intent:** A smaller, honest log: an annotation that says which statement wrote the rows and does not pretend to be a complete record of its inputs, while the tests keep their full power to catch a statement in the wrong commit or run with the wrong arguments.

### What I did

- `/proto/s4ql/v1/commit.proto`: `Statement` keeps `sql = 1`, reserves `2` and `parameters`, and its comment says what is agreed. The comment of `Value.real` lost its clause about parameters keeping -0.0. `make generate` rewrote `/internal/s4qlpb/commit.pb.go`; `make lint-proto` passes.
- `/internal/format/check.go`: `checkAnnotation` checks only the text now; `/internal/format/writer.go` and `/internal/format/reader.go` lost their sentences about parameters. The samples in `/internal/format/testdata/commit-v0-*.bin` were written again with `go test ./internal/format -run 'TestCommitSamples|TestManifestSamples' -update`. A new helper `oldStatement` in `/internal/format/reader_test.go` builds a statement of the old shape, with its parameters as unknown field 2; the reader hands out its text and the writer refuses it.
- `/internal/core/tx.go`: `Tx.Annotate(ctx, sql string) error`. The core tests' `note` step and their expected lines lost the parameters (`"note <sql>"`).
- `/internal/engine`: `writeTx.annotate(ctx, sql)`, `bugs.annotation(ctx, sql) string`, `Conn.commitFailed` without args, `textRun` without args. The planted bugs `annotateNext` and `argsFromStart` are removed, with their plantable entries and their rows in `TestSimulation_plantedBugs`.
- `/internal/engine/simulation_test.go`: `logCommit` has no `params`. `run.noteOf(note)` gives the attempt whose ID the text carries and its caller's arguments; the replay of the log in SQLite, `verifyTxs`, the type rule, the arithmetic refusals, and the tally "commits with a parameter" use it. `run.verify` replaces the check "commit holds the parameters ... and its caller gave the arguments" by "commit %v holds the text %q of the statement %v, whose own text is %q", with `ownText` as the statement's bounds in the attempt's text. `annotateText` has that symptom first now.
- `/internal/engine/engine_test.go`: `logAnnotations` gives the text only and fails on an annotation that holds an unknown field. The pinned expectations in `engine_test.go`, `tx_test.go`, `several_test.go`, `/conn_test.go`, and `/tx_test.go` lost their `with ...` parts; where a test named the annotation of an argument, it now checks the argument in the row.
- `/internal/simulator/run.go`: `commitObject` appends `Statement{SQL: s.GetSql()}` and the function `argValue`, used nowhere else, is gone. `/internal/simulator/run_test.go`: a commit's last statement has 0 arguments, and the two comparisons of the argument with the key are gone.
- `/internal/sql/mutation/equivalent.txt`: the four equivalent mutants of `annotateNext` are gone, since `TestEquivalent` refuses a key that the generator no longer makes.
- Docs: `/internal/format/README.md` (Annotations, the -0.0 and NUL rules, the commit time), `/internal/core/README.md`, `/internal/engine/README.md` (Annotations, the simulation's checks, the planted bugs and their tables), `/docs/todo/storage-formats.md`.

### Why

The simulation already wrote a comment `/* id N */` into every statement it runs, also into each statement of a text of several, and the engine annotates that text, so the ID was the link from an annotation to the attempt all along; the arguments were read from the annotation only out of convenience. Taking them from the attempt needs no hook and no field in the format. The new check that the annotation is the attempt's own text keeps the link sound: an annotation of another statement's text, or the whole text, is caught by it before anything else uses the ID.

### What worked

The ID in the text made the change small: `run.noteOf` replaced four copies of the regexp lookup. Reading an old commit object needed no code at all, since the rules of the format already skip unknown fields inside an annotation.

### What didn't work

- The first `go test -short ./internal/engine` panicked with `engine: no planted bug is named annotateNext` from `TestSimulation_plantedBugs`, before I removed the table rows.
- `go test -run TestEquivalent ./internal/sql/mutation` failed with `no such mutant: internal/engine/engine.go Conn.write operator: a.GetInteger() + 1 => a.GetInteger() - 1`, so the brief's "do not touch `internal/sql/mutation`" could not hold for `equivalent.txt`.
- The harness refused two Bash calls that held a heredoc with Python: "this command is too complex to verify that it stays inside the worktree". Writing the scripts to the scratchpad with Write and running them worked.

### What I learned

The planted bug `annotateText` was found before by " and commit " (a statement in two commits, through the first ID of the whole text); the simulation never compared an annotation's text with its statement. The new check is stronger than what it replaces for texts.

### What was tricky

`internal/simulator` builds only with the tag `s4ql_browser`, so `go vet ./...` stays quiet about it; `GOOS=js GOARCH=wasm go vet -tags s4ql_browser ./internal/simulator` and `go test -tags s4ql_browser ./internal/simulator/...` find it.

### What warrants review

- `run.verify` in `/internal/engine/simulation_test.go`: the text check uses `ownText(a.src)`, which assumes the engine annotates exactly `src[Bounds().Start:Bounds().End]` of the attempt's text, as `planOf` does.
- That a commit object of the old shape reads (`/internal/format/reader_test.go`, "should hand out a statement of the shape before version 0 lost its parameters").

### Future work

`Statement.Args` in `/internal/simulator/trace.go` is now always empty; its owner may remove it with the site's display of arguments.

## Step 2: planted faults, self-review, and a check of each commit's rows

**Author:** builder (annotation-text)

### Prompt Context

**Verbatim prompt:** Prove with planted faults of your own, one at a time and reverted, that the tests still catch: a statement's text annotated in the wrong commit; two statements' annotations swapped; a statement that ran with another call's arguments. [...] Run the `fabrik:code-review` skill over your diff and fix what matters.

**Interpretation:** Plant each fault in a copy of the tree, see which check finds it, revert; then let two reviewers read the diff and fix what they agree on or what is serious.

**Inferred intent:** Evidence that removing the annotation's arguments weakened no check.

### What I did

The faults ran in a copy of the worktree at `/Volumes/990/s4ql/annotation-text/fault` (rsync without `.git`), with a file `internal/engine/fault.go` of helpers, so the worktree stayed clean for the reviewers:

- **Wrong commit:** `Conn.write` annotated each statement of its own with the text of the statement before it on the connection. `go test -count=1 -p 4 -run '^TestSimulation$' ./internal/engine/` failed in every workload: "the committed statement 77, ..., is not in the log" and "the replay of the log holds". `TestDifferential`, `TestEngine_annotations`, and `TestConn_ExecAll` failed too.
- **Two annotations swapped:** the core's commit path swapped the first two annotations of a commit object. `TestSimulation` failed with "commit 5 holds the statements [53 52 54 55 57] of the transaction 7, whose statements that succeeded and write are [52 53 54 55 57]", in each workload.
- **Another call's arguments:** `Conn.write` ran the sink with the arguments of the call before it on the connection with as many arguments. `TestSimulation` failed in each workload, by statements that gave another answer than SQLite ("at commit 12 gives ..., and in SQLite ..."). A second variant, each integer argument plus one, was found with `S4QL_ENGINE_SIM_SEEDS=10` by "the replay of the log holds" in 2 seeds of 3.

The two reviewers both found that the third kind was now found only by after-effects: the removed check compared every commit's arguments with the caller's, and the replay compares only the final state. So I added `commitRows` in `/internal/engine/simulation_test.go`: after SQLite ran the statements of commit k with the callers' arguments, it must hold each row that the commit upserts, by its key, and no row of a key that it deletes, for each table that no later change of the commit creates or drops. With it, the plus-one variant was found in 10 places by "it upserts the row 505,2,3 of the table tIEs, and SQLite holds ... rows [\"504,2,2\"] for its key after the statements of the commit". The swapped-arguments variant still showed no such line in 10 seeds: its swaps mostly made inserts fail on UNIQUE, which the answers find first. To keep a guard, the planted bug `argsNext` runs a statement of its own with each integer argument plus one: `S4QL_ENGINE_SIM_SEEDS=200 go test -run '^TestSimulation_plantedBugs$/^argsNext$' -v ./internal/engine/` gave "97 of 200 seeds of the profile arguments find the bug, 95 of them by \"for its key after the statements of the commit\"". The first run of it failed on "seed 2825 finds the bug by another violation: [... gives 19/1555: UNIQUE constraint failed: ..., and in SQLite ...]", so its list of violations has the usual others.

Other fixes from the review: the doc comments of `noteOf`, `ownText`, and `Tx.Annotate` no longer lean on their callers; the proto comments of `Annotation.statement` and `Statement` say "a statement that ran in the commit"; `bugs.annotation` checks the bug before it looks the text up; the duplicate ID lookups in `run.verify` use `of`; two test names in `/conn_test.go`; the refusal of the old shape in `/internal/format/writer_test.go` pins "holds field 2"; the sentence on parameters in the -0.0 rule of the format's README; the engine's README describes `commitRows` and `argsNext`.

Then `git merge main`: `docs/todo/storage-formats.md` conflicted, since main had changed the not-built item when the spec got its sentence ("without their parameters"); I kept my built item without its sentence about the spec. `internal/simulator/run.go` merged on its own; my change there is the same lines as before.

### Why

The brief asked that every check that judged the annotation's arguments still judge something real. `commitRows` judges the same thing per commit, from the rows instead of the annotation, and is stronger: it also finds a statement whose wrong rows a later commit overwrites.

### What worked

Planting faults in a copy of the tree kept the reviewers' view of the worktree clean, and the copy builds on its own since the module has no paths outside it.

### What didn't work

- Running a JSON file with tabs through my replace script: "json.decoder.JSONDecodeError: Invalid control character at: line 1 column 4 (char 3)". I used Edit for those.
- A `sed -i` with a multi-line replacement was refused by the harness as too complex; Edit again.

### What I learned

Removing the draw `intn(n.host, 100)` of the core's simulation changes the random stream of every core seed, as both reviewers said. Keeping it would not keep the runs the same anyway, since the commit objects are smaller and their names differ, so I kept the removal and ran the core's full simulation and its planted bugs at the nightly's size.

### What was tricky

`commitRows` must leave out a table that a later change of the same commit creates or drops, since SQLite then holds the table as that change leaves it; and an upsert that does not list every column, which `verifyRows` reports.

### What warrants review

- `commitRows`: the net effect of a commit holds each key at most once, which the check assumes.
- The new planted bug `argsNext` and its row in the README tables.

### Future work

Outside my scope, for the lead: `docs/roadmap.md` lines 15, 146, 205, and 217 still describe the arguments in the annotation and eight bugs of several statements; `internal/sql/mutation/README.md` line 219 mentions the equivalent mutants of `annotateNext`; `internal/simulator/trace.go` (`Statement.Args`), `internal/simulator/README.md` (lines 613 and 948), and the fixtures of `internal/simulator/sitecheck/check.html` describe arguments of a commit's statements, and the public simulator shows `values (?)` where it showed the key.

## Step 3: the runs at the nightly's sizes

**Author:** builder (annotation-text)

### Prompt Context

**Verbatim prompt:** `make test` and the lint target pass; the whole engine package and the core package pass without `-short`; the night-sized runs pass, one at a time [...] Also the core's planted bugs at the nightly's size if you touch `internal/core` tests

**Interpretation:** Run each long check once, one after another, on the merged branch.

**Inferred intent:** That the change lands with the nightly green.

### What I did

A script, `/Volumes/990/s4ql/annotation-text/runs.sh`, ran the checks one at a time at `81d21d35` (the merge of main), with `GOTMPDIR` and `TMPDIR` on `/Volumes/990`: `make test` (261 s), `make lint` (19 s), `go test ./internal/engine/` (370 s), `go test ./internal/core/` (155 s), `S4QL_ENGINE_SIM_SEEDS=100` `TestSimulation` (401 s), `./drivercheck/` (71 s), `TestDifferential -sequences 400` (662 s), all green; the core's `TestSimulation_plantedBugs` with `S4QL_SIM_SEEDS=1000` in its four shards (922, 837, 1025, and 1002 s), green. The engine's `TestSimulation_plantedBugs` with `S4QL_ENGINE_SIM_SEEDS=1000` (11,925 s) failed in nine bugs, each on a seed that found it by a violation outside its list: `commitRows` ("it upserts the row ... and SQLite holds ... for its key after the statements of the commit") for `listOrder`, `zeroLeftOut`, `keepOldKey`, `nullDefault`, `unturnedBound`, `setFromNew`, `nullCompares`, and `copySelf`, and for `argsNext` "changes no row, is in no commit, and changes 1 rows in SQLite". Each bug writes a row that SQLite does not hold, which is what the check finds, so their lists took the symptom; `arithmeticFinds` took it for the bugs of arithmetic. A rerun of the nine at 1,000 seeds failed once more, in `nullCompares`, by "it deletes the key [text:\"é\"] of the table ReinDex, and SQLite holds ... for it after the statements of the commit": the bug deletes a row that SQLite keeps. `arithmeticFinds` took the delete's symptom too, and `nullCompares` then passed: 486 of 1,000 seeds, 397 of them by ", and in SQLite".

The counts of the run at 1,000 seeds: `annotateText` 999 of 1,000, each by "whose own text is"; `argsNext` 439 of 1,000, 432 of them by `commitRows`; the other bugs of several statements as before.

### Why

The new check is stronger than the one it replaced, so it finds bugs that write wrong rows earlier than their own violations in some seeds. The rule of AGENTS.md is that a list of symptoms is widened only when the bug is shown to cause exactly that symptom, and each seed above shows it.

### What worked

Running the checks one after another from a script, with a summary line for each, kept to one test process at a time without watching each.

### What didn't work

- The first run of the planted bugs, as above.
- I started the rerun while the script had moved on to the core's planted bugs, so two test processes ran at once; I stopped the rerun at once and started it after the script ended.

### What I learned

A stricter check changes which violation a planted bug is found by first, so it needs a run of all planted bugs at the nightly's size, not only of the bugs it was written for.

### What was tricky

Nothing beyond the above.

### What warrants review

The widened lists of the nine bugs in `/internal/engine/simulation_test.go`, and `arithmeticFinds`.

### Future work

None of my own.

## Step 4: two more merges of main

**Author:** builder (annotation-text)

### Prompt Context

**Verbatim prompt:** before you hand in, `git merge main` and resolve conflicts in `internal/simulator` in favour of main's version, then reapply your minimal change.

**Interpretation:** Bring the branch up to main, and run again what the merge can change.

**Inferred intent:** A branch that merges cleanly and stays green.

### What I did

The merge at `55e37d07` conflicted in `/docs/todo/storage-formats.md` (main added the item of a random value in the header; I kept it and dropped the old not-built item of the annotation) and in `/internal/engine/README.md` (main's wording of arithmetic and text, without the annotation's arguments). Main's new test of blob arguments in `/conn_test.go` expected `with x'01'` and the like in the annotations; it now expects the texts, and the rows stay checked by the cache of a second instance. At `55e37d07`, `S4QL_ENGINE_SIM_SEEDS=100` `TestSimulation` passed (388 s), and `S4QL_ENGINE_SIM_SEEDS=1000` `TestSimulation_plantedBugs` passed (11,903 s): `argsNext` 439 of 1,000, 432 by its own violation; `annotateText` 999 of 1,000, each by its own. The merge at `1a43e814` (main's text in the generated workload) had no conflict; `make test`, `make lint`, and `TestSimulation` with 100 seeds (331 s) passed there. `internal/simulator` merged without a conflict each time, and my change there is the same lines.

### Why

Main's text engine and its workload change what the simulation generates, which the new check of the commit's rows sees.

### What worked

The 100-seed simulation is the guard against a false finding of `commitRows`, at a twentieth of the cost of the planted bugs.

### What didn't work

The first `make test` after the merge at `55e37d07` failed: `conn_test.go:917: Expected "insert into r values (1, ?) with x'01'", but got "insert into r values (1, ?)"`.

### What I learned

Nothing new.

### What was tricky

Nothing.

### What warrants review

The planted bugs at 1,000 seeds ran at `55e37d07`, not after the last merge of main's workload of text; the nightly runs them next.

### Future work

None of my own.

## Step 5: an adversarial reading, the stale docs, and the landing

**Author:** builder (annotation-text-land)

### Prompt Context

**Verbatim prompt:** You come fresh to a finished branch that changes the commit format: first read it as an adversary, then tidy what it left stale, then land it. [...] 1. **Read it as an adversary**, the checks first, since a weaker oracle hides faults [...] plant at least five others of your own choosing in the engine's write path and in the annotation path, one at a time, never committed [...] Check two of the eight by running a few seeds each. [...] One outside opinion from agy [...] 2. **Tidy what is stale** [...] 4. **Land.** Merge `origin/main` [...]

**Interpretation:** Try to break the new checks with faults of my own, confirm two widened lists of planted bugs, get one outside view of the format, fix the stale docs, merge main, run the long checks once, and merge the pull request.

**Inferred intent:** That the commit format loses its parameters without the tests losing any power to find a wrong commit.

### What I did

- **Faults of my own**, each planted in the worktree or in a copy of it at `/Volumes/990/s4ql/annotation-text-land/copy`, run with `go test -short -count=1 -run '^(TestSimulation|TestDifferential)$' ./internal/engine/`, and reverted:
  1. Each text argument of a statement of its own ran with an `x` appended (`Conn.write`): `TestSimulation` failed first by `commitRows` ("it upserts the row null,\"Ax\" of the table NO, and SQLite holds ... rows [] for its key after the statements of the commit"), and `TestDifferential` failed too.
  2. `coreTx.annotate` dropped the annotation of every statement whose text holds `delete`: `TestSimulation` failed by "commit 6 holds the statements [68 69] of the transaction 10, whose statements that succeeded and write are [66 68 69]". `TestDifferential` passed, as expected: both of its instances run the same engine.
  3. `coreTx.annotate` cut the `/* id N */` comment out of the text: `TestSimulation` panicked, `index out of range [0] with length 0` in `stmttest.LimitRefusal`, from `arithmeticRefusals` in `run.verify`. The check of the IDs had reported "commit 1 holds the statement ..., of no attempt", but the replay of the log went on and judged the arithmetic of a statement with parameters with no arguments. Before the branch the arguments came from the annotation, so this could not happen. Fixed, see below; the fault now fails by "of no attempt".
  4. The core's spool of a touched table left out the second row of the commit object while the cache kept it (`spoolTouched` in `/internal/core/tx.go`): "the replay of the log holds" in every environment, and `TestDifferential`. `commitRows` does not see a missing row, only a wrong one; the old checks did not either.
  5. A delete in a transaction ran with each integer argument minus one (`writeIn`): `TestSimulation` failed in the default and hammer environments.
  6. An update in a transaction annotated its text twice: "commit 8 holds the statements [82 82] of the transaction 11 ..." and "the statement 82 is in commit 8 and commit 8".
  7. Each statement of a text of several took its arguments from the start of the text (`execAll`): `TestConn_ExecAll`, `TestDifferential`, `TestSimulation`, and three planted bugs' runs failed.
- **Two widened lists:** with a temporary log of each seed found only by the commit's rows, `S4QL_ENGINE_SIM_SEEDS=1000 go test -count=1 -v -run '^TestSimulation_plantedBugs$/^(keepOldKey|nullCompares)$' ./internal/engine/` (238 s): `keepOldKey` 435 of 1,000, five seeds found by `commitRows` alone; `nullCompares` 486 of 1,000, four. Seeds 15299 (`keepOldKey`, "it upserts the row -1 of the table pragma_x, and SQLite holds ... rows []") and 23853 (`nullCompares`, "it deletes the key [text:\"é\"] of the table ReinDex, and SQLite holds ... [\"\\\"it's\\\",\\\"é\\\",3\"]") replayed with `-replay <seed> -workload <w> -bug <bug>` give that violation, and pass without `-bug`.
- **The format:** a test in `/internal/format/reader_test.go` that a statement whose field 2 holds bytes that are no value, or a value of text that is not valid UTF-8, reads with its text: version 0 refused both before, since it decoded the field. A comment on `reserved 2` in `/proto/s4ql/v1/commit.proto` says what the number held. The old shape reads and the writer refuses it, as the branch's tests show.
- **agy** (Gemini, default model) read the diff of `internal/format`, `internal/core`, and the proto from a clean directory: no definite problem; the one change of behaviour it named is the one the new test pins.
- **The fix of fault 3**, in `run.verify` of `/internal/engine/simulation_test.go`: the check of arithmetic of a note runs only where the note has as many arguments as parameters, as `typeRule` and `run.flattened` already require; a note of no attempt otherwise runs in SQLite as it is, as before the branch.
- **Docs:** `/docs/roadmap.md` (seven planted bugs of several statements, the annotation and the simulation's check in slices 2 and 3), `/internal/sql/mutation/README.md` (the equivalent mutants of `annotateNext`), and the notes of `/docs/todo/` that described the annotation's arguments (`slice-02.md`, `slice-03.md`, `arithmetic.md`, `several-statements.md`, whose table lost `argsFromStart` and has `annotateText`'s new counts).
- **The merge of `origin/main` at 6653a01e:** one conflict, in `/internal/engine/README.md`, main's sentence with `update.runIn` and `annotateNext` where the branch had `argsNext`; I took main's and put `argsNext` in. `TestEquivalent` of `internal/sql/mutation` passed without a change of names. `make test` and `make lint` passed.
- **The long runs after the merge**, one at a time from `/Volumes/990/s4ql/annotation-text-land/runs.sh`, all green: `TestDifferential -sequences 400` with `-p 3` (834 s, at the merge `5b84cd7b`), and at `c3284686` `S4QL_ENGINE_SIM_SEEDS=100` `TestSimulation` (434 s), `argsNext` at 1,000 seeds (105 s: 439 seeds find it, 432 by "for its key after the statements of the commit"), `annotateText` at 1,000 seeds (329 s: 999, each by "whose own text is"), and `go test ./drivercheck/` of `internal/sqlite/crosscheck` (68 s). The machine ran mutation testing and another branch's planted bugs beside them.

### Why

The new oracle, `commitRows`, replaced a direct comparison of each annotation's arguments with the caller's, so faults that make a commit wrong had to be tried against it one by one.

### What worked

The planted-bug harness itself proved the widened lists: it runs each seed that finds a bug again without it, so a seed found only by `commitRows` is a symptom of the bug.

### What didn't work

- Fault 3 at first: `panic: runtime error: index out of range [0] with length 0` from `stmttest.LimitRefusal`.
- My first fix skipped a note of no attempt in the replay. Both reviewers found that it put SQLite behind the log, so later checks would report the skip as faults, and that a note of an attempt with fewer arguments than parameters still panicked. I replaced it with the guard by the count of the arguments.
- The harness refused Bash calls that held `git` with other commands or a heredoc into Python with `date`: "this command names git in a form too complex to verify". Plain separate calls and scripts in a file worked.

### What I learned

`commitRows` checks each row a commit holds against SQLite, and not the other way round: a commit that leaves out a row that its statements changed in SQLite is found by the replay of the log and by later answers, not at its commit.

### What was tricky

Planting a fault in the worktree while a long run of the same package was about to compile its next step would have put the fault into that run, so the later faults went into a copy of the tree.

### What warrants review

- The guard `len(given) == s.Parameters()` before `arithmeticRefusals` in `run.verify`.
- The long runs after the merge, below.

### Future work

- `commitRows` could also require that every key whose row the commit's statements changed in SQLite is in the commit, which would find fault 4 at its commit; the replay finds it now.
- `internal/simulator` still has `Statement.Args` in `trace.go`, which is always empty, its README (the `args` of a commit's statements), and the fixtures of `sitecheck/check.html`; its owner removes them with the site's display.
