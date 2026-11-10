package sim_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

	"maragu.dev/s4ql/internal/sim"
)

// workload that callers run against a [toyDB].
type workload struct {
	// callers is the number of tasks that use the database at the same time.
	callers int
	// ops is the number of transactions that each caller runs.
	ops int
	// think is the longest simulated time that a caller waits before each transaction.
	think time.Duration
	// latency is the unit of the simulated store's latency.
	latency time.Duration
	options toyOptions
	faults  faults
}

// defaultWorkload has enough callers, simulated time, and faults for commits, renewals, reads, and
// lease timeouts to overlap.
var defaultWorkload = workload{
	callers: 4,
	ops:     12,
	think:   15 * time.Second,
	latency: time.Millisecond,
	options: toyOptions{readers: 2, busyTimeout: 1000},
	faults:  faults{errors: 30, lostAcks: 30, delays: 10},
}

// hammerWorkload has callers that never wait between transactions, so that with the scheduler off
// they are in SQLite and in the toy library's own code at the same time.
var hammerWorkload = workload{
	callers: 8,
	ops:     40,
	latency: time.Millisecond,
	options: toyOptions{readers: 4, busyTimeout: 1000},
	faults:  faults{errors: 30, lostAcks: 30, delays: 10},
}

// outcome of running a [workload].
type outcome struct {
	commits, failedCommits, rollbacks, reads, failedReads, expiredReads atomic.Int64
	// overtakenReads are reads that ended when the manifest had a commit that their snapshot did not,
	// so a commit was in progress or finished while they ran.
	overtakenReads atomic.Int64

	mu sync.Mutex
	// violations of the invariants that the workload checks.
	violations []string
	// acked holds the writes of every acknowledged commit, by its number, as the callers made them.
	acked map[int64][][2]int64
	// lines of the trace of a free run.
	lines []string
}

// violate records a violation, also in the trace of e.
func (o *outcome) violate(ctx context.Context, e tracer, format string, args ...any) {
	e.Trace(ctx, "VIOLATION: "+format, args...)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.violations = append(o.violations, fmt.Sprintf(format, args...))
}

// expected reports whether err is one that the faults of the simulated store explain.
func expected(err error) bool {
	return errors.Is(err, errInjected) || errors.Is(err, errSwapFailed) || errors.Is(err, errLeaseExpired)
}

