# meyer as s4ql's SQL parser, measured against SQLite and rqlite/sql

Date: 2026-11-16. Tags: **V** = verified in this run (measured, or read in source, with the reference). **I** = inferred. **NV** = not verified.

Question: can s4ql use `github.com/sqlc-dev/meyer` as its SQL parser, as a dependency, instead of writing its own, and how does it compare with `github.com/rqlite/sql`? The alternative already agreed in principle is a parser of s4ql's own for all of SQLite's grammar, with a pass after it that rejects what [../roadmap.md](../roadmap.md) does not list ([sql-front-end.md](sql-front-end.md) §2.4 and §2.5). This note gives the measured facts. It does not decide. The study of `rqlite/sql` itself is in [rqlite-sql.md](rqlite-sql.md); here `rqlite/sql` only goes through the same measurements, on the same inputs.

**Setup.** SQLite 3.54.1 through the binding in `internal/sqlite`, with the options of `internal/sqlite/options.h`. meyer at commit `59b2dbf9b1872316bc6c6d3379b500df84db3e01` (tag v0.1.2, 2026-08-25), cloned from https://github.com/sqlc-dev/meyer. rqlite/sql at commit `5c0832fea6e181e88826f212bff8107fa0a6947e` (2026-10-16), cloned from https://github.com/rqlite/sql. Paths below without a repository prefix are relative to the meyer clone. SQLite sources: `sqlite-src-3540100.zip` (SHA-256 `387e7f6c…c71b`), `sqlite-src-3530400.zip` (SHA-256 `d18fa15a…232b`, the value in meyer's `parser/testdata/README.md`), and `sqlite-doc-3540100.zip`, all from https://sqlite.org/2026/.

**Method.** A throwaway Go test package inside the worktree, with a temporary `replace` of both modules to the clones and a temporary addition to `internal/sqlite/sqlite.go` that returns, for one `sqlite3_prepare_v2`, the result code, `sqlite3_errmsg`, `sqlite3_error_offset`, and the tail, plus `sqlite3_complete` and a bounded step loop. None of it is committed. For every input:

- **SQLite's verdict.** The input is prepared on a new `:memory:` connection. It is **rejected by the parser** if the message is `near "X": syntax error`, `unrecognized token: "X"`, or `incomplete input`. It is **parsed** if it prepares, or if it fails with another message after the parser has read all of it. "All of it" is tested by preparing it again with its last `;` removed and one space added: an error raised at the end of input leaves the tail at the end, and an error raised by a grammar action in the middle (for example `no such table` at `trigger_decl`, or `table t1 already exists`) leaves it short: `sqlite3RunParser` moves past each token before it checks for an error, and sets the tail where it stopped (`sqlite3.c` 187579-187742, the tail at 187723; measured: `create trigger tr after insert on nosuch begin select 1; end` stops at byte 46 of 60). Such a statement is **stopped**, and is settled where possible by preparing it again in a database that has the missing names: the TCL scripts and the documentation were replayed statement by statement on one connection per file, and a missing table or database named in the message was created and the statement prepared again, up to five times. What stays stopped is left out of the counts and counted on its own.
- **Each parser's verdict.** `parser.ParseString` of meyer and `sql.NewParser(…).ParseStatements` of rqlite/sql, each in a goroutine with a recover and a 10-second limit.
- **Agreement.** A parser agrees when it accepts what SQLite parsed and rejects what SQLite's parser rejected. The two ways to disagree are counted apart: **the parser accepts what SQLite rejects**, which is the dangerous kind for "reject, never reinterpret", and **the parser rejects what SQLite accepts**.

The scripts, the corpus, and the raw results are kept in the scratch directory and listed at the end.

## Short answer

- **Acceptance.** On 526,495 inputs that SQLite classifies, meyer agrees on 526,493. The two exceptions are one bug: meyer accepts `select cast.* from t`, and the same with `raise` and the three `current_*` keywords, which SQLite rejects with `near ".": syntax error`. rqlite/sql agrees on 511,342 (97.12%): it accepts 4,626 inputs that SQLite rejects (among them `SELECT SELECT 1`) and rejects 10,527 that SQLite accepts.
- **Errors.** On the 304,706 inputs that SQLite's parser and meyer both reject, meyer's message equals SQLite's on all of them, and its offset on all but 21, where SQLite gives -1. rqlite/sql's messages are never SQLite's (0 of 300,082), and it stops at SQLite's position on 81.4%.
- **Tokens.** meyer's lexer gives sqlite3GetToken's tokens on 532,571 of 535,220 inputs; the other 2,649 differ only because it has already done the `WINDOW`/`OVER`/`FILTER` decision that SQLite makes in `sqlite3RunParser`. The lexer is built into the parser; s4ql's tokenizer cannot feed it without a change to meyer.
- **The tree.** It keeps what s4ql's later layers need: raw text and quote style of every name, byte spans on every node, raw literals, and the optional keywords. But its shape was wrong where it could be checked: a new oracle (evaluate an expression in SQLite, and evaluate meyer's tree printed with every operation in parentheses) found that meyer reads `NOT x NOT NULL` as `(NOT x) NOT NULL`, where SQLite reads `NOT (x NOT NULL)`. So `where not v1 not null` gives different rows. The same cause makes meyer reject `'a' like not 'b' not null escape 'x'`, which SQLite accepts. meyer's own tests cannot see this: its round trip prints the tree back in the same token order.
- **Other differences.** An error raised by a grammar action is reported at once by meyer for digit separators, where SQLite lets a later syntax error win (`select 1_ 2`: SQLite `near "2"`, meyer `unrecognized token: "1_"`); meyer accepts nesting from 2,494 to 9,998 levels that SQLite rejects with `Recursion limit`; errors that need the schema (`table t1 already exists` in the middle of a statement) are out of its scope by design.
- **Robustness.** No panic and no hang in meyer on any input, in 5 minutes of its own fuzz target (24.7 million inputs) or in a 10-minute differential fuzz against SQLite. rqlite/sql panicked on 35 inputs and took more than 10 seconds on a compound select of 100,000 terms.
- **The project.** MIT, one author, six pull requests, the parser written in about four hours on 2026-07-30 in an agent loop, last commit 2026-08-25, pinned to SQLite 3.53.4. Between 3.53.4 and 3.54.1 the grammar rules and the tokenizer are the same; only action code differs. sqlc uses it on its main branch since 2026-08-01, in no release yet.

