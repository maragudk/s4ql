package parser

import (
	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/token"
)

// fallback holds the keywords of `%fallback ID` in parse.y, with the compile options of the binding:
// the parser reads such a keyword as the name ID where the keyword itself cannot be shifted.
// EXCEPT, INTERSECT, and UNION fall back only with SQLITE_OMIT_COMPOUND_SELECT, and WITHIN only with
// SQLITE_ENABLE_ORDERED_SET_AGGREGATES, so they are not here.
var fallback = func() [256]bool {
	var f [256]bool
	for _, k := range []token.Kind{
		token.Abort, token.Action, token.After, token.Analyze, token.Asc, token.Attach, token.Before,
		token.Begin, token.By, token.Cascade, token.Cast, token.ColumnKw, token.Conflict,
		token.Database, token.Deferred, token.Desc, token.Detach, token.Do, token.Each, token.End,
		token.Exclusive, token.Explain, token.Fail, token.For, token.Ignore, token.Immediate,
		token.Initially, token.Instead, token.LikeKw, token.Match, token.No, token.Plan, token.Query,
		token.Key, token.Of, token.Offset, token.Pragma, token.Raise, token.Recursive, token.Release,
		token.Replace, token.Restrict, token.Row, token.Rows, token.Rollback, token.Savepoint,
		token.Temp, token.Trigger, token.Vacuum, token.View, token.Virtual, token.With,
		token.Without, token.Nulls, token.First, token.Last, token.Current, token.Following,
		token.Partition, token.Preceding, token.Range, token.Unbounded, token.Exclude,
		token.Groups, token.Others, token.Ties, token.Generated, token.Always, token.Materialized,
		token.Reindex, token.Rename, token.CTimeKw, token.If,
	} {
		f[k] = true
	}
	return f
}()

// isFallback reports whether the keyword kind falls back to ID: sqlite3ParserFallback(k)==TK_ID.
func isFallback(k token.Kind) bool {
	return fallback[k]
}

// class of names: the terminals that a nonterminal or token class of parse.y for a name accepts.
// Every class also accepts the keywords that fall back to ID.
type class int

const (
	// classNm is nm: ID, INDEXED, JOIN_KW, and STRING.
	classNm class = iota + 1
	// classIdj is idj: ID, INDEXED, and JOIN_KW.
	classIdj
	// classId is id: ID and INDEXED.
	classId
	// classIds is ids: ID and STRING.
	classIds
	// classID is ID alone.
	classID
)

// isName reports whether the token t can be read as a name of class c. A keyword that falls back to
// ID can be; the caller must have checked first for the keywords that the grammar can shift where it
// is, since Lemon tries the fallback only for a token that has no action of its own in the state.
func isName(t tok, c class) bool {
	if isFallback(t.kind) || t.kind == token.ID {
		return true
	}
	switch c {
	case classNm:
		return t.kind == token.Indexed || t.kind == token.JoinKw || t.kind == token.String
	case classIdj:
		return t.kind == token.Indexed || t.kind == token.JoinKw
	case classId:
		return t.kind == token.Indexed
	case classIds:
		return t.kind == token.String
	}
	return false
}

// name of class c at the current token, or a syntax error.
func (p *parser) name(c class) ast.Token {
	if !isName(p.cur(), c) {
		p.fail()
	}
	return p.advance()
}

// nm reads a name of the nonterminal nm.
//
// parse.y: nm ::= idj. nm ::= STRING.
func (p *parser) nm() ast.Token {
	return p.name(classNm)
}

// qualifiedName reads nm dbnm: a name with an optional schema before it.
//
// parse.y: dbnm ::= . dbnm ::= DOT nm. fullname ::= nm. fullname ::= nm DOT nm.
func (p *parser) qualifiedName() ast.QualifiedName {
	first := p.nm()
	if dot := p.accept(token.Dot); dot.Present() {
		return ast.QualifiedName{Schema: first, Dot: dot, Name: p.nm()}
	}
	return ast.QualifiedName{Name: first}
}

// ifNotExists reads an optional IF NOT EXISTS.
//
// parse.y: ifnotexists ::= . ifnotexists ::= IF NOT EXISTS.
func (p *parser) ifNotExists() ast.IfNotExists {
	if !p.is(token.If) {
		return ast.IfNotExists{}
	}
	return ast.IfNotExists{If: p.advance(), Not: p.expect(token.Not), Exists: p.expect(token.Exists)}
}

// ifExists reads an optional IF EXISTS.
//
// parse.y: ifexists ::= IF EXISTS. ifexists ::= .
func (p *parser) ifExists() ast.IfNotExists {
	if !p.is(token.If) {
		return ast.IfNotExists{}
	}
	return ast.IfNotExists{If: p.advance(), Exists: p.expect(token.Exists)}
}

// idList reads a list of names separated by commas.
//
// parse.y: idlist ::= idlist COMMA nm. idlist ::= nm.
func (p *parser) idList() *ast.IDList {
	start := p.mark()
	l := &ast.IDList{Names: []ast.Token{p.nm()}}
	for p.is(token.Comma) {
		l.Commas = append(l.Commas, p.advance())
		l.Names = append(l.Names, p.nm())
	}
	return finish(p, l, start)
}

// columnNames reads a list of names in parentheses, each with an optional COLLATE and ASC or DESC.
//
// parse.y: eidlist_opt ::= LP eidlist RP. eidlist ::= eidlist COMMA nm collate sortorder.
// eidlist ::= nm collate sortorder. collate ::= . collate ::= COLLATE ids.
func (p *parser) columnNames() *ast.ColumnNames {
	start := p.mark()
	l := &ast.ColumnNames{LParen: p.expect(token.LP)}
	for {
		cs := p.mark()
		c := &ast.ColumnName{Name: p.nm()}
		if p.is(token.Collate) {
			c.Collate = p.advance()
			c.Collation = p.name(classIds)
		}
		c.Order = p.sortOrder()
		l.Columns = append(l.Columns, finish(p, c, cs))
		if !p.is(token.Comma) {
			break
		}
		l.Commas = append(l.Commas, p.advance())
	}
	l.RParen = p.expect(token.RP)
	return finish(p, l, start)
}

// sortOrder reads an optional ASC or DESC.
//
// parse.y: sortorder ::= ASC. sortorder ::= DESC. sortorder ::= .
func (p *parser) sortOrder() ast.Token {
	if p.is(token.Asc) || p.is(token.Desc) {
		return p.advance()
	}
	return ast.Token{}
}
