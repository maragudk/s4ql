//go:build s4ql_browser

// Package simulator runs the storage core of s4ql, unchanged, under the simulation of internal/sim
// with the simulated store of internal/store/simstore, for a page that shows how the code reacts to
// faults of its environment. It builds only with the tag s4ql_browser, and runs natively as well as in
// a browser.
//
// It runs in two ways. A batch run takes a [Request], which is a seed, settings, and a schedule of
// actions and faults at fake times, and runs it to its end: the same request gives the same
// [Response], byte for byte, every time. A live session takes a [Session], the settings and a seed,
// and has no end: the page advances its fake time, acts at the time of the session, and asks for the
// state now, one command at a time, and fake time stands still between the commands. A session that
// gets the actions of a request's schedule at their times gives the same events as the batch run, and
// the same hash.
//
// [Engine.Handle] takes a message as JSON and returns the answer as JSON. A run happens in a
// testing/synctest bubble of its own, which needs a [testing.T]: a program gets one by being a test
// binary, as internal/simulator/engine is.
//
// The README next to this file describes the messages, the request, the session, the events of the
// trace, the state, and the invariants that the engine checks.
package simulator

import (
	"encoding/json/v2"
	"fmt"
	"testing"
)

// Engine of runs, which keeps the last batch run for the state at a time, and the live session, if
// one has started and not ended.
type Engine struct {
	last *Response
	live *session
}

// Handle a message of the given kind, with its body as JSON, and return the answer as JSON. A message
// that cannot be handled gets an answer with only an error, and the engine, and its session, go on:
//
//   - "run" takes a [Request] and answers with a [Response].
//   - "state" takes {"t": T} and answers with the [State] of the last run at the time T.
//   - "start" takes a [Session], ends the session that runs, if any, starts a new one, and answers
//     with a [Live] of its events at time 0.
//   - "advance" takes {"by": D} and moves the session's fake time on by D milliseconds.
//   - "act" takes an [Action] without "at" and does it at the session's time.
//   - "now" takes {} or {"commits": N} and answers with the [State] of the session now.
//   - "end" takes {} and ends the session, with the last checks of its invariants and its hash.
//
// The commands of a session answer with a [Live]. The session runs on a goroutine of its own, in a
// bubble of t, so t must outlive it: [Engine.Close] ends it.
func (e *Engine) Handle(t *testing.T, kind string, body []byte) []byte {
	switch kind {
	case "run":
		req, err := ParseRequest(body)
		if err != nil {
			// A state from now on would be of a run that the page did not ask for last.
			e.last = nil
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
	case "start":
		req, err := ParseSession(body)
		if err != nil {
			return marshal(Live{Error: err.Error()})
		}
		e.Close()
		s, a := startSession(t, req)
		if !a.final {
			e.live = s
		}
		return a.body
	case "advance", "act", "now", "end":
		if e.live == nil {
			return marshal(Live{Error: "there is no session: start one"})
		}
		c, err := e.live.parseCommand(kind, body)
		if err != nil {
			return marshal(Live{Error: err.Error()})
		}
		a := e.live.do(c)
		if a.final {
			e.live = nil
		}
		return a.body
	default:
		return marshal(Response{Error: fmt.Sprintf("no message is of the kind %q; the kinds are run, state, start, advance, act, now, and end", kind)})
	}
}

// Close the engine: end its session, if one runs. The engine can be used again afterwards.
func (e *Engine) Close() {
	if e.live != nil {
		e.live.do(command{kind: "end"})
		e.live = nil
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
