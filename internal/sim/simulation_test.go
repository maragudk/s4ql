package sim_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/sqlite"
)

// clockFaults that a workload does to the clocks of its instances, as a set of bits. Each instance
// gets at most one of them in its life, so that at most one of its two clocks is wrong at a time.
//
// The faults are those that the lease's design survives. A wall clock set backward on a holder that
// can still reach the store is not one of them: the holder renews with an expiry that is already past
// on the other instances' clocks, another takes the lease, and both of the holder's own checks still
// pass. So wallBackward cuts the instance off from the store for as long as its clock is wrong.
type clockFaults int

const (
	// stopMonotonic holds an instance's monotonic clock still for a while.
	stopMonotonic clockFaults = 1 << iota
	// wallBackward sets an instance's wall clock backward for a while, and cuts it off from the store
	// for as long, so that it writes no expiry from the wrong clock.
	wallBackward
	// wallForward sets an instance's wall clock forward by less than the takeover margin, for good.
	wallForward
	// partition cuts an instance off from the store for a while, and moves no clock. It is not a fault
	// of a clock, and is there to show what wallBackward does without the shift.
	partition
)

// workload that callers run against [toyDB] instances.
type workload struct {
	// instances that run at the start, each with its own cache.
	instances int
	// callers is the number of tasks that use each instance at the same time.
	callers int
	// ops is the number of transactions that each caller runs.
	ops int
	// think is the longest simulated time that a caller waits before each transaction.
	think time.Duration
	// latency is the unit of the simulated store's latency.
	latency time.Duration
	options toyOptions
	faults  faults
	// clocks that the workload moves, under the scheduler only.
	clocks clockFaults
	// crashes of a random instance, each followed by a restart on what it left on disk, under the
	// scheduler only.
	crashes int
	// buggify for the run.
	buggify bool
}

// leaseLossExpected reports whether an instance may lose its lease, which it can only to another.
func (w workload) leaseLossExpected() bool {
	return w.instances > 1
}

// singleWorkload has one instance, and enough callers, simulated time, and faults for commits,
// renewals, reads, and lease timeouts to overlap.
var singleWorkload = workload{
	instances: 1,
	callers:   4,
	ops:       12,
	think:     15 * time.Second,
	latency:   time.Millisecond,
	options:   toyOptions{readers: 2, busyTimeout: 1000},
	faults:    faults{errors: 30, lostAcks: 30, delays: 10},
}

// fullWorkload has everything the harness can do: two instances that compete for the lease, clocks
// that move, a crash and a restart, buggify, and faults of the store.
var fullWorkload = workload{
	instances: 2,
	callers:   3,
	ops:       12,
	think:     15 * time.Second,
	latency:   time.Millisecond,
	options:   toyOptions{readers: 2, busyTimeout: 1000},
	faults:    faults{errors: 30, lostAcks: 30, delays: 10},
	clocks:    stopMonotonic | wallBackward | wallForward,
	crashes:   1,
	buggify:   true,
}

// hammerWorkload has callers that never wait between transactions, so that with the scheduler off
// they are in SQLite and in the toy library's own code at the same time.
var hammerWorkload = workload{
	instances: 1,
	callers:   8,
	ops:       40,
	latency:   time.Millisecond,
	options:   toyOptions{readers: 4, busyTimeout: 1000},
	faults:    faults{errors: 30, lostAcks: 30, delays: 10},
}

// attempt of a commit, as the caller that made it saw it.
type attempt struct {
	// id of the transaction, as in its change file.
	id   uint64
	seq  int64
	sets [][2]int64
	// state is pending until Commit returns, and stays pending if the caller's instance crashed.
	state attemptState
}

type attemptState int

const (
	pending attemptState = iota
	acknowledged
	failed
	// unknown is a commit whose outcome Commit reported as unknown.
	unknown
)

// outcome of running a [workload].
type outcome struct {
	commits, failedCommits, unknownCommits, rollbacks, reads, failedReads, expiredReads atomic.Int64
	// cutCommits are commits whose caller's instance crashed before Commit returned.
	cutCommits atomic.Int64
	// overtakenReads are reads that ended when the manifest had a commit that their snapshot did not,
	// so a commit was in progress or finished while they ran.
	overtakenReads atomic.Int64
	// stops of an instance, and restarts after a crash.
	stops, restarts atomic.Int64
	// acked is the highest commit that a caller knows to be acknowledged.
	acked atomic.Int64

	mu sync.Mutex
	// violations of the invariants that the workload checks.
	violations []string
	attempts   []*attempt
	// crashed databases, whose files are closed after the run.
	crashed []*toyDB
}

func newOutcome() *outcome {
	return &outcome{}
}

// violate records a violation, also in the trace of h.
func (o *outcome) violate(ctx context.Context, h tracer, format string, args ...any) {
	h.Trace(ctx, "VIOLATION: "+format, args...)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.violations = append(o.violations, fmt.Sprintf(format, args...))
}

