package parser

import (
	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/token"
)

// delete reads DELETE FROM, after the WITH clause w, if any, which started at start. In a trigger
// it has no WITH and no RETURNING.
//
// parse.y: cmd ::= with DELETE FROM xfullname indexed_opt where_opt_ret. trigger_cmd ::= DELETE FROM
// xfullname tridxby where_opt scanpt.
func (p *parser) delete(start int, w *ast.With, inTrigger bool) *ast.Delete {
	d := &ast.Delete{With: w, Delete: p.advance(), From: p.expect(token.From)}
	d.Table = p.targetTable()
	if p.is(token.Indexed) || p.is(token.Not) {
		d.Indexed = p.indexedBy()
	}
	d.Where, d.Returning = p.whereOptRet(inTrigger)
	return finish(p, d, start)
}

// whereOptRet reads an optional WHERE and an optional RETURNING, or only the WHERE in a trigger.
//
// parse.y: where_opt_ret ::= . where_opt_ret ::= WHERE expr. where_opt_ret ::= RETURNING selcollist.
// where_opt_ret ::= WHERE expr RETURNING selcollist.
func (p *parser) whereOptRet(inTrigger bool) (*ast.Where, *ast.Returning) {
	w := p.whereOpt()
	if !inTrigger && p.is(token.Returning) {
		return w, p.returning()
	}
	return w, nil
}

// update reads UPDATE, after the WITH clause w, if any, which started at start. In a trigger it has
// no WITH and no RETURNING.
//
// parse.y: cmd ::= with UPDATE orconf xfullname indexed_opt SET setlist from where_opt_ret.
// trigger_cmd ::= UPDATE orconf xfullname tridxby SET setlist from where_opt scanpt.
func (p *parser) update(start int, w *ast.With, inTrigger bool) *ast.Update {
	u := &ast.Update{With: w, Update: p.advance()}
	u.Or, u.Resolution = p.orConf()
	u.Table = p.targetTable()
	if p.is(token.Indexed) || p.is(token.Not) {
		u.Indexed = p.indexedBy()
	}
	u.Set = p.expect(token.Set)
	u.Assignments, u.Commas = p.setList()
	if p.is(token.From) {
		u.From = p.from()
	}
	u.Where, u.Returning = p.whereOptRet(inTrigger)
	return finish(p, u, start)
}

// orConf reads an optional OR and a conflict resolution.
//
// parse.y: orconf ::= . orconf ::= OR resolvetype.
func (p *parser) orConf() (ast.Token, ast.Token) {
	if !p.is(token.Or) {
		return ast.Token{}, ast.Token{}
	}
	or := p.advance()
	return or, p.resolveType()
}

// resolveType reads a conflict resolution.
//
// parse.y: resolvetype ::= raisetype. resolvetype ::= IGNORE. resolvetype ::= REPLACE.
// raisetype ::= ROLLBACK. raisetype ::= ABORT. raisetype ::= FAIL.
func (p *parser) resolveType() ast.Token {
	switch p.cur().kind {
	case token.Rollback, token.Abort, token.Fail, token.Ignore, token.Replace:
		return p.advance()
	}
	p.fail()
	return ast.Token{}
}

// targetTable reads the table of INSERT, UPDATE, or DELETE.
//
// parse.y: xfullname ::= nm. xfullname ::= nm DOT nm. xfullname ::= nm AS nm.
// xfullname ::= nm DOT nm AS nm.
func (p *parser) targetTable() *ast.TargetTable {
	start := p.mark()
	t := &ast.TargetTable{Name: p.qualifiedName()}
	if p.is(token.As) {
		t.As = p.advance()
		t.Alias = p.nm()
	}
	return finish(p, t, start)
}

