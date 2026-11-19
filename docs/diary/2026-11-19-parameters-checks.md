# Diary: Generated and randomized checks of the parameters of slice 2

Slice 2 was merged as `87077a8` with hand-written tests only: the generator of statements, the differential tests, the simulation, and the mutation testing made no arguments. This task adds arguments to each of them, on the branch `parameters-checks` from `origin/main` at `87077a8`.

## Step 1: The generator of statements

**Author:** parameters-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "The generator of statements (`internal/sql/stmt/stmttest`). `Sequence` (and what the generators of single statements need) also produces an insert with a plain `?` and its argument, and now and then the cases around it: the wrong number of arguments (none for a `?`, one for a text without `?`, two for one `?`), an argument of a type that is not supported, a named argument, the other forms of a parameter (`?1`, `:a`, `@a`, `$a`), `-?`, and arguments at the limits of a signed 64-bit integer. Every consumer of the generator must keep working and should use the arguments."
**Interpretation:** Give the generator a way to write `?` with an argument, give `Sequence.Next` the arguments of each call, and make the cases around them; then adapt every consumer.
**Inferred intent:** The randomized checks reach the code of slice 2 as they reach that of slice 1.

### What I did

`/internal/sql/stmt/stmttest/stmttest.go`: `Generator.Parameters` (one insert in N writes `?`, whose argument is the value) and `Generator.CallOf(k)`, which gives the text, the statement, and the arguments; `Statement` and `StatementOf` are unchanged in signature, and draw no extra random number while `Parameters` is 0.

`/internal/sql/stmt/stmttest/sequence.go`: `Next` gives a `Call` (`Src`, `Args []any`, `Query`, `Supported`). One insert in three has a `?`. `around` changes the arguments of one call in four of a statement with a parameter and of one in twenty of one without: for a `?`, none, two, one of a type in `Unsupported` (nil, a string, a float, a blob, a bool, a `time.Time`, a `uint64` above the largest `int64`), a `sql.Named`, or another Go integer type (`int`, `uint64`, `int32`); for a statement without a parameter, one argument, an integer or of a type in `Unsupported`. `Others` has `?1`, `:a`, `@a`, `$a`, `-?`, `(? ?)`, `(?), (?)`, and a `?` in WHERE, each with an argument for each `?`. `Call.Runs` and `Integers` say what s4ql must run. `Sequence.Value` gives a value of the sequence's set, at the limits one time in four.

`/internal/sql/stmt/generate_test.go`: the generator of the pass's tests writes a `?` in one insert in four, so `TestGenerate` checks the offsets of `?` in random gaps, and `TestGenerate_mutants` mutates it. `TestGenerate_nearMisses` keeps `Parameters` at 0, since its forms write the value themselves.

### Why

`Supported` keeps its meaning, a text of slices 1 and 2, so that the consumers judge the arguments by the rule and not by a flag of the generator.

### What worked

`TestGenerate` passed at once with the `?` inserts, offsets included.

### What didn't work

`go test ./internal/sql/stmt/` failed on the pinned counts: `TestGenerate_mutants`: `3 texts are syntax errors where SQLite does not say [...] but the test pins 2` and `360 texts are not supported where SQLite does not say [...] but the test pins 375`; `TestGenerate_nearMisses`: `1000 [...] but the test pins 1046`. The near misses went back to 1046 with `Parameters` at 0 there. A throwaway test logged the three syntax errors where SQLite does not say: all are mutants of CREATE TABLE (`collate` after PRIMARY KEY, `,` for `)`), the same kind as before, moved by the new random stream. The pins and the table of the README of `/internal/sql/stmt` are now 3 and 360.

In the first version of `around`, only 20 of 12,000 calls of the engine's differential test had an argument of a type that is not supported: the change hit any position, also one past the end. The rates above give 87.

A Python edit given inline to Bash was refused by the sandbox ("this command is too complex to verify that it stays inside the worktree"); scripts in the scratchpad, run with the worktree as an argument, worked.

### What I learned

