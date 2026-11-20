# Diary: Slice 10 of the roadmap through the engine and the driver, `if not exists` and `if exists`

Slice 10 adds `create table if not exists t (...)`, `drop table if exists t`, `create index if not exists i on t (...)`, and `drop index if exists i`. The statement pass reads them since pull request #107 (diary `2026-11-20-if-exists-stmt.md`), and the engine refuses them with the error that the pass gave before, through `existsRefusal`. This task runs them in the engine and the driver, keeps a statement that does nothing out of the log, removes what the pass left as temporary, and turns the clauses on in the generated checks, with a planted bug in the simulation. The branch is `if-exists-engine`, from `origin/index-checks` (pull request #108) with `origin/main` merged in.

## Step 1: The requirements

**Author:** if-exists-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Task: slice 10 through the engine and the driver, with its generated checks [...] 1. Statement by statement, s4ql does what SQLite does: the same schema afterwards, the same `RowsAffected` [...] the same error and the same order among errors, for every case the statement pass measured [...] 2. A statement that does nothing writes nothing: no commit object, no annotation in the log, nothing in the cache [...] Test this through the log and through replay on a second instance. 3. The known difference stays [...] But what the pass supports and only the engine can refuse [...] must follow SQLite when the object exists [...] 4. Remove what the statement pass left as temporary [...] 5. Generated checks: turn on `Generator.Exists` in `stmttest.Sequence` with rules in `stmttest/rule.go` for the four forms [...] with tallies that count every kind of outcome [...] One planted bug in the simulation [...] Extend an existing fuzz target's alphabet to the four forms [...] 6. Driver [...] 7. [...] Telemetry: the statement's event says whether the statement did its work or nothing [...] 8. CI's time is watched [...]"
**Interpretation:** Give the plans of the four statements their clause, let the sink of a statement that finds its object there (or not there) be one that does nothing, and keep such a statement out of the annotation and the commit in and outside transactions; then the generated checks, the simulation, a planted bug, a fuzz alphabet, and the documents.
**Inferred intent:** glue's migration tool can run `create table if not exists` against s4ql, with the same trust in correctness as the slices before.

### What I did

Read the roadmap's slice 10, the diary of the statement pass and its groups in `docs/todo.md`, the diaries of the engine and the checks of slice 9, `internal/engine/engine.go`, `tx.go`, `operator.go` (`buildSink`, `buildCreateIndex`, `buildDropIndex`), `telemetry.go`, `exists.go`, the core's `Tx.Commit` (a transaction without changes writes no commit object, also with annotations), `stmttest.Sequence` and `rule.go`, `reachIndex` and `pair.step` of the differential tests, and the simulation's `run.statement`, `run.verify`, `run.verifyTxs`, and `TestSimulation_plantedBugs`.

### Why

The pattern of the engine step of slice 9 and of its generated checks is the pattern here.

### What worked

Nothing to run yet.

### What didn't work

Nothing yet.

### What I learned

- The core writes no commit object for a transaction without changes even when it holds annotations, so a statement of its own that does nothing and annotated anyway would still leave the log as it was. Only an annotation in a transaction that changed something else would show.
- The simulation takes a write of its own that succeeded as acknowledged, and requires it in the log unless it is marked `unchanged`; a statement that does nothing needs the same mark.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

- The engine, the driver, the hand tests, and the removal of `existsRefusal` and the wrappers: step 2.
- The generated checks, the simulation and its planted bug, the fuzz alphabet, and the documents: the steps after it.
