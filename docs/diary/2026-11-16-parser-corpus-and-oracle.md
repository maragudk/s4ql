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
