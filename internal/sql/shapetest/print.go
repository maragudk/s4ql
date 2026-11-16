package shapetest

import (
	"fmt"
	"strings"

	"maragu.dev/s4ql/internal/sql/ast"
)

// Print the tree from n as SQL text written from its structure, so that the text shows the shape
// that the tree claims: every operation is in parentheses, and every clause is written by the node
// that holds it, in the place that the grammar gives that node. Names, literals, and keywords are
// written as their tokens were written, so nothing is normalized; the tokens are separated by one
// space, except around the dots of a qualified name. The rules for where parentheses go, and where
// they cannot, are in the README next to this file.
//
// The text never depends on the order of the tokens in the source, only on the fields of the nodes,
// so a tree of the wrong shape prints as a text of that shape.
func Print(n ast.Node) string {
	var p printer
	p.node(n)
	return p.b.String()
}

// printer of a tree.
type printer struct {
	b strings.Builder

	// column, if set, gives the text that a column reference is printed as instead of its tokens.
	column func(*ast.ColumnRef) string

	// printed, if set, is called with every token of the tree that is printed.
	printed func(ast.Token)
}

// word writes s as a token of its own.
func (p *printer) word(s string) {
	if p.b.Len() > 0 {
		p.b.WriteByte(' ')
	}
	p.b.WriteString(s)
}

// tok writes t if it is present.
func (p *printer) tok(t ast.Token) {
	if !t.Present() {
		return
	}
	if p.printed != nil {
		p.printed(t)
	}
	p.word(t.Text)
}

// toks writes each of ts.
func (p *printer) toks(ts ...ast.Token) {
	for _, t := range ts {
		p.tok(t)
	}
}

// dotted writes the tokens of a name with dots, without spaces around the dots.
func (p *printer) dotted(ts ...ast.Token) {
	first := true
	for _, t := range ts {
		if !t.Present() {
			continue
		}
		if p.printed != nil {
			p.printed(t)
		}
		if first {
			p.word(t.Text)
			first = false
		} else {
			p.b.WriteString(t.Text)
		}
	}
}

// qualified writes [schema.]name.
func (p *printer) qualified(q ast.QualifiedName) {
	p.dotted(q.Schema, q.Dot, q.Name)
}

// comma writes a comma, which takes the place of the comma tokens of a list: a list of n items has
// n-1 commas wherever they were written.
func (p *printer) comma(i int) {
	if i > 0 {
		p.word(",")
	}
}

// commaTokens of a list that the printer writes itself, given so that a check of the printed tokens
// can count them as printed.
func (p *printer) commaTokens(ts []ast.Token) {
	if p.printed != nil {
		for _, t := range ts {
			p.printed(t)
		}
	}
}

func (p *printer) exprs(es []ast.Expr, commas []ast.Token) {
	p.commaTokens(commas)
	for i, e := range es {
		p.comma(i)
		p.expr(e)
	}
}

func (p *printer) terms(ts []*ast.OrderingTerm, commas []ast.Token) {
	p.commaTokens(commas)
	for i, t := range ts {
		p.comma(i)
		p.orderingTerm(t)
	}
}

// wrapped reports whether the printer puts e in parentheses of its own: every operation, whose
// grouping the parentheses show. Names, literals, parameters, and the forms that have parentheses
// of their own, such as a function call, CAST, a subquery, and a row value, are not wrapped.
func wrapped(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Unary, *ast.Binary, *ast.Is, *ast.NullTest, *ast.Like, *ast.Between, *ast.InList, *ast.InSelect,
		*ast.InTable, *ast.Collate, *ast.Case:
		return true
	case *ast.Paren:
		return wrapped(e.X) || isParen(e.X)
	default:
		return false
	}
}

func isParen(e ast.Expr) bool {
	_, ok := e.(*ast.Paren)
	return ok
}

// node writes any node.
func (p *printer) node(n ast.Node) {
	switch n := n.(type) {
	case *ast.Statement:
		p.statement(n)
	case ast.Cmd:
		p.cmd(n)
	case ast.Expr:
		p.expr(n)
	case *ast.Select:
		p.selectStmt(n)
	default:
		panic(fmt.Sprintf("shapetest: Print of a node of type %T", n))
	}
}

