package simstore_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
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
	"maragu.dev/s4ql/internal/store/storetest"
)

// harness for the conformance suite: each test runs once for each of seeds seeds, under the
// scheduler, against a calm store with latency, so that racing requests overlap.
func harness(seeds int) storetest.Harness {
	return func(t *testing.T, f func(t *testing.T, ctx context.Context, env storetest.Env)) {
		for seed := range uint64(seeds) {
			synctest.Test(t, func(t *testing.T) {
				s := sim.New(seed, sim.Options{Jitter: 10 * time.Millisecond})
				st := simstore.New(s, simstore.Options{Latency: 50 * time.Millisecond})
				err := s.Run(func(ctx context.Context) {
					f(t, ctx, storetest.Env{
						Host:   s,
						Client: func(name string) store.Store { return st.Client(name, nil) },
					})
				})
				is.NotError(t, err)
			})
		}
	}
}

func TestClient(t *testing.T) {
	storetest.Test(t, harness(20))
}

func TestClient_CutOff(t *testing.T) {
	t.Run("should let no request of the client through while it is cut off, and requests of others", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(0, sim.Options{})
			is.NotError(t, s.Run(func(ctx context.Context) {
				st := simstore.New(s, simstore.Options{Latency: latency})
				a, b := st.Client("a", nil), st.Client("b", nil)
				a.CutOff(ctx, time.Minute)
				_, err := a.Create(ctx, "a", strings.NewReader("data"), nil)
				is.True(t, errors.Is(err, simstore.ErrFault), fmt.Sprint(err))
				_, _, err = a.Get(ctx, "a")
				is.True(t, errors.Is(err, simstore.ErrFault), fmt.Sprint(err))
				_, err = b.Create(ctx, "b", strings.NewReader("data"), nil)
				is.NotError(t, err)
				is.EqualSlice(t, []string{"b"}, st.Keys())

				is.NotError(t, s.Sleep(ctx, time.Minute))
				_, err = a.Create(ctx, "a", strings.NewReader("data"), nil)
				is.NotError(t, err)
			}))
		})
	})

	t.Run("should lose the response of a write that arrived before the client was cut off", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(0, sim.Options{})
			is.NotError(t, s.Run(func(ctx context.Context) {
				st := simstore.New(s, simstore.Options{Latency: latency})
				a := st.Client("a", nil)
				var err error
				task := s.Go(ctx, "writer", func(ctx context.Context) {
					_, err = a.Create(ctx, "k", strings.NewReader("data"), nil)
				})
				// Cut the client off once the write is applied, while its response is on the way.
				for len(st.History()) == 0 {
					is.NotError(t, s.Sleep(ctx, time.Millisecond))
				}
				a.CutOff(ctx, time.Hour)
				is.NotError(t, task.Wait(ctx))
				is.True(t, errors.Is(err, simstore.ErrFault), fmt.Sprint(err))
				is.Equal(t, 1, len(st.History()))
			}))
		})
	})
}

func TestClient_Get(t *testing.T) {
	t.Run("should fail to read content after the context of the read is done", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(0, sim.Options{})
			is.NotError(t, s.Run(func(ctx context.Context) {
				st := simstore.New(s, simstore.Options{Latency: latency})
				c := st.Client("a", nil)
				_, err := c.Create(ctx, "k", strings.NewReader("data"), nil)
				is.NotError(t, err)
				readCtx, cancel := s.WithTimeout(ctx, time.Hour)
				body, _, err := c.Get(readCtx, "k")
				is.NotError(t, err)
				cancel()
				_, err = io.ReadAll(body)
				is.True(t, errors.Is(err, context.Canceled), fmt.Sprint(err))
			}))
		})
	})

	t.Run("should break off content at its start, in the middle, and at its end, under the fault", func(t *testing.T) {
		const size = 100
		var start, middle, end int
		for seed := range uint64(20) {
			synctest.Test(t, func(t *testing.T) {
				s := sim.New(seed, sim.Options{})
				is.NotError(t, s.Run(func(ctx context.Context) {
					st := simstore.New(s, simstore.Options{Faults: simstore.Faults{BrokenRead: 1}})
					c := st.Client("a", nil)
					content := strings.Repeat("x", size)
					_, err := c.Create(ctx, "k", strings.NewReader(content), nil)
					is.NotError(t, err)
					body, _, err := c.Get(ctx, "k")
					is.NotError(t, err)
					data, err := io.ReadAll(body)
					is.True(t, errors.Is(err, simstore.ErrFault), fmt.Sprint(err))
					is.Equal(t, content[:len(data)], string(data))
					switch len(data) {
					case 0:
						start++
					case size:
						end++
					default:
						middle++
					}
				}))
			})
		}
		is.True(t, middle > 0 && start+end < 20, fmt.Sprint(start, " ", middle, " ", end))
	})
}

