// Package core is the storage core of s4ql: one database whose truth is a log of commit objects in
// an object store, behind a manifest that is replaced only with a compare-and-swap, with a SQLite
// database as the local cache that serves reads, and a lease in the manifest that lets one instance
// at a time read and write.
//
// A [DB] is one instance of one database. [Open] opens its cache and starts the one goroutine of
// the package, which takes the lease, catches the cache up from the log, renews the lease, and
// finds out what became of a manifest swap whose outcome is unknown. [DB.Begin] starts a write
// transaction, of which one runs at a time, and [DB.Read] a read transaction, of which several
// run in parallel. A transaction changes and reads tables and rows; the package knows no SQL but
// what it writes itself for the cache.
//
// The rules that the package follows are in docs/spec.md under "Concurrency" and "Constraints".
// What it does in each case is in the doc comments of [DB], [Tx.Commit], and [DB.Read].
//
// The package waits, reads the time, and draws random numbers only through the [host.Host] that
// [Open] is given, with two exceptions that decide nothing: log/slog stamps each event with the
// time, and os.CreateTemp draws the name of a spool file.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/store"
)

// Options of a [DB]. The zero value of each option means its default.
type Options struct {
	// Lease is how long the lease of this instance lasts, counted from when the instance sent its
	// last renewal. The default is 30 seconds.
	Lease time.Duration
	// RenewAfter is how long after its last renewal the instance renews the lease, if no commit
	// has renewed it. It is shorter than Lease, and the time between the two is what a renewal has to
	// succeed in. The default is two thirds of Lease.
	RenewAfter time.Duration
	// Readers is the number of connections for read transactions, and so the number of read
	// transactions that run at once. The default is 4.
	Readers int
	// BusyTimeout is how long SQLite waits for a lock on the cache before it reports "busy". The
	// default is one second.
	BusyTimeout time.Duration
	// StoreTimeout is the timeout of each request to the store for the manifest. The default is 5
	// seconds.
	StoreTimeout time.Duration
	// ObjectTimeout is the timeout of each upload and each download of a commit object, which
	// includes applying a commit object that is downloaded. The default is 5 minutes.
	ObjectTimeout time.Duration
	// Watch is how often an instance that does not hold the lease reads the manifest, to see whether
	// the holder still writes it. The default is 5 seconds.
	Watch time.Duration
	// Logger of the events of the instance: one event for each unit of work. The default discards
	// them.
	Logger *slog.Logger

	// syncOff stops SQLite from syncing the cache to disk, for tests.
	syncOff bool
	// bugs that tests plant.
	bugs bugs
}

