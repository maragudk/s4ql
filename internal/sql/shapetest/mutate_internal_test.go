package shapetest

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/corpus"
	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/token"
)

// mutation of the shape of a tree: a kind, and a function that gives the sites in a tree where the
// mutation applies. Each site is a function that applies the mutation there and names what it did.
type mutation struct {
	kind  string
	sites func(stmt *ast.Statement) []func() string
	// same, if not empty, says why SQLite treats the two trees the same, so that the check cannot
	// notice the mutation.
	same string
}

var exprType = reflect.TypeFor[ast.Expr]()

// exprSlots of the tree: a pointer to every field or element of a slice that holds an expression,
// in the order of a walk.
func exprSlots(root ast.Node) []*ast.Expr {
	var slots []*ast.Expr
	ast.Walk(root, func(n ast.Node) bool {
		v := reflect.ValueOf(n).Elem()
		for i := range v.NumField() {
			f := v.Field(i)
			switch {
			case f.Type() == exprType && !f.IsNil():
				slots = append(slots, f.Addr().Interface().(*ast.Expr))
			case f.Kind() == reflect.Slice && f.Type().Elem() == exprType:
				for j := range f.Len() {
					slots = append(slots, f.Index(j).Addr().Interface().(*ast.Expr))
				}
			}
		}
		return true
	})
	return slots
}

// operands of an operator, in the order of the text, and whether the operator starts with its
// first operand and ends with its last.
func operands(e ast.Expr) (ops []*ast.Expr, leads, trails bool) {
	switch e := e.(type) {
	case *ast.Unary:
		return []*ast.Expr{&e.X}, false, true
	case *ast.Binary:
		return []*ast.Expr{&e.X, &e.Y}, true, true
	case *ast.Is:
		return []*ast.Expr{&e.X, &e.Y}, true, true
	case *ast.Like:
		if e.EscapeX != nil {
			return []*ast.Expr{&e.X, &e.Y, &e.EscapeX}, true, true
		}
		return []*ast.Expr{&e.X, &e.Y}, true, true
	case *ast.Between:
		return []*ast.Expr{&e.X, &e.Low, &e.High}, true, true
	case *ast.NullTest:
		return []*ast.Expr{&e.X}, true, false
	case *ast.Collate:
		return []*ast.Expr{&e.X}, true, false
	case *ast.InList:
		return []*ast.Expr{&e.X}, true, false
	case *ast.InSelect:
		return []*ast.Expr{&e.X}, true, false
	case *ast.InTable:
		return []*ast.Expr{&e.X}, true, false
	}
	return nil, false, false
}

// rotate gives the sites where an operator P and an operator C right after or before it in the text
// can be grouped the other way, with the tokens in the same order: P(C(..., d), ...) becomes
// C(..., P(d, ...)), and P(..., C(d, ...)) becomes C(P(..., d), ...).
func rotate(stmt *ast.Statement) []func() string {
	var sites []func() string
	for _, slot := range exprSlots(stmt) {
		p := *slot
		pops, pleads, ptrails := operands(p)
		if len(pops) == 0 {
			continue
		}
		if c := *pops[0]; pleads {
			if cops, _, ctrails := operands(c); len(cops) > 0 && ctrails {
				sites = append(sites, func() string {
					last := cops[len(cops)-1]
					*pops[0] = *last
					*last = p
					*slot = c
					return form(p) + " under " + form(c)
				})
			}
		}
		if c := *pops[len(pops)-1]; ptrails {
			if cops, cleads, _ := operands(c); len(cops) > 0 && cleads {
				sites = append(sites, func() string {
					first := cops[0]
					*pops[len(pops)-1] = *first
					*first = p
					*slot = c
					return form(p) + " under " + form(c)
				})
			}
		}
	}
	return sites
}

// swapOperands of a binary operator.
func swapOperands(stmt *ast.Statement) []func() string {
	var sites []func() string
	ast.Walk(stmt, func(n ast.Node) bool {
		var x, y *ast.Expr
		switch b := n.(type) {
		case *ast.Binary:
			x, y = &b.X, &b.Y
		case *ast.Is:
			x, y = &b.X, &b.Y
		case *ast.Like:
			x, y = &b.X, &b.Y
		default:
			return true
		}
		e := n.(ast.Expr)
		sites = append(sites, func() string {
			*x, *y = *y, *x
			if Print(*x) == Print(*y) {
				return form(e) + ", with equal operands"
			}
			return form(e)
		})
		return true
	})
	return sites
}

