# Diary: Slice 12 of the roadmap in the statement pass, a value on the left of a comparison

Slice 12 lets a comparison of a WHERE have the literal or `?` on the left and the column on the right, as in `? >= timeout` and `5 < a`, because goqite, the queue that glue uses, writes its receive query both ways round. This task is the statement pass, `internal/sql/stmt`, and its generator `stmttest`; the engine refuses such a condition as not supported, with the error that the pass gave it before, until its builder comes. The work is on the branch `value-left-stmt`, from `e640ef5` on `main`.

## Step 1: SQLite's behaviour, the condition, the recognizer, the checks, the generator, and the refusal in the engine

**Author:** value-left-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Task: the statement pass of slice 12. Only `internal/sql/stmt` and its test helper `internal/sql/stmt/stmttest`. [...] 1. `stmt.Condition` [...] can say which side the column is on, in a way that makes the wrong reading hard: decide between keeping the condition normalised as written (a field that says the value is on the left, with the operator as written) and normalising it at recognition to column-operator-value with the operator turned around (`5 < a` becomes `a > 5`). [...] Choose, give the reason in the code and the diary, and keep the text's own order recoverable for messages and spans. 2. The rule of the package holds without exception [...] Find out from SQLite itself, not from memory: every operator slice 7 has, turned around [...], with each kind of literal on the left [...], with `?` on the left; a literal `null` on the left [...]; `5 is null`, `null is null`, `? is null`, `5 is not null` [...]; a column on both sides [...]; a value on both sides [...]; a parenthesised side; a double-quoted word on the left or right [...]; a bare word that is a keyword; conditions of both orders joined by `and` in one `where`, in `select`, `update`, and `delete`. 3. For the engine builder after you, measure in SQLite and record [...] 4. `stmttest`: the `Generator` writes conditions with the value on the left, valid and invalid, behind an option that is off by default [...] show that with the option off the generator's output is byte-identical to before [...] the engine answers 'not supported' for a condition with the value on the left, at one place, with a test that pins it [...]"
**Interpretation:** Read `value op column` into the condition that the rest of s4ql already runs, keep the text's order in the condition, give every neighbouring form a name and an offset that SQLite's behaviour justifies, measure the facts that the engine needs, generate the new form behind `Generator.ValueFirst`, and refuse it in the engine with the old error.
**Inferred intent:** The front half of slice 12 with the same trust as slices 1 to 11, so that the engine's builder has little more to do than remove a refusal and turn the generated checks on.

### What I did

SQLite first, with the shell built from `/internal/sqlite/c/shell.c` and `/internal/sqlite/options.h` into the scratchpad, and scripts written there with the Write tool:

- Every operator turned around (`=`, `==`, `<>`, `!=`, `<`, `<=`, `>`, `>=`), with a column on the right, on a strict table with a column of each type and on one that is not strict: 1,440 pairs of `value op column` and `column op' value`, 20 values from NULL to `x'35'`, gave the same rows every time. `EXPLAIN QUERY PLAN` shows that SQLite turns the comparison round itself: `5 < k` is `SEARCH t USING PRIMARY KEY (k>?)`, `5 >= a and a > 1` is `SEARCH t USING INDEX i (a>? AND a<?)`.
- SQLite parses every form of the brief: `0x5 = a`, `true = a`, `1_0 = a`, `current_time = a`, `-null = a`, `- -5 = a`, `-'x' = a`, `-? = a`, `5 is null`, `null is null`, `? is null`, `5 is not null`, `5 is a`, `5 is distinct from a`, `a = b`, `a = a`, `1 = 1`, `? = ?`, `1 = ?`, `(5) < a`, `5 < (a)`, `5 < "a"`, `5 < t.a`, `5 < abort`, `5 < true`. Syntax errors: `5 < select`, `5 <`, `< a`. `null = a` gives no rows, as `a = null` does.
- The order of the errors, under the length limit of 1,000: the names come first in the order of the text, also after a value on the left that is too long (`'<1200 bytes>' < a and 1 = nope` gives `no such column: nope`); the walk past a test for NULL of a missing column is as in slice 7 (`nope is null and nope2 is null and 1 = nope3` names `nope2`); then `string or blob too big`. A missing column in double quotes on the right gives `no such column: "nope" - should this be a string literal in single-quotes?`, as on the left.

