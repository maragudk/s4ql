package core

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"maragu.dev/is"
)

func TestEnded(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Unix(0, 0))
	defer cancelExpired()

	tests := []struct {
		name     string
		ctx      context.Context
		err      error
		expected bool
	}{
		{name: "should report the error of a canceled context", ctx: canceled, err: context.Canceled, expected: true},
		{name: "should report the error of a context past its deadline", ctx: expired, err: context.DeadlineExceeded, expected: true},
		{name: "should report the error of the context wrapped", ctx: canceled, err: fmt.Errorf("scan: %w", context.Canceled), expected: true},
		{name: "should not report no error of a context that has ended", ctx: canceled, err: nil, expected: false},
		{name: "should not report another error of a context that has ended", ctx: canceled, err: errors.New("disk I/O error"), expected: false},
		{name: "should not report the error of another context that has ended", ctx: expired, err: context.Canceled, expected: false},
		{name: "should not report the error of a context that has not ended", ctx: t.Context(), err: context.Canceled, expected: false},
		{name: "should not report no error of a context that has not ended", ctx: t.Context(), err: nil, expected: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			is.Equal(t, test.expected, ended(test.ctx, test.err))
		})
	}
}

func TestEndedError(t *testing.T) {
	t.Run("should give the error of a context that ended without a cause", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		is.Equal(t, context.Canceled, endedError(ctx))
	})

	t.Run("should give the error of a context past its deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(t.Context(), time.Unix(0, 0))
		defer cancel()
		is.Equal(t, context.DeadlineExceeded, endedError(ctx))
	})

	t.Run("should give the error of a context that ended with a cause, with the cause in the text and not wrapped", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		cause := fmt.Errorf("another goroutine: %w, %w", ErrOutcomeUnknown, ErrStopped)
		cancel(cause)
		err := endedError(ctx)
		is.True(t, errors.Is(err, context.Canceled), err.Error())
		for _, target := range []error{cause, ErrOutcomeUnknown, ErrStopped} {
			is.True(t, !errors.Is(err, target), err.Error())
		}
		is.Equal(t, "context canceled (cause: another goroutine: the outcome of the commit is unknown, the instance has stopped)", err.Error())
	})
}
