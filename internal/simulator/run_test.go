//go:build s4ql_browser

package simulator_test

import (
	"encoding/json/v2"
	"fmt"
	"slices"
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

	// The actions below are on a, which holds the lease from the start in these runs of two instances,
	// from 30 s to 90 s. b takes the lease over in between.
	switched := func(do string) string {
		return fmt.Sprintf(`{"seed": 1, "end": 150000, "instances": 2, "schedule": [{"at": 30000, "do": %q, "instance": "a", "on": true},
			{"at": 90000, "do": %q, "instance": "a"}]}`, do, do)
	}
	sent := func(res simulator.Response, process string, from, to float64) int {
		return len(find(res, func(e simulator.Event) bool {
			return e.Kind == "request" && e.What == "sent" && e.Process == process && e.T >= from && e.T < to
		}))
	}
	taken := func(res simulator.Response, process string, from, to float64) bool {
		return len(find(res, func(e simulator.Event) bool {
			return e.Kind == "core" && e.Event == "lease" && e.Attrs["outcome"] == "taken" && e.Process == process && e.T >= from && e.T < to
		})) > 0
	}

	t.Run("should hang the network of an instance, so that no request of it is sent and no answer reaches it", func(t *testing.T) {
		res := run(t, &e, switched("hangNetwork"))
		is.True(t, taken(res, "a", 0, 30_000), "a holds the lease")
		is.True(t, sent(res, "a", 0, 30_000) > 0, "requests before the hang")
		is.Equal(t, 0, sent(res, "a", 30_000, 90_000))
		hung := find(res, func(e simulator.Event) bool { return e.Kind == "request" && e.What == "hung" && e.Process == "a" })
		is.True(t, len(hung) > 0, "requests that hang")
		// A caller that waits gives up at its own timeout, and the core counts a renewal without an
		// answer as one of unknown outcome.
		unknown := find(res, func(e simulator.Event) bool {
			return e.Kind == "core" && e.Event == "lease" && e.Attrs["outcome"] == "unknown" && e.Process == "a" && e.T < 90_000
		})
		is.True(t, len(unknown) > 0, "renewals without an answer")
		is.True(t, strings.Contains(fmt.Sprint(unknown[0].Attrs["error"]), "no answer came while the network of the instance hung"), fmt.Sprint(unknown[0].Attrs["error"]))
		is.True(t, taken(res, "b", 30_000, 90_000), "b takes the lease over")
		is.True(t, sent(res, "a", 90_000, 150_000) > 0, "requests after the hang")
		s := simulator.StateAt(res, 60_000)
		is.True(t, s.Instances[0].NetworkHung, "the state shows the hang")
		is.True(t, !simulator.StateAt(res, 90_000).Instances[0].NetworkHung, "the state shows the end of the hang")
	})

	t.Run("should disconnect the network of an instance, so that each request of it fails at once and is not sent", func(t *testing.T) {
		res := run(t, &e, switched("disconnectNetwork"))
		is.Equal(t, 0, sent(res, "a", 30_000, 90_000))
		disconnected := find(res, func(e simulator.Event) bool {
			return e.Kind == "request" && e.What == "disconnected" && e.Process == "a"
		})
		is.True(t, len(disconnected) > 0, "requests that fail")
		unknown := find(res, func(e simulator.Event) bool {
			return e.Kind == "core" && e.Event == "lease" && e.Attrs["outcome"] == "unknown" && e.Process == "a" && e.T < 90_000
		})
		is.True(t, len(unknown) > 0, "renewals that fail")
		is.True(t, strings.Contains(fmt.Sprint(unknown[0].Attrs["error"]), "the network of the instance is disconnected"), fmt.Sprint(unknown[0].Attrs["error"]))
		is.True(t, taken(res, "b", 30_000, 90_000), "b takes the lease over")
		is.True(t, sent(res, "a", 90_000, 150_000) > 0, "requests after the disconnect")
		is.True(t, simulator.StateAt(res, 60_000).Instances[0].NetworkDisconnected, "the state shows the disconnect")
	})

	t.Run("should end the requests of an instance that are on their way when its network is disconnected", func(t *testing.T) {
		// The run without the disconnect has a request on its way at the time of the disconnect, and
		// the run with it is the same up to that time.
		const request = `{"seed": 1, "end": 60000, "instances": 1, "latency": 5000, "schedule": [%v]}`
		without := run(t, &e, fmt.Sprintf(request, ""))
		sentAt := find(without, func(e simulator.Event) bool { return e.Kind == "request" && e.What == "sent" && e.T > 20_000 })[0].T
		at := sentAt + 1
		res := run(t, &e, fmt.Sprintf(request, fmt.Sprintf(`{"at": %v, "do": "disconnectNetwork", "instance": "a", "on": true}`, at)))
		cut := find(res, func(e simulator.Event) bool {
			return e.Kind == "request" && e.What == "disconnected" && e.Request > 0
		})
		is.True(t, len(cut) > 0, "a request on its way gets an error at the disconnect")
		is.Equal(t, at, cut[0].T)
		is.True(t, !slices.ContainsFunc(simulator.StateAt(res, at).Requests,
			func(r simulator.RequestState) bool { return r.Request == cut[0].Request && r.Waiting }), "the caller does not wait for it")
		is.True(t, slices.ContainsFunc(simulator.StateAt(res, at-0.5).Requests,
			func(r simulator.RequestState) bool { return r.Request == cut[0].Request && r.Waiting }), "the caller waits for it before")
	})

	t.Run("should end the waits of an instance whose network hangs when its network is disconnected", func(t *testing.T) {
		res := run(t, &e, `{"seed": 1, "end": 60000, "instances": 2, "schedule": [{"at": 30000, "do": "hangNetwork", "instance": "a", "on": true},
			{"at": 40000, "do": "disconnectNetwork", "instance": "a", "on": true}]}`)
		cut := find(res, func(e simulator.Event) bool { return e.Kind == "request" && e.What == "disconnected" && e.T == 40_000 })
		is.True(t, len(cut) > 0, "a call that waits gets an error at the disconnect")
		is.Equal(t, 0, len(find(res, func(e simulator.Event) bool {
			return e.Kind == "request" && e.What == "returned" && e.Process == "a" && e.T > 40_000
		})))
	})

	t.Run("should fail the disk of an instance, after which its core stops, and its process opens it again once the disk works", func(t *testing.T) {
		res := run(t, &e, switched("failDisk"))
		stop := find(res, func(e simulator.Event) bool {
			return e.Kind == "core" && e.Event == "stop" && e.Attrs["outcome"] == "local_failure" && e.Process == "a"
		})
		is.Equal(t, 1, len(stop))
		is.True(t, stop[0].T > 30_000 && stop[0].T < 90_000, "a stop while the disk fails")
		failed := find(res, func(e simulator.Event) bool {
			return e.Kind == "process" && e.What == "open_failed" && e.Process == "a"
		})
		is.True(t, len(failed) > 1, "opens that fail, again and again")
		is.True(t, failed[len(failed)-1].T < 90_000, "no open fails after the disk works")
		opened := find(res, func(e simulator.Event) bool {
			return e.Kind == "process" && e.What == "opened" && e.Process == "a" && e.T >= 90_000
		})
		is.Equal(t, 1, len(opened))
		is.True(t, taken(res, "b", 30_000, 90_000), "b takes the lease over")
		is.True(t, simulator.StateAt(res, 60_000).Instances[0].DiskFailing, "the state shows the failing disk")
		is.Equal(t, "failed", simulator.StateAt(res, 60_000).Instances[0].State)
	})

	t.Run("should pause both clocks of an instance, which stops its tasks, and resume them", func(t *testing.T) {
		res := run(t, &e, `{"seed": 1, "end": 150000, "instances": 2, "schedule": [{"at": 30000, "do": "pauseClocks", "instance": "a"},
			{"at": 90000, "do": "resumeClocks", "instance": "a"}]}`)
		clocks := find(res, func(e simulator.Event) bool { return e.Kind == "clock" })
		is.Equal(t, 2, len(clocks))
		is.True(t, clocks[0].WallStopped && clocks[0].MonotonicStopped, "both clocks stand still")
		is.True(t, !clocks[1].WallStopped && !clocks[1].MonotonicStopped, "both clocks run")
		is.Equal(t, 30_000.0, deref(clocks[1].Wall))
		is.Equal(t, 0, sent(res, "a", 30_000, 90_000))
		started := find(res, func(e simulator.Event) bool {
			return e.Kind == "call" && e.What == "started" && e.Process == "a" && e.T > 30_000 && e.T < 90_000
		})
		is.Equal(t, 0, len(started))
		is.True(t, taken(res, "b", 30_000, 90_000), "b takes the lease over")
		is.Equal(t, 30_000.0, simulator.StateAt(res, 60_000).Instances[0].Wall)
		// The holder that wakes up believes in its lease until it reads the manifest, which the
		// design accepts for a process whose two clocks both stood still.
		is.Equal(t, res.Summary.Violations, res.Summary.Excused)
	})

	t.Run("should show a close as the release of the lease, which another instance takes at once", func(t *testing.T) {
		res := run(t, &e, `{"seed": 1, "end": 60000, "instances": 2, "schedule": [{"at": 30000, "do": "close", "instance": "a"}]}`)
		released := find(res, func(e simulator.Event) bool {
			return e.Kind == "core" && e.Event == "lease" && e.Attrs["lease.transition"] == "released" && e.Attrs["outcome"] == "released"
		})
		is.Equal(t, 1, len(released))
		is.Equal(t, "a", released[0].Process)
		// Without the release, b would wait for the lease to run out, 30 s.
		is.True(t, taken(res, "b", 30_000, 35_000), "b takes the lease within 5 s")
	})

	t.Run("should not switch a fault of a process that has crashed, but of the instance", func(t *testing.T) {
		res := run(t, &e, `{"end": 30000, "instances": 1, "schedule": [{"at": 1000, "do": "crash", "instance": "a"},
			{"at": 2000, "do": "failDisk", "instance": "a", "on": true}, {"at": 3000, "do": "pauseClocks", "instance": "a"}]}`)
		actions := find(res, func(e simulator.Event) bool { return e.Kind == "action" })
		is.Equal(t, "done", actions[1].What)
		is.True(t, deref(actions[1].On), "on")
		is.Equal(t, "ignored", actions[2].What)
	})
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
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