// expected reports whether err from a call of a caller is one that the faults and the design
// explain, and for an error that joins several, whether each of them is. An instance that stopped for
// a reason that is not explained is reported by its node.
func expected(err error) bool {
	if errors.Is(err, errStopped) {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, err := range joined.Unwrap() {
			if err != nil && !expected(err) {
				return false
			}
		}
		return true
	}
	for _, target := range []error{errInjected, errSwapFailed, errLeaseExpired, errNotServing, errStopped, errOutcomeUnknown, errPrecondition, context.DeadlineExceeded} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// cluster is where a workload runs: the host of the test itself, and a way to start the host of an
// instance.
type cluster struct {
	world tracer
	// instance starts the host of a new instance. Under the scheduler it is a [*sim.Instance].
	instance func(name string) tracer
	// dir for the caches.
	dir string
}

// node is one process: an instance, its cache, its database, and its callers.
type node struct {
	name   string
	host   tracer
	client *storeClient
	path   string
	// callers that have not returned.
	callers atomic.Int64

	mu sync.Mutex
	db *toyDB
}

func (n *node) current() *toyDB {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.db
}

// do f as a task of the node's instance, and wait until it has returned, because only a task of an
// instance may wait through its host.
func (n *node) do(ctx context.Context, name string, f func(ctx context.Context)) {
	_ = n.host.Go(ctx, name, f).Wait(ctx)
}

// run the workload w as the task of ctx, in the cluster c, and record what happened in o. It checks
// these invariants:
//
//   - A read that starts after a commit is acknowledged, by any instance, sees that commit.
//   - A read never sees a commit that the manifest does not have, so the cache is never ahead.
//   - An instance stops only for a reason that the faults explain.
//   - Some instance serves again within two minutes of the end of the workload, also after a crash.
//   - At the end, the log holds every acknowledged commit with the writes that its caller made, and
//     no commit that is known to have failed, and every cache holds what the log says up to its
//     commit.
//   - After every instance is closed, the lease is released, or held by one that could not release
//     it.
func (w workload) run(ctx context.Context, c cluster, o *outcome) {
	store := newMemStore(c.world, w.faults, w.latency)
	var mu sync.Mutex
	var nodes []*node
	// holders that may hold the lease at the end: crashed databases, and those that could not release
	// it.
	holders := map[string]bool{}

	write := func(ctx context.Context, n *node, d *toyDB) {
		tx, err := d.Begin(ctx)
		if err != nil {
			if !expected(err) {
				o.violate(ctx, c.world, "begin: %v", err)
			}
			return
		}
		var sets [][2]int64
		for range 1 + intn(n.host, 3) {
			k := int64(intn(n.host, 5))
			if err := tx.Set(k); err != nil {
				o.violate(ctx, c.world, "set: %v", err)
			}
			sets = append(sets, [2]int64{k, tx.seq})
			c.world.Trace(ctx, "set %v in transaction %v", k, tx.seq)
			n.host.Yield(ctx, "caller: between statements")
		}
		if intn(n.host, 10) == 0 {
			if err := tx.Rollback(ctx); err != nil {
				o.violate(ctx, c.world, "rollback: %v", err)
			}
			o.rollbacks.Add(1)
			c.world.Trace(ctx, "rollback of transaction %v", tx.seq)
			return
		}

		a := &attempt{id: tx.id, seq: tx.seq, sets: sets}
		o.mu.Lock()
		o.attempts = append(o.attempts, a)
		o.mu.Unlock()
		err = tx.Commit(ctx)
		o.mu.Lock()
		switch {
		case err == nil:
			a.state = acknowledged
		case errors.Is(err, errOutcomeUnknown):
			a.state = unknown
		default:
			a.state = failed
		}
		o.mu.Unlock()
		switch {
		case err == nil:
			for {
				high := o.acked.Load()
				if tx.seq <= high || o.acked.CompareAndSwap(high, tx.seq) {
					break
				}
			}
			o.commits.Add(1)
			c.world.Trace(ctx, "commit of transaction %v: acknowledged", tx.seq)
		case errors.Is(err, errOutcomeUnknown):
			o.unknownCommits.Add(1)
			c.world.Trace(ctx, "commit of transaction %v: %v", tx.seq, err)
		default:
			o.failedCommits.Add(1)
			c.world.Trace(ctx, "commit of transaction %v: %v", tx.seq, err)
			if !expected(err) {
				o.violate(ctx, c.world, "commit of transaction %v: %v", tx.seq, err)
			}
		}
	}

	read := func(ctx context.Context, n *node, d *toyDB) {
		before := o.acked.Load()
		k := int64(intn(n.host, 5))
		seq, v, err := d.Read(ctx, k)
		if err != nil {
			o.failedReads.Add(1)
			if errors.Is(err, errLeaseExpired) {
				o.expiredReads.Add(1)
			}
			c.world.Trace(ctx, "read of %v: %v", k, err)
			if !expected(err) {
				o.violate(ctx, c.world, "read of %v: %v", k, err)
			}
			return
		}
		man, err := store.manifest()
		if err != nil {
			o.violate(ctx, c.world, "manifest: %v", err)
		}
		o.reads.Add(1)
		if man.Seq > seq {
			o.overtakenReads.Add(1)
		}
		c.world.Trace(ctx, "read of %v: %v as of commit %v", k, v, seq)
		if seq < before {
			o.violate(ctx, c.world, "a read saw commit %v after commit %v was acknowledged", seq, before)
		}
		if seq > man.Seq {
			o.violate(ctx, c.world, "a read saw commit %v while the manifest was at %v", seq, man.Seq)
		}
		if want, err := valueAt(store, seq, k); err != nil {
			o.violate(ctx, c.world, "the log: %v", err)
		} else if v != want {
			o.violate(ctx, c.world, "a read saw %v at key %v at commit %v, and the log has %v", v, k, seq, want)
		}
	}

	// stopped checks why the database of n stopped, closes it, and opens it again, as an application
	// does.
	stopped := func(ctx context.Context, n *node, d *toyDB) {
		cause := d.stopErr()
		o.stops.Add(1)
		c.world.Trace(ctx, "%v", cause)
		explained := errors.Is(cause, errOutcomeUnknown) ||
			(errors.Is(cause, errLeaseLost) && w.leaseLossExpected()) ||
			(errors.Is(cause, errLocal) && w.buggify)
		if !explained {
			o.violate(ctx, c.world, "the instance stopped: %v", cause)
		}
		if err := d.Close(ctx); err != nil {
			mu.Lock()
			holders[d.id] = true
			mu.Unlock()
			if !expected(err) && !errors.Is(err, errPrecondition) {
				o.violate(ctx, c.world, "close: %v", err)
			}
		}
		next, err := openToy(ctx, n.host, n.client, n.path, w.options)
		if err != nil {
			o.violate(ctx, c.world, "open again: %v", err)
			return
		}
		n.mu.Lock()
		n.db = next
		n.mu.Unlock()
	}

	// start a node with its callers and the task that opens its database again when it stops, and
	// return the tasks.
	start := func(ctx context.Context, name, path string) (*node, []host.Task) {
		n := &node{name: name, host: c.instance(name), client: store.client(), path: path}
		d, err := openToy(ctx, n.host, n.client, path, w.options)
		if err != nil {
			o.violate(ctx, c.world, "open: %v", err)
			return n, nil
		}
		n.db = d
		mu.Lock()
		nodes = append(nodes, n)
		mu.Unlock()

		var tasks []host.Task
		n.callers.Store(int64(w.callers))
		for i := range w.callers {
			tasks = append(tasks, n.host.Go(ctx, fmt.Sprint("caller-", i), func(ctx context.Context) {
				defer n.callers.Add(-1)
				for range w.ops {
					if w.think > 0 {
						_ = n.host.Sleep(ctx, time.Duration(intn(n.host, int(w.think/time.Millisecond)))*time.Millisecond)
					}
					d := n.current()
					// Every call has a timeout, as a request of an application has.
					ctx, cancel := n.host.WithTimeout(ctx, 30*time.Second)
					if intn(n.host, 100) < 35 {
						write(ctx, n, d)
					} else {
						read(ctx, n, d)
					}
					cancel()
				}
			}))
		}
		tasks = append(tasks, n.host.Go(ctx, "supervisor", func(ctx context.Context) {
			for n.callers.Load() > 0 {
				if d := n.current(); d.stopErr() != nil {
					stopped(ctx, n, d)
				}
				_ = n.host.Sleep(ctx, 2*time.Second)
			}
		}))
		return n, tasks
	}

	var tasks []host.Task
	for i := range w.instances {
		name := string(rune('a' + i))
		n, nodeTasks := start(ctx, name, filepath.Join(c.dir, name+".db"))
		tasks = append(tasks, nodeTasks...)
		if inst, ok := n.host.(*sim.Instance); ok && w.clocks != 0 {
			tasks = append(tasks, c.world.Go(ctx, "clocks of "+name, func(ctx context.Context) {
				w.moveClocks(ctx, c.world, inst, n)
			}))
		}
	}
	if _, ok := c.world.(*sim.Scheduler); ok && w.crashes > 0 {
		tasks = append(tasks, c.world.Go(ctx, "crasher", func(ctx context.Context) {
			var restarted []host.Task
			for i := range w.crashes {
				_ = c.world.Sleep(ctx, time.Duration(10_000+intn(c.world, 110_000))*time.Millisecond)
				mu.Lock()
				var live []*node
				for _, n := range nodes {
					if !n.host.(*sim.Instance).Crashed() {
						live = append(live, n)
					}
				}
				mu.Unlock()
				if len(live) == 0 {
					break
				}
				victim := live[intn(c.world, len(live))]
				victim.host.(*sim.Instance).Crash()
				d := victim.current()
				o.mu.Lock()
				o.crashed = append(o.crashed, d)
				o.mu.Unlock()
				mu.Lock()
				holders[d.id] = true
				mu.Unlock()

				// The restarted instance finds the files as the crash left them, with every commit that
				// the cache had.
				name := fmt.Sprint(victim.name, i+2)
				path := filepath.Join(c.dir, name+".db")
				crashed, err := cacheState(victim.path)
				if err != nil {
					o.violate(ctx, c.world, "the cache of %v: %v", victim.name, err)
					return
				}
				if err := copyCache(victim.path, path); err != nil {
					o.violate(ctx, c.world, "copy of the cache: %v", err)
					return
				}
				if restarted, err := cacheState(path); err != nil || restarted != crashed {
					o.violate(ctx, c.world, "the cache of %v is %v %v after the restart, and was %v at the crash", name, restarted, err, crashed)
				}
				c.world.Trace(ctx, "%v restarts on the cache of %v at commit %v", name, victim.name, crashed.seq)
				o.restarts.Add(1)
				_, nodeTasks := start(ctx, name, path)
				restarted = append(restarted, nodeTasks...)
			}
			for _, task := range restarted {
				_ = task.Wait(ctx)
			}
		}))
	}
	for _, task := range tasks {
		if err := task.Wait(ctx); err != nil {
			o.violate(ctx, c.world, "wait: %v", err)
		}
	}

	var live []*node
	for _, n := range nodes {
		if inst, ok := n.host.(*sim.Instance); !ok || !inst.Crashed() {
			live = append(live, n)
		}
	}

	// The clocks are right again and nothing crashes any more, so an instance must serve soon: an
	// expired lease is taken over within its margin, and catching up takes seconds.
	served := false
	for range 120 {
		for _, n := range live {
			n.do(ctx, "end", func(ctx context.Context) {
				if d := n.current(); d.stopErr() != nil {
					stopped(ctx, n, d)
				}
				if _, _, err := n.current().Read(ctx, 0); err == nil {
					served = true
				}
			})
		}
		if served {
			break
		}
		_ = c.world.Sleep(ctx, time.Second)
	}
	if !served && len(live) > 0 {
		o.violate(ctx, c.world, "no instance serves two minutes after the end of the workload")
	}

	for _, n := range live {
		n.do(ctx, "closer", func(ctx context.Context) {
			d := n.current()
			if err := d.Close(ctx); err != nil {
				mu.Lock()
				holders[d.id] = true
				mu.Unlock()
				if !expected(err) && !errors.Is(err, errPrecondition) {
					o.violate(ctx, c.world, "close: %v", err)
				}
			}
		})
	}
	o.verify(ctx, c.world, store, live, holders)
}

// moveClocks of the instance of n by one fault that the seed picks from those of w, at a time that
// the seed picks, or by none.
func (w workload) moveClocks(ctx context.Context, h tracer, inst *sim.Instance, n *node) {
	var kinds []clockFaults
	for _, kind := range []clockFaults{stopMonotonic, wallBackward, wallForward, partition} {
		if w.clocks&kind != 0 {
			kinds = append(kinds, kind)
		}
	}
	// As often as not, the clocks of an instance are right.
	kind := intn(h, 2*len(kinds))
	if kind >= len(kinds) {
		return
	}
	if err := h.Sleep(ctx, time.Duration(intn(h, 100_000))*time.Millisecond); err != nil {
		return
	}
	span := time.Duration(20_000+intn(h, 60_000)) * time.Millisecond
	switch kinds[kind] {
	case stopMonotonic:
		inst.StopMonotonic()
		_ = h.Sleep(ctx, span)
		inst.StartMonotonic()
	case wallBackward, partition:
		// Both draw the same numbers, so that a seed gives the same partition with and without the shift.
		n.client.partitioned.Store(true)
		h.Trace(ctx, "%v is cut off from the store", n.name)
		by := time.Duration(20_000+intn(h, 60_000)) * time.Millisecond
		if kinds[kind] == partition {
			by = 0
		}
		inst.ShiftWall(-by)
		_ = h.Sleep(ctx, span)
		inst.ShiftWall(by)
		n.client.partitioned.Store(false)
		h.Trace(ctx, "%v reaches the store again", n.name)
	case wallForward:
		inst.ShiftWall(time.Duration(intn(h, int(takeoverMargin/time.Millisecond)/2)) * time.Millisecond)
	}
}

// cache is what a cache holds: its commit, and the values at its keys.
type cache struct {
	seq int64
	kv  [5]int64
}

// cacheState of the cache at path, read on a connection of its own, which sees what is committed.
func cacheState(path string) (cache, error) {
	var state cache
	c, err := sqlite.Open(path)
	if err != nil {
		return state, err
	}
	state.seq, err = queryInt(c, "select seq from meta")
	for k := range state.kv {
		if err == nil {
			state.kv[k], err = queryInt(c, "select coalesce((select v from kv where k = ?), 0)", int64(k))
		}
	}
	return state, errors.Join(err, c.Close())
}

// valueAt is the value at key k after commit seq in the log of store, or 0 if no commit up to it
// sets k.
func valueAt(store *memStore, seq, k int64) (int64, error) {
	man, err := store.manifest()
	if err != nil {
		return 0, err
	}
	for name := man.Head; name != ""; {
		data, ok := store.object(name)
		if !ok {
			return 0, fmt.Errorf("no change file %v", name)
		}
		var file changeFile
		if err := json.Unmarshal(data, &file); err != nil {
			return 0, err
		}
		if file.Seq <= seq {
			for _, set := range slices.Backward(file.Sets) {
				if set[0] == k {
					return set[1], nil
				}
			}
		}
		name = file.Prev
	}
	return 0, nil
}

// copyCache at from to to, with its write-ahead log, as files that a crashed process left behind.
// SQLite rebuilds the shared-memory index from the log when it opens them.
func copyCache(from, to string) error {
	for _, suffix := range []string{"", "-wal"} {
		src, err := os.Open(from + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		dst, err := os.Create(to + suffix)
		if err != nil {
			_ = src.Close()
			return err
		}
		_, err = io.Copy(dst, src)
		_ = src.Close()
		if err := errors.Join(err, dst.Close()); err != nil {
			return err
		}
	}
	return nil
}

// verify the log in store against the attempts of the callers, the caches of the nodes that did not
// crash against the log, and the lease against holders that may still hold it.
func (o *outcome) verify(ctx context.Context, h tracer, store *memStore, live []*node, holders map[string]bool) {
	man, err := store.manifest()
	if err != nil {
		o.violate(ctx, h, "manifest: %v", err)
		return
	}
	if man.Holder != "" && !holders[man.Holder] {
		o.violate(ctx, h, "the lease is still held by %v after every instance was closed", man.Holder)
	}

	// The log is followed from its newest commit to its first.
	log := make([]changeFile, man.Seq+1)
	name, seq := man.Head, man.Seq
	for name != "" {
		data, ok := store.object(name)
		if !ok {
			o.violate(ctx, h, "the log has no change file %v for commit %v", name, seq)
			return
		}
		if sum := sha256.Sum256(data); name != "changes/"+hex.EncodeToString(sum[:]) {
			o.violate(ctx, h, "the change file %v is not named by its content", name)
		}
		var file changeFile
		if err := json.Unmarshal(data, &file); err != nil {
			o.violate(ctx, h, "change file %v: %v", name, err)
			return
		}
		if file.Seq != seq || seq <= 0 {
			o.violate(ctx, h, "the change file %v is commit %v, where the log should have commit %v", name, file.Seq, seq)
			return
		}
		log[seq] = file
		name, seq = file.Prev, seq-1
	}
	if seq != 0 {
		o.violate(ctx, h, "the log ends before commit %v, not before the first", seq)
		return
	}

	o.mu.Lock()
	attempts := map[uint64]attempt{}
	for _, a := range o.attempts {
		attempts[a.id] = *a
	}
	o.mu.Unlock()
	for _, file := range log[1:] {
		a, ok := attempts[file.ID]
		switch {
		case !ok:
			o.violate(ctx, h, "the log has commit %v of transaction %x, which no caller made", file.Seq, file.ID)
		case a.state == failed:
			o.violate(ctx, h, "the log has commit %v of transaction %x, whose commit failed", file.Seq, file.ID)
		case a.seq != file.Seq || !slices.Equal(a.sets, file.Sets):
			o.violate(ctx, h, "the log has commit %v of transaction %x with the writes %v, and the caller made commit %v with the writes %v", file.Seq, file.ID, file.Sets, a.seq, a.sets)
		}
	}
	for _, a := range attempts {
		switch a.state {
		case acknowledged:
			if a.seq > man.Seq || log[a.seq].ID != a.id {
				o.violate(ctx, h, "commit %v of transaction %x was acknowledged, and the log does not have it", a.seq, a.id)
			}
		case pending:
			o.cutCommits.Add(1)
		}
	}

	for _, n := range live {
		c, err := sqlite.Open(n.path)
		if err != nil {
			o.violate(ctx, h, "open the cache of %v: %v", n.name, err)
			continue
		}
		o.verifyCache(ctx, h, n.name, c, log)
		if err := c.Close(); err != nil {
			o.violate(ctx, h, "close the cache of %v: %v", n.name, err)
		}
	}
}

// verifyCache c of the node name against the log up to the commit that the cache has.
func (o *outcome) verifyCache(ctx context.Context, h tracer, name string, c *sqlite.Conn, log []changeFile) {
	seq, err := queryInt(c, "select seq from meta")
	if err != nil {
		o.violate(ctx, h, "the cache of %v: %v", name, err)
		return
	}
	if seq >= int64(len(log)) {
		o.violate(ctx, h, "the cache of %v has commit %v, and the log only %v", name, seq, len(log)-1)
		return
	}
	want := map[int64]int64{}
	for _, file := range log[1 : seq+1] {
		for _, set := range file.Sets {
			want[set[0]] = set[1]
		}
	}
	for k := range int64(5) {
		got, err := queryInt(c, "select coalesce((select v from kv where k = ?), 0)", k)
		if err != nil {
			o.violate(ctx, h, "the cache of %v: %v", name, err)
		}
		if got != want[k] {
			o.violate(ctx, h, "the cache of %v has %v at key %v at commit %v, and the log has %v", name, got, k, seq, want[k])
		}
	}
}

// simulation is one run of a workload under the scheduler.
type simulation struct {
	outcome *outcome
	// err is the scheduler's own failure, such as a deadlock or a broken rule.
	err error
	// lines of the trace.
	lines []string
	// draw is the next random number after the run, which differs if the run drew a different number
	// of them.
	draw  uint64
	steps int
}

// hash of the trace and the next random number.
func (s simulation) hash() string {
	sum := sha256.Sum256([]byte(strings.Join(s.lines, "\n") + fmt.Sprint("\n", s.draw)))
	return hex.EncodeToString(sum[:])
}

// order in which the scheduler ran the tasks and from where, without the simulated times and without
// what the tasks traced themselves.
func (s simulation) order() string {
	var order []string
	for _, line := range s.lines {
		if _, step, ok := strings.Cut(line, " "); ok && strings.Contains(step, " resumes from ") {
			order = append(order, step)
		}
	}
	return strings.Join(order, "\n")
}

// violations of the simulation, including the scheduler's own failure.
func (s simulation) violations() []string {
	var violations []string
	if s.err != nil {
		violations = append(violations, "scheduler: "+s.err.Error())
	}
	return append(violations, s.outcome.violations...)
}

// count of the lines of the trace that contain substring.
func (s simulation) count(substring string) int {
	var n int
	for _, line := range s.lines {
		if strings.Contains(line, substring) {
			n++
		}
	}
	return n
}

// simulate the workload w under the scheduler with seed, in a bubble of its own.
func simulate(t *testing.T, seed uint64, w workload) simulation {
	t.Helper()
	s := simulation{outcome: newOutcome()}
	synctest.Test(t, func(t *testing.T) {
		scheduler := sim.New(seed, sim.Options{Jitter: 50 * time.Millisecond, Buggify: sim.Buggify{On: w.buggify}})
		c := cluster{
			world:    scheduler,
			instance: func(name string) tracer { return scheduler.NewInstance(name) },
			dir:      t.TempDir(),
		}
		s.err = scheduler.Run(func(ctx context.Context) {
			w.run(ctx, c, s.outcome)
		})
		s.lines, s.draw, s.steps = scheduler.Lines(), scheduler.Uint64(), scheduler.Steps()
		// The operating system would have closed the files of a crashed process.
		for _, d := range s.outcome.crashed {
			d.closeFiles()
		}
	})
	return s
}

// free runs the workload w with the scheduler off, in a bubble of its own for the fake clock. Every
// instance shares the one host, and the workload moves no clocks and crashes nothing.
func free(t *testing.T, w workload) *outcome {
	t.Helper()
	o := newOutcome()
	synctest.Test(t, func(t *testing.T) {
		r := sim.NewFree(sim.Buggify{On: w.buggify})
		c := cluster{
			world:    r,
			instance: func(string) tracer { return r },
			dir:      t.TempDir(),
		}
		is.NotError(t, r.Run(func(ctx context.Context) {
			w.run(ctx, c, o)
		}))
	})
	return o
}

// seeds to run for the workload w, from 0. The number is n unless the environment variable
// S4QL_SIM_SEEDS sets another, and a quarter of that for a workload whose callers never pause,
// because its runs are several times longer.
func seeds(t *testing.T, n int, w workload) uint64 {
	t.Helper()
	if v := os.Getenv("S4QL_SIM_SEEDS"); v != "" {
		var err error
		n, err = strconv.Atoi(v)
		is.NotError(t, err)
	}
	if w.think == 0 {
		n = max(1, n/4)
	}
	return uint64(n)
}

// firstDifference between two traces, for a failure message.
func firstDifference(a, b []string) string {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return fmt.Sprintf("line %v:\n  first:  %v\n  second: %v", i, a[i], b[i])
		}
	}
	return fmt.Sprintf("one trace has %v lines and the other %v", len(a), len(b))
}

