# Diary: Check the shape of the syntax tree against SQLite

The third of the four checks of the parser in `/docs/todo.md`: the tree of `/internal/sql/parser` is written out again with every operation in parentheses and every clause explicit, and SQLite must treat that text as it treats the original, with the same values for an expression and the same `EXPLAIN` output for a statement. It is built in `/internal/sql/shapetest`.

## Step 1: Requirements and scope

**Author:** tree-shape-check-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "You are the builder for the feature "Check the shape of the syntax tree against SQLite" in s4ql [...] What is **not** yet checked is that the tree has the right **shape**: that operators are grouped as SQLite groups them, and that every clause hangs on the node SQLite attaches it to." The brief has six parts: "1. A printer that exposes the tree's shape", "2. The comparison with SQLite" (same bytecode, same values, the printed text parses and is a fixed point, and a way to compare statements whose tables do not exist), "3. Systematic tests of precedence and association" (every ordered pair of operator forms and a sample of triples, and hand-written statement structure), "4. Proof that the check can fail" (shape mutations and their detection rates), "5. Fix what it finds", and "6. Wire it in" (corpus in the normal run, a fuzz test in `make fuzz`, a library with a README, run times).
**Interpretation:** Write a printer that walks the tree's structure and never the token order, find with SQLite where added parentheses are neutral, compare original and printed text through `EXPLAIN` against a schema built for each statement and through evaluation of each expression with its column references replaced by values, generate operator pairs and triples, damage trees on purpose to measure detection, and wire it all into the tests.
**Inferred intent:** Make a wrong grouping or a misplaced clause in the parser's tree impossible to miss, before an engine computes wrong results from it.

### What I did

I read `/AGENTS.md`, the section "The SQL front end" of `/docs/todo.md`, `/internal/sql/parser/README.md`, the node types of `/internal/sql/ast`, the READMEs of `/internal/sql/oracle`, `/internal/sql/parsetest`, and `/internal/sqlite`, `/docs/research/sqlite-parse-tree.md` (§7, the lowering done by the grammar's actions, which says that parentheses leave no node), and the diary of the parser. The work is on the branch `tree-shape-check`, made from `origin/sql-parser`. Another builder changes `constant.go`, `functions.go`, and the `#N` machinery of the parser at the same time, so everything here is in the new package.

### Why

The check has many parts, and the order matters: the printer first, then the comparison on the corpus, then the generator and the mutations that prove the comparison can fail.

### What worked

Nothing to report yet beyond the reading.

### What didn't work

Nothing yet.

### What I learned

`parse.y` drops parentheses (`expr(A) ::= LP expr(X) RP. {A = X;}`), so wrapping an operation in parentheses is neutral for SQLite wherever an expression stands, except for what SQLite copies from the text: the names of unaliased result columns, the text of a CHECK constraint, the text of a column's DEFAULT, and the declared type.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: The printer, and the comparison on the corpus

**Author:** tree-shape-check-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the brief) "A function that writes a tree out as SQL text **from the tree's structure** [...] Beware the trap: every node owns its tokens, so a printer that just writes all tokens in source order reproduces the original text whatever the shape of the tree, and proves nothing." and "The difficulty you must solve is that SQLite cannot prepare a statement whose tables and columns do not exist".
**Interpretation:** A printer with one function per node type that writes children by role, and a comparison that builds whatever schema a statement needs.
**Inferred intent:** A check that compares most of the corpus for real, and says how much it skipped.

### What I did

`/internal/sql/shapetest/print.go` writes every node type by its fields; operations are wrapped, atoms are not, and a `Paren` keeps its parentheses only around an atom, so the printed text is a fixed point. `checkPrinted` in `/internal/sql/shapetest/check.go` requires that every token is printed once or deliberately left out, which catches a forgotten field. `/internal/sql/shapetest/schema.go` guesses tables and columns from the tree and fixes the guess from SQLite's messages; `check.go` compares `EXPLAIN` programs, executes DDL and compares probes (pragmas and the programs of INSERT, UPDATE, DELETE, and a SELECT of comparisons), and evaluates every expression on its own. `/internal/sql/shapetest/report.go` counts everything by kind, way, reason, and form.

