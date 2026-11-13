package core_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/store"
	"maragu.dev/s4ql/internal/store/simstore"
)

// prefix of the database of a workload.
const prefix = "tenant/db"

// tracer is a host that also records a trace.
type tracer interface {
	host.Host
	Trace(ctx context.Context, format string, args ...any)
}

// intn is a random number in [0, n) from h.
func intn(h host.Host, n int) int {
	return int(h.Uint64() % uint64(n))
}

// clockFaults that a workload does to the clocks of its instances, as a set of bits.
//
// The lease compares no clock of one machine with another's: a holder compares each of its clocks
// only with itself, and an instance that waits to take the lease waits on its own monotonic clock.
// So the faults move any one clock of an instance that starts with the workload in either direction,
// by small and large amounts, while the instance can reach the store. Two faults are left out, because the design does not
// survive them, and accepts that:
//
//   - Both clocks of one instance wrong in the same direction, so that both show less time since a
//     renewal than has passed: a wall clock set backward or standing still, and a monotonic clock
//     that stands still. A holder that can no longer renew then serves after its lease is over by
//     both of its checks, while another instance takes the lease. So an instance whose monotonic
//     clock stands still has its wall clock moved only forward. The design accepts stale reads when
//     both clocks of the holder fail.
//   - A monotonic clock that runs fast or jumps forward on an instance that waits to take the lease.
//     The wait is then shorter than the holder's lease, and the instance can take over from a holder
//     that is cut off but still serves. The margin covers a rate that is wrong by up to the margin
//     divided by the duration, which is far more than the drift of a real monotonic clock. The
//     harness can hold a monotonic clock still but cannot make one run fast, so this fault is out of
//     reach anyway. A monotonic clock that runs slow is the same fault as one that stands still.
type clockFaults int

const (
	// stopMonotonic holds an instance's monotonic clock still for a while.
	stopMonotonic clockFaults = 1 << iota
	// wallForward sets an instance's wall clock forward, by a small or a large amount, for good.
	wallForward
	// wallBackward sets an instance's wall clock backward, by a small or a large amount, for good.
	wallBackward
	// stopWall holds an instance's wall clock still for a while, and it goes on from where it stood.
	// A holder whose wall clock stands still writes the same wall-clock time at every renewal.
	stopWall
)

// workload that callers run against instances of one database.
type workload struct {
	// instances that run at the start, each with its own cache.
	instances int
	// callers is the number of tasks that use each instance at the same time.
	callers int
	// ops is the number of transactions that each caller runs.
	ops int
	// writes is the share of the transactions, in percent, that write. The others read.
	writes int
	// think is the longest simulated time that a caller waits before each transaction.
	think time.Duration
	// hold is the longest simulated time that a write transaction waits between two of its
	// statements.
	hold time.Duration
	// timeout of each call of a caller, as a request of an application has one.
	timeout time.Duration
	// options that each instance opens the database with.
	options core.Options
	// durations of the lease, of which each instance draws one.
	durations []time.Duration
	// latency is the longest time that a request takes each way.
	latency time.Duration
	// faults of the store.
	faults simstore.Faults
	// damaged is the share of the downloads of a commit object, in a thousand, whose bytes are
	// damaged on their way.
	damaged int
	// faulty is the share of the instances, in percent, whose clocks move.
	faulty int
	// clocks that the workload moves, under the scheduler only.
	clocks clockFaults
	// unmoved draws the clock faults as clocks does, at the same times, and moves no clock. It is the
	// control for a bug that needs a clock to move.
	unmoved bool
	// crashes of a random instance, each followed by a restart on a copy of what it left on disk,
	// under the scheduler only.
	crashes int
	// closes of a random instance while its callers run, each followed by opening it again, as an
	// application that shuts down cleanly and starts again.
	closes int
	// buggify for the run.
	buggify bool
	// steady says that nothing in the workload can keep an instance from renewing its lease in
	// time, so that a read that is refused because the lease has run out by a clock is a violation.
	steady bool
}

// liveness is how long after the end of the workload some instance must serve. The store is calm
// by then, no clock stands still, and nothing crashes. What the faults started before goes on to
// its end: a request that is slow on both ways and then waits to be applied late, a client that is
// cut off, an outage, and the timeout of a caller, which the first term bounds. After that, an
// instance waits for the longest lease and the margin, may find at the end of the wait that a
// write of the holder before it arrived late, and wait once more, and reads the manifest once for
// each wait.
func (w workload) liveness() time.Duration {
	f := w.faults
	quiet := 2*f.SlowBy + f.LateBy + max(f.CutOffFor, f.OutageFor) + w.timeout
	lease := slices.Max(w.durations)
	return quiet + 2*(lease+core.Margin(lease)+w.options.Watch) + 10*time.Second
}

// attempt of a commit, as the caller that made it saw it.
type attempt struct {
	// id of the transaction, which its annotation carries into the commit object.
	id   uint64
	node string
	// parent is the position at which the transaction began.
	parent format.Name
	// effect is the net effect of the transaction by the model, as the lines of its commit object.
	effect []string
	// after is the state of the database after the transaction, by the model.
	after string
	// state is pending until Commit returns, and stays pending if the caller's instance crashed.
	state attemptState
	// returned is the count of the run when Commit returned.
	returned int64
}

type attemptState int

const (
	pending attemptState = iota
	acknowledged
	failed
	// unknownState is a commit whose outcome Commit reported as unknown.
	unknownState
)

// seen is one read: the count of the run when it started, and the commit whose state it saw.
type seen struct {
	started int64
	commit  int
}

// outcome of running a [workload].
type outcome struct {
	commits, emptyCommits, failedCommits, unknownCommits, rollbacks, reads, refusedReads, expiredReads, overtakenReads atomic.Int64
	refusedWrites, abandonedWrites, statements                                                                         atomic.Int64
	// cutCommits are commits whose caller's instance crashed before Commit returned.
	cutCommits atomic.Int64
	// stops of an instance, restarts after a crash, and instances that were closed and opened again
	// while their callers ran.
	stops, restarts, reopens atomic.Int64
	// recoveries are the restarts after a crash whose cache had commits in its write-ahead log that
	// its file alone did not have, which the Open of the restarted instance recovered.
	// recoveredFrames counts the frames of their logs, and mostFrames is the most in one.
	recoveries, recoveredFrames, mostFrames atomic.Int64
	// acked is the highest commit that a caller knows to be acknowledged.
	acked atomic.Int64
	// count of the starts of reads and the returns of commits, which orders them.
	count atomic.Int64
	// serveAfter is how long after the end of the workload an instance served.
	serveAfter time.Duration

	mu sync.Mutex
	// violations of the invariants that the workload checks.
	violations []string
	attempts   []*attempt
	seen       []seen
	// events of the instances, counted by their name and outcome. Under the scheduler only.
	events map[string]int
	// crashed databases, whose files are closed after the run.
	crashed []*core.DB
}

