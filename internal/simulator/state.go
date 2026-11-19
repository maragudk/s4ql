//go:build s4ql_browser

package simulator

import (
	"slices"

	"maragu.dev/s4ql/internal/format"
)

// State of a run at a time, folded from the events of its trace up to and including that time.
type State struct {
	// Error says why there is no state, and is the only field then.
	Error     string          `json:"error,omitzero"`
	T         float64         `json:"t"`
	Bucket    Bucket          `json:"bucket"`
	Instances []InstanceState `json:"instances"`
	// Requests to the store that are in flight: on their way, held back, or waited for by their
	// caller.
	Requests []RequestState `json:"requests"`
	// Calls of the application that have started and not ended.
	Calls []CallState `json:"calls"`
	// Client of the run, if it has one.
	Client ClientState `json:"client,omitzero"`
	// OutageUntil is when the last outage of the store ends, and Calm whether the store makes no new
	// fault happen.
	OutageUntil float64 `json:"outageUntil,omitzero"`
	Calm        bool    `json:"calm,omitzero"`
	// Violations of the invariants so far, and Excused those that the design accepts.
	Violations int `json:"violations"`
	Excused    int `json:"excused"`
}

// Bucket of the store: the manifest, and the commit objects.
type Bucket struct {
	Manifest        *ManifestObject `json:"manifest"`
	ManifestVersion string          `json:"manifestVersion,omitzero"`
	// ManifestWrites is the number of writes of the manifest that the store applied.
	ManifestWrites int `json:"manifestWrites"`
	// Commits are the commit objects, in the order in which they were written.
	Commits []CommitState `json:"commits"`
	// Log is the names of the commit objects of the log, from the first to the head.
	Log []string `json:"log"`
	// CommitCount and LogLength are the numbers of commit objects in the bucket and in the log, in a
	// state that lists only the last of each.
	CommitCount int `json:"commitCount,omitzero"`
	LogLength   int `json:"logLength,omitzero"`
}

// CommitState is a commit object in the bucket: what it holds, who wrote it and when, and whether
// the log holds it.
type CommitState struct {
	CommitObject `json:",inline"`
	Process      string  `json:"process"`
	At           float64 `json:"at"`
	InLog        bool    `json:"inLog"`
}

// InstanceState is an instance as its events show it.
type InstanceState struct {
	Instance string `json:"instance"`
	Process  string `json:"process"`
	// State is "waiting" for an instance that has not started yet, "starting", "running", "closing"
	// from a close of an action until its end, "closed", "stopped", "crashed", or "failed" for a
	// database that could not be opened.
	State string `json:"state"`
	// Holder is the ID under which the process holds the lease.
	Holder string `json:"holder,omitzero"`
	// Lease is "held" from the event of the core that the lease is taken until the one that it is
	// lost or released, or the stop or crash of the process, and "none" otherwise.
	Lease string `json:"lease"`
	// Position of the cache: the name of the last commit object that it holds.
	Position string `json:"position"`
	// Wall and Monotonic are the clocks of the process, as in [Event].
	Wall             float64 `json:"wall"`
	Monotonic        float64 `json:"monotonic"`
	WallStopped      bool    `json:"wallStopped,omitzero"`
	MonotonicStopped bool    `json:"monotonicStopped,omitzero"`
	// CutOffUntil is when the last cut-off of the process from the store ends.
	CutOffUntil float64 `json:"cutOffUntil,omitzero"`
	// NetworkHung, NetworkDisconnected, and DiskFailing are the faults of the instance that an
	// action switched on and no action has switched off. They hold for the instance across a
	// restart.
	NetworkHung         bool `json:"networkHung,omitzero"`
	NetworkDisconnected bool `json:"networkDisconnected,omitzero"`
	DiskFailing         bool `json:"diskFailing,omitzero"`
}

// RequestState is a request to the store that is in flight.
type RequestState struct {
	Request  int     `json:"request"`
	Instance string  `json:"instance"`
	Process  string  `json:"process"`
	Method   string  `json:"method"`
	Key      string  `json:"key"`
	Purpose  string  `json:"purpose"`
	Sent     float64 `json:"sent"`
	// Phase is "to_store" on the way to the store, "held" when the store holds it back to apply it
	// late, "stuck" when it is stuck in the network, "to_caller" on the way back, and "done" once
	// the store side has ended while the caller still waits.
	Phase string `json:"phase"`
	// Waiting is whether the caller waits for it.
	Waiting bool `json:"waiting"`
	// Happened is what happened to the request so far, by the What of its events.
	Happened []string `json:"happened"`
}