### Why

Bytecode is SQLite's own reading of its own tree, so equal bytecode for a fully parenthesized text is the strongest evidence short of reading SQLite's memory.

### What worked

Parentheses are neutral in bytecode: `select a+b*c from t where a order by 1` and `select (a+(b*c)) from t where (a) order by (1)` give identical programs. The first full corpus run gave 86% of the 83,991 statements compared by bytecode or probes, and zero faults.

### What didn't work

- The first corpus run had faults of the check itself, not of the parser. `TRUE` in an expression was replaced by a value on the printed side and not on the original. The P4 of `VOpen` is `vtab:794F45C1B0`, a memory address. `CREATE TABLE t1(... f TEXT(50)) STRICT` gave `unknown datatype for t1.f: "TEXT ( 50 )"` once printed with spaces: SQLite keeps the type's text.
- `e int                         Always default(5)` has the type INT in SQLite and `int Always` when printed with one space: `sqlite3AddColumn` strips a trailing ALWAYS only from a type of 16 bytes or more. The printer now keeps the spacing of a type from the offsets of its tokens.
- `SELECT [count(*)],y FROM (SELECT count(*), y FROM t1 GROUP BY y)` printed fails with `no such column: count(*)`: SQLite names the column by its text. Such statements are counted as skipped, with that reason (5 in the corpus).
- A fix of the schema that changed the schema and then returned false left the original observed on another schema than the printed text (`table t1 has 3 columns but 1 values were supplied` against `no such column: a`). The loop now always observes the schema as it is.
- `go test` with 10 goroutines used about 2 cores: SQLite serializes its allocations over threads. The corpus and pair tests run in 8 processes of the test binary instead (`checkSharded`); results go back through `encoding/gob`, since `encoding/json/v2` refused the invalid UTF-8 of corpus entries (`jsontext: invalid UTF-8 within "/6984/SQL"`).

### What I learned

The oracle creates `create table "t1" (c)` for ALTER TABLE, so a printed `ALTER TABLE t1 ADD COLUMN c ...` is `NotSaid` with `duplicate column name: c`, as the original is. And an action error such as `IN(...) element has 3 terms - expected 2` comes at an added `)` of the printed text, which makes the printed text `NotSaid`, `StoppedEarly`, with the same message; the check accepts exactly that.

### What was tricky

What SQLite copies from the text into its programs: the text of an unnamed CHECK in its Halt, the step text in Trace, the default's text, the column names. Each needed a mask or a probe that leaves it out, with a reason.

### What warrants review

The masks in `explain` and the fields left out in `tableProbes`; the rule in `compare` that accepts a printed text that stops early with the same message.

### Future work

None from this step.

## Step 3: Pairs, triples, hand-written statements, and mutations

**Author:** tree-shape-check-builder

### Prompt Context

**Verbatim prompt:** (excerpt) "Write a generator that, for **every ordered pair** of operator forms in SQLite's expression grammar [...]" and "A check that never fails proves nothing until it is shown to fail on a wrong tree."
**Interpretation:** Generate every pair and a sample of triples, write statements by hand for the structure of statements, and damage trees on purpose to measure detection.
**Inferred intent:** Evidence that the check would catch a wrong grouping or a misplaced clause, and an honest list of what it cannot catch.

### What I did

`/internal/sql/shapetest/generate.go` has 61 forms as token templates; `Pairs` nests every form in every operand of every form, `Triples` samples three. `TestCheck_pairs` runs them and `parsetest` on them. `Structure` in `/internal/sql/shapetest/export_test.go` holds the hand-written statements. `/internal/sql/shapetest/mutate_internal_test.go` has 15 mutations: rotation of two operators with the tokens in the same order (which covers NOT in and out, postfix operators on the wrong operand, and COLLATE), operand swaps, the NOT of IS NOT, NOT BETWEEN outward, LIMIT, ON to the next join, join operators, ORDER BY and LIMIT of compounds and subqueries, the dangling ELSE, FILTER and OVER, ORDER BY between arguments and window, the WHEREs of an upsert, column constraints, CONSTRAINT name, and digit separators.

