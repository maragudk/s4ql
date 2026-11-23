# Diary: the review of the generated checks of slice 14, RETURNING

An independent review of the generated checks of slice 14 (`returning` on `insert`, `update`, and `delete`; branch `worktree-agent-adba0e2e9333a5567`, diary `/docs/diary/2026-11-23-returning-checks.md`) planted 19 faults of its own in the production code. The generated checks found 16, table tests alone found 2, and one survived everything. Nothing blocked the merge, and the production code was right. This step fixes what the lead chose from the findings, all in tests and documents.

## Step 1: the order by statement, two aims, a driver kind, and stale text

**Author:** builder (returning-checks-review)

### Prompt Context

**Verbatim prompt:** "You are fixing the findings of an independent review of the generated checks of slice 14 of s4ql (`maragu.dev/s4ql`) [...] The reviewer planted 19 faults of its own in the production code. 16 were found by the generated checks, 2 only by table tests, and one survived everything. Nothing blocks the merge, and the production code is right. These are the fixes I want; all are in tests and documents: 1. **The order of `returning` rows is pinned for all statements together, not per statement.** [...] Fix: the kinds in `pair.checkReturned` name the statement, both directions are required for `update` and for `delete` [...] in every test that counts them, and the table test of variation covers a `delete` as well as an `update`. Then prove it: plant the fault for `delete` only and for `update` only (by hand, reverted afterwards), and show that the generated checks fail for each. 2. **Two faults were found rarely** [...] Make them sturdier [...] Plant each fault by hand, count how many sequences now find it, plain and under `-short`, and report the counts. [...] 3. **A driver kind is not required**: "update with RETURNING through exec ok" [...] Require it. 4. **Stale text** [...] 5. **One line in the engine's README** [...] 6. **Open item**, in `docs/todo/generated-workload.md` [...] Keep every existing least number honest [...] no required kind may be lowered or dropped to make a run pass." There was no new message from Markus.
**Interpretation:** split the kinds of the order of RETURNING by statement and require all four; aim the profile `returning` so that the two rare faults are found by many sequences, with counts before and after; require the driver kind; correct two documents and add one line and one open item; recount by the rules of `/internal/sql/stmt/stmttest/README.md`.
**Inferred intent:** the generated checks should catch each of the reviewer's faults reliably, also under `-short` where that costs nothing, and the documents should say what the checks really check.

### What I did

**Point 1.** `pair.checkReturned` in `/internal/engine/differential_test.go` takes the statement and counts "update with RETURNING whose rows came in the order of the key" and its reverse, and the same for a delete. The first part of `TestEngine_returning` in `/internal/engine/returning_test.go` loops until all four are reached (at most 20 rounds), and the table test of variation in `TestConn_returning` runs a delete of the three rows after the update of them, over 40 seeds, and requires both orders of each at least 5 times. Both `.kinds` files of the engine have the four kinds in place of the two. `TestConn_differential` in `/internal/engine/tx_test.go` runs 80 sequences of `returning` where it ran 40, because with 40 the delete in the reverse order was reached twice, and a required kind needs a run that reaches it three times.

The proof, with a fault planted by hand in `Engine.order` in `/internal/engine/engine.go` (draw the direction, then set `backward` to false for a `*Delete`, or for an `*Update`), and reverted with `git checkout`: for the delete, `go test -count=1 -run '^(TestDifferential|TestConn_differential)$' ./internal/engine/` failed with "returning reaches "reached: delete with RETURNING whose rows came in the reverse of the order of the key" 0 times, and at least 5 were expected, a third of 17" and "testdata/reached/TestDifferential.kinds requires 7 of them", and the same in `TestConn_differential` ("requires 2 of them"); the table tests failed with "the rows of RETURNING of an update or of a delete came in one direction only in 20 rounds" (counts 40, 0 for the delete) and "Expected "2", but got "1"" for `delete`. For the update, the same four tests failed on the reverse of the update.