func TestClient_Create(t *testing.T) {
	t.Run("should not send a request whose context is done", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(0, sim.Options{})
			is.NotError(t, s.Run(func(ctx context.Context) {
				st := simstore.New(s, simstore.Options{Latency: latency, Faults: simstore.Faults{Late: 1}})
				done, cancel := s.WithTimeout(ctx, time.Hour)
				cancel()
				_, err := st.Client("a", nil).Create(done, "k", strings.NewReader("data"), nil)
				is.True(t, errors.Is(err, context.Canceled), fmt.Sprint(err))
				is.NotError(t, st.Wait(ctx))
				is.Equal(t, 0, len(st.History()))
			}))
		})
	})
}

// latency of the store in a probe.
const latency = time.Second

// op that a caller of a probe made, as the caller saw it.
type op struct {
	client string
	// kind is get, create, or swap.
	kind    string
	key     string
	content string
	err     error
	// readErr is the error of reading the content of a get.
	readErr error
	// gaveUp is set if the timeout of the call ended it.
	gaveUp     bool
	start, end time.Duration
	// done is set when the call has returned, which it never does if its instance crashed first.
	done bool
}

// probed is what a probe saw.
type probed struct {
	err     error
	lines   []string
	ops     []*op
	history []simstore.Write
	// crashed is the time at which instance c crashed, or -1.
	crashed time.Duration
}

// trace reports whether a line of the trace contains marker.
func (p probed) trace(marker string) bool {
	return slices.ContainsFunc(p.lines, func(line string) bool { return strings.Contains(line, marker) })
}

// written is the write of the content of o, if the store applied it.
func (p probed) written(o *op) (simstore.Write, bool) {
	i := slices.IndexFunc(p.history, func(w simstore.Write) bool { return string(w.Content) == o.content })
	if i < 0 || o.kind == "get" {
		return simstore.Write{}, false
	}
	return p.history[i], true
}

var (
	cutOffLine = regexp.MustCompile(`^(\S+) .*store: client (\S+) cut off for (\S+)$`)
	outageLine = regexp.MustCompile(`^(\S+) .*store: unreachable for (\S+)$`)
)

// breach of a cut-off or an outage, from the windows of them in the trace: a write of a client that
// the store applied while the client could not reach it, or a call of the client that succeeded
// while it could not. It is empty if there is none.
func (p probed) breach() string {
	// windows of each client, by its name, and of every client, by the empty name.
	windows := map[string][][2]time.Duration{}
	for _, line := range p.lines {
		var at, d time.Duration
		var client string
		var err error
		if m := cutOffLine.FindStringSubmatch(line); m != nil {
			client = m[2]
			if at, err = time.ParseDuration(m[1]); err == nil {
				d, err = time.ParseDuration(m[3])
			}
		} else if m := outageLine.FindStringSubmatch(line); m != nil {
			if at, err = time.ParseDuration(m[1]); err == nil {
				d, err = time.ParseDuration(m[2])
			}
		} else {
			continue
		}
		if err != nil {
			return fmt.Sprintf("the line %q: %v", line, err)
		}
		windows[client] = append(windows[client], [2]time.Duration{at, at + d})
	}
	cut := func(client string, at time.Duration) bool {
		return slices.ContainsFunc(append(windows[client], windows[""]...), func(w [2]time.Duration) bool {
			return at > w[0] && at < w[1]
		})
	}
	for _, w := range p.history {
		if cut(w.Client, w.Time) {
			return fmt.Sprintf("request %v of %v was applied at %v, while the client was cut off", w.Request, w.Client, w.Time)
		}
	}
	for _, o := range p.ops {
		if o.done && o.err == nil && o.readErr == nil && cut(o.client, o.end) {
			return fmt.Sprintf("a %v of %v by %v succeeded at %v, while the client was cut off", o.kind, o.key, o.client, o.end)
		}
	}
	return ""
}

// any reports whether some op is one for which f is true.
func (p probed) any(f func(o *op) bool) bool {
	return slices.ContainsFunc(p.ops, f)
}

