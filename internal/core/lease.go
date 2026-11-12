package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/store"
)

// maxManifestSize is the largest manifest that an instance reads, in bytes. A manifest is a few
// hundred bytes.
const maxManifestSize = 1 << 20

// watch is the version of the manifest that an instance without the lease waits on, and the
// reading of its monotonic clock after the read that first returned that version. When the
// manifest has stood still at that version for the duration of the holder's lease and the margin,
// the instance takes the lease with a swap from that version.
type watch struct {
	version store.Version
	since   time.Duration
}

// flight is a swap of the manifest that the instance sent as the holder of the lease, from the
// version that it knows.
type flight struct {
	// man is the manifest that the swap writes, and data its bytes.
	man  format.Manifest
	data []byte
	sum  store.Checksum
	// sent is when the swap was first sent. The lease that it gives counts from then: not from a
	// later attempt of the same swap, because the first one may be the one that the store applies,
	// and not from the answer.
	sent sentAt
	// commit is the name of the commit object that the swap makes the head of the log, if it is the
	// swap of a commit, and not a renewal.
	commit   format.Name
	isCommit bool
	// attempts counts how often the swap was sent.
	attempts int
	// unknown is whether an attempt of the swap ended without an answer.
	unknown bool
}

// newFlight is a swap that this instance sends as the holder: the manifest as it knows it, with a
// new lease entry, and with the commit object commit as the new head if isCommit. Every write of
// the manifest by the holder carries a new salt, so that it changes the bytes of the manifest, and
// with them its version: an instance that watches then sees that the holder is alive, and no
// version that a holder wrote ever comes back, so a swap that is sent from one can be applied only
// while the manifest still has it. It requires the manifest lock.
func (d *DB) newFlight(commit format.Name, isCommit bool) (*flight, error) {
	sent := d.now()
	lease := format.Lease{}
	if d.man.Lease != nil {
		// The fields of the lease that this version does not know stay, while the same holder renews.
		lease = *d.man.Lease
	}
	salt := d.host.Uint64()
	if d.bugs.keepSalt && !isCommit && d.man.Lease != nil {
		// The planted bug.
		salt = d.man.Lease.Salt
	}
	lease.Holder, lease.Duration, lease.Renewed, lease.Salt = d.id, d.options.Lease, sent.wall, salt

	next := d.man
	next.Lease = &lease
	if isCommit {
		next.Head = commit
	}
	data, err := next.Marshal()
	if err != nil {
		return nil, err
	}
	return &flight{man: next, data: data, sum: sha256.Sum256(data), sent: sent, commit: commit, isCommit: isCommit}, nil
}

// sendResult of one attempt of a swap.
type sendResult int

const (
	// sendApplied: the store applied the swap, and the instance has adopted its manifest.
	sendApplied sendResult = iota
	// sendRefused: the store did not apply this attempt, and never will.
	sendRefused
	// sendUnknown: the attempt ended without an answer. The store may have applied it, may not have,
	// and may still apply it.
	sendUnknown
)

// refusal reports whether err is one of the errors of the store that mean that a write was not
// applied.
func refusal(err error) bool {
	return errors.Is(err, store.ErrChanged) || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrExists) || errors.Is(err, store.ErrChecksum)
}

// send one attempt of the swap f, from the version that the instance knows. It requires the
// manifest lock.
func (d *DB) send(ctx context.Context, f *flight) (sendResult, error) {
	f.attempts++
	rctx, cancel := d.host.WithTimeout(ctx, d.options.StoreTimeout)
	version, err := d.store.Swap(rctx, d.manifestKey, d.version, bytes.NewReader(f.data), &f.sum)
	cancel()
	switch {
	case err == nil:
		d.adopt(f, version)
		return sendApplied, nil
	case refusal(err):
		return sendRefused, err
	}
	f.unknown = true
	for _, other := range d.flights {
		if other == f {
			return sendUnknown, err
		}
	}
	d.flights = append(d.flights, f)
	return sendUnknown, err
}