// notOfIs moves the NOT of IS NOT to its right operand: a IS NOT b becomes a IS (NOT b).
func notOfIs(stmt *ast.Statement) []func() string {
	var sites []func() string
	ast.Walk(stmt, func(n ast.Node) bool {
		if is, ok := n.(*ast.Is); ok && is.Not.Present() && !is.Distinct.Present() {
			sites = append(sites, func() string {
				is.Y = &ast.Unary{Op: is.Not, X: is.Y}
				is.Not = ast.Token{}
				return "IS NOT"
			})
		}
		return true
	})
	return sites
}

// notOutward moves the NOT of NOT BETWEEN, NOT LIKE, NOT IN, and NOT NULL over the whole operation:
// x NOT BETWEEN a AND b becomes NOT (x BETWEEN a AND b).
func notOutward(stmt *ast.Statement) []func() string {
	var sites []func() string
	for _, slot := range exprSlots(stmt) {
		var not *ast.Token
		switch e := (*slot).(type) {
		case *ast.Between:
			not = &e.Not
		case *ast.Like:
			not = &e.Not
		case *ast.InList:
			not = &e.Not
		case *ast.InSelect:
			not = &e.Not
		case *ast.InTable:
			not = &e.Not
		}
		if not == nil || !not.Present() {
			continue
		}
		sites = append(sites, func() string {
			e := *slot
			f := form(e)
			tok := *not
			*not = ast.Token{}
			*slot = &ast.Unary{Op: tok, X: e}
			return f
		})
	}
	return sites
}

// swapLimit swaps the operands of LIMIT a, b and of LIMIT a OFFSET b.
func swapLimit(stmt *ast.Statement) []func() string {
	var sites []func() string
	ast.Walk(stmt, func(n ast.Node) bool {
		if l, ok := n.(*ast.Limit); ok && l.Y != nil {
			sites = append(sites, func() string {
				l.X, l.Y = l.Y, l.X
				return "LIMIT " + strings.ToUpper(l.Sep.Text)
			})
		}
		return true
	})
	return sites
}

// fromLists of the tree: the items of every FROM clause and every join in parentheses.
func fromLists(stmt *ast.Statement) [][]*ast.FromItem {
	var lists [][]*ast.FromItem
	ast.Walk(stmt, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.From:
			lists = append(lists, n.Items)
		case *ast.NestedFrom:
			lists = append(lists, n.Items)
		}
		return true
	})
	return lists
}

// moveOn moves the ON or USING of a join to the join before or after it, if that one has none.
func moveOn(stmt *ast.Statement) []func() string {
	var sites []func() string
	for _, items := range fromLists(stmt) {
		for i, it := range items {
			if it.Constraint == nil {
				continue
			}
			for _, j := range []int{i - 1, i + 1} {
				if j < 1 || j >= len(items) || items[j].Constraint != nil {
					continue
				}
				other := items[j]
				sites = append(sites, func() string {
					other.Constraint, it.Constraint = it.Constraint, nil
					return fmt.Sprintf("%T to the join %+d", other.Constraint, j-i)
				})
			}
		}
	}
	return sites
}

// moveJoinOp swaps the join operators of two neighbouring items, such as LEFT JOIN and JOIN.
func moveJoinOp(stmt *ast.Statement) []func() string {
	var sites []func() string
	for _, items := range fromLists(stmt) {
		for i := 1; i+1 < len(items); i++ {
			a, b := items[i], items[i+1]
			if joinText(a.Join) == joinText(b.Join) {
				continue
			}
			sites = append(sites, func() string {
				a.Join, b.Join = b.Join, a.Join
				return "join operator"
			})
		}
	}
	return sites
}

func joinText(j *ast.JoinOp) string {
	if j == nil {
		return ""
	}
	var words []string
	for _, w := range append(append([]ast.Token{j.Comma}, j.Words...), j.Join) {
		words = append(words, strings.ToUpper(w.Text))
	}
	return strings.Join(words, " ")
}

// cores of every SELECT of the tree, with the SELECT.
func selects(stmt *ast.Statement) []*ast.Select {
	var ss []*ast.Select
	ast.Walk(stmt, func(n ast.Node) bool {
		if s, ok := n.(*ast.Select); ok {
			ss = append(ss, s)
		}
		return true
	})
	return ss
}

func lastCore(s *ast.Select) *ast.SimpleSelect {
	c := s.First
	if len(s.Compounds) > 0 {
		c = s.Compounds[len(s.Compounds)-1].Core
	}
	ss, _ := c.(*ast.SimpleSelect)
	return ss
}

