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
