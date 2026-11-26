# Diary: the workload of text and the engine's differential test

The engine and the driver compute `||`, `lower`, and `hex` since #184, but only hand-written cases check them: the generators of `/internal/sql/stmt/stmttest` write no text, so `TestDifferential`, `TestConn_differential`, the simulation, and the driver's generated test never meet it. This task gives the generators text, as arithmetic got its workload on 2026-11-25, and makes the engine's differential test judge it against SQLite and the rules of `stmttest`.

## Step 1: the brief

**Author:** builder (text-workload)

### Prompt Context

**Verbatim prompt:** "## The task: the workload for text and the engine's differential test, and nothing else 1. The generators write text expressions wherever the engine computes them: `||`, `lower`, `hex`, nested, mixed with columns, literals, and parameters of every type the rules allow and of types they refuse, NULL in every position, empty text, blobs in `hex`, non-ASCII text, text in the key and in indexed columns, text beside arithmetic in one statement, in subqueries, in prepared statements run again with other arguments, and results near the length limit under a lowered limit (just under, exactly at, just over). 2. The rules of `stmttest` that say what s4ql refuses and what it computes stay the oracle beside SQLite: where the generator can produce a form that the notes list as \"s4ql is stricter\" or as one of the three recorded cases where s4ql gives no rows and SQLite gives `string or blob too big`, the differential pair must judge it by a rule that names exactly that case, not by a general excuse. A list of accepted differences is widened only when the recorded difference is shown to cause exactly that symptom. 3. `reached.go`: add the \"sometimes\" points that show the workload reaches each kind of text statement, each refusal, each outcome at the length limit, and text serving an access (a key lookup and an index bound from a text constant), and make the run fail when one is never reached. 4. The engine's `TestDifferential` runs the new workload. [...] 5. If the new workload finds a difference from SQLite that is a fault in the engine, that is the point of the work [...]" Out of scope: planted bugs for text, the driver's generated test for text, mutation testing, upserts in the generators.
**Interpretation:** a new area of the generator with its own stream and profile, as arithmetic has; text written by the hooks of WHERE and SET; the pair of the engine judging every difference from SQLite that text brings by a rule that names one recorded difference; kinds in the `.kinds` files for each kind of text statement, refusal, outcome at the limit, and access served; and the nightly-sized runs before the report.
**Inferred intent:** trust in text rests on the same generated machinery as arithmetic, and the generated statements look for the faults that the hand cases did not think of.

### What I did

Read `/docs/todo/text-functions.md`, the section Text of `/internal/engine/README.md`, `/internal/sql/stmt/stmttest/README.md`, the arithmetic parts of `/internal/sql/stmt/stmttest/sequence.go`, `/internal/sql/stmt/stmttest/arithmetic.go`, and the pair of `/internal/engine/differential_test.go` and `/internal/engine/expression_test.go`. A baseline `go test -count=1 -run '^TestDifferential$' ./internal/engine/` took 71 s at a load of 21.

### Why

The recipe "Add a feature area" of the README of `stmttest` is how every earlier area was added, and its steps keep the other profiles' sequences byte for byte.

### What worked

The rules of `stmttest` already type text (`expressionClass`, `textRefusals`), so the pair's `checkArithmetic` judges the refusals of text without a change.

### What didn't work

Nothing yet.

### What I learned

`stmttest.LengthLimit` is 1,000, and half of the sequences of `TestDifferential` run under it (`variantOf`), so lengths near 1,000 reach the edges of the limit there and pass in the other half.

### What was tricky

The area `text` is already the stream of how a statement is written, so the new area needs another name.

### What warrants review

Nothing yet.

### Future work

The steps below.

## Step 2: the area, the profile, the pair's rules of text, the kinds, and the recounts

**Author:** builder (text-workload)

### Prompt Context

**Verbatim prompt:** (the brief of step 1)
**Interpretation:** write the generator, run it against SQLite, and give every difference that it shows a rule that names its recorded cause, or report it.
**Inferred intent:** the generated statements look for the faults of text, and the rules stay exact.

### What I did

