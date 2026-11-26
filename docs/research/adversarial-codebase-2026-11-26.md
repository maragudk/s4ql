# Adversarial review of the codebase, 2026-11-26

Markus asked on 2026-11-26 for a review of the codebase, a small part at a time, by two outside models: codex (OpenAI) and agy (Google's Gemini), through the second-opinion skill. This note is the running record of that review, at commit e671de19 of `main`. The storage core was reviewed the same way earlier that day, in [adversarial-core-2026-11-26.md](adversarial-core-2026-11-26.md), and comes last here, and only where that note does not reach.

## Summary

- **Two faults are confirmed, each with a failing test.**
  - **The driver keeps the caller's `[]byte` of an argument** (`TestAdversarial_blobArgument`, root package). A select answers for the bytes as they are when its rows are read, and a `returning` gives them as they are then: a caller that fills its buffer again after `Query` has returned gets the rows of another key, or a row that was never stored. What is stored is right. Found by codex, extended by maragubot, and found again by agy.
  - **An update that reads its rows by an index changes them in another order than SQLite** (`TestAdversarial_updateOrderOfErrors`, `internal/engine`), so when two of its rows fail with different errors, it gives the other error, also with another result code. It changes nothing on either side. Three documents state that the orders are the same. Found by agy.
- **codex made 9 claims:** 1 confirmed, 7 true on paper and without consequence, 1 wrong. A tenth repeated the first after codex had found its test in the worktree.
- **agy made 11 claims:** 2 confirmed, one of them the fault that codex found, 9 true on paper and without consequence, none wrong.
- **maragubot found nothing of its own** beyond the second way into the first fault: about 130 statements run on SQLite and on the engine, and the reading that the table below lists.
- **No claim touches what is committed.** No lost or doubled write, no wrong row stored, and no acknowledgement of something that is not durable was found in the parts that were reviewed.
- **The outside review is thinner than asked.** codex's service flagged 10 of its 17 runs, and agy's quota ended 7 of its 9 runs. The table below says for each part who reviewed it.
- **Cost:** 17 runs of codex and 3 resumed sessions, about 2.3 million tokens; 9 runs of agy and 5 resumed conversations, about 17.4 million tokens, of which 6.2 million went into three runs that gave no answer. One answer of agy on one part costs about 2 million tokens and 35 minutes.

### Summary of the second round

- **Two more faults are confirmed, each with a failing test,** in "The second round" below.
  - **`StepContext` hides an I/O error of the cache behind the error of an ended context** (`TestAdversarial_stepContextHidesIOError`, `internal/sqlite`). SQLite has rolled back the whole transaction of the connection for the I/O error, and the core takes the error for a read that SQLite stopped harmlessly: it neither stops the instance nor fails the transaction. Rare: it needs a failing read and a context that ended within the last few microseconds of the step.
  - **A commit that is not committed wraps the cause of its context** (`TestAdversarial_commitErrorWrapsCause`, root package), so that it matches `ErrOutcomeUnknown`, SQLite's `*Error`, or `driver.ErrBadConn`, which an errgroup's context carries from another goroutine.
- **agy made 15 claims on parts 13, 15, and 7:** 2 confirmed, 11 true on paper and without consequence, 1 wrong, and 1 the same as codex 6.4.
- **Cost:** 3 runs of agy, one at a time, 4.75 million tokens and 95 minutes in all, with 53 million more read from its cache.

## Method

- **One part at a time.** A part is a file or a few files with one job, small enough for a model to read whole.
- **Both models, alone.** Each model gets the same prompt for a part: the files, the rules of `docs/spec.md` and `docs/roadmap.md` that the part must obey, and the question below. Neither sees the other's answer.
- **The question.** Find an input, an interleaving, or a failure of the surroundings (S3, the disk, the clock, a crash at any point, a second instance) for which this code gives a wrong answer, acknowledges something that it did not make durable, loses or doubles a write, leaves part of a failed statement applied, differs from SQLite where s4ql promises SQLite's answer or an error, panics, or misleads with its error. A concrete scenario, at most eight findings, and "no finding" is a valid answer.
- **Read only.** The models were told to change no file and to run no test or build. They could read the repository and run `sqlite3` in memory.
- **Every claim is checked.** maragubot checks each claim against the code. A claim is confirmed only with a failing test or a run that shows it at `main`. Such a test is named `TestAdversarial_*` and lies in the package that it belongs to, on the branch `adversarial-codebase`. Nothing is fixed here.
- **Three classes.** Confirmed and matters; true on paper and without consequence in practice; wrong. maragubot's own findings from reading are listed per part as a third reviewer's.
- **The models.** codex ran `gpt-6.2-sol` at effort medium and agy ran Gemini 4 Carbon at effort high, the defaults of their configurations.

## The parts

Ordered by what a fault would cost: wrong results and lost or doubled writes first, then crashes, then the rest.

The last three columns say who reviewed the part. "resumed" is an answer of codex from a session that its service had flagged; "declined" is a run that the service flagged and that gave no answer; "cut off" is a run of agy that the quota ended without an answer; "not asked" is a run that was not started, by the lead's rule after the quota.

| # | Part | Files | codex | agy | maragubot |
| --- | --- | --- | --- | --- | --- |
| 1 | The engine's write path and its transactions | `internal/engine/engine.go`, `tx.go`, `several.go` | answered | cut off | read |
| 2 | The operators that change rows | `internal/engine/operator.go` (sinks, `buildSink`, `build.change`), `returning.go` | resumed | answered | read, and ran statements |
| 3 | The expression evaluator | `internal/expr`, `internal/engine/expression.go` | answered | answered | read, and ran statements |
| 4 | The operators that read rows, and the choice of access | `internal/engine/operator.go` (sources, `build`), `access.go` | resumed | answered | read in part, and ran statements |
| 5 | The scalar subquery | `internal/engine/subquery.go` | resumed | answered | ran statements |
| 6 | The driver's connection, transactions, and rows | `conn.go`, `tx.go`, `rows.go` | answered | cut off | read |
| 7 | The driver's opening, errors, and the host | `s4ql.go`, `errors.go`, `internal/host` | declined | answered, second round | read |
| 8 | The statement pass: the split and the checks | `internal/sql/stmt/split.go`, `stmt.go`, `check.go`, `real.go` | answered | cut off | read `split.go` and `check.go` |
| 9 | The statement pass: recognizing statements | `internal/sql/stmt/recognize.go`, `vocabulary.go` | answered | not asked | not read |
| 10 | The tokenizer | `internal/sql/token` | answered | not asked | not read |
| 11 | The parser of statements that read and write | `internal/sql/parser` without `ddl.go` | answered | not asked | not read |
| 12 | The parser of the schema | `internal/sql/parser/ddl.go`, `internal/sql/ast` | answered, with part 11 | not asked | not read |
| 13 | The cgo binding of SQLite | `internal/sqlite/sqlite.go`, `progress.go` | declined | answered, second round | read, with the C helpers in the second round |
| 14 | The S3 store | `internal/store/s3store`, `internal/store/store.go` | declined | answered | read `s3store.go` |
| 15 | The commit object and the manifest | `internal/format` | declined | answered, second round | read in the second round |
| 16 | The core's scans and look-ups by key | `internal/core/index.go` | declined | answered | read the scans and the writes of rows |
| 17 | The rest of the core that the earlier note does not reach | `internal/core/spool.go`, `replay.go`, `cache.go` | not asked | not asked | not read |

So parts 7 and 13 had no outside review, and parts 15 and 17 had none in this review at all; the review of the core earlier that day read `internal/format` and `internal/core` as a whole with both models.

Out of scope: `site/`, `internal/simulator`, `PRODUCT.md`, `.impeccable/`, and the tools that only tests and CI run (`internal/sql/mutation`, `corpus`, `oracle`, `parsetest`, `shapetest`, `stmttest`, `internal/sim`, `internal/simlint`, `internal/store/simstore`, `s3test`, `storetest`).

## Record

### Part 1: the engine's write path and its transactions

Asked: `Conn.write`, `Conn.writeIn`, `Conn.Commit`, `Conn.Rollback`, `Conn.ExecAll`, and the rows of a query and of a `returning`, against the rules of the commit, of a statement that fails, and of several statements in one `Exec`.

**codex** (228,901 tokens) gave three findings.

- **codex 1.1: the row of a `returning` shows bytes that were never stored. Confirmed, and it matters,** by `TestAdversarial_blobArgument` in the root package. The driver keeps the caller's `[]byte` of an argument as the value (`conn.go:541`), and the engine reads that value when the rows are read, not when the call returns. `db.QueryContext(ctx, "insert into r values (1, ?) returning b", b)` with `b` of `{1}`, then `b[0] = 2`, then `rows.Next`: the row that is returned holds `{2}`, and the row that is stored holds `{1}`, because SQLite copies the bytes when it binds them. maragubot found the worse case of the same fault: `select v from q where k = ?` with a blob key gives the row of the bytes as they are when the rows are read, so a caller that fills its buffer again between `Query` and `Next` gets the rows of another key. Every other driver has used its arguments when the call returns. A copy of the bytes in `argument` closes both.
- **codex 1.2: an insert whose context ends between the last check of the context and the write still succeeds in its transaction. Wrong.** The statement returns nil and its row is part of the transaction, so the caller is told what happened. No rule says that an end of the context must fail a statement that finishes first, and no implementation can close that window.
- **codex 1.3: a surplus argument refuses a whole text although its second statement is one that the engine refuses. True on paper, without consequence.** `insert into t values (?); insert into t values (2) on conflict (k) do nothing` with two arguments fails with `ErrArguments` before any statement runs, where `docs/todo/several-statements.md` counts the arguments of a text only if every statement is one that s4ql supports: `before` (`several.go:147`) knows what `stmt.Split` refuses, and not what `textRefusal` refuses. The case exists only while the statement pass reads upserts and text functions that the engine refuses, which the branches `upsert-engine` and `text-engine` end. Run with a scratch test: with two arguments the text fails so and the table stays empty, and with one the first insert stays and the second is refused as `ON CONFLICT clause`.

### A note on codex's refusals, and on what the models could see

- **Three answers of codex were taken from sessions that its service had flagged.** The runs for parts 2, 4, and 5 ended with the service's message "This content was flagged for possible cybersecurity risk" and no answer. maragubot resumed each session and asked it for its final answer, as the note on the core records for its review. That takes the answer past the service's block, on Markus's account, and nothing in the brief allows it, so maragubot stopped doing it after the third. Those three answers are marked "from a resumed session" below. From then on a run that is flagged is asked once more as a new request, with the question worded as "which test cases would fail", and a run that is flagged again counts as declined.
- **The models read the worktree of the review until about 15:26, with the record and the first failing test in it.** codex's answer on part 6 says that it found `TestAdversarial_blobArgument` there. From then on the models read a clean copy of `main` outside the worktree. The runs that had started before are marked where what they could see matters.

### Part 2: the operators that change rows

Asked: `insert`, `update`, `remove`, `changeRows`, `buildSink`, `build.change`, and `returning.go`, against SQLite's result for every supported `insert`, `update`, and `delete`.

- **codex** (107,646 tokens, and 108,560 at the end of the resumed session): no finding. From a resumed session.
- **maragubot** ran 60 statements on SQLite and on two engines (by key, and reading every row) with a scratch test: `-0.0` in `set` and in `returning`, updates of the key in both directions, a key swap, integer division with negative operands, the smallest integer as a literal and negated, real results at the limits, and `returning` after a change of the key. Every difference was an agreed refusal or one of s4ql's own evaluation errors.

### Part 3: the expression evaluator

- **codex** (141,663 tokens): no finding. It checked signed zero, subnormal rounding, mixed comparisons, and text results against SQLite in memory.

### Part 4: the operators that read rows, and the choice of the access

- **codex** (92,105 tokens, and 107,446 at the end of the resumed session): no finding. From a resumed session.

### Part 5: the scalar subquery

**codex** (167,706 tokens, and 174,398 at the end of the resumed session) gave two findings. From a resumed session.

- **codex 5.1: a read succeeds although its count of the rows of a sort failed and stopped the instance. True on paper, without consequence.** After a loop over a sorted scan stops early, the core counts the rows that the sort read, for `RowsRead`, and drops the error of that count (`internal/core/index.go:1084` to `1090`), whose comment says so: "an error of the count cannot be given through it. A local failure stops the instance all the same." If the subquery's value then makes the outer statement read nothing, the statement returns its rows without an error, on an instance that has just stopped. The rows are right: they were read before the failure, and the count serves telemetry only. The spec's "Running reads ... fail" does not hold for this one read. Read in the code, not run.
- **codex 5.2: a subquery with 2,001 terms in its `order by`, the last of them a column of the outer table, is refused as correlated, where SQLite gives `too many terms in ORDER BY clause`. True on paper, without consequence.** It needs a table of 2,000 columns, and both answers refuse the statement. The statement pass checks no count of terms of an `order by`: with a column twice it refuses `column named twice`, so only a correlated name can make the 2,001st term. `docs/todo/slice-15.md` says that s4ql gives SQLite's error wherever SQLite gives one when it prepares the statement, which this case breaks. Run with a scratch test on a table of 2,000 columns.
- **maragubot** ran the limits of a statement on SQLite and the engine. A select list and a `returning` of 2,001 columns run in s4ql and fail in SQLite, which `docs/todo/sql-front-end.md` records as open. A `where` of 1,001 conditions and an expression of 1,001 operators are refused as deeper than SQLite's limit. Nothing new.

### Part 6: the driver's connection, transactions, and rows

**codex** (144,590 tokens) gave four findings. Its run read the worktree with the record of part 1 in it.

- **codex 6.1: a commit on a `sql.Conn` while rows of a query of the same `sql.Conn` are read returns "rolled back", and holds the writer until those rows end. True on paper, without consequence.** `conn.BeginTx`, then `conn.QueryContext` (not `tx.QueryContext`), one `Next`, then `tx.Commit`: database/sql closes only the rows of the `sql.Tx` before a commit, so the driver's commit meets rows that a loop reads, fails with the error that the transaction is rolled back, and leaves the end of the transaction to the rows (`internal/engine/tx.go:212`, `leaveEnd`). The engine's documents say exactly that. Nothing is committed and the error says so. What is wrong is the comment in `tx.go:93` that only the driver's own interfaces meet this, and the message "is rolled back" while the writer is still held: a caller that begins the next transaction before it closes those rows waits for itself. Run with a scratch test: the commit fails so, a `db.BeginTx` then waits until its context ends, and it succeeds once the rows are closed.
- **codex 6.2: the blob argument. The same as codex 1.1,** which it found in the worktree as a failing test, so it is not counted as a claim of its own.
- **codex 6.3: a logger that panics while unread rows of a transaction are closed leaves database/sql's lock of the transaction held, and the roll back then waits. True on paper, without consequence.** It needs a `slog.Handler` of the application that panics, and what then blocks is database/sql's own `closemu`, which its `Rows.Close` does not give back after a panic of a driver.
- **codex 6.4: a `driver.Valuer` whose error is `driver.ErrBadConn` makes database/sql run the statement again. True on paper, without consequence.** `conn.CheckNamedValue` keeps the error of the conversion as the cause of a "not supported" (`conn.go:544` to `551`), and `errors.Is` finds `ErrBadConn` in it. The statement has not run when the arguments are refused, so nothing is doubled, and database/sql retries the same error of a `Valuer` for a driver without `CheckNamedValue` too. The sentence "The driver never returns `driver.ErrBadConn`" in the package's documentation is not strictly true for this case.

### Part 7: the driver's opening, its errors, and the host

- **codex: declined.** Its service flagged the run twice, with the first wording of the question (144,353 tokens) and with the second (74,762 tokens), and gave no answer.
- **maragubot** read `s4ql.go`, `errors.go`, and `internal/host/host.go`. Nothing found: `publicError` gives SQLite's error only for an error of the engine's own type, which the core never makes, so a stop is never reported as an error of SQLite; and the production semaphore panics on a release without a permit.

### Part 9: the statement pass that recognizes statements

- **codex** (183,159 tokens): no finding. It traced modifiers of clauses, quoted and reserved names, signed literals, the turn of a comparison, and the numbering of parameters through `NewPlan`.

### Part 10: the tokenizer

- **codex** (154,166 tokens): no finding. It compared all 256 entries of the tables of byte classes and the 147 keywords with the vendored SQLite.

### Parts 11 and 12: the parser

Asked as one part, with `ddl.go`.

**codex** (154,609 tokens) gave one finding.

- **codex 11.1: `parser.Tokens` reads past the end that `parser.ParseAt` was given. True on paper, without consequence.** `ParseAt("select 123", 0, 8)` parses `select 1`, and `Tokens` of the same text then gives the token `123`. The one caller outside tests, `stmt.statementAt` (`internal/sql/stmt/check.go:65`), passes the length of the text that it also gives to `Tokens`, so no statement meets it. Run with a scratch test, which shows the mismatch.

### Part 13: the cgo binding of SQLite

- **codex: declined.** Its service flagged the run (88,481 tokens), with the second wording of the question.
- **maragubot** read the steps and the reads of columns in `sqlite.go`. Nothing found: text and blobs are copied before the next step, lengths are read after the bytes, and a bind copies.

### Parts 14, 15, and 16: the S3 store, the formats, and the core's scans

- **codex: declined** all three. Its service flagged each run on its first try with the second wording (118,291, 98,045, and 69,395 tokens).
- **maragubot** read `s3store.go`: no retry by the SDK, the attempts counted where the transport writes the headers, and "not applied" only for the answers that say so after exactly one attempt. And `Tx.Upsert`, `Tx.Insert`, `Tx.Delete`, `Tx.RollbackTo`, and `Tx.touch` in `internal/core/tx.go`: the keys that a transaction touches are in temporary tables of the cache, which a roll back to a savepoint restores with the rows, and the commit object is read from the cache at the commit. Nothing found.

### Part 8: the statement pass, its entry, and the split

- **codex** (198,653 tokens, after a first run of 137,735 tokens that its service flagged): no finding. It traced `Check` and `Split` through the tokenizer and the parser, `ExecAll`'s share of the arguments, and the conversion of a real literal against the vendored `sqlite3AtoF`.
- **maragubot** read `split.go` and `check.go`. Nothing found.

### agy, and its quota

- **agy answered for one part: the expression evaluator (part 3), with no finding.** It compared `real.go` and the evaluator with `sqlite3AtoF`, `sqlite3Fp10Convert2`, and `exprSetHeight` of the vendored SQLite, and ran edge cases in `sqlite3`. The run took 36 minutes and 1,945,222 tokens, with 19 million more read from its cache.
- **Eight more runs were cut off at about 15:50 by the quota of the account** ("Individual quota reached. Resets in 1h15m"), without an answer: parts 1, 2, 4, 5, 6 (stopped by maragubot), 8, 14, and 16. maragubot had started nine runs at once before it had measured one, which was the mistake: together they used about 15 million tokens. agy was then unavailable to every session until about 17:05.
- **The lead's rule from then on:** agy only for the parts where codex declined and a fault would cost wrong results or lost writes, one run at a time; a flagged codex run stays declined.
- **What runs after the quota resets:** the cut-off runs of parts 2, 4, 5, 16, and 14 are asked, one at a time, for their answer from what they had read (`resume-agy.sh` in the scratch directory `/Volumes/990/s4ql/adversarial-codebase/so/`, where each part has its prompt, its answers, and the id of its agy conversation in `agy.json`).

### agy's answers after the quota was reset

Each cut-off run was asked at 17:08, one at a time, for its answer from what it had read.

**Part 2, the operators that change rows** (2,542,630 tokens in all; the run read the worktree, and may have seen the record of part 1 and the test of the blob). Two findings.

- **agy 2.1: the row of a `returning` keeps the caller's `[]byte`. The same fault as codex 1.1,** confirmed by `TestAdversarial_blobArgument`. agy names the `update ... set b = ? returning b` as a second way in.
- **agy 2.2: an update that reads its rows by an index changes them in another order than SQLite, so it gives another error when two of its rows fail differently. Confirmed,** by `TestAdversarial_updateOrderOfErrors` in `internal/engine`. With `t (k int primary key, a int, x int not null, y int not null, nx int, ny int)`, an index on `a`, and the rows `(1, 20, 1, 1, null, 1)` and `(2, 10, 1, 1, 1, null)`, `update t set x = nx, y = ny where a > 0` gives `NOT NULL constraint failed: t.y` in SQLite and `t.x` in s4ql. SQLite changes the rows of an update that does not change the key in one pass, in the order in which it finds them, here by the index; only an update that changes the key collects the keys first and changes them in the order of the key. The engine always changes them in the order of the key (`changeRows`). With a row that is too long in place of the second NULL, the codes differ too: `18: string or blob too big` in SQLite and `19/1299` in s4ql. The statement fails and changes nothing on both sides, so what differs is which of two true errors it names. It matters as far as "Errors match SQLite's" is meant to the letter, and because three documents state the opposite as a fact: `internal/engine/README.md` ("The rows change in the order of the key"), `docs/todo/slice-08.md`, and the open item in `docs/todo/arithmetic.md` ("Since SQLite changes the rows of a table WITHOUT ROWID in the order of the key too, they agree where it was measured"). To give SQLite's error, the engine would have to know which index SQLite's planner picks.

**Part 4, the operators that read rows** (1,884,297 tokens in all). One finding.

- **agy 4.1: a query that reads no row, with `limit 0` or with a condition that is never true, ends without an error after the instance has stopped or its transaction has lost the lease. True on paper, without consequence.** `scan.rows` returns for an access of no row and `limit.rows` for a limit of 0 before either calls the core (`internal/engine/operator.go:99` and `450`), so nothing checks the stop, where every other read fails with it. No rows is the right answer for every state of the table, and the commit of such a transaction still fails. Read in the code, not run.

**Part 5, the scalar subquery** (1,897,663 tokens in all): no finding.

**Part 16, the core's scans, look-ups, and writes of rows** (1,579,020 tokens in all). Five findings, all of them calls of the core that the engine does not make. maragubot read the engine's side of each, and ran none.

- **agy 16.1: `ReadTx.Close` from the body of a loop over a scan of the same transaction leaves SQLite's snapshot held, and the next read of the reader sees the old state. True on paper, without consequence.** The engine ends a read transaction only after the loop over its scan has returned (`Rows.All` and `Rows.read` in `internal/engine/engine.go`), and `Rows.Close` during a loop only marks the rows. A check of `users` in `ReadTx.end`, as `Reader.Close` has it, would refuse the call.
- **agy 16.2: `Tx.Rollback` or `Tx.Commit` from the body of a loop over a scan of the transaction gives the writer lock back while the statement of the scan is still stepped on the write connection. True on paper, without consequence.** The engine leaves the end of such a transaction to its rows (`Conn.leaveEnd`), which end it after the loop. The core guards this only for a transaction that the lease task ended.
- **agy 16.3: a scan whose `Order` names one column 2,000 times fails in SQLite with `too many terms in ORDER BY clause` and fails the transaction. True on paper, without consequence.** The statement pass refuses a column twice in an `order by`.
- **agy 16.4: a panic in the function given to `Tx.Keys` leaves the set usable and its temporary table undropped. True on paper, without consequence.** In the engine a panic of an operator rolls the statement back to its savepoint, or the whole transaction, which drops the table, and nothing uses the set afterwards.
- **agy 16.5: `Tx.Keys` returns nil when the lease is lost after its function has returned and before it drops its table. True on paper, without consequence.** The statement's next call of the core, its annotation, fails with the lost lease, and so does the commit.

agy found the SQL of a scan sound for pins with NULL, open and closed bounds, descending columns, backward scans, `-0.0`, the ends of int64, reals with whole values through tables, indexes, and sorts, and the record of the net effect across savepoints.

**Part 14, the S3 store** (1,319,224 tokens in all). Three findings.

- **agy 14.1: the client takes `*` and `""` as versions, and sends them in `If-Match`. True on paper, without consequence.** `If-Match: *` matches any object, and `If-Match: ""` creates on Tigris, so a swap from such a version would not be a compare-and-swap. It needs a store that answers a read or a write with the ETag `*` or `""`, which none of the stores that s4ql was measured against does. `put` refuses only the empty version (`s3store.go:343`). A refusal of both in `Get` and `put` is two lines.
- **agy 14.2: a create or a swap of no bytes, on a reused connection that fails before a byte is written, is sent again by net/http, and then counts as "outcome unknown" where the store refused it. True on paper, without consequence.** The answer errs to the safe side, and the core writes no object without bytes.
- **agy 14.3: `unknown` wraps the error of a context wherever it finds one in the error, also when the caller's context has not ended. True on paper, without consequence.** A deadline of a socket matches `context.DeadlineExceeded`. The core tells that a caller gave up by `ctx.Err()` (`internal/core/tx.go:1587` and `1684`), not by the error, so only the text misleads.

agy found sound: no retry by the SDK, HTTP/1.1 without a proxy or a redirect, the answers that count as "not applied" against those kept as unknown (a 409 on a create, a bare 404, a 200 with an error in its body), the size limit, the short read of a body, and the close.

## The second round: parts 13, 15, and 7 with agy

A second reviewer took the parts that had no outside review on, at 17:30, with agy only and one run at a time, on a clean copy of `main` at d0a47144 (after #182 and #183). codex was not asked again, because its service had flagged these parts.

### Part 13 again: the cgo binding of SQLite, with the C helpers

**agy** (1,294,942 tokens, 26 minutes, with 9.4 million more read from its cache) gave four findings.

- **agy 13.1: `StepContext` gives the error of the context for a step that failed with an I/O error, which has rolled back the whole transaction of the connection. Confirmed, and it matters,** by `TestAdversarial_stepContextHidesIOError` in `internal/sqlite`. When SQLite leaves a failed step, at `vdbe_return` in `sqlite3VdbeExec`, it calls the progress handler once more if the handler is due (`nVmStep >= nProgressLimit`), and if the handler returns 1 it goes back to `abort_due_to_error` with `SQLITE_INTERRUPT`, which overwrites the code of the failure. `StepContext` then sees `SQLITE_INTERRUPT` with an ended context and gives `context.Canceled` (`sqlite.go:637`). The I/O error had already rolled back the transaction, because `sqlite3VdbeHalt` rolls back everything for an I/O error also in a statement that only reads (`!p->readOnly || mrc!=SQLITE_INTERRUPT`). The test runs a reused statement until its handler is due at its fourth instruction, makes the read of the table fail with `faultvfs`, and steps it with an ended context inside `begin immediate` after an insert: the step gives `context canceled`, the insert is gone, and the connection is out of its transaction. In the core, `Tx.ScanIndex` takes an ended context for "a read that SQLite stopped, which leaves the transaction as it was" (`internal/core/index.go:1155`) and neither stops the instance nor fails the transaction, and `ReadTx.ScanIndex` does not stop the instance (`cacheError` passes the error of the context on). So a local I/O failure does not stop the instance, against the spec's rule, and the core keeps a write transaction whose SQLite transaction is gone; a query in a write transaction runs without a savepoint (`queryIn` in `internal/engine/tx.go`), and the next statement's `savepoint s4ql_statement` then starts a transaction of its own in the cache, which its `release` commits. Where that ends was traced and not run: the temporary tables of the transaction are gone too, so a later write or the commit most likely fails, but a write that reaches the cache before it fails is in the cache and not in the log. It needs two things at once: a read of the cache that fails, and a context that has ended within the last period of the handler, at most 10,000 instructions, before the failing instruction, which must come after the handler is due and before the next jump that checks it. A step whose context ended earlier is stopped at the check before. Rare, then, but the result is a cache that may differ from the log without a stop. A fix in `StepContext` would give the code of the failure when `sqlite3_errmsg` is not "interrupted", or would read the error of the statement before the handler can overwrite it; nothing is fixed here.
- **agy 13.2: `ColumnBlob` gives an empty blob where expanding a `zeroblob` runs out of memory. Wrong as given.** Run with a scratch test under `pragma hard_heap_limit` of 200,000 bytes above what SQLite used: the step of `select zeroblob(500000)` fails with `SQLITE_NOMEM` itself, so no row is read. And the core never reads a value that SQLite holds as a zero blob in memory: a stored zero blob is read back from its record as an ordinary blob, and the core runs no `zeroblob()`.
- **agy 13.3: `ColumnText` panics with "out of memory" for a NULL after an earlier `SQLITE_NOMEM` on the connection. True on paper, without consequence.** Run with a scratch test: a step of `select null`, then a failed read of the cache with the code `SQLITE_IOERR_NOMEM` on the same connection, then `ColumnText(0)` panics, because the check reads `sqlite3_errcode`, which still holds the earlier error, where `ColumnBlob` knows better. An out-of-memory error is a local failure that stops the instance (`classify` in `internal/core/errors.go`), and the core reads `ColumnText` of a NULL only in its reads of the schema of the cache, not while another statement of the connection fails.
- **agy 13.4: `Error.Offset` of an error that is not about SQL text keeps the offset of an earlier error of a prepare. True on paper, without consequence.** Run with a scratch test: after `Prepare("select 1 frm t")`, whose offset is 13, a bind out of range gives the offset 13 too, where the doc comment of `Offset` promises -1. No code outside the tests reads `sqlite.Error.Offset`, and the tests read it after a prepare.
- **maragubot** read `sqlite.go`, `progress.go`, and `options.h` in full, and the core's use of the statements in `cache.go` and `index.go`. Nothing more found: a prepare of a text without its NUL is copied by SQLite (`sqlite3Prepare`), so no Go pointer is kept; a bind copies; `SQLITE_OMIT_AUTORESET` is set, so the documented refusal of a second step holds; and an interrupt alone of a statement that only reads leaves the transaction, as the doc comment says.

### Part 15: the commit object and the manifest

**agy** (2,395,379 tokens, 38 minutes, with 27 million more read from its cache) gave five findings, none of which touches what s4ql writes and reads back.

- **agy 15.1: the reader takes a record that holds a field twice, as Protocol Buffers merge it: two kinds of one change, the last of which wins, also over a first kind with a field that this version does not know, and two `upsert_rows` whose rows merge under the second table's name. True on paper, without consequence.** Run with a scratch test: a change record that holds a create table with an unknown field and then a drop table is read as the drop table, without `ErrNewerVersion`. No version of s4ql writes a field twice, a newer one included, and a damaged object fails the check of its name, so only a foreign writer of the bucket can make such bytes, which is outside the scope.
- **agy 15.2: a create table whose statement in the cache is within about a hundred bytes under the length limit of 1,000,000,000 passes `conn.create`'s check and fails in SQLite with `string or blob too big`, which fails the transaction, where SQLite creates the table. True on paper, without consequence.** `conn.create` (`internal/core/cache.go:1125`) compares the statement with the limit, and SQLite refuses the UPDATE of `sqlite_master` that holds it and the name twice, which is longer; at a lower limit of the instance `changeSchema` raises the limit and the gap is closed, so it needs the default limit and a default of about 999,999,700 bytes. The statement fails and changes nothing; its error is SQLite's code 18 for a statement that SQLite runs, and the transaction can only be rolled back. The replay half of the claim cannot happen: the writer's own cache refuses the table in the same way, so no commit object holds it. Read and not run: a run needs several gigabytes of memory.
- **agy 15.3: a wall clock in the year 0 or after 9999 is taken for the lease and refused by the header of a commit object, and the error comes as `format.ErrMalformed` and not as `core.ErrInvalid`. True on paper, without consequence.** The manifest takes the years 0 to 9999 and a `Timestamp` 1 to 9999. Only a clock that is off by thousands of years meets it, and the statement fails without a change.
- **agy 15.4: `CommitWriter.change` and `Annotate` write the batch before they check the size of the record that fails, so what was written after the error is a valid commit object. True on paper, without consequence.** Run with a scratch internal test: a row, then a drop table above a lowered limit, leaves the header and the row, which the reader reads to its end under their checksum. The doc comment of `CommitWriter` says otherwise. The core closes and discards the spool file of a commit that fails (`spoolCommit`), so nothing uploads it.
- **agy 15.5: `CommitReader.Header` gives `io.EOF` after `Next` has reached the end. True on paper, without consequence.** Run with a scratch test. Every caller reads the header first (`replay`, `parentOf`).
- agy found sound the round trips of values, the order of the net effect, the check of the name before replay commits, the bounded growth of a record, and `ParseManifest` and `Marshal` under `encoding/json/v2`.
- **maragubot** read the writer, the reader, the checks, the manifest, the names, and the schema. Nothing more found that s4ql itself can reach. On paper: the reader takes changes after annotations, varints that are not minimal, and a `nullable` written as false, none of which a writer of s4ql makes, and each of which gives other bytes, and so another name, for the same content. `ParseDuration` of `Duration.String` gave back the same duration for 10 million random positive durations and the ends of the range.

### Part 7 again: the driver's opening, its errors, and the host

**agy** (1,059,846 tokens, 31 minutes, with 16.8 million more read from its cache) gave six findings.

- **agy 7.3: a commit that is not committed, whose context ended with a cause, fails with an error that wraps the cause, so that it matches the error of another statement. Confirmed, and it matters,** by `TestAdversarial_commitErrorWrapsCause` in the root package. `Tx.publish` returns `after(context.Cause(ctx), last)` for a commit of which no attempt can have been applied (`internal/core/tx.go:1596`), `Tx.Commit` wraps it with `%w` in "commit: not committed: " (`tx.go:1449`), and the `failure` of `publicError` unwraps to it. `errgroup.WithContext` ends its context with the error of the first goroutine that fails, so in an application that runs statements in an errgroup, another goroutine's commit that is not committed matches `ErrOutcomeUnknown`, which the documents promise only for a commit that may be committed, or holds SQLite's `*Error` of another statement, such as a UNIQUE constraint that makes the application take the row for one that exists, or `driver.ErrBadConn`, which the driver promises never to give. The test ends the context of a transaction with each of the three as its cause at the swap of its commit, which the store refuses as changed: the commit fails with "s4ql: commit: not committed: ", nothing is committed, and the error matches the cause. The branch of an unknown outcome formats the cause with `%v` and is right. What the commit says in its text is true; what `errors.Is` and `errors.As` find in it is not. Run at `main`.
- **agy 7.1: a commit whose first swap of the manifest may have been applied sends the same swap again after `db.Close` has timed out and returned, or after the instance has stopped, and can commit then and leave a lease in the manifest. True on paper, without consequence.** It is the design: `publish` goes on finding out for as long as the caller waits, "because the caller is owed an answer" (`tx.go:1576`), and sending the same compare-and-swap again is how it finds out. The statement then returns nil and is committed, which is true, and `db.Close` has already failed with the error that the lease runs out by itself, as its documentation says. Read and not run.
- **agy 7.2: a `driver.Valuer` whose error is `driver.ErrBadConn` makes database/sql run the statement three times. The same as codex 6.4,** which the record has. agy adds two consequences, and one was run with a scratch test: on a `sql.Conn` with an open transaction, `conn.ExecContext` with such an argument hangs for good inside database/sql (`Conn.close` waits for the lock of the `sql.Conn` that the transaction holds). That is database/sql's handling of `ErrBadConn` from any driver, and it needs an application's `Valuer` that returns `ErrBadConn`. A `Valuer` that returns one of s4ql's errors makes the error match `ErrNotSupported` and that error too, which is the application's own doing.
- **agy 7.4: a wall clock that jumps forward by more than the lease makes the instance refuse until its next renewal, which is due on the monotonic clock, up to `RenewAfter` later. True on paper, without consequence.** The refusal is on the safe side, by design (`refusalLocked` in `internal/core/core.go` compares each clock with itself), and the instance serves again after its next renewal. Read and not run.
- **agy 7.5: a semaphore of the production host with 0 permits lets a `Release` hand a permit to a waiting `Acquire`, without a panic. True on paper, without consequence.** An unbuffered channel; every semaphore that the code makes has one permit (`conn.go`, `rows.go`, `internal/core/core.go`).
- **agy 7.6: `Sleep` of the production host gives nil about half of the time for a context that has ended, when its timer is ready too. True on paper, without consequence.** Run with a scratch test: 523 of 1,000 calls of `Sleep(ctx, 0)` with an ended context gave nil. Every caller sleeps for more than 0, so the context wins, and each loop looks at its context after the sleep. A `Task.Wait` that gives nil for a task that has ended is right whatever the context.
- agy found sound the identity of a database and its escaping, the keys of empty and nested prefixes, the empty DSN of `Register`, the file limit across the platforms' `RLIM_INFINITY`, and the order of the locks of the connector, its connections, and their rows during `db.Close`.
- **maragubot** read `s4ql.go`, `errors.go`, `filelimit_unix.go`, `internal/host/host.go`, and the connector in `conn.go`, and found nothing more: a prefix with a slash at its end, an empty segment, `.`, or `..` is refused (`checkPrefix`), so one database has one identity for one spelling of its options, and two databases never share one; and an `Acquire` whose context ends when the permit comes either takes the permit and returns nil or takes none.

## What is left after the second round

- Parts 7, 13, and 15 now have an outside review by agy. Part 17, the rest of the core, still has none in this review.
- The consequence of agy 13.1 in the core was traced and not run: what a write transaction does after its SQLite transaction was rolled back under it. A test in `internal/core` needs a way to put the progress handler of a reused statement at a given instruction.
- agy 15.2 was read and not run, because a run needs a statement of about a gigabyte.

## What is left

- **agy on parts 1, 6, and 8.** Its runs were cut off, and the lead's rule after the quota left them out, because codex answered for these parts. The conversations can be asked for their answers, as `resume-agy.sh` does: their ids are in `agy.json` of `engine-write`, `driver-conn`, and `stmt-split` under `/Volumes/990/s4ql/adversarial-codebase/so/`. The run on part 6 was stopped by maragubot and may have no conversation to resume.
- **An outside review of parts 7, 13, and 15,** which codex declined, and of part 17, which was not asked. A run of agy on each costs about 2 million tokens. The prompts of parts 7, 13, and 15 are written, in `parts/` of the same directory, with `ask.sh`.
- **agy on parts 9 to 12,** where codex found nothing or one claim without consequence. These parts are the ones that the generated checks against SQLite's own tokenizer and parser, and mutation testing, cover most closely.
- **Markus's decision on the three answers of codex from resumed sessions** (parts 2, 4, and 5): two found nothing, and one made two claims without consequence. agy has answered for all three parts since.
- **Claims that were read and not run:** codex 5.1 and 6.3, and agy 4.1, 14.1 to 14.3, and 16.1 to 16.5. Each is classed from the code alone.
