package simstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/store"
	"maragu.dev/s4ql/internal/store/simstore"
)

func TestStore_Object(t *testing.T) {
	t.Run("should agree with what a read through a client returns, for every key and the history", func(t *testing.T) {
		for seed := range uint64(10) {
			var checked int
			p := probe(t, seed, probeOptions{store: simstore.Options{Latency: latency}}, func(ctx context.Context, st *simstore.Store) {
				c := st.Client("checker", nil)
				last := map[string]simstore.Write{}
				for _, w := range st.History() {
					last[w.Key] = w
				}
				var keys []string
				for k := range last {
					keys = append(keys, k)
				}
				slices.Sort(keys)
				is.EqualSlice(t, keys, st.Keys())

				for _, k := range keys {
					content, v, ok := st.Object(k)
					is.True(t, ok)
					is.True(t, bytes.Equal(content, last[k].Content) && v == last[k].Version, "the object is not the last write of its key")
					body, got, err := c.Get(ctx, k)
					is.NotError(t, err)
					data, err := io.ReadAll(body)
					is.NotError(t, err)
					is.True(t, bytes.Equal(data, content) && got == v, "a read does not give the object")
					checked++
				}
				_, _, ok := st.Object("missing")
				is.True(t, !ok)
			})
			is.NotError(t, p.err)
			is.True(t, checked > 1)
		}
	})

	t.Run("should not let a caller change what it holds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(0, sim.Options{})
			is.NotError(t, s.Run(func(ctx context.Context) {
				st := simstore.New(s, simstore.Options{})
				content := []byte("data")
				_, err := st.Client("a", nil).Create(ctx, "k", bytes.NewReader(content), nil)
				is.NotError(t, err)
				content[0] = 'X'
				got, _, _ := st.Object("k")
				got[1] = 'X'
				st.History()[0].Content[2] = 'X'
				got, _, _ = st.Object("k")
				is.Equal(t, "data", string(got))
				is.Equal(t, "data", string(st.History()[0].Content))
			}))
		})
	})
}

func TestStore_History(t *testing.T) {
	t.Run("should give the same content the same version at every key, and other content another", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(0, sim.Options{})
			is.NotError(t, s.Run(func(ctx context.Context) {
				st := simstore.New(s, simstore.Options{Latency: latency})
				c := st.Client("a", nil)
				one, err := c.Create(ctx, "one", strings.NewReader("same"), nil)
				is.NotError(t, err)
				two, err := c.Create(ctx, "two", strings.NewReader("same"), nil)
				is.NotError(t, err)
				is.Equal(t, one, two)

				// A swap that writes the bytes that are there leaves the version, and a swap from it still
				// succeeds afterwards.
				again, err := c.Swap(ctx, "one", one, strings.NewReader("same"), nil)
				is.NotError(t, err)
				is.Equal(t, one, again)
				other, err := c.Swap(ctx, "one", one, strings.NewReader("other"), nil)
				is.NotError(t, err)
				is.True(t, other != one)

				history := st.History()
				is.Equal(t, 4, len(history))
				is.True(t, history[0].Create && history[1].Create && !history[2].Create && !history[3].Create)
				is.Equal(t, one, history[3].From)
				is.True(t, slices.IsSortedFunc(history, func(a, b simstore.Write) int { return int(a.Time - b.Time) }))
			}))
		})
	})

	t.Run("should give versions that differ between stores with different seeds", func(t *testing.T) {
		versions := map[store.Version]bool{}
		for seed := range uint64(5) {
			synctest.Test(t, func(t *testing.T) {
				s := sim.New(seed, sim.Options{})
				is.NotError(t, s.Run(func(ctx context.Context) {
					v, err := simstore.New(s, simstore.Options{}).Client("a", nil).Create(ctx, "k", strings.NewReader("same"), nil)
					is.NotError(t, err)
					versions[v] = true
				}))
			})
		}
		// So nothing outside the store can compute a version from the content.
		is.Equal(t, 5, len(versions))
	})
}

