# Diary: Slice 8 of the roadmap in the engine and the driver, `update` and `delete`

Slice 8 adds `update t set b = <value>, c = <value> [where ...]` and `delete from t [where ...]`. The statement pass reads both since pull request #88 (branch `update-delete-stmt`, diary `2026-11-20-update-delete-stmt.md`). This task is the core, the engine, and the driver: find the rows as a select finds them, change them without a cursor open on a table that changes, SQLite's errors in SQLite's order, `RowsAffected`, and the net effect in the commit object. The work is on the branch `update-delete-engine`, from `origin/update-delete-stmt` at `bcac770`.

## Step 1: The set of keys in the core

**Author:** update-delete-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Statement by statement, s4ql does what SQLite does: same rows afterwards, same `RowsAffected`, same error (message and codes) and the same order among errors as SQLite [...] Rows are found as `select` finds them: `keyAccess` [...] and `Filter`. Reuse; do not duplicate. The test option that forces a full scan must cover update and delete too [...] Changing rows while finding them must not rest on undefined behaviour of SQLite (modifying a table under an open cursor on it), and must be right when the update changes the key or the columns the `where` reads (no row changed twice, none missed). It must work for any number of rows without holding them all in memory in Go [...] Choose the mechanism, give the reason in the code's comments and the diary, and add what core needs [...] The commit object a transaction writes has the net effect per row [...] Test this through the log, not only in the cache. [...] Driver: `Exec` with update and delete in and outside transactions [...] Remove what the statement pass left as temporary [...] The generated checks [...] are the NEXT builder's job"
**Interpretation:** Run both statements end to end with SQLite as the judge of every row, count, and error; find the rows with the operators of the select; collect the keys of the found rows in the cache first, and change the rows after, so that SQLite never has a cursor on a table that changes; check the commit objects and a replay.
**Inferred intent:** Slice 8 with the same trust as slices 1 to 7, and hooks for the builder of the generated checks.

### What I did

First SQLite itself, with the pinned shell built from `internal/sqlite/c/shell.c` with `internal/sqlite/options.h` into the scratchpad. Facts (table `t (a int primary key, b text, c real not null, d blob not null)`):

- `update t set c = null where a = 9`, which finds no row, succeeds with `changes()` 0: SQLite checks NOT NULL for each row that it changes, when it steps.
- `update t set d = null, c = null where a = 1` gives `NOT NULL constraint failed: t.c`, and `set c = null, a = null` gives `t.a`: the first column in the order of the table, the key among them, as for an insert.
- `update t set a = 2 where a = 1` with a row 2 gives `UNIQUE constraint failed: t.a`; `set a = 1 where a = 1` changes 1 row; `set a = 9 where a >= 2` of two rows gives UNIQUE; `set a = 2, c = null where a = 1` gives NOT NULL first.
- With `.limit length 20`: a too-long string literal in SET or WHERE gives no error when no row is changed; a row whose record is too big gives `string or blob too big`, also when the new key exists (the record is made before the key is looked up); NOT NULL comes before the record. `no such column` comes before every length. Under that low limit, NOT NULL gives only `constraint failed`, because SQLite's message is longer than the limit.

The mechanism: `Tx.Keys(ctx, name, f)` in `/internal/core/tx.go` makes a temporary table `s4ql_keys_N` of the key columns of the table for the call of f, `KeySet.Add` puts a key in with `insert or ignore` (checked as `Tx.Delete` checks a key, `ErrTooBig` from SQLite passed back as for a write), and `KeySet.All` gives the keys in key order, each with `select ... where (k0, ...) > (?, ...) order by ... limit 1`, a statement that has ended before the caller gets the key. Keys drops the table and finalizes the three statements at rest on it when f returns, unless the transaction can only be rolled back. `createKeyTable` is the creation of the temporary tables that `touch` had inline.

### Why

