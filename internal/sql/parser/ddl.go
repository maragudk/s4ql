package parser

import (
	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/token"
)

// create reads a command that starts with CREATE.
//
// parse.y: createkw ::= CREATE. create_table ::= createkw temp TABLE ifnotexists nm dbnm.
// cmd ::= createkw temp VIEW ... cmd ::= createkw uniqueflag INDEX ... cmd ::= createkw
// trigger_decl ... create_vtab ::= createkw VIRTUAL TABLE ... temp ::= TEMP. temp ::= .
// uniqueflag ::= UNIQUE. uniqueflag ::= .
func (p *parser) create() ast.Cmd {
	start := p.mark()
	create := p.advance()
	temp := p.accept(token.Temp)
	switch p.cur().kind {
	case token.Table:
		return p.createTable(start, create, temp)
	case token.View:
		return p.createView(start, create, temp)
	case token.Trigger:
		return p.createTrigger(start, create, temp)
	}
	if !temp.Present() {
		switch p.cur().kind {
		case token.Unique, token.Index:
			return p.createIndex(start, create)
		case token.Virtual:
			return p.createVirtualTable(start, create)
		}
	}
	p.fail()
	return nil
}

// createTable reads CREATE TABLE after CREATE and TEMP.
//
// parse.y: cmd ::= create_table create_table_args. create_table_args ::= LP columnlist
// conslist_opt RP table_option_set. create_table_args ::= AS select. columnlist ::= columnlist
// COMMA columnname carglist. columnlist ::= columnname carglist. conslist_opt ::= .
// conslist_opt ::= COMMA conslist. conslist ::= conslist tconscomma tcons. conslist ::= tcons.
// tconscomma ::= COMMA. tconscomma ::= .
func (p *parser) createTable(start int, create, temp ast.Token) *ast.CreateTable {
	t := &ast.CreateTable{Create: create, Temp: temp, Table: p.advance()}
	t.IfNotExists = p.ifNotExists()
	t.Name = p.qualifiedName()
	if p.is(token.As) {
		t.As = p.advance()
		t.Select = p.selectStmt(nil)
		return finish(p, t, start)
	}
	t.LParen = p.expect(token.LP)
	t.Columns = append(t.Columns, p.columnDef())
	constraints := false
	for p.is(token.Comma) {
		t.Commas = append(t.Commas, p.advance())
		if p.startsTableConstraint() {
			constraints = true
			break
		}
		t.Columns = append(t.Columns, p.columnDef())
	}
	if constraints {
		for {
			t.Constraints = append(t.Constraints, p.tableConstraint())
			if p.is(token.Comma) {
				// After a comma a constraint must follow; tableConstraint fails at any other token.
				t.Commas = append(t.Commas, p.advance())
			} else if !p.startsTableConstraint() {
				break
			}
		}
	}
	t.RParen = p.expect(token.RP)
	// The first option can be empty, so a comma can come right after the parenthesis. WITHOUT falls
	// back to ID, so it is a name here too.
	if isName(p.cur(), classNm) {
		t.Options = append(t.Options, p.tableOption())
	}
	for p.is(token.Comma) {
		t.OptionCommas = append(t.OptionCommas, p.advance())
		t.Options = append(t.Options, p.tableOption())
	}
	return finish(p, t, start)
}

// tableOption reads one table option.
//
// parse.y: table_option_set ::= . table_option_set ::= table_option. table_option_set ::=
// table_option_set COMMA table_option. table_option ::= WITHOUT nm. table_option ::= nm.
func (p *parser) tableOption() *ast.TableOption {
	start := p.mark()
	o := &ast.TableOption{}
	if p.is(token.Without) {
		o.Without = p.advance()
	}
	o.Name = p.nm()
	return finish(p, o, start)
}

// startsTableConstraint reports whether the current token starts a table constraint.
func (p *parser) startsTableConstraint() bool {
	switch p.cur().kind {
	case token.Constraint, token.Primary, token.Unique, token.Check, token.Foreign:
		return true
	}
	return false
}

