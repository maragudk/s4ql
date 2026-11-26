package stmttest

import (
	"fmt"
	"math"
	"strings"

	"maragu.dev/s4ql/internal/sql/stmt"
)

// The columns that an expression of [Generator.expression] may have.
const (
	noColumn = iota
	anyColumn
	withColumn
)

// arithmetic operators of an expression, with their text.
var arithmetic = []struct {
	kind stmt.ExpressionKind
	text string
}{{stmt.ExprAdd, "+"}, {stmt.ExprSubtract, "-"}, {stmt.ExprMultiply, "*"}, {stmt.ExprDivide, "/"}}

// precedence of the operator of the kind k, higher for one that binds more tightly, as in SQLite.
func precedence(k stmt.ExpressionKind) int {
	switch k {
	case stmt.ExprNegate:
		return 3
	case stmt.ExprMultiply, stmt.ExprDivide:
		return 2
	}
	return 1
}

// expressionTerm of arithmetic, the value of an assignment or of a comparison, after the arguments
// args of the parameters before it: an operator at random, with columns as [noColumn],
// [anyColumn], or [withColumn] says, one time in six in parentheses, or, if alone, one time in four a
// column alone. It gives the term and the arguments with those of its parameters.
func (g *Generator) expressionTerm(columns int, alone bool, args []any) (stmt.Term, []any) {
	start := g.b.Len()
	if alone && g.r.IntN(4) == 0 {
		e := g.column(g.pick(SelectColumn))
		return stmt.Term{Expression: e, Span: stmt.Span{Start: start, End: g.b.Len()}}, args
	}
	paren := g.r.IntN(6) == 0
	if paren {
		g.openParen()
		g.gap(false)
	}
	var e stmt.Expression
	e, args = g.operator(g.operatorKind(), 2, columns, args)
	if paren {
		g.gap(false)
		g.punct(")")
	}
	return stmt.Term{Expression: e, Span: stmt.Span{Start: start, End: g.b.Len()}}, args
}

// subjectOf a comparison, the column of the raw name in an operator, as the side that the condition
// tests, after the arguments args: its negation, or the column on either side of a binary operator
// whose other operand has no column. It gives the expression and the arguments with those of its
// parameters.
func (g *Generator) subjectOf(raw string, args []any) (stmt.Expression, []any) {
	start := g.b.Len()
	if g.r.IntN(3) == 0 {
		g.open = false
		g.punct("-")
		g.gap(false)
		x := g.column(raw)
		return stmt.Expression{Kind: stmt.ExprNegate, Operands: []stmt.Expression{x}, Span: stmt.Span{Start: start, End: g.b.Len()}}, args
	}
	op := arithmetic[g.r.IntN(len(arithmetic))]
	var x, y stmt.Expression
	if g.r.IntN(2) == 0 {
		x = g.column(raw)
		g.operatorText(op.text)
		y, args = g.operand(op.kind, false, 1, noColumn, args)
	} else {
		x, args = g.operand(op.kind, true, 1, noColumn, args)
		g.operatorText(op.text)
		y = g.column(raw)
	}
	return stmt.Expression{Kind: op.kind, Operands: []stmt.Expression{x, y}, Span: stmt.Span{Start: start, End: g.b.Len()}}, args
}

// operatorKind at random: a negation one time in five, and otherwise one of [arithmetic].
func (g *Generator) operatorKind() stmt.ExpressionKind {
	if g.r.IntN(5) == 0 {
		return stmt.ExprNegate
	}
	return arithmetic[g.r.IntN(len(arithmetic))].kind
}

