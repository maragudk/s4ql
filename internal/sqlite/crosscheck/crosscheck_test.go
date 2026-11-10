package crosscheck_test

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sqlite"
)

func TestLink(t *testing.T) {
	t.Run("should link the binding and mattn/go-sqlite3 into one binary as two separate copies of SQLite", func(t *testing.T) {
		// The two copies are built with different options, so each answers with its own. If calls from
		// one binding reached the other copy, they would agree.
		c := binding(t)
		is.Equal(t, "integer:1", bindingQuery(t, c, "pragma foreign_keys")[0])
		is.True(t, slices.Contains(sqlite.CompileOptions(), "DQS=0"))

		m := mattn(t)
		is.Equal(t, "integer:0", mattnQuery(t, m, "pragma foreign_keys")[0])
		var options []string
		rows, err := m.QueryContext(t.Context(), "pragma compile_options")
		is.NotError(t, err)
		for rows.Next() {
			var o string
			is.NotError(t, rows.Scan(&o))
			options = append(options, o)
		}
		is.NotError(t, rows.Err())
		is.True(t, !slices.Contains(options, "DQS=0"))
		is.True(t, !slices.Equal(options, sqlite.CompileOptions()))
	})
}

func TestVersion(t *testing.T) {
	t.Run("should be the same SQLite version and source ID in the binding and mattn/go-sqlite3", func(t *testing.T) {
		version, _, sourceID := sqlite3.Version()
		is.Equal(t, sqlite.Version(), version)
		is.Equal(t, sqlite.SourceID(), sourceID)

		var sqlVersion, sqlSourceID string
		m := mattn(t)
		is.NotError(t, m.QueryRowContext(t.Context(), "select sqlite_version(), sqlite_source_id()").Scan(&sqlVersion, &sqlSourceID))
		is.Equal(t, sqlite.Version(), sqlVersion)
		is.Equal(t, sqlite.SourceID(), sqlSourceID)
	})
}

func TestSource(t *testing.T) {
	t.Run("should be the same C code in the binding and mattn/go-sqlite3, apart from mattn's wrapper", func(t *testing.T) {
		out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/mattn/go-sqlite3").Output()
		is.NotError(t, err)
		dir := strings.TrimSpace(string(out))

		for vendored, bundled := range map[string]string{"sqlite3.c": "sqlite3-binding.c", "sqlite3.h": "sqlite3-binding.h"} {
			ours, err := os.ReadFile(filepath.Join("..", "c", vendored))
			is.NotError(t, err)
			theirs, err := os.ReadFile(filepath.Join(dir, bundled))
			is.NotError(t, err)

			// mattn wraps the amalgamation in #ifndef USE_LIBSQLITE3, so that it can be switched off.
			theirs, ok := bytes.CutPrefix(theirs, []byte("#ifndef USE_LIBSQLITE3\n"))
			is.True(t, ok, bundled+" prefix")
			i := bytes.LastIndex(theirs, []byte("#else // USE_LIBSQLITE3\n"))
			is.True(t, i >= 0, bundled+" suffix")
			is.True(t, bytes.Equal(ours, theirs[:i]), vendored+" differs from "+bundled)
		}
	})
}

func TestBindInt64(t *testing.T) {
	for _, v := range []int64{0, 1, -1, math.MaxInt64, math.MinInt64, math.MaxInt32 + 1} {
		t.Run(fmt.Sprintf("should bind and read %v as mattn/go-sqlite3 does", v), func(t *testing.T) {
			compareBindInt64(t, v)
		})
	}

	t.Run("should bind several numbered and named parameters by index as mattn/go-sqlite3 does", func(t *testing.T) {
		query := "select ?2, :a, ?1, ?2 + ?1"

		c := binding(t)
		s, err := c.Prepare(query)
		is.NotError(t, err)
		t.Cleanup(func() { is.NotError(t, s.Finalize()) })
		for i, v := range []int64{10, 20, 30} {
			is.NotError(t, s.BindInt64(i+1, v))
		}
		expected := bindingRows(t, s)

		m := mattn(t)
		rows, err := m.QueryContext(t.Context(), query, int64(10), int64(20), int64(30))
		is.NotError(t, err)
		is.EqualSlice(t, expected, mattnRows(t, rows))
		is.EqualSlice(t, []string{"integer:20,integer:30,integer:10,integer:30"}, expected)
	})
}

func FuzzBindInt64(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(math.MaxInt64))
	f.Add(int64(math.MinInt64))

	f.Fuzz(func(t *testing.T, v int64) {
		compareBindInt64(t, v)
	})
}

