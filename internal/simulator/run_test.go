//go:build s4ql_browser

package simulator_test

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/simulator"
)

// find the events of res for which match is true.
func find(res simulator.Response, match func(e simulator.Event) bool) []simulator.Event {
	var out []simulator.Event
	for _, e := range res.Events {
		if match(e) {
			out = append(out, e)
		}
	}
	return out
}

// sentIn reports whether the request of e, an event of a request, was sent from from to before to.
func sentIn(res simulator.Response, e simulator.Event, from, to float64) bool {
	for _, s := range res.Events {
		if s.Kind == "request" && s.What == "sent" && s.Request == e.Request {
			return s.T >= from && s.T < to
		}
	}
	return false
}

func TestRun_actions(t *testing.T) {
	var e simulator.Engine

	t.Run("should crash a process, after which it makes no call", func(t *testing.T) {
		res := run(t, &e, `{"end": 60000, "schedule": [{"at": 20000, "do": "crash", "instance": "a"}]}`)
		crashed := find(res, func(e simulator.Event) bool { return e.Kind == "process" && e.What == "crashed" })
		is.Equal(t, 1, len(crashed))
		is.Equal(t, 20_000.0, crashed[0].T)
		is.Equal(t, "a", crashed[0].Process)
		before := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.Process == "a" && e.T < 20_000 })
		after := find(res, func(e simulator.Event) bool {
			return e.Kind == "call" && e.What == "started" && e.Process == "a" && e.T > 20_000
		})
		is.True(t, len(before) > 0, "calls before the crash")
		is.Equal(t, 0, len(after))
	})

	t.Run("should restart a crashed instance in a new process on a copy of its cache", func(t *testing.T) {
		res := run(t, &e, `{"end": 90000, "instances": 1, "schedule": [{"at": 40000, "do": "crash", "instance": "a"}, {"at": 45000, "do": "restart", "instance": "a"}]}`)
		started := find(res, func(e simulator.Event) bool { return e.Kind == "process" && e.What == "opened" && e.Process == "a2" })
		is.Equal(t, 1, len(started))
		is.Equal(t, 45_000.0, started[0].T)
		at := simulator.StateAt(res, 45_000)
		is.Equal(t, "a2", at.Instances[0].Process)
		before := simulator.StateAt(res, 39_999)
		is.True(t, before.Instances[0].Position != strings.Repeat("0", 64), "a cache with commits before the crash")
		is.Equal(t, before.Instances[0].Position, at.Instances[0].Position)
		served := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What == "served" && e.Process == "a2" })
		is.True(t, len(served) > 0, "reads served by the new process")
	})

	t.Run("should ignore a restart of an instance that has not crashed, and an action on a crashed one", func(t *testing.T) {
		res := run(t, &e, `{"end": 30000, "schedule": [{"at": 10000, "do": "restart", "instance": "a"},
			{"at": 11000, "do": "crash", "instance": "b"}, {"at": 12000, "do": "write", "instance": "b"}]}`)
		ignored := find(res, func(e simulator.Event) bool { return e.Kind == "action" && e.What == "ignored" })
		is.Equal(t, 2, len(ignored))
		is.Equal(t, "restart", ignored[0].Do)
		is.Equal(t, "write", ignored[1].Do)
	})

	t.Run("should close a database, which releases the lease, and open it again", func(t *testing.T) {
		res := run(t, &e, `{"end": 90000, "instances": 1, "schedule": [{"at": 20000, "do": "close", "instance": "a"}, {"at": 50000, "do": "open", "instance": "a"}]}`)
		closed := find(res, func(e simulator.Event) bool { return e.Kind == "process" && e.What == "closed" })
		is.Equal(t, 1, len(closed))
		s := simulator.StateAt(res, 40_000)
		is.Equal(t, "closed", s.Instances[0].State)
		is.True(t, s.Bucket.Manifest.Lease == nil, "no lease in the manifest")
		stoppedCalls := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What == "stopped" })
		is.True(t, len(stoppedCalls) > 0, "calls refused while closed")
		taken := find(res, func(e simulator.Event) bool {
			return e.Kind == "core" && e.Event == "lease" && e.Attrs["outcome"] == "taken" && e.T > 50_000
		})
		is.Equal(t, 1, len(taken))
		is.Equal(t, "running", simulator.StateAt(res, 89_000).Instances[0].State)
	})

	t.Run("should cut a process off from the store, so that what it sends does not arrive", func(t *testing.T) {
		res := run(t, &e, `{"end": 60000, "instances": 1, "schedule": [{"at": 20000, "do": "cutOff", "instance": "a", "for": 20000}]}`)
		cut := find(res, func(e simulator.Event) bool { return e.Kind == "store" && e.What == "cut_off" })
		is.Equal(t, 1, len(cut))
		is.Equal(t, 20_000.0, cut[0].For)
		is.Equal(t, "a", cut[0].Instance)
		lost := find(res, func(e simulator.Event) bool { return e.What == "lost" && sentIn(res, e, 20_000, 40_000) })
		applied := find(res, func(e simulator.Event) bool { return e.What == "applied" && sentIn(res, e, 20_000, 40_000) })
		is.True(t, len(lost) > 0, "requests lost")
		is.Equal(t, 0, len(applied))
		is.Equal(t, 40_000.0, simulator.StateAt(res, 30_000).Instances[0].CutOffUntil)
	})

	t.Run("should make the store unreachable for every process in an outage", func(t *testing.T) {
		res := run(t, &e, `{"end": 60000, "schedule": [{"at": 20000, "do": "outage", "for": 15000}]}`)
		is.Equal(t, 1, len(find(res, func(e simulator.Event) bool { return e.Kind == "store" && e.What == "outage" })))
		// A request sent just before the end of the outage arrives after it.
		lost := find(res, func(e simulator.Event) bool { return e.What == "lost" && sentIn(res, e, 20_000, 34_900) })
		applied := find(res, func(e simulator.Event) bool { return e.What == "applied" && sentIn(res, e, 20_000, 34_900) })
		is.True(t, len(lost) > 1, "requests lost")
		is.Equal(t, 0, len(applied))
	})

	t.Run("should calm the store, after which no request it sends is lost", func(t *testing.T) {
		res := run(t, &e, `{"end": 60000, "faults": {"lostRequest": 0.5}, "schedule": [{"at": 20000, "do": "calm"}]}`)
		is.True(t, len(find(res, func(e simulator.Event) bool { return e.What == "lost" && sentIn(res, e, 0, 20_000) })) > 0, "requests lost before")
		is.Equal(t, 0, len(find(res, func(e simulator.Event) bool { return e.What == "lost" && sentIn(res, e, 20_000, 60_000) })))
		is.True(t, simulator.StateAt(res, 30_000).Calm, "a calm store")
	})

	t.Run("should end damaged downloads when the store is calm", func(t *testing.T) {
		res := run(t, &e, `{"end": 120000, "faults": {"damaged": 1}, "schedule": [{"at": 20000, "do": "crash", "instance": "a"},
			{"at": 20000, "do": "crash", "instance": "b"}, {"at": 60000, "do": "calm"}]}`)
		is.True(t, len(find(res, func(e simulator.Event) bool { return e.What == "damaged" && e.T < 60_000 })) > 0, "damaged before")
		is.Equal(t, 0, len(find(res, func(e simulator.Event) bool { return e.What == "damaged" && e.T > 60_000 })))
	})

	t.Run("should ignore an open of a database that is open", func(t *testing.T) {
		res := run(t, &e, `{"end": 30000, "instances": 1, "schedule": [{"at": 10000, "do": "open", "instance": "a"}]}`)
		ignored := find(res, func(e simulator.Event) bool { return e.Kind == "action" && e.What == "ignored" })
		is.Equal(t, 1, len(ignored))
	})

	t.Run("should shift, stop, and start a wall clock", func(t *testing.T) {
		res := run(t, &e, `{"end": 90000, "schedule": [{"at": 20000, "do": "shiftWall", "instance": "a", "by": -60000},
			{"at": 30000, "do": "stopWall", "instance": "a"}, {"at": 50000, "do": "startWall", "instance": "a"}]}`)
		clocks := find(res, func(e simulator.Event) bool { return e.Kind == "clock" })
		is.Equal(t, 3, len(clocks))
		is.Equal(t, -40_000.0, *clocks[0].Wall)
		is.True(t, clocks[1].WallStopped, "stopped")
		is.Equal(t, -30_000.0, simulator.StateAt(res, 30_000).Instances[0].Wall)
		is.Equal(t, -30_000.0, simulator.StateAt(res, 45_000).Instances[0].Wall)
		is.Equal(t, -20_000.0, simulator.StateAt(res, 60_000).Instances[0].Wall)
		is.Equal(t, 60_000.0, simulator.StateAt(res, 60_000).Instances[1].Wall)
	})

	t.Run("should stop a monotonic clock, which holds the renewals of the lease", func(t *testing.T) {
		res := run(t, &e, `{"end": 90000, "instances": 1, "workload": {"on": false}, "schedule": [{"at": 30000, "do": "stopMonotonic", "instance": "a"}, {"at": 70000, "do": "startMonotonic", "instance": "a"}]}`)
		s := simulator.StateAt(res, 60_000)
		is.True(t, s.Instances[0].MonotonicStopped, "stopped")
		started := res.Events[0].T
		is.Equal(t, 30_000-started, s.Instances[0].Monotonic)
		renewals := func(from, to float64) int {
			return len(find(res, func(e simulator.Event) bool {
				return e.What == "sent" && e.Purpose == "renew" && e.T >= from && e.T < to
			}))
		}
		is.True(t, renewals(0, 30_000) > 0, "renewals before")
		is.Equal(t, 0, renewals(31_000, 70_000))
		is.True(t, renewals(70_000, 90_000) > 0, "renewals after")
	})

	t.Run("should lose the answer of the next request of a kind", func(t *testing.T) {
		res := run(t, &e, `{"end": 60000, "instances": 1, "schedule": [{"at": 10000, "do": "loseAnswer", "request": "commit"}]}`)
		lost := find(res, func(e simulator.Event) bool { return e.What == "answer_lost" })
		is.Equal(t, 1, len(lost))
		is.Equal(t, "commit", lost[0].Purpose)
		is.True(t, lost[0].T > 10_000, "after the action")
		applied := find(res, func(e simulator.Event) bool { return e.What == "applied" && e.Request == lost[0].Request })
		is.Equal(t, 1, len(applied))
	})

	t.Run("should make the next request of a kind of one instance fail before it is sent", func(t *testing.T) {
		res := run(t, &e, `{"end": 60000, "schedule": [{"at": 10000, "do": "failRequest", "request": "upload", "instance": "b"}]}`)
		failed := find(res, func(e simulator.Event) bool { return e.Kind == "request" && e.What == "failed" })
		is.Equal(t, 1, len(failed))
		is.Equal(t, "upload", failed[0].Purpose)
		is.Equal(t, "b", failed[0].Process)
		is.Equal(t, 0, failed[0].Request)
	})

	t.Run("should make a write and a read of the visitor, without the workload", func(t *testing.T) {
		res := run(t, &e, `{"end": 30000, "instances": 1, "workload": {"on": false}, "schedule": [{"at": 10000, "do": "write", "instance": "a"}, {"at": 20000, "do": "read", "instance": "a"}]}`)
		calls := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What != "started" })
		is.Equal(t, 2, len(calls))
		is.Equal(t, "visitor", calls[0].Caller)
		is.Equal(t, "committed", calls[0].What)
		is.Equal(t, int64(1), calls[0].Value)
		is.Equal(t, "served", calls[1].What)
		is.EqualSlice(t, []int64{1}, calls[1].Keys)
	})

	t.Run("should switch the workload off", func(t *testing.T) {
		res := run(t, &e, `{"end": 60000, "schedule": [{"at": 30000, "do": "workload", "on": false}]}`)
		is.True(t, len(find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What == "started" && e.T < 30_000 })) > 0, "calls before")
		is.Equal(t, 0, len(find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What == "started" && e.T > 30_000 })))
	})
}

