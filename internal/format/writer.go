package format

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"maragu.dev/s4ql/internal/s4qlpb"
)

// batchSize is the size in bytes above which [CommitWriter] starts a new batch of rows or keys. A
// batch is larger only if it holds a single row or key that is larger.
const batchSize = 1 << 20

// headroom is the room that a batch keeps before its bytes for the tag and length of its record and
// of its change, which are known only when it is written: two tags of at most five bytes, and two
// lengths of at most ten.
const headroom = 2 * (5 + binary.MaxVarintLen64)

// marshalOptions for everything that [CommitWriter] writes, so that the same commit object is always
// the same bytes with the same version of the Protocol Buffers library.
var marshalOptions = proto.MarshalOptions{Deterministic: true}

// CommitWriter of one commit object, which writes it as a stream: the header first, then the
// changes, then the annotations, so that it never holds the whole commit object in memory. The
// bytes it writes are a valid encoding of one [s4qlpb.CommitObject], the same bytes as marshalling
// the whole message deterministically, and it computes the name of the commit object as it writes.
//
// The caller hands it rows to upsert and primary keys to delete one at a time, and the writer
// groups them into batches: consecutive rows of the same table and the same columns into one
// [s4qlpb.UpsertRows], and consecutive keys of the same table into one [s4qlpb.DeleteRows], of up to
// about one MiB each. The order of the changes is kept. A batch is written when the next change
// does not belong to it, when it is full, and on [CommitWriter.Annotate] and [CommitWriter.Close].
//
// It refuses what [CommitReader] refuses, so it never writes a commit object that s4ql cannot read.
// It refuses a change after an annotation, so the bytes are those of the whole message. After an
// error, nothing more is written, every call returns the same error, and what was written is not a
// valid commit object.
//
// It makes one call of Write on the underlying writer for each change and annotation, and one for
// the header.
type CommitWriter struct {
	w    io.Writer
	hash hash.Hash
	err  error

	// maxRecordSize is [MaxRecordSize], and smaller in tests.
	maxRecordSize int

	// annotating is true after the first annotation, after which no change can follow.
	annotating bool

	// batch is the batch of rows or keys that is not written yet.
	batch batch
}

// batch of rows to upsert or of keys to delete, which [CommitWriter] has not written yet.
type batch struct {
	// field of [s4qlpb.Change] that the batch is, or 0 if there is no batch.
	field   protowire.Number
	table   string
	columns []uint32
	// keySize is the number of values in each key of a batch of keys.
	keySize int
	// prefix is the length of the marshalled [s4qlpb.UpsertRows] or [s4qlpb.DeleteRows] without its
	// rows or keys.
	prefix int
	// buf is the headroom, then the marshalled message without its rows or keys, then the encoded
	// rows or keys, each with its tag and length, in the order they came. The rows or keys are the
	// last field in number, so this is the same bytes as marshalling the whole message, because
	// marshalling writes the fields in the order of their numbers.
	buf []byte
}

// newBatch of the given field of [s4qlpb.Change], for m, the message without its rows or keys.
func newBatch(field protowire.Number, m proto.Message) (batch, error) {
	buf, err := marshalOptions.MarshalAppend(make([]byte, headroom, headroom+256), m)
	if err != nil {
		return batch{}, err
	}
	return batch{field: field, prefix: len(buf) - headroom, buf: buf}, nil
}

// rowsSize of the encoded rows or keys in the batch.
func (b batch) rowsSize() int {
	return len(b.buf) - headroom - b.prefix
}

// changeSize of the batch as a [s4qlpb.Change], with extra bytes of rows or keys added.
func (b batch) changeSize(extra int) int {
	return protowire.SizeTag(b.field) + protowire.SizeBytes(len(b.buf)-headroom+extra)
}

// NewCommitWriter that writes a commit object to w, and writes its header first: version 0, the name
// of the parent, and the commit time.
func NewCommitWriter(w io.Writer, parent Name, commitTime time.Time) (*CommitWriter, error) {
	cw := &CommitWriter{w: w, hash: sha256.New(), maxRecordSize: MaxRecordSize}

	h := s4qlpb.Header_builder{
		Version:    new(uint32(commitVersion)),
		Parent:     parent[:],
		CommitTime: timestamppb.New(commitTime),
	}.Build()
	if err := checkHeader(h); err != nil {
		return nil, err
	}
	if err := cw.writeRecord(headerField, h); err != nil {
		return nil, err
	}
	return cw, nil
}

