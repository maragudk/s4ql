package engine

import (
	"cmp"
	"errors"
	"fmt"
	"math"

	"maragu.dev/s4ql/internal/expr"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// The arithmetic of the piece of work arithmetic: the operators evaluate the expressions of a
// statement with the evaluator of [expr], in three steps, each where SQLite gives the errors of its
// kind.
//
//   - The names of an expression are looked up where the walk over the statement reaches it, in the
//     order of the text, as SQLite resolves them when it prepares the statement: see [missingName].
//   - Once SQLite would have given every error of the names and of the lengths, each expression is
//     checked against the table and bound to the arguments, and its type is checked against what it
//     is compared with or assigned to, by the rule of exact types: see [bindArithmetic] and
//     [comparable].
//   - An expression without a column is a constant of one run of the statement: it is computed once,
//     after the checks of the statement that come before its access is chosen, and serves a lookup by
//     the key or a bound of an index as an argument does. An expression with a column is computed for
//     each row that the statement reads or changes, and never serves the access.

// missingName of the expression e: the first column, in the order of the text, for which found
// reports false, and true; or the zero Name and false if it finds every one.
func missingName(e stmt.Expression, found func(stmt.Name) bool) (stmt.Name, bool) {
	if e.Kind == stmt.ExprColumn && !found(e.Column) {
		return e.Column, true
	}
	for _, o := range e.Operands {
		if name, missing := missingName(o, found); missing {
			return name, true
		}
	}
	return stmt.Name{}, false
}

// leftmostOffset of the expression e: the offset of its first leaf in the text, where SQLite gives
// the offset of an error of a comparison whose left side is e. A negative literal starts at its sign,
// and SQLite gives the offset of the number after it, which is one more where the sign and the number
// have no space between them, as they mostly have. A call is a node with an offset of its own, its
// name, where SQLite stops.
func leftmostOffset(e stmt.Expression) int {
	for len(e.Operands) > 0 && e.Kind != stmt.ExprCall {
		e = e.Operands[0]
	}
	switch v := e.Value.(type) {
	case stmt.Integer:
		if v < 0 {
			return e.Span.Start + 1
		}
	case stmt.Real:
		if math.Signbit(float64(v)) {
			return e.Span.Start + 1
		}
	}
	return e.Span.Start
}

// argumentLengths of the expression e, with the arguments args: SQLite's error for the first argument
// of its parameters, in the order of the text, that is longer than the limit, as SQLite gives it when
// it binds the argument, before [bindArithmetic] refuses a text or a blob in arithmetic. The zero
// Expression has no parameter.
func argumentLengths(e stmt.Expression, args []*s4qlpb.Value, limit int) error {
	if e.Kind == stmt.ExprParameter {
		if e.Parameter < 1 || e.Parameter > len(args) {
			return internalError("the parameter %v of a statement with %v arguments", e.Parameter, len(args))
		}
		return checkLength(args[e.Parameter-1], limit)
	}
	for _, o := range e.Operands {
		if err := argumentLengths(o, args, limit); err != nil {
			return err
		}
	}
	return nil
}

// bindArithmetic checks the expression e, whose every column the table t has, and binds it to the
// arguments args: the [expr.Bound], or "not supported" for a column of type TEXT or BLOB, the first
// in the order of the text, and then for an argument that is text or a blob, as [expr.Check] and
// [expr.Program.Bind] refuse them. A fault of the evaluator is an internal error.
func bindArithmetic(t *s4qlpb.CreateTable, e stmt.Expression, args []*s4qlpb.Value) (*expr.Bound, error) {
	of := columnPositions(t)
	columns := t.GetColumns()
	program, err := expr.Check(e, func(name stmt.Name) (expr.Column, error) {
		position, ok := of[name.Fold()]
		if !ok {
			return expr.Column{}, internalError("an expression of the column %v, which the table %v does not have", name.Value, t.GetTable())
		}
		return expr.Column{Position: position, Type: columns[position].GetType()}, nil
	})
	if err != nil {
		return nil, fromExpr(err)
	}
	bound, err := program.Bind(args)
	if err != nil {
		return nil, fromExpr(err)
	}
	return &bound, nil
}

// fromExpr gives the error err of the evaluator as the engine gives it: a fault of the evaluator,
// which matches [expr.ErrInternal], as an internal error of the engine, and every other error, a
// refusal or an evaluation error, as it is.
func fromExpr(err error) error {
	if errors.Is(err, expr.ErrInternal) {
		return internalError("%v", err)
	}
	return err
}

// class of a value for the rule of exact types: the kind of the values that a side of a comparison
// or an assignment gives, which must be the same on both sides. classAny is the class of NULL, and
// of an expression whose every value is NULL, which meet every class.
type class int

const (
	classAny class = iota
	classInteger
	classReal
	classText
	classBlob
)

func (c class) String() string {
	switch c {
	case classInteger:
		return "integer"
	case classReal:
		return "real"
	case classText:
		return "text"
	case classBlob:
		return "blob"
	}
	return "null"
}

// columnClass of a column of the type typ: one for INT and INTEGER.
func columnClass(typ s4qlpb.Type) class {
	switch typ {
	case s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INTEGER:
		return classInteger
	case s4qlpb.Type_TYPE_REAL:
		return classReal
	case s4qlpb.Type_TYPE_TEXT:
		return classText
	case s4qlpb.Type_TYPE_BLOB:
		return classBlob
	}
	return classAny
}

// arithmeticClass of the bound expression b.
func arithmeticClass(b *expr.Bound) class {
	switch b.Type() {
	case expr.TypeInteger:
		return classInteger
	case expr.TypeReal:
		return classReal
	}
	return classAny
}

// valueClass of the value v.
func valueClass(v *s4qlpb.Value) class {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return classInteger
	case s4qlpb.Value_Real_case:
		return classReal
	case s4qlpb.Value_Text_case:
		return classText
	case s4qlpb.Value_Blob_case:
		return classBlob
	}
	return classAny
}

