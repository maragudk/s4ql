//go:build s4ql_speed

package sim_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sim"
)

// TestSimulationSpeed logs how fast the scheduler is on this machine, with tasks that only yield. It
// is a test and not a benchmark because a bubble needs a [testing.T]. It asserts nothing that the
// other tests do not, so it is built only with the tag s4ql_speed.
func TestSimulationSpeed(t *testing.T) {
	t.Run("should log the speed of the scheduler with eight tasks that only yield", func(t *testing.T) {
		start := time.Now()
		var steps int
		for seed := range uint64(20) {
			s, err := schedule(t, seed, sim.Options{Jitter: time.Millisecond}, func(ctx context.Context, s *sim.Scheduler) {
				for i := range 8 {
					s.Go(ctx, fmt.Sprint("worker-", i), func(ctx context.Context) {
						for range 2000 {
							s.Yield(ctx, "between")
						}
					})
				}
			})
			is.NotError(t, err)
			steps += s.Steps()
		}
		elapsed := time.Since(start)
		t.Logf("%v steps in %v, %.0f steps per second", steps, elapsed.Round(time.Millisecond), float64(steps)/elapsed.Seconds())
	})
}
