# SQL oracle

`internal/sql/oracle` gives SQLite's verdict on SQL text. s4ql's own parser, which is not written yet, will be tested against it through the harness in [`internal/sql/parsetest`](../parsetest/README.md).

`oracle.Judge(src)` judges the first statement of `src` and returns a `Verdict`: whether the statement parses in the vendored SQLite 3.54.1, SQLite's syntax error and its byte offset if it does not, and where the statement ends. Any bytes are allowed, NUL bytes and invalid UTF-8 included, and the verdict is the same on every call. `Judge` prepares the text with `sqlite3_prepare_v2`, through `Conn.PrepareFirst` of the binding in [`internal/sqlite`](../../sqlite/README.md), and never steps it.

This serves the first of the four checks of the parser that [docs/todo.md](../../../docs/todo.md) lists: agreement with SQLite on acceptance and on the exact message and byte offset of a syntax error. The other checks are not built.

SQLite's parser runs the actions of its grammar while it parses. Some actions raise errors that are not about syntax, such as `unknown table option`, and such an error can stop the parse before the statement's end. SQLite has then said nothing about the syntax of the rest. The oracle never guesses: a `Parses` or a `SyntaxError` is a fact about SQLite's grammar, and every other outcome is `NotSaid`. The doc comments in `oracle.go` hold the details of what follows.

## Parses

SQLite's parser read the whole statement, its end included, without a syntax error. The statement either prepared, or failed with a message that is not a syntax message after the parser had been handed the statement's end. That end is the statement's semicolon, or the empty semicolon that SQLite feeds its parser at the end of the text. `Verdict.Message` holds the error that came after the parse, or `""`.

Three facts about SQLite make this sound:

- The parse loop stops after the token on which an error was raised, and sets the tail after that token. The token that ends at the tail is therefore the one.
- The actions that build a statement and resolve names run when the end arrives. That is where `no such table` comes from.
- A syntax error on the end would replace any such message, so the message that remains is not about syntax.

Three cases need more care:

- When the tail is at the end of the text and the last token ends there too, the tail does not tell the last token from the empty semicolon after it. `Judge` then prepares the text again with a space added, where the empty semicolon leaves the tail one byte further.
- A semicolon can stand inside two statements. In CREATE TRIGGER it is the statement's end only after `; END`. In CREATE VIRTUAL TABLE the arguments of the module are any tokens, `;` too, so a semicolon is inside the arguments while a parenthesis is open.
- A variable `#N` and a misplaced digit separator are syntax errors that actions raise, and a later message can replace them. A statement that holds either and fails with a message that is not a syntax message is `NotSaid`, never `Parses`.

Only the result codes SQLITE_ERROR and SQLITE_AUTH count.

## SyntaxError

The statement does not parse in SQLite, in any schema. The result code is SQLITE_ERROR, and the message is one of three, which `oracle.IsSyntaxMessage` recognizes:

- `near "X": syntax error`
- `incomplete input`
- `unrecognized token: "X"`

They come from the grammar's syntax-error routine, from the parse loop on a token that the tokenizer does not know, and from the two actions that check a token's text: a variable `#N` outside SQLite's nested parses, and a misplaced digit separator.

## NotSaid

SQLite does not say whether the statement parses. `Verdict.Reason` says why:

- `StoppedEarly`: an action's error or a limit stopped the parse before the statement's end.
- `InsideTrigger`: the parse stopped on a semicolon inside the body of CREATE TRIGGER.
- `InsideArguments`: the parse stopped on a semicolon inside the arguments of CREATE VIRTUAL TABLE.
- `Overwritten`: the parse reached the end, but a later message may have replaced the syntax error of a `#N` or of a misplaced digit separator.
- `Unsettled`: the text with a space added did not settle whether the parse had reached the end.
- `NotParsed`: SQLite gave another result code, such as SQLITE_TOOBIG, or did not parse the text at all.

## Connection settings

Each prepare runs on a new connection to an empty `:memory:` database, so a PRAGMA that acts when it is prepared cannot leak into the next verdict. The connection has:

- extended result codes, and the compile options of the binding in [`internal/sqlite/options.h`](../../sqlite/options.h)
- comments allowed, which is the default of SQLITE_DBCONFIG_ENABLE_COMMENTS; with it off, a comment is an unrecognized token
- `Conn.DenyProcessPragmas`, because the PRAGMAs `soft_heap_limit`, `hard_heap_limit`, `temp_store_directory`, and `data_store_directory` change the whole process when they are prepared. They fail with `not authorized` after the parse, so their syntax is still judged.
- SQLite's default limits

The limits that show in verdicts are the compile-time defaults of 3.54.1:

| Limit | Value | In a verdict |
| --- | --- | --- |
| SQLITE_LIMIT_SQL_LENGTH | 1,000,000,000 bytes | A longer text is `NotSaid`, `NotParsed`. |
| SQLITE_LIMIT_PARSER_DEPTH | 2500 | 2,493 nested parentheses parse; 2,494 give `Recursion limit`, which is `NotSaid`, `StoppedEarly`. |
| SQLITE_LIMIT_EXPR_DEPTH | 1000 | `Expression tree is too large (maximum depth 1000)` is raised at the end, so the statement is `Parses`. |
| SQLITE_LIMIT_VARIABLE_NUMBER | 32766 | `variable number must be between ?1 and ?32766` is raised when the token after the variable arrives: `select ?32767` is `Parses`, and `select ?32767 from t` is `StoppedEarly`. |
| SQLITE_LIMIT_COMPOUND_SELECT | 500 | `too many terms in compound SELECT` is raised at the end, so the statement is `Parses`. |
| SQLITE_LIMIT_COLUMN | 2000 | `too many columns on t` in CREATE TABLE is `StoppedEarly`; `too many columns in result set` is raised at the end. |

