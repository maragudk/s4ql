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
