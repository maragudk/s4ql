// Package simlint is a lint for library code, which must wait, read the time, start goroutines, and
// draw randomness only through the host interface in maragu.dev/s4ql/internal/host, so that a
// simulation can implement the interface and replay a run from a seed. A simulation can notice some
// breaches of that rule only when the code that breaks it runs, and others never, so the lint checks
// the code itself.
//
// [Analyzer] reports, in every Go file that is not a test:
//
//   - a go statement, and iter.Pull, iter.Pull2, runtime.SetFinalizer, runtime.AddCleanup, and an
//     import of golang.org/x/sync/errgroup, because they start goroutines, and a goroutine must be
//     started with host.Host.Go;
//   - a channel receive, a send, a range over a channel or over a type parameter whose type set
//     holds one, and a select without a default, because they can wait outside the host;
//     every rule holds in the clauses of a select too, with a default or without, and only the
//     receive or send that a clause waits on is left to the rule for the select;
//   - the time package's clock, sleeps, timers, and tickers, and timestamppb.Now of Protocol
//     Buffers, because the time is host.Host.Wall and host.Host.Monotonic, and waiting is
//     host.Host.Sleep;
//   - context.WithTimeout, context.WithDeadline, and their Cause variants, because a timeout is
//     host.Host.WithTimeout, context.AfterFunc, because it starts a goroutine, and
//     context.Background and context.TODO, because code passes on the context it was given;
//   - sync.Cond and sync.WaitGroup, because a wait for another goroutine is a host.Task or a
//     host.Semaphore;
//   - io.Pipe and its types io.PipeReader and io.PipeWriter, because their goroutines block on
//     waits that the scheduler cannot see, and a wait must be a host.Semaphore or a host.Task;
//   - an import of math/rand, math/rand/v2, crypto/rand, or hash/maphash, because randomness is
//     host.Host.Uint64;
//   - host.New, because it makes the production host, and library code runs on the host it is
//     given, so that a simulation can give it another.
//
// The packages that implement the host, and others that never run under a simulation, are exempt.
// Their list is in this file.
package simlint

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Analyzer of the rules for library code, as the package documentation describes.
var Analyzer = &analysis.Analyzer{
	Name: "simlint",
	Doc:  "report what library code must not do so that the simulation can replay it",
	URL:  "https://github.com/maragudk/s4ql/tree/main/internal/simlint",
	Run:  run,
}

// exempt packages, by import path, with the reason each is exempt.
var exempt = map[string]string{
	"maragu.dev/s4ql/internal/host":                  "it is the production host, which maps the host onto ordinary Go",
	"maragu.dev/s4ql/internal/sim":                   "it is the simulation, which implements the host",
	"maragu.dev/s4ql/internal/simulator/sitecheck":   "it is a command that checks the simulator's page of the site in headless Chromium, and never runs under the simulation",
	"maragu.dev/s4ql/internal/sql/mutation":          "it is a command that runs the mutation testing of the SQL front end, and never runs under the simulation",
	"maragu.dev/s4ql/internal/sql/parsetest":         "it is a harness for the tests of the SQL parser, which compares it with SQLite and never runs under the simulation",
	"maragu.dev/s4ql/internal/sql/shapetest":         "it is a check of the tree of the SQL parser against SQLite, for tests, and never runs under the simulation",
	"maragu.dev/s4ql/internal/sqlite/browser":        "it is a command that runs a test binary in headless Chromium, and never runs under the simulation",
	"maragu.dev/s4ql/internal/sqlite/browser/update": "it is a command that vendors the SQLite WebAssembly build, and never runs under the simulation",
	"maragu.dev/s4ql/internal/sqlite/update":         "it is a command that vendors SQLite, and never runs under the simulation",
	"maragu.dev/s4ql/internal/store/s3store":         "it is the client of real S3, whose SDK and net/http start goroutines, read the clock, and wait on the network outside the host, so it never runs under the simulation",
}

