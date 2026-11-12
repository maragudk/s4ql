package core_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/store/simstore"
)

// options of the instances of a workload, with the bugs of the given names planted.
func options(bugs ...string) core.Options {
	o := core.Options{
		Readers: 2,
		// A caller that waits for a lock of the cache with the scheduler off is a moment from getting
		// it, unless the machine is very slow.
		BusyTimeout: time.Minute,
		// A download that gets no answer holds a catch-up up for this long.
		ObjectTimeout: 10 * time.Second,
	}
	o.SyncOff()
	for _, bug := range bugs {
		o.Plant(bug)
	}
	return o
}

// singleWorkload has one instance, and enough callers, simulated time, and faults of the store for
// commits, renewals, reads, unknown outcomes, and lease timeouts to overlap.
var singleWorkload = workload{
	instances: 1,
	callers:   4,
	ops:       12,
	writes:    40,
	think:     15 * time.Second,
	timeout:   30 * time.Second,
	options:   options(),
	durations: []time.Duration{30 * time.Second},
	latency:   50 * time.Millisecond,
	faults:    simstore.AllFaults(),
}

// fullWorkload has everything: three instances that compete for the lease, with leases of
// different durations, clocks that move, a crash and a restart, buggify, every fault of the store,
// and downloads that are damaged.
var fullWorkload = workload{
	instances: 3,
	callers:   3,
	ops:       12,
	writes:    40,
	think:     15 * time.Second,
	hold:      10 * time.Second,
	timeout:   30 * time.Second,
	options:   options(),
	durations: []time.Duration{20 * time.Second, 30 * time.Second, 45 * time.Second},
	latency:   50 * time.Millisecond,
	faults:    simstore.AllFaults(),
	damaged:   20,
	clocks:    stopMonotonic | wallForward | wallBackward | stopWall,
	faulty:    50,
	crashes:   1,
	buggify:   true,
}

// hammerWorkload has callers that never wait between transactions, so that with the scheduler off
// they are in SQLite and in the core's own code at the same time.
var hammerWorkload = workload{
	instances: 1,
	callers:   8,
	ops:       30,
	writes:    40,
	timeout:   30 * time.Second,
	options: func() core.Options {
		o := options()
		o.Readers = 4
		return o
	}(),
	durations: []time.Duration{30 * time.Second},
	latency:   50 * time.Millisecond,
	faults:    simstore.AllFaults(),
}

// tally of what happened in the runs of a workload.
type tally struct {
	runs, steps                                                                                           int
	commits, emptyCommits, failedCommits, unknownCommits, cutCommits, rollbacks, abandoned, refusedWrites int64
	reads, refusedReads, expiredReads, overtakenReads, statements, stops, restarts                        int64
	events                                                                                                map[string]int
	// trace counts the lines of the traces that contain each marker.
	trace map[string]int
	// serveAfter is how long after the end of the workload an instance served, at most, and the seed
	// where it took longest.
	serveAfter time.Duration
	slowest    uint64
}

// markers of the trace that are counted: the faults of the store, and what the harness did.
var markers = map[string]string{
	"lost requests":             "was lost on its way to the store",
	"lost responses":            "was lost on its way back",
	"calls with no answer":      "no answer comes",
	"conflicts":                 "a conflict with another write in flight",
	"slow ways":                 "is slow on its way",
	"broken reads":              "whose content breaks off",
	"cut-offs":                  "cut off for",
	"outages":                   "unreachable for",
	"writes stuck":              "is stuck in the network",
	"writes applied late":       "and is applied",
	"callers that gave up":      "the caller gives up",
	"damaged downloads":         "is damaged",
	"buggify points that fired": " fires",
	"moves of a clock":          " clock",
	"crashes":                   " crashes",
	"timeouts that expired":     " expires",
}

