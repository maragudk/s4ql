//go:build s4ql_browser

package simulator_test

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/simulator"
)

// allFaults of the store, as simstore.AllFaults has them, with damaged downloads.
const allFaults = `{"lostRequest": 0.02, "lostResponse": 0.02, "hang": 0.2, "conflict": 0.1, "slow": 0.02, "slowBy": 45000,
	"brokenRead": 0.02, "cutOff": 0.005, "cutOffFor": 40000, "outage": 0.002, "outageFor": 20000,
	"late": 0.5, "afterCrash": 0.5, "lateBy": 60000, "damaged": 0.02}`

// run the request, given as JSON, on e, and decode the response. The run must not fail.
func run(t *testing.T, e *simulator.Engine, request string) simulator.Response {
	t.Helper()
	var res simulator.Response
	is.NotError(t, json.Unmarshal(e.Handle(t, "run", []byte(request)), &res))
	is.Equal(t, "", res.Error)
	is.Equal(t, "", res.SchedulerError)
	return res
}

// lines of the events of res, each as its JSON.
func lines(t *testing.T, res simulator.Response) []string {
	t.Helper()
	var out []string
	for _, e := range res.Events {
		b, err := json.Marshal(e, json.Deterministic(true))
		is.NotError(t, err)
		out = append(out, string(b))
	}
	return out
}

