# Diary: Mutation testing and fuzz targets of slice 4, the other types

Slice 4 of the roadmap added the column types `integer`, `real`, `text`, and `blob`, and their literals and arguments, through the statement pass (#69), the format and the core (#68), the engine and the driver (#70), and the generated checks (#71, branch `types-checks`). This task runs the mutation tester `internal/sql/mutation` over the code that slice 4 added or changed, closes each gap that a surviving mutant shows or lists the mutant as equivalent, and adds fuzz targets of the types. The branch is `types-mutation`, from `origin/types-checks`.

## Step 1: The fuzz targets of the types, and the start of the runs

**Author:** types-mutation builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "1. **Mutation testing of the code that slice 4 added or changed**, with the project's mutation tester, in the packages it can run: the statement pass (`internal/sql/stmt`, with the port of SQLite's conversion of reals in `real.go` and the new literals and types in `recognize.go`), the engine (`internal/engine`: `insert.check`, `checkArgument`, `checkLength`, `checkType`, `fromCore`, the typed sink and scan), the storage core (`internal/core`: `checkValue`, `bind`, `readRow`, `typeOf`, `Tx.Upsert`, `Tx.Delete`, `Tx.touch`, `Tx.spoolTouched`), the format's checks (`internal/format/check.go`), and the driver (root package: `argument`, `CheckNamedValue`, `value`). [...] 2. **For each mutant that survives:** either it is equivalent, and goes on the list with the reason, or a test is missing, and you add the test that kills it. [...] Two known gaps from a review of the core, to close here: nothing pins that `bind` stores `-0.0` as `0.0` [...] and only the random savepoint test covers "upsert of a key that existed, then delete it" (add a direct case to `TestTx_types`). 3. **Fuzz targets for the types**, where a target adds something that the generated tests do not reach: at least one that feeds arbitrary bytes as text and blob arguments and as string and blob literals through the engine and compares with SQLite [...] Give each target seeds, and run it for about a minute locally. [...]"
**Interpretation:** Run the tester over the functions of slice 4 in five packages, classify every survivor, close the gaps with tests (strengthening the generated or table-driven ones first), add a fuzz target of arbitrary bytes in every form of value, and update `docs/todo.md`.
**Inferred intent:** The same trust in the types of slice 4 as in slices 1 to 3: every small change of the new code is noticed by some test, or is shown to change nothing.

### What I did

The mutation runs read the files while they run, so they run on a copy of the worktree in the scratchpad (`rsync` without `.git`), with a GOCACHE of their own, and I edit the worktree meanwhile. The run of `stmt` selects `real.go`, `recognizer.literal`, `recognizer.column`, `columnTypes`, `literalName`, and `Type.String` (781 mutants); the run of the engine selects all of `errors.go`, `operator.go`, and `plan.go`, `fromCore` and `coreDB.lengthLimit` in `storage.go`, and `Conn.write`, `Conn.checkOutside`, and `Conn.writeIn`, which now call the sink's `check` (1,010 mutants). Both run with 4 workers at once.

`/internal/engine/differential_test.go` has two new fuzz targets, `FuzzTypes` on the engine's database in memory and `FuzzTypes_core` on the storage core. Each creates a table of each of the five types and gives its input, a `[]byte`, as the value of an insert into each table, in the form that a second input chooses: a text argument, a blob argument, a string literal, a blob literal (in lower or upper case), the bits of a real argument, the bits of an integer argument, or SQL text in the place of the value. Each insert runs twice, so that the second meets the key of the first, a `select *` reads each table, and the tables are compared at the end; all through `pair.step`, so the rules for arguments, types, and lengths are those of `TestDifferential`. `FuzzTypes_core` also takes a length limit (from 301 to 555 bytes, or SQLite's default) and, after the inserts, replays the log into an empty cache and compares the tables again. The seeds are 27 values at the edges of every type (NUL, invalid UTF-8, a lone lead byte, a quote, `1e999`, `-0.0`, the bits of NaN, the infinities, -0.0, the smallest subnormal, the largest float, and `1 << 63`) in each of the seven forms, and values of 295 to 302 bytes at the limit of 301 in four forms.

### Why

