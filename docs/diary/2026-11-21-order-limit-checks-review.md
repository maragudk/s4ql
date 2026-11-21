# Diary: the review of the generated checks of slice 13

Pull request #125 brought the generated checks of slice 13, `order by` and `limit` on `select`. CI failed on it, in `TestSimulation_plantedBugs/turnMixed` under the address sanitizer, and an independent review found gaps: the access that a run reads by was never compared with the model, two hand-made faults of `delivers` passed every generated test, `excusedSortTooBig` was too wide, some least numbers were below a third, the driver check required no kind of slice 13, the README of the engine had stale totals, and the jobs of CI had grown by more than 10%. This diary is the fix of those nine points, on the branch `order-limit-checks`.

## Step 1: the nine fixes of the review

**Author:** builder (order-limit-checks, review)

### Prompt Context

**Verbatim prompt:** "You are a builder on s4ql (repository maragudk/s4ql, Go): a SQL database on S3 with a local SQLite cache and its own SQL engine. [...] CI fails on the branch, and an independent review found real gaps. Fix these; nothing else." followed by the nine points "CI failure", "The access a query really used is never compared with the model", "A wrong-order fault in `delivers` is caught once in 800 sequences, and only by the model", "A pinned column in the middle of a key is seldom generated", "`excusedSortTooBig` is too wide", "Least numbers", "The driver check", "`internal/engine/README.md`", and "CI time", and the rules (no timing in tests, no `internal/sql/mutation/`, scratch on `/Volumes/990`, notes in `docs/todo/slice-13.md`, this diary, push and wait for CI, do not merge).
**Interpretation:** make the checks of slice 13 catch the faults that the reviewer planted by hand, through generated shapes that reach them and a check of the access of the run, make `turnMixed` found in one seed in five in every mode of CI, and bring the jobs of CI back near `main`.
**Inferred intent:** trust in the ORDER BY of slice 13 rests on checks that a fault of the choice or of the order cannot slip past, and on CI that is green and not slower.

### What I did