func (y *tally) add(seed uint64, s simulation) {
	if y.events == nil {
		y.events, y.trace = map[string]int{}, map[string]int{}
	}
	o := s.outcome
	y.runs++
	y.steps += s.steps
	y.commits += o.commits.Load()
	y.emptyCommits += o.emptyCommits.Load()
	y.failedCommits += o.failedCommits.Load()
	y.unknownCommits += o.unknownCommits.Load()
	y.cutCommits += o.cutCommits.Load()
	y.rollbacks += o.rollbacks.Load()
	y.abandoned += o.abandonedWrites.Load()
	y.refusedWrites += o.refusedWrites.Load()
	y.reads += o.reads.Load()
	y.refusedReads += o.refusedReads.Load()
	y.expiredReads += o.expiredReads.Load()
	y.overtakenReads += o.overtakenReads.Load()
	y.statements += o.statements.Load()
	y.stops += o.stops.Load()
	y.restarts += o.restarts.Load()
	for kind, n := range o.events {
		y.events[kind] += n
	}
	for name, marker := range markers {
		y.trace[name] += s.count(marker)
	}
	if o.serveAfter > y.serveAfter {
		y.serveAfter, y.slowest = o.serveAfter, seed
	}
}

// sorted counts of a map, as text.
func sorted(counts map[string]int) string {
	var parts []string
	for name, n := range counts {
		parts = append(parts, fmt.Sprint(name, " ", n))
	}
	slices.Sort(parts)
	return strings.Join(parts, ", ")
}

func (y *tally) log(t *testing.T, w workload) {
	t.Helper()
	t.Logf("in the first run of each of %v seeds: %v steps, %v statements in write transactions, %v commits, %v commits of nothing, %v failed commits, %v of unknown outcome, %v cut off by a crash, %v rollbacks, %v transactions abandoned, %v refused at their start",
		y.runs, y.steps, y.statements, y.commits, y.emptyCommits, y.failedCommits, y.unknownCommits, y.cutCommits, y.rollbacks, y.abandoned, y.refusedWrites)
	t.Logf("%v reads of which %v were overtaken by a commit, %v refused reads of which %v for a lease that had run out by a clock, %v stops of an instance, %v restarts after a crash",
		y.reads, y.overtakenReads, y.refusedReads, y.expiredReads, y.stops, y.restarts)
	t.Logf("in the traces: %v", sorted(y.trace))
	t.Logf("events of the instances: %v", sorted(y.events))
	t.Logf("an instance served at most %v after the end of the workload, in seed %v, where %v is the bound", y.serveAfter, y.slowest, w.liveness())
}

// TestSimulation runs each workload in a parallel subtest, every seed twice. The hash of all its
// traces that each subtest logs is the same with -parallel 1.
func TestSimulation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		workload workload
		// seeds of the default run.
		seeds int
	}{
		{name: "should give the same trace when a seed is run twice, and no violations, with one instance", workload: singleWorkload, seeds: 40},
		{name: "should give the same trace when a seed is run twice, and no violations, with instances that compete, clocks that move, crashes, and buggify", workload: fullWorkload, seeds: 40},
		{name: "should give the same trace when a seed is run twice, and no violations, with callers that never pause", workload: hammerWorkload, seeds: 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			n := seeds(t, test.seeds)
			hashes := map[string]bool{}
			all := sha256.New()
			var y tally
			var differing int
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
				y.add(seed, first)
			}
			elapsed := time.Since(start)
			t.Logf("%v seeds, each run twice: %v pairs differed; %v distinct traces", n, differing, len(hashes))
			y.log(t, test.workload)
			// The other subtests run at the same time, so this is no measure of speed.
			t.Logf("%v runs in %v, %.0f runs per second", 2*n, elapsed.Round(time.Millisecond), float64(2*n)/elapsed.Seconds())
			// The hash depends only on the code and the seeds, so it can be compared between machines.
			t.Logf("hash of all traces: %x", all.Sum(nil))
			is.Equal(t, 0, differing)
			if n >= 40 {
				is.Equal(t, int(n), len(hashes), "two seeds gave the same trace")
				is.True(t, y.commits > 0 && y.reads > 0 && y.failedCommits > 0 && y.refusedReads > 0 && y.overtakenReads > 0, "the workload did not reach every kind of result")
				is.True(t, y.trace["timeouts that expired"] > 0, "no timeout expired")
				is.True(t, y.events["lease/renewed/renewed"] > 0 && y.events["lease/taken/taken"] > 0, "no lease was taken or renewed")
			}
		})
	}
}

