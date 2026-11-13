package sim_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/host"
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

// since is the simulated time since [sim.Epoch] on the wall clock of h.
func since(h host.Host) time.Duration {
	return h.Wall().Sub(sim.Epoch())
}

// matching lines of s's trace, without their time.
func matching(s *sim.Scheduler, substring string) []string {
	var lines []string
	for _, line := range s.Lines() {
		if strings.Contains(line, substring) {
			_, line, _ = strings.Cut(line, " ")
			lines = append(lines, line)
		}
	}
	return lines
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
					h := host.Host(s)
					if i%2 == 1 {
						h = s.NewInstance(fmt.Sprint("instance-", i))
					}
					h.Go(ctx, fmt.Sprint("worker-", i), func(ctx context.Context) {
						for range 20 {
							n := running.Add(1)
							for {
								m := most.Load()
								if n <= m || most.CompareAndSwap(m, n) {
									break
								}
							}
							// Give any other goroutine that is running the time to be counted too.
							runtime.Gosched()
							running.Add(-1)
							h.Yield(ctx, "between")
						}
					})
				}
			})
			is.NotError(t, err)
			is.Equal(t, 1, most.Load())
		}
	})

	// Moving simulated time, expiring a timeout, and ending a crashed task are not steps.
	t.Run("should count a step each time it runs a task from one wait to its next, and at nothing else", func(t *testing.T) {
		s, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			is.Equal(t, 1, s.Steps())
			s.Yield(ctx, "first")
			is.Equal(t, 2, s.Steps())
			is.NotError(t, s.Go(ctx, "worker", func(ctx context.Context) {
				s.Yield(ctx, "in the worker")
			}).Wait(ctx))
			// The worker ran twice, and main once more after its wait.
			is.Equal(t, 5, s.Steps())

			is.NotError(t, s.Sleep(ctx, time.Hour))
			is.Equal(t, 6, s.Steps())
			timeoutCtx, cancel := s.WithTimeout(ctx, time.Second)
			defer cancel()
			is.True(t, errors.Is(s.Sleep(timeoutCtx, time.Hour), context.DeadlineExceeded))
			is.Equal(t, 7, s.Steps())

			a := s.NewInstance("a")
			a.Go(ctx, "sleeper", func(ctx context.Context) { _ = a.Sleep(ctx, time.Hour) })
			is.NotError(t, s.Sleep(ctx, time.Second))
			// The sleeper started, and main woke.
			is.Equal(t, 9, s.Steps())
			a.Crash()
		})
		is.NotError(t, err)
		is.Equal(t, 9, s.Steps())
		is.Equal(t, 9, len(matching(s, " resumes from ")))
	})

	t.Run("should delay half of the yields, releases, and starts of a task by up to the jitter, and the others not at all", func(t *testing.T) {
		delays := map[string][]time.Duration{}
		_, err := schedule(t, 1, sim.Options{Jitter: time.Second}, func(ctx context.Context, s *sim.Scheduler) {
			m := s.NewSemaphore("m", 1)
			for range 400 {
				before := since(s)
				s.Yield(ctx, "between")
				delays["yield"] = append(delays["yield"], since(s)-before)

				is.NotError(t, m.Acquire(ctx))
				before = since(s)
				m.Release(ctx)
				delays["release"] = append(delays["release"], since(s)-before)

				before = since(s)
				is.NotError(t, s.Go(ctx, "task", func(context.Context) {
					delays["start"] = append(delays["start"], since(s)-before)
				}).Wait(ctx))
			}
		})
		is.NotError(t, err)
		for what, ds := range delays {
			var none int
			for _, d := range ds {
				is.True(t, d >= 0 && d <= time.Second, fmt.Sprint(what, " delayed by ", d))
				if d == 0 {
					none++
				}
			}
			t.Logf("%v of %v of each %v had no delay", none, len(ds), what)
			is.True(t, none > len(ds)/4 && none < len(ds)*3/4, fmt.Sprint(none, " of ", len(ds), " of each ", what, " had no delay"))
		}
		is.Equal(t, 3, len(delays))
	})

	t.Run("should panic on a context that no task of the scheduler was started with", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			other := sim.New(1, sim.Options{})
			for _, c := range []context.Context{t.Context(), ctx} {
				func() {
					defer func() {
						is.Equal(t, "sim: the context does not belong to a task of this scheduler", fmt.Sprint(recover()))
					}()
					other.Yield(c, "never")
				}()
			}
		})
		is.NotError(t, err)
	})

	t.Run("should fail with the waiting tasks when they deadlock, and unwind them", func(t *testing.T) {
		var unwound atomic.Int64
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a, b := s.NewSemaphore("a", 1), s.NewSemaphore("b", 1)
			for i, order := range [][]host.Semaphore{{a, b}, {b, a}} {
				s.Go(ctx, fmt.Sprint("worker-", i), func(ctx context.Context) {
					defer unwound.Add(1)
					is.NotError(t, order[0].Acquire(ctx))
					defer order[0].Release(ctx)
					// Both tasks hold their first semaphore before either wants its second.
					is.NotError(t, s.Sleep(ctx, time.Second))
					is.NotError(t, order[1].Acquire(ctx))
					t.Error("acquired both semaphores")
				})
			}
		})
		is.Equal(t, "deadlock: no task can run: worker-0 at acquire of b, worker-1 at acquire of a", errorString(err))
		is.Equal(t, 2, unwound.Load())
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

	t.Run("should fail when a goroutine started with a go statement waits while its task waits too", func(t *testing.T) {
		for range 100 {
			_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
				go s.Yield(ctx, "in the goroutine")
				s.Yield(ctx, "after go")
			})
			is.Equal(t, "task main waits at in the goroutine in a goroutine that is not its own", errorString(err))
		}
	})

	t.Run("should fail when a goroutine started with a go statement waits while its task waits for it on a channel", func(t *testing.T) {
		for range 100 {
			_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
				done := make(chan struct{})
				go func() {
					defer close(done)
					s.Yield(ctx, "in the goroutine")
				}()
				<-done
				s.Yield(ctx, "after the goroutine")
			})
			is.Equal(t, "task main waits at in the goroutine in a goroutine that is not its own", errorString(err))
		}
	})

	t.Run("should fail when a task waits with a context that has a deadline", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			_ = s.Sleep(ctx, time.Hour)
		})
		is.Equal(t, "task main waits at sleep with a context that has a deadline, which simulated time does not reach", errorString(err))
	})

	t.Run("should fail when a task waits through the host of an instance that it does not belong to", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			_ = a.Sleep(ctx, time.Second)
		})
		is.Equal(t, "task main waits at sleep through the host of instance a, which is not its own", errorString(err))
	})

	t.Run("should fail when a task of an instance takes a semaphore of the test", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			m := s.NewSemaphore("m", 1)
			a := s.NewInstance("a")
			is.NotError(t, a.Go(ctx, "worker", func(ctx context.Context) {
				_ = m.Acquire(ctx)
			}).Wait(ctx))
		})
		is.Equal(t, "task a/worker waits at acquire of m through the host of the test, which is not its own", errorString(err))
	})

	t.Run("should let a task of an instance sleep through the host of the test", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			is.NotError(t, a.Go(ctx, "worker", func(ctx context.Context) {
				is.NotError(t, s.Sleep(ctx, time.Second))
			}).Wait(ctx))
		})
		is.NotError(t, err)
	})

	t.Run("should fail after the maximum number of steps, with the tasks that wait", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{MaxSteps: 100}, func(ctx context.Context, s *sim.Scheduler) {
			for {
				s.Yield(ctx, "forever")
			}
		})
		is.Equal(t, "no end after 100 steps; waiting: main at forever", errorString(err))
	})

	t.Run("should not fail when the run ends with the last allowed step", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{MaxSteps: 1}, func(context.Context, *sim.Scheduler) {})
		is.NotError(t, err)
	})

	// A task that wakes on time for ever keeps the run from being a deadlock of every task, so two
	// tasks that block each other are only found by the limit on steps.
	t.Run("should name tasks that deadlock while another task keeps waking, after the maximum number of steps", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{MaxSteps: 1000}, func(ctx context.Context, s *sim.Scheduler) {
			a, b := s.NewSemaphore("a", 1), s.NewSemaphore("b", 1)
			for i, order := range [][]host.Semaphore{{a, b}, {b, a}} {
				s.Go(ctx, fmt.Sprint("worker-", i), func(ctx context.Context) {
					_ = order[0].Acquire(ctx)
					defer order[0].Release(ctx)
					_ = s.Sleep(ctx, time.Second)
					_ = order[1].Acquire(ctx)
				})
			}
			for {
				_ = s.Sleep(ctx, time.Minute)
			}
		})
		is.Equal(t, "no end after 1000 steps; waiting: main at sleep, worker-0 at acquire of b, worker-1 at acquire of a", errorString(err))
	})
}

