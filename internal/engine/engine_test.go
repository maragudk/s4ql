package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
	"maragu.dev/s4ql/internal/store/simstore"
)

const (
	t1 = "create table t1 (v1 int primary key) strict, without rowid"
	// t3 is a table of three columns, of three types, with the key first.
	t3 = "create table t3 (v1 int primary key, v2 text not null, v3 real not null) strict, without rowid"
)

// manyColumns of a create table: n columns, the first the primary key, and every other NOT NULL.
func manyColumns(n int) string {
	cs := []string{"c0 int primary key"}
	for i := 1; i < n; i++ {
		cs = append(cs, fmt.Sprint("c", i, " int not null"))
	}
	return strings.Join(cs, ", ")
}

// pin runs the statements of setup and then src, with args for its parameters, on a new instance of
// the engine and on SQLite, and requires the same answer of both, and the answer want: an error as
// "code/extended: message", or "columns ...; rows ..." for a query, or "" for a statement that
// changes the database. After a statement that is no query, the state of both must be the same.
func pin(t *testing.T, setup []string, src, want string, args ...any) {
	t.Helper()
	i := newInstance(t, engine.Options{})
	o := newOracle(t)
	for _, s := range setup {
		is.Equal(t, "", i.exec(s).err, s)
		is.Equal(t, "", o.run(t, s).err, s)
	}
	query := strings.Contains(strings.ToLower(src), "select")
	var got answer
	if query {
		got = i.queryValues(src, values(args))
	} else {
		got = i.execValues(src, values(args))
	}
	sqlite := o.run(t, src, oracleArgs(args)...)
	if !query {
		sqlite.columns, sqlite.rows = "", nil
		is.Equal(t, o.state(t), i.state(), "SQLite and the engine")
	}
	is.Equal(t, sqlite.String(), got.String(), "SQLite and the engine")
	text := got.err
	if got.err == "" && query {
		text = fmt.Sprintf("columns %v; rows %v", got.columns, strings.Join(got.rows, " "))
	}
	is.Equal(t, want, text)
}

func TestEngine_errors(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		src   string
		want  string
	}{
		{name: "should give that a table exists, with its name as written", setup: []string{t1},
			src: `create table "T1" (x int primary key) strict, without rowid`, want: `1/1: table "T1" already exists`},
		{name: "should give that a table exists for a name in another case", setup: []string{t1},
			src: "CREATE TABLE T1 (x int primary key) without rowid, strict", want: "1/1: table T1 already exists"},
		{name: "should give the syntax error when no table of the name exists", setup: []string{t1},
			src: "create table t2 (v1 int primary key) strict without rowid", want: `1/1: near "without": syntax error`},
		{name: "should give that there is no table to read, with the name without its quotes",
			src: `select * from "T""2"`, want: `1/1: no such table: T"2`},
		{name: "should give that there is no table to write",
			src: "insert into T2 (v1) values (1)", want: "1/1: no such table: T2"},
		{name: "should give that there is no table to drop",
			src: `drop table "t 2"`, want: "1/1: no such table: t 2"},
		{name: "should give that there is no table before that there is no column",
			src: "insert into t2 (v2) values (1)", want: "1/1: no such table: t2"},
		{name: "should give that there is no column, with the names as the statement writes them", setup: []string{t1},
			src: `insert into "T1" ("V""2") values (1)`, want: `1/1: table T1 has no column named V"2`},
		{name: "should give that a key exists, with the names as the table stores them",
			setup: []string{`create table "Foo bar" ("Bar""x" int primary key) strict, without rowid`, `insert into "foo BAR" ("bar""X") values (-0)`},
			src:   `insert into "FOO BAR" ("BAR""X") values (0)`, want: `19/1555: UNIQUE constraint failed: Foo bar.Bar"x`},
		{name: "should take names of letters outside ASCII in another case for other names",
			setup: []string{`create table "été" (v int primary key) strict, without rowid`},
			src:   `insert into "ÉTÉ" (v) values (1)`, want: "1/1: no such table: ÉTÉ"},
		{name: "should give the rows in the order of the key, with the column as created",
			setup: []string{"create table t (Kéy int primary key) strict, without rowid", "insert into t (kéy) values (3)", "insert into t (kéy) values (-9223372036854775808)", "insert into t (kéy) values (9223372036854775807)", "insert into t (kéy) values (0)"},
			src:   "select * from T", want: "columns Kéy INT; rows -9223372036854775808 0 3 9223372036854775807"},
		{name: "should give no rows for an empty table", setup: []string{t1},
			src: "select * from t1", want: "columns v1 INT; rows "},
		{name: "should give the syntax error of SQLite",
			src: "selec * from t1", want: `1/1: near "selec": syntax error`},
		{name: "should drop a table that a name in another case names", setup: []string{t1, "insert into t1 (v1) values (1)"},
			src: "drop table T1", want: ""},
		{name: "should give that a column is named twice, with the name of the later column",
			src: `create table t (v int primary key, w text not null, "V" real not null) strict, without rowid`, want: "1/1: duplicate column name: V"},
		{name: "should give that a table exists before that a column is named twice", setup: []string{t3},
			src: "create table T3 (v int primary key, v text not null) strict, without rowid", want: "1/1: table T3 already exists"},
		{name: "should give that a table has more columns than SQLite allows",
			src: "create table t (" + manyColumns(2001) + ") strict, without rowid", want: "1/1: too many columns on t"},
		{name: "should give the number of columns and of values of an insert without a list", setup: []string{t3},
			src: "insert into T3 values (1, 'a')", want: "1/1: table T3 has 3 columns but 2 values were supplied"},
		{name: "should give the number of values and of columns of an insert with a list", setup: []string{t3},
			src: "insert into t3 (v3, v1) values (1.5, 1, 'a')", want: "1/1: 3 values for 2 columns"},
		{name: "should give that there is no column before the number of values", setup: []string{t3},
			src: "insert into t3 (v1, x) values (1)", want: "1/1: table t3 has no column named x"},
		{name: "should give that a column has no value, with the names as the table stores them", setup: []string{t3},
			src: "insert into T3 (V3, V1) values (1.5, 1)", want: "19/1299: NOT NULL constraint failed: t3.v2"},
		{name: "should give that the key has no value", setup: []string{t3},
			src: "insert into t3 (v2, v3) values ('a', 1.5)", want: "19/1299: NOT NULL constraint failed: t3.v1"},
		{name: "should give the first column without a value, in the order of the table", setup: []string{t3},
			src: "insert into t3 (v1) values (1)", want: "19/1299: NOT NULL constraint failed: t3.v2"},
		{name: "should give that a column has no value before that a key exists", setup: []string{t3, "insert into t3 values (1, 'a', 1.5)"},
			src: "insert into t3 (v1, v2) values (1, 'a')", want: "19/1299: NOT NULL constraint failed: t3.v3"},
		{name: "should give that a column has no value before the type of another", setup: []string{t3},
			src: "insert into t3 (v1, v3) values (1, 'a')", want: "19/1299: NOT NULL constraint failed: t3.v2"},
		{name: "should give that a key exists for a key at any position", setup: []string{"create table k (a text not null, b int primary key) strict, without rowid", "insert into k values ('x', 1)"},
			src: "insert into k (b, a) values (1, 'y')", want: "19/1555: UNIQUE constraint failed: k.b"},
		{name: "should give that a column of a select does not exist, the first in the list", setup: []string{t3},
			src: "select v1, x, y from t3", want: "1/1: no such column: x"},
		{name: "should give that a column in double quotes does not exist, as SQLite does with DQS=0", setup: []string{t3},
			src: `select "V""1" from t3`, want: `1/1: no such column: "V"1" - should this be a string literal in single-quotes?`},
		{name: "should give that there is no table before that there is no column of a select",
			src: "select x from t3", want: "1/1: no such table: t3"},
		{name: "should give the columns of a select in its order, with repeats and the names as the table stores them",
			setup: []string{t3, "insert into t3 values (2, 'b', 2.5)", "insert into t3 (v3, v2, v1) values (1.5, 'a', 1)"},
			src:   "select V3, v1, v3 from T3", want: `columns v3 REAL, v1 INT, v3 REAL; rows real 1.5,1,real 1.5 real 2.5,2,real 2.5`},
		{name: "should give every column of * in the order of the table, and the rows in the order of a key that is not the first column",
			setup: []string{"create table k (a text not null, b int primary key) strict, without rowid", "insert into k values ('x', 2)", "insert into k values ('y', 1)"},
			src:   "select * from k", want: `columns a TEXT, b INT; rows "y",1 "x",2`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pin(t, c.setup, c.src, c.want)
		})
	}

	t.Run("should give the syntax error that SQLite gives on an empty database, after the start of a table that exists", func(t *testing.T) {
		for _, src := range []string{"create table t1 (v1 int primary key) strict without rowid", "create table main.T1 as selec"} {
			i := newInstance(t, engine.Options{})
			o := newOracle(t)
			want := o.run(t, src).err
			is.True(t, strings.HasSuffix(want, ": syntax error"), want)
			is.Equal(t, "", i.exec(t1).err)
			is.Equal(t, "", o.run(t, t1).err)
			is.Equal(t, want, i.exec(src).err, src)
			is.Equal(t, want, i.query(src).err, src)
			// SQLite gives that the table exists, which is the one difference between SQLite and s4ql
			// that the tests accept.
			is.True(t, strings.HasSuffix(o.run(t, src).err, " already exists"), src)
		}
	})

	t.Run("should give the syntax error on a closed instance and with a context that has ended", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(t1).err)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		for _, src := range []string{"create table t1 (v1 int primary key) strict without rowid", "selec * from t1", "insert into t1 (v1) values ("} {
			want := newOracle(t).run(t, src).err
			_, err := i.c.Exec(ctx, src)
			is.Equal(t, want, errText(t, src, err), src)
			_, err = i.c.Query(ctx, src)
			is.Equal(t, want, errText(t, src, err), src)
		}
		i.close()
		for _, src := range []string{"create table T1 (", "selec * from t1"} {
			want := newOracle(t).run(t, src).err
			_, err := i.c.Exec(t.Context(), src)
			is.Equal(t, want, errText(t, src, err), src)
			_, err = i.c.Query(ctx, src)
			is.Equal(t, want, errText(t, src, err), src)
		}
	})

	t.Run("should give not supported, and no statement, as errors of their own", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		_, err := i.c.Exec(t.Context(), "select * from t1 order by v1")
		is.True(t, errors.Is(err, stmt.ErrNotSupported))
		var se *sqlerr.Error
		is.True(t, !errors.As(err, &se))
		_, err = i.c.Exec(t.Context(), `create table "" (v int primary key) strict, without rowid`)
		is.True(t, errors.Is(err, stmt.ErrNotSupported))
		_, err = i.c.Query(t.Context(), " ; -- nothing")
		is.True(t, errors.Is(err, engine.ErrNoStatement))
		_, err = i.c.Exec(t.Context(), "")
		is.True(t, errors.Is(err, engine.ErrNoStatement))
	})

	t.Run("should refuse a literal of another type than its column as not supported, after SQLite's errors of the names", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		for _, typ := range []string{"int", "integer", "real", "text", "blob"} {
			is.Equal(t, "", i.exec("create table "+typ+"s (v "+typ+" primary key) strict, without rowid").err)
		}
		literals := map[string]string{"1": "an integer literal", "-0.0": "a real literal", "1.0": "a real literal", "'1'": "a string literal", "''": "a string literal", "x'31'": "a blob literal"}
		holds := map[string]string{"int": "1", "integer": "1", "real": "1.0", "text": "'1'", "blob": "x'31'"}
		for _, typ := range []string{"int", "integer", "real", "text", "blob"} {
			for literal, what := range literals {
				src := fmt.Sprint("insert into ", strings.ToUpper(typ), "S (V) values (", literal, ")")
				if literal == holds[typ] || (typ == "real" && literal == "-0.0") || (typ == "text" && literal == "''") {
					continue
				}
				want := fmt.Sprintf("s4ql: not supported: %v for the column %vs.v of type %v", what, typ, strings.ToUpper(typ))
				i.changes = 7
				for _, run := range []func(src string, args ...int64) answer{i.exec, i.query} {
					got := run(src)
					is.Equal(t, "not supported", got.err, src)
					is.Equal(t, want, got.message, src)
				}
				is.Equal(t, int64(7), i.changes, "the count of changes after "+src)
				is.Equal(t, "1/1: no such table: nosuch", i.exec("insert into nosuch (v) values ("+literal+")").err)
				is.Equal(t, "1/1: table "+typ+"s has no column named w", i.exec("insert into "+typ+"s (w) values ("+literal+")").err)
			}
		}
		is.Equal(t, "blobs (v BLOB): \nintegers (v INTEGER): \nints (v INT): \nreals (v REAL): \ntexts (v TEXT): \n", i.state())
		is.Equal(t, 5, len(logAnnotations(t, i.store)))
	})

	t.Run("should pass an error of the core through as it is", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		i.close()
		_, err := i.c.Exec(t.Context(), t1)
		is.True(t, errors.Is(err, core.ErrStopped))
		_, err = i.c.Query(t.Context(), "select * from t1")
		is.True(t, errors.Is(err, core.ErrStopped))
		var se *sqlerr.Error
		is.True(t, !errors.As(err, &se))
	})

	t.Run("should pass the error of a context that ended through", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := i.c.Exec(ctx, t1)
		is.True(t, errors.Is(err, context.Canceled))
	})
}