// adopt the manifest of the swap f, which the store is known to have applied, with the version
// that it has there. Every other swap in flight was sent from the version that f replaced, so none
// of them can be applied any more. It requires the manifest lock.
func (d *DB) adopt(f *flight, version store.Version) {
	d.man, d.version, d.flights, d.unsure = f.man, version, nil, false
	sent := f.sent
	if d.bugs.leaseFromAnswer {
		// The planted bug.
		sent = d.now()
	}
	d.mu.Lock()
	d.holder, d.sent = true, sent
	d.mu.Unlock()
}

// readManifest of the database and its version, or found false if there is none. It stops the
// instance for a manifest that it cannot read: one that needs a newer version of s4ql, or one that
// is malformed.
func (d *DB) readManifest(ctx context.Context) (m format.Manifest, version store.Version, found bool, err error) {
	m, version, found, err = d.fetchManifest(ctx)
	if errors.Is(err, format.ErrNewerVersion) || errors.Is(err, format.ErrMalformed) {
		err = d.stop(ctx, err)
	}
	return m, version, found, err
}

// fetchManifest of the database and its version, or found false if there is none. Its error wraps
// [format.ErrNewerVersion] or [format.ErrMalformed] for a manifest that this version cannot read,
// and is the error of the store otherwise.
func (d *DB) fetchManifest(ctx context.Context) (m format.Manifest, version store.Version, found bool, err error) {
	rctx, cancel := d.host.WithTimeout(ctx, d.options.StoreTimeout)
	defer cancel()
	body, version, err := d.store.Get(rctx, d.manifestKey)
	if errors.Is(err, store.ErrNotFound) {
		return format.Manifest{}, "", false, nil
	}
	if err != nil {
		return format.Manifest{}, "", false, err
	}
	data, err := io.ReadAll(io.LimitReader(body, maxManifestSize+1))
	_ = body.Close()
	if err != nil {
		return format.Manifest{}, "", false, err
	}
	if len(data) > maxManifestSize {
		return format.Manifest{}, "", false, fmt.Errorf("manifest: %w: it is larger than %v bytes", format.ErrMalformed, maxManifestSize)
	}
	if m, err = format.ParseManifest(data); err != nil {
		return format.Manifest{}, "", false, fmt.Errorf("manifest: %w", err)
	}
	return m, version, true, nil
}

// observed is what a read of the manifest tells a holder with swaps in flight, or one whose swap
// was refused.
type observed int

const (
	// observedNothing: the read failed.
	observedNothing observed = iota
	// observedUnchanged: the manifest has the version that the instance knows. No swap in flight has
	// been applied, and each still can be. A read alone never rules that out, because a request can
	// arrive after the read.
	observedUnchanged
	// observedLanded: the manifest is the one of a swap in flight, which the instance has adopted.
	observedLanded
	// observedLost: the manifest is another, so another instance has taken the lease, or the
	// manifest is gone. The instance no longer holds the lease, and no swap in flight can be applied
	// any more, though one may have been applied before the lease was taken.
	observedLost
)

// observe the manifest. It requires the manifest lock, and that the instance holds the lease as
// far as it knows.
func (d *DB) observe(ctx context.Context) (observed, *flight, format.Manifest, error) {
	m, version, found, err := d.readManifest(ctx)
	if err != nil {
		return observedNothing, nil, m, err
	}
	if found && version == d.version {
		return observedUnchanged, nil, m, nil
	}
	if found && m.Lease != nil && m.Lease.Holder == d.id {
		for _, f := range d.flights {
			if f.man.Lease.Salt == m.Lease.Salt && f.man.Head == m.Head {
				d.adopt(f, version)
				return observedLanded, f, m, nil
			}
		}
	}
	d.lose(ctx, m)
	return observedLost, nil, m, nil
}

// lose the lease: m is the manifest that shows that another instance has it. It requires the
// manifest lock.
func (d *DB) lose(ctx context.Context, m format.Manifest) {
	d.version, d.flights = "", nil
	d.mu.Lock()
	d.holder, d.serving = false, false
	d.wakeLocked()
	d.mu.Unlock()

	holder := ""
	if m.Lease != nil {
		holder = m.Lease.Holder
	}
	d.event(ctx, slog.LevelWarn, eventLease, slog.String(attrTransition, "lost"), slog.String(attrOutcome, "lost"), slog.String(attrLeaseHolder, holder))
}

