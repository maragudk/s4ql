//go:build !s4ql_browser

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
	codeTooBig            = 18
	codeMisuse            = 21
	codeAuth              = 23
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

func TestOpenVFS(t *testing.T) {
	t.Run("should open a database file through the default VFS for an empty name, and through a VFS by its name", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		c, err := sqlite.OpenVFS(path, "")
		is.NotError(t, err)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		is.NotError(t, c.Close())

		// The default VFS on macOS and Linux.
		c, err = sqlite.OpenVFS(path, "unix")
		is.NotError(t, err)
		is.NotError(t, c.Exec("insert into t1 (v1) values (42)"))
		is.EqualSlice(t, []string{"integer:42"}, query(t, c, "select * from t1"))
		is.NotError(t, c.Close())
	})

	t.Run("should fail for a VFS that is not registered", func(t *testing.T) {
		_, err := sqlite.OpenVFS(filepath.Join(t.TempDir(), "test.db"), "s4ql-no-such-vfs")
		is.Equal(t, codeError, code(t, err))
		is.Equal(t, "no such vfs: s4ql-no-such-vfs", message(t, err))
	})

	t.Run("should refuse a name with a NUL byte", func(t *testing.T) {
		_, err := sqlite.OpenVFS(filepath.Join(t.TempDir(), "test.db"), "unix\x00")
		is.Equal(t, "sqlite: VFS name contains a NUL byte", errorString(t, err))
	})
}

func TestOpenExistingVFS(t *testing.T) {
	t.Run("should open a database file that exists, through the default VFS and through a VFS by its name", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		c := open(t, path)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		is.NotError(t, c.Exec("insert into t1 (v1) values (42)"))

		for _, vfs := range []string{"", "unix"} {
			e, err := sqlite.OpenExistingVFS(path, vfs)
			is.NotError(t, err)
			is.EqualSlice(t, []string{"integer:42"}, query(t, e, "select * from t1"))
			is.NotError(t, e.Close())
		}
	})

	t.Run("should fail with SQLITE_CANTOPEN for a file that does not exist, and not create it", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		_, err := sqlite.OpenExistingVFS(path, "")
		is.Equal(t, 14, code(t, err))
		is.Equal(t, "unable to open database file", message(t, err))
		_, err = os.Stat(path)
		is.True(t, errors.Is(err, os.ErrNotExist))
	})

	t.Run("should fail for a file that was deleted while another connection holds it open", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		c := open(t, path)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		is.NotError(t, c.Exec("begin"))
		is.EqualSlice(t, []string{"integer:0"}, query(t, c, "select count(*) from t1"))
		is.NotError(t, os.Remove(path))

		_, err := sqlite.OpenExistingVFS(path, "")
		is.Equal(t, 14, code(t, err))
		_, err = os.Stat(path)
		is.True(t, errors.Is(err, os.ErrNotExist))
		is.NotError(t, c.Exec("rollback"))
	})

	t.Run("should refuse a path or a name with a NUL byte", func(t *testing.T) {
		dir := t.TempDir()
		_, err := sqlite.OpenExistingVFS(filepath.Join(dir, "test.db")+"\x00.other", "")
		is.Equal(t, "sqlite: path contains a NUL byte", errorString(t, err))
		_, err = sqlite.OpenExistingVFS(filepath.Join(dir, "test.db"), "unix\x00")
		is.Equal(t, "sqlite: VFS name contains a NUL byte", errorString(t, err))
	})
}

func TestFindVFS(t *testing.T) {
	t.Run("should find no VFS for a name that is not registered", func(t *testing.T) {
		is.True(t, sqlite.FindVFS("s4ql-no-such-vfs") == nil)
	})

	t.Run("should find the default VFS by its name, the unix VFS on macOS and Linux", func(t *testing.T) {
		vfs := sqlite.FindVFS("")
		is.True(t, vfs != nil)
		is.True(t, sqlite.FindVFS("unix") == vfs)
	})

	t.Run("should find no VFS for a name with a NUL byte", func(t *testing.T) {
		is.True(t, sqlite.FindVFS("unix\x00x") == nil)
	})
}

