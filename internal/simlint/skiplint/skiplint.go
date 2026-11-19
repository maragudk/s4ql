// Package skiplint is a lint for tests: a test never skips itself, except under -short. A skipped
// test looks like a passing one, so a skip for a missing variable or credential turns a check off
// without anyone seeing it. A test that cannot run fails, with a message that says what is missing.
//
// [Analyzer] reports, in every Go file, a test or not, each use of the methods Skip, Skipf, and
// SkipNow of the package testing, on testing.T, testing.B, testing.F, or testing.TB, and of a
// method of another interface with the same name and parameters, which a *testing.T can stand
// behind. It allows only a call that is a statement of its own directly in the body of an if
// statement whose condition is testing.Short() and nothing else, with no init statement:
//
//	if testing.Short() {
//		t.Skip("needs a bucket")
//	}
//
// The files of the package that its build leaves out by their build constraints, such as a test
// that needs the race detector, have no type information, so in them every selector named Skip,
// Skipf, or SkipNow counts, and the condition must be written testing.Short().
//
// A fuzz target returns for an input that it leaves out, a test that needs a build mode is in a
// file with a build constraint, and a tool that a person runs is not a test that skips without its
// variable.
package skiplint

import (
	"go/ast"
	"go/parser"
	"go/types"
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

func run(pass *analysis.Pass) (any, error) {
	typed := matcher{
		isSkip: func(sel *ast.SelectorExpr) bool { return isSkip(pass.TypesInfo.Uses[sel.Sel]) },
		isShort: func(fun ast.Expr) bool {
			var name *ast.Ident
			switch fun := fun.(type) {
			case *ast.SelectorExpr:
				name = fun.Sel
			case *ast.Ident:
				name = fun
			default:
				return false
			}
			f, ok := pass.TypesInfo.Uses[name].(*types.Func)
			return ok && f.Pkg() != nil && f.Pkg().Path() == "testing" && f.Name() == "Short" && f.Signature().Recv() == nil
		},
	}
	for _, file := range pass.Files {
		typed.check(pass, file)
	}

	untyped := matcher{
		isSkip: func(sel *ast.SelectorExpr) bool { return isSkipName(sel.Sel.Name) },
		isShort: func(fun ast.Expr) bool {
			sel, ok := fun.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			pkg, ok := sel.X.(*ast.Ident)
			return ok && pkg.Name == "testing" && sel.Sel.Name == "Short"
		},
	}
	for _, name := range pass.IgnoredFiles {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		content, err := pass.ReadFile(name)
		if err != nil {
			return nil, err
		}
		file, err := parser.ParseFile(pass.Fset, name, content, parser.SkipObjectResolution)
		if err != nil {
			// A file that does not parse is the compiler's to report, in the build that has it.
			continue
		}
		untyped.check(pass, file)
	}
	return nil, nil
}

// matcher of skips and of the condition that allows them, with type information or without.
type matcher struct {
	// isSkip reports whether the selector is a method that skips a test.
	isSkip func(sel *ast.SelectorExpr) bool
	// isShort reports whether the function of a call is testing.Short.
	isShort func(fun ast.Expr) bool
}

// check the file, and report each skip that is not directly under if testing.Short().
func (m matcher) check(pass *analysis.Pass, file *ast.File) {
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		if sel, ok := n.(*ast.SelectorExpr); ok && m.isSkip(sel) && !m.underShort(stack) {
			pass.Reportf(sel.Pos(), "%v skips the test other than in if testing.Short() { ... }; a test that cannot run fails, and skips only under -short", sel.Sel.Name)
		}
		return true
	})
}

// underShort reports whether the selector at the end of stack, the path from the file to it, is the
// function of a call that is a statement directly in the body of an if statement whose condition is
// testing.Short() alone, with no init statement.
func (m matcher) underShort(stack []ast.Node) bool {
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
	cond, ok := i.Cond.(*ast.CallExpr)
	return ok && len(cond.Args) == 0 && m.isShort(cond.Fun)
}

// isSkipName reports whether name is that of a method of testing that skips a test.
func isSkipName(name string) bool {
	return name == "Skip" || name == "Skipf" || name == "SkipNow"
}

// isSkip reports whether obj is the method Skip, Skipf, or SkipNow of a type of the package
// testing, or a method of another interface with the name and the parameters of one of them.
func isSkip(obj types.Object) bool {
	f, ok := obj.(*types.Func)
	if !ok || !isSkipName(f.Name()) || f.Signature().Recv() == nil {
		return false
	}
	if f.Pkg() != nil && f.Pkg().Path() == "testing" {
		return true
	}
	if !types.IsInterface(f.Signature().Recv().Type()) {
		return false
	}
	sig := f.Signature()
	params := sig.Params()
	if sig.Results().Len() != 0 {
		return false
	}
	switch f.Name() {
	case "SkipNow":
		return params.Len() == 0
	case "Skip":
		return sig.Variadic() && params.Len() == 1 && isAnySlice(params.At(0).Type())
	default:
		return sig.Variadic() && params.Len() == 2 && types.Identical(params.At(0).Type(), types.Typ[types.String]) &&
			isAnySlice(params.At(1).Type())
	}
}

// isAnySlice reports whether t is a slice of the empty interface, the type of ...any.
func isAnySlice(t types.Type) bool {
	s, ok := t.Underlying().(*types.Slice)
	if !ok {
		return false
	}
	i, ok := s.Elem().Underlying().(*types.Interface)
	return ok && i.Empty()
}
