//go:build js && wasm && s4ql_browser

package engine_test

import (
	"syscall/js"
	"testing"

	"maragu.dev/s4ql/internal/simulator"
)

// TestEngine serves the messages of the page for as long as the program runs. The Worker of
// worker.js sets the object s4qlEngine, with the functions ready and done, before the program
// starts. The test sets s4qlEngine.submit(id, kind, body), which takes one message at a time and
// returns false while the engine is busy, calls ready() when it can take the first, and calls
// done(id, answer) with the answer to each, as JSON. A run needs no JavaScript stack below it, so the
// test handles the messages on its own goroutine and not in the callback of submit.
func TestEngine(t *testing.T) {
	hook := js.Global().Get("s4qlEngine")
	if hook.Type() != js.TypeObject {
		t.Skip("no page drives the engine: s4qlEngine is not set")
	}
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
	for m := range messages {
		hook.Call("done", m.id, string(e.Handle(t, m.kind, m.body)))
	}
}