// moveOrderBy moves the ORDER BY or LIMIT of a compound SELECT, which its last part holds, to its
// first part, and the ORDER BY or LIMIT of a SELECT that reads from one subquery into the
// subquery, or out of it.
func moveOrderBy(stmt *ast.Statement) []func() string {
	var sites []func() string
	for _, s := range selects(stmt) {
		last := lastCore(s)
		if last == nil {
			continue
		}
		if first, ok := s.First.(*ast.SimpleSelect); ok && len(s.Compounds) > 0 {
			if last.OrderBy != nil && first.OrderBy == nil {
				sites = append(sites, func() string {
					first.OrderBy, last.OrderBy = last.OrderBy, nil
					return "ORDER BY of a compound to its first part"
				})
			}
			if last.Limit != nil && first.Limit == nil {
				sites = append(sites, func() string {
					first.Limit, last.Limit = last.Limit, nil
					return "LIMIT of a compound to its first part"
				})
			}
		}
		if len(s.Compounds) > 0 || last.From == nil || len(last.From.Items) != 1 {
			continue
		}
		sub, ok := last.From.Items[0].Source.(*ast.SubquerySource)
		if !ok {
			continue
		}
		inner := lastCore(sub.Select)
		if inner == nil || len(sub.Select.Compounds) > 0 {
			continue
		}
		if last.OrderBy != nil && inner.OrderBy == nil && inner.Limit == nil {
			sites = append(sites, func() string {
				inner.OrderBy, last.OrderBy = last.OrderBy, nil
				return "ORDER BY into a subquery"
			})
		}
		if inner.OrderBy != nil && last.OrderBy == nil && last.Limit == nil {
			sites = append(sites, func() string {
				last.OrderBy, inner.OrderBy = inner.OrderBy, nil
				return "ORDER BY out of a subquery"
			})
		}
		if last.Limit != nil && inner.Limit == nil {
			sites = append(sites, func() string {
				inner.Limit, last.Limit = last.Limit, nil
				return "LIMIT into a subquery"
			})
		}
	}
	return sites
}

// moveElse moves the ELSE of a CASE to a CASE without ELSE in its last result, and back: the
// dangling ELSE.
func moveElse(stmt *ast.Statement) []func() string {
	var sites []func() string
	ast.Walk(stmt, func(n ast.Node) bool {
		outer, ok := n.(*ast.Case)
		if !ok || len(outer.Whens) == 0 {
			return true
		}
		inner, ok := outer.Whens[len(outer.Whens)-1].Result.(*ast.Case)
		if !ok {
			return true
		}
		switch {
		case outer.Else.Present() && !inner.Else.Present():
			sites = append(sites, func() string {
				inner.Else, inner.ElseX, outer.Else, outer.ElseX = outer.Else, outer.ElseX, ast.Token{}, nil
				return "ELSE into the inner CASE"
			})
		case inner.Else.Present() && !outer.Else.Present():
			sites = append(sites, func() string {
				outer.Else, outer.ElseX, inner.Else, inner.ElseX = inner.Else, inner.ElseX, ast.Token{}, nil
				return "ELSE out of the inner CASE"
			})
		}
		return true
	})
	return sites
}

// moveFilterOver moves the FILTER or OVER of a function call to a call in its arguments.
func moveFilterOver(stmt *ast.Statement) []func() string {
	var sites []func() string
	ast.Walk(stmt, func(n ast.Node) bool {
		outer, ok := n.(*ast.FuncCall)
		if !ok || (outer.Filter == nil && outer.Over == nil) {
			return true
		}
		for _, a := range outer.Args {
			inner, ok := a.(*ast.FuncCall)
			if !ok || inner.Filter != nil || inner.Over != nil {
				continue
			}
			sites = append(sites, func() string {
				inner.Filter, inner.Over, outer.Filter, outer.Over = outer.Filter, outer.Over, nil, nil
				return "FILTER or OVER to an argument"
			})
		}
		return true
	})
	return sites
}

// moveWindowOrder moves the ORDER BY in the arguments of a function into its window, and back.
func moveWindowOrder(stmt *ast.Statement) []func() string {
	var sites []func() string
	ast.Walk(stmt, func(n ast.Node) bool {
		f, ok := n.(*ast.FuncCall)
		if !ok || f.Over == nil || f.Over.Window == nil {
			return true
		}
		w := f.Over.Window
		switch {
		case f.OrderBy != nil && w.OrderBy == nil:
			sites = append(sites, func() string {
				w.OrderBy, f.OrderBy = f.OrderBy, nil
				return "ORDER BY of the arguments into the window"
			})
		case w.OrderBy != nil && f.OrderBy == nil && len(f.Args) > 0:
			sites = append(sites, func() string {
				f.OrderBy, w.OrderBy = w.OrderBy, nil
				return "ORDER BY of the window into the arguments"
			})
		}
		return true
	})
	return sites
}

