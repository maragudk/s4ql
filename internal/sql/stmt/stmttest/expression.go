package stmttest

import (
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
