package faultvfs_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sqlite"
	"maragu.dev/s4ql/internal/sqlite/faultvfs"
)

// newVFS that is closed when the test ends, after the cleanups that were registered later.
func newVFS(t *testing.T) *faultvfs.VFS {
	t.Helper()
	v, err := faultvfs.New()
	is.NotError(t, err)
	t.Cleanup(func() { is.NotError(t, v.Close()) })
	return v
}

// open a database in WAL mode through v, closed when the test ends.
func open(t *testing.T, v *faultvfs.VFS, path string) *sqlite.Conn {
	t.Helper()
	c, err := sqlite.OpenVFS(path, v.Name())
	is.NotError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	is.NotError(t, c.Exec("pragma journal_mode = wal"))
	return c
}

// code of SQLite's error, or 0 for nil.
func code(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var sqliteErr *sqlite.Error
	is.True(t, errors.As(err, &sqliteErr), fmt.Sprint("not an SQLite error: ", err))
	return int(sqliteErr.Code)
}

// count the rows of the table t1.
func count(t *testing.T, c *sqlite.Conn) int64 {
	t.Helper()
	s, err := c.Prepare("select count(*) from t1")
	is.NotError(t, err)
	defer func() { is.NotError(t, s.Finalize()) }()
	ok, err := s.Step()
	is.NotError(t, err)
	is.True(t, ok)
	return s.ColumnInt64(0)
}

// SQLite's result codes, from https://sqlite.org/rescode.html.
const (
	codeCantOpen   = 14
	codeFull       = 13
	codeIOErrRead  = 266
	codeIOErrWrite = 778
)

