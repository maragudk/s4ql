# Diary: The review round of the engine of slice 1

Two independent reviewers checked the branch `engine-slice-1`, which holds the first SQL engine in `/internal/engine`. They found no correctness bug, but found tests that prove less than they seem to, and Markus reversed one design choice: a syntax error never depends on the state of the database. This diary is the round of changes that answers the review, on the same branch. The diary of the engine itself is `/docs/diary/2026-11-18-engine-slice-1.md`.

## Step 1: The brief

**Author:** engine-review-builder

### Prompt Context

**Verbatim prompt:** "You are a builder for s4ql (`maragu.dev/s4ql`) [...] The branch `engine-slice-1` holds the first SQL engine (`internal/engine`), written by an earlier builder and not merged. [...] Two independent reviewers checked the branch. They found no correctness bug in the engine, confirmed every test number of the first builder by running it again, and caught all 11 faults they planted themselves. They also found that some tests prove less than they seem to, and one design choice was reversed by the product owner. Your job is this round of changes, and no more. [...] 1. Take `stmt.StartedTable` out of production. [...] 2. The simulation cannot see a read transaction left open. [...] 3. The differential fuzzing is seed fuzzing. [...] 4. A refused statement is never checked. [...] 5. Events on panic paths are wrong. [...] 6. Operators hold guesses about SQLite beyond slice 1. [...] 7. `internal/sql/stmt` logs, and does not assert, how many texts escape comparison. [...] 8. Assert what the simulation workloads reach. [...] 9. Small records. [...] The driver is not part of this work. The engine keeps push-style rows. The names of SQLite's built-in virtual tables stay "not supported"."

**Interpretation:** Nine points, each with its own proof: remove the lookup of a started table from the engine and keep its classifier as test code; make the end of a simulation run take every read connection and plant a bug that leaves a read open; add a fuzz target over SQL text on a database with tables; check the statements that the engine refuses; log one event with a fault outcome on every panic path; make the unreachable branches of the operators internal errors; assert the excused counts of `stmt`; assert minimum counts of what the simulation reaches, and make the default workload reach transactions more often if that is cheap; and three small records in the roadmap and the todo.

**Inferred intent:** Trust in correctness: every test should prove what it seems to prove, and a fault that a test claims to find should be shown to be found.

### What I did

Checked out `engine-slice-1` in my worktree and merged `origin/main` into it; the merge had no conflicts. Read `AGENTS.md`, the READMEs of `/internal/engine` and `/internal/sql/stmt`, the spec's rule on syntax errors, the roadmap, the section "The SQL front end" of `/docs/todo.md`, and the engine's code and its differential and simulation tests.

### Why

The changes reach into tests that the first builder wrote with care; they have to keep what those tests prove while closing the gaps.

### What worked

The merge of main was clean, and `go build ./...` and `go vet` pass on the merged branch.

### What didn't work

Nothing yet.

### What I learned

`stmttest` is already a package for tests only, which imports the SQLite binding, so it is a natural home for the classifier of a started table: the tests of `stmt` and of `engine` can both use it.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.
