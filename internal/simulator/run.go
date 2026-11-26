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
	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/sql/sqlerr"
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
}

// The table of the workload, with one column of integers, its primary key, and the statements of
// the calls. A write inserts the row of its key, and creates the table first if the database does
// not have it. A delete deletes the row of one key, and a read selects it.
const (
	tableName = "barcodes"
	createSQL = "create table barcodes (code integer primary key) strict, without rowid"
	insertSQL = "insert into barcodes (code) values (?)"
	deleteSQL = "delete from barcodes where code = ?"
	selectSQL = "select code from barcodes where code = ?"
)

// The ops of the calls, as the events of the calls name them. A call of the box is the visitor's own
// statement, whatever it is.
const (
	opWrite  = "write"
	opDelete = "delete"
	opRead   = "read"
	opSQL    = "sql"
)

// opOf the call that the number u from the seed gives, by the shares of the workload w: a write
// transaction by w.Writes, which deletes by w.Deletes and inserts otherwise, or a read.
func opOf(u uint64, w Workload) string {
	switch {
	case int(u%100) >= w.Writes:
		return opRead
	case int(u/100%100) < w.Deletes:
		return opDelete
	default:
		return opWrite
	}
}

// barcode is the key of the nth write of a run, from 1: a number of 13 digits in the form of an
// EAN-13 barcode, the prefix 57, ten digits that a bijection of n modulo 10^10 gives, and the check
// digit of EAN-13. So the keys of a run are distinct, and do not rise with n. The bijection is four
// rounds of a Feistel network on the two halves of five digits each: a round maps (hi, lo) to
// (lo, hi + f(lo)) modulo 10^5, which can be undone whatever f is.
func barcode(n int64) int64 {
	const half = 100_000
	x := n % (half * half)
	hi, lo := x/half, x%half
	for round := range int64(4) {
		hi, lo = lo, (hi+(lo*lo*31_337+lo*7_919+round*104_729+12_345)%half)%half
	}
	body := 57*half*half + hi*half + lo
	sum := int64(0)
	for i, d := 0, body; i < 12; i, d = i+1, d/10 {
		// The digit at i from the right is at the position 12-i from the left, and EAN-13 weighs
		// the even positions by 3.
		w := int64(1)
		if i%2 == 0 {
			w = 3
		}
		sum += w * (d % 10)
	}
	return body*10 + (10-sum%10)%10
}

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
	if res.Error != "" {
		return res
	}
	res.Request = &req
	return res
}

func newRun(t *testing.T, req Request, world *sim.Scheduler, end time.Duration, disks []disk, live bool) *run {
	return &run{
		req:       req,
		world:     world,
		rec:       newRecorder(world, end, live),
		dir:       t.TempDir(),
		auto:      req.Workload.On,
		manifests: map[store.Version]*ManifestObject{},
		disks:     disks,
	}
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
	keys  knownKeys

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
	// target is the instance that the client calls now, or nil in a run without a client.
	target *instance
	// box is the task of the visitor's statement that is on its way, or nil: see [run.sql].
	box host.Task
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
	// eng is the engine of SQL on db, through which the calls go, and engines counts the engines
	// that the process has had.
	eng     *engine.Engine
	engines int
	// shut is the last database of the process whose close has ended.
	shut   *core.DB
	holder string
	// closed is set while the database is closed by an action, and final once it is closed for the
	// end of the run. failed is set while the last open of the database failed.
	closed, final, failed bool
	// The clocks as the actions left them.
	wallStopped, monotonicStopped bool
}

