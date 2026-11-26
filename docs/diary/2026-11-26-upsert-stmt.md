# Diary: the statement pass of upserts

The first step of the piece of work "upserts": the statement pass in `internal/sql/stmt` reads and types `insert ... on conflict (key) do nothing` and `insert ... on conflict (key) do update set ... [where ...]`, with `excluded.c`, and the engine refuses them cleanly until its own step. The engine and driver, the generated checks, and mutation testing are later steps.

## Step 1: read and type the ON CONFLICT clause, and refuse it in the engine

**Author:** builder (upsert-stmt)

### Prompt Context

**Verbatim prompt:** You are a builder in s4ql (`maragu.dev/s4ql`), a Go SQL database with its own SQL engine, checked against SQLite. Its milestone is to run the Go libraries glue and goqite on it. Your ONE task is the first step of a new piece of work, **upserts**: the STATEMENT PASS for `insert ... on conflict (key) do nothing` and `insert ... on conflict (key) do update set ...`. The engine and driver, the generated checks, and mutation testing follow as steps of their own; do not start them.

## Setup

Your worktree starts from `origin/main`. Run `git reset --hard main`, then `git branch -m upsert-stmt` (plain single git commands, never in loops or compound commands). Read `AGENTS.md` first; it binds you (tests never skip except under `-short` and never depend on timing; no `.env` copies; large scratch only in `/Volumes/990/s4ql/upsert-stmt/`, with `GOTMPDIR` and `TMPDIR` set there for long runs). Never use the word "eval" in a shell command (the harness refuses it). One test process at a time, `-p 4`. No attribution lines in commits. Other builders work in `internal/expr`, `internal/core`, `internal/engine` (tests), and `internal/sql/mutation`: change `internal/sql/stmt` (and `internal/sql/parser` only if you must), keep changes elsewhere to what keeps the tree compiling and green, and say what you changed outside `internal/sql/stmt`. Do not touch `README.md`, `docs/roadmap.md`, `docs/spec.md`, or `site/`.

## What is agreed (product owner, 2026-11-24)

Read `docs/todo/writing.md` (the entry on upserts, with the product owner's words), `docs/roadmap.md` area "Writing", `docs/research/feature-map.md` (core gap 4), and the rules in `docs/spec.md` section "SQL" (for every statement s4ql gives SQLite's answer or an error, never a different answer; no compatibility quirks). In short:

- An `insert` without a conflict clause fails on a key that exists and changes nothing, as today.
- `insert ... on conflict (key) do nothing` and `insert ... on conflict (key) do update set ...` are supported.
- `replace into` and `insert or abort|fail|rollback|ignore|replace` stay rejected.

Also read `docs/research/non-determinism.md` (the rows on upserts: with several source rows of one key, the order in which a write visits rows decides what it writes; s4ql inserts exactly one row per statement today, which keeps that out for now).

