# Diary: the engine's simulation of several statements in one `Exec`, with planted bugs

The engine's simulation (`/internal/engine/simulation_test.go`) left the profile `several` out, and ran the texts of several statements of `mixed` through `engine.Conn.Exec`, which refuses them for their second statement. This task runs them through `engine.Conn.ExecAll`, in the default and the hammer environment, plants bugs in the loop of `ExecAll` and its commit path, and shows that the simulation finds each.

## Step 1: texts of several statements in the simulation, and eight planted bugs

**Author:** several-statements-bugs

### Prompt Context

**Verbatim prompt:** "Your ONE task: **the engine's simulation for several statements in one `Exec`, with planted bugs**. Mutation testing follows as a step of its own; do not start it. … 1. **The simulation runs the profile `several`** (and the texts of several statements that `mixed` writes) through `ExecAll`, in its default and its hammer environment, with crashes between and inside the statements of a text. What must hold: the statements of a text are in the log as running them one after another would leave them (outside a transaction one commit per statement that changed something, in order; a crash between two statements leaves exactly the earlier ones; the failed statement and those after it leave nothing), the annotations replay to the replayed rows, and callers were told what the log holds. Give `several` cells and take it out of the list of profiles without one. 2. **Planted bugs** in the loop and its commit path … at least these six … 3. **Accepted symptoms.** … 4. **A small fix in the driver check** … 5. **Night sizes before you hand in** …"
**Interpretation:** model each statement of a text as an attempt of its own, so that every check of a statement alone applies to it, learn from the engine's events which statements ran, add the checks that only a text has, plant the six bugs and others, and derive each bug's symptoms from 1,000 seeds.
**Inferred intent:** the simulation, with its crashes and faults of the store, judges `ExecAll` as it judges `Exec`, and a planted fault of the loop cannot pass it.

### What I did

