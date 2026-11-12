// Package sim is the simulation harness: a seeded scheduler that runs plain blocking Go code one
// goroutine at a time inside a testing/synctest bubble, so that a seed replays a run exactly.
//
// Library code waits, reads the time, and draws randomness only through [host.Host]. [Scheduler]
// implements it for the test itself, and hands out an [Instance] for every process that the test
// simulates, which implements it too. Each task is a goroutine that, at every wait, records where it
// waits and blocks until the scheduler picks it. The scheduler waits until no goroutine in the bubble
// runs, picks one of the tasks that may run by its seed, and lets it run to its next wait. So one
// task runs at a time, and the seed decides the order. [Free] is a second implementation, with real
// goroutines in parallel, for the race detector.
//
// What the scheduler offers beyond the host:
//
//   - Instances. Each has its own wall clock, which the test can shift forward and backward with
//     [Instance.ShiftWall] and hold still with [Instance.StopWall], and its own monotonic clock,
//     which the test can hold still with [Instance.StopMonotonic]. Sleeps and timeouts of an
//     instance run on its monotonic clock, as Go's timers do, so they stand still with it.
//   - Crashes. [Instance.Crash] ends the instance: none of its tasks ever runs again. To restart it,
//     the test starts a new instance on what the crashed one left behind.
//   - Buggify. [Options.Buggify] switches it on for the run. Each point is then switched on or off
//     the first time it is reached, and a point that is on fires at random. Every decision comes from
//     the seed and is in the trace.
//   - A trace. Every step of the scheduler and every line from [Scheduler.Trace] or
//     [Instance.Trace], in order, with the simulated time. Running a seed twice and comparing the
//     traces is how nondeterminism is found.
//
// A crash differs from a real one in these ways:
//
//   - A crashed task is not stopped where it is but at its next wait, which is where every task of
//     the instance is unless the crash came from one of them.
//   - A crashed task ends when the run ends, after every task that did not crash has returned. Its
//     deferred calls run then, and a wait in them ends it at once. A real crash runs none. Whatever
//     they change without waiting, in memory, in files, or in SQLite, is changed after the run, so a
//     check of the run must be made before the run ends, by a task.
//   - Whatever a crashed task was waiting for never happens for it. A request that it sent and that
//     has not arrived when it crashes, and is carried by its own task, never arrives. A real network
//     can deliver it after the process has died. A simulated object store that wants to deliver it
//     must carry it on a task that does not crash.
//   - Memory of the crashed instance, such as open database connections, stays until the test frees
//     it. Files stay as they are, and the test decides what a restarted instance finds: a real process
//     crash leaves every write that the operating system has, so copying the files at the moment of
//     the crash is faithful, and a power loss is not simulated.
//
// A context from [host.Host.WithTimeout] reports [context.DeadlineExceeded] after the timeout, as one
// from [context.WithTimeout] does, but a context derived from it reports [context.Canceled], with
// [context.DeadlineExceeded] as its cause. The waits of the host return [context.DeadlineExceeded] for
// both. Code that tells a timeout from a cancellation by the error of the context, and not by the
// error of a wait, uses [context.Cause].
//
// The rules for code that runs under the scheduler, and what happens when it breaks them:
//
//   - It waits only through the host: no channel operation that blocks, no sync.Cond or
//     sync.WaitGroup, no time.Sleep. The run fails with the task and the place it last waited at.
//   - It starts goroutines only with [host.Host.Go], never with a go statement or anything else that
//     starts one. The run fails when such a goroutine reaches the host. One that never does is not
//     noticed.
//   - It passes the context of its task, or one derived from it, to the host. A context that no task
//     was started with makes the host panic.
//   - A task waits, and sets a timeout, only through the host of its own instance or through the
//     [Scheduler] itself, and uses a semaphore only through the host that made it. The run fails.
//   - It passes no context with a deadline from the context package to the host, because such a
//     deadline is a timer of the bubble's clock, which stands still. [host.Host.WithTimeout] is the
//     timeout to use. The run fails.
//   - It does not read the time package's clock, which stands still at [Epoch]. Nothing notices.
//   - It draws no randomness from anywhere but the host. Nothing notices except running a seed twice.
//   - It holds no sync.Mutex while it waits. The run hangs until the test times out.
//   - It does nothing that differs between two runs with the same seed, such as ranging over a map
//     to decide an order. Only running a seed twice and comparing the traces notices.
//   - It switches to another task only where it waits, so a race in a stretch of code without a
//     wait is never found. Where shared state is touched between waits, [host.Host.Yield] marks a
//     point where another task may run.
package sim

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing/synctest"
	"time"

	"maragu.dev/s4ql/internal/host"
)