// leaseLoop is the one goroutine of the instance. Until the instance stops, it takes the lease
// when the instance does not hold it, catches the cache up and settles the swaps whose outcome is
// unknown when the instance holds the lease and does not serve, and renews the lease while the
// instance serves. When the instance has stopped, it tries once to release the lease.
func (d *DB) leaseLoop(ctx context.Context) {
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	d.mu.Lock()
	d.cancelWork = cancel
	if d.stopped != nil {
		cancel()
	}
	d.mu.Unlock()

	for {
		d.mu.Lock()
		stopped, holder, serving := d.stopped != nil, d.holder, d.serving
		d.mu.Unlock()
		if stopped {
			break
		}

		var wait time.Duration
		switch {
		case !holder:
			wait = d.acquire(work)
		case !serving:
			wait = d.recover(work)
		default:
			wait = d.renew(work)
		}
		if wait > 0 {
			_ = d.pause(work, wait)
		}
	}

	d.releaseOnce(ctx)
}

// acquire the lease, catch up the cache, and serve, or return how long to wait before the next
// try.
//
// If the manifest has no lease entry, or names this instance, it takes the lease at once.
// Otherwise it takes the lease only when it has watched the manifest stand still for the duration
// of the holder's lease and the margin, by its own monotonic clock, and then with a swap from the
// version that it watched. If that swap is applied, nothing replaced that version, so the holder
// sent its last write before this instance first read it, and the holder's own time for reads ran
// out before the wait ended. No clock of one machine is compared with a clock of another.
func (d *DB) acquire(ctx context.Context) time.Duration {
	held := locks{d: d}
	defer held.release(ctx)
	if held.acquireWriter(ctx) != nil || held.acquireManifest(ctx) != nil {
		return 0
	}
	if d.stoppedError() != nil {
		return 0
	}

	clock := d.stopwatch()
	m, version, found, err := d.readManifest(ctx)
	if err != nil {
		return retryAfter
	}
	previous := ""
	if found && m.Lease != nil && m.Lease.Holder != d.id {
		previous = m.Lease.Holder
		if d.bugs.takeoverAfterRelease {
			// The planted bug.
			return d.options.Watch
		}
		// The wait counts from after the read, because the read shows the manifest as it was at
		// some moment before its answer came.
		now := d.host.Monotonic()
		switch {
		case version == d.watch.version:
		case d.bugs.lateVersion && d.watch.version != "":
			// The planted bug.
			d.watch.version = version
		default:
			d.watch = watch{version: version, since: now}
		}
		duration := m.Lease.Duration
		if d.bugs.ownDuration {
			// The planted bug.
			duration = d.options.Lease
		}
		if remaining := d.watch.since + duration + margin(duration) - now; remaining > 0 {
			return min(remaining, d.options.Watch)
		}
	}

	next := m
	sent := d.now()
	next.Lease = &format.Lease{Holder: d.id, Duration: d.options.Lease, Renewed: sent.wall, Salt: d.host.Uint64()}
	data, err := next.Marshal()
	if err != nil {
		return retryAfter
	}
	sum := sha256.Sum256(data)
	rctx, cancel := d.host.WithTimeout(ctx, d.options.StoreTimeout)
	var taken store.Version
	if found {
		taken, err = d.store.Swap(rctx, d.manifestKey, version, bytes.NewReader(data), &sum)
	} else {
		// The first instance of a new database creates the manifest, with an empty log.
		taken, err = d.store.Create(rctx, d.manifestKey, bytes.NewReader(data), &sum)
	}
	cancel()
	switch {
	case err == nil:
	case refusal(err):
		// The manifest changed, so its holder is alive, or another instance was faster: read it
		// again, and wait again from the new version.
		return retrySoon
	default:
		// If the request is applied after all, the manifest names this instance, and the next try
		// takes the lease at once.
		d.unsure = true
		return retryAfter
	}

	d.adopt(&flight{man: next, sent: sent}, taken)
	d.watch = watch{}
	d.event(ctx, slog.LevelInfo, eventLease, slog.String(attrTransition, "taken"), slog.String(attrOutcome, "taken"),
		slog.String(attrLeaseHolder, d.id), slog.String(attrLeasePrevious, previous), slog.Duration(attrLeaseDuration, d.options.Lease),
		slog.String(attrCommit, next.Head.String()), clock.total())
	return d.recoverLocked(ctx)
}

