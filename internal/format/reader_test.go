package format_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
	"maragu.dev/is"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
)

func TestCommitReader(t *testing.T) {
	t.Run("should read the header, then the changes and annotations in order, then the end", func(t *testing.T) {
		b := join(
			record(1, testHeader()),
			record(2, createTable("t", []string{"a"}, []uint32{0})),
			record(3, statement("insert into t values (1)", integer(1))),
			record(2, upsertRows("t", []uint32{0}, []*s4qlpb.Value{integer(1)})),
			record(3, statement("delete from t")),
			record(2, deleteRows("t", []*s4qlpb.Value{integer(1)})),
			record(2, dropTable("t")),
		)

		h, items, err := readAll(b, nameOf(b))
		is.NotError(t, err)
		is.True(t, proto.Equal(testHeader(), h), "header differs")
		is.Equal(t, 6, len(items))
		is.Equal(t, s4qlpb.Change_CreateTable_case, items[0].change.WhichKind())
		is.Equal(t, "insert into t values (1)", items[1].annotation.GetStatement().GetSql())
		is.Equal(t, s4qlpb.Change_UpsertRows_case, items[2].change.WhichKind())
		is.Equal(t, "delete from t", items[3].annotation.GetStatement().GetSql())
		is.Equal(t, s4qlpb.Change_DeleteRows_case, items[4].change.WhichKind())
		is.Equal(t, s4qlpb.Change_DropTable_case, items[5].change.WhichKind())
	})

	t.Run("should read a commit object with only a header", func(t *testing.T) {
		b := record(1, testHeader())
		h, items, err := readAll(b, nameOf(b))
		is.NotError(t, err)
		is.NotNil(t, h)
		is.Equal(t, 0, len(items))
	})

	t.Run("should return the header from Header before Next, and the same error after the end", func(t *testing.T) {
		b := record(1, testHeader())
		r := format.NewCommitReader(bytes.NewReader(b), nameOf(b))
		h, err := r.Header()
		is.NotError(t, err)
		is.True(t, proto.Equal(testHeader(), h), "header differs")
		h2, err := r.Header()
		is.NotError(t, err)
		is.True(t, h == h2, "header is read twice")

		_, _, err = r.Next()
		is.True(t, errors.Is(err, io.EOF), "not the end")
		_, _, err = r.Next()
		is.True(t, errors.Is(err, io.EOF), "not the end the second time")
	})

	t.Run("should read a record that is larger than the read buffer", func(t *testing.T) {
		large := strings.Repeat("x", 3<<20)
		b := join(record(1, testHeader()), record(2, upsertRows("t", []uint32{0}, []*s4qlpb.Value{text(large)})))
		_, items, err := readAll(b, nameOf(b))
		is.NotError(t, err)
		is.Equal(t, large, items[0].change.GetUpsertRows().GetRows()[0].GetValues()[0].GetText())
	})

	t.Run("should read empty text, an empty blob, zero, negative zero, and infinities as values", func(t *testing.T) {
		values := []*s4qlpb.Value{text(""), blob(nil), integer(0), realValue(0), realValue(math.Copysign(0, -1)), realValue(math.Inf(1)), realValue(math.Inf(-1)), integer(math.MinInt64), null()}
		columns := make([]uint32, len(values))
		for i := range columns {
			columns[i] = uint32(i)
		}
		b := join(record(1, testHeader()), record(2, upsertRows("t", columns, values)))
		_, items, err := readAll(b, nameOf(b))
		is.NotError(t, err)
		got := items[0].change.GetUpsertRows().GetRows()[0].GetValues()
		for i := range values {
			is.True(t, proto.Equal(values[i], got[i]), values[i].String())
		}
		is.True(t, math.Signbit(got[4].GetReal()), "negative zero lost its sign")
	})

	t.Run("should report a mismatch with the name at the end, after handing out what it read", func(t *testing.T) {
		b := join(record(1, testHeader()), record(2, dropTable("t")))
		r := format.NewCommitReader(bytes.NewReader(b), format.Name(sha256.Sum256([]byte("other"))))
		_, err := r.Header()
		is.NotError(t, err)
		c, _, err := r.Next()
		is.NotError(t, err)
		is.NotNil(t, c)
		_, _, err = r.Next()
		is.True(t, errors.Is(err, format.ErrNameMismatch), "error is not a mismatch: "+errorString(err))
		_, _, err = r.Next()
		is.True(t, errors.Is(err, format.ErrNameMismatch), "error is not a mismatch the second time: "+errorString(err))
	})

	t.Run("should report a mismatch with the name, not the version, for a newer commit object that does not match its name", func(t *testing.T) {
		b := join(record(1, header(1, format.EmptyLog[:], time.Unix(0, 0))), record(2, dropTable("t")))
		_, _, err := readAll(b, format.Name(sha256.Sum256([]byte("other"))))
		is.True(t, errors.Is(err, format.ErrNameMismatch), "error is not a mismatch: "+errorString(err))
		is.True(t, !errors.Is(err, format.ErrNewerVersion), "error is newer version")
	})

	t.Run("should report a mismatch with the name, not the refusal, for a corrupt commit object", func(t *testing.T) {
		b := join(record(1, testHeader()), record(2, dropTable("t")))
		name := nameOf(b)
		corrupt := bytes.Clone(b)
		corrupt[len(corrupt)-1] = 0xff
		_, _, err := readAll(corrupt, name)
		is.True(t, errors.Is(err, format.ErrNameMismatch), "error is not a mismatch: "+errorString(err))
		is.True(t, !errors.Is(err, format.ErrMalformed), "error is malformed")
	})

	t.Run("should report a mismatch with the name for a commit object that was cut short", func(t *testing.T) {
		b := join(record(1, testHeader()), record(2, dropTable("t")))
		_, _, err := readAll(b[:len(b)-2], nameOf(b))
		is.True(t, errors.Is(err, format.ErrNameMismatch), "error is not a mismatch: "+errorString(err))
	})

	t.Run("should read the rest of the commit object after a refusal before it compares the name", func(t *testing.T) {
		b := join(record(2, dropTable("t")), record(1, testHeader()), bytes.Repeat([]byte{0xff}, 100<<10))
		src := bytes.NewReader(b)
		_, _, err := readAll2(src, nameOf(b))
		is.True(t, errors.Is(err, format.ErrMalformed), "error is not malformed: "+errorString(err))
		is.Equal(t, 0, src.Len())
	})

	t.Run("should return an error of the source as it is, and not as one of the three cases", func(t *testing.T) {
		b := join(record(1, testHeader()), record(2, dropTable("t")))
		failure := errors.New("connection reset")
		src := io.MultiReader(bytes.NewReader(b[:len(b)-2]), &errorReader{err: failure})
		_, _, err := readAll2(src, nameOf(b))
		is.True(t, errors.Is(err, failure), "error is not the source's: "+errorString(err))
		is.True(t, !errors.Is(err, format.ErrNameMismatch) && !errors.Is(err, format.ErrMalformed) && !errors.Is(err, format.ErrNewerVersion), "error is one of the three")
	})

	t.Run("should return an error of the source that fails once and then ends", func(t *testing.T) {
		b := join(record(1, testHeader()), record(2, dropTable("t")))
		failure := errors.New("connection reset")
		src := io.MultiReader(bytes.NewReader(b[:len(b)-2]), &errorReader{err: failure, once: true})
		_, _, err := readAll2(src, nameOf(b))
		is.True(t, errors.Is(err, failure), "error is not the source's: "+errorString(err))
		is.True(t, !errors.Is(err, format.ErrNameMismatch) && !errors.Is(err, format.ErrMalformed) && !errors.Is(err, format.ErrNewerVersion), "error is one of the three")
	})

	t.Run("should return an error of the source while it reads the rest after a refusal", func(t *testing.T) {
		b := join(record(2, dropTable("t")), record(1, testHeader()))
		failure := errors.New("connection reset")
		src := io.MultiReader(bytes.NewReader(b), &errorReader{err: failure})
		_, _, err := readAll2(src, nameOf(b))
		is.True(t, errors.Is(err, failure), "error is not the source's: "+errorString(err))
		is.True(t, !errors.Is(err, format.ErrNameMismatch) && !errors.Is(err, format.ErrMalformed) && !errors.Is(err, format.ErrNewerVersion), "error is one of the three")
	})

	t.Run("should skip an annotation of a kind it does not know", func(t *testing.T) {
		b := join(
			record(1, testHeader()),
			record(3, withUnknown(&s4qlpb.Annotation{}, 2, protowire.BytesType)),
			record(3, statement("select 1")),
		)
		_, items, err := readAll(b, nameOf(b))
		is.NotError(t, err)
		is.Equal(t, 1, len(items))
		is.Equal(t, "select 1", items[0].annotation.GetStatement().GetSql())
	})

	t.Run("should skip an empty annotation", func(t *testing.T) {
		b := join(record(1, testHeader()), record(3, &s4qlpb.Annotation{}))
		_, items, err := readAll(b, nameOf(b))
		is.NotError(t, err)
		is.Equal(t, 0, len(items))
	})

	t.Run("should hand out an annotation with fields it does not know, at any depth", func(t *testing.T) {
		a := statement("select ?", withUnknown(&s4qlpb.Value{}, 6, protowire.VarintType), integer(1))
		withUnknown(a, 9, protowire.Fixed64Type)
		withUnknown(a.GetStatement(), 3, protowire.VarintType)
		withUnknown(a.GetStatement().GetParameters()[1], 7, protowire.BytesType)
		b := join(record(1, testHeader()), record(3, a))
		_, items, err := readAll(b, nameOf(b))
		is.NotError(t, err)
		is.Equal(t, 1, len(items))
		is.True(t, proto.Equal(a, items[0].annotation), "annotation differs")
	})

	t.Run("should hand out a statement with a NaN parameter", func(t *testing.T) {
		b := join(record(1, testHeader()), record(3, statement("select ?", realValue(math.NaN()))))
		_, items, err := readAll(b, nameOf(b))
		is.NotError(t, err)
		is.Equal(t, 1, len(items))
	})

	newer := []struct {
		name string
		b    []byte
	}{
		{name: "should refuse version 1 as newer", b: join(record(1, header(1, format.EmptyLog[:], time.Unix(0, 0))))},
		{name: "should refuse a field it does not know in the commit object as newer", b: join(record(1, testHeader()), record(4, &s4qlpb.Null{}))},
		{name: "should refuse a field it does not know in the commit object as newer, also before the header", b: join(protowire.AppendVarint(protowire.AppendTag(nil, 5, protowire.VarintType), 1), record(1, testHeader()))},
		{name: "should refuse a field it does not know in the header as newer", b: join(record(1, withUnknown(testHeader(), 4, protowire.VarintType)))},
		{name: "should refuse a field it does not know in the commit time as newer", b: join(record(1, func() *s4qlpb.Header {
			h := testHeader()
			withUnknown(h.GetCommitTime(), 3, protowire.VarintType)
			return h
		}()))},
		{name: "should refuse a change of a kind it does not know as newer", b: changeRecord(withUnknown(&s4qlpb.Change{}, 5, protowire.BytesType))},
		{name: "should refuse a change with a known kind and a field it does not know as newer", b: changeRecord(withUnknown(dropTable("t"), 6, protowire.VarintType))},
		{name: "should refuse a field it does not know in create table as newer", b: changeRecord(func() *s4qlpb.Change {
			c := createTable("t", []string{"a"}, []uint32{0})
			withUnknown(c.GetCreateTable(), 4, protowire.VarintType)
			return c
		}())},
		{name: "should refuse a field it does not know in a column as newer", b: changeRecord(func() *s4qlpb.Change {
			c := createTable("t", []string{"a", "b"}, []uint32{0})
			withUnknown(c.GetCreateTable().GetColumns()[1], 3, protowire.VarintType)
			return c
		}())},
		{name: "should refuse a type it does not know as newer", b: changeRecord(func() *s4qlpb.Change {
			c := createTable("t", []string{"a"}, []uint32{0})
			c.GetCreateTable().GetColumns()[0].SetType(s4qlpb.Type(2))
			return c
		}())},
		{name: "should refuse a field it does not know in drop table as newer", b: changeRecord(func() *s4qlpb.Change {
			c := dropTable("t")
			withUnknown(c.GetDropTable(), 2, protowire.VarintType)
			return c
		}())},
		{name: "should refuse a field it does not know in upsert rows as newer", b: changeRecord(func() *s4qlpb.Change {
			c := upsertRows("t", []uint32{0}, []*s4qlpb.Value{integer(1)})
			withUnknown(c.GetUpsertRows(), 4, protowire.VarintType)
			return c
		}())},
		{name: "should refuse a field it does not know in a row as newer", b: changeRecord(func() *s4qlpb.Change {
			c := upsertRows("t", []uint32{0}, []*s4qlpb.Value{integer(1)})
			withUnknown(c.GetUpsertRows().GetRows()[0], 2, protowire.VarintType)
			return c
		}())},
		{name: "should refuse a value of a kind it does not know as newer", b: changeRecord(upsertRows("t", []uint32{0, 1}, []*s4qlpb.Value{integer(1), withUnknown(&s4qlpb.Value{}, 6, protowire.BytesType)}))},
		{name: "should refuse a value with a known kind and a field it does not know as newer", b: changeRecord(upsertRows("t", []uint32{0}, []*s4qlpb.Value{withUnknown(integer(1), 7, protowire.VarintType)}))},
		{name: "should refuse a null with a field it does not know as newer", b: changeRecord(upsertRows("t", []uint32{0}, []*s4qlpb.Value{func() *s4qlpb.Value {
			v := null()
			withUnknown(v.GetNull(), 1, protowire.VarintType)
			return v
		}()}))},
		{name: "should refuse a field it does not know in delete rows as newer", b: changeRecord(func() *s4qlpb.Change {
			c := deleteRows("t", []*s4qlpb.Value{integer(1)})
			withUnknown(c.GetDeleteRows(), 3, protowire.VarintType)
			return c
		}())},
		{name: "should refuse a field it does not know in a primary key as newer", b: changeRecord(func() *s4qlpb.Change {
			c := deleteRows("t", []*s4qlpb.Value{integer(1)})
			withUnknown(c.GetDeleteRows().GetPrimaryKeys()[0], 2, protowire.VarintType)
			return c
		}())},
	}
	for _, test := range newer {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := readAll(test.b, nameOf(test.b))
			is.True(t, errors.Is(err, format.ErrNewerVersion), "error is not newer version: "+errorString(err))
			is.True(t, !errors.Is(err, format.ErrMalformed), "error is malformed")
			is.True(t, !errors.Is(err, format.ErrNameMismatch), "error is a mismatch")
		})
	}

	malformed := []struct {
		name string
		b    []byte
	}{
		{name: "should refuse no bytes", b: nil},
		{name: "should refuse a change before the header", b: join(record(2, dropTable("t")), record(1, testHeader()))},
		{name: "should refuse an annotation before the header", b: join(record(3, statement("select 1")), record(1, testHeader()))},
		{name: "should refuse only a change", b: join(record(2, dropTable("t")))},
		{name: "should refuse a header given twice", b: join(record(1, testHeader()), record(1, testHeader()))},
		{name: "should refuse a header given twice after a change", b: join(record(1, testHeader()), record(2, dropTable("t")), record(1, testHeader()))},
		{name: "should refuse a header without a version", b: join(record(1, func() *s4qlpb.Header {
			h := testHeader()
			h.ClearVersion()
			return h
		}()))},
		{name: "should refuse a header without a parent", b: join(record(1, header(0, nil, time.Unix(0, 0))))},
		{name: "should refuse a parent of 31 bytes", b: join(record(1, header(0, make([]byte, 31), time.Unix(0, 0))))},
		{name: "should refuse a parent of 33 bytes", b: join(record(1, header(0, make([]byte, 33), time.Unix(0, 0))))},
		{name: "should refuse a header without a commit time", b: join(record(1, func() *s4qlpb.Header {
			h := testHeader()
			h.ClearCommitTime()
			return h
		}()))},
		{name: "should refuse a commit time that is not valid", b: join(record(1, func() *s4qlpb.Header {
			h := testHeader()
			h.SetCommitTime(&timestamppb.Timestamp{Seconds: 1, Nanos: 1_000_000_000})
			return h
		}()))},
		{name: "should refuse a header that is not Protocol Buffers", b: join(protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), []byte{0xff}))},
		{name: "should refuse a change record with the wire type of a fixed 32-bit number", b: func() []byte {
			b := record(2, dropTable("t"))
			b[0] = byte(protowire.EncodeTag(2, protowire.Fixed32Type))
			return join(record(1, testHeader()), b)
		}()},
		{name: "should refuse a record that is not a message", b: join(record(1, testHeader()), protowire.AppendVarint(protowire.AppendTag(nil, 2, protowire.VarintType), 1))},
		{name: "should refuse a tag with an invalid wire type", b: join(record(1, testHeader()), []byte{0x17})},
		{name: "should refuse a tag with field number 0", b: join(record(1, testHeader()), []byte{0x02, 0x00})},
		{name: "should refuse a tag that is too long", b: join(record(1, testHeader()), bytes.Repeat([]byte{0xff}, 11))},
		{name: "should refuse a record that is cut short", b: func() []byte {
			b := join(record(1, testHeader()), record(2, dropTable("t")))
			return b[:len(b)-2]
		}()},
		{name: "should refuse a header that is cut short", b: record(1, testHeader())[:20]},
		{name: "should refuse a length that is cut short", b: join(record(1, testHeader()), []byte{0x12, 0x80})},
		{name: "should refuse a record above the limit", b: join(record(1, testHeader()), protowire.AppendVarint([]byte{0x12}, format.MaxRecordSize+1))},
		{name: "should refuse a record whose length is far more than what follows", b: join(record(1, testHeader()), protowire.AppendVarint([]byte{0x12}, format.MaxRecordSize), []byte{1, 2, 3})},
		{name: "should refuse an empty change", b: changeRecord(&s4qlpb.Change{})},
		{name: "should refuse a change that is not Protocol Buffers", b: join(record(1, testHeader()), protowire.AppendBytes([]byte{0x12}, []byte{0x0a, 0x05}))},
		{name: "should refuse a create table without a name", b: changeRecord(createTable("", []string{"a"}, []uint32{0}))},
		{name: "should refuse a create table whose name holds NUL", b: changeRecord(createTable("t\x00u", []string{"a"}, []uint32{0}))},
		{name: "should refuse a create table without columns", b: changeRecord(createTable("t", nil, []uint32{0}))},
		{name: "should refuse a column without a name", b: changeRecord(createTable("t", []string{"a", ""}, []uint32{0}))},
		{name: "should refuse a column whose name holds NUL", b: changeRecord(createTable("t", []string{"a\x00"}, []uint32{0}))},
		{name: "should refuse two columns with the same name", b: changeRecord(createTable("t", []string{"a", "b", "a"}, []uint32{0}))},
		{name: "should refuse two columns whose names differ only in the case of ASCII letters", b: changeRecord(createTable("t", []string{"Ab", "aB"}, []uint32{0}))},
		{name: "should refuse a column without a type", b: changeRecord(func() *s4qlpb.Change {
			c := createTable("t", []string{"a"}, []uint32{0})
			c.GetCreateTable().GetColumns()[0].ClearType()
			return c
		}())},
		{name: "should refuse a column of TYPE_UNSPECIFIED", b: changeRecord(func() *s4qlpb.Change {
			c := createTable("t", []string{"a"}, []uint32{0})
			c.GetCreateTable().GetColumns()[0].SetType(s4qlpb.Type_TYPE_UNSPECIFIED)
			return c
		}())},
		{name: "should refuse an empty primary key", b: changeRecord(createTable("t", []string{"a"}, nil))},
		{name: "should refuse a primary key that lists a column twice", b: changeRecord(createTable("t", []string{"a", "b"}, []uint32{1, 0, 1}))},
		{name: "should refuse a primary key that points outside the columns", b: changeRecord(createTable("t", []string{"a", "b"}, []uint32{2}))},
		{name: "should refuse a drop table without a name", b: changeRecord(dropTable(""))},
		{name: "should refuse a drop table whose name is not valid UTF-8", b: changeRecord(rawChange(2, protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), []byte("\xff"))))},
		{name: "should refuse an upsert rows without a table name", b: changeRecord(upsertRows("", []uint32{0}, []*s4qlpb.Value{integer(1)}))},
		{name: "should refuse an upsert rows without columns", b: changeRecord(upsertRows("t", nil, []*s4qlpb.Value{}))},
		{name: "should refuse an upsert rows that lists a column twice", b: changeRecord(upsertRows("t", []uint32{0, 1, 0}, []*s4qlpb.Value{integer(1), integer(2), integer(3)}))},
		{name: "should refuse an upsert rows without rows", b: changeRecord(s4qlpb.Change_builder{UpsertRows: s4qlpb.UpsertRows_builder{Table: new("t"), Columns: []uint32{0}}.Build()}.Build())},
		{name: "should refuse a row with fewer values than columns", b: changeRecord(upsertRows("t", []uint32{0, 1}, []*s4qlpb.Value{integer(1)}, []*s4qlpb.Value{integer(1), integer(2)}))},
		{name: "should refuse a row with more values than columns", b: changeRecord(upsertRows("t", []uint32{0, 1}, []*s4qlpb.Value{integer(1), integer(2)}, []*s4qlpb.Value{integer(1), integer(2), integer(3)}))},
		{name: "should refuse a value without a kind in a row", b: changeRecord(upsertRows("t", []uint32{0}, []*s4qlpb.Value{{}}))},
		{name: "should refuse a real that is NaN in a row", b: changeRecord(upsertRows("t", []uint32{0}, []*s4qlpb.Value{realValue(math.NaN())}))},
		{name: "should refuse text that is not valid UTF-8 in a row", b: changeRecord(rawChange(3, join(
			protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), []byte("t")),
			protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), []byte{0}),
			protowire.AppendBytes(protowire.AppendTag(nil, 3, protowire.BytesType),
				protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType),
					protowire.AppendBytes(protowire.AppendTag(nil, 3, protowire.BytesType), []byte("\xc0\x80")))),
		)))},
		{name: "should refuse a delete rows without a table name", b: changeRecord(deleteRows("", []*s4qlpb.Value{integer(1)}))},
		{name: "should refuse a delete rows without keys", b: changeRecord(deleteRows("t"))},
		{name: "should refuse a primary key without values", b: changeRecord(deleteRows("t", []*s4qlpb.Value{}))},
		{name: "should refuse primary keys with different numbers of values", b: changeRecord(deleteRows("t", []*s4qlpb.Value{integer(1)}, []*s4qlpb.Value{integer(1), integer(2)}))},
		{name: "should refuse a value without a kind in a primary key", b: changeRecord(deleteRows("t", []*s4qlpb.Value{{}}))},
		{name: "should refuse a real that is NaN in a primary key", b: changeRecord(deleteRows("t", []*s4qlpb.Value{realValue(math.NaN())}))},
		{name: "should refuse an annotation that is not Protocol Buffers", b: join(record(1, testHeader()), protowire.AppendBytes([]byte{0x1a}, []byte{0x0a, 0x05}))},
		{name: "should refuse SQL text that is not valid UTF-8", b: join(record(1, testHeader()), protowire.AppendBytes([]byte{0x1a},
			protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType),
				protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), []byte("\xff")))))},
	}
	for _, test := range malformed {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := readAll(test.b, nameOf(test.b))
			is.True(t, errors.Is(err, format.ErrMalformed), "error is not malformed: "+errorString(err))
			is.True(t, !errors.Is(err, format.ErrNewerVersion), "error is newer version")
			is.True(t, !errors.Is(err, format.ErrNameMismatch), "error is a mismatch")
		})
	}
}

