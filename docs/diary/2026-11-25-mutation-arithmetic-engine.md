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
