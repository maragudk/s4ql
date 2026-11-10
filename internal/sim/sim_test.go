package sim_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sim"
)

// schedule runs f as the main task of a new scheduler for seed in its own bubble, and returns the
// scheduler and the result of the run.
func schedule(t *testing.T, seed uint64, options sim.Options, f func(ctx context.Context, s *sim.Scheduler)) (*sim.Scheduler, error) {
	t.Helper()
	var s *sim.Scheduler
	var err error
	synctest.Test(t, func(t *testing.T) {
		s = sim.New(seed, options)
		err = s.Run(func(ctx context.Context) { f(ctx, s) })
	})
	return s, err
}

// errorString of err, or the empty string for nil.
func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestScheduler_Run(t *testing.T) {
	// workers starts four tasks that each trace three times with a yield in between.
	workers := func(ctx context.Context, s *sim.Scheduler) {
		for i := range 4 {
			s.Go(ctx, fmt.Sprint("worker-", i), func(ctx context.Context) {
				for j := range 3 {
					s.Trace(ctx, "at %v", j)
					s.Yield(ctx, "between")
				}
			})
		}
	}

	t.Run("should give the same trace for the same seed and different traces for different seeds", func(t *testing.T) {
		traces := map[string]bool{}
		for seed := range uint64(200) {
			first, err := schedule(t, seed, sim.Options{}, workers)
			is.NotError(t, err)
			second, err := schedule(t, seed, sim.Options{}, workers)
			is.NotError(t, err)
			is.EqualSlice(t, first.Lines(), second.Lines())
			is.Equal(t, first.Uint64(), second.Uint64())
			traces[strings.Join(first.Lines(), "\n")] = true
		}
		t.Logf("%v distinct traces from 200 seeds", len(traces))
		is.True(t, len(traces) > 150)
	})

	// The scheduler cannot make a task deterministic that is not. Running a seed twice is what
	// catches it.
	t.Run("should give different traces for the same seed when a task depends on the order of a map", func(t *testing.T) {
		byMapOrder := func(ctx context.Context, s *sim.Scheduler) {
			for name := range map[string]bool{"a": true, "b": true, "c": true, "d": true, "e": true, "f": true} {
				s.Trace(ctx, "at %v", name)
				s.Yield(ctx, "between")
			}
		}
		var differing int
		for seed := range uint64(50) {
			first, err := schedule(t, seed, sim.Options{}, byMapOrder)
			is.NotError(t, err)
			second, err := schedule(t, seed, sim.Options{}, byMapOrder)
			is.NotError(t, err)
			if strings.Join(first.Lines(), "\n") != strings.Join(second.Lines(), "\n") {
				differing++
			}
		}
		t.Logf("%v of 50 seeds gave two different traces", differing)
		is.True(t, differing > 0)
	})

	t.Run("should run exactly one task at a time", func(t *testing.T) {
		for seed := range uint64(50) {
			var running, most atomic.Int64
			_, err := schedule(t, seed, sim.Options{Jitter: time.Millisecond}, func(ctx context.Context, s *sim.Scheduler) {
				for i := range 8 {
					s.Go(ctx, fmt.Sprint("worker-", i), func(ctx context.Context) {
						for range 20 {
							n := running.Add(1)
							for {
								m := most.Load()
								if n <= m || most.CompareAndSwap(m, n) {
									break
								}
							}
							running.Add(-1)
							s.Yield(ctx, "between")
						}
					})
				}
			})
			is.NotError(t, err)
			is.Equal(t, 1, most.Load())
		}
	})

	t.Run("should fail with the waiting tasks when they deadlock, and unwind them", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a, b := s.NewSemaphore("a", 1), s.NewSemaphore("b", 1)
			for i, order := range [][]sim.Semaphore{{a, b}, {b, a}} {
				s.Go(ctx, fmt.Sprint("worker-", i), func(ctx context.Context) {
					is.NotError(t, order[0].Acquire(ctx))
					defer order[0].Release(ctx)
					// Both tasks hold their first semaphore before either wants its second.
					is.NotError(t, s.Sleep(ctx, time.Second))
					is.NotError(t, order[1].Acquire(ctx))
					t.Error("acquired both semaphores")
				})
			}
		})
		is.Equal(t, "deadlock: every task waits on a condition that is false: worker-0 at acquire of b, worker-1 at acquire of a", errorString(err))
	})

	t.Run("should fail when a task blocks on something the scheduler does not know", func(t *testing.T) {
		var raw chan struct{}
		synctest.Test(t, func(t *testing.T) {
			raw = make(chan struct{})
			s := sim.New(1, sim.Options{})
			err := s.Run(func(ctx context.Context) {
				s.Yield(ctx, "before")
				<-raw
			})
			is.Equal(t, "task main is blocked outside the scheduler, after before", errorString(err))
			// The scheduler cannot unwind the task, and the bubble would report a deadlock.
			close(raw)
		})
	})

	t.Run("should fail when a task blocks on the bubble's fake clock", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(1, sim.Options{})
			err := s.Run(func(ctx context.Context) {
				time.Sleep(time.Second)
			})
			is.Equal(t, "task main is blocked outside the scheduler, after start", errorString(err))
			// Fake time moves only while the bubble's first goroutine waits, and the task must return
			// before the bubble ends.
			time.Sleep(time.Second)
		})
	})

	t.Run("should fail when a task starts a goroutine with a go statement that then waits", func(t *testing.T) {
		for range 100 {
			synctest.Test(t, func(t *testing.T) {
				s := sim.New(1, sim.Options{})
				err := s.Run(func(ctx context.Context) {
					go func() {
						// The goroutine is not a task of its own, so the failure reaches it as a panic.
						defer func() { _ = recover() }()
						s.Yield(ctx, "in the goroutine")
					}()
					s.Yield(ctx, "after go")
				})
				is.True(t, strings.HasPrefix(errorString(err), "task main reached "), errorString(err))
				is.True(t, strings.HasSuffix(errorString(err), " without the scheduler having run it"), errorString(err))
			})
		}
	})

	t.Run("should fail after the maximum number of steps", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{MaxSteps: 100}, func(ctx context.Context, s *sim.Scheduler) {
			for {
				s.Yield(ctx, "forever")
			}
		})
		is.Equal(t, "no end after 100 steps", errorString(err))
	})
}

