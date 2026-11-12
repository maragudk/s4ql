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

// TestClient_free runs the store with real goroutines in parallel, for the race detector.
func TestClient_free(t *testing.T) {
	t.Run("should pass the conformance suite", func(t *testing.T) {
		storetest.Test(t, func(t *testing.T, f func(t *testing.T, ctx context.Context, env storetest.Env)) {
			synctest.Test(t, func(t *testing.T) {
				r := sim.NewFree(sim.Buggify{})
				st := simstore.New(r, simstore.Options{Latency: 50 * time.Millisecond})
				is.NotError(t, r.Run(func(ctx context.Context) {
					f(t, ctx, storetest.Env{Host: r, Client: func(name string) store.Store { return st.Client(name, nil) }})
				}))
			})
		})
	})

	t.Run("should take requests from callers in parallel with every fault on", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := sim.NewFree(sim.Buggify{})
			st := simstore.New(r, simstore.Options{Latency: 50 * time.Millisecond, Faults: simstore.AllFaults()})
			is.NotError(t, r.Run(func(ctx context.Context) {
				var tasks []host.Task
				for i := range 8 {
					c := st.Client(fmt.Sprint("caller-", i), nil)
					tasks = append(tasks, r.Go(ctx, fmt.Sprint("caller-", i), func(ctx context.Context) {
						for n := range 20 {
							ctx, cancel := r.WithTimeout(ctx, time.Second)
							body, v, err := c.Get(ctx, "counter")
							switch {
							case errors.Is(err, store.ErrNotFound):
								_, _ = c.Create(ctx, "counter", strings.NewReader(fmt.Sprint(i, n)), nil)
							case err == nil:
								_, _ = io.ReadAll(body)
								_ = body.Close()
								_, _ = c.Swap(ctx, "counter", v, strings.NewReader(fmt.Sprint(i, n)), nil)
							}
							cancel()
						}
					}))
				}
				for _, task := range tasks {
					is.NotError(t, task.Wait(ctx))
				}
				is.NotError(t, st.Wait(ctx))
				is.True(t, len(st.History()) > 0)
			}))
		})
	})
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
				// Each way is slow by up to an hour, so the response is still on its way when the poll below
				// finds the write, up to a second after it was applied.
				st := simstore.New(s, simstore.Options{Latency: latency, Faults: simstore.Faults{Slow: 1, SlowBy: time.Hour}})
				a := st.Client("a", nil)
				var err error
				task := s.Go(ctx, "writer", func(ctx context.Context) {
					_, err = a.Create(ctx, "k", strings.NewReader("data"), nil)
				})
				for len(st.History()) == 0 {
					is.NotError(t, s.Sleep(ctx, time.Second))
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
	// from is the version that a swap was from.
	from store.Version
	// timeout of the call, and gaveUp, which is set if the timeout ended it.
	timeout    time.Duration
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

// breach of a cut-off or an outage, from the windows of them in the trace: a write of a client that
// the store applied while the client could not reach it, or a call of the client that succeeded
// while it could not. It is empty if there is none.
func (p probed) breach() string {
	// windows of each client, by its name, and of every client, by the empty name.
	windows := map[string][][2]time.Duration{}
	cutOffLine := regexp.MustCompile(`^(\S+) .*store: client (\S+) cut off for (\S+)$`)
	outageLine := regexp.MustCompile(`^(\S+) .*store: unreachable for (\S+)$`)
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
						o := &op{client: name, content: fmt.Sprint(name, "-", i), timeout: timeout, start: s.Monotonic()}
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
			o.kind, o.from = "swap", v
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
		Hang:         0.3,
		Conflict:     0.3,
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
	// late is longer than any request takes to arrive without a slow request or a late delay.
	late := 2 * latency

	// faulty reports whether the call of o failed for a fault, and returned.
	faulty := func(o *op) bool {
		return o.done && !o.gaveUp && errors.Is(o.err, simstore.ErrFault)
	}
	unreachable := func(o *op) bool {
		return (o.err != nil && strings.Contains(o.err.Error(), "unreachable")) ||
			(o.readErr != nil && strings.Contains(o.readErr.Error(), "unreachable"))
	}

	tests := []struct {
		name string
		// off switches the fault off.
		off func(f *simstore.Faults)
		// with switches on what the fault needs to happen at all.
		with func(f *simstore.Faults)
		// alike switches off the other faults that look the same to the callers and in the history.
		alike func(f *simstore.Faults)
		// markers are what the trace says when the fault happens.
		markers []string
		// seen reports whether the fault happened, by what the callers and the history saw, in a run
		// with only this fault, and what it needs, on.
		seen func(p probed) bool
		// never reports whether the fault did not happen, by what the callers and the history saw, in
		// a run with every fault on but this one and those that look alike.
		never func(p probed) bool
	}{
		{
			name:    "a request that is lost on its way",
			off:     func(f *simstore.Faults) { f.LostRequest = 0 },
			alike:   func(f *simstore.Faults) { f.CutOff, f.Outage, f.LostResponse = 0, 0, 0 },
			markers: []string{"was lost on its way to the store"},
			seen: func(p probed) bool {
				return p.any(func(o *op) bool {
					_, written := p.written(o)
					return o.kind != "get" && faulty(o) && !written
				})
			},
			never: func(p probed) bool {
				return !p.any(func(o *op) bool {
					_, written := p.written(o)
					return o.kind != "get" && faulty(o) && !written
				})
			},
		},
		{
			name:    "a response that is lost on its way back",
			off:     func(f *simstore.Faults) { f.LostResponse = 0 },
			alike:   func(f *simstore.Faults) { f.CutOff, f.Outage, f.Late = 0, 0, 0 },
			markers: []string{"was lost on its way back"},
			seen: func(p probed) bool {
				return p.any(func(o *op) bool {
					_, written := p.written(o)
					return faulty(o) && written
				})
			},
			never: func(p probed) bool {
				return !p.any(func(o *op) bool {
					_, written := p.written(o)
					return o.done && errors.Is(o.err, simstore.ErrFault) && written
				})
			},
		},
		{
			name:    "a call that gets no answer until its context ends",
			off:     func(f *simstore.Faults) { f.Hang = 0 },
			with:    func(f *simstore.Faults) { f.LostRequest = 0.3 },
			alike:   func(*simstore.Faults) {},
			markers: []string{"no answer reaches the caller"},
			seen: func(p probed) bool {
				return p.any(func(o *op) bool { return o.gaveUp && o.timeout == 10*time.Minute })
			},
			never: func(p probed) bool {
				return !p.any(func(o *op) bool { return o.gaveUp && o.timeout == 10*time.Minute })
			},
		},
		{
			name:    "a swap from the right version that is refused for a conflict",
			off:     func(f *simstore.Faults) { f.Conflict = 0 },
			alike:   func(*simstore.Faults) {},
			markers: []string{"a conflict with another write in flight"},
			seen:    func(p probed) bool { return p.any(p.conflicted) },
			never:   func(p probed) bool { return !p.any(p.conflicted) },
		},
		{
			name:    "a request that is slow on its way there or back",
			off:     func(f *simstore.Faults) { f.Slow = 0 },
			alike:   func(*simstore.Faults) {},
			markers: []string{"is slow on its way"},
			seen: func(p probed) bool {
				// An object is created with one request, so the time to its write is the way there,
				// and the time after it the way back.
				object := func(o *op) bool { return o.kind == "create" && strings.HasPrefix(o.key, "objects/") }
				there := p.any(func(o *op) bool {
					w, written := p.written(o)
					return object(o) && written && w.Time-o.start > late
				})
				back := p.any(func(o *op) bool {
					w, written := p.written(o)
					return object(o) && o.done && !o.gaveUp && written && o.end-w.Time > late
				})
				return there && back
			},
			never: func(p probed) bool {
				return !p.any(func(o *op) bool { return o.done && !o.gaveUp && o.end-o.start > longest })
			},
		},
		{
			name:    "a read whose content breaks off",
			off:     func(f *simstore.Faults) { f.BrokenRead = 0 },
			alike:   func(f *simstore.Faults) { f.CutOff, f.Outage = 0, 0 },
			markers: []string{"whose content breaks off"},
			seen: func(p probed) bool {
				return p.any(func(o *op) bool { return errors.Is(o.readErr, simstore.ErrFault) })
			},
			never: func(p probed) bool {
				return !p.any(func(o *op) bool { return errors.Is(o.readErr, simstore.ErrFault) })
			},
		},
		{
			name:    "a client that is cut off",
			off:     func(f *simstore.Faults) { f.CutOff = 0 },
			alike:   func(f *simstore.Faults) { f.Outage = 0 },
			markers: []string{"cut off for"},
			seen:    func(p probed) bool { return p.any(unreachable) },
			never:   func(p probed) bool { return !p.any(unreachable) },
		},
		{
			name:    "a store that nobody reaches",
			off:     func(f *simstore.Faults) { f.Outage = 0 },
			alike:   func(f *simstore.Faults) { f.CutOff = 0 },
			markers: []string{"store: unreachable for"},
			seen:    func(p probed) bool { return p.any(unreachable) },
			never:   func(p probed) bool { return !p.any(unreachable) },
		},
		{
			name: "a write that is applied long after its caller gave up or got an error",
			off:  func(f *simstore.Faults) { f.Late = 0 },
			// A write is stuck in the network only after it was lost.
			with:    func(f *simstore.Faults) { f.LostRequest = 0.3 },
			alike:   func(f *simstore.Faults) { f.AfterCrash = 0 },
			markers: []string{"the caller gave up, and is applied", "is stuck in the network"},
			seen: func(p probed) bool {
				// Both a write whose caller gave up, and one whose caller got an error, land long after
				// the call returned.
				gaveUp := p.any(func(o *op) bool {
					w, written := p.written(o)
					return o.done && o.gaveUp && written && w.Time > o.end && w.Time-o.start > late
				})
				stuck := p.any(func(o *op) bool {
					w, written := p.written(o)
					return faulty(o) && written && w.Time-o.end > late
				})
				return gaveUp && stuck
			},
			never: func(p probed) bool {
				return !p.any(func(o *op) bool {
					w, written := p.written(o)
					return o.done && written && w.Time > o.end
				})
			},
		},
		{
			name:    "a write that is applied long after its sender crashed",
			off:     func(f *simstore.Faults) { f.AfterCrash = 0 },
			alike:   func(f *simstore.Faults) { f.Late = 0 },
			markers: []string{"the sender crashed, and is applied"},
			seen: func(p probed) bool {
				return p.any(func(o *op) bool {
					w, written := p.written(o)
					return !o.done && written && w.Time > p.crashed && w.Time-o.start > late
				})
			},
			never: func(p probed) bool {
				return !p.any(func(o *op) bool {
					w, written := p.written(o)
					return o.client == "c" && written && w.Time > p.crashed
				})
			},
		},
	}

	const seeds = 30
	run := func(t *testing.T, f simstore.Faults, check func(seed uint64, p probed)) {
		t.Helper()
		for seed := range uint64(seeds) {
			p := probe(t, seed, probeOptions{store: simstore.Options{Latency: latency, Faults: f}, crash: true, impatient: true}, nil)
			is.NotError(t, p.err)
			is.Equal(t, "", p.breach(), fmt.Sprint("seed ", seed))
			check(seed, p)
		}
	}
	traced := func(p probed, markers []string) bool {
		return slices.ContainsFunc(markers, p.trace)
	}

	for _, test := range tests {
		t.Run("should make "+test.name+" happen, and say so in the trace", func(t *testing.T) {
			// Only this fault is on, and what it needs: every probability that its switch leaves alone
			// is zero.
			off := faultsOn()
			test.off(&off)
			only := onlyFault(faultsOn(), off)
			if test.with != nil {
				test.with(&only)
			}
			var seen, wasTraced int
			run(t, only, func(_ uint64, p probed) {
				if test.seen(p) {
					seen++
				}
				if traced(p, test.markers) {
					wasTraced++
				}
			})
			t.Logf("%v of %v seeds saw it, and %v traced it", seen, seeds, wasTraced)
			is.True(t, seen > 0, "no seed saw it")
			is.True(t, wasTraced > 0, "no seed traced it")
		})

		t.Run("should never make "+test.name+" happen with it off, and never trace it, with every other fault on", func(t *testing.T) {
			f := faultsOn()
			test.off(&f)
			run(t, f, func(seed uint64, p probed) {
				is.True(t, !traced(p, test.markers), fmt.Sprint("seed ", seed, " traced it"))
			})
		})

		t.Run("should never make "+test.name+" happen with it off, by what the callers and the history see, with every fault on that does not look the same", func(t *testing.T) {
			f := faultsOn()
			test.off(&f)
			test.alike(&f)
			run(t, f, func(seed uint64, p probed) {
				is.True(t, test.never(p), fmt.Sprint("seed ", seed, " saw it"))
			})
		})
	}
}

// conflicted reports whether o is a swap that the store refused with [store.ErrChanged] although no
// write to its key was applied while it ran, so the version it was from was still there.
func (p probed) conflicted(o *op) bool {
	if o.kind != "swap" || !o.done || !errors.Is(o.err, store.ErrChanged) {
		return false
	}
	return !slices.ContainsFunc(p.history, func(w simstore.Write) bool {
		return w.Key == o.key && w.Time >= o.start && w.Time <= o.end
	})
}

// onlyFault is on with every probability zeroed that off has too, where off is on with one fault
// switched off: so it has only that one fault on.
func onlyFault(on, off simstore.Faults) simstore.Faults {
	only := on
	for _, p := range []struct{ only, off *float64 }{
		{&only.LostRequest, &off.LostRequest},
		{&only.LostResponse, &off.LostResponse},
		{&only.Hang, &off.Hang},
		{&only.Conflict, &off.Conflict},
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

func TestClient_crash(t *testing.T) {
	t.Run("should decide by AfterCrash a write whose caller gave up and whose sender crashed before it arrived", func(t *testing.T) {
		for _, test := range []struct {
			faults  simstore.Faults
			applied bool
		}{
			{faults: simstore.Faults{AfterCrash: 1}, applied: true},
			{faults: simstore.Faults{Late: 1}, applied: false},
		} {
			synctest.Test(t, func(t *testing.T) {
				s := sim.New(0, sim.Options{})
				is.NotError(t, s.Run(func(ctx context.Context) {
					st := simstore.New(s, simstore.Options{Latency: latency, Faults: test.faults})
					inst := s.NewInstance("a")
					c := st.Client("a", inst)
					is.NotError(t, inst.Go(ctx, "caller", func(ctx context.Context) {
						ctx, cancel := inst.WithTimeout(ctx, time.Nanosecond)
						defer cancel()
						_, err := c.Create(ctx, "k", strings.NewReader("data"), nil)
						is.True(t, errors.Is(err, context.DeadlineExceeded))
					}).Wait(ctx))
					inst.Crash()
					is.NotError(t, st.Wait(ctx))
					is.Equal(t, test.applied, len(st.History()) == 1)
				}))
			})
		}
	})

	t.Run("should send nothing for a sender that has crashed, also from a deferred call when the run ends", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(0, sim.Options{})
			var deferredErr error
			is.NotError(t, s.Run(func(ctx context.Context) {
				st := simstore.New(s, simstore.Options{Latency: latency, Faults: simstore.Faults{AfterCrash: 1}})
				inst := s.NewInstance("a")
				c := st.Client("a", inst)
				_ = inst.Go(ctx, "caller", func(ctx context.Context) {
					defer func() {
						_, deferredErr = c.Create(ctx, "deferred", strings.NewReader("data"), nil)
					}()
					inst.Crash()
					// The task runs on after its own crash until it waits, and its request is refused.
					_, err := c.Create(ctx, "k", strings.NewReader("data"), nil)
					is.True(t, errors.Is(err, simstore.ErrFault), fmt.Sprint(err))
					_ = s.Sleep(ctx, time.Second)
				})
				is.NotError(t, s.Sleep(ctx, time.Minute))
				is.NotError(t, st.Wait(ctx))
				is.Equal(t, 0, len(st.History()))
			}))
			is.True(t, errors.Is(deferredErr, simstore.ErrFault), fmt.Sprint(deferredErr))
		})
	})

	t.Run("should fail the call of a live caller whose request a crashed sender sent", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(0, sim.Options{})
			is.NotError(t, s.Run(func(ctx context.Context) {
				st := simstore.New(s, simstore.Options{Latency: latency})
				c := st.Client("a", &crashing{s: s})
				_, err := c.Create(ctx, "k", strings.NewReader("data"), nil)
				is.True(t, errors.Is(err, simstore.ErrFault), fmt.Sprint(err))
				is.Equal(t, 0, len(st.History()))
			}))
		})
	})
}

