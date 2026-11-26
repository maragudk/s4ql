package engine

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"strings"

	"maragu.dev/s4ql/internal/expr"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// The expressions of the pieces of work arithmetic and text functions: the operators evaluate the
// expressions of a statement with the evaluator of [expr], in three steps, each where SQLite gives the
// errors of its kind.
//
//   - The names of an expression are looked up where the walk over the statement reaches it, in the
//     order of the text, as SQLite resolves them when it prepares the statement: see [resolver].
//   - Once SQLite would have given every error of the names and of the lengths, each expression is
//     checked against the table and bound to the arguments, and its type is checked against what it
//     is compared with or assigned to, by the rule of exact types: see [bindExpression] and
//     [comparable].
//   - An expression without a column is a constant of one run of the statement: it is computed once,
//     after the checks of the statement that come before its access is chosen, and serves a lookup by
//     the key or a bound of an index as an argument does. An expression with a column is computed for
//     each row that the statement reads or changes, and never serves the access. Text is longer than
//     the length limit where SQLite gives its `string or blob too big`, and SQLite gives that error
//     where it computes the text, which is not always where the engine computes it: [hoisted] and
//     [build.constants] give it where SQLite does.

// isText reports whether the expression e is of text: a concatenation or a call at its root, whose
// operands are text too, as the pass gives them.
func isText(e stmt.Expression) bool {
	return e.Kind == stmt.ExprConcat || e.Kind == stmt.ExprCall
}

// hasCall reports whether a node of the expression e is a call.
func hasCall(e stmt.Expression) bool {
	if e.Kind == stmt.ExprCall {
		return true
	}
	for _, o := range e.Operands {
		if hasCall(o) {
			return true
		}
	}
	return false
}

// resolver of the expressions of one walk of SQLite over the names of a statement, as SQLite resolves
// them when it prepares it: found reports whether a table of the walk has a column of the name. A
// missing column ends the walk with SQLite's `no such column`, but in two places, where SQLite goes on
// and the column is pending: inside the argument of a call, whose node walks its argument itself and
// goes on whatever it found, and in a test for NULL, which does the same. A later missing column
// replaces a pending one, a column that the table has and a call go on beside it, and any other node
// that the walk reaches ends it with the pending column. So `hex(nosuch) = nosuch2` gives `no such
// column: nosuch2`, `hex(nosuch) || s || x = 'a'` gives x, and `lower(nosuch) = s and nosuch2 = 1`
// gives nosuch, at the comparison after it. A column of excluded, of DO UPDATE, is a column as any
// other: the row that the insert proposes has the columns of the table, so found reports for it
// whether the table has its column, and it is missing as [missingExcluded] names it.
type resolver struct {
	found   func(stmt.Name) bool
	pending *stmt.Name
}

// resolve the names of the expression e: the column that ends the walk, and true, or false where the
// walk goes on, perhaps with a pending column.
func (n *resolver) resolve(e stmt.Expression) (stmt.Name, bool) {
	switch e.Kind {
	case stmt.ExprColumn:
		if n.found(e.Column) {
			return stmt.Name{}, false
		}
		return e.Column, true
	case stmt.ExprExcluded:
		if n.found(e.Column) {
			return stmt.Name{}, false
		}
		return missingExcluded(e), true
	case stmt.ExprCall:
		for _, o := range e.Operands {
			if name, stop := n.resolve(o); stop {
				n.pending = &name
				break
			}
		}
		return stmt.Name{}, false
	}
	if n.pending != nil {
		return *n.pending, true
	}
	for _, o := range e.Operands {
		if name, stop := n.resolve(o); stop {
			return name, true
		}
	}
	return stmt.Name{}, false
}

// missingExcluded is the name of the column of excluded e as SQLite names it where the table does not have it,
// in `no such column: excluded.c`: the values of the two names as the statement writes them, at the
// offset of the first, and without the question about quotes.
func missingExcluded(e stmt.Expression) stmt.Name {
	name := e.Table.Value + "." + e.Column.Value
	return stmt.Name{Value: name, Raw: name, Offset: e.Table.Offset}
}