// Epoch is the time at which a [Scheduler] starts, which is also where the fake clock of a
// testing/synctest bubble starts. Every wall clock starts there.
func Epoch() time.Time {
	return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
}

// never is the wake time of a task that waits on a condition alone.
const never = time.Duration(math.MaxInt64)

// Options for a [Scheduler].
type Options struct {
	// Jitter is the longest simulated delay that a yield, a release, or the start of a task gets. Half
	// of them get none. With no jitter, a task that yields always runs again before simulated time
	// moves, so a task that is waiting on time can never run in between.
	Jitter time.Duration
	// MaxSteps after which the run fails, to end a run that never finishes. Zero means one million.
	MaxSteps int
	// Buggify for the run.
	Buggify Buggify
}

// Buggify switches buggify on for a run, with FoundationDB's three gates: buggify is on or off for
// the run, each point is switched on once per run with the probability Activation, and a point that
// is on fires with the probability Fire each time it is reached.
type Buggify struct {
	On bool
	// Activation is the probability that a point is on for the run. Zero means 0.25.
	Activation float64
	// Fire is the probability that a point that is on fires. Zero means 0.25.
	Fire float64
}

// withDefaults sets the probabilities that are zero to their defaults.
func (b Buggify) withDefaults() Buggify {
	if b.Activation == 0 {
		b.Activation = 0.25
	}
	if b.Fire == 0 {
		b.Fire = 0.25
	}
	return b
}

// Scheduler runs one task at a time and picks the next one by its seed. It keeps its own simulated
// clock and never lets a task wait on the bubble's fake clock. As a [host.Host] it is the host of the
// test itself, outside every instance: its clocks are the simulated time, its tasks never crash, and
// every task may wait through it.
type Scheduler struct {
	options Options
	// world is the host of the test itself.
	world *Instance

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
	// unwinding is set once every task of an instance that has not crashed has returned, and the
	// tasks of crashed instances are ended.
	unwinding bool
	instances map[string]*Instance
	// timeouts that are not done, in the order in which they were made.
	timeouts []*timeout
	// sites of buggify that have been reached, and whether each is on.
	sites map[string]bool
}

var _ host.Host = (*Scheduler)(nil)

// New scheduler for a seed. It must be created and used inside a testing/synctest bubble.
func New(seed uint64, options Options) *Scheduler {
	if options.MaxSteps == 0 {
		options.MaxSteps = 1_000_000
	}
	options.Buggify = options.Buggify.withDefaults()
	s := &Scheduler{
		options:   options,
		rng:       rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		tasks:     map[string]*task{},
		instances: map[string]*Instance{},
		sites:     map[string]bool{},
	}
	s.world = &Instance{s: s}
	return s
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
		// Every goroutine in the bubble is durably blocked when Wait returns. The tests of this
		// package show that a goroutine in a call into C counts as running, so Wait also waits for C
		// code to return.
		synctest.Wait()

		next, due, done, err := s.next()
		if done {
			return err
		}
		// Timeouts expire while no task runs, in the order in which they were made.
		for _, t := range due {
			t.expire()
		}
		if next != nil {
			next.wake <- struct{}{}
		}
	}
}

