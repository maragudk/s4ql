package sim_test

// A toy library with the shape of s4ql, in plain blocking Go: the truth is a log in an object store
// behind a compare-and-swap manifest, a SQLite database is the cache that serves reads, one write
// transaction runs at a time, and a background task keeps a lease. It exists to exercise the harness
// and is not a design for s4ql. Where the harness has to show an ability, it follows the design that
// is agreed for s4ql: a read checks the lease against the wall clock and the monotonic clock, an
// instance without the lease keeps trying to take it and catches its cache up from the log when it
// does, and a local failure after the manifest swap stops the instance.
//
// The toy knows the world only through [host.Host], and none of its non-test code would break the
// rules of internal/simlint.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/sqlite"
)

const (
	manifestKey = "manifest.json"
	// takeoverMargin that an instance waits on its own monotonic clock, beyond the duration of the
	// holder's lease, before it takes the lease from the holder.
	takeoverMargin = 2 * time.Second
)

var (
	errNotFound       = errors.New("object not found")
	errExists         = errors.New("object exists")
	errPrecondition   = errors.New("precondition failed")
	errInjected       = errors.New("injected fault")
	errSwapFailed     = errors.New("manifest swap failed")
	errOutcomeUnknown = errors.New("the outcome of a manifest swap is unknown")
	errLeaseLost      = errors.New("lease lost")
	errLeaseExpired   = errors.New("lease expired")
	errNotServing     = errors.New("not serving yet")
	errLocal          = errors.New("local I/O failure")
	errStopped        = errors.New("stopped")
)

// objectStore is the part of S3 that the toy library uses.
type objectStore interface {
	// Put data at key if no object is there, and fail with errExists otherwise.
	Put(ctx context.Context, key string, data []byte) error
	// Get the object at key and its ETag, or fail with errNotFound.
	Get(ctx context.Context, key string) (data []byte, etag string, err error)
	// Swap the object at key for data if its ETag is still etag, where the empty ETag means that no
	// object is there, and return the new ETag. It fails with errPrecondition otherwise.
	Swap(ctx context.Context, key string, data []byte, etag string) (newETag string, err error)
}

// manifest of the toy database.
type manifest struct {
	// Head is the name of the newest change file.
	Head string `json:"head"`
	// Seq counts the commits.
	Seq int64 `json:"seq"`
	// Lease is the lease entry, or nil if no instance holds the lease.
	Lease *lease `json:"lease,omitzero"`
}

// lease entry of the manifest. It has no expiry: an instance that wants the lease waits for the
// duration on its own monotonic clock, and no clock is compared with another machine's.
type lease struct {
	// Holder is the ID of the instance that holds the lease.
	Holder string `json:"holder"`
	// Duration of the lease in nanoseconds, which the holder counts from when it sent the write, and
	// which an instance that watches waits for. Version 2 of encoding/json has no default encoding of
	// a [time.Duration].
	Duration int64 `json:"duration"`
	// Wall is the holder's wall clock when it sent the write. Nothing reads it for a decision.
	Wall time.Time `json:"wall"`
	// Salt is new at every write by the holder, so that no two writes of the manifest by holders have
	// the same bytes, and so the same version.
	Salt string `json:"salt"`
}

// changeFile is one commit in the log.
type changeFile struct {
	// ID of the transaction, which tells apart two transactions with the same writes.
	ID  uint64 `json:"id"`
	Seq int64  `json:"seq"`
	// Prev is the name of the change file before this one, or empty for the first.
	Prev string `json:"prev"`
	// Sets are the keys and values that the commit wrote, in the order it wrote them.
	Sets [][2]int64 `json:"sets"`
}

// bugs that can be planted in the toy library.
type bugs struct {
	// renewOutsideLock makes the renewal read the manifest's ETag under the swap lock and then swap
	// after releasing it, so that a renewal and a commit can be in flight with the same ETag.
	renewOutsideLock bool
	// cacheFirst makes a commit go to the cache before the manifest swap, so that a read can see a
	// transaction that is not committed.
	cacheFirst bool
	// unlockedCounter makes every read add to a counter without synchronisation, which is a data race.
	unlockedCounter bool
	// wallOnly makes a read check the lease against the wall clock only, so a wall clock that is set
	// backward or stands still keeps an instance serving after another has taken the lease.
	wallOnly bool
	// monotonicOnly makes a read check the lease against the monotonic clock only, so a monotonic
	// clock that stood still keeps an instance serving after another has taken the lease.
	monotonicOnly bool
	// takeoverAfterRelease makes an instance take the lease of another only after it was released, as
	// if every holder released its lease, and forgets that a holder can also die.
	takeoverAfterRelease bool
	// keepSalt makes a renewal write the salt of the manifest it replaces, so that a renewal by a
	// holder whose wall clock stands still writes the same bytes, and the same version.
	keepSalt bool
	// ownDuration makes an instance that watches the lease wait for its own lease duration, and not
	// for the holder's.
	ownDuration bool
	// lateVersion makes an instance that watches the lease read the manifest again at the end of its
	// wait, and swap against that version, and not against the one it waited on.
	lateVersion bool
	// leaseFromAnswer makes a holder count its lease from the answer to a swap, and not from when it
	// sent the swap.
	leaseFromAnswer bool
	// ignoreCacheError makes a commit whose manifest swap succeeded ignore a failure of the cache, and
	// the instance go on serving, as if the commit were safe because the object store has it.
	ignoreCacheError bool
}