func TestRegisterVFS(t *testing.T) {
	t.Run("should register a VFS under its name, not as the default, and unregister it", func(t *testing.T) {
		// The unix-excl VFS is one of SQLite's own, so registering it again changes nothing but the
		// order of the list.
		vfs := sqlite.FindVFS("unix-excl")
		is.True(t, vfs != nil)
		is.NotError(t, sqlite.RegisterVFS(vfs))
		is.True(t, sqlite.FindVFS("") == sqlite.FindVFS("unix"), "the default VFS changed")
		is.NotError(t, sqlite.UnregisterVFS(vfs))
		is.True(t, sqlite.FindVFS("unix-excl") == nil)
		is.NotError(t, sqlite.UnregisterVFS(vfs))
		is.NotError(t, sqlite.RegisterVFS(vfs))
		is.True(t, sqlite.FindVFS("unix-excl") == vfs)
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

	t.Run("should give the byte offset of the token that an error names", func(t *testing.T) {
		c := memory(t)

		_, err := c.Prepare("select 1 frm t")
		is.Equal(t, `near "t": syntax error`, message(t, err))
		is.Equal(t, 13, offset(t, err))

		_, err = c.Prepare("select 'ünterminated")
		is.Equal(t, `unrecognized token: "'ünterminated"`, message(t, err))
		is.Equal(t, 7, offset(t, err))

		_, err = c.Prepare("select 1, nosuch")
		is.Equal(t, "no such column: nosuch", message(t, err))
		is.Equal(t, 10, offset(t, err))
	})

	t.Run("should give the offset -1 for an error that names no token", func(t *testing.T) {
		c := memory(t)

		_, err := c.Prepare("select * from")
		is.Equal(t, "incomplete input", message(t, err))
		is.Equal(t, -1, offset(t, err))

		_, err = c.Prepare("select 1_")
		is.Equal(t, `unrecognized token: "1_"`, message(t, err))
		is.Equal(t, -1, offset(t, err))

		_, err = c.Prepare("select * from nosuch")
		is.Equal(t, "no such table: nosuch", message(t, err))
		is.Equal(t, -1, offset(t, err))
	})

	t.Run("should give the offset -1 for an error that is not about SQL text", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Close())

		_, err := c.Prepare("select 1")
		is.Equal(t, codeMisuse, code(t, err))
		is.Equal(t, -1, offset(t, err))
	})
}

func TestConn_PrepareFirst(t *testing.T) {
	t.Run("should prepare the first statement and give the offset after it", func(t *testing.T) {
		c := memory(t)

		s, tail, err := c.PrepareFirst("select 1; select 2")
		is.NotError(t, err)
		is.Equal(t, 9, tail)
		is.EqualSlice(t, []string{"integer:1"}, rows(t, s))
		is.NotError(t, s.Finalize())
	})

	t.Run("should give the end of the text as the tail of a statement without a semicolon", func(t *testing.T) {
		c := memory(t)

		s, tail, err := c.PrepareFirst("select 1  ")
		is.NotError(t, err)
		is.Equal(t, 10, tail)
		is.NotError(t, s.Finalize())
	})

	t.Run("should read the text up to its first NUL byte", func(t *testing.T) {
		c := memory(t)

		s, tail, err := c.PrepareFirst("select 1\x00 this is not read")
		is.NotError(t, err)
		is.Equal(t, 8, tail)
		is.NotError(t, s.Finalize())

		_, tail, err = c.PrepareFirst("select \x00 1")
		is.Equal(t, "incomplete input", message(t, err))
		is.Equal(t, 7, tail)
	})

	t.Run("should skip empty statements before the first statement", func(t *testing.T) {
		c := memory(t)

		s, tail, err := c.PrepareFirst(" ; ;select 1;")
		is.NotError(t, err)
		is.Equal(t, 13, tail)
		is.NotError(t, s.Finalize())
	})

	t.Run("should give no statement and no error for text without a statement", func(t *testing.T) {
		c := memory(t)

		for _, sql := range []string{"", " ", "-- comment", ";", "\x00select 1"} {
			s, tail, err := c.PrepareFirst(sql)
			is.NotError(t, err, sql)
			is.True(t, s == nil, sql)
			is.Equal(t, min(len(sql), strings.IndexByte(sql+"\x00", 0)), tail, sql)
		}
	})

	t.Run("should give where the parser stopped, and the error's offset, on an error", func(t *testing.T) {
		c := memory(t)

		s, tail, err := c.PrepareFirst("select 1 frm t; select 2")
		is.True(t, s == nil)
		is.Equal(t, `near "t": syntax error`, message(t, err))
		is.Equal(t, 13, offset(t, err))
		is.Equal(t, 14, tail)
	})

	t.Run("should give the tail -1 when SQLite refuses the text before parsing it", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Close())

		_, tail, err := c.PrepareFirst("select 1")
		is.Equal(t, codeMisuse, code(t, err))
		is.Equal(t, -1, tail)
	})
}

