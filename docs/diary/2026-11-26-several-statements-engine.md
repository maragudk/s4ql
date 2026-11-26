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