The generated checks draw values from small sets of edges. The fuzz targets give the engine, the core, and the replay any bytes in every form, which reaches texts and blobs that no generator writes, and the replay reaches the format's checks with them.

### What worked

A throwaway count with a tally over the seeds of `FuzzTypes_core` showed that the seeds alone reach every outcome per type: ok, `UNIQUE constraint failed`, a value of another type, `string or blob too big` for a value and for a record, -0.0 meeting 0.0 by literal and by argument, NaN, the infinities, text with NUL and not valid UTF-8. A minute of each, with `-parallel 2` while the mutation runs held 8 cores: `FuzzTypes` 17,892 to 26,424 executions in two runs, `FuzzTypes_core` 5,927, no failure. The executions per second dropped to 0 for some seconds now and then; every input of the corpus that the run made takes under 10 ms on its own, so that is the load of the machine.

### What didn't work

- The first run of the seeds failed with `"select v from t_int": the engine does not support a statement of slice 1: s4ql: not supported: result column other than * (at offset 7)`: slice 1 has only `select *`.
- `FuzzTypes_core` with a length limit of 30 + n failed with `open the cache: local I/O failure: sqlite: string or blob too big (code 18)`. `core.Options.LengthLimit` accepts every limit from 30, SQLite's smallest, but the cache's own `create table main.s4ql_cache (...)` is about 200 bytes, which SQLite stores in `sqlite_schema` and refuses above the limit: a limit of 201 fails, 301 opens. The option is used only by tests; the bound is recorded in `docs/todo.md` and the fuzz target uses limits from 301.

### What I learned

The fuzz targets in a directory of `FUZZDIRS` need no list: `make fuzz` reads the names from the test files, and CI's shards take them.

### What was tricky

Nothing so far.

### What warrants review

The forms in `typesCall`, and whether `Supported` is right for each: a string literal is supported only when the text is valid UTF-8 without NUL, and SQL text never is.

### Future work

The results of the mutation runs, the core, the format, and the driver.

## Step 2: The engine, the format, and the tables of `real.go`

**Author:** types-mutation builder

### Prompt Context

**Verbatim prompt:** (from the brief, as in step 1) "**For each mutant that survives:** either it is equivalent, and goes on the list with the reason, or a test is missing, and you add the test that kills it. Prefer strengthening a generated or table-driven test over adding a one-off test."
**Interpretation:** Read the report of each run, and for each survivor decide: equivalent with a reason, or a test that kills it, in the table-driven test of its file where there is one.
**Inferred intent:** Every survivor is explained, and the list of equivalent mutants holds only what no test can tell apart.

### What I did

The run of the engine made 1,010 mutants in 39 min with 4 workers, beside the run of `stmt` and the work of other sessions on the machine (a load average of 24 on 10 cores): 262 did not compile, 701 were killed (673 by the hand-written tests, 1 by the generator, 27 by the simulation), 2 timed out, 45 survived, 10 of them listed. Of the 35 others, the tests that kill them:

- `/internal/engine/engine_test.go`: `TestEngine_tooBig` compares the offset of each error with SQLite's, for `string or blob too big` and for a write in a read-only transaction (-1 for both, which three mutants each of `tooBig` and `readOnlyDatabase` changed). `answer` in `/internal/engine/differential_test.go` has an `offset` for it, which is not in its text, since SQLite does not give the same offset on every connection.
- `/internal/engine/errors_internal_test.go` (new): the internal errors of `checkType` for a column of no type and an expression of no value, and `kindName` of a value of no kind.
- `/internal/engine/operator_internal_test.go`: `TestInsert` checks the type of every row, not only the first, and the four internal errors of `insert.check` with exact messages (the swapped numbers); `TestBuild` has a nil blob literal.
- `/internal/engine/plan_test.go`: `Format` of `1.25`, since every other real of the table has at most one significant digit and precision 0 gave the same text for them.
- `/internal/engine/storage_internal_test.go`: `fromCore` for each refusal of the core, among them `core.ErrReaderClosed`, which no test named, and `core.ErrTooBig` as SQLite's error with its fields.