func TestEngine_parameters(t *testing.T) {
	cases := []struct {
		name  string
		setup []string
		src   string
		args  []any
		want  string
	}{
		{name: "should insert the argument of the parameter", setup: []string{t1},
			src: "insert into t1 (v1) values (?)", args: []any{42}, want: ""},
		{name: "should insert the smallest integer", setup: []string{t1},
			src: "insert into t1 (v1) values (?)", args: []any{-9223372036854775808}, want: ""},
		{name: "should insert the largest integer, with comments around the parameter", setup: []string{t1},
			src: "insert into T1 (V1) values (/* a */ ? -- b\n);", args: []any{9223372036854775807}, want: ""},
		{name: "should give that a key exists, inserted before through a literal", setup: []string{t1, "insert into t1 (v1) values (-7)"},
			src: "insert into t1 (v1) values (?)", args: []any{-7}, want: "19/1555: UNIQUE constraint failed: t1.v1"},
		{name: "should give that a key exists, with the names as the table stores them",
			setup: []string{`create table "Foo" ("Bar" int primary key) strict, without rowid`, "insert into foo (bar) values (0)"},
			src:   "insert into FOO (BAR) values (?)", args: []any{0}, want: "19/1555: UNIQUE constraint failed: Foo.Bar"},
		{name: "should give that there is no table to write",
			src: "insert into t2 (v1) values (?)", args: []any{1}, want: "1/1: no such table: t2"},
		{name: "should give that there is no column", setup: []string{t1},
			src: "insert into t1 (v2) values (?)", args: []any{1}, want: "1/1: table t1 has no column named v2"},
		{name: "should give the syntax error of SQLite, whatever the arguments",
			src: "insert into t1 (v1) values (? ?)", args: []any{1, 2}, want: `1/1: near "?": syntax error`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pin(t, c.setup, c.src, c.want, c.args...)
		})
	}

	t.Run("should read the rows that parameters inserted, as SQLite does", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		o := newOracle(t)
		is.Equal(t, o.run(t, t1).String(), i.exec(t1).String())
		for _, v := range []int64{3, -9223372036854775808, 0, 9223372036854775807, 3} {
			src := "insert into t1 (v1) values (?)"
			is.Equal(t, o.run(t, src, v).String(), i.exec(src, v).String(), v)
			changes, _ := o.changes(t)
			is.Equal(t, changes, i.changes, v)
		}
		is.Equal(t, o.state(t), i.state())
		is.Equal(t, o.run(t, "select * from t1").String(), i.query("select * from t1").String())
	})

	t.Run("should refuse another number of arguments than parameters, before anything runs", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(t1).err)
		cases := []struct {
			src  string
			args []int64
			want string
		}{
			{src: "insert into t1 (v1) values (?)", want: "s4ql: wrong number of arguments: 0 for a statement with 1 parameter"},
			{src: "insert into t1 (v1) values (?)", args: []int64{1, 2}, want: "s4ql: wrong number of arguments: 2 for a statement with 1 parameter"},
			{src: "insert into t1 (v1) values (1)", args: []int64{1}, want: "s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
			{src: "select * from t1", args: []int64{1}, want: "s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
			{src: t1, args: []int64{1, 2, 3}, want: "s4ql: wrong number of arguments: 3 for a statement with 0 parameters"},
			{src: "drop table t1", args: []int64{1}, want: "s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
		}
		for _, c := range cases {
			_, err := i.c.Exec(t.Context(), c.src, integers(c.args)...)
			is.True(t, errors.Is(err, engine.ErrArguments), c.src)
			is.True(t, !errors.Is(err, stmt.ErrNotSupported), c.src)
			is.Equal(t, c.want, fmt.Sprint(err), c.src)
			is.Equal(t, c.want, fmt.Sprint(engine.Check(c.src, len(c.args))), c.src)
			_, err = i.c.Query(t.Context(), c.src, integers(c.args)...)
			is.Equal(t, c.want, fmt.Sprint(err), c.src)
		}
		is.Equal(t, "t1 (v1 INT): \n", i.state())
		is.Equal(t, 0, len(logAnnotations(t, i.store)[1:]))
	})

	t.Run("should refuse an argument that no column holds as not supported, before the instance", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec("create table t1 (v1 text primary key) strict, without rowid").err)
		is.Equal(t, "", i.exec("create table t2 (v2 real primary key) strict, without rowid").err)
		refused := map[*s4qlpb.Value]string{
			s4qlpb.Value_builder{Real: new(math.NaN())}.Build():     "is a real that is not a number",
			s4qlpb.Value_builder{Real: new(math.Inf(1))}.Build():    "is an infinite real",
			s4qlpb.Value_builder{Real: new(math.Inf(-1))}.Build():   "is an infinite real",
			s4qlpb.Value_builder{Text: new("a\x00b")}.Build():       "is text that holds the character NUL",
			s4qlpb.Value_builder{Text: new("\x00")}.Build():         "is text that holds the character NUL",
			s4qlpb.Value_builder{Text: new("a\xffb")}.Build():       "is text that is not valid UTF-8",
			s4qlpb.Value_builder{Text: new("\xed\xa0\x80")}.Build(): "is text that is not valid UTF-8",
			s4qlpb.Value_builder{Blob: nil}.Build():                 "has no value",
			nil:                                                     "has no value",
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		for v, which := range refused {
			want := "s4ql: not supported: argument 1, which " + which
			for _, src := range []string{"insert into t1 (v1) values (?)", "insert into t2 (v2) values (?)", "insert into nosuch (v) values (?)"} {
				for _, ctx := range []context.Context{t.Context(), ctx} {
					_, err := i.c.Exec(ctx, src, v)
					is.True(t, errors.Is(err, stmt.ErrNotSupported), src)
					var se *sqlerr.Error
					is.True(t, !errors.As(err, &se), src)
					is.Equal(t, want, fmt.Sprint(err), src)
					_, err = i.c.Query(ctx, src, v)
					is.Equal(t, want, fmt.Sprint(err), src)
				}
			}
		}
		is.Equal(t, "t1 (v1 TEXT): \nt2 (v2 REAL): \n", i.state())
		is.Equal(t, 2, len(logAnnotations(t, i.store)))
	})

	t.Run("should refuse an argument of another type than its column as not supported, after SQLite's errors of the names", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		args := map[string]*s4qlpb.Value{
			"an integer": s4qlpb.Value_builder{Integer: new(int64(1))}.Build(),
			"a real":     s4qlpb.Value_builder{Real: new(1.0)}.Build(),
			"text":       s4qlpb.Value_builder{Text: new("1")}.Build(),
			"a blob":     s4qlpb.Value_builder{Blob: []byte("1")}.Build(),
		}
		holds := map[string]string{"int": "an integer", "integer": "an integer", "real": "a real", "text": "text", "blob": "a blob"}
		for typ, held := range holds {
			is.Equal(t, "", i.exec(fmt.Sprintf("create table %vs (v %v primary key) strict, without rowid", typ, typ)).err)
			for what, v := range args {
				if what == held {
					continue
				}
				src := fmt.Sprintf(`insert into "%vS" (V) values (?)`, strings.ToUpper(typ))
				want := fmt.Sprintf("s4ql: not supported: argument 1, which is %v, for the column %vs.v of type %v", what, typ, strings.ToUpper(typ))
				i.changes = 7
				is.Equal(t, want, i.execValues(src, []*s4qlpb.Value{v}).message, src)
				is.Equal(t, want, i.queryValues(src, []*s4qlpb.Value{v}).message, src)
				is.Equal(t, int64(7), i.changes, "the count of changes after "+src)
				is.Equal(t, "1/1: no such table: nosuch", i.execValues("insert into nosuch (v) values (?)", []*s4qlpb.Value{v}).err)
				is.Equal(t, "1/1: table "+typ+"s has no column named w", i.execValues("insert into "+typ+"s (w) values (?)", []*s4qlpb.Value{v}).err)
			}
		}
		is.Equal(t, "blobs (v BLOB): \nintegers (v INTEGER): \nints (v INT): \nreals (v REAL): \ntexts (v TEXT): \n", i.state())
	})

	t.Run("should judge the text before the arguments, and the arguments before the instance", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(t1).err)
		text := map[string]string{
			"insert into t1 (v1) values (? ?)":        `near "?": syntax error`,
			"insert into t1 (v1) values (?1)":         "s4ql: not supported: numbered parameter (at offset 28)",
			"select * from t1 where v1 = ? or v1 = ?": "s4ql: not supported: OR (at offset 30)",
			" ; ": "s4ql: the text holds no statement",
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		check := func(ctx context.Context) {
			t.Helper()
			for src, want := range text {
				for _, args := range [][]*s4qlpb.Value{nil, integers([]int64{1}), integers([]int64{1, 2}), {nil}} {
					_, err := i.c.Exec(ctx, src, args...)
					is.Equal(t, want, fmt.Sprint(err), src)
					_, err = i.c.Query(ctx, src, args...)
					is.Equal(t, want, fmt.Sprint(err), src)
					is.Equal(t, want, fmt.Sprint(engine.Check(src, len(args))), src)
				}
			}
			for _, src := range []string{"insert into t1 (v1) values (?)", "select * from t1 where v1 = ?"} {
				_, err := i.c.Exec(ctx, src)
				is.True(t, errors.Is(err, engine.ErrArguments), src)
				_, err = i.c.Query(ctx, src, integers([]int64{1, 2})...)
				is.True(t, errors.Is(err, engine.ErrArguments), src)
				_, err = i.c.Exec(ctx, src, s4qlpb.Value_builder{Text: new("1\x00")}.Build())
				is.Equal(t, "s4ql: not supported: argument 1, which is text that holds the character NUL", fmt.Sprint(err), src)
				_, err = i.c.Query(ctx, src, s4qlpb.Value_builder{Real: new(math.NaN())}.Build())
				is.Equal(t, "s4ql: not supported: argument 1, which is a real that is not a number", fmt.Sprint(err), src)
			}
		}
		check(ctx)
		_, err := i.c.Exec(ctx, "insert into t1 (v1) values (?)", integers([]int64{1})...)
		is.True(t, errors.Is(err, context.Canceled))
		i.close()
		check(t.Context())
		_, err = i.c.Exec(t.Context(), "insert into t1 (v1) values (?)", integers([]int64{1})...)
		is.True(t, errors.Is(err, core.ErrStopped))
	})

	t.Run("should replay rows that parameters inserted, with an empty cache", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(t1).err)
		for _, v := range []int64{5, -9223372036854775808, 9223372036854775807} {
			is.Equal(t, "", i.exec("insert into t1 (v1) values (?)", v).err)
		}
		want := i.state()
		i.restart(true, engine.Options{})
		is.Equal(t, want, i.state())
		is.Equal(t, "t1 (v1 INT): -9223372036854775808 5 9223372036854775807\n", want)
		is.Equal(t, "19/1555: UNIQUE constraint failed: t1.v1", i.exec("insert into t1 (v1) values (?)", 5).err)
	})
}

// edge is a value at an edge of its type, as a literal and as an argument.
type edge struct {
	literal string
	arg     any
}

// edges of each type of a column, in an order that is not the order of the keys.
var edges = map[string][]edge{
	"int": {
		{"0", 0}, {"9223372036854775807", int64(math.MaxInt64)}, {"-9223372036854775808", int64(math.MinInt64)},
		{"-1", -1}, {"1", 1}, {"9223372036854775806", int64(math.MaxInt64 - 1)}, {"-9223372036854775807", int64(math.MinInt64 + 1)},
	},
	"real": {
		{"0.0", 0.0}, {"-0.0", math.Copysign(0, -1)}, {"5e-324", math.SmallestNonzeroFloat64}, {"-5e-324", -math.SmallestNonzeroFloat64},
		{"1.7976931348623157e308", math.MaxFloat64}, {"-1.7976931348623157e308", -math.MaxFloat64},
		{"2.2250738585072014e-308", 0x1p-1022}, {"2.225073858507201e-308", 0x1p-1022 - math.SmallestNonzeroFloat64},
		{"1.0", 1.0}, {"-1.5", -1.5}, {"9223372036854775807.0", 0x1p63}, {"0.1", 0.1},
	},
	"text": {
		{"''", ""}, {"'a'", "a"}, {"'A'", "A"}, {"'é'", "é"}, {"'日本語'", "日本語"}, {"'🙂'", "🙂"}, {"'it''s'", "it's"},
		{"' '", " "}, {"'a '", "a "}, {"'\u007f'", "\u007f"}, {"'\U0010ffff'", "\U0010ffff"}, {"'é'", "é"},
	},
	"blob": {
		{"x''", []byte{}}, {"x'00'", []byte{0}}, {"x'0000'", []byte{0, 0}}, {"x'ff'", []byte{0xff}}, {"x'0001'", []byte{0, 1}},
		{"x'6162'", []byte("ab")}, {"x'61'", []byte("a")}, {"x'fffe00'", []byte{0xff, 0xfe, 0}},
	},
}

func init() {
	edges["integer"] = edges["int"]
}

// newOracleLimited is [newOracle] with SQLite's length limit limit.
func newOracleLimited(t testing.TB, limit int) *oracle {
	t.Helper()
	o := newOracle(t)
	is.NotError(t, o.c.SetLengthLimit(limit))
	return o
}

func TestEngine_types(t *testing.T) {
	t.Run("should insert and read the edges of each type as SQLite does, through literals and arguments, refuse each again, and replay them", func(t *testing.T) {
		for typ, es := range edges {
			for _, through := range []string{"literal", "argument"} {
				t.Run(typ+" "+through, func(t *testing.T) {
					i := newInstance(t, engine.Options{})
					o := newOracle(t)
					create := fmt.Sprintf("create table t (v %v primary key) strict, without rowid", typ)
					is.Equal(t, "", i.exec(create).err)
					is.Equal(t, "", o.run(t, create).err)
					// Each value twice: the second time it, or -0.0 after 0.0, is a key that exists.
					for _, e := range slices.Concat(es, es) {
						src, args := "insert into t (v) values ("+e.literal+")", []any(nil)
						if through == "argument" {
							src, args = "insert into t (v) values (?)", []any{e.arg}
						}
						want := o.run(t, src, oracleArgs(args)...)
						got := i.execValues(src, values(args))
						is.Equal(t, want.String(), got.String(), e.literal)
						changes, _ := o.changes(t)
						is.Equal(t, changes, i.changes, e.literal)
					}
					want := o.run(t, "select * from t")
					is.Equal(t, want.String(), i.query("select * from t").String())
					is.Equal(t, len(es)-map[bool]int{true: 1}[typ == "real"], len(want.rows))
					state := o.state(t)
					is.Equal(t, state, i.state())
					i.restart(true, engine.Options{})
					is.Equal(t, state, i.state(), "after a replay of the log into an empty cache")
					is.Equal(t, want.String(), i.query("select * from t").String())
				})
			}
		}
	})

	t.Run("should keep INT and INTEGER apart, also after a replay of the log", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec("create table a (v INT primary key) strict, without rowid").err)
		is.Equal(t, "", i.exec("create table b (v integer primary key) strict, without rowid").err)
		is.Equal(t, "", i.exec("insert into b (v) values (1)").err)
		want := "a (v INT): \nb (v INTEGER): 1\n"
		is.Equal(t, want, i.state())
		is.Equal(t, `columns "v INTEGER", rows ["1"]`, i.query("select * from B").String())
		i.restart(true, engine.Options{})
		is.Equal(t, want, i.state())
		is.Equal(t, `columns "v INT", rows []`, i.query("select * from a").String())
	})

	t.Run("should store -0.0 as 0.0, take it for the key 0.0, and annotate the argument as given", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec("create table t (v real primary key) strict, without rowid").err)
		negative := []*s4qlpb.Value{s4qlpb.Value_builder{Real: new(math.Copysign(0, -1))}.Build()}
		is.Equal(t, "", i.execValues("insert into t (v) values (?)", negative).err)
		is.Equal(t, `columns "v REAL", rows ["real 0"]`, i.query("select * from t").String())
		is.Equal(t, "19/1555: UNIQUE constraint failed: t.v", i.exec("insert into t (v) values (0.0)").err)
		is.Equal(t, "19/1555: UNIQUE constraint failed: t.v", i.exec("insert into t (v) values (-0.0)").err)
		is.Equal(t, "insert into t (v) values (?) with real -0", logAnnotations(t, i.store)[1][0])
		i.restart(true, engine.Options{})
		is.Equal(t, "t (v REAL): real 0\n", i.state())
	})

	t.Run("should give a []byte of the rows of its own, and a blob that is empty and not nil", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec("create table t (v blob primary key) strict, without rowid").err)
		arg := []byte("ab")
		is.Equal(t, "", i.execValues("insert into t (v) values (?)", values([]any{arg})).err)
		arg[0] = 'x'
		is.Equal(t, "", i.exec("insert into t (v) values (x'')").err)
		rows, err := i.c.Query(t.Context(), "select * from t")
		is.NotError(t, err)
		var got [][]byte
		for row, err := range rows.All() {
			is.NotError(t, err)
			got = append(got, row[0].GetBlob())
		}
		is.Equal(t, 2, len(got))
		is.True(t, got[0] != nil && len(got[0]) == 0, "the empty blob")
		is.Equal(t, "ab", string(got[1]))
	})
}