func TestClient_Swap(t *testing.T) {
	t.Run("should refuse a swap for a conflict only while another write to its key is in flight", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(0, sim.Options{})
			is.NotError(t, s.Run(func(ctx context.Context) {
				st := simstore.New(s, simstore.Options{Latency: latency, Faults: simstore.Faults{Conflict: 1}})
				c := st.Client("a", nil)
				v, err := c.Create(ctx, "k", strings.NewReader("0"), nil)
				is.NotError(t, err)
				// One write at a time never conflicts, also with a write to another key in flight.
				other := s.Go(ctx, "other", func(ctx context.Context) {
					_, err := st.Client("b", nil).Create(ctx, "other", strings.NewReader("x"), nil)
					is.NotError(t, err)
				})
				for i := 1; i <= 5; i++ {
					v, err = c.Swap(ctx, "k", v, strings.NewReader(fmt.Sprint(i)), nil)
					is.NotError(t, err)
				}
				is.NotError(t, other.Wait(ctx))

				// Of racing swaps from the right version, the conflicts refuse some.
				var tasks []host.Task
				var conflicts int
				for i := range 4 {
					c := st.Client(fmt.Sprint("racer-", i), nil)
					tasks = append(tasks, s.Go(ctx, fmt.Sprint("racer-", i), func(ctx context.Context) {
						_, err := c.Swap(ctx, "k", v, strings.NewReader(fmt.Sprint("racer ", i)), nil)
						if err != nil && strings.Contains(err.Error(), "conflict") {
							conflicts++
						}
					}))
				}
				for _, task := range tasks {
					is.NotError(t, task.Wait(ctx))
				}
				is.True(t, conflicts > 0)
			}))
		})
	})
}