The equivalent ones, in `/internal/sql/mutation/equivalent.txt`: eleven in the planted bug `convertReal` (other conditions under which it converts a real, and its absence in a transaction that spans statements), one more of `annotateNext` (each argument as an integer), the loop of `checkArguments` (every statement has at most one parameter), the panic of `NewPlan` for a value of another type (`stmt.Value` has an unexported method), the precision -2 of `strconv.FormatFloat`, and the swap of `strings.ContainsAny`, which is symmetric.

The run of the format, all of `checkRow`, `checkPrimaryKey`, `checkStored`, and `checkValue`, made 90 mutants in 23 min: 24 not compiled, 61 killed, 5 survived. Four were tests that looked only at `ErrMalformed`: the reader's and the writer's tables of refusals in `/internal/format/reader_test.go` and `/internal/format/writer_test.go` now have a field `why` that the error must say, for every refused value and for the counts of values. The fifth is equivalent: `GetReal` of a value that is not a real is 0.0 without a sign.

`/internal/sql/mutation/main.go`: `listedPkgs` names `internal/format` and the root too, so that the list can hold their mutants.

The run of `stmt` was slow: each survivor in `real.go` runs every stage, the engine's tests among them. After 304 of 781 mutants, most of the survivors were bounds of `atof` and the 26 entries of `powerOfTenScaleLo` with one more or one less. A throwaway internal test with a copy of `atof` that takes the bounds as functions found, for each bound of the mantissa, a literal on which it gives another float, and for the bound of the exponent, literals of 10,000 digits after the point: `realEdges` in `/internal/sql/stmt/real_test.go` has them, and `TestCheck_reals` compares each, with and without a sign, with SQLite first. For the tables, `/internal/sql/stmt/real_internal_test.go` (new) checks `powerOfTenBase` against the exact powers of 10, `powerOfTenScale` and `powerOfTenScaleLo` against the 96 most significant bits of 10^(27g) rounded to the nearest, and the special entry for 10^-1 cut, and `multiply160` against the exact product with `math/big`, with a case where a carry into the low 32 bits shows. I stopped the run, and ran the mutants that had no result or had survived again on a copy with these tests.

### Why

A ±1 in the last of the 96 bits of a power of 10 changes the float only for a literal within about 2^-96 of a rounding boundary, which no random text reaches and which would need a search like that for the hardest cases of decimal conversion. The tables are data with a rule, and a test of the rule pins every entry.

### What worked

The throwaway copy of `atof` found a literal for each of six bounds of the mantissa in a few hundred thousand random texts around the digits 1844674407370955160, under a second.

### What didn't work

- My first guess of the rule of the tables, the 96 bits cut, failed: `Expected "1376144556", but got "1376144557"` for 1e-324. SQLite rounds them to the nearest, and cuts only its special case for 10^-1 (0xcccccccc, where rounding gives 0xcccccccd).
- A check that `powerOfTen(p)` is within one of the exact bits for every p failed at 1e-305 by -2, so the product of two tables is not that close; I dropped it, as the tables and `multiply160` are pinned on their own.
- The sandbox refused a `python3` heredoc that edited the format's tests ("too complex to verify that it stays inside the worktree"); a script in the scratchpad, run with `python3 -I`, worked.
- `string or blob too big` as a pointer compared with `is.Equal` failed with two equal texts, `Expected "string or blob too big", but got "string or blob too big" (type *sqlerr.Error)`; the test compares the struct.

### What I learned

- The reader of the format cannot reach the check of UTF-8 in `checkValue`: Protocol Buffers refuses `string field contains invalid UTF-8` first. Only the writer reaches it.
- A surviving mutant runs every stage, so in `stmt` each one costs minutes of the engine's tests; closing the gaps and running the rest again is faster than waiting.

### What was tricky

Telling the bounds of `atof` that matter from those that do not: `largest-10` and `largest-8` give the same quotient by 10 as `largest-9`, while `largest+9` is exact in Go's untyped constants and moves the bound by 2.

### What warrants review

- `realEdges`, and the two rules of the tables in `TestPowerOfTen`.
- The new equivalent entries of the planted bugs, and whether a planted bug should be made narrower so that fewer of its mutants survive.

### Future work

The results of the core, the driver, and the second run of `stmt`.

## Step 3: The rest of `stmt`, the core, the driver, and the confirmation runs

