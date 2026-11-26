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

`TestHegel_generator` first failed with `FailedHealthCheck: LargeInitialTestCase`: the existing generator, fed draws that shrink toward 0, takes its rare branches at 0 (`g.r.IntN(30) == 0` is the deep tree of 900 operators). With the check suppressed it runs, but it found none of the ten faults in 1,000 cases: Hegel's draws of a uint64 are far from uniform, so the generator's odds are lost.

`FuzzExpr` found the faults only through its seed corpus, which is the generator's cases, and reported them unminimized: `failure while testing seed corpus entry: FuzzExpr/seed#75`.

### What I learned

A generator written for `math/rand` cannot simply be handed to Hegel: its choices must be written so that the smallest choice is the simplest case, which the existing generators do not do. Hegel's own generators find faults of one value well and faults of two related values (the smallest integer divided by -1, a text exactly at the length limit) poorly, unless the generator draws those relations, as the seeded one does.

### What was tricky

Hegel's draws are not i.i.d. by the weights a generator gives: `WeightedBooleans(1/3)` for the text trees gave about half text. The run is fine, but odds in Hegel are a hint, not a measure.

### What warrants review

The generator `drawCase` and its comments in `/internal/expr/hegel_test.go`; the edges of the length limit (`drawEdge`) were added after the first measurement missed every fault of the limit.

### Future work

None beyond the note.