// withDefaults sets the options that are zero to their defaults, and checks the others.
func (o Options) withDefaults() (Options, error) {
	if o.Lease == 0 {
		o.Lease = 30 * time.Second
	}
	if o.RenewAfter == 0 {
		o.RenewAfter = o.Lease / 3 * 2
	}
	if o.Readers == 0 {
		o.Readers = 4
	}
	if o.BusyTimeout == 0 {
		o.BusyTimeout = time.Second
	}
	if o.StoreTimeout == 0 {
		o.StoreTimeout = 5 * time.Second
	}
	if o.ObjectTimeout == 0 {
		o.ObjectTimeout = 5 * time.Minute
	}
	if o.Watch == 0 {
		o.Watch = 5 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	switch {
	case o.Lease > 365*24*time.Hour:
		// Sums of a time and a lease stay far from the end of the range of a duration.
		return o, fmt.Errorf("the lease lasts %v, which is more than a year", o.Lease)
	case o.Lease < 0 || o.RenewAfter < 0 || o.RenewAfter >= o.Lease:
		return o, fmt.Errorf("the lease lasts %v and is renewed after %v, which must be above zero and shorter", o.Lease, o.RenewAfter)
	case o.Readers < 0:
		return o, fmt.Errorf("%v read connections", o.Readers)
	case o.BusyTimeout < 0 || o.StoreTimeout < 0 || o.ObjectTimeout < 0 || o.Watch < 0:
		return o, errors.New("a timeout or an interval below zero")
	}
	return o, nil
}

const (
	// retryAfter is how long the instance waits before it asks the store again after a request
	// that failed without an answer, at the most.
	retryAfter = time.Second
	// retrySoon is how long it waits before it asks again after an answer that settles nothing.
	retrySoon = 100 * time.Millisecond
	// backOff is what a step of the lease task returns in place of a time to wait when the store
	// gave no answer: the task then waits longer after each such step in a row.
	backOff time.Duration = -1
)

// margin that an instance waits on its own monotonic clock, beyond the duration of the holder's
// lease, before it takes the lease from a holder that has stopped writing the manifest.
//
// The wait is safe without a margin if both clocks run at the true rate: the holder counts its
// lease from before its write was sent, and the instance that waits counts from after it has read
// that write. The margin is for a monotonic clock of the instance that waits which runs fast
// against the holder's. A tenth of the duration covers a rate that is wrong by ten percent, where
// the drift of a quartz clock is about a hundredth of a percent, and the two seconds at the least
// cover a short lease on a machine whose clock is stepped while it runs. It costs every takeover
// from a holder that died that much time, and a takeover after a clean release nothing.
func margin(duration time.Duration) time.Duration {
	return max(2*time.Second, duration/10)
}

// DB is one instance of one database.
//
// An instance serves reads and writes only while it holds the lease of the database and its cache
// is caught up with the log. Until then, and whenever it loses the lease, calls fail at once with
// an error that wraps [ErrNoLease], and the instance keeps trying to get the lease. An instance that
// never gets it, because another one holds it, is a spare, which is a normal state.
//
// An instance stops for good when its cache fails, when it meets something in the store that it
// cannot read, and when it is closed. Every call then fails with an error that wraps [ErrStopped]
// and the cause, [DB.Done] is closed, and [DB.Err] is that error. The one call that goes on is a
// [Tx.Commit] whose manifest swap was sent and may be applied, which goes on finding out whether
// it is. A stopped instance tries once to release the lease. Nothing brings it back: the application closes it and opens the database
// again, which catches the cache up from its recorded position.
type DB struct {
	host    host.Host
	store   store.Store
	options Options
	bugs    bugs
	log     *slog.Logger
	// id under which the instance holds the lease.
	id          string
	prefix      string
	manifestKey string
	path        string

	// writerLock is held for a whole write transaction, and by the lease task while it takes the
	// lease and catches up. It guards w. It is acquired before manifestLock, never after.
	writerLock host.Semaphore
	// w is the write connection to the cache, which is opened first and stays open.
	w *conn

	// manifestLock is held around each swap of the manifest and what follows from its answer. It
	// guards the fields from man to lastTakeFrom.
	manifestLock host.Semaphore
	// man is the manifest as this instance last knew the store to have it while it held the lease,
	// and version is its version there. The version is empty while the instance does not hold the
	// lease.
	man     format.Manifest
	version store.Version
	// flights are the swaps that were sent from version and whose outcome is unknown. At most one of
	// them is ever applied, because each is a swap from the same version.
	flights []*flight
	// watch is the version of the manifest that the instance waits on to take the lease.
	watch watch
	// unsure is whether a request that takes or releases the lease had an outcome that is unknown,
	// so that the manifest may name this instance as the holder without its knowing.
	unsure bool
	// lastTake is the take of the lease that was sent last, and lastTakeFrom the version that it was
	// sent from, under the planted bug resendTake only.
	lastTake     *flight
	lastTakeFrom store.Version

	// readerPool has one permit for every read connection.
	readerPool host.Semaphore
	// readers are all the read connections, idle or not. It does not change after Open.
	readers []*conn

	// mu guards the fields below. It is never held while waiting.
	mu sync.Mutex
	// idle read connections.
	idle []*conn
	// stopped is the error of every call once the instance has stopped for good, and done is closed
	// then.
	stopped error
	done    chan struct{}
	// holder is whether the instance holds the lease, as far as it knows: whether version is set.
	holder bool
	// serving is whether the instance holds the lease, its cache is caught up, and no commit of it
	// has an outcome that it does not know. It becomes true only under the writer lock.
	serving bool
	// unresolved is whether a commit of the instance has an outcome that it does not know.
	unresolved bool
	// sent is when the last renewal that is known to have happened was first sent.
	sent sentAt
	// waiters are the waits that a change of stopped, holder, or serving ends, and a lease that is
	// valid again by the clocks, by a number. changes counts those changes.
	waiters map[uint64]context.CancelFunc
	waiter  uint64
	changes uint64
	// cancelWork ends what the lease task is doing when the instance stops, and cancelTask ends the
	// lease task, also its one try to release the lease, when the instance is closed.
	cancelWork, cancelTask context.CancelFunc
	// closing is whether [DB.Close] has begun, which it does together with the stop: every
	// connection that no call uses is then closed, and every other one when the last call that uses
	// it ends. closeErrs are the errors of those closes that no Close has returned yet. closed is
	// whether a Close has come to its end.
	closing, closed bool
	closeErrs       []error
	// racy is the counter of the planted data race.
	racy int

	// leaseTask is the one goroutine of the instance.
	leaseTask host.Task
}

// sentAt is when a request was sent, by both clocks of the host.
type sentAt struct {
	monotonic time.Duration
	wall      time.Time
}

// now by both clocks.
func (d *DB) now() sentAt {
	return sentAt{monotonic: d.host.Monotonic(), wall: d.host.Wall()}
}

// Open the database under the key prefix in the store s, with the cache in the SQLite database at
// path, which it creates if there is none. An empty prefix means that the keys have none.
//
// It fails only on local problems: options that make no sense, a prefix that cannot be part of a
// key, and a cache that cannot be opened or that this version did not make, with an error that
// wraps [ErrLocal] or [ErrCacheVersion]. It does not contact the store and does not wait for the
// lease: [DB.Wait] does. Nothing rebuilds a cache that cannot be opened.
func Open(ctx context.Context, h host.Host, s store.Store, path, prefix string, options Options) (_ *DB, err error) {
	options, err = options.withDefaults()
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if err := checkPrefix(prefix); err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, fmt.Errorf("open: the cache needs the path of a file, not %q", path)
	}

	d := &DB{
		host:        h,
		store:       s,
		options:     options,
		bugs:        options.bugs,
		id:          fmt.Sprintf("%016x%016x", h.Uint64(), h.Uint64()),
		prefix:      prefix,
		manifestKey: key(prefix, "manifest.json"),
		path:        path,
		done:        make(chan struct{}),
		waiters:     map[uint64]context.CancelFunc{},
	}
	d.log = options.Logger.With(attrPrefix, prefix, attrInstance, d.id)
	d.writerLock = h.NewSemaphore("writer", 1)
	d.manifestLock = h.NewSemaphore("manifest", 1)
	d.readerPool = h.NewSemaphore("readers", options.Readers)

	defer func() {
		if err != nil {
			d.closeCache()
		}
	}()

	// The write connection opens first and runs the first statement, so that recovering the
	// write-ahead log after a crash, which reports "busy" to every other connection, is over before
	// a read connection exists.
	busy := options.BusyTimeout.Milliseconds()
	if d.w, err = openConn(path, busy); err != nil {
		return nil, openError(err)
	}
	if err := d.w.setUp(options.syncOff); err != nil {
		return nil, openError(err)
	}
	for range options.Readers {
		c, err := openConn(path, busy)
		if err != nil {
			return nil, openError(err)
		}
		d.idle = append(d.idle, c)
		d.readers = append(d.readers, c)
		if err := c.exec("pragma query_only = on"); err != nil {
			return nil, openError(err)
		}
	}

	// The task outlives the context of this call, and ends when the instance stops.
	d.leaseTask = h.Go(context.WithoutCancel(ctx), "lease of "+d.id, d.leaseLoop)
	return d, nil
}

