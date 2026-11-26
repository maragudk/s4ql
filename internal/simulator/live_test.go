//go:build s4ql_browser

package simulator_test

import (
	"encoding/json/v2"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/simulator"
)

// live session on an engine of its own, which the test ends when it is done. events holds every
// event that the answers brought, in order.
type live struct {
	t      *testing.T
	e      *simulator.Engine
	events []simulator.Event
	last   simulator.Live
}

// startLive session with the settings, given as JSON.
func startLive(t *testing.T, settings string) *live {
	t.Helper()
	l := &live{t: t, e: &simulator.Engine{}}
	t.Cleanup(l.e.Close)
	l.do("start", settings)
	return l
}

// do the command of the kind with the body, which must not be refused, and keep its events.
func (l *live) do(kind, body string) simulator.Live {
	l.t.Helper()
	a := l.try(kind, body)
	is.Equal(l.t, "", a.Error)
	l.events = append(l.events, a.Events...)
	l.last = a
	return a
}

// try the command of the kind with the body, and decode the answer.
func (l *live) try(kind, body string) simulator.Live {
	l.t.Helper()
	var a simulator.Live
	is.NotError(l.t, json.Unmarshal(l.e.Handle(l.t, kind, []byte(body)), &a))
	return a
}

// advance the session by ms milliseconds of fake time.
func (l *live) advance(ms float64) simulator.Live {
	l.t.Helper()
	return l.do("advance", fmt.Sprintf(`{"by": %v}`, ms))
}

// act the action, given as JSON, at the time of the session.
func (l *live) act(action string) simulator.Live {
	l.t.Helper()
	return l.do("act", action)
}

// end the session, and return its last answer.
func (l *live) end() simulator.Live {
	l.t.Helper()
	return l.do("end", "{}")
}

// advanceTo the time t of the session.
func (l *live) advanceTo(t float64) simulator.Live {
	l.t.Helper()
	return l.do("advance", fmt.Sprintf(`{"to": %v}`, t))
}

// eventLines of events, each as its JSON.
func eventLines(t *testing.T, events []simulator.Event) []string {
	t.Helper()
	return lines(t, simulator.Response{Events: events})
}

// replay the schedule of the batch request in a live session with the same settings: advance to the
// time of each action and do it, then advance to the end, and end. It returns the session, and the
// state that "now" gave after each action, with the number of events that had come before it.
func replay(t *testing.T, settings string, schedule []simulator.Action, end float64) (*live, []nowState) {
	t.Helper()
	l := startLive(t, settings)
	var states []nowState
	for _, a := range schedule {
		l.advanceTo(a.At)
		a.At = 0
		b, err := json.Marshal(a)
		is.NotError(t, err)
		l.do("act", string(b))
		states = append(states, nowState{state: *l.do("now", "{}").State, events: len(l.events)})
	}
	l.advanceTo(end)
	l.do("end", "{}")
	return l, states
}

// nowState is a state that "now" gave, and the number of events of the session before it.
type nowState struct {
	state  simulator.State
	events int
}