- `/internal/engine/differential_test.go`: `pair.checkAccess` compares `Rows.Access` of every select that SQLite and s4ql take with `stmttest.ChosenAccess` on the indexes that SQLite lists, after the comparison of the rows; `Engine.Forced` in `/internal/engine/export_test.go` leaves out an engine that forces a path. `reachKeyShapes` counts "ORDER BY delivered around a pinned column" and "ORDER BY of the key of an index with a gap, over rows that the gap orders". `TestDifferential_plantedBugs` takes a list of failures for each bug, its own first, and requires its least by its own. `sortTooBig` refuses a select with a long condition, with a case in `TestPair_sortTooBig`. `shardedAdd` shares the seeds of `FuzzTypes`, `FuzzTypes_core`, and `FuzzSQL_core` among the shards of the sanitizer.
- `/internal/engine/bugs.go`, `/internal/engine/access.go`, `/internal/engine/operator.go`: the planted bugs `skipGap` (a key (x, y, z) delivers ORDER BY x, z) and `lastIndexWins` (the choice at run time reads the indexes reversed).
- `/internal/sql/stmt/stmttest/sequence.go`, `/internal/sql/stmt/stmttest/profile.go`: `Profile.Aimed` and `Sequence.aimedWhere` (goqite's receive, a pin at the start or in the middle of a key with an ORDER BY of the rest, a range after a pin at the start), `Profile.Ties` and `tieValue`, the forms of an ORDER BY of a key in `orderColumns` (a gap, mixed directions with the first or second column turned, neither), `keysOf`; the profile `order` has three columns in each table and weighs the selects as the inserts.
- `/internal/sql/stmt/stmttest/rule.go`: `WhereCases` compares an integer and a real exactly (`sameValue`), with two cases in `TestWhereCases`.
- `/internal/engine/simulation_test.go`: the three planted bugs of slice 13 run in the hammer environment.
- `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`: `reachOrder` and eight required kinds, 50 sequences of `order`, and `again` keeps an integer argument of LIMIT in the other runs of a prepared select.
- `/internal/engine/access_internal_test.go`: two cases of a gap in `TestChooseAccess_orderBy`.
- `/Makefile`: `ASANSHARDED` holds the three fuzz tests, and `ASANLAST` runs six heavy tests of the engine whole in the last shard. No workflow file changed.
- Recounts of the four `.tsv`, every kind of slice 13 at a third of its total (goqite's receive with a pin at 6 of 18), and the docs: `/internal/engine/README.md`, `/internal/sql/stmt/stmttest/README.md`, `/internal/sqlite/README.md`, `/docs/todo/slice-13.md`, `/docs/todo/generated-workload.md`.

### Why

The model was checked only through `engine.AccessOf`, which takes the indexes as the test gives them; the run reads its own. A sort gives correct rows whatever the access, so a wrong choice shows only against the model, and a wrong delivered order only where rows tie on the earlier columns. The generator wrote the shapes where the reviewer's faults matter too rarely: a gap needs a key of three columns, a pin in the middle needs a WHERE drawn for the key, and every shape needs rows that tie.

### What worked

- The reviewer's reversal of the indexes, as `lastIndexWins`, is found by `pair.checkAccess` alone in 7 of the first 12 sequences of `order`. `skipGap` is found in 11 of 30, 4 of them by the rows of an ordered select.
- The reviewer's second fault, by hand (`for at < len(key) && !matched && pinOf(...)` in `delivers`), failed `TestChooseAccess_orderBy/a_pinned_column_in_the_middle_of_the_key`, and `go test ./internal/engine/ -run 'TestDifferential$|TestConn_differential' -short` with "the engine chooses a scan of every row, sorted, and the model a scan of the index immediate, delivered backward" and "the run reads the rows by a scan of every row, sorted, and the model chooses a scan of the index immediate, delivered backward".
- `turnMixed`, `limitFirst`, and `ascendLast` in the hammer environment: 49, 82, and 118 of the first 200 seeds, the same plain, under `-short`, under `-race`, and with `asanEnabled` set true in `noasan_test.go` for the run (reverted after). The environment has no crashes, so the sanitizer's mode plays the same seeds.
- `make test`, `make lint`, `make check-browser`, and `make fuzz-shards FUZZGROUP=c FUZZSHARDS=4` pass.

### What didn't work

- The aimed WHERE wrote a select that panicked in `stmttest.WhereCases`: `panic: interface conversion: interface {} is float64, not int64`, from `compareValues` at `rule.go:845`, for `... where x'00' == glob and "TEmP" > -1.5E+00 and ? >= temp order by ..., "a" desc` on an INT key. A column of ORDER BY that the table does not have makes `ConditionRule` give "", so the contradiction check met a real and an integer.
- The first gap kind was reached once in 800 sequences: keys of three columns were rare in tables of two, and the rows seldom tied. Three columns for each table of `order` and `Profile.Ties` raised it to 17.
- In the default environment of the simulation, `turnMixed` stayed at 9 to 13 of 60 seeds whatever I changed; a count of the seeds where the bug's path fired showed 41 of 60 with a hit and 13 found, because most ordered selects give 0 to 2 rows there. The hammer environment raised it.
- `go test` with `S4QL_ENGINE_SIM_SEEDS` gave the same counts after a change of the generator twice; I could not rule out a cached result, and used `-count 1` from then on.
- The driver check failed at 50 sequences of `order`: `"... order by REGEXP DESC limit ?" with the arguments []interface {}{3}: s4ql gives prepare, exec several times: exec failed with s4ql: not supported: argument 1, which is a negative integer, for LIMIT, ...; then: 11 affected, last insert id 0, where the rule gives ""`. `again` drew 3 for the second run after -2^63. It now keeps an integer argument of LIMIT.
- Several commands with a heredoc or with `git` beside other commands were refused by the permission system ("this command is too complex to verify that it stays inside the worktree"); I wrote the scripts to the scratchpad and ran them alone.

### What I learned

- `Rows.Access` and `AccessOf` share the code of the choice but not its input; a test that checks only one of them leaves the indexes of the run unchecked.
- `-tags asan` without `-asan` does not build: `runtime2.go:740:28: ... overflows uintptr`. The sanitizer's mode of the simulation is the constant `asanEnabled`, which a local run can flip in `noasan_test.go`.
- CI's job times vary by half between runs of `main` itself: `asan-engine (1)` 223 to 421 s, `test-engine` 182 to 319 s.

### What was tricky

- Drawing the aimed WHERE from `rOrder` inside `Sequence.where` changes no other profile only because `Profile.Aimed` is 0 there; the draws must be gated before any draw.
- `skipGap` changes the choice too, so `pair.checkAccess` can find it before the rows differ; the own failure of a bug had to become the first of a list.

### What warrants review

- `pair.checkAccess` and `Engine.Forced`: an engine with only a direction forced is checked, one that forces a path or a sort is not.
- `Sequence.aimedWhere` and the forms of `orderColumns`, against the README of `stmttest`.
- `ASANLAST` in the `Makefile`: the six tests leave the first shard and run in the last; a stale name runs nothing and leaves nothing out.
- The job times of CI against `main`, given the noise above.

### Future work

- "LIMIT that cuts a group of rows that tie" (4) and "delivered select whose rows that tie came in the order of the key of its access" (14, from 31) are thin; see `/docs/todo/generated-workload.md`.
