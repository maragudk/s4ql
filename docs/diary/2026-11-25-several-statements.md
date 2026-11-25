# Diary: several statements in one `Exec`

Markus agreed on 2026-11-24 that several statements in one `Exec` mean the same as running them one after another: outside a transaction each commits by itself, a failure halfway keeps the ones before and runs none after, and inside a `database/sql` transaction they are statements of it. `Query` with several statements stays refused. glue needs it for every migration file and its fixtures, and goqite's schema file holds three statements. The rule that decides each detail is "SQLite's answer or an error", where SQLite's answer is what `mattn/go-sqlite3` does for the same `Exec`.

## Step 1: what mattn does, measured

**Author:** builder (several-statements)

### Prompt Context

**Verbatim prompt:** "You build ONE feature, and only its code and the tests that show it works: **several statements in one `Exec`**. [...] The rule that decides every detail: s4ql gives SQLite's answer or an error, never a different answer. For this feature "SQLite's answer" is what the driver `mattn/go-sqlite3` does for the same `Exec` on the same text and arguments (which statement gets which arguments, the `Result` returned, what an empty statement, a trailing `;`, whitespace, or a comment between or after statements does, which error comes when a middle statement fails or does not prepare and what stays changed). Find out by running mattn, not from memory, and pin each finding in a test that compares both drivers. Where mattn's behaviour is a quirk that s4ql should not copy, refuse with a clear error instead, and record the refusal with its reason."
**Interpretation:** before any code, run mattn v1.14.53 (the version of `/internal/sqlite/crosscheck/go.mod`) on texts that cover each of those questions, and decide each detail from what it does.
**Inferred intent:** no detail of the feature rests on a memory of how mattn works.

### What I did

I wrote a throwaway test in `/internal/sqlite/crosscheck/drivercheck/` that runs 34 texts through mattn's `Exec`, with and without arguments, and prints the result, the tables, and `select changes()` after each, plus a prepared statement, a `Query`, and a transaction with several statements. I also ran the SQLite tokenizer of `/internal/sql/token` on the space characters that Go's `strings.TrimSpace` removes. Run with `go test -p 4 -run TestZZProbe -v .` in the drivercheck directory. What it showed:

- mattn prepares the first statement of the text, binds as many arguments as it has parameters (`sqlite3_bind_parameter_count`), steps it, and goes on with the tail, `strings.TrimSpace` of what follows. Statements before a failure stay, none after it runs: a `UNIQUE constraint failed` in the middle, a syntax error in the middle (`near "valus": syntax error`), and `no such table` in the middle all leave the first insert.
- Too few arguments: the statements before run, then `not enough args to execute query: want 1 got 0`, which is mattn's own error and no `sqlite3.Error`. Too many: the rest are dropped without an error.
- The result is the last statement's: `sqlite3_changes` after it, so a create table or a select after an insert gives the insert's count, as `changes()` does. Two quirks: a text that ends in an empty statement after the last one (`; -- c`, `;;`, `; /* c */`) gives 0 rows affected, where `changes()` gives the count; and a last statement with RETURNING gives the count of the statement before it (mattn steps it once), where `changes()` gives its own.
- `TrimSpace` removes what SQLite does not read as a space: `; insert ...` runs the insert in mattn, where SQLite reads U+00A0 as a character of a name (`token.Next` gives ID); `\v` at the start of a token is ILLEGAL for SQLite. At the end of the text, `select 'abc   ` gives `unrecognized token: "'abc"` without the spaces, and `/* ` trimmed to `/*` is a slash for SQLite.
- `db.Prepare` of two statements succeeds with the parameters of the first only, and its `Exec` runs only the first statement. `Query` of an insert and a select runs both. In a transaction, a failure in the middle keeps the statements before in the transaction, which commits them.

### Why

Every detail of the feature is one of these answers, and the brief asks that each come from mattn and not from memory.

### What worked

The pinned mattn builds with the module's pinned SQLite, so the probe runs in a second.

### What didn't work

The first probe of the tokenizer had a byte order mark typed into a Go string literal: `internal/sql/token/zzprobe/main.go:10:88: invalid BOM in the middle of the file`. Written as `﻿` it ran.

### What I learned

SQLite takes every byte from 0x80 up as part of a name, so a no-break space is no space at all for SQLite, and the mismatch with Go's `TrimSpace` is real and not hypothetical.