// next task to run, taken out of the parked state, or the timeouts that are due. It returns done
// when the run has ended, which is when all tasks have returned, or when the run has failed and no
// task is left to unwind, and then err is the failure. It returns no task when only timeouts ended or
// simulated time moved.
func (s *Scheduler) next() (next *task, due []*timeout, done bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.current != nil {
		// Unwinding cannot reach this task, so the bubble will report a deadlock on top of this failure
		// unless something else lets the task go.
		s.fail(fmt.Errorf("task %v is blocked outside the scheduler, after %v", s.current.name, s.current.site))
		s.current = nil
	}

	// A task that is not parked here is blocked outside the scheduler, and the run has failed.
	var names, live []string
	for name, t := range s.tasks {
		if !t.parked {
			continue
		}
		names = append(names, name)
		if !t.inst.crashed {
			live = append(live, name)
		}
	}
	slices.Sort(names)
	slices.Sort(live)
	waits := func() string {
		var waits []string
		for _, name := range live {
			waits = append(waits, name+" at "+s.tasks[name].site)
		}
		return strings.Join(waits, ", ")
	}

	if s.failure == nil && len(s.tasks) == 0 {
		return nil, nil, true, nil
	}
	if s.failure == nil && len(live) == 0 {
		s.unwinding = true
	}
	if s.failure == nil && !s.unwinding && s.steps >= s.options.MaxSteps {
		s.fail(fmt.Errorf("no end after %v steps; waiting: %v", s.steps, waits()))
	}

	if s.failure != nil || s.unwinding {
		// End the tasks one at a time: each ends its goroutine from inside its wait, which runs its
		// deferred calls.
		if len(names) == 0 {
			return nil, nil, true, s.failure
		}
		t := s.tasks[names[0]]
		t.parked = false
		s.current = t
		return t, nil, false, nil
	}

	// A timeout whose context is done some other way needs nothing more.
	s.timeouts = slices.DeleteFunc(s.timeouts, func(t *timeout) bool { return t.Context.Err() != nil })
	earliest := never
	for _, t := range s.timeouts {
		if t.clock.crashed {
			continue
		}
		if t.clock.mono() >= t.at {
			due = append(due, t)
			continue
		}
		earliest = min(earliest, t.clock.trueAt(t.at))
	}

	var eligible []*task
	for _, name := range live {
		t := s.tasks[name]
		if t.clock.mono() >= t.at || (t.cond != nil && t.cond()) {
			eligible = append(eligible, t)
		}
		earliest = min(earliest, t.clock.trueAt(t.at))
	}

	// A task that may run at the instant when timeouts are due runs first or after them, as the seed
	// decides, as a timer and a goroutine race in production.
	if len(due) > 0 && (len(eligible) == 0 || s.rng.IntN(2) == 0) {
		for _, t := range due {
			s.trace = append(s.trace, fmt.Sprintf("%v %v expires", s.now, t.name))
		}
		s.timeouts = slices.DeleteFunc(s.timeouts, func(t *timeout) bool { return slices.Contains(due, t) })
		return nil, due, false, nil
	}

	if len(eligible) == 0 {
		if earliest == never {
			s.fail(fmt.Errorf("deadlock: no task can run: %v", waits()))
			return nil, nil, false, nil
		}
		s.now = earliest
		return nil, nil, false, nil
	}

	t := eligible[s.rng.IntN(len(eligible))]
	t.parked = false
	s.current = t
	s.steps++
	s.trace = append(s.trace, fmt.Sprintf("%v %v resumes from %v", s.now, t.name, t.site))
	return t, nil, false, nil
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

// jitter is a simulated delay for a point where a task lets others run. It requires the mutex.
func (s *Scheduler) jitter() time.Duration {
	if s.options.Jitter == 0 || s.rng.IntN(2) == 0 {
		return 0
	}
	return time.Duration(s.rng.Int64N(int64(s.options.Jitter) + 1))
}

// addTrace adds a line to the trace at the current simulated time. It requires the mutex.
func (s *Scheduler) addTrace(who, format string, args ...any) {
	s.trace = append(s.trace, fmt.Sprintf("%v %v ", s.now, who)+fmt.Sprintf(format, args...))
}

// Lines of the trace so far: one for every step of the scheduler and one for every call to
// [Scheduler.Trace] and other events of the run, in the order in which they happened.
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

// NewInstance of a process, with a name that no other instance of the run has. Its wall clock starts
// at the simulated time, and its monotonic clock at zero. The tasks that it starts are named after it.
func (s *Scheduler) NewInstance(name string) *Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" || strings.Contains(name, "/") {
		panic("sim: an instance needs a name without a slash")
	}
	if _, ok := s.instances[name]; ok {
		panic("sim: an instance named " + name + " exists already")
	}
	i := &Instance{s: s, name: name, created: s.now}
	s.instances[name] = i
	s.addTrace(name, "starts")
	return i
}

