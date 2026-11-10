// Package sim is a research spike: a seeded scheduler that runs plain blocking Go code one goroutine
// at a time inside a testing/synctest bubble, so that a seed replays a run exactly.
//
// Code that is to run under the scheduler does all of its waiting through a runtime: it starts
// goroutines with Go, waits for them with [Task.Wait], waits on time with Sleep, takes locks from
// NewSemaphore, and marks other points where another goroutine may run with Yield. There are two
// runtimes with the same methods. [Scheduler] runs one task at a time and chooses the next by its
// seed. [Free] maps each method to the plain Go operation, so tasks run in parallel as the Go
// runtime sees fit.
//
// A context carries the identity of the task that runs. A task must pass the context it was started
// with, or one derived from it, to every method of the runtime.
//
// The rules for code that runs under [Scheduler], and what happens when it breaks them:
//
//   - It waits only through the runtime: no channel operations that block, no sync.Cond or
//     sync.WaitGroup, no time.Sleep. The run fails with the task and the place it last waited at.
//   - It starts goroutines only with Go, never with a go statement. The run fails when such a
//     goroutine reaches the runtime. One that never does is not noticed.
//   - It passes no context with a deadline to the runtime, because a deadline is a timer of the
//     bubble's clock, which stands still. The run fails.
//   - It does not read the time package's clock, which stands still at [Epoch]. Nothing notices.
//   - It holds no sync.Mutex while it waits. The run hangs until the test times out.
//   - It does nothing that differs between two runs with the same seed, such as ranging over a map
//     to decide an order. Only running a seed twice and comparing the traces notices.
package sim

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing/synctest"
	"time"
)

// Epoch is the time at which a [Scheduler] starts, which is also where the fake clock of a
// testing/synctest bubble starts.
func Epoch() time.Time {
	return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
}

// Task is a goroutine started by a runtime.
type Task interface {
	// Wait until the task has returned. It fails only if ctx is done first.
	Wait(ctx context.Context) error
}

// Semaphore with a fixed number of permits, which is safe to hold while waiting on anything else the
// runtime offers. With one permit it is a lock.
type Semaphore interface {
	// Acquire a permit, waiting until one is free. It fails only if ctx is done first.
	Acquire(ctx context.Context) error
	// Release a permit.
	Release(ctx context.Context)
}

// never is the wake time of a task that waits on a condition alone.
const never = time.Duration(math.MaxInt64)

type taskKey struct{}

// task of a [Scheduler]. The fields from goroutine on are guarded by the scheduler's mutex.
type task struct {
	s    *Scheduler
	name string
	// wake is received from by the task's goroutine while it is parked, and sent to by the scheduler.
	wake chan struct{}

	// goroutine is the ID of the task's goroutine.
	goroutine uint64
	parked    bool
	// site names where the task is parked.
	site string
	// at is the simulated time from which the task may run again, or never.
	at time.Duration
	// cond, if not nil, lets the task run again as soon as it returns true, whatever at says.
	cond func() bool
	done bool
}

// Options for a [Scheduler].
type Options struct {
	// Jitter is the longest simulated delay that a yield, a release, or the start of a task gets. Half
	// of them get none. With no jitter, a task that yields always runs again before simulated time
	// moves, so a task that is waiting on time can never run in between.
	Jitter time.Duration
	// MaxSteps after which the run fails, to end a run that never finishes. Zero means one million.
	MaxSteps int
}

// Scheduler is a runtime that runs one task at a time and picks the next one by its seed. It keeps
// its own simulated clock and never lets a task wait on the bubble's fake clock.
type Scheduler struct {
	options Options

	mu  sync.Mutex
	rng *rand.Rand
	// now is the simulated time since [Epoch].
	now time.Duration
	// tasks that have not returned, by name.
	tasks map[string]*task
	// current is the one task that is running, or nil.
	current *task
	steps   int
	trace   []string
	// failure is the first broken rule, deadlock, or other reason for which the run failed.
	failure error
}

// New scheduler for a seed. It must be created and used inside a testing/synctest bubble.
func New(seed uint64, options Options) *Scheduler {
	if options.MaxSteps == 0 {
		options.MaxSteps = 1_000_000
	}
	return &Scheduler{
		options: options,
		rng:     newRand(seed),
		tasks:   map[string]*task{},
	}
}