func (p *printer) statement(s *ast.Statement) {
	p.toks(s.Explain, s.Query, s.Plan)
	p.cmd(s.Cmd)
	// The semicolon ends the statement and is no part of its shape, so it is left out.
	if p.printed != nil && s.Semi.Present() {
		p.printed(s.Semi)
	}
}

func (p *printer) expr(e ast.Expr) {
	if e == nil {
		return
	}
	if wrapped(e) {
		if _, ok := e.(*ast.Paren); !ok {
			p.word("(")
			p.exprBody(e)
			p.word(")")
			return
		}
	}
	p.exprBody(e)
}

// exprBody writes e without the parentheses that [wrapped] gives it.
func (p *printer) exprBody(e ast.Expr) {
	switch e := e.(type) {
	case *ast.Literal:
		p.tok(e.Value)
	case *ast.Variable:
		p.tok(e.Name)
	case *ast.ColumnRef:
		if p.column != nil {
			if s := p.column(e); s != "" {
				p.word(s)
				return
			}
		}
		p.dotted(e.Schema, e.Dot1, e.Table, e.Dot2, e.Column)
	case *ast.Paren:
		// The parentheses are written only around what has none of its own, so that printing the
		// printed text again gives the same text.
		if wrapped(e.X) || isParen(e.X) {
			if p.printed != nil {
				p.printed(e.LParen)
				p.printed(e.RParen)
			}
			p.expr(e.X)
			return
		}
		p.tok(e.LParen)
		p.expr(e.X)
		p.tok(e.RParen)
	case *ast.Row:
		p.tok(e.LParen)
		p.exprs(e.Exprs, e.Commas)
		p.tok(e.RParen)
	case *ast.Subquery:
		p.tok(e.LParen)
		p.selectStmt(e.Select)
		p.tok(e.RParen)
	case *ast.Exists:
		p.toks(e.Exists, e.LParen)
		p.selectStmt(e.Select)
		p.tok(e.RParen)
	case *ast.Unary:
		p.tok(e.Op)
		p.expr(e.X)
	case *ast.Binary:
		p.expr(e.X)
		p.tok(e.Op)
		p.expr(e.Y)
	case *ast.Is:
		p.expr(e.X)
		p.toks(e.Is, e.Not, e.Distinct, e.From)
		p.expr(e.Y)
	case *ast.NullTest:
		p.expr(e.X)
		p.toks(e.Op, e.Null)
	case *ast.Like:
		p.expr(e.X)
		p.toks(e.Not, e.Op)
		p.expr(e.Y)
		if e.Escape.Present() {
			p.tok(e.Escape)
			p.expr(e.EscapeX)
		}
	case *ast.Between:
		p.expr(e.X)
		p.toks(e.Not, e.Between)
		p.expr(e.Low)
		p.tok(e.And)
		p.expr(e.High)
	case *ast.InList:
		p.expr(e.X)
		p.toks(e.Not, e.In, e.LParen)
		p.exprs(e.List, e.Commas)
		p.tok(e.RParen)
	case *ast.InSelect:
		p.expr(e.X)
		p.toks(e.Not, e.In, e.LParen)
		p.selectStmt(e.Select)
		p.tok(e.RParen)
	case *ast.InTable:
		p.expr(e.X)
		p.toks(e.Not, e.In)
		p.qualified(e.Name)
		if e.LParen.Present() {
			p.tok(e.LParen)
			p.exprs(e.Args, e.Commas)
			p.tok(e.RParen)
		}
	case *ast.Collate:
		p.expr(e.X)
		p.toks(e.Collate, e.Name)
	case *ast.Cast:
		p.toks(e.Cast, e.LParen)
		p.expr(e.X)
		p.tok(e.As)
		p.typeName(e.Type)
		p.tok(e.RParen)
	case *ast.FuncCall:
		p.funcCall(e)
	case *ast.Case:
		p.tok(e.Case)
		p.expr(e.Operand)
		for _, w := range e.Whens {
			p.tok(w.When)
			p.expr(w.Condition)
			p.tok(w.Then)
			p.expr(w.Result)
		}
		if e.Else.Present() {
			p.tok(e.Else)
			p.expr(e.ElseX)
		}
		p.tok(e.End)
	case *ast.Raise:
		p.toks(e.Raise, e.LParen, e.Action)
		if e.Comma.Present() {
			p.tok(e.Comma)
			p.expr(e.Message)
		}
		p.tok(e.RParen)
	default:
		panic(fmt.Sprintf("shapetest: Print of an expression of type %T", e))
	}
}

