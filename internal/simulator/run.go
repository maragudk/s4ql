//go:build s4ql_browser

package simulator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/store"
	"maragu.dev/s4ql/internal/store/simstore"
)

// Response to a request: the trace of the run, and what the run came to.
type Response struct {
	// Error says why the request was refused, and is the only field then.
	Error string `json:"error,omitzero"`
	// Request is the request with the default of every field that it left out.
	Request *Request `json:"request,omitzero"`
	Events  []Event  `json:"events,omitzero"`
	Summary *Summary `json:"summary,omitzero"`
	// Hash of the text trace of the scheduler and its next random number, as the tests of the
	// storage core compute it. Two runs with the same hash ran the same way, step for step.
	Hash  string `json:"hash,omitzero"`
	Steps int    `json:"steps,omitzero"`
	// Lines of the text trace of the scheduler, if the request asked for them.
	Lines []string `json:"lines,omitzero"`
	// SchedulerError is the scheduler's own failure, such as a run that exceeded its steps.
	SchedulerError string `json:"schedulerError,omitzero"`
}

// Summary of a run: counts of what happened, by kind.
type Summary struct {
	// Calls of the application, by the op and how each ended, such as "write committed".
	Calls map[string]int `json:"calls"`
	// Requests to the store, by what happened to them, such as "lost".
	Requests map[string]int `json:"requests"`
	// Log is the number of commit objects in the log at the end.
	Log int `json:"log"`
	// Violations of the invariants, and Excused those that the design accepts for the faults.
	Violations int `json:"violations"`
	Excused    int `json:"excused"`
}

// The table of the workload: one column of integers, its primary key.
const tableName = "t"

func workloadTable() *s4qlpb.CreateTable {
	return s4qlpb.CreateTable_builder{
		Table:      new(tableName),
		Columns:    []*s4qlpb.Column{s4qlpb.Column_builder{Name: new("k"), Type: new(s4qlpb.Type_TYPE_INT)}.Build()},
		PrimaryKey: []uint32{0},
	}.Build()
}

// prefix of the database in the store.
const prefix = "db"

// simulate the request in a testing/synctest bubble of its own, under the scheduler with the
// request's seed, in a temporary directory that t removes afterwards.
func simulate(t *testing.T, req Request) Response {
	var res Response
	disks, err := takeDisks(req.Instances)
	if err != nil {
		return Response{Error: "the disks of the instances cannot be made: " + err.Error()}
	}
	defer putDisks(disks)
	synctest.Test(t, func(t *testing.T) {
		world := sim.New(req.Seed, sim.Options{Jitter: 50 * time.Millisecond, Buggify: sim.Buggify{On: req.Buggify}, MaxSteps: 5_000_000})
		r := newRun(t, req, world, duration(req.End), disks, false)
		r.ends(world.Run(r.main), &res)
		if req.Lines {
			res.Lines = world.Lines()
		}
		res.Events = r.rec.events
	})
	res.Request = &req
	return res
}

func newRun(t *testing.T, req Request, world *sim.Scheduler, end time.Duration, disks []disk, live bool) *run {
	r := &run{
		req:       req,
		world:     world,
		rec:       newRecorder(world, end, live),
		dir:       t.TempDir(),
		auto:      req.Workload.On,
		manifests: map[store.Version]*ManifestObject{},
		disks:     disks,
	}
	r.check = newChecker(r)
	return r
}

// ends the run, whose scheduler returned err, with what res says of a run that has ended: the
// failure of the scheduler, the hash of its trace, its steps, and the summary.
func (r *run) ends(err error, res *Response) {
	if err != nil {
		res.SchedulerError = err.Error()
	}
	sum := sha256.Sum256([]byte(strings.Join(r.world.Lines(), "\n") + fmt.Sprint("\n", r.world.Uint64())))
	res.Hash, res.Steps = hex.EncodeToString(sum[:]), r.world.Steps()
	res.Summary = r.summarize()
}

// run is one run: what its tasks share.
type run struct {
	req   Request
	world *sim.Scheduler
	rec   *recorder
	dir   string
	store *simstore.Store
	check *checker

	mu        sync.Mutex
	instances []*instance
	calls     int
	values    int64
	// visitors counts the calls of the schedule, which name their tasks.
	visitors int
	auto     bool
	ending   bool
	// calm is set once the store is calm, which also ends the damaged downloads.
	calm      bool
	armed     []Action
	manifests map[store.Version]*ManifestObject
	// disks of the instances, in their order.
	disks []disk
	// actions is the number of actions so far, which a live session gives as their indexes.
	actions int
	// session of a live run, or nil for a batch run, and endedAt the time at which the session ended.
	session *session
	endedAt time.Duration
}

// instance is one instance of the database, by its name, which a process after process runs.
type instance struct {
	name  string
	lease time.Duration
	// restarts of the instance after a crash, which name its processes.
	restarts int
	p        *process
	// disk through which its processes open their caches.
	disk disk

	// The faults of its network that actions switch, guarded by the mutex of the run. hung is set
	// while no request of the instance gets an answer, and disconnected while each fails at once.
	// inFlight holds the requests of the instance that are on their way, with the cancel of the
	// context of each, which a disconnect calls.
	hung, disconnected bool
	inFlight           map[*pending]context.CancelFunc
}

