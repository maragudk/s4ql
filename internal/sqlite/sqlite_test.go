package sqlite_test

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sqlite"
)

// Result codes from https://sqlite.org/rescode.html that several tests use.
const (
	codeError             = 1
	codeMisuse            = 21
	codeRange             = 25
	codeConstraintPrimary = 1555
)

func TestOpen(t *testing.T) {
	t.Run("should open and close an in-memory database", func(t *testing.T) {
		c, err := sqlite.Open(":memory:")
		is.NotError(t, err)
		is.NotError(t, c.Close())
	})

	t.Run("should open and close a temporary database", func(t *testing.T) {
		c, err := sqlite.Open("")
		is.NotError(t, err)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		is.NotError(t, c.Close())
	})

	t.Run("should create a database file, and keep its rows when opened again", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")

		c, err := sqlite.Open(path)
		is.NotError(t, err)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		is.NotError(t, c.Exec("insert into t1 (v1) values (42)"))
		is.NotError(t, c.Close())

		c = open(t, path)
		is.EqualSlice(t, []string{"integer:42"}, query(t, c, "select * from t1"))
	})

	t.Run("should fail with SQLITE_CANTOPEN when the file cannot be created", func(t *testing.T) {
		_, err := sqlite.Open(filepath.Join(t.TempDir(), "missing", "test.db"))
		is.Equal(t, 14, code(t, err))
		is.Equal(t, "unable to open database file", message(t, err))
	})

	t.Run("should refuse a path with a NUL byte instead of opening the part before it", func(t *testing.T) {
		dir := t.TempDir()
		_, err := sqlite.Open(filepath.Join(dir, "test.db") + "\x00.other")
		is.Equal(t, "sqlite: path contains a NUL byte", errorString(t, err))

		_, err = os.Stat(filepath.Join(dir, "test.db"))
		is.True(t, errors.Is(err, os.ErrNotExist))
	})
}

func TestConn_Close(t *testing.T) {
	t.Run("should fail with SQLITE_BUSY while a statement is not finalized, and succeed after", func(t *testing.T) {
		c, err := sqlite.Open(":memory:")
		is.NotError(t, err)

		s := prepare(t, c, "select 1")

		err = c.Close()
		is.Equal(t, 5, code(t, err))
		is.Equal(t, "unable to close due to unfinalized statements or unfinished backups", message(t, err))

		is.NotError(t, s.Finalize())
		is.NotError(t, c.Close())
	})

	t.Run("should be safe to call twice, and make other calls return SQLITE_MISUSE", func(t *testing.T) {
		c, err := sqlite.Open(":memory:")
		is.NotError(t, err)
		is.NotError(t, c.Close())
		is.NotError(t, c.Close())

		_, err = c.Prepare("select 1")
		is.Equal(t, codeMisuse, code(t, err))
		is.Equal(t, "bad parameter or other API misuse", message(t, err))
		is.Equal(t, codeMisuse, code(t, c.Exec("select 1")))
	})
}

func TestConn_Prepare(t *testing.T) {
	t.Run("should fail with SQLITE_ERROR and SQLite's message on a syntax error", func(t *testing.T) {
		c := memory(t)

		_, err := c.Prepare("selec 1")
		is.Equal(t, codeError, code(t, err))
		is.Equal(t, `near "selec": syntax error`, message(t, err))
		is.Equal(t, `sqlite: near "selec": syntax error (code 1)`, errorString(t, err))
	})

	t.Run("should allow trailing whitespace and a semicolon", func(t *testing.T) {
		c := memory(t)

		s, err := c.Prepare("select 1; \t\n\f\r")
		is.NotError(t, err)
		is.NotError(t, s.Finalize())
	})

	t.Run("should refuse more than one statement", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))

		_, err := c.Prepare("select 1; drop table t1")
		is.Equal(t, `sqlite: SQL continues after the first statement: " drop table t1"`, errorString(t, err))

		is.EqualSlice(t, []string{"integer:1"}, query(t, c, "select count(*) from sqlite_schema"))
	})

	t.Run("should refuse SQL after a NUL byte instead of ignoring it", func(t *testing.T) {
		c := memory(t)

		_, err := c.Prepare("select 1\x00; drop table t1")
		is.Equal(t, `sqlite: SQL continues after the first statement: "\x00; drop table t1"`, errorString(t, err))
	})

	t.Run("should shorten long refused SQL in the error", func(t *testing.T) {
		c := memory(t)

		_, err := c.Prepare("select 1;" + strings.Repeat("x", 200))
		is.Equal(t, `sqlite: SQL continues after the first statement: "`+strings.Repeat("x", 100)+`..."`, errorString(t, err))
	})

	t.Run("should refuse SQL without a statement", func(t *testing.T) {
		c := memory(t)

		for _, sql := range []string{"", " ", "-- comment", ";"} {
			_, err := c.Prepare(sql)
			is.Equal(t, "sqlite: no statement in SQL", errorString(t, err), sql)
		}
	})
}

