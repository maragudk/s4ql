package parser

import (
	"fmt"
	"strings"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/token"
)

// Precedence of the operators, as the %left, %right, and %nonassoc lines of parse.y give it, from
// the lowest. Lemon resolves a conflict between shifting an operator and reducing a rule by these:
// the precedence of a rule is that of its first terminal with a precedence, unless the rule names one
// in brackets.
const (
	precOr      = 1 // %left OR.
	precAnd     = 2 // %left AND.
	precNot     = 3 // %right NOT.
	precEq      = 4 // %left IS MATCH LIKE_KW BETWEEN IN ISNULL NOTNULL NE EQ.
	precCompare = 5 // %left GT LE LT GE.
	// 6 is %right ESCAPE, which like reads itself after its right operand.
	precBit     = 7  // %left BITAND BITOR LSHIFT RSHIFT.
	precAdd     = 8  // %left PLUS MINUS.
	precMul     = 9  // %left STAR SLASH REM.
	precConcat  = 10 // %left CONCAT PTR.
	precCollate = 11 // %left COLLATE.
	precBitNot  = 12 // %right BITNOT. Also the precedence of unary + and -.
)

// infixPrec gives the precedence of the token kind k after an expression, where it starts an
// infix or postfix operator, or 0 if it starts none.
func infixPrec(k token.Kind) int {
	switch k {
	case token.Or:
		return precOr
	case token.And:
		return precAnd
	case token.Not:
		return precNot
	case token.Is, token.Match, token.LikeKw, token.Between, token.In, token.IsNull, token.NotNull,
		token.NE, token.EQ:
		return precEq
	case token.GT, token.LE, token.LT, token.GE:
		return precCompare
	case token.BitAnd, token.BitOr, token.LShift, token.RShift:
		return precBit
	case token.Plus, token.Minus:
		return precAdd
	case token.Star, token.Slash, token.Rem:
		return precMul
	case token.Concat, token.Ptr:
		return precConcat
	case token.Collate:
		return precCollate
	}
	return 0
}

// expr reads an expression.
func (p *parser) expr() ast.Expr {
	return p.exprPrec(0, false)
}

// exprPrec reads an expression as the operand of a rule of precedence min, or of no rule for 0.
//
// After an operand, Lemon either shifts the operator that follows, which continues the operand,
// or reduces the rule. It shifts if the operator's precedence is higher than the rule's, or equal
// and the operator is right-associative (only NOT is, among the tokens that start an operator after
// an expression). That is the condition of this loop.
//
// With stopAtAnd, the loop ends at an AND that follows the operand at its top level: that AND is
// the one of BETWEEN, as the state after `expr between_op expr` shifts it for the rule
// `expr ::= expr between_op expr AND expr`, while every other operator there continues the operand.
//
// parse.y: the rules for expr with an operator.
func (p *parser) exprPrec(min int, stopAtAnd bool) ast.Expr {
	p.enter()
	defer p.leave()
	x := p.unary()
	for {
		k := p.cur().kind
		prec := infixPrec(k)
		// A token that starts no operator has the precedence 0, which is never above min.
		if prec < min || (prec == min && k != token.Not) {
			return x
		}
		if stopAtAnd && k == token.And {
			return x
		}
		x = p.infix(x, prec)
	}
}