// swapUpsertWhere swaps the WHERE of the conflict target of an upsert with that of its DO UPDATE.
func swapUpsertWhere(stmt *ast.Statement) []func() string {
	var sites []func() string
	ast.Walk(stmt, func(n ast.Node) bool {
		if u, ok := n.(*ast.Upsert); ok && u.LParen.Present() && u.Update.Present() && (u.Where != nil || u.TargetWhere != nil) {
			sites = append(sites, func() string {
				u.Where, u.TargetWhere = u.TargetWhere, u.Where
				return "WHERE of an upsert"
			})
		}
		return true
	})
	return sites
}

// moveColumnConstraint moves a constraint of a column to the next column.
func moveColumnConstraint(stmt *ast.Statement) []func() string {
	var sites []func() string
	ast.Walk(stmt, func(n ast.Node) bool {
		ct, ok := n.(*ast.CreateTable)
		if !ok {
			return true
		}
		for i := 0; i+1 < len(ct.Columns); i++ {
			a, b := ct.Columns[i], ct.Columns[i+1]
			for j, c := range a.Constraints {
				if _, ok := c.(*ast.ConstraintName); ok {
					continue
				}
				sites = append(sites, func() string {
					a.Constraints = slices.Delete(slices.Clone(a.Constraints), j, j+1)
					b.Constraints = append([]ast.ColumnConstraint{c}, b.Constraints...)
					return fmt.Sprintf("%T to the next column", c)
				})
			}
		}
		return true
	})
	return sites
}

// moveConstraintName moves CONSTRAINT name in a table's constraints after the constraint it names.
func moveConstraintName(stmt *ast.Statement) []func() string {
	var sites []func() string
	ast.Walk(stmt, func(n ast.Node) bool {
		ct, ok := n.(*ast.CreateTable)
		if !ok {
			return true
		}
		for i := 0; i+1 < len(ct.Constraints); i++ {
			if _, ok := ct.Constraints[i].(*ast.ConstraintName); !ok {
				continue
			}
			sites = append(sites, func() string {
				cs := ct.Constraints
				cs[i], cs[i+1] = cs[i+1], cs[i]
				return "CONSTRAINT name after its constraint"
			})
		}
		return true
	})
	return sites
}

// columnDefs of the tree: the columns of CREATE TABLE and of ALTER TABLE ... ADD COLUMN.
func columnDefs(stmt *ast.Statement) []*ast.ColumnDef {
	var defs []*ast.ColumnDef
	ast.Walk(stmt, func(n ast.Node) bool {
		if d, ok := n.(*ast.ColumnDef); ok {
			defs = append(defs, d)
		}
		return true
	})
	return defs
}

// firstGenerated gives the GENERATED ALWAYS AS of a column, if it is the first constraint and is
// written without GENERATED ALWAYS.
func firstGenerated(d *ast.ColumnDef) *ast.GeneratedConstraint {
	if len(d.Constraints) == 0 {
		return nil
	}
	g, ok := d.Constraints[0].(*ast.GeneratedConstraint)
	if !ok || g.Generated.Present() || g.Always.Present() {
		return nil
	}
	return g
}

// typeEndsInGeneratedAlways reports whether the words of a type end in GENERATED ALWAYS.
func typeEndsInGeneratedAlways(t *ast.TypeName) bool {
	if t == nil {
		return false
	}
	n := len(t.Names)
	return !t.LParen.Present() && n >= 2 && strings.EqualFold(t.Names[n-2].Text, "generated") && strings.EqualFold(t.Names[n-1].Text, "always")
}

// generatedAlwaysToAs moves GENERATED ALWAYS at the end of a type into the AS after it, and GENERATED
// ALWAYS of AS into the type, with the tokens in the same order: a int generated always as (1) has
// the type int generated always, and a parser could read the words as part of AS.
func generatedAlwaysToAs(stmt *ast.Statement) []func() string {
	var sites []func() string
	for _, d := range columnDefs(stmt) {
		if g := firstGenerated(d); g != nil && d.Type != nil && typeEndsInGeneratedAlways(d.Type) {
			sites = append(sites, func() string {
				n := len(d.Type.Names)
				g.Generated, g.Always = d.Type.Names[n-2], d.Type.Names[n-1]
				d.Type.Names = d.Type.Names[:n-2]
				if len(d.Type.Names) == 0 {
					d.Type = nil
				}
				return "GENERATED ALWAYS of the type into AS"
			})
		}
		if len(d.Constraints) > 0 {
			if g, ok := d.Constraints[0].(*ast.GeneratedConstraint); ok && g.Generated.Present() && (d.Type == nil || !d.Type.LParen.Present()) {
				sites = append(sites, func() string {
					if d.Type == nil {
						d.Type = &ast.TypeName{}
					}
					d.Type.Names = append(d.Type.Names, g.Generated, g.Always)
					g.Generated, g.Always = ast.Token{}, ast.Token{}
					return "GENERATED ALWAYS of AS into the type"
				})
			}
		}
	}
	return sites
}

