package engine_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/store/simstore"
)

// hostRand is the [stmttest.Rand] of a host, so that the statements of a simulation come from its
// seed.
type hostRand struct {
	h host.Host
}

func (r hostRand) IntN(n int) int  { return int(r.h.Uint64() % uint64(n)) }
func (r hostRand) Uint64() uint64 { return r.h.Uint64() }

// workload of engine statements that callers run against instances of one database, under the
// scheduler, while the store fails, hangs, and loses requests.
type workload struct {
	// instances that run at the start, each with its own cache and its callers.
	instances int
	callers   int
	// ops is the number of statements of each caller.
	ops int
	// think is the longest time that a caller waits before each statement, and timeout the timeout of
	// each statement.
	think, timeout time.Duration
	// durations of the lease, of which each instance draws one.
	durations []time.Duration
	faults    simstore.Faults
	// crashes of a random instance, each followed by a new instance on a copy of its cache or on an
	// empty cache. Under the address sanitizer there are none: see [run.crash].
	crashes int
	// closes of a random instance, each followed by opening it again on its cache.
	closes int
	// bugs planted in the engine.
	bugs []string
}

// hammerWorkload has one instance whose callers hardly wait between their statements, so that two
// statements of one instance often run at once.
var hammerWorkload = workload{
	instances: 1,
	callers:   4,
	ops:       30,
	think:     200 * time.Millisecond,
	timeout:   5 * time.Second,
	durations: []time.Duration{30 * time.Second},
	faults:    simstore.AllFaults(),
}

// defaultWorkload has two instances that compete for the lease, two callers on each, a crash and a
// close, and every fault of the store.
var defaultWorkload = workload{
	instances: 2,
	callers:   2,
	ops:       20,
	think:     10 * time.Second,
	timeout:   5 * time.Second,
	durations: []time.Duration{20 * time.Second, 30 * time.Second},
	faults:    simstore.AllFaults(),
	crashes:   1,
	closes:    1,
}

// attemptState of a statement, as its caller saw it.
type attemptState int

const (
	// pending is a statement whose call did not return, because its instance crashed.
	pending attemptState = iota
	// succeeded is a statement that succeeded: a write that is acknowledged, or a query that gave its
	// rows.
	succeeded
	// sqliteFailed is a statement that failed with SQLite's error.
	sqliteFailed
	// failed is a statement that failed with an error of the core or of the context that means that
	// it did nothing.
	failed
	// unknown is a write whose commit had an unknown outcome.
	unknown
	// skipped is a text that is not supported or holds no statement.
	skipped
)

// attempt of a statement.
type attempt struct {
	id    uint64
	node  string
	src   string
	query bool
	// write is true for a statement that changes the database if it succeeds.
	write bool
	state attemptState
	// err is SQLite's error of a statement that failed with it, as [answer] has it, and rows the
	// answer of a query that succeeded.
	answer answer
	// position is the state that the statement saw, from its event.
	position string
	// acked is the number of writes that were acknowledged when the statement started.
	acked int
}

// idKey is the key of the context of a statement that holds its attempt.
type idKey struct{}

// node is one process: an instance of the host, its cache, its database, and its engine.
type node struct {
	name   string
	inst   *sim.Instance
	path   string
	lease  time.Duration
	client *simstore.Client

	callers   atomic.Int64
	reopening atomic.Bool

	mu sync.Mutex
	d  *core.DB
	e  *engine.Engine
}

func (n *node) current() (*core.DB, *engine.Engine) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.d, n.e
}

// do f as a task of the node's instance, and wait until it has returned.
func (n *node) do(ctx context.Context, name string, f func(ctx context.Context)) {
	_ = n.inst.Go(ctx, name, f).Wait(ctx)
}

// run is one run of a workload under the scheduler.
type run struct {
	t     testing.TB
	w     workload
	world *sim.Scheduler
	store *simstore.Store
	dir   string

	mu         sync.Mutex
	nodes      []*node
	attempts   map[uint64]*attempt
	order      []*attempt
	acked      []uint64
	violations []string
	nextID     uint64
	// q is the sequence of statements that every caller takes its next statement from, so that
	// they meet at the same tables.
	q *sequence
	// crashed databases, which stay open: see [run.crash].
	crashed []*core.DB
}

