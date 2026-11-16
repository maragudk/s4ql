# Diary: Parser corpus and oracle harness

What s4ql's own parser for all of SQLite's grammar will be tested against, built before the parser: `sqlite3_error_offset` in the binding, an oracle that gives SQLite 3.54.1's verdict on the first statement of any text, a corpus of SQL statements extracted from SQLite's own public-domain distribution, and a harness that compares a parser under test with the oracle. The parser itself is not part of this task.

## Step 1: Requirements and scope

**Author:** parser-corpus-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "You are the builder for the feature "Parser corpus and oracle harness" in s4ql [...] Before the parser is written, we build what it is tested against. That is your task. You do NOT write the parser." The brief lists five parts: "`sqlite3_error_offset` in the binding", "An oracle function that gives SQLite's verdict on the first statement of a piece of SQL text, built on `sqlite3_prepare_v2`", "A corpus tool in Go (no TCL interpreter, no new module dependency)", "A test harness for a parser under test", and "Measure the run time of the new tests plain, with `-race`, and under the sanitizer job".
**Interpretation:** Build the first of the four agreed checks of the parser (acceptance, message, and offset against `sqlite3_prepare_v2`), with everything it needs, and prove it with stand-in parsers, since no parser exists.
**Inferred intent:** The parser builder who comes next starts with a judge that is never wrong: a wrong "parses" or "syntax error" from the oracle would poison every later test, so certainty matters more than coverage.

### What I did

I read `/AGENTS.md`, the section "The SQL front end" of `/docs/todo.md`, `/docs/research/sql-front-end.md`, `/docs/research/meyer.md`, `/docs/research/sqlite-parse-tree.md`, `/internal/sql/token`, `/internal/sqlite/README.md`, `/internal/sqlite/update/main.go`, `/internal/sqlite/sqlite.go`, the `Makefile`, `.golangci.yml`, the workflows, and `/docs/diary/2026-11-13-tokenizer.md`. I downloaded `sqlite-src-3540100.zip` and `sqlite-doc-3540100.zip` from sqlite.org into the scratch directory to read `src/parse.y`, `src/tokenize.c`, `src/prepare.c`, and the test files.

The requirements from the brief:

- `sqlite3_error_offset` exposed in the binding, in the way its API already works, and tested.
- An oracle with four outcomes for the first statement of any bytes: parses (errors after parsing count as parses, and it must be shown how the two are told apart), syntax error with SQLite's exact message and offset, "SQLite does not say" when an action or another check stopped the parse early, and the tail. Anything uncertain goes to "does not say", and those cases are counted and listed. Deterministic, documented connection settings; find out where the empty schema stops SQLite early and choose what gives the most certain verdicts.
- A corpus tool in Go that extracts statements from pinned, hash-verified downloads of SQLite 3.54.1 (TCL tests, documentation, fuzz corpora), writes deduplicated, stably ordered, compact test data with a README, behind a `make` target, byte-identical when regenerated. No stored expectations. Check that the S3 credentials from the main repository's `.env` do not occur in it, without printing them.
- A harness: a small parser interface, a corpus run that groups disagreements by kind and a useful key with counts and keeps "does not say" apart, seeded one-token mutations built on `/internal/sql/token`, a fuzz test against the oracle in `make fuzz`, a hand-written corpus, property-based generation only if it earns its place, and stand-in parsers that prove the harness reports correctly.
- Run times measured plain, with `-race`, and under `make test-asan`; split the corpus run if it is too slow.
- `make lint`, `make test`, `make test-asan`, `-count=20`, `-race`; no test depends on timing; a README next to the new packages; `/docs/todo.md` updated; `README.md`, `docs/spec.md`, and `docs/roadmap.md` untouched.

### Why

The todo entry of 2026-11-16 agrees four checks for the parser, and the first needs SQLite's offset and a corpus with no stored expectations. Building the judge first means the parser is written against it from its first line.

### What worked

The meyer research note had already measured most of SQLite's error behaviour that matters here, so the reading went quickly: the tail moves past the token on which an action raised an error, the last message raised while one token is handled wins, and `#N` and digit separators are checked in actions.

### What didn't work

Nothing yet. No code has been written.

### What I learned

The repository has no property-based testing library: no Hegel, `rapid`, or `testing/quick` in any Go file or `go.mod`.

### What was tricky

The harness's worktree isolation refuses shell commands that it cannot verify stay inside the worktree, such as a Python here-document that edits a file, so all edits go through the editor tools.

### What warrants review

