package s3store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"maragu.dev/is"

	"maragu.dev/s4ql/internal/store"
)

func discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestNew(t *testing.T) {
	t.Run("should make a transport that goes through no proxy, also one that the environment names", func(t *testing.T) {
		t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
		t.Setenv("AWS_ACCESS_KEY_ID", "test")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
		c, err := New(t.Context(), Options{Bucket: "b", Region: "r"})
		is.NotError(t, err)
		hc, ok := c.s3.Options().HTTPClient.(*http.Client)
		is.True(t, ok)
		transport, ok := hc.Transport.(*http.Transport)
		is.True(t, ok)
		is.True(t, transport.Proxy == nil)
		is.True(t, transport == c.transport)
	})
}

func TestRequest_refusal(t *testing.T) {
	t.Run("should take a refusal for one only if the request went out once", func(t *testing.T) {
		c := &Client{log: discard()}
		for _, op := range []string{opCreate, opSwap} {
			r := &request{c: c, op: op, status: http.StatusPreconditionFailed, code: "PreconditionFailed"}
			r.attempts.Store(1)
			_, err := r.refusal()
			is.True(t, errors.Is(err, store.ErrExists) || errors.Is(err, store.ErrChanged))

			// A second attempt can be refused because the first was applied.
			r.attempts.Store(2)
			_, err = r.refusal()
			is.True(t, err == nil, op)

			// No count means that the count did not reach the transport, and nothing is known.
			r.attempts.Store(0)
			_, err = r.refusal()
			is.True(t, err == nil, op)
		}
	})
}

// serverClient is a client of [New] of an HTTP server with the handler h, in a test that is isolated
// from the machine's AWS configuration, and the dialer of its transport, which keeps the connections.
func serverClient(t *testing.T, h http.HandlerFunc) (*Client, *dialer) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	for _, name := range []string{"AWS_PROFILE", "AWS_CA_BUNDLE", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_S3", "AWS_DEFAULTS_MODE"} {
		t.Setenv(name, "")
	}
	c, err := New(t.Context(), Options{
		Bucket: "bucket", Endpoint: srv.URL, Region: "us-east-1", PathStyle: true,
		AccessKeyID: "test", SecretAccessKey: "test",
	})
	is.NotError(t, err)
	d := &dialer{}
	c.transport.DialContext = d.dial
	return c, d
}

// dialer of a transport in a test, which keeps every connection that it dials.
type dialer struct {
	mu    sync.Mutex
	conns []*conn
	// hold, if it is not nil, is waited for by a dial, after the dial has been counted in dialing.
	hold    chan struct{}
	dialing sync.WaitGroup
	// dialed, if it is not nil, gets each connection when its dial has ended.
	dialed chan *conn
}

func (d *dialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	hold := d.hold
	d.mu.Unlock()
	if hold != nil {
		d.dialing.Done()
		<-hold
	}
	var nd net.Dialer
	nc, err := nd.DialContext(context.WithoutCancel(ctx), network, address)
	if err != nil {
		return nil, err
	}
	cn := &conn{Conn: nc, closed: make(chan struct{})}
	d.mu.Lock()
	d.conns = append(d.conns, cn)
	dialed := d.dialed
	d.mu.Unlock()
	if dialed != nil {
		dialed <- cn
	}
	return cn, nil
}

// all connections that d has dialed, closed or not.
func (d *dialer) all() []*conn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.conns)
}

// conn that a [dialer] dialed, which tells when it has been closed.
type conn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

// Close the connection, and then signal closed.
func (c *conn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}

// isClosed reports whether cn has been closed. It does not block.
func isClosed(cn *conn) bool {
	select {
	case <-cn.closed:
		return true
	default:
		return false
	}
}

