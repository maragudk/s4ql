package stmttest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

func TestReached_Check(t *testing.T) {
	// reached of the kinds and the counts given, in files of their own.
	reached := func(t *testing.T, kinds, counts string) stmttest.Reached {
		t.Helper()
		dir := t.TempDir()
		r := stmttest.Reached{Kinds: filepath.Join(dir, "kinds"), Counts: filepath.Join(dir, "counts"), Doc: "A test.",
			Exact:   func(kind string) bool { return strings.HasPrefix(kind, "excused: ") },
			Totaled: func(run string) bool { return !strings.Contains(run, "/") }}
		is.NotError(t, os.WriteFile(r.Kinds, []byte(kinds), 0o644))
		if counts != "" {
			is.NotError(t, os.WriteFile(r.Counts, []byte(counts), 0o644))
		}
		return r
	}
	// The runs reach "rows found" 12 times in all, and "no row found" 32 times.
	const kinds = "# What must be reached.\n5\trows found\n12\tno row found\n"
	const counts = "# A test.\na\t9\t3\trows found\na\t4\t=\texcused: long\nb\t30\t10\tno row found\nb\t3\t1\trows found\n"
	runs := func() map[string]map[string]int {
		return map[string]map[string]int{
			"a": {"rows found": 9, "no row found": 2, "excused: long": 4, "another kind": 100},
			"b": {"rows found": 3, "no row found": 30},
		}
	}

	tests := []struct {
		name string
		// kinds and counts are the two files, and change changes the runs of [runs].
		kinds, counts string
		names         []string
		change        func(runs map[string]map[string]int)
		// failures are a part of each failure, in their order, and err a part of the error.
		failures []string
		err      string
	}{
		{name: "should pass for runs that reach what the file says, and more of a kind that is not exact", kinds: kinds, counts: counts,
			change: func(runs map[string]map[string]int) { runs["a"]["rows found"] = 900 }},
		{name: "should pass for a run that reaches a kind as often as its least number", kinds: kinds, counts: counts,
			change: func(runs map[string]map[string]int) { runs["a"]["rows found"], runs["b"]["no row found"] = 3, 10 }},
		{name: "should fail for a run that reaches a kind less often than its least number", kinds: kinds, counts: counts,
			change:   func(runs map[string]map[string]int) { runs["a"]["rows found"] = 2 },
			failures: []string{`a reaches "rows found" 2 times, and at least 3 were expected`}},
		{name: "should fail for an exact kind that a run gives more often, and less often", kinds: kinds, counts: counts,
			change:   func(runs map[string]map[string]int) { runs["a"]["excused: long"] = 5 },
			failures: []string{`a gives "excused: long" 5 times`}},
		{name: "should fail for an exact kind that a run without a row of it gives", kinds: kinds, counts: counts,
			change:   func(runs map[string]map[string]int) { runs["b"]["excused: long"] = 1 },
			failures: []string{`b gives "excused: long" 1 times, and`}},
		{name: "should fail for a least number of 0", kinds: kinds, counts: strings.Replace(counts, "a\t9\t3\t", "a\t2\t0\t", 1),
			failures: []string{`the least number 0 of "rows found" of "a", reached 2 times`}},
		{name: "should fail for a least number above a third of its count", kinds: kinds, counts: strings.Replace(counts, "a\t9\t3\t", "a\t9\t4\t", 1),
			failures: []string{`the least number 4 of "rows found" of "a", reached 9 times`}},
		{name: "should fail for a least number below a third of its count, as one lowered by hand is", kinds: kinds, counts: strings.Replace(counts, "a\t9\t3\t", "a\t900\t1\t", 1),
			failures: []string{`the least number 1 of "rows found" of "a", reached 900 times`}},
		{name: "should fail for a total below the least number of its kind", kinds: kinds, counts: counts,
			change:   func(runs map[string]map[string]int) { runs["a"]["no row found"], runs["b"]["no row found"] = 0, 11 },
			failures: []string{`the runs reach "no row found" 11 times in all, and`}},
		{name: "should fail for a row lowered and the other rows of its kind deleted, as a recount after the kind fell writes them, by the least number of its total",
			kinds: kinds, counts: strings.Replace(strings.Replace(counts, "a\t9\t3\t", "a\t3\t1\t", 1), "b\t3\t1\trows found\n", "", 1),
			change:   func(runs map[string]map[string]int) { runs["a"]["rows found"], runs["b"]["rows found"] = 3, 0 },
			failures: []string{`the runs reach "rows found" 3 times in all, and`}},
		{name: "should add only the runs that are totaled to a total", kinds: kinds, counts: counts + "a/c\t3\t1\trows found\n", names: []string{"a", "b", "a/c"},
			change: func(runs map[string]map[string]int) {
				runs["a/c"] = map[string]int{"rows found": 3}
				runs["a"]["rows found"], runs["b"]["rows found"] = 3, 1
			},
			failures: []string{`the runs reach "rows found" 4 times in all`}},
		{name: "should fail for a required kind without a row in any run, and say which runs reach it", kinds: kinds + "1\tthree bounds\n", counts: counts,
			change:   func(runs map[string]map[string]int) { runs["b"]["three bounds"] = 2 },
			failures: []string{`has no row of "three bounds", which is required, in any run: the runs reach it b 2 times`}},
		{name: "should fail for a run without a row, of which nothing is required", kinds: kinds, counts: counts, names: []string{"a", "b", "c"},
			failures: []string{`has no row of the run "c"`}},
		{name: "should fail for a row of a run that the test does not have", kinds: kinds, counts: counts, names: []string{"a"},
			failures: []string{`has a row of the run "b"`, `has a row of the run "b"`, `the runs reach "no row found" 2 times in all`}},
		{name: "should fail for a row of a kind that is neither required nor exact", kinds: kinds, counts: counts + "b\t6\t2\tanother kind\n",
			failures: []string{`has a row of "another kind"`}},
		{name: "should fail for a row that is exact of a kind that is not", kinds: kinds, counts: strings.Replace(counts, "b\t3\t1\t", "b\t3\t=\t", 1),
			failures: []string{`has a row of "rows found" that is exact: true`}},
		{name: "should give an error for a file of kinds without a kind", kinds: "# None.\n", counts: counts, err: "holds no kind"},
		{name: "should give an error for a required kind that is exact", kinds: kinds + "1\texcused: long\n", counts: counts, err: "which is exact"},
		{name: "should give an error for a kind without a least number", kinds: kinds + "three bounds\n", counts: counts, err: "not a least number, a tab, and a kind"},
		{name: "should give an error for a least number of 0", kinds: kinds + "0\tthree bounds\n", counts: counts, err: "is not 1 at least"},
		{name: "should give an error for a kind twice", kinds: kinds + "1\trows found\n", counts: counts, err: "twice"},
		{name: "should give an error for a file of counts without a row", kinds: kinds, counts: "# None.\n", err: "holds no row"},
		{name: "should give an error for a file of counts that is not there", kinds: kinds, err: "no such file"},
		{name: "should give an error for a row that is not four fields", kinds: kinds, counts: "a\t9\trows found\n", err: "not a run, a count, a least number, and a kind"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(stmttest.RecountVariable, "")
			runs := runs()
			if test.change != nil {
				test.change(runs)
			}
			names := test.names
			if names == nil {
				names = []string{"a", "b"}
			}
			failures, err := reached(t, test.kinds, test.counts).Check(names, runs)
			if test.err != "" {
				is.True(t, err != nil && strings.Contains(err.Error(), test.err), err)
				return
			}
			is.NotError(t, err)
			is.Equal(t, len(test.failures), len(failures), strings.Join(failures, "\n"))
			for i, failure := range test.failures {
				is.True(t, strings.Contains(failures[i], failure), failures[i])
			}
		})
	}

	t.Run("should write, with S4QL_RECOUNT=1, a file that the check then passes, with a row for each required kind that a run reaches three times and for each exact kind", func(t *testing.T) {
		r := reached(t, kinds, "")
		t.Setenv(stmttest.RecountVariable, "1")
		failures, err := r.Check([]string{"a", "b"}, runs())
		is.NotError(t, err)
		is.Equal(t, 0, len(failures))
		written, err := os.ReadFile(r.Counts)
		is.NotError(t, err)
		is.True(t, strings.HasSuffix(string(written), "a\t4\t=\texcused: long\na\t9\t3\trows found\nb\t30\t10\tno row found\nb\t3\t1\trows found\n"), string(written))
		t.Setenv(stmttest.RecountVariable, "")
		failures, err = r.Check([]string{"a", "b"}, runs())
		is.NotError(t, err)
		is.Equal(t, 0, len(failures), strings.Join(failures, "\n"))
	})

	t.Run("should write nothing, with S4QL_RECOUNT=1, for a required kind that no run reaches three times, and for a run without a row", func(t *testing.T) {
		r := reached(t, kinds+"1\tthree bounds\n7\tno run\n", "")
		t.Setenv(stmttest.RecountVariable, "1")
		all := runs()
		all["b"]["three bounds"] = 2
		all["c"] = map[string]int{"rows found": 2}
		failures, err := r.Check([]string{"a", "b", "c"}, all)
		is.NotError(t, err)
		is.Equal(t, 4, len(failures), strings.Join(failures, "\n"))
		is.True(t, strings.Contains(failures[0], `the run "c" reaches no required kind three times`), failures[0])
		is.True(t, strings.Contains(failures[1], `no run reaches "three bounds", which is required, three times: the runs reach it b 2 times`), failures[1])
		is.True(t, strings.Contains(failures[2], `no run reaches "no run", which is required, three times: no run reaches it`), failures[2])
		is.True(t, strings.Contains(failures[3], `the runs reach "no run" 0 times in all, and`), failures[3])
		_, err = os.Stat(r.Counts)
		is.True(t, os.IsNotExist(err), "no file is written")
	})
}