func TestSimulation(t *testing.T) {
	for _, test := range []struct {
		name     string
		workload workload
	}{
		{name: "should give the same trace when a seed is run twice, and no violations, with one instance", workload: singleWorkload},
		{name: "should give the same trace when a seed is run twice, and no violations, with instances that compete, clocks that move, crashes, and buggify", workload: fullWorkload},
		{name: "should give the same trace when a seed is run twice, and no violations, with callers that never pause", workload: hammerWorkload},
	} {
		t.Run(test.name, func(t *testing.T) {
			n := seeds(t, 200, test.workload)
			hashes, orders := map[string]bool{}, map[string]bool{}
			all := sha256.New()
			var steps, differing, buggifies, clockMoves, crashes, timeouts int
			var commits, failedCommits, unknownCommits, cutCommits, rollbacks, reads, failedReads, expiredReads, overtakenReads, stops, restarts int64
			start := time.Now()
			for seed := range n {
				first, second := simulate(t, seed, test.workload), simulate(t, seed, test.workload)
				if first.hash() != second.hash() {
					differing++
					t.Errorf("seed %v gave two different traces, at %v", seed, firstDifference(first.lines, second.lines))
				}
				if v := first.violations(); len(v) > 0 {
					t.Errorf("seed %v: %v", seed, strings.Join(v, "; "))
				}
				hashes[first.hash()] = true
				orders[first.order()] = true
				all.Write([]byte(first.hash()))
				steps += first.steps
				buggifies += first.count(" fires")
				clockMoves += first.count(" monotonic clock") + first.count(" shifts its wall clock")
				crashes += first.count(" crashes")
				timeouts += first.count(" expires")
				o := first.outcome
				commits += o.commits.Load()
				failedCommits += o.failedCommits.Load()
				unknownCommits += o.unknownCommits.Load()
				cutCommits += o.cutCommits.Load()
				rollbacks += o.rollbacks.Load()
				reads += o.reads.Load()
				failedReads += o.failedReads.Load()
				expiredReads += o.expiredReads.Load()
				overtakenReads += o.overtakenReads.Load()
				stops += o.stops.Load()
				restarts += o.restarts.Load()
			}
			elapsed := time.Since(start)
			t.Logf("%v seeds, each run twice: %v pairs differed; %v distinct traces, and %v distinct orders of tasks", n, differing, len(hashes), len(orders))
			t.Logf("in the first run of each seed: %v steps, %v commits, %v failed commits, %v of unknown outcome, %v cut off by a crash, %v rollbacks, %v reads of which %v were overtaken by a commit, %v failed reads of which %v for an expired lease",
				steps, commits, failedCommits, unknownCommits, cutCommits, rollbacks, reads, overtakenReads, failedReads, expiredReads)
			t.Logf("and %v buggify points that fired, %v moves of a clock, %v crashes and %v restarts, %v stops of an instance, %v timeouts that expired", buggifies, clockMoves, crashes, restarts, stops, timeouts)
			t.Logf("%v runs in %v, %.0f runs per second, %.0f steps per second",
				2*n, elapsed.Round(time.Millisecond), float64(2*n)/elapsed.Seconds(), float64(2*steps)/elapsed.Seconds())
			// The hash depends only on the code and the seeds, so it can be compared between machines.
			t.Logf("hash of all traces: %x", all.Sum(nil))
			is.Equal(t, 0, differing)
			if n >= 50 {
				is.True(t, len(orders) > int(n)/2, "the seed hardly changes the order in which tasks run")
				is.True(t, commits > 0 && reads > 0 && failedCommits > 0 && expiredReads > 0 && overtakenReads > 0, "the workload did not reach every kind of result")
				is.True(t, timeouts > 0, "no timeout expired")
				if test.workload.buggify {
					is.True(t, buggifies > 0 && stops > 0, "buggify never fired, or no instance ever stopped")
				}
				if test.workload.buggify && n >= 200 {
					is.True(t, unknownCommits > 0, "no commit had an unknown outcome")
				}
				if test.workload.clocks != 0 {
					is.True(t, clockMoves > 0, "no clock moved")
				}
				if test.workload.crashes > 0 {
					is.True(t, crashes > 0 && restarts == int64(crashes), "no instance crashed, or not every crash was followed by a restart")
				}
			}
		})
	}
}

