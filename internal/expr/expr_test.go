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
		{name: "should give the largest integer as a product that is exactly it", src: "a * b", row: row(integer(7), integer(maxInt/7), float(0), float(0)), sqlite: expr.IntegerValue(maxInt)},
		{name: "should give the largest integer as a product of two negative integers", src: "a * b", row: row(integer(-7), integer(-(maxInt / 7)), float(0), float(0)), sqlite: expr.IntegerValue(maxInt)},
		{name: "should divide the smallest integer by a negative integer other than -1", src: "a / b", row: row(integer(minInt), integer(-2), float(0), float(0)), sqlite: expr.IntegerValue(1 << 62)},
		{name: "should truncate the quotient of the smallest integer by a negative integer toward zero", src: "a / -3", row: row(integer(minInt), integer(0), float(0), float(0)), sqlite: expr.IntegerValue(3074457345618258602)},
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
		{name: "should give a subnormal real that is not zero for a product below the smallest normal real", src: "c * d", row: row(integer(0), integer(0), float(1e-300), float(1e-10)),
			sqlite: expr.RealValue(1e-310)},
		{name: "should give a negative subnormal real for a difference of two normal reals", src: "c - d", row: row(integer(0), integer(0), float(0x1p-1022), float(0x1.0000000000001p-1022)),
			sqlite: expr.RealValue(-0x1p-1074)},
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
		{name: "should give a real for an integer divided by a real", src: "a / c", row: row(integer(7), integer(0), float(2), float(0)), sqlite: expr.RealValue(3.5)},

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

	t.Run("should refuse a concatenation and a call, which the evaluator does not compute yet, at the root, after the names", func(t *testing.T) {
		for _, c := range []struct{ src, what, at string }{
			{"s || 'a'", "concatenation", "s ||"},
			{"'a' || hex(x)", "concatenation", "'a'"},
			{"lower(s)", "call of lower", "lower"},
			{"hex(lower(?) || s)", "call of hex", "hex"},
		} {
			e, full, err := parse(c.src)
			is.NotError(t, err)
			_, err = expr.Check(e, resolve)
			var ns *stmt.NotSupportedError
			is.True(t, errors.As(err, &ns), "a *stmt.NotSupportedError")
			is.True(t, errors.Is(err, stmt.ErrNotSupported))
			is.Equal(t, c.what, ns.What, c.src)
			is.Equal(t, strings.Index(full, c.at), ns.Offset, c.src)
		}
		e, _, err := parse("lower(zz)")
		is.NotError(t, err)
		_, err = expr.Check(e, resolve)
		is.Error(t, errNoSuchColumn, err)
	})

	t.Run("should give the error of resolve before a refusal of the type of another column", func(t *testing.T) {
		e, _, err := parse("s + zz")
		is.NotError(t, err)
		_, err = expr.Check(e, resolve)
		is.Error(t, errNoSuchColumn, err)
	})

	t.Run("should give the first error of resolve in the order of the text", func(t *testing.T) {
		e, _, err := parse("(a + yy) * zz")
		is.NotError(t, err)
		var names []string
		_, err = expr.Check(e, func(n stmt.Name) (expr.Column, error) {
			names = append(names, n.Value)
			if n.Value == "a" {
				return expr.Column{Position: 0, Type: s4qlpb.Type_TYPE_INT}, nil
			}
			return expr.Column{}, errors.New("no such column: " + n.Value)
		})
		is.Equal(t, "no such column: yy", err.Error())
		is.EqualSlice(t, []string{"a", "yy"}, names)
	})

	t.Run("should resolve a name in any case and in double quotes", func(t *testing.T) {
		e, _, err := parse(`A * "c"`)
		is.NotError(t, err)
		x, err := expr.Check(e, resolve)
		is.NotError(t, err)
		b, err := x.Bind(nil)
		is.NotError(t, err)
		v, err := b.Eval(row(integer(3), integer(0), float(0.5), float(0)))
		is.NotError(t, err)
		is.Equal(t, expr.RealValue(1.5), v)
	})

	t.Run("should give an internal error that names the fault for a tree that the pass never gives", func(t *testing.T) {
		column := stmt.Expression{Kind: stmt.ExprColumn, Column: stmt.Name{Value: "a"}}
		for _, c := range []struct {
			e    stmt.Expression
			want string
		}{
			{stmt.Expression{}, "an expression of kind 0"},
			{stmt.Expression{Kind: stmt.ExprValue, Value: stmt.Text("1")}, "a literal of stmt.Text in arithmetic"},
			{stmt.Expression{Kind: stmt.ExprValue, Value: stmt.Null{}}, "a literal of stmt.Null in arithmetic"},
			{stmt.Expression{Kind: stmt.ExprParameter}, "a parameter numbered 0"},
			{stmt.Expression{Kind: stmt.ExprAdd, Operands: []stmt.Expression{column}}, "+ with 1 operands"},
			{stmt.Expression{Kind: stmt.ExprNegate, Operands: []stmt.Expression{column, column}}, "- with 2 operands"},
			{stmt.Expression{Kind: stmt.ExprNegate}, "- with 0 operands"},
			{stmt.Expression{Kind: stmt.ExpressionKind(99)}, "an expression of kind 99"},
			{stmt.Expression{Kind: stmt.ExprConcat, Operands: []stmt.Expression{column}}, "|| with 1 operands"},
			{stmt.Expression{Kind: stmt.ExprCall, Operands: []stmt.Expression{column}}, "a call of function 0 with 1 operands"},
			{stmt.Expression{Kind: stmt.ExprCall, Function: stmt.FuncLower}, "a call of function 1 with 0 operands"},
		} {
			_, err := expr.Check(c.e, resolve)
			is.True(t, errors.Is(err, expr.ErrInternal), err)
			is.Equal(t, "s4ql: internal error: "+c.want, err.Error())
		}
	})

	t.Run("should give an internal error that names the fault for a column of no type or at no position", func(t *testing.T) {
		e, _, err := parse("a + 1")
		is.NotError(t, err)
		for _, c := range []struct {
			column expr.Column
			want   string
		}{
			{expr.Column{Position: 0}, "a column of type TYPE_UNSPECIFIED"},
			{expr.Column{Position: -1, Type: s4qlpb.Type_TYPE_INT}, "a column at position -1"},
		} {
			_, err := expr.Check(e, func(stmt.Name) (expr.Column, error) { return c.column, nil })
			is.True(t, errors.Is(err, expr.ErrInternal), err)
			is.Equal(t, "s4ql: internal error: "+c.want, err.Error())
		}
	})
}