// TestSimulationSeed runs the one seed that the environment variable S4QL_SIM_SEED names, with the
// workload that S4QL_SIM_WORKLOAD names (single, the default, full, or hammer), and logs its whole
// trace. The environment variable S4QL_SIM_BUG can name a planted bug by its name in
// [plantedBugs], to run its workload instead, and S4QL_SIM_CONTROL to run the control of that
// bug.
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
	if name := os.Getenv("S4QL_SIM_BUG"); name != "" {
		i := slices.IndexFunc(plantedBugs, func(b plantedBug) bool { return b.bug == name })
		is.True(t, i >= 0, "no planted bug is named "+name)
		w = plantedBugs[i].workload
		if os.Getenv("S4QL_SIM_CONTROL") != "" {
			w = *plantedBugs[i].control
		}
		if os.Getenv("S4QL_SIM_CLEAN") != "" {
			w.options = w.options.Unplanted()
		}
	}

	s := simulate(t, seed, w)
	t.Logf("seed %v: %v steps, hash %v, trace:\n%v", seed, s.steps, s.hash(), strings.Join(s.lines, "\n"))
	is.EqualSlice(t, nil, s.violations())
}

// plantedBug that the checks must find.
type plantedBug struct {
	// bug is the name of the field of the core's planted bugs.
	bug  string
	name string
	// workload that finds the bug, with the bug planted.
	workload workload
	// control is the same workload without the one ability that the bug needs, with which no seed
	// may find it.
	control *workload
	// symptoms of the bug: every seed that finds anything must find a violation with one of them.
	symptoms []string
}

// plant the bug with the given name in a copy of w.
func plant(w workload, bug string) workload {
	w.options.Plant(bug)
	return w
}

// competing is a workload aimed at the rules of the lease: two instances that compete for it, with
// every fault of the store, and no clock faults, crashes, buggify, or damaged downloads, changed
// by change. Its callers run 30 transactions each, every instance gets the faults of a clock that
// change asks for, and a client is cut off more often and for longer than a lease lasts, because
// a lease is lost to another instance only while its holder cannot renew it.
func competing(change func(w *workload)) workload {
	w := fullWorkload
	w.instances, w.ops, w.faulty = 2, 30, 100
	w.clocks, w.crashes, w.buggify, w.damaged, w.hold = 0, 0, false, 0, 0
	w.think = 6 * time.Second
	w.faults.CutOff, w.faults.CutOffFor = 0.02, 2*time.Minute
	change(&w)
	return w
}

// unknownOutcomes is a workload aimed at the rules of a manifest swap whose outcome is unknown:
// one instance whose callers give up before a request to the store times out, and a store that
// loses many requests and answers, and delivers most of the writes that it lost later.
func unknownOutcomes(change func(w *workload)) workload {
	w := singleWorkload
	w.ops, w.think, w.timeout = 30, 5*time.Second, 4*time.Second
	w.faults.LostRequest, w.faults.LostResponse, w.faults.Hang = 0.1, 0.1, 0.5
	w.faults.Late, w.faults.LateBy = 0.8, 30*time.Second
	change(&w)
	return w
}

// calm is a workload on a store without faults.
func calm(w workload) workload {
	w.faults = simstore.Faults{}
	return w
}

// Symptoms of planted bugs, as parts of the texts of violations.
const (
	// stale: a read missed a commit that was acknowledged before it started.
	stale = "after commit"
	// early: an instance took the lease before the manifest had stood still for long enough.
	early = "takes the lease of a holder"
	// unsettled: a read missed a commit that had returned with an unknown outcome before it started.
	unsettled = "returned with an unknown outcome"
)