func TestConn_DenyProcessPragmas(t *testing.T) {
	t.Run("should refuse the PRAGMAs that change settings for the whole process", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.DenyProcessPragmas())

		for _, sql := range []string{
			"pragma soft_heap_limit = 1",
			"PRAGMA Hard_Heap_Limit=1",
			"pragma main.soft_heap_limit",
			"pragma temp_store_directory = 'x'",
			"pragma data_store_directory = 'x'",
		} {
			_, _, err := c.PrepareFirst(sql)
			is.Equal(t, codeAuth, code(t, err), sql)
			is.Equal(t, "not authorized", message(t, err), sql)
		}
	})

	t.Run("should allow other PRAGMAs and other statements", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.DenyProcessPragmas())

		is.EqualSlice(t, []string{"integer:1"}, query(t, c, "pragma foreign_keys"))
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		is.EqualSlice(t, []string{"integer:0"}, query(t, c, "select count(*) from t1"))
	})
}

func TestConn_SetLengthLimit(t *testing.T) {
	t.Run("should refuse a longer string or blob with SQLITE_TOOBIG", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.SetLengthLimit(1000))

		is.EqualSlice(t, []string{"integer:1000"}, query(t, c, "select length(zeroblob(1000))"))
		err := c.Exec("select zeroblob(1001)")
		is.Equal(t, 18, code(t, err))
		is.Equal(t, "string or blob too big", message(t, err))
	})

	t.Run("should refuse a create table whose update of sqlite_schema is as long as the limit, on every host", func(t *testing.T) {
		// SQLite writes the table with "UPDATE 'main'.sqlite_master SET type='table', name=%Q,
		// tbl_name=%Q, rootpage=#2, sql=%Q WHERE rowid=#1", which it formats under the limit: 96
		// bytes, the name twice, and the text that it keeps, each in quotes. Without
		// SQLITE_WITHOUT_ZONEMALLOC, Apple's allocator lets it take a text some bytes longer.
		for _, shape := range []struct {
			name   string
			create func(n int) string
			length func(n int) int
		}{
			{"a long table name", func(n int) string {
				return "create table " + strings.Repeat("x", n) + " (v int primary key) strict, without rowid"
			}, func(n int) int {
				return 96 + 2*(n+2) + 2 + len("CREATE TABLE ") + n + len(" (v int primary key) strict, without rowid")
			}},
			{"a long column name", func(n int) string {
				return "create table t (" + strings.Repeat("x", n) + " int primary key) strict, without rowid"
			}, func(n int) int {
				return 96 + 2*3 + 2 + len("CREATE TABLE t (") + n + len(" int primary key) strict, without rowid")
			}},
			{"quotes in the table name", func(n int) string {
				return `create table "` + strings.Repeat("'", n) + `" (v int primary key) strict, without rowid`
			}, func(n int) int {
				return 96 + 2*(2*n+2) + 2 + len(`CREATE TABLE ""`) + 2*n + len(" (v int primary key) strict, without rowid")
			}},
		} {
			c := memory(t)
			is.NotError(t, c.SetLengthLimit(1000))
			for n := 1; n < 1000; n++ {
				s, err := c.Prepare(shape.create(n))
				if shape.length(n) < 1000 {
					is.NotError(t, err, shape.name, n)
					is.NotError(t, s.Finalize())
					continue
				}
				is.Equal(t, 18, code(t, err), shape.name, n)
			}
		}
	})

	t.Run("should refuse a limit that is not positive, and a closed connection", func(t *testing.T) {
		c := memory(t)
		is.Equal(t, codeMisuse, code(t, c.SetLengthLimit(0)))

		closed, err := sqlite.Open(":memory:")
		is.NotError(t, err)
		is.NotError(t, closed.Close())
		is.Equal(t, codeMisuse, code(t, closed.SetLengthLimit(10)))
	})
}

