// Package ctxtest has a context for tests that ends at a point of the code under test, and not at a
// point in time, so that a test of what a call does when its context ends during the call does not
// depend on timing.
package ctxtest

import (
	"context"
	"runtime"
	"sync"
	"time"
)

// Context that [EndAtErr] gives.
type Context struct {
	parent context.Context
	done   chan struct{}

	// mu guards the fields after it.
	mu sync.Mutex
	// left is the number of calls of Err up to the one that ends the context.
	left    int
	calls   int
	err     error
	endedBy string
}

// EndAtErr gives a context that ends with [context.Canceled] at the nth call of its Err, counted
// from 1, which gives [context.Canceled], as every later call does. A call before it gives nil, and
// an n below 1 ends the context at the first call. The context ends at no other time, not when
// parent ends, and has no deadline. Its values are those of parent.
//
// Code that looks at the Err of its context at points of its own, such as SQLite's progress handler
// every so many instructions of its virtual machine, so finds the context ended at the same point
// every time, on any machine.
func EndAtErr(parent context.Context, n int) *Context {
	return &Context{parent: parent, left: n, done: make(chan struct{})}
}

// EndedBy is the name of the function that made the call of Err that ended the context, such as
// "maragu.dev/s4ql/internal/sqlite.s4qlInterrupted", and "" before that call. A test so knows which
// of the points at which the code under test looks at the context found it ended.
func (c *Context) EndedBy() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.endedBy
}

// Calls of Err so far.
func (c *Context) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *Context) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (c *Context) Done() <-chan struct{} {
	return c.done
}

func (c *Context) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err == nil {
		if c.left--; c.left <= 0 {
			c.err = context.Canceled
			close(c.done)
			var pc [1]uintptr
			if runtime.Callers(2, pc[:]) == 1 {
				f, _ := runtime.CallersFrames(pc[:]).Next()
				c.endedBy = f.Function
			}
		}
	}
	return c.err
}

func (c *Context) Value(key any) any {
	return c.parent.Value(key)
}
