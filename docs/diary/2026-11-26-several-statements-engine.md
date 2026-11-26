# Diary: the engine's differential tests of several statements in one `Exec`

The generated texts of several statements reached `engine.Conn.ExecAll` only through the driver's generated test: the engine's `TestDifferential` and `TestConn_differential` left the profile `several` out and ran the texts of several statements of `mixed` through `Conn.Exec`, which refuses them for their second statement. This step judges `ExecAll` in the engine's own differential tests against running the statements one after another, with SQLite as the oracle.

## Step 1: the walk, the oracle's split, the generator's fix, the recounts, and the planted faults

**Author:** builder (several-statements-engine)

### Prompt Context

**Verbatim prompt:** "Your ONE task: **the engine's differential tests for several statements in one `Exec`**. The simulation's planted bugs and mutation testing follow as steps of their own; do not start them." The brief then names four parts: `TestDifferential` and `TestConn_differential` run the profile `several` and the texts of several statements of `mixed` through `Conn.ExecAll`, judging which statements ran, the result, the error with its offset in the whole text, the tables, the commits, and the annotations, reusing the rules of a statement alone; reach kinds with floors at a third of their totals; a fix to `Sequence.several` so that the excuse of a last statement with RETURNING is reached again; and seven planted faults, each failing a judging check.
**Interpretation:** route every text of several statements through Exec to a new judge, `pair.several`, whose oracle is the statements run one after another, each judged by `pair.step`.
**Inferred intent:** the engine's loop is compared with SQLite on generated texts by the engine's own tests, which see the commits and annotations that the driver cannot, before the simulation and mutation testing follow.

### What I did

- `/internal/engine/several_test.go`: `pair.several`. The instance of the pair runs each statement alone through `pair.step` (with `pair.walking` set, so `pair.bothWays` does nothing), with as many of the arguments as it has parameters, up to the first that fails; then the other instance runs the whole text through `ExecAll`, with the path that the rules choose. The error (kind, message, offset, moved by the start of the part by `shifted`), the result (`instance.sets` counts the results that set the count of changes), the tables (`pair.tablesAfter`, by a select of each table in a transaction), and the new commits and annotations of both stores must agree. `sqliteParts` reads the statements on `sqlite.Tokenize` and `sqlite.Complete`; `spaceRule` and `trimmedEnd` are the oracle's rule of spaces on SQLite's tokens; the rule of more arguments than parameters writes its message. `pair.reachDependent`, `pair.reachSeveral`, `pair.sameLog`, and the tests `TestSqliteParts` and `TestSpaceRule`.
- `/internal/engine/differential_test.go`: `pair.step` calls `pair.several` for a call with `Statements` through Exec; `pair.bothWays` returns at once while walking; `sequencesOf` runs twice as many sequences of `several`; `messageKind` knows "unrecognized token"; `instance.sets`.
- `/internal/engine/tx_test.go`: `TestConn_differential` runs `several` four times as many; `end()` counts a commit or a roll back of a transaction in which a text failed, and compares the whole logs of both instances.
- `/internal/sql/stmt/stmttest/several.go`: `severalReturning`, drawn for the last three statements of a text of three or more one time in four in a profile with RETURNING. Digests of `mixed` and `several` in `/internal/sql/stmt/stmttest/profile_internal_test.go`.
- Recounts with `S4QL_RECOUNT=1`: `/internal/sqlite/crosscheck/drivercheck/testdata/reached/TestGenerated.tsv` (floor of "last statement with RETURNING that ran" raised to 40 in its `.kinds`), and the engine's `TestDifferential`, `TestConn_differential`, and `TestSimulation` `.tsv` files; 28 and 35 new kinds in the engine's two `.kinds` files.
- Docs: `/docs/todo/several-statements.md` (section "The engine's differential tests"), `/internal/engine/README.md`, `/internal/sql/stmt/stmttest/README.md`.

### Why

Running the statements alone on one instance reuses every rule that judges a statement alone, against SQLite, and gives commits of the same statements alone to compare with `ExecAll`'s. The oracle's split uses SQLite's tokenizer so that it does not lean on `stmt.Split`, the code under test.

### What worked

The first run under `-short` found only an oracle bug (below); the full runs found no difference between `ExecAll` and the statements one after another. Every planted fault failed a judging check within seconds: one commit for the whole text ("ExecAll writes the commits" / "annotates its commits with"), the commit of a failed statement written anyway ("after it, the other instance holds"), the annotation of the whole text ("ExecAll annotates its commits with"), the statement after a failed one run ("after it, the other instance holds"), the second statement's arguments one too late ("ExecAll gives"), a failure in a write transaction after the first statement rolling back ("after it, in the transaction, select * from \"cast\" gives"), and a select skipped ("ExecAll gives"). The driver check reaches the RETURNING excuse 44 times again.