// openError of the cache.
func openError(err error) error {
	if errors.Is(err, ErrCacheVersion) {
		return fmt.Errorf("open the cache: %w", err)
	}
	return fmt.Errorf("open the cache: %w: %w", ErrLocal, err)
}

// checkPrefix can be the start of the keys of a database: valid UTF-8, short enough for the key of
// a commit object to fit in the 1024 bytes of a key, and without a path segment that is empty, ".",
// or "..".
func checkPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if !utf8.ValidString(prefix) || len(prefix) > 900 {
		return fmt.Errorf("the prefix %q is not valid UTF-8 of at most 900 bytes", prefix)
	}
	for segment := range strings.SplitSeq(prefix, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("the prefix %q has a path segment that is empty, %q, or %q", prefix, ".", "..")
		}
	}
	return nil
}

// key of the object with the given name under the prefix.
func key(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}

// commitKey of the commit object with the given name: in a folder named by the first two
// hexadecimal digits of the name, and with the whole name, so that a tool that copies the bucket to
// a disk does not make one folder of every commit object, and a file that is copied alone still
// names itself.
func (d *DB) commitKey(name format.Name) string {
	s := name.String()
	return key(d.prefix, "commits/"+s[:2]+"/"+s)
}

// spoolDir and spoolPattern say where a transaction keeps its files: next to the cache.
func (d *DB) spoolDir() string {
	return filepath.Dir(d.path)
}