func TestStore_Wait(t *testing.T) {
	t.Run("should wait for a write that is applied long after its caller gave up", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(0, sim.Options{})
			is.NotError(t, s.Run(func(ctx context.Context) {
				st := simstore.New(s, simstore.Options{Latency: latency, Faults: simstore.Faults{Late: 1, LateBy: time.Hour}})
				// The caller gives up while the write is on its way, before it can arrive.
				short, cancel := s.WithTimeout(ctx, time.Nanosecond)
				_, err := st.Client("a", nil).Create(short, "k", strings.NewReader("late"), nil)
				cancel()
				is.True(t, errors.Is(err, context.DeadlineExceeded))
				gaveUp := s.Monotonic()
				is.Equal(t, 0, len(st.History()))

				is.NotError(t, st.Wait(ctx))
				history := st.History()
				is.Equal(t, 1, len(history))
				is.True(t, history[0].Time > gaveUp, "the write was applied before its caller gave up")
				is.True(t, s.Monotonic() >= history[0].Time)
				_, _, ok := st.Object("k")
				is.True(t, ok)
			}))
		})
	})
}

func TestStore_Outage(t *testing.T) {
	t.Run("should let no request of any client through while the store is unreachable, and all after", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(0, sim.Options{})
			is.NotError(t, s.Run(func(ctx context.Context) {
				st := simstore.New(s, simstore.Options{Latency: latency})
				a, b := st.Client("a", nil), st.Client("b", nil)
				st.Outage(ctx, time.Minute)
				for _, c := range []*simstore.Client{a, b} {
					_, err := c.Create(ctx, "k", strings.NewReader("data"), nil)
					is.True(t, errors.Is(err, simstore.ErrFault), fmt.Sprint(err))
				}
				is.Equal(t, 0, len(st.History()))

				is.NotError(t, s.Sleep(ctx, time.Minute))
				for _, c := range []*simstore.Client{a, b} {
					_, err := c.Create(ctx, "k", strings.NewReader("data"), nil)
					is.True(t, err == nil || errors.Is(err, store.ErrExists), fmt.Sprint(err))
				}
				is.Equal(t, 1, len(st.History()))
			}))
		})
	})
}

// The workload: several instances whose callers swap a counter object, create objects named by their
// content, and read, against a store with every fault on, and a crash of one instance after which
// another instance joins. A model checks everything that the callers and the history saw.

// attempt of a write, as its caller saw it.
type attempt struct {
	client  string
	key     string
	create  bool
	from    store.Version
	content string
	// wrongChecksum is set for a create sent with a checksum of other content.
	wrongChecksum bool
	version       store.Version
	err           error
	// done is set when the call has returned, which it never does if its instance crashed first.
	done bool
	end  time.Duration
}

// definite reports whether the error of a is one that says the write was not applied.
func (a *attempt) definite() bool {
	return errors.Is(a.err, store.ErrNotFound) || errors.Is(a.err, store.ErrExists) ||
		errors.Is(a.err, store.ErrChanged) || errors.Is(a.err, store.ErrChecksum)
}

// read, as its caller saw it.
type read struct {
	key     string
	content []byte
	version store.Version
	err     error
	readErr error
	// before and after are the number of writes that the store had applied when the read was sent
	// and when its content was read.
	before, after int
	// done is set when the read has ended, which it never does if its instance crashed first.
	done bool
}

// world of one run of the workload.
type world struct {
	s  *sim.Scheduler
	st *simstore.Store

	mu        sync.Mutex
	instances map[string]*sim.Instance
	attempts  []*attempt
	reads     []*read
	// objects whose create was acknowledged, by key.
	objects    []string
	violations []string
	// crashed is the time at which an instance crashed, and victim its name.
	crashed time.Duration
	victim  string
}

func (w *world) violate(ctx context.Context, format string, args ...any) {
	w.s.Trace(ctx, "VIOLATION: "+format, args...)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.violations = append(w.violations, fmt.Sprintf(format, args...))
}