// item that [format.CommitReader.Next] returned.
type item struct {
	change     *s4qlpb.Change
	annotation *s4qlpb.Annotation
}

// readAll of the commit object in b with the given name.
func readAll(b []byte, name format.Name) (*s4qlpb.Header, []item, error) {
	return readAll2(bytes.NewReader(b), name)
}

// readAll2 of the commit object from r with the given name.
func readAll2(r io.Reader, name format.Name) (*s4qlpb.Header, []item, error) {
	cr := format.NewCommitReader(r, name)
	h, err := cr.Header()
	if err != nil {
		return nil, nil, err
	}
	var items []item
	for {
		c, a, err := cr.Next()
		if errors.Is(err, io.EOF) {
			return h, items, nil
		}
		if err != nil {
			return nil, nil, err
		}
		if (c == nil) == (a == nil) {
			panic("not exactly one of change and annotation")
		}
		items = append(items, item{change: c, annotation: a})
	}
}

func FuzzCommitReader(f *testing.F) {
	for _, sample := range commitSamples() {
		b, err := os.ReadFile(filepath.Join("testdata", sample.file))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte{})
	f.Add(changeRecord(withUnknown(&s4qlpb.Change{}, 5, protowire.BytesType)))
	f.Add(join(record(1, testHeader()), record(3, withUnknown(&s4qlpb.Annotation{}, 2, protowire.BytesType))))
	f.Add(changeRecord(upsertRows("t", []uint32{0, 1}, []*s4qlpb.Value{integer(1)})))

	other := format.Name(sha256.Sum256([]byte("other")))

	f.Fuzz(func(t *testing.T, input []byte) {
		// With its own name, the commit object is read, or refused as newer or malformed.
		_, items, err := readAll(input, nameOf(input))
		switch {
		case err == nil:
			// What the reader accepts is one commit object message for any reader of Protocol
			// Buffers.
			var whole s4qlpb.CommitObject
			if err := proto.Unmarshal(input, &whole); err != nil {
				t.Fatalf("the reader accepts what proto.Unmarshal refuses: %v", err)
			}
			var changes int
			for _, item := range items {
				if item.change != nil {
					changes++
				}
			}
			if changes != len(whole.GetChanges()) {
				t.Fatalf("the reader read %v changes, and proto.Unmarshal %v", changes, len(whole.GetChanges()))
			}
		case errors.Is(err, format.ErrNameMismatch):
			t.Fatalf("mismatch with the right name: %v", err)
		case !errors.Is(err, format.ErrMalformed) && !errors.Is(err, format.ErrNewerVersion):
			t.Fatalf("error is neither malformed nor newer version: %v", err)
		}

		// With another name, it is always a mismatch, whatever else is wrong with it.
		_, _, err = readAll(input, other)
		if !errors.Is(err, format.ErrNameMismatch) {
			t.Fatalf("error with the wrong name is not a mismatch: %v", err)
		}
		if errors.Is(err, format.ErrMalformed) || errors.Is(err, format.ErrNewerVersion) {
			t.Fatalf("mismatch is also another case: %v", err)
		}
	})
}

