package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const module = "maragu.dev/s4ql"

// Layers of the tests, in the order in which they run against a mutant. A mutant is killed by the
// first layer that fails.
const (
	layerHand      = "hand-written"
	layerCorpus    = "corpus"
	layerGenerator = "generator"
	layerLALR      = "LALR automaton"
	layerFuzzSeeds = "fuzz seeds"
	layerMutants   = "mutants"
	layerShape     = "shape check"
	layerSim       = "simulation"
	layerOther     = "other packages"
	// layerOwnership is not a stage of its own: a failure of the ownership check inside the tests of
	// the parser, with no disagreement with SQLite, is counted for it.
	layerOwnership = "ownership"
)

var layers = []string{layerHand, layerCorpus, layerOwnership, layerGenerator, layerLALR, layerFuzzSeeds, layerMutants, layerShape, layerSim, layerOther}

// parserLayers of the tests of package parser that are not hand-written.
var parserLayers = map[string]string{
	"TestParse_corpus":  layerCorpus,
	"TestGenerate":      layerGenerator,
	"TestRuleCoverage":  layerLALR,
	"FuzzParse":         layerFuzzSeeds,
	"FuzzGenerate":      layerFuzzSeeds,
	"TestParse_mutants": layerMutants,
}

// stmtLayers of the tests of package stmt that are not hand-written, when stmt is the mutated package.
var stmtLayers = map[string]string{
	"TestCheck_corpus":        layerCorpus,
	"TestAllowList":           layerCorpus,
	"TestGenerate_nearMisses": layerMutants,
	"TestCheck_grammar":       layerGenerator,
	"TestGenerate":            layerGenerator,
	"FuzzCheck":               layerFuzzSeeds,
	"FuzzGenerate":            layerFuzzSeeds,
	"TestCheck_corpusMutants": layerMutants,
	"TestGenerate_mutants":    layerMutants,
}

// engineLayers of the tests of package engine that are not hand-written, when engine is the mutated
// package.
var engineLayers = map[string]string{
	"TestDifferential":           layerGenerator,
	"FuzzDifferential":           layerFuzzSeeds,
	"FuzzDifferential_core":      layerFuzzSeeds,
	"FuzzTypes":                  layerFuzzSeeds,
	"FuzzTypes_core":             layerFuzzSeeds,
	"FuzzWhere":                  layerFuzzSeeds,
	"FuzzWhere_core":             layerFuzzSeeds,
	"FuzzSQL":                    layerFuzzSeeds,
	"FuzzSQL_core":               layerFuzzSeeds,
	"FuzzColumns":                layerFuzzSeeds,
	"FuzzColumns_core":           layerFuzzSeeds,
	"FuzzChanges":                layerFuzzSeeds,
	"FuzzChanges_core":           layerFuzzSeeds,
	"FuzzCreateIndex_core":       layerFuzzSeeds,
	"FuzzIndexes":                layerFuzzSeeds,
	"FuzzIndexes_core":           layerFuzzSeeds,
	"TestSimulation":             layerSim,
	"TestSimulation_plantedBugs": layerSim,
}

const (
	parserPkg = "internal/sql/parser"
	shapePkg  = "internal/sql/shapetest"
	stmtPkg   = "internal/sql/stmt"
	enginePkg = "internal/engine"
	exprPkg   = "internal/expr"
)

// exprLayers of the tests of package expr that are not hand-written, when expr is the mutated
// package.
var exprLayers = map[string]string{
	"TestDifferential": layerGenerator,
	"FuzzExpr":         layerFuzzSeeds,
}

// stage of the run of a mutant: the tests of one layer in one test binary.
type stage struct {
	layer string
	// pkg is the directory of the package of the tests, relative to the root of the module.
	pkg      string
	tests    []string
	baseline time.Duration
	timeout  time.Duration
	// first reports whether the stage holds the hand-written tests of the mutated package that
	// -first names, which run before its other hand-written tests.
	first bool
}

// result of a mutant.
type result struct {
	Key  string
	File string
	Line int
	Op   string
	From string
	To   string
	// Status is killed, survived, timeout, or uncompiled.
	Status string
	// Layer that killed the mutant, or in which it timed out.
	Layer string `json:",omitempty"`
	// Detail of the failure: the test that failed, and the start of the message or of the error of
	// the compiler.
	Detail  string `json:",omitempty"`
	Seconds float64
	// Build is the seconds spent building: the compile of the mutated package alone, and the test
	// binaries of the stages that ran.
	Build float64 `json:",omitempty"`
	// Stages that ran, in order, with the seconds of each.
	Stages []stageSeconds `json:",omitempty"`
}

// stageSeconds of one stage of a mutant.
type stageSeconds struct {
	Layer   string
	Pkg     string
	Seconds float64
}