// plantedBugs that the checks must find. Each holds the workload that finds the bug. A bug that needs
// one ability of the harness also holds a control: the same workload without that ability, with
// which no seed may find it.
var plantedBugs = []struct {
	name     string
	workload workload
	control  *workload
	// symptoms of the bug: every seed that finds anything must find a violation with one of them.
	symptoms []string
}{
	{
		name: "a renewal that swaps the manifest outside the lock",
		workload: func() workload {
			w := singleWorkload
			w.options.bugs.renewOutsideLock = true
			return w
		}(),
		symptoms: []string{"lease lost"},
	},
	{
		name: "a commit that reaches the cache before the manifest",
		workload: func() workload {
			// Without faults, a commit never fails, so the cache is ahead of the manifest only while a
			// commit is on its way, and only a read at that moment finds the bug.
			w := singleWorkload
			w.options.bugs.cacheFirst = true
			w.faults = faults{}
			return w
		}(),
		symptoms: []string{"while the manifest was at"},
	},
	{
		name: "a read that checks the lease against the wall clock only",
		workload: func() workload {
			w := fullWorkload
			w.options.bugs.wallOnly = true
			w.clocks, w.crashes, w.buggify = wallBackward, 0, false
			return w
		}(),
		control: func() *workload {
			w := fullWorkload
			// The same partitions, without the shift of the wall clock.
			w.options.bugs.wallOnly = true
			w.clocks, w.crashes, w.buggify = partition, 0, false
			return &w
		}(),
		symptoms: []string{"after commit"},
	},
	{
		name: "a read that checks the lease against the monotonic clock only",
		workload: func() workload {
			w := fullWorkload
			w.options.bugs.monotonicOnly = true
			w.clocks, w.crashes, w.buggify = stopMonotonic, 0, false
			return w
		}(),
		control: func() *workload {
			w := fullWorkload
			w.options.bugs.monotonicOnly = true
			w.clocks, w.crashes, w.buggify = 0, 0, false
			return &w
		}(),
		symptoms: []string{"after commit"},
	},
	{
		// A lease that is not released is left only by a holder that crashed, or one that could not
		// release it, which the faults of one instance never cause.
		name: "an instance that takes over a lease only after a release",
		workload: func() workload {
			w := singleWorkload
			w.options.bugs.takeoverAfterRelease = true
			w.crashes = 1
			return w
		}(),
		control: func() *workload {
			w := singleWorkload
			w.options.bugs.takeoverAfterRelease = true
			return &w
		}(),
		symptoms: []string{"no instance serves"},
	},
	{
		name: "a commit that ignores a failure of the cache after the manifest swap",
		workload: func() workload {
			w := singleWorkload
			w.options.bugs.ignoreCacheError = true
			w.buggify = true
			return w
		}(),
		control: func() *workload {
			w := singleWorkload
			w.options.bugs.ignoreCacheError = true
			return &w
		}(),
		symptoms: []string{"a read saw", "where the log should have commit", "the cache of", "the log does not have it", "the caller made commit"},
	},
}

