# Diary: Mutation testing of slice 8, `update` and `delete`

Slice 8 of the roadmap went through the statement pass (#88), the engine, the core's set of keys, and the driver (#91), and the generated checks (#94, in review). This task runs the mutation tester `internal/sql/mutation` over the code that slice 8 added or changed, closes the gaps that surviving mutants show or lists them as equivalent, puts the remaining fuzz targets of the engine in the tester's layer "fuzz seeds", and strengthens the detection of the planted bug `openBound`, which the checks of slice 8 made rare. The branch is `update-delete-mutation`, from `origin/update-delete-checks` at `9cc5e8a`, with `origin/main` at `20ac604` and `origin/where-mutation` (#93, the mutation testing of slice 7) at `a1d0b71` merged in.

## Step 1: The requirements, the merges, the layer of the fuzz seeds, and the selection

**Author:** update-delete-mutation builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Scope: the code that slice 8 added or changed (pull requests #88, #91, #94). In the statement package: the recognition of `update` and `delete`, and `recognizer.where`/`conditions` as #88 changed them. In the engine: `changeRows`, `update.check`, `update.run`, the delete path, `engine.check`, the planted bugs `changeNothing` and `keepOldKey`. In core: `Tx.Keys`, `KeySet.Add`, `KeySet.All`, `createKeyTable`, `dropKeys`, the delete of a row. In the driver: `check` and what #91 changed in `conn.go`. [...] 1. Every mutant is killed by a test, or is listed in `equivalent.txt` with a reason a reader can check. [...] 2. Put the fuzz targets that are not yet in the tester's \"fuzz seeds\" layer into it if they belong there by the tester's logic [...] 3. A planted bug has grown thin and you strengthen its detection: `openBound` (slice 7) is found in 8 of 200 seeds of the simulation [...] raise its share the honest way, by making the workload exercise key ranges with open bounds more [...] not by changing what counts as found. Recount all planted bugs afterwards [...]". And from the lead during the run: "when pull request #93 (`where-mutation`) was merged with `main`, a small agent moved two entries in `internal/sql/mutation/equivalent.txt` from `build.filter ... #4` to `build.bind ... #4` [...] Verify with the tester that each of the two entries names the mutant its reason describes and that the reason is still true [...]".
**Interpretation:** Select the functions of slice 8 by `git blame` as the round of slice 7 did, run each package with `-first`, put the six fuzz targets in "fuzz seeds", find out what made `openBound` rare and move it where the workload reaches it, and run the two moved entries through the tester.
**Inferred intent:** The same trust in slice 8 as in slices 1 to 7, and a planted bug whose detection does not hang on a few lucky seeds.

### What I did

- Merged `origin/main` and `origin/where-mutation` into the branch. The first merge of `where-mutation` conflicted in `/internal/engine/README.md`, where slice 8 had added a bullet on the type of the values of SET beside the one that #93 reworded for the merge of `checkCompared` into `checkType`; both stay. `update.check` in `/internal/engine/operator.go` called `checkType` with four arguments, and now passes the use "for", as `insert.check` does. `where-mutation` then got a merge of `origin/main` of its own, which I merged on top.
- `engineLayers` in `/internal/sql/mutation/run.go` puts `FuzzSQL`, `FuzzSQL_core`, `FuzzColumns`, `FuzzColumns_core`, `FuzzChanges`, and `FuzzChanges_core` in "fuzz seeds", as the other fuzz targets of the engine: what runs of them in `go test` is their seeds, and the layer is for seeds. The new `TestEngineLayers` in `/internal/sql/mutation/run_test.go` reads the test files of the engine and fails for a fuzz target that is not in that layer, so that the next one cannot be left out again. The README of the tester names all twelve.
- A throwaway script in the scratchpad (`udm/select.py`) runs `git blame -w --line-porcelain HEAD` on each file of the listing of `-list` and counts, per function, the mutants on lines whose commit is in #88 (`db555bc^1..db555bc^2`), #91 (`321794c^1..321794c^2`), #94 (`origin/main..origin/update-delete-checks`), or the merge above. From that list, and the brief, the selection of 900 mutants:
  - `stmt`, 162: `recognizer.update`, `recognizer.delete`, `recognizer.where`, `recognizer.conditions`, the methods `Parameters`, `Bounds`, and `setSpan` of `Update` and `Delete`, `Select.Parameters`, and the changed lines of `recognizer.statement`, `recognizer.selectCmd`, and `fieldNames`.
  - The engine, 455: `build.change`, `build.filter`, `changeRows`, `update.check`, `update.run`, `remove.check`, `remove.run`, `finds`, `createTable.check`, `dropTable.check`, `check`, `setsChanges`, `readOnly.delete`, `readOnly.keys`, `noSuchSetColumn`, the `keys`, `add`, `all`, and `delete` of `coreKeys` and `coreTx`, and the changed lines of `build.conditions`, `buildSink`, `Conn.checkOutside`, `Conn.write`, `Conn.writeIn`, `NewPlan`, `Format`, `rowsOf`, and `report.of`. The planted bugs `changeNothing` and `keepOldKey` are a line each in `changeRows` and `update.run`.
  - The core, 271: `Tx.Keys`, `KeySet.Add`, `KeySet.All`, `KeySet.after`, `Tx.createKeyTable`, `Tx.dropKeys`, `Tx.touch`, and `Tx.Delete`, the delete of a row, which slice 8 did not change but which every delete and every change of a key now runs.
  - The driver, 12: `check` of `conn.go`, the only function with a mutant that #91 changed there; its change of `s4ql.go` is a doc comment.
- Started the runs of `stmt`, the core, and the driver on a copy of the tree (`udm/tree1`), and one of the two moved entries of `build.bind`.

### Why

The engine's run waits for the change of the workload of `openBound`, since its simulation layer runs the planted bugs and the run's results belong to one state of the tests.

### What worked

- The script of the round of slice 7, with the ranges of three pull requests in place of one, gave the selection in one run.
- The driver's run: 12 mutants, 5 not compiled, 7 killed, none survived.

### What didn't work

- A here-doc that wrote the script into the scratchpad was refused: "This agent is isolated in the worktree [...], but this command is too complex to verify that it stays inside the worktree." The script went through the editor, as the round of slice 7 found.
- `git reset --hard HEAD`, to start the merge of `where-mutation` again after it had moved on, was refused by the permission system as destructive. I committed the resolved merge instead and merged the newer `where-mutation` on top, which gave the same tree.

### What I learned

- #88 and #91 are merge commits, so `git blame` gives their lines to the commits of their branches; the range of the second parent from the first is the pull request.

### What was tricky

- `recognizer.conditions` has only one changed line by `git blame`, its signature, which has no mutant; the brief names it whole, and the stmt run takes it whole.

### What warrants review

- The selection: whether a function of slice 8 is missing, above all in `stmttest`, which is code for tests and is not mutated.
- `Tx.Delete` in the selection, which slice 8 did not change.

### Future work

- The engine's run, after the change for `openBound`, and the survivors of every run.
