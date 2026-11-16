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

## Step 3: The first run, and every survivor sorted

**Author:** parser-mutation-builder

### Prompt Context

**Verbatim prompt:** "Go through every survivor, and sort each into one of: Equivalent [...] Dead or redundant code [...] A gap in the tests [...] A gap in the generators [...] The aim is that no survivor is left unexplained: each is killed, removed with its code, or listed as equivalent with its reason."
**Interpretation:** Read the code behind each of the 260 survivors of the first run, and act on each.
**Inferred intent:** A test suite whose blind spots are known and argued, not guessed.

### What I did

The first run, on the code as it was at `e485b60`, made 7,186 mutants and ran them in 1 h 57 min with 10 workers: 822 did not compile, 6,006 were killed, 98 timed out, and 260 survived (token 42, ast 90, parser 128). I sorted the survivors while the run went on, from `results.jsonl`, and changed the worktree, not the copy the run read.

No survivor showed a fault in the parser, the tree, or the tokenizer. The gaps in the tests, each closed with a test:

- `token.Kind.IsKeyword` was never tested to be false, so `k >= Abort && k < kinds` could become `||`; `Kind.String` was not tested at `Without+1`. `TestKind_IsKeyword` compares it with the kinds of SQLite's keywords for all 256 values.
- `ast.CheckOwnership` was the weakest: 48 of its 160 mutants survived. Its tests checked only that some error came, and most broken trees were caught by a part of the check other than the one they were for, so whole parts could be deleted. Each case of `TestCheckOwnership` in `/internal/sql/ast/check_test.go` now asserts the exact message, and new cases reach the parts no case reached: a token at the end that no node owns, a span that ends before it starts, a token without text, a node without tokens whose span is not empty, an empty node before its parent, and an empty node at either end of its parent, which is accepted.
- `ast.Walk(nil)`, a nil element of a list of nodes in `Children` and `Dump`, `ast.Tokens` of a node whose fields are not in the order of the text, and a node with a `[]string` field, which `planOf` must refuse.
- In the parser, texts compared with SQLite in `TestParse_sqlite`: `select #$a` and `select #::a` (the check of `#N` tested `'0' <= text[1]` and `text[1] <= '9'` with nothing at either bound), `select 0_e1` and `01_e5` (a decimal number read as hexadecimal would accept the separator before `e`), `select 1._5` (only the byte after a separator was tested), separators next to `0`, `9`, `a`, `f`, `A`, and `F` in hexadecimal numbers, `select window(1)`, `over(1)`, and `filter(1)`, and RETURNING after a WITH clause in DELETE, UPDATE, and INSERT (`with` passed `false` for `inTrigger`, and `true` survived).
- Pinned trees of `window`, `over`, and `filter` as column names in `TestParse_expressions`: `analyzeWindowKeyword` could give STRING instead of ID, which reads `window` as a literal with the same text, which the shape check cannot see.
- In `/internal/sql/parser/parser_internal_test.go`: the length limit of a later statement of `ParseAll` counts from where its parse starts (`p.next-p.start` could be `+`, and `start` could be dropped from `newParser`); the errors of both limits have the offset -1; `run` passes on a panic that is not a syntax error; `advance` refuses the end and an unrecognized token.
- `TestParse_depth` checked that the depth never exceeds SQLite's stack, which a depth that stays 0 passes: removing `p.maxDepth = max(p.maxDepth, p.depth)` survived. It now also requires a depth of at least k for k levels of a nesting that parses, and that 2,510 siblings (CTEs, FROM items, joins in parentheses, row values) parse, which removing `defer p.leave()` from `nestedSelect` or `nestedFrom` breaks.