What the evidence favours, with the reason, is in §9. In short: meyer is far closer to SQLite than rqlite/sql on every measured property. The faults left in it were found only by tests of the kind s4ql would run, not by meyer's own, so s4ql needs those tests whatever it chooses, and the choice is then about who owns the code they check.

## 1. State of the project

### 1.1 Facts (V unless marked)

- **Licence:** MIT, "Copyright (c) 2026 The sqlc Authors" (`LICENSE`). The SQLite test SQL in `parser/testdata` is public domain (`parser/testdata/README.md`).
- **Author and history:** seven commits, all by "Kyle Gray", through six pull requests opened and merged by the GitHub account `kyleconroy`, which also opened the sqlc pull request below (`gh api repos/sqlc-dev/meyer/pulls/N`, read 2026-11-16). PRs 1 to 5 on 2026-07-30 and 2026-07-31, PR 6 on 2026-08-25. Tags v0.1.0, v0.1.1, v0.1.2, the last on 2026-08-25. 3 stars, 0 forks, no issue ever opened, and the six pull requests all merged (GitHub API). Repository created 2026-07-30.
- **How it was written:** `CLAUDE.md` is a development guide for an agent, with "the loop" (`cmd/next-test` picks a failing corpus case, the agent implements it, `-check-parse` records progress). PR 3, "Implement the lexer, AST and parser for the SQLite dialect", opened 19:20 and merged 23:14 UTC on 2026-07-30, adds 163,009 lines in 1,951 files (most of them corpus), and its description starts "Runs 'the loop' from `CLAUDE.md` to completion".
- **Size:** `parser/` 3,272 lines without tests, `ast/` 2,928, `token/` 581, `lexer/` 513.
- **Dependencies:** none; `go.mod` says `go 1.24` and has no `require`. `CLAUDE.md`: "`go.mod` must never gain a `require` line."
- **Tests:** `go test ./...` passes at the cloned commit. CI runs `go build`, `go vet`, and `go test -race ./...` (`.github/workflows/ci.yml`). A second workflow runs `cmd/difftest` weekly against a downloaded and compiled SQLite (`.github/workflows/difftest.yml`).
- **Use by sqlc:** sqlc pull request #4535, "sqlite: replace the ANTLR parser with meyer", merged 2026-08-01 (https://github.com/sqlc-dev/sqlc/pull/4535). sqlc's `go.mod` on its main branch requires `github.com/sqlc-dev/meyer v0.1.2`, and `internal/engine/sqlite/parse.go` calls `parser.Options{UpdateDeleteLimit: true}.ParseFile`, so sqlc parses with a grammar option that the default build of SQLite does not have, and uses the trivia and the spans to find `-- name:` comments. sqlc's latest release is v1.31.1 of 2026-04-22, before the switch, so no released sqlc uses meyer (GitHub API, 2026-11-16).
- **Tracking SQLite:** the pin is two constants with SHA-256 sums (`internal/sqlitesrc/sqlitesrc.go` 38, `cmd/regenerate-parse`), and the reference sources `parse.y`, `tokenize.c`, and `mkkeywordhash.c` are vendored under `internal/reference/`; they are byte-identical to those of 3.53.4 (V, `diff`). Three tests rebuild the keyword table, the 73 `%fallback` entries, and the list of nonterminals from them (`token/keywords_test.go` 42 and 78, `parser/grammar_test.go` 26), so a new pin with a grammar change fails. meyer has not moved to 3.54.x.

### 1.2 What 3.53.4 to 3.54.1 changes (V, `diff` of the two source zips)

- `src/tokenize.c` and `tool/mkkeywordhash.c`: identical.
- `src/parse.y`: two places, both C action code, no rule changed. The action of `seltablist ::= stl_prefix LP seltablist RP as on_using` now carries `INDEXED BY` and `NOT INDEXED` from a parenthesized single table (3.54.1 lines 777-814), and the rewrite of `x IN (c)` to `x = c` is skipped when `c` has a `COLLATE` (1519-1525).
- `tool/lemon.c`: one assertion added, the `-c` and `-r` options removed. `tool/lempar.c`: a comment.

So the language, the tokens, and the syntax messages are the same, and meyer, which runs no actions, is unaffected (I). The measurements here were all made against 3.54.1.

### 1.3 Its tests, and what its 21,326 cases compare (V)

- **The corpus.** 945 files `parser/testdata/*.test` with 21,326 cases and 42,257 statements, extracted by `internal/tclextract` from literal brace blocks of `do_execsql_test`, `do_catchsql_test`, and `do_eqp_test` in SQLite 3.53.4's TCL scripts (`parser/testdata/README.md`). Each statement was prepared with `sqlite3_prepare_v2` on an empty in-memory database by `internal/sqlitesrc/oracle/oracle.c`, compiled with no options, after splitting at `;` with `sqlite3_complete` as the shell does. The raw outcome is stored: `ok`, or code, offset, tail, and message. No metadata file lists a case as to do.
- **What a case checks** (`parser/parser_test.go` 86-123, `internal/testfile/testfile.go` 111-167): if any statement failed with a message of the syntax family, meyer must reject the case with the first such message and, when SQLite's offset is not -1, the same offset. Otherwise meyer must accept the case, except that a failure inside text that SQLite never reached is not counted.
- **What it does not check.** Only 156 of the 21,326 cases expect a rejection. Of the 42,257 statements, 11,046 prepared, 157 failed with a message of the syntax family, and 31,054 failed with another message on the empty database, most with `no such table`. Messages raised by grammar actions count as semantic, so meyer may accept them (`CLAUDE.md` 55). The README says it plainly: "There is no upstream parse-tree oracle".
- **The other checks.** A round trip (render the tree, parse again, compare the trees without spans and raw text), 5 hand-written snapshot files under `parser/testdata/ast/`, span invariants, 45 error cases taken from the oracle (`parser/errors_test.go`), the fuzz target `FuzzParse` (no panic, a `*parser.Error` with an offset inside the input, the round trip holds), and `cmd/difftest`, which mutates corpus SQL and the inputs of SQLite's `test/fuzzdata*.db` one token at a time and compares verdict, message, and offset with a live SQLite.
- **A limit of the round trip** (I, from `ast/render.go` 123-126): the renderer writes `X op Y` without adding parentheses, so the rendering has the same tokens in the same order as the input, and parsing it again builds the same tree whatever the tree's precedence is. The round trip finds dropped clauses and renderer bugs, not a wrong shape. §5.3 shows such a shape bug.

