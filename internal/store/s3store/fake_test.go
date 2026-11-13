package s3store_test

import (
	"crypto/md5"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/store/s3store"
)

// fake S3 for tests without a network: a TLS server that keeps objects in memory, applies
// PutObject with If-None-Match: * and If-Match as S3 does, checks the payload against
// x-amz-content-sha256, and lets a test take over any request with a hook.
type fake struct {
	srv *httptest.Server

	mu      sync.Mutex
	objects map[string]fakeObject
	// requests by method and key, such as "PUT k".
	requests map[string]int
	// hook, if it is not nil, sees every request first, and answers it itself if it returns true.
	hook func(w http.ResponseWriter, r *http.Request, f *fake) bool
}

type fakeObject struct {
	data     []byte
	etag     string
	checksum string
}

// newFake S3, which is closed when the test ends.
func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{objects: map[string]fakeObject{}, requests: map[string]int{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// client of the fake, with credentials and a region from the environment of the test.
func (f *fake) client(t *testing.T, opts ...func(*s3store.Options)) *s3store.Client {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ENDPOINT_URL_S3", "")
	o := s3store.Options{
		Bucket:    "bucket",
		Endpoint:  f.srv.URL,
		Region:    "us-east-1",
		PathStyle: true,
		RootCAs:   f.pool(),
	}
	for _, opt := range opts {
		opt(&o)
	}
	c, err := s3store.New(t.Context(), o)
	is.NotError(t, err)
	return c
}

func (f *fake) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(f.srv.Certificate())
	return p
}

// count of the requests with the given method and key.
func (f *fake) count(method, key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[method+" "+key]
}

// object at key, and whether there is one.
func (f *fake) object(key string) (fakeObject, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	return o, ok
}

// put an object at key directly.
func (f *fake) put(key string, data []byte) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := fakeObject{data: data, etag: etag(data)}
	f.objects[key] = o
	return o.etag
}

func etag(data []byte) string {
	return fmt.Sprintf(`"%x"`, md5.Sum(data))
}

// key of a path-style request to the bucket.
func key(r *http.Request) string {
	return strings.TrimPrefix(r.URL.Path, "/bucket/")
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests[r.Method+" "+key(r)]++
	hook := f.hook
	f.mu.Unlock()
	if hook != nil && hook(w, r, f) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.get(w, r)
	case http.MethodPut:
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		f.apply(w, r, data)
	default:
		fail(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func (f *fake) get(w http.ResponseWriter, r *http.Request) {
	o, ok := f.object(key(r))
	if !ok {
		fail(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	w.Header().Set("ETag", o.etag)
	w.Header().Set("Content-Length", fmt.Sprint(len(o.data)))
	if o.checksum != "" && r.Header.Get("x-amz-checksum-mode") == "ENABLED" {
		w.Header().Set("x-amz-checksum-sha256", o.checksum)
		w.Header().Set("x-amz-checksum-type", "FULL_OBJECT")
	}
	_, _ = w.Write(o.data)
}

// apply a PutObject with the given content, as S3 does.
func (f *fake) apply(w http.ResponseWriter, r *http.Request, data []byte) {
	sum := sha256.Sum256(data)
	if h := r.Header.Get("x-amz-content-sha256"); h != "UNSIGNED-PAYLOAD" && h != hex.EncodeToString(sum[:]) {
		fail(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch")
		return
	}
	if h := r.Header.Get("x-amz-checksum-sha256"); h != "" && h != base64.StdEncoding.EncodeToString(sum[:]) {
		fail(w, http.StatusBadRequest, "BadDigest")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(r)
	current, exists := f.objects[k]
	switch {
	case r.Header.Get("If-None-Match") == "*" && exists:
		fail(w, http.StatusPreconditionFailed, "PreconditionFailed")
		return
	case r.Header.Get("If-Match") != "" && !exists:
		fail(w, http.StatusNotFound, "NoSuchKey")
		return
	case r.Header.Get("If-Match") != "" && r.Header.Get("If-Match") != current.etag:
		fail(w, http.StatusPreconditionFailed, "PreconditionFailed")
		return
	}
	o := fakeObject{data: data, etag: etag(data), checksum: r.Header.Get("x-amz-checksum-sha256")}
	f.objects[k] = o
	w.Header().Set("ETag", o.etag)
	w.Header().Set("x-amz-request-id", "fake-request")
	w.WriteHeader(http.StatusOK)
}

// fail the request with an error of S3.
func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", "fake-request")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%v</Code><Message>%v</Message><RequestId>fake-request</RequestId></Error>`, code, code)
}

// cut the connection of the request without an answer.
func cut(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(err)
	}
	_ = conn.Close()
}
