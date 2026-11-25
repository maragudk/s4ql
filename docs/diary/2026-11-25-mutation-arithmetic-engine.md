# Diary: mutation testing of arithmetic in the engine

The arithmetic of pull requests #154 and #156 in `internal/engine`: mutate every declaration that `git diff ac97306..main` touches in the engine's code, and end every survivor as killed by a test or listed in `/internal/sql/mutation/equivalent.txt` with its reason. Slice 15's round (`/docs/diary/2026-11-24-mutation-15.md`) is the model. `internal/expr` and `internal/sql/stmt` are another builder's at the same time.

## Step 1: The selection and the start of the run

**Author:** mutation-arithmetic-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Mutate every declaration that `git diff ac97306..main` touches in the non-test code of `internal/engine` (all of `expression.go`, and the touched declarations in `operator.go`, `plan.go`, `engine.go`, `tx.go`, `subquery.go`, `telemetry.go`, `bugs.go`, and any other), whole, as the earlier rounds did. List the declarations in the record. Start the long run early and in the background, and work on survivors as results come in; commit and write the diary as you go, so that a fresh builder could take over if your context runs out."
**Interpretation:** Select by whole declaration, as slices 14 and 15 did, run the engine on a copy of the tree under `/Volumes/990/s4ql/mutation-arithmetic-engine/` with at most 4 workers, and classify the survivors as they come.
**Inferred intent:** The same trust in the engine's arithmetic as in the slices before it.

### What I did

- The scripts are in `/Volumes/990/s4ql/mutation-arithmetic-engine/scripts/`. `pick/main.go` (built as `pick.bin`) reads the saved `git diff -U0 ac97306..main` of the eight code files of the engine that changed (`diff.txt`), marks every top-level declaration that holds an added line or the place of a removal (`decls.txt`), and prints the mutants of the tool's `-list` whose line is in one (`sel.txt`). `run.sh` runs the tool on the copy `tree/` with `-run` set to the exact `file:line` of each selected mutant, `-j 4`, `-factor 5`, and `-first 'TestEngine_arithmetic|TestNewPlan|TestPair_arithmetic'`, which take 4 s together.
- The declarations: all of `expression.go`; in `operator.go`, `condition`, `condition.perRow`, `bounds`, `condition.holds`, `condition.computed`, `condition.valueOf`, `update`, `update.check`, `update.perRow`, `update.newRow`, `update.run`, `errStopped`, `build.walk`, `build.bind`, `build.computeAndReach`, `build.constants`, `build.values`, `build.types`, `build.arithmetic`, `build.reach`, `build.change`, and `update.bind`; in `plan.go`, `Condition`, `Arithmetic`, `Arithmetic.expr`, `rowsOf`, `term`, `formatNode`, `formatExpr`, and `formatArithmetic`; in `engine.go`, `Conn.write` and `ran`; in `subquery.go`, `subqueries.widths`, `subqueries.types`, `build.compute`, and `build.value`; `outcomeOf` in `telemetry.go`; `Conn.writeIn` in `tx.go`; and in `bugs.go` the struct `bugs`, which has no mutants: the hooks of the seven planted bugs of arithmetic are in the declarations above. 2,488 mutants, 195 of them in `build.change`, 172 in `Conn.write`, and 154 in `condition.holds`.
- Started the run at 17:37 at a load of 14 on 10 cores, with swap nearly full and another mutation run beside it.

### Why

The brief asks for the scope of slices 14 and 15, by whole declaration.

### What worked

- Building the picker with `go build -C dir`, and running it with plain absolute paths.

### What didn't work

- A command that changed into the scratch directory, wrote a `go.mod` with a here-doc, built, and ran the picker in one line was refused: "this command is too complex to verify that it stays inside the worktree". Split into a Write of `go.mod`, a `go build -C`, and a plain run.

### What I learned

- `bugs.go` changes only the fields of the struct `bugs`, which the generator does not mutate.

### What was tricky

- Nothing yet.

### What warrants review

- The selection: `decls.txt` and `sel.txt` in the scratch directory, and the list of declarations above.

### Future work

- The survivors of the run.

## Step 2: The first survivors, and the stop of the run

**Author:** mutation-arithmetic-engine builder

### Prompt Context