// compareBindInt64 binds v as a parameter in both bindings and compares what they read back.
func compareBindInt64(t *testing.T, v int64) {
	t.Helper()
	query := "select ?1, typeof(?1) = 'integer', ?1 + 0 = ?1"

	c := binding(t)
	s, err := c.Prepare(query)
	is.NotError(t, err)
	t.Cleanup(func() { is.NotError(t, s.Finalize()) })
	is.NotError(t, s.BindInt64(1, v))
	expected := bindingRows(t, s)

	m := mattn(t)
	rows, err := m.QueryContext(t.Context(), query, v)
	is.NotError(t, err)
	is.EqualSlice(t, expected, mattnRows(t, rows))
	is.EqualSlice(t, []string{fmt.Sprintf("integer:%v,integer:1,integer:1", v)}, expected)
}

func TestSlice1(t *testing.T) {
	// slice1Scripts are sequences of the statements in slice 1 of docs/roadmap.md, including the errors
	// they can produce, run on a fresh database in each binding.
	slice1Scripts := map[string][]string{
		"the slice 1 example": {
			"create table t1 (v1 int primary key) strict, without rowid",
			"select * from t1",
			"insert into t1 (v1) values (1)",
			"select * from t1",
		},
		"several rows in primary-key order": {
			"create table t1 (v1 int primary key) strict, without rowid",
			"insert into t1 (v1) values (3)",
			"insert into t1 (v1) values (-9223372036854775808)",
			"insert into t1 (v1) values (9223372036854775807)",
			"insert into t1 (v1) values (0)",
			"select * from t1",
		},
		"several tables with other names": {
			"create table a (b int primary key) strict, without rowid",
			"create table t2 (v2 int primary key) strict, without rowid",
			"insert into a (b) values (1)",
			"insert into t2 (v2) values (2)",
			"select * from a",
			"select * from t2",
		},
		"errors": {
			"select * from t1",
			"insert into t1 (v1) values (1)",
			"create table t1 (v1 int primary key) strict, without rowid",
			"create table t1 (v1 int primary key) strict, without rowid",
			"insert into t1 (v1) values (1)",
			"insert into t1 (v1) values (1)",
			"insert into t1 (v1) values (9223372036854775808)",
			"select * from t1",
		},
	}

	for name, script := range slice1Scripts {
		t.Run("should give the same results and errors as mattn/go-sqlite3 for "+name, func(t *testing.T) {
			compareScript(t, script)
		})
	}

	t.Run("should give the errors of slice 1, so that the comparison above compares errors", func(t *testing.T) {
		c := binding(t)
		var outcomes []string
		for _, statement := range slice1Scripts["errors"] {
			outcomes = append(outcomes, strings.Join(bindingOutcome(t, c, statement), ";"))
		}
		is.EqualSlice(t, []string{
			"error 1: no such table: t1",
			"error 1: no such table: t1",
			"",
			"error 1: table t1 already exists",
			"",
			"error 1555: UNIQUE constraint failed: t1.v1",
			"error 3091: cannot store REAL value in INT column t1.v1",
			"v1;integer:1",
		}, outcomes)
	})
}

func FuzzSlice1(f *testing.F) {
	// The slice 1 example: create t0, select, insert 1, select.
	f.Add([]byte{0, 0, 2, 0, 1, 0, 1, 2, 0})
	// The error script: select and insert on a missing table, create twice, a duplicate key, a value
	// too large for an int column, and a select.
	f.Add([]byte{2, 0, 1, 0, 1, 0, 0, 0, 0, 1, 0, 1, 1, 0, 1, 1, 0, 6, 2, 0})
	// Several tables, with the largest and smallest values and a random one.
	f.Add([]byte{0, 0, 0, 1, 1, 0, 4, 1, 1, 5, 1, 0, 7, 1, 2, 3, 2, 0, 2, 1, 2, 2})

	f.Fuzz(func(t *testing.T, data []byte) {
		// Each statement is one byte for its kind and one for its table, and for an insert, one byte
		// that picks its value from a small set and, for a random value, eight bytes more. The small
		// sets make errors for existing and missing tables and duplicate keys likely.
		values := []string{"0", "1", "-1", "2", fmt.Sprint(math.MaxInt64), fmt.Sprint(math.MinInt64), "9223372036854775808"}
		var script []string
		for len(data) >= 2 && len(script) < 50 {
			kind, table := data[0]%3, data[1]%3
			data = data[2:]
			switch kind {
			case 0:
				script = append(script, fmt.Sprintf("create table t%v (v%v int primary key) strict, without rowid", table, table))
			case 1:
				if len(data) < 1 {
					data = nil
					continue
				}
				i := int(data[0]) % (len(values) + 1)
				data = data[1:]
				var v string
				if i < len(values) {
					v = values[i]
				} else {
					if len(data) < 8 {
						data = nil
						continue
					}
					v = fmt.Sprint(int64(binary.LittleEndian.Uint64(data)))
					data = data[8:]
				}
				script = append(script, fmt.Sprintf("insert into t%v (v%v) values (%v)", table, table, v))
			case 2:
				script = append(script, fmt.Sprintf("select * from t%v", table))
			}
		}
		compareScript(t, script)
	})
}

