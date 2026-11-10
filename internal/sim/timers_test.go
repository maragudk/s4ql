package sim_test

import (
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"
)

// TestSynctestTimerOrder records what testing/synctest does with timers that are due at the same fake
// instant, which decides whether waits on time can be left to the runtime's timers.
func TestSynctestTimerOrder(t *testing.T) {
	tests := []struct {
		name string
		// start four waits that are due at the same instant, each of which calls add with its index.
		start func(add func(i int))
	}{
		{
			name: "goroutines that sleep until the same instant should not wake in a fixed order",
			start: func(add func(i int)) {
				for i := range 4 {
					go func() {
						time.Sleep(time.Second)
						add(i)
					}()
				}
			},
		},
		{
			name: "AfterFunc timers due at the same instant should not run in a fixed order",
			start: func(add func(i int)) {
				for i := range 4 {
					time.AfterFunc(time.Second, func() { add(i) })
				}
			},
		},
		{
			name: "a select over timers due at the same instant should not choose in a fixed order",
			start: func(add func(i int)) {
				var timers [4]*time.Timer
				for i := range timers {
					timers[i] = time.NewTimer(time.Second)
				}
				go func() {
					time.Sleep(2 * time.Second)
					for range timers {
						select {
						case <-timers[0].C:
							add(0)
						case <-timers[1].C:
							add(1)
						case <-timers[2].C:
							add(2)
						case <-timers[3].C:
							add(3)
						}
					}
				}()
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			orders := map[string]int{}
			for range 300 {
				synctest.Test(t, func(t *testing.T) {
					var mu sync.Mutex
					var order []int
					test.start(func(i int) {
						mu.Lock()
						defer mu.Unlock()
						order = append(order, i)
					})
					time.Sleep(3 * time.Second)
					synctest.Wait()
					mu.Lock()
					defer mu.Unlock()
					orders[fmt.Sprint(order)]++
				})
			}
			t.Logf("%v distinct orders in 300 runs: %v", len(orders), orders)
			is.True(t, len(orders) > 1, "the order was the same in every run")
		})
	}
}
