// Package library breaks every rule of the lint once, and does the allowed things next to them.
package library

import (
	"context"
	crand "crypto/rand" // want `crypto/rand draws randomness outside the host`
	"math/rand"         // want `math/rand draws randomness outside the host`
	randv2 "math/rand/v2" // want `math/rand/v2 draws randomness outside the host`
	"sync"
	"time"
	. "time"
)

func goStatement() {
	go func() {}() // want `a go statement starts a goroutine outside the host`
}

func clock() {
	_ = time.Now()               // want `time.Now reads the real clock`
	_ = time.Since(time.Time{})  // want `time.Since reads the real clock`
	_ = time.Until(time.Time{})  // want `time.Until reads the real clock`
	time.Sleep(time.Second)      // want `time.Sleep waits on the real clock`
	<-time.After(time.Second)    // want `time.After waits on the real clock`
	time.AfterFunc(0, func() {}) // want `time.AfterFunc starts a goroutine on the real clock`
	_ = time.NewTimer(0)         // want `time.NewTimer waits on the real clock`
	_ = time.NewTicker(1)        // want `time.NewTicker waits on the real clock`
	_ = time.Tick(1)             // want `time.Tick waits on the real clock`
	var _ *time.Timer            // want `time.Timer waits on the real clock`
	var _ *time.Ticker           // want `time.Ticker waits on the real clock`
	now := time.Now              // want `time.Now reads the real clock`
	_ = now
	_ = Now() // want `time.Now reads the real clock`

	// Times and durations are fine.
	_ = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Second).Sub(time.Unix(0, 0))
}

func contexts(ctx context.Context) {
	_, _ = context.WithTimeout(ctx, time.Second)                // want `context.WithTimeout sets a timer on the real clock`
	_, _ = context.WithTimeoutCause(ctx, time.Second, nil)      // want `context.WithTimeoutCause sets a timer on the real clock`
	_, _ = context.WithDeadline(ctx, time.Time{})               // want `context.WithDeadline sets a timer on the real clock`
	_, _ = context.WithDeadlineCause(ctx, time.Time{}, nil)     // want `context.WithDeadlineCause sets a timer on the real clock`
	_ = context.AfterFunc(ctx, func() {})                       // want `context.AfterFunc starts a goroutine`

	// Cancellation and values are fine.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cancel()
}

type embedsWaitGroup struct {
	sync.WaitGroup // want `sync.WaitGroup waits outside the host`
}

func syncs() {
	var _ sync.WaitGroup          // want `sync.WaitGroup waits outside the host`
	_ = sync.NewCond(&sync.Mutex{}) // want `sync.NewCond waits outside the host`
	var _ *sync.Cond              // want `sync.Cond waits outside the host`

	// A mutex that is not held across a wait, and a once, are fine.
	var mu sync.Mutex
	mu.Lock()
	mu.Unlock()
	var once sync.Once
	once.Do(func() {})
}

func randomness() {
	_ = rand.Int()
	_ = randv2.Int()
	_, _ = crand.Read(nil)
}
