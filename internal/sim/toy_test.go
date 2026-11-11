package sim_test

// A toy library with the shape of s4ql, in plain blocking Go: the truth is a log in an object store
// behind a compare-and-swap manifest, a SQLite database is the cache that serves reads, one write
// transaction runs at a time, and a background task renews a lease. It exists to exercise the
// scheduler and is not a design for s4ql.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/sqlite"
)

const (
	manifestKey   = "manifest.json"
	leaseDuration = 30 * time.Second
)

var (
	errNotFound     = errors.New("object not found")
	errExists       = errors.New("object exists")
	errPrecondition = errors.New("precondition failed")
	errInjected     = errors.New("injected fault")
	errSwapFailed   = errors.New("manifest swap failed")
	errLeaseLost    = errors.New("lease lost")
	errLeaseExpired = errors.New("lease expired")
	errStopped      = errors.New("stopped")
	errClosed       = errors.New("closed")
)

// env is what the toy library needs from the world: tasks, time, locks, and randomness.
type env interface {
	Go(ctx context.Context, name string, f func(ctx context.Context)) sim.Task
	Yield(ctx context.Context, site string)
	Sleep(ctx context.Context, d time.Duration) error
	Now() time.Time
	NewSemaphore(name string, n int) sim.Semaphore
	Uint64() uint64
}

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
	// Holder of the lease, or empty.
	Holder  string    `json:"holder"`
	Expires time.Time `json:"expires"`
}

// changeFile is one commit in the log.
type changeFile struct {
	Seq int64 `json:"seq"`
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
}

// toyOptions for opening a [toyDB].
type toyOptions struct {
	// readers is the number of read connections.
	readers int
	// busyTimeout in milliseconds for every connection.
	busyTimeout int
	// synchronousOff stops SQLite from syncing the cache to disk.
	synchronousOff bool
	bugs           bugs
}

// toyDB is the toy database.
type toyDB struct {
	env   env
	store objectStore
	bugs  bugs
	// id of this instance as holder of the lease.
	id string

	// writer lets one write transaction in at a time, and guards wconn.
	writer sim.Semaphore
	wconn  *sqlite.Conn
	// swap lets one manifest swap be in flight at a time, and guards etag and man.
	swap sim.Semaphore
	etag string
	man  manifest
	// readers has one permit for every read connection.
	readers sim.Semaphore

	// mu guards idle and stopped. It is never held while waiting.
	mu      sync.Mutex
	idle    []*sqlite.Conn
	stopped error

	// deadline of the lease in nanoseconds since the Unix epoch, measured from when the last successful
	// swap was sent.
	deadline atomic.Int64
	// racy is the counter of the unlockedCounter bug.
	racy int

	cancel  context.CancelFunc
	renewer sim.Task
}

// openToy database with a new cache at path. It takes the lease and starts the renewal task.
func openToy(ctx context.Context, e env, store objectStore, path string, options toyOptions) (_ *toyDB, err error) {
	d := &toyDB{
		env:     e,
		store:   store,
		bugs:    options.bugs,
		id:      fmt.Sprintf("%016x", e.Uint64()),
		writer:  e.NewSemaphore("writer", 1),
		swap:    e.NewSemaphore("swap", 1),
		readers: e.NewSemaphore("readers", options.readers),
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
			for _, c := range append(d.idle, d.wconn) {
				if c != nil {
					_ = c.Close()
				}
			}
		}
	}()

	if d.wconn, err = open(); err != nil {
		return nil, err
	}
	for _, sql := range []string{
		"create table kv (k integer primary key, v integer not null) strict",
		"create table meta (id integer primary key check (id = 1), seq integer not null) strict",
		"insert into meta (id, seq) values (1, 0)",
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

	if err := d.takeLease(ctx); err != nil {
		return nil, err
	}

	ctx, d.cancel = context.WithCancel(ctx)
	d.renewer = e.Go(ctx, "renewer of "+d.id, d.renewLoop)
	return d, nil
}