func TestConn_AddTestFunctions(t *testing.T) {
	t.Run("should leave REGEXP, MATCH, and GLOB with ESCAPE as SQLite has them without the stubs", func(t *testing.T) {
		c := memory(t)

		is.Equal(t, "no such function: regexp", message(t, c.Exec("select 'a' regexp 'b'")))
		is.Equal(t, "unable to use function MATCH in the requested context", message(t, c.Exec("select 'a' match 'b'")))
		is.Equal(t, "wrong number of arguments to function glob()", message(t, c.Exec("select 'a' glob 'b' escape 'c'")))
	})

	t.Run("should order the text of the first argument to the second, a prefix first", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.AddTestFunctions())

		is.EqualSlice(t, []string{"integer:-1,integer:0,integer:1,integer:-1,integer:1,integer:0,integer:-1"},
			query(t, c, "select regexp('a', 'b'), regexp('b', 'b'), regexp('b', 'a'), regexp('a', 'ab'), regexp('ab', 'a'), regexp('', ''), regexp(x'00', x'0000')"))
		is.EqualSlice(t, []string{"integer:-1,integer:1,integer:0"},
			query(t, c, "select match(12, 3), match(x'ff', 'a'), match(2.5, '2.5')"))
	})

	t.Run("should call the stubs with the operands of the operators in the order SQLite gives them", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.AddTestFunctions())

		// X REGEXP Y is regexp(Y, X), and X GLOB Y ESCAPE Z is glob(Y, X, Z).
		is.EqualSlice(t, []string{"integer:1,integer:-1,integer:0,integer:0,integer:1"},
			query(t, c, "select 'a' regexp 'b', 'a' match 'B', not ('a' regexp 'b'), not ('a' not match 'a'), 'a' not regexp 'a'"))
		is.EqualSlice(t, []string{"integer:3,integer:-4,integer:-3,integer:1,integer:0"},
			query(t, c, "select 'a' glob 'b' escape 'a', 'b' glob 'a' escape 'c', 'b' regexp 'a' escape 'b', 'a' match 'a' escape '', 'x' glob 'x' escape 'x'"))
	})

	t.Run("should give NULL if any argument is NULL", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.AddTestFunctions())

		is.EqualSlice(t, []string{"null:,null:,null:,null:"},
			query(t, c, "select regexp(null, 'a'), match('a', null), glob('a', 'b', null), null regexp null"))
	})

	t.Run("should keep the GLOB of SQLite with two arguments", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.AddTestFunctions())

		is.EqualSlice(t, []string{"integer:1,integer:0"}, query(t, c, "select 'abc' glob 'a*', 'abc' glob 'b*'"))
	})

	t.Run("should be deterministic, so that an index and a CHECK can use them", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.AddTestFunctions())

		is.NotError(t, c.Exec("create table t1 (v1 text check ((v1 regexp 'm') >= 0))"))
		is.NotError(t, c.Exec("create index i1 on t1 (regexp(v1, 'm'))"))
		is.NotError(t, c.Exec("insert into t1 (v1) values ('a')"))
		is.Equal(t, "CHECK constraint failed: (v1 regexp 'm') >= 0", message(t, c.Exec("insert into t1 (v1) values ('z')")))
	})

	t.Run("should refuse a closed connection", func(t *testing.T) {
		closed, err := sqlite.Open(":memory:")
		is.NotError(t, err)
		is.NotError(t, closed.Close())
		is.Equal(t, codeMisuse, code(t, closed.AddTestFunctions()))
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

func TestStmt_BindDouble(t *testing.T) {
	for _, v := range []float64{0, 1.5, -2.25, 0.1, 1e300, -1e-300, math.MaxFloat64, -math.MaxFloat64, math.SmallestNonzeroFloat64, 1 << 53, math.Inf(1), math.Inf(-1), math.Copysign(0, -1)} {
		t.Run(fmt.Sprintf("should bind %v and read back the same bits as a real", v), func(t *testing.T) {
			c := memory(t)
			s := prepare(t, c, "select ?, typeof(?1)")

			is.NotError(t, s.BindDouble(1, v))
			ok, err := s.Step()
			is.NotError(t, err)
			is.True(t, ok)
			is.Equal(t, sqlite.TypeFloat, s.ColumnType(0))
			is.Equal(t, math.Float64bits(v), math.Float64bits(s.ColumnDouble(0)))
			is.Equal(t, "real", s.ColumnText(1))
		})
	}

	t.Run("should bind a NaN as NULL, as SQLite does", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select ?, typeof(?1)")

		is.NotError(t, s.BindDouble(1, math.NaN()))
		is.EqualSlice(t, []string{`null:,text:"null"`}, rows(t, s))
	})

	t.Run("should store bound reals in a strict table and read the same bits back, but -0.0 as 0.0 and a NaN as NULL", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (k1 int primary key, v1 real) strict"))
		insert := prepare(t, c, "insert into t1 (k1, v1) values (?, ?)")
		values := []float64{1.5, 3, math.MaxFloat64, math.SmallestNonzeroFloat64, math.Inf(1), math.Inf(-1), math.Copysign(0, -1), math.NaN()}
		for k, v := range values {
			is.NotError(t, insert.BindInt64(1, int64(k)))
			is.NotError(t, insert.BindDouble(2, v))
			is.EqualSlice(t, nil, rows(t, insert))
			is.NotError(t, insert.Reset())
		}

		// SQLite stores a real without a fraction as an integer in the record, and reads it back as a
		// real, so the sign of -0.0 does not survive. And a NaN is NULL before it is stored.
		is.EqualSlice(t, []string{
			`float:1.5,text:"real"`,
			`float:3,text:"real"`,
			`float:1.7976931348623157e+308,text:"real"`,
			`float:5e-324,text:"real"`,
			`float:+Inf,text:"real"`,
			`float:-Inf,text:"real"`,
			`float:0,text:"real"`,
			`null:,text:"null"`,
		}, query(t, c, "select v1, typeof(v1) from t1 order by k1"))

		s := prepare(t, c, "select count(*) from t1 where v1 = ?")
		is.NotError(t, s.BindDouble(1, math.MaxFloat64))
		is.EqualSlice(t, []string{"integer:1"}, rows(t, s))
	})

	t.Run("should fail with SQLITE_RANGE for a parameter index out of range, and with SQLITE_MISUSE when finalized", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select ?")

		for _, i := range []int{0, -1, 2, 1<<32 + 1, math.MinInt} {
			is.Equal(t, codeRange, code(t, s.BindDouble(i, 1)), fmt.Sprint(i))
		}
		is.NotError(t, s.Finalize())
		is.Equal(t, codeMisuse, code(t, s.BindDouble(1, 1)))
	})
}

