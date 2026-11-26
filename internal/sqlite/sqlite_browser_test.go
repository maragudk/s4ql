//go:build js && wasm && s4ql_browser

package sqlite_test

import (
	"bytes"
	_ "embed"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sqlite"
)

var (
	//go:embed c/source.json
	amalgamationSource []byte
	//go:embed browser/source.json
	browserSource []byte
)

// Result codes from https://sqlite.org/rescode.html that the tests use.
const (
	codeError  = 1
	codeBusy   = 5
	codeMisuse = 21
)

func open(t *testing.T, path string) *sqlite.Conn {
	t.Helper()
	c, err := sqlite.Open(path)
	is.NotError(t, err)
	t.Cleanup(func() { is.NotError(t, c.Close()) })
	return c
}

func code(t *testing.T, err error) sqlite.ResultCode {
	t.Helper()
	var e *sqlite.Error
	is.True(t, errors.As(err, &e), "not an *sqlite.Error: "+err.Error())
	return e.Code
}

// queryInt64 runs sql, which gives one row with one integer.
func queryInt64(t *testing.T, c *sqlite.Conn, sql string) int64 {
	t.Helper()
	s, err := c.Prepare(sql)
	is.NotError(t, err)
	defer func() { is.NotError(t, s.Finalize()) }()
	ok, err := s.Step()
	is.NotError(t, err)
	is.True(t, ok)
	is.Equal(t, sqlite.TypeInteger, s.ColumnType(0))
	return s.ColumnInt64(0)
}

func TestVersion(t *testing.T) {
	t.Run("should run the version of SQLite that the cgo binding vendors", func(t *testing.T) {
		var amalgamation, browser struct {
			Version string `json:"version"`
		}
		is.NotError(t, json.Unmarshal(amalgamationSource, &amalgamation))
		is.NotError(t, json.Unmarshal(browserSource, &browser))
		is.Equal(t, amalgamation.Version, browser.Version)
		is.Equal(t, amalgamation.Version, sqlite.Version())
		is.True(t, len(sqlite.SourceID()) > 64)
	})
}

func TestMemoryUsed(t *testing.T) {
	t.Run("should count what SQLite allocates, and keep the most that it held as the high-water mark until the mark is reset", func(t *testing.T) {
		before, _ := sqlite.MemoryUsed(true)
		c, err := sqlite.Open(":memory:")
		is.NotError(t, err)
		// An in-memory database holds its pages in memory that SQLite allocates, some 4 MB here.
		is.NotError(t, c.Exec("create table t (v)"))
		is.NotError(t, c.Exec("with recursive n(i) as (select 1 union all select i + 1 from n where i < 4000) insert into t select randomblob(1000) from n"))
		held, highwater := sqlite.MemoryUsed(false)
		is.True(t, held > before+4_000_000, fmt.Sprint(before, " ", held))
		is.True(t, highwater >= held, fmt.Sprint(held, " ", highwater))

		is.NotError(t, c.Close())
		after, kept := sqlite.MemoryUsed(false)
		is.True(t, after < held-4_000_000, fmt.Sprint(held, " ", after))
		is.True(t, kept >= highwater, fmt.Sprint(highwater, " ", kept))

		_, last := sqlite.MemoryUsed(true)
		is.Equal(t, kept, last)
		now, mark := sqlite.MemoryUsed(false)
		is.True(t, mark >= now && mark < held-4_000_000, fmt.Sprint(held, " ", now, " ", mark))
	})
}

func TestOpen_options(t *testing.T) {
	t.Run("should give a connection the settings of options.h that a PRAGMA can make", func(t *testing.T) {
		c := open(t, filepath.Join(t.TempDir(), "a.db"))
		is.Equal(t, int64(4096), queryInt64(t, c, "pragma page_size"))
		is.Equal(t, int64(-2000), queryInt64(t, c, "pragma cache_size"))
		is.Equal(t, int64(-2000), queryInt64(t, c, "pragma temp.cache_size"))
		is.Equal(t, int64(1), queryInt64(t, c, "pragma foreign_keys"))
		is.Equal(t, int64(1), queryInt64(t, c, "pragma temp_store"))
	})

	t.Run("should make a database file with pages of 4096 bytes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a.db")
		c := open(t, path)
		is.NotError(t, c.Exec("create table t (k integer primary key) strict"))
		b, err := os.ReadFile(path)
		is.NotError(t, err)
		// The page size is at offset 16 of the header, big-endian.
		is.Equal(t, 4096, int(b[16])<<8|int(b[17]))
		is.Equal(t, 2*4096, len(b))
	})

	t.Run("should fail for a file that is not a database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a.db")
		is.NotError(t, os.WriteFile(path, bytes.Repeat([]byte("x"), 8192), 0o644))
		_, err := sqlite.Open(path)
		is.Equal(t, sqlite.ResultCode(26), code(t, err))
	})
}

