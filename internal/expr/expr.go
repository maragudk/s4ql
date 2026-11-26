// Package expr evaluates a [stmt.Expression] over the values of a row and the arguments of a
// statement: the arithmetic +, -, *, and / on integers and reals, and - before an operand; the
// concatenation || of text; and the functions lower and hex. For every expression it gives the value
// that SQLite gives, bit for bit and of the same type, or an error, and never another value.
//
// An expression is checked once, before any row is read, by [Check], which finds its columns and
// refuses one of a type that its place does not take: text or a blob in arithmetic, a number or a
// blob in a concatenation or as the argument of lower, and a number as the argument of hex. Once the
// arguments of the statement are known, [Program.Bind] refuses an argument of such a type in the same
// way, and gives the type of the result. [Bound.Eval] then computes the value for a row, or gives an
// [*Error]: for an integer overflow, a division by zero, or a real result that is not finite, where
// SQLite gives a real, NULL, or an infinity; and for a text longer than the length limit, where
// SQLite fails too. Arithmetic allocates nothing; an operation that makes new text allocates it.
package expr

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strings"

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
	// TypeText is an expression whose every value is text or NULL: a concatenation, a call, or a
	// column of type TEXT alone.
	TypeText
	// TypeBlob is an expression whose every value is a blob or NULL: a column of type BLOB alone.
	TypeBlob
)