func TestScheduler_Sleep(t *testing.T) {
	t.Run("should wake tasks in the order of simulated time, without real time passing", func(t *testing.T) {
		realStart := time.Now()
		s, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			for _, hours := range []int{3, 1, 2} {
				s.Go(ctx, fmt.Sprint("sleeper-", hours), func(ctx context.Context) {
					is.NotError(t, s.Sleep(ctx, time.Duration(hours)*time.Hour))
					s.Trace(ctx, "woke at %v", since(s))
				})
			}
		})
		is.NotError(t, err)
		is.True(t, time.Since(realStart) < time.Hour)
		is.EqualSlice(t, []string{"sleeper-1 woke at 1h0m0s", "sleeper-2 woke at 2h0m0s", "sleeper-3 woke at 3h0m0s"}, matching(s, "woke"))
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
				is.Equal(t, time.Second, since(s))
			})
			is.NotError(t, s.Sleep(ctx, time.Second))
			cancel()
			is.NotError(t, sleeper.Wait(context.WithoutCancel(ctx)))
		})
		is.NotError(t, err)
	})

	t.Run("should sleep until the context is done when the duration is longer than simulated time can hold", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			is.NotError(t, s.Sleep(ctx, time.Second))

			sleepCtx, cancel := context.WithCancel(ctx)
			sleeper := s.Go(sleepCtx, "sleeper", func(ctx context.Context) {
				is.Error(t, context.Canceled, s.Sleep(ctx, math.MaxInt64))
				is.Equal(t, time.Hour+time.Second, since(s))
			})
			is.NotError(t, s.Sleep(ctx, time.Hour))
			cancel()
			is.NotError(t, sleeper.Wait(ctx))
		})
		is.NotError(t, err)
	})
}

