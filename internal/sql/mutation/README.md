# Mutation testing of the tokenizer, tree, parser, pass after the parser, and engine

`internal/sql/mutation` measures the tests of `internal/sql/token`, `internal/sql/ast`, [`internal/sql/parser`](../parser/README.md), [`internal/sql/stmt`](../stmt/README.md), and [`internal/engine`](../../engine/README.md). It changes the code of these packages on purpose, one small change at a time. It runs the tests against each change, and reports every change that no test notices.

A copy of the code with one change is a mutant. The tests kill a mutant when one of them fails on it. A mutant that no test kills is a survivor, and a survivor is one of three things:

- It is equivalent: the code behaves the same with the change. It is listed in [`equivalent.txt`](equivalent.txt) with the reason.
- It is in code that nothing needs. That code is removed.
- It shows a gap in the tests. A new test closes the gap.

Only code that is not a test is changed. The tests are those that `make test` runs: the tests of every package under `internal` that depend on the changed package. They run on the whole corpus. The package at the root, the driver, is not mutated by default; with `-pkgs .` it is, and its tests are its own, without `TestOpen_realBucket`, which needs a real bucket. The tests of `internal/store/s3store` and `internal/store/s3test` that need a real bucket are left out too, which matters for `-pkgs internal/core`: `TestClient_conformance`, `TestClient_realBucket`, `TestClient_core`, and `TestDeleteOld_realBucket`. The differential test of the driver is in another module, and does not run on its mutants. The list of equivalent mutants may name mutants of the core, of the storage formats in `internal/format`, and of the root, which a run mutates only when `-pkgs` names them (`listedPkgs`).

## How to run

```
make mutation
```

The results go to the directory `MUTATIONDIR`. The default is `$TMPDIR/s4ql-mutation`, or `/tmp/s4ql-mutation` if `TMPDIR` is not set. `MUTATIONFLAGS` passes more flags:

```
make mutation MUTATIONDIR=/tmp/m MUTATIONFLAGS='-pkgs internal/sql/parser -run expr.go'
```

The target runs `go run ./internal/sql/mutation -out DIR` in the root of the module. The flags are:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-out` | | The directory of the results. Required, except with `-list`. |
| `-pkgs` | the five packages | The packages to change, separated by commas. |
| `-run` | | A regular expression. Only the mutants whose [key](#keys) or `file:line` matches it are run. |
| `-list` | | Print the mutants and exit. |
| `-report` | | Write the report of the results in `-out` again and exit. |
| `-j` | the number of CPUs | The number of mutants that run at once. |
| `-procs` | 2 | GOMAXPROCS of each test binary. 0 leaves it unset. |
| `-factor` | 20 | The time limit of a stage, as a multiple of its time on the code as it is. |
| `-min-time` | 1m | The smallest time limit of a stage. |
| `-memory` | 4294967296 | The limit of the resident memory of a test binary, in bytes. |
| `-gocache` | the usual cache | GOCACHE for the builds of the mutants. |
| `-equivalent` | `internal/sql/mutation/equivalent.txt` | The list of equivalent mutants. |
| `-first` | | A regular expression. The hand-written tests of the mutated package that it names run first, in a stage of their own, before its other hand-written tests. |

Before you start a run, know these points:

- **A run continues.** A run with the same `-out` skips the mutants that already have a result. A run that was stopped goes on where it stopped.
- **Results belong to one state of the code.** The first run records a fingerprint of the files under `internal`, code and tests, without this directory, of `go.mod` and `go.sum`, and of the version of Go. A later run on anything else refuses the directory, and so does `-report`. Remove the directory or use another `-out` then.
- **A run stops on a failure of its own.** If a test binary does not start, or a file changed since its mutants were made, the run stops without a result for that mutant.
- **It is not part of `make test` or of CI.** A full run takes too long for that. See [Results](#results).
- **The build cache grows.** Each mutant adds about 3 MB to the Go build cache, which is about 20 GB for a full run. `-gocache` gives the builds a cache of their own, which you can delete afterwards.
- **The run reads the files while it runs.** For each mutant, it reads the file again from the module that it runs in. Do not edit `internal` during a run. To keep editing, run it on a copy of the tree.

## The operators

An operator is one kind of change. The generator in `generate.go` reads each file with `go/parser` and applies every operator at every place where it fits. It does not change types or imports. Two operators that give the same text give one mutant.

- `operator`: a comparison becomes its boundary and its negation, so `<` becomes `<=` and `>=`. `==` and `!=` change places, and so do `&&` and `||`, `+` and `-`, `*` and `/`, and `+=` and `-=`. `%` becomes `*`.
- `negation`: `!x` becomes `x`.
- `condition`: a condition becomes `true`, `false`, and its negation. A condition is that of an `if`, of a `for`, or of a case of a switch without a tag, or an operand of `&&` or `||`. A comparison and a `!x` are not negated here, because `operator` and `negation` do that.
- `boolean`: `true` and `false` change places.
- `constant`: an integer or a character becomes the next value and the value before it, but not a negative one.
- `neighbour`: a use of a constant that is declared in a block becomes the next and the previous constant of the block. In a block of more than 20 names, such as the kinds of tokens, it becomes only the next one. The name `token.X` in `ast` and `parser` changes in the same way.
- `remove statement`: a call, an assignment that declares nothing, an increment, a `return`, a `break` or `continue`, a `defer`, or a send is removed.
- `increment`: `++` and `--` change places.
- `min/max`: `min` and `max` change places.
- `swap arguments`: two adjacent arguments of a call change places.
- `element`: one element of a composite literal is removed.
- `case element`: one expression of a case that has two or more is removed.
- `swap cases`: the bodies of two adjacent cases of a switch change places.

Two tables of the parser get every change of a single entry:

- **The fallback set**, `fallback` in `parser/names.go`. Each keyword in it is removed, by `element`. Each keyword that is not in it is added, by `add to set`.
- **The precedence table**, the switch of `infixPrec` in `parser/expr.go`. Each kind moves to the clause before and to the clause after, by `move in table`. Each kind that is in no clause is added to the first clause and to the last, by `add to table`. Space and Comment are left out, because the parser never sees them. A clause of one kind is not emptied: `neighbour` changes the precedence that it returns.

## Keys

Each mutant has a key that names it without a line number. A key in the list of equivalent mutants therefore stays valid when the file changes elsewhere.

```
parser/ddl.go parser.tableOption condition: p.is(token.Without) => false
```

The key holds the file under `internal/sql`, or its path from the root for a file outside it, the function or the declaration at package level, the operator, and the code before and after the change.

- An element of a table is named by its index: `token/token.go aiClass[89] constant: 2 => 1`.
- A constant shows the expression around it when that is short: `token/token.go Next constant: i = 1 => i = 0`.
- Equal keys in one function get `#2`, `#3`, and so on, in the order of the file.
- A removal has nothing after `=>`.

`-list` prints each mutant as its `file:line`, a tab, and its key.

## How a mutant runs

The tool never changes the repository. It writes the changed copy of the file into the directory of its worker, and the go command compiles that copy in place of the original through `-overlay`. Mutants therefore run in parallel.

Before the first mutant, the tool builds the tests of the code as it is and times each stage. Every stage must pass there.

The tests run in stages, in this order. The first stage that fails kills the mutant, and the report counts the mutant for the layer of that stage.

