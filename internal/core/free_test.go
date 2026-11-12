package core_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"
)

// TestFreeRunning runs the workloads with the scheduler off, on real goroutines in parallel, for
// the race detector, which sees nothing under the scheduler.
func TestFreeRunning(t *testing.T) {
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

// plantRace is the environment variable that makes [TestPlantedDataRace] run.
const plantRace = "S4QL_CORE_PLANT_RACE"

// TestPlantedDataRace runs the workload whose callers never pause with a planted data race: every
// read adds to a counter without synchronisation. It only does anything when the environment
// variable S4QL_CORE_PLANT_RACE is "scheduler" or "free", because a race that is reported fails the
// test: [TestFreeRunning_plantedDataRace] runs it that way, and requires the report.
func TestPlantedDataRace(t *testing.T) {
	w := plant(hammerWorkload, "unlockedCounter")
	switch mode := os.Getenv(plantRace); mode {
	case "scheduler":
		for seed := range uint64(5) {
			is.EqualSlice(t, nil, simulate(t, seed, w).violations())
		}
	case "free":
		for range 5 {
			is.EqualSlice(t, nil, free(t, w).violations)
		}
	default:
		t.Skip(plantRace + " is not set")
	}
}

// TestFreeRunning_plantedDataRace shows that the free-running mode is what the race detector
// needs: it runs [TestPlantedDataRace] in a process of its own, and the race detector must report
// the planted race with the scheduler off, and nothing under the scheduler, which runs one
// goroutine at a time. It needs a test binary that is built with the race detector.
func TestFreeRunning_plantedDataRace(t *testing.T) {
	if !raceEnabled {
		t.Skip("the test binary is built without the race detector")
	}
	run := func(mode string) (string, error) {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestPlantedDataRace$", "-test.count", "1")
		cmd.Env = append(os.Environ(), plantRace+"="+mode)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("should report the planted data race with the scheduler off", func(t *testing.T) {
		out, err := run("free")
		is.True(t, err != nil, "the run with the planted race passed")
		is.True(t, strings.Contains(out, "WARNING: DATA RACE"), out)
	})

	t.Run("should report no data race under the scheduler, where one goroutine runs at a time", func(t *testing.T) {
		out, err := run("scheduler")
		is.NotError(t, err, out)
		is.True(t, !strings.Contains(out, "WARNING: DATA RACE"), out)
	})
}
