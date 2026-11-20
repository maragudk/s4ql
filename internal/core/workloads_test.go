package core_test

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"maps"
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
// different durations, clocks that move, a crash and a restart, instances that are closed and
// opened again while their callers run, buggify, every fault of the store, and downloads that are
// damaged.
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
	closes:    1,
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
	options:   options(),
	durations: []time.Duration{30 * time.Second},
	latency:   50 * time.Millisecond,
	faults:    simstore.AllFaults(),
}

// What each workload must reach, at least, in the seeds of the default run of [TestSimulation]:
// each kind of result, fault, and event, by the names of [tally.reached]. A minimum is a quarter of
// what the seeds reached on 2026-11-18, for each kind that they reached four times at least. A run
// depends only on the code and its seed, so a minimum fails only when the code or the workload
// changes, never by chance, and a quarter leaves room for a change that moves faults between seeds.
// A kind of fault or event that a change switches off falls to zero, and fails. The upserts and
// deletes of keys of the other types, and of -0.0, are a quarter of what the seeds reached on
// 2026-11-19, when the tables got those types; the other minimums stayed as they were, and the
// seeds still reach them. The upserts of rows with NULL, and the upserts of NULL into a column that
// does not take it, which the core refuses, are a quarter of what the seeds reached on 2026-11-20,
// when the tables got columns that take NULL.
var (
	// singleReaches is what the 40 seeds of the default run of [singleWorkload] must reach. It also
	// has one failed commit, of the two that they reached, which the rule leaves out.
	singleReaches = map[string]int64{
		"callers that gave up":          25,
		"calls with no answer":          11,
		"commits":                       149,
		"commits of nothing":            25,
		"conflicts":                     3,
		"cut-offs":                      3,
		"deletes of a blob key":         22,
		"deletes of a text key":         10,
		"deletes of the key -0.0":       3,
		"event catch_up/caught_up":      10,
		"event commit/committed":        149,
		"event commit/empty":            25,
		"event lease/released/released": 20,
		"event lease/renewed/renewed":   8,
		"event lease/renewed/unknown":   6,
		"event lease/taken/taken":       20,
		"event refusal/refused":         6,
		"event stop/closed":             20,
		"failed commits":                1,
		"lost requests":                 6,
		"lost responses":                6,
		"reads":                         286,
		"reads overtaken by a commit":   5,
		"reads refused by a clock":      1,
		"refused reads":                 2,
		"refused upserts of NULL":       5,
		"rollbacks":                     20,
		"slow ways":                     16,
		"timeouts that expired":         27,
		"transactions refused at start": 4,
		"upserts with NULL":             52,
		"upserts with a blob key":       35,
		"upserts with a text key":       37,
		"upserts with the key -0.0":     7,
		"writes applied late":           3,
		"writes stuck":                  21,
	}

	// fullReaches is what the 30 seeds of the default run of [fullWorkload] must reach.
	fullReaches = map[string]int64{
		"broken reads":                             21,
		"buggify points that fired":                19,
		"callers that gave up":                     95,
		"calls with no answer":                     31,
		"commits":                                  76,
		"commits of nothing":                       15,
		"conflicts":                                2,
		"crashes":                                  7,
		"cut-offs":                                 8,
		"damaged downloads":                        1,
		"deletes of a blob key":                    16,
		"deletes of a text key":                    2,
		"deletes of the key -0.0":                  2,
		"event catch_up/caught_up":                 17,
		"event commit/committed":                   71,
		"event commit/committed_and_stopped":       4,
		"event commit/empty":                       15,
		"event commit/not_committed":               3,
		"event lease/lost/lost":                    2,
		"event lease/released/released":            24,
		"event lease/renewed/renewed":              123,
		"event lease/renewed/unknown":              18,
		"event lease/taken/taken":                  29,
		"event refusal/refused":                    628,
		"event stop/closed":                        42,
		"event stop/local_failure":                 4,
		"event unknown_outcome/renewal/lease_lost": 1,
		"event unknown_outcome/renewal/renewed":    7,
		"failed commits":                           3,
		"instances closed and opened":              12,
		"lost requests":                            36,
		"lost responses":                           33,
		"moves of a clock":                         124,
		"outages":                                  3,
		"reads":                                    177,
		"reads refused by a clock":                 7,
		"refused reads":                            394,
		"restarts after a crash":                   7,
		"restarts that recovered from WAL":         5,
		"refused upserts of NULL":                  6,
		"rollbacks":                                12,
		"slow ways":                                69,
		"stops":                                    4,
		"timeouts that expired":                    362,
		"transactions abandoned":                   1,
		"transactions refused at start":            244,
		"upserts with NULL":                        29,
		"upserts with a blob key":                  22,
		"upserts with a text key":                  26,
		"upserts with the key -0.0":                2,
		"writes applied late":                      3,
		"writes stuck":                             13,
	}

	// hammerReaches is what the 30 seeds of the default run of [hammerWorkload] must reach: a quarter
	// of what they reached on 2026-11-19. Until then the default run had 10 seeds, which reached 20
	// reads refused by a clock on 2026-11-18, 8 after each caller got a reader of its own, and none
	// after [core.DB.Reader] got its yield. That is chance and no change of the code: over 200 seeds
	// the workload reached 88 of them before the readers and 80 after, about 4 in 10 seeds, which is
	// less than the minimum of 5 that the 10 seeds had.
	hammerReaches = map[string]int64{
		"broken reads":                               1,
		"callers that gave up":                       71,
		"calls with no answer":                       28,
		"commits":                                    485,
		"commits of nothing":                         90,
		"commits of unknown outcome":                 2,
		"conflicts":                                  38,
		"cut-offs":                                   7,
		"deletes of a blob key":                      85,
		"deletes of a text key":                      26,
		"deletes of the key -0.0":                    9,
		"event catch_up/caught_up":                   8,
		"event commit/committed":                     485,
		"event commit/empty":                         90,
		"event commit/not_committed":                 3,
		"event commit/unknown":                       2,
		"event lease/released/released":              15,
		"event lease/renewed/renewed":                6,
		"event lease/renewed/unknown":                6,
		"event lease/taken/taken":                    15,
		"event refusal/refused":                      171,
		"event stop/closed":                          15,
		"event unknown_outcome/commit/not_committed": 1,
		"event unknown_outcome/renewal/renewed":      1,
		"failed commits":                             3,
		"lost requests":                              24,
		"lost responses":                             20,
		"outages":                                    3,
		"reads":                                      971,
		"reads overtaken by a commit":                169,
		"reads refused by a clock":                   4,
		"refused reads":                              91,
		"refused upserts of NULL":                    23,
		"rollbacks":                                  66,
		"slow ways":                                  47,
		"timeouts that expired":                      117,
		"transactions refused at start":              96,
		"upserts with NULL":                          145,
		"upserts with a blob key":                    87,
		"upserts with a text key":                    143,
		"upserts with the key -0.0":                  22,
		"writes applied late":                        9,
		"writes stuck":                               50,
	}

	// idleReaches is what the 20 seeds of the default run of [idling] must reach: a quarter of what
	// they reached on 2026-11-19, with the write transactions that an instance ended because it lost its
	// lease, which only this workload reaches often.
	idleReaches = map[string]int64{
		"broken reads":                                    7,
		"callers that gave up":                            126,
		"calls with no answer":                            94,
		"commits":                                         70,
		"commits of nothing":                              11,
		"commits of unknown outcome":                      1,
		"conflicts":                                       10,
		"cut-offs":                                        47,
		"event catch_up/caught_up":                        10,
		"event catch_up/failed":                           1,
		"event commit/committed":                          70,
		"event commit/empty":                              11,
		"event commit/not_committed":                      2,
		"event commit/unknown":                            1,
		"event lease/lost/lost":                           9,
		"event lease/released/released":                   12,
		"event lease/renewed/conflict":                    2,
		"event lease/renewed/renewed":                     88,
		"event lease/renewed/unknown":                     220,
		"event lease/taken/taken":                         22,
		"event refusal/refused":                           551,
		"event stop/closed":                               15,
		"event transaction_ended/rolled_back":             5,
		"event unknown_outcome/commit/not_committed":      1,
		"event unknown_outcome/renewal/lease_lost":        9,
		"event unknown_outcome/renewal/renewed":           9,
		"failed commits":                                  2,
		"idle transactions ended for the lease":           5,
		"idle transactions left open ended for the lease": 5,
		"lost requests":                                   17,
		"lost responses":                                  11,
		"outages":                                         1,
		"reads":                                           230,
		"reads overtaken by a commit":                     5,
		"reads refused by a clock":                        92,
		"refused reads":                                   310,
		"refused upserts of NULL":                         6,
		"rollbacks":                                       10,
		"slow ways":                                       37,
		"timeouts that expired":                           458,
		"transactions refused at start":                   272,
		"upserts with NULL":                               16,
		"writes applied late":                             4,
		"writes stuck":                                    133,
	}

	// lateReaches is what the 10 seeds of the default run of [lateWrites] must reach.
	lateReaches = map[string]int64{
		"broken reads":                             14,
		"callers that gave up":                     192,
		"calls with no answer":                     79,
		"commits":                                  72,
		"commits of nothing":                       12,
		"commits of unknown outcome":               3,
		"conflicts":                                14,
		"crashes":                                  5,
		"cut-offs":                                 31,
		"event catch_up/caught_up":                 8,
		"event catch_up/failed":                    2,
		"event commit/committed":                   72,
		"event commit/empty":                       12,
		"event commit/not_committed":               13,
		"event commit/unknown":                     3,
		"event lease/lost/lost":                    7,
		"event lease/released/released":            6,
		"event lease/renewed/conflict":             4,
		"event lease/renewed/renewed":              63,
		"event lease/renewed/unknown":              72,
		"event lease/taken/taken":                  15,
		"event refusal/refused":                    673,
		"event stop/closed":                        16,
		"event unknown_outcome/commit/lease_lost":  1,
		"event unknown_outcome/renewal/lease_lost": 5,
		"event unknown_outcome/renewal/renewed":    8,
		"failed commits":                           13,
		"instances closed and opened":              6,
		"lost requests":                            31,
		"lost responses":                           24,
		"outages":                                  2,
		"reads":                                    178,
		"reads overtaken by a commit":              1,
		"reads refused by a clock":                 8,
		"refused reads":                            410,
		"restarts after a crash":                   5,
		"restarts that recovered from WAL":         2,
		"refused upserts of NULL":                  3,
		"rollbacks":                                12,
		"slow ways":                                132,
		"timeouts that expired":                    509,
		"transactions refused at start":            263,
		"upserts with NULL":                        26,
		"writes applied late":                      11,
		"writes stuck":                             73,
	}
)

