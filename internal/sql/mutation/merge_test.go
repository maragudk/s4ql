package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"maragu.dev/is"
)

// writeShard writes the directory of the results of a shard, with the fingerprint fp, into dir.
func writeShard(t *testing.T, dir, fp string, results ...result) {
	t.Helper()
	is.NotError(t, os.MkdirAll(dir, 0o755))
	is.NotError(t, os.WriteFile(filepath.Join(dir, "fingerprint"), []byte(fp+"\n"), 0o644))
	var lines []byte
	for _, r := range results {
		line, err := json.Marshal(r)
		is.NotError(t, err)
		lines = append(append(lines, line...), '\n')
	}
	is.NotError(t, os.WriteFile(filepath.Join(dir, "results.jsonl"), lines, 0o644))
}

func TestMergeShards(t *testing.T) {
	ms := []mutant{{Key: "a"}, {Key: "b"}, {Key: "c"}}

	t.Run("should merge the results of the shards into the results of the output", func(t *testing.T) {
		shards, out := t.TempDir(), t.TempDir()
		is.NotError(t, os.WriteFile(filepath.Join(out, "fingerprint"), []byte("fp\n"), 0o644))
		writeShard(t, filepath.Join(shards, "shard-1"), "fp", result{Key: "a", Status: statusKilled})
		writeShard(t, filepath.Join(shards, "shard-2"), "fp", result{Key: "b", Status: statusSurvived}, result{Key: "c", Status: statusTimeout})

		is.NotError(t, mergeShards(out, filepath.Join(shards, "*"), ms))
		results, err := readResults(filepath.Join(out, "results.jsonl"))
		is.NotError(t, err)
		is.Equal(t, 3, len(results))
		is.Equal(t, statusKilled, results["a"].Status)
		is.Equal(t, statusSurvived, results["b"].Status)
		is.Equal(t, statusTimeout, results["c"].Status)
	})

	t.Run("should merge a shard that has no results yet, whose mutants then have none", func(t *testing.T) {
		shards, out := t.TempDir(), t.TempDir()
		is.NotError(t, os.WriteFile(filepath.Join(out, "fingerprint"), []byte("fp\n"), 0o644))
		writeShard(t, filepath.Join(shards, "shard-1"), "fp", result{Key: "a", Status: statusKilled})
		is.NotError(t, os.MkdirAll(filepath.Join(shards, "shard-2"), 0o755))
		is.NotError(t, os.WriteFile(filepath.Join(shards, "shard-2", "fingerprint"), []byte("fp\n"), 0o644))

		is.NotError(t, mergeShards(out, filepath.Join(shards, "*"), ms))
		results, err := readResults(filepath.Join(out, "results.jsonl"))
		is.NotError(t, err)
		is.Equal(t, 1, len(results))
	})

	tests := []struct {
		name   string
		shards func(t *testing.T, dir string)
		want   string
	}{
		{name: "should refuse two shards with a result of the same mutant, which are not disjoint", want: "more than one shard", shards: func(t *testing.T, dir string) {
			writeShard(t, filepath.Join(dir, "shard-1"), "fp", result{Key: "a", Status: statusKilled})
			writeShard(t, filepath.Join(dir, "shard-2"), "fp", result{Key: "a", Status: statusKilled})
		}},
		{name: "should refuse a shard of other code", want: "other code", shards: func(t *testing.T, dir string) {
			writeShard(t, filepath.Join(dir, "shard-1"), "other", result{Key: "a", Status: statusKilled})
		}},
		{name: "should refuse a result of a mutant that is not in the selection", want: "not in the selection", shards: func(t *testing.T, dir string) {
			writeShard(t, filepath.Join(dir, "shard-1"), "fp", result{Key: "z", Status: statusKilled})
		}},
		{name: "should refuse a pattern that matches no shard", want: "no shard", shards: func(t *testing.T, dir string) {}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			shards, out := t.TempDir(), t.TempDir()
			is.NotError(t, os.WriteFile(filepath.Join(out, "fingerprint"), []byte("fp\n"), 0o644))
			test.shards(t, shards)
			err := mergeShards(out, filepath.Join(shards, "*"), ms)
			is.True(t, err != nil && strings.Contains(err.Error(), test.want), "error: "+errString(err))
		})
	}
}

