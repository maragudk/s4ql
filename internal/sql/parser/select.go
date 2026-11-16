package parser

import (
	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/token"
)

// selectStmt reads a SELECT or VALUES, with its compound operators. The WITH clause before it, if
// any, has been read by the caller and is w; otherwise it is read here.
//
// parse.y: select ::= WITH wqlist selectnowith. select ::= WITH RECURSIVE wqlist selectnowith.
// select ::= selectnowith. selectnowith ::= oneselect. selectnowith ::= selectnowith multiselect_op
// oneselect. multiselect_op ::= UNION. multiselect_op ::= UNION ALL.
// multiselect_op ::= EXCEPT|INTERSECT.
func (p *parser) selectStmt(w *ast.With) *ast.Select {
	p.enter()
	defer p.leave()
	start := p.mark()
	if w != nil {
		start = w.Bounds().Start
	} else if p.is(token.With) {
		w = p.withClause()
	}
	s := &ast.Select{With: w, First: p.oneSelect()}
	for p.is(token.Union) || p.is(token.Except) || p.is(token.Intersect) {
		cs := p.mark()
		c := &ast.Compound{Op: p.advance()}
		if c.Op.Kind == token.Union {
			c.All = p.accept(token.All)
		}
		c.Core = p.oneSelect()
		s.Compounds = append(s.Compounds, finish(p, c, cs))
	}
	return finish(p, s, start)
}

// oneSelect reads one part of a select: SELECT with its clauses, or VALUES.
//
// parse.y: oneselect ::= SELECT distinct selcollist from where_opt groupby_opt having_opt
// orderby_opt limit_opt. oneselect ::= SELECT distinct selcollist from where_opt groupby_opt
// having_opt window_clause orderby_opt limit_opt. oneselect ::= values. oneselect ::= mvalues.
func (p *parser) oneSelect() ast.SelectCore {
	if p.is(token.Values) {
		return p.values()
	}
	start := p.mark()
	s := &ast.SimpleSelect{Select: p.expect(token.Select)}
	if p.is(token.Distinct) || p.is(token.All) {
		s.Distinct = p.advance()
	}
	s.Columns, s.Commas = p.resultColumns()
	if p.is(token.From) {
		s.From = p.from()
	}
	s.Where = p.whereOpt()
	if p.is(token.Group) {
		gs := p.mark()
		g := &ast.GroupBy{Group: p.advance(), By: p.expect(token.By)}
		g.Exprs, g.Commas = p.exprList()
		s.GroupBy = finish(p, g, gs)
	}
	if p.is(token.Having) {
		hs := p.mark()
		h := &ast.Having{Having: p.advance()}
		h.X = p.expr()
		s.Having = finish(p, h, hs)
	}
	if p.is(token.Window) {
		s.Window = p.windowClause()
	}
	if p.is(token.Order) {
		s.OrderBy = p.orderBy()
	}
	if p.is(token.Limit) {
		s.Limit = p.limitClause()
	}
	return finish(p, s, start)
}

// values reads VALUES and its rows.
//
// parse.y: values ::= VALUES LP nexprlist RP. mvalues ::= values COMMA LP nexprlist RP.
// mvalues ::= mvalues COMMA LP nexprlist RP.
func (p *parser) values() *ast.Values {
	start := p.mark()
	v := &ast.Values{Values: p.advance()}
	for {
		rs := p.mark()
		r := &ast.ValuesRow{LParen: p.expect(token.LP)}
		r.Exprs, r.Commas = p.exprList()
		r.RParen = p.expect(token.RP)
		v.Rows = append(v.Rows, finish(p, r, rs))
		if !p.is(token.Comma) {
			break
		}
		v.Commas = append(v.Commas, p.advance())
	}
	return finish(p, v, start)
}

// resultColumns reads the columns of a SELECT or a RETURNING clause.
//
// parse.y: selcollist ::= sclp scanpt expr scanpt as. selcollist ::= sclp scanpt STAR.
// selcollist ::= sclp scanpt nm DOT STAR. sclp ::= selcollist COMMA. sclp ::= . as ::= AS nm.
// as ::= ids. as ::= .
func (p *parser) resultColumns() ([]*ast.ResultColumn, []ast.Token) {
	var cols []*ast.ResultColumn
	var commas []ast.Token
	for {
		start := p.mark()
		c := &ast.ResultColumn{}
		switch {
		case p.is(token.Star):
			c.Star = p.advance()
		case p.isTableStar():
			c.Table = p.advance()
			c.Dot = p.advance()
			c.Star = p.advance()
		default:
			c.Expr = p.expr()
			c.As, c.Alias = p.alias()
		}
		cols = append(cols, finish(p, c, start))
		if !p.is(token.Comma) {
			return cols, commas
		}
		commas = append(commas, p.advance())
	}
}

