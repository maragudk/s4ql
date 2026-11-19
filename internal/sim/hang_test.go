//go:build s4ql_hang

package sim_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sim"
)

// TestScheduler_mutexHeldAcrossWait shows a broken rule that hangs the run: a sync.Mutex held while
// its holder waits. The task that wants the mutex is blocked, but not durably, so synctest.Wait
// never returns and the test hangs until go test's timeout. It fails by design, so it is built only
// with the tag s4ql_hang, which make sim-hang sets, with a short -timeout.
func TestScheduler_mutexHeldAcrossWait(t *testing.T) {
	_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
		var mu sync.Mutex
		for i := range 2 {
			s.Go(ctx, fmt.Sprint("worker-", i), func(ctx context.Context) {
				mu.Lock()
				defer mu.Unlock()
				is.NotError(t, s.Sleep(ctx, time.Hour))
			})
		}
	})
	is.NotError(t, err)
}
