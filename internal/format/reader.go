package format

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"maragu.dev/s4ql/internal/s4qlpb"
)

// MaxRecordSize is the largest record of a commit object that [CommitReader] reads and
// [CommitWriter] writes, in bytes: one header, one change, or one annotation, without its tag and
// length. It is 2 GiB less one byte, the limit of Protocol Buffers for one message. It is above
// SQLite's limit of 1,000,000,000 bytes for one value and for one row, plus what the encoding adds
// to a row, so that any row SQLite can hold fits in one change.
//
// A reader holds one record in memory at a time, and allocates memory as the bytes of a record
// arrive, not by the length that the record claims.
const MaxRecordSize = 1<<31 - 1

// Field numbers of [s4qlpb.CommitObject].
const (
	headerField     protowire.Number = 1
	changesField    protowire.Number = 2
	annotationField protowire.Number = 3
)

// unmarshalOptions for the records of a commit object. Unknown fields are kept, because they are
// how a reader finds what a newer version wrote. The schema is at most six messages deep, so a
// deeper message is from a newer version or malformed; the limit of 100 leaves room for newer
// versions and keeps crafted input from going deep.
// Lazy decoding is off, so that a message never refers to the bytes it was decoded from.
var unmarshalOptions = proto.UnmarshalOptions{RecursionLimit: 100, NoLazyDecoding: true}

// CommitReader of one commit object, which reads it as a stream: the header first, then the changes
// and the annotations one at a time, so that it never holds more than one record of the commit
// object in memory.
//
// It refuses a commit object that this version cannot be sure of applying as the writer meant it:
//
//   - a header that is missing, not first, or given twice
//   - a header with a version other than 0, a parent that is not 32 bytes, or no valid commit time
//   - a field or an enum value it does not know, anywhere outside the annotations
//   - a change or a value with no known kind, and a column with no type
//   - a create table with no name, no columns, columns without names, two columns with the same
//     name, or a primary key that is empty, lists a column twice, or points outside its columns
//   - an upsert rows with no table name, no columns, a column listed twice, no rows, or a row with
//     a number of values that differs from the number of its columns
//   - a delete rows with no table name, no keys, a key with no values, or keys with different
//     numbers of values
//   - the name of a table or a column that holds the character NUL
//   - text that is not valid UTF-8, anywhere, and a real that is NaN
//   - a record larger than [MaxRecordSize], and bytes that are not Protocol Buffers
//
// It skips an annotation of a kind it does not know. It does not refuse a field it does not know
// inside an annotation, and an annotation it hands out can hold such fields, and values with no
// known kind.
//
// It checks the bytes against the name of the commit object, their SHA-256 checksum, as it reads,
// and reports a mismatch at the end. Until [CommitReader.Next] has reported the end with [io.EOF],
// nothing that the reader has handed out can be trusted: the caller must use none of it before then,
// for example by applying the changes in a transaction that it commits only after the end. When a
// refusal comes before the end, the reader reads the rest of the commit object first, and reports a
// mismatch with the name, not the refusal, if there is one. A corrupt commit object is then never
// reported as needing a newer version.
//
// After an error, the reader returns the same error from every call.
type CommitReader struct {
	r      *bufio.Reader
	source *sourceReader
	hash   hash.Hash
	name   Name

	// maxRecordSize is [MaxRecordSize], and smaller in tests.
	maxRecordSize uint64

	header  *s4qlpb.Header
	records int
	err     error
}

// NewCommitReader of the commit object with the given name, from r. It reads nothing until the first
// call of a method.
func NewCommitReader(r io.Reader, name Name) *CommitReader {
	h := sha256.New()
	source := &sourceReader{r: r}
	return &CommitReader{
		r:      bufio.NewReaderSize(io.TeeReader(source, h), 64<<10),
		source: source,
		hash:   h,
		name:   name,

		maxRecordSize: MaxRecordSize,
	}
}

// Header of the commit object, which is its first record. [CommitReader.Next] reads it if it has not
// been read.
func (r *CommitReader) Header() (*s4qlpb.Header, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.header != nil {
		return r.header, nil
	}

	num, b, err := r.readRecord()
	switch {
	case errors.Is(err, io.EOF):
		return nil, r.fail(fmt.Errorf("%w: commit object has no header", ErrMalformed))
	case err != nil:
		return nil, r.fail(err)
	case num != headerField:
		return nil, r.fail(fmt.Errorf("%w: commit object does not start with its header", ErrMalformed))
	}

	h := &s4qlpb.Header{}
	if err := unmarshalOptions.Unmarshal(b, h); err != nil {
		return nil, r.fail(fmt.Errorf("%w: header: %v", ErrMalformed, err))
	}
	if err := checkHeader(h); err != nil {
		return nil, r.fail(err)
	}
	r.header = h
	return h, nil
}

