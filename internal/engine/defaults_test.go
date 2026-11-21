package engine_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// duo is an instance of the engine and SQLite, which run the same statements.
type duo struct {
	t *testing.T
	i *instance
	o *oracle
}

// newDuo under the length limit, 0 for SQLite's default, with the statements of setup run on both,
// which must succeed.
func newDuo(t *testing.T, limit int, setup ...string) *duo {
	t.Helper()
	d := &duo{t: t, i: newInstanceLimited(t, engine.Options{}, nil, limit), o: newOracle(t)}
	if limit > 0 {
		d.o = newOracleLimited(t, limit)
	}
	for _, src := range setup {
		d.run(src).ok()
	}
	return d
}

// result of a statement that a [duo] ran.
type result struct {
	t   *testing.T
	src string
	got answer
}

// ok fails the test unless the statement succeeded.
func (r result) ok() {
	r.t.Helper()
	is.Equal(r.t, "", r.got.err, r.src)
}

// is fails the test unless the statement failed with the error want, as [answer] writes it.
func (r result) is(want string) {
	r.t.Helper()
	is.Equal(r.t, want, r.got.err, r.src)
}

// run src with args on both, as SQLite binds them, and fail the test unless both give the same
// answer and the same count of changes. A statement that the engine does not support is not run in
// SQLite, and gives the answer "not supported" and its message.
func (d *duo) run(src string, args ...any) result {
	d.t.Helper()
	var got answer
	query := strings.HasPrefix(strings.ToLower(strings.TrimSpace(src)), "select")
	if query {
		got = d.i.queryValues(src, values(args))
	} else {
		got = d.i.execValues(src, values(args))
	}
	if got.err == "not supported" {
		return result{t: d.t, src: src, got: got}
	}
	want := d.o.run(d.t, src, oracleArgs(args)...)
	if !query {
		want.columns, want.rows = "", nil
	}
	is.Equal(d.t, want.String(), got.String(), src)
	changes, _ := d.o.changes(d.t)
	is.Equal(d.t, changes, d.i.changes, "the count of changes after "+src)
	return result{t: d.t, src: src, got: got}
}

// same fails the test unless the engine and SQLite hold the same tables, defaults, and rows, and
// gives the state.
func (d *duo) same() string {
	d.t.Helper()
	want := d.o.state(d.t)
	is.Equal(d.t, want, d.i.state())
	return want
}

// everyDefault is a table with a column for each kind of default, also one that does not take NULL
// with DEFAULT NULL, and columns that take NULL without a default.
const everyDefault = "create table t (k int primary key, i int default -7, g integer not null default 9223372036854775807, " +
	"r real default .5, z real not null default -0.0, s text default 'it''s', b blob default X'aBcD', e text not null default '', " +
	"x blob default x'', u text default null, w int, m int not null default null) strict, without rowid"