// waitClosed until each of conns is closed, or fail the test. net/http closes a connection in a
// goroutine of its own when it does not put the connection back in the pool, or puts it back after
// [http.Transport.CloseIdleConnections]: after an answer without content, which it hands to the call
// first, after content that is closed before its end, and after a dial that ends when its request is
// gone. So a connection may still be open when its call or the close of its content returns. The
// bound is far above the time that such a close takes on a slow machine, and far below the idle
// timeout of the transport, after which a leaked connection would be closed too.
func waitClosed(t *testing.T, conns ...*conn) {
	t.Helper()
	limit := time.After(30 * time.Second)
	for _, cn := range conns {
		select {
		case <-cn.closed:
		case <-limit:
			t.Fatal("a connection is still open after 30 seconds")
		}
	}
}

func TestClient_Close(t *testing.T) {
	t.Run("should close every connection of its transport when it is closed, also those that are idle", func(t *testing.T) {
		// Both writes are in the handler at once, so that they go over two connections.
		var arrived sync.WaitGroup
		arrived.Add(2)
		c, d := serverClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			arrived.Done()
			arrived.Wait()
			w.Header().Set("ETag", `"v"`)
		})

		var wg sync.WaitGroup
		for _, k := range []string{"a", "b"} {
			wg.Go(func() {
				_, err := c.Create(t.Context(), k, strings.NewReader("data"), nil)
				is.NotError(t, err)
			})
		}
		wg.Wait()
		conns := d.all()
		is.Equal(t, 2, len(conns))

		// The connections are idle in the pool, unless net/http kept one out of it, which it then
		// closes itself.
		is.NotError(t, c.Close())
		waitClosed(t, conns...)
		is.NotError(t, c.Close())
	})

	t.Run("should return at once from a close during a write, which succeeds, and close its connection when it has ended", func(t *testing.T) {
		arrived := make(chan struct{})
		release := make(chan struct{})
		c, d := serverClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			close(arrived)
			<-release
			w.Header().Set("ETag", `"v"`)
		})

		done := make(chan error, 1)
		var version store.Version
		go func() {
			var err error
			version, err = c.Create(t.Context(), "k", strings.NewReader("data"), nil)
			done <- err
		}()
		<-arrived
		is.NotError(t, c.Close())
		conns := d.all()
		is.Equal(t, 1, len(conns))
		is.True(t, !isClosed(conns[0]))

		close(release)
		is.NotError(t, <-done)
		is.Equal(t, store.Version(`"v"`), version)
		// net/http does not put the connection back in the pool after the close, and closes it after
		// it has handed the answer to the call, so it may still be open when the call returns.
		waitClosed(t, conns...)
		is.Equal(t, 1, len(d.all()))
	})

	t.Run("should close the connection of a read whose content is open when the content is closed", func(t *testing.T) {
		c, d := serverClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"v"`)
			_, _ = w.Write([]byte("data"))
		})

		body, _, err := c.Get(t.Context(), "k")
		is.NotError(t, err)
		is.NotError(t, c.Close())
		conns := d.all()
		is.Equal(t, 1, len(conns))
		is.True(t, !isClosed(conns[0]))

		data, err := io.ReadAll(body)
		is.NotError(t, err)
		is.Equal(t, "data", string(data))
		is.NotError(t, body.Close())
		waitClosed(t, conns...)
		is.Equal(t, 1, len(d.all()))
	})

	t.Run("should close the idle connections at once while the content of a read is open", func(t *testing.T) {
		// Both writes are in the handler at once, so that they go over two connections.
		var arrived sync.WaitGroup
		arrived.Add(2)
		c, d := serverClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"v"`)
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte("data"))
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			arrived.Done()
			arrived.Wait()
		})
		var wg sync.WaitGroup
		for _, k := range []string{"a", "b"} {
			wg.Go(func() {
				_, err := c.Create(t.Context(), k, strings.NewReader("data"), nil)
				is.NotError(t, err)
			})
		}
		wg.Wait()
		is.Equal(t, 2, len(d.all()))

		// The read takes one of the two connections, and the other stays idle, unless net/http kept
		// one out of the pool, which it then closes itself, and the read dials one of its own.
		var read atomic.Pointer[conn]
		ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				cn, ok := info.Conn.(*conn)
				is.True(t, ok)
				read.Store(cn)
			},
		})
		body, _, err := c.Get(ctx, "k")
		is.NotError(t, err)
		readConn := read.Load()
		is.True(t, readConn != nil)
		is.NotError(t, c.Close())
		var others []*conn
		for _, cn := range d.all() {
			if cn != readConn {
				others = append(others, cn)
			}
		}
		is.True(t, len(others) > 0)
		waitClosed(t, others...)
		is.True(t, !isClosed(readConn))

		// The content is closed before its end, so net/http does not reuse the connection, or puts it
		// back in the pool after the close, and closes it in a goroutine of its own.
		is.NotError(t, body.Close())
		waitClosed(t, readConn)
	})

	t.Run("should send a call after the close, and close its connection when it has ended", func(t *testing.T) {
		var requests atomic.Int32
		c, d := serverClient(t, func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("ETag", `"v"`)
		})
		is.NotError(t, c.Close())

		version, err := c.Create(t.Context(), "k", strings.NewReader("data"), nil)
		is.NotError(t, err)
		is.Equal(t, store.Version(`"v"`), version)
		is.Equal(t, int32(1), requests.Load())
		conns := d.all()
		is.Equal(t, 1, len(conns))
		waitClosed(t, conns...)
	})

	t.Run("should keep no connection whose dial ends after the close and after the call that it was for", func(t *testing.T) {
		c, d := serverClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"v"`)
		})
		hold := make(chan struct{})
		d.hold = hold
		d.dialing.Add(1)
		d.dialed = make(chan *conn, 1)
		is.NotError(t, c.Close())

		// The call ends while its dial is held, and net/http finishes the dial after it, and would
		// keep the connection as an idle one.
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, _, err := c.Get(ctx, "k")
			done <- err
		}()
		d.dialing.Wait()
		cancel()
		is.True(t, errors.Is(<-done, context.Canceled))
		close(hold)

		// The dial ends in a goroutine of net/http, so the connection shows up some time after.
		var cn *conn
		select {
		case cn = <-d.dialed:
		case <-time.After(30 * time.Second):
			t.Fatal("the dial has not ended after 30 seconds")
		}
		waitClosed(t, cn)
		is.Equal(t, 1, len(d.all()))
	})

	t.Run("should not break calls that run at once after the close", func(t *testing.T) {
		// Each call that ends while no other is in flight closes the idle connections. A call that
		// starts at that moment must not get a connection that is being closed, which would make
		// the outcome of a write unknown.
		c, _ := serverClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("ETag", `"v"`)
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte("data"))
			}
		})
		is.NotError(t, c.Close())

		// Two calls at a time end often with no other in flight. The counts made a run fail nearly
		// every time when the client closed the sockets of the pool itself.
		calls := map[string]func(i int) error{
			"get": func(int) error {
				body, _, err := c.Get(t.Context(), "k")
				if err != nil {
					return err
				}
				_, err = io.Copy(io.Discard, body)
				return errors.Join(err, body.Close())
			},
			"create": func(i int) error {
				_, err := c.Create(t.Context(), fmt.Sprint("k", i), strings.NewReader("data"), nil)
				return err
			},
		}
		var wg sync.WaitGroup
		errs := make(chan error, len(calls))
		for _, call := range calls {
			wg.Go(func() {
				for i := range 8000 {
					if err := call(i); err != nil {
						errs <- err
						return
					}
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			is.NotError(t, err)
		}
	})

	t.Run("should leave alone an HTTP client that it did not make", func(t *testing.T) {
		d := &canned{answer: func(*http.Request) *http.Response { return answer(http.StatusOK, `"v"`, "") }}
		c := d.client()
		_, err := c.Create(t.Context(), "k", strings.NewReader("data"), nil)
		is.NotError(t, err)
		is.NotError(t, c.Close())
		is.Equal(t, int32(0), d.closedIdle.Load())
	})
}

// canned is the HTTP client of a [Client] that sends nothing: each request gets the answer that
// answer gives for it, and the context of the call ends while the request is in flight, after the
// request has reached the store and before its answer is taken. That is the order in which an answer
// comes in as the context ends, which a real deadline leaves to a race, and the SDK and the client
// then run under a context that is really done.
type canned struct {
	answer     func(r *http.Request) *http.Response
	cancel     context.CancelFunc
	closedIdle atomic.Int32
}

// Do the request: report the headers as written to the trace of the client, read the body, end the
// context of the call, and give the answer.
func (d *canned) Do(r *http.Request) (*http.Response, error) {
	if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteHeaders != nil {
		trace.WroteHeaders()
	}
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
	if d.cancel != nil {
		d.cancel()
	}
	res := d.answer(r)
	res.Request = r
	return res, nil
}

// CloseIdleConnections counts the calls, which the client must not make on an HTTP client of someone
// else.
func (d *canned) CloseIdleConnections() {
	d.closedIdle.Add(1)
}

// client on d, which the client does not own.
func (d *canned) client() *Client {
	return &Client{
		s3: s3.New(s3.Options{
			Region:       "us-east-1",
			BaseEndpoint: aws.String("http://store.invalid"),
			UsePathStyle: true,
			Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
			HTTPClient:   d,
			Retryer:      aws.NopRetryer{},

			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: aws.ResponseChecksumValidationWhenSupported,
		}),
		bucket: "bucket",
		log:    discard(),
	}
}

// ctx of the next call, which the next request ends.
func (d *canned) ctx(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	d.cancel = cancel
	return ctx
}

// answer with the status, the ETag if it is not empty, and the body.
func answer(status int, etag, body string) *http.Response {
	return cutAnswer(status, etag, body, -1)
}

// cutAnswer is [answer] whose body breaks off with [io.ErrUnexpectedEOF] after at bytes, or that
// ends as it should if at is negative. The content length is always that of the whole body.
func cutAnswer(status int, etag, body string, at int) *http.Response {
	h := http.Header{}
	if etag != "" {
		h.Set("ETag", etag)
	}
	if body != "" && strings.HasPrefix(body, "<") {
		h.Set("Content-Type", "application/xml")
	}
	var r io.Reader = strings.NewReader(body)
	if at >= 0 {
		r = io.MultiReader(strings.NewReader(body[:at]), errReader{io.ErrUnexpectedEOF})
	}
	h.Set("Content-Length", fmt.Sprint(len(body)))
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprint(status, " ", http.StatusText(status)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(r),
		ContentLength: int64(len(body)),
	}
}

// s3Error is the body of an error of S3 with the code.
func s3Error(code string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>%v</Code><Message>%v</Message><RequestId>r</RequestId></Error>`, code, code)
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) {
	return 0, e.err
}

