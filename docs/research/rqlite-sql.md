# rqlite/sql as s4ql's SQL parser

Date: 2026-11-16. Tags: **V** = verified in this run (measured, or read in source at the commit below). **I** = inferred. **NV** = not verified.

Question: can s4ql use `github.com/rqlite/sql` as its SQL parser, as a dependency, instead of a parser of its own for all of SQLite's grammar? This note gives the measured facts for that choice. It does not decide. [sql-front-end.md](sql-front-end.md) §2.3 has a short paragraph on rqlite/sql that was read from source without running it; this note checks it and goes further.

No contact was made with the rqlite/sql project. The bugs found are written down in §9 in a form that could be reported upstream later, if Markus agrees.

**Setup.**

- rqlite/sql cloned at its head, commit `5c0832fea6e181e88826f212bff8107fa0a6947e` (2026-10-16, "Merge pull request #117"). File and line references below are to that commit.
- SQLite 3.54.1, the vendored amalgamation with `internal/sqlite/options.h`, through the project's binding. "Measured" below means: measured against SQLite 3.54.1 through the binding, with Go 1.27.4 on macOS.
- The binding was given two temporary functions for the run and then reverted: one that prepares a statement and returns the extended code, `sqlite3_errmsg`, `sqlite3_error_offset`, and the offset of the tail; and `sqlite3_complete`. The binding's `s4ql_prepare` calls `sqlite3_prepare_v3` with no flags, which behaves as `sqlite3_prepare_v2`.
- A throwaway test package in the module (not committed, and `go.mod` reverted) imported rqlite/sql through a `replace` to the clone. Its parts: corpus extraction, the comparison, a check of meaning through `EXPLAIN`, tree dumps, keyword sets, nesting probes, and a fuzz target.

**How to repeat.** Build the corpus of §2.1, then for each statement: prepare it with SQLite on a fresh `:memory:` connection; parse it with `sql.NewParser(strings.NewReader(s)).ParseStatement()` and require that a second `ParseStatement` returns `io.EOF`; tokenize it with rqlite/sql's `Scanner` and with `internal/sql/token`; record everything. The numbers below are counts of those records.

## Short answer

