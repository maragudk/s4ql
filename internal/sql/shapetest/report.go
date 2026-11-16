package shapetest

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// Report of a run of [Check] over many texts.
type Report struct {
	// Texts is the number of texts.
	Texts int

	// NotChecked counts the texts that were not statements to check, by reason: those that do not
	// parse, and those on which SQLite does not say.
	NotChecked map[string]int

	// Kinds counts the statements that were checked, by kind and by the [Way] they were compared.
	Kinds map[string]map[Way]int

	// ByValues counts the statements, by kind, of which at least one expression was compared by
	// values, and Uncompared those that SQLite compared in no way but by an error: neither by
	// bytecode nor probes, nor by an expression compared by bytecode or values, or compared before.
	ByValues, Uncompared map[string]int

	// Declared counts the statements, by kind, of which what the tree declares was compared with what
	// SQLite made of the text: see [Result].Declared.
	Declared map[string]int

	// Reasons counts the statements that were compared by [SameError] or [Skipped], by the error or
	// the reason.
	Reasons map[Way]map[string]int

	// Exprs counts the expressions.
	Exprs ExprCounts

	// Forms counts the expressions that hold each form of expression, by how they were compared.
	Forms map[string]*ExprCounts

	// Faults holds the results with a fault, in the order of the texts.
	Faults []Result
}

// ExprCounts of the expressions, by how they were compared. An expression compared by both
// bytecode and values counts in both.
type ExprCounts struct {
	// All is the number of expressions, and Duplicates the number of those that their [Checker] had
	// compared before, which are not counted in the others.
	All, Duplicates int
	// Bytecode counts those with the same program, Values those with the same values.
	Bytecode, Values int
	// Skipped counts by the reason why the values were not compared.
	Skipped map[string]int
}

func (c *ExprCounts) add(r ExprResult) {
	c.All++
	if r.Duplicate {
		c.Duplicates++
		return
	}
	if r.Bytecode {
		c.Bytecode++
	}
	if r.Values > 0 {
		c.Values++
	}
	if r.Skipped != "" {
		if c.Skipped == nil {
			c.Skipped = map[string]int{}
		}
		c.Skipped[r.Skipped]++
	}
}

// Run [Check] on every text, in turn, with one [Checker]. SQLite serializes its allocations over
// the threads of a process, so goroutines would barely run in parallel; to check many texts in
// parallel, split them over processes and [Summarize] the results.
func Run(sqls []string) Report {
	var c Checker
	defer c.Close()
	results := make([]Result, len(sqls))
	for i, sql := range sqls {
		results[i] = c.Check(sql)
	}
	return Summarize(results)
}

// Summarize the results of [Check] in a report.
func Summarize(results []Result) Report {
	r := Report{
		Texts:      len(results),
		NotChecked: map[string]int{},
		Kinds:      map[string]map[Way]int{},
		ByValues:   map[string]int{},
		Uncompared: map[string]int{},
		Declared:   map[string]int{},
		Reasons:    map[Way]map[string]int{},
		Forms:      map[string]*ExprCounts{},
	}
	for _, res := range results {
		if res.NotChecked() {
			r.NotChecked[res.Reason]++
			continue
		}
		if res.Fault != "" {
			r.Faults = append(r.Faults, res)
			continue
		}
		if r.Kinds[res.Kind] == nil {
			r.Kinds[res.Kind] = map[Way]int{}
		}
		r.Kinds[res.Kind][res.Way]++
		if res.Way == SameError || res.Way == Skipped {
			if r.Reasons[res.Way] == nil {
				r.Reasons[res.Way] = map[string]int{}
			}
			r.Reasons[res.Way][res.Reason]++
		}
		if res.Declared {
			r.Declared[res.Kind]++
		}
		byValues, compared := false, res.Way == Bytecode || res.Way == Probes
		for _, e := range res.Exprs {
			byValues = byValues || e.Values > 0
			compared = compared || e.Bytecode || e.Values > 0
		}
		if byValues {
			r.ByValues[res.Kind]++
		}
		if !compared {
			r.Uncompared[res.Kind]++
		}
		for _, e := range res.Exprs {
			r.Exprs.add(e)
			if e.Duplicate {
				continue
			}
			for _, f := range e.Forms {
				if r.Forms[f] == nil {
					r.Forms[f] = &ExprCounts{}
				}
				r.Forms[f].add(e)
			}
		}
	}
	return r
}

