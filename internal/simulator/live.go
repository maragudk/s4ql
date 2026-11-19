//go:build s4ql_browser

package simulator

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"math"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/simulator/handoff"
)

// Live is the answer to a command of a live session. Every answer has the events of the session
// since the answer before, so the answers together have every event of the session, in order.
type Live struct {
	// Error says why the command was refused, and is the only field then. The session goes on.
	Error string `json:"error,omitzero"`
	// T is the fake time of the session when the command was done.
	T      float64 `json:"t"`
	Events []Event `json:"events"`
	// Steps of the scheduler so far.
	Steps int `json:"steps"`
	// State is the state of the session, in the answer to "now".
	State *State `json:"state,omitzero"`
	// Ended is set on the last answer of a session: the answer to "end", or to the command during
	// which the scheduler failed. It has the fields from here on, as a [Response] has them.
	Ended          bool     `json:"ended,omitzero"`
	Summary        *Summary `json:"summary,omitzero"`
	Hash           string   `json:"hash,omitzero"`
	SchedulerError string   `json:"schedulerError,omitzero"`
}

// session of a live run. Its controller is the first task of the run, which does one command at a
// time and answers it, and then waits for the next on the line. The line is made outside the bubble
// of the run, so that a wait on it is not durable: while the controller waits for a command, no other
// task runs, and fake time stands still, however long the wait.
type session struct {
	// instances of the run, for the check of an action.
	instances int
	line      *handoff.Line[command, answer]
}

// command of a session, for its controller.
type command struct {
	kind string
	// by is how far an advance moves fake time, or to the time that it moves it to if by is
	// negative.
	by, to time.Duration
	// action of an act.
	action Action
	// commits that the state of a now lists at most, if above 0.
	commits int
}

// answer of the controller, as JSON. final is set on the last answer of the session.
type answer struct {
	body  []byte
	final bool
}

// startSession with the settings in req, and return it with the answer to its start, which has the
// events up to the first wait of the controller, which first runs after the delay that the
// scheduler gives the start of a task. A goroutine of its own runs the
// session in a bubble until it ends.
func startSession(t *testing.T, req Request) (*session, answer) {
	disks, err := takeDisks(req.Instances)
	if err != nil {
		return nil, answer{body: refused("the disks of the instances cannot be made: " + err.Error())}
	}
	s := &session{instances: req.Instances, line: handoff.New[command, answer]()}
	first := s.line.Start(func() {
		var last Live
		synctest.Test(t, func(t *testing.T) {
			world := sim.New(req.Seed, sim.Options{Jitter: 50 * time.Millisecond, Buggify: sim.Buggify{On: req.Buggify}, MaxSteps: math.MaxInt})
			r := newRun(t, req, world, math.MaxInt64, disks, true)
			r.session = s
			var res Response
			r.ends(world.Run(r.serve), &res)
			last = Live{T: millis(r.endedAt), Events: nonNil(r.rec.take()), Steps: res.Steps, Ended: true,
				Summary: res.Summary, Hash: res.Hash, SchedulerError: res.SchedulerError}
			if last.SchedulerError != "" {
				last.T = millis(world.Monotonic())
			}
		})
		putDisks(disks)
		s.line.Last(answer{body: marshal(last), final: true})
	})
	return s, first
}

// do the command c in the session, and return the answer.
func (s *session) do(c command) answer {
	return s.line.Ask(c)
}

// serve is the first task of a live session, its controller. It starts the instances, answers, and
// then does each command as it comes and answers it, until the command "end". It does what the first
// task of a batch run does, in the same order: an advance sleeps as the batch run sleeps until the
// time of its next action, and an action is done as the batch run does it.
func (r *run) serve(ctx context.Context) {
	r.setUp(ctx)
	s := r.session
	var state *State
	var refusal string
	for {
		a := answer{body: refused(refusal)}
		if refusal == "" {
			a = answer{body: marshal(Live{T: millis(r.world.Monotonic()), Events: nonNil(r.rec.take()), Steps: r.world.Steps(), State: state})}
		}
		c := s.line.Answer(a)
		state, refusal = nil, ""
		switch c.kind {
		case "advance":
			// An advance to a time sleeps until it, as a batch run sleeps until the time of its next
			// action.
			to := c.to
			if c.by >= 0 {
				to = r.world.Monotonic() + c.by
			}
			if to-r.world.Monotonic() > duration(maxEnd) {
				refusal = fmt.Sprintf("to is %v, more than %v after the time of the session", millis(to), maxEnd)
				break
			}
			r.sleepUntil(ctx, to)
		case "act":
			r.act(ctx, r.actions, c.action)
			r.actions++
		case "now":
			state = new(r.rec.state(c.commits))
		case "end":
			r.endedAt = r.world.Monotonic()
			r.end(ctx)
			return
		}
	}
}

func nonNil(events []Event) []Event {
	if events == nil {
		return []Event{}
	}
	return events
}

// parseCommand of the kind with its body as JSON, for the session s.
func (s *session) parseCommand(kind string, body []byte) (command, error) {
	if len(body) == 0 {
		body = []byte("{}")
	}
	c := command{kind: kind}
	switch kind {
	case "advance":
		var q struct {
			By *float64 `json:"by"`
			To *float64 `json:"to"`
		}
		if err := json.Unmarshal(body, &q, json.RejectUnknownMembers(true)); err != nil {
			return command{}, fmt.Errorf("the message is not JSON of an advance: %w", err)
		}
		switch {
		case (q.By == nil) == (q.To == nil):
			return command{}, fmt.Errorf("an advance has either by or to")
		case q.By != nil && (!nonNegative(*q.By) || *q.By > maxEnd):
			return command{}, fmt.Errorf("by is %v, and must be from 0 to %v", *q.By, maxEnd)
		case q.By != nil:
			c.by = duration(*q.By)
		case !nonNegative(*q.To) || *q.To > math.MaxInt64/1e6:
			return command{}, fmt.Errorf("to is %v, and must be a time from 0 on", *q.To)
		default:
			c.by, c.to = -1, duration(*q.To)
		}
	case "act":
		if err := json.Unmarshal(body, &c.action, json.RejectUnknownMembers(true)); err != nil {
			return command{}, fmt.Errorf("the message is not JSON of an action: %w", err)
		}
		if c.action.At != 0 {
			return command{}, fmt.Errorf("an action of a session has no at, and is done at the time of the session")
		}
		if err := c.action.check(s.instances); err != nil {
			return command{}, err
		}
	case "now":
		var q struct {
			Commits int `json:"commits"`
		}
		if err := json.Unmarshal(body, &q, json.RejectUnknownMembers(true)); err != nil {
			return command{}, fmt.Errorf("the message is not JSON of a state: %w", err)
		}
		if q.Commits < 0 {
			return command{}, fmt.Errorf("commits is %v, and must be 0 or above", q.Commits)
		}
		c.commits = q.Commits
	case "end":
		var q struct{}
		if err := json.Unmarshal(body, &q, json.RejectUnknownMembers(true)); err != nil {
			return command{}, fmt.Errorf("the message is not JSON of an end: %w", err)
		}
	}
	return c, nil
}