func nameOf(b []byte) format.Name {
	return format.Name(sha256.Sum256(b))
}

func join(bs ...[]byte) []byte {
	return bytes.Join(bs, nil)
}

// record of a commit object: field of [s4qlpb.CommitObject] with the message m.
func record(field protowire.Number, m proto.Message) []byte {
	b, err := proto.Marshal(m)
	if err != nil {
		panic(err)
	}
	return protowire.AppendBytes(protowire.AppendTag(nil, field, protowire.BytesType), b)
}

// changeRecord of a commit object with a valid header and the change c.
func changeRecord(c *s4qlpb.Change) []byte {
	return join(record(1, testHeader()), record(2, c))
}

// rawChange with the bytes b as the message of the given field.
func rawChange(field protowire.Number, b []byte) *s4qlpb.Change {
	c := &s4qlpb.Change{}
	c.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, field, protowire.BytesType), b))
	return c
}

// withUnknown field in m, with a value of the given wire type, and return m.
func withUnknown[M proto.Message](m M, field protowire.Number, typ protowire.Type) M {
	var b []byte
	switch typ {
	case protowire.VarintType:
		b = protowire.AppendVarint(protowire.AppendTag(nil, field, typ), 1)
	case protowire.Fixed64Type:
		b = protowire.AppendFixed64(protowire.AppendTag(nil, field, typ), 1)
	case protowire.BytesType:
		b = protowire.AppendBytes(protowire.AppendTag(nil, field, typ), []byte{0x08, 0x01})
	}
	r := m.ProtoReflect()
	r.SetUnknown(append(r.GetUnknown(), b...))
	return m
}

