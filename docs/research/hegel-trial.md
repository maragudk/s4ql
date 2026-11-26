# A trial of Hegel beside the seeded generators

Measured on 2026-11-26 on the branch `hegel-trial`, at `main` of that day (`e671de19`), on the Mac Mini (darwin arm64) while other agents ran long tests on it (a load average of about 20), so the times below are wall times on a busy machine and count for less than the numbers of cases.

Markus set the bar on 2026-11-26: "I've heard great things about Hegel, so if there's anything to be gained from it as an additional safety net, I think we should adopt it." Earlier the same day: "I don't want to switch, I'm looking for it as an addition if it adds value." So the question is not whether Hegel beats the seeded checks, but whether it gains anything beside them: a fault that they miss or find later, a much smaller failing case, or a property that they do not state.

## In short

Hegel adds one thing that nothing in the project gives: a failing case shrunk to its essentials. A bug planted in the engine that the seeded differential test reports after 16 to 38 statements, many of them in odd spellings with comments and quoted keywords, Hegel reports, for ten of eleven bugs, as 2 to 5 plain statements. It finds no planted fault that the seeded checks miss, and it misses some that they find at once: the faults of two related values (the smallest integer divided by -1, a text exactly at the length limit) and a bug that needs a table of many rows. Adopt it as an addition, for series of statements against SQLite and for pure components with an oracle, with generators written for Hegel. Do not try to drive the existing seeded generators with it.

## What it needs

- **The module** `hegel.dev/go/hegel`, pinned at **v0.10.3** (2026-11-24). It needs Go 1.26.1 or later, and adds `github.com/ebitengine/purego` at an alpha pseudo-version (`v0.12.0-alpha.1.0.20261015130536-d456b1bb95e8`) and `golang.org/x/sys v0.48.0` to `go.mod`; `go.sum` gains six lines. Its own tool dependencies (staticcheck, gomutants, go-test-coverage) stay out of our module graph.
- **A native engine, embedded.** The Go library calls `libhegel`, an engine in Rust that is a port of Hypothesis's, through `purego` (dlopen without cgo). The module embeds a prebuilt library for linux amd64 (5.5 MB) and arm64, darwin arm64 (4.8 MB), and windows amd64 and arm64, and writes it to a cache of the user at the first run. Nothing is installed: no Python, no `uv`, no server process, no Rust toolchain. It ran here at the first `go test`. There is no library for Intel macOS.
- **No browser.** The module does not build for `GOOS=js GOARCH=wasm`: `purego` fails with `undefined: syscallArgs`. A Hegel test in a package of `BROWSERPKGS` (`internal/core`, `internal/sim`, `internal/store/simstore`, `internal/sqlite`, the simulator) needs the constraint `//go:build !(js && wasm)` in its file, or `make check-browser` and the browser jobs fail. The two tests of this trial are in `internal/expr` and `internal/engine`, which the browser does not run.
- **Stability.** Hegel is in beta, and a release of the form `0.N.0` may break the API. v0.10.0 (2026-11-19) moved the generators of dates and times into a new package and changed what `Default` draws for two types; v0.10.1 to v0.10.3 followed within five days. Pin the exact version, and move it on purpose.
- **Determinism.** With `WithDerandomize(true)` a run draws the same cases on every machine, which is what CI needs; with `WithDatabase("")` it writes no directory of failing examples beside the package (in CI Hegel disables it by itself); and the health check `TooSlow` is suppressed, since it depends on the speed of the machine. No test depends on time otherwise. Under `-short`, `hegel.Test` runs 10 cases, whatever the test asks for.
- **The race detector and the address sanitizer.** Hegel's own CI runs `go test -race`. The address sanitizer was not run in this trial (`make test-asan` was out of bounds); the library is a shared object that is not instrumented, loaded by dlopen, which ASan normally accepts, but the jobs `asan` and `asan-engine` must show it once on the branch before anything lands.

## The two tests

**`TestHegel_cases` in `internal/expr/hegel_test.go`.** Hegel draws a typed tree of arithmetic two times in three and of text otherwise, at most four levels deep, over a row of the eight columns of the existing `schema`, with literals, columns, and parameters, NULL from a column or an argument, one leaf in 40 of the wrong type for the refusals, and a length limit from 30 to 4,000 bytes one case in four, with texts and blobs whose lengths sit at an edge of the limit. Each case goes to the existing `checker.check`, the oracle of `TestDifferential`: SQLite's value bit for bit, or the error that the exact reference justifies, with SQLite's quirk shown. 5,000 cases take about 2 seconds, and 15 under the race detector.

