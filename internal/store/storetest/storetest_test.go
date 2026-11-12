package storetest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/store"
	"maragu.dev/s4ql/internal/store/simstore"
	"maragu.dev/s4ql/internal/store/storetest"
)

// recorder is a [storetest.T] that records failures instead of reporting them.
type recorder struct {
	mu       sync.Mutex
	failures []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// wrongStore wraps a correct client and breaks it in one way.
type wrongStore struct {
	name string
	wrap func(c store.Store) store.Store
	// finds is the case of the suite that must find it, among any others that do.
	finds string
}

var wrongStores = []wrongStore{
	{
		name:  "a swap that ignores the version it is from",
		wrap:  func(c store.Store) store.Store { return &ignoreVersion{Store: c} },
		finds: "should fail a swap from an older version with ErrChanged, and keep the object",
	},
	{
		name:  "a create that overwrites an object that is there",
		wrap:  func(c store.Store) store.Store { return &overwrite{Store: c} },
		finds: "should fail a create where an object is with ErrExists, and keep the object",
	},
	{
		name:  "a read that serves what it read first",
		wrap:  func(c store.Store) store.Store { return &stale{Store: c, seen: map[string]seen{}} },
		finds: "should reflect in a read of any client every write that returned before it",
	},
	{
		name:  "a write that drops its checksum",
		wrap:  func(c store.Store) store.Store { return &dropChecksum{Store: c} },
		finds: "should refuse a create and a swap whose content does not match its checksum with ErrChecksum",
	},
	{
		name:  "a swap that creates where no object is",
		wrap:  func(c store.Store) store.Store { return &swapCreates{Store: c} },
		finds: "should fail a swap where no object is with ErrNotFound, and create nothing",
	},
}

// ignoreVersion swaps from whatever version is there.
type ignoreVersion struct{ store.Store }

func (s *ignoreVersion) Swap(ctx context.Context, key string, _ store.Version, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	for {
		body, v, err := s.Get(ctx, key)
		if err != nil {
			return "", err
		}
		_ = body.Close()
		next, err := s.Store.Swap(ctx, key, v, content, checksum)
		if !errors.Is(err, store.ErrChanged) {
			return next, err
		}
		if _, err := content.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
	}
}

// overwrite swaps where a create finds an object.
type overwrite struct{ store.Store }

func (s *overwrite) Create(ctx context.Context, key string, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	start, err := content.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", err
	}
	v, err := s.Store.Create(ctx, key, content, checksum)
	if !errors.Is(err, store.ErrExists) {
		return v, err
	}
	body, from, err := s.Get(ctx, key)
	if err != nil {
		return "", err
	}
	_ = body.Close()
	if _, err := content.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	return s.Swap(ctx, key, from, content, checksum)
}

// stale serves the first content it read of a key from then on.
type stale struct {
	store.Store
	mu   sync.Mutex
	seen map[string]seen
}

type seen struct {
	content []byte
	version store.Version
}

func (s *stale) Get(ctx context.Context, key string) (io.ReadCloser, store.Version, error) {
	s.mu.Lock()
	old, ok := s.seen[key]
	s.mu.Unlock()
	if ok {
		return io.NopCloser(bytes.NewReader(old.content)), old.version, nil
	}
	body, v, err := s.Store.Get(ctx, key)
	if err != nil {
		return nil, "", err
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	s.seen[key] = seen{content: data, version: v}
	s.mu.Unlock()
	return io.NopCloser(bytes.NewReader(data)), v, nil
}

// dropChecksum writes without the checksum it is given.
type dropChecksum struct{ store.Store }

func (s *dropChecksum) Create(ctx context.Context, key string, content io.ReadSeeker, _ *store.Checksum) (store.Version, error) {
	return s.Store.Create(ctx, key, content, nil)
}

func (s *dropChecksum) Swap(ctx context.Context, key string, from store.Version, content io.ReadSeeker, _ *store.Checksum) (store.Version, error) {
	return s.Store.Swap(ctx, key, from, content, nil)
}

// swapCreates creates the object that a swap finds missing.
type swapCreates struct{ store.Store }

func (s *swapCreates) Swap(ctx context.Context, key string, from store.Version, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	start, err := content.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", err
	}
	v, err := s.Store.Swap(ctx, key, from, content, checksum)
	if !errors.Is(err, store.ErrNotFound) {
		return v, err
	}
	if _, err := content.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	return s.Create(ctx, key, content, checksum)
}

// failures of each case of the suite against the simulated store under seeds, with each client
// wrapped by wrap, by the name of the case.
func failures(t *testing.T, seeds int, wrap func(store.Store) store.Store) map[string][]string {
	t.Helper()
	found := map[string][]string{}
	for _, c := range storetest.Cases() {
		r := &recorder{}
		for seed := range uint64(seeds) {
			synctest.Test(t, func(t *testing.T) {
				s := sim.New(seed, sim.Options{Jitter: 10 * time.Millisecond})
				st := simstore.New(s, simstore.Options{Latency: 50 * time.Millisecond})
				err := s.Run(func(ctx context.Context) {
					c.Run(r, ctx, storetest.Env{
						Host:   s,
						Client: func(name string) store.Store { return wrap(st.Client(name, nil)) },
					})
				})
				is.NotError(t, err)
			})
		}
		if len(r.failures) > 0 {
			found[c.Name] = r.failures
		}
	}
	return found
}

func TestCases(t *testing.T) {
	t.Run("should pass every case against a correct store", func(t *testing.T) {
		found := failures(t, 5, func(c store.Store) store.Store { return c })
		is.Equal(t, 0, len(found), fmt.Sprint(found))
	})

	for _, wrong := range wrongStores {
		t.Run("should fail for "+wrong.name, func(t *testing.T) {
			found := failures(t, 5, wrong.wrap)
			var names []string
			for name, failures := range found {
				names = append(names, name)
				t.Logf("%v: %v", name, failures[0])
			}
			slices.Sort(names)
			is.True(t, slices.Contains(names, wrong.finds), fmt.Sprint("the cases that failed: ", names))
		})
	}
}
