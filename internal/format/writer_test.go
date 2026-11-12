package format_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"maragu.dev/is"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
)

var testCommitTime = time.Date(2026, 11, 12, 12, 0, 0, 123_456_789, time.UTC)

func TestCommitWriter(t *testing.T) {
	t.Run("should write the bytes of one commit object message, and its name", func(t *testing.T) {
		var b bytes.Buffer
		name := writeExample(t, &b)
		is.Equal(t, format.Name(sha256.Sum256(b.Bytes())), name)

		var whole s4qlpb.CommitObject
		is.NotError(t, proto.Unmarshal(b.Bytes(), &whole))
		is.Equal(t, 6, len(whole.GetChanges()))
		is.Equal(t, 2, len(whole.GetAnnotations()))

		again, err := proto.MarshalOptions{Deterministic: true}.Marshal(&whole)
		is.NotError(t, err)
		is.True(t, bytes.Equal(b.Bytes(), again), "the whole message marshals to other bytes")

		again, err = proto.Marshal(&whole)
		is.NotError(t, err)
		is.True(t, bytes.Equal(b.Bytes(), again), "the whole message marshals to other bytes without determinism")
	})

	t.Run("should write the same bytes for the same commit object", func(t *testing.T) {
		var b1, b2 bytes.Buffer
		writeExample(t, &b1)
		writeExample(t, &b2)
		is.True(t, bytes.Equal(b1.Bytes(), b2.Bytes()), "bytes differ")
	})

	t.Run("should write what the reader reads back, with the header first", func(t *testing.T) {
		var b bytes.Buffer
		name := writeExample(t, &b)
		h, items, err := readAll(b.Bytes(), name)
		is.NotError(t, err)
		is.Equal(t, uint32(0), h.GetVersion())
		is.True(t, testCommitTime.Equal(h.GetCommitTime().AsTime()), "commit time differs")
		parent := sha256.Sum256([]byte("parent"))
		is.True(t, bytes.Equal(parent[:], h.GetParent()), "parent differs")

		is.Equal(t, 8, len(items))
		is.True(t, proto.Equal(exampleCreateTable(), items[0].change.GetCreateTable()), "create table differs")
		is.True(t, proto.Equal(upsertRows("t", []uint32{0, 1}, []*s4qlpb.Value{integer(1), text("a")}, []*s4qlpb.Value{integer(2), null()}), items[1].change), "first upsert differs")
		is.True(t, proto.Equal(upsertRows("t", []uint32{1, 0}, []*s4qlpb.Value{blob([]byte{1}), integer(3)}), items[2].change), "upsert with other columns differs")
		is.True(t, proto.Equal(deleteRows("t", []*s4qlpb.Value{integer(1)}, []*s4qlpb.Value{integer(2)}), items[3].change), "delete differs")
		is.True(t, proto.Equal(upsertRows("t", []uint32{0}, []*s4qlpb.Value{integer(4)}), items[4].change), "upsert after delete differs")
		is.True(t, proto.Equal(dropTable("t"), items[5].change), "drop differs")
		is.True(t, proto.Equal(statement("insert into t values (?, ?)", integer(1), text("a")), items[6].annotation), "first annotation differs")
		is.True(t, proto.Equal(statement("drop table t"), items[7].annotation), "second annotation differs")
	})

	t.Run("should write a commit object with only a header", func(t *testing.T) {
		var b bytes.Buffer
		w, err := format.NewCommitWriter(&b, format.EmptyLog, testCommitTime)
		is.NotError(t, err)
		name, err := w.Close()
		is.NotError(t, err)
		h, items, err := readAll(b.Bytes(), name)
		is.NotError(t, err)
		is.True(t, bytes.Equal(format.EmptyLog[:], h.GetParent()), "parent is not the empty log")
		is.Equal(t, 0, len(items))
	})

	t.Run("should keep the rows in order, and start a new batch for a new table or new columns", func(t *testing.T) {
		var b bytes.Buffer
		w, err := format.NewCommitWriter(&b, format.EmptyLog, testCommitTime)
		is.NotError(t, err)
		is.NotError(t, w.UpsertRow("a", []uint32{0}, []*s4qlpb.Value{integer(1)}))
		is.NotError(t, w.UpsertRow("a", []uint32{0}, []*s4qlpb.Value{integer(2)}))
		is.NotError(t, w.UpsertRow("b", []uint32{0}, []*s4qlpb.Value{integer(3)}))
		is.NotError(t, w.UpsertRow("a", []uint32{0}, []*s4qlpb.Value{integer(4)}))
		is.NotError(t, w.UpsertRow("a", []uint32{0, 1}, []*s4qlpb.Value{integer(5), integer(6)}))
		is.NotError(t, w.DeleteRow("a", []*s4qlpb.Value{integer(1)}))
		is.NotError(t, w.DeleteRow("b", []*s4qlpb.Value{integer(3)}))
		is.NotError(t, w.DeleteRow("b", []*s4qlpb.Value{integer(4)}))
		name, err := w.Close()
		is.NotError(t, err)

		_, items, err := readAll(b.Bytes(), name)
		is.NotError(t, err)
		want := []*s4qlpb.Change{
			upsertRows("a", []uint32{0}, []*s4qlpb.Value{integer(1)}, []*s4qlpb.Value{integer(2)}),
			upsertRows("b", []uint32{0}, []*s4qlpb.Value{integer(3)}),
			upsertRows("a", []uint32{0}, []*s4qlpb.Value{integer(4)}),
			upsertRows("a", []uint32{0, 1}, []*s4qlpb.Value{integer(5), integer(6)}),
			deleteRows("a", []*s4qlpb.Value{integer(1)}),
			deleteRows("b", []*s4qlpb.Value{integer(3)}, []*s4qlpb.Value{integer(4)}),
		}
		is.Equal(t, len(want), len(items))
		for i := range want {
			is.True(t, proto.Equal(want[i], items[i].change), want[i].String())
		}
	})

	t.Run("should make batches of about one MiB", func(t *testing.T) {
		var b bytes.Buffer
		w, err := format.NewCommitWriter(&b, format.EmptyLog, testCommitTime)
		is.NotError(t, err)
		value := strings.Repeat("x", 1000)
		const rows = 3000
		for i := range rows {
			is.NotError(t, w.UpsertRow("t", []uint32{0, 1}, []*s4qlpb.Value{integer(int64(i)), text(value)}))
		}
		for range rows {
			is.NotError(t, w.DeleteRow("t", []*s4qlpb.Value{text(value)}))
		}
		name, err := w.Close()
		is.NotError(t, err)

		_, items, err := readAll(b.Bytes(), name)
		is.NotError(t, err)
		var upserted, deleted int
		var upsertSizes, deleteSizes []int
		for _, item := range items {
			size := proto.Size(item.change)
			is.True(t, size <= 1<<20+100, "batch is above one MiB")
			if ur := item.change.GetUpsertRows(); ur != nil {
				for _, row := range ur.GetRows() {
					is.Equal(t, int64(upserted), row.GetValues()[0].GetInteger())
					upserted++
				}
				upsertSizes = append(upsertSizes, size)
			} else {
				deleted += len(item.change.GetDeleteRows().GetPrimaryKeys())
				deleteSizes = append(deleteSizes, size)
			}
		}
		is.Equal(t, rows, upserted)
		is.Equal(t, rows, deleted)
		is.Equal(t, 3, len(upsertSizes))
		is.Equal(t, 3, len(deleteSizes))
		for _, size := range append(upsertSizes[:2], deleteSizes[:2]...) {
			is.True(t, size >= 1<<20-2000, "batch that is not the last is well below one MiB")
		}
	})

	t.Run("should write a row larger than a batch in a batch of its own", func(t *testing.T) {
		var b bytes.Buffer
		w, err := format.NewCommitWriter(&b, format.EmptyLog, testCommitTime)
		is.NotError(t, err)
		large := strings.Repeat("x", 2<<20)
		is.NotError(t, w.UpsertRow("t", []uint32{0}, []*s4qlpb.Value{text("a")}))
		is.NotError(t, w.UpsertRow("t", []uint32{0}, []*s4qlpb.Value{text(large)}))
		is.NotError(t, w.UpsertRow("t", []uint32{0}, []*s4qlpb.Value{text("b")}))
		name, err := w.Close()
		is.NotError(t, err)

		_, items, err := readAll(b.Bytes(), name)
		is.NotError(t, err)
		is.Equal(t, 3, len(items))
		is.Equal(t, large, items[1].change.GetUpsertRows().GetRows()[0].GetValues()[0].GetText())
	})

	t.Run("should copy the values it is handed", func(t *testing.T) {
		var b bytes.Buffer
		w, err := format.NewCommitWriter(&b, format.EmptyLog, testCommitTime)
		is.NotError(t, err)
		columns := []uint32{0}
		v := integer(1)
		is.NotError(t, w.UpsertRow("t", columns, []*s4qlpb.Value{v}))
		v.SetInteger(2)
		columns[0] = 1
		name, err := w.Close()
		is.NotError(t, err)

		_, items, err := readAll(b.Bytes(), name)
		is.NotError(t, err)
		is.True(t, proto.Equal(upsertRows("t", []uint32{0}, []*s4qlpb.Value{integer(1)}), items[0].change), "the change differs")
	})

	t.Run("should make one call of Write for the header and for each change and annotation", func(t *testing.T) {
		var cw countingWriter
		writeExample(t, &cw)
		is.Equal(t, 9, cw.writes)
	})

	t.Run("should refuse a change after an annotation", func(t *testing.T) {
		w, err := format.NewCommitWriter(&bytes.Buffer{}, format.EmptyLog, testCommitTime)
		is.NotError(t, err)
		is.NotError(t, w.Annotate(statement("select 1")))
		is.True(t, w.DropTable("t") != nil, "no error")
		_, err = w.Close()
		is.True(t, err != nil, "no error from Close")
	})

	t.Run("should return the error of the underlying writer, and the same error after it", func(t *testing.T) {
		failure := errors.New("disk full")
		fw := &failingWriter{failAfter: 1, err: failure}
		w, err := format.NewCommitWriter(fw, format.EmptyLog, testCommitTime)
		is.NotError(t, err)
		is.NotError(t, w.UpsertRow("t", []uint32{0}, []*s4qlpb.Value{integer(1)}))
		err = w.DropTable("t")
		is.True(t, errors.Is(err, failure), "error is not the writer's")
		err = w.DropTable("u")
		is.True(t, errors.Is(err, failure), "error is not the writer's the second time")
		_, err = w.Close()
		is.True(t, errors.Is(err, failure), "error is not the writer's from Close")
	})

	t.Run("should return the error of the underlying writer for the header", func(t *testing.T) {
		failure := errors.New("disk full")
		_, err := format.NewCommitWriter(&failingWriter{err: failure}, format.EmptyLog, testCommitTime)
		is.True(t, errors.Is(err, failure), "error is not the writer's")
	})

	t.Run("should return an error from Close after Close", func(t *testing.T) {
		w, err := format.NewCommitWriter(&bytes.Buffer{}, format.EmptyLog, testCommitTime)
		is.NotError(t, err)
		_, err = w.Close()
		is.NotError(t, err)
		_, err = w.Close()
		is.True(t, err != nil, "no error")
		is.True(t, w.DropTable("t") != nil, "no error from a change")
	})

	t.Run("should refuse a commit time that a header cannot hold", func(t *testing.T) {
		_, err := format.NewCommitWriter(&bytes.Buffer{}, format.EmptyLog, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))
		is.True(t, errors.Is(err, format.ErrMalformed), "error is not malformed: "+errorString(err))
	})

	refused := []struct {
		name  string
		write func(w *format.CommitWriter) error
	}{
		{name: "should refuse a create table without a name", write: func(w *format.CommitWriter) error {
			return w.CreateTable(createTable("", []string{"a"}, []uint32{0}).GetCreateTable())
		}},
		{name: "should refuse a create table without columns", write: func(w *format.CommitWriter) error {
			return w.CreateTable(createTable("t", nil, []uint32{0}).GetCreateTable())
		}},
		{name: "should refuse a create table with two columns of the same name", write: func(w *format.CommitWriter) error {
			return w.CreateTable(createTable("t", []string{"a", "A"}, []uint32{0}).GetCreateTable())
		}},
		{name: "should refuse a create table with an empty primary key", write: func(w *format.CommitWriter) error {
			return w.CreateTable(createTable("t", []string{"a"}, nil).GetCreateTable())
		}},
		{name: "should refuse a create table whose primary key points outside its columns", write: func(w *format.CommitWriter) error {
			return w.CreateTable(createTable("t", []string{"a"}, []uint32{1}).GetCreateTable())
		}},
		{name: "should refuse a create table whose primary key lists a column twice", write: func(w *format.CommitWriter) error {
			return w.CreateTable(createTable("t", []string{"a", "b"}, []uint32{0, 0}).GetCreateTable())
		}},
		{name: "should refuse a column of TYPE_UNSPECIFIED", write: func(w *format.CommitWriter) error {
			ct := createTable("t", []string{"a"}, []uint32{0}).GetCreateTable()
			ct.GetColumns()[0].SetType(s4qlpb.Type_TYPE_UNSPECIFIED)
			return w.CreateTable(ct)
		}},
		{name: "should refuse a column of a type it does not know", write: func(w *format.CommitWriter) error {
			ct := createTable("t", []string{"a"}, []uint32{0}).GetCreateTable()
			ct.GetColumns()[0].SetType(s4qlpb.Type(2))
			return w.CreateTable(ct)
		}},
		{name: "should refuse a create table with a field it does not know", write: func(w *format.CommitWriter) error {
			return w.CreateTable(withUnknown(createTable("t", []string{"a"}, []uint32{0}).GetCreateTable(), 9, protowire.VarintType))
		}},
		{name: "should refuse a create table whose name is not valid UTF-8", write: func(w *format.CommitWriter) error {
			return w.CreateTable(createTable("\xff", []string{"a"}, []uint32{0}).GetCreateTable())
		}},
		{name: "should refuse a drop table without a name", write: func(w *format.CommitWriter) error {
			return w.DropTable("")
		}},
		{name: "should refuse a drop table whose name holds NUL", write: func(w *format.CommitWriter) error {
			return w.DropTable("a\x00")
		}},
		{name: "should refuse an upsert without a table name", write: func(w *format.CommitWriter) error {
			return w.UpsertRow("", []uint32{0}, []*s4qlpb.Value{integer(1)})
		}},
		{name: "should refuse an upsert whose table name is not valid UTF-8", write: func(w *format.CommitWriter) error {
			return w.UpsertRow("\xff", []uint32{0}, []*s4qlpb.Value{integer(1)})
		}},
		{name: "should refuse an upsert without columns", write: func(w *format.CommitWriter) error {
			return w.UpsertRow("t", nil, nil)
		}},
		{name: "should refuse an upsert that lists a column twice", write: func(w *format.CommitWriter) error {
			return w.UpsertRow("t", []uint32{0, 0}, []*s4qlpb.Value{integer(1), integer(1)})
		}},
		{name: "should refuse an upsert with fewer values than columns", write: func(w *format.CommitWriter) error {
			return w.UpsertRow("t", []uint32{0, 1}, []*s4qlpb.Value{integer(1)})
		}},
		{name: "should refuse an upsert with a value without a kind", write: func(w *format.CommitWriter) error {
			return w.UpsertRow("t", []uint32{0}, []*s4qlpb.Value{{}})
		}},
		{name: "should refuse an upsert with a value of a kind it does not know", write: func(w *format.CommitWriter) error {
			return w.UpsertRow("t", []uint32{0}, []*s4qlpb.Value{withUnknown(&s4qlpb.Value{}, 6, protowire.VarintType)})
		}},
		{name: "should refuse an upsert with a real that is NaN", write: func(w *format.CommitWriter) error {
			return w.UpsertRow("t", []uint32{0}, []*s4qlpb.Value{realValue(math.NaN())})
		}},
		{name: "should refuse an upsert with text that is not valid UTF-8", write: func(w *format.CommitWriter) error {
			return w.UpsertRow("t", []uint32{0}, []*s4qlpb.Value{text("\xff")})
		}},
		{name: "should refuse a delete without a table name", write: func(w *format.CommitWriter) error {
			return w.DeleteRow("", []*s4qlpb.Value{integer(1)})
		}},
		{name: "should refuse a delete without values", write: func(w *format.CommitWriter) error {
			return w.DeleteRow("t", nil)
		}},
		{name: "should refuse a delete with more values than the key before it in the batch", write: func(w *format.CommitWriter) error {
			if err := w.DeleteRow("t", []*s4qlpb.Value{integer(1)}); err != nil {
				return err
			}
			return w.DeleteRow("t", []*s4qlpb.Value{integer(1), integer(2)})
		}},
		{name: "should refuse a delete with a value without a kind", write: func(w *format.CommitWriter) error {
			return w.DeleteRow("t", []*s4qlpb.Value{{}})
		}},
		{name: "should refuse a delete with a value with a field it does not know", write: func(w *format.CommitWriter) error {
			return w.DeleteRow("t", []*s4qlpb.Value{withUnknown(integer(1), 7, protowire.VarintType)})
		}},
		{name: "should refuse an annotation without a kind", write: func(w *format.CommitWriter) error {
			return w.Annotate(&s4qlpb.Annotation{})
		}},
		{name: "should refuse an annotation of a kind it does not know", write: func(w *format.CommitWriter) error {
			return w.Annotate(withUnknown(&s4qlpb.Annotation{}, 2, protowire.BytesType))
		}},
		{name: "should refuse a statement with a field it does not know", write: func(w *format.CommitWriter) error {
			a := statement("select 1")
			withUnknown(a.GetStatement(), 3, protowire.VarintType)
			return w.Annotate(a)
		}},
		{name: "should refuse a statement whose SQL is not valid UTF-8", write: func(w *format.CommitWriter) error {
			return w.Annotate(statement("select '\xff'"))
		}},
		{name: "should refuse a statement with a parameter without a kind", write: func(w *format.CommitWriter) error {
			return w.Annotate(statement("select ?", &s4qlpb.Value{}))
		}},
		{name: "should refuse a statement with a parameter that is NaN", write: func(w *format.CommitWriter) error {
			return w.Annotate(statement("select ?", realValue(math.NaN())))
		}},
	}
	for _, test := range refused {
		t.Run(test.name, func(t *testing.T) {
			var b bytes.Buffer
			w, err := format.NewCommitWriter(&b, format.EmptyLog, testCommitTime)
			is.NotError(t, err)
			written := b.Len()

			err = test.write(w)
			is.True(t, errors.Is(err, format.ErrMalformed) || errors.Is(err, format.ErrNewerVersion), "error is not a refusal: "+errorString(err))

			is.Equal(t, err, w.DropTable("u"))
			_, closeErr := w.Close()
			is.Equal(t, err, closeErr)
			is.Equal(t, written, b.Len())
		})
	}
}