func (d *DB) spoolPattern() string {
	return filepath.Base(d.path) + "-spool-*"
}

// closeCache closes every connection to the cache, also one that a call uses, ignoring errors. It
// is for an instance that nothing uses any more.
func (d *DB) closeCache() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.readers {
		_ = c.close()
	}
	d.idle = nil
	if d.w != nil {
		_ = d.w.close()
	}
}

// Done is closed when the instance has stopped for good.
func (d *DB) Done() <-chan struct{} {
	return d.done
}

// Err is nil while the instance has not stopped, and after that an error that wraps [ErrStopped]
// and the cause of the stop.
func (d *DB) Err() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopped
}

// stop the instance for good because of cause, unless it has stopped already, and return the error
// that every later call returns. No read and no write starts after it returns. The lease task then
// tries once to release the lease.
func (d *DB) stop(ctx context.Context, cause error) error {
	d.mu.Lock()
	first, err := d.stopLocked(cause)
	d.mu.Unlock()
	if first {
		d.stopEvent(ctx, cause)
	}
	return err
}

// stopLocked is [DB.stop] without the event, which the caller logs with [DB.stopEvent] if first is
// true. It requires mu.
func (d *DB) stopLocked(cause error) (first bool, err error) {
	first = d.stopped == nil
	if first {
		d.stopped = fmt.Errorf("%w: %w", ErrStopped, cause)
		close(d.done)
		if d.cancelWork != nil {
			d.cancelWork()
		}
		d.wakeLocked()
	}
	return first, d.stopped
}

// stopEvent logs the stop for the cause.
func (d *DB) stopEvent(ctx context.Context, cause error) {
	level := slog.LevelError
	if errors.Is(cause, ErrClosed) {
		level = slog.LevelInfo
	}
	d.event(ctx, level, eventStop, slog.String(attrOutcome, stopOutcome(cause)), errAttr(cause))
}

// stopOutcome names the kind of the cause of a stop.
func stopOutcome(cause error) string {
	switch {
	case errors.Is(cause, ErrClosed):
		return "closed"
	case errors.Is(cause, ErrLocal):
		return "local_failure"
	case errors.Is(cause, ErrCacheNotInLog):
		return "cache_not_in_log"
	case errors.Is(cause, format.ErrNewerVersion):
		return "newer_version"
	case errors.Is(cause, format.ErrNameMismatch):
		return "name_mismatch"
	case errors.Is(cause, format.ErrMalformed):
		return "malformed"
	case errors.Is(cause, ErrLogBroken):
		return "log_broken"
	}
	return "other"
}

// cacheError turns an error from the cache, during the given operation, into the error to return. A
// local I/O failure stops the instance. "Busy" is logged and returned, and stops nothing. Any other
// error is returned as it is.
func (d *DB) cacheError(ctx context.Context, operation string, err error) error {
	switch {
	case err == nil:
		return nil
	case classify(err) == kindLocal || errors.Is(err, ErrCacheVersion):
		return d.stop(ctx, fmt.Errorf("%w: %v: %w", ErrLocal, operation, err))
	case classify(err) == kindBusy:
		d.event(ctx, slog.LevelWarn, eventBusy, slog.String(attrOperation, operation), slog.String(attrOutcome, "busy"), errAttr(err))
		return fmt.Errorf("%w: %v: %w", ErrBusy, operation, err)
	}
	return err
}

