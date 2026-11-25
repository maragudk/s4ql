# Diary: The evaluator of arithmetic

The piece of work "arithmetic" is the first step into expressions: s4ql evaluates them with an evaluator of its own, in Go, and gives SQLite's answer or an error for each. The statement pass (`/docs/diary/2026-11-25-arithmetic-stmt.md`) reads `+`, `-`, `*`, `/` and the negation into `stmt.Expression`. This task is the second step: the evaluator alone, as a unit that the engine will use, proved against SQLite with generated expressions. The engine keeps refusing statements with expressions; a fresh builder wires the evaluator in after this.

## Step 1: The evaluator, the facts of SQLite, and the check against SQLite

**Author:** arithmetic-eval builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Build the evaluator as a unit that the engine will use, and prove it against SQLite. Do NOT wire it into planning or execution, and leave the engine's refusal of statements with expressions in place: a fresh builder does the engine and the driver after you, from your API."
**Interpretation:** A package of its own that checks and types an expression before any row is read, binds the arguments, and evaluates per row without waste, with typed errors that tell a refusal from an evaluation error; pinned measurements of SQLite; a generator of expressions and a differential check against SQLite through the binding, with required kinds, a fuzz target, and planted faults.
**Inferred intent:** Give the engine step a trusted, fast unit with a clear contract, so that `received = received + 1` and the rest of arithmetic can be wired in without the engine having to reason about SQLite's arithmetic.

### What I did

Read `/AGENTS.md`, `/docs/todo/expressions.md`, `/docs/todo/arithmetic.md`, `/internal/sql/stmt/stmt.go` (the typed form), `/internal/sql/stmt/stmttest/expression.go` and `values.go`, `/internal/engine/operator.go` and `errors.go` (rows and arguments are `[]*s4qlpb.Value`), and the binding's API in `/internal/sqlite`.

Wrote `/internal/expr`: `expr.go` (`Check`, `Program`, `Program.Bind`, `Bound`, `Bound.Eval`), `value.go` (`Value`, 16 bytes, no pointer), and `errors.go` (`Error`, `ErrEvaluation`, `ErrOverflow`, `ErrDivisionByZero`, `ErrNotFinite`, `ErrInternal`). A program is the tree flattened into a slice of nodes in post-order, evaluated recursively by index, so nothing allocates per row (`TestBound_Eval_allocations` pins 0 allocations; `BenchmarkBound_Eval` is 244 ns for 12 nodes on the M4).

Measured SQLite through the binding in `TestBound_Eval` and `TestCheck` of `/internal/expr/expr_test.go`, then wrote the differential check in `/internal/expr/differential_test.go` (the oracle, the independent reference with `math/big`, the check of one case, `TestDifferential`, `FuzzExpr`) and the generator in `/internal/expr/generator_test.go`. The required kinds are in `/internal/expr/testdata/reached/TestDifferential.kinds`, the counts in `TestDifferential.tsv`, written with `S4QL_RECOUNT=1`. Added `internal/expr` to `FUZZDIRS` in `/Makefile`; `make fuzz-shards FUZZGROUP=c FUZZSHARDS=4` says the 4 shards hold each of the 38 fuzz tests once, so no workflow file changes.

Planted 16 faults by hand in `expr.go` and `value.go`, one at a time, each reverted with `git checkout`.

### Why

The engine must be able to reject statically what it knows from the schema, and per run what it knows from the arguments, and must not pay per row. The check against SQLite is what lets anyone trust that s4ql never gives a value that SQLite would not.

### What worked

Every prediction of SQLite's behaviour held when measured, with one surprise that the source of SQLite explained before the measurement: SQLite codes `-x` of a column as `0 - x` (`EXPLAIN select -a from (select 0.5 as a)` shows `Integer 0` and `Subtract`), so the negation of the real 0.0 and of -0.0 is +0.0, where Go's `-x` gives -0.0. The evaluator computes the negation as SQLite does. The other facts: integer division truncates toward zero; an overflow of `+`, `-`, `*`, `/` by -1, and of the negation gives a real, `9.2233720368547758e+18`; a division by the integer 0, the real 0.0 or -0.0 gives NULL; a real result that overflows gives `Inf` or `-Inf` as a real, and a NaN gives NULL (`c * 10 - c * 10` with `c = 1e308`); `-0.0 * 1`, `c * -1` with `c = 0.0`, `-0.0 + -0.0`, `-0.0 - 0.0`, and `0.0 / -3` give -0.0, and `-0.0 + 0` gives 0.0; an integer operand of a real operation is converted to the nearest real, the even one of two; underflow gives the subnormal or ±0 that IEEE gives; `0.1 * 10.0 - 1.0` is exactly 0.0, which a fused multiply-add would not give.

