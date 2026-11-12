// Package storetest is the conformance suite of [store.Store]: tests of what every store must do,
// written once and run against each implementation, so that a simulated store and the client for
// real S3 are held to the same rules.
//
// The suite assumes a calm store: one that, in the time of a test, loses no request and is always
// reachable. It checks only what S3 promises, so a store that does more, such as one that gives the
// same content the same version every time, passes it, and so does one that does not.
//
// A test of the suite runs in a task of a [host.Host], and starts other tasks through it to race
// writes against each other, so the suite runs the same way on a simulated host and on the host for
// production.
package storetest

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

	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/store"
)

// T is the part of [testing.T] that a test of the suite uses. A test only reports failures with
// it, and never stops the goroutine, because it runs in a task of a host and not in the goroutine of
// the test.
type T interface {
	Helper()
	Errorf(format string, args ...any)
}

// Env is what one test of the suite runs with.
type Env struct {
	// Host to start tasks with, and to wait for them.
	Host host.Host
	// Client of one store, by a name for it that no other client of the test has. Each call gives a
	// new client of the same store, which is empty when the test starts.
	Client func(name string) store.Store
}

// Harness runs f once with a new, empty store, as a task of a host with the context ctx, and
// reports a failure of the harness itself on t. It passes on to f the [testing.T] that f may report
// on.
type Harness func(t *testing.T, f func(t *testing.T, ctx context.Context, env Env))

// Case of the suite: one test, by a name that says what a store should do.
type Case struct {
	Name string
	Run  func(t T, ctx context.Context, env Env)
}

// Test runs every case of the suite as a subtest, each with the store from h.
func Test(t *testing.T, h Harness) {
	for _, c := range Cases() {
		t.Run(c.Name, func(t *testing.T) {
			h(t, func(t *testing.T, ctx context.Context, env Env) {
				c.Run(t, ctx, env)
			})
		})
	}
}

// Cases of the suite, in a fixed order.
func Cases() []Case {
	return []Case{
		{Name: "should fail a read of a key with no object with ErrNotFound", Run: getNotFound},
		{Name: "should read what a create wrote, with the version that the create returned", Run: createThenGet},
		{Name: "should fail a create where an object is with ErrExists, and keep the object", Run: createExists},
		{Name: "should swap an object from its version, and read the new content and version", Run: swap},
		{Name: "should swap from the version that the last swap returned, without a read", Run: swapChain},
		{Name: "should fail a swap from an older version with ErrChanged, and keep the object", Run: swapChanged},
		{Name: "should reflect in a read of any client every write that returned before it", Run: readAfterWrite},
		{Name: "should fail a swap where no object is with ErrNotFound, and create nothing", Run: swapNotFound},
		{Name: "should refuse a create and a swap whose content does not match its checksum with ErrChecksum", Run: checksum},
		{Name: "should accept a create and a swap whose content matches its checksum", Run: checksumMatches},
		{Name: "should write the content from the current offset of the reader to its end", Run: offset},
		{Name: "should store empty content, and content of a mebibyte", Run: sizes},
		{Name: "should keep keys apart, also keys that share a prefix or are not ASCII", Run: keys},
		{Name: "should give each read its own content, unchanged by a later write", Run: snapshot},
		{Name: "should let exactly one of many racing creates of one key succeed", Run: raceCreate},
		{Name: "should let exactly one of many racing swaps from one version succeed", Run: raceSwap},
		{Name: "should lose no increment of a counter that racing clients swap", Run: raceCounter},
	}
}

// content of an object with the given text.
func content(text string) io.ReadSeeker {
	return strings.NewReader(text)
}

// read the object at key with c, and return its content and version. It reports a failure on t,
// and returns ok false, if the read or its content fails.
func read(t T, ctx context.Context, c store.Store, key string) (data []byte, v store.Version, ok bool) {
	t.Helper()
	body, v, err := c.Get(ctx, key)
	if err != nil {
		t.Errorf("get %q: %v", key, err)
		return nil, "", false
	}
	data, err = io.ReadAll(body)
	if closeErr := body.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		t.Errorf("content of %q: %v", key, err)
		return nil, "", false
	}
	if v == "" {
		t.Errorf("get %q gave an empty version", key)
		return nil, "", false
	}
	return data, v, true
}

// expect that the object at key has want as content and, unless v is empty, v as version.
func expect(t T, ctx context.Context, c store.Store, key string, want string, v store.Version) {
	t.Helper()
	data, got, ok := read(t, ctx, c, key)
	if !ok {
		return
	}
	if string(data) != want {
		t.Errorf("content of %q is %q, want %q", key, data, want)
	}
	if v != "" && got != v {
		t.Errorf("version of %q is %q, want %q", key, got, v)
	}
}