// toyOptions for opening a [toyDB].
type toyOptions struct {
	// readers is the number of read connections.
	readers int
	// busyTimeout in milliseconds for every connection.
	busyTimeout int
	// synchronousOff stops SQLite from syncing the cache to disk.
	synchronousOff bool
	// leaseDuration of the lease that this instance takes. It renews the lease after two thirds of it.
	leaseDuration time.Duration
	bugs          bugs
}

// toyDB is the toy database.
type toyDB struct {
	host    host.Host
	store   objectStore
	bugs    bugs
	readerN int
	// id of this instance as holder of the lease.
	id string

	// writer lets one write transaction in at a time, and guards wconn.
	writer host.Semaphore
	wconn  *sqlite.Conn
	// swap lets one manifest swap be in flight at a time, and guards etag and man, which are the
	// manifest as this instance last swapped it in, and pending, a renewal whose outcome is unknown.
	swap    host.Semaphore
	etag    string
	man     manifest
	pending *pendingSwap
	// unsure is whether an acquire of the lease had an outcome that is unknown, so that the manifest
	// may name this instance as the holder without its knowing. It is guarded by swap too.
	unsure bool
	// waited is the version of the manifest for which this instance has waited the duration of its
	// lease and the margin. Only the task that takes the lease uses it.
	waited string
	// readers has one permit for every read connection.
	readers host.Semaphore

	// mu guards idle, serving, and stopped. It is never held while waiting.
	mu      sync.Mutex
	idle    []*sqlite.Conn
	serving bool
	stopped error

	// duration of the lease that this instance takes.
	duration time.Duration
	// sent and sentWall are the readings of the monotonic clock, and of the wall clock in nanoseconds
	// since the Unix epoch, when the last swap that is known to have happened was sent.
	sent     atomic.Int64
	sentWall atomic.Int64
	// racy is the counter of the unlockedCounter bug.
	racy int

	cancel context.CancelFunc
	leaser host.Task
}

// openToy database with the cache at path, which it creates if there is none. It does not wait for
// the lease: a task takes it, catches up the cache, and keeps the lease from then on.
func openToy(ctx context.Context, h host.Host, store objectStore, path string, options toyOptions) (_ *toyDB, err error) {
	d := &toyDB{
		host:     h,
		store:    store,
		bugs:     options.bugs,
		readerN:  options.readers,
		duration: options.leaseDuration,
		id:       fmt.Sprintf("%016x", h.Uint64()),
		writer:   h.NewSemaphore("writer", 1),
		swap:     h.NewSemaphore("swap", 1),
		readers:  h.NewSemaphore("readers", options.readers),
	}

	open := func() (*sqlite.Conn, error) {
		c, err := sqlite.Open(path)
		if err != nil {
			return nil, err
		}
		pragmas := []string{fmt.Sprint("pragma busy_timeout = ", options.busyTimeout), "pragma journal_mode = wal"}
		if options.synchronousOff {
			pragmas = append(pragmas, "pragma synchronous = off")
		}
		for _, pragma := range pragmas {
			if err := c.Exec(pragma); err != nil {
				_ = c.Close()
				return nil, err
			}
		}
		return c, nil
	}

	defer func() {
		if err != nil {
			d.closeFiles()
		}
	}()

	if d.wconn, err = open(); err != nil {
		return nil, err
	}
	for _, sql := range []string{
		"create table if not exists kv (k integer primary key, v integer not null) strict",
		"create table if not exists meta (id integer primary key check (id = 1), seq integer not null) strict",
		"insert or ignore into meta (id, seq) values (1, 0)",
	} {
		if err := d.wconn.Exec(sql); err != nil {
			return nil, err
		}
	}
	for range options.readers {
		c, err := open()
		if err != nil {
			return nil, err
		}
		d.idle = append(d.idle, c)
	}

	ctx, d.cancel = context.WithCancel(ctx)
	d.leaser = h.Go(ctx, "leaser of "+d.id, d.leaseLoop)
	return d, nil
}

// closeFiles of the cache, ignoring errors.
func (d *toyDB) closeFiles() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range append(d.idle, d.wconn) {
		if c != nil {
			_ = c.Close()
		}
	}
	d.idle = nil
}

// stop the instance for good because of cause, and return the error that every later call returns.
func (d *toyDB) stop(cause error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped == nil {
		d.stopped = fmt.Errorf("%w: %w", errStopped, cause)
	}
	return d.stopped
}

func (d *toyDB) stopErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopped
}

func (d *toyDB) isServing() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.serving
}