func (p *process) current() *core.DB {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.db
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
// at their times, and at the end lets every process close its database.
func (r *run) main(ctx context.Context) {
	if !r.setUp(ctx) {
		return
	}
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
	r.end()
}

// setUp the store, the instances, each with its first process, and the client. With
// [Request.Standby], only the first instance starts here, and a task starts the others once it
// serves.
func (r *run) setUp(ctx context.Context) bool {
	r.store = simstore.New(storeHost{Scheduler: r.world, r: r}, simstore.Options{
		Latency: duration(r.req.Latency),
		Faults:  r.req.Faults.store(),
	})
	for i := range r.req.Instances {
		in := &instance{name: instanceName(i), lease: duration(r.req.Leases[i%len(r.req.Leases)]), disk: r.disks[i], inFlight: map[*pending]context.CancelFunc{}}
		r.instances = append(r.instances, in)
		if i > 0 && r.req.Standby {
			r.rec.add(Event{Kind: "process", What: "waiting", Instance: in.name, Text: "the instance starts once a serves"})
			continue
		}
		r.start(ctx, in, cachePath(r.dir, in))
	}
	var gate host.Task
	if r.req.Standby && len(r.instances) > 1 {
		gate = r.world.Go(ctx, "standby", r.standby)
	}
	if name := r.req.Client.Instance; name != "" {
		r.target = r.instances[name[0]-'a']
		r.rec.add(Event{Kind: "client", What: "started", Instance: name})
		r.world.Go(ctx, "client", func(ctx context.Context) { r.client(ctx, gate) })
	}
	return true
}

// cachePath of the first process of the instance in, in the directory dir.
func cachePath(dir string, in *instance) string {
	return filepath.Join(dir, in.name+".db")
}

// end the run: let every process close its database.
func (r *run) end() {
	r.mu.Lock()
	r.ending = true
	r.mu.Unlock()
	// Nothing after the end is in the trace: the processes close their databases, and requests that
	// are on their way end, so that the run ends. A monotonic clock that stands still would hold
	// the sleeps of its process for good.
	r.rec.stop()
	for _, in := range r.instances {
		if in.p != nil {
			in.p.inst.StartMonotonic()
		}
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
		Lease:  p.i.lease,
		Logger: slog.New(coreHandler{r: r, p: p}),
		VFS:    p.i.disk.name(),
	}
	d, err := core.Open(ctx, procHost{Instance: p.inst, r: r, p: p}, p.client, p.path, prefix, options)
	if err != nil {
		p.mu.Lock()
		p.failed = true
		p.mu.Unlock()
		r.rec.add(Event{Kind: "process", What: "open_failed", Instance: p.i.name, Process: p.name, Error: err.Error()})
		return
	}
	// The engine draws the directions in which its queries read from a seed of its own, which the seed
	// of the run, the process, and the number of its engine fix, so that the draws of the scheduler
	// are those of the callers and the faults alone.
	p.mu.Lock()
	p.engines++
	directions := r.req.Seed*1_000_003 + uint64(p.engines)*7919
	p.mu.Unlock()
	for _, b := range []byte(p.i.name + "/" + p.name) {
		directions = directions*31 + uint64(b)
	}
	eng := engine.New(d, procHost{Instance: p.inst, r: r, p: p}, engine.Options{Logger: slog.New(positionHandler{}), Directions: &directions})
	p.mu.Lock()
	p.db, p.eng, p.failed = d, eng, false
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

// caller of the workload on p: until the end of the run, it pauses and then makes a call, a write, a
// delete, or a read, while the workload is on.
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
		r.call(ctx, p, name, opOf(p.inst.Uint64(), w))
	}
}

// call of the application, by its caller: its number, its op, the key that a write inserts, or that
// a delete deletes or a read selects, whether a read expects the row, and when it started.
type call struct {
	n       int
	caller  string
	op      string
	value   int64
	key     int64
	row     bool
	started time.Duration
	// text is the statement of a call of the box.
	text string
	// ended is set once the event that ends the call is in the trace, and commit is the name of the
	// commit object of a write of the box, once the core has logged its commit. Both are guarded by
	// the mutex of the run.
	ended  bool
	commit string
}

