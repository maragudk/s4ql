# Diary: The pass after the parser, for slice 1

The pass between `/internal/sql/parser` and the engine: it takes SQL text and gives SQLite's syntax error, "not supported", no statement, or a statement of slice 1 in a typed form that the engine can run. It is the gate that keeps everything that `/docs/roadmap.md` does not list out of the engine.

## Step 1: Requirements and scope

**Author:** sql-check-slice-1-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "You are the builder for the feature "The pass after the parser, for slice 1" in s4ql [...] Your task is the piece between them: the pass that takes SQL text and gives one of three answers: SQLite's syntax error; "not supported"; or a statement that s4ql supports, in a typed form that the engine can run. It is the gate that keeps everything unlisted out, so its job is to be impossible to get past by accident." The brief has three parts: "1. The three answers" (the exact syntax error with codes 1 and 1, the "not supported" error with `ErrNotSupported`, the prefix `s4ql: not supported: `, a deterministic rule for what and where, and no result code; the typed form built "only from what it has positively recognized"; the no-statement outcome), "2. What is supported: slice 1, exactly" (an allow-list over tokens checked generically through `ast.Tokens`, `ast.Children`, and `ast.AllTokens`; names, integer values, the column type, the table options, one statement per text, and every open form decided by "When in doubt, it is not supported" and written down in a README), and "3. Tests: SQLite is the judge" (eight layers: meaning checked against SQLite for every accepted text, agreement on syntax errors over the corpus, its mutants, and the grammar generator, a generator of slice 1, mutants of it, a reflection test of the allow-list, hand-written tests, fuzzing to at least 100 million executions, and mutation testing with `/internal/sql/mutation`).
**Interpretation:** Build a new package under `/internal/sql/` with one function from text to one of four outcomes, a recognizer over the syntax tree that takes the tokens of the supported form in text order so that any token it does not know stops it, a README that defines slice 1 token for token, and the eight test layers, with SQLite deciding every question of meaning.
**Inferred intent:** The engine can be built on a small typed form and trust that nothing outside slice 1 ever reaches it, and every answer the pass gives can be traced to a check against SQLite.

### What I did

I read `/AGENTS.md`, `/docs/roadmap.md`, the section "The SQL front end" of `/docs/todo.md`, §2.5 and §3 of `/docs/research/sql-front-end.md`, the READMEs of the parser, oracle, harness, shape check, and mutation testing, the package docs of `/internal/sql/ast`, and `/docs/diary/2026-11-16-sql-parser.md`. I checked out the branch `sql-check-slice-1` from `origin/main` at `b5ad525`.

### Why

The brief names many decisions that the pass must make where the roadmap leaves the form open, and each must be settled by a test against SQLite before code depends on it.

### What worked

Nothing to report yet beyond the reading.

### What didn't work

Nothing yet. No code had been written.

### What I learned

The grammar generator that the brief asks for in the second test layer lives in the test files of `/internal/sql/parser` (`grammar_test.go`), so no other package can import it as it is.

### What was tricky

The worktree isolation refuses shell commands with here-documents, so scratch files are written with the editor tool.

### What warrants review

Nothing yet.

### Future work

None yet.