func TestProgram_Type(t *testing.T) {
	tests := []struct {
		name string
		src  string
		// typ is the type of the program, 0 if it depends on the arguments, and bound the type with
		// args.
		typ   expr.Type
		args  []*s4qlpb.Value
		bound expr.Type
	}{
		{name: "should give an integer for integer columns and literals", src: "a * (b - 3) / -a", typ: expr.TypeInteger, bound: expr.TypeInteger},
		{name: "should give a real for a real column", src: "a + c", typ: expr.TypeReal, bound: expr.TypeReal},
		{name: "should give a real for a real literal", src: "1.0 * a", typ: expr.TypeReal, bound: expr.TypeReal},
		{name: "should give the type of the operand for a negation", src: "-c", typ: expr.TypeReal, bound: expr.TypeReal},
		{name: "should give the type of a column alone", src: "b", typ: expr.TypeInteger, bound: expr.TypeInteger},
		{name: "should give the type of an integer argument when it is bound", src: "a + ?", args: []*s4qlpb.Value{integer(1)}, bound: expr.TypeInteger},
		{name: "should give the type of a real argument when it is bound", src: "a + ?", args: []*s4qlpb.Value{float(1)}, bound: expr.TypeReal},
		{name: "should give NULL for a NULL argument", src: "a + -?", args: []*s4qlpb.Value{null()}, bound: expr.TypeNull},
		{name: "should give NULL for a NULL argument beside a real", src: "(c + ?) * 2.5", args: []*s4qlpb.Value{null()}, bound: expr.TypeNull},
		{name: "should give a real for one of two arguments that is a real", src: "? * ?", args: []*s4qlpb.Value{integer(1), float(2)}, bound: expr.TypeReal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e, _, err := parse(test.src)
			is.NotError(t, err)
			x, err := expr.Check(e, resolve)
			is.NotError(t, err)
			typ, ok := x.Type()
			is.Equal(t, test.typ, typ)
			is.Equal(t, test.typ != 0, ok)
			b, err := x.Bind(test.args)
			is.NotError(t, err)
			is.Equal(t, test.bound, b.Type())
		})
	}
}