The decision on the form, in `/internal/sql/stmt/stmt.go`: a condition is normalised at recognition. `5 < a` is `Condition{Column: a, Operator: OpGreater, Value: 5, ValueFirst: true}`. `Operator.Swapped` turns an operator round (`<` and `>`, `<=` and `>=`, `=` and `!=` themselves, and no operator for a test for NULL).

`/internal/sql/stmt/recognize.go`: `recognizer.condition` reads a comparison whose left side is a value as `isValue` says (a literal or a `?`, with at most one sign, the forms that slice 7 named `value on the left of a comparison`) with `recognizer.term`, the reader of the value on the right, then the operator, then the column; `recognizer.leftColumn` is now `recognizer.conditionColumn`, which reads the column on either side and rejects a value there under the name its caller gives: `value compared with a value` on the right, `value before IS` before IS. `depth` counts a sign on either side. The name `value on the left of a comparison` is gone from the pass; the README's table has the two new names.

Tests. `/internal/sql/stmt/check_test.go`: the eight old cases of a value on the left are gone, and there are 15 supported cases (every operator, every type, the limits of 64 bits, parameters on both sides numbered in the order of the text, goqite's receive as far as slice 12 takes it, comments in every gap, an update, a delete, both orders in one WHERE) and 60 not supported or syntax errors. Three subtests: the keywords that SQLite reads as a column on the right are those it reads on the left (84), also in an update and a delete; "should read a value on the left as SQLite does", the 2,880 selects of the measurement above with literals and `?`; and "should give SQLite's errors for a value on the left in the order of the other side", 18 texts. The test of the depth limit has `1 = v` (999) and `-1 = v` (998). `/internal/sql/stmt/stmt_test.go`: `checkConditions` checks the order of value and column by `ValueFirst`; the canonical text writes every condition with its column first, so `checkWhere` and `checkChange` compare the rows of the text with those of the condition turned round. `/internal/sql/stmt/generate_test.go`: `newGenerator` sets `ValueFirst` to 3, `difference` tells `side of a column`, and the near misses have 30 new forms in place of four that are now supported. `FuzzLiteral` in `/internal/sql/stmt/real_test.go` puts its literal on the left too.

`/internal/sql/stmt/stmttest/stmttest.go`: `Generator.ValueFirst` writes one comparison in ValueFirst, at random or from a hook, with its value first and the operator swapped in any spelling, and a hook's condition with `ValueFirst` set always so; `Generator.spelling` picks a spelling. `TestGenerator_ValueFirst` in `/internal/sql/stmt/stmttest/stmttest_test.go`.

The engine: `valueFirstRefusal` in `/internal/engine/valuefirst.go`, called once in `check` in `/internal/engine/engine.go`, gives `value on the left of a comparison` at the start of the value of the first such condition; `TestCheck_valueFirst` in `/internal/engine/valuefirst_test.go` pins it. `/internal/sql/mutation/equivalent.txt`: the equivalent mutant of `recognizer.leftColumn` is now of `recognizer.conditionColumn`; its reason holds on the right too.

### Why

Normalised, not as written: every reader of a condition that runs it, the engine's fitting of conditions to the key and to indexes (`access.go`), its filter (`operator.go`), its rule of exact types, and `stmttest`'s rules and expected rows (`rule.go`), reads column, operator, value today. With the operator as written and a flag, each of them would have to turn the operator round, and one that forgot would give wrong rows without an error. Normalised, one that ignores the flag still gives the right rows, which is what SQLite does too (its plan shows `k>?` for `5 < k`). What the normalised form loses is only the text's order, and that is kept: `ValueFirst`, `Column.Offset`, and `Value.Span`, and `Operator.Swapped` gives the operator as written. Parameters are numbered by `recognizer.term` in the order of the text, whichever side the value is on. A plan or a golden file shows the condition with the column first.

### What worked

- Every new case passed its check against SQLite on the first full run, the meaning checks included. Two planted faults showed that the checks bite: the recognizer and the generator both without the swap made `TestGenerate` fail on the rows (`generate_test.go:83: Expected "null; text ...`), and the measurement with the operator not swapped failed at `int strict, without rowid: select k from c where 0 < v order by k`.
- A dump of 81,000 lines, statements and calls of `Generator` of every kind in eight settings, three of them with hooks, and of `Sequence`, over 30 seeds, written before the change and after it with ValueFirst 0, was the same byte for byte once the 19,301 fields `, ValueFirst:false` of `%#v` were stripped, with no `ValueFirst` left (`cmp old.txt new2.txt && echo SAME`).
- `go test -count=1 ./internal/sql/stmt/...` passed (56 s), and `TestEquivalent` of `/internal/sql/mutation`.

### What didn't work

- A Bash heredoc that ran a Python edit of `check_test.go` was refused: "this command is too complex to verify that it stays inside the worktree". The same script written to the scratchpad with the Write tool and run as `python3 -I script.py <file>` worked.
- My first measurement script put the value's text in a label of the select, and `'5'` broke the quoting (`near "5": syntax error`); the label now holds the index of the value.
- The case `update t set a = <long> where 'x' < a` expected `string or blob too big` and got no error: `'x' < 5` is false, text is greater than any number, so no row changed. It is `'x' > a` now.

### What I learned

SQLite applies a column's affinity to the value on either side of a comparison, so the two orders agree on a table that is not strict too, where `'5' = a` converts the text for an INT column. A comparison is resolved by the walker at the node of the operator before its operands, so the stop after a missing column is the same with the value on either side.

### What was tricky

- Keeping the old answers of the forms that slice 12 does not take: `~1 < v`, `- -5 = v`, and `-v = 1` still go through `conditionColumn` and keep `unary operator` at the start, because only what slice 7 called a value on the left is read as one now.
- The pinned numbers moved, because the generator draws more numbers with `ValueFirst` on: `TestGenerate_mutants` from 41 to 27 syntax errors and from 154 to 150 not supported where SQLite does not say, and `TestGenerate_nearMisses` from 2,130 to 1,979. A throwaway tally of the excused texts by SQLite's message, hooked into `verify` and removed before the commit, found only create tables, of the same kinds as before: `table _ has more than one primary key`, `generated columns cannot be part of the PRIMARY KEY`, `default value of column _ is not constant`, `AUTOINCREMENT ...`, `unknown table option`, `duplicate column name`, two `no such collation sequence`, and 12 `non-deterministic functions prohibited in index expressions`. The near misses moved because 253 of the 2,000 statements are create tables, where 272 were.

### What warrants review

- `recognizer.condition` and `recognizer.conditionColumn` in `/internal/sql/stmt/recognize.go`, and the names and offsets of the new rejects.
- The doc comment of `stmt.Condition`, which is the contract for the engine's builder.
- `valueFirstRefusal`: a text with a value on the left and a later token that the pass refuses now gets the later refusal, where `main` named the value.

### Future work

- The README of the pass beyond its two names, `docs/todo.md`, and the checks of the branch: step 2.
- The engine and the driver for slice 12, which remove `valueFirstRefusal` and set `Generator.ValueFirst` and `ValueFirst` in the hooks of `Sequence`.

## Step 2: The decisions, the README, the review, and the checks

**Author:** value-left-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "When you get to recording decisions, `git fetch` and merge `origin/main`: if `docs/todo/` exists, write your group in `docs/todo/slice-12.md` [...] Finish: `make test`, `make lint`, `make check-browser`, and `make fuzz-shards FUZZGROUP=c FUZZSHARDS=4` pass; `make test-asan` only for the packages whose fuzz tests you changed. Self-review against these requirements."
**Interpretation:** Merge the split of the todo, write the group of decisions and the facts for the engine in `docs/todo/slice-12.md`, bring the README of the pass up to date, review the branch, fix what matters, and run every check.
**Inferred intent:** A branch whose code, numbers, and documents agree, ready for the lead and for the builder of the engine.

### What I did

Merged `origin/main` (`6c9cd10`, the split of `docs/todo.md` into `docs/todo/`), wrote `/docs/todo/slice-12.md` with the group "Decisions of the statement pass of slice 12 [...] for the lead to confirm" and the facts of SQLite for the engine, and added it to the list in `/docs/todo.md`.

A writer sub-agent updated `/internal/sql/stmt/README.md`: the typed form of `Condition` and `Operator.Swapped`, `value op column` in the definition of WHERE with the forms that stay not supported, the DELETE example, the depth, "How it is tested", and "Results" from a full run (`go test -count=1 -v ./internal/sql/stmt`, 88 s). I read its diff. It found three counts of step 1 wrong, now corrected there: 60 new not-supported cases, not 61; two excused mutants with `no such collation sequence`, not one; and 12 near misses with `non-deterministic functions prohibited in index expressions` that my list left out. It also found that the corpus has 17 more supported texts (13 SELECT, 1 UPDATE, 3 DELETE with a value on the left), and that `Operator.Swapped` for a test for NULL was not covered; `TestOperator_Swapped` in `/internal/sql/stmt/stmt_test.go` covers every operator and the zero one now.

The review (`code-review high origin/main...HEAD`) found no bug in the pass or the generator, and three points:

- The driver's `Prepare` now takes `select * from t where 1 = v`, which `main` refused, because `check` in `/conn.go` calls `stmt.Check` alone and `valueFirstRefusal` sits in `engine.check`; the first run refuses it. The brief keeps the driver out of this task, so I left it and recorded it in the todo; it is for the lead to decide, and it ends when the engine runs slice 12.
- The engine's refusal now loses to a later refusal of the pass, or to the pass's name for the value itself (`+1 < v` gives `+ sign`). Known and meant: recorded in the todo with examples.
- The name `value on the left of a comparison` that the engine gives is no longer in the README's vocabulary. The doc comment of `valueFirstRefusal` and the todo say so.

### Why

The rule that a moved number needs its reason, and the README as the definition of the forms.

### What worked

`make test`, `make lint` (0 issues), `make check-browser`, and `make fuzz-shards FUZZGROUP=c FUZZSHARDS=4` passed. `make asan-test ASANPKGS="maragu.dev/s4ql/internal/sql/stmt maragu.dev/s4ql/internal/sql/stmt/stmttest"` in the `golang` image passed (47 s and 2 s), since `FuzzLiteral` changed and the seeds of `FuzzCheck` and `FuzzGenerate` changed with the cases and the generator. `FuzzLiteral` ran 20 s and found nothing. Under `-race`, `TestGenerator_ValueFirst` and `TestCheck_valueFirst` passed 20 times, and the new subtests and cases of `TestCheck` 5 times.

### What didn't work

Nothing new.

### What I learned

Nothing beyond step 1.

### What was tricky

Nothing beyond step 1.

### What warrants review

- `/docs/todo/slice-12.md`, above all the choice of the normalised form and the facts for the engine.
- The driver's `Prepare`, which takes a value on the left until the engine runs it.
- The SELECT section and "Results" of `/internal/sql/stmt/README.md`.

### Future work

- The engine and the driver for slice 12: remove `valueFirstRefusal`, set `Generator.ValueFirst` in their generated checks and `ValueFirst` in the hooks of `Sequence`, and check the plans by the key and by indexes with the value on the left.
- Mutation testing and long fuzz runs of the code of slice 12 in the pass.
- The roadmap's slice 12 is unchanged, so the site needs nothing from this step.
