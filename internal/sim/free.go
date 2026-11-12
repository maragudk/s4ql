package sim

import (
	"context"
	"math/rand/v2"
	"runtime"
	"sync"

	"maragu.dev/s4ql/internal/host"
)

// Free is a [host.Host] with real goroutines that run in parallel, for the race detector. It is the
// production host of [host.New], with three differences: a yield lets other goroutines run, buggify
// works, and [Free.Run] waits for every task. Inside a testing/synctest bubble its clocks are the
// bubble's fake clock. Nothing in a run with it is replayable, and it has no instances: a test uses
// the one host for every simulated process, with no crashes and no clocks of their own.
//
// It shares no state between goroutines that the race detector would count as synchronisation,
// beyond what each method needs: buggify draws from the generator of math/rand/v2 and not from a
// seed, and it keeps no trace.
type Free struct {
	host.Host
	buggify Buggify
	// tasks that have not returned.
	tasks sync.WaitGroup
	// sites of buggify that have been reached, and whether each is on.
	sites sync.Map
}

var _ host.Host = (*Free)(nil)

// NewFree runtime, with buggify as [Buggify] describes.
func NewFree(buggify Buggify) *Free {
	return &Free{Host: host.New(), buggify: buggify.withDefaults()}
}

// Run f and wait until it and every task started from it have returned. It never fails.
func (r *Free) Run(f func(ctx context.Context)) error {
	r.Go(context.Background(), "main", f)
	r.tasks.Wait()
	return nil
}

// Go as [host.Host] describes, and so that [Free.Run] waits for the task.
func (r *Free) Go(ctx context.Context, name string, f func(ctx context.Context)) host.Task {
	r.tasks.Add(1)
	return r.Host.Go(ctx, name, func(ctx context.Context) {
		defer r.tasks.Done()
		f(ctx)
	})
}

// Yield lets other goroutines run.
func (r *Free) Yield(context.Context, string) {
	runtime.Gosched()
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