// Go as [host.Host] describes, as a task of the test itself, which no crash ends. The task first
// runs when the scheduler picks it, and its name must be one that no other running task has.
func (s *Scheduler) Go(ctx context.Context, name string, f func(ctx context.Context)) host.Task {
	return s.world.Go(ctx, name, f)
}

// NewSemaphore as [host.Host] describes, for tasks of the test itself.
func (s *Scheduler) NewSemaphore(name string, n int) host.Semaphore {
	return s.world.NewSemaphore(name, n)
}

// Sleep as [host.Host] describes, for d of simulated time.
func (s *Scheduler) Sleep(ctx context.Context, d time.Duration) error {
	return s.world.Sleep(ctx, d)
}

// WithTimeout as [host.Host] describes, after d of simulated time.
func (s *Scheduler) WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return s.world.WithTimeout(ctx, d)
}

// Yield as [host.Host] describes.
func (s *Scheduler) Yield(ctx context.Context, site string) {
	s.world.Yield(ctx, site)
}

// Wall is the simulated time, from [Epoch] on.
func (s *Scheduler) Wall() time.Time {
	return s.world.Wall()
}

// Monotonic is the simulated time since [Epoch].
func (s *Scheduler) Monotonic() time.Duration {
	return s.world.Monotonic()
}

// Uint64 is the next number from the seed. The scheduler draws from the same sequence to choose
// tasks, so a run that takes a different turn anywhere draws different numbers from then on.
func (s *Scheduler) Uint64() uint64 {
	return s.world.Uint64()
}

// Buggify as [host.Host] describes, under [Options.Buggify].
func (s *Scheduler) Buggify(ctx context.Context, site string) bool {
	return s.world.Buggify(ctx, site)
}

// Trace adds a line to the trace of the run, at the current simulated time and under the name of the
// task of ctx.
func (s *Scheduler) Trace(ctx context.Context, format string, args ...any) {
	s.world.Trace(ctx, format, args...)
}

// Instance of a simulated process: a [host.Host] with its own clocks, whose tasks a crash can end.
type Instance struct {
	s *Scheduler
	// name of the instance, which is empty for the host of the test itself.
	name string

	// The fields from here on are guarded by the scheduler's mutex.

	// created is the simulated time at which the instance started.
	created time.Duration
	// wallOffset of the wall clock from the simulated time.
	wallOffset time.Duration
	// wallHeld is whether the wall clock stands still, since the simulated time wallHeldAt.
	wallHeld   bool
	wallHeldAt time.Duration
	// held is whether the monotonic clock stands still, since the simulated time heldAt.
	held   bool
	heldAt time.Duration
	// lost is the time for which the monotonic clock stood still before.
	lost    time.Duration
	crashed bool
}

var _ host.Host = (*Instance)(nil)

// mono is the reading of the monotonic clock. It requires the mutex.
func (i *Instance) mono() time.Duration {
	if i.held {
		return i.heldAt - i.created - i.lost
	}
	return i.s.now - i.created - i.lost
}

// trueAt is the simulated time at which the monotonic clock reads m, or never while it stands still.
// It requires the mutex.
func (i *Instance) trueAt(m time.Duration) time.Duration {
	offset := i.created + i.lost
	if i.held || m > never-offset {
		return never
	}
	return m + offset
}

// at is the reading of the monotonic clock after d from now, or never if it overflows. It requires
// the mutex.
func (i *Instance) at(d time.Duration) time.Duration {
	now := i.mono()
	if d > never-now {
		return never
	}
	return now + max(d, 0)
}

// String names the instance, or the test for the host of the test itself.
func (i *Instance) String() string {
	if i.name == "" {
		return "the test"
	}
	return "instance " + i.name
}

// ShiftWall clock by d, forward or backward. The monotonic clock does not change.
func (i *Instance) ShiftWall(d time.Duration) {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	i.wallOffset += d
	i.s.addTrace(i.name, "shifts its wall clock by %v", d)
}