func TestOpenExistingVFS(t *testing.T) {
	t.Run("should open a database file that exists", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a.db")
		c := open(t, path)
		is.NotError(t, c.Exec("create table t (k integer primary key) strict"))
		is.NotError(t, c.Exec("insert into t values (1)"))
		e, err := sqlite.OpenExistingVFS(path, "")
		is.NotError(t, err)
		is.Equal(t, int64(1), queryInt64(t, e, "select count(*) from t"))
		is.NotError(t, e.Close())
	})

	t.Run("should fail with SQLITE_CANTOPEN for a file that does not exist, and not create it", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a.db")
		_, err := sqlite.OpenExistingVFS(path, "")
		is.Equal(t, sqlite.ResultCode(14), code(t, err))
		_, err = os.Stat(path)
		is.True(t, errors.Is(err, os.ErrNotExist))
	})
}

func TestOpen_readOnly(t *testing.T) {
	t.Run("should open a file that may not be written read-only, as SQLite's unix VFS does", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a.db")
		c := open(t, path)
		is.NotError(t, c.Exec("create table t (k integer primary key) strict"))
		is.NotError(t, os.Chmod(path, 0o444))
		r := open(t, path)
		is.Equal(t, int64(0), queryInt64(t, r, "select count(*) from t"))
		// SQLITE_READONLY
		is.Equal(t, sqlite.ResultCode(8), code(t, r.Exec("insert into t values (1)")))
	})
}

func TestConn_Close(t *testing.T) {
	t.Run("should refuse to close with a statement that is not finalized, as sqlite3_close does", func(t *testing.T) {
		c, err := sqlite.Open(":memory:")
		is.NotError(t, err)
		s, err := c.Prepare("select 1")
		is.NotError(t, err)
		err = c.Close()
		is.Equal(t, sqlite.ResultCode(codeBusy), code(t, err))
		is.Equal(t, "sqlite: unable to close due to unfinalized statements or unfinished backups (code 5)", err.Error())
		is.NotError(t, s.Finalize())
		is.NotError(t, c.Close())
		is.NotError(t, c.Close())
	})
}

func TestStmt_Step(t *testing.T) {
	t.Run("should refuse to step a statement that finished until it is reset, as with SQLITE_OMIT_AUTORESET", func(t *testing.T) {
		c := open(t, ":memory:")
		s, err := c.Prepare("select 1")
		is.NotError(t, err)
		defer func() { is.NotError(t, s.Finalize()) }()
		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		ok, err = s.Step()
		is.NotError(t, err)
		is.True(t, !ok)
		_, err = s.Step()
		is.Equal(t, sqlite.ResultCode(codeMisuse), code(t, err))
		is.NotError(t, s.Reset())
		ok, err = s.Step()
		is.NotError(t, err)
		is.True(t, ok)
	})

	t.Run("should refuse to step a statement that failed until it is reset", func(t *testing.T) {
		c := open(t, ":memory:")
		is.NotError(t, c.Exec("create table t (k integer primary key) strict"))
		is.NotError(t, c.Exec("insert into t values (1)"))
		s, err := c.Prepare("insert into t values (1)")
		is.NotError(t, err)
		defer func() { _ = s.Finalize() }()
		_, err = s.Step()
		is.Equal(t, sqlite.ResultCode(1555), code(t, err))
		_, err = s.Step()
		is.Equal(t, sqlite.ResultCode(codeMisuse), code(t, err))
		is.Equal(t, sqlite.ResultCode(1555), code(t, s.Reset()))
		_, err = s.Step()
		is.Equal(t, sqlite.ResultCode(1555), code(t, err))
	})
}