func TestConn_Exec(t *testing.T) {
	t.Run("should run a statement without results", func(t *testing.T) {
		c := memory(t)

		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		is.NotError(t, c.Exec("insert into t1 (v1) values (1)"))
		is.EqualSlice(t, []string{"integer:1"}, query(t, c, "select * from t1"))
	})

	t.Run("should run a statement with results and discard them", func(t *testing.T) {
		c := memory(t)

		is.NotError(t, c.Exec("select 1 union all select 2"))
	})

	t.Run("should fail with the extended result code and message of a constraint violation", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		is.NotError(t, c.Exec("insert into t1 (v1) values (1)"))

		err := c.Exec("insert into t1 (v1) values (1)")
		is.Equal(t, codeConstraintPrimary, code(t, err))
		is.Equal(t, "UNIQUE constraint failed: t1.v1", message(t, err))

		var sqliteErr *sqlite.Error
		is.True(t, errors.As(err, &sqliteErr))
		is.Equal(t, sqlite.ResultCode(19), sqliteErr.Code.Primary()) // SQLITE_CONSTRAINT
	})

	t.Run("should run each statement in its own transaction, so a failed statement changes nothing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		c := open(t, path)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		is.NotError(t, c.Exec("insert into t1 (v1) values (1)"))
		is.Equal(t, codeConstraintPrimary, code(t, c.Exec("insert into t1 (v1) values (1)")))

		other := open(t, path)
		is.NotError(t, other.Exec("insert into t1 (v1) values (2)"))
		is.EqualSlice(t, []string{"integer:1", "integer:2"}, query(t, c, "select * from t1"))
	})
}

func TestStmt_BindInt64(t *testing.T) {
	for _, v := range []int64{0, 1, -1, math.MaxInt64, math.MinInt64} {
		t.Run(fmt.Sprintf("should bind %v and read it back as an integer", v), func(t *testing.T) {
			c := memory(t)
			s := prepare(t, c, "select ?")

			is.NotError(t, s.BindInt64(1, v))
			is.EqualSlice(t, []string{fmt.Sprint("integer:", v)}, rows(t, s))
		})
	}

	t.Run("should bind numbered and named parameters by index", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select ?2, :a, ?1")

		is.NotError(t, s.BindInt64(1, 1))
		is.NotError(t, s.BindInt64(2, 2))
		is.NotError(t, s.BindInt64(3, 3))
		is.EqualSlice(t, []string{"integer:2,integer:3,integer:1"}, rows(t, s))
	})

	t.Run("should leave an unbound parameter NULL", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select ?")

		is.EqualSlice(t, []string{"null:"}, rows(t, s))
	})

	t.Run("should fail with SQLITE_RANGE for a parameter index out of range, also beyond 32 bits", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select ?")

		for _, i := range []int{0, -1, 2, 1<<32 + 1, math.MinInt} {
			err := s.BindInt64(i, 1)
			is.Equal(t, codeRange, code(t, err), fmt.Sprint(i))
			is.Equal(t, "column index out of range", message(t, err))
		}
		is.EqualSlice(t, []string{"null:"}, rows(t, s))
	})

	t.Run("should insert a bound value into a table", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		s := prepare(t, c, "insert into t1 (v1) values (?)")

		is.NotError(t, s.BindInt64(1, math.MinInt64))
		is.EqualSlice(t, nil, rows(t, s))
		is.EqualSlice(t, []string{"integer:-9223372036854775808"}, query(t, c, "select * from t1"))
	})
}

func TestStmt_BindText(t *testing.T) {
	for _, v := range []string{"", "a", "it's", "smørrebrød 🥪", "a\x00b", "\x00", "\xff\xfe", strings.Repeat("x", 1<<20)} {
		t.Run(fmt.Sprintf("should bind text of %v bytes and read the same bytes back as text", len(v)), func(t *testing.T) {
			c := memory(t)
			s := prepare(t, c, "select ?, length(cast(?1 as blob))")

			is.NotError(t, s.BindText(1, v))
			ok, err := s.Step()
			is.NotError(t, err)
			is.True(t, ok)
			is.Equal(t, sqlite.TypeText, s.ColumnType(0))
			is.True(t, v == s.ColumnText(0), "the text is not what was bound")
			is.Equal(t, int64(len(v)), s.ColumnInt64(1))
		})
	}

	t.Run("should store bound text in a table and find it again by a bound text", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 text primary key) strict, without rowid"))
		insert := prepare(t, c, "insert into t1 (v1) values (?)")
		is.NotError(t, insert.BindText(1, `a"b'c`))
		is.EqualSlice(t, nil, rows(t, insert))

		s := prepare(t, c, "select count(*) from t1 where v1 = ?")
		is.NotError(t, s.BindText(1, `a"b'c`))
		is.EqualSlice(t, []string{"integer:1"}, rows(t, s))
	})

	t.Run("should fail with SQLITE_RANGE for a parameter index out of range, and with SQLITE_MISUSE when finalized", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select ?")

		for _, i := range []int{0, -1, 2, 1<<32 + 1, math.MinInt} {
			is.Equal(t, codeRange, code(t, s.BindText(i, "a")), fmt.Sprint(i))
		}
		is.NotError(t, s.Finalize())
		is.Equal(t, codeMisuse, code(t, s.BindText(1, "a")))
	})
}

func TestStmt_BindNull(t *testing.T) {
	t.Run("should make a parameter NULL again that was bound before a reset", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select ?")

		is.NotError(t, s.BindInt64(1, 7))
		is.EqualSlice(t, []string{"integer:7"}, rows(t, s))
		is.NotError(t, s.Reset())
		is.NotError(t, s.BindNull(1))
		is.EqualSlice(t, []string{"null:"}, rows(t, s))
	})

	t.Run("should fail with SQLITE_RANGE for a parameter index out of range, and with SQLITE_MISUSE when finalized", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select ?")

		for _, i := range []int{0, -1, 2, 1<<32 + 1, math.MinInt} {
			is.Equal(t, codeRange, code(t, s.BindNull(i)), fmt.Sprint(i))
		}
		is.NotError(t, s.Finalize())
		is.Equal(t, codeMisuse, code(t, s.BindNull(1)))
	})
}