// plantedBugs that the checks must find. Each holds the workload that finds the bug. A bug that
// needs one ability of the harness or one fault also holds a control: the same workload without it,
// with which no seed may find the bug.
var plantedBugs = []plantedBug{
	{
		// A renewal is in flight together with a commit only if it is sent while callers commit,
		// which they do every few seconds, so that a renewal is seldom due after 20 seconds. So the
		// lease is renewed a second after the last renewal.
		bug:  "renewOutsideLock",
		name: "a renewal that swaps the manifest outside the lock",
		workload: plant(func() workload {
			w := singleWorkload
			w.options.RenewAfter = time.Second
			return w
		}(), "renewOutsideLock"),
		symptoms: []string{"lost the lease", "and the transaction began at", "the head of the log went back"},
	},
	{
		// Without faults, a commit never fails, so the cache is ahead of the log only while a commit
		// is on its way, and only a read at that moment finds the bug.
		bug:      "cacheFirst",
		name:     "a commit that reaches the cache before the manifest",
		workload: plant(calm(singleWorkload), "cacheFirst"),
		symptoms: []string{"which the log does not hold"},
	},
	{
		bug:  "wallOnly",
		name: "a read that checks the lease against the wall clock only",
		workload: plant(competing(func(w *workload) {
			w.clocks = wallBackward | stopWall
		}), "wallOnly"),
		control: new(plant(competing(func(w *workload) {
			w.clocks, w.unmoved = wallBackward|stopWall, true
		}), "wallOnly")),
		symptoms: []string{stale},
	},
	{
		bug:  "monotonicOnly",
		name: "a read that checks the lease against the monotonic clock only",
		workload: plant(competing(func(w *workload) {
			w.clocks = stopMonotonic
		}), "monotonicOnly"),
		control: new(plant(competing(func(w *workload) {
			w.clocks, w.unmoved = stopMonotonic, true
		}), "monotonicOnly")),
		symptoms: []string{stale},
	},
	{
		// A lease that is not released is left only by a holder that crashed, or one that could not
		// release it, which a store without faults never causes.
		bug:  "takeoverAfterRelease",
		name: "an instance that takes over a lease only after a release",
		workload: plant(func() workload {
			w := calm(singleWorkload)
			w.crashes = 1
			return w
		}(), "takeoverAfterRelease"),
		control:  new(plant(calm(singleWorkload), "takeoverAfterRelease")),
		symptoms: []string{"no instance serves", "the replay of the whole log"},
	},
	{
		// A renewal without a new salt writes the bytes of the manifest that it replaces only if the
		// holder's wall clock shows the same time as at that write, so the bug needs a wall clock that
		// stands still. A commit in between writes other bytes, so few transactions write.
		bug:  "keepSalt",
		name: "a renewal that writes no new salt",
		workload: plant(competing(func(w *workload) {
			w.clocks, w.writes = stopWall, 10
		}), "keepSalt"),
		control: new(plant(competing(func(w *workload) {
			w.clocks, w.writes, w.unmoved = stopWall, 10, true
		}), "keepSalt")),
		symptoms: []string{stale, early, "a version that it had before"},
	},
	{
		// The wait of the bug succeeds against a live holder only if it ends before the holder's next
		// write, so the instance with the shorter lease watches a holder that renews every 60 seconds.
		// With leases of one duration, the bug waits as long as the design does, so its control shows
		// only that the workload finds nothing else.
		bug:  "ownDuration",
		name: "an instance that waits for its own lease duration, and not for the holder's",
		workload: plant(competing(func(w *workload) {
			w.durations = []time.Duration{10 * time.Second, 90 * time.Second}
		}), "ownDuration"),
		control: new(plant(competing(func(w *workload) {
			w.durations = []time.Duration{30 * time.Second, 30 * time.Second}
		}), "ownDuration")),
		symptoms: []string{stale, early},
	},
	{
		// The bug shows when the manifest changes during the wait. With one node, an instance that
		// waits for the lease waits for a holder that crashed, whose manifest changes only if a write
		// that it sent arrives late.
		bug:      "lateVersion",
		name:     "an instance that swaps against the version it read last, and not the one it watched",
		workload: plant(competing(func(w *workload) {}), "lateVersion"),
		control: new(plant(competing(func(w *workload) {
			w.instances, w.crashes, w.faults.Late, w.faults.AfterCrash = 1, 1, 0, 0
		}), "lateVersion")),
		symptoms: []string{stale, early},
	},
	{
		// Only an answer that comes late after the store applied the swap makes the lease of the bug
		// outlast the wait of another instance, which starts when it reads the swap. So the answers are
		// often slow, a request waits for its answer for longer than they are, and the leases are short
		// enough for the wait to end before a slow answer comes.
		bug:  "leaseFromAnswer",
		name: "a holder that counts its lease from the answer to a swap",
		workload: plant(competing(func(w *workload) {
			w.faults.Slow, w.options.StoreTimeout = 0.1, time.Minute
			w.durations = []time.Duration{10 * time.Second}
		}), "leaseFromAnswer"),
		control: new(plant(competing(func(w *workload) {
			w.faults.Slow, w.options.StoreTimeout = 0, time.Minute
			w.durations = []time.Duration{10 * time.Second}
		}), "leaseFromAnswer")),
		symptoms: []string{stale},
	},
	{
		bug:  "ignoreCacheError",
		name: "a commit that ignores a failure of the cache after the manifest swap",
		workload: plant(func() workload {
			w := singleWorkload
			w.buggify = true
			return w
		}(), "ignoreCacheError"),
		control:  new(plant(singleWorkload, "ignoreCacheError")),
		symptoms: []string{stale, "a read saw at commit", "the cache of", "began at the position", "on top of", "the instance stopped"},
	},
	{
		// A swap has an unknown outcome only if the store or the network fails.
		bug:      "ackUnknown",
		name:     "a commit that is acknowledged before the outcome of its swap is known",
		workload: plant(unknownOutcomes(func(w *workload) {}), "ackUnknown"),
		control:  new(plant(calm(unknownOutcomes(func(w *workload) {})), "ackUnknown")),
		symptoms: []string{"was acknowledged, and the log does not have it", "which the log does not hold", "and the transaction began at", stale},
	},
	{
		// A read of an unchanged manifest is wrong about a swap only if the swap arrives after the
		// read, which a write does that is stuck in the network.
		bug:      "settleByRead",
		name:     "a commit counted as not committed after a read of an unchanged manifest, without the swap that rules it out",
		workload: plant(unknownOutcomes(func(w *workload) {}), "settleByRead"),
		control: new(plant(unknownOutcomes(func(w *workload) {
			w.faults.Late = 0
		}), "settleByRead")),
		symptoms: []string{unsettled, "lost the lease"},
	},
	{
		bug:      "serveUnknown",
		name:     "an instance that goes on serving after a commit returned with an unknown outcome",
		workload: plant(unknownOutcomes(func(w *workload) {}), "serveUnknown"),
		control:  new(plant(calm(unknownOutcomes(func(w *workload) {})), "serveUnknown")),
		symptoms: []string{unsettled},
	},
	{
		// The log is looked at only when another instance has taken the lease while a commit still
		// waits for its outcome, which takes longer than a lease lasts: so the callers wait for longer,
		// the answer to a swap is often lost, and a client is often cut off.
		bug:  "lostMeansFailed",
		name: "a commit counted as not committed because the lease was lost, without a look at the log",
		workload: plant(competing(func(w *workload) {
			w.writes, w.timeout, w.faults.LostResponse, w.faults.CutOff = 70, 100*time.Second, 0.2, 0.05
		}), "lostMeansFailed"),
		control: new(plant(competing(func(w *workload) {
			w.writes, w.timeout, w.faults.LostResponse, w.faults.CutOff, w.instances = 70, 100*time.Second, 0.2, 0.05, 1
		}), "lostMeansFailed")),
		symptoms: []string{"whose commit failed"},
	},
	{
		// Replay keeps a part of a commit object only if the commit object breaks off. A download
		// has five minutes, so that none ends because its time is up.
		bug:  "replayCommitsEarly",
		name: "a replay that commits before the reader has reported the end",
		workload: plant(competing(func(w *workload) {
			w.faults.BrokenRead, w.options.ObjectTimeout, w.crashes = 0.3, 5*time.Minute, 1
		}), "replayCommitsEarly"),
		control: new(plant(competing(func(w *workload) {
			w.faults.BrokenRead, w.options.ObjectTimeout, w.crashes = 0, 5*time.Minute, 1
		}), "replayCommitsEarly")),
		symptoms: []string{"the cache of", "the instance stopped", "a read saw at commit", "the replay of the whole log"},
	},
	{
		bug:      "keyTwice",
		name:     "a commit object that lists a key twice",
		workload: plant(calm(singleWorkload), "keyTwice"),
		symptoms: []string{"is no net effect"},
	},
	{
		bug:      "forgetDelete",
		name:     "a commit object that forgets a delete",
		workload: plant(calm(singleWorkload), "forgetDelete"),
		symptoms: []string{"with the changes", "a read saw at commit", "the cache of", "was acknowledged, and the log does not have it"},
	},
	{
		// Only bytes that are damaged on their way give a commit object that cannot be read.
		bug:  "skipUnreadable",
		name: "a catch-up that skips a commit object that it cannot read",
		workload: plant(competing(func(w *workload) {
			w.damaged, w.crashes = 200, 1
		}), "skipUnreadable"),
		control: new(plant(competing(func(w *workload) {
			w.damaged, w.crashes = 0, 1
		}), "skipUnreadable")),
		symptoms: []string{"the cache of", "a read saw at commit", "the instance stopped", "began at the position", "the replay of the whole log"},
	},
	{
		// The renewal waits for the writer lock for longer than the lease has left only if a write
		// transaction stays open that long.
		bug:  "renewWaitsForWriter",
		name: "a renewal that waits for the writer lock",
		workload: plant(func() workload {
			w := calm(singleWorkload)
			w.hold, w.timeout, w.steady = 40*time.Second, 5*time.Minute, true
			return w
		}(), "renewWaitsForWriter"),
		control: new(plant(func() workload {
			w := calm(singleWorkload)
			w.timeout, w.steady = 5*time.Minute, true
			return w
		}(), "renewWaitsForWriter")),
		symptoms: []string{"the lease had run out by a clock"},
	},
}

