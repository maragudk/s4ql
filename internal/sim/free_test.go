package sim_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/sim"
)

func TestFree(t *testing.T) {
	t.Run("should sleep on the bubble's fake clock and stop sleeping when the context is done", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := sim.NewFree(sim.Buggify{})
			is.NotError(t, r.Run(func(ctx context.Context) {
				start, wall := r.Monotonic(), r.Wall()
				is.NotError(t, r.Sleep(ctx, time.Hour))
				is.Equal(t, time.Hour, r.Monotonic()-start)
				is.Equal(t, time.Hour, r.Wall().Sub(wall))

				ctx, cancel := context.WithCancel(ctx)
				sleeper := r.Go(ctx, "sleeper", func(ctx context.Context) {
					is.Error(t, context.Canceled, r.Sleep(ctx, time.Hour))
				})
				// The sleeper is asleep before the context is cancelled.
				synctest.Wait()
				cancel()
				is.NotError(t, sleeper.Wait(context.WithoutCancel(ctx)))
				is.Equal(t, time.Hour, r.Monotonic()-start)
			}))
		})
	})

	t.Run("should end a wait after a timeout with the error for an exceeded deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := sim.NewFree(sim.Buggify{})
			is.NotError(t, r.Run(func(ctx context.Context) {
				ctx, cancel := r.WithTimeout(ctx, time.Second)
				defer cancel()
				is.True(t, errors.Is(r.Sleep(ctx, time.Hour), context.DeadlineExceeded))
			}))
		})
	})

	t.Run("should return the context's error from a wait for a task or a permit when the context is done first", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := sim.NewFree(sim.Buggify{})
			is.NotError(t, r.Run(func(ctx context.Context) {
				m := r.NewSemaphore("m", 1)
				is.NotError(t, m.Acquire(ctx))
				sleeper := r.Go(ctx, "sleeper", func(ctx context.Context) {
					is.NotError(t, r.Sleep(ctx, time.Hour))
				})

				waitCtx, cancel := context.WithCancel(ctx)
				waiter := r.Go(ctx, "waiter", func(ctx context.Context) {
					is.Error(t, context.Canceled, m.Acquire(waitCtx))
					is.Error(t, context.Canceled, sleeper.Wait(waitCtx))
				})
				// The waiter waits for the permit before the context is cancelled.
				synctest.Wait()
				cancel()
				is.NotError(t, waiter.Wait(ctx))
				m.Release(ctx)
			}))
		})
	})

	t.Run("should let only as many goroutines in as there are permits", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := sim.NewFree(sim.Buggify{})
			var inside, most atomic.Int64
			is.NotError(t, r.Run(func(ctx context.Context) {
				m := r.NewSemaphore("m", 2)
				var tasks []host.Task
				for i := range 6 {
					tasks = append(tasks, r.Go(ctx, fmt.Sprint("worker-", i), func(ctx context.Context) {
						for range 50 {
							is.NotError(t, m.Acquire(ctx))
							n := inside.Add(1)
							for {
								v := most.Load()
								if n <= v || most.CompareAndSwap(v, n) {
									break
								}
							}
							is.NotError(t, r.Sleep(ctx, time.Millisecond))
							inside.Add(-1)
							m.Release(ctx)
						}
					}))
				}
				for _, task := range tasks {
					is.NotError(t, task.Wait(ctx))
				}
			}))
			is.Equal(t, 2, most.Load())
		})
	})

	t.Run("should return from a yield and a trace, with any context, and record nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := sim.NewFree(sim.Buggify{})
			start := r.Monotonic()
			// Unlike the scheduler's, they accept a context that no task was started with.
			r.Yield(t.Context(), "site")
			r.Trace(t.Context(), "line %v", 1)
			is.NotError(t, r.Run(func(ctx context.Context) {
				r.Yield(ctx, "site")
				r.Trace(ctx, "line %v", 2)
			}))
			is.Equal(t, time.Duration(0), r.Monotonic()-start)
		})
	})

	t.Run("should wait in Run for tasks that the tasks start, also after the first task has returned", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := sim.NewFree(sim.Buggify{})
			var done atomic.Bool
			is.NotError(t, r.Run(func(ctx context.Context) {
				r.Go(ctx, "child", func(ctx context.Context) {
					r.Go(ctx, "grandchild", func(ctx context.Context) {
						is.NotError(t, r.Sleep(ctx, time.Hour))
						done.Store(true)
					})
				})
			}))
			is.True(t, done.Load())
		})
	})

	t.Run("should buggify only when switched on", func(t *testing.T) {
		off := sim.NewFree(sim.Buggify{})
		always := sim.NewFree(sim.Buggify{On: true, Activation: 1, Fire: 1})
		for range 100 {
			is.True(t, !off.Buggify(t.Context(), "site"))
			is.True(t, always.Buggify(t.Context(), "site"))
		}
	})

	t.Run("should keep a site that is off for the run off", func(t *testing.T) {
		r := sim.NewFree(sim.Buggify{On: true, Activation: 0.5, Fire: 1})
		for i := range 20 {
			site := fmt.Sprint("site-", i)
			first := r.Buggify(t.Context(), site)
			for range 50 {
				is.Equal(t, first, r.Buggify(t.Context(), site))
			}
		}
	})
}
