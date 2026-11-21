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

## Step 3: The survivors, the tests and the code that close them, a fault, the full disk, and the merge of `main`

**Author:** index-mutation builder

### Prompt Context

**Verbatim prompt:** (from the brief) "A survivor that shows a missing test: write the test in the package's style [...] A survivor that shows a real fault: fix the code, add the test, and put it in the first line of your report. The rules of `chooseAccess` deserve the most care [...]". And from the coordinator during the run: "Tonight the machine's disk filled to 0 bytes and stopped every agent [...] keep `ixm` under 5 GB [...] If any of your runs failed or gave odd results while the disk was full [...] treat those results as void and rerun them [...] `main` gained #111 [...] #112 [...] and #113; merge `origin/main` before your pull request."
**Interpretation:** Classify each survivor as it comes, write its test or its reason, restart a run on a new copy of the tree when its survivors are known to be killed by new tests, and rerun what the full disk and the merge touched.
**Inferred intent:** Numbers that a rerun on the final code gives again.

### What I did

- A real fault: `engine.Conn.queryIn` drew the direction of a query in a transaction and did not record it, so its event had `statement.access` and no `statement.direction`, which the README of the engine promises for every query. The survivor `Conn.queryIn boolean: true => false` showed it. `r.drew(how)` is there now, and `TestEngine_report` requires the directions of the queries of a write and of a read-only transaction, and no draw for the update and the delete between them, which also kills `Conn.writeIn boolean: false => true #3`.
- `chooseAccess`: four survivors were the check of rule 4, `len(best.equal) == 0 && !best.ranged()`, which is the primary key's fit itself (an index wins only by more pins or a range, so a winner without either is the primary key, whose access pins and bounds nothing, the zero access); the check is gone, and the doc of rule 4 says why. Two were the parameter `byNull` of `pinOf`, which `pointOf` passed false: `never` gives no row for IS NULL of a column of the primary key first, and no such column takes NULL; the parameter is gone. No survivor of `chooseAccess` was a rule that the tests did not pin, and none needed an excuse.
- Code that nothing needs, removed: the bounds of `core.conn.scan`, which both callers left empty; the refusal of an index without columns in `core.conn.loadIndexes`, which SQLite never gives (the merge of #112 brought it back as `foreign(...)`, and it is gone again).
- New tests: `TestForcedAccess` (an internal error of `fit`); `TestEngine_reading` (the directions of a seed against splitmix64 from Vigna's code); `TestFilter` (a failed read of the indexes for a select, an update, and a delete, and a path of an index that the table does not have, which reads as the rules choose); `TestBuild` (failed lookups of a table and of an index for a create table, a create index, and a drop index, and the internal error of a create index that names a column twice); `TestNewPlan` (the plans of a create index and a drop index); `TestFromCore` (`ErrIndexExists` and `ErrNoIndex`); `TestCheck` and `TestRecognize` of `stmt` (a schema before a reserved name of an index, and the guards `isNext` of `createIndex`, `indexColumn`, and `notIf`); `TestOpen_cache_indexes` of the core (an index of a reserved name and an index on a table of a reserved name that another process adds). The fake of the engine fails the read of the indexes, the lookup of an index, and the lookup of a table of one name. `lookUpIndex` reads the owner of an index with `limit 1`, so that its query fails only before its row.
- The equivalent ones are in a section of `/internal/sql/mutation/equivalent.txt` of their own: the variants of the planted bugs `openBound` and `shortKey`, the direction that a sink of a read-only transaction and the planted bug `checkOutside` draw and never read, `max` and the sum of `indexTooBig`, which do not care about order, the errors of `fit` that `checkType` rules out, the order of `pragma_index_list` in `loadIndexes`, `pragma_index_xinfo`'s positions, the errors that a caller reads before the bool of `conn.insert` and `conn.delete`, and the version of the cache.
- Runs: I stopped and restarted the engine's run twice and the core's three times on new copies of the tree, each with the mutants that had no result and the survivors that the list did not hold (`ixm/left.py`, `ixm/plan.py`), so that a survivor that a new test kills does not cost its ten minutes again.
- The full disk: the run `r3-core` stopped with `no space left on device`, and 10 of its 25 results held I/O errors; all 25 are void and ran again. No other run's results hold an error of the disk. The build cache of the runs is trimmed every minute to stay under 1.5 GB (`find -mmin +3 -delete`), since a mutant of the core leaves about 14 MB in it.
- The merge of `origin/main` (#107's follow-ups, #110, #111, #112, #113, and the engine of slice 10): conflicts in the pins of `TestDifferential` and the driver check, the README of the engine, `docs/todo.md`, `plan_test.go`, `index.go`, and `index_test.go`. I took `main`'s side and recounted. The bound of the key draws `IntN(6) == 4` now, which leaves the share of the key equal to a value of an insert as `main` draws it; of the shares I tried (2, 3, 4, 5 of six), only 4 loses no required kind in either test, and 5 lost "path of an index with 3 pinned". The table with a rowid in `TestOpen_cache_indexes` is gone, since the strict load of #112 refuses it at its first read. After the merge, the mutants of the selection in functions that the merge changed ran again, with those without a result.
- The recount of the planted bugs after the merge failed twice: seeds 158 and 78 found `keepFailed` and `keepOldKey` by "is not in the log". Without the bugs they find nothing. The traces show why: under `keepFailed` an update that changes the key of a row that its transaction inserted fails with UNIQUE after the delete of the old key, which stays, so the net effect of the transaction is nothing and its acknowledged insert is in no commit object; under `keepOldKey` a later delete takes the row that the bug left, and the net effect of an acknowledged update and that delete is nothing. Both bugs count that violation now, with the reason in the comment.

### Why

A restart costs the planning of a run, 4 to 9 minutes under this load, and saves the ten minutes of each survivor that a new test kills.

### What worked

- `ixm/mutant.py` checked every new test against its mutant in seconds before a rerun.

### What didn't work

- The disk filled while three runs and a recount ran; the build cache of the runs was 18 GB.
- `python3 -` with nothing on stdin hung a command, which I stopped.
- Shell commands that chained `git` with other commands were refused as too complex to verify; I ran `git` alone.

### What I learned

- A mutant of the core costs about 14 MB of build cache, so a run of a thousand of them needs the cache trimmed while it runs.
- Under a load of 25 to 32, a survivor of the core costs 10 to 15 minutes, since it runs every test of the core and of the engine.

### What was tricky

- Telling the survivors that the tests of a later copy kill from those that are equivalent, across five runs of the engine and four of the core; `plan.py` takes each mutant's last result.

### What warrants review

- The reasons of the new section of `equivalent.txt`, above all those of `loadIndexes`, which rest on what `pragma_index_list` and `pragma_index_xinfo` give.
- The two violations that `keepFailed` and `keepOldKey` count now.

### Future work

- The core's last run, the numbers in the README of the runner, and the checks.

## Step 4: Two more merges of `main`, the lost scratch directory, the last runs, and the checks

**Author:** index-mutation builder

### Prompt Context

**Verbatim prompt:** (from the coordinator during the run) "For your merge of `origin/main`: `docs/todo.md` was split (pull request #115). [...] take `main`'s index for `docs/todo.md`, and move what you added or changed into `docs/todo/slice-09.md` [...] Also on `main` since you last merged: slice 10 (#110) and slice 11 (#114) through the engine with their generated checks (many pins moved [...]), so recount your planted bugs and tallies after the merge." And from the brief: "Finish: `make test`, `make lint`, `make check-browser`, `make fuzz-shards` pass."
**Interpretation:** Merge again, keep the reach of the two tallies without moving `main`'s pins where that can be helped, rerun what the merges changed, and record the results.
**Inferred intent:** A branch that merges into `main` as it is now, with numbers of its code.

### What I did

- The scratchpad of the session was emptied twice by a cleanup outside the round (the whole directory, not only `ixm`), which stopped the runs of the core and the engine each time; I told the coordinator and moved the runs into an untracked `.ixm/` of the worktree, with backups of the results every minute. The results that survived in the backup (319 of the core, 524 of the engine) count; the rest ran again (`r7`).
- Merged `origin/main` twice more: with the split of the todo, slices 10 to 13, #111, #112, and #113. `docs/todo.md` is `main`'s; the decisions of this round are in a section of `docs/todo/slice-09.md`, its item on the mutation round is gone, and the open failures of the core are in `docs/todo/storage-core-tests.md`.
- The bound of the key in `Sequence.changeConditions` drew its value before the spelling of the column, where `main` draws the spelling first, so every sequence drew in another order than `main` and every merge moved `main`'s pins and some rare least numbers. It draws in `main`'s order now, and the operator comes from the place of the value: for every third of the values that the sequence keeps, the key is bounded on one side of it, with `<`, `<=`, `>`, and `>=` in turn. `TestDifferential` passes with `main`'s pins and least numbers and reaches the update to an upper bound 11 times; the driver check reaches the delete to an upper bound twice, and only its least number of "update WHERE that reads a get of one key ok, rows changed" is a third of the new count, 2.
- The lines that the merges wrote are left out of the selection (`ixm/select.py` takes the mutants on lines of #92, #97, #105, #108, and this round's own commits, by `git blame`), and the mutants of the functions that a merge changed ran again (`ixm/changed.py`).
- New tests from the last runs: `TestChooseAccess` (an index that pins more over a later one that pins less and bounds), `TestEngine_report` (the access of every statement of a transaction), `TestReadTx_Close` and a subtest beside it (`Indexes`, `Index`, `ScanIndex`, and `ScanPlan` after the end, and the refusals of `ScanPlan`), `TestTx_CreateIndex` (the length and the limit of an index too long for the cache, and the whole refusal of an index without a name), `TestScanIndex` (an upper bound of another storage class beside a valid lower one), `TestReplay_indexes` (an index of 250 columns), and `TestTx_readFails` (a read that fails under a create index, a drop index, and a scan by an index).
- The planted bugs over 200 seeds after the last merge: every bug is found, none without it; the table in the README of the engine has the counts. `openBound` is found by 11 seeds of the hammer workload, where it was 32 before slice 9 and 24 after the first changes of this round. The core's two bugs of slice 9 were found by 33 and 29 of 200 seeds, as #108 measured.
- `make test`, `make lint`, `make check-browser`, and `make fuzz-shards FUZZGROUP=c FUZZSHARDS=4` and `=6` pass; the new tests pass 10 times under `-race` and 20 times without.
- The self-review with the code-review skill on #120 found three small things, all fixed: `Sequence.Mistyped` took the column of a value from the order of the shape where the list had renamed one, so it could give a real to a column of text (it takes the column that the list names now, as its test does); the least numbers of the two kinds that the driver check requires again were 2, near what the run reaches, and are 1 now, a third or 1 as the rest; and the todo said that `openBound` was found as often with `Mistyped` as without, where the numbers are 62 and 54. The planted bugs over 200 seeds ran again after the fix of `Mistyped`: `convertReal` 60, `openBound` 11, `checkOutside` 35, and none without its bug.

### Why

Drawing the bound from numbers that `main` draws anyway keeps the generated checks of the other slices as they are, which every later merge then keeps too.

### What worked

- The comparison of the run with the bound switched off against `main`'s own run, which showed that the order of the draws, not the bound, moved the pins.

### What didn't work

- Each merge of `main` before that moved a few rare least numbers below theirs; four shares of the bound were tried before the order of the draws was found.
- A first try at a test of the commit of a create index under a fault of a read failed the commit elsewhere, so 18 mutants of failures around indexes in the core stay open, with the test to write in `docs/todo/storage-core-tests.md`.
- Nine equivalent mutants of `conn.loadIndexes` and `conn.lookUpIndex` timed out in the run after #112 under a load of about 30; the tool counts them with the killed ones, and their entries stay.

### What I learned

- The order of evaluation in a composite literal is the order of its random draws: moving one call out of a literal changes every number after it.
- A scratch directory outside the worktree can vanish under a long run; the results belong where the run's owner controls them.

### What was tricky

- Keeping four packages' results, from nine runs on six copies of the tree, apart from the mutants that the merges removed or moved; `ixm/tally.py` takes each mutant's last result in the current selection.

### What warrants review

- The 18 open mutants of the core and the test that would close them.
- That the scope leaves out the lines of #107, #111, #112, and slices 10 to 13 in the functions of slice 9.

### Future work

- The test of the failures of the core around indexes, in `docs/todo/storage-core-tests.md`.
- Long fuzz runs of the code of slice 9 in the pass, in `docs/todo/slice-09.md`.
