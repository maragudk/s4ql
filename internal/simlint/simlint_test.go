package simlint_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"maragu.dev/s4ql/internal/simlint"
)

func TestAnalyzer(t *testing.T) {
	t.Run("should report every planted violation in library code, and nothing in its tests or in an exempt package", func(t *testing.T) {
		analysistest.Run(t, analysistest.TestData(), simlint.Analyzer, "library", "maragu.dev/s4ql/internal/host")
	})
}
