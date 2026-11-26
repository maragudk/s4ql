// Package expr evaluates the arithmetic of a [stmt.Expression]: +, -, *, and / on integers and
// reals, and - before an operand, over the values of a row and the arguments of a statement. For
// every expression it gives the value that SQLite gives, bit for bit and of the same type, or an
// error, and never another value.
//
// An expression is checked once, before any row is read, by [Check], which finds its columns and
// refuses one of type TEXT or BLOB. Once the arguments of the statement are known, [Program.Bind]
// refuses an argument that is text or a blob, and gives the type of the result. [Bound.Eval] then
// computes the value for a row, without allocating, or gives an [*Error] for an integer overflow, a
// division by zero, or a real result that is not finite, where SQLite gives a real, NULL, or an
// infinity.
package expr

import (
	"errors"
	"fmt"
	"math"
	"math/bits"

	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// Type of the values of an expression.
type Type int

// The types of an expression. The zero value is no type.
const (
	// TypeInteger is an expression whose every value is an integer or NULL.
	TypeInteger Type = iota + 1
	// TypeReal is an expression whose every value is a real or NULL.
	TypeReal
	// TypeNull is an expression whose every value is NULL, because an argument of it is NULL.
	TypeNull
)

// String of the type: "integer", "real", or "null", or "" for no type.
func (t Type) String() string {
	switch t {
	case TypeInteger:
		return "integer"
	case TypeReal:
		return "real"
	case TypeNull:
		return "null"
	}
	return ""
}

// Column of a table, as an expression reads it from a row: the position of its value in the row,
// from 0, and its declared type.
type Column struct {
	Position int
	Type     s4qlpb.Type
}

// Program of a checked expression, with its columns resolved to their positions in a row, in the
// form that [Bound.Eval] computes. It is not changed after [Check], so it is safe for concurrent use.
// The zero Program is not checked, and its [Program.Bind] fails.
type Program struct {
	// nodes of the expression, each operator after its operands, so that the last is the root.
	nodes []node
	// typ is the type of the expression, or 0 if it has a parameter.
	typ Type
}

// node of a [Program]: a literal, a parameter, a column, or an operator.
type node struct {
	kind stmt.ExpressionKind
	// value of a literal.
	value Value
	// index of the value of a column in the row, or of the argument of a parameter, from 0.
	index int
	// typ of a column: [TypeInteger] or [TypeReal].
	typ Type
	// left and right operands of an operator, as indexes in the nodes. A negation has a left one only,
	// and nothing reads them for another node.
	left, right int32
	// span of the node in the text of the statement.
	span stmt.Span
}

// Check the expression e, before any row is read, with resolve to find each column that it names:
// the [Program], or an error. The error is the first that resolve gives, in the order of the text, or
// else the [stmt.NotSupportedError] "column of type TEXT in arithmetic" or "column of type BLOB in
// arithmetic" for the first column of such a type, at its offset. Every name is resolved before any
// type is looked at, so that an error of resolve, such as SQLite's for a column that the table does
// not have, comes before a refusal for the type of another column, as in SQLite, which finds the
// names when it prepares the statement. A column alone, as in `set a = b`, is an expression too, and
// is refused for the same types.
func Check(e stmt.Expression, resolve func(stmt.Name) (Column, error)) (*Program, error) {
	columns := map[string]Column{}
	if err := resolveColumns(e, resolve, columns); err != nil {
		return nil, err
	}
	x := &Program{}
	if _, err := x.add(e, columns); err != nil {
		return nil, err
	}
	if !x.hasParameter() {
		typ, err := x.typeOf(nil)
		if err != nil {
			return nil, err
		}
		x.typ = typ
	}
	return x, nil
}

// resolveColumns of e with resolve into columns, by their folded names, in the order of the text:
// the first error of resolve, or nil.
func resolveColumns(e stmt.Expression, resolve func(stmt.Name) (Column, error), columns map[string]Column) error {
	if e.Kind == stmt.ExprColumn {
		c, err := resolve(e.Column)
		if err != nil {
			return err
		}
		columns[e.Column.Fold()] = c
	}
	for _, o := range e.Operands {
		if err := resolveColumns(o, resolve, columns); err != nil {
			return err
		}
	}
	return nil
}

// add the node e and its operands to x, its operands first, with the columns that [Check] resolved
// by their folded names, and give the index of e in the nodes.
func (x *Program) add(e stmt.Expression, columns map[string]Column) (int32, error) {
	n := node{kind: e.Kind, span: e.Span}
	operands := 0
	switch e.Kind {
	case stmt.ExprValue:
		switch v := e.Value.(type) {
		case stmt.Integer:
			n.value = integerValue(int64(v))
		case stmt.Real:
			n.value = realValue(float64(v))
		default:
			return 0, internalError("a literal of %T in arithmetic", e.Value)
		}
	case stmt.ExprParameter:
		if e.Parameter < 1 {
			return 0, internalError("a parameter numbered %v", e.Parameter)
		}
		n.index = e.Parameter - 1
	case stmt.ExprColumn:
		c := columns[e.Column.Fold()]
		switch c.Type {
		case s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INTEGER:
			n.typ = TypeInteger
		case s4qlpb.Type_TYPE_REAL:
			n.typ = TypeReal
		case s4qlpb.Type_TYPE_TEXT:
			return 0, &stmt.NotSupportedError{What: "column of type TEXT in arithmetic", Offset: e.Span.Start}
		case s4qlpb.Type_TYPE_BLOB:
			return 0, &stmt.NotSupportedError{What: "column of type BLOB in arithmetic", Offset: e.Span.Start}
		default:
			return 0, internalError("a column of type %v", c.Type)
		}
		if c.Position < 0 {
			return 0, internalError("a column at position %v", c.Position)
		}
		n.index = c.Position
	case stmt.ExprNegate:
		operands = 1
	case stmt.ExprAdd, stmt.ExprSubtract, stmt.ExprMultiply, stmt.ExprDivide:
		operands = 2
	default:
		return 0, internalError("an expression of kind %v", int(e.Kind))
	}
	if len(e.Operands) != operands {
		return 0, internalError("%v with %v operands", e.Kind, len(e.Operands))
	}
	var err error
	if operands > 0 {
		if n.left, err = x.add(e.Operands[0], columns); err != nil {
			return 0, err
		}
	}
	if operands > 1 {
		if n.right, err = x.add(e.Operands[1], columns); err != nil {
			return 0, err
		}
	}
	x.nodes = append(x.nodes, n)
	return int32(len(x.nodes) - 1), nil
}

// hasParameter reports whether a node of x is a parameter.
func (x *Program) hasParameter() bool {
	for _, n := range x.nodes {
		if n.kind == stmt.ExprParameter {
			return true
		}
	}
	return false
}

// Type of the values of x, and true, if x has no parameter: [TypeInteger] or [TypeReal]. It is 0 and
// false if x has one, since its type then depends on the arguments, and [Bound.Type] gives it.
func (x *Program) Type() (Type, bool) {
	return x.typ, x.typ != 0
}

// typeOf x with the arguments args: the type of a literal, a column, or an argument is that of its
// value, that of a negation that of its operand, and that of a binary operator [TypeNull] if an
// operand is NULL, which only an argument is for every row, or else [TypeReal] if an operand is a
// real, and [TypeInteger] otherwise. The error is a refusal for the first parameter, in the order of
// the text, whose argument is text or a blob.
func (x *Program) typeOf(args []*s4qlpb.Value) (Type, error) {
	types := make([]Type, len(x.nodes))
	for i, n := range x.nodes {
		switch n.kind {
		case stmt.ExprValue:
			types[i] = n.value.Type()
		case stmt.ExprColumn:
			types[i] = n.typ
		case stmt.ExprParameter:
			if n.index >= len(args) {
				return 0, internalError("parameter %v of %v arguments", n.index+1, len(args))
			}
			switch a := args[n.index]; a.WhichKind() {
			case s4qlpb.Value_Integer_case:
				types[i] = TypeInteger
			case s4qlpb.Value_Real_case:
				types[i] = TypeReal
			case s4qlpb.Value_Null_case:
				types[i] = TypeNull
			case s4qlpb.Value_Text_case:
				return 0, &stmt.NotSupportedError{What: fmt.Sprintf("argument %v in arithmetic, which is text", n.index+1), Offset: n.span.Start}
			case s4qlpb.Value_Blob_case:
				return 0, &stmt.NotSupportedError{What: fmt.Sprintf("argument %v in arithmetic, which is a blob", n.index+1), Offset: n.span.Start}
			default:
				return 0, internalError("argument %v of no kind", n.index+1)
			}
		case stmt.ExprNegate:
			types[i] = types[n.left]
		default:
			switch l, r := types[n.left], types[n.right]; {
			case l == TypeNull || r == TypeNull:
				types[i] = TypeNull
			case l == TypeReal || r == TypeReal:
				types[i] = TypeReal
			default:
				types[i] = TypeInteger
			}
		}
	}
	return types[len(types)-1], nil
}

// Bound is a [Program] with the arguments of its statement, from [Program.Bind]. The zero Bound is
// not bound, and its [Bound.Eval] fails.
type Bound struct {
	x    *Program
	args []*s4qlpb.Value
	typ  Type
}

// Bind the arguments args of the statement to x, by the numbers of its parameters, which count from
// 1: the [Bound], or the [stmt.NotSupportedError] "argument 2 in arithmetic, which is text" or
// "argument 2 in arithmetic, which is a blob" for the first parameter of x, in the order of the
// text, whose argument is text or a blob. Bound holds args, which must not change while it is used.
// The arguments are not checked otherwise: a real that is not finite, which s4ql refuses as an
// argument before, gives what the arithmetic gives, an error that the result is not finite or a
// value. An expression without a parameter takes any args, also nil, and allocates nothing.
func (x *Program) Bind(args []*s4qlpb.Value) (Bound, error) {
	if len(x.nodes) == 0 {
		return Bound{}, internalError("a program that is not checked")
	}
	if x.typ != 0 {
		return Bound{x: x, args: args, typ: x.typ}, nil
	}
	typ, err := x.typeOf(args)
	if err != nil {
		return Bound{}, err
	}
	return Bound{x: x, args: args, typ: typ}, nil
}

// Type of the values of b: [TypeInteger], [TypeReal], or [TypeNull].
func (b Bound) Type() Type {
	return b.typ
}

// Eval b on the row, which holds the values of the columns of the table at their positions: its
// value, which has the [Bound.Type] of b or is NULL, or an [*Error]. It allocates nothing but an
// error.
//
// Each operator computes its left operand, then its right one, then itself, and the first error
// ends the evaluation. An operator with a NULL operand is NULL, and an error of its other operand
// still comes, so that whether an operator fails depends on its own operands only. Integers give an
// integer, and / truncates toward zero; an operand that is a real makes the operation real, and an
// integer operand is converted to the nearest real. A - before an operand is 0 minus the operand, as
// SQLite computes it, so that the negation of the real 0.0 or -0.0 is 0.0.
func (b Bound) Eval(row []*s4qlpb.Value) (Value, error) {
	if b.x == nil {
		return Value{}, internalError("an expression that is not bound")
	}
	return evalNode(b.x.nodes, int32(len(b.x.nodes)-1), row, b.args)
}

// evalNode of the nodes at the index i on the row with the arguments args.
func evalNode(nodes []node, i int32, row, args []*s4qlpb.Value) (Value, error) {
	n := &nodes[i]
	switch n.kind {
	case stmt.ExprValue:
		return n.value, nil
	case stmt.ExprParameter:
		return valueOf(args[n.index], 0)
	case stmt.ExprColumn:
		if n.index >= len(row) {
			return Value{}, internalError("the column at %v of a row of %v values", n.index, len(row))
		}
		return valueOf(row[n.index], n.typ)
	case stmt.ExprNegate:
		v, err := evalNode(nodes, n.left, row, args)
		if err != nil || v.kind == kindNull {
			return v, err
		}
		return arithmetic(stmt.ExprSubtract, integerValue(0), v, n)
	}
	l, err := evalNode(nodes, n.left, row, args)
	if err != nil {
		return Value{}, err
	}
	r, err := evalNode(nodes, n.right, row, args)
	if err != nil {
		return Value{}, err
	}
	if l.kind == kindNull || r.kind == kindNull {
		return Value{}, nil
	}
	return arithmetic(n.kind, l, r, n)
}

// arithmetic of the operator op on the values l and r, neither NULL, for the node n, whose kind and
// span an error names.
func arithmetic(op stmt.ExpressionKind, l, r Value, n *node) (Value, error) {
	if l.kind == kindInteger && r.kind == kindInteger {
		v, kind := integerArithmetic(op, int64(l.bits), int64(r.bits))
		switch {
		case errors.Is(kind, ErrInternal):
			return Value{}, kind
		case kind != nil:
			return Value{}, &Error{Kind: kind, Op: n.kind, Span: n.span}
		}
		return integerValue(v), nil
	}
	a, b := l.float(), r.float()
	var f float64
	// Each result is converted to float64 explicitly, which rounds it, so that the compiler may not
	// fuse a multiplication and an addition into one instruction, as SQLite never does.
	switch op {
	case stmt.ExprAdd:
		f = float64(a + b)
	case stmt.ExprSubtract:
		f = float64(a - b)
	case stmt.ExprMultiply:
		f = float64(a * b)
	case stmt.ExprDivide:
		if b == 0 {
			return Value{}, &Error{Kind: ErrDivisionByZero, Op: n.kind, Span: n.span}
		}
		f = float64(a / b)
	default:
		return Value{}, internalError("arithmetic of the operator %v", op)
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return Value{}, &Error{Kind: ErrNotFinite, Op: n.kind, Span: n.span}
	}
	return realValue(f), nil
}

// integerArithmetic of the operator op on a and b: the result, or the kind of the error, an
// overflow or a division by zero, or an internal error for an operator that is not arithmetic.
func integerArithmetic(op stmt.ExpressionKind, a, b int64) (int64, error) {
	switch op {
	case stmt.ExprAdd:
		c := a + b
		if (a^c)&(b^c) < 0 {
			return 0, ErrOverflow
		}
		return c, nil
	case stmt.ExprSubtract:
		c := a - b
		if (a^b)&(a^c) < 0 {
			return 0, ErrOverflow
		}
		return c, nil
	case stmt.ExprMultiply:
		hi, lo := bits.Mul64(magnitude(a), magnitude(b))
		negative := (a < 0) != (b < 0)
		switch {
		case hi != 0, !negative && lo > math.MaxInt64, negative && lo > 1<<63:
			return 0, ErrOverflow
		case negative:
			return int64(-lo), nil
		}
		return int64(lo), nil
	case stmt.ExprDivide:
		switch {
		case b == 0:
			return 0, ErrDivisionByZero
		case a == math.MinInt64 && b == -1:
			return 0, ErrOverflow
		}
		return a / b, nil
	}
	return 0, internalError("arithmetic of the operator %v", op)
}

// magnitude of i, its absolute value, which fits a uint64 also for the smallest integer.
func magnitude(i int64) uint64 {
	if i < 0 {
		return -uint64(i)
	}
	return uint64(i)
}