// newRand is a generator for seed.
func newRand(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
}

// goroutineID of the calling goroutine, read from the first line of its stack trace, which is
// "goroutine 123 [running]:". Go has no other way to tell goroutines apart.
func goroutineID() uint64 {
	var buf [64]byte
	fields := strings.Fields(string(buf[:runtime.Stack(buf[:], false)]))
	if len(fields) < 2 {
		panic("sim: no goroutine ID in the stack trace")
	}
	id, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		panic("sim: no goroutine ID in the stack trace: " + err.Error())
	}
	return id
}

// Run f as the first task, named main, and schedule tasks until all of them have returned. It fails
// if every task waits on a condition that is false, if a task breaks a rule that the scheduler can
// see, or after [Options.MaxSteps].
func (s *Scheduler) Run(f func(ctx context.Context)) error {
	s.Go(context.Background(), "main", f)

	for {
		// Every goroutine in the bubble is durably blocked when Wait returns. A goroutine in a call
		// into C is not durably blocked, so Wait also waits for C code to return.
		synctest.Wait()

		next, done, err := s.next()
		if done {
			return err
		}
		if next != nil {
			next.wake <- struct{}{}
		}
	}
}

// next task to run, taken out of the parked state. It returns done when the run has ended, which is
// when all tasks have returned, or when the run has failed and no task is left to unwind, and then
// err is the failure. It returns neither a task nor done when only simulated time moved.
func (s *Scheduler) next() (next *task, done bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.current != nil {
		// Unwinding cannot reach this task, so the bubble will report a deadlock on top of this failure
		// unless something else lets the task go.
		s.fail(fmt.Errorf("task %v is blocked outside the scheduler, after %v", s.current.name, s.current.site))
		s.current = nil
	}

	names := make([]string, 0, len(s.tasks))
	for name, t := range s.tasks {
		if t.parked {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	waits := func() string {
		var waits []string
		for _, name := range names {
			waits = append(waits, name+" at "+s.tasks[name].site)
		}
		return strings.Join(waits, ", ")
	}

	if s.failure == nil && len(s.tasks) == 0 {
		return nil, true, nil
	}
	if s.steps >= s.options.MaxSteps {
		s.fail(fmt.Errorf("no end after %v steps; waiting: %v", s.steps, waits()))
	}

	if s.failure != nil {
		// Unwind the parked tasks one at a time: each ends its goroutine from inside its wait, which
		// runs its deferred calls.
		if len(names) == 0 {
			return nil, true, s.failure
		}
		t := s.tasks[names[0]]
		t.parked = false
		s.current = t
		return t, false, nil
	}

	var eligible []*task
	earliest := never
	for _, name := range names {
		t := s.tasks[name]
		if t.at <= s.now || (t.cond != nil && t.cond()) {
			eligible = append(eligible, t)
		}
		earliest = min(earliest, t.at)
	}

	if len(eligible) == 0 {
		if earliest == never {
			s.fail(fmt.Errorf("deadlock: every task waits on a condition that is false: %v", waits()))
			return nil, false, nil
		}
		s.now = earliest
		return nil, false, nil
	}

	t := eligible[s.rng.IntN(len(eligible))]
	t.parked = false
	s.current = t
	s.steps++
	s.trace = append(s.trace, fmt.Sprintf("%v %v resumes from %v", s.now, t.name, t.site))
	return t, false, nil
}

// fail the run with err unless it has failed already. It requires the mutex.
func (s *Scheduler) fail(err error) {
	if s.failure == nil {
		s.failure = err
	}
}

// taskOf ctx, which panics for a context that no task of this scheduler was started with.
func (s *Scheduler) taskOf(ctx context.Context) *task {
	t, _ := ctx.Value(taskKey{}).(*task)
	if t == nil || t.s != s {
		panic("sim: the context does not belong to a task of this scheduler")
	}
	return t
}

// park the task of ctx at site until the scheduler runs it again, which it does no earlier than at
// the simulated time at, unless cond is not nil and returns true before. The scheduler calls cond
// with its mutex held while no task runs.
//
// If the run has failed, or fails because this wait breaks a rule, park does not return: it ends the
// calling goroutine with [runtime.Goexit], which runs the goroutine's deferred calls and cannot be
// recovered from.
func (s *Scheduler) park(ctx context.Context, site string, at time.Duration, cond func() bool) {
	t := s.taskOf(ctx)
	goroutine := goroutineID()

	s.mu.Lock()
	if s.failure == nil {
		_, deadline := ctx.Deadline()
		switch {
		case goroutine != t.goroutine:
			// A go statement in the task started this goroutine, and it carries the task's context.
			s.fail(fmt.Errorf("task %v waits at %v in a goroutine that is not its own", t.name, site))
		case s.current != t:
			s.fail(fmt.Errorf("task %v waits at %v without the scheduler having run it", t.name, site))
		case deadline:
			s.fail(fmt.Errorf("task %v waits at %v with a context that has a deadline, which simulated time does not reach", t.name, site))
		}
	}
	if s.failure != nil {
		s.mu.Unlock()
		runtime.Goexit()
	}
	s.current = nil
	t.parked, t.site, t.at, t.cond = true, site, at, cond
	s.mu.Unlock()

	<-t.wake

	s.mu.Lock()
	failed := s.failure != nil
	s.mu.Unlock()
	if failed {
		runtime.Goexit()
	}
}

// jitter is a simulated delay for a point where a task lets others run. It requires the mutex.
func (s *Scheduler) jitter() time.Duration {
	if s.options.Jitter == 0 || s.rng.IntN(2) == 0 {
		return 0
	}
	return time.Duration(s.rng.Int64N(int64(s.options.Jitter) + 1))
}

// Go starts f as a new task with a name that no other running task has. The task first runs when the
// scheduler picks it.
func (s *Scheduler) Go(ctx context.Context, name string, f func(ctx context.Context)) Task {
	t := &task{s: s, name: name, wake: make(chan struct{}), parked: true, site: "start"}

	s.mu.Lock()
	if _, ok := s.tasks[name]; ok {
		s.mu.Unlock()
		panic("sim: a task named " + name + " is already running")
	}
	t.at = s.now + s.jitter()
	s.tasks[name] = t
	s.mu.Unlock()

	go func() {
		s.mu.Lock()
		t.goroutine = goroutineID()
		s.mu.Unlock()

		<-t.wake
		// The deferred call also runs when a wait ends the goroutine because the run has failed.
		defer func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			t.done = true
			delete(s.tasks, name)
			if s.current == t {
				s.current = nil
			} else {
				s.fail(fmt.Errorf("task %v returned without the scheduler having run it", name))
			}
		}()

		s.mu.Lock()
		failed := s.failure != nil
		s.mu.Unlock()
		if failed {
			return
		}
		f(context.WithValue(ctx, taskKey{}, t))
	}()
	return t
}

// Wait until the task has returned, as [Task] describes.
func (t *task) Wait(ctx context.Context) error {
	t.s.park(ctx, "wait for "+t.name, never, func() bool { return t.done || ctx.Err() != nil })

	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	if !t.done {
		return ctx.Err()
	}
	return nil
}

// Yield marks a point named site where other tasks may run before this one continues.
func (s *Scheduler) Yield(ctx context.Context, site string) {
	s.mu.Lock()
	at := s.now + s.jitter()
	s.mu.Unlock()
	s.park(ctx, site, at, nil)
}

// Sleep for d of simulated time. It fails only if ctx is done first.
func (s *Scheduler) Sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	at := never
	if d < never-s.now {
		at = s.now + d
	}
	s.mu.Unlock()
	s.park(ctx, "sleep", at, func() bool { return ctx.Err() != nil })

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.now < at {
		return ctx.Err()
	}
	return nil
}

