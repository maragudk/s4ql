# Diary: SQL parser

s4ql's own parser for all of SQLite 3.54.1's grammar, into a syntax tree that keeps everything as written, tested against SQLite through the oracle, corpus, and harness of `/internal/sql/oracle`, `/internal/sql/corpus`, and `/internal/sql/parsetest`.

## Step 1: Requirements and scope

**Author:** sql-parser-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "You are the builder for the feature "SQL parser" in s4ql [...] Your task is the parser itself and its syntax tree." The brief lists three parts: "1. The syntax tree" ("Everything as written", "Every node records its span [...] and every token it consumed directly", optional clauses explicit, SQLite's precedence), "2. The parser" ("The language it accepts is exactly SQLite 3.54.1's", "Syntax errors are SQLite's, exactly", action errors left to the pass after the parser with a list of them, "No input may crash it or make it run away", a benchmark), and "3. Tests" ("The corpus check to zero", "Mutants to zero", "Fuzzing against SQLite", "A generator from the grammar", "Token ownership", "Hand-written tests", a deep-nesting test).
**Interpretation:** Port `parse.y` by hand as recursive descent with precedence climbing, reproduce Lemon's behaviour (precedence, `%fallback`, `%wildcard`, token classes) and SQLite's parse loop (window keywords, the empty semicolon, the checks of `#N` and digit separators), and prove it with every test layer, to zero disagreements.
**Inferred intent:** A front end whose verdict on any text can be trusted to be SQLite's, and a tree in which a later pass can see every token that was read, so that "not supported" can be decided exactly.

### What I did

I read `/AGENTS.md`, the section "The SQL front end" of `/docs/todo.md`, `/docs/research/sql-front-end.md` §2, `/docs/research/sqlite-parse-tree.md` §7, `/internal/sql/token`, the READMEs of the oracle, corpus, and harness, the diaries of the tokenizer and of the corpus, and, from the verified archive `sqlite-src-3540100.zip` (SHA-256 `387e7f6c...`, as in `/internal/sql/corpus/source.json`), `src/parse.y`, `src/tokenize.c`, `tool/lemon.c`, and `tool/lempar.c`.

The requirements, condensed: a tree package and a parser package; the tree as written with spans and owned tokens, checkable generically; the exact language and exact errors of SQLite; action errors accepted and listed; a depth limit that never rejects what SQLite parses; nothing quadratic; corpus, mutants, fuzzing, a grammar generator with rule coverage, ownership checks with a proof on a broken tree, hand-written shape tests; run times plain, `-race`, and `make test-asan`; READMEs; `/docs/todo.md`; this diary. Not in scope: the pass that rejects, the round-trip printer, name resolution, any public API, `/README.md`, `/docs/spec.md`, `/docs/roadmap.md`. No other project's parser may be read or used.

### Why

The brief is long and the order of work it asks for (one statement end to end, then widen, then mutants, generator, fuzzing) depends on having the requirements in one place.

### What worked

Nothing to report yet beyond the reading.

### What didn't work

Nothing yet. No code had been written.

### What I learned

The amalgamation in `/internal/sqlite/c/sqlite3.c` holds the generated parser. Building `tool/lemon.c` and running it on `parse.y` with `-S` gives tables byte-identical to the amalgamation's (`yy_action` and `yyFallback` have the same MD5), so the vendored SQLite was built from the default grammar: no `SQLITE_OMIT_*` that changes the grammar, no `SQLITE_ENABLE_UPDATE_DELETE_LIMIT` or `SQLITE_UDL_CAPABLE_PARSER`, no ordered-set aggregates.

### What was tricky

The worktree isolation refuses shell here-documents that write files, so scratch scripts are written with the editor tool.

### What warrants review

That the requirements above match the brief.

### Future work

None yet.