// columnDef reads a column definition: a name, a type, and constraints.
//
// parse.y: columnname ::= nm typetoken. carglist ::= carglist ccons. carglist ::= .
func (p *parser) columnDef() *ast.ColumnDef {
	start := p.mark()
	c := &ast.ColumnDef{Name: p.nm()}
	c.Type = p.typeToken()
	for {
		cc := p.columnConstraint()
		if cc == nil {
			return finish(p, c, start)
		}
		c.Constraints = append(c.Constraints, cc)
	}
}

// typeToken reads an optional type name. Every keyword that falls back to ID is a word of the type
// here, GENERATED too: in the states after a column's name and after a word of its type, the empty
// typetoken and `typetoken ::= typename` are the default reductions, which Lemon does not list as
// actions, so GENERATED has no action of its own there and falls back to ID, which has one.
//
// parse.y: typetoken ::= . typetoken ::= typename. typetoken ::= typename LP signed RP.
// typetoken ::= typename LP signed COMMA signed RP. typename ::= ids. typename ::= typename ids.
func (p *parser) typeToken() *ast.TypeName {
	if !isName(p.cur(), classIds) {
		return nil
	}
	start := p.mark()
	t := &ast.TypeName{}
	for isName(p.cur(), classIds) {
		t.Names = append(t.Names, p.advance())
	}
	if p.is(token.LP) {
		t.LParen = p.advance()
		t.Arg1 = p.signedNumber()
		if p.is(token.Comma) {
			t.Comma = p.advance()
			t.Arg2 = p.signedNumber()
		}
		t.RParen = p.expect(token.RP)
	}
	return finish(p, t, start)
}

// signedNumber reads a number with an optional sign.
//
// parse.y: signed ::= plus_num. signed ::= minus_num. plus_num ::= PLUS number. plus_num ::= number.
// minus_num ::= MINUS number.
func (p *parser) signedNumber() *ast.SignedNumber {
	start := p.mark()
	s := &ast.SignedNumber{}
	if p.is(token.Plus) || p.is(token.Minus) {
		s.Sign = p.advance()
	}
	s.Number = p.number()
	return finish(p, s, start)
}

// columnConstraint reads one constraint of a column, or returns nil if none starts at the current
// token.
//
// parse.y: the rules for ccons, generated, and autoinc.
func (p *parser) columnConstraint() ast.ColumnConstraint {
	start := p.mark()
	switch p.cur().kind {
	case token.Constraint:
		return finish(p, &ast.ConstraintName{Constraint: p.advance(), Name: p.nm()}, start)
	case token.Default:
		return p.defaultConstraint()
	case token.Null:
		n := &ast.NullConstraint{Null: p.advance()}
		n.Conflict = p.onConf()
		return finish(p, n, start)
	case token.Not:
		not := p.advance()
		if p.is(token.Deferrable) {
			return finish(p, &ast.DeferConstraint{Defer: p.deferrable(not, start)}, start)
		}
		n := &ast.NullConstraint{Not: not, Null: p.expect(token.Null)}
		n.Conflict = p.onConf()
		return finish(p, n, start)
	case token.Primary:
		pk := &ast.PrimaryKeyConstraint{Primary: p.advance(), Key: p.expect(token.Key)}
		pk.Order = p.sortOrder()
		pk.Conflict = p.onConf()
		pk.Autoincrement = p.accept(token.Autoincr)
		return finish(p, pk, start)
	case token.Unique:
		u := &ast.UniqueConstraint{Unique: p.advance()}
		u.Conflict = p.onConf()
		return finish(p, u, start)
	case token.Check:
		c := &ast.CheckConstraint{Check: p.advance(), LParen: p.expect(token.LP)}
		c.X = p.expr()
		c.RParen = p.expect(token.RP)
		return finish(p, c, start)
	case token.References:
		return finish(p, &ast.ReferencesConstraint{References: p.references()}, start)
	case token.Deferrable:
		return finish(p, &ast.DeferConstraint{Defer: p.deferrable(ast.Token{}, start)}, start)
	case token.Collate:
		return finish(p, &ast.CollateConstraint{Collate: p.advance(), Name: p.name(classIds)}, start)
	case token.Generated:
		g := &ast.GeneratedConstraint{Generated: p.advance(), Always: p.expect(token.Always)}
		return p.generated(g, start)
	case token.As:
		return p.generated(&ast.GeneratedConstraint{}, start)
	}
	return nil
}