// leaseLoop is the task that takes the lease and then keeps it, until ctx is done or the instance
// has stopped.
func (d *toyDB) leaseLoop(ctx context.Context) {
	for d.stopErr() == nil {
		if !d.isServing() {
			wait, err := d.acquire(ctx)
			if err == nil {
				continue
			}
			if err := d.host.Sleep(ctx, wait); err != nil {
				return
			}
			continue
		}

		early := d.host.Buggify(ctx, "toy: renew the lease early")
		if wait := d.renewalDue() - d.host.Monotonic(); wait > 0 && !early {
			if err := d.host.Sleep(ctx, wait); err != nil {
				return
			}
			// A commit may have renewed the lease in the meantime.
			continue
		}
		if err := d.renew(ctx, early); err != nil {
			// Try again shortly, unless the instance has stopped.
			if err := d.host.Sleep(ctx, time.Second); err != nil {
				return
			}
		}
	}
}

// renewalDue is the reading of the monotonic clock at which the lease is renewed, two thirds of its
// duration after the last swap that is known to have happened was sent.
func (d *toyDB) renewalDue() time.Duration {
	return time.Duration(d.sent.Load()) + d.duration*2/3
}

// acquire the lease, catch up the cache, and start serving. If the manifest has the lease entry of
// another instance, it first waits for the duration of that lease and the margin on this instance's
// monotonic clock, and then swaps against the version it read before the wait, which fails if the
// holder has written the manifest since. It waits once for a version: if the manifest still has it
// after a swap that failed for another reason, the holder has written nothing since. It holds the
// writer lock and the swap lock, except during the wait, so that a transaction waits for it. It
// returns how long to wait before trying again if it fails.
func (d *toyDB) acquire(ctx context.Context) (time.Duration, error) {
	locked := false
	lock := func() error {
		if err := d.writer.Acquire(ctx); err != nil {
			return err
		}
		if err := d.swap.Acquire(ctx); err != nil {
			d.writer.Release(ctx)
			return err
		}
		locked = true
		return d.stopErr()
	}
	unlock := func() {
		d.swap.Release(ctx)
		d.writer.Release(ctx)
		locked = false
	}
	defer func() {
		if locked {
			unlock()
		}
	}()

	if err := lock(); err != nil {
		return 0, err
	}
	man, etag, err := d.readManifest(ctx)
	if err != nil {
		return time.Second, err
	}
	if man.Lease != nil && man.Lease.Holder != d.id && etag != d.waited {
		if d.bugs.takeoverAfterRelease {
			// The planted bug.
			return 5 * time.Second, errors.New("the lease is held by another instance")
		}
		wait := time.Duration(man.Lease.Duration)
		if d.bugs.ownDuration {
			// The planted bug.
			wait = d.duration
		}
		unlock()
		if err := d.host.Sleep(ctx, wait+takeoverMargin); err != nil {
			return 0, err
		}
		if err := lock(); err != nil {
			return 0, err
		}
		d.waited = etag
		if d.bugs.lateVersion {
			// The planted bug.
			if man, etag, err = d.readManifest(ctx); err != nil {
				return time.Second, err
			}
		}
	}

	man.Lease = d.newLease(false)
	swapped, etag, at, err := d.trySwap(ctx, man, etag, "")
	switch {
	case errors.Is(err, errPrecondition):
		// The manifest changed, so its holder is alive, or another instance took the lease: read it
		// again, and wait again from the new version.
		return 0, err
	case errors.Is(err, errOutcomeUnknown):
		// If the swap happened, the next acquire finds this instance's ID in the manifest, and takes
		// the lease at once.
		d.unsure = true
		return time.Second, err
	case err != nil:
		return time.Second, err
	}
	d.unsure = false
	d.adopt(swapped, etag, at)
	if err := d.catchUp(ctx); err != nil {
		return time.Second, err
	}
	d.mu.Lock()
	d.serving = true
	d.mu.Unlock()
	return 0, nil
}

// readManifest and its ETag, or the zero manifest and the empty ETag if there is none.
func (d *toyDB) readManifest(ctx context.Context) (manifest, string, error) {
	var man manifest
	data, etag, err := d.store.Get(ctx, manifestKey)
	switch {
	case errors.Is(err, errNotFound):
		return man, "", nil
	case err != nil:
		return man, "", err
	}
	if err := json.Unmarshal(data, &man); err != nil {
		return man, "", d.stop(err)
	}
	return man, etag, nil
}

// newLease entry for a write of the manifest by this instance as the holder, with a new salt. The
// swap sets the wall clock when it sends the write. It requires the swap lock.
func (d *toyDB) newLease(renewal bool) *lease {
	salt := fmt.Sprintf("%016x", d.host.Uint64())
	if renewal && d.bugs.keepSalt && d.man.Lease != nil {
		// The planted bug.
		salt = d.man.Lease.Salt
	}
	return &lease{Holder: d.id, Duration: int64(d.duration), Salt: salt}
}

// sentAt is when a swap was sent, by the monotonic clock and by the wall clock.
type sentAt struct {
	monotonic time.Duration
	wall      time.Time
}

// adopt the manifest that this instance swapped in, and the lease that the swap sent at gives. It
// requires the swap lock.
func (d *toyDB) adopt(man manifest, etag string, at sentAt) {
	d.man, d.etag = man, etag
	d.sent.Store(int64(at.monotonic))
	d.sentWall.Store(at.wall.UnixNano())
}

