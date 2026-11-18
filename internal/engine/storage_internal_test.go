package engine

import (
	"errors"
	"iter"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/s4qlpb"
)

func TestFromCore(t *testing.T) {
	t.Run("should make a call on a transaction that has ended an internal error", func(t *testing.T) {
		is.True(t, errors.Is(fromCore(core.ErrTxDone), ErrInternal))
	})

	t.Run("should end a scan at its first error", func(t *testing.T) {
		seq := iter.Seq2[[]*s4qlpb.Value, error](func(yield func([]*s4qlpb.Value, error) bool) {
			if !yield(nil, core.ErrNoTable) {
				return
			}
			yield([]*s4qlpb.Value{}, nil)
		})
		n := 0
		for _, err := range fromCoreSeq(seq) {
			is.True(t, errors.Is(err, ErrInternal))
			n++
		}
		is.Equal(t, 1, n)
	})
}