// tally of what happened in the runs of a workload.
type tally struct {
	runs, steps                                                                                           int
	endedTxs, keptEnded                                                                                   int64
	commits, emptyCommits, failedCommits, unknownCommits, cutCommits, rollbacks, abandoned, refusedWrites int64
	reads, refusedReads, expiredReads, overtakenReads, statements, stops, restarts, reopens               int64
	recoveries, recoveredFrames, mostFrames                                                               int64
	events                                                                                                map[string]int
	// trace counts the lines of the traces that contain each marker.
	trace map[string]int
	// landings counts the writes that the store held back, as [run.verifyLandings] does, and
	// landingSeeds the seeds that had at least one of each.
	landings, landingSeeds map[string]int
	// serveAfter is how long after the end of the workload an instance served, at most, and the seed
	// where it took longest.
	serveAfter time.Duration
	slowest    uint64
}

// markers of the trace that are counted: the faults of the store, what the harness did, and the
// keys of the other types that matter most.
var markers = map[string]string{
	"upserts with the key -0.0": "upsert r-0,",
	"deletes of the key -0.0":   "delete r-0 from",
	"upserts with a text key":   `upsert "`,
	"upserts with a blob key":   "upsert x'",
	"deletes of a text key":     `delete "`,
	"deletes of a blob key":     "delete x'",
	"upserts with NULL":         "with NULL: <nil>",
	"refused upserts of NULL":   "takes none: invalid",
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
		y.events, y.trace, y.landings, y.landingSeeds = map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
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
	y.reopens += o.reopens.Load()
	y.endedTxs += o.endedTxs.Load()
	y.keptEnded += o.keptEnded.Load()
	y.recoveries += o.recoveries.Load()
	y.recoveredFrames += o.recoveredFrames.Load()
	y.mostFrames = max(y.mostFrames, o.mostFrames.Load())
	for kind, n := range o.events {
		y.events[kind] += n
	}
	for name, marker := range markers {
		y.trace[name] += s.count(marker)
	}
	for kind, n := range o.landings {
		y.landings[kind] += n
		if n > 0 {
			y.landingSeeds[kind]++
		}
	}
	if o.serveAfter > y.serveAfter {
		y.serveAfter, y.slowest = o.serveAfter, seed
	}
}

