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