// writeExample commit object to w, with every kind of change and two annotations, and return its
// name.
func writeExample(t *testing.T, w interface{ Write([]byte) (int, error) }) format.Name {
	t.Helper()
	cw, err := format.NewCommitWriter(w, format.Name(sha256.Sum256([]byte("parent"))), testCommitTime)
	is.NotError(t, err)
	is.NotError(t, cw.CreateTable(exampleCreateTable()))
	is.NotError(t, cw.UpsertRow("t", []uint32{0, 1}, []*s4qlpb.Value{integer(1), text("a")}))
	is.NotError(t, cw.UpsertRow("t", []uint32{0, 1}, []*s4qlpb.Value{integer(2), null()}))
	is.NotError(t, cw.UpsertRow("t", []uint32{1, 0}, []*s4qlpb.Value{blob([]byte{1}), integer(3)}))
	is.NotError(t, cw.DeleteRow("t", []*s4qlpb.Value{integer(1)}))
	is.NotError(t, cw.DeleteRow("t", []*s4qlpb.Value{integer(2)}))
	is.NotError(t, cw.UpsertRow("t", []uint32{0}, []*s4qlpb.Value{integer(4)}))
	is.NotError(t, cw.DropTable("t"))
	is.NotError(t, cw.Annotate(statement("insert into t values (?, ?)", integer(1), text("a"))))
	is.NotError(t, cw.Annotate(statement("drop table t")))
	name, err := cw.Close()
	is.NotError(t, err)
	return name
}

func exampleCreateTable() *s4qlpb.CreateTable {
	return createTable("t", []string{"id", "v"}, []uint32{0}).GetCreateTable()
}

// countingWriter counts the calls of Write.
type countingWriter struct {
	writes int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	return len(p), nil
}

// failingWriter fails with err after failAfter calls of Write.
type failingWriter struct {
	failAfter int
	err       error
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.failAfter == 0 {
		return 0, w.err
	}
	w.failAfter--
	return len(p), nil
}
