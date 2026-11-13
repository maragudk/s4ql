package s3store_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"maragu.dev/is"

	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/store"
	"maragu.dev/s4ql/internal/store/s3store"
	"maragu.dev/s4ql/internal/store/storetest"
)

// refusals are the four errors of a write that mean that it was not applied.
var refusals = []error{store.ErrNotFound, store.ErrExists, store.ErrChanged, store.ErrChecksum}

// isUnknown reports whether err is the error of a write whose outcome is unknown.
func isUnknown(err error) bool {
	if err == nil {
		return false
	}
	for _, r := range refusals {
		if errors.Is(err, r) {
			return false
		}
	}
	return true
}

func sum(data string) *store.Checksum {
	return new(sha256.Sum256([]byte(data)))
}

func TestClient_answers(t *testing.T) {
	type answer struct {
		status int
		code   string
	}
	tests := []struct {
		name   string
		answer answer
		create error
		swap   error
	}{
		{name: "412 PreconditionFailed", answer: answer{412, "PreconditionFailed"}, create: store.ErrExists, swap: store.ErrChanged},
		{name: "409 ConditionalRequestConflict", answer: answer{409, "ConditionalRequestConflict"}, swap: store.ErrChanged},
		{name: "404 NoSuchKey", answer: answer{404, "NoSuchKey"}, swap: store.ErrNotFound},
		{name: "400 BadDigest", answer: answer{400, "BadDigest"}, create: store.ErrChecksum, swap: store.ErrChecksum},
		{name: "400 XAmzContentSHA256Mismatch", answer: answer{400, "XAmzContentSHA256Mismatch"}, create: store.ErrChecksum, swap: store.ErrChecksum},
		{name: "412 with another code", answer: answer{412, "SomethingElse"}},
		{name: "409 OperationAborted", answer: answer{409, "OperationAborted"}},
		{name: "404 NoSuchBucket", answer: answer{404, "NoSuchBucket"}},
		{name: "400 InvalidArgument", answer: answer{400, "InvalidArgument"}},
		{name: "403 AccessDenied", answer: answer{403, "AccessDenied"}},
		{name: "500 InternalError", answer: answer{500, "InternalError"}},
		{name: "503 SlowDown", answer: answer{503, "SlowDown"}},
		{name: "429 TooManyRequests", answer: answer{429, "TooManyRequests"}},
		{name: "307 TemporaryRedirect", answer: answer{307, "TemporaryRedirect"}},
	}

	for _, test := range tests {
		for _, op := range []string{"create", "swap"} {
			want := test.create
			if op == "swap" {
				want = test.swap
			}
			name := fmt.Sprintf("should report %v on a %v as an unknown outcome", test.name, op)
			if want != nil {
				name = fmt.Sprintf("should report %v on a %v as %v", test.name, op, want)
			}
			t.Run(name, func(t *testing.T) {
				f := newFake(t)
				f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
					if r.Method != http.MethodPut {
						return false
					}
					if test.answer.status == 307 {
						w.Header().Set("Location", f.srv.URL+"/bucket/elsewhere")
					}
					fail(w, test.answer.status, test.answer.code)
					return true
				}
				c := f.client(t)
				var err error
				if op == "create" {
					_, err = c.Create(t.Context(), "k", strings.NewReader("data"), sum("data"))
				} else {
					_, err = c.Swap(t.Context(), "k", `"v"`, strings.NewReader("data"), sum("data"))
				}
				if want != nil {
					is.True(t, errors.Is(err, want), fmt.Sprint(err))
				} else {
					is.True(t, isUnknown(err), fmt.Sprint(err))
				}
				is.Equal(t, 1, f.count(http.MethodPut, "k"))
			})
		}
	}

	t.Run("should not follow a redirect of a write, also of empty content", func(t *testing.T) {
		f := newFake(t)
		f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
			if key(r) != "k" {
				return false
			}
			w.Header().Set("Location", f.srv.URL+"/bucket/elsewhere")
			fail(w, http.StatusTemporaryRedirect, "TemporaryRedirect")
			return true
		}
		_, err := f.client(t).Create(t.Context(), "k", strings.NewReader(""), nil)
		is.True(t, isUnknown(err), fmt.Sprint(err))
		is.Equal(t, 1, f.count(http.MethodPut, "k"))
		is.Equal(t, 0, f.count(http.MethodPut, "elsewhere"))
	})

	t.Run("should report a success without an ETag as an unknown outcome", func(t *testing.T) {
		f := newFake(t)
		f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
			return true
		}
		_, err := f.client(t).Create(t.Context(), "k", strings.NewReader("data"), nil)
		is.True(t, isUnknown(err), fmt.Sprint(err))
	})

	t.Run("should send a write once also when the environment asks the SDK for retries", func(t *testing.T) {
		t.Setenv("AWS_MAX_ATTEMPTS", "5")
		t.Setenv("AWS_RETRY_MODE", "adaptive")
		f := newFake(t)
		f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
			fail(w, http.StatusServiceUnavailable, "SlowDown")
			return true
		}
		c := f.client(t)
		_, err := c.Create(t.Context(), "k", strings.NewReader("data"), nil)
		is.True(t, isUnknown(err), fmt.Sprint(err))
		is.Equal(t, 1, f.count(http.MethodPut, "k"))
		_, _, err = c.Get(t.Context(), "k")
		is.True(t, err != nil)
		is.Equal(t, 1, f.count(http.MethodGet, "k"))
	})

	t.Run("should fail a read where no object is with ErrNotFound, and other answers with other errors", func(t *testing.T) {
		f := newFake(t)
		f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
			switch key(r) {
			case "no-bucket":
				fail(w, http.StatusNotFound, "NoSuchBucket")
			case "denied":
				fail(w, http.StatusForbidden, "AccessDenied")
			case "broken":
				fail(w, http.StatusInternalServerError, "InternalError")
			case "no-etag":
				w.Header().Set("Content-Length", "4")
				_, _ = w.Write([]byte("data"))
			default:
				return false
			}
			return true
		}
		c := f.client(t)
		_, _, err := c.Get(t.Context(), "missing")
		is.True(t, errors.Is(err, store.ErrNotFound), fmt.Sprint(err))
		for _, k := range []string{"no-bucket", "denied", "broken", "no-etag"} {
			_, _, err := c.Get(t.Context(), k)
			is.True(t, err != nil && !errors.Is(err, store.ErrNotFound), k, fmt.Sprint(err))
		}
	})
}