// process that runs an instance: a simulated machine with its clocks, its cache, and its callers.
type process struct {
	i      *instance
	name   string
	inst   *sim.Instance
	client store.Store
	path   string
	// lifecycle is held while the database of the process is opened or closed.
	lifecycle host.Semaphore

	mu sync.Mutex
	db *core.DB
	// shut is the last database of the process whose close has ended.
	shut   *core.DB
	holder string
	// closed is set while the database is closed by an action, and final once it is closed for the
	// end of the run. failed is set while the last open of the database failed.
	closed, final, failed bool
	// The clocks as the actions left them, and whether they ever had faults that the design does not
	// survive together: see [checker.read].
	wallStopped, monotonicStopped, wallStoppedEver, monotonicStoppedEver, wallBackEver bool
}

func (p *process) current() *core.DB {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.db
}

// bothClocks reports whether both clocks of the process have been wrong in the direction that
// makes a holder believe in its lease too long.
func (p *process) bothClocks() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.monotonicStoppedEver && (p.wallStoppedEver || p.wallBackEver)
}

// procHost is the host of a process as the storage core sees it: the instance of the scheduler, which
// also notes the ID of the lease task and the points of buggify that fire.
type procHost struct {
	*sim.Instance
	r *run
	p *process
}

func (h procHost) Go(ctx context.Context, name string, f func(ctx context.Context)) host.Task {
	if id, ok := strings.CutPrefix(name, "lease of "); ok {
		h.p.mu.Lock()
		h.p.holder = id
		h.p.mu.Unlock()
	}
	return h.Instance.Go(ctx, name, f)
}

func (h procHost) Buggify(ctx context.Context, site string) bool {
	fired := h.Instance.Buggify(ctx, site)
	if fired {
		h.r.rec.add(Event{Kind: "buggify", What: "fired", Instance: h.p.i.name, Process: h.p.name, Site: site})
	}
	return fired
}

// storeHost is the host of the simulated store: the scheduler, whose trace also feeds the events of
// the requests.
type storeHost struct {
	*sim.Scheduler
	r *run
}

func (h storeHost) Trace(ctx context.Context, format string, args ...any) {
	h.Scheduler.Trace(ctx, format, args...)
	h.r.storeEvent(ctx, format, args)
}

// main is the first task of a batch run. It starts the instances, does the actions of the schedule
// at their times, and at the end runs the last checks and lets every process close its database.
func (r *run) main(ctx context.Context) {
	r.setUp(ctx)
	schedule := slices.Clone(r.req.Schedule)
	order := make([]int, len(schedule))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		switch {
		case schedule[a].At < schedule[b].At:
			return -1
		case schedule[a].At > schedule[b].At:
			return 1
		}
		return 0
	})
	for _, i := range order {
		r.sleepUntil(ctx, duration(schedule[i].At))
		r.act(ctx, i, schedule[i])
	}
	r.sleepUntil(ctx, duration(r.req.End))
	r.end(ctx)
}

// setUp the store and the instances, each with its first process.
func (r *run) setUp(ctx context.Context) {
	r.store = simstore.New(storeHost{Scheduler: r.world, r: r}, simstore.Options{
		Latency: duration(r.req.Latency),
		Faults:  r.req.Faults.store(),
	})
	for i := range r.req.Instances {
		in := &instance{name: instanceName(i), lease: duration(r.req.Leases[i%len(r.req.Leases)]), disk: r.disks[i], inFlight: map[*pending]context.CancelFunc{}}
		r.instances = append(r.instances, in)
		r.start(ctx, in, filepath.Join(r.dir, in.name+".db"))
	}
}

// end the run: run the last checks, and let every process close its database.
func (r *run) end(ctx context.Context) {
	r.check.final(ctx)
	r.mu.Lock()
	r.ending = true
	r.mu.Unlock()
	// Nothing after the end is in the trace: the processes close their databases, and requests that
	// are on their way end, so that the run ends. A monotonic clock that stands still would hold
	// the sleeps of its process for good.
	r.rec.stop()
	for _, in := range r.instances {
		in.p.inst.StartMonotonic()
	}
}

// sleepUntil the time t of the scheduler.
func (r *run) sleepUntil(ctx context.Context, t time.Duration) {
	if d := t - r.world.Monotonic(); d > 0 {
		_ = r.world.Sleep(ctx, d)
	}
}

func (r *run) isCalm() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calm
}

func (r *run) isEnding() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ending
}

// start a new process of the instance in, with its cache at path: open its database, and start its
// callers and its supervisor.
func (r *run) start(ctx context.Context, in *instance, path string) {
	name := in.name
	if in.restarts > 0 {
		name = fmt.Sprint(in.name, in.restarts+1)
	}
	inst := r.world.NewInstance(name)
	p := &process{i: in, name: name, inst: inst, path: path}
	p.lifecycle = inst.NewSemaphore("lifecycle", 1)
	p.client = &client{r: r, p: p, inner: r.store.Client(name, inst)}
	in.p = p
	wall, monotonic := millis(inst.Wall().Sub(sim.Epoch())), millis(inst.Monotonic())
	r.rec.add(Event{Kind: "process", What: "started", Instance: in.name, Process: name, Wall: &wall, Monotonic: &monotonic})
	r.open(ctx, p)

	for i := range r.req.Workload.Callers {
		caller := fmt.Sprint("caller-", i)
		inst.Go(ctx, caller, func(ctx context.Context) { r.caller(ctx, p, caller) })
	}
	inst.Go(ctx, "supervisor", func(ctx context.Context) { r.supervise(ctx, p) })
}

