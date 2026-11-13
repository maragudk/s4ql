package core

import (
	"context"
	"log/slog"
	"time"
)

// The events that an instance logs: one for each unit of work, when the unit ends, with everything
// that is known about it as flat attributes.
//
// The level says how much an operator needs the event, so that a healthy instance that commits
// and renews logs nothing at the default level, Info, after it has started:
//
//   - Debug: a commit that succeeded, also one of nothing; a renewal of the lease that succeeded;
//     and a call that was refused for lack of a valid lease.
//   - Info: a change of state: the lease taken, lost, or released, a catch-up, and a close.
//   - Warn: a commit that failed or whose outcome is unknown, a commit whose cache then failed, a
//     swap of unknown outcome and how it was settled, a renewal, a take, or a release of the lease
//     that failed, a catch-up that failed, and "busy" of the cache after the busy timeout.
//   - Error: a stop for good for any cause but a close, with the cause.
//
// An event of the call of a caller carries the caller's context. An event of the lease task
// carries the context that [Open] was given, without its cancellation, and an event of [DB.Close]
// the context of Close.
const (
	// eventCommit is one call of [Tx.Commit].
	eventCommit = "commit"
	// eventLease is one change of the lease as the instance knows it: see [attrTransition].
	eventLease = "lease"
	// eventCatchUp is one run of applying the commit objects that the cache lacks.
	eventCatchUp = "catch_up"
	// eventRefusal is one read or write that was refused for lack of a valid lease.
	eventRefusal = "refusal"
	// eventUnknown is one swap of the manifest whose outcome was unknown, when it is known.
	eventUnknown = "unknown_outcome"
	// eventBusy is one "busy" of the cache after the busy timeout.
	eventBusy = "busy"
	// eventStop is the stop of the instance for good.
	eventStop = "stop"
)

// The names of the attributes: lowercase, with dots for namespaces and underscores inside words,
// and one name for each concept in every event.
const (
	// attrPrefix is the key prefix of the database, and attrInstance the ID under which the instance
	// holds the lease. Every event has both.
	attrPrefix   = "db.prefix"
	attrInstance = "instance.id"

	// attrOutcome says how the unit of work ended, in a word.
	attrOutcome = "outcome"
	// attrError is the error of a unit of work that failed.
	attrError = "error"
	// attrOperation is what was being done: begin, read, commit, and so on.
	attrOperation = "operation"
	// attrReason is why a read or a write was refused.
	attrReason = "reason"
	// attrAttempts is the number of requests to the store that the unit of work sent.
	attrAttempts = "attempts"

	// attrTransition of the lease: taken, renewed, lost, or released.
	attrTransition = "lease.transition"
	// attrLeaseHolder is the holder that the manifest names, attrLeasePrevious the one that it named
	// before, and attrLeaseDuration how long the lease of this instance lasts.
	attrLeaseHolder   = "lease.holder"
	attrLeasePrevious = "lease.previous_holder"
	attrLeaseDuration = "lease.duration"

	// attrCommit is the name of a commit object, and attrParent the name of the one before it.
	attrCommit = "commit.name"
	attrParent = "commit.parent"
	// attrChanges is the number of tables and rows that changed, attrAnnotations the number of
	// annotations, and attrBytes the size of the commit object.
	attrChanges     = "commit.changes"
	attrAnnotations = "commit.annotations"
	attrBytes       = "commit.bytes"

	// attrCatchUpFrom and attrCatchUpTo are the positions of the cache before and after a catch-up,
	// and attrCatchUpCommits the number of commit objects that it applied.
	attrCatchUpFrom    = "catch_up.from"
	attrCatchUpTo      = "catch_up.to"
	attrCatchUpCommits = "catch_up.commits"

	// attrUnknownKind is what the swap with the unknown outcome was for: commit or renewal.
	attrUnknownKind = "unknown.kind"

	// The durations of the phases of a unit of work, on the monotonic clock of the host.
	attrDurationTotal  = "duration.total"
	attrDurationSpool  = "duration.spool"
	attrDurationUpload = "duration.upload"
	attrDurationSwap   = "duration.swap"
	attrDurationCache  = "duration.cache"
)

// event logs one unit of work, with the context of the call that did it.
func (d *DB) event(ctx context.Context, level slog.Level, name string, attrs ...slog.Attr) {
	d.log.LogAttrs(ctx, level, name, attrs...)
}

// enabled reports whether the logger takes events at the level, so that the attributes of an event
// that it would drop are not built.
func (d *DB) enabled(ctx context.Context, level slog.Level) bool {
	return d.log.Enabled(ctx, level)
}

// errAttr is the attribute for the error of a unit of work.
func errAttr(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.String(attrError, err.Error())
}

// stopwatch measures the phases of a unit of work on the monotonic clock of the host.
type stopwatch struct {
	d     *DB
	start time.Duration
	lap   time.Duration
}

func (d *DB) stopwatch() stopwatch {
	now := d.host.Monotonic()
	return stopwatch{d: d, start: now, lap: now}
}

// phase that ends now, as an attribute with the given name.
func (s *stopwatch) phase(name string) slog.Attr {
	now := s.d.host.Monotonic()
	a := slog.Duration(name, now-s.lap)
	s.lap = now
	return a
}

// total time since the start.
func (s *stopwatch) total() slog.Attr {
	return slog.Duration(attrDurationTotal, s.d.host.Monotonic()-s.start)
}