The statement pass of earlier features is your model: read `internal/sql/stmt/README.md`, `recognize.go`, `stmt.go`, the first section of `docs/todo/arithmetic.md` and of `docs/todo/text-functions.md` (how a typed form is defined, where expressions may stand, how refusals are named and offset, how SQLite's own errors come through with its offsets), and how `stmt.Insert` and `stmt.Update` are typed today (`returning`, parameters numbered in text order, expressions in `set`).

## What to build

The pass reads and types:

- `on conflict (<columns>) do nothing` and `on conflict (<columns>) do update set <column> = <value or expression>, ... [where <conditions>]`, after the `values` of an insert and before `returning`.
- The conflict target: the columns of the table's primary key, or of a unique index (decide from what s4ql's tables have: read how `create table` and unique indexes are typed and what the engine knows of them; whether the target matches a key is a check against the table that belongs to the later engine step, but the typed form must carry the target's columns with their spans).
- In `do update`: the right sides of `set` are what an `update`'s are today (values, parameters, columns of the existing row, arithmetic, and the text forms), plus `excluded.<column>`, the row that the insert proposed; the `where` of `do update` in the forms an `update`'s `where` has, also over `excluded.<column>`.
- `returning` after the clause, as for an insert today.

Give the typed form as fields of `stmt.Insert` (a conflict clause with its target, its action, its assignments and conditions), with spans in the whole text and parameters numbered in text order across `values`, the clause, and `returning`.

Refused from the text, each with a clear `ErrNotSupported` naming the form and its offset, each checked against what SQLite does for the same text: `on conflict` without a target (SQLite allows it for `do nothing` and, as the last clause, for `do update`: decide by the agreed rule, lean to refusing, and record why); a target with a `where` (a partial index) or with a collation or an order on a column; several `on conflict` clauses in one insert; `on conflict` on an `update`; `replace into` and every `insert or ...` stay refused with their existing messages; a subquery in the clause's `set` or `where` unless the forms an `update` supports there fall out naturally (say which you took); an assignment to a column of the target or of the primary key (SQLite allows changing a key in `do update`: find what it does, and decide whether the pass refuses it or leaves it to the engine step; record it). Decide the further single cases by the agreed rule and SQLite's behaviour, and record each decision.

What SQLite itself rejects (a syntax error, an unknown column in `excluded`, and the like) must come out as SQLite's error with SQLite's offset wherever the pass can know it from the text; what needs the table is left to the engine step: say which.

Keep everything else working: the generators and rules in `internal/sql/stmt/stmttest` must keep compiling and passing and need not write the new forms yet. Statements with a conflict clause must be refused cleanly by the layers that cannot run them yet (the engine gives `ErrNotSupported`, never a panic and never an insert that ignores the clause): find where the engine builds an insert's plan and make that a clear refusal with a test, touching as little as possible.

## Tests

Hand cases in `TestCheck` and its neighbours, each compared with SQLite where SQLite has an answer; the pass's fuzz targets with seeds for the new forms; depth and length limits. Before you hand in: the pass's fuzz targets for five minutes in all, and `go test -count=1 -p 3 -run '^TestDifferential$' -timeout 40m ./internal/engine/ -sequences 400` once, to show the generated checks are undisturbed.

## Docs and finish

Decisions under a heading "The statement pass" in a new file `docs/todo/upserts.md` (add it to the index `docs/todo.md`; leave the agreed entry in `docs/todo/writing.md` and point from it to your file). Update `internal/sql/stmt/README.md`. Diary with the `fabrik:diary` skill in `docs/diary/2026-11-26-upsert-stmt.md`; commit as you go so that a fresh builder could take over. `make test` and the lint target pass; new tests pass with `-count=20` and `-race`. Run the `fabrik:code-review` skill over your diff and fix what matters. Do not push.

If the task threatens to pass half of your context, stop at a committed, green state and say what remains.

Report in at most 20 lines: branch and last commit; the typed form; every refusal you decided with what SQLite does for it; what is left to the engine step because it needs the table; what you changed outside `internal/sql/stmt`; any fault you found in existing code; anything open or that looks like a question of design for the product owner.

**Interpretation:** Extend the recognizer so that one ON CONFLICT clause with a target is a supported part of an insert, typed as `stmt.Insert.Conflict`, with `excluded.c` as a new kind of expression node in DO UPDATE; refuse every other form by name and offset; check the typed form against SQLite; make the engine refuse such a statement before it reads the database.

**Inferred intent:** Give the engine step a precise, SQLite-checked contract for upserts, and make sure nothing in the running system can execute an upsert as a plain insert in the meantime.

### What I did

- Probed SQLite 3.54.1 with the shell on about seventy texts (scripts in the session's scratchpad): targets with DESC, COLLATE, WHERE, parentheses, expressions, two clauses, assignments to the key, `excluded` in every place, a table named `excluded`, the order of the errors of names, the rows and counts, and the types.
- `/internal/sql/stmt/stmt.go`: `Insert.Conflict`, the types `Conflict` and `ConflictAction` (`DoNothing`, `DoUpdate`), the kind `ExprExcluded`, the field `Expression.Table`, `HasColumn` true for an excluded column, and `Insert.Parameters` counting the clause.
- `/internal/sql/stmt/recognize.go`: `conflict`, `targetColumn`, `isExcluded`, `excludedColumn`; the assignment loop of update moved into `assignments`, which the DO UPDATE shares, with the refusal of an assignment to the target; `operand`, `expression`, and `isCondition` read `excluded.c` while the recognizer reads DO UPDATE, and `operand` refuses a subquery there.
- Tests: about eighty new cases in `TestCheck`, depth edges and the 2,001-assignment case in its subtests, `checkUpsert` and `checkConflict` in `stmt_test.go`, `TestConflictAction_String`, `TestConflict_Present`, the allow-list for the new nodes and fields, and seeds for `FuzzSplit` (`FuzzCheck` takes every case as a seed already).
- Outside the package: `/internal/engine/upsert.go` (`upsertRefusal`, called first in `textRefusal`), a panic in `engine.NewPlan` for an insert with a clause, `TestEngine_upsert`, `stmttest.UpsertRefusal` and its test, and a hook with the excuse `excusedUpsert` and two seeds in the engine's differential pair and `FuzzSQL`.
- Docs: `/docs/todo/upserts.md`, its line in `/docs/todo.md`, a pointer in `/docs/todo/writing.md`, and the README of the pass (typed form, INSERT, a new section Upserts, the vocabulary, How it is tested).

### Why

The typed form is the contract for the engine step, so it carries everything the engine needs to print SQLite's messages (the target names with offsets, the qualifier `excluded` as written). A new kind of node rather than a flag on `ExprColumn` keeps old code from reading `excluded.v` as the table's `v` without noticing.

### What worked

- The comparison of three texts in SQLite, the source, its canonical statement, and the canonical statement with every operator in parentheses, on many states of a table, caught a planted bug (rendering `excluded.v` as `v`) in every one of the ten supported cases with an excluded column, after the states were widened (see below).
- `excluded.c` fell into the existing condition logic once `HasColumn` counted it: the side tests, `ValueFirst`, and the operator swap needed no change.

### What didn't work

- The first `checkUpsert` used six rotated samples per column. The planted bug above failed only 5 of the 10 cases: no state had a row equal to the inserted values, or two columns equal, or NULLs. I added states of the insert's values, of one value in every column, and of NULL, and a second set of arguments.
- Two cases had more names than values without a list of columns, which the check skips silently; I gave them a third value.
- `TestCheck_corpus` failed with `"INSERT INTO t4(a,b) VALUES(50E60) ON CONFLICT(a) DO NOTHING": sqlite: 1 values for 2 columns (code 1)` from `d.parameters`, which prepares the statement; the parameter count is now checked only where SQLite prepares it.
- I first applied the limit of 2,000 assignments to DO UPDATE too. The test failed with `Expected "too many columns in set list", but got "no such table: t"`: SQLite checks the length of the set list only in the rule of UPDATE (`sqlite3ExprListCheckLength(...,"set list")` in `/internal/sqlite/c/sqlite3.c`), so the pass no longer refuses a 2,001st assignment of DO UPDATE.
- A first engine test case, `on conflict (nosuch) do update set nosuch = excluded.nosuch`, was refused by the pass as an assignment to the target, not by the engine; I changed the column of the assignment.

### What I learned

- SQLite counts `excluded.v` 2 deep (the dot is an operator), which the depth edges against SQLite confirmed.
- In an upsert into a table named `excluded`, SQLite reads `excluded.v` as the table's column.
- In DO UPDATE SQLite looks up the values of SET before the columns of SET (`set zz = yy` gives `no such column: yy`), unlike an update.
- `on conflict (k) where k > 0` matches the primary key, which is not partial.

### What was tricky

- The vocabulary test reads the names of refusals from the README, so each new name needed its row there before any test passed.
- Keeping `HasColumn` meaningful: it now means "has a value for each row", for the table's row or the proposed one.

### What warrants review

- Whether refusing the targetless form, the assignment to the key, and subqueries in DO UPDATE is the right line (`docs/todo/upserts.md`).
- `checkUpsert` in `/internal/sql/stmt/stmt_test.go`: whether its states can tell apart every way the typed form could be wrong.
- The hook in `/internal/engine/differential_test.go`, in a file other builders touch.

### Future work

- The engine step: run DO NOTHING and DO UPDATE, pin SQLite's order of name errors, the target check against the key, and remove `upsertRefusal`.
- The generators of `stmttest` should write upserts; mutation testing of the new code.

## Step 2: long runs, self-review, and its fixes

**Author:** builder (upsert-stmt)

### Prompt Context

**Verbatim prompt:** (the brief of Step 1) "Before you hand in: the pass's fuzz targets for five minutes in all, and `go test -count=1 -p 3 -run '^TestDifferential$' -timeout 40m ./internal/engine/ -sequences 400` once ... Run the `fabrik:code-review` skill over your diff and fix what matters."
**Interpretation:** Show that the generated checks are undisturbed, fuzz the pass, have two independent reviewers read the diff, and fix what they find that matters.
**Inferred intent:** Hand in a branch whose test net does not cry wolf on the new forms anywhere in the tree.

### What I did

- `TestDifferential` with 400 sequences: `ok maragu.dev/s4ql/internal/engine 1150.197s`.
- Fuzzing, one process at a time, scratch on `/Volumes/990/s4ql/upsert-stmt/`: `FuzzCheck` 3 minutes (3.3 million runs), `FuzzSplit` 1 minute, `FuzzGenerate` 1 minute, all `ok`.
- `make lint`: 0 issues.
- Two reviewers on the diff, each measuring its claims with the shell. Fixed:
  - Both: the driver's differential pair (`internal/sqlite/crosscheck/drivercheck`) had no rule for the refusal, so any upsert that `FuzzSequence` reached would have failed with `s4ql gives exec: exec failed with s4ql: not supported: ON CONFLICT clause (at offset 26), where the rule gives ""`. Added the rule in `checkNotSupported`, `excusedUpsert` there, and a `TestPair` subtest through Exec, Query, and Prepare, which fails without the rule.
  - One reviewer, serious: `checkUpsert` expected the error of a target of two columns where SQLite gives `1 values for 2 columns` first (`insert into t (k, j) values (1) on conflict (k, j) do nothing`), a valid input on which `FuzzCheck` would fail. Fixed, with a case. The branch of two columns now runs the canonical texts too, and the check stops above 2,000 names, which no table holds.
  - One reviewer: in the engine's pair the upsert rule came after the rule of the length limit, so a long literal in an upsert would have been judged by the wrong rule. Moved, with a test on an instance with the length limit.
  - Both: `'excluded'.v` was refused as `column with the name of a table`; SQLite takes it as excluded, so it is now `single-quoted name`, like brackets and backticks.
  - Both: comments that described callers (`upsertRefusal`, `NewPlan` and its panic text, `stmttest.UpsertRefusal`, `textRefusal`), stale comments in `recognize.go`, the doc of `Condition`, and `assignments` keyed the limit on `target == nil` where `r.doUpdate` says it.
  - Docs: the order of the errors of names in DO UPDATE is assignment by assignment, value before column (an update names `yy` first for `set zz = yy` too, so my contrast was wrong); SQLite takes a target `t.k`; the Results table of the README, which had not been updated since before text functions.
  - `checkExpression` now requires the zero `Table` on every node but `ExprExcluded`.

### Why

A differential test that fails on a correct answer is worse than none: it is the next nightly's false alarm.

### What worked

- Reviewers who reproduced each claim with the shell or a probe test; every serious finding came with a command.

### What didn't work

- My first `checkUpsert` had two oracles that were wrong on valid inputs (above); the five minutes of fuzzing did not find them, the reviewers did.
- A shell command with a quoted `'\''` in an inline Python script was refused by the harness, yet its edits were on disk afterwards; I checked each edit with `grep` before going on.

### What I learned

- `excusedUpsert`-style constants are a pattern of the pairs (each excuse is a named tally), so I kept it although it is used once.
- `TestAllowList` now allows `ColumnRef.Table` for every statement; recorded in `docs/todo/upserts.md`.

### What was tricky

- Two differential pairs, in two modules, each with its own idea of what "not supported" may mean for a text that the pass takes.

### What warrants review

- `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`, `checkNotSupported`, and the move of the hook in `/internal/engine/differential_test.go`.
- A pre-existing fault the reviewer found, not fixed here: on `main`, `update t1 set v1 = 1 where lower(v1) = 'a'` through `callPrepareExec` fails the driver's pair, since `checkRefused` rejects a refusal at prepare and Prepare runs `engine.Check` with `textRefusal`.

### Future work

- The fault above, in the driver's pair, for the lead to assign.
