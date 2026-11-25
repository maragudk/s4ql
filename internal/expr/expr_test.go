package expr_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/expr"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// row of the columns of [schema]: a, b, c, and d as given, s the text '4', and x the blob x'35',
// which SQLite would read as the number 4 and 5.
func row(a, b, c, d *s4qlpb.Value) []*s4qlpb.Value {
	return []*s4qlpb.Value{a, b, c, d, text("4"), blob([]byte("5"))}
}

// zeros is a row of zeros.
var zeros = row(integer(0), integer(0), float(0), float(0))

// The facts of SQLite 3.54.1 that the evaluator rests on, measured through the binding, and what s4ql
// gives for each. A fact is SQLite's answer for the expression with the row and the arguments; s4ql
// gives the same value, or the error of the kind named at the operator whose text is named.
func TestBound_Eval(t *testing.T) {
	const (
		maxInt = math.MaxInt64
		minInt = math.MinInt64
	)
	negZero := math.Copysign(0, -1)
	inf := math.Inf(1)
	tests := []struct {
		name string
		src  string
		row  []*s4qlpb.Value
		args []*s4qlpb.Value
		// sqlite is SQLite's value.
		sqlite expr.Value
		// err is the kind of s4ql's error, or nil if s4ql gives SQLite's value, and at is the text of
		// the operator that fails.
		err error
		at  string
	}{
		{name: "should divide integers to an integer that is truncated toward zero", src: "7 / 2", row: zeros, sqlite: expr.IntegerValue(3)},
		{name: "should truncate a negative quotient toward zero", src: "-7 / 2", row: zeros, sqlite: expr.IntegerValue(-3)},
		{name: "should truncate a quotient by a negative integer toward zero", src: "7 / -2", row: zeros, sqlite: expr.IntegerValue(-3)},
		{name: "should give a positive quotient of two negative integers", src: "-7 / -2", row: zeros, sqlite: expr.IntegerValue(3)},
		{name: "should give an integer for the product of integers", src: "a * b", row: row(integer(-4), integer(5), float(0), float(0)), sqlite: expr.IntegerValue(-20)},
		{name: "should give the smallest integer as a product that just fits", src: "a * b", row: row(integer(-1<<31), integer(1<<32), float(0), float(0)), sqlite: expr.IntegerValue(minInt)},
		{name: "should give the smallest integer as a sum that just fits", src: "a + b", row: row(integer(minInt+1), integer(-1), float(0), float(0)), sqlite: expr.IntegerValue(minInt)},

		{name: "should refuse a sum above the largest integer, which SQLite gives as a real", src: "a + 1", row: row(integer(maxInt), integer(0), float(0), float(0)),
			sqlite: expr.RealValue(0x1p63), err: expr.ErrOverflow, at: "a + 1"},
		{name: "should refuse a difference below the smallest integer, which SQLite gives as a real", src: "a - 1", row: row(integer(minInt), integer(0), float(0), float(0)),
			sqlite: expr.RealValue(-0x1p63), err: expr.ErrOverflow, at: "a - 1"},
		{name: "should refuse a product above the largest integer, which SQLite gives as a real", src: "a * 2", row: row(integer(1<<62), integer(0), float(0), float(0)),
			sqlite: expr.RealValue(0x1p63), err: expr.ErrOverflow, at: "a * 2"},
		{name: "should refuse the product of the smallest integer and -1", src: "a * -1", row: row(integer(minInt), integer(0), float(0), float(0)),
			sqlite: expr.RealValue(0x1p63), err: expr.ErrOverflow, at: "a * -1"},
		{name: "should refuse the quotient of the smallest integer by -1, which SQLite gives as a real", src: "a / -1", row: row(integer(minInt), integer(0), float(0), float(0)),
			sqlite: expr.RealValue(0x1p63), err: expr.ErrOverflow, at: "a / -1"},
		{name: "should refuse the negation of the smallest integer, which SQLite gives as a real", src: "-a", row: row(integer(minInt), integer(0), float(0), float(0)),
			sqlite: expr.RealValue(0x1p63), err: expr.ErrOverflow, at: "-a"},
		{name: "should refuse the smallest integer literal minus 1", src: "-9223372036854775808 - 1", row: zeros,
			sqlite: expr.RealValue(-0x1p63), err: expr.ErrOverflow, at: "-9223372036854775808 - 1"},

		{name: "should refuse an integer division by zero, which SQLite gives as NULL", src: "a / b", row: row(integer(5), integer(0), float(0), float(0)),
			sqlite: expr.Value{}, err: expr.ErrDivisionByZero, at: "a / b"},
		{name: "should refuse a real division by zero", src: "c / d", row: row(integer(0), integer(0), float(5), float(0)),
			sqlite: expr.Value{}, err: expr.ErrDivisionByZero, at: "c / d"},
		{name: "should refuse a real division by negative zero", src: "c / d", row: row(integer(0), integer(0), float(5), float(negZero)),
			sqlite: expr.Value{}, err: expr.ErrDivisionByZero, at: "c / d"},
		{name: "should refuse a division of an integer by the real zero", src: "a / d", row: row(integer(5), integer(0), float(0), float(0)),
			sqlite: expr.Value{}, err: expr.ErrDivisionByZero, at: "a / d"},
		{name: "should refuse a division of a real by the integer zero", src: "c / b", row: row(integer(0), integer(0), float(5), float(0)),
			sqlite: expr.Value{}, err: expr.ErrDivisionByZero, at: "c / b"},
		{name: "should refuse a division of zero by zero", src: "c / ?", row: zeros, args: []*s4qlpb.Value{integer(0)},
			sqlite: expr.Value{}, err: expr.ErrDivisionByZero, at: "c / ?"},

		{name: "should give NULL for a column that is NULL", src: "a + 1", row: row(null(), integer(0), float(0), float(0)), sqlite: expr.Value{}},
		{name: "should give NULL for an argument that is NULL", src: "? * 2.5", row: zeros, args: []*s4qlpb.Value{null()}, sqlite: expr.Value{}},
		{name: "should give NULL for the negation of NULL", src: "-c", row: row(integer(0), integer(0), null(), float(0)), sqlite: expr.Value{}},
		{name: "should give NULL for NULL divided by zero", src: "a / b", row: row(null(), integer(0), float(0), float(0)), sqlite: expr.Value{}},
		{name: "should give NULL for a number divided by NULL", src: "c / ?", row: row(integer(0), integer(0), float(1), float(0)), args: []*s4qlpb.Value{null()}, sqlite: expr.Value{}},
		{name: "should refuse a division by zero beside NULL, where SQLite gives NULL", src: "a + b / ?", row: row(null(), integer(1), float(0), float(0)), args: []*s4qlpb.Value{integer(0)},
			sqlite: expr.Value{}, err: expr.ErrDivisionByZero, at: "b / ?"},
		{name: "should refuse an overflow beside NULL, where SQLite gives NULL", src: "(b + 1) * a", row: row(null(), integer(maxInt), float(0), float(0)),
			sqlite: expr.Value{}, err: expr.ErrOverflow, at: "b + 1"},

		{name: "should refuse a product of reals that is infinite, which SQLite gives as Inf", src: "c * 10", row: row(integer(0), integer(0), float(1e308), float(0)),
			sqlite: expr.RealValue(inf), err: expr.ErrNotFinite, at: "c * 10"},
		{name: "should refuse a sum of reals that is infinite", src: "c + d", row: row(integer(0), integer(0), float(math.MaxFloat64), float(math.MaxFloat64)),
			sqlite: expr.RealValue(inf), err: expr.ErrNotFinite, at: "c + d"},
		{name: "should refuse a difference of reals that is infinite", src: "c - d", row: row(integer(0), integer(0), float(-math.MaxFloat64), float(math.MaxFloat64)),
			sqlite: expr.RealValue(math.Inf(-1)), err: expr.ErrNotFinite, at: "c - d"},
		{name: "should refuse a quotient of reals that is infinite", src: "c / d", row: row(integer(0), integer(0), float(1e308), float(1e-10)),
			sqlite: expr.RealValue(inf), err: expr.ErrNotFinite, at: "c / d"},
		{name: "should refuse the first infinity, where SQLite gives NULL for not a number", src: "c * 10 - c * 10", row: row(integer(0), integer(0), float(1e308), float(0)),
			sqlite: expr.Value{}, err: expr.ErrNotFinite, at: "c * 10"},
		{name: "should give the smallest subnormal real for a quotient that underflows to it", src: "c / d", row: row(integer(0), integer(0), float(math.SmallestNonzeroFloat64), float(1.5)),
			sqlite: expr.RealValue(math.SmallestNonzeroFloat64)},
		{name: "should give zero for a product that underflows", src: "c * d", row: row(integer(0), integer(0), float(1e-200), float(1e-200)), sqlite: expr.RealValue(0)},
		{name: "should give negative zero for a negative product that underflows", src: "c * d", row: row(integer(0), integer(0), float(-1e-200), float(1e-200)), sqlite: expr.RealValue(negZero)},

		{name: "should negate the real zero to zero, as SQLite computes 0 - c", src: "-c", row: zeros, sqlite: expr.RealValue(0)},
		{name: "should negate the real negative zero to zero", src: "-c", row: row(integer(0), integer(0), float(negZero), float(0)), sqlite: expr.RealValue(0)},
		{name: "should negate a real", src: "-c", row: row(integer(0), integer(0), float(1.5), float(0)), sqlite: expr.RealValue(-1.5)},
		{name: "should negate the integer zero to zero", src: "-a", row: zeros, sqlite: expr.IntegerValue(0)},
		{name: "should give negative zero for the product of zero and -1", src: "c * -1", row: zeros, sqlite: expr.RealValue(negZero)},
		{name: "should give negative zero for the literal -0.0 times 1", src: "-0.0 * 1", row: zeros, sqlite: expr.RealValue(negZero)},
		{name: "should give negative zero for the sum of two negative zeros", src: "c + d", row: row(integer(0), integer(0), float(negZero), float(negZero)), sqlite: expr.RealValue(negZero)},
		{name: "should give zero for negative zero plus the integer zero", src: "c + a", row: row(integer(0), integer(0), float(negZero), float(0)), sqlite: expr.RealValue(0)},
		{name: "should give zero for a real minus itself", src: "c - c", row: row(integer(0), integer(0), float(-2.5), float(0)), sqlite: expr.RealValue(0)},
		{name: "should give negative zero for negative zero minus zero", src: "c - d", row: row(integer(0), integer(0), float(negZero), float(0)), sqlite: expr.RealValue(negZero)},
		{name: "should give negative zero for zero divided by a negative integer", src: "c / -3", row: zeros, sqlite: expr.RealValue(negZero)},

		{name: "should make the sum of an integer and a real a real", src: "a + 0.5", row: row(integer(3), integer(0), float(0), float(0)), sqlite: expr.RealValue(3.5)},
		{name: "should make the sum of a real and an integer a real", src: "0.5 + a", row: row(integer(3), integer(0), float(0), float(0)), sqlite: expr.RealValue(3.5)},
		{name: "should convert an integer above 2^53 on the left to the nearest even real", src: "a + c", row: row(integer(1<<53+1), integer(0), float(0), float(0)),
			sqlite: expr.RealValue(0x1p53)},
		{name: "should convert an integer above 2^53 on the right to the nearest even real", src: "c + a", row: row(integer(1<<53+3), integer(0), float(0), float(0)),
			sqlite: expr.RealValue(0x1p53 + 4)},
		{name: "should convert the largest integer to 2^63", src: "a * 1.0", row: row(integer(maxInt), integer(0), float(0), float(0)), sqlite: expr.RealValue(0x1p63)},
		{name: "should convert the largest integer before it subtracts 2^63", src: "a - c", row: row(integer(maxInt), integer(0), float(0x1p63), float(0)), sqlite: expr.RealValue(0)},
		{name: "should compute an integer operation before the real one above it", src: "(a + b) * 1.0", row: row(integer(1<<53), integer(1), float(0), float(0)),
			sqlite: expr.RealValue(0x1p53)},
		{name: "should compute the integer operation of two integers that are also above 2^53", src: "a - b + c", row: row(integer(1<<53+1), integer(1<<53), float(0.5), float(0)),
			sqlite: expr.RealValue(1.5)},
		{name: "should give an integer for an integer divided by a real", src: "a / c", row: row(integer(7), integer(0), float(2), float(0)), sqlite: expr.RealValue(3.5)},

		{name: "should round the product before the difference, as no fused instruction does", src: "c * d - 1.0", row: row(integer(0), integer(0), float(0.1), float(10)),
			sqlite: expr.RealValue(0)},
		{name: "should round the product of literals before the difference", src: "0.1 * 10.0 - 1.0", row: zeros, sqlite: expr.RealValue(0)},
		{name: "should round the product before the sum, with the sum on the left", src: "? + c * d", row: row(integer(0), integer(0), float(0.1), float(10)),
			args: []*s4qlpb.Value{float(-1)}, sqlite: expr.RealValue(0)},
		{name: "should round the product of an argument and a column before the sum", src: "c + ? * d", row: row(integer(0), integer(0), float(-1), float(3)),
			args: []*s4qlpb.Value{float(1.0 / 3)}, sqlite: expr.RealValue(0)},
	}
	o := newOracle(t)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e, full, err := parse(test.src)
			is.NotError(t, err)
			sqliteValue, err := o.value(full[e.Span.Start:e.Span.End], test.row, test.args)
			is.NotError(t, err)
			is.Equal(t, test.sqlite, sqliteValue, "SQLite's value")

			x, err := expr.Check(e, resolve)
			is.NotError(t, err)
			b, err := x.Bind(test.args)
			is.NotError(t, err)
			v, err := b.Eval(test.row)
			if test.err == nil {
				is.NotError(t, err)
				is.Equal(t, test.sqlite, v)
				return
			}
			var ee *expr.Error
			is.True(t, errors.As(err, &ee), "an *expr.Error")
			is.True(t, errors.Is(err, test.err), "the kind of the error")
			is.True(t, errors.Is(err, expr.ErrEvaluation))
			is.Equal(t, test.at, full[ee.Span.Start:ee.Span.End])
			is.Equal(t, expr.Value{}, v)
		})
	}
}

