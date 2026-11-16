package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// mutant of one file: one small change of its code, made of one or more edits of its text.
type mutant struct {
	// Pkg is the directory of the package, relative to the root of the module.
	Pkg string
	// File is the path of the file, relative to the root of the module.
	File string
	// Line of the first edit.
	Line int
	// Func is the function or the package-level declaration that holds the change.
	Func string
	// Op is the name of the mutation operator.
	Op string
	// From is the code before the change, and To after it, each on one line.
	From, To string
	// Key names the mutant independently of line numbers, so that a list of equivalent mutants
	// survives edits elsewhere in the file.
	Key string

	edits []edit
}

// edit of a text: the bytes [start, end) become text.
type edit struct {
	start, end int
	text       string
}

// apply the edits of the mutant to src, the text of its file.
func (m mutant) apply(src []byte) []byte {
	edits := slices.Clone(m.edits)
	slices.SortFunc(edits, func(a, b edit) int { return b.start - a.start })
	out := slices.Clone(src)
	for _, e := range edits {
		out = slices.Concat(out[:e.start], []byte(e.text), out[e.end:])
	}
	return out
}

// constBlocks of a package: for each constant declared in a block of two or more, the names of the
// block in order. The mutation of a constant's use to its neighbour in the block reads them.
type constBlocks map[string][]string

// universe of the mutation: what the generator knows of the packages.
type universe struct {
	// consts of each package by its directory, relative to the root of the module.
	consts map[string]constBlocks
	// kinds of package token in order, and keywords, the kinds of the keywords in order.
	kinds, keywords []string
}

// readConsts of the package in dir, from its files that are not tests.
func readConsts(dir string) (constBlocks, error) {
	files, err := goFiles(dir)
	if err != nil {
		return nil, err
	}
	blocks := constBlocks{}
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			var names []string
			for _, s := range g.Specs {
				for _, n := range s.(*ast.ValueSpec).Names {
					names = append(names, n.Name)
				}
			}
			if len(names) < 2 {
				continue
			}
			for _, n := range names {
				blocks[n] = names
			}
		}
	}
	return blocks, nil
}

// goFiles of the package in dir that are not tests, sorted.
func goFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	slices.Sort(files)
	return files, nil
}

// newUniverse reads the constants of the packages in dirs, which are relative to root, and the kinds
// of package token.
func newUniverse(root string, dirs []string) (*universe, error) {
	u := &universe{consts: map[string]constBlocks{}}
	for _, d := range append([]string{tokenDir}, dirs...) {
		if _, ok := u.consts[d]; ok {
			continue
		}
		c, err := readConsts(filepath.Join(root, d))
		if err != nil {
			return nil, err
		}
		u.consts[d] = c
	}
	tc := u.consts[tokenDir]
	u.kinds = slices.Concat(tc["Illegal"], tc["Abort"])
	u.kinds = slices.DeleteFunc(u.kinds, func(s string) bool { return s == "kinds" })
	u.keywords = slices.DeleteFunc(slices.Clone(tc["Abort"]), func(s string) bool { return s == "kinds" })
	if len(u.kinds) == 0 || len(u.keywords) == 0 {
		return nil, fmt.Errorf("no kinds found in %v", tokenDir)
	}
	return u, nil
}

const tokenDir = "internal/sql/token"

// generate the mutants of the file at path, relative to root, in the package in pkg.
func (u *universe) generate(root, pkg, path string) ([]mutant, error) {
	src, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	g := &generator{u: u, pkg: pkg, path: path, src: src, fset: fset, file: f, seen: map[string]bool{}}
	for _, d := range f.Decls {
		g.decl(d)
	}
	return g.mutants, nil
}

// generator of the mutants of one file.
type generator struct {
	u    *universe
	pkg  string
	path string
	src  []byte
	fset *token.FileSet
	file *ast.File
	// fn is the name of the declaration being walked.
	fn      string
	mutants []mutant
	// seen keys and texts, so that two operators that give the same text give one mutant.
	seen map[string]bool
}

// text of the source from p to q.
func (g *generator) text(p, q token.Pos) string {
	return string(g.src[g.off(p):g.off(q)])
}

// off of the position p in the file.
func (g *generator) off(p token.Pos) int {
	return g.fset.Position(p).Offset
}

// oneLine collapses the spaces of s into single spaces, and cuts it if it is long.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	return s
}

