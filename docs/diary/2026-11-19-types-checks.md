# Diary: Slice 4 of the roadmap in the generated checks, the other types

The engine and the driver hold the five column types of slice 4 since the branch `types-engine` (pull request #70), with hand-written and table-driven comparisons with SQLite. The generated checks still made only `INT` tables and integer values. This task teaches the generator `stmttest` every type, literal, and argument of slice 4, runs the engine's differential test, the driver's generated comparison with mattn's driver, and the engine's simulation over it, pins what each reaches, and plants a bug of types in the simulation. The branch is `types-checks`, from `origin/types-engine`.

## Step 1: Types in the generator, the differential tests, and the simulation

**Author:** types-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "1. **Types in the generator `stmttest`:** tables of all five types, in the letter cases SQLite accepts; literals of all four kinds, with weight on the edges [...] arguments of all supported Go kinds and of the rejected ones [...] values of the column's type and, less often, of another type; keys that SQLite treats as equal (`0.0` and `-0.0`), so that the unique error is reached for every type. Then remove the wrapping in the statement pass's tests if the generator makes it needless, and replace `typeArgument` in the simulation. 2. **The engine's differential test and the driver's generated test** run over these sequences and compare results, row order, and errors [...] Pin how often each interesting case is reached [...] 3. **The simulation of the engine** runs with typed tables and values, and a new planted bug for types shows that it finds a fault there. [...] 4. Update the pinned numbers that move and explain each in your report."
**Interpretation:** Move the values and literals of the wrapper in the pass's tests into `stmttest`, give each name of a table of a `Sequence` a type, generate values mostly of it, and make every generated check compare, excuse by the agreed rules, count, and require what it reaches; then a planted bug that only types expose.
**Inferred intent:** The generated checks, which found the most bugs in slices 1 to 3, cover slice 4 with the same trust, so that mutation testing and fuzzing of the types can build on them.

### What I did

`/internal/sql/stmt/stmttest/values.go` is new: `Integer` (moved), `Real`, `Text`, `Blob`, and `ValueOf` give values with weight on the edges (64-bit limits; `0.0` and `-0.0`, subnormals, limits of float64, values that are hard to round; the empty string, `''`, multi-byte text, text that looks like SQL or a number; the empty blob, zero bytes); one in 30 texts and blobs is about `LengthLimit` (1000) bytes. `Literal` writes a value in every form: integers with leading zeros and `-0`, reals in seven forms, one of them in 17 to 40 digits whose value the pass reads (SQLite reads more than 19 digits by its own rounding), strings with `''`, blobs with `x` or `X` and hex in lower, upper, or mixed case. `Argument` gives the Go value of a value, `Unsupported` holds the arguments that no column holds (nil, bool, `time.Time`, a `uint64` above int64, NaN, both infinities, a float32 infinity, text with NUL, invalid UTF-8, a nil `[]byte`), and `ArgumentKind` and `LiteralKind` name what a test reaches. `/internal/sql/stmt/stmttest/stmttest.go`: `Generator.Type` and `Generator.Value` take the table's name, and nil gives a type or a value at random; the create table writes its type in any case. `/internal/sql/stmt/stmttest/sequence.go`: each folded name of a table has a type of the sequence, which a create table declares nine times in ten and a value has nine times in ten; small sets of each type, with `0.0` and `-0.0`, so keys repeat; one in 16 texts and blobs is long; `around` gives a float32 for a float64, and for text or a blob a rejected value of the same Go type; `Like` gives another value of the same Go type; `Others` lost the INTEGER create table and gained `1e999`, `-1e400`, `9223372036854775808`, a hex literal, `null`, `-'a'`, two broken literals, and the types `any` and `varchar`. `Call.Runs` went, as nothing used it.

`/internal/sql/stmt/generate_test.go`: the wrapper is gone, and `newGenerator` gives a `*stmttest.Generator`. The pinned numbers moved: mutants 1 to 0 and 367 to 343, near misses 985 to 956; `/internal/sql/stmt/README.md` explains why.

`/internal/engine/differential_test.go`: a float32 argument becomes a float64 as the driver converts it; `typeRule` is the rule for a value of another type, shared with the simulation; `checkLength` excuses a literal longer than the limit and requires `18/18: string or blob too big`; `checkType` decides nothing in a read-only transaction (new `pair.readOnly`, which `/internal/engine/tx_test.go` sets) or for an argument longer than the limit; an insert whose record is too long sets SQLite's count of changes to 0, as a broken constraint does; half of the sequences of `TestDifferential` run at a length limit of 1000 in the cache and in SQLite; `pair.reach` counts each argument and literal by kind and each insert by the type of its column and its outcome, and `reachedDifferential` requires a least number of each.

`/internal/engine/simulation_test.go`: `typeArgument` went. A refusal for the type is recorded and checked by `typeRule` against SQLite on the state that the statement saw, in `verify` for a statement of its own and in `verifyTxs` in a transaction; every insert of the log must have a value of the type of its column (`stores a value of another type than its column`). `readRows` compared every value as an integer; it compares every type now. The planted bug `convertReal` (`/internal/engine/bugs.go`, `/internal/engine/operator.go`) stores a real that is a whole number in an INT or INTEGER column as the integer.

`/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`: `again` uses `Sequence.Like`, `pair.reach` counts and `TestGenerated` requires what the run reaches, and the pinned numbers moved.

### Why

The rules of slice 4 refuse what SQLite converts, so each generated check must know, for every insert, the type of the column in SQLite where the call runs. Sharing `typeRule` keeps the rule of the differential test and of the simulation one.

### What worked

The first full run of `TestDifferential` over typed sequences passed apart from its pins, and so did 3,000 more sequences from seed 1,000,000: the engine agrees with SQLite on every type, every edge, `-0.0` as a key, and `string or blob too big` for values and records. No difference from SQLite and no bug of the engine, the driver, or the core was found.

### What didn't work

- `TestConn_differential` failed at once: `the engine gives 8/8: attempt to write a readonly database [...], and the rule "s4ql: not supported: an integer literal for the column [x].outer of type TEXT"`. In a read-only transaction both refuse the write before the value; `checkType` now decides nothing there.
- `TestDifferential` failed with `SQLite counts 0 changes after its error, and 1 before` for a text of 996 bytes at the limit of 1000: the record is too long, which SQLite finds while it steps the insert and so sets the count to 0. The test accepts that now; the engine was right.
- `TestSimulation` failed on seed 0 with rows `["0"]` where SQLite gave `["real 0.5"]`: `readRows` wrote `v.GetInteger()` for every value. A gap of the check, not of the engine.
- A throwaway run of `TestGenerated` over 1,500 other seeds failed twice. First `Sequence.Like` turned a `+Inf` argument of a prepared statement into a real for its later runs, which the check of refusals did not expect; `Like` keeps a value that no column holds now. Then, at seed 103,080 and 107,735, `drop table` failed in SQLite with `6/6: database table is locked`: mattn's `Exec` of a `select` steps it once and does not reset it (`execSync` in `sqlite3.go` of v1.14.53), and the statement that `sql.Tx.StmtContext` gives for one prepared outside the transaction stays open until the transaction ends, on a pinned connection and through the pool. Closing that statement after the run helps only on a pinned connection, since through the pool `database/sql` keeps the one derived from the `sql.DB`'s statement until the connection is released; SQLite's side now prepares such a query in the transaction.
- `TestSimulation_plantedBugs` with 200 seeds failed for `readNewest` at seed 141, by `the end of the transaction 26: the transaction has ended`: when the bug's read of the newest state fails, the transaction keeps the read that the bug ended. That violation is the bug's, and is in its list now.
- The sandbox refused a `python3` heredoc of many replacements ("this command is too complex to verify that it stays inside the worktree"), after it had accepted smaller ones; the Edit tool worked.

### What I learned

- `fmt.Sprintf` with explicit argument indexes does not complain about arguments that a form does not use, which `Others` relies on.
- `database/sql` re-prepares a statement of a `sql.Conn` for `Tx.StmtContext` and closes it with the statement, but shares one of a `sql.DB` with its parent.

### What was tricky

- The order of the errors decides which rule applies: the length of an argument comes before the type, so `checkType` must leave an argument that is too long to the comparison with SQLite, and a literal that is too long has a rule of its own.
- A planted bug that the existing checks find through integers would show nothing new. `convertReal` is invisible to every comparison with SQLite, since SQLite converts the real of the log exactly as the bug did; only the new check of the log finds it.

### What warrants review

- `typeRule`, `checkLength`, and the order of `checkArguments`, `checkLength`, and `checkType` in `pair.step`.
- The changes of `verify` and `verifyTxs` in the simulation, and the exact check of the log.
- The pinned numbers and the least numbers in `TestDifferential` and `TestGenerated`, and the reasons in the report.

### Future work

- Mutation testing of the new code and fuzz targets of the types, for a fresh builder.
- The driver's generated test does not reach `string or blob too big`: `s4ql.Options` has no length limit, and the driver's `TestTypes` in `/conn_test.go` covers it by hand.

## Step 2: The long runs, the self-review, and the checks

**Author:** types-checks builder

### Prompt Context

**Verbatim prompt:** (from the brief) "Run `make test`, `make lint`, and `make check-browser`. [...] run new tests with `-count=20`, also under `-race`. [...] Review your own work with the code-review skill before you report, and fix what it finds."
**Interpretation:** Run the long and the repeated runs, document the checks in the engine's README, review the branch, fix the findings, and run the checks of the brief.
**Inferred intent:** Hand the lead a branch that is checked, documented, and reviewed.

### What I did

`S4QL_ENGINE_SIM_SEEDS=1000 go test ./internal/engine/ -run 'TestSimulation$'` passed for the three workloads, and `S4QL_ENGINE_SIM_SEEDS=200` for `TestSimulation_plantedBugs` found every bug, `convertReal` in 14 of 200 seeds, each by its own violation, and none without the bug. `TestSimulation` counts "refused for the type" now, with least numbers of 0.5, 0.1, and 1 for each seed in the default, idle, and hammer workloads (the first 200 seeds gave 505, 112, and 980). A writer brought the facts into `/internal/engine/README.md`; its report noted that SQLite converts a whole real for a STRICT INTEGER column too, so the comments of `convertReal` said "for a column that is not STRICT" wrongly, and that a tenth value takes any of the five types, not always another; both are fixed.

The review (`code-review high`, against `origin/types-engine`) found one important issue and five nits. The important one: an insert of the wrong type that the engine accepted in a transaction that did not commit never reaches the log, so the new check of the log does not see it, and SQLite converts it in `verifyTxs` and agrees. `verifyTxs` now checks every insert of a transaction that succeeded with `typeRule`. Of the nits I fixed: `around` gave a float32 that overflowed to an infinity, which the generator meant as a value (now only where it stays finite, which moved the pin of `TestDifferential` for arguments that no column holds from 141 to 140); `readRows` named a nil blob as `x''`; `coreState` let a nil blob or a value of no kind pass; gofmt had turned `''` in the comment of `Literal` into a typographic quote; the comment of `Sequence.Value`. I left one: the real literals of 17 to 40 digits take their value from the pass itself in `TestGenerate`, which `verify`'s check of the meaning in SQLite, `TestCheck_reals`, and `FuzzLiteral` cover against SQLite.

`make test` failed once in `/internal/sql/mutation`: `no such mutant: internal/engine/tx.go Conn.writeIn element: skipUnique: c.e.bugs.skipUnique =>`, since the line now names `convertReal` too. The key in `/internal/sql/mutation/equivalent.txt` is `skipUnique: c.e.bugs.skipUnique, =>` now.

### Why

A check of a planted bug is only as strong as the paths where it looks; the transaction that never commits was the one path left.

### What worked

The repeated runs: `-count=20` of `TestDifferential`, `TestConn_differential`, `TestSimulation`, and `TestSimulation_plantedBugs`, also under `-race`, of the generator's tests in `stmt` under `-race`, and of `TestGenerated` and `TestTypes` of the driver (`-count=5` under `-race`) all passed, and `-count=5` again after the fixes of the review. `make test`, `make lint`, and `make check-browser` pass.

### What didn't work

The failure of `make test` above, and nothing else.

### What I learned

A renamed line renames its mutants; `TestEquivalent` in `internal/sql/mutation` catches the stale key.

### What was tricky

Nothing more.

### What warrants review

The new check in `verifyTxs`, and the least numbers of the simulation for "refused for the type".

### Future work

As in step 1.