// newCall of the caller with the op, which starts now. A write takes the key of the next value of
// the counter of the run. A delete and a read take a key that the application knows, which
// [knownKeys.pickDelete] and [knownKeys.pickRead] pick by a number from the seed. While there is no
// key to pick, the call is a write. A call of the box runs the statement text, and takes nothing
// from the counter or the seed.
func (r *run) newCall(caller, op, text string) *call {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	c := &call{n: r.calls, caller: caller, op: op, started: r.world.Monotonic(), text: text}
	var ok bool
	switch op {
	case opSQL:
		return c
	case opRead:
		c.key, c.row, ok = r.keys.pickRead(r.world.Uint64())
	case opDelete:
		c.key, ok = r.keys.pickDelete(r.world.Uint64())
	}
	if !ok {
		c.op = opWrite
		r.values++
		c.value = barcode(r.values)
	}
	return c
}

// statement of the call: the text of its insert, delete, or select, and its argument, or the text of
// a call of the box, which has none.
func (c *call) statement() (string, []any) {
	switch c.op {
	case opWrite:
		return insertSQL, []any{c.value}
	case opDelete:
		return deleteSQL, []any{c.key}
	case opSQL:
		return c.text, nil
	default:
		return selectSQL, []any{c.key}
	}
}

// event of the call c on the instance in, whose process is p or nil, that says what happened. An
// event that ends the call has the time at which it started.
func (c *call) event(in *instance, p *process, what string) Event {
	sql, args := c.statement()
	e := Event{Kind: "call", What: what, Instance: in.name, Call: c.n, Caller: c.caller, Op: c.op, Value: c.value, SQL: sql, Args: args}
	if c.op == opRead {
		e.Expect = "none"
		if c.row {
			e.Expect = "row"
		}
	}
	if p != nil {
		e.Process = p.name
	}
	if what != "started" {
		e.Started = new(millis(c.started))
	}
	return e
}

// callEnded with the event e, which ends the call c unless it has ended already: the application
// learns how an insert or a delete ended, and the event is recorded.
func (r *run) callEnded(c *call, e Event) {
	r.mu.Lock()
	ended := c.ended
	c.ended = true
	r.mu.Unlock()
	if ended {
		return
	}
	switch c.op {
	case opWrite:
		r.keys.ended(c.value, false, e.What)
	case opDelete:
		r.keys.ended(c.key, true, e.What)
	}
	r.rec.add(e)
}

// call of the application on p, which starts now, in a task of p.
func (r *run) call(ctx context.Context, p *process, caller, op string) {
	c := r.newCall(caller, op, "")
	r.rec.add(c.event(p.i, p, "started"))
	r.serveCall(ctx, p, c)
}

// serveCall c in a task of p: one write, delete, or read on the engine of p, on a connection of its own, with
// the timeout of the workload, and the event that ends the call.
func (r *run) serveCall(ctx context.Context, p *process, c *call) {
	e := c.event(p.i, p, "")
	ctx, cancel := p.inst.WithTimeout(ctx, duration(r.req.Workload.Timeout))
	defer cancel()
	p.mu.Lock()
	eng := p.eng
	p.mu.Unlock()
	if eng == nil {
		e.What, e.Error = "failed", "the instance has no database open"
		r.callEnded(c, e)
		return
	}
	conn := eng.Conn()
	defer func() { _ = conn.Close(ctx) }()
	switch c.op {
	case opWrite:
		e.What, e.Error = r.write(ctx, conn, insertSQL, c.value)
	case opDelete:
		e.What, e.Error = r.write(ctx, conn, deleteSQL, c.key)
	case opSQL:
		r.boxStatement(ctx, conn, c, &e)
	default:
		var keys []int64
		var position format.Name
		e.What, e.Error, keys, position = r.read(ctx, conn, c.key)
		if e.What == "served" {
			e.Keys, e.Position = keys, position.String()
		}
	}
	r.callEnded(c, e)
}

