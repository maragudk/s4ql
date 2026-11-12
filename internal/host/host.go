// Package host is everything that library code waits on, reads the time from, or draws randomness
// from, behind one small interface, [Host].
//
// Library code does none of these things in any other way, so that another implementation of the
// interface can run the library one goroutine at a time under a seed. [New] is the implementation for
// production, which maps each method onto ordinary Go.
//
// Code passes the context it was given, or one derived from it, to every method that takes one, so
// that an implementation can tell from the context which goroutine calls.
package host

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"time"
)

// Host is what library code needs from the world: goroutines, locks, waiting, clocks, randomness, and
// the points where a simulation may make it do something unusual.
type Host interface {
	// Go starts f in a new goroutine and returns a handle to wait for it. The name says what the
	// goroutine is for, and no other goroutine of the host that has not returned has it.
	Go(ctx context.Context, name string, f func(ctx context.Context)) Task

	// NewSemaphore with n permits, which is safe to hold while waiting on anything else the host
	// offers. With one permit it is a lock. The name says what the semaphore is for.
	NewSemaphore(name string, n int) Semaphore

	// Sleep for d on the monotonic clock. It fails only if ctx is done first, with the context's
	// error.
	Sleep(ctx context.Context, d time.Duration) error

	// WithTimeout returns a copy of ctx that is done after d on the monotonic clock, or when ctx is
	// done, or when the returned function is called, whichever is first. Its error after the timeout
	// is [context.DeadlineExceeded]. Every wait of the host ends when it is done.
	WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc)

	// Yield marks a point named site where other goroutines may run before this one continues.
	Yield(ctx context.Context, site string)

	// Wall is the time on the wall clock, which can be set forward and backward. It has no monotonic
	// reading, so comparisons and differences of two readings use the wall clock.
	Wall() time.Time

	// Monotonic is the time on the monotonic clock, as a duration since an arbitrary origin that is
	// the same for every reading of one host. It never goes backward, but it can stand still while
	// the wall clock moves.
	Monotonic() time.Duration

	// Uint64 is a random number. In production it is unpredictable, so that two processes never
	// draw the same sequence.
	Uint64() uint64

	// Buggify reports whether to do something legal but unusual at the point named site, such as
	// returning an error or taking a rare branch. It is always false in production.
	Buggify(ctx context.Context, site string) bool
}

// Task is a goroutine started by [Host.Go].
type Task interface {
	// Wait until the goroutine has returned. It fails only if ctx is done first, with the context's
	// error.
	Wait(ctx context.Context) error
}

// Semaphore with a fixed number of permits, from [Host.NewSemaphore].
type Semaphore interface {
	// Acquire a permit, waiting until one is free. It fails only if ctx is done first, with the
	// context's error.
	Acquire(ctx context.Context) error

	// Release a permit. It panics if no permit is held.
	Release(ctx context.Context)
}

// production is the [Host] of [New].
type production struct {
	// start is the origin of the monotonic clock.
	start time.Time
}

// New host for production: real goroutines, the time package's clocks and timers, and random
// numbers from crypto/rand.
func New() Host {
	return &production{start: time.Now()}
}

// Go as [Host] describes. The name is not used.
func (p *production) Go(ctx context.Context, _ string, f func(ctx context.Context)) Task {
	t := make(productionTask)
	go func() {
		defer close(t)
		f(ctx)
	}()
	return t
}

// productionTask is closed when its goroutine has returned.
type productionTask chan struct{}

func (t productionTask) Wait(ctx context.Context) error {
	select {
	case <-t:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NewSemaphore as [Host] describes. The name is not used.
func (p *production) NewSemaphore(_ string, n int) Semaphore {
	return make(productionSemaphore, n)
}

// productionSemaphore holds one value for every permit that is taken.
type productionSemaphore chan struct{}

func (s productionSemaphore) Acquire(ctx context.Context) error {
	// A context that is done wins over a free permit, as it does in every other wait.
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s productionSemaphore) Release(context.Context) {
	select {
	case <-s:
	default:
		panic("host: release of a semaphore that is not held")
	}
}

// Sleep as [Host] describes.
func (p *production) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WithTimeout as [Host] describes. The timers of the time package run on the monotonic clock.
func (p *production) WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

// Yield does nothing, because the Go runtime switches goroutines wherever it likes.
func (p *production) Yield(context.Context, string) {}

// Wall as [Host] describes.
func (p *production) Wall() time.Time {
	return time.Now().Round(0)
}

// Monotonic as [Host] describes, with the origin at [New].
func (p *production) Monotonic() time.Duration {
	return time.Since(p.start)
}

// Uint64 as [Host] describes, from crypto/rand.
func (p *production) Uint64() uint64 {
	var b [8]byte
	// It never fails.
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint64(b[:])
}

// Buggify is always false.
func (p *production) Buggify(context.Context, string) bool {
	return false
}