// isTableStar reports whether the current tokens are nm DOT STAR. CAST, RAISE, and CTIME_KW are
// keywords where an expression can start, so they are not names here.
func (p *parser) isTableStar() bool {
	t := p.cur()
	if t.kind == token.Cast || t.kind == token.Raise || t.kind == token.CTimeKw || !isName(t, classNm) {
		return false
	}
	return p.peek(1).kind == token.Dot && p.peek(2).kind == token.Star
}

// alias reads an optional alias: AS nm, or ids.
//
// parse.y: as ::= AS nm. as ::= ids. as ::= .
func (p *parser) alias() (ast.Token, ast.Token) {
	if p.is(token.As) {
		as := p.advance()
		return as, p.nm()
	}
	if isName(p.cur(), classIds) {
		return ast.Token{}, p.advance()
	}
	return ast.Token{}, ast.Token{}
}

// whereOpt reads an optional WHERE clause.
//
// parse.y: where_opt ::= . where_opt ::= WHERE expr.
func (p *parser) whereOpt() *ast.Where {
	if !p.is(token.Where) {
		return nil
	}
	start := p.mark()
	w := &ast.Where{Where: p.advance()}
	w.X = p.expr()
	return finish(p, w, start)
}

// from reads FROM and its items.
//
// parse.y: from ::= FROM seltablist.
func (p *parser) from() *ast.From {
	start := p.mark()
	f := &ast.From{From: p.advance()}
	f.Items = p.selTabList()
	return finish(p, f, start)
}

// selTabList reads the items of a FROM clause, or of a join in parentheses, and the join operators
// between them.
//
// parse.y: stl_prefix ::= seltablist joinop. stl_prefix ::= . seltablist ::= stl_prefix nm dbnm as
// on_using. seltablist ::= stl_prefix nm dbnm as indexed_by on_using. seltablist ::= stl_prefix nm
// dbnm LP exprlist RP as on_using. seltablist ::= stl_prefix LP select RP as on_using.
// seltablist ::= stl_prefix LP seltablist RP as on_using.
func (p *parser) selTabList() []*ast.FromItem {
	var items []*ast.FromItem
	var join *ast.JoinOp
	for {
		start := p.mark()
		if join != nil {
			start = join.Bounds().Start
		}
		item := &ast.FromItem{Join: join}
		table := false
		if p.is(token.LP) {
			ss := p.mark()
			lp := p.advance()
			if p.startsSelect() {
				s := &ast.SubquerySource{LParen: lp, Select: p.selectStmt(nil)}
				s.RParen = p.expect(token.RP)
				item.Source = finish(p, s, ss)
			} else {
				item.Source = p.nestedFrom(lp, ss)
			}
		} else {
			ss := p.mark()
			name := p.qualifiedName()
			if p.is(token.LP) {
				f := &ast.TableFunction{Name: name, LParen: p.advance()}
				if !p.is(token.RP) {
					f.Args, f.Commas = p.exprList()
				}
				f.RParen = p.expect(token.RP)
				item.Source = finish(p, f, ss)
			} else {
				item.Source = finish(p, &ast.TableSource{Name: name}, ss)
				table = true
			}
		}
		item.As, item.Alias = p.alias()
		if table && (p.is(token.Indexed) || p.is(token.Not)) {
			item.Indexed = p.indexedBy()
		}
		item.Constraint = p.onUsing()
		items = append(items, finish(p, item, start))
		join = p.joinOp()
		if join == nil {
			return items
		}
	}
}

// nestedFrom reads a list of joined items in parentheses, after the "(" lp at start.
func (p *parser) nestedFrom(lp ast.Token, start int) *ast.NestedFrom {
	p.enter()
	defer p.leave()
	n := &ast.NestedFrom{LParen: lp}
	n.Items = p.selTabList()
	n.RParen = p.expect(token.RP)
	return finish(p, n, start)
}

// indexedBy reads INDEXED BY name or NOT INDEXED.
//
// parse.y: indexed_by ::= INDEXED BY nm. indexed_by ::= NOT INDEXED. indexed_opt ::= indexed_by.
func (p *parser) indexedBy() *ast.IndexedBy {
	start := p.mark()
	ib := &ast.IndexedBy{}
	if p.is(token.Not) {
		ib.Not = p.advance()
		ib.Indexed = p.expect(token.Indexed)
	} else {
		ib.Indexed = p.advance()
		ib.By = p.expect(token.By)
		ib.Index = p.nm()
	}
	return finish(p, ib, start)
}