// spoolError stops the instance for a failure of a spool file, which is a local I/O failure.
func (d *DB) spoolError(ctx context.Context, err error) error {
	return d.stop(ctx, fmt.Errorf("%w: spool file: %w", ErrLocal, err))
}

// stoppedError is the error of the stop, or nil.
func (d *DB) stoppedError() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopped
}

// refusalLocked is why the instance serves nothing now, in a word, or empty if it serves. It
// requires mu.
func (d *DB) refusalLocked() string {
	switch {
	case !d.holder:
		return "not_holder"
	case d.unresolved:
		return "outcome_unknown"
	case !d.serving:
		return "not_caught_up"
	}
	// Each clock is compared only with itself. The wall clock catches a monotonic clock that stood
	// still, as Go's does on Linux while the machine is suspended, and the monotonic clock catches a
	// wall clock that was set backward.
	now := d.now()
	wall := now.wall.Sub(d.sent.wall) < d.options.Lease
	monotonic := now.monotonic-d.sent.monotonic < d.options.Lease
	switch {
	case d.bugs.wallOnly:
		// The planted bug.
		monotonic = true
	case d.bugs.monotonicOnly:
		// The planted bug.
		wall = true
	}
	switch {
	case !monotonic:
		return "lease_expired_monotonic"
	case !wall:
		return "lease_expired_wall"
	}
	return ""
}

// servingLocked returns nil if the instance serves and less than the duration of its lease has
// passed since it sent its last renewal, by its wall clock and by its monotonic clock. Otherwise
// it returns the error of the stop, or one that wraps [ErrNoLease]. It requires mu.
func (d *DB) servingLocked() error {
	if d.stopped != nil {
		return d.stopped
	}
	if reason := d.refusalLocked(); reason != "" {
		return fmt.Errorf("%w: %v", ErrNoLease, reason)
	}
	return nil
}

// usableLocked returns nil if the instance has not stopped and serves, whatever its clocks say. It
// is the check of a transaction that is open: a write transaction is judged by its manifest swap,
// and a read transaction was checked against the clocks when it started. It requires mu.
func (d *DB) usableLocked() error {
	switch {
	case d.stopped != nil:
		return d.stopped
	case !d.holder:
		return errLeaseLost
	case !d.serving:
		return fmt.Errorf("%w: %v", ErrNoLease, d.refusalLocked())
	}
	return nil
}

// usable is [DB.usableLocked] without mu.
func (d *DB) usable() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.usableLocked()
}

// refused logs that the operation was refused for lack of a valid lease, if err says so, and
// returns err.
func (d *DB) refused(ctx context.Context, operation string, err error) error {
	if errors.Is(err, ErrNoLease) && d.enabled(ctx, slog.LevelDebug) {
		reason := strings.TrimPrefix(err.Error(), ErrNoLease.Error()+": ")
		d.event(ctx, slog.LevelDebug, eventRefusal, slog.String(attrOperation, operation), slog.String(attrReason, reason), slog.String(attrOutcome, "refused"))
	}
	return err
}

// wakeLocked ends every wait that is registered. It requires mu.
func (d *DB) wakeLocked() {
	d.changes++
	for id, cancel := range d.waiters {
		cancel()
		delete(d.waiters, id)
	}
}

// waitLocked registers a wait and returns its context, which is done when ctx is, when stopped,
// holder, or serving changes, and when the lease is valid again after it had run out by a clock.
// The caller registers under mu, in the same critical section in which it found that it has to
// wait, or after it has checked there that changes is what it was when it found that, so that no
// change is missed. It calls the returned function after the wait, without mu. It requires mu.
func (d *DB) waitLocked(ctx context.Context) (context.Context, func()) {
	wctx, cancel := context.WithCancel(ctx)
	d.waiter++
	id := d.waiter
	d.waiters[id] = cancel
	return wctx, func() {
		d.mu.Lock()
		delete(d.waiters, id)
		d.mu.Unlock()
		cancel()
	}
}

// pause for at most duration, or until the state of the instance changes: see [DB.waitLocked]. It
// does not wait at all if the state has changed since changes was read. It fails only if ctx is done
// first, with the error of the wait.
func (d *DB) pause(ctx context.Context, duration time.Duration, changes uint64) error {
	d.mu.Lock()
	if d.changes != changes {
		d.mu.Unlock()
		return nil
	}
	wctx, end := d.waitLocked(ctx)
	d.mu.Unlock()
	err := d.host.Sleep(wctx, duration)
	end()
	if ctx.Err() != nil {
		return err
	}
	return nil
}

