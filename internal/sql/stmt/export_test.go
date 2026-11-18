package stmt

import (
	goast "go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"
)

// ASTTypes of package ast, read from its source: the name of every exported struct type, and the
// names of the exported fields of each.
func ASTTypes(t testing.TB) map[string][]string {
	t.Helper()
	files, err := filepath.Glob("../ast/*.go")
	is.NotError(t, err)
	types := map[string][]string{}
	fset := gotoken.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := goparser.ParseFile(fset, name, nil, 0)
		is.NotError(t, err)
		goast.Inspect(f, func(n goast.Node) bool {
			ts, ok := n.(*goast.TypeSpec)
			if !ok {
				return true
			}
			if st, ok := ts.Type.(*goast.StructType); ok && ts.Name.IsExported() {
				types[ts.Name.Name] = []string{}
				for _, field := range st.Fields.List {
					for _, id := range field.Names {
						types[ts.Name.Name] = append(types[ts.Name.Name], id.Name)
					}
				}
			}
			return true
		})
	}
	return types
}

// NodeTypes of package ast: every exported struct type of [ASTTypes] but the four that are not nodes,
// sorted. A new struct type that is not a node shows up in the tests as a node that no statement has.
func NodeTypes(t testing.TB) []string {
	t.Helper()
	var names []string
	for name := range ASTTypes(t) {
		switch name {
		case "Span", "Token", "QualifiedName", "IfNotExists":
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
