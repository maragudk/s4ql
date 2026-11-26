package engine

import (
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/core/coretest"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/store/simstore"
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

	t.Run("should refuse a table and an index whose statement in the cache is longer than the length limit as not supported, by its name", func(t *testing.T) {
		h := sim.NewFree(sim.Buggify{})
		d, err := core.Open(t.Context(), h, simstore.New(h, simstore.Options{}).Client("a", nil), filepath.Join(t.TempDir(), "a.db"), "db", coretest.SyncOff(core.Options{LengthLimit: 1000}))
		is.NotError(t, err)
		defer func() { _ = d.Close(t.Context()) }()
		is.NotError(t, d.Wait(t.Context()))
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		defer func() { _ = tx.Rollback(t.Context()) }()
		w := coreTx{tx: tx}
		column := func(name string) *s4qlpb.Column {
			return s4qlpb.Column_builder{Name: new(name), Type: new(s4qlpb.Type_TYPE_INT)}.Build()
		}
		long := strings.Repeat("x", 1000)

		err = w.createTable(t.Context(), s4qlpb.CreateTable_builder{Table: new(long), Columns: []*s4qlpb.Column{column("k")}, PrimaryKey: []uint32{0}}.Build())
		is.True(t, errors.Is(err, stmt.ErrNotSupported))
		is.Equal(t, "s4ql: not supported: the table "+long+", whose statement in the cache is longer than the length limit", err.Error())

		is.NotError(t, w.createTable(t.Context(), s4qlpb.CreateTable_builder{Table: new("t"), Columns: []*s4qlpb.Column{column("k")}, PrimaryKey: []uint32{0}}.Build()))
		err = w.createIndex(t.Context(), s4qlpb.CreateIndex_builder{Index: new(long), Table: new("t"), Columns: []*s4qlpb.IndexColumn{s4qlpb.IndexColumn_builder{Column: new(uint32(0))}.Build()}}.Build())
		is.True(t, errors.Is(err, stmt.ErrNotSupported))
		is.Equal(t, "s4ql: not supported: the index "+long+", whose statement in the cache is longer than the length limit", err.Error())

		// Any other refusal of the core is an internal error.
		err = w.createIndex(t.Context(), s4qlpb.CreateIndex_builder{Index: new("i"), Table: new("nosuch"), Columns: []*s4qlpb.IndexColumn{s4qlpb.IndexColumn_builder{Column: new(uint32(0))}.Build()}}.Build())
		is.True(t, errors.Is(err, ErrInternal), fmt.Sprint(err))
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