// onUsing reads an optional ON or USING after an item of a FROM clause. ON is %nonassoc with a
// higher precedence than the [OR] of the empty rule, so an ON here is always the join's, even where
// an ON CONFLICT of an upsert could follow.
//
// parse.y: on_using ::= ON expr. on_using ::= USING LP idlist RP. on_using ::= . [OR]
func (p *parser) onUsing() ast.JoinConstraint {
	start := p.mark()
	switch {
	case p.is(token.On):
		o := &ast.JoinOn{On: p.advance()}
		o.X = p.expr()
		return finish(p, o, start)
	case p.is(token.Using):
		u := &ast.JoinUsing{Using: p.advance(), LParen: p.expect(token.LP)}
		u.Columns = p.idList()
		u.RParen = p.expect(token.RP)
		return finish(p, u, start)
	}
	return nil
}

// joinOp reads an optional join operator.
//
// parse.y: joinop ::= COMMA|JOIN. joinop ::= JOIN_KW JOIN. joinop ::= JOIN_KW nm JOIN.
// joinop ::= JOIN_KW nm nm JOIN.
func (p *parser) joinOp() *ast.JoinOp {
	start := p.mark()
	switch p.cur().kind {
	case token.Comma:
		return finish(p, &ast.JoinOp{Comma: p.advance()}, start)
	case token.Join:
		return finish(p, &ast.JoinOp{Join: p.advance()}, start)
	case token.JoinKw:
		j := &ast.JoinOp{Words: []ast.Token{p.advance()}}
		for range 2 {
			if p.is(token.Join) {
				break
			}
			j.Words = append(j.Words, p.nm())
		}
		j.Join = p.expect(token.Join)
		return finish(p, j, start)
	}
	return nil
}

// orderBy reads ORDER BY and its terms.
//
// parse.y: orderby_opt ::= ORDER BY sortlist.
func (p *parser) orderBy() *ast.OrderBy {
	start := p.mark()
	o := &ast.OrderBy{Order: p.advance(), By: p.expect(token.By)}
	o.Terms, o.Commas = p.sortList()
	return finish(p, o, start)
}

// sortList reads one or more ordering terms separated by commas.
//
// parse.y: sortlist ::= sortlist COMMA expr sortorder nulls. sortlist ::= expr sortorder nulls.
// nulls ::= NULLS FIRST. nulls ::= NULLS LAST. nulls ::= .
func (p *parser) sortList() ([]*ast.OrderingTerm, []ast.Token) {
	var terms []*ast.OrderingTerm
	var commas []ast.Token
	for {
		start := p.mark()
		t := &ast.OrderingTerm{X: p.expr()}
		t.Order = p.sortOrder()
		if p.is(token.Nulls) {
			t.Nulls = p.advance()
			if !p.is(token.First) && !p.is(token.Last) {
				p.fail()
			}
			t.Which = p.advance()
		}
		terms = append(terms, finish(p, t, start))
		if !p.is(token.Comma) {
			return terms, commas
		}
		commas = append(commas, p.advance())
	}
}

// limitClause reads LIMIT.
//
// parse.y: limit_opt ::= LIMIT expr. limit_opt ::= LIMIT expr OFFSET expr. limit_opt ::= LIMIT expr
// COMMA expr.
func (p *parser) limitClause() *ast.Limit {
	start := p.mark()
	l := &ast.Limit{Limit: p.advance()}
	l.X = p.expr()
	if p.is(token.Offset) || p.is(token.Comma) {
		l.Sep = p.advance()
		l.Y = p.expr()
	}
	return finish(p, l, start)
}

// withClause reads WITH and its common table expressions.
//
// parse.y: with ::= WITH wqlist. with ::= WITH RECURSIVE wqlist. wqlist ::= wqitem.
// wqlist ::= wqlist COMMA wqitem. wqitem ::= withnm eidlist_opt wqas LP select RP.
// withnm ::= nm. wqas ::= AS. wqas ::= AS MATERIALIZED. wqas ::= AS NOT MATERIALIZED.
func (p *parser) withClause() *ast.With {
	start := p.mark()
	w := &ast.With{With: p.advance()}
	w.Recursive = p.accept(token.Recursive)
	for {
		cs := p.mark()
		c := &ast.CTE{Name: p.nm()}
		if p.is(token.LP) {
			c.Columns = p.columnNames()
		}
		c.As = p.expect(token.As)
		if p.is(token.Not) {
			c.Not = p.advance()
			c.Materialized = p.expect(token.Materialized)
		} else {
			c.Materialized = p.accept(token.Materialized)
		}
		c.LParen = p.expect(token.LP)
		c.Select = p.selectStmt(nil)
		c.RParen = p.expect(token.RP)
		w.CTEs = append(w.CTEs, finish(p, c, cs))
		if !p.is(token.Comma) {
			return finish(p, w, start)
		}
		w.Commas = append(w.Commas, p.advance())
	}
}

