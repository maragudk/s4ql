package stmttest_test

import (
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/expr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

// arithmeticColumns are the columns of the table t of the tests of the rules of arithmetic: a key of
// INT, an INTEGER, a REAL, a TEXT, and a BLOB column.
var arithmeticColumns = []stmttest.TableColumn{{Name: "k", Type: "INT", PrimaryKey: true}, {Name: "i", Type: "INTEGER", Nullable: true},
	{Name: "r", Type: "REAL", Nullable: true}, {Name: "s", Type: "TEXT", Nullable: true}, {Name: "b", Type: "BLOB", Nullable: true}}

// arithmeticColumnsOf the table of the name: t and u, of [arithmeticColumns], and none else.
func arithmeticColumnsOf(name string) (string, []stmttest.TableColumn) {
	if name == "t" || name == "u" {
		return name, arithmeticColumns
	}
	return "", nil
}

func TestArithmeticRefusals(t *testing.T) {
	tests := []struct {
		src      string
		args     []any
		readOnly bool
		want     []string
	}{
		{"select * from t where k = i + 1", nil, false, nil},
		{"select * from t where k = r + 1", nil, false, []string{"s4ql: not supported: an expression of type real compared with the column t.k of type INT"}},
		{"select * from t where s = k + 1", nil, false, []string{"s4ql: not supported: an expression of type integer compared with the column t.s of type TEXT"}},
		{"select * from t where k = s + 1", nil, false, []string{"s4ql: not supported: column of type TEXT in arithmetic (at offset 26)"}},
		{"select * from t where k = ? + 1", []any{"a"}, false, []string{"s4ql: not supported: argument 1 in arithmetic, which is text (at offset 26)"}},
		{"select * from t where k = ? + 1", []any{nil}, false, nil},
		{"select * from t where k = i", nil, false, nil},
		{"select * from t where k = r", nil, false, []string{"s4ql: not supported: the column t.r of type REAL compared with the column t.k of type INT"}},
		{"select * from t where k * 2 < r * 3", nil, false, []string{"s4ql: not supported: an expression of type real compared with an expression of type integer"}},
		{"update t set i = k + 1", nil, false, nil},
		{"update t set r = k + 1", nil, false, []string{"s4ql: not supported: an expression of type integer for the column t.r of type REAL"}},
		{"update t set r = k + 1", nil, true, nil},
		{"update t set r = i", nil, false, []string{"s4ql: not supported: the column t.i of type INTEGER for the column t.r of type REAL"}},
		{"update t set i = null, k = k + 1", nil, false, nil},
		{"select * from t where k = (select k from u where k + 1 = 'a')", nil, false, []string{"s4ql: not supported: a string literal compared with an expression of type integer"}},
		{"select * from t where k = (select k from u where k + 1 = 2 limit ?)", []any{-1.5}, false, []string{"s4ql: not supported: argument 1, which is a real, for LIMIT, which takes an integer from 0"}},
		{"select * from t where k + 0 = 1 limit ?", []any{"x"}, false, []string{"s4ql: not supported: argument 1, which is text, for LIMIT, which takes an integer from 0"}},
		{"select * from t where k = (select k from u where x + 1 = 2)", nil, false, []string{"s4ql: not supported: correlated subquery (at offset 49)"}},
		{"select * from t where k = -s", nil, false, []string{"s4ql: not supported: column of type TEXT in arithmetic (at offset 27)"}},
		{"select * from t where r = s + 1", nil, false, []string{"s4ql: not supported: column of type TEXT in arithmetic (at offset 26)"}},
		{"select * from t where -b < 1", nil, false, []string{"s4ql: not supported: column of type BLOB in arithmetic (at offset 23)"}},
		{"select * from t where k + ? = (select r from u)", []any{"a"}, false, []string{"s4ql: not supported: argument 1 in arithmetic, which is text (at offset 26)"}},
		{"update t set r = -s", nil, false, []string{"s4ql: not supported: column of type TEXT in arithmetic (at offset 18)"}},
		{"update t set k = ? * 2", []any{[]byte{1}}, false, []string{"s4ql: not supported: argument 1 in arithmetic, which is a blob (at offset 17)"}},
		{"select * from t where k + 1 = 5.5", nil, false, []string{"s4ql: not supported: a real literal compared with an expression of type integer"}},
		{"select * from t where 'a' = r * 2", nil, false, []string{"s4ql: not supported: a string literal compared with an expression of type real"}},
		{"select * from t where k + ? = ?", []any{nil, int64(1)}, false, nil},
		{"select * from t where k + ? = ?", []any{nil, "a"}, false, []string{"s4ql: not supported: argument 2, which is text, compared with an expression of type null"}},
		{"select * from t where k * 1 = (select r from u)", nil, false, []string{"s4ql: not supported: subquery 1, of the column u.r of type REAL, compared with an expression of type integer"}},
		{"select * from t where k * 1 = (select s from u)", nil, false, []string{"s4ql: not supported: subquery 1, of the column u.s of type TEXT, compared with an expression of type integer"}},
		{"select * from t where s = ? + 1", []any{nil}, false, []string{"s4ql: not supported: an expression of type null compared with the column t.s of type TEXT"}},
		{"update t set b = ? + 1", []any{nil}, false, []string{"s4ql: not supported: an expression of type null for the column t.b of type BLOB"}},
		{"update t set r = ? + 1", []any{nil}, false, nil},
		// Text has the one refusal of its root, and no class.
		{"update t set s = lower(s)", nil, false, []string{"s4ql: not supported: call of lower (at offset 17)"}},
		{"update t set k = 'a' || hex(b)", nil, false, []string{"s4ql: not supported: concatenation (at offset 17)"}},
		{"update t set s = lower(s)", nil, true, []string{"s4ql: not supported: call of lower (at offset 17)"}},
		{"select * from t where k = hex(?)", []any{"a"}, false, []string{"s4ql: not supported: call of hex (at offset 26)"}},
		{"select * from t where s || 'a' < r * 2", nil, false, []string{"s4ql: not supported: concatenation (at offset 22)"}},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("should give %q for %q with %v", test.want, test.src, test.args), func(t *testing.T) {
			s, err := stmt.Check(test.src)
			is.NotError(t, err)
			is.True(t, stmttest.HasExpression(s))
			got := stmttest.ArithmeticRefusals("t", arithmeticColumns, s, test.args, test.readOnly, arithmeticColumnsOf)
			is.Equal(t, fmt.Sprintf("%q", test.want), fmt.Sprintf("%q", got))
			for _, refusal := range got {
				is.True(t, stmttest.ArithmeticRefusal(refusal), refusal)
			}
		})
	}
}

