//go:build cgo

// The tests in this file are of a step whose context ends within it, which SQLite stops in the cgo
// binding, whose [sqlite.Stmt.StepContext] installs a progress handler. The browser's binding runs
// a step to its end, so they are not built there.

package core

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/ctxtest"
	"maragu.dev/s4ql/internal/sqlite/faultvfs"
)

func TestConn_stepContext(t *testing.T) {
	t.Run("should end the step of a sort with the error of the context when the context ends in it, and put the length limit of the connection back after it", func(t *testing.T) {
		step := stepSort(t)
		ctx := ctxtest.EndAtErr(t.Context(), 2)
		err := step(ctx, faultvfs.Fault{})
		is.True(t, errors.Is(err, context.Canceled), fmt.Sprint(err))
		is.Equal(t, "maragu.dev/s4ql/internal/sqlite.s4qlInterrupted", ctx.EndedBy())
	})
}