// reached is how often each kind of result, fault, and event happened, by a name: the counts of
// the outcomes, the markers of the traces, and the events of the instances, with "event " before
// their kind.
func (y *tally) reached() map[string]int64 {
	counts := map[string]int64{
		"commits":                                         y.commits,
		"commits of nothing":                              y.emptyCommits,
		"failed commits":                                  y.failedCommits,
		"commits of unknown outcome":                      y.unknownCommits,
		"commits cut off by a crash":                      y.cutCommits,
		"rollbacks":                                       y.rollbacks,
		"transactions abandoned":                          y.abandoned,
		"transactions refused at start":                   y.refusedWrites,
		"reads":                                           y.reads,
		"refused reads":                                   y.refusedReads,
		"reads refused by a clock":                        y.expiredReads,
		"reads overtaken by a commit":                     y.overtakenReads,
		"stops":                                           y.stops,
		"restarts after a crash":                          y.restarts,
		"instances closed and opened":                     y.reopens,
		"restarts that recovered from WAL":                y.recoveries,
		"idle transactions ended for the lease":           y.endedTxs,
		"idle transactions left open ended for the lease": y.keptEnded,
	}
	for name, n := range y.trace {
		counts[name] = int64(n)
	}
	for kind, n := range y.events {
		counts["event "+kind] = int64(n)
	}
	return counts
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
	t.Logf("%v reads of which %v were overtaken by a commit, %v refused reads of which %v for a lease that had run out by a clock, %v stops of an instance, %v restarts after a crash, %v instances closed and opened again while their callers ran",
		y.reads, y.overtakenReads, y.refusedReads, y.expiredReads, y.stops, y.restarts, y.reopens)
	t.Logf("%v restarts whose Open recovered commits from a write-ahead log that the file of the cache alone lacked, with %v frames in those logs, and %v in the largest",
		y.recoveries, y.recoveredFrames, y.mostFrames)
	t.Logf("in the traces: %v", sorted(y.trace))
	t.Logf("events of the instances: %v", sorted(y.events))
	if len(y.landings) > 0 {
		t.Logf("writes that the store held back, by what became of them: %v", sorted(y.landings))
		t.Logf("seeds with at least one of each: %v", sorted(y.landingSeeds))
	}
	t.Logf("an instance served at most %v after the end of the workload, in seed %v, where %v is the bound", y.serveAfter, y.slowest, w.liveness())
}