func TestScheduler_Sleep(t *testing.T) {
	t.Run("should wake tasks in the order of simulated time, without real time passing", func(t *testing.T) {
		realStart := time.Now()
		s, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			for _, hours := range []int{3, 1, 2} {
				s.Go(ctx, fmt.Sprint("sleeper-", hours), func(ctx context.Context) {
					is.NotError(t, s.Sleep(ctx, time.Duration(hours)*time.Hour))
					s.Trace(ctx, "woke at %v", s.Now().Sub(sim.Epoch))
				})
			}
		})
		is.NotError(t, err)
		is.True(t, time.Since(realStart) < time.Minute)

		var woke []string
		for _, line := range s.Lines() {
			if strings.Contains(line, "woke") {
				woke = append(woke, line)
			}
		}
		is.EqualSlice(t, []string{"1h0m0s sleeper-1 woke at 1h0m0s", "2h0m0s sleeper-2 woke at 2h0m0s", "3h0m0s sleeper-3 woke at 3h0m0s"}, woke)
	})

	t.Run("should wake tasks that are due at the same instant in an order that the seed decides", func(t *testing.T) {
		orders := map[string]bool{}
		for seed := range uint64(300) {
			run := func() string {
				var order []int
				_, err := schedule(t, seed, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
					for i := range 4 {
						s.Go(ctx, fmt.Sprint("sleeper-", i), func(ctx context.Context) {
							is.NotError(t, s.Sleep(ctx, time.Second))
							order = append(order, i)
						})
					}
				})
				is.NotError(t, err)
				return fmt.Sprint(order)
			}
			first, second := run(), run()
			is.Equal(t, first, second)
			orders[first] = true
		}
		is.Equal(t, 24, len(orders))
	})

	t.Run("should return the context's error when the context is done first", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			ctx, cancel := context.WithCancel(ctx)
			sleeper := s.Go(ctx, "sleeper", func(ctx context.Context) {
				is.Error(t, context.Canceled, s.Sleep(ctx, time.Hour))
				is.Equal(t, time.Second, s.Now().Sub(sim.Epoch))
			})
			is.NotError(t, s.Sleep(ctx, time.Second))
			cancel()
			is.NotError(t, sleeper.Wait(context.WithoutCancel(ctx)))
		})
		is.NotError(t, err)
	})
}

func TestScheduler_NewSemaphore(t *testing.T) {
	t.Run("should let only as many tasks in as there are permits, also while the holders wait", func(t *testing.T) {
		for seed := range uint64(50) {
			var most int
			_, err := schedule(t, seed, sim.Options{Jitter: time.Millisecond}, func(ctx context.Context, s *sim.Scheduler) {
				m := s.NewSemaphore("m", 2)
				var inside int
				for i := range 6 {
					s.Go(ctx, fmt.Sprint("worker-", i), func(ctx context.Context) {
						for range 5 {
							is.NotError(t, m.Acquire(ctx))
							inside++
							most = max(most, inside)
							is.NotError(t, s.Sleep(ctx, time.Millisecond))
							s.Yield(ctx, "inside")
							inside--
							m.Release(ctx)
						}
					})
				}
			})
			is.NotError(t, err)
			is.Equal(t, 2, most)
		}
	})

	t.Run("should return the context's error when the context is done before a permit is free", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			m := s.NewSemaphore("m", 1)
			is.NotError(t, m.Acquire(ctx))

			waitCtx, cancel := context.WithCancel(ctx)
			waiter := s.Go(waitCtx, "waiter", func(ctx context.Context) {
				is.Error(t, context.Canceled, m.Acquire(ctx))
			})
			is.NotError(t, s.Sleep(ctx, time.Second))
			cancel()
			is.NotError(t, waiter.Wait(ctx))
			m.Release(ctx)
		})
		is.NotError(t, err)
	})
}