func TestSimulation_plantedBugs(t *testing.T) {
	for _, bug := range plantedBugs {
		name := "should find " + bug.name + " with some seed and again on every rerun of it"
		if bug.control != nil {
			name += ", and never without the ability it needs"
		}
		t.Run(name, func(t *testing.T) {
			n := seeds(t, 200, bug.workload)
			var found []uint64
			for seed := range n {
				first := simulate(t, seed, bug.workload)
				if len(first.violations()) == 0 {
					continue
				}
				found = append(found, seed)
				is.True(t, slices.ContainsFunc(first.violations(), func(v string) bool {
					return slices.ContainsFunc(bug.symptoms, func(symptom string) bool { return strings.Contains(v, symptom) })
				}), fmt.Sprintf("seed %v found something else: %v", seed, first.violations()))
				if len(found) > 3 {
					continue
				}
				// The same seed finds the same violations at the same place in the same trace.
				for range 5 {
					again := simulate(t, seed, bug.workload)
					is.Equal(t, first.hash(), again.hash())
					is.EqualSlice(t, first.violations(), again.violations())
				}
				if len(found) == 1 {
					at := max(0, slices.IndexFunc(first.lines, func(line string) bool { return strings.Contains(line, "VIOLATION") }))
					t.Logf("seed %v, rerun 5 times with the same trace of %v lines; the trace up to its first violation:\n  %v",
						seed, len(first.lines), strings.Join(first.lines[max(0, at-25):at+1], "\n  "))
				}
			}
			t.Logf("%v of %v seeds found the bug; the first are %v", len(found), n, found[:min(len(found), 10)])
			if bug.control != nil {
				var controlFound int
				for seed := range n {
					if v := simulate(t, seed, *bug.control).violations(); len(v) > 0 {
						controlFound++
						t.Errorf("seed %v found the bug without the ability it needs: %v", seed, v)
					}
				}
				t.Logf("%v of %v seeds found the bug without the ability it needs", controlFound, n)
			}
			if n >= 200 {
				is.True(t, len(found) > 0, "no seed found the bug")
			}
		})
	}
}