// The flags of the test binary that replay one seed of [TestSimulation], see [replaySeed], and of
// [TestTx_Savepoint_random], which reads only -replay.
var (
	replay         = flag.String("replay", "", "replay this one seed in TestSimulation and TestTx_Savepoint_random, and nothing else, and log its trace or its steps")
	replayWorkload = flag.String("workload", "single", "workload of the replay: single, full, hammer, late, or idle")
	replayBug      = flag.String("bug", "", "planted bug whose workload the replay runs")
	replayControl  = flag.Bool("control", false, "replay the control of the bug")
	replayClean    = flag.Bool("clean", false, "replay the workload of the bug without the bug")
)

// TestSimulation runs each workload in a parallel subtest, every seed twice. The hash of all its
// traces that each subtest logs is the same with -parallel 1. With the flag -replay, it replays
// that one seed instead: see [replaySeed].
func TestSimulation(t *testing.T) {
	t.Parallel()
	if *replay != "" {
		replaySeed(t)
		return
	}
	for _, test := range []struct {
		name     string
		workload workload
		// replay is the name of the workload for the flag -workload.
		replay string
		// seeds of the default run.
		seeds int
		// reaches is what the seeds of the default run must reach at least.
		reaches map[string]int64
	}{
		{name: "should give the same trace when a seed is run twice, and no violations, with one instance", workload: singleWorkload, replay: "single", seeds: 40, reaches: singleReaches},
		{name: "should give the same trace when a seed is run twice, and no violations, with instances that compete, clocks that move, crashes, and buggify", workload: fullWorkload, replay: "full", seeds: 30, reaches: fullReaches},
		{name: "should give the same trace when a seed is run twice, and no violations, with callers that never pause", workload: hammerWorkload, replay: "hammer", seeds: 30, reaches: hammerReaches},
		{name: "should give the same trace when a seed is run twice, and no violations, with writes that land long after they were sent", workload: lateWrites(func(w *workload) {}), replay: "late", seeds: 10, reaches: lateReaches},
		{name: "should give the same trace when a seed is run twice, and no violations, with write transactions that sit idle while their instance is cut off", workload: idling(func(w *workload) {}), replay: "idle", seeds: 20, reaches: idleReaches},
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
					t.Errorf("seed %v gave two different traces, at %v\nreplay it with: %v", seed, firstDifference(first.lines, second.lines), replayCommand(seed, "-workload", test.replay))
				}
				if v := first.violations(); len(v) > 0 {
					t.Errorf("seed %v: %v\nreplay it with: %v", seed, strings.Join(v, "; "), replayCommand(seed, "-workload", test.replay))
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
			is.Equal(t, int(n), len(hashes), "two seeds gave the same trace")
			// The seeds are 0 to n-1, so more seeds than the default run reach at least as much.
			if n < uint64(test.seeds) {
				t.Logf("NOT CHECKED: what the workload reached, because %v seeds are fewer than the %v of the default run", n, test.seeds)
				return
			}
			reached := y.reached()
			var short bool
			for _, name := range slices.Sorted(maps.Keys(test.reaches)) {
				if least := test.reaches[name]; reached[name] < least {
					short = true
					t.Errorf("the workload reached %q %v times, and must reach it %v times at least", name, reached[name], least)
				}
			}
			if short {
				// The minimums by the rule above, from this run, for a change that is meant to move them.
				var minimums strings.Builder
				for _, name := range slices.Sorted(maps.Keys(reached)) {
					if reached[name] >= 4 {
						fmt.Fprintf(&minimums, "\t%q: %v,\n", name, reached[name]/4)
					}
				}
				t.Logf("the minimums from this run:\n%v", minimums.String())
			}
			if test.workload.faults.Long > 0 {
				// Writes of both kinds land long after they were sent, after another node took the lease,
				// in at least a quarter of the seeds, and after other nodes took it twice, and after
				// another node committed. A late create of a commit object is applied when its
				// transaction failed, and refused when an attempt after it was applied.
				for _, kind := range []string{"manifest swaps", "commit objects"} {
					for _, what := range []string{"after other nodes took the lease twice", "after a commit of another node", "later than LateBy allows"} {
						is.True(t, y.landingSeeds[kind+" "+what] > 0, "no seed had "+kind+" that landed "+what)
					}
					is.True(t, 4*y.landingSeeds[kind+" after another node took the lease"] >= int(n), "too few seeds had "+kind+" that landed after another node took the lease")
				}
				is.True(t, y.landings["commit objects applied"] > 0 && y.landings["commit objects refused"] > 0, "no late create of a commit object was applied, or none was refused")
			}
		})
	}
}