1. `hand-written`: the tests of the changed package, those that `-first` names first, in a stage of their own, and then the tests of the parser that are in no later stage
2. `corpus`: `TestParse_corpus`
3. `generator`: `TestGenerate`
4. `LALR automaton`: `TestRuleCoverage`
5. `fuzz seeds`: the seeds of `FuzzParse` and `FuzzGenerate`
6. `mutants`: `TestParse_mutants`
7. `shape check`: the tests of [`internal/sql/shapetest`](../shapetest/README.md)
8. `simulation`: the tests of `internal/engine` under the simulation, when it is the changed package
9. `other packages`: the tests of the other packages under `internal` that depend on the changed package

The tests of `internal/sql/stmt`, when it is the changed package, run in the same layers: `TestCheck_corpus` and `TestAllowList` in `corpus`, `TestCheck_grammar` and `TestGenerate` in `generator`, `FuzzCheck` and `FuzzGenerate` in `fuzz seeds`, and `TestCheck_corpusMutants`, `TestGenerate_mutants`, and `TestGenerate_nearMisses` in `mutants`. Its other tests are `hand-written`. When another package is changed, the tests of `internal/sql/stmt` are `other packages`. The tests of `internal/engine`, when it is the changed package, run in these layers: `TestDifferential` in `generator`, the seeds of every fuzz target of the engine in `fuzz seeds`: `FuzzDifferential`, `FuzzDifferential_core`, `FuzzSQL`, `FuzzSQL_core`, `FuzzTypes`, `FuzzTypes_core`, `FuzzColumns`, `FuzzColumns_core`, `FuzzWhere`, `FuzzWhere_core`, `FuzzChanges`, `FuzzChanges_core`, `FuzzCreateIndex_core`, `FuzzIndexes`, and `FuzzIndexes_core`, and `TestSimulation` and `TestSimulation_plantedBugs` in `simulation`; its other tests are `hand-written`. When a package under `internal/sql` is changed, the tests of `internal/engine` are `other packages`.

Each stage runs with `-test.failfast`. The layer `ownership` is not a stage. A mutant counts for it when the ownership check fails inside the tests of the parser and the output shows no disagreement with SQLite.

Three more rules decide the result of a mutant:

- **Time.** The limit of a stage is 20 times its measured time on the code as it is, and at least one minute. A mutant over the limit is timed out.
- **Memory.** A watchdog kills a test binary whose resident memory grows over 4 GiB, which a mutant that loops while it allocates reaches. That mutant is timed out too.
- **Compilation.** A mutant that does not compile is not counted in the score.

## The output

The directory of the results holds:

- `results.jsonl`, with one line for each mutant: its key, file, line, operator, the code before and after, the status, the layer, the test that failed with the start of its message, and the seconds it took. The status is `killed`, `survived`, `timeout`, or `uncompiled`.
- `report.txt`, which the tool also prints.
- `fingerprint`, `baseline`, and `work`, which the tool uses itself.

The report has a row for each package, a row for each file, and a total. Its columns are:

| Column | Meaning |
| --- | --- |
| generated | The mutants that have a result. |
| not compiled | The mutants that the compiler refused. |
| killed | The mutants on which a stage failed. |
| hand-written to other packages | The killed mutants, by the layer that killed them. |
| timed out | The mutants over the limit of time or of memory. |
| survived | The sum of the next two columns. |
| equivalent | The survivors that are in `equivalent.txt`. |
| unexplained | The survivors that are not. |
| score | (killed + timed out) / (generated - not compiled) |
| score w/o equivalent | The same, with the equivalent mutants taken out of the divisor. |

Three things can follow the table:

- the number of mutants that have no result yet
- each survivor that is not in `equivalent.txt`, with its `file:line` and its key
- each entry of `equivalent.txt` that matches no survivor: its mutant was run and killed, or the generator makes no mutant with that key

After a full run, the two lists of keys must be empty. A run of only some mutants checks only the entries of the mutants that it selected.

While it runs, the tool prints every survivor and every 50th result.

## The list of equivalent mutants

`equivalent.txt` holds a key on a line of its own, as `-list` prints it, and the reason on the indented line after it. A line that starts with `#` is a comment.

```
# Letters that start no keyword.
token/token.go aiClass[89] constant: 2 => 1
	No keyword starts with 'Y', so the lookup gives ID for the same word.
```

A key without a reason, or a key that is listed twice, is an error. `TestEquivalent` checks that every key names a mutant that the generator makes, so an entry cannot outlive its code. The report shows an entry whose mutant no longer survives.

The entries are of these classes:

- **The tokenizer.** A constant that the code never names, and the length of a table that a byte indexes. Character classes that share a case of `Next`. Letters that start no keyword, and words with a digit: the word is looked up and is an ID of the same length. Loops of `Next` that start one byte early, on a byte that is already checked.
- **The tree.** The cache of plans, which only saves time. An order that a later sort or a later check makes irrelevant. Conditions in `checkSpans` that its other checks cover.
- **Precedences.** Only the order of the levels matters. Level 6, that of ESCAPE, is unused, so its neighbours can move onto it. AND and NOT can share a level, because NOT continues on a tie and AND stops. COLLATE and the unary operators can share one, because no infix operator has the unary level. The flag `stopAtAnd` of `exprPrec` matters only for the low operand of BETWEEN.
- **Bounds of character tests** in `checkVariable` and `digitSeparatorError`, moved by one onto a character that cannot stand there.
- **The end of input.** The guard in `Tokens` that the tokenizer never reaches, the value of `kindEOF`, the empty semicolon after a semicolon, the offsets of the two end tokens, and the tail that `run` returns with an error.
- **The pass after the parser.** In `childAt`, the test that a child starts before the token: the children of an accepted node are in the order of the text.

## Results

The final run of `token`, `ast`, and `parser`, on their code as it is now, made 7,029 mutants:

| Package | Generated | Not compiled | Killed | Timed out | Equivalent | Unexplained | Score | Score without equivalent |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `token` | 2,514 | 118 | 2,293 | 66 | 37 | 0 | 98.46% | 100.00% |
| `ast` | 421 | 89 | 325 | 0 | 7 | 0 | 97.89% | 100.00% |
| `parser` | 4,094 | 599 | 3,418 | 31 | 46 | 0 | 98.68% | 100.00% |
| total | 7,029 | 806 | 6,036 | 97 | 90 | 0 | 98.55% | 100.00% |

The layers that killed them first: hand-written 5,545, corpus 221, ownership 94, generator 119, LALR automaton 10, fuzz seeds 0, mutants 13, shape check 34, other packages 0. Every survivor is in `equivalent.txt`.

The run took 1 h 15 min with 10 workers on an Apple M4 with 10 cores and 16 GB, while a fuzz run used 4 of the cores. Five of its mutants survived only because the tests that kill them came after the copy of the tree that the run read; a run of those five on the final tests killed them all in 1 min. Planning, which builds and times every stage on the code as it is, takes about 3 min of the total.

### The pass after the parser and the engine

On 2026-11-18, the run of `internal/sql/stmt` and `internal/engine` with 6 workers, while other work used the machine, made 2,102 mutants in about 35 min. It left 110 survivors: 74 in the engine, 34 in `stmt.StartedTable`, one in the recognizer, and the known equivalent one. Each was a gap in the tests but those listed in `equivalent.txt`: the events of the engine on its paths of failure (their outcomes, rows, positions, and durations), the end of a read transaction after a loop that stops early, `Rows.Close` during a loop, the offsets of the errors, plans that no statement of slice 1 gives, the ends of the texts of `StartedTable`, and an empty name after a schema name. The runs of the affected files on the tests that close them, and of all of the engine, then left:

| Package | Generated | Not compiled | Killed | Timed out | Equivalent | Unexplained | Score | Score without equivalent |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `stmt` | 1,122 | 123 | 970 | 14 | 15 | 0 | 98.50% | 100.00% |
| `engine` | 955 | 312 | 632 | 0 | 11 | 0 | 98.29% | 100.00% |

After the review of the engine on 2026-11-18, `StartedTable` moved out of `stmt` into `stmttest`, which is code for tests and is not mutated, and the engine changed. The runs of each package with 6 workers, while other work loaded the machine, then left:

| Package | Generated | Not compiled | Killed | Timed out | Equivalent | Unexplained |
| --- | --- | --- | --- | --- | --- | --- |
| `stmt` | 896 | 117 | 766 | 12 | 1 | 0 |
| `engine` | 1,025 | 308 | 703 | 0 | 12 | 2, which tests added after the run kill |

The run of `stmt` took 7 min, and that of the engine 13 min.

Of the engine's killed mutants, the layers that killed them first were hand-written 628 and simulation 4. The equivalent ones of the engine are the planted bugs, a yield to the scheduler, a bound that means "none", the order of the attributes of an event, and a check of the level that `log/slog` makes again.

### The parameters of slice 2

On 2026-11-19, the code that slice 2 added was mutated: `recognizer.parameter`, the `?` in `recognizer.insert` and after a sign in `recognizer.integer`, and the methods `Parameters` in `stmt`; `prepare`, `Check`, `checkArguments`, `countArguments`, the parameter of `NewPlan`, and its case of `eval` in the engine; and `arguments`, `argument`, `CheckNamedValue`, and `overflows` in the driver at the root, for which the runner learned to plan the stage of the root's own tests. The runs with 8 and 4 workers took a few minutes each:

| Package | Generated | Not compiled | Killed | Timed out | Equivalent | Unexplained |
| --- | --- | --- | --- | --- | --- | --- |
| `stmt` | 49 | 8 | 40 | 0 | 1 | 0 |
| `engine` | 77 | 12 | 65 | 0 | 0 | 0 |
| `.` | 80 | 23 | 55 | 2 | 0 | 0 |

The hand-written tests killed every killed mutant first. Two survived the first run and were gaps in the tests, which new tests close: the guard that the name of a parameter is the next token, which only a tree that no text gives can reach (`TestRecognize`), and a `driver.Valuer` whose own value is an unsigned integer above the largest `int64`, which must give the value of its `Value` (`TestParameters`). The one equivalent mutant is the acceptance of a parameter's node, whose one token is taken at once. The two that time out loop for good on a pointer in `overflows`. The planted bug `annotateNext` of the engine has three equivalent mutants, each another wrong annotation.

### The transactions of slice 3

On 2026-11-19, the code of transactions was mutated: all of `tx.go` in the engine, the methods `serving`, `savepoint`, `release`, and `rollbackTo` of `coreTx` and `coreReadTx` in `storage.go`, and `Conn.Close`, `Conn.newReport`, `Conn.write`, `Conn.query`, `Conn.pending`, and `Rows.read` in `engine.go`, which the transactions changed; and `Tx.Savepoint`, `Tx.Release`, `Tx.RollbackTo`, `Tx.Serving`, and `ReadTx.Serving` in the core, with `-pkgs internal/core`, which no earlier run of this tool mutated. The runs had 8 workers, while other work loaded the machine:

```
go run ./internal/sql/mutation -out DIR -j 8 -pkgs internal/engine -run '^internal/engine/(tx\.go |storage\.go (coreTx|coreReadTx)\.(serving|savepoint|release|rollbackTo) |engine\.go (Conn\.(Close|newReport|write|query|pending) |Rows\.read ))'
go run ./internal/sql/mutation -out DIR -j 8 -pkgs internal/core -run '^internal/core/(tx\.go Tx\.(Savepoint|Release|RollbackTo|Serving) |read\.go ReadTx\.Serving )'
```

| Package | Generated | Not compiled | Killed | Timed out | Equivalent | Unexplained |
| --- | --- | --- | --- | --- | --- | --- |
| `engine` | 628 | 128 | 469 | 8 | 23 | 0 |
| `core` | 101 | 24 | 63 | 4 | 10 | 0 |

The run of the engine took 19 min, and that of the core 36 min, of which a mutant that times out took half: each mutant of the core runs all the tests of the core, 85 s on the code as it is, and the time limit is 20 times that. In the engine, the hand-written tests killed 428 mutants first and the simulation 27; in the core, the hand-written tests killed all 63: `TestTx_Savepoint` first for 54, `TestReadTx_Serving` for 7, and the runtime for 2, whose mutants unlock a mutex that they did not lock. The 4 timeouts of the core remove the unlock of `DB.mu` in a `Serving` or the `defer tx.leave()` of `Tx.Savepoint` and `Tx.RollbackTo`, so a later call waits for good; the 8 of the engine leave a transaction open, so a later begin waits for good.

The first run of the engine left 32 survivors that were not listed. Fourteen were gaps in the tests, which new tests close, and a run of their lines killed each of them: the events of a statement in a transaction (the rows of a write, the parent of a write in a write transaction and none in a read-only one, the refusal of a query before and at its start, and the refusal of a query while rows hold the end of a transaction), the duration of the event of a transaction, a single event for a read-only commit inside a loop over its rows and the cut of the loop in the event of its query, the words of the refusal of a begin on a closed connection, a statement of a transaction whose context has ended, which a create does not check itself, and a release that fails (`TestConn_events`, `TestConn_rows`, `TestConn_writeIn`). The other 18 are in `equivalent.txt`: 11 in the planted bugs, the order of two joined errors, the check of the level of an event that `log/slog` makes again, and four in the end of a transaction that its rows hold, where `Conn.leaveEnd` sets two flags that `Rows.read` tests one after the other, and where one order of two errors is reached only after a local failure in a roll back. All 10 survivors of the core are in `equivalent.txt`: `DB.refused` gives back nil for nil, the two calls of `forget` in `Tx.RollbackTo` each do what the other does, and six checks of an error of SQLite's `SAVEPOINT`, `ROLLBACK TO`, and `RELEASE` inside an open transaction, which no test can make fail.

The run of the core needed three changes of the tool. The tests of `internal/store/s3store` and `internal/store/s3test` that need a real bucket are now left out, as `TestOpen_realBucket` is, because their packages depend on the core and the planning of the first run failed on `TestClient_conformance`. `oneLine` now cuts at the start of a character and replaces bytes that are not UTF-8, because a run stopped with `mutation: jsontext: invalid UTF-8 within "/Detail" after offset 438` when the message of a failed test was cut inside a character; `TestOneLine` shows both. And the list of equivalent mutants may name mutants of the core, which `TestEquivalent` and the report now know (`listedPkgs`).

### The types of slice 4

On 2026-11-20, the code that slice 4 added or changed was mutated, in five packages: in `stmt`, all of `real.go`, the port of SQLite's conversion of a real literal, and `recognizer.literal`, `recognizer.column`, `columnTypes`, `literalName`, and `Type.String`; in the engine, all of `errors.go`, `operator.go`, and `plan.go`, `fromCore` and `coreDB.lengthLimit`, and `Conn.write`, `Conn.checkOutside`, and `Conn.writeIn`, which call the new check of the sink; in the core, `bind`, `readRow`, `typeOf`, `typeSQL`, `typeNames`, `table.checkValue`, `table.checkRow`, `table.checkKey`, `conn.exists`, the lines of `openConn`, `conn.load`, and `Options.withDefaults` that slice 4 changed, `DB.LengthLimit`, `isTooBig`, and `Tx.Upsert`, `Tx.Delete`, `Tx.touch`, `Tx.recording`, `Tx.spoolTouched`, `Tx.committed`, `Tx.dropTouched`, `Tx.dropTemps`, `tooBig`, and `keyColumns`; in the format, `checkRow`, `checkPrimaryKey`, `checkStored`, and `checkValue` of `check.go`; and in the driver, `argument`, `arguments`, `check`, `conn.CheckNamedValue`, `overflows`, and `value`. For example:

```
go run ./internal/sql/mutation -out DIR -j 4 -pkgs internal/sql/stmt -run '^stmt/(real\.go |recognize\.go (recognizer\.(literal|column) |columnTypes|literalName)|stmt\.go Type\.String)'
go run ./internal/sql/mutation -out DIR -j 4 -factor 3 -pkgs internal/core -run '^internal/core/tx\.go (Tx\.(Upsert|Delete|touch|spoolTouched) )'
```

The runs had 2 to 4 workers each, two or three runs at once, while other work held the machine at a load of about 25 on 10 cores. The run of the core took 2 h with `-factor 3`, since each of its survivors runs the core's tests, 3 min under that load. A rerun of the survivors on the tests that close them, then of the last ones, gave:

| Package | Generated | Not compiled | Killed | Timed out | Equivalent | Unexplained | Killed by the new tests |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `stmt` | 781 | 62 | 671 | 8 | 40 | 0 | 63 |
| `engine` | 1,010 | 262 | 721 | 2 | 25 | 0 | 20 |
| `core` | 628 | 128 | 451 | 4 | 39 | 6 | 48 |
| `format` | 90 | 24 | 65 | 0 | 1 | 0 | 4 |
| `.` | 118 | 37 | 79 | 2 | 0 | 0 | 2 |

The hand-written tests killed every killed mutant first, but 28 of the engine's: 27 that its simulation killed, and one that its differential test killed. `FuzzTypes` and `FuzzTypes_core` were in the layer `hand-written` in these runs, and are in `fuzz seeds` since. No mutant showed a fault in the code. The gaps that the new tests close:

- **`real.go`.** Random texts, even those made to be hard to round, never reach the bounds of `atof` nor the products near a tie of the rounding, where `fp10Convert2` decides by the 32 bits after the 64 of the product. `realEdges` in `TestCheck_reals` holds literals at each: the digits 1844674407370955160 at which SQLite stops reading a mantissa, exponents at and past 10,000 after 10,000 digits of a fraction, 2^54 - 1, and ties with 10^16 and 10^27 that a search found, each with and without a sign and compared with SQLite. The ties came from a throwaway search that compiled each mutant beside the original and solved for 64-bit mantissas whose product with the power of 10 has its low bits zero. `TestPowerOfTen` checks the tables of powers of 10 against the exact powers: SQLite rounds the 96 bits of 10^(27g) to the nearest, and cuts its special entry for 10^-1. `TestMultiply160` checks `multiply160` against `math/big`.
- **The engine.** The offset of `string or blob too big` and of a write in a read-only transaction, against SQLite's; the internal errors of `checkType`, `insert.check`, and a nil blob literal; the check of every row of an insert; `Format` of a real of more than one digit; and every refusal of the core in `fromCore`.
- **The core.** `TestBind` binds every kind directly, and pins that -0.0 is bound as 0.0, which SQLite would keep for a parameter that it only selects; in a column of the cache SQLite stores 0.0 anyway. Its case for nil, which no caller passed, is gone. `TestTx_types` upserts a key that existed and then deletes it, and checks the column and the table in the message of every refused value, rows of another number of values, and a key whose second value is refused. `TestTx_tooBig` and the new `TestTx_readFails` fail the write-ahead log, the main file, and the temporary file under an upsert, a delete, and a commit, and require a local failure, never `ErrTooBig`, a refused delete that finds no row, and no commit object. The lower bound of `Options.LengthLimit` is 1000 now: below about 300 bytes SQLite refuses the cache's own schema, so 30, SQLite's smallest, could never open a cache.
- **The format and the driver.** The tables of refusals of the reader and the writer check why a value is refused, not only that it is, and `TestValue` of the driver checks that a blob is a copy, NULL is nil, and a value of no kind is an internal error.

The equivalent ones are in `equivalent.txt`, by package. Those of `real.go` each say why: a bound that gives the same quotient, an exponent for which the result is 0 or the infinity either way, a commutative product, a byte that a real token never holds. Those of the engine are mostly in the planted bug `convertReal`. The six unexplained ones of the core are not equivalent: they drop the error of the `create table` or the `drop table` of a temporary table of a transaction, which only a failure of the temporary file at that statement reaches, and no test makes yet. They are not in the list, and `docs/todo.md` has the test to write.

### The several columns of slice 5 and the NULL of slice 6

On 2026-11-20, the code that slices 5 and 6 added or changed was mutated: every function with a line that a commit since #71 changed, and not #76 alone, found with `git blame` of each file. That gave, in `stmt`, `recognizer.createTable`, `recognizer.column`, `recognizer.insert`, `recognizer.literal`, `recognizer.parameter`, `recognizer.resultColumn`, `recognizer.selectCmd`, `isNull`, `maxParameters`, `CreateTable.ColumnsError`, `CreateTable.Key`, `Insert.Parameters`, `maxColumns`, and `fieldNames`; in the engine, `buildSink`, `buildValues`, `buildNode`, `columnsError`, `eval`, `insert.check`, `insert.run`, `null`, `positionsOf`, `checked`, `Conn.write`, `Conn.checkOutside`, `Conn.writeIn`, the `insertNew` of `readOnly` and of `coreTx`, `checkArgument`, `checkType`, the errors of the columns, `NewPlan`, `term`, `names`, `Format`, `formatNode`, `formatExpr`, and `zeroOf` of the planted bug `zeroLeftOut`; in the core, `conn.setUp`, `conn.head`, `conn.record`, `conn.position`, `conn.insert`, `record.check`, `record.position`, `record.versionError`, `newTable`, `table.createSQL`, `table.createTable`, `cacheVersion`, `Tx.CreateTable`, `Tx.Insert`, `Tx.committed`, and `zeroValue`, and only the changed lines of `conn.load`, `table.checkValue`, `Tx.spoolCommit`, `Tx.spoolTouched`, and `Open`, which #76 mutated whole; in the format, `checkCreateTable` and `checkPrimaryKey`; and in the driver, `argument` and `check` of `conn.go`. `conn.upgrade` and `conn.checkNoNull` were left out, since the upgrade of a cache of version 1 is to be removed. The runs had 2 to 4 workers each, two to four runs at once, at a load of 25 to 30 on 10 cores; the core's took 2 h 32 min with `-factor 3`, and `-first` ran 15 of its tests first, which kill most of its mutants in 22 s, where its whole hand-written stage takes 5 min. The survivors ran again on the tests that close them, the engine's with `-min-time 5m` and those of the planted bugs of the core with `-min-time 10m`, since a planted bug that a mutant makes rare lets the simulation run to its limit of seeds:

| Package | Generated | Not compiled | Killed | Timed out | Equivalent | Unexplained | Killed by the new tests |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `stmt` | 608 | 79 | 514 | 1 | 14 | 0 | 10 |
| `engine` | 1,056 | 261 | 762 | 2 | 31 | 0 | 12 |
| `core` | 411 | 73 | 286 | 21 | 31 | 0 | 24 |
| `format` | 97 | 40 | 57 | 0 | 0 | 0 | 5 |
| `.` | 54 | 24 | 30 | 0 | 0 | 0 | 0 |

