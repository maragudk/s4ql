//go:build s4ql_browser

package simulator_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/simulator"
)

// withClient is a request of two instances with no callers in the processes, where a starts alone and
// b stands by, and the client calls a, with the rest of the request in more and the schedule. A more
// without a workload gets one without callers.
func withClient(more, schedule string) string {
	if !strings.Contains(more, `"workload"`) {
		more += `, "workload": {"callers": 0}`
	}
	return `{"instances": 2, "client": {"instance": "a"}, "standby": true` + more + `, "schedule": [` + schedule + `]}`
}

// clientCalls of res: the events of the calls of the client, which started in from to before to.
func clientCalls(res simulator.Response, what string, from, to float64) []simulator.Event {
	return find(res, func(e simulator.Event) bool {
		return e.Kind == "call" && e.Caller == "client" && (what == "" || e.What == what) && e.T >= from && e.T < to
	})
}

// ended calls of the client in res, from from to before to: their events that are not "started".
func ended(res simulator.Response, from, to float64) []simulator.Event {
	return find(res, func(e simulator.Event) bool {
		return e.Kind == "call" && e.Caller == "client" && e.What != "started" && e.T >= from && e.T < to
	})
}

// whats of the events, each once, in order.
func whats(events []simulator.Event) []string {
	var out []string
	for _, e := range events {
		if !slices.Contains(out, e.What) {
			out = append(out, e.What)
		}
	}
	return out
}

// noViolation fails unless every violation in res is excused.
func noViolation(t *testing.T, res simulator.Response, why string) {
	t.Helper()
	for _, e := range find(res, func(e simulator.Event) bool { return e.Kind == "check" && !e.Excused }) {
		t.Errorf("%v: a violation of %v at %v: %v", why, e.Invariant, e.T, e.Text)
	}
}

// eachCall checks that every call of the client in res has one event that ends it, on the instance
// and the process that it started on, and returns the calls in the order they started.
func eachCall(t *testing.T, res simulator.Response) []simulator.Event {
	t.Helper()
	started := map[int]simulator.Event{}
	var order []simulator.Event
	ends := map[int]int{}
	for _, e := range res.Events {
		if e.Kind != "call" || e.Caller != "client" {
			continue
		}
		if e.What == "started" {
			started[e.Call] = e
			order = append(order, e)
			continue
		}
		s, ok := started[e.Call]
		is.True(t, ok, fmt.Sprint("call ", e.Call, " ends without a start"))
		is.Equal(t, s.Instance, e.Instance, fmt.Sprint("the instance of call ", e.Call))
		is.Equal(t, s.Process, e.Process, fmt.Sprint("the process of call ", e.Call))
		ends[e.Call]++
	}
	for i, s := range order {
		// The last call can still be on its way at the end.
		if i < len(order)-1 {
			is.Equal(t, 1, ends[s.Call], fmt.Sprint("the ends of call ", s.Call))
		}
	}
	return order
}

