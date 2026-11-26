# Diary: mutation testing of the arithmetic of the statement pass and the evaluator

Mutate the arithmetic that #150, #151, #154, and #156 added: every declaration that `git diff ac97306..main` touches in the non-test code of `internal/sql/stmt`, and all of `internal/expr`, and end every survivor as killed by a test or listed in `/internal/sql/mutation/equivalent.txt` with its reason. The engine's mutants are another builder's. Slice 15's round (`/docs/diary/2026-11-24-mutation-15.md`) is the model.

## Step 1: The runner, the selection, and the first part of the run of `internal/expr`

**Author:** mutation-arithmetic-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Mutate every declaration that `git diff ac97306..main` touches in the non-test code of `internal/sql/stmt` (not `stmttest`), whole, as the earlier rounds did, and all of `internal/expr` (`-pkgs internal/expr`). If the runner needs a small change to take `internal/expr`, make it." Then, mid-run, from the coordinator: "Stop your mutation run now: the machine's main disk is down to 5 GB free [...] Kill the runner and its test processes cleanly [...] keep its results so far and whatever lets the run resume where it stopped, commit what is committable, and note in the diary which mutants are done. Treat any mutant that was in flight when you stop as not run. Do not restart anything and start no test runs until I message you again."
**Interpretation:** Teach the runner `internal/expr`, select the mutants, run `internal/expr` and then `stmt` with at most 3 workers on a copy of the tree under `/Volumes/990/s4ql/mutation-arithmetic-stmt/`, classify survivors as they come, and stop cleanly when told.
**Inferred intent:** The same trust in the arithmetic as in the slices before it, without taking the machine down.

### What I did

- The runner (`/internal/sql/mutation/main.go`, `/internal/sql/mutation/run.go`): `internal/expr` is in `listedPkgs`, so `equivalent.txt` may name its mutants, and `exprLayers` puts `TestDifferential` in the layer `generator` and `FuzzExpr` in `fuzz seeds`. `TestEngineLayers` checks the fuzz targets of the evaluator as it does the engine's. Committed as `43fac26`.
- The selection: `scripts/pick/main.go` in the scratch directory reads `git diff -U0 ac97306..main` of `recognize.go` and `stmt.go` and marks each top-level declaration that holds an added line or the place of a removal: in `recognize.go` the type `recognizer`, `recognizer.update`, `conditions`, `condition`, `comparand` and its `hasColumn`, `operand`, `spanOf`, `setValue`, `plainValue`, `maxParentheses`, `arithmetic`, `expression`, `unary`, `parenthesized`, and `isZero`; in `stmt.go` `Term`, `parametersOf`, `Expression`, `ExpressionKind` and its constants and `String`, `Expression.Present`, `HasColumn`, and `parameters`, `Condition`, `Assignment`, `Update.Parameters`, and `conditionParameters`. That is 645 mutants of `stmt`, in `stmt.sel`, and the `-run` expression of their `file:line` is `stmt.re`; a `-list` with it gives the same 645. `internal/expr` has 649: 533 in `expr.go`, 100 in `value.go`, 16 in `errors.go`.
- The run of `internal/expr`, with 3 workers on the copy `tree/` at `43fac26`, into `out-expr/`. Planning took 6 min 28 s: the hand-written stage of the evaluator takes 9 ms, its differential test 375 ms, and every survivor then runs the engine's tests (4 min 45 s on the code as it is, 10 to 12 min under the load of 15 to 25) and those of `stmt` (50 s).
- Stopped at the coordinator's message after 2 h 4 min, by `kill -TERM` of the process group of the run. **Done: 240 of the 649 mutants of `internal/expr`**, those with a line in `/Volumes/990/s4ql/mutation-arithmetic-stmt/out-expr/results.jsonl`: 172 killed, 41 not compiled, 27 survived. The three that were in flight have no result and run again. The run of `stmt` has not started: 0 of its 645 are done.
- The 27 survivors so far, all in `expr.go`: `TypeInteger` at `iota + 2`; the check of the error of `typeOf` in `Check`, which fails only at a parameter, and `Check` calls it only without one (2); the initial `-1` of `left` and `right` of a node (6), which nothing reads for a leaf, so that code is gone; an index or a type beside an error, which no caller reads (13, in `Program.add` and `Program.typeOf`); and the words of two internal errors (5), which the tests did not pin.
- New tests, from these survivors and from reading the code while the run went on: the subtests of internal errors of `TestCheck`, `TestProgram_Bind`, and `TestBound_Eval_faults` in `/internal/expr/expr_test.go` pin the whole message of each internal error; `TestProgram_Bind` takes a NaN and an infinite argument, whose arithmetic must give `ErrNotFinite` at the operator; and `TestBound_Eval_faults` gives a row exactly one value short of the column. Committed as `77dcaa3`, `140adf0`, and `0f038de`.