// recover is what an instance does that holds the lease and does not serve: see
// [DB.recoverLocked]. It returns how long to wait before the next try.
func (d *DB) recover(ctx context.Context) time.Duration {
	held := locks{d: d}
	defer held.release(ctx)
	if held.acquireWriter(ctx) != nil || held.acquireManifest(ctx) != nil {
		return 0
	}
	d.mu.Lock()
	ok := d.stopped == nil && d.holder && !d.serving
	d.mu.Unlock()
	if !ok {
		return 0
	}
	return d.recoverLocked(ctx)
}

// recoverLocked settles the swaps whose outcome is unknown, catches the cache up with the log,
// and then serves. It returns how long to wait before the next try, if it did not get that far. It
// requires the writer lock and the manifest lock, and that the instance holds the lease.
//
// A commit whose outcome is unknown is settled without pushing it: the instance reads the
// manifest. If the manifest is the one of the commit, the commit is in the log, and catching up
// applies it to the cache. If the manifest is unchanged, the instance renews the lease without the
// commit. When that renewal is applied, the manifest has a new version, the request of the commit
// is refused whenever it arrives, and the transaction is not committed. If another instance has
// taken the lease, this one takes it again in time, and catching up then applies whatever the log
// holds.
func (d *DB) recoverLocked(ctx context.Context) time.Duration {
	if len(d.flights) > 0 {
		settled := d.settling()
		o, landed, _, _ := d.observe(ctx)
		switch o {
		case observedNothing:
			return retryAfter
		case observedLost:
			d.settledEvent(ctx, settled, nil, true)
			return 0
		case observedLanded:
			d.settledEvent(ctx, settled, landed, false)
		case observedUnchanged:
			if d.bugs.settleByRead {
				// The planted bug.
				d.flights = nil
				break
			}
			if wait := d.renewLocked(ctx); len(d.flights) > 0 || !d.holds() {
				return max(wait, retrySoon)
			}
		}
	}
	if err := d.keepLease(ctx); err != nil {
		return retrySoon
	}
	if err := d.catchUp(ctx); err != nil {
		return retryAfter
	}
	if len(d.flights) > 0 || !d.holds() {
		// A renewal during the catch-up had an outcome that is unknown, or lost the lease.
		return retrySoon
	}

	d.mu.Lock()
	if d.stopped == nil {
		d.serving, d.unresolved = true, false
		d.wakeLocked()
	}
	d.mu.Unlock()
	return 0
}

// holds reports whether the instance holds the lease, as far as it knows.
func (d *DB) holds() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.holder
}

// settling describes the swaps in flight for the event that says how they were settled. It
// requires the manifest lock.
type settling struct {
	kind     string
	commit   format.Name
	attempts int
	since    time.Duration
}

func (d *DB) settling() settling {
	s := settling{kind: "renewal", since: d.host.Monotonic()}
	for _, f := range d.flights {
		s.attempts += f.attempts
		s.since = min(s.since, f.sent.monotonic)
		if f.isCommit {
			s.kind, s.commit = "commit", f.commit
		}
	}
	return s
}

// settledEvent logs how the swaps that s describes were settled: by the swap landed, which is
// known to have been applied, or by the loss of the lease.
func (d *DB) settledEvent(ctx context.Context, s settling, landed *flight, lost bool) {
	outcome := "renewed"
	switch {
	case lost:
		outcome = "lease_lost"
	case s.kind == "commit" && landed.isCommit:
		outcome = "committed"
	case s.kind == "commit":
		// A renewal was applied, which rules the swap of the commit out.
		outcome = "not_committed"
	}
	attrs := []slog.Attr{
		slog.String(attrUnknownKind, s.kind), slog.String(attrOutcome, outcome), slog.Int(attrAttempts, s.attempts),
		slog.Duration(attrDurationTotal, d.host.Monotonic()-s.since),
	}
	if s.kind == "commit" {
		attrs = append(attrs, slog.String(attrCommit, s.commit.String()))
	}
	d.event(ctx, slog.LevelWarn, eventUnknown, attrs...)
}