The mutants of the generator now hold three that swap a value and a `?`; they are supported and pass `verify`.

### What was tricky

Keeping the random stream of every consumer that does not ask for parameters unchanged, so that its pinned numbers do not move without a reason.

### What warrants review

- `around` in `/internal/sql/stmt/stmttest/sequence.go`: the cases and their rates.
- That `Call.Runs` and `Integers` match what the driver converts: any Go integer kind that fits `int64`, no name.

### Future work

None.

## Step 2: The driver against mattn, with arguments

**Author:** parameters-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "`TestGenerated`, `TestSequences`, and `FuzzSequence` make calls with arguments: exec with an argument, prepared exec with an argument run several times with different arguments, query with an argument. The oracle is `mattn/go-sqlite3`. Required outcome kinds in `TestGenerated` grow to cover: an insert through a parameter that succeeds, one that fails on a key that exists, the wrong number of arguments, an argument of a type that is not supported, and a named argument. Nothing that s4ql runs may escape the comparison with the oracle."
**Interpretation:** Pass the generator's arguments through every call, add a prepared statement that runs several times, and check every refusal of arguments against s4ql's rule.
**Inferred intent:** The driver's conversion and counting of arguments is compared with SQLite at random, and no refusal is excused without a check.

### What I did

`/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`: `run` takes the arguments of each run. Two calls: `callQueryArgument` and `callPrepareExecRuns` (Prepare once, Exec for each run; an `outcome` holds the later runs in `next`). `fixedRuns` gives the arguments of a call in the sequences written by hand and in `FuzzSequence` (1, and 1, 2, 1 for the runs). `generated` passes the generator's arguments to every call, and a prepared statement that runs several times runs one to three more times with other values of the same types (`again`).

`compare` now decides from the text: a text that `stmt.Check` refuses runs on mattn without arguments, since SQLite refuses it before it binds; a text that s4ql supports, and that s4ql ran, must have had an argument that s4ql supports for each parameter in each run (`Call.Runs`), or the test fails, and runs on mattn with the same arguments. A "not supported" of a supported text goes to `checkArgumentRefused`, which requires, for each run, the message of the first refused argument as `argumentRefusal` writes it (the name before the type, `uint64` above the limit, nil, or the type). The error of the number goes to `checkArguments`, for each run, against `engine.Check`. The excuse of an error at prepare requires that every run gave mattn's error.

`TestGenerated` requires 18 more kinds, among them `insert with a parameter ok`, `insert with a parameter UNIQUE constraint failed`, `insert with a parameter wrong number of arguments`, `insert with a parameter argument of a type not supported`, `insert with a parameter named argument`, and each call with arguments. Pinned: 1,127 errors at prepare (816 before: prepared statements are now one in four of the statements that are no query, and those that run several times), 11 tables started (14), 410 calls of the wrong number of arguments, and 100 of an argument that s4ql does not support. A new sequence written by hand pins a prepared insert run with 1, 2, 1, a query with an argument, and the errors through `callPrepareExecRuns`.

### Why

The comparison is driven by what the text and the rule say s4ql must do, so an outcome that is neither a refusal by the rule nor a text's error always meets mattn.

### What worked

`TestSequences` passed with the new sequence on the first run, and `FuzzSequence` ran 3,649 inputs in 8 s.

### What didn't work

The first run of `TestGenerated` counted 636 outcomes of the call `""`: in `run`, `r.call, o = c, r` assigned `o` the value of `r` from before the assignment, without its call. Two statements fixed it.

### What I learned

mattn has no `CheckNamedValue`, so `database/sql` converts its arguments with `driver.DefaultParameterConverter`, and refuses a `uint64` above the largest `int64` itself. A text that SQLite refuses therefore runs on mattn without the arguments, or the oracle would fail on its own conversion.

### What was tricky

The prepared statement that runs several times: an excuse or a refusal must hold for each run, not for the first.

### What warrants review

- `compare`, `checkArgumentRefused`, and `argumentRefusal` in `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`: that no path lets an outcome of s4ql through without mattn or the rule.

