package sqlite_test

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sqlite"
)

// Result codes from https://sqlite.org/rescode.html, used in the tests below.
const (
	codeError              = 1
	codeBusy               = 5
	codeCantOpen           = 14
	codeConstraint         = 19
	codeMisuse             = 21
	codeRange              = 25
	codeConstraintForeign  = 787
	codeConstraintPrimary  = 1555
	codeConstraintDataType = 3091
)

// expectedOptions are the options set in options.h, as SQLite reports them.
var expectedOptions = []string{
	"DEFAULT_FOREIGN_KEYS",
	"DQS=0",
	"ENABLE_API_ARMOR",
	"OMIT_LOAD_EXTENSION",
	"OMIT_SHARED_CACHE",
	"THREADSAFE=1",
}

func TestOpen(t *testing.T) {
	t.Run("should open and close an in-memory database", func(t *testing.T) {
		c, err := sqlite.Open(":memory:")
		is.NotError(t, err)
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
		is.Equal(t, codeCantOpen, code(t, err))
		is.Equal(t, "unable to open database file", message(t, err))
	})
}

func TestConn_Close(t *testing.T) {
	t.Run("should fail with SQLITE_BUSY while a statement is not finalized, and succeed after", func(t *testing.T) {
		c, err := sqlite.Open(":memory:")
		is.NotError(t, err)

		s := prepare(t, c, "select 1")

		err = c.Close()
		is.Equal(t, codeBusy, code(t, err))

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
		is.Equal(t, `sqlite: near "selec": syntax error (code 1)`, err.Error())
	})

	t.Run("should allow trailing whitespace and a semicolon", func(t *testing.T) {
		c := memory(t)

		s, err := c.Prepare("select 1; \t\n")
		is.NotError(t, err)
		is.NotError(t, s.Finalize())
	})

	t.Run("should refuse more than one statement", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))

		_, err := c.Prepare("select 1; drop table t1")
		is.Equal(t, `sqlite: SQL continues after the first statement: " drop table t1"`, err.Error())

		is.EqualSlice(t, []string{"integer:1"}, query(t, c, "select count(*) from sqlite_schema"))
	})

	t.Run("should refuse SQL after a NUL byte instead of ignoring it", func(t *testing.T) {
		c := memory(t)

		_, err := c.Prepare("select 1\x00; drop table t1")
		is.Equal(t, `sqlite: SQL continues after the first statement: "\x00; drop table t1"`, err.Error())
	})

	t.Run("should refuse SQL without a statement", func(t *testing.T) {
		c := memory(t)

		for _, sql := range []string{"", " ", "-- comment", ";"} {
			_, err := c.Prepare(sql)
			is.Equal(t, "sqlite: no statement in SQL", fmt.Sprint(err), sql)
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
		is.Equal(t, sqlite.ResultCode(codeConstraint), sqliteErr.Code.Primary())
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

	t.Run("should fail with SQLITE_RANGE for a parameter index out of range", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select ?")

		is.Equal(t, codeRange, code(t, s.BindInt64(0, 1)))
		err := s.BindInt64(2, 1)
		is.Equal(t, codeRange, code(t, err))
		is.Equal(t, "column index out of range", message(t, err))
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

func TestStmt_Column(t *testing.T) {
	t.Run("should report the column count and names", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))
		s := prepare(t, c, `select v1, v1 as "my name", 1 + 1 from t1`)

		is.Equal(t, 3, s.ColumnCount())
		is.Equal(t, "v1", s.ColumnName(0))
		is.Equal(t, "my name", s.ColumnName(1))
		is.Equal(t, "1 + 1", s.ColumnName(2))
		is.Equal(t, "", s.ColumnName(3))
		is.Equal(t, "", s.ColumnName(-1))
	})

	t.Run("should report a zero column count for a statement without results", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "create table t1 (v1 int primary key) strict, without rowid")

		is.Equal(t, 0, s.ColumnCount())
	})

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

	t.Run("should read NULL for a column index out of range", func(t *testing.T) {
		c := memory(t)
		s := prepare(t, c, "select 1")

		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, sqlite.TypeNull, s.ColumnType(1))
		is.Equal(t, int64(0), s.ColumnInt64(1))
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
	})

	t.Run("should fail with SQLITE_CONSTRAINT_DATATYPE for an integer literal too large for an int column", func(t *testing.T) {
		c := memory(t)
		is.NotError(t, c.Exec("create table t1 (v1 int primary key) strict, without rowid"))

		err := c.Exec("insert into t1 (v1) values (9223372036854775808)")
		is.Equal(t, codeConstraintDataType, code(t, err))
		is.Equal(t, "cannot store REAL value in INT column t1.v1", message(t, err))
	})
}