// add a mutant with the operator op, which changes the node n by the edits.
func (g *generator) add(op string, n ast.Node, edits ...edit) {
	g.addSpan(op, g.off(n.Pos()), g.off(n.End()), edits...)
}

// addSpan adds a mutant with the operator op, which changes the bytes [start, end) by the edits.
// From and To show those bytes before and after.
func (g *generator) addSpan(op string, start, end int, edits ...edit) {
	m := mutant{Pkg: g.pkg, File: g.path, Func: g.fn, Op: op, edits: edits}
	for _, e := range edits {
		start, end = min(start, e.start), max(end, e.end)
	}
	m.Line = lineOf(g.src, edits[0].start)
	m.From = oneLine(string(g.src[start:end]))
	after := m.apply(g.src)
	m.To = oneLine(string(after[start : end+len(after)-len(g.src)]))
	if m.From == m.To {
		return
	}
	textKey := string(after)
	if g.seen[textKey] {
		return
	}
	g.seen[textKey] = true
	base := fmt.Sprintf("%v %v %v: %v => %v", strings.TrimPrefix(g.path, "internal/sql/"), g.fn, op, m.From, m.To)
	m.Key = base
	for n := 2; g.seen["key "+m.Key]; n++ {
		m.Key = fmt.Sprintf("%v #%v", base, n)
	}
	g.seen["key "+m.Key] = true
	g.mutants = append(g.mutants, m)
}

// replace gives the edit that replaces the node n by text.
func (g *generator) replace(n ast.Node, text string) edit {
	return edit{start: g.off(n.Pos()), end: g.off(n.End()), text: text}
}

// decl walks a declaration of the file.
func (g *generator) decl(d ast.Decl) {
	switch d := d.(type) {
	case *ast.FuncDecl:
		g.fn = d.Name.Name
		if d.Recv != nil && len(d.Recv.List) == 1 {
			t := d.Recv.List[0].Type
			if s, ok := t.(*ast.StarExpr); ok {
				t = s.X
			}
			if id, ok := t.(*ast.Ident); ok {
				g.fn = id.Name + "." + d.Name.Name
			}
		}
		if d.Body != nil {
			g.walk(d.Body)
		}
		if d.Name.Name == "infixPrec" && g.pkg == "internal/sql/parser" {
			g.precedenceTable(d)
		}
	case *ast.GenDecl:
		if d.Tok == token.IMPORT || d.Tok == token.TYPE {
			return
		}
		for _, s := range d.Specs {
			vs := s.(*ast.ValueSpec)
			g.fn = vs.Names[0].Name
			for _, v := range vs.Values {
				g.walk(v)
			}
			if d.Tok == token.VAR && vs.Names[0].Name == "fallback" && g.pkg == "internal/sql/parser" {
				g.fallbackSet(vs)
			}
		}
	}
}

// walk the node n and add the mutants of every node under it.
func (g *generator) walk(n ast.Node) {
	var stack []ast.Node
	ast.Inspect(n, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		var parent ast.Node
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		stack = append(stack, n)
		g.node(n, parent)
		return true
	})
}

// relational operators and what each becomes: its boundary and its negation.
var operators = map[token.Token][]token.Token{
	token.LSS:  {token.LEQ, token.GEQ},
	token.LEQ:  {token.LSS, token.GTR},
	token.GTR:  {token.GEQ, token.LEQ},
	token.GEQ:  {token.GTR, token.LSS},
	token.EQL:  {token.NEQ},
	token.NEQ:  {token.EQL},
	token.LAND: {token.LOR},
	token.LOR:  {token.LAND},
	token.ADD:  {token.SUB},
	token.SUB:  {token.ADD},
	token.MUL:  {token.QUO},
	token.QUO:  {token.MUL},
	token.REM:  {token.MUL},
}