## 2. Acceptance against SQLite

### 2.1 The corpus

| Source | Records | How it was taken |
|---|---|---|
| SQLite 3.54.1 TCL scripts, `test/*.test` and `ext/*/*.test` | 41,937 blocks | brace blocks after `execsql`, `catchsql`, `db eval`, and the `do_*_test` commands; a wider set than meyer's extractor |
| SQLite's fuzz inputs, `test/fuzzdata1.db` to `fuzzdata8.db` (table `xsql`) | 35,934 scripts | read through the binding |
| sqllogictest, mirror https://github.com/gregrahn/sqllogictest: `test/select1-5.test`, `test/evidence/*`, one file from each `test/random/*` | 62,191 records | `statement` and `query` records |
| SQLite 3.54.1 documentation, `sqlite-doc-3540100.zip` | 491 blocks | `<pre>` blocks that start with a statement keyword |
| rqlite/sql's test files | 805 strings | string literals that start like a statement, read with `go/parser` |
| meyer's corpus, its test files, and its snapshot inputs | 21,326 cases, 151 strings, 5 files | as stored |
| `internal/sql/token`'s test files | 733 strings | all string literals |
| Hand-written quirks: §1.1 of [sql-front-end.md](sql-front-end.md), the slice-1 inputs of its §3, and about 300 more | 321 | in the test |

163,894 records in all. They were split into statements with `sqlite3_complete` as the shell does, trimmed of outer spaces, tabs, line feeds, carriage returns, and form feeds, and deduplicated: 155,516 statements; 10,820 with a NUL byte were left out, because the binding refuses them. Then up to three one-token mutations of each of the 138,971 statements that SQLite parsed, that were at most 3,000 bytes long, and that had at least two tokens (delete, duplicate, swap with the next, replace with or insert a token from a pool of all 147 keywords, punctuation, and literals, or cut after the token; PCG seed 1, 2): 379,704 new statements. Total 535,220.

**Independence from meyer.** A statement counts as *in meyer's corpus* if its text equals a statement of meyer's corpus, tests, or snapshots: 30,009 statements. The other 125,507 are *independent*: 63,979 from sqllogictest, the docs, rqlite/sql's tests, s4ql's token tests, and the quirks, which meyer's author did not test against (I, from meyer's tooling); 41,998 from the fuzz inputs, which `cmd/difftest` uses, so meyer has been tuned on them; 19,465 from the TCL scripts that meyer's extractor did not take; 65 from more than one of these. Mutations are counted by their base: 299,208 of independent bases, 80,496 of bases in meyer's corpus.

**Classified by SQLite:** 304,708 rejected by the parser, 60,266 prepared, 161,521 parsed and then failed (145,057 of these with `no such table`), 7,475 stopped and not settled, 1,095 that held a second statement after the split, 154 with no statement.

### 2.2 Results (measured against SQLite 3.54.1 through the binding)

| Inputs | Classified | meyer agrees | meyer accepts, SQLite rejects | meyer rejects, SQLite accepts | rqlite/sql agrees | rqlite/sql accepts, SQLite rejects | rqlite/sql rejects, SQLite accepts |
|---|---|---|---|---|---|---|---|
| Independent, not mutated | 125,056 | 125,056 | 0 | 0 | 122,628 (98.06%) | 92 | 2,336 |
| of which sqllogictest, docs, rqlite tests, token tests, quirks | 63,901 | 63,901 | 0 | 0 | 63,842 | | |
| of which fuzz inputs only | 41,675 | 41,675 | 0 | 0 | 39,562 | | |
| of which TCL only | 19,416 | 19,416 | 0 | 0 | 19,160 | | |
| In meyer's corpus, not mutated | 29,865 | 29,865 | 0 | 0 | 29,486 | 30 | 349 |
| Mutations of independent bases | 293,280 | 293,280 | 0 | 0 | 283,411 | 3,762 | 6,107 |
| Mutations of bases in meyer's corpus | 78,294 | 78,292 | 2 | 0 | 75,817 | 742 | 1,735 |
| **All** | **526,495** | **526,493** | **2** | **0** | **511,342 (97.12%)** | **4,626** | **10,527** |
| All valid UTF-8 | 496,222 | 496,220 | 2 | 0 | 482,757 | 4,420 | 9,045 |

Three targeted generators added what the corpus lacked (same classification, measured against SQLite 3.54.1):

