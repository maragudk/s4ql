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