func TestRun_faults(t *testing.T) {
	var e simulator.Engine
	tests := []struct {
		name, faults, schedule string
		kind, what, text       string
	}{
		{name: "lost requests", faults: `{"lostRequest": 0.2}`, kind: "request", what: "lost", text: "was lost on its way to the store"},
		{name: "lost responses", faults: `{"lostResponse": 0.2}`, kind: "request", what: "response_lost", text: "was lost on its way back"},
		{name: "callers that get no answer", faults: `{"lostRequest": 0.2, "hang": 1}`, kind: "request", what: "no_answer"},
		{name: "slow requests", faults: `{"slow": 0.2, "slowBy": 3000}`, kind: "request", what: "slow"},
		{name: "broken reads", faults: `{"brokenRead": 0.3}`, kind: "request", what: "read_broken"},
		{name: "random cut-offs", faults: `{"cutOff": 0.05, "cutOffFor": 5000}`, kind: "store", what: "cut_off"},
		{name: "random outages", faults: `{"outage": 0.05, "outageFor": 5000}`, kind: "store", what: "outage"},
		{name: "conflicts", faults: `{"conflict": 1, "lostRequest": 0.1, "late": 1, "lateBy": 20000}`, kind: "request", what: "refused", text: "conflict"},
		{name: "writes stuck in the network", faults: `{"lostRequest": 0.3, "late": 1, "lateBy": 20000}`, kind: "request", what: "stuck"},
		{name: "stuck writes that arrive", faults: `{"lostRequest": 0.3, "late": 1, "lateBy": 20000}`, kind: "request", what: "arrived"},
		{name: "writes held back after the caller gave up", faults: `{"slow": 0.3, "slowBy": 20000, "late": 1, "lateBy": 20000}`, kind: "request", what: "held", text: "the caller gave up"},
		{name: "writes held back after a crash", faults: `{"slow": 0.5, "slowBy": 20000, "afterCrash": 1, "lateBy": 20000}`,
			schedule: `{"at": 20000, "do": "crash", "instance": "a"}, {"at": 20000, "do": "crash", "instance": "b"}, {"at": 20000, "do": "crash", "instance": "c"}`,
			kind:     "request", what: "held", text: "the sender crashed"},
		{name: "damaged downloads", faults: `{"damaged": 1}`, schedule: `{"at": 20000, "do": "crash", "instance": "a"}, {"at": 20000, "do": "crash", "instance": "b"}`,
			kind: "request", what: "damaged"},
		{name: "points of buggify", faults: `{}`, kind: "buggify", what: "fired"},
	}
	for _, test := range tests {
		t.Run("should show "+test.name, func(t *testing.T) {
			request := fmt.Sprintf(`{"seed": 5, "end": 120000, "buggify": %v, "faults": %v, "schedule": [%v]}`, test.kind == "buggify", test.faults, test.schedule)
			res := run(t, &e, request)
			found := find(res, func(e simulator.Event) bool {
				return e.Kind == test.kind && e.What == test.what && strings.Contains(e.Text+e.Error, test.text)
			})
			is.True(t, len(found) > 0, "an event "+test.kind+"/"+test.what)
			is.Equal(t, 0, res.Summary.Violations)
		})
	}

	t.Run("should classify every line of the store under every fault", func(t *testing.T) {
		res := run(t, &e, `{"seed": 6, "end": 300000, "faults": `+allFaults+`, "schedule": [{"at": 30000, "do": "crash", "instance": "a"}]}`)
		unknown := find(res, func(e simulator.Event) bool { return (e.Kind == "request" || e.Kind == "store") && e.What == "" })
		b, _ := json.Marshal(unknown)
		is.Equal(t, 0, len(unknown), string(b))
		noRequest := find(res, func(e simulator.Event) bool {
			return e.Kind == "request" && e.Request == 0 && e.What != "failed" && e.What != "too_large" && e.What != "returned"
		})
		is.Equal(t, 0, len(noRequest))
	})
}