func (r *run) violate(ctx context.Context, format string, args ...any) {
	r.world.Trace(ctx, "VIOLATION: "+format, args...)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.violations = append(r.violations, fmt.Sprintf(format, args...))
}

// handler of the events of the engines: it writes them into the trace, and records the position of
// each statement in its attempt.
type handler struct {
	r *run
}

func (h handler) Enabled(context.Context, slog.Level) bool { return true }
func (h handler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h handler) WithGroup(string) slog.Handler            { return h }

func (h handler) Handle(ctx context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(rec.Message)
	var position string
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key != engine.AttrSQL {
			fmt.Fprint(&b, " ", a.Key, "=", a.Value)
		}
		if a.Key == engine.AttrPosition {
			position = a.Value.String()
		}
		return true
	})
	h.r.world.Trace(ctx, "%v", b.String())
	if id, ok := ctx.Value(idKey{}).(uint64); ok && rec.Message == engine.EventStatement {
		h.r.mu.Lock()
		h.r.attempts[id].position = position
		h.r.mu.Unlock()
	}
	return nil
}

// open the database of n on its cache, with its engine.
func (r *run) open(ctx context.Context, n *node) error {
	d, err := core.Open(ctx, n.inst, n.client, n.path, "db", core.Options{
		Readers:       2,
		BusyTimeout:   time.Minute,
		ObjectTimeout: 10 * time.Second,
		Lease:         n.lease,
	})
	if err != nil {
		return err
	}
	var options engine.Options
	for _, bug := range r.w.bugs {
		options.Plant(bug)
	}
	options.Logger = slog.New(handler{r})
	n.mu.Lock()
	n.d, n.e = d, engine.New(d, n.inst, options)
	n.mu.Unlock()
	return nil
}

// close the database of n. A close that does not end within five minutes is a violation.
func (r *run) close(ctx context.Context, n *node) {
	ctx, cancel := n.inst.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	d, _ := n.current()
	if err := d.Close(ctx); err != nil {
		r.world.Trace(ctx, "close of %v: %v", n.name, err)
	}
	if ctx.Err() != nil {
		r.violate(ctx, "the close of %v did not end", n.name)
	}
}

// idPattern finds the ID that a statement's text carries.
var idPattern = regexp.MustCompile(`/\* id (\d+) \*/`)

// withID puts the comment of the ID into the text of a statement of slice 1, after its first token,
// where the annotation keeps it.
func withID(src string, id uint64) string {
	s, err := stmt.Check(src)
	if err != nil || s == nil {
		return src
	}
	at := s.Bounds().Start
	for at < len(src) && ('a' <= src[at]|0x20 && src[at]|0x20 <= 'z') {
		at++
	}
	return src[:at] + fmt.Sprint(" /* id ", id, " */ ") + src[at:]
}