**Author:** types-mutation builder

### Prompt Context

**Verbatim prompt:** (from the brief, as in step 1) "Two known gaps from a review of the core, to close here: nothing pins that `bind` stores `-0.0` as `0.0` (SQLite does it anyway, so say whether that mutant is equivalent), and only the random savepoint test covers \"upsert of a key that existed, then delete it\" (add a direct case to `TestTx_types`). [...] If mutation testing or fuzzing finds a real bug or a difference from SQLite: fix it if the fix is small and clearly right, with a test, and describe it prominently in your report."
**Interpretation:** Finish the survivors of `stmt` and the core, run every survivor again on the final tests, and record what stays open.
**Inferred intent:** A list of equivalent mutants that a reviewer can trust, and no survivor left unexplained.

### What I did

`stmt`: the second run, of the 612 mutants that had no result or had survived, on a copy with `realEdges`, `TestPowerOfTen`, and `TestMultiply160`, left 54 survivors. For the 44 in `real.go` I wrote a throwaway test in `internal/sql/mutation` that, for each surviving key, writes a small module with the original `real.go` and the mutated one, each with an exported `Atof`, and `go run`s a search for a literal on which they differ. Four searches: random texts; exact floats and midpoints that have short decimal forms; mantissas solved, with a modular inverse, so that their product with the top 64 bits of 10^p has its bits 64 to 64+k near zero; and the same solved for a tie of the rounding with a few units in the 32 bits after it. The last found ties that kill `pwr10l != 0 => true`, `pwr10l != 0 => pwr10l != 1`, `mid1-mid2 > 1 => > 2`, and the two mutants of `n == 0` in `powerOfTen`; the earlier ones killed `u64Bit(s)-1 => -2` and the special case of 10^-1. 2^54 - 1 kills the two bounds of `u >= u64Bit(55)-2`, which I found by reasoning, not by search. Every literal is in `realEdges` and agrees with SQLite. The remaining ones are in `equivalent.txt`, each with its reason; the bounds of p and e have arithmetic in their reasons (for example, for p below -348, `pwr10to2` gives at most -1160, so e is at least 1149 and the result is 0 before the power is looked up). In `recognize.go`: `TestCheck` has `t.true` and `-t.false`, and `TestRecognize` gives the recognizer a blob token of an odd number of hex digits, which pins the panic and its message; the tests and not `recognize.go` changed, as the brief asks.

The core: the run of 628 mutants took 2 h with `-factor 3` and left 93 survivors. New tests: `TestBind` in `/internal/core/cache_test.go`, with `core.Bind` in `/internal/core/export_test.go`, binds every kind directly, among them -0.0, which SQLite keeps for a parameter that it only selects, so the mutant of `bind` that keeps the sign is not equivalent at the level of `bind`, though it is in every column of the cache, where SQLite stores 0.0 anyway. The `case nil` of `bind` is gone: no caller passed nil. `TestTx_types` has the upsert of a key that existed and then its delete, the column and the table in the message of every refused value, and a subtest of rows of another number of values and a key of two columns whose second value is refused. `TestTx_tooBig` fails the write-ahead log under an upsert and a delete, and the temporary file where a transaction records the keys that it touches, and requires `ErrLocal`, never `ErrTooBig`, and `found` false; `TestTx_readFails` (new) fails one read under an upsert into a table that existed and under a commit. `TestDB_stop_cache` checks the message of a column of another type. The lower bound of `Options.LengthLimit` is 1000.

The driver: `value` in `/rows.go` had two survivors, the copy of a blob and the case of NULL; `TestValue` in `/rows_test.go`, with `s4ql.Value` in `/export_test.go`, kills both.

`FuzzDifferential_core` takes a fourth input, whether the cache and SQLite have the lowered length limit.

Then a rerun of every survivor that a new test should kill, on a copy of the final tree: 15 of `stmt` (14 killed; the 15th, `!c.Table.Present() => true`, is equivalent with its twin, since a table name is then the next token), 20 of the engine, 4 of the format, and 2 of the driver, all killed by the hand-written tests; and the core's again.

### Why

A mutant of the port of SQLite's arithmetic is either equivalent, which needs a proof, or killed by a literal, which needs the literal: a random search does not reach the cases, so the search solves for them.