// node adds the mutants of the node n, whose parent is parent.
func (g *generator) node(n, parent ast.Node) {
	switch n := n.(type) {
	case *ast.BinaryExpr:
		for _, to := range operators[n.Op] {
			at := g.off(n.OpPos)
			g.add("operator", n, edit{start: at, end: at + len(n.Op.String()), text: to.String()})
		}
		if n.Op == token.LAND || n.Op == token.LOR {
			g.condition(n.X)
			g.condition(n.Y)
		}
	case *ast.UnaryExpr:
		if n.Op == token.NOT {
			g.add("negation", n, g.replace(n, g.text(n.X.Pos(), n.X.End())))
		}
	case *ast.IfStmt:
		g.condition(n.Cond)
	case *ast.ForStmt:
		if n.Cond != nil {
			g.condition(n.Cond)
		}
	case *ast.SwitchStmt:
		if n.Tag == nil {
			for _, c := range n.Body.List {
				for _, e := range c.(*ast.CaseClause).List {
					g.condition(e)
				}
			}
		}
		g.swapCases(n.Body)
	case *ast.TypeSwitchStmt:
		g.swapCases(n.Body)
	case *ast.CaseClause:
		if len(n.List) > 1 {
			g.removeElements("case element", n.List)
		}
		g.statements(n.Body)
	case *ast.BlockStmt:
		g.statements(n.List)
	case *ast.BasicLit:
		g.literal(n)
	case *ast.Ident:
		switch n.Name {
		case "true":
			g.add("boolean", n, g.replace(n, "false"))
		case "false":
			g.add("boolean", n, g.replace(n, "true"))
		}
		if !g.isDeclOrKey(n, parent) {
			g.neighbour(n, n.Name, g.u.consts[g.pkg], "")
		}
	case *ast.SelectorExpr:
		if x, ok := n.X.(*ast.Ident); ok && x.Name == "token" && g.pkg != tokenDir {
			g.neighbour(n, n.Sel.Name, g.u.consts[tokenDir], "token.")
		}
	case *ast.CallExpr:
		if id, ok := n.Fun.(*ast.Ident); ok && (id.Name == "min" || id.Name == "max") {
			to := map[string]string{"min": "max", "max": "min"}[id.Name]
			g.add("min/max", n, g.replace(id, to))
		}
		for i := 0; i+1 < len(n.Args); i++ {
			a, b := n.Args[i], n.Args[i+1]
			ta, tb := g.text(a.Pos(), a.End()), g.text(b.Pos(), b.End())
			if ta != tb {
				g.add("swap arguments", n, g.replace(a, tb), g.replace(b, ta))
			}
		}
	case *ast.CompositeLit:
		g.removeElements("element", n.Elts)
	case *ast.AssignStmt:
		switch n.Tok {
		case token.ADD_ASSIGN:
			g.add("operator", n, edit{start: g.off(n.TokPos), end: g.off(n.TokPos) + 2, text: "-="})
		case token.SUB_ASSIGN:
			g.add("operator", n, edit{start: g.off(n.TokPos), end: g.off(n.TokPos) + 2, text: "+="})
		}
	case *ast.IncDecStmt:
		g.incDec(n)
	}
}

// isDeclOrKey reports whether the identifier n is the name of a declaration, the key of a composite
// literal's element, or a field after a dot, where it does not stand for a constant of the package.
func (g *generator) isDeclOrKey(n *ast.Ident, parent ast.Node) bool {
	switch p := parent.(type) {
	case *ast.ValueSpec:
		return slices.Contains(p.Names, n)
	case *ast.Field, *ast.FuncDecl, *ast.TypeSpec, *ast.LabeledStmt, *ast.BranchStmt:
		return true
	case *ast.KeyValueExpr:
		return p.Key == n
	case *ast.SelectorExpr:
		return p.Sel == n
	case *ast.AssignStmt:
		return p.Tok == token.DEFINE && slices.Contains(p.Lhs, ast.Expr(n))
	}
	return false
}

// incDec flips ++ and --.
func (g *generator) incDec(n *ast.IncDecStmt) {
	to := map[token.Token]string{token.INC: "--", token.DEC: "++"}[n.Tok]
	g.add("increment", n, edit{start: g.off(n.TokPos), end: g.off(n.TokPos) + 2, text: to})
}

// condition adds the mutants of a condition: true, false, and its negation. A comparison is negated
// by the mutation of its operator instead, and a negation by its removal.
func (g *generator) condition(e ast.Expr) {
	if id, ok := e.(*ast.Ident); ok && (id.Name == "true" || id.Name == "false") {
		return
	}
	g.add("condition", e, g.replace(e, "true"))
	g.add("condition", e, g.replace(e, "false"))
	switch x := e.(type) {
	case *ast.UnaryExpr:
		if x.Op == token.NOT {
			return
		}
	case *ast.BinaryExpr:
		if _, ok := operators[x.Op]; ok && x.Op != token.LAND && x.Op != token.LOR {
			return
		}
	}
	text := g.text(e.Pos(), e.End())
	switch e.(type) {
	case *ast.Ident, *ast.CallExpr, *ast.SelectorExpr, *ast.ParenExpr, *ast.IndexExpr:
		text = "!" + text
	default:
		text = "!(" + text + ")"
	}
	g.add("condition", e, g.replace(e, text))
}

