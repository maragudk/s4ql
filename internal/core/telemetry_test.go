package core_test

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sync"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
)

// recorder is a handler that keeps the events of an instance, each as its attributes by name, with
// its name under "event".
type recorder struct {
	mu     sync.Mutex
	events []map[string]any
	attrs  []slog.Attr
	root   *recorder
}

func (r *recorder) logger() *slog.Logger {
	return slog.New(r)
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	root := r
	if r.root != nil {
		root = r.root
	}
	return &recorder{root: root, attrs: append(slices.Clone(r.attrs), attrs...)}
}

func (r *recorder) WithGroup(string) slog.Handler { return r }

func (r *recorder) Handle(_ context.Context, record slog.Record) error {
	event := map[string]any{"event": record.Message, "level": record.Level}
	for _, a := range r.attrs {
		event[a.Key] = a.Value.Any()
	}
	record.Attrs(func(a slog.Attr) bool {
		if a.Key != "" {
			event[a.Key] = a.Value.Any()
		}
		return true
	})
	root := r
	if r.root != nil {
		root = r.root
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	root.events = append(root.events, event)
	return nil
}

// named events, in the order in which they were logged.
func (r *recorder) named(name string) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var events []map[string]any
	for _, event := range r.events {
		if event["event"] == name {
			events = append(events, event)
		}
	}
	return events
}

// with the given value of an attribute, among the events of a name.
func (r *recorder) with(name, attr string, value any) []map[string]any {
	var events []map[string]any
	for _, event := range r.named(name) {
		if event[attr] == value {
			events = append(events, event)
		}
	}
	return events
}

// at is the value of the attribute of the event with the given name, as text.
func at(event map[string]any, name string) string {
	return fmt.Sprint(event[name])
}