func TestReached_Least(t *testing.T) {
	t.Run("should check the least numbers of the runs named and nothing else, and write nothing with S4QL_RECOUNT=1", func(t *testing.T) {
		dir := t.TempDir()
		r := stmttest.Reached{Kinds: filepath.Join(dir, "kinds"), Counts: filepath.Join(dir, "counts"), Exact: func(kind string) bool { return kind == "excused" }}
		const counts = "a\t9\t3\trows found\na\t4\t=\texcused\nb\t30\t10\tno row found\n"
		is.NotError(t, os.WriteFile(r.Kinds, []byte("3\trows found\n12\tno row found\n"), 0o644))
		is.NotError(t, os.WriteFile(r.Counts, []byte(counts), 0o644))
		t.Setenv(stmttest.RecountVariable, "1")
		failures, err := r.Least([]string{"a", "b"}, map[string]map[string]int{"a": {"rows found": 40, "excused": 17}, "b": {"no row found": 12}})
		is.NotError(t, err)
		is.Equal(t, 0, len(failures), strings.Join(failures, "\n"))
		failures, err = r.Least([]string{"a", "b"}, map[string]map[string]int{"a": {"rows found": 2}, "b": {"no row found": 10}})
		is.NotError(t, err)
		is.Equal(t, 3, len(failures), strings.Join(failures, "\n"))
		is.True(t, strings.Contains(failures[0], `a reaches "rows found" 2 times`), failures[0])
		is.True(t, strings.Contains(failures[1], `the runs reach "rows found" 2 times in all, and`), failures[1])
		is.True(t, strings.Contains(failures[2], `the runs reach "no row found" 10 times in all, and`), failures[2])
		written, err := os.ReadFile(r.Counts)
		is.NotError(t, err)
		is.Equal(t, counts, string(written))
	})
}
