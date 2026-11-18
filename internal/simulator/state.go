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
	// State is "starting", "running", "closing" from a close of an action until its end, "closed",
	// "stopped", "crashed", or "failed" for a database that could not be opened.
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

// StateAt the time t of the run res, from the events up to and including t.
func StateAt(res Response, t float64) State {
	s := State{T: t, Instances: []InstanceState{}, Requests: []RequestState{}, Calls: []CallState{}, Bucket: Bucket{Commits: []CommitState{}, Log: []string{}}}
	// clocks of each instance at its last event about them.
	type clocks struct{ t, wall, monotonic float64 }
	base := map[string]clocks{}
	instance := map[string]int{}
	requests := map[int]*RequestState{}
	var order []int
	arrived := map[int]bool{}
	commits := map[string]*CommitState{}
	calls := map[int]CallState{}
	empty := format.EmptyLog.String()

	at := func(e Event) *InstanceState {
		i, ok := instance[e.Instance]
		if !ok {
			i = len(s.Instances)
			instance[e.Instance] = i
			s.Instances = append(s.Instances, InstanceState{Instance: e.Instance, Lease: "none", Position: empty})
		}
		return &s.Instances[i]
	}

	for _, e := range res.Events {
		if e.T > t {
			break
		}
		switch e.Kind {
		case "process":
			in := at(e)
			switch e.What {
			case "started":
				in.Process, in.State, in.Holder, in.Lease, in.CutOffUntil = e.Process, "starting", "", "none", 0
				in.WallStopped, in.MonotonicStopped = false, false
				base[e.Instance] = clocks{t: e.T, wall: deref(e.Wall), monotonic: deref(e.Monotonic)}
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
				for id, c := range calls {
					if c.Process == e.Process {
						delete(calls, id)
					}
				}
				for _, rs := range requests {
					if rs.Process == e.Process {
						rs.Waiting = false
					}
				}
			}
		case "clock":
			in := at(e)
			in.WallStopped, in.MonotonicStopped = e.WallStopped, e.MonotonicStopped
			base[e.Instance] = clocks{t: e.T, wall: deref(e.Wall), monotonic: deref(e.Monotonic)}
		case "core":
			in := at(e)
			if in.Process != e.Process {
				break
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
				applied, _ := e.Attrs["catch_up.commits"].(int64)
				switch {
				case outcome == "caught_up":
					in.Position = to
				case applied > 0:
					in.Position = after(commits, from, to, int(applied))
				}
			case "stop":
				if outcome != "closed" {
					in.State = "stopped"
				}
				in.Lease = "none"
			}
		case "call":
			if e.What == "started" {
				calls[e.Call] = CallState{Call: e.Call, Instance: e.Instance, Process: e.Process, Caller: e.Caller, Op: e.Op, Started: e.T, Value: e.Value}
				break
			}
			delete(calls, e.Call)
			if in := at(e); e.What == "served" && in.Process == e.Process {
				in.Position = e.Position
			}
		case "store":
			switch e.What {
			case "cut_off":
				in := at(e)
				in.CutOffUntil = max(in.CutOffUntil, e.T+e.For)
			case "outage":
				s.OutageUntil = max(s.OutageUntil, e.T+e.For)
			case "calm":
				s.Calm = true
			}
		case "action":
			if e.Do == "close" && e.What == "done" {
				at(e).State = "closing"
			}
		case "check":
			s.Violations++
			if e.Excused {
				s.Excused++
			}
		case "request":
			if e.What == "applied" && e.Object != nil {
				switch o := e.Object; {
				case o.Manifest != nil:
					s.Bucket.Manifest, s.Bucket.ManifestVersion = o.Manifest, e.Version
					s.Bucket.ManifestWrites++
				case o.Commit != nil:
					if commits[o.Commit.Name] == nil {
						c := &CommitState{CommitObject: *o.Commit, Process: e.Process, At: e.T}
						commits[o.Commit.Name] = c
						s.Bucket.Commits = append(s.Bucket.Commits, *c)
					}
				}
			}
			if e.Request == 0 {
				break
			}
			rs := requests[e.Request]
			if e.What == "sent" {
				rs = &RequestState{Request: e.Request, Instance: e.Instance, Process: e.Process, Method: e.Method, Key: e.Key, Purpose: e.Purpose,
					Sent: e.T, Phase: "to_store", Waiting: true, Happened: []string{}}
				requests[e.Request] = rs
				order = append(order, e.Request)
				break
			}
			if rs == nil {
				break
			}
			rs.Happened = append(rs.Happened, e.What)
			switch e.What {
			case "returned", "answer_lost":
				rs.Waiting = false
			case "held":
				rs.Phase = "held"
			case "stuck":
				rs.Phase = "stuck"
			case "arrived":
				rs.Phase, arrived[e.Request] = "to_store", true
			case "applied", "refused", "read", "read_broken", "not_found":
				rs.Phase = "to_caller"
				if arrived[e.Request] {
					rs.Phase = "done"
				}
			case "answered", "no_caller", "response_lost", "lost", "never_applied":
				rs.Phase = "done"
			}
		}
	}

	for i := range s.Instances {
		in := &s.Instances[i]
		b := base[in.Instance]
		in.Wall, in.Monotonic = b.wall, b.monotonic
		if !in.WallStopped {
			in.Wall += t - b.t
		}
		if !in.MonotonicStopped {
			in.Monotonic += t - b.t
		}
	}
	if m := s.Bucket.Manifest; m != nil {
		for name := m.Head; name != empty && len(s.Bucket.Log) <= len(commits); {
			c := commits[name]
			if c == nil {
				break
			}
			s.Bucket.Log = append(s.Bucket.Log, name)
			name = c.Parent
		}
		slices.Reverse(s.Bucket.Log)
	}
	for i := range s.Bucket.Commits {
		s.Bucket.Commits[i].InLog = slices.Contains(s.Bucket.Log, s.Bucket.Commits[i].Name)
	}
	for _, id := range order {
		if rs := requests[id]; rs.Waiting || rs.Phase != "done" {
			s.Requests = append(s.Requests, *rs)
		}
	}
	for _, c := range calls {
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

func deref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}