func TestVFS(t *testing.T) {
	t.Run("should pass every operation on to the default VFS while nothing is armed", func(t *testing.T) {
		v := newVFS(t)
		path := filepath.Join(t.TempDir(), "test.db")
		c := open(t, v, path)
		is.NotError(t, c.Exec("create table t1 (k int primary key) strict"))
		is.NotError(t, c.Exec("insert into t1 values (1), (2)"))
		is.NotError(t, c.Close())

		c = open(t, v, path)
		is.Equal(t, int64(2), count(t, c))
		is.NotError(t, c.Close())
		is.Equal(t, 0, v.Failures())
	})

	t.Run("should fail a write to the write-ahead log, after which SQLite has rolled the transaction back", func(t *testing.T) {
		v := newVFS(t)
		c := open(t, v, filepath.Join(t.TempDir(), "test.db"))
		is.NotError(t, c.Exec("create table t1 (k int primary key) strict"))
		is.NotError(t, c.Exec("insert into t1 values (1)"))

		v.Fail(faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.WAL})
		err := c.Exec("insert into t1 values (2)")
		is.Equal(t, codeIOErrWrite, code(t, err))
		is.Equal(t, 1, v.Failures())

		v.Fail(faultvfs.Fault{})
		is.Equal(t, int64(1), count(t, c))
		is.NotError(t, c.Exec("insert into t1 values (3)"))
		is.Equal(t, int64(2), count(t, c))
	})

	t.Run("should let the operations before After through, fail Times of them, and then let them through again", func(t *testing.T) {
		v := newVFS(t)
		c := open(t, v, filepath.Join(t.TempDir(), "test.db"))
		is.NotError(t, c.Exec("create table t1 (k int primary key) strict"))

		// A commit writes at least one frame to the log, so a skip that large lets several through.
		v.Fail(faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.WAL, After: 1_000_000})
		for i := range 3 {
			is.NotError(t, c.Exec(fmt.Sprint("insert into t1 values (", i, ")")))
		}
		is.Equal(t, 0, v.Failures())

		v.Fail(faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.WAL, Times: 1})
		is.Equal(t, codeIOErrWrite, code(t, c.Exec("insert into t1 values (10)")))
		is.NotError(t, c.Exec("insert into t1 values (11)"))
		is.NotError(t, c.Exec("insert into t1 values (12)"))
		is.Equal(t, 1, v.Failures())
		is.Equal(t, int64(5), count(t, c))
	})

	t.Run("should fail with the code of the fault", func(t *testing.T) {
		v := newVFS(t)
		c := open(t, v, filepath.Join(t.TempDir(), "test.db"))
		is.NotError(t, c.Exec("create table t1 (k int primary key) strict"))

		v.Fail(faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.AnyFile, Code: codeFull})
		is.Equal(t, codeFull, code(t, c.Exec("insert into t1 values (1)")))
	})

	t.Run("should fail the open of a database file with SQLITE_CANTOPEN", func(t *testing.T) {
		v := newVFS(t)
		v.Fail(faultvfs.Fault{Ops: faultvfs.Open, Files: faultvfs.MainDB})
		_, err := sqlite.OpenVFS(filepath.Join(t.TempDir(), "test.db"), v.Name())
		is.Equal(t, codeCantOpen, code(t, err))
		is.Equal(t, 1, v.Failures())
	})

	t.Run("should fail a read of the database file, which fails the statement and nothing after it", func(t *testing.T) {
		v := newVFS(t)
		path := filepath.Join(t.TempDir(), "test.db")
		c := open(t, v, path)
		is.NotError(t, c.Exec("create table t1 (k int primary key) strict"))
		is.NotError(t, c.Exec("insert into t1 values (1)"))
		is.NotError(t, c.Exec("pragma wal_checkpoint(truncate)"))
		is.NotError(t, c.Close())

		// A new connection has no page in its cache, so its first statement reads the file.
		c = open(t, v, path)
		v.Fail(faultvfs.Fault{Ops: faultvfs.Read, Files: faultvfs.MainDB, Times: 1})
		s, err := c.Prepare("select count(*) from t1")
		if err == nil {
			_, err = s.Step()
			_ = s.Finalize()
		}
		is.Equal(t, codeIOErrRead, code(t, err))
		is.Equal(t, int64(1), count(t, c))
	})

	t.Run("should not fail the rollback of a read transaction, whatever operation fails", func(t *testing.T) {
		v := newVFS(t)
		c := open(t, v, filepath.Join(t.TempDir(), "test.db"))
		is.NotError(t, c.Exec("create table t1 (k int primary key) strict"))
		is.NotError(t, c.Exec("insert into t1 values (1)"))

		is.NotError(t, c.Exec("begin"))
		is.Equal(t, int64(1), count(t, c))
		v.Fail(faultvfs.Fault{Ops: faultvfs.Open | faultvfs.Read | faultvfs.Write | faultvfs.Truncate | faultvfs.Sync | faultvfs.Size | faultvfs.Lock, Files: faultvfs.AnyFile})
		is.NotError(t, c.Exec("rollback"))
	})

	t.Run("should fail a write that spills a large transaction to the write-ahead log before its commit", func(t *testing.T) {
		v := newVFS(t)
		c := open(t, v, filepath.Join(t.TempDir(), "test.db"))
		is.NotError(t, c.Exec("create table t1 (k int primary key, v text) strict"))
		is.NotError(t, c.Exec("begin immediate"))
		v.Fail(faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.WAL})
		// 5000 rows of a kibibyte each are more than the 2 MB of SQLite's default page cache.
		err := c.Exec(`
			insert into t1
			with recursive n(i) as (select 1 union all select i + 1 from n where i < 5000)
			select i, printf('%01024d', i) from n`)
		is.Equal(t, codeIOErrWrite, code(t, err))
		_ = c.Exec("rollback")
		v.Fail(faultvfs.Fault{})
		is.Equal(t, int64(0), count(t, c))
	})

	t.Run("should refuse to close while a file opened through it is open", func(t *testing.T) {
		v, err := faultvfs.New()
		is.NotError(t, err)
		c, err := sqlite.OpenVFS(filepath.Join(t.TempDir(), "test.db"), v.Name())
		is.NotError(t, err)
		is.NotError(t, c.Exec("create table t1 (k int primary key) strict"))
		is.True(t, v.Close() != nil, "the VFS closed under an open file")
		is.NotError(t, c.Close())
		name := v.Name()
		is.NotError(t, v.Close())
		_, err = sqlite.OpenVFS(filepath.Join(t.TempDir(), "test.db"), name)
		is.True(t, err != nil, "the VFS is still registered")
	})
}