func (p *printer) funcCall(f *ast.FuncCall) {
	p.toks(f.Name, f.LParen, f.Distinct, f.Star)
	p.exprs(f.Args, f.Commas)
	p.orderBy(f.OrderBy)
	p.tok(f.RParen)
	if f.Filter != nil {
		p.toks(f.Filter.Filter, f.Filter.LParen, f.Filter.Where)
		p.expr(f.Filter.X)
		p.tok(f.Filter.RParen)
	}
	if o := f.Over; o != nil {
		p.toks(o.Over, o.Name)
		if o.LParen.Present() {
			p.tok(o.LParen)
			p.window(o.Window)
			p.tok(o.RParen)
		}
	}
}

func (p *printer) typeName(t *ast.TypeName) {
	if t == nil {
		return
	}
	// SQLite keeps the text of a type as written, from its first token to its last, and its length
	// matters: a type of 16 bytes or more that ends in ALWAYS loses that word, and GENERATED before
	// it. So the tokens of a type are written with as many spaces between them as the source has
	// bytes there, which keeps the length of the type.
	var ts []ast.Token
	ts = append(ts, t.Names...)
	ts = append(ts, t.LParen)
	if t.Arg1 != nil {
		ts = append(ts, t.Arg1.Sign, t.Arg1.Number)
	}
	ts = append(ts, t.Comma)
	if t.Arg2 != nil {
		ts = append(ts, t.Arg2.Sign, t.Arg2.Number)
	}
	ts = append(ts, t.RParen)
	var last ast.Token
	for _, tok := range ts {
		if !tok.Present() {
			continue
		}
		if !last.Present() {
			p.tok(tok)
		} else {
			if gap := tok.Offset - last.End(); gap > 0 {
				p.b.WriteString(strings.Repeat(" ", gap))
			} else if gap < 0 {
				p.b.WriteByte(' ')
			}
			p.tight(tok)
		}
		last = tok
	}
}

// tight writes t, if it is present, without a space before it.
func (p *printer) tight(t ast.Token) {
	if !t.Present() {
		return
	}
	if p.printed != nil {
		p.printed(t)
	}
	p.b.WriteString(t.Text)
}

func (p *printer) orderBy(o *ast.OrderBy) {
	if o == nil {
		return
	}
	p.toks(o.Order, o.By)
	p.terms(o.Terms, o.Commas)
}

func (p *printer) orderingTerm(t *ast.OrderingTerm) {
	p.expr(t.X)
	p.toks(t.Order, t.Nulls, t.Which)
}

func (p *printer) selectStmt(s *ast.Select) {
	if s == nil {
		return
	}
	p.with(s.With)
	p.selectCore(s.First)
	for _, c := range s.Compounds {
		p.toks(c.Op, c.All)
		p.selectCore(c.Core)
	}
}

func (p *printer) with(w *ast.With) {
	if w == nil {
		return
	}
	p.toks(w.With, w.Recursive)
	p.commaTokens(w.Commas)
	for i, c := range w.CTEs {
		p.comma(i)
		p.tok(c.Name)
		p.columnNames(c.Columns)
		p.toks(c.As, c.Not, c.Materialized, c.LParen)
		p.selectStmt(c.Select)
		p.tok(c.RParen)
	}
}

func (p *printer) columnNames(c *ast.ColumnNames) {
	if c == nil {
		return
	}
	p.tok(c.LParen)
	p.commaTokens(c.Commas)
	for i, n := range c.Columns {
		p.comma(i)
		p.toks(n.Name, n.Collate, n.Collation, n.Order)
	}
	p.tok(c.RParen)
}

func (p *printer) idList(l *ast.IDList) {
	if l == nil {
		return
	}
	p.commaTokens(l.Commas)
	for i, n := range l.Names {
		p.comma(i)
		p.tok(n)
	}
}

