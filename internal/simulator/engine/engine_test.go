//go:build js && wasm && s4ql_browser

package engine_test

import (
	"syscall/js"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/simulator"
)

// TestEngine serves the messages of the page for as long as the program runs. The Worker of
// worker.js sets the object s4qlEngine, with the functions ready and done, before the program
// starts, and [serve] serves it. Without the object, as in a run of the tests, the test serves an
// object of its own, which plays the page.
func TestEngine(t *testing.T) {
	if hook := js.Global().Get("s4qlEngine"); hook.Type() == js.TypeObject {
		serve(t, hook, 0)
		return
	}

	t.Run("should take one message at a time, refuse a malformed one, and answer each on one engine", func(t *testing.T) {
		run, state := []byte(`{"seed": 1, "end": 1000}`), []byte(`{"t": 500}`)
		// The answer to the state depends on the run before it, so a serve that used a new engine
		// for each message would answer that there is no run yet.
		var e simulator.Engine
		want := []string{string(e.Handle(t, "run", run)), string(e.Handle(t, "state", state))}

		hook := js.Global().Get("Object").New()
		var accepted []bool
		var ids []int
		var answers []string
		ready := js.FuncOf(func(js.Value, []js.Value) any {
			submit := hook.Get("submit")
			// The first message waits for the engine, so the second is refused while it does.
			accepted = append(accepted, submit.Invoke(1, "run", string(run)).Bool(), submit.Invoke(2, "state", string(state)).Bool())
			return nil
		})
		defer ready.Release()
		done := js.FuncOf(func(_ js.Value, args []js.Value) any {
			ids = append(ids, args[0].Int())
			answers = append(answers, args[1].String())
			if len(answers) == 1 {
				// Nothing waits now, so the first is refused for its missing body alone.
				submit := hook.Get("submit")
				accepted = append(accepted, submit.Invoke(2, "state").Bool(), submit.Invoke(2, "state", string(state)).Bool())
			}
			return nil
		})
		defer done.Release()
		hook.Set("ready", ready)
		hook.Set("done", done)

		serve(t, hook, 2)
		is.EqualSlice(t, []bool{true, false, false, true}, accepted)
		is.EqualSlice(t, []int{1, 2}, ids)
		is.EqualSlice(t, want, answers)
	})
}

// serve the messages of hook: n of them, or for as long as the program runs if n is 0. serve sets
// hook.submit(id, kind, body), which takes one message at a time and returns false while the engine
// is busy, calls hook.ready() when it can take the first, and calls hook.done(id, answer) with the
// answer to each, as JSON. A run needs no JavaScript stack below it, so serve handles the messages
// on its own goroutine and not in the callback of submit.
func serve(t *testing.T, hook js.Value, n int) {
	t.Helper()
	type message struct {
		id   int
		kind string
		body []byte
	}
	messages := make(chan message, 1)
	submit := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) != 3 {
			return false
		}
		m := message{id: args[0].Int(), kind: args[1].String(), body: []byte(args[2].String())}
		select {
		case messages <- m:
			return true
		default:
			return false
		}
	})
	defer submit.Release()
	hook.Set("submit", submit)
	hook.Call("ready")

	var e simulator.Engine
	var served int
	for m := range messages {
		hook.Call("done", m.id, string(e.Handle(t, m.kind, m.body)))
		if served++; served == n {
			return
		}
	}
}