func TestStmt_ColumnText(t *testing.T) {
	t.Run("should convert a value of another type as SQLite does, and read NULL as the empty text", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select 12, 2.5, null, x'6162', ''")

		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)

		is.Equal(t, "12", s.ColumnText(0))
		is.Equal(t, "2.5", s.ColumnText(1))
		is.Equal(t, "", s.ColumnText(2))
		is.Equal(t, "ab", s.ColumnText(3))
		is.Equal(t, sqlite.TypeText, s.ColumnType(4))
		is.Equal(t, "", s.ColumnText(4))
	})

	t.Run("should read the text of each row, and not the text of the row before", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select * from (values ('first'), ('2'), ('the third'))")

		var got []string
		for {
			ok, err := s.Step()
			is.NotError(t, err)
			if !ok {
				break
			}
			got = append(got, s.ColumnText(0))
		}
		is.EqualSlice(t, []string{"first", "2", "the third"}, got)
	})

	t.Run("should panic without a current row, for an index out of range, and on a finalized statement", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select 'a'")

		is.Equal(t, "sqlite: no current row", panicMessage(t, func() { s.ColumnText(0) }))
		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, "sqlite: column index 1 out of range for 1 columns", panicMessage(t, func() { s.ColumnText(1) }))
		is.NotError(t, s.Finalize())
		is.Equal(t, "sqlite: statement is finalized", panicMessage(t, func() { s.ColumnText(0) }))
	})
}

func TestStmt_Reset(t *testing.T) {
	t.Run("should let a finished statement run again, with the values that were bound", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key, v2 int) strict, without rowid"))
		s := prepare(t, c, "insert into t1 (v1, v2) values (?, ?)")

		is.NotError(t, s.BindInt64(2, 7))
		for i := range int64(3) {
			is.NotError(t, s.BindInt64(1, i))
			is.EqualSlice(t, nil, rows(t, s))
			is.NotError(t, s.Reset())
		}
		is.EqualSlice(t, []string{"integer:0,integer:7", "integer:1,integer:7", "integer:2,integer:7"}, query(t, c, "select * from t1"))
	})

	t.Run("should start a statement again from its first row, and leave no current row", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select * from (values (1), (2))")

		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.NotError(t, s.Reset())
		is.Equal(t, "sqlite: no current row", panicMessage(t, func() { s.ColumnInt64(0) }))
		is.EqualSlice(t, []string{"integer:1", "integer:2"}, rows(t, s))
	})

	t.Run("should return the error of a failed step again, and let the statement run again", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		is.NotError(t, c.Exec("insert into t1 (v1) values (1)"))
		s := prepare(t, c, "insert into t1 (v1) values (?)")

		is.NotError(t, s.BindInt64(1, 1))
		_, err := s.Step()
		is.Equal(t, codeConstraintPrimary, code(t, err))
		is.Equal(t, codeConstraintPrimary, code(t, s.Reset()))

		is.NotError(t, s.BindInt64(1, 2))
		is.EqualSlice(t, nil, rows(t, s))
		is.EqualSlice(t, []string{"integer:1", "integer:2"}, query(t, c, "select * from t1"))
	})

	t.Run("should fail with SQLITE_MISUSE on a finalized statement", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select 1")

		is.NotError(t, s.Finalize())
		is.Equal(t, codeMisuse, code(t, s.Reset()))
	})
}

func TestStmt_ColumnCount(t *testing.T) {
	t.Run("should report the number of result columns", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select 1, 2, 3")

		is.Equal(t, 3, s.ColumnCount())
	})

	t.Run("should report zero for a statement without results", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "create table t1 (v1 int primary key) strict, without rowid")

		is.Equal(t, 0, s.ColumnCount())
	})
}

func TestStmt_ColumnName(t *testing.T) {
	t.Run("should report the column names, before and after stepping", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		s := prepare(t, c, `select v1, v1 as "my name", 1 + 1 from t1`)

		is.Equal(t, "v1", s.ColumnName(0))
		is.Equal(t, "my name", s.ColumnName(1))
		is.Equal(t, "1 + 1", s.ColumnName(2))

		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, !ok)
		is.Equal(t, "v1", s.ColumnName(0))
	})

	t.Run("should panic for an index out of range", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select 1")

		is.Equal(t, "sqlite: column index 1 out of range for 1 columns", panicMessage(t, func() { s.ColumnName(1) }))
		is.Equal(t, "sqlite: column index -1 out of range for 1 columns", panicMessage(t, func() { s.ColumnName(-1) }))
	})
}

func TestStmt_ColumnType(t *testing.T) {
	t.Run("should report the type of each value, which can differ between rows of a column", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select 1 union all select 1.5 union all select 'a' union all select x'00' union all select null")

		var types []sqlite.Type
		for {
			ok, err := s.Step()
			is.NotError(t, err)
			if !ok {
				break
			}
			types = append(types, s.ColumnType(0))
		}
		is.EqualSlice(t, []sqlite.Type{sqlite.TypeInteger, sqlite.TypeFloat, sqlite.TypeText, sqlite.TypeBlob, sqlite.TypeNull}, types)
		is.Equal(t, "integer float text blob null", fmt.Sprint(types[0], types[1], types[2], types[3], types[4]))
		is.Equal(t, "Type(0)", fmt.Sprint(sqlite.Type(0)))
	})

	t.Run("should panic without a current row: before the first step, and after the last", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select 1")

		is.Equal(t, "sqlite: no current row", panicMessage(t, func() { s.ColumnType(0) }))
		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, sqlite.TypeInteger, s.ColumnType(0))
		ok, err = s.Step()
		is.NotError(t, err)
		is.True(t, !ok)
		is.Equal(t, "sqlite: no current row", panicMessage(t, func() { s.ColumnType(0) }))
	})

	t.Run("should panic for an index out of range", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select 1")
		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)

		is.Equal(t, "sqlite: column index 1 out of range for 1 columns", panicMessage(t, func() { s.ColumnType(1) }))
		is.Equal(t, "sqlite: column index 4294967296 out of range for 1 columns", panicMessage(t, func() { s.ColumnType(1 << 32) }))
	})
}

