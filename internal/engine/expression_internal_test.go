package engine

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/expr"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestLeftmostOffset(t *testing.T) {
	t.Run("should give the offset of the first leaf, after the sign of a negative literal, or of the name of a call, as SQLite gives it for row value misused", func(t *testing.T) {
		// SQLite gives row value misused for these with a subquery of * of two columns on the right at
		// the offsets that the comments have, measured with the pinned shell.
		for _, c := range []struct {
			src  string
			want int
		}{
			{"select * from t where k + 1 = 1", 22},
			{"select * from t where -5 + k = 1", 23},
			{"select * from t where lower(v) = 1", 22},
			{"select * from t where   LOWER (v) != 1", 24},
			{"select * from t where hex(lower(v)) < 1", 22},
			{"select * from t where lower(v) || 'a' = 1", 22},
			{"select * from t where v || 'a' = 1", 22},
		} {
			s, err := stmt.Check(c.src)
			is.NotError(t, err, c.src)
			is.Equal(t, c.want, leftmostOffset(s.(*stmt.Select).Where[0].Expression), c.src)
		}
	})
}

func TestArgumentLengths(t *testing.T) {
	t.Run("should give the internal error of a parameter that the arguments do not have, which no statement gives", func(t *testing.T) {
		args := []*s4qlpb.Value{integer(1), integer(2)}
		for _, n := range []int{0, 3} {
			e := stmt.Expression{Kind: stmt.ExprAdd, Operands: []stmt.Expression{{Kind: stmt.ExprParameter, Parameter: 1}, {Kind: stmt.ExprParameter, Parameter: n}}}
			err := argumentLengths(e, args, 1000)
			is.Equal(t, fmt.Sprintf("s4ql: internal error: the parameter %v of a statement with 2 arguments", n), err.Error())
		}
		for _, n := range []int{1, 2} {
			is.NotError(t, argumentLengths(stmt.Expression{Kind: stmt.ExprParameter, Parameter: n}, args, 1000))
		}
	})
}

func TestFromExpr(t *testing.T) {
	t.Run("should give a fault of the evaluator as an internal error of the engine, a text too long as SQLite's string or blob too big, and every other error as it is", func(t *testing.T) {
		fault := fmt.Errorf("%w: a row without its column", expr.ErrInternal)
		err := fromExpr(fault)
		is.True(t, errors.Is(err, ErrInternal))
		is.Equal(t, "s4ql: internal error: "+fault.Error(), err.Error())

		refusal := fmt.Errorf("%w: column of type TEXT in arithmetic", stmt.ErrNotSupported)
		is.Equal(t, refusal, fromExpr(refusal))

		long := &expr.Error{Kind: expr.ErrTooLong, Op: stmt.ExprCall, Function: stmt.FuncHex}
		is.Equal(t, tooBig().Error(), fromExpr(long).Error())
		_, isSQLite := errors.AsType[*sqlerr.Error](fromExpr(long))
		is.True(t, isSQLite)
		// A write that fails so before any row sets the count of changes to 0, and one that fails with
		// SQLite's error of an argument that is too long leaves it.
		is.Equal(t, Result{SetsChanges: true}, built(&Delete{}, fromExpr(long)))
		is.Equal(t, Result{}, built(&Delete{}, tooBig()))
		is.Equal(t, Result{}, built(&Query{}, fromExpr(long)))
		overflow := &expr.Error{Kind: expr.ErrOverflow, Op: stmt.ExprAdd}
		is.Equal(t, error(overflow), fromExpr(overflow))
	})
}

func TestClass_String(t *testing.T) {
	t.Run("should name each class, and the class of arithmetic whose every value is NULL as null", func(t *testing.T) {
		for c, want := range map[class]string{classAny: "null", classInteger: "integer", classReal: "real", classText: "text", classBlob: "blob", classNumber: "null"} {
			is.Equal(t, want, c.String())
		}
	})
}