// StopWall clock, so that it reads the same until [Instance.StartWall], except for the shifts of
// [Instance.ShiftWall]. The monotonic clock goes on.
func (i *Instance) StopWall() {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	if !i.wallHeld {
		i.wallHeld, i.wallHeldAt = true, i.s.now
	}
	i.s.addTrace(i.name, "stops its wall clock")
}

// StartWall clock again after [Instance.StopWall]. It goes on from where it stood, so it is behind by
// the time it stood still.
func (i *Instance) StartWall() {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	if i.wallHeld {
		i.wallOffset -= i.s.now - i.wallHeldAt
		i.wallHeld = false
	}
	i.s.addTrace(i.name, "starts its wall clock")
}

// StopMonotonic clock, so that it reads the same until [Instance.StartMonotonic]. The sleeps and
// timeouts of the instance stand still with it. The wall clock goes on.
func (i *Instance) StopMonotonic() {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	if !i.held {
		i.held, i.heldAt = true, i.s.now
	}
	i.s.addTrace(i.name, "stops its monotonic clock")
}

// StartMonotonic clock again after [Instance.StopMonotonic]. It goes on from where it stood.
func (i *Instance) StartMonotonic() {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	if i.held {
		i.lost += i.s.now - i.heldAt
		i.held = false
	}
	i.s.addTrace(i.name, "starts its monotonic clock")
}

// Crash the instance. None of its tasks runs again after its next wait, and a task that waits for
// one of them sees it as returned. A task of the instance that calls Crash goes on until it waits.
// The tasks end when the run ends.
func (i *Instance) Crash() {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	i.crashed = true
	i.s.addTrace(i.name, "crashes")
}

// Crashed reports whether the instance has crashed.
func (i *Instance) Crashed() bool {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	return i.crashed
}

// Go as [host.Host] describes, as a task of the instance. The task first runs when the scheduler
// picks it, and its name, after the name of the instance and a slash, must be one that no other
// running task has. Any task may start one, except while the run ends: then the run fails, unless
// the instance has crashed and the task never runs.
func (i *Instance) Go(ctx context.Context, name string, f func(ctx context.Context)) host.Task {
	s := i.s
	if i.name != "" {
		name = i.name + "/" + name
	}
	t := &task{s: s, inst: i, name: name, wake: make(chan struct{}), parked: true, site: "start", clock: s.world}

	s.mu.Lock()
	if _, ok := s.tasks[name]; ok {
		s.mu.Unlock()
		panic("sim: a task named " + name + " is already running")
	}
	if s.unwinding && !i.crashed {
		s.fail(fmt.Errorf("task %v starts while the run ends, after every task that did not crash has returned", name))
	}
	t.at = s.now + s.jitter()
	s.tasks[name] = t
	s.mu.Unlock()

	go func() {
		s.mu.Lock()
		t.goroutine = goroutineID()
		s.mu.Unlock()

		<-t.wake
		// The deferred call also runs when a wait ends the goroutine because the run has failed or the
		// instance has crashed.
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
		stop := s.failure != nil || i.crashed
		s.mu.Unlock()
		if stop {
			return
		}
		f(context.WithValue(ctx, taskKey{}, t))
	}()
	return t
}

// NewSemaphore as [host.Host] describes. Only tasks of the instance may use it.
func (i *Instance) NewSemaphore(name string, n int) host.Semaphore {
	if i.name != "" {
		name = i.name + "/" + name
	}
	return &semaphore{inst: i, name: name, n: n, free: n}
}

// Sleep as [host.Host] describes, on the monotonic clock of the instance.
func (i *Instance) Sleep(ctx context.Context, d time.Duration) error {
	i.s.mu.Lock()
	at := i.at(d)
	i.s.mu.Unlock()
	i.s.park(ctx, i, "sleep", i, at, func() bool { return ctx.Err() != nil })
	return waitErr(ctx)
}