func TestScheduler_WithTimeout(t *testing.T) {
	t.Run("should end every kind of wait after the timeout, with the error for an exceeded deadline", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			m := s.NewSemaphore("m", 1)
			is.NotError(t, m.Acquire(ctx))
			sleeper := s.Go(ctx, "sleeper", func(ctx context.Context) { _ = s.Sleep(ctx, time.Hour) })

			for i, wait := range []func(ctx context.Context) error{
				func(ctx context.Context) error { return s.Sleep(ctx, time.Hour) },
				m.Acquire,
				sleeper.Wait,
			} {
				timeoutCtx, cancel := s.WithTimeout(ctx, time.Second)
				is.True(t, errors.Is(wait(timeoutCtx), context.DeadlineExceeded))
				is.True(t, errors.Is(timeoutCtx.Err(), context.DeadlineExceeded))
				is.True(t, errors.Is(context.Cause(timeoutCtx), context.DeadlineExceeded))
				is.Equal(t, time.Duration(i+1)*time.Second, since(s))
				cancel()
			}
			m.Release(ctx)
		})
		is.NotError(t, err)
	})

	t.Run("should end the waits of a context derived from it with the error for an exceeded deadline, which is its cause", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			timeoutCtx, cancel := s.WithTimeout(ctx, time.Second)
			defer cancel()
			type key struct{}
			derived, cancelDerived := context.WithCancel(context.WithValue(timeoutCtx, key{}, "value"))
			defer cancelDerived()
			is.True(t, errors.Is(s.Sleep(derived, time.Hour), context.DeadlineExceeded))
			is.True(t, errors.Is(context.Cause(derived), context.DeadlineExceeded))
			// The one difference from context.WithTimeout.
			is.Error(t, context.Canceled, derived.Err())
			is.Equal(t, "value", derived.Value(key{}))
			_, ok := derived.Deadline()
			is.True(t, !ok)
		})
		is.NotError(t, err)
	})

	// The context package starts a goroutine to watch a parent of a type it does not know, and such a
	// goroutine would run outside the scheduler, at a time that the seed does not decide.
	t.Run("should cancel a context derived from it at once, also through a value, and start no goroutine", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			before := runtime.NumGoroutine()
			type key struct{}
			timeoutCtx, cancel := s.WithTimeout(ctx, time.Hour)
			derived, cancelDerived := context.WithCancel(context.WithValue(timeoutCtx, key{}, "value"))
			defer cancelDerived()
			nested, cancelNested := s.WithTimeout(context.WithValue(derived, key{}, "nested"), time.Hour)
			defer cancelNested()
			is.Equal(t, before, runtime.NumGoroutine())

			cancel()
			for _, c := range []context.Context{timeoutCtx, derived, nested} {
				is.Error(t, context.Canceled, c.Err())
				select {
				case <-c.Done():
				default:
					t.Error("the context is not done")
				}
			}
		})
		is.NotError(t, err)
	})

	t.Run("should be cancelled at once by its cancel function, and not move simulated time", func(t *testing.T) {
		s, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			timeoutCtx, cancel := s.WithTimeout(ctx, time.Hour)
			derived, cancelDerived := context.WithCancel(timeoutCtx)
			defer cancelDerived()
			cancel()
			is.Error(t, context.Canceled, timeoutCtx.Err())
			is.Error(t, context.Canceled, derived.Err())
			is.Error(t, context.Canceled, s.Sleep(timeoutCtx, time.Hour))
		})
		is.NotError(t, err)
		is.Equal(t, time.Duration(0), since(s))
		is.EqualSlice(t, nil, matching(s, "expires"))
	})

	t.Run("should be done at once when its parent is", func(t *testing.T) {
		s, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			parent, cancelParent := context.WithCancel(ctx)
			timeoutCtx, cancel := s.WithTimeout(parent, time.Hour)
			defer cancel()
			cancelParent()
			is.Error(t, context.Canceled, timeoutCtx.Err())
			select {
			case <-timeoutCtx.Done():
			default:
				t.Error("the context is not done")
			}
			is.Error(t, context.Canceled, s.Sleep(timeoutCtx, time.Hour))
			is.Equal(t, time.Duration(0), since(s))

			// A timeout of a parent that is done is done, and not expired.
			atOnce, cancelAtOnce := s.WithTimeout(parent, 0)
			defer cancelAtOnce()
			is.Error(t, context.Canceled, atOnce.Err())
			is.Error(t, context.Canceled, context.Cause(atOnce))

			// It never expires later either.
			is.NotError(t, s.Sleep(ctx, 2*time.Hour))
			is.Error(t, context.Canceled, timeoutCtx.Err())
		})
		is.NotError(t, err)
		is.EqualSlice(t, nil, matching(s, "expires"))
	})

	t.Run("should let the seed decide whether a sleep that ends when the timeout does ends first", func(t *testing.T) {
		outcomes := map[string]int{}
		for seed := range uint64(100) {
			_, err := schedule(t, seed, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
				timeoutCtx, cancel := s.WithTimeout(ctx, time.Second)
				defer cancel()
				outcomes[errorString(s.Sleep(timeoutCtx, time.Second))]++
			})
			is.NotError(t, err)
		}
		t.Logf("%v", outcomes)
		is.True(t, outcomes[""] > 0 && outcomes[context.DeadlineExceeded.Error()] > 0)
	})

	t.Run("should fail when a task sets a timeout through the host of an instance that it does not belong to", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			_, cancel := a.WithTimeout(ctx, time.Second)
			defer cancel()
		})
		is.Equal(t, "task main sets a timeout on the host of instance a, which is not its own", errorString(err))
	})

	t.Run("should name the task that set it and its duration", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			is.NotError(t, a.Go(ctx, "worker", func(ctx context.Context) {
				timeoutCtx, cancel := a.WithTimeout(ctx, time.Second)
				defer cancel()
				is.Equal(t, "timeout of a/worker after 1s", fmt.Sprint(timeoutCtx))
			}).Wait(ctx))
			// The context of the outer test would be cancelled from outside the bubble, which a context
			// derived from it inside the bubble does not allow.
			timeoutCtx, cancel := s.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			is.Equal(t, "timeout of the test after 1m0s", fmt.Sprint(timeoutCtx))
		})
		is.NotError(t, err)
	})

	t.Run("should panic when a timeout on the host of an instance has no context of a task of the scheduler", func(t *testing.T) {
		// foreign is the context of a task of another scheduler, which carries no channel of its bubble.
		var foreign context.Context
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) { foreign = ctx })
		is.NotError(t, err)

		_, err = schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			// The context of the outer test would be cancelled from outside the bubble, which a context
			// derived from it inside the bubble does not allow.
			for _, c := range []context.Context{context.Background(), foreign} {
				func() {
					defer func() {
						is.Equal(t, "sim: a timeout on the host of an instance needs the context of a task of the instance", fmt.Sprint(recover()))
					}()
					a.WithTimeout(c, time.Second)
				}()
			}
			timeoutCtx, cancel := s.WithTimeout(foreign, time.Minute)
			defer cancel()
			is.Equal(t, "timeout of the test after 1m0s", fmt.Sprint(timeoutCtx))
		})
		is.NotError(t, err)
	})

	t.Run("should be done at once with a timeout that is not positive", func(t *testing.T) {
		s, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			timeoutCtx, cancel := s.WithTimeout(ctx, 0)
			defer cancel()
			is.True(t, errors.Is(timeoutCtx.Err(), context.DeadlineExceeded))
			select {
			case <-timeoutCtx.Done():
			default:
				t.Error("the context is not done")
			}
		})
		is.NotError(t, err)
		is.EqualSlice(t, []string{"main has a timeout of 0s, which expires at once"}, matching(s, "timeout"))
	})

	t.Run("should let a task of an instance set a timeout through the host of the test, on the simulated time", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			a.StopMonotonic()
			is.NotError(t, a.Go(ctx, "worker", func(ctx context.Context) {
				timeoutCtx, cancel := s.WithTimeout(ctx, time.Second)
				defer cancel()
				is.True(t, errors.Is(s.Sleep(timeoutCtx, time.Hour), context.DeadlineExceeded))
				is.Equal(t, time.Second, since(s))
			}).Wait(ctx))
		})
		is.NotError(t, err)
	})

	t.Run("should expire timeouts that are due at the same instant in the order in which they were made", func(t *testing.T) {
		s, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			for i := range 4 {
				s.Go(ctx, fmt.Sprint("waiter-", i), func(ctx context.Context) {
					timeoutCtx, cancel := s.WithTimeout(ctx, time.Second)
					defer cancel()
					_ = s.Sleep(timeoutCtx, time.Hour)
				})
			}
		})
		is.NotError(t, err)
		lines := matching(s, "expires")
		is.Equal(t, 4, len(lines))
		// Which task made its timeout first is up to the seed, and the timeouts expire in that order.
		var made []string
		for _, line := range s.Lines() {
			if _, step, ok := strings.Cut(line, " "); ok && strings.HasPrefix(step, "waiter-") && strings.HasSuffix(step, " resumes from start") {
				made = append(made, strings.TrimSuffix(step, " resumes from start"))
			}
		}
		for i, line := range lines {
			is.Equal(t, "timeout of "+made[i]+" after 1s expires", line)
		}
	})
}