func TestStmt_ColumnInt64(t *testing.T) {
	t.Run("should convert a value of another type as SQLite does, which is why ColumnType comes first", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select '12abc', 2.9, null")

		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)

		is.Equal(t, int64(12), s.ColumnInt64(0))
		is.Equal(t, int64(2), s.ColumnInt64(1))
		is.Equal(t, int64(0), s.ColumnInt64(2))
	})

	t.Run("should panic without a current row and for an index out of range", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select 1")

		is.Equal(t, "sqlite: no current row", panicMessage(t, func() { s.ColumnInt64(0) }))
		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, "sqlite: column index 1 out of range for 1 columns", panicMessage(t, func() { s.ColumnInt64(1) }))
	})
}

func TestStmt_Step(t *testing.T) {
	t.Run("should return the extended result code of a failed step, and finalize reports it again", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		is.NotError(t, c.Exec("insert into t1 (v1) values (1)"))
		s, err := c.Prepare("insert into t1 (v1) values (1)")
		is.NotError(t, err)

		_, err = s.Step()
		is.Equal(t, codeConstraintPrimary, code(t, err))
		is.Equal(t, "UNIQUE constraint failed: t1.v1", message(t, err))

		err = s.Finalize()
		is.Equal(t, codeConstraintPrimary, code(t, err))
		is.Equal(t, "UNIQUE constraint failed: t1.v1", message(t, err))
	})

	t.Run("should fail with SQLITE_MISUSE instead of running a finished statement again", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		s := prepare(t, c, "insert into t1 (v1) values (1)")

		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, !ok)

		_, err = s.Step()
		is.Equal(t, codeMisuse, code(t, err))
		is.Equal(t, "bad parameter or other API misuse", message(t, err))
		is.EqualSlice(t, []string{"integer:1"}, query(t, c, "select count(*) from t1"))
	})

	t.Run("should fail with SQLITE_MISUSE instead of running a failed statement again", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		is.NotError(t, c.Exec("insert into t1 (v1) values (1)"))
		s, err := c.Prepare("insert into t1 (v1) values (1)")
		is.NotError(t, err)
		t.Cleanup(func() { _ = s.Finalize() })

		_, err = s.Step()
		is.Equal(t, codeConstraintPrimary, code(t, err))

		_, err = s.Step()
		is.Equal(t, codeMisuse, code(t, err))
		is.Equal(t, "bad parameter or other API misuse", message(t, err))
	})

	t.Run("should fail with SQLITE_CONSTRAINT_DATATYPE for an integer literal too large for an int column", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))

		err := c.Exec("insert into t1 (v1) values (9223372036854775808)")
		is.Equal(t, 3091, code(t, err))
		is.Equal(t, "cannot store REAL value in INT column t1.v1", message(t, err))
	})
}

func TestStmt_Finalize(t *testing.T) {
	t.Run("should be safe to call twice, and make other calls fail with SQLITE_MISUSE or panic", func(t *testing.T) {
		c := memory(t)
		s, err := c.Prepare("select ?")
		is.NotError(t, err)

		is.NotError(t, s.Finalize())
		is.NotError(t, s.Finalize())

		err = s.BindInt64(1, 1)
		is.Equal(t, codeMisuse, code(t, err))
		is.Equal(t, "bad parameter or other API misuse", message(t, err))
		_, err = s.Step()
		is.Equal(t, codeMisuse, code(t, err))
		is.Equal(t, "bad parameter or other API misuse", message(t, err))

		is.Equal(t, "sqlite: statement is finalized", panicMessage(t, func() { s.ColumnCount() }))
		is.Equal(t, "sqlite: statement is finalized", panicMessage(t, func() { s.ColumnName(0) }))
		is.Equal(t, "sqlite: statement is finalized", panicMessage(t, func() { s.ColumnType(0) }))
		is.Equal(t, "sqlite: statement is finalized", panicMessage(t, func() { s.ColumnInt64(0) }))
	})
}

func TestVersion(t *testing.T) {
	t.Run("should report the vendored version", func(t *testing.T) {
		is.Equal(t, readManifest(t).Version, sqlite.Version())
	})
}

func TestSourceID(t *testing.T) {
	t.Run("should report the source ID in the vendored sqlite3.h", func(t *testing.T) {
		header, err := os.ReadFile("c/sqlite3.h")
		is.NotError(t, err)
		match := regexp.MustCompile(`(?m)^#define SQLITE_SOURCE_ID\s+"([^"]+)"$`).FindSubmatch(header)
		is.True(t, match != nil)
		is.Equal(t, string(match[1]), sqlite.SourceID())
	})
}