// replaySeed runs the one seed of the flag -replay, with the workload of the flag -workload, and
// logs its whole trace. The flag -bug names a planted bug by its name in [plantedBugs], to run its
// workload instead, with -control to run the control of that bug, or -clean to run the workload
// without the bug. [replayCommand] gives the command.
func replaySeed(t *testing.T) {
	t.Helper()
	seed, err := strconv.ParseUint(*replay, 10, 64)
	is.NotError(t, err)

	w := singleWorkload
	switch *replayWorkload {
	case "single":
	case "full":
		w = fullWorkload
	case "hammer":
		w = hammerWorkload
	case "late":
		w = lateWrites(func(*workload) {})
	case "idle":
		w = idling(func(*workload) {})
	default:
		t.Fatalf("no workload is named %v", *replayWorkload)
	}
	if name := *replayBug; name != "" {
		i := slices.IndexFunc(plantedBugs, func(b plantedBug) bool { return b.bug == name })
		is.True(t, i >= 0, "no planted bug is named "+name)
		w = plantedBugs[i].workload
		if *replayControl {
			is.True(t, plantedBugs[i].control != nil, "the bug "+name+" has no control")
			w = *plantedBugs[i].control
		}
		if *replayClean {
			w.options = w.options.Unplanted()
		}
	}

	s := simulate(t, seed, w)
	t.Logf("seed %v: %v steps, hash %v, trace:\n%v", seed, s.steps, s.hash(), strings.Join(s.lines, "\n"))
	is.EqualSlice(t, nil, s.violations())
}

