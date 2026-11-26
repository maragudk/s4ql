# Hegel

The decisions and notes of the property-based tests with Hegel (`hegel.dev/go/hegel`), an addition to the seeded generators, the simulation, and the fuzz tests, which stay the main checks. The trial and its numbers are in [the note of the trial](../research/hegel-trial.md). Two tests are built: `TestHegel_cases` in `internal/expr/hegel_test.go`, which checks the evaluator through `checker.check`, and `TestHegel_series` in `internal/engine/hegel_test.go`, which checks series of statements on the engine against SQLite through `pair.step`.

## Landing

- **The dependency is pinned at `v0.10.3`, because its API is in beta.** A release of the form `0.N.0` may change the API, so the version is moved on purpose, after its changelog is read, and never by a blanket update of the modules.
- **The series draw text since the landing, by its builder on 2026-11-26.** After the engine computed `||`, `lower`, and `hex` (#184), the generator of `TestHegel_series` writes them where the pass supports them: a column of text in SET takes an expression of text one time in three, and a comparison of a column of text takes one on its value side one time in three and holds the column inside one, such as `lower(c1)` or `c1 || ?`, one time in four. Operands are columns of the type, literals, and parameters, NULL among them, and, one time in 20, a column or a parameter of any type, so now and then of another, which s4ql refuses where SQLite converts it. The forms that the pass refuses from the text, such as a number in a concatenation or parentheses around a call, are not written, since `pair.step` takes every statement of a series as supported. In a run of 200 series, about 190 of 2,700 statements hold text, some of their selects give rows, and the refusals of a column and of an argument in text are reached; the run takes about 6 seconds, where it took 5.5.

## Open

- **The next places for a Hegel test, in the order of what they are expected to give:**
  1. The storage core (`internal/core`): a series of commits, restarts, replays, and catch-ups against a model of the log, where today's failures arrive as long simulations. The core builds for the browser and Hegel does not build for `js/wasm`, so the file needs `//go:build !(js && wasm)`.
  2. The driver (`maragu.dev/s4ql` through `database/sql`): the series of `TestHegel_series` through the driver, with transactions.
  3. Laws of the evaluator that need no SQLite and that no test states: a parameter or a column replaced by its value as a literal gives the same answer, a case that succeeds under a length limit succeeds under every larger one, and `Bound.Type` agrees with every value of `Eval`.
  4. The commit format (`internal/format`): encoding and decoding a commit object as a round trip, and a decoder that refuses every changed byte, beside its fuzz test.
- **What Hegel missed in the trial: the faults that need related values.** Its generators draw each value on its own, so a fault that needs two values to meet, such as more rows than a LIMIT sorted by no index (`limitFirst`), or a key and a value that agree, is found only where the generator draws the relation on purpose. The seeded generators and their profiles do that, and a Hegel test does not count on it.
- **The series of `TestHegel_series` draw no text longer than 8 bytes,** so they reach `string or blob too big` only by an update that doubles a text again and again. The cases where s4ql and SQLite differ there are pinned in `TestEngine_text`.