// probeOptions of a probe.
type probeOptions struct {
	store simstore.Options
	// crash instance c at a random time.
	crash bool
	// impatient gives half of the calls a timeout so short that they often give up.
	impatient bool
}

// probe the store with three instances, a, b, and c, whose callers create objects, swap a counter,
// and read, with seed. After the callers, and every request, are done, it calls after, if not nil,
// as a task of the test.
func probe(t *testing.T, seed uint64, options probeOptions, after func(ctx context.Context, st *simstore.Store)) probed {
	t.Helper()
	p := probed{crashed: -1}
	synctest.Test(t, func(t *testing.T) {
		s := sim.New(seed, sim.Options{Jitter: 10 * time.Millisecond})
		var mu sync.Mutex
		p.err = s.Run(func(ctx context.Context) {
			st := simstore.New(s, options.store)
			var tasks []host.Task
			instances := map[string]*sim.Instance{}
			for _, name := range []string{"a", "b", "c"} {
				inst := s.NewInstance(name)
				instances[name] = inst
				c := st.Client(name, inst)
				tasks = append(tasks, inst.Go(ctx, "caller", func(ctx context.Context) {
					for i := range 25 {
						timeout := 10 * time.Minute
						if options.impatient && intn(s, 2) == 0 {
							timeout = time.Duration(intn(s, int(2*latency/time.Millisecond))) * time.Millisecond
						}
						ctx, cancel := inst.WithTimeout(ctx, timeout)
						o := &op{client: name, content: fmt.Sprint(name, "-", i), start: s.Monotonic()}
						mu.Lock()
						p.ops = append(p.ops, o)
						mu.Unlock()
						call(ctx, s, c, o, i)
						cancel()
						mu.Lock()
						o.end, o.done = s.Monotonic(), true
						o.gaveUp = errors.Is(o.err, context.DeadlineExceeded) || errors.Is(o.readErr, context.DeadlineExceeded)
						mu.Unlock()
					}
				}))
			}
			if options.crash {
				tasks = append(tasks, s.Go(ctx, "crasher", func(ctx context.Context) {
					_ = s.Sleep(ctx, time.Duration(intn(s, 15_000))*time.Millisecond)
					instances["c"].Crash()
					mu.Lock()
					p.crashed = s.Monotonic()
					mu.Unlock()
				}))
			}
			for _, task := range tasks {
				_ = task.Wait(ctx)
			}
			if err := st.Wait(ctx); err != nil {
				t.Errorf("wait for the store: %v", err)
			}
			p.history = st.History()
			if after != nil {
				after(ctx, st)
			}
		})
		p.lines = s.Lines()
	})
	return p
}

// call the store with c for the op o, the ith of its caller: create an object of its own, swap the
// counter, or read the counter or an object.
func call(ctx context.Context, s *sim.Scheduler, c *simstore.Client, o *op, i int) {
	switch intn(s, 3) {
	case 0:
		o.kind, o.key = "create", "objects/"+o.content
		_, o.err = c.Create(ctx, o.key, strings.NewReader(o.content), new(sha256.Sum256([]byte(o.content))))
	case 1:
		o.key = "counter"
		body, v, err := c.Get(ctx, o.key)
		switch {
		case errors.Is(err, store.ErrNotFound):
			o.kind = "create"
			_, o.err = c.Create(ctx, o.key, strings.NewReader(o.content), nil)
		case err != nil:
			o.kind, o.err = "get", err
		default:
			_ = body.Close()
			o.kind = "swap"
			_, o.err = c.Swap(ctx, o.key, v, strings.NewReader(o.content), nil)
		}
	default:
		o.kind, o.key = "get", "counter"
		if i > 0 && intn(s, 2) == 0 {
			o.key = fmt.Sprint("objects/", o.client, "-", intn(s, i))
		}
		body, _, err := c.Get(ctx, o.key)
		if o.err = err; err == nil {
			_, o.readErr = io.ReadAll(body)
			_ = body.Close()
		}
	}
}

// intn is a random number in [0, n) from h.
func intn(h host.Host, n int) int {
	return int(h.Uint64() % uint64(n))
}

// faultsOn has every fault on, often.
func faultsOn() simstore.Faults {
	return simstore.Faults{
		LostRequest:  0.3,
		LostResponse: 0.3,
		Slow:         0.3,
		SlowBy:       30 * time.Second,
		BrokenRead:   0.3,
		CutOff:       0.1,
		CutOffFor:    10 * time.Second,
		Outage:       0.05,
		OutageFor:    10 * time.Second,
		Late:         0.8,
		AfterCrash:   0.8,
		LateBy:       30 * time.Second,
	}
}