func newOutcome() *outcome {
	return &outcome{events: map[string]int{}}
}

// violate records a violation, also in the trace of h.
func (o *outcome) violate(ctx context.Context, h tracer, format string, args ...any) {
	h.Trace(ctx, "VIOLATION: "+format, args...)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.violations = append(o.violations, fmt.Sprintf(format, args...))
}

// checkClosed records a violation for each cache in dir that a connection still has open after the
// run, as far as [leftOpen] can tell.
func (o *outcome) checkClosed(t *testing.T, dir string) {
	t.Helper()
	open, err := leftOpen(dir)
	is.NotError(t, err)
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, name := range open {
		o.violations = append(o.violations, fmt.Sprint("a connection to the cache ", name, " is still open after the run"))
	}
}

// expected reports whether err from a call of a caller is one that the faults and the design
// explain: a refusal for lack of the lease, a stop, a commit of unknown outcome, and the end of the
// caller's own timeout.
func expected(err error) bool {
	for _, target := range []error{core.ErrNoLease, core.ErrStopped, core.ErrOutcomeUnknown, context.DeadlineExceeded, context.Canceled} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// traceHandler writes the events of an instance into the trace of the run, and counts them.
type traceHandler struct {
	h     tracer
	o     *outcome
	attrs []slog.Attr
}

func (t traceHandler) Enabled(context.Context, slog.Level) bool { return true }

func (t traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	t.attrs = append(slices.Clone(t.attrs), attrs...)
	return t
}

func (t traceHandler) WithGroup(string) slog.Handler { return t }

func (t traceHandler) Handle(ctx context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	kind := r.Message
	add := func(a slog.Attr) bool {
		if a.Key == "" {
			return true
		}
		fmt.Fprint(&b, " ", a.Key, "=", a.Value)
		switch a.Key {
		case core.AttrTransition, core.AttrOutcome, core.AttrUnknownKind:
			kind += "/" + a.Value.String()
		}
		return true
	}
	for _, a := range t.attrs {
		add(a)
	}
	r.Attrs(add)
	t.o.mu.Lock()
	t.o.events[kind]++
	t.o.mu.Unlock()
	t.h.Trace(ctx, "%v", b.String())
	return nil
}

// damagingStore damages the bytes of some downloads of commit objects on their way: it changes
// one byte. The store behind it never changes content, and a network can.
type damagingStore struct {
	store.Store
	h tracer
	// share of the downloads, in a thousand.
	share int
	// damaged counts, for each key, the downloads in a row that were damaged, and twice is set when
	// two in a row were: only then may an instance stop for bytes that do not match their name.
	damaged map[string]int
	twice   *atomic.Bool
}

func (d damagingStore) Get(ctx context.Context, key string) (io.ReadCloser, store.Version, error) {
	body, version, err := d.Store.Get(ctx, key)
	if err != nil || !strings.Contains(key, "/commits/") {
		return body, version, err
	}
	if intn(d.h, 1000) >= d.share {
		d.damaged[key] = 0
		return body, version, err
	}
	if d.damaged[key]++; d.damaged[key] >= 2 {
		d.twice.Store(true)
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		return nil, "", err
	}
	at := intn(d.h, len(data))
	data[at] ^= 0x40
	d.h.Trace(ctx, "byte %v of the download of %v is damaged", at, key[len(key)-8:])
	return io.NopCloser(bytes.NewReader(data)), version, nil
}

// cluster is where a workload runs: the host of the test itself, and a way to start the host of an
// instance.
type cluster struct {
	world tracer
	// instance starts the host of a new instance. Under the scheduler it is a [*sim.Instance].
	instance func(name string) tracer
	// dir for the caches.
	dir string
}

// node is one process: an instance, its cache, its database, and its callers.
type node struct {
	name   string
	host   tracer
	client store.Store
	path   string
	// options that the node opens its database with.
	options core.Options
	// callers that have not returned.
	callers atomic.Int64
	// damagedTwice is set when two downloads in a row of one commit object were damaged for the node.
	damagedTwice atomic.Bool
	// reopening is set while a task closes the node's database and opens it again, so that one
	// task does it at a time, and no database is opened that nothing closes.
	reopening atomic.Bool

	mu sync.Mutex
	db *core.DB
}

func (n *node) current() *core.DB {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.db
}

// do f as a task of the node's instance, and wait until it has returned, because only a task of an
// instance may wait through its host.
func (n *node) do(ctx context.Context, name string, f func(ctx context.Context)) {
	_ = n.host.Go(ctx, name, f).Wait(ctx)
}

// run is one run of a workload: what the tasks of the run share.
type run struct {
	w     workload
	c     cluster
	o     *outcome
	store *simstore.Store
	model *model

	mu    sync.Mutex
	nodes []*node
	// nodeOf each instance ID that was opened: the name of its node, which is the name of its client
	// of the store.
	nodeOf map[string]string
	// holders that may hold the lease at the end: crashed instances, and those that could not
	// release it.
	holders map[string]bool
	// closedAt is when each instance that was closed was closed, by the clock of the store.
	closedAt map[string]time.Duration
}

// The tables that the callers of a workload use: three names, each in two spellings that SQLite
// takes for one, with characters that an identifier must be quoted for, and four shapes.
var (
	tableNames = [][2]string{{"t0", "T0"}, {`q"t'1`, `Q"T'1`}, {"tbl 2;--", "TBL 2;--"}}
	shapes     = []func(name string) *s4qlpb.CreateTable{
		func(name string) *s4qlpb.CreateTable { return tableOf(name, 1, "k", "v") },
		func(name string) *s4qlpb.CreateTable { return tableOf(name, 2, "a", "b", `v "w"`) },
		func(name string) *s4qlpb.CreateTable { return tableOf(name, 1, "select") },
		func(name string) *s4qlpb.CreateTable {
			// The primary key is the third column and then the second.
			ct := tableOf(name, 0, "v", "k2", "k1")
			ct.SetPrimaryKey([]uint32{2, 1})
			return ct
		},
	}
)

// randomRow for the table, with small keys, so that callers meet at the same rows.
func randomRow(h host.Host, ct *s4qlpb.CreateTable) []*s4qlpb.Value {
	values := make([]any, len(ct.GetColumns()))
	for i := range values {
		switch {
		case slices.Contains(ct.GetPrimaryKey(), uint32(i)):
			values[i] = intn(h, 6/len(ct.GetPrimaryKey()))
		case intn(h, 5) == 0:
			values[i] = nil
		default:
			values[i] = intn(h, 100) - 50
		}
	}
	return row(values...)
}

// errAbandon is the error of a statement after which a caller gives its transaction up, because the
// instance refused it for a reason that the faults explain.
var errAbandon = errors.New("abandon")

// statement runs one random statement of a write transaction in the core and in the model, and
// compares what the two say. It returns errAbandon if the core refused it for a reason that the
// faults explain.
func (r *run) statement(ctx context.Context, n *node, tx *core.Tx, m *mtx) error {
	h := n.host
	r.o.statements.Add(1)
	// refused reports what to do about the error of a statement.
	refused := func(what string, err error) error {
		if expected(err) {
			return errAbandon
		}
		r.o.violate(ctx, r.c.world, "%v: %v", what, err)
		return errAbandon
	}

	// The tables that the transaction sees, in a fixed order.
	var existing []string
	for folded := range m.now.tables {
		existing = append(existing, folded)
	}
	slices.Sort(existing)
	slot := tableNames[intn(h, len(tableNames))]
	name := slot[intn(h, 2)]

	pick := intn(h, 100)
	switch {
	case len(existing) == 0 || pick < 12:
		ct := shapes[intn(h, len(shapes))](name)
		created := m.create(ct)
		err := tx.CreateTable(ctx, ct)
		r.c.world.Trace(ctx, "create table %q: %v", name, err)
		switch {
		case created && err != nil:
			return refused("create table", err)
		case !created && errors.Is(err, core.ErrTableExists):
		case !created:
			if err == nil || !expected(err) {
				r.o.violate(ctx, r.c.world, "create table %q, which exists: %v", name, err)
			}
			return errAbandon
		}

	case pick < 18:
		dropped := m.drop(name)
		err := tx.DropTable(ctx, name)
		r.c.world.Trace(ctx, "drop table %q: %v", name, err)
		switch {
		case dropped && err != nil:
			return refused("drop table", err)
		case !dropped && errors.Is(err, core.ErrNoTable):
		case !dropped:
			if err == nil || !expected(err) {
				r.o.violate(ctx, r.c.world, "drop table %q, which does not exist: %v", name, err)
			}
			return errAbandon
		}

	default:
		t := m.now.tables[existing[intn(h, len(existing))]]
		// Either spelling of the name finds the table.
		name := t.create.GetTable()
		if intn(h, 2) == 0 {
			name = strings.ToUpper(name)
		}
		values := randomRow(h, t.create)
		key := t.key(values)
		switch {
		case pick < 60:
			m.upsert(name, values)
			err := tx.Upsert(ctx, name, values)
			r.c.world.Trace(ctx, "upsert %v into %q: %v", text(values), name, err)
			if err != nil {
				return refused("upsert", err)
			}

		case pick < 78:
			_, want := m.remove(name, key)
			found, err := tx.Delete(ctx, name, key)
			r.c.world.Trace(ctx, "delete %v from %q: %v %v", text(key), name, found, err)
			if err != nil {
				return refused("delete", err)
			}
			if found != want {
				r.o.violate(ctx, r.c.world, "a delete of the row %v of %q in a transaction found a row: %v, and the model: %v", text(key), name, found, want)
			}

		case pick < 90:
			got, err := tx.Get(ctx, name, key)
			if err != nil {
				return refused("get", err)
			}
			if want := t.rows[text(key)]; (got == nil) != (want == nil) || text(got) != text(want) {
				r.o.violate(ctx, r.c.world, "a transaction read the row %v of %q as %v, and its own writes make it %v", text(key), name, text(got), text(want))
			}

		default:
			var got []string
			for values, err := range tx.Scan(ctx, name) {
				if err != nil {
					return refused("scan", err)
				}
				got = append(got, text(values))
			}
			var want []string
			for _, values := range t.sorted() {
				want = append(want, text(values))
			}
			if !slices.Equal(got, want) {
				r.o.violate(ctx, r.c.world, "a transaction scanned %q as %v, and its own writes make it %v", name, got, want)
			}
		}
	}
	return nil
}

// write runs one write transaction of a caller of n on d.
func (r *run) write(ctx context.Context, n *node, d *core.DB) {
	o, world := r.o, r.c.world
	tx, err := d.Begin(ctx)
	if err != nil {
		o.refusedWrites.Add(1)
		world.Trace(ctx, "begin: %v", err)
		if !expected(err) {
			o.violate(ctx, world, "begin: %v", err)
		}
		if errors.Is(err, core.ErrNoLease) && intn(n.host, 2) == 0 {
			// As an application that waits for the database.
			_ = d.Wait(ctx)
		}
		return
	}
	rollback := func() {
		if err := tx.Rollback(ctx); err != nil && !expected(err) {
			o.violate(ctx, world, "rollback: %v", err)
		}
	}

	_, before, ok := r.model.at(tx.Position())
	if !ok {
		o.violate(ctx, world, "a write transaction began at the position %v, which the log does not hold", tx.Position())
		rollback()
		return
	}
	m := newTx(before)
	id := n.host.Uint64()
	world.Trace(ctx, "transaction %v begins at %v", id, short(tx.Position()))
	if err := tx.Annotate(ctx, fmt.Sprint("transaction ", id), row(int64(intn(n.host, 100)), "text", nil)); err != nil {
		if !expected(err) {
			o.violate(ctx, world, "annotate: %v", err)
		}
		o.abandonedWrites.Add(1)
		rollback()
		return
	}
	for range 1 + intn(n.host, 5) {
		if err := r.statement(ctx, n, tx, m); err != nil {
			o.abandonedWrites.Add(1)
			rollback()
			return
		}
		n.host.Yield(ctx, "caller: between statements")
		if r.w.hold > 0 && intn(n.host, 4) == 0 {
			_ = n.host.Sleep(ctx, time.Duration(intn(n.host, int(r.w.hold/time.Millisecond)))*time.Millisecond)
		}
	}
	if intn(n.host, 10) == 0 {
		rollback()
		o.rollbacks.Add(1)
		world.Trace(ctx, "rollback of transaction %v", id)
		return
	}

	a := &attempt{id: id, node: n.name, parent: tx.Position(), effect: m.effect(), after: m.now.text()}
	o.mu.Lock()
	o.attempts = append(o.attempts, a)
	o.mu.Unlock()
	err = tx.Commit(ctx)
	returned := o.count.Add(1)
	state := failed
	switch {
	case err == nil:
		state = acknowledged
	case errors.Is(err, core.ErrOutcomeUnknown):
		state = unknownState
	}
	o.mu.Lock()
	a.state, a.returned = state, returned
	o.mu.Unlock()
	world.Trace(ctx, "commit of transaction %v with %v changes: %v", id, len(a.effect), err)

	switch state {
	case acknowledged:
		if len(a.effect) == 0 {
			o.emptyCommits.Add(1)
			return
		}
		o.commits.Add(1)
		commit, _, ok := r.model.of(id)
		if !ok {
			o.violate(ctx, world, "the commit of transaction %v was acknowledged, and the log does not have it", id)
			return
		}
		for {
			high := o.acked.Load()
			if int64(commit) <= high || o.acked.CompareAndSwap(high, int64(commit)) {
				break
			}
		}
	case unknownState:
		o.unknownCommits.Add(1)
	default:
		o.failedCommits.Add(1)
		if !expected(err) {
			o.violate(ctx, world, "commit of transaction %v: %v", id, err)
		}
	}
}

// short form of the name of a commit object for the trace.
func short(name format.Name) string {
	return name.String()[:8]
}

// read runs one read transaction of a caller of n on d, and compares what it sees with the log.
// It reports whether the read was served.
func (r *run) read(ctx context.Context, n *node, d *core.DB) bool {
	o, world := r.o, r.c.world
	before := o.acked.Load()
	started := o.count.Add(1)
	rd, err := d.Read(ctx)
	if err != nil {
		o.refusedReads.Add(1)
		if errors.Is(err, core.ErrNoLease) && strings.Contains(err.Error(), "lease_expired") {
			o.expiredReads.Add(1)
		}
		world.Trace(ctx, "read: %v", err)
		if !expected(err) {
			o.violate(ctx, world, "read: %v", err)
		}
		if errors.Is(err, core.ErrNoLease) && intn(n.host, 2) == 0 {
			_ = d.Wait(ctx)
		}
		return false
	}
	position := rd.Position()
	n.host.Yield(ctx, "caller: inside a read")
	state, err := dump(ctx, rd)
	closeErr := rd.Close(ctx)
	if err = errors.Join(err, closeErr); err != nil {
		// A read that is running when the instance stops is aborted.
		o.refusedReads.Add(1)
		world.Trace(ctx, "read at %v: %v", short(position), err)
		if !errors.Is(err, core.ErrStopped) {
			o.violate(ctx, world, "read at %v: %v", short(position), err)
		}
		return false
	}

	commit, want, ok := r.model.at(position)
	if !ok {
		o.violate(ctx, world, "a read saw the position %v, which the log does not hold", position)
		return false
	}
	o.reads.Add(1)
	if r.model.head() > commit {
		o.overtakenReads.Add(1)
	}
	world.Trace(ctx, "read as of commit %v", commit)
	if got := show(state); got != want.text() {
		o.violate(ctx, world, "a read saw at commit %v the state %q, and the log has %q", commit, got, want.text())
	}
	if int64(commit) < before {
		o.violate(ctx, world, "a read missed commit %v, which was acknowledged before it started: it saw commit %v", before, commit)
	}
	o.mu.Lock()
	o.seen = append(o.seen, seen{started: started, commit: commit})
	o.mu.Unlock()
	return true
}

// open the database of n, with a logger that writes its events into the trace.
func (r *run) open(ctx context.Context, n *node) (*core.DB, error) {
	options := n.options
	if _, ok := r.c.world.(*sim.Scheduler); ok {
		options.Logger = slog.New(traceHandler{h: r.c.world, o: r.o})
	}
	d, err := core.Open(ctx, n.host, n.client, n.path, prefix, options)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.nodeOf[d.ID()] = n.name
	r.mu.Unlock()
	n.mu.Lock()
	n.db = d
	n.mu.Unlock()
	return d, nil
}

// close the database d of n, and note whether it may still hold the lease. A close that does not
// come to its end within five minutes is a violation: every transaction of a caller ends within
// the timeout of the caller, and the release of the lease gives up after the lease's duration.
func (r *run) close(ctx context.Context, n *node, d *core.DB) {
	ctx, cancel := n.host.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	err := d.Close(ctx)
	r.mu.Lock()
	r.closedAt[d.ID()] = r.c.world.Monotonic()
	if err != nil {
		r.holders[d.ID()] = true
	}
	r.mu.Unlock()
	if err != nil {
		r.c.world.Trace(ctx, "close of %v: %v", n.name, err)
	}
	if ctx.Err() != nil {
		r.o.violate(ctx, r.c.world, "the close of %v did not end: %v", n.name, err)
	}
}

// stopped checks why the database d of n stopped, closes it, and opens it again, as an application
// does.
func (r *run) stopped(ctx context.Context, n *node, d *core.DB) {
	cause := d.Err()
	r.o.stops.Add(1)
	r.c.world.Trace(ctx, "%v: %v", n.name, cause)
	// Under buggify, the cache fails to commit after a manifest swap, and says that it was made to.
	// It stops for bytes that do not match their name only after a second download.
	explained := (errors.Is(cause, core.ErrLocal) && r.w.buggify && strings.Contains(cause.Error(), "injected")) ||
		(errors.Is(cause, format.ErrNameMismatch) && n.damagedTwice.Load())
	if !explained {
		r.o.violate(ctx, r.c.world, "the instance stopped: %v", cause)
	}
	r.close(ctx, n, d)
	if _, err := r.open(ctx, n); err != nil {
		r.o.violate(ctx, r.c.world, "open again: %v", err)
	}
}

// isStopped reports whether d has stopped, by its channel.
func isStopped(d *core.DB) bool {
	select {
	case <-d.Done():
		return true
	default:
		return false
	}
}

// start a node with its callers and the task that opens its database again when it stops, and
// return the tasks.
func (r *run) start(ctx context.Context, name, path string) (*node, []host.Task) {
	w, c := r.w, r.c
	h := c.instance(name)
	var sender simstore.Sender
	if inst, ok := h.(*sim.Instance); ok {
		sender = inst
	}
	n := &node{name: name, host: h, path: path, options: w.options}
	n.client = r.store.Client(name, sender)
	if w.damaged > 0 {
		n.client = damagingStore{Store: n.client, h: c.world, share: w.damaged, damaged: map[string]int{}, twice: &n.damagedTwice}
	}
	n.options.Lease = w.durations[intn(c.world, len(w.durations))]
	if _, err := r.open(ctx, n); err != nil {
		r.o.violate(ctx, c.world, "open: %v", err)
		return n, nil
	}
	r.mu.Lock()
	r.nodes = append(r.nodes, n)
	r.mu.Unlock()

	var tasks []host.Task
	n.callers.Store(int64(w.callers))
	for i := range w.callers {
		tasks = append(tasks, n.host.Go(ctx, fmt.Sprint("caller-", i), func(ctx context.Context) {
			defer n.callers.Add(-1)
			for range w.ops {
				if w.think > 0 {
					_ = n.host.Sleep(ctx, time.Duration(intn(n.host, int(w.think/time.Millisecond)))*time.Millisecond)
				}
				d := n.current()
				// Every call has a timeout, as a request of an application has.
				ctx, cancel := n.host.WithTimeout(ctx, w.timeout)
				if intn(n.host, 100) < w.writes {
					r.write(ctx, n, d)
				} else {
					r.read(ctx, n, d)
				}
				cancel()
			}
		}))
	}
	tasks = append(tasks, n.host.Go(ctx, "supervisor", func(ctx context.Context) {
		for n.callers.Load() > 0 {
			if d := n.current(); isStopped(d) && n.reopening.CompareAndSwap(false, true) {
				r.stopped(ctx, n, d)
				n.reopening.Store(false)
			}
			_ = n.host.Sleep(ctx, 2*time.Second)
		}
	}))
	if w.closes > 0 {
		tasks = append(tasks, n.host.Go(ctx, "restarter", func(ctx context.Context) {
			for range w.closes {
				// Each node is closed with a chance of one in two, at a time of its own.
				if intn(n.host, 2) == 0 {
					continue
				}
				_ = n.host.Sleep(ctx, time.Duration(5_000+intn(n.host, 100_000))*time.Millisecond)
				if n.callers.Load() == 0 || !n.reopening.CompareAndSwap(false, true) {
					return
				}
				c.world.Trace(ctx, "%v is closed and opened again", n.name)
				r.close(ctx, n, n.current())
				r.o.reopens.Add(1)
				if _, err := r.open(ctx, n); err != nil {
					r.o.violate(ctx, c.world, "open again: %v", err)
				}
				n.reopening.Store(false)
			}
		}))
	}
	return n, tasks
}

// run the workload w as the task of ctx, in the cluster c, and record what happened in o. It
// checks these invariants:
//
//   - A read sees a state that the log had: the state after one of its commit objects, by a replay
//     of the log in the model.
//   - A read that starts after a commit is acknowledged, by any instance, sees that commit.
//   - A read that starts after a commit returned with an unknown outcome sees that commit, if the
//     log ever holds it: so the instance served nothing while it did not know.
//   - A write transaction reads its own writes, and its commit object is the net effect that the
//     model gives for its statements.
//   - The log holds every acknowledged commit, each transaction at most once, and no commit that is
//     known to have failed.
//   - An instance stops only for a reason that the faults explain, and a call fails only with an
//     error that they explain.
//   - Some instance serves again soon after the end of the workload: see [workload.liveness].
//   - The cache of every instance, also of one that crashed, holds what the log says up to the
//     position that it records.
//   - A new instance with an empty cache replays the whole log to the same tables and rows.
//   - The history of the store agrees: see [run.verifyHistory].
//   - After every instance is closed, the lease is released, or held by one that crashed or could
//     not release it, or by one whose request for the lease arrived after it was closed: see
//     [run.verifyLease].
func (w workload) run(ctx context.Context, c cluster, o *outcome) {
	st := simstore.New(c.world, simstore.Options{Latency: w.latency, Faults: w.faults})
	r := &run{w: w, c: c, o: o, store: st, model: newModel(st, prefix), nodeOf: map[string]string{}, holders: map[string]bool{}, closedAt: map[string]time.Duration{}}

	var tasks []host.Task
	for i := range w.instances {
		name := string(rune('a' + i))
		n, nodeTasks := r.start(ctx, name, filepath.Join(c.dir, name+".db"))
		tasks = append(tasks, nodeTasks...)
		if inst, ok := n.host.(*sim.Instance); ok && w.clocks != 0 {
			tasks = append(tasks, c.world.Go(ctx, "clocks of "+name, func(ctx context.Context) {
				w.moveClocks(ctx, c.world, inst)
			}))
		}
	}
	if _, ok := c.world.(*sim.Scheduler); ok && w.crashes > 0 {
		tasks = append(tasks, c.world.Go(ctx, "crasher", func(ctx context.Context) {
			var restarted []host.Task
			for i := range w.crashes {
				_ = c.world.Sleep(ctx, time.Duration(10_000+intn(c.world, 110_000))*time.Millisecond)
				r.mu.Lock()
				var live []*node
				for _, n := range r.nodes {
					if !n.host.(*sim.Instance).Crashed() {
						live = append(live, n)
					}
				}
				r.mu.Unlock()
				if len(live) == 0 {
					break
				}
				victim := live[intn(c.world, len(live))]
				victim.host.(*sim.Instance).Crash()
				d := victim.current()
				o.mu.Lock()
				o.crashed = append(o.crashed, d)
				o.mu.Unlock()
				r.mu.Lock()
				r.holders[d.ID()] = true
				r.mu.Unlock()

				// The restarted instance finds a copy of the files as the crash left them: the cache and
				// its write-ahead log, with the transactions that are not checkpointed, and without the
				// shared-memory index, which SQLite rebuilds from the log. Nothing opens the copy before
				// the instance does, so its Open recovers the cache from the log.
				name := fmt.Sprint(victim.name, i+2)
				path := filepath.Join(c.dir, name+".db")
				crashedAt, crashedState, err := cacheDump(victim.path)
				if err != nil {
					o.violate(ctx, c.world, "the cache of %v: %v", victim.name, err)
					return
				}
				if err := copyCache(victim.path, path); err != nil {
					o.violate(ctx, c.world, "copy of the cache: %v", err)
					return
				}
				frames, err := walFrames(path)
				if err != nil {
					o.violate(ctx, c.world, "the write-ahead log of %v: %v", name, err)
					return
				}
				// Whether the file of the cache alone, without the log, lacks commits that the cache had.
				filed, err := fileOnly(path)
				behind := err != nil || filed != crashedAt
				c.world.Trace(ctx, "%v restarts on the cache of %v at %v", name, victim.name, short(crashedAt))
				o.restarts.Add(1)
				n, nodeTasks := r.start(ctx, name, path)
				restarted = append(restarted, nodeTasks...)
				if n.current() == nil {
					continue
				}

				// Open recovered the cache from the log, before any task of the new instance ran: it
				// holds what the cache held at the crash, which is what the log says up to its position.
				at, state, err := cacheDump(path)
				commit, want, ok := r.model.at(at)
				switch {
				case err != nil:
					o.violate(ctx, c.world, "the cache of %v after the restart: %v", name, err)
				case at != crashedAt || show(state) != show(crashedState):
					o.violate(ctx, c.world, "the cache of %v is at %v after the restart, and was at %v at the crash", name, short(at), short(crashedAt))
				case !ok:
					o.violate(ctx, c.world, "the cache of %v is at the position %v after the restart, which the log does not hold", name, short(at))
				case show(state) != want.text():
					o.violate(ctx, c.world, "the cache of %v holds at commit %v after the restart the state %q, and the log has %q", name, commit, show(state), want.text())
				case behind:
					o.recoveries.Add(1)
					o.recoveredFrames.Add(frames)
					for {
						most := o.mostFrames.Load()
						if frames <= most || o.mostFrames.CompareAndSwap(most, frames) {
							break
						}
					}
				}
			}
			for _, task := range restarted {
				_ = task.Wait(ctx)
			}
		}))
	}
	for _, task := range tasks {
		if err := task.Wait(ctx); err != nil {
			o.violate(ctx, c.world, "wait: %v", err)
		}
	}
	st.Calm(ctx)

	var live, crashed []*node
	for _, n := range r.nodes {
		if inst, ok := n.host.(*sim.Instance); ok && inst.Crashed() {
			crashed = append(crashed, n)
		} else {
			live = append(live, n)
		}
	}

	// No monotonic clock stands still any more, nothing crashes, and the store makes no new fault
	// happen, so an instance must serve soon.
	served := false
	ended, bound := c.world.Monotonic(), w.liveness()
	for !served && c.world.Monotonic()-ended < bound {
		for _, n := range live {
			n.do(ctx, "end", func(ctx context.Context) {
				if d := n.current(); isStopped(d) {
					r.stopped(ctx, n, d)
				}
				ctx, cancel := n.host.WithTimeout(ctx, w.timeout)
				defer cancel()
				if r.read(ctx, n, n.current()) {
					served = true
				}
			})
		}
		if !served {
			_ = c.world.Sleep(ctx, time.Second)
		}
	}
	o.serveAfter = c.world.Monotonic() - ended
	if !served && len(live) > 0 {
		o.violate(ctx, c.world, "no instance serves %v after the end of the workload", bound)
	}

	for _, n := range live {
		n.do(ctx, "closer", func(ctx context.Context) {
			r.close(ctx, n, n.current())
		})
	}
	// Every request has arrived or is lost, so the log does not change any more.
	if err := st.Wait(ctx); err != nil {
		o.violate(ctx, c.world, "wait for the store: %v", err)
	}
	r.verify(ctx, live, crashed)
}

// moveClocks of inst by a few faults that the seed picks from those of w, at times that the seed
// picks, or by none. An instance either has its monotonic clock stopped, and its wall clock moved only
// forward, or has its wall clock moved in any direction: see [clockFaults].
func (w workload) moveClocks(ctx context.Context, h tracer, inst *sim.Instance) {
	var monotonicKinds, wallKinds []clockFaults
	if w.clocks&stopMonotonic != 0 {
		monotonicKinds = append(monotonicKinds, stopMonotonic)
		if w.clocks&wallForward != 0 {
			monotonicKinds = append(monotonicKinds, wallForward)
		}
	}
	for _, kind := range []clockFaults{wallForward, wallBackward, stopWall} {
		if w.clocks&kind != 0 {
			wallKinds = append(wallKinds, kind)
		}
	}
	if intn(h, 100) >= w.faulty {
		return
	}
	kinds := wallKinds
	if len(monotonicKinds) > 0 && (len(wallKinds) == 0 || intn(h, 2) == 0) {
		kinds = monotonicKinds
	}
	if len(kinds) == 0 {
		return
	}

	for range 1 + intn(h, 4) {
		if err := h.Sleep(ctx, time.Duration(intn(h, 60_000))*time.Millisecond); err != nil {
			return
		}
		// A small move is under two seconds, and a large one up to two hours.
		by := time.Duration(intn(h, 2000)) * time.Millisecond
		if intn(h, 2) == 0 {
			by = time.Duration(intn(h, 7200)) * time.Second
		}
		// A monotonic clock stands still for 20 to 80 seconds, and a wall clock for up to five minutes.
		span := time.Duration(20_000+intn(h, 60_000)) * time.Millisecond
		wallSpan := time.Duration(20_000+intn(h, 280_000)) * time.Millisecond
		drawn := kinds[intn(h, len(kinds))]
		kind := drawn
		if w.unmoved {
			kind = 0
		}
		switch kind {
		case stopMonotonic:
			inst.StopMonotonic()
			_ = h.Sleep(ctx, span)
			inst.StartMonotonic()
		case wallForward:
			inst.ShiftWall(by)
		case wallBackward:
			inst.ShiftWall(-by)
		case stopWall:
			inst.StopWall()
			_ = h.Sleep(ctx, wallSpan)
			inst.StartWall()
		default:
			// The control sleeps as the faults that stop a clock do, so that a seed draws the same
			// numbers at the same times.
			switch drawn {
			case stopMonotonic:
				_ = h.Sleep(ctx, span)
			case stopWall:
				_ = h.Sleep(ctx, wallSpan)
			}
		}
	}
}

// copyCache at from to to, with its write-ahead log, as files that a crashed process left behind.
// The shared-memory index is not copied: the first connection to a cache that no other connection
// has open rebuilds it from the log, whether its file is there or not, and that recovery is what the
// copy is for.
func copyCache(from, to string, suffixes ...string) error {
	if len(suffixes) == 0 {
		suffixes = []string{"", "-wal"}
	}
	for _, suffix := range suffixes {
		src, err := os.Open(from + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		dst, err := os.Create(to + suffix)
		if err != nil {
			_ = src.Close()
			return err
		}
		_, err = io.Copy(dst, src)
		_ = src.Close()
		if err := errors.Join(err, dst.Close()); err != nil {
			return err
		}
	}
	return nil
}

// walFrames is the number of frames in the write-ahead log of the cache at path, from the size of
// the log and the page size in its header, and zero if it has none. Frames after the last commit
// of the log, and those of transactions that are checkpointed already, count too.
func walFrames(path string) (int64, error) {
	data, err := os.ReadFile(path + "-wal")
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(data) < 32) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// The header of the log is 32 bytes, with the page size at offset 8, and each frame is a header
	// of 24 bytes and a page (https://sqlite.org/fileformat2.html#walformat).
	page := int64(binary.BigEndian.Uint32(data[8:12]))
	return (int64(len(data)) - 32) / (24 + page), nil
}

// fileOnly is the position that the file of the cache at path records, read from a copy of the
// file without its write-ahead log, which is removed again. It fails for a file that does not hold
// the table of the position yet.
func fileOnly(path string) (_ format.Name, err error) {
	copied := path + "-file"
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if removeErr := os.Remove(copied + suffix); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
	}()
	if err := copyCache(path, copied, ""); err != nil {
		return format.Name{}, err
	}
	position, _, err := cacheDump(copied)
	return position, err
}

