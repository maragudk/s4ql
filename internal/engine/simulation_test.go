package engine_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
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

	"google.golang.org/protobuf/proto"
	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
	"maragu.dev/s4ql/internal/sqlite"
	"maragu.dev/s4ql/internal/store/simstore"
	"maragu.dev/s4ql/internal/testshard"
)

// hostRand is the [stmttest.Rand] of a host, so that the statements of a simulation come from its
// seed.
type hostRand struct {
	h host.Host
}

func (r hostRand) IntN(n int) int { return int(r.h.Uint64() % uint64(n)) }
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
	// txs makes one turn of a caller in txs a transaction that spans statements instead of a
	// statement of its own, if it is more than 0: see [run.transaction].
	txs int
	// idlers is the number of callers of each instance that hold a write transaction open and idle
	// across a cut-off of their instance from the store, for up to idle, and leave the last one open
	// after the end of the workload: see [run.idler].
	idlers int
	idle   time.Duration
	// mistyped makes the sequence give more inserts a value of another type than its column, as
	// [stmttest.Sequence.Mistyped] says.
	mistyped bool
}

// hammerWorkload has one instance whose callers hardly wait between their statements, so that two
// statements of one instance often run at once. Its sequence gives more inserts a value of another
// type than its column, among them whole reals for a column of INT or INTEGER, which SQLite converts
// and s4ql refuses.
var hammerWorkload = workload{
	instances: 1,
	callers:   4,
	ops:       30,
	think:     200 * time.Millisecond,
	timeout:   5 * time.Second,
	durations: []time.Duration{30 * time.Second},
	faults:    simstore.AllFaults(),
	txs:       4,
	mistyped:  true,
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
	txs:       4,
}

// idleWorkload is the default workload with an idler on each instance, whose write transactions sit
// idle while the instance is cut off from the store: see [run.idler].
var idleWorkload = func() workload {
	w := defaultWorkload
	w.idlers, w.idle = 1, time.Minute
	return w
}()

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
	// skipped is a text that is not supported or holds no statement, or a call whose arguments the
	// engine refuses.
	skipped
)

// attempt of a statement.
type attempt struct {
	id   uint64
	node string
	src  string
	// args are the arguments that the caller gave, as values of the engine.
	args  []*s4qlpb.Value
	query bool
	// write is true for a statement that changes the database if it succeeds.
	write bool
	// unchanged is set for an update or a delete that succeeded and found no row, by the engine's
	// count, which changes nothing, so that a transaction of it alone leaves no commit object. The
	// engine's word is not taken for it: [run.verify] runs each such statement of its own in SQLite.
	unchanged bool
	// nothing is set for a statement of IF NOT EXISTS or IF EXISTS that did nothing, by its event (see
	// [engine.AttrNothing]): it writes nothing, is not annotated, and is in no commit object. The
	// engine's word is not taken for it either: [run.verify] and [run.verifyTxs] run it in SQLite,
	// where it must change nothing.
	nothing bool
	// changes is the engine's count of changes of an update or a delete that succeeded, which
	// [run.verify] and [run.verifyTxs] compare with SQLite's.
	changes int64
	state   attemptState
	// answer of the statement: SQLite's error of a statement that failed with it, "not supported",
	// "no statement", or [wrongArguments] for one that was skipped, and the rows of a query that
	// succeeded.
	answer answer
	// position is the state that the statement saw, from its event.
	position string
	// access is how the statement read the rows of its table, and index the name of the index that it
	// read by, or that a create index or a drop index names, from its event: see [engine.AttrAccess]
	// and [engine.AttrIndex].
	access, index string
	// acked is the number of writes that were acknowledged when the statement started.
	acked int
	// partial is set for a query whose rows were not read to their end.
	partial bool
	// typeRefused is set for an insert, or a select with WHERE, that the engine refused as "not
	// supported" for a value of another type than its column.
	typeRefused bool
	// reason why a statement failed: no lease, stopped, timeout, or canceled.
	reason string
	// tx is the transaction that spans statements that the statement ran in, or nil for a statement
	// of its own.
	tx *txRecord
	// gen is the generation of the engine of the node that ran the statement, and start and stop
	// the numbers of the run's clock of events when its call began and returned.
	gen         int
	start, stop uint64
}

// txRecord is a transaction that spans statements, as its caller saw it.
type txRecord struct {
	id       uint64
	node     string
	gen      int
	readOnly bool
	// stmts of the transaction, in their order.
	stmts []*attempt
	// acked is the number of writes that were acknowledged when the begin was called.
	acked int
	// start, began, ending, and stop are the numbers of the run's clock of events when the begin was
	// called, the begin returned, the commit or roll back was called, and it returned; 0 for what did
	// not happen.
	start, began, ending, stop uint64
	// commit is true if the caller called the commit, and not the roll back.
	commit bool
	// lost is set when a call of the transaction failed because its instance ended it for the lease.
	lost bool
	// end of the transaction: "" while it is open or its end did not return, because its instance
	// crashed, and otherwise "not begun", "committed", "unknown", "not committed", or "rolled back".
	end string
}

// writes of the transaction that succeeded, which its commit object must hold, in order: not those
// that did nothing.
func (t *txRecord) writes() []*attempt {
	var ws []*attempt
	for _, a := range t.stmts {
		if a.write && a.state == succeeded && !a.nothing {
			ws = append(ws, a)
		}
	}
	return ws
}

// position of the state that the statements of the transaction saw, from the first of them that
// saw one, or "" if none did.
func (t *txRecord) position() string {
	for _, a := range t.stmts {
		if a.position != "" {
			return a.position
		}
	}
	return ""
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

	callers atomic.Int64
	// running is the number of statements that the node's callers run.
	running   atomic.Int64
	reopening atomic.Bool

	mu sync.Mutex
	d  *core.DB
	e  *engine.Engine
	// gen counts the opens of the node's database, so that it tells its engines apart.
	gen int
	// clients are the callers of the node, each with its connection.
	clients []*client
	// kept is the write transaction that an idler left open at its end, or nil: see [run.idler].
	kept *keptTx
}

// keptTx is a write transaction that an idler left open and idle at the end of the workload, on the
// connection c of the engine of the generation gen.
type keptTx struct {
	tx  *txRecord
	c   *engine.Conn
	gen int
}

func (n *node) current() (*core.DB, *engine.Engine) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.d, n.e
}

// generation of the engine e of the node, or -1 if it is not the current one.
func (n *node) generation(e *engine.Engine) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.e != e {
		return -1
	}
	return n.gen
}

// client is a caller of a node, which owns one connection of the engine, as a connection of a pool
// is one: every statement of the caller runs on it. When the node has opened its database again,
// the client closes the connection, whose database is closed, and takes one of the new engine.
type client struct {
	e *engine.Engine
	c *engine.Conn
}

// newClient of the node.
func (n *node) newClient() *client {
	c := &client{}
	n.mu.Lock()
	n.clients = append(n.clients, c)
	n.mu.Unlock()
	return c
}

// conn of the client on the engine e.
func (c *client) conn(ctx context.Context, r *run, e *engine.Engine) *engine.Conn {
	if c.e != e {
		c.close(ctx, r)
		c.e, c.c = e, e.Conn()
	}
	return c.c
}

// close the connection of the client, if it has one. No loop over its rows runs, so the close fails
// for nothing.
func (c *client) close(ctx context.Context, r *run) {
	if c.c == nil {
		return
	}
	if err := c.c.Close(ctx); err != nil {
		r.violate(ctx, "close of a connection: %v", err)
	}
	c.e, c.c = nil, nil
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
	// seed of the scheduler, from which each engine gets the seed of the directions of its scans.
	seed  uint64
	store *simstore.Store
	dir   string

	mu         sync.Mutex
	nodes      []*node
	attempts   map[uint64]*attempt
	order      []*attempt
	acked      []uint64
	violations []string
	nextID     uint64
	// txs are the transactions that span statements, in the order of their begins.
	txs    []*txRecord
	nextTx uint64
	// clock of events of the run, which numbers the begins and ends of calls, so that their order
	// can be compared.
	clock uint64
	// q is the sequence of statements that every caller takes its next statement from, so that
	// they meet at the same tables.
	q *sequence
	// crashed databases, which stay open: see [run.crash].
	crashed []*core.DB
	// checks is the number of integrity checks of caches: see [run.checkCache].
	checks int
	// ways in which the engines read their rows, by the node and the generation of each engine: "rules",
	// "every row", or "index" and the name of the index of its path, as [run.open] chooses them.
	ways map[string]string
	// events of the run that are not statements: crashes, closes, and restarts, by kind.
	events map[string]int
	// wide is the number of rows of tables of several columns in the log, nulls the number of NULL
	// values in its rows, defaults the number of values in its rows that are the default of their
	// column, NULL apart, and defaultTables the number of its create tables with a default, as
	// [run.verifyRows] counts them.
	wide, nulls, defaults, defaultTables int
	// endedForLease are the numbers of the run's clock of events at which an instance ended a write
	// transaction because it lost its lease, by the node and the generation of its engine, as the
	// event of the storage core says: see [run.verifyOverlaps].
	endedForLease map[string][]uint64
}

// coreHandler of the events of the storage core of one engine, of the node and the generation that
// key names: it records the ends of write transactions for the lease, and writes them into the
// trace. The other events of the core are left out, as before it had a logger.
type coreHandler struct {
	r   *run
	key string
}

func (h coreHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}
func (h coreHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h coreHandler) WithGroup(string) slog.Handler      { return h }

func (h coreHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Message != "transaction_ended" {
		return nil
	}
	h.r.mu.Lock()
	h.r.endedForLease[h.key] = append(h.r.endedForLease[h.key], h.r.tick())
	h.r.mu.Unlock()
	var b strings.Builder
	rec.Attrs(func(a slog.Attr) bool {
		fmt.Fprint(&b, " ", a.Key, "=", a.Value)
		return true
	})
	h.r.world.Trace(ctx, "core %v:%v", rec.Message, b.String())
	return nil
}

// tick the run's clock of events, and give its new number. The caller holds r.mu.
func (r *run) tick() uint64 {
	r.clock++
	return r.clock
}

// event of the run of the kind, which [run.events] counts.
func (r *run) event(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events[kind]++
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
	var position, access, index string
	var nothing bool
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key != engine.AttrSQL {
			fmt.Fprint(&b, " ", a.Key, "=", a.Value)
		}
		switch a.Key {
		case engine.AttrPosition:
			position = a.Value.String()
		case engine.AttrAccess:
			access = a.Value.String()
		case engine.AttrIndex:
			index = a.Value.String()
		case engine.AttrNothing:
			nothing = a.Value.Bool()
		}
		return true
	})
	h.r.world.Trace(ctx, "%v", b.String())
	if id, ok := ctx.Value(idKey{}).(uint64); ok && rec.Message == engine.EventStatement {
		h.r.mu.Lock()
		a := h.r.attempts[id]
		a.position, a.access, a.index, a.nothing = position, access, index, nothing
		h.r.mu.Unlock()
	}
	return nil
}

