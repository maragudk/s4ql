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