// setList reads the assignments of SET.
//
// parse.y: setlist ::= setlist COMMA nm EQ expr. setlist ::= setlist COMMA LP idlist RP EQ expr.
// setlist ::= nm EQ expr. setlist ::= LP idlist RP EQ expr.
func (p *parser) setList() ([]*ast.Assignment, []ast.Token) {
	var as []*ast.Assignment
	var commas []ast.Token
	for {
		start := p.mark()
		a := &ast.Assignment{}
		if p.is(token.LP) {
			a.LParen = p.advance()
			a.Columns = p.idList()
			a.RParen = p.expect(token.RP)
		} else {
			a.Column = p.nm()
		}
		a.Eq = p.expect(token.EQ)
		a.X = p.expr()
		as = append(as, finish(p, a, start))
		if !p.is(token.Comma) {
			return as, commas
		}
		commas = append(commas, p.advance())
	}
}

// insert reads INSERT or REPLACE, after the WITH clause w, if any, which started at start. In a
// trigger it has no WITH and no DEFAULT VALUES.
//
// parse.y: cmd ::= with insert_cmd INTO xfullname idlist_opt select upsert. cmd ::= with insert_cmd
// INTO xfullname idlist_opt DEFAULT VALUES returning. insert_cmd ::= INSERT orconf.
// insert_cmd ::= REPLACE. idlist_opt ::= . idlist_opt ::= LP idlist RP. trigger_cmd ::= scanpt
// insert_cmd INTO xfullname idlist_opt select upsert scanpt.
func (p *parser) insert(start int, w *ast.With, inTrigger bool) *ast.Insert {
	in := &ast.Insert{With: w, Insert: p.advance()}
	if in.Insert.Kind == token.Insert {
		in.Or, in.Resolution = p.orConf()
	}
	in.Into = p.expect(token.Into)
	in.Table = p.targetTable()
	if p.is(token.LP) {
		in.LParen = p.advance()
		in.Columns = p.idList()
		in.RParen = p.expect(token.RP)
	}
	if !inTrigger && p.is(token.Default) {
		in.Default = p.advance()
		in.Values = p.expect(token.Values)
		if p.is(token.Returning) {
			in.Returning = p.returning()
		}
		return finish(p, in, start)
	}
	in.Select = p.selectStmt(nil)
	in.Upserts, in.Returning = p.upserts()
	return finish(p, in, start)
}

// upserts reads the ON CONFLICT clauses after the SELECT of an INSERT, and a RETURNING. A clause
// without a target must be the last, and only RETURNING can follow it.
//
// parse.y: upsert ::= . upsert ::= RETURNING selcollist. upsert ::= ON CONFLICT LP sortlist RP
// where_opt DO UPDATE SET setlist where_opt upsert. upsert ::= ON CONFLICT LP sortlist RP where_opt
// DO NOTHING upsert. upsert ::= ON CONFLICT DO NOTHING returning. upsert ::= ON CONFLICT DO UPDATE
// SET setlist where_opt returning. returning ::= . returning ::= RETURNING selcollist.
func (p *parser) upserts() ([]*ast.Upsert, *ast.Returning) {
	var us []*ast.Upsert
	for p.is(token.On) {
		start := p.mark()
		u := &ast.Upsert{On: p.advance(), Conflict: p.expect(token.Conflict)}
		target := p.is(token.LP)
		if target {
			u.LParen = p.advance()
			u.Target, u.TargetCommas = p.sortList()
			u.RParen = p.expect(token.RP)
			u.TargetWhere = p.whereOpt()
		}
		u.Do = p.expect(token.Do)
		switch {
		case p.is(token.Nothing):
			u.Nothing = p.advance()
		case p.is(token.Update):
			u.Update = p.advance()
			u.Set = p.expect(token.Set)
			u.Assignments, u.Commas = p.setList()
			u.Where = p.whereOpt()
		default:
			p.fail()
		}
		us = append(us, finish(p, u, start))
		if !target {
			break
		}
	}
	if p.is(token.Returning) {
		return us, p.returning()
	}
	return us, nil
}