// infix reads the operator at the current token, of precedence prec, with x as its left operand.
func (p *parser) infix(x ast.Expr, prec int) ast.Expr {
	start := x.Bounds().Start
	switch p.cur().kind {
	case token.Collate:
		// parse.y: expr ::= expr COLLATE ids.
		c := &ast.Collate{X: x, Collate: p.advance()}
		c.Name = p.name(classIds)
		return finish(p, c, start)
	case token.IsNull, token.NotNull:
		// parse.y: expr ::= expr ISNULL|NOTNULL.
		return finish(p, &ast.NullTest{X: x, Op: p.advance()}, start)
	case token.Not:
		not := p.advance()
		switch p.cur().kind {
		case token.Null:
			// parse.y: expr ::= expr NOT NULL.
			return finish(p, &ast.NullTest{X: x, Op: not, Null: p.advance()}, start)
		case token.LikeKw, token.Match:
			return p.like(x, not)
		case token.Between:
			return p.between(x, not)
		case token.In:
			return p.in(x, not)
		}
		p.fail()
	case token.LikeKw, token.Match:
		return p.like(x, ast.Token{})
	case token.Between:
		return p.between(x, ast.Token{})
	case token.In:
		return p.in(x, ast.Token{})
	case token.Is:
		// parse.y: expr ::= expr IS expr. expr ::= expr IS NOT expr. expr ::= expr IS NOT DISTINCT
		// FROM expr. expr ::= expr IS DISTINCT FROM expr.
		is := &ast.Is{X: x, Is: p.advance()}
		is.Not = p.accept(token.Not)
		if p.is(token.Distinct) {
			is.Distinct = p.advance()
			is.From = p.expect(token.From)
		}
		is.Y = p.exprPrec(precEq, false)
		return finish(p, is, start)
	}
	// A binary operator: parse.y: expr ::= expr AND expr. expr ::= expr OR expr. expr ::= expr
	// LT|GT|GE|LE expr. expr ::= expr EQ|NE expr. expr ::= expr BITAND|BITOR|LSHIFT|RSHIFT expr.
	// expr ::= expr PLUS|MINUS expr. expr ::= expr STAR|SLASH|REM expr. expr ::= expr CONCAT expr.
	// expr ::= expr PTR expr.
	b := &ast.Binary{X: x, Op: p.advance()}
	b.Y = p.exprPrec(prec, false)
	return finish(p, b, start)
}

// like reads LIKE, GLOB, REGEXP, or MATCH, after x and an optional NOT. The right operand and the
// operand of ESCAPE are read with the precedence of the rule, LIKE_KW; ESCAPE has a higher
// precedence, so it is shifted after the right operand.
//
// parse.y: likeop ::= LIKE_KW|MATCH. likeop ::= NOT LIKE_KW|MATCH. expr ::= expr likeop expr.
// [LIKE_KW] expr ::= expr likeop expr ESCAPE expr. [LIKE_KW]
func (p *parser) like(x ast.Expr, not ast.Token) ast.Expr {
	start := x.Bounds().Start
	l := &ast.Like{X: x, Not: not, Op: p.advance()}
	l.Y = p.exprPrec(precEq, false)
	if p.is(token.Escape) {
		l.Escape = p.advance()
		l.EscapeX = p.exprPrec(precEq, false)
	}
	return finish(p, l, start)
}

// between reads BETWEEN after x and an optional NOT.
//
// The low operand continues over every operator but an AND at its top level, which is BETWEEN's
// own. The high operand is read with the precedence of the rule: after it, the state holds both
// `expr ::= expr AND expr` and the BETWEEN rule complete when an AND inside the low operand was
// taken for BETWEEN's, and Lemon reduces the rule of the higher precedence, BETWEEN.
//
// parse.y: between_op ::= BETWEEN. between_op ::= NOT BETWEEN. expr ::= expr between_op expr AND
// expr. [BETWEEN]
func (p *parser) between(x ast.Expr, not ast.Token) ast.Expr {
	start := x.Bounds().Start
	b := &ast.Between{X: x, Not: not, Between: p.advance()}
	b.Low = p.exprPrec(0, true)
	b.And = p.expect(token.And)
	b.High = p.exprPrec(precEq, false)
	return finish(p, b, start)
}

// in reads IN after x and an optional NOT: a list or a SELECT in parentheses, or a table or a
// table-valued function.
//
// parse.y: in_op ::= IN. in_op ::= NOT IN. expr ::= expr in_op LP exprlist RP. expr ::= expr in_op
// LP select RP. expr ::= expr in_op nm dbnm paren_exprlist. paren_exprlist ::= . paren_exprlist ::=
// LP exprlist RP.
func (p *parser) in(x ast.Expr, not ast.Token) ast.Expr {
	start := x.Bounds().Start
	in := p.advance()
	if p.is(token.LP) {
		lp := p.advance()
		if p.startsSelect() {
			s := &ast.InSelect{X: x, Not: not, In: in, LParen: lp, Select: p.selectStmt(nil)}
			s.RParen = p.expect(token.RP)
			return finish(p, s, start)
		}
		l := &ast.InList{X: x, Not: not, In: in, LParen: lp}
		if !p.is(token.RP) {
			l.List, l.Commas = p.exprList()
		}
		l.RParen = p.expect(token.RP)
		return finish(p, l, start)
	}
	t := &ast.InTable{X: x, Not: not, In: in, Name: p.qualifiedName()}
	if p.is(token.LP) {
		t.LParen = p.advance()
		if !p.is(token.RP) {
			t.Args, t.Commas = p.exprList()
		}
		t.RParen = p.expect(token.RP)
	}
	return finish(p, t, start)
}

