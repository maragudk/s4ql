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

## Step 2: Why `openBound` grew thin, and the hammer workload

**Author:** update-delete-mutation builder

### Prompt Context

**Verbatim prompt:** (from the brief) "`openBound` (slice 7) is found in 8 of 200 seeds of the simulation, the second time only at seed 55 of the default 100. Find out why the workload reaches it so seldom now (the checks of slice 8 changed the mix of statements) and raise its share the honest way, by making the workload exercise key ranges with open bounds more (as an earlier round did for `convertReal` by moving it to the hammer workload), not by changing what counts as found. Recount all planted bugs afterwards (`S4QL_ENGINE_SIM_SEEDS=200`), update the engine README's table, and confirm that none is found without its bug."
**Interpretation:** Measure what the bug needs before and after the sequence wrote updates and deletes, and run the bug where the workload gives more of it.
**Inferred intent:** A planted bug whose detection does not hang on a few seeds, found by the same violation as before.

### What I did

- The bug needs a select with WHERE, compared with SQLite, on an engine that reads by the key, whose closed bound of the key is the key of a row. `git archive b1d380d^` put the tree before the hooks of slice 8 in the sequence into the scratchpad (`udm/old`), and `S4QL_ENGINE_SIM_SEEDS=200 go test ./internal/engine/ -run '^TestSimulation$/^default$' -v` ran there and here. Of 200 seeds of the default workload, the selects with WHERE that the checks compare with SQLite went from 1,408 to 1,007, and those that give rows from 167 to 84. A third of the selects run on an engine that reads every row, which has no bug to find. The updates and deletes take turns of the selects, 3,649 of them in the 200 seeds, and delete rows (a delete without WHERE, one in five, empties its table), so fewer selects meet a row.
- The hammer workload has one instance, opened once, so every select reads by the key, and it runs more statements. `openBound` moved there in `TestSimulation_plantedBugs` in `/internal/engine/simulation_test.go`, with the reason in the comment. 32 of 200 seeds find it, each by its own violation ", and in SQLite", against 8 in the default workload, and the default run finds it the second time at seed 14, where it did at seed 55.
- `S4QL_ENGINE_SIM_SEEDS=200 go test ./internal/engine/ -run '^TestSimulation_plantedBugs$' -v`, in 303 s: the other fourteen counts are what the generated checks of slice 8 gave, and every seed that finds a bug finds nothing without it. `/internal/engine/README.md` has the new row, the workload, and the reason.

### Why

Moving the bug to the workload that reaches it is what the round of slices 5 and 6 did for `convertReal`, and keeps what counts as found. A change of `stmttest.Sequence` to write more selects by the key would have moved every pinned count of the differential tests and of `drivercheck`, for one planted bug.

### What worked

- The counts of the simulation's own tallies, "selects with WHERE ... that give rows", showed the cause without new code.

### What didn't work

Nothing.

### What I learned

- The hammer workload reads by the key only, since every second engine of a node reads every row and its one node opens once.

### What was tricky

Nothing.

### What warrants review

- `openBound` in the hammer workload, beside `checkOutside`, `readNewest`, and `convertReal`: the default workload no longer runs it, so a fault that only two instances or a crash show would not be found by it there.

### Future work

None.

## Step 3: The runs, the survivors, and the tests that close them

**Author:** update-delete-mutation builder

### Prompt Context

**Verbatim prompt:** (from the brief) "Every mutant is killed by a test, or is listed in `equivalent.txt` with a reason a reader can check. No survivor stays unexplained. A survivor that shows a missing test: write the test in the package's style, compared with SQLite where the neighbours compare. A survivor that shows a real fault: fix the code, add the test, and put it in the first line of your report." And from the lead: "Verify with the tester that each of the two entries names the mutant its reason describes and that the reason is still true".
**Interpretation:** Classify each survivor, check each new test against its mutant before the rerun, and rerun the survivors with the tester on a copy of the tree with the new tests.
**Inferred intent:** Numbers in the README that a rerun on the final tests gives again, and reasons in `equivalent.txt` that hold.

### What I did