// operator of the kind, without parentheses around it, after the arguments args, with operands of
// at most depth more levels of operators, and with columns as [noColumn], [anyColumn], or
// [withColumn] says. A negation is of a column, a parameter, or an operator in parentheses, and never
// of a literal, which would be a negative literal.
func (g *Generator) operator(kind stmt.ExpressionKind, depth, columns int, args []any) (stmt.Expression, []any) {
	start := g.b.Len()
	var operands []stmt.Expression
	if kind == stmt.ExprNegate {
		g.open = false
		g.punct("-")
		g.gap(false)
		var x stmt.Expression
		switch {
		case depth > 0 && g.r.IntN(3) == 0, columns == noColumn && g.Parameters == 0:
			g.openParen()
			g.gap(false)
			x, args = g.operator(arithmetic[g.r.IntN(len(arithmetic))].kind, depth-1, columns, args)
			g.gap(false)
			g.punct(")")
		case columns == withColumn || columns == anyColumn && (g.Parameters == 0 || g.r.IntN(2) == 0):
			x = g.column(g.pick(SelectColumn))
		default:
			x, args = g.parameter(args)
		}
		operands = []stmt.Expression{x}
	} else {
		text := ""
		for _, a := range arithmetic {
			if a.kind == kind {
				text = a.text
			}
		}
		// A required column is on one side at random, and the other may have one.
		left, right := columns, columns
		if columns == withColumn {
			if g.r.IntN(2) == 0 {
				right = anyColumn
			} else {
				left = anyColumn
			}
		}
		x, a := g.operand(kind, true, depth, left, args)
		g.operatorText(text)
		y, a := g.operand(kind, false, depth, right, a)
		args = a
		operands = []stmt.Expression{x, y}
	}
	return stmt.Expression{Kind: kind, Operands: operands, Span: stmt.Span{Start: start, End: g.b.Len()}}, args
}

// operatorText of a binary operator, with gaps around it; a gap after a - is never empty, so that a
// negative literal after it does not make a comment.
func (g *Generator) operatorText(text string) {
	g.gap(false)
	g.punct(text)
	g.gap(text == "-")
}

// operand of a binary operator of the kind parent, on its left if left, after the arguments args: one
// time in three an operator, if depth allows one, in parentheses where SQLite would otherwise group it
// with another operator and one time in four elsewhere; and otherwise a column, a parameter, or a
// literal, as columns allows. A literal on the right of a division is never zero.
func (g *Generator) operand(parent stmt.ExpressionKind, left bool, depth, columns int, args []any) (stmt.Expression, []any) {
	if depth > 0 && g.r.IntN(3) == 0 {
		kind := g.operatorKind()
		needed := kind != stmt.ExprNegate && (precedence(kind) < precedence(parent) || !left && precedence(kind) == precedence(parent))
		paren := needed || g.r.IntN(4) == 0
		if paren {
			g.openParen()
			g.gap(false)
		}
		var e stmt.Expression
		e, args = g.operator(kind, depth-1, columns, args)
		if paren {
			g.gap(false)
			g.punct(")")
		}
		return e, args
	}
	switch {
	case columns == withColumn || columns == anyColumn && g.r.IntN(3) == 0:
		return g.column(g.pick(SelectColumn)), args
	case g.Parameters > 0 && g.params.IntN(g.Parameters) == 0:
		return g.parameter(args)
	}
	start := g.b.Len()
	var v stmt.Value = stmt.Integer(Integer(g.r))
	if g.r.IntN(3) == 0 {
		v = stmt.Real(Real(g.r))
	}
	g.open = false
	text, value := Literal(g.r, v)
	if parent == stmt.ExprDivide && !left && (value == stmt.Integer(0) || value == stmt.Real(0)) {
		text, value = Literal(g.r, stmt.Integer(1+g.r.IntN(9)))
	}
	g.b.WriteString(text)
	return stmt.Expression{Kind: stmt.ExprValue, Value: value, Span: stmt.Span{Start: start, End: g.b.Len()}}, args
}