// verify the log against the attempts of the callers and the reads, the caches against the log, a
// replay of the whole log into an empty cache, the history of the store, and the lease.
func (r *run) verify(ctx context.Context, live, crashed []*node) {
	o, h, m := r.o, r.c.world, r.model
	if err := m.sync(); err != nil {
		o.violate(ctx, h, "the log: %v", err)
		return
	}

	o.mu.Lock()
	attempts := slices.Clone(o.attempts)
	reads := slices.Clone(o.seen)
	o.mu.Unlock()
	for _, a := range attempts {
		commit, object, found := m.of(a.id)
		switch {
		case a.state == pending:
			o.cutCommits.Add(1)
		case a.state == failed && found:
			o.violate(ctx, h, "the log has commit %v of transaction %v, whose commit failed", commit, a.id)
		case a.state == acknowledged && !found && len(a.effect) > 0:
			o.violate(ctx, h, "the commit of transaction %v was acknowledged, and the log does not have it", a.id)
		}
		if !found {
			continue
		}
		// Whatever the caller was told, a transaction that is in the log is there as it was made.
		var lines []string
		for _, line := range object.lines {
			if !strings.HasPrefix(line, "note ") {
				lines = append(lines, line)
			}
		}
		_, after, _ := m.at(object.name)
		switch {
		case len(a.effect) == 0:
			o.violate(ctx, h, "the log has commit %v of transaction %v, which changed nothing", commit, a.id)
		case object.parent != a.parent:
			o.violate(ctx, h, "the log has commit %v of transaction %v on top of %v, and the transaction began at %v", commit, a.id, object.parent, a.parent)
		case !slices.Equal(lines, a.effect):
			o.violate(ctx, h, "the log has commit %v of transaction %v with the changes %q, and the caller made changes with the net effect %q", commit, a.id, lines, a.effect)
		case after.text() != a.after:
			o.violate(ctx, h, "the log has after commit %v of transaction %v the state %q, and the caller made %q", commit, a.id, after.text(), a.after)
		}
		if a.state != unknownState {
			continue
		}
		// The instance served nothing while it did not know, and no other instance served a state
		// without the commit once the caller had its answer.
		for _, read := range reads {
			if read.started > a.returned && read.commit < commit {
				o.violate(ctx, h, "a read missed commit %v of transaction %v, whose commit had returned with an unknown outcome before it started: it saw commit %v", commit, a.id, read.commit)
				break
			}
		}
	}

	// The cache of every instance holds what the log says up to its position.
	for _, n := range slices.Concat(live, crashed) {
		position, state, err := cacheDump(n.path)
		if err != nil {
			o.violate(ctx, h, "the cache of %v: %v", n.name, err)
			continue
		}
		commit, want, ok := m.at(position)
		switch {
		case !ok:
			o.violate(ctx, h, "the cache of %v is at the position %v, which the log does not hold", n.name, position)
		case show(state) != want.text():
			o.violate(ctx, h, "the cache of %v holds at commit %v the state %q, and the log has %q", n.name, commit, show(state), want.text())
		}
	}

	r.verifyHistory(ctx)
	r.verifyLease(ctx)
	r.verifyReplay(ctx)

	// An instance loses its lease only to another one. With one node, a new instance starts only
	// when the one before it has crashed or was closed, and what that one sent before can no longer
	// be applied once the new one has taken the lease.
	o.mu.Lock()
	lost := o.events["lease/lost/lost"]
	o.mu.Unlock()
	if lost > 0 && r.w.instances == 1 {
		o.violate(ctx, h, "an instance lost the lease %v times, with no other instance", lost)
	}
	if expired := o.expiredReads.Load(); expired > 0 && r.w.steady {
		o.violate(ctx, h, "%v reads were refused because the lease had run out by a clock, with nothing in the way of a renewal", expired)
	}

}

