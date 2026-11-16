# Diary: Judge SQL on a connection whose schema is loaded

`sqlite3_error_offset` is documented to give the byte offset of the token an error refers to. On a fresh connection, SQLite 3.54.1 loses that offset for an error raised with the end of a SELECT, CREATE VIEW, ATTACH, or DETACH: the action of the command loads the schema, and the nested SQL of the first load clears the offset. The oracle in `/internal/sql/oracle` opened a new connection for every prepare, so it reported that artifact. The parser in `/internal/sql/parser` copied it with a special rule, and the oracle carried `Verdict.OffsetNotSaid` for the multi-row VALUES case. This task loads the schema before the text is prepared, and removes both rules.

## Step 1: Requirements, the warm-up, and the evidence

**Author:** oracle-warm-connection-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "1. **In the oracle, load the schema before the text is prepared**, on every connection that `Judge` uses (also the ones with setup statements, attached databases, and the probe with a space appended), so that SQLite reports the documented offset. Find the cheapest certain way [...] 2. **Then simplify, as far as the evidence allows:** [...] The parser gives the token's offset for every `#N` error, and the special rule (`resetsOffset`, `endsAt`, and what only they need) goes away. [...] `oracle.Verdict.OffsetNotSaid`, its condition [...] and its handling and counting in `parsetest` go away, if no case is left that needs them. [...] The existing rule that gives NotSaid/Overwritten for a `#N` syntax error found only with setup: check whether it is still needed on a warm connection [...] 3. **The oracle's verdict must stay the same in everything else.** Compare, over the whole corpus, the verdicts before and after the change [...] 4. **Measure the cost** [...] 5. **Prove it:** the whole corpus, the mutants, and the generator at zero disagreements; `FuzzParse` on several cores for at least 100 million executions with no finding, and `FuzzGenerate` for a while [...]"
**Interpretation:** Warm every connection in `schema.prepare` after the setup, show by tests against SQLite that the documented offset comes back, compare every verdict on the corpus before and after, then delete the parser's rule, `OffsetNotSaid`, and the harness's handling of it, and keep the setup rule only if it is still needed.
**Inferred intent:** The tests of the parser should rest on SQLite's verdict on the text, not on how new the oracle's connection is, and two special rules that existed only for that artifact should go.

### What I did

I read `/AGENTS.md`, the READMEs of `oracle`, `parser`, and `parsetest`, both diaries of 2026-11-16, and the code paths in `/internal/sqlite/c/sqlite3.c`: `sqlite3ReadSchema`, `sqlite3Init`, `sqlite3InitOne`, `sqlite3InitCallback`, `sqlite3Prepare`, `sqlite3ErrorClear`, `sqlite3ErrorMsg`, `OP_ParseSchema`, `attachFunc`, and the action of `cmd ::= select` (case 84 of `yy_reduce`).

What the source says:

- `sqlite3InitOne` runs `SELECT*FROM"main".sqlite_master ORDER BY rowid` through `sqlite3_exec`. The end of a successful prepare in `sqlite3Prepare` calls `sqlite3ErrorClear`, which sets `db->errByteOffset = -1`. That nested prepare runs after the outer `#N` error has set its offset, so the offset is lost.
- `sqlite3ReadSchema` does nothing once every database has `DB_SchemaLoaded`.
- `cmd ::= select` calls `sqlite3ReadSchema` unless `DBFLAG_EncodingFixed` is set. In `sqlite3InitOne` the flag is masked back after the first call of the callback, so only a row read from a schema table sets it. In an empty database it therefore stays unset, and a prepare of `select 1` always calls `sqlite3Init`, which loads main, temp, and every attached database.
- `attachFunc` calls `sqlite3Init` after it attaches, and `OP_ParseSchema` for CREATE TABLE or VIEW (with a WHERE clause in P4) adds to the loaded schema without clearing it. So a setup statement leaves the schemas loaded, and the warm-up after the setup is a no-op or a load.

`/internal/sql/oracle/oracle.go`: `schema.prepare` calls the new `loadSchema` after the setup and before the text. `loadSchema` prepares and finalizes `select 1`, with a doc comment that explains why. That covers every connection of `Judge`: the first prepare, those with setup, the probe in `reachedEnd`, and the prepare per digit separator in `hasLexicalError`.