// open the database of p, as a task of the scheduler or of p.
func (r *run) open(ctx context.Context, p *process) {
	options := core.Options{
		Lease:   p.i.lease,
		Readers: r.req.Readers,
		Logger:  slog.New(coreHandler{r: r, p: p}),
		VFS:     p.i.disk.name(),
	}
	d, err := core.Open(ctx, procHost{Instance: p.inst, r: r, p: p}, p.client, p.path, prefix, options)
	if err != nil {
		p.mu.Lock()
		p.failed = true
		p.mu.Unlock()
		r.rec.add(Event{Kind: "process", What: "open_failed", Instance: p.i.name, Process: p.name, Error: err.Error()})
		return
	}
	p.mu.Lock()
	p.db, p.failed = d, false
	holder := p.holder
	p.mu.Unlock()
	r.rec.add(Event{Kind: "process", What: "opened", Instance: p.i.name, Process: p.name, Holder: holder})
}

// shutDB closes the database of p unless its close has ended already. The caller holds the
// lifecycle of p.
func (r *run) shutDB(ctx context.Context, p *process) {
	p.mu.Lock()
	d, shut := p.db, p.shut
	p.mu.Unlock()
	if d != nil && d != shut {
		r.closeDB(ctx, p, d)
	}
}

// closeDB of p and wait for its close for up to five minutes.
func (r *run) closeDB(ctx context.Context, p *process, d *core.DB) {
	ctx, cancel := p.inst.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	err := d.Close(ctx)
	p.mu.Lock()
	p.shut = d
	p.mu.Unlock()
	e := Event{Kind: "process", What: "closed", Instance: p.i.name, Process: p.name}
	if err != nil {
		e.Error = err.Error()
	}
	r.rec.add(e)
}

// supervise the database of p as an application does: when it has stopped for good, close it and
// open it again, and when it could not be opened, open it again. At the end of the run it closes the
// database for good.
func (r *run) supervise(ctx context.Context, p *process) {
	for !r.isEnding() {
		if (r.mustReopen(p) || r.mustRetry(p)) && p.lifecycle.Acquire(ctx) == nil {
			// An action may have closed or opened the database while the supervisor waited.
			switch {
			case r.isEnding():
			case r.mustReopen(p):
				d := p.current()
				r.rec.add(Event{Kind: "process", What: "stopped", Instance: p.i.name, Process: p.name, Error: errText(d.Err())})
				r.closeDB(ctx, p, d)
				r.open(ctx, p)
			case r.mustRetry(p):
				r.open(ctx, p)
			}
			p.lifecycle.Release(ctx)
		}
		_ = p.inst.Sleep(ctx, 2*time.Second)
	}
	if p.lifecycle.Acquire(ctx) != nil {
		return
	}
	defer p.lifecycle.Release(ctx)
	p.mu.Lock()
	p.final = true
	p.mu.Unlock()
	r.shutDB(ctx, p)
}

// mustReopen reports whether the database of p has stopped for good, for a reason other than a close
// by an action, and has not been closed since.
func (r *run) mustReopen(p *process) bool {
	p.mu.Lock()
	d, shut, closed := p.db, p.shut, p.closed
	p.mu.Unlock()
	return d != nil && d != shut && !closed && stopped(d)
}

// mustRetry reports whether the last open of the database of p failed, and no action has closed it
// since.
func (r *run) mustRetry(p *process) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failed && !p.closed
}

// stopped reports whether d has stopped for good, by its channel.
func stopped(d *core.DB) bool {
	select {
	case <-d.Done():
		return true
	default:
		return false
	}
}

// caller of the workload on p: until the end of the run, it pauses and then makes a call, a write or
// a read, while the workload is on.
func (r *run) caller(ctx context.Context, p *process, name string) {
	w := r.req.Workload
	for !r.isEnding() {
		pause := time.Millisecond
		if think := duration(w.Think); think > 0 {
			pause += time.Duration(p.inst.Uint64() % uint64(think))
		}
		_ = p.inst.Sleep(ctx, pause)
		r.mu.Lock()
		on, ending := r.auto, r.ending
		r.mu.Unlock()
		switch {
		case ending:
			return
		case !on:
			_ = p.inst.Sleep(ctx, time.Second)
			continue
		}
		write := int(p.inst.Uint64()%100) < w.Writes
		r.call(ctx, p, name, write)
	}
}

