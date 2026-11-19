//go:build race

package core_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"maragu.dev/is"
)

// raceEnabled reports whether the test binary is built with the race detector.
const raceEnabled = true

// TestFreeRunning_plantedDataRace shows that the free-running mode is what the race detector
// needs: the race detector must report a planted data race with the scheduler off, and nothing under
// the scheduler, which runs one goroutine at a time. The race is in the workload whose callers never
// pause: every read adds to a counter without synchronisation. A race that is reported fails the
// test binary, so the test runs the workload in a process of its own: this test again, with the
// environment variable S4QL_CORE_PLANT_RACE set to "free" or "scheduler", in which it runs the
// workload in that mode and nothing else.
func TestFreeRunning_plantedDataRace(t *testing.T) {
	w := plant(hammerWorkload, "unlockedCounter")
	switch mode := os.Getenv("S4QL_CORE_PLANT_RACE"); mode {
	case "":
	case "scheduler":
		for seed := range uint64(5) {
			is.EqualSlice(t, nil, simulate(t, seed, w).violations())
		}
		return
	case "free":
		for range 5 {
			is.EqualSlice(t, nil, free(t, w).violations)
		}
		return
	default:
		t.Fatalf("S4QL_CORE_PLANT_RACE is %q, and must be free or scheduler", mode)
	}

	run := func(mode string) (string, error) {
		cmd := exec.Command(os.Args[0], "-test.run", "^"+t.Name()+"$", "-test.count", "1")
		cmd.Env = append(os.Environ(), "S4QL_CORE_PLANT_RACE="+mode)
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
