//go:build s4ql_browser

// Package simulator runs the storage core of s4ql, unchanged, under the simulation of internal/sim
// with the simulated store of internal/store/simstore, for a page that shows how the code reacts to
// faults of its environment. It builds only with the tag s4ql_browser, and runs natively as well as in
// a browser.
//
// A run is deterministic: a [Request], which is a seed, settings, and a schedule of actions and faults
// at fake times, gives the same [Response], byte for byte, every time. Two requests whose schedules
// agree before a time T give traces that agree before T, so a page can show a run up to T, add an
// action at T, and run again, and the visitor sees the run go on from where it was.
//
// [Engine.Handle] takes a request as JSON and returns the response as JSON. The run happens in a
// testing/synctest bubble of its own, which needs a [testing.T]: a program gets one by being a test
// binary, as internal/simulator/engine is.
//
// The README next to this file describes the request, the events of the trace, the state at a time,
// and the invariants that the engine checks.
package simulator

import (
	"encoding/json/v2"
	"fmt"
	"testing"
)

// Engine of runs, which keeps the last run for the state at a time.
type Engine struct {
	last *Response
}

// Handle a message of the kind "run" or "state", with its body as JSON, and return the answer as
// JSON. A message that cannot be handled gets an answer with only an error, and the engine goes on:
//
//   - "run" takes a [Request] and answers with a [Response].
//   - "state" takes {"t": T} and answers with the [State] of the last run at the time T.
func (e *Engine) Handle(t *testing.T, kind string, body []byte) []byte {
	switch kind {
	case "run":
		req, err := ParseRequest(body)
		if err != nil {
			return marshal(Response{Error: err.Error()})
		}
		res := simulate(t, req)
		e.last = &res
		return marshal(res)
	case "state":
		var q struct {
			T float64 `json:"t"`
		}
		if err := json.Unmarshal(body, &q, json.RejectUnknownMembers(true)); err != nil {
			return marshal(State{Error: fmt.Sprintf("the message is not JSON of a time: %v", err)})
		}
		if e.last == nil {
			return marshal(State{Error: "there is no run yet"})
		}
		return marshal(StateAt(*e.last, q.T))
	default:
		return marshal(Response{Error: fmt.Sprintf("no message is of the kind %q; the kinds are run and state", kind)})
	}
}

// marshal v as JSON, the same bytes for the same value.
func marshal(v any) []byte {
	b, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		b, _ = json.Marshal(Response{Error: "the answer cannot be written as JSON: " + err.Error()})
	}
	return b
}