// TestSimulationSeed runs the one seed that the environment variable S4QL_SIM_SEED names, with the
// workload that S4QL_SIM_WORKLOAD names (single, the default, full, or hammer), and logs its whole
// trace. The environment variable S4QL_SIM_BUG can name a planted bug by its number in
// [plantedBugs], from 0, to run its workload instead.
func TestSimulationSeed(t *testing.T) {
	v := os.Getenv("S4QL_SIM_SEED")
	if v == "" {
		t.Skip("S4QL_SIM_SEED is not set")
	}
	seed, err := strconv.ParseUint(v, 10, 64)
	is.NotError(t, err)

	w := singleWorkload
	switch name := os.Getenv("S4QL_SIM_WORKLOAD"); name {
	case "", "single":
	case "full":
		w = fullWorkload
	case "hammer":
		w = hammerWorkload
	default:
		t.Fatalf("no workload is named %v", name)
	}
	if v := os.Getenv("S4QL_SIM_BUG"); v != "" {
		i, err := strconv.Atoi(v)
		is.NotError(t, err)
		w = plantedBugs[i].workload
	}

	s := simulate(t, seed, w)
	t.Logf("seed %v: %v steps, hash %v, trace:\n%v", seed, s.steps, s.hash(), strings.Join(s.lines, "\n"))
	is.EqualSlice(t, nil, s.violations())
}