func TestScheduler_Go(t *testing.T) {
	t.Run("should return a task whose wait fails with the context's error when the context is done first", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			sleeper := s.Go(ctx, "sleeper", func(ctx context.Context) {
				is.NotError(t, s.Sleep(ctx, time.Hour))
			})

			waitCtx, cancel := context.WithCancel(ctx)
			canceller := s.Go(ctx, "canceller", func(ctx context.Context) {
				is.NotError(t, s.Sleep(ctx, time.Second))
				cancel()
			})
			is.Error(t, context.Canceled, sleeper.Wait(waitCtx))
			is.Equal(t, time.Second, since(s))

			is.NotError(t, canceller.Wait(ctx))
			is.NotError(t, sleeper.Wait(ctx))
			is.Equal(t, time.Hour, since(s))
		})
		is.NotError(t, err)
	})

	t.Run("should panic when a running task has the name already", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			defer func() {
				is.Equal(t, "sim: a task named main is already running", fmt.Sprint(recover()))
			}()
			s.Go(ctx, "main", func(context.Context) {})
		})
		is.NotError(t, err)
	})

	t.Run("should name the tasks of an instance after it", func(t *testing.T) {
		s, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			is.NotError(t, a.Go(ctx, "worker", func(ctx context.Context) {
				a.Trace(ctx, "runs")
			}).Wait(ctx))
		})
		is.NotError(t, err)
		is.EqualSlice(t, []string{"a/worker runs"}, matching(s, "runs"))
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

	t.Run("should keep every permit after acquires that a cancellation or a timeout ended", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			is.NotError(t, a.Go(ctx, "worker", func(ctx context.Context) {
				m := a.NewSemaphore("m", 2)
				is.NotError(t, m.Acquire(ctx))
				is.NotError(t, m.Acquire(ctx))
				for range 3 {
					timeoutCtx, cancel := a.WithTimeout(ctx, time.Second)
					is.Error(t, context.DeadlineExceeded, m.Acquire(timeoutCtx))
					cancel()
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					is.Error(t, context.Canceled, m.Acquire(cancelled))
				}
				m.Release(ctx)
				m.Release(ctx)

				// Both permits are free, and no more.
				timeoutCtx, cancel := a.WithTimeout(ctx, time.Second)
				defer cancel()
				is.NotError(t, m.Acquire(timeoutCtx))
				is.NotError(t, m.Acquire(timeoutCtx))
				is.Error(t, context.DeadlineExceeded, m.Acquire(timeoutCtx))
				m.Release(ctx)
				m.Release(ctx)
			}).Wait(ctx))
		})
		is.NotError(t, err)
	})

	t.Run("should let the seed decide whether a task that waits for the permit runs before the task that released it goes on", func(t *testing.T) {
		orders := map[string]int{}
		for seed := range uint64(50) {
			s, err := schedule(t, seed, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
				m := s.NewSemaphore("m", 1)
				is.NotError(t, m.Acquire(ctx))
				waiter := s.Go(ctx, "waiter", func(ctx context.Context) {
					is.NotError(t, m.Acquire(ctx))
					s.Trace(ctx, "acquires")
					m.Release(ctx)
				})
				is.NotError(t, s.Sleep(ctx, time.Second))
				m.Release(ctx)
				s.Trace(ctx, "goes on")
				is.NotError(t, waiter.Wait(ctx))
			})
			is.NotError(t, err)
			first := slices.IndexFunc(s.Lines(), func(line string) bool {
				return strings.HasSuffix(line, " acquires") || strings.HasSuffix(line, " goes on")
			})
			orders[strings.Fields(s.Lines()[first])[1]]++
		}
		t.Logf("which task went first: %v", orders)
		is.True(t, orders["waiter"] > 0 && orders["main"] > 0, "the same task always went first")
	})

	t.Run("should panic on the release of a semaphore that is not held", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			m := s.NewSemaphore("m", 2)
			is.NotError(t, m.Acquire(ctx))
			m.Release(ctx)
			defer func() {
				is.Equal(t, "sim: release of a semaphore that is not held", fmt.Sprint(recover()))
			}()
			m.Release(ctx)
		})
		is.NotError(t, err)
	})

	t.Run("should return the context's error when the context is done, also with a permit free", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			m := s.NewSemaphore("m", 1)
			ctx, cancel := context.WithCancel(ctx)
			cancel()
			is.Error(t, context.Canceled, m.Acquire(ctx))
		})
		is.NotError(t, err)
	})
}

func TestScheduler_NewInstance(t *testing.T) {
	t.Run("should panic on an instance whose name is empty or has a slash", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			for _, name := range []string{"", "a/b"} {
				func() {
					defer func() {
						is.Equal(t, "sim: an instance needs a name without a slash", fmt.Sprint(recover()))
					}()
					s.NewInstance(name)
				}()
			}
		})
		is.NotError(t, err)
	})
}

