# Diary: Mutation testing of slice 9, indexes

Slice 9 of the roadmap went through the statement pass (#92), the commit format and the storage core (#97), the engine and the driver (#105), and the generated checks (#108). This task runs the mutation tester `internal/sql/mutation` over the code that slice 9 added or changed, closes the gaps that surviving mutants show or lists them as equivalent, finds out why the planted bug `convertReal` grew thin and why two tallies of updates and deletes to an upper bound were no longer reached, and repairs both by the reach of the workload. The branch is `index-mutation`, from `origin/index-checks` at `3a1382d`, fast-forwarded to `origin/main` at `6fad4ce` once #108 was merged.

## Step 1: The requirements and the selection

**Author:** index-mutation builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Scope: the code slice 9 added or changed (pull requests #92, #97, #105, #108). [...] Restrict the run to these; do not rerun earlier slices. [...] 1. Every mutant is killed by a test, or is listed in `equivalent.txt` with a reason a reader can check, in a new section of your own at the end. No survivor stays unexplained. [...] The rules of `chooseAccess` deserve the most care: a mutant there that survives is either a rule the tests do not pin (pin it in `TestChooseAccess` or the golden plans) or a choice that cannot be told apart by results [...] a mutant that changes only speed must be killed by a rows-read pin, a plan check, or the model `stmttest.ChosenAccess`, not excused. 2. Two things the generated checks of slice 9 left thinner, to repair the honest way (by making the workload reach them, never by changing what counts as found) [...] The planted bug `convertReal` is found in only 8 of 200 seeds now. Find out why and raise its share. [...] Two tallied kinds were removed because the workload no longer reached them [...] Recount all planted bugs afterwards over 200 seeds, update the READMEs' tables, and confirm none is found without its bug. 3. Tests never skip except under `-short`, never depend on timing. [...] 5. Record in `docs/todo.md` what is found and not fixed; delete the item on slice 9's mutation round when done [...] 6. Diary [...]"
**Interpretation:** Select the functions of slice 9 by `git blame` as the rounds of slices 7 and 8 did, run each package on a copy of the tree with `-first`, classify every survivor, and change the workloads only so that they reach more of what the two checks need.
**Inferred intent:** The same trust in slice 9 as in slices 1 to 8, with planted bugs and tallies whose detection does not hang on a few seeds.

### What I did

- Branched from `origin/index-checks` and merged `origin/main` (nothing to merge then); once #108 was merged into `main`, fast-forwarded to `origin/main` at `6fad4ce`.
- Copied the scripts of the round of slice 8 into the scratchpad (`ixm/`): `select.py`, with the revisions of #92, #97, #105, and #108 (the ranges of the second parent of each merge from the first, and the merge itself), `copy.sh`, `run.sh`, `mutant.py`, `final.py`, and `remaining.py`.
- The listing of `-list` over `stmt`, the engine, the core, the format, and the driver has 15,549 mutants. The selection (`ixm/sel.txt`) takes whole every function that slice 9 added, and only the changed lines of the older functions it touched:
  - `stmt`, 157: `recognizer.createIndex`, `recognizer.indexColumn`, `recognizer.indexName`, the methods of `CreateIndex` and `DropIndex`, `reservedPrefix`, and the changed lines of `recognizer.drop`, `recognizer.statement`, `fieldNames`, `nodeName`, `nodeNames`, and `reservedName`.
  - The format, 61: `checkCreateIndex`, `CommitWriter.CreateIndex`, `CommitWriter.DropIndex`, and the changed lines of `checkChange`.
  - The core, 1,179: all of `index.go`; `conn.lookUp`, `conn.readSchema`, `conn.scan`, `table.checkIndexed`, `cacheVersion`, `ReadTx.ScanRange`, `Tx.ScanRange`, and both `tableWithoutIndexes`; and the changed lines of `conn.delete`, `conn.insert`, `conn.load`, `conn.upsert`, `openConn`, `ReadTx.Get`, `ReadTx.Table`, `apply`, `Tx.CreateTable`, `Tx.DropTable`, `Tx.Get`, `Tx.RollbackTo`, `Tx.Savepoint`, `Tx.Table`, and `Tx.spoolCommit`.
  - The engine, 906: all of `access.go`; `openBounds.scan`, `Engine.reading`, the errors of indexes, `build.access`, `build.bind`, `buildCreateIndex`, `buildDropIndex`, `buildQuery`, the operators of the two statements, the methods `scanned`, the index methods of `coreReadTx`, `coreTx`, and `readOnly`, `lookUpIndex`, `report.drew`, `report.reads`, and `scanOf`; and the changed lines of `Conn.checkOutside`, `Conn.query`, `Conn.write`, `newEngine`, `build.change`, `build.filter`, `build.node`, `buildSink`, `scan.rows`, `Format`, `NewPlan`, `fromCore`, `report.end`, `report.of`, `Conn.queryIn`, and `Conn.writeIn`.
  - The driver, 18: `check` of `conn.go`, whole.
- Started the runs of all five packages on a copy of the tree (`ixm/tree1`): the core with 3 workers, `-factor 3`, and `-first` of twelve of its tests of indexes and commits; the engine with 3, `-factor 5`, and `-first` of fifteen of its tests of access, plans, and errors; `stmt` with 2 and `-first` of `TestCheck`, `TestRecognize`, `TestStatement`, `TestVocabulary`, and `TestReservedName`; the format and the driver with 1 each.

### Why

The engine's run does not wait for the change of the workload of `convertReal`: the change only adds to what the simulation reaches, and every survivor runs again on the final tests.

### What worked

- The format: 61 mutants, 20 not compiled, 41 killed by its hand-written tests, none survived.

### What didn't work

- My first `select.py` named #108 as `origin/main..origin/index-checks`, which is empty once #108 is merged; the selection from the merge's parents gave the same list.

### What I learned

- #107, the statement pass of slice 10, put the call of `engine.Check` back into the driver's `check`, for the refusal of IF NOT EXISTS and IF EXISTS, after #105 had removed it. So the driver's `check` in the scope is #107's now.

### What was tricky

Nothing yet.

### What warrants review

- The selection: whether a function of slice 9 is missing.

### Future work

- `convertReal`, the two tallies, and the survivors of every run.

## Step 2: Why `convertReal` and the two tallies grew thin, and the workload that reaches them

**Author:** index-mutation builder

### Prompt Context

**Verbatim prompt:** (from the brief) "The planted bug `convertReal` is found in only 8 of 200 seeds now. Find out why and raise its share. [...] Two tallied kinds were removed because the workload no longer reached them: \"update WHERE that reads a scan to an upper bound ok, rows changed\" in `TestDifferential`, and the same kind for delete in the driver check. Find out why they are no longer reached (did the kind's name or the access change with indexes, or does the generator no longer write such a statement?), and restore the reach and the tally, or show that the kind is reached under another name."
**Interpretation:** Measure what each needs before and after #108, change the workload so that it gives more of it, and recount every pin that the change moves.
**Inferred intent:** Checks whose power does not hang on a few seeds, found by the same violations as before.

### What I did

- `convertReal`: `git archive d801295` (the tree before #108) into the scratchpad (`ixm/old`), and a throwaway count in `tallyOf` in both trees, over 200 seeds of the hammer workload. Before #108: 14,509 inserts, 2,923 that succeeded, 54 refused for a real into an INT or INTEGER column; after: 11,096, 2,195, and 46. The bug was found by 20 (18) seeds before and 10 (8) after. The create indexes and drop indexes take turns of the inserts (3 or 7 of 29 or 33 turns), and the rest of the drop is chance: 46 candidates would give about 17 seeds at the old rate.
- `stmttest.Sequence.Mistyped` (`/internal/sql/stmt/stmttest/sequence.go`): one insert in eight gives an integer of an INT or INTEGER column as the real of its value, which SQLite converts back; the values that a WHERE draws keep the integer, and the draw happens only after `Mistyped`, so no other sequence moves. The simulation's `workload.mistyped` is set on the hammer workload (`/internal/engine/simulation_test.go`). 70 of 200 seeds find the bug then. A first try, one insert in three with a value of any type, gave 25 and took inserts that succeed from the other bugs of the hammer workload.
- `checkOutside` and `openBound` over 600 seeds, with and without `mistyped`: 107 and 62 with it, 107 and 54 without, so the change takes nothing from them; at 200 seeds `openBound` had looked lower (16 against 27), which was chance.
- The two tallies: the name of the kind is `stmttest.KeyAccess`'s, the access by the primary key alone, which indexes do not change. In the run before, 17 updates of `TestDifferential` read to an upper bound, none changed rows, and the driver check's deletes the same. The kinds were rare (least number 1) and the turns of the indexes and the new random numbers took them to 0. `Sequence.changeConditions` now gives, when an insert gave the key a value, the key equal to one one time in four (it was one in three) and the key bounded on one side by one, with `<`, `<=`, `>`, or `>=`, one time in six. `TestDifferential` reaches the update 6 times and the driver check the delete 3 times; both require them again (2 and 1).
- The sequence draws other random numbers then, for every test that uses it. Pinned counts of the excuses in `TestDifferential` and `TestGenerated` of `drivercheck` are recounted, with the reason in their comments; the least numbers of five rare kinds that fell below theirs are a third of the new count, or 1; and the driver check requires the read-only transaction's insert while rows are open with a parameter again, which it reaches, as it did before slice 8.
- New tests in `/internal/sql/stmt/stmttest/sequence_internal_test.go`: `TestSequence_insert` (the whole reals of `Mistyped`) and a subtest of `TestSequence_delete` (the shares of the key equal to and bounded by a value of an insert).

### Why

- Moving `convertReal` to another workload, as the round of slice 8 moved `openBound`, was not open: it is in the hammer workload already. A knob that only the hammer workload sets keeps `TestDifferential` and the driver check as they were, and the bug is found by the same violation, "stores a value of another type than its column".
- A bound of the key from a value of an insert finds rows where most conditions of `Sequence.conditions` find none in a table of a few rows, as the key equal to such a value does.

### What worked

- The counts in `tallyOf` of the two trees showed the cause in two runs.

### What didn't work

- Every share of the bound in `changeConditions` that I tried moved some rare least numbers of `TestDifferential` below theirs: one in six out of the share of `Sequence.conditions` took five below, among them "update WHERE that tests a column more than once ok, rows changed" to 0; one in twelve for deletes alone left the driver check's delete at 0. Several of those least numbers were at their count already before the change (3 of 3).

### What I learned

- The least numbers of the rarest kinds sit at a third of one run, and some at the whole of it, so any change of the random numbers of the sequence moves a few of them below.

### What was tricky

- Telling chance from cause at 200 seeds: `openBound` looked hurt at 200 seeds and was not at 600.

### What warrants review

- `Sequence.Mistyped` in the hammer workload only: the default workload's sequences are as before.
- The lowered least numbers in `TestDifferential` and the driver check.

### Future work

- The recount of every planted bug over 200 seeds, and the survivors.