// expectNotFound at key.
func expectNotFound(t T, ctx context.Context, c store.Store, key string) {
	t.Helper()
	body, _, err := c.Get(ctx, key)
	if err == nil {
		_ = body.Close()
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get %q: got %v, want ErrNotFound", key, err)
	}
}

// create text at key with c, and report a failure of the create on t.
func create(t T, ctx context.Context, c store.Store, key, text string) (store.Version, bool) {
	t.Helper()
	v, err := c.Create(ctx, key, content(text), nil)
	if err != nil {
		t.Errorf("create %q: %v", key, err)
		return "", false
	}
	if v == "" {
		t.Errorf("create %q gave an empty version", key)
		return "", false
	}
	return v, true
}

func getNotFound(t T, ctx context.Context, env Env) {
	expectNotFound(t, ctx, env.Client("a"), "missing")
}

func createThenGet(t T, ctx context.Context, env Env) {
	a, b := env.Client("a"), env.Client("b")
	v, ok := create(t, ctx, a, "k", "hello")
	if !ok {
		return
	}
	// Another client reads it too, because the store and not the client holds the truth.
	expect(t, ctx, a, "k", "hello", v)
	expect(t, ctx, b, "k", "hello", v)
}

func createExists(t T, ctx context.Context, env Env) {
	c := env.Client("a")
	v, ok := create(t, ctx, c, "k", "first")
	if !ok {
		return
	}
	for _, text := range []string{"second", "first"} {
		if _, err := c.Create(ctx, "k", content(text), nil); !errors.Is(err, store.ErrExists) {
			t.Errorf("create %q over an object: got %v, want ErrExists", text, err)
		}
	}
	expect(t, ctx, c, "k", "first", v)
}

func swap(t T, ctx context.Context, env Env) {
	c := env.Client("a")
	if _, ok := create(t, ctx, c, "k", "one"); !ok {
		return
	}
	_, v, ok := read(t, ctx, c, "k")
	if !ok {
		return
	}
	next, err := c.Swap(ctx, "k", v, content("two"), nil)
	if err != nil {
		t.Errorf("swap: %v", err)
		return
	}
	if next == "" || next == v {
		t.Errorf("swap to other content gave the version %q, from %q", next, v)
	}
	expect(t, ctx, env.Client("b"), "k", "two", next)
}

func swapChain(t T, ctx context.Context, env Env) {
	c := env.Client("a")
	v, ok := create(t, ctx, c, "k", "0")
	if !ok {
		return
	}
	for i := 1; i <= 5; i++ {
		next, err := c.Swap(ctx, "k", v, content(fmt.Sprint(i)), nil)
		if err != nil {
			t.Errorf("swap %v: %v", i, err)
			return
		}
		v = next
	}
	expect(t, ctx, c, "k", "5", v)
}

func swapChanged(t T, ctx context.Context, env Env) {
	a, b := env.Client("a"), env.Client("b")
	old, ok := create(t, ctx, a, "k", "one")
	if !ok {
		return
	}
	v, err := b.Swap(ctx, "k", old, content("two"), nil)
	if err != nil {
		t.Errorf("swap: %v", err)
		return
	}
	// Both the content that is there and the content that was there are refused from the old
	// version, which only the content that was there has.
	for _, text := range []string{"three", "two", "one"} {
		if _, err := a.Swap(ctx, "k", old, content(text), nil); !errors.Is(err, store.ErrChanged) {
			t.Errorf("swap to %q from an older version: got %v, want ErrChanged", text, err)
		}
	}
	expect(t, ctx, a, "k", "two", v)
}

func readAfterWrite(t T, ctx context.Context, env Env) {
	a, b := env.Client("a"), env.Client("b")
	v, ok := create(t, ctx, a, "k", "0")
	if !ok {
		return
	}
	for i := 1; i <= 3; i++ {
		expect(t, ctx, a, "k", fmt.Sprint(i-1), v)
		expect(t, ctx, b, "k", fmt.Sprint(i-1), v)
		// The clients take turns, so that each reads the other's writes and its own.
		writer := a
		if i%2 == 0 {
			writer = b
		}
		next, err := writer.Swap(ctx, "k", v, content(fmt.Sprint(i)), nil)
		if err != nil {
			t.Errorf("swap %v: %v", i, err)
			return
		}
		v = next
	}
	expect(t, ctx, a, "k", "3", v)
	expect(t, ctx, b, "k", "3", v)
}

