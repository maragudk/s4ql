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