// write runs the statement, an insert or a delete, with the key as its argument, in a write
// transaction on conn, and says how the call ended, and its error. The transaction of an insert
// creates the table first if the database does not have it, so the first write of a run commits both
// statements. A statement that fails in the transaction has no effect, and the transaction stays
// open, so the insert can come again after the create.
func (r *run) write(ctx context.Context, conn *engine.Conn, statement string, key int64) (string, string) {
	if err := conn.Begin(ctx); err != nil {
		return refusal(err), err.Error()
	}
	arg := s4qlpb.Value_builder{Integer: new(key)}.Build()
	_, err := conn.Exec(ctx, statement, arg)
	if statement == insertSQL && noTable(err) {
		if _, err = conn.Exec(ctx, createSQL); err == nil {
			_, err = conn.Exec(ctx, insertSQL, arg)
		}
	}
	if err != nil {
		_ = conn.Rollback(ctx)
		return "abandoned", err.Error()
	}
	switch err := conn.Commit(ctx); {
	case err == nil:
		return "committed", ""
	case errors.Is(err, core.ErrOutcomeUnknown):
		return "unknown", err.Error()
	default:
		return "not_committed", err.Error()
	}
}

// read the row of the key with one select on conn, and say how the call ended, its error, and
// what it saw: the keys, in order, and the position of the read, which the engine gives in its event
// of the statement. A select on a database without the table is served with no rows and with the
// error of the engine: it saw a state, as any other read does.
func (r *run) read(ctx context.Context, conn *engine.Conn, key int64) (string, string, []int64, format.Name) {
	seen := &statementSeen{}
	ctx = context.WithValue(ctx, statementKey{}, seen)
	keys := []int64{}
	rows, err := conn.Query(ctx, selectSQL, s4qlpb.Value_builder{Integer: new(key)}.Build())
	if err == nil {
		for row, rerr := range rows.All() {
			if rerr != nil {
				err = rerr
				break
			}
			keys = append(keys, row[0].GetInteger())
		}
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}
	switch {
	case err == nil && seen.position != nil:
		return "served", "", keys, *seen.position
	case noTable(err) && seen.position != nil:
		return "served", err.Error(), []int64{}, *seen.position
	case err == nil:
		return "failed", "the engine gave no position for the read", nil, format.Name{}
	default:
		return refusal(err), err.Error(), nil, format.Name{}
	}
}

// noTable reports whether err is the engine's error for a statement on the table of the workload in
// a database that does not have it.
func noTable(err error) bool {
	var se *sqlerr.Error
	return errors.As(err, &se) && se.Message == "no such table: "+tableName
}

// statementSeen is what the simulator needs of the engine's event of a statement: the position of
// the state that the statement saw. A read puts one on its context, under statementKey, and
// [positionHandler] fills it in.
type statementSeen struct {
	position *format.Name
}

type statementKey struct{}

// positionHandler is the handler of the logger of an engine. It takes the position from the event
// of each statement whose context carries a [statementSeen], and is disabled for every other
// statement, so that the engine builds no event for it.
type positionHandler struct{}

func (positionHandler) Enabled(ctx context.Context, _ slog.Level) bool {
	_, ok := ctx.Value(statementKey{}).(*statementSeen)
	return ok
}

func (h positionHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h positionHandler) WithGroup(string) slog.Handler { return h }

func (positionHandler) Handle(ctx context.Context, rec slog.Record) error {
	seen, ok := ctx.Value(statementKey{}).(*statementSeen)
	if !ok || rec.Message != "statement" {
		return nil
	}
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key != "statement.position" {
			return true
		}
		if n, err := format.ParseName(a.Value.String()); err == nil {
			seen.position = &n
		}
		return false
	})
	return nil
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

// coreEvent of the process p, from its logger. The core logs its event of a commit on the context of
// the commit, so a write of the box learns there the name of its commit object. A transaction that
// changed nothing has a name too, of a commit object that is never written.
func (r *run) coreEvent(ctx context.Context, p *process, name, level string, attrs map[string]any) {
	if c, ok := ctx.Value(boxKey{}).(*call); ok && name == "commit" && attrs["outcome"] != "empty" {
		if commit, ok := attrs["commit.name"].(string); ok {
			r.mu.Lock()
			c.commit = commit
			r.mu.Unlock()
		}
	}
	r.rec.add(Event{Kind: "core", Event: name, Level: level, Instance: p.i.name, Process: p.name, Attrs: attrs})
}