func TestEngine_tooBig(t *testing.T) {
	// limit is the length limit of the cache and of SQLite in these tests.
	const limit = 1000

	t.Run("should give SQLite's error for an argument or a row that is too long, with SQLite's count of changes", func(t *testing.T) {
		for _, typ := range []string{"text", "blob"} {
			i := newInstanceLimited(t, engine.Options{}, nil, limit)
			o := newOracleLimited(t, limit)
			create := fmt.Sprintf("create table t (v %v primary key) strict, without rowid", typ)
			is.Equal(t, "", i.exec(create).err)
			is.Equal(t, "", o.run(t, create).err)
			var outcomes []string
			for n := limit - 12; n <= limit+2; n++ {
				var arg any = strings.Repeat("a", n)
				if typ == "blob" {
					arg = bytes.Repeat([]byte{0}, n)
				}
				src := "insert into t (v) values (?)"
				want := o.run(t, src, arg)
				got := i.execValues(src, values([]any{arg}))
				is.Equal(t, want.String(), got.String(), n)
				changes, _ := o.changes(t)
				is.Equal(t, changes, i.changes, n)
				outcomes = append(outcomes, want.err)
			}
			is.Equal(t, o.state(t), i.state())
			// Both a row whose value fits and whose record does not, and a value that does not fit.
			is.True(t, outcomes[0] == "" && outcomes[len(outcomes)-1] == "18/18: string or blob too big", fmt.Sprint(outcomes))
			is.True(t, slices.Index(outcomes, "18/18: string or blob too big") < len(outcomes)-3, fmt.Sprint(outcomes))
		}
	})

	t.Run("should give SQLite's error for a literal whose row is too long, and for one that is too long", func(t *testing.T) {
		i := newInstanceLimited(t, engine.Options{}, nil, limit)
		o := newOracleLimited(t, limit)
		create := "create table t (v text primary key) strict, without rowid"
		is.Equal(t, "", i.exec(create).err)
		is.Equal(t, "", o.run(t, create).err)
		for n := limit - 12; n <= limit; n++ {
			src := "insert into t (v) values ('" + strings.Repeat("a", n) + "')"
			want := o.run(t, src)
			is.Equal(t, want.String(), i.exec(src).String(), n)
			changes, _ := o.changes(t)
			is.Equal(t, changes, i.changes, n)
		}
		// SQLite reads a literal of the text only up to its limit for a text, which is the same
		// limit unless a test lowers this one alone, so it is not compared.
		for _, src := range []string{"insert into t (v) values ('" + strings.Repeat("a", limit+1) + "')", "insert into t (v) values (x'" + strings.Repeat("00", limit+1) + "')"} {
			is.Equal(t, "18/18: string or blob too big", i.exec(src).err)
		}
		is.Equal(t, o.state(t), i.state())
	})

	t.Run("should give SQLite's errors of the names first, then the error of an argument that is too long, also before a read-only transaction and the type of the column", func(t *testing.T) {
		i := newInstanceLimited(t, engine.Options{}, nil, limit)
		o := newOracleLimited(t, limit)
		for _, s := range []string{"create table t (v text primary key) strict, without rowid", "create table n (v int primary key) strict, without rowid"} {
			is.Equal(t, "", i.exec(s).err)
			is.Equal(t, "", o.run(t, s).err)
		}
		long := strings.Repeat("a", limit+1)
		// Each error has SQLite's offset too, which is -1 for those that name no token.
		run := func(src string, arg any, want string) {
			t.Helper()
			sqlite, got := o.run(t, src, arg), i.execValues(src, values([]any{arg}))
			is.Equal(t, want, sqlite.err, "SQLite: "+src)
			is.Equal(t, want, got.err, src)
			is.Equal(t, sqlite.offset, got.offset, src)
		}
		run("insert into nosuch (v) values (?)", long, "1/1: no such table: nosuch")
		run("insert into t (w) values (?)", long, "1/1: table t has no column named w")
		run("insert into t (v) values (?)", long, "18/18: string or blob too big")
		run("insert into n (v) values (?)", long, "18/18: string or blob too big")
		is.NotError(t, i.c.BeginReadOnly(t.Context()))
		for _, s := range oracleBegin(true) {
			is.Equal(t, "", o.run(t, s).err, s)
		}
		run("insert into nosuch (v) values (?)", long, "1/1: no such table: nosuch")
		run("insert into t (v) values (?)", long, "18/18: string or blob too big")
		run("insert into n (v) values (?)", long, "18/18: string or blob too big")
		run("insert into t (v) values (?)", "a", "8/8: attempt to write a readonly database")
		// SQLite refuses the write before it looks at the type of the value.
		run("insert into n (v) values (?)", "a", "8/8: attempt to write a readonly database")
		is.NotError(t, i.c.Rollback(t.Context()))
	})

	t.Run("should give the number of values before an argument that is too long, and that argument before a column without a value or a key that exists", func(t *testing.T) {
		i := newInstanceLimited(t, engine.Options{}, nil, limit)
		o := newOracleLimited(t, limit)
		for _, s := range []string{"create table m (k int primary key, v text not null, w blob not null) strict, without rowid", "insert into m values (1, 'a', x'01')"} {
			is.Equal(t, "", i.exec(s).err)
			is.Equal(t, "", o.run(t, s).err)
		}
		long := strings.Repeat("a", limit+1)
		run := func(src string, want string, args ...any) {
			t.Helper()
			is.Equal(t, want, o.run(t, src, args...).err, "SQLite: "+src)
			is.Equal(t, want, i.execValues(src, values(args)).err, src)
			changes, _ := o.changes(t)
			is.Equal(t, changes, i.changes, "the count of changes after "+src)
		}
		run("insert into m values (?, ?)", "1/1: table m has 3 columns but 2 values were supplied", int64(2), long)
		run("insert into m (k, v) values (?, ?, ?)", "1/1: 3 values for 2 columns", int64(2), long, []byte{1})
		run("insert into m (k, v) values (?, ?)", "18/18: string or blob too big", int64(2), long)
		run("insert into m values (?, ?, ?)", "18/18: string or blob too big", int64(1), long, []byte{1})
		run("insert into m (k, w) values (?, ?)", "19/1299: NOT NULL constraint failed: m.v", int64(2), []byte{1})
		is.Equal(t, o.state(t), i.state())
	})

	t.Run("should give that a row is too long before that its key exists, where its values fit and its record does not, as SQLite builds the record first", func(t *testing.T) {
		for _, tx := range []bool{false, true} {
			i := newInstanceLimited(t, engine.Options{}, nil, limit)
			o := newOracleLimited(t, limit)
			for _, s := range []string{"create table m (k int primary key, a text not null, b blob not null) strict, without rowid", "insert into m values (1, 'x', x'01')"} {
				is.Equal(t, "", i.exec(s).err)
				is.Equal(t, "", o.run(t, s).err)
			}
			if tx {
				is.NotError(t, i.c.Begin(t.Context()))
				for _, s := range oracleBegin(false) {
					is.Equal(t, "", o.run(t, s).err, s)
				}
			}
			var outcomes []string
			// A text and a blob of n bytes each fit the limit, and their record does not from a length
			// between.
			for n := 480; n <= 520; n += 4 {
				text, blob := strings.Repeat("a", n), bytes.Repeat([]byte{2}, n)
				for _, c := range []struct {
					src  string
					args []any
				}{
					{"insert into m values (?, ?, ?)", []any{int64(1), text, blob}},
					{"insert into m (b, k, a) values (?, ?, ?)", []any{blob, int64(1), text}},
					{"insert into m values (?, ?, ?)", []any{int64(n), text, blob}},
				} {
					want := o.run(t, c.src, c.args...)
					is.Equal(t, want.String(), i.execValues(c.src, values(c.args)).String(), fmt.Sprint(c.src, " with ", n, " bytes, in a transaction: ", tx))
					changes, _ := o.changes(t)
					is.Equal(t, changes, i.changes, fmt.Sprint("the count of changes after ", c.src, " with ", n, " bytes"))
					outcomes = append(outcomes, want.err)
				}
			}
			// Both a key that exists and a row that is too long, in the lengths that were tried.
			is.True(t, slices.Contains(outcomes, "19/1555: UNIQUE constraint failed: m.k") && slices.Contains(outcomes, "18/18: string or blob too big"), fmt.Sprint(outcomes))
			if tx {
				is.NotError(t, i.c.Commit(t.Context()))
				is.Equal(t, "", o.run(t, "commit").err)
			}
			is.Equal(t, o.state(t), i.state())
		}
	})

	t.Run("should find the length at which a row with NULL is too long as SQLite does, where a NULL takes no bytes of its own, also before its key exists and after NOT NULL", func(t *testing.T) {
		for _, tx := range []bool{false, true} {
			i := newInstanceLimited(t, engine.Options{}, nil, limit)
			o := newOracleLimited(t, limit)
			for _, s := range []string{
				"create table m (k int primary key, a text, b blob, c real, d int not null) strict, without rowid",
				"insert into m values (1, 'x', x'01', 1.5, 1)",
			} {
				is.Equal(t, "", i.exec(s).err)
				is.Equal(t, "", o.run(t, s).err)
			}
			if tx {
				is.NotError(t, i.c.Begin(t.Context()))
				for _, s := range oracleBegin(false) {
					is.Equal(t, "", o.run(t, s).err, s)
				}
			}
			var outcomes []string
			// moved counts the lengths at which a row with NULL fits and the same row with a real in
			// its place does not.
			moved := 0
			for n := 488; n <= 512; n++ {
				text, blob := strings.Repeat("a", n), bytes.Repeat([]byte{2}, n)
				var got []string
				for _, c := range []struct {
					src  string
					args []any
				}{
					{"insert into m values (?, ?, ?, ?, ?)", []any{int64(n), text, blob, nil, int64(1)}},
					{"insert into m values (?, ?, ?, ?, ?)", []any{int64(1000 + n), text, blob, 2.5, int64(1)}},
					{"insert into m values (?, ?, ?, null, ?)", []any{int64(1), text, blob, int64(1)}},
					{"insert into m (b, k, a, d) values (?, ?, ?, ?)", []any{blob, int64(2000 + n), text, int64(1)}},
					{"insert into m values (?, ?, ?, ?, ?)", []any{int64(3000 + n), text, blob, nil, nil}},
					{"insert into m values (?, null, ?, null, ?)", []any{int64(4000 + n), append(slices.Clone(blob), blob...), int64(1)}},
				} {
					want := o.run(t, c.src, c.args...)
					is.Equal(t, want.String(), i.execValues(c.src, values(c.args)).String(), fmt.Sprint(c.src, " with ", n, " bytes, in a transaction: ", tx))
					changes, _ := o.changes(t)
					is.Equal(t, changes, i.changes, fmt.Sprint("the count of changes after ", c.src, " with ", n, " bytes"))
					got = append(got, want.err)
				}
				if got[0] == "" && got[1] == "18/18: string or blob too big" {
					moved++
				}
				outcomes = append(outcomes, got...)
			}
			is.True(t, moved > 0, "no length at which NULL fits and a real does not")
			for _, want := range []string{"", "19/1555: UNIQUE constraint failed: m.k", "18/18: string or blob too big", "19/1299: NOT NULL constraint failed: m.d"} {
				is.True(t, slices.Contains(outcomes, want), want)
			}
			if tx {
				is.NotError(t, i.c.Commit(t.Context()))
				is.Equal(t, "", o.run(t, "commit").err)
			}
			want := o.state(t)
			is.Equal(t, want, i.state())
			i.restart(true, engine.Options{})
			is.Equal(t, want, i.state(), "after a replay of the log")
		}
	})

	t.Run("should keep a transaction usable after a value that is too long", func(t *testing.T) {
		i := newInstanceLimited(t, engine.Options{}, nil, limit)
		is.Equal(t, "", i.exec("create table t (v blob primary key) strict, without rowid").err)
		is.NotError(t, i.c.Begin(t.Context()))
		is.Equal(t, "", i.exec("insert into t (v) values (x'01')").err)
		for _, n := range []int{limit + 1, limit} {
			got := i.execValues("insert into t (v) values (?)", values([]any{make([]byte, n)}))
			is.Equal(t, "18/18: string or blob too big", got.err, n)
		}
		is.Equal(t, "", i.exec("insert into t (v) values (x'02')").err)
		is.NotError(t, i.c.Commit(t.Context()))
		want := "t (v BLOB): x'01' x'02'\n"
		is.Equal(t, want, i.state())
		i.restart(true, engine.Options{})
		is.Equal(t, want, i.state())
	})
}

func TestEngine_Exec(t *testing.T) {
	t.Run("should count one change for an insert, and none set by a create table, a drop table, or a query", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		ctx := t.Context()
		res, err := i.c.Exec(ctx, t1)
		is.NotError(t, err)
		is.Equal(t, engine.Result{}, res)
		res, err = i.c.Exec(ctx, "insert into t1 (v1) values (1)")
		is.NotError(t, err)
		is.Equal(t, engine.Result{Changes: 1, SetsChanges: true}, res)
		res, err = i.c.Exec(ctx, "select * from t1")
		is.NotError(t, err)
		is.Equal(t, engine.Result{}, res)
		res, err = i.c.Exec(ctx, "drop table t1")
		is.NotError(t, err)
		is.Equal(t, engine.Result{}, res)
	})

	t.Run("should run a query to its end and give its error", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		_, err := i.c.Exec(t.Context(), "select * from t1")
		is.Equal(t, "no such table: t1", err.Error())
	})
}

func TestEngine_Query(t *testing.T) {
	t.Run("should run a statement that is not a query, with its result and no rows", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		ctx := t.Context()
		_, err := i.c.Exec(ctx, t1)
		is.NotError(t, err)
		rows, err := i.c.Query(ctx, "insert into t1 (v1) values (7)")
		is.NotError(t, err)
		is.Equal(t, engine.Result{Changes: 1, SetsChanges: true}, rows.Result())
		is.Equal(t, 0, len(rows.Columns()))
		for _, err := range rows.All() {
			_ = err
			t.Fatal("a row")
		}
		is.NotError(t, rows.Close())
		is.Equal(t, `columns "v1 INT", rows ["7"]`, i.query("select * from t1").String())
	})

	t.Run("should end the read transaction when the loop stops early, at Close, and at Close during the loop", func(t *testing.T) {
		i := newInstance(t, engine.Options{Logger: slog.New(slog.DiscardHandler)})
		ctx := t.Context()
		_, err := i.c.Exec(ctx, t1)
		is.NotError(t, err)
		for v := range 10 {
			_, err := i.c.Exec(ctx, fmt.Sprint("insert into t1 (v1) values (", v, ")"))
			is.NotError(t, err)
		}
		// A read transaction that is left open shows at the next query of the connection, which
		// fails.
		for range 10 {
			rows, err := i.c.Query(ctx, "select * from t1")
			is.NotError(t, err)
			for _, err := range rows.All() {
				_ = err
				break
			}
			is.NotError(t, rows.Close())

			rows, err = i.c.Query(ctx, "select * from t1")
			is.NotError(t, err)
			is.NotError(t, rows.Close())
			for _, err := range rows.All() {
				_ = err
				t.Fatal("a row after Close")
			}

			rows, err = i.c.Query(ctx, "select * from t1")
			is.NotError(t, err)
			n := 0
			for _, err := range rows.All() {
				is.NotError(t, err)
				n++
				is.NotError(t, rows.Close())
			}
			is.Equal(t, 1, n)
		}
		noneOpen(t, i)
	})

	t.Run("should end the read transaction when the loop panics", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		ctx := t.Context()
		_, err := i.c.Exec(ctx, t1)
		is.NotError(t, err)
		_, err = i.c.Exec(ctx, "insert into t1 (v1) values (1)")
		is.NotError(t, err)
		for range 10 {
			rows, err := i.c.Query(ctx, "select * from t1")
			is.NotError(t, err)
			func() {
				defer func() { is.Equal(t, "the consumer", recover()) }()
				for _, err := range rows.All() {
					_ = err
					panic("the consumer")
				}
			}()
		}
		noneOpen(t, i)
	})
}

// noneOpen requires that the instance i holds no transaction: a write transaction can begin, and so
// can a read transaction on the connection of i. A write transaction that is left open makes it wait
// until the test times out, and a read transaction that is left open makes it fail.
func noneOpen(t *testing.T, i *instance) {
	t.Helper()
	ctx := t.Context()
	tx, err := i.d.Begin(ctx)
	is.NotError(t, err)
	readFree(t, i.c)
	is.NotError(t, tx.Rollback(ctx))
}

// readFree requires that no read transaction is open on the connection c: a query of a table that
// does not exist gets its read transaction, in which it finds that out. A read transaction that is
// left open makes it fail with another error.
func readFree(t *testing.T, c *engine.Conn) {
	t.Helper()
	_, err := c.Query(t.Context(), `select * from "no such table"`)
	var se *sqlerr.Error
	is.True(t, errors.As(err, &se), fmt.Sprint(err))
	is.Equal(t, `no such table: no such table`, se.Message)
}

// logAnnotations of the database in the store s, in the order of the log, each commit object as its
// annotations: the text of each, and " with " and the value of each of its parameters.
func logAnnotations(t testing.TB, s *simstore.Store) [][]string {
	t.Helper()
	data, _, ok := s.Object("db/manifest.json")
	if !ok {
		return nil
	}
	m, err := format.ParseManifest(data)
	is.NotError(t, err)
	var commits [][]string
	for name := m.Head; name != format.EmptyLog; {
		key := name.String()
		data, _, ok := s.Object("db/commits/" + key[:2] + "/" + key)
		is.True(t, ok, key)
		r := format.NewCommitReader(bytes.NewReader(data), name)
		h, err := r.Header()
		is.NotError(t, err)
		var notes []string
		for {
			_, a, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			is.NotError(t, err)
			if a != nil {
				note := a.GetStatement().GetSql()
				for _, p := range a.GetStatement().GetParameters() {
					note += " with " + valueText(t, p)
				}
				notes = append(notes, note)
			}
		}
		commits = append(commits, notes)
		name, err = format.NameFromBytes(h.GetParent())
		is.NotError(t, err)
	}
	slices.Reverse(commits)
	return commits
}

// logChanges of the log in the store s, oldest commit first, each as its changes, one line for each
// row: "upsert t 1,2" for a row of the table t, and "delete t 1" for the key of one. A create and a
// drop of a table are "create t" and "drop t".
func logChanges(t testing.TB, s *simstore.Store) [][]string {
	t.Helper()
	data, _, ok := s.Object("db/manifest.json")
	if !ok {
		return nil
	}
	m, err := format.ParseManifest(data)
	is.NotError(t, err)
	var commits [][]string
	for name := m.Head; name != format.EmptyLog; {
		key := name.String()
		data, _, ok := s.Object("db/commits/" + key[:2] + "/" + key)
		is.True(t, ok, key)
		r := format.NewCommitReader(bytes.NewReader(data), name)
		h, err := r.Header()
		is.NotError(t, err)
		var lines []string
		for {
			c, _, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			is.NotError(t, err)
			switch {
			case c == nil:
			case c.HasCreateTable():
				lines = append(lines, "create "+c.GetCreateTable().GetTable())
			case c.HasDropTable():
				lines = append(lines, "drop "+c.GetDropTable().GetTable())
			case c.HasUpsertRows():
				for _, row := range c.GetUpsertRows().GetRows() {
					var vs []string
					for _, v := range row.GetValues() {
						vs = append(vs, valueText(t, v))
					}
					lines = append(lines, "upsert "+c.GetUpsertRows().GetTable()+" "+strings.Join(vs, ","))
				}
			case c.HasDeleteRows():
				for _, k := range c.GetDeleteRows().GetPrimaryKeys() {
					var vs []string
					for _, v := range k.GetValues() {
						vs = append(vs, valueText(t, v))
					}
					lines = append(lines, "delete "+c.GetDeleteRows().GetTable()+" "+strings.Join(vs, ","))
				}
			}
		}
		commits = append(commits, lines)
		name, err = format.NameFromBytes(h.GetParent())
		is.NotError(t, err)
	}
	slices.Reverse(commits)
	return commits
}