// typeWordToAs moves the last word of a type, which is not ALWAYS, into the AS after it as its
// ALWAYS: a int always as (1) has the type int always.
func typeWordToAs(stmt *ast.Statement) []func() string {
	var sites []func() string
	for _, d := range columnDefs(stmt) {
		g := firstGenerated(d)
		if g == nil || d.Type == nil || d.Type.LParen.Present() || typeEndsInGeneratedAlways(d.Type) {
			continue
		}
		sites = append(sites, func() string {
			n := len(d.Type.Names)
			g.Always = d.Type.Names[n-1]
			d.Type.Names = d.Type.Names[:n-1]
			if n == 1 {
				d.Type = nil
			}
			return "the last word of the type into AS"
		})
	}
	return sites
}

// constraintToType moves the tokens of the first constraint of a column into its type, as words of
// the type, with the tokens in the same order: a parser that took a constraint for words of the
// type.
func constraintToType(stmt *ast.Statement) []func() string {
	var sites []func() string
	for _, d := range columnDefs(stmt) {
		if len(d.Constraints) == 0 || (d.Type != nil && d.Type.LParen.Present()) {
			continue
		}
		sites = append(sites, func() string {
			c := d.Constraints[0]
			if d.Type == nil {
				d.Type = &ast.TypeName{}
			}
			d.Type.Names = append(d.Type.Names, ast.AllTokens(c)...)
			d.Constraints = d.Constraints[1:]
			return fmt.Sprintf("%T into the type", c)
		})
	}
	return sites
}

// moveColumnConstraintName moves CONSTRAINT name in a column's constraints after the constraint it
// names.
func moveColumnConstraintName(stmt *ast.Statement) []func() string {
	var sites []func() string
	for _, d := range columnDefs(stmt) {
		for i := 0; i+1 < len(d.Constraints); i++ {
			if _, ok := d.Constraints[i].(*ast.ConstraintName); !ok {
				continue
			}
			sites = append(sites, func() string {
				cs := d.Constraints
				cs[i], cs[i+1] = cs[i+1], cs[i]
				return fmt.Sprintf("CONSTRAINT name after %T", cs[i])
			})
		}
	}
	return sites
}

// conflictOf gives the ON CONFLICT of a constraint that can have one.
func conflictOf(n ast.Node) (**ast.OnConflict, bool) {
	switch c := n.(type) {
	case *ast.NullConstraint:
		return &c.Conflict, true
	case *ast.PrimaryKeyConstraint:
		return &c.Conflict, true
	case *ast.UniqueConstraint:
		return &c.Conflict, true
	case *ast.CheckConstraint:
		return &c.Conflict, true
	case *ast.TablePrimaryKey:
		return &c.Conflict, true
	case *ast.TableUnique:
		return &c.Conflict, true
	}
	return nil, false
}

// moveConflict moves the ON CONFLICT of a constraint to the next constraint of the same column, or
// of the table, that can have one and has none.
func moveConflict(stmt *ast.Statement) []func() string {
	var sites []func() string
	try := func(nodes []ast.Node, column bool) {
		for i := 0; i+1 < len(nodes); i++ {
			from, ok := conflictOf(nodes[i])
			if !ok || *from == nil {
				continue
			}
			to, ok := conflictOf(nodes[i+1])
			if !ok || *to != nil {
				continue
			}
			if _, isCheck := nodes[i+1].(*ast.CheckConstraint); isCheck && column {
				// A column's CHECK has no ON CONFLICT in the grammar, so SQLite would reject the
				// printed text.
				continue
			}
			a, b := nodes[i], nodes[i+1]
			sites = append(sites, func() string {
				*to, *from = *from, nil
				return fmt.Sprintf("ON CONFLICT of %T to %T", a, b)
			})
		}
	}
	for _, d := range columnDefs(stmt) {
		var nodes []ast.Node
		for _, c := range d.Constraints {
			nodes = append(nodes, c)
		}
		try(nodes, true)
	}
	ast.Walk(stmt, func(n ast.Node) bool {
		if ct, ok := n.(*ast.CreateTable); ok {
			var nodes []ast.Node
			for _, c := range ct.Constraints {
				nodes = append(nodes, c)
			}
			try(nodes, false)
		}
		return true
	})
	return sites
}