Tests in `oracle_test.go` pin the parser depth and the expression depth.

## The empty schema

Some statements must name an object that exists, and in an empty database SQLite stops them early:

- CREATE TRIGGER always stops at BEGIN, with `no such table`.
- ALTER TABLE ... ADD COLUMN with column constraints stops before its end, with `no such table`.
- A name in an unknown schema stops at the next token, with `unknown database`.

On such a `NotSaid`, `Judge` creates what is missing and judges again: the table (`create table "name" (c)`), a view for an INSTEAD OF trigger, or an attached `:memory:` database for the unknown schema. It does this at most three times for one statement, and `Verdict.Setup` lists the statements it ran. The grammar does not depend on the schema, so a `Parses` or a `SyntaxError` found this way holds for the empty database too.

One offset does depend on the schema: that of the error of a variable `#N` raised on the semicolon at the end. SELECT, CREATE VIEW, ATTACH, and DETACH read the schema in their action even after an error, and reading it the first time runs SQL that leaves the offset at -1. `create view aux.v as select #1` gives the offset 28 once `aux` is attached, since attaching read the schema, while in the empty database the action's `unknown database aux` overwrites the error. A `#N` error found with setup is therefore `NotSaid`, `Overwritten`, as in the empty database.

## Verdicts on the corpus

On the 139,234 statements of the corpus in [`internal/sql/corpus`](../corpus/README.md):

- `Parses`: 84,215. Of these, 25,608 prepared and 58,607 failed after the parse, 53,588 of them with `no such table`.
- `SyntaxError`: 54,711.
- `NotSaid`: 308. Of these, 295 stopped early, 11 stopped inside a trigger, and 2 are overwritten. None is unsettled or inside arguments.

The largest groups that stopped early are `duplicate column name` (78), `no such collation sequence` (49), `object name reserved for internal use` (24), `no such column`, as for `UNIQUE(rowid)` (18), and `unknown join type` (14). The report of the harness lists every group.

Setup was needed for 1,463 statements and settled 1,379 of them: 1,011 CREATE TRIGGER, 189 CREATE TABLE in a schema that is not attached, 167 ALTER TABLE, and 12 CREATE VIRTUAL TABLE. With the empty schema alone, 1,687 statements would be `NotSaid`.

## How the oracle is checked

- `TestJudge` in `oracle_test.go` holds a table of texts with their whole verdicts.
- `oracle_internal_test.go` covers the code paths that would need a text of a gigabyte or a failing connection.
- `TestJudge_corpus` checks five properties on every entry of the corpus, or on every tenth with `-short` and under the address sanitizer:
  1. The fields of the verdict are in range.
  2. The offset of a syntax error is at the token that the message names.
  3. A statement that parses is complete: with `)` in place of its end it gives `near ")": syntax error` at that `)`, because no statement of the grammar can go on with `)`.
  4. A syntax error is the first one: the text before its token has no syntax error that names an earlier token.
  5. Bytes after a NUL byte change nothing.
- `FuzzJudge` checks the same properties, and that two calls give the same verdict. `make fuzz` runs it.

## What the parser must match

These are the facts about SQLite's messages, offsets, and tails that are easy to get wrong. Each example is a text and SQLite's verdict on it.

- The `X` in `near "X"` is the token's text as written, in raw bytes. `incomplete input` has the offset -1.
- `unrecognized token: "X"` has its offset at the token, and its tail at the start of the token, not after it.
- An unterminated string, quoted name, or blob runs to the end of the text or to a NUL byte: `select 'a\x00b'` gives `unrecognized token: "'a"`.
- A misplaced digit separator has the offset -1, and its message shows the number partly rewritten: `select 1_` gives `unrecognized token: "1_"`, and `select 1__2__3` gives `unrecognized token: "12_2__3"`.
- A variable `#N` has the offset -1 when its error is raised at the end of the text: `select #1` gives `near "#1": syntax error` at -1, but `select #1 from t` gives it at 7.
- The corpus has 2,836 syntax errors with the offset -1: 2,812 `incomplete input`, 13 for `#N`, and 11 for a digit separator.
- The checks of `#N` and of the digit separator run when the next token arrives, and a syntax error on that token wins: `select 1_ 2` gives `near "2": syntax error` at 10, and `select (1_` gives `incomplete input`.
- The last message raised while one token is handled wins. `create table t (x) foo bar` gives `near "bar": syntax error`, which replaces the action's `unknown table option: foo`. `select 0 limit 0 union select #0` gives `LIMIT clause should come after UNION not before`.
- `/*` as the last two bytes of the text, or right before a NUL byte, is the two tokens `/` and `*`: `select 1 /*` gives `near "*": syntax error` at 10.
- A NUL byte ends the text: `select 1\x00 frm` parses with the tail 8, and `\x00select 1` has no statement.
- The tail of a statement that parses is after its semicolon, or at the end of the text. Empty statements before it are skipped: `;; select 1;` has the tail 12.
- Some messages look like syntax errors but come from actions, so a pass after the parser must raise them, not the parser: `syntax error after column name "a"` (a COLLATE or ASC/DESC in the column list of a CTE or a view), `unknown table option`, `ORDER BY clause should come after UNION not before`, and `variable number must be between ?1 and ?32766`.
- `select cast.* from t` is a syntax error at the `.`, and so is the same with `raise`, `current_date`, `current_time`, and `current_timestamp`. `select abort.* from t` parses.
- The limit on the parser's stack is a limit of the parse, not syntax: 2,494 nested parentheses give `Recursion limit`.
