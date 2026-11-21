# Diary: The three weakenings that the review of the workload profiles found

An independent review of pull request #123 found three ways in which the branch checks less than `main`: a floor could fall in a recount without a change in a file written by hand, `mixed` ran about a quarter of the sequences that the one sequence of all areas ran before, and several rare kinds were reached about a third as often. This task fixes those three and nothing else.

## Step 1: Hand-written totals, `mixed` back to two thirds, and the rare kinds

**Author:** workload-profiles-fix builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "An independent review found three real weakenings against `main`. Fix them; nothing else. 1. A floor must not fall without a change in a hand-written file [...] Build: each kind in a `.kinds` file carries a hand-written least number for its total over all runs of that test (all profiles, environments, cells). The check fails when the sum of what the runs reached is below it. A recount never writes a `.kinds` file. The starting numbers are the floors `main` has for the same kinds [...] a kind that is new on the branch gets a third of its total now. [...] 2. `mixed` gets its share back [...] in each of the four tests `mixed` runs at least two thirds of the sequences (or seeds) that the whole test ran on `main` (so at least 200, 167, 100, and 40). Pay for it by taking sequences from focused profiles where their least numbers allow, before adding to the total. The time of each package's default run, plain and under `-race`, may grow by at most 10% over the branch as it is now [...] If the two aims conflict, keep the time and say how far `mixed` got. Add two kinds that count a combination [...] 3. Rare kinds [...] After the recount, list every required kind whose total is under 6 and, for each, either raise its odds in the profile it belongs to or leave it and name it in your report. Do not drop a kind."

**Interpretation:** Give every line of the four `.kinds` files a least number of the total of its kind, taken from `main`'s maps by a script; make `stmttest.Reached` check the totals in the check, in `Least`, and in the recount; raise `mixed` in the four tests within the time; add two kinds that only `mixed` reaches; recount; and list the thin kinds.

**Inferred intent:** The floors that `main` had come back as numbers that only a builder can lower, and the meetings of areas that only `mixed` reaches get back the share they had, with a tally that shows when they lose it again.

### What I did