In the core, 2 of the 24 that the new tests catch time out: they drop the check in `Tx.Insert` that the transaction has not ended, and the test that calls it after the end then runs past the time limit. Seven mutants of the planted bug `convertReal` timed out in the first run of the engine and were killed by its simulation in the rerun, in the hammer workload. The hand-written tests killed every killed mutant first, but 25 of the engine's, which its simulation killed, and one of `stmt`, which its corpus killed. One mutant showed a difference from SQLite: `CreateTable.ColumnsError` gave `duplicate column name` and `too many columns` the offset of the column's name, and SQLite gives -1; the offset is not promised, and is -1 now. The gaps that the new tests close:

- **The pass.** Guards of `recognizer.createTable`, `recognizer.column`, and `recognizer.resultColumn` that test whether a token is the next one matter only when an earlier token of the same column or select is left untaken: ON CONFLICT or ASC before a second PRIMARY KEY or a NULL, NOT NULL ON CONFLICT in a table without a key, and DISTINCT before `*`, TRUE, or a literal, which are cases of `TestCheck` now. Two guards that only a tree that the parser never gives reaches, a parameter beyond SQLite's limit and a column named twice in the list of an insert, are cases of `TestRecognize`. `checkCreateTable` of the pass's tests compares the offset of `ColumnsError` with SQLite's.
- **The engine.** `pin` compares the offset of every error that it pins with SQLite's; `TestInsert`, `TestValues`, and `TestNewPlan` have a row of more values than expressions, a row of another number of values than its positions, and a select of one column.
- **The core.** `TestOpen_cache` fails each read of the open of a cache, one after the other, and each lock, write, and sync of the make of a new one, and requires a local failure, never `ErrCacheVersion` or `ErrCacheDatabase`, which kills the two mutants of `conn.head` that the core of slice 6 left; it refuses a database that holds tables and no `s4ql_cache`. `TestTx_tooBig` fails the temporary file at the create of the temporary tables of a transaction, at `DropTable`, and at the commit, through a wrapper of the spool file that arms the fault at the one write of the commit object, which kills the six mutants that slice 4 left; it fails an insert locally, and refuses a create table whose declaration in the cache is longer than the limit. `TestTx_Commit` refuses `Tx.Insert` after the end and into no table, and commits nothing of a row that a transaction inserts and deletes. `TestTx_null` checks why a NULL is refused, and that the cache declares a column that does not take NULL as NOT NULL. `TestDB_stop_cache` refuses a key with the default NULL in a table that is not STRICT, and a table that s4ql did not make through a create table of its name.
- **The format.** The tables of refusals of the reader and the writer say why every create table is refused.

The equivalent ones are in `equivalent.txt`. Most are in the planted bugs, among them the variants of `convertReal` that convert in more columns or fewer, and in the rejects of the recognizer whose next token describe names in the same way; the others are the pragma of `synchronous`, which only a power loss shows, statements whose only failure is for want of memory, and the declaration of a key as `not null`, which a key of a table without rowid is anyway. The planted bug `convertReal` runs in the hammer workload since, where 20 of 200 seeds find it, against 5 in the default one; one mutant of it that the list held as equivalent, which converts only for an INTEGER column, is killed there and left the list, and two that the first run killed in the default workload, which convert the reals that are not whole and plant no conversion in a transaction that spans statements, survive in the hammer one, as the list holds.

### The WHERE of slice 7

On 2026-11-20, the code that slice 7 added or changed was mutated: every function with a line that a commit of #80, #81, or #82 changed, found with `git blame` of each file, and only the changed lines of `recognizer.insert`, `recognizer.selectCmd`, `Conn.exec`, `Conn.queryText`, `Conn.query`, `newEngine`, `Conn.queryIn`, `insert.check`, and `Tx.spoolCommit`. That gave, in `stmt`, `recognizer.conditions`, `condition`, `isCondition`, `leftColumn`, and `term`, `depth`, `isBinary`, `isValue`, `isTrueOrFalse`, `comparisons`, `maxDepth`, `Operator.String`, the operators, and `Select.Parameters`; in the engine, `scan.rows`, `filter`, `condition.holds`, `compare`, `keyAccess`, `tighter`, `buildQuery`, `build.node`, `build.filter`, `checkType`, into which `checkCompared` was merged before the run, `NewPlan`, `formatNode`, `bound.core`, the `scanRange` of `coreReadTx` and `coreTx`, and the planted bug `openBound`; in the core, `conn.scan`, `table.checkBounds`, `ReadTx.Scan`, `ReadTx.ScanRange`, `Tx.Scan`, and `Tx.ScanRange`; and in the driver, `check`, whose call of the engine slice 7 removed. `engine.AccessOf` is code for tests and is not mutated. The runs had 1 to 4 workers each, four runs at once, at a load of 18 to 30 on 10 cores; the core's took 1 h 8 min with `-factor 3`, and that of the engine 38 min. The survivors then ran again on the tests that close them, after the merge of slice 8 into the branch, with all of `recognizer.where` and `recognizer.conditions`, which slice 8 changed:

| Package | Generated | Not compiled | Killed | Timed out | Equivalent | Unexplained | Killed by the new tests |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `stmt` | 313 | 70 | 239 | 2 | 2 | 0 | 8 |
| `engine` | 695 | 155 | 517 | 0 | 23 | 0 | 12 |
| `core` | 202 | 66 | 111 | 6 | 19 | 0 | 6 |
| `.` | 12 | 5 | 7 | 0 | 0 | 0 | 0 |

The rerun of `stmt` made 72 mutants, of which 11 did not compile, 57 were killed, 2 timed out, and the 2 equivalent ones survived. The hand-written tests killed every killed mutant first, but 5 of the engine's, which its simulation killed, 2 of `stmt`'s, which its corpus killed, and one of the core's, which the tests of the engine killed. The seeds of `FuzzWhere` and `FuzzWhere_core` ran in the layer `hand-written` in these runs, and are in `fuzz seeds` since. No mutant showed a fault in the code. The gaps that the new tests close:

- **The pass.** A unary operator other than NOT as a whole condition, such as `where -v` and `where ~v`, a `+` and a `~` before a value on the left of a comparison, and a column named `true` or `false` with the name of its table in a condition, are cases of `TestCheck`.
- **The engine.** `TestFilter` ends a filter whose condition cannot be tested on a row with that internal error, which nothing else reaches; requires that a condition that fails is not true and that a comparison that fails gives 0; and reads every row of a table whose key is two columns, which no plan of SQL gives. `TestEngine_where` requires that the option that reads every row reaches its engine, which no comparison of rows can see.
- **The core.** `TestScanRange` refuses a bound that is not a key next to one that is, names the lower of two refused bounds, and fails every read of a scan after the look-up of its table, in a read and in a write transaction, as a local failure.

The equivalent ones are in `equivalent.txt`. Most are in the planted bug `openBound`, whose variants open only one of the two bounds, which the simulation finds by the other, or also open a bound without a value, which bounds nothing. In the rest of the engine, a condition that fails is also not true, a test for NULL has no value whose `HasNull` could be true, and `keyAccess` cannot fail after `checkType`. In the core, they are the failure paths of `conn.scan` that no file system of the tests reaches, since its prepare and its bind come after the look-up and the checks of the bounds, and a failed step gives its error again in the reset; the order of the key, which a table WITHOUT ROWID without an index gives without ORDER BY; and spaces of the SQL. In `stmt`, they are `max`, which is commutative, and the accept of a column reference without a table, whose one token is taken at once.