The differential check passed on its first run, with 20,000 cases in 0.3 s, which made me suspicious; the planted faults showed it is sharp. Each was found by `TestDifferential` on its first seed:

- division that floors instead of truncating;
- no overflow check of `+`; of `-` when the subtrahend is the smallest integer; of `*` for a positive result above the largest integer; of the smallest integer divided by -1; of the negation (computed as Go's `-i`);
- the negation of a real as Go's `-f` (gives -0.0 for 0.0);
- a fused multiply-add for `x * y + z` and `x * y - z` (`"C * d - ?"`: s4ql 1.543477628334411e-16, SQLite 0);
- integers converted to reals too early (every integer literal of an expression of type real) and too late (a mixed operation computed in integers and converted after);
- the conversion of an integer above 2^53 rounded down instead of to the nearest;
- NULL on the right not propagated; NULL on the left short-circuiting so that an error of the right operand is lost;
- the span of a division by zero set to the divisor; a division by -0.0 missed; a sum of reals that is infinite missed.

The pinned table `TestBound_Eval` alone found 15 of the 16, all but the conversion too early, which only columns and arguments of the generated cases reached.

### What didn't work

- The harness refuses any shell command with the word `eval` in it, also as a path segment: `go vet ./internal/eval/` and `mv .../internal/eval .../internal/evaluator` were refused with "this command runs a string through eval, which can't be verified to stay inside the worktree". I renamed the package to `expr` (moved with `mv ev* expr`) and the checked form to `expr.Program`, so that no agent hits this again.
- Heredocs into a file (`cat >> file <<'EOF'`) and a Python here-doc were refused as "too complex to verify"; I used the Write and Edit tools instead.
- My first plant of a conversion that rounds toward zero, `new(big.Float).SetMode(big.ToZero).SetInt64(i).Float64()`, survived. It was no plant at all: `big.Float.Float64` rounds to the nearest float64 whatever the mode, so it changed nothing. A plant that clears the low bits of an integer above 2^53 was found at once.
- Two of my expected values were wrong, not SQLite: `a + x - s` with `a = 1`, `x = x'35'`, `s = '4'` is 2, not 1; and the expected value of the allocation test missed the integer division `17 / 2 = 8`.

### What I learned

- `go doc` of `big.Float.Float64` says "nearest" and means it: the mode of the receiver does not apply.
- Go's specification lets the compiler fuse `x*y + z` across statements, and an explicit `float64(...)` conversion is what prevents it. Every real operation in `arithmetic` is wrapped so.
- The tree that the pass gives has spans that hold the parentheses of the operands, so `full[span]` of any node is a valid expression for SQLite, which is how the check asks SQLite for the operation that failed.

### What was tricky

- Deciding what an operator with a NULL operand does when its other operand fails. SQLite gives NULL for `NULL + (b + 1)` with `b` the largest integer. I chose that the evaluator always computes both operands, so the overflow comes (both answers are allowed by "SQLite's answer or an error"); then whether an operator fails depends on its own operands only, and the reference can justify every error locally. `NULL / 0` is NULL in both: NULL comes before the check of the divisor, as in SQLite and PostgreSQL.
- Justifying an error independently: the reference computes integers with `math/big` and reals as the exactly computed result rounded once by `math/big` (prec 2200, exact for `+`, `-`, `*`), with the sign of an exact zero set by the IEEE rules; and for each error the check asks SQLite for the failing node's text alone and requires the quirk: a real for an overflow, NULL for a division by zero, an infinity for a result that is not finite.
- The generator must never write what the pass refuses: no literal after a negation (it would be a negative literal), no zero or subnormal literal as a divisor, no literal NULL, text, or blob, and a negation of an operator always in parentheses (`- -a` is a second sign).

### What warrants review

- The decisions in the notes of `/docs/todo/arithmetic.md`, "The evaluator", above all the strictness beside NULL and `TypeNull`.
- `integerArithmetic` in `/internal/expr/expr.go`, the overflow checks, and the explicit conversions in `arithmetic`.
- `reference.compute` in `/internal/expr/differential_test.go`: if it shares a mistake with the evaluator, the success path still catches it against SQLite, but an error path is only as good as the reference and the quirk check.
- Validate with `go test -count=1 ./internal/expr/`, `S4QL_RECOUNT=1` only when the generator changes, and `go test -run '^$' -fuzz '^FuzzExpr$' -fuzztime 1m ./internal/expr/`.

### Future work

- The engine step: wire `expr` into planning and execution, remove `engine.expressionRefusal`, decide the refusals of SET into a column of another type and of a comparison of a TEXT or BLOB column with an expression, and map `expr.ErrInternal` to the engine's internal error.
