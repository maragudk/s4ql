package core_test

import (
	"strings"
	"testing"
	"time"

	"maragu.dev/is"
)

// TestFreeRunning runs the workloads with the scheduler off, on real goroutines in parallel, for
// the race detector, which sees nothing under the scheduler.
func TestFreeRunning(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		workload workload
		runs     int
	}{
		{name: "should run the workload with one instance with the scheduler off and no violations", workload: singleWorkload, runs: 30},
		{name: "should run instances that compete for the lease, with buggify, with the scheduler off and no violations", workload: fullWorkload, runs: 30},
		{name: "should run callers that never pause with the scheduler off and no violations", workload: hammerWorkload, runs: 30},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			n := seeds(t, test.runs)
			var commits, failedCommits, unknownCommits, reads, refusedReads, stops int64
			start := time.Now()
			for i := range n {
				o := free(t, test.workload)
				if len(o.violations) > 0 {
					t.Errorf("run %v: %v", i, strings.Join(o.violations, "; "))
				}
				commits += o.commits.Load()
				failedCommits += o.failedCommits.Load()
				unknownCommits += o.unknownCommits.Load()
				reads += o.reads.Load()
				refusedReads += o.refusedReads.Load()
				stops += o.stops.Load()
			}
			elapsed := time.Since(start)
			t.Logf("%v free runs in %v: %v commits, %v failed commits, %v of unknown outcome, %v reads, %v refused reads, %v stops",
				n, elapsed.Round(time.Millisecond), commits, failedCommits, unknownCommits, reads, refusedReads, stops)
			is.True(t, commits > 0 && reads > 0, "the workload committed or read nothing")
		})
	}
}