// all events.
func (r *recorder) all() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func TestEvents(t *testing.T) {
	t.Run("should log one event for each unit of work, with its outcome and what is known about it", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			events := &recorder{}
			s := newScriptStore(w.client())
			d := w.openOn(t, "a", s, core.Options{Logger: events.logger()})

			// A refusal for lack of the lease, before the instance has it.
			s.on("get", "manifest", hang, 1)
			is.Error(t, core.ErrNoLease, readErr(t, d))
			refusals := events.named(core.EventRefusal)
			is.Equal(t, 1, len(refusals))
			is.Equal(t, fmt.Sprint("refused"), at(refusals[0], core.AttrOutcome))
			is.Equal(t, fmt.Sprint("read"), at(refusals[0], core.AttrOperation))
			is.Equal(t, fmt.Sprint("not_holder"), at(refusals[0], core.AttrReason))

			// The lease is taken.
			is.NotError(t, d.Wait(t.Context()))
			taken := events.with(core.EventLease, core.AttrTransition, "taken")
			is.Equal(t, 1, len(taken))
			is.Equal(t, fmt.Sprint("taken"), at(taken[0], core.AttrOutcome))
			is.Equal(t, fmt.Sprint(d.ID()), at(taken[0], "lease.holder"))
			is.Equal(t, fmt.Sprint(30*time.Second), at(taken[0], "lease.duration"))

			// A commit, with the name of its commit object, its size, and the time of each phase.
			transact(t, d, create("t1", 1, "k"), upsert("t1", 1), upsert("t1", 2), note("insert"))
			commits := events.named(core.EventCommit)
			is.Equal(t, 1, len(commits))
			is.Equal(t, fmt.Sprint("committed"), at(commits[0], core.AttrOutcome))
			is.Equal(t, fmt.Sprint(w.manifest(t).Head.String()), at(commits[0], core.AttrCommit))
			is.Equal(t, fmt.Sprint("0000000000000000000000000000000000000000000000000000000000000000"), at(commits[0], "commit.parent"))
			is.Equal(t, fmt.Sprint(int64(3)), at(commits[0], "commit.changes"))
			is.Equal(t, fmt.Sprint(int64(1)), at(commits[0], "commit.annotations"))
			is.Equal(t, fmt.Sprint(int64(2)), at(commits[0], "attempts"))
			is.True(t, commits[0]["commit.bytes"].(int64) > 0)
			for _, phase := range []string{"duration.total", "duration.spool", "duration.upload", "duration.swap", "duration.cache"} {
				_, ok := commits[0][phase].(time.Duration)
				is.True(t, ok, phase)
			}

			// A commit of nothing, and a commit that is refused.
			transact(t, d)
			is.Equal(t, 1, len(events.with(core.EventCommit, core.AttrOutcome, "empty")))

			// The lease is renewed.
			time.Sleep(21 * time.Second)
			renewed := events.with(core.EventLease, core.AttrTransition, "renewed")
			is.Equal(t, 1, len(renewed))
			is.Equal(t, fmt.Sprint("renewed"), at(renewed[0], core.AttrOutcome))
			is.Equal(t, fmt.Sprint(int64(1)), at(renewed[0], "attempts"))

			// A renewal whose outcome is unknown, and how it was settled.
			s.on("swap", "manifest", lose, 1)
			time.Sleep(22 * time.Second)
			is.Equal(t, 1, len(events.with(core.EventLease, core.AttrOutcome, "unknown")))
			unknown := events.named(core.EventUnknown)
			is.Equal(t, 1, len(unknown))
			is.Equal(t, fmt.Sprint("renewal"), at(unknown[0], core.AttrUnknownKind))
			is.Equal(t, fmt.Sprint("renewed"), at(unknown[0], core.AttrOutcome))
			is.Equal(t, fmt.Sprint(int64(2)), at(unknown[0], "attempts"))

			// A commit whose outcome is unknown when its caller gives up, and how it was settled.
			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			upsert("t1", 3)(t, tx)
			s.on("swap", "manifest", stick, 1)
			s.on("swap", "", drop, -1)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			is.Error(t, core.ErrOutcomeUnknown, tx.Commit(ctx))
			cancel()
			is.Equal(t, 1, len(events.with(core.EventCommit, core.AttrOutcome, "unknown")))
			is.Error(t, core.ErrNoLease, readErr(t, d))
			is.Equal(t, 1, len(events.with(core.EventRefusal, core.AttrReason, "outcome_unknown")))
			s.clear()
			is.NotError(t, d.Wait(t.Context()))
			settled := events.with(core.EventUnknown, core.AttrUnknownKind, "commit")
			is.Equal(t, 1, len(settled))
			is.Equal(t, fmt.Sprint("not_committed"), at(settled[0], core.AttrOutcome))
			is.True(t, settled[0][core.AttrCommit] != nil)

			// The lease is lost to another instance, which catches up.
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)
			other := &recorder{}
			b := w.serve(t, "b", core.Options{Logger: other.logger()})
			caught := other.named(core.EventCatchUp)
			is.Equal(t, 1, len(caught))
			is.Equal(t, fmt.Sprint("caught_up"), at(caught[0], core.AttrOutcome))
			is.Equal(t, fmt.Sprint(int64(1)), at(caught[0], "catch_up.commits"))
			is.Equal(t, fmt.Sprint(w.manifest(t).Head.String()), at(caught[0], "catch_up.to"))
			is.Equal(t, fmt.Sprint(d.ID()), at(other.with(core.EventLease, core.AttrTransition, "taken")[0], "lease.previous_holder"))
			s.clear()
			time.Sleep(6 * time.Second)
			lost := events.with(core.EventLease, core.AttrTransition, "lost")
			is.Equal(t, 1, len(lost))
			is.Equal(t, fmt.Sprint("lost"), at(lost[0], core.AttrOutcome))
			is.Equal(t, fmt.Sprint(b.ID()), at(lost[0], "lease.holder"))

			// The lease is released, and the instance stops.
			is.NotError(t, b.Close(t.Context()))
			released := other.with(core.EventLease, core.AttrTransition, "released")
			is.Equal(t, 1, len(released))
			is.Equal(t, fmt.Sprint("released"), at(released[0], core.AttrOutcome))
			stops := other.named(core.EventStop)
			is.Equal(t, 1, len(stops))
			is.Equal(t, fmt.Sprint("closed"), at(stops[0], core.AttrOutcome))

			// Every event names the database and the instance, and every name is lowercase with dots
			// and underscores.
			name := regexp.MustCompile(`^[a-z_]+(\.[a-z_]+)*$`)
			for _, event := range slices.Concat(events.all(), other.all()) {
				is.Equal(t, "db", at(event, core.AttrPrefix))
				is.True(t, at(event, core.AttrInstance) == d.ID() || at(event, core.AttrInstance) == b.ID())
				is.True(t, event[core.AttrOutcome] != nil, fmt.Sprint(event))
				for attr := range event {
					is.True(t, name.MatchString(attr), attr)
				}
			}
		})
	})

	t.Run("should log nothing and work when no logger is given", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"))
		is.Equal(t, "t1(k*0): ", read(t, d))
	})
}