That the requirements above match the brief.

### Future work

None yet.

## Step 2: The binding: the error offset, the first statement of any text, and process PRAGMAs

**Author:** parser-corpus-builder

### Prompt Context

**Verbatim prompt:** "`sqlite3_error_offset` in the binding. Expose the byte offset of the error for the last failed call on a connection, in the way that fits the binding's existing API. Tested."
**Interpretation:** Errors in the binding are values captured at the moment of failure, so the offset belongs on `sqlite.Error` next to the message. The oracle also needs to prepare text that `Conn.Prepare` refuses.
**Inferred intent:** Give the oracle everything it needs from SQLite, through the one file that may call SQLite.

### What I did

`/internal/sqlite/sqlite.go` gained `Error.Offset` (filled by `sqlite3_error_offset` wherever the message comes from the connection, -1 otherwise), `Conn.PrepareFirst` (prepares the first statement of any bytes and returns the tail also on an error, or -1 when SQLite did not set it, as on a closed connection), `Conn.DenyProcessPragmas` (a static C authorizer that denies `soft_heap_limit`, `hard_heap_limit`, `temp_store_directory`, and `data_store_directory`), and later `sqlite.Complete` (`sqlite3_complete`, for the corpus tool). Tests in `/internal/sqlite/sqlite_test.go`; the README lists the additions.

### Why

The verdict needs the offset, the tail on error, and NUL bytes passed through. The PRAGMAs above change settings for the whole process when they are prepared (`src/pragma.c` acts in `sqlite3Pragma` at code generation), and the corpus has them, so one verdict could change the next, or another test's process. The authorizer is checked at the top of `sqlite3Pragma` (pragma.c 471), after the whole statement has been parsed, so a denied PRAGMA still gets a full syntax check.

### What worked

The authorizer is a static C function in the binding's translation unit, so no `//export` and no second copy of the preamble.

### What didn't work

My first test expected `select * from nosuch` to carry an offset; it fails with `Expected "14", but got "-1"`. `no such table` is formatted without `%T`, so SQLite records no offset. `no such column: nosuch` does carry one (10 in `select 1, nosuch`), through the expression's offset.

### What I learned

`sqlite3_prepare_v3` does not set the tail when it refuses the text before parsing (SQLITE_TOOBIG for the length, or misuse), and `s4ql_prepare` turned that into "the end of the text". The oracle must not read that as "the parse reached the end", so `s4ql_prepare_first` reports -1.

### What was tricky

`sqlite3ErrorMsg` sets `errByteOffset` to -2 before formatting and to -1 after if no `%T` recorded one, and a suppressed error (`db->suppressErr`) still does this, so an offset can be lost after it was recorded. That is why `select #1` has offset -1 while `select #1 from t` has 7.

### What warrants review

That `Error.Offset` is -1 on every path that does not read it from the connection: `errorFromConn` with a nil connection or a different code, and the length checks in `Prepare` and `PrepareFirst`. `internal/core` constructs two `sqlite.Error` values for injected I/O errors without `Offset`; they get 0, which nothing reads.

### Future work

None.

## Step 3: The oracle and how it decides

**Author:** parser-corpus-builder

### Prompt Context

**Verbatim prompt:** "An oracle function that gives SQLite's verdict on the first statement of a piece of SQL text, built on `sqlite3_prepare_v2`. [...] classify with care: a wrong "parses" or wrong "syntax error" here would poison every later test of the parser. If a case cannot be classified with certainty, it goes to "does not say", and you count and list such cases".
**Interpretation:** Decide, from what SQLite reports (code, message, offset, tail), whether the parser saw the whole statement, and prove the rule.
**Inferred intent:** A judge whose every "parses" and "syntax error" is a fact about SQLite's grammar.

### What I did

`/internal/sql/oracle/oracle.go`: `Judge(src) Verdict` with the kinds `Parses`, `SyntaxError`, `NotSaid` and a `Reason` for the last. I read `sqlite3RunParser` (sqlite3.c 187579-187742), `sqlite3Prepare` (149133-149286), `sqlite3ErrorMsg` and `sqlite3RecordErrorByteOffset`, the `%syntax_error` and `%stack_overflow` of `parse.y`, the trigger and virtual-table rules, and `addModuleArgument` in `vtab.c`. The rules:

- Prepared: parses.
- A message of the syntax family (`near "X": syntax error`, `incomplete input`, `unrecognized token: "X"`) with SQLITE_ERROR: syntax error. Only the syntax-error routine, the parse loop's unknown token, and two token-checking actions (`#N`, digit separators) produce them.
- Any other message: the loop stops after the token on which the error was raised and sets the tail after it, so the token that ends at the tail tells whether the statement's end (a `;`, or the empty semicolon at the end of the text) had arrived. When the tail and the last token both end at the end of the text, a second prepare with a space appended tells them apart. Inside CREATE TRIGGER a `;` ends the statement only after `; END`; inside CREATE VIRTUAL TABLE only with no parenthesis open. A `#N` or misplaced digit separator in the statement makes it `Overwritten`, since a later action message can replace theirs. Only SQLITE_ERROR and SQLITE_AUTH count.
- `NotSaid` with a message about a missing table, view, or schema: create it (a table, a view for INSTEAD OF, `attach ':memory:'`) and judge again, a few objects at most, recorded in `Verdict.Setup`.

Each prepare opens a new `:memory:` connection with `DenyProcessPragmas`.

### Why

The meyer research note classified "stopped" statements with a heuristic (remove the last `;`, add a space). Reading the parse loop gave an exact rule instead, and the cases where it is not exact (the two statements with inner semicolons, the overwritten lexical errors) have reasons of their own.

### What worked

Two properties check the classification on every corpus entry (`TestJudge_corpus`) and in `FuzzJudge`: every `Parses` statement with `)` in place of its end gives `near ")": syntax error` exactly at that `)`, and the text before a syntax error's token has no earlier syntax error. Both hold on all 139,234 entries, and in 17 million fuzz executions. A misclassified "parses" would fail the first, since a parse stopped early would stop at the same place again.

A semicolon inside virtual-table arguments is reachable: with 2,100 arguments that each start with `;`, `too many columns on v` is raised on the arrival of a `;` with a parenthesis open. The test is in `oracle_test.go`.

### What didn't work

- `select ----1` in my test for the expression depth is a comment; `~` works.
- My first test for an overwritten digit separator, `select 1_ limit 0 union select 1`, raises the separator error at `limit`, which is a syntax error, not overwritten; `select 0 limit 0 union select 1_` is the overwritten case.
- The first fuzz run failed in 8 seconds on `/*;` (`not complete: /*`): the property removed the final `;` and so turned an unterminated comment into the two tokens `/` and `*`. The test now removes the `;` only if it is a token.
- I wrote a test for SQLITE_TOOBIG with a 1 GB string, then removed it before running it; `oracle_internal_test.go` gives `classify` such results directly.

### What I learned

With an empty schema, every CREATE TRIGGER stops at BEGIN (`no such table: main.t`, from `sqlite3BeginTrigger` in the action of `trigger_decl`), and a schema-qualified CREATE stops at the next token (`unknown database`). The fixes settle 1,378 of the 1,463 corpus statements that needed them; without them 1,687 would be `NotSaid` instead of 309.

`#N` gets offset -1 when it is raised at the end of the text and the token's offset otherwise; a misplaced digit separator always gets -1, and the message shows the number half rewritten (`12_2__3` for `1__2__3`).

### What was tricky

The synthetic semicolon at the end has the same token type as a real one, so the end of a trigger body without END looks like a statement's end; only the grammar's shape (`; END ;`) tells them apart. And "the last message wins" works both ways: a syntax error on a token replaces an action's message on the same token, which is safe, while an action's message can replace `#N`'s, which is not.

### What warrants review

`reachedEnd` and `insideStatement` in `/internal/sql/oracle/oracle.go`, against `sqlite3RunParser`. The claim that no other statement of the grammar shifts a `;` before its end (from `parse.y`: `SEMI` appears in `ecmd`, `trigger_cmd_list`, and through `%wildcard ANY` in virtual-table arguments). The claim that the grammar does not depend on the schema, which makes the fixes safe.

### Future work

The `Unsettled` path is not covered by a test; no input reaches it.

## Step 4: The corpus and its tool

**Author:** parser-corpus-builder

### Prompt Context

**Verbatim prompt:** "A corpus tool in Go (no TCL interpreter, no new module dependency) that extracts SQL statements from pinned, hash-verified downloads of SQLite 3.54.1: the TCL test files [...], the documentation, and the fuzz corpora in the source tree."
**Interpretation:** A command like `/internal/sqlite/update`, a deterministic extractor, a compact line format, and an embedded reader.
**Inferred intent:** A large corpus of real SQL with no stored answers, which anyone can regenerate and check.

### What I did

