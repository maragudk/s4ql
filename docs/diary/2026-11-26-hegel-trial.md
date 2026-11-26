# Diary: a trial of Hegel beside the seeded generators

A bounded trial, on the branch `hegel-trial`, of property-based testing with Hegel (`hegel.dev/go/hegel`) as an addition to the seeded generators of `internal/sql/stmt/stmttest`, the expression generator of `internal/expr`, and Go's fuzzing. Nothing existing is replaced or changed. The result, with the numbers, is in `/docs/research/hegel-trial.md`.

## Step 1: What Hegel needs, and the check of the evaluator

**Author:** builder (hegel-trial)

### Prompt Context

**Verbatim prompt:** "The task: a bounded trial that measures whether Hegel adds value beside what exists. Nothing existing is replaced or changed. [...] 1. Find out what Hegel for Go needs to run [...] Pin an exact version. 2. Choose two places where Hegel is most likely to add something the seeded generators and the fuzz tests do not give, and write a Hegel test for each. [...] 3. Measure the value with faults [...] 4. Write the result to `docs/research/hegel-trial.md`". A later message from the lead: "Markus has set the bar for adoption. His words on 2026-11-26: \"I've heard great things about Hegel, so if there's anything to be gained from it as an additional safety net, I think we should adopt it.\" [...] Write the two Hegel tests to the quality of tests that land, not as throwaway [...] In the note, add a concrete proposal for landing them as an addition".
**Interpretation:** pin Hegel, write two Hegel tests that could land (one pure, one stateful), measure them against planted faults beside the existing checks, and write the note with a proposal.
**Inferred intent:** decide, on numbers, whether Hegel is worth a dependency as an extra safety net, and have the tests ready if it is.

### What I did

Added `hegel.dev/go/hegel v0.10.3` (2026-11-24; it brings `github.com/ebitengine/purego` at an alpha pseudo-version and `golang.org/x/sys v0.48.0`). Read its README, CHANGELOG, `runner.go`, and `internal/libhegel`: the Go library loads a prebuilt Rust engine, `libhegel`, embedded in the module for linux amd64 and arm64, darwin arm64, and windows, through `purego` without cgo. It ran here at once, with nothing installed.

Wrote `/internal/expr/hegel_test.go`: `TestHegel_cases` draws a typed tree of arithmetic or text with Hegel's own generators, and `TestHegel_generator` drives the existing `generator` of `generator_test.go` through a `rand.Source` whose numbers are Hegel draws. Both check each case with the existing `checker.check`, so the oracle is the one `TestDifferential` uses. Both run derandomized, with no example database and without the health check `TooSlow`, which depends on the machine.

Planted ten faults by hand in `/internal/expr/expr.go` on a throwaway copy (a script outside the repository applies one, runs a measuring test, and restores the file), and measured the seeded `TestDifferential` cases, Hegel's own generator over five seeds, the bridge, and `FuzzExpr`.

### Why