// statements adds the removal of each statement of a list that can be removed: a call or other
// expression, an assignment that declares nothing, an increment, a return, a break or continue,
// a defer, and a send.
func (g *generator) statements(list []ast.Stmt) {
	for _, s := range list {
		switch s := s.(type) {
		case *ast.AssignStmt:
			if s.Tok == token.DEFINE {
				continue
			}
		case *ast.ExprStmt, *ast.IncDecStmt, *ast.ReturnStmt, *ast.BranchStmt, *ast.DeferStmt, *ast.SendStmt:
		default:
			continue
		}
		g.add("remove statement", s, g.replace(s, ""))
	}
}

// removeElements adds the removal of each element of a list of expressions separated by commas,
// with its comma.
func (g *generator) removeElements(op string, list []ast.Expr) {
	for i, e := range list {
		start, end := g.off(e.Pos()), g.off(e.End())
		if i+1 < len(list) {
			end = g.off(list[i+1].Pos())
		} else if i > 0 {
			start = g.off(list[i-1].End())
		} else if j := g.commaAfter(end); j >= 0 {
			end = j + 1
		}
		g.addSpan(op, g.off(e.Pos()), g.off(e.End()), edit{start: start, end: end, text: ""})
	}
}

// commaAfter the offset at, past spaces, or -1 if the next byte that is not a space is not a comma.
func (g *generator) commaAfter(at int) int {
	for i := at; i < len(g.src); i++ {
		switch g.src[i] {
		case ' ', '\t', '\n', '\r':
			continue
		case ',':
			return i
		}
		return -1
	}
	return -1
}

// literal adds the change of an integer or a character to its neighbours.
func (g *generator) literal(n *ast.BasicLit) {
	switch n.Kind {
	case token.INT:
		v, err := strconv.ParseInt(strings.ReplaceAll(n.Value, "_", ""), 0, 64)
		if err != nil {
			return
		}
		g.add("constant", n, g.replace(n, fmt.Sprint(v+1)))
		if v > 0 {
			g.add("constant", n, g.replace(n, fmt.Sprint(v-1)))
		}
	case token.CHAR:
		r, _, _, err := strconv.UnquoteChar(n.Value[1:len(n.Value)-1], '\'')
		if err != nil {
			return
		}
		g.add("constant", n, g.replace(n, strconv.QuoteRune(r+1)))
		if r > 0 {
			g.add("constant", n, g.replace(n, strconv.QuoteRune(r-1)))
		}
	}
}

// neighbour adds the change of a use of the constant name, of the blocks, to its neighbours in its
// block: both neighbours in a block of at most 20 names, and the next one in a longer block, such
// as the kinds of tokens. The node n is the use, and prefix the qualifier of a name in its text.
func (g *generator) neighbour(n ast.Node, name string, blocks constBlocks, prefix string) {
	block, ok := blocks[name]
	if !ok {
		return
	}
	if prefix != "" {
		// Another package sees only the exported names of the block.
		block = slices.DeleteFunc(slices.Clone(block), func(s string) bool { return !ast.IsExported(s) })
	}
	i := slices.Index(block, name)
	if i < 0 {
		return
	}
	var to []string
	if i+1 < len(block) {
		to = append(to, block[i+1])
	}
	if i > 0 && (len(block) <= 20 || i+1 == len(block)) {
		to = append(to, block[i-1])
	}
	for _, t := range to {
		g.add("neighbour", n, g.replace(n, prefix+t))
	}
}

