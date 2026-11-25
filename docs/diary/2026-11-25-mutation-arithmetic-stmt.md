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