func TestStmt_BindBlob(t *testing.T) {
	large := make([]byte, 4<<20)
	for i := range large {
		large[i] = byte(i * 7)
	}
	for _, v := range [][]byte{{}, {0}, {0, 0, 0}, []byte("a\x00b"), {0xff, 0xfe}, large} {
		t.Run(fmt.Sprintf("should bind a blob of %v bytes and read the same bytes back as a blob", len(v)), func(t *testing.T) {
			c := memory(t)
			s := prepare(t, c, "select ?, typeof(?1), length(?1)")

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
		})
	}

	t.Run("should bind a nil slice as the empty blob, not as NULL", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select ?, typeof(?1), length(?1)")

		is.NotError(t, s.BindBlob(1, nil))
		is.EqualSlice(t, []string{`blob:,text:"blob",integer:0`}, rows(t, s))
	})

	t.Run("should copy the bytes when binding, so that a later change to the slice changes nothing", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select ?")

		v := []byte("abc")
		is.NotError(t, s.BindBlob(1, v))
		v[0] = 'x'
		is.EqualSlice(t, []string{"blob:616263"}, rows(t, s))
	})

	t.Run("should store bound blobs in a strict table and find them again by a bound blob", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 blob primary key) strict, without rowid"))
		insert := prepare(t, c, "insert into t1 (v1) values (?)")
		for _, v := range [][]byte{{}, {0}, []byte("a\x00b"), large} {
			is.NotError(t, insert.BindBlob(1, v))
			is.EqualSlice(t, nil, rows(t, insert))
			is.NotError(t, insert.Reset())
		}
		is.EqualSlice(t, []string{`blob:,text:"blob"`, `blob:00,text:"blob"`, `blob:610062,text:"blob"`}, query(t, c, "select v1, typeof(v1) from t1 where length(v1) < 10 order by v1"))

		s := prepare(t, c, "select v1 from t1 where v1 = ?")
		is.NotError(t, s.BindBlob(1, large))
		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.True(t, bytes.Equal(large, s.ColumnBlob(0)), "the large blob is not what was stored")
	})

	t.Run("should fail with SQLITE_TOOBIG for a blob longer than the length limit", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.SetLengthLimit(1000))
		s := prepare(t, c, "select ?")

		err := s.BindBlob(1, make([]byte, 1001))
		is.Equal(t, codeTooBig, code(t, err))
		is.Equal(t, "string or blob too big", message(t, err))
		is.EqualSlice(t, []string{"null:"}, rows(t, s))
		is.NotError(t, s.Reset())
		is.NotError(t, s.BindBlob(1, make([]byte, 1000)))
	})

	t.Run("should fail with SQLITE_RANGE for a parameter index out of range, and with SQLITE_MISUSE when finalized", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select ?")

		for _, i := range []int{0, -1, 2, 1<<32 + 1, math.MinInt} {
			is.Equal(t, codeRange, code(t, s.BindBlob(i, []byte("a"))), fmt.Sprint(i))
		}
		is.NotError(t, s.Finalize())
		is.Equal(t, codeMisuse, code(t, s.BindBlob(1, []byte("a"))))
	})
}

