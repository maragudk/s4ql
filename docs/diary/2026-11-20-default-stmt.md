# Diary: Slice 11 of the roadmap in the statement pass, `default` with a literal

Slice 11 adds `default <literal>` to a column declaration, where the literal is one of the literals that `insert` takes, or `null`. An `insert` with a list of columns that leaves the column out stores the default. The reason is goqite, the queue that glue uses, whose schema has two columns with `default 0`. This task is the statement pass, `internal/sql/stmt`, and its generator `stmttest`; the engine refuses a create table with a DEFAULT as not supported, with the error that the pass gave it before, until its builder comes, and the core and the commit format change in a later step. The work is on the branch `default-stmt`, from `d801295` on `main` (pull request #107, the pass of slice 10).

## Step 1: SQLite's behaviour, the field, the recognizer, the checks, the generator, and the refusal in the engine

**Author:** default-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Task: the statement pass of slice 11. Only `internal/sql/stmt` and its test helper `internal/sql/stmt/stmttest`. [...] 1. `stmt.Column` (the column of `stmt.CreateTable`) carries its default: whether one was written, and its value as the kind of value that `insert`'s terms carry (`stmt.Value`: integer, real, text, blob, null), shaped like its neighbours. Reuse the recognition of literals that `insert` has; do not write a second one. 2. The rule of the package holds without exception [...] Find out from SQLite itself, not from memory, what it does with: a signed number [...], `default true` and `default false`, a hex integer, a real in every form `insert` takes, a text with quotes inside, a blob literal, `default null` on a column that is `not null` and on the primary key column, a double-quoted word [...], a bare word, `default ?`, `default (1)`, `default (1+1)`, `default current_timestamp`, `default` with nothing, `default` twice on one column, `default` before and after `not null` and `primary key`, `default` on the primary key column, a default whose literal is not of the column's type on a `strict` table [...], and a literal longer than the length limit. [...] For a default whose type is not exactly the column's type: s4ql's rule of exact types refuses such a value at `insert`, so a declaration that could never store its default is 'not supported' at `create table`, with a name of its own, unless the measurement in SQLite gives a reason against it (say so). [...] 3. `CreateTable.SQL` [...] must still equal what SQLite stores in `sqlite_schema` for every supported text, defaults included [...] 4. `insert ... default values` stays 'not supported' [...] 5. For the engine and core builders after you, measure in SQLite and record in the diary and in `docs/todo.md` [...] 6. `stmttest`: the `Generator` writes defaults, valid and invalid, behind an option that is off by default [...] show that with the option off the generator's output is byte-identical to before [...] 7. [...] the engine answers 'not supported' for a `create table` that carries a default, at one place, with a test that pins it, in the manner of `existsRefusal` [...]"
**Interpretation:** Take DEFAULT and a literal token for token in a column declaration, by the code that reads the value of an insert, refuse every other form of DEFAULT with a name and at a token, refuse a literal of another type than the column, find each fact with the pinned shell and pin it in tests, generate defaults behind `Generator.Defaults`, and refuse a create table with a DEFAULT in the engine with the old error.
**Inferred intent:** The front half of slice 11 with the same trust as slices 1 to 10, and the facts that the builders of the engine and the core need: what an insert stores, the order of its errors, and the exact `dflt_value` texts that the cache will read back.

### What I did

SQLite first, with the shell built from `/internal/sqlite/c/shell.c` and `/internal/sqlite/options.h` into the scratchpad (`cc -include options.h ... shell.c sqlite3.c`). 65 create tables, each with one form of DEFAULT, then `pragma_table_info` of each, an insert that leaves the column out of each, every pair of a literal kind and a column type, the key with a DEFAULT, DEFAULT NULL on a NOT NULL column and on the key, the order of the errors of an insert under `pragma query_only` and the length limit of 1,000 (`.limit length 1000`), and an update and a select. The findings, all in `/docs/todo.md`:

- SQLite takes every form at create, also a literal of the wrong type, `true`, `+1`, `0x10`, `1_000`, `-'a'`, `"abc"`, `abc`, `[abc]`, `current_time`, `-current_time`, two DEFAULTs (it keeps the last), and DEFAULT NULL on a NOT NULL column or the key. Syntax errors: `default ?`, `default` with nothing, `- -1`, `+-1`, `~1`, `not null`, `-?`, `-(1)`, `()`, `'x' 'y'`, `on`, `primary key`, `1.5e`, `x'0'`. `default (?)` gives `default value of column [v] is not constant`, an error of a grammar action.
- At the insert, a STRICT table converts or refuses the default as it does a value: an INT column takes `1.0` and `'1'` as 1 and refuses `1.5`, a TEXT column stores `1.0` as `'1.0'`, a BLOB column refuses everything but a blob. So the exact types of an insert would refuse such a default every time, and the pass refuses the declaration.
- `dflt_value` is the text of the literal as written, from its sign to its end: `- /* c */ 1`, `'it''s'`, `X'aBcD'`, `NULL`.

The code. `/internal/sql/stmt/stmt.go`: `Column.Default` of the new type `Default`, with `Value` (nil for none, `Null{}` for NULL) and `Span` (of the literal with its sign, which is `dflt_value`). `/internal/sql/stmt/recognize.go`: `recognizer.literal` is split into `recognizer.sign`, which takes a `-` and refuses every other sign, and `recognizer.value`, which reads the literal; `literal` calls both for an insert, and the new `recognizer.defaultValue` calls both for a DEFAULT, whose sign is a token of its own in the tree (`ast.DefaultConstraint.Sign`). `defaultValue` refuses TRUE and FALSE, a name (`DefaultConstraint.Name`, named `name after DEFAULT` in `/internal/sql/stmt/vocabulary.go`), parentheses (`DefaultConstraint.LParen`, `DEFAULT of an expression`), and, through `takes`, a literal of another type (`DEFAULT of another type than its column`, at the literal). `recognizer.column` takes the DEFAULT anywhere among PRIMARY KEY and NOT NULL, once (`second DEFAULT`). The four new names are in the tables of `/internal/sql/stmt/README.md`.

Tests. `/internal/sql/stmt/check_test.go`: 36 supported cases, 48 not supported, and 13 syntax errors in place of the two cases that refused every DEFAULT; a subtest with a table like goqite's; and the subtest "should answer for DEFAULT what SQLite does when an insert, an update, or a select meets it", with 19 texts on a database of five tables, with and without `pragma query_only`, at the length limit of 1,000. `/internal/sql/stmt/stmt_test.go`: `createColumns` writes the DEFAULT in the canonical text; `db.schema` compares a default by the value that SQLite gives its text, since `dflt_value` differs between spellings; `checkCreateTable` checks `dflt_value` against the span; and the new `checkDefaults` checks, on the table of the text and on that of the canonical text, that an insert that leaves the column out stores the value, or gives `NOT NULL constraint failed`, and for the key a `UNIQUE constraint failed` the second time. `/internal/sql/stmt/vocabulary_test.go`: the allow-list has `DefaultConstraint` and its fields `Default`, `Sign`, and `X`. `/internal/sql/stmt/generate_test.go`: `newGenerator` sets `Defaults` to 3, `difference` tells a default apart, and the near misses have 39 forms of DEFAULT that are not supported or are syntax errors, in place of three that are now supported.

`/internal/sql/stmt/stmttest/stmttest.go`: `Generator.Defaults` and `Generator.withDefault`, which draws only when Defaults is not 0 or `Create` gives a `Default.Value`, and puts the DEFAULT at a place at random among the constraints. `TestGenerator_Defaults` in `/internal/sql/stmt/stmttest/stmttest_test.go`.

The engine: `defaultRefusal` in `/internal/engine/defaults.go`, called once in `check` in `/internal/engine/engine.go` after `existsRefusal`, gives `DEFAULT` at the keyword of the first DEFAULT, found as the last token before the span of its literal; `TestCheck_defaults` in `/internal/engine/defaults_test.go` pins it.

`/internal/sql/mutation/equivalent.txt`: three equivalent mutants of `recognizer.literal` are now in `recognizer.value`, and `TestEquivalent` failed with `no such mutant: stmt/recognize.go recognizer.literal constant: math.IsInf(f, 0) => math.IsInf(f, 1)` until they were renamed.

### Why

The brief's list of forms and the rule of the package; SQLite's behaviour decided every detail, and the rule of exact types decided the refusal of a default of another type.

### What worked

- Every supported case passed its meaning check against SQLite on the first run, with the spans of a script of my own. Two planted faults showed that the checks bite: a value one higher made the canonical text differ (`the canonical text ... gives ...`), and with the type check off `checkDefaults` failed with `"insert into \"t\" (\"k\") values (7)" stores real 1 (0x3ff0000000000000) in v, want integer 1`.
- A dump of 58,920 lines, statements and calls of `Generator` in eight settings and of `Sequence`, over 30 seeds, written before and after the change with Defaults 0, was the same byte for byte once the 16,658 zero `Default:stmt.Default{...}` fields of `%#v` were stripped, and no other `Default:` was left.

### What didn't work

- A command that wrote a file with a heredoc (`cat > m1.sql <<'EOF'`) was refused by the permission system as "too complex to verify that it stays inside the worktree"; scripts written to the scratchpad with the Write tool and run with `python3 script.py` worked.
- My first table of the insert facts expected `string or blob too big` for `insert into z (k, a) values (1, '<600 bytes>')`, and SQLite gave `NOT NULL constraint failed: z.c`: `c` was `not null default null`, and NOT NULL comes before the size of the record. That is now a fact of the table.

### What I learned

SQLite's grammar gives DEFAULT a sign of its own (`ccons ::= DEFAULT MINUS scantok term`), so the sign and the literal are not a unary expression as in VALUES, and `dflt_value` is the text from the sign to the end of the term, comments between them included. A name after DEFAULT, quoted or not, is a string even with `SQLITE_DQS=0`, which `options.h` already says.

### What was tricky

The pinned numbers. `TestGenerate_mutants` went from 29 to 41 syntax errors and from 151 to 154 not supported where SQLite does not say, and `TestGenerate_nearMisses` from 1,760 to 2,130. A throwaway tally, hooked into `verify` and removed before the commit, grouped the excused texts by SQLite's message: the mutants are all `duplicate column name` (40 and 22), `unknown table option` (132), and one `no such collation sequence`, the same kinds as before, moved because the generator draws more numbers with Defaults on. The near misses are 272 for each of five forms of `table "t" has more than one primary key`, 272 `generated columns cannot be part of the PRIMARY KEY`, 272 `default value of column [...] is not constant` of the new form `default (?)`, 216 `AUTOINCREMENT ...`, and 10 `non-deterministic functions ...`: 272 create tables of 2,000, where 258 were.

### What warrants review

- `recognizer.sign`, `recognizer.value`, and `recognizer.defaultValue` in `/internal/sql/stmt/recognize.go`: the split of `literal` must not change what an insert accepts (the cases of slices 4 to 8 all pass unchanged), and the offset of the type refusal is that of the literal with its sign.
- `checkDefaults` and the change of `db.schema` in `/internal/sql/stmt/stmt_test.go`.
- The groups of slice 11 in `/docs/todo.md`, above all the order of the errors and the `dflt_value` texts for the core.

### Future work

- The README of the pass, beyond the four names of the vocabulary, and the review: step 2.
- The engine and the driver for slice 11, which remove `defaultRefusal`; the core and the commit format, whose cache must tell a column with `not null default null` from a nullable one.

## Step 2: The README, the review, and the checks

**Author:** default-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Finish: `make test`, `make lint`, `make check-browser`, `make fuzz-shards` pass; `make test-asan` only for the packages whose fuzz tests you changed. Self-review against these requirements."
**Interpretation:** Bring the README of the pass up to date, review the branch with the code-review skill, fix what matters, and run every check.
**Inferred intent:** A branch whose code, numbers, and documents agree, ready for the lead and for the builders of the engine and the core.

### What I did

A writer sub-agent updated `/internal/sql/stmt/README.md`: the typed form (`Column.Default`, `stmt.Default`), the column-def grammar, a section on DEFAULT under CREATE TABLE in the shape of the one on IF NOT EXISTS, the INSERT sentences that a DEFAULT changes, "How it is tested", and "Results" from a full run of the package (`go test -count=1 -v ./internal/sql/stmt`, 100 s). I read its diff. It found two claims of `/docs/todo.md` that no test pins, the NOT NULL of the first of two left-out columns and the place of `no such table`; the todo now says that both were measured with the shell only. Coverage under `-short` is still 99.5%, now with the code of slice 11.

The review (`code-review high origin/main...HEAD`) found one real fault: `check` in `/internal/engine/engine.go` called `existsRefusal` before `defaultRefusal`, so `create table if not exists t (k int primary key default 1) ...` got `IF NOT EXISTS (at offset 13)` from the engine and the driver, where on `main` it got `DEFAULT (at offset 48)`: the pass refused the DEFAULT itself and never gave the statement. That is goqite's own shape, IF NOT EXISTS and DEFAULT together. `defaultRefusal` now comes first, and `TestCheck_defaults` pins `DEFAULT (at offset 48)`; the todo and the README say so. The review found nothing else: the split of `recognizer.literal` changes no answer of an insert, 30 extra texts of its own got the expected values and refusals, and the generator draws as before with Defaults 0.

### Why

The README is the definition of the forms, and the engine's refusal must give what the pass gave before, so that the engine and the driver answer as they did until their builder comes.

### What worked

`make test` (with the crosscheck module), `make lint` (0 issues), `make check-browser`, and `make fuzz-shards FUZZGROUP=c FUZZSHARDS=4` and `FUZZGROUP=go FUZZSHARDS=2` passed. `make asan-test ASANPKGS="maragu.dev/s4ql/internal/sql/stmt maragu.dev/s4ql/internal/sql/stmt/stmttest"` in the `golang` image passed (49 s and 2 s): the seeds of `FuzzCheck` and `FuzzGenerate` changed with the cases and the generator, though their code did not. `go test -race -count=20` of `TestGenerator_*` passed, and after the fix of the order the engine's tests under `-short`, the root package, and the crosscheck module passed again.

### What didn't work

`make test` ran before the fix of the order of the refusals; only the engine, the root package, and the crosscheck module ran again after it, since nothing else calls `check`.

### What I learned

A refusal that stands in for an old answer has to reproduce the old order of the refusals too, not only the old message: the pass of slice 10 had made IF NOT EXISTS a supported token, so on `main` the DEFAULT after it was the first token that the pass did not take.

### What was tricky

Nothing beyond step 1.

### What warrants review

- The section on DEFAULT under CREATE TABLE and the "Results" of `/internal/sql/stmt/README.md`.
- The order of `defaultRefusal` and `existsRefusal` in `check` of `/internal/engine/engine.go`.

### Future work

- The engine and the driver for slice 11; the todo has what to remove and the facts of SQLite that they need.
- The core and the commit format for slice 11, with the `dflt_value` texts in the todo.
- Mutation testing and long fuzz runs of the code of slice 11 in the pass.
- The status of the README, the site, and the roadmap are for the lead's sub-agent after the merge.