func TestInstance_clocks(t *testing.T) {
	t.Run("should start the wall clock at the simulated time and the monotonic clock at zero", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			is.NotError(t, s.Sleep(ctx, time.Hour))
			a := s.NewInstance("a")
			is.Equal(t, time.Hour, since(a))
			is.Equal(t, time.Duration(0), a.Monotonic())
			is.NotError(t, s.Sleep(ctx, time.Second))
			is.Equal(t, time.Hour+time.Second, since(a))
			is.Equal(t, time.Second, a.Monotonic())
		})
		is.NotError(t, err)
	})

	t.Run("should give the test clocks that both read the simulated time, whatever the instances' clocks do", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			a.ShiftWall(-time.Hour)
			a.StopWall()
			a.StopMonotonic()
			is.NotError(t, s.Sleep(ctx, time.Minute))
			is.Equal(t, time.Minute, s.Monotonic())
			is.Equal(t, sim.Epoch().Add(time.Minute), s.Wall())
		})
		is.NotError(t, err)
	})

	t.Run("should shift the wall clock of one instance forward and backward, and leave its monotonic clock and other instances alone", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a, b := s.NewInstance("a"), s.NewInstance("b")
			a.ShiftWall(time.Minute)
			is.Equal(t, time.Minute, since(a))
			a.ShiftWall(-time.Hour)
			is.Equal(t, time.Minute-time.Hour, since(a))
			is.Equal(t, time.Duration(0), since(b))
			is.Equal(t, time.Duration(0), since(s))

			is.NotError(t, s.Sleep(ctx, time.Second))
			is.Equal(t, time.Minute-time.Hour+time.Second, since(a))
			is.Equal(t, time.Second, a.Monotonic())
		})
		is.NotError(t, err)
	})

	t.Run("should hold the monotonic clock still while the wall clock goes on, and go on from where it stood", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			is.NotError(t, s.Sleep(ctx, time.Second))
			a.StopMonotonic()
			is.NotError(t, s.Sleep(ctx, time.Minute))
			is.Equal(t, time.Second, a.Monotonic())
			is.Equal(t, time.Minute+time.Second, since(a))
			a.StartMonotonic()
			is.NotError(t, s.Sleep(ctx, time.Second))
			is.Equal(t, 2*time.Second, a.Monotonic())
		})
		is.NotError(t, err)
	})

	t.Run("should hold the wall clock still while the monotonic clock goes on, and go on from where it stood", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			is.NotError(t, s.Sleep(ctx, time.Second))
			a.StopWall()
			a.StopWall()
			is.NotError(t, s.Sleep(ctx, time.Minute))
			is.Equal(t, time.Second, since(a))
			is.Equal(t, time.Minute+time.Second, a.Monotonic())
			a.ShiftWall(time.Hour)
			is.Equal(t, time.Hour+time.Second, since(a))
			a.StartWall()
			a.StartWall()
			is.NotError(t, s.Sleep(ctx, time.Second))
			is.Equal(t, time.Hour+2*time.Second, since(a))
		})
		is.NotError(t, err)
	})

	t.Run("should hold the sleeps and timeouts of an instance still with its monotonic clock", func(t *testing.T) {
		s, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			sleeper := a.Go(ctx, "sleeper", func(ctx context.Context) {
				is.NotError(t, a.Sleep(ctx, 10*time.Second))
				a.Trace(ctx, "woke with the monotonic clock at %v", a.Monotonic())
				timeoutCtx, cancel := a.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				is.True(t, errors.Is(a.Sleep(timeoutCtx, time.Hour), context.DeadlineExceeded))
				a.Trace(ctx, "timed out with the monotonic clock at %v", a.Monotonic())
			})
			// The test's own sleeps run on the simulated time, which the instance's clock does not hold.
			is.NotError(t, s.Sleep(ctx, 5*time.Second))
			a.StopMonotonic()
			is.NotError(t, s.Sleep(ctx, time.Minute))
			a.StartMonotonic()
			is.NotError(t, s.Sleep(ctx, 10*time.Second))
			a.StopMonotonic()
			is.NotError(t, s.Sleep(ctx, time.Minute))
			a.StartMonotonic()
			is.NotError(t, sleeper.Wait(ctx))
		})
		is.NotError(t, err)
		is.EqualSlice(t, []string{
			"a/sleeper woke with the monotonic clock at 10s",
			"a/sleeper timed out with the monotonic clock at 20s",
		}, matching(s, "monotonic clock at"))
		var at []string
		for _, line := range s.Lines() {
			if strings.Contains(line, "monotonic clock at") {
				at = append(at, strings.Fields(line)[0])
			}
		}
		is.EqualSlice(t, []string{"1m10s", "2m20s"}, at)
	})

	t.Run("should keep a clock that stands still where it stopped first, and start it only once", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			is.NotError(t, s.Sleep(ctx, time.Second))
			a.StopWall()
			a.StopMonotonic()
			is.NotError(t, s.Sleep(ctx, time.Minute))
			a.StopWall()
			a.StopMonotonic()
			is.NotError(t, s.Sleep(ctx, time.Minute))
			is.Equal(t, time.Second, since(a))
			is.Equal(t, time.Second, a.Monotonic())

			a.StartWall()
			a.StartMonotonic()
			is.NotError(t, s.Sleep(ctx, time.Minute))
			a.StartWall()
			a.StartMonotonic()
			is.Equal(t, time.Minute+time.Second, since(a))
			is.Equal(t, time.Minute+time.Second, a.Monotonic())
		})
		is.NotError(t, err)
	})

	t.Run("should trace every move of a clock, with the instance and the time", func(t *testing.T) {
		s, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			is.NotError(t, s.Sleep(ctx, time.Second))
			a.ShiftWall(-time.Minute)
			a.StopWall()
			a.StartWall()
			a.StopMonotonic()
			a.StartMonotonic()
		})
		is.NotError(t, err)
		is.EqualSlice(t, []string{
			"0s a starts",
			"1s a shifts its wall clock by -1m0s",
			"1s a stops its wall clock",
			"1s a starts its wall clock",
			"1s a stops its monotonic clock",
			"1s a starts its monotonic clock",
		}, slices.DeleteFunc(s.Lines(), func(line string) bool { return !strings.Contains(line, " a ") }))
	})

	t.Run("should sleep until the context is done on an instance that started late, when the duration is longer than its clock can hold", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			is.NotError(t, s.Sleep(ctx, time.Hour))
			a := s.NewInstance("a")
			a.StopMonotonic()
			is.NotError(t, s.Sleep(ctx, time.Minute))
			a.StartMonotonic()

			sleepCtx, cancel := context.WithCancel(ctx)
			sleeper := a.Go(sleepCtx, "sleeper", func(ctx context.Context) {
				is.Error(t, context.Canceled, a.Sleep(ctx, math.MaxInt64-time.Minute))
				is.Equal(t, 2*time.Hour+time.Minute, since(s))
			})
			is.NotError(t, s.Sleep(ctx, time.Hour))
			cancel()
			is.NotError(t, sleeper.Wait(ctx))
		})
		is.NotError(t, err)
	})

	t.Run("should report a deadlock when every task sleeps on a monotonic clock that stands still", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			a.StopMonotonic()
			is.NotError(t, a.Go(ctx, "sleeper", func(ctx context.Context) {
				_ = a.Sleep(ctx, time.Second)
			}).Wait(ctx))
		})
		is.Equal(t, "deadlock: no task can run: a/sleeper at sleep, main at wait for a/sleeper", errorString(err))
	})

	t.Run("should let a task of an instance whose monotonic clock stands still yield, take locks, and start tasks", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{Jitter: time.Second}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			a.StopMonotonic()
			m := a.NewSemaphore("m", 1)
			is.NotError(t, a.Go(ctx, "worker", func(ctx context.Context) {
				for range 20 {
					is.NotError(t, m.Acquire(ctx))
					a.Yield(ctx, "inside")
					m.Release(ctx)
				}
				is.NotError(t, a.Go(ctx, "child", func(ctx context.Context) { a.Yield(ctx, "child") }).Wait(ctx))
			}).Wait(ctx))
			is.Equal(t, time.Duration(0), a.Monotonic())
			is.True(t, since(s) > 0, "jitter should have moved simulated time")
		})
		is.NotError(t, err)
	})
}