func TestCompileOptions(t *testing.T) {
	t.Run("should report every option set in options.h", func(t *testing.T) {
		b, err := os.ReadFile("options.h")
		is.NotError(t, err)
		defines := regexp.MustCompile(`(?m)^#define SQLITE_(\w+) (\S+)$`).FindAllSubmatch(b, -1)
		is.True(t, len(defines) >= 10, "options.h has too few options; is the pattern above still right?")

		options := sqlite.CompileOptions()
		for _, d := range defines {
			name, value := string(d[1]), string(d[2])
			// SQLite reports some options with their value, and some on/off options by name only.
			ok := slices.Contains(options, name+"="+value) || (value == "1" && slices.Contains(options, name))
			is.True(t, ok, fmt.Sprintf("SQLITE_%v %v not reported", name, value))
		}
	})

	t.Run("should treat double-quoted strings as identifiers only", func(t *testing.T) {
		c := memory(t)

		_, err := c.Prepare(`select "abc"`)
		is.Equal(t, codeError, code(t, err))
		is.Equal(t, `no such column: "abc" - should this be a string literal in single-quotes?`, message(t, err))

		err = c.Exec(`create table t1 (v1 int primary key check (v1 != "abc")) strict, without rowid`)
		is.Equal(t, codeError, code(t, err))
		is.Equal(t, `no such column: "abc" - should this be a string literal in single-quotes?`, message(t, err))
	})

	t.Run("should still accept a double-quoted DEFAULT, as SQLite does for any identifier after DEFAULT", func(t *testing.T) {
		c := memory(t)

		is.NotError(t, c.Exec(`create table t1 (v1 int primary key, v2 text default "abc") strict, without rowid`))
		is.NotError(t, c.Exec(`create table t2 (v1 int primary key, v2 text default abc) strict, without rowid`))
		is.EqualSlice(t, []string{"integer:1", "integer:1"}, query(t, c, `select dflt_value = '"abc"' from pragma_table_info('t1') where name = 'v2' union all select dflt_value = 'abc' from pragma_table_info('t2') where name = 'v2'`))
	})

	t.Run("should enforce foreign keys on a new connection", func(t *testing.T) {
		c := memory(t)

		is.EqualSlice(t, []string{"integer:1"}, query(t, c, "pragma foreign_keys"))
		is.NotError(t, c.Exec("create table p (id int primary key) strict, without rowid"))
		is.NotError(t, c.Exec("create table c (id int primary key, p int references p (id)) strict, without rowid"))
		err := c.Exec("insert into c values (1, 2)")
		is.Equal(t, 787, code(t, err)) // SQLITE_CONSTRAINT_FOREIGNKEY
		is.Equal(t, "FOREIGN KEY constraint failed", message(t, err))
	})

	t.Run("should not have the load_extension function", func(t *testing.T) {
		c := memory(t)

		_, err := c.Prepare("select load_extension('x')")
		is.Equal(t, codeError, code(t, err))
		is.Equal(t, "no such function: load_extension", message(t, err))
	})

	t.Run("should not memory-map a database file, even when asked to", func(t *testing.T) {
		c := open(t, filepath.Join(t.TempDir(), "test.db"))
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))

		// The pragma returns the size it set, which SQLite caps at the compile-time maximum.
		is.EqualSlice(t, []string{"integer:0"}, query(t, c, "pragma mmap_size = 268435456"))
		is.EqualSlice(t, []string{"integer:0"}, query(t, c, "pragma mmap_size"))
	})

	t.Run("should have the math functions", func(t *testing.T) {
		c := memory(t)

		is.EqualSlice(t, []string{"integer:1,integer:1,integer:1"}, query(t, c, "select sqrt(16) = 4, pow(2, 10) = 1024, trunc(pi()) = 3"))
	})

	t.Run("should fail an application-defined function that sets a subtype without declaring that it does", func(t *testing.T) {
		path, err := buildSubtype()
		if err != nil {
			t.Fatal("building the subtype program:", err)
		}
		out, err := exec.Command(path).CombinedOutput()
		is.NotError(t, err, string(out))
		is.Equal(t, "declared: ok\nundeclared: error 1: misuse of sqlite3_result_subtype() by undeclared()\n", string(out))
	})
}

func TestVendored(t *testing.T) {
	t.Run("should have vendored files with the checksums recorded in source.json and source.h", func(t *testing.T) {
		m := readManifest(t)
		is.Equal(t, 4, len(m.Files))

		header, err := os.ReadFile("source.h")
		is.NotError(t, err)
		is.True(t, bytes.Contains(header, fmt.Appendf(nil, "#define S4QL_SQLITE_VERSION %q\n", m.Version)), "version in source.h")

		for name, sum := range m.Files {
			b, err := os.ReadFile(filepath.Join("c", name))
			is.NotError(t, err)
			got := sha256.Sum256(b)
			is.Equal(t, sum, hex.EncodeToString(got[:]), name)

			define := fmt.Sprintf("#define S4QL_SHA256_%v %q\n", strings.ToUpper(strings.ReplaceAll(name, ".", "_")), sum)
			is.True(t, bytes.Contains(header, []byte(define)), "checksum of "+name+" in source.h")
		}
	})
}

func TestTokenize(t *testing.T) {
	tk := tokenCodes(t)

	t.Run("should give the type and length of each token, and end with a token of length 0", func(t *testing.T) {
		tokens := sqlite.Tokenize("select 1;")
		is.EqualSlice(t, []sqlite.Token{
			{Type: tk["SELECT"], Len: 6},
			{Type: tk["SPACE"], Len: 1},
			{Type: tk["INTEGER"], Len: 1},
			{Type: tk["SEMI"], Len: 1},
			{Type: tk["ILLEGAL"], Len: 0},
		}, tokens)
	})

	t.Run("should give only the token of length 0 for the empty text", func(t *testing.T) {
		is.EqualSlice(t, []sqlite.Token{{Type: tk["ILLEGAL"], Len: 0}}, sqlite.Tokenize(""))
	})

	t.Run("should stop at a NUL byte, as SQLite's parser does", func(t *testing.T) {
		tokens := sqlite.Tokenize("'a\x00b' c")
		is.EqualSlice(t, []sqlite.Token{
			{Type: tk["ILLEGAL"], Len: 2},
			{Type: tk["ILLEGAL"], Len: 0},
		}, tokens)
	})
}

func TestKeywords(t *testing.T) {
	t.Run("should give the 147 keywords of this build in upper case, each once", func(t *testing.T) {
		keywords := sqlite.Keywords()
		is.Equal(t, 147, len(keywords))
		seen := map[string]bool{}
		for _, k := range keywords {
			is.True(t, regexp.MustCompile(`^[A-Z_]+$`).MatchString(k), k)
			is.True(t, !seen[k], k)
			seen[k] = true
		}
		is.True(t, seen["SELECT"])
		is.True(t, seen["CURRENT_TIMESTAMP"])
		is.True(t, !seen["STRICT"])
	})
}

