//go:build s4ql_browser

package simulator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"maragu.dev/s4ql/internal/sim"
)

// Event of the trace. Kind says what it is about, and What what happened, in a word; the README
// lists both, with the fields that each carries. A field that does not apply is left out.
type Event struct {
	// T is the fake time of the event, in milliseconds from the start of the run.
	T    float64 `json:"t"`
	Kind string  `json:"kind"`
	What string  `json:"what,omitzero"`

	// Instance is the name of the instance, a, b, and so on, which stays the same across a
	// restart, and Process the name of the process that runs it now, such as a2 after its first
	// restart. Holder is the ID under which the process holds the lease.
	Instance string `json:"instance,omitzero"`
	Process  string `json:"process,omitzero"`
	Holder   string `json:"holder,omitzero"`

	// Call is the number of a call of the application, Caller who made it, Op whether it is a
	// write, a delete, or a read, and Started when it started.
	Call    int      `json:"call,omitzero"`
	Caller  string   `json:"caller,omitzero"`
	Op      string   `json:"op,omitzero"`
	Started *float64 `json:"started,omitzero"`
	// Value is the key of the row that a write inserts, and Keys those that a read saw. SQL is the
	// statement of a call, and Args its arguments by position. Expect is what a read expects: "row"
	// for a key that the application knows is in the store, and "none" for one whose delete it knows
	// was committed.
	Value  int64   `json:"value,omitzero"`
	SQL    string  `json:"sql,omitzero"`
	Args   []any   `json:"args,omitzero"`
	Expect string  `json:"expect,omitzero"`
	Keys   []int64 `json:"keys,omitzero"`
	// Position is the name of the last commit object in the state that a call saw.
	Position string `json:"position,omitzero"`
	// Columns, Rows, and Count are what a select of the box gave: the names of its columns, its
	// first rows, each with a cell for each column, nil for NULL, and the number of all its rows.
	// Affected is the number of rows that a write of the box changed, if the statement counts them,
	// and Commit the name of its commit object, if it wrote one.
	Columns  []string  `json:"columns,omitzero"`
	Rows     [][]*Cell `json:"rows,omitzero"`
	Count    int       `json:"count,omitzero"`
	Affected *int64    `json:"affected,omitzero"`
	Commit   string    `json:"commit,omitzero"`

	// Request is the number of a request to the store, Method its method, Key its key, Purpose
	// what it is for, and Bytes the size of what it sends.
	Request int    `json:"request,omitzero"`
	Method  string `json:"method,omitzero"`
	Key     string `json:"key,omitzero"`
	Purpose string `json:"purpose,omitzero"`
	Bytes   int    `json:"bytes,omitzero"`
	// Version of the object after a write, or of the object that a read got.
	Version string `json:"version,omitzero"`
	// Write is the number of a write that the store applied, from 1.
	Write int `json:"write,omitzero"`
	// Object is what a write that the store applied wrote.
	Object *Object `json:"object,omitzero"`
	// Way is "there" or "back", for a request that is slow on its way.
	Way string `json:"way,omitzero"`

	// By is a delay or the shift of a clock, and For how long a cut-off or an outage lasts.
	By  float64 `json:"by,omitzero"`
	For float64 `json:"for,omitzero"`
	// Wall and Monotonic are the clocks of a process after the event: the wall clock in
	// milliseconds from the start of the run, and the monotonic clock in milliseconds since the
	// process started. WallStopped and MonotonicStopped say whether each stands still.
	Wall             *float64 `json:"wall,omitzero"`
	Monotonic        *float64 `json:"monotonic,omitzero"`
	WallStopped      bool     `json:"wallStopped,omitzero"`
	MonotonicStopped bool     `json:"monotonicStopped,omitzero"`

	// Event, Level, and Attrs are an event of the storage core: its name, its level, and its
	// attributes, with durations in milliseconds.
	Event string         `json:"event,omitzero"`
	Level string         `json:"level,omitzero"`
	Attrs map[string]any `json:"attrs,omitzero"`

	// Action is the index of an action in the schedule, and Do the action. On is what an action that
	// switches something switched it to.
	Action *int   `json:"action,omitzero"`
	Do     string `json:"do,omitzero"`
	On     *bool  `json:"on,omitzero"`

	// Invariant is the name of an invariant that a check found violated, and Excused says whether
	// the design accepts the violation for the faults of the run.
	Invariant string `json:"invariant,omitzero"`
	Excused   bool   `json:"excused,omitzero"`

	// Site is a point of buggify.
	Site string `json:"site,omitzero"`

	Error string `json:"error,omitzero"`
	// Text says what happened in a sentence, as the store, the scheduler, or the engine put it.
	Text string `json:"text,omitzero"`
}