// swapActions swaps the events of two actions of a foreign key, so that the action of ON DELETE
// becomes that of ON UPDATE and back.
func swapActions(stmt *ast.Statement) []func() string {
	var sites []func() string
	ast.Walk(stmt, func(n ast.Node) bool {
		r, ok := n.(*ast.References)
		if !ok {
			return true
		}
		for i, a := range r.Args {
			for _, b := range r.Args[i+1:] {
				if a.On.Present() && b.On.Present() && !strings.EqualFold(a.Event.Text, b.Event.Text) && joinTokens(a.Action) != joinTokens(b.Action) {
					sites = append(sites, func() string {
						a.Event, b.Event = b.Event, a.Event
						return "ON " + strings.ToUpper(a.Event.Text) + " and ON " + strings.ToUpper(b.Event.Text)
					})
				}
			}
		}
		return true
	})
	return sites
}

func joinTokens(ts []ast.Token) string {
	var words []string
	for _, t := range ts {
		words = append(words, strings.ToUpper(t.Text))
	}
	return strings.Join(words, " ")
}

// defaultForms changes the form of a DEFAULT, with the tokens in the same order: a name after
// DEFAULT, which SQLite reads as a string, becomes a column reference, and the sign of a signed
// literal becomes a unary operator on it, which only DEFAULT ( expr ) can hold.
func defaultForms(stmt *ast.Statement) []func() string {
	var sites []func() string
	ast.Walk(stmt, func(n ast.Node) bool {
		d, ok := n.(*ast.DefaultConstraint)
		if !ok {
			return true
		}
		switch {
		case d.Name.Present():
			sites = append(sites, func() string {
				d.X, d.Name = &ast.ColumnRef{Column: d.Name}, ast.Token{}
				return "the name of DEFAULT as a column"
			})
		case d.Sign.Present():
			sites = append(sites, func() string {
				d.X, d.Sign = &ast.Unary{Op: d.Sign, X: d.X}, ast.Token{}
				return "the sign of DEFAULT as an operator"
			})
		}
		return true
	})
	return sites
}

// splitNumber splits a number with digit separators in a result column into a number and an alias,
// as a parser that does not know digit separators would read it: 1_000 becomes 1 AS _000.
func splitNumber(stmt *ast.Statement) []func() string {
	var sites []func() string
	ast.Walk(stmt, func(n ast.Node) bool {
		rc, ok := n.(*ast.ResultColumn)
		if !ok || rc.Alias.Present() {
			return true
		}
		lit, ok := rc.Expr.(*ast.Literal)
		if !ok || lit.Value.Kind != token.QNumber {
			return true
		}
		i := strings.IndexByte(lit.Value.Text, '_')
		if i <= 0 {
			return true
		}
		sites = append(sites, func() string {
			v := lit.Value
			rc.Alias = ast.Token{Kind: token.ID, Offset: v.Offset + i, Text: v.Text[i:]}
			lit.Value = ast.Token{Kind: token.Integer, Offset: v.Offset, Text: v.Text[:i]}
			return "number with digit separators"
		})
		return true
	})
	return sites
}

var mutations = []mutation{
	{"rotate two operators", rotate, ""},
	{"swap the operands", swapOperands, ""},
	{"move the NOT of IS NOT to its operand", notOfIs, ""},
	{"move the NOT of NOT BETWEEN, LIKE, IN out", notOutward, "SQLite reads x NOT BETWEEN a AND b as NOT (x BETWEEN a AND b), and NOT LIKE, NOT GLOB, NOT REGEXP, NOT MATCH, and NOT IN the same way"},
	{"swap the operands of LIMIT", swapLimit, ""},
	{"move ON or USING to the next join", moveOn, ""},
	{"swap two join operators", moveJoinOp, ""},
	{"move ORDER BY or LIMIT", moveOrderBy, ""},
	{"move the ELSE of a nested CASE", moveElse, ""},
	{"move FILTER or OVER to an argument", moveFilterOver, ""},
	{"move ORDER BY between arguments and window", moveWindowOrder, ""},
	{"swap the WHEREs of an upsert", swapUpsertWhere, ""},
	{"move a column constraint", moveColumnConstraint, ""},
	{"move CONSTRAINT name", moveConstraintName, ""},
	{"move a column's CONSTRAINT name", moveColumnConstraintName, ""},
	{"split a number with digit separators", splitNumber, ""},
	{"move GENERATED ALWAYS between type and AS", generatedAlwaysToAs, "sqlite3AddColumn drops GENERATED ALWAYS from the end of a type of 16 bytes or more, which a type that ends in these words always is"},
	{"move the last word of a type into AS", typeWordToAs, ""},
	{"move a column's first constraint into its type", constraintToType, ""},
	{"move ON CONFLICT to the next constraint", moveConflict, ""},
	{"swap ON DELETE and ON UPDATE", swapActions, ""},
	{"change the form of DEFAULT", defaultForms, ""},
}

