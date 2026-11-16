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
	{"split a number with digit separators", splitNumber, ""},
}

// outcome of a mutation: detected, not detected although compared, or not compared.
type outcome struct {
	detected, undetected, notCompared int
	// examples of undetected mutations, by what was done.
	examples map[string][]string
}

// mutate applies every mutation at up to perKind sites of each statement of sqls that parses, and
// gives the outcome by kind.
func mutate(t *testing.T, sqls []string, perKind int) map[string]*outcome {
	t.Helper()
	outcomes := map[string]*outcome{}
	for _, m := range mutations {
		outcomes[m.kind] = &outcome{examples: map[string][]string{}}
	}
	var c Checker
	defer c.Close()
	for _, src := range sqls {
		orig, _, err := parser.Parse(src)
		if err != nil || orig == nil {
			continue
		}
		for _, m := range mutations {
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
				compared := r.Way == Bytecode || r.Way == Probes
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
				case !compared:
					o.notCompared++
				default:
					o.undetected++
					if len(o.examples[what]) < 3 {
						o.examples[what] = append(o.examples[what], fmt.Sprintf("%q printed as %q", src, r.Printed))
					} else {
						o.examples[what] = append(o.examples[what], "")
					}
				}
			}
		}
	}
	return outcomes
}

func reportMutations(t *testing.T, outcomes map[string]*outcome) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "%-46s %8s %10s %12s %8s\n", "mutation", "detected", "undetected", "not compared", "rate")
	for _, m := range mutations {
		o := outcomes[m.kind]
		rate := 0.0
		if o.detected+o.undetected > 0 {
			rate = 100 * float64(o.detected) / float64(o.detected+o.undetected)
		}
		fmt.Fprintf(&b, "%-46s %8d %10d %12d %7.1f%%\n", m.kind, o.detected, o.undetected, o.notCompared, rate)
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
	for i, g := range append(Pairs(3, 1), Triples(4, 2000)...) {
		if i%(step/4) == 0 {
			sqls = append(sqls, g.SQL)
		}
	}
	outcomes := mutate(t, sqls, 2)
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
}