const (
	statusKilled     = "killed"
	statusSurvived   = "survived"
	statusTimeout    = "timeout"
	statusUncompiled = "uncompiled"
	// statusError is a failure of the run itself, such as a test binary that does not start, or a
	// file that changed since its mutants were made. It is not a result: the run stops.
	statusError = "error"
)

// runner of the mutants.
type runner struct {
	root    string
	out     string
	factor  float64
	minTime time.Duration
	memory  int64
	procs   int
	gocache string
	// first names the hand-written tests of the mutated package that run first, or is nil.
	first *regexp.Regexp
	// noPrecheck leaves out the compile of the mutated package alone before the first test binary.
	noPrecheck bool
	// stages of the tests for each mutated package, in order.
	stages map[string][]stage
}

// testPackages of the module under internal whose test binaries depend on the package in pkg, as
// directories relative to the root, sorted. For the package at the root, ".", which no package under
// internal depends on, they are the root itself.
func (r *runner) testPackages(pkg string) ([]string, error) {
	patterns := []string{"./internal/..."}
	target := module + "/" + pkg
	if pkg == "." {
		patterns, target = []string{"."}, module
	}
	out, err := r.goCmd(context.Background(), append([]string{"list", "-test", "-f", "{{.ImportPath}}\t{{join .Deps \" \"}}"}, patterns...)...)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for line := range strings.Lines(string(out)) {
		path, deps, _ := strings.Cut(strings.TrimSpace(line), "\t")
		if !strings.HasSuffix(path, ".test") {
			continue
		}
		path = strings.TrimSuffix(path, ".test")
		if path == module+"/internal/sql/mutation" {
			continue
		}
		depends := path == target
		for d := range strings.FieldsSeq(deps) {
			if d == target {
				depends = true
			}
		}
		if depends {
			dir := strings.TrimPrefix(path, module+"/")
			if path == module {
				dir = "."
			}
			dirs = append(dirs, dir)
		}
	}
	slices.Sort(dirs)
	return slices.Compact(dirs), nil
}

// plan the stages for the mutants of each package in pkgs: build the test binaries of the code as it
// is, list their tests, and time each stage, which must pass.
func (r *runner) plan(pkgs []string) error {
	r.stages = map[string][]stage{}
	dir := filepath.Join(r.out, "baseline")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	built := map[string][]string{}
	for _, pkg := range pkgs {
		tps, err := r.testPackages(pkg)
		if err != nil {
			return err
		}
		var stages []stage
		for _, tp := range tps {
			tests, ok := built[tp]
			if !ok {
				bin := filepath.Join(dir, binName(tp))
				if msg, err := r.build(context.Background(), tp, "", bin); err != nil {
					return fmt.Errorf("building the tests of %v: %w\n%v", tp, err, msg)
				}
				out, err := exec.Command(bin, "-test.list", ".*").Output()
				if err != nil {
					return fmt.Errorf("listing the tests of %v: %w", tp, err)
				}
				for _, t := range strings.Fields(string(out)) {
					if (strings.HasPrefix(t, "Test") || strings.HasPrefix(t, "Fuzz")) && !outside[t] {
						tests = append(tests, t)
					}
				}
				built[tp] = tests
			}
			byLayer := map[string][]string{}
			var first []string
			for _, t := range tests {
				l := layerOther
				switch {
				case tp == parserPkg:
					l = layerHand
					if pl, ok := parserLayers[t]; ok {
						l = pl
					}
				case tp == shapePkg:
					l = layerShape
				case tp == pkg && pkg == stmtPkg:
					l = layerHand
					if sl, ok := stmtLayers[t]; ok {
						l = sl
					}
				case tp == pkg && pkg == enginePkg:
					l = layerHand
					if el, ok := engineLayers[t]; ok {
						l = el
					}
				case tp == pkg && pkg == exprPkg:
					l = layerHand
					if xl, ok := exprLayers[t]; ok {
						l = xl
					}
				case tp == pkg:
					l = layerHand
				}
				if tp == pkg && l == layerHand && r.first != nil && r.first.MatchString(t) {
					first = append(first, t)
					continue
				}
				byLayer[l] = append(byLayer[l], t)
			}
			for l, ts := range byLayer {
				stages = append(stages, stage{layer: l, pkg: tp, tests: ts})
			}
			if first != nil {
				stages = append(stages, stage{layer: layerHand, pkg: tp, tests: first, first: true})
			}
		}
		slices.SortFunc(stages, func(a, b stage) int {
			if d := slices.Index(layers, a.layer) - slices.Index(layers, b.layer); d != 0 {
				return d
			}
			// Among the hand-written tests, those of the mutated package run first, and among them those
			// that -first names.
			if (a.pkg == pkg) != (b.pkg == pkg) {
				if a.pkg == pkg {
					return -1
				}
				return 1
			}
			if a.first != b.first {
				if a.first {
					return -1
				}
				return 1
			}
			return strings.Compare(a.pkg, b.pkg)
		})
		for i := range stages {
			s := &stages[i]
			start := time.Now()
			res := r.runStage(context.Background(), filepath.Join(dir, binName(s.pkg)), *s, 10*time.Minute)
			if res.Status != statusSurvived {
				return fmt.Errorf("the tests of %v in %v fail on the code as it is: %v %v", s.layer, s.pkg, res.Status, res.Detail)
			}
			s.baseline = time.Since(start)
			s.timeout = max(r.minTime, time.Duration(r.factor*float64(s.baseline)))
			fmt.Fprintf(os.Stderr, "%v: stage %v in %v, %v tests, %v, time limit %v\n", pkg, s.layer, s.pkg, len(s.tests),
				s.baseline.Round(time.Millisecond), s.timeout.Round(time.Second))
		}
		r.stages[pkg] = stages
	}
	return nil
}