// expectedErr reports whether err of a call is one that the store may return in the workload.
func expectedErr(err error) bool {
	return err == nil || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrExists) || errors.Is(err, store.ErrChanged) ||
		errors.Is(err, store.ErrChecksum) || errors.Is(err, simstore.ErrFault) || errors.Is(err, context.DeadlineExceeded)
}

// record an attempt before it is sent.
func (w *world) record(a *attempt) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.attempts = append(w.attempts, a)
}

// returned marks a as returned with v and err.
func (w *world) returned(a *attempt, v store.Version, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a.version, a.err, a.done, a.end = v, err, true, w.s.Monotonic()
}

// caller runs ops operations with the client c of the instance inst, whose name is name.
func (w *world) caller(ctx context.Context, inst *sim.Instance, c *simstore.Client, name string, ops int) {
	for i := range ops {
		_ = inst.Sleep(ctx, time.Duration(intn(inst, 3000))*time.Millisecond)
		timeout := 5 * time.Minute
		if intn(inst, 2) == 0 {
			timeout = time.Duration(intn(inst, 2000)) * time.Millisecond
		}
		ctx, cancel := inst.WithTimeout(ctx, timeout)
		id := fmt.Sprint(name, "-", i)
		switch n := intn(inst, 10); {
		case n < 4:
			w.swapCounter(ctx, c, name, id)
		case n < 7:
			w.createObject(ctx, inst, c, name, id)
		default:
			w.read(ctx, inst, c)
		}
		cancel()
	}
}

// swapCounter reads the counter and swaps it for one more, or creates it.
func (w *world) swapCounter(ctx context.Context, c *simstore.Client, name, id string) {
	body, v, err := c.Get(ctx, "counter")
	if errors.Is(err, store.ErrNotFound) {
		a := &attempt{client: name, key: "counter", create: true, content: "1 " + id}
		w.record(a)
		v, err := c.Create(ctx, a.key, strings.NewReader(a.content), nil)
		w.returned(a, v, err)
		return
	}
	if err != nil {
		return
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		return
	}
	n, err := strconv.Atoi(strings.Fields(string(data))[0])
	if err != nil {
		w.violate(ctx, "the counter is %q", data)
		return
	}
	a := &attempt{client: name, key: "counter", from: v, content: fmt.Sprint(n+1, " ", id)}
	w.record(a)
	next, err := c.Swap(ctx, a.key, v, strings.NewReader(a.content), nil)
	w.returned(a, next, err)
}

// createObject named by its content, which it creates again while the outcome is unknown, as the
// upload of a commit object does. One in ten is sent with a checksum of other content, once.
func (w *world) createObject(ctx context.Context, inst *sim.Instance, c *simstore.Client, name, id string) {
	content := "object " + id + " " + strings.Repeat("x", intn(inst, 2000))
	sum := sha256.Sum256([]byte(content))
	key := "objects/" + hex.EncodeToString(sum[:])
	wrong := intn(inst, 10) == 0
	if wrong {
		sum = sha256.Sum256([]byte("other content"))
	}
	for range 3 {
		a := &attempt{client: name, key: key, create: true, content: content, wrongChecksum: wrong}
		w.record(a)
		v, err := c.Create(ctx, key, strings.NewReader(content), &sum)
		w.returned(a, v, err)
		if err == nil {
			w.mu.Lock()
			w.objects = append(w.objects, key)
			w.mu.Unlock()
		}
		if err == nil || a.definite() || wrong || ctx.Err() != nil {
			return
		}
	}
}

// read the counter or an object.
func (w *world) read(ctx context.Context, inst *sim.Instance, c *simstore.Client) {
	r := &read{key: "counter"}
	w.mu.Lock()
	if len(w.objects) > 0 && intn(inst, 2) == 0 {
		r.key = w.objects[intn(inst, len(w.objects))]
	}
	w.reads = append(w.reads, r)
	w.mu.Unlock()

	before := len(w.st.History())
	body, v, err := c.Get(ctx, r.key)
	var content []byte
	var readErr error
	if err == nil {
		content, readErr = io.ReadAll(body)
		_ = body.Close()
	}
	after := len(w.st.History())
	w.mu.Lock()
	defer w.mu.Unlock()
	r.version, r.err, r.content, r.readErr, r.before, r.after, r.done = v, err, content, readErr, before, after, true
}

