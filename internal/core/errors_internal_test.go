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