// declarationKinds of mutation, which damage what a CREATE TABLE or an ALTER TABLE declares.
var declarationKinds = []string{
	"move a column constraint", "move CONSTRAINT name", "move a column's CONSTRAINT name",
	"move GENERATED ALWAYS between type and AS", "move the last word of a type into AS",
	"move a column's first constraint into its type", "move ON CONFLICT to the next constraint",
	"swap ON DELETE and ON UPDATE", "change the form of DEFAULT",
}

// outcome of a mutation: detected, not detected although compared, or not compared.
type outcome struct {
	detected, undetected, notCompared int
	// examples of undetected mutations, by what was done.
	examples map[string][]string
	// byWhat counts the detected, the undetected, and those not compared, by what was done.
	byWhat map[string]*[3]int
	// byDeclared counts the mutations that only the comparison of what the tree declares detected.
	byDeclared int
}

// mutate applies every mutation at up to perKind sites of each statement of sqls that parses, and
// gives the outcome by kind.
func mutate(t *testing.T, outcomes map[string]*outcome, sqls []string, perKind int, ms []mutation) {
	t.Helper()
	for _, m := range ms {
		if outcomes[m.kind] == nil {
			outcomes[m.kind] = &outcome{examples: map[string][]string{}, byWhat: map[string]*[3]int{}}
		}
	}
	var c Checker
	defer c.Close()
	for _, src := range sqls {
		orig, _, err := parser.Parse(src)
		if err != nil || orig == nil {
			continue
		}
		for _, m := range ms {
			n := len(m.sites(orig))
			for k := range min(n, perKind) {
				// Spread the sites that are tried over the statement.
				site := k * n / min(n, perKind)
				shown, _, _ := parser.Parse(src)
				what := m.sites(shown)[site]()
				c.seen = nil
				var r Result
				c.compare(&r, src, orig, shown)
				o := outcomes[m.kind]
				if o.byWhat[what] == nil {
					o.byWhat[what] = &[3]int{}
				}
				// The comparison of declarations compares only what damage to declarations changes.
				compared := r.Way == Bytecode || r.Way == Probes || (r.Declared && slices.Contains(declarationKinds, m.kind))
				if !compared {
					// The statement was compared only by its expressions: the mutation was compared
					// if every expression that it changed was.
					before, after := topExprs(orig), topExprs(shown)
					if len(before) == len(after) && len(r.Exprs) == len(after) {
						changed, all := 0, true
						for i := range after {
							if Print(before[i]) != Print(after[i]) {
								changed++
								all = all && (r.Exprs[i].Bytecode || r.Exprs[i].Values > 0)
							}
						}
						compared = changed > 0 && all
					}
				}
				switch {
				case r.Fault != "":
					o.detected++
					o.byWhat[what][0]++
					if strings.HasPrefix(r.Fault, declaredFault) {
						o.byDeclared++
					}
				case !compared:
					o.notCompared++
					o.byWhat[what][2]++
				default:
					o.undetected++
					o.byWhat[what][1]++
					if len(o.examples[what]) < 3 {
						o.examples[what] = append(o.examples[what], fmt.Sprintf("%q printed as %q", src, r.Printed))
					} else {
						o.examples[what] = append(o.examples[what], "")
					}
				}
			}
		}
	}
}

func reportMutations(t *testing.T, outcomes map[string]*outcome) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "%-46s %8s %10s %12s %8s %8s\n", "mutation", "detected", "undetected", "not compared", "rate", "declared")
	for _, m := range mutations {
		o := outcomes[m.kind]
		rate := 0.0
		if o.detected+o.undetected > 0 {
			rate = 100 * float64(o.detected) / float64(o.detected+o.undetected)
		}
		fmt.Fprintf(&b, "%-46s %8d %10d %12d %7.1f%% %8d\n", m.kind, o.detected, o.undetected, o.notCompared, rate, o.byDeclared)
	}
	fmt.Fprintf(&b, "\n%-46s %-46s %8s %10s %12s\n", "mutation", "what", "detected", "undetected", "not compared")
	for _, m := range mutations {
		o := outcomes[m.kind]
		if !slices.Contains(declarationKinds, m.kind) {
			continue
		}
		for _, what := range sortedKeys(o.byWhat) {
			c := o.byWhat[what]
			fmt.Fprintf(&b, "%-46s %-46s %8d %10d %12d\n", m.kind, what, c[0], c[1], c[2])
		}
	}
	for _, m := range mutations {
		o := outcomes[m.kind]
		whats := slices.SortedFunc(func(yield func(string) bool) {
			for k := range o.examples {
				if !yield(k) {
					return
				}
			}
		}, func(a, b string) int {
			return cmp.Or(cmp.Compare(len(o.examples[b]), len(o.examples[a])), strings.Compare(a, b))
		})
		for _, what := range whats {
			es := o.examples[what]
			fmt.Fprintf(&b, "\nundetected, %s: %s: %d\n", m.kind, what, len(es))
			for _, e := range es {
				if e != "" {
					fmt.Fprintf(&b, "  %s\n", e)
				}
			}
		}
	}
	return b.String()
}

