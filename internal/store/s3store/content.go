package s3store

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// errReturned is the error of a read of the content of a write after the write has returned.
var errReturned = errors.New("s3store: the content of a write is read after the write has returned")

// guard of the content of a write. The HTTP transport may still read a request's body after the
// round trip has returned, from a goroutine of its own, as [http.RoundTripper] allows. Once the
// guard is closed, it lets no read or seek through to the content, and a read or seek that has
// started is over when close returns.
type guard struct {
	mu     sync.Mutex
	r      io.ReadSeeker
	closed bool
}

func (g *guard) Read(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return 0, errReturned
	}
	return g.r.Read(p)
}

func (g *guard) Seek(offset int64, whence int) (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return 0, errReturned
	}
	return g.r.Seek(offset, whence)
}

// close the guard for good.
func (g *guard) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
}

// body of an object that a read returned. It fails with [io.ErrUnexpectedEOF] if the content ends
// before the length that the answer gave, and logs the event of the read when it is closed.
type body struct {
	r    *request
	body io.ReadCloser
	size int64
	// read is the number of bytes read so far, and err the first error of a read, or io.EOF.
	read   int64
	err    error
	closed bool
}

func (b *body) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	n, err := b.body.Read(p)
	b.read += int64(n)
	switch {
	case b.read > b.size:
		err = fmt.Errorf("s3store: get %q: the content is longer than the %v bytes of the answer", b.r.key, b.size)
	case errors.Is(err, io.EOF) && b.read < b.size:
		err = fmt.Errorf("s3store: get %q: the content ended after %v of %v bytes: %w", b.r.key, b.read, b.size, io.ErrUnexpectedEOF)
	case err != nil && !errors.Is(err, io.EOF):
		err = fmt.Errorf("s3store: get %q: %w", b.r.key, err)
	}
	if err != nil {
		b.err = err
	}
	return n, err
}

// Close the body, and log the event of the read once.
func (b *body) Close() error {
	err := b.body.Close()
	if b.closed {
		return err
	}
	b.closed = true
	b.r.bytes = b.read
	switch {
	case errors.Is(b.err, io.EOF):
		_ = b.r.end(outcomeOK, nil)
	case b.err != nil:
		_ = b.r.end(outcomeFailed, b.err)
	default:
		_ = b.r.end(outcomeIncomplete, nil)
	}
	return err
}