func TestStmt_ColumnDouble(t *testing.T) {
	t.Run("should convert a value of another type as SQLite does, and read NULL as 0", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select 3, '2.5', '2.5abc', x'31', null, 9223372036854775807")

		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)

		is.Equal(t, 3.0, s.ColumnDouble(0))
		is.Equal(t, 2.5, s.ColumnDouble(1))
		is.Equal(t, 2.5, s.ColumnDouble(2))
		is.Equal(t, 1.0, s.ColumnDouble(3))
		is.Equal(t, sqlite.TypeNull, s.ColumnType(4))
		is.Equal(t, uint64(0), math.Float64bits(s.ColumnDouble(4)))
		is.Equal(t, 9223372036854775807.0, s.ColumnDouble(5))
	})

	t.Run("should read reals that SQL makes, with the sign of -0.0 and the infinities", func(t *testing.T) {
		c := memory(t)
		is.EqualSlice(t, []string{"float:-0,float:+Inf,float:-Inf,float:0.1,null:"}, query(t, c, "select -0.0, 1e999, -1e999, 0.1, 1e999 - 1e999"))
	})

	t.Run("should panic without a current row, for an index out of range, and on a finalized statement", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select 1.5")

		is.Equal(t, "sqlite: no current row", panicMessage(t, func() { s.ColumnDouble(0) }))
		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, "sqlite: column index 1 out of range for 1 columns", panicMessage(t, func() { s.ColumnDouble(1) }))
		is.NotError(t, s.Finalize())
		is.Equal(t, "sqlite: statement is finalized", panicMessage(t, func() { s.ColumnDouble(0) }))
	})
}