// forbidden identifiers, by the import path of their package and their name, with what to use
// instead.
var forbidden = map[string]map[string]string{
	"time": {
		"Now":       "reads the real clock; use host.Host.Wall or host.Host.Monotonic",
		"Since":     "reads the real clock; use host.Host.Monotonic",
		"Until":     "reads the real clock; use host.Host.Monotonic",
		"Sleep":     "waits on the real clock; use host.Host.Sleep",
		"After":     "waits on the real clock; use host.Host.Sleep or host.Host.WithTimeout",
		"AfterFunc": "starts a goroutine on the real clock; use host.Host.Go and host.Host.Sleep",
		"NewTimer":  "waits on the real clock; use host.Host.Sleep or host.Host.WithTimeout",
		"NewTicker": "waits on the real clock; use host.Host.Sleep in a loop",
		"Tick":      "waits on the real clock; use host.Host.Sleep in a loop",
		"Timer":     "waits on the real clock; use host.Host.Sleep or host.Host.WithTimeout",
		"Ticker":    "waits on the real clock; use host.Host.Sleep in a loop",
	},
	"context": {
		"WithTimeout":       "sets a timer on the real clock; use host.Host.WithTimeout",
		"WithTimeoutCause":  "sets a timer on the real clock; use host.Host.WithTimeout",
		"WithDeadline":      "sets a timer on the real clock; use host.Host.WithTimeout",
		"WithDeadlineCause": "sets a timer on the real clock; use host.Host.WithTimeout",
		"AfterFunc":         "starts a goroutine; use host.Host.Go",
		"Background":        "starts a context of its own; pass on the context you were given, or context.WithoutCancel of it",
		"TODO":              "starts a context of its own; pass on the context you were given, or context.WithoutCancel of it",
	},
	"sync": {
		"Cond":      "waits outside the host; use a host.Semaphore",
		"NewCond":   "waits outside the host; use a host.Semaphore",
		"WaitGroup": "waits outside the host; use host.Task.Wait",
	},
	"io": {
		"Pipe":       "blocks its goroutines on waits outside the host; use a host.Semaphore or a host.Task",
		"PipeReader": "blocks its goroutines on waits outside the host; use a host.Semaphore or a host.Task",
		"PipeWriter": "blocks its goroutines on waits outside the host; use a host.Semaphore or a host.Task",
	},
	"iter": {
		"Pull":  "starts a goroutine; use the iterator with a range",
		"Pull2": "starts a goroutine; use the iterator with a range",
	},
	"runtime": {
		"SetFinalizer": "runs a function in a goroutine outside the host",
		"AddCleanup":   "runs a function in a goroutine outside the host",
	},
	"google.golang.org/protobuf/types/known/timestamppb": {
		"Now": "reads the real clock; use timestamppb.New with host.Host.Wall",
	},
	// The packages that make the production host today are exempt from every rule: the host itself,
	// and the simulation's free-running mode. A package that must make it and keep the other rules,
	// such as one that runs a real instance, needs an exception for this rule alone, which the lint
	// does not have yet.
	"maragu.dev/s4ql/internal/host": {
		"New": "makes the production host; run on the host.Host that the code is given",
	},
}

// forbiddenImports of packages, by import path, with what to use instead.
var forbiddenImports = map[string]string{
	"math/rand":                  "draws randomness outside the host; use host.Host.Uint64",
	"math/rand/v2":               "draws randomness outside the host; use host.Host.Uint64",
	"crypto/rand":                "draws randomness outside the host; use host.Host.Uint64",
	"hash/maphash":               "draws randomness outside the host; use host.Host.Uint64",
	"golang.org/x/sync/errgroup": "starts goroutines outside the host; use host.Host.Go",
}

