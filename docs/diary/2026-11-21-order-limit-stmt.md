# Diary: Slice 13 of the roadmap in the statement pass, `order by` and `limit` on `select`

Slice 13 lets a select have ORDER BY with one or more columns, each ASC or DESC, and LIMIT with a number or `?`, because goqite picks the next message with `order by priority desc, created limit 1`. This task is the statement pass, `internal/sql/stmt`, and its generator `stmttest`; the engine refuses such a select as not supported, with the error that the pass gave it before, until its builder comes. The work is on the branch `order-limit-stmt`, from `c7ce0d4` on `main`.

## Step 1: SQLite's behaviour, the fields, the recognizer, the checks, the generator, and the refusal in the engine

**Author:** order-limit-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Task: the statement pass of slice 13. Only `internal/sql/stmt` and its test helper `internal/sql/stmt/stmttest`. [...] 1. `stmt.Select` carries the ordering (the columns in the order of the text, each with its name and its direction) and the limit (absent, a literal number, or a parameter), shaped like its neighbours, with spans where the neighbours carry them. Parameters are numbered in the order of the text: those of `where` before the one of `limit`. 2. The rule of the package holds without exception [...] Find out from SQLite itself, not from memory [...] 3. For the engine builder after you, measure in SQLite and record [...] 4. `stmttest`: the `Generator` writes `order by` and `limit`, valid and invalid, behind an option that is off by default [...] show that with the option off the generator's output is byte-identical to before [...] the engine answers 'not supported' for a `select` with an ordering or a limit, at one place, with a test that pins it [...]"
**Interpretation:** Read ORDER BY and LIMIT into `stmt.Select` in the shapes the package already has, give every neighbouring form a name and an offset that SQLite's behaviour justifies, measure what the engine needs, generate the new forms behind an option, and refuse them in the engine with the old error.
**Inferred intent:** The front half of slice 13 with the trust of slices 1 to 12, so that the engine's builder has the facts, the fields, and the generated checks ready, and only has to remove a refusal.

### What I did

SQLite first, with the shell built from `/internal/sqlite/c/shell.c`, `/internal/sqlite/c/sqlite3.c`, and `/internal/sqlite/options.h` into the scratchpad, and scripts written there with the Write tool, and a throwaway test of the binding for the arguments of `limit ?` (deleted). Every form of the brief, and the plans of goqite's receive; the results are in `/docs/todo/slice-13.md`.

`/internal/sql/stmt/stmt.go`: `Select.OrderBy []OrderColumn` (`Name`, `Descending`, as `IndexColumn`), `Select.Limit Term` (the zero Term for none), `Select.Limited`, and `Select.Parameters` counting the `?` of LIMIT.

`/internal/sql/stmt/recognize.go`: `recognizer.orderBy`, `recognizer.orderColumn` (shaped like `recognizer.indexColumn`), `recognizer.limit`, and `recognizer.limitCount`, called by `selectCmd` after WHERE; the `?` of LIMIT is numbered after those of the conditions. Five new names in the README's table: `ordering term other than a column`, `LIMIT other than an integer`, `negative LIMIT`, `LIMIT of an expression`, and `OFFSET`.

Tests. `/internal/sql/stmt/stmt_test.go`: `canonical` writes ORDER BY and LIMIT (`canonicalOrder`); `checkSelect` sends a select with ORDER BY or LIMIT to `checkWhere`, which now puts the values of `orderValues` (NULL and values of every type, two that compare equal) in a column of ORDER BY that no condition tests, 30 rows at least so that rows tie, binds `limitArg` to a `?` of LIMIT, and calls the new `checkOrder`: the canonical text with `*` reads its rows in the order of ORDER BY, each column in its direction, as the new `compareValues` orders values (NULL, numbers exactly by `big.Float`, texts and blobs by bytes); with LIMIT, as many as it says and the first ones of the text without LIMIT by their values in ORDER BY; and the text's rows are those rows with the select list. `checkWhere` also requires SQLite's `no such column` for the first column of ORDER BY that neither the select list nor WHERE names, on a table without it and on one without all of them. `checkPositions` checks the offsets of the columns of ORDER BY, that no two are one name, the span of LIMIT, the number of its `?`, and `Parameters`. `anyTable.rows` is split into `anyTable.pools` and `whereRows`.