// isUnknown reports whether err is the error of a write whose outcome is unknown: it is none of the
// errors that mean that a write was not applied.
func isUnknown(err error) bool {
	if err == nil {
		return false
	}
	for _, r := range []error{store.ErrNotFound, store.ErrExists, store.ErrChanged, store.ErrChecksum, store.ErrTooLarge} {
		if errors.Is(err, r) {
			return false
		}
	}
	return true
}

func TestClient_endedContext(t *testing.T) {
	t.Run("should wrap the error of a context that ended before the answer came in, unless the answer shows what happened", func(t *testing.T) {
		d := &canned{answer: func(r *http.Request) *http.Response {
			k := strings.TrimPrefix(r.URL.Path, "/bucket/")
			switch k {
			case "broken":
				return answer(http.StatusInternalServerError, "", s3Error("InternalError"))
			case "applied":
				return answer(http.StatusOK, `"v"`, "")
			case "exists":
				return answer(http.StatusPreconditionFailed, "", s3Error("PreconditionFailed"))
			case "missing":
				return answer(http.StatusNotFound, "", s3Error("NoSuchKey"))
			case "cut-body":
				return cutAnswer(http.StatusOK, `"v"`, strings.Repeat("x", 100), 4)
			case "cut-exists":
				body := s3Error("PreconditionFailed")
				return cutAnswer(http.StatusPreconditionFailed, "", body, strings.Index(body, "Precondition"))
			case "cut-missing":
				body := s3Error("NoSuchKey")
				return cutAnswer(http.StatusNotFound, "", body, strings.Index(body, "NoSuch"))
			case "cut-applied":
				return cutAnswer(http.StatusOK, `"v"`, "<x/>", 1)
			case "empty-missing":
				return answer(http.StatusNotFound, "", "")
			default:
				// An answer without headers, as a store or a proxy may give to a request that it sees
				// end.
				return answer(http.StatusOK, "", "")
			}
		}}
		c := d.client()
		data := func() io.ReadSeeker { return bytes.NewReader([]byte("data")) }

		for _, k := range []string{"no-etag", "broken"} {
			_, _, err := c.Get(d.ctx(t), k)
			is.True(t, errors.Is(err, context.Canceled), k, fmt.Sprint(err))

			_, err = c.Create(d.ctx(t), k, data(), nil)
			is.True(t, isUnknown(err) && errors.Is(err, context.Canceled), k, fmt.Sprint(err))
			is.Equal(t, 1, strings.Count(err.Error(), context.Canceled.Error()), err.Error())
		}

		// The content of a read that fails after the end of the context.
		body, _, err := c.Get(d.ctx(t), "cut-body")
		is.NotError(t, err)
		_, err = io.ReadAll(body)
		_ = body.Close()
		is.True(t, errors.Is(err, io.ErrUnexpectedEOF) && errors.Is(err, context.Canceled), fmt.Sprint(err))

		// An answer that shows what happened is the outcome, also after the end of the context.
		version, err := c.Create(d.ctx(t), "applied", data(), nil)
		is.NotError(t, err)
		is.Equal(t, store.Version(`"v"`), version)

		_, err = c.Create(d.ctx(t), "exists", data(), nil)
		is.True(t, errors.Is(err, store.ErrExists), fmt.Sprint(err))

		_, err = c.Swap(d.ctx(t), "exists", `"v"`, data(), nil)
		is.True(t, errors.Is(err, store.ErrChanged), fmt.Sprint(err))

		_, _, err = c.Get(d.ctx(t), "missing")
		is.True(t, errors.Is(err, store.ErrNotFound), fmt.Sprint(err))

		// A refusal whose body breaks off does not show what happened.
		_, err = c.Create(d.ctx(t), "cut-exists", data(), nil)
		is.True(t, isUnknown(err) && errors.Is(err, context.Canceled), fmt.Sprint(err))

		_, err = c.Swap(d.ctx(t), "cut-exists", `"v"`, data(), nil)
		is.True(t, isUnknown(err) && errors.Is(err, context.Canceled), fmt.Sprint(err))

		_, _, err = c.Get(d.ctx(t), "cut-missing")
		is.True(t, !errors.Is(err, store.ErrNotFound) && errors.Is(err, context.Canceled), fmt.Sprint(err))

		// A write that was applied, whose answer breaks off, is reported as one whose outcome is
		// unknown: the safe direction.
		_, err = c.Create(d.ctx(t), "cut-applied", data(), nil)
		is.True(t, isUnknown(err) && errors.Is(err, context.Canceled), fmt.Sprint(err))

		// A 404 without the code of S3 is not taken for a missing object.
		_, _, err = c.Get(d.ctx(t), "empty-missing")
		is.True(t, !errors.Is(err, store.ErrNotFound) && errors.Is(err, context.Canceled), fmt.Sprint(err))
	})
}
