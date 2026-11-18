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

The worktree isolation refused a compound shell command that wrote a scratch file with a here-document (`cd ... && cat > file <<'EOF'`): "this command is too complex to verify that it stays inside the worktree". Scratch files are written with the editor tool instead.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: What SQLite does with the forms of slice 1, and the design of the pass

**Author:** sql-check-slice-1-builder

### Prompt Context

**Verbatim prompt:** (the same brief) "Where the roadmap leaves a choice of form open, decide by these rules, and write every such decision down in the package's README as the precise definition of slice 1 [...] A form is supported only if you have shown, by a test against SQLite, that SQLite treats it exactly as the plain form [...] Allow-list over tokens."
**Interpretation:** Settle each open form by probing SQLite 3.54.1 through the binding, then build a recognizer that cannot accept a token it was not written to take.
**Inferred intent:** Every decision in the README can be traced to a fact about SQLite, and the gate holds by construction, not by a list of things to reject.

### What I did

I probed SQLite through a throwaway test, `/internal/sql/stmt/scratch_test.go` (never committed), that read statements from a file in the scratchpad and printed each result with its codes. Then I wrote the package `/internal/sql/stmt`: `stmt.go` (the typed form and the errors), `check.go` (`Check`: the checks before parsing, the parse, the second statement), `recognize.go` (the recognizer), and `vocabulary.go` (the names of "not supported", and the reserved table names).

The recognizer holds `ast.AllTokens` of the statement and a cursor. `take(t)` requires that `t` is the token at the cursor and moves on, so a token in a field that the recognizer never reads is at the cursor when it takes the next token it expects, and stops it. At the end, the cursor must be past the last token: that is the generic check. A node is marked accepted when the recognizer enters it; the name of a token that stops it comes from the outermost construct around it that is not accepted, by a walk down from the root through accepted nodes.

### Why

The brief asks that a statement be supported only if every token is accounted for, checked generically at the end. Taking tokens in the order of the text gives that, and gives the "first token not part of a supported statement" for the offset of the error for free.

### What worked

The facts, all on 3.54.1 through `internal/sqlite`:

- `table %T already exists` prints the raw token: `table "t""1" already exists`. `no such table` and `table T1 has no column named V2` print the dequoted name as written in the statement (`table t"1 has no column named V"2`; `main.T1` with a schema). `UNIQUE constraint failed: Foo.Bar` prints the stored names, `a b.c"d` unquoted.
- `ÆØÅ` and `æøå` are two tables; `k` and `"K"` are one.
- `-9223372036854775808`, `- 9223372036854775807`, `-/*x*/--y\n9223372036854775804`, `00012`, `-0`, and literals with 40 leading zeros all store integers. `9223372036854775808` and `-9223372036854775809` fail with `cannot store REAL value in INT column n.v` (19/3091).
- `integer primary key` in a STRICT, WITHOUT ROWID table behaves as `int primary key` (NOT NULL implied, `'1'` stored as 1, `1.0` as 1, the same autoindex), but `pragma_table_xinfo` records `INTEGER`, not `INT`. `"int"` and `'int'` as the type record `INT`.
- Both orders of the options give `wr` 1 and `strict` 1; `strict, strict, without rowid` is accepted by SQLite too.
- `create table "" (...)` works; the empty name is a name.
- **Eponymous virtual tables.** `select * from json_each` and `select * from pragma_table_list` read rows without a CREATE, in any case of the letters; `drop table json_each` gives `table json_each may not be dropped` and `insert into json_each (key) values (1)` gives `table json_each may not be modified`. 59 of the 67 PRAGMAs of `pragma_pragma_list` give such a table. An engine that knows only its own tables would answer `no such table` for all of them, so the pass reserves these names in every statement, as the lead decided for `sqlite_`.

### What didn't work

My first list of eponymous tables was json_each, json_tree, and the PRAGMAs, from `pragma_module_list` on a used connection. The test of the list failed with `json_each` missing from the modules of a fresh connection, which led me to `sqlite3LocateTable` and `sqlite3JsonVtabRegister` in the amalgamation: SQLite registers `json_each`, `json_tree`, `jsonb_each`, and `jsonb_tree` only when a statement names them. The list now has all four, and the test takes its candidates from `sqlite3JsonVtabRegister` too.

### What I learned

`\v` (0x0b) is not a space for SQLite's tokenizer, though C's `isspace` says it is: the first generator of slice 1 put `\v` in a gap and got `unrecognized token: ""`.

### What was tricky

Naming a token by its owner alone gives the wrong name for expressions: in `values (1 + 2)` the first untaken token is the `1`, which a `Literal` owns. Naming the outermost node that the recognizer did not accept gives `binary operator`.

