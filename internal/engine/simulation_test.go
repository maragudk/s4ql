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
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/core/coretest"
	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/expr"
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

// hostRand is the [stmttest.Rand] of a host, for what the callers of a simulation draw for
// themselves: their waits, their transactions, and how they read the rows of a query. The statements
// come from the sequence of the seed, whatever the scheduler does.
type hostRand struct {
	h host.Host
}

func (r hostRand) IntN(n int) int { return int(r.h.Uint64() % uint64(n)) }
func (r hostRand) Uint64() uint64 { return r.h.Uint64() }

// workload of engine statements that callers run against instances of one database, under the
// scheduler, while the store fails, hangs, and loses requests: the environment of a simulation. Its
// statements are those of the sequence of the seed, with the profile of the seed, as
// [stmttest.ProfileOf] gives it, so a simulation is an environment and a profile, and a test runs an
// environment on the seeds of the profiles that it names. One turn of a caller in
// [stmttest.Profile.Transactions] is a transaction that spans statements instead of a statement of
// its own: see [run.transaction].
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
	// idlers is the number of callers of each instance that hold a write transaction open and idle
	// across a cut-off of their instance from the store, for up to idle, and leave the last one open
	// after the end of the workload: see [run.idler].
	idlers int
	idle   time.Duration
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
	// succeeded is a statement that succeeded: a write that succeeded, whose commit returned outside a
	// transaction that spans statements, or a query that gave its rows.
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
	// notRun is a statement of a text of several statements after the one that failed, which did not
	// run, and so did nothing.
	notRun
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
	// committed is the number of writes whose commit had returned when the statement started, or, for a
	// statement of a transaction that spans statements, when the begin of the transaction was called.
	committed int
	// partial is set for a query whose rows were not read to their end.
	partial bool
	// typeRefused is set for an insert, or a select with WHERE, that the engine refused as "not
	// supported" for a value of another type than its column, and for a select with LIMIT that it
	// refused so for the argument of the LIMIT, as [typeRule] says for both.
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
	// subqueries are the values of the scalar subqueries of the statement, of slice 15, in the order of
	// the text, as the engine computed them the last time that it ran the statement, which its hook
	// gives the run (see [run.open]), or nil for a statement whose subqueries it did not compute.
	subqueries []*s4qlpb.Value
	// newer is set for a statement of a transaction that spans statements that started after a write
	// was committed since the begin of the transaction, which it must not see.
	newer bool
	// text is the text of several statements that the statement is of, or nil for a statement that
	// ran alone: see [run.several].
	text *severalText
}