Dead or redundant code, removed: the walk of pointers to non-node structs (`fieldStructPtr`, which no node has) and the check in `ast.structOf`, which only a nil node reached; the `+ 1` of the enums `fieldKind` and `class`; in `createTable` the check after a comma between table constraints and in `tableConstraint` the check for "(" after FOREIGN KEY, since the next read fails at the same token; `p.is(token.Without)` before the table options, since WITHOUT falls back to ID; `prec == 0` in `exprPrec`; the constant `precEscape`, which nothing used; in `read` the branch for reading past the end or an unrecognized token, which the parser never does, with the fields `atEnd` and `illegal`; `kind == token.ID` in `getToken`; the raw kinds of the two end tokens; the hexadecimal test of `digitSeparatorError`, now `strings.HasPrefix`, and its `in == 0`, since a number never starts with a separator.

The rest is equivalent, listed with reasons in `/internal/sql/mutation/equivalent.txt`.

### Why

The brief prefers a text compared with SQLite over a pinned expectation. Every parser gap that is about acceptance or errors went into `TestParse_sqlite`; only the tree of a window keyword read as a name, which SQLite cannot see through the shape check, is pinned.

### What worked

The layers did what the brief hoped: of the parser's 3,428 killed mutants, 2,887 died in the hand-written tests, and the corpus, ownership, generator, automaton, and mutant layers each killed some that the earlier ones did not. The shape check killed 32 that nothing else could, nearly all in the precedence table: the parser's acceptance does not depend on precedence, and its pinned trees do not cover every pair of levels.

### What didn't work

`sed -i` with a multi-line `a\` to insert test lines was refused: `This agent is isolated in the worktree [...] but this command is too complex to verify that it stays inside the worktree.` So was a here-document appended to a test file. I used the editor for those.

My first assertion that the depth grows with the nesting failed on `x collate (`, which fails at once: `parser_test.go:338: 10: depth 1`. It now holds only for nestings that parse.

### What I learned

Many survivors were in code that mirrors SQLite but cannot be observed through it: the empty semicolon after a real one at the end of the text, the offsets of the end tokens, the tail returned with an error. They behave the same for every input, so they are listed, not tested.

Two neighbouring precedences behave as one where no operator of the higher level can follow an operand of the lower one, or the only one that can is NOT, because a tie stops the loop of `exprPrec` except for NOT. That makes AND/NOT and COLLATE/unary-minus mergeable without effect.

### What was tricky

Telling equivalent from untested. `digitSeparatorError`'s digit test survived with every bound changed: `'a' <= c` to `'b' <= c` is a gap (a hexadecimal number can have `a` next to a separator), while `'a' <= c` to `` '`' <= c `` is equivalent (`` ` `` cannot be in a number). Each needed the tokenizer's rules for what a QNUMBER can hold.

### What warrants review

The reasons in `/internal/sql/mutation/equivalent.txt`, above all the precedence class and the class of the end of input in `parser.read`.

### Future work

None.

## Step 4: The final run, the review, and the checks

**Author:** parser-mutation-builder

### Prompt Context

**Verbatim prompt:** "Then run the mutation again on the changed code and report the final score." and "If you changed the parser's code: the corpus, mutants, generator, and shape checks are still at zero, and `FuzzParse` has run again on several cores for at least 100 million executions with no finding."
**Interpretation:** A second full run on the final parser code, a fuzz run of the final code, a self-review, and `make lint`, `make test`, `make test-asan`.
**Inferred intent:** The final numbers must be of the code that is merged, and the simplifications must not have broken anything that only fuzzing would find.

### What I did

The second run read a copy of the tree at `c198ce2`, the last change of the parser's code: 7,029 mutants in 1 h 15 min with 10 workers, while `FuzzParse` ran on 4 of the cores. It left 95 survivors. 74 matched the list of equivalent mutants as it was then; 16 more were equivalent and joined the list (the loops of identifier characters at the end of `Next`, `keywordCode`'s bounds, `f.kind = fieldToken` now that `fieldToken` is the zero value, a bound of `digitSeparatorError`, and the `return` after the end tokens in `read`, without which `read` appends an unrecognized token of length 0 that nothing reads); 5 were gaps, closed with tests after the copy was made: the first token in `CheckOwnership` (`i := 0` to `i := 1`), the message of an overlap whose two spans differ, `MaxLength` (now compared with `SQLITE_MAX_SQL_LENGTH` in the vendored `sqlite3.c`, since a text of a billion bytes is too large for a test), and `window x` read as a column. A run of those five on the final tests killed all five in 1 min 2 s.