func testHeader() *s4qlpb.Header {
	return header(0, format.EmptyLog[:], time.Date(2026, 11, 12, 12, 0, 0, 123_456_789, time.UTC))
}

func header(version uint32, parent []byte, commitTime time.Time) *s4qlpb.Header {
	return s4qlpb.Header_builder{Version: &version, Parent: parent, CommitTime: timestamppb.New(commitTime)}.Build()
}

func createTable(table string, columns []string, primaryKey []uint32) *s4qlpb.Change {
	var cs []*s4qlpb.Column
	for _, name := range columns {
		cs = append(cs, s4qlpb.Column_builder{Name: &name, Type: new(s4qlpb.Type_TYPE_INT)}.Build())
	}
	return s4qlpb.Change_builder{CreateTable: s4qlpb.CreateTable_builder{Table: &table, Columns: cs, PrimaryKey: primaryKey}.Build()}.Build()
}

func dropTable(table string) *s4qlpb.Change {
	return s4qlpb.Change_builder{DropTable: s4qlpb.DropTable_builder{Table: &table}.Build()}.Build()
}

func upsertRows(table string, columns []uint32, rows ...[]*s4qlpb.Value) *s4qlpb.Change {
	var rs []*s4qlpb.Row
	for _, values := range rows {
		rs = append(rs, s4qlpb.Row_builder{Values: values}.Build())
	}
	return s4qlpb.Change_builder{UpsertRows: s4qlpb.UpsertRows_builder{Table: &table, Columns: columns, Rows: rs}.Build()}.Build()
}