// run the workload as the task of ctx.
func (w *world) run(ctx context.Context) {
	const ops = 15
	var tasks []host.Task
	start := func(name string) {
		inst := w.s.NewInstance(name)
		w.mu.Lock()
		w.instances[name] = inst
		w.mu.Unlock()
		c := w.st.Client(name, inst)
		for i := range 2 {
			tasks = append(tasks, inst.Go(ctx, fmt.Sprint("caller-", i), func(ctx context.Context) {
				w.caller(ctx, inst, c, name, ops)
			}))
		}
	}
	instances := []string{"a", "b", "c"}
	for _, name := range instances {
		start(name)
	}
	tasks = append(tasks, w.s.Go(ctx, "crasher", func(ctx context.Context) {
		_ = w.s.Sleep(ctx, time.Duration(intn(w.s, 30_000))*time.Millisecond)
		victim := instances[intn(w.s, len(instances))]
		w.mu.Lock()
		w.crashed, w.victim = w.s.Monotonic(), victim
		inst := w.instances[victim]
		w.mu.Unlock()
		inst.Crash()
		start("d")
	}))

	// Tasks are added while the first ones run, by the crasher.
	for i := 0; i < len(tasks); i++ {
		_ = tasks[i].Wait(ctx)
	}
	if err := w.st.Wait(ctx); err != nil {
		w.violate(ctx, "wait for the store: %v", err)
	}
	w.check(ctx)
}