func TestComparable(t *testing.T) {
	t.Run("should let one class meet itself and NULL meet every class, arithmetic of NULL meet numbers only, and nothing else", func(t *testing.T) {
		all := []class{classAny, classInteger, classReal, classText, classBlob, classNumber}
		meets := map[[2]class]bool{}
		for _, c := range all {
			meets[[2]class{c, c}], meets[[2]class{classAny, c}], meets[[2]class{c, classAny}] = true, true, true
		}
		for _, c := range []class{classInteger, classReal} {
			meets[[2]class{classNumber, c}], meets[[2]class{c, classNumber}] = true, true
		}
		for _, a := range all {
			for _, b := range all {
				is.Equal(t, meets[[2]class{a, b}], comparable(a, b), fmt.Sprint(a, " ", b))
			}
		}
	})
}

func TestExpressionClass(t *testing.T) {
	t.Run("should give text whose every value is NULL the class text, and arithmetic of NULL its own class", func(t *testing.T) {
		table := newFake().with("t", []string{"k", "a"}).tables["t"].ct
		null := []*s4qlpb.Value{null()}
		for src, want := range map[string]class{"lower(?)": classText, "? || ?": classText, "hex(?)": classText, "? + 1": classNumber} {
			s, err := stmt.Check("select * from t where k = " + src)
			is.NotError(t, err, src)
			e := s.(*stmt.Select).Where[0].Value.Expression
			b, err := bindExpression(table, e, slices.Repeat(null, strings.Count(src, "?")), 1000)
			is.NotError(t, err, src)
			is.Equal(t, want, expressionClass(e, b), src)
		}
	})
}

func TestCompareScalars(t *testing.T) {
	t.Run("should compare integers and reals by their number and text by its bytes, and give the internal error of two classes or of a class that it does not compare, which the checks of the types rule out", func(t *testing.T) {
		n, err := compareScalars(scalar{class: classInteger, i: 1}, scalar{class: classInteger, i: 2})
		is.NotError(t, err)
		is.Equal(t, -1, n)
		n, err = compareScalars(scalar{class: classReal, f: 0}, scalar{class: classReal, f: math.Copysign(0, -1)})
		is.NotError(t, err)
		is.Equal(t, 0, n)
		for _, c := range []struct {
			a, b string
			want int
		}{{"a", "b", -1}, {"b", "a", 1}, {"a", "ab", -1}, {"B", "a", -1}, {"\u00e9", "z", 1}, {"", "", 0}} {
			n, err = compareScalars(scalar{class: classText, s: c.a}, scalar{class: classText, s: c.b})
			is.NotError(t, err)
			is.Equal(t, c.want, n, c.a+" "+c.b)
		}
		for _, c := range []struct {
			a, b scalar
			err  string
		}{
			{a: scalar{class: classInteger, i: 1}, b: scalar{class: classReal, f: 1}, err: "a comparison of values of the classes integer and real"},
			{a: scalar{class: classReal, f: 1}, b: scalar{class: classInteger, i: 1}, err: "a comparison of values of the classes real and integer"},
			{a: scalar{class: classText, s: "1"}, b: scalar{class: classInteger, i: 1}, err: "a comparison of values of the classes text and integer"},
			{a: scalar{}, b: scalar{}, err: "a comparison of values of class null"},
			{a: scalar{class: classBlob}, b: scalar{class: classBlob}, err: "a comparison of values of class blob"},
		} {
			n, err := compareScalars(c.a, c.b)
			is.Equal(t, 0, n, c.err)
			is.Equal(t, "s4ql: internal error: "+c.err, fmt.Sprint(err))
		}
	})

	t.Run("should give the internal error of a blob, which no expression that a condition compares gives", func(t *testing.T) {
		_, err := protoScalar(s4qlpb.Value_builder{Blob: []byte{1}}.Build())
		is.Equal(t, "s4ql: internal error: a value of kind blob in a comparison of an expression", fmt.Sprint(err))
	})
}