SQLite's documentation leaves undefined what a SELECT gives when its own table changes while it steps; it defines a change of another table. So the engine's update and delete scan with the operators of a select and add only to the set while the scan runs, then read one key at a time and change its row while no statement is open. The set is in the cache, as the touched keys are, so a statement can change any number of rows, and the keys of rows that the statement writes, also under a new key, are never in the set, so no row is changed twice. A callback bounds the life of the set, so no table and no statement at rest outlives the statement.

### What worked

`TestTx_Keys` in `/internal/core/tx_test.go`: the edges of each type, shuffled and added twice, come back once each in the model's order (-0.0 as 0.0); a key of two columns, 3,000 keys; changes of the table and the set between two keys (a key added after the last comes, one before does not); the error of f, a set in a savepoint that rolls back, and a set that is gone (`ErrInvalid`); no such table, a key of another type or width, and a key of the length limit, whose record SQLite refuses. `go test -race -count=20` passed.

### What didn't work

- A Bash heredoc that appended to the test file was refused ("too complex to verify that it stays inside the worktree"); the Edit tool worked.
- The expected state in one subtest lacked the type in `read`'s format (`t(c0*0:text)`).

### What I learned

The core already writes the net effect of any mix of upserts and deletes per key (`touch`, `spoolTouched`), so an update that changes the key is a delete and an insert, and the commit object holds the new row and the delete of the old key with no change to the format.

### What was tricky

Where the type refusal of SET goes, since SQLite has none: see step 2.

### What warrants review

- `Tx.Keys`, `KeySet.Add`, `KeySet.All`, and `dropKeys` in `/internal/core/tx.go`, in particular the drop after a failed f and the finalize of the statements at rest.

### Future work

- The engine and the driver: step 2.

## Step 2: The engine, the driver, the generated tests, and the review

**Author:** update-delete-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "An unknown column in `set` or `where` gives SQLite's error, in SQLite's order [...] where the exact-type refusal has no counterpart in SQLite, place it as `insert` places it. A statement that fails changes nothing [...] Remove what the statement pass left as temporary: the refusal in `engine.check`, the `engine.Check` call in `conn.go`'s `check`, and the `checkText` wrappers in the tests where they now can go [...] Decisions on details the roadmap does not settle: [...] record them in `docs/todo.md` in a group \"decisions of the engine step of slice 8, for the lead to confirm\"."
**Interpretation:** Plans, operators, and errors for both statements; the driver; tests by hand against SQLite both ways; the temporary refusals gone; decisions and the README written down.
**Inferred intent:** Slice 8 working end to end, ready for the builder of the generated checks.

### What I did

- `/internal/engine/plan.go`: `Update` (with `Assignment`) and `Delete`, whose `Source` is `rowsOf`, the scan or filter that a select reads too, and their `Format`.
- `/internal/engine/operator.go`: `build.filter` split into `build.conditions` (names) and `build.bind` (lengths, types, access), so that `build.change` can put the SET between them in SQLite's order; the sinks `update` and `remove`; `changeRows`; `finds`. The sink's `check` takes a context, because NOT NULL of an update depends on whether it finds a row. `buildSink` takes `scanAll`.
- `/internal/engine/storage.go`: `writer.delete`, `writer.keys`, `keySet`, `coreKeys`; `fake` and `memory` got both. `/internal/engine/errors.go`: `noSuchSetColumn`. `/internal/engine/engine.go`: the refusal in `check` is gone, `setsChanges`, the doc of `Exec`. `/internal/engine/telemetry.go`: kinds `update` and `delete`.
- `/conn.go`: `check` is `stmt.Check` alone again; `/s4ql.go`: the doc of `RowsAffected`.
- Tests: `TestEngine_update`, `TestEngine_delete` (with `changeTrio`: SQLite, an engine by the key, and one that reads every row, comparing answer, count, and state after each statement; the 44 shapes; lengths at a limit of 1000; a table of 2,000 columns), `TestChangeRows` (calls of the fake both ways), `TestNewPlan`, cases of `TestConn_Begin` (in a write transaction, a roll back, read-only), `TestConn_netEffect` (commit objects read from the log with `logChanges`, and a replay), and the driver's `TestUpdateDelete`.
- The `checkText` wrappers are gone. The generated tests then ran the update of `stmttest.Others` and failed on its type refusals (`"update \"WiTH\" set \"AnalyZe\" = 4": the engine does not support a statement that the pass supports`), so `stmttest.UpdateRule`, `DeleteRule`, `WhereRule`, and `FoundQuery` went into `/internal/sql/stmt/stmttest/rule.go`, used by `typeRule`, `typeRefusal`, the new `readOnlyRule`, and `drivercheck`'s `pair.typeRefusal`. Pins moved: `excusedType` 478 to 483 in `TestDifferential`, 231 to 234 in `TestGenerated`.
- `/internal/sql/mutation/equivalent.txt`: the equivalent mutants of `Tx.touch` that moved to `Tx.createKeyTable`, and `#3` became `#2`.
- `docs/todo.md`: the decisions, and the item for the generated checks. A writer sub-agent updated `/internal/engine/README.md`; I read its diff and fixed the sentence on the exceptions of the count.

