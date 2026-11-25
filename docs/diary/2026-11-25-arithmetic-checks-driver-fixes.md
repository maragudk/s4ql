# Diary: fixes after the review of the driver check of arithmetic

An independent review of the driver check of arithmetic, in `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`, found six gaps: a failed update or delete was never read back, the sentinel of an evaluation error was not tied to its message, rows left open could fail on a direction that s4ql draws, NULL was counted by s4ql's rows, a kind of the reach file was dropped, and `messageKind` kept offsets in the tally's keys. This task closes each on the branch `arithmetic-checks-driver-fix`, which starts from `arithmetic-checks`.

## Step 1: the six findings, the planted faults, and the recount

**Author:** builder (arithmetic-checks-driver-fix)

### Prompt Context

**Verbatim prompt:** "You are a builder in s4ql (`maragu.dev/s4ql`) [...] Your task is small and bounded: close the gaps that an independent review found in the **driver's generated test for arithmetic**, all in `internal/sqlite/crosscheck/drivercheck/drivercheck_test.go` and its `testdata/reached/` files. [...] 1. **A failed statement is never read back.** [...] 2. **The kind of an evaluation error is not tied to its message.** [...] 3. **A latent flake.** [...] 4. **A count that varies between runs.** [...] 5. **A kind that was dropped.** [...] 6. **`messageKind` (near 1379)** has no evaluation kinds [...] If a tightened check shows s4ql doing something it should not, that is a fault in s4ql: do not hide it; record it and report it plainly."
**Interpretation:** tighten the driver check in six places, prove findings 1 and 2 with three planted faults that `TestGenerated` must now catch, prove finding 3 with a test of the reviewer's statement under `-count=200`, recount, and measure which counts vary.
**Inferred intent:** the driver check of arithmetic should fail on a wrong change, a wrong sentinel, or a wrong row, and not flake on a direction.

### What I did

- `pair.compareChanged` takes s4ql's outcome and reads the table back after every compared or justified run of an update or a delete, also a failed one, in `pair.stepRuns` and `pair.openWrite`; it counts a read after an evaluation error outside a transaction and in one. The hand sequence "arithmetic, and a column compared with a column" has a transaction with three failing statements and a select of the table.
- `s4qlText` requires `errors.Is(err, kind)` exactly for the public sentinel whose message starts the error's message ("s4ql: integer overflow" + " in ").
- `pair.justify` holds what `pair.checkEvaluation` did to justify an error. `pair.openRows` reads s4ql's first row with `nextRow` before SQLite's query, and an error there, or of `QueryContext`, or of the rest in `pair.closeRows`, goes to `pair.reopened`: a justified evaluation error of a select with arithmetic and LIMIT or a subquery, and nothing else. `pair.partOf` checks the rows that came before an error of the rest against SQLite's select without LIMIT. A subtest of `TestPair` runs the reviewer's statement on two tables until each path is reached.
- `read` clears `null` on an error; `pair.reach` counts NULL of a QueryRow or a select with LIMIT by SQLite's rows of the run.
- `messageKind` has the three kinds. `TestGenerated.kinds` has the insert of NULL beside a value of another type back at 2, the two reads after an evaluation error at 4 and 2, and a comment on the counts that vary.
- `TestGenerated` runs sequences 40 to 399 of `arithmetic` with no tally. `pair.compare` sends a statement with arithmetic with an evaluated run to `checkEvaluation` before the "not supported" path, and `checkEvaluation` judges a refused run by `checkRefused`.
- Scratch scripts in the session's scratchpad: `kinds.py` (counts of kinds out of the `-v` log), `vary.py` (rows of `.tsv` files that differ), `plant.py` (the three planted faults). Large logs in `/Volumes/990/s4ql/arithmetic-checks-driver-fix/`.
- Commit `a96ea1d`, and a second commit after the code review.

### Why

A run that failed was only assumed to change nothing, the kind of an error could be wrong without a test seeing it, and a reopened select could fail the test on a draw of the host.

### What worked

- The hand sequence caught both engine faults at once, on `"update t2 set i2 = i2 + 1": after it, the table holds in s4ql ... "int64:2"`.
- With 400 sequences of `arithmetic`, `TestGenerated` catches the savepoint fault at seed 11565, the autocommit fault at seed 6445, and the wrong sentinel at once: `s4ql gives the evaluation error "s4ql: integer overflow in * (at offset 106)", which matches s4ql: integer overflow: false, where its message names that kind: true`. The old test with the same fault passes `s4qlText`.
- The reviewer's statement passes 200 times; the `TestPair` subtest reaches both paths in a few runs.

