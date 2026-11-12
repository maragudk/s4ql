package core

import (
	"context"
	"time"
)

// Plant the bug with the given name in o, which is a field of [bugs]. It panics for a name that is
// none.
func (o *Options) Plant(name string) {
	set, ok := plantable[name]
	if !ok {
		panic("core: no planted bug is named " + name)
	}
	set(&o.bugs)
}

// plantable bugs, by the names of the fields of [bugs].
var plantable = map[string]func(b *bugs){
	"renewOutsideLock":     func(b *bugs) { b.renewOutsideLock = true },
	"cacheFirst":           func(b *bugs) { b.cacheFirst = true },
	"wallOnly":             func(b *bugs) { b.wallOnly = true },
	"monotonicOnly":        func(b *bugs) { b.monotonicOnly = true },
	"takeoverAfterRelease": func(b *bugs) { b.takeoverAfterRelease = true },
	"keepSalt":             func(b *bugs) { b.keepSalt = true },
	"ownDuration":          func(b *bugs) { b.ownDuration = true },
	"lateVersion":          func(b *bugs) { b.lateVersion = true },
	"leaseFromAnswer":      func(b *bugs) { b.leaseFromAnswer = true },
	"ignoreCacheError":     func(b *bugs) { b.ignoreCacheError = true },
	"ackUnknown":           func(b *bugs) { b.ackUnknown = true },
	"settleByRead":         func(b *bugs) { b.settleByRead = true },
	"serveUnknown":         func(b *bugs) { b.serveUnknown = true },
	"lostMeansFailed":      func(b *bugs) { b.lostMeansFailed = true },
	"replayCommitsEarly":   func(b *bugs) { b.replayCommitsEarly = true },
	"keyTwice":             func(b *bugs) { b.keyTwice = true },
	"forgetDelete":         func(b *bugs) { b.forgetDelete = true },
	"skipUnreadable":       func(b *bugs) { b.skipUnreadable = true },
	"renewWaitsForWriter":  func(b *bugs) { b.renewWaitsForWriter = true },
	"unlockedCounter":      func(b *bugs) { b.unlockedCounter = true },
}

// Planted reports whether o has a planted bug.
func (o Options) Planted() bool {
	return o.bugs != bugs{}
}

// Unplanted is o without planted bugs.
func (o Options) Unplanted() Options {
	o.bugs = bugs{}
	return o
}

// SyncOff stops SQLite from syncing the cache to disk.
func (o *Options) SyncOff() {
	o.syncOff = true
}

// ID under which the instance holds the lease.
func (d *DB) ID() string {
	return d.id
}

// CloseCache closes the connections of an instance that a test has crashed, as the operating system
// closes the files of a process that has died.
func (d *DB) CloseCache() {
	d.closeCache()
}

// PauseUnlessChanged pauses as the lease task does, for the duration, with the count of changes of
// the state as it was before the last change if stale, and as it is now otherwise.
func (d *DB) PauseUnlessChanged(ctx context.Context, duration time.Duration, stale bool) error {
	d.mu.Lock()
	changes := d.changes
	d.mu.Unlock()
	if stale {
		changes--
	}
	return d.pause(ctx, duration, changes)
}

// Margin of the wait for a lease of the given duration.
func Margin(duration time.Duration) time.Duration {
	return margin(duration)
}

// Quote an identifier.
func Quote(name string) string {
	return quote(name)
}

// Classify an error from the cache: "local", "busy", or "other".
func Classify(err error) string {
	switch classify(err) {
	case kindLocal:
		return "local"
	case kindBusy:
		return "busy"
	}
	return "other"
}

// The names of the attributes and events, for the tests of the telemetry.
const (
	EventCommit  = eventCommit
	EventLease   = eventLease
	EventCatchUp = eventCatchUp
	EventRefusal = eventRefusal
	EventUnknown = eventUnknown
	EventBusy    = eventBusy
	EventStop    = eventStop

	AttrOutcome     = attrOutcome
	AttrTransition  = attrTransition
	AttrPrefix      = attrPrefix
	AttrInstance    = attrInstance
	AttrOperation   = attrOperation
	AttrReason      = attrReason
	AttrCommit      = attrCommit
	AttrUnknownKind = attrUnknownKind
)

// CacheVersion that this package makes.
const CacheVersion = cacheVersion