func (p *printer) selectCore(c ast.SelectCore) {
	switch c := c.(type) {
	case *ast.SimpleSelect:
		p.toks(c.Select, c.Distinct)
		p.resultColumns(c.Columns, c.Commas)
		p.from(c.From)
		p.where(c.Where)
		if g := c.GroupBy; g != nil {
			p.toks(g.Group, g.By)
			p.exprs(g.Exprs, g.Commas)
		}
		if h := c.Having; h != nil {
			p.tok(h.Having)
			p.expr(h.X)
		}
		if w := c.Window; w != nil {
			p.tok(w.Window)
			p.commaTokens(w.Commas)
			for i, d := range w.Defs {
				p.comma(i)
				p.toks(d.Name, d.As, d.LParen)
				p.window(d.Window)
				p.tok(d.RParen)
			}
		}
		p.orderBy(c.OrderBy)
		p.limit(c.Limit)
	case *ast.Values:
		p.tok(c.Values)
		p.commaTokens(c.Commas)
		for i, r := range c.Rows {
			p.comma(i)
			p.tok(r.LParen)
			p.exprs(r.Exprs, r.Commas)
			p.tok(r.RParen)
		}
	default:
		panic(fmt.Sprintf("shapetest: Print of a part of a select of type %T", c))
	}
}

func (p *printer) limit(l *ast.Limit) {
	if l == nil {
		return
	}
	// LIMIT a, b is written as it is, and not as LIMIT b OFFSET a, because SQLite numbers the
	// parameters ? in the order of the text.
	p.tok(l.Limit)
	p.expr(l.X)
	p.tok(l.Sep)
	p.expr(l.Y)
}

func (p *printer) where(w *ast.Where) {
	if w != nil {
		p.tok(w.Where)
		p.expr(w.X)
	}
}

func (p *printer) resultColumns(cs []*ast.ResultColumn, commas []ast.Token) {
	p.commaTokens(commas)
	for i, c := range cs {
		p.comma(i)
		switch {
		case c.Star.Present():
			p.dotted(c.Table, c.Dot, c.Star)
		default:
			p.expr(c.Expr)
			p.alias(c.As, c.Alias)
		}
	}
}

// alias writes AS and an alias, with the AS also where it was left out, since SQLite reads an
// alias the same with and without it.
func (p *printer) alias(as, alias ast.Token) {
	if !alias.Present() {
		return
	}
	if as.Present() {
		p.tok(as)
	} else {
		p.word("AS")
	}
	p.tok(alias)
}

func (p *printer) from(f *ast.From) {
	if f == nil {
		return
	}
	p.tok(f.From)
	p.fromItems(f.Items)
}

func (p *printer) fromItems(items []*ast.FromItem) {
	for _, it := range items {
		if j := it.Join; j != nil {
			p.tok(j.Comma)
			p.toks(j.Words...)
			p.tok(j.Join)
		}
		switch s := it.Source.(type) {
		case *ast.TableSource:
			p.qualified(s.Name)
		case *ast.TableFunction:
			p.qualified(s.Name)
			p.tok(s.LParen)
			p.exprs(s.Args, s.Commas)
			p.tok(s.RParen)
		case *ast.SubquerySource:
			p.tok(s.LParen)
			p.selectStmt(s.Select)
			p.tok(s.RParen)
		case *ast.NestedFrom:
			p.tok(s.LParen)
			p.fromItems(s.Items)
			p.tok(s.RParen)
		default:
			panic(fmt.Sprintf("shapetest: Print of a source of type %T", s))
		}
		p.alias(it.As, it.Alias)
		p.indexedBy(it.Indexed)
		switch c := it.Constraint.(type) {
		case nil:
		case *ast.JoinOn:
			p.tok(c.On)
			p.expr(c.X)
		case *ast.JoinUsing:
			p.toks(c.Using, c.LParen)
			p.idList(c.Columns)
			p.tok(c.RParen)
		default:
			panic(fmt.Sprintf("shapetest: Print of a join constraint of type %T", c))
		}
	}
}

func (p *printer) indexedBy(i *ast.IndexedBy) {
	if i != nil {
		p.toks(i.Not, i.Indexed, i.By, i.Index)
	}
}