func TestJustify(t *testing.T) {
	t.Run("should ask SQLite for the quirk of the operator that failed, for a row that the conditions before it hold for", func(t *testing.T) {
		s, err := stmt.Check("select * from t where i > 0 and k / (i - 1) = 2 and r < 1")
		is.NotError(t, err)
		var asked []string
		rows := func(src string, args []any) bool {
			asked = append(asked, fmt.Sprintf("%v %#v", src, args))
			return true
		}
		is.Equal(t, "", stmttest.Justify(s, nil, expr.ErrDivisionByZero, stmt.ExprDivide, 32, nil, rows))
		is.Equal(t, 1, len(asked))
		is.Equal(t, `select 1 from "t" where "i" > ? and "k" is not null and ("i" - ?) is not null and ("k" / ("i" - ?)) is null limit 1 []interface {}{0, 1, 1}`, asked[0])
	})

	t.Run("should refuse an error that no row justifies, and one at no operator of the statement", func(t *testing.T) {
		s, err := stmt.Check("update t set i = i + 1 where k = 1")
		is.NotError(t, err)
		none := func(string, []any) bool { return false }
		is.Equal(t, "SQLite's result of its operator justifies it for no row that the rule may evaluate", stmttest.Justify(s, nil, expr.ErrOverflow, stmt.ExprAdd, 17, nil, none))
		is.Equal(t, "is at no operator of the statement", stmttest.Justify(s, nil, expr.ErrOverflow, stmt.ExprAdd, 3, nil, none))
		is.Equal(t, "is at no operator of the statement", stmttest.Justify(s, nil, expr.ErrOverflow, stmt.ExprMultiply, 17, nil, none))
	})

	t.Run("should take a negation for a - of the message, and need no row for an expression without a column", func(t *testing.T) {
		s, err := stmt.Check("select * from t where k = -?")
		is.NotError(t, err)
		var asked []string
		rows := func(src string, args []any) bool {
			asked = append(asked, src)
			return true
		}
		e, ok := stmttest.EvaluationOf("s4ql: integer overflow in - (at offset 26)")
		is.True(t, ok)
		is.Equal(t, "", stmttest.Justify(s, []any{int64(-9223372036854775808)}, e.Kind, e.Op, e.Span.Start, nil, rows))
		is.Equal(t, "select 1 where typeof(?) = 'integer' and typeof(-(?)) = 'real'", asked[0])
	})
}