// generated reads AS and the expression of a generated column, with an optional name after it.
//
// parse.y: ccons ::= GENERATED ALWAYS AS generated. ccons ::= AS generated. generated ::= LP expr RP.
// generated ::= LP expr RP ID.
func (p *parser) generated(g *ast.GeneratedConstraint, start int) *ast.GeneratedConstraint {
	g.As = p.expect(token.As)
	g.LParen = p.expect(token.LP)
	g.X = p.expr()
	g.RParen = p.expect(token.RP)
	if isName(p.cur(), classID) {
		g.Storage = p.advance()
	}
	return finish(p, g, start)
}

// defaultConstraint reads DEFAULT and its value. After DEFAULT, CURRENT_DATE, CURRENT_TIME, and
// CURRENT_TIMESTAMP are terms, and every other keyword that falls back to ID is a name.
//
// parse.y: ccons ::= DEFAULT scantok term. ccons ::= DEFAULT LP expr RP. ccons ::= DEFAULT PLUS
// scantok term. ccons ::= DEFAULT MINUS scantok term. ccons ::= DEFAULT scantok id.
func (p *parser) defaultConstraint() *ast.DefaultConstraint {
	start := p.mark()
	d := &ast.DefaultConstraint{Default: p.advance()}
	switch {
	case p.is(token.LP):
		d.LParen = p.advance()
		d.X = p.expr()
		d.RParen = p.expect(token.RP)
	case p.is(token.Plus) || p.is(token.Minus):
		d.Sign = p.advance()
		d.X = p.term()
	case p.isTerm():
		d.X = p.term()
	case isName(p.cur(), classId):
		d.Name = p.advance()
	default:
		p.fail()
	}
	return finish(p, d, start)
}

// isTerm reports whether the current token is a term.
func (p *parser) isTerm() bool {
	switch p.cur().kind {
	case token.Null, token.Float, token.Blob, token.String, token.Integer, token.QNumber, token.CTimeKw:
		return true
	}
	return false
}

// term reads a literal.
//
// parse.y: term ::= NULL|FLOAT|BLOB. term ::= STRING. term ::= INTEGER. term ::= QNUMBER.
// term ::= CTIME_KW.
func (p *parser) term() ast.Expr {
	if !p.isTerm() {
		p.fail()
	}
	start := p.mark()
	if p.is(token.QNumber) {
		p.checkDigitSeparators()
	}
	return finish(p, &ast.Literal{Value: p.advance()}, start)
}

// onConf reads an optional ON CONFLICT clause.
//
// parse.y: onconf ::= . onconf ::= ON CONFLICT resolvetype.
func (p *parser) onConf() *ast.OnConflict {
	if !p.is(token.On) {
		return nil
	}
	start := p.mark()
	o := &ast.OnConflict{On: p.advance(), Conflict: p.expect(token.Conflict)}
	o.Resolution = p.resolveType()
	return finish(p, o, start)
}