// txRecord is a transaction that spans statements, as its caller saw it.
type txRecord struct {
	id       uint64
	node     string
	gen      int
	readOnly bool
	// stmts of the transaction, in their order.
	stmts []*attempt
	// committed is the number of writes whose commit had returned when the begin was called.
	committed int
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
	// running is the number of statements that the node's callers run, and between the number of
	// texts of several statements that they run after a statement of the text succeeded.
	running   atomic.Int64
	between   atomic.Int64
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
	committed  []uint64
	violations []string
	nextID     uint64
	// txs are the transactions that span statements, in the order of their begins.
	txs []*txRecord
	// texts of several statements that ran through ExecAll, in the order of their calls.
	texts  []*severalText
	nextTx uint64
	// clock of events of the run, which numbers the begins and ends of calls, so that their order
	// can be compared.
	clock uint64
	// q is the sequence of statements that every caller takes its next statement from, so that
	// they meet at the same tables.
	q *sequence
	// crashed databases, whose files [simulate] closes after the run: see [run.crash].
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
	var ev severalEvent
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key != engine.AttrSQL {
			fmt.Fprint(&b, " ", a.Key, "=", a.Value)
		}
		switch a.Key {
		case engine.AttrSQL:
			ev.sql = true
			if m := idPattern.FindStringSubmatch(a.Value.String()); m != nil {
				ev.id, _ = strconv.ParseUint(m[1], 10, 64)
			}
		case engine.AttrOutcome:
			ev.outcome = a.Value.String()
		case engine.AttrRows:
			ev.rows = a.Value.Int64()
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
	if text, ok := ctx.Value(severalKey{}).(*severalText); ok && rec.Message == engine.EventStatement {
		h.r.mu.Lock()
		h.r.logged(text, ev, position, access, index, nothing)
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
	// A crash copies the files of the cache as the operating system holds them, and no sync fails,
	// so syncs of the cache would check nothing.
	d, err := core.Open(ctx, n.inst, n.client, n.path, "db", coretest.SyncOff(core.Options{
		BusyTimeout:   time.Minute,
		ObjectTimeout: 10 * time.Second,
		Lease:         n.lease,
		Logger:        slog.New(coreHandler{r: r, key: key}),
	}))
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
	// A sequence of a profile without indexes has no name of an index to force a path of.
	if names := r.q.IndexNames(); other && (draw%2 == 0 || len(names) == 0) {
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
	e := engine.New(d, n.inst, options)
	// The values that the engine computes for the subqueries of a statement, by the attempt of its
	// context, which [run.flattened] checks against SQLite's candidates and SQLite then runs with:
	// where the ORDER BY of a subquery does not decide its first row, the engine and SQLite may take
	// another each.
	e.ObserveSubqueries(func(ctx context.Context, vs []*s4qlpb.Value) []*s4qlpb.Value {
		if id, ok := ctx.Value(idKey{}).(uint64); ok {
			r.mu.Lock()
			if a := r.attempts[id]; a != nil {
				a.subqueries = slices.Clone(vs)
			}
			r.mu.Unlock()
		}
		if text, ok := ctx.Value(severalKey{}).(*severalText); ok {
			// They are of the statement after those whose events are logged, which runs now, also if its
			// instance crashes before its event.
			r.mu.Lock()
			if k := len(text.events); k < len(text.attempts) {
				text.attempts[k].subqueries = slices.Clone(vs)
			}
			r.mu.Unlock()
		}
		return vs
	})
	n.mu.Lock()
	n.d, n.e = d, e
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
	if call.Statements > 0 && !call.Query {
		return r.several(ctx, n, c, gen, call, tx)
	}
	query := call.Query
	r.mu.Lock()
	a := r.newAttempt(n, gen, call.Src, values(call.Args), query, tx)
	call.Src = a.src
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
	r.classify(ctx, a, call, res, got, err, 0)
	return a
}

// severalKey is the key of the context of a call of [engine.Conn.ExecAll] that holds its text.
type severalKey struct{}

// severalText is a text of several statements that a caller ran in one call of
// [engine.Conn.ExecAll], as its caller saw it: an attempt for each of its statements, as SQLite reads
// them, or one attempt of the whole text that s4ql's rules refuse before any statement runs.
type severalText struct {
	n  *node
	tx *txRecord
	// between is set once a statement of the text succeeded that another follows, until the call
	// returns, while the text counts in [node.between].
	between bool
	// refused is set for a text that the rules refuse before any statement runs, with the answer want.
	refused bool
	want    answer
	// attempts of the statements, in their order, and calls, the call of each as a statement alone,
	// with its own arguments, as [checkArguments] judges it. base is where the text of each starts
	// in the text that ran, and few the error of each that gets fewer arguments than it has
	// parameters, as the rule of ExecAll writes it, or "".
	attempts []*attempt
	calls    []stmttest.Call
	base     []int
	few      []string
	// events of statements that the engine logged in the call, in their order, and failedAt the
	// number of the first of them whose outcome was not ok, or -1.
	events   []severalEvent
	failedAt int
	// returned is set when the call returned, with err.
	returned bool
	err      error
}

// severalEvent is an event of a statement that the engine logged in a call of ExecAll: whether it has
// its text, the ID that the text carries, or 0, its outcome, and its number of rows.
type severalEvent struct {
	sql     bool
	id      uint64
	outcome string
	rows    int64
}

// several runs the call of a text of several statements, which is not a query, on the connection c
// of a caller of n, whose engine has the generation gen, through [engine.Conn.ExecAll], in the
// transaction tx that spans statements, or on its own if tx is nil, and records each statement as an
// attempt of its own, as if it ran alone: the statements of [sqliteParts], each with the comment of
// its ID, as many of the arguments as it has parameters, in order, and the text with its spaces,
// comments, and empty statements before it as its text, as [pair.several] runs it. A text that the
// rules refuse before any statement runs, as [refusedBefore] says, is one attempt, which must be
// refused so. The events that the engine logs for the statements tell which ran, in order: each
// event is of the statement in its place, every statement before the first whose event is not ok
// succeeded, and outside a transaction it is committed then, by its event, which the engine logs
// after its commit returned, as for a statement of its own, also if the call does not return because
// its instance crashed and its caller never learns of it; the statement of that event ended with the
// error of the call; and the statements after it did not run. It gives the attempt of the statement
// that failed, or of the last.
func (r *run) several(ctx context.Context, n *node, c *engine.Conn, gen int, call stmttest.Call, tx *txRecord) *attempt {
	parts := sqliteParts(call.Src)
	want, why := refusedBefore(call.Src, len(call.Args), parts)
	text := &severalText{n: n, tx: tx, refused: why != "", want: want, failedAt: -1}
	src := call.Src
	if !utf8.ValidString(src) || strings.ContainsRune(src, 0) {
		// The generator writes no such text.
		r.violate(ctx, "the oracle has no rule of the text %q, which is not valid UTF-8 or holds NUL", src)
	}
	r.mu.Lock()
	if text.refused || !utf8.ValidString(src) || strings.ContainsRune(src, 0) {
		// The text writes nothing, so it needs no ID.
		text.refused = true
		a := r.newAttempt(n, gen, src, values(call.Args), false, tx)
		text.attempts, text.calls, text.base, text.few = []*attempt{a}, []stmttest.Call{call}, []int{0}, []string{""}
	} else {
		var b strings.Builder
		used := 0
		for j, part := range parts {
			piece := call.Src[part.span.Start:part.span.End]
			take, few := call.Args[used:], ""
			if s, err := stmt.Check(piece); err == nil && s != nil {
				take = call.Args[used:min(len(call.Args), used+s.Parameters())]
				if len(take) < s.Parameters() {
					// The offset of the statement in the text that runs, with the IDs before it.
					at := b.Len() + part.statement.Start - part.span.Start
					few = fmt.Sprintf("s4ql: wrong number of arguments: %v for a text whose statements up to the one at offset %v have %v %v", len(call.Args), at, used+s.Parameters(), parameterNoun(used+s.Parameters()))
				}
			}
			a := r.newAttempt(n, gen, piece, values(take), false, tx)
			supported := j < len(call.Supports) && call.Supports[j]
			text.attempts, text.base, text.few = append(text.attempts, a), append(text.base, b.Len()), append(text.few, few)
			text.calls = append(text.calls, stmttest.Call{Src: a.src, Args: take, Supported: supported})
			used += len(take)
			b.WriteString(a.src)
		}
		src = b.String()
	}
	for _, a := range text.attempts {
		a.text = text
	}
	r.texts = append(r.texts, text)
	r.mu.Unlock()
	if !text.refused {
		// The comments of the IDs are inside statements, after their first token, and must leave the
		// statements where SQLite reads them.
		again := sqliteParts(src)
		same := len(again) == len(parts)
		for j := range again {
			same = same && again[j].span.Start == text.base[j]
		}
		if !same {
			r.violate(ctx, "SQLite reads the text %q otherwise with the IDs of its statements, %q", call.Src, src)
		}
	}
	ctx = context.WithValue(ctx, severalKey{}, text)
	n.running.Add(1)
	defer n.running.Add(-1)
	res, err := c.ExecAll(ctx, src, values(call.Args)...)
	r.mu.Lock()
	defer r.mu.Unlock()
	text.returned, text.err = true, err
	if text.between {
		n.between.Add(-1)
	}
	return r.severalEnded(ctx, text, res, err)
}

// logged the event ev of a statement of the text, which the engine logged in its call of ExecAll,
// and record what it says in the attempt in its place, as [run.several] says; the values of its
// subqueries are there already, from the observer of [run.open]. The caller holds r.mu.
func (r *run) logged(text *severalText, ev severalEvent, position, access, index string, nothing bool) {
	text.events = append(text.events, ev)
	k := len(text.events) - 1
	if k >= len(text.attempts) {
		return
	}
	a := text.attempts[k]
	a.position, a.access, a.index, a.nothing = position, access, index, nothing
	a.stop = r.tick()
	if text.failedAt >= 0 || ev.outcome != "ok" {
		if text.failedAt < 0 {
			text.failedAt = k
		}
		return
	}
	a.state = succeeded
	if s, _ := stmt.Check(a.src); isChange(s) {
		a.changes, a.unchanged = ev.rows, ev.rows == 0
	}
	if a.write && !a.nothing && a.tx == nil {
		r.committed = append(r.committed, a.id)
	}
	if k+1 < len(text.attempts) && !text.between {
		text.between = true
		text.n.between.Add(1)
	}
	if k+1 < len(text.attempts) {
		next := text.attempts[k+1]
		next.start = r.tick()
		if next.tx == nil {
			next.committed = len(r.committed)
		} else {
			next.newer = len(r.committed) > next.tx.committed
		}
	}
}

// severalEnded is the end of the call of ExecAll of the text, which returned res and err: it checks
// the events of the call against how a text runs, as [run.several] says, and its result, which is
// that of the last statement that sets the count of changes, by its event, also when it fails;
// classifies the statement that failed with err; and marks the statements after it as not run. It
// gives the attempt of the statement that failed, or of the last. The caller holds r.mu.
func (r *run) severalEnded(ctx context.Context, text *severalText, res engine.Result, err error) *attempt {
	var failures []string
	evs, as := text.events, text.attempts
	if len(evs) > len(as) {
		failures = append(failures, fmt.Sprintf("ExecAll logs %v events of statements for the text %q of %v", len(evs), as[0].src, len(as)))
	}
	for j, ev := range evs {
		// A statement that s4ql does not support carries no ID, and the text of its event, the rest of
		// the text from it on, can hold the IDs of the statements after it. An event above Debug has no
		// text.
		if j < len(as) && ev.sql && ev.id != as[j].id && idPattern.MatchString(as[j].src) {
			failures = append(failures, fmt.Sprintf("ExecAll logs the event of the statement %v in the place of the statement %v", ev.id, as[j].id))
		}
	}
	if k := text.failedAt; k >= 0 && len(evs) > k+1 {
		failures = append(failures, fmt.Sprintf("ExecAll runs %v statements after the statement %v, which failed", len(evs)-k-1, as[min(k, len(as)-1)].id))
	}
	at := text.failedAt
	switch {
	case err == nil && (at >= 0 || len(evs) != len(as)):
		failures = append(failures, fmt.Sprintf("ExecAll succeeds with the text of the statements %v, and logs %v events of statements, of which the first that is not ok is %v", as[0].id, len(evs), at))
	case err != nil && at < 0:
		failures = append(failures, fmt.Sprintf("ExecAll fails with %v, and the events of its %v statements that ran are ok", err, len(evs)))
		at = len(evs)
	}
	for j, a := range as {
		if a.state != succeeded || j >= len(text.calls) {
			continue
		}
		if kind, failure := checkArguments(text.calls[j], answer{}); kind != "" {
			failures = append(failures, fmt.Sprintf("the statement %v succeeded: %v", a.id, failure))
		}
	}
	var failedOne *attempt
	if err != nil && at < len(as) {
		a := as[at]
		failedOne = a
		switch {
		case text.refused:
			got := answer{err: "not supported", message: err.Error()}
			switch {
			case errors.Is(err, engine.ErrArguments):
				got.err = wrongArguments
			case errors.Is(err, engine.ErrNoStatement):
				got.err = "no statement"
			}
			a.state, a.answer = skipped, got
			if got.err != text.want.err || got.message != text.want.message || len(evs) != 1 {
				failures = append(failures, fmt.Sprintf("ExecAll of %q, which the rules refuse before any statement runs with %v (%q), gives %v (%q) and logs %v events", a.src, text.want, text.want.message, got, got.message, len(evs)))
			}
		case text.few[at] != "":
			a.state, a.answer = skipped, answer{err: wrongArguments, message: err.Error()}
			if !errors.Is(err, engine.ErrArguments) || err.Error() != text.few[at] {
				failures = append(failures, fmt.Sprintf("the statement %v, %q, gets fewer arguments than it has parameters, and ExecAll gives %v, where the rule gives %q", a.id, a.src, err, text.few[at]))
			}
		default:
			r.classify(ctx, a, text.calls[at], engine.Result{}, answer{}, err, text.base[at])
		}
	}
	// The result is that of the last statement that sets the count of changes, by the event of each
	// that succeeded, also of a call that failed. An insert, update, or delete that failed after it
	// began to write sets the count to 0, and one that failed before, as for a table that is not there
	// or without the lease, leaves it; the simulation cannot tell the two from SQLite's error or one of
	// the core, so it takes either, and the differential tests check which. One that s4ql refused
	// before it ran, as not supported, for its arguments, or for an evaluation error of arithmetic,
	// which is skipped, leaves it.
	ran := len(evs)
	if err != nil {
		ran = min(at, ran)
	}
	var want engine.Result
	for j, a := range as[:min(ran, len(as))] {
		s, _ := stmt.Check(a.src)
		if _, insert := s.(*stmt.Insert); isChange(s) || insert {
			want = engine.Result{Changes: evs[j].rows, SetsChanges: true}
		}
	}
	wants := []engine.Result{want}
	if failedOne != nil && failedOne.state != skipped {
		s, _ := stmt.Check(failedOne.src)
		if _, insert := s.(*stmt.Insert); isChange(s) || insert {
			wants = append(wants, engine.Result{SetsChanges: true})
		}
	}
	if !slices.Contains(wants, res) {
		failures = append(failures, fmt.Sprintf("ExecAll of the text of the statements %v gives the result %+v and the error %v, and the events of its statements give one of %+v", as[0].id, res, err, wants))
	}
	for j, a := range as {
		if a.state == pending && (err == nil || j > at) {
			a.state, a.stop = notRun, r.tick()
		}
	}
	r.mu.Unlock()
	for _, f := range failures {
		r.violate(ctx, "%v", f)
	}
	r.mu.Lock()
	if failedOne != nil {
		return failedOne
	}
	return as[len(as)-1]
}

// newAttempt of a statement of the text src, with the arguments args, as a query if query is set,
// on n, whose engine has the generation gen, in the transaction tx that spans statements, or on its
// own if tx is nil: it records the attempt, and gives it, with the comment of its ID in its text, as
// [withID] puts it there. The caller holds r.mu.
func (r *run) newAttempt(n *node, gen int, src string, args []*s4qlpb.Value, query bool, tx *txRecord) *attempt {
	r.nextID++
	a := &attempt{id: r.nextID, node: n.name, query: query, committed: len(r.committed), args: args, tx: tx, gen: gen, start: r.tick()}
	if tx != nil {
		// A statement of a transaction must see what was committed before the transaction began.
		a.committed, a.newer = tx.committed, len(r.committed) > tx.committed
		tx.stmts = append(tx.stmts, a)
	}
	a.src = withID(src, a.id)
	if s, _ := stmt.Check(a.src); s != nil {
		_, isSelect := s.(*stmt.Select)
		a.write = !isSelect
	}
	r.attempts[a.id] = a
	r.order = append(r.order, a)
	return a
}

// classify the attempt a of the call, which ended with the result res, the rows got of a query, and
// err, by what its caller was told, and check what it was told against the rules of its arguments
// and of its refusals. The offsets in the error of a statement of a text of several statements are in
// the whole text, and base is where the text of a starts in it, so that they are in the text of a
// alone. The caller holds r.mu.
func (r *run) classify(ctx context.Context, a *attempt, call stmttest.Call, res engine.Result, got answer, err error, base int) {
	tx := a.tx
	var se *sqlerr.Error
	switch {
	case err == nil:
		a.state, a.answer = succeeded, got
		if s, _ := stmt.Check(a.src); res.SetsChanges && isChange(s) {
			a.changes, a.unchanged = res.Changes, res.Changes == 0
		}
		// A write of a transaction is committed with its transaction, and a statement that did nothing wrote
		// nothing.
		if a.write && !a.nothing && tx == nil {
			r.committed = append(r.committed, a.id)
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
	case errors.Is(err, expr.ErrEvaluation):
		// An evaluation error of arithmetic does nothing, and [run.arithmetic] checks it against SQLite's
		// result of its operator on the state that the statement saw.
		a.state, a.answer = skipped, answer{err: evaluationError, message: err.Error()}
		a.answer.eval, _ = errors.AsType[*expr.Error](err)
		a.answer = unshifted(a.answer, base)
	case errors.Is(err, stmt.ErrNotSupported), errors.Is(err, engine.ErrNoStatement), errors.Is(err, engine.ErrArguments):
		a.state, a.answer = skipped, unshifted(answer{err: "not supported", message: err.Error()}, base)
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
		if kind == "" && supported && a.answer.err == "not supported" && (typeRefusal(a.src, a.answer.message) || strings.Contains(a.answer.message, ", for LIMIT, which takes ") || subqueryRefused(s, a.answer.message) ||
			s != nil && expressed(s) && stmttest.ArithmeticRefusal(a.answer.message)) {
			// The refusal of a value of another type than its column, after the insert or the select
			// found its table and its columns, or of an argument of LIMIT, of slice 13, after those, or of
			// a scalar subquery, of slice 15, as [subqueryRefused] says, which [run.verify] checks against
			// SQLite on the state that it saw, by [typeRule], or of the types of arithmetic, which it
			// checks by [run.arithmetic].
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
}

// unshifted answer a of a statement whose text starts at the offset base of a text of several
// statements: with the offset that the message of s4ql's own error says, and the span of an
// evaluation error, in the text of the statement alone, as [shifted] moves them the other way.
func unshifted(a answer, base int) answer {
	if base == 0 {
		return a
	}
	if m := atOffset.FindStringSubmatchIndex(a.message); m != nil {
		n, err := strconv.Atoi(a.message[m[2]:m[3]])
		if err != nil {
			panic(err)
		}
		a.message = a.message[:m[2]] + fmt.Sprint(n-base) + a.message[m[3]:]
	}
	if a.eval != nil {
		e := *a.eval
		e.Span.Start -= base
		e.Span.End -= base
		a.eval = &e
	}
	return a
}

// subqueryKinds of the attempt a of the statement s with scalar subqueries, of slice 15, a select,
// an update, or a delete, which kind names in the plural, for [tallyOf]: the statement, by its kind
// and how it ended, of its own or in a transaction, whether the engine computed its subqueries, with
// two of them, with RETURNING that gave rows, by the rows that it changed, and by the refusal of
// s4ql.
func subqueryKinds(a *attempt, s stmt.Statement, kind string) []string {
	kinds := []string{"statements with subqueries"}
	in := map[bool]string{true: " of their own", false: " in a transaction"}[a.tx == nil]
	switch a.state {
	case succeeded:
		kinds = append(kinds, kind+" with subqueries that succeeded", kind+" with subqueries that succeeded"+in)
		if a.subqueries == nil {
			kinds = append(kinds, "statements with subqueries that succeeded, whose subqueries the engine did not compute")
		} else {
			kinds = append(kinds, kind+" with subqueries that succeeded, whose subqueries the engine computed"+in)
		}
		if len(subqueriesIn(s)) > 1 {
			kinds = append(kinds, "statements with two subqueries or more that succeeded")
		}
		if hasReturning(a.src) && a.query && len(a.answer.rows) > 0 {
			kinds = append(kinds, kind+" with subqueries and RETURNING that gave rows")
		}
		if isChange(s) && !a.unchanged {
			kinds = append(kinds, kind+" with subqueries that changed rows"+in)
		}
		if a.query && len(a.answer.rows) > 0 {
			kinds = append(kinds, kind+" with subqueries that gave rows")
		}
		if a.tx != nil && a.tx.readOnly && a.subqueries != nil && a.newer {
			// A write was committed after the begin of the read-only transaction and before the
			// statement, so that the newest state is not the one that its subqueries must read.
			kinds = append(kinds, kind+" with subqueries that the engine computed in a read-only transaction after a write was committed")
			if a.query && len(a.answer.rows) > 0 {
				kinds = append(kinds, kind+" with subqueries that gave rows in a read-only transaction after a write was committed")
			}
		}
	case sqliteFailed:
		kinds = append(kinds, kind+" with subqueries with SQLite's error")
	case skipped:
		switch m := a.answer.message; {
		case strings.Contains(m, ": correlated subquery "):
			kinds = append(kinds, "statements with a correlated subquery, refused")
		case strings.Contains(m, ": subquery ") && strings.Contains(m, " compared with the column "):
			kinds = append(kinds, "statements with a subquery of another type than its column, refused")
		case strings.Contains(m, ", for LIMIT, which takes "):
			kinds = append(kinds, "statements with subqueries and an argument of LIMIT that s4ql refuses")
		default:
			kinds = append(kinds, "statements with subqueries refused otherwise")
		}
	case unknown:
		kinds = append(kinds, "statements with subqueries whose outcome is unknown")
	case failed:
		kinds = append(kinds, "statements with subqueries that failed: "+a.reason)
	}
	return kinds
}

// arithmeticKinds of the attempt a of a statement with arithmetic, a select, an update, or a
// delete, which kind names in the plural, for [tallyOf]: the statement by how it ended, of its own or
// in a transaction, an evaluation error by its kind, a refusal of its types, and an update or a delete
// that changed rows.
func arithmeticKinds(a *attempt, kind string) []string {
	in := map[bool]string{true: " of their own", false: " in a transaction"}[a.tx == nil]
	kinds := []string{"statements with arithmetic"}
	switch {
	case a.state == succeeded:
		kinds = append(kinds, kind+" with arithmetic that succeeded"+in)
		if !a.unchanged && a.write {
			kinds = append(kinds, kind+" with arithmetic that changed rows"+in)
		}
	case a.answer.eval != nil:
		kinds = append(kinds, "statements with arithmetic that failed: "+a.answer.eval.Kind.Error()+in)
	case a.typeRefused:
		kinds = append(kinds, "statements with arithmetic refused for its types"+in)
	case a.state == sqliteFailed:
		kinds = append(kinds, "statements with arithmetic with SQLite's error")
	}
	return kinds
}

// arithmeticFinds are the violations by which [TestSimulation_plantedBugs] finds a planted bug of
// arithmetic that gives a wrong value or a wrong row, own, the bug's own, first: the rows that differ
// from SQLite's, and the states, the commits, the counts of changes, the rows of RETURNING, and the
// evaluation errors that no row justifies that follow from a write that changed rows wrongly.
func arithmeticFinds(own string) []string {
	finds := []string{own}
	for _, f := range []string{", and in SQLite", "which began at commit", "the replay of the log holds", "for its key after the statements of the commit", "for it after the statements of the commit", "fails in SQLite", "rows in SQLite, and its caller was told",
		"justifies it for no row", "gives the rows of RETURNING", "is in no commit, and changes"} {
		if f != own {
			finds = append(finds, f)
		}
	}
	return finds
}

// subqueryFinds are the violations by which [TestSimulation_plantedBugs] finds a planted bug of the
// scalar subqueries of slice 15, own, the bug's own, first: a value that is not among SQLite's
// candidates, and the answers, the counts of changes, and the states that follow from a wrong value.
func subqueryFinds(own string) []string {
	finds := []string{own}
	for _, f := range []string{"gives the subquery", ", and in SQLite", "rows in SQLite, and its caller was told", "the replay of the log holds", "which began at commit",
		"gives the rows of RETURNING", "fails in SQLite", "is in no commit, and changes"} {
		if f != own {
			finds = append(finds, f)
		}
	}
	return finds
}

// subqueryRefused reports whether message is s4ql's refusal of the statement s with a scalar
// subquery, of slice 15, that the rule of [subqueryRule] gives by the state of the database beside
// the refusals of a type: of a correlated subquery, and of a second subquery of * of more than one
// column in an update or a delete.
func subqueryRefused(s stmt.Statement, message string) bool {
	return s != nil && subqueried(s) && (strings.Contains(message, ": correlated subquery (at offset ") || strings.Contains(message, ": second subquery of * of more than one column (at offset "))
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

// hasValueFirst reports whether s is a select, an update, or a delete with a comparison whose value
// is on the left of its column, of slice 12.
func hasValueFirst(s stmt.Statement) bool {
	var where []stmt.Condition
	switch s := s.(type) {
	case *stmt.Select:
		where = s.Where
	case *stmt.Update:
		where = s.Where
	case *stmt.Delete:
		where = s.Where
	}
	return slices.ContainsFunc(where, func(c stmt.Condition) bool { return c.ValueFirst })
}

// hasReturning reports whether the text src is an insert, an update, or a delete with RETURNING, of
// slice 14.
func hasReturning(src string) bool {
	switch s, _ := stmt.Check(src); s := s.(type) {
	case *stmt.Insert:
		return s.Returning.Present()
	case *stmt.Update:
		return s.Returning.Present()
	case *stmt.Delete:
		return s.Returning.Present()
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
	tx := &txRecord{id: r.nextTx, node: n.name, gen: gen, readOnly: !idle && rnd.IntN(4) == 0, committed: len(r.committed), start: r.tick()}
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
			r.committed = append(r.committed, a.id)
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
	return [...]string{"pending", "succeeded", "SQLite's error", "failed", "unknown", "skipped", "not run"}[s]
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
				if txs := r.q.Profile().Transactions; txs > 0 && rnd.IntN(txs) == 0 {
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
// empty cache. The crashed database stays open until the end of the run, when [simulate] closes its
// files, as the operating system would. The address sanitizer runs no crashes.
func (r *run) crash(ctx context.Context, i int) []host.Task {
	// The victim is a node that runs a statement, if one does within ten seconds, so that a crash
	// often cuts a statement. In the profile of several statements, it is first a node that runs a
	// text of several statements after a statement of it succeeded, if one does within ten seconds,
	// so that a crash often cuts a text between its statements or in a later one.
	var live []*node
	for range 200 {
		if r.q.Profile() != stmttest.Several {
			break
		}
		r.mu.Lock()
		for _, n := range r.nodes {
			if !n.inst.Crashed() && n.between.Load() > 0 {
				live = append(live, n)
			}
		}
		r.mu.Unlock()
		if len(live) > 0 {
			break
		}
		_ = r.world.Sleep(ctx, 50*time.Millisecond)
	}
	aimed := len(live) > 0
	for range 200 {
		if aimed {
			break
		}
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
		store: simstore.New(world, simstore.Options{Latency: 50 * time.Millisecond, Faults: w.faults}), q: newSequenceFrom(seed)}
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

// logCommit is a commit object of the log: its name, its changes, and the texts of its annotations.
type logCommit struct {
	name    format.Name
	changes []*s4qlpb.Change
	notes   []string
}

// noteOf the annotation of the log whose text is note: the attempt of the statement whose ID the text
// carries, and the arguments that its caller gave, which the annotation does not hold; or nil and no
// arguments for a text that carries the ID of no attempt. Each attempt has an ID of its own, which
// the run wrote into the text of its statement, so a note that is the own text of an attempt leads
// to the one call of its statement.
func (r *run) noteOf(note string) (*attempt, []*s4qlpb.Value) {
	m := idPattern.FindStringSubmatch(note)
	if m == nil {
		return nil, nil
	}
	id, _ := strconv.ParseUint(m[1], 10, 64)
	a := r.attempts[id]
	if a == nil {
		return nil, nil
	}
	return a, a.args
}

// ownText of the statement of the text src, from its first token to its end, which is the text of its
// annotation, or src if [stmt.Check] refuses it.
func ownText(src string) string {
	s, err := stmt.Check(src)
	if err != nil || s == nil {
		return src
	}
	span := s.Bounds()
	return src[span.Start:span.End]
}

// commitRows checks the changes of the commit c of the log against SQLite, which has run the
// statements of c with the arguments that their callers gave: each row that c upserts is the row of
// its key there, and each key that c deletes has no row there. A table that a later change of c
// creates or drops is left out, since SQLite holds it as that change leaves it. So a statement that
// ran with other arguments than its caller gave is found at its commit, also where a later commit
// changes the row again. defs holds the tables of the log before c, and gets those after it. It gives
// what differs.
func commitRows(t testing.TB, o *oracle, c logCommit, defs map[string]*s4qlpb.CreateTable) []string {
	t.Helper()
	var failures []string
	for i, ch := range c.changes {
		var table string
		var rows [][]*s4qlpb.Value
		switch {
		case ch.HasCreateTable():
			defs[fold(ch.GetCreateTable().GetTable())] = ch.GetCreateTable()
			continue
		case ch.HasDropTable():
			delete(defs, fold(ch.GetDropTable().GetTable()))
			continue
		case ch.HasUpsertRows():
			table = ch.GetUpsertRows().GetTable()
			for _, row := range ch.GetUpsertRows().GetRows() {
				rows = append(rows, row.GetValues())
			}
		case ch.HasDeleteRows():
			table = ch.GetDeleteRows().GetTable()
		default:
			continue
		}
		def := defs[fold(table)]
		later := slices.ContainsFunc(c.changes[i+1:], func(l *s4qlpb.Change) bool {
			return l.HasCreateTable() && fold(l.GetCreateTable().GetTable()) == fold(table) ||
				l.HasDropTable() && fold(l.GetDropTable().GetTable()) == fold(table)
		})
		if def == nil || later {
			// [run.verifyRows] reports an upsert of a table that the log does not have.
			continue
		}
		var where []string
		for _, p := range def.GetPrimaryKey() {
			where = append(where, stmttest.Quote(def.GetColumns()[p].GetName())+" = ?")
		}
		read := "select * from " + stmttest.Quote(table) + " where " + strings.Join(where, " and ")
		for _, values := range rows {
			if len(values) != len(def.GetColumns()) {
				// [run.verifyRows] reports a row that does not hold every column.
				continue
			}
			var key []*s4qlpb.Value
			for _, p := range def.GetPrimaryKey() {
				key = append(key, values[p])
			}
			var texts []string
			for _, v := range values {
				text, _ := valueString(v)
				texts = append(texts, text)
			}
			if got := o.run(t, read, oracleValues(key)...); got.err != "" || len(got.rows) != 1 || got.rows[0] != strings.Join(texts, ",") {
				failures = append(failures, fmt.Sprintf("it upserts the row %v of the table %v, and SQLite holds %v for its key after the statements of the commit", strings.Join(texts, ","), table, got))
			}
		}
		for _, key := range ch.GetDeleteRows().GetPrimaryKeys() {
			if got := o.run(t, read, oracleValues(key.GetValues())...); got.err != "" || len(got.rows) != 0 {
				failures = append(failures, fmt.Sprintf("it deletes the key %v of the table %v, and SQLite holds %v for it after the statements of the commit", valuesText(key.GetValues()), table, got))
			}
		}
	}
	return failures
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
//     twice. The annotation of each statement is its own text, from its first token to its end.
//     It holds no arguments, so those that its caller gave come from the attempt whose ID the text
//     carries, as [run.noteOf] says.
//   - Every write that was committed is in the log: a statement of its own when it succeeded,
//     and the writes of a transaction when its commit succeeded.
//   - The statements of the log, run in SQLite in their order with the arguments that their callers
//     gave, each succeed, the value of each insert has the type of its column there, which SQLite
//     would convert or refuse otherwise, as [typeRule] says, and after the statements of a commit
//     SQLite holds each row that the commit upserts and none that it deletes, as [commitRows] says.
//   - Each insert of its own that the engine refused for the type of its value is refused so by
//     [typeRule] in SQLite on the state that it saw.
//   - Each statement of its own that failed with SQLite's error gives that error in SQLite on the
//     state that it saw, and each query that succeeded gives its rows there: the state after a
//     commit of the log, which holds every write that was committed before the statement
//     started.
//   - Each statement of a transaction that spans statements gives in SQLite what it gave, on the
//     state that the transaction began from, which holds every write that was committed before
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
			if want := ownText(a.src); c.notes[i] != want {
				r.violate(ctx, "commit %v holds the text %q of the statement %v, whose own text is %q", k+1, c.notes[i], id, want)
			}
			inLog[id] = k + 1
		}
	}
	r.verifyTexts(ctx, inLog)
	// The observations of each state of the log: the statements of their own that saw it.
	seen := map[int][]*attempt{}
	// refused are the inserts and the selects of their own that the engine refused for the type of a
	// value, by the state of the log that each saw.
	refused := map[int][]*attempt{}
	empty := newOracle(r.t)
	defer empty.close()
	for _, a := range r.order {
		if a.state == skipped && (a.answer.err == "not supported" || a.answer.err == evaluationError) {
			if m := syntaxOfNotSupported(r.t, empty, a.src); a.answer.err == "not supported" && m != "" {
				r.violate(ctx, "the engine does not support the statement %v, %q, and SQLite gives %v", a.id, a.src, m)
			}
			if (a.typeRefused || a.answer.err == evaluationError) && a.tx == nil {
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
		for _, id := range r.committed[:a.committed] {
			if inLog[id] > at {
				r.violate(ctx, "the statement %v saw commit %v, and the statement %v, in commit %v, was committed before it started", a.id, at, id, inLog[id])
			}
		}
		seen[at] = append(seen[at], a)
	}
	o := newOracle(r.t)
	// defs are the tables of the log before the commit that SQLite runs, for [commitRows].
	defs := map[string]*s4qlpb.CreateTable{}
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
			src, args, failure := r.flattened(o, a, a.src, oracleValues(a.args), "of their own")
			if failure != "" {
				r.violate(ctx, "the statement %v at commit %v: %v", a.id, k, failure)
			}
			want := o.run(r.t, src, args...)
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
			if !a.query || a.partial {
				// A write through Query whose rows were not read to their end, as one with RETURNING, is
				// compared by its error alone.
				want.columns, want.rows = "", nil
			}
			if got := a.answer.String(); differs(r.t, o, a, src, args, want) {
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
			// The values that the engine computed decide the order of the rule, as whether an update
			// changes a row before the types of its SET, so each must be among SQLite's candidates first.
			if _, _, failure := r.flattened(o, a, a.src, oracleValues(a.args), "refused of their own"); failure != "" {
				r.violate(ctx, "the statement %v at commit %v: %v", a.id, k, failure)
			}
			if s, _ := stmt.Check(a.src); s != nil && expressed(s) {
				if failure := r.arithmetic(o, a, false); failure != "" {
					r.violate(ctx, "the statement %v, %q, at commit %v: %v", a.id, a.src, k, failure)
				}
				continue
			}
			if want := typeRule(r.t, o, a.src, oracleValues(a.args), r.computed(a)...); a.answer.message != want {
				r.violate(ctx, "the statement %v, %q, at commit %v gives %q, and the rule for the type of a value %q", a.id, a.src, k, a.answer.message, want)
			}
		}
		if k < len(log) {
			for _, note := range log[k].notes {
				// The value of an insert of the log has the type of its column, which SQLite would
				// convert or refuse otherwise.
				of, given := r.noteOf(note)
				if want := typeRule(r.t, o, note, oracleValues(given)); want != "" {
					r.violate(ctx, "commit %v, %q, stores a value of another type than its column: %v", k+1, note, want)
				}
				if s, _ := stmt.Check(note); s != nil && expressed(s) {
					if refusals := arithmeticRefusals(r.t, o, s, oracleValues(given), false); len(refusals) > 0 {
						r.violate(ctx, "commit %v, %q, has arithmetic that the rules of its types refuse: %q", k+1, note, refusals)
					}
					r.event("writes with arithmetic in the log")
				}
				src, args, failure := r.flattened(o, of, note, oracleValues(given), "of the log")
				if failure != "" {
					r.violate(ctx, "commit %v: %v", k+1, failure)
				}
				ran := o.run(r.t, src, args...)
				if ran.err != "" {
					r.violate(ctx, "commit %v, %q, fails in SQLite: %v", k+1, note, ran.err)
				}
				// The count of changes of an update or a delete of its own, which its caller was given.
				changes, _ := o.changes(r.t)
				if of != nil {
					if a := of; a.tx == nil && a.state == succeeded && a.query && !a.partial && hasReturning(a.src) {
						// The rows of RETURNING that the caller of a statement of its own read are those of the
						// change that the log holds, as SQLite gives them on the state before it.
						if a.answer.String() != ran.String() {
							r.violate(ctx, "commit %v, %q, gives the rows of RETURNING %v in SQLite, and its caller read %v", k+1, note, ran, a.answer)
						}
						r.event("statements with RETURNING of their own whose rows are compared with the log")
					}
					if a := of; a.tx == nil && a.state == succeeded && a.write {
						if s, _ := stmt.Check(a.src); isChange(s) {
							if a.changes != changes {
								r.violate(ctx, "commit %v, %q, changes %v rows in SQLite, and its caller was told %v", k+1, note, changes, a.changes)
							}
							r.event("updates and deletes of their own whose count of changes is compared with SQLite")
						}
					}
				}
			}
			for _, failure := range commitRows(r.t, o, log[k], defs) {
				r.violate(ctx, "commit %v: %v", k+1, failure)
			}
		}
	}
	nothing := r.verifyTxs(ctx, log, index, inLog)
	for _, id := range r.committed {
		// An update or a delete that found no row changes nothing, and is in the log only if the
		// transaction that ran it changed something else.
		if inLog[id] == 0 && !nothing[id] && !r.attempts[id].unchanged {
			r.violate(ctx, "the committed statement %v, %q, is not in the log", id, r.attempts[id].src)
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

// verifyTexts checks the statements of each text of several statements outside a transaction in the
// log, whose commit of each statement inLog gives: they are there as running them one after another
// leaves them, each in a commit of its own, in their order, and a statement is there only if every
// statement before it in the text succeeded, so that a crash or a failure between two statements
// leaves the earlier ones alone. [run.verify] checks that each statement that was committed is
// there, and that one that failed or did not run is not. Those of a transaction are in its commit
// object, which it checks as it checks every transaction.
func (r *run) verifyTexts(ctx context.Context, inLog map[uint64]int) {
	for _, text := range r.texts {
		if text.tx != nil {
			continue
		}
		last, of := 0, uint64(0)
		for j, a := range text.attempts {
			at := inLog[a.id]
			if at == 0 {
				continue
			}
			if k := slices.IndexFunc(text.attempts[:j], func(b *attempt) bool { return b.state != succeeded }); k >= 0 {
				r.violate(ctx, "the statement %v of a text of several statements is in commit %v, and the statement %v before it in the text ended as %v", a.id, at, text.attempts[k].id, text.attempts[k].state)
			}
			if at <= last {
				r.violate(ctx, "the statement %v of a text of several statements is in commit %v, and the statement %v before it in the text in commit %v", a.id, at, of, last)
			}
			last, of = at, a.id
		}
	}
}

// verifyTxs checks what each statement of a transaction that spans statements gave. The statements
// of a transaction must all have seen one state, the one that it began from, which must be a state
// of the log that holds every write that was committed before the begin. SQLite then runs the
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
// transactions, which are committed and need not be in the log.
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
		for _, id := range r.committed[:tx.committed] {
			if inLog[id] > at {
				r.violate(ctx, "the transaction %v saw commit %v, and the statement %v, in commit %v, was committed before it began", tx.id, at, id, inLog[id])
			}
		}
		o := newOracle(r.t)
		for _, c := range log[:at] {
			for _, note := range c.notes {
				// [run.verify] checks the values of the subqueries of the log on the same states.
				of, given := r.noteOf(note)
				src, args, _ := r.flattened(o, of, note, oracleValues(given), "")
				o.run(r.t, src, args...)
			}
		}
		if tx.readOnly {
			o.run(r.t, "pragma query_only = 1")
		}
		before, beforeState := o.tables(r.t), o.state(r.t)
		for _, a := range tx.stmts {
			if a.typeRefused || a.answer.err == evaluationError {
				// A read-only transaction refuses an insert before it looks at its value, and runs a
				// select. The values that the engine computed must be among SQLite's candidates, as for
				// a statement of its own that was refused.
				if _, _, failure := r.flattened(o, a, a.src, oracleValues(a.args), "refused in a transaction"); failure != "" {
					r.violate(ctx, "the statement %v of the transaction %v (%v), which began at commit %v: %v", a.id, tx.id, kind, at, failure)
				}
				if s, _ := stmt.Check(a.src); s != nil && expressed(s) {
					// A statement of arithmetic that was refused or failed did nothing, and the transaction
					// goes on with the state before it.
					if failure := r.arithmetic(o, a, tx.readOnly); failure != "" {
						r.violate(ctx, "the statement %v, %q, of the transaction %v (%v), which began at commit %v: %v", a.id, a.src, tx.id, kind, at, failure)
					}
					r.event("statements with arithmetic refused or failed in a transaction, checked by the rules")
					continue
				}
				want := typeRule(r.t, o, a.src, oracleValues(a.args), r.computed(a)...)
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
			if want := typeRule(r.t, o, a.src, oracleValues(a.args), r.computed(a)...); a.state == succeeded && want != "" {
				// The check of the log does not see an insert of a transaction that did not commit.
				r.violate(ctx, "the statement %v, %q, of the transaction %v (%v) succeeded, and stores a value of another type than its column: %v", a.id, a.src, tx.id, kind, want)
			}
			if st, _ := stmt.Check(a.src); a.state == succeeded && st != nil && expressed(st) {
				if refusals := arithmeticRefusals(r.t, o, st, oracleValues(a.args), tx.readOnly); len(refusals) > 0 {
					r.violate(ctx, "the statement %v, %q, of the transaction %v (%v) succeeded, and the rules of the types of arithmetic refuse it: %q", a.id, a.src, tx.id, kind, refusals)
				}
			}
			var stateBefore string
			if a.nothing {
				stateBefore = o.state(r.t)
			}
			src, args, failure := r.flattened(o, a, a.src, oracleValues(a.args), "in a transaction")
			if failure != "" {
				r.violate(ctx, "the statement %v of the transaction %v (%v), which began at commit %v: %v", a.id, tx.id, kind, at, failure)
			}
			want := o.run(r.t, src, args...)
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
			if got := a.answer.String(); differs(r.t, o, a, src, args, want) {
				r.violate(ctx, "the statement %v, %q, of the transaction %v (%v), which began at commit %v, gives %v, and in SQLite %v", a.id, a.src, tx.id, kind, at, got, want)
			}
			if a.query && a.state == succeeded && hasReturning(a.src) {
				r.event("statements with RETURNING in a transaction whose rows are compared with SQLite")
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

// arithmetic checks the attempt a of a statement with arithmetic, which s4ql refused for its types or
// which failed with an evaluation error, on the database of o, the state that it saw, in a read-only
// transaction if readOnly, against s4ql's rules for arithmetic: a refusal must be one of
// [arithmeticRefusals], and an evaluation error must be justified by SQLite's own result of its
// operator, as [stmttest.Justify] says, with the values that the engine computed for its
// subqueries, and never of an update or a delete in a read-only transaction, which computes nothing.
// It gives what is wrong, or "".
func (r *run) arithmetic(o *oracle, a *attempt, readOnly bool) string {
	s, _ := stmt.Check(a.src)
	args := oracleValues(a.args)
	if s == nil || len(args) != s.Parameters() {
		return fmt.Sprintf("gives %q, and has another number of arguments than parameters", a.answer.message)
	}
	if err := o.prepare(r.t, a.src); err != nil {
		// An update or a delete with two subqueries of `*` of more than one column, which SQLite does not
		// prepare, is refused by the rule of [subqueryRule], whatever arithmetic it has.
		if want := subqueryRule(r.t, o, a.src, s, args, readOnly, r.computed(a)); want != "" && a.answer.message == want {
			return ""
		}
		return fmt.Sprintf("gives %q, and SQLite gives %v", a.answer.message, sqliteText(r.t, err))
	}
	if a.answer.err != evaluationError {
		if refusals := arithmeticRefusals(r.t, o, s, args, readOnly); !slices.Contains(refusals, a.answer.message) {
			return fmt.Sprintf("gives %q, and the rules of the types of arithmetic %q", a.answer.message, refusals)
		}
		r.event("statements with arithmetic refused by the rules of its types")
		return ""
	}
	ee := a.answer.eval
	if ee == nil {
		return fmt.Sprintf("gives %q, which is no evaluation error of the evaluator", a.answer.message)
	}
	if _, isSelect := s.(*stmt.Select); readOnly && !isSelect {
		return fmt.Sprintf("gives %q in a read-only transaction, which computes nothing for a write", a.answer.message)
	}
	var computed []stmt.Term
	for _, v := range r.computed(a) {
		computed = append(computed, valueTerm(v))
	}
	rows := func(src string, args []any) bool {
		got := o.run(r.t, src, args...)
		if got.err != "" {
			r.t.Fatalf("%q: SQLite gives %v", src, got.err)
		}
		return len(got.rows) > 0
	}
	if why := stmttest.Justify(s, args, ee.Kind, ee.Op, ee.Span.Start, computed, rows); why != "" {
		return fmt.Sprintf("gives %q, which %v", a.answer.message, why)
	}
	r.event("evaluation errors that SQLite's result of their operator justifies")
	return ""
}

// computed are the values that the engine computed for the subqueries of the attempt a, as its hook
// recorded them, or nil.
func (r *run) computed(a *attempt) []*s4qlpb.Value {
	r.mu.Lock()
	defer r.mu.Unlock()
	return a.subqueries
}

// flattened gives the text and the arguments that SQLite runs on the database of o in place of the
// text src, with the arguments args as SQLite binds them, of the statement of the attempt a, or of a
// note of the log of it, and "" or a violation. Where the engine computed the scalar subqueries of
// the statement, as [attempt.subqueries] holds them, each value must be one that a legal order puts
// first on the database of o, as [subqueryCall.check] says, and SQLite runs the text with each
// subquery replaced by a parameter bound to that value, as [subqueryCall.flatten] gives it, so that
// SQLite takes the row that the engine took where the ORDER BY of a subquery leaves several. A
// statement whose subqueries the engine did not compute, and one of no attempt, runs as it is. What
// the values reach is counted as events after where, and not for an empty where, as for the log that
// a transaction began from, whose values [run.verify] counts.
func (r *run) flattened(o *oracle, a *attempt, src string, args []any, where string) (string, []any, string) {
	if a == nil {
		return src, args, ""
	}
	r.mu.Lock()
	computed := a.subqueries
	r.mu.Unlock()
	if computed == nil {
		return src, args, ""
	}
	s, err := stmt.Check(src)
	if err != nil || s == nil || !subqueried(s) || len(args) != s.Parameters() {
		return src, args, fmt.Sprintf("the engine computed %v subqueries of %q, with %v arguments, whose subqueries SQLite does not run", len(computed), src, len(args))
	}
	sc := &subqueryCall{src: src, s: s, subs: subqueriesIn(s), args: args}
	for _, q := range sc.subs {
		candidates, first := subqueryCandidates(r.t, o, src, q, args)
		sc.candidates, sc.first = append(sc.candidates, candidates), append(sc.first, first)
	}
	count := func(string) {}
	if where != "" {
		count = func(kind string) { r.event("subqueries " + where + ": " + kind) }
		r.event("statements with subqueries " + where + " whose values are checked against SQLite's")
	}
	if failure := sc.check(r.t, fmt.Sprint("the statement ", a.id), computed, count); failure != "" {
		return src, args, failure
	}
	flat, bound := sc.flatten(computed)
	return flat, bound, ""
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
		// The operating system would have closed the files of a crashed process. Nothing reads them
		// after the run: the checks of a crashed cache are at its crash.
		if s.r != nil {
			for _, d := range s.r.crashed {
				d.CloseCache()
			}
		}
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

// simSeeds is n, or the number in the environment variable S4QL_ENGINE_SIM_SEEDS, which is 1 at
// least.
func simSeeds(t *testing.T, n int) uint64 {
	t.Helper()
	if v := os.Getenv("S4QL_ENGINE_SIM_SEEDS"); v != "" {
		var err error
		n, err = strconv.Atoi(v)
		is.NotError(t, err)
		is.True(t, n >= 1, "S4QL_ENGINE_SIM_SEEDS is a number of seeds, 1 at least")
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

// tallyOf the simulation s: its statements, by how they ended, its writes that were committed,
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
		if sel, ok := s.(*stmt.Select); ok && (sel.OrderBy != nil || sel.Limited()) {
			what := map[bool]string{true: "selects with ORDER BY", false: "selects with LIMIT and without ORDER BY"}[sel.OrderBy != nil]
			switch {
			case a.state == succeeded && a.query && len(a.answer.rows) > 1:
				counts[what+" that gave rows"]++
			case a.state == sqliteFailed:
				counts[what+" with SQLite's error"]++
			}
			if sel.Limited() && a.state == succeeded {
				counts["selects with LIMIT that succeeded"]++
			}
		}
		if hasReturning(a.src) {
			counts["statements with RETURNING"]++
			switch {
			case a.state == succeeded && a.query && len(a.answer.rows) > 0 && a.tx == nil:
				counts["statements with RETURNING of their own that gave rows"]++
			case a.state == succeeded && a.query && len(a.answer.rows) > 0 && a.tx.end == "rolled back":
				counts["statements with RETURNING that gave rows in a transaction that was rolled back"]++
			case a.state == succeeded && a.query && len(a.answer.rows) > 0:
				counts["statements with RETURNING that gave rows in a transaction"]++
			case a.state == sqliteFailed:
				counts["statements with RETURNING with SQLite's error"]++
			case a.state == failed && a.query && a.tx == nil && a.position != "":
				// It failed after its write transaction began, with an error of the core or of its context,
				// which is mostly that of its commit.
				counts["statements with RETURNING of their own through Query that failed in their write transaction"]++
			case a.state == unknown && a.query:
				counts["statements with RETURNING of their own through Query whose outcome is unknown"]++
			}
		}
		if s != nil && subqueried(s) {
			for _, what := range subqueryKinds(a, s, kind) {
				counts[what]++
			}
		}
		if s != nil && expressed(s) {
			for _, what := range arithmeticKinds(a, kind) {
				counts[what]++
			}
		}
		if hasValueFirst(s) {
			counts["statements with a value on the left"]++
			switch {
			case a.state == succeeded && (isChange(s) && !a.unchanged || a.query && len(a.answer.rows) > 0):
				counts["statements with a value on the left that found rows"]++
			case a.state == sqliteFailed:
				counts["statements with a value on the left with SQLite's error"]++
			}
			if a.access == "primary_key" || a.access == "index" {
				counts["statements with a value on the left read by a key or an index"]++
			}
		}
	}
	counts["indexes dropped while the rows of a query that read by them were open"] = r.droppedUnder()
	counts["committed writes"] = len(r.committed)
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
		if slices.ContainsFunc(c.notes, func(note string) bool { _, given := r.noteOf(note); return len(given) > 0 }) {
			counts["commits with a parameter"]++
		}
		if len(c.notes) > 1 {
			counts["commits of more than one statement"]++
		}
	}
	for what, n := range severalKinds(r, log) {
		counts[what] += n
	}
	return counts
}

// severalKinds of the texts of several statements of the run r, whose log is log, for [tallyOf]: the
// texts, by how they ended, outside a transaction and in one; those refused before any statement
// ran; those whose call did not return, because their instance crashed, after a statement of them
// succeeded; and those outside a transaction with a commit of one statement and of two or more. The
// statements that did not run after one that failed are the state "not run".
func severalKinds(r *run, log []logCommit) map[string]int {
	counts := map[string]int{}
	inLog := map[uint64]int{}
	for k, c := range log {
		for _, note := range c.notes {
			if m := idPattern.FindStringSubmatch(note); m != nil {
				id, _ := strconv.ParseUint(m[1], 10, 64)
				inLog[id] = k + 1
			}
		}
	}
	for _, text := range r.texts {
		where := map[bool]string{false: " of their own", true: " in a transaction"}[text.tx != nil]
		counts["texts of several statements"+where]++
		ran := slices.ContainsFunc(text.attempts, func(a *attempt) bool { return a.state == succeeded })
		switch {
		case text.refused:
			counts["texts of several statements refused before any statement ran"]++
		case !text.returned && ran:
			counts["texts of several statements whose call did not return after a statement succeeded"]++
		case !text.returned:
		case text.err == nil:
			counts["texts of several statements that ran to their end"+where]++
		case ran:
			failed := text.attempts[min(max(text.failedAt, 0), len(text.attempts)-1)]
			counts["texts of several statements that failed after a statement succeeded"+where]++
			counts[fmt.Sprint("texts of several statements that failed after a statement succeeded, as ", failed.state)]++
		default:
			counts["texts of several statements that failed at their first statement"+where]++
		}
		commits := 0
		for _, a := range text.attempts {
			if inLog[a.id] > 0 {
				commits++
			}
		}
		switch {
		case text.tx != nil:
		case commits == 1:
			counts["texts of several statements with one commit"]++
		case commits > 1:
			counts["texts of several statements with a commit of each of two statements or more"]++
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

// simCell is a number of seeds of a profile that [TestSimulation] runs in an environment.
type simCell struct {
	profile *stmttest.Profile
	seeds   int
}

// differs reports whether the answer of the statement a of the simulation differs from want,
// SQLite's answer on the database of o to the text src of the statement with the arguments args, as
// [run.flattened] gives them: for a query of a select with ORDER BY or LIMIT that both answer with
// rows, as [ordered.compare] says, and otherwise by their texts, as [answer.String] gives them.
func differs(t testing.TB, o *oracle, a *attempt, src string, args []any, want answer) bool {
	t.Helper()
	if s, _ := stmt.Check(src); a.query && a.answer.err == "" && want.err == "" {
		if sel, ok := s.(*stmt.Select); ok && (sel.OrderBy != nil || sel.Limited()) {
			return orderedOf(sel, args).compare(t, o, a.answer, want) != ""
		}
	}
	return a.answer.String() != want.String()
}

// simEnvironment is an environment of [TestSimulation]: see [simEnvironments].
type simEnvironment struct {
	name    string
	w       workload
	first   uint64
	cells   []simCell
	noLease float64
}

// simEnvironments are the environments of [TestSimulation], each with the number of the first seed
// of each profile that it runs, so that no two environments, and neither [TestDifferential] nor the
// planted bugs, run the same sequences; its cells, each a profile and the number of its seeds; and
// the largest share of its statements that may be refused for want of the lease, about twice what
// its seeds gave on 2026-11-21, 0.034, 0.106, and 0.032. Each profile runs in the default and in the
// hammer environment, and the idle one, whose kinds are those of the lease, runs five. The cells of
// the mixed profile, the only one in which every area meets the others, hold 100 seeds, two thirds
// of the 150 of the one sequence of all areas before the profiles. A cell that is added to an
// environment adds to what the environment reaches, and takes nothing from another cell.
var simEnvironments = []simEnvironment{
	{"default", defaultWorkload, 100_000, []simCell{{stmttest.Mixed, 34}, {stmttest.Tables, 6}, {stmttest.Where, 6}, {stmttest.Changes, 8}, {stmttest.Indexes, 10},
		{stmttest.Exists, 6}, {stmttest.Defaults, 6}, {stmttest.ValueFirst, 6}, {stmttest.Transactions, 6}, {stmttest.Arguments, 6}, {stmttest.Order, 6}, {stmttest.Returning, 6}, {stmttest.Subquery, 8},
		{stmttest.Arithmetic, 8}, {stmttest.Several, 30}}, 0.07},
	{"idle", idleWorkload, 200_000, []simCell{{stmttest.Mixed, 33}, {stmttest.Changes, 8}, {stmttest.Indexes, 10}, {stmttest.Exists, 6}, {stmttest.Transactions, 12}}, 0.2},
	{"hammer", hammerWorkload, 300_000, []simCell{{stmttest.Mixed, 33}, {stmttest.Tables, 8}, {stmttest.Where, 10}, {stmttest.Changes, 6}, {stmttest.Indexes, 10},
		{stmttest.Exists, 6}, {stmttest.Defaults, 6}, {stmttest.ValueFirst, 10}, {stmttest.Transactions, 6}, {stmttest.Arguments, 6}, {stmttest.Order, 6}, {stmttest.Returning, 16}, {stmttest.Subquery, 20},
		{stmttest.Arithmetic, 12}, {stmttest.Several, 12}}, 0.06},
}

// TestSimulation runs each environment of [simEnvironments], the default, the idle, and the hammer
// workload, on seeds of the profiles of its cells, and the first seed of each cell twice, whose
// traces must be the same. What must be reached is in testdata/reached/TestSimulation.tsv, as
// [stmttest.Reached] says: by the seeds of each cell, and by those of each environment together,
// every kind of outcome, of fault of the store, and of event of
// testdata/reached/TestSimulation.kinds, which [tallyOf] counts, at least a third as often as when
// the file was written, so that the test fails when a kind stops happening; and the integrity checks
// of the caches exactly, since every seed runs each. S4QL_RECOUNT=1 writes the file again. The
// environments together must reach each kind at least as often as the number before it in
// TestSimulation.kinds, which is written by hand, so that a recount cannot lower a floor. And at
// most the share noLease of the statements of an environment may be refused for want of the lease,
// so that the test fails when most statements no longer reach a transaction. The seeds are the same
// in every run, so the counts are too.
//
// With S4QL_ENGINE_SIM_SEEDS, each cell runs that many seeds in place of its own, for breadth, and
// the test checks the least numbers alone, if each cell runs as many seeds as its own at least, and
// so those of the file among them. Under -short, the race detector, or the address sanitizer, a cell
// runs a tenth of its seeds, one at least, the first seed of three cells of each environment runs
// twice, and the test checks no count. With the flag -replay, it replays that one seed instead: see
// [replaySeed].
func TestSimulation(t *testing.T) {
	if *replay != "" {
		replaySeed(t)
		return
	}
	for _, p := range stmttest.Profiles {
		if !slices.ContainsFunc(simEnvironments, func(e simEnvironment) bool {
			return slices.ContainsFunc(e.cells, func(c simCell) bool { return c.profile == p })
		}) {
			t.Fatalf("no environment runs the profile %v", p.Name)
		}
	}
	runs := map[string]map[string]int{}
	var names []string
	// ran is the number of environments whose subtest ran, and more reports whether each cell ran as
	// many seeds as its own at least.
	ran, more := 0, true
	for _, c := range simEnvironments {
		counts := map[string]int{}
		runs[c.name], names = counts, append(names, c.name)
		for _, cell := range c.cells {
			name := c.name + "/" + cell.profile.Name
			runs[name], names = map[string]int{}, append(names, name)
		}
		t.Run(c.name, func(t *testing.T) {
			ran++
			seeds := 0
			for i, cell := range c.cells {
				of := runs[c.name+"/"+cell.profile.Name]
				n := simSeeds(t, cell.seeds)
				if slow() {
					n = max(n/10, 1)
				}
				more = more && n >= uint64(cell.seeds)
				for k := range n {
					seed := cell.profile.Seed(c.first + k)
					seeds++
					s := simulate(t, seed, c.w)
					if v := s.violations(); len(v) > 0 {
						t.Fatalf("seed %v: %v\nreplay it with: %v", seed, strings.Join(v, "\n"), replayCommand(seed, "-workload", c.name))
					}
					for kind, v := range tallyOf(s) {
						counts[kind] += v
						of[kind] += v
					}
					if k == 0 && (i < 3 || !slow()) {
						again := simulate(t, seed, c.w)
						is.True(t, slices.Equal(s.lines, again.lines), fmt.Sprint("seed ", seed, " gives two traces; replay it with: ", replayCommand(seed, "-workload", c.name)))
					}
				}
			}
			t.Logf("%v seeds: %v", seeds, counts)
			if slow() {
				return
			}
			if share := float64(counts["failed: no lease"]) / float64(counts["statements"]); share > c.noLease {
				t.Errorf("%v seeds refuse %v of %v statements for want of the lease, %.2f of them, and at most %v were expected: most statements must reach a transaction", seeds, counts["failed: no lease"], counts["statements"], share, c.noLease)
			}
		})
	}
	path := filepath.Join("testdata", "reached", "TestSimulation")
	reached := stmttest.Reached{Kinds: path + ".kinds", Counts: path + ".tsv", Doc: "What the seeds of each environment of TestSimulation of the engine, and of each of its cells, reach.",
		Exact: func(kind string) bool { return strings.HasPrefix(kind, "integrity checks ") },
		// The counts of an environment are the sums of those of its cells.
		Totaled: func(run string) bool { return !strings.Contains(run, "/") }}
	check := reached.Check
	switch {
	case t.Failed():
		return
	case slow() || ran < len(simEnvironments) || !more:
		t.Logf("NOT CHECKED: what the seeds reach, in a run of a part of the seeds")
		noRecount(t)
		return
	case os.Getenv("S4QL_ENGINE_SIM_SEEDS") != "":
		noRecount(t)
		check = reached.Least
	}
	failures, err := check(names, runs)
	is.NotError(t, err)
	for _, failure := range failures {
		t.Error(failure)
	}
}

// The flags of the test binary that replay one seed of [TestSimulation]: see [replaySeed].
var (
	replay         = flag.String("replay", "", "replay this one seed in TestSimulation, and nothing else, and log its trace")
	replayWorkload = flag.String("workload", "default", "workload of the replay: default, hammer, or idle")
	replayBug      = flag.String("bug", "", "planted bug of the replay")
)

// replaySeed runs the seed of the flag -replay, with the profile of the seed, of the workload of
// the flag -workload, with the bug of the flag -bug, and logs its trace. [replayCommand] gives the
// command.
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

// TestSimulation_plantedBugs runs each planted bug in its environment, on the seeds of its profile,
// until two seeds have found the bug by its own violation, and each seed that finds the bug again
// without it, which must find nothing. With S4QL_ENGINE_SIM_SEEDS, it runs that many seeds of the
// profile of each bug, and logs how many find it. With S4QL_TEST_SHARD set, it runs the bugs of that
// shard only: see [testshard].
func TestSimulation_plantedBugs(t *testing.T) {
	limit := simSeeds(t, 100)
	// The environment and the profile of each bug, and the violations that find it. The comment of a
	// bug says why its profile exercises what the bug breaks, and how many of the first 200 seeds of
	// the profile found the bug on 2026-11-21, with S4QL_ENGINE_SIM_SEEDS=200. Count them again when
	// the generator of the profile changes.
	bugs := []struct {
		name string
		w    workload
		// replay is the name of the workload for the flag -workload.
		replay string
		// profile of the seeds that the bug runs on: the one that weighs what the bug breaks.
		profile *stmttest.Profile
		// finds holds a part of each violation that the bug causes, its own violation first, which the
		// bug must cause in two seeds at least.
		finds []string
	}{
		// An insert that replaces a row makes a commit of the log fail in SQLite, and an insert of a
		// write transaction that replaces a row succeeds, where SQLite fails. The profile of the
		// tables weighs the inserts, whose keys repeat: 180 of 200 seeds find the bug, 178 of them by
		// its own violation.
		{"skipUnique", defaultWorkload, "default", stmttest.Tables, []string{"fails in SQLite: 19/1555: UNIQUE constraint failed", "(write), which began at commit"}},
		// A statement that succeeds while its commit did not land counts as committed, and is not in
		// the log. The profile of the tables weighs the inserts, and so the commits: 48 of 200 seeds find
		// the bug, each by its own violation, where 39 do on the mixed profile.
		{"ackUnknown", defaultWorkload, "default", stmttest.Tables, []string{"is not in the log"}},
		// The check of an insert in a read transaction of its own lets two inserts of one key both
		// commit, and the second commit fails in SQLite. That needs two inserts of one key on one
		// instance at once, which the hammer environment gives, and the profile of the tables weighs
		// the inserts: 79 of 200 seeds find the bug, each by its own violation.
		{"checkOutside", hammerWorkload, "hammer", stmttest.Tables, []string{"fails in SQLite: 19/1555: UNIQUE constraint failed"}},
		// A write transaction left open keeps the instance from a write, and from a close that
		// releases the lease, so the next instance never runs the queries of its connections. Any
		// write that fails leaves one open, and about half of the statements of the mixed profile meet
		// no table: 196 of 200 seeds find the bug, each by its own violation.
		{"keepOpen", defaultWorkload, "default", stmttest.Mixed, []string{"no instance runs a write", "a query on each connection of its callers ten minutes after the end of the workload"}},
		// A read transaction left open fails every later query of its connection, and the check of
		// the connections after the workload.
		// In a transaction, the rows of the query stay open, which refuses every later statement of the
		// connection. The profile of the selects with WHERE weighs the queries, some of which a caller
		// closes unread: 149 of 200 seeds find the bug, 78 of them by its own violation.
		{"keepRead", defaultWorkload, "default", stmttest.Where, []string{"a read transaction left open after the end of the workload", "the reader has a read transaction open", "the rows of an earlier query of the connection are open"}},
		// A statement that runs with other arguments than its caller gave writes other rows than SQLite
		// does with its caller's, which the annotation does not show, since it holds no arguments. Every
		// value of the profile of the arguments is a parameter: 97 of 200 seeds find the bug, 95 of them
		// by its own violation, a row of the commit that SQLite does not hold after the statements of the
		// commit.
		{"argsNext", defaultWorkload, "default", stmttest.Arguments, []string{"for its key after the statements of the commit", "for it after the statements of the commit", ", and in SQLite", "the replay of the log holds", "fails in SQLite", "rows in SQLite, and its caller was told", "is in no commit, and changes"}},
		// A failed statement that is kept is in the commit object of its transaction, whose replay in
		// SQLite then fails. What it changed before it failed is kept too: an update that changes the key
		// of a row that the transaction inserted, and fails with UNIQUE after the delete of the old key,
		// leaves the insert and the delete, whose net effect is nothing, so the commit object holds
		// neither, and the insert that its caller was told of is not in the log. In the profile of the
		// transactions, one turn of a caller in two is a transaction: 198 of 200 seeds find the bug,
		// each by its own violation. What the failed statement changed also shows to the later
		// statements of its transaction, which then give other rows than SQLite's, as in seed 20456.
		{"keepFailed", defaultWorkload, "default", stmttest.Transactions, []string{"whose statements that succeeded and write are", "fails in SQLite", "is not in the log", "(write), which began at commit"}},
		// A read-only transaction that moves on to the newest state reads rows that were committed after
		// its beginning, and its later statements report the newer state. When its read of the newest
		// state fails, the transaction keeps the read that the bug ended, whose end then fails. The bug
		// needs commits between two statements of one transaction, which the callers of the hammer
		// environment give, on the profile of the transactions: 194 of 200 seeds find the bug, 173 of
		// them by its own violation.
		{"readNewest", hammerWorkload, "hammer", stmttest.Transactions, []string{"(read-only), which began at commit", "the statements of the read-only transaction", "the transaction has ended"}},
		// A roll back that commits leaves in the log a transaction that its caller rolled back. On the
		// profile of the transactions, 109 of 200 seeds find the bug, each by its own violation.
		{"rollbackKeeps", defaultWorkload, "default", stmttest.Transactions, []string{"and its caller rolled it back"}},
		// SQLite converts the real of the log as the bug did, so the replay of the log agrees with the
		// engine, and only the check of the type of each value of the log finds it. In a table of
		// several columns, the real that the bug converts can be the first value of another type than
		// its column, and the engine then refuses a later one, where the rule refuses the real. The bug
		// needs an insert of a whole real into an INT or INTEGER column that otherwise succeeds. The
		// hammer environment runs more statements than the default one, on one instance that no other
		// instance takes the lease from, and one insert in eight of the profile of the tables gives an
		// integer of such a column as a real, [stmttest.Profile.Mistyped]: 119 of 200 seeds find the
		// bug, 114 of them by its own violation.
		{"convertReal", hammerWorkload, "hammer", stmttest.Tables, []string{"stores a value of another type than its column", "and the rule for the type of a value"}},
		// A row that holds its values in other columns than its statement names: the replay of the log
		// into an empty cache, which writes the rows, differs from SQLite, which runs the statements; a
		// row whose key is another value makes a later insert of the log fail in SQLite, and a query
		// give other rows than in SQLite; and a value in a column of another type is refused where the
		// rule refuses nothing. The log holds no such value, since the check of the type sees the values
		// in the columns where the bug puts them. The profile of the tables weighs the inserts and
		// their lists of columns: 127 of 200 seeds find the bug, 35 of them by its own violation.
		{"listOrder", defaultWorkload, "default", stmttest.Tables, []string{"the replay of the log holds", "for its key after the statements of the commit", "fails in SQLite", ", and in SQLite", "and the rule for the type of a value", "rows in SQLite, and its caller was told", "is in no commit, and changes", "which began at commit"}},
		// A column left out of a list that holds the zero value of its type where SQLite has NULL: a
		// query of the row gives other rows than in SQLite, which runs the statements of the log, and
		// so does the replay of the log into an empty cache, which writes its rows. No check of a value
		// that is not NULL finds it: the zero value has the type of its column, and no column that
		// takes NULL is the key. An update or a delete whose WHERE meets the zero value changes another
		// number of rows than in SQLite, and then the statements after it can differ. The bug meets
		// only a column without a default, and no shape of the profile of the tables has one; the
		// hammer environment runs more statements than the default one: 64 of 200 seeds find the bug,
		// 60 of them by its own violation.
		{"zeroLeftOut", hammerWorkload, "hammer", stmttest.Tables, []string{", and in SQLite", "the replay of the log holds", "for its key after the statements of the commit", "rows in SQLite, and its caller was told", "is in no commit, and changes", "which began at commit", "fails in SQLite"}},
		// A scan by one or two bounds of the key that leaves out the row of a closed bound: a select with
		// WHERE, of its own or in a transaction, gives fewer rows than in SQLite. Nothing else sees it:
		// the select writes nothing, and fails nothing. An engine that reads every row, as some engines
		// of a node do (see [run.open]), does not have the bug. The bug needs a select whose bound of the
		// key is the key of a row, on an engine that reads by the key. The one engine of the hammer
		// environment reads as the rules choose, and the profile of the selects with WHERE weighs
		// such selects, and writes no update or delete that takes their rows: 134 of 200 seeds find the
		// bug, each by its own violation.
		{"openBound", hammerWorkload, "hammer", stmttest.Where, []string{", and in SQLite", "which began at commit"}},
		// A scan that pins a column of the key of an index, or of the primary key, to the value of a
		// condition on another column. Where the value is of another type than the column, or NULL in
		// the primary key, the storage core refuses the scan, which the engine had checked, as an
		// internal error. Otherwise a select gives other rows than in SQLite, and an update or a delete
		// changes other rows, after which the statements can differ. The profile of the indexes weighs
		// the WHERE that pin the columns of an index, and the hammer environment runs more of them
		// than the default one: 81 of 200 seeds find the bug, 66 of them by its own violation, where 43
		// and 38 do in the default environment.
		{"pinByPlace", hammerWorkload, "hammer", stmttest.Indexes, []string{"the storage core refused a call that the engine had checked", ", and in SQLite", "which began at commit", "rows in SQLite, and its caller was told", "is in no commit, and changes", "the replay of the log holds", "fails in SQLite"}},
		// An update or a delete that changes nothing and says so: of its own, it is in no commit, and
		// SQLite, which runs it on the state that it saw, changes rows; in a transaction, SQLite gives
		// another count; and the rows that it should have changed make later statements differ. The
		// profile of the updates and deletes weighs them, and aims most of them at a table that is
		// there: 192 of 200 seeds find the bug, 178 of them by its own violation.
		{"changeNothing", defaultWorkload, "default", stmttest.Changes, []string{"is in no commit, and changes", "rows in SQLite, and its caller was told", ", and in SQLite", "which began at commit", "the replay of the log holds", "fails in SQLite"}},
		// An update that changes the key and leaves the old row: the commit object holds the new row and
		// no delete of the old key, so the replay of the log into an empty cache, which writes its rows,
		// differs from SQLite, which runs the statements; and the row that stays makes later statements
		// differ, among them a write whose net effect on the rows that the bug left is nothing, which is
		// then in no commit object, and not in the log that its caller was told of. The profile of the
		// updates and deletes weighs the updates, some of which change the key of a row: 90 of 200
		// seeds find the bug, 38 of them by its own violation.
		{"keepOldKey", defaultWorkload, "default", stmttest.Changes, []string{"the replay of the log holds", "for its key after the statements of the commit", ", and in SQLite", "which began at commit", "rows in SQLite, and its caller was told", "fails in SQLite", "is in no commit, and changes", "is not in the log"}},
		// A create table with IF NOT EXISTS of a table that exists, which replaces the table with the
		// one of the statement, empty, where SQLite does nothing: the commit object holds the drop and
		// the create, so the replay of the log into an empty cache, which writes them, differs from
		// SQLite, which runs the statement and does nothing; and the rows that the table lost make later
		// statements differ. One create in two of the profile of the clauses of slice 10 says IF NOT
		// EXISTS, of tables that are there and that are not: 183 of 200 seeds find the bug, 106 of them
		// by its own violation. The table of the statement can have other types than SQLite's, so a
		// value that the engine stores is of another type than SQLite's column, as in seed 7557.
		{"replaceExisting", defaultWorkload, "default", stmttest.Exists, []string{"the replay of the log holds", ", and in SQLite", "which began at commit", "rows in SQLite, and its caller was told", "fails in SQLite", "is in no commit, and changes", "and the rule for the type of a value", "stores a value of another type than its column"}},
		// An insert that leaves out a column with a default and stores NULL there: a query of the row
		// gives other rows than in SQLite, which runs the statements of the log, and so does the replay
		// of the log into an empty cache, which writes its rows; where the column does not take NULL, the
		// engine gives NOT NULL where SQLite stores the default, so a later statement can differ. A
		// write in a transaction plants it too. Every shape of the profile of the defaults has a
		// default, and the list of an insert leaves a column with a default out one time in two: 137
		// of 200 seeds find the bug, each by its own violation.
		{"nullDefault", defaultWorkload, "default", stmttest.Defaults, []string{", and in SQLite", "the replay of the log holds", "for its key after the statements of the commit", "which began at commit", "rows in SQLite, and its caller was told", "fails in SQLite", "is in no commit, and changes", "and the rule for the type of a value"}},
		// A scan whose bound from a condition with its value on the left, of slice 12, is the operator as
		// the text writes it, so that `5 < k` reads the keys below 5: a select with WHERE gives fewer
		// rows than in SQLite, since the filter above the scan drops the rows that the scan read wrongly
		// and cannot give those that it did not read; an update or a delete changes fewer rows, after
		// which the statements can differ. An engine that reads every row does not have the bug, and
		// the one engine of the hammer environment reads as the rules choose. One comparison in two of
		// the profile of the values on the left has its value there: 130 of 200 seeds find the bug, 92
		// of them by its own violation.
		{"unturnedBound", hammerWorkload, "hammer", stmttest.ValueFirst, []string{", and in SQLite", "which began at commit", "rows in SQLite, and its caller was told", "is in no commit, and changes", "the replay of the log holds", "for its key after the statements of the commit", "fails in SQLite"}},
		// A key delivers an ORDER BY of mixed directions as if each were in the direction of its first
		// column, so a select gives its rows in another order than SQLite's where they tie in the first.
		// The profile of the ORDER BY draws ORDER BY of the leading columns of the key of an index with
		// the first or the second turned, over values that tie, [stmttest.Profile.Ties], and the hammer
		// environment runs more statements on one instance than the default one, which it needs to find
		// the bug in one seed in five: 49 of 200 seeds find the bug, each by its own violation, as many
		// without crashes, as under the address sanitizer, since the environment has none. In the
		// default one, whose seeds play out otherwise without crashes, CI's sanitizer found it in 1 of 100.
		{"turnMixed", hammerWorkload, "hammer", stmttest.Order, []string{", and in SQLite"}},
		// A LIMIT takes the first rows of an access that is sorted, and sorts those, so a select gives
		// other rows than SQLite's first: in the hammer environment, as for turnMixed, 82 of 200 seeds
		// find the bug, each by its own violation.
		{"limitFirst", hammerWorkload, "hammer", stmttest.Order, []string{", and in SQLite"}},
		// A sort by a descending last column of ORDER BY sorts it ascending: in the hammer environment,
		// as for turnMixed, 118 of 200 seeds find the bug, each by its own violation.
		{"ascendLast", hammerWorkload, "hammer", stmttest.Order, []string{", and in SQLite"}},
		// A statement with RETURNING of its own through Query whose commit failed gives its rows
		// without the error, so its caller takes it for committed, and it is not in the log. The bug
		// needs a commit that fails, which every fault of the store gives, of a statement with RETURNING
		// through Query, which the profile of RETURNING weighs; the hammer environment, which has no
		// crashes, plays the same seeds under the address sanitizer: 62 of 200 seeds find the bug, each
		// by its own violation, after the review of the checks of slice 14 aimed the profile.
		{"rowsOfFailed", hammerWorkload, "hammer", stmttest.Returning, []string{"is not in the log"}},
		// An update with RETURNING that gives the rows as they were: of its own, its rows differ from
		// those that SQLite gives for its commit in the log, and in a transaction from SQLite's on the
		// state that the transaction saw. In the hammer environment, as for rowsOfFailed: 149 of 200
		// seeds find the bug, 54 of them by its own violation.
		{"oldValues", hammerWorkload, "hammer", stmttest.Returning, []string{"gives the rows of RETURNING", ", and in SQLite"}},
		// A statement with RETURNING that changes several rows and leaves out the last, found as
		// oldValues is, in a transaction more often than of its own: 143 of 200 seeds find the bug, 125
		// of them by its own violation.
		{"dropLast", hammerWorkload, "hammer", stmttest.Returning, []string{", and in SQLite", "gives the rows of RETURNING"}},
		// The bugs of the scalar subqueries of slice 15, of 200 seeds on 2026-11-24, before the aims of
		// the review of their checks that day, which changed the profile and did not count them again;
		// every bug was still found by two seeds of its own violation after them. A value that is not among SQLite's candidates is found by [run.flattened], whose
		// violation says that the statement "gives the subquery" a value, and SQLite one of others. The
		// hammer environment has no crashes, so the first 100 of the seeds are those of the sanitizer's
		// setting; on the profile before its last aims they found each bug 14 to 87 times.
		//
		// The value of the last row of several: 184 seeds, all by their own violation.
		{"lastRow", hammerWorkload, "hammer", stmttest.Subquery, subqueryFinds("gives the subquery")},
		// The first row whose value is not NULL, which needs NULL first under an ORDER BY that decides
		// it: 104 seeds, all by their own violation.
		{"firstNotNull", hammerWorkload, "hammer", stmttest.Subquery, subqueryFinds("gives the subquery")},
		// An update or a delete that sees its own changes, found by the count of its changes in SQLite's
		// run of the log or of its transaction: 138 seeds, 77 of them by their own violation.
		{"ownChanges", hammerWorkload, "hammer", stmttest.Subquery, subqueryFinds("rows in SQLite, and its caller was told")},
		// A value kept from the last run of a statement of the same plan, which the profile's texts run
		// again: 106 seeds, all by their own violation.
		{"keepValue", hammerWorkload, "hammer", stmttest.Subquery, subqueryFinds("gives the subquery")},
		// No row taken for a value that every row matches, found by the rows of a select: 192 seeds, 186
		// of them by their own violation.
		{"noRowMatches", hammerWorkload, "hammer", stmttest.Subquery, subqueryFinds(", and in SQLite")},
		// A subquery of a write in a transaction that reads the newest committed state: 106 seeds, 60 of
		// them by a value that is not a candidate on the state of the transaction. The others find that
		// the committed state lacks a table that the transaction created, which the storage refuses.
		{"outsideTx", hammerWorkload, "hammer", stmttest.Subquery, append(subqueryFinds("gives the subquery"), "the storage core refused a call that the engine had checked")},
		// The values of two subqueries swapped: 150 seeds, 147 of them by their own violation. The others
		// find that a value of another type than its column, which the swap gave a condition after the
		// check of the types, reaches the storage, which refuses it, or the engine's own comparison,
		// which fails as an internal error, as in seed 10252.
		{"swapSubqueries", hammerWorkload, "hammer", stmttest.Subquery, append(subqueryFinds("gives the subquery"), "the storage core refused a call that the engine had checked", "internal error: a comparison of a value of kind")},
		// The first row of a subquery with LIMIT 0: 138 seeds, all by their own violation.
		{"limitZeroRow", hammerWorkload, "hammer", stmttest.Subquery, subqueryFinds("gives the subquery")},
		// The first row of the access of the conditions alone, and not of the ORDER BY: 174 seeds, all by
		// their own violation.
		{"orderIgnored", hammerWorkload, "hammer", stmttest.Subquery, subqueryFinds("gives the subquery")},
		// The bugs of arithmetic, on the profile of arithmetic in the hammer environment, which runs more
		// statements on one instance than the default one, of 1000 seeds on 2026-11-25, each bug in a
		// process of its own. A wrong value or a wrong row of arithmetic shows as the rows of a select
		// that differ from SQLite's on the state that it saw, of its own or in a transaction, and then as
		// what follows from the rows that a write changed wrongly: the replay of the log, a commit of the
		// log that fails in SQLite, a count of changes, the rows of RETURNING, and an evaluation error
		// that no row of SQLite justifies, on the rows that differ; see [arithmeticFinds].
		//
		// The values of SET computed from the row as the assignments before left it: 334 seeds, 278 of
		// them by their own violation.
		{"setFromNew", hammerWorkload, "hammer", stmttest.Arithmetic, arithmeticFinds(", and in SQLite")},
		// A comparison with a side that is NULL true: 262 seeds, 244 of them by their own violation,
		// counted before it was planted in a comparison of two columns too, on 2026-11-26. A condition
		// of a subquery whose arithmetic has a NULL, as `k + ? = 0` with NULL, finds a row for it, which
		// gives the subquery a value where SQLite has none.
		{"nullCompares", hammerWorkload, "hammer", stmttest.Arithmetic, append(arithmeticFinds(", and in SQLite"), "gives the subquery")},
		// An update that stops at its evaluation error and succeeds with the rows before it: 202 seeds,
		// 168 of them by their own violation; its commit in the log fails in SQLite, which computes the
		// quirk on every row, in 86.
		{"evalStops", hammerWorkload, "hammer", stmttest.Arithmetic, arithmeticFinds(", and in SQLite")},
		// A row whose comparison fails with an evaluation error left out: 287 seeds, 271 of them by their
		// own violation.
		{"errorSkipsRow", hammerWorkload, "hammer", stmttest.Arithmetic, arithmeticFinds(", and in SQLite")},
		// A copy of a column alone into a column of another class: 222 seeds, 119 of them by the storage
		// core, which refuses the value of another kind than its column; the rules of the types refuse
		// the update that succeeded in a transaction, or whose commit the log holds; and a copy into
		// the key compares the new key of another kind with the old one, as in seed 11085, which fails as
		// an internal error.
		{"copyAcross", hammerWorkload, "hammer", stmttest.Arithmetic, []string{"the storage core refused a call that the engine had checked",
			"the rules of the types of arithmetic refuse it", "has arithmetic that the rules of its types refuse", "internal error: a comparison of a value of kind"}},
		// A copy of a column alone that keeps the value of the column that it sets: 174 seeds, 136 of
		// them by their own violation.
		{"copySelf", hammerWorkload, "hammer", stmttest.Arithmetic, arithmeticFinds(", and in SQLite")},
		// The bugs of the loop of ExecAll and its commit path, on the profile of several statements, of
		// 1000 seeds on 2026-11-26, each bug in a process of its own, in the hammer environment, which
		// runs more statements on one instance than the default one, but for commitLater. As for every
		// bug, a seed that finds one must have a violation in its list, and need not have each of them
		// there: a list holds the violations that the bug causes itself, and leaves out those that
		// follow from one of them, as the commit of oneCommit that holds a statement after another of
		// its text also gives the violation of verifyTexts "is in commit N, and the statement M before
		// it in the text in commit N", and a call of a transaction after the begin of undoInTx failed
		// gives "no transaction is open".
		//
		// All statements of a text outside a transaction in one commit: 999 seeds, 976 of them by their
		// own violation. The bug logs an ok event for each statement before the commit at the end,
		// so a statement that started after one of them saw a state without it, of its own or in a
		// transaction, and the values of its subqueries were not SQLite's on that state; and the commit
		// at the end can fail, which loses statements whose events said they committed.
		{"oneCommit", hammerWorkload, "hammer", stmttest.Several, []string{"is a statement of its own", "was committed before it started", "is not in the log", "was committed before it began", "and SQLite gives"}},
		// The statements after a failed one run: 1000 seeds, all by their own violation.
		{"goOn", hammerWorkload, "hammer", stmttest.Several, []string{"which failed"}},
		// A statement that fails while it runs commits what it changed before it failed, which needs an
		// update that changes rows before it fails on a later one: 234 seeds, 225 of them by their own
		// violation, and the others by an update that failed with an evaluation error, which its caller
		// was told as a statement refused.
		{"commitFailed", hammerWorkload, "hammer", stmttest.Several, []string{"and its caller was told SQLite's error", "and its caller was told skipped"}},
		// A write that another statement follows succeeds with its commit left for the next statement:
		// a crash between the two, which the default environment has, or a commit that then fails loses
		// it, and a statement that started after its event saw a state without it. 610 seeds, 602 of them
		// by their own violation; in 296 of them, a statement is lost whose call never returned because
		// its instance crashed after it, as the crash of [run.crash] aims at such a text.
		{"commitLater", defaultWorkload, "default", stmttest.Several, []string{"is not in the log", "was committed before it began", "was committed before it started"}},
		// The annotation of each statement holds the whole text, which is not the statement's own text,
		// and whose first ID is that of the first statement, which is then in two commits: 999 seeds,
		// each by its own violation.
		{"annotateText", hammerWorkload, "hammer", stmttest.Several, []string{"whose own text is", " and commit ", "is not in the log"}},
		// A failure after the first statement of a text in a write transaction rolls it back and begins a
		// new one: 987 seeds, 510 of them by their own violation. The others find that the place of the
		// writer was free while the caller's transaction was open, that its commit lost the writes
		// before the failure, or that its later statements saw a newer state.
		{"undoInTx", hammerWorkload, "hammer", stmttest.Several, []string{"whose statements that succeeded and write are", "while the write transaction", "is not in the log", "saw the states"}},
		// The result of the first statement that sets the count of changes: 983 seeds, all by their own
		// violation, 976 before the result of a call that failed was checked too.
		{"firstResult", hammerWorkload, "hammer", stmttest.Several, []string{"and the events of its statements"}},
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
			for k := uint64(0); k < limit && (own < 2 || os.Getenv("S4QL_ENGINE_SIM_SEEDS") != ""); k++ {
				seed := b.profile.Seed(k)
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
			t.Logf("%v of %v seeds of the profile %v find the bug, %v of them by %q", found, seeds, b.profile.Name, own, b.finds[0])
			is.True(t, own >= 2, fmt.Sprintf("the bug is found by %q too rarely", b.finds[0]))
		})
	}
}