The evaluator has the clearest property in the project (SQLite's value, bit for bit, or a justified error), a ready oracle, and a fast case, so it shows what Hegel's generation and shrinking give with the least noise.

### What worked

Hegel's shrinking. Every fault that its generator found was shrunk to two to eleven nodes, most to two or three, with no argument and only the column that matters: `nullShort` from 27 nodes in the seeded case to `(a + (b / b))`, `subOverflow` from 19 nodes to `-?` with the argument 1.

### What didn't work

The first generator wrote `-(c)` and `(-5)`, which the pass refuses ("not supported: parentheses"), so most negations were rejected as no case. Hegel's statistics leave rejected cases out, so this showed only in a probe of the checker with those texts. Fixed by writing the operand of a negation without parentheses and never as a literal, and a negative literal without parentheses.

`TestHegel_generator` first failed with `FailedHealthCheck: LargeInitialTestCase`: the existing generator, fed draws that shrink toward 0, takes its rare branches at 0 (`g.r.IntN(30) == 0` is the deep tree of 900 operators). With the check suppressed it runs, but it found none of the ten faults in 1,000 cases: Hegel's draws of a uint64 are far from uniform, so the generator's odds are lost. It was removed in commit `926d01bc`; only `TestHegel_cases` stays.

`FuzzExpr` found the faults only through its seed corpus, which is the generator's cases, and reported them unminimized: `failure while testing seed corpus entry: FuzzExpr/seed#75`.

### What I learned

A generator written for `math/rand` cannot simply be handed to Hegel: its choices must be written so that the smallest choice is the simplest case, which the existing generators do not do. Hegel's own generators find faults of one value well and faults of two related values (the smallest integer divided by -1, a text exactly at the length limit) poorly, unless the generator draws those relations, as the seeded one does.

### What was tricky

Hegel's draws are not i.i.d. by the weights a generator gives: `WeightedBooleans(1/3)` for the text trees gave about half text. The run is fine, but odds in Hegel are a hint, not a measure.

### What warrants review

The generator `drawCase` and its comments in `/internal/expr/hegel_test.go`; the edges of the length limit (`drawEdge`) were added after the first measurement missed every fault of the limit.

### Future work

None beyond the note.

## Step 2: Series of statements on the engine, and the planted bugs

**Author:** builder (hegel-trial)

### Prompt Context

**Verbatim prompt:** as in step 1.
**Interpretation:** the stateful half of the trial: a Hegel test where today's failures arrive as long seeded sequences.
**Inferred intent:** see whether Hegel's shrinking turns those sequences into something a person reads at a glance.

### What I did

Wrote `/internal/engine/hegel_test.go`: `TestHegel_series` draws 1 to 30 statements on two tables with Hegel's generators (`series`), runs each through the existing `pair.step` of `/internal/engine/differential_test.go`, and compares the tables at the end, with restarts of the instance as `differentialFind` has them. Ran it at `main`, where it first failed on the generator's own mistakes, then passed. Measured eleven planted bugs of the engine with a throwaway test (kept in `/Volumes/990/s4ql/hegel-trial/`, never committed): `differentialFind` on the profile of each bug until the first sequence that finds it, beside the committed Hegel test, and `oldValues` on three more seeds.

### Why

A failure of `TestDifferential` or of the simulation is a seed and every statement before it, in the generator's odd spellings; that is the place the brief named as most likely to gain.

### What worked

Shrinking. Nine of the eleven bugs came out as two to four plain statements, where the seeded check reported 16 to 38. `starKeyFirst`: `create table t(c0 int, k int primary key) strict, without rowid` and `delete from t returning *`, where the engine gives the columns as `k INT, c0 INT`.

Hegel's first failure at `main` was itself a two-statement case: `create index i1 on t(k, k)`, which s4ql refuses with `not supported: column named twice`, and the series called supported. The same followed for `order by k, k`. Both were mistakes of the generator, fixed by drawing the columns of an index and of an ORDER BY without repeats.

### What didn't work

The first run at `main` was not deterministic: `newInstanceLimited` draws the directions of the scans from a random seed unless the flag `-directions` gives one, and logs it at a failure, which printed thousands of lines `an instance of the engine drew the directions of its scans from the seed ...`. Fixed by drawing the directions from Hegel and setting the flag for the series.

`limitFirst` was not found in 200 series: it needs more rows than the LIMIT, sorted by no index, which a short series rarely builds.

Shrinking is slow on a busy machine: 2,700 to 12,700 more series and 16 seconds to 4.5 minutes for one failure, against 7 seconds for a clean run of 200.

A `zsh` parse error (`(eval):36: parse error near 'time'`) from a command that began with an edit by `python3` and ended with `time go test` meant the edit never ran, and the dead loop over `rand.Perm(0)` and a wrong comment stayed in the first commit; the review below found them.

### What I learned

The oracle of `TestDifferential` lifts out cleanly: `pair.step` takes a `stmttest.Call`, so a new generator needs no new rules. The helpers take `testing.TB`, which is what made a test-case-scoped `testing.TB` possible later.

### What was tricky

`pair` and `instance` register cleanups, temporary directories, and fatal errors on the `testing.TB` that they are given. Given the test that runs Hegel, every series of a shrinking keeps its instances until the end of the test, and a fatal error calls `Goexit` from inside Hegel's callback.

### What warrants review

`runSeries` and `seriesT` in `/internal/engine/hegel_test.go`, and the model of the shapes in `series.createTable`.

### Future work

The next places for Hegel are listed in `/docs/research/hegel-trial.md`.

## Step 3: Self-review, and the fixes

**Author:** builder (hegel-trial)

### Prompt Context

**Verbatim prompt:** as in step 1.
**Interpretation:** review the diff, fix what matters, and measure the tests as they would land.
**Inferred intent:** that the numbers in the note are those of the committed tests.

### What I did

Ran the review skill over `main...hegel-trial`, the checklist of `hegel-review`, and an adversarial sub-agent that read only `/internal/expr/README.md` and `/internal/expr/expr.go`. Then: gave each series a `testing.TB` of its own (`seriesT`), whose cleanups and temporary directories end with the series and whose fatal errors panic, which Hegel takes as a failure and shrinks; kept a `-directions` that the person gives; noted restarts for Hegel's report; fixed the model of the shapes (a create table of a table that is there no longer replaces its shape, which had sent later statements to columns that did not exist); removed the dead loop; drew values from the whole type one time in four and the length limit of `TestDifferential` one series in four; and in the evaluator's test, kept rows and arguments within the limit, since a longer one is no case. Measured both tests again as committed.

### Why

The review's first three findings were real: a helper's `Fatalf` would end the whole run with nothing shrunk, a shrinking of 12,000 series would keep 24,000 temporary directories and connections open, and every instance would log a wrong replay hint. The checklist of `hegel-review` flagged the narrowed domains of the engine's values and the fixed length limit.

### What worked

After the fixes both tests pass at `main`, with `-race` (about 15 seconds each) and under `-short` (10 cases each), and leave no temporary directory behind. `golangci-lint run` reports 0 issues on both packages, `go vet ./...` passes, `go mod tidy` changes nothing, and the browser packages still vet for `GOOS=js GOARCH=wasm`.

### What didn't work

Nothing failed in this step.

### What I learned

The adversarial review listed what the measurements showed: an independent draw seldom makes related values, such as the smallest integer and -1, or a length exactly at the limit. It also listed laws that need no SQLite, which are in the note as a next place.

### What was tricky

Hegel prints each draw with the source line of its call, which reads poorly for a line with two draws or a draw in a condition; the notes of the series carry the readable report.

### What warrants review

That the numbers of the tables in `/docs/research/hegel-trial.md` are those of the measurements after the fixes, where it says so.

### Future work

The proposal in the note; nothing else.

## Step 4: Landing, after the merge of `main`, with text in the series

**Author:** builder (hegel-tests)

### Prompt Context

**Verbatim prompt:** "Your task is bounded: take the finished trial branch of Hegel (property-based testing with shrinking) through a merge of `main` and its pull request, as an addition to the existing checks. [...] 1. Merge `origin/main`. [...] Make the two Hegel tests compile and pass against `main` as it is now. `TestHegel_series` goes through the existing `pair.step`: where the pair's interface changed, follow it; do not copy rules of the pair into the Hegel test. 2. Decide whether the series now also draw text expressions. If the trial's generator of statements can be widened to `||`, `lower`, and `hex` in a small change that keeps the test at about its present run time, do it; otherwise leave it and record it as open. [...] 5. Record what is open in a new file `docs/todo/hegel.md` [...] 6. Push `hegel-tests`, open a pull request [...] and land it"
**Interpretation:** merge `main`, make the two tests pass there, widen the series to text if it is cheap, record what is open, and land the branch once every check, the sanitizer jobs among them, has passed.
**Inferred intent:** Hegel becomes a standing extra safety net beside the seeded checks, covering what `main` computes now.

### What I did

Renamed the branch to `hegel-tests` and merged `origin/main` (#181 to #184). The merge had no conflict, and both tests compiled and passed at once: `TestHegel_series` calls only `pair.step`, `newInstanceLimited`, `newPairWith`, and `stmttest.LengthLimit`, which #184 left as they were, and it never used `textRefusal`.

Widened the generator of `/internal/engine/hegel_test.go` to text: `series.text` writes `||`, `lower`, or `hex` of operands that `series.operand` draws (another expression of text up to two levels, a column of the type, a literal, or a parameter, NULL among them, and one time in 20 a column or a parameter of any type); `series.textOf` writes one that holds a given column. A column of text in SET takes an expression of text one time in three, and a comparison of a column of text takes one on its value side one time in three and holds the column inside one one time in four. Wrote `/docs/todo/hegel.md` and indexed it in `/docs/todo.md`.

### Why

`pair.step` already judges text through `checkArithmetic` and its refusals, so drawing text costs only the generator, and the rules stay in the pair. Only forms that the pass supports are written, since the series marks every statement as supported, and a statement that the pass refuses would fail as "the engine does not support a statement that the pass supports".

### What worked

A probe that counted the outcome of each statement (in a throwaway copy of the file) showed, for 200 series: about 190 of 2,700 statements with text, 12 selects of text that gave rows, and the refusals `argument 1 in a concatenation, which is a real` and `column of type REAL in a call of lower` reached. At the first weights (one in five, one in seven, one in 50) only about 120 statements held text, no select of text gave a row, and no refusal of text came, so the weights were raised. The run takes about 6 seconds, where it took 5.5. Both tests pass ten times in a row (`go test -count=10 -run '^TestHegel_' ./internal/expr/ ./internal/engine/`, 50 s for the engine) and under `-race` (16 s each). `make lint` and `make check-browser` pass.

### What didn't work

The first version of `where` drew the column's side before the value's and then swapped them one time in five, which the review found: with `textOf` the column's side can hold a parameter too, so the arguments were appended in the wrong order for `v op left`, and `hex(?)` could get the text and `c1 || ?` the blob. Both instances bind the same way, so it gave no false failure, only other cases than the generator meant. Fixed by drawing the sides in the order of the text.

The first CI run of the pull request failed in the job `crosscheck`, at its step "Check that go.mod and go.sum are tidy" (`go mod tidy && git diff --exit-code go.mod go.sum` in `/internal/sqlite/crosscheck`): that module requires the main one, so its `go.sum` needs the sums of Hegel, `purego`, and `x/sys` too. The trial had tidied only the main module. Fixed by `go mod tidy` there; `internal/simlint` needed nothing.

### What I learned

Hegel's statistics need a probe to read where the cases go: the `Event`s are not printed by a passing `go test`, and the outcomes per kind of statement were what showed that the first weights reached no row and no refusal of text.

### What was tricky

A comparison needs a column on one side, or the pass refuses it as `value compared with a value`, so the side of the column is either the column or `textOf` of it, never a free expression of text.

### What warrants review

That `series.operand` writes only forms that the pass takes: no number, NULL, or blob literal in `||` or `lower`, no parentheses around a call or its argument. And the results of `asan` and `asan-engine` on the pull request, the first run of Hegel's native library under the sanitizer.

### Future work

In `/docs/todo/hegel.md`: the next places for a Hegel test, the faults of related values that Hegel misses, and text longer than 8 bytes, which the series do not draw.