// verifyLease checks that the lease is released when every instance is closed, unless its holder
// crashed or could not release it, or a request of the holder arrived after the holder was closed:
// a swap that takes a free lease can be applied late, because the manifest without a lease entry
// has the same version whenever the head is the same.
func (r *run) verifyLease(ctx context.Context) {
	o, h := r.o, r.c.world
	data, _, ok := r.store.Object(prefix + "/manifest.json")
	if !ok {
		return
	}
	man, err := format.ParseManifest(data)
	if err != nil {
		o.violate(ctx, h, "the manifest: %v", err)
		return
	}
	if man.Lease != nil && !r.holders[man.Lease.Holder] {
		history := r.store.History()
		last := history[len(history)-1]
		for _, write := range history {
			if write.Key == prefix+"/manifest.json" {
				last = write
			}
		}
		if closed, ok := r.closedAt[man.Lease.Holder]; !ok || last.Time <= closed {
			o.violate(ctx, h, "the lease is still held by %v after every instance was closed", man.Lease.Holder)
		}
	}
}

// verifyReplay opens a new instance with an empty cache, which must replay the whole log to the
// state that the model has.
func (r *run) verifyReplay(ctx context.Context) {
	o, c, m := r.o, r.c, r.model
	n := &node{name: "replay", host: c.instance("replay"), path: filepath.Join(c.dir, "replay.db"), options: r.w.options.Unplanted()}
	var sender simstore.Sender
	if inst, ok := n.host.(*sim.Instance); ok {
		sender = inst
	}
	n.client = r.store.Client(n.name, sender)
	n.options.Lease = r.w.durations[0]
	n.do(ctx, "replay", func(ctx context.Context) {
		d, err := r.open(ctx, n)
		if err != nil {
			o.violate(ctx, c.world, "open for the replay: %v", err)
			return
		}
		wctx, cancel := n.host.WithTimeout(ctx, r.w.liveness())
		err = d.Wait(wctx)
		cancel()
		if err != nil {
			o.violate(ctx, c.world, "the replay of the whole log into an empty cache: %v %v", err, d.Err())
		} else {
			rd, err := d.Read(ctx)
			if err != nil {
				o.violate(ctx, c.world, "read after the replay: %v", err)
			} else {
				state, err := dump(ctx, rd)
				_ = rd.Close(ctx)
				commit, want, ok := m.at(rd.Position())
				if err != nil || !ok || commit != m.head() || show(state) != want.text() {
					o.violate(ctx, c.world, "the replay of the whole log into an empty cache gave at commit %v of %v the state %q, and the log has %q: %v", commit, m.head(), show(state), want.text(), err)
				}
			}
		}
		r.close(ctx, n, d)
	})
	position, state, err := cacheDump(n.path)
	commit, want, ok := m.at(position)
	if err != nil || !ok || commit != m.head() || show(state) != want.text() {
		o.violate(ctx, c.world, "the cache of the replay holds at commit %v of %v the state %q, and the log has %q: %v", commit, m.head(), show(state), want.text(), err)
	}
}

