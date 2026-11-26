package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// mergeShards merges the results of the shards in the directories that the pattern matches into the
// results of out, which has none yet and whose fingerprint must be that of every shard. Each result
// must be of a mutant of the selection mutants, and of one shard only. A shard without results
// counts: its mutants are left without a result, which the report shows, and so does the mutant of
// a last line that a shard did not finish, when it was stopped while it wrote.
func mergeShards(out, pattern string, mutants []mutant) error {
	if _, err := os.Stat(filepath.Join(out, "results.jsonl")); err == nil {
		return fmt.Errorf("%v holds results already; merge into a directory without them", out)
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	var dirs []string
	for _, d := range matches {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			dirs = append(dirs, d)
		}
	}
	if len(dirs) == 0 {
		return fmt.Errorf("no shard matches %v", pattern)
	}
	fp, err := os.ReadFile(filepath.Join(out, "fingerprint"))
	if err != nil {
		return err
	}
	selected := map[string]bool{}
	for _, m := range mutants {
		selected[m.Key] = true
	}
	merged := map[string]result{}
	from := map[string]string{}
	for _, d := range dirs {
		shardFP, err := os.ReadFile(filepath.Join(d, "fingerprint"))
		if err != nil {
			return fmt.Errorf("the shard in %v has no fingerprint: %w", d, err)
		}
		if !bytes.Equal(bytes.TrimSpace(shardFP), bytes.TrimSpace(fp)) {
			return fmt.Errorf("the shard in %v ran on other code or another SQLite or Go", d)
		}
		results, err := readShardResults(filepath.Join(d, "results.jsonl"))
		if err != nil {
			return err
		}
		for k, res := range results {
			if !selected[k] {
				return fmt.Errorf("the shard in %v has a result of a mutant that is not in the selection, so it ran another one: %v", d, k)
			}
			if other, ok := from[k]; ok {
				return fmt.Errorf("the mutant %v has a result in more than one shard: %v and %v", k, other, d)
			}
			from[k] = d
			merged[k] = res
		}
	}
	var b bytes.Buffer
	for _, k := range slices.Sorted(maps.Keys(merged)) {
		line, err := json.Marshal(merged[k])
		if err != nil {
			return err
		}
		b.Write(append(line, '\n'))
	}
	return os.WriteFile(filepath.Join(out, "results.jsonl"), b.Bytes(), 0o644)
}

// readShardResults reads the results of a shard at path, as [readResults] does, without its last
// line if that does not end in a newline: a shard that was stopped while it wrote a result.
func readShardResults(path string) (map[string]result, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]result{}, nil
	}
	if err != nil {
		return nil, err
	}
	whole := data[:bytes.LastIndexByte(data, '\n')+1]
	if len(whole) < len(data) {
		fmt.Fprintf(os.Stderr, "%v: the last line is not finished, so its mutant has no result\n", path)
	}
	return parseResults(path, whole)
}

// summaryCounts of a report: the mutants of the selection, those without a result, the survivors
// that the list of equivalent mutants does not explain, the mutants that timed out, and the entries
// of the list that match no surviving mutant.
type summaryCounts struct {
	mutants, pending, unexplained, timeouts, stale int
}

// writeSummaryFiles writes the summary of the results in Markdown into out/summary.md, and its
// counts into out/counts, one name=value a line, as GitHub Actions reads the outputs of a step.
func writeSummaryFiles(out string, mutants []mutant, all map[string]bool, results map[string]result, eq map[string]string) error {
	var b bytes.Buffer
	c := writeSummary(&b, mutants, all, results, eq)
	if err := os.WriteFile(filepath.Join(out, "summary.md"), b.Bytes(), 0o644); err != nil {
		return err
	}
	counts := fmt.Sprintf("mutants=%v\npending=%v\nunexplained=%v\ntimeouts=%v\nstale=%v\n", c.mutants, c.pending, c.unexplained, c.timeouts, c.stale)
	return os.WriteFile(filepath.Join(out, "counts"), []byte(counts), 0o644)
}