- **Keywords as names.** 96 statement templates with a name slot (`select K.* from t`, `create table t (x K)`, `pragma x = K`, `select * from t window K as ()`, and so on), each filled with all 147 keywords, the non-keywords `rowid`, `strict`, `int`, `true`, `false`, the seven join words, and quoted forms: 16,491 classified statements (two templates and two words appear twice). meyer agrees on 16,484; the 7 others are all `select K.* from t` with `K` one of `raise`, `cast`, `current_date`, `current_time`, `current_timestamp`. rqlite/sql agrees on 13,259; it accepts 170 that SQLite rejects in 39 templates (for example `create table null (x)`, `pragma x = select`, `select cast(1 as left)`) and rejects 3,062 that SQLite accepts.
- **Shapes of slice 1 and the next slices.** A random generator of `create table` (one to three columns, eleven types, nineteen column constraints, twelve forms of table options), `insert` (with conflict clauses, `default values`, `select`, `returning`, upserts), `select` (with `where`, joins, `group by`, `order by`, `limit`), `update`, `delete`, the transaction statements, and `drop table`, over names that mix quoting styles, keywords, and non-ASCII letters, and integer literals at the edges (PCG seed 7, 11): 40,000 statements, plus 71,841 one-token mutations, prepared on a schema where the names resolve. Classified: 34,934 generated and 67,894 mutated. meyer agrees on all 102,828. rqlite/sql accepts 472 that SQLite rejects (for example `delete from t limit 1`, `insert into t default values on conflict do nothing`, `select temp.t1.* from t1`) and rejects 21,018 that SQLite accepts (for example `begin transaction s`, `create table 'z' (…)`, `create table t (true integer)`, `insert into t values (1_000)`, `update strict set …`).
- **The shape of expressions** (§5.3): of 300,000 random constant expressions, 16 that SQLite accepts are rejected by meyer, all of the form `… LIKE NOT x NOT NULL ESCAPE …`.

### 2.3 The disagreements of meyer, by cause

1. **`K.*` for a keyword that starts an expression.** `select cast.* from t`, `select raise.* from t`, and `select current_date.* from t` (also `current_time`, `current_timestamp`, and after another column, `select x, cast.* from t`) are accepted by meyer. SQLite: `near ".": syntax error` at offset 11, 12, and 19. Cause (V, `parser/parse_select.go` 169): the result column takes `nm DOT STAR` when the token is in the class `nm`, which includes the `%fallback` keywords; but `%fallback` applies only where the parser state has no action for the token, and at the start of a result column `CAST`, `RAISE`, and `CTIME_KW` begin an expression, so SQLite shifts them as themselves. `select abort.* from t` is accepted by both, as it should be. Kind: meyer accepts what SQLite rejects. Corner: no slice needs `t.*` with such a table name.
2. **`NOT x NOT NULL`** (§5.3): `select 'a' like not 'b' not null escape 'x'` is accepted by SQLite and rejected by meyer with `near "escape": syntax error`. Kind: meyer rejects what SQLite accepts; the same cause also changes the tree of accepted statements, which is worse.

No other acceptance difference of meyer was found in any measurement of this note.

### 2.4 The disagreements of rqlite/sql, grouped (measured; the causes are studied in [rqlite-sql.md](rqlite-sql.md))

rqlite/sql accepts what SQLite rejects in these groups, among others: a `SELECT` keyword where an expression starts, read as a subquery without parentheses (3,570 of the 4,626, for example `SELECT SELECT 1`, `INSERT INTO t VALUES(select 1)`, inside triggers and views too); a trailing comma in a list (`a IN ('x' ,)`); `ORDER BY` and `LIMIT` on `DELETE`, which the default build of SQLite does not have; an alias after the table of `UPDATE` and `DELETE` (`UPDATE t1 t1 SET …`); join keywords and `null` as names or types (`CREATE TABLE null(a)`, `CREATE TABLE t(c0 left REAL)`); digit separators and number tokens that SQLite calls unrecognized (`SELECT 1_`, `SELECT 1._5`, `SELECT 5a`); `/*` at the end of input; a vertical tab after `select`. It rejects what SQLite accepts mostly because its list of keywords that may be names is not SQLite's (`temp.sqlite_master`, `DETACH ?`, `CREATE TABLE t(rowid TEXT)`, `UPDATE savepoint SET …`), join forms (`OUTER LEFT NATURAL JOIN`), `ORDER BY` inside aggregates, virtual table arguments, `CREATE TABLE t(a),STRICT`, single-quoted names, `begin transaction s`, and digit separators in valid numbers (`1_000`).

### 2.5 What matters for the slices

- **Slice 1** (`create table … (v1 int primary key) strict, without rowid`, `select *`, single-row `insert` with a column list, `drop table`): meyer agreed on every generated and mutated statement of these shapes; the one-column `create table` with keyword names agreed in every template. rqlite/sql rejects valid slice-1 shapes with names that SQLite accepts (`create table 'z' …`, a column named `true` or `rowid`, a table named `strict`), and accepts `create table null (…)`.
- **Next slices** (`where`, more column types, `update`, `delete`, joins, transactions): meyer's `NOT x NOT NULL` bug is inside the expression grammar of `where`; its `K.*` bug is in the result columns of a join. rqlite/sql's `SELECT SELECT`, its alias after `UPDATE` and `DELETE`, its `DELETE … LIMIT`, and its keyword list all fall inside these slices.
- **Corners**: deep nesting, digit separators, `#N`, virtual table arguments, window frames, `RAISE`.

## 3. The tokenizer

- **Comparison** (measured against SQLite 3.54.1 through `sqlite.Tokenize`, on all 535,220 statements, the quirks of [sql-front-end.md](sql-front-end.md) §1.1 included): meyer's `lexer.Lex`, with spaces and comments dropped on both sides, gives the same kinds and byte ranges as sqlite3GetToken on 532,571; the other 2,649 differ only in that `WINDOW`, `OVER`, or `FILTER` is `ID` in meyer where sqlite3GetToken says `WINDOW`, `OVER`, or `FILTER`. That is by design: `lexer.resolveWindowKeywords` (`lexer/lexer.go` 87-) does the decision of `analyzeWindowKeyword` and its two siblings, which SQLite makes in `sqlite3RunParser`; whether meyer decides as the parser does shows in the acceptance results, where no disagreement involves these words. No other difference. As a control, `internal/sql/token` gave the same tokens as sqlite3GetToken on all 535,220.
- **Can s4ql's tokenizer feed meyer's parser?** Not without changing meyer. `newParser` calls `lexer.Lex` itself (`parser/parser.go` 266-270), the parser type is unexported, and no entry point takes tokens. The two token sets match by name: meyer's kinds carry SQLite's `TK_` names (`token/token.go`), as do `internal/sql/token`'s. A copy that s4ql owned could swap the lexer for `internal/sql/token` plus the window decision (I).
- **Bytes, not runes:** meyer's lexer works on bytes and reads a NUL as the end, as SQLite does (`lexer/lexer.go` 23-24, 138-149). rqlite/sql reads runes through `io.RuneReader` (`scanner.go` 418-439), and its offsets count runes.

