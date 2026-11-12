package sim

import (
	"context"
	"math/rand/v2"
	"runtime"
	"sync"
	"time"

	"maragu.dev/s4ql/internal/host"
)

// Free is a [host.Host] that maps each method to the plain Go operation, with real goroutines that
// run in parallel, for the race detector. Inside a testing/synctest bubble its clocks are the
// bubble's fake clock. Nothing in a run with it is replayable, and it has no instances: a test uses
// the one host for every simulated process, with no crashes and no clocks of their own.
//
// It shares no state between goroutines that the race detector would count as synchronisation,
// beyond what each method needs: random numbers come from the generator of math/rand/v2 and not
// from a seed, and it keeps no trace.
type Free struct {
	buggify Buggify
	// start is the origin of the monotonic clock.
	start time.Time
	// tasks that have not returned.
	tasks sync.WaitGroup
	// sites of buggify that have been reached, and whether each is on.
	sites sync.Map
}

var _ host.Host = (*Free)(nil)

// NewFree runtime, with buggify as [Buggify] describes.
func NewFree(buggify Buggify) *Free {
	return &Free{buggify: buggify.withDefaults(), start: time.Now()}
}

// Run f and wait until it and every task started from it have returned. It never fails.
func (r *Free) Run(f func(ctx context.Context)) error {
	r.Go(context.Background(), "main", f)
	r.tasks.Wait()
	return nil
}

// freeTask of a [Free] runtime, whose channel is closed when the task has returned.
type freeTask chan struct{}

func (t freeTask) Wait(ctx context.Context) error {
	select {
	case <-t:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Go starts f in a new goroutine. The name is not used.
func (r *Free) Go(ctx context.Context, _ string, f func(ctx context.Context)) host.Task {
	t := make(freeTask)
	r.tasks.Go(func() {
		defer close(t)
		f(ctx)
	})
	return t
}

// NewSemaphore with n permits. The name is not used.
func (r *Free) NewSemaphore(_ string, n int) host.Semaphore {
	return make(freeSemaphore, n)
}

// freeSemaphore of a [Free] runtime, which holds one value for every permit that is taken.
type freeSemaphore chan struct{}

func (m freeSemaphore) Acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m freeSemaphore) Release(context.Context) {
	select {
	case <-m:
	default:
		panic("sim: release of a semaphore that is not held")
	}
}

// Sleep for d. It fails only if ctx is done first.
func (r *Free) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WithTimeout is [context.WithTimeout].
func (r *Free) WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

// Yield lets other goroutines run.
func (r *Free) Yield(context.Context, string) {
	runtime.Gosched()
}

// Wall is the time package's time, without a monotonic reading.
func (r *Free) Wall() time.Time {
	return time.Now().Round(0)
}

// Monotonic is the time since [NewFree].
func (r *Free) Monotonic() time.Duration {
	return time.Since(r.start)
}

// Uint64 is a random number from the generator of math/rand/v2.
func (r *Free) Uint64() uint64 {
	return rand.Uint64()
}

// Buggify as [host.Host] describes, under the options of [NewFree], with random numbers that come
// from no seed.
func (r *Free) Buggify(_ context.Context, site string) bool {
	if !r.buggify.On {
		return false
	}
	on, ok := r.sites.Load(site)
	if !ok {
		on, _ = r.sites.LoadOrStore(site, rand.Float64() < r.buggify.Activation)
	}
	return on.(bool) && rand.Float64() < r.buggify.Fire
}

// Trace does nothing, because a trace shared by goroutines that run in parallel would synchronise
// them and hide data races from the race detector.
func (r *Free) Trace(context.Context, string, ...any) {}