func TestStmt_BindInt64(t *testing.T) {
	t.Run("should keep every 64-bit integer exact, also those that a JavaScript Number cannot hold", func(t *testing.T) {
		c := open(t, ":memory:")
		s, err := c.Prepare("select ?")
		is.NotError(t, err)
		defer func() { is.NotError(t, s.Finalize()) }()
		for _, v := range []int64{0, 1, -1, 1<<53 - 1, 1 << 53, 1<<53 + 1, -(1<<53 - 1), -(1 << 53), -(1<<53 + 1), 1 << 62, math.MaxInt64, math.MinInt64} {
			is.NotError(t, s.BindInt64(1, v))
			ok, err := s.Step()
			is.NotError(t, err)
			is.True(t, ok)
			is.Equal(t, sqlite.TypeInteger, s.ColumnType(0))
			is.Equal(t, v, s.ColumnInt64(0))
			is.NotError(t, s.Reset())
		}
		is.Equal(t, int64(math.MaxInt64), queryInt64(t, c, "select 9223372036854775807"))
		is.Equal(t, int64(math.MinInt64), queryInt64(t, c, "select -9223372036854775807 - 1"))
	})
}

func TestStmt_BindText(t *testing.T) {
	t.Run("should keep the bytes of text as they are, with NUL bytes and bytes that are not UTF-8", func(t *testing.T) {
		c := open(t, ":memory:")
		s, err := c.Prepare("select ?, length(cast(? as blob))")
		is.NotError(t, err)
		defer func() { is.NotError(t, s.Finalize()) }()
		for _, v := range []string{"", "a", "a\x00b", "\xff\xfe", "æøå", string(make([]byte, 100000))} {
			is.NotError(t, s.BindText(1, v))
			is.NotError(t, s.BindText(2, v))
			ok, err := s.Step()
			is.NotError(t, err)
			is.True(t, ok)
			is.Equal(t, sqlite.TypeText, s.ColumnType(0))
			is.Equal(t, v, s.ColumnText(0))
			is.Equal(t, int64(len(v)), s.ColumnInt64(1))
			is.NotError(t, s.Reset())
		}
		is.NotError(t, s.BindNull(1))
		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, sqlite.TypeNull, s.ColumnType(0))
	})
}

func TestStmt_BindDouble(t *testing.T) {
	t.Run("should keep the bits of every real, also -0.0 and the infinities, which a JavaScript Number passed by syscall/js does not", func(t *testing.T) {
		c := open(t, ":memory:")
		s, err := c.Prepare("select ?, typeof(?1)")
		is.NotError(t, err)
		defer func() { is.NotError(t, s.Finalize()) }()
		for _, v := range []float64{0, math.Copysign(0, -1), 1.5, -2.25, 0.1, 1 << 53, math.MaxFloat64, -math.MaxFloat64, math.SmallestNonzeroFloat64, math.Inf(1), math.Inf(-1)} {
			is.NotError(t, s.BindDouble(1, v))
			ok, err := s.Step()
			is.NotError(t, err)
			is.True(t, ok)
			is.Equal(t, sqlite.TypeFloat, s.ColumnType(0))
			is.Equal(t, math.Float64bits(v), math.Float64bits(s.ColumnDouble(0)))
			is.Equal(t, "real", s.ColumnText(1))
			is.NotError(t, s.Reset())
		}

		// SQLite binds a NaN as NULL.
		is.NotError(t, s.BindDouble(1, math.NaN()))
		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, sqlite.TypeNull, s.ColumnType(0))
		is.Equal(t, "null", s.ColumnText(1))
	})

	t.Run("should store reals in a strict table, where -0.0 reads back as 0.0, as in the cgo build", func(t *testing.T) {
		c := open(t, ":memory:")
		is.NotError(t, c.Exec("create table t1 (k1 int primary key, v1 real) strict"))
		values := []float64{1.5, 3, math.MaxFloat64, math.Inf(-1), math.Copysign(0, -1)}
		insert, err := c.Prepare("insert into t1 (k1, v1) values (?, ?)")
		is.NotError(t, err)
		defer func() { is.NotError(t, insert.Finalize()) }()
		for k, v := range values {
			is.NotError(t, insert.BindInt64(1, int64(k)))
			is.NotError(t, insert.BindDouble(2, v))
			ok, err := insert.Step()
			is.NotError(t, err)
			is.True(t, !ok)
			is.NotError(t, insert.Reset())
		}

		s, err := c.Prepare("select v1 from t1 order by k1")
		is.NotError(t, err)
		defer func() { is.NotError(t, s.Finalize()) }()
		for _, v := range []float64{1.5, 3, math.MaxFloat64, math.Inf(-1), 0} {
			ok, err := s.Step()
			is.NotError(t, err)
			is.True(t, ok)
			is.Equal(t, sqlite.TypeFloat, s.ColumnType(0))
			is.Equal(t, math.Float64bits(v), math.Float64bits(s.ColumnDouble(0)))
		}
	})

	t.Run("should read a real that SQL makes, and convert a value of another type as SQLite does", func(t *testing.T) {
		c := open(t, ":memory:")
		s, err := c.Prepare("select -0.0, 3, '2.5', null")
		is.NotError(t, err)
		defer func() { is.NotError(t, s.Finalize()) }()
		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, math.Float64bits(math.Copysign(0, -1)), math.Float64bits(s.ColumnDouble(0)))
		is.Equal(t, 3.0, s.ColumnDouble(1))
		is.Equal(t, 2.5, s.ColumnDouble(2))
		is.Equal(t, uint64(0), math.Float64bits(s.ColumnDouble(3)))
	})
}