// givenTerm of arithmetic or of text that a hook gives, the value of an assignment or of a comparison,
// after the arguments args, as [Generator.given] writes it, one time in six in parentheses if it is an
// operator but a call. A column alone is the term of the column, and a value alone a plain value, as
// [Generator.term] writes it, since neither is an expression of the statement pass. It gives the term
// and the arguments with those of its parameters.
func (g *Generator) givenTerm(e stmt.Expression, args []any) (stmt.Term, []any) {
	start := g.b.Len()
	switch e.Kind {
	case stmt.ExprValue:
		return g.term(e.Value, args)
	case stmt.ExprColumn:
		x := g.givenColumn(e.Column.Raw)
		return stmt.Term{Expression: x, Span: x.Span}, args
	}
	// The pass refuses parentheses around a call, which hold nothing of their own.
	paren := e.Kind != stmt.ExprCall && g.r.IntN(6) == 0
	if paren {
		g.openParen()
		g.gap(false)
	}
	x, args := g.given(e, 0, false, args)
	if paren {
		g.gap(false)
		g.punct(")")
	}
	return stmt.Term{Expression: x, Span: stmt.Span{Start: start, End: g.b.Len()}}, args
}

// The places of an operand of text, for [Generator.givenText]: the root of an expression, an operand
// of ||, and the argument of lower or of hex.
const (
	placeRoot = iota
	placeConcat
	placeLower
	placeHex
)

// givenText writes the expression of text e that a hook gives, a concatenation, a call of lower or
// hex, a column, or a value, at the place, after the arguments args: a column by the raw text of its
// name; a value as [Generator.textValue] writes it; a concatenation with its right operand in
// parentheses where it is a concatenation too, since || groups to the left, and one time in four
// its left one; and a call by its name in any case of its letters, one time in eight in double
// quotes, with or without a gap before its parenthesis, and never with parentheses around its
// argument or around the call, which the pass refuses. Arithmetic inside text, which the pass
// refuses, and a parameter, whose argument it does not know, panic. It gives the typed form of what it
// writes and the arguments with those of its parameters.
func (g *Generator) givenText(e stmt.Expression, place int, args []any) (stmt.Expression, []any) {
	start := g.b.Len()
	switch e.Kind {
	case stmt.ExprColumn:
		return g.givenColumn(e.Column.Raw), args
	case stmt.ExprValue:
		return g.textValue(e.Value, place, args)
	case stmt.ExprConcat:
		x, args := g.textOperand(e.Operands[0], false, args)
		g.gap(false)
		g.punct("||")
		g.gap(false)
		y, args := g.textOperand(e.Operands[1], true, args)
		return stmt.Expression{Kind: stmt.ExprConcat, Operands: []stmt.Expression{x, y}, Span: stmt.Span{Start: start, End: g.b.Len()}}, args
	case stmt.ExprCall:
		name := e.Function.String()
		switch g.r.IntN(4) {
		case 0:
			name = strings.ToUpper(name)
		case 1:
			name = randomCase(g.r, name)
		}
		if g.r.IntN(8) == 0 {
			name = Quote(name)
		}
		g.open = false
		g.b.WriteString(name)
		g.gap(false)
		g.openParen()
		g.gap(false)
		argument := placeLower
		if e.Function == stmt.FuncHex {
			argument = placeHex
		}
		x, args := g.givenText(e.Operands[0], argument, args)
		g.gap(false)
		g.punct(")")
		return stmt.Expression{Kind: stmt.ExprCall, Function: e.Function, Operands: []stmt.Expression{x}, Span: stmt.Span{Start: start, End: g.b.Len()}}, args
	}
	panic(fmt.Sprintf("stmttest: an expression of the kind %v in text that a hook gives", e.Kind))
}

// textOperand e of a concatenation, on its right if right, as [Generator.givenText] writes it.
func (g *Generator) textOperand(e stmt.Expression, right bool, args []any) (stmt.Expression, []any) {
	if e.Kind != stmt.ExprConcat || !right && g.r.IntN(4) != 0 {
		return g.givenText(e, placeConcat, args)
	}
	g.openParen()
	g.gap(false)
	x, args := g.givenText(e, placeConcat, args)
	g.gap(false)
	g.punct(")")
	return x, args
}

