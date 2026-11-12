package core_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/store"
)

// errScripted is the error of a request that a [scriptStore] made fail without an answer, so that
// its outcome is unknown to the caller.
var errScripted = errors.New("scripted fault")

// what a [scriptStore] does to a request.
type what int

const (
	// pass the request on.
	pass what = iota
	// drop the request: it is not applied, and the caller gets an error without an answer.
	drop
	// lose the answer: the request is applied, and the caller gets an error without an answer.
	lose
	// conflict: a swap is not applied, and refused as changed, as a store may refuse one that
	// races another write.
	conflict
	// hang: the request is not applied, and the caller gets no answer until its context ends.
	hang
	// stick: the request is not applied now, the caller gets an error without an answer, and the
	// test applies the request later with [scriptStore.deliver].
	stick
	// hold the answer: the request is applied now, and the caller gets the answer after a delay.
	hold
	// corrupt the content of a get: one of its bytes is changed.
	corrupt
	// breakOff the content of a get after half of it, with an error.
	breakOff
	// missing: a get finds no object.
	missing
)

// rule of a [scriptStore]: what to do to the next times requests of the operation op ("get",
// "create", or "swap") whose key contains key.
type rule struct {
	op    string
	key   string
	what  what
	times int
	// delay of the answer, for hold.
	delay time.Duration
	// find is what corrupt looks for in the content: it changes the first byte of it. If it is
	// empty, corrupt changes the byte in the middle of the content.
	find string
}

// scriptStore is a store that does to requests what a test tells it, and passes the others on to
// the store behind it. It is for scenarios that a test spells out, where the simulated store
// draws its faults from a seed.
type scriptStore struct {
	store.Store

	mu    sync.Mutex
	rules []*rule
	// seen are the requests, each as its operation and its key.
	seen []string
	// stuck are the requests that stick, each ready to be applied.
	stuck []func() error
	// unchecked are the keys of the writes that came without the checksum of their content.
	unchecked []string
}

func newScriptStore(s store.Store) *scriptStore {
	return &scriptStore{Store: s}
}

// on the next times requests of op whose key contains key, do w, with the delay if w is hold. A
// negative times means every request from now on.
func (s *scriptStore) on(op, key string, w what, times int, delay ...time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &rule{op: op, key: key, what: w, times: times}
	if len(delay) > 0 {
		r.delay = delay[0]
	}
	s.rules = append(s.rules, r)
}

// damage the next times downloads whose key contains key: the first byte of find in the content
// is changed.
func (s *scriptStore) damage(key, find string, times int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, &rule{op: "get", key: key, what: corrupt, times: times, find: find})
}

// check that a write came with the checksum of its content.
func (s *scriptStore) check(key string, data []byte, checksum *store.Checksum) {
	if checksum == nil || *checksum != sha256.Sum256(data) {
		s.mu.Lock()
		s.unchecked = append(s.unchecked, key)
		s.mu.Unlock()
	}
}

// clear the rules.
func (s *scriptStore) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = nil
}

// count the requests seen of op whose key contains key.
func (s *scriptStore) count(op, key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	for _, seen := range s.seen {
		if seenOp, seenKey, _ := strings.Cut(seen, " "); seenOp == op && strings.Contains(seenKey, key) {
			n++
		}
	}
	return n
}

// deliver the requests that stuck, in the order in which they were sent, and return the error of
// each.
func (s *scriptStore) deliver() []error {
	s.mu.Lock()
	stuck := s.stuck
	s.stuck = nil
	s.mu.Unlock()
	var errs []error
	for _, apply := range stuck {
		errs = append(errs, apply())
	}
	return errs
}

// match the request with a rule, and use the rule up.
func (s *scriptStore) match(op, key string) rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, op+" "+key)
	for _, r := range s.rules {
		if r.op == op && strings.Contains(key, r.key) && r.times != 0 {
			if r.times > 0 {
				r.times--
			}
			return *r
		}
	}
	return rule{}
}

// write a create or a swap as its rule says, where apply applies it.
func (s *scriptStore) write(ctx context.Context, r rule, apply func(ctx context.Context) (store.Version, error)) (store.Version, error) {
	switch r.what {
	case drop:
		return "", errScripted
	case lose:
		if _, err := apply(ctx); err != nil && !errors.Is(err, store.ErrChanged) && !errors.Is(err, store.ErrExists) {
			return "", err
		}
		return "", errScripted
	case conflict:
		return "", fmt.Errorf("scripted conflict: %w", store.ErrChanged)
	case hang:
		<-ctx.Done()
		return "", fmt.Errorf("%w: %w", errScripted, ctx.Err())
	case stick:
		s.mu.Lock()
		s.stuck = append(s.stuck, func() error {
			_, err := apply(context.WithoutCancel(ctx))
			return err
		})
		s.mu.Unlock()
		return "", errScripted
	case hold:
		version, err := apply(ctx)
		select {
		case <-time.After(r.delay):
			return version, err
		case <-ctx.Done():
			return "", fmt.Errorf("%w: %w", errScripted, ctx.Err())
		}
	}
	return apply(ctx)
}

func (s *scriptStore) Create(ctx context.Context, key string, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	data, err := io.ReadAll(content)
	if err != nil {
		return "", err
	}
	s.check(key, data, checksum)
	return s.write(ctx, s.match("create", key), func(ctx context.Context) (store.Version, error) {
		return s.Store.Create(ctx, key, bytes.NewReader(data), checksum)
	})
}

func (s *scriptStore) Swap(ctx context.Context, key string, from store.Version, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	data, err := io.ReadAll(content)
	if err != nil {
		return "", err
	}
	s.check(key, data, checksum)
	return s.write(ctx, s.match("swap", key), func(ctx context.Context) (store.Version, error) {
		return s.Store.Swap(ctx, key, from, bytes.NewReader(data), checksum)
	})
}

func (s *scriptStore) Get(ctx context.Context, key string) (io.ReadCloser, store.Version, error) {
	r := s.match("get", key)
	switch r.what {
	case drop:
		return nil, "", errScripted
	case hang:
		<-ctx.Done()
		return nil, "", fmt.Errorf("%w: %w", errScripted, ctx.Err())
	case missing:
		return nil, "", fmt.Errorf("scripted: %w", store.ErrNotFound)
	}
	body, version, err := s.Store.Get(ctx, key)
	if err != nil || (r.what != corrupt && r.what != breakOff) {
		return body, version, err
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		return nil, "", err
	}
	if r.what == corrupt {
		at := len(data) / 2
		if r.find != "" {
			at = bytes.Index(data, []byte(r.find))
		}
		data[at] ^= 0x01
		return io.NopCloser(bytes.NewReader(data)), version, nil
	}
	return io.NopCloser(io.MultiReader(bytes.NewReader(data[:len(data)/2]), errReader{})), version, nil
}

// errReader fails every read.
type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errScripted
}

// bubble runs f in a bubble of testing/synctest, with a calm store and a host with real
// goroutines on the bubble's fake clock, so that a test moves the time by sleeping.
func bubble(t *testing.T, f func(t *testing.T, w *world)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		f(t, newWorld(t))
	})
}

// manifest of the world's database as the store has it now.
func (w *world) manifest(t *testing.T) format.Manifest {
	t.Helper()
	data, _, ok := w.store.Object("db/manifest.json")
	is.True(t, ok, "the store has no manifest")
	m, err := format.ParseManifest(data)
	is.NotError(t, err)
	return m
}