## 4. Errors

### 4.1 What an error is

- **meyer:** `*parser.Error{Message, Offset, SQL}`; `Message` is SQLite's text, `Offset` the byte offset or -1, and `Error()` adds `line:column: ` in front (`parser/parser.go` 47-62). The three messages are those of SQLite's parser, plus `Recursion limit` past 10,000 levels (260). `ParseStatement` returns `expected exactly one statement` with offset -1 for zero or several statements (187-196), which is not a SQLite message. Internally errors are panics of a private type, recovered at each entry point; any other panic is passed on (132-145).
- **rqlite/sql:** `*sql.Error{Pos, Msg}` with messages such as `expected expression, found 'EOF'` (`parser.go` 4028-4052); `Pos.Offset` counts runes.

### 4.2 Measured on the inputs that SQLite's parser rejects (against SQLite 3.54.1)

| Inputs rejected by SQLite and the parser | meyer: same message | meyer: same offset | rqlite/sql: same message | rqlite/sql: same position |
|---|---|---|---|---|
| Independent, not mutated | 15,303 of 15,303 | 15,290 | 0 of 15,211 | 13,695 (90.0%) |
| In meyer's corpus | 246 of 246 | 238 | 0 of 216 | 156 |
| Mutations | 289,157 of 289,157 | 289,157 | 0 of 284,655 | 230,378 |
| **All** | **304,706 of 304,706** | **304,685** | **0 of 300,082** | **244,229 (81.4%)** |
| Shapes of the slices (§2.2) | 64,197 of 64,290 | | 0 | 32,126 of 63,818 |

"Same position" for rqlite/sql means its error position, turned into a byte offset, equals `sqlite3_error_offset`, or is at the end of the input for `incomplete input`. The 21 offsets of meyer that differ are all errors for which SQLite reports -1: `#N` (`SELECT #1`: SQLite -1, meyer 7) and a misplaced digit separator (`SELECT 1_`, `SELECT 0xFF__EF`). SQLite raises both from a grammar action, without `%T`, so they carry no offset; meyer gives the token's offset, and its own harness skips offsets of -1 (`parser/parser_test.go` 118). For `incomplete input` SQLite's offset is -1 and meyer's is the length of the input.

### 4.3 The hard cases

- **A digit separator error before a syntax error.** SQLite checks digit separators in the action of `term ::= QNUMBER`, which runs when the next token arrives, and then reports a syntax error on that token if there is one; the last message wins (`sqlite3.c` 37103-37104). meyer fails at once (`parser/parse_expr.go` 754). Measured: `select 1_ 2` gives `near "2": syntax error` at 10 in SQLite and `unrecognized token: "1_"` in meyer; `select (1_` gives `incomplete input` and `unrecognized token: "1_"`. All 93 message differences in the slice shapes are this one cause. meyer already defers the other action error of this kind, `#N` (`parser.go` 351-366, `parse_expr.go` 679-690): `select #1 2` matches.
- **Several misplaced separators in one number.** SQLite reports the last, with the number half rewritten in place: `select 1__2__3` gives `unrecognized token: "12_2__3"`; meyer reports the first, `"1__2__3"`. `select 1_2__3` gives `"122__3"` in both.
- **Errors raised by grammar actions that need the schema.** Out of meyer's scope: "No semantic analysis … Errors SQLite raises post-parse are out of scope" (`PLAN.md` 431-432). With `t1` existing, `create table t1 (v1 int primary key) 'abc` gives `table t1 already exists` at 13 in SQLite; meyer, which has no schema, gives `unrecognized token: "'abc"` at 37. To give SQLite's message, s4ql would have to raise such errors at the token where SQLite's action runs, so a pass after a complete parse is not enough for statements with two faults (I).
- **Action errors that need no schema.** meyer accepts them all, as its corpus allows (`CLAUDE.md` 55): `unknown table option: foo`, `ORDER BY clause should come after UNION not before`, `unknown join type` (`t left right join u`), `variable number must be between ?1 and ?32766` (`select ?0`), `Expression tree is too large (maximum depth 1000)`, `too many terms in compound SELECT`, `unknown datatype for t.v1: "foo"`, `PRIMARY KEY missing on table t`. A pass after parsing has to raise these itself, at the right moment when a statement has two faults (§2.1 of [sql-front-end.md](sql-front-end.md)).
- **Two action errors at one token.** The differential fuzz (§6) found `seleCt 0 limit 0 union seleCt#0`: SQLite raises the syntax error for `#0` and then `LIMIT clause should come after UNION not before`, both while the end of input is handled, and reports the second; meyer reports `near "#0": syntax error`. Inputs with two faults of the syntax family agreed wherever they occurred; NV how many of the inputs have two faults, because mutations were made only of statements that SQLite parsed.

## 5. The syntax tree

### 5.1 What it keeps (V, read in `ast/` and checked with `cmd/debug-parse`)