### What worked

Solving for the mantissa: `x = target * inverse(odd part of H) mod 2^(64+s+2-t)`, kept when x is a 64-bit number with its top bit set, gives inputs whose product sits where `fp10Convert2` looks at its extra 32 bits. A first version that only made the low bits zero found nothing in 2.4 million tries, because it never made a tie.

### What didn't work

- The first version of the tie search hung for 34 min on the first mutant: for small p the odd part of 10^p's top bits has many factors of two, so the modulus was 2^11 and the loop over multiples below 2^64 had 2^53 steps. A random multiple from 2^63 on fixed it.
- `timeout 3000 go test ...` failed with `command not found: timeout` on macOS.
- `TestTx_readFails` first armed every read to fail, and its mutants survived: the upsert after the failed read of whether the row exists needs the same pages, and fails anyway. A fault of `Times: 1` fails only the first read, which kills the mutant that drops the error of `exists`. For the commit, even one failed read fails the commit at a later step, `commit: sqlite: disk I/O error (code 266)`: SQLite rolls the transaction back after a read fails in it, so the mutants that drop the error of a query of `spoolTouched` are equivalent, and listed so.
- Six mutants of the core drop the error of the `create table` or `drop table` of a temporary table; no test fails the temporary file at those two statements. They stay out of `equivalent.txt`, so a run lists them as unexplained, and `docs/todo.md` has the test to write; the review asked for that, after a first version listed them in a section of their own.

### What I learned

- SQLite's tables of powers of 10 round the 96 bits to the nearest; only the special entry for 10^-1 is cut.
- `FUZZDIRS` in the Makefile lists directories, and `make fuzz` reads the names of the fuzz tests from their files, so CI's shards take new targets without a change of the workflow.

### What was tricky

The run reads the files of its tree for each mutant, so every run worked on an `rsync` copy, and a rerun on new tests needs a new copy and a new `-out`, since the fingerprint of the tree changes.

### What warrants review

- The reasons in `equivalent.txt` for `real.go` and for the core, above all the arithmetic of the bounds of p and e, and the six of the core that are reachable.
- The lower bound of 1000 for `core.Options.LengthLimit`.
- `FuzzTypes` and `FuzzTypes_core`, and whether `typesCall` marks each form as supported rightly.

### Future work

- A test that fails the temporary file at the `create table` or `drop table` of a temporary table of a transaction (in `docs/todo.md`).

## Step 4: The review, and the checks

**Author:** types-mutation builder

### Prompt Context

**Verbatim prompt:** (from the brief) "Run `make test`, `make lint`, and `make check-browser` at the end. [...] Review your own work with the code-review skill before you report, and fix what it finds."
**Interpretation:** Review the branch, fix the findings that matter, and run the three checks.
**Inferred intent:** A branch that the lead can merge after a read.

### What I did

`make test`, `make lint`, and `make check-browser` passed before the review. The review (`code-review high`, against 205a1ed, since `origin/types-checks` was gone after its merge) found one important issue and eight nits. The important one: the seeds of an upper-case blob literal at the length limit used `formBlobLiteral | 8`, which is 11, and `11 % forms` is `formRealArgument`; the form is now `form % forms`, and an odd `form / forms` writes the blob in upper case, with seeds of `formBlobLiteral + forms` at the limit and for every value. Of the nits I fixed: the placeholder of the core's row in the README of the tester (already filled in), the six reachable mutants of the core out of `equivalent.txt` so that a run lists them as unexplained, `FuzzTypes` and `FuzzTypes_core` in the layer `fuzz seeds` of `engineLayers` in `/internal/sql/mutation/run.go`, the comment of `FuzzTypes` on the limit that it ignores, the comment of `Options.LengthLimit` on a long identity, the reason of `t9 (k int) strict` in `TestDB_stop_cache`, which was empty and so checked nothing, the names in `TestPowerOfTen` (the order of its arguments was right already), and the new fuzz targets in `/internal/engine/README.md`.

### Why

A seed that does not test what its comment says is worse than none.

### What worked

The review read the base from the reflog when the branch it was given was gone.

### What didn't work

Nothing new.

