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

// spoolFaults makes operations on the spool files of an instance fail on demand. Each failure is
// an I/O error of the operating system, [syscall.EIO]. Whether an operation fails depends only on
// the operations before it, so a test gives the same result on every machine.
type spoolFaults struct {
	mu    sync.Mutex
	rules []*spoolRule
	// failed is the number of operations that failed.
	failed int
}

// spoolRule fails the next operations op ("read", "write", or "seek") on the spool files of the
// given kind ("commit" or "annotations"), as many as times.
type spoolRule struct {
	kind, op string
	times    int
}

// failSpools of the instance that is opened with the options o, as f says.
func failSpools(o *core.Options) *spoolFaults {
	f := &spoolFaults{}
	o.WrapSpools(func(kind string, file *os.File) core.SpoolFile {
		return &faultySpool{File: file, kind: kind, faults: f}
	})
	return f
}

// on makes the next operations op on a spool file of the kind fail, as many as times.
func (f *spoolFaults) on(kind, op string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, &spoolRule{kind: kind, op: op, times: times})
}

// failures so far.
func (f *spoolFaults) failures() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failed
}

// fail reports whether the operation op on a spool file of the kind fails, and counts it if it
// does.
func (f *spoolFaults) fail(kind, op string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rules {
		if r.kind == kind && r.op == op && r.times > 0 {
			r.times--
			f.failed++
			return true
		}
	}
	return false
}

// faultySpool is a spool file whose operations fail when its faults say so.
type faultySpool struct {
	*os.File
	kind   string
	faults *spoolFaults
}

func (s *faultySpool) Read(p []byte) (int, error) {
	if s.faults.fail(s.kind, "read") {
		return 0, &fs.PathError{Op: "read", Path: s.Name(), Err: syscall.EIO}
	}
	return s.File.Read(p)
}

func (s *faultySpool) Write(p []byte) (int, error) {
	if s.faults.fail(s.kind, "write") {
		return 0, &fs.PathError{Op: "write", Path: s.Name(), Err: syscall.EIO}
	}
	return s.File.Write(p)
}

func (s *faultySpool) Seek(offset int64, whence int) (int64, error) {
	if s.faults.fail(s.kind, "seek") {
		return 0, &fs.PathError{Op: "seek", Path: s.Name(), Err: syscall.EIO}
	}
	return s.File.Seek(offset, whence)
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