// catchUp the cache with the log, from the commit it has to the commit the manifest has. It requires
// the writer lock and the swap lock.
func (d *toyDB) catchUp(ctx context.Context) error {
	cached, err := queryInt(d.wconn, "select seq from meta")
	if err != nil {
		return d.stop(fmt.Errorf("%w: %w", errLocal, err))
	}
	if cached > d.man.Seq {
		return d.stop(fmt.Errorf("the cache has commit %v, and the log only %v", cached, d.man.Seq))
	}

	var files []changeFile
	name := d.man.Head
	for seq := d.man.Seq; seq > cached; seq-- {
		file, err := d.getChange(ctx, name)
		if err != nil {
			return err
		}
		if file.Seq != seq {
			return d.stop(fmt.Errorf("the change file %v is commit %v, where the log should have commit %v", name, file.Seq, seq))
		}
		files = append(files, file)
		name = file.Prev
	}
	if len(files) == 0 {
		return nil
	}

	apply := func() error {
		if err := d.wconn.Exec("begin immediate"); err != nil {
			return err
		}
		for i := len(files) - 1; i >= 0; i-- {
			for _, set := range files[i].Sets {
				if err := exec(d.wconn, "insert into kv (k, v) values (?, ?) on conflict (k) do update set v = excluded.v", set[0], set[1]); err != nil {
					return err
				}
			}
		}
		if err := exec(d.wconn, "update meta set seq = ?", d.man.Seq); err != nil {
			return err
		}
		return d.wconn.Exec("commit")
	}
	if err := apply(); err != nil {
		_ = d.wconn.Exec("rollback")
		return d.stop(fmt.Errorf("%w: %w", errLocal, err))
	}
	return nil
}

// getChange file by its name, checked against its content. It tries again after an injected fault.
func (d *toyDB) getChange(ctx context.Context, name string) (changeFile, error) {
	for range 10 {
		data, _, err := d.store.Get(ctx, name)
		switch {
		case errors.Is(err, errInjected):
			if err := d.host.Sleep(ctx, 100*time.Millisecond); err != nil {
				return changeFile{}, err
			}
			continue
		case err != nil:
			return changeFile{}, d.stop(fmt.Errorf("change file %v: %w", name, err))
		}
		if sum := sha256.Sum256(data); name != "changes/"+hex.EncodeToString(sum[:]) {
			return changeFile{}, d.stop(fmt.Errorf("the change file %v is not named by its content", name))
		}
		var file changeFile
		if err := json.Unmarshal(data, &file); err != nil {
			return changeFile{}, d.stop(err)
		}
		return file, nil
	}
	return changeFile{}, errInjected
}

// trySwap the manifest for next if the manifest's ETag is still etag. A next with a lease entry gets
// the wall clock of when the swap is sent. It returns the manifest as swapped in, its new ETag, and
// when the swap was sent, from which the lease that it gives counts. A swap that fails without saying
// whether it happened is resolved by reading the manifest. It fails with errSwapFailed if the
// manifest is known to be unchanged, with errPrecondition if something else changed it, and with
// errOutcomeUnknown if reading it does not tell. For a swap that commits, change is the name of its
// change file, and the swap counts as applied also when something else changed the manifest after
// it: see [toyDB.resolveSwap].
func (d *toyDB) trySwap(ctx context.Context, next manifest, etag, change string) (manifest, string, sentAt, error) {
	// A swap that has been sent is seen through to a known outcome, also when ctx is done meanwhile.
	// Abandoned halfway, it would leave the instance with an ETag that may be out of date.
	ctx = context.WithoutCancel(ctx)

	// The lease counts from when the swap is sent, because it cannot have reached the store earlier.
	at := sentAt{monotonic: d.host.Monotonic(), wall: d.host.Wall()}
	if next.Lease != nil {
		entry := *next.Lease
		entry.Wall = at.wall
		next.Lease = &entry
	}
	data, err := json.Marshal(next)
	if err != nil {
		return manifest{}, "", sentAt{}, err
	}

	newETag, err := d.store.Swap(ctx, manifestKey, data, etag)
	if d.bugs.leaseFromAnswer {
		// The planted bug.
		at = sentAt{monotonic: d.host.Monotonic(), wall: d.host.Wall()}
	}
	if err != nil && !errors.Is(err, errPrecondition) {
		newETag, err = d.resolveSwap(ctx, data, etag, change, next.Seq)
	}
	if err != nil {
		// The manifest and the time are returned also here, for a swap whose outcome is unknown.
		return next, "", at, err
	}
	return next, newETag, at, nil
}

// pendingSwap is a renewal whose outcome is unknown.
type pendingSwap struct {
	// next is the manifest that the renewal swapped in, if it happened.
	next manifest
	// etag is the ETag that the renewal swapped from.
	etag string
	// sent is when it was sent.
	sent sentAt
}