### What didn't work

- The first oracle shifted an offset of -1, SQLite's "no offset": `1/1: no such table: OUTER ("no such table: OUTER" at offset -1) ... and the statements one after another ... at offset 27`. `shifted` now moves only offsets from 0 on.
- `TestConn_differential` failed on a statement with arithmetic: `evaluation error ("s4ql: division by zero in / (at offset 178)") ... and the statements one after another evaluation error ("s4ql: integer overflow in - (at offset 185)")`. The two instances draw their directions from seeds of their own, so another row failed first; the other instance's error is now justified by `pair.justify`.
- Adding the excuse to the driver's `.kinds` as a floor failed: `testdata/reached/TestGenerated.kinds holds the kind "excused: mattn's result of the statement before a last statement with RETURNING, ..." which is exact`. It is pinned exactly in the `.tsv` already; the line was removed.
- The recount failed on `the kind "other 1/1: unrecognized token: \"'a''b);\n..." of the run "several" has a tab or a line break`: a part that a string without its end runs to the end of the text. The engine's `messageKind` now knows "unrecognized token", as the driver's does.
- The `.kinds` floors need the `.tsv` rows, which only a recount with the kinds listed writes, so it took a second recount.
- A plant of "a failure in a transaction undoes the statements of the text before it" with one savepoint around the text broke the core: `the storage core refused a call that the engine had checked: the transaction can only be rolled back`; the core keeps one savepoint at a time. The plant used a roll back and a new begin instead.
- The worktree harness refused shell commands with heredocs ("this command is too complex to verify"); I used Edit and scripts in the scratchpad.

### What I learned

The engine's tests see what the driver check cannot: the commits and their annotations, which `pair.several` compares with those of the statements alone. Two of the seven faults (the one commit for a text, the whole text in the annotation) change nothing that a table or an error shows.

### What was tricky

The other instance must not run the walk's statements, so `pair.bothWays`, which runs every call on it, does nothing during the walk, and `pair.several` sets its path back to the rules' before `ExecAll`. The offsets of s4ql's own errors are in their messages, `(at offset N)`, and move with the part.

### What warrants review

- `pair.several`: the order of the rules before any statement runs, against `engine.CheckAll`, and the arguments that each statement of the walk takes, also for a text of one statement and a statement that the rules refuse.
- `spaceRule` on SQLite's tokens against the driver check's on s4ql's, and `sqliteParts` against `stmt.Split`'s parts.
- The justification of an evaluation error of another row, whose count varies with the other instance's random seed.

### Future work

The simulation's planted bugs for the loop, mutation testing of `stmt.Split` and `ExecAll`, and a fuzz target of `Split`.

## Step 2: the self-review

**Author:** builder (several-statements-engine)

### Prompt Context

**Verbatim prompt:** (the brief of step 1) "`make test` and the lint target pass; the whole engine package and the driver check pass without `-short`; new tests pass with `-count=20` and `-race`."
**Interpretation:** review the diff with the code-review skill, fix what matters, and run the checks of the brief.
**Inferred intent:** the branch is ready for the lead's review.

### What I did

Ran `/code-review medium main...HEAD`. It reported two important findings and two nits. Fixed both important ones in `/internal/engine/several_test.go`: in a transaction `pair.tablesAfter` also reads the tables that a create table or a drop table of the text names, so a table that only the other instance has fails the call; and `pair.several` gives the other instance an observer of subqueries that forces nothing before `ExecAll`, since the one that the last `pair.bothWays` left could force `p.force`. Left the two nits, recorded in `/docs/todo/several-statements.md`: `txDifferential`'s bookkeeping counts nothing of a text of several statements, and a failure that depends on the other instance's directions replays with the `-directions` seed that the failing test logs.

### Why

The first finding is real: a statement that runs after a failed one and creates a table, in a transaction that is then rolled back, would have shown nowhere. The second is latent, since no profile with several statements writes subqueries now, but costs one line.

### What worked

`make lint` reported 0 issues; `make test` passed; `TestSqliteParts` and `TestSpaceRule` passed with `-race -count 20`; `TestDifferential` and `TestConn_differential` passed under `-race`.

### What didn't work

Nothing failed in this step.

### What I learned

The other instance keeps the path and the observer of subqueries that the last `pair.bothWays` set; anything that runs on it outside `bothWays` must set both.

### What was tricky

Nothing beyond the findings.

### What warrants review

The names that `pair.tablesAfter` reads in a transaction: those of SQLite's tables and those that the text's create tables and drop tables name.

### Future work

As in step 1.

