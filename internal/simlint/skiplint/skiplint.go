// Package skiplint is a lint for tests: a test never skips itself, except under -short. A skipped
// test looks like a passing one, so a skip for a missing variable or credential turns a check off
// without anyone seeing it. A test that cannot run fails, with a message that says what is missing.
//
// [Analyzer] reports, in every Go file, a test or not, each use of the methods Skip, Skipf, and
// SkipNow of the package testing, on testing.T, testing.B, testing.F, or testing.TB, except a call
// that is a statement of its own directly in the body of an if statement whose condition is
// testing.Short() and nothing else, with no init statement:
//
//	if testing.Short() {
//		t.Skip("needs a bucket")
//	}
//
// A fuzz target returns for an input that it leaves out, a test that needs a build mode is in a
// file with a build constraint, and a tool that a person runs is not a test that skips without its
// variable.
package skiplint

import (
	"go/ast"
	"go/types"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Analyzer of the rule for skips, as the package documentation describes.
var Analyzer = &analysis.Analyzer{
	Name: "skiplint",
	Doc:  "report a test that skips itself other than under -short",
	URL:  "https://github.com/maragudk/s4ql/tree/main/internal/simlint/skiplint",
	Run:  run,
}

// allowed files and directories, by the end of their path with a slash before it, and a slash after
// a directory, with the reason. The tests against a real bucket skip without its variables, and are
// being changed to fail instead and skip only under -short; these entries go once that is merged.
var allowed = map[string]string{
	"/internal/store/s3store/s3store_test.go": "its tests against a real bucket are being changed to fail without the bucket",
	"/internal/store/s3test/":                 "its tests against a real bucket are being changed to fail without the bucket",
}

func run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		if isAllowed(filepath.ToSlash(pass.Fset.Position(file.Package).Filename)) {
			continue
		}
		var stack []ast.Node
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || !isSkip(pass.TypesInfo.Uses[sel.Sel]) {
				return true
			}
			if !underShort(pass, stack) {
				pass.Reportf(sel.Pos(), "%v skips the test other than in if testing.Short() { ... }; a test that cannot run fails, and skips only under -short", sel.Sel.Name)
			}
			return true
		})
	}
	return nil, nil
}

// isAllowed reports whether the file with the given path, with slashes, is in [allowed].
func isAllowed(path string) bool {
	for suffix := range allowed {
		if strings.HasSuffix(suffix, "/") {
			if strings.Contains(path, suffix) {
				return true
			}
		} else if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// isSkip reports whether obj is the method Skip, Skipf, or SkipNow of a type of the package testing.
func isSkip(obj types.Object) bool {
	f, ok := obj.(*types.Func)
	if !ok || f.Pkg() == nil || f.Pkg().Path() != "testing" {
		return false
	}
	switch f.Name() {
	case "Skip", "Skipf", "SkipNow":
		return f.Signature().Recv() != nil
	}
	return false
}

// underShort reports whether the selector at the end of stack, the path from the file to it, is the
// function of a call that is a statement directly in the body of an if statement whose condition is
// testing.Short() alone, with no init statement.
func underShort(pass *analysis.Pass, stack []ast.Node) bool {
	if len(stack) < 5 {
		return false
	}
	sel, call, stmt, block, ifStmt := stack[len(stack)-1], stack[len(stack)-2], stack[len(stack)-3], stack[len(stack)-4], stack[len(stack)-5]
	if c, ok := call.(*ast.CallExpr); !ok || c.Fun != sel {
		return false
	}
	if s, ok := stmt.(*ast.ExprStmt); !ok || s.X != call {
		return false
	}
	i, ok := ifStmt.(*ast.IfStmt)
	if !ok || i.Body != block || i.Init != nil {
		return false
	}
	cond, ok := ast.Unparen(i.Cond).(*ast.CallExpr)
	if !ok || len(cond.Args) != 0 {
		return false
	}
	var name *ast.Ident
	switch fun := ast.Unparen(cond.Fun).(type) {
	case *ast.SelectorExpr:
		name = fun.Sel
	case *ast.Ident:
		name = fun
	default:
		return false
	}
	f, ok := pass.TypesInfo.Uses[name].(*types.Func)
	return ok && f.Pkg() != nil && f.Pkg().Path() == "testing" && f.Name() == "Short" && f.Signature().Recv() == nil
}
