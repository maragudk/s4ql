# Diary: Take the emulation of SQLite's code generator out of the SQL parser

The SQL parser in `/internal/sql/parser` imitated part of SQLite's code generator to match the offset of one syntax error: a variable `#N` at the end of a SELECT, CREATE VIEW, ATTACH, or DETACH keeps its offset after a multi-row VALUES with constant rows, and loses it otherwise. The lead decided that this does not belong in the parser. The oracle in `/internal/sql/oracle` now says that it does not know that offset, the harness in `/internal/sql/parsetest` does not compare it, and the parser gives -1 there.

## Step 1: Requirements, removal, and the oracle's new condition

**Author:** parser-no-codegen-emulation-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "1. **Remove the emulation from the parser**: `constant.go`, `functions.go`, their tests, and the `withAt` / `schemaReadAt` / `valuesRow` machinery. [...] 2. **Make the oracle say that it does not know the offset** in exactly the cases where SQLite's offset depends on whether it read the schema during the parse. [...] Decide the condition from the text at the token level, conservatively, without any knowledge of functions or constant expressions [...] Find the narrowest condition that is certain to cover every case in which the removed emulation changed the answer; if in doubt, widen it. [...] 3. **Prove it**" (tests in the oracle, the corpus, mutants, and generator at zero disagreements, the fuzz regression input kept, at least 100 million executions of `FuzzParse`, statement coverage of the parser).
**Interpretation:** Revert commit `117c471` and its follow-ups in the parser, keep the simple `#N` rule (`resetsOffset`), add a flag to `oracle.Verdict` for a syntax error whose offset is not a fact of the grammar, and make `parsetest` skip the offset for it and count such texts apart.
**Inferred intent:** The parser's tests must depend only on SQLite's grammar, not on its code generator, the list of registered functions, or the next upgrade of SQLite.

### What I did

I read `/AGENTS.md`, the READMEs of the parser, oracle, and harness, step 5 of `/docs/diary/2026-11-16-sql-parser.md`, `/internal/sql/oracle/oracle.go`, `/internal/sql/parsetest/parsetest.go`, and the parser code that commit `117c471` touched. The branch `parser-no-codegen-emulation` starts from `origin/sql-parser`.

Removal: `git rm` of `/internal/sql/parser/constant.go`, `constant_test.go`, `functions.go`, and `functions_test.go` (574 lines), and the fields `withAt` and `schemaReadAt` and the call of `valuesRow` in `/internal/sql/parser/parser.go` and `/internal/sql/parser/select.go`. `/internal/sql/parser/export_test.go` went back to its state before `117c471`; `git diff 117c471^ -- internal/sql/parser/*.go` is now empty. The parser then failed its own fuzz regression input, as expected:

    --- FAIL: FuzzParse/3a4e020f0616c39a (0.00s)
        parser_test.go:358: other offset: "VALUES(0),(0)UNION SELECT#0": SQLite: syntax error "near \"#0\": syntax error" at 25, tail 27; parser: ok=false "near \"#0\": syntax error" at -1, tail 0

The oracle: `Verdict.OffsetNotSaid` is a bool, set by `Judge` through `schema.offsetNotSaid` for a `SyntaxError` with the message of a `#N`, when all three hold:

1. The error came with the statement's end. This reuses `reachedEnd`: anything but `StoppedEarly`, `InsideTrigger`, and `InsideArguments` counts as the end, `Unsettled` and `NotParsed` too.
2. `readsSchemaAfterError`: after EXPLAIN or EXPLAIN QUERY PLAN, the first token is SELECT, VALUES, WITH, ATTACH, or DETACH, or the statement starts with CREATE [TEMP] VIEW. WITH also lets INSERT, UPDATE, and DELETE through, which widens.
3. `hasMultiRowValues`: a VALUES followed by `(`, and after its matching `)` a `,` and a `(`.

`Offset` keeps what SQLite gave. I kept the existing rule for a `#N` found with setup (`NotSaid`, `Overwritten`) and did not fold it into the flag: there, the empty database gives another message (`unknown database aux`), so the message is not SQLite's verdict either, and turning it into a `SyntaxError` with the offset unsaid would claim more than the oracle knows.

The harness: `Compare` returns no disagreement after the message check when `OffsetNotSaid` is set; `Report.OffsetNotSaid` counts the texts that agree on the rest, which are not in `Agreed`; `Disagreements` subtracts them; they are listed in `NotSaidGroups` under the key `offset not said: <pattern>`; `String` says `SQLite does not say on N, nor the offset on M`.

