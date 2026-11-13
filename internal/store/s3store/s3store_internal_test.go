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
		}
	})
}