### The UPDATE and DELETE of slice 8

On 2026-11-20, the code that slice 8 added or changed was mutated: every function with a line that a commit of #88, #91, or #94 changed, found with `git blame` of each file, and only the changed lines of `recognizer.statement`, `recognizer.selectCmd`, `fieldNames`, `build.conditions`, `buildSink`, `Conn.checkOutside`, `Conn.write`, `Conn.writeIn`, `NewPlan`, `Format`, `rowsOf`, and `report.of`. That gave, in `stmt`, `recognizer.update`, `recognizer.delete`, `recognizer.where`, `recognizer.conditions`, the methods `Parameters`, `Bounds`, and `setSpan` of `Update` and `Delete`, and `Select.Parameters`; in the engine, `build.change`, `build.filter`, `changeRows`, `update.check`, `update.run`, `remove.check`, `remove.run`, `finds`, `createTable.check`, `dropTable.check`, `check`, `setsChanges`, `readOnly.delete`, `readOnly.keys`, `noSuchSetColumn`, and the `keys`, `add`, `all`, and `delete` of `coreKeys` and `coreTx`, with the planted bugs `changeNothing` and `keepOldKey`, a line each in `changeRows` and `update.run`; in the core, `Tx.Keys`, `KeySet.Add`, `KeySet.All`, `KeySet.after`, `Tx.createKeyTable`, `Tx.dropKeys`, `Tx.touch`, and `Tx.Delete`, the delete of a row, which slice 8 did not change but which every delete and change of a key runs; and in the driver, `check`. The runs had 1 to 4 workers each, three or four runs at once, at a load of 15 to 30 on 10 cores; the engine's took 47 min with `-factor 5`, and that of `stmt` 13 min. The core's ran 183 of its mutants in 1 h 29 min with `-factor 3`, since each survivor runs every test of the core, about 10 min under that load; it was stopped there, and its other mutants ran with its survivors on the tests that close them. The survivors of the engine and of `stmt` ran again on those tests too. `main` then brought the pass of slice 9 (#92), which changed the driver's `check` again, and the driver ran again on that code; what #92 and #95 changed in the other functions above, the refusal of an index in the engine's `check` and the length of the schema of a create table in `buildSink`, is not in this run:

| Package | Generated | Not compiled | Killed | Timed out | Equivalent | Unexplained | Killed by the new tests |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `stmt` | 162 | 27 | 132 | 2 | 1 | 0 | 2 |
| `engine` | 455 | 159 | 287 | 0 | 9 | 0 | 45 |
| `core` | 271 | 62 | 159 | 33 | 17 | 0 | 14 |
| `.` | 18 | 2 | 16 | 0 | 0 | 0 | 1 |

The hand-written tests killed every killed mutant first, but 7 of the engine's, which its simulation killed, and 2 of the core's, which the tests of the engine killed. The 33 of the core that time out leave out the end of a call of the transaction, so that a later call waits for good, or read the same key of a set again and again. The seeds of `FuzzSQL`, `FuzzSQL_core`, `FuzzColumns`, `FuzzColumns_core`, `FuzzChanges`, and `FuzzChanges_core` ran in the layer "fuzz seeds" in these runs, and `TestEngineLayers` requires every fuzz target of the engine there. No mutant showed a fault in the code. The gaps that the new tests close:

- **The pass.** Two guards of `recognizer.update` that test whether a token is the next one, for a column named twice in SET and for an assignment beyond SQLite's limit, matter only for a tree that the parser never gives, which are cases of `TestRecognize`.
- **The engine.** `changeTrio` of `TestEngine_update` and `TestEngine_delete` compares the offset of each error with SQLite's, and `TestEngine_events` has the events of an update and a delete. The fake of `operator_internal_test.go` fails a delete, an add of a key, and the read of the keys on demand, and `TestChangeRows` pins every failure of `changeRows`, `update.check`, `update.run`, and `remove.run` with the calls of the fake, the stop at the next key once the context ends, a check of NOT NULL that reads the source once, the planted bugs through `buildSink`, and the internal errors of `build.change` for plans that no statement gives and of a found key whose row is gone.
- **The core.** `TestTx_Keys` refuses a set after the end of the transaction, gives the error of f before that of a drop of the set that fails locally, and gives no error of its own for a set that f leaves where the transaction can only be rolled back, and `ErrClosed` for an instance that f closes. An add to a set and a read of it after f has closed the instance are refused, and the read gives its error once.
- **The driver.** `TestPrepare` refuses an index at the prepare, which the driver's `check` asks of `engine.Check` since #92.

The equivalent ones are in `equivalent.txt`, in a section of slice 8 at its end. In the engine, they are a sink of a read-only transaction that is never run, the key that is never nullable, as for an insert, a comparison of two keys of which only the equality is read and which cannot fail after `update.check`, and `changeNothing` and `keepOldKey` left out of a write in a transaction, which statements of their own find. In the core, they are the number and the name of the temporary table of a set, which only need to differ from the others, the statements of a set that `Tx.dropKeys` leaves at rest, or whose finalize cannot fail, since `conn.rest` finalizes a statement whose reset fails, the block of the error of `KeySet.Add`, which gives nil for nil, and the return after the error of `KeySet.All`, whose key is nil then; with them stay those of `Tx.touch` and `Tx.createKeyTable` that earlier rounds listed. The two entries of `build.bind` that a merge had moved from `build.filter` name the error of `keyAccess`, and ran again: both survive, since `build.bind` checks the type of every value before `keyAccess`, for a select, an update, and a delete.

### The indexes of slice 9

On 2026-11-21, the code that slice 9 added or changed was mutated: the mutants on a line that #92, #97, #105, or #108 wrote, or this round itself, found with `git blame` of each file, in the functions that these pull requests added or changed. That gave, in `stmt`, `recognizer.createIndex`, `recognizer.indexColumn`, `recognizer.indexName`, the methods of `CreateIndex` and `DropIndex`, `reservedPrefix`, and lines of `recognizer.drop`, `recognizer.statement`, `fieldNames`, `nodeName`, `nodeNames`, and `reservedName`; in the format, `checkCreateIndex`, `CommitWriter.CreateIndex`, `CommitWriter.DropIndex`, and lines of `checkChange`; in the core, all of `index.go` that slice 9 wrote, `conn.lookUp`, `conn.readSchema`, `conn.scan`, `table.checkIndexed`, `ReadTx.ScanRange`, `Tx.ScanRange`, both `tableWithoutIndexes`, and lines of `conn.delete`, `conn.insert`, `conn.load`, `conn.upsert`, `openConn`, `ReadTx.Get`, `ReadTx.Table`, `apply`, `Tx.CreateTable`, `Tx.DropTable`, `Tx.Get`, `Tx.RollbackTo`, `Tx.Savepoint`, `Tx.Table`, and `Tx.spoolCommit`; and in the engine, all of `access.go`, `openBounds.scan`, `Engine.reading`, the errors of indexes, `build.access`, `build.bind`, `buildCreateIndex`, `buildDropIndex`, `buildQuery`, the operators of the two statements, the methods `scanned`, the index methods of the storage, `report.drew`, `report.reads`, `scanOf`, and lines of `Conn.checkOutside`, `Conn.query`, `Conn.write`, `newEngine`, `build.change`, `build.filter`, `build.node`, `buildSink`, `scan.rows`, `Format`, `NewPlan`, `fromCore`, `report.end`, `report.of`, `Conn.queryIn`, and `Conn.writeIn`. The lines that #107, #111, #112, and the engines of slices 10 to 12 wrote in the same functions are left to their own rounds. The driver's `check` has no line of slice 9 left: #107 put back the call that #105 removed, and slice 11 took it out again; its 18 mutants ran once, before that, and 14 were killed, 2 did not compile, and 2 survived in the call of #107.