// references reads REFERENCES and its actions.
//
// parse.y: REFERENCES nm eidlist_opt refargs. refargs ::= . refargs ::= refargs refarg.
// refarg ::= MATCH nm. refarg ::= ON INSERT refact. refarg ::= ON DELETE refact. refarg ::= ON
// UPDATE refact. refact ::= SET NULL. refact ::= SET DEFAULT. refact ::= CASCADE.
// refact ::= RESTRICT. refact ::= NO ACTION.
func (p *parser) references() *ast.References {
	start := p.mark()
	r := &ast.References{References: p.expect(token.References), Table: p.nm()}
	if p.is(token.LP) {
		r.Columns = p.columnNames()
	}
	for {
		as := p.mark()
		a := &ast.RefArg{}
		switch {
		case p.is(token.Match):
			a.Match = p.advance()
			a.Name = p.nm()
		case p.is(token.On):
			a.On = p.advance()
			switch p.cur().kind {
			case token.Insert, token.Delete, token.Update:
				a.Event = p.advance()
			default:
				p.fail()
			}
			switch p.cur().kind {
			case token.Set:
				set := p.advance()
				if !p.is(token.Null) && !p.is(token.Default) {
					p.fail()
				}
				a.Action = []ast.Token{set, p.advance()}
			case token.Cascade, token.Restrict:
				a.Action = []ast.Token{p.advance()}
			case token.No:
				a.Action = []ast.Token{p.advance(), p.expect(token.Action)}
			default:
				p.fail()
			}
		default:
			return finish(p, r, start)
		}
		r.Args = append(r.Args, finish(p, a, as))
	}
}

// deferrable reads [NOT] DEFERRABLE [INITIALLY DEFERRED|IMMEDIATE], where not is the NOT already
// read, if any, which started at start.
//
// parse.y: defer_subclause ::= NOT DEFERRABLE init_deferred_pred_opt. defer_subclause ::= DEFERRABLE
// init_deferred_pred_opt. init_deferred_pred_opt ::= . init_deferred_pred_opt ::= INITIALLY
// DEFERRED. init_deferred_pred_opt ::= INITIALLY IMMEDIATE.
func (p *parser) deferrable(not ast.Token, start int) *ast.Deferrable {
	d := &ast.Deferrable{Not: not, Deferrable: p.expect(token.Deferrable)}
	if p.is(token.Initially) {
		d.Initially = p.advance()
		if !p.is(token.Deferred) && !p.is(token.Immediate) {
			p.fail()
		}
		d.Mode = p.advance()
	}
	return finish(p, d, start)
}

// tableConstraint reads one table constraint.
//
// parse.y: tcons ::= CONSTRAINT nm. tcons ::= PRIMARY KEY LP sortlist autoinc RP onconf.
// tcons ::= UNIQUE LP sortlist RP onconf. tcons ::= CHECK LP expr RP onconf. tcons ::= FOREIGN KEY LP
// eidlist RP REFERENCES nm eidlist_opt refargs defer_subclause_opt.
func (p *parser) tableConstraint() ast.TableConstraint {
	start := p.mark()
	switch p.cur().kind {
	case token.Constraint:
		return finish(p, &ast.ConstraintName{Constraint: p.advance(), Name: p.nm()}, start)
	case token.Primary:
		pk := &ast.TablePrimaryKey{Primary: p.advance(), Key: p.expect(token.Key), LParen: p.expect(token.LP)}
		pk.Columns, pk.Commas = p.sortList()
		pk.Autoincrement = p.accept(token.Autoincr)
		pk.RParen = p.expect(token.RP)
		pk.Conflict = p.onConf()
		return finish(p, pk, start)
	case token.Unique:
		u := &ast.TableUnique{Unique: p.advance(), LParen: p.expect(token.LP)}
		u.Columns, u.Commas = p.sortList()
		u.RParen = p.expect(token.RP)
		u.Conflict = p.onConf()
		return finish(p, u, start)
	case token.Check:
		c := &ast.CheckConstraint{Check: p.advance(), LParen: p.expect(token.LP)}
		c.X = p.expr()
		c.RParen = p.expect(token.RP)
		c.Conflict = p.onConf()
		return finish(p, c, start)
	case token.Foreign:
		fk := &ast.TableForeignKey{Foreign: p.advance(), Key: p.expect(token.Key)}
		fk.Columns = p.columnNames()
		fk.References = p.references()
		if p.is(token.Not) || p.is(token.Deferrable) {
			ds := p.mark()
			fk.Defer = p.deferrable(p.accept(token.Not), ds)
		}
		return finish(p, fk, start)
	}
	p.fail()
	return nil
}