// takeLease in the manifest, creating the manifest if there is none. It tries again after an injected
// fault.
func (d *toyDB) takeLease(ctx context.Context) error {
	if err := d.swap.Acquire(ctx); err != nil {
		return err
	}
	defer d.swap.Release(ctx)

	for {
		var man manifest
		data, etag, err := d.store.Get(ctx, manifestKey)
		switch {
		case errors.Is(err, errNotFound):
		case err == nil:
			if err := json.Unmarshal(data, &man); err != nil {
				return err
			}
		case errors.Is(err, errInjected):
			if err := d.env.Sleep(ctx, time.Second); err != nil {
				return err
			}
			continue
		default:
			return err
		}
		if man.Holder != "" && man.Holder != d.id && d.env.Now().Before(man.Expires) {
			return errors.New("lease held by another instance")
		}
		if man.Seq != 0 {
			return errors.New("the toy cannot catch its cache up")
		}

		man.Holder = d.id
		d.etag = etag
		err = d.swapManifest(ctx, man)
		if errors.Is(err, errSwapFailed) {
			continue
		}
		return err
	}
}

// swapManifest for next, and take over its ETag and the new lease deadline. It requires the swap
// lock. It fails with errSwapFailed if the manifest is known to be unchanged, and stops the instance
// with errLeaseLost if something else changed the manifest.
func (d *toyDB) swapManifest(ctx context.Context, next manifest) error {
	swapped, etag, err := d.trySwap(ctx, next, d.etag)
	if err != nil {
		return err
	}
	d.etag, d.man = etag, swapped
	d.deadline.Store(swapped.Expires.UnixNano())
	return nil
}

// trySwap the manifest for next, with a lease that lasts from now, if the manifest's ETag is still
// etag. It returns the manifest as swapped in and its new ETag. A swap that fails without saying
// whether it happened is resolved by reading the manifest. It fails with errSwapFailed if the
// manifest is known to be unchanged, and stops the instance with errLeaseLost if something else
// changed the manifest.
func (d *toyDB) trySwap(ctx context.Context, next manifest, etag string) (manifest, string, error) {
	// A swap that has been sent is seen through to a known outcome, also when ctx is done meanwhile.
	// Abandoned halfway, it would leave the instance with an ETag that may be out of date.
	ctx = context.WithoutCancel(ctx)

	// The lease counts from when the swap is sent, because it cannot have reached the store earlier.
	next.Expires = d.env.Now().Add(leaseDuration)
	data, err := json.Marshal(next)
	if err != nil {
		return manifest{}, "", err
	}

	newETag, err := d.store.Swap(ctx, manifestKey, data, etag)
	if err != nil && !errors.Is(err, errPrecondition) {
		newETag, err = d.resolveSwap(ctx, data, etag)
	}
	if errors.Is(err, errPrecondition) {
		return manifest{}, "", d.stop(errLeaseLost)
	}
	if err != nil {
		return manifest{}, "", err
	}
	return next, newETag, nil
}