// startsSelect reports whether the current token starts a select: SELECT, VALUES, or WITH. It is
// used after a "(", where Lemon shifts WITH as the keyword.
func (p *parser) startsSelect() bool {
	k := p.cur().kind
	return k == token.Select || k == token.Values || k == token.With
}

// exprList reads one or more expressions separated by commas.
//
// parse.y: nexprlist ::= nexprlist COMMA expr. nexprlist ::= expr.
func (p *parser) exprList() ([]ast.Expr, []ast.Token) {
	exprs := []ast.Expr{p.expr()}
	var commas []ast.Token
	for p.is(token.Comma) {
		commas = append(commas, p.advance())
		exprs = append(exprs, p.expr())
	}
	return exprs, commas
}

// unary reads a prefix operator and its operand, or a primary expression.
//
// parse.y: expr ::= NOT expr. expr ::= BITNOT expr. expr ::= PLUS|MINUS expr. [BITNOT]
func (p *parser) unary() ast.Expr {
	start := p.mark()
	switch p.cur().kind {
	case token.Not:
		u := &ast.Unary{Op: p.advance()}
		u.X = p.exprPrec(precNot, false)
		return finish(p, u, start)
	case token.BitNot, token.Plus, token.Minus:
		u := &ast.Unary{Op: p.advance()}
		u.X = p.exprPrec(precBitNot, false)
		return finish(p, u, start)
	}
	return p.primary()
}

// primary reads an expression that is not an operator applied to others.
//
// parse.y: expr ::= term. expr ::= LP expr RP. expr ::= idj. expr ::= nm DOT nm. expr ::= nm DOT
// nm DOT nm. term ::= NULL|FLOAT|BLOB. term ::= STRING. term ::= INTEGER. term ::= QNUMBER.
// term ::= CTIME_KW. expr ::= VARIABLE. expr ::= CAST LP expr AS typetoken RP. expr ::= LP nexprlist
// COMMA expr RP. expr ::= LP select RP. expr ::= EXISTS LP select RP. expr ::= CASE case_operand
// case_exprlist case_else END. expr ::= RAISE LP IGNORE RP. expr ::= RAISE LP raisetype COMMA expr RP.
// and the function calls.
func (p *parser) primary() ast.Expr {
	start := p.mark()
	t := p.cur()
	switch t.kind {
	case token.LP:
		return p.paren()
	case token.Null, token.Float, token.Blob, token.Integer, token.CTimeKw:
		return finish(p, &ast.Literal{Value: p.advance()}, start)
	case token.QNumber:
		p.checkDigitSeparators()
		return finish(p, &ast.Literal{Value: p.advance()}, start)
	case token.String:
		if p.peek(1).kind != token.Dot {
			return finish(p, &ast.Literal{Value: p.advance()}, start)
		}
		return p.columnRef()
	case token.Variable:
		p.checkVariable()
		return finish(p, &ast.Variable{Name: p.advance()}, start)
	case token.Cast:
		return p.cast()
	case token.Exists:
		e := &ast.Exists{Exists: p.advance(), LParen: p.expect(token.LP)}
		e.Select = p.selectStmt(nil)
		e.RParen = p.expect(token.RP)
		return finish(p, e, start)
	case token.Case:
		return p.caseExpr()
	case token.Raise:
		return p.raise()
	}
	if isName(t, classIdj) {
		switch p.peek(1).kind {
		case token.LP:
			return p.funcCall()
		case token.Dot:
			return p.columnRef()
		}
		return finish(p, &ast.ColumnRef{Column: p.advance()}, start)
	}
	p.fail()
	return nil
}

