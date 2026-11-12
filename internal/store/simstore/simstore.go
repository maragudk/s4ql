// Package simstore is a simulated object store: a [store.Store] in memory that runs under the
// simulation harness, and that does on purpose what S3 and a network do, each from the seed.
//
// It is the store that the correctness of s4ql is judged against, so where it departs from S3 it is
// less forgiving than S3, never more:
//
//   - It is strongly consistent: every request is applied at one instant, at which a read sees every
//     write applied before it, and a write is applied completely or not at all. It loses no write
//     that it has applied, serves no stale content, and changes no content.
//   - A version comes from the content, as the ETag of S3 does for a simple upload: the same content
//     gets the same version, at every key. A swap that writes the bytes that are there already
//     leaves the version as it was. The version is a keyed hash, with a key from the seed, so that
//     nothing outside the store can compute it from the content.
//   - A conditional write is checked when it arrives, not when it is sent. Of two that race, the one
//     that arrives first wins, as on S3, where "the first write operation to finish succeeds"
//     (https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html).
//   - A request travels to the store and back in simulated time. A request whose caller has given up,
//     or whose sender has crashed, is carried on by a task of the store, so that it can still be
//     applied afterwards.
//
// The faults, in [Faults], each have their own probability, and a probability of zero switches the
// fault off. With every fault off, and no client cut off by the test, no request fails. Every
// request and its outcome are in the trace of the host.
//
// A test can look at the store directly, without faults and without waiting, with [Store.Object],
// [Store.Keys], and [Store.History].
package simstore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/store"
)

// ErrFault is wrapped by the error of every request that a fault of the store or the network made
// fail. Such an error is none of the errors of [store.Store] that tell a case apart, so for a write
// its outcome is unknown.
var ErrFault = errors.New("simulated fault")

// Host is what the store runs on: a host that also keeps a trace. Under the simulation it is the
// host of the test itself, whose clock is the simulated time and whose tasks no crash ends, and not
// the host of an instance.
type Host interface {
	host.Host
	Trace(ctx context.Context, format string, args ...any)
}

// Options of a [Store].
type Options struct {
	// Latency is the longest time that a request takes on its way to the store, and again on its way
	// back. Each way takes a time from zero to Latency, from the seed.
	Latency time.Duration
	Faults  Faults
}

// Faults that the store makes happen, each with the probability that it happens to one request.
// A probability of zero switches the fault off.
type Faults struct {
	// LostRequest is the probability that a request is lost on its way to the store. It is not
	// applied, and the caller gets an error.
	LostRequest float64
	// LostResponse is the probability that the response to a request is lost on its way back. A
	// write was applied, or failed with one of the errors of [store.Store], and the caller gets
	// another error.
	LostResponse float64
	// Slow is the probability that one way of a request takes up to SlowBy longer.
	Slow   float64
	SlowBy time.Duration
	// BrokenRead is the probability that the content of a read fails at a point in the middle, or at
	// its start.
	BrokenRead float64
	// CutOff is the probability that a request cuts its client off from the store, for up to
	// CutOffFor. While a client is cut off, no request of it arrives, and no response reaches it.
	CutOff    float64
	CutOffFor time.Duration
	// Outage is the probability that a request makes the store unreachable for every client, for up
	// to OutageFor.
	Outage    float64
	OutageFor time.Duration
	// Late is the probability that a write whose caller gave up before the write arrived is still
	// applied, up to LateBy after it arrived. Otherwise it is never applied.
	Late float64
	// AfterCrash is the probability that a write whose sender crashed before the write arrived is
	// still applied, up to LateBy after it arrived. Otherwise it is never applied.
	AfterCrash float64
	LateBy     time.Duration
}

// AllFaults on, each often enough to happen in most runs of a few hundred requests, with durations
// that are long against a lease of 30 seconds.
func AllFaults() Faults {
	return Faults{
		LostRequest:  0.02,
		LostResponse: 0.02,
		Slow:         0.02,
		SlowBy:       45 * time.Second,
		BrokenRead:   0.02,
		CutOff:       0.005,
		CutOffFor:    40 * time.Second,
		Outage:       0.002,
		OutageFor:    20 * time.Second,
		Late:         0.5,
		AfterCrash:   0.5,
		LateBy:       60 * time.Second,
	}
}