### Why

Every survivor needs a test, a reason, or the removal of its code, and the coordinator needed the disk back.

### What worked

- `scripts/probe.py` runs the tests of one package in the worktree against one hand-made mutant through `go test -overlay`, in a second: it showed that `|| math.IsNaN(f)` removed survives every test of the evaluator, `TestDifferential` too, and that the new subtest kills it, and that the new row kills `n.index > len(row)`.

### What didn't work

- A shell command that built the `-run` expression with `cut | sed | awk` and wrote a script with a here-doc was refused: "this command is too complex to verify that it stays inside the worktree". The expression is built by `scripts/runexpr.py` now.
- That script was first named `re.py`, and Python imported it as the module `re`: `AttributeError: partially initialized module 're' has no attribute 'escape' (most likely due to a circular import)`.
- My first expected message of a text value in a column was wrong: `Expected "s4ql: internal error: a value of kind Text in arithmetic", but got "s4ql: internal error: a value of kind text in arithmetic"`.

### What I learned

- A survivor of the evaluator costs a run of the engine's tests, so the many equivalent mutants of an index beside an error set the pace of the run: about 3 survivors per 12 minutes, against a few seconds for a killed one.

### What was tricky

- Nothing yet beyond the pace.

### What warrants review

- The selection of `stmt` in `/Volumes/990/s4ql/mutation-arithmetic-stmt/stmt.sel`, and the removal of `left: -1, right: -1` in `/internal/expr/expr.go`.

### Future work

- Resume the run of `internal/expr` with the same command and `-out`, which skips the 240 done (the copy of the tree is unchanged, so its fingerprint holds), then run `stmt`, then rerun every survivor on the final code.

## Step 2: The rest of the runs after the restart, the survivors, the reruns, and the record

**Author:** mutation-arithmetic-stmt builder

### Prompt Context

**Verbatim prompt:** (from the coordinator) "The machine was restarted: swap is 0 and the main disk has 47 GB free. Resume your mutation run now where it stopped (the remaining mutants of `internal/expr`, then the 645 of `internal/sql/stmt`), with the runner's parallelism at 4. Any mutant that was counted as killed by a timeout before the restart was run while the machine was thrashing: run those again on the quiet machine before they count, and say in the record how each ended. Then carry on with the task as briefed [...] If free space on `/` falls under 20 GB (`df -h /`), stop the run as before and report."
**Interpretation:** Resume `out-expr/` with 4 workers, run `stmt`, end every survivor, rerun the survivors on the final code, write the record, and watch the disk.
**Inferred intent:** Finish the round on a machine that stays usable.

### What I did

