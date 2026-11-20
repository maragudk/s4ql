# Diary: Slice 8 of the roadmap in the generated checks, `update` and `delete`

The engine and the driver run the `update` and `delete` of slice 8 since the branch `update-delete-engine` (pull request #91, in review), with hand-written comparisons with SQLite, both ways of reading rows. `stmttest.Sequence`, which the engine's differential tests, the simulation, and `drivercheck`'s generated comparison with mattn's driver draw from, wrote no update or delete beyond the one text of `stmttest.Others`. This task makes it write both, checks every one with a WHERE both ways, counts what the workload reaches, plants a bug of the new code in the simulation, and adds a fuzz target. The branch is `update-delete-checks`, from `origin/update-delete-engine` at `655bca7`.

## Step 1: The requirements

**Author:** update-delete-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Turn on `Generator.Changes` or the hooks `Generator.Update`/`Generator.Delete` in `stmttest.Sequence`, so that the generated sequences of the differential tests, the simulation, and the driver check contain updates and deletes, valid and invalid, with and without `where`, with literals, `null`, and arguments, with key changes that succeed and that collide, in and outside transactions, with rollbacks. [...] Every update and delete with a `where` must also be checked both ways [...] Choose the device [...] and give the reason. [...] `checkLength` and `longCondition` [...] cover the `set` literals [...] `reach` and the tallies [...] fail if a kind is never reached. [...] drivercheck compares `RowsAffected` through the pool only for inserts: do it for updates and deletes, and read the tables back afterwards. [...] at least one planted bug in the new code [...] A fuzz target for the new statements through the engine and through core [...] If a generated check finds a real difference from SQLite or a lost or wrong write, that is the point of this step: fix it if the fix is small and clearly right [...] Never weaken a comparison to make a test pass."
**Interpretation:** Hooks `Sequence.update` and `Sequence.delete` that draw from what the inserts wrote; a second engine in the pair of the engine's tests that reads the other way and runs every call; the access of an update and a delete checked against the rule; the length rules for SET; counted cases of slice 8 with least numbers in each test; `RowsAffected` and the tables in `drivercheck`; a planted bug of a change of the key; `FuzzChanges` and `FuzzChanges_core`.
**Inferred intent:** The generated checks, which found most bugs of the earlier slices, cover slice 8 with the same trust before mutation testing of its code.

### What I did

Read the roadmap's slice 8, the diaries of the engine step of slice 8 and of the generated checks of slice 7, and the decisions in `docs/todo.md`.

### Why

The pattern of slice 7 is the pattern here.

### What worked

Nothing to run yet.

### What didn't work

Nothing yet.

### What I learned

`Sequence` does not set the pools of `UpdateTable`, `UpdateColumn`, and `DeleteTable`, so a hook must give the names, or the pools must be set.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

- The sequence's hooks, the checks, the planted bug, and the fuzz targets: the next steps.

## Step 2: Updates and deletes in the sequence, a second engine, the counts, `drivercheck`, and the fuzz targets

**Author:** update-delete-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "`pair.bothWays` reruns only selects by the other access path. Every update and delete with a `where` must also be checked both ways (the path the engine picks, and the forced full scan), with the same rows afterwards and the same count. Choose the device (a second engine, or `engine.AccessOf` extended to update and delete) and give the reason. [...] drivercheck compares `RowsAffected` through the pool only for inserts: do it for updates and deletes, and read the tables back afterwards. [...] A fuzz target for the new statements through the engine and through core"
**Interpretation:** Write the hooks, run every call on a second engine, extend `AccessOf` too, pin what moves, and add the fuzz targets.
**Inferred intent:** Every generated update and delete is judged by SQLite, by mattn, and by the other way of reading rows.

### What I did

- `/internal/sql/stmt/stmttest/sequence.go`: `Sequence.update`, `Sequence.delete`, `Sequence.changeConditions`, `Sequence.conditions` (the body of `where`), the pools of `UpdateTable`, `UpdateColumn`, `DeleteTable`, and the kinds of `next` (3 and 2 in 24). Tests `TestSequence_update` and `TestSequence_delete`.
- `/internal/sql/stmt/stmttest/rule.go`: `SelectOf`, `SetCases`, `RowQueries`, and `whereText`/`boundValue` out of `FoundQuery`.
- `/internal/engine/differential_test.go`: `pair.other` and the new `pair.bothWays`; `pair.checkLength` for SET and WHERE of a change (`excusedLongChange`), `pair.longChange`, `changeTerms`; `reachChange`; least numbers of slice 8 in `reachedDifferential`; `FuzzChanges`, `FuzzChanges_core`, `changesOf`, and `fuzzBytes`, which `whereOf` uses now. `/internal/engine/tx_test.go`: the second engine's begin and end, and least numbers. `/internal/engine/export_test.go`: `AccessOf` of an update and a delete through `buildSink` on `tableOf`, which is a `writer` now.
- `drivercheck_test.go`: the count through the pool, `pair.compareChanged`, `pair.checkQuoted` for an update and a delete, `pair.reachChange`, least numbers, pins, and 250 sequences.

### Why

A second engine, because an update or a delete cannot run twice on one engine, and the device must see the rows afterwards; `AccessOf` too, because the comparison of rows cannot see a plan that reads more rows than the rule says. The second engine runs every call, so it holds what the first holds, and it replaced the second run of a select.

### What worked

- No difference from SQLite, from mattn, or between the two engines, at the first run with updates and deletes, nor after.
- `FuzzChanges` 223,248 and `FuzzChanges_core` 9,089 executions in a minute each, nothing found.

### What didn't work

- The first run of `TestDifferential` failed on a long literal of SET: `the engine gives 18/18: string or blob too big ("string or blob too big"), and the rule "s4ql: not supported: a string literal for the column PRECediNg.wiTH of type INT"`. That is the agreed order (the length before the type); `pair.checkLength` holds the rule for a change now.
- `drivercheck` failed on a delete whose WHERE has `"""" != -0000.00`: s4ql gives `no such column: """ - should this be a string literal in single-quotes?` and mattn deletes a row, reading `""""` as a string. `pair.checkQuoted` covers an update and a delete now.
- A first `TestSimulation` failed at seed 2 with `the acknowledged statement 59, ... insert ..., is not in the log`: a write transaction that inserted a row and deleted it commits nothing, which the old rule of `verifyTxs` (only tables made and dropped) did not know. A transaction whose rows SQLite has at its end as at its beginning may leave no commit object now.
- My key of the tally of commits and roll backs lacked the space after `reached:`, so `TestConn_differential` counted 0.
- Python and sed one-liners with several edits, and a heredoc appended to this diary, were refused as too complex to verify; separate commands and the Edit tool worked.

### What I learned

- Most WHERE of a select find no row of a table of a few rows, so a change would mostly find nothing; one WHERE in three of a change is the key equal to a value that an insert gave it.
- With fewer inserts and selects in the sequence, a few least numbers of slices 4 to 7 fell under a third, and two outcomes of `drivercheck` were no longer reached in 200 sequences.

### What was tricky

- The second engine in transactions: `txDifferential` and `FuzzChanges` begin and end it beside the first, and the table read back after a change runs through the engine's connection, so it sees the transaction's own rows.

### What warrants review

- `Sequence.update` and `Sequence.changeConditions`; `pair.bothWays`; `reachChange` and `SetCases`; the pinned numbers in `TestDifferential` and `TestGenerated`.

### Future work

- The additions of the review of pull request #91: step 3.

## Step 3: The additions of the review of pull request #91, and the planted bugs

**Author:** update-delete-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's message) "1. `attempt.unchanged` in `internal/engine/simulation_test.go` excuses on the engine's own word. [...] Fix it by deciding with SQLite [...] show that the planted `return 0, nil` is found in a large share of seeds, and keep it or a bug like it as one of your planted bugs. 2. A known difference to record and excuse precisely, not broadly: a multi-row update where a later row goes over the length limit. [...] 3. A test gap, behaviour correct: no test cancels an update or a delete midway. Add one [...] a context that flips after n `Err()` calls"
**Interpretation:** SQLite decides whether an unchanged write changed nothing; an excuse of exactly the shape of the record that SQLite does not measure; a cancel at each point of a change.
**Inferred intent:** No check that takes the engine's word, and no check that fails by platform.

### What I did

- `/internal/engine/simulation_test.go`: `attempt.changes`; an update or a delete of its own that changed no row goes into the statements that `run.verify` runs in SQLite on the state that they saw, in a savepoint that is rolled back, with no error and a count of 0 required, and the check of the acknowledged writes before it; the count of each update and delete compared with SQLite's, of its own at its commit and in a transaction in `verifyTxs`; tallies and least numbers of the updates and deletes, a quarter of the first 1,000 seeds; the planted bugs `changeNothing` (the review's `return 0, nil` at the top of `changeRows`) and `keepOldKey` (an update of the key that leaves the old row), in `/internal/engine/bugs.go` and `/internal/engine/operator.go`, through a field `bugs` of `build`, `update`, and `remove`, and given in `writeIn` too.
- `pair.recordSlips` and `excusedRecordSlip` in `differential_test.go`: an update that the engine refuses for a record runs in SQLite in a savepoint; if SQLite succeeds or gives UNIQUE, it is excused only when it finds two rows or more and an update of one of them alone gets `string or blob too big` in SQLite (`stmttest.RowQueries`); SQLite's count is set to 0 after it. Not pinned, since its count depends on the platform. `TestPair_recordSlips` pins the reviewer's shape, the same with a key that exists, one row, and two rows within the limit. The known difference is in `docs/todo.md`, and the order of the errors of an update there says that a later row is the exception.
- `TestEngine_cancelChange` in `engine_test.go`: `flipContext`, whose `Err` gives `context.Canceled` after n calls; for n from 0 until the statement succeeds, five updates and deletes over 40 rows, of their own and in a write transaction that goes on, inserts, and commits; the rows unchanged after each cancel, the end state, and a replay into an empty cache equal to SQLite.

