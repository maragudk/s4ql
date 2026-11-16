package shapetest_test

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/corpus"
	"maragu.dev/s4ql/internal/sql/shapetest"
)

func slow() bool {
	return shapetest.Slow()
}

func corpusSQL(t testing.TB) []string {
	t.Helper()
	var sqls []string
	for _, e := range corpus.All() {
		sqls = append(sqls, e.SQL)
	}
	if slow() {
		var some []string
		for i := 0; i < len(sqls); i += 10 {
			some = append(some, sqls[i])
		}
		sqls = some
	}
	return sqls
}

// shards that check the texts of a test, each in a process of its own. SQLite serializes its
// allocations over the threads of a process, so goroutines barely run in parallel, and processes
// do. The number is fixed, so that the report, which counts an expression as seen before within a
// shard, is the same on every machine.
const shards = 8

// checkSharded checks the texts in [shards] processes that run the test again, and gives the report
// of all results. In such a process it checks its share of the texts, writes the results to a file,
// and returns false; the caller then returns at once.
func checkSharded(t *testing.T, sqls []string) (shapetest.Report, bool) {
	t.Helper()
	r, _, ok := checkShardedResults(t, sqls)
	return r, ok
}

// checkShardedResults is [checkSharded], with the results of the texts in their order. Under
// -short, the race detector, and the address sanitizer, which check a tenth of the texts and need
// much more memory for each process, it checks them in this process.
func checkShardedResults(t *testing.T, sqls []string) (shapetest.Report, []shapetest.Result, bool) {
	t.Helper()
	if slow() {
		var c shapetest.Checker
		defer c.Close()
		results := make([]shapetest.Result, len(sqls))
		for i, sql := range sqls {
			results[i] = c.Check(sql)
		}
		return shapetest.Summarize(results), results, true
	}
	if shard := os.Getenv("S4QL_SHAPETEST_SHARD"); shard != "" {
		i, err := strconv.Atoi(shard)
		is.NotError(t, err)
		var c shapetest.Checker
		defer c.Close()
		var results []shapetest.Result
		for j := i; j < len(sqls); j += shards {
			results = append(results, c.Check(sqls[j]))
		}
		var data bytes.Buffer
		is.NotError(t, gob.NewEncoder(&data).Encode(results))
		is.NotError(t, os.WriteFile(os.Getenv("S4QL_SHAPETEST_OUT"), data.Bytes(), 0o600))
		return shapetest.Report{}, nil, false
	}
	dir := t.TempDir()
	cmds := make([]*exec.Cmd, shards)
	outputs := make([]strings.Builder, shards)
	for i := range shards {
		args := []string{"-test.run=^" + t.Name() + "$", "-test.count=1"}
		if testing.Short() {
			args = append(args, "-test.short")
		}
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env = append(os.Environ(), fmt.Sprint("S4QL_SHAPETEST_SHARD=", i), "S4QL_SHAPETEST_OUT="+filepath.Join(dir, fmt.Sprint(i)))
		cmd.Stdout, cmd.Stderr = &outputs[i], &outputs[i]
		is.NotError(t, cmd.Start())
		cmds[i] = cmd
	}
	results := make([]shapetest.Result, len(sqls))
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("shard %d: %v\n%s", i, err, outputs[i].String())
		}
		data, err := os.ReadFile(filepath.Join(dir, fmt.Sprint(i)))
		is.NotError(t, err)
		var part []shapetest.Result
		is.NotError(t, gob.NewDecoder(bytes.NewReader(data)).Decode(&part))
		for j, r := range part {
			results[i+j*shards] = r
		}
	}
	return shapetest.Summarize(results), results, true
}

// TestCheck_corpus checks the shape of the tree of every statement of the corpus that parses, or of
// every tenth entry under -short, the race detector, and the address sanitizer.
func TestCheck_corpus(t *testing.T) {
	sqls := corpusSQL(t)
	r, ok := checkSharded(t, sqls)
	if !ok {
		return
	}
	t.Log("\n" + r.String())
	is.Equal(t, len(sqls), r.Texts)
	is.Equal(t, 0, len(r.Faults))
}

// TestCheck_statements checks the statements of [shapetest.Structure], and requires that each is
// compared by bytecode or probes, not skipped.
func TestCheck_statements(t *testing.T) {
	var c shapetest.Checker
	defer c.Close()
	for _, sql := range shapetest.Structure {
		t.Run(sql, func(t *testing.T) {
			r := c.Check(sql)
			is.Equal(t, "", r.Fault)
			if r.Way != shapetest.Bytecode && r.Way != shapetest.Probes {
				t.Errorf("not compared: %v %v", r.Way, r.Reason)
			}
		})
	}
}

// FuzzCheck checks the shape of the tree of any text that the parser parses, and what the tree
// declares. It is seeded with every 25th entry of the corpus, or every 100th under -short, the race
// detector, and the address sanitizer, the statements of [shapetest.Structure] and
// [shapetest.Declared], and a sample of the generated pairs.
func FuzzCheck(f *testing.F) {
	step := 25
	if slow() {
		step = 100
	}
	for i, e := range corpus.All() {
		if i%step == 0 {
			f.Add(e.SQL)
		}
	}
	for _, sql := range shapetest.Structure {
		f.Add(sql)
	}
	for _, sql := range shapetest.Declared {
		f.Add(sql)
	}
	for i, g := range shapetest.Pairs(5, 1) {
		if i%20 == 0 {
			f.Add(g.SQL)
		}
	}
	f.Fuzz(func(t *testing.T, src string) {
		if r := shapetest.Check(src); r.Fault != "" {
			t.Fatalf("%q printed as %q: %s", src, r.Printed, r.Fault)
		}
	})
}