// Now is the simulated time.
func (s *Scheduler) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Epoch().Add(s.now)
}

// Uint64 is the next number from the seed. The scheduler draws from the same sequence to choose
// tasks, so a run that takes a different turn anywhere draws different numbers from then on.
func (s *Scheduler) Uint64() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rng.Uint64()
}

// Trace adds a line to the trace of the run, at the current simulated time and under the name of the
// task of ctx.
func (s *Scheduler) Trace(ctx context.Context, format string, args ...any) {
	t := s.taskOf(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trace = append(s.trace, fmt.Sprintf("%v %v ", s.now, t.name)+fmt.Sprintf(format, args...))
}

// Lines of the trace so far: one for every step of the scheduler and one for every call to
// [Scheduler.Trace], in the order in which they happened.
func (s *Scheduler) Lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.trace)
}

// Steps that the scheduler has taken, each of which ran one task from one wait to its next.
func (s *Scheduler) Steps() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.steps
}

// NewSemaphore with n permits. The name appears in the trace.
func (s *Scheduler) NewSemaphore(name string, n int) Semaphore {
	return &semaphore{s: s, name: name, n: n, free: n}
}

// semaphore of a [Scheduler].
type semaphore struct {
	s    *Scheduler
	name string
	// n is the number of permits.
	n int
	// free permits, guarded by the scheduler's mutex.
	free int
}