func TestClient_connections(t *testing.T) {
	t.Run("should report a create as an unknown outcome when the connection is cut after the store applied it, and send it once", func(t *testing.T) {
		f := newFake(t)
		c := f.client(t)
		// A read first, so that the write goes out on a connection that is reused, where net/http
		// would send a request again if it could.
		_, _, _ = c.Get(t.Context(), "warm")
		f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
			if r.Method != http.MethodPut {
				return false
			}
			data, _ := io.ReadAll(r.Body)
			f.put(key(r), data)
			cut(w)
			return true
		}
		_, err := c.Create(t.Context(), "k", strings.NewReader("data"), sum("data"))
		is.True(t, isUnknown(err), fmt.Sprint(err))
		is.Equal(t, 1, f.count(http.MethodPut, "k"))
		_, ok := f.object("k")
		is.True(t, ok)
	})

	t.Run("should report a swap as an unknown outcome when the connection is cut after the store applied it, and send it once", func(t *testing.T) {
		f := newFake(t)
		c := f.client(t)
		v := store.Version(f.put("k", []byte("one")))
		_, _, _ = c.Get(t.Context(), "warm")
		f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
			if r.Method != http.MethodPut {
				return false
			}
			data, _ := io.ReadAll(r.Body)
			f.put(key(r), data)
			cut(w)
			return true
		}
		_, err := c.Swap(t.Context(), "k", v, strings.NewReader("two"), sum("two"))
		is.True(t, isUnknown(err), fmt.Sprint(err))
		is.Equal(t, 1, f.count(http.MethodPut, "k"))
		o, _ := f.object("k")
		is.Equal(t, "two", string(o.data))
	})

	t.Run("should report a write as an unknown outcome when the connection is cut before the store read it", func(t *testing.T) {
		f := newFake(t)
		c := f.client(t)
		_, _, _ = c.Get(t.Context(), "warm")
		f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
			if r.Method != http.MethodPut {
				return false
			}
			cut(w)
			return true
		}
		_, err := c.Create(t.Context(), "k", strings.NewReader("data"), sum("data"))
		is.True(t, isUnknown(err), fmt.Sprint(err))
		is.Equal(t, 1, f.count(http.MethodPut, "k"))
		_, ok := f.object("k")
		is.True(t, !ok)
	})

	t.Run("should let net/http send a read again on a reused connection, which shows that the writes above would see it", func(t *testing.T) {
		f := newFake(t)
		c := f.client(t)
		f.put("k", []byte("data"))
		_, _, _ = c.Get(t.Context(), "warm")
		var cuts atomic.Int32
		f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
			if key(r) == "k" && cuts.Add(1) == 1 {
				cut(w)
				return true
			}
			return false
		}
		body, _, err := c.Get(t.Context(), "k")
		is.NotError(t, err)
		data, err := io.ReadAll(body)
		is.NotError(t, err)
		is.NotError(t, body.Close())
		is.Equal(t, "data", string(data))
		is.Equal(t, 2, f.count(http.MethodGet, "k"))
	})

	t.Run("should fail a read whose content ends before its length", func(t *testing.T) {
		f := newFake(t)
		f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
			w.Header().Set("ETag", `"v"`)
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte("only half of it"))
			_ = http.NewResponseController(w).Flush()
			cut(w)
			return true
		}
		body, _, err := f.client(t).Get(t.Context(), "k")
		is.NotError(t, err)
		_, err = io.ReadAll(body)
		_ = body.Close()
		is.True(t, errors.Is(err, io.ErrUnexpectedEOF), fmt.Sprint(err))
	})

	t.Run("should fail a read whose content does not match the checksum that the store holds", func(t *testing.T) {
		f := newFake(t)
		f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
			w.Header().Set("ETag", `"v"`)
			w.Header().Set("Content-Length", "4")
			w.Header().Set("x-amz-checksum-sha256", base64.StdEncoding.EncodeToString(sum("other")[:]))
			w.Header().Set("x-amz-checksum-type", "FULL_OBJECT")
			_, _ = w.Write([]byte("data"))
			return true
		}
		body, _, err := f.client(t).Get(t.Context(), "k")
		is.NotError(t, err)
		_, err = io.ReadAll(body)
		_ = body.Close()
		is.True(t, err != nil, "a read of content that does not match its checksum succeeded")
	})

	t.Run("should end a write and a read whose answer is slow when the context ends", func(t *testing.T) {
		f := newFake(t)
		release := make(chan struct{})
		defer close(release)
		f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
			if key(r) == "slow-body" {
				w.Header().Set("ETag", `"v"`)
				w.Header().Set("Content-Length", "100")
				_, _ = w.Write([]byte("part"))
				_ = http.NewResponseController(w).Flush()
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return true
		}
		c := f.client(t)

		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		_, err := c.Create(ctx, "k", strings.NewReader("data"), sum("data"))
		is.True(t, isUnknown(err) && errors.Is(err, context.DeadlineExceeded), fmt.Sprint(err))

		ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		_, _, err = c.Get(ctx, "k")
		is.True(t, errors.Is(err, context.DeadlineExceeded), fmt.Sprint(err))

		ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		body, _, err := c.Get(ctx, "slow-body")
		is.NotError(t, err)
		_, err = io.ReadAll(body)
		_ = body.Close()
		is.True(t, errors.Is(err, context.DeadlineExceeded), fmt.Sprint(err))
	})
}