// resolveSwap of the manifest from etag to data, whose outcome is unknown, by reading the manifest.
// It returns the manifest's ETag if the manifest is now data.
func (d *toyDB) resolveSwap(ctx context.Context, data []byte, etag string) (string, error) {
	for range 10 {
		got, gotETag, err := d.store.Get(ctx, manifestKey)
		switch {
		case err != nil && !errors.Is(err, errNotFound):
			if err := d.env.Sleep(ctx, 100*time.Millisecond); err != nil {
				return "", err
			}
		case gotETag == etag:
			return "", errSwapFailed
		case bytes.Equal(got, data):
			return gotETag, nil
		default:
			return "", errPrecondition
		}
	}
	return "", d.stop(errors.New("the outcome of a manifest swap is unknown"))
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

// renewLoop is the renewal task: it renews the lease when a renewal is due, until ctx is done or the
// instance has stopped.
func (d *toyDB) renewLoop(ctx context.Context) {
	for d.stopErr() == nil {
		if wait := d.renewalDue().Sub(d.env.Now()); wait > 0 {
			if err := d.env.Sleep(ctx, wait); err != nil {
				return
			}
			// A commit may have renewed the lease in the meantime.
			continue
		}
		if err := d.renew(ctx); err != nil {
			// Try again shortly, unless the instance has stopped.
			if err := d.env.Sleep(ctx, time.Second); err != nil {
				return
			}
		}
	}
}

// renewalDue is the time of the next renewal, which is 20 seconds after the last successful swap was
// sent.
func (d *toyDB) renewalDue() time.Time {
	return time.Unix(0, d.deadline.Load()).Add(20*time.Second - leaseDuration)
}

// renew the lease with a manifest swap, unless a commit has renewed it in the meantime.
func (d *toyDB) renew(ctx context.Context) error {
	if err := d.swap.Acquire(ctx); err != nil {
		return err
	}
	if d.renewalDue().After(d.env.Now()) {
		d.swap.Release(ctx)
		return nil
	}

	if !d.bugs.renewOutsideLock {
		err := d.swapManifest(ctx, d.man)
		d.swap.Release(ctx)
		return err
	}

	// The planted bug: the swap happens without the lock, with the ETag that was read under it. It
	// differs from the code above in nothing else.
	next, etag := d.man, d.etag
	d.swap.Release(ctx)

	swapped, etag, err := d.trySwap(ctx, next, etag)
	if err != nil {
		return err
	}

	if err := d.swap.Acquire(ctx); err != nil {
		return err
	}
	d.etag = etag
	d.man.Expires = swapped.Expires
	d.deadline.Store(swapped.Expires.UnixNano())
	d.swap.Release(ctx)
	return nil
}

// Close the database: stop the renewal, release the lease, and close the cache. If it fails with an
// error from the object store, the lease is not released and times out.
func (d *toyDB) Close(ctx context.Context) error {
	d.cancel()
	if err := d.renewer.Wait(ctx); err != nil {
		return err
	}
	if err := d.writer.Acquire(ctx); err != nil {
		return err
	}
	defer d.writer.Release(ctx)
	if err := d.swap.Acquire(ctx); err != nil {
		return err
	}
	defer d.swap.Release(ctx)

	var errs []error
	if d.stopErr() == nil {
		next := d.man
		next.Holder = ""
		errs = append(errs, d.swapManifest(ctx, next))
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped == nil {
		d.stopped = fmt.Errorf("%w: %w", errStopped, errClosed)
	}
	for _, c := range append(d.idle, d.wconn) {
		errs = append(errs, c.Close())
	}
	d.idle = nil
	return errors.Join(errs...)
}

// toyTx is a write transaction.
type toyTx struct {
	d *toyDB
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
	return &toyTx{d: d, seq: seq + 1}, nil
}

// Set the value at key k to the number of the transaction's commit.
func (tx *toyTx) Set(k int64) error {
	tx.writes = append(tx.writes, [2]int64{k, tx.seq})
	return exec(tx.d.wconn, "insert into kv (k, v) values (?, ?) on conflict (k) do update set v = excluded.v", k, tx.seq)
}

// Get the value at key k as the transaction sees it, or 0 if there is none.
func (tx *toyTx) Get(k int64) (int64, error) {
	return queryInt(tx.d.wconn, "select coalesce((select v from kv where k = ?), 0)", k)
}

// Rollback the transaction.
func (tx *toyTx) Rollback(ctx context.Context) error {
	defer tx.d.writer.Release(ctx)
	return tx.d.wconn.Exec("rollback")
}

// Commit the transaction: upload its change file, swap the manifest, and commit to the cache. It
// returns nil only if the manifest swap happened and the cache has the transaction. If the swap
// happened and the cache then fails, it stops the instance.
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
		if err := d.wconn.Exec("commit"); err != nil {
			return d.stop(err)
		}
	}
	return nil
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

	file, err := json.Marshal(changeFile{Seq: tx.seq, Prev: d.man.Head, Sets: tx.writes})
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
	return d.swapManifest(ctx, next)
}

