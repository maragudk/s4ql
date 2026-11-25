# Diary: fixes after the review of the engine's checks of arithmetic

An independent review of the engine's generated checks of arithmetic (branch `arithmetic-checks`) found six gaps in the tests of `/internal/engine/` and in the rule `stmttest.Justify`: a write that reads rows outside its access was judged by nothing, a failed statement in a transaction was not read back, the order of the values of SET was pinned by no test, one reach kind counted more than its name said and `pair.due` never judged a select with LIMIT, a message of the simulation was wrong, and the README of the engine had a wrong list and lacked the counts of the planted bugs. The work is on the branch `arithmetic-checks-engine-fix`.

## Step 1: the six findings, the planted faults, the recount

**Author:** builder (arithmetic-checks-engine-fix)

### Prompt Context

**Verbatim prompt:** "You are a builder in s4ql (`maragu.dev/s4ql`), a Go SQL database with its own SQL engine checked against SQLite. Your task is small and bounded: close the gaps that an independent review found in the **engine's generated checks for arithmetic** (`internal/engine/` tests, and the rules they use in `internal/sql/stmt/stmttest/arithmetic.go`). Do nothing beyond the list below. [...] 1. **A write that reads rows outside its bounds is judged by nothing.** [...] 2. **A failed statement in a transaction is not read back in `TestConn_differential`.** [...] 3. **The order in which the values of `set` are computed is pinned by no test.** [...] 4. **One reach kind counts more than its name says.** [...] 5. **`simulation_test.go`, `run.arithmetic`:** when `ee == nil` the message says \"in a read-only transaction\" even outside one. [...] 6. **`internal/engine/README.md`:** [...] If a tightened rule shows s4ql doing something it should not [...] that is a fault in s4ql: fix it if it is small, record it, and report it plainly. Do not loosen the rule to hide it."
**Interpretation:** tighten the engine's judges where the review found them blind, prove each with the named planted fault without `-short`, keep every exported signature of `stmttest` and every digest, recount, and record.
**Inferred intent:** a fault in how a write reads its rows, or in how a transaction takes back a failed statement, or in the order of SET, must fail a check that judges, and not only move a pinned count.

### What I did

- `/internal/sql/stmt/stmttest/arithmetic.go`: `JustifyWithin`, which is `Justify` with the conditions of the range of the access, added to the guard of a site of the statement's own WHERE (not SET, not a subquery). `Justify` calls it with nil, so the simulation and the driver check are unchanged. `TestJustifyWithin` in `arithmetic_test.go` (red first: the offsets of my first draft were wrong, then green). Commit `d11b0f4`.
- `/internal/engine/expression_test.go`: `pair.accessRange` (the core of `pair.modelGuard`, which keeps its restriction to a select without subquery or LIMIT), `pair.writeGuard` (an update or a delete without a subquery, an instance that does not read every row: the range of `accessRange` of `stmttest.SelectOf`), and `pair.justify` passes it to `JustifyWithin`.
- `/internal/engine/tx_test.go`: after a statement in a write transaction that fails with an evaluation error, `txDifferential` reads its table back through the connection and compares it with SQLite's; the profile `arithmetic` runs 80 sequences, not 40. `pair.changedBefore` in `expression_test.go` counts an update that changed a row before the row that failed (by SQLite: the first row of its WHERE in the order of the key shows no quirk of SET). Commit `26fc292`.
- `TestEngine_arithmetic`: a subtest of six updates with two failing assignments each, and in the last subtest an update in a transaction that fails on its last row and must leave the rows before it. Commit `8ec0ab0`.
- `pair.due` takes `everyRow` (`readEvery`: a select with LIMIT through Query that gave fewer rows than its LIMIT) and then judges it by `accessRange`; the reach kind "select with LIMIT that gave rows where a row that it did not read fails" uses `accessRange` as its guard. Commit `f1cefd9`.
- `/internal/engine/simulation_test.go`: `run.arithmetic` says "which is no evaluation error of the evaluator" for a nil `ee`. `/internal/engine/README.md`: the list of `writeIn`, the sentence on `nullDefault` in the same paragraph, which `tx.go` does give `buildSink`, the counts and floors of `TestDifferential_plantedBugs`, and the new rules. Commit `4006b52`.
- Recount: new required kinds in `/internal/engine/testdata/reached/TestDifferential.kinds` and `TestConn_differential.kinds`, the floors of the arithmetic block of the latter raised to a third of the run with 80 sequences (scratch `floors.py`), and `S4QL_RECOUNT=1 go test -p 2 -count=1 ./internal/engine/ -run '^TestDifferential$|^TestConn_differential$'`. Commit `df7930a`.
- Docs: `/docs/todo/arithmetic.md` "Fixes after the review of the engine's checks", `/docs/todo/generated-workload.md` the lenient judges rewritten to what is left, and two new open items. Commit `3028107`.
- Self-review with the code-review skill, and the stale comment before `pair.checkAccess` corrected. Commit `4808643`.

### Why

A judge that only a pinned count backs lets a fault pass as soon as someone recounts. Each finding was a fault that moved a count and failed no rule.

### What worked