func TestBindExpression(t *testing.T) {
	t.Run("should give the internal error of a column that the table does not have, which the walk over the statement rules out", func(t *testing.T) {
		table := newFake().with("t", []string{"k", "a"}).tables["t"].ct
		e := stmt.Expression{Kind: stmt.ExprAdd, Operands: []stmt.Expression{
			{Kind: stmt.ExprColumn, Column: stmt.Name{Value: "a"}},
			{Kind: stmt.ExprColumn, Column: stmt.Name{Value: "b"}},
		}}
		_, err := bindExpression(table, e, nil, 1000)
		is.Equal(t, "s4ql: internal error: an expression of the column b, which the table t does not have", err.Error())
	})
}

func TestResolver(t *testing.T) {
	t.Run("should end the walk at a missing column, but go on after one inside a call, which a later missing column replaces and any other node ends the walk with", func(t *testing.T) {
		for _, c := range []struct{ src, want string }{
			{"lower(s) || s", ""},
			{"x || s", "x"},
			{"s || x || y", "x"},
			{"lower(x)", "x"},
			{"lower(x) || y", "y"},
			{"lower(x) || s", "x"},
			{"lower(x) || s || y", "y"},
			{"lower(x) || 'a'", "x"},
			{"lower(x) || ?", "x"},
			{"lower(x) || lower(y)", "y"},
			{"lower(x) || lower(s)", "x"},
			{"lower(x || y)", "x"},
			{"lower(hex(x) || y)", "y"},
			{"lower(hex(x) || s) || 'a'", "x"},
			{"(lower(x) || s) || (s || y)", "x"},
			{"x + y", "x"},
		} {
			s, err := stmt.Check("update t set s = " + c.src)
			is.NotError(t, err, c.src)
			r := &resolver{found: func(n stmt.Name) bool { return n.Value == "s" }}
			name, missing := r.all(s.(*stmt.Update).Set[0].Value.Expression)
			is.Equal(t, c.want != "", missing, c.src)
			is.Equal(t, c.want, name.Value, c.src)
		}
	})
}

func TestHoisted(t *testing.T) {
	t.Run("should give the concatenations without a column and without a call that SQLite computes before any row, but not the value of SET itself", func(t *testing.T) {
		for _, c := range []struct {
			src  string
			set  bool
			want []string
		}{
			{"? || ? || s", true, []string{"? || ?"}},
			{"? || ? || s", false, []string{"? || ?"}},
			{"? || ?", true, nil},
			{"? || ?", false, []string{"? || ?"}},
			{"lower(? || ?)", false, nil},
			{"hex(?) || (? || ?)", true, []string{"? || ?"}},
			{"(? || ?) || lower(?)", true, []string{"? || ?"}},
			{"hex(?) || (? || ?)", false, nil},
			{"lower(?)", true, nil},
			{"? || ? || ?", true, []string{"? || ?"}},
			{"s || ? || ?", true, nil},
			{"s || (? || ?)", true, []string{"? || ?"}},
			{"lower(? || ?) || s", true, nil},
			{"lower(? || ?)", true, nil},
			{"lower(? || ? || s)", true, []string{"? || ?"}},
			{"hex(?) || s", false, nil},
			{"'a' || ? || lower(s) || (? || 'b')", false, []string{"'a' || ?", "? || 'b'"}},
			{"s + 1", true, nil},
		} {
			s, err := stmt.Check("update t set s = " + c.src)
			is.NotError(t, err, c.src)
			src := "update t set s = " + c.src
			var got []string
			for _, e := range hoisted(s.(*stmt.Update).Set[0].Value.Expression, c.set) {
				got = append(got, src[e.Span.Start:e.Span.End])
			}
			is.EqualSlice(t, c.want, got, c.src)
		}
	})
}