- `/internal/sql/stmt/stmttest/expression.go`: `Generator.givenText`, `textOperand`, and `textValue` write a tree of `||`, `lower`, and `hex` that a hook gives: a call by its name in any case, one time in eight in double quotes, a right operand that is a concatenation in parentheses, a value as a `?` where the pass would refuse the literal (NULL, a number, a blob but in `hex`) or where it is longer than 40 bytes. `Generator.given` hands text at the root to it; `givenTerm` puts no parentheses around a call.
- `/internal/sql/stmt/stmttest/profile.go`: the area `textfunctions` (the area `text` is the spelling of a statement), the fields `TextFunctions`, `TextAgain`, and `TextShapes`, the profile `textfunctions` at slot 15, and `Mixed` with `TextFunctions: 8, TextAgain: 40`.
- `/internal/sql/stmt/stmttest/text.go` (new): `Sequence.textHooks` wraps the hooks after those of arithmetic; `textWhere`, `textCondition`, `textSet`, `textInner`, `textKeyWhere`, `textEdgeWhere`, `textEdge`, `textEdgeConstant`, `textConstantOf`, `textFault`, `goqiteID`, `textLike`, and `TextRefused` (23 texts that the pass refuses). `/internal/sql/stmt/stmttest/sequence.go`: the stream, a text that runs again (`textAgain`), the refused texts in `next`, and a table of TEXT columns in `shapeOf` for `TextShapes`. `/internal/sql/stmt/stmttest/text_internal_test.go`: `TestSequence_text` compares the typed form of 3,000 statements of each of two profiles with the pass's and requires each kind of text.
- `/internal/engine/expression_test.go`: `pair.textDifference` and its rules, nine kinds of excuse, each named after the sentence of the notes that it checks; `pair.textTooLong` and `textTooLongWithin`; `pair.reachOfText` and `textEdge`; `reachArithmetic` names a site of text "text"; `reachServed` counts the constants of text apart; `literalOf` reads text, so that the model's access of a select with a constant of text is checked (it was skipped). `TestPair_text` runs each recorded statement of the notes through the pair and requires its excuse, and requires none where the cause is missing.
- `/internal/engine/differential_test.go`: `pair.step` asks `textDifference` before a difference fails; `pair.bothWays` judges a select whose text is too long by one path and not by the other, and a statement whose subquery's text is too long by one direction and not by the other, by `textTooLong`; `sequencesOf` doubles `textfunctions`. `/internal/engine/subquery_test.go`: the candidates of a subquery that SQLite fails alone with `string or blob too big` are SQLite's rows without the limit, where the subquery has no ORDER BY. `/internal/engine/simulation_test.go`: cells of 8 and 12 seeds of `textfunctions`, which the test requires of every profile.
- Recounted the four `.tsv` files; only the rows of `mixed`, of `textfunctions`, and of the environments of the simulation changed. Appended 110 kinds of text to `/internal/engine/testdata/reached/TestDifferential.kinds` at a third of their totals. Moved "insert with two faults: a value of another type" from `TestGenerated.kinds` to the kinds that `TestGenerated` requires once.

### Why

Each excuse names one sentence of `/docs/todo/text-functions.md` (or of `/docs/todo/sql-front-end.md` for the upper bound of a search) and checks its cause with SQLite: the text that SQLite finds too long alone, the subquery that SQLite fails alone, the condition that SQLite's propagation turns into a constant, the rows of the engine's access as the model chooses them.

### What worked

- No sequence of 1,000 of `textfunctions` and 800 of `mixed` found a value of s4ql that differs from SQLite's for a row that both evaluate.
- The driver check and the simulation met text in `mixed` and in the new profile without a new rule: they run without a lowered length limit, and the shared rules type text already.

### What didn't work

- `sqlite3_changes` keeps the count of the last write through a select, so `textDifference` first never matched a select: the check of "SQLite changed no row" read the count of an earlier insert. It reads the count of changes for writes only now.
- `pair.rows` called `t.Fatalf` on `string or blob too big` for the guard of `pair.due` (`"select 1 from \"TiES\" where ... \"été\" = lower(?) ... and 0 limit 1": SQLite gives 18/18: string or blob too big`). It gives no row there now; the rules of text judge such a statement.
- Measured in the pinned shell, `.param set ?3 "printf('%.1000c', 'f')"` under `.limit length 1000` leaves `?3` NULL, since the shell's own table of parameters cannot hold a record that long, so my first measurements of propagation said SQLite does not propagate. With 900 bytes it does.
- The notes say `where u = v and u = lower(? || ?)` fails in SQLite on an empty table; the shell gives no error for it, and `TestPair_text` uses `lower(u) = 'a' and u = lower(? || ?)`, which fails.

### What I learned

- SQLite computes a constant without a call that serves its search before the search (`where s = ? || ?` fails on an empty table), but codes the value of a term that may serve a search in place: the upper bound of a search only when a row reaches it, and a term of another index than the one that SQLite searches only on the rows it reads (`select k from q where v is null and id = ? || ?` with `v` NOT NULL and indexed gives no row). The engine computes both before the search. This is the case of the recorded "a constant of text with a call that may serve the access fails before any row" without a call, and it is new to the notes.
- SQLite propagates `u = 'q'` into another term only where that term then has no column at all (`propagateConstants`, `constInsert`), and only in a select.

### What was tricky

- The other instance of the pair reads in other directions and by forced paths, so whether a text is too long depends on the rows that it reads before the first row of a subquery or before the end of an access; each such difference is judged by SQLite's text of a row that the rule may evaluate, as an evaluation error of arithmetic is.

### What warrants review

- `pair.textDifference` in `/internal/engine/expression_test.go`, rule by rule, against the sentences of the notes that each names, and `TestPair_text`.
- `excusedTextOrder` and `excusedTextAccess`, the two general cases of "the rows that a statement evaluates are those of its access": each requires that SQLite find the text too long on a row of the engine's access, as the model chooses it, or only outside it.

### Future work

The long runs, the documents, and the review.