// CallState is a call of the application that has not ended.
type CallState struct {
	Call     int     `json:"call"`
	Instance string  `json:"instance"`
	Process  string  `json:"process"`
	Caller   string  `json:"caller"`
	Op       string  `json:"op"`
	Started  float64 `json:"started"`
	Value    int64   `json:"value,omitzero"`
}

// ClientState is the client of a run.
type ClientState struct {
	// Instance that the client calls now.
	Instance string `json:"instance"`
}

// StateAt the time t of the run res, from the events up to and including t.
func StateAt(res Response, t float64) State {
	f := newFold()
	for _, e := range res.Events {
		if e.T > t {
			break
		}
		f.add(e)
	}
	return f.state(t, 0)
}

// fold of the events of a run into its state, one event at a time, so that a live session keeps its
// state and not its events. It forgets what no state can show any more: a request once its caller
// does not wait for it and the store side has ended, and a call once it has ended.
type fold struct {
	s State
	// base holds the clocks of each instance at its last event about them.
	base     map[string]clocks
	instance map[string]int
	requests map[int]*RequestState
	// order of the requests in requests, by their numbers, in the order sent. It also holds numbers
	// of requests that are forgotten, until it is compacted.
	order   []int
	arrived map[int]bool
	commits map[string]*CommitState
	calls   map[int]CallState

	// log is the chain of commit objects from the first to head, as far as the commit objects are
	// known, and inLog its names. It is whole when it reaches the empty log, and then grows by one
	// when the head moves on to a child of the head. Otherwise follow is set, and the next state
	// follows the chain again from the head.
	log    []string
	inLog  map[string]bool
	head   string
	whole  bool
	follow bool
}

// clocks of an instance at an event: its time, and the readings of the two clocks.
type clocks struct{ t, wall, monotonic float64 }

func newFold() *fold {
	return &fold{
		s:        State{Instances: []InstanceState{}, Bucket: Bucket{Commits: []CommitState{}}},
		base:     map[string]clocks{},
		instance: map[string]int{},
		requests: map[int]*RequestState{},
		arrived:  map[int]bool{},
		commits:  map[string]*CommitState{},
		calls:    map[int]CallState{},
		inLog:    map[string]bool{},
		head:     format.EmptyLog.String(),
		whole:    true,
	}
}

// of is the state of the instance of e, which starts as a new one the first time.
func (f *fold) of(e Event) *InstanceState {
	i, ok := f.instance[e.Instance]
	if !ok {
		i = len(f.s.Instances)
		f.instance[e.Instance] = i
		f.s.Instances = append(f.s.Instances, InstanceState{Instance: e.Instance, Lease: "none", Position: format.EmptyLog.String()})
	}
	return &f.s.Instances[i]
}