func TestInstance_Crash(t *testing.T) {
	t.Run("should never run a task of a crashed instance again, and end it after every other task", func(t *testing.T) {
		for seed := range uint64(50) {
			var events []string
			var mu sync.Mutex
			event := func(e string) {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, e)
			}
			_, err := schedule(t, seed, sim.Options{Jitter: time.Millisecond}, func(ctx context.Context, s *sim.Scheduler) {
				a := s.NewInstance("a")
				m := a.NewSemaphore("m", 1)
				var tasks []host.Task
				for i := range 3 {
					tasks = append(tasks, a.Go(ctx, fmt.Sprint("worker-", i), func(ctx context.Context) {
						defer event(fmt.Sprint("worker-", i, " ends"))
						for {
							is.NotError(t, m.Acquire(ctx))
							event(fmt.Sprint("worker-", i, " runs"))
							_ = a.Sleep(ctx, time.Second)
							m.Release(ctx)
						}
					}))
				}
				is.NotError(t, s.Sleep(ctx, 10*time.Second))
				a.Crash()
				event("crash")
				for _, task := range tasks {
					is.NotError(t, task.Wait(ctx))
				}
				is.NotError(t, s.Sleep(ctx, time.Hour))
				event("main ends")
			})
			is.NotError(t, err)

			crash := slices.Index(events, "crash")
			is.True(t, crash > 0)
			is.Equal(t, "main ends", events[crash+1])
			for _, e := range events[crash+2:] {
				is.True(t, strings.HasSuffix(e, " ends"), e)
			}
			is.Equal(t, crash+5, len(events))
		}
	})

	t.Run("should let a crashed task go on until its next wait when it crashed its own instance", func(t *testing.T) {
		s, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			is.NotError(t, a.Go(ctx, "worker", func(ctx context.Context) {
				a.Crash()
				a.Trace(ctx, "goes on")
				a.Yield(ctx, "after the crash")
				a.Trace(ctx, "never gets here")
			}).Wait(ctx))
		})
		is.NotError(t, err)
		is.EqualSlice(t, []string{"a/worker goes on"}, matching(s, "a/worker go"))
		is.EqualSlice(t, nil, matching(s, "never"))
	})

	t.Run("should start no task of a crashed instance", func(t *testing.T) {
		s, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			a.Crash()
			is.NotError(t, a.Go(ctx, "worker", func(ctx context.Context) {
				a.Trace(ctx, "never runs")
			}).Wait(ctx))
		})
		is.NotError(t, err)
		is.EqualSlice(t, nil, matching(s, "never"))
	})

	t.Run("should end a crashed task that waits in a deferred call at once, also in a goroutine it started", func(t *testing.T) {
		var ended atomic.Int64
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			m := a.NewSemaphore("m", 1)
			a.Go(ctx, "worker", func(ctx context.Context) {
				is.NotError(t, m.Acquire(ctx))
				defer ended.Add(1)
				defer m.Release(ctx)
				defer a.Go(ctx, "started in a deferred call", func(context.Context) { t.Error("runs") })
				_ = a.Sleep(ctx, time.Hour)
			})
			is.NotError(t, s.Sleep(ctx, time.Second))
			a.Crash()
		})
		is.NotError(t, err)
		is.Equal(t, 1, ended.Load())
	})

	t.Run("should not apply what a crashed task sent, if it crashed before the request arrived", func(t *testing.T) {
		for seed := range uint64(100) {
			var applied []int
			_, err := schedule(t, seed, sim.Options{Jitter: time.Millisecond}, func(ctx context.Context, s *sim.Scheduler) {
				a := s.NewInstance("a")
				// send is a request through the network of the test, which arrives after a second.
				send := func(ctx context.Context, i int) {
					_ = s.Sleep(ctx, time.Second)
					applied = append(applied, i)
				}
				a.Go(ctx, "sender", func(ctx context.Context) {
					for i := 0; ; i++ {
						send(ctx, i)
						a.Yield(ctx, "between")
					}
				})
				is.NotError(t, s.Sleep(ctx, 10*time.Second+time.Duration(seed)*time.Millisecond))
				a.Crash()
				n := len(applied)
				is.NotError(t, s.Sleep(ctx, time.Hour))
				is.Equal(t, n, len(applied))
			})
			is.NotError(t, err)
			is.True(t, len(applied) >= 9 && len(applied) <= 10, fmt.Sprint(applied))
		}
	})

	t.Run("should fail when a deferred call of a crashed task starts a task that did not crash", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			a.Go(ctx, "worker", func(ctx context.Context) {
				defer s.Go(ctx, "cleanup", func(ctx context.Context) {
					t.Error("runs")
				})
				_ = a.Sleep(ctx, time.Hour)
			})
			is.NotError(t, s.Sleep(ctx, time.Second))
			a.Crash()
		})
		is.Equal(t, "task cleanup starts while the run ends, after every task that did not crash has returned", errorString(err))
	})

	t.Run("should report a crash to the test and to tasks of other instances", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a, b := s.NewInstance("a"), s.NewInstance("b")
			is.True(t, !a.Crashed())
			a.Crash()
			is.True(t, a.Crashed())
			is.True(t, !b.Crashed())
			is.NotError(t, b.Go(ctx, "watcher", func(ctx context.Context) {
				is.True(t, a.Crashed())
				is.True(t, !b.Crashed())
			}).Wait(ctx))
		})
		is.NotError(t, err)
	})

	t.Run("should never expire a timeout of a crashed instance", func(t *testing.T) {
		s, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			a.Go(ctx, "waiter", func(ctx context.Context) {
				timeoutCtx, cancel := a.WithTimeout(ctx, time.Minute)
				defer cancel()
				_ = a.Sleep(timeoutCtx, time.Hour)
			})
			is.NotError(t, s.Sleep(ctx, time.Second))
			a.Crash()
			is.NotError(t, s.Sleep(ctx, time.Hour))
		})
		is.NotError(t, err)
		is.EqualSlice(t, nil, matching(s, "expires"))
	})

	t.Run("should report a deadlock, without moving simulated time, when the only wait on time belongs to a crashed instance", func(t *testing.T) {
		s, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			m := s.NewSemaphore("m", 1)
			is.NotError(t, m.Acquire(ctx))
			a.Go(ctx, "sleeper", func(ctx context.Context) {
				timeoutCtx, cancel := a.WithTimeout(ctx, time.Minute)
				defer cancel()
				_ = a.Sleep(timeoutCtx, time.Hour)
			})
			is.NotError(t, s.Sleep(ctx, time.Second))
			a.Crash()
			s.Go(ctx, "waiter", func(ctx context.Context) { _ = m.Acquire(ctx) })
			is.NotError(t, m.Acquire(ctx))
		})
		is.Equal(t, "deadlock: no task can run: main at acquire of m, waiter at acquire of m", errorString(err))
		is.Equal(t, time.Second, since(s))
		is.EqualSlice(t, nil, matching(s, "expires"))
	})

	t.Run("should panic on a second instance with the same name", func(t *testing.T) {
		_, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			a := s.NewInstance("a")
			a.Crash()
			defer func() {
				is.Equal(t, "sim: an instance named a exists already", fmt.Sprint(recover()))
			}()
			s.NewInstance("a")
		})
		is.NotError(t, err)
	})
}