// paren reads an expression that starts with "(": a SELECT, an expression, or a row value.
//
// parse.y: expr ::= LP expr RP. expr ::= LP nexprlist COMMA expr RP. expr ::= LP select RP.
func (p *parser) paren() ast.Expr {
	start := p.mark()
	lp := p.advance()
	if p.startsSelect() {
		s := &ast.Subquery{LParen: lp, Select: p.selectStmt(nil)}
		s.RParen = p.expect(token.RP)
		return finish(p, s, start)
	}
	x := p.expr()
	if !p.is(token.Comma) {
		return finish(p, &ast.Paren{LParen: lp, X: x, RParen: p.expect(token.RP)}, start)
	}
	r := &ast.Row{LParen: lp, Exprs: []ast.Expr{x}}
	for p.is(token.Comma) {
		r.Commas = append(r.Commas, p.advance())
		r.Exprs = append(r.Exprs, p.expr())
	}
	r.RParen = p.expect(token.RP)
	return finish(p, r, start)
}

// columnRef reads a qualified name: nm DOT nm, or nm DOT nm DOT nm.
//
// parse.y: expr ::= nm DOT nm. expr ::= nm DOT nm DOT nm.
func (p *parser) columnRef() ast.Expr {
	start := p.mark()
	first := p.nm()
	dot := p.expect(token.Dot)
	second := p.nm()
	if !p.is(token.Dot) {
		return finish(p, &ast.ColumnRef{Table: first, Dot2: dot, Column: second}, start)
	}
	dot2 := p.advance()
	return finish(p, &ast.ColumnRef{Schema: first, Dot1: dot, Table: second, Dot2: dot2, Column: p.nm()}, start)
}

// cast reads CAST.
//
// parse.y: expr ::= CAST LP expr AS typetoken RP.
func (p *parser) cast() ast.Expr {
	start := p.mark()
	c := &ast.Cast{Cast: p.advance(), LParen: p.expect(token.LP)}
	c.X = p.expr()
	c.As = p.expect(token.As)
	c.Type = p.typeToken()
	c.RParen = p.expect(token.RP)
	return finish(p, c, start)
}

// caseExpr reads CASE.
//
// parse.y: expr ::= CASE case_operand case_exprlist case_else END. case_exprlist ::= case_exprlist
// WHEN expr THEN expr. case_exprlist ::= WHEN expr THEN expr. case_else ::= ELSE expr.
// case_else ::= . case_operand ::= expr. case_operand ::= .
func (p *parser) caseExpr() ast.Expr {
	start := p.mark()
	c := &ast.Case{Case: p.advance()}
	if !p.is(token.When) {
		c.Operand = p.expr()
	}
	for {
		ws := p.mark()
		w := &ast.When{When: p.expect(token.When)}
		w.Condition = p.expr()
		w.Then = p.expect(token.Then)
		w.Result = p.expr()
		c.Whens = append(c.Whens, finish(p, w, ws))
		if !p.is(token.When) {
			break
		}
	}
	if p.is(token.Else) {
		c.Else = p.advance()
		c.ElseX = p.expr()
	}
	c.End = p.expect(token.End)
	return finish(p, c, start)
}

// raise reads RAISE.
//
// parse.y: expr ::= RAISE LP IGNORE RP. expr ::= RAISE LP raisetype COMMA expr RP.
// raisetype ::= ROLLBACK. raisetype ::= ABORT. raisetype ::= FAIL.
func (p *parser) raise() ast.Expr {
	start := p.mark()
	r := &ast.Raise{Raise: p.advance(), LParen: p.expect(token.LP)}
	switch p.cur().kind {
	case token.Ignore:
		r.Action = p.advance()
	case token.Rollback, token.Abort, token.Fail:
		r.Action = p.advance()
		r.Comma = p.expect(token.Comma)
		r.Message = p.expr()
	default:
		p.fail()
	}
	r.RParen = p.expect(token.RP)
	return finish(p, r, start)
}