// compareScript runs each statement in both bindings, on a fresh database each, and compares the
// outcome of every statement: the column names and rows, or the error.
func compareScript(t *testing.T, script []string) {
	t.Helper()
	c := binding(t)
	m := mattn(t)

	for _, statement := range script {
		expected := bindingOutcome(t, c, statement)

		var actual []string
		if strings.HasPrefix(statement, "select") {
			rows, err := m.QueryContext(t.Context(), statement)
			if err != nil {
				actual = mattnError(t, err)
			} else {
				columns, err := rows.Columns()
				is.NotError(t, err)
				actual = append([]string{strings.Join(columns, ",")}, mattnRows(t, rows)...)
			}
		} else {
			_, err := m.ExecContext(t.Context(), statement)
			if err != nil {
				actual = mattnError(t, err)
			}
		}

		is.EqualSlice(t, expected, actual, statement)
	}
}

// bindingOutcome of a statement: for a statement with results, the column names followed by the
// rows encoded by [bindingRows], and for an error, its extended code and message.
func bindingOutcome(t *testing.T, c *sqlite.Conn, statement string) []string {
	t.Helper()
	s, err := c.Prepare(statement)
	if err != nil {
		return bindingError(t, err)
	}
	defer func() { _ = s.Finalize() }()

	var out []string
	if n := s.ColumnCount(); n > 0 {
		var names []string
		for i := range n {
			names = append(names, s.ColumnName(i))
		}
		out = append(out, strings.Join(names, ","))
	}

	for {
		ok, err := s.Step()
		if err != nil {
			return bindingError(t, err)
		}
		if !ok {
			return out
		}
		out = append(out, bindingRow(t, s))
	}
}

func bindingError(t *testing.T, err error) []string {
	t.Helper()
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		t.Fatalf("expected *sqlite.Error, got %T: %v", err, err)
	}
	return []string{fmt.Sprintf("error %v: %v", sqliteErr.Code, sqliteErr.Message)}
}

func mattnError(t *testing.T, err error) []string {
	t.Helper()
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) {
		t.Fatalf("expected sqlite3.Error, got %T: %v", err, err)
	}
	return []string{fmt.Sprintf("error %v: %v", int(sqliteErr.ExtendedCode), sqliteErr.Error())}
}

// binding connection to a new in-memory database, closed when the test ends.
func binding(t *testing.T) *sqlite.Conn {
	t.Helper()
	c, err := sqlite.Open(":memory:")
	is.NotError(t, err)
	t.Cleanup(func() { is.NotError(t, c.Close()) })
	return c
}

// mattn connection to a new in-memory database, closed when the test ends. It is a single
// connection, because each connection to ":memory:" has its own database.
func mattn(t *testing.T) *sql.Conn {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	is.NotError(t, err)
	t.Cleanup(func() { is.NotError(t, db.Close()) })
	c, err := db.Conn(t.Context())
	is.NotError(t, err)
	t.Cleanup(func() { is.NotError(t, c.Close()) })
	return c
}

func bindingQuery(t *testing.T, c *sqlite.Conn, query string) []string {
	t.Helper()
	s, err := c.Prepare(query)
	is.NotError(t, err)
	defer func() { is.NotError(t, s.Finalize()) }()
	return bindingRows(t, s)
}

func mattnQuery(t *testing.T, c *sql.Conn, query string) []string {
	t.Helper()
	rows, err := c.QueryContext(t.Context(), query)
	is.NotError(t, err)
	return mattnRows(t, rows)
}

// bindingRows of a statement, stepped to the end, encoded by [bindingRow].
func bindingRows(t *testing.T, s *sqlite.Stmt) []string {
	t.Helper()
	var rows []string
	for {
		ok, err := s.Step()
		is.NotError(t, err)
		if !ok {
			return rows
		}
		rows = append(rows, bindingRow(t, s))
	}
}

// bindingRow encodes the current row as its values joined by commas, each as its type and, for an
// integer, its decimal value.
func bindingRow(t *testing.T, s *sqlite.Stmt) string {
	t.Helper()
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
	return strings.Join(values, ",")
}

// mattnRows encodes rows as [bindingRow] does, from the Go types that mattn/go-sqlite3 returns for
// SQLite's types.
func mattnRows(t *testing.T, rows *sql.Rows) []string {
	t.Helper()
	defer func() { is.NotError(t, rows.Close()) }()

	columns, err := rows.Columns()
	is.NotError(t, err)

	var out []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		is.NotError(t, rows.Scan(pointers...))

		var encoded []string
		for i, v := range values {
			switch v := v.(type) {
			case int64:
				encoded = append(encoded, fmt.Sprint("integer:", v))
			case nil:
				encoded = append(encoded, "null:")
			default:
				t.Fatalf("value of Go type %T in column %v, which the binding cannot read", v, i)
			}
		}
		out = append(out, strings.Join(encoded, ","))
	}
	is.NotError(t, rows.Err())
	return out
}
