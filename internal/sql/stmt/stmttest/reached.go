package stmttest

import (
	"bufio"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// RecountVariable is the environment variable that makes [Reached.Check] write its file from the
// run, where it checks the run against the file otherwise: S4QL_RECOUNT=1.
const RecountVariable = "S4QL_RECOUNT"

// Reached is what the runs of a generated test must reach: the kinds of outcome that the test
// requires, which a builder writes, and how often each run reached each when the test last counted,
// which the test writes. A run is the sequences of a [Profile] in the test, or the seeds of an
// environment of a simulation.
type Reached struct {
	// Kinds is the path of the file of the kinds that the test requires, one on a line. A line that
	// begins with # is a comment. A builder adds the kinds of a new area here, and takes a kind out
	// only with a word in the commit on where else it is pinned.
	Kinds string
	// Counts is the path of the file of the counts, which the test writes with [RecountVariable]
	// set to 1 and checks otherwise. A row is a run, what the run reached, the least number that the
	// test requires of it, a third, or = for a count that is pinned exactly, and the kind.
	Counts string
	// Doc is the first line of the file of the counts, which says whose it is.
	Doc string
	// Exact reports whether the kind is pinned exactly, such as a kind of text that a test excuses
	// from its comparison: a run must give it as often as its row says, and a run without a row of it
	// not at all. It may be nil, for no such kind.
	Exact func(kind string) bool
}

// reachedRow is a row of the counts of a [Reached].
type reachedRow struct {
	run, kind      string
	reached, least int
	exact          bool
}

// Check what the runs reached, the counts of each kind by the name of the run, in the order names,
// and give what fails, or with [RecountVariable] set to 1 write the file of the counts from the
// runs.
//
// A run must reach each kind of its rows at least as often as the least number of the row, which is
// a third of what the run reached when the file was written, rounded down. A kind that a run reaches
// fewer than three times has no row of that run, so no least number is 0, and a row whose least
// number is 0 or more than a third of its count fails the check: such a number checks nothing, or
// has no margin. Each kind that the test requires must have a row in one run at least, so a kind
// that every run reaches fewer than three times fails the check and the recount, which then writes
// nothing: the generator stopped reaching it, or never did, and the test says which runs reach it
// how often. Only a kind that the test requires, and a kind that is exact, has rows.
func (c Reached) Check(names []string, runs map[string]map[string]int) ([]string, error) {
	required, err := readKinds(c.Kinds)
	if err != nil {
		return nil, err
	}
	exact := c.Exact
	if exact == nil {
		exact = func(string) bool { return false }
	}
	counts := func(name string) map[string]int { return runs[name] }
	if os.Getenv(RecountVariable) == "1" {
		return c.recount(names, counts, required, exact)
	}
	rows, err := readCounts(c.Counts)
	if err != nil {
		return nil, err
	}
	var failures []string
	has := map[[2]string]bool{}
	rowOf := map[string]bool{}
	for _, row := range rows {
		has[[2]string{row.run, row.kind}] = true
		got := counts(row.run)[row.kind]
		switch {
		case !slices.Contains(names, row.run):
			failures = append(failures, fmt.Sprintf("%v has a row of the run %q, which the test does not have", c.Counts, row.run))
		case row.exact != exact(row.kind) || !row.exact && !slices.Contains(required, row.kind):
			failures = append(failures, fmt.Sprintf("%v has a row of %q that is exact: %v, and the test requires the kind: %v, and has it exact: %v; write the file again with %v=1", c.Counts, row.kind, row.exact, slices.Contains(required, row.kind), exact(row.kind), RecountVariable))
		case row.exact && got != row.reached:
			failures = append(failures, fmt.Sprintf("%v gives %q %v times, and %v pins %v: change a pinned number only on purpose, with %v=1, and say why in the commit", row.run, row.kind, got, c.Counts, row.reached, RecountVariable))
		case !row.exact && (row.least < 1 || 3*row.least > row.reached):
			failures = append(failures, fmt.Sprintf("%v: the least number %v of %q of %q, reached %v times, is 0 or more than a third: it checks nothing, or has no margin", c.Counts, row.least, row.kind, row.run, row.reached))
		case !row.exact && got < row.least:
			failures = append(failures, fmt.Sprintf("%v reaches %q %v times, and at least %v were expected, a third of %v: something that the run did stopped happening", row.run, row.kind, got, row.least, row.reached))
		}
		if !row.exact {
			rowOf[row.kind] = true
		}
	}
	for _, kind := range required {
		if !rowOf[kind] {
			failures = append(failures, fmt.Sprintf("%v has no row of %q, which the test requires, in any run: %v", c.Counts, kind, reachedBy(names, counts, kind)))
		}
	}
	for _, name := range names {
		for _, kind := range sortedKinds(counts(name)) {
			if n := counts(name)[kind]; n != 0 && exact(kind) && !has[[2]string{name, kind}] {
				failures = append(failures, fmt.Sprintf("%v gives %q %v times, and %v pins none", name, kind, n, c.Counts))
			}
		}
	}
	return failures, nil
}

// reachedBy says how often each run reaches the kind.
func reachedBy(names []string, counts func(name string) map[string]int, kind string) string {
	var by []string
	for _, name := range names {
		if n := counts(name)[kind]; n > 0 {
			by = append(by, fmt.Sprint(name, " ", n))
		}
	}
	if by == nil {
		return "no run reaches it"
	}
	return "the runs reach it " + strings.Join(by, ", ") + " times"
}

// recount writes the file of the counts from the runs, as [Reached.Check] says, or gives the kinds
// that the test requires and that no run reaches three times, and writes nothing.
func (c Reached) recount(names []string, counts func(name string) map[string]int, required []string, exact func(string) bool) ([]string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# %v\n", c.Doc)
	b.WriteString("# A row is a run, what the run reached when this file was written, the least number that the\n")
	b.WriteString("# test requires of it, a third, or = for a count that is pinned exactly, and the kind. The kinds\n")
	fmt.Fprintf(&b, "# are those of %v. Written by the test with %v=1 in a full run: see\n", c.Kinds, RecountVariable)
	b.WriteString("# stmttest.Reached. Do not edit a number by hand.\n")
	rowOf := map[string]bool{}
	for _, name := range names {
		for _, kind := range sortedKinds(counts(name)) {
			n := counts(name)[kind]
			if strings.ContainsAny(kind, "\t\n") || strings.ContainsAny(name, "\t\n") {
				return nil, fmt.Errorf("the kind %q of the run %q has a tab or a line break", kind, name)
			}
			switch {
			case exact(kind) && n != 0:
				fmt.Fprintf(&b, "%v\t%v\t=\t%v\n", name, n, kind)
			case !exact(kind) && n >= 3 && slices.Contains(required, kind):
				fmt.Fprintf(&b, "%v\t%v\t%v\t%v\n", name, n, n/3, kind)
				rowOf[kind] = true
			}
		}
	}
	var failures []string
	for _, kind := range required {
		if !rowOf[kind] {
			failures = append(failures, fmt.Sprintf("no run reaches %q, which the test requires, three times: %v; %v is not written", kind, reachedBy(names, counts, kind), c.Counts))
		}
	}
	if len(failures) > 0 {
		return failures, nil
	}
	return nil, os.WriteFile(c.Counts, []byte(b.String()), 0o644)
}

// readKinds reads the kinds of the file at path, as [Reached.Kinds] says.
func readKinds(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var kinds []string
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		if line := lines.Text(); line != "" && !strings.HasPrefix(line, "#") {
			kinds = append(kinds, line)
		}
	}
	return kinds, lines.Err()
}

// readCounts reads the rows of the file at path, as [Reached.Counts] says.
func readCounts(path string) ([]reachedRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var rows []reachedRow
	lines := bufio.NewScanner(f)
	for n := 1; lines.Scan(); n++ {
		line := lines.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, "\t", 4)
		if len(fields) != 4 {
			return nil, fmt.Errorf("%v:%v: not a run, a count, a least number, and a kind", path, n)
		}
		row := reachedRow{run: fields[0], kind: fields[3], exact: fields[2] == "="}
		if row.reached, err = strconv.Atoi(fields[1]); err != nil {
			return nil, fmt.Errorf("%v:%v: %w", path, n, err)
		}
		if !row.exact {
			if row.least, err = strconv.Atoi(fields[2]); err != nil {
				return nil, fmt.Errorf("%v:%v: %w", path, n, err)
			}
		}
		rows = append(rows, row)
	}
	return rows, lines.Err()
}

// sortedKinds of the counts.
func sortedKinds(counts map[string]int) []string {
	kinds := make([]string, 0, len(counts))
	for kind := range counts {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return kinds
}
