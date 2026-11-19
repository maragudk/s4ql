// Command simlint runs the lints of maragu.dev/s4ql/internal/simlint, for library code that runs
// under the simulation, and of maragu.dev/s4ql/internal/simlint/skiplint, for tests that skip
// themselves, over the packages it is given, as in "go run ./internal/simlint/cmd/simlint ./...".
package main

import (
	"golang.org/x/tools/go/analysis/multichecker"

	"maragu.dev/s4ql/internal/simlint"
	"maragu.dev/s4ql/internal/simlint/skiplint"
)

func main() {
	multichecker.Main(simlint.Analyzer, skiplint.Analyzer)
}