// check what the callers and the history saw against a model of the store.
func (w *world) check(ctx context.Context) {
	w.mu.Lock()
	attempts, reads := slices.Clone(w.attempts), slices.Clone(w.reads)
	w.mu.Unlock()
	history := w.st.History()

	byContent := map[string][]*attempt{}
	for _, a := range attempts {
		byContent[a.content] = append(byContent[a.content], a)
		if a.done && !expectedErr(a.err) {
			w.violate(ctx, "a write of %v returned %v", a.content, a.err)
		}
		switch {
		case a.done && a.wrongChecksum && (a.err == nil || errors.Is(a.err, store.ErrExists)):
			w.violate(ctx, "a create with a wrong checksum of %v returned %v", a.key, a.err)
		case !a.wrongChecksum && errors.Is(a.err, store.ErrChecksum):
			w.violate(ctx, "a create with its own checksum of %v returned %v", a.key, a.err)
		}
	}

	// Replay the history on a model, which every write must fit, in the order the store applied
	// them, and record the state of every key after each prefix of the history.
	type state struct {
		content []byte
		version store.Version
	}
	model := map[string]state{}
	// states[k][key] is the state of key after k writes, if it changed at write k, or the state
	// before.
	states := []map[string]state{{}}
	versions := map[string]store.Version{}
	written := map[string][]simstore.Write{}
	for i, wr := range history {
		as := byContent[string(wr.Content)]
		if len(as) == 0 || as[0].key != wr.Key || as[0].create != wr.Create {
			w.violate(ctx, "write %v of %v to %v was not attempted", i, string(wr.Content), wr.Key)
		}
		written[string(wr.Content)] = append(written[string(wr.Content)], wr)

		old, exists := model[wr.Key]
		switch {
		case wr.Create && exists:
			w.violate(ctx, "write %v creates %v, where an object is", i, wr.Key)
		case !wr.Create && !exists:
			w.violate(ctx, "write %v swaps %v, where no object is", i, wr.Key)
		case !wr.Create && old.version != wr.From:
			w.violate(ctx, "write %v swaps %v from %v, and its version was %v", i, wr.Key, wr.From, old.version)
		}
		if v, ok := versions[string(wr.Content)]; ok && v != wr.Version {
			w.violate(ctx, "the same content has the versions %v and %v", v, wr.Version)
		}
		versions[string(wr.Content)] = wr.Version
		model[wr.Key] = state{content: wr.Content, version: wr.Version}
		states = append(states, map[string]state{wr.Key: model[wr.Key]})

		if wr.Key == "counter" {
			n, _ := strconv.Atoi(strings.Fields(string(wr.Content))[0])
			if want := 1; !wr.Create {
				prev, _ := strconv.Atoi(strings.Fields(string(old.content))[0])
				want = prev + 1
				if n != want {
					w.violate(ctx, "write %v makes the counter %v, after %v", i, n, prev)
				}
			} else if n != want {
				w.violate(ctx, "write %v creates the counter at %v", i, n)
			}
		}
	}
	distinct := map[store.Version]string{}
	for content, v := range versions {
		if other, ok := distinct[v]; ok {
			w.violate(ctx, "the contents %q and %q have the same version %v", other, content, v)
		}
		distinct[v] = content
	}

	// Each write of a content belongs to an attempt that may have applied it, and an attempt that was
	// acknowledged was applied exactly once, with the version it returned.
	for content, as := range byContent {
		ws := written[content]
		var possible, acked int
		for _, a := range as {
			if !a.definite() {
				possible++
			}
			if a.done && a.err == nil {
				acked++
				if len(ws) != 1 || ws[0].Version != a.version || ws[0].From != a.from {
					w.violate(ctx, "the acknowledged write of %v to %v is written %v times", content, a.key, len(ws))
				}
			}
			if errors.Is(a.err, store.ErrExists) && !a.create {
				w.violate(ctx, "a swap returned ErrExists")
			}
			// Only the caller of an object creates it, so an object exists only if an earlier attempt
			// of the caller wrote it.
			if errors.Is(a.err, store.ErrExists) && a.key != "counter" && (len(ws) == 0 || ws[0].Time > a.end) {
				w.violate(ctx, "a create of %v returned ErrExists, and no earlier attempt wrote it", a.key)
			}
		}
		if len(ws) > possible || len(ws) > 1 {
			w.violate(ctx, "%v is written %v times by %v attempts that may have written it", content, len(ws), possible)
		}
		if acked > 1 {
			w.violate(ctx, "%v was acknowledged %v times", content, acked)
		}
	}

	// The store holds what the model holds.
	var keys []string
	for k, want := range model {
		keys = append(keys, k)
		content, v, ok := w.st.Object(k)
		if !ok || !bytes.Equal(content, want.content) || v != want.version {
			w.violate(ctx, "the store holds %v at %v, and the history leads to %v", v, k, want.version)
		}
	}
	slices.Sort(keys)
	if !slices.Equal(keys, w.st.Keys()) {
		w.violate(ctx, "the store has the keys %v, and the history %v", w.st.Keys(), keys)
	}

	// A read gives the state of its key at some instant while it ran, or the start of it, if its
	// content broke off.
	for _, r := range reads {
		if !r.done {
			continue
		}
		if !expectedErr(r.err) || !expectedErr(r.readErr) {
			w.violate(ctx, "a read of %v returned %v %v", r.key, r.err, r.readErr)
		}
		if r.err != nil && !errors.Is(r.err, store.ErrNotFound) {
			continue
		}
		var ok bool
		var current state
		var exists bool
		for k := 0; k <= r.after && !ok; k++ {
			if s, changed := states[k][r.key]; changed {
				current, exists = s, true
			}
			if k < r.before {
				continue
			}
			switch {
			case errors.Is(r.err, store.ErrNotFound):
				ok = !exists
			case r.readErr != nil:
				ok = exists && r.version == current.version && bytes.HasPrefix(current.content, r.content)
			default:
				ok = exists && r.version == current.version && bytes.Equal(current.content, r.content)
			}
		}
		if !ok {
			w.violate(ctx, "a read of %v gave %v %v between writes %v and %v, which no state then had", r.key, r.version, r.err, r.before, r.after)
		}
	}
}

// workloadRun is what one run of the workload saw.
type workloadRun struct {
	err        error
	lines      []string
	draw       uint64
	violations []string
	attempts   []*attempt
	reads      []*read
	history    []simstore.Write
}

