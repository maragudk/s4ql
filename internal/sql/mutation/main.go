// Command mutation runs mutation testing of the SQL tokenizer, syntax tree, parser, the pass after
// the parser, and the engine: it changes their code on purpose, one small change at a time, runs the
// tests against each change, and reports the changes that no test notices.
//
// Usage, from the root of the module:
//
//	go run ./internal/sql/mutation -out DIR
//	go run ./internal/sql/mutation -out DIR -pkgs internal/sql/parser -run 'expr.go'
//	go run ./internal/sql/mutation -list
//	go run ./internal/sql/mutation -out DIR -report
//
// Each mutant is a copy of one file with one change, which the go command compiles in place of the
// file through -overlay, so the repository is never changed and mutants run in parallel. The tests
// run in stages, in an order that puts the fast layers first, each with a time limit of a multiple
// of its time on the code as it is. The results go to DIR/results.jsonl, and a run with the same DIR skips the mutants that
// already have a result. The report, in DIR/report.txt, counts the mutants by package and by file,
// and lists each survivor that is not in the list of equivalent mutants in equivalent.txt.
//
// The README next to this file describes the operators, the stages, and the list.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

func main() {
	if err := start(); err != nil {
		fmt.Fprintln(os.Stderr, "mutation:", err)
		os.Exit(1)
	}
}

// allPkgs are the packages that the command mutates by default.
var allPkgs = []string{"internal/sql/token", "internal/sql/ast", "internal/sql/parser", "internal/sql/stmt", "internal/engine"}

// listedPkgs are the packages whose mutants the list of equivalent mutants names: those of
// [allPkgs], and the core, which a run mutates only when -pkgs names it.
var listedPkgs = append(slices.Clone(allPkgs), "internal/core")

