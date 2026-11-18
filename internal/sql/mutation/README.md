# Mutation testing of the tokenizer, tree, parser, and pass after the parser

`internal/sql/mutation` measures the tests of `internal/sql/token`, `internal/sql/ast`, [`internal/sql/parser`](../parser/README.md), and [`internal/sql/stmt`](../stmt/README.md). It changes the code of these packages on purpose, one small change at a time. It runs the tests against each change, and reports every change that no test notices.

A copy of the code with one change is a mutant. The tests kill a mutant when one of them fails on it. A mutant that no test kills is a survivor, and a survivor is one of three things:

- It is equivalent: the code behaves the same with the change. It is listed in [`equivalent.txt`](equivalent.txt) with the reason.
- It is in code that nothing needs. That code is removed.
- It shows a gap in the tests. A new test closes the gap.

Only code that is not a test is changed. The tests are those that `make test` runs: the tests of every package under `internal/sql` that depend on the changed package. They run on the whole corpus.

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
| `-pkgs` | the four packages | The packages to change, separated by commas. |
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

Before you start a run, know these points:

- **A run continues.** A run with the same `-out` skips the mutants that already have a result. A run that was stopped goes on where it stopped.
- **Results belong to one state of the code.** The first run records a fingerprint of the files under `internal/sql`, code and tests, without this directory, of `internal/sqlite`, of `go.mod` and `go.sum`, and of the version of Go. A later run on anything else refuses the directory, and so does `-report`. Remove the directory or use another `-out` then.
- **A run stops on a failure of its own.** If a test binary does not start, or a file changed since its mutants were made, the run stops without a result for that mutant.
- **It is not part of `make test` or of CI.** A full run takes too long for that. See [Results](#results).
- **The build cache grows.** Each mutant adds about 3 MB to the Go build cache, which is about 20 GB for a full run. `-gocache` gives the builds a cache of their own, which you can delete afterwards.
- **The run reads the files while it runs.** For each mutant, it reads the file again from the module that it runs in. Do not edit `internal/sql` during a run. To keep editing, run it on a copy of the tree.

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

The key holds the file under `internal/sql`, the function or the declaration at package level, the operator, and the code before and after the change.

- An element of a table is named by its index: `token/token.go aiClass[89] constant: 2 => 1`.
- A constant shows the expression around it when that is short: `token/token.go Next constant: i = 1 => i = 0`.
- Equal keys in one function get `#2`, `#3`, and so on, in the order of the file.
- A removal has nothing after `=>`.

`-list` prints each mutant as its `file:line`, a tab, and its key.

## How a mutant runs

The tool never changes the repository. It writes the changed copy of the file into the directory of its worker, and the go command compiles that copy in place of the original through `-overlay`. Mutants therefore run in parallel.

Before the first mutant, the tool builds the tests of the code as it is and times each stage. Every stage must pass there.

The tests run in stages, in this order. The first stage that fails kills the mutant, and the report counts the mutant for the layer of that stage.

1. `hand-written`: the tests of the changed package, and then the tests of the parser that are in no later stage
2. `corpus`: `TestParse_corpus`
3. `generator`: `TestGenerate`
4. `LALR automaton`: `TestRuleCoverage`
5. `fuzz seeds`: the seeds of `FuzzParse` and `FuzzGenerate`
6. `mutants`: `TestParse_mutants`
7. `shape check`: the tests of [`internal/sql/shapetest`](../shapetest/README.md)
8. `other packages`: the tests of the other packages under `internal/sql` that depend on the changed package

The tests of `internal/sql/stmt`, when it is the changed package, run in the same layers: `TestCheck_corpus` and `TestAllowList` in `corpus`, `TestCheck_grammar` and `TestGenerate` in `generator`, `FuzzCheck` and `FuzzGenerate` in `fuzz seeds`, and `TestCheck_corpusMutants`, `TestGenerate_mutants`, and `TestGenerate_nearMisses` in `mutants`. Its other tests are `hand-written`. When another package is changed, the tests of `internal/sql/stmt` are `other packages`.

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

## Results

The final run, on the code as it is now, made 7,029 mutants:

| Package | Generated | Not compiled | Killed | Timed out | Equivalent | Unexplained | Score | Score without equivalent |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `token` | 2,514 | 118 | 2,293 | 66 | 37 | 0 | 98.46% | 100.00% |
| `ast` | 421 | 89 | 325 | 0 | 7 | 0 | 97.89% | 100.00% |
| `parser` | 4,094 | 599 | 3,418 | 31 | 46 | 0 | 98.68% | 100.00% |
| total | 7,029 | 806 | 6,036 | 97 | 90 | 0 | 98.55% | 100.00% |

The layers that killed them first: hand-written 5,545, corpus 221, ownership 94, generator 119, LALR automaton 10, fuzz seeds 0, mutants 13, shape check 34, other packages 0. Every survivor is in `equivalent.txt`.

The run took 1 h 15 min with 10 workers on an Apple M4 with 10 cores and 16 GB, while a fuzz run used 4 of the cores. Five of its mutants survived only because the tests that kill them came after the copy of the tree that the run read; a run of those five on the final tests killed them all in 1 min. Planning, which builds and times every stage on the code as it is, takes about 3 min of the total.

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

### What the first run found

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