func deleteRows(table string, keys ...[]*s4qlpb.Value) *s4qlpb.Change {
	var ks []*s4qlpb.PrimaryKey
	for _, values := range keys {
		ks = append(ks, s4qlpb.PrimaryKey_builder{Values: values}.Build())
	}
	return s4qlpb.Change_builder{DeleteRows: s4qlpb.DeleteRows_builder{Table: &table, PrimaryKeys: ks}.Build()}.Build()
}

func statement(sql string, parameters ...*s4qlpb.Value) *s4qlpb.Annotation {
	return s4qlpb.Annotation_builder{Statement: s4qlpb.Statement_builder{Sql: &sql, Parameters: parameters}.Build()}.Build()
}

func integer(i int64) *s4qlpb.Value {
	return s4qlpb.Value_builder{Integer: &i}.Build()
}

func realValue(f float64) *s4qlpb.Value {
	return s4qlpb.Value_builder{Real: &f}.Build()
}

func text(s string) *s4qlpb.Value {
	return s4qlpb.Value_builder{Text: &s}.Build()
}

func blob(b []byte) *s4qlpb.Value {
	if b == nil {
		b = []byte{}
	}
	return s4qlpb.Value_builder{Blob: b}.Build()
}

func null() *s4qlpb.Value {
	return s4qlpb.Value_builder{Null: &s4qlpb.Null{}}.Build()
}

