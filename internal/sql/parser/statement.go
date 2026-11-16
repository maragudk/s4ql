package parser

import (
	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/token"
)

// statement reads the first statement, after any empty statements, and the semicolon that ends it.
// It returns nil if the text has no statement.
//
// parse.y: input ::= cmdlist. cmdlist ::= cmdlist ecmd. cmdlist ::= ecmd. ecmd ::= SEMI.
// ecmd ::= cmdx SEMI. ecmd ::= explain cmdx SEMI. explain ::= EXPLAIN. explain ::= EXPLAIN QUERY
// PLAN. cmdx ::= cmd.
func (p *parser) statement() *ast.Statement {
	for p.is(token.Semi) && !p.cur().synthetic() {
		p.advance()
	}
	if p.cur().atEnd() {
		return nil
	}
	start := p.mark()
	s := &ast.Statement{}
	if p.is(token.Explain) {
		s.Explain = p.advance()
		if p.is(token.Query) {
			s.Query = p.advance()
			s.Plan = p.expect(token.Plan)
		}
	}
	s.Cmd = p.cmd()
	if !p.is(token.Semi) {
		p.fail()
	}
	if !p.cur().synthetic() {
		s.Semi = p.advance()
	} else {
		// The empty semicolon at the end is not a token of the text; SQLite's parser shifts it, which
		// ends the statement.
		p.i++
	}
	return finish(p, s, start)
}

// cmd reads a command.
//
// parse.y: the rules for cmd.
func (p *parser) cmd() ast.Cmd {
	switch p.cur().kind {
	case token.Begin:
		return p.begin()
	case token.Commit, token.End:
		return p.commit()
	case token.Rollback:
		return p.rollback()
	case token.Savepoint:
		start := p.mark()
		return finish(p, &ast.Savepoint{Savepoint: p.advance(), Name: p.nm()}, start)
	case token.Release:
		return p.release()
	case token.Create:
		return p.create()
	case token.Drop:
		return p.drop()
	case token.Select, token.Values:
		start := p.mark()
		return finish(p, &ast.SelectCmd{Select: p.selectStmt(nil)}, start)
	case token.With:
		return p.with()
	case token.Delete:
		return p.delete(p.mark(), nil, false)
	case token.Update:
		return p.update(p.mark(), nil, false)
	case token.Insert, token.Replace:
		return p.insert(p.mark(), nil, false)
	case token.Vacuum:
		return p.vacuum()
	case token.Pragma:
		return p.pragma()
	case token.Attach:
		return p.attach()
	case token.Detach:
		return p.detach()
	case token.Reindex:
		start := p.mark()
		r := &ast.Reindex{Reindex: p.advance()}
		if isName(p.cur(), classNm) {
			r.Name = p.qualifiedName()
		}
		return finish(p, r, start)
	case token.Analyze:
		start := p.mark()
		a := &ast.Analyze{Analyze: p.advance()}
		if isName(p.cur(), classNm) {
			a.Name = p.qualifiedName()
		}
		return finish(p, a, start)
	case token.Alter:
		return p.alterTable()
	}
	p.fail()
	return nil
}

// with reads a command that starts with a WITH clause: a SELECT, or a DELETE, UPDATE, INSERT, or
// REPLACE.
//
// parse.y: select ::= WITH wqlist selectnowith. select ::= WITH RECURSIVE wqlist selectnowith.
// cmd ::= with DELETE ... cmd ::= with UPDATE ... cmd ::= with insert_cmd ... with ::= WITH wqlist.
// with ::= WITH RECURSIVE wqlist.
func (p *parser) with() ast.Cmd {
	start := p.mark()
	w := p.withClause()
	switch p.cur().kind {
	case token.Select, token.Values:
		return finish(p, &ast.SelectCmd{Select: p.selectStmt(w)}, start)
	case token.Delete:
		return p.delete(start, w, false)
	case token.Update:
		return p.update(start, w, false)
	case token.Insert, token.Replace:
		return p.insert(start, w, false)
	}
	p.fail()
	return nil
}

// begin reads BEGIN.
//
// parse.y: cmd ::= BEGIN transtype trans_opt. transtype ::= . transtype ::= DEFERRED.
// transtype ::= IMMEDIATE. transtype ::= EXCLUSIVE.
func (p *parser) begin() *ast.Begin {
	start := p.mark()
	b := &ast.Begin{Begin: p.advance()}
	if p.is(token.Deferred) || p.is(token.Immediate) || p.is(token.Exclusive) {
		b.Mode = p.advance()
	}
	b.Transaction, b.Name = p.transOpt()
	return finish(p, b, start)
}

// transOpt reads an optional TRANSACTION with an optional name.
//
// parse.y: trans_opt ::= . trans_opt ::= TRANSACTION. trans_opt ::= TRANSACTION nm.
func (p *parser) transOpt() (ast.Token, ast.Token) {
	if !p.is(token.Transaction) {
		return ast.Token{}, ast.Token{}
	}
	t := p.advance()
	if isName(p.cur(), classNm) {
		return t, p.advance()
	}
	return t, ast.Token{}
}