func TestStmt_Finalize(t *testing.T) {
	t.Run("should be safe to call twice, and make other calls return SQLITE_MISUSE or nothing", func(t *testing.T) {
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

		is.Equal(t, 0, s.ColumnCount())
		is.Equal(t, "", s.ColumnName(0))
		is.Equal(t, sqlite.TypeNull, s.ColumnType(0))
		is.Equal(t, int64(0), s.ColumnInt64(0))
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
	t.Run("should include every option set in options.h", func(t *testing.T) {
		options := sqlite.CompileOptions()
		for _, o := range expectedOptions {
			is.True(t, slices.Contains(options, o), "missing "+o)
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

	t.Run("should still read a double-quoted DEFAULT as a string, which the grammar does for any identifier there", func(t *testing.T) {
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
		is.Equal(t, codeConstraintForeign, code(t, err))
		is.Equal(t, "FOREIGN KEY constraint failed", message(t, err))
	})

	t.Run("should not have the load_extension function", func(t *testing.T) {
		c := memory(t)

		_, err := c.Prepare("select load_extension('x')")
		is.Equal(t, codeError, code(t, err))
		is.Equal(t, "no such function: load_extension", message(t, err))
	})
}

func TestVendored(t *testing.T) {
	t.Run("should have vendored files with the checksums recorded in source.json", func(t *testing.T) {
		m := readManifest(t)
		is.Equal(t, 4, len(m.Files))

		for name, sum := range m.Files {
			b, err := os.ReadFile(filepath.Join("c", name))
			is.NotError(t, err)
			got := sha256.Sum256(b)
			is.Equal(t, sum, hex.EncodeToString(got[:]), name)
		}
	})
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

// shellCases are run by both the binding and the sqlite3 shell, and must give the same results. They
// follow slice 1 of docs/roadmap.md, plus queries for the version and the compile options.
var shellCases = []struct {
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
		name:  "the version",
		query: "select sqlite_version() = '" + sqlite.Version() + "', sqlite_source_id() = '" + sqlite.SourceID() + "'",
	},
	{
		name:  "the number of compile options",
		query: "select count(*) from pragma_compile_options",
	},
	{
		name:  "foreign keys on a new connection",
		query: "pragma foreign_keys",
	},
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

	for _, test := range shellCases {
		t.Run("should give the same results as the binding for "+test.name, func(t *testing.T) {
			c := memory(t)
			for _, sql := range test.setup {
				is.NotError(t, c.Exec(sql))
			}
			s := prepare(t, c, test.query)
			expected := rows(t, s)

			// The shell prints each value in the encoding of the rows helper, computed in SQL, so that
			// the comparison does not depend on how the shell formats values.
			var columns, encoded []string
			for i := range s.ColumnCount() {
				col := fmt.Sprintf("c%v", i)
				columns = append(columns, col)
				encoded = append(encoded, fmt.Sprintf("typeof(%[1]v) || ':' || case typeof(%[1]v) when 'integer' then %[1]v else hex(%[1]v) end", col))
			}
			query := test.query
			if strings.HasPrefix(query, "pragma ") {
				query = "select * from pragma_" + strings.TrimPrefix(query, "pragma ")
			}
			script := strings.Join(test.setup, ";\n")
			script += fmt.Sprintf(";\nwith q (%v) as (%v) select %v from q;\n",
				strings.Join(columns, ", "), query, strings.Join(encoded, " || ',' || "))

			out := shellRun(t, script)
			if len(expected) == 0 {
				is.EqualSlice(t, []string{""}, out)
				return
			}
			is.EqualSlice(t, expected, out)
		})
	}

	t.Run("should fail with the same messages as the binding for the errors of slice 1", func(t *testing.T) {
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

		for _, test := range tests {
			c := memory(t)
			for _, sql := range test.setup {
				is.NotError(t, c.Exec(sql))
			}
			msg := message(t, c.Exec(test.sql))

			script := strings.Join(append(test.setup, test.sql), ";\n") + ";\n"
			_, stderr, err := shellExec(t, script)
			is.True(t, err != nil, "shell exit status for "+test.sql)
			is.True(t, strings.Contains(stderr, msg), fmt.Sprintf("shell error %q does not contain %q", stderr, msg))
		}
	})
}

func FuzzStmt_BindInt64(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(math.MaxInt64))
	f.Add(int64(math.MinInt64))
	f.Add(int64(-1))

	c, err := sqlite.Open(":memory:")
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = c.Close() })
	if err := c.Exec("create table t1 (v1 int primary key) strict, without rowid"); err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, v int64) {
		// Store the value and read it back, so it is not only passed through.
		insert := prepare(t, c, "insert into t1 (v1) values (?)")
		is.NotError(t, insert.BindInt64(1, v))
		// A value seen before fails as a duplicate key, and then finalizing reports the same error.
		_, err := insert.Step()
		if err != nil {
			is.Equal(t, codeConstraintPrimary, code(t, err))
			is.Equal(t, codeConstraintPrimary, code(t, insert.Finalize()))
		} else {
			is.NotError(t, insert.Finalize())
		}

		sel := prepare(t, c, "select v1, typeof(v1) = 'integer' from t1 where v1 = ?")
		is.NotError(t, sel.BindInt64(1, v))
		ok, err := sel.Step()
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

func FuzzConn_Prepare(f *testing.F) {
	f.Add("select 1")
	f.Add("select ?, :a, @b, $c, ?5")
	f.Add("select 1; select 2")
	f.Add("select \x00 1")
	f.Add("create table t2 (v2 int primary key) strict, without rowid")
	f.Add("insert into t1 (v1) values (1)")
	f.Add("/* unterminated")
	f.Add("select 'unterminated")
	f.Add("")

	c, err := sqlite.Open(":memory:")
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = c.Close() })
	if err := c.Exec("create table t1 (v1 int primary key) strict, without rowid"); err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, sql string) {
		// Statements are prepared and inspected, not stepped, so that fuzzed SQL cannot run for long
		// or change the schema.
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

		for i := -1; i <= s.ColumnCount(); i++ {
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
	t.Cleanup(func() { is.NotError(t, s.Finalize()) })
	return s
}

// query with a single statement and return its rows encoded by [rows].
func query(t *testing.T, c *sqlite.Conn, sql string) []string {
	t.Helper()
	s := prepare(t, c, sql)
	return rows(t, s)
}

// rows of a statement, stepped to the end, each encoded as its values joined by commas, and each value
// as its type and, for an integer, its decimal value. A value that is not an integer or NULL is
// encoded by type alone, because the binding reads only integers.
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

var (
	shellOnce sync.Once
	shellDir  string
	shellPath string
	shellErr  error
)

// TestMain removes the sqlite3 shell built by [shell], if any.
func TestMain(m *testing.M) {
	code := m.Run()
	if shellDir != "" {
		_ = os.RemoveAll(shellDir)
	}
	os.Exit(code)
}

// shell builds the official sqlite3 shell from the vendored source, with the compile options in
// options.h and the C compiler that cgo uses, once per test binary, and returns its path.
func shell(t *testing.T) string {
	t.Helper()
	shellOnce.Do(func() {
		shellDir, shellErr = os.MkdirTemp("", "s4ql-sqlite3-")
		if shellErr != nil {
			return
		}
		shellPath = filepath.Join(shellDir, "sqlite3")

		cc, err := exec.Command("go", "env", "CC").Output()
		if err != nil {
			shellErr = err
			return
		}
		args := strings.Fields(string(cc))
		args = append(args, "-O2", "-include", "options.h", "-Ic", "-o", shellPath, "c/shell.c", "c/sqlite3.c", "-lm", "-lpthread")
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			shellErr = fmt.Errorf("%v: %w\n%s", strings.Join(args, " "), err, out)
		}
	})
	if shellErr != nil {
		t.Fatal("building the sqlite3 shell:", shellErr)
	}
	return shellPath
}

// shellExec runs the sqlite3 shell on an in-memory database with the given script on stdin.
func shellExec(t *testing.T, script string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(shell(t), "-batch", "-bail", "-noheader", "-list", ":memory:")
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
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