// replayCommand is the command that replays the seed with [replaySeed], with the flags that choose
// the workload, such as "-workload", "full", and the race detector or the address sanitizer if the
// test binary has one.
func replayCommand(seed uint64, flags ...string) string {
	var mode string
	if raceEnabled {
		mode = "-race "
	}
	if asanEnabled {
		mode = "-asan "
	}
	return "go test " + mode + "-run '^TestSimulation$' -v ./internal/core/ -replay " + fmt.Sprint(seed) + " " + strings.Join(flags, " ")
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
	w.clocks, w.crashes, w.closes, w.buggify, w.damaged, w.hold = 0, 0, 0, false, 0, 0
	w.think = 6 * time.Second
	w.faults.CutOff, w.faults.CutOffFor = 0.02, 2*time.Minute
	change(&w)
	return w
}

// unknownOutcomes is a workload aimed at the rules of a manifest swap whose outcome is unknown:
// one instance whose callers give up before a request to the store times out, and a store that
// loses many requests and answers, and delivers most of the writes that it lost later.
// idling is the workload of instances that compete for the lease, with an idler on each, whose
// write transactions sit idle while the instance is cut off from the store: see [run.idler].
func idling(change func(w *workload)) workload {
	w := competing(func(w *workload) {})
	w.idlers, w.idle = 1, time.Minute
	change(&w)
	return w
}

func unknownOutcomes(change func(w *workload)) workload {
	w := singleWorkload
	w.ops, w.think, w.timeout = 30, 5*time.Second, 4*time.Second
	w.faults.LostRequest, w.faults.LostResponse, w.faults.Hang = 0.1, 0.1, 0.5
	w.faults.Late, w.faults.LateBy = 0.8, 30*time.Second
	change(&w)
	return w
}