// verifyHistory checks the writes of the manifest that the store applied, in their order, against
// the rules of the manifest:
//
//   - Only its first write creates it.
//   - A write with a lease entry is sent by the instance that the entry names, and a write without
//     one by the instance that the entry before it named: a holder releases only its own lease.
//   - The head changes only to a commit object whose parent is the head before, in a write of the
//     holder: the log never forks and never loses a commit.
//   - Every write changes the version of the manifest, and no version that a write with a lease
//     entry gave ever comes back.
//   - A write that takes the lease from another holder's entry is applied no earlier than that
//     holder's duration and the margin after the write before it. The clock of the store is the true
//     time of the run, and an instance that waits to take the lease waits no shorter in true time
//     than by its own clock, because the workload never makes a monotonic clock run fast.
func (r *run) verifyHistory(ctx context.Context) {
	o, h := r.o, r.c.world
	var before *format.Manifest
	var beforeAt time.Duration
	var beforeVersion store.Version
	versions := map[store.Version]bool{}
	for i, write := range r.store.History() {
		if write.Key != prefix+"/manifest.json" {
			continue
		}
		man, err := format.ParseManifest(write.Content)
		if err != nil {
			o.violate(ctx, h, "write %v of the store is a manifest that cannot be read: %v", i+1, err)
			return
		}
		if write.Create != (before == nil) {
			o.violate(ctx, h, "write %v of the store creates the manifest: %v, and it is the first: %v", i+1, write.Create, before == nil)
		}
		head := format.EmptyLog
		if before != nil {
			head = before.Head
		}
		switch {
		case man.Lease != nil && r.nodeOf[man.Lease.Holder] != write.Client:
			o.violate(ctx, h, "write %v of the store names the holder %v, and %v sent it", i+1, man.Lease.Holder, write.Client)
		case man.Lease == nil && (before == nil || before.Lease == nil || r.nodeOf[before.Lease.Holder] != write.Client):
			o.violate(ctx, h, "write %v of the store releases a lease that %v does not hold", i+1, write.Client)
		}
		if man.Head != head {
			now, _, _ := r.model.at(man.Head)
			was, _, ok := r.model.at(head)
			if !ok || now != was+1 || man.Lease == nil {
				o.violate(ctx, h, "write %v of the store moves the head from commit %v to commit %v", i+1, was, now)
			}
		}
		if write.Version == beforeVersion || versions[write.Version] {
			o.violate(ctx, h, "write %v of the store gives the manifest a version that it had before", i+1)
		}
		if man.Lease != nil {
			versions[write.Version] = true
		}
		if before != nil && before.Lease != nil && man.Lease != nil && before.Lease.Holder != man.Lease.Holder {
			if stood, need := write.Time-beforeAt, before.Lease.Duration+core.Margin(before.Lease.Duration); stood < need {
				o.violate(ctx, h, "write %v of the store takes the lease of a holder whose manifest stood still for %v, and the holder's duration and the margin are %v", i+1, stood, need)
			}
		}
		before, beforeAt, beforeVersion = &man, write.Time, write.Version
	}
}