// open the database of n on its cache, with its engine.
func (r *run) open(ctx context.Context, n *node) error {
	n.mu.Lock()
	key := fmt.Sprint(n.name, "/", n.gen+1)
	// Every second engine of a node reads its rows another way than by the rules, so that the checks
	// of the statements against SQLite see more ways of reading rows, also across crashes and closes:
	// every row of a table, or by one of the names of the indexes of the sequence, in a table that has
	// an index of the name, and by the rules in another.
	other := n.gen%2 == 1
	generation := n.gen
	n.mu.Unlock()
	d, err := core.Open(ctx, n.inst, n.client, n.path, "db", core.Options{
		BusyTimeout:   time.Minute,
		ObjectTimeout: 10 * time.Second,
		Lease:         n.lease,
		Logger:        slog.New(coreHandler{r: r, key: key}),
	})
	if err != nil {
		return err
	}
	var options engine.Options
	for _, bug := range r.w.bugs {
		options.Plant(bug)
	}
	// The engine draws the direction of each query from a seed of its own, which the seed of the run,
	// the node, and the number of its engine fix, and not from the host, so that the draws of the
	// scheduler are those of a run without them, and a replay of the seed reads in the same
	// directions.
	engineSeed := r.seed*1_000_003 + uint64(generation)*7919
	for _, b := range []byte(n.name) {
		engineSeed = engineSeed*31 + uint64(b)
	}
	options.Seed(engineSeed)
	// Which other way an engine reads by is drawn from its seed too, one engine in two of each, and not
	// by the number of the engine, since a node rarely opens its database more than twice. The seed is
	// mixed first, as splitmix64 does, since its last bit follows the parities of the seed of the run
	// and of the node's name.
	draw := (engineSeed ^ engineSeed>>30) * 0xbf58476d1ce4e5b9
	draw = (draw ^ draw>>27) * 0x94d049bb133111eb
	draw ^= draw >> 31
	way := "rules"
	if names := r.q.IndexNames(); other && draw%2 == 0 {
		way = "every row"
		options.ScanAll()
	} else if other {
		index := names[draw/2%uint64(len(names))]
		way = "index " + index
		options.Path(engine.Path{Index: index})
	}
	r.mu.Lock()
	r.ways[key] = way
	r.mu.Unlock()
	options.Logger = slog.New(handler{r})
	n.mu.Lock()
	n.d, n.e = d, engine.New(d, n.inst, options)
	n.gen++
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

// withID puts the comment of the ID into the text of a statement that the pass supports, after its
// first token, where the annotation keeps it.
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

// statement runs the call, of the sequence of the run or of the workload, on the connection c of a
// caller of n, whose engine has the generation gen, records it, and gives it. It runs in the
// transaction tx that spans statements, or on its own if tx is nil.
func (r *run) statement(ctx context.Context, n *node, c *engine.Conn, gen int, call stmttest.Call, tx *txRecord) *attempt {
	query := call.Query
	r.mu.Lock()
	r.nextID++
	a := &attempt{id: r.nextID, node: n.name, query: query, acked: len(r.acked), args: values(call.Args), tx: tx, gen: gen, start: r.tick()}
	if tx != nil {
		// A statement of a transaction must see what was acknowledged before the transaction began.
		a.acked = tx.acked
		tx.stmts = append(tx.stmts, a)
	}
	a.src = withID(call.Src, a.id)
	call.Src = a.src
	if s, _ := stmt.Check(a.src); s != nil {
		_, isSelect := s.(*stmt.Select)
		a.write = !isSelect
	}
	r.attempts[a.id] = a
	r.order = append(r.order, a)
	r.mu.Unlock()
	ctx = context.WithValue(ctx, idKey{}, a.id)
	n.running.Add(1)
	defer n.running.Add(-1)

	var got answer
	var err error
	var res engine.Result
	if query {
		var rows *engine.Rows
		rows, err = c.Query(ctx, a.src, a.args...)
		if rows != nil {
			res = rows.Result()
		}
		if err == nil {
			// One query in four is not read to its end: its loop stops at the first row, it is closed
			// unread, or its consumer panics. Its rows are then not compared. The others are read to
			// their end, and the index that one reads by is dropped while its rows are open.
			switch (hostRand{n.inst}).IntN(12) {
			case 0:
				for _, err := range rows.All() {
					_ = err
					break
				}
				a.partial = true
			case 1:
				err = rows.Close()
				a.partial = true
			case 2:
				func() {
					defer func() { _ = recover() }()
					for _, err := range rows.All() {
						_ = err
						panic("the consumer")
					}
				}()
				a.partial = true
			default:
				got, err = readRows(rows, func() { r.dropUnder(ctx, n, gen, rows, tx) })
			}
		}
	} else {
		res, err = c.Exec(ctx, a.src, a.args...)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	a.stop = r.tick()
	var se *sqlerr.Error
	switch {
	case err == nil:
		a.state, a.answer = succeeded, got
		if s, _ := stmt.Check(a.src); res.SetsChanges && isChange(s) {
			a.changes, a.unchanged = res.Changes, res.Changes == 0
		}
		// A write of a transaction is acknowledged by its commit, and a statement that did nothing wrote
		// nothing.
		if a.write && !a.nothing && tx == nil {
			r.acked = append(r.acked, a.id)
		}
		// A call that the rule for arguments refuses must not succeed. One with a value of another
		// type than its column must not either, which [run.verify] checks on the log.
		if kind, failure := checkArguments(call, answer{}); kind != "" {
			r.mu.Unlock()
			r.violate(ctx, "the statement %v succeeded: %v", a.id, failure)
			r.mu.Lock()
		}
	case errors.As(err, &se):
		a.state, a.answer = sqliteFailed, answer{err: fmt.Sprint(se.Code, "/", se.ExtendedCode, ": ", se.Message)}
	case errors.Is(err, stmt.ErrNotSupported), errors.Is(err, engine.ErrNoStatement), errors.Is(err, engine.ErrArguments):
		a.state, a.answer = skipped, answer{err: "not supported", message: err.Error()}
		switch {
		case errors.Is(err, engine.ErrNoStatement):
			a.answer.err = "no statement"
		case errors.Is(err, engine.ErrArguments):
			a.answer.err = wrongArguments
		}
		// A call that s4ql refuses for its arguments must be refused by its rule, and a statement of
		// the generator, and every other that the pass supports, with the arguments that it takes must
		// run.
		kind, failure := checkArguments(call, a.answer)
		s, _ := stmt.Check(a.src)
		supported := call.Supported || s != nil
		if kind == "" && supported && a.answer.err == "not supported" && typeRefusal(a.src, a.answer.message) {
			// The refusal of a value of another type than its column, after the insert or the select
			// found its table and its columns, which [run.verify] checks against SQLite on the state that
			// it saw.
			kind, a.typeRefused = excusedType, true
		}
		if kind == "" && (supported || a.answer.err == wrongArguments) {
			failure = fmt.Sprintf("the statement %v, %q, with %v arguments: %v", a.id, a.src, len(call.Args), err)
		}
		if failure != "" {
			r.mu.Unlock()
			r.violate(ctx, "%v", failure)
			r.mu.Lock()
		}
	case errors.Is(err, core.ErrOutcomeUnknown):
		a.state = unknown
		if tx != nil {
			r.mu.Unlock()
			r.violate(ctx, "the statement %v of the transaction %v has an unknown outcome, and commits nothing", a.id, tx.id)
			r.mu.Lock()
		}
	case errors.Is(err, core.ErrNoLease):
		a.state, a.reason = failed, "no lease"
		if tx != nil && errors.Is(err, core.ErrTxLeaseLost) {
			tx.lost = true
		}
	case errors.Is(err, core.ErrStopped):
		a.state, a.reason = failed, "stopped"
	case errors.Is(err, context.DeadlineExceeded):
		a.state, a.reason = failed, "timeout"
	case errors.Is(err, context.Canceled):
		a.state, a.reason = failed, "canceled"
	default:
		a.state = failed
		r.mu.Unlock()
		r.violate(ctx, "statement %v, %q: %v", a.id, a.src, err)
		r.mu.Lock()
	}
	r.world.Trace(ctx, "statement %v ends as %v: %v", a.id, a.state, err)
	return a
}

// hasClause reports whether s is a create table or a create index with IF NOT EXISTS, or a drop
// table or a drop index with IF EXISTS, of slice 10.
func hasClause(s stmt.Statement) bool {
	switch s := s.(type) {
	case *stmt.CreateTable:
		return s.IfNotExists
	case *stmt.DropTable:
		return s.IfExists
	case *stmt.CreateIndex:
		return s.IfNotExists
	case *stmt.DropIndex:
		return s.IfExists
	}
	return false
}

// isChange reports whether s is an update or a delete.
func isChange(s stmt.Statement) bool {
	switch s.(type) {
	case *stmt.Update, *stmt.Delete:
		return true
	}
	return false
}

// reasonOf an error that means that a call did nothing: no lease, stopped, timeout, or canceled, and
// "" for another error.
func reasonOf(err error) string {
	switch {
	case errors.Is(err, core.ErrNoLease):
		return "no lease"
	case errors.Is(err, core.ErrStopped):
		return "stopped"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return ""
}

// transaction runs a transaction that spans statements on the connection c of a caller of n, whose
// engine has the generation gen, records it, and gives the reason of [reasonOf] why it did not begin,
// ended early, or did not commit: "no lease" or "stopped" if a call of it was refused so, "timeout"
// or "canceled" if its begin or its commit ran out of time, and otherwise "". One transaction in
// four is read-only.
// It has two to six statements of the sequence of the run, a short wait before each, and each call
// has a timeout of up to the workload's. It ends with a commit, or one time in four with a roll
// back, and with a roll back after a refusal. A transaction of an idler is a write transaction that
// then sits idle, maybe while its instance is cut off from the store, and runs one more statement,
// or stays open if keep is set: see [run.idler].
func (r *run) transaction(ctx context.Context, n *node, c *engine.Conn, gen int, rnd hostRand) string {
	return r.transactionOf(ctx, n, c, gen, rnd, false, false)
}

// transactionOf is [run.transaction], for an idler if idle is set, which leaves the transaction
// open if keep is set too.
func (r *run) transactionOf(ctx context.Context, n *node, c *engine.Conn, gen int, rnd hostRand, idle, keep bool) string {
	timeout := func() (context.Context, context.CancelFunc) {
		return n.inst.WithTimeout(ctx, time.Duration(1+rnd.IntN(int(r.w.timeout/time.Millisecond)))*time.Millisecond)
	}
	r.mu.Lock()
	r.nextTx++
	tx := &txRecord{id: r.nextTx, node: n.name, gen: gen, readOnly: !idle && rnd.IntN(4) == 0, acked: len(r.acked), start: r.tick()}
	r.txs = append(r.txs, tx)
	r.mu.Unlock()
	kind := map[bool]string{false: "write", true: "read-only"}[tx.readOnly]
	r.world.Trace(ctx, "%v transaction %v begins", kind, tx.id)

	bctx, cancel := timeout()
	var err error
	if tx.readOnly {
		err = c.BeginReadOnly(bctx)
	} else {
		err = c.Begin(bctx)
	}
	cancel()
	r.mu.Lock()
	tx.began = r.tick()
	if err != nil {
		tx.began, tx.end = 0, "not begun"
	}
	r.mu.Unlock()
	if err != nil {
		r.world.Trace(ctx, "transaction %v does not begin: %v", tx.id, err)
		reason := reasonOf(err)
		if reason == "" {
			r.violate(ctx, "the begin of the transaction %v: %v", tx.id, err)
		}
		return reason
	}

	var reason string
	for range 2 + rnd.IntN(5) {
		_ = n.inst.Sleep(ctx, time.Duration(rnd.IntN(500))*time.Millisecond)
		sctx, cancel := timeout()
		a := r.statement(sctx, n, c, gen, r.q.Next(), tx)
		cancel()
		if a.reason == "no lease" || a.reason == "stopped" {
			reason = a.reason
			break
		}
	}
	if idle && reason == "" {
		// The transaction sits idle, and the instance is cut off from the store meanwhile, for long
		// enough that another instance can take the lease, always before a transaction that is kept.
		if keep || rnd.IntN(2) == 0 {
			cut := time.Duration(30_000+rnd.IntN(30_000)) * time.Millisecond
			if keep {
				cut = 2 * time.Minute
			}
			r.world.Trace(ctx, "%v is cut off for %v while the transaction %v sits idle", n.name, cut, tx.id)
			n.client.CutOff(ctx, cut)
		}
		if keep {
			_ = n.inst.Sleep(ctx, 3*time.Minute)
			r.world.Trace(ctx, "the transaction %v stays open", tx.id)
			n.mu.Lock()
			n.kept = &keptTx{tx: tx, c: c, gen: gen}
			n.mu.Unlock()
			return ""
		}
		_ = n.inst.Sleep(ctx, time.Duration(rnd.IntN(int(r.w.idle/time.Millisecond)))*time.Millisecond)
		sctx, cancel := timeout()
		if a := r.statement(sctx, n, c, gen, r.q.Next(), tx); a.reason == "no lease" || a.reason == "stopped" {
			reason = a.reason
		}
		cancel()
	}

	r.mu.Lock()
	tx.commit = reason == "" && rnd.IntN(4) != 0
	tx.ending = r.tick()
	r.mu.Unlock()
	cctx, cancel := timeout()
	if tx.commit {
		err = c.Commit(cctx)
	} else {
		err = c.Rollback(cctx)
	}
	cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	tx.stop = r.tick()
	var unexpected bool
	switch {
	case !tx.commit:
		tx.end = "rolled back"
		// A roll back on an instance that lost its lease or was closed returns nil too.
		unexpected = err != nil
	case err == nil:
		tx.end = "committed"
		for _, a := range tx.writes() {
			r.acked = append(r.acked, a.id)
		}
	case errors.Is(err, core.ErrOutcomeUnknown):
		tx.end = "unknown"
	default:
		tx.end = "not committed"
		if reason == "" {
			reason = reasonOf(err)
		}
		unexpected = reasonOf(err) == ""
		tx.lost = tx.lost || errors.Is(err, core.ErrTxLeaseLost)
	}
	if tx.lost {
		r.events["transactions ended for the lease"]++
	}
	r.world.Trace(ctx, "transaction %v ends as %v: %v", tx.id, tx.end, err)
	if unexpected {
		r.mu.Unlock()
		r.violate(ctx, "the end of the transaction %v: %v", tx.id, err)
		r.mu.Lock()
	}
	return reason
}

// dropUnder drops the index that the rows of a query read by, if they read by one, with a statement
// of its own on a new connection of the engine of n of the generation gen, while the rows are open,
// so that the check of the rows against SQLite on the state that the query saw shows that the drop
// leaves the rows of an open query as they are: see [run.droppedUnder]. The rows of a write
// transaction tx are left alone, since the drop would wait for its place of the writer.
func (r *run) dropUnder(ctx context.Context, n *node, gen int, rows *engine.Rows, tx *txRecord) {
	access, ok := rows.Access()
	if !ok || access.Index == "" || (tx != nil && !tx.readOnly) {
		return
	}
	_, e := n.current()
	if n.generation(e) != gen {
		return
	}
	r.world.Trace(ctx, "the index %q is dropped while the rows of a query that reads by it are open", access.Index)
	c := e.Conn()
	sctx, cancel := n.inst.WithTimeout(ctx, r.w.timeout)
	r.statement(sctx, n, c, gen, stmttest.Call{Src: `drop index "` + strings.ReplaceAll(access.Index, `"`, `""`) + `"`, Supported: true}, nil)
	cancel()
	if err := c.Close(ctx); err != nil {
		r.violate(ctx, "close of the connection of a drop index: %v", err)
	}
}

// readRows of a query to their end, and call open, if it is not nil, before the first row, while the
// rows are open.
func readRows(rows *engine.Rows, open func()) (answer, error) {
	if open != nil {
		open()
	}
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
			text, ok := valueString(v)
			switch {
			case !ok:
				return answer{}, fmt.Errorf("the engine gives %v", text)
			case v.HasBlob() && v.GetBlob() == nil:
				return answer{}, errors.New("the engine gives a blob that is nil")
			}
			values[j] = text
		}
		a.rows = append(a.rows, strings.Join(values, ","))
	}
	return a, nil
}

func (s attemptState) String() string {
	return [...]string{"pending", "succeeded", "SQLite's error", "failed", "unknown", "skipped"}[s]
}

// waitServing waits until the database of n serves, for up to two minutes, also across a close
// and open of n, and reports whether it does. The caller of an instance that does not serve waits so,
// and then stops, as the client of an application behind a check of its health would turn to
// another instance, so that most statements reach a transaction.
func (r *run) waitServing(ctx context.Context, n *node) bool {
	ctx, cancel := n.inst.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		d, _ := n.current()
		if d.Wait(ctx) == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		// The database stopped: the node opens it again, or a close has done so meanwhile.
		_ = n.inst.Sleep(ctx, time.Second)
	}
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
		// The connection of a caller stays open when the caller ends, for the check that no statement
		// left a read transaction open on it: see [run.readsFree].
		c := n.newClient()
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
				conn := c.conn(ctx, r, e)
				gen := n.generation(e)
				var reason string
				if r.w.txs > 0 && rnd.IntN(r.w.txs) == 0 {
					reason = r.transaction(ctx, n, conn, gen, rnd)
				} else {
					sctx, cancel := inst.WithTimeout(ctx, time.Duration(1+rnd.IntN(int(r.w.timeout/time.Millisecond)))*time.Millisecond)
					reason = r.statement(sctx, n, conn, gen, r.q.Next(), nil).reason
					cancel()
				}
				if reason == "no lease" && !r.waitServing(ctx, n) {
					return
				}
			}
		}))
	}
	for i := range r.w.idlers {
		c := n.newClient()
		n.callers.Add(1)
		tasks = append(tasks, inst.Go(ctx, fmt.Sprint("idler-", i), func(ctx context.Context) {
			defer n.callers.Add(-1)
			r.idler(ctx, n, c, rnd)
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
				r.event("close and open")
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

// idler is a caller of n that runs two write transactions on the connection of the client c, each of
// which sits idle after its statements, while n is cut off from the store for long enough, often,
// that another instance can take the lease: see [run.transactionOf]. The second is cut off always,
// and stays open after the end of the workload, so that the end of the workload checks that n
// serves again while it sits idle: see [run.endKept] and [run.readsFree]. An instance that has lost
// its lease ends such a transaction, so that it can take the lease back.
func (r *run) idler(ctx context.Context, n *node, c *client, rnd hostRand) {
	for i := range 2 {
		_ = n.inst.Sleep(ctx, time.Duration(rnd.IntN(int(r.w.think/time.Millisecond)))*time.Millisecond)
		_, e := n.current()
		if e == nil || (i == 1 && !r.waitServing(ctx, n)) {
			return
		}
		_, e = n.current()
		r.transactionOf(ctx, n, c.conn(ctx, r, e), n.generation(e), rnd, true, i == 1)
	}
}

// endKept is the write of the end of the workload on n, on the connection of the transaction that an
// idler kept open, if it is on the engine e of n. It reports whether the write ran, which it does if
// the transaction is open and n serves, and it ends the transaction if n has ended it for the lease,
// or if it is of an engine before e. ok is false if n has no such transaction.
func (r *run) endKept(ctx context.Context, n *node, e *engine.Engine) (served, ok bool) {
	n.mu.Lock()
	k := n.kept
	n.mu.Unlock()
	if k == nil {
		return false, false
	}
	var err error
	if n.generation(e) == k.gen {
		_, err = k.c.Exec(ctx, `drop table "no such table"`)
		if se := (*sqlerr.Error)(nil); errors.As(err, &se) {
			return true, true
		}
		if !errors.Is(err, core.ErrTxLeaseLost) {
			// The instance does not serve now, and has not ended the transaction.
			return false, true
		}
	}
	// The transaction ended: for the lease, or with the close of its engine.
	r.mu.Lock()
	k.tx.ending = r.tick()
	k.tx.lost = k.tx.lost || errors.Is(err, core.ErrTxLeaseLost)
	r.mu.Unlock()
	rollbackErr := k.c.Rollback(ctx)
	r.mu.Lock()
	k.tx.stop, k.tx.end = r.tick(), "rolled back"
	if k.tx.lost {
		r.events["transactions ended for the lease"]++
		r.events["kept transactions ended for the lease"]++
	}
	r.mu.Unlock()
	r.world.Trace(ctx, "the transaction %v that was kept open ends as rolled back: %v, after: %v", k.tx.id, rollbackErr, err)
	if rollbackErr != nil {
		r.violate(ctx, "the roll back of the transaction %v that was kept open: %v", k.tx.id, rollbackErr)
	}
	n.mu.Lock()
	n.kept = nil
	n.mu.Unlock()
	return false, true
}

// crash a random live node, and start a new node on a copy of what its cache left on disk, or on an
// empty cache. The crashed database stays open until the test binary ends: only the core's own tests
// can close the cache of a crashed instance, so the address sanitizer, which reports what is left
// open, runs no crashes.
func (r *run) crash(ctx context.Context, i int) []host.Task {
	// The victim is a node that runs a statement, if one does within ten seconds, so that a crash
	// often cuts a statement.
	var live []*node
	for range 200 {
		live = nil
		var running []*node
		r.mu.Lock()
		for _, n := range r.nodes {
			if !n.inst.Crashed() {
				live = append(live, n)
				if n.running.Load() > 0 {
					running = append(running, n)
				}
			}
		}
		r.mu.Unlock()
		if len(running) > 0 {
			live = running
			break
		}
		_ = r.world.Sleep(ctx, 50*time.Millisecond)
	}
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
	r.event(map[bool]string{true: "crash, then an empty cache", false: "crash, then a copy of the cache"}[empty])
	r.checkCache(ctx, victim.path, "integrity checks after a crash")
	if empty && indexesAtEnd(readLog(r.store)) > 0 {
		// The new node replays a log that leaves an index, at least as far as the log is now.
		r.event("crash, then an empty cache, of a log with an index")
	}
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

// checkCache runs SQLite's integrity check on a copy of the cache at path, with its write-ahead
// log, as [copyCache] makes it, so that the cache itself, on which a new node may start, is never
// opened by it. A fault that the check finds, such as an index that does not hold what its table
// holds, is a violation. The check counts as an event of the kind.
func (r *run) checkCache(ctx context.Context, path, kind string) {
	r.mu.Lock()
	r.checks++
	at := filepath.Join(r.dir, fmt.Sprint("check-", r.checks, ".db"))
	r.mu.Unlock()
	r.event(kind)
	if _, err := os.Stat(path); err != nil {
		r.violate(ctx, "the cache at %v to check: %v", path, err)
		return
	}
	if err := copyCache(path, at); err != nil {
		r.violate(ctx, "copy of the cache at %v to check: %v", path, err)
		return
	}
	faults, err := integrityCheck(at)
	switch {
	case err != nil:
		r.violate(ctx, "the integrity check of the cache at %v: %v", path, err)
	case len(faults) > 0:
		r.violate(ctx, "the integrity check of the cache at %v, %v: %v", path, kind, strings.Join(faults, "; "))
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(at + suffix)
	}
}

// integrityCheck of the database at path: the faults that `pragma integrity_check` gives, or none.
func integrityCheck(path string) (faults []string, err error) {
	c, err := sqlite.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	s, err := c.Prepare("pragma integrity_check")
	if err != nil {
		return nil, err
	}
	for {
		ok, err := s.Step()
		if err != nil {
			return nil, errors.Join(err, s.Finalize())
		}
		if !ok {
			return faults, s.Finalize()
		}
		if fault := s.ColumnText(0); fault != "ok" {
			faults = append(faults, fault)
		}
	}
}

// do the workload, and check what happened: see [run.verify].
func (w workload) do(ctx context.Context, world *sim.Scheduler, seed uint64, dir string, t testing.TB) *run {
	r := &run{t: t, w: w, world: world, seed: seed, dir: dir, attempts: map[uint64]*attempt{}, events: map[string]int{}, endedForLease: map[string][]uint64{}, ways: map[string]string{},
		store: simstore.New(world, simstore.Options{Latency: 50 * time.Millisecond, Faults: w.faults}), q: newSequenceFrom(hostRand{world})}
	if seed%2 == 1 {
		// The sequence of every second seed writes more create indexes, and more WHERE that an index
		// serves, as [stmttest.Sequence.Indexed] says.
		r.q.Indexed()
	}
	if w.mistyped {
		r.q.Mistyped()
	}
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
				// The transaction that an idler kept open holds the place of the writer while it is
				// open, so the write runs in it.
				if ran, ok := r.endKept(ctx, n, e); ok {
					served = served || ran
					return
				}
				// A connection that only writes has no reader to close.
				var se *sqlerr.Error
				if _, err := e.Conn().Exec(ctx, `drop table "no such table"`); errors.As(err, &se) {
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
	r.readsFree(ctx, live)
	// Each live node is closed now. The cache of a node that crashed was checked at its crash.
	for _, n := range live {
		r.checkCache(ctx, n.path, "integrity checks at the end")
	}
	if err := r.store.Wait(ctx); err != nil {
		r.violate(ctx, "wait for the store: %v", err)
	}
	r.verify(ctx)
	return r
}

// readsFree requires of each live instance that no statement of the workload left a read
// transaction open, and closes it: every connection of its callers that is on its engine must run
// a query, which fails on a connection whose read transaction is open. The query is of a table that
// does not exist, which SQLite's error says once the query has its read transaction. An instance
// runs a query only while it holds the lease, so each instance whose connections have all run
// theirs is closed, which releases the lease for the next. An instance that has not run them
// within ten minutes is a violation.
func (r *run) readsFree(ctx context.Context, live []*node) {
	errs := map[*node]error{}
	for start := r.world.Monotonic(); len(live) > 0 && r.world.Monotonic()-start < 10*time.Minute; {
		var left []*node
		for _, n := range live {
			free := false
			n.do(ctx, "reads", func(ctx context.Context) {
				_, e := n.current()
				// A transaction kept open that the instance has ended since is rolled back, so that its
				// connection runs the query outside it.
				_, _ = r.endKept(ctx, n, e)
				n.mu.Lock()
				clients := slices.Clone(n.clients)
				n.mu.Unlock()
				for _, c := range clients {
					// The connection of a caller that ran its last statement before the node opened its
					// database again is of a database that is closed.
					if c.e != e {
						continue
					}
					wait, cancel := n.inst.WithTimeout(ctx, r.w.timeout)
					_, err := c.c.Query(wait, `select * from "no such table"`)
					cancel()
					if se, ok := errors.AsType[*sqlerr.Error](err); !ok || se.Message != "no such table: no such table" {
						errs[n] = err
						return
					}
				}
				free = true
				for _, c := range clients {
					c.close(ctx, r)
				}
				r.close(ctx, n)
			})
			// A read transaction that is left open stays open, so the instance is not asked again.
			if !free && !errors.Is(errs[n], core.ErrReaderBusy) {
				left = append(left, n)
				continue
			}
			if !free {
				r.violate(ctx, "%v has a connection of a caller with a read transaction left open after the end of the workload: %v", n.name, errs[n])
				n.do(ctx, "closer", func(ctx context.Context) { r.close(ctx, n) })
			}
		}
		if live = left; len(live) > 0 {
			_ = r.world.Sleep(ctx, time.Second)
		}
	}
	for _, n := range live {
		n.mu.Lock()
		k := n.kept
		n.mu.Unlock()
		if k != nil {
			// The other instances are closed, so n could take the lease.
			r.violate(ctx, "%v, where the write transaction %v is open and idle, does not serve ten minutes after the end of the workload: %v", n.name, k.tx.id, errs[n])
		} else {
			r.violate(ctx, "%v does not run a query on each connection of its callers ten minutes after the end of the workload: %v", n.name, errs[n])
		}
		n.do(ctx, "closer", func(ctx context.Context) { r.close(ctx, n) })
	}
}

// logCommit is a commit object of the log: its name, its changes, and the texts of its annotations
// and their parameters.
type logCommit struct {
	name    format.Name
	changes []*s4qlpb.Change
	notes   []string
	params  [][]*s4qlpb.Value
}

// verifyRows checks the rows of the changes of the log against the tables of the log before them:
// each row that a commit upserts is of a table that exists there, has a value for every column of
// the table, in the order of the table, and each value has the type of its column, or is NULL in a
// column that takes NULL. The replay of the
// log in SQLite runs its statements, and not its rows, so it does not see a row that holds the
// values of a statement in other columns. It counts the rows of tables of several columns, the NULL
// values in the rows, the values that are the default of their column, which is not NULL, and the
// create tables with a default, of slice 11, in r.
func (r *run) verifyRows(ctx context.Context, log []logCommit) {
	tables := map[string]*s4qlpb.CreateTable{}
	for k, c := range log {
		for _, ch := range c.changes {
			switch {
			case ch.HasCreateTable():
				tables[fold(ch.GetCreateTable().GetTable())] = ch.GetCreateTable()
				if slices.ContainsFunc(ch.GetCreateTable().GetColumns(), (*s4qlpb.Column).HasDefault) {
					r.defaultTables++
				}
			case ch.HasDropTable():
				delete(tables, fold(ch.GetDropTable().GetTable()))
			case ch.HasUpsertRows():
				u := ch.GetUpsertRows()
				t := tables[fold(u.GetTable())]
				if t == nil {
					r.violate(ctx, "commit %v upserts rows of the table %q, which the log does not have there", k+1, u.GetTable())
					continue
				}
				columns := t.GetColumns()
				all := make([]uint32, len(columns))
				for i := range all {
					all[i] = uint32(i)
				}
				if !slices.Equal(u.GetColumns(), all) {
					r.violate(ctx, "commit %v upserts rows of the table %v for the columns %v, and not for each of its %v columns in their order", k+1, t.GetTable(), u.GetColumns(), len(columns))
					continue
				}
				for _, row := range u.GetRows() {
					values := row.GetValues()
					if len(values) != len(columns) {
						r.violate(ctx, "commit %v holds a row of the table %v of %v values for %v columns", k+1, t.GetTable(), len(values), len(columns))
						continue
					}
					if len(columns) > 1 {
						r.wide++
					}
					for i, v := range values {
						if d := columns[i].GetDefault(); columns[i].HasDefault() && !d.HasNull() && proto.Equal(d, v) {
							r.defaults++
						}
						if v.HasNull() {
							r.nulls++
							// NULL has no type. The core refuses it for a column that does not take
							// it, so this finds a fault of the engine and of the core together.
							if !columns[i].GetNullable() {
								r.violate(ctx, "commit %v holds a row of the table %v with NULL in the column %v, which does not take NULL", k+1, t.GetTable(), columns[i].GetName())
							}
							continue
						}
						if !holds(columns[i].GetType(), v) {
							r.violate(ctx, "commit %v holds a row of the table %v whose value %v of the column %v is of another type than its column, %v", k+1, t.GetTable(), valuesText([]*s4qlpb.Value{v}), columns[i].GetName(), declType(columns[i].GetType()))
						}
					}
				}
			}
		}
	}
}

// holds reports whether a column of the type t holds the value v: an integer for INT and INTEGER, a
// real for REAL, text for TEXT, and a blob for BLOB.
func holds(t s4qlpb.Type, v *s4qlpb.Value) bool {
	switch t {
	case s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INTEGER:
		return v.HasInteger()
	case s4qlpb.Type_TYPE_REAL:
		return v.HasReal()
	case s4qlpb.Type_TYPE_TEXT:
		return v.HasText()
	case s4qlpb.Type_TYPE_BLOB:
		return v.HasBlob()
	}
	return false
}

// fold a name as SQLite compares names.
func fold(name string) string {
	return stmt.Name{Value: name}.Fold()
}

// verify what happened in the run against SQLite and the log:
//
//   - Every commit object of the log holds the statements of one call. A statement of its own is
//     of an attempt that succeeded, whose outcome is unknown, or whose instance crashed while it
//     ran. A transaction that spans statements is a write transaction whose caller called its
//     commit, and whose commit succeeded, had an unknown outcome, or did not return because its
//     instance crashed; the commit object holds exactly the statements of the transaction that
//     succeeded and that write, in order, and its parent is the state that the transaction began
//     from, so no commit landed while it was open. No attempt and no transaction is in the log
//     twice. The parameters of each statement are exactly the arguments that the caller gave.
//   - Every write that was acknowledged is in the log: a statement of its own when it succeeded,
//     and the writes of a transaction when its commit succeeded.
//   - The statements of the log, run in SQLite in their order with their parameters, each succeed,
//     and the value of each insert has the type of its column there, which SQLite would convert or
//     refuse otherwise, as [typeRule] says.
//   - Each insert of its own that the engine refused for the type of its value is refused so by
//     [typeRule] in SQLite on the state that it saw.
//   - Each statement of its own that failed with SQLite's error gives that error in SQLite on the
//     state that it saw, and each query that succeeded gives its rows there: the state after a
//     commit of the log, which holds every write that was acknowledged before the statement
//     started.
//   - Each statement of a transaction that spans statements gives in SQLite what it gave, on the
//     state that the transaction began from, which holds every write that was acknowledged before
//     the begin, after the statements of the transaction before it, and under `pragma query_only =
//     1` for a read-only transaction: see [run.verifyTxs]. So no statement sees a write that is
//     not committed, of its own transaction excepted.
//   - Two write transactions on one engine are never open at once, and no statement of its own
//     commits on it while one is: see [run.verifyOverlaps].
//   - Each text that the engine does not support is one that SQLite prepares on an empty database
//     without a syntax error, as in [pair.step].
//   - A new instance with an empty cache replays the log to the state that SQLite has after it, its
//     indexes included, and SQLite's integrity check of its cache finds no fault, as it finds none in
//     the cache of a node after its crash or at the end of the workload: see [run.checkCache].
func (r *run) verify(ctx context.Context) {
	log := readLog(r.store)
	r.verifyRows(ctx, log)
	index := map[string]int{format.EmptyLog.String(): 0}
	inLog := map[uint64]int{}
	txInLog := map[uint64]int{}
	for k, c := range log {
		index[c.name.String()] = k + 1
		var ids []uint64
		for _, note := range c.notes {
			m := idPattern.FindStringSubmatch(note)
			if m == nil {
				r.violate(ctx, "commit %v holds the statement %q, of no attempt", k+1, note)
				ids = nil
				break
			}
			id, _ := strconv.ParseUint(m[1], 10, 64)
			if r.attempts[id] == nil {
				r.violate(ctx, "commit %v holds the statement %v, of no attempt", k+1, id)
				ids = nil
				break
			}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			if len(c.notes) == 0 {
				r.violate(ctx, "commit %v holds no statement", k+1)
			}
			continue
		}
		first := r.attempts[ids[0]]
		if tx := first.tx; tx == nil {
			a := first
			switch {
			case len(ids) != 1:
				r.violate(ctx, "commit %v holds %v statements, and the first, %v, is a statement of its own", k+1, len(ids), a.id)
			case a.state != succeeded && a.state != unknown && a.state != pending:
				r.violate(ctx, "the statement %v is in commit %v, and its caller was told %v", a.id, k+1, a.state)
			}
		} else {
			var want []uint64
			for _, a := range tx.writes() {
				want = append(want, a.id)
			}
			parent := format.EmptyLog.String()
			if k > 0 {
				parent = log[k-1].name.String()
			}
			switch {
			case slices.ContainsFunc(ids, func(id uint64) bool { return r.attempts[id].tx != tx }):
				r.violate(ctx, "commit %v holds the statements %v, which are not all of the transaction %v", k+1, ids, tx.id)
			case txInLog[tx.id] > 0:
				r.violate(ctx, "the transaction %v is in commit %v and commit %v", tx.id, txInLog[tx.id], k+1)
			case tx.readOnly:
				r.violate(ctx, "commit %v holds the read-only transaction %v", k+1, tx.id)
			case !tx.commit:
				r.violate(ctx, "the transaction %v is in commit %v, and its caller rolled it back", tx.id, k+1)
			case tx.end != "committed" && tx.end != "unknown" && tx.end != "":
				r.violate(ctx, "the transaction %v is in commit %v, and its caller was told %v", tx.id, k+1, tx.end)
			case !slices.Equal(ids, want):
				r.violate(ctx, "commit %v holds the statements %v of the transaction %v, whose statements that succeeded and write are %v", k+1, ids, tx.id, want)
			case tx.position() != parent:
				r.violate(ctx, "the transaction %v began at %q, and its commit %v follows %q: another commit landed while it was open", tx.id, tx.position(), k+1, parent)
			}
			txInLog[tx.id] = k + 1
		}
		for i, id := range ids {
			a := r.attempts[id]
			if inLog[id] > 0 {
				r.violate(ctx, "the statement %v is in commit %v and commit %v", id, inLog[id], k+1)
			}
			if a.nothing {
				r.violate(ctx, "the statement %v, %q, did nothing, and is in commit %v", id, a.src, k+1)
			}
			if valuesText(c.params[i]) != valuesText(a.args) {
				r.violate(ctx, "commit %v holds the parameters %v of the statement %v, %q, and its caller gave the arguments %v", k+1, valuesText(c.params[i]), id, a.src, valuesText(a.args))
			}
			inLog[id] = k + 1
		}
	}
	// The observations of each state of the log: the statements of their own that saw it.
	seen := map[int][]*attempt{}
	// refused are the inserts and the selects of their own that the engine refused for the type of a
	// value, by the state of the log that each saw.
	refused := map[int][]*attempt{}
	empty := newOracle(r.t)
	defer empty.close()
	for _, a := range r.order {
		if a.state == skipped && a.answer.err == "not supported" {
			if m := syntaxOfNotSupported(r.t, empty, a.src); m != "" {
				r.violate(ctx, "the engine does not support the statement %v, %q, and SQLite gives %v", a.id, a.src, m)
			}
			if a.typeRefused && a.tx == nil {
				if at, ok := index[a.position]; ok {
					refused[at] = append(refused[at], a)
				} else {
					r.violate(ctx, "the statement %v, %q, is refused for the type of its value, and saw the state %q, which the log does not have", a.id, a.src, a.position)
				}
			}
			continue
		}
		// The statements that are compared with SQLite on the state that they saw: those that failed with
		// SQLite's error, the queries that succeeded and were read to their end, the updates and deletes
		// that succeeded and changed no row by the engine's count, and the statements that did nothing,
		// which are in no commit object.
		if a.state != sqliteFailed && (a.state != succeeded || !a.query || a.write || a.partial) && (a.state != succeeded || !a.unchanged && !a.nothing) {
			continue
		}
		if a.position == "" {
			// A statement that saw no state is a syntax error, which is the one that SQLite gives on an
			// empty database, whatever the database holds.
			if want := empty.run(r.t, a.src); a.answer.err == "" || a.answer.String() != want.String() {
				r.violate(ctx, "the statement %v, %q, saw no state, and gives %v, and SQLite on an empty database %v", a.id, a.src, a.answer, want)
			}
			continue
		}
		if a.tx != nil {
			// See [run.verifyTxs].
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
			// A write here changed nothing in the engine, so it runs in a savepoint that is rolled back,
			// and SQLite stays at the state of the log.
			if a.write {
				is.Equal(r.t, "", o.run(r.t, "savepoint s4ql_seen").err)
			}
			var before string
			if a.nothing {
				before = o.state(r.t)
			}
			want := o.run(r.t, a.src, oracleValues(a.args)...)
			changes, _ := o.changes(r.t)
			if a.nothing {
				if after := o.state(r.t); want.err == "" && after != before {
					r.violate(ctx, "the statement %v, %q, at commit %v does nothing, is in no commit, and changes SQLite's database from\n%v\nto\n%v", a.id, a.src, k, before, after)
				}
				r.event("statements that do nothing, compared with SQLite")
			}
			if a.write {
				is.Equal(r.t, "", o.run(r.t, "rollback to s4ql_seen").err)
				is.Equal(r.t, "", o.run(r.t, "release s4ql_seen").err)
			}
			if !a.query {
				want.columns, want.rows = "", nil
			}
			if got := a.answer.String(); got != want.String() {
				r.violate(ctx, "the statement %v, %q, at commit %v gives %v, and in SQLite %v", a.id, a.src, k, got, want)
			}
			if a.unchanged && want.err == "" && changes != 0 {
				r.violate(ctx, "the statement %v, %q, at commit %v changes no row, is in no commit, and changes %v rows in SQLite", a.id, a.src, k, changes)
			}
			if a.unchanged {
				r.event("updates and deletes that change no row, compared with SQLite")
			}
			r.countWhere(a, "of their own")
		}
		for _, a := range refused[k] {
			if want := typeRule(r.t, o, a.src, oracleValues(a.args)); a.answer.message != want {
				r.violate(ctx, "the statement %v, %q, at commit %v gives %q, and the rule for the type of a value %q", a.id, a.src, k, a.answer.message, want)
			}
		}
		if k < len(log) {
			for i, note := range log[k].notes {
				// The value of an insert of the log has the type of its column, which SQLite would
				// convert or refuse otherwise.
				if want := typeRule(r.t, o, note, oracleValues(log[k].params[i])); want != "" {
					r.violate(ctx, "commit %v, %q, stores a value of another type than its column: %v", k+1, note, want)
				}
				if a := o.run(r.t, note, oracleValues(log[k].params[i])...); a.err != "" {
					r.violate(ctx, "commit %v, %q, fails in SQLite: %v", k+1, note, a.err)
				}
				// The count of changes of an update or a delete of its own, which its caller was given.
				changes, _ := o.changes(r.t)
				if m := idPattern.FindStringSubmatch(note); m != nil {
					id, _ := strconv.ParseUint(m[1], 10, 64)
					if a := r.attempts[id]; a != nil && a.tx == nil && a.state == succeeded && a.write {
						if s, _ := stmt.Check(a.src); isChange(s) {
							if a.changes != changes {
								r.violate(ctx, "commit %v, %q, changes %v rows in SQLite, and its caller was told %v", k+1, note, changes, a.changes)
							}
							r.event("updates and deletes of their own whose count of changes is compared with SQLite")
						}
					}
				}
			}
		}
	}
	nothing := r.verifyTxs(ctx, log, index, inLog)
	for _, id := range r.acked {
		// An update or a delete that found no row changes nothing, and is in the log only if the
		// transaction that ran it changed something else.
		if inLog[id] == 0 && !nothing[id] && !r.attempts[id].unchanged {
			r.violate(ctx, "the acknowledged statement %v, %q, is not in the log", id, r.attempts[id].src)
		}
	}
	r.verifyOverlaps(ctx)

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
	r.checkCache(ctx, n.path, "integrity checks after the replay")
	if indexesAtEnd(log) > 0 {
		r.event("replays of a log with an index")
	}
}

// verifyTxs checks what each statement of a transaction that spans statements gave. The statements
// of a transaction must all have seen one state, the one that it began from, which must be a state
// of the log that holds every write that was acknowledged before the begin. SQLite then runs the
// log up to that state, under `pragma query_only = 1` for a read-only transaction, and each
// statement of the transaction that succeeded or failed with SQLite's error, in their order, and
// must give what the engine gave: for a write transaction, the state of its beginning and its own
// writes so far; for a read-only one, the state of its beginning, whatever committed meanwhile. A
// statement that failed otherwise, or was refused, did nothing, and is not run; an insert refused for
// the type of its value must be refused so by [typeRule] there, and never in a read-only
// transaction, which refuses the write first.
//
// A write transaction whose net effect is nothing writes no commit object: one whose writes that
// succeeded are all of tables that were not there when it began and are not there at its end, whose
// writes must not be in the log. One whose rows SQLite has at its end as at its beginning may write
// none either, as one that inserted a row and deleted it. The count of changes of each update and
// delete that succeeded must be SQLite's. verifyTxs gives the IDs of the writes of such
// transactions, which are acknowledged and need not be in the log.
func (r *run) verifyTxs(ctx context.Context, log []logCommit, index map[string]int, inLog map[uint64]int) (nothing map[uint64]bool) {
	nothing = map[uint64]bool{}
	for _, tx := range r.txs {
		position := tx.position()
		if position == "" {
			continue
		}
		kind := map[bool]string{false: "write", true: "read-only"}[tx.readOnly]
		var states []string
		for _, a := range tx.stmts {
			if a.position != "" && !slices.Contains(states, a.position) {
				states = append(states, a.position)
			}
		}
		if len(states) > 1 {
			// The statements are still compared with SQLite on the state of the first.
			r.violate(ctx, "the statements of the %v transaction %v saw the states %q", kind, tx.id, states)
		}
		at, ok := index[position]
		if !ok {
			r.violate(ctx, "the transaction %v saw the state %q, which the log does not have", tx.id, position)
			continue
		}
		for _, id := range r.acked[:tx.acked] {
			if inLog[id] > at {
				r.violate(ctx, "the transaction %v saw commit %v, and the statement %v, in commit %v, was acknowledged before it began", tx.id, at, id, inLog[id])
			}
		}
		o := newOracle(r.t)
		for _, c := range log[:at] {
			for i, note := range c.notes {
				o.run(r.t, note, oracleValues(c.params[i])...)
			}
		}
		if tx.readOnly {
			o.run(r.t, "pragma query_only = 1")
		}
		before, beforeState := o.tables(r.t), o.state(r.t)
		for _, a := range tx.stmts {
			if a.typeRefused {
				// A read-only transaction refuses an insert before it looks at its value, and runs a
				// select.
				want := typeRule(r.t, o, a.src, oracleValues(a.args))
				if tx.readOnly {
					want = readOnlyRule(r.t, o, a.src, oracleValues(a.args))
				}
				if a.answer.message != want {
					r.violate(ctx, "the statement %v, %q, of the transaction %v (%v), which began at commit %v, gives %q, and the rule for the type of a value %q", a.id, a.src, tx.id, kind, at, a.answer.message, want)
				}
				continue
			}
			if a.position == "" || (a.state != succeeded && a.state != sqliteFailed) {
				continue
			}
			if want := typeRule(r.t, o, a.src, oracleValues(a.args)); a.state == succeeded && want != "" {
				// The check of the log does not see an insert of a transaction that did not commit.
				r.violate(ctx, "the statement %v, %q, of the transaction %v (%v) succeeded, and stores a value of another type than its column: %v", a.id, a.src, tx.id, kind, want)
			}
			var stateBefore string
			if a.nothing {
				stateBefore = o.state(r.t)
			}
			want := o.run(r.t, a.src, oracleValues(a.args)...)
			changes, _ := o.changes(r.t)
			if a.nothing {
				if after := o.state(r.t); want.err == "" && after != stateBefore {
					r.violate(ctx, "the statement %v, %q, of the transaction %v (%v), which began at commit %v, does nothing, and changes SQLite's database from\n%v\nto\n%v", a.id, a.src, tx.id, kind, at, stateBefore, after)
				}
				r.event("statements that do nothing in a transaction, compared with SQLite")
			}
			if a.partial {
				continue
			}
			if !a.query {
				want.columns, want.rows = "", nil
			}
			if got := a.answer.String(); got != want.String() {
				r.violate(ctx, "the statement %v, %q, of the transaction %v (%v), which began at commit %v, gives %v, and in SQLite %v", a.id, a.src, tx.id, kind, at, got, want)
			}
			if s, _ := stmt.Check(a.src); a.state == succeeded && isChange(s) {
				if want.err == "" && a.changes != changes {
					r.violate(ctx, "the statement %v, %q, of the transaction %v (%v), which began at commit %v, changes %v rows in SQLite, and its caller was told %v", a.id, a.src, tx.id, kind, at, changes, a.changes)
				}
				r.event("updates and deletes in a transaction whose count of changes is compared with SQLite")
			}
			r.countWhere(a, "in a transaction")
		}
		after := o.tables(r.t)
		afterState := o.state(r.t)
		o.close()
		writes := tx.writes()
		if tx.readOnly || len(writes) == 0 {
			continue
		}
		// A transaction whose writes that succeeded are all of tables that were not there when it began
		// and are not there at its end writes no commit object. One whose rows SQLite has as they were
		// when it began may write none either: the core commits nothing for a key whose row was not
		// there before and is not there after, as for a row that the transaction inserted and deleted,
		// and commits the row of a key that it wrote, also with the values that it had.
		ofTables := !slices.ContainsFunc(writes, func(a *attempt) bool {
			// A drop index names no table, and is of a table that is there.
			if s, _ := stmt.Check(a.src); s != nil {
				if _, ok := s.(*stmt.DropIndex); ok {
					return true
				}
			}
			name := tableOf(a.src)
			return before[name] || after[name]
		})
		if !ofTables && afterState != beforeState {
			continue
		}
		for _, a := range writes {
			nothing[a.id] = true
			if ofTables && inLog[a.id] > 0 {
				r.violate(ctx, "the statement %v of the transaction %v, whose net effect is nothing, is in commit %v", a.id, tx.id, inLog[a.id])
			}
		}
	}
	return nothing
}

// way in which the engine of the attempt a read its rows: see [run.ways].
func (r *run) way(a *attempt) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ways[fmt.Sprint(a.node, "/", a.gen)]
}

// countWhere counts the attempt a, which [run.verify] or [run.verifyTxs] compared with SQLite, as an
// event of the run if it is a select with WHERE: by where it ran, of their own or in a transaction,
// and, for a query that succeeded, by whether it gave rows.
func (r *run) countWhere(a *attempt, where string) {
	s, _ := stmt.Check(a.src)
	if sel, ok := s.(*stmt.Select); !ok || sel.Where == nil {
		return
	}
	r.event("selects with WHERE " + where + " compared with SQLite")
	switch way := r.way(a); {
	case way == "every row":
		r.event("selects with WHERE " + where + " compared with SQLite, on an engine that reads every row")
	case strings.HasPrefix(way, "index "):
		r.event("selects with WHERE " + where + " compared with SQLite, on an engine that forces an index path")
	}
	if a.access == "index" {
		r.event("selects with WHERE " + where + " compared with SQLite, read by an index")
	}
	if a.state == succeeded && a.query && len(a.answer.rows) > 0 {
		r.event("selects with WHERE " + where + " that give rows, compared with SQLite")
	}
}

// tableOf the statement of src, which the pass supports, folded as SQLite compares names.
func tableOf(src string) string {
	s, err := stmt.Check(src)
	if err != nil {
		panic(err)
	}
	switch s := s.(type) {
	case *stmt.CreateTable:
		return s.Table.Fold()
	case *stmt.Insert:
		return s.Table.Fold()
	case *stmt.DropTable:
		return s.Table.Fold()
	case *stmt.Select:
		return s.Table.Fold()
	case *stmt.Update:
		return s.Table.Fold()
	case *stmt.Delete:
		return s.Table.Fold()
	case *stmt.CreateIndex:
		return s.Table.Fold()
	}
	panic(fmt.Sprintf("a statement of type %T", s))
}

// tables of the database of the oracle, by their names folded as SQLite compares names.
func (o *oracle) tables(t testing.TB) map[string]bool {
	t.Helper()
	s, err := o.c.Prepare("select name from sqlite_schema where type = 'table'")
	is.NotError(t, err)
	names := map[string]bool{}
	for {
		ok, err := s.Step()
		is.NotError(t, err)
		if !ok {
			break
		}
		names[stmt.Name{Value: s.ColumnText(0)}.Fold()] = true
	}
	is.NotError(t, s.Finalize())
	return names
}

// verifyOverlaps checks that the write transactions of one engine never overlap. A write
// transaction holds the place of the writer from the return of its begin to its commit or roll back,
// so no other write transaction of the engine may begin in that time, and no statement of its own
// that writes may both start and commit in it. An engine is one node's between two opens of its
// database.
func (r *run) verifyOverlaps(ctx context.Context) {
	for _, a := range r.txs {
		if a.readOnly || a.began == 0 {
			continue
		}
		end := a.ending
		if end == 0 {
			end = math.MaxUint64
		}
		// An instance that lost its lease ends the write transaction that holds the place, and frees
		// the place, before the caller ends it.
		for _, at := range r.endedForLease[fmt.Sprint(a.node, "/", a.gen)] {
			if a.began < at && at < end {
				end = at
				break
			}
		}
		for _, b := range r.txs {
			if b != a && !b.readOnly && b.began != 0 && b.node == a.node && b.gen == a.gen && a.began < b.began && b.began < end {
				r.violate(ctx, "the write transaction %v began on %v while the write transaction %v was open there", b.id, a.node, a.id)
			}
		}
		for _, s := range r.order {
			if s.tx == nil && s.write && !s.nothing && s.state == succeeded && s.node == a.node && s.gen == a.gen && a.began < s.start && s.stop < end {
				r.violate(ctx, "the statement %v committed on %v while the write transaction %v was open there", s.id, a.node, a.id)
			}
		}
	}
}

// indexesAtEnd of the log: the number of indexes that its tables have after its last commit.
func indexesAtEnd(log []logCommit) int {
	// The tables of the indexes, by the folds of the names of the indexes.
	indexes := map[string]string{}
	for _, c := range log {
		for _, ch := range c.changes {
			switch {
			case ch.HasCreateIndex():
				indexes[fold(ch.GetCreateIndex().GetIndex())] = fold(ch.GetCreateIndex().GetTable())
			case ch.HasDropIndex():
				delete(indexes, fold(ch.GetDropIndex().GetIndex()))
			case ch.HasDropTable():
				maps.DeleteFunc(indexes, func(_, table string) bool { return table == fold(ch.GetDropTable().GetTable()) })
			}
		}
	}
	return len(indexes)
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
			ch, a, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				panic(err)
			}
			if ch != nil {
				c.changes = append(c.changes, ch)
			}
			if a != nil {
				c.notes = append(c.notes, a.GetStatement().GetSql())
				c.params = append(c.params, a.GetStatement().GetParameters())
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

// valuesText of the values vs of the engine: the kind and the value of each, by position.
func valuesText(vs []*s4qlpb.Value) string {
	var texts []string
	for _, v := range vs {
		texts = append(texts, fmt.Sprintf("%v:%v", v.WhichKind(), stmtValue(v)))
	}
	return "[" + strings.Join(texts, " ") + "]"
}

// stmtValue of the value v of the engine, whatever its kind.
func stmtValue(v *s4qlpb.Value) any {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return v.GetInteger()
	case s4qlpb.Value_Real_case:
		return v.GetReal()
	case s4qlpb.Value_Text_case:
		return fmt.Sprintf("%q", v.GetText())
	case s4qlpb.Value_Blob_case:
		return fmt.Sprintf("%x", v.GetBlob())
	}
	return nil
}

// oracleValues of vs, values of the engine that a column holds, as the oracle binds them.
func oracleValues(vs []*s4qlpb.Value) []any {
	var as []any
	for _, v := range vs {
		switch v.WhichKind() {
		case s4qlpb.Value_Integer_case:
			as = append(as, v.GetInteger())
		case s4qlpb.Value_Real_case:
			as = append(as, v.GetReal())
		case s4qlpb.Value_Text_case:
			as = append(as, v.GetText())
		case s4qlpb.Value_Blob_case:
			as = append(as, v.GetBlob())
		case s4qlpb.Value_Null_case:
			as = append(as, nil)
		default:
			panic(fmt.Sprintf("a value of kind %v for the oracle", v.WhichKind()))
		}
	}
	return as
}

// coreState of the database d, as [oracle.state] gives it.
func coreState(ctx context.Context, d *core.DB) (string, error) {
	rd, err := d.Reader(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = rd.Close(ctx) }()
	r, err := rd.Read(ctx)
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
			var dflt string
			if c.HasDefault() {
				text, ok := valueString(c.GetDefault())
				if !ok {
					return "", fmt.Errorf("the column %v.%v has the default %v", ct.GetTable(), c.GetName(), text)
				}
				dflt = text
			}
			columns = append(columns, columnText(c.GetName(), declType(c.GetType()), c.GetNullable(), dflt))
		}
		var rows []string
		for row, err := range r.Scan(ctx, ct.GetTable()) {
			if err != nil {
				return "", err
			}
			texts := make([]string, len(row))
			for j, v := range row {
				text, ok := valueString(v)
				if !ok || v.HasBlob() && v.GetBlob() == nil {
					return "", fmt.Errorf("the table %v holds %v, or a blob that is nil", ct.GetTable(), text)
				}
				texts[j] = text
			}
			rows = append(rows, strings.Join(texts, ","))
		}
		fmt.Fprintf(&b, "%v (%v): %v\n", ct.GetTable(), strings.Join(columns, ", "), strings.Join(rows, " "))
	}
	indexes, err := indexesText(ctx, r, tables)
	if err != nil {
		return "", err
	}
	return b.String() + indexes, nil
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
			s.r = w.do(ctx, world, seed, dir, t)
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

// storeFaults are the faults of [simstore.AllFaults] by a name, each with what the store writes into
// the trace when it injects the fault.
var storeFaults = map[string]string{
	"fault: lost request":  "was lost on its way to the store",
	"fault: lost response": "was lost on its way back",
	"fault: no answer":     "no answer reaches the caller",
	"fault: conflict":      "a conflict with another write in flight",
	"fault: slow way":      "is slow on its way",
	"fault: broken read":   "whose content breaks off",
	"fault: cut-off":       "cut off for",
	"fault: outage":        "store: unreachable for",
	"fault: write applied after its caller gave up": "the caller gave up, and is applied",
	"fault: write stuck in the network":             "is stuck in the network",
	"fault: write applied after a crash":            "the sender crashed, and is applied",
}

// storeLine matches a line of the trace that a task of a request of the store wrote, also the task
// of a write that was stuck in the network.
var storeLine = regexp.MustCompile(`^\S+ store request \d+(, stuck)? store: `)

// tallyOf the simulation s: its statements, by how they ended, its writes that were acknowledged,
// its queries that were not read to their end, its inserts and selects refused for the type of a
// value, its create indexes and drop indexes by outcome, its statements of IF NOT EXISTS or IF EXISTS
// by whether they did nothing, did their work, or failed with SQLite's error, its selects, updates,
// and deletes that read by an index, its statements on an engine that forces an index path, its
// queries whose index was
// dropped while their rows were open, its commits and those of a statement with a parameter, the
// rows of tables of several columns and the NULL values in the log, the faults that the store
// injected, by kind, and its other events.
func tallyOf(s simulation) map[string]int {
	r := s.r
	counts := maps.Clone(r.events)
	for _, line := range s.lines {
		if !storeLine.MatchString(line) {
			continue
		}
		for name, marker := range storeFaults {
			if strings.Contains(line, marker) {
				counts[name]++
			}
		}
	}
	counts["statements"] = len(r.order)
	for _, a := range r.order {
		counts[a.state.String()]++
		if a.reason != "" {
			counts[fmt.Sprint(a.state, ": ", a.reason)]++
		}
		if a.partial {
			counts["query not read to its end"]++
		}
		if a.typeRefused {
			counts["refused for the type"]++
		}
		s, _ := stmt.Check(a.src)
		var kind string
		switch s.(type) {
		case *stmt.CreateIndex:
			kind = "create index"
		case *stmt.DropIndex:
			kind = "drop index"
		case *stmt.Select:
			kind = "selects"
		case *stmt.Update:
			kind = "updates"
		case *stmt.Delete:
			kind = "deletes"
		}
		switch {
		case (kind == "create index" || kind == "drop index") && a.state == succeeded:
			counts[kind+": ok"]++
		case (kind == "create index" || kind == "drop index") && a.state == sqliteFailed:
			counts[kind+": "+errorKind(a.answer.err)]++
		case a.access == "index":
			counts[kind+" read by an index"]++
		}
		if forced, ok := strings.CutPrefix(r.way(a), "index "); ok && a.position != "" {
			counts["statements on an engine that forces an index path"]++
			if a.access == "index" && fold(a.index) == fold(forced) {
				counts["statements read by the index that their engine forces"]++
			}
		}
		if hasClause(s) {
			switch {
			case a.state == succeeded && a.nothing:
				counts["statements of IF NOT EXISTS or IF EXISTS that did nothing"]++
			case a.state == succeeded:
				counts["statements of IF NOT EXISTS or IF EXISTS that did their work"]++
			case a.state == sqliteFailed:
				counts["statements of IF NOT EXISTS or IF EXISTS with SQLite's error"]++
			}
		}
		if isChange(s) {
			counts["updates and deletes"]++
			switch {
			case a.state == succeeded && a.unchanged:
				counts["updates and deletes that succeeded and changed no row"]++
			case a.state == succeeded:
				counts["updates and deletes that succeeded and changed rows"]++
			case a.state == sqliteFailed:
				counts["updates and deletes with SQLite's error"]++
			}
		}
	}
	counts["indexes dropped while the rows of a query that read by them were open"] = r.droppedUnder()
	counts["acknowledged writes"] = len(r.acked)
	for _, tx := range r.txs {
		counts[map[bool]string{false: "write transactions", true: "read-only transactions"}[tx.readOnly]]++
		if tx.end != "" {
			counts["transactions "+tx.end]++
		}
		counts["statements in a transaction"] += len(tx.stmts)
	}
	log := readLog(r.store)
	counts["commits"] = len(log)
	counts["rows of several columns"] = r.wide
	counts["NULL values in the log"] = r.nulls
	counts["values of a default in the log"] = r.defaults
	counts["create tables with a default in the log"] = r.defaultTables
	for _, c := range log {
		if slices.ContainsFunc(c.params, func(ps []*s4qlpb.Value) bool { return len(ps) > 0 }) {
			counts["commits with a parameter"]++
		}
		if len(c.notes) > 1 {
			counts["commits of more than one statement"]++
		}
	}
	return counts
}

// errorNames match the names in SQLite's errors, which [errorKind] leaves out.
var errorNames = []*regexp.Regexp{
	regexp.MustCompile(`(?s)^(index ).*( already exists)$`),
	regexp.MustCompile(`(?s)^(there is already a table named ).*()$`),
	regexp.MustCompile(`(?s)^(table ).*( has no column named ).*()$`),
	regexp.MustCompile(`(?s)^([^:]*: ).*()$`),
}

// errorKind of the text of SQLite's error err, as an attempt's answer gives it: its codes and its
// message without the names in it, so that errors of one kind count together.
func errorKind(err string) string {
	codes, message, _ := strings.Cut(err, ": ")
	for _, re := range errorNames {
		if m := re.FindStringSubmatch(message); m != nil {
			message = strings.Join(m[1:], "…")
			break
		}
	}
	return codes + ": " + message
}

// droppedUnder is the number of queries that read by an index, gave their rows, and were read to
// their end, while a drop index of the index committed on their node: one of its own that started
// and returned in that time, or one of a transaction whose commit was called and returned in it.
// Those rows are compared with SQLite on the state that the query saw, which holds the index.
func (r *run) droppedUnder() int {
	var drops []*attempt
	for _, a := range r.order {
		if s, _ := stmt.Check(a.src); a.state == succeeded && a.index != "" {
			if _, ok := s.(*stmt.DropIndex); ok && (a.tx == nil || a.tx.end == "committed") {
				drops = append(drops, a)
			}
		}
	}
	var n int
	for _, q := range r.order {
		if !q.query || q.state != succeeded || q.partial || q.access != "index" {
			continue
		}
		if slices.ContainsFunc(drops, func(d *attempt) bool {
			start, stop := d.start, d.stop
			if d.tx != nil {
				start, stop = d.tx.ending, d.tx.stop
			}
			return d.node == q.node && fold(d.index) == fold(q.index) && q.start < start && stop < q.stop
		}) {
			n++
		}
	}
	return n
}

// TestSimulation runs the default workload, the idle workload, and the hammer workload for many seeds each, and twice
// for some, whose traces must be the same. In a run of at least 50 seeds, every kind of outcome, of
// fault of the store, and of event must occur at least as often as reached says, for each seed:
// about a quarter of what the first 1,000 seeds give, so that the test fails when a kind stops
// happening, and not by chance. And at most the share noLease of the statements may be refused
// for want of the lease, about twice what the first 1,000 seeds give, so that the test fails when
// most statements no longer reach a transaction. The seeds are the same in every run, so the counts
// are too. With the flag -replay, it replays that one seed instead: see [replaySeed].
func TestSimulation(t *testing.T) {
	if *replay != "" {
		replaySeed(t)
		return
	}
	n := simSeeds(t, 50)
	if slow() {
		n = max(n/10, 2)
	}
	for _, c := range []struct {
		name string
		w    workload
		// reached is the least number of each kind for each seed.
		reached map[string]float64
		// noLease is the largest share of the statements that may be refused for want of the lease.
		noLease float64
	}{
		// Each least number is a quarter of what the first 1,000 seeds gave on 2026-11-20, rounded down
		// to two digits, or one in fifty seeds for a kind of which they gave 60 to 80; and every seed
		// checks the integrity of each cache. The numbers moved that day, since the sequence writes
		// create indexes and drop indexes, which take turns of the other statements, the sequence of
		// every second seed is indexed, a quarter of the engines of a node force an index path, where a
		// quarter read every row and a half by the rules, and a query that reads by an index has the
		// index dropped while its rows are open. [run.countWhere] and [tallyOf] count them.
		{"default", defaultWorkload, map[string]float64{
			"succeeded": 5.4, "SQLite's error": 12, "unknown": 0.1, "pending": 0.18, "skipped": 3.8, "failed: no lease": 0.85,
			"failed: stopped": 0.034, "failed: timeout": 0.2, "query not read to its end": 0.44, "acknowledged writes": 3.2,
			"commits": 2.3, "commits with a parameter": 0.61, "crash, then an empty cache": 0.12,
			"crash, then a copy of the cache": 0.12, "close and open": 0.32,
			"fault: lost request": 0.4, "fault: lost response": 0.38, "fault: no answer": 0.35, "fault: conflict": 0.072,
			"fault: slow way": 0.84, "fault: broken read": 0.25, "fault: cut-off": 0.12, "fault: outage": 0.048,
			"fault: write applied after its caller gave up": 0.081, "fault: write stuck in the network": 0.22,
			"fault: write applied after a crash": 0.034,
			"statements in a transaction":        12, "read-only transactions": 0.87, "transactions committed": 2.2,
			"transactions rolled back": 0.79, "transactions not committed": 0.063, "transactions unknown": 0.052,
			"commits of more than one statement": 0.35, "refused for the type": 0.77, "rows of several columns": 0.81,
			"NULL values in the log": 0.27,
			// The selects with WHERE that the checks compare with SQLite, of their own and in a transaction: those
			// on an engine that reads every row, on one that forces an index path, and those read by an index,
			// and those that give rows. The selects of their own read by an index have no least number here,
			// since a quarter of what the first 1,000 seeds give is less than one in 50 seeds; the hammer
			// workload requires them.
			"selects with WHERE of their own compared with SQLite":                                             0.6,
			"selects with WHERE of their own compared with SQLite, on an engine that reads every row":          0.094,
			"selects with WHERE of their own compared with SQLite, on an engine that forces an index path":     0.1,
			"selects with WHERE of their own that give rows, compared with SQLite":                             0.037,
			"selects with WHERE in a transaction compared with SQLite":                                         0.88,
			"selects with WHERE in a transaction compared with SQLite, on an engine that reads every row":      0.15,
			"selects with WHERE in a transaction compared with SQLite, on an engine that forces an index path": 0.15,
			"selects with WHERE in a transaction compared with SQLite, read by an index":                       0.034,
			"selects with WHERE in a transaction that give rows, compared with SQLite":                         0.052,
			// The updates and deletes: those that succeeded and changed rows, those with SQLite's error, those of
			// their own that changed no row and that the checks run in SQLite, and those whose count of changes
			// the checks compare with SQLite's.
			"updates and deletes": 3.5, "updates and deletes that succeeded and changed rows": 0.27,
			"updates and deletes with SQLite's error": 1.6, "updates and deletes that change no row, compared with SQLite": 0.35,
			"updates and deletes of their own whose count of changes is compared with SQLite":     0.11,
			"updates and deletes in a transaction whose count of changes is compared with SQLite": 0.51,
			// The create indexes and drop indexes, by outcome; the selects, updates, and deletes that read by an
			// index; the statements on an engine that forces an index path, and those that read by its index;
			// the replays of a log with an index; and the queries whose index was dropped while their rows were
			// open.
			"create index: ok": 0.43, "create index: 1/1: index … already exists": 0.65,
			"create index: 1/1: no such column: …": 0.11, "create index: 1/1: no such table: …": 1.1,
			"create index: 1/1: there is already a table named …":     0.56,
			"create index: 8/8: attempt to write a readonly database": 0.079, "drop index: ok": 0.14,
			"drop index: 1/1: no such index: …": 0.45, "drop index: 8/8: attempt to write a readonly database": 0.022,
			"selects read by an index": 0.073, "updates read by an index": 0.035, "deletes read by an index": 0.034,
			"statements on an engine that forces an index path":     3.1,
			"statements read by the index that their engine forces": 0.07,
			"crash, then an empty cache, of a log with an index":    0.034, "replays of a log with an index": 0.12,
			"indexes dropped while the rows of a query that read by them were open": 0.02,
			// Every seed checks the integrity of the cache of each crash, of the replay, and of each live node
			// at the end.
			"integrity checks after a crash": 1, "integrity checks after the replay": 1, "integrity checks at the end": 2,
			// The statements of slice 10, which one create and drop in three of the sequence is since
			// 2026-11-20: those that did nothing, did their work, and failed with SQLite's error, and those
			// that did nothing and that the checks run in SQLite, of their own and in a transaction.
			"statements of IF NOT EXISTS or IF EXISTS that did nothing": 0.98, "statements of IF NOT EXISTS or IF EXISTS that did their work": 0.54,
			"statements of IF NOT EXISTS or IF EXISTS with SQLite's error": 0.76, "statements that do nothing, compared with SQLite": 0.41,
			"statements that do nothing in a transaction, compared with SQLite": 0.57,
			// The defaults of slice 11, which the shapes of the sequence have since 2026-11-21: the create
			// tables of the log with a default, and the values of its rows that are the default of their
			// column, NULL apart, which [run.verifyRows] counts.
			"create tables with a default in the log": 0.34, "values of a default in the log": 0.3,
		}, 0.15},
		// The ends of transactions for the lease, which only this workload reaches.
		{"idle", idleWorkload, map[string]float64{
			"succeeded": 1.6, "SQLite's error": 6.8, "pending": 0.17, "skipped": 2.3, "failed: no lease": 1.6,
			"failed: stopped": 0.73, "failed: timeout": 3, "query not read to its end": 0.12, "acknowledged writes": 0.58,
			"commits": 0.42, "commits with a parameter": 0.088, "crash, then an empty cache": 0.12,
			"crash, then a copy of the cache": 0.12, "close and open": 0.33,
			"fault: lost request": 0.32, "fault: lost response": 0.31, "fault: no answer": 2.7, "fault: conflict": 0.14,
			"fault: slow way": 0.93, "fault: broken read": 0.24, "fault: cut-off": 0.14, "fault: outage": 0.066,
			"fault: write applied after its caller gave up": 0.11, "fault: write stuck in the network": 4.2,
			"statements in a transaction": 7.6, "read-only transactions": 0.74, "transactions committed": 0.75,
			"transactions rolled back": 0.78, "transactions not committed": 0.039, "commits of more than one statement": 0.065,
			"transactions ended for the lease": 0.34, "kept transactions ended for the lease": 0.33,
			"refused for the type": 0.17, "rows of several columns": 0.13, "NULL values in the log": 0.045,
			"selects with WHERE of their own compared with SQLite":                                             0.51,
			"selects with WHERE of their own compared with SQLite, on an engine that reads every row":          0.086,
			"selects with WHERE of their own compared with SQLite, on an engine that forces an index path":     0.081,
			"selects with WHERE in a transaction compared with SQLite":                                         0.57,
			"selects with WHERE in a transaction compared with SQLite, on an engine that reads every row":      0.095,
			"selects with WHERE in a transaction compared with SQLite, on an engine that forces an index path": 0.085,
			"updates and deletes": 2.4, "updates and deletes that succeeded and changed rows": 0.043,
			"updates and deletes with SQLite's error":                                             0.93,
			"updates and deletes that change no row, compared with SQLite":                        0.052,
			"updates and deletes in a transaction whose count of changes is compared with SQLite": 0.14,
			"create index: ok": 0.14, "create index: 1/1: index … already exists": 0.089,
			"create index: 1/1: no such column: …": 0.039, "create index: 1/1: no such table: …": 0.87,
			"create index: 1/1: there is already a table named …":     0.091,
			"create index: 8/8: attempt to write a readonly database": 0.036, "drop index: ok": 0.02,
			"drop index: 1/1: no such index: …":                 0.23,
			"statements on an engine that forces an index path": 1.4,
			"replays of a log with an index":                    0.038,
			"integrity checks after a crash":                    1, "integrity checks after the replay": 1, "integrity checks at the end": 2,
			"statements of IF NOT EXISTS or IF EXISTS that did nothing": 0.31, "statements of IF NOT EXISTS or IF EXISTS that did their work": 0.25,
			"statements of IF NOT EXISTS or IF EXISTS with SQLite's error": 0.43, "statements that do nothing, compared with SQLite": 0.068,
			"statements that do nothing in a transaction, compared with SQLite": 0.24,
			"create tables with a default in the log":                           0.1, "values of a default in the log": 0.055,
		}, 0.15},
		// The least number of the selects with WHERE in a transaction read by an index is a quarter of
		// the 186 that the first 1,000 seeds gave on 2026-11-20, where it was 0.06, since the sequence
		// writes the clauses of slice 10 and draws other random numbers.
		{"hammer", hammerWorkload, map[string]float64{
			"succeeded": 10, "SQLite's error": 23, "unknown": 0.2, "skipped": 7.8, "failed: no lease": 1.2,
			"failed: timeout": 2.9, "query not read to its end": 0.96, "acknowledged writes": 5.9, "commits": 4.2,
			"commits with a parameter": 1.2,
			"fault: lost request":      0.23, "fault: lost response": 0.21, "fault: no answer": 0.25, "fault: conflict": 0.14,
			// The broken reads have no least number here: the first 1,000 seeds gave 81 on 2026-11-21, a
			// quarter of which is less than one in 50 seeds, and the first 50 give none, since the sequence
			// writes defaults and draws other random numbers. The default and idle workloads require 12
			// each.
			"fault: slow way": 0.45, "fault: cut-off": 0.058, "fault: outage": 0.026,
			"fault: write applied after its caller gave up": 0.079, "fault: write stuck in the network": 0.41,
			"statements in a transaction": 24, "read-only transactions": 1.8, "transactions committed": 4.2,
			"transactions rolled back": 1.5, "transactions not committed": 0.15, "transactions unknown": 0.1,
			"commits of more than one statement": 0.64, "refused for the type": 1.5, "rows of several columns": 1.7,
			"NULL values in the log":                                                     0.56,
			"selects with WHERE of their own compared with SQLite":                       1.3,
			"selects with WHERE of their own compared with SQLite, read by an index":     0.027,
			"selects with WHERE of their own that give rows, compared with SQLite":       0.089,
			"selects with WHERE in a transaction compared with SQLite":                   1.6,
			"selects with WHERE in a transaction compared with SQLite, read by an index": 0.046,
			"selects with WHERE in a transaction that give rows, compared with SQLite":   0.11,
			"updates and deletes": 7, "updates and deletes that succeeded and changed rows": 0.6,
			"updates and deletes with SQLite's error": 2.9, "updates and deletes that change no row, compared with SQLite": 0.69,
			"updates and deletes of their own whose count of changes is compared with SQLite":     0.27,
			"updates and deletes in a transaction whose count of changes is compared with SQLite": 1,
			"create index: ok": 0.7, "create index: 1/1: index … already exists": 1.3,
			"create index: 1/1: no such column: …": 0.18, "create index: 1/1: no such table: …": 1.8,
			"create index: 1/1: there is already a table named …":     1.4,
			"create index: 8/8: attempt to write a readonly database": 0.16, "drop index: ok": 0.26,
			"drop index: 1/1: no such index: …": 0.85, "drop index: 8/8: attempt to write a readonly database": 0.042,
			"selects read by an index": 0.14, "updates read by an index": 0.063, "deletes read by an index": 0.051,
			"replays of a log with an index":                                        0.11,
			"indexes dropped while the rows of a query that read by them were open": 0.03,
			"integrity checks after the replay":                                     1, "integrity checks at the end": 1,
			"statements of IF NOT EXISTS or IF EXISTS that did nothing": 1.9, "statements of IF NOT EXISTS or IF EXISTS that did their work": 0.92,
			"statements of IF NOT EXISTS or IF EXISTS with SQLite's error": 1.4, "statements that do nothing, compared with SQLite": 0.81,
			"statements that do nothing in a transaction, compared with SQLite": 1.1,
			"create tables with a default in the log":                           0.49, "values of a default in the log": 0.6,
		}, 0.12},
	} {
		t.Run(c.name, func(t *testing.T) {
			counts := map[string]int{}
			for seed := range n {
				s := simulate(t, seed, c.w)
				if v := s.violations(); len(v) > 0 {
					t.Fatalf("seed %v: %v\nreplay it with: %v", seed, strings.Join(v, "\n"), replayCommand(seed, "-workload", c.name))
				}
				for k, v := range tallyOf(s) {
					counts[k] += v
				}
				counts["trace lines"] += len(s.lines)
				if seed < 3 {
					again := simulate(t, seed, c.w)
					is.True(t, slices.Equal(s.lines, again.lines), fmt.Sprint("seed ", seed, " gives two traces; replay it with: ", replayCommand(seed, "-workload", c.name)))
				}
			}
			t.Logf("%v seeds: %v", n, counts)
			if n < 50 {
				return
			}
			for kind, least := range c.reached {
				want := int(least * float64(n))
				if least > 0 && want == 0 {
					t.Errorf("%v seeds require none of %q at its least number %v, so it checks nothing: raise it or drop it, and say where the kind is still required", n, kind, least)
				}
				if counts[kind] < want {
					t.Errorf("%v seeds give %v of %q, and at least %v were expected: something that the workload did stopped happening", n, counts[kind], kind, want)
				}
			}
			if share := float64(counts["failed: no lease"]) / float64(counts["statements"]); share > c.noLease {
				t.Errorf("%v seeds refuse %v of %v statements for want of the lease, %.2f of them, and at most %v were expected: most statements must reach a transaction", n, counts["failed: no lease"], counts["statements"], share, c.noLease)
			}
		})
	}
}

// The flags of the test binary that replay one seed of [TestSimulation]: see [replaySeed].
var (
	replay         = flag.String("replay", "", "replay this one seed in TestSimulation, and nothing else, and log its trace")
	replayWorkload = flag.String("workload", "default", "workload of the replay: default, hammer, or idle")
	replayBug      = flag.String("bug", "", "planted bug of the replay")
)

// replaySeed runs the seed of the flag -replay, of the workload of the flag -workload, with the bug
// of the flag -bug, and logs its trace. [replayCommand] gives the command.
func replaySeed(t *testing.T) {
	t.Helper()
	seed, err := strconv.ParseUint(*replay, 10, 64)
	is.NotError(t, err)
	w := defaultWorkload
	switch *replayWorkload {
	case "default":
	case "hammer":
		w = hammerWorkload
	case "idle":
		w = idleWorkload
	default:
		t.Fatalf("no workload is named %v", *replayWorkload)
	}
	if *replayBug != "" {
		w.bugs = []string{*replayBug}
	}
	s := simulate(t, seed, w)
	for _, line := range s.lines {
		t.Log(line)
	}
	for _, v := range s.violations() {
		t.Error(v)
	}
}

// replayCommand is the command that replays the seed with [replaySeed], with the flags that choose
// the workload and the bug, such as "-workload", "hammer", and the race detector or the address
// sanitizer if the test binary has one, which change what a run does.
func replayCommand(seed uint64, flags ...string) string {
	var mode string
	if raceEnabled {
		mode = "-race "
	}
	if asanEnabled {
		mode = "-asan "
	}
	return "go test " + mode + "-run '^TestSimulation$' -v ./internal/engine/ -replay " + fmt.Sprint(seed) + " " + strings.Join(flags, " ")
}

// TestSimulation_plantedBugs runs the workload with each planted bug until it finds the bug by its own
// violation in two seeds, and the same seeds without the bug, which find nothing. With
// S4QL_TEST_SHARD set, it runs the bugs of that shard only: see [testshard].
func TestSimulation_plantedBugs(t *testing.T) {
	limit := simSeeds(t, 100)
	// The workload of each bug, and the violation that finds it: the check of an insert in a read
	// transaction of its own needs two inserts of one key on one instance at once.
	bugs := []struct {
		name string
		w    workload
		// replay is the name of the workload for the flag -workload.
		replay string
		// finds holds a part of each violation that the bug causes, its own violation first, which the
		// bug must cause in two seeds at least.
		finds []string
	}{
		// An insert of a write transaction that replaces a row succeeds, where SQLite fails.
		{"skipUnique", defaultWorkload, "default", []string{"fails in SQLite: 19/1555: UNIQUE constraint failed", "(write), which began at commit"}},
		{"ackUnknown", defaultWorkload, "default", []string{"is not in the log"}},
		{"checkOutside", hammerWorkload, "hammer", []string{"fails in SQLite: 19/1555: UNIQUE constraint failed"}},
		// A write transaction left open keeps the instance from a write, and from a close that
		// releases the lease, so the next instance never runs the queries of its connections.
		{"keepOpen", defaultWorkload, "default", []string{"no instance runs a write", "a query on each connection of its callers ten minutes after the end of the workload"}},
		// A read transaction left open fails every later query of its connection, and the check of
		// the connections after the workload.
		// In a transaction, the rows of the query stay open, which refuses every later statement of the
		// connection.
		{"keepRead", defaultWorkload, "default", []string{"a read transaction left open after the end of the workload", "the reader has a read transaction open", "the rows of an earlier query of the connection are open"}},
		// An annotation with the wrong parameters gives a log that, replayed, inserts other rows than
		// the callers did, and maybe one that a later insert of the log meets.
		{"annotateNext", defaultWorkload, "default", []string{"and its caller gave the arguments", "the replay of the log holds", "fails in SQLite: 19/1555: UNIQUE constraint failed"}},
		// A failed statement that is kept is in the commit object of its transaction, whose replay in
		// SQLite then fails. What it changed before it failed is kept too: an update that changes the key
		// of a row that the transaction inserted, and fails with UNIQUE after the delete of the old key,
		// leaves the insert and the delete, whose net effect is nothing, so the commit object holds
		// neither, and the insert that its caller was told of is not in the log.
		{"keepFailed", defaultWorkload, "default", []string{"whose statements that succeeded and write are", "fails in SQLite", "is not in the log"}},
		// A read-only transaction that moves on to the newest state reads rows that were committed after
		// its beginning, and its later statements report the newer state. When its read of the newest
		// state fails, the transaction keeps the read that the bug ended, whose end then fails.
		{"readNewest", hammerWorkload, "hammer", []string{"(read-only), which began at commit", "the statements of the read-only transaction", "the transaction has ended"}},
		{"rollbackKeeps", defaultWorkload, "default", []string{"and its caller rolled it back"}},
		// SQLite converts the real of the log as the bug did, so the replay of the log agrees with the
		// engine, and only the check of the type of each value of the log finds it. In a table of
		// several columns, the real that the bug converts can be the first value of another type than
		// its column, and the engine then refuses a later one, where the rule refuses the real. The bug
		// needs an insert of a whole real into an INT or INTEGER column that otherwise succeeds, which
		// a sequence gives rarely; the hammer workload runs more statements than the default one, on
		// one instance that no other instance takes the lease from. Of the first 200 seeds, with WHERE
		// in the sequence, 14 found the bug by its own violation there and 12 in the default workload;
		// with updates and deletes in it, 18 there; and on 2026-11-20, with indexes in it, which take
		// turns of the inserts, 8 there, and the second of them is seed 59. Since the sequence of the
		// hammer workload gives whole reals for INT and INTEGER columns, [stmttest.Sequence.Mistyped],
		// 81 there, and with the clauses of slice 10 too, 72.
		{"convertReal", hammerWorkload, "hammer", []string{"stores a value of another type than its column", "and the rule for the type of a value"}},
		// A row that holds its values in other columns than its statement names: the replay of the log
		// into an empty cache, which writes the rows, differs from SQLite, which runs the statements; a
		// row whose key is another value makes a later insert of the log fail in SQLite, and a query
		// give other rows than in SQLite; and a value in a column of another type is refused where the
		// rule refuses nothing. The log holds no such value, since the check of the type sees the values
		// in the columns where the bug puts them.
		{"listOrder", defaultWorkload, "default", []string{"the replay of the log holds", "fails in SQLite", ", and in SQLite", "and the rule for the type of a value", "rows in SQLite, and its caller was told", "is in no commit, and changes", "which began at commit"}},
		// A column left out of a list that holds the zero value of its type where SQLite has NULL: a
		// query of the row gives other rows than in SQLite, which runs the statements of the log, and
		// so does the replay of the log into an empty cache, which writes its rows. No check of a value
		// that is not NULL finds it: the zero value has the type of its column, and no column that
		// takes NULL is the key. An update or a delete whose WHERE meets the zero value changes another
		// number of rows than in SQLite, and then the statements after it can differ. Since the shapes
		// of the sequence have defaults, on 2026-11-21, the bug meets only a column without one, and
		// the default workload found it in 6 of the first 200 seeds, where it found it in 39; the hammer
		// workload, which runs more statements, finds it in 18, 15 of them by its own violation.
		{"zeroLeftOut", hammerWorkload, "hammer", []string{", and in SQLite", "the replay of the log holds", "rows in SQLite, and its caller was told", "is in no commit, and changes", "which began at commit", "fails in SQLite"}},
		// A scan by one or two bounds of the key that leaves out the row of a closed bound: a select with
		// WHERE, of its own or in a transaction, gives fewer rows than in SQLite. Nothing else sees it:
		// the select writes nothing, and fails nothing. An engine that reads every row, as some engines
		// of a node do (see [run.open]), does not have the bug. The bug needs a select whose bound of the key is
		// the key of a row, which the updates and deletes of the sequence made rarer: they take turns of
		// the selects, and delete rows. The hammer workload runs more statements than the default one,
		// on one engine that reads by the key. Of the first 200 seeds, with updates and deletes in the
		// sequence, 32 found the bug there and 8 in the default workload; on 2026-11-20, with indexes
		// in it, 27 there; and with the bounds of the key of updates and deletes, and the whole reals of
		// [stmttest.Sequence.Mistyped], 24, and with the clauses of slice 10 too, 18.
		{"openBound", hammerWorkload, "hammer", []string{", and in SQLite", "which began at commit"}},
		// A scan that pins a column of the key of an index, or of the primary key, to the value of a
		// condition on another column. Where the value is of another type than the column, or NULL in
		// the primary key, the storage core refuses the scan, which the engine had checked, as an
		// internal error. Otherwise a select gives other rows than in SQLite, and an update or a delete
		// changes other rows, after which the statements can differ. Of the first 200 seeds, on
		// 2026-11-20, with the sequence of every second seed indexed, 19 found the bug, each by the
		// refusal of the core, and the second of them is seed 23; with the bounds of the key of updates
		// and deletes, 22, and with the clauses of slice 10 too, 28.
		{"pinByPlace", defaultWorkload, "default", []string{"the storage core refused a call that the engine had checked", ", and in SQLite", "which began at commit", "rows in SQLite, and its caller was told", "is in no commit, and changes", "the replay of the log holds", "fails in SQLite"}},
		// An update or a delete that changes nothing and says so: of its own, it is in no commit, and
		// SQLite, which runs it on the state that it saw, changes rows; in a transaction, SQLite gives
		// another count; and the rows that it should have changed make later statements differ.
		{"changeNothing", defaultWorkload, "default", []string{"is in no commit, and changes", "rows in SQLite, and its caller was told", ", and in SQLite", "which began at commit", "the replay of the log holds", "fails in SQLite"}},
		// An update that changes the key and leaves the old row: the commit object holds the new row and
		// no delete of the old key, so the replay of the log into an empty cache, which writes its rows,
		// differs from SQLite, which runs the statements; and the row that stays makes later statements
		// differ, among them a write whose net effect on the rows that the bug left is nothing, which is
		// then in no commit object, and not in the log that its caller was told of.
		{"keepOldKey", defaultWorkload, "default", []string{"the replay of the log holds", ", and in SQLite", "which began at commit", "rows in SQLite, and its caller was told", "fails in SQLite", "is in no commit, and changes", "is not in the log"}},
		// A create table with IF NOT EXISTS of a table that exists, which replaces the table with the
		// one of the statement, empty, where SQLite does nothing: the commit object holds the drop and
		// the create, so the replay of the log into an empty cache, which writes them, differs from
		// SQLite, which runs the statement and does nothing; and the rows that the table lost make later
		// statements differ.
		{"replaceExisting", defaultWorkload, "default", []string{"the replay of the log holds", ", and in SQLite", "which began at commit", "rows in SQLite, and its caller was told", "fails in SQLite", "is in no commit, and changes", "and the rule for the type of a value"}},
		// An insert that leaves out a column with a default and stores NULL there: a query of the row
		// gives other rows than in SQLite, which runs the statements of the log, and so does the replay
		// of the log into an empty cache, which writes its rows; where the column does not take NULL, the
		// engine gives NOT NULL where SQLite stores the default, so a later statement can differ. Of the
		// first 200 seeds, on 2026-11-21, 56 found the bug, 53 of them by its own violation. A write in a
		// transaction plants it too.
		{"nullDefault", defaultWorkload, "default", []string{", and in SQLite", "the replay of the log holds", "which began at commit", "rows in SQLite, and its caller was told", "fails in SQLite", "is in no commit, and changes", "and the rule for the type of a value"}},
	}
	shard := testshard.Get(t)
	for i, b := range bugs {
		if !shard.Holds(i) {
			continue
		}
		bug := b.name
		t.Run(bug, func(t *testing.T) {
			w := b.w
			w.bugs = []string{bug}
			// own is the number of seeds that find the bug by its own violation.
			found, own, seeds := 0, 0, uint64(0)
			for seed := uint64(0); seed < limit && (own < 2 || os.Getenv("S4QL_ENGINE_SIM_SEEDS") != ""); seed++ {
				seeds++
				if v := simulate(t, seed, w).violations(); len(v) > 0 {
					if !slices.ContainsFunc(v, func(v string) bool {
						return slices.ContainsFunc(b.finds, func(f string) bool { return strings.Contains(v, f) })
					}) {
						t.Fatalf("seed %v finds the bug by another violation: %v\nreplay it with: %v", seed, v, replayCommand(seed, "-workload", b.replay, "-bug", bug))
					}
					if found == 0 {
						t.Logf("seed %v finds: %v", seed, v[0])
					}
					found++
					if slices.ContainsFunc(v, func(v string) bool { return strings.Contains(v, b.finds[0]) }) {
						own++
					}
					if v := simulate(t, seed, b.w).violations(); len(v) > 0 {
						t.Fatalf("seed %v finds something without the bug: %v\nreplay it with: %v", seed, v, replayCommand(seed, "-workload", b.replay))
					}
				}
			}
			t.Logf("%v of %v seeds find the bug, %v of them by %q", found, seeds, own, b.finds[0])
			is.True(t, own >= 2, fmt.Sprintf("the bug is found by %q too rarely", b.finds[0]))
		})
	}
}