Red first: `TestSchema_prepare` in `/internal/sql/oracle/oracle_internal_test.go` prepares each text on a fresh connection (the new helper `prepareFresh`) and with `schema.prepare`, and checks that message, code, and tail are the same and that the offsets are the expected ones. Its first version failed on each of the 13 cases without setup that lose the offset (`Expected "7", but got "-1"` for `select #1`), until `loadSchema` was in. It also failed on two of my guesses: `attach #1 as a` gives 7 on both connections, because the error is raised on AS, before the end. And with setup, the fresh connection gave the token's offset, not -1. So the table now has an expected offset for each connection. The cases include the six rows of the brief's table, which my tests confirm (-1 / 7, -1 / 24, -1 / 7, -1 / 15, -1 / 28, -1 / 15), plus `attach 'x' as #1` (-1 / 14), `values(0),(a) union select #0` (-1 / 27), `values(0),(0) union select #0` (27 / 27), `select ?0` (-1 / 7), `delete from t where #1` (20 / 20), and cases with a table, a trigger on a table, a view, a table in temp, and an attached schema. With setup, the fresh connection already gives the token's offset, because the setup statements load the schema.

Then I compared the verdicts. A scratch test, deleted again, wrote every verdict (kind, message, offset, code, tail, reason, `OffsetNotSaid`, setup) for the corpus and three mutants of each entry, 555,505 texts, to a file in the scratchpad. I made the baseline from the same code with `loadSchema` switched off by a temporary environment variable. Then I ran it again after the change, and once more after all removals.

### Why

A connection whose schema is loaded gives what `sqlite3_error_offset` documents. The -1 came from the order of the oracle's own work: a new connection for every prepare. Loading the schema first is a fact about the connection, not about the text, so it cannot change which statements parse.

### What worked

The comparison: 16 corpus verdicts and 6 mutant verdicts changed, every one only in its offset, from -1 to the offset of the token that the message names. Kind, message, code, tail, reason, and setup did not change on any of the 555,505 texts. In the corpus, 13 changes are `#N` syntax errors (for example `DETACH x IS #1`, -1 to 12, and `select #1`, -1 to 7). The other 3 are `Parses` verdicts with `variable number must be between ?1 and ?32766` (`select ?0`, `select ?32767`, `select ?99999999999999999999`, -1 to 7). The brief did not mention that message. It is not a syntax message, and the harness does not compare offsets of `Parses`. In the mutants, all 6 are `#N` errors. After the change, no `#N` error in the corpus or the mutants has the offset -1. The only syntax errors with -1 are `incomplete input` (2,812 in the corpus) and a digit separator (11).

The cost: `Judge` over the corpus took 3.10, 3.75, and 3.08 s without the warm-up and 3.51, 3.42, and 3.06 s with it, on a busy machine. That spread hides the cost, and I first called it unmeasurable; step 3 corrects that.

### What didn't work

The first scratch dump split the corpus from the mutants by `i % 4`. That is wrong, because `parsetest.Mutants` can give fewer than three mutants. I added an origin column and dumped again.

Several compound shell commands were refused: "This agent is isolated in the worktree [...], but this command is too complex to verify that it stays inside the worktree." I split them, and wrote the scratch files with the Write tool.

### What I learned

`DBFLAG_EncodingFixed` is not set by loading an empty schema, only by reading a row. So in the empty database, the SELECT action calls `sqlite3ReadSchema` on every prepare. The question is never whether that call happens, only whether the schema is loaded yet.

### What was tricky

Making sure the baseline really was the old code: the switch reproduced the old `prepare` exactly, and the `OffsetNotSaid` column of the baseline came from the old code too.

### What warrants review

`loadSchema` and its comment in `/internal/sql/oracle/oracle.go`, against the functions of `sqlite3.c` named above, and the table of `TestSchema_prepare`.

### Future work

None.

## Step 2: Removing the rules, and the setup rule kept

**Author:** oracle-warm-connection-builder

### Prompt Context

**Verbatim prompt:** (same brief, step 2) "Then simplify, as far as the evidence allows"
**Interpretation:** Delete what only the lost offset needed, test first, and keep the setup rule if a reason other than the offset remains.
**Inferred intent:** Less special-case code in the parser and the oracle, with no loss of soundness.

### What I did

Parser, test first: in `/internal/sql/parser/parser_test.go`, `TestParse` now expects the variable's offset for `select #1` (7), `create view v as select #1` (24), `attach x as #1` (12), a new `detach #1` (7), and `values(0),(a) union select #1` (27). `TestParse_offsetAfterValues` now checks that every syntax error among its 1,034 texts has an offset. Both failed against the old parser (`Expected "{false near "#1": syntax error 7 0}", but got "{false near "#1": syntax error -1 0}"`, and 94 disagreements `near VARIABLE: syntax error: parser -1`). Then I removed `endsAt`, `resetsOffset`, the field `pendingError.variable`, and the argument of `parser.result` from `/internal/sql/parser/parser.go` and `/internal/sql/parser/expr.go`. The timing of the pending error (`trigger`, `setPending`) is unchanged.