`/internal/sql/stmt/check_test.go`: six old cases have new answers, five of them supported now; 78 new cases, 8 supported (both selects of the roadmap, goqite's receive in full, comments in every gap, `limit 0`, the largest limit, a keyword as a column, and a WHERE of every form of slices 7 and 12 before ORDER BY and LIMIT) and 70 not supported or syntax errors; and four subtests: the keywords that SQLite reads as a column in ORDER BY (84, the same in four places and the same as those of the select list), the order of a column of each type of a strict table, LIMIT with each kind of argument through the binding, and the order of SQLite's errors for ORDER BY. `/internal/sql/stmt/recognize_internal_test.go`: the guard of `column named twice` on the next token. `/internal/sql/stmt/vocabulary_test.go`: the allow list has the fields of ORDER BY and LIMIT. `/internal/sql/stmt/generate_test.go`: `newGenerator` sets `OrderBy` to 3, `difference` tells ordering columns, direction, and limit apart, and the near misses of a select have 54 new forms in place of five that are now supported.

`/internal/sql/stmt/stmttest/stmttest.go`: `Generator.OrderBy` and the hook `Generator.Order`, written by `Generator.order` after WHERE, and `Generator.limitValue`. `TestGenerator_OrderBy` in `/internal/sql/stmt/stmttest/stmttest_test.go`.

The engine: `orderLimitRefusal` in `/internal/engine/orderby.go`, called once in `check` in `/internal/engine/engine.go`, gives `ORDER BY clause` at the ORDER, or `LIMIT clause` at the LIMIT, the last token of its kind before the first column or the number; `TestCheck_orderLimit` in `/internal/engine/orderby_test.go` pins it, and `orderedOrLimited` there is the one exception of the differential tests in `/internal/engine/differential_test.go`, whose fuzz seeds from the corpus have ORDER BY. The driver: `TestPrepare` in `/conn_test.go` refuses `group by` after WHERE at its prepare in place of `order by`, since `Prepare` calls `stmt.Check` alone.

### Why

The shapes: `OrderColumn` is `IndexColumn` by another name, and `Limit` is a `Term` because a Term is what the package gives for "a literal or a parameter, with its span"; the zero Term stands for none, as for a test for NULL, and no statement holds a pointer. LIMIT takes only a non-negative integer of 64 bits by the rule of exact types: SQLite converts a real or a text that holds an integer when it runs, refuses every other value with `datatype mismatch`, and reads a negative number as no limit, none of which a reader of `limit -1` or `limit '5'` would guess. A column named twice in ORDER BY is refused as in the three other lists of columns, since the later one orders nothing. `checkOrder` checks the meaning against an order computed in Go, not only against the canonical text, because the canonical text and the text agree also when both read the direction wrong.

### What worked

- Every new case passed its check against SQLite on its first full run but one, whose offset I had counted wrong (36, not 35).
- With `OrderBy` 0 the generator is unchanged: a dump of 73,800 lines (`%#v` of `Generator.Statement` and `Generator.CallOf` of every kind in six settings, three with hooks, and of `Sequence.Next` with and without `Indexed`, over 30 seeds), written with the generator before the change and after it, was the same (`cmp before.txt after.txt && echo SAME`).
- `go test ./internal/sql/stmt/...` passed (59 s), `go test -short ./internal/engine/... .` passed, and `golangci-lint run` on the three packages gave 0 issues.

### What didn't work

- `go test -short ./...` first failed in two places outside the pass. `TestPrepare` in `/conn_test.go`: "conn_test.go:422: Not true ... no error", since the driver's `Prepare` now takes `order by`; and `FuzzSQL` and `FuzzSQL_core` of the engine: `"SELECT a FROM t1 WHERE r < 65536 ORDER BY x": the engine does not support a statement that the pass supports: s4ql: not supported: ORDER BY clause (at offset 33)`, from seeds of the corpus. Fixed as described above.
- `TestGenerate_mutants` and `TestGenerate_nearMisses` failed on their pinned numbers: "41 texts are syntax errors where SQLite does not say [...] but the test pins 27", "131 [...] pins 150", and "2089 [...] pins 1979". A throwaway tally by SQLite's message, hooked into `verify` and removed, showed create tables only and one select with ON in place of WHERE; with `OrderBy` 0 the numbers were 27, 150, and 1,979 again, so they moved by the generator's draws. They are pinned at the new values.
- Bash here-docs that ran Python or wrote SQL were refused: "this command is too complex to verify that it stays inside the worktree". Scripts written to the scratchpad with the Write tool and run as `python3 -I script.py <file>` or `./sqlite3 :memory: < script.sql` worked.
- My first measurement of positions read `select a, k from t order by 2` on a table whose second column is the key, so every form looked like a position; it is `select k, a` now.
- The strict TEXT column of the subtest of types took the integer 1, converting it to `'1'`; it is refused a blob now.

### What I learned

SQLite reads an ORDER BY term as a position only for an integer that fits 32 bits, after any number of signs, parentheses, and COLLATE (`sqlite3ExprIsInteger` on the term without its COLLATE), so `order by 2147483648` is a constant. SQLite reads `offset` after a table as its alias, so `select * from t offset 2` is a syntax error near the `2`. A LIMIT that is not an integer is refused by OP_MustBeInt when the statement runs, on an empty table too, after every error of names.

### What was tricky

- Keeping the meaning check sound for ties: SQLite gives tied rows in the order it reads them, the same for the text and for the canonical text with `*`, so the text's rows are compared with those of the canonical text row for row, and those with LIMIT with the text without LIMIT only by their values in ORDER BY.
- The `?` of LIMIT is the last parameter, so the text without LIMIT is run with one argument fewer, and the check that a text has as many parameters as the statement says now compares with the arguments that each text is run with.

### What warrants review

- `recognizer.orderColumn`, `recognizer.limit`, and `recognizer.limitCount` in `/internal/sql/stmt/recognize.go`, and the names and offsets of the new rejects.
- The doc comments of `stmt.Select.OrderBy` and `stmt.Select.Limit`, which are the contract for the engine's builder.
- `checkOrder` and `compareValues` in `/internal/sql/stmt/stmt_test.go`.
- The change of `TestPrepare` in `/conn_test.go` and the exception `orderedOrLimited` in the engine's differential tests, both outside the pass.

### Future work

- The README of the pass beyond its five names, and the checks of the branch: step 2.
- The engine and the driver for slice 13: remove `orderLimitRefusal` and `orderedOrLimited`, give `TestPrepare` back its `order by` if wanted, set `Generator.OrderBy` in their generated checks and `Generator.Order` in the hooks of `Sequence`, vary the rows that tie, and decide the refusal of an argument of LIMIT that is not an int64 from 0.