// renewalDue is the reading of the monotonic clock at which the lease is renewed.
func (d *DB) renewalDue() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sent.monotonic + d.options.RenewAfter
}

// renew the lease if a renewal is due, and return how long to wait before the next look. It takes
// the manifest lock only, so it runs while a write transaction is open.
func (d *DB) renew(ctx context.Context) time.Duration {
	early := d.host.Buggify(ctx, "core: the lease is renewed early")
	if wait := d.renewalDue() - d.host.Monotonic(); wait > 0 && !early {
		return wait
	}

	held := locks{d: d}
	defer held.release(ctx)
	if d.bugs.renewWaitsForWriter {
		// The planted bug.
		if held.acquireWriter(ctx) != nil {
			return 0
		}
	}
	if held.acquireManifest(ctx) != nil {
		return 0
	}
	d.mu.Lock()
	ok := d.stopped == nil && d.holder && d.serving
	d.mu.Unlock()
	if !ok {
		return 0
	}
	// A commit may have renewed the lease while this waited for the lock.
	if d.renewalDue() > d.host.Monotonic() && !early {
		return 0
	}

	if d.bugs.renewOutsideLock && len(d.flights) == 0 {
		// The planted bug: the swap is sent without the lock, from the version that was read under
		// it, and its manifest is adopted whatever happened in between.
		f, err := d.newFlight(format.Name{}, false)
		if err != nil {
			return retryAfter
		}
		from := d.version
		held.releaseManifest(ctx)
		rctx, cancel := d.host.WithTimeout(ctx, d.options.StoreTimeout)
		version, err := d.store.Swap(rctx, d.manifestKey, from, bytes.NewReader(f.data), &f.sum)
		cancel()
		if held.acquireManifest(ctx) != nil || err != nil {
			return retryAfter
		}
		d.adopt(f, version)
		return 0
	}
	return d.renewLocked(ctx)
}

// keepLease renews the lease if a renewal is due, for a holder that is busy with something long.
// It fails if the instance no longer holds the lease. It requires the manifest lock.
func (d *DB) keepLease(ctx context.Context) error {
	if d.renewalDue() <= d.host.Monotonic() {
		d.renewLocked(ctx)
	}
	if err := d.stoppedError(); err != nil {
		return err
	}
	if !d.holds() {
		return errLeaseLost
	}
	return nil
}

// renewLocked sends a renewal: a swap of the manifest for the same manifest with a new lease
// entry. If an earlier renewal had an outcome that is unknown, it sends that same swap again. It
// returns how long to wait before the next try, which is zero after a renewal that is known to
// have happened and after the loss of the lease. It requires the manifest lock.
//
// A renewal that ends without an answer leaves the lease as the last renewal that is known to
// have happened gave it, so reads go on for as long as the clocks say that it holds. After a
// refusal, the instance reads the manifest, because a refusal does not say whether an earlier
// attempt was applied, and the store may refuse a swap from the right version while another
// write to the manifest is on its way.
func (d *DB) renewLocked(ctx context.Context) time.Duration {
	clock := d.stopwatch()
	var f *flight
	for _, other := range d.flights {
		if !other.isCommit {
			f = other
		}
	}
	settled := d.settling()
	pending := len(d.flights) > 0
	if f == nil {
		var err error
		if f, err = d.newFlight(format.Name{}, false); err != nil {
			d.event(ctx, slog.LevelError, eventLease, slog.String(attrTransition, "renewed"), slog.String(attrOutcome, "failed"), errAttr(err))
			return retryAfter
		}
	}

	result, err := d.send(ctx, f)
	outcome := "renewed"
	wait := time.Duration(0)
	switch result {
	case sendApplied:
		if pending {
			d.settledEvent(ctx, settled, f, false)
		}
	case sendUnknown:
		outcome, wait = "unknown", retryAfter
	case sendRefused:
		o, landed, _, observeErr := d.observe(ctx)
		switch o {
		case observedNothing:
			outcome, wait, err = "unknown", retryAfter, observeErr
		case observedUnchanged:
			outcome, wait = "conflict", retrySoon
		case observedLanded:
			if pending {
				d.settledEvent(ctx, settled, landed, false)
			}
			err = nil
		case observedLost:
			if pending {
				d.settledEvent(ctx, settled, nil, true)
			}
			// The loss has its own event.
			return 0
		}
	}
	if outcome == "renewed" {
		err = nil
	}
	level := slog.LevelInfo
	if outcome != "renewed" {
		level = slog.LevelWarn
	}
	d.event(ctx, level, eventLease, slog.String(attrTransition, "renewed"), slog.String(attrOutcome, outcome),
		slog.String(attrLeaseHolder, d.id), slog.Int(attrAttempts, f.attempts), clock.total(), errAttr(err))
	return wait
}

