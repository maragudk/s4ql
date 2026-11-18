//go:build s4ql_browser

package simulator_test

import (
	"fmt"
	"slices"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/simulator"
)

func TestStateAt(t *testing.T) {
	var e simulator.Engine

	t.Run("should follow the position of each cache by the events of the core, as the reads see it", func(t *testing.T) {
		for seed := range 5 {
			res := run(t, &e, fmt.Sprintf(`{"seed": %v, "end": 150000, "buggify": true, "faults": %v, "schedule": [
				{"at": 30000, "do": "crash", "instance": "a"}, {"at": 40000, "do": "restart", "instance": "a"},
				{"at": 60000, "do": "close", "instance": "b"}, {"at": 70000, "do": "open", "instance": "b"}]}`, seed+20, allFaults))
			// Without the reads, the state knows the positions from the events of the core alone.
			withoutCalls := simulator.Response{Events: slices.DeleteFunc(slices.Clone(res.Events), func(e simulator.Event) bool { return e.Kind == "call" })}
			reads := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What == "served" })
			is.True(t, len(reads) > 5, "reads")
			for _, r := range reads {
				// The state that a read sees is fixed after it starts and before it ends, so its position is
				// one that the cache had between the two.
				var positions []string
				for _, at := range []float64{*r.Started, r.T} {
					for _, in := range simulator.StateAt(withoutCalls, at).Instances {
						if in.Process == r.Process {
							positions = append(positions, in.Position)
						}
					}
				}
				is.True(t, slices.Contains(positions, r.Position), fmt.Sprintf("seed %v: the read of call %v at %v saw %v, and the state says %v", seed+20, r.Call, r.T, r.Position[:8], positions))
			}
		}
	})

	t.Run("should hold the log of the store", func(t *testing.T) {
		res := run(t, &e, `{"seed": 2, "end": 120000, "faults": `+allFaults+`}`)
		s := simulator.StateAt(res, 120_000)
		is.Equal(t, res.Summary.Log, len(s.Bucket.Log))
		is.True(t, len(s.Bucket.Log) > 5, "commits")
		is.Equal(t, s.Bucket.Manifest.Head, s.Bucket.Log[len(s.Bucket.Log)-1])
		inLog := 0
		for _, c := range s.Bucket.Commits {
			if c.InLog {
				inLog++
			}
		}
		is.Equal(t, len(s.Bucket.Log), inLog)
		// The log holds every acknowledged write, and those of unknown outcome that landed.
		committed := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What == "committed" })
		unknown := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What == "unknown" })
		is.True(t, len(committed) <= len(s.Bucket.Log) && len(s.Bucket.Log) <= len(committed)+len(unknown), "the log holds the commits")
	})

	t.Run("should list a request in flight from when it is sent until it has ended for both sides", func(t *testing.T) {
		res := run(t, &e, `{"end": 30000, "instances": 1}`)
		sent := find(res, func(e simulator.Event) bool { return e.What == "sent" })[5]
		inFlight := func(at float64) *simulator.RequestState {
			for _, r := range simulator.StateAt(res, at).Requests {
				if r.Request == sent.Request {
					return &r
				}
			}
			return nil
		}
		r := inFlight(sent.T)
		is.True(t, r != nil, "in flight when sent")
		is.Equal(t, "to_store", r.Phase)
		is.True(t, r.Waiting, "waited for")
		returned := find(res, func(e simulator.Event) bool { return e.What == "returned" && e.Request == sent.Request })[0]
		is.True(t, inFlight(returned.T) == nil, "not in flight once returned")
	})

	t.Run("should list a call from its start until its end", func(t *testing.T) {
		res := run(t, &e, `{"end": 30000, "instances": 1}`)
		started := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What == "started" })[3]
		ended := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What != "started" && e.Call == started.Call })[0]
		has := func(at float64) bool {
			return slices.ContainsFunc(simulator.StateAt(res, at).Calls, func(c simulator.CallState) bool { return c.Call == started.Call })
		}
		is.True(t, has(started.T), "listed at its start")
		is.True(t, !has(ended.T), "not listed at its end")
	})
}