// tokenCodes of the parser, by name without the TK_ prefix, from the definitions in the vendored
// c/sqlite3.c, which has them twice, once from parse.h and once in parse.c. The two must agree.
func tokenCodes(t *testing.T) map[string]int {
	t.Helper()
	b, err := os.ReadFile("c/sqlite3.c")
	is.NotError(t, err)
	codes := map[string]int{}
	matches := regexp.MustCompile(`(?m)^#define TK_(\w+) +(\d+)$`).FindAllSubmatch(b, -1)
	for _, m := range matches {
		code, err := strconv.Atoi(string(m[2]))
		is.NotError(t, err)
		if c, ok := codes[string(m[1])]; ok {
			is.Equal(t, c, code, string(m[1]))
		}
		codes[string(m[1])] = code
	}
	is.True(t, len(codes) > 150, "too few token codes; is the pattern above still right?")
	is.Equal(t, 2*len(codes), len(matches))
	return codes
}

func TestSymbols(t *testing.T) {
	t.Run("should keep all of SQLite's symbols local, so another copy of SQLite can be linked into the same binary", func(t *testing.T) {
		// Build a binary that links the binding. The running test binary may have no symbol table.
		path := filepath.Join(t.TempDir(), "sqlite.test")
		out, err := exec.Command("go", "test", "-c", "-o", path, ".").CombinedOutput()
		is.NotError(t, err, string(out))

		var global, local []string
		if f, err := macho.Open(path); err == nil {
			defer func() { _ = f.Close() }()
			for _, s := range f.Symtab.Syms {
				const nExt, nType, nSect = 0x01, 0x0e, 0x0e
				name, ok := strings.CutPrefix(s.Name, "_")
				if !ok || !strings.HasPrefix(name, "sqlite3") || s.Type&nType != nSect {
					continue
				}
				if s.Type&nExt != 0 {
					global = append(global, name)
				} else {
					local = append(local, name)
				}
			}
		} else {
			f, err := elf.Open(path)
			is.NotError(t, err)
			defer func() { _ = f.Close() }()
			symbols, err := f.Symbols()
			is.NotError(t, err)
			for _, s := range symbols {
				if !strings.HasPrefix(s.Name, "sqlite3") || s.Section == elf.SHN_UNDEF {
					continue
				}
				if elf.ST_BIND(s.Info) == elf.STB_LOCAL {
					local = append(local, s.Name)
				} else {
					global = append(global, s.Name)
				}
			}
		}

		is.EqualSlice(t, nil, global)
		is.True(t, len(local) > 100, fmt.Sprintf("only %v local SQLite symbols, so the check above proves little", len(local)))
	})
}

func TestShell(t *testing.T) {
	t.Run("should report the same version and source ID as the binding", func(t *testing.T) {
		out := shellRun(t, "select sqlite_version(); select sqlite_source_id();")
		is.EqualSlice(t, []string{sqlite.Version(), sqlite.SourceID()}, out)
	})

	t.Run("should report exactly the same compile options as the binding", func(t *testing.T) {
		out := shellRun(t, "pragma compile_options;")
		is.EqualSlice(t, sqlite.CompileOptions(), out)
	})

	// Statements of the shapes the binding is used for, plus a few queries about the build.
	tests := []struct {
		name  string
		setup []string
		query string
	}{
		{
			name:  "an empty table",
			setup: []string{"create table t1 (v1 int primary key) strict, without rowid"},
			query: "select * from t1",
		},
		{
			name: "rows in primary-key order",
			setup: []string{
				"create table t1 (v1 int primary key) strict, without rowid",
				"insert into t1 (v1) values (3)",
				"insert into t1 (v1) values (-1)",
				"insert into t1 (v1) values (9223372036854775807)",
				"insert into t1 (v1) values (-9223372036854775808)",
				"insert into t1 (v1) values (0)",
			},
			query: "select * from t1",
		},
		{
			name: "several tables",
			setup: []string{
				"create table t1 (v1 int primary key) strict, without rowid",
				"create table t2 (v2 int primary key) strict, without rowid",
				"insert into t1 (v1) values (1)",
				"insert into t2 (v2) values (2)",
				"insert into t2 (v2) values (1)",
			},
			query: "select * from t2",
		},
		{
			name:  "the number of compile options",
			query: "select count(*) from pragma_compile_options",
		},
		{
			name:  "foreign keys on a new connection",
			query: "select * from pragma_foreign_keys",
		},
		{
			name:  "the math functions",
			query: "select sqrt(16) = 4, pow(2, 10) = 1024",
		},
	}

	for _, test := range tests {
		t.Run("should give the same results as the binding for "+test.name, func(t *testing.T) {
			c := memory(t)
			for _, sql := range test.setup {
				is.NotError(t, c.Exec(sql))
			}
			s := prepare(t, c, test.query)
			expected := rows(t, s)

			// The shell runs the query inside a common table expression that encodes each value as the
			// rows helper does, in SQL, so that the comparison does not depend on how the shell formats
			// values. The values pass through the common table expression unchanged.
			var columns, encoded []string
			for i := range s.ColumnCount() {
				col := fmt.Sprintf("c%v", i)
				columns = append(columns, col)
				encoded = append(encoded, fmt.Sprintf("typeof(%[1]v) || ':' || case typeof(%[1]v) when 'integer' then %[1]v when 'null' then '' else 'unexpected' end", col))
			}
			script := strings.Join(test.setup, ";\n")
			script += fmt.Sprintf(";\nwith q (%v) as (%v) select %v from q;\n",
				strings.Join(columns, ", "), test.query, strings.Join(encoded, " || ',' || "))

			out := shellRun(t, script)
			if len(expected) == 0 {
				is.EqualSlice(t, []string{""}, out)
				return
			}
			is.EqualSlice(t, expected, out)
		})
	}

	t.Run("should fail with the same messages as the binding for the errors of these statements", func(t *testing.T) {
		tests := []struct {
			setup []string
			sql   string
		}{
			{sql: "select * from t1"},
			{sql: "insert into t1 (v1) values (1)"},
			{setup: []string{"create table t1 (v1 int primary key) strict, without rowid"}, sql: "create table t1 (v1 int primary key) strict, without rowid"},
			{setup: []string{"create table t1 (v1 int primary key) strict, without rowid", "insert into t1 (v1) values (1)"}, sql: "insert into t1 (v1) values (1)"},
			{setup: []string{"create table t1 (v1 int primary key) strict, without rowid"}, sql: "insert into t1 (v1) values (9223372036854775808)"},
			{sql: `select "abc"`},
		}

		// The shell prints errors as "Error near line 3: message" or "Parse error near line 1: message",
		// without the result code, so only the message is compared.
		pattern := regexp.MustCompile(`(?m)near line \d+: (.*)$`)
		for _, test := range tests {
			c := memory(t)
			for _, sql := range test.setup {
				is.NotError(t, c.Exec(sql))
			}
			msg := message(t, c.Exec(test.sql))
			is.True(t, msg != "", "empty message for "+test.sql)

			script := strings.Join(append(test.setup, test.sql), ";\n") + ";\n"
			_, stderr, err := shellExec(t, script)
			is.True(t, err != nil, "shell exit status for "+test.sql)
			match := pattern.FindStringSubmatch(stderr)
			is.True(t, match != nil, "no error in shell output: "+stderr)
			is.Equal(t, msg, match[1])
		}
	})
}