func TestEngine_live(t *testing.T) {
	t.Run("should give the same events, steps, summary, and hash as a batch run with the same actions at the same times", func(t *testing.T) {
		// Every kind of action, two at the same time, and one at a fraction of a millisecond.
		schedule := `[
			{"at": 5000, "do": "write", "instance": "a"}, {"at": 5000, "do": "read", "instance": "b"},
			{"at": 12000, "do": "hangNetwork", "instance": "a", "on": true}, {"at": 31000.5, "do": "hangNetwork", "instance": "a"},
			{"at": 40000, "do": "disconnectNetwork", "instance": "b", "on": true}, {"at": 47000, "do": "disconnectNetwork", "instance": "b"},
			{"at": 50000, "do": "failDisk", "instance": "c", "on": true}, {"at": 58000, "do": "failDisk", "instance": "c"},
			{"at": 60000, "do": "crash", "instance": "a"}, {"at": 63000, "do": "restart", "instance": "a"},
			{"at": 66000, "do": "close", "instance": "b"}, {"at": 70000, "do": "open", "instance": "b"},
			{"at": 72000, "do": "pauseClocks", "instance": "c"}, {"at": 90000, "do": "resumeClocks", "instance": "c"},
			{"at": 92000, "do": "shiftWall", "instance": "a", "by": 45000}, {"at": 94000, "do": "stopWall", "instance": "b"},
			{"at": 95000, "do": "startWall", "instance": "b"}, {"at": 96000, "do": "stopMonotonic", "instance": "b"},
			{"at": 99000, "do": "startMonotonic", "instance": "b"}, {"at": 100000, "do": "cutOff", "instance": "c", "for": 8000},
			{"at": 101000, "do": "outage", "for": 3000}, {"at": 104000, "do": "loseAnswer", "request": "commit"},
			{"at": 104000, "do": "failRequest", "request": "any", "instance": "b"}, {"at": 106000, "do": "workload"},
			{"at": 110000, "do": "workload", "on": true}, {"at": 115000, "do": "calm"}]`
		for _, seed := range []int{1, 2, 3} {
			settings := fmt.Sprintf(`"seed": %v, "instances": 3, "buggify": true, "faults": %v, "workload": {}`, seed, allFaults)
			res := run(t, &simulator.Engine{}, `{`+settings+`, "end": 130000, "schedule": `+schedule+`}`)

			l, states := replay(t, `{`+settings+`}`, res.Request.Schedule, 130_000)
			is.True(t, l.last.Ended, "the session has ended")
			is.Equal(t, "", l.last.SchedulerError)
			is.EqualSlice(t, eventLines(t, res.Events), eventLines(t, l.events))
			is.Equal(t, res.Hash, l.last.Hash)
			is.Equal(t, res.Steps, l.last.Steps)
			is.Equal(t, fmt.Sprint(*res.Summary), fmt.Sprint(*l.last.Summary))

			// The state of the session after each action is the fold of the events it has given so
			// far, as a page that folds them itself would have it.
			for i, s := range states {
				want, err := json.Marshal(simulator.StateAt(simulator.Response{Events: l.events[:s.events]}, s.state.T), json.Deterministic(true))
				is.NotError(t, err)
				got, err := json.Marshal(s.state, json.Deterministic(true))
				is.NotError(t, err)
				is.Equal(t, string(want), string(got), fmt.Sprint("the state after action ", i))
			}
		}
	})

	t.Run("should give the same events, steps, summary, and hash as a batch run with the client, its switches, and a standby", func(t *testing.T) {
		// The first switch comes before the instances that stand by have started.
		schedule := `[
			{"at": 0, "do": "client", "instance": "b"}, {"at": 0, "do": "client", "instance": "a"},
			{"at": 10000, "do": "crash", "instance": "a"}, {"at": 12000, "do": "client", "instance": "c"},
			{"at": 30000, "do": "restart", "instance": "a"}, {"at": 45000, "do": "client", "instance": "a"},
			{"at": 50000, "do": "close", "instance": "a"}, {"at": 51000, "do": "client", "instance": "b"},
			{"at": 70000, "do": "open", "instance": "a"}, {"at": 80000, "do": "failDisk", "instance": "b", "on": true},
			{"at": 90000, "do": "failDisk", "instance": "b"}, {"at": 95000, "do": "client", "instance": "a"}]`
		for _, seed := range []int{1, 2, 3} {
			settings := fmt.Sprintf(`"seed": %v, "instances": 3, "faults": %v, "workload": {"callers": 0, "think": 500}, "client": {"instance": "a"}, "standby": true`, seed, allFaults)
			res := run(t, &simulator.Engine{}, `{`+settings+`, "end": 120000, "schedule": `+schedule+`}`)
			is.True(t, res.Summary.Calls["read served"]+res.Summary.Calls["write committed"] > 0, "the client is served")

			l, states := replay(t, `{`+settings+`}`, res.Request.Schedule, 120_000)
			is.True(t, l.last.Ended, "the session has ended")
			is.Equal(t, "", l.last.SchedulerError)
			is.EqualSlice(t, eventLines(t, res.Events), eventLines(t, l.events))
			is.Equal(t, res.Hash, l.last.Hash)
			is.Equal(t, res.Steps, l.last.Steps)
			is.Equal(t, fmt.Sprint(*res.Summary), fmt.Sprint(*l.last.Summary))
			for i, s := range states {
				want, err := json.Marshal(simulator.StateAt(simulator.Response{Events: l.events[:s.events]}, s.state.T), json.Deterministic(true))
				is.NotError(t, err)
				got, err := json.Marshal(s.state, json.Deterministic(true))
				is.NotError(t, err)
				is.Equal(t, string(want), string(got), fmt.Sprint("the state after action ", i))
			}
			is.Equal(t, "a", states[len(states)-1].state.Client.Instance)
		}
	})

	t.Run("should keep fake time still between advances, however long the page waits", func(t *testing.T) {
		l := startLive(t, `{"seed": 4}`)
		l.advanceTo(7000)
		before := l.do("now", "{}")
		is.Equal(t, 7_000.0, before.T)
		for range 50 {
			// Other goroutines get every chance to run between the commands.
			runtime.Gosched()
			after := l.do("now", "{}")
			is.Equal(t, before.T, after.T)
			is.Equal(t, before.Steps, after.Steps)
			is.Equal(t, 0, len(after.Events))
		}
		is.Equal(t, 9_000.0, l.advance(2000).T)
	})

	t.Run("should answer an advance with the events of the stretch, at times within it", func(t *testing.T) {
		l := startLive(t, `{"seed": 5}`)
		at := l.last.T
		for range 20 {
			a := l.advance(1500)
			is.Equal(t, at+1500, a.T)
			for _, e := range a.Events {
				is.True(t, e.T >= at && e.T <= a.T, fmt.Sprint("an event at ", e.T, " in the stretch to ", a.T))
			}
			at = a.T
		}
		is.True(t, slices.ContainsFunc(l.events, func(e simulator.Event) bool { return e.Kind == "call" && e.What == "served" }), "a read served")
	})

	t.Run("should refuse a malformed command, and go on", func(t *testing.T) {
		l := startLive(t, `{"seed": 6}`)
		l.advanceTo(1000)
		for _, test := range []struct{ kind, body, want string }{
			{"advance", `{"by": `, "not JSON of an advance"},
			{"advance", `{"by": -1}`, "by is -1"},
			{"advance", `{"for": 1}`, "not JSON of an advance"},
			{"advance", `{}`, "either by or to"},
			{"advance", `{"by": 1, "to": 2}`, "either by or to"},
			{"advance", `{"to": 99999999}`, "more than 1800000 after"},
			{"act", `{"do": "explode"}`, `no action is called "explode"`},
			{"act", `{"do": "crash"}`, "crash needs an instance"},
			{"act", `{"do": "crash", "instance": "c"}`, "crash needs an instance from a to b"},
			{"act", `{"at": 5, "do": "calm"}`, "has no at"},
			{"now", `{"commits": -1}`, "commits is -1"},
			{"end", `{"now": true}`, "not JSON of an end"},
			{"start", `{"instances": 9}`, "instances is 9"},
		} {
			a := l.try(test.kind, test.body)
			is.True(t, strings.Contains(a.Error, test.want), a.Error)
		}
		is.Equal(t, 2_000.0, l.advanceTo(2000).T)
	})

	t.Run("should answer a command without a session with an error", func(t *testing.T) {
		var e simulator.Engine
		for _, kind := range []string{"advance", "act", "now", "end"} {
			var a simulator.Live
			is.NotError(t, json.Unmarshal(e.Handle(t, kind, []byte(`{}`)), &a))
			is.True(t, strings.Contains(a.Error, "there is no session"), a.Error)
		}
		l := startLive(t, `{}`)
		l.advance(1000)
		l.do("end", "{}")
		is.True(t, strings.Contains(l.try("advance", `{"by": 1}`).Error, "there is no session"), "no session after the end")
	})

	t.Run("should end the session that runs when another starts", func(t *testing.T) {
		l := startLive(t, `{"seed": 7}`)
		l.advance(3000)
		a := l.do("start", `{"seed": 8}`)
		is.True(t, a.T < 1000, fmt.Sprint("a new session at ", a.T))
		is.Equal(t, 1_000.0, l.advanceTo(1000).T)
	})

	// The client alone calls less than the four callers: shorter pauses give it about as many calls,
	// and fewer rows to keep fill the table as early.
	for _, test := range []struct {
		settings string
		log      int
	}{
		{settings: `{"seed": 10}`, log: 500},
		{settings: `{"seed": 10, "workload": {"callers": 0, "think": 1000}, "client": {"instance": "a"}, "standby": true}`, log: 300},
	} {
		t.Run("should take as many steps and give as many events for a fake minute after an hour as at the start, with "+test.settings, func(t *testing.T) {
			l := startLive(t, test.settings)
			type minute struct{ steps, events, bytes int }
			var minutes []minute
			steps := l.last.Steps
			for range 60 {
				a := l.advance(60_000)
				b, err := json.Marshal(a.Events)
				is.NotError(t, err)
				minutes = append(minutes, minute{steps: a.Steps - steps, events: len(a.Events), bytes: len(b)})
				steps = a.Steps
				l.events = nil
			}
			mean := func(ms []minute) minute {
				var sum minute
				for _, m := range ms {
					sum.steps, sum.events, sum.bytes = sum.steps+m.steps, sum.events+m.events, sum.bytes+m.bytes
				}
				return minute{sum.steps / len(ms), sum.events / len(ms), sum.bytes / len(ms)}
			}
			// The table holds all the rows that the writes keep from about the eighth minute on, so a read
			// costs the same from then on.
			early, late := mean(minutes[10:20]), mean(minutes[50:60])
			t.Logf("steps, events, and bytes of events in a fake minute: %+v in minutes 10 to 20, %+v in minutes 50 to 60", early, late)
			is.True(t, float64(late.steps) < 1.15*float64(early.steps), "steps")
			is.True(t, float64(late.events) < 1.15*float64(early.events), "events")
			is.True(t, float64(late.bytes) < 1.15*float64(early.bytes), "bytes")
			rows := l.do("now", "{}").State
			is.True(t, len(rows.Bucket.Log) > test.log, fmt.Sprint("a long log of ", len(rows.Bucket.Log)))
		})
	}

	t.Run("should list only the last commit objects of the bucket if asked", func(t *testing.T) {
		l := startLive(t, `{"seed": 9}`)
		l.advance(60_000)
		all := l.do("now", "{}").State
		some := l.do("now", `{"commits": 3}`).State
		is.True(t, len(all.Bucket.Commits) > 3, "more than three commit objects")
		is.Equal(t, 3, len(some.Bucket.Commits))
		is.Equal(t, len(all.Bucket.Commits), some.Bucket.CommitCount)
		is.Equal(t, len(all.Bucket.Log), some.Bucket.LogLength)
		is.EqualSlice(t, all.Bucket.Log[len(all.Bucket.Log)-3:], some.Bucket.Log)
	})
}
