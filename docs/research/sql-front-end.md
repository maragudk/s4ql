# The SQL front end: tokenizer, parser, errors, API, and layers

Date: 2026-11-13. Tags: **V** = verified in this run (run on SQLite, or read in source or docs, with the reference). **I** = inferred. **NV** = not verified.

Question: how should the first SQL slice of [../roadmap.md](../roadmap.md) be built, from text to rows and back to the application? This note lays out the facts and the options for six subjects: the tokenizer, the parser, SQLite's exact errors for slice 1, how SQLite-compatible engines test against their reference, the public Go API, and the layers of the engine. It does not decide. Where the evidence favours an option, it says which and why.

What other notes already cover is not repeated: sqllogictest, SQLancer's oracles and corpora in [external-test-suites.md](external-test-suites.md) and [testing-strategies.md](testing-strategies.md); the oracle's configuration and the legacy behaviours in [sqlite-docs-inventory.md](sqlite-docs-inventory.md) §2 and §4; the Go SQLite bindings in [sqlite-cache-and-oracle.md](sqlite-cache-and-oracle.md) §4.

**Setup.** SQLite 3.54.1, source ID `2026-10-16 14:45:20 555e07ef…6ca76`: the vendored amalgamation `internal/sqlite/c/sqlite3.c`, compiled with `internal/sqlite/options.h`, as the tests of `internal/sqlite` compile the shell. Line numbers that cite `sqlite3.c` refer to that file. The grammar `src/parse.y` and the tool `tool/lemon.c` are not in the amalgamation and were fetched from the tag `version-3.54.1` of the GitHub mirror of SQLite (https://github.com/sqlite/sqlite/blob/version-3.54.1/src/parse.y).

**Method.** Three throwaway C programs in the scratch directory, not published:

- `probe.c` opens `:memory:` with `SQLITE_OPEN_EXRESCODE`, as `sqlite.Open` does, prepares and steps each statement of a script, and prints the primary and extended result code, `sqlite3_errmsg`, `sqlite3_error_offset`, whether the error came at prepare or at step, and the rows. "Run on 3.54.1" below means this program. It links the same C code with the same options as the binding, so its results are the binding's, with `SQLITE_DEFAULT_FOREIGN_KEYS=1` and `SQLITE_DQS=0`.
- `tok.c` includes `sqlite3.c` into its own translation unit and calls SQLite's private tokenizer `sqlite3GetToken` directly.
- The same `probe.c` calls `sqlite3_complete` and the `sqlite3_keyword_*` functions.

The Homebrew `sqlite3` shell on the machine reports the same version and source ID, but its compile options differ (see [sqlite-docs-inventory.md](sqlite-docs-inventory.md) §4.2), so it was not used for results.

## Short answer

- **Tokenizer.** SQLite's tokenizer is one byte-level function with quirks a Go copy must keep (the vertical tab, the BOM, non-ASCII bytes as identifier characters, digit separators checked later). s4ql's restrictions do not shrink it, because rejected tokens must still be found to be rejected. The private `sqlite3GetToken` can be called from a C test program that includes the amalgamation, which gives an exact token-by-token oracle for Go's fuzzer. The evidence favours a tokenizer for all of SQLite's tokens, checked that way.
- **Parser.** `near "X": syntax error` depends only on the grammar's language, so any parser of the same language that stops at the first impossible token reproduces it. A parser for slice 1 alone cannot, because it stops earlier than SQLite. A new MIT Go project, `sqlc-dev/meyer`, is a hand-written port of all of `parse.y` that claims SQLite's exact syntax messages and offsets on 21,326 cases from SQLite's test scripts; it is young, has one author, and pins 3.53.4, whose grammar rules and tokenizer equal 3.54.1's. The evidence favours a full-grammar parser that s4ql owns, with a pass after parsing that rejects what slice 1 does not list, and meyer as a reference or a starting point.
- **Errors.** Measured on 3.54.1: a repeated key gives `UNIQUE constraint failed: t1.v1` (19 / 1555, at step); the other four slice-1 errors are `table t1 already exists` and `no such table: t2` (1 / 1, at prepare). `already exists` quotes the name as written, quotes and all; `no such table` the name as written without quotes; `UNIQUE` the stored names. Slice 1's shapes can also produce `table t1 has no column named v2`, `object name reserved for internal use`, `cannot store REAL value in INT column` (for 9223372036854775808), and `hex literal too big`. For most inputs s4ql will reject, SQLite has no error at all. Markus decides what s4ql promises to match (code, extended code, message) and how "not supported" is marked.
- **Testing.** No surveyed project compares error messages in its live differential tests; the closest one, musql (Go, SQLite oracle), compares outcome kind, column names, and typed values, and counts "declined" separately from "wrong". Turso skips on both sides what either cannot prepare, so states never drift. §4.3 sketches a harness for slice 1: hand-written scripts, a Hegel generator of slice-1 shapes, a one-token mutator for the rejection rule, fuzzing of the tokenizer, and table snapshots after each change and after replay.
- **API.** `database/sql` runs an operation up to three times when the driver returns `driver.ErrBadConn` (`sql.go` 1574-1589), which would run a single-statement insert again after an unknown commit outcome; such errors must never wrap it. `driver.Tx.Commit` takes no context, `DB.Close` takes none, and nothing in `database/sql` carries the stop of an instance. The evidence favours the agreed driver as the main interface, built as a thin adapter over an own API that carries what the driver cannot.
- **Layers.** The core's catalogue (SQLite's schema in the cache, through `Tx.Table`) and errors cover slice 1's cases, but the engine must format SQLite's messages from the syntax tree, raise `UNIQUE` itself before `Tx.Upsert`, and choose the order of its checks. For slice 1 a bytecode machine and a tree of iterators hardly differ; SQLite's bytecode is not a stable interface by its own documentation, and the Go engines surveyed all use iterators.

Open for Markus, collected from the sections: whether single-quoted names are rejected (§1.2); what "any integer value" includes (§3.2); what an error promises and how "not supported" is marked (§3.4); whether SQLite may classify rejected statements in production (§2.4, option 7); whether the own API is public (§5.5).

## 1. The tokenizer

### 1.1 How SQLite tokenizes (V, `sqlite3.c` 186493-187880, `tokenize.c`)

SQLite's tokenizer is one function, `sqlite3GetToken(z, &type)` (`sqlite3.c` 187252). It returns the length of the token at `z` and its type. It looks only at bytes: a table `aiClass` (186554) maps the first byte to one of 31 classes, and a `switch` on the class reads the rest. The parser loop `sqlite3RunParser` (187579) calls it once per token and feeds each token to the parser at once, so tokenizing and parsing are interleaved. The facts a Go tokenizer must copy:

- **Whitespace.** A whitespace token starts at space, tab, line feed, form feed, or carriage return (`aiClass`), and then continues over every byte for which `sqlite3Isspace` is true (187264), which also includes the vertical tab 0x0b (`sqlite3CtypeMap`, 24242). So a vertical tab is legal after another whitespace byte and illegal at the start of a token. Run on 3.54.1: `select\v1` fails with `unrecognized token: "\v"` at offset 6, and `select \v1` returns 1.
- **The byte order mark.** The three bytes EF BB BF are whitespace only where a token starts (187554). Run on 3.54.1: a leading BOM is accepted, and `select<BOM>1` fails with `near "select<BOM>1": syntax error`, because the BOM bytes are identifier characters inside a word.
- **Identifier characters** are ASCII letters, digits, `_`, `$`, and every byte from 0x80 up (`IdChar`, 187147, mask 0x46 of `sqlite3CtypeMap`). A non-ASCII space such as U+00A0 is therefore part of an identifier. Run on 3.54.1: `select <U+00A0>` fails with `no such column:  ` (the identifier is the no-break space), and `select 1<U+00A0>` fails with `unrecognized token: "1 "`.
- **Keywords.** A word of ASCII letters and `_` is looked up in a generated perfect hash (`keywordhash.h`, inside `tokenize.c`), without regard to ASCII case. There are 147 keywords (`SQLITE_N_KEYWORD`, 187113). A word with a digit, `$`, or a non-ASCII byte is never a keyword (187519-187527). `strict`, `rowid`, `int`, `integer`, `true`, and `false` are not keywords: `sqlite3_keyword_check` returns 0 for each (run on 3.54.1), and the tokenizer gave `TK_ID` for `strict`, `rowid`, and `int` (run of `tok.c`). `WINDOW`, `OVER`, and `FILTER` are keywords only when the tokens around them say so; the tokenizer looks ahead and behind for them (`analyzeWindowKeyword` and the two after it, 187225-187245).
- **Quotes.** `'…'` is a string, `"…"` and `` `…` `` are identifiers, and each doubles its own quote to escape it (187375-187399). `[…]` is an identifier with no escape (187478). An unterminated quote makes one illegal token that runs to the end of the input. Run on 3.54.1: `select 'abc` fails with `unrecognized token: "'abc"`, and the same for `"`, `` ` ``, and `[`.
- **Blobs.** `x'…'` or `X'…'` with an even number of hex digits; otherwise the token is illegal up to the next quote (187531-187543). Run on 3.54.1: `select x'0'` fails with `unrecognized token: "x'0'"`.
- **Numbers** (187400-187477). Decimal digits, an optional fraction, an optional exponent; `.5` is a number. `0x` followed by a hex digit starts a hex integer. A `_` between digits (the digit separator of 3.46.0) changes the token type to `TK_QNUMBER`, which the parser checks later (`sqlite3DequoteNumber`, 37174). A number followed directly by an identifier character is one illegal token. Run on 3.54.1: `1x`, `1e`, `0x`, and `0xg` fail with `unrecognized token`; `1_` and `1__2` also fail with `unrecognized token`, but at offset -1, because that check runs after the tokenizer.
- **Parameters.** `?`, `?NNN`, `:name`, `@name`, `$name` (with Tcl suffixes `::` and `(…)`), and `#name` (187483-187517). `#N` is accepted only in SQLite's own nested parses; otherwise the parser reports a syntax error (`parse.y` 1210-1228).
- **Comments.** `--` runs to the line feed or the end of input; `/*` runs to `*/` or the end of input, so an unterminated block comment is accepted (187270, 187300). Run on 3.54.1: `select 1 /* open` returns 1. Comments are dropped only while `SQLITE_DBCONFIG_ENABLE_COMMENTS` is on, which is the default; with it off, a comment is an unrecognized token (187675-187688).
- **Operators.** One- and two-byte operators, among them `->` and `->>` (187274), `==`, `<>`, `!=`, `||`, `<<`, `>>`. A lone `!` is illegal.
- **The end of the input** is a NUL byte (`CC_NUL`, 187562). The binding refuses SQL with a NUL in it (`Conn.Prepare`).
- **Illegal tokens** come back as `TK_ILLEGAL`. The parser loop then reports `unrecognized token: "X"`, where X is the token's text (187686), and stops. The message is formatted with `%T`, which records the byte offset of the token for `sqlite3_error_offset` (`sqlite3.c` 33813-33819).
- **The first error in reading order wins, with an exception.** Because tokens are fed one at a time, a syntax error before an illegal token is reported, and the illegal token is never read. Run on 3.54.1: `selec 'abc` fails with `near "selec": syntax error`. The exception is a check that runs in a parser action, described in §2.1.

### 1.2 What s4ql's restrictions remove, and what they do not

The restrictions in the roadmap remove some cases, but not from the tokenizer's job of finding token boundaries:

- **UTF-8 only.** A statement that is not valid UTF-8 is rejected before it is parsed, so the tokenizer only sees valid UTF-8. Bytes from 0x80 up are still identifier characters, byte by byte, as in SQLite. EBCDIC goes away.
- **Double quotes are identifiers only.** The tokenizer already treats `"…"` as an identifier (`TK_ID`). The double-quoted-string fallback happens later, in name resolution, and `SQLITE_DQS=0` switches it off in the oracle. Nothing changes in the tokenizer.
- **No brackets or backticks.** These are still tokens that the tokenizer must recognize: `[a b]` is one token, and rejecting it needs to know where it ends. The rejection is a check on the token's first byte, not a change to tokenizing.
- **Not covered by the restrictions: single-quoted names.** SQLite accepts a string where a name is expected (`nm ::= STRING`, `parse.y` 340). Run on 3.54.1: `create table 'l' ('v1' int primary key) strict, without rowid` succeeds. This is the fallback that [sqlite-docs-inventory.md](sqlite-docs-inventory.md) §4.2 lists as a gap of the oracle. Under "reject, never reinterpret" s4ql would reject it, but the roadmap does not say so yet. **Open for Markus.**

So the tokenizer is the same size with or without the restrictions. The trade-off is between a tokenizer that knows all of SQLite's tokens and one that knows only slice 1's:

- **All of SQLite's tokens.** Every token boundary is SQLite's, so the position of the first error and the text of `unrecognized token: "X"` and `near "X"` can match for any input. The tokenizer can be checked against `sqlite3GetToken` for every input (§1.3). Unsupported tokens such as blobs, floats, and parameters are found and then rejected by the parser or a later pass.
- **Only slice 1's tokens.** Every other byte sequence is an error at once. The boundaries of what it rejects are not SQLite's, so its error positions and texts differ from SQLite's for most rejected inputs, and the tokenizer has to grow in each slice.

The first gives an exact oracle at the token level, which the second cannot use.

### 1.3 How to check a Go tokenizer against SQLite

SQLite has no public tokenizer API. What can be observed:

- **The private tokenizer itself (V, run of `tok.c`).** In the amalgamation, `SQLITE_PRIVATE` is `static`, so a C file that includes `sqlite3.c` can call `sqlite3GetToken` and `sqlite3KeywordCode` and see every token's type and length. `tok.c` did this with the vendored file and `options.h`. A differential test can then feed the same bytes to the Go tokenizer and to `sqlite3GetToken`, token by token, under Go's fuzzer. The cost in trust: the function is not a public interface and may change between versions, but the version is pinned, and a test that breaks on an upgrade is the intended signal. The binding's rule that every C call stays in `sqlite.go` does not apply to a test program; the tests of `internal/sqlite` already build C programs from the same sources (`buildProgram` in `internal/sqlite/sqlite_test.go`). A test program that includes the amalgamation needs its own compile of it, because `sqlite3GetToken` is not in the object file's symbol table. The token type numbers (`TK_ID` 60, `TK_STRING` 118, and so on) are generated per version by Lemon, so a test compares token classes through a mapping, not raw numbers.
- **Error messages and offsets (V).** `unrecognized token: "X"` gives the text of an illegal token, and `sqlite3_error_offset` gives its start. `near "X": syntax error` gives the full text of the token where the parser stopped. At the start of a statement, almost every token is a syntax error, so `<word> …` returns the exact extent of the first token: `select<BOM>1` showed that the BOM joins the identifier. These are end-to-end checks that the differential harness of §4 makes anyway.
- **Column names (V).** The name of an unnamed result column is the source text of its expression, from its first token to its last: `select 1 /* a */ + /* b */ 2` has the column name `1 /* a */ + /* b */ 2`. That shows where an expression's tokens start and end. (The rule for column names without `AS` is itself not documented; [sqlite-docs-inventory.md](sqlite-docs-inventory.md) lists it as a gap.)
- **The keyword set (V).** `sqlite3_keyword_count` (147), `sqlite3_keyword_name`, and `sqlite3_keyword_check` give exactly the tokenizer's keyword set. A test can compare a Go keyword table with it in full.
- **`sqlite3_complete` and `sqlite3_incomplete` are a different scanner (V).** They are in `complete.c` (`sqlite3.c` 187881-188256), not in the tokenizer, and their whitespace is space, tab, CR, LF, and FF, without the vertical tab (188083-188087). `sqlite3_incomplete` is new in 3.54.0 ("Add the sqlite3_incomplete() interface", https://sqlite.org/changes.html). Run on 3.54.1: `select 1;--x` is not complete, because a `--` comment that the input ends inside counts as pending (188107-188116), although the header says "Whitespace and comments that follow the final semicolon are ignored" (`sqlite3.h` 2970). They tell whether literals and comments are closed and whether a statement ends with a semicolon, and nothing about token boundaries. They are a weak oracle, and only for their own job.
- **The authorizer (I).** `sqlite3_set_authorizer` receives table and column names after dequoting, which shows how a quoted name was read. The binding has no authorizer, and a Go callback would need `//export`, which the binding forbids (`internal/sqlite/README.md`, "Symbols"). A callback written in C that collects the names into a buffer would avoid that. Not tried.

The evidence favours the first: it is exact, it covers every byte sequence, and it fits Go's fuzzer. The others remain useful as statement-level checks.

## 2. The parser

### 2.1 How SQLite parses, and where its errors come from (V)

- **Lemon, LALR(1).** `parse.y` (2173 lines at 3.54.1) is turned into `parse.c` by SQLite's own parser generator Lemon (`sqlite3.c` 180743-186492). The parser is a push parser: `sqlite3RunParser` hands it one token at a time (187692).
- **No error recovery.** `parse.y` defines `YYNOERRORRECOVERY` (line 76). The first syntax error ends the parse.
- **The two syntax messages** come from `%syntax_error` (`parse.y` 45-51): `near "X": syntax error`, where X is the text of the token on which the parser had no action, or `incomplete input` when that token is the end of the input. At the end, the loop feeds a synthetic `;` of length 0 (187653-187663), and its text is empty, which selects the second message. Run on 3.54.1: `select * from`, `select * from t where`, `drop table`, `create table t`, and `insert into t (v1) values (1` all fail with `incomplete input`.
- **Keywords as names.** `%fallback ID` (`parse.y` 272-295) lists the keywords that the parser reads as an identifier when the keyword cannot be shifted where it is: 73 tokens in this build, where `CTIME_KW` and `LIKE_KW` each stand for several words. The token classes `id`, `ids`, and `idj` (326-334) add `INDEXED`, `STRING`, and the join keywords. So `create table abort (…)`, `create table left (…)`, and `create table key (rowid int primary key) …` succeed, and `create table select (…)` fails with `near "select": syntax error` (run on 3.54.1). `%wildcard ANY` (296) matches any token; it is used only for the arguments of virtual tables.
- **Checks inside the grammar's actions.** Some errors come from C code that runs when a rule is reduced, in the middle of parsing: `unknown table option: X` (`parse.y` 235-249), the check of digit separators, and, through `sqlite3StartTable`, `table X already exists` and `object name reserved for internal use: X`. The parse loop checks for an error after each token (187696). So these errors can win over a syntax error or an illegal token that comes later. Run on 3.54.1, with `t1` existing: `create table t1 (v1 int primary key) 'abc` fails with `table t1 already exists`, not with the unterminated string.
- **The last message raised while one token is handled wins.** `sqlite3ErrorMsg` replaces any earlier message (`sqlite3.c` 37103-37104), and the loop checks only between tokens. When one token triggers several reductions that each raise an error, the last one is reported. Run on 3.54.1: `create table t2 (v1 int primary key) strict, bar, without rowid` fails with `unknown table option: bar` (the reduction happens on the next `,`, and the loop stops), but `create table t2 (v1 foo primary key) strict, without rowid, bar` fails with `unknown datatype for t2.v1: "foo"`, and `create table t2 (v1 int) strict, without rowid, bar` with `PRIMARY KEY missing on table t2`: there the option is reduced on the end of input, and `sqlite3EndTable` runs in the same step and overwrites the message.
- **Errors after the parse.** Name resolution and code generation run after the grammar's actions. `no such table`, `table t has no column named v2`, `2 values for 1 columns`, and `hex literal too big` come from there. Run on 3.54.1: `insert into t1 (v1) values (0x1ffffffffffffffff, 1)` fails with `2 values for 1 columns`, and the same statement with one value with `hex literal too big: 0x1ffffffffffffffff`.

### 2.2 What it takes for another parser to say `near "X"` for the same input (I, from LR theory, to be confirmed by differential tests)

An LALR(1) parser without error recovery has the correct-prefix property: it stops at the first token that cannot extend any valid statement of the grammar, and never shifts a wrong token. Lemon's default reductions can reduce before it notices, but they do not move the error to another token. So the token in `near "X"` is a property of the grammar's language, after `%fallback` and the token classes, not of Lemon. Any parser that recognizes exactly the same language and also stops at the first token that cannot continue reports the same X. A recursive-descent parser with one token of lookahead can have the property, if every choice point knows the full set of tokens that may follow.

Three things make it hard:

- **A subset grammar stops earlier.** s4ql's parser knows slice 1's grammar, not SQLite's. For an input that SQLite parses further, it stops at a token that SQLite accepts. `select * from t1 where` is `incomplete input` in SQLite; a slice-1 parser stops at `where`. To give SQLite's syntax message for an input outside the subset, the engine needs a recognizer for SQLite's whole grammar, or must not claim SQLite's message for such inputs.
- **Keywords as names depend on the parser state.** Whether `abort`, `key`, or `left` is a name depends on what the grammar allows at that point. A hand-written parser has to make the same choice at each point; a parser generated from `parse.y` gets it for free.
- **Checks in actions, and their order.** For an exact match with several errors in one statement, the engine has to run its checks at the same moments as SQLite (§2.1). This affects only inputs with more than one error.

### 2.3 What exists in Go (V unless marked; sources read on 2026-11-13, no third-party code was run)

- **github.com/sqlc-dev/meyer**, MIT, tags v0.1.0 to v0.1.2, last commit 2026-08-25, seven commits by one author, two stars, no dependencies. A hand-written recursive-descent port of `parse.y`: "The grammar is SQLite's src/parse.y ... Every non-trivial function here names the rule or rules it implements" (`parser/parser.go` 1-7). It reports SQLite's own syntax messages, `incomplete input` and `near "X": syntax error`, with a byte offset (`parser.go` 333-343); its `Error()` adds `line:col:` in front, and the raw message is a field. Fallback keywords are sets derived from `parse.y`, single-quoted names are kept as such, digit separators follow `sqlite3DequoteNumber`, and `strict` and `without rowid` are table options (`ast/ddl.go`). Every node has a byte span, and an identifier keeps its dequoted name, its raw text, and its quote (`ast/expr.go` 22-27). Its README claims "21,326 of 21,326 cases" extracted from SQLite's test scripts, with expectations from `sqlite3_prepare_v2` on an empty in-memory database, plus a fuzz target and a differential tool against a live SQLite; these claims were not checked here. It pins SQLite 3.53.4 (`parser/testdata/README.md`). Between 3.53.4 and 3.54.1, `tokenize.c` is identical and `parse.y` differs by 20 diff lines, all inside C action code and none in a grammar rule (V, diff of the two tags). Most errors raised in grammar actions and after parsing are outside its scope (`PLAN.md` 431-432). sqlc replaced its ANTLR parser with meyer on its main branch on 2026-08-01 (sqlc pull request #4535); sqlc's latest release, v1.31.1, still has the ANTLR parser.
- **github.com/rqlite/sql**, MIT, maintained (last commit 2026-10-16), 180 stars. Hand-written recursive descent, not derived from `parse.y`; fallback keywords are a hand-kept list (`token.go` 481); no digit separators (`scanner.go`). Errors are Go style, such as `1:7: expected expression, found 'EOF'` (`parser.go` 4028-4052), never SQLite's. An identifier keeps its dequoted name and a quoted flag, not its raw text. **github.com/benbjohnson/sql** is a 2024 fork-snapshot of it, not maintained.
- **modernc.org/sqlite**, BSD-3, v1.62.0 with SQLite 3.54.1. Its parser is SQLite's Lemon output transpiled to Go, in unexported functions; there is no Go parser or AST API. Its messages are SQLite's, but only through preparing a statement.
- **ANTLR grammars** derived from Bart Kiers' MIT grammar in `antlr/grammars-v4` (`sql/sqlite`): sqlc v1.31.1, `stephenafamo/sqlparser` ("not guaranteed to be 100% accurate"), and others. Errors are ANTLR's ("no viable alternative at input …"). The ANTLR Go runtime was last released in 2024.
- **Others:** `joaobnv/mel` (BSD-3, "experimental", SQLite 3.46.1 in the package path, errors `expecting %s, got %s`); `ncruces/go-sqlite3/util/sql3util` (only `CREATE TABLE` and `ALTER TABLE`, a C parser compiled to Wasm, returns metadata, not a tree); chirst/cdb (a learning project with its own subset).
- **Lemon for Go.** Lemon itself emits only C: "Lemon is an LALR(1) parser generator for C" (`doc/lemon.html`). `gopikchr/golemon` is a Go port of `lemon.c` with a Go template, MIT, that supports `%fallback`, `%wildcard`, and `%token_class` and tracks upstream, but its README says "there are almost certainly further bugs ... I intend no support for this project". `kyleconroy/golite` tried golemon on `parse.y` in 2022 and was abandoned with a failing build.
- **goyacc** (`golang.org/x/tools/cmd/goyacc`, last change 2026-04-28) is LALR(1) and has no `%fallback`, `%wildcard`, or `%token_class`; its default message is `syntax error: unexpected X` (`yacc.go` 3283-3293), and a grammar's own error rule would have to build `near "X"`. vitess (`vitess.io/vitess/go/vt/sqlparser`), dolthub/vitess, and TiDB's parser (`pkg/parser`, a goyacc fork from modernc) all use goyacc grammars for MySQL, with messages like `… at position N near 'X'`.

On dependencies: the spec has no rule. The module depends on the AWS SDK and `google.golang.org/protobuf` (`go.mod`), and the project wrote its own SQLite binding so that "no third-party code sits between the engine and SQLite" (`internal/sqlite/README.md`, "Why our own binding").

### 2.4 The options

1. **A hand-written recursive-descent parser for slice 1's grammar only, grown per slice.** Small and readable; nothing unlisted is ever parsed. It cannot tell a SQLite syntax error from an unsupported construct (§2.2), so its error for an unlisted input is its own. Checked against the oracle only on the inputs it accepts, and on its rejections only as "rejected".
2. **A hand-written port of all of `parse.y`, s4ql's own, in the way meyer is built.** Every syntax message can match SQLite for every input, and acceptance can be compared with `sqlite3_prepare_v2` over SQLite's own test scripts, its fuzz corpora, and Go's fuzzer. The cost in trust: a large surface that slice 1 does not execute, which must still be right, because its job is to classify. The "supported?" pass of §2.5 then decides what runs.
3. **meyer as a dependency, or a copy of it in the repository.** The same properties as option 2, and it exists. The costs: a young project with one author and seven commits, pinned to 3.53.4 (the grammar rules and the tokenizer are unchanged in 3.54.1, as above), whose claims were not checked here; its tree shape has no oracle ("There is no upstream parse-tree oracle", README); and third-party code would sit between the application's SQL and the engine. A copy that s4ql owns and checks against its own pinned SQLite removes the dependency, but not the need to check it.
4. **A parser generated from `parse.y` with a Lemon for Go (golemon), with the actions rewritten in Go.** Lemon's own table construction gives Lemon's own decisions, including `%fallback` and conflict resolution. The cost: golemon's author promises no support, the one attempt on `parse.y` was abandoned, and `parse.y`'s actions are C that call SQLite's code generator, so all of them must be rewritten.
5. **goyacc or ANTLR with a translated grammar.** goyacc lacks `%fallback`, so the grammar or the lexer would need rewriting, and every such change is a place where the language can drift from SQLite's (I). ANTLR is ALL(*), not LALR, and its runtime is quiet since 2024. Neither brings an advantage over options 2 to 4.
6. **rqlite/sql.** Maintained and complete for most statements, but neither its errors nor its keyword handling are SQLite's, and its tree does not keep raw names. Its messages would need replacing throughout.
7. **SQLite as the classifier.** When s4ql rejects a statement, it could prepare it with the binding on an empty connection to learn whether SQLite would give a syntax error, and report that. The spec says "SQLite does not run the application's SQL"; preparing is not running, but it is SQLite parsing the application's SQL in production. A design question for Markus, not a technical one. Name errors would need care, because an empty connection has no tables.

The evidence favours a full-grammar parser that s4ql owns (option 2), with meyer (option 3) as a reference to compare against and borrow tests from, or as a starting point that s4ql then owns. Reason: only a full grammar can give SQLite's syntax message for an input outside the subset and tell it from "not supported"; a hand-written parser keeps raw token text and positions as §3.1 needs; meyer shows that the approach can be checked against `sqlite3_prepare_v2` at scale. Option 1 is the smaller start if Markus decides that s4ql does not promise SQLite's syntax messages for unsupported inputs (§3.4).

### 2.5 Where to enforce "reject what is not listed"

Two places are possible, and they can be combined.

- **In the grammar.** The parser accepts only the listed shapes, and anything else is a syntax error. Each slice extends the grammar. Simple, and nothing unlisted can reach later layers. But the error for an unlisted input is the subset parser's, and it cannot tell "not supported" from "a syntax error in SQLite" (§2.2), because it does not know SQLite's grammar.
- **In a pass after parsing.** The parser accepts all of SQLite's grammar into a full syntax tree, and a separate pass, a list of what slice 1 allows, rejects the rest as not supported. The parser is then checked against SQLite's own acceptance as a whole, and the list is small, explicit, and reviewable per slice: it is the roadmap in code. The cost: the whole grammar must be parsed correctly, including the parts that are never executed, and a bug in the pass lets an unlisted construct through to layers that may not handle it. A rule that later layers fail loudly on any node they do not know limits that.
- **Combined.** A full recognizer that only classifies (syntax error in SQLite, or valid but not supported), next to a subset parser that builds the tree for what is supported.

The evidence favours a check after parsing, or the combined form, over the grammar alone, because only they can tell an s4ql rejection from a SQLite syntax error, which §3 shows matters for the errors.

## 3. SQLite's exact errors for slice 1

All results in this section are run on 3.54.1 with `probe.c`, on a fresh `:memory:` database, after `create table t1 (v1 int primary key) strict, without rowid` where the case needs it. "Prepare" or "step" says which call failed; with the binding, a prepare error comes from `Conn.Prepare` and a step error from `Stmt.Step`. The offset is `sqlite3_error_offset`.

### 3.1 The errors that slice 1 names

| Case | Statement | Message | Primary | Extended | At | Offset |
|---|---|---|---|---|---|---|
| Key inserted twice | `insert into t1 (v1) values (1)`, twice | `UNIQUE constraint failed: t1.v1` | 19 `SQLITE_CONSTRAINT` | 1555 `SQLITE_CONSTRAINT_PRIMARYKEY` | step | -1 |
| Table exists | `create table t1 (…) strict, without rowid`, again | `table t1 already exists` | 1 `SQLITE_ERROR` | 1 | prepare | 13 |
| Read a missing table | `select * from t2` | `no such table: t2` | 1 | 1 | prepare | -1 |
| Write a missing table | `insert into t2 (v1) values (1)` | `no such table: t2` | 1 | 1 | prepare | -1 |
| Drop a missing table | `drop table t2` | `no such table: t2` | 1 | 1 | prepare | -1 |

How names appear in these messages, which a Go engine must copy:

- **`already exists` quotes the name as written, with its quotes.** Its format is `%s %T already exists` (`sqlite3.c` 128126), where `%T` is the raw token. `create table T1 …` after `t1` gives `table T1 already exists`, `create table "t1" …` gives `table "t1" already exists`, and `create table main."t10" …` gives `table "t10" already exists` at offset 18. So the syntax tree must keep the raw text of a name token, not only its value.
- **`no such table` uses the name as written, without quotes.** `drop table T2` gives `no such table: T2`, `select * from "t2"` gives `no such table: t2`, and a schema name is kept: `drop table main.zz` gives `no such table: main.zz`.
- **`UNIQUE constraint failed` uses the stored names**, whatever the statement wrote: after `create table Foo (Bar int primary key) …`, `insert into FOO (BAR) values (1)` twice gives `UNIQUE constraint failed: Foo.Bar`. Names are not quoted: a table `a b` with a column `c"d` gives `UNIQUE constraint failed: a b.c"d`.
- **Names compare without regard to ASCII case only.** `ÆØÅ` and `æøå` are two tables. This matches `fold` in `internal/core/cache.go`.

### 3.2 Other errors that slice 1's statement shapes can produce

The roadmap says "any table name, column name, and integer value", in fixed shapes. These shapes can produce more errors than the five above (run on 3.54.1):

| Statement | Message | Primary / extended | At |
|---|---|---|---|
| `insert into t1 (v2) values (1)` (a column the table does not have) | `table t1 has no column named v2` | 1 / 1 | prepare |
| `create table sqlite_x (…) strict, without rowid` (also `SQLITE_X`) | `object name reserved for internal use: sqlite_x` | 1 / 1 | prepare |
| `drop table sqlite_schema` (also `sqlite_master`) | `table sqlite_master may not be dropped` | 1 / 1 | prepare |
| `insert into t1 (v1) values (9223372036854775808)` | `cannot store REAL value in INT column t1.v1` | 19 / 3091 `SQLITE_CONSTRAINT_DATATYPE` | step |
| `insert into t1 (v1) values (0x1ffffffffffffffff)` | `hex literal too big: 0x1ffffffffffffffff` | 1 / 1, offset 28 | prepare |
| `insert into t1 (v1) values (1_)` | `unrecognized token: "1_"` | 1 / 1, offset -1 | prepare |
| `insert into t1 (v1) values (null)` | `NOT NULL constraint failed: t1.v1` | 19 / 1299 `SQLITE_CONSTRAINT_NOTNULL` | step |

And what SQLite does with integer literals, which bears on what "any integer value" means:

- `9223372036854775807` and `-9223372036854775808` are stored as integers. `9223372036854775808` and `-9223372036854775809` are real numbers, because a literal outside the 64-bit range is a float ([sqlite-docs-inventory.md](sqlite-docs-inventory.md) §1.2), and the insert fails as in the table.
- `-1`, `- 3`, and `+2` are accepted: they are unary operators applied to a literal, so they are expressions, not literals.
- `0x10` stores 16, and `0xffffffffffffffff` stores -1 (two's complement).
- `1_000` stores 1000, `00012` stores 12, `1e3` and `2.0` store 1000 and 2, `'7'` stores 7, and `2.5` and `'a'` fail with `SQLITE_CONSTRAINT_DATATYPE`.

**Open for Markus:** which of these slice 1 accepts. Its listed shape is "an integer value", which could mean a decimal integer literal only, or also a sign, hex, and digit separators. A negative number needs unary minus, which is an expression.

### 3.3 What SQLite says for inputs near slice 1 that s4ql rejects

Accepted by SQLite (each succeeds on 3.54.1): a second column; `integer` spelled out; `create table c (v1 int primary key)` with no options; `strict` alone; `without rowid` alone; `without rowid, strict` in the other order; `any` as the type; a name starting with `s4ql_`; `if not exists`; `temp`; `main.` before the name; `primary key (v1)` as a table constraint; `[k]` and `` `v1` `` and `'l'` as names; `not null` or `asc` on the column; `insert into d values (1)` without a column list; several rows; `insert … select`; `where`; `select v1`; `select * from main.d`; `drop table if exists` of a missing table; `begin` and `commit`; `replace into`, `insert or replace`, `on conflict do nothing`, and `returning *`; `select * from t9 as x`, `select t9.* from t9`, `select * from (t9)`, `select all *`. Also `insert into t (v1, v1) values (1, 2)`, which SQLite accepts.

Rejected by SQLite, with its message (1 / 1 at prepare unless noted):

| Input | Message |
|---|---|
| `create table t (v1 int) strict, without rowid` | `PRIMARY KEY missing on table t` |
| `create table t (v1 int primary key) strict without rowid` | `near "without": syntax error` (offset 43) |
| `create table t (v1 int primary key) strict, without` | `incomplete input` |
| `… strict, without rowid, foo` | `unknown table option: foo` |
| `… "strict", without "rowid"` | `unknown table option: "strict"` |
| `create table t (v1 foo primary key) strict, without rowid` | `unknown datatype for t.v1: "foo"` |
| `create table t2 (v1 int(10) primary key) strict, without rowid` | `unknown datatype for t2.v1: "int(10)"` |
| `create table t (v1 primary key) strict, without rowid` | `missing datatype for t.v1` |
| `create table select (…)` | `near "select": syntax error` (offset 13) |
| `create table zz.t11 (…)` | `unknown database zz` (offset 13) |
| `drop table if exists sqlite_schema` | `table sqlite_master may not be dropped` |
| `insert into t (v1) values (1, 2)` | `2 values for 1 columns` |
| `commit` outside a transaction | `cannot commit - no transaction is active` (at step) |

Two things follow for how s4ql words its own "not supported" errors:

- **For most inputs that s4ql rejects, SQLite gives no error at all.** A second column, `integer`, `where`, `if exists`, and a table without `strict` or `without rowid` all succeed in SQLite. So there is no SQLite message to match, and s4ql's error is its own by necessity.
- **Some inputs that s4ql rejects are also SQLite errors.** `create table sqlite_x …` fails in SQLite with `object name reserved for internal use`; `create table t (v1 int) strict, without rowid` fails with `PRIMARY KEY missing`. For these, s4ql can either give SQLite's error or its own "not supported".

### 3.4 The open question for Markus

Which parts of an error s4ql promises to match, and how a "not supported" error is told apart. The facts that bear on it:

- **The result codes are documented as stable; the messages are not.** "The names and numeric values for existing result codes are fixed and unchanging" (https://sqlite.org/rescode.html), while no page promises stable message text, and 3.20.0 changed messages ([sqlite-docs-inventory.md](sqlite-docs-inventory.md) §1.7).
- **The messages are still deterministic for a pinned version,** and the oracle is pinned. The project already compares messages exactly between the binding and the shell (`internal/sqlite/README.md`, "Tests").
- **The extended code carries little for slice 1.** Every error but the two constraint errors is 1 / 1 (`SQLITE_ERROR`); only the message tells `no such table` from `already exists` from a syntax error. Matching the code alone would accept an engine that says "no such table" for a table that exists.
- **Matching messages exactly is harder in three places:** the raw token text of names (§3.1), the order of errors in a statement with several (§2.1), and syntax errors for inputs outside the subset (§2.2).
- **The offset** (`sqlite3_error_offset`) is set only for some errors (those formatted with `%T`), and the binding does not expose it.

Options:

1. **Match code, extended code, and message, exactly, for every input that SQLite rejects and that has slice-1 shape; give "not supported" for everything else.** Strong and testable. "Slice-1 shape" must then be defined precisely, and the inputs that both reject (the second bullet of §3.3) need a rule: the evidence suggests giving SQLite's error where the shape is supported and SQLite rejects it, and "not supported" where the shape is not supported.
2. **Match code and extended code always, message only where it is cheap.** Weaker: as shown above, it does not tell the slice-1 errors apart.
3. **Match everything, including SQLite's syntax errors for inputs outside the subset.** Needs a full recognizer of SQLite's grammar (§2.2), or asking SQLite.

How to tell "not supported" apart, whichever option:

- **A distinct Go error type or sentinel,** say `s4ql.ErrNotSupported`, wrapped by every rejection, that `errors.Is` finds. The result code it carries is a question too: `SQLITE_ERROR` (1) with an s4ql message, as SQLite does for its own refusals, or no SQLite code at all. A code of 1 lets code that only looks at SQLite codes treat it as an error of the statement, which is what it is.
- **A message that can never be one of SQLite's,** for example one that starts with `s4ql:` or says "not supported by s4ql", and names the construct and, where possible, its position. SQLite's own refusals of features have the form `X is not supported` in a few places (NV: not surveyed), so a prefix that names s4ql removes any doubt.
- **The position.** Giving the byte offset of the first unsupported token would match what `sqlite3_error_offset` gives for token errors.

## 4. How SQLite-compatible engines test against their reference

The repositories were read at their heads on 2026-11-13 (V unless marked); paths are relative to each repository. What sqllogictest, SQLancer, and SQLsmith are is in [testing-strategies.md](testing-strategies.md) §2 and [external-test-suites.md](external-test-suites.md) §2 and §4.

### 4.1 What each project really does

- **samyfodil/musql** (Apache-2.0, created 2026-07): a Go engine that reimplements SQLite's SQL with `mattn/go-sqlite3` as its oracle, the closest setup to s4ql's. Its harness is a separate Go module (`compat-harness/go.mod`) that runs a JSON list of statements on one connection per engine and compares the JSON byte for byte: the kind of outcome (rows, ok, error), column names, and each value tagged by storage class, rows in the order returned. Error text is compared only when `COMPAT_ERRTEXT=1`, set for one corpus ("drivers word errors differently"). Corpora: SQL mined from all 1474 TCL `.test` files of SQLite, sqllogictest files with the expected results thrown away and re-run on SQLite, Turso's `.sqltest` files with SQLite as the oracle, and a seeded random generator. A refusal is its own bucket: the tally is `pass, mutualReject, propertyMatch, outOfScope, unsupported, wrong, panics` (`tcl_test.go`), "unsupported" means the engine declined a statement the oracle ran, and "a rejection is not an answer", so it is never scored wrong. The run fails on any wrong result above a per-file allowlist, and on any panic. Its README claims 73,855 mined statements, none wrong, 16 declined (not checked). NV whether CI runs it.
- **Turso** (Rust rewrite of SQLite; COMPAT.md tracks SQLite 3.50.4: "Any deviation … not explicitly documented as an opt-in extension is considered a bug").
  - A deterministic simulator (`testing/simulator/`) with a `--differential` mode against SQLite; rows are compared as a multiset; error against success fails; both erroring is only logged, with a TODO that the messages "might be different". CI runs 1000 tests per run.
  - A differential fuzzer (`testing/differential-oracle/`) with schema-aware generators, one on proptest. Values keep their storage class; after each change it compares snapshots of every table ordered by rowid. Before a statement runs, it runs `EXPLAIN <stmt>` on both engines, and if either fails to prepare, the statement is skipped on both, so the two states never drift. Messages are not compared.
  - SQLite's TCL suite through a `sqlite3` command backed by Turso: 854 files, 230 pass, 600 fail, 5 hang, 10 skip at the head; the list of failing files "only ever shrinks". `catchsql` there compares exact message text.
  - A test DSL with hand-written `expect error {pattern}` (substring or regex), validated against the `sqlite3` shell, and "matrix" cases compared live: "same rows in the same order, or both rejecting the statement (error messages are not compared)" (`src/matrix_oracle.rs`).
  - cargo-fuzz targets compared with rusqlite, a small weighted grammar generator (`tests/fuzz/grammar_generator.rs`), SQLancer with a list of ignored error substrings such as "not supported" and "syntax error", SQLRight, and Antithesis templates.
- **libSQL** (a C fork of SQLite 3.47.0): no differential testing. It runs SQLite's full TCL suite in CI and adds TCL and Rust tests for its additions.
- **rqlite**: none. It runs real SQLite; its parser `rqlite/sql` only rewrites `RANDOM()` and `'now'`, and a statement it cannot parse goes to SQLite unchanged (`command/sql/processor.go`).
- **go-mysql-server and Dolt**: hand-written expectations (`enginetest/queries`), with an expected error as a GMS error kind or an exact `err.Error()` string. A MySQL harness runs the same tests against a real MySQL only when `MYSQL_TEST_HOST` is set, and only for some suites; there an error kind only checks that some error occurred. Error codes map to MySQL's (`sql.CastSQLError`), while messages are GMS's own (`table not found: %s`), so only codes match. SQLite's sqllogictest corpus with results regenerated on MySQL (dolthub/sqllogictest) counts an unsupported statement as not ok.
- **DuckDB**: its sqllogictest runner passes `statement error` if the message contains the expected text or matches a regex, and an empty expectation accepts any error (`result_helper.cpp`). SQLite's corpus runs nightly with settings that imitate SQLite, and unsupported features are handled by excluding whole files with reasons (`test/sqlite/test_sqllogictest.cpp`). No live comparison with another engine was found.
- **CockroachDB**: logic tests check the SQLSTATE exactly and the message by regex (`pkg/sql/logictest/logic.go`). Its differential core `cmpconn` compares rows in order and, with `ignoreSQLErrors`, ignores and counts any query on which either side errors; errors are never compared. Comparison with Postgres (`pkg/cmd/smithcmp`) is manual. pg_regress runs against a checked-in snapshot of expected differences.
- **GlueSQL**: its own dialect, no reference engine; errors compared as its own error values.
- **Grammar-driven generators.** `pingcap/go-randgen` runs grammar-generated SQL on two MySQL connections and reports differences, comparing errors only as error or no error; it has no licence file (GitHub's licence API returns 404), so it cannot be used. SQLRight and Squirrel (MIT) were last pushed in 2022 and 2023.

### 4.2 Patterns

- **Nobody else compares error messages in their live differential tests.** musql leaves them off by default, Turso compares only error against success, CockroachDB ignores errors. Messages are matched only in fixed expectations: Turso's TCL runs, DuckDB's and CockroachDB's substring or regex matches. If s4ql compares exact messages live, as §3.4 option 1 asks, it is stricter than every project surveyed. That is possible because the oracle is the real C code at a pinned version, in the same process.
- **"The reference accepts it, we don't" is its own outcome** in the projects that test most: musql's "unsupported" bucket, Turso's `EXPLAIN` pre-check that skips the statement on both sides, Turso's and CockroachDB's shrinking lists. Counting it as a failure (Dolt) mixes coverage with correctness.
- **State is compared after changes,** not only query results: Turso snapshots every table.
- **Corpora come from SQLite itself:** its TCL scripts and its sqllogictest files, with the expected results produced again by the real SQLite, so no stored expectation is trusted.

### 4.3 A differential harness for slice 1

What it could look like, drawn from the above:

- **Two engines, one script.** A script is a list of statements. Each statement runs against s4ql, through the same entry point an application uses (the own API, and in a second run the `database/sql` driver), and against SQLite through `internal/sqlite` on `:memory:` or a file, set up as the oracle (`SQLITE_DQS=0`, foreign keys on, extended codes; [sqlite-docs-inventory.md](sqlite-docs-inventory.md) §4.2).
- **What is compared, per statement:**
  - the outcome kind: rows, ok, error, or "not supported";
  - for rows: the column names, the number of columns, and each value with its storage class, in order, since slice 1 promises key order;
  - for an error: the primary and extended code and the message, to the extent Markus decides (§3.4), and whether it came at prepare or at step if the API shows that;
  - for "not supported": nothing from the oracle; the oracle does not run the statement, as in Turso's pre-check, so the two states stay the same. Whether SQLite would have accepted it is recorded, which gives musql's buckets: supported and equal, both reject, s4ql declines what SQLite runs, s4ql declines what SQLite also rejects, and wrong.
  - after each statement that can change something, every table on both sides in key order (Turso's snapshot), and, as the roadmap asks, the same after a reopen with an empty cache that replays the log.
- **Any wrong result fails the run.** A decline is counted, never wrong.
- **Where statements come from:**
  - **Hand-written cases** for every row of §3.1 to §3.3, each a fixed script. These are the regression tests and the documentation of the slice.
  - **A grammar-driven generator** of slice 1's shapes, written as Hegel generators so that a failure shrinks ([testing-strategies.md](testing-strategies.md) §1): random names from a pool that mixes case, quotes, non-ASCII letters, and fallback keywords such as `abort` and `left`; integers at the edges of the 64-bit range; and the statements in random order, so that tables exist or not and keys repeat. Every generated statement is in slice 1's shape, so the expected outcome is never "not supported".
  - **A mutating generator** that changes a valid statement by one token (a second column, `integer`, `where`, `if exists`, a missing `strict`), to check that each becomes "not supported" and never runs. This tests the "reject what is not listed" rule.
  - **Go's fuzzer** on raw bytes for the tokenizer against `sqlite3GetToken` (§1.3), and for the whole front end against `sqlite3_prepare_v2`'s accept or reject.
  - **Corpora:** SQL mined from SQLite's TCL scripts, as musql and meyer do. For slice 1 nearly all of it is "not supported", which tests the rejection path at scale.
- **Determinism.** s4ql's side can run inside the simulation of `internal/sim`, so that the same seed replays the same run, and a failure prints its seed and its shrunk script.

## 5. The public Go API

### 5.1 What is already agreed, and where

On 2026-11-10 Markus agreed that `database/sql` is the main interface, that the driver is not registered by a blank import, and that an explicit top-level `Register` function exists for code that needs `sql.Open` by name. The open function builds a `driver.Connector` and passes it to `sql.OpenDB`, so the main path needs no registration (`docs/conversations/2026-11-10-kickoff.txt`, around lines 6107-6190). This is not written in `spec.md` or `todo.md`. Whether an own API exists next to the driver was not discussed there.

What the spec fixes that bears on the API: one instance holds the lease; opening does not wait for it, and a separate call waits until the instance serves (`DB.Wait` in the core); an instance without a valid lease fails reads and new writes at once with a temporary error (`core.ErrNoLease`); a commit can end with "outcome unknown" (`core.ErrOutcomeUnknown`) if the caller gives up; a local I/O failure stops the instance for good and signals the stop (`DB.Done`, `DB.Err`); `DB.Close(ctx)` releases the lease. The spec also says "The number of connections in the database pool sets how many reads run at once", which could mean the `database/sql` pool or the core's `Options.Readers`. In slice 1 each statement is its own transaction.

### 5.2 What `database/sql/driver` asks of a driver, in Go 1.27.3 (V, `go doc database/sql/driver` and `$GOROOT/src/database/sql`)

- **Interfaces.** The package doc: "Drivers should implement Connector and DriverContext interfaces"; "All Conn implementations should implement the following interfaces: Pinger, SessionResetter, and Validator"; with context support, "ExecerContext, QueryerContext, ConnPrepareContext, and ConnBeginTx". New in Go 1.27: `RowsColumnScanner`, which scans into the caller's destination, and "As of Go 1.27, database/sql will not call Next if a Rows implements RowsColumnScanner".
- **`sql.OpenDB(connector)`** never connects by itself; the pool calls `Connector.Connect` when it needs a connection. If the connector implements `io.Closer`, `DB.Close` calls it. `io.Closer.Close` takes no context, while `core.DB.Close` takes one.
- **Isolation and read-only.** `ConnBeginTx.BeginTx` "must check opts.Isolation … If the driver does not support a non-default level and one is set … an error must be returned", and the same for `ReadOnly`. A driver without `ConnBeginTx` gets the same refusal from `database/sql` (`ctxutil.go` 97-119).
- **`driver.Tx.Commit()` takes no context.** `sql.Tx.Commit` first returns `tx.ctx.Err()` without calling the driver if the context of `BeginTx` is done, then cancels the transaction's own derived context, and only then calls the driver's `Commit` (`sql.go` 2292-2325). The driver's `BeginTx` receives the caller's context itself, before `database/sql` derives one (`beginDC`, `sql.go` 1904-1931), so a driver can keep that context and pass it to `core.Tx.Commit`. The caller "giving up" in the spec's sense is then the end of the context given to `BeginTx`.
- **A rollback when the context ends.** When the `BeginTx` context ends before `Commit`, a goroutine of `database/sql` rolls the transaction back (`awaitDone`, `sql.go` 2214-2227). It keeps the connection only if the conn implements both `SessionResetter` and `Validator` (`sql.go` 1908-1910).
- **`driver.ErrBadConn` makes `database/sql` run the operation again.** `DB.retry` (`sql.go` 1574-1589) tries twice on a pooled connection and once more on a new one when the error `errors.Is` `ErrBadConn`. It wraps `DB.PingContext`, `DB.ExecContext`, `DB.QueryContext`, `DB.PrepareContext`, `DB.BeginTx`, `DB.Conn`, `Stmt.ExecContext`, and `Stmt.QueryContext` (`sql.go` 904, 1603, 1676, 1746, 1878, 1954, 2653, 2797). `Tx.Commit` is not retried; an `ErrBadConn` from it only discards the connection (`sql.go` 2320). The doc of `ErrBadConn`: "To prevent duplicate operations, ErrBadConn should NOT be returned if there's a possibility that the database server might have performed the operation."
- **Named parameters** arrive as `driver.NamedValue{Name, Ordinal, Value}`; `NamedValueChecker` controls which Go types are accepted. Slice 1 has no parameters.
- **`Result`** has `LastInsertId` and `RowsAffected`. In SQLite, "Inserts into WITHOUT ROWID tables are not recorded" by `last_insert_rowid` ([sqlite-docs-inventory.md](sqlite-docs-inventory.md) §1.4), so for s4ql it is always 0 or an error.

The hazard for s4ql: in slice 1, `DB.ExecContext("insert …")` runs one statement as its own transaction, with its commit inside the call. If that commit's outcome is unknown and the driver returned an error that wraps `ErrBadConn`, `database/sql` would run the insert again on another connection, which for an insert that did commit gives `UNIQUE constraint failed` for a row the application wrote once, and for later statements such as `update x = x + 1` a double effect. So an error that wraps `core.ErrOutcomeUnknown` must never wrap `ErrBadConn`. The same holds for `ErrNoLease`: running the statement again on another connection of the same instance cannot help, because the lease belongs to the instance. The drivers surveyed return `ErrBadConn` only from `Ping` or `ResetSession` (below), which is the safe pattern.

### 5.3 How other embedded Go databases shape their API (V, read in source at the heads of 2026-11-13 unless noted)

| Project | API | Registration | Notable driver interfaces | `BeginTx` options | `ErrBadConn` |
|---|---|---|---|---|---|
| mattn/go-sqlite3 | driver only; extras on `*SQLiteConn` through `sql.Conn.Raw` | `init()` registers `sqlite3` (`sqlite3.go` 437-441); no Connector | Pinger, ExecerContext, QueryerContext, ConnBeginTx, all `RowsColumnType*`; no SessionResetter, Validator, NamedValueChecker | ignored (`sqlite3_sql.go` 40-42) | only `Ping` on a closed conn |
| modernc.org/sqlite | driver only; extras through `Conn.Raw` and small interfaces | `init()` registers `sqlite`; `NewConnector(dsn)` for `sql.OpenDB` (`connector.go`), whose doc argues against `sql.Register` | Connector, SessionResetter, Validator, Pinger, context interfaces | isolation ignored; read-only not enforced (`tx.go` 23-36) | only `ResetSession` on an unusable conn |
| ncruces/go-sqlite3 | own API first (`sqlite3.Conn`, `Txn` with `End(&err)`); the `driver` package wraps it and exposes `Raw() *sqlite3.Conn` | `init()` registers `sqlite3`; DriverContext with `OpenConnector` | NamedValueChecker; no Pinger, SessionResetter, Validator | serializable is `BEGIN IMMEDIATE`, linearizable `BEGIN EXCLUSIVE`, others refused; read-only enforced with `PRAGMA query_only` (`driver/driver.go` 346-380) | never |
| zombiezen.com/go/sqlite | own API only, "deliberately does not provide a `database/sql` driver" (README) | none | none | deferred `func(*error)` end functions | n/a |
| chaisql/chai | driver only since v0.17.0, which dropped `chai.Open` (release notes) | `init()` registers `chai`; only `OpenConnector` works | ExecerContext, QueryerContext, SessionResetter; `LastInsertId` and `RowsAffected` return "not supported" | any non-zero isolation refused; read-only honoured | only `ResetSession` |
| go.etcd.io/bbolt | own API: `Update`/`View` closures, or `Begin(writable)` with `Commit` | none | none | one writer at a time | n/a |
| cockroachdb/pebble | own API: batches with `Commit(opts)`, no transactions | none | none | n/a | n/a; a failure in the commit pipeline calls `Logger.Fatalf` (`db.go` 837-841) |

Also in the survey: go-mysql-server ships a `database/sql` driver built by `driver.New(provider, options)` with `OpenConnector`, and does not call `sql.Register`. Not verified: whether `NewConnector` is in modernc's very latest tag (pkg.go.dev lists it for v1.62.0, 2026-11-07).

### 5.4 How the spec maps onto `database/sql`

| s4ql | `database/sql` | Fit |
|---|---|---|
| `Open` does not wait for the lease | `sql.OpenDB` does not connect | good |
| `Wait` until serving | `DB.PingContext`, through `Pinger` | good, if `Ping` waits rather than fails; but `Ping` retries on `ErrBadConn`, so `Ping` must not return it for `ErrNoLease` |
| temporary `ErrNoLease` | an ordinary error from `Exec`, `Query`, `Begin` | good, as long as it never wraps `ErrBadConn` |
| `Commit(ctx)` and "outcome unknown" | `Tx.Commit()` without a context; the driver keeps the `BeginTx` context | workable, but the caller cannot give a separate deadline for the commit alone |
| outcome unknown of a single statement | `DB.ExecContext` with the statement's context | workable; must never wrap `ErrBadConn` |
| the stop, `Done` and `Err` | no equivalent | needs an own function or type |
| `Close(ctx)` with a deadline for releasing the lease | `DB.Close()` without a context, through `io.Closer` | needs an own function, or a fixed timeout |
| one writer at a time; a second writer waits | `DB.BeginTx` waits inside the driver's `BeginTx` | good; the pool's `SetMaxOpenConns` then also limits waiting writers |
| read transactions | `BeginTx` with `ReadOnly: true` | good, and a write in it can be refused |
| isolation levels | `TxOptions.Isolation` | s4ql's isolation is one level: refuse every level but the default and `LevelSerializable`, as ncruces and chai do (NV whether serializable is the right name for s4ql's level; it is a design question) |
| each statement its own transaction | `DB.ExecContext`, `DB.QueryContext` outside a `Tx` | good; `begin` is rejected in slice 1 anyway |

### 5.5 The options

- **A `database/sql` driver only.** Every Go tool works: sqlc, migration tools, and the bank and ring workloads of Hegel ([testing-strategies.md](testing-strategies.md) §1). The cost in trust: `database/sql` adds a pool, retries, a rollback goroutine, and context rules that s4ql does not own, and the stop, `Close(ctx)`, and a commit deadline cannot be expressed. Each of these must be pinned by tests against `database/sql` itself.
- **An own API only.** s4ql owns every rule, the API can carry `Wait`, `Done`, `Err`, `Close(ctx)`, and `Commit(ctx)` directly, and tests drive exactly what users call. The cost: no ecosystem, which conflicts with what Markus agreed.
- **Both: an own API, with the driver as a thin adapter over it.** This is ncruces' shape, and go-mysql-server's driver is built the same way over its engine. The engine and its oracle tests drive the own API; the adapter is small, and its added behaviours (no `ErrBadConn` for an unknown outcome, the kept `BeginTx` context, the refusal of isolation levels) get their own tests. Things the driver cannot express are reached through the own API, or through `sql.Conn.Raw`. The cost: two surfaces to keep consistent.

The evidence favours both, with the driver as the main public interface, as agreed, and the own API under it, because the facts of 5.2 and 5.4 show several rules of the spec that only an own API can carry, and the retry hazard is easier to test in a thin adapter. Whether the own API is public or internal is a design question for Markus.

## 6. How the engine is layered

### 6.1 From text to rows

A pipeline that fits the facts above:

1. **UTF-8 check** of the whole statement (roadmap, "Restrictions").
2. **Tokenizer** (§1), with SQLite's token boundaries.
3. **Parser** into a syntax tree that keeps each name's raw token text, its dequoted value, and its byte offset, because the messages of §3.1 use all three.
4. **"Supported?" pass** (§2.5), which rejects with s4ql's "not supported" error.
5. **Name resolution** against the catalogue: does the table exist, does the column exist, is the name reserved.
6. **Planning:** for slice 1, trivial: a scan in key order, a point lookup and an upsert, a create, a drop.
7. **Execution** over the core: `DB.Read` and a `ReadTx` for `select`; `DB.Begin`, the change, `Tx.Annotate` with the statement's text, and `Tx.Commit` for the others, since each statement is its own transaction.
8. **Errors** formatted as SQLite formats them, from the syntax tree and the catalogue.

### 6.2 What the core already gives the engine (V, `internal/core`)

- **The catalogue is SQLite's own schema in the cache.** The core keeps no table of tables (`internal/core/README.md`, "The cache"). `Tx.Table`, `Tx.Tables`, `ReadTx.Table`, and `ReadTx.Tables` return an `s4qlpb.CreateTable`: the name as created, the columns with their names and types (only `TYPE_INT` so far), and the primary key as positions (`proto/s4ql/v1/commit.proto` 74-101). Names compare as SQLite compares them, ASCII case only (`fold`, `internal/core/cache.go`).
- **The core's errors match the cases of §3.1 but not their messages.** `ErrTableExists`, `ErrNoTable`, and `ErrReservedName` are the signals, and the engine must write SQLite's message itself, since only the syntax tree has the raw token text (`table "t1" already exists`).
- **`Tx.Upsert` inserts or replaces.** SQLite's `insert` fails on an existing key, so the engine must `Get` the key first and raise `UNIQUE constraint failed: <table>.<column>` itself, with the stored names. SQLite raises it at step, not at prepare, which matters for when a `database/sql` caller sees it (from `Exec`, or from `Rows.Next` and `Rows.Err` for a query).
- **`ReadTx.Scan` returns rows in primary-key order**, which is the order of `select *` that the roadmap asks for.
- **`ErrReservedName` covers `s4ql_` and `sqlite_`.** SQLite itself accepts `s4ql_` and refuses `sqlite_` with `object name reserved for internal use` (§3.2). The engine decides which message each gets (§3.4).
- **The order of checks.** SQLite checks that a table exists while it parses (`sqlite3StartTable`, §2.1), before it reads the column list. The engine's order of "supported?", "exists?", and "valid?" decides which error wins for inputs with several faults; matching SQLite's order is only needed for the inputs that s4ql promises to match.

### 6.3 Execution: a bytecode machine or a tree of iterators

Facts:

- **SQLite compiles to bytecode for its VDBE**, and the bytecode is not an interface: "The VDBE opcodes are not part of the interface definition for SQLite", "The number of opcodes and their names and meanings change from one release of SQLite to the next", and "Applications that use SQLite should not depend on any of the details found in this document" (https://sqlite.org/opcode.html). The layers are on https://sqlite.org/arch.html.
- **Turso mirrors it.** Its `core/translate` generates bytecode for `core/vdbe` (`insn.rs`), and its debugging guide says to compare `EXPLAIN` output with SQLite's: "If the bytecode is different, that's the bug -- work towards fixing code generation" (`docs/agent-guides/debugging.md`, `CONTRIBUTING.md`). Not verified: whether a test compares `EXPLAIN` automatically. chirst/cdb, a learning project "heavily inspired by SQLite", also compiles to SQLite-named opcodes (`vm/vm.go`).
- **The Go engines surveyed use trees of iterators.** go-mysql-server turns plan nodes into `RowIter`s with `Next(*Context) (Row, error)` (`sql/rowexec/builder.go` 48, `sql/rows.go` 101-106). chai links stream operators that each yield an `Iterator` (`internal/stream/operator.go` 24-81). TiDB has an Open-Next-Close executor whose `Next` returns a batch of rows in a `chunk.Chunk` (`pkg/executor/internal/exec/executor.go`). ramsql builds `Node`s that return whole results (`engine/agnostic/predicate.go` 95-99).

Trade-offs:

- **A bytecode machine like the VDBE.** Some of SQLite's observable behaviour comes from its code generator: when an error is raised (prepare or step), the order in which expressions and constraints are checked, where affinity is applied, and the order of rows without `ORDER BY`, which follows the plan. Copying the generator's structure makes these easier to match, and with a pinned version `EXPLAIN` gives a second oracle, at the level of the plan, as Turso uses it. The costs in trust: bytecode is a large surface that is checked mainly end to end; a Go test of one layer alone needs a hand-made program; and the oracle at that level is unstable across versions by SQLite's own words.
- **A tree of iterators.** Each operator can be tested alone with Go tests, and plans are data that tests can inspect. The costs: SQLite's order of checks and of rows must be reproduced on purpose where it is observable, and there is no plan-level oracle.
- **For slice 1** the two hardly differ: one scan in key order, one lookup and write, one create, one drop. The choice binds later slices more than this one, and the roadmap's rule of small slices checked against the oracle applies to either.

### 6.4 Where the oracle plugs in

- **The token level:** the Go tokenizer against `sqlite3GetToken` (§1.3).
- **The statement level:** the same statements against s4ql and against SQLite through the binding, comparing rows, their order, column names, and errors (§4).
- **The state level:** the cache is a SQLite database with the same tables as the oracle's. After each statement, the rows of each table in the cache can be compared with the oracle's, through the binding, and with the state after a replay from S3 alone, which the roadmap asks for ("Surviving a restart").
- **The parse level:** whether SQLite accepts a statement at all (prepare succeeds, or fails with a syntax error) against whether s4ql's parser accepts it. This needs a full parser, or the combined form of §2.5.

## Not verified

- meyer's claims of coverage and exactness; its tests were not run.
- Whether CI runs musql's harness, and whether Turso compares `EXPLAIN` output automatically.
- That Lemon's default reductions never move the token of a syntax error (§2.2); it follows from LR theory and should be confirmed by the differential tests.
- Whether SQLite has messages of the form `X is not supported` that could be confused with s4ql's (§3.4).
- The authorizer as a way to see dequoted names (§1.3).
- Whether `NewConnector` is in modernc's very latest tag.

## Sources

- SQLite 3.54.1: `internal/sqlite/c/sqlite3.c`, `sqlite3.h`; `src/parse.y`, `src/tokenize.c`, `tool/lemon.c` at https://github.com/sqlite/sqlite/tree/version-3.54.1 and `src/parse.y`, `src/tokenize.c` at `version-3.53.4`; https://sqlite.org/changes.html, https://sqlite.org/opcode.html, https://sqlite.org/arch.html, https://sqlite.org/rescode.html, https://sqlite.org/lang_keywords.html, Lemon's `doc/lemon.html`.
- Go 1.27.3: `go doc database/sql/driver`; `$GOROOT/src/database/sql/sql.go` and `ctxutil.go`.
- Parsers: https://github.com/sqlc-dev/meyer, https://github.com/rqlite/sql, https://github.com/benbjohnson/sql, https://gitlab.com/cznic/sqlite, https://github.com/sqlc-dev/sqlc (pull request #4535), https://github.com/antlr/grammars-v4/tree/master/sql/sqlite, https://github.com/stephenafamo/sqlparser, https://github.com/joaobnv/mel, https://github.com/ncruces/go-sqlite3, https://github.com/gopikchr/golemon, https://github.com/kyleconroy/golite, https://pkg.go.dev/golang.org/x/tools/cmd/goyacc, https://github.com/vitessio/vitess, https://github.com/dolthub/vitess, https://github.com/pingcap/tidb/tree/master/pkg/parser.
- Testing: https://github.com/samyfodil/musql, https://github.com/tursodatabase/turso, https://github.com/tursodatabase/libsql, https://github.com/rqlite/rqlite, https://github.com/dolthub/go-mysql-server, https://github.com/dolthub/sqllogictest, https://github.com/duckdb/duckdb, https://github.com/cockroachdb/cockroach, https://github.com/gluesql/gluesql, https://github.com/pingcap/go-randgen.
- APIs and engines: https://github.com/mattn/go-sqlite3, https://pkg.go.dev/modernc.org/sqlite, https://github.com/ncruces/go-sqlite3, https://github.com/zombiezen/go-sqlite, https://github.com/etcd-io/bbolt, https://github.com/cockroachdb/pebble, https://github.com/chaisql/chai (release v0.17.0), https://github.com/proullon/ramsql, https://github.com/chirst/cdb.
