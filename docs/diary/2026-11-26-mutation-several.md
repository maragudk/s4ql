# Diary: mutation testing of several statements in one `Exec`

The code of pull requests #157, #158, #160, #163, and #168: mutate every declaration that they touch in non-test code, whole, and end every survivor as killed by a test or listed in `/internal/sql/mutation/equivalent.txt` with its reason. The arithmetic round of the engine (`/docs/diary/2026-11-25-mutation-arithmetic-engine.md`) is the model.

## Step 1: The selection, the runner's driver check, and the start of the run

**Author:** mutation-several builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Mutate every declaration that those pull requests touch in non-test code, whole, as the earlier rounds did, and list the declarations in the record. If the runner cannot take a package yet (`internal/sql/parser`, or the root package), extend it as the last round did for `internal/expr`, with the tests that kill its mutants in their layers (for the root package: `conn_test.go` and the driver check in `internal/sqlite/crosscheck/drivercheck`, which is a module of its own)."
**Interpretation:** Select the declarations from the diffs of the five pull requests, run them on a copy of the tree under `/Volumes/990/s4ql/mutation-several/` with 4 workers, and let the root's mutants run the driver check too.
**Inferred intent:** The same trust in the loop of several statements as in the slices before it.

### What I did

- The non-test code that the five pull requests changed, from `git diff --stat` of each: #157 changed `conn.go`, `s4ql.go` (a doc comment only), `internal/engine/engine.go`, `errors.go`, `several.go`, `telemetry.go`, `internal/sql/parser/parser.go`, `internal/sql/stmt/check.go`, and `split.go`; #158 `split.go` (and `stmttest`, which is test code); #160 and #163 only tests and `stmttest`; #168 `bugs.go`, `engine.go`, `several.go`, and `tx.go`.
- The declarations: all of `stmt/split.go`; `Check`, `checkWhole`, and `statementAt` in `stmt/check.go`; `parser.ParseAt`; all of `internal/engine/several.go`; in `bugs.go` the hooks of #168, `bugs.ofText`, `textOf`, `textRun.commit`, `bugs.annotation`, `Conn.commitFailed`, `Conn.inOneCommit`, and `Conn.undo` (the fields of `bugs` have no mutants); `Engine.newReportFrom` in `telemetry.go`; `checkArguments`, `checkValues`, and `countArguments` in `errors.go`; `Conn.exec`, `Conn.execPlan`, `Conn.newReport`, `Conn.newReportFrom`, `Conn.write`, `prepare`, and `planOf` in `engine.go`; `Conn.writeIn` in `tx.go`; and in the driver `conn.ExecContext`, `conn.exec`, `conn.QueryContext`, `conn.PrepareContext`, `arguments`, and `prepared.ExecContext`. 1,125 mutants: 211 in `stmt`, 3 in the parser, 821 in the engine, 90 in the driver. The regular expression is `/Volumes/990/s4ql/mutation-several/sel.re`.
- The runner took the parser and the root already. Two extensions, in `/internal/sql/mutation/run.go`: `TestSplit_differential` runs in the layer `generator` and the seeds of `FuzzSplit`, `FuzzLiteral`, and `FuzzStartedTable` in `fuzz seeds` when `stmt` is mutated (`TestEngineLayers` now checks the fuzz targets of `stmt` too); and a mutant of the root runs the tests of the driver check, in the module `internal/sqlite/crosscheck`, `TestGenerated` in `generator`, the seeds of `FuzzSequence` in `fuzz seeds`, and the others in `other packages`. The build of a package in that module runs the go command in its directory; the module replaces `maragu.dev/s4ql` with the tree, so the overlay of a mutant of the root reaches it. `TestRunner_run` has a tiny module of the driver check and a mutant that only it kills. The driver check takes 27 s on the code as it is.
- `scripts/run.sh` runs the tool on the copy `tree/` with `-j 4 -factor 5` and a `-first` of the tests of the feature. `scripts/emit/` (a copy of the tool's `generate.go` with a `main`, built as `emit.bin`) and `scripts/mut.sh` run `go test -overlay` of the worktree against one mutant by its key, which checks a new test against its survivor. Started the run at 12:12, at a load of 8 on 10 cores.

### Why

The brief asks for the scope of the earlier rounds, by whole declaration, and the driver check is the generated check of the driver's `ExecContext`, which a mutant of the root did not run before.

### What worked

- Selecting by the function in the key, which `-list` prints, instead of by lines: the declarations are whole anyway.

### What didn't work

- A shell command that wrote a file with a here-doc and ran `grep` in the scratch directory was refused: "this command is too complex to verify that it stays inside the worktree". The regular expression went into a file through the Write tool.
- The first expectation of the new case of `TestRunner_run` named line 10 of the tiny driver check; its failure was `TestGenerated: d_test.go:11: not 5`.

### What I learned

- The fingerprint of a run covers the files under `internal`, so the code of the root is not in it; the copy of the tree keeps the run's code fixed anyway.

### What was tricky

- Two other builders change `Conn.write`, `Conn.writeIn`, and perhaps `bugs.annotation` at the same time, for the annotation of a statement. Their mutants run on the code as it is now, and the merge of `main` at the end decides which run again.

### What warrants review

- The extension of the runner in `/internal/sql/mutation/run.go` and its test in `/internal/sql/mutation/run_test.go`.

### Future work

- The survivors of the run.

## Step 2: The run, its survivors, and the merge of `main`

**Author:** mutation-several builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Every surviving mutant gets one of two ends: a test that kills it (prefer the place where such behaviour is already tested; a generated or differential check before a hand-written pin where one fits), or an entry in `equivalent.txt` with the reason why no test can tell it from the original. No survivor stays unexplained."
**Interpretation:** Classify the survivors as they come, with a test or a reason each, and run every survivor and timeout again on the final code.
**Inferred intent:** A round with nothing left open.

### What I did

- The run took 8 h 41 min with 4 workers, planning 12 min 35 s of it, at a load of 8 to 20 on 10 cores beside other builders. 1,125 mutants: 235 not compiled, 756 killed (629 first by the hand-written tests, 127 by the simulation), 27 timed out, 107 survived. Of the 107, 26 were in `equivalent.txt` before (all of the driver's two, those of `Conn.write` and `Conn.writeIn` from earlier rounds), 4 are killed by new tests, and 77 are new entries.
- New tests, each checked against its survivor with `scripts/mut.sh`: `TestConn_ExecAll_panic` in `/internal/engine/several_internal_test.go` (a statement of a text that panics in the engine logs its event with `internal_error`, after the events of the statements before it), which kills the removal of `defer r.panicked(ctx)` in `Conn.execPart`; and two cases of the subtest of `CheckAll` in `/internal/engine/several_test.go`, a text of two statements with the right number of arguments and a text of one statement before a syntax error, with one argument, which kill the three mutants of the condition of a text of one statement in `CheckAll`.
- Two entries of `equivalent.txt` had moved with #168: the branch of `commitLater` in `Conn.write` came before that of `rowsOfFailed`, so the entries `r.rows = n => #2` and `SetsChanges: setsChanges(p) =>` of `rowsOfFailed` are `#3` and `#2` now; the run shows the new keys survive and the old ones are killed.
- Merged `main` (144 commits, among them the upserts, text in expressions, and the round of mutation testing in CI). It changed `Conn.write` and `Conn.writeIn` (an upsert takes no `checkOutside`, and a failed build gives `built(p, err)`), which changes no key of a survivor and replaces 9 mutants of the lines that it changed with 18 new ones, 1,134 in all; and `stmt.Split`, `ExecAll`, and the other declarations not at all.
- The rerun on `tree2/`, a copy of the merged code with the new tests, takes every survivor, every timeout, and the 18 new mutants, 152 in all, with `-min-time 5m` (`scripts/run2.sh`, `rerun.re`).

### Why

The brief's lessons: a timeout counts only after a run alone, and every survivor runs again on the final code.

### What worked

- Classifying the survivors by the file in which they came: almost all of `bugs.go` and of the loop of `execAll` are variants of the planted bugs, which only run with a bug planted.

### What didn't work

- `pgrep -f "mutation-several/out1"` in a wait loop matched the wait loop's own command line, which named the results file; the loop now looks for `mutation -out /Volumes/990/s4ql/mutation-several/`.
- A command that set a shell variable from `jq` and then appended a here-doc in the worktree was refused: "this command is too complex to verify that it stays inside the worktree". The key went in by hand.

### What I learned

- The new rule of `AGENTS.md` on `main`, "Do not start a mutation round on a developer's machine", came in while this run was under way; the run had started under the brief before, and only the rerun of its survivors and timeouts follows it.
- Issue #179, the sample of the round in CI since 8c53a53f, lists 13 survivors of the engine among them, of `bugs.go` and `several.go`, which this round explains.

### What was tricky

- The order of the numbers `#n` of equal keys in a function is that of the walk of the generator, not of the lines: the last `r.rows = n` of `Conn.write` has no number, and the one of `commitLater` is `#2`.

### What warrants review

- The reasons of the entries in the section of several statements at the end of `/internal/sql/mutation/equivalent.txt`, in particular those of `staysComment` and of `Conn.split`.

### Future work

- The rerun's results.

## Step 3: The rerun, the self-review, and the second merge of `main`

**Author:** mutation-several builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Before handing in: `git merge main`; `go test ./internal/sql/mutation/` passes (`TestEquivalent`); for entries that moved in the merge, run the mutant again before you update its entry. `make test` and the lint target pass; the engine package passes without `-short`; new tests pass with `-count=20` and `-race`."
**Interpretation:** Finish the rerun, review the branch, merge `main` again, and run again what the merge changed.
**Inferred intent:** A branch that lands on the code as it is.

### What I did

- The rerun of 152 took 3 h 3 min: 18 killed (the 4 that the new tests kill, 6 of the timeouts, and 8 of the new mutants), 10 not compiled, 10 timed out again, 114 survived. Eight of the survivors had timed out in the first run, at the limit of one minute of the layers `generator` and `mutants` under load, and are equivalent: `Check`'s guard, the tails beside the errors of `statementAt`, `tail <= len(src)` in `Split`, the shortcut of `trimmedEnd`, a guard of `staysComment`, and `annotateText => true` in `bugs.annotation`. The 10 that time out again loop or wait for good.
- The self-review (the `code-review` skill, at medium, over `main...HEAD`) found no bug and four nits. Fixed: `testPackages` returns an error of the stat of the driver check other than its absence, where it left the check out silently; the counts of the merge say that it replaced 9 mutants with 18. Answered: the driver check runs only on mutants of the root, as no test outside `internal` runs on the engine's; the 20 survivors of the engine and `stmt` outside the planted bugs ran against its `TestGenerated` and `TestSeveral` with `scripts/drv.sh`, and all survive. The local rerun follows the brief, which asked for it, and this branch cannot be pushed to start the workflow; it is noted here.
- `main` moved by 66 commits meanwhile, among them the annotation of a statement as its text alone (#191), which removed the planted bug `argsFromStart` and changed `bugs.ofText`, `bugs.annotation`, `Conn.commitFailed`, `Conn.write`, `Conn.writeIn`, and the text of the planted bugs in `Conn.execAll`. The second merge was clean. 68 mutants of the selection went and 38 came; seven entries named mutants that are gone, and are removed. The 38 new ones and the 41 survivors and timeouts of the changed declarations ran again in 1 h 4 min (`out3/`): 23 killed, 16 not compiled, 2 timed out, 38 survived, all in the list, and `bugs.annotation condition: !b.annotateText => false`, which the new shape of `bugs.annotation` lets the simulation kill, left the list.
- Final: 1,104 mutants, 237 not compiled, 755 killed, 10 timed out, 102 equivalent, none unexplained. The record is the section "Several statements in one `Exec`" of `/internal/sql/mutation/README.md`, the section at the end of `/internal/sql/mutation/equivalent.txt`, and the heading "Mutation testing" of `/docs/todo/several-statements.md`.

### Why

The merge changed declarations of the round, so their survivors had to meet the new code and its tests.

### What worked

- Comparing the keys of `-list` before and after a merge, with the last result of each mutant: it named exactly the gone and the new mutants, so the reruns took 152 and 79 mutants and not the whole selection.

### What didn't work

- An edit of `scripts/run2.sh` failed with "File has not been read yet", and the `-list` that followed used the old regular expression and counted 139 mutants instead of 79; after a Read and the edit it counted 79.
- `gofmt -l internal/sql/mutation` listed `run.go`, for a blank line that `main` brought; it is formatted now.

### What I learned

- `main`'s runner plans only the packages that have mutants to run, so a rerun of the engine alone plans in 3 min 35 s, where the first run, which planned the parser and every package that depends on it, took 12 min 35 s.

### What was tricky

- Keeping the counts right over two merges: the table counts the last result of each mutant of the selection on the final code.

### What warrants review

- The removal of the seven entries of `argsFromStart` and the old `bugs.annotation`, and that of `bugs.annotation condition: !b.annotateText => false`.

### Future work

- `internal/sql/mutation/round.txt` is not changed: this round mutated the declarations of the pull requests of several statements, not every declaration since the commit there. Issue #179 lists survivors of `bugs.go` and `several.go` that this round explains.