// funcCall reads a call of a function by name.
//
// parse.y: expr ::= idj LP distinct exprlist RP. expr ::= idj LP distinct exprlist ORDER BY
// sortlist RP. expr ::= idj LP STAR RP. and each with filter_over. distinct ::= DISTINCT.
// distinct ::= ALL. distinct ::= . exprlist ::= nexprlist. exprlist ::= .
func (p *parser) funcCall() ast.Expr {
	start := p.mark()
	f := &ast.FuncCall{Name: p.advance(), LParen: p.expect(token.LP)}
	if p.is(token.Star) {
		f.Star = p.advance()
	} else {
		if p.is(token.Distinct) || p.is(token.All) {
			f.Distinct = p.advance()
		}
		if !p.is(token.RP) && !p.is(token.Order) {
			f.Args, f.Commas = p.exprList()
		}
		if p.is(token.Order) {
			f.OrderBy = p.orderBy()
		}
	}
	f.RParen = p.expect(token.RP)
	p.filterOver(&f.Filter, &f.Over)
	return finish(p, f, start)
}

// filterOver reads an optional FILTER clause and an optional OVER clause after a function call.
//
// parse.y: filter_over ::= filter_clause over_clause. filter_over ::= over_clause.
// filter_over ::= filter_clause. filter_clause ::= FILTER LP WHERE expr RP. over_clause ::= OVER LP
// window RP. over_clause ::= OVER nm.
func (p *parser) filterOver(filter **ast.Filter, over **ast.Over) {
	if p.is(token.Filter) {
		start := p.mark()
		f := &ast.Filter{Filter: p.advance(), LParen: p.expect(token.LP), Where: p.expect(token.Where)}
		f.X = p.expr()
		f.RParen = p.expect(token.RP)
		*filter = finish(p, f, start)
	}
	if p.is(token.Over) {
		start := p.mark()
		o := &ast.Over{Over: p.advance()}
		if p.is(token.LP) {
			o.LParen = p.advance()
			o.Window = p.window()
			o.RParen = p.expect(token.RP)
		} else {
			o.Name = p.nm()
		}
		*over = finish(p, o, start)
	}
}

// checkVariable records the error of a variable #N at the current token, which the action of
// `expr ::= VARIABLE` raises outside SQLite's nested parses when the next token arrives.
//
// parse.y: expr ::= VARIABLE.
func (p *parser) checkVariable() {
	t := p.cur()
	text := p.src[t.off : t.off+t.n]
	if len(text) >= 2 && text[0] == '#' && '0' <= text[1] && text[1] <= '9' {
		p.setPending(&pendingError{message: fmt.Sprintf(`near "%s": syntax error`, text), offset: t.off})
	}
}

// checkDigitSeparators of the number at the current token, as the action of `term ::= QNUMBER`
// does when the next token arrives: sqlite3DequoteNumber in util.c, which removes the separators
// from the token's text in place and reports each one that is not between two digits with the text
// as it is at that moment. The last report wins.
//
// parse.y: term ::= QNUMBER.
func (p *parser) checkDigitSeparators() {
	t := p.cur()
	if msg, bad := digitSeparatorError(p.src[t.off : t.off+t.n]); bad {
		p.setPending(&pendingError{message: msg, offset: -1})
	}
}

// digitSeparatorError of the number z, as sqlite3DequoteNumber reports it.
func digitSeparatorError(z string) (string, bool) {
	buf := append([]byte(z), 0)
	hex := strings.HasPrefix(z, "0x") || strings.HasPrefix(z, "0X")
	digit := func(c byte) bool {
		if hex {
			return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
		}
		return '0' <= c && c <= '9'
	}
	var msg string
	bad := false
	out := 0
	// A number starts with a digit or a ".", so a separator is never first and has a byte before it.
	for in := 0; ; in++ {
		c := buf[in]
		if c != '_' {
			buf[out] = c
			out++
		} else if !digit(buf[in-1]) || !digit(buf[in+1]) {
			msg = fmt.Sprintf(`unrecognized token: "%s"`, buf[:len(z)])
			bad = true
		}
		if c == 0 {
			break
		}
	}
	return msg, bad
}

// setPending records a pending error, which SQLite raises when the token after the current one
// arrives. Only the first counts: the parse loop stops after the token on which it was raised.
func (p *parser) setPending(e *pendingError) {
	if p.pending == nil {
		e.trigger = p.i + 1
		p.pending = e
	}
}