func TestProgram_Bind(t *testing.T) {
	t.Run("should take the arguments of the whole statement, by the numbers of the parameters", func(t *testing.T) {
		e, _, err := parse("? - ?")
		is.NotError(t, err)
		e.Operands[0].Parameter, e.Operands[1].Parameter = 3, 1
		x, err := expr.Check(e, resolve)
		is.NotError(t, err)
		b, err := x.Bind([]*s4qlpb.Value{integer(10), text("2"), integer(100)})
		is.NotError(t, err)
		v, err := b.Eval(nil)
		is.NotError(t, err)
		is.Equal(t, expr.IntegerValue(90), v)
	})

	t.Run("should refuse the first of two refused arguments in the order of the text, whatever their kinds", func(t *testing.T) {
		e, full, err := parse("? + ?")
		is.NotError(t, err)
		x, err := expr.Check(e, resolve)
		is.NotError(t, err)
		first := strings.Index(full, "?")
		for _, c := range []struct {
			args []*s4qlpb.Value
			what string
		}{
			{[]*s4qlpb.Value{text("1"), blob([]byte("1"))}, "argument 1 in arithmetic, which is text"},
			{[]*s4qlpb.Value{blob([]byte("1")), text("1")}, "argument 1 in arithmetic, which is a blob"},
		} {
			_, err := x.Bind(c.args)
			var ns *stmt.NotSupportedError
			is.True(t, errors.As(err, &ns), "a *stmt.NotSupportedError")
			is.Equal(t, c.what, ns.What)
			is.Equal(t, first, ns.Offset)
		}
	})

	t.Run("should give an internal error that names the fault for a parameter without an argument, or one of no kind", func(t *testing.T) {
		e, _, err := parse("a + ?")
		is.NotError(t, err)
		x, err := expr.Check(e, resolve)
		is.NotError(t, err)
		for _, c := range []struct {
			args []*s4qlpb.Value
			want string
		}{
			{nil, "parameter 1 of 0 arguments"},
			{[]*s4qlpb.Value{{}}, "argument 1 of no kind"},
		} {
			_, err := x.Bind(c.args)
			is.True(t, errors.Is(err, expr.ErrInternal), err)
			is.Equal(t, "s4ql: internal error: "+c.want, err.Error())
		}
	})

	t.Run("should take an argument that is not finite, whose arithmetic gives an error that the result is not finite or a value", func(t *testing.T) {
		e, full, err := parse("c + ? * d")
		is.NotError(t, err)
		x, err := expr.Check(e, resolve)
		is.NotError(t, err)
		for _, c := range []struct {
			arg, d float64
			at     string
		}{
			{math.NaN(), 1, "? * d"},
			{math.Inf(1), 0, "? * d"},
			{math.Inf(-1), 1, "? * d"},
		} {
			b, err := x.Bind([]*s4qlpb.Value{float(c.arg)})
			is.NotError(t, err)
			_, err = b.Eval(row(integer(0), integer(0), float(1), float(c.d)))
			var ee *expr.Error
			is.True(t, errors.As(err, &ee), "an *expr.Error")
			is.True(t, errors.Is(err, expr.ErrNotFinite), err)
			is.Equal(t, c.at, full[ee.Span.Start:ee.Span.End])
		}
	})

	t.Run("should give an internal error for the zero Program", func(t *testing.T) {
		_, err := (&expr.Program{}).Bind(nil)
		is.True(t, errors.Is(err, expr.ErrInternal), err)
		is.Equal(t, "s4ql: internal error: a program that is not checked", err.Error())
	})
}

