package host_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/host"
)

func TestNew(t *testing.T) {
	t.Run("should start a goroutine and wait until it has returned", func(t *testing.T) {
		h := host.New()
		var ran bool
		task := h.Go(t.Context(), "worker", func(context.Context) { ran = true })
		is.NotError(t, task.Wait(t.Context()))
		is.True(t, ran)
	})

	t.Run("should stop waiting for a goroutine when the context is done", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := host.New()
			release := make(chan struct{})
			task := h.Go(t.Context(), "worker", func(context.Context) { <-release })
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			is.Error(t, context.Canceled, task.Wait(ctx))
			close(release)
			is.NotError(t, task.Wait(t.Context()))
		})
	})

	t.Run("should sleep on the clock and stop sleeping when the context is done", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := host.New()
			start := h.Monotonic()
			is.NotError(t, h.Sleep(t.Context(), time.Hour))
			is.Equal(t, time.Hour, h.Monotonic()-start)

			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			is.Error(t, context.Canceled, h.Sleep(ctx, time.Hour))
		})
	})

	t.Run("should end a context after the timeout with the error for an exceeded deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := host.New()
			ctx, cancel := h.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := h.Sleep(ctx, time.Hour)
			is.True(t, errors.Is(err, context.DeadlineExceeded))
			is.Equal(t, time.Second, h.Monotonic())
		})
	})

	t.Run("should let in only as many as there are permits, and the next when a permit is released", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := host.New()
			s := h.NewSemaphore("s", 2)
			is.NotError(t, s.Acquire(t.Context()))
			is.NotError(t, s.Acquire(t.Context()))

			var third atomic.Bool
			task := h.Go(t.Context(), "third", func(ctx context.Context) {
				is.NotError(t, s.Acquire(ctx))
				third.Store(true)
			})
			synctest.Wait()
			is.True(t, !third.Load(), "a third acquire got a permit")

			s.Release(t.Context())
			is.NotError(t, task.Wait(t.Context()))
			is.True(t, third.Load())
		})
	})

	t.Run("should fail an acquire when the context is done, also with a permit free", func(t *testing.T) {
		h := host.New()
		s := h.NewSemaphore("s", 1)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		is.Error(t, context.Canceled, s.Acquire(ctx))
	})

	t.Run("should panic on the release of a semaphore that is not held", func(t *testing.T) {
		h := host.New()
		s := h.NewSemaphore("s", 1)
		defer func() {
			is.Equal(t, "host: release of a semaphore that is not held", recover())
		}()
		s.Release(t.Context())
	})

	t.Run("should read the wall clock without a monotonic reading", func(t *testing.T) {
		h := host.New()
		now := h.Wall()
		is.Equal(t, now, now.Round(0))
		is.True(t, time.Since(now) < time.Minute)
	})

	t.Run("should read a monotonic clock that does not go backward", func(t *testing.T) {
		h := host.New()
		first := h.Monotonic()
		is.True(t, first >= 0)
		is.True(t, h.Monotonic() >= first)
	})

	t.Run("should draw different random numbers", func(t *testing.T) {
		h := host.New()
		seen := map[uint64]bool{}
		for range 100 {
			seen[h.Uint64()] = true
		}
		is.Equal(t, 100, len(seen))
	})

	t.Run("should never buggify", func(t *testing.T) {
		h := host.New()
		for range 1000 {
			is.True(t, !h.Buggify(t.Context(), "site"))
		}
	})
}
