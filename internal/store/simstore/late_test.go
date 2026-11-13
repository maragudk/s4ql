package simstore_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/store"
	"maragu.dev/s4ql/internal/store/simstore"
)

// held runs writes that the faults f hold back, with seed: a caller creates an object, then sends
// a swap of it and a second create of it, and gives each up at once, and sends a third create that
// it waits for. It returns the landings and the trace.
func held(t *testing.T, seed uint64, f simstore.Faults) ([]simstore.Landing, []string) {
	t.Helper()
	var landings []simstore.Landing
	var lines []string
	synctest.Test(t, func(t *testing.T) {
		s := sim.New(seed, sim.Options{})
		is.NotError(t, s.Run(func(ctx context.Context) {
			st := simstore.New(s, simstore.Options{Latency: latency, Faults: f})
			c := st.Client("a", nil)
			patient, cancel := s.WithTimeout(ctx, time.Hour)
			defer cancel()
			v, err := c.Create(patient, "k", strings.NewReader("first"), nil)
			if f.LostRequest == 0 {
				is.NotError(t, err)
			} else {
				// The create was lost, so the swap is from a version that no write gives.
				v = "lost"
			}
			for _, write := range []func(ctx context.Context) error{
				func(ctx context.Context) error {
					_, err := c.Swap(ctx, "k", v, strings.NewReader("second"), nil)
					return err
				},
				func(ctx context.Context) error {
					_, err := c.Create(ctx, "k", strings.NewReader("third"), nil)
					return err
				},
			} {
				ctx, cancel := s.WithTimeout(ctx, time.Nanosecond)
				err := write(ctx)
				cancel()
				is.True(t, err != nil, "a write whose caller gave up succeeded")
			}
			_, _ = c.Create(patient, "other", strings.NewReader("fourth"), nil)
			is.NotError(t, st.Wait(ctx))
			landings = st.Landings()
		}))
		lines = s.Lines()
	})
	return landings, lines
}

func TestStore_Landings(t *testing.T) {
	const seeds = 20
	long := simstore.Faults{Late: 1, LateBy: time.Second, Long: 1, LongBy: 15 * time.Minute}

	t.Run("should hold a write back for up to LongBy by Long, and record its landing, applied or refused", func(t *testing.T) {
		var longer, applied, refused int
		for seed := range uint64(seeds) {
			landings, _ := held(t, seed, long)
			is.Equal(t, 2, len(landings), fmt.Sprint("seed ", seed))
			for _, l := range landings {
				is.Equal(t, "a", l.Client)
				is.Equal(t, "k", l.Key)
				is.True(t, l.Landed-l.Sent <= latency+long.LongBy, fmt.Sprint("seed ", seed, ": a write landed ", l.Landed-l.Sent, " after it was sent"))
				if l.Landed-l.Sent > latency+long.LateBy {
					longer++
				}
				// The swap is applied, and the second create is refused, because the object exists.
				switch {
				case !l.Create && l.Applied:
					applied++
				case l.Create && !l.Applied:
					refused++
				default:
					t.Errorf("seed %v: a landing that is applied %v is a create %v", seed, l.Applied, l.Create)
				}
			}
		}
		t.Logf("of %v landings, %v came later than LateBy allows", 2*seeds, longer)
		is.True(t, longer > seeds, "too few writes were held back for longer than LateBy")
		is.Equal(t, seeds, applied)
		is.Equal(t, seeds, refused)
	})

	t.Run("should hold a write that was lost for up to LongBy by Long, and record its landing", func(t *testing.T) {
		f := long
		f.LostRequest = 1
		var longer int
		for seed := range uint64(seeds) {
			landings, _ := held(t, seed, f)
			// The first and the last create were lost on their way, and are stuck in the network, and
			// so are the two writes whose caller gave up, which are lost when they arrive. Which ones
			// are applied depends on the order in which they land.
			is.True(t, len(landings) >= 2, fmt.Sprint("seed ", seed, ": ", len(landings), " landings"))
			for _, l := range landings {
				is.True(t, l.Landed-l.Sent <= latency+f.LongBy, fmt.Sprint("seed ", seed))
				if l.Landed-l.Sent > latency+f.LateBy {
					longer++
				}
			}
		}
		is.True(t, longer > 0, "no write that was lost was held back for longer than LateBy")
	})

	t.Run("should hold no write back for longer than LateBy without Long, whatever LongBy is", func(t *testing.T) {
		short := long
		short.Long = 0
		without := short
		without.LongBy = 0
		for seed := range uint64(seeds) {
			landings, lines := held(t, seed, short)
			is.Equal(t, 2, len(landings))
			for _, l := range landings {
				is.True(t, l.Landed-l.Sent <= latency+short.LateBy, fmt.Sprint("seed ", seed, ": a write landed ", l.Landed-l.Sent, " after it was sent"))
			}
			_, linesWithout := held(t, seed, without)
			is.EqualSlice(t, linesWithout, lines)
		}
	})

	t.Run("should record no landing of a write that the store applied while its caller waited", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(0, sim.Options{})
			is.NotError(t, s.Run(func(ctx context.Context) {
				st := simstore.New(s, simstore.Options{Latency: latency, Faults: long})
				c := st.Client("a", nil)
				_, err := c.Create(ctx, "k", strings.NewReader("data"), nil)
				is.NotError(t, err)
				_, err = c.Create(ctx, "k", strings.NewReader("data"), nil)
				is.True(t, errors.Is(err, store.ErrExists))
				is.NotError(t, st.Wait(ctx))
				is.Equal(t, 0, len(st.Landings()))
			}))
		})
	})
}