// Compared is the number of statements compared by [Bytecode] or [Probes].
func (r Report) Compared() int {
	n := 0
	for _, ways := range r.Kinds {
		n += ways[Bytecode] + ways[Probes]
	}
	return n
}

// Checked is the number of statements that were checked, faults included.
func (r Report) Checked() int {
	n := len(r.Faults)
	for _, ways := range r.Kinds {
		for _, c := range ways {
			n += c
		}
	}
	return n
}

func (r Report) String() string {
	var b strings.Builder
	checked := r.Checked()
	fmt.Fprintf(&b, "%d texts, %d statements checked, %d with a fault\n", r.Texts, checked, len(r.Faults))
	for _, reason := range sortedKeys(r.NotChecked) {
		fmt.Fprintf(&b, "  not checked, %s: %d\n", reason, r.NotChecked[reason])
	}
	if checked > 0 {
		uncompared := 0
		for _, n := range r.Uncompared {
			uncompared += n
		}
		fmt.Fprintf(&b, "compared by bytecode or probes: %d (%.1f%%); compared in no way but by an error: %d (%.1f%%)\n",
			r.Compared(), 100*float64(r.Compared())/float64(checked), uncompared, 100*float64(uncompared)/float64(checked))
	}
	fmt.Fprintf(&b, "\n%-28s %8s %8s %8s %8s %8s %8s %8s %8s\n", "kind", "bytecode", "probes", "error", "skipped", "values", "none", "declared", "fault")
	faults := map[string]int{}
	for _, f := range r.Faults {
		faults[f.Kind]++
	}
	kinds := slices.Collect(maps.Keys(r.Kinds))
	for k := range faults {
		if !slices.Contains(kinds, k) {
			kinds = append(kinds, k)
		}
	}
	slices.Sort(kinds)
	for _, k := range kinds {
		w := r.Kinds[k]
		fmt.Fprintf(&b, "%-28s %8d %8d %8d %8d %8d %8d %8d %8d\n", k, w[Bytecode], w[Probes], w[SameError], w[Skipped], r.ByValues[k], r.Uncompared[k], r.Declared[k], faults[k])
	}
	for _, way := range []Way{SameError, Skipped} {
		reasons := r.Reasons[way]
		if len(reasons) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s, the most common reasons:\n", way)
		keys := sortedKeys(reasons)
		slices.SortStableFunc(keys, func(a, b string) int { return cmp.Compare(reasons[b], reasons[a]) })
		for i, k := range keys {
			if i == 15 {
				fmt.Fprintf(&b, "  and %d more\n", len(keys)-i)
				break
			}
			fmt.Fprintf(&b, "  %d: %s\n", reasons[k], k)
		}
	}
	e := r.Exprs
	fmt.Fprintf(&b, "\nexpressions: %d, of which %d seen before; of the others %d by bytecode, %d by values\n", e.All, e.Duplicates, e.Bytecode, e.Values)
	for _, k := range sortedKeys(e.Skipped) {
		fmt.Fprintf(&b, "  values not compared, %s: %d\n", k, e.Skipped[k])
	}
	fmt.Fprintf(&b, "\n%-32s %8s %8s %8s %8s\n", "form", "exprs", "bytecode", "values", "skipped")
	for _, f := range sortedKeys(r.Forms) {
		c := r.Forms[f]
		skipped := 0
		for _, n := range c.Skipped {
			skipped += n
		}
		fmt.Fprintf(&b, "%-32s %8d %8d %8d %8d\n", f, c.All, c.Bytecode, c.Values, skipped)
	}
	for i, f := range r.Faults {
		if i == 20 {
			fmt.Fprintf(&b, "\nand %d more faults\n", len(r.Faults)-i)
			break
		}
		fmt.Fprintf(&b, "\nfault: %q\n  printed: %q\n  %s\n", f.SQL, f.Printed, f.Fault)
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// Test runs [Run] on the texts, logs the report, and fails the test if any statement has a fault.
func Test(t testing.TB, sqls []string) Report {
	t.Helper()
	r := Run(sqls)
	t.Log("\n" + r.String())
	if len(r.Faults) > 0 {
		t.Errorf("%d statements with a fault in the shape of their tree", len(r.Faults))
	}
	return r
}
