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

## Step 2: The six guard rails

**Author:** sql-guard-rails-builder

### Prompt Context

**Verbatim prompt:** (the six points of the brief, quoted in step 1)
**Interpretation:** Build each point, and prove points 1 and 2 by planting the audit's two faults.
**Inferred intent:** Each weakening of a guard rail fails a test or a CI job.

### What I did

- **Point 1.** `make test-sql` in `/Makefile` runs `go test -count 1 -shuffle on ./internal/sql/...` without `-race` and `-asan`, and the job `sql` in `/.github/workflows/ci.yml` runs it. The two READMEs of `/internal/sql/parser` and `/internal/sql/shapetest` say so.
- **Point 2.** `pinned` in `/internal/sql/parser/parser_test.go` pins, in a full run with the default flags, the corpus's 308 texts on which SQLite does not say, the mutants' 745, the generator's 5,357, and the generator's 31,731 texts that parse. `/internal/sql/parsetest/parsetest_test.go` pins the 308 of `TestRun_corpus`. `outcomes` and `pinnedOutcomes` in `/internal/sql/shapetest/check_test.go` pin, for `TestCheck_corpus` and `TestCheck_pairs`, the texts not checked by reason, the statements by way (bytecode, probes, same error, skipped), and those compared in no way but by an error. `TestCheck_mutations` pins detected, undetected, and not compared for each of the 22 kinds of damage. `/internal/sql/stmt` is left alone, as the brief says.
- **Point 3.** `TestRunner_run` in `/internal/sql/mutation/run_test.go` writes a module `maragu.dev/s4ql` with three packages to a temporary directory, calls `plan`, and runs a killed, a surviving, an uncompiled, and a timed-out mutant through `run`, and one whose file changed since it was made, which gives `error`.
- **Point 4.** `make fuzz` takes its directories from `FUZZDIRS`, and fails with `fuzz: cannot list the fuzz tests of DIR` or `fuzz: no fuzz test in DIR`.
- **Point 5.** `make corpus-verify` runs `go run ./internal/sql/corpus/update -check`; the job `sqlite-verify` in `/.github/workflows/sqlite-verify.yml` runs it, and the workflow's paths now include the corpus, `parse.y`, the tokenizer, and the binding's Go files, which the update tool uses.
- **Point 6.** `/internal/sqlite/crosscheck/crosscheck_test.go` reads texts in both bindings, with `TestBindText` and `FuzzBindText`. The binding has no functions for reals or blobs, so `/internal/sqlite/README.md` now says that the cross-check covers integers, texts, and NULL only, and no longer claims "statement reuse", which no test of the cross-check covers.
- `/docs/todo.md` has the line on mutating `oracle` and `parsetest` as a possible later check.

### Why

Each point closes one gap that the audits proved.

### What worked

The proof of point 1: with the parser made to fail on `ON INSERT CASCADE` (a `p.fail()` after `ON INSERT` when `CASCADE` follows, in `references` of `/internal/sql/parser/ddl.go`), `go test -race -count=1 ./internal/sql/parser ./internal/sql/shapetest ./internal/sql/parsetest` passed, and `make test-sql` failed in `TestParse_mutants`, `TestParse_corpus`, `TestGenerate`, `TestRuleCoverage`, `TestCheck_mutations`, both `TestCheck_corpus`, with `rejects what SQLite parses: "CREATE TABLE c(j REFERENCES p ON INSERT CASCADE)"`. The proof of point 2: with `classify` in `/internal/sql/oracle/oracle.go` made to return `NotSaid` for `unrecognized token` past byte 20, `make test-sql` failed with `texts of the corpus on which SQLite does not say: 1850, but the test pins 308`, `mutants of the corpus on which SQLite does not say: 2056, but the test pins 745`, and in `TestRun_corpus`. Both faults were reverted with `git checkout`.

`make test-sql` took 54.9 s of real time on this machine (`/usr/bin/time -p`: real 54.94, user 134.75), with other work running.

