// Package simlint is a lint for library code, which must run under the simulation in
// maragu.dev/s4ql/internal/sim. The scheduler there can replay a run only if library code waits,
// reads the time, and draws randomness only through maragu.dev/s4ql/internal/host, and it notices
// some breaches of that rule only when the code that breaks it runs, and others never.
//
// [Analyzer] reports, in every Go file of the module that is not a test:
//
//   - a go statement, because a goroutine must be started with host.Host.Go;
//   - the time package's clock, sleeps, timers, and tickers, because the time is host.Host.Wall and
//     host.Host.Monotonic, and waiting is host.Host.Sleep;
//   - context.WithTimeout, context.WithDeadline, and their Cause variants, because a timeout is
//     host.Host.WithTimeout, and context.AfterFunc, because it starts a goroutine;
//   - sync.Cond and sync.WaitGroup, because a wait for another goroutine is a host.Task or a
//     host.Semaphore;
//   - an import of math/rand, math/rand/v2, or crypto/rand, because randomness is host.Host.Uint64.
//
// The packages that implement the host, and others that never run under the simulation, are exempt.
// Their list is in this file.
package simlint

import (
	"go/ast"
	"go/types"
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
	"maragu.dev/s4ql/internal/host":          "it is the production host, which maps the host onto ordinary Go",
	"maragu.dev/s4ql/internal/sim":           "it is the simulation, which implements the host",
	"maragu.dev/s4ql/internal/sqlite/update": "it is a command that vendors SQLite, and never runs under the simulation",
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
	},
	"sync": {
		"Cond":      "waits outside the host; use a host.Semaphore",
		"NewCond":   "waits outside the host; use a host.Semaphore",
		"WaitGroup": "waits outside the host; use host.Task.Wait",
	},
}

// forbiddenImports of packages, by import path, with what to use instead.
var forbiddenImports = map[string]string{
	"math/rand":    "draws randomness outside the host; use host.Host.Uint64",
	"math/rand/v2": "draws randomness outside the host; use host.Host.Uint64",
	"crypto/rand":  "draws randomness outside the host; use host.Host.Uint64",
}

func run(pass *analysis.Pass) (any, error) {
	if _, ok := exempt[pass.Pkg.Path()]; ok {
		return nil, nil
	}

	for _, file := range pass.Files {
		if strings.HasSuffix(pass.Fset.Position(file.Package).Filename, "_test.go") {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.GoStmt:
				pass.Reportf(n.Pos(), "a go statement starts a goroutine outside the host; use host.Host.Go")

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
		})
	}
	return nil, nil
}