func run(pass *analysis.Pass) (any, error) {
	if _, ok := exempt[pass.Pkg.Path()]; ok {
		return nil, nil
	}

	for _, file := range pass.Files {
		if strings.HasSuffix(pass.Fset.Position(file.Package).Filename, "_test.go") {
			continue
		}
		var inspect func(n ast.Node) bool
		inspect = func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.GoStmt:
				pass.Reportf(n.Pos(), "a go statement starts a goroutine outside the host; use host.Host.Go")

			case *ast.SelectStmt:
				if !hasDefault(n) {
					pass.Reportf(n.Pos(), "a select without a default waits outside the host; use a host.Semaphore or a host.Task")
				}
				// The receive or send of each clause is reported with the select, or never waits
				// because the select has a default. The rest of each clause is checked as any other
				// code: its channel and the value it sends, which run before the select chooses,
				// where it puts what it receives, which runs after the select chose it, and its body.
				for _, clause := range n.Body.List {
					clause := clause.(*ast.CommClause)
					for _, operand := range commOperands(clause.Comm) {
						ast.Inspect(operand, inspect)
					}
					for _, stmt := range clause.Body {
						ast.Inspect(stmt, inspect)
					}
				}
				return false

			case *ast.UnaryExpr:
				if n.Op == token.ARROW {
					pass.Reportf(n.Pos(), "a channel receive waits outside the host; use a host.Semaphore or a host.Task")
				}

			case *ast.SendStmt:
				pass.Reportf(n.Pos(), "a channel send waits outside the host; use a host.Semaphore or a host.Task")

			case *ast.RangeStmt:
				if isChan(pass.TypesInfo.TypeOf(n.X)) {
					pass.Reportf(n.Pos(), "a range over a channel waits outside the host; use a host.Semaphore or a host.Task")
				}

			case *ast.ImportSpec:
				path, err := strconv.Unquote(n.Path.Value)
				if err != nil {
					return true
				}
				if why, ok := forbiddenImports[path]; ok {
					pass.Reportf(n.Pos(), "%v %v", path, why)
				}

			case *ast.Ident:
				obj := pass.TypesInfo.Uses[n]
				if obj == nil || obj.Pkg() == nil || obj.Parent() != obj.Pkg().Scope() {
					return true
				}
				if _, ok := obj.(*types.PkgName); ok {
					return true
				}
				if why, ok := forbidden[obj.Pkg().Path()][obj.Name()]; ok {
					pass.Reportf(n.Pos(), "%v.%v %v", obj.Pkg().Name(), obj.Name(), why)
				}
			}
			return true
		}
		ast.Inspect(file, inspect)
	}
	return nil, nil
}

// commOperands of the communication of a select's clause: its expressions without the receive or
// the send itself. A default clause has none.
func commOperands(comm ast.Stmt) []ast.Expr {
	switch comm := comm.(type) {
	case *ast.SendStmt:
		return []ast.Expr{comm.Chan, comm.Value}
	case *ast.ExprStmt:
		return []ast.Expr{receiveOperand(comm.X)}
	case *ast.AssignStmt:
		// The receive is the one expression on the right.
		return append(slices.Clone(comm.Lhs), receiveOperand(comm.Rhs[0]))
	}
	return nil
}

// receiveOperand is the channel of the receive expression x, which may be in parentheses.
func receiveOperand(x ast.Expr) ast.Expr {
	return ast.Unparen(x).(*ast.UnaryExpr).X
}

// isChan reports whether t is a channel, or a type parameter whose type set holds a channel. The
// underlying type of a type parameter is the interface of its constraint. A range over a type
// parameter is valid only if every type in its set ranges alike, so one channel in the set means
// that the range is over a channel.
func isChan(t types.Type) bool {
	switch u := t.Underlying().(type) {
	case *types.Chan:
		return true
	case *types.Interface:
		for e := range u.EmbeddedTypes() {
			if union, ok := e.(*types.Union); ok {
				for term := range union.Terms() {
					if isChan(term.Type()) {
						return true
					}
				}
			} else if isChan(e) {
				return true
			}
		}
	}
	return false
}

// hasDefault reports whether the select has a default clause.
func hasDefault(s *ast.SelectStmt) bool {
	for _, clause := range s.Body.List {
		if clause.(*ast.CommClause).Comm == nil {
			return true
		}
	}
	return false
}
