//go:build s4ql_browser

package simulator_test

import (
	"cmp"
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
		is.Equal(t, 13, len(fmt.Sprint(calls[0].Value)))
		is.Equal(t, "served", calls[1].What)
		is.EqualSlice(t, []int64{calls[0].Value}, calls[1].Keys)
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

	t.Run("should only insert while no insert has been acknowledged", func(t *testing.T) {
		res := run(t, &e, withClient(`, "end": 60000, "workload": {"callers": 0, "think": 500, "writes": 10}`,
			`{"at": 0, "do": "disconnectNetwork", "instance": "a", "on": true}, {"at": 0, "do": "read", "instance": "a"}`))
		calls := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What == "started" })
		is.True(t, len(calls) > 20, "the client calls")
		for _, c := range calls {
			is.Equal(t, "write", c.Op, fmt.Sprint("call ", c.Call))
		}
		is.Equal(t, 0, len(find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What == "committed" })))
		is.Equal(t, 0, res.Summary.Violations)
	})

	t.Run("should write barcodes with an insert, delete one with a delete, read one with a select, and annotate each commit object with its statements", func(t *testing.T) {
		res := run(t, &e, `{"seed": 2, "end": 120000, "instances": 1, "workload": {"callers": 6, "think": 500, "writes": 70}}`)
		// arg of a statement as text: JSON gives a number as a float64, which holds 13 digits exactly.
		arg := func(v any) string {
			if f, ok := v.(float64); ok {
				return fmt.Sprint(int64(f))
			}
			return fmt.Sprint(v)
		}
		commits := find(res, func(e simulator.Event) bool { return e.What == "applied" && e.Object != nil && e.Object.Commit != nil })
		is.True(t, len(commits) > 50, "many commit objects")
		first := commits[0].Object.Commit
		is.EqualSlice(t, []string{"barcodes"}, first.Tables)
		is.Equal(t, 2, len(first.Statements))
		is.Equal(t, "create table barcodes (code integer primary key) strict, without rowid", first.Statements[0].SQL)
		is.Equal(t, 0, len(first.Statements[0].Args))
		// Each commit object inserts one row, or deletes one that a commit object before it inserted.
		inserted, deleted := map[int64]bool{}, map[int64]bool{}
		for _, ev := range commits {
			c := ev.Object.Commit
			is.Equal(t, 1, len(c.Keys)+len(c.Deletes))
			is.True(t, len(c.Statements) > 0, "a commit object holds its statements")
			last := c.Statements[len(c.Statements)-1]
			is.Equal(t, 1, len(last.Args))
			if len(c.Keys) == 1 {
				inserted[c.Keys[0]] = true
				is.Equal(t, "insert into barcodes (code) values (?)", last.SQL)
				is.Equal(t, fmt.Sprint(c.Keys[0]), arg(last.Args[0]))
				continue
			}
			k := c.Deletes[0]
			is.True(t, inserted[k] && !deleted[k], fmt.Sprint("a delete of a row that is there: ", k))
			deleted[k] = true
			is.Equal(t, 1, len(c.Statements))
			is.Equal(t, "delete from barcodes where code = ?", last.SQL)
			is.Equal(t, fmt.Sprint(k), arg(last.Args[0]))
		}
		is.True(t, len(deleted) > 5 && len(deleted) < len(inserted)/2, fmt.Sprint(len(deleted), " deletes of ", len(inserted), " rows"))
		for _, c := range find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.Op == "delete" }) {
			is.Equal(t, "delete from barcodes where code = ?", c.SQL)
			is.Equal(t, int64(0), c.Value)
			if c.What == "committed" {
				is.True(t, deleted[int64(c.Args[0].(float64))], fmt.Sprint("the commit of the delete of ", arg(c.Args[0]), " deletes it"))
			}
		}
		// Every key is a barcode of EAN-13 with the prefix 57, and the keys of the writes, in the
		// order taken, are distinct and do not rise.
		var keys []int64
		for _, c := range find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.Op == "write" && e.What == "started" }) {
			keys = append(keys, c.Value)
		}
		is.True(t, len(keys) > 50, "many writes")
		seen, rises := map[int64]bool{}, 0
		for i, k := range keys {
			s := fmt.Sprint(k)
			is.True(t, len(s) == 13 && s[:2] == "57", fmt.Sprint("a barcode of 13 digits with the prefix 57: ", s))
			sum := 0
			for j, d := range s[:12] {
				sum += int(d-'0') * (1 + 2*(j%2))
			}
			is.Equal(t, (10-sum%10)%10, int(s[12]-'0'), "the check digit of "+s)
			is.True(t, !seen[k], "a key once: "+s)
			seen[k] = true
			if i > 0 && k > keys[i-1] {
				rises++
			}
		}
		is.True(t, rises > len(keys)/4 && rises < len(keys)*3/4, fmt.Sprint(rises, " rises in ", len(keys), " keys"))
		for _, c := range find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.Op == "write" }) {
			is.Equal(t, "insert into barcodes (code) values (?)", c.SQL)
			is.Equal(t, fmt.Sprint(c.Value), arg(c.Args[0]))
			if c.What == "committed" {
				is.True(t, inserted[c.Value], fmt.Sprint("the commit of ", c.Value, " inserts it"))
			}
		}
		// A read selects one key that the application knows is in the store, and expects its row, or,
		// one in eight, the key of a recent acknowledged delete, and expects no row. A read that is not
		// stale gives what it expects.
		served := find(res, func(e simulator.Event) bool { return e.Kind == "call" && e.What == "served" })
		is.True(t, len(served) > 20, "many reads")
		found, gone := 0, 0
		for _, r := range served {
			is.Equal(t, "select code from barcodes where code = ?", r.SQL)
			key := arg(r.Args[0])
			switch r.Expect {
			case "row":
				// A delete of the key can start while the read runs, and commit before the read
				// fixes its state.
				gives := len(r.Keys) == 1 && fmt.Sprint(r.Keys[0]) == key
				is.True(t, gives || len(r.Keys) == 0 && deleted[int64(r.Args[0].(float64))], fmt.Sprint("a read of ", key, " gives ", r.Keys))
				if gives {
					found++
				}
			case "none":
				is.True(t, len(r.Keys) == 0 && deleted[int64(r.Args[0].(float64))], fmt.Sprint("a read of the deleted ", key, " gives ", r.Keys))
				gone++
			default:
				t.Fatal("a read expects", r.Expect)
			}
		}
		is.True(t, gone > 0 && gone < found/3, fmt.Sprint(found, " reads found their row, and ", gone, " found that it was gone"))
		is.Equal(t, 0, res.Summary.Violations)
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
		// seed of the run, 5 if it is 0. A fault that needs a write on its way at a crash shows
		// only with some seeds.
		seed int
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
		{name: "writes held back after a crash", faults: `{"slow": 0.5, "slowBy": 20000, "afterCrash": 1, "lateBy": 20000}`, seed: 2,
			schedule: `{"at": 20000, "do": "crash", "instance": "a"}, {"at": 20000, "do": "crash", "instance": "b"}, {"at": 20000, "do": "crash", "instance": "c"}`,
			kind:     "request", what: "held", text: "the sender crashed"},
		{name: "damaged downloads", faults: `{"damaged": 1}`, schedule: `{"at": 20000, "do": "crash", "instance": "a"}, {"at": 20000, "do": "crash", "instance": "b"}`,
			kind: "request", what: "damaged"},
		{name: "points of buggify", faults: `{}`, kind: "buggify", what: "fired"},
	}
	for _, test := range tests {
		t.Run("should show "+test.name, func(t *testing.T) {
			seed := cmp.Or(test.seed, 5)
			request := fmt.Sprintf(`{"seed": %v, "end": 120000, "buggify": %v, "faults": %v, "schedule": [%v]}`, seed, test.kind == "buggify", test.faults, test.schedule)
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