// leftLocked is how long the lease of an instance that serves is still valid by the clocks that
// [DB.refusalLocked] looks at, and a moment at the least, so that a wait for it always waits. It
// requires mu.
func (d *DB) leftLocked() time.Duration {
	now := d.now()
	monotonic := d.options.Lease - (now.monotonic - d.sent.monotonic)
	wall := d.options.Lease - now.wall.Sub(d.sent.wall)
	left := min(monotonic, wall)
	switch {
	case d.bugs.wallOnly:
		left = wall
	case d.bugs.monotonicOnly:
		left = monotonic
	}
	return max(left, retrySoon)
}

// acquireServing acquires a permit of the semaphore for a caller that needs the instance to serve.
// It fails at once, and whenever the instance stops serving or its lease runs out while the caller
// waits, with the error of [DB.servingLocked], and with the error of the wait if ctx is done first.
func (d *DB) acquireServing(ctx context.Context, sem host.Semaphore) error {
	for {
		d.mu.Lock()
		if err := d.servingLocked(); err != nil {
			d.mu.Unlock()
			return err
		}
		wctx, end := d.waitLocked(ctx)
		left := d.leftLocked()
		d.mu.Unlock()

		// Nothing wakes a caller when the lease runs out, so the wait ends then, and looks again.
		tctx, cancel := d.host.WithTimeout(wctx, left)
		err := sem.Acquire(tctx)
		cancel()
		end()
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			// The state changed: look again.
			continue
		}

		d.mu.Lock()
		err = d.servingLocked()
		d.mu.Unlock()
		if err != nil {
			sem.Release(ctx)
			return err
		}
		return nil
	}
}

// Wait until the instance serves: until it holds the lease, its cache is caught up, and the lease
// is valid by the clocks. It fails with the error of the stop if the instance stops first, and
// with the error of the wait if ctx is done first. A read or a write that follows can still fail,
// because the instance can stop serving at any time.
func (d *DB) Wait(ctx context.Context) error {
	for {
		d.mu.Lock()
		err := d.servingLocked()
		if err == nil || d.stopped != nil {
			d.mu.Unlock()
			return err
		}
		wctx, end := d.waitLocked(ctx)
		d.mu.Unlock()

		err = d.host.Sleep(wctx, time.Duration(1<<63-1))
		end()
		if ctx.Err() != nil {
			return err
		}
	}
}

// Close the instance: stop it for good, close the cache, and release the lease, so that the
// manifest then has no lease entry and the next instance does not wait.
//
// The cache is closed whatever the store does. Every connection that no call uses is closed at
// once, also the one of a transaction that is open, whose every call fails from then on: a write
// transaction is then rolled back, and its Rollback returns nil. A connection that a call uses is
// closed when the call ends. After the stop, every call fails at its next check, except a
// [Tx.Commit] whose swap may be applied, which goes on finding out, and a request to the store that
// is on its way ends at its answer or its timeout. Close waits for those calls until ctx is done,
// and returns an error if one still runs.
//
// The release of the lease gets half of the time that ctx has left, if ctx has a deadline, and the
// duration of the lease at the most. Close returns an error that says that the lease is not
// released, if it is not, and the lease then times out. A request to take the lease that this
// instance sent earlier, and that got no answer, can still be applied after Close has returned,
// while the log has not grown: the manifest then names this instance, and the next instance waits
// for that lease to time out. Closing an instance whose Close came to its end does nothing.
func (d *DB) Close(ctx context.Context) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	// The lease task is ended in the same critical section as the stop, so that it never starts its
	// one try to release the lease. The release below reads the manifest first, so it also does what
	// that try would do.
	first, _ := d.stopLocked(ErrClosed)
	d.closing = true
	if d.cancelTask != nil {
		d.cancelTask()
	}
	d.closeIdleLocked()
	d.mu.Unlock()
	if first {
		d.stopEvent(ctx, ErrClosed)
	}

	rctx, cancel := d.releaseContext(ctx)
	releaseErr := d.release(rctx)
	cancel()
	if releaseErr != nil {
		releaseErr = fmt.Errorf("close: the lease is not released, and times out: %w", releaseErr)
	}

	if err := d.leaseTask.Wait(ctx); err != nil {
		return errors.Join(d.closeErrors(false), releaseErr, fmt.Errorf("close: the lease task: %w", err))
	}
	if err := d.waitClosed(ctx); err != nil {
		return errors.Join(d.closeErrors(false), releaseErr, fmt.Errorf("close: a call still uses the cache, which is closed when the call ends: %w", err))
	}
	return errors.Join(d.closeErrors(true), releaseErr)
}