// releaseOnce tries once to release the lease, if the instance holds it: it swaps the manifest for
// one without a lease entry. If the try fails, the lease times out, as after a crash.
func (d *DB) releaseOnce(ctx context.Context) {
	held := locks{d: d}
	defer held.release(ctx)
	if held.acquireManifest(ctx) != nil || d.version == "" {
		return
	}
	clock := d.stopwatch()
	err := d.releaseFrom(ctx, d.man, d.version)
	outcome := "released"
	if err != nil {
		// The manifest may have no lease entry all the same, and may still name this instance.
		outcome, d.unsure = "failed", true
	}
	d.event(ctx, slog.LevelInfo, eventLease, slog.String(attrTransition, "released"), slog.String(attrOutcome, outcome), slog.Int(attrAttempts, 1), clock.total(), errAttr(err))
}

// releaseFrom swaps the manifest m, which names this instance as the holder and has the given
// version, for the same manifest without a lease entry. It requires the manifest lock.
func (d *DB) releaseFrom(ctx context.Context, m format.Manifest, version store.Version) error {
	m.Lease = nil
	data, err := m.Marshal()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	rctx, cancel := d.host.WithTimeout(ctx, d.options.StoreTimeout)
	defer cancel()
	if _, err := d.store.Swap(rctx, d.manifestKey, version, bytes.NewReader(data), &sum); err != nil {
		return err
	}
	d.version, d.flights, d.unsure = "", nil, false
	d.mu.Lock()
	d.holder = false
	d.mu.Unlock()
	return nil
}

// release the lease when the instance is closed, until the manifest does not name this instance,
// ctx is done, or it has tried for as long as the lease lasts. Unlike [DB.releaseOnce], it reads
// the manifest first, so it also releases a lease that the instance holds without knowing it.
func (d *DB) release(ctx context.Context) error {
	held := locks{d: d}
	defer held.release(ctx)
	if err := held.acquireManifest(ctx); err != nil {
		return err
	}
	clock := d.stopwatch()
	attempts := 0
	for d.version != "" || d.unsure {
		attempts++
		m, version, found, err := d.fetchManifest(ctx)
		if errors.Is(err, format.ErrNewerVersion) || errors.Is(err, format.ErrMalformed) {
			// The manifest cannot be read, so the lease cannot be released.
			return err
		}
		if err == nil {
			if !found || m.Lease == nil || m.Lease.Holder != d.id {
				d.version, d.flights, d.unsure = "", nil, false
				return nil
			}
			err = d.releaseFrom(ctx, m, version)
		}
		if err == nil {
			d.event(ctx, slog.LevelInfo, eventLease, slog.String(attrTransition, "released"), slog.String(attrOutcome, "released"), slog.Int(attrAttempts, attempts), clock.total())
			return nil
		}
		if d.host.Monotonic()-clock.start > d.options.Lease {
			return fmt.Errorf("gave up after %v attempts: %w", attempts, err)
		}
		if sleepErr := d.host.Sleep(ctx, retrySoon); sleepErr != nil {
			return after(sleepErr, err)
		}
	}
	return nil
}