// run the workload w as the task of ctx, with the cache at path, and record what happened in o. It
// checks these invariants:
//
//   - A read that starts after a commit is acknowledged sees that commit.
//   - A read never sees a commit that the manifest does not have, so the cache is never ahead.
//   - The instance never loses its lease, because no other instance exists.
//   - At the end, the log holds exactly the acknowledged commits with the writes that the callers
//     made, and the cache holds what the log says.
//   - After a close that reports no error, the lease is released.
func (w workload) run(ctx context.Context, e tracer, path string, o *outcome) {
	store := newMemStore(e, w.faults, w.latency)

	d, err := openToy(ctx, e, store, path, w.options)
	if err != nil {
		o.violate(ctx, e, "open: %v", err)
		return
	}

	// acked is the highest commit that a caller knows to be acknowledged.
	var acked atomic.Int64

	write := func(ctx context.Context) {
		tx, err := d.Begin(ctx)
		if err != nil {
			o.violate(ctx, e, "begin: %v", err)
			return
		}
		var sets [][2]int64
		for range 1 + intn(e, 3) {
			k := int64(intn(e, 5))
			if err := tx.Set(k); err != nil {
				o.violate(ctx, e, "set: %v", err)
			}
			sets = append(sets, [2]int64{k, tx.seq})
			e.Trace(ctx, "set %v in transaction %v", k, tx.seq)
			e.Yield(ctx, "caller: between statements")
		}
		if intn(e, 10) == 0 {
			if err := tx.Rollback(ctx); err != nil {
				o.violate(ctx, e, "rollback: %v", err)
			}
			o.rollbacks.Add(1)
			e.Trace(ctx, "rollback of transaction %v", tx.seq)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			o.failedCommits.Add(1)
			e.Trace(ctx, "commit of transaction %v: %v", tx.seq, err)
			if !expected(err) {
				o.violate(ctx, e, "commit of transaction %v: %v", tx.seq, err)
			}
			return
		}
		o.mu.Lock()
		o.acked[tx.seq] = sets
		o.mu.Unlock()
		for {
			high := acked.Load()
			if tx.seq <= high || acked.CompareAndSwap(high, tx.seq) {
				break
			}
		}
		o.commits.Add(1)
		e.Trace(ctx, "commit of transaction %v: acknowledged", tx.seq)
	}

	read := func(ctx context.Context) {
		before := acked.Load()
		k := int64(intn(e, 5))
		seq, v, err := d.Read(ctx, k)
		if err != nil {
			o.failedReads.Add(1)
			if errors.Is(err, errLeaseExpired) {
				o.expiredReads.Add(1)
			}
			e.Trace(ctx, "read of %v: %v", k, err)
			if !expected(err) {
				o.violate(ctx, e, "read of %v: %v", k, err)
			}
			return
		}
		man, err := store.manifest()
		if err != nil {
			o.violate(ctx, e, "manifest: %v", err)
		}
		o.reads.Add(1)
		if man.Seq > seq {
			o.overtakenReads.Add(1)
		}
		e.Trace(ctx, "read of %v: %v as of commit %v", k, v, seq)
		if seq < before {
			o.violate(ctx, e, "a read saw commit %v after commit %v was acknowledged", seq, before)
		}
		if seq > man.Seq {
			o.violate(ctx, e, "a read saw commit %v while the manifest was at %v", seq, man.Seq)
		}
		if v > seq {
			o.violate(ctx, e, "a read saw the value of commit %v in the snapshot of commit %v", v, seq)
		}
	}

	var tasks []sim.Task
	for i := range w.callers {
		tasks = append(tasks, e.Go(ctx, fmt.Sprint("caller-", i), func(ctx context.Context) {
			for range w.ops {
				if w.think > 0 {
					_ = e.Sleep(ctx, time.Duration(intn(e, int(w.think/time.Millisecond)))*time.Millisecond)
				}
				if intn(e, 100) < 35 {
					write(ctx)
				} else {
					read(ctx)
				}
			}
		}))
	}
	for _, task := range tasks {
		if err := task.Wait(ctx); err != nil {
			o.violate(ctx, e, "wait: %v", err)
		}
	}

	if d.stopErr() == nil {
		o.verify(ctx, e, d, store)
	}

	// The renewal task runs until the close, so the instance can still stop after the callers are done.
	err = d.Close(ctx)
	switch cause := d.stopErr(); {
	case !errors.Is(cause, errClosed):
		o.violate(ctx, e, "the instance stopped: %v", cause)
	case err != nil && !expected(err):
		o.violate(ctx, e, "close: %v", err)
	case err == nil:
		man, err := store.manifest()
		if err != nil {
			o.violate(ctx, e, "manifest: %v", err)
		}
		if man.Holder != "" {
			o.violate(ctx, e, "the lease is still held by %v after the close", man.Holder)
		}
	}
}