### Why

The corpus alone leaves many operator pairs untouched, and a zero-fault run says nothing until the check is seen to fail.

### What worked

Every pair and triple agrees; 3,042 of the 3,721 pairs are compared by bytecode and values. Rotation and swap are detected in 97% of the compared cases, and every other mutation that changes meaning in 85% to 100%.

### What didn't work

- With literal leaves and untyped columns, many mutations went unnoticed for reasons that were the check's: SQLite folds `x OR 5`, `-4 ISNULL`, and `x IN ()`, and a column without a type has no affinity or collation. Leaves became columns of a one-row table `s4ql_v` with typed columns, the schema's columns got types, and the values are compared also through `v = 'A'`, `v < 'b'`, `v = 1`, `v = '1'`, which show collation and affinity. Undetected rotations fell from 82 to 69 of a larger compared set, and every remaining class is explained.
- Probes of tables with foreign keys failed on both sides with `no such table: main.p` (foreign keys are on in the binding), so a moved CONSTRAINT name went unnoticed; the schema now makes parent tables with a key.
- The mutation test first counted a mutation as compared when any expression of the statement was; it now looks at the expressions the mutation changed.

### What I learned

SQLite lowers `x NOT BETWEEN a AND b` to `NOT (x BETWEEN a AND b)`, and NOT LIKE, NOT IN the same way: that mutation is never detected, by design, and the test fails if it ever is.

### What was tricky

Telling a hole in the check from a change that SQLite really treats the same. Each undetected class was looked at by example; the explanations are in the README.

### What warrants review

The list of undetected classes and their explanations in `/internal/sql/shapetest/README.md`.

### Future work

REGEXP, MATCH, and GLOB with ESCAPE are compared only by their error, because the binding cannot register functions.

## Step 4: Self-review, fuzzing, memory, and the sanitizer

**Author:** tree-shape-check-builder

### Prompt Context

**Verbatim prompt:** (excerpt) "as a fuzz test in `make fuzz` (seeded from the corpus; any text that parses goes through the comparison), run long on several cores until quiet; report the execution count and add every finding as a regression case." and "Measure the run times plain, with `-race`, and under `make test-asan`".
**Interpretation:** Review the diff with competing reviewers, fuzz until a run finds nothing, and make the package pass every test mode within CI's budget.
**Inferred intent:** A check that is itself trustworthy and cheap enough to stay in the normal run.

### What I did

The code-review skill found ten points; I fixed seven: the text of an unnamed CHECK is taken as SQLite takes it (between the parentheses, trimmed, dequoted by a copy of `sqlite3Dequote`, comments kept); `pragma table_xinfo` has `hidden` at index 6, not 5, so primary keys were left out of the probes and generated columns broke the UPDATE probe; a repeated expression now returns the fault of its first comparison; INDEXED BY makes its index on its own table; a fix that changes nothing returns false; a fault of `checkPrinted` passes through the oracle too; duplicates no longer print. A writer sub-agent wrote `/internal/sql/shapetest/README.md` from a brief; I reviewed it, and corrected what the code had changed since. Four fuzz runs of `FuzzCheck` with 6 workers found four faults of the check, each now a regression case in `/internal/sql/shapetest/testdata/fuzz/FuzzCheck`.

### Why

The check is only worth what its own mistakes leave of it.

### What worked

The fuzzer: about 30,000 executions per second, most texts do not parse and cost little.

### What didn't work

