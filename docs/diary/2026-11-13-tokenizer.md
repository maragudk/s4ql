# Diary: SQL tokenizer

The first piece of s4ql's own SQL front end: a tokenizer in Go that splits SQL text into exactly the tokens that SQLite's private tokenizer `sqlite3GetToken` gives, in the pinned 3.54.1 built with `/internal/sqlite/options.h`, for any bytes. It is checked token by token against that function, by hand-written cases, every keyword, and a fuzzer. The parser, the engine, and the public API are not part of this task.

## Step 1: Requirements and scope

**Author:** tokenizer-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "A tokenizer in Go that splits SQL text into exactly the tokens that SQLite's own tokenizer, `sqlite3GetToken` of the pinned 3.54.1 built with the project's compile options, gives for the same bytes: the same boundaries and the same kinds, for every input, legal or not."
**Interpretation:** Port `sqlite3GetToken` and its keyword lookup to Go, in an internal package of its own, and prove it equal to the C function with an in-process oracle.
**Inferred intent:** The SQL front end starts on a base that can be trusted completely, so that error positions and token texts can later match SQLite's for any input.

### What I did

I read `/AGENTS.md`, `/docs/roadmap.md`, `/docs/research/sql-front-end.md` sections 1 and 2.1, `/internal/sqlite/README.md`, `/docs/diary/2026-11-10-sqlite-binding.md`, and `tokenize.c` in `/internal/sqlite/c/sqlite3.c` (lines 186493-187880). The researcher's throwaway probe `tok.c` was still in the scratch directory and showed how to reach `sqlite3GetToken`.

The requirements from the brief:

- An internal package that gives tokens with kind, byte offset, and length, so that the raw text of every token is recoverable.
- All of SQLite's tokens: every keyword of the pinned build, identifiers in all four quoting styles, strings, blobs, every numeric form with hex and digit separators, variables, operators, whitespace, both comment forms, and illegal tokens. s4ql's restrictions are not applied here.
- SQLite's behaviour on any bytes, including invalid UTF-8, NUL bytes, and input that ends inside a token. No panic, always ends.
- The scope is `sqlite3GetToken` only. What `sqlite3RunParser` does on top is left to the parser and written down in the package documentation with the place in SQLite's source. No error messages or error types for SQL errors.
- No new dependencies.
- Verification: an exact oracle through SQLite's own `sqlite3GetToken`, without a second copy of SQLite in production builds; a checked mapping from `TK_` codes to Go kinds; hand-written cases for every quirk; a fuzz test that fits `make fuzz`; every keyword in three cases and near-keywords; mutation checks by hand; `make lint`, `make test`, `make test-asan`.
- A package comment, corrections to the research note if any, and `/docs/todo.md` entries if something is left open. `README.md`, `docs/spec.md`, and `docs/roadmap.md` stay unchanged.

### Why

The research note favours a tokenizer for all of SQLite's tokens, checked against the private tokenizer, because only that gives SQLite's token boundaries for inputs that s4ql will reject.

### What worked

The probe confirmed that a C file that includes the amalgamation can call `sqlite3GetToken`.

### What didn't work

Nothing yet. No code has been written.

### What I learned

`sqlite3GetToken` never reads past the NUL that ends its input: every lookahead of two or three bytes is guarded by a test of the byte before it. So the C function can be fed a NUL-terminated copy of any Go bytes.

### What was tricky

Nothing yet.

### What warrants review

The interpretation of "the same kinds" at a NUL byte, decided in the next step.

### Future work

None yet.

## Step 2: The oracle in the binding, and the tokenizer

**Author:** tokenizer-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Tests call SQLite's own `sqlite3GetToken` and compare token by token: kind and length. That function is private to the amalgamation, so it needs a way in. Constraint: a production build of s4ql must not carry a second copy of SQLite. A test-only package with its own compiled copy, or a thin wrapper in the binding's translation unit, are both acceptable; choose after reading how the binding is built, and say why."
**Interpretation:** Choose the way in, build it test-first, then port `sqlite3GetToken` to Go and compare.
**Inferred intent:** An oracle that is exactly the code SQLite's parser runs, at no cost in trust to the production build.