`/internal/sql/corpus/update/main.go` pins both archives in `/internal/sql/corpus/source.json` (`-version 3.54.1` read the PRODUCT lines; SHA3-256 and SHA-256 recorded), checks them, extracts TCL command arguments, `<pre>` blocks, and the `xsql` tables of `test/fuzzdata*.db`, splits with `sqlite.Complete`, deduplicates, sorts, and writes `tcl.txt`, `doc.txt`, `fuzz.txt`. `-archives DIR` reads local copies; `-check` compares. `/internal/sql/corpus/corpus.go` embeds the files and has `Encode`, `Decode`, `Parse`, `Read`, `All`. `/internal/sql/corpus/own.txt` holds 219 hand-written statements. `make corpus` runs the tool. `.gitattributes` marks the three generated files as generated without diffs.

### Why

One statement per line with backslash escapes keeps the files greppable and the diffs of `own.txt` readable; JSON could not hold invalid UTF-8 under `encoding/json/v2`.

### What worked

Two runs gave byte-identical files (same SHA-1 sums), and `-check` reports them up to date. The cheap check for the S3 credentials from the main repository's `.env` found none (counts only, nothing printed).

### What didn't work

The first extraction gave a 23.8 MB `fuzz.txt` (126,224 statements). Leaving out statements that are nothing but a NUL and what follows, and capping fuzz statements at 128 bytes (TCL and docs at 1,000), brought it to 6.0 MB and 83,247 statements; the whole corpus is 11.4 MB, 1.4 MB with gzip. `own.txt` first had `select\f1`, which `Decode` refuses (`unknown escape \f`); it is `\x0c` now.

### What I learned

Of 139,234 statements, 84,214 parse (58,607 of them with an error after the parse, mostly `no such table`), 54,711 are syntax errors (53,840 from the fuzz corpora), and on 309 SQLite does not say. `fuzzdata8.db` stores its scripts as blobs.

### What was tricky

TCL quoted arguments get only simple backslash handling and no substitution, so `$var` stays in the SQL; that is fine, since the oracle decides what each text is.

### What warrants review

The size cap for fuzz statements, which trades coverage for repository size, and the extraction regexps in `main.go`.

### Future work

None.

## Step 5: The harness, its proof, and the run times

**Author:** parser-corpus-builder

### Prompt Context

**Verbatim prompt:** "A test harness for a parser under test [...] proof that the harness works, with stand-ins, since no parser exists" and "Measure the run time of the new tests plain, with `-race`, and under the sanitizer job (`make test-asan`)".
**Interpretation:** A small interface, a report grouped for working through, mutations, fuzzing, and stand-ins that are wrong in known ways.
**Inferred intent:** The parser builder writes a `Parse` method and gets an exact, grouped list of where it differs from SQLite.

### What I did

`/internal/sql/parsetest/parsetest.go`: `Parser`, `Result`, `Oracle`, `Compare`, `Check`, `Pattern`, `Run`, `Report`, `Test`; `/internal/sql/parsetest/mutate.go`: `Mutants`. Tests: SQLite against itself on the whole corpus and on 8,000 mutants; stand-ins that accept everything, are off by one in the offset, give an offset where SQLite has none, differ in one message, reject valid statements, and ignore the semicolon; `FuzzMutants`. `FuzzJudge` and `FuzzMutants` are in `make fuzz`. `/internal/simlint/simlint.go` exempts `parsetest`, which imports `math/rand/v2` for the mutations and never runs under the simulation.

### Why

Mutations give inputs near valid statements, where a hand-written parser is most likely to differ.

### What worked

Every stand-in was reported with the expected count in the expected group on the first run.

### What didn't work

`Run` first judged on `GOMAXPROCS` goroutines: the corpus took 9.5 s with 10, 6.6 s with 2, and 2.9 s with 1, because SQLite takes global mutexes when it opens connections and allocates memory. It judges sequentially now. `make lint` failed on the simulation lint: `parsetest/mutate.go:5:2: math/rand/v2 draws randomness outside the host; use host.Host.Uint64`.

### What I learned

Measured on an Apple M4, and in Docker (`golang:1.27.3`, Linux arm64) for the sanitizer: `go test ./internal/sql/oracle` 4.9-7 s plain, 9.7 s with `-race`, 35.5 s with `-asan` on the whole corpus; `./internal/sql/parsetest` 4.7-6.5 s, 10.2 s, and 40 s. Under the sanitizer the two corpus tests now use every tenth entry, as with `-short`, which gave 3.9 s and 13.5 s.

### What was tricky

Nothing beyond the lock contention.

