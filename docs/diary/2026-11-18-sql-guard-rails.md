# Diary: Guard rails around the comparison with SQLite

Two audits of the tests of the SQL front end found no dishonest test, but found that the guard rails around the comparison with SQLite can be weakened without any test failing: CI never runs the full comparison, the number of texts that escape comparison is only logged, the mutation runner has no test, `make fuzz` can skip silently, the corpus is never re-checked in CI, and the cross-check with `mattn/go-sqlite3` covers less than its README says. This work closes those gaps, on the branch `sql-guard-rails`.

## Step 1: Requirements and scope

**Author:** sql-guard-rails-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Two independent audits of the tests found no dishonest test in the SQL packages, but they found that the guard rails around the comparison with SQLite can be weakened without any test failing. Both gaps were proven by planting a fault. Your job is to close them. This is a small, focused task; do not widen it." The six points: "1. CI must compare with SQLite on the whole corpus." "2. The number of texts that escape comparison must be asserted, not logged." (but "Leave `internal/sql/stmt` alone") "3. The mutation runner has no test of its own." "4. `make fuzz` can skip silently." "5. The corpus is never re-checked in CI." (in the scheduled workflow with the `sqlite-verify` job) "6. The cross-check with `mattn/go-sqlite3` covers integers and NULL only [...] Extend the cross-check to text, blob, and real values if that is straightforward. If it is not, make the README say what the test covers." Out of scope: `site/`, the top-level `README.md`, `docs/spec.md`, `docs/roadmap.md`, `internal/sql/stmt`, `internal/engine`, the storage core, and the stores; and "Do not add `oracle` or `parsetest` to the mutation tester's packages; add one line on it to `docs/todo.md`".
**Interpretation:** Add a `make` target and a CI job that run `./internal/sql/...` without `-race` and `-asan`; pin the counts of texts on which SQLite does not say, and the shape check's outcomes and detection rates, in the full runs; test `runIn` and `runStage` on a fake module; make `make fuzz` fail when listing fails or finds nothing; run `corpus/update -check` in the scheduled workflow; widen the cross-check or correct the README.
**Inferred intent:** Make each guard rail fail loudly when it is weakened, so that the trust in the comparison with SQLite does not rest on someone reading a log.

### What I did

I read `/AGENTS.md`, `/Makefile`, `/.github/workflows/ci.yml` and `/.github/workflows/sqlite-verify.yml`, the sampling in `/internal/sql/parser/parser_test.go`, `/internal/sql/parser/grammar_test.go`, `/internal/sql/parser/lalr_test.go`, `/internal/sql/shapetest/check_test.go`, `/internal/sql/shapetest/generate_test.go`, and `/internal/sql/shapetest/mutate_internal_test.go`, and `/internal/sql/mutation/run.go`. I ran `go test -count=1 -v ./internal/sql/...` once without the race detector to read the numbers that the full runs log: the corpus has 308 texts on which SQLite does not say, the mutants 745, and the generator 5,357 of 60,000.

### Why

The counts to pin must come from a full run on this code, not from the brief.

### What worked

The full run of `./internal/sql/...` passes on `origin/main` at `4f77693`.

### What didn't work

Nothing yet.

### What I learned

`make test` runs without `-race`, so it already runs the full comparison locally; only CI, whose reusable test workflow uses `-race`, never does. The shape check's counts are deterministic: its sharding uses a fixed number of shards, and the mutations of `TestCheck_mutations` choose their sites by position, with no randomness.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.
