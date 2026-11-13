package core_test

import (
	"io/fs"
	"os"
	"slices"
	"sync"
	"syscall"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
)

// spoolFaults makes reads of the spool files of an instance fail on demand. Each failure is an I/O
// error of the operating system, [syscall.EIO]. Whether a read fails depends only on the reads
// before it, and not on time.
type spoolFaults struct {
	mu sync.Mutex
	// reads that are still to fail, by the kind of spool file: "commit" or "annotations".
	reads map[string]int
	// failed is the number of reads that failed.
	failed int
}

// failSpools of the instance that is opened with the options o, from when [spoolFaults.failReads]
// arms them.
func failSpools(o *core.Options) *spoolFaults {
	f := &spoolFaults{reads: map[string]int{}}
	o.WrapSpools(func(kind string, file *os.File) core.SpoolFile {
		return &faultySpool{f: file, kind: kind, faults: f}
	})
	return f
}

// failReads makes the next reads of a spool file of the kind fail, as many as times.
func (f *spoolFaults) failReads(kind string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[kind] += times
}

// failures so far.
func (f *spoolFaults) failures() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failed
}

// failRead reports whether a read of a spool file of the kind fails, and counts it if it does.
func (f *spoolFaults) failRead(kind string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reads[kind] == 0 {
		return false
	}
	f.reads[kind]--
	f.failed++
	return true
}

// faultySpool is a spool file whose reads fail when its faults say so. It does not embed the file,
// so that no method of the file, such as ReadFrom or WriteTo, goes around the faults.
type faultySpool struct {
	f      *os.File
	kind   string
	faults *spoolFaults
}

func (s *faultySpool) Read(p []byte) (int, error) {
	if s.faults.failRead(s.kind) {
		return 0, &fs.PathError{Op: "read", Path: s.f.Name(), Err: syscall.EIO}
	}
	return s.f.Read(p)
}

func (s *faultySpool) Write(p []byte) (int, error) {
	return s.f.Write(p)
}

func (s *faultySpool) Seek(offset int64, whence int) (int64, error) {
	return s.f.Seek(offset, whence)
}

func (s *faultySpool) Close() error {
	return s.f.Close()
}

func TestSpool(t *testing.T) {
	t.Run("should keep no spool file under a name, neither while a transaction is open nor after its commit", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})

		// names in the directory of the cache, other than the cache's own files.
		names := func() []string {
			t.Helper()
			entries, err := os.ReadDir(w.dir)
			is.NotError(t, err)
			var others []string
			for _, e := range entries {
				if !slices.Contains([]string{"a.db", "a.db-wal", "a.db-shm"}, e.Name()) {
					others = append(others, e.Name())
				}
			}
			return others
		}

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		create("t1", 1, "k")(t, tx)
		note("insert", 1)(t, tx)
		is.EqualSlice(t, nil, names())
		is.NotError(t, tx.Commit(t.Context()))
		is.EqualSlice(t, nil, names())
	})
}