func TestStmt_BindBlob(t *testing.T) {
	t.Run("should keep the bytes of a blob as they are, bind the empty blob and a nil slice as the empty blob, and read NULL as nil", func(t *testing.T) {
		c := open(t, ":memory:")
		s, err := c.Prepare("select ?, typeof(?1), length(?1)")
		is.NotError(t, err)
		defer func() { is.NotError(t, s.Finalize()) }()
		large := make([]byte, 4<<20)
		for i := range large {
			large[i] = byte(i * 7)
		}
		for _, v := range [][]byte{nil, {}, {0}, []byte("a\x00b"), {0xff, 0xfe}, large} {
			is.NotError(t, s.BindBlob(1, v))
			ok, err := s.Step()
			is.NotError(t, err)
			is.True(t, ok)
			is.Equal(t, sqlite.TypeBlob, s.ColumnType(0))
			got := s.ColumnBlob(0)
			is.True(t, got != nil, "the blob is nil")
			is.True(t, bytes.Equal(v, got), "the blob is not what was bound")
			is.Equal(t, "blob", s.ColumnText(1))
			is.Equal(t, int64(len(v)), s.ColumnInt64(2))
			is.NotError(t, s.Reset())
		}

		is.NotError(t, s.BindNull(1))
		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, sqlite.TypeNull, s.ColumnType(0))
		is.True(t, s.ColumnBlob(0) == nil, "NULL is not nil")
	})

	t.Run("should store blobs in a strict table, and give copies that keep their bytes after the next step", func(t *testing.T) {
		c := open(t, ":memory:")
		is.NotError(t, c.Exec("create table t1 (v1 blob primary key) strict, without rowid"))
		insert, err := c.Prepare("insert into t1 (v1) values (?)")
		is.NotError(t, err)
		defer func() { is.NotError(t, insert.Finalize()) }()
		for _, v := range [][]byte{{}, {0}, {0, 0}} {
			is.NotError(t, insert.BindBlob(1, v))
			ok, err := insert.Step()
			is.NotError(t, err)
			is.True(t, !ok)
			is.NotError(t, insert.Reset())
		}

		s, err := c.Prepare("select v1 from t1 order by v1")
		is.NotError(t, err)
		defer func() { is.NotError(t, s.Finalize()) }()
		var got [][]byte
		for {
			ok, err := s.Step()
			is.NotError(t, err)
			if !ok {
				break
			}
			got = append(got, s.ColumnBlob(0))
		}
		is.Equal(t, 3, len(got))
		is.True(t, got[0] != nil && len(got[0]) == 0, "the empty blob is not an empty slice")
		is.EqualSlice(t, []byte{0}, got[1])
		is.EqualSlice(t, []byte{0, 0}, got[2])
	})
}

func TestConn_Prepare(t *testing.T) {
	t.Run("should give the message and the offset of a syntax error", func(t *testing.T) {
		c := open(t, ":memory:")
		_, err := c.Prepare("select 1 frm t")
		var e *sqlite.Error
		is.True(t, errors.As(err, &e))
		is.Equal(t, sqlite.ResultCode(codeError), e.Code)
		is.Equal(t, `near "t": syntax error`, e.Message)
		is.Equal(t, 13, e.Offset)
	})

	t.Run("should give the tail of the first statement", func(t *testing.T) {
		c := open(t, ":memory:")
		s, tail, err := c.PrepareFirst("select 1; select 2")
		is.NotError(t, err)
		is.NotError(t, s.Finalize())
		is.Equal(t, 9, tail)
		is.Equal(t, "1", firstColumnName(t, c, "select 1"))
	})
}

func firstColumnName(t *testing.T, c *sqlite.Conn, sql string) string {
	t.Helper()
	s, err := c.Prepare(sql)
	is.NotError(t, err)
	defer func() { is.NotError(t, s.Finalize()) }()
	return s.ColumnName(0)
}