- Finding 1: the planted fault (in `build.change` of `operator.go`, `s.access = access{}` where a condition is computed for each row) fails `go test -p 2 ./internal/engine/ -run 'TestDifferential$'` with two statements, such as a delete with `"Ω" > 10 / (Ω - - 1)`: `the engine gives "s4ql: division by zero in / (at offset 70)", which SQLite's result of its operator justifies it for no row that the rule may evaluate`. With the guard turned off, only `arithmetic gives "excused: evaluation error that SQLite's result of its operator justifies" 138 times, and testdata/reached/TestDifferential.tsv pins 136` failed, as the review said.
- Finding 2: with 80 sequences, the planted release of the savepoint after an evaluation error in `Conn.writeIn` fails `TestConn_differential` with "after its evaluation error in the transaction, the table holds". The new hand case of `TestEngine_arithmetic` fails on it too: `rows ["1,-3" "2,-10" "3,-30" "4,4"]` for `["1,11" "2,20" "3,30" "4,4"]`.
- Finding 3: reversing the loop of `update.newRow` fails the new subtest: `Expected "s4ql: division by zero in / (at offset 17)", but got "s4ql: integer overflow in + (at offset 28)"`.
- The tightened rules found no fault of s4ql: 55 errors of writes in `TestDifferential` and 49 in `TestConn_differential` are judged by the access, and 116 and 147 selects with LIMIT by `pair.due`, and all pass.
- `go test -p 2 -count=1 ./internal/engine/` (224 s), `make test`, `make lint`, the new tests with `-race -count=20`, and the differential tests once under `-race` pass.

### What didn't work

- The first try of finding 2 failed to prove anything: with the read back in place and the fault planted, `TestConn_differential` passed, but for `arithmetic gives "excused: evaluation error that SQLite's result of its operator justifies" 76 times, and testdata/reached/TestConn_differential.tsv pins 75`. A scratch test (`TestZZScratchTx`, deleted) ran `txDifferential` on 300 seeds of `arithmetic` with the fault: `3 of 300 fail`, at the seed indexes 41, 68, and 123, all above the 40 that the test ran. An update whose SET fails on a later row than the first is that rare. 80 sequences take in 41 and 68.
- The Python one-liner that edited `expression_test.go` with a heredoc was refused by the harness ("this command is too complex to verify that it stays inside the worktree"); the Edit tool did the same edits. The scratch test had to be written with the Write tool too.
- A grep for counts with `"$k[^:]*"` in zsh failed with `bad math expression: operand expected at '^:'`; a small Python script (`kc.py` in the scratchpad) read the tallies.
- My first `TestJustifyWithin` used the wrong offsets (22 and 53 for 20 and 56), so it gave "is at no operator of the statement".

### What I learned

- A write's access is the model's access of the select of its WHERE without ORDER BY and LIMIT; `stmttest.SelectOf` gives that select with its parameters renumbered, and `pair.reducedSelect` computes its constants in SQLite, so no engine call is needed.
- A select with LIMIT that gives fewer rows than its LIMIT read every row of its access, whatever its direction, so its due errors are decidable without a model of the order.
- Only a pinned count moved for the fault of finding 2 because no sequence before seed index 41 had an update that changed a row and then failed; the end of the transaction compares the state too, and would have found it in such a sequence.

### What was tricky

- `-scanall` makes the instance read every row, so `pair.writeGuard` gives no guard there; the other instance of the pair reads a write with arithmetic by the rules, and `bothWays` justifies only a select of it, so the guard is never given to a forced path.
- The read back queries the instance once more after each such error, which draws a direction from its seeded host; the later draws of those sequences move, which the recount covers.

### What warrants review

- `pair.writeGuard` and `stmttest.JustifyWithin`: whether the model's range is exactly what the engine reads for every write without a subquery. The full runs say yes; a write with a subquery stays lenient.
- `readEvery`: a select run through Exec gives no rows, so it is left out.
- The floors raised in the arithmetic block of `TestConn_differential.kinds`.

### Future work

- The open items now in `/docs/todo/generated-workload.md`: a generated check of the order of SET, the rare update that changed rows before its error, the due errors of a select with LIMIT that gave as many rows, and `pair.due` of a write, which `pair.writeGuard` could serve.

## Step 2: the code review

**Author:** builder (arithmetic-checks-engine-fix)

### Prompt Context

**Verbatim prompt:** (the brief of step 1) "`make test` and the lint target pass; the whole engine package passes without `-short` (`go test ./internal/engine/`); new tests pass with `-count=20` and `-race`."
**Interpretation:** review the diff with the code-review skill, fix what is in the scope, and report the rest.
**Inferred intent:** hand back a branch whose checks are honest, with what lies outside the scope named.

### What I did

- Ran the code-review skill at the medium level over `main...HEAD`, which holds the commits of the branch `arithmetic-checks` too. It gave six findings, none in the code of this branch.
- Fixed one: the comment before `pair.checkAccess` in `/internal/engine/differential_test.go` said that the access of a select with arithmetic was not checked, which it has been since the generated checks.
- Left five for the lead, as outside this task: `pair.checkArithmetic` returns no rule where SQLite does not prepare an update or a delete with arithmetic and two subqueries of `*` of several columns, which the engine refuses with "second subquery of *" (a claimed false failure, not seen in any run); two in the driver check (`openRows` running a select with arithmetic again in another direction, and `checkEvaluation` passing no computed subqueries to `Justify`), whose directory another builder owns; `Justify` accepting a constant of a subquery with LIMIT 0, which `pair.due` says is never computed; and `failingRow` of `Sequence` set by a subquery's WHERE, which would change the generator and its digests.

### Why

The brief forbids touching the driver check and the generator's output, and asks for nothing beyond its list.

### What worked

- `make lint` gives 0 issues; `make test` exits 0.

### What didn't work

- Nothing failed in this step.

### What I learned

- A review of `main...HEAD` on a stacked branch reviews the branch below too; `arithmetic-checks...HEAD` is the diff of this branch alone.

### What was tricky

- Telling the findings on this branch's code from those on the branch below it.

### What warrants review

- The five findings left, above.

### Future work

- The five findings left, for the lead to assign.