// errorReader fails every read with err, or only the first if once is set, and then ends.
type errorReader struct {
	err    error
	once   bool
	failed bool
}

func (r *errorReader) Read([]byte) (int, error) {
	if r.once && r.failed {
		return 0, io.EOF
	}
	r.failed = true
	return 0, r.err
}

// commitSample is a commit object of version 0 that is committed as a file in testdata.
type commitSample struct {
	file       string
	parent     format.Name
	commitTime time.Time
	// write the changes and annotations of the sample.
	write func(w *format.CommitWriter) error
	// want is what the reader reads from the sample, after its header.
	want []item
}

func commitSamples() []commitSample {
	return []commitSample{
		{
			file:       "commit-v0-first.bin",
			parent:     format.EmptyLog,
			commitTime: time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC),
			write: func(w *format.CommitWriter) error {
				return errors.Join(
					w.CreateTable(createTable("users", []string{"id", "age"}, []uint32{0}).GetCreateTable()),
					w.UpsertRow("users", []uint32{0, 1}, []*s4qlpb.Value{integer(1), integer(42)}),
					w.UpsertRow("users", []uint32{0, 1}, []*s4qlpb.Value{integer(2), null()}),
					w.Annotate(statement("create table users (id int primary key, age int) strict")),
					w.Annotate(statement("insert into users values (?, ?), (?, ?)", integer(1), integer(42), integer(2), null())),
				)
			},
			want: []item{
				{change: createTable("users", []string{"id", "age"}, []uint32{0})},
				{change: upsertRows("users", []uint32{0, 1}, []*s4qlpb.Value{integer(1), integer(42)}, []*s4qlpb.Value{integer(2), null()})},
				{annotation: statement("create table users (id int primary key, age int) strict")},
				{annotation: statement("insert into users values (?, ?), (?, ?)", integer(1), integer(42), integer(2), null())},
			},
		},
		{
			file:       "commit-v0-all.bin",
			parent:     format.Name(sha256.Sum256([]byte("commit-v0-first.bin"))),
			commitTime: time.Date(2026, 11, 12, 9, 31, 2, 123_456_789, time.UTC),
			write: func(w *format.CommitWriter) error {
				return errors.Join(
					w.CreateTable(createTable("kv", []string{"a", "b", "c"}, []uint32{1, 0}).GetCreateTable()),
					w.UpsertRow("kv", []uint32{0, 1, 2}, []*s4qlpb.Value{integer(-7), text("ü"), realValue(2.5)}),
					w.UpsertRow("kv", []uint32{0, 1, 2}, []*s4qlpb.Value{integer(8), text(""), blob([]byte{0, 0xff})}),
					w.UpsertRow("kv", []uint32{1, 0, 2}, []*s4qlpb.Value{text("x"), integer(9), null()}),
					w.UpsertRow("kv", []uint32{0, 1}, []*s4qlpb.Value{integer(-7), text("ü")}),
					w.DeleteRow("kv", []*s4qlpb.Value{text("x"), integer(9)}),
					w.DeleteRow("kv", []*s4qlpb.Value{text(""), integer(8)}),
					w.DropTable("users"),
					w.Annotate(statement("update kv set c = ? where a = ?", null(), integer(-7))),
					w.Annotate(statement("select ?, ?, ?, ?, ?", integer(math.MaxInt64), realValue(-0.5), text("t"), blob([]byte("b")), null())),
				)
			},
			want: []item{
				{change: createTable("kv", []string{"a", "b", "c"}, []uint32{1, 0})},
				{change: upsertRows("kv", []uint32{0, 1, 2},
					[]*s4qlpb.Value{integer(-7), text("ü"), realValue(2.5)},
					[]*s4qlpb.Value{integer(8), text(""), blob([]byte{0, 0xff})})},
				{change: upsertRows("kv", []uint32{1, 0, 2}, []*s4qlpb.Value{text("x"), integer(9), null()})},
				{change: upsertRows("kv", []uint32{0, 1}, []*s4qlpb.Value{integer(-7), text("ü")})},
				{change: deleteRows("kv", []*s4qlpb.Value{text("x"), integer(9)}, []*s4qlpb.Value{text(""), integer(8)})},
				{change: dropTable("users")},
				{annotation: statement("update kv set c = ? where a = ?", null(), integer(-7))},
				{annotation: statement("select ?, ?, ?, ?, ?", integer(math.MaxInt64), realValue(-0.5), text("t"), blob([]byte("b")), null())},
			},
		},
	}
}