// CreateTable as the next change.
func (w *CommitWriter) CreateTable(ct *s4qlpb.CreateTable) error {
	return w.change(s4qlpb.Change_builder{CreateTable: ct}.Build())
}

// DropTable as the next change.
func (w *CommitWriter) DropTable(table string) error {
	return w.change(s4qlpb.Change_builder{DropTable: s4qlpb.DropTable_builder{Table: &table}.Build()}.Build())
}

// change that is not a batch: write the batch before it, then the change.
func (w *CommitWriter) change(c *s4qlpb.Change) error {
	if err := w.startChange(); err != nil {
		return err
	}
	if err := checkChange(c); err != nil {
		return w.fail(in(err, "change"))
	}
	if err := w.flush(); err != nil {
		return err
	}
	return w.writeRecord(changesField, c)
}

// UpsertRow into the table as the next change: the values of the given columns, as positions in the
// table's columns, in the same order. The writer copies the values before it returns.
func (w *CommitWriter) UpsertRow(table string, columns []uint32, values []*s4qlpb.Value) error {
	if err := w.startChange(); err != nil {
		return err
	}
	if err := checkIdentifier(table); err != nil {
		return w.fail(in(err, "upsert row table"))
	}
	if err := checkColumnPositions(columns); err != nil {
		return w.fail(in(err, "upsert row"))
	}
	row := s4qlpb.Row_builder{Values: values}.Build()
	if err := checkKnown(row.ProtoReflect()); err != nil {
		return w.fail(in(err, "upsert row"))
	}
	if err := checkRow(len(columns), values); err != nil {
		return w.fail(in(err, "upsert row"))
	}

	same := w.batch.field == upsertRowsField && w.batch.table == table && slices.Equal(w.batch.columns, columns)
	return w.add(same, rowsField, row, func() (batch, error) {
		b, err := newBatch(upsertRowsField, s4qlpb.UpsertRows_builder{Table: &table, Columns: columns}.Build())
		b.table, b.columns = table, slices.Clone(columns)
		return b, err
	})
}

// DeleteRow from the table as the next change: the row with the given values of its primary-key
// columns, in key order. The writer copies the values before it returns.
func (w *CommitWriter) DeleteRow(table string, primaryKey []*s4qlpb.Value) error {
	if err := w.startChange(); err != nil {
		return err
	}
	if err := checkIdentifier(table); err != nil {
		return w.fail(in(err, "delete row table"))
	}
	key := s4qlpb.PrimaryKey_builder{Values: primaryKey}.Build()
	if err := checkKnown(key.ProtoReflect()); err != nil {
		return w.fail(in(err, "delete row"))
	}

	same := w.batch.field == deleteRowsField && w.batch.table == table
	want := len(primaryKey)
	if same {
		want = w.batch.keySize
	}
	if err := checkPrimaryKey(want, primaryKey); err != nil {
		return w.fail(in(err, "delete row"))
	}

	return w.add(same, primaryKeysField, key, func() (batch, error) {
		b, err := newBatch(deleteRowsField, s4qlpb.DeleteRows_builder{Table: &table}.Build())
		b.table, b.keySize = table, len(primaryKey)
		return b, err
	})
}