// verify that the log in store holds exactly the acknowledged commits with the writes that the
// callers made, and that the cache of d holds what the log says.
func (o *outcome) verify(ctx context.Context, e tracer, d *toyDB, store *memStore) {
	tx, err := d.Begin(ctx)
	if err != nil {
		o.violate(ctx, e, "begin at the end: %v", err)
		return
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil {
			o.violate(ctx, e, "rollback at the end: %v", err)
		}
	}()

	man, err := store.manifest()
	if err != nil {
		o.violate(ctx, e, "manifest: %v", err)
	}
	o.mu.Lock()
	acked := maps.Clone(o.acked)
	o.mu.Unlock()
	if cached := tx.seq - 1; man.Seq != cached || man.Seq != int64(len(acked)) {
		o.violate(ctx, e, "%v commits were acknowledged, the manifest has %v, and the cache has %v", len(acked), man.Seq, cached)
	}

	// The log is followed from its newest commit to its first, so the first value found for a key is
	// the one that the database must have.
	want := map[int64]int64{}
	name, seq := man.Head, man.Seq
	for name != "" {
		data, ok := store.object(name)
		if !ok {
			o.violate(ctx, e, "the log has no change file %v for commit %v", name, seq)
			return
		}
		if sum := sha256.Sum256(data); name != "changes/"+hex.EncodeToString(sum[:]) {
			o.violate(ctx, e, "the change file %v is not named by its content", name)
		}
		var file changeFile
		if err := json.Unmarshal(data, &file); err != nil {
			o.violate(ctx, e, "change file %v: %v", name, err)
			return
		}
		if file.Seq != seq {
			o.violate(ctx, e, "the change file %v is commit %v, where the log should have commit %v", name, file.Seq, seq)
		}
		if !slices.Equal(file.Sets, acked[file.Seq]) {
			o.violate(ctx, e, "the change file of commit %v has the writes %v, and the acknowledged writes are %v", file.Seq, file.Sets, acked[file.Seq])
		}
		for _, set := range slices.Backward(file.Sets) {
			if _, ok := want[set[0]]; !ok {
				want[set[0]] = set[1]
			}
		}
		name, seq = file.Prev, seq-1
	}
	if seq != 0 {
		o.violate(ctx, e, "the log ends before commit %v, not before the first", seq)
	}

	for k := range int64(5) {
		got, err := tx.Get(k)
		if err != nil {
			o.violate(ctx, e, "get: %v", err)
		}
		if got != want[k] {
			o.violate(ctx, e, "the cache has %v at key %v, and the log has %v", got, k, want[k])
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

// simulate the workload w under the scheduler with seed, in a bubble of its own.
func simulate(t *testing.T, seed uint64, w workload) simulation {
	t.Helper()
	s := simulation{outcome: &outcome{acked: map[int64][][2]int64{}}}
	synctest.Test(t, func(t *testing.T) {
		scheduler := sim.New(seed, sim.Options{Jitter: 50 * time.Millisecond})
		path := filepath.Join(t.TempDir(), "cache.db")
		s.err = scheduler.Run(func(ctx context.Context) {
			w.run(ctx, scheduler, path, s.outcome)
		})
		s.lines, s.draw, s.steps = scheduler.Lines(), scheduler.Uint64(), scheduler.Steps()
	})
	return s
}

// free runs the workload w with the scheduler off and with seed, in a bubble of its own for the fake
// clock.
func free(t *testing.T, seed uint64, w workload) *outcome {
	t.Helper()
	o := &outcome{acked: map[int64][][2]int64{}}
	synctest.Test(t, func(t *testing.T) {
		r := sim.NewFree(seed)
		path := filepath.Join(t.TempDir(), "cache.db")
		is.NotError(t, r.Run(func(ctx context.Context) {
			w.run(ctx, r, path, o)
		}))
		o.lines = r.Lines()
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
		{name: "should give the same trace when a seed is run twice, and no violations", workload: defaultWorkload},
		{name: "should give the same trace when a seed is run twice, and no violations, also with callers that never pause", workload: hammerWorkload},
	} {
		t.Run(test.name, func(t *testing.T) {
			n := seeds(t, 200, test.workload)
			hashes, orders := map[string]bool{}, map[string]bool{}
			all := sha256.New()
			var steps, differing int
			var commits, failedCommits, rollbacks, reads, failedReads, expiredReads, overtakenReads int64
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
				o := first.outcome
				commits += o.commits.Load()
				failedCommits += o.failedCommits.Load()
				rollbacks += o.rollbacks.Load()
				reads += o.reads.Load()
				failedReads += o.failedReads.Load()
				expiredReads += o.expiredReads.Load()
				overtakenReads += o.overtakenReads.Load()
			}
			elapsed := time.Since(start)
			t.Logf("%v seeds, each run twice: %v pairs differed; %v distinct traces, and %v distinct orders of tasks", n, differing, len(hashes), len(orders))
			t.Logf("in the first run of each seed: %v steps, %v commits, %v failed commits, %v rollbacks, %v reads of which %v were overtaken by a commit, %v failed reads of which %v for an expired lease",
				steps, commits, failedCommits, rollbacks, reads, overtakenReads, failedReads, expiredReads)
			t.Logf("%v runs in %v, %.0f runs per second, %.0f steps per second",
				2*n, elapsed.Round(time.Millisecond), float64(2*n)/elapsed.Seconds(), float64(2*steps)/elapsed.Seconds())
			// The hash depends only on the code and the seeds, so it can be compared between machines.
			t.Logf("hash of all traces: %x", all.Sum(nil))
			is.Equal(t, 0, differing)
			if n >= 50 {
				is.True(t, len(orders) > int(n)/2, "the seed hardly changes the order in which tasks run")
				is.True(t, commits > 0 && reads > 0 && failedCommits > 0 && expiredReads > 0 && overtakenReads > 0, "the workload did not reach every kind of result")
			}
		})
	}

	renewOutsideLock := defaultWorkload
	renewOutsideLock.options.bugs.renewOutsideLock = true
	// Without faults, a commit never fails, so the cache is ahead of the manifest only for as long as
	// a commit is on its way, and only a read at that moment finds the bug.
	cacheFirst := defaultWorkload
	cacheFirst.options.bugs.cacheFirst = true
	cacheFirst.faults = faults{}
	for _, test := range []struct {
		name     string
		workload workload
		// symptom of the bug: every seed that finds anything must find a violation with it.
		symptom string
	}{
		{
			name:     "should find a renewal that swaps the manifest outside the lock, with some seed and again on every rerun of it",
			workload: renewOutsideLock,
			symptom:  "lease lost",
		},
		{
			name:     "should find a commit that reaches the cache before the manifest, with some seed and again on every rerun of it",
			workload: cacheFirst,
			symptom:  "while the manifest was at",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			n := seeds(t, 200, test.workload)
			var found []uint64
			for seed := range n {
				first := simulate(t, seed, test.workload)
				if len(first.violations()) == 0 {
					continue
				}
				found = append(found, seed)
				is.True(t, slices.ContainsFunc(first.violations(), func(v string) bool { return strings.Contains(v, test.symptom) }),
					fmt.Sprintf("seed %v found something else: %v", seed, first.violations()))
				if len(found) > 3 {
					continue
				}
				// The same seed finds the same violations at the same place in the same trace.
				for range 5 {
					again := simulate(t, seed, test.workload)
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
			if n >= 200 {
				is.True(t, len(found) > 0, "no seed found the bug")
			}
		})
	}
}

// TestSimulationSeed runs the one seed that the environment variable S4QL_SIM_SEED names, with the
// default workload, and logs its whole trace. The environment variable S4QL_SIM_BUG can name a
// planted bug, renewOutsideLock or cacheFirst.
func TestSimulationSeed(t *testing.T) {
	v := os.Getenv("S4QL_SIM_SEED")
	if v == "" {
		t.Skip("S4QL_SIM_SEED is not set")
	}
	seed, err := strconv.ParseUint(v, 10, 64)
	is.NotError(t, err)

	w := defaultWorkload
	switch bug := os.Getenv("S4QL_SIM_BUG"); bug {
	case "":
	case "renewOutsideLock":
		w.options.bugs.renewOutsideLock = true
	case "cacheFirst":
		w.options.bugs.cacheFirst = true
	default:
		t.Fatalf("no planted bug is named %v", bug)
	}

	s := simulate(t, seed, w)
	t.Logf("seed %v: %v steps, hash %v, trace:\n%v", seed, s.steps, s.hash(), strings.Join(s.lines, "\n"))
	is.EqualSlice(t, nil, s.violations())
}

// contend runs one writer and eight readers as tasks of e, each on its own connection to the
// database at path and in a loop without pauses, and returns how often the writer got SQLITE_BUSY
// when it began a transaction. No connection has a busy timeout.
func contend(ctx context.Context, t *testing.T, e env, path string) int64 {
	t.Helper()
	setup := openWAL(t, path, 0)
	is.NotError(t, setup.Exec("create table t1 (k integer primary key, v integer not null) strict"))
	is.NotError(t, setup.Exec("insert into t1 (k, v) values (1, 0)"))

	var stop atomic.Bool
	var busyBegins atomic.Int64
	var tasks []sim.Task
	for i := range 8 {
		c := openWAL(t, path, 0)
		tasks = append(tasks, e.Go(ctx, fmt.Sprint("reader-", i), func(ctx context.Context) {
			for !stop.Load() {
				is.NotError(t, c.Exec("begin"))
				e.Yield(ctx, "reader: after begin")
				is.NotError(t, c.Exec("select v from t1 where k = 1"))
				e.Yield(ctx, "reader: after select")
				is.NotError(t, c.Exec("commit"))
				e.Yield(ctx, "reader: after commit")
			}
		}))
	}
	c := openWAL(t, path, 0)
	tasks = append(tasks, e.Go(ctx, "writer", func(ctx context.Context) {
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
			e.Yield(ctx, "writer: after update")
			is.NotError(t, c.Exec("commit"))
			e.Yield(ctx, "writer: after commit")
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
		for seed := range uint64(10) {
			synctest.Test(t, func(t *testing.T) {
				r := sim.NewFree(seed)
				is.NotError(t, r.Run(func(ctx context.Context) {
					busyBegins += contend(ctx, t, r, filepath.Join(t.TempDir(), "test.db"))
				}))
			})
		}
		t.Logf("with the scheduler off, the writer got SQLITE_BUSY %v times in 10000 transactions", busyBegins)
	})
}

// TestSimulationSpeed logs how fast simulated runs are on this machine: the scheduler with tasks
// that only yield, the toy library, and the toy library when SQLite does not sync the cache to disk.
// It is a test and not a benchmark because a bubble needs a [testing.T].
func TestSimulationSpeed(t *testing.T) {
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

	syncOff := defaultWorkload
	syncOff.options.synchronousOff = true
	for _, test := range []struct {
		name     string
		workload workload
	}{
		{name: "should log the speed of the toy library", workload: defaultWorkload},
		{name: "should log the speed of the toy library without syncing the cache to disk", workload: syncOff},
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
		{name: "should run the same workload with the scheduler off and no violations", workload: defaultWorkload},
		{name: "should run callers that never pause with the scheduler off and no violations", workload: hammerWorkload},
	} {
		t.Run(test.name, func(t *testing.T) {
			n := seeds(t, 200, test.workload)
			var commits, failedCommits, reads, failedReads, expiredReads int64
			start := time.Now()
			for seed := range n {
				o := free(t, seed, test.workload)
				if len(o.violations) > 0 {
					t.Errorf("seed %v: %v", seed, strings.Join(o.violations, "; "))
				}
				commits += o.commits.Load()
				failedCommits += o.failedCommits.Load()
				reads += o.reads.Load()
				failedReads += o.failedReads.Load()
				expiredReads += o.expiredReads.Load()
			}
			elapsed := time.Since(start)
			t.Logf("%v free runs in %v, %.0f runs per second: %v commits, %v failed commits, %v reads, %v failed reads of which %v for an expired lease",
				n, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds(), commits, failedCommits, reads, failedReads, expiredReads)
		})
	}

	// The fake clock orders most of a run with the scheduler off, so a seed often gives the same run
	// again. How often depends on the workload and on chance, so these two only log it.
	t.Run("should log how often a seed gives two different traces with the scheduler off", func(t *testing.T) {
		for _, test := range []struct {
			name     string
			workload workload
		}{
			{name: "callers that pause", workload: defaultWorkload},
			{name: "callers that never pause", workload: hammerWorkload},
		} {
			n := seeds(t, 200, test.workload)
			var differing int
			for seed := range n {
				first, second := free(t, seed, test.workload), free(t, seed, test.workload)
				if !slices.Equal(first.lines, second.lines) {
					differing++
				}
			}
			t.Logf("with %v, %v of %v seeds gave two different traces when run twice with the scheduler off", test.name, differing, n)
		}
	})

	t.Run("should log how often the planted renewal bug is found, and found again with the same seed, with the scheduler off", func(t *testing.T) {
		w := defaultWorkload
		w.options.bugs.renewOutsideLock = true
		n := seeds(t, 200, w)
		var found, again int
		for seed := range n {
			if len(free(t, seed, w).violations) == 0 {
				continue
			}
			found++
			if len(free(t, seed, w).violations) > 0 {
				again++
			}
		}
		t.Logf("%v of %v free runs found the bug; run again with the same seeds, %v of those %v found it", found, n, again, found)
	})
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
		for seed := range uint64(50) {
			is.EqualSlice(t, nil, free(t, seed, w).violations)
		}
	default:
		t.Skip("S4QL_SIM_PLANT_RACE is not set")
	}
}