### What I learned

`x | 8` and `x % 7` do not compose: a flag must live in its own part of the byte.

### What was tricky

Nothing more.

### What warrants review

As in step 3.

### Future work

As in step 3.

## Step 5: The merge of `main`, with slices 5 and 6

**Author:** merge builder

### Prompt Context

**Verbatim prompt:** (from the brief) "In your worktree: `git fetch origin`, `git checkout -B types-mutation-merged origin/types-mutation`, then merge `origin/main` into it with a merge commit. [...] No diary step is needed beyond one short step appended to `docs/diary/2026-11-20-types-mutation.md` with the diary skill, saying what the merge had to change."
**Interpretation:** Merge slice 5 (several columns) and the pass of slice 6 (`null`) into this branch, resolve the conflicts so that both sides' tests and notes stay true, and keep the fuzz targets and the lower bound of the length limit working.
**Inferred intent:** A branch that the lead can merge into `main` without losing the mutation testing of slice 4.

### What I did

Three files conflicted. In `/docs/todo.md`, the status of the SQL front end is `main`'s, with the branch's sentence on the mutation testing and `FuzzTypes` in place of "mutation testing and fuzzing of the types are not built yet"; the entry "Not built: mutation testing and fuzz targets of the types of slice 4" is gone, as the branch had it, and `main`'s entries for slices 5 and 6 are kept. In `/internal/engine/operator_internal_test.go`, both new subtests of `TestInsert` are kept; the branch's exact messages of the internal errors of `insert.check` became `main`'s "an insert of 1 values from 0 expressions into 1 columns", and a fifth case, a row of two values into a table of one column, reaches the other half of `main`'s condition. In `/internal/sql/mutation/equivalent.txt`, a list of every key that the generator makes over `listedPkgs` showed four entries that match no mutant: `columnPosition` and the two `slices.Equal` of `buildSink` (gone in `main`, which had removed the entries too) and the panic of `NewPlan` for a value (now at the end of `term`, where removing it does not compile). They are removed. `checkArguments condition: err != nil => true #2` still names a mutant, but its reason, at most one parameter, is false since slice 5: run alone, `TestConn_differential` kills it. It is removed. The other entries whose code `main` touched (`insert.check` with `convertReal`, `Conn.write`, `Conn.writeIn`, `recognizer.literal`, `Tx.touch` that `Tx.Insert` now also calls) were read against the new code and hold.

After the merge, `FuzzTypes` also inserts into a table of two columns for each type, `(v <type> not null, k int primary key)`, with `insert into t (k, v) values (1, ...)`, so that the value is not the key and the second insert meets the key at position 1. A throwaway count showed these inserts reach ok, `UNIQUE constraint failed: t_int_k.k`, every refusal of the type, and `string or blob too big`. The slice 5 entry of `/docs/todo.md` now says that `FuzzDifferential_core` has the lowered limit among its inputs.

### Why

The status and the open entries of `todo.md` must describe the merged code, and an entry of `equivalent.txt` with a false reason hides a gap in the tests.

### What worked

Comparing `equivalent.txt` with `go run ./internal/sql/mutation -list -pkgs <all listed packages>` finds every stale key at once, where `TestEquivalent` stops at the first one. `-run` on one key checks one mutant in about a minute.

### What didn't work

`TestEquivalent` reported only `no such mutant: internal/engine/operator.go buildSink swap arguments: ...`, the first of four. A first run of the mutant into the scratchpad's `mut` directory refused: "the results in .../scratchpad/mut are of other code or another SQLite or Go"; another run had used the name, so I used a new directory.

### What I learned

A key that still matches is not enough: the reason can go stale when a later slice changes what reaches the code, as one parameter per statement did.

### What was tricky

Nothing more.

### What warrants review

The rewritten cases of `TestInsert`, the five entries removed from `equivalent.txt`, and the tables of two columns in `FuzzTypes`. `make test`, `make lint`, and `make check-browser` pass; `FuzzTypes` ran 22,817 executions in 30 s and `FuzzTypes_core` 3,583 in 30 s on 2 workers, with nothing found.

### Future work

The mutation testing of slices 4 and 5 together, which the next builder runs, will tell whether the entries kept here still survive.