// lateWrites is a workload aimed at writes that land long after they were sent: three instances
// that compete for the lease, with crashes and restarts, instances that are closed and opened
// again, and clients that are cut off for longer than a lease lasts, and a store that holds back
// most of the writes that it loses or whose callers gave up, half of them for up to 15 minutes,
// changed by change. The callers run for about 15 minutes, so that a write of one holder lands
// after others took the lease and committed, both a swap of the manifest and a create of a commit
// object, and [run.verifyLandings] counts how often.
func lateWrites(change func(w *workload)) workload {
	w := competing(func(w *workload) {})
	w.instances, w.crashes, w.closes = 3, 2, 1
	w.ops, w.think, w.timeout = 40, 30*time.Second, 10*time.Second
	w.faults.Slow = 0.05
	w.faults.Late, w.faults.AfterCrash, w.faults.Long, w.faults.LongBy = 0.8, 0.8, 0.5, 15*time.Minute
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
	stale = "which was acknowledged before it started"
	// early: an instance took the lease before the manifest had stood still for long enough.
	early = "takes the lease of a holder"
	// unsettled: a read missed a commit that had returned with an unknown outcome before it started.
	unsettled = "had returned with an unknown outcome before it started"
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
		// enough for the wait to end before a slow answer comes. An instance also learns late of a swap
		// whose answer was lost, which every cut-off can cause, so no fault can be taken away that
		// leaves the takeovers and takes the late answers, and the bug has no control. A workload
		// without takeovers, of one instance, could not find it, so it would be no control. Every
		// seed that finds the bug runs again without it, which shows that it finds nothing else.
		bug:  "leaseFromAnswer",
		name: "a holder that counts its lease from the answer to a swap",
		workload: plant(competing(func(w *workload) {
			w.faults.Slow, w.options.StoreTimeout = 0.1, time.Minute
			w.durations = []time.Duration{10 * time.Second}
		}), "leaseFromAnswer"),
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
		// the answer to a swap is often lost, and a client is often cut off. A lease is lost only to
		// another instance, so a workload of one instance could not find the bug, and would be no
		// control; the bug has none. Every seed that finds it runs again without it.
		bug:  "lostMeansFailed",
		name: "a commit counted as not committed because the lease was lost, without a look at the log",
		workload: plant(competing(func(w *workload) {
			w.writes, w.timeout, w.faults.LostResponse, w.faults.CutOff = 70, 100*time.Second, 0.2, 0.05
		}), "lostMeansFailed"),
		symptoms: []string{"whose commit failed"},
	},
	{
		// Replay keeps a part of a commit object only if the commit object breaks off. A download
		// has five minutes, so that none ends because its time is up. The content of a download also
		// breaks off when its client is cut off or the store is out of reach while it is read, which
		// happens when replay renews the lease between two changes. So the control has neither, and
		// its catch-ups are those after the crash.
		bug:  "replayCommitsEarly",
		name: "a replay that commits before the reader has reported the end",
		workload: plant(competing(func(w *workload) {
			w.faults.BrokenRead, w.options.ObjectTimeout, w.crashes = 0.3, 5*time.Minute, 1
		}), "replayCommitsEarly"),
		control: new(plant(competing(func(w *workload) {
			w.faults.BrokenRead, w.faults.CutOff, w.faults.Outage, w.options.ObjectTimeout, w.crashes = 0, 0, 0, 5*time.Minute, 1
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
		bug:      "blobCut",
		name:     "a commit object that cuts a blob at its first zero byte",
		workload: plant(calm(singleWorkload), "blobCut"),
		// Two keys that the cut makes one, such as x'0000' and x'00', are a key listed twice.
		symptoms: []string{"with the changes", "a read saw at commit", "the cache of", "the replay of the whole log", "is no net effect"},
	},
	{
		bug:      "nullZero",
		name:     "a commit object that writes a NULL as the zero value of its column's type",
		workload: plant(calm(singleWorkload), "nullZero"),
		symptoms: []string{"with the changes", "a read saw at commit", "the cache of"},
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
		// A take is sent from a manifest without a lease entry, whose version comes back whenever a
		// holder releases the lease at the same head, so the callers only read, and two instances
		// close and open again often. A take that is sent again with the same bytes gives the
		// manifest a version that it had before only when its first attempt lands after the next
		// release, which needs a write that is held back for longer than a holder keeps the lease.
		// Over 1000 seeds on 2026-11-13, the workload found the bug in 387, the same workload with
		// writes held back for up to LateBy only, a minute, in 33, and the control in none. The
		// default run found it four times within the first 10 seeds.
		bug:  "resendTake",
		name: "a take of the lease that is sent again with the same bytes",
		workload: plant(lateWrites(func(w *workload) {
			w.instances, w.writes, w.closes, w.faults.LostRequest, w.faults.Long = 2, 0, 30, 0.2, 1
		}), "resendTake"),
		control: new(plant(lateWrites(func(w *workload) {
			w.instances, w.writes, w.closes, w.faults.LostRequest, w.faults.Long = 2, 0, 30, 0.2, 1
			w.faults.Late, w.faults.AfterCrash = 0, 0
		}), "resendTake")),
		symptoms: []string{"a version that it had before"},
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
	{
		bug:      "keepIdleTx",
		name:     "a lease task that leaves an idle write transaction open when the instance has lost its lease",
		workload: plant(idling(func(w *workload) {}), "keepIdleTx"),
		control: new(plant(idling(func(w *workload) {
			w.instances = 1
		}), "keepIdleTx")),
		symptoms: []string{"is open and idle, does not serve"},
	},
}

// TestSimulation_plantedBugs runs the workload of each bug until four seeds have found the bug,
// from at most 200 seeds, and its control for 10 seeds. It runs the same workload without the bug
// for 10 seeds, and for every seed that found the bug. With S4QL_SIM_SEEDS set, all three run that
// many seeds, and the workload with the bug runs all of them, to count how many find it.
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
				is.True(t, known, fmt.Sprintf("seed %v found something else: %v\nreplay it with: %v", seed, first.violations(), replayCommand(seed, "-bug", bug.bug)))
				if len(found) > 2 {
					continue
				}
				// The same seed finds the same violations at the same place in the same trace.
				for range 2 {
					again := simulate(t, seed, bug.workload)
					rerun := fmt.Sprint("seed ", seed, " gave another trace when run again; replay it with: ", replayCommand(seed, "-bug", bug.bug))
					is.Equal(t, first.hash(), again.hash(), rerun)
					is.EqualSlice(t, first.violations(), again.violations(), rerun)
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
			n = seeds(t, 10)
			if bug.control != nil {
				is.True(t, bug.control.options.Planted(), "the control has no planted bug")
				var controlFound int
				for seed := range n {
					if v := simulate(t, seed, *bug.control).violations(); len(v) > 0 {
						controlFound++
						t.Errorf("seed %v found the bug without what it needs: %v\nreplay it with: %v", seed, v, replayCommand(seed, "-bug", bug.bug, "-control"))
					}
				}
				t.Logf("%v of %v seeds found the bug without what it needs", controlFound, n)
			}
			// The workload of a bug reaches what the full workload seldom does, so it must also find
			// nothing in a core without bugs: not in the first seeds, and not in any seed that found the
			// bug, so that what such a seed found is the bug's doing, and not something else whose text
			// happens to hold a symptom of the bug.
			clean := bug.workload
			clean.options = clean.options.Unplanted()
			var cleanSeeds []uint64
			for seed := range n {
				cleanSeeds = append(cleanSeeds, seed)
			}
			for _, seed := range found {
				if seed >= n {
					cleanSeeds = append(cleanSeeds, seed)
				}
			}
			var cleanFound int
			for _, seed := range cleanSeeds {
				if v := simulate(t, seed, clean).violations(); len(v) > 0 {
					cleanFound++
					t.Errorf("seed %v found something without the bug: %v\nreplay it with: %v", seed, v, replayCommand(seed, "-bug", bug.bug, "-clean"))
				}
			}
			t.Logf("%v of %v seeds found something without the bug: the first %v, and every other seed that found the bug", cleanFound, len(cleanSeeds), n)
			switch {
			case !counting:
				is.Equal(t, 4, len(found), fmt.Sprintf("%v seeds did not find the bug four times", ran))
			case ran >= 200:
				is.True(t, len(found) > 0, fmt.Sprintf("none of %v seeds found the bug", ran))
			default:
				t.Logf("NOT CHECKED: that the bug is found, because %v seeds are fewer than 200, in which a bug that is seldom found may not be", ran)
			}
		})
	}
}