### What didn't work

- `go test ./internal/sqlite/crosscheck/drivercheck/` from the root gives `main module (maragu.dev/s4ql) does not contain package`: the crosscheck is a module of its own, so tests run from `/internal/sqlite/crosscheck`.
- With 40 sequences of `arithmetic`, `TestGenerated` passed with either engine fault planted: of 14 and 7 reads after an evaluation error, none followed a change, since the generator's failing SET mostly fails on every row or on the first, and a WHERE is computed before any change. A scratch run of 1,000 sequences found the autocommit fault in 15 and the savepoint fault in 7; the first are the 202nd and the 233rd.
- Counting all 400 broke the exact pins: five recounts gave `excused: SQLite's error at prepare, s4ql's at the run of the statement` 1394 and 1395, the excuses of rows open, of isolation levels, and of a begin in a transaction each varying by one or two. A select whose arithmetic fails by direction changes whether rows stay open, `pick` draws other calls after it, and the rest of the sequence differs. So the 360 more run with no tally.
- The 400 sequences failed at seed 7437 with no fault planted: `prepare, exec several times: exec failed with s4ql: division by zero in / (at offset 215); then: exec failed with s4ql: not supported: the column ReleaSe.jsOn of type INTEGER for the column ReleaSe.temporary of type REAL; ...` against SQLite's `0 affected`. The refused runs went to SQLite. A gap of the test, fixed by judging each run in `checkEvaluation`. The engine computing a constant of WHERE before refusing a column of SET is its own order, which the rules allow.
- Shell commands with `cd` and `git` together, or loops with `git checkout`, were refused by the harness; single commands with `git -C` worked.

### What I learned

- The driver check's directions come from the host, so any count after a select that fails by direction can drift: it changes `open`, and `pick` draws by it.
- A planted fault in the engine's write path needs an update whose SET fails on a later row in the order of the key; the generator rarely writes one.

### What was tricky

- Telling a fault caught from a gap of the test: the first scratch run "found" the savepoint fault at seed 7437, which was the gap above, and the old test with 400 sequences "found" it through the flake of finding 3.

### What warrants review

- The 360 uncounted sequences in `TestGenerated`, and the time: the test takes about 27 s, from 16 s.
- `pair.reopened`'s rule that only a select with LIMIT or a subquery may fail when run again.
- `pair.partOf` ignores the order of an ORDER BY for the rows before an error.

### Future work

- A generator that aims at an update whose SET fails on a later row, so the counted sequences reach it.
- Fixed directions for the driver check, or a pin per run in `stmttest.Reached`, so the excuses of `arithmetic` cannot drift.

## Step 2: the code review

**Author:** builder (arithmetic-checks-driver-fix)

### Prompt Context

**Verbatim prompt:** (the brief of step 1) "`make test` and the lint target pass; `go test ./internal/sqlite/crosscheck/drivercheck/` passes with `-count=5`, and the new tests with `-count=20` and `-race`."
**Interpretation:** review `86953a5..HEAD` with the code-review skill, fix what matters, and run the checks.
**Inferred intent:** hand back a branch whose checks are honest.

### What I did

- The code-review skill, at the medium level, gave eight findings. Fixed four: the rows before an error of the rest are checked (`pair.partOf`); `pair.reopened` refuses an error of a select without LIMIT and without a subquery, which reads every row in every direction; the `TestPair` subtest requires both paths reached, by where `reopened` counts them; NULL is re-read in SQLite only for a QueryRow or a select with LIMIT.
- Left: the exact pins that could still drift (recorded as open), a run with the wrong number of arguments beside an evaluated run (the runs of a prepared statement all have one number of arguments, so it cannot come), an excuse counted before a later run fails (the test fails then anyway), and nothing checking that the uncounted sequences still reach the case (recorded as open).

### Why

A wrong row before a justified error would otherwise pass unseen, and a test that may never reach its path proves nothing.

### What worked

- Ten runs of `TestGenerated`, and five recounts, saw only floors and the excuse of an evaluation error vary.

### What didn't work

- Nothing failed in this step.

### What I learned

- Nothing beyond step 1.

### What was tricky

- Nothing beyond step 1.

### What warrants review

- As in step 1.

### Future work

- As in step 1.