func FuzzStmt_BindInt64(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(math.MaxInt64))
	f.Add(int64(math.MinInt64))
	f.Add(int64(-1))

	f.Fuzz(func(t *testing.T, v int64) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))

		// Store the value and read it back, so it is not only passed through.
		insert := prepare(t, c, "insert into t1 (v1) values (?)")
		is.NotError(t, insert.BindInt64(1, v))
		ok, err := insert.Step()
		is.NotError(t, err)
		is.True(t, !ok)

		sel := prepare(t, c, "select v1, typeof(v1) = 'integer' from t1 where v1 = ?")
		is.NotError(t, sel.BindInt64(1, v))
		ok, err = sel.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, sqlite.TypeInteger, sel.ColumnType(0))
		is.Equal(t, v, sel.ColumnInt64(0))
		is.Equal(t, int64(1), sel.ColumnInt64(1))

		ok, err = sel.Step()
		is.NotError(t, err)
		is.True(t, !ok)
	})
}

func FuzzStmt_BindText(f *testing.F) {
	f.Add("")
	f.Add("a")
	f.Add("a\x00b")
	f.Add("\xff")
	f.Add("smørrebrød")

	f.Fuzz(func(t *testing.T, v string) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 text primary key) strict, without rowid"))

		// Store the value and read it back, so it is not only passed through.
		insert := prepare(t, c, "insert into t1 (v1) values (?)")
		is.NotError(t, insert.BindText(1, v))
		ok, err := insert.Step()
		is.NotError(t, err)
		is.True(t, !ok)

		sel := prepare(t, c, "select v1, length(cast(v1 as blob)) from t1 where v1 = ?")
		is.NotError(t, sel.BindText(1, v))
		ok, err = sel.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, sqlite.TypeText, sel.ColumnType(0))
		is.True(t, v == sel.ColumnText(0), "the text is not what was bound")
		is.Equal(t, int64(len(v)), sel.ColumnInt64(1))

		ok, err = sel.Step()
		is.NotError(t, err)
		is.True(t, !ok)
	})
}

func FuzzConn_Prepare(f *testing.F) {
	f.Add("select 1")
	f.Add("select ?, :a, @b, $c, ?5")
	f.Add("select 1; select 2")
	f.Add("select \x00 1")
	f.Add("create table t2 (v2 int primary key) strict, without rowid")
	f.Add("insert into t1 (v1) values (1)")
	f.Add("select * from t1")
	f.Add("/* unterminated")
	f.Add("select 'unterminated")
	f.Add("")

	f.Fuzz(func(t *testing.T, sql string) {
		// Statements are prepared and inspected, not stepped, so that fuzzed SQL cannot run for long.
		// Some PRAGMAs act when prepared, so each input gets a new connection, and the PRAGMAs that
		// change settings for the whole process are skipped.
		lower := strings.ToLower(sql)
		if strings.Contains(lower, "heap_limit") || strings.Contains(lower, "directory") {
			t.Skip()
		}

		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))

		s, err := c.Prepare(sql)
		if err != nil {
			is.True(t, s == nil)
			var sqliteErr *sqlite.Error
			if errors.As(err, &sqliteErr) {
				is.True(t, sqliteErr.Code != 0)
				is.True(t, sqliteErr.Message != "")
			}
			return
		}

		for i := range s.ColumnCount() {
			_ = s.ColumnName(i)
		}
		for i := range 3 {
			_ = s.BindInt64(i, int64(i))
		}
		is.NotError(t, s.Finalize())
	})
}

// memory database for a test, closed when the test ends.
func memory(t *testing.T) *sqlite.Conn {
	t.Helper()
	return open(t, ":memory:")
}

// open a database for a test, closed when the test ends.
func open(t *testing.T, path string) *sqlite.Conn {
	t.Helper()
	c, err := sqlite.Open(path)
	is.NotError(t, err)
	t.Cleanup(func() { is.NotError(t, c.Close()) })
	return c
}

