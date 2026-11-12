package simstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"maragu.dev/s4ql/internal/store"
)

// Client of a [Store], through which one sender reaches it, and which a test can cut off from it.
type Client struct {
	s      *Store
	name   string
	sender Sender
	// cutUntil is the simulated time until which the client is cut off. It is guarded by the store's
	// mutex.
	cutUntil time.Duration
}

var _ store.Store = (*Client)(nil)

// CutOff the client from the store for d from now, so that no request of it arrives and no response
// reaches it.
func (c *Client) CutOff(ctx context.Context, d time.Duration) {
	s := c.s
	s.mu.Lock()
	c.cutUntil = max(c.cutUntil, s.host.Monotonic()+d)
	s.mu.Unlock()
	s.host.Trace(ctx, "store: client %v cut off for %v", c.name, d)
}

// kind of a request.
type kind int

const (
	get kind = iota
	create
	swap
)

// request from a client to the store.
type request struct {
	id   int
	kind kind
	key  string
	// from, content, and checksum are those of a write.
	from     store.Version
	content  []byte
	checksum *store.Checksum

	// The fields from here on are guarded by the store's mutex.

	// gaveUp is set when the caller stops waiting for the response.
	gaveUp bool
	// version, and for a get its content and breakAt, are the result, unless err is not nil.
	version store.Version
	// breakAt is where the content of a get breaks off, or -1.
	breakAt int
	err     error
}

// String describes the request for the trace.
func (r *request) String() string {
	switch r.kind {
	case get:
		return fmt.Sprintf("request %v: get %v", r.id, r.key)
	case create:
		return fmt.Sprintf("request %v: create %v with %v bytes%v", r.id, r.key, len(r.content), r.checksumText())
	default:
		return fmt.Sprintf("request %v: swap %v from %v to %v bytes%v", r.id, r.key, short(r.from), len(r.content), r.checksumText())
	}
}

func (r *request) checksumText() string {
	if r.checksum == nil {
		return ""
	}
	return " and a checksum"
}

// Get as [store.Store] describes.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, store.Version, error) {
	checkKey(key)
	r := &request{kind: get, key: key}
	if err := c.send(ctx, r); err != nil {
		return nil, "", err
	}
	return &body{ctx: ctx, content: r.content, breakAt: r.breakAt, request: r.id}, r.version, nil
}

// Create as [store.Store] describes.
func (c *Client) Create(ctx context.Context, key string, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	checkKey(key)
	data, err := readContent(content)
	if err != nil {
		return "", err
	}
	r := &request{kind: create, key: key, content: data, checksum: checksum}
	if err := c.send(ctx, r); err != nil {
		return "", err
	}
	return r.version, nil
}

// Swap as [store.Store] describes. It panics for an empty version, which no read or write returns.
func (c *Client) Swap(ctx context.Context, key string, from store.Version, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	checkKey(key)
	if from == "" {
		panic("simstore: a swap from an empty version")
	}
	data, err := readContent(content)
	if err != nil {
		return "", err
	}
	r := &request{kind: swap, key: key, from: from, content: data, checksum: checksum}
	if err := c.send(ctx, r); err != nil {
		return "", err
	}
	return r.version, nil
}

// checkKey panics for a key that S3 refuses, or that S3 and its SDKs may treat as another key: one
// that is empty, longer than 1024 bytes, not valid UTF-8, or has a path segment that is "." or "..".
// See https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-keys.html.
func checkKey(key string) {
	if key == "" || len(key) > 1024 || !utf8.ValidString(key) {
		panic(fmt.Sprintf("simstore: the key %q is empty, longer than 1024 bytes, or not UTF-8", key))
	}
	for segment := range strings.SplitSeq(key, "/") {
		if segment == "." || segment == ".." {
			panic(fmt.Sprintf("simstore: the key %q has a path segment that is %q", key, segment))
		}
	}
}