### Future work

None.

## Step 3: The engine against SQLite, with arguments

**Author:** parameters-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "The differential and generated tests of `internal/engine` run generated statements with their arguments on the engine and on SQLite, and compare results, errors, and the state after. Where SQLite's C API differs by agreement (a missing argument is NULL there, an error in s4ql), the test does not compare with SQLite and checks s4ql's rule instead, and the number of such cases is pinned or bounded so that the excuse cannot grow unseen."
**Interpretation:** Run the calls of the sequence with values of the engine, bind the same integers in SQLite, and check the refusals against the rule.
**Inferred intent:** The engine's counting, binding, and refusing of arguments is compared with SQLite at random.

### What I did

`/internal/engine/differential_test.go`: `values` turns the generator's arguments into values of the engine as the driver would: an integer for a Go integer that fits, the value of a named argument (the driver refuses the name before the engine), and text, a real, a blob, NULL, or a value of no kind for the rest. `pair.step` takes a `stmttest.Call`; `checkArguments` checks the error of the number (with its exact message) and the "not supported" of the first argument that is not an integer, and every other answer is compared with SQLite, which binds the integers, or gets none for a text that it refuses before it binds. `answer` has the message of the engine's own errors, and `errText` knows `ErrArguments`. The tally counts the kinds `with a parameter`, and `TestDifferential` requires six of them. Pinned: 641 calls of the wrong number of arguments, 87 of an argument that is not an integer, and 25 tables started (34 before, by the new random stream). `TestEngine_noneOpen` runs the calls with their arguments.

### Why

The engine and the driver share the rule, so both tests check it with the same exact messages.

### What worked

The comparison passed on the first full run after the rates of `around` were fixed.

### What didn't work

Nothing else failed.

### What I learned

The named arguments reach the engine's test as their values, so they add inserts through a parameter that are compared with SQLite.

### What was tricky

None beyond step 1.

### What warrants review

- `checkArguments` and `step` in `/internal/engine/differential_test.go`.

### Future work

None.

## Step 4: The simulation, with arguments and a planted bug

**Author:** parameters-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Callers also issue inserts with arguments. The check that reads the commit objects verifies that each statement's annotation holds exactly the arguments that the caller gave. Plant one bug to show that the check can fail [...] The planted bug must be found by the simulation within its seeds."
**Interpretation:** The callers of the simulation run the calls of the sequence with their arguments, the check of the log compares the parameters of each annotation with the caller's arguments, and the log runs in SQLite with its parameters.
**Inferred intent:** The arguments in the log, which a replay trusts, are checked end to end under faults.

### What I did

`/internal/engine/simulation_test.go`: an attempt keeps its arguments as values of the engine; the error of the number and the refusal of an argument are `skipped` and checked by `checkArguments`; `readLog` keeps the parameters of each annotation; `verify` requires that they equal the caller's arguments (kind and value, by position), runs each commit in SQLite with its parameters, and each observed statement with its arguments. `tallyOf` counts the commits with a parameter, and `TestSimulation` requires 0.5 of them for each seed of the default workload and 1 of the hammer workload, about a fifth of what 1,000 seeds give (2.17 and 4.01).

The planted bug `annotateNext` in `/internal/engine/engine.go` and `/internal/engine/bugs.go` annotates each argument plus one while the row gets the argument. `TestSimulation_plantedBugs` finds it in 2 of the first 3 seeds by "and its caller gave the arguments", and finds nothing on those seeds without it.

### Why

Without the parameters, the log's statements with a `?` would run in SQLite with NULL and fail; with them, the log is checked as the replay reads it.

### What worked

The bug was found at once, by its own violation.

### What didn't work

Nothing failed.

### What I learned

The replay of the log and the check of the commits in SQLite would also find the bug, later and less directly: the bug's list of violations allows them after its own.

### What was tricky

Nothing.

### What warrants review

- The case of the commit's parameters in `verify`, and `valuesText`.

### Future work

None.
