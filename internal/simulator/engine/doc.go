//go:build s4ql_browser

// Package engine is the program of the simulator in a browser. The program is the test binary of
// this package, built with GOOS=js GOARCH=wasm and the tag s4ql_browser, because a run of
// maragu.dev/s4ql/internal/simulator needs a testing.T, and a test binary is the supported way to
// get one. Its one test, TestEngine, serves the messages that worker.js passes to it, and skips when
// no page drives it.
//
// The files next to this one are what a page needs besides the program and the files of
// internal/sqlite/browser: worker.js, the Worker that runs the program; simulator.js, the module
// that a page imports to talk to the Worker; and index.html, a page that shows the whole path
// working. See the README of internal/simulator.
package engine
