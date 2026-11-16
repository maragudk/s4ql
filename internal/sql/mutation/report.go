package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"text/tabwriter"
)

// readEquivalent reads the list of equivalent mutants: a key on a line of its own, and the reason
// on the indented line after it. Lines that start with # are comments.
func readEquivalent(p string) (map[string]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	eq := map[string]string{}
	key := ""
	s := bufio.NewScanner(f)
	n := 0
	for s.Scan() {
		n++
		line := s.Text()
		switch {
		case strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t"):
			if key == "" {
				return nil, fmt.Errorf("%v:%v: a reason without a key", p, n)
			}
			eq[key] = strings.TrimSpace(line)
			key = ""
		default:
			if key != "" {
				return nil, fmt.Errorf("%v:%v: the key %q has no reason", p, n-1, key)
			}
			if _, ok := eq[line]; ok {
				return nil, fmt.Errorf("%v:%v: the key %q is listed twice", p, n, line)
			}
			key = line
		}
	}
	if key != "" {
		return nil, fmt.Errorf("%v: the key %q has no reason", p, key)
	}
	return eq, s.Err()
}

// counts of the results of a group of mutants.
type counts struct {
	generated, uncompiled, killed, timeout, equivalent, unexplained int
	byLayer                                                         map[string]int
}

func (c *counts) add(res result, equivalent bool) {
	if c.byLayer == nil {
		c.byLayer = map[string]int{}
	}
	c.generated++
	switch res.Status {
	case statusUncompiled:
		c.uncompiled++
	case statusKilled:
		c.killed++
		c.byLayer[res.Layer]++
	case statusTimeout:
		c.timeout++
	case statusSurvived:
		if equivalent {
			c.equivalent++
		} else {
			c.unexplained++
		}
	}
}

// score is the share of the mutants that compile that the tests kill or that time out, and the same
// share without the equivalent mutants.
func (c counts) score() (all, nonEquivalent float64) {
	n := c.generated - c.uncompiled
	if n == 0 {
		return 0, 0
	}
	k := float64(c.killed + c.timeout)
	all = 100 * k / float64(n)
	if n-c.equivalent > 0 {
		nonEquivalent = 100 * k / float64(n-c.equivalent)
	}
	return all, nonEquivalent
}

// writeReport of the results of the mutants to w: the counts per package and per file, the
// survivors that the list of equivalent mutants does not explain, and the entries of that list that
// match no surviving mutant: an entry whose mutant was run and was not a survivor, or whose key is
// not in all, the keys of every mutant that the generator makes.
func writeReport(w io.Writer, mutants []mutant, all map[string]bool, results map[string]result, eq map[string]string) {
	byPkg, byFile := map[string]*counts{}, map[string]*counts{}
	var total counts
	var unexplained []result
	pending := 0
	for _, m := range mutants {
		res, ok := results[m.Key]
		if !ok {
			pending++
			continue
		}
		_, isEq := eq[m.Key]
		isEq = isEq && res.Status == statusSurvived
		for _, c := range []*counts{get(byPkg, m.Pkg), get(byFile, m.File), &total} {
			c.add(res, isEq)
		}
		if res.Status == statusSurvived && !isEq {
			unexplained = append(unexplained, res)
		}
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	header := "\tgenerated\tnot compiled\tkilled"
	for _, l := range layers {
		header += "\t" + l
	}
	header += "\ttimed out\tsurvived\tequivalent\tunexplained\tscore\tscore w/o equivalent\t"
	_, _ = fmt.Fprintln(tw, header)
	row := func(name string, c counts) {
		line := fmt.Sprintf("%v\t%v\t%v\t%v", name, c.generated, c.uncompiled, c.killed)
		for _, l := range layers {
			line += fmt.Sprintf("\t%v", c.byLayer[l])
		}
		all, ne := c.score()
		line += fmt.Sprintf("\t%v\t%v\t%v\t%v\t%.2f%%\t%.2f%%\t", c.timeout, c.equivalent+c.unexplained, c.equivalent, c.unexplained, all, ne)
		_, _ = fmt.Fprintln(tw, line)
	}
	for _, p := range sortedKeys(byPkg) {
		row(p, *byPkg[p])
		for _, f := range sortedKeys(byFile) {
			if path.Dir(f) == p {
				row("  "+path.Base(f), *byFile[f])
			}
		}
	}
	row("total", total)
	_ = tw.Flush()
	if pending > 0 {
		_, _ = fmt.Fprintf(w, "\n%v mutants have no result yet.\n", pending)
	}
	_, _ = fmt.Fprintf(w, "\n%v survivors are not in the list of equivalent mutants:\n", len(unexplained))
	for _, r := range unexplained {
		_, _ = fmt.Fprintf(w, "\n%v:%v\n%v\n", r.File, r.Line, r.Key)
	}
	selected := map[string]bool{}
	for _, m := range mutants {
		selected[m.Key] = true
	}
	var stale []string
	for k := range eq {
		res, ok := results[k]
		if !all[k] || selected[k] && ok && res.Status != statusSurvived {
			stale = append(stale, k)
		}
	}
	slices.Sort(stale)
	if len(stale) > 0 {
		_, _ = fmt.Fprintf(w, "\n%v entries of the list of equivalent mutants match no surviving mutant:\n", len(stale))
		for _, k := range stale {
			_, _ = fmt.Fprintf(w, "%v\n", k)
		}
	}
}

// get the counts of the key k of m, made if needed.
func get(m map[string]*counts, k string) *counts {
	if m[k] == nil {
		m[k] = &counts{}
	}
	return m[k]
}

func sortedKeys[V any](m map[string]V) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
