//go:build s4ql_browser

package simulator

import (
	"context"
	"fmt"
	"time"

	"maragu.dev/s4ql/internal/host"
)

// standby starts the instances after the first once the first serves, or once it is clear that it
// does not: its process has crashed, its database is not open or stops, or the duration of its lease
// has passed by the time of the run. A task of the first process waits for its database to serve,
// because only a task of the process may wait through it, and this task, a task of the run, waits
// for that one.
func (r *run) standby(ctx context.Context) {
	first := r.instances[0]
	p := first.p
	if d := p.current(); d != nil && !p.inst.Crashed() {
		lease := first.lease
		serves := p.inst.Go(ctx, "serves", func(ctx context.Context) {
			ctx, cancel := p.inst.WithTimeout(ctx, lease)
			defer cancel()
			_ = d.Wait(ctx)
		})
		wait, cancel := r.world.WithTimeout(ctx, lease)
		_ = serves.Wait(wait)
		cancel()
	}
	if r.isEnding() {
		return
	}
	for _, in := range r.instances[1:] {
		r.start(ctx, in, cachePath(r.dir, in))
	}
}

// client of the run, as a task of the run, which no crash ends: until the end of the run, it pauses
// and then makes a call, a write or a read, on its target, the instance that it calls now. It first
// waits for the task gate, if any, which starts the instances that stand by.
func (r *run) client(ctx context.Context, gate host.Task) {
	if gate != nil {
		_ = gate.Wait(ctx)
	}
	w := r.req.Workload
	for !r.isEnding() {
		pause := time.Millisecond
		if think := duration(w.Think); think > 0 {
			pause += time.Duration(r.world.Uint64() % uint64(think))
		}
		_ = r.world.Sleep(ctx, pause)
		if r.isEnding() {
			return
		}
		r.clientCall(ctx, int(r.world.Uint64()%100) < w.Writes)
	}
}

// clientCall makes one call of the client on its target. The call fails at once with "down" if the
// target cannot take it. Otherwise a task of the process of the target makes it, as the application
// in the process serves a request, and the client waits for that task. If the process crashes first,
// the call ends with "lost": the client gets no answer, so the outcome of a write is unknown.
func (r *run) clientCall(ctx context.Context, write bool) {
	r.mu.Lock()
	in := r.target
	r.mu.Unlock()
	p := in.p
	c := r.newCall("client", write)
	r.rec.add(c.event(in, p, "started"))
	if why := down(p); why != "" {
		r.callDown(ctx, c, in, p, why)
		return
	}
	task := p.inst.Go(ctx, fmt.Sprint("client-", c.n), func(ctx context.Context) {
		// The process can have lost its database since the call was sent.
		if why := down(p); why != "" {
			r.callDown(ctx, c, in, p, why)
			return
		}
		r.serveCall(ctx, p, c)
	})
	_ = task.Wait(ctx)
	r.mu.Lock()
	ended := c.ended
	r.mu.Unlock()
	if !ended {
		e := c.event(in, p, "lost")
		e.Error = "the process of the instance crashed during the call"
		r.callEnded(ctx, c, e)
	}
}

// callDown ends the call c on the instance in, whose process is p or nil, with "down", for the
// reason why.
func (r *run) callDown(ctx context.Context, c *call, in *instance, p *process, why string) {
	e := c.event(in, p, "down")
	e.Error = why
	r.callEnded(ctx, c, e)
}

// down says why the process p cannot take a call of the client, or returns "" if it can. It cannot if
// it is nil, because its instance has not started, if it has crashed, or if its database is not open.
func down(p *process) string {
	switch {
	case p == nil:
		return "the instance has not started"
	case p.inst.Crashed():
		return "the process of the instance has crashed"
	}
	p.mu.Lock()
	d, shut, closed, failed := p.db, p.shut, p.closed, p.failed
	p.mu.Unlock()
	switch {
	case failed:
		return "the database of the instance could not be opened"
	case d == nil:
		return "the database of the instance is not open"
	case closed || d == shut:
		return "the database of the instance is closed"
	case stopped(d):
		return "the database of the instance has stopped"
	}
	return ""
}