### What I did

The oracle is a thin wrapper in the binding's translation unit. `/internal/sqlite/sqlite.go` got a static C function `s4ql_tokenize` in its preamble, which calls `sqlite3GetToken` over a NUL-terminated copy of the text and writes each token's type and length into two arrays, and the Go functions `sqlite.Tokenize` (returning `[]sqlite.Token{Type, Len}`, ending with the token of length 0 at the NUL) and `sqlite.Keywords` (from the public `sqlite3_keyword_count` and `sqlite3_keyword_name`). The tests `TestTokenize` and `TestKeywords` in `/internal/sqlite/sqlite_test.go` came first and failed to build ("undefined: sqlite.Tokenize"), then passed.

The reasons for the wrapper over a test-only package with its own compile:

- A separate package would `#include` the amalgamation from another directory, and the go command does not hash files outside a package's directory. That is the stale-build problem that the binding's review found (`/docs/diary/2026-11-10-sqlite-binding.md`, Step 5) and solved with `source.h`. The wrapper gets that tracking for free.
- No second compile of the 9 MB amalgamation in every test build and every `-asan` build, and no second set of local symbols to guard.
- The oracle is the same object, with the same options, as the code the cache runs, so the two cannot drift.
- The cost is two exported functions in a production package that production does not call. Go's linker drops unreachable Go functions; the C wrapper is a few instructions, and `sqlite3GetToken` is in the binary anyway because the parser uses it.