// call of the application: one write or read transaction on the database of p, with the timeout of
// the workload.
func (r *run) call(ctx context.Context, p *process, caller string, write bool) {
	r.mu.Lock()
	r.calls++
	call := r.calls
	var value int64
	if write {
		r.values++
		value = r.values
	}
	r.mu.Unlock()
	op := "read"
	if write {
		op = "write"
	}
	started := r.world.Monotonic()
	r.rec.add(Event{Kind: "call", What: "started", Instance: p.i.name, Process: p.name, Call: call, Caller: caller, Op: op, Value: value})
	e := Event{Kind: "call", Instance: p.i.name, Process: p.name, Call: call, Caller: caller, Op: op, Value: value, Started: new(millis(started))}

	ctx, cancel := p.inst.WithTimeout(ctx, duration(r.req.Workload.Timeout))
	defer cancel()
	d := p.current()
	if d == nil {
		e.What, e.Error = "failed", "the instance has no database open"
		r.rec.add(e)
		return
	}
	if write {
		e.What, e.Error = r.write(ctx, d, value)
		r.check.writeEnded(ctx, value, e.What)
	} else {
		before := r.check.readStarted()
		var keys []int64
		var position format.Name
		e.What, e.Error, keys, position = r.read(ctx, d)
		if e.What == "served" {
			e.Keys, e.Position = keys, position.String()
			r.check.readEnded(ctx, p, before, keys, position)
		}
	}
	r.rec.add(e)
}

// write one row with the key value in a write transaction on d, and say how the call ended, and its
// error.
func (r *run) write(ctx context.Context, d *core.DB, value int64) (string, string) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return refusal(err), err.Error()
	}
	if _, err := tx.Table(ctx, tableName); errors.Is(err, core.ErrNoTable) {
		err = tx.CreateTable(ctx, workloadTable())
		if err != nil {
			_ = tx.Rollback(ctx)
			return "abandoned", err.Error()
		}
	} else if err != nil {
		_ = tx.Rollback(ctx)
		return "abandoned", err.Error()
	}
	if err := tx.Upsert(ctx, tableName, []*s4qlpb.Value{s4qlpb.Value_builder{Integer: new(value)}.Build()}); err != nil {
		_ = tx.Rollback(ctx)
		return "abandoned", err.Error()
	}
	if keep := int64(r.req.Workload.Keep); keep > 0 && value > keep {
		if _, err := tx.Delete(ctx, tableName, []*s4qlpb.Value{s4qlpb.Value_builder{Integer: new(value - keep)}.Build()}); err != nil {
			_ = tx.Rollback(ctx)
			return "abandoned", err.Error()
		}
	}
	switch err := tx.Commit(ctx); {
	case err == nil:
		return "committed", ""
	case errors.Is(err, core.ErrOutcomeUnknown):
		return "unknown", err.Error()
	default:
		return "not_committed", err.Error()
	}
}

// read the keys of the table of the workload in a read transaction on d, and say how the call
// ended, its error, and what it saw.
func (r *run) read(ctx context.Context, d *core.DB) (string, string, []int64, format.Name) {
	rtx, err := d.Read(ctx)
	if err != nil {
		return refusal(err), err.Error(), nil, format.Name{}
	}
	defer func() { _ = rtx.Close(ctx) }()
	keys := []int64{}
	for row, err := range rtx.Scan(ctx, tableName) {
		if errors.Is(err, core.ErrNoTable) {
			break
		}
		if err != nil {
			return "failed", err.Error(), nil, format.Name{}
		}
		keys = append(keys, row[0].GetInteger())
	}
	return "served", "", keys, rtx.Position()
}