// contend runs one writer and eight readers as tasks of h, each on its own connection to the
// database at path and in a loop without pauses, and returns how often the writer got SQLITE_BUSY
// when it began a transaction. No connection has a busy timeout.
func contend(ctx context.Context, t *testing.T, h host.Host, path string) int64 {
	t.Helper()
	setup := openWAL(t, path, 0)
	is.NotError(t, setup.Exec("create table t1 (k integer primary key, v integer not null) strict"))
	is.NotError(t, setup.Exec("insert into t1 (k, v) values (1, 0)"))

	var stop atomic.Bool
	var busyBegins atomic.Int64
	var tasks []host.Task
	for i := range 8 {
		c := openWAL(t, path, 0)
		tasks = append(tasks, h.Go(ctx, fmt.Sprint("reader-", i), func(ctx context.Context) {
			for !stop.Load() {
				is.NotError(t, c.Exec("begin"))
				h.Yield(ctx, "reader: after begin")
				is.NotError(t, c.Exec("select v from t1 where k = 1"))
				h.Yield(ctx, "reader: after select")
				is.NotError(t, c.Exec("commit"))
				h.Yield(ctx, "reader: after commit")
			}
		}))
	}
	c := openWAL(t, path, 0)
	tasks = append(tasks, h.Go(ctx, "writer", func(ctx context.Context) {
		defer stop.Store(true)
		for range 1000 {
			for {
				err := c.Exec("begin immediate")
				if err == nil {
					break
				}
				is.True(t, busy(err), fmt.Sprint(err))
				busyBegins.Add(1)
			}
			is.NotError(t, c.Exec("update t1 set v = v + 1 where k = 1"))
			h.Yield(ctx, "writer: after update")
			is.NotError(t, c.Exec("commit"))
			h.Yield(ctx, "writer: after commit")
		}
	}))
	for _, task := range tasks {
		is.NotError(t, task.Wait(ctx))
	}
	return busyBegins.Load()
}