func TestEngine_Handle(t *testing.T) {
	t.Run("should give the same bytes for the same request twice, also on another engine", func(t *testing.T) {
		request := `{"seed": 3, "end": 180000, "buggify": true, "faults": ` + allFaults + `, "schedule": [
			{"at": 20000, "do": "crash", "instance": "a"}, {"at": 30000, "do": "restart", "instance": "a"},
			{"at": 40000, "do": "loseAnswer", "request": "commit"}, {"at": 50000, "do": "shiftWall", "instance": "b", "by": -60000},
			{"at": 60000, "do": "close", "instance": "c"}, {"at": 70000, "do": "open", "instance": "c"}]}`
		var e1, e2 simulator.Engine
		first := e1.Handle(t, "run", []byte(request))
		second := e1.Handle(t, "run", []byte(request))
		third := e2.Handle(t, "run", []byte(request))
		is.True(t, bytes.Equal(first, second), "the same bytes twice")
		is.True(t, bytes.Equal(first, third), "the same bytes on another engine")
		var res simulator.Response
		is.NotError(t, json.Unmarshal(first, &res))
		is.Equal(t, "", res.SchedulerError)
		is.True(t, len(res.Events) > 100, "a trace with events")
	})

	t.Run("should give the same events for many seeds with every fault, twice each", func(t *testing.T) {
		var e simulator.Engine
		for seed := range 10 {
			request := fmt.Sprintf(`{"seed": %v, "end": 90000, "buggify": true, "faults": %v, "schedule": [
				{"at": 15000, "do": "crash", "instance": "b"}, {"at": 25000, "do": "restart", "instance": "b"}]}`, seed+100, allFaults)
			first, second := run(t, &e, request), run(t, &e, request)
			is.Equal(t, first.Hash, second.Hash)
			is.EqualSlice(t, lines(t, first), lines(t, second))
			is.Equal(t, 0, first.Summary.Violations-first.Summary.Excused, fmt.Sprint("seed ", seed+100))
		}
	})

	t.Run("should give the same events before the time from which two schedules differ", func(t *testing.T) {
		var e simulator.Engine
		common := `{"at": 20000, "do": "crash", "instance": "a"}, {"at": 35000, "do": "restart", "instance": "a"}`
		for _, at := range []float64{40_000, 61_234.5} {
			before := run(t, &e, `{"seed": 9, "end": 150000, "faults": `+allFaults+`, "schedule": [`+common+`]}`)
			after := run(t, &e, fmt.Sprintf(`{"seed": 9, "end": 150000, "faults": %v, "schedule": [%v,
				{"at": %v, "do": "crash", "instance": "b"}, {"at": %v, "do": "loseAnswer", "request": "any"}]}`, allFaults, common, at, at))
			a, b := prefix(t, before, at), prefix(t, after, at)
			is.True(t, len(a) > 50, "events before the time")
			is.EqualSlice(t, a, b)
			is.True(t, len(lines(t, before)) > len(a), "events after the time")
			is.True(t, !slices.Equal(lines(t, before), lines(t, after)), "different events after the time")
		}
	})

	t.Run("should give the same events before the end for a later end", func(t *testing.T) {
		var e simulator.Engine
		short := run(t, &e, `{"seed": 4, "end": 60000, "faults": `+allFaults+`}`)
		long := run(t, &e, `{"seed": 4, "end": 120000, "faults": `+allFaults+`}`)
		is.EqualSlice(t, prefix(t, short, 60_000), prefix(t, long, 60_000))
	})

	t.Run("should answer a malformed request with an error and go on", func(t *testing.T) {
		var e simulator.Engine
		for _, request := range []string{`{"seed": `, `{"instances": 9}`, `[]`, ``, `{"schedule": [{"at": -1, "do": "crash", "instance": "a"}]}`} {
			var res simulator.Response
			is.NotError(t, json.Unmarshal(e.Handle(t, "run", []byte(request)), &res))
			is.True(t, res.Error != "", "an error for "+request)
			is.Equal(t, 0, len(res.Events))
		}
		res := run(t, &e, `{"end": 10000}`)
		is.True(t, len(res.Events) > 0, "a run after the errors")
	})

	t.Run("should answer a message of an unknown kind with an error", func(t *testing.T) {
		var e simulator.Engine
		var res simulator.Response
		is.NotError(t, json.Unmarshal(e.Handle(t, "walk", []byte(`{}`)), &res))
		is.True(t, strings.Contains(res.Error, `"walk"`), res.Error)
	})

	t.Run("should answer a state before any run with an error", func(t *testing.T) {
		var e simulator.Engine
		var s simulator.State
		is.NotError(t, json.Unmarshal(e.Handle(t, "state", []byte(`{"t": 1000}`)), &s))
		is.Equal(t, "there is no run yet", s.Error)
	})

	t.Run("should answer a malformed state with an error", func(t *testing.T) {
		var e simulator.Engine
		run(t, &e, `{"end": 10000}`)
		var s simulator.State
		is.NotError(t, json.Unmarshal(e.Handle(t, "state", []byte(`{"time": 1000}`)), &s))
		is.True(t, strings.Contains(s.Error, "not JSON of a time"), s.Error)
	})

	t.Run("should answer a state of the last run", func(t *testing.T) {
		var e simulator.Engine
		run(t, &e, `{"end": 30000, "instances": 2}`)
		var s simulator.State
		is.NotError(t, json.Unmarshal(e.Handle(t, "state", []byte(`{"t": 29000}`)), &s))
		is.Equal(t, "", s.Error)
		is.Equal(t, 2, len(s.Instances))
		is.Equal(t, 29_000.0, s.T)
	})

	t.Run("should give the scheduler's lines when asked", func(t *testing.T) {
		var e simulator.Engine
		res := run(t, &e, `{"end": 5000, "lines": true}`)
		is.Equal(t, res.Steps, countSteps(res.Lines))
	})
}

// prefix of the events of res before the time at, as JSON lines.
func prefix(t *testing.T, res simulator.Response, at float64) []string {
	t.Helper()
	var out []string
	for i, e := range res.Events {
		if e.T >= at {
			break
		}
		out = append(out, lines(t, simulator.Response{Events: res.Events[i : i+1]})...)
	}
	return out
}

// countSteps in the lines of the scheduler: one line for each step, which says where a task resumes.
func countSteps(lines []string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, " resumes from ") {
			n++
		}
	}
	return n
}
