# Diary: Mutation testing of the SQL parser

Measure whether the tests of the SQL tokenizer, syntax tree, and parser (`/internal/sql/token`, `/internal/sql/ast`, `/internal/sql/parser`) notice when the code is wrong: change the code on purpose, one small change at a time, run the tests that `make test` runs for `internal/sql/...`, and sort every change that no test notices into equivalent, dead code, a gap in the tests, or a gap in the generators.

## Step 1: Requirements and scope

**Author:** parser-mutation-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "You are the builder for a testing task in s4ql (`maragu.dev/s4ql`, repo `maragudk/s4ql`): "Mutation-test the SQL parser". [...] What nobody has measured is whether those tests would notice if the parser's code were wrong. That is your task: change the parser's code on purpose, one small change at a time, run the tests, and see which changes go unnoticed. A change that no test notices is either code that does not matter (and should be simplified away) or a gap in the tests (and gets a test). The product owner's standard is total confidence in the tokenizer and parser."
**Interpretation:** Build a reproducible mutation run over the three packages, with the operators the brief lists (conditions, `&&`/`||`, comparison boundaries, constants, token kinds and precedences to neighbours, statement removal, swapped cases and arguments, and every single-entry change of the fallback set and the precedence table), run in parallel with time limits, report kills per test layer, and explain every survivor.
**Inferred intent:** Turn "the tests are strong on paper" into a measured statement, and close whatever the measurement shows is open, with SQLite as the judge where possible.

### What I did

I read `/AGENTS.md`, `/internal/sql/parser/README.md`, `/internal/sql/parsetest/README.md`, the start of `/internal/sql/shapetest/README.md`, the package docs and code of `/internal/sql/ast`, the test files of `/internal/sql/parser`, all of the parser's code, the tokenizer, and the diary `/docs/diary/2026-11-16-sql-parser.md` for the form. The worktree was at `e485b60`, the tip of `origin/main`, and I made the branch `parser-mutation-testing`.

### Why

