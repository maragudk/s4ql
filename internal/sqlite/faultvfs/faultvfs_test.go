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

// The tests compare SQLite's result codes as numbers, which https://sqlite.org/rescode.html lists:
// 13 is SQLITE_FULL, 14 SQLITE_CANTOPEN, 266 SQLITE_IOERR_READ, 778 SQLITE_IOERR_WRITE, 1034
// SQLITE_IOERR_FSYNC, 1546 SQLITE_IOERR_TRUNCATE, 1802 SQLITE_IOERR_FSTAT, and 3850
// SQLITE_IOERR_LOCK.

func TestFault(t *testing.T) {
	tests := []struct {
		name string
		// journal is the journal mode of the database.
		journal string
		fault   faultvfs.Fault
		// sql runs after the fault is armed, on a connection that has created the table t1 and
		// inserted a row into it.
		sql  string
		code int
	}{
		{name: "should fail a write to the write-ahead log", journal: "wal",
			fault: faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.WAL}, sql: "insert into t1 values (2)", code: 778},
		{name: "should fail a truncate of the write-ahead log", journal: "wal",
			fault: faultvfs.Fault{Ops: faultvfs.Truncate, Files: faultvfs.WAL}, sql: "pragma wal_checkpoint(truncate)", code: 1546},
		{name: "should fail a sync of the write-ahead log", journal: "wal",
			fault: faultvfs.Fault{Ops: faultvfs.Sync, Files: faultvfs.WAL}, sql: "insert into t1 values (2)", code: 1034},
		{name: "should fail a read of the size of the database file", journal: "delete",
			fault: faultvfs.Fault{Ops: faultvfs.Size, Files: faultvfs.MainDB}, sql: "insert into t1 values (2)", code: 1802},
		{name: "should fail a lock of the database file", journal: "delete",
			fault: faultvfs.Fault{Ops: faultvfs.Lock, Files: faultvfs.MainDB}, sql: "insert into t1 values (2)", code: 3850},
		{name: "should fail a write to the rollback journal", journal: "delete",
			fault: faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.MainJournal}, sql: "insert into t1 values (2)", code: 778},
		// SQLite opens the file of a temporary table only when the table outgrows its page cache.
		{name: "should fail the open of a temporary file", journal: "wal",
			fault: faultvfs.Fault{Ops: faultvfs.Open, Files: faultvfs.Temp}, code: 14, sql: `
				create temp table t2 as
				with recursive n(i) as (select 1 union all select i + 1 from n where i < 5000)
				select i, printf('%01024d', i) as v from n`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			v := newVFS(t)
			c, err := sqlite.OpenVFS(filepath.Join(t.TempDir(), "test.db"), v.Name())
			is.NotError(t, err)
			t.Cleanup(func() { _ = c.Close() })
			is.NotError(t, c.Exec("pragma journal_mode = "+test.journal))
			// A sync at every commit, also of the write-ahead log.
			is.NotError(t, c.Exec("pragma synchronous = full"))
			is.NotError(t, c.Exec("create table t1 (k int primary key) strict"))
			is.NotError(t, c.Exec("insert into t1 values (1)"))

			v.Fail(test.fault)
			is.Equal(t, test.code, code(t, c.Exec(test.sql)))
			is.True(t, v.Failures() > 0)
			v.Fail(faultvfs.Fault{})
			is.Equal(t, int64(1), count(t, c))
		})
	}
}

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
		is.Equal(t, 778, code(t, err))
		is.Equal(t, 1, v.Failures())

		v.Fail(faultvfs.Fault{})
		is.Equal(t, int64(1), count(t, c))
		is.NotError(t, c.Exec("insert into t1 values (3)"))
		is.Equal(t, int64(2), count(t, c))
	})

	t.Run("should let the operations before After through, fail Times of them, and then let them through again", func(t *testing.T) {
		v := newVFS(t)
		path := filepath.Join(t.TempDir(), "test.db")

		// A connection opens the database file once, when it is opened.
		v.Fail(faultvfs.Fault{Ops: faultvfs.Open, Files: faultvfs.MainDB, After: 2, Times: 2})
		var codes []int
		for range 6 {
			c, err := sqlite.OpenVFS(path, v.Name())
			codes = append(codes, code(t, err))
			if err == nil {
				is.NotError(t, c.Close())
			}
		}
		is.EqualSlice(t, []int{0, 0, 14, 14, 0, 0}, codes)
		is.Equal(t, 2, v.Failures())
	})

	t.Run("should fail no operation on a file of another kind", func(t *testing.T) {
		v := newVFS(t)
		c := open(t, v, filepath.Join(t.TempDir(), "test.db"))
		is.NotError(t, c.Exec("create table t1 (k int primary key) strict"))

		// In WAL mode, a commit writes to the log and to no journal.
		v.Fail(faultvfs.Fault{Ops: faultvfs.AnyOp, Files: faultvfs.MainJournal | faultvfs.Temp})
		is.NotError(t, c.Exec("insert into t1 values (1)"))
		is.NotError(t, c.Exec("pragma wal_checkpoint(truncate)"))
		is.Equal(t, int64(1), count(t, c))
		is.Equal(t, 0, v.Failures())
	})

	t.Run("should leave no file open after an open that the default VFS refuses", func(t *testing.T) {
		v := newVFS(t)
		_, err := sqlite.OpenVFS(filepath.Join(t.TempDir(), "missing", "test.db"), v.Name())
		is.Equal(t, 14, code(t, err))
		is.Equal(t, 0, v.Failures())
		// The cleanup of the test closes the VFS, which it refuses while a file is open.
	})

	t.Run("should fail with the code of the fault", func(t *testing.T) {
		v := newVFS(t)
		c := open(t, v, filepath.Join(t.TempDir(), "test.db"))
		is.NotError(t, c.Exec("create table t1 (k int primary key) strict"))

		v.Fail(faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.AnyFile, Code: 13})
		is.Equal(t, 13, code(t, c.Exec("insert into t1 values (1)")))
	})

	t.Run("should fail the open of a database file with SQLITE_CANTOPEN", func(t *testing.T) {
		v := newVFS(t)
		v.Fail(faultvfs.Fault{Ops: faultvfs.Open, Files: faultvfs.MainDB})
		_, err := sqlite.OpenVFS(filepath.Join(t.TempDir(), "test.db"), v.Name())
		is.Equal(t, 14, code(t, err))
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
		is.Equal(t, 266, code(t, err))
		is.Equal(t, int64(1), count(t, c))
	})

	// SQLite ends a read transaction in WAL mode with functions that return nothing and that only
	// unlock, which no fault can make fail: pager_unlock and sqlite3WalEndReadTransaction.
	t.Run("should meet no operation that can fail in the rollback of a read transaction", func(t *testing.T) {
		v := newVFS(t)
		c := open(t, v, filepath.Join(t.TempDir(), "test.db"))
		is.NotError(t, c.Exec("create table t1 (k int primary key) strict"))
		is.NotError(t, c.Exec("insert into t1 values (1)"))

		is.NotError(t, c.Exec("begin"))
		is.Equal(t, int64(1), count(t, c))
		v.Fail(faultvfs.Fault{Ops: faultvfs.AnyOp, Files: faultvfs.AnyFile})
		is.NotError(t, c.Exec("rollback"))
		is.Equal(t, 0, v.Failures())
	})

	t.Run("should do nothing when closed twice, and panic when used after its close", func(t *testing.T) {
		v, err := faultvfs.New()
		is.NotError(t, err)
		is.NotError(t, v.Close())
		is.NotError(t, v.Close())
		for name, use := range map[string]func(){
			"name":     func() { _ = v.Name() },
			"fail":     func() { v.Fail(faultvfs.Fault{}) },
			"failures": func() { _ = v.Failures() },
		} {
			func() {
				defer func() { is.Equal(t, "faultvfs: the VFS is closed", recover(), name) }()
				use()
			}()
		}
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
		is.Equal(t, 778, code(t, err))
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
