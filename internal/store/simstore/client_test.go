package simstore_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/store"
	"maragu.dev/s4ql/internal/store/simstore"
	"maragu.dev/s4ql/internal/store/storetest"
)

// harness for the conformance suite: each test runs once for each of seeds seeds, under the
// scheduler, against a calm store with latency, so that racing requests overlap.
func harness(seeds int) storetest.Harness {
	return func(t *testing.T, f func(t *testing.T, ctx context.Context, env storetest.Env)) {
		for seed := range uint64(seeds) {
			synctest.Test(t, func(t *testing.T) {
				s := sim.New(seed, sim.Options{Jitter: 10 * time.Millisecond})
				st := simstore.New(s, simstore.Options{Latency: 50 * time.Millisecond})
				err := s.Run(func(ctx context.Context) {
					f(t, ctx, storetest.Env{
						Host:   s,
						Client: func(name string) store.Store { return st.Client(name, nil) },
					})
				})
				is.NotError(t, err)
			})
		}
	}
}

func TestClient(t *testing.T) {
	storetest.Test(t, harness(20))
}
