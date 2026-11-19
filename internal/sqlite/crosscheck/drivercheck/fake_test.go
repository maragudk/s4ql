package drivercheck_test

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeS3 is an object store in memory behind S3's HTTP API, path-style and without TLS, for
// s4ql's store client: GetObject, and PutObject with If-None-Match: * and with If-Match, which it
// applies as S3 does. It checks the content against x-amz-content-sha256 and x-amz-checksum-sha256.
type fakeS3 struct {
	srv *httptest.Server

	mu      sync.Mutex
	objects map[string]fakeObject
}

type fakeObject struct {
	data     []byte
	etag     string
	checksum string
}

// newFakeS3, which is closed when the test ends.
func newFakeS3(tb testing.TB) *fakeS3 {
	tb.Helper()
	f := &fakeS3{objects: map[string]fakeObject{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	tb.Cleanup(f.srv.Close)
	return f
}

// isolate the test from the AWS configuration of the machine that runs it: no shared files, and no
// settings of the SDK from the environment. The options of s4ql name the rest.
func isolate(tb testing.TB) {
	tb.Helper()
	dir := tb.TempDir()
	tb.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	tb.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	for _, name := range []string{
		"AWS_PROFILE", "AWS_SESSION_TOKEN", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_S3", "AWS_CA_BUNDLE",
		"AWS_DEFAULTS_MODE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
	} {
		tb.Setenv(name, "")
	}
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutPrefix(r.URL.Path, "/bucket/")
	if !ok {
		fail(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.get(w, r, key)
	case http.MethodPut:
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		f.put(w, r, key, data)
	default:
		fail(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func (f *fakeS3) get(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	o, ok := f.objects[key]
	f.mu.Unlock()
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

// put the content data at key, as S3 applies a PutObject.
func (f *fakeS3) put(w http.ResponseWriter, r *http.Request, key string, data []byte) {
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
	current, exists := f.objects[key]
	switch match := r.Header.Get("If-Match"); {
	case r.Header.Get("If-None-Match") == "*" && exists:
		fail(w, http.StatusPreconditionFailed, "PreconditionFailed")
		return
	case match != "" && !exists:
		fail(w, http.StatusNotFound, "NoSuchKey")
		return
	case match != "" && match != current.etag:
		fail(w, http.StatusPreconditionFailed, "PreconditionFailed")
		return
	}
	o := fakeObject{data: data, etag: fmt.Sprintf(`"%x"`, md5.Sum(data)), checksum: r.Header.Get("x-amz-checksum-sha256")}
	f.objects[key] = o
	w.Header().Set("ETag", o.etag)
	w.WriteHeader(http.StatusOK)
}

// fail the request with an error of S3.
func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%v</Code><Message>%v</Message></Error>`, code, code)
}