// writeSummary of the results of the mutants to w in Markdown, for the summary of a run of GitHub
// Actions and its issue: the counts per package, and what a builder acts on, with what it needs for
// each: the unexplained survivors, the mutants that timed out, which count as killed and may run
// again with a longer limit, the mutants without a result, and the stale entries of the list.
func writeSummary(w io.Writer, mutants []mutant, all map[string]bool, results map[string]result, eq map[string]string) summaryCounts {
	byPkg := map[string]*counts{}
	var total counts
	var unexplained, timeouts []mutant
	c := summaryCounts{mutants: len(mutants)}
	for _, m := range mutants {
		res, ok := results[m.Key]
		if !ok {
			c.pending++
			continue
		}
		_, isEq := eq[m.Key]
		isEq = isEq && res.Status == statusSurvived
		get(byPkg, m.Pkg).add(res, isEq)
		total.add(res, isEq)
		switch {
		case res.Status == statusSurvived && !isEq:
			unexplained = append(unexplained, m)
		case res.Status == statusTimeout:
			timeouts = append(timeouts, m)
		}
	}
	stale := staleEntries(mutants, all, results, eq)
	c.unexplained, c.timeouts, c.stale = len(unexplained), len(timeouts), len(stale)

	p := func(format string, args ...any) {
		_, _ = fmt.Fprintf(w, format, args...)
	}
	p("| Package | Generated | Not compiled | Killed | Timed out | Equivalent | Unexplained | Score w/o equivalent |\n")
	p("| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	row := func(name string, c counts) {
		_, ne := c.score()
		p("| %v | %v | %v | %v | %v | %v | %v | %.2f%% |\n", name, c.generated, c.uncompiled, c.killed, c.timeout, c.equivalent, c.unexplained, ne)
	}
	for _, pkg := range sortedKeys(byPkg) {
		row("`"+pkg+"`", *byPkg[pkg])
	}
	row("total", total)
	var killedBy []string
	for _, l := range layers {
		if n := total.byLayer[l]; n > 0 {
			killedBy = append(killedBy, fmt.Sprintf("%v %v", l, n))
		}
	}
	if len(killedBy) > 0 {
		p("\nThe layers that killed them first: %v.\n", strings.Join(killedBy, ", "))
	}
	if c.pending > 0 {
		p("\n%v mutants have no result: a shard failed or stopped before it ran them.\n", c.pending)
	}

	p("\n### %v unexplained survivors\n", len(unexplained))
	if len(unexplained) > 0 {
		p("\nKill each with a test, remove the code that nothing needs, or list it in `internal/sql/mutation/equivalent.txt` with the reason. The first line of each block is its key.\n")
	}
	for _, m := range unexplained {
		res := results[m.Key]
		p("\n`%v:%v`, in `%v`, operator `%v`:\n\n```text\n%v\n```\n\n```diff\n- %v\n+ %v\n```\n", res.File, res.Line, m.Func, res.Op, m.Key, res.From, res.To)
	}

	if len(timeouts) > 0 {
		p("\n### %v timed out\n\nThey count as killed. Each may run again alone with a longer limit, with `-run` and its `file:line`.\n\n", len(timeouts))
		for _, m := range timeouts {
			res := results[m.Key]
			p("- `%v:%v` in %v, %v\n", res.File, res.Line, res.Layer, res.Detail)
		}
		p("\n```text\n")
		for _, m := range timeouts {
			p("%v\n", m.Key)
		}
		p("```\n")
	}

	if len(stale) > 0 {
		p("\n### %v entries of the list of equivalent mutants match no surviving mutant\n\nIts mutant was killed, or the generator makes no mutant with that key. Remove the entry, or correct its key.\n\n```text\n", len(stale))
		for _, k := range stale {
			p("%v\n", k)
		}
		p("```\n")
	}
	return c
}