func TestClient_faults(t *testing.T) {
	// longest is the longest that an op takes without a slow request: two requests, each both ways
	// and the start of the task that carries it.
	longest := 4*latency + 100*time.Millisecond

	tests := []struct {
		name string
		// off switches the fault off.
		off func(f *simstore.Faults)
		// marker is what the trace says when the fault happens.
		marker string
		// seen reports whether the fault happened, by what the callers and the history saw, in a run
		// with no other fault.
		seen func(p probed) bool
		// never reports whether the fault did not happen, by what the callers and the history saw, in
		// a run with every other fault.
		never func(p probed) bool
	}{
		{
			name:   "a request that is lost on its way",
			off:    func(f *simstore.Faults) { f.LostRequest = 0 },
			marker: "was lost on its way to the store",
			seen: func(p probed) bool {
				return p.any(func(o *op) bool {
					_, written := p.written(o)
					return o.kind != "get" && errors.Is(o.err, simstore.ErrFault) && !written
				})
			},
			never: func(p probed) bool {
				return !p.any(func(o *op) bool { return o.err != nil && strings.Contains(o.err.Error(), "on its way to the store") })
			},
		},
		{
			name:   "a response that is lost on its way back",
			off:    func(f *simstore.Faults) { f.LostResponse = 0 },
			marker: "was lost on its way back",
			seen: func(p probed) bool {
				return p.any(func(o *op) bool {
					_, written := p.written(o)
					return errors.Is(o.err, simstore.ErrFault) && written
				})
			},
			never: func(p probed) bool {
				// A write that was applied, and reported as a fault, lost its response to a cut-off.
				return !p.any(func(o *op) bool {
					_, written := p.written(o)
					return errors.Is(o.err, simstore.ErrFault) && written && !strings.Contains(o.err.Error(), "unreachable")
				})
			},
		},
		{
			name:   "a request that is slow",
			off:    func(f *simstore.Faults) { f.Slow = 0 },
			marker: "is slow on its way",
			seen: func(p probed) bool {
				return p.any(func(o *op) bool { return o.done && o.end-o.start > longest })
			},
			never: func(p probed) bool {
				return !p.any(func(o *op) bool { return o.done && !o.gaveUp && o.end-o.start > longest })
			},
		},
		{
			name:   "a read whose content breaks off",
			off:    func(f *simstore.Faults) { f.BrokenRead = 0 },
			marker: "whose content breaks off",
			seen: func(p probed) bool {
				return p.any(func(o *op) bool { return errors.Is(o.readErr, simstore.ErrFault) })
			},
			never: func(p probed) bool {
				return !p.any(func(o *op) bool { return errors.Is(o.readErr, simstore.ErrFault) })
			},
		},
		{
			name:   "a client that is cut off",
			off:    func(f *simstore.Faults) { f.CutOff = 0 },
			marker: "cut off for",
			seen: func(p probed) bool {
				return p.any(func(o *op) bool { return o.err != nil && strings.Contains(o.err.Error(), "unreachable") })
			},
			never: func(p probed) bool { return !p.trace("cut off for") },
		},
		{
			name:   "a store that nobody reaches",
			off:    func(f *simstore.Faults) { f.Outage = 0 },
			marker: "store: unreachable for",
			seen: func(p probed) bool {
				return p.any(func(o *op) bool { return o.err != nil && strings.Contains(o.err.Error(), "unreachable") })
			},
			never: func(p probed) bool { return !p.trace("store: unreachable for") },
		},
		{
			name:   "a write that is applied after its caller gave up",
			off:    func(f *simstore.Faults) { f.Late = 0 },
			marker: "the caller gave up, and is applied",
			seen: func(p probed) bool {
				return p.any(func(o *op) bool {
					w, written := p.written(o)
					return o.gaveUp && written && w.Time > o.end
				})
			},
			never: func(p probed) bool {
				return !p.any(func(o *op) bool {
					w, written := p.written(o)
					// A request that arrives after its sender crashed is the other fault's.
					afterCrash := o.client == "c" && p.crashed >= 0 && w.Time > p.crashed
					return o.gaveUp && written && w.Time > o.end && !afterCrash
				})
			},
		},
		{
			name:   "a write that is applied after its sender crashed",
			off:    func(f *simstore.Faults) { f.AfterCrash = 0 },
			marker: "the sender crashed, and is applied",
			seen: func(p probed) bool {
				return p.any(func(o *op) bool {
					w, written := p.written(o)
					return !o.done && written && w.Time > p.crashed
				})
			},
			never: func(p probed) bool {
				return !p.any(func(o *op) bool {
					w, written := p.written(o)
					// A request that arrived before the crash, after its caller gave up, is the other
					// fault's.
					return o.client == "c" && !o.gaveUp && written && w.Time > p.crashed
				})
			},
		},
	}

	const seeds = 30
	for _, test := range tests {
		t.Run("should make "+test.name+" happen, and say so in the trace", func(t *testing.T) {
			// Only this fault is on: every probability that its switch leaves alone is zero.
			off := faultsOn()
			test.off(&off)
			only := onlyFault(faultsOn(), off)

			var seen, traced int
			for seed := range uint64(seeds) {
				p := probe(t, seed, probeOptions{store: simstore.Options{Latency: latency, Faults: only}, crash: true, impatient: true}, nil)
				is.NotError(t, p.err)
				is.Equal(t, "", p.breach(), fmt.Sprint("seed ", seed))
				if test.seen(p) {
					seen++
				}
				if p.trace(test.marker) {
					traced++
				}
			}
			t.Logf("%v of %v seeds saw it, and %v traced it", seen, seeds, traced)
			is.True(t, seen > 0, "no seed saw it")
			is.True(t, traced > 0, "no seed traced it")
		})

		t.Run("should never make "+test.name+" happen with it off, with every other fault on", func(t *testing.T) {
			f := faultsOn()
			test.off(&f)
			for seed := range uint64(seeds) {
				p := probe(t, seed, probeOptions{store: simstore.Options{Latency: latency, Faults: f}, crash: true, impatient: true}, nil)
				is.NotError(t, p.err)
				is.Equal(t, "", p.breach(), fmt.Sprint("seed ", seed))
				is.True(t, !p.trace(test.marker), fmt.Sprint("seed ", seed, " traced it"))
				is.True(t, test.never(p), fmt.Sprint("seed ", seed, " saw it"))
			}
		})
	}
}