// swapCases adds the swap of the bodies of each two adjacent clauses of a switch.
func (g *generator) swapCases(body *ast.BlockStmt) {
	var clauses []*ast.CaseClause
	for _, s := range body.List {
		clauses = append(clauses, s.(*ast.CaseClause))
	}
	bodyOf := func(i int) (int, int) {
		start := g.off(clauses[i].Colon) + 1
		end := g.off(body.Rbrace)
		if i+1 < len(clauses) {
			end = g.off(clauses[i+1].Pos())
		}
		return start, end
	}
	for i := 0; i+1 < len(clauses); i++ {
		s1, e1 := bodyOf(i)
		s2, e2 := bodyOf(i + 1)
		b1, b2 := string(g.src[s1:e1]), string(g.src[s2:e2])
		if strings.TrimSpace(b1) == strings.TrimSpace(b2) {
			continue
		}
		g.addSpan("swap cases", g.off(clauses[i].Pos()), e2,
			edit{start: s1, end: e1, text: b2}, edit{start: s2, end: e2, text: b1})
	}
}

// fallbackSet adds, to the list of keywords that fall back to ID, each keyword that is not in it.
// The removal of each of its keywords is a mutant of [generator.removeElements].
func (g *generator) fallbackSet(vs *ast.ValueSpec) {
	ast.Inspect(vs, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !g.allKinds(lit.Elts) {
			return true
		}
		in := g.kindsOf(lit.Elts)
		for _, k := range g.u.keywords {
			if !in[k] {
				at := g.off(lit.Rbrace)
				g.addSpan("add to set", at, at, edit{start: at, end: at, text: "token." + k + ", "})
			}
		}
		return false
	})
}

// precedenceTable adds the mutants of the switch of infixPrec: each kind moved to the clause before
// and after, and each kind that is in no clause added to the first and to the last clause. Space
// and Comment are not added, since the parser never sees them. A clause of one kind is not emptied:
// the changes of its precedence to its neighbours are the mutants of [generator.neighbour].
func (g *generator) precedenceTable(d *ast.FuncDecl) {
	var sw *ast.SwitchStmt
	for _, s := range d.Body.List {
		if s, ok := s.(*ast.SwitchStmt); ok {
			sw = s
		}
	}
	if sw == nil {
		return
	}
	var clauses []*ast.CaseClause
	in := map[string]bool{}
	for _, s := range sw.Body.List {
		c := s.(*ast.CaseClause)
		clauses = append(clauses, c)
		for k := range g.kindsOf(c.List) {
			in[k] = true
		}
	}
	appendTo := func(c *ast.CaseClause, k string) edit {
		at := g.off(c.List[len(c.List)-1].End())
		return edit{start: at, end: at, text: ", token." + k}
	}
	for i, c := range clauses {
		if len(c.List) < 2 {
			continue
		}
		for j, e := range c.List {
			k := strings.TrimPrefix(g.text(e.Pos(), e.End()), "token.")
			start, end := g.off(e.Pos()), g.off(e.End())
			if j+1 < len(c.List) {
				end = g.off(c.List[j+1].Pos())
			} else {
				start = g.off(c.List[j-1].End())
			}
			remove := edit{start: start, end: end, text: ""}
			for _, to := range []int{i - 1, i + 1} {
				if to < 0 || to >= len(clauses) {
					continue
				}
				d := clauses[to]
				g.addSpan("move in table", g.off(min(c.Pos(), d.Pos())), g.off(max(c.Colon, d.Colon)), remove, appendTo(d, k))
			}
		}
	}
	first, last := clauses[0], clauses[len(clauses)-1]
	for _, k := range g.u.kinds {
		if in[k] || k == "Space" || k == "Comment" {
			continue
		}
		for _, c := range []*ast.CaseClause{first, last} {
			g.addSpan("add to table", g.off(c.Pos()), g.off(c.Colon), appendTo(c, k))
		}
	}
}

// allKinds reports whether every element of the list is a kind of token, as token.X.
func (g *generator) allKinds(list []ast.Expr) bool {
	return len(list) > 0 && len(g.kindsOf(list)) == len(list)
}

// kindsOf the elements of the list that are kinds of token, as token.X.
func (g *generator) kindsOf(list []ast.Expr) map[string]bool {
	kinds := map[string]bool{}
	for _, e := range list {
		s, ok := e.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if x, ok := s.X.(*ast.Ident); ok && x.Name == "token" && slices.Contains(g.u.kinds, s.Sel.Name) {
			kinds[s.Sel.Name] = true
		}
	}
	return kinds
}

// lineOf the offset at in src, from 1.
func lineOf(src []byte, at int) int {
	return bytes.Count(src[:at], []byte("\n")) + 1
}
