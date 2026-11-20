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
		// The log holds every acknowledged write, and those that landed of the writes of unknown outcome
		// and of the writes whose call has not ended when the run ends: each write has one event for
		// its start, and one for its end if it ends.
		committed := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What == "committed" })
		unknown := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What == "unknown" })
		started := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.Op == "write" && e.What == "started" })
		ended := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.Op == "write" && e.What != "started" })
		open := len(started) - len(ended)
		is.True(t, len(committed) <= len(s.Bucket.Log) && len(s.Bucket.Log) <= len(committed)+len(unknown)+open,
			fmt.Sprint("the log holds the commits: ", len(s.Bucket.Log), " commits for ", len(committed), " writes acknowledged, ", len(unknown), " of unknown outcome, and ", open, " that have not ended"))
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

	t.Run("should end the calls of a crashed process, and its waits for requests", func(t *testing.T) {
		// A run is the same before the crash as without it, so the crash comes while a call that the run
		// without it has is in progress.
		request := `{"end": 60000, "schedule": [%v]}`
		without := run(t, &e, fmt.Sprintf(request, ""))
		var at float64
		var process string
		for _, c := range find(without, func(e simulator.Event) bool { return e.Kind == "call" && e.What != "started" }) {
			if c.T-*c.Started > 20 {
				at, process = *c.Started+10, c.Process
				break
			}
		}
		is.True(t, at > 0, "a call that takes a while")
		of := func(c simulator.CallState) bool { return c.Process == process }
		res := run(t, &e, fmt.Sprintf(request, fmt.Sprintf(`{"at": %v, "do": "crash", "instance": %q}`, at, process)))
		is.True(t, slices.ContainsFunc(simulator.StateAt(res, at-1).Calls, of), "calls before the crash")
		after := simulator.StateAt(res, at)
		is.True(t, !slices.ContainsFunc(after.Calls, of), "no calls after the crash")
		is.True(t, !slices.ContainsFunc(after.Requests, func(r simulator.RequestState) bool { return r.Process == process && r.Waiting }), "no waits after the crash")
	})

	t.Run("should show a database as closing from the action until its close ends", func(t *testing.T) {
		res := run(t, &e, `{"end": 60000, "instances": 1, "schedule": [{"at": 20000, "do": "close", "instance": "a"}]}`)
		is.Equal(t, "closing", simulator.StateAt(res, 20_000).Instances[0].State)
		closed := find(res, func(e simulator.Event) bool { return e.Kind == "process" && e.What == "closed" })[0]
		is.Equal(t, "closed", simulator.StateAt(res, closed.T).Instances[0].State)
	})

	t.Run("should list a call from its start until its end", func(t *testing.T) {
		res := run(t, &e, `{"end": 30000, "instances": 1}`)
		// A call that takes a while: one can also end at the time of its start.
		var started, ended simulator.Event
		for _, s := range find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What == "started" }) {
			e := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What != "started" && e.Call == s.Call })[0]
			if e.T > s.T {
				started, ended = s, e
				break
			}
		}
		is.True(t, ended.T > started.T, "a call that takes a while")
		has := func(at float64) bool {
			return slices.ContainsFunc(simulator.StateAt(res, at).Calls, func(c simulator.CallState) bool { return c.Call == started.Call })
		}
		is.True(t, has(started.T), "listed at its start")
		is.True(t, !has(ended.T), "not listed at its end")
	})
}
