package format

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/s4qlpb"
)

func TestCommitWriter_maxRecordSize(t *testing.T) {
	t.Run("should refuse a record above the limit, and write nothing more", func(t *testing.T) {
		var b bytes.Buffer
		w, err := NewCommitWriter(&b, EmptyLog, time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC))
		is.NotError(t, err)
		written := b.Len()
		w.maxRecordSize = 103

		err = w.DropTable(strings.Repeat("x", 100))
		is.True(t, errors.Is(err, ErrMalformed), "error is not malformed")
		is.Equal(t, written, b.Len())
		_, err = w.Close()
		is.True(t, errors.Is(err, ErrMalformed), "error from Close is not malformed")
	})

	t.Run("should write a record at the limit", func(t *testing.T) {
		var b bytes.Buffer
		w, err := NewCommitWriter(&b, EmptyLog, time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC))
		is.NotError(t, err)
		w.maxRecordSize = 104

		is.NotError(t, w.DropTable(strings.Repeat("x", 100)))
		name, err := w.Close()
		is.NotError(t, err)
		is.Equal(t, Name(sha256.Sum256(b.Bytes())), name)
	})

	t.Run("should refuse a row that is above the limit in the call that hands it in, and write nothing more", func(t *testing.T) {
		var b bytes.Buffer
		w, err := NewCommitWriter(&b, EmptyLog, time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC))
		is.NotError(t, err)
		is.NotError(t, w.UpsertRow("t", []uint32{0}, []*s4qlpb.Value{s4qlpb.Value_builder{Integer: new(int64(1))}.Build()}))
		written := b.Len()
		w.maxRecordSize = 100

		err = w.UpsertRow("u", []uint32{0}, []*s4qlpb.Value{s4qlpb.Value_builder{Text: new(strings.Repeat("x", 100))}.Build()})
		is.True(t, errors.Is(err, ErrMalformed), "error is not malformed")
		is.Equal(t, written, b.Len())
	})

	t.Run("should refuse a key that is above the limit in the call that hands it in", func(t *testing.T) {
		w, err := NewCommitWriter(&bytes.Buffer{}, EmptyLog, time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC))
		is.NotError(t, err)
		w.maxRecordSize = 100

		err = w.DeleteRow("t", []*s4qlpb.Value{s4qlpb.Value_builder{Text: new(strings.Repeat("x", 100))}.Build()})
		is.True(t, errors.Is(err, ErrMalformed), "error is not malformed")
	})

	t.Run("should start a new batch where the next row would take a batch above the limit", func(t *testing.T) {
		var b bytes.Buffer
		w, err := NewCommitWriter(&b, EmptyLog, time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC))
		is.NotError(t, err)
		w.maxRecordSize = 50
		for i := range 21 {
			is.NotError(t, w.UpsertRow("t", []uint32{0}, []*s4qlpb.Value{s4qlpb.Value_builder{Integer: new(int64(i))}.Build()}))
		}
		name, err := w.Close()
		is.NotError(t, err)

		r := NewCommitReader(bytes.NewReader(b.Bytes()), name)
		r.maxRecordSize = 50
		var rows, batches int
		for {
			c, _, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			is.NotError(t, err)
			rows += len(c.GetUpsertRows().GetRows())
			batches++
		}
		is.Equal(t, 21, rows)
		is.True(t, batches > 1, "one batch")
	})
}