// comparable reports whether values of the classes a and b may meet in a comparison or an
// assignment: where they are the same, or either is that of NULL.
func comparable(a, b class) bool {
	return a == b || a == classAny || b == classAny
}

// meetsArithmetic reports whether a value of the class c, of a column or of a plain value, may meet an
// arithmetic expression of the class of arithmetic a in a comparison or an assignment, as
// [comparable] says. Text and blobs never do, whatever a is, also the class of NULL of an expression
// whose argument is NULL: SQLite converts a number to text or a blob by the affinity of a column, and
// orders a number before text and blobs elsewhere, so the rule refuses them from the types alone.
func meetsArithmetic(c, a class) bool {
	return c != classText && c != classBlob && comparable(c, a)
}

// expressionName of an expression of the class c in a message: "an expression of type integer".
func expressionName(c class) string {
	return fmt.Sprint("an expression of type ", c)
}

// valueName of the value v of the plain expression e, a literal or an argument, in a message, as
// [checkType] names it: "an integer literal", or "argument 1, which is text,".
func valueName(e Expr, v *s4qlpb.Value) (string, error) {
	switch e := e.(type) {
	case *Parameter:
		return fmt.Sprintf("argument %v, which is %v,", e.Number, kindName(v)), nil
	case *Integer:
		return "an integer literal", nil
	case *Real:
		return "a real literal", nil
	case *Text:
		return "a string literal", nil
	case *Blob:
		return "a blob literal", nil
	}
	return "", internalError("a value of an expression of type %T", e)
}

// columnName of the column at position of the table t in a message: "the column t.a of type INT",
// with the names as the table stores them.
func columnName(t *s4qlpb.CreateTable, position int) string {
	c := t.GetColumns()[position]
	return fmt.Sprintf("the column %v.%v of type %v", t.GetTable(), c.GetName(), typeName(c.GetType()))
}

// number is a value of a side of a comparison of arithmetic, an integer, a real, or NULL, which it
// compares without an allocation.
type number struct {
	class class
	i     int64
	f     float64
}

// numberOf the value v of an expression.
func numberOf(v expr.Value) number {
	if i, ok := v.Integer(); ok {
		return number{class: classInteger, i: i}
	}
	if f, ok := v.Real(); ok {
		return number{class: classReal, f: f}
	}
	return number{}
}

// protoNumber of the value v of a column or of a constant: an integer, a real, or NULL. Any other is
// an internal error, which the checks of the types rule out.
func protoNumber(v *s4qlpb.Value) (number, error) {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return number{class: classInteger, i: v.GetInteger()}, nil
	case s4qlpb.Value_Real_case:
		return number{class: classReal, f: v.GetReal()}, nil
	case s4qlpb.Value_Null_case:
		return number{}, nil
	}
	return number{}, internalError("a value of kind %v in a comparison of arithmetic", v.WhichKind())
}

// compareNumbers a and b, which are of one class and not NULL, as SQLite compares them: by their
// number, with 0.0 the same as -0.0.
func compareNumbers(a, b number) (int, error) {
	switch {
	case a.class != b.class:
		return 0, internalError("a comparison of values of the classes %v and %v", a.class, b.class)
	case a.class == classInteger:
		return cmp.Compare(a.i, b.i), nil
	case a.class == classReal:
		return cmp.Compare(a.f, b.f), nil
	}
	return 0, internalError("a comparison of values of class %v", a.class)
}