// TestScheduler_window shows the limit of the scheduler: it switches tasks only where they wait, so a
// race in a stretch of code without such a point never happens under it.
func TestScheduler_window(t *testing.T) {
	// claim runs four tasks that each take a flag if it is not taken, and returns how many took it. The
	// check and the act are not one step, which is the bug.
	claim := func(ctx context.Context, r interface {
		Go(ctx context.Context, name string, f func(ctx context.Context)) sim.Task
	}, between func(ctx context.Context)) int64 {
		var taken atomic.Bool
		var owners atomic.Int64
		var tasks []sim.Task
		for i := range 4 {
			tasks = append(tasks, r.Go(ctx, fmt.Sprint("worker-", i), func(ctx context.Context) {
				if !taken.Load() {
					between(ctx)
					taken.Store(true)
					owners.Add(1)
				}
			}))
		}
		for _, task := range tasks {
			is.NotError(t, task.Wait(ctx))
		}
		return owners.Load()
	}

	t.Run("should never find a race in a window without a point where tasks can switch", func(t *testing.T) {
		for seed := range uint64(1000) {
			_, err := schedule(t, seed, sim.Options{Jitter: time.Millisecond}, func(ctx context.Context, s *sim.Scheduler) {
				is.Equal(t, 1, claim(ctx, s, func(context.Context) {}))
			})
			is.NotError(t, err)
		}
	})

	t.Run("should find the race for some seeds, and for those again on every rerun, once the window has a yield", func(t *testing.T) {
		var found int
		for seed := range uint64(1000) {
			var owners [3]int64
			for i := range owners {
				_, err := schedule(t, seed, sim.Options{Jitter: time.Millisecond}, func(ctx context.Context, s *sim.Scheduler) {
					owners[i] = claim(ctx, s, func(ctx context.Context) { s.Yield(ctx, "window") })
				})
				is.NotError(t, err)
			}
			is.Equal(t, owners[0], owners[1])
			is.Equal(t, owners[0], owners[2])
			if owners[0] > 1 {
				found++
			}
		}
		t.Logf("%v of 1000 seeds found the race", found)
		is.True(t, found > 0)
	})

	t.Run("can find the race without a yield when tasks run in parallel, but not on demand", func(t *testing.T) {
		var found int
		for seed := range uint64(1000) {
			synctest.Test(t, func(t *testing.T) {
				r := sim.NewFree(seed)
				is.NotError(t, r.Run(func(ctx context.Context) {
					if claim(ctx, r, func(context.Context) {}) > 1 {
						found++
					}
				}))
			})
		}
		t.Logf("%v of 1000 free runs found the race", found)
	})
}

// TestScheduler_mutexHeldAcrossWait shows the one broken rule that the scheduler cannot report: a
// sync.Mutex held while its holder waits. The task that wants the mutex is blocked, but not durably,
// so synctest.Wait never returns and the test hangs until go test's timeout. It only runs when the
// environment variable S4QL_SIM_HANG is set, with a short -timeout.
func TestScheduler_mutexHeldAcrossWait(t *testing.T) {
	if os.Getenv("S4QL_SIM_HANG") == "" {
		t.Skip("S4QL_SIM_HANG is not set")
	}
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

func TestFree(t *testing.T) {
	t.Run("should sleep on the bubble's fake clock and stop sleeping when the context is done", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := sim.NewFree(1)
			is.NotError(t, r.Run(func(ctx context.Context) {
				start := r.Now()
				is.NotError(t, r.Sleep(ctx, time.Hour))
				is.Equal(t, time.Hour, r.Now().Sub(start))

				ctx, cancel := context.WithCancel(ctx)
				sleeper := r.Go(ctx, "sleeper", func(ctx context.Context) {
					is.Error(t, context.Canceled, r.Sleep(ctx, time.Hour))
				})
				cancel()
				is.NotError(t, sleeper.Wait(context.WithoutCancel(ctx)))
			}))
		})
	})

	t.Run("should let only as many goroutines in as there are permits", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := sim.NewFree(1)
			var inside, most atomic.Int64
			is.NotError(t, r.Run(func(ctx context.Context) {
				m := r.NewSemaphore("m", 2)
				var tasks []sim.Task
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
}