`main` was merged three times during the round, and each time the mutants of the functions that the merge changed, and every survivor that the list did not hold, ran again on the new code. The scratch directory of the runs was emptied twice by a cleanup outside the round, and the full disk of the machine voided one run of the core (`no space left on device`, and I/O errors in 10 of its 25 results); those runs ran again. The runs had 2 to 4 workers each, two to four at once, at a load of 20 to 32 on 10 cores; a survivor of the core took 7 to 15 minutes, since it runs every test of the core and of the engine, and `-first` ran a dozen tests of indexes and commits of the core first, which kill most mutants in seconds. The last results of each mutant:

| Package | Generated | Not compiled | Killed | Timed out | Equivalent | Unexplained | Killed by the new tests |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `stmt` | 141 | 23 | 117 | 0 | 1 | 0 | 4 |
| `format` | 61 | 20 | 41 | 0 | 0 | 0 | 0 |
| `core` | 1,137 | 285 | 738 | 38 | 76 | 0 | 26 |
| `engine` | 794 | 231 | 540 | 0 | 23 | 0 | 37 |

The hand-written tests killed every killed mutant first, but 3 of the core's, which the tests of the engine killed. Of the 38 that time out in the core, 9 are in `equivalent.txt`: mutants of `conn.loadIndexes` and `conn.lookUpIndex` that survived the runs before the merge of #112 and ran past their limit after it, under the load of that run. One mutant showed a fault: `Conn.queryIn` drew the direction of a query in a transaction and did not put it in the event, which the README of the engine promises for every query; it does now. The gaps that the new tests close:

- **The pass.** `TestCheck` refuses a schema before a reserved name of an index, in a create index and in a drop index, as "schema name". `TestRecognize` gives the recognizer a second column of an index and a WHERE of a create index that are not the next token, and a create index named by the bare word IF without IF NOT EXISTS, which it takes.
- **The engine.** `TestChooseAccess` pins that an index that pins more wins over a later one that pins less and bounds the next column, which no test of the rules held after the merge of slice 11. `TestForcedAccess` gives the internal error of `fit`. `TestEngine_reading` pins the directions of a seed against splitmix64 from Vigna's code, and `TestEngine_report` the direction and the access in the event of each statement of a transaction. The fake of `operator_internal_test.go` fails the read of the indexes, the lookup of an index, and the lookup of a table of one name: `TestFilter` and `TestBuild` pin that a select, an update, a delete, a create table, a create index, and a drop index pass those errors on, that a path of an index that the table does not have reads as the rules choose, and the internal error of a create index that names a column twice. `TestNewPlan` pins the plans of a create index and a drop index, and `TestFromCore` the refusals of indexes of the core.
- **The core.** `TestOpen_cache_indexes` refuses an index on a table of a reserved name, and finds no index under a reserved name, that another process adds. `TestReadTx_Close` refuses `Indexes`, `Index`, `ScanIndex`, and `ScanPlan` after the end of a read transaction, and the new subtests refuse the plan of a scan of no table, of no index, and of too many pins. `TestTx_CreateIndex` pins the length and the limit of an index too long for the cache and the whole refusal of an index without a name; `TestScanIndex` refuses a valid lower bound with an upper bound of another storage class; `TestReplay_indexes` applies an index of exactly 250 columns; and `TestTx_readFails` fails a read under a create index, a drop index, and a scan by an index.

Code that nothing needs went: the check of rule 4 in `engine.chooseAccess`, the parameter `byNull` of `engine.pinOf`, the bounds of `core.conn.scan`, and the refusal of an index without columns in `core.conn.loadIndexes`; `core.conn.lookUpIndex` reads the owner of an index with LIMIT 1, so that its query fails only before its row. The equivalent ones are in `equivalent.txt`, in a section of slice 9: variants of the planted bugs `openBound`, `shortKey`, `turnDescBound`, and `checkOutside`; the order of the arguments of `max` and of the sum of `indexTooBig`; errors of `fit` that `checkType` rules out; what `pragma_index_list` and `pragma_index_xinfo` give; errors that a caller reads before the number or the bool beside them, and the number of changes of the replay, which no caller reads; the prepare and the bind of a scan, which come after the lookup and the checks; a step that fails, whose error the reset gives again; the version of the cache; and the spaces of a condition. The 18 that the round left unexplained in the core ignore the error of a read of the cache or of a write of the spool file at one place of the commit of a transaction with a create index or a drop index, or of the replay of one, which no test made fail then. On 2026-11-21, `TestTx_Commit_indexFaults` and `TestReplay_indexFaults` fail each read of the cache, on a cache whose page cache is shrunk to one page so that SQLite reads most pages from the file, and each write of the spool file, one fault in each run, at the commit and at the replay of five transactions with indexes. They kill 14 of the 18, and the table counts them among the killed. The other 4 ignore an error of `CommitWriter.CreateIndex` or `CommitWriter.DropIndex`, which the writer keeps and returns again at its next method or its close, so the commit fails as before; they are in `equivalent.txt`.

### IF NOT EXISTS, DEFAULT, and a value on the left of slices 10 to 12

On 2026-11-21, the code that slices 10, 11, and 12 added or changed was mutated: the mutants on a line that #107, #109, #110, #111, #114, #116, or #118 wrote, found with `git blame` of each file at `25c39a7`. That gave, in `stmt`, `recognizer.ifExists`, `recognizer.notIf`, `recognizer.sign`, `recognizer.defaultValue`, `takes`, `recognizer.conditionColumn`, `depth`, `Operator.Swapped`, and lines of `recognizer.createTable`, `recognizer.createIndex`, `recognizer.drop`, `recognizer.column`, `recognizer.literal`, `recognizer.condition`, `recognizer.isCondition`, and `fieldNames`; in the engine, `Conn.nothingToWrite`, `nothing`, `replaceTable`, `ifClause`, `report.doesNothing`, `tooLongForCache`, and lines of `Conn.write`, `Conn.writeIn`, `buildSink`, `buildCreateIndex`, `buildDropIndex`, `Format`, `NewPlan`, `report.end`, `report.of`, `coreTx.createTable`, `coreTx.createIndex`, `fit`, `build.conditions`, and `rowsOf`; in the core, all of `default.go`, and lines of `conn.load`, `newTable`, `table.createSQL`, `table.createTable`, `cacheVersion`, `conn.create`, `conn.createIndex`, and `Tx.CreateTable`; in the format, `checkDefault`, and lines of `checkCreateTable`, `checkStored`, and `checkStoredValue`; and in the driver, `check`. The runs had 1 to 3 workers each, three at once, at a load of 11 to 24 on 10 cores, on copies of the tree on another disk; the core's took 1 h 40 min with `-factor 3`, and the engine's 1 h 29 min with `-factor 5`. The survivors, and the mutants on the lines that the round changed, then ran again on the final code:

| Package | Generated | Not compiled | Killed | Timed out | Equivalent | Unexplained | Killed by the new tests | Removed |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `stmt` | 264 | 43 | 215 | 0 | 0 | 0 | 5 | 6 |
| `engine` | 413 | 68 | 321 | 4 | 20 | 0 | 14 | 0 |
| `core` | 315 | 50 | 248 | 0 | 8 | 0 | 4 | 9 |
| `format` | 67 | 13 | 53 | 0 | 1 | 0 | 0 | 0 |
| `.` | 10 | 4 | 6 | 0 | 0 | 0 | 0 | 0 |

Removed counts the survivors in code that nothing needs, which is gone. The hand-written tests killed every killed mutant first, but 21 of the engine's, which its simulation and its other layers killed. No mutant showed a fault in the code. The gaps that the new tests close:

- **The pass.** `TestRecognize` takes a create table named by the bare word IF without IF NOT EXISTS, from a tree that the parser never gives. `TestCheck` refuses ON CONFLICT before a second DEFAULT, and a DEFAULT in parentheses around a literal that the value refuses for itself, `(0x10)` and `(current_time)`, as an expression.
- **The engine.** `TestEngine_write` gives the event of a statement of IF NOT EXISTS or IF EXISTS that does nothing, its position and no parent, and takes the write path where a reader of its own cannot be opened; `TestConn_writeIn` releases such a statement in a transaction without a rollback to its savepoint; `TestEngine_reading` draws no direction for one; `TestBuild` refuses a plan whose default is a nil blob literal; `TestFromCore` maps `core.ErrTooLongForCache` of a table and of an index to "not supported", on a real core.
- **The core.** `TestOpen_cache_defaults` pins the cache's statement of a table whose key is two columns, `TestTx_tooBig` the whole message of a table and of an index too long for the cache, and `TestDefaultSQL` refuses every literal for a column of no known type.

Code that nothing needs went: the guards of a name after DEFAULT in `recognizer.defaultValue` and the test `value != ""` in `recognizer.conditionColumn`; the `if`s around the clone of a default in `core.newTable` and `table.createTable`, `pk == 0` in `conn.load`, and the checks of the quotes and the `x'` in `core.parseDefault`, which its round trip makes. The equivalent ones are in `equivalent.txt`, in sections of slices 10 and 11: variants of the planted bugs `replaceExisting` and `nullDefault`, `nothing.run`, which no caller reaches, the error beside a sink of `buildSink`, the length check of a left-out default, the version of the cache, the precision of `strconv.FormatFloat` and the bit size of `strconv.ParseFloat`, and the errors that the round trip of `parseDefault` refuses anyway. Slice 12 left one survivor, in `conditionColumn`.

### What the first run of the parser found

The first run, before any fix, made 7,186 mutants. It took 1 h 57 min with 10 workers on an Apple M4 with 10 cores and 16 GB, while the machine did other work.

| Package | Generated | Survived |
| --- | --- | --- |
| `token` | 2,514 | 42 |
| `ast` | 480 | 90 |
| `parser` | 4,192 | 128 |
| total | 7,186 | 260 |

Of the 7,186 mutants, 822 did not compile, 6,006 were killed, 98 timed out, and 260 survived. That is a score of 95.91%.

The run found no fault in the tokenizer, the tree, or the parser. Every survivor was equivalent, in code that nothing needs, or a gap in the tests.

#### Gaps in the tests

Each gap is closed with a test.

In `token`:

- `Kind.IsKeyword` was never checked to be false, and `Kind.String` was not checked at the first kind past the last. `TestKind_IsKeyword` and `TestKind_String` check both.

In `ast`:

- The tests of `CheckOwnership` only checked that some error came, so most parts of the check could be removed unnoticed. Each broken tree now requires the exact message. The new cases are a token at the end that no node owns, a span that ends before it starts, a token without text, a node without tokens whose span is not empty, an empty node before its parent, and an empty node at either end of its parent, which the check accepts.
- `Walk` of nil, and a nil element in a list of nodes, for `Children` and `Dump`.
- The order of `Tokens` when the fields are not in the order of the text.
- A node with a slice of strings, on which `planOf` panics.

In `parser`, compared with SQLite in `TestParse_sqlite`:

- the variables `#$a` and `#::a`, on which the check of `#N` must not fire
- a digit separator before `e`, as in `0_e1` and `01_e5`, and after a dot, as in `1._5`
- digit separators next to the bounds of the hexadecimal digits, as in `0x9_0`, `0xa_f`, and `0xF_A`
- WINDOW, OVER, and FILTER read as names, as in `select window(1)`. The trees of `window`, `over`, and `filter` are pinned too.
- RETURNING after a WITH clause, in DELETE, UPDATE, and INSERT

In `parser`, in its other tests:

- the length limit of a later statement of `ParseAll`, counted from where its parse starts
- the offset -1 of the errors of the limits
- a panic that is not a syntax error, which must pass through `run`
- the guard of `advance`
- the depth in `TestParse_depth`, which must grow with the nesting. The check that the depth stays below SQLite's stack was vacuous for a depth of 0.
- siblings, which are not nesting: 2,510 common table expressions parse, and so do 2,510 FROM items and 2,510 expressions in parentheses

#### Code removed

In `ast`:

- the walk of a pointer to a struct that is not a node, which no node has
- the check in `structOf` that only a nil node reached
- the `+ 1` at the start of the enum `fieldKind`

In `parser`:

- the `+ 1` at the start of the enum `class`
- the check after a comma between table constraints, and the check for `(` after FOREIGN KEY: the next read fails at the same token anyway
- `p.is(token.Without)` before the table options, because WITHOUT falls back to ID
- `prec == 0` in `exprPrec`
- the unused constant `precEscape`
- in `read`, the reading past the end of input or past an unrecognized token, which the parser never does, with its two fields
- `kind == token.ID` in `getToken`
- the raw kinds of the end tokens
- in `digitSeparatorError`, the test `in == 0`, because a number never starts with a separator. Its test for a hexadecimal number is now `strings.HasPrefix`.
- `lastEnd: start` in `newParser`

#### The precedence table

The shape check kills most mutants of the precedence table. The hand-written tests of the parser do not pin every pair of levels. SQLite does, through [`internal/sql/shapetest`](../shapetest/README.md).

### The pass after the parser

The final run of `internal/sql/stmt` alone, on the code as it is now, made 890 mutants:

| Package | Generated | Not compiled | Killed | Timed out | Equivalent | Unexplained | Score | Score without equivalent |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `stmt` | 890 | 117 | 760 | 12 | 1 | 0 | 99.87% | 100.00% |

The hand-written tests killed 759 of them first and the corpus 1. The run took 8 min 30 s with 5 workers on an Apple M4, while a fuzz run used 4 of the cores. One survivor of it was killed by a new hand-written case, in a run of the 7 mutants of that line.

The first run, on the first version of the package, made 976 mutants, of which 55 survived. None showed a fault in the gate. They were:

- Guards that the order of the tree makes redundant: a check that a token is the next one, where nothing can come before it, and `accept` of a node whose tokens get a name of their own anyway. The guards were removed.
- Gaps in the tests: a missing part reported at the wrong token when an earlier token is not supported, as in `with c as (select 1) replace into ...`, a schema before a reserved name, a type of two words, and the case of `@` and `[` next to the ASCII letters. Each has a case in `TestCheck` now.
- Code that nothing needs: a fast path for ASCII in the check for UTF-8, a branch for an error of the parser of another type, and names of fields that no offset can reach.
- Paths that only a tree that the parser never gives reaches, such as the end of the tokens where the recognizer expects one. `TestRecognize` gives the recognizer such trees.
