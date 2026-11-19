package skiplint_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"maragu.dev/s4ql/internal/simlint/skiplint"
)

func TestAnalyzer(t *testing.T) {
	t.Run("should report every planted skip other than in if testing.Short(), in tests and in other files", func(t *testing.T) {
		analysistest.Run(t, analysistest.TestData(), skiplint.Analyzer, "skips")
	})

	t.Run("should allow the files and directories in its list, and report the other files of their packages", func(t *testing.T) {
		analysistest.Run(t, analysistest.TestData(), skiplint.Analyzer, "maragu.dev/s4ql/internal/store/s3store", "maragu.dev/s4ql/internal/store/s3test")
	})
}