// all the names of the expression e, as a walk of its own: the column that ends it, also one pending
// at its end, and true, or false.
func (n *resolver) all(e stmt.Expression) (stmt.Name, bool) {
	if name, stop := n.resolve(e); stop {
		return name, true
	}
	if n.pending != nil {
		return *n.pending, true
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
// it binds the argument, before [bindExpression] refuses an argument of a type that its place does
// not take. The zero Expression has no parameter.
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

// bindExpression checks the expression e, whose every column the table t has, and binds it to the
// arguments args, with the length limit of the database: the [expr.Bound], or "not supported" for the
// first column, in the order of the text, of a type that its place does not take, and then for the
// first such argument, as [expr.Check] and [expr.Program.Bind] refuse them: a TEXT or BLOB column, or
// text or a blob, in arithmetic, and a number in a concatenation or a call, or a blob in a
// concatenation or a call of lower. A literal longer than the limit gives SQLite's `string or blob
// too big`, as [fromExpr] says. A fault of the evaluator is an internal error.
func bindExpression(t *s4qlpb.CreateTable, e stmt.Expression, args []*s4qlpb.Value, limit int) (*expr.Bound, error) {
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
	bound, err := program.Bind(args, limit)
	if err != nil {
		return nil, fromExpr(err)
	}
	return &bound, nil
}

// hoisted of the expression e: the subtrees of text that SQLite computes once, before any row, as it
// factors them out of its loop into the start of the statement. SQLite codes an operand, and a side of
// a comparison, as a temporary, which it factors out where it has no column: a concatenation without a
// call before any row, and one with a call, or a call, when the first row reaches it, with nothing
// under it factored out. It codes the value of an assignment of SET, and a constant that serves the
// access, in place, if inPlace, which factors out nothing but a call without a column, and then only
// to the first row, and codes its operands as temporaries. So `set s = ? || ? || s`, `set s = hex(?) ||
// (? || ?)`, and `where ? || ? || s = 'x'` compute `? || ?` before any row, and fail there where it is
// longer than the limit, also on an empty table, and `set s = ? || ?` and `where lower(? || ?) || s =
// 'x'` do not. A side of a comparison without a column is itself such a subtree where it has no call.
// Arithmetic has none, as it never fails in SQLite.
func hoisted(e stmt.Expression, inPlace bool) []stmt.Expression {
	if !isText(e) {
		return nil
	}
	var subtrees []stmt.Expression
	var walk func(e stmt.Expression, temporary bool)
	walk = func(e stmt.Expression, temporary bool) {
		if !e.HasColumn() {
			switch {
			case e.Kind == stmt.ExprCall || temporary && hasCall(e):
				return
			case temporary:
				if e.Kind == stmt.ExprConcat {
					subtrees = append(subtrees, e)
				}
				return
			}
		}
		for _, o := range e.Operands {
			walk(o, true)
		}
	}
	walk(e, !inPlace)
	return subtrees
}

// bindHoisted binds the subtrees of the expression e that [hoisted] gives, each as [bindExpression]
// binds it, in the order of the text. The expression e is bound already, so they give no error but
// a fault.
func bindHoisted(t *s4qlpb.CreateTable, e stmt.Expression, inPlace bool, args []*s4qlpb.Value, limit int) ([]*expr.Bound, error) {
	var bs []*expr.Bound
	for _, sub := range hoisted(e, inPlace) {
		b, err := bindExpression(t, sub, args, limit)
		if err != nil {
			return nil, err
		}
		bs = append(bs, b)
	}
	return bs, nil
}

// computeHoisted computes each of the bound subtrees bs, which have no column, once: SQLite's
// `string or blob too big` for the first, in their order, that is longer than the limit.
func computeHoisted(bs []*expr.Bound) error {
	for _, b := range bs {
		if _, err := b.Eval(nil); err != nil {
			return fromExpr(err)
		}
	}
	return nil
}

// fromExpr gives the error err of the evaluator as the engine gives it: a fault of the evaluator,
// which matches [expr.ErrInternal], as an internal error of the engine; a text longer than the length
// limit, [expr.ErrTooLong], as SQLite's own `string or blob too big`, which SQLite gives there too,
// when it steps the statement, as [computedTooBig] says; and every other error, a refusal or an
// evaluation error, as it is.
func fromExpr(err error) error {
	switch {
	case errors.Is(err, expr.ErrInternal):
		return internalError("%v", err)
	case errors.Is(err, expr.ErrTooLong):
		return &computedTooBig{err: tooBig()}
	}
	return err
}

// class of a value for the rule of exact types: the kind of the values that a side of a comparison
// or an assignment gives, which must be the same on both sides. classAny is the class of NULL, which
// meets every class, and classNumber that of arithmetic whose every value is NULL, which meets every
// class but text and blobs.
type class int

const (
	classAny class = iota
	classInteger
	classReal
	classText
	classBlob
	classNumber
)

// String of the class c, as a message names it: an expression of the class classNumber is "of type
// null", as the evaluator types it.
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

// expressionClass of the expression e, bound as b: the class of the type of b, and for an expression
// whose every value is NULL, because an argument of it is NULL, the class of what it is: classText
// for text, which never meets a number, and classNumber for arithmetic, which never meets text or a
// blob. A column alone has the class of its column.
func expressionClass(e stmt.Expression, b *expr.Bound) class {
	switch b.Type() {
	case expr.TypeInteger:
		return classInteger
	case expr.TypeReal:
		return classReal
	case expr.TypeText:
		return classText
	case expr.TypeBlob:
		return classBlob
	}
	if isText(e) {
		return classText
	}
	return classNumber
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
// assignment: where they are the same, or either is that of NULL, or one is classNumber and the other
// a number. So text and blobs never meet arithmetic, whatever its arguments, also arithmetic whose
// every value is NULL: SQLite converts a number to text or a blob by the affinity of a column, and
// orders a number before text and blobs elsewhere, so the rule refuses them from the types alone. And
// text never meets a number, also text whose every value is NULL: SQLite's `lower(v) > 1` is true for
// every row whose v is not NULL.
func comparable(a, b class) bool {
	number := func(c class) bool { return c == classInteger || c == classReal || c == classNumber }
	return a == b || a == classAny || b == classAny || (a == classNumber && number(b)) || (b == classNumber && number(a))
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
// with the names as the table stores them, or "the column excluded.a of type INT" for a column of the
// row that an upsert proposes, of its combined table, as [excludedName] names it.
func columnName(t *s4qlpb.CreateTable, position int) string {
	c := t.GetColumns()[position]
	if name, ok := strings.CutPrefix(c.GetName(), excludedPrefix); ok {
		return fmt.Sprintf("the column excluded.%v of type %v", name, typeName(c.GetType()))
	}
	return fmt.Sprintf("the column %v.%v of type %v", t.GetTable(), c.GetName(), typeName(c.GetType()))
}

// scalar is a value of a side of a comparison of an expression, an integer, a real, text, or NULL,
// which it compares without an allocation of its own.
type scalar struct {
	class class
	i     int64
	f     float64
	s     string
}

// scalarOf the value v of an expression. A blob, which no expression that a condition compares gives,
// is an internal error.
func scalarOf(v expr.Value) (scalar, error) {
	if i, ok := v.Integer(); ok {
		return scalar{class: classInteger, i: i}, nil
	}
	if f, ok := v.Real(); ok {
		return scalar{class: classReal, f: f}, nil
	}
	if s, ok := v.Text(); ok {
		return scalar{class: classText, s: s}, nil
	}
	if v.Null() {
		return scalar{}, nil
	}
	return scalar{}, internalError("a value of type %v in a comparison of an expression", v.Type())
}

// protoScalar of the value v of a column or of a constant: an integer, a real, text, or NULL. Any
// other is an internal error, which the checks of the types rule out.
func protoScalar(v *s4qlpb.Value) (scalar, error) {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return scalar{class: classInteger, i: v.GetInteger()}, nil
	case s4qlpb.Value_Real_case:
		return scalar{class: classReal, f: v.GetReal()}, nil
	case s4qlpb.Value_Text_case:
		return scalar{class: classText, s: v.GetText()}, nil
	case s4qlpb.Value_Null_case:
		return scalar{}, nil
	}
	return scalar{}, internalError("a value of kind %v in a comparison of an expression", v.WhichKind())
}

// compareScalars a and b, which are of one class and not NULL, as SQLite compares them: numbers by
// their number, with 0.0 the same as -0.0, and text by its bytes, as the collation BINARY does.
func compareScalars(a, b scalar) (int, error) {
	switch {
	case a.class != b.class:
		return 0, internalError("a comparison of values of the classes %v and %v", a.class, b.class)
	case a.class == classInteger:
		return cmp.Compare(a.i, b.i), nil
	case a.class == classReal:
		return cmp.Compare(a.f, b.f), nil
	case a.class == classText:
		return strings.Compare(a.s, b.s), nil
	}
	return 0, internalError("a comparison of values of class %v", a.class)
}