- **Names:** `ast.Ident{Span, Name, Raw, Quote}`, with `Name` dequoted, `Raw` exactly as written, and `Quote` one of none, `"`, `` ` ``, `[`, `'` (`ast/expr.go` 9-27). `create table "T1" …` keeps `Raw: "\"T1\""`, which `table "T1" already exists` needs.
- **Positions:** every node embeds `Span{Start, Stop}`, byte offsets into the input. So the exact text of any node can be sliced from the input, whatever the node's fields keep.
- **Table options:** `CreateTableStmt.Options` is a list of `TableOption{Without, Name *Ident}` in source order, with any name, as the grammar allows; `without "rowid"` keeps the quoted raw text, which SQLite refuses with `unknown table option`, so a pass can refuse it too. `Temp`, `IfNotExists`, the schema of the name, and `Select` (for `as select`) are fields.
- **Types:** `TypeName{Name, Args, Raw}`, with `Name` the type's tokens as written joined by one space, and `Raw` the source span: `int` and `integer` and `INT` stay apart.
- **Literals:** `Literal{Kind, Value, Raw}`; `Raw` is as written, `Value` is the dequoted string or, for a number with digit separators, the number without them (`0x1_0` has `Value: "0x10"`, `Raw: "0x1_0"`). A sign is a `UnaryExpr`, not part of the literal. Numbers are not converted, so range checks (`hex literal too big`) are left to the caller.
- **Optional keywords:** `IfExists`, `Temp` (one bool for `TEMP` and `TEMPORARY`), `HasTransaction`, `HasType`, `Keyword` (`COMMIT` or `END`), `ColumnKeyword`, `HasDatabase`, `HasAs`, explicit `ASC` (`SortAsc` apart from the default), `Limit.Comma`, `NullCheckExpr.Test` (`ISNULL`, `NOTNULL`, `NOT NULL` apart), `IsExpr.Distinct`, `InsertStmt.Replace` apart from `OrConflict`, the words of a join (a comma join has none, so it can be told from `JOIN`), parentheses (`ParenExpr`).
- **What it folds:** `=` and `==` are one operator, so are `<>` and `!=`, and `TEMP` and `TEMPORARY`; the word `JOIN` is stored upper case. The span keeps the text of each.

### 5.2 Can a pass over it reject everything unlisted?

For slice 1 the tree has every field the pass needs: the statement type, `Temp`, `IfNotExists`, the schema, the quote style and raw text of every name, the column count, the type's raw text and its arguments, every column and table constraint with its order, conflict clause and `AUTOINCREMENT`, the options in order with their raw text, `Select`, the insert's column list, row count and `DefaultValues`, `Upserts`, `Returning`, `IfExists` of `drop`. Nothing in a slice-1 shape is lost. Two hazards (I): the tree is a set of Go structs with many optional fields, so a pass that lists what it checks lets an unchecked field through; a check that every field it does not allow is the zero value, done by reflection over the node types, closes that, and also fails when a new version of meyer adds a field. And the tree must have the right shape, which §5.3 shows is not given.

### 5.3 Does it reinterpret? A check of the tree's shape (measured against SQLite 3.54.1)

meyer's README: "There is no upstream parse-tree oracle". For expressions there is one: SQLite can evaluate an expression. The check generates a constant expression from all operators and their precedence levels (`OR` to `COLLATE`, `IS [NOT] [DISTINCT FROM]`, `[NOT] BETWEEN`, `[NOT] IN`, `[NOT] LIKE … ESCAPE`, `ISNULL`, `NOTNULL`, `NOT NULL`, `CASE`, `CAST`, `->`, unary operators, functions) over constants of every type, without parentheses most of the time; SQLite evaluates `select <expr>`; meyer parses it, and its tree is printed with every operation in parentheses; SQLite evaluates that too. A tree of the wrong shape gives a different value for some inputs.

300,000 expressions (PCG seed 42, 43): SQLite failed to prepare 23,962 (21,109 with `near "X": syntax error`, 1,905 with `incomplete input`, 948 with `wrong number of arguments to function GLOB()` for `GLOB … ESCAPE`), and these were skipped; an error at step, such as a malformed JSON argument, counts as a value and is compared. meyer rejected 16 that SQLite accepts (§2.3). 276,022 compared: 275,572 equal, **450 differ**, and every one of the 450 contains `NOT … NOT NULL`. The smallest: `select not 7 not null` is 0 in SQLite and meyer's tree is `((NOT 7) NOT NULL)`, which is 1.

Cause (V, `parser/parse_expr.go` 99-104 and 182-210): meyer parses the operand of prefix `NOT` at precedence `precNot + 1`, and gives `expr NOT NULL` the precedence of `NOT`, so the operand stops before `NOT NULL` and the outer loop applies `NOT NULL` to `NOT x`. In SQLite's grammar both are at the level of `%right NOT`; after `NOT expr` with `NOT` as the next token, Lemon resolves the shift-reduce conflict at equal precedence by the right associativity, and shifts, so `NOT NULL` binds to `x` (I, from `parse.y` and Lemon's rules; consistent with all 450 values). In a table (measured): with one row where `v1` is NULL, `select count(*) from t3 where not v1 not null` gives 1 in SQLite, and the shape of meyer's tree written out, `where (not v1) not null`, gives 0. That is a reinterpretation: the statement is accepted and means something else.

The check covers expressions only. Statements (join order, compound selects, `ON` placement, the `LIMIT x, y` swap) have no such oracle here (NV); for those, two independent parsers compared with each other would be one.

### 5.4 Fit with the restrictions