- The runs, each on an `rsync` copy of the worktree (`udm/copy.sh`, `udm/run.sh`), at a load of 15 to 30 on 10 cores: `stmt` with 3 workers and `-first 'TestCheck$|TestRecognize|TestStatement'` (162 mutants, 12 min 42 s, 3 survivors); the engine with 4, `-factor 5`, and `-first` of `TestChangeRows`, `TestEngine_update`, `TestEngine_delete`, `TestNewPlan`, `TestCheckType`, `TestFilter`, `TestConn_netEffect`, and `TestEngine_errors` (455, 47 min, 54 survivors); the core with 2, `-factor 3`, and `-first` of six of its transaction tests (183 of 271 in 1 h 29 min, 18 survivors, then stopped, see below); the driver with 1 (12, 1 min, none).
- The two entries of `build.bind ... #4` that a merge had moved from `build.filter`: the tester ran both, and both survive. `#4` is the fourth `err != nil` of `build.bind`, the error of `keyAccess`, and the reason holds: `build.bind` calls `checkType` on every value before `keyAccess`, which gives no row for NULL before it compares, so `tighter` compares values of one kind; `build.bind` is the only caller of `keyAccess`, for a select and for an update and a delete. The entries stay as they are.
- `udm/mutant.py` checked each new test against its mutant through `-overlay` in seconds. It printed "KILLED" for a mutant that did not compile, which a `\&\&` in a shell argument made once; it says "UNCOMPILED" now.
- The new tests, each in the style of its neighbours:
  - `/internal/sql/stmt/recognize_internal_test.go`, `TestRecognize`: an update whose second column of SET is not at the next token, and one whose assignment beyond SQLite's limit is not, trees that the parser never gives, as the round of slices 5 and 6 did for the list of an insert.
  - `/internal/engine/engine_test.go`: `changeTrio.run` compares the offset of each error with SQLite's, as `pin` does, which pins the offset -1 of `noSuchSetColumn`; `TestEngine_events` has the events of an update and a delete.
  - `/internal/engine/operator_internal_test.go`: the fake fails a delete, an add of a key, and the read of the keys on demand (`deleteFails`, `addFails`, `readKeysFails`). `TestChangeRows` pins that the check of NOT NULL reads the source once for two columns, the errors of that read, of the get of a found row, of the delete of a delete and of a change of the key, and of the set of keys; the stop of `changeRows` at the next key once the context ends; the planted bugs `changeNothing` and `keepOldKey` through `buildSink`; the internal errors of `update.run` and `remove.run` for a found key whose row is gone, from a scan of another fake; and those of `build.change` for plans that no statement gives: a source that is no scan, a key of two columns or past the columns, a column set twice, a parameter without its argument, and an update that sets nothing.
  - `/internal/core/tx_test.go`, `TestTx_Keys`: a set refused after the end of the transaction; the error of f before that of a drop of the set that fails locally, with a fault of the temporary file under 4,000 keys of 900 bytes as `TestTx_tooBig` makes one; no error of its own for a set that f leaves where the transaction can only be rolled back; and `ErrClosed` for an instance that f closes, the one way `Tx.checkOpen` fails while the transaction has not failed.
- The engine's 54 survivors ran again on a copy with the new tests (`udm/tree3`, 3 workers, `-min-time 5m`): 45 killed by the hand-written tests, and the 9 others equivalent. The 3 of `stmt`: 2 killed, and `max` that is commutative, listed since slice 7.
- The core's run took 10 min for each survivor, which runs every test of the core under that load. I stopped it at 183 and ran the 88 left and the 11 survivors that `equivalent.txt` did not list on a copy with the new tests (`udm/tree4`, `udm/remaining.py`), 3 workers.
- The new section at the end of `/internal/sql/mutation/equivalent.txt` has 7 mutants of the engine and the ones of the core: `readOnly.delete`, whose sink is never run; the key that is never nullable in `update.check`, as in `insert.check`; the swapped arguments of a `compare` of which only the equality is read; the error of that `compare`, which `update.check` rules out; `changeNothing` and `keepOldKey` left out of `Conn.writeIn`, as `skipUnique` and `convertReal` are; the number and the name of the table of a set; and the finalize of statements at rest in `Tx.dropKeys`.

### Why

A test of an internal error builds what no statement gives, as `TestInsert` does for the insert; the alternative, deleting the checks, would leave a fault of the plan to panic later, far from its cause.

### What worked

- The fake of the engine's operators took three more failures in a few lines each, and every path of `changeRows` is pinned with its calls.
- `faultvfs` and the large keys of `TestTx_tooBig` made the drop of a set fail for real.

### What didn't work

- `git reset --hard` was refused earlier (step 1). A here-doc into the scratchpad was refused; the scripts went through the editor.
- My first try of an add that fails locally added one key after the fault, which SQLite still held in its page cache; the test adds until one fails, at most 4,000 more.

### What I learned

- `conn.rest` finalizes a statement whose reset fails, so a statement at rest always finalizes without an error, and the binding's `Finalize` of a finalized statement is a no-op.
- `DB.Close` during f closes the idle write connection, and `Tx.checkOpen` then fails with `ErrClosed` while `Tx.state` is still nil.

### What was tricky

- `Tx.dropKeys` gives nil for a transaction that can only be rolled back, so only an f that swallows the error of a call shows that path; the engine's `changeRows` always passes the error on.

