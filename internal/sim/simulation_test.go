package sim_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

// outcome of running a [workload].
type outcome struct {
	commits, failedCommits, rollbacks, reads, failedReads, expiredReads atomic.Int64
	// overtakenReads are reads that ended when the manifest had a commit that their snapshot did not,
	// so a commit was in progress or finished while they ran.
	overtakenReads atomic.Int64

	mu sync.Mutex
	// violations of the invariants that the workload checks.
	violations []string
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

// run the workload w as the task of ctx, with the cache at path, and check the invariants:
//
//   - A read that starts after a commit is acknowledged sees that commit.
//   - A read never sees a commit that the manifest does not have, so the cache is never ahead.
//   - The instance never loses its lease, because no other instance exists.
//   - At the end, the manifest and the cache have exactly the acknowledged commits.
func (w workload) run(ctx context.Context, e tracer, path string) *outcome {
	o := &outcome{}
	store := newMemStore(e, w.faults, w.latency)

	d, err := openToy(ctx, e, store, path, w.options)
	if err != nil {
		o.violate(ctx, e, "open: %v", err)
		return o
	}

	// acked is the highest commit that a caller knows to be acknowledged.
	var acked atomic.Int64

	write := func(ctx context.Context) {
		tx, err := d.Begin(ctx)
		if err != nil {
			o.violate(ctx, e, "begin: %v", err)
			return
		}
		for range 1 + intn(e, 3) {
			k := int64(intn(e, 5))
			if err := tx.Set(k); err != nil {
				o.violate(ctx, e, "set: %v", err)
			}
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

	if err := d.stopErr(); err != nil {
		o.violate(ctx, e, "the instance stopped: %v", err)
	} else {
		tx, err := d.Begin(ctx)
		if err != nil {
			o.violate(ctx, e, "begin at the end: %v", err)
		} else {
			man, err := store.manifest()
			if err != nil {
				o.violate(ctx, e, "manifest: %v", err)
			}
			if cached := tx.seq - 1; man.Seq != cached || man.Seq != o.commits.Load() {
				o.violate(ctx, e, "%v commits were acknowledged, the manifest has %v, and the cache has %v", o.commits.Load(), man.Seq, cached)
			}
			if err := tx.Rollback(ctx); err != nil {
				o.violate(ctx, e, "rollback at the end: %v", err)
			}
		}
	}
	if err := d.Close(ctx); err != nil {
		o.violate(ctx, e, "close: %v", err)
	}
	return o
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

// simulate the workload w under the scheduler with seed, in a bubble of its own.
func simulate(t *testing.T, seed uint64, w workload) simulation {
	t.Helper()
	var s simulation
	synctest.Test(t, func(t *testing.T) {
		scheduler := sim.New(seed, sim.Options{Jitter: 50 * time.Millisecond})
		path := filepath.Join(t.TempDir(), "cache.db")
		s.err = scheduler.Run(func(ctx context.Context) {
			s.outcome = w.run(ctx, scheduler, path)
		})
		s.lines, s.draw, s.steps = scheduler.Lines(), scheduler.Uint64(), scheduler.Steps()
	})
	return s
}

// violations of the simulation, including the scheduler's own failure.
func (s simulation) violations() []string {
	var violations []string
	if s.err != nil {
		violations = append(violations, "scheduler: "+s.err.Error())
	}
	if s.outcome != nil {
		violations = append(violations, s.outcome.violations...)
	}
	return violations
}

// seeds to run for the workload w, which is n unless the environment variable S4QL_SIM_SEEDS sets
// another number, and a quarter of that for a workload whose callers never pause, because its runs
// are several times longer.
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
		{
			name:     "should give the same trace when a seed is run twice, a different trace for every seed, and no violations",
			workload: defaultWorkload,
		},
		{
			name:     "should give the same trace when a seed is run twice, also with callers that never pause",
			workload: hammerWorkload,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			n := seeds(t, 200, test.workload)
			hashes := map[string]bool{}
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
			t.Logf("%v seeds, each run twice: %v pairs differed, %v distinct traces", n, differing, len(hashes))
			t.Logf("in the first run of each seed: %v steps, %v commits, %v failed commits, %v rollbacks, %v reads of which %v were overtaken by a commit, %v failed reads of which %v for an expired lease",
				steps, commits, failedCommits, rollbacks, reads, overtakenReads, failedReads, expiredReads)
			t.Logf("%v runs in %v, %.0f runs per second, %.0f steps per second",
				2*n, elapsed.Round(time.Millisecond), float64(2*n)/elapsed.Seconds(), float64(2*steps)/elapsed.Seconds())
			// The same on every run of the test, on every machine, as long as the code is the same.
			t.Logf("hash of all traces: %x", all.Sum(nil))
			is.Equal(t, 0, differing)
			is.Equal(t, int(n), len(hashes))
			is.True(t, commits > 0 && reads > 0 && failedCommits > 0 && expiredReads > 0 && overtakenReads > 0, "the workload did not reach every kind of result")
		})
	}

	for _, test := range []struct {
		name string
		bugs bugs
		// symptoms of the bug: every seed that finds anything must find a violation with one of these.
		symptoms []string
	}{
		{
			name:     "should find a renewal that swaps the manifest outside the lock, with some seed and again on every rerun of it",
			bugs:     bugs{renewOutsideLock: true},
			symptoms: []string{"lease lost"},
		},
		{
			name:     "should find a commit that reaches the cache before the manifest, with some seed and again on every rerun of it",
			bugs:     bugs{cacheFirst: true},
			symptoms: []string{"while the manifest was at", "commits were acknowledged, the manifest has"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := defaultWorkload
			w.options.bugs = test.bugs

			n := seeds(t, 200, w)
			var found []uint64
			for seed := range n {
				first := simulate(t, seed, w)
				if len(first.violations()) == 0 {
					continue
				}
				found = append(found, seed)
				is.True(t, slices.ContainsFunc(first.violations(), func(v string) bool {
					return slices.ContainsFunc(test.symptoms, func(symptom string) bool { return strings.Contains(v, symptom) })
				}), fmt.Sprintf("seed %v found something else: %v", seed, first.violations()))
				if len(found) > 3 {
					continue
				}
				// The same seed finds the same violations at the same place in the same trace.
				for range 5 {
					again := simulate(t, seed, w)
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
			is.True(t, len(found) > 0, "no seed found the bug")
		})
	}
}

// TestSimulationSpeed logs how fast simulated runs are on this machine, and how much of that is the
// scheduler itself and how much is SQLite syncing the cache to disk. It asserts nothing.
func TestSimulationSpeed(t *testing.T) {
	t.Run("scheduler alone", func(t *testing.T) {
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
		{name: "toy library", workload: defaultWorkload},
		{name: "toy library without syncing the cache to disk", workload: syncOff},
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

// hammerWorkload has callers that never wait between transactions, so that under the free runtime
// they are in SQLite and in the toy library's own code at the same time.
var hammerWorkload = workload{
	callers: 8,
	ops:     40,
	latency: time.Millisecond,
	options: toyOptions{readers: 4, busyTimeout: 1000},
	faults:  faults{errors: 30, lostAcks: 30, delays: 10},
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

	// Under the scheduler one task at a time is in SQLite, so SQLite never finds a lock taken. With the
	// scheduler off it can: a writer in a tight loop among readers gets SQLITE_BUSY, as
	// TestSynctestWithCgo shows. This workload is not that tight, and the test logs whether it gets
	// SQLITE_BUSY with the scheduler off, on the bubble's fake clock and on the real clock outside a
	// bubble. It asserts only that the scheduled runs get none.
	t.Run("should never get SQLITE_BUSY under the scheduler without a busy timeout", func(t *testing.T) {
		w := hammerWorkload
		w.options.busyTimeout = 0
		n := seeds(t, 200, w)

		for seed := range n {
			is.EqualSlice(t, nil, simulate(t, seed, w).violations())
		}

		// busy counts the violations in the runs of f, and fails the test for any that is not SQLITE_BUSY.
		busy := func(f func(seed uint64) *outcome) (violations, runs int) {
			for seed := range n {
				o := f(seed)
				for _, violation := range o.violations {
					if !strings.Contains(violation, "database is locked") {
						t.Errorf("seed %v: %v", seed, violation)
					}
				}
				if len(o.violations) > 0 {
					runs++
				}
				violations += len(o.violations)
			}
			return violations, runs
		}

		onFakeClock, _ := busy(func(seed uint64) *outcome { return free(t, seed, w) })

		// The store's latency is in microseconds and it injects no faults, so that the run is short on the
		// real clock.
		w.latency, w.faults = time.Microsecond, faults{}
		onRealClock, runs := busy(func(seed uint64) *outcome {
			r := sim.NewFree(seed)
			path := filepath.Join(t.TempDir(), "cache.db")
			var o *outcome
			is.NotError(t, r.Run(func(ctx context.Context) {
				o = w.run(ctx, r, path)
			}))
			return o
		})

		t.Logf("with no busy timeout, in %v runs each: SQLITE_BUSY 0 times under the scheduler, %v times with the scheduler off on the fake clock, and %v times in %v runs with the scheduler off on the real clock",
			n, onFakeClock, onRealClock, runs)
	})

	// The fake clock orders most of a free run, so a seed often gives the same run again, but not
	// always. This logs how often, and asserts nothing.
	t.Run("can find the planted renewal bug with the scheduler off, but does not always find it again with the same seed", func(t *testing.T) {
		for name, w := range map[string]workload{"callers that pause": defaultWorkload, "callers that never pause": hammerWorkload} {
			n := seeds(t, 200, w)
			var differing int
			for seed := range n {
				first, second := free(t, seed, w), free(t, seed, w)
				if !slices.Equal(first.lines, second.lines) {
					differing++
				}
			}
			t.Logf("with %v, %v of %v seeds gave two different traces when run twice with the scheduler off", name, differing, n)
		}

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

// free runs the workload w under the free runtime with seed, in a bubble of its own for the fake
// clock.
func free(t *testing.T, seed uint64, w workload) *outcome {
	t.Helper()
	var o *outcome
	synctest.Test(t, func(t *testing.T) {
		r := sim.NewFree(seed)
		path := filepath.Join(t.TempDir(), "cache.db")
		is.NotError(t, r.Run(func(ctx context.Context) {
			o = w.run(ctx, r, path)
		}))
		o.lines = r.Lines()
	})
	return o
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