// createView reads CREATE VIEW after CREATE and TEMP.
//
// parse.y: cmd ::= createkw temp VIEW ifnotexists nm dbnm eidlist_opt AS select.
func (p *parser) createView(start int, create, temp ast.Token) *ast.CreateView {
	v := &ast.CreateView{Create: create, Temp: temp, View: p.advance()}
	v.IfNotExists = p.ifNotExists()
	v.Name = p.qualifiedName()
	if p.is(token.LP) {
		v.Columns = p.columnNames()
	}
	v.As = p.expect(token.As)
	v.Select = p.selectStmt(nil)
	return finish(p, v, start)
}

// createIndex reads CREATE INDEX after CREATE.
//
// parse.y: cmd ::= createkw uniqueflag INDEX ifnotexists nm dbnm ON nm LP sortlist RP where_opt.
func (p *parser) createIndex(start int, create ast.Token) *ast.CreateIndex {
	ix := &ast.CreateIndex{Create: create}
	ix.Unique = p.accept(token.Unique)
	ix.Index = p.expect(token.Index)
	ix.IfNotExists = p.ifNotExists()
	ix.Name = p.qualifiedName()
	ix.On = p.expect(token.On)
	ix.Table = p.nm()
	ix.LParen = p.expect(token.LP)
	ix.Columns, ix.Commas = p.sortList()
	ix.RParen = p.expect(token.RP)
	ix.Where = p.whereOpt()
	return finish(p, ix, start)
}

// createTrigger reads CREATE TRIGGER after CREATE and TEMP.
//
// parse.y: cmd ::= createkw trigger_decl BEGIN trigger_cmd_list END. trigger_decl ::= temp TRIGGER
// ifnotexists nm dbnm trigger_time trigger_event ON fullname foreach_clause when_clause.
// trigger_time ::= BEFORE|AFTER. trigger_time ::= INSTEAD OF. trigger_time ::= .
// trigger_event ::= DELETE|INSERT. trigger_event ::= UPDATE. trigger_event ::= UPDATE OF idlist.
// foreach_clause ::= . foreach_clause ::= FOR EACH ROW. when_clause ::= . when_clause ::= WHEN expr.
// trigger_cmd_list ::= trigger_cmd_list trigger_cmd SEMI. trigger_cmd_list ::= trigger_cmd SEMI.
func (p *parser) createTrigger(start int, create, temp ast.Token) *ast.CreateTrigger {
	t := &ast.CreateTrigger{Create: create, Temp: temp, Trigger: p.advance()}
	t.IfNotExists = p.ifNotExists()
	t.Name = p.qualifiedName()
	switch p.cur().kind {
	case token.Before, token.After:
		t.Time = p.advance()
	case token.Instead:
		t.Time = p.advance()
		t.TimeOf = p.expect(token.Of)
	}
	switch p.cur().kind {
	case token.Delete, token.Insert:
		t.Event = p.advance()
	case token.Update:
		t.Event = p.advance()
		if p.is(token.Of) {
			t.EventOf = p.advance()
			t.Columns = p.idList()
		}
	default:
		p.fail()
	}
	t.On = p.expect(token.On)
	t.Table = p.qualifiedName()
	if p.is(token.For) {
		t.For = p.advance()
		t.Each = p.expect(token.Each)
		t.Row = p.expect(token.Row)
	}
	if p.is(token.When) {
		t.When = p.advance()
		t.WhenX = p.expr()
	}
	t.Begin = p.expect(token.Begin)
	for {
		t.Steps = append(t.Steps, p.triggerCmd())
		if !p.is(token.Semi) {
			p.fail()
		}
		if p.cur().synthetic() {
			p.i++
		} else {
			t.Semis = append(t.Semis, p.advance())
		}
		if p.is(token.End) {
			t.End = p.advance()
			return finish(p, t, start)
		}
	}
}