**Verbatim prompt:** (the coordinator, mid-run) "Stop your mutation run now: the machine's main disk is down to 5 GB free, because macOS's `fseventsd` has grown to 51 GB of swap under the file activity of the mutation runs. Kill the runner and its test processes cleanly (only your own, under `/Volumes/990/s4ql/mutation-arithmetic-engine/`), keep its results so far and whatever lets the run resume where it stopped, commit what is committable, and note in the diary which mutants are done. Treat any mutant that was in flight when you stop as not run. Do not restart anything and start no test runs until I message you again."
**Interpretation:** Stop the runner and my own waiting loops, keep `out1/` and `tree/`, and record where the run stands.
**Inferred intent:** Free the machine without losing the two hours of results.

### What I did

- Before the stop: the run gave its results at about 2.4 a minute, at a load of 14 to 25 with another run beside it; the planning took 3 min 28 s, the hand-written stage 77 s, and the 16 mutants of `Conn.write` that time out each take its limit of 6 min 25 s.
- `scripts/surv.sh` lists the survivors of a `results.jsonl` that `equivalent.txt` of the worktree does not hold; `scripts/emit/` (a copy of the tool's `generate.go` with a `main`, built as `emit.bin`), `scripts/mut.sh`, and `scripts/many.sh` run `go test -overlay` of the worktree against one mutant by its key; `killed_new.txt` holds the survivors that a new test kills, each checked so.
- Survivors closed so far: 10 of `leftmostOffset`, the offset of `row value misused` for a subquery of `*` on the right of an expression that a select's condition tests. The cases of `TestEngine_subquery` held no expression whose first leaf is a negative literal or 0; eight new `pin` cases compare the offset with SQLite's, for `k + 1`, `5 - k`, `0 * k`, `-5 + k`, `-5 * k * 2`, `2.5 * k`, `-2.5 * k`, and `-0.0 + k`. And 6 of `argumentLengths`, its internal error for a parameter that the arguments do not have, which no statement gives: `TestArgumentLengths` in the new `/internal/engine/expression_internal_test.go`.
- Stopped the runner (the background task of `run.sh`) at 19:43, after 2 h 6 min, and my three loops that waited on the count of results. No runner, `go test`, or test binary of mine is left.

### Where the run stands

- 288 of the 2,488 mutants have a result in `/Volumes/990/s4ql/mutation-arithmetic-engine/out1/results.jsonl`: all 178 of `engine.go` (`Conn.write` and `ran`), and 110 of `expression.go`, from `missingName` into `bindArithmetic`, in the order of the file. 169 killed, 16 timed out, 72 not compiled, 31 survived. Of the 31, 11 are in `equivalent.txt` already (those of `Conn.write` from earlier rounds), 16 are killed by the new tests, and 4 are open: `argumentLengths swap arguments` of its internal error, which the exact message of `TestArgumentLengths` should kill but was not checked before the stop, and three of the internal error of `bindArithmetic` for a column that the table does not have (its removal and two swaps of its arguments).
- The mutants that were in flight have no result, and count as not run. The run resumes where it stopped with the same command, `scripts/run.sh /Volumes/990/s4ql/mutation-arithmetic-engine/sel.txt /Volumes/990/s4ql/mutation-arithmetic-engine/out1`: the tool skips every mutant with a result, and the copy `tree/`, whose fingerprint `out1/` holds, is unchanged.

### Why

The coordinator stopped every run for the main disk, at 4 GB free when I checked.

### What worked

- A planted fault's exact `file:line` in `-run`, and the keys of `results.jsonl` with `jq`, to pick what to check by hand.

### What didn't work

- Waiting with a foreground `until` loop: after 180 s the harness moves it to the background, and twice I then started a second copy by mistake, which I stopped.

### What I learned

- The case `v < 0` of `leftmostOffset` was reached by no test: the subtests of `row value misused` had only expressions that start with a column.

### What was tricky

- Nothing beyond the slow run.

### What warrants review

- The eight new `pin` cases in `/internal/engine/subquery_test.go`, which compare their offsets with SQLite's.

### Future work

- Resume the run when the coordinator allows it; check the four open survivors above.

## Step 3: The resumed run, and the survivors of `expression.go` and `operator.go`

**Author:** mutation-arithmetic-engine builder

### Prompt Context

**Verbatim prompt:** (the coordinator) "The machine was restarted: swap is 0 and the main disk has 47 GB free. Resume your mutation run now where it stopped, with the runner's parallelism at 6. Three things: (1) the 16 mutants that were counted as killed by a timeout were run while the machine was thrashing, so run those 16 again on the quiet machine before they count, and say in the record how each ended; (2) note the rate of the resumed run in the diary; (3) carry on with the task as briefed: no survivor unexplained, the four open ones included, then the README section, `equivalent.txt`, the notes in `docs/todo/arithmetic.md`, `make test`, lint, the whole engine package without `-short`, and the final report. If free space on `/` falls under 20 GB (`df -h /`), stop the run as before and report."
**Interpretation:** Take the 16 timeouts out of `results.jsonl` so that the tool runs them again, resume with `-j 6`, and go on classifying.
**Inferred intent:** Results that do not depend on the state of the machine.

### What I did

- Kept the results of the first part as `out1/results-before-restart.jsonl` and its 16 timeouts as `out1/timeouts-before-restart.jsonl`, and wrote `out1/results.jsonl` without them, so the tool ran them again. The resumed run (`run2.log`) started at 20:33 with 2,216 mutants to run with 6 at once, at a load of about 4 after the restart, and planned the stages again in about 3 min: the hand-written stage took 64 s on the quiet machine, 77 s before. A watch stops me if `/` falls under 20 GB; it stayed at 45 to 47 GB.
- **The 16 timeouts again.** Each of the 16 timed out again, at about 62 s, in the first stage (`-first`, whose limit is 1 min): 15 mutants of `Conn.write` that leave a transaction open or skip its roll back, so that the next statement of the test waits for good, and `leftmostOffset remove statement: e = e.Operands[0]`, which loops for good. They count as timed out.
- **The rate.** 326 results in the first 1 h 13 min of the resumed run, 4.4 a minute, while the mutants of `update.check` and the timeouts ran; then 7.9 and 10.7 a minute: 1,300 of the 2,216 after 3 h 8 min, about 7 a minute over all, against 2.4 a minute before the restart.
- **New tests**, each checked against its survivors with `scripts/many.sh` (which now tells a missing key from a kill):
  - `TestBindArithmetic` (a column that the table does not have), `TestFromExpr`, `TestClass_String`, and `TestCompareNumbers` in `/internal/engine/expression_internal_test.go`.
  - In `TestFilter` of `/internal/engine/operator_internal_test.go`: the internal errors of a pair (`a = b`) whose second column the row does not have; the internal errors of a computed condition (position, a text in the row, two classes, an operator), and that a computed condition that overflows is not true and fails, on either side and with the value first; a filter without a projection that refuses a type (`build.bind`); a read-only update or delete that computes no constant and reads no index (`build.constants`); the internal error of a pair in `build.types`.
  - `TestUpdate_newRow`: the internal errors of a copy of a column that the row does not have and of an assignment of no value.
  - In `TestEngine_arithmetic`: the refusals of the two sides of a comparison in the order of the text, a TEXT column on the left before a text argument on the right and the other way round.
- **Code changed.** The message of the internal error of `compareNumbers` for two classes, "a comparison of an real with a integer", reads "a comparison of values of the classes real and integer" now. And the planted bug `nullCompares` was never planted in a comparison of two columns, which its comment in `/internal/engine/bugs.go` promises: `build.types` set `paired` and went on before `build.arithmetic`, which sets the flag, so its branch in `condition.holds` was dead, which is why every one of its 25 mutants survived. `build.types` sets the flag for a pair now; `TestDifferential_plantedBugs/nullCompares` and `TestSimulation_plantedBugs/nullCompares` still find it.
- Equivalent entries so far are at the end of `/internal/sql/mutation/equivalent.txt`.

### Why

Each survivor needs a test or a reason.

### What worked

- A throwaway test file that prints what a mutant does, run with `scripts/mut.sh`, to tell a real gap from an equivalent mutant: it showed that `build.bind` is reached only by a plan without a projection, and that the mutant of `build.constants` reads the indexes of a read-only sink.

### What didn't work

- `many.sh` first counted every run without `ok` as a kill, so a key that no longer named a mutant (after the change of the message of `compareNumbers`) showed as killed; it now prints `MISSING` with the emitter's "no mutant with key".
- A command that appended to a file in the scratch directory and then changed into the worktree for `git` was refused: "this command changes directory to a location computed at runtime before running git". Split in two.

### What I learned

- A survivor that sits only in a planted bug can show that the planted bug itself is dead code, as for `nullCompares` of a pair.

### What was tricky

- The order of the checks: a mutant that binds the value before the tested side changes only which refusal of two comes, and the tests pinned only one refusal at a time.

### What warrants review

- The one-line change in `build.types` that plants `nullCompares` in a pair.
- The reasons of the entries for `build.types` and `build.arithmetic` in `/internal/sql/mutation/equivalent.txt`.

### Future work

- The rest of the run, and a rerun of the mutants of the changed declarations on the final code.
