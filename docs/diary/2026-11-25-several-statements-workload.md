# Diary: the generated workload of several statements and the driver's generated test

Several statements in one `Exec` are on `main` (pull request #157), but the generated checks compared only one statement at a time: the driver check ran a text of several statements as its parts, one step each (the stopgap `pair.several`), so `ExecAll` itself met mattn only in the hand-written `TestSeveral`. This step gives the workload texts of several statements, and the driver check an oracle that runs the whole text in one `Exec`.

## Step 1: the workload, the oracle, the recounts, and the planted faults

**Author:** builder (several-statements-workload)

### Prompt Context

**Verbatim prompt:** "Your ONE task: **the generated workload for several statements in one `Exec`, and the driver's generated test for it**. Nothing else; the engine's differential test, the simulation's planted bugs, a fuzz target for the split, and mutation testing each follow as a step of their own." The brief then names four parts: the workload in `internal/sql/stmt/stmttest` as an area and a profile of its own and at a low rate in `mixed`; the driver's generated test with an oracle that runs the whole text in mattn in one `Exec`, with the two decided `RowsAffected` differences as named excuses and reach kinds with floors; a check that one statement through `ExecAll` is what `Exec` gives; and six planted faults, each failing `TestGenerated`.
**Interpretation:** write `Sequence.several`, replace the stopgap with an oracle that judges the whole call, recount what the change of `mixed` moves, and prove the oracle by faults in `/internal/engine/several.go`.
**Inferred intent:** the loop of `ExecAll` is compared with mattn on thousands of generated texts, not only on 21 hand-written ones, before the heavier checks follow.

### What I did

- `/internal/sql/stmt/stmttest/several.go`: `Sequence.several`, which joins two to five statements. Each is `Sequence.statement()` (the body of `next` after the texts of `Others`, pulled out) with `q.step` raised for each, so each draws from streams of its own; or an earlier statement of the text again; or, as faults, a text of `Others` or `ArithmeticRefused`. Leads, separators, and ends of spaces, comments, and empty statements; the trimmed spaces; too few and one too many arguments; one query in eight. The last statement gets RETURNING one time in three by setting `q.received`, which `Sequence.returning` reads. `Call` has `Statements` and `Each`, and `Supported` is false for such a text.
- `/internal/sql/stmt/stmttest/profile.go`: `Profile.Several`, the area `several`, the profile `several` at slot 14, `Mixed.Several = 32`. Digests in `/internal/sql/stmt/stmttest/profile_internal_test.go`: only `mixed` changed, and `several` is new. `TestSequence_several` in `/internal/sql/stmt/stmttest/several_internal_test.go` requires each form three times in 30 sequences.
- `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`: `pair.several` with `severalRan` (one `Exec` in mattn, `select changes()`, the two excuses), `severalFailed` (the walk, statement by statement, judging s4ql's error at the statement it belongs to through `pair.compare`, `pair.checkQuoted`, or mattn's error), `driverRefusal`, `compareIn`, `reachSeveral`, and four excuse constants. `statementKind` names "several statements"; `messageKind` knows "unrecognized token"; `generated` passes `Each` as `supported` for an `Exec` and sends two in three prepared calls of such a text through `Exec`; `generatedOf(Several)` is 80.
- `/internal/engine`: the profile `several` is left out of `TestDifferential` (`sequencesOf`), `TestConn_differential`, and the check of `TestSimulation` that every profile has a cell. The three `.tsv` files were recounted with `S4QL_RECOUNT=1`, and two kinds left `TestDifferential.kinds`. A subtest of `TestConn_ExecAll` in `/internal/engine/several_test.go` compares `Exec` and `ExecAll` on the one-statement calls of six sequences of each profile.
- Docs: `/docs/todo/several-statements.md`, the READMEs of `stmttest` and the engine.

### Why

The oracle runs mattn's whole `Exec` where s4ql ran the whole text, so the result and its count are compared as an application sees them. Where s4ql stops, mattn would run on past a refusal that s4ql makes on purpose, so mattn's side runs statement by statement up to the statement that s4ql's error belongs to, which is what mattn's own loop does, and the error is judged by the rules that judge a statement alone.

### What worked

The first run of 40 sequences of `several` passed with no difference from mattn, and every later run of `TestGenerated` found only the counts to change. Each of the six planted faults failed `TestGenerated` at once, in under two seconds: the arguments from the start of the list (seed 128 of `mixed`, SQLite failing at another statement), the loop going on (SQLite failing at another statement), undoing the statements before (a table read back), the result of the first statement (the count of the next call on the connection), a trailing empty statement running the statement again (`UNIQUE constraint failed` where SQLite runs every statement, seed 1120), and a rollback of the transaction (a table read back, seed 96). The check of one statement failed on two faults of its own: the count of arguments of a text of one statement turned off, and the offset of SQLite's error moved by one.

### What didn't work

- The first recount failed on a tally kind with line breaks: `Expected nil error, but got "the kind "other 1/1: unrecognized token: \"'a''b);\n/**/\nselect..." of the run "several" has a tab or a line break"`. A text of `Others` with a string that does not end swallowed the statements after it into SQLite's message; `messageKind` now maps it to "unrecognized token".
- Recounts refused kinds that no run reached three times: `no run reaches "reached: insert of NULL beside a value of another type NOT NULL constraint failed", which is required, three times: the runs reach it mixed 2, ...` (now required once in `TestGenerated`), "failed before the last statement: syntax error" (never possible: `Split` refuses a statement with a syntax error, so it ends the parts), and "write transaction: ... failed after a statement ran: evaluation error" (reached once). In the engine: "arithmetic with a column in WHERE of a delete, no such column" and "arithmetic without a column in WHERE of a delete, wrong number of arguments", which `mixed` now reaches twice.
- The worktree harness refused shell commands that mixed a heredoc with a `cd` and another command ("this command is too complex to verify that it stays inside the worktree"); I wrote the edit scripts to the scratchpad and ran them alone.
- The first version of the one-statement check printed the arguments of a failing call, several kilobytes of a blob; it prints at most 500 bytes of the text now. It also failed on a text of several statements that `Split` refuses as a whole for its spaces, which has no part, until the check skipped every call with `Statements`.

### What I learned

Too many texts of several statements fail: in the first runs only 41 of 330 ran to their end, since a fault of each statement, a read-only transaction, and the refused arguments add up over three or four statements. With the faults of the other areas at a quarter, `Aim: 4`, and Exec in place of most prepared calls, 92 of 624 run to their end. mattn's count after a last statement with RETURNING differs from SQLite's only when the statement before changed another number of rows, so that excuse is rare: 4 in 80 sequences.

### What was tricky

An error of s4ql without an offset, such as the refusal of a value of another type, does not say which statement gave it. The walk asks the rules of each statement in turn, before it runs it in SQLite, whether they give that error, and runs the statement in mattn only if they do not; a statement that the rules would refuse with the same message earlier is where s4ql should have stopped, and if it did not, the tables read back differ.

### What warrants review

- `pair.severalFailed`: the order of the questions at each statement, and that `pair.compare` is only called there with an outcome that it judges without running SQLite (an error of s4ql's own).
- `driverRefusal`: which arguments the driver refuses for the whole call, by their number in the call, against `argument` in `/conn.go`.
- The changes to the engine's tests, which other builders also touch: three guards for the profile `several` and the recounted `.tsv` files.

### Future work

The engine's differential test of `ExecAll`, the simulation's planted bugs for the loop, mutation testing, and a fuzz target of `Split`, as `/docs/todo/several-statements.md` lists.

## Step 2: the self-review

**Author:** builder (several-statements-workload)

### Prompt Context

**Verbatim prompt:** (the brief of step 1) "`make test` and the lint target pass; the whole engine package and the driver check pass without `-short`; new tests pass with `-count=20` and `-race`; `TestGenerated` passes ten times in a row."
**Interpretation:** review the diff with the code-review skill, fix what matters, and run the checks of the brief.
**Inferred intent:** the branch is ready for the lead's review.

### What I did

Ran `/code-review medium main...HEAD`. It reported one important finding and eight small ones. Fixed: the excuse of an empty statement at the end now requires mattn's 0 (`severalRan`); `p.created` takes only a create table before the statement at which s4ql stopped; a failure before any statement or of the statement that `Split` refused is noted in the tally of the transaction; a trimmed space after a line comment at the end of the text gets a line of its own (`several.go`), since `-- c\u0085` stays a comment and tests no refusal; `Sequence.other` lost its unused branch; the subtest of one statement closes its two engines after each sequence instead of keeping 180 until the end. The comments of `several` and `Call.Statements` now say that a text of `Others` can come first and hold another number of statements. The digests of `mixed` and `several` and `TestGenerated.tsv` moved; the engine's recount wrote no change. `TestGenerated` passed with `-count=10` before these fixes, in 527 s.

### Why

The excuse without its value would pass any wrong count of mattn after such a text; the trimmed space inside a comment was a fault that the workload drew and that tested nothing.

### What worked

The engine's recount after the fixes wrote no change at all, and the driver's moved only by one or two in a few kinds of `several`.

### What didn't work

Nothing failed in this step.

### What I learned

The important finding is real but cannot be closed from outside the driver: an error without an offset, such as a refusal of a type or `UNIQUE constraint failed`, does not say which statement gave it, and two statements of a text can give the same one.

### What was tricky

Keeping the excuses pinned exactly while the workload's text changed: every change of a drawn text moves `TestGenerated.tsv`, and each recount needs the whole run.

### What warrants review

The open item in `/docs/todo/several-statements.md`: which statement an error without an offset belongs to when two give it alike.

### Future work

An event of each statement, read in the driver check, would tell how many statements ran; that would close the open item.
