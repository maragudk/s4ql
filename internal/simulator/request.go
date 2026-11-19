//go:build s4ql_browser

package simulator

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"maragu.dev/s4ql/internal/store/simstore"
)

// Request of one run: a seed, the settings, the schedule of actions and faults, and the end. Every
// time and duration is in milliseconds of fake time, and every time counts from the start of the
// run. A field that is missing has the default that [defaultRequest] gives it.
type Request struct {
	Seed uint64 `json:"seed"`
	// End of the run. Nothing in the trace happens after it.
	End float64 `json:"end"`
	// Instances at the start, named a, b, c, and so on.
	Instances int `json:"instances"`
	// Leases are the durations of the lease of the instances, one for each in turn, from a. A
	// restarted instance keeps the duration of the one it replaces.
	Leases []float64 `json:"leases"`
	// Readers is the number of read connections of each instance.
	Readers int `json:"readers"`
	// Latency is the longest time that a request takes to the store, and again back.
	Latency  float64  `json:"latency"`
	Faults   Faults   `json:"faults"`
	Buggify  bool     `json:"buggify"`
	Workload Workload `json:"workload"`
	// Schedule of the actions and faults, each at its time. Actions at the same time happen in the
	// order of the schedule.
	Schedule []Action `json:"schedule"`
	// Lines asks for the text trace of the scheduler in the response.
	Lines bool `json:"lines"`
}

// Faults of the store that happen at random, from the seed, each with its probability for one
// request, as [simstore.Faults] describes. Damaged is the probability that a download of a commit
// object reaches the instance with one byte changed.
type Faults struct {
	LostRequest  float64 `json:"lostRequest"`
	LostResponse float64 `json:"lostResponse"`
	Hang         float64 `json:"hang"`
	Slow         float64 `json:"slow"`
	SlowBy       float64 `json:"slowBy"`
	BrokenRead   float64 `json:"brokenRead"`
	CutOff       float64 `json:"cutOff"`
	CutOffFor    float64 `json:"cutOffFor"`
	Outage       float64 `json:"outage"`
	OutageFor    float64 `json:"outageFor"`
	Conflict     float64 `json:"conflict"`
	Late         float64 `json:"late"`
	AfterCrash   float64 `json:"afterCrash"`
	LateBy       float64 `json:"lateBy"`
	Long         float64 `json:"long"`
	LongBy       float64 `json:"longBy"`
	Damaged      float64 `json:"damaged"`
}

// Workload of the callers that each instance runs, as an application would.
type Workload struct {
	// On is whether the callers make calls from the start. The action "workload" switches them.
	On bool `json:"on"`
	// Callers of each instance.
	Callers int `json:"callers"`
	// Think is the longest pause of a caller before each call. Each pause is from zero to Think.
	Think float64 `json:"think"`
	// Writes is the share of calls that are write transactions, in percent. The others are reads.
	Writes int `json:"writes"`
	// Timeout of each call.
	Timeout float64 `json:"timeout"`
	// Keep is the number of rows that the writes keep in the table: each write also deletes the other
	// rows with the smallest keys, until the table has at most Keep rows, so that the table, and with
	// it the cost of a read, stops growing. A write never deletes its own row, and reads the whole
	// table to find the others. Zero keeps every row.
	Keep int `json:"keep"`
}

// Action of the schedule.
type Action struct {
	// At is the time of the action.
	At float64 `json:"at"`
	// Do is what to do: see [actions].
	Do string `json:"do"`
	// Instance that the action is for, by its name, where the action is for one.
	Instance string `json:"instance,omitzero"`
	// For is how long a cut-off or an outage lasts.
	For float64 `json:"for,omitzero"`
	// By is how far a wall clock is shifted, forward or, if negative, backward.
	By float64 `json:"by,omitzero"`
	// Request is the kind of request that a lost answer or a failed request is for: see [purposes].
	Request string `json:"request,omitzero"`
	// On is whether the action "workload" switches the callers on or off, and whether a fault that
	// the visitor switches, such as "hangNetwork", is on or off.
	On bool `json:"on,omitzero"`
}

// actions that a schedule can hold, with whether each needs an instance.
var actions = map[string]bool{
	"write":          true,
	"read":           true,
	"crash":          true,
	"restart":        true,
	"close":          true,
	"open":           true,
	"cutOff":         true,
	"shiftWall":      true,
	"stopWall":       true,
	"startWall":      true,
	"stopMonotonic":  true,
	"startMonotonic": true,
	"pauseClocks":    true,
	"resumeClocks":   true,
	// The faults that the visitor switches on and off, each for one instance.
	"hangNetwork":       true,
	"disconnectNetwork": true,
	"failDisk":          true,
	"loseAnswer":        false,
	"failRequest":       false,
	"outage":            false,
	"calm":              false,
	"workload":          false,
}