// closeErrors are the errors of the connections that were closed since the last call, and closed
// marks the end of a [DB.Close].
func (d *DB) closeErrors(closed bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	err := errors.Join(d.closeErrs...)
	d.closeErrs = nil
	d.closed = d.closed || closed
	return err
}

// releaseContext is the context of the release of the lease in [DB.Close]: with half of the time
// that ctx has left, if ctx has a deadline, so that the release does not take the time of the calls
// that still use the cache. The time left is measured by the wall clock of the host, against which
// the deadline was set, so a step of that clock during the close changes only how long the release
// tries, and the release then ends at ctx's deadline at the latest.
func (d *DB) releaseContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	return d.host.WithTimeout(ctx, deadline.Sub(d.host.Wall())/2)
}

// use the connection c for a call, which gives it back with [DB.unuse]. It fails with the error of
// the stop if [DB.Close] has closed c.
func (d *DB) use(c *conn) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c.closed {
		return d.stopped
	}
	c.users++
	return nil
}

// unuse gives back the connection c after a call. While the instance is closed, it closes every
// connection that no call uses any more.
func (d *DB) unuse(c *conn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c.users--
	if d.closing {
		d.closeIdleLocked()
	}
}

// closeIdleLocked closes every connection to the cache that no call uses, the read connections
// first, so that the write connection is the last to close when no call uses any, keeps the errors
// of the closes for [DB.Close] to return, and wakes the waits if it closed one. It requires mu, and
// that the instance has stopped.
func (d *DB) closeIdleLocked() {
	closed := false
	for _, c := range append(slices.Clone(d.readers), d.w) {
		if !c.closed && c.users == 0 {
			c.closed, closed = true, true
			if err := c.close(); err != nil {
				d.closeErrs = append(d.closeErrs, fmt.Errorf("close: a connection to the cache: %w", err))
			}
		}
	}
	if closed {
		d.wakeLocked()
	}
}

// waitClosed waits until every connection to the cache is closed. It fails with the error of the
// wait if ctx is done first.
func (d *DB) waitClosed(ctx context.Context) error {
	for {
		d.mu.Lock()
		open := !d.w.closed
		for _, c := range d.readers {
			open = open || !c.closed
		}
		if !open {
			d.mu.Unlock()
			return nil
		}
		wctx, end := d.waitLocked(ctx)
		d.mu.Unlock()

		err := d.host.Sleep(wctx, time.Duration(1<<63-1))
		end()
		if ctx.Err() != nil {
			return err
		}
	}
}

// locks that a function holds and gives back in a deferred call. Each lock is marked as given back
// before it is released, so that a deferred call that runs while a release is under way does not
// release it a second time.
type locks struct {
	d        *DB
	writer   bool
	manifest bool
}

func (l *locks) acquireWriter(ctx context.Context) error {
	if err := l.d.writerLock.Acquire(ctx); err != nil {
		return err
	}
	l.writer = true
	return nil
}

func (l *locks) acquireManifest(ctx context.Context) error {
	if err := l.d.manifestLock.Acquire(ctx); err != nil {
		return err
	}
	l.manifest = true
	return nil
}

func (l *locks) releaseManifest(ctx context.Context) {
	if l.manifest {
		l.manifest = false
		l.d.manifestLock.Release(ctx)
	}
}

// release the locks that are held, the manifest lock first.
func (l *locks) release(ctx context.Context) {
	l.releaseManifest(ctx)
	if l.writer {
		l.writer = false
		l.d.writerLock.Release(ctx)
	}
}