// Store is the simulated store. Its clients, from [Store.Client], implement [store.Store].
type Store struct {
	host    Host
	options Options
	// key of the hash that gives a version.
	key []byte

	mu      sync.Mutex
	objects map[string]object
	history []Write
	// requests counts the requests that were sent.
	requests int
	// carriers are the tasks that carry requests, in the order in which they started.
	carriers []host.Task
	// outageUntil is the simulated time until which no client reaches the store.
	outageUntil time.Duration
	clients     map[string]bool
}

// object at a key.
type object struct {
	content []byte
	version store.Version
}

// Write that the store applied.
type Write struct {
	// Time at which the write was applied, on the monotonic clock of the store's host.
	Time time.Duration
	// Client that sent the write, by its name.
	Client string
	// Request is the number of the request in the trace.
	Request int
	Key     string
	// Create is true for a create, and false for a swap.
	Create bool
	// From is the version that a swap was from.
	From store.Version
	// Version of the object after the write.
	Version store.Version
	Content []byte
}

// New store on h, which is empty. Its random numbers are drawn from h.
func New(h Host, options Options) *Store {
	key := binary.LittleEndian.AppendUint64(nil, h.Uint64())
	return &Store{host: h, options: options, key: key, objects: map[string]object{}, clients: map[string]bool{}}
}

// version of content.
func (s *Store) version(content []byte) store.Version {
	mac := hmac.New(sha256.New, s.key)
	mac.Write(content)
	return store.Version(hex.EncodeToString(mac.Sum(nil)[:16]))
}

// Object at key as it is now, read without faults and without waiting, and whether there is one.
func (s *Store) Object(key string) (content []byte, version store.Version, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	return slices.Clone(o.content), o.version, ok
}

// Keys of the objects as they are now, in order.
func (s *Store) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.objects {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// History of the writes that the store applied, in the order in which it applied them.
func (s *Store) History() []Write {
	s.mu.Lock()
	defer s.mu.Unlock()
	history := slices.Clone(s.history)
	for i := range history {
		history[i].Content = slices.Clone(history[i].Content)
	}
	return history
}

// Wait until every request that was sent has been applied or lost, including those that are
// carried on after their callers gave up. Requests that are sent while it waits are waited for too.
func (s *Store) Wait(ctx context.Context) error {
	for i := 0; ; i++ {
		s.mu.Lock()
		if i >= len(s.carriers) {
			s.mu.Unlock()
			return nil
		}
		t := s.carriers[i]
		s.mu.Unlock()
		if err := t.Wait(ctx); err != nil {
			return err
		}
	}
}

// Outage of the store for d from now, in which no client reaches it.
func (s *Store) Outage(ctx context.Context, d time.Duration) {
	s.mu.Lock()
	s.outageUntil = max(s.outageUntil, s.host.Monotonic()+d)
	s.mu.Unlock()
	s.host.Trace(ctx, "store: unreachable for %v", d)
}

// Sender of requests, as the instance of the simulation that a client belongs to.
type Sender interface {
	// Crashed reports whether the sender has crashed.
	Crashed() bool
}

// Client of the store, by a name that no other client of the store has, for the requests of
// sender. If sender is nil, the client never crashes.
func (s *Store) Client(name string, sender Sender) *Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients[name] {
		panic("simstore: a client named " + name + " exists already")
	}
	s.clients[name] = true
	return &Client{s: s, name: name, sender: sender}
}

// chance reports whether something with the probability p happens, from the seed. It draws no
// number for a probability of zero, so a fault that is off leaves the random numbers as they were.
func (s *Store) chance(p float64) bool {
	if p <= 0 {
		return false
	}
	return float64(s.host.Uint64()>>11)/(1<<53) < p
}

// upTo is a duration from zero to d, from the seed.
func (s *Store) upTo(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(s.host.Uint64() % uint64(d+1))
}

// short form of a version for the trace.
func short(v store.Version) string {
	return string(v[:min(len(v), 8)])
}

// errorf is an error of a fault.
func errorf(format string, args ...any) error {
	return fmt.Errorf("%w: %v", ErrFault, fmt.Sprintf(format, args...))
}