func TestEngine_annotations(t *testing.T) {
	t.Run("should annotate each commit with the text of its statement, from its first token to its end, and commit nothing for a statement that fails", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		texts := []string{
			"  /* before */ create table t1 (v1 int primary key) strict, without rowid; -- after",
			"insert into t1 (v1) /* inside */ values (1)",
			"insert into t1 (v1) values (1)",
			"select * from t1",
			"drop table nosuch",
			";drop table T1;;",
		}
		for _, src := range texts {
			_, _ = i.c.Exec(t.Context(), src)
		}
		is.Equal(t, fmt.Sprint([][]string{
			{"create table t1 (v1 int primary key) strict, without rowid;"},
			{"insert into t1 (v1) /* inside */ values (1)"},
			{"drop table T1;"},
		}), fmt.Sprint(logAnnotations(t, i.store)))
	})

	t.Run("should annotate each commit with the arguments of its statement, and commit nothing for a statement that fails", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(t1).err)
		is.Equal(t, "", i.exec("insert into t1 (v1) values (?);", -9223372036854775808).err)
		is.Equal(t, "", i.exec("insert into t1 (v1) values (1)").err)
		is.Equal(t, "19/1555: UNIQUE constraint failed: t1.v1", i.exec("insert into t1 (v1) values (?)", 1).err)
		is.Equal(t, "", i.query("insert into t1 (v1) values ( ? ) -- q", 9223372036854775807).err)
		is.Equal(t, fmt.Sprint([][]string{
			{"create table t1 (v1 int primary key) strict, without rowid"},
			{"insert into t1 (v1) values (?); with -9223372036854775808"},
			{"insert into t1 (v1) values (1)"},
			{"insert into t1 (v1) values ( ? ) with 9223372036854775807"},
		}), fmt.Sprint(logAnnotations(t, i.store)))
	})

	t.Run("should annotate an insert of several values with all its arguments, in the order of its parameters", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(t3).err)
		is.Equal(t, "", i.execValues("insert into t3 (v3, v1, v2) values (?, 1, ?)", values([]any{1.5, "a"})).err)
		is.Equal(t, "", i.execValues("insert into t3 values (?, ?, ?)", values([]any{int64(2), "b", 2.5})).err)
		is.Equal(t, "19/1299: NOT NULL constraint failed: t3.v3", i.execValues("insert into t3 (v1, v2) values (?, ?)", values([]any{int64(3), "c"})).err)
		is.Equal(t, fmt.Sprint([][]string{
			{t3},
			{`insert into t3 (v3, v1, v2) values (?, 1, ?) with real 1.5 with "a"`},
			{`insert into t3 values (?, ?, ?) with 2 with "b" with real 2.5`},
		}), fmt.Sprint(logAnnotations(t, i.store)))
		want := "t3 (v1 INT, v2 TEXT, v3 REAL): 1,\"a\",real 1.5 2,\"b\",real 2.5\n"
		is.Equal(t, want, i.state())
		i.restart(true, engine.Options{})
		is.Equal(t, want, i.state(), "after a replay of the log")
	})
}

// events records the events of a logger.
type events struct {
	mu      sync.Mutex
	records []map[string]string
	levels  []slog.Level
}

func (e *events) Enabled(context.Context, slog.Level) bool { return true }
func (e *events) WithAttrs([]slog.Attr) slog.Handler       { return e }
func (e *events) WithGroup(string) slog.Handler            { return e }

func (e *events) Handle(_ context.Context, r slog.Record) error {
	m := map[string]string{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = append(e.records, m)
	e.levels = append(e.levels, r.Level)
	return nil
}

// last event of the engine, with its level.
func (e *events) last(t *testing.T) (map[string]string, slog.Level) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	is.True(t, len(e.records) > 0)
	return e.records[len(e.records)-1], e.levels[len(e.levels)-1]
}

func TestEngine_events(t *testing.T) {
	t.Run("should log one event for each statement, with its kind, table, outcome, rows, and position, and the text at Debug only", func(t *testing.T) {
		h := &events{}
		ch := &events{}
		i := newInstanceLogged(t, engine.Options{Logger: slog.New(h)}, slog.New(ch))
		ctx := t.Context()

		_, err := i.c.Exec(ctx, t1)
		is.NotError(t, err)
		e, level := h.last(t)
		is.Equal(t, slog.LevelDebug, level)
		is.Equal(t, engine.EventStatement, e["msg"])
		is.Equal(t, "create_table", e[engine.AttrKind])
		is.Equal(t, "t1", e[engine.AttrTable])
		is.Equal(t, "ok", e[engine.AttrOutcome])
		is.Equal(t, "0", e[engine.AttrRows])
		is.Equal(t, t1, e[engine.AttrSQL])
		is.Equal(t, format.EmptyLog.String(), e[engine.AttrParent])
		is.Equal(t, e[engine.AttrParent], e[engine.AttrPosition])
		_, ok := e[engine.AttrDuration]
		is.True(t, ok)

		_, err = i.c.Exec(ctx, "insert into T1 (v1) values (5)")
		is.NotError(t, err)
		e, _ = h.last(t)
		is.Equal(t, "insert", e[engine.AttrKind])
		is.Equal(t, "T1", e[engine.AttrTable])
		is.Equal(t, "1", e[engine.AttrRows])
		parent := e[engine.AttrParent]
		// The event of the commit that the insert caused has the same parent, under the same name.
		commit, _ := ch.last(t)
		is.Equal(t, "commit", commit["msg"])
		is.Equal(t, parent, commit[engine.AttrParent])
		is.Equal(t, "committed", commit[engine.AttrOutcome])

		_, err = i.c.Exec(ctx, "insert into t1 (v1) values (5)")
		is.True(t, err != nil)
		e, level = h.last(t)
		is.Equal(t, slog.LevelDebug, level)
		is.Equal(t, "sqlite_error", e[engine.AttrOutcome])
		is.Equal(t, "19", e[engine.AttrCode])
		is.Equal(t, "1555", e[engine.AttrExtendedCode])
		is.Equal(t, "UNIQUE constraint failed: t1.v1", e[engine.AttrError])
		is.Equal(t, "0", e[engine.AttrRows])
		is.True(t, e[engine.AttrParent] != parent, "the parent of the insert after a commit")

		rows, err := i.c.Query(ctx, "select * from t1")
		is.NotError(t, err)
		for _, err := range rows.All() {
			is.NotError(t, err)
		}
		e, _ = h.last(t)
		is.Equal(t, "select", e[engine.AttrKind])
		is.Equal(t, "1", e[engine.AttrRows])
		_, ok = e[engine.AttrParent]
		is.True(t, !ok, "a query has no parent")
		is.Equal(t, e[engine.AttrPosition] != "", true)

		_, err = i.c.Exec(ctx, "select * from t1 where 1")
		is.True(t, err != nil)
		e, _ = h.last(t)
		is.Equal(t, "not_supported", e[engine.AttrOutcome])
		_, ok = e[engine.AttrKind]
		is.True(t, !ok, "not supported has no kind")

		_, err = i.c.Exec(ctx, "")
		is.True(t, err != nil)
		e, _ = h.last(t)
		is.Equal(t, "no_statement", e[engine.AttrOutcome])

		// No value of an argument is in an event, of a statement that succeeds or that fails.
		for _, args := range [][]int64{{4815162342}, {4815162342, 4815162343}} {
			_, _ = i.c.Exec(ctx, "insert into t1 (v1) values (?)", integers(args)...)
			e, level = h.last(t)
			is.Equal(t, slog.LevelDebug, level)
			is.Equal(t, "insert", e[engine.AttrKind])
			is.Equal(t, "insert into t1 (v1) values (?)", e[engine.AttrSQL])
			is.True(t, !strings.Contains(fmt.Sprint(e), "481516234"), fmt.Sprint(e))
		}
		is.Equal(t, "wrong_arguments", e[engine.AttrOutcome])
		is.Equal(t, "s4ql: wrong number of arguments: 2 for a statement with 1 parameter", e[engine.AttrError])
		ch.mu.Lock()
		ofCore := fmt.Sprint(ch.records)
		ch.mu.Unlock()
		is.True(t, !strings.Contains(ofCore, "481516234"), ofCore)

		i.close()
		_, err = i.c.Exec(ctx, "insert into t1 (v1) values (6)")
		is.True(t, err != nil)
		e, level = h.last(t)
		is.Equal(t, slog.LevelDebug, level)
		is.Equal(t, "refused", e[engine.AttrOutcome])
	})

	t.Run("should log no text and no event of a healthy instance at Info", func(t *testing.T) {
		var b strings.Builder
		i := newInstance(t, engine.Options{Logger: slog.New(slog.NewTextHandler(&b, nil))})
		for _, src := range []string{t1, "insert into t1 (v1) values (1)", "insert into t1 (v1) values (1)", "select * from t1", "selec"} {
			_, _ = i.c.Exec(t.Context(), src)
		}
		is.Equal(t, "", b.String())
	})
}

func TestEngine_internal(t *testing.T) {
	t.Run("should never give an error that wraps a refusal of the core", func(t *testing.T) {
		for _, err := range []error{core.ErrTableExists, core.ErrNoTable, core.ErrReservedName, core.ErrInvalid} {
			got := engine.FromCore(fmt.Errorf("x: %w", err))
			is.True(t, errors.Is(got, engine.ErrInternal))
			is.True(t, !errors.Is(got, err))
		}
		is.True(t, errors.Is(engine.FromCore(core.ErrNoLease), core.ErrNoLease))
		is.True(t, engine.FromCore(nil) == nil)
	})
}

func TestEngine_noneOpen(t *testing.T) {
	// run a call of the sequence on i, with its arguments, and consume the rows of a query in the way
	// mode says: to the end, a loop that stops at once, a close without a loop, or a loop that panics.
	run := func(t *testing.T, i *instance, c stmttest.Call, mode int) {
		t.Helper()
		if mode == 0 {
			_, _ = i.c.Exec(t.Context(), c.Src, values(c.Args)...)
			return
		}
		rows, err := i.c.Query(t.Context(), c.Src, values(c.Args)...)
		if err != nil {
			return
		}
		switch mode {
		case 1:
			for _, err := range rows.All() {
				_ = err
			}
		case 2:
			for _, err := range rows.All() {
				_ = err
				break
			}
		case 3:
			is.NotError(t, rows.Close())
		case 4:
			func() {
				defer func() { _ = recover() }()
				for _, err := range rows.All() {
					_ = err
					panic("the consumer")
				}
			}()
		}
	}

	t.Run("should hold no transaction after any statement, error, early stop, or panic", func(t *testing.T) {
		n := 300
		if slow() {
			n /= 10
		}
		i := newInstance(t, engine.Options{})
		r := rand.New(rand.NewPCG(*seed, 0x0be))
		q := newSequence(t, r)
		for range n {
			run(t, i, q.Next(), r.IntN(5))
			noneOpen(t, i)
		}
	})

	t.Run("should find a transaction left open with the planted bug", func(t *testing.T) {
		var options engine.Options
		options.Plant("keepOpen")
		i := newInstance(t, options)
		_, err := i.c.Exec(t.Context(), "drop table t")
		is.True(t, err != nil)
		// The transaction is open, so no machine is slow enough for the begin to succeed.
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		_, err = i.d.Begin(ctx)
		is.True(t, errors.Is(err, context.DeadlineExceeded))
	})
}

func TestEngine_namesOfTheCore(t *testing.T) {
	t.Run("should take every name that the pass supports, as SQLite does, in every statement", func(t *testing.T) {
		pools, err := stmttest.NamePools()
		is.NotError(t, err)
		names := slices.Concat(pools...)
		r := rand.New(rand.NewPCG(*seed, 0x4a3e))
		// Random names of letters, marks, symbols, spaces, quotes, and runes from every plane.
		runes := []rune{'a', 'Z', '_', ' ', '"', '\'', '`', '[', ']', ';', '-', '/', '*', '\t', '\n', 'é', 'Ω', 'ı', 'ſ', 0x301, 0x200b, 0xfeff, '\U0001F600', '\U0010FFFD', 0x7f, 0x1}
		n := 2000
		if slow() {
			n /= 10
		}
		for range n {
			b := make([]rune, 1+r.IntN(8))
			for i := range b {
				b[i] = runes[r.IntN(len(runes))]
			}
			names = append(names, stmttest.Quote(string(b)))
		}
		i := newInstance(t, engine.Options{})
		o := newOracle(t)
		checked := 0
		for _, n := range names {
			for _, src := range []string{
				"create table " + n + " (" + n + " int primary key) strict, without rowid",
				"insert into " + n + " (" + n + ") values (1)",
				"select * from " + n,
				"drop table " + n,
			} {
				got := i.query(src)
				if got.err == "not supported" {
					continue
				}
				checked++
				is.Equal(t, o.run(t, src).String(), got.String(), src)
			}
		}
		t.Logf("%v statements of %v names", checked, len(names))
	})
}

func TestConn(t *testing.T) {
	const query = "select * from t1"
	// count the rows on their way to the end.
	count := func(t *testing.T, rows *engine.Rows) int {
		t.Helper()
		var n int
		for _, err := range rows.All() {
			is.NotError(t, err)
			n++
		}
		return n
	}

	t.Run("should run the queries of twelve connections at once, each on the state of its start, and refuse a second query of a connection while the rows of its first are open", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		ctx := t.Context()
		_, err := i.c.Exec(ctx, t1)
		is.NotError(t, err)

		// Each connection starts its query after one more insert than the connection before it.
		const n = 12
		var conns []*engine.Conn
		var open []*engine.Rows
		for k := range n {
			_, err := i.c.Exec(ctx, fmt.Sprint("insert into t1 (v1) values (", k, ")"))
			is.NotError(t, err)
			c := i.e.Conn()
			rows, err := c.Query(ctx, query)
			is.NotError(t, err)
			conns, open = append(conns, c), append(open, rows)
		}

		// The rows of a connection hold its reader, so its next statement that reads fails at once. A
		// write needs no reader.
		_, err = conns[0].Query(ctx, query)
		is.True(t, errors.Is(err, core.ErrReaderBusy), fmt.Sprint(err))
		_, err = conns[0].Exec(ctx, query)
		is.True(t, errors.Is(err, core.ErrReaderBusy), fmt.Sprint(err))
		is.True(t, !errors.Is(err, engine.ErrInternal), fmt.Sprint(err))
		_, err = conns[0].Exec(ctx, fmt.Sprint("insert into t1 (v1) values (", n, ")"))
		is.NotError(t, err)

		for k, rows := range open {
			is.Equal(t, k+1, count(t, rows))
		}
		// The rows have ended, and each connection reads again, what is committed now.
		for _, c := range conns {
			rows, err := c.Query(ctx, query)
			is.NotError(t, err)
			is.Equal(t, n+1, count(t, rows))
			readFree(t, c)
			is.NotError(t, c.Close(ctx))
		}
		noneOpen(t, i)
	})

	t.Run("should not close a connection inside the loop over the rows of its query, and close it after the loop", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		ctx := t.Context()
		_, err := i.c.Exec(ctx, t1)
		is.NotError(t, err)
		for v := range 3 {
			_, err := i.c.Exec(ctx, fmt.Sprint("insert into t1 (v1) values (", v, ")"))
			is.NotError(t, err)
		}
		c := i.e.Conn()
		rows, err := c.Query(ctx, query)
		is.NotError(t, err)
		var n int
		for _, err := range rows.All() {
			is.NotError(t, err)
			n++
			is.True(t, errors.Is(c.Close(ctx), core.ErrReaderInUse))
		}
		is.Equal(t, 3, n)
		// The refusals closed nothing: the connection reads again.
		readFree(t, c)
		is.NotError(t, c.Close(ctx))
		_, err = c.Query(ctx, query)
		is.True(t, errors.Is(err, engine.ErrInternal), fmt.Sprint(err))
	})

	t.Run("should close the reader of a connection after the close of its instance, and refuse its statements with the stop", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		ctx := t.Context()
		_, err := i.c.Exec(ctx, t1)
		is.NotError(t, err)
		c := i.e.Conn()
		rows, err := c.Query(ctx, query)
		is.NotError(t, err)

		// The close of the instance does not wait for rows that no loop reads.
		i.close()
		var failed int
		for _, err := range rows.All() {
			is.True(t, errors.Is(err, core.ErrStopped), fmt.Sprint(err))
			failed++
		}
		is.Equal(t, 1, failed)
		_, err = c.Query(ctx, query)
		is.True(t, errors.Is(err, core.ErrStopped), fmt.Sprint(err))
		is.NotError(t, c.Close(ctx))
		is.NotError(t, c.Close(ctx))
	})
}

