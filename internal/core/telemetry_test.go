package core_test

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
)

// recorder is a handler that keeps the events of an instance, each as its attributes by name, with
// its name under "event", its level under "level", and the value of [callerKey] in its context, if
// there is one, under "caller".
type recorder struct {
	// minimum level of the events that it takes, or nil for every level.
	minimum slog.Leveler

	mu     sync.Mutex
	events []map[string]any
	attrs  []slog.Attr
	root   *recorder
}

// callerKey is the key of a value that a test puts into the context of a call.
type callerKey struct{}

func (r *recorder) logger() *slog.Logger {
	return slog.New(r)
}

func (r *recorder) rootOf() *recorder {
	if r.root != nil {
		return r.root
	}
	return r
}

func (r *recorder) Enabled(_ context.Context, level slog.Level) bool {
	minimum := r.rootOf().minimum
	return minimum == nil || level >= minimum.Level()
}

func (r *recorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recorder{root: r.rootOf(), attrs: append(slices.Clone(r.attrs), attrs...)}
}

func (r *recorder) WithGroup(string) slog.Handler { return r }

func (r *recorder) Handle(ctx context.Context, record slog.Record) error {
	event := map[string]any{"event": record.Message, "level": record.Level}
	if caller := ctx.Value(callerKey{}); caller != nil {
		event["caller"] = caller
	}
	for _, a := range r.attrs {
		event[a.Key] = a.Value.Any()
	}
	record.Attrs(func(a slog.Attr) bool {
		if a.Key != "" {
			event[a.Key] = a.Value.Any()
		}
		return true
	})
	root := r.rootOf()
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

// levelOf the event.
func levelOf(event map[string]any) slog.Level {
	level, _ := event["level"].(slog.Level)
	return level
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
			// A refusal for lack of the lease, before the instance has it. The store hangs before the
			// instance opens, so that the lease task cannot read the manifest first.
			s.on("get", "manifest", hang, 1)
			d := w.openOn(t, "a", s, core.Options{Logger: events.logger()})
			is.Error(t, core.ErrNoLease, readErr(t, d))
			refusals := events.named(core.EventRefusal)
			is.Equal(t, 1, len(refusals))
			is.Equal(t, "refused", at(refusals[0], core.AttrOutcome))
			is.Equal(t, "read", at(refusals[0], core.AttrOperation))
			is.Equal(t, "not_holder", at(refusals[0], core.AttrReason))
			is.Equal(t, slog.LevelDebug, levelOf(refusals[0]))

			// The lease is taken.
			is.NotError(t, d.Wait(t.Context()))
			taken := events.with(core.EventLease, core.AttrTransition, "taken")
			is.Equal(t, 1, len(taken))
			is.Equal(t, "taken", at(taken[0], core.AttrOutcome))
			is.Equal(t, fmt.Sprint(d.ID()), at(taken[0], "lease.holder"))
			is.Equal(t, fmt.Sprint(30*time.Second), at(taken[0], "lease.duration"))
			is.Equal(t, slog.LevelInfo, levelOf(taken[0]))

			// A commit, with the name of its commit object, its size, and the time of each phase.
			transact(t, d, create("t1", 1, "k"), upsert("t1", 1), upsert("t1", 2), note("insert"))
			commits := events.named(core.EventCommit)
			is.Equal(t, 1, len(commits))
			is.Equal(t, "committed", at(commits[0], core.AttrOutcome))
			is.Equal(t, slog.LevelDebug, levelOf(commits[0]))
			is.Equal(t, fmt.Sprint(w.manifest(t).Head.String()), at(commits[0], core.AttrCommit))
			is.Equal(t, "0000000000000000000000000000000000000000000000000000000000000000", at(commits[0], "commit.parent"))
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
			empty := events.with(core.EventCommit, core.AttrOutcome, "empty")
			is.Equal(t, 1, len(empty))
			is.Equal(t, slog.LevelDebug, levelOf(empty[0]))

			// The lease is renewed.
			time.Sleep(21 * time.Second)
			renewed := events.with(core.EventLease, core.AttrTransition, "renewed")
			is.Equal(t, 1, len(renewed))
			is.Equal(t, "renewed", at(renewed[0], core.AttrOutcome))
			is.Equal(t, fmt.Sprint(int64(1)), at(renewed[0], "attempts"))
			is.Equal(t, slog.LevelDebug, levelOf(renewed[0]))

			// A renewal whose outcome is unknown, and how it was settled.
			s.on("swap", "manifest", lose, 1)
			time.Sleep(22 * time.Second)
			unknownRenewals := events.with(core.EventLease, core.AttrOutcome, "unknown")
			is.Equal(t, 1, len(unknownRenewals))
			is.Equal(t, slog.LevelWarn, levelOf(unknownRenewals[0]))
			unknown := events.named(core.EventUnknown)
			is.Equal(t, 1, len(unknown))
			is.Equal(t, slog.LevelWarn, levelOf(unknown[0]))
			is.Equal(t, "renewal", at(unknown[0], core.AttrUnknownKind))
			is.Equal(t, "renewed", at(unknown[0], core.AttrOutcome))
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
			unknownCommits := events.with(core.EventCommit, core.AttrOutcome, "unknown")
			is.Equal(t, 1, len(unknownCommits))
			is.Equal(t, slog.LevelWarn, levelOf(unknownCommits[0]))
			is.Error(t, core.ErrNoLease, readErr(t, d))
			is.Equal(t, 1, len(events.with(core.EventRefusal, core.AttrReason, "outcome_unknown")))
			s.clear()
			is.NotError(t, d.Wait(t.Context()))
			settled := events.with(core.EventUnknown, core.AttrUnknownKind, "commit")
			is.Equal(t, 1, len(settled))
			is.Equal(t, "not_committed", at(settled[0], core.AttrOutcome))
			is.Equal(t, slog.LevelWarn, levelOf(settled[0]))
			is.True(t, settled[0][core.AttrCommit] != nil)

			// The lease is lost to another instance, which catches up.
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)
			other := &recorder{}
			b := w.serve(t, "b", core.Options{Logger: other.logger()})
			caught := other.named(core.EventCatchUp)
			is.Equal(t, 1, len(caught))
			is.Equal(t, "caught_up", at(caught[0], core.AttrOutcome))
			is.Equal(t, slog.LevelInfo, levelOf(caught[0]))
			is.Equal(t, fmt.Sprint(int64(1)), at(caught[0], "catch_up.commits"))
			is.Equal(t, fmt.Sprint(w.manifest(t).Head.String()), at(caught[0], "catch_up.to"))
			is.Equal(t, fmt.Sprint(d.ID()), at(other.with(core.EventLease, core.AttrTransition, "taken")[0], "lease.previous_holder"))
			s.clear()
			time.Sleep(6 * time.Second)
			lost := events.with(core.EventLease, core.AttrTransition, "lost")
			is.Equal(t, 1, len(lost))
			is.Equal(t, "lost", at(lost[0], core.AttrOutcome))
			is.Equal(t, slog.LevelInfo, levelOf(lost[0]))
			is.Equal(t, fmt.Sprint(b.ID()), at(lost[0], "lease.holder"))

			// The lease is released, and the instance stops.
			is.NotError(t, b.Close(t.Context()))
			released := other.with(core.EventLease, core.AttrTransition, "released")
			is.Equal(t, 1, len(released))
			is.Equal(t, "released", at(released[0], core.AttrOutcome))
			is.Equal(t, slog.LevelInfo, levelOf(released[0]))
			stops := other.named(core.EventStop)
			is.Equal(t, 1, len(stops))
			is.Equal(t, "closed", at(stops[0], core.AttrOutcome))
			is.Equal(t, slog.LevelInfo, levelOf(stops[0]))

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

	t.Run("should log nothing at the level Info after it has started, while it is healthy, commits, and renews", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			events := &recorder{minimum: slog.LevelInfo}
			d := w.open(t, "a", core.Options{Logger: events.logger()})
			is.NotError(t, d.Wait(t.Context()))
			is.Equal(t, 1, len(events.with(core.EventLease, core.AttrTransition, "taken")))
			started := len(events.all())

			transact(t, d, create("t1", 1, "k"), upsert("t1", 1))
			for i := range 10 {
				transact(t, d, upsert("t1", i))
				transact(t, d)
				_ = read(t, d)
				// The lease task renews the lease 20 seconds after the last commit.
				time.Sleep(21 * time.Second)
			}
			is.Equal(t, started, len(events.all()), fmt.Sprint(events.all()[started:]))

			// The same instance logs the same work at the level Debug.
			debug := &recorder{minimum: slog.LevelDebug}
			b := w.open(t, "b", core.Options{Logger: debug.logger()})
			is.NotError(t, d.Close(t.Context()))
			is.NotError(t, b.Wait(t.Context()))
			transact(t, b, upsert("t1", 1))
			time.Sleep(25 * time.Second)
			is.Equal(t, 1, len(debug.with(core.EventCommit, core.AttrOutcome, "committed")))
			is.True(t, len(debug.with(core.EventLease, core.AttrTransition, "renewed")) > 0, "no renewal is logged at the level Debug")
		})
	})

	t.Run("should log the events of a call with the context of its caller, and those of the lease task with the context of Open", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			events := &recorder{}
			s := newScriptStore(w.client())
			s.on("get", "manifest", hang, 1)
			opened := context.WithValue(t.Context(), callerKey{}, "open")
			d, err := core.Open(opened, w.host, s, filepath.Join(w.dir, "a.db"), "db", core.Options{Logger: events.logger()})
			is.NotError(t, err)

			_, err = d.Read(context.WithValue(t.Context(), callerKey{}, "read"))
			is.Error(t, core.ErrNoLease, err)
			_, err = d.Begin(context.WithValue(t.Context(), callerKey{}, "begin"))
			is.Error(t, core.ErrNoLease, err)
			refusals := events.named(core.EventRefusal)
			is.Equal(t, 2, len(refusals))
			is.Equal(t, "read", at(refusals[0], "caller"))
			is.Equal(t, "begin", at(refusals[1], "caller"))

			is.NotError(t, d.Wait(t.Context()))
			is.Equal(t, "open", at(events.with(core.EventLease, core.AttrTransition, "taken")[0], "caller"))

			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			create("t1", 1, "k")(t, tx)
			is.NotError(t, tx.Commit(context.WithValue(t.Context(), callerKey{}, "commit")))
			commits := events.named(core.EventCommit)
			is.Equal(t, 1, len(commits))
			is.Equal(t, "commit", at(commits[0], "caller"))

			is.NotError(t, d.Close(context.WithValue(t.Context(), callerKey{}, "close")))
			is.Equal(t, "close", at(events.named(core.EventStop)[0], "caller"))
		})
	})

	t.Run("should log nothing and work when no logger is given", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"))
		is.Equal(t, "t1(k*0): ", read(t, d))
	})
}