func TestBound_Eval_faults(t *testing.T) {
	e, _, err := parse("a + c")
	is.NotError(t, err)
	x, err := expr.Check(e, resolve)
	is.NotError(t, err)
	b, err := x.Bind(nil)
	is.NotError(t, err)

	t.Run("should give an internal error that names the fault for a row without the column", func(t *testing.T) {
		_, err := b.Eval([]*s4qlpb.Value{integer(1)})
		is.True(t, errors.Is(err, expr.ErrInternal), err)
		is.Equal(t, "s4ql: internal error: the column at 2 of a row of 1 values", err.Error())
		_, err = b.Eval([]*s4qlpb.Value{integer(1), integer(2)})
		is.Equal(t, "s4ql: internal error: the column at 2 of a row of 2 values", err.Error())
	})

	t.Run("should give an internal error that names the fault for a value of another type than its column", func(t *testing.T) {
		for _, c := range []struct {
			row  []*s4qlpb.Value
			want string
		}{
			{row(float(1), integer(0), float(1), float(0)), "a value of type real in a column of type integer"},
			{row(integer(1), integer(0), integer(1), float(0)), "a value of type integer in a column of type real"},
			{row(text("1"), integer(0), float(1), float(0)), "a value of kind text in arithmetic"},
			{row(&s4qlpb.Value{}, integer(0), float(1), float(0)), "a value of kind not set in arithmetic"},
		} {
			_, err := b.Eval(c.row)
			is.True(t, errors.Is(err, expr.ErrInternal), err)
			is.Equal(t, "s4ql: internal error: "+c.want, err.Error())
		}
	})

	t.Run("should give an internal error for arithmetic of an operator that no program holds, of integers and of reals", func(t *testing.T) {
		for _, c := range []struct {
			l, r expr.Value
		}{
			{expr.IntegerValue(1), expr.IntegerValue(2)},
			{expr.RealValue(1), expr.IntegerValue(2)},
		} {
			v, err := expr.Arithmetic(stmt.ExprNegate, c.l, c.r)
			is.True(t, errors.Is(err, expr.ErrInternal), err)
			is.True(t, !errors.Is(err, expr.ErrEvaluation), err)
			is.Equal(t, "s4ql: internal error: arithmetic of the operator -", err.Error())
			is.Equal(t, expr.Value{}, v)
		}
	})

	t.Run("should give an internal error for the zero Bound", func(t *testing.T) {
		_, err := expr.Bound{}.Eval(zeros)
		is.True(t, errors.Is(err, expr.ErrInternal), err)
		is.Equal(t, "s4ql: internal error: an expression that is not bound", err.Error())
	})
}

func TestBound_Eval_allocations(t *testing.T) {
	t.Run("should allocate nothing for a row", func(t *testing.T) {
		e, _, err := parse("(a + ? * 3) / (b - 1) + -c * 2.5 - d / 7")
		is.NotError(t, err)
		x, err := expr.Check(e, resolve)
		is.NotError(t, err)
		b, err := x.Bind([]*s4qlpb.Value{integer(4)})
		is.NotError(t, err)
		r := row(integer(5), integer(3), float(1.5), float(-2))
		var v expr.Value
		allocations := testing.AllocsPerRun(100, func() {
			v, err = b.Eval(r)
		})
		is.NotError(t, err)
		// (5 + 4 * 3) / (3 - 1) is the integer 8.
		want := 8 + -3.75
		want -= -2.0 / 7
		is.Equal(t, expr.RealValue(want), v)
		// The address sanitizer moves values to the heap that the compiler keeps on the stack
		// otherwise: 14 allocations here.
		if !asanEnabled {
			is.Equal(t, 0.0, allocations)
		}
	})

	t.Run("should allocate nothing to bind an expression without a parameter", func(t *testing.T) {
		e, _, err := parse("(a + 3) / (b - 1) + -c * 2.5 - d / 7")
		is.NotError(t, err)
		x, err := expr.Check(e, resolve)
		is.NotError(t, err)
		var b expr.Bound
		allocations := testing.AllocsPerRun(100, func() {
			b, err = x.Bind(nil)
		})
		is.NotError(t, err)
		is.Equal(t, expr.TypeReal, b.Type())
		if !asanEnabled {
			is.Equal(t, 0.0, allocations)
		}
	})
}

func BenchmarkBound_Eval(b *testing.B) {
	e, _, err := parse("(a + ? * 3) / (b - 1) + -c * 2.5 - d / 7")
	if err != nil {
		b.Fatal(err)
	}
	x, err := expr.Check(e, resolve)
	if err != nil {
		b.Fatal(err)
	}
	bound, err := x.Bind([]*s4qlpb.Value{integer(4)})
	if err != nil {
		b.Fatal(err)
	}
	r := row(integer(5), integer(3), float(1.5), float(-2))
	for b.Loop() {
		if _, err := bound.Eval(r); err != nil {
			b.Fatal(err)
		}
	}
}
