# Diary: Workload profiles and random streams of their own for the generated checks

The generated tests draw one mixed sequence of fixed length from one random stream, and every slice that added a feature to it made some tallied kinds rarer and shifted every later draw, so that pinned numbers moved everywhere and least numbers were lowered or dropped. This task gives each feature area a workload profile and a random stream of its own, recounts every pinned number once, and writes down how the next builder adds an area.

## Step 1: The requirements, and the design before it is built

**Author:** workload-profiles builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Task: give each feature area a workload profile and a random stream of its own. Design it, build it, and recount everything. The goals, in order: 1. Adding a feature must not dilute or shift the others. A sequence draws its profile from its seed. A profile weights one feature area heavily [...] over a base of the statements that area needs to be meaningful, and there is a mixed profile where areas meet, because faults live at the meeting points too. Each area draws from a random stream of its own, derived from the seed and the area's name, arguments included, so that a change in one area's generator cannot change what another area draws. [...] 2. Every tallied kind is required somewhere with margin. Least numbers are set per profile, each at about a third of what the profile reaches in the test's run and never at a number that rounds to zero at the test's seed count [...] 3. Every planted bug is found with margin, by the property that should find it. [...] The simulation's workloads (default, idle, hammer) and the profiles must be one coherent device, not two. 4. No check is weakened. [...] The total number of generated statements compared with SQLite per test run does not go down. 5. CI's time stays in hand. Each package's test run stays within about its present time plus a third [...] 6. The next builder can add an area in an afternoon's reading. [...] Slice 13 (`order by` and `limit`) is the first user: its statement pass is on `main` with a generator option `OrderBy` that is off; do not turn it on, but check on paper that adding it as an area needs no change to any other area's pins."

**Interpretation:** Rebuild `stmttest.Sequence` so that what it writes is a function of a seed alone, through a profile that the seed names and through random streams by area; make every generated test run every profile and require what each reaches; bind each planted bug to the profile that exercises what it breaks; recount every pin once; and document the steps to add an area.

**Inferred intent:** A slice that adds a feature costs its own time and its own pins, and nothing of the margin of the slices before it, so that the trust in the generated checks grows with the roadmap and does not thin.

### What I did

Read `AGENTS.md`, `/internal/sql/stmt/stmttest/sequence.go` and `stmttest.go`, the users of the sequence (`differentialFind`, `txDifferential`, `workload.do`, `generated` of the driver check, `fuzzSQL`), `TestDifferential` with `reachedDifferential`, `TestConn_differential`, `TestSimulation` with its three maps, `TestSimulation_plantedBugs`, `/internal/engine/bugs.go`, `/internal/testshard`, and the Makefile's and the workflows' test jobs. A read-only sub-agent went through the six diaries and `docs/todo/slice-08.md` to `slice-13.md` and listed every least number that was lowered, moved, or dropped, the 14 of commit 005368d, and the shares of the planted bugs over time. Started a run of the packages on `origin/main` for the times before the change, with logs in `/Volumes/990/s4ql/workload-profiles/base/`.

The design, before any code:

- **A seed names its profile.** `stmttest.NewSequence(seed)` takes a seed, not a `Rand`. The profiles are a list with a fixed slot each; the profile of a seed is the one at the slot `seed % Slots`, and `Profile.Seed(k)` is the k-th seed of a profile. A test runs each profile on its own seeds and keeps a tally for each, so a profile that is added runs on seeds that no other profile had, and the others run as before. One number still replays a run: `-seed N` and `-replay N` need no second flag.
- **The profiles**: `mixed`, in which every area is on with the odds of the sequence of today; and one that weights an area over the base that it needs: `tables` (types, several columns, NULL, the faults of an insert, whole reals for integers), `where` (selects with WHERE on keys and ranges), `changes` (update and delete), `indexes` (create and drop index, WHERE that an index serves), `exists` (IF NOT EXISTS and IF EXISTS on the four statements), `defaults`, `valuefirst`, `transactions` (the harness's begins, commits, and roll backs), and `arguments` (parameters, the arguments around them, and the texts of `Others`). A profile is a set of numbers: the weights of the kinds of statement and the odds of each area, 0 for an area that is off. `Indexed()` and `Mistyped()` become numbers of a profile.
- **A stream by area, and by statement.** Each area draws from a PCG seeded by the seed, the name of the area, and the number of the statement in the sequence, and the shape of a table from the seed, the area, and the name of the table. So a draw more in one statement of one area moves nothing in another statement or another area, and the shape of a table does not depend on which statement names it first. The areas: `names`, `plan` (which statement, on which table), `tables`, `inserts`, `selects`, `changes`, `indexes`, `exists`, `defaults`, `valuefirst`, `arguments`, `others`, and `text` (gaps, case, the spelling of literals). `stmttest.Generator` keeps its one `Rand` for the statement pass's own tests, and gets streams that are not exported for the name of the table, the clause of slice 10, the parameter, and the value on the left, which are its `Rand` unless a `Sequence` sets them. A harness draws what is its own (transactions, calls, restarts) from `Sequence.Stream(name)`, not from the statements' numbers.
- **An area that is off draws nothing.** A test runs each profile with a stream that panics in the place of every area that the profile has off, and a test pins a digest of the first statements of each profile, so a change in an area shows in the digests of the profiles that have it on and in no other.
- **Least numbers in a file that the test rewrites.** Each generated test keeps `testdata/reached/<test>.tsv`: profile, kind, what the run reached, and the least number, a third of it. With `S4QL_RECOUNT=1` the test writes the file from its run; without it, it checks the file. A kind that the run reaches fewer than three times in a profile has no row there, so no least number rounds to zero, and the recount fails if a kind of the old file has a row in no profile, so a kind leaves only when a builder deletes it by hand and says where else it is pinned. The exact pins, such as the texts excused from the comparison, are rows of the same file. The file is seeded with every kind of the maps of today and the kinds that the slices dropped.
- **The simulation**: a workload is an environment (`default`, `idle`, `hammer`: instances, callers, faults, crashes, idlers) and, by the seed, a profile. Each environment runs cells of a profile and a number of seeds; its least numbers are over all its cells, so a cell that is added only adds. Each planted bug names its environment and the profile that exercises what it breaks. The odds of a transaction are a number of the profile, which the simulation, `TestConn_differential`, and the driver check read.

### Why

Three things made the pins move: the draws of a new feature shifted every later draw, a new statement took turns of the others, and all of it ran in one tally. Streams by area and by statement remove the first, profiles on seeds of their own the second and the third. The least numbers are more than a thousand, in several tests, and after this change by profile; by hand they cannot be recounted without errors, which is how 14 of them came to check nothing, so the recount is a command and its result a diff that a reviewer reads.

### What worked

The sub-agent's list of the lowered and dropped kinds, with the exact strings, in one pass.

### What didn't work

The harness refused a compound shell command that created the scratch directory and wrote a script in one line: "this command is too complex to verify that it stays inside the worktree". Plain single commands and the Write tool do it.

### What I learned

- The sequence of the simulation draws from the scheduler's random numbers (`hostRand{world}`), so every jitter of the scheduler shifts the statements and every statement the schedule. With streams from the seed, the statements of a seed are the same whatever the schedule does; only which caller runs which statement is the scheduler's.
- The driver check draws its calls and transactions (`pick`, `pickLevel`, `withNils`) from the sequence's own `Rand`.
- `make test` runs with `-short`, under which the generated tests run a tenth and check no least number; the full runs are CI's job `test-sql` and the driver check's module.
- The core's simulation does not use `stmttest.Sequence`: it has workloads and integer minimums of its own (`/internal/core/workloads_test.go`).

### What was tricky

Nothing built yet. The open risk is the mixed profile: an area that joins it changes its statements with the clause or the kind of that area, and so its tallies; the streams by statement keep every other statement of it the same text.

### What warrants review

The design above, before the code: the slots, the streams by statement, and the file of least numbers.

### Future work

- A counter of the statements compared with SQLite, on the code of today, for the number before.
- The mechanism in `stmttest`, then each test, then the recounts.

## Step 2: The mechanism in `stmttest`, and the engine's tests on profiles

**Author:** workload-profiles builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "1. Adding a feature must not dilute or shift the others. [...] 2. Every tallied kind is required somewhere with margin. Least numbers are set per profile, each at about a third of what the profile reaches in the test's run and never at a number that rounds to zero at the test's seed count [...] Go through every tally that exists and every one that the diaries and todo files above record as lowered, moved to a sibling, or dropped [...] and restore each in the profile where it belongs. [...] 3. Every planted bug is found with margin, by the property that should find it. [...] Keep the commit history readable: the mechanism first, then the recounts per test."

**Interpretation:** Build the profiles, the streams, and the file of counts; run `TestDifferential`, `TestConn_differential`, `TestSimulation`, and the planted bugs on profiles; and recount each.

**Inferred intent:** The pins of the engine hold what they held before and what the slices had dropped, each in the profile that weighs it.

### What I did

- `687d2e3`: a counter of the calls that run in SQLite, on the sequence of before: 12,108 of 14,400 in `TestDifferential`, 2,998 of 3,600 in `TestConn_differential`, 8,487 of 12,000 in `TestGenerated` of the driver check.
- `f97c285`, the mechanism. `/internal/sql/stmt/stmttest/profile.go`: `Profile`, the ten profiles, `Slots`, `ProfileOf`, `Profile.Seed`, the areas, `stepStream` (seeded anew for each statement), `newStream` (for the shape of a table), and `offStream`, which panics. `/internal/sql/stmt/stmttest/sequence.go`: `NewSequence(seed)`, every hook on the stream of its area, `Sequence.Stream` for what a test draws for itself, `Sequence.aim`, `Sequence.fault`, and the indexes that the sequence keeps (`sequenceIndex`). `/internal/sql/stmt/stmttest/stmttest.go`: the generator's four streams that are not exported, which are its one `Rand` for the tests of the pass. `/internal/sql/stmt/stmttest/reached.go`: `Reached`, with the kinds that a test requires in a `.kinds` file and the counts in a `.tsv` file that `S4QL_RECOUNT=1` writes. `/internal/sql/stmt/stmttest/profile_internal_test.go`: the slots, the streams, the same sequence twice, a digest of each profile, the areas that are off, and the same statements with other numbers for the clause of slice 10 and for the value on the left.
- `e58af31`, `1f723e8`, `b35c934`, `4d00d86`: the counts of `TestDifferential` (600 sequences, 25,642 calls compared), `TestConn_differential` (280 sequences, 15,173 calls compared), and `TestSimulation` (cells of profiles in each environment, 166 seeds), and the planted bugs of `TestDifferential_plantedBugs` on their profiles (21 and 15 of 30, where 11 and 6 of 60).
- Each planted bug of `TestSimulation_plantedBugs` names the profile that weighs what it breaks. The default run finds each twice by its own violation within 15 seeds at the most, where `listOrder` needed 83, and takes 7.2 s, where it took 13.8 s.
- `TestSimulation` of the core fails for a minimum below 1.
- A job `generated-engine` in `/.github/workflows/nightly.yml` runs `TestDifferential` with `-sequences 400` and `TestSimulation` with 100 seeds of each cell.

### Why

A focused profile at 40 sequences has fewer statements of its area than 300 mixed sequences had, so the counts of rare kinds do not rise by focus alone. What raised them was to make the statements do their work: of the statements of the one sequence, about half met a table that was not there. `Sequence.aim` makes three statements in four of a focused profile name a table that the sequence created, and `Sequence.fault` gives the statements beside the focus a quarter of their faults.

### What worked

- The first run of every profile through `TestDifferential` found no difference from SQLite, and neither did any later run, the simulation, or the transactions: 25,642 + 15,173 compared calls and 166 seeds.
- `TestSimulation` reached every kind of its three old maps and the 14 dropped ones at the first recount.
- The recount as a command: each change of a weight was one run and a list of the kinds that no profile reaches three times, with the count of each profile.

### What didn't work

- The first form of the file of counts held every kind that a profile reached three times, and every kind of the file had to stay. After the first recount the next change of the generator failed for `"createindex 1/1: there is already a table named FolLoWiNG"` and other chance kinds with a name in them. The kinds that a test requires are a file of their own now, which a builder writes, and only they and the exact kinds have rows.
- A test that claimed "a text of `Others` in the place of a statement changes no other statement" failed: the statement that the text replaced would have stored values that later conditions draw from. The numbers that another statement draws are the same; its text depends on what the statements before it did. The test keeps the two areas for which the claim holds, the clause of slice 10 and the value on the left.
- `run.open` of the simulation divided by the number of names of indexes: `panic: runtime error: integer divide by zero` in a profile without indexes. Such an engine reads every row now.
- `TestSequence_createIndex` expected three kept indexes of a table that the sequence had not created: `Expected "3", but got "0"`. The test creates the table in the model first.
- 16 sequences of the profile of the defaults in transactions reached "insert that leaves out the key with a default ok" twice, and the same 16 with other seeds 8 times: the count of a kind that hangs on the shape of one or two tables varies much between few sequences. The test runs 40 of that profile.

### What I learned

- An index is chosen by the rules in 8 of 100 WHERE that the sequence aims at an index, since a WHERE that pins the key reads by the key. `Profile.Columns` of 2 and more create indexes without the key in the profiles of the indexes raise it.
- With `Others` and `Around` off, nearly every statement of a focused profile runs in SQLite: 3,786 of 3,840 in the profile of the clauses.

### What was tricky

- The kinds that a profile reaches three to five times move with every change of a weight. Seven rounds went into them: more sequences for five profiles, `Profile.Columns`, a WHERE with a bound on the first column of an index and no pin, the key pinned after the columns of an index, a drop table that names an index, the two zeros in one real of four of an update, and a text and a blob of a byte more than the length limit among the edges.
- The harness refuses a shell command that it finds "too complex to verify that it stays inside the worktree", also one with a long here-doc for Python. Scripts written with the Write tool and run by a plain command pass.

### What warrants review

- `/internal/sql/stmt/stmttest/profile.go` and `Sequence.aim`, `Sequence.fault` in `/internal/sql/stmt/stmttest/sequence.go`: the model of the tables and indexes that the sequence keeps is what makes the focused profiles work, and it is new.
- `/internal/sql/stmt/stmttest/reached.go`: a least number is a third, a required kind needs a row in a run, and nothing else has a row.
- The rows of `/internal/engine/testdata/reached/*.tsv` with a least number of 1: they hold, and they are thin.

### Future work

- The driver check, by a builder of its own; the shares of the planted bugs over 200 seeds; the documents.

## Step 3: The driver check, the shares of the planted bugs, the documents, the review, and the checks

**Author:** workload-profiles builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "3. Every planted bug is found with margin, by the property that should find it. For each of the engine's planted bugs [...] say which profile or simulation workload finds it and in what share of seeds [...] none is found without its bug. [...] 5. CI's time stays in hand. Each package's test run stays within about its present time plus a third (measure before and after, with and without `-race` [...]) 6. The next builder can add an area in an afternoon's reading. [...] Once the implementation is done, self-review before handing back to the lead. [...] Finish: `make test`, `make lint`, `make check-browser` pass"

**Interpretation:** Bring the driver check onto the profiles, count the planted bugs over 200 seeds, write the documents, have the branch reviewed, fix what the review finds, and measure the times against the code before.

**Inferred intent:** The branch is what an independent reviewer can read and merge, with its weak points named.

### What I did

- A builder of its own moved `TestGenerated` of the driver check onto the profiles (`4ad7e43`): 362 sequences of 48 texts, 12,862 calls compared with SQLite where 8,487 of 250 sequences were, the counts in `/internal/sqlite/crosscheck/drivercheck/testdata/reached/`, and what the harness draws for itself on `Sequence.Stream("calls")`.
- `S4QL_ENGINE_SIM_SEEDS=200` of `TestSimulation_plantedBugs`, 668 s: each bug on its profile, none found without its bug. Found, and by its own violation: `skipUnique` 180 (178), `ackUnknown` 39 on the mixed profile and 48 on that of the tables, where it runs now, `checkOutside` 79, `keepOpen` 196, `keepRead` 149 (78), `annotateNext` 154, `keepFailed` 198, `readNewest` 194 (173), `rollbackKeeps` 109, `convertReal` 119 (114), `listOrder` 127 (35), `zeroLeftOut` 64 (60), `openBound` 134 (134), `pinByPlace` 43 (38) in the default environment and 81 (66) in the hammer one, where it runs now, `changeNothing` 192 (178), `keepOldKey` 90 (38), `replaceExisting` 183 (106), `nullDefault` 137 (137), `unturnedBound` 130 (92).
- The commands of the nightly job, run here before the review: `TestDifferential -sequences 400`, 6,000 sequences, in 268 s, and `TestSimulation` with 100 seeds of each of its 18 cells of then in 106 s. No difference from SQLite, no violation.
- Two writers wrote `/internal/sql/stmt/stmttest/README.md`, brought `/internal/engine/README.md`, `/internal/sql/stmt/README.md`, and the comments beside the planted bugs up to date, and a third corrected them after the review. `/docs/todo/generated-workload.md` holds the decisions and what is open, and the open item on `listOrder` left `/docs/todo/slice-12.md`.
- Two reviewers read the branch (`aff5aeb` has the fixes): see below.
- `stmttest.pcg` (`ed5910e`), for the lint of the simulation.
- Merged `origin/main`. `make test`, `make lint`, `make check-browser`, and `make fuzz-shards FUZZGROUP=c FUZZSHARDS=4` pass.

### Why

The review asked what the first recount of the simulation had lost. Its counts were by environment, and a third of a sum over cells of which only one carries an area is less than the least number of the one sequence before, by the seed. The counts are by cell now, and by environment for the kinds of the store and the lease that a cell of six seeds does not reach.

### What worked

- Both reviewers ran scripts over the files of counts against the maps of `main`, and found the same two weakenings in `TestSimulation`. Neither could break the streams: no draw from a wrong stream, no map order that reaches a statement, and the generator of the tests of the pass draws as before.
- The generator written out in the package gives the numbers of `math/rand/v2` exactly, so no digest and no count moved.
- Times, back to back with a copy of `687d2e3` on the same machine, twice: the engine's package 68 s and 67 s before, 81 s and 81 s after; with the race detector 153 s before and 131 s after; the five generated tests 30 s before and 43.5 s after (`TestDifferential` 6.2 to 17.0, `TestConn_differential` 1.2 to 6.4, `TestSimulation` 6.5 to 10.4, `TestSimulation_plantedBugs` 13.3 to 6.7, `TestDifferential_plantedBugs` 2.7 to 2.9).

### What didn't work

- The first recount of `TestSimulation` gave the integrity checks of the caches a least number of a third, where every seed had to run each: 20 of 60. They are exact rows now.
- 18 pairs of an environment and a kind reached less than the least number of `main` for the seeds of the environment, among them the statements of IF NOT EXISTS with SQLite's error in the hammer environment, 39 where 78 were required. The default and the hammer environment run every profile now, and the idle one five.
- `Reached.Check` passed for a run without a row, for a file of kinds without a kind, and for a least number lowered by hand (`run 900 1 kind` with a count of 1). It fails for each now, with table tests.
- `go test -short -run '^TestDifferential$' -seed 5 -sequences 9` ran no sequence and passed, and `-seed 0` could not be told from no flag. `-run '^TestSimulation$/^hammer$'` failed for the environments that did not run.
- `make lint` failed: `internal/sql/stmt/stmttest/profile.go:5:2: math/rand/v2 draws randomness outside the host; use host.Host.Uint64`. The lint is right about the import and cannot see that the streams are seeded from the seed alone; the package has the generator of its own now, and the lint no exception.
- I wrote 660 sequences for `TestDifferential` in two commit messages and in the documents; it runs 600, five profiles of 80 and five of 40. The documents are corrected, the messages of `e58af31` and `f97c285` are not.
- The driver check is over the time: 7.7 s before and 10.9 s after, and with the race detector 24.6 s and 34.7 s, both about 41 in 100 more, where a third was the aim. It has no slow run, since CI's job runs it with the race detector and checks its counts there. Fewer sequences would take kinds below three.
- The harness refused `git -C` into a sub-agent's worktree. I removed each merged worktree with `git worktree remove` without `--force`, which refuses a tree with changes, and deleted its branches with `git branch -d`.

### What I learned

- The driver builder's new seeds found a fault in the driver check itself, not in s4ql: `pair.typeRefusal` looked a table up inside the transaction for a query that runs beside it, so the rule expected the name of the column as the transaction had made it anew. Seed 1958 of the profile of the defaults; a subtest of `TestSequences` pins it.
- A least number as a third of a sum hides which part of the sum carries the kind. Counts by cell say it.

### What was tricky

- Two kinds of the driver check stay required once over all profiles, and not by a profile: "value on the left of != finds rows" and "WHERE with a column that the table does not have under IS NULL or IS NOT NULL, alone no such column". No profile reaches them three times in the driver check's 362 sequences; the engine's `TestDifferential` requires both by profile.
- One reviewer found that for 39 kinds of `TestDifferential` the least numbers of the profiles add up to less than the one least number of `main`, such as "create index no such table", 213 before and 110 now, since the focused profiles aim at tables that are there. The run still meets every one of the 382 least numbers of `main` in its totals. I did not add a row for the total with the numbers of `main`: the least numbers are by profile, a third each.

### What warrants review

- `/internal/engine/simulation_test.go`, `simEnvironments` and `TestSimulation`: the cells, the two kinds of run, and what a part of the seeds checks.
- `/internal/sql/stmt/stmttest/reached.go` with `/internal/sql/stmt/stmttest/reached_test.go`.
- `stmttest.pcg` in `/internal/sql/stmt/stmttest/profile.go`, against `TestPCG`.
- The driver check's time, and its two kinds required once.
- `/.github/workflows/nightly.yml`: the pull request changes a workflow file.

### Future work

- The mutation testing of slices 10 to 12, which waited for this.
- Slice 13 as the first new area: the steps are in the README of `stmttest`.