The checks that look at a token's content (a reserved name, the type `int`, an option's word) must run only when that token is at the cursor; otherwise an earlier untaken token would be reported under the wrong name. `isNext` guards each.

### What warrants review

`recognize.go` as a whole: every path that returns a statement must have taken each token it read. And the decision on the eponymous virtual tables, which goes beyond the roadmap's restrictions (see the report to the lead).

### Future work

None from this step.

## Step 3: The test layers, and moving the grammar generator

**Author:** sql-check-slice-1-builder

### Prompt Context

**Verbatim prompt:** (the same brief) "3. Tests: SQLite is the judge [...] 1. Every accepted statement is checked against SQLite for meaning, not only for syntax. [...] 2. Agreement on syntax errors: on the whole corpus [...], its one-token mutants [...], and sentences from the parser's grammar generator [...] 3. A generator of slice 1 [...] 4. Mutants of supported statements [...] 5. The allow-list holds [...] 6. Hand-written tests [...] 7. Fuzzing [...] 8. Mutation testing of the new package"
**Interpretation:** One function, `verify`, that holds any text to SQLite, called from every layer; and a generator of slice 1 whose statements carry the typed form they must give.
**Inferred intent:** A supported statement is never taken on trust: for each one, SQLite runs it and the typed form must describe what SQLite did.

### What I did

I moved the grammar parser and the sentence generator from `/internal/sql/parser/grammar_test.go` into `/internal/sql/parsetest/grammar.go`, exported (`ParseGrammar`, `NewGenerator`, `Sentence`, `IntN`, `Used`), and changed the parser's `TestGrammar`, `TestGenerate`, `FuzzGenerate`, and `TestRuleCoverage` to use it, with the same random stream. Then I wrote the tests of `/internal/sql/stmt`: `verify` in `check_test.go`, `checkMeaning` in `stmt_test.go`, the generator of slice 1 in `generate_test.go`, the allow-list test in `vocabulary_test.go`, and the hand-written cases. `verify` reads the vocabulary of "not supported" from the README, so the README is the list that the tests hold the code to.

### Why

The brief's second layer needs sentences of the grammar outside the parser's tests, and duplicating 300 lines of generator would let two copies drift.

### What worked

- The corpus: 139,234 texts and the 44,460 prefixes before a byte that is rejected before parsing; 729 supported (3 CREATE TABLE, 192 DROP TABLE, 184 INSERT, 350 SELECT), all of them checked in SQLite. Every supported text looks right on reading: they are plain statements of the corpus, and `INSERT INTO t1( current_date ) VALUES(1)` (CURRENT_DATE as a column name) and `INSERT INTO echo_abc(rowid) VALUES(31427)` (a real column named rowid in a WITHOUT ROWID table) pass the check of meaning too.
- 100,000 mutants of the generator: 80,514 syntax errors, 8,249 not supported, 11,234 supported. The supported ones are all the same statement with a semicolon or comment changed, or the same form with another name (a keyword from the mutation pool) or with the sign dropped. None is another kind of statement.
- The allow-list test found one field that the parser can never set, `Drop.IfExists.Not` (DROP has IF EXISTS, never IF NOT EXISTS), and nothing else missing.

### What didn't work

- `\v` in a gap gave `unrecognized token: ""` (step 2).
- The first decoy table of `checkMeaning` was named `t + "_other"`, and for the table `sqlite` that gave `sqlite: object name reserved for internal use: sqlite_other (code 1)`. The decoy is now `"other_" + t`.
- `golangci-lint` flagged `verify(t, c.src)` with errcheck, since `verify` returned an error that the callers dropped; `verify` now returns no error.
- Requiring a syntax error only where the oracle says `SyntaxError` failed on the known gap: `create table full (virtual int primary key collate STRICT, without rowid;; ...` gives `near ";": syntax error` from the parser and `no such collation sequence: STRICT` (NotSaid, stopped early) from SQLite. The tests count such texts apart, as "syntax error where SQLite does not say".

### What I learned

The corpus holds 44,460 texts with a NUL byte or a byte that is not UTF-8, most from `fuzz.txt`. Without the prefixes before such a byte, the agreement on syntax errors would see only a third of the corpus.

### What was tricky

The generator must write a gap between two words but may write none between a word and punctuation, and a gap must never begin with `-` after a minus sign, where it would make a comment. None of the gaps does.

### What warrants review

`checkMeaning`: each kind of statement checks both what SQLite stores and the error messages that print each form of a name.

### Future work

None from this step.
