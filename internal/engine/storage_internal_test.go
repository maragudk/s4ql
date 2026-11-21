package engine

import (
	"errors"
	"fmt"
	"iter"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/sqlerr"
)

func TestFromCore(t *testing.T) {
	t.Run("should make each refusal of the core an internal error, and a value or a row that is too long SQLite's error", func(t *testing.T) {
		for _, err := range []error{core.ErrTableExists, core.ErrNoTable, core.ErrIndexExists, core.ErrNoIndex, core.ErrReservedName, core.ErrInvalid, core.ErrTxDone, core.ErrReaderClosed} {
			is.True(t, errors.Is(fromCore(fmt.Errorf("a call: %w", err)), ErrInternal), err.Error())
		}
		var se *sqlerr.Error
		is.True(t, errors.As(fromCore(fmt.Errorf("upsert: %w", core.ErrTooBig)), &se))
		is.Equal(t, sqlerr.Error{Code: 18, ExtendedCode: 18, Message: "string or blob too big", Offset: -1}, *se)
		other := errors.New("another error")
		is.Equal(t, other, fromCore(other))
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