The brief names the tests that count (those of `make test` for `internal/sql/...`), the layers to attribute kills to, and the constraints (no new module dependency, no other parser's code, nothing outside the worktree), so the tool had to be designed around them.

### What worked

The earlier mutation work of the storage core (`/docs/diary/2026-11-12-storage-core.md`, Step 4) used `go test -overlay`, which compiles a mutated copy of a file in place of the original without touching the tree. That is the "copy of the tree" the brief asks for, at a fraction of the cost of copying the module per mutant.

### What didn't work

Nothing yet.

### What I learned

`internal/sql/oracle` and `internal/sql/parsetest` import `internal/sql/token`, so a mutant of the tokenizer reaches the judge's helpers too. The tokenizer's own tests compare it with SQLite's C `sqlite3GetToken`, which a Go mutant cannot reach, so the judge there stays independent.

### What was tricky

Nothing yet.

### What warrants review

That the requirements above match the brief.

### Future work

None yet.

## Step 2: A mutation tester of our own

**Author:** parser-mutation-builder

### Prompt Context

**Verbatim prompt:** "Choose the tool yourself: a small mutator of your own on `go/ast` in a test-only command inside the repository, or an existing tool run from outside without adding it to `go.mod`. No new module dependency." and "Run the tests against each mutant, in an order that kills fast [...] in parallel across cores, each mutant in a copy of the tree so that runs do not disturb each other or the repository, with a time limit per mutant that is a large multiple of the measured normal run time"
**Interpretation:** Write the mutator and the runner as a command in the repository, like `/internal/sql/corpus/update`, with its operators chosen for this parser.
**Inferred intent:** A run that anyone can repeat after a change to the parser, with results that say which test layer noticed each change.

### What I did

I wrote `/internal/sql/mutation`, a `main` package:

- `generate.go` walks the `go/ast` of each non-test file and makes text edits: relational operators to their boundary and negation, `&&` and `||` swapped, arithmetic `+ - * / %`, `+=` and `-=`, `++` and `--`; every condition of an `if`, a `for`, a tagless `case`, and each operand of `&&` and `||` to `true`, `false`, and its negation; removal of `!`; `true` and `false` flipped; integers and characters to their neighbours; a use of a constant of a block (token kinds, precedences, name classes, character classes) to its neighbour in the block; removal of a call, an assignment that declares nothing, an increment, a `return`, a `break` or `continue`, a `defer`; the bodies of two adjacent cases swapped; two adjacent arguments swapped; `min` and `max` swapped; each element of a composite literal and of a multi-element `case` list removed. For the two tables the brief names, it adds every keyword missing from the fallback set of `/internal/sql/parser/names.go`, moves each kind of `infixPrec` to the clause before and after, and adds every kind missing from `infixPrec` to its first and last clause. Each mutant has a key without line numbers, such as `parser/ddl.go parser.createTable condition: p.is(token.Without) => false`, so a list of equivalent mutants survives edits elsewhere.
- `run.go` builds the test binaries of the code as it is, lists their tests, sorts them into stages (the parser's hand-written tests, `TestParse_corpus`, `TestGenerate`, `TestRuleCoverage`, the seeds of `FuzzParse` and `FuzzGenerate`, `TestParse_mutants`, `/internal/sql/shapetest`, and the other packages whose tests depend on the mutated one), and times each stage. For each mutant it writes the mutated file into a directory of its worker, compiles each test binary with `go test -c -vet=off -overlay`, and runs the stages in order with `-test.failfast`. The time limit of a stage is 20 times its measured time, at least a minute. A watchdog kills a test binary whose resident memory passes 4 GiB, which a mutant that loops while it appends reaches within seconds. A failure of the ownership check inside the parser's tests, with no disagreement with SQLite, is counted for the ownership layer.
- `report.go` counts by package and file and lists every survivor that `equivalent.txt` does not explain.

The run happens on a copy of the worktree in the scratchpad (`rsync -a --exclude .git`), so that I could change the code and the tests in the worktree while it ran.

### Why

An outside tool would have needed a download from outside the repository and would not know the layers of this project's tests or the tables of the parser. The overlay keeps the repository untouched and lets ten mutants run at once on one module.

### What worked

A smoke run of 63 mutants of `isName` took 55 s with 10 workers, 31 s of which were the baseline. The memory watchdog fired on its first chance: `token/token.go Tokens remove statement: return tokens =>` loops forever appending, and was killed "over the memory limit of 4096 MiB".

### What didn't work

The first full run failed at once: `mutation: the tests of hand-written in internal/sql/token fail on the code as it is: killed fork/exec ../run1/baseline/internal_sql_token.test: no such file or directory`. The output directory was relative, and the test binaries run in the directory of their package. `-out` is now made absolute.

The first version made neighbours like `token.Add => token.kinds`, of an unexported name in another package, which never compile, and removed the only element of a `case` list, which is a syntax error. Both are no longer generated.

### What I learned

Each mutant adds about 3 MB to the Go build cache (180 MB for the smoke run of 63), so a full run of about 7,000 adds about 20 GB. `-gocache` points the builds at a cache of their own that can be deleted afterwards.

The shape check spawns child processes, and with ten workers in that stage at once the machine's load average reached 76. The stage is then several times slower than its baseline, which the time limit of 20 times absorbs.

### What was tricky

Keys. A key with only the literal, `aiClass constant: 2 => 1 #3`, depends on how many equal literals come before it. Elements of a table are now named by their index (`aiClass[89] constant: 2 => 1`), and a constant or a neighbour shows its parent expression when that is short (`Next constant: i = 1 => i = 0`).

### What warrants review

`/internal/sql/mutation/run.go`: the attribution of a failure to a layer, and the classification of a time-out (`panic: test timed out`, the context's deadline, or the watchdog).

### Future work

None.