// resolvePending renewal, if there is one, by reading the manifest. It fails with errOutcomeUnknown
// while reading does not tell, and stops the instance if another instance changed the manifest. It
// requires the swap lock.
func (d *toyDB) resolvePending(ctx context.Context) error {
	p := d.pending
	if p == nil {
		return nil
	}
	data, err := json.Marshal(p.next)
	if err != nil {
		return err
	}
	etag, err := d.resolveSwap(ctx, data, p.etag, "", 0)
	switch {
	case err == nil:
		d.adopt(p.next, etag, p.sent)
	case errors.Is(err, errSwapFailed):
	case errors.Is(err, errPrecondition):
		d.pending = nil
		return d.stop(errLeaseLost)
	default:
		return err
	}
	d.pending = nil
	return nil
}

// errCommittedElsewhere is the error of a swap that committed, after which another instance changed
// the manifest.
var errCommittedElsewhere = fmt.Errorf("%w after the commit", errPrecondition)

// resolveSwap of the manifest from etag to data, whose outcome is unknown, by reading the manifest.
// It returns the manifest's ETag if the manifest is now data. If another instance changed the
// manifest in the meantime, a swap that commits the change file change as commit seq happened if and
// only if the log that the manifest points to has the change file, and then it fails with
// errCommittedElsewhere.
func (d *toyDB) resolveSwap(ctx context.Context, data []byte, etag, change string, seq int64) (string, error) {
	if d.host.Buggify(ctx, "toy: the read-back of a swap does not tell") {
		return "", errOutcomeUnknown
	}
	for range 10 {
		got, gotETag, err := d.store.Get(ctx, manifestKey)
		switch {
		case err != nil && !errors.Is(err, errNotFound):
			if err := d.host.Sleep(ctx, 100*time.Millisecond); err != nil {
				return "", err
			}
			continue
		case gotETag == etag:
			return "", errSwapFailed
		case bytes.Equal(got, data):
			return gotETag, nil
		case change == "":
			return "", errPrecondition
		}

		// Another instance may have taken the lease and committed on top, so the log is followed back
		// from its newest commit, and the manifest alone does not tell.
		var man manifest
		if err := json.Unmarshal(got, &man); err != nil {
			return "", d.stop(err)
		}
		name := man.Head
		for at := man.Seq; at >= seq; at-- {
			if name == change {
				return "", errCommittedElsewhere
			}
			file, err := d.getChange(ctx, name)
			if err != nil {
				return "", errOutcomeUnknown
			}
			name = file.Prev
		}
		return "", errPrecondition
	}
	return "", errOutcomeUnknown
}

// swapAsHolder swaps the manifest for next as the holder of the lease, and adopts it. It requires the
// swap lock. It stops the instance if something else changed the manifest, or if the outcome is
// unknown. For a swap that commits, change is the name of its change file, and it returns nil if the
// commit is in the log, also when the instance has lost the lease after it.
func (d *toyDB) swapAsHolder(ctx context.Context, next manifest, change string) error {
	swapped, etag, at, err := d.trySwap(ctx, next, d.etag, change)
	if errors.Is(err, errCommittedElsewhere) {
		_ = d.stop(errLeaseLost)
		return nil
	}
	if err := d.asHolder(err); err != nil {
		return err
	}
	d.adopt(swapped, etag, at)
	return nil
}

// asHolder turns the error of a swap by the holder of the lease into the error to return, and stops
// the instance where it must.
func (d *toyDB) asHolder(err error) error {
	switch {
	case errors.Is(err, errPrecondition):
		return d.stop(errLeaseLost)
	case errors.Is(err, errOutcomeUnknown):
		return d.stop(err)
	}
	return err
}

// renew the lease with a manifest swap, unless a commit has renewed it in the meantime and the
// renewal is not forced.
func (d *toyDB) renew(ctx context.Context, force bool) error {
	if err := d.swap.Acquire(ctx); err != nil {
		return err
	}
	if d.renewalDue() > d.host.Monotonic() && !force {
		d.swap.Release(ctx)
		return nil
	}

	// A renewal whose outcome is unknown leaves the lease as the last swap that is known to have
	// happened set it, so reads go on for as long as the clocks say that it holds, and the next swap
	// first finds out what became of the renewal.
	if err := d.resolvePending(ctx); err != nil {
		d.swap.Release(ctx)
		return err
	}

	next := d.man
	next.Lease = d.newLease(true)
	if !d.bugs.renewOutsideLock {
		swapped, etag, at, err := d.trySwap(ctx, next, d.etag, "")
		if errors.Is(err, errOutcomeUnknown) {
			d.pending = &pendingSwap{next: swapped, etag: d.etag, sent: at}
		} else if err = d.asHolder(err); err == nil {
			d.adopt(swapped, etag, at)
		}
		d.swap.Release(ctx)
		return err
	}

	// The planted bug: the swap happens without the lock, with the ETag that was read under it. It
	// differs from the code above in nothing else.
	from := d.etag
	d.swap.Release(ctx)

	swapped, etag, at, err := d.trySwap(ctx, next, from, "")
	if !errors.Is(err, errOutcomeUnknown) {
		if err := d.asHolder(err); err != nil {
			return err
		}
	}

	if err := d.swap.Acquire(ctx); err != nil {
		return err
	}
	if errors.Is(err, errOutcomeUnknown) {
		d.pending = &pendingSwap{next: swapped, etag: from, sent: at}
	} else {
		d.etag = etag
		d.man.Lease = swapped.Lease
		d.sent.Store(int64(at.monotonic))
		d.sentWall.Store(at.wall.UnixNano())
	}
	d.swap.Release(ctx)
	return err
}