// TestSimulation_plantedBugs runs the workload of each bug until four seeds have found the bug,
// from at most 200 seeds, and its control and the same workload without the bug for 30 seeds each.
// With S4QL_SIM_SEEDS set, all three run that many seeds, and the workload with the bug runs all of
// them, to count how many find it.
func TestSimulation_plantedBugs(t *testing.T) {
	t.Parallel()
	_, counting := os.LookupEnv("S4QL_SIM_SEEDS")
	for _, bug := range plantedBugs {
		name := "should find " + bug.name + " with some seed and again on every rerun of it"
		if bug.control != nil {
			name += ", and never without what it needs"
		}
		name += ", and nothing without the bug"
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			is.True(t, bug.workload.options.Planted(), "the workload has no planted bug")
			n := seeds(t, 200)
			var found []uint64
			var ran uint64
			var y tally
			symptoms := map[string]int{}
			for seed := range n {
				if !counting && len(found) == 4 {
					break
				}
				ran++
				first := simulate(t, seed, bug.workload)
				y.add(seed, first)
				if len(first.violations()) == 0 {
					continue
				}
				found = append(found, seed)
				var known bool
				for _, symptom := range bug.symptoms {
					if slices.ContainsFunc(first.violations(), func(v string) bool { return strings.Contains(v, symptom) }) {
						symptoms[symptom]++
						known = true
					}
				}
				is.True(t, known, fmt.Sprintf("seed %v found something else: %v", seed, first.violations()))
				if len(found) > 2 {
					continue
				}
				// The same seed finds the same violations at the same place in the same trace.
				for range 2 {
					again := simulate(t, seed, bug.workload)
					is.Equal(t, first.hash(), again.hash())
					is.EqualSlice(t, first.violations(), again.violations())
				}
				if len(found) == 1 {
					at := max(0, slices.IndexFunc(first.lines, func(line string) bool { return strings.Contains(line, "VIOLATION") }))
					t.Logf("seed %v, rerun twice with the same trace of %v lines; its first violation: %v", seed, len(first.lines), first.lines[at])
				}
			}
			t.Logf("%v of %v seeds found the bug; the first are %v; by symptom: %v", len(found), ran, found[:min(len(found), 10)], sorted(symptoms))
			if counting {
				y.log(t, bug.workload)
			}
			n = seeds(t, 30)
			if bug.control != nil {
				is.True(t, bug.control.options.Planted(), "the control has no planted bug")
				var controlFound int
				for seed := range n {
					if v := simulate(t, seed, *bug.control).violations(); len(v) > 0 {
						controlFound++
						t.Errorf("seed %v found the bug without what it needs: %v", seed, v)
					}
				}
				t.Logf("%v of %v seeds found the bug without what it needs", controlFound, n)
			}
			// The workload of a bug reaches what the full workload seldom does, so it must also find
			// nothing in a core without bugs.
			clean := bug.workload
			clean.options = clean.options.Unplanted()
			var cleanFound int
			for seed := range n {
				if v := simulate(t, seed, clean).violations(); len(v) > 0 {
					cleanFound++
					t.Errorf("seed %v found something without the bug: %v", seed, v)
				}
			}
			t.Logf("%v of %v seeds found something without the bug", cleanFound, n)
			if !counting || ran >= 200 {
				is.True(t, len(found) > 0, fmt.Sprintf("none of %v seeds found the bug", ran))
			}
		})
	}
}