The TK_ codes are numbered by Lemon per version, so the tests never hard-code them. `readTokenCodes` in `/internal/sql/token/token_test.go` (and `tokenCodes` in the binding's test) parse the `#define TK_` lines of the vendored `sqlite3.c`, which has them twice, from `parse.h` and in `parse.c`, and fail if the two disagree. Each Go kind is named after its TK_ code (`Kind.String`), and the mapping is built by name. `TestKind` checks that every kind has a distinct code, and that every TK_ code that `sqlite3GetToken` can assign has a kind and no kind is left over: it reads the `*tokenType = TK_…` assignments in the function's body and the table `aKWCode`.

The package is `/internal/sql/token`, so that the parser and later packages of the front end can sit next to it under `/internal/sql/`. Its API:

- `Kind`, one per TK_ code the tokenizer gives (33 non-keyword kinds and 136 keyword kinds for the 147 keywords, since some keywords share a code: `LikeKw`, `JoinKw`, `CTimeKw`, `Temp`), with `String` and `IsKeyword`.
- `Next(z string) (Kind, int)`, the port of `sqlite3GetToken`.
- `Tokens(src string) []Token`, with `Token{Kind, Offset, Len}` and `Token.Text(src)`.

`Next` follows `tokenize.c` case by case, with SQLite's `aiClass` and `sqlite3CtypeMap` tables copied verbatim, and a byte reader `at(i)` that gives 0 at or past the end, which stands in for the NUL that ends SQLite's string. The two C loops written in one line with side effects in their condition (the block comment and the bracketed name) are quoted in a comment above their Go form. Keywords are a map from the upper-case word to the kind, looked up through a stack buffer of 17 bytes; an internal test checks the map against `sqlite.Keywords`.

At the end of the text, or at a NUL byte, `Next` gives `Illegal` and length 0, as `sqlite3GetToken` does, and `Tokens` stops there without a token for the end. I chose this over a separate end kind so that the mapping of kinds stays one to one with SQLite's and `Next` can be read against the C code without a special case. The parser can tell a NUL from the end by comparing where the tokens stop with `len(src)`.

The tests in `/internal/sql/token/token_test.go`:

- 175 hand-written cases, each with the tokens written out by hand and each also compared with the oracle. They cover every quirk of section 1.1 of the research note and the ones below.
- Every input of one and two bytes, and every input of three and four bytes over an alphabet of 40 bytes chosen to hit every character class and the BOM, compared with the oracle.
- Every keyword of `sqlite.Keywords` in lower, upper, and mixed case, and eight near-keywords per keyword (one letter less at either end, `S`, `_`, `1`, or a byte from 0x80 appended, `A` prepended, `_` inserted), each in two cases.
- `FuzzTokens`, seeded with the cases and 15 statements that use most kinds of tokens. `/Makefile`'s `fuzz` target now includes `internal/sql/token`, so `make fuzz` and `make test-asan` run it.

### Why

The research note favours a tokenizer for all of SQLite's tokens checked against the private tokenizer. The exhaustive short inputs complement the fuzzer: most of the tokenizer's decisions are made within three bytes of the start of a token.

### What worked

The port passed the oracle on the first run, apart from one expectation I had written wrong by hand (below). The exhaustive tests of short inputs run in about three seconds.

### What didn't work

My hand-written expectation for `1_e5` was wrong: "Expected "ILLEGAL "1_e5"", but got "QNUMBER "1_e5""". The digit loop takes the `_`, stops at `e`, and the exponent rule then reads `e5`, so the token is one `QNUMBER`; the parser's `sqlite3DequoteNumber` rejects it later. The oracle agreed with the Go code, and I fixed the expectation.

The first fuzz run, started in the background, failed at once: "failure while testing seed corpus entry: FuzzTokens/seed#129 ... panic: runtime error: slice bounds out of range [3:2]". It had compiled after I had started the mutation checks of the next step, and so fuzzed a deliberately broken tokenizer. I restored the code and ran it again.

The worktree's sandbox refused a compound shell command that ran the mutations in a loop ("this command is too complex to verify that it stays inside the worktree"), so I made each mutation with the editor.

### What I learned

Findings about `sqlite3GetToken` that the research note did not have, all checked against the oracle and three of them end to end with the binding:

- `/*` as the last two bytes of the input is not a comment, because of the test `z[2]==0` (187301): it is `/` and then `*`. `select 1 /*` fails with `near "*": syntax error`, while `select 1 /* ` returns 1.
- The Tcl suffixes `::` and `(…)` are allowed after all four named variable forms, `:`, `@`, `#`, and `$`, not only after `$`. `select :a(b)`, `select #a(b)`, and `select @a::b` each return a row.
- A malformed blob is illegal up to and including the next quote: `x'0g'` is one illegal token.
- A NUL byte inside a token ends the token: `'a<NUL>b'` gives the illegal token `'a`.
- A BOM after other whitespace is a whitespace token of its own; the whitespace loop does not continue over it.
- A word of letters is looked up as a keyword only if the byte after it is no identifier character, so `select$`, `select1`, and `select<0x80>` are names, while `select_` is a word of keyword characters that is no keyword.
- `1_e5` is one `QNUMBER`, and `1..2` is two floats, `1.` and `.2`.

I corrected section 1.1 of `/docs/research/sql-front-end.md` for the first four, and added to section 1.3 how the oracle was built.

### What was tricky

Choosing what `Next` gives at the end. SQLite gives `TK_ILLEGAL` with length 0, which is also what it gives at an embedded NUL. A separate end kind would read better in a parser, but it would break the one-to-one mapping of kinds, which is what the tests check.

### What warrants review

- `Tokenize` and `Keywords` in `/internal/sqlite/sqlite.go`: two exported functions in the production binding for the tests' sake.
- `Next` in `/internal/sql/token/token.go`, side by side with `sqlite3.c` 187252-187574.
- The parsing of the `TK_` definitions and of the assignments in `sqlite3GetToken` in `TestKind`: it is how the mapping is checked rather than assumed.

### Future work

The parser takes over what the package documentation lists as left out.