func TestEngine_concurrent(t *testing.T) {
	t.Run("should run statements from several goroutines at once, for the race detector", func(t *testing.T) {
		i := newInstance(t, engine.Options{Logger: slog.New(&events{})})
		_, err := i.c.Exec(t.Context(), t1)
		is.NotError(t, err)
		// Each goroutine has a connection of its own, as each has in a pool.
		conns := make([]*engine.Conn, 8)
		for g := range conns {
			conns[g] = i.e.Conn()
		}
		var wg sync.WaitGroup
		for g, c := range conns {
			wg.Go(func() {
				for v := range 20 {
					src := fmt.Sprint("insert into t1 (v1) values (", v%5, ")")
					if v%2 == 1 {
						src = "select * from t1"
					}
					rows, err := c.Query(t.Context(), src)
					var se *sqlerr.Error
					if err != nil && !errors.As(err, &se) {
						t.Errorf("goroutine %v: %v", g, err)
						return
					}
					if err == nil {
						for _, err := range rows.All() {
							if err != nil {
								t.Errorf("goroutine %v: %v", g, err)
							}
						}
					}
				}
			})
		}
		wg.Wait()
		is.Equal(t, `columns "v1 INT", rows ["0" "1" "2" "3" "4"]`, i.query("select * from t1").String())
		noneOpen(t, i)
		for _, c := range conns {
			readFree(t, c)
			is.NotError(t, c.Close(t.Context()))
		}
	})
}

// shape of a table of several columns: the type of each column, in order, the position of the
// column of its primary key, and whether each column takes NULL, which the key never does.
type shape struct {
	types    []string
	key      int
	nullable []bool
}

// shapes of tables of two and three columns of mixed types, with the key at each position. Each
// has every column NOT NULL, every column but the key taking NULL, and, for three columns, each
// column but the key taking NULL alone, so that a column that takes NULL is of each type at each
// position, beside one that does not.
func shapes() []shape {
	var ss []shape
	for _, types := range [][]string{{"int", "text"}, {"blob", "real"}, {"text", "integer", "real"}, {"real", "blob", "int"}, {"integer", "text", "blob"}} {
		for key := range types {
			n := len(types)
			ss = append(ss, shape{types: types, key: key, nullable: make([]bool, n)})
			all := make([]bool, n)
			for i := range all {
				all[i] = i != key
			}
			ss = append(ss, shape{types: types, key: key, nullable: all})
			if n < 3 {
				continue
			}
			for i := range n {
				if i != key {
					one := make([]bool, n)
					one[i] = true
					ss = append(ss, shape{types: types, key: key, nullable: one})
				}
			}
		}
	}
	return ss
}

// The names of the columns of a shape, by position: as its create table writes them, as the table
// stores them, and in another spelling of the same names, which the other statements write.
var (
	columnNames  = []string{"a", `"B b"`, "C"}
	storedNames  = []string{"a", "B b", "C"}
	spelledNames = []string{"A", `"b B"`, "c"}
)

func (s shape) String() string {
	var nullable []int
	for i, n := range s.nullable {
		if n {
			nullable = append(nullable, i)
		}
	}
	return fmt.Sprintf("%v with the key at %v and NULL at %v", strings.Join(s.types, ", "), s.key, nullable)
}

// create table of the shape, with the name table.
func (s shape) create(table string) string {
	var cs []string
	for i, typ := range s.types {
		c := columnNames[i] + " " + typ + " not null"
		switch {
		case i == s.key:
			c = columnNames[i] + " " + typ + " primary key"
		case s.nullable[i]:
			c = columnNames[i] + " " + typ
		}
		cs = append(cs, c)
	}
	return "create table " + table + " (" + strings.Join(cs, ", ") + ") strict, without rowid"
}

// own is the refusal of a value of another type than its column at the position wrong by s4ql's own
// rule, for an insert that gives NULL to the columns at the positions nulls or leaves them out: ""
// if one of them does not take NULL, since SQLite's `NOT NULL constraint failed` comes first, and
// refusal(wrong) otherwise.
func (s shape) own(nulls []int, wrong int, refusal func(int) string) string {
	for _, i := range nulls {
		if !s.nullable[i] {
			return ""
		}
	}
	return refusal(wrong)
}

// literal of the type typ for the number k: k as an integer, k and a half as a real, k as text, and
// the byte k as a blob.
func literal(typ string, k int) string {
	switch typ {
	case "int", "integer":
		return fmt.Sprint(k)
	case "real":
		return fmt.Sprint(k, ".5")
	case "text":
		return fmt.Sprintf("'%v'", k)
	}
	return fmt.Sprintf("x'%02x'", k)
}

// argument of the type typ for the number k, with the value that [literal] gives.
func argument(typ string, k int) any {
	switch typ {
	case "int", "integer":
		return int64(k)
	case "real":
		return float64(k) + 0.5
	case "text":
		return fmt.Sprint(k)
	}
	return []byte{byte(k)}
}

// otherLiteral is a literal of another type than typ, with its kind as the message of the engine
// names it.
func otherLiteral(typ string) (literal, kind string) {
	switch typ {
	case "int", "integer":
		return "1.5", "a real literal"
	case "real":
		return "1", "an integer literal"
	case "text":
		return "x'01'", "a blob literal"
	}
	return "'x'", "a string literal"
}

// columnsStep is a statement of [TestEngine_columns], with its arguments, and, for a statement that
// the engine refuses by its own rule, the message of the refusal, which is not compared with SQLite.
type columnsStep struct {
	src  string
	args []any
	own  string
}

// steps of the shape on its table t, which does not exist before them.
func (s shape) steps() []columnsStep {
	n := len(s.types)
	inOrder, reversed := make([]int, n), make([]int, n)
	for i := range n {
		inOrder[i], reversed[i] = i, n-1-i
	}
	without := func(left ...int) []int {
		return slices.DeleteFunc(slices.Clone(inOrder), func(i int) bool { return slices.Contains(left, i) })
	}
	list := func(names []string, order []int) string {
		var ns []string
		for _, i := range order {
			ns = append(ns, names[i])
		}
		return strings.Join(ns, ", ")
	}
	// rowAt of literals for the number k, for the columns in order, with the literal replaced for the
	// column at the position wrong, if it is not -1, by one of another type, and for the column at
	// the position p, if it is not -1, by put.
	rowAt := func(order []int, k, wrong, p int, put string) string {
		var vs []string
		for _, i := range order {
			v := literal(s.types[i], k)
			switch i {
			case wrong:
				v, _ = otherLiteral(s.types[i])
			case p:
				v = put
			}
			vs = append(vs, v)
		}
		return strings.Join(vs, ", ")
	}
	row := func(order []int, k, wrong int) string {
		return rowAt(order, k, wrong, -1, "")
	}
	insert := func(order []int, k, wrong int) string {
		return fmt.Sprintf("insert into t (%v) values (%v)", list(spelledNames, order), row(order, k, wrong))
	}
	insertAll := func(k, wrong int) string {
		return fmt.Sprintf("insert into t values (%v)", row(inOrder, k, wrong))
	}
	refusal := func(position int) string {
		_, kind := otherLiteral(s.types[position])
		return fmt.Sprintf("s4ql: not supported: %v for the column t.%v of type %v", kind, storedNames[position], strings.ToUpper(s.types[position]))
	}
	params := strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
	var reversedArgs []any
	for _, i := range reversed {
		reversedArgs = append(reversedArgs, argument(s.types[i], 3))
	}

	steps := []columnsStep{
		{src: s.create("t")},
		{src: s.create("T")},
		// A table that exists comes before a column named twice, which comes after the check of the
		// table.
		{src: "create table t (a int primary key, A text not null) strict, without rowid"},
		{src: "create table u (a int primary key, b text not null, A text not null) strict, without rowid"},
		{src: "select * from t"},
		{src: insertAll(4, -1)},
		{src: insert(reversed, 1, -1)},
		{src: fmt.Sprintf("insert into T (%v) values (%v)", list(spelledNames, reversed), params), args: reversedArgs},
		{src: fmt.Sprintf("insert into t (%v) values (?, %v)", list(columnNames, inOrder), row(inOrder[1:], 2, -1)), args: []any{argument(s.types[0], 2)}},
		// The rows in the order of the key, with the names of the columns as the table stores them.
		{src: "select * from t"},
		{src: "select * from T"},
		{src: fmt.Sprintf("select %v, %v, %v from t", spelledNames[n-1], columnNames[0], columnNames[n-1])},
		{src: fmt.Sprintf("select %v from t", spelledNames[s.key])},
		{src: fmt.Sprintf("select %v from t", list(spelledNames, reversed))},
		{src: "select x from t"},
		{src: `select "x" from t`},
		{src: `select "a""b" from t`},
		{src: fmt.Sprintf(`select %v, x, "y" from t`, columnNames[0])},
		{src: "select x from nosuch"},
		// The number of values, after the names.
		{src: fmt.Sprintf("insert into t values (%v)", row(inOrder[1:], 5, -1))},
		{src: fmt.Sprintf("insert into t values (%v, %v)", row(inOrder, 5, -1), literal(s.types[0], 5))},
		{src: fmt.Sprintf("insert into t (%v) values (%v)", list(spelledNames, inOrder), row(inOrder[1:], 5, -1))},
		{src: fmt.Sprintf("insert into t (%v) values (%v, 5)", list(spelledNames, without(0)), row(inOrder, 5, -1))},
		{src: fmt.Sprintf("insert into t (%v, x) values (%v)", spelledNames[0], literal(s.types[0], 5))},
		{src: fmt.Sprintf("insert into t (x, %v) values (1, 2)", spelledNames[0])},
		{src: "insert into nosuch (x) values (1, 2)"},
		{src: "insert into nosuch values (1)"},
		// A key that exists.
		{src: insert(inOrder, 1, -1)},
		{src: insertAll(1, -1)},
	}
	for left := range n {
		other := (left + 1) % n
		steps = append(steps,
			// A column left out, also the key, and also with a key that exists: NOT NULL first.
			columnsStep{src: insert(without(left), 6, -1)},
			columnsStep{src: insert(without(left), 1, -1)},
			// A value of another type than its column, also with a key that exists: the type first,
			// where SQLite has its OP_TypeCheck.
			columnsStep{src: insertAll(7, left), own: refusal(left)},
			columnsStep{src: insertAll(1, left), own: refusal(left)},
			// A value of another type, with another column left out: NOT NULL first, as in SQLite,
			// unless the column takes NULL.
			columnsStep{src: insert(without(other), 8, left), own: s.own([]int{other}, left, refusal)},
		)
	}
	// NULL for each column: as a literal, in either case, and as a nil argument, in a list in the
	// order of the table and in another. It goes into a column that takes NULL, of any type, and gets
	// SQLite's `NOT NULL constraint failed` in any other column and in the key, also when the key
	// exists, and before a value of another type in another column.
	for p := range n {
		other, k := (p+1)%n, 20+10*p
		steps = append(steps,
			columnsStep{src: fmt.Sprintf("insert into t values (%v)", rowAt(inOrder, k, -1, p, "NULL"))},
			columnsStep{src: fmt.Sprintf("insert into t values (%v)", rowAt(inOrder, 1, -1, p, "null"))},
			columnsStep{src: fmt.Sprintf("insert into t values (%v)", rowAt(inOrder, k+1, other, p, "null")), own: s.own([]int{p}, other, refusal)},
			columnsStep{src: fmt.Sprintf("insert into T (%v) values (%v)", list(spelledNames, reversed), rowAt(reversed, k+2, -1, p, "?")), args: []any{nil}},
			columnsStep{src: fmt.Sprintf("insert into t (%v) values (%v)", list(columnNames, inOrder), rowAt(inOrder, k+3, other, p, "?")), args: []any{nil}, own: s.own([]int{p}, other, refusal)},
		)
		// NULL for one column and another left out: one case, for the first column in the order of
		// the table that does not take NULL.
		for q := range n {
			if q != p {
				steps = append(steps, columnsStep{src: fmt.Sprintf("insert into t (%v) values (%v)", list(spelledNames, without(q)), rowAt(without(q), k+4+q, -1, p, "null"))})
			}
		}
	}
	// Two values of another type: the first in the order of the table, whatever the list's order.
	steps = append(steps, columnsStep{src: fmt.Sprintf("insert into t (%v) values (%v)", list(spelledNames, reversed),
		strings.Join(slices.Collect(func(yield func(string) bool) {
			for _, i := range reversed {
				v, _ := otherLiteral(s.types[i])
				if !yield(v) {
					return
				}
			}
		}), ", ")), own: refusal(0)})
	if n == 3 {
		// Two columns left out: SQLite names the first in the order of the table.
		steps = append(steps, columnsStep{src: insert([]int{2}, 9, -1)}, columnsStep{src: insert([]int{1}, 9, -1)}, columnsStep{src: insert([]int{0}, 9, -1)})
	}
	// An argument of another type than its column, which the message names by its number.
	args := make([]any, n)
	for i := range n {
		args[i] = argument(s.types[i], 10)
	}
	last, wrongType, kind := n-1, "text", "text"
	if s.types[n-1] == "text" {
		wrongType, kind = "int", "an integer"
	}
	args[last] = argument(wrongType, 10)
	steps = append(steps,
		columnsStep{src: "insert into t values (" + params + ")", args: args,
			own: fmt.Sprintf("s4ql: not supported: argument %v, which is %v, for the column t.%v of type %v", n, kind, storedNames[last], strings.ToUpper(s.types[last]))},
		columnsStep{src: "select * from t"},
		columnsStep{src: "drop table u"},
	)
	return steps
}

// TestEngine_columns compares tables of two and three columns of mixed types, with the key at each
// position, with SQLite: the answer of each statement, which is its error with its codes and
// message, or the names and declared types of its columns and its rows in their order; the count of
// changes after it; and the tables at the end, also after a replay of the log into an empty cache. A
// statement that the engine refuses by its own rule, for a value of another type than its column, is
// checked against the message of the rule, and must leave the count of changes.
func TestEngine_columns(t *testing.T) {
	for _, s := range shapes() {
		t.Run(s.String(), func(t *testing.T) {
			i := newInstance(t, engine.Options{})
			o := newOracle(t)
			for _, step := range s.steps() {
				query := strings.HasPrefix(step.src, "select")
				var got answer
				if query {
					got = i.queryValues(step.src, values(step.args))
				} else {
					got = i.execValues(step.src, values(step.args))
				}
				if step.own != "" {
					before, _ := o.changes(t)
					is.Equal(t, "not supported", got.err, step.src)
					is.Equal(t, step.own, got.message, step.src)
					is.Equal(t, before, i.changes, "the count of changes after "+step.src)
					continue
				}
				want := o.run(t, step.src, oracleArgs(step.args)...)
				if !query {
					want.columns, want.rows = "", nil
				}
				is.Equal(t, want.String(), got.String(), "SQLite and the engine: "+step.src)
				changes, _ := o.changes(t)
				is.Equal(t, changes, i.changes, "the count of changes after "+step.src)
			}
			want := o.state(t)
			is.True(t, strings.Contains(want, "t ("), want)
			is.Equal(t, want, i.state())
			i.restart(true, engine.Options{})
			is.Equal(t, want, i.state(), "after a replay of the log")
		})
	}

	t.Run("should create, fill, and read a table of as many columns as SQLite allows, and replay it", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		o := newOracle(t)
		vs := make([]string, 2000)
		for j := range vs {
			vs[j] = fmt.Sprint(j)
		}
		for _, src := range []string{"create table t (" + manyColumns(2000) + ") strict, without rowid", "insert into t values (" + strings.Join(vs, ", ") + ")", "select c1999, c0, c1999 from t"} {
			want := o.run(t, src)
			if !strings.HasPrefix(src, "select") {
				want.columns, want.rows = "", nil
				is.Equal(t, want.String(), i.exec(src).String(), src)
				continue
			}
			is.Equal(t, `columns "c1999 INT, c0 INT, c1999 INT", rows ["1999,0,1999"]`, want.String())
			is.Equal(t, want.String(), i.query(src).String())
		}
		want := o.state(t)
		is.Equal(t, want, i.state())
		i.restart(true, engine.Options{})
		is.Equal(t, want, i.state(), "after a replay of the log")
	})
}