- `/internal/sql/stmt/stmttest/reached.go`: a line of a `.kinds` file is the least number, a tab, and the kind (`readKinds` refuses a line without a number, a number below 1, and a kind twice). `Reached.totals` sums each kind over the runs that the new field `Reached.Totaled` names, all runs if it is nil, and fails below the number. `Check`, `Least`, and the recount call it; the recount then writes nothing. `TestSimulation` sets `Totaled` to the environments, whose counts are the sums of their cells. Tests first, in `/internal/sql/stmt/stmttest/reached_test.go`: the reviewer's edit (a row from 9 3 to 3 1 and the other row of the kind deleted, with runs that reach it 3 times) fails by the total and by nothing else, a total below its number, only totaled runs add, the errors of the file, the recount, and `Least`.
- A throwaway script (`floors.py`, in the session's scratchpad) read `main`'s `differential_test.go`, `tx_test.go`, `simulation_test.go`, and the driver check's test file from `git show origin/main:...`: the maps of least numbers, with the prefix `reached: ` where `main` read `y["reached: "+kind]`, and each list of kinds required once as 1, with the constants `string(callX)` of the driver check folded in. For the simulation it took `int(least*50)` for each environment, as `main` did, and summed the three. It found a number of `main` for 393 of the 397 kinds of `TestDifferential`, all 47 old ones of `TestConn_differential`, all 82 of `TestSimulation`, and 363 of the 370 of `TestGenerated`. The kinds of `main` that no `.kinds` file holds are the integrity checks of the simulation, which are exact rows now, and the two kinds that the driver check requires once in its code, as before. A second script wrote the numbers into the `.kinds` files, and a third of the total for a new kind.
- To read the totals, I set every number to 999999999 and ran the tests in check mode: the failure of each kind says its total and how often each run reaches it, which a script read back. No code was needed for that.
- `TestDifferential`: `sequencesOf` gives `mixed` five times `-sequences`, 200 of 720. `TestSimulation`: the cells of `mixed` hold 34, 33, and 33 seeds, 100 of 264. `TestConn_differential` already ran 40 of `mixed`. Nothing was taken from a focused profile of the engine, since the time allowed it.
- `TestGenerated` of the driver check: `mixed` runs 128, and the focused profiles 13 to 56 (`generatedOf`), 411 in all where there were 362. A throwaway hack dumped the tally after each sequence of each profile with a timestamp, from which I read, for each focused profile, the fewest first sequences that still meet every row of its own in the `.tsv` before this step, and the time of a sequence of each profile (`mixed` 15.2 ms, `arguments` 11.9, `changes` 24.5). The hack is removed.
- Two kinds that count a meeting of areas, in `pair.reach` of `/internal/engine/differential_test.go` and `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`: "insert that leaves out a column with a default that an index holds ok" (`stmttest.LeavesOutIndexed`, with a table test in `/internal/sql/stmt/stmttest/rule_test.go`) and "value on the left on a table that a create table if not exists found" (the pair keeps the tables that such a create table found, until a drop table succeeds). Only `mixed` reaches either: 47 and 144 times in `TestDifferential`, 7 and 49 in `TestConn_differential`, and 19 and 62 in `TestGenerated`. All three require both.
- Recounted the four tests with `S4QL_RECOUNT=1`, and checked the diff of each `.tsv` row by row with a script.
- Timed each package back to back with test binaries built before the change (in `/Volumes/990/s4ql/workload-profiles-fix/before/`) and after, see below.

### Why

A third of each run's count is only as good as the count that the recount writes. Totals that only a person writes make a fall a diff of a hand-written file. Totals over all runs, rather than numbers by run, keep the profiles free to move a kind from one run to another, which is what a change of a profile's sizes does.

### What worked

- `main`'s floors all hold in the new totals, also "insert with two faults: a value of another type" of the driver check, which the review found at 3 against 7: 128 sequences of `mixed` reach it 7 times, and the run 13 times in all.
- In the engine's `.tsv` files no row fell: only the rows of `mixed`, of its cells, and of the environments that hold them rose or were added.
- The engine's package, back to back with the binaries of before, twice, on a machine with a load of 12 to 20 from other agents: 83.5 s and 102.5 s before, 88.7 s and 97.4 s after; under the race detector 144.1 s and 142.2 s before, 152.3 s and 144.4 s after, 3.6 in 100 more on average. Under the race detector the generated tests run a tenth, so more of `mixed` costs little there.
- The rare kinds of `TestDifferential` that the review named rose with `mixed`: "argument float32 that is not finite" from 5 to 11 (`main` 5), "argument bool" from 3 to 9 (`main` 3), and "create table if not exists duplicate column name" from 4 to 6 (`main` 4).

### What didn't work

- The harness refused a loop that wrote `git show` into files of the scratchpad: "this command names git in a form too complex to verify that it stays inside the worktree". One plain command per file passed. It also refused `for f in ...; do ...; done; git diff --stat -- '*.kinds'` for the git command at its end.
- The first sizes of the driver check, `mixed` 168 and the focused profiles cut to 16 to 40, left six kinds without a run that reaches them three times, among them "create table if not exists duplicate column name" (`mixed` 2, `exists` 2) and "read-only transaction: insert while rows are open" (1 in all).
- A second set with `tables`, `changes`, and `indexes` at 8 sequences passed the recount and took the package 6.7 in 100 over, but 59 rows of those three profiles fell below their own least numbers, such as "insert with a column without a value: NOT NULL constraint failed" of `tables`, 9 with a least number of 3, to 0. That is not "where their least numbers allow", so I took the sizes that meet each profile's old rows from the dump instead.
- With those sizes and `mixed` at 160, the package took 11.71 s against 10.56 s, 10.9 in 100 more, over the limit. `mixed` at 144 took 11.47 s, 8.6 in 100 more, but under the race detector 38.47 s against 33.86 s, 13.6 in 100 more (three runs each, back to back). `mixed` at 128 took 36.62 s under the race detector, 8.2 in 100 more, and 11.29 s without it against 10.45 s, 8.0 in 100 more. The aims conflict: `mixed` got to 128 of the 167.
- My model of the time of the driver check, the time of a sequence of each profile times the sequences, gave 26.5 s for `TestGenerated` under the race detector at `mixed` 144, where it took 28.3 s: a later sequence costs more than an early one, likely since the world that the sequences share grows.

### What I learned

- The rare kinds of the driver check sit at the edge of their profile's size: "create table if not exists duplicate column name" reaches 3 in `exists` from 55 sequences, "beside a write transaction: select ok with rows" 3 in `defaults` from 56, and "value on the left of > finds rows" 3 in `valuefirst` from 33. A sequence more or less of such a profile decides whether the kind has a row.
- A sequence of `mixed` costs less than one of most focused profiles in the driver check, since more of its texts fail early.

### What was tricky

- Which runs add to a total. In the simulation an environment's counts are the sums of its cells, so a total over every run counts each seed twice; `Reached.Totaled` names the environments.
- "Its total now" for a new kind: the numbers of the new kinds of the driver check were first a third of the totals with `mixed` at 168, and had to be taken again at 128.

### What warrants review

- `/internal/sql/stmt/stmttest/reached.go`, `Reached.totals`, and that `Least` checks the totals too, also for the nightly runs.
- The four `.kinds` files against `main`: the scripts are in the session's scratchpad, and their method is above.
- `generatedOf` in `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`: each size is the fewest first sequences that meet the profile's rows of before, or that a kind needs, and the rows of `changes`, `defaults`, `tables`, and `transactions` fell in the diff of `TestGenerated.tsv` for that reason.
- The two kinds of meetings: the set of found tables is kept by the pair, so a roll back of a transaction can leave a table in it that is gone, which makes the count a little higher than the meeting; it is a tally, not a check of an answer.

### Future work

- The kinds under 6 in the totals, which I left: see the todo file of the profiles.
- `TestSimulation` does not count the two meetings; `tallyOf` would.