func (p *printer) window(w *ast.Window) {
	if w == nil {
		return
	}
	p.toks(w.Base, w.Partition, w.PartitionBy)
	p.exprs(w.PartitionExprs, w.PartitionCommas)
	p.orderBy(w.OrderBy)
	if f := w.Frame; f != nil {
		p.toks(f.Unit, f.Between)
		p.frameBound(f.Start)
		if f.Between.Present() {
			p.tok(f.And)
			p.frameBound(f.End)
		}
		p.tok(f.Exclude)
		p.toks(f.Excluded...)
	}
}

func (p *printer) frameBound(b *ast.FrameBound) {
	if b == nil {
		return
	}
	p.toks(b.Unbounded, b.Current, b.Row)
	p.expr(b.X)
	p.tok(b.Direction)
}

func (p *printer) returning(r *ast.Returning) {
	if r != nil {
		p.tok(r.Returning)
		p.resultColumns(r.Columns, r.Commas)
	}
}

func (p *printer) targetTable(t *ast.TargetTable) {
	p.qualified(t.Name)
	if t.Alias.Present() {
		p.toks(t.As, t.Alias)
	}
}

func (p *printer) assignments(as []*ast.Assignment, commas []ast.Token) {
	p.commaTokens(commas)
	for i, a := range as {
		p.comma(i)
		p.tok(a.Column)
		if a.LParen.Present() {
			p.tok(a.LParen)
			p.idList(a.Columns)
			p.tok(a.RParen)
		}
		p.tok(a.Eq)
		p.expr(a.X)
	}
}

func (p *printer) ifNotExists(i ast.IfNotExists) {
	p.toks(i.If, i.Not, i.Exists)
}

func (p *printer) cmd(c ast.Cmd) {
	switch c := c.(type) {
	case *ast.Begin:
		p.toks(c.Begin, c.Mode, c.Transaction, c.Name)
	case *ast.Commit:
		p.toks(c.Commit, c.Transaction, c.Name)
	case *ast.Rollback:
		p.toks(c.Rollback, c.Transaction, c.TransactionName, c.To, c.Savepoint, c.SavepointName)
	case *ast.Savepoint:
		p.toks(c.Savepoint, c.Name)
	case *ast.Release:
		p.toks(c.Release, c.Savepoint, c.Name)
	case *ast.SelectCmd:
		p.selectStmt(c.Select)
	case *ast.Insert:
		p.insert(c)
	case *ast.Update:
		p.with(c.With)
		p.toks(c.Update, c.Or, c.Resolution)
		p.targetTable(c.Table)
		p.indexedBy(c.Indexed)
		p.tok(c.Set)
		p.assignments(c.Assignments, c.Commas)
		p.from(c.From)
		p.where(c.Where)
		p.returning(c.Returning)
	case *ast.Delete:
		p.with(c.With)
		p.toks(c.Delete, c.From)
		p.targetTable(c.Table)
		p.indexedBy(c.Indexed)
		p.where(c.Where)
		p.returning(c.Returning)
	case *ast.Pragma:
		p.tok(c.Pragma)
		p.qualified(c.Name)
		p.toks(c.Eq, c.LParen)
		if c.Value != nil {
			p.toks(c.Value.Sign, c.Value.Value)
		}
		p.tok(c.RParen)
	case *ast.Vacuum:
		p.toks(c.Vacuum, c.Schema, c.Into)
		p.expr(c.File)
	case *ast.Attach:
		p.toks(c.Attach, c.Database)
		p.expr(c.File)
		p.tok(c.As)
		p.expr(c.Name)
		if c.Key.Present() {
			p.tok(c.Key)
			p.expr(c.KeyX)
		}
	case *ast.Detach:
		p.toks(c.Detach, c.Database)
		p.expr(c.Name)
	case *ast.Reindex:
		p.tok(c.Reindex)
		p.qualified(c.Name)
	case *ast.Analyze:
		p.tok(c.Analyze)
		p.qualified(c.Name)
	case *ast.CreateTable:
		p.createTable(c)
	case *ast.Drop:
		p.toks(c.Drop, c.Object)
		p.ifNotExists(c.IfExists)
		p.qualified(c.Name)
	case *ast.CreateView:
		p.toks(c.Create, c.Temp, c.View)
		p.ifNotExists(c.IfNotExists)
		p.qualified(c.Name)
		p.columnNames(c.Columns)
		p.tok(c.As)
		p.selectStmt(c.Select)
	case *ast.CreateIndex:
		p.toks(c.Create, c.Unique, c.Index)
		p.ifNotExists(c.IfNotExists)
		p.qualified(c.Name)
		p.toks(c.On, c.Table, c.LParen)
		p.terms(c.Columns, c.Commas)
		p.tok(c.RParen)
		p.where(c.Where)
	case *ast.CreateTrigger:
		p.createTrigger(c)
	case *ast.CreateVirtualTable:
		p.toks(c.Create, c.Virtual, c.Table)
		p.ifNotExists(c.IfNotExists)
		p.qualified(c.Name)
		p.toks(c.Using, c.Module)
		if c.LParen.Present() {
			p.tok(c.LParen)
			p.commaTokens(c.Commas)
			for i, a := range c.Args {
				p.comma(i)
				// An argument is any tokens: it has no structure to show.
				p.toks(a.Tokens...)
			}
			p.tok(c.RParen)
		}
	case *ast.AlterTable:
		p.toks(c.Alter, c.Table)
		p.qualified(c.Name)
		p.alterAction(c.Action)
	default:
		panic(fmt.Sprintf("shapetest: Print of a command of type %T", c))
	}
}

