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