// statement runs one statement of the sequence q on the engine e of n, and records it.
func (r *run) statement(ctx context.Context, n *node, e *engine.Engine, q *sequence) {
	src, query := q.next()
	r.mu.Lock()
	r.nextID++
	a := &attempt{id: r.nextID, node: n.name, query: query, acked: len(r.acked)}
	a.src = withID(src, a.id)
	if s, _ := stmt.Check(a.src); s != nil {
		_, isSelect := s.(*stmt.Select)
		a.write = !isSelect
	}
	r.attempts[a.id] = a
	r.order = append(r.order, a)
	r.mu.Unlock()
	ctx = context.WithValue(ctx, idKey{}, a.id)

	var got answer
	var err error
	if query {
		var rows *engine.Rows
		rows, err = e.Query(ctx, a.src)
		if err == nil {
			got, err = readRows(rows)
		}
	} else {
		_, err = e.Exec(ctx, a.src)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var se *sqlerr.Error
	switch {
	case err == nil:
		a.state, a.answer = succeeded, got
		if a.write {
			r.acked = append(r.acked, a.id)
		}
	case errors.As(err, &se):
		a.state, a.answer = sqliteFailed, answer{err: fmt.Sprint(se.Code, "/", se.ExtendedCode, ": ", se.Message)}
	case errors.Is(err, stmt.ErrNotSupported), errors.Is(err, engine.ErrNoStatement):
		a.state = skipped
	case errors.Is(err, core.ErrOutcomeUnknown):
		a.state = unknown
	case errors.Is(err, core.ErrNoLease), errors.Is(err, core.ErrStopped), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		a.state = failed
	default:
		a.state = failed
		r.mu.Unlock()
		r.violate(ctx, "statement %v, %q: %v", a.id, a.src, err)
		r.mu.Lock()
	}
	r.world.Trace(ctx, "statement %v ends as %v: %v", a.id, a.state, err)
}

// readRows of a query to their end.
func readRows(rows *engine.Rows) (answer, error) {
	var a answer
	var columns []string
	for _, c := range rows.Columns() {
		columns = append(columns, c.Name+" "+c.DeclType)
	}
	a.columns = strings.Join(columns, ", ")
	for row, err := range rows.All() {
		if err != nil {
			return answer{}, err
		}
		values := make([]string, len(row))
		for j, v := range row {
			values[j] = fmt.Sprint(v.GetInteger())
		}
		a.rows = append(a.rows, strings.Join(values, ","))
	}
	return a, nil
}

func (s attemptState) String() string {
	return [...]string{"pending", "succeeded", "SQLite's error", "failed", "unknown", "skipped"}[s]
}

// start a node with its callers and the task that opens its database again when it stops.
func (r *run) start(ctx context.Context, name, path string) (*node, []host.Task) {
	inst := r.world.NewInstance(name)
	n := &node{name: name, inst: inst, path: path, lease: r.w.durations[r.world.Uint64()%uint64(len(r.w.durations))]}
	n.client = r.store.Client(name, inst)
	rnd := hostRand{inst}
	if err := r.open(ctx, n); err != nil {
		r.violate(ctx, "open %v: %v", name, err)
		return n, nil
	}
	r.mu.Lock()
	r.nodes = append(r.nodes, n)
	r.mu.Unlock()
	var tasks []host.Task
	n.callers.Store(int64(r.w.callers))
	for i := range r.w.callers {
		tasks = append(tasks, inst.Go(ctx, fmt.Sprint("caller-", i), func(ctx context.Context) {
			defer n.callers.Add(-1)
			for range r.w.ops {
				_ = inst.Sleep(ctx, time.Duration(rnd.IntN(int(r.w.think/time.Millisecond)))*time.Millisecond)
				_, e := n.current()
				if e == nil {
					return
				}
				// Every statement has a timeout, as a request of an application has, of up to timeout, so
				// that a commit can end with its outcome unknown.
				ctx, cancel := inst.WithTimeout(ctx, time.Duration(1+rnd.IntN(int(r.w.timeout/time.Millisecond)))*time.Millisecond)
				r.statement(ctx, n, e, r.q)
				cancel()
			}
		}))
	}
	tasks = append(tasks, inst.Go(ctx, "supervisor", func(ctx context.Context) {
		for n.callers.Load() > 0 {
			d, _ := n.current()
			if d == nil {
				return
			}
			select {
			case <-d.Done():
				if n.reopening.CompareAndSwap(false, true) {
					r.violate(ctx, "%v stopped: %v", n.name, d.Err())
					r.close(ctx, n)
					if err := r.open(ctx, n); err != nil {
						r.violate(ctx, "open %v again: %v", n.name, err)
					}
					n.reopening.Store(false)
				}
			default:
			}
			_ = inst.Sleep(ctx, 2*time.Second)
		}
	}))
	if r.w.closes > 0 {
		tasks = append(tasks, inst.Go(ctx, "restarter", func(ctx context.Context) {
			for range r.w.closes {
				if rnd.IntN(2) == 0 {
					continue
				}
				_ = inst.Sleep(ctx, time.Duration(5_000+rnd.IntN(60_000))*time.Millisecond)
				if n.callers.Load() == 0 || !n.reopening.CompareAndSwap(false, true) {
					return
				}
				r.world.Trace(ctx, "%v is closed and opened again", n.name)
				r.close(ctx, n)
				if err := r.open(ctx, n); err != nil {
					r.violate(ctx, "open %v again: %v", n.name, err)
				}
				n.reopening.Store(false)
			}
		}))
	}
	return n, tasks
}

// crash a random live node, and start a new node on a copy of what its cache left on disk, or on an
// empty cache. The crashed database stays open until the test binary ends: only the core's own tests
// can close the cache of a crashed instance, so the address sanitizer, which reports what is left
// open, runs no crashes.
func (r *run) crash(ctx context.Context, i int) []host.Task {
	r.mu.Lock()
	var live []*node
	for _, n := range r.nodes {
		if !n.inst.Crashed() {
			live = append(live, n)
		}
	}
	r.mu.Unlock()
	if len(live) == 0 {
		return nil
	}
	victim := live[r.world.Uint64()%uint64(len(live))]
	victim.inst.Crash()
	d, _ := victim.current()
	r.mu.Lock()
	r.crashed = append(r.crashed, d)
	r.mu.Unlock()
	name := fmt.Sprint(victim.name, i+2)
	path := filepath.Join(r.dir, name+".db")
	empty := r.world.Uint64()%2 == 0
	r.world.Trace(ctx, "%v crashes, and %v starts on %v", victim.name, name, map[bool]string{true: "an empty cache", false: "a copy of its cache"}[empty])
	if !empty {
		if err := copyCache(victim.path, path); err != nil {
			r.violate(ctx, "copy of the cache: %v", err)
			return nil
		}
	}
	_, tasks := r.start(ctx, name, path)
	return tasks
}

// copyCache at from to to, with its write-ahead log, as a crashed process leaves them, without the
// shared-memory index, which SQLite rebuilds from the log.
func copyCache(from, to string) error {
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(from + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(to+suffix, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// do the workload, and check what happened: see [run.verify].
func (w workload) do(ctx context.Context, world *sim.Scheduler, dir string, t testing.TB) *run {
	r := &run{t: t, w: w, world: world, dir: dir, attempts: map[uint64]*attempt{},
		store: simstore.New(world, simstore.Options{Latency: 50 * time.Millisecond, Faults: w.faults}), q: newSequenceFrom(hostRand{world})}
	var tasks []host.Task
	for i := range w.instances {
		name := string(rune('a' + i))
		_, nodeTasks := r.start(ctx, name, filepath.Join(dir, name+".db"))
		tasks = append(tasks, nodeTasks...)
	}
	if w.crashes > 0 && !asanEnabled {
		tasks = append(tasks, world.Go(ctx, "crasher", func(ctx context.Context) {
			var restarted []host.Task
			for i := range w.crashes {
				_ = world.Sleep(ctx, time.Duration(10_000+world.Uint64()%60_000)*time.Millisecond)
				restarted = append(restarted, r.crash(ctx, i)...)
			}
			for _, task := range restarted {
				_ = task.Wait(ctx)
			}
		}))
	}
	for _, task := range tasks {
		_ = task.Wait(ctx)
	}
	r.store.Calm(ctx)

	var live []*node
	for _, n := range r.nodes {
		if !n.inst.Crashed() {
			live = append(live, n)
		}
	}
	// Some live instance must run a write transaction soon: the store is calm, and nothing crashes.
	// A drop of a table that does not exist runs one and changes nothing.
	served := false
	for ended := world.Monotonic(); !served && world.Monotonic()-ended < 10*time.Minute; {
		for _, n := range live {
			n.do(ctx, "end", func(ctx context.Context) {
				ctx, cancel := n.inst.WithTimeout(ctx, w.timeout)
				defer cancel()
				_, e := n.current()
				var se *sqlerr.Error
				if _, err := e.Exec(ctx, `drop table "no such table"`); errors.As(err, &se) {
					served = true
				}
			})
		}
		if !served {
			_ = world.Sleep(ctx, time.Second)
		}
	}
	if !served && len(live) > 0 {
		r.violate(ctx, "no instance runs a write ten minutes after the end of the workload")
	}
	for _, n := range live {
		n.do(ctx, "closer", func(ctx context.Context) { r.close(ctx, n) })
	}
	if err := r.store.Wait(ctx); err != nil {
		r.violate(ctx, "wait for the store: %v", err)
	}
	r.verify(ctx)
	return r
}

// logCommit is a commit object of the log: its name and the texts of its annotations.
type logCommit struct {
	name  format.Name
	notes []string
}

// verify what happened in the run against SQLite and the log:
//
//   - Every commit object of the log holds one statement, of an attempt that succeeded, whose
//     outcome is unknown, or whose instance crashed while it ran, and no attempt is in it twice.
//   - Every write that was acknowledged is in the log.
//   - The statements of the log, run in SQLite in their order, each succeed.
//   - Each statement that failed with SQLite's error gives that error in SQLite on the state that
//     it saw, and each query that succeeded gives its rows there: the state after a commit of the
//     log, which holds every write that was acknowledged before the statement started.
//   - A new instance with an empty cache replays the log to the state that SQLite has after it.
func (r *run) verify(ctx context.Context) {
	log := readLog(r.store)
	index := map[string]int{format.EmptyLog.String(): 0}
	inLog := map[uint64]int{}
	for k, c := range log {
		index[c.name.String()] = k + 1
		if len(c.notes) != 1 {
			r.violate(ctx, "commit %v holds %v statements", k+1, len(c.notes))
			continue
		}
		m := idPattern.FindStringSubmatch(c.notes[0])
		if m == nil {
			r.violate(ctx, "commit %v holds the statement %q, of no attempt", k+1, c.notes[0])
			continue
		}
		id, _ := strconv.ParseUint(m[1], 10, 64)
		a, ok := r.attempts[id]
		switch {
		case !ok:
			r.violate(ctx, "commit %v holds the statement %v, of no attempt", k+1, id)
		case inLog[id] > 0:
			r.violate(ctx, "the statement %v is in commit %v and commit %v", id, inLog[id], k+1)
		case a.state != succeeded && a.state != unknown && a.state != pending:
			r.violate(ctx, "the statement %v is in commit %v, and its caller was told %v", id, k+1, a.state)
		}
		inLog[id] = k + 1
	}
	for _, id := range r.acked {
		if inLog[id] == 0 {
			r.violate(ctx, "the acknowledged statement %v, %q, is not in the log", id, r.attempts[id].src)
		}
	}

	// The observations of each state of the log: the statements that saw it.
	seen := map[int][]*attempt{}
	for _, a := range r.order {
		if a.state != sqliteFailed && (a.state != succeeded || !a.query || a.write) {
			continue
		}
		if a.position == "" {
			// A syntax error of a statement that saw no state.
			if !strings.HasPrefix(a.answer.err, "1/1: ") {
				r.violate(ctx, "the statement %v, %q, saw no state, and gives %v", a.id, a.src, a.answer)
			}
			continue
		}
		at, ok := index[a.position]
		if !ok {
			r.violate(ctx, "the statement %v saw the state %q, which the log does not have", a.id, a.position)
			continue
		}
		for _, id := range r.acked[:a.acked] {
			if inLog[id] > at {
				r.violate(ctx, "the statement %v saw commit %v, and the statement %v, in commit %v, was acknowledged before it started", a.id, at, id, inLog[id])
			}
		}
		seen[at] = append(seen[at], a)
	}
	o := newOracle(r.t)
	for k := 0; k <= len(log); k++ {
		for _, a := range seen[k] {
			want := o.run(r.t, a.src)
			if !a.query {
				want.columns, want.rows = "", nil
			}
			if got := a.answer.String(); got != want.String() {
				r.violate(ctx, "the statement %v, %q, at commit %v gives %v, and in SQLite %v", a.id, a.src, k, got, want)
			}
		}
		if k < len(log) && len(log[k].notes) == 1 {
			if a := o.run(r.t, log[k].notes[0]); a.err != "" {
				r.violate(ctx, "commit %v, %q, fails in SQLite: %v", k+1, log[k].notes[0], a.err)
			}
		}
	}

	// A new instance replays the whole log into an empty cache.
	n := &node{name: "replay", inst: r.world.NewInstance("replay"), path: filepath.Join(r.dir, "replay.db"), lease: time.Minute}
	n.client = r.store.Client("replay", n.inst)
	n.do(ctx, "replay", func(ctx context.Context) {
		if err := r.open(ctx, n); err != nil {
			r.violate(ctx, "open the replay: %v", err)
			return
		}
		defer r.close(ctx, n)
		ctx, cancel := n.inst.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		d, _ := n.current()
		if err := d.Wait(ctx); err != nil {
			r.violate(ctx, "the replay does not serve: %v", err)
			return
		}
		got, err := coreState(ctx, d)
		if err != nil {
			r.violate(ctx, "the state of the replay: %v", err)
		} else if want := o.state(r.t); got != want {
			r.violate(ctx, "the replay of the log holds\n%v\nand SQLite\n%v", got, want)
		}
	})
}

// readLog of the database in the store s, oldest first.
func readLog(s *simstore.Store) []logCommit {
	data, _, ok := s.Object("db/manifest.json")
	if !ok {
		return nil
	}
	m, err := format.ParseManifest(data)
	if err != nil {
		panic(err)
	}
	var log []logCommit
	for name := m.Head; name != format.EmptyLog; {
		key := name.String()
		data, _, ok := s.Object("db/commits/" + key[:2] + "/" + key)
		if !ok {
			panic("no commit object " + key)
		}
		c := logCommit{name: name}
		r := format.NewCommitReader(strings.NewReader(string(data)), name)
		h, err := r.Header()
		if err != nil {
			panic(err)
		}
		for {
			_, a, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				panic(err)
			}
			if a != nil {
				c.notes = append(c.notes, a.GetStatement().GetSql())
			}
		}
		log = append(log, c)
		if name, err = format.NameFromBytes(h.GetParent()); err != nil {
			panic(err)
		}
	}
	slices.Reverse(log)
	return log
}

// coreState of the database d, as [oracle.state] gives it.
func coreState(ctx context.Context, d *core.DB) (string, error) {
	r, err := d.Read(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close(ctx) }()
	tables, err := r.Tables(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, ct := range tables {
		var columns []string
		for _, c := range ct.GetColumns() {
			columns = append(columns, c.GetName()+" INT")
		}
		var rows []string
		for row, err := range r.Scan(ctx, ct.GetTable()) {
			if err != nil {
				return "", err
			}
			rows = append(rows, fmt.Sprint(row[0].GetInteger()))
		}
		fmt.Fprintf(&b, "%v (%v): %v\n", ct.GetTable(), strings.Join(columns, ", "), strings.Join(rows, " "))
	}
	return b.String(), nil
}

// simulation of one seed of a workload.
type simulation struct {
	r     *run
	err   error
	lines []string
}

// simulate the workload w under the scheduler with seed.
func simulate(t *testing.T, seed uint64, w workload) simulation {
	t.Helper()
	var s simulation
	synctest.Test(t, func(t *testing.T) {
		world := sim.New(seed, sim.Options{Jitter: 50 * time.Millisecond, MaxSteps: 5_000_000})
		dir := t.TempDir()
		s.err = world.Run(func(ctx context.Context) {
			s.r = w.do(ctx, world, dir, t)
		})
		s.lines = world.Lines()
	})
	return s
}

// violations of a simulation, with the scheduler's own failure.
func (s simulation) violations() []string {
	var v []string
	if s.err != nil {
		v = append(v, "scheduler: "+s.err.Error())
	}
	if s.r != nil {
		v = append(v, s.r.violations...)
	}
	return v
}

// simSeeds is n, or the number in the environment variable S4QL_ENGINE_SIM_SEEDS.
func simSeeds(t *testing.T, n int) uint64 {
	t.Helper()
	if v := os.Getenv("S4QL_ENGINE_SIM_SEEDS"); v != "" {
		var err error
		n, err = strconv.Atoi(v)
		is.NotError(t, err)
	}
	return uint64(n)
}

// TestSimulation runs the default workload and the hammer workload for many seeds each, and twice
// for some, whose traces must be the same.
func TestSimulation(t *testing.T) {
	n := simSeeds(t, 50)
	if slow() {
		n = max(n/10, 2)
	}
	for _, c := range []struct {
		name string
		w    workload
	}{{"default", defaultWorkload}, {"hammer", hammerWorkload}} {
		t.Run(c.name, func(t *testing.T) {
			counts := map[string]int{}
			for seed := range n {
				s := simulate(t, seed, c.w)
				if v := s.violations(); len(v) > 0 {
					t.Fatalf("seed %v: %v", seed, strings.Join(v, "\n"))
				}
				for _, a := range s.r.order {
					counts[a.state.String()]++
				}
				counts["commits"] += len(readLog(s.r.store))
				counts["trace lines"] += len(s.lines)
				if seed < 3 {
					again := simulate(t, seed, c.w)
					is.True(t, slices.Equal(s.lines, again.lines), fmt.Sprint("seed ", seed, " gives two traces"))
				}
			}
			t.Logf("%v seeds: %v", n, counts)
		})
	}
}

// TestSimulationSeed runs the seed of the environment variable S4QL_ENGINE_SIM_SEED, with the bug
// of S4QL_ENGINE_SIM_BUG, and logs its trace.
func TestSimulationSeed(t *testing.T) {
	v := os.Getenv("S4QL_ENGINE_SIM_SEED")
	if v == "" {
		t.Skip("S4QL_ENGINE_SIM_SEED is not set")
	}
	seed, err := strconv.ParseUint(v, 10, 64)
	is.NotError(t, err)
	w := defaultWorkload
	if bug := os.Getenv("S4QL_ENGINE_SIM_BUG"); bug != "" {
		w.bugs = []string{bug}
	}
	s := simulate(t, seed, w)
	for _, line := range s.lines {
		t.Log(line)
	}
	for _, v := range s.violations() {
		t.Error(v)
	}
}

// TestSimulation_plantedBugs runs the workload with each planted bug until it finds the bug in two
// seeds, and the same seeds without the bug, which find nothing.
func TestSimulation_plantedBugs(t *testing.T) {
	limit := simSeeds(t, 100)
	// The workload of each bug: the check of an insert in a read transaction of its own needs two
	// inserts of one key on one instance at once.
	bugs := []struct {
		name string
		w    workload
	}{
		{"skipUnique", defaultWorkload},
		{"ackUnknown", defaultWorkload},
		{"checkOutside", hammerWorkload},
		{"keepOpen", defaultWorkload},
	}
	for _, b := range bugs {
		bug := b.name
		t.Run(bug, func(t *testing.T) {
			w := b.w
			w.bugs = []string{bug}
			found, seeds := 0, uint64(0)
			for seed := uint64(0); seed < limit && (found < 2 || os.Getenv("S4QL_ENGINE_SIM_SEEDS") != ""); seed++ {
				seeds++
				if v := simulate(t, seed, w).violations(); len(v) > 0 {
					if found == 0 {
						t.Logf("seed %v finds: %v", seed, v[0])
					}
					found++
					if v := simulate(t, seed, b.w).violations(); len(v) > 0 {
						t.Fatalf("seed %v finds something without the bug: %v", seed, v)
					}
				}
			}
			t.Logf("%v of %v seeds find the bug", found, seeds)
			is.True(t, found >= 2, "the bug is found too rarely")
		})
	}
}