// onlyFault is on with every probability zeroed that off has too, where off is on with one fault
// switched off: so it has only that one fault on.
func onlyFault(on, off simstore.Faults) simstore.Faults {
	only := on
	for _, p := range []struct{ only, off *float64 }{
		{&only.LostRequest, &off.LostRequest},
		{&only.LostResponse, &off.LostResponse},
		{&only.Slow, &off.Slow},
		{&only.BrokenRead, &off.BrokenRead},
		{&only.CutOff, &off.CutOff},
		{&only.Outage, &off.Outage},
		{&only.Late, &off.Late},
		{&only.AfterCrash, &off.AfterCrash},
	} {
		if *p.only == *p.off {
			*p.only = 0
		}
	}
	return only
}

func TestClient_calm(t *testing.T) {
	t.Run("should fail no request with every fault off and patient callers", func(t *testing.T) {
		for seed := range uint64(30) {
			p := probe(t, seed, probeOptions{store: simstore.Options{Latency: latency}}, nil)
			is.NotError(t, p.err)
			is.Equal(t, 75, len(p.ops))
			for _, o := range p.ops {
				failed := o.err != nil && !errors.Is(o.err, store.ErrChanged) && !errors.Is(o.err, store.ErrExists) && !errors.Is(o.err, store.ErrNotFound)
				is.True(t, !failed && o.readErr == nil, fmt.Sprintf("seed %v: %v %v: %v %v", seed, o.kind, o.key, o.err, o.readErr))
			}
		}
	})
}

func TestClient_replay(t *testing.T) {
	t.Run("should give the same trace when a seed is run twice, with every fault on and a crash", func(t *testing.T) {
		distinct := map[string]bool{}
		for seed := range uint64(50) {
			options := probeOptions{store: simstore.Options{Latency: latency, Faults: faultsOn()}, crash: true, impatient: true}
			first, second := probe(t, seed, options, nil), probe(t, seed, options, nil)
			is.NotError(t, first.err)
			is.EqualSlice(t, first.lines, second.lines, fmt.Sprint("seed ", seed))
			distinct[strings.Join(first.lines, "\n")] = true
		}
		is.Equal(t, 50, len(distinct), "two seeds gave the same trace")
	})
}
