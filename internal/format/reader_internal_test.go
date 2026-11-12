package format

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"
)

func TestCommitReader_maxRecordSize(t *testing.T) {
	var b bytes.Buffer
	w, err := NewCommitWriter(&b, EmptyLog, time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC))
	is.NotError(t, err)
	headerSize := b.Len()
	is.NotError(t, w.DropTable(strings.Repeat("x", 100)))
	name, err := w.Close()
	is.NotError(t, err)

	read := func(limit uint64) error {
		r := NewCommitReader(bytes.NewReader(b.Bytes()), name)
		r.maxRecordSize = limit
		for {
			_, _, err := r.Next()
			if err != nil {
				return err
			}
		}
	}

	t.Run("should refuse a record above the limit", func(t *testing.T) {
		err := read(uint64(headerSize) + 10)
		is.True(t, errors.Is(err, ErrMalformed), "error is not malformed: "+err.Error())
	})

	t.Run("should read a record at the limit", func(t *testing.T) {
		// The change is the table name, its tag and length, and the tag and length of the drop table.
		err := read(100 + 4)
		is.True(t, errors.Is(err, io.EOF), "error is not the end: "+err.Error())
	})
}