// act on the action a, the action with index i in the schedule, as the first task.
func (r *run) act(ctx context.Context, i int, a Action) {
	e := Event{Kind: "action", Action: &i, Do: a.Do, Instance: a.Instance, For: a.For, By: a.By, SQL: a.SQL, Text: "done"}
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
	switch a.Do {
	case "hangNetwork", "disconnectNetwork", "failDisk", "client", "loseAnswer", "failRequest":
		// The faults belong to the instance, an armed fault only names the instance of its request,
		// and the client calls an instance whatever runs it.
	default:
		if in != nil && p == nil {
			ignore("the instance has not started")
			r.rec.add(e)
			return
		}
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
		p.inst.Go(ctx, task, func(ctx context.Context) { r.call(ctx, p, "visitor", a.Do) })
	case "crash":
		p.inst.Crash()
		// The calls of the process on the network end with it, and a disconnect has none to end.
		r.mu.Lock()
		for k := range in.inFlight {
			if k.p == p {
				delete(in.inFlight, k)
			}
		}
		r.mu.Unlock()
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
	case "sql":
		if why := r.sql(ctx, i, a.SQL, &e); why != "" {
			ignore(why)
		}
	case "client":
		// A call that is on its way stays on the instance it went to.
		r.mu.Lock()
		hasClient := r.target != nil
		if hasClient {
			r.target = in
		}
		r.mu.Unlock()
		if !hasClient {
			ignore("the run has no client")
		}
	}
	r.rec.add(e)
}

// clock action a on p, with an event of the clocks after it.
func (r *run) clock(p *process, a Action) {
	p.mu.Lock()
	switch a.Do {
	case "shiftWall":
		p.inst.ShiftWall(duration(a.By))
	case "stopWall":
		p.inst.StopWall()
		p.wallStopped = true
	case "startWall":
		p.inst.StartWall()
		p.wallStopped = false
	case "stopMonotonic":
		p.inst.StopMonotonic()
		p.monotonicStopped = true
	case "startMonotonic":
		p.inst.StartMonotonic()
		p.monotonicStopped = false
	case "pauseClocks":
		p.inst.StopWall()
		p.inst.StopMonotonic()
		p.wallStopped, p.monotonicStopped = true, true
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
	// A disconnect ends the wait, as it ends the calls on their way.
	r, in := c.r, c.p.i
	hangCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.mu.Lock()
	prev := in.inFlight[k]
	in.inFlight[k] = func() {
		if prev != nil {
			prev()
		}
		cancel()
	}
	r.mu.Unlock()
	waited := c.p.inst.Sleep(hangCtx, math.MaxInt64)
	r.mu.Lock()
	if prev != nil {
		in.inFlight[k] = prev
	} else {
		delete(in.inFlight, k)
	}
	cut := k.cut
	r.mu.Unlock()
	if cut {
		r.rec.add(Event{Kind: "request", What: "disconnected", Request: k.request, Instance: in.name, Process: c.p.name, Method: k.method,
			Key: k.key, Purpose: k.purpose, Text: "the network of the instance was disconnected while the caller waited for an answer, and its caller gets an error"})
		return errDisconnected
	}
	err := fmt.Errorf("simulator: no answer came while the network of the instance hung: %w", waited)
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
	c.r.mu.Lock()
	cut := c.k.cut
	c.r.mu.Unlock()
	if cut {
		return 0, errDisconnected
	}
	return c.ReadCloser.Read(p)
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
			e.Object = r.object(e.Key, version, content)
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

// object that a write to key wrote: what a manifest or a commit object says.
func (r *run) object(key string, version store.Version, content []byte) *Object {
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
		change, annotation, err := cr.Next()
		if errors.Is(err, io.EOF) {
			return c, nil
		}
		if err != nil {
			return nil, err
		}
		if s := annotation.GetStatement(); s != nil {
			c.Statements = append(c.Statements, Statement{SQL: s.GetSql()})
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
	return &s
}