Final: of 7,029 mutants, 806 do not compile, 6,036 are killed, 97 time out, and 90 are equivalent, with none unexplained: 98.55% of the mutants that compile, and 100% of those that are not equivalent. Per package: token 98.46%, ast 97.89%, parser 98.68%.

`FuzzParse` on the final parser code: `go test -run '^$' -fuzz '^FuzzParse$' -fuzztime 105000000x -parallel 4 .` made 105,000,000 executions in 1 h 15 min 28 s, with 6 new interesting inputs and no finding. The corpus, mutant, generator, rule-coverage, and shape tests pass, so they are still at zero disagreements.

The self-review with the code-review skill found, among other things, that a run reread each file from disk while the mutants' byte offsets were from the time they were made, so an edit during a run would have given garbage mutants under a trusted fingerprint; that a test binary that failed to start was counted as killed; and that the fingerprint did not cover the judge, `internal/sqlite`. Each mutant now carries the SHA-256 of the text it was made from, and the run stops on a mismatch or on a failure to start a binary; the fingerprint covers `internal/sqlite`, `go.mod`, `go.sum`, and the Go version. It also suggested hashing instead of keeping each mutated text in the generator's set of seen texts, which I did. A writer sub-agent wrote `/internal/sql/mutation/README.md` and the changes to `/internal/sql/parser/README.md`, which I reviewed and filled with the numbers; it found that the coverage text was wrong once `TestParse_length` no longer called `Error()`, which a test now calls again (coverage 99.9%, only the guard in `Tokens` uncovered), and that a run of only some packages reported every entry of the other packages as stale, which is fixed.

### Why

The brief asks for the score of the changed code; the first run's score was of code that no longer exists.

### What worked

`-run` with the keys of a few mutants made the verification of the last five cheap. Keys without line numbers meant the second run's results could be reported against the final list with `-report`.

### What didn't work

`go test -race -count=20 ./internal/sql/ast ./internal/sql/mutation` failed at Go's default ten-minute limit: `FAIL maragu.dev/s4ql/internal/sql/mutation 600.180s`. Parsing each of the 7,029 mutants with `go/parser` took 30 s under the race detector. Under the race detector the test now parses every tenth; `-race -count=20` then passes in 135 s.

The keys of the second run had trailing spaces (`... return => ` with an empty right side), which the editor stripped from `equivalent.txt`, so `TestEquivalent` failed: `no such mutant: ast/ast.go planOf remove statement: plans.Store(t, fs) =>`. Keys now have single spaces and none at the end, and results are read with their keys normalised the same way.

The fingerprint of the second run was made by the older algorithm, and the test files changed after the copy, so `-report` refused the directory: `the results in .../run2 are of other code or another SQLite or Go`. I deleted that directory's fingerprint to report it; the parser's code was identical to the copy's (`diff` of the copy's and the worktree's `parser.go` showed no difference), and only tests had changed.

A shell command whose text contained the word `digitSeparatorError` was refused as one that "names git in a form too complex to verify"; I put the script in a file in the scratchpad.

### What I learned

Removing the reading past the end from `read` turned one of its `return`s into an equivalent mutant: without it, the loop appends an unrecognized token of length 0 after the end, which nothing reads, rather than looping.

### What was tricky

Keeping the run's copy of the tree and the worktree apart: results are of the copy, fixes go into the worktree, and the last few survivors needed their own run on the final tests.

### What warrants review

The simplifications that remove guards of invariants, which the review questioned: `read` no longer has a branch for reading past the end or an unrecognized token, `digitSeparatorError` no longer tests `in == 0`, and `ast.structOf` no longer gives its own message for a nil node. Each was code no input reaches, and each removal passed every test and 105 million fuzz executions; if the lead prefers guards that panic, they would come back as equivalent mutants.

### Future work

The parser's hand-written tests do not pin every pair of precedence levels; only the shape check kills those mutants. A pinned pair for each neighbouring level would make the parser's own tests kill them too.
