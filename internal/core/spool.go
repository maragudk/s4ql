package core

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"google.golang.org/protobuf/proto"

	"maragu.dev/s4ql/internal/s4qlpb"
)

// spool is a local file that a transaction writes and then reads, so that what it holds is never
// in memory at once: the commit object, whose key in the store is its checksum and so is known
// only when it is complete, and the annotations, which a commit object holds after all its
// changes. The file has no name once it is open, so it is gone when it is closed, and when the
// process ends.
type spool struct {
	f spoolFile
	w *bufio.Writer
	// size is the number of bytes written.
	size int64
}

// spoolFile is the file of a spool: an [*os.File], or a wrapper of one that a test makes fail.
type spoolFile interface {
	io.ReadWriteSeeker
	io.Closer
}

// newSpool file of the given kind, "commit" or "annotations", next to the cache.
func (d *DB) newSpool(kind string) (*spool, error) {
	f, err := os.CreateTemp(d.spoolDir(), d.spoolPattern())
	if err != nil {
		return nil, err
	}
	if err := os.Remove(f.Name()); err != nil {
		_ = f.Close()
		return nil, err
	}
	var file spoolFile = f
	if wrap := d.options.local.spool; wrap != nil {
		file = wrap(kind, f)
	}
	return &spool{f: file, w: bufio.NewWriterSize(file, 64<<10)}, nil
}

// Write to the file, through a buffer.
func (s *spool) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	s.size += int64(n)
	return n, err
}

// rewind writes what is buffered and moves to the start of the file, to read it.
func (s *spool) rewind() error {
	if err := s.w.Flush(); err != nil {
		return err
	}
	_, err := s.f.Seek(0, io.SeekStart)
	return err
}

// close the file, which deletes it.
func (s *spool) close() {
	if s != nil {
		_ = s.f.Close()
	}
}

// writeAnnotation to the spool, after its length.
func (s *spool) writeAnnotation(a *s4qlpb.Annotation) error {
	data, err := proto.Marshal(a)
	if err != nil {
		return err
	}
	if _, err := s.Write(binary.AppendUvarint(nil, uint64(len(data)))); err != nil {
		return err
	}
	_, err = s.Write(data)
	return err
}

// annotations of the spool, in the order in which they were written. It rewinds the spool first.
func (s *spool) annotations(each func(a *s4qlpb.Annotation) error) error {
	if err := s.rewind(); err != nil {
		return err
	}
	r := bufio.NewReaderSize(s.f, 64<<10)
	for {
		size, err := binary.ReadUvarint(r)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return fmt.Errorf("an annotation of %v bytes: %w", size, err)
		}
		a := &s4qlpb.Annotation{}
		if err := proto.Unmarshal(data, a); err != nil {
			return err
		}
		if err := each(a); err != nil {
			return err
		}
	}
}