// refusal of a call at its start, in a word.
func refusal(err error) string {
	switch {
	case errors.Is(err, core.ErrNoLease):
		return "refused"
	case errors.Is(err, core.ErrStopped):
		return "stopped"
	case errors.Is(err, core.ErrBusy):
		return "busy"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "timed_out"
	default:
		return "failed"
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// coreEvent of the process p, from its logger.
func (r *run) coreEvent(_ context.Context, p *process, name, level string, attrs map[string]any) {
	r.rec.add(Event{Kind: "core", Event: name, Level: level, Instance: p.i.name, Process: p.name, Attrs: attrs})
}

// act on the action a, the action with index i in the schedule, as the first task.
func (r *run) act(ctx context.Context, i int, a Action) {
	e := Event{Kind: "action", Action: &i, Do: a.Do, Instance: a.Instance, For: a.For, By: a.By, Text: "done"}
	ignore := func(why string) { e.What, e.Text = "ignored", why }
	e.What = "done"
	var in *instance
	var p *process
	if a.Instance != "" {
		in = r.instances[a.Instance[0]-'a']
		p = in.p
	}
	crashed := p != nil && p.inst.Crashed()
	needsLive := map[string]bool{"write": true, "read": true, "crash": true, "close": true, "open": true, "cutOff": true,
		"shiftWall": true, "stopWall": true, "startWall": true, "stopMonotonic": true, "startMonotonic": true,
		"pauseClocks": true, "resumeClocks": true}
	switch a.Do {
	case "workload", "hangNetwork", "disconnectNetwork", "failDisk":
		e.On = new(a.On)
	}
	if needsLive[a.Do] && crashed {
		ignore("the process of the instance has crashed")
		r.rec.add(e)
		return
	}
	e.Process = ""
	if p != nil {
		e.Process = p.name
	}

	switch a.Do {
	case "write", "read":
		r.mu.Lock()
		r.visitors++
		task := fmt.Sprint("visitor-", r.visitors)
		r.mu.Unlock()
		p.inst.Go(ctx, task, func(ctx context.Context) { r.call(ctx, p, "visitor", a.Do == "write") })
	case "crash":
		p.inst.Crash()
		r.rec.add(Event{Kind: "process", What: "crashed", Instance: in.name, Process: p.name})
	case "restart":
		if !crashed {
			ignore("the process of the instance has not crashed")
			break
		}
		in.restarts++
		path := filepath.Join(r.dir, fmt.Sprint(in.name, in.restarts+1, ".db"))
		if err := copyCache(p.path, path); err != nil {
			e.What, e.Error, e.Text = "failed", err.Error(), "the cache could not be copied"
			break
		}
		r.rec.add(e)
		r.start(ctx, in, path)
		return
	case "close":
		p.mu.Lock()
		d, closed := p.db, p.closed
		p.mu.Unlock()
		if d == nil || closed {
			ignore("the database of the instance is not open")
			break
		}
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		p.inst.Go(ctx, fmt.Sprint("close-", i), func(ctx context.Context) {
			if p.lifecycle.Acquire(ctx) != nil {
				return
			}
			defer p.lifecycle.Release(ctx)
			r.shutDB(ctx, p)
		})
	case "open":
		// A database that a close of an action stopped, that could not be opened, or whose close by
		// the supervisor was not followed by an open, can be opened.
		p.mu.Lock()
		openable := p.closed || p.db == nil || p.db == p.shut
		p.mu.Unlock()
		if !openable {
			ignore("the database of the instance is open")
			break
		}
		p.inst.Go(ctx, fmt.Sprint("open-", i), func(ctx context.Context) {
			if p.lifecycle.Acquire(ctx) != nil {
				return
			}
			defer p.lifecycle.Release(ctx)
			p.mu.Lock()
			final := p.final
			p.mu.Unlock()
			if !final {
				// The close of the action before may not have run yet.
				r.shutDB(ctx, p)
				p.mu.Lock()
				p.closed = false
				p.mu.Unlock()
				r.open(ctx, p)
			}
		})
	case "cutOff":
		p.client.(*client).inner.CutOff(ctx, duration(a.For))
	case "outage":
		r.store.Outage(ctx, duration(a.For))
	case "calm":
		r.store.Calm(ctx)
		r.mu.Lock()
		r.calm = true
		r.mu.Unlock()
	case "shiftWall", "stopWall", "startWall", "stopMonotonic", "startMonotonic", "pauseClocks", "resumeClocks":
		r.clock(p, a)
	case "hangNetwork", "disconnectNetwork":
		r.network(in, a)
	case "failDisk":
		in.disk.fail(a.On)
	case "loseAnswer", "failRequest":
		r.mu.Lock()
		r.armed = append(r.armed, a)
		r.mu.Unlock()
	case "workload":
		r.mu.Lock()
		r.auto = a.On
		r.mu.Unlock()
	}
	r.rec.add(e)
}

// clock action a on p, with an event of the clocks after it.
func (r *run) clock(p *process, a Action) {
	p.mu.Lock()
	switch a.Do {
	case "shiftWall":
		p.inst.ShiftWall(duration(a.By))
		if a.By < 0 {
			p.wallBackEver = true
		}
	case "stopWall":
		p.inst.StopWall()
		p.wallStopped, p.wallStoppedEver = true, true
	case "startWall":
		p.inst.StartWall()
		p.wallStopped = false
	case "stopMonotonic":
		p.inst.StopMonotonic()
		p.monotonicStopped, p.monotonicStoppedEver = true, true
	case "startMonotonic":
		p.inst.StartMonotonic()
		p.monotonicStopped = false
	case "pauseClocks":
		p.inst.StopWall()
		p.inst.StopMonotonic()
		p.wallStopped, p.wallStoppedEver, p.monotonicStopped, p.monotonicStoppedEver = true, true, true, true
	case "resumeClocks":
		p.inst.StartWall()
		p.inst.StartMonotonic()
		p.wallStopped, p.monotonicStopped = false, false
	}
	wallStopped, monotonicStopped := p.wallStopped, p.monotonicStopped
	p.mu.Unlock()
	wall, monotonic := millis(p.inst.Wall().Sub(sim.Epoch())), millis(p.inst.Monotonic())
	r.rec.add(Event{Kind: "clock", What: a.Do, Instance: p.i.name, Process: p.name, By: a.By,
		Wall: &wall, Monotonic: &monotonic, WallStopped: wallStopped, MonotonicStopped: monotonicStopped})
}

// copyCache at from to to, with its write-ahead log, as files that a crashed process left behind.
// The shared-memory index is not copied: the first connection to the copy rebuilds it from the log.
func copyCache(from, to string) error {
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(from + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(to+suffix, b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// client of the store for one process: the client of the simulated store, which also gives each
// call a record for the events of its request, and makes the faults that actions arm and the damaged
// downloads of [Faults.Damaged].
type client struct {
	r     *run
	p     *process
	inner *simstore.Client
}

// pending is the record of one call to the store, which the events of its request read.
type pending struct {
	p       *process
	request int
	method  string
	key     string
	purpose string
	bytes   int
	// cut is set when the network of the instance is disconnected while the request is on its way. It
	// is guarded by the mutex of the run.
	cut bool
}

type pendingKey struct{}

func (c *client) Get(ctx context.Context, key string) (io.ReadCloser, store.Version, error) {
	k := &pending{p: c.p, method: "get", key: key, purpose: "read"}
	if strings.Contains(key, "/commits/") {
		k.purpose = "download"
	}
	ctx = context.WithValue(ctx, pendingKey{}, k)
	if err := c.blocked(ctx, k); err != nil {
		return nil, "", err
	}
	fault := c.r.trip(k)
	if fault == "failRequest" {
		return nil, "", c.r.failed(k)
	}
	sendCtx, arrived, done := c.away(ctx, k)
	body, version, err := c.inner.Get(sendCtx, key)
	if netErr := arrived(ctx); netErr != nil {
		if err == nil {
			_ = body.Close()
		}
		done()
		return nil, "", netErr
	}
	if err != nil {
		done()
	} else {
		// The content comes over the network after the answer, so a disconnect breaks it off.
		body = &content{ReadCloser: body, r: c.r, k: k, done: done}
	}
	if err == nil && k.purpose == "download" && c.r.req.Faults.Damaged > 0 && !c.r.isCalm() && c.r.chance(c.r.req.Faults.Damaged) {
		body = c.damage(k, body)
	}
	if fault == "loseAnswer" {
		if err == nil {
			_ = body.Close()
		}
		return nil, "", c.r.lostAnswer(k)
	}
	c.r.returned(k, version, err)
	return body, version, err
}

// damage one byte of the content of the download k on its way. The content is read here, so a read
// that breaks off still breaks off where it did, after the bytes before.
func (c *client) damage(k *pending, body io.ReadCloser) io.ReadCloser {
	data, err := io.ReadAll(body)
	_ = body.Close()
	if len(data) > 0 {
		at := int(c.r.world.Uint64() % uint64(len(data)))
		data[at] ^= 0x40
		c.r.rec.add(Event{Kind: "request", What: "damaged", Request: k.request, Instance: c.p.i.name, Process: c.p.name,
			Method: k.method, Key: k.key, Purpose: k.purpose, Text: fmt.Sprintf("byte %v of the download is damaged on its way", at)})
	}
	var rest io.Reader = bytes.NewReader(data)
	if err != nil {
		rest = io.MultiReader(rest, failing{err})
	}
	return io.NopCloser(rest)
}

// failing reader that fails with its error.
type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }

func (c *client) Create(ctx context.Context, key string, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	return c.write(ctx, "create", key, "", content, func(ctx context.Context) (store.Version, error) {
		return c.inner.Create(ctx, key, content, checksum)
	})
}

func (c *client) Swap(ctx context.Context, key string, from store.Version, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	return c.write(ctx, "swap", key, from, content, func(ctx context.Context) (store.Version, error) {
		return c.inner.Swap(ctx, key, from, content, checksum)
	})
}

// write of the given method, which send makes.
func (c *client) write(ctx context.Context, method, key string, from store.Version, content io.ReadSeeker, send func(ctx context.Context) (store.Version, error)) (store.Version, error) {
	data, err := peek(content)
	if err != nil {
		return "", err
	}
	k := &pending{p: c.p, method: method, key: key, bytes: len(data), purpose: c.r.purpose(key, from, data)}
	ctx = context.WithValue(ctx, pendingKey{}, k)
	if err := c.blocked(ctx, k); err != nil {
		return "", err
	}
	fault := c.r.trip(k)
	if fault == "failRequest" {
		return "", c.r.failed(k)
	}
	sendCtx, arrived, done := c.away(ctx, k)
	version, err := send(sendCtx)
	netErr := arrived(ctx)
	done()
	if netErr != nil {
		return "", netErr
	}
	if fault == "loseAnswer" {
		return "", c.r.lostAnswer(k)
	}
	c.r.returned(k, version, err)
	return version, err
}

// errDisconnected is the error of a call to the store while the network of its instance is
// disconnected. It tells the caller nothing about whether the store applied the request.
var errDisconnected = fmt.Errorf("simulator: the network of the instance is disconnected: %w", simstore.ErrFault)

// blocked returns an error if the network of the instance keeps the call k from being sent: at once
// while it is disconnected, and while it hangs once the caller gives up, after waiting for an answer
// that does not come.
func (c *client) blocked(ctx context.Context, k *pending) error {
	r, in := c.r, c.p.i
	r.mu.Lock()
	hung, disconnected := in.hung, in.disconnected
	r.mu.Unlock()
	switch {
	case disconnected:
		r.rec.add(Event{Kind: "request", What: "disconnected", Instance: in.name, Process: c.p.name, Method: k.method, Key: k.key,
			Purpose: k.purpose, Bytes: k.bytes, Text: "the network of the instance is disconnected, so the request fails at once, and is not sent"})
		return errDisconnected
	case hung:
		r.rec.add(Event{Kind: "request", What: "hung", Instance: in.name, Process: c.p.name, Method: k.method, Key: k.key,
			Purpose: k.purpose, Bytes: k.bytes, Text: "the network of the instance hangs, so the request is not sent, and its caller waits for an answer until it gives up"})
		return c.hang(ctx, k)
	}
	return nil
}

// hang the call k until its caller gives up, and return why it gave up.
func (c *client) hang(ctx context.Context, k *pending) error {
	err := fmt.Errorf("simulator: no answer came while the network of the instance hung: %w", c.p.inst.Sleep(ctx, math.MaxInt64))
	c.r.returned(k, "", err)
	return err
}

// away is the context with which the call k is sent, which a disconnect of the network of the
// instance ends, and two functions. arrived returns the error that the network gives the caller in
// place of the answer, if any: after a disconnect, an error, and while the network hangs, none
// until the caller gives up, because the answer is lost. done ends the call's time on the network,
// which for a get is when its content is closed.
func (c *client) away(ctx context.Context, k *pending) (sendCtx context.Context, arrived func(ctx context.Context) error, done func()) {
	r, in := c.r, c.p.i
	sendCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	in.inFlight[k] = cancel
	r.mu.Unlock()
	done = func() {
		r.mu.Lock()
		delete(in.inFlight, k)
		r.mu.Unlock()
		cancel()
	}
	return sendCtx, func(ctx context.Context) error {
		r.mu.Lock()
		cut, hung := k.cut, in.hung
		r.mu.Unlock()
		switch {
		case cut:
			r.rec.add(Event{Kind: "request", What: "disconnected", Request: k.request, Instance: in.name, Process: c.p.name, Method: k.method,
				Key: k.key, Purpose: k.purpose, Text: "the network of the instance was disconnected while the request was on its way, and its caller gets an error"})
			return errDisconnected
		case hung:
			r.rec.add(Event{Kind: "request", What: "hung", Request: k.request, Instance: in.name, Process: c.p.name, Method: k.method,
				Key: k.key, Purpose: k.purpose, Text: "the answer is lost, because the network of the instance hangs, and the caller waits until it gives up"})
			return c.hang(ctx, k)
		}
		return nil
	}, done
}

// content of a get as it comes over the network: a disconnect breaks it off.
type content struct {
	io.ReadCloser
	r    *run
	k    *pending
	done func()
}

func (c *content) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.r.mu.Lock()
	cut := c.k.cut
	c.r.mu.Unlock()
	if err != nil && cut {
		err = errDisconnected
	}
	return n, err
}

func (c *content) Close() error {
	err := c.ReadCloser.Close()
	c.done()
	return err
}

// network switches the fault of the action a, hangNetwork or disconnectNetwork, on the instance in. A
// disconnect ends the calls of the instance that are on their way.
func (r *run) network(in *instance, a Action) {
	r.mu.Lock()
	var cancels []context.CancelFunc
	switch a.Do {
	case "hangNetwork":
		in.hung = a.On
	case "disconnectNetwork":
		in.disconnected = a.On
		if a.On {
			for k, cancel := range in.inFlight {
				k.cut = true
				cancels = append(cancels, cancel)
			}
		}
	}
	r.mu.Unlock()
	// The order does not matter: a cancel only makes a task that waits for its context able to run.
	for _, cancel := range cancels {
		cancel()
	}
}

// peek at the content of a write, from its current offset to its end, and leave the offset where it
// was.
func peek(content io.ReadSeeker) ([]byte, error) {
	start, err := content.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(content)
	if err != nil {
		return nil, err
	}
	if _, err := content.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	return data, nil
}

// purpose of a write of data to key, from the version from for a swap.
func (r *run) purpose(key string, from store.Version, data []byte) string {
	if strings.Contains(key, "/commits/") {
		return "upload"
	}
	next, err := format.ParseManifest(data)
	if err != nil {
		return "write"
	}
	if from == "" {
		return "take"
	}
	r.mu.Lock()
	before := r.manifests[from]
	r.mu.Unlock()
	switch {
	case before == nil:
		return "write"
	case next.Lease == nil:
		return "release"
	case before.Lease == nil || before.Lease.Holder != next.Lease.Holder:
		return "take"
	case before.Head != next.Head.String():
		return "commit"
	default:
		return "renew"
	}
}

// chance of p, from the seed.
func (r *run) chance(p float64) bool {
	return float64(r.world.Uint64()>>11)/(1<<53) < p
}

// trip the first fault that an action armed for the call k, if any, and return its kind.
func (r *run) trip(k *pending) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, a := range r.armed {
		if (a.Request == "any" || a.Request == k.purpose) && (a.Instance == "" || a.Instance == k.p.i.name) {
			r.armed = slices.Delete(r.armed, i, i+1)
			return a.Do
		}
	}
	return ""
}

// errFault is the error of a call that an armed fault made fail. Such an error is none of those of
// [store.Store] that tell a case apart, so for a write its outcome is unknown.
var errFault = fmt.Errorf("simulator: a fault of the schedule: %w", simstore.ErrFault)

// failed call k, which an armed fault ended before its request was sent.
func (r *run) failed(k *pending) error {
	r.rec.add(Event{Kind: "request", What: "failed", Instance: k.p.i.name, Process: k.p.name, Method: k.method, Key: k.key,
		Purpose: k.purpose, Bytes: k.bytes, Text: "an armed fault makes the request fail before it is sent"})
	return fmt.Errorf("the request failed before it was sent: %w", errFault)
}

// lostAnswer of the call k, which an armed fault lost on its way back to the caller.
func (r *run) lostAnswer(k *pending) error {
	r.rec.add(Event{Kind: "request", What: "answer_lost", Request: k.request, Instance: k.p.i.name, Process: k.p.name, Method: k.method,
		Key: k.key, Purpose: k.purpose, Text: "an armed fault loses the answer on its way to the caller"})
	return fmt.Errorf("the answer was lost: %w", errFault)
}

// returned call k to its caller, with the version or the error that the caller gets.
func (r *run) returned(k *pending, version store.Version, err error) {
	e := Event{Kind: "request", What: "returned", Request: k.request, Instance: k.p.i.name, Process: k.p.name, Method: k.method,
		Key: k.key, Purpose: k.purpose, Version: string(version)}
	if err != nil {
		e.Version, e.Error = "", err.Error()
	}
	r.rec.add(e)
}

// storeEvent of a line that the simulated store traced.
func (r *run) storeEvent(ctx context.Context, format string, args []any) {
	e := storeLine(format, args)
	k, _ := ctx.Value(pendingKey{}).(*pending)
	if e.Kind == "request" && k != nil {
		if e.What == "sent" && len(args) > 1 {
			_, _ = fmt.Sscanf(fmt.Sprint(args[1]), "request %d:", &k.request)
		}
		e.Request, e.Instance, e.Process = k.request, k.p.i.name, k.p.name
		e.Method, e.Key, e.Purpose = k.method, k.key, k.purpose
		if e.What == "sent" {
			e.Bytes = k.bytes
		}
	}
	if e.Kind == "store" && e.Process != "" {
		e.Instance = r.instanceOf(e.Process)
	}
	switch e.What {
	case "applied":
		if len(args) > 3 {
			_, _ = fmt.Sscanf(fmt.Sprint(args[3]), "applied as write %d", &e.Write)
		}
		if content, version, ok := r.store.Object(e.Key); ok {
			e.Version = string(version)
			e.Object = r.object(ctx, e.Key, version, content)
		}
	case "read":
		if _, version, ok := r.store.Object(e.Key); ok {
			e.Version = string(version)
		}
	}
	r.rec.add(e)
}

// instanceOf the process with the given name.
func (r *run) instanceOf(process string) string {
	return strings.TrimRight(process, "0123456789")
}

// object that a write to key wrote: what a manifest or a commit object says, which the checker also
// learns.
func (r *run) object(ctx context.Context, key string, version store.Version, content []byte) *Object {
	if strings.HasSuffix(key, "/manifest.json") {
		m, err := format.ParseManifest(content)
		if err != nil {
			return &Object{Error: err.Error()}
		}
		mo := &ManifestObject{Head: m.Head.String()}
		if l := m.Lease; l != nil {
			mo.Lease = &Lease{Holder: l.Holder, Duration: millis(l.Duration), Renewed: millis(l.Renewed.Sub(sim.Epoch())), Salt: fmt.Sprintf("%016x", l.Salt)}
		}
		r.mu.Lock()
		r.manifests[version] = mo
		r.mu.Unlock()
		r.check.manifest(ctx, mo)
		return &Object{Manifest: mo}
	}
	name, err := format.ParseName(key[strings.LastIndexByte(key, '/')+1:])
	if err != nil {
		return &Object{Error: err.Error()}
	}
	c, err := readCommit(content, name)
	if err != nil {
		return &Object{Error: err.Error()}
	}
	r.check.commit(c)
	return &Object{Commit: c}
}

// readCommit object of the given name.
func readCommit(content []byte, name format.Name) (*CommitObject, error) {
	cr := format.NewCommitReader(bytes.NewReader(content), name)
	h, err := cr.Header()
	if err != nil {
		return nil, err
	}
	parent, err := format.NameFromBytes(h.GetParent())
	if err != nil {
		return nil, err
	}
	c := &CommitObject{Name: name.String(), Parent: parent.String()}
	for {
		change, _, err := cr.Next()
		if errors.Is(err, io.EOF) {
			return c, nil
		}
		if err != nil {
			return nil, err
		}
		if change == nil {
			continue
		}
		c.Changes++
		if ct := change.GetCreateTable(); ct != nil {
			c.Tables = append(c.Tables, ct.GetTable())
		}
		if u := change.GetUpsertRows(); u != nil && u.GetTable() == tableName {
			for _, row := range u.GetRows() {
				if v := row.GetValues(); len(v) > 0 {
					c.Keys = append(c.Keys, v[0].GetInteger())
				}
			}
		}
		if d := change.GetDeleteRows(); d != nil && d.GetTable() == tableName {
			for _, key := range d.GetPrimaryKeys() {
				if v := key.GetValues(); len(v) > 0 {
					c.Deletes = append(c.Deletes, v[0].GetInteger())
				}
			}
		}
	}
}

// summarize the run from the counts of its events.
func (r *run) summarize() *Summary {
	r.rec.mu.Lock()
	s := r.rec.summary
	r.rec.mu.Unlock()
	s.Log = r.check.logAtEnd
	return &s
}