// outside are the tests that need something from outside the machine, which a run of the mutants
// does not have: a real bucket and its credentials. They are of the root, of internal/store/s3store,
// and of internal/store/s3test, which test a package that depends on internal/core.
var outside = map[string]bool{
	"TestOpen_realBucket":      true,
	"TestClient_conformance":   true,
	"TestClient_realBucket":    true,
	"TestClient_core":          true,
	"TestDeleteOld_realBucket": true,
}

// binName of the test binary of the package in pkg.
func binName(pkg string) string {
	if pkg == "." {
		return "root.test"
	}
	return strings.ReplaceAll(pkg, "/", "_") + ".test"
}

// goCmd runs the go command in the root of the module and gives its output.
func (r *runner) goCmd(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = r.root
	if r.gocache != "" {
		cmd.Env = append(os.Environ(), "GOCACHE="+r.gocache)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.Bytes(), err
}

// build the test binary of the package in pkg into bin, with the overlay file, if not empty, and
// give the output of the compiler.
func (r *runner) build(ctx context.Context, pkg, overlay, bin string) (string, error) {
	args := []string{"test", "-c", "-vet=off", "-o", bin}
	if overlay != "" {
		args = append(args, "-overlay", overlay)
	}
	out, err := r.goCmd(ctx, append(args, "./"+pkg)...)
	return string(out), err
}

// run the mutant m in the directory dir, which the caller owns, through the stages of its package.
func (r *runner) run(ctx context.Context, m mutant, dir string) result {
	start := time.Now()
	res := r.runIn(ctx, m, dir)
	res.Key, res.File, res.Line, res.Op, res.From, res.To = m.Key, m.File, m.Line, m.Op, m.From, m.To
	res.Seconds = time.Since(start).Seconds()
	return res
}

func (r *runner) runIn(ctx context.Context, m mutant, dir string) result {
	src, err := os.ReadFile(filepath.Join(r.root, m.File))
	if err != nil {
		return result{Status: statusError, Detail: err.Error()}
	}
	if sha256.Sum256(src) != m.sum {
		return result{Status: statusError, Detail: m.File + " changed since its mutants were made"}
	}
	mutated := filepath.Join(dir, filepath.Base(m.File))
	if err := os.WriteFile(mutated, m.apply(src), 0o644); err != nil {
		return result{Status: statusError, Detail: err.Error()}
	}
	overlay := filepath.Join(dir, "overlay.json")
	data, err := json.Marshal(map[string]map[string]string{"Replace": {filepath.Join(r.root, m.File): mutated}})
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(overlay, data, 0o644); err != nil {
		return result{Status: statusError, Detail: err.Error()}
	}
	var cost result
	if !r.noPrecheck {
		start := time.Now()
		msg, err := r.compile(ctx, m.Pkg, overlay)
		cost.Build += time.Since(start).Seconds()
		if err != nil {
			return result{Status: statusUncompiled, Detail: firstLines(msg, 3), Build: cost.Build}
		}
	}
	built := map[string]string{}
	defer func() {
		for _, bin := range built {
			_ = os.Remove(bin)
		}
	}()
	for _, s := range r.stages[m.Pkg] {
		bin, ok := built[s.pkg]
		if !ok {
			bin = filepath.Join(dir, binName(s.pkg))
			start := time.Now()
			msg, err := r.build(ctx, s.pkg, overlay, bin)
			cost.Build += time.Since(start).Seconds()
			if err != nil {
				return result{Status: statusUncompiled, Detail: firstLines(msg, 3), Build: cost.Build, Stages: cost.Stages}
			}
			built[s.pkg] = bin
		}
		start := time.Now()
		res := r.runStage(ctx, bin, s, s.timeout)
		cost.Stages = append(cost.Stages, stageSeconds{Layer: s.layer, Pkg: s.pkg, Seconds: time.Since(start).Seconds()})
		if res.Status != statusSurvived {
			res.Build, res.Stages = cost.Build, cost.Stages
			return res
		}
	}
	return result{Status: statusSurvived, Build: cost.Build, Stages: cost.Stages}
}

// compile the package in pkg alone, with the overlay file, without building a test binary, and give
// the output of the compiler: a mutant that does not compile is found for the cost of one package.
func (r *runner) compile(ctx context.Context, pkg, overlay string) (string, error) {
	out, err := r.goCmd(ctx, "build", "-overlay", overlay, "./"+pkg)
	return string(out), err
}

var (
	failLine  = regexp.MustCompile(`(?m)^\s*--- FAIL: (\S+)`)
	ownership = regexp.MustCompile(`is owned by no node|is owned by a node, but was not read|of the tree is .* but the token read is|has the span|owns an empty token|has no tokens, but its span|overlaps .* at|the empty .* is at`)
)

// runStage runs the tests of the stage s in the binary bin, with the time limit timeout.
func (r *runner) runStage(ctx context.Context, bin string, s stage, timeout time.Duration) result {
	ctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-test.run", "^("+strings.Join(s.tests, "|")+")$", "-test.failfast",
		"-test.timeout", timeout.String())
	cmd.Dir = filepath.Join(r.root, s.pkg)
	cmd.WaitDelay = 5 * time.Second
	if r.procs > 0 {
		cmd.Env = append(os.Environ(), fmt.Sprint("GOMAXPROCS=", r.procs))
	}
	var out head
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return result{Status: statusError, Detail: err.Error()}
	}
	overMemory := make(chan bool, 1)
	done := make(chan struct{})
	go func() {
		overMemory <- r.watchMemory(cmd.Process.Pid, done)
	}()
	err := cmd.Wait()
	close(done)
	if <-overMemory {
		return result{Status: statusTimeout, Layer: s.layer, Detail: fmt.Sprintf("over the memory limit of %v MiB", r.memory>>20)}
	}
	if err == nil {
		return result{Status: statusSurvived}
	}
	text := out.String()
	if ctx.Err() != nil || strings.Contains(text, "panic: test timed out") {
		return result{Status: statusTimeout, Layer: s.layer, Detail: "over the time limit of " + timeout.String()}
	}
	layer := s.layer
	if s.pkg == parserPkg && ownership.MatchString(text) && !strings.Contains(text, "disagrees with SQLite") {
		layer = layerOwnership
	}
	detail := ""
	if f := failLine.FindAllStringSubmatch(text, -1); len(f) > 0 {
		detail = f[len(f)-1][1] + ": "
	}
	return result{Status: statusKilled, Layer: layer, Detail: detail + firstFailure(text)}
}