**`TestHegel_series` in `internal/engine/hegel_test.go`.** Hegel draws a series of 1 to 30 statements on two tables, `t` and `u`, whose shapes it draws at each create table: a key at any place and of any of the five types, one to three other columns, NOT NULL and defaults now and then. The statements are create and drop table and index (with IF NOT EXISTS and IF EXISTS), inserts with and without a list of columns in any order, updates and deletes with a WHERE of comparisons and IS NULL joined by AND, the value on either side, selects of `*` or of columns with ORDER BY and LIMIT, RETURNING on a quarter of the changes, a third of the values as `?`, three values in four from a few of each type, so that keys meet, and the others from the whole type, one value in 100 of the wrong type, a restart of the instance one step in 20, and the length limit of `TestDifferential` one series in four. Each statement goes to the existing `pair.step`, the oracle of `TestDifferential`, and the tables at the end are compared. The directions of the scans, which `TestDifferential` draws at random and logs, are drawn by Hegel too, so that a failure replays exactly. The helpers of `TestDifferential` take a `testing.TB`, and each series gets one of its own (`seriesT`), which closes the instances and removes the directories of the series at its end, and turns a fatal error of a helper into a failure of the series that Hegel shrinks, where `Goexit` on the test would end the run. 200 series take about 7 seconds, and 15 under the race detector.

Both found nothing at `main`.

A third test drove the existing expression generator of `generator_test.go` with a `rand.Source` whose numbers are Hegel's draws. It found none of the ten faults below in 1,000 cases, and was dropped. Hegel's draws are not uniform and shrink toward 0, and the generator's 0 is often its rarest branch (`g.r.IntN(30) == 0` is a tree of 900 operators), so its odds are lost and the smallest case is the largest; Hegel's health check `LargeInitialTestCase` says so. A generator for Hegel must be written so that the smallest choice is the simplest case.

## What was measured

Faults were planted by hand in a copy of `internal/expr/expr.go` that was never committed, and the planted bugs of the engine (`engine.Options.Plant`) were used as they are. For each, the existing seeded check ran its cases in order until the first failure, and Hegel ran the committed test's configuration. The size of a failing case is the number of nodes of the expression, or the number of statements of the series up to the one that fails.

### The evaluator

The seeded check is `TestDifferential` of `internal/expr`, 3,600 cases for each of eight seeds; its failing case is the generator's case as drawn. "Hegel" is `TestHegel_cases` as committed (derandomized, 5,000 cases); "Hegel, 5 seeds" is its generator, before a last change that keeps rows and arguments within the limit, on five other seeds of 2,000 cases each.

| Fault planted in `expr.go` | Seeded: cases to the failure, nodes | Hegel: cases to the failure, nodes shrunk to | Hegel, 5 seeds: found, nodes shrunk to |
| --- | --- | --- | --- |
| `negZero`: the negation as `-1 * x`, which gives -0.0 for 0.0 | 10, 4 | 833, 8 | 5 of 5, 2 to 4 |
| `nullShort`: a NULL on the left skips the right operand and its error | 77, 27 | 160, 8 | 5 of 5, 5 to 11 |
| `mulEdge`: the smallest integer as a product is an overflow | 120, 3 | not found | 2 of 5, 3 and 10 |
| `divMinusOne`: the smallest integer divided by -1 is no overflow | 54, 15 | not found | 1 of 5, 3 |
| `lowerLimit`: `lower` may make a text as long as the limit | 98, 10 | 627, 2 | 4 of 5, 2 |
| `hexLimit`: `hex` may make a text as long as the limit | 164, 7 | 3,069, 2 | 2 of 5, 2 |
| `negZeroDiv`: a division by -0.0 is no division by zero | 149, 23 | 45, 10 | 5 of 5, 3 to 9 |
| `concatLimit`: a concatenation as long as the limit is too long | 45, 5 | not found | 2 of 5, 3 |
| `subOverflow`: the overflow of `-` checked as that of `+` | 5, 19 | 2, 2 | 5 of 5, 2 |
| `lowerUnicode`: `lower` folds every letter, as Unicode does | 15, 2 | 23, 2 | 5 of 5, 2 |