func TestEngine_defaults(t *testing.T) {
	t.Run("should create a table with every kind of default, hand the defaults to the log, and have them after a replay and a restart on the same cache", func(t *testing.T) {
		d := newDuo(t, 0, everyDefault)
		want := d.same()
		is.Equal(t, "t (k INT, i INT NULL DEFAULT -7, g INTEGER DEFAULT 9223372036854775807, r REAL NULL DEFAULT real 0.5, "+
			`z REAL DEFAULT real 0, s TEXT NULL DEFAULT "it's", b BLOB NULL DEFAULT x'abcd', e TEXT DEFAULT "", x BLOB NULL DEFAULT x'', `+
			"u TEXT NULL DEFAULT null, w INT NULL, m INT DEFAULT null): \n", want)
		var defaults []string
		for _, c := range readLog(d.i.store) {
			for _, ch := range c.changes {
				for _, column := range ch.GetCreateTable().GetColumns() {
					if column.HasDefault() {
						defaults = append(defaults, column.GetName()+"="+valueText(t, column.GetDefault()))
					}
				}
			}
		}
		is.Equal(t, `[i=-7 g=9223372036854775807 r=real 0.5 z=real 0 s="it's" b=x'abcd' e="" x=x'' u=null m=null]`, fmt.Sprint(defaults))
		insert := "insert into t (k, m) values (?, 0)"
		d.run(insert, int64(1)).ok()
		for _, empty := range []bool{true, false} {
			d.i.restart(empty, engine.Options{})
			is.Equal(t, d.o.state(t), d.i.state(), fmt.Sprint("after a restart, with an empty cache: ", empty))
		}
		// The engine after the restart fills in the same defaults.
		d.run(insert, int64(2)).ok()
		d.same()
	})

	t.Run("should store the default of each column that the list of an insert leaves out, NULL where it has none, and an explicit NULL as NULL", func(t *testing.T) {
		d := newDuo(t, 0, everyDefault)
		d.run("insert into t (k, m) values (1, 0)").ok()
		d.run("insert into t (m, k, i, s, u) values (0, 2, null, ?, ?)", nil, "u").ok()
		d.run("insert into t (k, r, w, m, x) values (?, ?, 4, 5, ?)", int64(3), 1.5, []byte{}).ok()
		d.run("insert into t values (4, 1, 2, 3.5, 4.5, 'a', x'01', 'b', x'02', 'c', 5, 6)").ok()
		d.run("insert into t (k, m, b, e) values (5, 1, null, 'v')").ok()
		d.run("select * from t").ok()
		d.run("select z, k from t where z = 0.0 and r = 0.5").ok()
		d.run("select k from t where s = 'it''s' and b = x'abcd' and i = -7").ok()
		is.Equal(t, `t (k INT, i INT NULL DEFAULT -7, g INTEGER DEFAULT 9223372036854775807, r REAL NULL DEFAULT real 0.5, `+
			`z REAL DEFAULT real 0, s TEXT NULL DEFAULT "it's", b BLOB NULL DEFAULT x'abcd', e TEXT DEFAULT "", x BLOB NULL DEFAULT x'', `+
			"u TEXT NULL DEFAULT null, w INT NULL, m INT DEFAULT null): "+
			`1,-7,9223372036854775807,real 0.5,real 0,"it's",x'abcd',"",x'',null,null,0 `+
			`2,null,9223372036854775807,real 0.5,real 0,null,x'abcd',"",x'',"u",null,0 `+
			`3,-7,9223372036854775807,real 1.5,real 0,"it's",x'abcd',"",x'',null,4,5 `+
			`4,1,2,real 3.5,real 4.5,"a",x'01',"b",x'02',"c",5,6 `+
			`5,-7,9223372036854775807,real 0.5,real 0,"it's",null,"v",x'',null,null,1`+"\n", d.same())
	})

	t.Run("should give NOT NULL constraint failed for a default NULL or an explicit NULL in a column that does not take it, naming the first such column in the order of the table, also the key", func(t *testing.T) {
		d := newDuo(t, 0, everyDefault,
			"create table a (k int primary key, x int not null default null, y int not null, z int not null default 1) strict, without rowid",
			"create table b (k text not null default null primary key, v int) strict, without rowid")
		for _, c := range []struct {
			src  string
			args []any
			want string
		}{
			{src: "insert into t (k) values (1)", want: "t.m"},
			{src: "insert into t (k, m, g) values (1, 0, null)", want: "t.g"},
			{src: "insert into t (k, m, z) values (1, 0, ?)", args: []any{nil}, want: "t.z"},
			{src: "insert into a (k) values (1)", want: "a.x"},
			{src: "insert into a (k, y) values (1, 2)", want: "a.x"},
			{src: "insert into a (k, x) values (1, 2)", want: "a.y"},
			{src: "insert into a (k, x, y, z) values (1, 2, 3, null)", want: "a.z"},
			{src: "insert into b (v) values (1)", want: "b.k"},
			{src: "insert into b (k, v) values (null, 1)", want: "b.k"},
		} {
			d.run(c.src, c.args...).is("19/1299: NOT NULL constraint failed: " + c.want)
		}
		d.run("insert into a (k, x, y) values (1, 2, 3)").ok()
		d.run("insert into b (k) values ('a')").ok()
		d.same()
	})

	t.Run("should store a default of the key, and give UNIQUE constraint failed the second time that an insert leaves the key out", func(t *testing.T) {
		d := newDuo(t, 0,
			"create table t (v text, k int primary key default 7) strict, without rowid",
			"create table u (k real default -0.0 primary key, v int) strict, without rowid")
		d.run("insert into t (v) values ('a')").ok()
		d.run("insert into t (v) values ('b')").is("19/1555: UNIQUE constraint failed: t.k")
		d.run("insert into t (k, v) values (8, 'b')").ok()
		d.run("insert into u (v) values (1)").ok()
		d.run("insert into u (k, v) values (0.0, 2)").is("19/1555: UNIQUE constraint failed: u.k")
		d.run("insert into u (v) values (3)").is("19/1555: UNIQUE constraint failed: u.k")
		d.run("select k from u where k = -0.0").ok()
		is.Equal(t, `t (v TEXT NULL, k INT DEFAULT 7): "a",7 "b",8`+"\nu (k REAL DEFAULT real 0, v INT NULL): real 0,1\n", d.same())
	})

	t.Run("should give string or blob too big for a record that a default makes too long, after NOT NULL and the type, and before UNIQUE", func(t *testing.T) {
		long := strings.Repeat("a", 500)
		d := newDuo(t, 1000,
			"create table t (k int primary key default 1, a text default '"+long+"', c text) strict, without rowid",
			"create table u (k int primary key, a blob default x'"+strings.Repeat("ab", 300)+"', c blob, n int not null default null) strict, without rowid")
		d.run("insert into t (c) values (?)", strings.Repeat("c", 400)).ok()
		d.run("insert into t (k, c) values (2, ?)", strings.Repeat("c", 600)).is("18/18: string or blob too big")
		// The key 1 exists: the record comes first.
		d.run("insert into t (c) values (?)", strings.Repeat("c", 600)).is("18/18: string or blob too big")
		d.run("insert into t (c) values ('c')").is("19/1555: UNIQUE constraint failed: t.k")
		d.run("insert into u (k, c) values (1, ?)", []byte(strings.Repeat("c", 750))).is("19/1299: NOT NULL constraint failed: u.n")
		d.run("insert into u (k, c, n) values (1, ?, 1)", []byte(strings.Repeat("c", 750))).is("18/18: string or blob too big")
		got := d.run("insert into u (k, c, n) values (1, ?, 1)", strings.Repeat("c", 750))
		got.is("not supported")
		is.Equal(t, "s4ql: not supported: argument 1, which is text, for the column u.c of type BLOB", got.got.message)
		d.run("insert into u (k, c, n) values (1, x'00', 1)").ok()
		d.same()
	})

	t.Run("should keep the order of the errors of an insert with defaults in play, each pair of neighbours as SQLite gives it", func(t *testing.T) {
		long := strings.Repeat("a", 500)
		d := newDuo(t, 1000,
			"create table t (k int primary key default 1, a text default '"+long+"', c text, n int not null default null) strict, without rowid",
			"insert into t (k, c, n) values (1, 'c', 1)")
		tooLong := strings.Repeat("x", 1001)
		for _, c := range []struct {
			name, src string
			args      []any
			want      string
		}{
			{name: "a syntax error before no such table", src: "insert into nosuch (k) values (1", want: `1/1: incomplete input`},
			{name: "no such table before a column that the table does not have", src: "insert into nosuch (zz) values (1)", want: "1/1: no such table: nosuch"},
			{name: "a column that the table does not have before the number of values", src: "insert into t (c, zz) values (1)", want: "1/1: table t has no column named zz"},
			{name: "the number of values before a value that is too long", src: "insert into t (c) values (?, ?)", args: []any{tooLong, "b"}, want: "1/1: 2 values for 1 columns"},
			{name: "a value that is too long before NOT NULL of a default", src: "insert into t (c) values (?)", args: []any{tooLong}, want: "18/18: string or blob too big"},
			{name: "NOT NULL of a default before the record that a default makes too long", src: "insert into t (k, c) values (2, ?)", args: []any{strings.Repeat("c", 600)}, want: "19/1299: NOT NULL constraint failed: t.n"},
			{name: "NOT NULL of a default before UNIQUE of the key's default", src: "insert into t (c) values ('c')", want: "19/1299: NOT NULL constraint failed: t.n"},
			{name: "the record that a default makes too long before UNIQUE of the key's default", src: "insert into t (c, n) values (?, 1)", args: []any{strings.Repeat("c", 600)}, want: "18/18: string or blob too big"},
			{name: "UNIQUE of the key's default", src: "insert into t (c, n) values ('c', 1)", want: "19/1555: UNIQUE constraint failed: t.k"},
		} {
			d.run(c.src, c.args...).is(c.want)
		}
		// s4ql's refusal of a value of another type comes after NOT NULL, where SQLite gives NOT NULL,
		// and before the record and UNIQUE, where SQLite would convert the value.
		d.run("insert into t (c) values (1)").is("19/1299: NOT NULL constraint failed: t.n")
		for _, c := range []struct {
			src  string
			args []any
		}{
			{src: "insert into t (c, n) values (1, 1)"},
			{src: "insert into t (k, c, n) values (2, ?, 'x')", args: []any{strings.Repeat("c", 600)}},
		} {
			got := d.i.execValues(c.src, values(c.args))
			is.Equal(t, "not supported", got.err, c.src)
		}
		is.Equal(t, "19/1555: UNIQUE constraint failed: t.k", d.o.run(t, "insert into t (c, n) values (1, 1)").err)
		is.Equal(t, "19/3091: cannot store TEXT value in INT column t.n", d.o.run(t, "insert into t (k, c, n) values (2, ?, 'x')", strings.Repeat("c", 600)).err)
		d.same()
	})

	t.Run("should give attempt to write a readonly database before every error of a row in a read-only transaction", func(t *testing.T) {
		long := strings.Repeat("a", 500)
		d := newDuo(t, 1000,
			"create table t (k int primary key default 1, a text default '"+long+"', c text, n int not null default null) strict, without rowid",
			"insert into t (k, c, n) values (1, 'c', 1)")
		is.NotError(t, d.i.c.BeginReadOnly(t.Context()))
		d.o.run(t, "pragma query_only = 1")
		for _, c := range []struct {
			src  string
			args []any
		}{
			{src: "insert into t (c) values ('c')"},
			{src: "insert into t (k, c) values (2, 'c')"},
			{src: "insert into t (c, n) values ('c', 1)"},
			{src: "insert into t (k, c, n) values (2, ?, 1)", args: []any{strings.Repeat("c", 600)}},
			{src: "insert into t (k, c, n) values (2, 'c', 1)"},
			{src: "insert into t (k, c, n) values (2, 1, 1)"},
		} {
			got := d.i.execValues(c.src, values(c.args))
			is.Equal(t, "8/8: attempt to write a readonly database", got.err, c.src)
			is.Equal(t, got.err, d.o.run(t, c.src, oracleArgs(c.args)...).err, c.src)
		}
		// A value that is too long comes before, as SQLite gives it when it binds the value.
		d.run("insert into t (k, c, n) values (2, ?, 1)", strings.Repeat("c", 1001)).is("18/18: string or blob too big")
		is.NotError(t, d.i.c.Commit(t.Context()))
		d.o.run(t, "pragma query_only = 0")
		d.same()
	})

	t.Run("should leave an insert without a list, an update, a delete, and a select as they are", func(t *testing.T) {
		d := newDuo(t, 0, everyDefault)
		d.run("insert into t values (1, null, 2, null, 3.5, null, null, 'e', null, null, null, 4)").ok()
		d.run("insert into t values (2)").is("1/1: table t has 12 columns but 1 values were supplied")
		d.run("update t set i = null, s = null where k = 1").ok()
		d.run("update t set g = null").is("19/1299: NOT NULL constraint failed: t.g")
		d.run("update t set m = null where k = 1").is("19/1299: NOT NULL constraint failed: t.m")
		d.run("update t set e = 'f' where k = 1").ok()
		d.run("select * from t where i is null").ok()
		d.run("insert into t (k, m) values (2, 0)").ok()
		d.run("delete from t where k = 2").ok()
		d.run("select * from t").ok()
		d.same()
	})

	t.Run("should use the defaults of the table as it is in a transaction, also after a roll back, a drop, and a create with other defaults", func(t *testing.T) {
		d := newDuo(t, 0)
		begin := func() {
			is.NotError(t, d.i.c.Begin(t.Context()))
			is.Equal(t, "", d.o.run(t, "begin").err)
		}
		end := func(commit bool) {
			if commit {
				is.NotError(t, d.i.c.Commit(t.Context()))
				is.Equal(t, "", d.o.run(t, "commit").err)
				return
			}
			is.NotError(t, d.i.c.Rollback(t.Context()))
			is.Equal(t, "", d.o.run(t, "rollback").err)
		}
		begin()
		d.run("create table t (k int primary key, v int default 1) strict, without rowid").ok()
		d.run("insert into t (k) values (1)").ok()
		// A statement that fails is rolled back to its savepoint, and the table keeps its default.
		d.run("insert into t (v) values (2)").is("19/1299: NOT NULL constraint failed: t.k")
		d.run("insert into t (k) values (3)").ok()
		end(true)
		begin()
		d.run("drop table t").ok()
		d.run("create table t (k int primary key, v int default 2, w text not null default 'w') strict, without rowid").ok()
		d.run("insert into t (k) values (4)").ok()
		d.run("select * from t").ok()
		end(false)
		d.run("insert into t (k) values (5)").ok()
		begin()
		d.run("drop table t").ok()
		d.run("create table t (k int primary key, v int default 3) strict, without rowid").ok()
		d.run("insert into t (k) values (6)").ok()
		end(true)
		want := d.same()
		is.Equal(t, "t (k INT, v INT NULL DEFAULT 3): 6,3\n", want)
		d.i.restart(true, engine.Options{})
		is.Equal(t, want, d.i.state(), "after a replay of the log")
	})

	t.Run("should check a create table and an insert with defaults at once, without the database", func(t *testing.T) {
		for _, src := range []string{everyDefault, "insert into t (k, m) values (?, 0)"} {
			s, err := stmt.Check(src)
			is.NotError(t, err)
			is.NotError(t, engine.Check(src, s.Parameters()), src)
		}
		err := engine.Check("create table t (k int primary key, v real default 1) strict, without rowid", 0)
		is.True(t, errors.Is(err, stmt.ErrNotSupported))
		is.Equal(t, "s4ql: not supported: DEFAULT of another type than its column (at offset 50)", err.Error())
	})

	t.Run("should refuse as not supported a table that SQLite takes under the length limit and whose statement in the cache is longer than the limit, and go on without it", func(t *testing.T) {
		// Each default of a real in its canonical form, 5e-01, is three bytes longer than .5.
		var columns []string
		for n := range 42 {
			columns = append(columns, fmt.Sprintf("c%v real default.5", n))
		}
		src := "create table t(k int primary key," + strings.Join(columns, ",") + ")strict,without rowid"
		d := newDuo(t, 1000)
		is.Equal(t, "", d.o.run(t, src).err, "SQLite takes the table")
		is.Equal(t, "", d.o.run(t, "drop table t").err)
		got := d.i.exec(src)
		is.Equal(t, "not supported", got.err)
		is.Equal(t, "s4ql: not supported: the table t, whose statement in the cache is longer than the length limit", got.message)
		is.NotError(t, d.i.c.Begin(t.Context()))
		is.Equal(t, "not supported", d.i.exec(src).err)
		is.Equal(t, "", d.i.exec("create table u (k int primary key, v real default .5) strict, without rowid").err, "the transaction goes on")
		is.NotError(t, d.i.c.Commit(t.Context()))
		is.Equal(t, "u (k INT, v REAL NULL DEFAULT real 0.5): \n", d.i.state())
	})
}