// Read the value at key k, or 0 if there is none, and the number of the last commit in the same
// snapshot of the cache.
func (d *toyDB) Read(ctx context.Context, k int64) (seq, v int64, err error) {
	if err := d.readers.Acquire(ctx); err != nil {
		return 0, 0, err
	}
	defer d.readers.Release(ctx)
	if err := d.stopErr(); err != nil {
		return 0, 0, err
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

	d.env.Yield(ctx, "read: before the snapshot")
	// SQLite takes the snapshot at the first read, not at BEGIN.
	if seq, err = queryInt(c, "select seq from meta"); err != nil {
		return 0, 0, err
	}
	// The lease is checked after the snapshot, so that a pause before the snapshot cannot hide that
	// the lease ran out.
	if !d.env.Now().Before(time.Unix(0, d.deadline.Load())) {
		return 0, 0, errLeaseExpired
	}
	d.env.Yield(ctx, "read: after the snapshot")
	if v, err = queryInt(c, "select coalesce((select v from kv where k = ?), 0)", k); err != nil {
		return 0, 0, err
	}
	return seq, v, nil
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

// tracer is an env that also records a trace.
type tracer interface {
	env
	Trace(ctx context.Context, format string, args ...any)
}

// intn is a random number in [0, n) from e.
func intn(e env, n int) int {
	return int(e.Uint64() % uint64(n))
}

// faults that the simulated store injects, each as a number of operations in a thousand.
type faults struct {
	// errors fail the operation without applying it.
	errors int
	// lostAcks apply the operation and report a failure.
	lostAcks int
	// delays make the operation take longer than the lease lasts.
	delays int
}

// memStore is the simulated object store: objects in memory, latency on the way in and on the way
// out, and injected faults.
type memStore struct {
	env    tracer
	faults faults
	// unit of latency: an operation takes between 1 and 50 units each way.
	unit time.Duration

	mu      sync.Mutex
	objects map[string]memObject
	// etags counts the ETags handed out.
	etags int
}

type memObject struct {
	data []byte
	etag string
}

func newMemStore(e tracer, faults faults, unit time.Duration) *memStore {
	return &memStore{env: e, faults: faults, unit: unit, objects: map[string]memObject{}}
}

// do an operation, described by what for the trace: wait for the request to arrive, apply it, and
// wait for the response.
func (s *memStore) do(ctx context.Context, what string, apply func() (string, error)) error {
	fault := "none"
	latency := time.Duration(1+intn(s.env, 50)) * s.unit
	switch n := intn(s.env, 1000); {
	case n < s.faults.errors:
		fault = "error"
	case n < s.faults.errors+s.faults.lostAcks:
		fault = "lost ack"
	case n < s.faults.errors+s.faults.lostAcks+s.faults.delays:
		// With a unit of a millisecond, this is 40 to 70 seconds, which is longer than the lease lasts.
		latency += time.Duration(40_000+1000*intn(s.env, 30)) * s.unit
	}

	if err := s.env.Sleep(ctx, latency); err != nil {
		s.env.Trace(ctx, "store %v: not applied, the request was abandoned: %v", what, err)
		return err
	}
	if fault == "error" {
		s.env.Trace(ctx, "store %v: injected error, not applied", what)
		return errInjected
	}

	s.mu.Lock()
	result, err := apply()
	s.mu.Unlock()

	if sleepErr := s.env.Sleep(ctx, time.Duration(1+intn(s.env, 50))*s.unit); sleepErr != nil {
		s.env.Trace(ctx, "store %v: applied with %v %v, the request was abandoned: %v", what, result, err, sleepErr)
		return sleepErr
	}
	if fault == "lost ack" {
		s.env.Trace(ctx, "store %v: applied with %v %v, reported as an injected error", what, result, err)
		return errInjected
	}
	s.env.Trace(ctx, "store %v: %v %v", what, result, err)
	return err
}

func (s *memStore) Put(ctx context.Context, key string, data []byte) error {
	return s.do(ctx, "put "+key[:min(len(key), 16)], func() (string, error) {
		if _, ok := s.objects[key]; ok {
			return "", errExists
		}
		s.etags++
		s.objects[key] = memObject{data: data, etag: fmt.Sprint("v", s.etags)}
		return "", nil
	})
}

func (s *memStore) Get(ctx context.Context, key string) (data []byte, etag string, err error) {
	err = s.do(ctx, "get "+key[:min(len(key), 16)], func() (string, error) {
		o, ok := s.objects[key]
		if !ok {
			return "", errNotFound
		}
		data, etag = o.data, o.etag
		return etag, nil
	})
	if err != nil {
		return nil, "", err
	}
	return data, etag, nil
}

func (s *memStore) Swap(ctx context.Context, key string, data []byte, etag string) (newETag string, err error) {
	err = s.do(ctx, "swap "+key+" from "+etag, func() (string, error) {
		if s.objects[key].etag != etag {
			return "", errPrecondition
		}
		s.etags++
		newETag = fmt.Sprint("v", s.etags)
		s.objects[key] = memObject{data: data, etag: newETag}
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
	o, ok := s.objects[key]
	return o.data, ok
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