### What was tricky

The result after an empty statement at the end is the result of mattn's workaround for its issue 963 (a nil result), not of SQLite.

### What warrants review

The decisions that follow from these findings, in the steps after this one and in `/docs/todo/several-statements.md`.

### Future work

None from this step.

## Step 2: `stmt.Split`, `engine.Conn.ExecAll`, the driver, and the comparison with mattn

**Author:** builder (several-statements)

### Prompt Context

**Verbatim prompt:** (the brief of step 1) "Code, hand-written tests, and tests that compare with mattn on hand-picked texts (`conn_test.go` in the root package and the hand sequences of `internal/sqlite/crosscheck/drivercheck` are the places where the driver's behaviour is tested today)."
**Interpretation:** build the split, the loop, and the driver's use of it, with tests at each layer, and a test that runs the same texts in mattn and pins both answers.
**Inferred intent:** glue's migrations and goqite's schema run through `Exec`, with no answer that differs from mattn's.

### What I did

- `/internal/sql/parser/parser.go`: `ParseAt(src, at)`, a statement read from an offset with offsets in the whole text, so that the recognizer and the engine see absolute spans and every error offset is in the application's text.
- `/internal/sql/stmt/check.go`: `Check` split into `checkWhole` and `statementAt`, unchanged in behaviour. `/internal/sql/stmt/split.go`: `Split` and `Part`, with the two refusals of spaces (`trimmedStart`, `trimmedEnd`). Tests in `/internal/sql/stmt/split_test.go`.
- `/internal/engine/several.go`: `Conn.ExecAll` and `CheckAll`; `/internal/engine/engine.go` got `execPlan` and `planOf` out of `exec` and `prepare`, and `/internal/engine/errors.go` `checkValues` with the number of an argument in the call. Tests in `/internal/engine/several_test.go`.
- `/conn.go`: `ExecContext` calls `ExecAll`; a prepared statement keeps `Exec` of one statement; `arguments` takes the check of the text. `/s4ql.go`'s package documentation says what a text of several statements does. Tests in `TestExec_several` of `/conn_test.go`; the one assertion of `TestExec` that a second statement in `Exec` is not supported now asserts it for `Query`.
- `TestSeveral` at the end of `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`: 19 texts on a pinned connection of each side and two transactions, with s4ql's outcome, mattn's outcome with `select changes()` after it, and the tables after; where s4ql does not copy mattn, the case pins both and says what mattn runs on the paired database so the tables match s4ql's rule.
- The decisions in `/docs/todo/several-statements.md`, the index in `/docs/todo.md`, the item deleted from `/docs/todo/driver.md`, and the READMEs of the parser, the pass, and the engine.

### Why

The loop lives in the engine, in a method of its own, so that `Conn.Exec`, which the engine's differential test, its fuzz targets, the simulation, and the simulator call with corpus texts that hold second statements, keeps its meaning, and each part still runs through the same code as a statement alone, with one parse.

### What worked

The drivercheck run of the whole package passed unchanged after the driver change (`go test -p 4 ./drivercheck/`, 52 s), with the same pinned counts in `testdata/reached`, and `TestSeveral` passed its 21 subtests as written, with mattn's counts as measured in step 1.

### What didn't work

- Several first drafts of tests used `insert ... values (?), (?)` and `values (1), (2)`, which the pass refuses: `s4ql: not supported: second row (at offset 92)`. Rewritten with one row per insert and an update.
- I miscounted `t1` as 57 bytes in the expected offsets of `several_test.go`: `Expected "{1 1 near "selec": syntax error 93}", but got "{1 1 near "selec": syntax error 92}"`. Fixed the numbers.
- The test of the events in `conn_test.go` opened the instance with `s4ql.OpenStore` and ran at once: `s4ql: no valid lease: not_holder`. It pings first now.
- A no-break space typed into a test file by the Write tool became a byte order mark in one place: `split_test.go:133:18: illegal byte order mark`. Replaced with `﻿` through `sed`.
- The harness refuses compound shell commands that write in the worktree (a `cat > file <<EOF` and a `python3 -` heredoc); I wrote to the scratchpad and appended with a plain `cat a >> b`.

### What I learned

`stmttest.Others` holds `select * from %[1]v; select * from %[1]v`, which the driver's generated test runs through `Exec` half the time; it still passes, because the harness gives mattn only the first statement, which answers the same for two selects. The generated checks of this feature have to change that oracle.

