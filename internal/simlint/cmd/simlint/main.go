// Command simlint runs the lint of maragu.dev/s4ql/internal/simlint over the packages it is given,
// as in "go run ./internal/simlint/cmd/simlint ./...".
package main

import (
	"golang.org/x/tools/go/analysis/singlechecker"

	"maragu.dev/s4ql/internal/simlint"
)

func main() {
	singlechecker.Main(simlint.Analyzer)
}