// String of the type: "integer", "real", "null", "text", or "blob", or "" for no type.
func (t Type) String() string {
	switch t {
	case TypeInteger:
		return "integer"
	case TypeReal:
		return "real"
	case TypeNull:
		return "null"
	case TypeText:
		return "text"
	case TypeBlob:
		return "blob"
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

// place of a node: what the node above it takes.
type place uint8

const (
	// placeRoot is the root, where a column alone takes every type.
	placeRoot place = iota
	// placeArithmetic is an operand of arithmetic, which takes a number.
	placeArithmetic
	// placeConcat is an operand of ||, which takes text.
	placeConcat
	// placeLower is the argument of lower, which takes text.
	placeLower
	// placeHex is the argument of hex, which takes text or a blob.
	placeHex
)

// String of the place, as a refusal names it: "in arithmetic".
func (p place) String() string {
	switch p {
	case placeConcat:
		return "in a concatenation"
	case placeLower:
		return "in a call of lower"
	case placeHex:
		return "in a call of hex"
	}
	return "in arithmetic"
}

// takes reports whether a value of the type t may stand in the place p.
func (p place) takes(t Type) bool {
	switch p {
	case placeConcat, placeLower:
		return t == TypeText
	case placeHex:
		return t == TypeText || t == TypeBlob
	case placeRoot:
		return true
	}
	return t == TypeInteger || t == TypeReal
}

// placeOf the operands of a node of the kind k and the function f.
func placeOf(k stmt.ExpressionKind, f stmt.Function) place {
	switch {
	case k == stmt.ExprConcat:
		return placeConcat
	case k == stmt.ExprCall && f == stmt.FuncLower:
		return placeLower
	case k == stmt.ExprCall:
		return placeHex
	}
	return placeArithmetic
}

// node of a [Program]: a literal, a parameter, a column, an operator, or a call.
type node struct {
	kind stmt.ExpressionKind
	// function of a call.
	function stmt.Function
	// place of the node.
	place place
	// value of a literal.
	value Value
	// index of the value of a column in the row, or of the argument of a parameter, from 0.
	index int
	// typ of a column: [TypeInteger], [TypeReal], [TypeText], or [TypeBlob].
	typ Type
	// left and right operands of an operator, as indexes in the nodes. A negation and a call have a
	// left one only, and nothing reads them for another node.
	left, right int32
	// span of the node in the text of the statement.
	span stmt.Span
}

// Check the expression e, before any row is read, with resolve to find each column that it names:
// the [Program], or an error. The error is the first that resolve gives, in the order of the text, or
// else the [stmt.NotSupportedError] for the first column, in the order of the text, of a type that
// its place does not take, at its offset: "column of type TEXT in arithmetic" or "column of type BLOB
// in arithmetic" for an operand of arithmetic; "column of type INT in a concatenation" for one of ||,
// of a type other than TEXT; "column of type REAL in a call of lower" for the argument of lower, of a
// type other than TEXT; and "column of type INTEGER in a call of hex" for the argument of hex, of a
// type other than TEXT and BLOB. Every name is resolved before any type is looked at, so that an
// error of resolve, such as SQLite's for a column that the table does not have, comes before a
// refusal for the type of another column, as in SQLite, which finds the names when it prepares the
// statement. A column alone, as in `set a = b`, is an expression too, of the type of its column.
func Check(e stmt.Expression, resolve func(stmt.Name) (Column, error)) (*Program, error) {
	columns := map[string]Column{}
	if err := resolveColumns(e, resolve, columns); err != nil {
		return nil, err
	}
	x := &Program{}
	if _, err := x.add(e, placeRoot, columns); err != nil {
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

// typeOfColumn of the declared type t, or 0 for no type.
func typeOfColumn(t s4qlpb.Type) Type {
	switch t {
	case s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INTEGER:
		return TypeInteger
	case s4qlpb.Type_TYPE_REAL:
		return TypeReal
	case s4qlpb.Type_TYPE_TEXT:
		return TypeText
	case s4qlpb.Type_TYPE_BLOB:
		return TypeBlob
	}
	return 0
}

// add the node e, in the place p, and its operands to x, its operands first, with the columns that
// [Check] resolved by their folded names, and give the index of e in the nodes.
func (x *Program) add(e stmt.Expression, p place, columns map[string]Column) (int32, error) {
	if p == placeRoot && (e.Kind == stmt.ExprValue || e.Kind == stmt.ExprParameter) {
		// A literal or a parameter alone is no expression of the pass, and is held to the rules of
		// arithmetic.
		p = placeArithmetic
	}
	n := node{kind: e.Kind, place: p, span: e.Span}
	operands := 0
	switch e.Kind {
	case stmt.ExprValue:
		switch v := e.Value.(type) {
		case stmt.Integer:
			n.value = integerValue(int64(v))
		case stmt.Real:
			n.value = realValue(float64(v))
		case stmt.Text:
			n.value = textValue(string(v))
		case stmt.Blob:
			n.value = blobValue(string(v))
		}
		if n.value.Null() || !p.takes(n.value.Type()) {
			return 0, internalError("a literal of %T %v", e.Value, p)
		}
	case stmt.ExprParameter:
		if e.Parameter < 1 {
			return 0, internalError("a parameter numbered %v", e.Parameter)
		}
		n.index = e.Parameter - 1
	case stmt.ExprColumn:
		c := columns[e.Column.Fold()]
		n.typ = typeOfColumn(c.Type)
		switch {
		case n.typ == 0:
			return 0, internalError("a column of type %v", c.Type)
		case !p.takes(n.typ):
			what := fmt.Sprintf("column of type %v %v", strings.TrimPrefix(c.Type.String(), "TYPE_"), p)
			return 0, &stmt.NotSupportedError{What: what, Offset: e.Span.Start}
		case c.Position < 0:
			return 0, internalError("a column at position %v", c.Position)
		}
		n.index = c.Position
	case stmt.ExprNegate:
		operands = 1
	case stmt.ExprAdd, stmt.ExprSubtract, stmt.ExprMultiply, stmt.ExprDivide, stmt.ExprConcat:
		operands = 2
	case stmt.ExprCall:
		if len(e.Operands) != 1 || e.Function.String() == "" {
			return 0, internalError("a call of function %v with %v operands", int(e.Function), len(e.Operands))
		}
		n.function = e.Function
		operands = 1
	default:
		return 0, internalError("an expression of kind %v", int(e.Kind))
	}
	if len(e.Operands) != operands {
		return 0, internalError("%v with %v operands", e.Kind, len(e.Operands))
	}
	if operands > 0 {
		// The pass never puts text in arithmetic, nor arithmetic in a concatenation or a call.
		text := e.Kind == stmt.ExprConcat || e.Kind == stmt.ExprCall
		if text && p == placeArithmetic || !text && p != placeArithmetic && p != placeRoot {
			return 0, internalError("%v %v", nodeName(e.Kind, e.Function), p)
		}
	}
	operandPlace := placeOf(e.Kind, e.Function)
	var err error
	if operands > 0 {
		if n.left, err = x.add(e.Operands[0], operandPlace, columns); err != nil {
			return 0, err
		}
	}
	if operands > 1 {
		if n.right, err = x.add(e.Operands[1], operandPlace, columns); err != nil {
			return 0, err
		}
	}
	x.nodes = append(x.nodes, n)
	return int32(len(x.nodes) - 1), nil
}

// nodeName of an operator of the kind k, or of a call of the function f, in a message: "+", "||",
// or "lower".
func nodeName(k stmt.ExpressionKind, f stmt.Function) string {
	if k == stmt.ExprCall {
		return f.String()
	}
	return k.String()
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

// Type of the values of x, and true, if x has no parameter: [TypeInteger], [TypeReal], [TypeText],
// or [TypeBlob]. It is 0 and false if x has one, since its type then depends on the arguments, and
// [Bound.Type] gives it.
func (x *Program) Type() (Type, bool) {
	return x.typ, x.typ != 0
}

// typeOf x with the arguments args: the type of a literal, a column, or an argument is that of its
// value; that of a negation that of its operand; that of an operator of arithmetic [TypeNull] if an
// operand is NULL, which only an argument is for every row, or else [TypeReal] if an operand is a
// real, and [TypeInteger] otherwise; that of a concatenation and of lower [TypeNull] if an operand
// is NULL, and [TypeText] otherwise; and that of hex [TypeText], since hex(NULL) is the empty
// text. The error is a refusal for the first parameter, in the order of the text, whose argument is
// of a type that its place does not take.
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
			var name string
			switch a := args[n.index]; a.WhichKind() {
			case s4qlpb.Value_Integer_case:
				types[i], name = TypeInteger, "an integer"
			case s4qlpb.Value_Real_case:
				types[i], name = TypeReal, "a real"
			case s4qlpb.Value_Text_case:
				types[i], name = TypeText, "text"
			case s4qlpb.Value_Blob_case:
				types[i], name = TypeBlob, "a blob"
			case s4qlpb.Value_Null_case:
				types[i] = TypeNull
			default:
				return 0, internalError("argument %v of no kind", n.index+1)
			}
			if p := n.place; types[i] != TypeNull && !p.takes(types[i]) {
				return 0, &stmt.NotSupportedError{What: fmt.Sprintf("argument %v %v, which is %v", n.index+1, p, name), Offset: n.span.Start}
			}
		case stmt.ExprNegate:
			types[i] = types[n.left]
		case stmt.ExprConcat:
			types[i] = TypeText
			if types[n.left] == TypeNull || types[n.right] == TypeNull {
				types[i] = TypeNull
			}
		case stmt.ExprCall:
			types[i] = TypeText
			if n.function == stmt.FuncLower && types[n.left] == TypeNull {
				types[i] = TypeNull
			}
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

// Bound is a [Program] with the arguments of its statement and the length limit, from
// [Program.Bind]. The zero Bound is not bound, and its [Bound.Eval] fails.
type Bound struct {
	x     *Program
	args  []*s4qlpb.Value
	typ   Type
	limit int
}

// Bind the arguments args of the statement to x, by the numbers of its parameters, which count from
// 1, with the length limit of the database, the largest text in bytes that SQLite makes, which must
// be positive: the [Bound], or the [stmt.NotSupportedError] for the first parameter of x, in the
// order of the text, whose argument is of a type that its place does not take: "argument 2 in
// arithmetic, which is text" or "argument 2 in arithmetic, which is a blob" for an operand of
// arithmetic; "argument 2 in a concatenation, which is an integer", "..., which is a real", or "...,
// which is a blob" for an operand of ||, and the same "in a call of lower" for the argument of lower;
// and "argument 2 in a call of hex, which is an integer" or "..., which is a real" for the argument
// of hex. Bound holds args, which must not change while it is used. The arguments are not checked
// otherwise: a real that is not finite, which s4ql refuses as an argument before, gives what the
// arithmetic gives, an error that the result is not finite or a value, and a text or a blob longer
// than the limit, which SQLite refuses when it binds it, gives the error of the first operation that
// makes a text longer than the limit, or NULL. An expression without a parameter takes any args, also
// nil, and allocates nothing.
func (x *Program) Bind(args []*s4qlpb.Value, limit int) (Bound, error) {
	switch {
	case len(x.nodes) == 0:
		return Bound{}, internalError("a program that is not checked")
	case limit < 1:
		return Bound{}, internalError("a length limit of %v", limit)
	case x.typ != 0:
		return Bound{x: x, args: args, typ: x.typ, limit: limit}, nil
	}
	typ, err := x.typeOf(args)
	if err != nil {
		return Bound{}, err
	}
	return Bound{x: x, args: args, typ: typ, limit: limit}, nil
}

// Type of the values of b: [TypeInteger], [TypeReal], [TypeText], [TypeBlob], or [TypeNull].
func (b Bound) Type() Type {
	return b.typ
}

// Eval b on the row, which holds the values of the columns of the table at their positions: its
// value, which has the [Bound.Type] of b or is NULL, or an [*Error].
//
// Each operator computes its left operand, then its right one, then itself, and the first error
// ends the evaluation. An operator with a NULL operand is NULL, and an error of its other operand
// still comes, so that whether an operator fails depends on its own operands only.
//
// Integers give an integer, and / truncates toward zero; an operand that is a real makes the
// operation real, and an integer operand is converted to the nearest real. A - before an operand is
// 0 minus the operand, as SQLite computes it, so that the negation of the real 0.0 or -0.0 is 0.0.
//
// || joins the bytes of its operands; lower changes the ASCII letters A to Z to a to z and leaves
// every other byte as it is, as SQLite does without ICU; and hex writes each byte of a blob, or of
// the UTF-8 of a text, as two upper-case hexadecimal digits, and is the empty text for NULL. Each
// is an [ErrTooLong] where SQLite gives `string or blob too big`: a concatenation longer than the
// limit, lower of a text as long as the limit or longer, and hex of a text or a blob at least half
// as long as the limit, since SQLite allocates one byte more than the result.
//
// Arithmetic allocates nothing but an error, and so does a text operation whose result is one of its
// operands: a concatenation with the empty text, lower of a text without an upper-case ASCII letter,
// and hex of the empty text, a blob, or NULL. Any other text operation allocates its result once.
// The text of a value is not copied from the row, the arguments, or a literal, so a value may share
// its bytes with them; Go's strings never change, so the caller may keep a value after the row
// changes, and a [Value.Proto] of it.
func (b Bound) Eval(row []*s4qlpb.Value) (Value, error) {
	if b.x == nil {
		return Value{}, internalError("an expression that is not bound")
	}
	return b.evalNode(int32(len(b.x.nodes)-1), row)
}

// evalNode of the nodes at the index i on the row.
func (b Bound) evalNode(i int32, row []*s4qlpb.Value) (Value, error) {
	n := &b.x.nodes[i]
	switch n.kind {
	case stmt.ExprValue:
		return n.value, nil
	case stmt.ExprParameter, stmt.ExprColumn:
		p, err := b.leaf(n, row)
		if err != nil {
			return Value{}, err
		}
		if n.kind == stmt.ExprParameter {
			return valueOf(p, 0)
		}
		return valueOf(p, n.typ)
	case stmt.ExprNegate:
		v, err := b.evalNode(n.left, row)
		if err != nil || v.kind == kindNull {
			return v, err
		}
		return arithmetic(stmt.ExprSubtract, integerValue(0), v, n)
	case stmt.ExprCall:
		if n.function == stmt.FuncHex {
			return b.hex(n, row)
		}
		v, err := b.evalNode(n.left, row)
		if err != nil || v.kind == kindNull {
			return v, err
		}
		// SQLite allocates a byte more than the text, for the NUL at its end, and refuses that
		// allocation where it is larger than the limit.
		if len(v.text)+1 > b.limit {
			return Value{}, &Error{Kind: ErrTooLong, Op: n.kind, Function: n.function, Span: n.span}
		}
		return textValue(lower(v.text)), nil
	}
	l, err := b.evalNode(n.left, row)
	if err != nil {
		return Value{}, err
	}
	r, err := b.evalNode(n.right, row)
	if err != nil {
		return Value{}, err
	}
	if l.kind == kindNull || r.kind == kindNull {
		return Value{}, nil
	}
	if n.kind == stmt.ExprConcat {
		if len(l.text)+len(r.text) > b.limit {
			return Value{}, &Error{Kind: ErrTooLong, Op: n.kind, Span: n.span}
		}
		return textValue(l.text + r.text), nil
	}
	return arithmetic(n.kind, l, r, n)
}

// leaf of the parameter or the column n: its value in the arguments or the row, which is not
// checked for the type of the column.
func (b Bound) leaf(n *node, row []*s4qlpb.Value) (*s4qlpb.Value, error) {
	if n.kind == stmt.ExprParameter {
		return b.args[n.index], nil
	}
	if n.index >= len(row) {
		return nil, internalError("the column at %v of a row of %v values", n.index, len(row))
	}
	return row[n.index], nil
}

// hex of the argument of the call n on the row. The bytes of an argument or a column are read
// where they are, so that a blob is not copied.
func (b Bound) hex(n *node, row []*s4qlpb.Value) (Value, error) {
	var s string
	var bs []byte
	switch o := &b.x.nodes[n.left]; o.kind {
	case stmt.ExprParameter, stmt.ExprColumn:
		p, err := b.leaf(o, row)
		if err != nil {
			return Value{}, err
		}
		var typ Type
		switch p.WhichKind() {
		case s4qlpb.Value_Text_case:
			s, typ = p.GetText(), TypeText
		case s4qlpb.Value_Blob_case:
			bs, typ = p.GetBlob(), TypeBlob
		case s4qlpb.Value_Null_case:
		default:
			return Value{}, internalError("a value of kind %v", p.WhichKind())
		}
		if o.kind == stmt.ExprColumn && typ != 0 && typ != o.typ {
			return Value{}, internalError("a value of type %v in a column of type %v", typ, o.typ)
		}
	default:
		v, err := b.evalNode(n.left, row)
		if err != nil {
			return Value{}, err
		}
		s = v.text
	}
	// SQLite allocates two bytes for each byte and one more, for the NUL at the end, and refuses
	// that allocation where it is larger than the limit.
	if 2*(len(s)+len(bs))+1 > b.limit {
		return Value{}, &Error{Kind: ErrTooLong, Op: n.kind, Function: n.function, Span: n.span}
	}
	if bs != nil {
		return textValue(hexOf(bs)), nil
	}
	return textValue(hexOf(s)), nil
}

// lower of s: s with the ASCII letters A to Z as a to z, and every other byte as it is. It is s
// itself where s has no such letter.
func lower(s string) string {
	i := 0
	for i < len(s) && (s[i] < 'A' || s[i] > 'Z') {
		i++
	}
	if i == len(s) {
		return s
	}
	var t strings.Builder
	t.Grow(len(s))
	t.WriteString(s[:i])
	for ; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		t.WriteByte(c)
	}
	return t.String()
}

// hexOf the bytes of s: two upper-case hexadecimal digits for each, or "" for none.
func hexOf[T string | []byte](s T) string {
	if len(s) == 0 {
		return ""
	}
	const digits = "0123456789ABCDEF"
	var t strings.Builder
	t.Grow(2 * len(s))
	for i := range len(s) {
		t.WriteByte(digits[s[i]>>4])
		t.WriteByte(digits[s[i]&0xf])
	}
	return t.String()
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