// WithTimeout as [host.Host] describes, on the monotonic clock of the instance, for a task of the
// instance. The context reports no deadline of its own, because its time is not the time package's.
//
// A context derived from it reports [context.Canceled] after the timeout, where one derived from
// [context.WithTimeout] reports [context.DeadlineExceeded], and its cause is
// [context.DeadlineExceeded] in both. The waits of the host return [context.DeadlineExceeded] for
// both.
func (i *Instance) WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	s := i.s
	t := &timeout{clock: i}
	t.Context, t.cancel = context.WithCancelCause(ctx)

	s.mu.Lock()
	who := "the test"
	if task, _ := ctx.Value(taskKey{}).(*task); task != nil && task.s == s {
		who = task.name
		if i != s.world && task.inst != i {
			s.fail(fmt.Errorf("task %v sets a timeout on the host of %v, which is not its own", task.name, i))
			s.mu.Unlock()
			runtime.Goexit()
		}
	} else if i != s.world {
		s.mu.Unlock()
		panic("sim: a timeout on the host of an instance needs the context of a task of the instance")
	}
	t.at = i.at(d)
	t.name = fmt.Sprintf("timeout of %v after %v", who, d)
	expired := d <= 0 && ctx.Err() == nil
	if d > 0 {
		s.timeouts = append(s.timeouts, t)
	} else if expired {
		s.addTrace(who, "has a timeout of %v, which expires at once", d)
	}
	s.mu.Unlock()

	if expired {
		t.expire()
	}
	return t, func() {
		s.mu.Lock()
		s.timeouts = slices.DeleteFunc(s.timeouts, func(other *timeout) bool { return other == t })
		s.mu.Unlock()
		t.cancel(context.Canceled)
	}
}

// Yield as [host.Host] describes.
func (i *Instance) Yield(ctx context.Context, site string) {
	i.s.mu.Lock()
	at := i.s.now + i.s.jitter()
	i.s.mu.Unlock()
	i.s.park(ctx, i, site, i.s.world, at, nil)
}

// Wall as [host.Host] describes.
func (i *Instance) Wall() time.Time {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	if i.wallHeld {
		return Epoch().Add(i.wallHeldAt + i.wallOffset)
	}
	return Epoch().Add(i.s.now + i.wallOffset)
}

// Monotonic as [host.Host] describes, with its origin when the instance started.
func (i *Instance) Monotonic() time.Duration {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	return i.mono()
}

// Uint64 is the next number from the seed of the run, which every instance shares.
func (i *Instance) Uint64() uint64 {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	return i.s.rng.Uint64()
}

// Buggify as [host.Host] describes, under [Options.Buggify]. Whether a point is on holds for the run,
// in every instance.
func (i *Instance) Buggify(ctx context.Context, site string) bool {
	s := i.s
	t := s.taskOf(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.options.Buggify
	if !b.On {
		return false
	}
	on, ok := s.sites[site]
	if !ok {
		on = s.rng.Float64() < b.Activation
		s.sites[site] = on
		s.addTrace(t.name, "switches buggify %v %v for the run", map[bool]string{true: "on", false: "off"}[on], site)
	}
	if !on {
		return false
	}
	fire := s.rng.Float64() < b.Fire
	if fire {
		s.addTrace(t.name, "buggify %v fires", site)
	} else {
		s.addTrace(t.name, "buggify %v does not fire", site)
	}
	return fire
}

// Trace adds a line to the trace of the run, at the current simulated time and under the name of the
// task of ctx.
func (i *Instance) Trace(ctx context.Context, format string, args ...any) {
	t := i.s.taskOf(ctx)
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	i.s.addTrace(t.name, format, args...)
}

type taskKey struct{}

// task of a [Scheduler]. The fields from goroutine on are guarded by the scheduler's mutex.
type task struct {
	s *Scheduler
	// inst is the instance that the task belongs to.
	inst *Instance
	name string
	// wake is received from by the task's goroutine while it is parked, and sent to by the scheduler.
	wake chan struct{}

	// goroutine is the ID of the task's goroutine.
	goroutine uint64
	parked    bool
	// site names where the task is parked.
	site string
	// at is the reading of the monotonic clock of clock from which the task may run again, or never.
	at    time.Duration
	clock *Instance
	// cond, if not nil, lets the task run again as soon as it returns true, whatever at says.
	cond func() bool
	done bool
}

// park the task of ctx at site, in a wait through the host r, until the scheduler runs it again,
// which it does no earlier than when the monotonic clock of clock reads at, unless cond is not nil
// and returns true before. The scheduler calls cond with its mutex held while no task runs.
//
// If the run has failed, or fails because this wait breaks a rule, or the task's instance has
// crashed and the run is ending, park does not return: it ends the calling goroutine with
// [runtime.Goexit], which runs the goroutine's deferred calls and cannot be recovered from.
func (s *Scheduler) park(ctx context.Context, r *Instance, site string, clock *Instance, at time.Duration, cond func() bool) {
	s.parkAs(ctx, r, false, site, clock, at, cond)
}

// parkAs is [Scheduler.park], where strict says that r must be the host of the task's own instance,
// also when r is the host of the test itself.
func (s *Scheduler) parkAs(ctx context.Context, r *Instance, strict bool, site string, clock *Instance, at time.Duration, cond func() bool) {
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
		case r != t.inst && (strict || r != s.world):
			s.fail(fmt.Errorf("task %v waits at %v through the host of %v, which is not its own", t.name, site, r))
		case deadline:
			s.fail(fmt.Errorf("task %v waits at %v with a context that has a deadline, which simulated time does not reach", t.name, site))
		}
	}
	if s.failure != nil || (s.unwinding && t.inst.crashed) {
		s.mu.Unlock()
		runtime.Goexit()
	}
	s.current = nil
	t.parked, t.site, t.clock, t.at, t.cond = true, site, clock, at, cond
	s.mu.Unlock()

	<-t.wake

	s.mu.Lock()
	stop := s.failure != nil || t.inst.crashed
	s.mu.Unlock()
	if stop {
		runtime.Goexit()
	}
}