// commit reads COMMIT or END.
//
// parse.y: cmd ::= COMMIT|END trans_opt.
func (p *parser) commit() *ast.Commit {
	start := p.mark()
	c := &ast.Commit{Commit: p.advance()}
	c.Transaction, c.Name = p.transOpt()
	return finish(p, c, start)
}

// rollback reads ROLLBACK.
//
// parse.y: cmd ::= ROLLBACK trans_opt. cmd ::= ROLLBACK trans_opt TO savepoint_opt nm.
// savepoint_opt ::= SAVEPOINT. savepoint_opt ::= .
func (p *parser) rollback() *ast.Rollback {
	start := p.mark()
	r := &ast.Rollback{Rollback: p.advance()}
	r.Transaction, r.TransactionName = p.transOpt()
	if p.is(token.To) {
		r.To = p.advance()
		r.Savepoint = p.accept(token.Savepoint)
		r.SavepointName = p.nm()
	}
	return finish(p, r, start)
}

// release reads RELEASE.
//
// parse.y: cmd ::= RELEASE savepoint_opt nm.
func (p *parser) release() *ast.Release {
	start := p.mark()
	r := &ast.Release{Release: p.advance()}
	r.Savepoint = p.accept(token.Savepoint)
	r.Name = p.nm()
	return finish(p, r, start)
}

// vacuum reads VACUUM.
//
// parse.y: cmd ::= VACUUM vinto. cmd ::= VACUUM nm vinto. vinto ::= INTO expr. vinto ::= .
func (p *parser) vacuum() *ast.Vacuum {
	start := p.mark()
	v := &ast.Vacuum{Vacuum: p.advance()}
	if isName(p.cur(), classNm) {
		v.Schema = p.advance()
	}
	if p.is(token.Into) {
		v.Into = p.advance()
		v.File = p.expr()
	}
	return finish(p, v, start)
}

// pragma reads PRAGMA.
//
// parse.y: cmd ::= PRAGMA nm dbnm. cmd ::= PRAGMA nm dbnm EQ nmnum. cmd ::= PRAGMA nm dbnm LP nmnum
// RP. cmd ::= PRAGMA nm dbnm EQ minus_num. cmd ::= PRAGMA nm dbnm LP minus_num RP.
func (p *parser) pragma() *ast.Pragma {
	start := p.mark()
	pr := &ast.Pragma{Pragma: p.advance(), Name: p.qualifiedName()}
	switch {
	case p.is(token.EQ):
		pr.Eq = p.advance()
		pr.Value = p.pragmaValue()
	case p.is(token.LP):
		pr.LParen = p.advance()
		pr.Value = p.pragmaValue()
		pr.RParen = p.expect(token.RP)
	}
	return finish(p, pr, start)
}

// pragmaValue reads the value of a PRAGMA.
//
// parse.y: nmnum ::= plus_num. nmnum ::= nm. nmnum ::= ON. nmnum ::= DELETE. nmnum ::= DEFAULT.
// plus_num ::= PLUS number. plus_num ::= number. minus_num ::= MINUS number.
// %token_class number INTEGER|FLOAT.
func (p *parser) pragmaValue() *ast.PragmaValue {
	start := p.mark()
	v := &ast.PragmaValue{}
	switch {
	case p.is(token.Plus) || p.is(token.Minus):
		v.Sign = p.advance()
		v.Value = p.number()
	case p.is(token.Integer) || p.is(token.Float):
		v.Value = p.advance()
	case p.is(token.On) || p.is(token.Delete) || p.is(token.Default) || isName(p.cur(), classNm):
		v.Value = p.advance()
	default:
		p.fail()
	}
	return finish(p, v, start)
}

// number reads INTEGER or FLOAT: the token class number of parse.y.
func (p *parser) number() ast.Token {
	if p.is(token.Integer) || p.is(token.Float) {
		return p.advance()
	}
	p.fail()
	return ast.Token{}
}

// attach reads ATTACH.
//
// parse.y: cmd ::= ATTACH database_kw_opt expr AS expr key_opt. key_opt ::= . key_opt ::= KEY expr.
// database_kw_opt ::= DATABASE. database_kw_opt ::= .
func (p *parser) attach() *ast.Attach {
	start := p.mark()
	a := &ast.Attach{Attach: p.advance()}
	a.Database = p.accept(token.Database)
	a.File = p.expr()
	a.As = p.expect(token.As)
	a.Name = p.expr()
	if p.is(token.Key) {
		a.Key = p.advance()
		a.KeyX = p.expr()
	}
	return finish(p, a, start)
}

// detach reads DETACH.
//
// parse.y: cmd ::= DETACH database_kw_opt expr.
func (p *parser) detach() *ast.Detach {
	start := p.mark()
	d := &ast.Detach{Detach: p.advance()}
	d.Database = p.accept(token.Database)
	d.Name = p.expr()
	return finish(p, d, start)
}