func TestRun_client(t *testing.T) {
	var e simulator.Engine

	t.Run("should start the first instance alone, and the other once the first serves, with the client on the first", func(t *testing.T) {
		res := run(t, &e, withClient(`, "end": 60000`, ""))
		waiting := find(res, func(e simulator.Event) bool { return e.Kind == "process" && e.What == "waiting" })
		is.Equal(t, 1, len(waiting))
		is.Equal(t, "b", waiting[0].Instance)
		taken := find(res, func(e simulator.Event) bool {
			return e.Kind == "core" && e.Event == "lease" && e.Attrs["lease.transition"] == "taken" && e.Attrs["outcome"] == "taken"
		})
		startedB := find(res, func(e simulator.Event) bool { return e.Kind == "process" && e.What == "started" && e.Instance == "b" })
		is.True(t, len(taken) > 0 && len(startedB) == 1, "a takes the lease, and b starts")
		is.Equal(t, "a", taken[0].Instance)
		is.True(t, taken[0].T <= startedB[0].T, fmt.Sprint("a takes the lease at ", taken[0].T, ", and b starts at ", startedB[0].T))
		calls := eachCall(t, res)
		is.True(t, len(calls) > 5, "the client calls")
		is.True(t, calls[0].T >= startedB[0].T, "the first call of the client comes once b has started")
		is.Equal(t, "served", ended(res, 0, 60_000)[0].What)
		// The processes have no callers, and the client calls only a, so b gets no call.
		is.Equal(t, 0, len(find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.Instance == "b" })))
		is.Equal(t, 0, len(find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.Caller != "client" })))
		client := find(res, func(e simulator.Event) bool { return e.Kind == "client" })
		is.Equal(t, 1, len(client))
		is.Equal(t, "started", client[0].What)
		is.Equal(t, "a", client[0].Instance)
		noViolation(t, res, "a client on a")
	})

	t.Run("should keep calling its target through a crash and a restart, which finds it down in between", func(t *testing.T) {
		res := run(t, &e, withClient(`, "end": 150000`, `{"at": 30000, "do": "crash", "instance": "a"}, {"at": 90000, "do": "restart", "instance": "a"}`))
		eachCall(t, res)
		down := ended(res, 30_000, 90_000)
		is.True(t, len(down) > 3, "calls while a is crashed")
		for _, e := range down {
			is.True(t, e.What == "down" || e.What == "lost", e.What)
			is.Equal(t, "a", e.Instance)
		}
		is.True(t, strings.Contains(clientCalls(res, "down", 30_000, 90_000)[0].Error, "crashed"), clientCalls(res, "down", 30_000, 90_000)[0].Error)
		after := ended(res, 90_000, 150_000)
		is.True(t, len(after) > 3, "calls after the restart")
		for _, e := range after {
			is.Equal(t, "a2", e.Process)
		}
		// b takes the lease once that of a has run out, and the restarted a stands by.
		is.True(t, slices.Contains(whats(after), "refused"), fmt.Sprint(whats(after)))
		is.Equal(t, 0, len(find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.Instance == "b" })))
		noViolation(t, res, "a crash of the target")
	})

	t.Run("should keep calling its target through a stop and a start", func(t *testing.T) {
		res := run(t, &e, withClient(`, "end": 120000`, `{"at": 30000, "do": "close", "instance": "a"}, {"at": 60000, "do": "open", "instance": "a"}`))
		eachCall(t, res)
		between := ended(res, 31_000, 60_000)
		is.True(t, len(between) > 3, "calls while a is stopped")
		for _, e := range between {
			is.Equal(t, "down", e.What)
		}
		is.True(t, strings.Contains(between[0].Error, "closed"), between[0].Error)
		after := ended(res, 61_000, 120_000)
		is.True(t, len(after) > 3, "calls after the start")
		// b took the lease at the release of a, so a stands by.
		is.EqualSlice(t, []string{"refused"}, whats(after))
		noViolation(t, res, "a stop of the target")
	})

	t.Run("should keep calling its target through a failed disk, and be served again once it works", func(t *testing.T) {
		res := run(t, &e, withClient(`, "end": 150000, "workload": {"callers": 0, "writes": 100}`,
			`{"at": 30000, "do": "failDisk", "instance": "a", "on": true}, {"at": 60000, "do": "failDisk", "instance": "a"}, {"at": 70000, "do": "client", "instance": "b"}`))
		eachCall(t, res)
		during := ended(res, 30_000, 60_000)
		is.True(t, len(during) > 3, "calls while the disk fails")
		is.True(t, slices.Contains(whats(during), "down"), fmt.Sprint(whats(during)))
		after := ended(res, 75_000, 150_000)
		is.True(t, slices.Contains(whats(after), "committed"), fmt.Sprint(whats(after)))
		noViolation(t, res, "a failed disk of the target")
	})

	t.Run("should move its calls to the instance that a switch names, and finish a call in flight on the old one", func(t *testing.T) {
		// With short pauses, a call of the client starts on a after the hang, and the hang makes it
		// wait for its timeout, past the switch.
		res := run(t, &e, withClient(`, "end": 120000, "workload": {"callers": 0, "think": 100, "writes": 100}`,
			`{"at": 20000, "do": "hangNetwork", "instance": "a", "on": true}, {"at": 30000, "do": "client", "instance": "b"}, {"at": 90000, "do": "client", "instance": "a"}`))
		calls := eachCall(t, res)
		var inFlight *simulator.Event
		for _, c := range calls {
			switch {
			case c.T < 30_000, c.T >= 90_000:
				is.Equal(t, "a", c.Instance, fmt.Sprint("call ", c.Call, " at ", c.T))
			default:
				is.Equal(t, "b", c.Instance, fmt.Sprint("call ", c.Call, " at ", c.T))
			}
			if c.T < 30_000 {
				inFlight = &c
			}
		}
		is.True(t, inFlight != nil, "a call before the switch")
		end := find(res, func(e simulator.Event) bool {
			return e.Kind == "call" && e.Call == inFlight.Call && e.What != "started"
		})
		is.Equal(t, 1, len(end))
		is.True(t, end[0].T > 30_000, fmt.Sprint("the call in flight at the switch ends at ", end[0].T))
		is.Equal(t, "a", end[0].Instance)
		switched := find(res, func(e simulator.Event) bool { return e.Kind == "action" && e.Do == "client" && e.What == "done" })
		is.Equal(t, 2, len(switched))
		is.Equal(t, "b", simulator.StateAt(res, 60_000).Client.Instance)
		is.Equal(t, "a", simulator.StateAt(res, 100_000).Client.Instance)
		noViolation(t, res, "a switch")
	})

	t.Run("should end a call in flight at a crash of its process as lost, which for a write is unknown", func(t *testing.T) {
		// The hang holds the write of the client on a until the crash.
		res := run(t, &e, withClient(`, "end": 40000, "workload": {"callers": 0, "think": 100, "writes": 100}`,
			`{"at": 20000, "do": "hangNetwork", "instance": "a", "on": true}, {"at": 25000, "do": "crash", "instance": "a"}`))
		eachCall(t, res)
		lost := clientCalls(res, "lost", 0, 40_000)
		is.Equal(t, 1, len(lost))
		is.Equal(t, 25_000.0, lost[0].T)
		is.Equal(t, "write", lost[0].Op)
		is.Equal(t, "a", lost[0].Process)
		is.True(t, lost[0].Value > 0, "the key of the write")
		is.True(t, strings.Contains(lost[0].Error, "crashed"), lost[0].Error)
		is.Equal(t, 1, res.Summary.Calls["write lost"])
		is.True(t, res.Summary.Calls["write down"] > 3, "the next calls find a down")
		noViolation(t, res, "a write lost at a crash")
	})

	t.Run("should ignore an action on an instance that has not started, except a fault of the instance", func(t *testing.T) {
		res := run(t, &e, withClient(`, "end": 20000`, `{"at": 0, "do": "crash", "instance": "b"}, {"at": 0, "do": "failDisk", "instance": "b", "on": true},
			{"at": 0, "do": "client", "instance": "b"}, {"at": 0, "do": "failRequest", "request": "read", "instance": "b"}`))
		actions := find(res, func(e simulator.Event) bool { return e.Kind == "action" })
		is.Equal(t, 4, len(actions))
		is.Equal(t, "ignored", actions[0].What)
		is.Equal(t, "the instance has not started", actions[0].Text)
		is.Equal(t, "done", actions[1].What)
		is.Equal(t, "done", actions[2].What)
		is.Equal(t, "done", actions[3].What)
		is.True(t, simulator.StateAt(res, 10).Instances[1].DiskFailing, "the disk of b fails")
		is.Equal(t, "waiting", simulator.StateAt(res, 10).Instances[1].State)

		// An armed fault for b waits for the first read of b, once b has started.
		res = run(t, &e, withClient(`, "end": 20000`, `{"at": 0, "do": "failRequest", "request": "read", "instance": "b"}`))
		failed := find(res, func(e simulator.Event) bool { return e.Kind == "request" && e.What == "failed" })
		is.Equal(t, 1, len(failed))
		is.Equal(t, "b", failed[0].Instance)
	})

	t.Run("should ignore a switch of the client in a run without one", func(t *testing.T) {
		res := run(t, &e, `{"instances": 2, "end": 10000, "schedule": [{"at": 1000, "do": "client", "instance": "b"}]}`)
		actions := find(res, func(e simulator.Event) bool { return e.Kind == "action" })
		is.Equal(t, 1, len(actions))
		is.Equal(t, "ignored", actions[0].What)
		is.Equal(t, "the run has no client", actions[0].Text)
		is.Equal(t, "", simulator.StateAt(res, 10_000).Client.Instance)
	})

	t.Run("should find no violation that is not excused over many seeds with every fault, crashes, stops, and switches", func(t *testing.T) {
		schedule := `{"at": 15000, "do": "crash", "instance": "a"}, {"at": 20000, "do": "client", "instance": "b"},
			{"at": 40000, "do": "restart", "instance": "a"}, {"at": 50000, "do": "crash", "instance": "b"},
			{"at": 52000, "do": "client", "instance": "a"}, {"at": 60000, "do": "restart", "instance": "b"},
			{"at": 75000, "do": "close", "instance": "a"}, {"at": 76000, "do": "client", "instance": "b"},
			{"at": 90000, "do": "open", "instance": "a"}, {"at": 95000, "do": "crash", "instance": "b"},
			{"at": 96000, "do": "client", "instance": "a"}, {"at": 110000, "do": "restart", "instance": "b"}`
		var lost, unknown int
		for seed := 1; seed <= 24; seed++ {
			res := run(t, &e, withClient(fmt.Sprintf(`, "seed": %v, "end": 150000, "faults": %v, "workload": {"callers": 0, "think": 400, "writes": 60}`, seed, allFaults), schedule))
			eachCall(t, res)
			noViolation(t, res, fmt.Sprint("seed ", seed))
			lost += res.Summary.Calls["write lost"]
			unknown += res.Summary.Calls["write unknown"]
		}
		t.Logf("writes lost at a crash: %v, and of unknown outcome: %v", lost, unknown)
		is.True(t, lost > 0, "writes lost at a crash")
	})
}