// watched content of a write, which records a read or seek that starts after the test marks the
// write as returned, and how many reads are in progress.
type watched struct {
	r        io.ReadSeeker
	returned atomic.Bool
	late     atomic.Int32
	reading  atomic.Int32
	delay    time.Duration
}

func (w *watched) Read(p []byte) (int, error) {
	w.reading.Add(1)
	defer w.reading.Add(-1)
	if w.returned.Load() {
		w.late.Add(1)
	}
	time.Sleep(w.delay)
	return w.r.Read(p)
}

func (w *watched) Seek(offset int64, whence int) (int64, error) {
	if w.returned.Load() {
		w.late.Add(1)
	}
	return w.r.Seek(offset, whence)
}

func TestClient_content(t *testing.T) {
	t.Run("should not read the content after a write has returned, also when the store answered before it read the content", func(t *testing.T) {
		f := newFake(t)
		held := make(chan struct{})
		t.Cleanup(func() { close(held) })
		f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
			// The answer comes before the body is read, and the connection stays open without
			// reading it, so that the transport goes on sending the body after the answer.
			conn, buf, err := http.NewResponseController(w).Hijack()
			if err != nil {
				panic(err)
			}
			const xml = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>PreconditionFailed</Code></Error>`
			_, _ = fmt.Fprintf(buf, "HTTP/1.1 412 Precondition Failed\r\nContent-Type: application/xml\r\nContent-Length: %v\r\n\r\n%v", len(xml), xml)
			_ = buf.Flush()
			<-held
			_ = conn.Close()
			return true
		}
		c := f.client(t)
		// Below the size at which the SDK asks for 100-continue, so that the body is sent at once,
		// and slow to read, so that the answer comes while it is being sent.
		content := &watched{r: bytes.NewReader(make([]byte, 2<<20-1)), delay: time.Millisecond}
		_, err := c.Create(t.Context(), "k", content, nil)
		reading := content.reading.Load()
		content.returned.Store(true)
		is.True(t, errors.Is(err, store.ErrExists), fmt.Sprint(err))
		is.Equal(t, int32(0), reading, "a read of the content was in progress when the write returned")
		time.Sleep(200 * time.Millisecond)
		is.Equal(t, int32(0), content.late.Load(), "the content was read after the write returned")
	})

	t.Run("should refuse content larger than one PutObject takes without sending anything", func(t *testing.T) {
		f := newFake(t)
		_, err := f.client(t).Create(t.Context(), "k", &sized{size: s3store.MaxSize + 1}, nil)
		is.True(t, errors.Is(err, s3store.ErrTooLarge), fmt.Sprint(err))
		is.Equal(t, 0, f.count(http.MethodPut, "k"))
	})

	t.Run("should write and read a large object with a small heap", func(t *testing.T) {
		const size = 256 << 20
		f := newFake(t)
		var got atomic.Int64
		f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
			switch r.Method {
			case http.MethodPut:
				n, _ := io.Copy(io.Discard, r.Body)
				got.Store(n)
				w.Header().Set("ETag", `"v"`)
			case http.MethodGet:
				w.Header().Set("ETag", `"v"`)
				w.Header().Set("Content-Length", fmt.Sprint(size))
				_, _ = io.Copy(w, &sized{size: size})
			}
			return true
		}
		c := f.client(t)

		before := allocated()
		_, err := c.Create(t.Context(), "k", &sized{size: size}, nil)
		is.NotError(t, err)
		is.Equal(t, int64(size), got.Load())
		body, _, err := c.Get(t.Context(), "k")
		is.NotError(t, err)
		n, err := io.Copy(io.Discard, body)
		is.NotError(t, err)
		is.NotError(t, body.Close())
		is.Equal(t, int64(size), n)
		// Twice 256 MiB went through, and less than a tenth of one of them was ever allocated.
		used := allocated() - before
		is.True(t, used < size/10, fmt.Sprint(used>>20, " MiB allocated"))
	})
}

// allocated is the number of bytes allocated on the heap so far.
func allocated() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

// sized content of the given size, made up as it is read, so that it is never in memory.
type sized struct {
	size int64
	at   int64
}

func (s *sized) Read(p []byte) (int, error) {
	if s.at >= s.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), s.size-s.at))
	for i := range n {
		p[i] = byte(s.at + int64(i))
	}
	s.at += int64(n)
	return n, nil
}

func (s *sized) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.at = offset
	case io.SeekCurrent:
		s.at += offset
	case io.SeekEnd:
		s.at = s.size + offset
	}
	return s.at, nil
}

func TestClient_requests(t *testing.T) {
	t.Run("should send no swap from an empty version, and no request for a key that the store does not take", func(t *testing.T) {
		f := newFake(t)
		c := f.client(t)
		_, err := c.Swap(t.Context(), "k", "", strings.NewReader("data"), nil)
		is.True(t, err != nil)
		for _, k := range []string{"", strings.Repeat("k", 1025), "a/../b", "./a", "a/.", "\xff"} {
			_, err := c.Create(t.Context(), k, strings.NewReader("data"), nil)
			is.True(t, err != nil, k)
			_, _, err = c.Get(t.Context(), k)
			is.True(t, err != nil, k)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		is.Equal(t, 0, len(f.requests), fmt.Sprint(f.requests))
	})

	t.Run("should send the checksum as the SHA-256 of the signed payload, and with the object", func(t *testing.T) {
		f := newFake(t)
		var payload, header string
		f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
			payload = r.Header.Get("x-amz-content-sha256")
			header = r.Header.Get("x-amz-checksum-sha256")
			return false
		}
		_, err := f.client(t).Create(t.Context(), "k", strings.NewReader("data"), sum("data"))
		is.NotError(t, err)
		is.Equal(t, fmt.Sprintf("%x", *sum("data")), payload)
		is.Equal(t, base64.StdEncoding.EncodeToString(sum("data")[:]), header)
		is.True(t, strings.Contains(payload, fmt.Sprintf("%x", *sum("data"))))
	})

	t.Run("should write the content from its current offset", func(t *testing.T) {
		f := newFake(t)
		r := strings.NewReader("skip:keep")
		_, err := r.Seek(5, io.SeekStart)
		is.NotError(t, err)
		_, err = f.client(t).Create(t.Context(), "k", r, sum("keep"))
		is.NotError(t, err)
		o, _ := f.object("k")
		is.Equal(t, "keep", string(o.data))
	})

	t.Run("should refuse a write whose checksum does not match, also of empty content", func(t *testing.T) {
		f := newFake(t)
		c := f.client(t)
		for _, data := range []string{"", "data"} {
			_, err := c.Create(t.Context(), "k", strings.NewReader(data), sum("other"))
			is.True(t, errors.Is(err, store.ErrChecksum), fmt.Sprint(err))
		}
		_, ok := f.object("k")
		is.True(t, !ok)
	})

	t.Run("should need a bucket and a region", func(t *testing.T) {
		_, err := s3store.New(t.Context(), s3store.Options{})
		is.True(t, err != nil)
		t.Setenv("AWS_REGION", "")
		t.Setenv("AWS_DEFAULT_REGION", "")
		t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
		t.Setenv("AWS_PROFILE", "")
		_, err = s3store.New(t.Context(), s3store.Options{Bucket: "b"})
		is.True(t, err != nil)
	})
}

// events that a test's logger received.
type events struct {
	mu      sync.Mutex
	records []slog.Record
}

func (e *events) Enabled(context.Context, slog.Level) bool { return true }
func (e *events) Handle(_ context.Context, r slog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = append(e.records, r)
	return nil
}
func (e *events) WithAttrs([]slog.Attr) slog.Handler { return e }
func (e *events) WithGroup(string) slog.Handler      { return e }

// last event, by its level and attributes.
func (e *events) last() (slog.Level, map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.records[len(e.records)-1]
	attrs := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	return r.Level, attrs
}

func (e *events) len() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.records)
}

func TestClient_events(t *testing.T) {
	t.Run("should log one event for each request, at Debug for normal work and at Warn for an unknown outcome", func(t *testing.T) {
		f := newFake(t)
		e := &events{}
		c := f.client(t, func(o *s3store.Options) { o.Logger = slog.New(e) })

		v, err := c.Create(t.Context(), "k", strings.NewReader("data"), sum("data"))
		is.NotError(t, err)
		level, attrs := e.last()
		is.Equal(t, slog.LevelDebug, level)
		is.Equal(t, "create", attrs["operation"])
		is.Equal(t, "k", attrs["store.key"])
		is.Equal(t, "ok", attrs["outcome"])
		is.Equal(t, "4", attrs["store.bytes"])
		is.Equal(t, "200", attrs["http.status_code"])
		is.Equal(t, "fake-request", attrs["store.request_id"])
		is.Equal(t, "1", attrs["attempts"])
		is.True(t, attrs["duration.total"] != "")

		_, err = c.Create(t.Context(), "k", strings.NewReader("data"), nil)
		is.True(t, errors.Is(err, store.ErrExists))
		level, attrs = e.last()
		is.Equal(t, slog.LevelDebug, level)
		is.Equal(t, "exists", attrs["outcome"])
		is.Equal(t, "412", attrs["http.status_code"])

		body, got, err := c.Get(t.Context(), "k")
		is.NotError(t, err)
		is.Equal(t, v, got)
		n := e.len()
		_, _ = io.ReadAll(body)
		is.Equal(t, n, e.len(), "the event of a read came before its content was closed")
		_ = body.Close()
		_ = body.Close()
		is.Equal(t, n+1, e.len())
		level, attrs = e.last()
		is.Equal(t, slog.LevelDebug, level)
		is.Equal(t, "get", attrs["operation"])
		is.Equal(t, "ok", attrs["outcome"])
		is.Equal(t, "4", attrs["store.bytes"])

		f.hook = func(w http.ResponseWriter, r *http.Request, f *fake) bool {
			fail(w, http.StatusInternalServerError, "InternalError")
			return true
		}
		_, err = c.Swap(t.Context(), "k", v, strings.NewReader("more"), nil)
		is.True(t, isUnknown(err))
		level, attrs = e.last()
		is.Equal(t, slog.LevelWarn, level)
		is.Equal(t, "unknown", attrs["outcome"])
		is.Equal(t, "500", attrs["http.status_code"])
		is.True(t, attrs["error"] != "")
	})
}

// realBucket gives the options of a real bucket from the environment, or skips the test.
func realBucket(t *testing.T) s3store.Options {
	t.Helper()
	bucket := os.Getenv("S4QL_S3_BUCKET")
	if bucket == "" {
		t.Skip("set S4QL_S3_BUCKET to a bucket, and AWS_REGION, AWS_ENDPOINT_URL_S3, and credentials for the SDK, to run the integration tests against a real store; see internal/store/s3store/README.md")
	}
	return s3store.Options{
		Bucket:    bucket,
		Endpoint:  os.Getenv("AWS_ENDPOINT_URL_S3"),
		Region:    os.Getenv("AWS_REGION"),
		PathStyle: os.Getenv("S4QL_S3_PATH_STYLE") == "true",
	}
}

// realClient of the bucket of opts, and a plain client of the SDK for what the store's interface
// cannot do, such as delete.
func realClient(t *testing.T, opts s3store.Options) (*s3store.Client, *s3.Client) {
	t.Helper()
	c, err := s3store.New(t.Context(), opts)
	is.NotError(t, err)
	cfg, err := config.LoadDefaultConfig(t.Context(), config.WithRegion(opts.Region))
	is.NotError(t, err)
	raw := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		}
		o.UsePathStyle = opts.PathStyle
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	return c, raw
}

// newPrefix under which a test writes, and which is deleted with everything under it when the
// test ends.
func newPrefix(t *testing.T, raw *s3.Client, bucket string) string {
	t.Helper()
	prefix := fmt.Sprintf("s4ql-test/s3store/%v-%v/", time.Now().UTC().Format("20060102T150405"), rand.Text()[:8])
	t.Cleanup(func() {
		// The context of the test is done when its cleanup runs.
		ctx := context.Background()
		pages := s3.NewListObjectsV2Paginator(raw, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
		for pages.HasMorePages() {
			page, err := pages.NextPage(ctx)
			if err != nil {
				t.Errorf("list %v to delete it: %v", prefix, err)
				return
			}
			for _, o := range page.Contents {
				if _, err := raw.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: o.Key}); err != nil {
					t.Errorf("delete %v: %v", aws.ToString(o.Key), err)
				}
			}
		}
	})
	return prefix
}

// prefixed store, which puts a prefix in front of every key.
type prefixed struct {
	c      *s3store.Client
	prefix string
}

func (p prefixed) Get(ctx context.Context, key string) (io.ReadCloser, store.Version, error) {
	return p.c.Get(ctx, p.prefix+key)
}

func (p prefixed) Create(ctx context.Context, key string, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	return p.c.Create(ctx, p.prefix+key, content, checksum)
}

func (p prefixed) Swap(ctx context.Context, key string, from store.Version, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	return p.c.Swap(ctx, p.prefix+key, from, content, checksum)
}

func TestClient_conformance(t *testing.T) {
	opts := realBucket(t)
	c, raw := realClient(t, opts)
	storetest.Test(t, func(t *testing.T, f func(t *testing.T, ctx context.Context, env storetest.Env)) {
		prefix := newPrefix(t, raw, opts.Bucket)
		f(t, t.Context(), storetest.Env{
			Host:   host.New(),
			Client: func(string) store.Store { return prefixed{c: c, prefix: prefix} },
		})
	})
}

func TestClient_realBucket(t *testing.T) {
	opts := realBucket(t)

	t.Run("should store the checksum with the object", func(t *testing.T) {
		c, raw := realClient(t, opts)
		k := newPrefix(t, raw, opts.Bucket) + "k"
		_, err := c.Create(t.Context(), k, strings.NewReader("data"), sum("data"))
		is.NotError(t, err)
		out, err := raw.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(opts.Bucket), Key: aws.String(k), ChecksumMode: types.ChecksumModeEnabled})
		is.NotError(t, err)
		_ = out.Body.Close()
		is.Equal(t, base64.StdEncoding.EncodeToString(sum("data")[:]), aws.ToString(out.ChecksumSHA256))
	})

	t.Run("should refuse a wrong checksum of empty content and of a mebibyte, and store nothing", func(t *testing.T) {
		c, raw := realClient(t, opts)
		prefix := newPrefix(t, raw, opts.Bucket)
		for _, data := range []string{"", strings.Repeat("x", 1<<20)} {
			k := fmt.Sprint(prefix, len(data))
			_, err := c.Create(t.Context(), k, strings.NewReader(data), sum("other"))
			is.True(t, errors.Is(err, store.ErrChecksum), fmt.Sprint(err))
			_, _, err = c.Get(t.Context(), k)
			is.True(t, errors.Is(err, store.ErrNotFound), fmt.Sprint(err))
		}
	})

	t.Run("should fail a read of an object whose stored checksum does not match its content", func(t *testing.T) {
		c, raw := realClient(t, opts)
		k := newPrefix(t, raw, opts.Bucket) + "k"
		// Written past the client, with a checksum header that does not match the content, which
		// Tigris stores without checking it.
		_, err := raw.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket:         aws.String(opts.Bucket),
			Key:            aws.String(k),
			Body:           strings.NewReader("data"),
			ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(sum("other")[:])),
		})
		if err != nil {
			t.Skipf("the store refused a checksum header that does not match the content, as AWS documents: %v", err)
		}
		body, _, err := c.Get(t.Context(), k)
		is.NotError(t, err)
		_, err = io.ReadAll(body)
		_ = body.Close()
		is.True(t, err != nil, "a read of content that does not match its stored checksum succeeded")
	})

	t.Run("should keep apart keys with characters that have a meaning in a URL or a path", func(t *testing.T) {
		c, raw := realClient(t, opts)
		prefix := newPrefix(t, raw, opts.Bucket)
		keys := []string{
			"q?x", "h#x", "pct%20x", "pct x", "plus+x", "amp&x", "eq=x", "semi;x", "tilde~x", "star*x",
			"quote'x", `dquote"x`, `back\x`, "a//b", "a/b", "/lead", "trail/", "trail", "tab\tx", "nl\nx",
			"emoji😀", "nfc-é", "nfd-é", ".hidden", "...", "a/.../b",
		}
		versions := map[string]store.Version{}
		for _, k := range keys {
			v, err := c.Create(t.Context(), prefix+k, strings.NewReader("content of "+k), nil)
			if err != nil {
				t.Errorf("create %q: %v", k, err)
				continue
			}
			versions[k] = v
		}
		for _, k := range keys {
			body, v, err := c.Get(t.Context(), prefix+k)
			if err != nil {
				t.Errorf("get %q: %v", k, err)
				continue
			}
			data, err := io.ReadAll(body)
			_ = body.Close()
			if err != nil || string(data) != "content of "+k || v != versions[k] {
				t.Errorf("get %q: %q, %q, %v", k, data, v, err)
			}
		}
	})

	t.Run("should take keys of 1024 bytes", func(t *testing.T) {
		c, raw := realClient(t, opts)
		prefix := newPrefix(t, raw, opts.Bucket)
		ascii := prefix + strings.Repeat("k", 1024-len(prefix))
		utf := prefix + strings.Repeat("æ", (1024-len(prefix))/2)
		for _, k := range []string{ascii, utf} {
			_, err := c.Create(t.Context(), k, strings.NewReader("data"), nil)
			is.NotError(t, err, fmt.Sprint(len(k)))
			body, _, err := c.Get(t.Context(), k)
			is.NotError(t, err)
			_ = body.Close()
		}
	})
}
