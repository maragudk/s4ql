package main

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"
)

// TestRunner_run runs mutants of a tiny module through the runner, from the plan of its stages to
// the verdict on each mutant.
func TestRunner_run(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                           "module " + module + "\n\ngo 1.24\n",
		"internal/sql/parser/p.go":         "package parser\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
		"internal/sql/parser/p_test.go":    "package parser\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif got := Add(1, 2); got != 3 {\n\t\tt.Errorf(\"Add(1, 2) = %v\", got)\n\t}\n}\n",
		"internal/sql/loop/loop.go":        "package loop\n\nfunc Sum(n int) int {\n\ts := 0\n\tfor i := 0; i < n; i++ {\n\t\ts += i\n\t}\n\treturn s\n}\n",
		"internal/sql/loop/loop_test.go":   "package loop\n\nimport \"testing\"\n\nfunc TestSum(t *testing.T) {\n\tif got := Sum(4); got != 6 {\n\t\tt.Errorf(\"Sum(4) = %v\", got)\n\t}\n}\n",
		"internal/sql/other/other.go":      "package other\n\nimport \"maragu.dev/s4ql/internal/sql/parser\"\n\nfunc Three() int {\n\treturn parser.Add(1, 2)\n}\n",
		"internal/sql/other/other_test.go": "package other\n\nimport \"testing\"\n\nfunc TestThree(t *testing.T) {\n\tif Three() != 3 {\n\t\tt.Error(\"not 3\")\n\t}\n}\n",
		// The package at the root, with a test that needs a real bucket, which fails here.
		"r.go":      "package root\n\nimport \"maragu.dev/s4ql/internal/sql/parser\"\n\nfunc Four() int {\n\treturn parser.Add(2, 2)\n}\n\nfunc Five() int {\n\treturn 5\n}\n",
		"r_test.go": "package root\n\nimport \"testing\"\n\nfunc TestFour(t *testing.T) {\n\tif Four() != 4 {\n\t\tt.Error(\"not 4\")\n\t}\n}\n\nfunc TestOpen_realBucket(t *testing.T) {\n\tt.Fatal(\"no bucket\")\n}\n",
		// The driver check, in a module of its own that replaces the module with the tree.
		"internal/sqlite/crosscheck/go.mod": "module " + module + "/internal/sqlite/crosscheck\n\ngo 1.24\n\nrequire " + module + " v0.0.0\n\nreplace " + module + " => ../../..\n",
		"internal/sqlite/crosscheck/drivercheck/d_test.go": "package drivercheck\n\nimport (\n\t\"testing\"\n\n\troot \"" + module + "\"\n)\n\n" +
			"func TestGenerated(t *testing.T) {\n\tif root.Five() != 5 {\n\t\tt.Error(\"not 5\")\n\t}\n}\n\nfunc TestSeveral(t *testing.T) {\n\tif root.Four() != 4 {\n\t\tt.Error(\"not 4\")\n\t}\n}\n",
	}
	for name, text := range files {
		path := filepath.Join(root, name)
		is.NotError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		is.NotError(t, os.WriteFile(path, []byte(text), 0o644))
	}

	// The time limit of a stage is never reached by a mutant that ends, however slow the machine.
	r := &runner{root: root, out: t.TempDir(), factor: 1, minTime: time.Hour, memory: 4 << 30, procs: 1}
	is.NotError(t, r.plan([]string{"internal/sql/parser", "internal/sql/loop", "."}))

	t.Run("should plan a stage for the tests of the package and one for each package that depends on it", func(t *testing.T) {
		var got []string
		for _, s := range r.stages["internal/sql/parser"] {
			got = append(got, s.layer+" "+s.pkg+" "+strings.Join(s.tests, ","))
		}
		is.EqualSlice(t, []string{"hand-written internal/sql/parser TestAdd", "other packages internal/sql/other TestThree"}, got)
		is.Equal(t, 1, len(r.stages["internal/sql/loop"]))
	})

	t.Run("should plan stages for the tests of the package at the root, without those that need a real bucket, and of the driver check", func(t *testing.T) {
		var got []string
		for _, s := range r.stages["."] {
			got = append(got, s.layer+" "+s.pkg+" "+strings.Join(s.tests, ","))
		}
		is.EqualSlice(t, []string{"hand-written . TestFour", "generator " + driverCheckPkg + " TestGenerated",
			"other packages " + driverCheckPkg + " TestSeveral"}, got)
	})

	// A mutant that loops for good is over any time limit, so a short one tells it apart without
	// depending on the speed of the machine.
	r.stages["internal/sql/loop"][0].timeout = time.Second

	tests := []struct {
		name, file, from, to string
		status, layer        string
		detail               string
	}{
		{name: "should kill a mutant that a test notices, in the layer of that test", file: "internal/sql/parser/p.go",
			from: "a + b", to: "a - b", status: statusKilled, layer: layerHand, detail: "TestAdd: p_test.go:7: Add(1, 2) = -1"},
		{name: "should let a mutant that no test notices survive", file: "internal/sql/parser/p.go",
			from: "a + b", to: "b + a", status: statusSurvived},
		{name: "should give a mutant that does not compile as uncompiled", file: "internal/sql/parser/p.go",
			from: "a + b", to: `a + "b"`, status: statusUncompiled, detail: "mismatched types"},
		{name: "should give a mutant that runs over the time limit as timed out, in its layer", file: "internal/sql/loop/loop.go",
			from: "i++", to: "i--", status: statusTimeout, layer: layerHand, detail: "over the time limit of 1s"},
		{name: "should kill a mutant of the package at the root that its tests notice", file: "r.go",
			from: "2, 2", to: "2, 3", status: statusKilled, layer: layerHand, detail: "TestFour: r_test.go:7: not 4"},
		{name: "should kill a mutant of the package at the root that only the driver check notices, in its layer", file: "r.go",
			from: "return 5", to: "return 6", status: statusKilled, layer: layerGenerator, detail: "TestGenerated: d_test.go:11: not 5"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := mutantOf(t, root, test.file, test.from, test.to)
			res := r.run(t.Context(), m, t.TempDir())
			is.Equal(t, test.status, res.Status, res.Detail)
			is.Equal(t, test.layer, res.Layer)
			is.True(t, strings.Contains(res.Detail, test.detail), res.Detail)
			is.Equal(t, m.Key, res.Key)
			is.True(t, res.Build > 0, "no time of the builds")
			if test.status != statusUncompiled {
				is.True(t, len(res.Stages) > 0 && res.Stages[0].Layer == layerHand, "no time of the stages")
			}
		})
	}

	t.Run("should stop with an error on a mutant of a file that changed since it was made", func(t *testing.T) {
		m := mutantOf(t, root, "internal/sql/parser/p.go", "a + b", "a - b")
		m.sum[0]++
		res := r.run(t.Context(), m, t.TempDir())
		is.Equal(t, statusError, res.Status)
		is.True(t, strings.Contains(res.Detail, "changed since its mutants were made"), res.Detail)
	})
}