### What warrants review

That `Pattern` gives keys that are useful to work through: the token kind for `near` messages, a description of the start for `unrecognized token`, and the part before the first colon for any other message.

### Future work

Property-based generation of statements was not added: the repository has no library for it, and the mutations and fuzzing cover generation for now.

## Step 6: READMEs, self-review, and the checks

**Author:** parser-corpus-builder

### Prompt Context

**Verbatim prompt:** "Docs: a README next to the new packages" and "Self-review your work before you report."
**Interpretation:** A writer drafts the three READMEs from a brief with every fact and number; the code-review skill reviews the branch; then the full checks run.
**Inferred intent:** The next builder can start from the READMEs, and the branch is ready for the lead.

### What I did

A writer sub-agent wrote `/internal/sql/oracle/README.md`, `/internal/sql/corpus/README.md`, and `/internal/sql/parsetest/README.md` on its own branch, which I read and merged. It checked every number of the brief by running the code, and corrected two: the statements settled by the setup are 1,010 CREATE TRIGGER, 189 CREATE TABLE, 167 ALTER TABLE, and 12 CREATE VIRTUAL TABLE (my brief's split did not add up), and `own.txt` has 219 statements in 250 lines. The code-review skill (level high) gave ten findings. I fixed seven:

- An attached schema written in brackets or quotes was never matched again (`create trigger [aux].tr ...` stayed `NotSaid`); its name is now dequoted. One corpus statement moved from `NotSaid` to `Parses` (308 now).
- `corpus.Encode` used `unicode.IsPrint`, whose tables change with Go, so the generated files would not stay byte-identical across Go versions; it escapes by a fixed list now (the files changed by about 1,400 bytes).
- The corpus test only checked `Decode(Encode(x)) == x`. It now checks that every line of every file is what `Encode` writes, which at once found `own.txt` writing `日` and `é` as escapes.
- `-check -version` rewrote `source.json`; the combination is refused.
- A CRLF checkout would break every corpus line; `.gitattributes` sets `eol=lf`.
- `parsetest.Pattern` took two arguments it never used.
- `Prepare` and `PrepareFirst` had two copies of the C wrapper; `Prepare` is built on `PrepareFirst` now.

Also the loop bound was by objects created, while one fix can create two; it counts fixes now (at most three).

### Why

Each fixed finding was a real fault or a real duplicate; the corpus test was the most useful, since it caught a fault on its first run.

### What worked

`make test-asan FUZZTIME=30s` passed on the branch before the review fixes, in a Docker container: `internal/sql/oracle` 8.7 s and `internal/sql/parsetest` 16.8 s within the full run, and the two new fuzz tests 30 s each. After the fixes, `-asan` on `./internal/sql/... ./internal/sqlite/...` passes (oracle 3.2 s, parsetest 9.6 s), as do `-race`, `make lint`, and the cross-check module. `-count=20` on all the new tests passed (oracle 187 s, parsetest 173 s for twenty runs, while Docker ran beside it).

### What didn't work

My new test for a trigger in `main` on `aux.t` expected offset -1 and tail 51, and failed with `Expected "-1", but got "15"`, then `Expected "51", but got "45"`: `trigger tr cannot reference objects in database aux` names the schema token and stops at ON's table.

### What I learned

Three findings I did not take, with the reason:

- Attaching the schema for `trigger X cannot reference objects in database Y` would not help: SQLite raises it for a trigger in `main` on a table of another schema whatever that schema holds, so it stays `NotSaid` (now a test).
- Using `sqlite3_complete` for the trigger rule: it takes `end` as a name, as in `select 1 as end;` inside a body, for the end of the trigger, so it would be wrong where the oracle must be exact. The comment on `insideStatement` says so.
- Fewer connections per verdict: a judgement takes about 10 microseconds, and the whole corpus about 3 s, so I kept one new connection per prepare, which keeps every verdict independent of the ones before.

### What was tricky

The writer worked from an earlier commit of the branch, so the merge brought its READMEs in beside commits it had not seen; the numbers it checked were still right, apart from the one statement the review fix moved, which I updated by hand.

### What warrants review

The merge commit of the writer's branch, and the fixed list in `visible` in `/internal/sql/corpus/corpus.go`.

### Future work

The `Unsettled` reason is still not reached by any test or corpus entry. Running `go run ./internal/sql/corpus/update -check` in CI would catch a hand edit of a generated file, but it downloads 27 MB from sqlite.org, so it is left out, as `make sqlite-verify` is left out of the main workflow.