- **Double-quoted strings:** `"a"` in an expression is `Ident` with `Quote: '"'`, never a string literal, so with `SQLITE_DQS=0` it is a name and name resolution decides, as in SQLite. A rule "a double-quoted string is rejected" becomes "a double-quoted name that does not resolve is an error", which the resolver sees.
- **`[brackets]` and backticks:** `Quote` is `[` or `` ` `` on the `Ident`, wherever a name can be. A pass can reject them.
- **Single-quoted names:** in a name position `'l'` is an `Ident` with `Quote: '\''` (`create table 'l' …`, an alias `select 1 'x'`, `collate 'nocase'`), and in an expression position it is a `Literal` of kind string. SQLite also reads a string as a column name in a key column list: measured, `create table t2 (a, primary key ('a'))` makes an index on column `a`, and a second insert of 1 fails with code 1555. meyer keeps it a `Literal` of kind string there, so a pass sees it and can reject it.

## 6. Robustness

- **meyer's own fuzz target** (`go test ./parser -fuzz FuzzParse -fuzztime 300s` in the clone, macOS, Go 1.27.4): 24,666,983 inputs, no failure, 938 new interesting inputs.
- **A differential fuzz against SQLite** (`FuzzMeyerAgainstSQLite`, in the throwaway package; seeds: the quirks, the keyword templates, four larger statements): meyer must accept what SQLite parses and reject the rest with the same message and offset; the two known causes are skipped. The first run stopped after 2 minutes 45 seconds (15 million inputs) on `seleCt?0/*`, an error of the harness: a space after `/*` makes a comment, so the completeness test did not hold for an input that ends in `/*`; fixed by counting such inputs as stopped. The second run stopped after 2 minutes 30 seconds on `seleCt 0 limit 0 union seleCt#0`, where SQLite's last message is an action error that overwrote a syntax error (§4.3); such inputs were then skipped too. The third run: 10 minutes, 87,290,959 inputs, no failure. So beyond the known causes, the fuzz found no difference of verdict, message, or offset, and no panic or hang.
- **Panics and hangs on the corpus:** meyer none of 535,220. rqlite/sql panicked with `assert failed` on 35 (for example `CREATE TABLE abc(a, b, c, CONSTRAINT two CHECK (b!=c), d)` and `CREATE TABLE t(a,PRIMARY KEY(a),)`), no hang.
- **Deep nesting** (measured against SQLite 3.54.1; times on macOS, arm64): SQLite reports `Recursion limit` from 2,494 nested parentheses, 2,495 `NOT`s, and 499 nested subqueries; meyer from 9,999, 9,999, and 5,000 (`maxDepth = 10000`, `parser/parser.go` 251, chosen above SQLite's limit on purpose, as its comment says). Between the two limits meyer accepts what SQLite rejects. rqlite/sql stops at 1,000 levels with `parse tree is too deep (maximum depth 1000)`, below SQLite's limit, so it rejects 1,000 parentheses that SQLite accepts. A million levels took meyer at most 180 ms, with no stack exhaustion. A compound select of 100,000 terms took meyer 85 ms and rqlite/sql more than 10 seconds (10,001 terms: 1.2 s).

## 7. The code, as a thing to depend on

- **How it is written:** by hand, recursive descent, precedence climbing for expressions (`parser/parse_expr.go` 20-38), written in an agent loop against the corpus (§1.1). 122 parser methods. Every function cites the `parse.y` rules it implements, and a test checks that every nonterminal of `parse.y` is named somewhere in `parser/` (`parser/grammar_test.go` 26-60). That test checks mention, not correspondence.
- **Can a reader check it against `parse.y`?** Rule by rule for the shape of each production, yes: the comments quote the rules, and the AST types quote them too. What a reader cannot check by reading is the part that Lemon computes and meyer encodes by hand: which `%fallback` keyword is a name in which parser state, how each shift-reduce conflict is resolved, and when a token is shifted before the parser knows the rule fails. Both bugs found here are of this kind: the `%fallback` set used in a state where the keyword has its own action (§2.3), and a precedence tie resolved the wrong way (§5.3). The package documentation names this habit itself ("SQLite's LALR parser shifts a token as soon as some rule can begin with it", `parser/parser.go` 20-26), and the difftest of PR 3 found nine more of this shape (PR 3 description).
- **What is surprising for a library on the application's path:** errors are panics inside, recovered at the entry points; the whole input is tokenized before parsing starts; `Parse` takes a `context.Context` and ignores it ("is not consulted", `parser/parser.go` 109-114); the recursion limit is deliberately not SQLite's; `ParseStatement` has a message of its own. None of these is a correctness fault for s4ql. Parsing is fast: the 535,220 statements took meyer 3.6 s in all.

## 8. What `rqlite/sql` showed on the same inputs

All numbers are in §2 to §6. In short, measured against SQLite 3.54.1: 97.12% agreement on acceptance, 4,626 accepted inputs that SQLite rejects, no message equal to SQLite's, 81.4% of error positions equal, 35 panics, a hang on a large compound select, a recursion limit below SQLite's. Its tree and its project are studied in [rqlite-sql.md](rqlite-sql.md).

## 9. Ways to use meyer, and the comparison

### 9.1 The options

1. **meyer as a dependency, as it is.** What exists today, closest to SQLite of anything measured. The cost in trust: the two bugs above, the digit separator order, and the recursion limit stay until upstream changes them, and upstream follows sqlc's needs (it already carries an option for a grammar the default SQLite lacks, which sqlc turns on); the pin moves on meyer's schedule, not s4ql's; third-party code sits between the application's SQL and the engine, which the binding was written to avoid for SQLite itself (`internal/sqlite/README.md`, "Why our own binding"); and s4ql must run its own differential tests on every version anyway, because meyer's own tests did not find these bugs.
2. **A copy that s4ql owns** (MIT allows it with the notice), checked against s4ql's pinned SQLite with the harness of this note. s4ql fixes the bugs, can put `internal/sql/token` in front, adds the tree-shape oracle and the slice generator to its tests, and moves the pin with its own SQLite. The cost in trust: about 7,300 lines that s4ql did not write must be read and owned like its own, and the copy is no longer an independent second implementation to compare against.
3. **A reference and a source of tests only.** s4ql writes its own full-grammar parser; meyer's corpus (21,326 cases from SQLite's scripts), its 45 error cases, its difftest method, and meyer itself serve as a second parser to compare trees with. Two independent parsers that are compared find what one alone and its own tests cannot, which is how both meyer bugs here were found (one by comparison with SQLite's acceptance, one by comparison with SQLite's evaluation). The cost: s4ql builds the whole grammar itself, and meyer gives no code to production.

### 9.2 Side by side

| Property | meyer | rqlite/sql | A parser of s4ql's own |
|---|---|---|---|
| Licence, dependencies | MIT; none | MIT; `go-test/deep`, used only by its tests | s4ql's |
| Age, people | since 2026-07-30; one author; 7 commits | copyright 2020 (`LICENSE`); see [rqlite-sql.md](rqlite-sql.md) | s4ql |
| SQLite version | 3.53.4 (same grammar and tokens as 3.54.1) | none tracked | s4ql's pin |
| Origin of the grammar | hand port of all of `parse.y` | hand-written, not from `parse.y` | to be decided |
| Agreement with SQLite on acceptance, 526,495 inputs | 526,493 (99.9996%) | 511,342 (97.12%) | to be measured by the same harness |
| Accepts what SQLite rejects | 2 in the corpus; 7 in the keyword templates (one cause) | 4,626; 170; 472 in the slice shapes | to be measured |
| Rejects what SQLite accepts | 0 in the corpus; 16 in the expression check (one cause) | 10,527; 3,062; 21,018 | to be measured |
| Same error message, 300,000+ rejections | 100%; 93 of 64,290 differ in the slice shapes (one cause) | 0% | can be exact for syntax errors (§2.2 of [sql-front-end.md](sql-front-end.md)) |
| Same error position | 99.99% (the 21 others: SQLite gives -1) | 81.4% | as above |
| Tokens equal to sqlite3GetToken, 535,220 inputs | all, apart from the window keywords, decided early by design | not measured | `internal/sql/token`: all |
| Takes s4ql's tokens | no | no | yes |
| Raw names, quote style | yes | no raw text ([sql-front-end.md](sql-front-end.md) §2.3) | by design |
| Positions on nodes | byte span on every node | see [rqlite-sql.md](rqlite-sql.md) | by design |
| Optional keywords, raw literals | yes (§5.1) | see [rqlite-sql.md](rqlite-sql.md) | by design |
| Tree shape checked against SQLite's evaluation | 450 of 276,022 differ (one cause, `NOT x NOT NULL`) | not measured | to be measured |
| Panics, hangs on the corpus | none | 35 panics | to be measured |
| Recursion limit | 9,999 (SQLite: 2,494) | 1,000 | can be set to SQLite's |
| Action errors without schema (`unknown table option`, …) | not raised (by design) | some, with its own messages | a pass after parsing, or in the parser |

### 9.3 What the evidence favours

Not option 1, and not rqlite/sql in any role but a source of test inputs. rqlite/sql fails "reject, never reinterpret" outright: it accepts thousands of inputs that SQLite rejects, some of them in slice shapes, and none of its messages is SQLite's. meyer is close enough that its remaining faults were invisible to its own tests, but one of them changes the meaning of an accepted `where` clause, which is the failure the roadmap forbids. A dependency would leave the fix and the pin outside s4ql.

Between options 2 and 3 the evidence does not decide by itself. Both end with code that s4ql owns, checked by the same harness: the differential acceptance and message test of this note, the slice generator, the one-token mutator, the keyword templates, and the evaluation oracle for the tree. The reason to prefer one: option 2 starts from code with measured agreement of 99.9996% on acceptance and 100% on messages, and its known faults are few, each found and located (§10); option 3 keeps meyer as an independent second implementation, which is what found the tree bug, and costs nothing in production trust. Whichever is chosen, that harness is the part that carries the trust, and it does not depend on who wrote the parser.

## 10. Findings that could be reported upstream later

Nothing was reported. Each with the smallest input, what meyer does, and what SQLite 3.54.1 does (3.53.4 has the same grammar, §1.2; not run on 3.53.4):

1. **`NOT x NOT NULL` builds the wrong tree.** `SELECT NOT 7 NOT NULL`: meyer's tree is `NOT 7` under `NOT NULL`; SQLite evaluates it to 0, which is `NOT (7 NOT NULL)`. Also `SELECT 'a' LIKE NOT 'b' NOT NULL ESCAPE 'x'`: meyer `near "ESCAPE": syntax error` at 33; SQLite accepts it. Cause: `parser/parse_expr.go` 103 parses the operand of `NOT` at `precNot + 1`, while `NOT` is `%right` in `parse.y`.
2. **`cast.*` and friends are accepted.** `SELECT cast.* FROM t`: meyer accepts; SQLite `near ".": syntax error` at 11. Also `raise`, `current_date`, `current_time`, `current_timestamp`. Cause: `parser/parse_select.go` 169 uses `token.IsName`, which includes `%fallback` keywords that have their own action at the start of a result column.
3. **The digit separator error is not deferred.** `SELECT 1_ 2`: meyer `unrecognized token: "1_"` at 7; SQLite `near "2": syntax error` at 10. `SELECT (1_`: meyer `unrecognized token: "1_"`; SQLite `incomplete input`. Cause: `parser/parse_expr.go` 754 fails at once, where `#N` uses the deferred error.
4. **Several separators in one number.** `SELECT 1__2__3`: meyer `unrecognized token: "1__2__3"`; SQLite `unrecognized token: "12_2__3"` (SQLite reports the last error, from its partly rewritten buffer).
5. **Offsets of action errors.** `SELECT #1` and `SELECT 1_`: meyer offset 7; SQLite -1.
6. **Recursion limit.** `SELECT ` followed by 2,494 `(`, `1`, and 2,494 `)`: SQLite `Recursion limit`; meyer accepts up to 9,998 levels. Deliberate in meyer (`parser/parser.go` 244-251), so a difference to document rather than a bug.

For rqlite/sql, the panics (`CREATE TABLE t(a,PRIMARY KEY(a),)` gives `assert failed`), `SELECT SELECT 1` accepted, and the hang on a compound select of 100,000 terms are listed here for [rqlite-sql.md](rqlite-sql.md) to report in its own form.

## Not verified

- Behaviour against SQLite 3.53.4 itself; all runs used 3.54.1.
- How many of the inputs have two independent faults (§4.3).
- The shape of trees beyond expressions (§5.3).
- Whether CI of meyer's difftest workflow runs green at the cloned commit (not visible without the Actions history; NV).
- The statements that stayed stopped (7,475) and the 1,095 inputs with a second statement were not compared.

## Where the material is

In the scratch directory `/private/tmp/claude-502/-Users-maragubot-Developer-s4ql--claude-worktrees-object-store/f1343515-adca-4ca0-9eb0-f507768f8fc7/scratchpad/meyer-research/`: `src-meyer/` and `src-rqlite/` (the clones), `src-sqlite/` (the three zips, unpacked), `corpus/` (the extracted records, the fuzz databases, the sqllogictest files), `scripts/` (the extractor, the analysis scripts, the patch to the binding, the copy of the throwaway test package, and the original `go.mod` and `go.sum`), and `out/` (the per-statement results `results.tsv`, the disagreement lists, and the outputs of the token, keyword, slice, and shape checks).