func TestEngine_nulls(t *testing.T) {
	const create = "create table t (a text, k int primary key, b real not null, c blob) strict, without rowid"

	t.Run("should create a table whose columns take NULL, with nullable in the commit object for those columns only, and the same table after a replay", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		o := newOracle(t)
		for _, src := range []string{create, "create table u (k int not null primary key, v text not null) strict, without rowid"} {
			is.Equal(t, "", i.exec(src).err, src)
			is.Equal(t, "", o.run(t, src).err, src)
		}
		var nullable []string
		for _, c := range readLog(i.store) {
			for _, ch := range c.changes {
				for _, column := range ch.GetCreateTable().GetColumns() {
					nullable = append(nullable, fmt.Sprint(ch.GetCreateTable().GetTable(), ".", column.GetName(), " ", column.HasNullable(), " ", column.GetNullable()))
				}
			}
		}
		is.Equal(t, "[t.a true true t.k false false t.b false false t.c true true u.k false false u.v false false]", fmt.Sprint(nullable))
		want := "t (a TEXT NULL, k INT, b REAL, c BLOB NULL): \nu (k INT, v TEXT): \n"
		is.Equal(t, want, o.state(t))
		is.Equal(t, want, i.state())
		i.restart(true, engine.Options{})
		is.Equal(t, want, i.state(), "after a replay of the log")
		// The replayed table still takes NULL where it did, and refuses it elsewhere.
		for _, src := range []string{"insert into t (k, b) values (1, 1.5)", "insert into t (k, a) values (2, 'x')"} {
			is.Equal(t, o.run(t, src).String(), i.exec(src).String(), src)
		}
		is.Equal(t, o.state(t), i.state())
	})

	t.Run("should take NULL as a literal, as a nil argument, and for a column that the list leaves out, give it back as NULL, and annotate the nil argument as NULL", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		o := newOracle(t)
		cases := []struct {
			src  string
			args []any
		}{
			{src: create},
			{src: "insert into t values (null, 1, 1.5, NULL)"},
			{src: "insert into t values (?, 2, 2.5, ?)", args: []any{nil, nil}},
			{src: "insert into t (b, k) values (3.5, 3)"},
			{src: "insert into t (c, k, b) values (?, ?, ?)", args: []any{[]byte{}, int64(4), 4.5}},
			{src: "insert into t (k, a, b) values (?, ?, ?)", args: []any{int64(5), "", 5.5}},
			{src: "select * from t"},
			{src: "select c, a, k, c from t"},
		}
		for _, c := range cases {
			want := o.run(t, c.src, oracleArgs(c.args)...)
			var got answer
			if strings.HasPrefix(c.src, "select") {
				got = i.queryValues(c.src, values(c.args))
			} else {
				want.columns, want.rows = "", nil
				got = i.execValues(c.src, values(c.args))
			}
			is.Equal(t, want.String(), got.String(), c.src)
			changes, _ := o.changes(t)
			is.Equal(t, changes, i.changes, c.src)
		}
		is.Equal(t, `columns "a TEXT, k INT, b REAL, c BLOB", rows ["null,1,real 1.5,null" "null,2,real 2.5,null" "null,3,real 3.5,null" "null,4,real 4.5,x''" "\"\",5,real 5.5,null"]`, i.query("select * from t").String())
		rows, err := i.c.Query(t.Context(), "select a from t")
		is.NotError(t, err)
		for row, err := range rows.All() {
			is.NotError(t, err)
			is.True(t, row[0].HasNull() || row[0].GetText() == "", fmt.Sprint(row[0]))
		}
		is.Equal(t, fmt.Sprint([]string{"insert into t values (?, 2, 2.5, ?) with null with null"}), fmt.Sprint(logAnnotations(t, i.store)[2]))
		var params []string
		for _, c := range readLog(i.store) {
			for _, ps := range c.params {
				for _, p := range ps {
					params = append(params, fmt.Sprint(p.WhichKind()))
				}
			}
		}
		is.Equal(t, "[null null blob integer real integer text real]", fmt.Sprint(params))
		want := o.state(t)
		is.Equal(t, want, i.state())
		i.restart(true, engine.Options{})
		is.Equal(t, want, i.state(), "after a replay of the log")
	})

	t.Run("should give SQLite's NOT NULL constraint failed for NULL in the key or in a column that does not take it, given or left out, for the first such column in the order of the table, before the type of a value and a key that exists", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		o := newOracle(t)
		for _, src := range []string{create, "insert into t values ('a', 1, 1.5, x'01')"} {
			is.Equal(t, "", i.exec(src).err)
			is.Equal(t, "", o.run(t, src).err)
		}
		cases := []struct {
			src  string
			args []any
			want string
		}{
			{src: "insert into t values ('a', null, 1.5, x'01')", want: "NOT NULL constraint failed: t.k"},
			{src: "insert into t values ('a', ?, 1.5, x'01')", args: []any{nil}, want: "NOT NULL constraint failed: t.k"},
			{src: "insert into t (a, b) values ('a', 1.5)", want: "NOT NULL constraint failed: t.k"},
			{src: "insert into t (k, a) values (2, 'a')", want: "NOT NULL constraint failed: t.b"},
			{src: "insert into t values (null, 2, null, null)", want: "NOT NULL constraint failed: t.b"},
			{src: "insert into t values (null, 2, ?, null)", args: []any{nil}, want: "NOT NULL constraint failed: t.b"},
			// A given NULL and a column left out are one case, in the order of the table.
			{src: "insert into t (b, a) values (null, 'a')", want: "NOT NULL constraint failed: t.k"},
			{src: "insert into t (k, a) values (null, 'a')", want: "NOT NULL constraint failed: t.k"},
			{src: "insert into t (c, b, a) values (?, ?, ?)", args: []any{nil, nil, nil}, want: "NOT NULL constraint failed: t.k"},
			// Before a value of another type, also in a column before it.
			{src: "insert into t values (1, 2, null, null)", want: "NOT NULL constraint failed: t.b"},
			{src: "insert into t (a, c, k) values (?, ?, 2)", args: []any{int64(1), "x"}, want: "NOT NULL constraint failed: t.b"},
			// Before a key that exists.
			{src: "insert into t values ('a', 1, null, x'01')", want: "NOT NULL constraint failed: t.b"},
			{src: "insert into t (k, c) values (1, null)", want: "NOT NULL constraint failed: t.b"},
		}
		for n, c := range cases {
			// An insert that succeeds before each, so that the error shows that it sets the count of
			// changes to 0.
			before := fmt.Sprintf("insert into t values ('x', %v, 1.5, null)", 100+n)
			is.Equal(t, "", o.run(t, before).err)
			is.Equal(t, "", i.exec(before).err)
			want := o.run(t, c.src, oracleArgs(c.args)...)
			is.Equal(t, "19/1299: "+c.want, want.err, "SQLite: "+c.src)
			got := i.execValues(c.src, values(c.args))
			is.Equal(t, want.String(), got.String(), c.src)
			changes, _ := o.changes(t)
			is.Equal(t, int64(0), changes, c.src)
			is.Equal(t, changes, i.changes, c.src)
			_, err := i.c.Exec(t.Context(), c.src, values(c.args)...)
			se, ok := errors.AsType[*sqlerr.Error](err)
			is.True(t, ok, c.src)
			is.Equal(t, sqlerr.Error{Code: 19, ExtendedCode: 1299, Message: c.want, Offset: -1}, *se)
		}
		is.Equal(t, o.state(t), i.state())
	})

	t.Run("should take NULL in a column of any type and give it no type: a value of another type in another column is refused by s4ql's rule, and NULL in its own is not", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(create).err)
		for _, c := range []struct {
			src  string
			args []any
			want string
		}{
			{src: "insert into t values (null, 1, 1.5, 'x')", want: "s4ql: not supported: a string literal for the column t.c of type BLOB"},
			{src: "insert into t values (?, 1, 1, null)", args: []any{nil}, want: "s4ql: not supported: an integer literal for the column t.b of type REAL"},
			{src: "insert into t (k, b, a) values (1, 1.5, ?)", args: []any{int64(1)}, want: "s4ql: not supported: argument 1, which is an integer, for the column t.a of type TEXT"},
		} {
			got := i.execValues(c.src, values(c.args))
			is.Equal(t, "not supported", got.err, c.src)
			is.Equal(t, c.want, got.message, c.src)
		}
		is.Equal(t, "t (a TEXT NULL, k INT, b REAL, c BLOB NULL): \n", i.state())
	})

	t.Run("should take NULL in a transaction, also after a roll back to the savepoint of a statement that failed, and refuse it in a read-only one", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(create).err)
		is.NotError(t, i.c.Begin(t.Context()))
		is.Equal(t, "", i.execValues("insert into t values (?, ?, ?, ?)", values([]any{nil, int64(1), 1.5, nil})).err)
		is.Equal(t, "19/1299: NOT NULL constraint failed: t.b", i.execValues("insert into t values (?, ?, ?, ?)", values([]any{nil, int64(2), nil, nil})).err)
		is.Equal(t, "19/1555: UNIQUE constraint failed: t.k", i.execValues("insert into t values (?, ?, ?, ?)", values([]any{nil, int64(1), 1.5, nil})).err)
		is.Equal(t, "", i.execValues("insert into t (k, b) values (?, ?)", values([]any{int64(2), 2.5})).err)
		is.NotError(t, i.c.Rollback(t.Context()))
		is.Equal(t, "t (a TEXT NULL, k INT, b REAL, c BLOB NULL): \n", i.state())
		is.NotError(t, i.c.Begin(t.Context()))
		is.Equal(t, "", i.execValues("insert into t values (?, ?, ?, ?)", values([]any{"a", int64(1), 1.5, nil})).err)
		is.Equal(t, "19/1299: NOT NULL constraint failed: t.b", i.exec("insert into t (k) values (2)").err)
		is.Equal(t, "", i.exec("insert into t (k, b) values (2, 2.5)").err)
		is.NotError(t, i.c.Commit(t.Context()))
		is.NotError(t, i.c.BeginReadOnly(t.Context()))
		is.Equal(t, "8/8: attempt to write a readonly database", i.exec("insert into t (k, b) values (3, null)").err)
		is.Equal(t, `columns "a TEXT, c BLOB", rows ["\"a\",null" "null,null"]`, i.query("select a, c from t").String())
		is.NotError(t, i.c.Commit(t.Context()))
		want := "t (a TEXT NULL, k INT, b REAL, c BLOB NULL): \"a\",1,real 1.5,null null,2,real 2.5,null\n"
		is.Equal(t, want, i.state())
		is.Equal(t, fmt.Sprint([][]string{
			{create},
			{`insert into t values (?, ?, ?, ?) with "a" with 1 with real 1.5 with null`, "insert into t (k, b) values (2, 2.5)"},
		}), fmt.Sprint(logAnnotations(t, i.store)))
		i.restart(true, engine.Options{})
		is.Equal(t, want, i.state(), "after a replay of the log")
	})

	t.Run("should check a text with NULL at once, without the database", func(t *testing.T) {
		for _, src := range []string{create, "insert into t values (null, ?, null, null)"} {
			s, err := stmt.Check(src)
			is.NotError(t, err)
			is.NotError(t, engine.Check(src, s.Parameters()), src)
		}
	})
}

// whereTrio is an engine that reads the rows by the key, one that reads every row of a table, and
// SQLite, which run the same statements.
type whereTrio struct {
	t          *testing.T
	byKey, all *instance
	o          *oracle
}

// newWhereTrio with the statements of setup run on each, which must give the same answers.
func newWhereTrio(t *testing.T, setup ...string) *whereTrio {
	t.Helper()
	var all engine.Options
	all.ScanAll()
	w := &whereTrio{t: t, byKey: newInstance(t, engine.Options{}), all: newInstance(t, all), o: newOracle(t)}
	for _, src := range setup {
		w.exec(src)
	}
	return w
}

// exec src, a statement that is no query, on each, and require the same answer of each.
func (w *whereTrio) exec(src string) {
	w.t.Helper()
	want := w.o.run(w.t, src)
	want.columns, want.rows = "", nil
	is.Equal(w.t, want.String(), w.byKey.exec(src).String(), src)
	is.Equal(w.t, want.String(), w.all.exec(src).String(), src)
}

// query src with args on each, require the same answer of each, and give SQLite's.
func (w *whereTrio) query(src string, args ...any) answer {
	w.t.Helper()
	want := w.o.run(w.t, src, oracleArgs(args)...)
	is.Equal(w.t, want.String(), w.byKey.queryValues(src, values(args)).String(), fmt.Sprintf("SQLite and the engine: %v with %#v", src, args))
	is.Equal(w.t, want.String(), w.all.queryValues(src, values(args)).String(), fmt.Sprintf("SQLite and the engine that reads every row: %v with %#v", src, args))
	return want
}