func TestScheduler_Buggify(t *testing.T) {
	// visit four sites a hundred times each, in two instances, and record which fired.
	visit := func(t *testing.T, seed uint64, b sim.Buggify) (*sim.Scheduler, map[string]int) {
		t.Helper()
		fired := map[string]int{}
		s, err := schedule(t, seed, sim.Options{Buggify: b}, func(ctx context.Context, s *sim.Scheduler) {
			for _, name := range []string{"a", "b"} {
				inst := s.NewInstance(name)
				is.NotError(t, inst.Go(ctx, "worker", func(ctx context.Context) {
					for range 50 {
						for _, site := range []string{"one", "two", "three", "four"} {
							if inst.Buggify(ctx, site) {
								fired[site]++
							}
						}
					}
				}).Wait(ctx))
			}
		})
		is.NotError(t, err)
		return s, fired
	}

	t.Run("should never fire and never draw from the seed when buggify is off", func(t *testing.T) {
		s, fired := visit(t, 1, sim.Buggify{})
		is.Equal(t, 0, len(fired))
		is.EqualSlice(t, nil, matching(s, "buggify"))
		withoutVisits, err := schedule(t, 1, sim.Options{}, func(ctx context.Context, s *sim.Scheduler) {
			for _, name := range []string{"a", "b"} {
				is.NotError(t, s.NewInstance(name).Go(ctx, "worker", func(context.Context) {}).Wait(ctx))
			}
		})
		is.NotError(t, err)
		is.Equal(t, withoutVisits.Uint64(), s.Uint64())
	})

	t.Run("should always fire with both probabilities at one", func(t *testing.T) {
		_, fired := visit(t, 1, sim.Buggify{On: true, Activation: 1, Fire: 1})
		is.Equal(t, 4, len(fired))
		for _, n := range fired {
			is.Equal(t, 100, n)
		}
	})

	t.Run("should buggify through the host of the test, with the task's name in the trace", func(t *testing.T) {
		s, err := schedule(t, 1, sim.Options{Buggify: sim.Buggify{On: true, Activation: 1, Fire: 1}}, func(ctx context.Context, s *sim.Scheduler) {
			is.True(t, s.Buggify(ctx, "site"))
		})
		is.NotError(t, err)
		is.EqualSlice(t, []string{"main switches buggify on site for the run", "main buggify site fires"}, matching(s, "buggify"))
	})

	t.Run("should switch each site on or off once per run, in the trace, and fire the same with the same seed", func(t *testing.T) {
		var on, sites, fires, visits int
		for seed := range uint64(200) {
			s, fired := visit(t, seed, sim.Buggify{On: true})
			again, firedAgain := visit(t, seed, sim.Buggify{On: true})
			is.EqualSlice(t, s.Lines(), again.Lines())
			is.Equal(t, fmt.Sprint(fired), fmt.Sprint(firedAgain))

			switches := matching(s, "switches buggify")
			is.Equal(t, 4, len(switches))
			for _, line := range switches {
				sites++
				if strings.Contains(line, " on ") {
					on++
					visits += 100
				}
			}
			var n int
			for _, count := range fired {
				n += count
			}
			is.Equal(t, n, len(matching(s, " fires")))
			fires += n
		}
		t.Logf("%v of %v sites on, and %v fires in %v visits of a site that is on", on, sites, fires, visits)
		is.True(t, on > sites/8 && on < sites*3/8, "about a quarter of sites should be on")
		is.True(t, fires > visits/8 && fires < visits*3/8, "about a quarter of visits should fire")
	})
}

// everything is a workload that uses the abilities of the scheduler together, for the check that a
// seed gives the same run twice. Instances have workers that share a lock and a pool, wait with
// timeouts and with contexts derived from them, hit a buggify point, draw numbers from the seed, and
// start and wait for tasks of their own, which wait through the host of the test. The test's own
// tasks share a semaphore, and one of them moves each kind of clock, crashes an instance, and starts
// it again. Another instance crashes itself. The workload fails t if a semaphore lets in more holders
// than it has permits, or has lost a permit at the end.
func everything(t *testing.T) func(ctx context.Context, s *sim.Scheduler) {
	return func(ctx context.Context, s *sim.Scheduler) {
		start := func(name string) (*sim.Instance, []host.Task) {
			inst := s.NewInstance(name)
			lock, pool := inst.NewSemaphore("lock", 1), inst.NewSemaphore("pool", 2)
			holders := map[host.Semaphore]int{}
			var workers []host.Task
			for i := range 3 {
				workers = append(workers, inst.Go(ctx, fmt.Sprint("worker-", i), func(ctx context.Context) {
					// A crashed worker ends when the run ends, and the trace pins the order.
					defer inst.Trace(ctx, "ends")
					for j := range 6 {
						m, permits := pool, 2
						if inst.Uint64()%2 == 0 {
							m, permits = lock, 1
						}
						timeoutCtx, cancel := inst.WithTimeout(ctx, time.Duration(1+inst.Uint64()%3)*time.Second)
						if err := m.Acquire(timeoutCtx); err != nil {
							inst.Trace(ctx, "gives up %v: %v", j, err)
							cancel()
							continue
						}
						holders[m]++
						if holders[m] > permits {
							t.Errorf("%v holders of a semaphore with %v permits", holders[m], permits)
						}
						if inst.Buggify(ctx, "hold longer") {
							_ = inst.Sleep(ctx, 2*time.Second)
						}
						derived, cancelDerived := context.WithCancel(timeoutCtx)
						err := inst.Sleep(derived, time.Duration(inst.Uint64()%1500)*time.Millisecond)
						cancelDerived()
						inst.Yield(ctx, "inside")
						holders[m]--
						m.Release(ctx)
						cancel()
						inst.Trace(ctx, "done %v with %v, at %v on the wall clock and %v on the monotonic clock", j, err, since(inst), inst.Monotonic())
					}
					child := inst.Go(ctx, fmt.Sprint("child-", i), func(ctx context.Context) {
						s.Yield(ctx, "through the test")
						inst.Trace(ctx, "ran")
					})
					_ = child.Wait(ctx)
				}))
			}
			// The check waits until the workers have returned, and then takes every permit.
			check := inst.Go(ctx, "check", func(ctx context.Context) {
				for _, worker := range workers {
					_ = worker.Wait(ctx)
				}
				timeoutCtx, cancel := inst.WithTimeout(ctx, time.Second)
				defer cancel()
				for _, m := range []host.Semaphore{lock, pool, pool} {
					if err := m.Acquire(timeoutCtx); err != nil {
						t.Errorf("a permit of instance %v is lost: %v", name, err)
					}
				}
			})
			return inst, append(workers, check)
		}

		world := s.NewSemaphore("world", 1)
		ticker := s.Go(ctx, "ticker", func(ctx context.Context) {
			for range 5 {
				_ = world.Acquire(ctx)
				s.Trace(ctx, "ticks at %v, %v", since(s), s.Monotonic())
				_ = s.Sleep(ctx, time.Duration(s.Uint64()%3000)*time.Millisecond)
				world.Release(ctx)
			}
		})

		a, aTasks := start("a")
		b, bTasks := start("b")
		c, cTasks := start("c")
		c.Go(ctx, "crasher", func(ctx context.Context) {
			_ = c.Sleep(ctx, time.Duration(c.Uint64()%5000)*time.Millisecond)
			c.Crash()
			c.Trace(ctx, "goes on after the crash")
			c.Yield(ctx, "after the crash")
		})

		pause := func() { _ = s.Sleep(ctx, time.Duration(s.Uint64()%2000)*time.Millisecond) }
		for range 4 {
			pause()
			_ = world.Acquire(ctx)
			inst := []*sim.Instance{a, b}[s.Uint64()%2]
			switch s.Uint64() % 3 {
			case 0:
				inst.ShiftWall(time.Duration(s.Uint64()%20)*time.Second - 10*time.Second)
			case 1:
				inst.StopWall()
				pause()
				inst.StartWall()
			case 2:
				inst.StopMonotonic()
				pause()
				inst.StartMonotonic()
			}
			world.Release(ctx)
		}
		a.Crash()
		if !a.Crashed() || b.Crashed() {
			t.Error("a crash of a was not reported, or one of b was")
		}
		_, a2Tasks := start("a2")
		for _, task := range slices.Concat(aTasks, bTasks, cTasks, a2Tasks, []host.Task{ticker}) {
			_ = task.Wait(ctx)
		}
	}
}