// Close the database: stop serving, wait for the transactions that are open, release the lease if
// this instance holds it or may hold it, and close the cache. If the release fails, the lease times
// out.
func (d *toyDB) Close(ctx context.Context) error {
	_ = d.stop(errors.New("closed"))
	d.cancel()
	if err := d.leaser.Wait(ctx); err != nil {
		return err
	}
	if err := d.writer.Acquire(ctx); err != nil {
		return err
	}
	defer d.writer.Release(ctx)
	for range d.readerN {
		if err := d.readers.Acquire(ctx); err != nil {
			return err
		}
		defer d.readers.Release(ctx)
	}
	if err := d.swap.Acquire(ctx); err != nil {
		return err
	}
	defer d.swap.Release(ctx)

	var errs []error
	if err := d.resolvePending(ctx); err != nil {
		errs = append(errs, fmt.Errorf("release of the lease: %w", err))
	} else if d.man.Lease != nil && d.man.Lease.Holder == d.id && !errors.Is(d.stopErr(), errLeaseLost) {
		// The release leaves no lease entry, so the next instance takes the lease without a wait.
		next := d.man
		next.Lease = nil
		if _, _, _, err := d.trySwap(ctx, next, d.etag, ""); err != nil {
			errs = append(errs, fmt.Errorf("release of the lease: %w", err))
		}
	} else if d.unsure {
		man, etag, err := d.readManifest(ctx)
		if err == nil && man.Lease != nil && man.Lease.Holder == d.id {
			man.Lease = nil
			_, _, _, err = d.trySwap(ctx, man, etag, "")
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("release of the lease: %w", err))
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range append(d.idle, d.wconn) {
		errs = append(errs, c.Close())
	}
	d.idle = nil
	return errors.Join(errs...)
}

// toyTx is a write transaction.
type toyTx struct {
	d  *toyDB
	id uint64
	// seq is the number that the commit of this transaction gets.
	seq    int64
	writes [][2]int64
}

// Begin a write transaction, waiting until no other is open.
func (d *toyDB) Begin(ctx context.Context) (*toyTx, error) {
	if err := d.writer.Acquire(ctx); err != nil {
		return nil, err
	}
	if err := d.stopErr(); err != nil {
		d.writer.Release(ctx)
		return nil, err
	}
	if !d.isServing() {
		d.writer.Release(ctx)
		return nil, errNotServing
	}
	if err := d.wconn.Exec("begin immediate"); err != nil {
		d.writer.Release(ctx)
		return nil, err
	}
	seq, err := queryInt(d.wconn, "select seq from meta")
	if err != nil {
		_ = d.wconn.Exec("rollback")
		d.writer.Release(ctx)
		return nil, err
	}
	return &toyTx{d: d, id: d.host.Uint64(), seq: seq + 1}, nil
}

// Set the value at key k to the number of the transaction's commit.
func (tx *toyTx) Set(k int64) error {
	tx.writes = append(tx.writes, [2]int64{k, tx.seq})
	return exec(tx.d.wconn, "insert into kv (k, v) values (?, ?) on conflict (k) do update set v = excluded.v", k, tx.seq)
}

// Rollback the transaction.
func (tx *toyTx) Rollback(ctx context.Context) error {
	defer tx.d.writer.Release(ctx)
	return tx.d.wconn.Exec("rollback")
}

// Commit the transaction: upload its change file, swap the manifest, and commit to the cache. It
// returns nil if and only if the manifest swap happened. If the swap happened and the cache then
// fails, it stops the instance, which catches up from the log when it is opened again.
func (tx *toyTx) Commit(ctx context.Context) error {
	d := tx.d
	defer d.writer.Release(ctx)

	if err := exec(d.wconn, "update meta set seq = ?", tx.seq); err != nil {
		_ = d.wconn.Exec("rollback")
		return err
	}
	if d.bugs.cacheFirst {
		// The planted bug: the cache has the transaction before the object store does.
		if err := d.wconn.Exec("commit"); err != nil {
			return d.stop(err)
		}
	}

	if err := tx.publish(ctx); err != nil {
		if !d.bugs.cacheFirst {
			_ = d.wconn.Exec("rollback")
		}
		return err
	}
	if !d.bugs.cacheFirst {
		if err := tx.commitCache(ctx); err != nil && !d.bugs.ignoreCacheError {
			_ = d.stop(fmt.Errorf("%w: %w", errLocal, err))
		}
	}
	return nil
}

// commitCache commits the transaction to the cache. Under buggify it fails, either after rolling
// back or after committing, because SQLite can report an I/O error from a commit that is durable.
func (tx *toyTx) commitCache(ctx context.Context) error {
	d := tx.d
	if d.host.Buggify(ctx, "toy: the cache fails to commit after the manifest swap") {
		if d.host.Uint64()%2 == 0 {
			_ = d.wconn.Exec("rollback")
			return errors.New("injected after a rollback")
		}
		if err := d.wconn.Exec("commit"); err != nil {
			return err
		}
		return errors.New("injected after the commit")
	}
	return d.wconn.Exec("commit")
}

// publish the transaction to the object store: upload the change file, then swap the manifest.
func (tx *toyTx) publish(ctx context.Context) error {
	d := tx.d
	if err := d.swap.Acquire(ctx); err != nil {
		return err
	}
	defer d.swap.Release(ctx)
	if err := d.stopErr(); err != nil {
		return err
	}
	if err := d.resolvePending(ctx); err != nil {
		if errors.Is(err, errOutcomeUnknown) {
			// The commit is not sent, so it is known to have failed.
			return fmt.Errorf("an earlier renewal is unresolved: %w", errSwapFailed)
		}
		return err
	}

	file, err := json.Marshal(changeFile{ID: tx.id, Seq: tx.seq, Prev: d.man.Head, Sets: tx.writes})
	if err != nil {
		return err
	}
	sum := sha256.Sum256(file)
	name := "changes/" + hex.EncodeToString(sum[:])
	// A file with this name can only be this file, left by an upload that reported a failure.
	if err := d.store.Put(ctx, name, file); err != nil && !errors.Is(err, errExists) {
		return err
	}

	next := d.man
	next.Head, next.Seq = name, tx.seq
	next.Lease = d.newLease(false)
	return d.swapAsHolder(ctx, next, name)
}

// Read the value at key k, or 0 if there is none, and the number of the last commit in the same
// snapshot of the cache. It fails with errLeaseExpired unless both clocks show that the lease holds.
func (d *toyDB) Read(ctx context.Context, k int64) (seq, v int64, err error) {
	if err := d.readers.Acquire(ctx); err != nil {
		return 0, 0, err
	}
	defer d.readers.Release(ctx)
	if err := d.stopErr(); err != nil {
		return 0, 0, err
	}
	if !d.isServing() {
		return 0, 0, errNotServing
	}

	d.mu.Lock()
	c := d.idle[len(d.idle)-1]
	d.idle = d.idle[:len(d.idle)-1]
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.idle = append(d.idle, c)
	}()

	if d.bugs.unlockedCounter {
		d.racy++
	}

	if err := c.Exec("begin"); err != nil {
		return 0, 0, err
	}
	defer func() {
		// Ending a transaction that only read cannot lose anything.
		if rollbackErr := c.Exec("rollback"); err == nil {
			err = rollbackErr
		}
	}()

	d.host.Yield(ctx, "read: before the snapshot")
	// SQLite takes the snapshot at the first read, not at BEGIN.
	if seq, err = queryInt(c, "select seq from meta"); err != nil {
		return 0, 0, err
	}
	if !d.leaseHolds() {
		return 0, 0, errLeaseExpired
	}
	d.host.Yield(ctx, "read: after the snapshot")
	if v, err = queryInt(c, "select coalesce((select v from kv where k = ?), 0)", k); err != nil {
		return 0, 0, err
	}
	return seq, v, nil
}

