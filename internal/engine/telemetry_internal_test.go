package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestOutcomeOf(t *testing.T) {
	cases := []struct {
		err     error
		outcome string
		level   slog.Level
	}{
		{nil, "ok", slog.LevelDebug},
		{noSuchTable("t"), "sqlite_error", slog.LevelDebug},
		{fmt.Errorf("x: %w", &stmt.NotSupportedError{What: "WHERE clause"}), "not_supported", slog.LevelDebug},
		{fmt.Errorf("x: %w", ErrNoStatement), "no_statement", slog.LevelDebug},
		{internalError("x"), "internal_error", slog.LevelError},
		{fmt.Errorf("%w: %w", core.ErrOutcomeUnknown, context.DeadlineExceeded), "core_error", slog.LevelWarn},
		{fmt.Errorf("x: %w", core.ErrNoLease), "refused", slog.LevelDebug},
		{fmt.Errorf("x: %w", core.ErrStopped), "refused", slog.LevelDebug},
		{fmt.Errorf("x: %w", context.Canceled), "canceled", slog.LevelDebug},
		{fmt.Errorf("x: %w", context.DeadlineExceeded), "canceled", slog.LevelDebug},
		{fmt.Errorf("x: %w", core.ErrBusy), "core_error", slog.LevelWarn},
		{errors.New("anything else"), "core_error", slog.LevelWarn},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.err), func(t *testing.T) {
			outcome, level := outcomeOf(c.err)
			is.Equal(t, c.outcome, outcome)
			is.Equal(t, c.level, level)
		})
	}
}