// simulation is one run of a workload under the scheduler.
type simulation struct {
	outcome *outcome
	// err is the scheduler's own failure, such as a deadlock or a broken rule.
	err error
	// lines of the trace.
	lines []string
	// draw is the next random number after the run, which differs if the run drew a different number
	// of them.
	draw  uint64
	steps int
}

// hash of the trace and the next random number.
func (s simulation) hash() string {
	sum := sha256.Sum256([]byte(strings.Join(s.lines, "\n") + fmt.Sprint("\n", s.draw)))
	return hex.EncodeToString(sum[:])
}

// violations of the simulation, including the scheduler's own failure.
func (s simulation) violations() []string {
	var violations []string
	if s.err != nil {
		violations = append(violations, "scheduler: "+s.err.Error())
	}
	return append(violations, s.outcome.violations...)
}

// count of the lines of the trace that contain substring.
func (s simulation) count(substring string) int {
	var n int
	for _, line := range s.lines {
		if strings.Contains(line, substring) {
			n++
		}
	}
	return n
}

// simulate the workload w under the scheduler with seed, in a bubble and a temporary directory of its
// own.
func simulate(t *testing.T, seed uint64, w workload) simulation {
	t.Helper()
	s := simulation{outcome: newOutcome()}
	synctest.Test(t, func(t *testing.T) {
		scheduler := sim.New(seed, sim.Options{Jitter: 50 * time.Millisecond, Buggify: sim.Buggify{On: w.buggify}, MaxSteps: 5_000_000})
		c := cluster{
			world:    scheduler,
			instance: func(name string) tracer { return scheduler.NewInstance(name) },
			dir:      t.TempDir(),
		}
		s.err = scheduler.Run(func(ctx context.Context) {
			w.run(ctx, c, s.outcome)
		})
		s.lines, s.draw, s.steps = scheduler.Lines(), scheduler.Uint64(), scheduler.Steps()
		// The operating system would have closed the files of a crashed process.
		for _, d := range s.outcome.crashed {
			d.CloseCache()
		}
		s.outcome.checkClosed(t, c.dir)
	})
	return s
}