// add the event e, the next of the run.
func (f *fold) add(e Event) {
	s := &f.s
	switch e.Kind {
	case "process":
		in := f.of(e)
		switch e.What {
		case "waiting":
			in.State = "waiting"
		case "started":
			in.Process, in.State, in.Holder, in.Lease, in.CutOffUntil = e.Process, "starting", "", "none", 0
			in.WallStopped, in.MonotonicStopped = false, false
			f.base[e.Instance] = clocks{t: e.T, wall: deref(e.Wall), monotonic: deref(e.Monotonic)}
		case "opened":
			in.State, in.Holder = "running", e.Holder
		case "open_failed":
			in.State = "failed"
		case "closed":
			in.State, in.Lease = "closed", "none"
		case "stopped":
			in.State, in.Lease = "stopped", "none"
		case "crashed":
			in.State, in.Lease = "crashed", "none"
			// The calls of the process end with it, and nothing waits for its requests, which the
			// network can still carry.
			for id, c := range f.calls {
				if c.Process == e.Process {
					delete(f.calls, id)
				}
			}
			for id, rs := range f.requests {
				if rs.Process == e.Process {
					rs.Waiting = false
					f.forget(id)
				}
			}
		}
	case "clock":
		in := f.of(e)
		in.WallStopped, in.MonotonicStopped = e.WallStopped, e.MonotonicStopped
		f.base[e.Instance] = clocks{t: e.T, wall: deref(e.Wall), monotonic: deref(e.Monotonic)}
	case "core":
		f.core(e)
	case "call":
		if e.What == "started" {
			f.calls[e.Call] = CallState{Call: e.Call, Instance: e.Instance, Process: e.Process, Caller: e.Caller, Op: e.Op, Started: e.T, Value: e.Value}
			break
		}
		delete(f.calls, e.Call)
		if in := f.of(e); e.What == "served" && in.Process == e.Process {
			in.Position = e.Position
		}
	case "store":
		switch e.What {
		case "cut_off":
			in := f.of(e)
			in.CutOffUntil = max(in.CutOffUntil, e.T+e.For)
		case "outage":
			s.OutageUntil = max(s.OutageUntil, e.T+e.For)
		case "calm":
			s.Calm = true
		}
	case "action":
		if e.What != "done" {
			break
		}
		switch e.Do {
		case "close":
			f.of(e).State = "closing"
		case "hangNetwork":
			f.of(e).NetworkHung = deref(e.On)
		case "disconnectNetwork":
			f.of(e).NetworkDisconnected = deref(e.On)
		case "failDisk":
			f.of(e).DiskFailing = deref(e.On)
		case "client":
			s.Client.Instance = e.Instance
		}
	case "client":
		if e.What == "started" {
			s.Client.Instance = e.Instance
		}
	case "check":
		s.Violations++
		if e.Excused {
			s.Excused++
		}
	case "request":
		f.request(e)
	}
}

// core event e, of the storage core of a process.
func (f *fold) core(e Event) {
	in := f.of(e)
	if in.Process != e.Process {
		return
	}
	outcome, _ := e.Attrs["outcome"].(string)
	switch e.Event {
	case "lease":
		switch transition, _ := e.Attrs["lease.transition"].(string); {
		case transition == "taken" && outcome == "taken":
			in.Lease = "held"
		case transition == "lost", transition == "released" && outcome == "released":
			in.Lease = "none"
		}
	case "commit":
		if name, ok := e.Attrs["commit.name"].(string); ok && (outcome == "committed" || outcome == "committed_and_stopped") {
			in.Position = name
		}
	case "catch_up":
		to, _ := e.Attrs["catch_up.to"].(string)
		from, _ := e.Attrs["catch_up.from"].(string)
		applied := attrInt(e.Attrs["catch_up.commits"])
		switch {
		case outcome == "caught_up":
			in.Position = to
		case applied > 0:
			in.Position = after(f.commits, from, to, applied)
		}
	case "stop":
		if outcome != "closed" {
			in.State = "stopped"
		}
		in.Lease = "none"
	}
}

// request event e.
func (f *fold) request(e Event) {
	if e.What == "applied" && e.Object != nil {
		switch o := e.Object; {
		case o.Manifest != nil:
			f.s.Bucket.Manifest, f.s.Bucket.ManifestVersion = o.Manifest, e.Version
			f.s.Bucket.ManifestWrites++
			f.moveHead(o.Manifest.Head)
		case o.Commit != nil:
			if f.commits[o.Commit.Name] == nil {
				c := &CommitState{CommitObject: *o.Commit, Process: e.Process, At: e.T}
				f.commits[o.Commit.Name] = c
				f.s.Bucket.Commits = append(f.s.Bucket.Commits, *c)
				// The commit object can be one that the log lacks.
				f.follow = f.follow || !f.whole
			}
		}
	}
	if e.Request == 0 {
		return
	}
	rs := f.requests[e.Request]
	if e.What == "sent" {
		rs = &RequestState{Request: e.Request, Instance: e.Instance, Process: e.Process, Method: e.Method, Key: e.Key, Purpose: e.Purpose,
			Sent: e.T, Phase: "to_store", Waiting: true, Happened: []string{}}
		f.requests[e.Request] = rs
		f.order = append(f.order, e.Request)
		return
	}
	if rs == nil {
		return
	}
	rs.Happened = append(rs.Happened, e.What)
	switch e.What {
	case "returned", "answer_lost", "disconnected":
		rs.Waiting = false
	case "held":
		rs.Phase = "held"
	case "stuck":
		rs.Phase = "stuck"
	case "arrived":
		rs.Phase, f.arrived[e.Request] = "to_store", true
	case "applied", "refused", "read", "read_broken", "not_found":
		rs.Phase = "to_caller"
		if f.arrived[e.Request] {
			rs.Phase = "done"
		}
	case "answered", "no_caller", "response_lost", "lost", "never_applied":
		rs.Phase = "done"
	}
	f.forget(e.Request)
}