// leaseHolds if less than the lease's duration has passed since the last swap that is known to have
// happened was sent, by the wall clock and by the monotonic clock. Each clock is compared only with
// itself.
func (d *toyDB) leaseHolds() bool {
	wall := d.host.Wall().Sub(time.Unix(0, d.sentWall.Load())) < d.duration
	monotonic := d.host.Monotonic()-time.Duration(d.sent.Load()) < d.duration
	switch {
	case d.bugs.wallOnly:
		return wall
	case d.bugs.monotonicOnly:
		return monotonic
	}
	return wall && monotonic
}

// exec a statement on c with integer parameters.
func exec(c *sqlite.Conn, sql string, args ...int64) error {
	_, err := queryInt(c, sql, args...)
	return err
}

// queryInt runs a statement on c with integer parameters and returns the first column of its first
// row, or 0 if it has no rows.
func queryInt(c *sqlite.Conn, sql string, args ...int64) (int64, error) {
	s, err := c.Prepare(sql)
	if err != nil {
		return 0, err
	}
	for i, arg := range args {
		if err := s.BindInt64(i+1, arg); err != nil {
			_ = s.Finalize()
			return 0, err
		}
	}
	var v int64
	row, err := s.Step()
	if err != nil {
		_ = s.Finalize()
		return 0, err
	}
	if row {
		v = s.ColumnInt64(0)
	}
	return v, s.Finalize()
}

// tracer is a host that also records a trace.
type tracer interface {
	host.Host
	Trace(ctx context.Context, format string, args ...any)
}

// intn is a random number in [0, n) from h.
func intn(h host.Host, n int) int {
	return int(h.Uint64() % uint64(n))
}

// faults that the simulated store injects, each as a number of operations in a thousand.
type faults struct {
	// errors fail the operation without applying it.
	errors int
	// lostAcks apply the operation and report a failure.
	lostAcks int
	// delays make the operation take 40 to 70 thousand units of latency longer, on the way in.
	delays int
	// delayAnswers puts a delay on the way out instead, as often as not. A delay on the way out is
	// one that the store applies the operation before.
	delayAnswers bool
}

