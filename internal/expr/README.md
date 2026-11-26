# The evaluator of expressions

`internal/expr` computes a `stmt.Expression`, the typed form of an expression that [the pass after the parser](../sql/stmt/README.md#arithmetic) gives, over the values of a row and the arguments of a statement. It computes arithmetic, `+`, `-`, `*`, and `/` on integers and reals and a `-` before an operand, and text: the concatenation `||` and the functions `lower` and `hex`. For every expression it gives the value that SQLite gives, of the same type and bit for bit, or an error. It never gives another value.

It does nothing else. It has no comparisons, no other functions, and no arithmetic on text or blobs, and it reads no table: the caller resolves the names of the columns and hands it the rows. It depends on `internal/sql/stmt` and `internal/s4qlpb` only, so the engine can import it.

The engine uses it to evaluate arithmetic; see [the section Arithmetic of the README of the engine](../engine/README.md#arithmetic). It does not yet run a statement with text, which it refuses before the evaluator sees it. The decisions behind the package, the measurements of SQLite, and what is left for the engine are in [docs/todo/arithmetic.md](../../docs/todo/arithmetic.md) and [docs/todo/text-functions.md](../../docs/todo/text-functions.md).

## The API

An engine calls the package three times: when it prepares a statement, when it knows the arguments, and for each row.

1. **`expr.Check(e, resolve)`** checks the expression `e` before any row is read and gives a `*expr.Program`. `resolve` is a `func(stmt.Name) (expr.Column, error)` that finds a column of the table. An `expr.Column` is the position of the column's value in a row, from 0, and its declared type, an `s4qlpb.Type`. `Check` resolves every name first, in the order of the text, and returns the first error of `resolve` as it is. So the engine's error for a column that the table does not have, SQLite's `no such column`, comes before any refusal of a type. Then `Check` refuses the first column, in the order of the text, of a type that its place does not take: arithmetic takes INT, INTEGER, and REAL, `||` and `lower` take TEXT, and `hex` takes TEXT and BLOB. A column alone, as in `set a = b`, is an expression too, of the type of its column.
2. **`Program.Type()`** gives the type of the values, `expr.TypeInteger`, `expr.TypeReal`, `expr.TypeText`, or `expr.TypeBlob`, and true, for an expression without a parameter. With a parameter, the type depends on the arguments, and it gives 0 and false. A `*Program` never changes after `Check`, so it is safe for concurrent use and can be kept with a prepared statement.
3. **`Program.Bind(args, limit)`** gives an `expr.Bound` once the arguments are known. `args` holds all the arguments of the statement as `[]*s4qlpb.Value`, and each parameter takes the argument of its number, from 1. `limit` is the length limit, the longest text or blob in bytes that SQLite makes. It must be positive, and `Bind` takes it as SQLite does: below 30 as 30, and above 1,000,000,000 as 1,000,000,000. `Bind` refuses the first parameter in the order of the text whose argument is of a type that its place does not take, as `Check` refuses a column; NULL meets every place. It checks nothing else: a real argument that is not finite, which the engine refuses before, goes into the arithmetic like any operand, and a text or a blob longer than the limit, which the engine refuses before as SQLite does, makes the first operation that it reaches too long, or NULL. Then `Bind` gives `ErrTooLong` for the first literal, in the order of the text, that is a text or a blob longer than the limit, before anything is computed: SQLite refuses such a text, also where it would be NULL beside another operand, and reads such a blob as NULL, which s4ql refuses. The `Bound` holds `args`, which must not change while it is in use. For an expression without a parameter, `Bind` takes any arguments, also nil, and allocates nothing.
4. **`Bound.Type()`** gives `TypeInteger`, `TypeReal`, `TypeText`, `TypeBlob`, or `TypeNull`. It is `TypeNull` when an argument that is NULL makes every value NULL, as it does for arithmetic, `||`, and `lower`, but not for `hex`, which gives the empty text for NULL. For an expression without a parameter, it is the type that `Program.Type` gives.
5. **`Bound.Eval(row)`** gives the value of the expression for a row, or an error. `row` holds the values of the columns of the table at the positions that `resolve` gave; an expression without a column takes a nil row. The value has the type that `Bound.Type` gives, or is NULL. The zero `Bound` is not bound, and its `Eval` fails with an internal error.

**Allocations.** Arithmetic allocates nothing but an error, and `TestBound_Eval_allocations` pins it. An operation of text allocates its result once, or not at all where its result is one of its operands: `||` with the empty text, `lower` of a text without an ASCII capital letter, and `hex` of the empty text, the empty blob, or NULL. So `'m_' || lower(hex(x))` costs two allocations, or three where the digits have a letter that `lower` changes. A text is never copied from the row, the arguments, or a literal; `hex` reads the bytes of a blob where they are. A value may share its bytes with the row, which is safe, since Go's strings never change: a caller that evaluates a row at a time may keep a value after the row is gone, and its `Proto` too, which does not copy a text either. A BLOB column alone is the one place where `Eval` copies bytes, since a `Value` holds a blob as a string.

An `expr.Value` is an integer, a real, a text, a blob, or NULL, which is the zero `Value`. It is a small struct, passed by value. `Null`, `Integer`, `Real`, `Text`, `Blob`, and `Type` read it, `Proto` gives it as a new `*s4qlpb.Value` for a row or an event, and `String` writes it, with `-0` for negative zero, a text as a string literal, and a blob as a blob literal. Two values are equal with `==` when they have the same type and the same bits or bytes, so 0.0 and -0.0 differ, and the empty text is not NULL. The package exports no constructor: a `Value` comes from `Bound.Eval`.

## Errors

A caller tells the errors apart with `errors.As` and `errors.Is`:

| Error | Matches | From | When |
| --- | --- | --- | --- |
| The error of `resolve` | whatever it matches | `Check` | `resolve` fails for a name. `Check` returns the error unchanged. |
| `*stmt.NotSupportedError` | `stmt.ErrNotSupported` | `Check`, `Bind` | An operand is of a type that its place does not take, which the types show before anything is computed. `Check` gives `column of type TEXT in arithmetic`, `column of type BLOB in arithmetic`, `column of type INT in a concatenation`, `column of type REAL in a call of lower`, `column of type INTEGER in a call of hex`, and so on, at the offset of the column. `Bind` gives `argument 2 in arithmetic, which is text`, `argument 2 in a concatenation, which is an integer`, `argument 2 in a call of lower, which is a blob`, `argument 2 in a call of hex, which is a real`, and so on, at the offset of the `?`. |
| `*expr.Error` | `expr.ErrEvaluation` and its `Kind`: `expr.ErrOverflow`, `expr.ErrDivisionByZero`, `expr.ErrNotFinite`, or `expr.ErrTooLong` | `Eval` | An operation has no value that s4ql gives, where SQLite gives one; or, for `ErrTooLong`, where SQLite fails too, with `string or blob too big`. See [Semantics](#semantics). |
| An error that wraps `expr.ErrInternal` | `expr.ErrInternal` | every step | A fault of s4ql: a tree that the pass never gives, a column of no type, a parameter without an argument, a row without the column, a value of another type than its column, or the zero `Bound`. The engine wraps it in its own internal error. |

A column refusal comes before an argument refusal, since `Check` comes before `Bind`.

An `*expr.Error` has four fields: `Kind`, `Op`, the operator that failed or `stmt.ExprCall` for a call, `Function`, the function of a call, and `Span`, the node in the text of the statement. Its message names the kind, the operator or the function, and the offset, such as `s4ql: integer overflow in + (at offset 17)` and `s4ql: text longer than the length limit in hex (at offset 17)`. It names no value, since a value may come from a row or an argument.

## Semantics

Each rule below was measured with SQLite 3.54.1, and the tests pin it against SQLite (see [How it is tested](#how-it-is-tested)).

**Order of evaluation and NULL.** Each operator computes its left operand, then its right operand, then itself, and the first error ends the evaluation. An operator with a NULL operand gives NULL, and so does the negation of NULL. The other operand is still computed, and its error still comes. So whether an operator fails depends on its own operands alone, never on a NULL beside it: `a + b / ?` with `a` NULL and the argument 0 is a division by zero, where SQLite gives NULL. NULL is tested before the divisor, so `a / b` with `a` NULL and `b` 0 is NULL, as in SQLite.

**Integers.** Two integers give an integer, and `/` truncates toward zero: `7 / 2` is 3, and `-7 / 2` and `7 / -2` are -3. A result that does not fit 64 bits is `ErrOverflow`, at the operator. SQLite gives a real there: `9223372036854775807 + 1` is `9.2233720368547758e+18`. This covers each of the four operators, the negation of the smallest integer, and the smallest integer divided by -1. A result that just fits, such as `-2147483648 * 4294967296`, is the smallest integer in both.

**Conversion of an integer to a real.** An operand that is a real makes the operation real. The integer operand is converted to the nearest real, the even one of two that are as near, as C converts it and so SQLite: `2^53 + 1` becomes `2^53`, and the largest integer becomes `2^63`. An integer operation below a real one is computed in integers first: `(a + b) * 1.0` with `a = 2^53` and `b = 1` is `2^53`.

**Rounding.** Each real operation rounds its result once, as SQLite's bytecode does. No two operations are fused into one instruction. Go may fuse `x*y + z` into a fused multiply-add on arm64, so `arithmetic` in `expr.go` converts each result to `float64` explicitly, which forbids it. `0.1 * 10.0 - 1.0` is exactly 0.0 in SQLite and in s4ql; a fused multiply-add gives `5.551115123125783e-17`.

**Division by zero.** A division by the integer 0, the real 0.0, or the real -0.0 is `ErrDivisionByZero`, whether the dividend is an integer or a real. SQLite gives NULL. The pass refuses a division by a literal zero already; this rule covers a zero from a row, an argument, or an operation.

**Results that are not finite.** A real result that is infinite is `ErrNotFinite`. SQLite gives the infinity as a real: `c * 10` with `c = 1e308` is `Inf`. SQLite gives NULL for a result that is not a number, such as `c * 10 - c * 10`. s4ql never reaches one, since from finite operands an infinity comes first, and s4ql refuses it there. Underflow is no error: `1e-200 * 1e-200` is 0.0, and `5e-324 / 1.5` is `5e-324`, as in SQLite.

**Negative zero and the negation.** SQLite keeps -0.0 through arithmetic, bit for bit, and so does s4ql: `-0.0 * 1`, `0.0 * -1`, `-0.0 + -0.0`, `-0.0 - 0.0`, and `0.0 / -3` are -0.0, and `-0.0 + 0` and `x - x` are 0.0. The negation is the exception. SQLite computes `-x` of a column or a parameter as `0 - x`, so the negation of 0.0 and of -0.0 is 0.0, where Go's `-x` gives -0.0. s4ql computes the negation as `0 - x` too. A `-` directly before a number is part of the literal, so the literal `-0.0` is -0.0.

**Text and blobs in arithmetic.** s4ql refuses an operand of arithmetic that is text or a blob: a column by its declared type at `Check`, and an argument at `Bind`. SQLite reads them as numbers: `s + 1` is 5 with `s = '4'`, `2 * x` is 10 with `x = x'35'`, and `a + ?` is 3.5 with `a = 1` and the argument `'2.5'`.

**Concatenation.** `||` joins the bytes of its operands, the left first. It gives NULL where an operand is NULL, from a column or an argument, as SQLite does: `s || 'a'` with `s` NULL is NULL. Its operands are both computed, so an error of one comes beside a NULL in the other, as SQLite's does, since SQLite computes both too. The empty text is no NULL: `s || ''` is `s`.

**lower.** `lower` changes the bytes `A` to `Z` to `a` to `z` and leaves every other byte as it is, as SQLite does without ICU: `lower('ÀBC')` is `'Àbc'`, and the Kelvin sign and the dotted capital I, which Unicode folds to `k` and `i`, stay as they are. It is NULL for NULL.

**hex.** `hex` writes each byte of a blob, or of the UTF-8 of a text, as two upper-case hexadecimal digits: `hex('é')` is `'C3A9'`, and `hex(x'00ab')` is `'00AB'`. It gives the empty text for NULL, as SQLite does: `hex(s)` with `s` NULL is `''`, and so `hex(lower(?))` with a NULL argument is `''` too. It takes text, where SQLite's answer is the hex of its UTF-8 bytes, which is no conversion.

**Bytes.** The three treat a text as its bytes, as SQLite does. So they give SQLite's answer also for a text that is not valid UTF-8, which `TestBound_Eval_text` pins, though s4ql never stores one, and the engine refuses such an argument before it binds it. A text that s4ql takes is valid UTF-8 without the character NUL, and so is every text that the three make of it.

**The length limit.** SQLite refuses to make a text longer than its length limit, with `string or blob too big`, and `Eval` gives `ErrTooLong` there, at the operation. SQLite counts what it allocates: `||` the length of the result, and `lower` and `hex` one byte more, for the NUL at the end. So `||` may make a text as long as the limit, `lower` one a byte shorter, and `hex` one of digits shorter than the limit: with a limit of 1,000, `lower` of a text of 1,000 bytes and `hex` of 500 bytes fail, in SQLite and in s4ql. A text literal longer than the limit fails in both, at `Bind` in s4ql. No other error of evaluation comes from the three.

**Text and numbers.** s4ql refuses a number where text is due and text where a number is due, and a blob in `||` and `lower`: a column by its declared type at `Check`, and an argument at `Bind`. SQLite converts: `a || 'x'` is `'1x'` with `a = 1`, `lower(x)` reads a blob as text, and `hex(1.5)` is `'312E35'`, the hex of the text `1.5`.

Where s4ql and SQLite differ, s4ql refuses, and nowhere does it give another value:

| Case | SQLite | s4ql |
| --- | --- | --- |
| An integer result outside 64 bits | a real | `ErrOverflow` |
| A division by 0, 0.0, or -0.0 | NULL | `ErrDivisionByZero` |
| An infinite real result | the infinity | `ErrNotFinite` |
| An error in one operand and NULL in the other | NULL | the error |
| A column of type TEXT or BLOB in arithmetic | the column read as a number | refused at `Check` |
| An argument that is text or a blob in arithmetic | the argument read as a number | refused at `Bind` |
| A blob literal longer than the limit | NULL | `ErrTooLong` at `Bind` |
| A column or an argument that is a number in `||`, `lower`, or `hex`, or a blob in `||` or `lower` | the number written as text, the blob read as text | refused at `Check` or `Bind` |

## How it is tested

`go test ./internal/expr/` runs every test. They need cgo, since they ask SQLite through [the binding](../sqlite/README.md).

**The pinned facts.** `TestBound_Eval` and `TestCheck` in `expr_test.go` hold the facts of [Semantics](#semantics), each as an expression with a row and arguments. Each case asks SQLite for its value and requires the value that the case states, then requires s4ql's answer: the same value, or the error of the stated kind at the stated operator, or the refusal with its `What` and offset. A change in SQLite's behaviour fails the test as well as a change in s4ql's. `TestCheck` also pins the order of the errors of `resolve`. The other tests pin the types, the binding by number, the internal errors, the messages, and that `Eval` makes no allocation (`TestBound_Eval_allocations`).

**The differential test.** `TestDifferential` in `differential_test.go` checks the cases of the generator in `generator_test.go`, 3,600 for each of eight seeds. Two cases in three are arithmetic. The generator draws trees of every operator and the negation, with literals, columns, parameters, and NULL, and often relates the values of an operator so that it lands on an edge: the bounds of 64 bits, a zero divisor, an infinite or underflowing real, a product whose sum would differ under a fused multiply-add, and integers above `2^53` with reals. A few cases hold a text or a blob, and a few are a column alone. The third case is text: trees of `||`, `lower`, and `hex` of literals, columns, and parameters, with empty text, every kind of ASCII byte, letters that are not ASCII, blobs as the argument of `hex`, and NULL. One in four has a length limit from 1,000 to 4,000 bytes, and most of those have values whose lengths put an operation on either side of it. One in 30 is as deep as the pass takes: a chain of more than 900 `||`, or calls nested 90 to 100 deep. A few hold a number, or a blob where text is due. Each case is written as the value of `update t set k = ...` and read by `stmt.Check`, so the evaluator gets the tree that the engine will give it. The row holds the columns `a INT`, `b INTEGER`, `c REAL`, `d REAL`, `s TEXT`, `x BLOB`, `t TEXT`, and `y BLOB`. SQLite computes the same text in `with v(a, b, c, d, s, x, t, y) as (select ?1, ..., ?8) select <expression> from v`, whose columns have no affinity, under the limit of the case; the result column is named, since SQLite refuses an unnamed one whose text, its name, is longer than the limit.

**The reference and the quirk check.** `reference` computes each case apart from the evaluator: integers exactly with `math/big`, which shows an overflow; reals as the exact result rounded once to a `float64` by `math/big`, with the sign of an exact zero by the rules of IEEE 754; and the negation as `0 - x`. Its value must equal SQLite's, and s4ql's must equal both, of the same type and bit for bit. Where the reference gives an error, s4ql must give that kind at the same operator and span. The test then asks SQLite for the text of the failing operator alone, which must show the quirk that s4ql refuses: a real from integer operands for an overflow, NULL from operands that are not NULL for a division by zero, and an infinity for a result that is not finite. For a text that is too long, SQLite must fail with `string or blob too big` for the operation alone and for the whole expression. The reference computes text apart from the evaluator too, with `unicode.ToLower` on the ASCII bytes and `hex.EncodeToString`. A refusal must name the first column, or else the first argument, in the order of the text, whose type its place does not take.

**The required kinds.** `testdata/reached/TestDifferential.kinds` lists the kinds that the seeds must reach together, each with its least number: each operator with each pair of operand types, each error of each operator that can have it, NULL, both sides of the conversion of an integer to a real, a fused multiply-add that would differ, the outcomes of the cases, and the edges: an integer result that is exactly the largest or the smallest integer, a real result that is subnormal and not zero, the smallest integer divided by a negative integer other than -1, and an error on the right of NULL. For text, it lists each kind of `||`, `lower`, and `hex`, each with NULL, the refusals of most places, each operation at the limit and over it, also beside NULL, a text literal longer than the limit, `lower` of letters that Unicode would fold, and trees deeper than 900. The file is written by hand. `testdata/reached/TestDifferential.tsv` holds the counts of each seed, and a seed must reach at least a third of each. [`stmttest.Reached`](../sql/stmt/stmttest/reached.go) has the rules. After a change of the generator, write the counts again:

```sh
S4QL_RECOUNT=1 go test -run '^TestDifferential$' ./internal/expr/
```

The recount never writes the file of the kinds, so lowering a least number there is a decision made by hand.

**The fuzz test.** `FuzzExpr` takes the text of an expression, the values of the row, the arguments, and a length limit from 30 bytes, and checks them by the same rules. A text of the row or of an argument is made valid UTF-8 without NUL, as s4ql takes it. Its seeds are cases of the generator, without the deep trees, and the cases that the fuzzing found, in `testdata/fuzz/FuzzExpr`. A text that is not an expression over these columns is no case, and neither is a value longer than the limit. To fuzz it for a minute:

```sh
go test -run '^$' -fuzz '^FuzzExpr$' -fuzztime 1m ./internal/expr/
```

`make fuzz` runs it with the other fuzz tests of the project, each for `FUZZTIME`. It links SQLite, so it is in the group `c`, which CI fuzzes under the address sanitizer in the jobs `asan-fuzz`.
