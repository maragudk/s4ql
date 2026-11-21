package stmttest

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// RecountVariable is the environment variable that makes [Reached.Check] write its file of counts
// from the runs, where it checks the runs against the file otherwise: S4QL_RECOUNT=1.
const RecountVariable = "S4QL_RECOUNT"

// Recounting reports whether [RecountVariable] is set to 1.
func Recounting() bool {
	return os.Getenv(RecountVariable) == "1"
}

// Reached is what the runs of a test must reach: the kinds of outcome that are required, in a file
// that is written by hand, and how often each run reached each of them at the last count, in a file
// that [Reached.Check] writes. A run is a named set of sequences, such as those of a [Profile].
type Reached struct {
	// Kinds is the path of the file of the required kinds, one on a line. A line that begins with #
	// is a comment. The file holds one kind at least.
	Kinds string
	// Counts is the path of the file of the counts, which [Reached.Check] writes with
	// [RecountVariable] set to 1 and checks otherwise. A row is a run, what the run reached, the
	// least number that is required of it, a third, or = for a count that is pinned exactly, and the
	// kind.
	Counts string
	// Doc is the first line of the file of the counts, which says whose it is.
	Doc string
	// Exact reports whether the kind is pinned exactly: a run must give it as often as its row says,
	// and a run without a row of it not at all. It may be nil, for no such kind.
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
// number is not the third of its count fails the check: a lower one checks less than the file says,
// and a higher one has no margin. Each required kind must have a row in one run at least, so a kind
// that every run reaches fewer than three times fails the check and the recount, which then writes
// nothing, and the failure says which runs reach it how often. Each run must have a row at least,
// so a run that was added without a recount fails. Only a required kind and an exact kind have
// rows.
func (c Reached) Check(names []string, runs map[string]map[string]int) ([]string, error) {
	required, exact, err := c.kinds()
	if err != nil {
		return nil, err
	}
	if Recounting() {
		return c.recount(names, runs, required, exact)
	}
	rows, err := readCounts(c.Counts)
	if err != nil {
		return nil, err
	}
	var failures []string
	has := map[[2]string]bool{}
	ofRun, ofKind := map[string]bool{}, map[string]bool{}
	for _, row := range rows {
		has[[2]string{row.run, row.kind}] = true
		ofRun[row.run] = true
		got := runs[row.run][row.kind]
		switch {
		case !slices.Contains(names, row.run):
			failures = append(failures, fmt.Sprintf("%v has a row of the run %q, which the test does not have", c.Counts, row.run))
		case row.exact != exact(row.kind) || !row.exact && !slices.Contains(required, row.kind):
			failures = append(failures, fmt.Sprintf("%v has a row of %q that is exact: %v, where the kind is required: %v, and is exact: %v; write the file again with %v=1", c.Counts, row.kind, row.exact, slices.Contains(required, row.kind), exact(row.kind), RecountVariable))
		case row.exact && got != row.reached:
			failures = append(failures, fmt.Sprintf("%v gives %q %v times, and %v pins %v: change a pinned number only on purpose, with %v=1, and say why in the commit", row.run, row.kind, got, c.Counts, row.reached, RecountVariable))
		case !row.exact && (row.least < 1 || row.least != row.reached/3):
			failures = append(failures, fmt.Sprintf("%v: the least number %v of %q of %q, reached %v times, is not a third of it, or is 0: it checks nothing, has no margin, or was changed by hand", c.Counts, row.least, row.kind, row.run, row.reached))
		case !row.exact && got < row.least:
			failures = append(failures, fmt.Sprintf("%v reaches %q %v times, and at least %v were expected, a third of %v: something that the run did stopped happening", row.run, row.kind, got, row.least, row.reached))
		}
		if !row.exact {
			ofKind[row.kind] = true
		}
	}
	for _, kind := range required {
		if !ofKind[kind] {
			failures = append(failures, fmt.Sprintf("%v has no row of %q, which is required, in any run: %v", c.Counts, kind, reachedBy(names, runs, kind)))
		}
	}
	for _, name := range names {
		if !ofRun[name] {
			failures = append(failures, fmt.Sprintf("%v has no row of the run %q, so nothing is required of it: write the file again with %v=1", c.Counts, name, RecountVariable))
		}
		for _, kind := range sortedKinds(runs[name]) {
			if n := runs[name][kind]; n != 0 && exact(kind) && !has[[2]string{name, kind}] {
				failures = append(failures, fmt.Sprintf("%v gives %q %v times, and %v pins none", name, kind, n, c.Counts))
			}
		}
	}
	return failures, nil
}

// Least checks the least numbers of the rows of the runs named, and nothing else, and gives what
// fails: for runs that hold more sequences than those of the file of the counts, each of which they
// hold too, so that they reach each kind at least as often, and give an exact kind more often. It
// never writes the file.
func (c Reached) Least(names []string, runs map[string]map[string]int) ([]string, error) {
	rows, err := readCounts(c.Counts)
	if err != nil {
		return nil, err
	}
	var failures []string
	for _, row := range rows {
		if got := runs[row.run][row.kind]; !row.exact && slices.Contains(names, row.run) && got < row.least {
			failures = append(failures, fmt.Sprintf("%v reaches %q %v times, and at least %v were expected, a third of %v: something that the run did stopped happening", row.run, row.kind, got, row.least, row.reached))
		}
	}
	return failures, nil
}

// kinds gives the required kinds of the file of [Reached.Kinds], and [Reached.Exact], or a function
// that is false for every kind if it is nil.
func (c Reached) kinds() ([]string, func(string) bool, error) {
	required, err := readKinds(c.Kinds)
	if err != nil {
		return nil, nil, err
	}
	if len(required) == 0 {
		return nil, nil, fmt.Errorf("%v holds no kind, so nothing would be required", c.Kinds)
	}
	exact := c.Exact
	if exact == nil {
		exact = func(string) bool { return false }
	}
	for _, kind := range required {
		if exact(kind) {
			return nil, nil, fmt.Errorf("%v holds the kind %q, which is exact: an exact kind is pinned for each run that gives it, and is not required", c.Kinds, kind)
		}
	}
	return required, exact, nil
}

// reachedBy says how often each run reaches the kind.
func reachedBy(names []string, runs map[string]map[string]int, kind string) string {
	var by []string
	for _, name := range names {
		if n := runs[name][kind]; n > 0 {
			by = append(by, fmt.Sprint(name, " ", n))
		}
	}
	if by == nil {
		return "no run reaches it"
	}
	return "the runs reach it " + strings.Join(by, ", ") + " times"
}

// recount writes the file of the counts from the runs, as [Reached.Check] says, or gives the
// required kinds that no run reaches three times and the runs that would have no row, and writes
// nothing.
func (c Reached) recount(names []string, runs map[string]map[string]int, required []string, exact func(string) bool) ([]string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# %v\n", c.Doc)
	b.WriteString("# A row is a run, what the run reached when this file was written, the least number that is\n")
	b.WriteString("# required of it, a third, or = for a count that is pinned exactly, and the kind. The kinds are\n")
	fmt.Fprintf(&b, "# those of %v. Written with %v=1 in a full run: see stmttest.Reached.\n", c.Kinds, RecountVariable)
	b.WriteString("# Do not edit a number by hand.\n")
	var failures []string
	ofKind := map[string]bool{}
	for _, name := range names {
		rows := 0
		for _, kind := range sortedKinds(runs[name]) {
			n := runs[name][kind]
			if strings.ContainsAny(kind, "\t\n") || strings.ContainsAny(name, "\t\n") {
				return nil, fmt.Errorf("the kind %q of the run %q has a tab or a line break", kind, name)
			}
			switch {
			case exact(kind) && n != 0:
				fmt.Fprintf(&b, "%v\t%v\t=\t%v\n", name, n, kind)
				rows++
			case !exact(kind) && n >= 3 && slices.Contains(required, kind):
				fmt.Fprintf(&b, "%v\t%v\t%v\t%v\n", name, n, n/3, kind)
				ofKind[kind] = true
				rows++
			}
		}
		if rows == 0 {
			failures = append(failures, fmt.Sprintf("the run %q reaches no required kind three times and gives no exact kind, so nothing would be required of it; %v is not written", name, c.Counts))
		}
	}
	for _, kind := range required {
		if !ofKind[kind] {
			failures = append(failures, fmt.Sprintf("no run reaches %q, which is required, three times: %v; %v is not written", kind, reachedBy(names, runs, kind), c.Counts))
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
	if err := lines.Err(); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New(path + " holds no row, so nothing would be checked")
	}
	return rows, nil
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