func TestEngine_where(t *testing.T) {
	// The operators of a comparison, in every spelling.
	operators := []string{"=", "==", "!=", "<>", "<", "<=", ">", ">="}

	t.Run("should compare each operator with the edges of each type and with values beside them, as SQLite does, on the key and on another column, by the key and with every row", func(t *testing.T) {
		// beside are values of each type beside the edges: values below, between, and above them, and
		// values that an edge begins or that begin an edge.
		ints := []edge{{"-2", int64(-2)}, {"2", int64(2)}, {"5", int64(5)}}
		beside := map[string][]edge{
			"int": ints, "integer": ints,
			"real": {{"0.5", 0.5}, {"-0.5", -0.5}, {"-1e300", -1e300}, {"1e300", 1e300}, {"2.0", 2.0}},
			"text": {{"'b'", "b"}, {"'aa'", "aa"}, {"'ä'", "ä"}, {"'日本'", "日本"}, {"'a\x01'", "a\x01"}, {"'~'", "~"}, {"'it'", "it"}},
			"blob": {{"x'7f'", []byte{0x7f}}, {"x'80'", []byte{0x80}}, {"x'ff00'", []byte{0xff, 0}}, {"x'616263'", []byte("abc")}, {"x'000000'", []byte{0, 0, 0}}},
		}
		for _, typ := range []string{"int", "integer", "real", "text", "blob"} {
			t.Run(typ, func(t *testing.T) {
				w := newWhereTrio(t, "create table e (k "+typ+" primary key, v "+typ+") strict, without rowid")
				es := edges[typ]
				for n, e := range es {
					v := es[(n+1)%len(es)].literal
					if n%3 == 2 {
						v = "null"
					}
					// -0.0 after 0.0 is a key that exists.
					src := fmt.Sprintf("insert into e values (%v, %v)", e.literal, v)
					want := w.o.run(t, src)
					is.Equal(t, want.String(), w.byKey.exec(src).String(), src)
					is.Equal(t, want.String(), w.all.exec(src).String(), src)
				}
				probes := slices.Concat(es, beside[typ])
				some, none := 0, 0
				for _, column := range []string{"k", "V"} {
					for _, op := range operators {
						for _, p := range probes {
							for _, a := range []answer{
								w.query(fmt.Sprintf("select * from e where %v %v %v", column, op, p.literal)),
								w.query(fmt.Sprintf("select k from e where %v %v ?", column, op), p.arg),
							} {
								if len(a.rows) > 0 && len(a.rows) < len(es)-1 {
									some++
								} else if len(a.rows) == 0 {
									none++
								}
							}
						}
						// A comparison with NULL is never true.
						is.Equal(t, 0, len(w.query(fmt.Sprintf("select * from e where %v %v null", column, op)).rows))
						is.Equal(t, 0, len(w.query(fmt.Sprintf("select * from e where %v %v ?", column, op), nil).rows))
					}
					is.Equal(t, map[string]int{"k": 0, "V": len(es) / 3}[column], len(w.query("select * from e where "+column+" is null").rows))
					w.query("select * from e where " + column + " IS NOT NULL")
				}
				is.True(t, some > 100 && none > 10, fmt.Sprint("some rows ", some, ", and none ", none))
			})
		}
	})

	t.Run("should give the rows of a table of every shape that one, two, and three conditions select, as SQLite does, by the key and with every row", func(t *testing.T) {
		for n, s := range shapes() {
			t.Run(s.String(), func(t *testing.T) {
				w := newWhereTrio(t, s.create("t"))
				// Four rows, of the numbers 10 to 40, with NULL in each column that takes it in the second
				// and the fourth.
				for k := 10; k <= 40; k += 10 {
					var vs []string
					for p, typ := range s.types {
						v := literal(typ, k)
						if s.nullable[p] && k%20 == 0 {
							v = "null"
						}
						vs = append(vs, v)
					}
					w.exec("insert into t values (" + strings.Join(vs, ", ") + ")")
				}
				type condition struct {
					src  string
					args []any
				}
				var pool []condition
				for p, typ := range s.types {
					name := spelledNames[p]
					for _, op := range operators {
						// Below, equal to, between, and above the values of the rows, NULL, and an argument.
						for _, k := range []int{5, 20, 25, 50} {
							pool = append(pool, condition{src: fmt.Sprint(name, " ", op, " ", literal(typ, k))})
						}
						pool = append(pool, condition{src: name + " " + op + " null"}, condition{src: name + " " + op + " ?", args: []any{argument(typ, 30)}})
					}
					pool = append(pool, condition{src: name + " is null"}, condition{src: name + " is not null"})
				}
				rows := 0
				for _, c := range pool {
					rows += len(w.query("select * from t where "+c.src, c.args...).rows)
				}
				r := rand.New(rand.NewPCG(uint64(n), 7))
				for range 80 {
					var srcs []string
					var args []any
					for range 2 + r.IntN(2) {
						c := pool[r.IntN(len(pool))]
						srcs, args = append(srcs, c.src), append(args, c.args...)
					}
					list := "*"
					if r.IntN(2) == 0 {
						list = columnNames[len(s.types)-1] + ", " + columnNames[0]
					}
					rows += len(w.query("select "+list+" from t where "+strings.Join(srcs, " and "), args...).rows)
				}
				// The key twice, with bounds that contradict each other, that hold one row, and that fix it.
				key, typ := spelledNames[s.key], s.types[s.key]
				lit := func(k int) string { return literal(typ, k) }
				for _, c := range []condition{
					{src: key + " > " + lit(30) + " and " + key + " < " + lit(20)},
					{src: key + " >= " + lit(20) + " and " + key + " <= " + lit(20)},
					{src: key + " > " + lit(10) + " and " + key + " <= " + lit(30) + " and " + key + " != " + lit(20)},
					{src: key + " = " + lit(20) + " and " + key + " = " + lit(30)},
					{src: key + " = " + lit(20) + " and " + key + " >= " + lit(20)},
					{src: key + " < ? and " + key + " >= ? and " + key + " < ?", args: []any{argument(typ, 40), argument(typ, 20), argument(typ, 35)}},
					{src: key + " is not null and " + key + " > " + lit(25)},
					{src: key + " is null and " + key + " > " + lit(25)},
				} {
					rows += len(w.query("select * from t where "+c.src, c.args...).rows)
				}
				is.True(t, rows > 100, fmt.Sprint(rows, " rows"))
			})
		}
	})

	t.Run("should give SQLite's errors of the names in SQLite's order, and the rows of a column in double quotes", func(t *testing.T) {
		setup := []string{"create table t (a int primary key, b text, c real not null) strict, without rowid", "insert into t values (1, 'x', 1.5)", "insert into t values (2, null, 2.5)"}
		for _, c := range []struct {
			src, want string
		}{
			{src: "select * from nosuch where a = 1", want: "1/1: no such table: nosuch"},
			{src: "select x from nosuch where y = 'a'", want: "1/1: no such table: nosuch"},
			{src: "select x from t where y = 1", want: "1/1: no such column: x"},
			{src: "select a, x from t where y is null", want: "1/1: no such column: x"},
			{src: "select * from t where x = 1", want: "1/1: no such column: x"},
			{src: "select * from t where a = 1 and B = 'x' and x = 1 and y = 1", want: "1/1: no such column: x"},
			// A test for NULL of a column that the table does not have does not stop SQLite: a later one
			// replaces its error, and the next comparison ends the walk with it.
			{src: "select * from t where x is null and y is null", want: "1/1: no such column: y"},
			{src: "select * from t where x is null and y = 1", want: "1/1: no such column: x"},
			{src: "select * from t where x is null and a = 1 and y = 1", want: "1/1: no such column: x"},
			{src: "select * from t where x is not null and a is null and y is not null and a = 2", want: "1/1: no such column: y"},
			{src: "select * from t where a is null and y is null and a = 1", want: "1/1: no such column: y"},
			{src: "select * from t where a = 1 and y is null", want: "1/1: no such column: y"},
			{src: "select * from t where x is null and a is not null", want: "1/1: no such column: x"},
			{src: `select * from t where "y" = 1`, want: `1/1: no such column: "y" - should this be a string literal in single-quotes?`},
			{src: `select * from t where x is not null and "Y""z" is null`, want: `1/1: no such column: "Y"z" - should this be a string literal in single-quotes?`},
			{src: "select * from t where rowid = 1", want: "1/1: no such column: rowid"},
			{src: "select * from t where _ROWID_ is null and oid = 1", want: "1/1: no such column: _ROWID_"},
			// The names come before the type of a value.
			{src: "select * from t where a = 'x' and y = 1", want: "1/1: no such column: y"},
			{src: "select y from t where a = 'x'", want: "1/1: no such column: y"},
			{src: `select * from t where "A" = 1 and "b" is not null`, want: `columns a INT, b TEXT, c REAL; rows 1,"x",real 1.5`},
			{src: "select c from T where A >= 1 and C < 3.0 and b is null", want: "columns c REAL; rows real 2.5"},
		} {
			pin(t, setup, c.src, c.want)
		}
	})

	t.Run("should refuse a value of another type than its column as not supported, for the first such condition in the order of the text, after SQLite's errors of the names and of a value that is too long, and take NULL", func(t *testing.T) {
		const limit = 1000
		i := newInstanceLimited(t, engine.Options{}, nil, limit)
		o := newOracleLimited(t, limit)
		for _, src := range []string{"create table T (a int primary key, b text, c real not null, d blob, e integer) strict, without rowid", "insert into t values (1, 'x', 1.5, x'01', 2)"} {
			is.Equal(t, "", i.exec(src).err, src)
			is.Equal(t, "", o.run(t, src).err, src)
		}
		long := strings.Repeat("x", limit+1)
		for _, c := range []struct {
			src  string
			args []any
			want string
		}{
			{src: "select * from t where A = '1'", want: "a string literal compared with the column T.a of type INT"},
			{src: "select * from t where a = 1.0", want: "a real literal compared with the column T.a of type INT"},
			{src: "select * from t where e = 1.0", want: "a real literal compared with the column T.e of type INTEGER"},
			{src: "select * from t where c = 1", want: "an integer literal compared with the column T.c of type REAL"},
			{src: "select * from t where c < -0", want: "an integer literal compared with the column T.c of type REAL"},
			{src: "select * from t where b = x'78'", want: "a blob literal compared with the column T.b of type TEXT"},
			{src: "select * from t where d >= 'a'", want: "a string literal compared with the column T.d of type BLOB"},
			{src: "select * from t where b is null and c = 1.5 and b = 1 and a = 'x'", want: "an integer literal compared with the column T.b of type TEXT"},
			{src: "select * from t where a = ?", args: []any{"1"}, want: "argument 1, which is text, compared with the column T.a of type INT"},
			{src: "select * from t where a = 1 and c != ?", args: []any{int64(1)}, want: "argument 1, which is an integer, compared with the column T.c of type REAL"},
			{src: "select * from t where a = ? and b = ? and c = ?", args: []any{int64(1), 2.5, "x"}, want: "argument 2, which is a real, compared with the column T.b of type TEXT"},
			{src: "select * from t where d = ?", args: []any{"x"}, want: "argument 1, which is text, compared with the column T.d of type BLOB"},
			{src: "select * from t where b < ?", args: []any{[]byte("x")}, want: "argument 1, which is a blob, compared with the column T.b of type TEXT"},
			{src: "select * from t where b = null and a = ? and c = 'x'", args: []any{nil}, want: "a string literal compared with the column T.c of type REAL"},
		} {
			i.changes = 7
			for _, run := range []func(string, []*s4qlpb.Value) answer{i.queryValues, i.execValues} {
				got := run(c.src, values(c.args))
				is.Equal(t, "not supported", got.err, c.src)
				is.Equal(t, "s4ql: not supported: "+c.want, got.message, c.src)
			}
			is.Equal(t, int64(7), i.changes, "the count of changes after "+c.src)
			_, err := i.c.Query(t.Context(), c.src, values(c.args)...)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), c.src)
			se, ok := errors.AsType[*sqlerr.Error](err)
			is.True(t, !ok, fmt.Sprint(se))
			// The rule of the generated tests gives the same refusal.
			s, err := stmt.Check(c.src)
			is.NotError(t, err)
			table, columns := o.columns(t, "t")
			is.Equal(t, "s4ql: not supported: "+c.want, stmttest.ConditionRule(table, columns, s.(*stmt.Select), oracleArgs(c.args)), c.src)
		}
		// These SQLite answers, as the engine does.
		for _, c := range []struct {
			src  string
			args []any
			want string
		}{
			{src: "select * from t where b = null and a = ?", args: []any{nil}, want: `columns "a INT, b TEXT, c REAL, d BLOB, e INTEGER", rows []`},
			{src: "select * from t where a = ? and b is not null", args: []any{int64(1)}, want: `columns "a INT, b TEXT, c REAL, d BLOB, e INTEGER", rows ["1,\"x\",real 1.5,x'01',2"]`},
			{src: "select * from nosuch where a = '1'", want: "1/1: no such table: nosuch"},
			{src: "select x from t where a = ?", args: []any{long}, want: "1/1: no such column: x"},
			{src: "select * from t where a = 1 and x = '1'", want: "1/1: no such column: x"},
			{src: "select * from t where b = ?", args: []any{long}, want: tooBig},
			{src: "select * from t where d = ?", args: []any{[]byte(long)}, want: tooBig},
			{src: "select * from t where a = ? and b = 'x'", args: []any{long}, want: tooBig},
			{src: "select * from t where a = 'x' and b = ?", args: []any{long}, want: tooBig},
			{src: "select * from t where b = '" + long + "'", want: tooBig},
			{src: "select * from t where a = 1.5 and b = '" + long + "'", want: tooBig},
		} {
			want := o.run(t, c.src, oracleArgs(c.args)...)
			is.Equal(t, c.want, want.String(), "SQLite: "+c.src)
			is.Equal(t, want.String(), i.queryValues(c.src, values(c.args)).String(), c.src)
		}
		// A blob literal that is too long, which SQLite takes for NULL, is SQLite's error for a value
		// that is too long, as in an insert.
		is.Equal(t, tooBig, i.query("select * from t where d = x'"+strings.Repeat("00", limit+1)+"'").err)
		is.Equal(t, `columns "a INT, b TEXT, c REAL, d BLOB, e INTEGER", rows []`, o.run(t, "select * from t where d = x'"+strings.Repeat("00", limit+1)+"'").String())
	})

	t.Run("should refuse an argument that no column holds before the instance, and number the parameters across the conditions in the order of the text", func(t *testing.T) {
		w := newWhereTrio(t, t3, "insert into t3 values (1, 'a', 1.5)", "insert into t3 values (2, 'b', 2.5)", "insert into t3 values (3, 'c', 3.5)")
		is.Equal(t, `columns "v1 INT, v2 TEXT", rows ["2,\"b\""]`, w.query("select v1, v2 from t3 where v1 >= ? and v2 != ? and v3 < ?", int64(2), "c", 3.0).String())
		is.Equal(t, `columns "v3 REAL", rows ["real 2.5"]`, w.query("select v3 from t3 where v1 = ? and v1 < 3 and v2 <= ?", int64(2), "b").String())
		for which, args := range map[string][]any{
			"argument 1, which is a real that is not a number":       {math.NaN(), "a"},
			"argument 2, which is an infinite real":                  {int64(1), math.Inf(-1)},
			"argument 2, which is text that is not valid UTF-8":      {int64(1), "\xff"},
			"argument 2, which is text that holds the character NUL": {int64(1), "a\x00"},
			"argument 1, which has no value":                         {[]byte(nil), "a"},
		} {
			got := w.byKey.queryValues("select * from t3 where v1 = ? and v2 = ?", values(args))
			is.Equal(t, "s4ql: not supported: "+which, got.message)
		}
		is.Equal(t, "s4ql: wrong number of arguments: 1 for a statement with 2 parameters", w.byKey.queryValues("select * from t3 where v1 = ? and v2 = ?", values([]any{int64(1)})).message)
		is.Equal(t, "s4ql: wrong number of arguments: 1 for a statement with 0 parameters", w.byKey.queryValues("select * from t3 where v1 = 1 and v2 is null", values([]any{int64(1)})).message)
		is.NotError(t, engine.Check("select * from t3 where v1 = ? and v2 = ?", 2))
		is.Equal(t, "s4ql: wrong number of arguments: 3 for a statement with 2 parameters", fmt.Sprint(engine.Check("select * from t3 where v1 = ? and v2 = ?", 3)))
		// Before the instance: on an instance that is closed, and with a context that has ended.
		w.byKey.close()
		got := w.byKey.queryValues("select * from t3 where v1 = ? and v2 = ?", values([]any{math.NaN(), "a"}))
		is.Equal(t, "s4ql: not supported: argument 1, which is a real that is not a number", got.message)
		_, err := w.byKey.c.Query(t.Context(), "select * from t3 where v1 = ?", values([]any{int64(1)})...)
		is.True(t, errors.Is(err, core.ErrStopped), fmt.Sprint(err))
	})

	t.Run("should select in a write transaction from its own writes, and in a read-only one from the state of its beginning, and write no annotation", func(t *testing.T) {
		const tw = "create table w (k int primary key, s text) strict, without rowid"
		pinTx(t, []string{tw, "insert into w values (1, 'a')", "insert into w values (5, null)"}, false, []txStep{
			{src: "insert into w values (3, 'c')"},
			{src: "select * from w where k > 1 and k < 5", want: `rows 3,"c"`},
			{src: "select * from w where k = ?", args: []any{3}, want: `rows 3,"c"`},
			{src: "select k from w where s is null or k = 1", want: "not supported"},
			{src: "select k from w where s is null", want: "rows 5"},
			{src: "select * from w where k = 'x'", want: "not supported"},
			{src: "select * from w where x = 1", want: "1/1: no such column: x"},
			{src: "insert into w values (2, 'b')"},
			{src: "select k from w where k <= 3 and s >= 'b'", want: "rows 2 3"},
		}, true, "w (k INT, s TEXT NULL): 1,\"a\" 2,\"b\" 3,\"c\" 5,null\n")
		i := pinTx(t, []string{tw, "insert into w values (1, 'a')"}, true, []txStep{
			{src: "select * from w where k >= 1", want: `rows 1,"a"`},
			{src: "select * from w where k = ? and s = ?", args: []any{1, "a"}, want: `rows 1,"a"`},
			{src: "select * from w where s = 1", want: "not supported"},
		}, true, "w (k INT, s TEXT NULL): 1,\"a\"\n")
		is.Equal(t, 2, len(logAnnotations(t, i.store)))

		// The rows of a query with WHERE in a transaction keep its other statements out until they end.
		i = newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(tw).err)
		is.Equal(t, "", i.exec("insert into w values (1, 'a')").err)
		is.NotError(t, i.c.Begin(t.Context()))
		rows, err := i.c.Query(t.Context(), "select * from w where k = 1")
		is.NotError(t, err)
		_, err = i.c.Exec(t.Context(), "select * from w where k = 1")
		is.True(t, errors.Is(err, engine.ErrRowsOpen))
		is.NotError(t, rows.Close())
		is.NotError(t, i.c.Rollback(t.Context()))
	})

	t.Run("should select the same rows after a replay of the log into an empty cache", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		o := newOracle(t)
		for _, src := range []string{"create table r (k text primary key, v real) strict, without rowid", "insert into r values ('b', 2.5)", "insert into r values ('a', null)", "insert into r values ('c', -0.0)"} {
			is.Equal(t, "", i.exec(src).err)
			is.Equal(t, "", o.run(t, src).err)
		}
		queries := []string{"select * from r where k > 'a'", "select * from r where v = 0.0", "select * from r where v is null", "select * from r where k = 'b' and v >= 2.5"}
		var before []string
		for _, src := range queries {
			want := o.run(t, src).String()
			is.Equal(t, want, i.query(src).String(), src)
			before = append(before, want)
		}
		i.restart(true, engine.Options{})
		for n, src := range queries {
			is.Equal(t, before[n], i.query(src).String(), "after a replay: "+src)
		}
	})

	t.Run("should check a text with WHERE at once, without the database", func(t *testing.T) {
		for src, n := range map[string]int{"select * from t where v = 1": 0, "select v from t where v >= ? and v < ? and w is not null": 2} {
			is.NotError(t, engine.Check(src, n), src)
		}
	})
}

// changeTrio is SQLite and two instances of the engine, one that reads the rows of a statement by the
// key and one that reads every row, which run the same statements.
type changeTrio struct {
	t          *testing.T
	byKey, all *instance
	o          *oracle
}

// newChangeTrio with the length limit limit, 0 for SQLite's default, and the statements of setup
// run on each.
func newChangeTrio(t *testing.T, limit int, setup ...string) *changeTrio {
	t.Helper()
	var all engine.Options
	all.ScanAll()
	o := newOracle(t)
	if limit > 0 {
		o = newOracleLimited(t, limit)
	}
	w := &changeTrio{t: t, byKey: newInstanceLimited(t, engine.Options{}, nil, limit), all: newInstanceLimited(t, all, nil, limit), o: o}
	for _, src := range setup {
		is.Equal(t, "", w.run(src).err, src)
	}
	return w
}

// run src, a statement that is no query, with args on each, and require SQLite's answer, SQLite's
// count of changes, and SQLite's state of each instance of the engine afterwards. It gives SQLite's
// answer.
func (w *changeTrio) run(src string, args ...any) answer {
	w.t.Helper()
	want := w.o.run(w.t, src, oracleArgs(args)...)
	want.columns, want.rows = "", nil
	changes, _ := w.o.changes(w.t)
	state := w.o.state(w.t)
	what := fmt.Sprintf("%v with %#v", src, args)
	for _, i := range []*instance{w.byKey, w.all} {
		is.Equal(w.t, want.String(), i.execValues(src, values(args)).String(), "SQLite and the engine: "+what)
		is.Equal(w.t, changes, i.changes, "the count of changes after "+what)
		is.Equal(w.t, state, i.state(), "the state after "+what)
	}
	return want
}