func TestConn_NoCheckpointOnClose(t *testing.T) {
	// wal opens a database in WAL mode at a new path, with a row in the log, which a checkpoint at
	// the close would copy into the file.
	wal := func(t *testing.T) (*sqlite.Conn, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "a.db")
		c, err := sqlite.Open(path)
		is.NotError(t, err)
		is.NotError(t, c.Exec("pragma journal_mode = wal"))
		is.NotError(t, c.Exec("pragma wal_autocheckpoint = 0"))
		is.NotError(t, c.Exec("create table t (k integer primary key) strict"))
		is.NotError(t, c.Exec("insert into t values (1)"))
		return c, path
	}
	walSize := func(t *testing.T, path string) int64 {
		t.Helper()
		info, err := os.Stat(path + "-wal")
		if errors.Is(err, os.ErrNotExist) {
			return -1
		}
		is.NotError(t, err)
		return info.Size()
	}

	t.Run("should checkpoint and delete the write-ahead log at the close without it, as the cgo binding does", func(t *testing.T) {
		c, path := wal(t)
		is.True(t, walSize(t, path) > 0)
		is.NotError(t, c.Close())
		is.Equal(t, int64(-1), walSize(t, path))
	})

	t.Run("should leave the write-ahead log at the close, and the next open should recover the row from it", func(t *testing.T) {
		c, path := wal(t)
		before := walSize(t, path)
		is.NotError(t, c.NoCheckpointOnClose())
		is.NotError(t, c.Close())
		is.Equal(t, before, walSize(t, path))

		c = open(t, path)
		is.Equal(t, int64(1), queryInt64(t, c, "select k from t"))
	})

	t.Run("should refuse a closed connection", func(t *testing.T) {
		c, err := sqlite.Open(":memory:")
		is.NotError(t, err)
		is.NotError(t, c.Close())
		is.Equal(t, sqlite.ResultCode(codeMisuse), code(t, c.NoCheckpointOnClose()))
	})
}

func TestWAL(t *testing.T) {
	t.Run("should share a database in WAL mode between connections, with snapshots and one writer", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a.db")
		w := open(t, path)
		is.NotError(t, w.Exec("pragma journal_mode = wal"))
		is.NotError(t, w.Exec("create table t (k integer primary key) strict"))
		is.NotError(t, w.Exec("insert into t values (1)"))
		r := open(t, path)

		is.NotError(t, r.Exec("begin"))
		is.Equal(t, int64(1), queryInt64(t, r, "select count(*) from t"))
		is.NotError(t, w.Exec("insert into t values (2)"))
		is.Equal(t, int64(1), queryInt64(t, r, "select count(*) from t"))
		is.NotError(t, r.Exec("commit"))
		is.Equal(t, int64(2), queryInt64(t, r, "select count(*) from t"))

		is.NotError(t, w.Exec("begin immediate"))
		is.Equal(t, sqlite.ResultCode(codeBusy), code(t, r.Exec("begin immediate")))
		is.NotError(t, w.Exec("rollback"))
		is.NotError(t, r.Exec("begin immediate"))
		is.NotError(t, r.Exec("rollback"))
	})

	t.Run("should keep the files where Go's os package sees them", func(t *testing.T) {
		dir := t.TempDir()
		c := open(t, filepath.Join(dir, "a.db"))
		is.NotError(t, c.Exec("pragma journal_mode = wal"))
		is.NotError(t, c.Exec("create table t (k integer primary key) strict"))
		entries, err := os.ReadDir(dir)
		is.NotError(t, err)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		is.EqualSlice(t, []string{"a.db", "a.db-wal"}, names)
		info, err := os.Stat(filepath.Join(dir, "a.db-wal"))
		is.NotError(t, err)
		is.True(t, info.Size() > 0)
	})

	t.Run("should let another goroutine release a lock while SQLite waits for it in its busy handler", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a.db")
		a := open(t, path)
		is.NotError(t, a.Exec("pragma journal_mode = wal"))
		b := open(t, path)
		// The timeout is far longer than the lock is held, so that the speed of the machine decides
		// nothing.
		is.NotError(t, b.Exec("pragma busy_timeout = 60000"))

		is.NotError(t, a.Exec("begin immediate"))
		released := make(chan error, 1)
		go func() {
			time.Sleep(100 * time.Millisecond)
			released <- a.Exec("rollback")
		}()
		is.NotError(t, b.Exec("begin immediate"))
		is.NotError(t, <-released)
		is.NotError(t, b.Exec("rollback"))
	})
}