// readContent of a write from its current offset to its end. It finds the size by seeking first,
// as a client for S3 must, which needs the size before it sends the content.
func readContent(content io.ReadSeeker) ([]byte, error) {
	start, err := content.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, fmt.Errorf("simstore: seek in the content: %w", err)
	}
	end, err := content.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("simstore: seek in the content: %w", err)
	}
	if _, err := content.Seek(start, io.SeekStart); err != nil {
		return nil, fmt.Errorf("simstore: seek in the content: %w", err)
	}
	if end < start {
		return nil, fmt.Errorf("simstore: the content ends at %v, before its offset %v", end, start)
	}
	data := make([]byte, end-start)
	if _, err := io.ReadFull(content, data); err != nil {
		return nil, fmt.Errorf("simstore: read the content: %w", err)
	}
	return data, nil
}

// send the request r and wait for its response. A task of the store carries the request, so that
// it goes on after the caller gives up or its sender crashes. The result is in r when send returns
// nil.
func (c *Client) send(ctx context.Context, r *request) error {
	s := c.s
	// A request whose context is done is never sent, as an HTTP client sends none.
	if err := context.Cause(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	s.requests++
	r.id = s.requests
	s.mu.Unlock()
	s.host.Trace(ctx, "store: %v from %v: sent", r, c.name)

	carrier := s.host.Go(context.WithoutCancel(ctx), fmt.Sprint("store request ", r.id), func(ctx context.Context) {
		s.carry(ctx, c, r)
	})
	s.mu.Lock()
	s.carriers = append(s.carriers, carrier)
	s.mu.Unlock()

	if err := carrier.Wait(ctx); err != nil {
		s.mu.Lock()
		r.gaveUp = true
		s.mu.Unlock()
		s.host.Trace(ctx, "store: request %v: the caller gives up: %v", r.id, err)
		return fmt.Errorf("simstore: request %v: %w", r.id, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return r.err
}

// carry the request r of c to the store, apply it, and carry the response back, with the faults
// that the seed picks.
func (s *Store) carry(ctx context.Context, c *Client, r *request) {
	f := s.options.Faults
	trace := func(format string, args ...any) {
		s.host.Trace(ctx, "store: request %v: "+format, append([]any{r.id}, args...)...)
	}
	fail := func(err error) {
		s.mu.Lock()
		r.err = err
		s.mu.Unlock()
		trace("%v", err)
	}

	if s.chance(f.CutOff) {
		c.CutOff(ctx, s.upTo(f.CutOffFor))
	}
	if s.chance(f.Outage) {
		s.Outage(ctx, s.upTo(f.OutageFor))
	}

	// The way there.
	_ = s.host.Sleep(ctx, s.way())

	s.mu.Lock()
	gaveUp := r.gaveUp
	s.mu.Unlock()
	crashed := c.sender != nil && c.sender.Crashed()
	if crashed || gaveUp {
		why, p := "the caller gave up", f.Late
		if crashed {
			why, p = "the sender crashed", f.AfterCrash
		}
		if r.kind == get || !s.chance(p) {
			trace("arrives after %v, and is never applied", why)
			return
		}
		late := s.upTo(f.LateBy)
		trace("arrives after %v, and is applied %v later", why, late)
		_ = s.host.Sleep(ctx, late)
	}

	if s.unreachable(c) {
		fail(errorf("request %v of %v did not reach the store, which was unreachable", r.id, c.name))
		return
	}
	if s.chance(f.LostRequest) {
		fail(errorf("request %v of %v was lost on its way to the store", r.id, c.name))
		return
	}
	s.apply(ctx, c, r)

	// The way back.
	_ = s.host.Sleep(ctx, s.way())

	s.mu.Lock()
	gaveUp = r.gaveUp
	s.mu.Unlock()
	switch {
	case gaveUp || (c.sender != nil && c.sender.Crashed()):
		trace("the response finds no caller")
	case s.unreachable(c):
		fail(errorf("the response to request %v of %v was lost, because the store was unreachable", r.id, c.name))
	case s.chance(f.LostResponse):
		fail(errorf("the response to request %v of %v was lost on its way back", r.id, c.name))
	default:
		trace("the response arrives")
	}
}

// way is the time that one way of a request takes.
func (s *Store) way() time.Duration {
	d := s.upTo(s.options.Latency)
	if s.chance(s.options.Faults.Slow) {
		d += s.upTo(s.options.Faults.SlowBy)
	}
	return d
}

// unreachable reports whether c cannot reach the store now.
func (s *Store) unreachable(c *Client) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.host.Monotonic()
	return now < c.cutUntil || now < s.outageUntil
}

// apply the request r of c at one instant, and record its result in r.
func (s *Store) apply(ctx context.Context, c *Client, r *request) {
	breakAt := -1
	var result string
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		o, ok := s.objects[r.key]
		if r.kind == get {
			if !ok {
				r.err = fmt.Errorf("simstore: get %v: %w", r.key, store.ErrNotFound)
				result = r.err.Error()
				return
			}
			r.content, r.version = o.content, o.version
			result = fmt.Sprintf("read %v bytes at version %v", len(o.content), short(o.version))
			return
		}

		// S3 checks a checksum as the content arrives. Whether it checks the checksum or the condition
		// first is not documented, and either way the write is not applied.
		switch {
		case r.checksum != nil && sha256.Sum256(r.content) != *r.checksum:
			r.err = fmt.Errorf("simstore: %v: %w", r, store.ErrChecksum)
		case r.kind == create && ok:
			r.err = fmt.Errorf("simstore: %v: %w", r, store.ErrExists)
		case r.kind == swap && !ok:
			r.err = fmt.Errorf("simstore: %v: %w", r, store.ErrNotFound)
		case r.kind == swap && o.version != r.from:
			r.err = fmt.Errorf("simstore: %v: the version is %v: %w", r, short(o.version), store.ErrChanged)
		}
		if r.err != nil {
			result = "not applied: " + errors.Unwrap(r.err).Error()
			return
		}

		r.version = s.version(r.content)
		s.objects[r.key] = object{content: r.content, version: r.version}
		s.history = append(s.history, Write{
			Time:    s.host.Monotonic(),
			Client:  c.name,
			Request: r.id,
			Key:     r.key,
			Create:  r.kind == create,
			From:    r.from,
			Version: r.version,
			Content: r.content,
		})
		result = fmt.Sprintf("applied as write %v, at version %v", len(s.history), short(r.version))
	}()

	if r.kind == get && r.err == nil && s.chance(s.options.Faults.BrokenRead) {
		breakAt = int(s.host.Uint64() % uint64(len(r.content)+1))
		result += fmt.Sprintf(", whose content breaks off after %v bytes", breakAt)
	}
	s.mu.Lock()
	r.breakAt = breakAt
	s.mu.Unlock()
	s.host.Trace(ctx, "store: request %v of %v: %v", r.id, c.name, result)
}

// body is the content of a get.
type body struct {
	ctx     context.Context
	content []byte
	// breakAt is where the content breaks off, or -1.
	breakAt int
	request int
	off     int
	closed  bool
}

func (b *body) Read(p []byte) (int, error) {
	if b.closed {
		return 0, errors.New("simstore: read of content that is closed")
	}
	if err := context.Cause(b.ctx); err != nil {
		return 0, fmt.Errorf("simstore: read of the content of request %v: %w", b.request, err)
	}
	end := len(b.content)
	if b.breakAt >= 0 {
		end = b.breakAt
	}
	if b.off >= end {
		if b.breakAt >= 0 {
			return 0, errorf("the content of request %v broke off after %v of %v bytes", b.request, b.breakAt, len(b.content))
		}
		return 0, io.EOF
	}
	n := copy(p, b.content[b.off:end])
	b.off += n
	return n, nil
}

func (b *body) Close() error {
	b.closed = true
	return nil
}