// textValue v of an expression of text at the place, as [Generator.givenText] writes it: a literal,
// or one time in [Generator.Parameters] a ?, and always a ? where the pass would refuse the literal or
// it is long: NULL, a number, a blob but as the argument of hex, and a text of more than 40 bytes,
// which keeps the text of the statement shorter than the length limit.
func (g *Generator) textValue(v stmt.Value, place int, args []any) (stmt.Expression, []any) {
	at := g.b.Len()
	g.open = false
	param := false
	switch v := v.(type) {
	case stmt.Null, stmt.Integer, stmt.Real:
		param = true
	case stmt.Blob:
		param = place != placeHex || len(v) > 20
	case stmt.Text:
		param = len(v) > 40
	}
	if param || g.Parameters > 0 && g.params.IntN(g.Parameters) == 0 {
		g.punct("?")
		return stmt.Expression{Kind: stmt.ExprParameter, Parameter: len(args) + 1, Span: stmt.Span{Start: at, End: at + 1}}, append(args, Argument(v))
	}
	text, value := Literal(g.r, v)
	g.b.WriteString(text)
	return stmt.Expression{Kind: stmt.ExprValue, Value: value, Span: stmt.Span{Start: at, End: g.b.Len()}}, args
}

// given writes the expression e that a hook gives, an operand of an operator of the kind parent, on
// its left if left, or 0 at the top, after the arguments args: its columns by the raw text of their
// names, and its values, of [stmt.ExprValue], as literals, or one time in [Generator.Parameters] as a
// ?, and always as a ? where a literal would be refused or read otherwise: NULL, text, and a blob,
// which the pass refuses in arithmetic as literals and s4ql refuses as arguments by their types; a
// value after a negation, which would be a negative literal; and a zero or a subnormal on the right
// of a division, which the pass refuses or SQLite reads as zero. An operator is in parentheses where
// SQLite would otherwise group it with another, and one time in four elsewhere, but at the top. A
// negation of a negation, which the pass refuses as a second sign, and a parameter, whose argument
// it does not know, panic. Text at the top is written as [Generator.givenText] writes it, and text
// under arithmetic panics. It gives the typed form of what it writes and the arguments with those of
// its parameters.
func (g *Generator) given(e stmt.Expression, parent stmt.ExpressionKind, left bool, args []any) (stmt.Expression, []any) {
	start := g.b.Len()
	switch e.Kind {
	case stmt.ExprColumn:
		return g.givenColumn(e.Column.Raw), args
	case stmt.ExprValue:
		return g.givenValue(e.Value, parent, left, args)
	case stmt.ExprConcat, stmt.ExprCall:
		if parent != 0 {
			panic("stmttest: text in arithmetic, which the statement pass refuses")
		}
		return g.givenText(e, placeRoot, args)
	case stmt.ExprNegate:
		g.open = false
		g.punct("-")
		g.gap(false)
		x := e.Operands[0]
		var y stmt.Expression
		switch x.Kind {
		case stmt.ExprNegate:
			panic("stmttest: a negation of a negation, which the statement pass refuses as a second sign")
		case stmt.ExprColumn, stmt.ExprValue:
			y, args = g.given(x, stmt.ExprNegate, false, args)
		default:
			g.openParen()
			g.gap(false)
			y, args = g.given(x, 0, false, args)
			g.gap(false)
			g.punct(")")
		}
		return stmt.Expression{Kind: stmt.ExprNegate, Operands: []stmt.Expression{y}, Span: stmt.Span{Start: start, End: g.b.Len()}}, args
	case stmt.ExprAdd, stmt.ExprSubtract, stmt.ExprMultiply, stmt.ExprDivide:
		x, args := g.givenOperand(e.Kind, true, e.Operands[0], args)
		g.operatorText(e.Kind.String())
		y, args := g.givenOperand(e.Kind, false, e.Operands[1], args)
		return stmt.Expression{Kind: e.Kind, Operands: []stmt.Expression{x, y}, Span: stmt.Span{Start: start, End: g.b.Len()}}, args
	}
	panic(fmt.Sprintf("stmttest: an expression of the kind %v that a hook gives", e.Kind))
}