func (p *printer) insert(c *ast.Insert) {
	p.with(c.With)
	p.toks(c.Insert, c.Or, c.Resolution, c.Into)
	p.targetTable(c.Table)
	if c.LParen.Present() {
		p.tok(c.LParen)
		p.idList(c.Columns)
		p.tok(c.RParen)
	}
	p.selectStmt(c.Select)
	p.toks(c.Default, c.Values)
	for _, u := range c.Upserts {
		p.toks(u.On, u.Conflict)
		if u.LParen.Present() {
			p.tok(u.LParen)
			p.terms(u.Target, u.TargetCommas)
			p.tok(u.RParen)
			p.where(u.TargetWhere)
		}
		p.toks(u.Do, u.Nothing, u.Update, u.Set)
		p.assignments(u.Assignments, u.Commas)
		p.where(u.Where)
	}
	p.returning(c.Returning)
}

func (p *printer) createTrigger(c *ast.CreateTrigger) {
	p.toks(c.Create, c.Temp, c.Trigger)
	p.ifNotExists(c.IfNotExists)
	p.qualified(c.Name)
	p.toks(c.Time, c.TimeOf, c.Event, c.EventOf)
	p.idList(c.Columns)
	p.tok(c.On)
	p.qualified(c.Table)
	p.toks(c.For, c.Each, c.Row)
	if c.When.Present() {
		p.tok(c.When)
		p.expr(c.WhenX)
	}
	p.tok(c.Begin)
	p.commaTokens(c.Semis)
	for _, s := range c.Steps {
		p.cmd(s)
		p.word(";")
	}
	p.tok(c.End)
}

func (p *printer) createTable(c *ast.CreateTable) {
	p.toks(c.Create, c.Temp, c.Table)
	p.ifNotExists(c.IfNotExists)
	p.qualified(c.Name)
	if c.As.Present() {
		p.tok(c.As)
		p.selectStmt(c.Select)
		return
	}
	p.tok(c.LParen)
	// Between two table constraints the comma can be left out, and a comma there means something:
	// it ends the CONSTRAINT name before it. So a comma is written between two items exactly where
	// one was written.
	type item struct {
		start int
		print func()
	}
	var items []item
	for _, d := range c.Columns {
		items = append(items, item{d.Span.Start, func() { p.columnDef(d) }})
	}
	for _, t := range c.Constraints {
		items = append(items, item{t.Bounds().Start, func() { p.tableConstraint(t) }})
	}
	commas := c.Commas
	p.commaTokens(commas)
	for i, it := range items {
		if i > 0 {
			if len(commas) > 0 && commas[0].Offset < it.start {
				p.word(",")
				commas = commas[1:]
			}
		}
		it.print()
	}
	p.tok(c.RParen)
	p.commaTokens(c.OptionCommas)
	if len(c.OptionCommas) == len(c.Options) && len(c.Options) > 0 {
		// The first option is empty.
		p.word(",")
	}
	for i, o := range c.Options {
		p.comma(i)
		p.toks(o.Without, o.Name)
	}
}

func (p *printer) columnDef(d *ast.ColumnDef) {
	p.tok(d.Name)
	p.typeName(d.Type)
	for _, c := range d.Constraints {
		p.columnConstraint(c)
	}
}