// Object that a write to the store wrote: a manifest or a commit object, as far as it can be read.
type Object struct {
	Manifest *ManifestObject `json:"manifest,omitzero"`
	Commit   *CommitObject   `json:"commit,omitzero"`
	// Error is why the object could not be read.
	Error string `json:"error,omitzero"`
}

// ManifestObject is what a manifest says.
type ManifestObject struct {
	Head  string `json:"head"`
	Lease *Lease `json:"lease"`
}

// Lease as a manifest records it. Renewed is the holder's wall clock at the renewal, in
// milliseconds from the start of the run, and Salt is the salt in hexadecimal.
type Lease struct {
	Holder   string  `json:"holder"`
	Duration float64 `json:"duration"`
	Renewed  float64 `json:"renewed"`
	Salt     string  `json:"salt"`
}

// CommitObject is what a commit object holds: its name, the name of its parent, the keys of the
// rows that it inserts into the table of the workload and of those that it deletes from it, the
// tables that it creates, the number of its changes, and the statements that its annotations hold.
type CommitObject struct {
	Name       string      `json:"name"`
	Parent     string      `json:"parent"`
	Keys       []int64     `json:"keys,omitzero"`
	Deletes    []int64     `json:"deletes,omitzero"`
	Tables     []string    `json:"tables,omitzero"`
	Changes    int         `json:"changes"`
	Statements []Statement `json:"statements,omitzero"`
}

// Statement of SQL: its text, and its arguments by position, each an integer, a real, or text as
// itself, a blob as hexadecimal text, or null.
type Statement struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args,omitzero"`
}

// recorder collects the events of a run, in the order in which they happen, and counts them for the
// summary. One task runs at a time under the scheduler, so the order is the same for a seed, and the
// mutex is there only for the race detector's sake.
//
// In a batch run it keeps every event. In a live session it keeps the events only until [recorder.take]
// hands them over, and folds each into the state of the session as it comes.
type recorder struct {
	world *sim.Scheduler
	end   time.Duration
	// fold of the events of a live session, or nil for a batch run.
	fold *fold

	mu      sync.Mutex
	events  []Event
	summary Summary
	stopped bool
}

func newRecorder(world *sim.Scheduler, end time.Duration, live bool) *recorder {
	r := &recorder{world: world, end: end, summary: Summary{Calls: map[string]int{}, Requests: map[string]int{}}}
	if live {
		r.fold = newFold()
	}
	return r
}

// add the event e at the time of the scheduler, unless the run is past its end or the recorder has
// stopped.
func (r *recorder) add(e Event) {
	now := r.world.Monotonic()
	r.mu.Lock()
	defer r.mu.Unlock()
	if now > r.end || r.stopped {
		return
	}
	e.T = millis(now)
	r.events = append(r.events, e)
	switch s := &r.summary; {
	case e.Kind == "call" && e.What != "started":
		s.Calls[e.Op+" "+e.What]++
	case e.Kind == "request":
		s.Requests[e.What]++
	case e.Kind == "check":
		s.Violations++
		if e.Excused {
			s.Excused++
		}
	}
	if r.fold != nil {
		r.fold.add(e)
	}
}

// take the events since the last take, which the recorder then forgets.
func (r *recorder) take() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	events := r.events
	r.events = nil
	return events
}

// state of a live session now, with at most commits commit objects if commits is above 0.
func (r *recorder) state(commits int) State {
	now := millis(r.world.Monotonic())
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fold.state(now, commits)
}

// stop the recorder: it adds no event from now on.
func (r *recorder) stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
}

// coreHandler is the handler of the logger of one process, which turns each event of the core into
// an event of the trace.
type coreHandler struct {
	r     *run
	p     *process
	attrs []slog.Attr
}