func TestJustifyWithin(t *testing.T) {
	t.Run("should ask for a row of the access too, for a site of the WHERE of the statement", func(t *testing.T) {
		s, err := stmt.Check("delete from t where k / (i - 1) = 2 and k > ?")
		is.NotError(t, err)
		var asked []string
		rows := func(src string, args []any) bool {
			asked = append(asked, fmt.Sprintf("%v %#v", src, args))
			return true
		}
		access := []stmt.Condition{{Column: stmt.Name{Value: "k"}, Operator: stmt.OpGreater, Value: stmt.Term{Value: stmt.Integer(3)}}}
		is.Equal(t, "", stmttest.JustifyWithin(s, []any{int64(3)}, expr.ErrDivisionByZero, stmt.ExprDivide, 20, nil, access, rows))
		is.Equal(t, 1, len(asked))
		is.Equal(t, `select 1 from "t" where "k" > ? and "k" is not null and ("i" - ?) is not null and ("k" / ("i" - ?)) is null limit 1 []interface {}{3, 1, 1}`, asked[0])
	})

	t.Run("should not ask for a row of the access for SET, which the whole WHERE guards, nor for a subquery", func(t *testing.T) {
		s, err := stmt.Check("update t set i = i + 1 where k = (select x from u where x * 2 = 4)")
		is.NotError(t, err)
		var asked []string
		rows := func(src string, args []any) bool {
			asked = append(asked, src)
			return false
		}
		access := []stmt.Condition{{Column: stmt.Name{Value: "k"}, Operator: stmt.OpGreater, Value: stmt.Term{Value: stmt.Integer(3)}}}
		is.Equal(t, "SQLite's result of its operator justifies it for no row that the rule may evaluate", stmttest.JustifyWithin(s, nil, expr.ErrOverflow, stmt.ExprAdd, 17, nil, access, rows))
		is.Equal(t, "SQLite's result of its operator justifies it for no row that the rule may evaluate", stmttest.JustifyWithin(s, nil, expr.ErrOverflow, stmt.ExprMultiply, 56, nil, access, rows))
		is.Equal(t, 2, len(asked))
		for _, src := range asked {
			is.True(t, !strings.Contains(src, `"k" > `), src)
		}
	})
}

func TestEvaluationOf(t *testing.T) {
	t.Run("should read the kind, the operator, and the offset of the message of an evaluation error, and nothing of another", func(t *testing.T) {
		for _, c := range []struct {
			message string
			kind    error
			op      stmt.ExpressionKind
			offset  int
		}{
			{"s4ql: integer overflow in + (at offset 17)", expr.ErrOverflow, stmt.ExprAdd, 17},
			{"s4ql: division by zero in / (at offset 3)", expr.ErrDivisionByZero, stmt.ExprDivide, 3},
			{"s4ql: real result that is not finite in * (at offset 0)", expr.ErrNotFinite, stmt.ExprMultiply, 0},
			{"s4ql: integer overflow in - (at offset 9)", expr.ErrOverflow, stmt.ExprSubtract, 9},
		} {
			e, ok := stmttest.EvaluationOf(c.message)
			is.True(t, ok, c.message)
			is.Equal(t, c.kind, e.Kind)
			is.Equal(t, c.op, e.Op)
			is.Equal(t, c.offset, e.Span.Start)
			is.Equal(t, c.message, (&expr.Error{Kind: c.kind, Op: c.op, Span: stmt.Span{Start: c.offset}}).Error())
		}
		_, ok := stmttest.EvaluationOf("s4ql: not supported: column of type TEXT in arithmetic (at offset 3)")
		is.True(t, !ok)
	})
}

func TestMissingColumn_arithmetic(t *testing.T) {
	t.Run("should look the names of a comparison with arithmetic up in the order of the text", func(t *testing.T) {
		for _, c := range []struct{ src, want string }{
			{"select * from t where k = x + y", "x"},
			{"select * from t where x + 1 = y", "x"},
			{"select * from t where 1 + y < x * 2", "y"},
			{"select * from t where k = i + y and z = 1", "y"},
			{"select * from t where z is null and k + y = 1", "z"},
			{"select * from t where k = i", ""},
		} {
			s, err := stmt.Check(c.src)
			is.NotError(t, err, c.src)
			name, missing := stmttest.MissingColumn(arithmeticColumns, s.(*stmt.Select))
			is.Equal(t, c.want != "", missing, c.src)
			is.Equal(t, c.want, name.Value, c.src)
		}
	})
}