// hash of the trace and the next random number.
func (r workloadRun) hash() string {
	sum := sha256.Sum256([]byte(strings.Join(r.lines, "\n") + fmt.Sprint("\n", r.draw)))
	return hex.EncodeToString(sum[:])
}

// count of the lines of the trace that contain substring.
func (r workloadRun) count(substring string) int {
	var n int
	for _, line := range r.lines {
		if strings.Contains(line, substring) {
			n++
		}
	}
	return n
}

// runWorkload under the scheduler with seed, with the faults f.
func runWorkload(t *testing.T, seed uint64, f simstore.Faults) workloadRun {
	t.Helper()
	var r workloadRun
	synctest.Test(t, func(t *testing.T) {
		s := sim.New(seed, sim.Options{Jitter: 50 * time.Millisecond})
		w := &world{s: s, crashed: -1, instances: map[string]*sim.Instance{}}
		r.err = s.Run(func(ctx context.Context) {
			w.st = simstore.New(s, simstore.Options{Latency: 200 * time.Millisecond, Faults: f})
			w.run(ctx)
		})
		r.lines, r.draw = s.Lines(), s.Uint64()
		r.violations, r.attempts, r.reads, r.history = w.violations, w.attempts, w.reads, w.st.History()
	})
	return r
}

// seeds to run, from 0: n, unless the environment variable S4QL_SIM_SEEDS sets another number.
func seeds(t *testing.T, n int) uint64 {
	t.Helper()
	if v := os.Getenv("S4QL_SIM_SEEDS"); v != "" {
		var err error
		n, err = strconv.Atoi(v)
		is.NotError(t, err)
	}
	return uint64(n)
}

func TestStore_workload(t *testing.T) {
	t.Run("should give the same trace when a seed is run twice, and no violation, with every fault on and a crash", func(t *testing.T) {
		n := seeds(t, 200)
		var differing int
		hashes := map[string]bool{}
		var acked, definite, unknown, cut, writes, reads, brokenReads, late, afterCrash, gaveUp int
		start := time.Now()
		for seed := range n {
			first, second := runWorkload(t, seed, simstore.AllFaults()), runWorkload(t, seed, simstore.AllFaults())
			if first.hash() != second.hash() {
				differing++
				t.Errorf("seed %v gave two different traces", seed)
			}
			if first.err != nil {
				t.Errorf("seed %v: %v", seed, first.err)
			}
			if len(first.violations) > 0 {
				t.Errorf("seed %v: %v", seed, strings.Join(first.violations, "; "))
			}
			hashes[first.hash()] = true
			for _, a := range first.attempts {
				switch {
				case !a.done:
					cut++
				case a.err == nil:
					acked++
				case a.definite():
					definite++
				default:
					unknown++
				}
			}
			for _, r := range first.reads {
				reads++
				if r.readErr != nil {
					brokenReads++
				}
			}
			writes += len(first.history)
			late += first.count("the caller gave up, and is applied")
			afterCrash += first.count("the sender crashed, and is applied")
			gaveUp += first.count("the caller gives up")
		}
		t.Logf("%v seeds, each run twice: %v pairs differed; %v distinct traces", n, differing, len(hashes))
		t.Logf("in the first run of each seed: %v writes attempted: %v acknowledged, %v failed for sure, %v of unknown outcome, %v cut off by the crash; %v writes applied",
			acked+definite+unknown+cut, acked, definite, unknown, cut, writes)
		t.Logf("%v reads, of which %v broke off; %v calls given up, %v writes applied after their caller gave up, %v after their sender crashed",
			reads, brokenReads, gaveUp, late, afterCrash)
		t.Logf("%v runs in %v", 2*n, time.Since(start).Round(time.Millisecond))
		is.Equal(t, 0, differing)
		if n >= 100 {
			is.True(t, len(hashes) == int(n), "two seeds gave the same trace")
			is.True(t, acked > 0 && definite > 0 && unknown > 0 && cut > 0 && brokenReads > 0 && late > 0 && afterCrash > 0, "the workload did not reach every kind of outcome")
		}
	})
}