// triggerCmd reads one step of a trigger.
//
// parse.y: trigger_cmd ::= UPDATE orconf xfullname tridxby SET setlist from where_opt scanpt.
// trigger_cmd ::= scanpt insert_cmd INTO xfullname idlist_opt select upsert scanpt.
// trigger_cmd ::= DELETE FROM xfullname tridxby where_opt scanpt. trigger_cmd ::= scanpt select
// scanpt. tridxby ::= . tridxby ::= INDEXED BY nm. tridxby ::= NOT INDEXED.
func (p *parser) triggerCmd() ast.Cmd {
	start := p.mark()
	switch p.cur().kind {
	case token.Update:
		return p.update(start, nil, true)
	case token.Insert, token.Replace:
		return p.insert(start, nil, true)
	case token.Delete:
		return p.delete(start, nil, true)
	case token.Select, token.Values, token.With:
		return finish(p, &ast.SelectCmd{Select: p.selectStmt(nil)}, start)
	}
	p.fail()
	return nil
}

// createVirtualTable reads CREATE VIRTUAL TABLE after CREATE. Its arguments are any tokens, but for
// the end of the input: the %wildcard ANY matches every token that has no action of its own, which
// is every token but "(", ")", and, outside parentheses, ",". The empty semicolon at the end of the
// text is such a token too, and the end of input after it is the syntax error.
//
// parse.y: cmd ::= create_vtab. cmd ::= create_vtab LP vtabarglist RP. create_vtab ::= createkw
// VIRTUAL TABLE ifnotexists nm dbnm USING nm. vtabarglist ::= vtabarg. vtabarglist ::= vtabarglist
// COMMA vtabarg. vtabarg ::= . vtabarg ::= vtabarg vtabargtoken. vtabargtoken ::= ANY.
// vtabargtoken ::= lp anylist RP. lp ::= LP. anylist ::= . anylist ::= anylist LP anylist RP.
// anylist ::= anylist ANY.
func (p *parser) createVirtualTable(start int, create ast.Token) *ast.CreateVirtualTable {
	v := &ast.CreateVirtualTable{Create: create, Virtual: p.advance(), Table: p.expect(token.Table)}
	v.IfNotExists = p.ifNotExists()
	v.Name = p.qualifiedName()
	v.Using = p.expect(token.Using)
	v.Module = p.nm()
	if !p.is(token.LP) {
		return finish(p, v, start)
	}
	v.LParen = p.advance()
	for {
		as := p.mark()
		a := &ast.ModuleArg{}
		depth := 0
		for {
			t := p.cur()
			if t.kind == kindEOF || t.kind == token.Illegal {
				p.fail()
			}
			if depth == 0 && (t.kind == token.Comma || t.kind == token.RP) {
				break
			}
			switch t.kind {
			case token.LP:
				depth++
			case token.RP:
				depth--
			}
			if t.synthetic() {
				// The empty semicolon matches ANY, and the end of input after it fails.
				p.i++
				continue
			}
			a.Tokens = append(a.Tokens, p.advance())
		}
		if len(a.Tokens) == 0 {
			ast.SetSpan(a, ast.Span{Start: as, End: as})
		} else {
			finish(p, a, as)
		}
		v.Args = append(v.Args, a)
		if p.is(token.RP) {
			v.RParen = p.advance()
			return finish(p, v, start)
		}
		v.Commas = append(v.Commas, p.advance())
	}
}

// drop reads DROP TABLE, VIEW, INDEX, or TRIGGER.
//
// parse.y: cmd ::= DROP TABLE ifexists fullname. cmd ::= DROP VIEW ifexists fullname. cmd ::= DROP
// INDEX ifexists fullname. cmd ::= DROP TRIGGER ifexists fullname.
func (p *parser) drop() *ast.Drop {
	start := p.mark()
	d := &ast.Drop{Drop: p.advance()}
	switch p.cur().kind {
	case token.Table, token.View, token.Index, token.Trigger:
		d.Object = p.advance()
	default:
		p.fail()
	}
	d.IfExists = p.ifExists()
	d.Name = p.qualifiedName()
	return finish(p, d, start)
}