// TestEngineLayers checks the layers of the tests of the engine, of the evaluator, and of the pass
// after the parser against their test files.
func TestEngineLayers(t *testing.T) {
	tests := []struct {
		pkg     string
		layers  map[string]string
		targets int
	}{
		{pkg: enginePkg, layers: engineLayers, targets: 12},
		{pkg: exprPkg, layers: exprLayers, targets: 1},
		{pkg: stmtPkg, layers: stmtLayers, targets: 5},
	}
	for _, test := range tests {
		t.Run("should count the seeds of every fuzz target of "+test.pkg+" in the layer fuzz seeds", func(t *testing.T) {
			root, err := moduleRoot()
			is.NotError(t, err)
			files, err := filepath.Glob(filepath.Join(root, test.pkg, "*_test.go"))
			is.NotError(t, err)
			fuzz := regexp.MustCompile(`(?m)^func (Fuzz\w*)\(\w+ \*testing\.F\)`)
			targets := map[string]bool{}
			for _, f := range files {
				src, err := os.ReadFile(f)
				is.NotError(t, err)
				for _, m := range fuzz.FindAllStringSubmatch(string(src), -1) {
					targets[m[1]] = true
					is.Equal(t, layerFuzzSeeds, test.layers[m[1]], m[1]+" in "+filepath.Base(f))
				}
			}
			is.True(t, len(targets) >= test.targets, "too few fuzz targets")
			for name, layer := range test.layers {
				if layer == layerFuzzSeeds {
					is.True(t, targets[name], "no fuzz target of "+test.pkg+" is named "+name)
				}
			}
		})
	}

	t.Run("should run the differential test of the evaluator in the layer generator", func(t *testing.T) {
		is.Equal(t, layerGenerator, exprLayers["TestDifferential"])
	})

	t.Run("should run the differential test of the split in the layer generator", func(t *testing.T) {
		is.Equal(t, layerGenerator, stmtLayers["TestSplit_differential"])
	})
}

// mutantOf the file in root, relative to it, that changes its one occurrence of from to to.
func mutantOf(t *testing.T, root, file, from, to string) mutant {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, file))
	is.NotError(t, err)
	is.Equal(t, 1, strings.Count(string(src), from))
	i := strings.Index(string(src), from)
	return mutant{Pkg: filepath.Dir(file), File: file, Op: "test", From: from, To: to, Key: file + " " + from + " => " + to,
		edits: []edit{{start: i, end: i + len(from), text: to}}, sum: sha256.Sum256(src)}
}