// givenOperand e of a binary operator of the kind parent, on its left if left, as [Generator.given]
// writes it.
func (g *Generator) givenOperand(parent stmt.ExpressionKind, left bool, e stmt.Expression, args []any) (stmt.Expression, []any) {
	if len(e.Operands) == 0 {
		return g.given(e, parent, left, args)
	}
	needed := e.Kind != stmt.ExprNegate && (precedence(e.Kind) < precedence(parent) || !left && precedence(e.Kind) == precedence(parent))
	paren := needed || g.r.IntN(4) == 0
	if paren {
		g.openParen()
		g.gap(false)
	}
	x, args := g.given(e, 0, false, args)
	if paren {
		g.gap(false)
		g.punct(")")
	}
	return x, args
}

// givenValue v of an expression, an operand of an operator of the kind parent, on its left if left, as
// [Generator.given] writes it.
func (g *Generator) givenValue(v stmt.Value, parent stmt.ExpressionKind, left bool, args []any) (stmt.Expression, []any) {
	at := g.b.Len()
	g.open = false
	param := parent == stmt.ExprNegate
	switch v := v.(type) {
	case stmt.Null, stmt.Text, stmt.Blob:
		param = true
	case stmt.Integer:
		param = param || parent == stmt.ExprDivide && !left && v == 0
	case stmt.Real:
		param = param || parent == stmt.ExprDivide && !left && math.Abs(float64(v)) < 0x1p-1022
	}
	if param || g.Parameters > 0 && g.params.IntN(g.Parameters) == 0 {
		g.punct("?")
		return stmt.Expression{Kind: stmt.ExprParameter, Parameter: len(args) + 1, Span: stmt.Span{Start: at, End: at + 1}}, append(args, Argument(v))
	}
	text, value := Literal(g.r, v)
	g.b.WriteString(text)
	return stmt.Expression{Kind: stmt.ExprValue, Value: value, Span: stmt.Span{Start: at, End: g.b.Len()}}, args
}

// givenColumn of an expression, of the raw name, in double quotes where it is a keyword that starts a
// select after an opening parenthesis, which names the same column.
func (g *Generator) givenColumn(raw string) stmt.Expression {
	if g.open && raw[0] != '"' && selectStart[strings.ToLower(raw)] {
		raw = Quote(raw)
	}
	g.open = false
	n := g.write(raw)
	return stmt.Expression{Kind: stmt.ExprColumn, Column: n, Span: stmt.Span{Start: n.Offset, End: g.b.Len()}}
}

// column of an expression, of the raw name, which is drawn again from the pool of [SelectColumn]
// while it is a keyword that starts a select after an opening parenthesis.
func (g *Generator) column(raw string) stmt.Expression {
	for g.open && selectStart[strings.ToLower(raw)] {
		raw = g.pick(SelectColumn)
	}
	g.open = false
	n := g.write(raw)
	return stmt.Expression{Kind: stmt.ExprColumn, Column: n, Span: stmt.Span{Start: n.Offset, End: g.b.Len()}}
}

// selectStart are the keywords that SQLite reads as the start of a subquery after an opening
// parenthesis, in lower case.
var selectStart = map[string]bool{"select": true, "with": true, "values": true}

// openParen writes an opening parenthesis of an expression, after which the next token must not be a
// keyword of [selectStart].
func (g *Generator) openParen() {
	g.punct("(")
	g.open = true
}

// parameter of an expression, a ?, after the arguments args, with an integer at random as its
// argument, which it adds to them.
func (g *Generator) parameter(args []any) (stmt.Expression, []any) {
	g.open = false
	at := g.b.Len()
	g.punct("?")
	return stmt.Expression{Kind: stmt.ExprParameter, Parameter: len(args) + 1, Span: stmt.Span{Start: at, End: at + 1}}, append(args, Argument(stmt.Integer(Integer(g.r))))
}
