// Package s4ql is the public package of the module: the database/sql driver, which makes the
// production host and a context of its own, and keeps every other rule.
package s4ql

import (
	"context"
	"iter"
	"time"

	"maragu.dev/s4ql/internal/host"
)

func allowed() {
	// The exceptions of the package, each for one identifier.
	_ = host.New()
	_ = context.Background()
}

func forbidden(seq iter.Seq[int], ctx context.Context, c chan int) {
	// An identifier of the same package as an exception is none.
	one, stop := iter.Pull(seq) // want `iter.Pull starts a goroutine`
	_, _ = one()
	stop()
	_ = context.TODO()                       // want `context.TODO starts a context of its own`
	_, cancel := context.WithTimeout(ctx, 0) // want `context.WithTimeout sets a timer on the real clock`
	cancel()

	// Every other rule holds.
	go func() {}() // want `a go statement starts a goroutine outside the host`
	_ = time.Now() // want `time.Now reads the real clock`
	<-c            // want `a channel receive waits outside the host`
	c <- 1         // want `a channel send waits outside the host`
}