func (h coreHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h coreHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return h
}

func (h coreHandler) WithGroup(string) slog.Handler { return h }

func (h coreHandler) Handle(ctx context.Context, rec slog.Record) error {
	attrs := map[string]any{}
	add := func(a slog.Attr) bool {
		if a.Key != "" {
			attrs[a.Key] = attrValue(a.Value)
		}
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	rec.Attrs(add)
	h.r.coreEvent(ctx, h.p, rec.Message, rec.Level.String(), attrs)
	return nil
}

// attrValue of an attribute of the core as JSON: a duration in milliseconds, a time as text, and
// anything else as its own kind or as text.
func attrValue(v slog.Value) any {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return millis(v.Duration())
	case slog.KindTime:
		return millis(v.Time().Sub(sim.Epoch()))
	default:
		return fmt.Sprint(v.Any())
	}
}

// storeLine is what the engine makes of a line that the simulated store traces: the kind of event
// and what happened, and the fields that it reads from the arguments. The format strings are those of
// [simstore], so that the arguments can be read by position; a line whose format is not known here
// becomes an event "store" with only its text.
func storeLine(format string, args []any) Event {
	text := fmt.Sprintf(format, args...)
	// Every line starts with the name of the store, which is not part of the text.
	if _, rest, ok := strings.Cut(text, ": "); ok {
		text = rest
	}
	arg := func(i int) any {
		if i < len(args) {
			return args[i]
		}
		return nil
	}
	dur := func(i int) float64 {
		d, _ := arg(i).(time.Duration)
		return millis(d)
	}
	str := func(i int) string { return fmt.Sprint(arg(i)) }
	e := Event{Kind: "request", Text: text}
	switch format {
	case "%v: client %v cut off for %v":
		e.Kind, e.What, e.Process, e.For = "store", "cut_off", str(1), dur(2)
	case "%v: unreachable for %v":
		e.Kind, e.What, e.For = "store", "outage", dur(1)
	case "%v: calm from now on":
		e.Kind, e.What = "store", "calm"
	case "%v: a write to %v of %v bytes from %v: not sent, because it is larger than %v bytes":
		e.What = "too_large"
	case "%v: %v from %v: sent":
		e.What = "sent"
	case "%v: request %v: the caller gives up: %v":
		e.What, e.Error = "gave_up", str(2)
	case "%v: request %v: no answer comes, and the caller waits until it gives up":
		e.What = "no_answer"
	case "%v: request %v: arrives after %v, and is never applied":
		e.What = "never_applied"
	case "%v: request %v: arrives after %v, and is applied %v later":
		e.What, e.By = "held", dur(3)
	case "%v: request %v: the response finds no caller":
		e.What = "no_caller"
	case "%v: request %v: the response arrives":
		e.What = "answered"
	case "%v: request %v: %v%v":
		e.What, e.Error = "response_lost", str(2)
	case "%v: request %v: %v%v, and is never applied":
		e.What, e.Error = "lost", str(2)
	case "%v: request %v: %v%v, and is stuck in the network, to arrive %v later":
		e.What, e.Error, e.By = "stuck", str(2), dur(4)
	case "%v: request %v: arrives after it was stuck, while the store is unreachable, and is never applied":
		e.What = "never_applied"
	case "%v: request %v: arrives after it was stuck":
		e.What = "arrived"
	case "%v: request %v: is slow on its way %v, by %v":
		e.What, e.Way, e.By = "slow", str(2), dur(3)
	case "%v: request %v of %v: %v":
		result := str(3)
		switch {
		case strings.HasPrefix(result, "applied as write "):
			e.What = "applied"
		case strings.HasPrefix(result, "not applied: "):
			e.What, e.Error = "refused", strings.TrimPrefix(result, "not applied: ")
		case strings.HasPrefix(result, "read "):
			e.What = "read"
			if _, broken, ok := strings.Cut(result, ", whose content breaks off after "); ok {
				e.What, e.Error = "read_broken", "the content breaks off after "+broken
			}
		default:
			e.What, e.Error = "not_found", result
		}
	default:
		e.Kind = "store"
	}
	return e
}