func swapNotFound(t T, ctx context.Context, env Env) {
	c := env.Client("a")
	// A version that the store gave, for another key with the same content.
	v, ok := create(t, ctx, c, "other", "content")
	if !ok {
		return
	}
	if _, err := c.Swap(ctx, "k", v, content("content"), nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("swap where no object is: got %v, want ErrNotFound", err)
	}
	expectNotFound(t, ctx, c, "k")
}

func checksum(t T, ctx context.Context, env Env) {
	c := env.Client("a")
	wrong := new(sha256.Sum256([]byte("something else")))
	if _, err := c.Create(ctx, "k", content("data"), wrong); !errors.Is(err, store.ErrChecksum) {
		t.Errorf("create with a wrong checksum: got %v, want ErrChecksum", err)
	}
	expectNotFound(t, ctx, c, "k")

	v, ok := create(t, ctx, c, "k", "data")
	if !ok {
		return
	}
	if _, err := c.Swap(ctx, "k", v, content("other data"), wrong); !errors.Is(err, store.ErrChecksum) {
		t.Errorf("swap with a wrong checksum: got %v, want ErrChecksum", err)
	}
	expect(t, ctx, c, "k", "data", v)
}

func checksumMatches(t T, ctx context.Context, env Env) {
	c := env.Client("a")
	v, err := c.Create(ctx, "k", content("data"), new(sha256.Sum256([]byte("data"))))
	if err != nil {
		t.Errorf("create with its checksum: %v", err)
		return
	}
	v, err = c.Swap(ctx, "k", v, content("more data"), new(sha256.Sum256([]byte("more data"))))
	if err != nil {
		t.Errorf("swap with its checksum: %v", err)
		return
	}
	expect(t, ctx, c, "k", "more data", v)
}

func offset(t T, ctx context.Context, env Env) {
	c := env.Client("a")
	r := strings.NewReader("skip:keep")
	if _, err := r.Seek(5, io.SeekStart); err != nil {
		t.Errorf("seek: %v", err)
		return
	}
	v, err := c.Create(ctx, "k", r, new(sha256.Sum256([]byte("keep"))))
	if err != nil {
		t.Errorf("create from an offset: %v", err)
		return
	}
	expect(t, ctx, c, "k", "keep", v)

	r = strings.NewReader("skip:swapped")
	if _, err := r.Seek(5, io.SeekStart); err != nil {
		t.Errorf("seek: %v", err)
		return
	}
	v, err = c.Swap(ctx, "k", v, r, nil)
	if err != nil {
		t.Errorf("swap from an offset: %v", err)
		return
	}
	expect(t, ctx, c, "k", "swapped", v)
}

func sizes(t T, ctx context.Context, env Env) {
	c := env.Client("a")
	if v, ok := create(t, ctx, c, "empty", ""); ok {
		expect(t, ctx, c, "empty", "", v)
	}

	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte(i * 7 / 3)
	}
	v, err := c.Create(ctx, "big", bytes.NewReader(big), new(sha256.Sum256(big)))
	if err != nil {
		t.Errorf("create of a mebibyte: %v", err)
		return
	}
	data, got, ok := read(t, ctx, c, "big")
	if ok && (!bytes.Equal(data, big) || got != v) {
		t.Errorf("a mebibyte came back as %v bytes with the version %q, and was written with %q", len(data), got, v)
	}
}

func keys(t T, ctx context.Context, env Env) {
	c := env.Client("a")
	ks := []string{"a", "a/", "a/b", "ab", "A", "æøå/ü", "a b"}
	versions := map[string]store.Version{}
	for _, k := range ks {
		v, ok := create(t, ctx, c, k, "content of "+k)
		if !ok {
			return
		}
		versions[k] = v
	}
	for _, k := range ks {
		expect(t, ctx, c, k, "content of "+k, versions[k])
	}
}

func snapshot(t T, ctx context.Context, env Env) {
	c := env.Client("a")
	v, ok := create(t, ctx, c, "k", "before")
	if !ok {
		return
	}
	body, _, err := c.Get(ctx, "k")
	if err != nil {
		t.Errorf("get: %v", err)
		return
	}
	defer func() { _ = body.Close() }()
	if _, err := c.Swap(ctx, "k", v, content("after"), nil); err != nil {
		t.Errorf("swap: %v", err)
		return
	}
	// The first read was answered before the swap was sent, so it can only be the old content, and
	// its content must not mix the two.
	data, err := io.ReadAll(body)
	if err != nil {
		t.Errorf("content: %v", err)
		return
	}
	if string(data) != "before" {
		t.Errorf("a read answered before a swap has %q, want %q", data, "before")
	}
	expect(t, ctx, c, "k", "after", "")
}