// Next change or annotation of the commit object, in the order of the commit object. One of the two
// is not nil. At the end of the commit object, if its bytes match its name, it returns [io.EOF].
func (r *CommitReader) Next() (*s4qlpb.Change, *s4qlpb.Annotation, error) {
	if _, err := r.Header(); err != nil {
		return nil, nil, err
	}

	for {
		num, b, err := r.readRecord()
		switch {
		case errors.Is(err, io.EOF):
			if Name(r.hash.Sum(nil)) != r.name {
				r.err = r.nameMismatch()
				return nil, nil, r.err
			}
			r.err = io.EOF
			return nil, nil, io.EOF
		case err != nil:
			return nil, nil, r.fail(err)
		}

		path := fmt.Sprintf("record %v", r.records)
		switch num {
		case headerField:
			return nil, nil, r.fail(fmt.Errorf("%w: %v is a second header", ErrMalformed, path))

		case changesField:
			c := &s4qlpb.Change{}
			if err := unmarshalOptions.Unmarshal(b, c); err != nil {
				return nil, nil, r.fail(fmt.Errorf("%w: %v: %v", ErrMalformed, path, err))
			}
			if err := checkChange(c, path); err != nil {
				return nil, nil, r.fail(err)
			}
			return c, nil, nil

		case annotationField:
			a := &s4qlpb.Annotation{}
			if err := unmarshalOptions.Unmarshal(b, a); err != nil {
				return nil, nil, r.fail(fmt.Errorf("%w: %v: %v", ErrMalformed, path, err))
			}
			if a.WhichKind() == s4qlpb.Annotation_Kind_not_set_case {
				// A kind of annotation that this version does not know, which it may skip.
				continue
			}
			return nil, a, nil
		}
	}
}

// readRecord of [s4qlpb.CommitObject] from the stream: its field number and its bytes. It returns [io.EOF] at the end of the stream, before a record. A field
// number that is not one of the three fields is from a newer version, and is not read.
func (r *CommitReader) readRecord() (protowire.Number, []byte, error) {
	tag, err := binary.ReadUvarint(r.r)
	if err != nil {
		if errors.Is(err, io.EOF) && r.source.err == nil {
			return 0, nil, io.EOF
		}
		return 0, nil, r.readError(err, "the tag of a record")
	}
	r.records++
	path := fmt.Sprintf("record %v", r.records)

	num, typ := protowire.DecodeTag(tag)
	switch {
	case !num.IsValid() || typ > protowire.Fixed32Type:
		return 0, nil, fmt.Errorf("%w: %v has an invalid tag %v", ErrMalformed, path, tag)
	case num != headerField && num != changesField && num != annotationField:
		return 0, nil, fmt.Errorf("%w: %v is field %v of the commit object, which this version of s4ql does not know", ErrNewerVersion, path, num)
	case typ != protowire.BytesType:
		return 0, nil, fmt.Errorf("%w: %v is field %v with wire type %v, not a message", ErrMalformed, path, num, typ)
	}

	size, err := binary.ReadUvarint(r.r)
	if err != nil {
		return 0, nil, r.readError(err, path+" length")
	}
	if size > r.maxRecordSize {
		return 0, nil, fmt.Errorf("%w: %v is %v bytes, above the limit of %v", ErrMalformed, path, size, r.maxRecordSize)
	}

	// Grow the buffer as the bytes arrive, so that a corrupt length that is larger than what follows
	// allocates no more than about twice what follows.
	b := make([]byte, 0, min(size, 64<<10))
	for uint64(len(b)) < size {
		if len(b) == cap(b) {
			b = slices.Grow(b, int(min(uint64(cap(b)), size-uint64(len(b)))))
		}
		n, err := r.r.Read(b[len(b):min(uint64(cap(b)), size)])
		b = b[:len(b)+n]
		if err != nil && uint64(len(b)) < size {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return 0, nil, r.readError(err, path)
		}
	}
	return num, b, nil
}

// readError for a failure to read part of a record: an error of the source, or the end of the
// stream inside the record, which is malformed.
func (r *CommitReader) readError(err error, what string) error {
	if r.source.err != nil {
		return &sourceError{err: r.source.err}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: commit object ends inside %v", ErrMalformed, what)
	}
	return fmt.Errorf("%w: %v: %v", ErrMalformed, what, err)
}

// fail with err: read the rest of the commit object, and return a mismatch with the name instead
// of err if there is one. An error of the source is returned as it is.
func (r *CommitReader) fail(err error) error {
	var se *sourceError
	if errors.As(err, &se) {
		r.err = se.err
		return r.err
	}

	if _, copyErr := io.Copy(io.Discard, r.r); copyErr != nil {
		r.err = fmt.Errorf("reading the rest of the commit object, after finding that it is %v: %w", err, copyErr)
		return r.err
	}
	if Name(r.hash.Sum(nil)) != r.name {
		r.err = r.nameMismatch()
		return r.err
	}
	r.err = fmt.Errorf("commit object %v: %w", r.name, err)
	return r.err
}

func (r *CommitReader) nameMismatch() error {
	return fmt.Errorf("%w: commit object %v has the checksum %v", ErrNameMismatch, r.name, Name(r.hash.Sum(nil)))
}

// sourceReader records the first error, other than [io.EOF], of the reader of a commit object, so
// that the [CommitReader] can tell it from a commit object that ends too early.
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && s.err == nil {
		s.err = err
	}
	return n, err
}

// sourceError wraps an error of the reader of a commit object, so that [CommitReader.fail] passes it
// on as it is.
type sourceError struct {
	err error
}

func (e *sourceError) Error() string {
	return e.err.Error()
}