// free runs the workload w with the scheduler off, in a bubble of its own for the fake clock. Every
// instance shares the one host, and the workload moves no clocks and crashes nothing.
func free(t *testing.T, w workload) *outcome {
	t.Helper()
	o := newOutcome()
	synctest.Test(t, func(t *testing.T) {
		r := sim.NewFree(sim.Buggify{On: w.buggify})
		c := cluster{
			world:    r,
			instance: func(string) tracer { return r },
			dir:      t.TempDir(),
		}
		is.NotError(t, r.Run(func(ctx context.Context) {
			w.run(ctx, c, o)
		}))
		o.checkClosed(t, c.dir)
	})
	return o
}

// seeds to run for the workload w, from 0. The number is n unless the environment variable
// S4QL_SIM_SEEDS sets another.
func seeds(t *testing.T, n int) uint64 {
	t.Helper()
	if v := os.Getenv("S4QL_SIM_SEEDS"); v != "" {
		var err error
		n, err = strconv.Atoi(v)
		is.NotError(t, err)
	}
	return uint64(n)
}

// firstDifference between two traces, for a failure message.
func firstDifference(a, b []string) string {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return fmt.Sprintf("line %v:\n  first:  %v\n  second: %v", i, a[i], b[i])
		}
	}
	return fmt.Sprintf("one trace has %v lines and the other %v", len(a), len(b))
}