// forget the request with the number id once no state can list it: its caller does not wait for it,
// and its phase is done. No event of a request after that lists it again: the events that move a
// phase away from done, "stuck" and "arrived", come before the store side of a request ends.
func (f *fold) forget(id int) {
	rs := f.requests[id]
	if rs == nil || rs.Waiting || rs.Phase != "done" {
		return
	}
	delete(f.requests, id)
	delete(f.arrived, id)
	if len(f.order) > 2*len(f.requests)+64 {
		f.order = slices.DeleteFunc(f.order, func(id int) bool { return f.requests[id] == nil })
	}
}

// moveHead of the log to the commit object of the given name.
func (f *fold) moveHead(head string) {
	if head == f.head {
		return
	}
	if c := f.commits[head]; f.whole && !f.follow && c != nil && c.Parent == f.head {
		f.log = append(f.log, head)
		f.inLog[head] = true
	} else {
		f.follow = true
	}
	f.head = head
}

// followLog from the head through the parents of the commit objects towards the empty log, as far as
// the commit objects are known.
func (f *fold) followLog() {
	empty := format.EmptyLog.String()
	f.log, f.inLog, f.follow = nil, map[string]bool{}, false
	name := f.head
	for name != empty && len(f.log) <= len(f.commits) {
		c := f.commits[name]
		if c == nil {
			break
		}
		f.log = append(f.log, name)
		f.inLog[name] = true
		name = c.Parent
	}
	f.whole = name == empty
	slices.Reverse(f.log)
}

// state at the time t, which is no earlier than the last event added. If commits is above 0, the
// bucket lists only the last that many commit objects and the last that many names of the log, and
// says how many it has of each in all.
func (f *fold) state(t float64, commits int) State {
	if f.follow {
		f.followLog()
	}
	s := f.s
	s.T = t
	s.Instances = slices.Clone(f.s.Instances)
	for i := range s.Instances {
		in := &s.Instances[i]
		b := f.base[in.Instance]
		in.Wall, in.Monotonic = b.wall, b.monotonic
		if !in.WallStopped {
			in.Wall += t - b.t
		}
		if !in.MonotonicStopped {
			in.Monotonic += t - b.t
		}
	}
	all, log := f.s.Bucket.Commits, f.log
	if commits > 0 {
		s.Bucket.CommitCount, s.Bucket.LogLength = len(all), len(log)
		all, log = all[max(0, len(all)-commits):], log[max(0, len(log)-commits):]
	}
	s.Bucket.Commits = make([]CommitState, len(all))
	for i, c := range all {
		c.InLog = f.inLog[c.Name]
		s.Bucket.Commits[i] = c
	}
	s.Bucket.Log = append([]string{}, log...)
	s.Requests = []RequestState{}
	for _, id := range f.order {
		if rs := f.requests[id]; rs != nil {
			c := *rs
			c.Happened = slices.Clone(rs.Happened)
			s.Requests = append(s.Requests, c)
		}
	}
	s.Calls = []CallState{}
	for _, c := range f.calls {
		s.Calls = append(s.Calls, c)
	}
	slices.SortFunc(s.Calls, func(a, b CallState) int { return a.Call - b.Call })
	return s
}

// after is the name of the commit object that is n commit objects after from, on the way from from
// to to, or to if the way cannot be followed.
func after(commits map[string]*CommitState, from, to string, n int) string {
	var way []string
	for name := to; name != from && len(way) <= len(commits); {
		c := commits[name]
		if c == nil {
			return to
		}
		way = append(way, name)
		name = c.Parent
	}
	if n > len(way) {
		return to
	}
	return way[len(way)-n]
}

// attrInt is an attribute of the core that is an integer: an int64 in the events of the engine, and a
// float64 in events that went through JSON, as those that a test or a page folds.
func attrInt(v any) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