func start() error {
	pkgs := flag.String("pkgs", strings.Join(allPkgs, ","), "packages to mutate, separated by commas")
	out := flag.String("out", "", "directory of the results (required, unless -list)")
	jobs := flag.Int("j", runtime.NumCPU(), "mutants to run at once")
	match := flag.String("run", "", "run only the mutants whose key or file:line matches this regular expression")
	list := flag.Bool("list", false, "list the mutants and exit")
	reportOnly := flag.Bool("report", false, "write the report of the results in -out and exit")
	factor := flag.Float64("factor", 20, "time limit of each stage, as a multiple of its time on the code as it is")
	minTime := flag.Duration("min-time", time.Minute, "smallest time limit of a stage")
	memory := flag.Int64("memory", 4<<30, "limit in bytes of the resident memory of a test binary")
	procs := flag.Int("procs", 2, "GOMAXPROCS of each test binary, or 0 to leave it unset")
	gocache := flag.String("gocache", "", "GOCACHE for the builds of the mutants, or empty for the usual one; each mutant adds about 3 MB to it")
	equivalent := flag.String("equivalent", "internal/sql/mutation/equivalent.txt", "the list of equivalent mutants")
	flag.Parse()

	root, err := moduleRoot()
	if err != nil {
		return err
	}
	dirs := strings.Split(*pkgs, ",")
	u, err := newUniverse(root, append(slices.Clone(listedPkgs), dirs...))
	if err != nil {
		return err
	}
	var re *regexp.Regexp
	if *match != "" {
		if re, err = regexp.Compile(*match); err != nil {
			return err
		}
	}
	// all holds the keys of the mutants of every package, which the report needs to tell an entry of
	// the list of equivalent mutants that names no mutant from one outside the selection.
	var mutants []mutant
	all := map[string]bool{}
	for _, d := range slices.Compact(slices.Sorted(slices.Values(append(slices.Clone(listedPkgs), dirs...)))) {
		selected := slices.Contains(dirs, d)
		files, err := goFiles(filepath.Join(root, d))
		if err != nil {
			return err
		}
		for _, f := range files {
			rel, _ := filepath.Rel(root, f)
			ms, err := u.generate(root, d, rel)
			if err != nil {
				return err
			}
			for _, m := range ms {
				all[m.Key] = true
				if selected && (re == nil || re.MatchString(m.Key) || re.MatchString(fmt.Sprintf("%v:%v", m.File, m.Line))) {
					mutants = append(mutants, m)
				}
			}
		}
	}
	if *list {
		for _, m := range mutants {
			fmt.Printf("%v:%v\t%v\n", m.File, m.Line, m.Key)
		}
		fmt.Fprintf(os.Stderr, "%v mutants\n", len(mutants))
		return nil
	}
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	if *out, err = filepath.Abs(*out); err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	eq, err := readEquivalent(filepath.Join(root, *equivalent))
	if err != nil {
		return err
	}
	if err := checkFingerprint(root, *out); err != nil {
		return err
	}
	resultsPath := filepath.Join(*out, "results.jsonl")
	results, err := readResults(resultsPath)
	if err != nil {
		return err
	}
	writeReportFile := func() error {
		f, err := os.Create(filepath.Join(*out, "report.txt"))
		if err != nil {
			return err
		}
		writeReport(f, mutants, all, results, eq)
		if err := f.Close(); err != nil {
			return err
		}
		writeReport(os.Stdout, mutants, all, results, eq)
		return nil
	}
	if *reportOnly {
		return writeReportFile()
	}

	var todo []mutant
	for _, m := range mutants {
		if _, ok := results[m.Key]; !ok {
			todo = append(todo, m)
		}
	}
	fmt.Fprintf(os.Stderr, "%v mutants, %v with a result already, %v to run with %v at once\n",
		len(mutants), len(mutants)-len(todo), len(todo), *jobs)
	r := &runner{root: root, out: *out, factor: *factor, minTime: *minTime, memory: *memory, procs: *procs, gocache: *gocache}
	began := time.Now()
	if err := r.plan(dirs); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "planned in %v\n", time.Since(began).Round(time.Second))

	rf, err := os.OpenFile(resultsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	work := make(chan mutant)
	done := make(chan result)
	var wg sync.WaitGroup
	for w := range *jobs {
		dir := filepath.Join(*out, "work", fmt.Sprint(w))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		wg.Go(func() {
			for m := range work {
				done <- r.run(context.Background(), m, dir)
			}
		})
	}
	go func() {
		for _, m := range todo {
			work <- m
		}
		close(work)
		wg.Wait()
		close(done)
	}()
	n := 0
	for res := range done {
		if res.Status == statusError {
			return fmt.Errorf("%v: %v", res.Key, res.Detail)
		}
		n++
		results[res.Key] = res
		line, err := json.Marshal(res)
		if err != nil {
			return err
		}
		if _, err := rf.Write(append(line, '\n')); err != nil {
			return err
		}
		if res.Status == statusSurvived || n%50 == 0 {
			fmt.Fprintf(os.Stderr, "%v/%v %v: %v %v:%v %v\n", n, len(todo), time.Since(began).Round(time.Second),
				res.Status, res.File, res.Line, res.Key)
		}
	}
	if err := rf.Close(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "ran %v mutants in %v\n", len(todo), time.Since(began).Round(time.Second))
	return writeReportFile()
}

// moduleRoot is the directory of the go.mod of the module in which the command runs.
func moduleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", err
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("not in a module")
	}
	return filepath.Dir(gomod), nil
}

// checkFingerprint of what the results in out depend on against the one that they were made with,
// and record it if out has none, so that results of other code are never reused: the files under
// internal but this command, among them the binding of SQLite in internal/sqlite, which judges, go.mod,
// go.sum, and the version of Go.
func checkFingerprint(root, out string) error {
	h := sha256.New()
	version, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		return err
	}
	_, _ = h.Write(version)
	add := func(path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		_, _ = fmt.Fprintf(h, "%v %v\n", rel, len(data))
		_, _ = h.Write(data)
		return nil
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		if err := add(filepath.Join(root, name)); err != nil {
			return err
		}
	}
	err = filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && path == filepath.Join(root, "internal", "sql", "mutation"):
			return filepath.SkipDir
		case d.IsDir():
			return nil
		}
		return add(path)
	})
	if err != nil {
		return err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	path := filepath.Join(out, "fingerprint")
	old, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.WriteFile(path, []byte(sum+"\n"), 0o644)
	case err != nil:
		return err
	case strings.TrimSpace(string(old)) != sum:
		return fmt.Errorf("the results in %v are of other code or another SQLite or Go; remove the directory or use another -out", out)
	}
	return nil
}
