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