// purposes of a request to the store, as the trace names them, which a lost answer or a failed
// request can be for. "any" is every request.
var purposes = []string{"any", "read", "take", "commit", "renew", "release", "upload", "download"}

// Limits of a request, so that a run stays small enough for a page.
const (
	maxEnd       = 30 * 60 * 1000
	maxInstances = 5
	maxCallers   = 8
	maxReaders   = 8
	maxSchedule  = 1000
	// maxKeep rows that the writes keep in the table.
	maxKeep = 10_000
)

// defaultRequest has the default of every field.
func defaultRequest() Request {
	return Request{
		Seed:      1,
		End:       120_000,
		Instances: 3,
		Leases:    []float64{30_000},
		Readers:   2,
		Latency:   50,
		Workload:  Workload{On: true, Callers: 2, Think: 5_000, Writes: 40, Timeout: 30_000},
	}
}

// ParseRequest from its JSON, with the defaults for missing fields, and check it. A member that is
// not a field of the request is an error.
func ParseRequest(b []byte) (Request, error) {
	r := defaultRequest()
	// A list that is given replaces the default: encoding/json/v2 empties a slice before it decodes
	// an array into it.
	if err := json.Unmarshal(b, &r, json.RejectUnknownMembers(true)); err != nil {
		return Request{}, fmt.Errorf("the request is not JSON of a request: %w", err)
	}
	if err := r.check(); err != nil {
		return Request{}, err
	}
	return r, nil
}

// check the request against its limits, and that its schedule makes sense.
func (r Request) check() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !positive(r.End) || r.End > maxEnd {
		bad("end is %v, and must be above 0 and at most %v", r.End, maxEnd)
	}
	if r.Instances < 1 || r.Instances > maxInstances {
		bad("instances is %v, and must be from 1 to %v", r.Instances, maxInstances)
	}
	if len(r.Leases) == 0 {
		bad("leases is empty")
	}
	for _, l := range r.Leases {
		if l < 1000 || l > 10*60*1000 || !finite(l) {
			bad("a lease of %v must be from 1000 to 600000", l)
		}
	}
	if r.Readers < 1 || r.Readers > maxReaders {
		bad("readers is %v, and must be from 1 to %v", r.Readers, maxReaders)
	}
	if !nonNegative(r.Latency) || r.Latency > 60_000 {
		bad("latency is %v, and must be from 0 to 60000", r.Latency)
	}
	for _, p := range r.Faults.probabilities() {
		if !finite(p.v) || p.v < 0 || p.v > 1 {
			bad("faults.%v is %v, and must be from 0 to 1", p.name, p.v)
		}
	}
	for _, d := range r.Faults.durations() {
		if !nonNegative(d.v) || d.v > maxEnd {
			bad("faults.%v is %v, and must be from 0 to %v", d.name, d.v, maxEnd)
		}
	}
	w := r.Workload
	if w.Callers < 0 || w.Callers > maxCallers {
		bad("workload.callers is %v, and must be from 0 to %v", w.Callers, maxCallers)
	}
	if !nonNegative(w.Think) || w.Think > maxEnd {
		bad("workload.think is %v, and must be from 0 to %v", w.Think, maxEnd)
	}
	if w.Writes < 0 || w.Writes > 100 {
		bad("workload.writes is %v, and must be from 0 to 100", w.Writes)
	}
	if !positive(w.Timeout) || w.Timeout > maxEnd {
		bad("workload.timeout is %v, and must be above 0 and at most %v", w.Timeout, maxEnd)
	}
	if w.Keep < 0 || w.Keep > maxKeep {
		bad("workload.keep is %v, and must be from 0 to %v", w.Keep, maxKeep)
	}
	if len(r.Schedule) > maxSchedule {
		bad("the schedule has %v actions, and may have at most %v", len(r.Schedule), maxSchedule)
	}
	for i, a := range r.Schedule {
		if !nonNegative(a.At) || a.At >= r.End {
			bad("schedule[%v]: %v at %v, which is not from 0 to before the end at %v", i, a.Do, a.At, r.End)
			continue
		}
		if err := a.check(r.Instances); err != nil {
			bad("schedule[%v]: %w", i, err)
		}
	}
	return errors.Join(errs...)
}

