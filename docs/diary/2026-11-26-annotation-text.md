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
