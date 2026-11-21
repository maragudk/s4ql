package ctxtest_test

import (
	"context"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/ctxtest"
)

type key struct{}

// lookAt the Err of ctx, from a function of a name that [ctxtest.Context.EndedBy] can give.
func lookAt(ctx context.Context) error {
	return ctx.Err()
}

func TestEndAtErr(t *testing.T) {
	t.Run("should end at the nth call of Err, and stay ended", func(t *testing.T) {
		ctx := ctxtest.EndAtErr(t.Context(), 3)
		for range 2 {
			is.NotError(t, ctx.Err())
			select {
			case <-ctx.Done():
				t.Fatal("done before the third call of Err")
			default:
			}
		}
		is.Error(t, context.Canceled, ctx.Err())
		<-ctx.Done()
		is.Error(t, context.Canceled, ctx.Err())
		is.Equal(t, 4, ctx.Calls())
	})

	t.Run("should give the function that made the call of Err that ended it, and none before", func(t *testing.T) {
		ctx := ctxtest.EndAtErr(t.Context(), 2)
		is.NotError(t, ctx.Err())
		is.Equal(t, "", ctx.EndedBy())
		is.Error(t, context.Canceled, lookAt(ctx))
		is.Error(t, context.Canceled, ctx.Err())
		is.Equal(t, "maragu.dev/s4ql/internal/ctxtest_test.lookAt", ctx.EndedBy())
	})

	t.Run("should end at the first call of Err for an n below 1", func(t *testing.T) {
		is.Error(t, context.Canceled, ctxtest.EndAtErr(t.Context(), 0).Err())
		is.Error(t, context.Canceled, ctxtest.EndAtErr(t.Context(), -1).Err())
	})

	t.Run("should have the values of its parent and no deadline, and not end with its parent", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.WithValue(t.Context(), key{}, "v"))
		cancel()
		ctx := ctxtest.EndAtErr(parent, 2)
		is.Equal(t, "v", ctx.Value(key{}))
		_, ok := ctx.Deadline()
		is.True(t, !ok)
		is.NotError(t, ctx.Err())
	})
}