Tests: two subtests in `TestJudge` in `/internal/sql/oracle/oracle_test.go` with 19 texts that get the flag (14 that keep the offset, 5 that give -1, among them the fuzz input and the shapes of the removed `TestParse_schemaRead`) and 11 that do not (an error before the end, INSERT, DELETE, CREATE TABLE AS, a trigger step, single-row VALUES, a VALUES whose row has inner parentheses, and the corpus entry described below), plus one for the setup rule with VALUES. `TestCompare` and a new `TestRun` in `/internal/sql/parsetest/parsetest_test.go`. In `/internal/sql/parser/parser_test.go`, `TestParse` pins `values(0),(0) union select #1` at -1, and `TestParse_sqlite` has five more texts.

### Why

The three conditions are each necessary for SQLite to keep an offset that the end would otherwise lose. The parse loop in `sqlite3RunParser` stops on the token whose action raised the error, so an error raised before the end has the token's offset with any schema. Of the callers of `sqlite3ReadSchema` in `/internal/sqlite/c/sqlite3.c` (`sqlite3Analyze`, `codeAttach`, `sqlite3LocateTable`, `sqlite3StartTable`, `sqlite3DropTable`, `sqlite3CreateIndex`, `sqlite3DropIndex`, `sqlite3Reindex`, `sqlite3MultiValues`, `sqlite3Pragma`, `sqlite3DropTrigger`, and `cmd ::= select` in `yy_reduce`), only `sqlite3MultiValues` runs before the end of one of the four commands, and only for the rules `mvalues ::= values COMMA LP nexprlist RP` and `mvalues ::= mvalues COMMA LP nexprlist RP`. The removed emulation changed the answer only when `valuesRow` set `schemaReadAt`, which needs a second row, `resetsOffset` of the command, and `endsAt` of the error, so the condition covers each of those by a superset.

### What worked

The corpus, the mutants, and the generator agreed with zero disagreements at once. A first, wider version (any VALUES token) gave one corpus text the flag: `WITH i(x) AS ( VALUES(1) UNION ALL SELECT x<1 FROM i ORDER BY 1)\nSELECT x FROM i LIMIT 0001-01-0,#0`, a single-row VALUES whose offset is -1 with any schema. Requiring a second row brought the corpus to zero, which `TestJudge` now pins with that text.

Counts with the final condition: corpus 139,234 texts, 138,926 agree, SQLite does not say on 308, offset not said on 0. Mutants (`TestParse_mutants`): 416,271 texts, 415,526 agree, 745 not said, 0 offset not said. Generator (`TestGenerate`): 60,000 texts, 54,643 agree, 5,357 not said, 0 offset not said. With a scratch test that I deleted again: of 194 `#N` syntax errors in the corpus, 3 have a VALUES before the tail; of 964 among the mutants, 46.

Statement coverage of `parser` is 99.5%, with the same five places that its README lists.

A first run of `FuzzParse` on 10 cores with this version made 78,667,800 executions in 8 minutes 33 seconds without a finding. I stopped it when the review in step 2 narrowed the condition, so that the counted run is on the final code.

### What didn't work

My first commit took the deletions of the parser with it: `git rm` had staged them, and `git add internal/sql/oracle && git commit` committed everything staged. I undid the three local commits with `git reset --soft 20acdfa` and `git reset`, and committed again file by file.

One expected offset in the new oracle test was wrong by hand: `delete from t where #0 in (values(0),(0))` has the tail 25 (after `in`), not 23. And `values((0),(0)),(0) union select #0`, which I meant as a row with inner parentheses, is `NotSaid`: the first row has two terms and the second one, and `sqlite3MultiValues` raises `all VALUES must have the same number of terms` while it parses. I used `values((0)+(0)),(0) union select #0`, which keeps the offset 33.

A python edit of the oracle test through Bash was refused by the sandbox ("this command is too complex to verify that it stays inside the worktree"); I made the same edits with the Edit tool.

### What I learned

SQLite's offset for this error is not only a matter of schema and functions: whether the rows are even accepted (`all VALUES must have the same number of terms`) is decided while SQLite parses a multi-row VALUES, so such a text never reaches the `#N` question. The oracle handles it as `StoppedEarly` already.

### What was tricky

Choosing how narrow to go. Requiring "no WITH before the VALUES" would also be sound in SQLite (`withnm ::= nm` sets `bHasWith` before any row of a VALUES after it is reduced), but it would rest on knowing where WITH can stand as a keyword, which is more than a token check, so I left it out: those texts only lose a comparison of the offset.

### What warrants review

`offsetNotSaid`, `readsSchemaAfterError`, and `hasMultiRowValues` in `/internal/sql/oracle/oracle.go`, against the argument above; the change in `Run` and `Compare` in `/internal/sql/parsetest/parsetest.go`, in particular that a text with the flag and another message is still a disagreement. Validate with `go test ./internal/sql/oracle ./internal/sql/parsetest ./internal/sql/parser -v -run 'TestJudge$|TestRun|TestCompare|TestParse_corpus|TestParse_mutants|TestGenerate'`.

