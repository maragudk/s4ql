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

// slow reports whether the tests run with -short, the race detector, or the address sanitizer, which
// make them several times slower: the tests on the corpus then use every tenth entry.
func slow() bool {
	return testing.Short() || raceEnabled || asanEnabled
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
		return shapetest.Report{}, false
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
	return shapetest.Summarize(results), true
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