## Step 3: the fixes after the independent review, and the landing

**Author:** builder (several-statements-engine, landing)

### Prompt Context

**Verbatim prompt:** "You finish and land a reviewed branch in s4ql (`maragu.dev/s4ql`, repository `maragudk/s4ql`): `several-statements-engine`, the engine's differential tests for several statements in one `Exec`. The task is small: three fixes from an independent review, the status texts, and the landing. Do nothing beyond it." The fixes: nothing checks how many times a statement of a text runs when a second run changes nothing; the driver check's excuse for a last statement with `returning` does not check mattn's number; `Supported` is judged for a whole text.
**Interpretation:** tighten `pair.several` to compare the events and the transaction's count of statements, pin mattn's number for the RETURNING excuse, and pass each statement of the walk its own support; then the roadmap lines, the merge of main, the checks, and the pull request.
**Inferred intent:** the engine's differential tests catch a loop that runs a statement twice, and every excuse covers exactly its known difference, before the branch lands.

### What I did

- `/internal/engine/several_test.go`: `recorder`, a logger of the events of statements that is enabled only while on; `pair.several` records the instance's events during the walk and the other instance's during `ExecAll`, and compares them (kind, outcome, transaction kind) and the change of `engine.Conn.TxStatements` on each; a text refused before any statement runs must log one event without a kind (`outcomeOf`, `pair.txKind`). The walk passes `c.Supports[j]` to each statement.
- `/internal/engine/differential_test.go`: `instance.events`, attached by `instance.directed` to every engine whose options have no logger.
- `/internal/engine/tx_export_test.go`: `Conn.TxStatements`.
- `/internal/sql/stmt/stmttest/sequence.go`, `several.go`, `several_internal_test.go`: `Call.Supports`, one bool per joined text; `Each` is now their conjunction, with the same draws.
- `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`: `severalBeforeLast`, and the RETURNING excuse requires mattn's number to be it; a `TestSeveral` case where no statement before the last sets a count.
- Merged main (the split's differential test and fuzz target, a site change); resolved the conflict in `/docs/todo/several-statements.md`; updated `/docs/roadmap.md` (the area "The driver" and the table "At a glance").

### Why

The review's planted fault (each select run twice in a read-only transaction) changes no table, error, result, or commit; only the events and the transaction's count show it. The excuse accepted any number of mattn's, so a regression in either side's count for such texts could hide behind it.

### What worked

The planted fault fails `TestConn_differential` with `ExecAll logs the events of statements ["select" ok "read_only" "select" ok "read_only" "delete" sqlite_error "read_only"] and the statements one after another ["select" ok "read_only" "delete" sqlite_error "read_only"]`. An extra `c.tx.statements++` in `execPart` fails it under `-short` with `ExecAll counts 2 statements in the transaction, and the statements one after another 1`. A probe of mattn (a temporary test, deleted) showed its result is SQLite's count before the last statement, also from an earlier call (`create table u ...; insert into t values (6, 6) returning k` gave 1, the count of the call before), and that `savepoint`, `rollback to`, and `release` keep SQLite's count. The planted expectation of one more than that number fails `TestGenerated` in 1.4 s (`mattn's result has 0 rows affected, and SQLite's count of changes after the text is 1`). No `.kinds` or `.tsv` count changed.

### What didn't work

- `go test ./internal/sqlite/crosscheck/drivercheck` from the root failed with `main module (maragu.dev/s4ql) does not contain package maragu.dev/s4ql/internal/sqlite/crosscheck/drivercheck`; the crosscheck is a module of its own, so it runs from `/internal/sqlite/crosscheck`.
- The worktree harness refused a command with two here-docs ("this command is too complex to verify"); I split it.
- A first probe of savepoints was not decisive, as the count was 1 before and after; a delete of 3 rows in the savepoint showed it.

### What I learned

A second run that reuses a statement's report logs nothing and counts nothing, since the report ends once; a real double run makes a new report, which is what both checks see. mattn's result for a last statement with RETURNING that gives a row is the connection's count before it, not "the count of the statement before" in the text.

### What was tricky

Aligning the walk's parts with the generator's texts for `Supports`: they are one to one only up to the first text that s4ql does not support, where the walk always stops, so a walk past them fails the test instead of guessing.

### What warrants review

- The recorder on every instance without a logger of its own: it is disabled outside `pair.several`, so the engine builds no record then.
- `severalBeforeLast` runs the statements before the last a second time in mattn, in a savepoint; a failure there returns "" so that the whole Exec reports it.

### Future work

The driver check still passes `Each` for a whole text to the statements of its walk (`supported = next.Each`); `Supports` would serve it too. The simulation's planted bugs and mutation testing stay open.