// TestCommitSamples reads the sample commit objects in testdata, and checks that they are what the
// current code writes, since the format is a draft. Run it with -update to write them again.
func TestCommitSamples(t *testing.T) {
	for _, sample := range commitSamples() {
		t.Run("should read "+sample.file+" as written, and write it the same", func(t *testing.T) {
			var b bytes.Buffer
			w, err := format.NewCommitWriter(&b, sample.parent, sample.commitTime)
			is.NotError(t, err)
			is.NotError(t, sample.write(w))
			_, err = w.Close()
			is.NotError(t, err)

			path := filepath.Join("testdata", sample.file)
			if *update {
				is.NotError(t, os.WriteFile(path, b.Bytes(), 0o644))
			}
			stored, err := os.ReadFile(path)
			is.NotError(t, err)
			is.True(t, bytes.Equal(b.Bytes(), stored), "the writer writes other bytes than the sample file")

			h, items, err := readAll(stored, nameOf(stored))
			is.NotError(t, err)
			is.True(t, proto.Equal(header(0, sample.parent[:], sample.commitTime), h), "header differs")
			is.Equal(t, len(sample.want), len(items))
			for i := range sample.want {
				is.True(t, proto.Equal(sample.want[i].change, items[i].change), "change differs")
				is.True(t, proto.Equal(sample.want[i].annotation, items[i].annotation), "annotation differs")
			}
		})
	}

	t.Run("should cover every field of every message in the schema", func(t *testing.T) {
		seen := map[protoreflect.FullName]bool{}
		for _, sample := range commitSamples() {
			stored, err := os.ReadFile(filepath.Join("testdata", sample.file))
			is.NotError(t, err)
			var whole s4qlpb.CommitObject
			is.NotError(t, proto.Unmarshal(stored, &whole))
			markSet(whole.ProtoReflect(), seen)
		}

		messages := s4qlpb.File_s4ql_v1_commit_proto.Messages()
		for i := range messages.Len() {
			fields := messages.Get(i).Fields()
			for j := range fields.Len() {
				name := fields.Get(j).FullName()
				is.True(t, seen[name], "no sample holds "+string(name))
			}
		}
	})
}

// markSet in seen the full name of every field that is set in m, at any depth.
func markSet(m protoreflect.Message, seen map[protoreflect.FullName]bool) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		seen[fd.FullName()] = true
		if fd.Message() == nil {
			return true
		}
		if fd.IsList() {
			for i := range v.List().Len() {
				markSet(v.List().Get(i).Message(), seen)
			}
			return true
		}
		markSet(v.Message(), seen)
		return true
	})
}
