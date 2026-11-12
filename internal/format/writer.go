package format

import (
	"crypto/sha256"
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

// Field numbers of [s4qlpb.Change], [s4qlpb.UpsertRows], and [s4qlpb.DeleteRows].
const (
	upsertRowsField protowire.Number = 3
	deleteRowsField protowire.Number = 4
	rowsField       protowire.Number = 3
	primaryKeyField protowire.Number = 2
)

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
	// encoded rows or keys, each with its tag and length, in the order they came.
	encoded []byte
}

// NewCommitWriter that writes a commit object to w, and writes its header first: version 0, the name
// of the parent, and the commit time.
func NewCommitWriter(w io.Writer, parent Name, commitTime time.Time) (*CommitWriter, error) {
	cw := &CommitWriter{w: w, hash: sha256.New()}

	h := s4qlpb.Header_builder{
		Version:    new(uint32(commitVersion)),
		Parent:     parent[:],
		CommitTime: timestamppb.New(commitTime),
	}.Build()
	if err := checkHeader(h); err != nil {
		return nil, err
	}
	b, err := marshalOptions.Marshal(h)
	if err != nil {
		return nil, err
	}
	if err := cw.writeRecord(headerField, b); err != nil {
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
	if err := checkChange(c, "change"); err != nil {
		return w.fail(err)
	}
	if err := w.flush(); err != nil {
		return err
	}
	b, err := marshalOptions.Marshal(c)
	if err != nil {
		return w.fail(err)
	}
	return w.writeRecord(changesField, b)
}

// UpsertRow into the table as the next change: the values of the given columns, as positions in the
// table's columns, in the same order. The writer copies the values before it returns.
func (w *CommitWriter) UpsertRow(table string, columns []uint32, values []*s4qlpb.Value) error {
	if err := w.startChange(); err != nil {
		return err
	}
	path := "upsert row"
	if err := checkIdentifier(table, path+" table"); err != nil {
		return w.fail(err)
	}
	if err := checkColumnPositions(columns, path+" columns"); err != nil {
		return w.fail(err)
	}
	row := s4qlpb.Row_builder{Values: values}.Build()
	if err := checkKnown(row.ProtoReflect(), path); err != nil {
		return w.fail(err)
	}
	if err := checkRow(len(columns), values, path); err != nil {
		return w.fail(err)
	}

	same := w.batch.field == upsertRowsField && w.batch.table == table && slices.Equal(w.batch.columns, columns)
	return w.add(same, rowsField, row, func() batch {
		return batch{field: upsertRowsField, table: table, columns: slices.Clone(columns)}
	})
}

// DeleteRow from the table as the next change: the row with the given values of its primary-key
// columns, in key order. The writer copies the values before it returns.
func (w *CommitWriter) DeleteRow(table string, primaryKey []*s4qlpb.Value) error {
	if err := w.startChange(); err != nil {
		return err
	}
	path := "delete row"
	if err := checkIdentifier(table, path+" table"); err != nil {
		return w.fail(err)
	}
	key := s4qlpb.PrimaryKey_builder{Values: primaryKey}.Build()
	if err := checkKnown(key.ProtoReflect(), path); err != nil {
		return w.fail(err)
	}

	same := w.batch.field == deleteRowsField && w.batch.table == table
	want := len(primaryKey)
	if same {
		want = w.batch.keySize
	}
	if err := checkPrimaryKey(want, primaryKey, path); err != nil {
		return w.fail(err)
	}

	return w.add(same, primaryKeyField, key, func() batch {
		return batch{field: deleteRowsField, table: table, keySize: len(primaryKey)}
	})
}

// add the row or key m, which is the given field of its batch, to the current batch if it belongs
// to it and fits, or else write the batch and start a new one with m.
func (w *CommitWriter) add(same bool, field protowire.Number, m proto.Message, newBatch func() batch) error {
	b, err := marshalOptions.Marshal(m)
	if err != nil {
		return w.fail(err)
	}

	size := protowire.SizeTag(field) + protowire.SizeBytes(len(b))
	if !same || len(w.batch.encoded)+size > batchSize {
		if err := w.flush(); err != nil {
			return err
		}
		w.batch = newBatch()
	}
	w.batch.encoded = protowire.AppendTag(w.batch.encoded, field, protowire.BytesType)
	w.batch.encoded = protowire.AppendBytes(w.batch.encoded, b)
	return nil
}

// Annotate the commit object with a, after all changes. The writer writes a at once, and refuses a
// change after it.
func (w *CommitWriter) Annotate(a *s4qlpb.Annotation) error {
	if w.err != nil {
		return w.err
	}
	if err := checkAnnotation(a, "annotation"); err != nil {
		return w.fail(err)
	}
	if err := w.flush(); err != nil {
		return err
	}
	w.annotating = true
	b, err := marshalOptions.Marshal(a)
	if err != nil {
		return w.fail(err)
	}
	return w.writeRecord(annotationField, b)
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

// flush the batch, if there is one, as a change. The batch is the marshalled message without its
// rows or keys, followed by the encoded rows or keys, which are the next field in number. It is
// the same bytes as marshalling the whole message, because marshalling writes the fields in the
// order of their numbers.
func (w *CommitWriter) flush() error {
	if w.batch.field == 0 {
		return nil
	}
	var m proto.Message
	switch w.batch.field {
	case upsertRowsField:
		m = s4qlpb.UpsertRows_builder{Table: &w.batch.table, Columns: w.batch.columns}.Build()
	case deleteRowsField:
		m = s4qlpb.DeleteRows_builder{Table: &w.batch.table}.Build()
	}
	b, err := marshalOptions.Marshal(m)
	if err != nil {
		return w.fail(err)
	}
	b = append(b, w.batch.encoded...)

	change := protowire.AppendTag(nil, w.batch.field, protowire.BytesType)
	change = protowire.AppendBytes(change, b)
	w.batch = batch{}
	return w.writeRecord(changesField, change)
}

// writeRecord of [s4qlpb.CommitObject]: its tag, its length, and its bytes, in one call of Write.
func (w *CommitWriter) writeRecord(field protowire.Number, b []byte) error {
	if len(b) > MaxRecordSize {
		return w.fail(fmt.Errorf("%w: record is %v bytes, above the limit of %v", ErrMalformed, len(b), MaxRecordSize))
	}
	record := protowire.AppendTag(make([]byte, 0, len(b)+16), field, protowire.BytesType)
	record = protowire.AppendBytes(record, b)
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