func TestStmt_ColumnBlob(t *testing.T) {
	t.Run("should read the bytes of a value of another type, an empty blob as an empty slice, and NULL as nil", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select 'a' || char(0) || 'b', 12, 2.5, x'', null, ''")

		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)

		is.Equal(t, "a\x00b", string(s.ColumnBlob(0)))
		is.Equal(t, "12", string(s.ColumnBlob(1)))
		is.Equal(t, "2.5", string(s.ColumnBlob(2)))
		is.Equal(t, sqlite.TypeBlob, s.ColumnType(3))
		empty := s.ColumnBlob(3)
		is.True(t, empty != nil && len(empty) == 0, "the empty blob is not an empty slice")
		is.True(t, s.ColumnBlob(4) == nil, "NULL is not nil")
		is.True(t, s.ColumnBlob(5) != nil && len(s.ColumnBlob(5)) == 0, "the empty text is not an empty slice")
	})

	t.Run("should give a copy that keeps its bytes after the next step and after finalizing", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select * from (values (x'0102'), (x'0304'))")

		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		first := s.ColumnBlob(0)
		ok, err = s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		second := s.ColumnBlob(0)
		is.NotError(t, s.Finalize())

		is.EqualSlice(t, []byte{1, 2}, first)
		is.EqualSlice(t, []byte{3, 4}, second)
	})

	t.Run("should panic without a current row, for an index out of range, and on a finalized statement", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select x'00'")

		is.Equal(t, "sqlite: no current row", panicMessage(t, func() { s.ColumnBlob(0) }))
		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, "sqlite: column index 1 out of range for 1 columns", panicMessage(t, func() { s.ColumnBlob(1) }))
		is.NotError(t, s.Finalize())
		is.Equal(t, "sqlite: statement is finalized", panicMessage(t, func() { s.ColumnBlob(0) }))
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