### Why

Splitting `build.filter` keeps one implementation of the conditions for select, update, and delete, as the brief asked, and puts SQLite's binding order (SET's parameters first) in one place. The rules in `rule.go` are the smallest thing that let the wrappers go without leaving updates out of the generated tests.

### What worked

- `TestEngine_update`, `TestEngine_delete`, `TestChangeRows`, and `TestConn_netEffect` agreed with SQLite at the first run, apart from my expected messages (the `sqlite: ` prefix is not in `Error()` of the engine's error, and a `RETURNING` offset of 21).
- Two planted mutants failed `TestEngine_update` at once: every update written in place (52 subtests failed) and NOT NULL without a found row (48).
- `-race -count=50` of `TestChangeRows`, `TestConn_netEffect`, `TestEngine_delete`, `TestNewPlan`, `TestConn_Begin`, `TestUpdateDelete`, and `TestUpdateRule`; `-count=20` and `-race -count=2` of `TestEngine_update`; `S4QL_ENGINE_SIM_SEEDS=300` of `TestSimulation`; `make lint`; `make check-browser`.

### What didn't work

- `make test` failed in `internal/sql/mutation`: `no such mutant: internal/core/tx.go Tx.touch condition: err != nil => false #3`, from the move of the creation of the key tables into `createKeyTable`.
- Bash commands with backticks in a heredoc or a Python script, and a loop that named git in a pipeline, were refused as too complex to verify; the Edit tool and plain commands worked.

### What I learned

- SQLite's update gives `no such column` for SET itself, without the question about quotes, and checks NOT NULL per row, so an update that finds no row takes NULL in a NOT NULL column.
- The core commits nothing for a transaction that changed nothing, so an update or a delete of its own that finds no row leaves no commit object and its annotation is nowhere.

### What was tricky

- Where the type refusal of SET goes: after NOT NULL as for an insert, but NOT NULL depends on the data, so `update.check` asks the source for a first row.
- The generated tests: the old wrappers became no-ops once the engine stopped refusing, so they could not stay; the rules had to come in.

### What warrants review

The review (`code-review high bcac770...HEAD`) found one real problem and two nits; the writer found two more in the tests:
- Fixed: `run.verify` of the simulation required every acknowledged statement in the log, also an update or a delete that found no row (`attempt.unchanged` now excuses it).
- Fixed: `tableOf` of the simulation panicked for an update in a transaction with more seeds (`a statement of type *stmt.Update`).
- Fixed: `pair.step` accepted a count of 0 after the read-only error and a record too big only for an insert (`changesRows`).
- Fixed: `drivercheck`'s found query did not check `rows.Err()`; `s4ql.go` had a ragged comment.
- To review: `changeRows`, `update.check`, `build.change` in `/internal/engine/operator.go`; `UpdateRule` in `rule.go`.

### Future work

- The generated checks of slice 8, and mutation testing of its code: the item in `docs/todo.md`.
- An update or a delete by a point lookup makes and drops a temporary table of keys for one row; a path without it is possible if the cost matters.