// The facts of SQLite 3.54.1 for the refusals of text and blobs, which SQLite computes as numbers,
// measured through the binding, and s4ql's refusal of each.
func TestCheck(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		args   []*s4qlpb.Value
		sqlite expr.Value
		// alone reports whether src is a column alone, whose value SQLite gives as it is.
		alone bool
		// what is refused, at the text of at; at check if atCheck, and when the arguments are bound
		// otherwise.
		what    string
		at      string
		atCheck bool
	}{
		{name: "should refuse a TEXT column, which SQLite reads as a number", src: "s + 1", sqlite: expr.IntegerValue(5),
			what: "column of type TEXT in arithmetic", at: "s", atCheck: true},
		{name: "should refuse a BLOB column, which SQLite reads as a number", src: "2 * x", sqlite: expr.IntegerValue(10),
			what: "column of type BLOB in arithmetic", at: "x", atCheck: true},
		{name: "should refuse a TEXT column alone, which is no arithmetic in SQLite", src: "s", alone: true, what: "column of type TEXT in arithmetic", at: "s", atCheck: true},
		{name: "should refuse the first of two such columns", src: "a + x - s", sqlite: expr.IntegerValue(2),
			what: "column of type BLOB in arithmetic", at: "x", atCheck: true},
		{name: "should refuse a text argument, which SQLite reads as a number", src: "a + ?", args: []*s4qlpb.Value{text("2.5")}, sqlite: expr.RealValue(3.5),
			what: "argument 1 in arithmetic, which is text", at: "?"},
		{name: "should refuse a blob argument", src: "? - ? * a", args: []*s4qlpb.Value{integer(1), blob([]byte("3"))}, sqlite: expr.IntegerValue(-2),
			what: "argument 2 in arithmetic, which is a blob", at: "?", atCheck: false},
		{name: "should refuse a column before an argument", src: "? + s", args: []*s4qlpb.Value{text("1")}, sqlite: expr.IntegerValue(5),
			what: "column of type TEXT in arithmetic", at: "s", atCheck: true},
	}
	o := newOracle(t)
	r := row(integer(1), integer(0), float(0), float(0))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e, full, err := parse(test.src)
			is.NotError(t, err)
			if !test.alone {
				sqliteValue, err := o.value(full[e.Span.Start:e.Span.End], r, test.args)
				is.NotError(t, err)
				is.Equal(t, test.sqlite, sqliteValue, "SQLite's value")
			}

			x, err := expr.Check(e, resolve)
			if !test.atCheck {
				is.NotError(t, err)
				_, err = x.Bind(test.args)
			}
			var ns *stmt.NotSupportedError
			is.True(t, errors.As(err, &ns), "a *stmt.NotSupportedError")
			is.True(t, errors.Is(err, stmt.ErrNotSupported))
			is.Equal(t, test.what, ns.What)
			at := strings.LastIndex(full, test.at)
			if test.at == "?" && strings.Count(test.src, "?") == 2 {
				at = strings.LastIndex(full, "?")
			}
			is.Equal(t, at, ns.Offset)
		})
	}
}