// watchMemory of the process pid until done is closed, and kill it if its resident memory grows
// over the limit, which a mutant that loops while it allocates does. It reports whether it did.
func (r *runner) watchMemory(pid int, done <-chan struct{}) bool {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return false
		case <-tick.C:
		}
		out, err := exec.Command("ps", "-o", "rss=", "-p", fmt.Sprint(pid)).Output()
		if err != nil {
			continue
		}
		kib, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err == nil && kib*1024 > r.memory {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
			return true
		}
	}
}

// firstFailure gives the first line of the output of a test run that tells why it failed.
func firstFailure(out string) string {
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "=== ") || strings.HasPrefix(line, "--- FAIL") || line == "FAIL" {
			continue
		}
		return oneLine(line)
	}
	return ""
}

// firstLines of s, at most n, on one line.
func firstLines(s string, n int) string {
	var lines []string
	for line := range strings.Lines(s) {
		if strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, strings.TrimSpace(line))
		if len(lines) == n {
			break
		}
	}
	return oneLine(strings.Join(lines, " | "))
}

// head keeps the first 64 KiB written to it, which hold the first failure.
type head struct {
	buf bytes.Buffer
}

func (t *head) Write(p []byte) (int, error) {
	if room := 64<<10 - t.buf.Len(); room > 0 {
		t.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (t *head) String() string {
	return t.buf.String()
}

// readResults from the file of results at path, if it exists.
func readResults(path string) (map[string]result, error) {
	results := map[string]result{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return results, nil
	}
	if err != nil {
		return nil, err
	}
	return parseResults(path, data)
}

// parseResults of the file of results at path, whose content is data.
func parseResults(path string, data []byte) (map[string]result, error) {
	results := map[string]result{}
	for line := range strings.Lines(string(data)) {
		var res result
		if err := json.Unmarshal([]byte(line), &res); err != nil {
			return nil, fmt.Errorf("%v: %w", path, err)
		}
		// Keys are compared with single spaces between their words.
		res.Key = strings.Join(strings.Fields(res.Key), " ")
		results[res.Key] = res
	}
	return results, nil
}