Oracle: I removed `Verdict.OffsetNotSaid`, `offsetNotSaid`, `readsSchemaAfterError`, and `hasMultiRowValues` from `/internal/sql/oracle/oracle.go`, and rewrote the doc comment of `Judge`. `commandStart` and `kindAt` stay, because `insideStatement` uses them. In `/internal/sql/oracle/oracle_test.go`, the two VALUES subtests became one table of 40 texts. Its expected offset is the index of `#0` in each text.

Harness: in `/internal/sql/parsetest/parsetest.go` I removed the skip in `Compare`, `Report.OffsetNotSaid`, its groups, and its part of the header of `Report.String`. `TestRun` in `/internal/sql/parsetest/parsetest_test.go` now shows that a parser that gives no offset disagrees on the `#N` errors at the end, three of five texts, in one group `near VARIABLE: syntax error: parser -1`.

The setup rule stays. A probe on the warm connection showed that in the empty database `create view aux.v as select #1` still gives `unknown database aux` at 12, with the tail 30, at the end. The action's error replaces the `#N` error on the same token, so a syntax error found with `aux` attached is not SQLite's verdict on the empty database. That is about the message, and the warm connection does not change it. `TestSchema_prepare` pins it. The probe also showed the rule is conservative: `create table aux.t as select #1` stops early with `unknown database aux` in the empty database, before the `#N`, yet is `NotSaid` with setup. That was so before too, and I left it.

### Why

Both removed rules existed only for the -1 of a fresh connection, and after step 1 no text needs them: no `#N` error with -1 is left in the corpus or the mutants, and the fuzzer found none (step 3).

### What worked

All three packages passed with the counts unchanged. Corpus: 139,234 texts, 138,926 agree, SQLite does not say on 308. Mutants: 416,271 texts, 415,526 agree, 745 not said. Generator: 60,000 texts, 54,643 agree, 5,357 not said. `TestParse_offsetAfterValues`: 1,034 texts, 1,008 syntax errors, all with an offset, 26 not said; before, 729 of them were counted as offset not said. The final dump against the baseline gave the same 22 offset changes and nothing else.

### What didn't work

`TestJudge_corpus` and `FuzzJudge` failed on property 4 of `check` ("a syntax error is the first"):

    oracle_test.go:355: syntax error before the first: near "#00000000000000001558330667639e182�03": syntax error
    --- FAIL: FuzzJudge/seed#85 (0.00s)
        oracle_test.go:343: syntax error before the first: near "#1": syntax error

The property had passed only because the prefix's `#N` error had the offset -1. In a text like `select #1 2` the error is `near "2"` at 10, because a syntax error on the token after a `#N` wins. The prefix `select #1 ` now gives the `#1` error at 7. That is SQLite's behaviour, not a fault. The property now allows a `#N` that is the last token of the prefix (`lastVariable` in the test). I also added to `check` that a `near "X"` syntax error never has the offset -1, so the corpus test and `FuzzJudge` look for any case that is left.

### What I learned

A property test can depend on an artifact without saying so. Property 4 was right only because of the lost offset.

### What was tricky

Deciding whether the setup rule had been about the offset or the message. The diary of the parser says both. The probe on the warm connection settled it: the message differs in the empty database.

### What warrants review

`check` and `lastVariable` in `/internal/sql/oracle/oracle_test.go`, and the doc comment of `Judge` on the setup rule.

### Future work

None.

## Step 3: Documentation, self-review, fuzzing, and the cost measured again

**Author:** oracle-warm-connection-builder

### Prompt Context

**Verbatim prompt:** (same brief) "READMEs of `oracle`, `parser`, and `parsetest` say what is true now [...] Self-review before you report." and "5. **Prove it:** [...] `FuzzParse` on several cores for at least 100 million executions with no finding, and `FuzzGenerate` for a while"
**Interpretation:** Have a writer update the three READMEs, review the branch with competing reviewers, and fuzz and test the final code.
**Inferred intent:** The branch can be merged without the lead having to redo any check.

### What I did

A writer sub-agent updated the three READMEs in its own worktree, on top of this branch. I cherry-picked its commit, read the diff against the code, and made three fixes myself: the name of the setup rule's test in `/internal/sql/oracle/oracle_test.go`, which still gave the offset as the reason; the new fuzz runs in `/internal/sql/parser/README.md`; and the sentence on the cost in `/internal/sql/oracle/README.md` (below).

The code-review skill (medium, `main...oracle-warm-connection`) reported ten points and no fault in the code. What I did with each:

1. to 3. The READMEs were stale. The writer was already updating them.
4. "The oracle now judges on a loaded connection, not a fresh one; this changes the oracle's contract." That is the change the brief asks for. I report it, and change nothing.
5. "`TestParse_offsetAfterValues` can pass while checking nothing." Agreed. It now requires that more than nine tenths of its texts give a syntax error.
6. "The second subtest of `TestSchema_prepare` reads `r.err.Message` without a nil check." Fixed.
7. "`prepareFresh` also skips `DenyProcessPragmas`." Fixed. It now differs from `prepare` only in the load.
8. "`loadSchema` adds a prepare to every prepare, even after a setup that has loaded the schema." True, but skipping it after setup would rest on an argument where a test is now enough. I kept the uniform call. The cost is below.
9. "The new invariant that a `near "X"` error has an offset may fail on fuzz input if other nested SQL clears the offset." That is what it is for: it finds any such case. `FuzzJudge` found none.
10. "One line of the comment of `loadSchema` is not wrapped." Fixed. The diary is this file.

The writer could not confirm that the cost is not measurable. I measured again with a scratch test, deleted again, that prepared each corpus entry on a bare new connection and with `schema.prepare`, interleaved, three rounds. The bare prepares took 1.58, 1.59, and 1.80 s, and with the load 2.11, 2.12, and 2.22 s. That is about 4 µs per prepare. `Judge` over the whole corpus took 2.97, 2.67, and 2.68 s in the same rounds, so the corpus still takes about 3 s. The README now says that.

Fuzzing, on 8 cores of a machine with a load average of about 30 from other work:

    go test -run '^$' -fuzz '^FuzzParse$' -fuzztime 20m -parallel 8 .
    fuzz: elapsed: 20m0s, execs: 77710539 (0/sec), new interesting: 17 (total: 7015)
    go test -run '^$' -fuzz '^FuzzParse$' -fuzztime 15m -parallel 8 .
    fuzz: elapsed: 15m0s, execs: 52087804 (0/sec), new interesting: 4 (total: 7019)
    go test -run '^$' -fuzz '^FuzzGenerate$' -fuzztime 10m -parallel 8 .
    fuzz: elapsed: 10m0s, execs: 12011778 (0/sec), new interesting: 12 (total: 991)
    go test -run '^$' -fuzz '^FuzzJudge$' -fuzztime 10m -parallel 8 .   (in /internal/sql/oracle)
    fuzz: elapsed: 10m0s, execs: 37366146 (0/sec), new interesting: 84 (total: 799)

That is 129,798,343 executions of `FuzzParse` in all, and none of the runs found anything. The first `FuzzParse` run started before the review fixes, which changed only tests and a comment. The regression input `/internal/sql/parser/testdata/fuzz/FuzzParse/3a4e020f0616c39a` still passes.

Checks: `make lint` and `make test` pass. `make test-asan` passed in Docker in 9 minutes 58 seconds: all tests, then each fuzz test for 30 seconds. Under the sanitizer, `oracle` took 10.7 s, `parser` 24.1 s, and `parsetest` 18.2 s, and the log has no LeakSanitizer or AddressSanitizer report. `go test -race` of `oracle`, `parsetest`, and `parser` passes, and the changed tests passed `-race -count=50` with the machine under load. Statement coverage of `parser` is still 99.5%, with the five places its README lists. That of `oracle` went from 92.9% to 90.6%, because covered code went away: the uncovered statements are the error return of `loadSchema`, like the other failure paths of `prepare`, and the bound in `kindAt`, which only `commandStart` uses now.

Run times under the same load: `make test` took 1:08, against 1:43 for the baseline on main at the start, so the machine's load changed between the runs. Of the packages, `oracle` went from 9.6 to 14.0 s, `parser` from 24.1 to 34.1 s, and `parsetest` from 8.6 to 11.9 s. The `sqlite` package, which this change does not touch, went from 41.0 to 58.7 s in the same runs.

### Why

The fuzzers compare the parser, which now always gives the token's offset, with SQLite on the loaded connection. A `#N` error with -1 anywhere would be a disagreement in `FuzzParse` and a failed property in `FuzzJudge`.

### What worked

The writer's caution on the cost. My first numbers were inside the noise, and I had written "none measurable" in its brief.

### What didn't work

My own first claim on the cost, as above. Also, `git status --short && (time make lint)` was refused: "this command names git in a form too complex to verify that it stays inside the worktree".

### What I learned

To measure a small cost on a busy machine, interleave the two variants per item instead of timing whole runs one after the other.

### What was tricky

Getting 100 million executions on a machine with a load average of about 30. It took two runs.

### What warrants review

The sentence on the cost and the section "The loaded schema" in `/internal/sql/oracle/README.md`.

### Future work

The writer's worktree and branch, `worktree-agent-a542f95dc522add97`, can be removed. Its commit is on this branch.
