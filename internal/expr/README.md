# The evaluator of expressions

`internal/expr` computes the arithmetic of a `stmt.Expression`, the typed form of an expression that [the pass after the parser](../sql/stmt/README.md#arithmetic) gives: `+`, `-`, `*`, and `/` on integers and reals, and a `-` before an operand, over the values of a row and the arguments of a statement. For every expression it gives the value that SQLite gives, of the same type and bit for bit, or an error. It never gives another value.

It does nothing else. It has no comparisons, no functions, and no arithmetic on text or blobs, and it reads no table: the caller resolves the names of the columns and hands it the rows. It depends on `internal/sql/stmt` and `internal/s4qlpb` only, so the engine can import it.

The engine does not use it yet. Until it does, `engine.expressionRefusal` refuses a statement with an expression as `s4ql: not supported: expression`. The decisions behind the package, the measurements of SQLite, and what is left for the engine are in [docs/todo/arithmetic.md](../../docs/todo/arithmetic.md).

## The API

An engine calls the package three times: when it prepares a statement, when it knows the arguments, and for each row.

1. **`expr.Check(e, resolve)`** checks the expression `e` before any row is read and gives a `*expr.Program`. `resolve` is a `func(stmt.Name) (expr.Column, error)` that finds a column of the table. An `expr.Column` is the position of the column's value in a row, from 0, and its declared type, an `s4qlpb.Type`. `Check` resolves every name first, in the order of the text, and returns the first error of `resolve` as it is. So the engine's error for a column that the table does not have, SQLite's `no such column`, comes before any refusal of a type. Then `Check` refuses the first column of type TEXT or BLOB in the order of the text. A column alone, as in `set a = b`, is an expression too, and is refused for the same types.
2. **`Program.Type()`** gives the type of the values, `expr.TypeInteger` or `expr.TypeReal`, and true, for an expression without a parameter. With a parameter, the type depends on the arguments, and it gives 0 and false. A `*Program` never changes after `Check`, so it is safe for concurrent use and can be kept with a prepared statement.
3. **`Program.Bind(args)`** gives an `expr.Bound` once the arguments are known. `args` holds all the arguments of the statement as `[]*s4qlpb.Value`, and each parameter takes the argument of its number, from 1. `Bind` refuses the first parameter in the order of the text whose argument is text or a blob. It checks nothing else: a real argument that is not finite, which the engine refuses before, goes into the arithmetic like any operand. The `Bound` holds `args`, which must not change while it is in use. For an expression without a parameter, `Bind` takes any arguments, also nil, and allocates nothing.
4. **`Bound.Type()`** gives `TypeInteger`, `TypeReal`, or `TypeNull`. It is `TypeNull` when an argument that is NULL makes every value NULL. For an expression without a parameter, it is the type that `Program.Type` gives.
5. **`Bound.Eval(row)`** gives the value of the expression for a row, or an error. `row` holds the values of the columns of the table at the positions that `resolve` gave; an expression without a column takes a nil row. The value has the type that `Bound.Type` gives, or is NULL. `Eval` allocates nothing but an error. The zero `Bound` is not bound, and its `Eval` fails with an internal error.

An `expr.Value` is an integer, a real, or NULL, which is the zero `Value`. It is a small struct, passed by value. `Null`, `Integer`, `Real`, and `Type` read it, `Proto` gives it as a new `*s4qlpb.Value` for a row or an event, and `String` writes it, with `-0` for negative zero. Two values are equal with `==` when they have the same type and the same bits, so 0.0 and -0.0 differ. The package exports no constructor: a `Value` comes from `Bound.Eval`.

## Errors

A caller tells the errors apart with `errors.As` and `errors.Is`:

| Error | Matches | From | When |
| --- | --- | --- | --- |
| The error of `resolve` | whatever it matches | `Check` | `resolve` fails for a name. `Check` returns the error unchanged. |
| `*stmt.NotSupportedError` | `stmt.ErrNotSupported` | `Check`, `Bind` | An operand is text or a blob, which the types show before anything is computed. `Check` gives `column of type TEXT in arithmetic` or `column of type BLOB in arithmetic`, at the offset of the column. `Bind` gives `argument 2 in arithmetic, which is text` or `argument 2 in arithmetic, which is a blob`, at the offset of the `?`. |
| `*expr.Error` | `expr.ErrEvaluation` and its `Kind`: `expr.ErrOverflow`, `expr.ErrDivisionByZero`, or `expr.ErrNotFinite` | `Eval` | An operation has no value that s4ql gives, where SQLite gives one. See [Semantics](#semantics). |
| An error that wraps `expr.ErrInternal` | `expr.ErrInternal` | every step | A fault of s4ql: a tree that the pass never gives, a column of no type, a parameter without an argument, a row without the column, a value of another type than its column, or the zero `Bound`. The engine wraps it in its own internal error. |

A column refusal comes before an argument refusal, since `Check` comes before `Bind`.

An `*expr.Error` has three fields: `Kind`, `Op`, the operator that failed, and `Span`, the operator's node in the text of the statement. Its message names the kind, the operator, and the offset, such as `s4ql: integer overflow in + (at offset 17)`. It names no value, since a value may come from a row or an argument.

## Semantics

Each rule below was measured with SQLite 3.54.1, and the tests pin it against SQLite (see [How it is tested](#how-it-is-tested)).

**Order of evaluation and NULL.** Each operator computes its left operand, then its right operand, then itself, and the first error ends the evaluation. An operator with a NULL operand gives NULL, and so does the negation of NULL. The other operand is still computed, and its error still comes. So whether an operator fails depends on its own operands alone, never on a NULL beside it: `a + b / ?` with `a` NULL and the argument 0 is a division by zero, where SQLite gives NULL. NULL is tested before the divisor, so `a / b` with `a` NULL and `b` 0 is NULL, as in SQLite.

**Integers.** Two integers give an integer, and `/` truncates toward zero: `7 / 2` is 3, and `-7 / 2` and `7 / -2` are -3. A result that does not fit 64 bits is `ErrOverflow`, at the operator. SQLite gives a real there: `9223372036854775807 + 1` is `9.2233720368547758e+18`. This covers each of the four operators, the negation of the smallest integer, and the smallest integer divided by -1. A result that just fits, such as `-2147483648 * 4294967296`, is the smallest integer in both.

**Conversion of an integer to a real.** An operand that is a real makes the operation real. The integer operand is converted to the nearest real, the even one of two that are as near, as C converts it and so SQLite: `2^53 + 1` becomes `2^53`, and the largest integer becomes `2^63`. An integer operation below a real one is computed in integers first: `(a + b) * 1.0` with `a = 2^53` and `b = 1` is `2^53`.

**Rounding.** Each real operation rounds its result once, as SQLite's bytecode does. No two operations are fused into one instruction. Go may fuse `x*y + z` into a fused multiply-add on arm64, so `arithmetic` in `expr.go` converts each result to `float64` explicitly, which forbids it. `0.1 * 10.0 - 1.0` is exactly 0.0 in SQLite and in s4ql; a fused multiply-add gives `5.551115123125783e-17`.

**Division by zero.** A division by the integer 0, the real 0.0, or the real -0.0 is `ErrDivisionByZero`, whether the dividend is an integer or a real. SQLite gives NULL. The pass refuses a division by a literal zero already; this rule covers a zero from a row, an argument, or an operation.

**Results that are not finite.** A real result that is infinite is `ErrNotFinite`. SQLite gives the infinity as a real: `c * 10` with `c = 1e308` is `Inf`. SQLite gives NULL for a result that is not a number, such as `c * 10 - c * 10`. s4ql never reaches one, since from finite operands an infinity comes first, and s4ql refuses it there. Underflow is no error: `1e-200 * 1e-200` is 0.0, and `5e-324 / 1.5` is `5e-324`, as in SQLite.

**Negative zero and the negation.** SQLite keeps -0.0 through arithmetic, bit for bit, and so does s4ql: `-0.0 * 1`, `0.0 * -1`, `-0.0 + -0.0`, `-0.0 - 0.0`, and `0.0 / -3` are -0.0, and `-0.0 + 0` and `x - x` are 0.0. The negation is the exception. SQLite computes `-x` of a column or a parameter as `0 - x`, so the negation of 0.0 and of -0.0 is 0.0, where Go's `-x` gives -0.0. s4ql computes the negation as `0 - x` too. A `-` directly before a number is part of the literal, so the literal `-0.0` is -0.0.

**Text and blobs.** s4ql refuses an operand that is text or a blob: a column by its declared type at `Check`, and an argument at `Bind`. SQLite reads them as numbers: `s + 1` is 5 with `s = '4'`, `2 * x` is 10 with `x = x'35'`, and `a + ?` is 3.5 with `a = 1` and the argument `'2.5'`.

Where s4ql and SQLite differ, s4ql refuses, and nowhere does it give another value:

| Case | SQLite | s4ql |
| --- | --- | --- |
| An integer result outside 64 bits | a real | `ErrOverflow` |
| A division by 0, 0.0, or -0.0 | NULL | `ErrDivisionByZero` |
| An infinite real result | the infinity | `ErrNotFinite` |
| An error in one operand and NULL in the other | NULL | the error |
| A column of type TEXT or BLOB | the column read as a number | refused at `Check` |
| An argument that is text or a blob | the argument read as a number | refused at `Bind` |

## How it is tested

`go test ./internal/expr/` runs every test. They need cgo, since they ask SQLite through [the binding](../sqlite/README.md).

**The pinned facts.** `TestBound_Eval` and `TestCheck` in `expr_test.go` hold the facts of [Semantics](#semantics), each as an expression with a row and arguments. Each case asks SQLite for its value and requires the value that the case states, then requires s4ql's answer: the same value, or the error of the stated kind at the stated operator, or the refusal with its `What` and offset. A change in SQLite's behaviour fails the test as well as a change in s4ql's. `TestCheck` also pins the order of the errors of `resolve`. The other tests pin the types, the binding by number, the internal errors, the messages, and that `Eval` makes no allocation (`TestBound_Eval_allocations`).

**The differential test.** `TestDifferential` in `differential_test.go` checks the cases of the generator in `generator_test.go`, 2,500 for each of eight seeds. The generator draws trees of every operator and the negation, with literals, columns, parameters, and NULL, and often relates the values of an operator so that it lands on an edge: the bounds of 64 bits, a zero divisor, an infinite or underflowing real, a product whose sum would differ under a fused multiply-add, and integers above `2^53` with reals. A few cases hold a text or a blob, and a few are a column alone. Each case is written as the value of `update t set k = ...` and read by `stmt.Check`, so the evaluator gets the tree that the engine will give it. The row holds the columns `a INT`, `b INTEGER`, `c REAL`, `d REAL`, `s TEXT`, and `x BLOB`. SQLite computes the same text in `with v(a, b, c, d, s, x) as (select ?1, ..., ?6) select <expression> from v`, whose columns have no affinity.

**The reference and the quirk check.** `reference` computes each case apart from the evaluator: integers exactly with `math/big`, which shows an overflow; reals as the exact result rounded once to a `float64` by `math/big`, with the sign of an exact zero by the rules of IEEE 754; and the negation as `0 - x`. Its value must equal SQLite's, and s4ql's must equal both, of the same type and bit for bit. Where the reference gives an error, s4ql must give that kind at the same operator and span. The test then asks SQLite for the text of the failing operator alone, which must show the quirk that s4ql refuses: a real from integer operands for an overflow, NULL from operands that are not NULL for a division by zero, and an infinity for a result that is not finite. A refusal must name the first TEXT or BLOB column, or else the first text or blob argument, in the order of the text.

**The required kinds.** `testdata/reached/TestDifferential.kinds` lists the kinds that the seeds must reach together, each with its least number: each operator with each pair of operand types, each error of each operator that can have it, NULL, both sides of the conversion of an integer to a real, a fused multiply-add that would differ, and the outcomes of the cases. The file is written by hand. `testdata/reached/TestDifferential.tsv` holds the counts of each seed, and a seed must reach at least a third of each. [`stmttest.Reached`](../sql/stmt/stmttest/reached.go) has the rules. After a change of the generator, write the counts again:

```sh
S4QL_RECOUNT=1 go test -run '^TestDifferential$' ./internal/expr/
```

The recount never writes the file of the kinds, so lowering a least number there is a decision made by hand.

**The fuzz test.** `FuzzExpr` takes the text of an expression, the values of `a` to `d`, and the arguments, and checks them by the same rules. Its seeds are 200 cases of the generator. A text that is not an expression over these columns is no case. To fuzz it for a minute:

```sh
go test -run '^$' -fuzz '^FuzzExpr$' -fuzztime 1m ./internal/expr/
```

`make fuzz` runs it with the other fuzz tests of the project, each for `FUZZTIME`. It links SQLite, so it is in the group `c`, which CI fuzzes under the address sanitizer in the jobs `asan-fuzz`.