// prepare a statement, finalized when the test ends.
func prepare(t *testing.T, c *sqlite.Conn, sql string) *sqlite.Stmt {
	t.Helper()
	s, err := c.Prepare(sql)
	is.NotError(t, err)
	t.Cleanup(func() { _ = s.Finalize() })
	return s
}

// query with a single statement and return its rows encoded by [rows].
func query(t *testing.T, c *sqlite.Conn, sql string) []string {
	t.Helper()
	s := prepare(t, c, sql)
	r := rows(t, s)
	is.NotError(t, s.Finalize())
	return r
}

// rows of a statement, stepped to the end, each encoded as its values joined by commas, and each value
// as its type and, for an integer, its decimal value. A value of any other type than integer or NULL
// fails the test, because the tests that use it read only integers.
func rows(t *testing.T, s *sqlite.Stmt) []string {
	t.Helper()
	var rows []string
	for {
		ok, err := s.Step()
		is.NotError(t, err)
		if !ok {
			return rows
		}
		var values []string
		for i := range s.ColumnCount() {
			switch typ := s.ColumnType(i); typ {
			case sqlite.TypeInteger:
				values = append(values, fmt.Sprint("integer:", s.ColumnInt64(i)))
			case sqlite.TypeNull:
				values = append(values, "null:")
			default:
				t.Fatalf("value of type %v in column %v, which the binding cannot read", typ, i)
			}
		}
		rows = append(rows, strings.Join(values, ","))
	}
}

// code of a [*sqlite.Error], failing the test for any other error.
func code(t *testing.T, err error) int {
	t.Helper()
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		t.Fatalf("expected *sqlite.Error, got %T: %v", err, err)
	}
	return int(sqliteErr.Code)
}

// message of a [*sqlite.Error], failing the test for any other error.
func message(t *testing.T, err error) string {
	t.Helper()
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		t.Fatalf("expected *sqlite.Error, got %T: %v", err, err)
	}
	return sqliteErr.Message
}

// errorString of an error, failing the test if there is none.
func errorString(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	return err.Error()
}

// panicMessage of f, failing the test if f does not panic.
func panicMessage(t *testing.T, f func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic")
		}
		msg = fmt.Sprint(r)
	}()
	f()
	return ""
}

type manifest struct {
	Version string            `json:"version"`
	Files   map[string]string `json:"files_sha256"`
}

func readManifest(t *testing.T) manifest {
	t.Helper()
	b, err := os.ReadFile("c/source.json")
	is.NotError(t, err)
	var m manifest
	is.NotError(t, json.Unmarshal(b, &m))
	return m
}

// buildDir holds the C programs built by [buildShell] and [buildSubtype], if any.
var buildDir string

// TestMain removes the C programs built by [buildShell] and [buildSubtype], if any.
func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// buildSQLite compiles the vendored c/sqlite3.c with the compile options in options.h and the C
// compiler that cgo uses, once per test binary, and returns the compiler command and the path of the
// object file.
var buildSQLite = sync.OnceValues(func() ([]string, error) {
	var err error
	buildDir, err = os.MkdirTemp("", "s4ql-sqlite3-")
	if err != nil {
		return nil, err
	}
	out, err := exec.Command("go", "env", "CC").Output()
	if err != nil {
		return nil, err
	}
	cc := strings.Fields(string(out))
	object := filepath.Join(buildDir, "sqlite3.o")
	if err := compile(cc, "-c", "-o", object, "c/sqlite3.c"); err != nil {
		return nil, err
	}
	return cc, nil
})

// buildProgram from the C file at source, linked with the object file from [buildSQLite], and return
// the path of the program.
func buildProgram(name, source string) (string, error) {
	cc, err := buildSQLite()
	if err != nil {
		return "", err
	}
	path := filepath.Join(buildDir, name)
	if err := compile(cc, "-o", path, source, filepath.Join(buildDir, "sqlite3.o"), "-lm", "-lpthread"); err != nil {
		return "", err
	}
	return path, nil
}

// compile with the C compiler command cc and the given arguments, after the options shared by every
// compilation: optimisation, the compile options in options.h, and the vendored headers.
func compile(cc []string, args ...string) error {
	args = slices.Concat(cc[1:], []string{"-O2", "-include", "options.h", "-Ic"}, args)
	if out, err := exec.Command(cc[0], args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%v %v: %w\n%s", cc[0], strings.Join(args, " "), err, out)
	}
	return nil
}

// buildShell builds the official sqlite3 shell from the vendored source, once per test binary, and
// returns its path.
var buildShell = sync.OnceValues(func() (string, error) {
	return buildProgram("sqlite3", "c/shell.c")
})

// buildSubtype builds testdata/subtype.c, once per test binary, and returns its path.
var buildSubtype = sync.OnceValues(func() (string, error) {
	return buildProgram("subtype", "testdata/subtype.c")
})

// shellExec runs the sqlite3 shell on an in-memory database with the given script on stdin, without
// reading any sqliterc file.
func shellExec(t *testing.T, script string) (string, string, error) {
	t.Helper()
	path, err := buildShell()
	if err != nil {
		t.Fatal("building the sqlite3 shell:", err)
	}
	cmd := exec.Command(path, "-batch", "-bail", "-noinit", "-noheader", "-list", ":memory:")
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}

// shellRun runs the sqlite3 shell like [shellExec], fails the test on any error, and returns the lines of output.
func shellRun(t *testing.T, script string) []string {
	t.Helper()
	stdout, stderr, err := shellExec(t, script)
	if err != nil || stderr != "" {
		t.Fatalf("sqlite3 shell: %v\n%v", err, stderr)
	}
	return strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
}
