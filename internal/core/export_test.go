package core

import (
	"context"
	"os"
	"time"

	"maragu.dev/s4ql/internal/sqlite"
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
	"renewOutsideLock":      func(b *bugs) { b.renewOutsideLock = true },
	"cacheFirst":            func(b *bugs) { b.cacheFirst = true },
	"wallOnly":              func(b *bugs) { b.wallOnly = true },
	"monotonicOnly":         func(b *bugs) { b.monotonicOnly = true },
	"takeoverAfterRelease":  func(b *bugs) { b.takeoverAfterRelease = true },
	"keepSalt":              func(b *bugs) { b.keepSalt = true },
	"ownDuration":           func(b *bugs) { b.ownDuration = true },
	"lateVersion":           func(b *bugs) { b.lateVersion = true },
	"leaseFromAnswer":       func(b *bugs) { b.leaseFromAnswer = true },
	"ignoreCacheError":      func(b *bugs) { b.ignoreCacheError = true },
	"ignoreCheckpointError": func(b *bugs) { b.ignoreCheckpointError = true },
	"ackUnknown":            func(b *bugs) { b.ackUnknown = true },
	"settleByRead":          func(b *bugs) { b.settleByRead = true },
	"serveUnknown":          func(b *bugs) { b.serveUnknown = true },
	"lostMeansFailed":       func(b *bugs) { b.lostMeansFailed = true },
	"replayCommitsEarly":    func(b *bugs) { b.replayCommitsEarly = true },
	"keyTwice":              func(b *bugs) { b.keyTwice = true },
	"forgetDelete":          func(b *bugs) { b.forgetDelete = true },
	"blobCut":               func(b *bugs) { b.blobCut = true },
	"nullZero":              func(b *bugs) { b.nullZero = true },
	"skipUnreadable":        func(b *bugs) { b.skipUnreadable = true },
	"resendTake":            func(b *bugs) { b.resendTake = true },
	"renewWaitsForWriter":   func(b *bugs) { b.renewWaitsForWriter = true },
	"keepIdleTx":            func(b *bugs) { b.keepIdleTx = true },
	"turnDescBound":         func(b *bugs) { b.turnDescBound = true },
	"shortKey":              func(b *bugs) { b.shortKey = true },
	"unlockedCounter":       func(b *bugs) { b.unlockedCounter = true },
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
	o.syncs = syncsOff
}

// SyncOn lets SQLite sync the cache to disk with synchronous = full, as it does by default, also
// where [Options.SyncOffByDefault] follows: it undoes [Options.SyncOff].
func (o *Options) SyncOn() {
	o.syncs = syncsFull
}

// SyncOffByDefault stops SQLite from syncing the cache to disk, unless [Options.SyncOn] or
// [Options.SyncOff] has said how it syncs.
func (o *Options) SyncOffByDefault() {
	if o.syncs == syncsDefault {
		o.syncs = syncsOff
	}
}

// Synchronous is SQLite's setting synchronous of the write connection: 0 for off, 2 for full. It
// requires that no call uses the instance.
func (d *DB) Synchronous() (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var setting int64
	err := d.w.query("pragma synchronous", func(s *sqlite.Stmt) error {
		setting = s.ColumnInt64(0)
		return nil
	})
	return setting, err
}

// ProgressPeriod sets the number of instructions of SQLite's virtual machine between two looks of
// a step of the cache at its context, on every connection of the instance, as
// [sqlite.Conn.SetProgressPeriod] says.
func (o *Options) ProgressPeriod(n int) {
	o.progressPeriod = n
}

// LoseCacheTx rolls back the transaction of the cache under the transaction, without the core
// knowing, as SQLite does for an error that it rolls the whole transaction back for.
func (tx *Tx) LoseCacheTx() error {
	return tx.c.c.Exec("rollback")
}

// LoseCacheTx is [Tx.LoseCacheTx] of the read transaction.
func (r *ReadTx) LoseCacheTx() error {
	return r.c.c.Exec("rollback")
}

// CheckpointAfter sets the number of frames in the write-ahead log from which the writer
// checkpoints the cache after a commit.
func (o *Options) CheckpointAfter(frames int) {
	o.checkpointAfter = frames
}

// AutoCheckpoint is SQLite's setting wal_autocheckpoint of the write connection and of the
// connection of each reader that is not closed, the write connection first. It requires that no call
// uses the instance.
func (d *DB) AutoCheckpoint() ([]int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var settings []int64
	for _, c := range append([]*conn{d.w}, d.readers...) {
		err := c.query("pragma wal_autocheckpoint", func(s *sqlite.Stmt) error {
			settings = append(settings, s.ColumnInt64(0))
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return settings, nil
}

// SpoolFile is the file of a spool, which [Options.WrapSpools] wraps.
type SpoolFile = spoolFile

// WrapSpools makes the instance wrap each of its spool files, of the kind "commit" or
// "annotations", with wrap when it creates the file.
func (o *Options) WrapSpools(wrap func(kind string, f *os.File) SpoolFile) {
	o.local.spool = &wrap
}

// ID under which the instance holds the lease.
func (d *DB) ID() string {
	return d.id
}

// Readers is the number of read connections that the instance keeps track of: those of the readers
// that are not closed.
func (d *DB) Readers() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.readers)
}

// SchemaReads is the number of statements with which the connection of the transaction has read
// tables and their indexes from the schema of the cache so far, in this transaction and in those
// before it: two for a table, and one more for its indexes and one for each of them.
func (tx *Tx) SchemaReads() int {
	return tx.c.schemaReads
}

// SchemaReads is [Tx.SchemaReads] of the connection of the read transaction.
func (r *ReadTx) SchemaReads() int {
	return r.c.schemaReads
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

// Bind args to the parameters of s.
func Bind(s *sqlite.Stmt, args ...any) error {
	return bind(s, args...)
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
	EventCommit     = eventCommit
	EventLease      = eventLease
	EventCatchUp    = eventCatchUp
	EventRefusal    = eventRefusal
	EventUnknown    = eventUnknown
	EventBusy       = eventBusy
	EventStop       = eventStop
	EventTxEnded    = eventTxEnded
	EventCheckpoint = eventCheckpoint

	AttrOutcome         = attrOutcome
	AttrTransition      = attrTransition
	AttrPrefix          = attrPrefix
	AttrInstance        = attrInstance
	AttrOperation       = attrOperation
	AttrReason          = attrReason
	AttrCommit          = attrCommit
	AttrBytes           = attrBytes
	AttrUnknownKind     = attrUnknownKind
	AttrParent          = attrParent
	AttrTxWaited        = attrTxWaited
	AttrCheckpointLog   = attrCheckpointLog
	AttrCheckpointMoved = attrCheckpointMoved
)

// CacheVersion that this package makes.
const CacheVersion = cacheVersion