### What didn't work

The first pins of `TestCheck_corpus` failed on Linux: `docker run ... golang:1.27.3 make test-sql` gave `bytecode: 60072`, `same error: 9146`, `compared in no way but by an error: 4455`, against 60065, 9153, and 4462 on macOS. A dump of the reasons of "same error" on both showed the one difference, `7 "failed to set lock proxy file"`: SQLite defines `SQLITE_ENABLE_LOCKING_STYLE` 1 under `__APPLE__`, and only then has `PRAGMA lock_proxy_file`. The pin now takes 7 from bytecode to same error on darwin and ios.

A Python one-liner that edited the Makefile was refused by a safety check, and a command that combined `git` with other steps was refused by the worktree's isolation check; plain separate commands worked.

### What I learned

The planted loosening of the oracle does not change the shape check's counts: a text with an unrecognized token does not parse in our parser either, so the shape check counts it as "the parser does not parse it" before it asks SQLite. Only the parser's and parsetest's pins catch it. The generator's count did not change either, since its texts have no unrecognized tokens.

### What was tricky

The pins must hold on every platform where the full tests run. macOS and Linux (arm64, in Docker) are checked; CI runs on Linux amd64, which I could not run here.

### What warrants review

The platform rule in `TestCheck_corpus`. Whether the time limit of one second for the timed-out mutant in `TestRunner_run` is safe: the mutant loops about 2^63 times, so it is over any limit, and the other mutants run with a limit of an hour.

### Future work

None beyond the line in `/docs/todo.md`.

## Step 3: Self-review

**Author:** sql-guard-rails-builder

### Prompt Context

**Verbatim prompt:** (from the brief) "Self-review your diff before you report."
**Interpretation:** Run the code-review skill over the branch, fix what matters, and run the checks.
**Inferred intent:** Hand over a branch that the lead can merge as it is.

### What I did

The review (`code-review`, medium, over `origin/main...sql-guard-rails`) gave 8 findings. I fixed four:

- The pins of `TestParse_mutants` and `TestGenerate` ran only when the flags equalled copies of their defaults, `*mutants == 3` and `*generatedCount == 20000`, so a change of a default would have switched the pin off in silence. `atDefault` in `/internal/sql/parser/parser_test.go` now compares each flag with its `DefValue`. I checked that the pin is live by pinning 5,358 for a moment (`texts of the generator on which SQLite does not say: 5357, but the test pins 5358`), and that `-args -generated=1000` passes without it.
- `/.github/workflows/sqlite-verify.yml` now also runs on `internal/sqlite/options.h`, which the binding that the corpus tool uses includes.
- The comment on the job `sql` in `/.github/workflows/ci.yml` said that the tests use a tenth of the corpus under the race detector; `TestRun_corpus` of parsetest uses all of it. It now says "most tests".

I left four as they are: the 308 pinned in both parser and parsetest (the brief names the parser's place, and parsetest's pin also runs under `-race` in CI), three forms of the pin's failure message (one per test file, each fitting what it compares), the few seconds of `TestRunner_run` in every run, and that `make test-sql` locally does what `make test` does for these packages (it exists for CI).

### Why

A guard rail that switches itself off is the kind of gap this work closes.

### What worked

`go test -count=20` and `go test -race -count=20` of `TestRunner_run` passed (103 s and 81 s), and so did both for `TestBindText`. The pinned tests passed three times in a row with `-count=3 -shuffle on`. `make lint` passed with 0 issues.

### What didn't work

Nothing in this step.

### What I learned

`go test -run X -generated 1000 ./pkg` reads `1000` as a package; a flag of the test binary goes after `-args`.

### What was tricky

Nothing more.

### What warrants review

That Linux amd64, where CI runs, gives the same pinned numbers as Linux arm64 and macOS; the first CI run of the job `sql` shows it.

### Future work

None.
