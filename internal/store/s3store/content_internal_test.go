package s3store

import (
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"maragu.dev/is"
)

// slowReader takes a while for each read, and counts the reads in progress.
type slowReader struct {
	io.ReadSeeker
	reading atomic.Int32
}

func (s *slowReader) Read(p []byte) (int, error) {
	s.reading.Add(1)
	defer s.reading.Add(-1)
	time.Sleep(50 * time.Millisecond)
	return s.ReadSeeker.Read(p)
}

func TestGuard(t *testing.T) {
	t.Run("should pass reads and seeks through until it is closed, and none after", func(t *testing.T) {
		g := &guard{r: strings.NewReader("data")}
		_, err := g.Seek(1, io.SeekStart)
		is.NotError(t, err)
		data, err := io.ReadAll(g)
		is.NotError(t, err)
		is.Equal(t, "ata", string(data))

		g.close()
		_, err = g.Seek(0, io.SeekStart)
		is.True(t, errors.Is(err, errReturned))
		_, err = g.Read(make([]byte, 1))
		is.True(t, errors.Is(err, errReturned))
	})

	t.Run("should wait in close for a read that is in progress", func(t *testing.T) {
		r := &slowReader{ReadSeeker: strings.NewReader("data")}
		g := &guard{r: r}
		started := make(chan struct{})
		go func() {
			close(started)
			_, _ = g.Read(make([]byte, 4))
		}()
		<-started
		for r.reading.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		g.close()
		is.Equal(t, int32(0), r.reading.Load())
	})
}

func TestBody(t *testing.T) {
	c := &Client{log: discard()}

	t.Run("should fail content that ends before the length of the answer", func(t *testing.T) {
		b := &body{r: &request{c: c, key: "k"}, body: io.NopCloser(strings.NewReader("short")), size: 10}
		_, err := io.ReadAll(b)
		is.True(t, errors.Is(err, io.ErrUnexpectedEOF), err.Error())
		is.NotError(t, b.Close())
	})

	t.Run("should fail content that is longer than the length of the answer", func(t *testing.T) {
		b := &body{r: &request{c: c, key: "k"}, body: io.NopCloser(strings.NewReader("longer than five")), size: 5}
		_, err := io.ReadAll(b)
		is.True(t, err != nil)
		is.NotError(t, b.Close())
	})

	t.Run("should end with io.EOF after content of the length of the answer", func(t *testing.T) {
		b := &body{r: &request{c: c, key: "k"}, body: io.NopCloser(strings.NewReader("exact")), size: 5}
		data, err := io.ReadAll(b)
		is.NotError(t, err)
		is.Equal(t, "exact", string(data))
		_, err = b.Read(make([]byte, 1))
		is.True(t, errors.Is(err, io.EOF))
	})
}