### Why

The count of 0 was the engine's own word, so a bug that changes nothing and says so passed. The record check of SQLite depends on its allocator, so the excuse measures each row alone, where SQLite's check is reliable.

### What worked

- `changeNothing`: 156 of 200 seeds of the default workload, 108 by its own violation, `is in no commit, and changes`. With the new check of the unchanged writes switched off (temporarily), 117 of 200, by the counts and the later statements. `keepOldKey`: 29 of 200, 15 by the replay of the log.
- `TestEngine_cancelChange` passed at once, also with `-race`: a canceled change leaves nothing, and the transaction goes on.

### What didn't work

- `TestPair_recordSlips` first used an insert of two rows, which slice 1 does not support, and then compared the states in the case where the engine changed rows and SQLite did not run.
- `TestEngine_cancelChange` first required more cancels than 40 for every statement; the update of one row's key has only 3 or 4 calls of `Err`, so it requires more than the rows that the statement changes now.

### What I learned

- Each key of `changeRows` calls `ctx.Err`, so a canceled change has a cancel point per row, and the statement's savepoint takes everything back.

### What was tricky

- The savepoint of the slip check must be rolled back whatever SQLite gives, and SQLite's count of changes set back to 0 (`update t set k = k where 0`), or later counts differ.

### What warrants review

- `run.verify` and `run.verifyTxs` in `simulation_test.go`, in particular the rule of a transaction whose rows end as they began.
- `pair.recordSlips` and `stmttest.RowQueries`.

### Future work

- Mutation testing of the code of slice 8, as `docs/todo.md` says.