// crashing is a sender that crashes as soon as simulated time moves.
type crashing struct {
	s *sim.Scheduler
}

func (c *crashing) Crashed() bool {
	return c.s.Monotonic() > 0
}

func TestClient_guards(t *testing.T) {
	t.Run("should panic for a key that S3 refuses or may treat as another, and for a swap from an empty version", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(0, sim.Options{})
			is.NotError(t, s.Run(func(ctx context.Context) {
				c := simstore.New(s, simstore.Options{}).Client("a", nil)
				for _, key := range []string{"", strings.Repeat("k", 1025), "\xff", "a/./b", "../b", "a/.."} {
					is.True(t, panics(func() { _, _, _ = c.Get(ctx, key) }), fmt.Sprintf("get %q", key))
					is.True(t, panics(func() { _, _ = c.Create(ctx, key, strings.NewReader(""), nil) }), fmt.Sprintf("create %q", key))
				}
				is.True(t, !panics(func() { _, _ = c.Create(ctx, strings.Repeat("k", 1024), strings.NewReader(""), nil) }))
				is.True(t, panics(func() { _, _ = c.Swap(ctx, "k", "", strings.NewReader(""), nil) }))
			}))
		})
	})

	t.Run("should fail to read content that is closed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(0, sim.Options{})
			is.NotError(t, s.Run(func(ctx context.Context) {
				c := simstore.New(s, simstore.Options{}).Client("a", nil)
				_, err := c.Create(ctx, "k", strings.NewReader("data"), nil)
				is.NotError(t, err)
				body, _, err := c.Get(ctx, "k")
				is.NotError(t, err)
				is.NotError(t, body.Close())
				_, err = body.Read(make([]byte, 4))
				is.True(t, err != nil && err != io.EOF, fmt.Sprint(err))
			}))
		})
	})

	t.Run("should break off content when the client is cut off while it reads", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(0, sim.Options{})
			is.NotError(t, s.Run(func(ctx context.Context) {
				c := simstore.New(s, simstore.Options{}).Client("a", nil)
				_, err := c.Create(ctx, "k", strings.NewReader("data"), nil)
				is.NotError(t, err)
				body, _, err := c.Get(ctx, "k")
				is.NotError(t, err)
				first := make([]byte, 2)
				_, err = body.Read(first)
				is.NotError(t, err)
				c.CutOff(ctx, time.Minute)
				_, err = io.ReadAll(body)
				is.True(t, errors.Is(err, simstore.ErrFault), fmt.Sprint(err))
			}))
		})
	})
}

// panics reports whether f panics.
func panics(f func()) (panicked bool) {
	defer func() { panicked = recover() != nil }()
	f()
	return false
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