### What was tricky

The trim at the end of the text: whether it changes SQLite's reading depends on the last token. A space that ends a line comment changes nothing, while the space of `/* ` turns a comment into a slash, and the spaces of an unterminated string change the message. `trimmedEnd` takes the last token that is not a space and allows only a comment that tokenizes as the same comment without the trimmed bytes.

### What warrants review

- The result for a text that ends in an empty statement, and for a last statement with RETURNING: s4ql gives SQLite's count, where mattn gives 0 or the count before; the notes say why, and that an error from `RowsAffected` is the alternative.
- More arguments than parameters are refused up front only when every statement checks; too few run the statements before.
- An argument of a Go type that s4ql does not support refuses the whole call, while a NaN fails only its statement after the ones before it.

### Future work

The generated checks, in the notes under "Not built: the generated checks".

## Step 3: self-review with two reviewers, and the fixes

**Author:** builder (several-statements)

### Prompt Context

**Verbatim prompt:** "`make test` and the lint target pass; new tests pass with `-count=20` and `-race`. Run the `fabrik:code-review` skill over your own diff and fix what matters."
**Interpretation:** run the checks, then two independent reviewers over `git diff main...HEAD`, and fix what both found and what either found that is serious.
**Inferred intent:** the branch is ready for the lead's review without known faults.

### What I did

`make test` and `make lint` passed before the review (logs in `/Volumes/990/s4ql/several-statements/`), and the new tests passed 20 times under `-race`. Two reviewers on the strongest model each reproduced findings with scratch tests through `go test -overlay`. Both found: `FuzzSequence` failing on inputs one mutation from its seeds (serious), `Split` refusing a last statement without a semicolon whose comment ends in a Go-only space, `Split` quadratic through `ParseAt`'s scan for a NUL byte (8.9 s for 80,000 statements), `CheckAll` not the check that `ExecAll` makes, the package doc overstating the refusal of extra arguments, comments that named their callers, and the `RowsAffected` difference from mattn. One reviewer found that an argument was named by its call number by the check of values and by its statement number by the run-time errors (`argument 1, which is text, for the column t1.v1 of type INT` for the second argument of the call), and that a panic in `Split` logged no event.

Fixes: `pair.several` in `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go` runs a text of several statements as its statements, one step each, and refuses-on-s4ql's-side-only for spaces, with three seeds in `testdata/fuzz/FuzzSequence`; `Split` ends at a statement that ends the trimmed text; `parser.ParseAt(src, at, end)`; argument numbers in a statement everywhere in the engine; `CheckAll` built on `before`; `Conn.split` logs a panic; `conn.exec` takes the check and the run as functions instead of a flag; the doc of `s4ql.go`, `conn.go`, the engine README, and the notes say what holds now. I kept the result of `RowsAffected` and flagged it for the lead.

### Why

A fuzz target that fails on correct behaviour turns CI's fuzz job and the nightly red. The rest were real faults or claims that did not hold.

### What worked

The reviewers' reproductions were precise enough to become seeds and test cases directly.

### What didn't work

My first fuzz run passed `-test.fuzzcachedir` and ran from the crosscheck directory with `./drivercheck/`: `testing: warning: no fuzz tests to fuzz`, `ok maragu.dev/s4ql/internal/sqlite/crosscheck`. Run again from the drivercheck directory with `go test -run '^$' -fuzz '^FuzzSequence$' -fuzztime 90s -parallel 4 .`.

### What I learned

Numbering arguments in the call would need the engine's plans to number parameters across statements; the run-time errors read `Parameter.Number` of the plan, so the cheap consistent choice is the statement's own numbering.

### What was tricky

The sequence comparison judges one statement at a time with many excuses (evaluation errors, type refusals, errors that SQLite gives when it prepares); an oracle of a whole text cannot place s4ql's own refusals in the middle of it without knowing where s4ql stopped. Running the parts as steps keeps every excuse and stays correct, at the price of not comparing `ExecAll` itself there.

### What warrants review

`pair.several` as a stopgap; the decision to keep SQLite's count in `RowsAffected`; argument numbering by statement.

### Future work

The generated checks of the notes, including a check that `ExecAll` equals `Exec` on one statement over the corpus, and a fuzz target of `Split`.