// own runs src with args on each instance of the engine, and not in SQLite, and requires the
// engine's own answer: the error err, as [answer] has it, with the message, and no change to the
// state or to the count of changes.
func (w *changeTrio) own(err, message, src string, args ...any) {
	w.t.Helper()
	what := fmt.Sprintf("%v with %#v", src, args)
	for _, i := range []*instance{w.byKey, w.all} {
		state, changes := i.state(), i.changes
		got := i.execValues(src, values(args))
		is.Equal(w.t, err, got.err, what)
		is.Equal(w.t, message, got.message, what)
		is.Equal(w.t, changes, i.changes, "the count of changes after "+what)
		is.Equal(w.t, state, i.state(), "the state after "+what)
	}
}

func TestEngine_update(t *testing.T) {
	// t3 with three rows, and n, whose column r takes NULL, with two.
	setup := []string{t3, "insert into t3 values (1, 'a', 1.5)", "insert into t3 values (2, 'b', 2.5)", "insert into t3 values (3, 'c', 3.5)",
		"create table n (k int primary key, r real, s text not null) strict, without rowid", "insert into n values (1, 1.5, 'a')", "insert into n values (2, null, 'b')"}

	cases := []struct {
		name string
		src  string
		args []any
		want string
	}{
		{name: "should update every row without WHERE, and count each, also one whose values do not change", src: "update t3 set v2 = 'b'"},
		{name: "should update the rows of a WHERE by the key", src: "update T3 set V3 = -0.0, v2 = 'z' where v1 = 2"},
		{name: "should update the rows of a WHERE by a range of the key", src: "update t3 set v2 = 'z' where v1 > 1 and v1 <= 3"},
		{name: "should update the rows of a WHERE on another column", src: "update t3 set v3 = 9.5 where v2 != 'b'"},
		{name: "should update no row and count none for a WHERE that holds for none", src: "update t3 set v2 = 'z' where v1 = 9"},
		{name: "should update no row for a comparison with NULL", src: "update t3 set v2 = 'z' where v2 = null"},
		{name: "should bind the arguments of SET before those of WHERE", src: "update t3 set v2 = ?, v3 = ? where v1 >= ? and v2 < ?", args: []any{"y", 7.5, int64(2), "c"}},
		{name: "should change each row that WHERE finds once, when the update changes the column that WHERE reads", src: "update t3 set v2 = 'c' where v2 < 'c'"},
		{name: "should change the key of a row to a key that no row has", src: "update t3 set v1 = 9 where v1 = 2"},
		{name: "should change the key of a row that a range of the key finds to one outside the range, once", src: "update t3 set v1 = -1, v2 = 'm' where v1 >= 3"},
		{name: "should change the key of a row to the key it has, which is no change of the key", src: "update t3 set v1 = 1, v2 = 'z' where v1 = 1"},
		{name: "should give that a key exists for a new key that another row has, and change nothing", src: "update t3 set v1 = 2 where v1 = 1", want: "19/1555: UNIQUE constraint failed: t3.v1"},
		{name: "should give that a key exists for a new key of two rows, and change nothing", src: "update t3 set v1 = 7, v2 = 'q' where v1 >= 2", want: "19/1555: UNIQUE constraint failed: t3.v1"},
		{name: "should give that a key exists for every row of a table to one key", src: "update t3 set v1 = 3", want: "19/1555: UNIQUE constraint failed: t3.v1"},
		{name: "should give that there is no table", src: "update t4 set x = 1 where y = 1", want: "1/1: no such table: t4"},
		{name: "should give the first column of SET that the table does not have, in the order of the text", src: "update t3 set v2 = 'x', z = 1, y = 2", want: "1/1: no such column: z"},
		{name: "should give a column of SET in double quotes that the table does not have without the question about quotes", src: `update t3 set "Z""z" = 1`, want: `1/1: no such column: Z"z`},
		{name: "should give a column of SET that the table does not have before one of WHERE", src: "update t3 set z = 1 where y is null", want: "1/1: no such column: z"},
		{name: "should give a column of WHERE that the table does not have by the rule of a select", src: "update t3 set v2 = 'x' where y is null and x = 1", want: "1/1: no such column: y"},
		{name: "should give the later of two tests for NULL of columns that the table does not have", src: "update t3 set v2 = 'x' where x is null and y is null", want: "1/1: no such column: y"},
		{name: "should give a column of WHERE in double quotes that the table does not have with the question about quotes", src: `update t3 set v2 = 'x' where "Q" = 1`, want: `1/1: no such column: "Q" - should this be a string literal in single-quotes?`},
		{name: "should give that a column does not take NULL for a row that the update finds", src: "update t3 set v2 = null where v1 = 2", want: "19/1299: NOT NULL constraint failed: t3.v2"},
		{name: "should give no error for NULL in a column that does not take it when the update finds no row", src: "update t3 set v2 = null where v1 = 9"},
		{name: "should give the first column that does not take NULL in the order of the table", src: "update t3 set v3 = null, v2 = null where v1 = 1", want: "19/1299: NOT NULL constraint failed: t3.v2"},
		{name: "should give that the key does not take NULL", src: "update t3 set v3 = 1.5, v1 = null", want: "19/1299: NOT NULL constraint failed: t3.v1"},
		{name: "should give that a column does not take NULL before that a key exists", src: "update t3 set v1 = 2, v3 = ? where v1 = 1", args: []any{nil}, want: "19/1299: NOT NULL constraint failed: t3.v3"},
		{name: "should give that a column does not take NULL before the type of another column", src: "update t3 set v3 = 'x', v2 = null where v1 = 1", want: "19/1299: NOT NULL constraint failed: t3.v2"},
		{name: "should take NULL in a column that takes it, as a literal and as an argument", src: "update n set r = ? where k = ?", args: []any{nil, int64(1)}},
		{name: "should take NULL in a column that takes it for every row", src: "update n set r = null, s = 'c'"},
		{name: "should update the rows whose column is NULL", src: "update n set r = 0.5 where r is null"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newChangeTrio(t, 0, setup...)
			is.Equal(t, c.want, w.run(c.src, c.args...).err)
		})
	}

	t.Run("should refuse a value of another type than its column as not supported, after the errors of the names and NOT NULL, whether the update finds a row or not", func(t *testing.T) {
		w := newChangeTrio(t, 0, setup...)
		w.run("update t3 set v2 = 'k'")
		w.own("not supported", "s4ql: not supported: an integer literal for the column t3.v2 of type TEXT", "update t3 set v2 = 1 where v1 = 1")
		w.own("not supported", "s4ql: not supported: an integer literal for the column t3.v2 of type TEXT", "update t3 set v2 = 1 where v1 = 9")
		w.own("not supported", "s4ql: not supported: a string literal for the column t3.v3 of type REAL", "update t3 set v3 = '1.5', v2 = null where v1 = 9")
		w.own("not supported", "s4ql: not supported: a real literal for the column t3.v1 of type INT", "update t3 set v1 = 1.0 where v1 = 1")
		// The first in the order of the table.
		w.own("not supported", "s4ql: not supported: an integer literal for the column t3.v2 of type TEXT", "update t3 set v3 = 'x', v2 = 1")
		w.own("not supported", "s4ql: not supported: argument 2, which is text, for the column t3.v3 of type REAL", "update t3 set v2 = ?, v3 = ? where v1 = ?", "a", "1.5", int64(1))
		// The refusal of a value of WHERE comes first, as for a select, also before NOT NULL.
		w.own("not supported", "s4ql: not supported: a string literal compared with the column t3.v1 of type INT", "update t3 set v2 = 1 where v1 = 'x'")
		w.own("not supported", "s4ql: not supported: a string literal compared with the column t3.v1 of type INT", "update t3 set v2 = null where v1 = '1'")
		is.Equal(t, "1/1: no such column: z", w.run("update t3 set v2 = 1, z = 2").err)
		is.Equal(t, "1/1: no such column: z", w.run("update t3 set v2 = 1 where z = 2").err)
		is.Equal(t, "19/1299: NOT NULL constraint failed: t3.v3", w.run("update t3 set v2 = 1, v3 = null where v1 = 3").err)
		is.Equal(t, int64(0), w.byKey.changes)
	})

	t.Run("should give SQLite's error for a value or a row that is too long, in SQLite's order", func(t *testing.T) {
		const limit = 1000
		w := newChangeTrio(t, limit, "create table t (k int primary key, v text, b blob) strict, without rowid",
			"insert into t values (1, 'a', x'00')", "insert into t values (2, 'b', null)")
		long, half := strings.Repeat("x", limit+1), strings.Repeat("y", limit/2)
		is.Equal(t, "18/18: string or blob too big", w.run("update t set v = ? where k = 9", long).err)
		is.Equal(t, "18/18: string or blob too big", w.run("update t set v = 'a' where v = ?", long).err)
		is.Equal(t, "1/1: no such column: z", w.run("update t set z = ? where k = 9", long).err)
		is.Equal(t, "1/1: no such column: z", w.run("update t set v = ? where z = 1", long).err)
		is.Equal(t, "", w.run("update t set v = ? where k = 1", half).err)
		is.Equal(t, "18/18: string or blob too big", w.run("update t set b = ? where k = 1", []byte(half)).err)
		is.Equal(t, "18/18: string or blob too big", w.run("update t set k = 2, b = ? where k = 1", []byte(half)).err)
		is.Equal(t, "19/1555: UNIQUE constraint failed: t.k", w.run("update t set k = 2, b = ? where k = 1", []byte("z")).err)
		is.Equal(t, "", w.run("update t set k = 3, b = ? where k = 1", []byte("z")).err)
		is.Equal(t, "", w.run("delete from t where v = ?", half).err)
		// A literal that is too long is too big for s4ql also when the update finds no row, where SQLite
		// steps no value: the known difference of docs/todo.md.
		w.own("18/18: string or blob too big", "string or blob too big", "update t set v = '"+long+"' where k = 9")
	})

	t.Run("should update a table of 2000 columns, with every column but the key in SET", func(t *testing.T) {
		w := newChangeTrio(t, 0, "create table t ("+manyColumns(2000)+") strict, without rowid")
		var zeros, sets []string
		for i := range 2000 {
			zeros = append(zeros, "0")
			if i > 0 {
				sets = append(sets, fmt.Sprint("c", i, " = ", i))
			}
		}
		w.run("insert into t values (" + strings.Join(zeros, ", ") + ")")
		is.Equal(t, "", w.run("update t set "+strings.Join(sets, ", ")+" where c0 = 0").err)
		is.Equal(t, "19/1299: NOT NULL constraint failed: t.c1", w.run("update t set c1999 = null, c1 = null").err)
	})

	t.Run("should update and delete in a table of every shape, as SQLite does, by the key and with every row", func(t *testing.T) {
		for _, s := range shapes() {
			t.Run(s.String(), func(t *testing.T) {
				w := newChangeTrio(t, 0, s.create("t"))
				inserts := func() {
					for k := 10; k <= 40; k += 10 {
						var vs []string
						for p, typ := range s.types {
							v := literal(typ, k)
							if s.nullable[p] && k%20 == 0 {
								v = "null"
							}
							vs = append(vs, v)
						}
						w.run("insert into t values (" + strings.Join(vs, ", ") + ")")
					}
				}
				inserts()
				key, keyType := spelledNames[s.key], s.types[s.key]
				errs := map[string]int{}
				for p, typ := range s.types {
					column := spelledNames[p]
					for _, step := range []struct {
						src  string
						args []any
					}{
						{src: fmt.Sprintf("update t set %v = %v", column, literal(typ, 15))},
						{src: fmt.Sprintf("update t set %v = %v where %v = %v", column, literal(typ, 25), key, literal(keyType, 20))},
						{src: fmt.Sprintf("update t set %v = ? where %v >= ? and %v < ?", column, key, key), args: []any{argument(typ, 35), argument(keyType, 20), argument(keyType, 40)}},
						{src: fmt.Sprintf("update t set %v = null where %v = %v", column, key, literal(keyType, 30))},
						{src: fmt.Sprintf("update t set %v = ? where %v > ?", column, key), args: []any{nil, argument(keyType, 90)}},
						{src: fmt.Sprintf("update t set %v = %v where %v = %v", column, literal(typ, 45), column, literal(typ, 35))},
						{src: fmt.Sprintf("update t set %v = %v where %v is null", column, literal(typ, 5), column)},
						{src: fmt.Sprintf("update t set %v = %v where %v >= %v", column, literal(typ, 50), column, literal(typ, 10))},
						{src: fmt.Sprintf("delete from t where %v = %v", column, literal(typ, 5))},
						{src: fmt.Sprintf("delete from t where %v < ? and %v is not null", key, column), args: []any{argument(keyType, 20)}},
					} {
						if a := w.run(step.src, step.args...); a.err != "" {
							errs[a.err]++
						}
					}
					inserts()
				}
				for _, src := range []string{
					fmt.Sprintf("update t set %v = %v where %v = %v", key, literal(keyType, 50), key, literal(keyType, 10)),
					fmt.Sprintf("update t set %v = %v where %v = %v", key, literal(keyType, 20), key, literal(keyType, 30)),
					fmt.Sprintf("update t set %v = %v where %v >= %v", key, literal(keyType, 60), key, literal(keyType, 20)),
					fmt.Sprintf("update t set %v = %v where %v > %v", key, literal(keyType, 5), key, literal(keyType, 40)),
					fmt.Sprintf("update t set %v = %v where %v = %v", key, literal(keyType, 20), key, literal(keyType, 20)),
					"delete from t where " + key + " > " + literal(keyType, 30),
					"delete from t",
				} {
					if a := w.run(src); a.err != "" {
						errs[a.err]++
					}
				}
				is.True(t, errs["19/1555: UNIQUE constraint failed: t."+storedNames[s.key]] >= 2, fmt.Sprint(errs))
				if !slices.Contains(s.nullable, false) || len(s.types) == 2 && slices.Contains(s.nullable, true) {
					return
				}
				is.True(t, errs["19/1299: NOT NULL constraint failed: t."+storedNames[s.key]] >= 1, fmt.Sprint(errs))
			})
		}
	})

	t.Run("should check a text of an update at once, without the database", func(t *testing.T) {
		is.NotError(t, engine.Check("update t set v = ? where k = ?", 2))
		is.Error(t, engine.ErrArguments, engine.Check("update t set v = ? where k = ?", 1))
	})
}

func TestEngine_delete(t *testing.T) {
	setup := []string{t3, "insert into t3 values (1, 'a', 1.5)", "insert into t3 values (2, 'b', 2.5)", "insert into t3 values (3, 'c', 3.5)"}
	cases := []struct {
		name string
		src  string
		args []any
		want string
	}{
		{name: "should delete every row without WHERE, and count each", src: "delete from t3"},
		{name: "should delete the row of a key", src: "DELETE FROM T3 WHERE V1 = ?", args: []any{int64(2)}},
		{name: "should delete the rows of a range of the key", src: "delete from t3 where v1 >= 2 and v1 < 9"},
		{name: "should delete the rows of a WHERE on another column", src: "delete from t3 where v3 > 1.5 and v2 != 'c'"},
		{name: "should delete no row and count none for a WHERE that holds for none", src: "delete from t3 where v1 > 3"},
		{name: "should delete no row for a comparison with NULL", src: "delete from t3 where v1 = ?", args: []any{nil}},
		{name: "should give that there is no table", src: "delete from t4 where x = 1", want: "1/1: no such table: t4"},
		{name: "should give a column of WHERE that the table does not have", src: "delete from t3 where v1 = 1 and x = 1", want: "1/1: no such column: x"},
		{name: "should give a column of WHERE in double quotes with the question about quotes", src: `delete from t3 where "X" is null`, want: `1/1: no such column: "X" - should this be a string literal in single-quotes?`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newChangeTrio(t, 0, setup...)
			is.Equal(t, c.want, w.run(c.src, c.args...).err)
		})
	}

	t.Run("should refuse a value of WHERE of another type than its column, after the errors of the names", func(t *testing.T) {
		w := newChangeTrio(t, 0, setup...)
		w.own("not supported", "s4ql: not supported: argument 1, which is a real, compared with the column t3.v1 of type INT", "delete from t3 where v1 = ?", 1.0)
		is.Equal(t, "1/1: no such column: x", w.run("delete from t3 where v1 = 'a' and x = 1").err)
	})

	t.Run("should delete any number of rows, and leave none", func(t *testing.T) {
		w := newChangeTrio(t, 0, t1)
		for i := range 3000 {
			w.byKey.exec(fmt.Sprint("insert into t1 values (", i, ")"))
		}
		res, err := w.byKey.c.Exec(t.Context(), "delete from t1 where v1 >= 1000")
		is.NotError(t, err)
		is.Equal(t, int64(2000), res.Changes)
		res, err = w.byKey.c.Exec(t.Context(), "delete from t1")
		is.NotError(t, err)
		is.Equal(t, int64(1000), res.Changes)
		is.Equal(t, "t1 (v1 INT): \n", w.byKey.state())
	})
}