**Point 2.** I measured with a temporary test, kept out of the branch, that ran `differentialFind` on the sequences of `returning` and `mixed` that `TestDifferential` runs (80 and 200, and 8 and 20 under `-short`) and counted those that fail with a fault planted by hand. The fault "update that moves the key returns the old key" was `given[u.key] = old[u.key]` before `u.ret.add` in the branch of a new key of `update.run` in `/internal/engine/operator.go`. The fault "`*` puts the columns with a default last" was in `buildReturning` in `/internal/engine/returning.go`. Before: 4 of 80 and 6 of 80 sequences of `returning`, 5 of 200 of `mixed` each, and none under `-short`. The generator changes, in `/internal/sql/stmt/stmttest/profile.go` and `/internal/sql/stmt/stmttest/sequence.go`, each a field that is 0 in every other profile: `Profile.NewKey` (an update of the row of a key by its key, whose SET has the key first with `keyOfStep` of the update's own number), `Profile.EveryRow` (an update without WHERE of columns outside the key), and `Profile.DefaultFirst` (one table in two has a default on its first column and none on its last), and the weight of updates in `returning` 12 where it was 7. After: the moved key 23 of 80 and 4 of 8 under `-short`; the defaults last 42 of 80 and 6 of 8 under `-short`. `mixed` did not change, 5 of 200 and 0 of 20 for each. The short run is no slower; it runs as many sequences. Two kinds count what the aims reach, in `reachReturning`: "update with RETURNING of the key that moves a row to another key", where SQLite has no row with the key of the SET before the update, and "`*` that gives a row of a table with a column without a default after one with a default", by statement. Two subtests in `/internal/sql/stmt/stmttest/sequence_internal_test.go` pin the shares of the three fields.

**Point 3.** "reached: update with RETURNING through exec ok" was reached 11 times by `returning` in `TestGenerated`, but had no row in the `.tsv`, because the `.tsv` holds only kinds of the `.kinds` file. It is required with 3.

**Points 4 to 6.** The README of the engine and "Generated checks" of `/docs/todo/slice-14.md` no longer say that `run.statement` checks that a failed `Query` gives no row; they say that `Conn.queryText` gives rows only with a nil error, and that `run.statement` reads none after an error (it guards `rows.All` with `err == nil`). The README says that the simulation checks the rule on the engine, and that through the driver `TestReturning` alone checks an unknown outcome and a commit applied while the context ends or the database closes. `/docs/todo/generated-workload.md` has the open item of the context between the commit and the rows.

**Recounts.** `S4QL_RECOUNT=1` for `TestDifferential`, `TestConn_differential`, `TestSimulation`, and `TestGenerated`. Only `returning` and the environments that hold its cells moved, and in `TestDifferential` two rows of `mixed` of the renamed and new kinds. The new least numbers are a third of totals that I counted with a second temporary test over all profiles.

### Why

A kind that does not name the statement lets the varied rows of one statement stand in for the fixed rows of the other, so the survivor passed. A fault found by 2 of 30 sequences passes most changes of the generator unseen; aiming the profile at where it shows is how slice 13 made its thin kinds sturdy.

### What worked

The temporary counting test made each change of the profile a run of seven seconds, so I could try three settings (weights 7, 10, and 12 for updates, `NewKey` 3 and 2, `EveryRow` 3 and 2) and pick by the counts of the faults and of the order of the update. The planted faults found the gaps exactly where the reviewer said.

### What didn't work

- The profile change moved counts in the other tests, and three recounts failed at first. `TestConn_differential`: "no run reaches "reached: delete with RETURNING whose rows came in the reverse of the order of the key", which is required, three times: the runs reach it returning 2 times", fixed by 80 sequences of `returning`; and the same for the delete with `*` on a table with a default before a column without, which I left out of `TestConn_differential.kinds` with a comment (`TestDifferential` requires it). `TestSimulation`: "no run reaches "statements with RETURNING of their own through Query whose outcome is unknown", which is required, three times: the runs reach it default 2, [...] hammer 2, hammer/mixed 1, hammer/returning 1 times", unchanged with 10 seeds of `returning` in the hammer environment, fixed with 16 (hammer 3). `TestGenerated`: "delete with RETURNING through exec ok" and "delete with RETURNING through prepare, exec several times ok" reached twice; 40 sequences fixed the first, 50 both.
- The excuse `excusedReturningTooBig`, pinned at 1 for `returning`, is no longer met by the default run. I added a subtest to `TestPair_returning` that pins it on three statements.
- Several of my edits ran as `python3 /dev/stdin <<'EOF'` after the sandbox refused `python3 - <<'EOF'` inside compound commands, and silently changed nothing. I saw it only when a planted fault did not fail the tests and `git diff internal/engine/engine.go` was empty. I redid them from script files in the scratchpad.

### What I learned

The `.tsv` of the driver check has rows only for kinds in its `.kinds` file, so a kind missing from the `.tsv` says nothing about how often it is reached; the log of `go test -v` does. "select * with NULL ok" in `TestGenerated` varies by one between two recounts of the same code, for `mixed` and for `order`; it is not caused by this change, and its least numbers are far below.

### What was tricky

Every aim draws from the stream of `changes` or of `defaults`, and moves every later statement of `returning`, so each change of the profile moved every count of that profile in four tests. Raising updates took deletes, and the driver check's rarest deletes fell below three; more sequences were cheaper than a second change of the weights. The kind of the moved key had to be counted before SQLite runs the statement, which `pair.step` does (`p.reach` before `o.run`).

### What warrants review

- The new fields of `Profile` and their draws in `Sequence.update` and `Sequence.shapeOf`; only the digest of `returning` changed, which `TestNewSequence` pins.
- The new least numbers in the three `.kinds` files, each a third of the totals of 2026-11-23, and the dropped kind in `TestConn_differential.kinds`.
- The run sizes: `TestConn_differential` 80 of `returning`, `TestGenerated` 50, the hammer cell of `returning` 16 seeds. Run time of `go test -count=1 ./internal/engine/` on the same loaded machine (load about 13), the base `a841d8c` from an archive in `/Volumes/990/s4ql/` and this branch back to back: 112.3 s before and 114.9 s after, 2.6 s more; a first run of the base, alone, took 112.5 s.
- The planted bugs of slice 14 in `TestDifferential_plantedBugs` now find 21, 16, 13, and 16 of 30; `oldValues` and `dropLast` require 7 and 4. `TestSimulation_plantedBugs` with `S4QL_ENGINE_SIM_SEEDS=200` finds `rowsOfFailed`, `oldValues`, and `dropLast` in 62, 149, and 143 seeds, 62, 54, and 125 by their own violation; the README and the comments beside the bugs say so.

### Future work

The thin kinds of slice 14 in `/docs/todo/generated-workload.md`, above all the unknown outcome in the simulation, which one run reaches exactly three times.

### Self-review

The code-review skill (medium) found no correctness bug and three nits, all fixed: the share test of the three fields counted a NewKey update whose SET had the fault of the changes as a ByKey update; the comment of `generatedOf` lost the rows left open in a transaction as a reason for the extra sequences; and the record of the earlier step in `/docs/todo/slice-14.md` still said that `TestGenerated` requires four kinds once. `make lint` gave 0 issues, `make test-sql` passed, and the changed table tests passed with `-count=20`, and with `-race -count=5`.