// TestSchedulerWithSQLiteLocks checks that SQLite never finds a lock taken while one task at a time
// runs, with a workload that does find one with the scheduler off.
func TestSchedulerWithSQLiteLocks(t *testing.T) {
	t.Run("should never give SQLITE_BUSY to a writer among readers when one task runs at a time", func(t *testing.T) {
		for seed := range uint64(10) {
			synctest.Test(t, func(t *testing.T) {
				s := sim.New(seed, sim.Options{})
				var busyBegins int64
				is.NotError(t, s.Run(func(ctx context.Context) {
					busyBegins = contend(ctx, t, s, filepath.Join(t.TempDir(), "test.db"))
				}))
				is.Equal(t, 0, busyBegins)
			})
		}
	})

	// This is the control for the test above. The number varies and is sometimes zero, so it is only
	// logged.
	t.Run("should log how often the same writer gets SQLITE_BUSY with the scheduler off", func(t *testing.T) {
		var busyBegins int64
		for range 10 {
			synctest.Test(t, func(t *testing.T) {
				r := sim.NewFree(sim.Buggify{})
				is.NotError(t, r.Run(func(ctx context.Context) {
					busyBegins += contend(ctx, t, r, filepath.Join(t.TempDir(), "test.db"))
				}))
			})
		}
		t.Logf("with the scheduler off, the writer got SQLITE_BUSY %v times in 10000 transactions", busyBegins)
	})
}

// TestSimulationSpeed logs how fast simulated runs are on this machine: the scheduler with tasks
// that only yield, and the workloads. It is a test and not a benchmark because a bubble needs a
// [testing.T]. It only runs when the environment variable S4QL_SIM_SPEED is set, because it asserts
// nothing that the other tests do not.
func TestSimulationSpeed(t *testing.T) {
	if os.Getenv("S4QL_SIM_SPEED") == "" {
		t.Skip("S4QL_SIM_SPEED is not set")
	}
	t.Run("should log the speed of the scheduler with eight tasks that only yield", func(t *testing.T) {
		start := time.Now()
		var steps int
		for seed := range uint64(20) {
			s, err := schedule(t, seed, sim.Options{Jitter: time.Millisecond}, func(ctx context.Context, s *sim.Scheduler) {
				for i := range 8 {
					s.Go(ctx, fmt.Sprint("worker-", i), func(ctx context.Context) {
						for range 2000 {
							s.Yield(ctx, "between")
						}
					})
				}
			})
			is.NotError(t, err)
			steps += s.Steps()
		}
		elapsed := time.Since(start)
		t.Logf("%v steps in %v, %.0f steps per second", steps, elapsed.Round(time.Millisecond), float64(steps)/elapsed.Seconds())
	})

	syncOff := singleWorkload
	syncOff.options.synchronousOff = true
	for _, test := range []struct {
		name     string
		workload workload
	}{
		{name: "should log the speed of the workload with one instance", workload: singleWorkload},
		{name: "should log the speed of the workload with one instance without syncing the cache to disk", workload: syncOff},
		{name: "should log the speed of the workload with everything", workload: fullWorkload},
	} {
		t.Run(test.name, func(t *testing.T) {
			n := seeds(t, 200, test.workload)
			start := time.Now()
			var steps int
			for seed := range n {
				s := simulate(t, seed, test.workload)
				is.EqualSlice(t, nil, s.violations())
				steps += s.steps
			}
			elapsed := time.Since(start)
			t.Logf("%v runs in %v, %.0f runs per second, %.0f steps per second, %.0f steps per run",
				n, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds(), float64(steps)/elapsed.Seconds(), float64(steps)/float64(n))
		})
	}
}

func TestFreeRunning(t *testing.T) {
	for _, test := range []struct {
		name     string
		workload workload
	}{
		{name: "should run the workload with one instance with the scheduler off and no violations", workload: singleWorkload},
		{name: "should run two instances that compete for the lease, with buggify, with the scheduler off and no violations", workload: fullWorkload},
		{name: "should run callers that never pause with the scheduler off and no violations", workload: hammerWorkload},
	} {
		t.Run(test.name, func(t *testing.T) {
			n := seeds(t, 200, test.workload)
			var commits, failedCommits, reads, failedReads, expiredReads, stops int64
			start := time.Now()
			for seed := range n {
				o := free(t, test.workload)
				if len(o.violations) > 0 {
					t.Errorf("run %v: %v", seed, strings.Join(o.violations, "; "))
				}
				commits += o.commits.Load()
				failedCommits += o.failedCommits.Load()
				reads += o.reads.Load()
				failedReads += o.failedReads.Load()
				expiredReads += o.expiredReads.Load()
				stops += o.stops.Load()
			}
			elapsed := time.Since(start)
			t.Logf("%v free runs in %v, %.0f runs per second: %v commits, %v failed commits, %v reads, %v failed reads of which %v for an expired lease, %v stops",
				n, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds(), commits, failedCommits, reads, failedReads, expiredReads, stops)
		})
	}
}

// TestPlantedDataRace shows what the race detector reports in each mode for a counter that reads
// change without synchronisation. It must be run with -race, and only does anything when the
// environment variable S4QL_SIM_PLANT_RACE is "scheduler" or "free", because a reported race fails
// the test.
func TestPlantedDataRace(t *testing.T) {
	w := hammerWorkload
	w.options.bugs.unlockedCounter = true

	switch mode := os.Getenv("S4QL_SIM_PLANT_RACE"); mode {
	case "scheduler":
		for seed := range uint64(50) {
			is.EqualSlice(t, nil, simulate(t, seed, w).violations())
		}
	case "free":
		for range 50 {
			is.EqualSlice(t, nil, free(t, w).violations)
		}
	default:
		t.Skip("S4QL_SIM_PLANT_RACE is not set")
	}
}