// add the row or key m, which is the given field of its batch, to the current batch if it belongs
// to it and fits, or else write the batch and start a new one with m. It refuses m if a batch of m
// alone is a record above the limit.
func (w *CommitWriter) add(same bool, field protowire.Number, m proto.Message, makeBatch func() (batch, error)) error {
	size := marshalOptions.Size(m)
	encodedSize := protowire.SizeTag(field) + protowire.SizeBytes(size)

	if !same || w.batch.rowsSize()+encodedSize > batchSize || w.batch.changeSize(encodedSize) > w.maxRecordSize {
		b, err := makeBatch()
		if err != nil {
			return w.fail(err)
		}
		if changeSize := b.changeSize(encodedSize); changeSize > w.maxRecordSize {
			return w.fail(fmt.Errorf("%w: a change of this row or key alone is %v bytes, above the limit of %v", ErrMalformed, changeSize, w.maxRecordSize))
		}
		if err := w.flush(); err != nil {
			return err
		}
		w.batch = b
	}

	buf := slices.Grow(w.batch.buf, encodedSize)
	buf = protowire.AppendTag(buf, field, protowire.BytesType)
	buf = protowire.AppendVarint(buf, uint64(size))
	start := len(buf)
	buf, err := marshalOptions.MarshalAppend(buf, m)
	if err != nil {
		return w.fail(err)
	}
	if len(buf)-start != size {
		return w.fail(fmt.Errorf("marshalled %v bytes, and the size was %v", len(buf)-start, size))
	}
	w.batch.buf = buf
	return nil
}

// Annotate the commit object with a, after all changes. The writer writes a at once, and refuses a
// change after it.
func (w *CommitWriter) Annotate(a *s4qlpb.Annotation) error {
	if w.err != nil {
		return w.err
	}
	if err := checkAnnotation(a); err != nil {
		return w.fail(in(err, "annotation"))
	}
	if err := w.flush(); err != nil {
		return err
	}
	w.annotating = true
	return w.writeRecord(annotationField, a)
}

// Close the commit object: write the batch that is not written yet, and return the name of the
// commit object. It does not close the underlying writer. After Close, every call returns an error.
func (w *CommitWriter) Close() (Name, error) {
	if w.err != nil {
		return Name{}, w.err
	}
	if err := w.flush(); err != nil {
		return Name{}, err
	}
	w.err = errors.New("commit writer is closed")
	return Name(w.hash.Sum(nil)), nil
}

// startChange refuses a change after an error or after an annotation.
func (w *CommitWriter) startChange() error {
	if w.err != nil {
		return w.err
	}
	if w.annotating {
		return w.fail(errors.New("a change cannot follow an annotation"))
	}
	return nil
}

// flush the batch, if there is one, as a change: the tags and lengths of the record and the change
// go into the headroom before the batch's bytes, which are then written as they are.
func (w *CommitWriter) flush() error {
	if w.batch.field == 0 {
		return nil
	}
	bodySize := len(w.batch.buf) - headroom
	var head []byte
	head = protowire.AppendTag(head, changesField, protowire.BytesType)
	head = protowire.AppendVarint(head, uint64(w.batch.changeSize(0)))
	head = protowire.AppendTag(head, w.batch.field, protowire.BytesType)
	head = protowire.AppendVarint(head, uint64(bodySize))
	start := headroom - len(head)
	copy(w.batch.buf[start:], head)
	record := w.batch.buf[start:]
	w.batch = batch{}
	return w.write(record)
}

// writeRecord of [s4qlpb.CommitObject]: its tag, its length, and the message m.
func (w *CommitWriter) writeRecord(field protowire.Number, m proto.Message) error {
	size := marshalOptions.Size(m)
	if size > w.maxRecordSize {
		return w.fail(fmt.Errorf("%w: record is %v bytes, above the limit of %v", ErrMalformed, size, w.maxRecordSize))
	}
	record := protowire.AppendTag(make([]byte, 0, size+headroom), field, protowire.BytesType)
	record = protowire.AppendVarint(record, uint64(size))
	start := len(record)
	record, err := marshalOptions.MarshalAppend(record, m)
	if err != nil {
		return w.fail(err)
	}
	if len(record)-start != size {
		return w.fail(fmt.Errorf("marshalled %v bytes, and the size was %v", len(record)-start, size))
	}
	return w.write(record)
}

// write one record in one call of Write, and add it to the checksum.
func (w *CommitWriter) write(record []byte) error {
	if _, err := w.w.Write(record); err != nil {
		return w.fail(err)
	}
	w.hash.Write(record)
	return nil
}

// fail with err, which every later call returns.
func (w *CommitWriter) fail(err error) error {
	w.err = err
	return err
}