- `run.statement` sends a call with `Statements` that is not a query to `run.several`. It reads the text's statements with the oracle's own `sqliteParts` (on SQLite's tokens), and the refusals before any statement with `refusedBefore`, which I took out of `pair.several` in `/internal/engine/several_test.go` so that both use one rule. Each statement becomes an attempt with the comment of its ID, its own arguments, and its part of the text (with the spaces, comments, and empty statements before it) as its text; a text that the rules refuse is one attempt. The text with the IDs is sent to `ExecAll` with a `severalKey` in its context.
- The handler of the engine's events gives each event of a statement in a call to the attempt in its place (`run.logged`): its position, access, index, `nothing`, the values of its subqueries, and, if the event is ok and no earlier one failed, `succeeded`, its count of changes from `statement.rows`, and its acknowledgement for a write outside a transaction. So a statement whose event says ok is acknowledged even if the call never returns because its instance crashed: its commit has returned, as for a statement alone, whose event is logged after its commit.
- `run.severalEnded` checks the events against the run of a text (no more events than statements, each event of a statement with an ID carries its ID, none after the first that failed, all ok if the call succeeded), the arguments of each statement that succeeded, the result of a call that succeeded against the event of its last insert, update, or delete, and classifies the statement that failed with `run.classify`, the switch of `run.statement` taken out, with the offsets of s4ql's own errors moved into the text of the statement (`unshifted`). A statement with fewer arguments than parameters is checked against ExecAll's rule message. Those after the failed one are `notRun`, a new state.
- `run.verifyTexts`: outside a transaction, a statement of a text is in the log only after every statement before it in the text succeeded, and each in a commit after that of the one before.
- `severalKinds` counts texts by how they ended, refused, crashed after a statement succeeded, with one commit or with a commit for each of several, and the statements that did not run.
- Cells `{stmttest.Several, 10}` in the default environment and `{stmttest.Several, 12}` in the hammer one; the skip of the profile in `TestSimulation` is gone.
- Planted bugs in `/internal/engine/bugs.go`, `/internal/engine/several.go`, `/internal/engine/engine.go`, `/internal/engine/tx.go`: `oneCommit`, `goOn`, `commitFailed`, `commitLater`, `annotateText`, `argsFromStart`, `undoInTx`, `firstResult`. Those that need the text in `Conn.write` get it through a context value (`textKey`), set only while one of them is planted.
- The driver check (`/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`) keeps the `Supports` of a generated call on the pair, and `pair.severalWalk` judges each statement by its own entry.

### Why

Treating each statement as an attempt reuses every check of a statement alone: the log's commits and their parameters, the acknowledged writes, the replay in SQLite, the checks of transactions, of arithmetic, and of types.

### What worked

A probe test of my own (not committed) that ran N seeds of a profile with a bug and printed every violation showed after two fixes that 100 seeds of each environment find nothing without a bug, and gave the first rates of each bug.

### What didn't work

- The first probe failed every seed with `ExecAll logs the event of the statement 207 in the place of the statement 206`. The event of a statement that `stmt.Split` refuses has the rest of the text from that statement on as its text, which holds the IDs of the statements after it; and a refused first statement has the whole text. The check now applies only to an attempt whose text carries an ID.
- Then `ExecAll logs the event of the statement 0 in the place of the statement 64`: an event above Debug (a commit of unknown outcome is Warn) has no text. The check now skips an event without `statement.sql`.
- My first loop that ran the classification over 1,000 seeds, `for x in "hammer none" …; do set -- $x; …`, ran in zsh, which does not split `$x`, so every run was of the default environment without a bug. I wrote the runs out one per line in `/Volumes/990/s4ql/several-statements-bugs/all.sh`.

### What I learned

An event of a statement in the default environment shows a crash between two statements well: of 300 seeds of `several` with `commitLater`, 38 lose a statement whose event was ok when the instance crashed in the commit that the bug deferred.

### What was tricky

The positions of a text's events. A statement that s4ql does not support carries no ID, so its event is placed by its position only.

### What warrants review

`run.logged` and `run.severalEnded` in `/internal/engine/simulation_test.go`, in particular the choice that an ok event acknowledges a statement of a text.

### Future work

See the next steps.

## Step 2: the symptoms of 1,000 seeds, a crash aimed between statements, and the self-review

**Author:** several-statements-bugs

### Prompt Context

**Verbatim prompt:** "3. **Accepted symptoms.** For each bug, derive its list of accepted symptoms from what the bug can cause, by classifying every violation over 1,000 seeds (`S4QL_ENGINE_SIM_SEEDS=1000`, bug by bug, not all bugs in one process), and show that no seed fails with a symptom outside its list and that the rerun without the bug finds nothing. Say for each bug how many of the 1,000 seeds find it, and how many by the bug's own symptom."
**Interpretation:** run each bug alone over the first 1,000 seeds of the profile `several`, record every violation of every seed, and build each bug's list from those, with an explanation of each entry.
**Inferred intent:** no list wider than what the bug causes, and each entry explained.

### What I did

- A probe test, kept out of the repository, ran 1,000 seeds of a bug in one process from a test binary on `/Volumes/990/s4ql/several-statements-bugs/`, one bug after another (`all.sh` there), and printed every violation of each seed. `cover.py` there counts the seeds, those by the bug's own violation, and picks greedily from a list of phrases the fewest that cover every seed; I then read the classes of each bug to see that each entry follows from the bug. The lists and counts are in `/docs/todo/several-statements.md` and in the comments of `TestSimulation_plantedBugs`.
- Without a bug, 1,000 seeds of each environment found nothing.
- `TestSimulation` failed after the first recount: `no run reaches "texts of several statements whose call did not return after a statement succeeded", which is required, three times`. The crasher of the default environment hit a text between its statements in about 1 seed of 40. In the profile `several`, `run.crash` now waits up to ten seconds for an instance that runs a text after one of its statements succeeded, and then as before. The cells reach 4 such texts, and 1,000 seeds 271; with that, `commitLater` is found by 610 of 1,000 seeds (534 before), 296 of them by a statement lost in a crash after it. The other profiles crash as before, so no other count moved.
- The kinds of texts are in `/internal/engine/testdata/reached/TestSimulation.kinds` at a third of their totals, and `TestSimulation.tsv` is recounted. I dropped a kind that counted the same as the state "not run".
- The driver check's fix: a refusal planted in `ExecAll` for the first statement of a text with a later statement that `stmt.Split` refuses failed `TestGenerated` both with the old walk and the new (`s4ql gives exec: exec failed with s4ql: not supported: PLANTED (at offset 24), where the rule gives ""`). The entry of `Supports` only matters where `stmt.Check` refuses the text of a statement, so I found no small fault that only the fix finds; the reason is recorded.
- The self-review (`code-review`, high) found four nits, all fixed: a text that is not valid UTF-8 or holds NUL had no rule (now an explicit violation, as in the differential tests; the generator writes none); `commitFailed` left the write transaction open when its annotation failed (now rolled back); the values of a statement's subqueries reached its attempt only with its event, so a crash after the commit and before the event lost them (the observer now writes them on the statement that runs, the first without an event); and every write looked up the text in the context before the flags of the bugs (the flags come first now).

### Why

A list taken from the bug's real violations, with each entry explained, is a list that a different fault cannot hide in.

### What worked

Reading the classes, not only the cover: they showed that every seed of `argsFromStart` and `commitLater` that a first cover counted outside the own violation had it, cut off by my probe's truncation of a violation at 600 bytes.

### What didn't work

- `for x in "hammer none" "default none" …; do set -- $x; …` in zsh: no word splitting, so ten runs of the default environment without a bug and `out-hammer goOn-.txt` as a file name.
- `python3 cover.py out-hammer-commitFailed.txt …` failed with `UnicodeDecodeError: 'utf-8' codec can't decode byte 0xed in position 5194`, because the probe cut violations inside a character; the scripts read with `errors='replace'`.
- The first version of the aimed crash broke out of its second loop when it found no running node: `if live != nil { break }` after `live` had been set to every live node. A flag `aimed` fixed it.

### What I learned

`undoInTx` is found by its own violation in only half of its seeds: the roll back frees the place of the writer, so another caller's statement commits while the caller's transaction is still open as far as the caller knows, which `run.verifyOverlaps` sees first.

### What was tricky

Telling a real second symptom from a truncated own one, above.

### What warrants review

The aimed crash in `run.crash`, which only the profile `several` takes, and the lists of `oneCommit` and `undoInTx`, the longest.

### Future work

Mutation testing of the loop, as the brief says.