// after is the first line of the trace from the one that ends with event on that bad matches, the
// empty string if there is none, or a message if no line ends with event.
func after(lines []string, event string, bad func(line string) bool) string {
	i := slices.IndexFunc(lines, func(line string) bool { return strings.HasSuffix(line, event) })
	if i < 0 {
		return "no line ends with" + event
	}
	if j := slices.IndexFunc(lines[i:], bad); j >= 0 {
		return lines[i+j]
	}
	return ""
}

// TestSimulation runs every seed twice and compares the traces, with [everything] as the workload.
func TestSimulation(t *testing.T) {
	t.Run("should give the same trace when a seed is run twice, with instances, clocks that move, crashes, buggify, timeouts, and semaphores together", func(t *testing.T) {
		options := sim.Options{Jitter: 50 * time.Millisecond, Buggify: sim.Buggify{On: true}}
		n := uint64(200)
		if v := os.Getenv("S4QL_SIM_SEEDS"); v != "" {
			var err error
			n, err = strconv.ParseUint(v, 10, 64)
			is.NotError(t, err)
		}
		traces := map[string]bool{}
		var all []string
		// cut counts the seeds where the crash of a cut off the work of one of its workers.
		var cut int
		for seed := range n {
			first, err := schedule(t, seed, options, everything(t))
			is.NotError(t, err)
			second, err := schedule(t, seed, options, everything(t))
			is.NotError(t, err)
			lines := first.Lines()
			is.EqualSlice(t, lines, second.Lines(), fmt.Sprint("seed ", seed))
			is.Equal(t, first.Steps(), second.Steps(), fmt.Sprint("seed ", seed))
			is.Equal(t, first.Uint64(), second.Uint64(), fmt.Sprint("seed ", seed))
			traces[strings.Join(lines, "\n")] = true
			all = append(all, lines...)

			// No task of an instance runs after the crash, and no timeout of it expires, except that the
			// task that crashed it goes on to its next wait.
			for _, name := range []string{"a", "c"} {
				bad := func(line string) bool {
					fields := strings.Fields(line)
					return strings.HasPrefix(fields[1], name+"/") && strings.Contains(line, " resumes from ") ||
						strings.Contains(line, " timeout of "+name+"/") && strings.HasSuffix(line, " expires")
				}
				is.Equal(t, "", after(lines, " "+name+" crashes", bad), fmt.Sprint("seed ", seed))
			}
			for i := range 3 {
				worker := fmt.Sprint(" a/worker-", i, " ")
				var finished int
				for _, line := range lines {
					if strings.Contains(line, worker+"done ") || strings.Contains(line, worker+"gives up ") {
						finished++
					}
				}
				if finished < 6 {
					cut++
					break
				}
			}
		}
		t.Logf("%v distinct traces from %v seeds, and the crash cut off work in %v", len(traces), n, cut)
		is.Equal(t, int(n), len(traces))
		is.True(t, cut > 0, "the crash never cut off any work")
		joined := strings.Join(all, "\n")
		for _, event := range []string{" expires", " gives up ", " fires", "a2/worker-0 resumes", "a2/child-0 ran", "b/check resumes",
			"ticker ticks", "a/worker-0 ends", "c/crasher goes on after the crash", "shifts its wall clock", "stops its wall clock", "starts its wall clock",
			"stops its monotonic clock", "starts its monotonic clock"} {
			is.True(t, strings.Contains(joined, event), "no seed has "+event)
		}
	})
}

// TestScheduler_window shows the limit of the scheduler: it switches tasks only where they wait, so a
// race in a stretch of code without such a point never happens under it.
func TestScheduler_window(t *testing.T) {
	// claim runs four tasks that each take a flag if it is not taken, and returns how many took it. The
	// check and the act are not one step, which is the bug.
	claim := func(ctx context.Context, h host.Host, between func(ctx context.Context)) int64 {
		var taken atomic.Bool
		var owners atomic.Int64
		var tasks []host.Task
		for i := range 4 {
			tasks = append(tasks, h.Go(ctx, fmt.Sprint("worker-", i), func(ctx context.Context) {
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

	// With the scheduler off the race can happen, but rarely and not on demand, so this only logs how
	// often it did.
	t.Run("should log how often tasks that run in parallel find the race without a yield", func(t *testing.T) {
		var found int
		for range 1000 {
			synctest.Test(t, func(t *testing.T) {
				r := sim.NewFree(sim.Buggify{})
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

// TestSimulationSpeed logs how fast the scheduler is on this machine, with tasks that only yield. It
// is a test and not a benchmark because a bubble needs a [testing.T]. It only runs when the
// environment variable S4QL_SIM_SPEED is set, because it asserts nothing that the other tests do not.
func TestSimulationSpeed(t *testing.T) {
	if os.Getenv("S4QL_SIM_SPEED") == "" {
		t.Skip("S4QL_SIM_SPEED is not set")
	}
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

// TestScheduler_mutexHeldAcrossWait shows a broken rule that hangs the run: a sync.Mutex held while
// its holder waits. The task that wants the mutex is blocked, but not durably, so synctest.Wait
// never returns and the test hangs until go test's timeout. It only runs when the environment
// variable S4QL_SIM_HANG is set, with a short -timeout.
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