// racers is how many tasks race in a test of the suite.
const racers = 8

// race runs f in racers tasks at once, each with its own client and its number, and waits for all
// of them.
func race(t T, ctx context.Context, env Env, name string, f func(ctx context.Context, i int, c store.Store)) {
	t.Helper()
	var tasks []host.Task
	for i := range racers {
		c := env.Client(fmt.Sprint("racer-", i))
		tasks = append(tasks, env.Host.Go(ctx, fmt.Sprint(name, "-", i), func(ctx context.Context) {
			f(ctx, i, c)
		}))
	}
	for _, task := range tasks {
		if err := task.Wait(ctx); err != nil {
			t.Errorf("wait for a racer: %v", err)
		}
	}
}

func raceCreate(t T, ctx context.Context, env Env) {
	var mu sync.Mutex
	winners := map[int]store.Version{}
	race(t, ctx, env, "create", func(ctx context.Context, i int, c store.Store) {
		v, err := c.Create(ctx, "k", content(fmt.Sprint("racer ", i)), nil)
		switch {
		case err == nil:
			mu.Lock()
			winners[i] = v
			mu.Unlock()
		case !errors.Is(err, store.ErrExists):
			t.Errorf("racer %v: create: got %v, want success or ErrExists", i, err)
		}
	})
	if len(winners) != 1 {
		t.Errorf("%v of %v racing creates succeeded, want 1", len(winners), racers)
		return
	}
	for i, v := range winners {
		expect(t, ctx, env.Client("reader"), "k", fmt.Sprint("racer ", i), v)
	}
}

func raceSwap(t T, ctx context.Context, env Env) {
	from, ok := create(t, ctx, env.Client("creator"), "k", "start")
	if !ok {
		return
	}
	var mu sync.Mutex
	winners := map[int]store.Version{}
	race(t, ctx, env, "swap", func(ctx context.Context, i int, c store.Store) {
		v, err := c.Swap(ctx, "k", from, content(fmt.Sprint("racer ", i)), nil)
		switch {
		case err == nil:
			mu.Lock()
			winners[i] = v
			mu.Unlock()
		case !errors.Is(err, store.ErrChanged):
			t.Errorf("racer %v: swap: got %v, want success or ErrChanged", i, err)
		}
	})
	if len(winners) != 1 {
		t.Errorf("%v of %v racing swaps succeeded, want 1", len(winners), racers)
		return
	}
	for i, v := range winners {
		expect(t, ctx, env.Client("reader"), "k", fmt.Sprint("racer ", i), v)
	}
}

func raceCounter(t T, ctx context.Context, env Env) {
	// Each increment also names its racer, so that no two swaps write the same bytes.
	if _, ok := create(t, ctx, env.Client("creator"), "counter", "0 creator"); !ok {
		return
	}
	const increments = 4
	race(t, ctx, env, "counter", func(ctx context.Context, i int, c store.Store) {
		// Every failed swap means that another succeeded, so a racer that fails more often than all
		// racers together can succeed never sees the swaps of the others.
		attempts := 0
		for n := 0; n < increments; attempts++ {
			if attempts > 2*racers*increments {
				t.Errorf("racer %v: %v swaps failed with ErrChanged, of %v that can succeed in all", i, attempts-n, racers*increments)
				return
			}
			data, v, ok := read(t, ctx, c, "counter")
			if !ok {
				return
			}
			var count int
			if _, err := fmt.Sscan(string(data), &count); err != nil {
				t.Errorf("racer %v: the counter is %q: %v", i, data, err)
				return
			}
			_, err := c.Swap(ctx, "counter", v, content(fmt.Sprint(count+1, " racer-", i, "-", n)), nil)
			switch {
			case err == nil:
				n++
			case !errors.Is(err, store.ErrChanged):
				t.Errorf("racer %v: swap: got %v, want success or ErrChanged", i, err)
				return
			}
		}
	})
	data, _, ok := read(t, ctx, env.Client("reader"), "counter")
	if !ok {
		return
	}
	var count int
	if _, err := fmt.Sscan(string(data), &count); err != nil || count != racers*increments {
		t.Errorf("the counter is %q after %v increments", data, racers*increments)
	}
}