### Future work

None.

## Step 2: Self-review, a narrower WITH rule, the broad test back, and the fuzz runs

**Author:** parser-no-codegen-emulation-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Self-review before you report." and "Run `FuzzParse` and `FuzzGenerate` again on several cores until they are quiet: at least 100 million executions of `FuzzParse` with no finding."
**Interpretation:** Run the code-review skill over the branch, act on what holds up, and then fuzz the final code.
**Inferred intent:** The condition must be both sound and as narrow as can be proved, and the proof must be on the code that is handed in.

### What I did

The code-review skill (medium) found no correctness bug and six points. What I did with each:

1. "Drop `token.With` and count only a VALUES before the first WITH token." Half right. WITH is in the `%fallback ID` list of `/internal/sql/parser/testdata/parse.y`, so a WITH in the middle of a statement may be a name: `select with from (values(0),(0)) union select #0` keeps the offset 46, which the suggested rule would have compared and got wrong. A WITH as the first token of the command is always the keyword, and `withnm ::= nm` sets `bHasWith` on its first name, before any row of a VALUES after it. So `readsSchemaAfterError` no longer accepts a statement that starts with WITH; a WITH elsewhere is not used. Both texts are in `TestJudge` now, with `with c as (values(0),(0)) select #0`, `explain with ...`, and `create view v as with c as (select 1) values(0),(0) union select #0` (flagged, offset -1).
2. "`TestParse_schemaRead` was the only broad test of soundness." Agreed. It is back as `TestParse_offsetAfterValues` in `/internal/sql/parser/parser_test.go`, unchanged in its 1,034 texts, with the parser that gives -1: zero disagreements means the oracle flags every text on which SQLite keeps the offset. SQLite keeps it on 389, the oracle flags 729, and SQLite does not say on 26. The removed `TestBuiltinFunctions` I did not bring back: the oracle's rule does not look at the expression in a row, so every function is the same to it.
3. "`reachedEnd` runs before the cheap token checks, and may prepare a probe." Fixed: the token checks come first.
4. "The EXPLAIN prefix is skipped in two places." Fixed with `commandStart`, used by `insideStatement` and `readsSchemaAfterError`. I left the cutting of the statement's tokens in `reachedEnd` alone: it computes them around the stop token, which `offsetNotSaid` does not need.
5. "A test row under the wrong comment." Already fixed in the worktree, and committed with point 1.
6. "`NotSaidGroups` holds the `offset not said` groups, which `NotSaid` does not count." Left as it is; the doc comment of `NotSaidGroups` says so, and the README of `parsetest` too.

Then, on the final code, with `-parallel 10` in `/internal/sql/parser`:

    go test -run '^$' -fuzz '^FuzzParse$' -fuzztime 20m -parallel 10 .
    fuzz: elapsed: 20m0s, execs: 173741251 (0/sec), new interesting: 34 (total: 6998)
    go test -run '^$' -fuzz '^FuzzGenerate$' -fuzztime 10m -parallel 10 .
    fuzz: elapsed: 10m0s, execs: 14200216 (0/sec), new interesting: 45 (total: 979)

Neither found anything. `make lint` and `make test` pass, and `go test -race` of `oracle`, `parsetest`, and `parser`. The new and changed tests passed `-count=50 -race` while `make test-asan` ran on the same machine.

`make test-asan` passed in Docker, with all tests and then each fuzz test for 30 seconds under the address sanitizer. The lead asked me to watch for the LeakSanitizer report that the parser's test binary gave once before: the log of this run has none.

### Why

The first point would have made the oracle wrong on a text that no test had; checking the grammar's fallback list before acting on it was what kept that out.

### What worked

Running the old broad test unchanged against the new code: it is a direct check that the token rule covers every place where SQLite keeps the offset, in forms of expression that no hand-written row has.

### What didn't work

My guess for `create view v as with c as (select 1) values(0),(0) union select #0` was that it keeps the offset; SQLite gives -1, since the CTE name comes before the rows there too. The test failed with `Expected "65", but got "-1"`, and I moved the row to the -1 group. It stays flagged, because the statement does not start with WITH, which only widens.

### What I learned

The reviewer's own check (the deleted test run against the new code) passed for its rule too, because none of the 1,034 texts uses WITH as a name. A test made of shapes can confirm a wrong narrowing.

### What was tricky

Nothing beyond the WITH question.

### What warrants review

`readsSchemaAfterError` and its comment on WITH in `/internal/sql/oracle/oracle.go`, against `with ::= WITH wqlist` and `withnm ::= nm` in `/internal/sql/parser/testdata/parse.y`.

### Future work

None.