// Wait as [host.Task] describes. A task of a crashed instance counts as returned.
func (t *task) Wait(ctx context.Context) error {
	t.s.park(ctx, t.s.world, "wait for "+t.name, t.s.world, never, func() bool {
		return t.done || t.inst.crashed || ctx.Err() != nil
	})
	return waitErr(ctx)
}

// semaphore of an [Instance].
type semaphore struct {
	inst *Instance
	name string
	// n is the number of permits.
	n int
	// free permits, guarded by the scheduler's mutex.
	free int
}

func (m *semaphore) Acquire(ctx context.Context) error {
	s := m.inst.s
	// The task parks even when a permit is free, so that the seed decides who gets it.
	s.parkAs(ctx, m.inst, true, "acquire of "+m.name, s.world, never, func() bool { return m.free > 0 || ctx.Err() != nil })

	if err := waitErr(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m.free--
	return nil
}

func (m *semaphore) Release(ctx context.Context) {
	s := m.inst.s
	s.mu.Lock()
	if m.free == m.n {
		s.mu.Unlock()
		panic("sim: release of a semaphore that is not held")
	}
	m.free++
	at := s.now + s.jitter()
	s.mu.Unlock()
	// The task parks so that a task that waits for the permit can run before this one continues.
	s.parkAs(ctx, m.inst, true, "release of "+m.name, s.world, at, nil)
}

// timeout is the context of [Instance.WithTimeout]: a cancelable copy of its parent, from
// [context.WithCancelCause], which the scheduler cancels with the cause [context.DeadlineExceeded]
// when the timeout expires. Its own error is then [context.DeadlineExceeded]. It is the copy's done
// channel and values that it reports, so a context derived from it is a context derived from the
// copy, which reports [context.Canceled] with that cause.
type timeout struct {
	context.Context
	cancel context.CancelCauseFunc
	clock  *Instance
	// at is the reading of clock's monotonic clock at which the timeout expires. It and name are set
	// before the timeout is shared.
	at      time.Duration
	name    string
	expired atomic.Bool
}

func (t *timeout) Err() error {
	if t.expired.Load() {
		return context.DeadlineExceeded
	}
	return t.Context.Err()
}

func (t *timeout) String() string {
	return t.name
}

// expire the timeout.
func (t *timeout) expire() {
	t.expired.Store(true)
	t.cancel(context.DeadlineExceeded)
}

// waitErr is the error of a wait that ctx ended, which is [context.DeadlineExceeded] also for a
// context derived from a [timeout] that expired, as it is for one derived from [context.WithTimeout].
func waitErr(ctx context.Context) error {
	err := ctx.Err()
	if errors.Is(err, context.Canceled) && errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return err
}