// alterTable reads ALTER TABLE.
//
// parse.y: cmd ::= ALTER TABLE fullname RENAME TO nm. cmd ::= alter_add carglist. alter_add ::=
// ALTER TABLE fullname ADD kwcolumn_opt nm typetoken. cmd ::= ALTER TABLE fullname DROP kwcolumn_opt
// nm. cmd ::= ALTER TABLE fullname RENAME kwcolumn_opt nm TO nm. cmd ::= ALTER TABLE fullname DROP
// CONSTRAINT nm. cmd ::= ALTER TABLE fullname ALTER kwcolumn_opt nm DROP NOT NULL. cmd ::= ALTER
// TABLE fullname ALTER kwcolumn_opt nm SET NOT NULL onconf. cmd ::= ALTER TABLE fullname ADD
// CONSTRAINT nm CHECK LP expr RP onconf. cmd ::= ALTER TABLE fullname ADD CHECK LP expr RP onconf.
// kwcolumn_opt ::= . kwcolumn_opt ::= COLUMNKW.
func (p *parser) alterTable() *ast.AlterTable {
	start := p.mark()
	a := &ast.AlterTable{Alter: p.advance(), Table: p.expect(token.Table), Name: p.qualifiedName()}
	as := p.mark()
	switch p.cur().kind {
	case token.Rename:
		rename := p.advance()
		if p.is(token.To) {
			a.Action = finish(p, &ast.RenameTable{Rename: rename, To: p.advance(), Name: p.nm()}, as)
			break
		}
		rc := &ast.RenameColumn{Rename: rename, Column: p.accept(token.ColumnKw)}
		rc.Old = p.nm()
		rc.To = p.expect(token.To)
		rc.New = p.nm()
		a.Action = finish(p, rc, as)
	case token.Add:
		add := p.advance()
		switch p.cur().kind {
		case token.Constraint:
			ac := &ast.AddCheck{Add: add, Constraint: p.advance()}
			ac.Name = p.nm()
			ac.Check = p.checkWithConflict()
			a.Action = finish(p, ac, as)
		case token.Check:
			a.Action = finish(p, &ast.AddCheck{Add: add, Check: p.checkWithConflict()}, as)
		default:
			ac := &ast.AddColumn{Add: add, Column: p.accept(token.ColumnKw)}
			ac.Def = p.columnDef()
			a.Action = finish(p, ac, as)
		}
	case token.Drop:
		drop := p.advance()
		if p.is(token.Constraint) {
			a.Action = finish(p, &ast.DropConstraint{Drop: drop, Constraint: p.advance(), Name: p.nm()}, as)
			break
		}
		dc := &ast.DropColumn{Drop: drop, Column: p.accept(token.ColumnKw)}
		dc.Name = p.nm()
		a.Action = finish(p, dc, as)
	case token.Alter:
		ac := &ast.AlterColumnNotNull{Alter: p.advance(), Column: p.accept(token.ColumnKw)}
		ac.Name = p.nm()
		switch p.cur().kind {
		case token.Drop:
			ac.Drop = p.advance()
			ac.Not = p.expect(token.Not)
			ac.Null = p.expect(token.Null)
		case token.Set:
			ac.Set = p.advance()
			ac.Not = p.expect(token.Not)
			ac.Null = p.expect(token.Null)
			ac.Conflict = p.onConf()
		default:
			p.fail()
		}
		a.Action = finish(p, ac, as)
	default:
		p.fail()
	}
	return finish(p, a, start)
}

// checkWithConflict reads CHECK (expression) and an optional ON CONFLICT.
func (p *parser) checkWithConflict() *ast.CheckConstraint {
	start := p.mark()
	c := &ast.CheckConstraint{Check: p.expect(token.Check), LParen: p.expect(token.LP)}
	c.X = p.expr()
	c.RParen = p.expect(token.RP)
	c.Conflict = p.onConf()
	return finish(p, c, start)
}
