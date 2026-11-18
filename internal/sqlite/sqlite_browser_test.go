//go:build js && wasm && s4ql_browser

package sqlite_test

import (
	"bytes"
	_ "embed"
	"encoding/json/v2"
	"errors"
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