The seeded check finds all ten within 164 cases, in under 30 milliseconds each. Hegel as committed finds seven, and shrinks each to 2 to 10 nodes, such as `-?` with the argument 1 for `subOverflow` (19 nodes seeded) and `lower('AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA')` under a limit of 30 for `lowerLimit` (10 nodes). Its shrinking is good, not perfect: for `negZero` it stopped at `(a * -((a + 0.0) + a))` with `a = 0`, and for `negZeroDiv` at `(a + (-(a + a) / (a * -0.0)))`, where two and three nodes would do and other seeds reached them (`-?` with the argument 0, and `(0 / ?)` with the argument -0.0). The three that it misses need two related values, which the seeded generator draws on purpose (`relate`, `edge`, `edgeOfLimit`) and an independent draw meets rarely. The first version of the Hegel generator, without lengths at the edges of the limit, missed all four faults of the limit.

`FuzzExpr`, fuzzed for up to 30 seconds with each fault, failed at once on an entry of its seed corpus, which holds the generator's cases (`failure while testing seed corpus entry: FuzzExpr/seed#75` for `nullShort`, the same 27 nodes), and Go does not minimize a failing seed. So for these faults the fuzz test is the seeded generator again.

### The engine

The seeded check is `differentialFind` of `TestDifferential`, on the sequences of the profile that weighs what the bug breaks (as `TestDifferential_plantedBugs` and `TestSimulation_plantedBugs` choose it), 48 statements each, until the first sequence that finds the bug; its failing case is the log of the statements before the failure. Hegel is `TestHegel_series` as committed (derandomized, 200 series). An earlier version of its generator, before the fixes of the review, found the same ten and shrank them alike; on three other seeds it found `oldValues` at the 9th, 31st, and 177th series, each shrunk to 3 statements.

| Planted bug | Seeded: sequence that finds it, statements up to the failure | Hegel: series that finds it, its statements, shrunk to | Hegel: cases and time of the shrinking |
| --- | --- | --- | --- |
| `skipUnique` (tables) | 1st, 38 | 3rd, 14, 3 | 4,982, 72 s |
| `listOrder` (tables) | 1st, 21 | 2nd, 2, 2 | 4,083, 28 s |
| `zeroLeftOut` (defaults) | 5th, 22 | 3rd, 7, 2 | 5,471, 55 s |
| `nullDefault` (defaults) | 1st, 16 | 3rd, 9, 2 | 6,087, 43 s |
| `openBound` (where) | 2nd, 29 | 9th, 13, 3 | 8,940, 161 s |
| `changeNothing` (changes) | 1st, 18 | 3rd, 16, 3 | 3,421, 27 s |
| `keepOldKey` (changes) | 1st, 26 | 10th, 11, 3 | 6,781, 57 s |
| `oldValues` (returning) | 1st, 21 | 3rd, 19, 5 | 9,472, 143 s |
| `ascendLast` (order) | 1st, 30 | 139th, 12, 4 | 6,358, 99 s |
| `limitFirst` (order) | 1st, 12 | not found in 200 | |
| `starKeyFirst` (returning) | 1st, 21 | 2nd, 2, 2 | 4,850, 32 s |

The seeded check finds every bug in its first sequences, in milliseconds. Hegel finds ten of the eleven, eight of them in its first ten series and `ascendLast` at the 139th, and shrinks them to two to five statements in plain SQL, eight of them to two or three. It misses `limitFirst`, which needs more rows than the LIMIT, sorted by no index, which a series of at most 30 statements on two tables rarely builds; the profile `order` builds such tables on purpose. For `oldValues` the seeded failure is the 21st statement of a sequence, written as `\fupDAtE -- c\n/**/\r\n"current_date"\f\n--\nSEt\nWITH== /*-*/ 'a', ...`; Hegel's, on one of the other seeds, is

```sql
create table t(k int primary key, c1 real not null) strict, without rowid
insert into t values (0, 0.0)
update t set k = 1 returning *
```

where the engine gives the row `0, 0.0` and SQLite `1, 0.0`. For `skipUnique`, the seeded failure is the 38th statement; Hegel's is three: a create table of `t(k int primary key, c1 int not null)` and `insert into t values (0, 0)` twice, the second of which SQLite refuses with `UNIQUE constraint failed` and the engine takes. For `starKeyFirst` it is two: a create table of `t(c0 int, k int primary key, c2 int)` and `delete from t returning *`, whose columns the engine gives as `k, c0, c2`. Its shrinking is not always minimal: for `oldValues` it kept a create and a drop of a second table that play no part, which makes five statements where three do.