func errString(err error) string {
	if err == nil {
		return "none"
	}
	return err.Error()
}

func TestWriteSummary(t *testing.T) {
	ms := []mutant{
		{Pkg: "internal/engine", File: "internal/engine/a.go", Func: "f", Key: "internal/engine/a.go f operator: x < 1 => x <= 1"},
		{Pkg: "internal/engine", File: "internal/engine/a.go", Key: "internal/engine/a.go f boolean: true => false"},
		{Pkg: "internal/engine", File: "internal/engine/a.go", Key: "internal/engine/a.go g negation: !ok => ok"},
		{Pkg: "internal/sql/stmt", File: "internal/sql/stmt/b.go", Key: "stmt/b.go h constant: 2 => 3"},
		{Pkg: "internal/sql/stmt", File: "internal/sql/stmt/b.go", Key: "stmt/b.go h constant: 2 => 1"},
		{Pkg: "internal/sql/stmt", File: "internal/sql/stmt/b.go", Key: "stmt/b.go h remove statement: x++ =>"},
	}
	results := map[string]result{
		ms[0].Key: {Key: ms[0].Key, File: ms[0].File, Line: 7, Op: "operator", From: "x < 1", To: "x <= 1", Status: statusSurvived},
		ms[1].Key: {Key: ms[1].Key, File: ms[1].File, Line: 9, Op: "boolean", From: "true", To: "false", Status: statusSurvived},
		ms[2].Key: {Key: ms[2].Key, File: ms[2].File, Line: 12, Op: "negation", From: "!ok", To: "ok", Status: statusTimeout, Layer: layerHand, Detail: "over the time limit of 1m0s"},
		ms[3].Key: {Key: ms[3].Key, File: ms[3].File, Line: 3, Op: "constant", From: "2", To: "3", Status: statusKilled, Layer: layerHand},
		ms[4].Key: {Key: ms[4].Key, File: ms[4].File, Line: 3, Op: "constant", From: "2", To: "1", Status: statusUncompiled},
	}
	all := map[string]bool{}
	for _, m := range ms {
		all[m.Key] = true
	}
	eq := map[string]string{ms[1].Key: "because", ms[3].Key: "it was", "stmt/b.go gone constant: 1 => 2": "gone"}

	var b bytes.Buffer
	c := writeSummary(&b, ms, all, results, eq)
	got := b.String()

	t.Run("should count the results", func(t *testing.T) {
		is.Equal(t, 6, c.mutants)
		is.Equal(t, 1, c.pending)
		is.Equal(t, 1, c.unexplained)
		is.Equal(t, 1, c.timeouts)
		is.Equal(t, 2, c.stale)
	})

	t.Run("should give a row of the table for each package and the total", func(t *testing.T) {
		is.True(t, strings.Contains(got, "| `internal/engine` | 3 | 0 | 0 | 1 | 1 | 1 |"), got)
		is.True(t, strings.Contains(got, "| `internal/sql/stmt` | 2 | 1 | 1 | 0 | 0 | 0 |"), got)
		is.True(t, strings.Contains(got, "| total | 5 | 1 | 1 | 1 | 1 | 1 |"), got)
	})

	t.Run("should give each unexplained survivor with its place, key, operator, and code before and after", func(t *testing.T) {
		is.True(t, strings.Contains(got, "`internal/engine/a.go:7`, in `f`, operator `operator`"), got)
		is.True(t, strings.Contains(got, "internal/engine/a.go f operator: x < 1 => x <= 1\n"), got)
		is.True(t, strings.Contains(got, "- x < 1\n+ x <= 1\n"), got)
		is.True(t, !strings.Contains(got, "boolean: true => false\n"), "a listed survivor is unexplained")
	})

	t.Run("should list the mutants that timed out, to run again", func(t *testing.T) {
		is.True(t, strings.Contains(got, "`internal/engine/a.go:12` in hand-written, over the time limit of 1m0s"), got)
	})

	t.Run("should list the mutants without a result, and the entries of the list that match no survivor", func(t *testing.T) {
		is.True(t, strings.Contains(got, "1 mutants have no result"), got)
		is.True(t, strings.Contains(got, "stmt/b.go h constant: 2 => 3\n"), got)
		is.True(t, strings.Contains(got, "stmt/b.go gone constant: 1 => 2\n"), got)
	})
}