- **The project.** MIT, one active maintainer (Philip O'Toole), 233 commits, no tags or releases, no open issues or pull requests, go 1.17 in `go.mod`, one test-only dependency. Its tests are hand-written expectations (76.4% statement coverage); it has no fuzz test and no automatic comparison with SQLite. rqlite uses it only to rewrite statements that call `random()` or time functions and to detect `RETURNING` and `EXPLAIN`, and sends any statement it cannot parse to SQLite unchanged, after recovering from parser panics. So little in rqlite depends on it rejecting what SQLite rejects.
- **Acceptance.** Over 67,656 statements, most of them from SQLite's own test scripts: of 53,965 that SQLite parses, rqlite/sql rejects 1,748 (3.2%); of 13,691 that SQLite rejects as a syntax error, rqlite/sql accepts 154 (1.1%) and panics on 3. On SQLite's test scripts alone, it accepts 47 of 371 syntax errors (12.7%).
- **Reinterpretation.** Some accepted inputs change meaning, the dangerous kind for "reject, never reinterpret": `select 1_000` becomes `select 1 AS "_000"`, `select 1x` becomes `select 1 AS "x"`, `drop table null` drops a table named `null`, and `select true from t` is the literal TRUE even where SQLite reads the column `"true"`. Its `LIMIT 1, 2` puts the offset in the field named `LimitExpr`, and its `String()` then prints `LIMIT 1 OFFSET 2`, which means the opposite.
- **Tokenizer.** Built into the parser; s4ql's tokenizer cannot feed it without a fork. Token boundaries match SQLite's in 99.6% of statements; the differences are digit separators, a number followed by letters, `$::name` and `#name`, `/*` at the end, U+00A0 and other Unicode spaces, and the byte order mark. Five words are keywords in rqlite/sql and names in SQLite: `ROWID`, `STORED`, `STRICT`, `TRUE`, `FALSE`.
- **Errors.** Go-style, `1:8: expected expression, found 'EOF'`, with a position in runes, not bytes. Where SQLite says `near "X": syntax error`, rqlite/sql's position is the start of X in 93.3% of 12,801 cases; for `incomplete input` it names EOF in 97.3%. The failing token itself is not in the error; only the position and a message are.
- **Tree.** It keeps positions of keywords and names, `IF EXISTS`, `STRICT`, `WITHOUT ROWID`, `int` against `integer`, and literals mostly as written. It loses the quote style (`"x"`, `[x]`, `` `x` `` and an alias `'x'` all become `Quoted: true`), raw name text, end positions, some keyword spellings (`<>`/`!=`, `TEMP`/`TEMPORARY`, `0X`/`0x`), and a repeated `strict`. A pass over the tree alone cannot reject brackets or backticks; it must go back to the source text through the positions.
- **Robustness.** A depth limit of 1000 stops stack overflow. Fuzzing found one panic site among those logged (an assertion in `parseConstraint`). A compound select with many terms takes quadratic time: 7.7 s for 30,000 `UNION`s.
- **What the evidence favours.** A parser that s4ql owns, as agreed, with rqlite/sql as a reference and a source of test inputs, not as the parser. Reason: its disagreements fall exactly where s4ql's rules are strict (names, quoting, numbers, reinterpretation); its tree drops distinctions that the "supported?" pass needs; and its errors would need SQLite's own grammar to classify anyway, which is the agreed parser itself. Using it as the parser would put a third-party parse, not checked against SQLite by its own tests, between the application's SQL and the engine. §8 lays out the options.

## 1. State of the project

All V, from the clone and from the GitHub API (read only) on 2026-11-16.

- **Licence.** MIT, "Copyright (c) 2020 Ben Johnson" (`LICENSE`).
- **Origin.** Ben Johnson wrote it in 2020; `README.md` credits him: "This parser was originally created by Ben Johnson. Many thanks to him for making it available for use by rqlite." The repository `rqlite/sql` was created on 2020-09-15.
- **`benbjohnson/sql` is a GitHub fork of `rqlite/sql`**, created on 2024-03-10 (`"fork": true, "parent": "rqlite/sql"`), last pushed 2024-03-11. Its default branch is at `f3365e4` (2024-03-09), which is an ancestor of the rqlite head: rqlite/sql is 187 commits ahead, 7,979 lines added and 1,288 removed since. The fork was the source of Ben Johnson's pull requests #19 to #28 and #30 in March 2024, and it is not maintained. Its `go.mod` still says `module github.com/rqlite/sql`.
- **Contributors** (`git shortlog -sn`): Philip O'Toole 167 commits, Ben Johnson 20, chk-n 18, Elara6331 12, Copilot 11 plus copilot-swe-agent 7, Walter Wanderley 3, three others 1 each. Brad Fitzpatrick has one commit, "Support '--' style comments" (`9614678`, 2024-03-10), and opened issues #18, #29, #31, and #32 in 2024 about parsing real schemas.
- **History.** 233 commits: 1 in 2020, 32 in 2022, 64 in 2024, 35 in 2025, 101 in 2026, 83 of them in October 2026. Pull requests #113 to #115 are titled "Parsing fixes by Claude", "More claude fixes", and "More fixes by Claude"; #116 and #117 fix `Walk` and `CURRENT_TIME`. The repository's `CLAUDE.md` gives agents the rule "The parser must accept exactly what the `sqlite3` binary accepts and reject what it rejects with a syntax error", to be checked by running statements in `sqlite3 :memory:` by hand.
- **Releases.** None: no tags. Users pin pseudo-versions; rqlite pins `v0.0.0-20261016220845-5c0832fea6e1`, the head (rqlite's `go.mod`, line 20).
- **Issues and pull requests.** 117 in all, all closed; open issues 0, open pull requests 0. The issues that bear on correctness are reports of valid SQL that failed to parse (#6, #8, #10, #29, #31, #34, #46, #74, #83, #89, #91, #92, #95 to #97, #100, #101, #108, #110), a wrong reconstruction of `JOIN ... ON` by `String()` (#47), and panics (#32, a `String()` panic). None reports rqlite/sql accepting what SQLite rejects.
- **Tests.** `parser_test.go` (6,941 lines), `ast_test.go`, `scanner_test.go`, `walk_test.go`, `clone_test.go`, `token_test.go`: hand-written inputs with expected trees or error strings. `go test -cover` passes with 76.4% statement coverage (V, run). No fuzz test (`grep -l "func Fuzz"` finds none), and no test runs SQLite. CI is CircleCI with `cimg/go:1.21.5`: `gofmt`, `go vet`, `go test -v`, and `go test -race` (`.circleci/config.yml`).
- **Dependencies.** `github.com/go-test/deep v1.0.8`, imported only by test files. Pure Go: no cgo, no `os/exec`, no network.
- **Go version.** `go 1.17` in `go.mod`; built here with 1.27.4.
- **Users.** rqlite itself, in `command/sql/processor.go`: it parses a statement only if its lower-cased text contains a time function, `random(`, `returning `, or `explain`; on a parse error it leaves the statement unchanged (`if err != nil { continue }`); it rewrites by `String()`; and it recovers from panics and counts them in `num_parser_panics`. A GitHub code search for `"github.com/rqlite/sql"` in `go.mod` files returns 83 results, among them siyuan-note/siyuan, maxpert/marmot, spacemeshos/go-spacemesh, sunary/sqlize, and the walterwanderley/sqlc-* generators (NV: how each uses it).

## 2. Acceptance against SQLite

### 2.1 The corpus

68,032 distinct statements, after dropping text with a NUL byte or invalid UTF-8 (s4ql rejects both before parsing), and one final semicolon.

| Source | Statements | How |
|---|---|---|
| SQLite's TCL test scripts, tag `version-3.54.1` of the GitHub mirror | 47,418 | 1,199 `test/*.test` files; 41,192 brace blocks after `execsql`, `catchsql`, `eval`, `do_execsql_test`, `do_catchsql_test`, and `do_eqp_test`; split at semicolons where `sqlite3_complete` says a statement ends |
| meyer's extracted cases (`sqlc-dev/meyer` at `59b2dbf`, `parser/testdata/*.test`) | 95 new | the same split; most were already in the above |
| SQLite's documentation, `sqlite-doc-3540100.zip` | 1,178 | every `<pre>` block, tags stripped, statements that start with a statement keyword |
| rqlite/sql's own tests | 726 | the string argument of every `AssertParse*` call and the other parse helpers, read with `go/parser`; expressions prefixed with `select ` |
| `internal/sql/token` tests | 325 | each case's text, alone and after `select ` |
| Generated | 18,290 | slice-1 shapes and the next plausible slices with every name (all 147 keywords, quote styles, `rowid`, `strict`, `true`, `null`, ...), value (signs, hex, digit separators, the 64-bit edges, parameters, ...), type, and table option; the quirks of [sql-front-end.md](sql-front-end.md) §1.1; and every one-token deletion, duplication, swap, insertion, and replacement (from 61 tokens) of 10 base statements |

### 2.2 How SQLite's outcome was classed

Each statement was prepared on a fresh `:memory:` connection.

- **syntax**: `near "X": syntax error`, `incomplete input`, `unrecognized token: "X"`, any message containing `syntax error`, or `Recursion limit`.
- **parsed**: success, or any other error whose tail reached the end of the statement, such as `no such table`. Many statements of SQLite's tests fail with `no such table` on an empty database; the parse was complete for them.
- **masked**: another error raised by a grammar action before the end, so the rest was never parsed. For `no such table: X` and `unknown database D`, the table was created or the database attached, and the statement prepared again, up to 8 times (949 statements). 358 stayed masked (for example `cannot create INSTEAD OF trigger on table`, `unknown table option`) and are left out of the counts below.
- **multi**: 18 texts of several statements, left out.

### 2.3 Counts

| | rqlite/sql accepts | rejects | panics |
|---|---|---|---|
| SQLite parses (53,965) | 52,217 | **1,748** | 0 |
| SQLite syntax error (13,691) | **154** | 13,534 | 3 |

By source:

| Source | SQLite parses: rqlite/sql rejects | SQLite syntax error: rqlite/sql accepts |
|---|---|---|
| SQLite's tests and meyer | 535 of 46,942 | 47 of 371 (and 1 panic) |
| Documentation | 24 of 1,098 | 1 of 67 |
| rqlite/sql's tests | 12 of 343 | 4 of 379 |
| `internal/sql/token` tests | 4 of 67 | 17 of 254 |
| Generated | 1,173 of 5,515 | 85 of 12,620 (and 2 panics) |

The generated corpus is built to hit names, numbers, and edges, so its rates are not those of ordinary SQL.

### 2.4 What rqlite/sql accepts and SQLite rejects

The dangerous direction. Grouped by cause by a classifier on the token at SQLite's error offset, then checked by reading the statements. Counts are of the 154.

| Cause | Count | Example | SQLite 3.54.1 |
|---|---|---|---|
| `ORDER BY` and `LIMIT` on `DELETE` | 45 | `DELETE FROM t1 LIMIT 5` | `near "LIMIT": syntax error` |
| `SELECT` without parentheses as an operand | 23 | `select select 1`; `select * from t where a = select 1` | `near "select": syntax error` |
| alias without `AS` on the table of `UPDATE` and `DELETE` | 21 | `UPDATE vals v SET a=1` | `near "v": syntax error` |
| join keywords and `INDEXED` as words of a type name | 19 | `create table t1 (v1 left primary key)` | `near "left": syntax error` |
| `NULL` as a name | 17 | `drop table null`; `create table null (...)` | `near "null": syntax error` |
| a number directly followed by letters, digits, or `_` | 16 | `SELECT 123abc`; `SELECT 0xFF__EF`; `select 1_` | `unrecognized token: "123abc"` |
| `NULL` read as a word of a type name | 5 | `create table t1 (v1 null int primary key)` | `near "int": syntax error` |
| `table.*` inside an expression | 2 | `select t.* = 1 from t` | `near "=": syntax error` |
| `/*` as the last two bytes, read as a comment | 2 | `select 1 /*` | `near "*": syntax error` |
| a byte order mark read as a name | 2 | `select <BOM>` | `incomplete input` |
| `INSERT ... SELECT ... ON CONFLICT` without `WHERE` | 1 | `INSERT INTO t1 SELECT * FROM t2 ON CONFLICT(x) DO UPDATE SET y=excluded.y` | `near "DO": syntax error` |
| U+00A0 read as whitespace | 1 | `select 1<U+00A0>from t1` | `unrecognized token: "1<U+00A0>from"` |

Notes:

- `DELETE ... LIMIT` is valid only in a SQLite built with `SQLITE_ENABLE_UPDATE_DELETE_LIMIT`, which the amalgamation and s4ql's build do not set. rqlite/sql accepts it on `DELETE` but rejects it on `UPDATE` (`update t set a = 1 limit 1`: `expected semicolon or EOF, found 'LIMIT'`), so it follows neither build (V, measured).
- `NULL` is a word of a type name in rqlite/sql (`isTypeNameToken`, `token.go` 550), so `v1 int null` is a column of type `int null` and no `NULL` constraint, where SQLite reads type `int` and a `NULL` constraint. After a sized type, where no further type word may follow, `text(50) null` is rejected (§2.5).
- Several of these change meaning rather than add syntax. A number followed by letters is split into a number and an alias: `select 1x` parses as `select 1 AS x`, and `select 1_` as `select 1 AS _` (V, `String()` prints `SELECT 1 AS "x"`). `drop table null` is a `DropTableStatement` for a table named `null`.
- Among the statements that SQLite parses but rejects in a grammar action, rqlite/sql accepts some that a syntax-only parser would also accept (`unknown datatype`, `PRIMARY KEY missing`), and four that are grammar restrictions: `qualified table names are not allowed on INSERT, UPDATE, and DELETE statements within triggers` (V, measured).

**Accepted by both, with different meaning.** Acceptance counts do not show this, so a second check was run: for each of the 18,286 statements that rqlite/sql accepts and that SQLite prepares in a schema where common names resolve (PRAGMA, EXPLAIN, VACUUM, ATTACH, DETACH left out), the `EXPLAIN` of the original and of rqlite/sql's `String()` were compared, with the stored text of `CREATE` statements masked. 67 differ (measured):

| Cause | Count | Example |
|---|---|---|
| quoting in the reprint, which `ALTER TABLE ... RENAME` and `ADD COLUMN` store as written | 42 | `ALTER TABLE t1 RENAME a TO b` reprinted with `"b"`; SQLite records whether the new name was quoted |
| digit separators read as a number and an alias | 10 | `select 1_000`: SQLite loads 1000, the reprint `SELECT 1 AS "_000"` loads 1 |
| `LIMIT a, b` | 8 | `select * from t1 limit 1, 2`: SQLite reads OFFSET 1 LIMIT 2; the reprint `LIMIT 1 OFFSET 2` |
| reprint does not prepare | 7 | `[char.3]` as a type loses its quotes; `select <BOM>1`; a column named from source text (`max(x)`) that the reprint respells `max("x")` |

The first and last groups are artefacts of reprinting, but they show what the tree does not keep (§5). The middle two are reinterpretations in the tree itself. With digit separators, `insert into t1 (v1) values (1_000)` is rejected (`expected comma or right paren, found _000`), while in a select list, where an alias may follow, it is silently misread.

**`true` and `false`.** SQLite reads them as names first: with a column `"true"` holding 5, `select true from t` returns 5 (measured). rqlite/sql always makes a `BoolLit` (`parser.go` 2850-2851).

### 2.5 What SQLite accepts and rqlite/sql rejects

The 1,748, grouped the same way.

| Cause | Count | Example |
|---|---|---|
| a fallback keyword as a table name or alias where rqlite/sql takes only `IDENT`, `QIDENT`, `BIDENT`: after `FROM`, `UPDATE`, `DELETE FROM`, after `AS`, and as an alias without `AS` | 881 | `SELECT * FROM temp.sqlite_master` (66 of SQLite's own statements); `select v1 as abort from t1`; `select * from left` |
| `ROWID`, `STRICT`, `TRUE`, `FALSE` treated as keywords | 196 | `CREATE TABLE t1(rowid TEXT, b INTEGER)`; `select v1 as true from t1` |
| a string literal as a name | 102 | `CREATE TABLE t10(a, 'ON', c)`; `ATTACH 'test3.db' AS 'ON'` |
| `$::name` parameters, reported as `expected expression, found 'ILLEGAL'` | 100 | `UPDATE sqlite_schema SET sql = $::newsql` (TCL scripts) |
| `ALTER TABLE ... ALTER COLUMN`, `ADD CONSTRAINT`, `DROP CONSTRAINT` | 53 | `ALTER TABLE t1 DROP CONSTRAINT abc` |
| virtual table arguments, which SQLite takes as any tokens | 47 | `CREATE VIRTUAL TABLE t10 USING fts3(<, b, c)` |
| aggregate `ORDER BY` inside a call | 31 | `SELECT group_concat(a ORDER BY a) FROM t1` |
| column constraints: `NULL` after a sized type (32), `NOT NULL` after `REFERENCES` (3), and other orders such as `CHECK (...) ON CONFLICT` or a constraint name alone | 46 | `CREATE TABLE t(a text(50) null)`; `CREATE TABLE t6(a REFERENCES t4(a) NOT NULL)` |
| `WINDOW`, `OVER`, `FILTER` as names | 30 | `SELECT sum(x) over over FROM over WINDOW over AS ()` |
| digit separators | 25 | `SELECT base64(zeroblob(2000_000_000))` |
| compound selects with `LIMIT` or `ORDER BY` before `UNION`, `EXCEPT`, or `INTERSECT` (SQLite parses them and then refuses them with `LIMIT clause should come after UNION not before`) | 22 | `SELECT x FROM t2 LIMIT 5 UNION SELECT a FROM t6` |
| row values with `VALUES` as an operand | 13 | `SELECT rowid FROM t1 WHERE (a, b) IN ( VALUES('abc', 'def') )` |
| `WITH` inside a parenthesised subquery expression | 11 | `SELECT (WITH w1 AS (SELECT 1) SELECT * FROM (SELECT 1)) FROM t0` |
| join operators of other words, which SQLite rejects later as `unknown join type` | 10 | `SELECT * FROM t1 INNER OUTER JOIN t2` |
| `#name` parameters | 9 | `select #a` |
| empty statement first | 10 | `; select * from t1` |
| trigger restrictions enforced as syntax | 4 | `CREATE TRIGGER ... BEGIN DELETE FROM aux.t1; END` |
| long tail: `begin transaction name`, `WITHOUT oid` and other unknown table options, `pragma_table_list()` with no arguments, `ESCAPE` after `GLOB`, `ATTACH ... AS ?`, ... | the rest | |

The first two groups come from one design choice: rqlite/sql has a single hand-kept list of keywords usable as names, `bareTokens` (`token.go` 481-493), used in every place. SQLite uses three token classes: `nm` (`ID`, `INDEXED`, join keywords, or a string), `ids` (`ID` or a string, for type names), and the `%fallback ID` list (`parse.y` 272-295, 326-340). So rqlite/sql both rejects names that SQLite accepts (fallback keywords after `FROM`, because `parseQualifiedTable` takes only `isIdentToken`, `parser.go` 2591) and accepts type names that SQLite rejects (join keywords, through `isTypeNameToken`, `token.go` 548-557).

### 2.6 Which groups matter for s4ql's slices

- **Slice 1** (`create table`, `select *`, `insert`, `drop table`, any name, any integer): `NULL` as a name is accepted (`create table null`, `insert into null`, `drop table null`); `rowid`, `strict`, `true`, `false` as column names are rejected; fallback keywords as table names are rejected after `FROM` but accepted after `INSERT INTO` and `DROP TABLE` (`parseIdent`, `parser.go` 1613-1626); brackets and backticks cannot be told from double quotes in the tree (§6); a repeated `strict` collapses into one; digit separators are rejected in `values (...)`; error positions are not SQLite's in about 7% of syntax errors (§4).
- **`where` and expressions**: `select 1x` and `select 1_000` misread as aliases; `SELECT` without parentheses accepted; `true`/`false` always literals; `table.*` accepted in expressions; row values with `VALUES` rejected.
- **More column types**: join keywords and `NULL` accepted as type words; type names joined by one space, so spacing is lost; quotes lost after the first word.
- **`update` and `delete`**: an alias without `AS` accepted; `DELETE ... ORDER BY/LIMIT` accepted; fallback keywords as table names rejected.
- **Joins**: join keywords as table names rejected (`select * from left`); join operators of unknown words rejected as syntax, where SQLite reports `unknown join type` later.
- **Transactions**: `begin transaction name` rejected; SQLite accepts it (measured).
- **Corners**: virtual table arguments, `ALTER TABLE ... CONSTRAINT`, `$::name` and `#name`, window keywords as names, the byte order mark, `/*` at the end.

## 3. The tokenizer

- **Built in.** `Parser` holds a `*Scanner` (`parser.go` 16-24), created from an `io.RuneReader` by `NewParser` (41-45). There is no interface for a token source, so `internal/sql/token` cannot feed the parser without changing rqlite/sql (V).
- **It reads runes, not bytes.** `Scanner.read` calls `ReadRune` and counts `Pos.Offset` and `Pos.Column` in runes (`scanner.go` 418-439), although `Pos.Column` is documented as "(byte count)" (`token.go` 640). Measured: in `select ä, "ä" from t1`, `t1` is at byte 21 and has `Offset` 19.
- **Differences from `sqlite3GetToken`, by reading** (V, `scanner.go`):
  - whitespace is `unicode.IsSpace` (line 29), so U+00A0, U+0085, U+2028, and the vertical tab are spaces anywhere; SQLite takes only space, tab, LF, FF, CR to start a space token and treats bytes from 0x80 up as name characters;
  - the byte order mark is a name character everywhere; SQLite skips it where a token starts;
  - `scanNumber` (336-416) stops at the first non-digit and does not check what follows, so `1x`, `0x1g`, `1_000` become two tokens; SQLite makes one illegal token or a `QNUMBER`;
  - `/*` at the end of the text is a comment (229-243); SQLite makes it `/` and `*`;
  - parameters: no `#name`, and the TCL suffixes `::` and `(...)` only after `$` and only when a name follows the `$` (245-303), so `$::a` and `:a(b)` are rejected;
  - an unterminated string, name, or blob is one `ILLEGAL` token, as in SQLite.
- **Measured over the 68,032 statements**, comparing the sequence of token start offsets (converted to bytes) and classes, up to the first illegal token: boundaries and classes equal in 56,702 (83.3%); boundaries equal in 67,780 (99.63%), the rest of the difference being keyword against name. The first difference in the other 252: `$::name`/`#name` against `ILLEGAL` 128, `QNUMBER` against `INTEGER` or `FLOAT` 67, an illegal number against `INTEGER` or `FLOAT` 34, `/` against `COMMENT` 5, the TCL suffix `(` 5, and Unicode spaces and the byte order mark 5, plus 8 where rqlite/sql reads on after SQLite's last token. `internal/sql/token` agreed with `sqlite.Tokenize` on every statement.
- **Keywords** (measured): SQLite has 147 (`sqlite3_keyword_count`), rqlite/sql 152 (`token.go` 80-230 plus `NULL`, `TRUE`, `FALSE`). Every SQLite keyword is a keyword in rqlite/sql. Five rqlite/sql keywords are names in SQLite: `ROWID`, `STORED`, `STRICT`, `TRUE`, `FALSE`.

## 4. Errors

- **Form** (V, `parser.go` 4028-4052). `*sql.Error{Pos, Msg}`; `Error()` gives `line:column: msg`, for example `1:8: expected expression, found 'EOF'`. `errorExpected` builds `expected <what>, found <token>`, with a literal's value (dequoted for strings) or a token's name in quotes. A depth error is `parse tree is too deep (maximum depth 1000)`. A panic is possible (§7).
- **What is available.** The error's position and its message. The failing token's kind and text are not in the error; they are unexported fields of the `Parser` (`tok`, `lit`). To give `near "X": syntax error`, s4ql would take `Pos.Offset`, convert runes to bytes, and read the token there with its own tokenizer; for `incomplete input`, it would match `found 'EOF'` in the message. Both rest on the message wording and on the position being the failing token's.
- **Does it stop where SQLite does?** Over the statements that both reject, where SQLite gives a syntax error (measured):

| SQLite's message | Cases | rqlite/sql's position is SQLite's offset | earlier | later | none |
|---|---|---|---|---|---|
| `near "X": syntax error` | 12,801 | 11,942 (93.3%) | 773 | 85 | 1 |
| `unrecognized token: "X"` | 278 | 245 (88.1%) | 0 | 33 | 0 |
| `incomplete input` | 451 | 439 name `EOF` (97.3%) | | | |

On SQLite's own test statements alone: `near` 158 of 175 (90.3%), `unrecognized token` 128 of 132, `incomplete input` 11 of 12.

- **Why it differs.** "Earlier" is mostly an error reported at the position of the statement's first keyword instead of the failing token: `drop - t1` gives `1:1: expected TABLE, VIEW, INDEX, or TRIGGER`, where SQLite says `near "-"` at offset 5; and rqlite/sql stopping on a name SQLite accepts (§2.5). "Later" is rqlite/sql accepting a token that SQLite does not (§2.4) and failing further on, for example `update t1 's' v1 = 2` (`expected SET, found v1`, SQLite `near "'s'"`).

## 5. The syntax tree

Read in `ast.go` and checked by dumping trees with `encoding/json/v2` (V).

What it keeps:

- **Positions** of keywords, parentheses, and names as `Pos{Offset, Line, Column}`, in runes. `CreateTableStatement` has `Create`, `Temp`, `Table`, `If`, `IfNot`, `IfNotExists`, `Lparen`, `Rparen`, `Without`, `Rowid`, `Strict`, `As` (`ast.go` 756-776); a zero `Pos` means absent. Nodes have no end position, and expressions have no span, so the text between two positions cannot be found from the tree.
- **Optional clauses**: `IF EXISTS`, `IF NOT EXISTS`, `TEMP`, `STRICT`, `WITHOUT ROWID` (with the order recoverable from the positions), `AS` before an alias (`As` position), `ASC`/`DESC`, `OffsetComma` for `LIMIT a, b`.
- **Names**: `Ident{NamePos, Name, Quoted}` (`ast.go` 1735-1739). `Name` keeps case and is dequoted; `int` and `INTEGER` stay distinct as `Type.Name.Name`.
- **Literals**: `NumberLit.Value` and `BindExpr.Name` as written, signs as `UnaryExpr`, strings dequoted in `StringLit.Value`.

What it loses:

- **The quote style.** `Quoted` is true for `"x"`, `[x]` (scanned as `QIDENT`, `scanner.go` 177-191), `` `x` `` (`BIDENT`), and a string used as an alias (`parser.go` 1605-1611). The raw text of a name is not kept, so `table "t1" already exists` ([sql-front-end.md](sql-front-end.md) §3.1) cannot be formatted from the tree alone.
- **Keyword spellings**: `<>` and `!=` are both `NE`, `=` and `==` both `EQ`, `TEMP` and `TEMPORARY` both `Temp`, `0X1F` becomes `0x1F` (`scanner.go` 348), and a blob keeps only its digits (`X'ab'` becomes `ab`).
- **Repeated options**: `strict, strict` keeps one `Strict` position (`parser.go` 418-432); SQLite accepts the statement.
- **Type names**: several words are joined with one space (`parser.go` 1628-1650), so `unsigned   big int` becomes `unsigned big int`, and quotes after the first word are dropped. SQLite keeps the text of the type as written (`parse.y` 356-357).
- **The meaning of `LIMIT a, b`**: `LimitExpr` holds `a`, which SQLite takes as the offset (`parser.go` 2355-2370); only `OffsetComma` tells.

What it reinterprets while parsing: digit separators and a number followed by letters become a number and an alias; `true` and `false` become `BoolLit`; `NULL` and `ROWID` become names (`parser.go` 1618); `SELECT` without parentheses becomes a `SelectExpr`; U+00A0, other Unicode spaces, and `/*` at the end are skipped.

Can a pass over the tree reject everything the roadmap does not list? Only with the source text beside it. A pass can see each node type and each optional clause. It cannot see from the tree whether a name was bracketed, backquoted, or single-quoted, how a keyword was spelled, or whether `strict` came twice; for those it must read the source at `Pos.Offset`, after converting runes to bytes, with a tokenizer of its own. And it cannot see what the parser already reinterpreted: `select 1_000` arrives as a valid `select 1 AS _000`, with nothing in the tree to reject.

## 6. Fit with the restrictions

| Restriction | Visible in the tree? |
|---|---|
| double-quoted string | Not a parser question: `"x"` is an `Ident` with `Quoted: true`, and name resolution decides, as with `SQLITE_DQS=0` |
| `[brackets]` | No: the same `Ident` as `"x"`. The first byte at the name's position tells |
| backticks | No: the same `Ident`. The first byte tells |
| single-quoted names | Rejected by rqlite/sql where SQLite takes `nm ::= STRING` for tables and columns (`create table 't1' (...)`, `insert into t ('a')`), so s4ql would reject them, but with rqlite/sql's error. As an alias (`select 1 as 'b'`, `from t 'x'`), accepted and indistinguishable from `"b"` |

## 7. Robustness

- **Fuzzing** (measured). rqlite/sql has no fuzz test. A Go fuzz target over `ParseStatement` and `String()`, seeded with every 20th statement of the corpus (3,402 seeds), ran for 300 s: 28.8 million executions. All 390,471 panics whose site was logged are one site, `assert failed` at `parser.go` 536 (`parseConstraint`); the log lost the site of 50,466 panics on text that is not valid UTF-8 (NV whether those are the same site). The site is reached whenever a table constraint and a comma are followed by a token that does not start a constraint. Smallest input: `create table zz (a, check (1), b)`. No input took more than 2 s.
- **Nesting** (measured). `maxParseDepth` is 1000 (`parser.go` 13), and nesting beyond it returns an error rather than overflowing the stack: parentheses, unary minus, `NOT`, subqueries, `CASE`, `CAST`, and function calls, each tested at 100 to 100,000 levels. It differs from SQLite at the edge: 1000 nested parentheses parse in SQLite and fail in rqlite/sql.
- **Long inputs** (measured, parse only). Left-nested chains of 100,000 `+`, `AND`, or `COLLATE` parse in under 20 ms. A compound select is quadratic: 10,000 `UNION`s take 0.52 s, 30,000 take 7.7 s. SQLite refused 1,000 terms with `too many terms in compound SELECT` (its documented default limit is 500, NV here). `String()` is quadratic on long join and `COLLATE` chains (17 s for 100,000 joins, 41 s for 100,000 `COLLATE`s, including a reparse).

## 8. Ways to use it

1. **As the parser, as it is.** It exists, is maintained, and parses 96.8% of what SQLite parses. The costs in trust: it accepts 1.1% of SQLite's syntax errors, some as reinterpretations; its tree does not carry the quote style or raw text; its errors are not SQLite's and their positions are SQLite's in about 93%; its tokenizer cannot be replaced; it is tested by hand-written expectations only; and its users do not depend on strict rejection (rqlite passes unparsed statements on to SQLite). A pre-pass with `internal/sql/token` could reject the lexical cases first (numbers followed by letters, digit separators, Unicode spaces, the byte order mark, `/*` at the end, brackets, backticks). It could not catch the grammar cases (`NULL` as a name, `SELECT` as an operand, alias without `AS`).
2. **As the tree builder, next to a recognizer of s4ql's own that classifies syntax errors.** The recognizer would be a parser of all of SQLite's grammar, checked against SQLite, which is the parser already agreed. rqlite/sql would add a second tree, and every disagreement between the two would have to be handled. The tree's losses (§5) remain.
3. **As a fork that s4ql owns.** MIT allows it. To meet s4ql's needs, the fork would replace the scanner with `internal/sql/token`, use byte offsets and end positions, keep raw text and quote style in names, follow SQLite's token classes instead of `bareTokens`, drop the five extra keywords and `BoolLit`, fix the groups of §2.4, and give SQLite's messages. That changes the lexer, the name handling, and the error path, which is most of what s4ql would check. What stays is the recursive-descent structure and the node types. The fork would need the same checks against SQLite as a parser of s4ql's own, and would carry design choices made for rqlite's purpose.
4. **As a reference only.** Its 726 test inputs, its node set, and its `Walk`, `Clone`, and `String()` invariants (`CLAUDE.md`) can inform s4ql's parser and tests. It can also run as a third opinion in s4ql's differential tests (s4ql, SQLite, rqlite/sql), where a disagreement points at a case worth a test.

Whether each group of §2.4 and §2.5 could be fixed upstream:

| Group | Bug against SQLite's behaviour, fixable upstream | Design choice of the library |
|---|---|---|
| number followed by letters; digit separators; Unicode spaces; byte order mark; `/*` at the end; `$::name`, `#name`, `:a(b)` | yes, in the scanner | |
| `NULL` as a name; `SELECT` as an operand; `table.*` in expressions; alias without `AS` on `UPDATE` and `DELETE`; `INSERT ... SELECT ... ON CONFLICT` without `WHERE`; the panic; quadratic compound selects; `LIMIT a, b` in `String()` | yes, each by itself | |
| fallback keywords and join keywords by context | yes in principle, case by case | the single `bareTokens` list is the cause |
| `ROWID`, `STORED`, `STRICT`, `TRUE`, `FALSE` as keywords; `BoolLit` | possible, but it changes the public node types | yes |
| `DELETE ... ORDER BY/LIMIT` | depends on which SQLite build is the reference | the rule "the `sqlite3` binary is the authority" names no build |
| missing grammar (`ALTER TABLE ... CONSTRAINT`, aggregate `ORDER BY`, `VALUES` as an operand, `WITH` in a subquery expression) | yes, as features | |
| virtual table arguments; trigger restrictions as syntax; Go-style errors and rune positions; no raw text or quote style in names | | yes |

What the evidence favours: option 4, with the agreed parser of s4ql's own. Reason: the disagreements fall where s4ql's rules are strictest, its tree drops the distinctions that the "supported?" pass needs, and SQLite's syntax errors can only be classified with SQLite's grammar, which option 2 and 3 would have to build anyway. Option 1 would make s4ql's correctness depend on a project whose own tests do not compare with SQLite and whose main user tolerates both parse failures and panics.

## 9. Bugs that could be reported upstream

Not reported. Each is the smallest input found, what rqlite/sql at `5c0832f` does, and what SQLite 3.54.1 does (measured).

1. **Panic.** `create table zz (a, check (1), b)`: panics with `assert failed` (`parser.go` 536). SQLite: `near "b": syntax error`. Also `create table zz (a, unique (a), b)`.
2. **`LIMIT a, b` reprinted with its meaning reversed.** `select * from t limit 1, 2`: `String()` gives `SELECT * FROM "t" LIMIT 1 OFFSET 2`. SQLite reads `LIMIT 1, 2` as offset 1, limit 2. rqlite itself rewrites statements by `String()` when they call `random()` or a time function.
3. **Digit separators read as an alias.** `select 1_000`: accepted as `SELECT 1 AS "_000"`. SQLite returns 1000. Also `select 0x1_0`.
4. **A number followed by letters read as an alias.** `select 1x`: accepted as `SELECT 1 AS "x"`. SQLite: `unrecognized token: "1x"`. Also `select 1_`.
5. **`NULL` as a name.** `drop table null`: accepted. SQLite: `near "null": syntax error`.
6. **`SELECT` as an operand without parentheses.** `select select 1`: accepted. SQLite: `near "select": syntax error`.
7. **Alias without `AS` on `UPDATE` and `DELETE`.** `update t x set a = 1`: accepted. SQLite: `near "x": syntax error`.
8. **Join keywords and `NULL` as type names.** `create table zz (a left)` and `create table zz (a null int)`: accepted. SQLite: `near "left"` and `near "int"`.
9. **`table.*` in an expression.** `select t.* = 1 from t`: accepted. SQLite: `near "=": syntax error`.
10. **`/*` at the end.** `select 1 /*`: accepted. SQLite: `near "*": syntax error`.
11. **Unicode whitespace.** `select 1<U+00A0>from t`: accepted. SQLite: `unrecognized token: "1<U+00A0>from"`.
12. **`INSERT ... SELECT ... ON CONFLICT` without `WHERE`.** `insert into t select * from t on conflict(a) do nothing`: accepted. SQLite: `near "do": syntax error`.
13. **Fallback keywords as table names.** `select * from temp.sqlite_master`: `expected table name or left paren, found 'TEMP'`. SQLite parses it.
14. **Words that are not keywords in SQLite.** `create table zz (rowid int)` and `create table zz (true int)`: rejected. SQLite parses them.
15. **Leading byte order mark.** `<BOM>select 1`: `expected statement`. SQLite parses it.
16. **TCL-style parameters.** `select $::a`: rejected. SQLite parses it.
17. **Quadratic compound select.** 30,000 `UNION`s take 7.7 s to parse.
18. **Documentation.** `Pos.Column` is documented as a byte count (`token.go` 640) and counts runes, as does `Pos.Offset`.

## Not verified

- How the 83 repositories that list rqlite/sql in `go.mod` use it.
- Whether the statements extracted from the TCL scripts are exactly what the tests run: the extraction takes brace blocks as written and skips substituted SQL, so a few blocks are templates rather than SQL (they appear as syntax errors on both sides).
- When SQLite added `ALTER TABLE ... ADD/DROP CONSTRAINT` and `ALTER COLUMN`; 3.54.1 parses them.
- Behaviour on text that is not valid UTF-8 or that holds NUL bytes, which the corpus left out because s4ql rejects such text first. The fuzzer did feed such text and found no other panic.
- The fuzzing covered 300 s, guided by coverage of rqlite/sql's Go code; a longer run may find more. The site of the panics on text that is not valid UTF-8 was not logged.