Shrinking is what costs. A clean run of 200 series takes 6 seconds; shrinking one failure took 3,400 to 9,500 more series and half a minute to almost 3 minutes on the busy machine. That is paid only when a test fails, but a failing CI job pays it.

## Where it adds value, and where it does not

It adds value where a failure is a series of steps. The seeded differential test and the simulation report a seed and every statement before the failure, which a person reduces by hand; Hegel reduces it before it reports, and gives the draws with it. That turns a planted or a real fault in the engine from a page of generated text into three lines of SQL.

It adds a second, independent generator. `TestHegel_series` writes its statements in plain spellings over shapes that the profiles of `stmttest` do not fix, such as the key of a table at any place among its columns, so it reaches some combinations from another side. Nothing in this trial shows it reaching a fault that the seeded checks do not; the planted bugs were all found by both.

It adds little where a failure is one value. For the evaluator, the seeded generator finds every fault within 164 cases with cases of 2 to 27 nodes, and a person reads those quickly. Hegel shrinks them further, but misses the faults of related values unless its generator draws the relation, which is what the seeded one already does.

It does not replace the seeded checks. They find the faults first, they reach their required kinds by count (`reached.go`), which Hegel's statistics only report, and a profile sends a whole run at one area. Hegel's weights are hints: `WeightedBooleans(1.0/3)` gave about half of the trees as text, and the cases that a test rejects with `Assume` do not show in its statistics at all.

## What adopting it as an addition would take

1. **The dependency.** `hegel.dev/go/hegel v0.10.3` in `go.mod`, pinned, and moved on purpose. A release of the form `0.N.0` is read before it is taken.
2. **The tests.** The two tests of this branch as they are, in the packages they check, beside the seeded tests and with their oracles: `TestHegel_cases` calls `checker.check` and `TestHegel_series` calls `pair.step`, so a rule of the oracle is written once. They own their generators, which do not touch `stmttest`, and need no recount file: Hegel's tests pin no counts, so a change of their generator moves no number of the seeded tests.
3. **CI.** Nothing to install, since the library is in the module and the module cache already holds it. The tests run in the jobs that already run their packages: under `-short` in `make test` with 10 cases each; in full in the job `test` (`internal/expr`, under the race detector) and in `sql` and `test-engine` (`internal/engine`). The cost is about 2 seconds for the evaluator and 7 seconds for the engine without the race detector, and 15 seconds each with it. Before landing, one run of the branch in CI must show the jobs `asan` and `asan-engine` pass with Hegel's library loaded. The nightly can run more cases (`WithTestCases`) for little, since a clean case is cheap; that is a decision for when the nightly is next changed.
4. **Reading a failure.** Hegel prints each draw with the line of its call, which reads poorly where a line holds two draws or a draw inside a condition. `TestHegel_series` therefore writes each statement with `Note`, which Hegel prints for the shrunk case, and the failure message holds the series. A test of Hegel writes one draw per line where it can.

## Recommendation

Add Hegel, pinned, as an addition, for two kinds of test:

- **Series of steps checked against an oracle**, where the shrinking pays the most: first `TestHegel_series` of the engine, as written.
- **Pure components with an oracle or a law**, where it is cheap: `TestHegel_cases` of the evaluator, as written.

Do not drive the seeded generators with Hegel, and do not count on it for faults of related values; the seeded generators stay the main check. The next places for a Hegel test, in the order of what they are expected to give:

1. **The storage core** (`internal/core`): a series of commits, restarts, replays, and catch-ups against a model of the log, where today's failures arrive as long simulations. Its file needs `//go:build !(js && wasm)`.
2. **The driver** (`maragu.dev/s4ql` through `database/sql`): the series of `TestHegel_series` through the driver, with transactions, whose generated test now reports a seed.
3. **The SQLite-free laws of the evaluator** that the adversarial review of this trial listed and no test states: a parameter or a column replaced by its value as a literal gives the same answer, a case that succeeds under a limit succeeds under every larger one, and `Bound.Type` agrees with every value of `Eval`.
4. **The commit format** (`internal/format`): encoding and decoding of a commit object as a round trip, and a decoder that refuses every changed byte, beside its fuzz test.
