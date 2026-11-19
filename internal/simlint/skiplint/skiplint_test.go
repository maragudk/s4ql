package skiplint_test

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"maragu.dev/s4ql/internal/simlint/skiplint"
)

func TestAnalyzer(t *testing.T) {
	t.Run("should report every planted skip other than in if testing.Short(), in tests and in other files", func(t *testing.T) {
		analysistest.Run(t, analysistest.TestData(), skiplint.Analyzer, "skips")
	})

	// analysistest reads no expectations from a file that the build leaves out, so it reports the
	// diagnostics in such a file as unexpected, and this test checks them.
	t.Run("should report a planted skip in a file that the build leaves out, and allow one under testing.Short()", func(t *testing.T) {
		var r recorder
		analysistest.Run(&r, analysistest.TestData(), skiplint.Analyzer, "tagged")
		if len(r.errors) == 0 {
			t.Fatal("no diagnostic")
		}
		for _, e := range r.errors {
			if !strings.Contains(e, "tagged/tagged_test.go:10:2: unexpected diagnostic: Skip skips the test") {
				t.Error(e)
			}
		}
	})
}

// recorder of the errors of analysistest.
type recorder struct {
	errors []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}