- Fuzz run 1, after 216,097 executions: `"CREATE TABLE A(A A--\nA)" printed as "CREATE TABLE A ( A A   A )"`: the original's type is `A--\nA`. A comment inside a type is part of it; such statements and expressions are now skipped.
- Fuzz run 2, after 25,381,073 executions: `"CREATE TABLE A(A CHECK(a| b),B CHECK(a|b))"`: two CHECK texts that print alike got different masks. Every CHECK text now has the same mask.
- Fuzz run 3, after 2,358,650 executions: a chain of about 1,300 unary `+` prints deeper than `MaxDepth`: `the printed text does not parse: s4ql: the statement is nested deeper than the limit of 2500`. Now skipped, as is SQLite's `Recursion limit` on the printed text.
- Fuzz run 4, after 96,317,059 executions in 30 minutes: a shallower chain gave `error: Recursion limit` only inside the query that evaluates the printed expression. Also skipped.
- `make test-asan` failed twice. First `shard 6: signal: killed`, and other packages timed out at 10 minutes: 8 processes under the sanitizer in Docker's 4 GB. The shards now run only in the plain mode. Then `signal: killed` again for the package alone: in-process the corpus test reached 2.5 GB, because `SELECT CAST (zeroblob(1E9) AS INT100R)` makes a value of a gigabyte (found by measuring `Maxrss` after each statement). I added `sqlite.Conn.SetLengthLimit` to the binding, with a test, and the check limits values to 100,000 bytes; the peak fell to 91 MB.
- `make lint` failed in simlint: `generate.go:4:2: math/rand/v2 draws randomness outside the host`. `shapetest` is now exempt, as `parsetest` is.

### What I learned

The machine ran at a load of 25 to 45 on 10 cores during this work, from other sessions, so wall times are high; the CPU times are the better measure.

### What was tricky

Memory under the sanitizer: a test that is fine on a laptop is killed in the 4 GB of Docker's VM when other packages run beside it.

### What warrants review

`sqlite.Conn.SetLengthLimit` in `/internal/sqlite/sqlite.go` is the one change outside the new package's code. The exemption in `/internal/simlint/simlint.go`.

### Future work

None beyond what the README lists.

## Step 5: Until the fuzzer was quiet, and the numbers

**Author:** tree-shape-check-builder

### Prompt Context

**Verbatim prompt:** (excerpt) "run long on several cores until quiet; report the execution count and add every finding as a regression case."
**Interpretation:** Keep fuzzing after each fix until a full run finds nothing, and put the final numbers in the README.
**Inferred intent:** Confidence that what the check reports is about the parser, not about the check.

### What I did

Fuzz runs 5 and 6 found two more faults of the check: `WITH A(A)AS(SELECT WITH%0)SELECT 0` prints `( WITH % 0 )`, where WITH after `(` is the keyword that starts a SELECT, so the printer no longer wraps an operation that starts with the name WITH; and a deep chain that reaches `Recursion limit` only under EXPLAIN, now skipped. Run 7 made 123,112,704 executions in 30 minutes and found nothing; the seven runs made about 313 million. I filled in the fuzzing and run times of `/internal/sql/shapetest/README.md` and updated its numbers from a last run of the corpus, the pairs, and the mutations.

### Why

Each finding of the fuzzer was a place where the printer or the comparison did not match SQLite, and each would have made the check report a fault of the parser that was not one.

### What worked

The parser's tree had SQLite's shape everywhere: no fault on the corpus (83,991 statements), on 3,721 pairs and 20,000 triples, on 66 statements by hand, or in 313 million fuzz executions.

### What didn't work

`go test ... -run=FuzzCheck/e3a68107840df925` showed `the printed text does not parse: s4ql: the statement is nested deeper than the limit of 2500`, and the run after its fix showed the same class one level shallower, twice: a printed text gains one level per operation, and EXPLAIN and the evaluating query add their own.

### What I learned

Every place where the printer adds a token is a place where SQLite's reading of the next token can change: WITH after `(`, and the depth of the parser's stack.

### What was tricky

Telling the last depth findings apart: the oracle's prepare, EXPLAIN, and the query around an expression each sit at a different depth.

### What warrants review

`startsWithWith` in `/internal/sql/shapetest/print.go`, and the three places in `/internal/sql/shapetest/check.go` that skip for depth.

### Future work

None.