func (m *semaphore) Acquire(ctx context.Context) error {
	// The task parks even when a permit is free, so that the seed decides who gets it.
	m.s.park(ctx, "acquire of "+m.name, never, func() bool { return m.free > 0 || ctx.Err() != nil })

	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	if m.free == 0 {
		return ctx.Err()
	}
	m.free--
	return nil
}

func (m *semaphore) Release(ctx context.Context) {
	m.s.mu.Lock()
	if m.free == m.n {
		m.s.mu.Unlock()
		panic("sim: release of a semaphore that is not held")
	}
	m.free++
	at := m.s.now + m.s.jitter()
	m.s.mu.Unlock()
	// The task parks so that a task that waits for the permit can run before this one continues.
	m.s.park(ctx, "release of "+m.name, at, nil)
}

// Free is a runtime that maps each method to the plain Go operation, with real goroutines that run
// in parallel. Inside a testing/synctest bubble its clock is the bubble's fake clock. Nothing in a
// run with it is replayable.
type Free struct {
	// tasks that have not returned.
	tasks sync.WaitGroup

	mu    sync.Mutex
	rng   *rand.Rand
	trace []string
}

// NewFree runtime. The seed sets the sequence of random numbers, but not who draws which.
func NewFree(seed uint64) *Free {
	return &Free{rng: newRand(seed)}
}

// Run f and wait until it and every task started from it have returned. It never fails.
func (r *Free) Run(f func(ctx context.Context)) error {
	r.Go(context.Background(), "main", f)
	r.tasks.Wait()
	return nil
}

// freeTask of a [Free] runtime, whose channel is closed when the task has returned.
type freeTask chan struct{}

func (t freeTask) Wait(ctx context.Context) error {
	select {
	case <-t:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Go starts f in a new goroutine. The name is not used.
func (r *Free) Go(ctx context.Context, _ string, f func(ctx context.Context)) Task {
	t := make(freeTask)
	r.tasks.Go(func() {
		defer close(t)
		f(ctx)
	})
	return t
}

// Yield lets other goroutines run.
func (r *Free) Yield(context.Context, string) {
	runtime.Gosched()
}

// Sleep for d. It fails only if ctx is done first.
func (r *Free) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Now is the time of the time package.
func (r *Free) Now() time.Time {
	return time.Now()
}

// Uint64 is the next number from the seed.
func (r *Free) Uint64() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rng.Uint64()
}

// Trace adds a line to the trace of the run. Lines of tasks that run in parallel come in no
// particular order.
func (r *Free) Trace(_ context.Context, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trace = append(r.trace, fmt.Sprintf(format, args...))
}

// Lines of the trace so far.
func (r *Free) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.trace)
}

// NewSemaphore with n permits. The name is not used.
func (r *Free) NewSemaphore(_ string, n int) Semaphore {
	return make(freeSemaphore, n)
}

// freeSemaphore of a [Free] runtime, which holds one value for every permit that is taken.
type freeSemaphore chan struct{}

func (m freeSemaphore) Acquire(ctx context.Context) error {
	select {
	case m <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m freeSemaphore) Release(context.Context) {
	select {
	case <-m:
	default:
		panic("sim: release of a semaphore that is not held")
	}
}