func TestStmt_ColumnDeclType(t *testing.T) {
	t.Run("should report the declared type of a column, in upper case if it is one of the types of a strict table, and none for an expression or a column without one", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 Int primary key, v2 Varchar ( 10 ), v3) without rowid"))
		is.NotError(t, c.Exec("create table t2 (v1 Int primary key, v2 integer) strict, without rowid"))
		s := prepare(t, c, `select *, v1 as x, v1 + 1 from t1`)
		var types []string
		for i := range s.ColumnCount() {
			types = append(types, s.ColumnDeclType(i))
		}
		s2 := prepare(t, c, `select * from t2`)
		for i := range s2.ColumnCount() {
			types = append(types, s2.ColumnDeclType(i))
		}
		is.EqualSlice(t, []string{"INT", "Varchar ( 10 )", "", "INT", "", "INT", "INTEGER"}, types)
	})

	t.Run("should panic for an index out of range", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select 1")

		is.Equal(t, "sqlite: column index 1 out of range for 1 columns", panicMessage(t, func() { s.ColumnDeclType(1) }))
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

func TestCompileOptions(t *testing.T) {
	t.Run("should report every option set in options.h", func(t *testing.T) {
		b, err := os.ReadFile("options.h")
		is.NotError(t, err)
		defines := regexp.MustCompile(`(?m)^#define SQLITE_(\w+) (\S+)$`).FindAllSubmatch(b, -1)
		is.True(t, len(defines) >= 10, "options.h has too few options; is the pattern above still right?")

		options := sqlite.CompileOptions()
		for _, d := range defines {
			name, value := string(d[1]), string(d[2])
			if name == "WITHOUT_ZONEMALLOC" {
				// SQLite does not report it; TestConn_SetLengthLimit checks what it does.
				continue
			}
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

func TestComplete(t *testing.T) {
	t.Run("should report a statement that ends with a semicolon as complete", func(t *testing.T) {
		is.True(t, sqlite.Complete("select 1;"))
		is.True(t, sqlite.Complete("select 1; -- done\n"))
		is.True(t, sqlite.Complete("create trigger tr after insert on t begin select 1; end;"))
	})

	t.Run("should report a statement without its end as not complete", func(t *testing.T) {
		is.True(t, !sqlite.Complete("select 1"))
		is.True(t, !sqlite.Complete("select ';"))
		is.True(t, !sqlite.Complete("select 1 /* ; */"))
		is.True(t, !sqlite.Complete("create trigger tr after insert on t begin select 1;"))
		is.True(t, !sqlite.Complete("select 1\x00;"))
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

func FuzzStmt_BindDouble(f *testing.F) {
	f.Add(0.0)
	f.Add(math.Copysign(0, -1))
	f.Add(1.5)
	f.Add(math.Inf(1))
	f.Add(math.NaN())
	f.Add(math.MaxFloat64)
	f.Add(math.SmallestNonzeroFloat64)

	f.Fuzz(func(t *testing.T, v float64) {
		c := memory(t)

		// A parameter passes the value through as it is, and a NaN as NULL.
		s := prepare(t, c, "select ?")
		is.NotError(t, s.BindDouble(1, v))
		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		if math.IsNaN(v) {
			is.Equal(t, sqlite.TypeNull, s.ColumnType(0))
			return
		}
		is.Equal(t, sqlite.TypeFloat, s.ColumnType(0))
		is.Equal(t, math.Float64bits(v), math.Float64bits(s.ColumnDouble(0)))

		// Store the value and read it back, so it is not only passed through. Only the sign of -0.0 is
		// lost there.
		is.NotError(t, c.Exec("create table t1 (v1 real) strict"))
		insert := prepare(t, c, "insert into t1 (v1) values (?)")
		is.NotError(t, insert.BindDouble(1, v))
		ok, err = insert.Step()
		is.NotError(t, err)
		is.True(t, !ok)

		sel := prepare(t, c, "select v1, typeof(v1) = 'real' from t1 where v1 = ?")
		is.NotError(t, sel.BindDouble(1, v))
		ok, err = sel.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, sqlite.TypeFloat, sel.ColumnType(0))
		expected := v
		if v == 0 {
			expected = 0
		}
		is.Equal(t, math.Float64bits(expected), math.Float64bits(sel.ColumnDouble(0)))
		is.Equal(t, int64(1), sel.ColumnInt64(1))
	})
}

func FuzzStmt_BindBlob(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0})
	f.Add([]byte("a\x00b"))
	f.Add([]byte{0xff})

	f.Fuzz(func(t *testing.T, v []byte) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 blob primary key) strict, without rowid"))

		// Store the value and read it back, so it is not only passed through.
		insert := prepare(t, c, "insert into t1 (v1) values (?)")
		is.NotError(t, insert.BindBlob(1, v))
		ok, err := insert.Step()
		is.NotError(t, err)
		is.True(t, !ok)

		sel := prepare(t, c, "select v1, length(v1) from t1 where v1 = ?")
		is.NotError(t, sel.BindBlob(1, v))
		ok, err = sel.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, sqlite.TypeBlob, sel.ColumnType(0))
		got := sel.ColumnBlob(0)
		is.True(t, got != nil, "the blob is nil")
		is.True(t, bytes.Equal(v, got), "the blob is not what was bound")
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
		// change settings for the whole process are left out.
		lower := strings.ToLower(sql)
		if strings.Contains(lower, "heap_limit") || strings.Contains(lower, "directory") {
			return
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
// as its type and its value: an integer in decimal, a real as the shortest decimal that reads back as
// the same bits, so -0.0 as -0, a text quoted by Go, and a blob in hexadecimal.
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
			case sqlite.TypeFloat:
				values = append(values, "float:"+strconv.FormatFloat(s.ColumnDouble(i), 'g', -1, 64))
			case sqlite.TypeText:
				values = append(values, fmt.Sprintf("text:%q", s.ColumnText(i)))
			case sqlite.TypeBlob:
				values = append(values, fmt.Sprintf("blob:%x", s.ColumnBlob(i)))
			case sqlite.TypeNull:
				values = append(values, "null:")
			default:
				t.Fatalf("value of type %v in column %v", typ, i)
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

// offset of a [*sqlite.Error], failing the test for any other error.
func offset(t *testing.T, err error) int {
	t.Helper()
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		t.Fatalf("expected *sqlite.Error, got %T: %v", err, err)
	}
	return sqliteErr.Offset
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