// check the action for a run with the given number of instances, apart from its time.
func (a Action) check(instances int) error {
	needsInstance, ok := actions[a.Do]
	switch {
	case !ok:
		return fmt.Errorf("no action is called %q", a.Do)
	case needsInstance && !validInstance(a.Instance, instances):
		return fmt.Errorf("%v needs an instance from a to %v, and has %q", a.Do, instanceName(instances-1), a.Instance)
	case !needsInstance && a.Instance != "" && a.Do != "loseAnswer" && a.Do != "failRequest":
		return fmt.Errorf("%v is for no instance, and has %q", a.Do, a.Instance)
	case a.Instance != "" && !validInstance(a.Instance, instances):
		return fmt.Errorf("%v is for an instance from a to %v, and has %q", a.Do, instanceName(instances-1), a.Instance)
	}
	switch a.Do {
	case "cutOff", "outage":
		if !positive(a.For) || a.For > maxEnd {
			return fmt.Errorf("%v needs for above 0 and at most %v, and has %v", a.Do, maxEnd, a.For)
		}
	case "shiftWall":
		if a.By == 0 || !finite(a.By) || math.Abs(a.By) > 24*60*60*1000 {
			return fmt.Errorf("shiftWall needs by, not 0, of at most a day either way, and has %v", a.By)
		}
	case "loseAnswer", "failRequest":
		if !slices.Contains(purposes, a.Request) {
			return fmt.Errorf("%v needs request, one of %v, and has %q", a.Do, purposes, a.Request)
		}
	}
	return nil
}

func validInstance(name string, instances int) bool {
	for i := range instances {
		if name == instanceName(i) {
			return true
		}
	}
	return false
}

// instanceName of the instance with index i: a, b, c, and so on.
func instanceName(i int) string {
	return string(rune('a' + i))
}

func finite(f float64) bool      { return !math.IsInf(f, 0) && !math.IsNaN(f) }
func positive(f float64) bool    { return finite(f) && f > 0 }
func nonNegative(f float64) bool { return finite(f) && f >= 0 }

// named number of a request, for the errors of its check in a fixed order.
type named struct {
	name string
	v    float64
}

func (f Faults) probabilities() []named {
	return []named{
		{"lostRequest", f.LostRequest}, {"lostResponse", f.LostResponse}, {"hang", f.Hang}, {"slow", f.Slow},
		{"brokenRead", f.BrokenRead}, {"cutOff", f.CutOff}, {"outage", f.Outage}, {"conflict", f.Conflict},
		{"late", f.Late}, {"afterCrash", f.AfterCrash}, {"long", f.Long}, {"damaged", f.Damaged},
	}
}

func (f Faults) durations() []named {
	return []named{{"slowBy", f.SlowBy}, {"cutOffFor", f.CutOffFor}, {"outageFor", f.OutageFor}, {"lateBy", f.LateBy}, {"longBy", f.LongBy}}
}

// store faults of simstore.
func (f Faults) store() simstore.Faults {
	return simstore.Faults{
		LostRequest: f.LostRequest, LostResponse: f.LostResponse, Hang: f.Hang,
		Slow: f.Slow, SlowBy: duration(f.SlowBy), BrokenRead: f.BrokenRead,
		CutOff: f.CutOff, CutOffFor: duration(f.CutOffFor), Outage: f.Outage, OutageFor: duration(f.OutageFor),
		Conflict: f.Conflict, Late: f.Late, AfterCrash: f.AfterCrash, LateBy: duration(f.LateBy),
		Long: f.Long, LongBy: duration(f.LongBy),
	}
}

// duration of ms milliseconds, rounded to the nanosecond.
func duration(ms float64) time.Duration {
	return time.Duration(math.Round(ms * 1e6))
}

// millis of d, the unit of every time and duration of a request and its trace.
func millis(d time.Duration) float64 {
	return float64(d) / 1e6
}

// Session is what a live session is started with: the fields of a [Request] that hold for the
// whole run. A session has no end and no schedule, and sends no text trace of the scheduler. A field
// that is missing has the default that [defaultSession] gives it.
type Session struct {
	Seed      uint64    `json:"seed"`
	Instances int       `json:"instances"`
	Leases    []float64 `json:"leases"`
	Readers   int       `json:"readers"`
	Latency   float64   `json:"latency"`
	Faults    Faults    `json:"faults"`
	Buggify   bool      `json:"buggify"`
	Workload  Workload  `json:"workload"`
}

// defaultSession has the default of every field: those of [defaultRequest], but two instances, and
// writes that keep the last 100 rows, so that a read costs the same however long the session runs.
func defaultSession() Session {
	r := defaultRequest()
	r.Workload.Keep = 100
	return Session{Seed: r.Seed, Instances: 2, Leases: r.Leases, Readers: r.Readers, Latency: r.Latency, Workload: r.Workload}
}

// ParseSession from its JSON, with the defaults for missing fields, and check it, as
// [ParseRequest] does. It returns the settings as a request whose end is infinite and whose schedule
// is empty.
func ParseSession(b []byte) (Request, error) {
	s := defaultSession()
	if err := json.Unmarshal(b, &s, json.RejectUnknownMembers(true)); err != nil {
		return Request{}, fmt.Errorf("the session is not JSON of a session: %w", err)
	}
	r := Request{Seed: s.Seed, End: maxEnd, Instances: s.Instances, Leases: s.Leases, Readers: s.Readers,
		Latency: s.Latency, Faults: s.Faults, Buggify: s.Buggify, Workload: s.Workload}
	if err := r.check(); err != nil {
		return Request{}, err
	}
	r.End = math.Inf(1)
	return r, nil
}