### What warrants review

- The reasons of the new section of `equivalent.txt`, above all those of `Tx.dropKeys`.
- `TestTx_Keys`'s reliance on SQLite's page cache of temporary tables, as in `TestTx_tooBig`: it requires that a fault happened, so a larger page cache fails it rather than passing it silently.

### Future work

- The core's rerun, the README of the runner, and the checks.

## Step 4: The rest of the core, the driver after slice 9, the review, and the checks

**Author:** update-delete-mutation builder

### Prompt Context

**Verbatim prompt:** (from the brief) "Record in `docs/todo.md` what is found and not fixed; delete the item on slice 8's mutation round when done; numbers of the run in `internal/sql/mutation/README.md` as the earlier rounds did. [...] Before you open your pull request, merge `origin/main` again. [...] Finish: `make test`, `make lint`, `make check-browser` pass."
**Interpretation:** Finish the core's run, rerun what the merges of `main` touched in the scope, write the numbers, review, and run the checks.
**Inferred intent:** A branch that the lead can merge without a second look at the numbers.

### What I did

- The core's other 88 mutants and its 11 survivors that the list did not hold ran on `udm/tree4` in 40 min (3 workers): 35 of the 88 killed, 15 timed out, 25 not compiled, 13 survived; 8 of the 11 killed. Of the survivors, 6 in `KeySet.Add`, `KeySet.All`, and `KeySet.after` showed that no test called them after f had closed the instance; `TestTx_Keys` does now, and requires that the read gives its error once, since a loop that sees no error at all passes otherwise. A run of those 6 on `udm/tree6` killed them all. The others went into `equivalent.txt`: the statements of a set that `Tx.dropKeys` leaves at rest, the block of the error of `KeySet.Add`, which gives nil for nil, and the return after the error of `KeySet.All`, whose key is nil then. Over the runs, the core has 271 mutants: 62 not compiled, 159 killed, 33 timed out, 17 equivalent, 0 unexplained; 14 survivors of the earlier runs are killed by the new tests. `udm/final.py` combines the results by key, the later over the earlier.
- The first merge of `origin/main` brought #92, the pass of slice 9, which gave the driver's `check` its call of `engine.Check` back, for the refusal of an index. The driver ran again on that code (`udm/tree5`, 18 mutants): one survivor, the skip of `engine.Check`, which `TestPrepare` kills now with a create index and a drop index refused at the prepare. The other changes of #92 and #95 to functions of the scope are the refusal of an index in the engine's `check` and the length of the schema of a create table in `buildSink`, and are not in this run; the README of the runner says so.
- The second merge of `origin/main` brought #97, the indexes of the core, whose commit removed the section of `conn.scan` from `equivalent.txt`, since `conn.scan` only calls `conn.scanIndex` now. The conflict was beside my section; I took the removal. #97 changed no function of the scope in `tx.go`.
- The review (`/code-review medium 79de6cd^..HEAD`) found no fault. It reported that the results of the run were not written yet, which this step does, that the diary lacked the step of the survivors, which step 3 is, that the fault VFS of the new subtests of `TestTx_Keys` was closed only at their end (it is closed at the cleanup of the test now, as `sleepCounter` does), and that `TestEngineLayers` checked one way only (it also requires that each fuzz target of the layer exists now).
- `/internal/sql/mutation/README.md` has the section "The UPDATE and DELETE of slice 8", and `/docs/todo.md` drops the items of this round, and keeps the long fuzz runs of the pass of slice 8.
- `make test`, `make lint`, and `make check-browser` pass after the second merge; the new tests pass 50 times in a row, and those of the core and the engine under `-race` too.

### Why

A run of the remaining mutants on the new tests gives the same verdict as a full run on them, in a third of the time, since only the survivors run every stage.

### What worked

- Running the survivors that `equivalent.txt` already listed only once: each takes the whole time limit of every stage.

### What didn't work

- The core's first run took about 10 min for each survivor at a load of 20 to 30, so I stopped it at 183 of 271 rather than wait the rest on tests that had changed.
- A shell command with a here-doc of Python that wrote the README was refused as "too complex to verify that it stays inside the worktree"; the edits went through the editor.

### What I learned

- A test of a sequence that must give an error needs to count the values: a loop that never runs passes every check inside it.

### What was tricky

- Telling which numbers of the core belong to which tree: `udm/final.py` takes each mutant's last result.

### What warrants review

- The 33 timeouts of the core, which leave out the end of a call or loop over the set: a timeout counts as killed, as in the earlier rounds.

### Future work

- Long fuzz runs of the code of slice 8 in the pass, as `docs/todo.md` says.