// TestCheck_mutations damages the trees of a sample of the corpus on purpose, in ways that change
// their shape, and shows how often the check notices.
func TestCheck_mutations(t *testing.T) {
	var sqls []string
	step := 20
	if Slow() {
		step = 200
	}
	for i, e := range corpus.All() {
		if i%step == 0 {
			sqls = append(sqls, e.SQL)
		}
	}
	sqls = append(sqls, Structure...)
	sqls = append(sqls, Declared...)
	for i, g := range append(Pairs(3, 1), Triples(4, 2000)...) {
		if i%(step/4) == 0 {
			sqls = append(sqls, g.SQL)
		}
	}
	outcomes := map[string]*outcome{}
	mutate(t, outcomes, sqls, 2, mutations)
	// The damage to declarations has few sites in that sample, so it also runs on every tenth
	// CREATE TABLE and ALTER TABLE of the corpus, or every hundredth.
	var ddl []string
	n := 0
	for _, e := range corpus.All() {
		stmt, _, err := parser.Parse(e.SQL)
		if err != nil || stmt == nil {
			continue
		}
		switch stmt.Cmd.(type) {
		case *ast.CreateTable, *ast.AlterTable:
			if n%(step/2) == 0 {
				ddl = append(ddl, e.SQL)
			}
			n++
		}
	}
	var declaring []mutation
	for _, m := range mutations {
		if slices.Contains(declarationKinds, m.kind) {
			declaring = append(declaring, m)
		}
	}
	mutate(t, outcomes, ddl, 2, declaring)
	t.Log("\n" + reportMutations(t, outcomes))
	for _, m := range mutations {
		o := outcomes[m.kind]
		switch {
		case m.same != "" && o.detected > 0:
			t.Errorf("the check noticed the mutation %q, which should be the same for SQLite: %s", m.kind, m.same)
		case m.same == "" && o.detected == 0:
			t.Errorf("the check never noticed the mutation %q", m.kind)
		}
	}
	if Slow() {
		return
	}
	// The sample and the sites are fixed, so the outcome of each mutation is pinned: a change of the
	// check that notices less cannot pass unseen.
	var b strings.Builder
	for _, m := range mutations {
		o := outcomes[m.kind]
		fmt.Fprintf(&b, "%v: %v detected, %v undetected, %v not compared\n", m.kind, o.detected, o.undetected, o.notCompared)
	}
	want := `rotate two operators: 2320 detected, 72 undetected, 32 not compared
swap the operands: 4480 detected, 133 undetected, 179 not compared
move the NOT of IS NOT to its operand: 125 detected, 0 undetected, 3 not compared
move the NOT of NOT BETWEEN, LIKE, IN out: 0 detected, 744 undetected, 8 not compared
swap the operands of LIMIT: 19 detected, 1 undetected, 0 not compared
move ON or USING to the next join: 6 detected, 1 undetected, 0 not compared
swap two join operators: 140 detected, 10 undetected, 2 not compared
move ORDER BY or LIMIT: 33 detected, 0 undetected, 0 not compared
move the ELSE of a nested CASE: 1 detected, 0 undetected, 0 not compared
move FILTER or OVER to an argument: 3 detected, 0 undetected, 0 not compared
move ORDER BY between arguments and window: 83 detected, 0 undetected, 0 not compared
swap the WHEREs of an upsert: 7 detected, 0 undetected, 0 not compared
move a column constraint: 494 detected, 16 undetected, 14 not compared
move CONSTRAINT name: 5 detected, 6 undetected, 0 not compared
move a column's CONSTRAINT name: 2 detected, 4 undetected, 2 not compared
split a number with digit separators: 2 detected, 0 undetected, 0 not compared
move GENERATED ALWAYS between type and AS: 0 detected, 13 undetected, 1 not compared
move the last word of a type into AS: 3 detected, 0 undetected, 0 not compared
move a column's first constraint into its type: 773 detected, 0 undetected, 17 not compared
move ON CONFLICT to the next constraint: 3 detected, 0 undetected, 0 not compared
swap ON DELETE and ON UPDATE: 4 detected, 0 undetected, 0 not compared
change the form of DEFAULT: 29 detected, 0 undetected, 0 not compared
`
	if got := b.String(); got != want {
		t.Errorf("the outcomes of the mutations are\n%v\nbut the test pins\n%v\nChange the pinned outcomes only on purpose, and say why in the commit", got, want)
	}
}
