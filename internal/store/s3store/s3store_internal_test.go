package s3store

import (
	"errors"
	"log/slog"
	"net/http"
	"testing"

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