func (p *printer) onConflict(o *ast.OnConflict) {
	if o != nil {
		p.toks(o.On, o.Conflict, o.Resolution)
	}
}

func (p *printer) columnConstraint(c ast.ColumnConstraint) {
	switch c := c.(type) {
	case *ast.ConstraintName:
		p.toks(c.Constraint, c.Name)
	case *ast.DefaultConstraint:
		p.toks(c.Default, c.Sign, c.LParen)
		p.expr(c.X)
		p.toks(c.RParen, c.Name)
	case *ast.NullConstraint:
		p.toks(c.Not, c.Null)
		p.onConflict(c.Conflict)
	case *ast.PrimaryKeyConstraint:
		p.toks(c.Primary, c.Key, c.Order)
		p.onConflict(c.Conflict)
		p.tok(c.Autoincrement)
	case *ast.UniqueConstraint:
		p.tok(c.Unique)
		p.onConflict(c.Conflict)
	case *ast.CheckConstraint:
		p.check(c)
	case *ast.ReferencesConstraint:
		p.references(c.References)
	case *ast.DeferConstraint:
		p.deferrable(c.Defer)
	case *ast.CollateConstraint:
		p.toks(c.Collate, c.Name)
	case *ast.GeneratedConstraint:
		p.toks(c.Generated, c.Always, c.As, c.LParen)
		p.expr(c.X)
		p.toks(c.RParen, c.Storage)
	default:
		panic(fmt.Sprintf("shapetest: Print of a column constraint of type %T", c))
	}
}

func (p *printer) check(c *ast.CheckConstraint) {
	p.toks(c.Check, c.LParen)
	p.expr(c.X)
	p.tok(c.RParen)
	p.onConflict(c.Conflict)
}

func (p *printer) references(r *ast.References) {
	p.toks(r.References, r.Table)
	p.columnNames(r.Columns)
	for _, a := range r.Args {
		p.toks(a.Match, a.Name, a.On, a.Event)
		p.toks(a.Action...)
	}
}

func (p *printer) deferrable(d *ast.Deferrable) {
	if d != nil {
		p.toks(d.Not, d.Deferrable, d.Initially, d.Mode)
	}
}

func (p *printer) tableConstraint(c ast.TableConstraint) {
	switch c := c.(type) {
	case *ast.ConstraintName:
		p.toks(c.Constraint, c.Name)
	case *ast.CheckConstraint:
		p.check(c)
	case *ast.TablePrimaryKey:
		p.toks(c.Primary, c.Key, c.LParen)
		p.terms(c.Columns, c.Commas)
		p.toks(c.Autoincrement, c.RParen)
		p.onConflict(c.Conflict)
	case *ast.TableUnique:
		p.toks(c.Unique, c.LParen)
		p.terms(c.Columns, c.Commas)
		p.tok(c.RParen)
		p.onConflict(c.Conflict)
	case *ast.TableForeignKey:
		p.toks(c.Foreign, c.Key)
		p.columnNames(c.Columns)
		p.references(c.References)
		p.deferrable(c.Defer)
	default:
		panic(fmt.Sprintf("shapetest: Print of a table constraint of type %T", c))
	}
}

func (p *printer) alterAction(a ast.AlterAction) {
	switch a := a.(type) {
	case *ast.RenameTable:
		p.toks(a.Rename, a.To, a.Name)
	case *ast.AddColumn:
		p.toks(a.Add, a.Column)
		p.columnDef(a.Def)
	case *ast.DropColumn:
		p.toks(a.Drop, a.Column, a.Name)
	case *ast.RenameColumn:
		p.toks(a.Rename, a.Column, a.Old, a.To, a.New)
	case *ast.DropConstraint:
		p.toks(a.Drop, a.Constraint, a.Name)
	case *ast.AlterColumnNotNull:
		p.toks(a.Alter, a.Column, a.Name, a.Drop, a.Set, a.Not, a.Null)
		p.onConflict(a.Conflict)
	case *ast.AddCheck:
		p.toks(a.Add, a.Constraint, a.Name)
		p.check(a.Check)
	default:
		panic(fmt.Sprintf("shapetest: Print of an alter action of type %T", a))
	}
}