// windowClause reads WINDOW and its definitions.
//
// parse.y: window_clause ::= WINDOW windowdefn_list. windowdefn_list ::= windowdefn.
// windowdefn_list ::= windowdefn_list COMMA windowdefn. windowdefn ::= nm AS LP window RP.
func (p *parser) windowClause() *ast.WindowClause {
	start := p.mark()
	wc := &ast.WindowClause{Window: p.advance()}
	for {
		ds := p.mark()
		d := &ast.WindowDef{Name: p.nm(), As: p.expect(token.As), LParen: p.expect(token.LP)}
		d.Window = p.window()
		d.RParen = p.expect(token.RP)
		wc.Defs = append(wc.Defs, finish(p, d, ds))
		if !p.is(token.Comma) {
			return finish(p, wc, start)
		}
		wc.Commas = append(wc.Commas, p.advance())
	}
}

// window reads the inside of the parentheses of a window, or nil if it is empty. PARTITION, RANGE,
// ROWS, and GROUPS are keywords where it starts, and any other name is the name of a base window.
//
// parse.y: window ::= PARTITION BY nexprlist orderby_opt frame_opt. window ::= nm PARTITION BY
// nexprlist orderby_opt frame_opt. window ::= ORDER BY sortlist frame_opt. window ::= nm ORDER BY
// sortlist frame_opt. window ::= frame_opt. window ::= nm frame_opt.
func (p *parser) window() *ast.Window {
	start := p.mark()
	w := &ast.Window{}
	switch p.cur().kind {
	case token.Partition, token.Order, token.Range, token.Rows, token.Groups, token.RP:
	default:
		w.Base = p.nm()
	}
	if p.is(token.Partition) {
		w.Partition = p.advance()
		w.PartitionBy = p.expect(token.By)
		w.PartitionExprs, w.PartitionCommas = p.exprList()
	}
	if p.is(token.Order) {
		w.OrderBy = p.orderBy()
	}
	if p.is(token.Range) || p.is(token.Rows) || p.is(token.Groups) {
		w.Frame = p.frame()
	}
	if !w.Base.Present() && w.Partition.Kind == 0 && w.OrderBy == nil && w.Frame == nil {
		return nil
	}
	return finish(p, w, start)
}

// frame reads the frame of a window.
//
// parse.y: frame_opt ::= range_or_rows frame_bound_s frame_exclude_opt. frame_opt ::= range_or_rows
// BETWEEN frame_bound_s AND frame_bound_e frame_exclude_opt. range_or_rows ::= RANGE|ROWS|GROUPS.
// frame_exclude_opt ::= EXCLUDE frame_exclude. frame_exclude ::= NO OTHERS. frame_exclude ::=
// CURRENT ROW. frame_exclude ::= GROUP|TIES.
func (p *parser) frame() *ast.Frame {
	start := p.mark()
	f := &ast.Frame{Unit: p.advance()}
	if p.is(token.Between) {
		f.Between = p.advance()
		f.Start = p.frameBound(token.Preceding)
		f.And = p.expect(token.And)
		f.End = p.frameBound(token.Following)
	} else {
		f.Start = p.frameBound(token.Preceding)
	}
	if p.is(token.Exclude) {
		f.Exclude = p.advance()
		switch p.cur().kind {
		case token.No:
			f.Excluded = []ast.Token{p.advance(), p.expect(token.Others)}
		case token.Current:
			f.Excluded = []ast.Token{p.advance(), p.expect(token.Row)}
		case token.Group, token.Ties:
			f.Excluded = []ast.Token{p.advance()}
		default:
			p.fail()
		}
	}
	return finish(p, f, start)
}

// frameBound reads a bound of a frame, where UNBOUNDED goes with the direction unbounded:
// PRECEDING for the start, FOLLOWING for the end.
//
// parse.y: frame_bound_s ::= frame_bound. frame_bound_s ::= UNBOUNDED PRECEDING.
// frame_bound_e ::= frame_bound. frame_bound_e ::= UNBOUNDED FOLLOWING. frame_bound ::= expr
// PRECEDING|FOLLOWING. frame_bound ::= CURRENT ROW.
func (p *parser) frameBound(unbounded token.Kind) *ast.FrameBound {
	start := p.mark()
	b := &ast.FrameBound{}
	switch p.cur().kind {
	case token.Unbounded:
		b.Unbounded = p.advance()
		b.Direction = p.expect(unbounded)
	case token.Current:
		b.Current = p.advance()
		b.Row = p.expect(token.Row)
	default:
		b.X = p.expr()
		if !p.is(token.Preceding) && !p.is(token.Following) {
			p.fail()
		}
		b.Direction = p.advance()
	}
	return finish(p, b, start)
}

// returning reads RETURNING and its columns.
//
// parse.y: returning ::= RETURNING selcollist.
func (p *parser) returning() *ast.Returning {
	start := p.mark()
	r := &ast.Returning{Returning: p.advance()}
	r.Columns, r.Commas = p.resultColumns()
	return finish(p, r, start)
}