// memStore is the simulated object store: objects in memory, latency on the way in and on the way
// out, and injected faults. Its latency is simulated time, which no instance's clock changes.
type memStore struct {
	host   tracer
	faults faults
	// unit of latency: an operation takes between 1 and 50 units each way.
	unit time.Duration

	// healed stops the faults, but not the delays of the operations that are in flight.
	healed atomic.Bool

	mu      sync.Mutex
	objects map[string][]byte
}

// etagOf an object with data. It comes from the content, as S3's ETag does, so two writes of the same
// bytes give the same ETag.
func etagOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func newMemStore(h tracer, faults faults, unit time.Duration) *memStore {
	return &memStore{host: h, faults: faults, unit: unit, objects: map[string][]byte{}}
}

// storeClient is how one instance reaches a [memStore], and is cut off from it by a partition.
type storeClient struct {
	s *memStore
	// partitioned fails every request that arrives, and every response that returns, while it is set.
	partitioned atomic.Bool
}

func (s *memStore) client() *storeClient {
	return &storeClient{s: s}
}

// do an operation, described by what for the trace: wait for the request to arrive, apply it, and
// wait for the response. An operation is never applied before its request has arrived, so nothing
// that a crashed instance sent after its crash is applied.
func (c *storeClient) do(ctx context.Context, what string, apply func() (string, error)) error {
	s := c.s
	fault := "none"
	latency := time.Duration(1+intn(s.host, 50)) * s.unit
	answerLatency := time.Duration(1+intn(s.host, 50)) * s.unit
	switch n := intn(s.host, 1000); {
	case s.healed.Load():
	case n < s.faults.errors:
		fault = "error"
	case n < s.faults.errors+s.faults.lostAcks:
		fault = "lost ack"
	case n < s.faults.errors+s.faults.lostAcks+s.faults.delays:
		delay := time.Duration(40_000+1000*intn(s.host, 30)) * s.unit
		// The number is drawn also without delayAnswers, so that a seed gives the same delays either way.
		if intn(s.host, 2) == 0 && s.faults.delayAnswers {
			answerLatency += delay
		} else {
			latency += delay
		}
	}

	if err := s.host.Sleep(ctx, latency); err != nil {
		s.host.Trace(ctx, "store %v: not applied, the request was abandoned: %v", what, err)
		return err
	}
	if c.partitioned.Load() {
		s.host.Trace(ctx, "store %v: partitioned, not applied", what)
		return errInjected
	}
	if fault == "error" {
		s.host.Trace(ctx, "store %v: injected error, not applied", what)
		return errInjected
	}

	s.mu.Lock()
	result, err := apply()
	s.mu.Unlock()
	if answerLatency > 50*s.unit {
		s.host.Trace(ctx, "store %v: applied with %v %v, the answer is delayed by %v", what, result, err, answerLatency)
	}

	if sleepErr := s.host.Sleep(ctx, answerLatency); sleepErr != nil {
		s.host.Trace(ctx, "store %v: applied with %v %v, the request was abandoned: %v", what, result, err, sleepErr)
		return sleepErr
	}
	if c.partitioned.Load() {
		s.host.Trace(ctx, "store %v: applied with %v %v, the response was lost to a partition", what, result, err)
		return errInjected
	}
	if fault == "lost ack" {
		s.host.Trace(ctx, "store %v: applied with %v %v, reported as an injected error", what, result, err)
		return errInjected
	}
	s.host.Trace(ctx, "store %v: %v %v", what, result, err)
	return err
}

func (c *storeClient) Put(ctx context.Context, key string, data []byte) error {
	return c.do(ctx, "put "+key[:min(len(key), 16)], func() (string, error) {
		if _, ok := c.s.objects[key]; ok {
			return "", errExists
		}
		c.s.objects[key] = data
		return "", nil
	})
}

func (c *storeClient) Get(ctx context.Context, key string) (data []byte, etag string, err error) {
	err = c.do(ctx, "get "+key[:min(len(key), 16)], func() (string, error) {
		o, ok := c.s.objects[key]
		if !ok {
			return "", errNotFound
		}
		data, etag = o, etagOf(o)
		return etag, nil
	})
	if err != nil {
		return nil, "", err
	}
	return data, etag, nil
}

func (c *storeClient) Swap(ctx context.Context, key string, data []byte, etag string) (newETag string, err error) {
	err = c.do(ctx, "swap "+key+" from "+etag, func() (string, error) {
		current := ""
		if o, ok := c.s.objects[key]; ok {
			current = etagOf(o)
		}
		if current != etag {
			return "", errPrecondition
		}
		newETag = etagOf(data)
		c.s.objects[key] = data
		return newETag, nil
	})
	if err != nil {
		return "", err
	}
	return newETag, nil
}

// object at key as it is in the store now, read without latency or faults.
func (s *memStore) object(key string) (data []byte, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok = s.objects[key]
	return data, ok
}

// manifest as it is in the store now, read without latency or faults, or the zero manifest if there
// is none.
func (s *memStore) manifest() (manifest, error) {
	var man manifest
	data, ok := s.object(manifestKey)
	if !ok {
		return man, nil
	}
	return man, json.Unmarshal(data, &man)
}