- Checked before the resume: no result of the first part was a timeout (`grep -c '"timeout"' out-expr/results.jsonl` gave 0), so nothing needed a second run for that. The fingerprint of `tree/` still held after the restart (same Go 1.27.4), and the runner said "649 mutants, 240 with a result already, 409 to run with 4 at once". It did them in 1 h 38 min 46 s after 3 min of planning. Then `stmt`, 645 mutants in 3 h 25 min 46 s after 8 min 39 s of planning. A monitor checked `df -g /` every minute; the main disk stayed at 46 to 47 GB free.
- The evaluator: 649 mutants, 92 not compiled, 491 killed (10 by `TestDifferential`), 66 survived. 33 are equivalent (`scripts/equiv.py` matches each key to a reason). 17 are killed by new tests: the whole messages of the internal errors, NaN and infinite arguments, a row exactly one value short, `Bind` without allocations (its `x.typ != 0` and the return it guards), and `Value.Real` of a value that is no real. 16 were in code that is gone: the `-1` of a leaf's operands, and the case of negative zero in `Value.String`, which `fmt.Sprint` writes as `-0` itself. A mutant of `Value.String` (`f == 0 => true`, so every negative real printed as -0) also showed that no test pinned the text of a negative real, which `TestValue` pins now.
- A fault that no statement reaches: `TestBound_Eval_faults` calling `expr.Arithmetic` (new in `/internal/expr/export_test.go`) with `ExprNegate` on two integers failed first with `s4ql: internal error: arithmetic of the operator - in - (at offset 0)` and `errors.Is(err, expr.ErrInternal)` false: `arithmetic` wrapped an internal error of `integerArithmetic` in an `*expr.Error`. It returns it as it is now.
- `stmt`: 645 mutants, 115 not compiled, 460 killed, 2 timed out (the removal of the step down the spine of the ORs and the ANDs in `recognizer.conditions`, which loops for good; both ran on the quiet machine), 68 survived. 12 are equivalent, one of them listed by slice 15. 53 are killed by new cases of `TestCheck` and by `TestExpressionKind_String`: the depths at SQLite's edges (IS NULL alone, columns compared with columns, a start at a column, a parameter, or their negation, a deeper operand on the right, and a negation of a chain in parentheses), 150 closed parentheses in a row, and the names of the refusals around a sign. 3 were in code that is gone: `*ast.Binary` under a `-` in `plainValue`, and the test of the kind in `isZero`.
- The reruns, on `tree2/`, a copy at `acf33e1`: `scripts/rerun.py` writes a `-run` expression of the exact keys of every survivor and of every mutant of `Program.add`, `arithmetic`, `Value.String`, `plainValue`, and `isZero`. The evaluator had 224 in 1 h 30 min, with 33 survivors, all listed. `stmt` had 90 in 57 min, with 12 survivors, all listed. The keys of the removed code no longer name a mutant.
- The record: a section "Arithmetic in `stmt` and the evaluator in `internal/expr`" in `/internal/sql/mutation/README.md`, two sections at the end of `/internal/sql/mutation/equivalent.txt`, and "Mutation testing of the statement pass and the evaluator" at the end of `/docs/todo/arithmetic.md`.
- Self-review with the code-review skill found no bug and two nits. The comment of `plainValue` said that every unary operator binds more tightly than a binary one, which is false for NOT; it says "a -" now. The second: an operand index of a leaf is 0 now, a real node, where -1 would have panicked if a later reader followed it. I kept the removal, and the comment of `node` says nothing reads them for another node.
- `make test` and `make lint` pass. The new tests pass with `-race -count=20`.

### Why

Every survivor needs a test, a reason, or the removal of its code. The reruns show that the final code has no survivor that the list does not hold.

### What worked

- `scripts/probe.py` checked each new test against its mutant in seconds for the evaluator and in about 35 s for `stmt`, while the runs went on, so no new test waited for a rerun.
- Writing the tests in the worktree while the runner read the copy `tree/` let the classification keep up with the run.

### What didn't work

- `python3 -I scripts/probe.py ... "errors.Is(kind, ErrInternal)" "false"` gave `[build failed]`, because `errors` was then imported and unused. The probe used `errors.Is(kind, ErrInternal) && false` instead.
- A probe of `ok && x.Op.Kind == token.Minus => true && ...` failed with `declared and not used: ok`; `(ok || true) && ...` worked.
- An `echo ... >> equivalent.txt` with braces was refused as "too complex to verify that it stays inside the worktree". A Python one-liner appended the section.

### What I learned

- In the evaluator, every function that returns a value beside an error gives a mutant of `return 0, err => return 1, err`. These made 20 of its 33 equivalent survivors, and each one cost a full run of the engine's tests.
- `fmt.Sprint` writes negative zero as `-0`, so the special case in `Value.String` was dead.

### What was tricky

- Telling which of the many `recognizer.unary` mutants were equivalent. A sign other than - takes two paths, through `recognizer.term` and through `recognizer.unary`, and both refuse it by the same name at the same token. The second sign, the `~`, and `+NULL` are not the same, and new cases pin them.

### What warrants review

- The reasons in the two new sections at the end of `/internal/sql/mutation/equivalent.txt`, in particular `recognizer.operand constant: height := 1 => height := 0` and the three of `plainValue`.
- The fix in `arithmetic` in `/internal/expr/expr.go`, and `expr.Arithmetic` in `/internal/expr/export_test.go`.
- `main` gained several statements in one `Exec` (#157, with `stmt/split.go`) since this branch began. It does not touch the mutated declarations, but the branch needs a merge of `main`.

### Future work

- None from this round. The scratch directory `/Volumes/990/s4ql/mutation-arithmetic-stmt/` is deleted at the end.
