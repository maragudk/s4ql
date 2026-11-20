package engine_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"math"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/store/simstore"
)

// txStep is a statement of a transaction in a pinned test, with its arguments and the answer that
// it must give: an error as "code/extended: message", "rows" and the rows for a query, "" for a
// statement that changes the database, or one of the engine's own answers, which SQLite does not
// run: "not supported", "no statement", or "wrong arguments".
type txStep struct {
	src  string
	args []any
	want string
}

// own reports whether the answer of the step is the engine's own, which is not compared with
// SQLite.
func (s txStep) own() bool {
	return s.want == "not supported" || s.want == "no statement" || s.want == "wrong arguments"
}

// oracleBegin and oracleEnd are what SQLite runs to begin and to end a transaction of the kind that
// the engine's transaction is: a write transaction is SQLite's `begin immediate`, and a read-only one
// is a transaction under `pragma query_only = 1`.
func oracleBegin(readOnly bool) []string {
	if readOnly {
		return []string{"pragma query_only = 1", "begin"}
	}
	return []string{"begin immediate"}
}

func oracleEnd(readOnly, commit bool) []string {
	end := "rollback"
	if commit {
		end = "commit"
	}
	if readOnly {
		return []string{end, "pragma query_only = 0"}
	}
	return []string{end}
}

// runIn runs the statement of the step on the connection of i, and gives its answer as the step
// writes it, with the rows of a query in the order in which they came, and the answer itself, and
// keeps SQLite's count of changes as its result says.
func runIn(t *testing.T, i *instance, s txStep) (string, answer) {
	t.Helper()
	if s.want == "wrong arguments" {
		_, err := i.c.Exec(t.Context(), s.src, values(s.args)...)
		is.True(t, errors.Is(err, engine.ErrArguments), fmt.Sprint(err))
		return s.want, answer{err: wrongArguments}
	}
	if strings.Contains(strings.ToLower(s.src), "select") {
		a := i.queryValues(s.src, values(s.args))
		if a.err != "" {
			return a.err, a
		}
		return strings.TrimSpace("rows " + strings.Join(a.rows, " ")), a
	}
	a := i.execValues(s.src, values(s.args))
	return a.err, a
}

// pinTx runs the statements of setup, each a transaction of its own, and then the steps in one
// transaction, read-only if readOnly, which ends with a commit if commit and with a roll back
// otherwise, on the engine and in SQLite. Each step must give its answer, with the rows of a query in
// the order of the key, and the same answer, with the rows as a multiset, and the same count of
// changes in both, and after the end, both must hold the state want, also after a replay of the log
// into an empty cache. It gives the instance.
func pinTx(t *testing.T, setup []string, readOnly bool, steps []txStep, commit bool, want string) *instance {
	t.Helper()
	// The rows of the steps are written out in the order of the key, which is forward.
	i := newInstance(t, forward())
	o := newOracle(t)
	for _, s := range setup {
		is.Equal(t, "", i.exec(s).err, s)
		is.Equal(t, "", o.run(t, s).err, s)
	}
	if readOnly {
		is.NotError(t, i.c.BeginReadOnly(t.Context()))
	} else {
		is.NotError(t, i.c.Begin(t.Context()))
	}
	for _, s := range oracleBegin(readOnly) {
		is.Equal(t, "", o.run(t, s).err, s)
	}
	for _, s := range steps {
		text, got := runIn(t, i, s)
		is.Equal(t, s.want, text, s.src)
		if s.own() {
			continue
		}
		// SQLite leaves the order of the rows unspecified, so they are compared as a multiset.
		is.Equal(t, o.run(t, s.src, oracleArgs(s.args)...).String(), got.String(), "SQLite and the engine: "+s.src)
		changes, _ := o.changes(t)
		is.Equal(t, changes, i.changes, "the count of changes after "+s.src)
	}
	if commit {
		is.NotError(t, i.c.Commit(t.Context()))
	} else {
		is.NotError(t, i.c.Rollback(t.Context()))
	}
	for _, s := range oracleEnd(readOnly, commit) {
		is.Equal(t, "", o.run(t, s).err, s)
	}
	is.Equal(t, want, o.state(t), "SQLite")
	is.Equal(t, want, i.state(), "the engine")
	i.restart(true, forward())
	is.Equal(t, want, i.state(), "the engine after a replay of the log")
	return i
}

const (
	insert1 = "insert into t1 (v1) values (1)"
	insert2 = "insert into t1 (v1) values (2)"
	insertP = "insert into t1 (v1) values (?)"
	selectT = "select * from t1"
	textT   = "create table tt (v text primary key) strict, without rowid"
	realT   = "create table rt (v real primary key) strict, without rowid"
	blobT   = "create table bt (v blob primary key) strict, without rowid"
	tn      = "create table n (k int primary key, r real, s text not null) strict, without rowid"
)

func TestConn_Begin(t *testing.T) {
	cases := []struct {
		name     string
		setup    []string
		readOnly bool
		steps    []txStep
		commit   bool
		want     string
	}{
		{name: "should commit several inserts as one transaction", setup: []string{t1}, commit: true,
			steps: []txStep{{src: insert1}, {src: insert2}, {src: "insert into t1 (v1) values (3)"}},
			want:  "t1 (v1 INT): 1 2 3\n"},
		{name: "should read its own writes, in the order of the key", setup: []string{t1, "insert into t1 (v1) values (5)"}, commit: true,
			steps: []txStep{{src: selectT, want: "rows 5"}, {src: insert2}, {src: selectT, want: "rows 2 5"}, {src: insert1}, {src: selectT, want: "rows 1 2 5"}},
			want:  "t1 (v1 INT): 1 2 5\n"},
		{name: "should keep the transaction open and what came before after a statement that fails, which has no effect", setup: []string{t1}, commit: true,
			steps: []txStep{
				{src: insert1},
				{src: insert1, want: "19/1555: UNIQUE constraint failed: t1.v1"},
				{src: insert2},
				{src: "insert into t2 (v1) values (3)", want: "1/1: no such table: t2"},
				{src: "insert into t1 (v2) values (3)", want: "1/1: table t1 has no column named v2"},
				{src: t1, want: "1/1: table t1 already exists"},
				{src: "drop table t2", want: "1/1: no such table: t2"},
				{src: "selec * from t1", want: `1/1: near "selec": syntax error`},
				{src: "select * from t1 order by v1", want: "not supported"},
				{src: " ; ", want: "no statement"},
				{src: insertP, args: []any{3, 4}, want: "wrong arguments"},
				{src: "insert into t1 (v1) values (3)"},
				{src: selectT, want: "rows 1 2 3"},
			},
			want: "t1 (v1 INT): 1 2 3\n"},
		{name: "should see a table that it creates and not one that it drops, in the statements after them", commit: true,
			steps: []txStep{
				{src: t1}, {src: insert1}, {src: selectT, want: "rows 1"},
				{src: "drop table T1"}, {src: selectT, want: "1/1: no such table: t1"}, {src: insert1, want: "1/1: no such table: t1"},
				{src: "create table T1 (x int primary key) strict, without rowid"}, {src: "insert into t1 (x) values (7)"}, {src: selectT, want: "rows 7"},
				{src: "create table t2 (v1 int primary key) strict, without rowid"},
			},
			want: "T1 (x INT): 7\nt2 (v1 INT): \n"},
		{name: "should undo a create table and its rows at a roll back",
			setup: []string{"create table t0 (v int primary key) strict, without rowid"},
			steps: []txStep{{src: t1}, {src: insert1}, {src: "insert into t0 (v) values (1)"}, {src: "drop table t0"}},
			want:  "t0 (v INT): \n"},
		{name: "should bring back a table that it dropped, with its rows, at a roll back", setup: []string{t1, insert1, insert2},
			steps: []txStep{{src: "drop table t1"}, {src: t1}, {src: "insert into t1 (v1) values (9)"}, {src: selectT, want: "rows 9"}},
			want:  "t1 (v1 INT): 1 2\n"},
		{name: "should leave nothing of a transaction that changed nothing in the end", setup: []string{t1}, commit: true,
			steps: []txStep{{src: "create table t2 (v int primary key) strict, without rowid"}, {src: "insert into t2 (v) values (1)"}, {src: "drop table t2"}},
			want:  "t1 (v1 INT): \n"},
		{name: "should bind the arguments of the statements in a transaction", setup: []string{t1}, commit: true,
			steps: []txStep{
				{src: insertP, args: []any{-9223372036854775808}},
				{src: insertP, args: []any{9223372036854775807}},
				{src: insertP, args: []any{9223372036854775807}, want: "19/1555: UNIQUE constraint failed: t1.v1"},
				{src: insertP, args: []any{0}},
				{src: selectT, want: "rows -9223372036854775808 0 9223372036854775807"},
			},
			want: "t1 (v1 INT): -9223372036854775808 0 9223372036854775807\n"},
		{name: "should refuse the texts that end or begin a transaction as not supported, and stay open", setup: []string{t1}, commit: true,
			steps: []txStep{
				{src: insert1},
				{src: "begin", want: "not supported"}, {src: "begin immediate", want: "not supported"}, {src: "commit", want: "not supported"},
				{src: "end", want: "not supported"}, {src: "rollback", want: "not supported"}, {src: "savepoint a", want: "not supported"},
				{src: "release a", want: "not supported"}, {src: "rollback to a", want: "not supported"},
				{src: insert2},
			},
			want: "t1 (v1 INT): 1 2\n"},
		{name: "should see the state of its beginning, read only, and refuse every write with SQLite's error under query_only, after the errors of the names",
			setup: []string{t1, insert1}, readOnly: true, commit: true,
			steps: []txStep{
				{src: selectT, want: "rows 1"},
				{src: insert2, want: "8/8: attempt to write a readonly database"},
				{src: insert1, want: "8/8: attempt to write a readonly database"},
				{src: insertP, args: []any{3}, want: "8/8: attempt to write a readonly database"},
				{src: "insert into t2 (v1) values (1)", want: "1/1: no such table: t2"},
				{src: "insert into t1 (v2) values (1)", want: "1/1: table t1 has no column named v2"},
				{src: "create table t2 (v int primary key) strict, without rowid", want: "8/8: attempt to write a readonly database"},
				{src: t1, want: "1/1: table t1 already exists"},
				{src: "drop table t1", want: "8/8: attempt to write a readonly database"},
				{src: "drop table t2", want: "1/1: no such table: t2"},
				{src: "selec", want: `1/1: near "selec": syntax error`},
				{src: insertP, want: "wrong arguments"},
				{src: "commit", want: "not supported"},
				{src: selectT, want: "rows 1"},
			},
			want: "t1 (v1 INT): 1\n"},
		{name: "should update and delete in a transaction, read its own changes, and keep the transaction open after a change that fails",
			setup: []string{tn, "insert into n values (1, 1.5, 'a')", "insert into n values (2, null, 'b')"}, commit: true,
			steps: []txStep{
				{src: "update n set r = ? where k = ?", args: []any{2.5, 2}},
				{src: "select * from n", want: `rows 1,real 1.5,"a" 2,real 2.5,"b"`},
				{src: "update n set k = 1 where k = 2", want: "19/1555: UNIQUE constraint failed: n.k"},
				{src: "update n set s = null", want: "19/1299: NOT NULL constraint failed: n.s"},
				{src: "update n set s = 1", want: "not supported"},
				{src: "update n set z = 1", want: "1/1: no such column: z"},
				{src: "update n set k = 3, s = 'c' where k = 2"},
				{src: "delete from n where k = 1"},
				{src: "delete from n where k = 1"},
				{src: "insert into n values (1, null, 'd')"},
				{src: "select * from n", want: `rows 1,null,"d" 3,real 2.5,"c"`},
			},
			want: "n (k INT, r REAL NULL, s TEXT): 1,null,\"d\" 3,real 2.5,\"c\"\n"},
		{name: "should undo updates and deletes at a roll back", setup: []string{tn, "insert into n values (1, 1.5, 'a')", "insert into n values (2, null, 'b')"},
			steps: []txStep{{src: "update n set k = 5 where k = 1"}, {src: "delete from n"}, {src: "select * from n", want: "rows"}},
			want:  "n (k INT, r REAL NULL, s TEXT): 1,real 1.5,\"a\" 2,null,\"b\"\n"},
		{name: "should refuse an update and a delete in a read-only transaction with SQLite's error, after the errors of the names, whether they find a row or not",
			setup: []string{tn, "insert into n values (1, 1.5, 'a')"}, readOnly: true, commit: true,
			steps: []txStep{
				{src: "update n set r = 2.5", want: "8/8: attempt to write a readonly database"},
				{src: "update n set r = 2.5 where k = 9", want: "8/8: attempt to write a readonly database"},
				{src: "update n set s = null where k = 1", want: "8/8: attempt to write a readonly database"},
				{src: "update n set s = 1 where k = 1", want: "8/8: attempt to write a readonly database"},
				{src: "update n set k = 1 where k = 1", want: "8/8: attempt to write a readonly database"},
				{src: "delete from n", want: "8/8: attempt to write a readonly database"},
				{src: "delete from n where k = ?", args: []any{nil}, want: "8/8: attempt to write a readonly database"},
				{src: "update m set r = 2.5", want: "1/1: no such table: m"},
				{src: "update n set z = 2.5", want: "1/1: no such column: z"},
				{src: "update n set r = 2.5 where z = 1", want: "1/1: no such column: z"},
				{src: "delete from m", want: "1/1: no such table: m"},
				{src: "delete from n where z is null", want: "1/1: no such column: z"},
				{src: "update n set r = 1.5 where k = 'a'", want: "not supported"},
				{src: "select * from n", want: `rows 1,real 1.5,"a"`},
			},
			want: "n (k INT, r REAL NULL, s TEXT): 1,real 1.5,\"a\"\n"},
		{name: "should write nothing at the roll back of a read-only transaction", setup: []string{t1, insert1}, readOnly: true,
			steps: []txStep{{src: selectT, want: "rows 1"}, {src: insert2, want: "8/8: attempt to write a readonly database"}},
			want:  "t1 (v1 INT): 1\n"},
		{name: "should commit values of every type, through literals and arguments, and keep the transaction open after a value of another type",
			setup: []string{textT, realT, blobT}, commit: true,
			steps: []txStep{
				{src: "insert into tt (v) values ('b')"},
				{src: "insert into tt (v) values (?)", args: []any{"a"}},
				{src: "insert into tt (v) values (?)", args: []any{"a"}, want: `19/1555: UNIQUE constraint failed: tt.v`},
				{src: "insert into tt (v) values (1)", want: "not supported"},
				{src: "insert into tt (v) values (?)", args: []any{[]byte("c")}, want: "not supported"},
				{src: "insert into rt (v) values (?)", args: []any{math.Copysign(0, -1)}},
				{src: "insert into rt (v) values (0.0)", want: `19/1555: UNIQUE constraint failed: rt.v`},
				{src: "insert into rt (v) values (-1.5e-310)"},
				{src: "insert into bt (v) values (x'')"},
				{src: "insert into bt (v) values (?)", args: []any{[]byte{0}}},
				{src: "insert into bt (v) values ('')", want: "not supported"},
				{src: "select * from tt", want: `rows "a" "b"`},
				{src: "select * from rt", want: `rows real -1.5e-310 real 0`},
				{src: "select * from bt", want: `rows x'' x'00'`},
			},
			want: "bt (v BLOB): x'' x'00'\nrt (v REAL): real -1.5e-310 real 0\ntt (v TEXT): \"a\" \"b\"\n"},
		{name: "should undo values of every type at a roll back", setup: []string{textT, realT, blobT, "insert into tt (v) values ('a')"},
			steps: []txStep{
				{src: "insert into tt (v) values (?)", args: []any{"b"}}, {src: "insert into rt (v) values (1.5)"}, {src: "insert into bt (v) values (?)", args: []any{[]byte("b")}},
				{src: "select * from tt", want: `rows "a" "b"`},
			},
			want: "bt (v BLOB): \nrt (v REAL): \ntt (v TEXT): \"a\"\n"},
		{name: "should refuse a write of every type in a read-only transaction with SQLite's error, before the type of the value",
			setup: []string{textT, "insert into tt (v) values ('a')"}, readOnly: true, commit: true,
			steps: []txStep{
				{src: "insert into tt (v) values ('b')", want: "8/8: attempt to write a readonly database"},
				{src: "insert into tt (v) values (?)", args: []any{"a"}, want: "8/8: attempt to write a readonly database"},
				{src: "insert into tt (v) values (1)", want: "8/8: attempt to write a readonly database"},
				{src: "insert into tt (v) values (?)", args: []any{1.5}, want: "8/8: attempt to write a readonly database"},
				{src: "insert into tt (x) values ('b')", want: "1/1: table tt has no column named x"},
				{src: "select * from tt", want: `rows "a"`},
			},
			want: "tt (v TEXT): \"a\"\n"},
		{name: "should commit rows of several columns, and leave nothing of an insert of several values that fails", setup: []string{t3}, commit: true,
			steps: []txStep{
				{src: "insert into t3 values (2, 'b', 2.5)"},
				{src: "insert into t3 (v3, v2, v1) values (?, ?, ?)", args: []any{1.5, "a", 1}},
				{src: "insert into t3 (v1, v2) values (3, 'c')", want: "19/1299: NOT NULL constraint failed: t3.v3"},
				{src: "insert into t3 values (3, 'c')", want: "1/1: table t3 has 3 columns but 2 values were supplied"},
				{src: "insert into t3 (v1, v2, v3) values (3, 'c')", want: "1/1: 2 values for 3 columns"},
				{src: "insert into t3 (v1, x) values (3, 'c')", want: "1/1: table t3 has no column named x"},
				{src: "insert into t3 values (?, ?, ?)", args: []any{2, "z", 9.5}, want: "19/1555: UNIQUE constraint failed: t3.v1"},
				{src: "insert into t3 values (3, 'c', 'x')", want: "not supported"},
				{src: "select v2, v1 from t3", want: `rows "a",1 "b",2`},
				{src: "select x from t3", want: "1/1: no such column: x"},
				{src: "insert into t3 (v2, v3, v1) values ('c', 3.5, 3)"},
				{src: "select * from t3", want: `rows 1,"a",real 1.5 2,"b",real 2.5 3,"c",real 3.5`},
			},
			want: "t3 (v1 INT, v2 TEXT, v3 REAL): 1,\"a\",real 1.5 2,\"b\",real 2.5 3,\"c\",real 3.5\n"},
		{name: "should undo a table of several columns and its rows at a roll back",
			setup: []string{t3, "insert into t3 values (1, 'a', 1.5)"},
			steps: []txStep{
				{src: "insert into t3 values (2, 'b', 2.5)"},
				{src: "create table k (a blob not null, b text primary key not null) strict, without rowid"},
				{src: "insert into k (b, a) values ('x', x'01')"},
				{src: "select b, a, b from k", want: `rows "x",x'01',"x"`},
			},
			want: "t3 (v1 INT, v2 TEXT, v3 REAL): 1,\"a\",real 1.5\n"},
		{name: "should give the errors of the names and of the number of values before the write that a read-only transaction refuses, and that refusal before a column without a value and the type",
			setup: []string{t3, "insert into t3 values (1, 'a', 1.5)"}, readOnly: true, commit: true,
			steps: []txStep{
				{src: "insert into t3 values (2, 'b')", want: "1/1: table t3 has 3 columns but 2 values were supplied"},
				{src: "insert into t3 (v1, v2) values (2)", want: "1/1: 1 values for 2 columns"},
				{src: "insert into t3 (x, v1) values (2)", want: "1/1: table t3 has no column named x"},
				{src: "create table t3 (v int primary key, v text not null) strict, without rowid", want: "1/1: table t3 already exists"},
				{src: "create table u (v int primary key, v text not null) strict, without rowid", want: "1/1: duplicate column name: v"},
				{src: "insert into t3 (v1, v2) values (2, 'b')", want: "8/8: attempt to write a readonly database"},
				{src: "insert into t3 values (1, 'b', 2.5)", want: "8/8: attempt to write a readonly database"},
				{src: "insert into t3 values (2, 'b', 'x')", want: "8/8: attempt to write a readonly database"},
				{src: "select v3, v1 from t3", want: `rows real 1.5,1`},
				{src: "select v1, x from t3", want: "1/1: no such column: x"},
			},
			want: "t3 (v1 INT, v2 TEXT, v3 REAL): 1,\"a\",real 1.5\n"},
		{name: "should commit NULL, through the literal, an argument, and a column left out, and keep the transaction open after NULL in a column that does not take it",
			setup: []string{tn}, commit: true,
			steps: []txStep{
				{src: "insert into n values (1, null, 'a')"},
				{src: "insert into n values (?, ?, ?)", args: []any{2, nil, "b"}},
				{src: "insert into n (k, s) values (3, 'c')"},
				{src: "insert into n values (4, 1.5, null)", want: "19/1299: NOT NULL constraint failed: n.s"},
				{src: "insert into n (r, s) values (?, ?)", args: []any{nil, "d"}, want: "19/1299: NOT NULL constraint failed: n.k"},
				{src: "insert into n (k, r) values (?, ?)", args: []any{4, 2.5}, want: "19/1299: NOT NULL constraint failed: n.s"},
				{src: "insert into n (k, s) values (4, 'd')"},
				{src: "create table m (a text, k int primary key) strict, without rowid"},
				{src: "insert into m (k) values (1)"},
				{src: "select * from n", want: `rows 1,null,"a" 2,null,"b" 3,null,"c" 4,null,"d"`},
				{src: "select a, k from m", want: `rows null,1`},
			},
			want: "m (a TEXT NULL, k INT): null,1\nn (k INT, r REAL NULL, s TEXT): 1,null,\"a\" 2,null,\"b\" 3,null,\"c\" 4,null,\"d\"\n"},
		{name: "should undo NULL and a table whose columns take NULL at a roll back",
			setup: []string{tn, "insert into n values (1, null, 'a')"},
			steps: []txStep{
				{src: "insert into n (k, s) values (2, 'b')"},
				{src: "create table m (a text, k int primary key) strict, without rowid"},
				{src: "insert into m values (?, ?)", args: []any{nil, 1}},
			},
			want: "n (k INT, r REAL NULL, s TEXT): 1,null,\"a\"\n"},
		{name: "should refuse NULL in a read-only transaction with SQLite's error, before NOT NULL",
			setup: []string{tn}, readOnly: true, commit: true,
			steps: []txStep{
				{src: "insert into n values (1, null, 'a')", want: "8/8: attempt to write a readonly database"},
				{src: "insert into n values (?, ?, ?)", args: []any{nil, nil, nil}, want: "8/8: attempt to write a readonly database"},
				{src: "insert into n (r) values (null)", want: "8/8: attempt to write a readonly database"},
				{src: "create table m (a text, k int primary key) strict, without rowid", want: "8/8: attempt to write a readonly database"},
			},
			want: "n (k INT, r REAL NULL, s TEXT): \n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pinTx(t, c.setup, c.readOnly, c.steps, c.commit, c.want)
		})
	}

	t.Run("should count the changes as SQLite does after an insert in a read-only transaction", func(t *testing.T) {
		// After an insert that succeeds the count is 1, and the refused insert sets it as SQLite does.
		pinTx(t, []string{t1}, true, []txStep{{src: insert1, want: "8/8: attempt to write a readonly database"}}, true, "t1 (v1 INT): \n")
		i := pinTx(t, []string{t1, insert1}, true, []txStep{{src: insert2, want: "8/8: attempt to write a readonly database"}}, true, "t1 (v1 INT): 1\n")
		is.Equal(t, "", i.exec(insert2).err)
	})
}

func TestConn_netEffect(t *testing.T) {
	setup := []string{tn, "insert into n values (1, 1.5, 'a')", "insert into n values (2, null, 'b')"}
	cases := []struct {
		name  string
		steps []txStep
		// commit is the changes of the commit object of the transaction, as [logChanges] gives them.
		commit string
		want   string
	}{
		{name: "should commit nothing of a row that was inserted, updated, and deleted",
			steps:  []txStep{{src: "insert into n values (5, null, 'x')"}, {src: "update n set r = 0.5, s = 'y' where k = 5"}, {src: "update n set k = 6 where k = 5"}, {src: "delete from n where k = 6"}},
			commit: "", want: "n (k INT, r REAL NULL, s TEXT): 1,real 1.5,\"a\" 2,null,\"b\"\n"},
		{name: "should commit the row as it is of a key that was changed away and back",
			steps:  []txStep{{src: "update n set k = 7 where k = 1"}, {src: "update n set k = 1, s = 'z' where k = 7"}},
			commit: `upsert n 1,real 1.5,"z"`, want: "n (k INT, r REAL NULL, s TEXT): 1,real 1.5,\"z\" 2,null,\"b\"\n"},
		{name: "should commit a row that was deleted and inserted again as its new values",
			steps:  []txStep{{src: "delete from n where k = 2"}, {src: "insert into n values (2, 2.5, 'q')"}},
			commit: `upsert n 2,real 2.5,"q"`, want: "n (k INT, r REAL NULL, s TEXT): 1,real 1.5,\"a\" 2,real 2.5,\"q\"\n"},
		{name: "should commit a row under a new key as the new row and the delete of the old key",
			steps:  []txStep{{src: "update n set k = 0 where k = 2"}},
			commit: `upsert n 0,null,"b" delete n 2`, want: "n (k INT, r REAL NULL, s TEXT): 0,null,\"b\" 1,real 1.5,\"a\"\n"},
		{name: "should commit a row that an update found, also when its values did not change",
			steps:  []txStep{{src: "update n set s = 'a' where k = 1"}, {src: "update n set k = 3 where k = 9"}},
			commit: `upsert n 1,real 1.5,"a"`, want: "n (k INT, r REAL NULL, s TEXT): 1,real 1.5,\"a\" 2,null,\"b\"\n"},
		{name: "should commit the deletes of every row, and nothing of a statement that failed",
			steps:  []txStep{{src: "update n set k = 1 where k = 2", want: "19/1555: UNIQUE constraint failed: n.k"}, {src: "delete from n"}},
			commit: "delete n 1 delete n 2", want: "n (k INT, r REAL NULL, s TEXT): \n"},
		{name: "should commit the net effect of many statements on rows of several keys",
			steps: []txStep{
				{src: "insert into n values (3, null, 'c')"}, {src: "update n set r = 3.5 where k >= 2"}, {src: "update n set k = 4 where k = 3"},
				{src: "delete from n where k = 1"}, {src: "insert into n values (1, null, 'd')"}, {src: "update n set k = 5 where r is null"},
			},
			commit: `upsert n 2,real 3.5,"b" upsert n 4,real 3.5,"c" upsert n 5,null,"d" delete n 1`,
			want:   "n (k INT, r REAL NULL, s TEXT): 2,real 3.5,\"b\" 4,real 3.5,\"c\" 5,null,\"d\"\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			i := pinTx(t, setup, false, c.steps, true, c.want)
			log := logChanges(t, i.store)
			if c.commit == "" {
				// A transaction that changes nothing in the end leaves no commit object.
				is.Equal(t, len(setup), len(log))
				return
			}
			is.Equal(t, len(setup)+1, len(log))
			is.Equal(t, c.commit, strings.Join(log[len(log)-1], " "))
		})
	}

	t.Run("should commit the rows of an update and of a delete of their own, and replay them", func(t *testing.T) {
		w := newChangeTrio(t, 0, setup...)
		w.run("update n set r = 0.5")
		w.run("update n set k = 3 where k = 1")
		w.run("delete from n where s = 'b'")
		w.run("delete from n where k = 9")
		// A statement that changes no row leaves no commit object.
		log := logChanges(t, w.byKey.store)
		is.Equal(t, len(setup)+3, len(log))
		is.Equal(t, `upsert n 1,real 0.5,"a" upsert n 2,real 0.5,"b"`, strings.Join(log[len(log)-3], " "))
		is.Equal(t, `upsert n 3,real 0.5,"a" delete n 1`, strings.Join(log[len(log)-2], " "))
		is.Equal(t, `delete n 2`, strings.Join(log[len(log)-1], " "))
		is.Equal(t, `[[update n set r = 0.5] [update n set k = 3 where k = 1] [delete from n where s = 'b']]`,
			fmt.Sprint(logAnnotations(t, w.byKey.store)[len(setup):]))
		state := w.o.state(t)
		w.byKey.restart(true, engine.Options{})
		is.Equal(t, state, w.byKey.state(), "after a replay of the log")
	})
}

// TestConn_differential runs random sequences of statements in transactions that span statements,
// on the engine and in SQLite, as [TestDifferential] runs them one by one: some in write
// transactions, some in read-only ones, and some on their own, each transaction ended by a commit or
// by a roll back. It compares the answers and the counts of changes after each statement, the tables
// after the end of each transaction, and the tables after a replay of the log into an empty cache.
func TestConn_differential(t *testing.T) {
	n := 60
	if slow() {
		n = 6
	}
	y := tally{}
	for s := range uint64(n) {
		txDifferential(t, 9_000_000+s, 60, y)
	}
	t.Logf("%v sequences in transactions: %v", n, y)
	// The selects with WHERE that [pair.bothWays] reads both ways, of which the run gave 438, 96 of
	// them in a read-only transaction, on 2026-11-20: a third of each. When the sequence wrote updates
	// and deletes, it gave 270 and 64, which these least numbers still hold; and 543 updates and deletes
	// with WHERE that [pair.bothWays] changes both ways, 115 of them in a read-only transaction, which
	// refuses them, and 21 commits and 8 roll backs of a write transaction whose update or delete
	// changed rows: a third of each.
	if !slow() {
		for kind, least := range map[string]int{
			"read both ways": 146, "read both ways in a read-only transaction": 32, "changed both ways": 181, "changed both ways in a read-only transaction": 38,
			"commit of a transaction whose update or delete changed rows": 7, "roll back of a transaction whose update or delete changed rows": 2,
			// The create indexes and drop indexes of slice 9 in transactions, of which the run gave 29
			// commits and 13 roll backs of a write transaction with one, 20 create indexes and 5 drop
			// indexes in a read-only transaction, which refuses them, and 91 WHERE on a table with an
			// index that [pair.everyPath] ran by its paths, on 2026-11-20: a third of each.
			"commit of a transaction with a create index or a drop index": 9, "roll back of a transaction with a create index or a drop index": 4,
			"create index attempt to write a readonly database": 6, "drop index attempt to write a readonly database": 1,
			"WHERE run by every path on a table with an index": 30,
			// The statements of slice 10 in transactions, as [reachExists] counts them, a third of what the
			// run gave on 2026-11-20, or 1: in a write transaction, each that did its work and each that
			// did nothing; in a read-only one, each that did nothing, which it allows, and each that would
			// write, which it refuses. The run gave no drop index that would write in a read-only
			// transaction, which the hand tests cover.
			"create table if not exists did its work in a write transaction": 6, "create table if not exists did nothing: a table of the name exists in a write transaction": 12,
			"drop table if exists did its work in a write transaction": 3, "drop table if exists did nothing: no table of the name exists in a write transaction": 2,
			"create index if not exists did its work in a write transaction": 4, "create index if not exists did nothing: an index of the name exists in a write transaction": 2,
			"drop index if exists did its work in a write transaction": 1, "drop index if exists did nothing: no index of the name exists in a write transaction": 3,
			"create table if not exists did nothing: a table of the name exists in a read-only transaction":  6,
			"create table if not exists attempt to write a readonly database in a read-only transaction":     5,
			"drop table if exists did nothing: no table of the name exists in a read-only transaction":       2,
			"drop table if exists attempt to write a readonly database in a read-only transaction":           1,
			"create index if not exists did nothing: an index of the name exists in a read-only transaction": 1,
			"create index if not exists attempt to write a readonly database in a read-only transaction":     2,
			"drop index if exists did nothing: no index of the name exists in a read-only transaction":       1,
		} {
			if y["reached: "+kind] < least {
				t.Errorf("the run reaches %q %v times, and at least %v were expected", kind, y["reached: "+kind], least)
			}
		}
		for _, kind := range []string{"update attempt to write a readonly database", "update with a parameter attempt to write a readonly database", "delete attempt to write a readonly database"} {
			if y[kind] == 0 {
				t.Errorf("no statement has the outcome %q", kind)
			}
		}
	}
}

// txDifferential runs n statements of the sequence of seed, in transactions, as
// [TestConn_differential] describes.
func txDifferential(t *testing.T, seed uint64, n int, y tally) {
	t.Helper()
	i := newInstance(t, engine.Options{})
	defer i.close()
	r := rand.New(rand.NewPCG(seed, 0x7e))
	q := newSequence(t, r)
	if seed%3 == 2 {
		q.Indexed()
	}
	p := newPair(t, i, y)
	p.rnd = rand.New(rand.NewPCG(seed, 0x9a7))
	defer p.other.close()
	var log []string
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("seed %v, after\n%v\n%v", seed, strings.Join(log, "\n"), fmt.Sprintf(format, args...))
	}
	// open is the kind of the transaction that is open: "", "write", or "read only", and changed
	// reports whether an update or a delete of the transaction changed rows.
	// indexed reports whether a create index or a drop index of the transaction succeeded.
	open, changed, indexed := "", false, false
	end := func() {
		t.Helper()
		commit := r.IntN(3) > 0
		log = append(log, fmt.Sprint("-- end the ", open, " transaction, with a commit: ", commit))
		for _, c := range []*engine.Conn{i.c, p.other.c} {
			if commit {
				is.NotError(t, c.Commit(t.Context()))
			} else {
				is.NotError(t, c.Rollback(t.Context()))
			}
		}
		for _, s := range oracleEnd(open == "read only", commit) {
			is.Equal(t, "", p.o.run(t, s).err, s)
		}
		if changed && y != nil {
			y["reached: "+map[bool]string{true: "commit", false: "roll back"}[commit]+" of a transaction whose update or delete changed rows"]++
		}
		if indexed && y != nil {
			y["reached: "+map[bool]string{true: "commit", false: "roll back"}[commit]+" of a transaction with a create index or a drop index"]++
		}
		open, changed, indexed = "", false, false
		want := p.o.state(t)
		if got := i.state(); got != want {
			fail("after the end of the transaction the engine holds\n%v\nand SQLite\n%v", got, want)
		}
		if got := p.other.state(); got != want {
			fail("after the end of the transaction the other engine, which reads the rows the other way, holds\n%v\nand SQLite\n%v", got, want)
		}
	}
	for range n {
		switch {
		case open == "" && r.IntN(4) == 0:
			open = "write"
			if r.IntN(3) == 0 {
				open = "read only"
				is.NotError(t, i.c.BeginReadOnly(t.Context()))
				is.NotError(t, p.other.c.BeginReadOnly(t.Context()))
			} else {
				is.NotError(t, i.c.Begin(t.Context()))
				is.NotError(t, p.other.c.Begin(t.Context()))
			}
			log = append(log, "-- begin a "+open+" transaction")
			for _, s := range oracleBegin(open == "read only") {
				is.Equal(t, "", p.o.run(t, s).err, s)
			}
		case open != "" && r.IntN(8) == 0:
			end()
		}
		c := q.Next()
		log = append(log, fmt.Sprintf("%v -- with %#v", c.Src, c.Args))
		p.readOnly, p.inTx = open == "read only", open != ""
		if failure := p.step(c); failure != "" {
			fail("%v", failure)
		}
		if s, _ := stmt.Check(c.Src); open == "write" && p.last.err == "" && i.changes > 0 && changesRows(c.Src) {
			_, isInsert := s.(*stmt.Insert)
			changed = changed || !isInsert
		}
		switch s, _ := stmt.Check(c.Src); s.(type) {
		case *stmt.CreateIndex, *stmt.DropIndex:
			indexed = indexed || open == "write" && p.last.err == ""
		}
	}
	if open != "" {
		end()
	}
	i.restart(true, engine.Options{})
	if got, want := i.state(), p.o.state(t); got != want {
		fail("after a replay of the log the engine holds\n%v\nand SQLite\n%v", got, want)
	}
}

// txErr is the kind of an error of a statement in a transaction, for the tests of transactions,
// which meet more kinds than [errText] knows.
func txErr(err error) string {
	var se *sqlerr.Error
	switch {
	case err == nil:
		return ""
	case errors.As(err, &se):
		return fmt.Sprint(se.Code, "/", se.ExtendedCode, ": ", se.Message)
	case errors.Is(err, stmt.ErrNotSupported):
		return "not supported"
	case errors.Is(err, engine.ErrRowsOpen):
		return "rows open"
	case errors.Is(err, engine.ErrTxDone):
		return "tx done"
	case errors.Is(err, engine.ErrNoTx):
		return "no tx"
	case errors.Is(err, engine.ErrTxOpen):
		return "tx open"
	case errors.Is(err, engine.ErrTooLarge):
		return "too large"
	case errors.Is(err, core.ErrOutcomeUnknown):
		return "unknown"
	case errors.Is(err, core.ErrNoLease):
		return "no lease"
	case errors.Is(err, core.ErrStopped):
		return "stopped"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "other: " + err.Error()
}

// do runs src with args on the connection c, through Query for a text with "select" in it, and
// gives its rows as "rows" and the rows, its error as [txErr] gives it, or "".
func do(t *testing.T, c *engine.Conn, src string, args ...int64) string {
	t.Helper()
	if !strings.Contains(strings.ToLower(src), "select") {
		_, err := c.Exec(t.Context(), src, integers(args)...)
		return txErr(err)
	}
	rows, err := c.Query(t.Context(), src, integers(args)...)
	if err != nil {
		return txErr(err)
	}
	var values []string
	for row, err := range rows.All() {
		if err != nil {
			return txErr(err)
		}
		values = append(values, valueText(t, row[0]))
	}
	return strings.TrimSpace("rows " + strings.Join(values, " "))
}

// conn of the engine of i, which is closed when the test ends.
func conn(t *testing.T, i *instance) *engine.Conn {
	t.Helper()
	c := i.e.Conn()
	t.Cleanup(func() { is.NotError(t, c.Close(context.WithoutCancel(t.Context()))) })
	return c
}

func TestConn_isolation(t *testing.T) {
	t.Run("should hide the writes of a transaction from every other connection until its commit, and show them to every read that starts after it", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(t1).err)
		w, ro := conn(t, i), conn(t, i)
		is.NotError(t, w.Begin(t.Context()))
		is.Equal(t, "", do(t, w, insert1))
		is.Equal(t, "rows 1", do(t, w, selectT))
		is.Equal(t, "rows", do(t, i.c, selectT))
		is.NotError(t, ro.BeginReadOnly(t.Context()))
		is.Equal(t, "rows", do(t, ro, selectT))
		is.Equal(t, "t1 (v1 INT): \n", i.state())

		is.NotError(t, w.Commit(t.Context()))
		is.Equal(t, "rows 1", do(t, i.c, selectT))
		is.Equal(t, "rows 1", do(t, w, selectT))
		// The read-only transaction began before the commit.
		is.Equal(t, "rows", do(t, ro, selectT))
		is.NotError(t, ro.Commit(t.Context()))
		is.Equal(t, "rows 1", do(t, ro, selectT))
	})

	t.Run("should keep the state of the beginning of a read-only transaction for each of its statements, while others commit rows and tables", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(t1).err)
		is.Equal(t, "", i.exec(insert1).err)
		ro, w := conn(t, i), conn(t, i)
		is.NotError(t, ro.BeginReadOnly(t.Context()))
		is.Equal(t, "rows 1", do(t, ro, selectT))

		is.Equal(t, "", i.exec(insert2).err)
		is.NotError(t, w.Begin(t.Context()))
		is.Equal(t, "", do(t, w, "insert into t1 (v1) values (3)"))
		is.Equal(t, "", do(t, w, "create table t2 (v int primary key) strict, without rowid"))
		is.NotError(t, w.Commit(t.Context()))
		is.Equal(t, "rows 1", do(t, ro, selectT))
		is.Equal(t, "", i.exec("drop table t1").err)

		is.Equal(t, "rows 1", do(t, ro, selectT))
		is.Equal(t, "1/1: no such table: t2", do(t, ro, "select * from t2"))
		is.Equal(t, "8/8: attempt to write a readonly database", do(t, ro, "insert into t1 (v1) values (4)"))
		is.Equal(t, "1/1: no such table: t2", do(t, ro, "insert into t2 (v) values (4)"))
		is.NotError(t, ro.Rollback(t.Context()))
		is.Equal(t, "1/1: no such table: t1", do(t, ro, selectT))
		is.Equal(t, "rows", do(t, ro, "select * from t2"))
	})

	t.Run("should let a read-only transaction and a write transaction begin and run at once, neither waiting for the other", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			i := newInstance(t, engine.Options{})
			is.Equal(t, "", i.exec(t1).err)
			w, ro := conn(t, i), conn(t, i)
			start := time.Now()
			is.NotError(t, w.Begin(t.Context()))
			is.NotError(t, ro.BeginReadOnly(t.Context()))
			is.Equal(t, "", do(t, w, insert1))
			is.Equal(t, "rows", do(t, ro, selectT))
			is.NotError(t, w.Commit(t.Context()))
			is.NotError(t, w.Begin(t.Context()))
			is.Equal(t, "", do(t, w, insert2))
			is.Equal(t, "rows", do(t, ro, selectT))
			is.NotError(t, w.Commit(t.Context()))
			is.NotError(t, ro.Commit(t.Context()))
			// No call waited: the clock of the bubble moves only while every goroutine waits.
			is.Equal(t, start, time.Now())
		})
	})

	for _, end := range []string{"commit", "rollback"} {
		t.Run("should begin a second write transaction only after the first has ended, by a "+end+", and let it see what the first committed", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Forward, so that the last select gives the two rows in the order of the key.
				i := newInstance(t, forward())
				is.Equal(t, "", i.exec(t1).err)
				first, second := conn(t, i), conn(t, i)
				is.NotError(t, first.Begin(t.Context()))
				is.Equal(t, "", do(t, first, insert1))

				// The channel holds the answer of the begin, so that its goroutine ends also when a check
				// before the receive fails the test, and the bubble does not end in a deadlock that hides
				// the failure.
				began := make(chan error, 1)
				go func() {
					began <- second.Begin(t.Context())
				}()
				time.Sleep(time.Hour)
				select {
				case err := <-began:
					t.Fatalf("a second write transaction began while the first was open, with %v", err)
				default:
				}

				want := "rows"
				if end == "commit" {
					is.NotError(t, first.Commit(t.Context()))
					want = "rows 1"
				} else {
					is.NotError(t, first.Rollback(t.Context()))
				}
				is.NotError(t, <-began)
				is.Equal(t, want, do(t, second, selectT))
				is.Equal(t, "", do(t, second, insert2))
				is.NotError(t, second.Commit(t.Context()))
				is.Equal(t, strings.TrimPrefix(want, "rows")+" 2", strings.TrimPrefix(do(t, i.c, selectT), "rows"))
			})
		})
	}

	t.Run("should give the error of a context that ends while a begin waits, and begin nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			i := newInstance(t, engine.Options{})
			is.Equal(t, "", i.exec(t1).err)
			first, second := conn(t, i), conn(t, i)
			is.NotError(t, first.Begin(t.Context()))

			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			err := second.Begin(ctx)
			is.Error(t, context.DeadlineExceeded, err)
			is.Error(t, engine.ErrNoTx, second.Commit(t.Context()))
			is.Error(t, engine.ErrNoTx, second.Rollback(t.Context()))

			is.NotError(t, first.Rollback(t.Context()))
			is.NotError(t, second.Begin(t.Context()))
			is.Equal(t, "", do(t, second, insert1))
			is.NotError(t, second.Commit(t.Context()))
			is.Equal(t, "rows 1", do(t, first, selectT))
		})
	})
}

func TestConn_Commit(t *testing.T) {
	t.Run("should write one commit object for a transaction, annotated in order with each statement that succeeded and wrote, with its arguments", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(t1).err)
		is.NotError(t, i.c.Begin(t.Context()))
		for _, s := range []txStep{
			{src: insert1},
			{src: insert1, want: "19/1555: UNIQUE constraint failed: t1.v1"},
			{src: selectT, want: "rows 1"},
			{src: " /* before */ " + insertP + "; -- after", args: []any{5}},
			{src: insertP, args: []any{5}, want: "19/1555: UNIQUE constraint failed: t1.v1"},
			{src: "insert into t2 (v1) values (1)", want: "1/1: no such table: t2"},
			{src: "create table t2 (v int primary key) strict, without rowid"},
			{src: "selec", want: `1/1: near "selec": syntax error`},
			{src: "drop table t2"},
			{src: "create table t3 (v int primary key) strict, without rowid"},
		} {
			got, _ := runIn(t, i, s)
			is.Equal(t, s.want, got, s.src)
		}
		is.NotError(t, i.c.Commit(t.Context()))
		is.Equal(t, fmt.Sprint([][]string{
			{t1},
			{insert1, insertP + "; with 5", "create table t2 (v int primary key) strict, without rowid", "drop table t2", "create table t3 (v int primary key) strict, without rowid"},
		}), fmt.Sprint(logAnnotations(t, i.store)))
		want := "t1 (v1 INT): 1 5\nt3 (v INT): \n"
		is.Equal(t, want, i.state())
		i.restart(true, engine.Options{})
		is.Equal(t, want, i.state())
	})

	t.Run("should write no commit object for a transaction that changes nothing, and commit it", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(t1).err)
		is.Equal(t, "", i.exec(insert1).err)
		for _, steps := range [][]string{
			{selectT, insert1, "drop table t2", "selec"},
			{"create table t2 (v int primary key) strict, without rowid", "insert into t2 (v) values (1)", "drop table t2"},
		} {
			is.NotError(t, i.c.Begin(t.Context()))
			for _, src := range steps {
				_ = do(t, i.c, src)
			}
			is.NotError(t, i.c.Commit(t.Context()))
		}
		is.NotError(t, i.c.BeginReadOnly(t.Context()))
		is.Equal(t, "8/8: attempt to write a readonly database", do(t, i.c, insert2))
		is.NotError(t, i.c.Commit(t.Context()))
		// And a transaction with changes that is rolled back.
		is.NotError(t, i.c.Begin(t.Context()))
		is.Equal(t, "", do(t, i.c, insert2))
		is.NotError(t, i.c.Rollback(t.Context()))
		is.Equal(t, 2, len(logAnnotations(t, i.store)))
		is.Equal(t, "t1 (v1 INT): 1\n", i.state())
	})

	t.Run("should fail at once with ErrTooLarge for a commit object that the store cannot take, commit nothing, and go on", func(t *testing.T) {
		// The limit is far above a manifest and a commit object of a few statements, and below that of
		// a hundred inserts.
		i := newInstanceOn(t, simstore.Options{MaxSize: 4 << 10})
		is.Equal(t, "", i.exec(t1).err)
		is.NotError(t, i.c.Begin(t.Context()))
		for v := range 100 {
			is.Equal(t, "", do(t, i.c, insertP, int64(v)))
		}
		err := i.c.Commit(t.Context())
		is.Error(t, engine.ErrTooLarge, err)
		is.Error(t, core.ErrTooLarge, err)
		is.True(t, strings.HasPrefix(err.Error(), "s4ql: the transaction is too large to commit: "), err.Error())
		is.Error(t, engine.ErrNoTx, i.c.Rollback(t.Context()))
		is.Equal(t, "t1 (v1 INT): \n", i.state())
		is.Equal(t, 1, len(logAnnotations(t, i.store)))

		is.NotError(t, i.c.Begin(t.Context()))
		is.Equal(t, "", do(t, i.c, insert1))
		is.NotError(t, i.c.Commit(t.Context()))
		is.Equal(t, "t1 (v1 INT): 1\n", i.state())
	})
}

// newInstanceOn is [newInstance] on a store with the given options.
func newInstanceOn(t *testing.T, options simstore.Options) *instance {
	t.Helper()
	h := sim.NewFree(sim.Buggify{})
	i := &instance{t: t, host: h, store: simstore.New(h, options), dir: t.TempDir()}
	i.open(true, engine.Options{})
	t.Cleanup(func() {
		i.close()
		is.NotError(t, i.c.Close(context.WithoutCancel(t.Context())))
	})
	return i
}

func TestConn_rows(t *testing.T) {
	kinds := []struct {
		name  string
		begin func(c *engine.Conn, ctx context.Context) error
		// write is the answer of an insert of a new key in the transaction.
		write string
	}{
		{name: "write", begin: (*engine.Conn).Begin},
		{name: "read-only", begin: (*engine.Conn).BeginReadOnly, write: "8/8: attempt to write a readonly database"},
	}
	for _, k := range kinds {
		t.Run("should refuse every statement of a "+k.name+" transaction while the rows of its earlier query are open, and go on after they end", func(t *testing.T) {
			i := newInstance(t, engine.Options{})
			is.Equal(t, "", i.exec(t1).err)
			is.Equal(t, "", i.exec(insert1).err)
			is.NotError(t, k.begin(i.c, t.Context()))
			rows, err := i.c.Query(t.Context(), selectT)
			is.NotError(t, err)
			is.Equal(t, "rows open", do(t, i.c, insert2))
			is.Equal(t, "rows open", do(t, i.c, selectT))
			is.Equal(t, "rows open", do(t, i.c, "drop table t1"))
			// Its errors of text and arguments come first.
			is.Equal(t, `1/1: near "selec": syntax error`, do(t, i.c, "selec"))
			is.NotError(t, rows.Close())
			is.Equal(t, k.write, do(t, i.c, insert2))
			is.Equal(t, "rows", strings.Fields(do(t, i.c, selectT))[0])
			is.NotError(t, i.c.Rollback(t.Context()))
			is.Equal(t, "rows 1", do(t, i.c, selectT))
		})

		t.Run("should close the rows of a "+k.name+" transaction that no loop reads at its commit and at its roll back, which then give nothing", func(t *testing.T) {
			for _, end := range []func(c *engine.Conn, ctx context.Context) error{(*engine.Conn).Commit, (*engine.Conn).Rollback} {
				i := newInstance(t, engine.Options{})
				is.Equal(t, "", i.exec(t1).err)
				is.Equal(t, "", i.exec(insert1).err)
				is.NotError(t, k.begin(i.c, t.Context()))
				rows, err := i.c.Query(t.Context(), selectT)
				is.NotError(t, err)
				is.NotError(t, end(i.c, t.Context()))
				for _, err := range rows.All() {
					t.Fatalf("the rows gave a row after the end of their transaction, with %v", err)
				}
				is.NotError(t, rows.Close())
				is.Equal(t, "rows 1", do(t, i.c, selectT))
				noneOpen(t, i)
			}
		})

		t.Run("should end a loop over the rows of a "+k.name+" transaction at its next row with ErrTxDone when the transaction rolls back", func(t *testing.T) {
			// Forward, so that the first row of the loop is that of the key 1.
			i := newInstance(t, forward())
			is.Equal(t, "", i.exec(t1).err)
			is.Equal(t, "", i.exec(insert1).err)
			is.Equal(t, "", i.exec(insert2).err)
			is.NotError(t, k.begin(i.c, t.Context()))
			rows, err := i.c.Query(t.Context(), selectT)
			is.NotError(t, err)
			var got []string
			for row, err := range rows.All() {
				if err != nil {
					got = append(got, txErr(err))
					continue
				}
				got = append(got, valueText(t, row[0]))
				is.NotError(t, i.c.Rollback(t.Context()))
				// The transaction is over, and its end waits for the loop: the connection runs
				// nothing meanwhile.
				is.Equal(t, "rows open", do(t, i.c, selectT))
				is.Equal(t, "rows open", do(t, i.c, insert1))
				is.Error(t, core.ErrReaderInUse, i.c.Close(t.Context()))
			}
			is.Equal(t, "1 tx done", strings.Join(got, " "))
			is.NotError(t, rows.Close())
			is.Equal(t, "rows 1 2", do(t, i.c, selectT))
			noneOpen(t, i)
		})
	}

	t.Run("should roll a write transaction back and fail its commit while a loop over its rows runs, and free the place of the writer when the loop ends", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// Forward, so that the first row of the loop is that of the key 1.
			i := newInstance(t, forward())
			is.Equal(t, "", i.exec(t1).err)
			is.NotError(t, i.c.Begin(t.Context()))
			is.Equal(t, "", do(t, i.c, insert1))
			is.Equal(t, "", do(t, i.c, insert2))
			other := conn(t, i)
			// The channel holds the answer of the begin, so that its goroutine ends also when a check
			// before the receive fails the test, and the bubble does not end in a deadlock that hides
			// the failure.
			began := make(chan error, 1)
			rows, err := i.c.Query(t.Context(), selectT)
			is.NotError(t, err)
			var got []string
			for row, err := range rows.All() {
				if err != nil {
					got = append(got, txErr(err))
					continue
				}
				got = append(got, valueText(t, row[0]))
				commitErr := i.c.Commit(t.Context())
				is.Error(t, engine.ErrRowsOpen, commitErr)
				is.True(t, strings.HasSuffix(commitErr.Error(), "so the transaction is rolled back and not committed"), commitErr.Error())
				go func() {
					began <- other.Begin(t.Context())
				}()
				time.Sleep(time.Hour)
				select {
				case err := <-began:
					t.Fatalf("another write transaction began while the loop held the transaction that was rolled back, with %v", err)
				default:
				}
			}
			is.Equal(t, "1 tx done", strings.Join(got, " "))
			is.NotError(t, <-began)
			is.Equal(t, "rows", do(t, other, selectT))
			is.NotError(t, other.Rollback(t.Context()))
			is.Equal(t, "t1 (v1 INT): \n", i.state())
		})
	})

	t.Run("should end a read-only transaction at a commit while a loop over its rows runs, which ends at its next row", func(t *testing.T) {
		// Forward, so that the first row of the loop is that of the key 1.
		i := newInstance(t, forward())
		is.Equal(t, "", i.exec(t1).err)
		is.Equal(t, "", i.exec(insert1).err)
		is.Equal(t, "", i.exec(insert2).err)
		is.NotError(t, i.c.BeginReadOnly(t.Context()))
		rows, err := i.c.Query(t.Context(), selectT)
		is.NotError(t, err)
		var got []string
		for row, err := range rows.All() {
			if err != nil {
				got = append(got, txErr(err))
				continue
			}
			got = append(got, valueText(t, row[0]))
			is.NotError(t, i.c.Commit(t.Context()))
		}
		is.Equal(t, "1 tx done", strings.Join(got, " "))
		is.Equal(t, "rows 1 2", do(t, i.c, selectT))
	})

	t.Run("should refuse a begin while the rows of a query of the connection are open", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(t1).err)
		rows, err := i.c.Query(t.Context(), selectT)
		is.NotError(t, err)
		is.Error(t, engine.ErrRowsOpen, i.c.Begin(t.Context()))
		is.Error(t, engine.ErrRowsOpen, i.c.BeginReadOnly(t.Context()))
		// A write of its own runs, as before.
		is.Equal(t, "", do(t, i.c, insert1))
		is.NotError(t, rows.Close())
		is.NotError(t, i.c.Begin(t.Context()))
		is.NotError(t, i.c.Rollback(t.Context()))
	})

	t.Run("should refuse a begin while a transaction is open, an end without one, and a begin on a closed connection", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		c := i.e.Conn()
		is.Error(t, engine.ErrNoTx, c.Commit(t.Context()))
		is.Error(t, engine.ErrNoTx, c.Rollback(t.Context()))
		is.NotError(t, c.Begin(t.Context()))
		is.Error(t, engine.ErrTxOpen, c.Begin(t.Context()))
		is.Error(t, engine.ErrTxOpen, c.BeginReadOnly(t.Context()))
		is.NotError(t, c.Commit(t.Context()))
		is.NotError(t, c.BeginReadOnly(t.Context()))
		is.Error(t, engine.ErrTxOpen, c.Begin(t.Context()))
		// The close of a connection rolls its transaction back.
		is.NotError(t, c.Close(t.Context()))
		for _, err := range []error{c.Begin(t.Context()), c.BeginReadOnly(t.Context())} {
			is.Error(t, engine.ErrInternal, err)
			is.True(t, strings.Contains(err.Error(), "a transaction on a connection that is closed"), err.Error())
		}
		noneOpen(t, i)
	})

	t.Run("should roll back the transaction of a connection that is closed, and free the place of the writer", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(t1).err)
		c := i.e.Conn()
		is.NotError(t, c.Begin(t.Context()))
		is.Equal(t, "", do(t, c, insert1))
		is.NotError(t, c.Close(t.Context()))
		noneOpen(t, i)
		is.Equal(t, "t1 (v1 INT): \n", i.state())
	})
}

func TestConn_stop(t *testing.T) {
	for _, end := range []string{"commit", "rollback"} {
		t.Run("should fail the statements of an open write transaction of an instance that has stopped, and its "+end, func(t *testing.T) {
			i := newInstance(t, engine.Options{})
			is.Equal(t, "", i.exec(t1).err)
			is.NotError(t, i.c.Begin(t.Context()))
			is.Equal(t, "", do(t, i.c, insert1))
			i.close()
			is.Equal(t, "stopped", do(t, i.c, insert2))
			is.Equal(t, "stopped", do(t, i.c, selectT))
			// The errors of text and arguments come first.
			is.Equal(t, `1/1: near "selec": syntax error`, do(t, i.c, "selec"))
			if end == "commit" {
				is.Equal(t, "stopped", txErr(i.c.Commit(t.Context())))
			} else {
				is.NotError(t, i.c.Rollback(t.Context()))
			}
			is.Error(t, engine.ErrNoTx, i.c.Rollback(t.Context()))
			i.open(false, engine.Options{})
			is.Equal(t, "t1 (v1 INT): \n", i.state())
		})
	}

	t.Run("should fail the statements of an open read-only transaction of an instance that has stopped, and end it", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec(t1).err)
		is.NotError(t, i.c.BeginReadOnly(t.Context()))
		is.Equal(t, "rows", do(t, i.c, selectT))
		i.close()
		is.Equal(t, "stopped", do(t, i.c, selectT))
		is.Equal(t, "stopped", do(t, i.c, insert1))
		is.NotError(t, i.c.Commit(t.Context()))
		is.Error(t, engine.ErrNoTx, i.c.Rollback(t.Context()))
	})

	for _, end := range []string{"commit", "rollback"} {
		t.Run("should fail the statements of an open transaction whose instance lost the lease, and its "+end+", and free the place of the writer", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := sim.NewFree(sim.Buggify{})
				s := simstore.New(h, simstore.Options{})
				dir := t.TempDir()
				open := func(name string, client *simstore.Client) (*core.DB, *engine.Conn) {
					d, err := core.Open(t.Context(), h, client, filepath.Join(dir, name+".db"), "db", core.Options{})
					is.NotError(t, err)
					c := engine.New(d, h, engine.Options{}).Conn()
					t.Cleanup(func() {
						ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
						defer cancel()
						_ = d.Close(ctx)
						is.NotError(t, c.Close(ctx))
					})
					is.NotError(t, d.Wait(t.Context()))
					return d, c
				}
				ca := s.Client("a", nil)
				a, c := open("a", ca)
				is.Equal(t, "", do(t, c, t1))
				is.NotError(t, c.Begin(t.Context()))
				is.Equal(t, "", do(t, c, insert1))

				// The instance a cannot reach the store, and b takes the lease and commits.
				ca.CutOff(t.Context(), 5*time.Minute)
				b, cb := open("b", s.Client("b", nil))
				is.Equal(t, "", do(t, cb, insert2))
				time.Sleep(10 * time.Minute)

				is.Equal(t, "no lease", do(t, c, "insert into t1 (v1) values (3)"))
				is.Equal(t, "no lease", do(t, c, selectT))
				is.Equal(t, `1/1: near "selec": syntax error`, do(t, c, "selec"))
				if end == "commit" {
					is.Equal(t, "no lease", txErr(c.Commit(t.Context())))
				} else {
					is.NotError(t, c.Rollback(t.Context()))
				}
				is.Error(t, engine.ErrNoTx, c.Rollback(t.Context()))
				is.Equal(t, "no lease", txErr(c.Begin(t.Context())))

				// Once a has the lease again, the place of the writer is free.
				is.NotError(t, b.Close(t.Context()))
				is.NotError(t, a.Wait(t.Context()))
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				is.NotError(t, c.Begin(ctx))
				is.Equal(t, "rows 2", do(t, c, selectT))
				is.NotError(t, c.Rollback(t.Context()))
			})
		})
	}
}

// leaseLost runs f in a bubble of synctest with two instances of one database, a and b, each with a
// connection of an engine, whose queries read forward, so that f writes the rows that it expects in
// the order of the key. The table t1 exists, and the engine of a logs to the events h.
func leaseLost(t *testing.T, f func(t *testing.T, ca *simstore.Client, a, b func() (*core.DB, *engine.Conn), h *events)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		h := sim.NewFree(sim.Buggify{})
		s := simstore.New(h, simstore.Options{})
		dir := t.TempDir()
		logged := &events{}
		open := func(name string, client *simstore.Client, log *events) func() (*core.DB, *engine.Conn) {
			return func() (*core.DB, *engine.Conn) {
				d, err := core.Open(t.Context(), h, client, filepath.Join(dir, name+".db"), "db", core.Options{})
				is.NotError(t, err)
				options := forward()
				if log != nil {
					options.Logger = slog.New(log)
				}
				c := engine.New(d, h, options).Conn()
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
					defer cancel()
					_ = d.Close(ctx)
					_ = c.Close(ctx)
				})
				is.NotError(t, d.Wait(t.Context()))
				return d, c
			}
		}
		ca := s.Client("a", nil)
		f(t, ca, open("a", ca, logged), open("b", s.Client("b", nil), nil), logged)
	})
}

// isLeaseLost checks that err is the error of a write transaction that the instance ended because it
// lost its lease.
func isLeaseLost(t *testing.T, err error) {
	t.Helper()
	is.Error(t, core.ErrTxLeaseLost, err)
	is.Equal(t, "no lease", txErr(err))
}

func TestConn_leaseLost(t *testing.T) {
	for _, end := range []string{"commit", "rollback"} {
		t.Run("should serve again without the application when its open write transaction is idle as it loses the lease, fail the statements and the commit of that transaction, return nil from its "+end+", and be usable after it", func(t *testing.T) {
			leaseLost(t, func(t *testing.T, ca *simstore.Client, openA, openB func() (*core.DB, *engine.Conn), h *events) {
				a, c := openA()
				is.Equal(t, "", do(t, c, t1))
				is.NotError(t, c.Begin(t.Context()))
				is.Equal(t, "", do(t, c, insert1))

				// The instance a cannot reach the store, and b takes the lease, commits, and is closed.
				ca.CutOff(t.Context(), 5*time.Minute)
				b, cb := openB()
				is.Equal(t, "", do(t, cb, insert2))
				time.Sleep(10 * time.Minute)
				is.NotError(t, b.Close(t.Context()))

				// a takes the lease back, though the transaction is still open on its connection.
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				is.NotError(t, a.Wait(ctx))

				_, err := c.Exec(t.Context(), "insert into t1 (v1) values (3)")
				isLeaseLost(t, err)
				_, err = c.Query(t.Context(), selectT)
				isLeaseLost(t, err)
				is.Equal(t, `1/1: near "selec": syntax error`, do(t, c, "selec"))
				if end == "commit" {
					err := c.Commit(t.Context())
					isLeaseLost(t, err)
					is.True(t, !errors.Is(err, core.ErrOutcomeUnknown))
				} else {
					is.NotError(t, c.Rollback(t.Context()))
				}
				e, level := h.last(t)
				is.Equal(t, slog.LevelDebug, level)
				is.Equal(t, engine.EventTransaction, e["msg"])
				is.Equal(t, end, e[engine.AttrTxEnd])
				is.Equal(t, "lease_lost", e[engine.AttrOutcome])
				is.True(t, strings.Contains(e[engine.AttrError], "the transaction was ended because the instance lost its lease"), e[engine.AttrError])
				is.Error(t, engine.ErrNoTx, c.Rollback(t.Context()))

				// The connection works, and nothing of the transaction that was ended is in the database.
				is.Equal(t, "rows 2", do(t, c, selectT))
				is.NotError(t, c.Begin(t.Context()))
				is.Equal(t, "", do(t, c, "insert into t1 (v1) values (4)"))
				is.NotError(t, c.Commit(t.Context()))
				is.Equal(t, "rows 2 4", do(t, c, selectT))
			})
		})
	}

	t.Run("should fail the statements of an open write transaction while its lease has run out by the clocks, keep the transaction, and commit it after the renewal", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := sim.NewFree(sim.Buggify{})
			s := simstore.New(h, simstore.Options{})
			ca := s.Client("a", nil)
			start := time.Now()
			d, err := core.Open(t.Context(), h, ca, filepath.Join(t.TempDir(), "a.db"), "db", core.Options{})
			is.NotError(t, err)
			// Forward, so that the last select gives the two rows in the order of the key.
			c := engine.New(d, h, forward()).Conn()
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
				defer cancel()
				_ = d.Close(ctx)
				is.NotError(t, c.Close(ctx))
			})
			is.NotError(t, d.Wait(t.Context()))
			is.Equal(t, "", do(t, c, t1))
			is.NotError(t, c.Begin(t.Context()))
			is.Equal(t, "", do(t, c, insert1))

			// The instance cannot reach the store for 45 seconds, and no other instance takes the lease,
			// which lasts 30 seconds.
			ca.CutOff(t.Context(), 45*time.Second)
			time.Sleep(time.Until(start.Add(35 * time.Second)))

			_, err = c.Exec(t.Context(), "insert into t1 (v1) values (3)")
			is.Error(t, core.ErrNoLease, err)
			is.True(t, !errors.Is(err, core.ErrTxLeaseLost), fmt.Sprint(err))
			is.True(t, strings.Contains(fmt.Sprint(err), "lease_expired"), fmt.Sprint(err))
			is.Equal(t, "no lease", do(t, c, selectT))
			is.Equal(t, "no lease", do(t, c, "insert into t1 (v1) values (4)"))

			// The store comes back, and the instance renews the lease.
			time.Sleep(time.Until(start.Add(45 * time.Second)))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			is.NotError(t, d.Wait(ctx))

			// The transaction is still open, with what it did before, and nothing of the statements
			// that failed.
			is.Equal(t, "rows 1", do(t, c, selectT))
			is.Equal(t, "", do(t, c, insert2))
			is.NotError(t, c.Commit(t.Context()))
			is.Equal(t, "rows 1 2", do(t, c, selectT))
			is.Equal(t, fmt.Sprint([][]string{
				{t1},
				{insert1, insert2},
			}), fmt.Sprint(logAnnotations(t, s)))
		})
	})

	t.Run("should end the rows of a query of the transaction that a loop reads when the lease is lost at their next row, and serve again only then", func(t *testing.T) {
		leaseLost(t, func(t *testing.T, ca *simstore.Client, openA, openB func() (*core.DB, *engine.Conn), h *events) {
			a, c := openA()
			is.Equal(t, "", do(t, c, t1))
			is.Equal(t, "", do(t, c, insert1))
			is.Equal(t, "", do(t, c, "insert into t1 (v1) values (3)"))
			is.NotError(t, c.Begin(t.Context()))
			rows, err := c.Query(t.Context(), selectT)
			is.NotError(t, err)
			next, stop := iter.Pull2(rows.All())
			defer stop()
			row, err, ok := next()
			is.True(t, ok)
			is.NotError(t, err)
			is.Equal(t, int64(1), row[0].GetInteger())

			ca.CutOff(t.Context(), 5*time.Minute)
			b, cb := openB()
			is.Equal(t, "", do(t, cb, insert2))
			time.Sleep(10 * time.Minute)
			is.NotError(t, b.Close(t.Context()))
			// The scan holds a statement of SQLite on the connection of the transaction, so the instance
			// waits for the loop.
			time.Sleep(time.Hour)
			waitCtx, cancel := context.WithCancel(t.Context())
			cancel()
			is.Error(t, context.Canceled, a.Wait(waitCtx))

			_, err, ok = next()
			is.True(t, ok)
			isLeaseLost(t, err)
			_, _, ok = next()
			is.True(t, !ok)
			is.NotError(t, rows.Close())
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			is.NotError(t, a.Wait(ctx))
			is.NotError(t, c.Rollback(t.Context()))
			is.Equal(t, "rows 1 2 3", do(t, c, selectT))
		})
	})
}

func TestConn_events(t *testing.T) {
	t.Run("should log one event for each transaction at its end, with its kind, its end, its outcome, and its number of statements, at Debug", func(t *testing.T) {
		h, ch := &events{}, &events{}
		i := newInstanceLogged(t, engine.Options{Logger: slog.New(h)}, slog.New(ch))
		is.Equal(t, "", i.exec(t1).err)

		is.NotError(t, i.c.Begin(t.Context()))
		is.Equal(t, "", do(t, i.c, insertP, 4815162342))
		e, _ := h.last(t)
		is.Equal(t, engine.EventStatement, e["msg"])
		is.Equal(t, "write", e[engine.AttrTxKind])
		is.Equal(t, "19/1555: UNIQUE constraint failed: t1.v1", do(t, i.c, insertP, 4815162342))
		is.Equal(t, "rows 4815162342", do(t, i.c, selectT))
		e, _ = h.last(t)
		is.Equal(t, e[engine.AttrPosition], e[engine.AttrParent])
		is.NotError(t, i.c.Commit(t.Context()))
		e, level := h.last(t)
		is.Equal(t, slog.LevelDebug, level)
		is.Equal(t, engine.EventTransaction, e["msg"])
		is.Equal(t, "write", e[engine.AttrTxKind])
		is.Equal(t, "commit", e[engine.AttrTxEnd])
		is.Equal(t, "ok", e[engine.AttrOutcome])
		is.Equal(t, "3", e[engine.AttrTxStatements])
		is.Equal(t, "1", e[engine.AttrTxWrites])
		is.Equal(t, e[engine.AttrTxPosition], e[engine.AttrParent])
		_, ok := e[engine.AttrDuration]
		is.True(t, ok)
		_, ok = e[engine.AttrSQL]
		is.True(t, !ok, "the event of a transaction has no text")
		// The event of the core's commit has the same parent.
		commit, _ := ch.last(t)
		is.Equal(t, "commit", commit["msg"])
		is.Equal(t, e[engine.AttrParent], commit[engine.AttrParent])

		is.NotError(t, i.c.BeginReadOnly(t.Context()))
		is.Equal(t, "rows 4815162342", do(t, i.c, selectT))
		is.NotError(t, i.c.Rollback(t.Context()))
		e, level = h.last(t)
		is.Equal(t, slog.LevelDebug, level)
		is.Equal(t, "read_only", e[engine.AttrTxKind])
		is.Equal(t, "rollback", e[engine.AttrTxEnd])
		is.Equal(t, "ok", e[engine.AttrOutcome])
		is.Equal(t, "1", e[engine.AttrTxStatements])
		is.Equal(t, "0", e[engine.AttrTxWrites])
		_, ok = e[engine.AttrParent]
		is.True(t, !ok, "a read-only transaction has no parent")

		is.NotError(t, i.c.Begin(t.Context()))
		is.Equal(t, "rows 4815162342", do(t, i.c, selectT))
		i.close()
		is.Equal(t, "stopped", txErr(i.c.Commit(t.Context())))
		e, level = h.last(t)
		is.Equal(t, slog.LevelDebug, level)
		is.Equal(t, "refused", e[engine.AttrOutcome])
		is.True(t, e[engine.AttrError] != "", "the event of a commit that failed has its error")

		// No value of an argument is in any event, of the engine or of the core.
		h.mu.Lock()
		all := fmt.Sprint(h.records)
		h.mu.Unlock()
		ch.mu.Lock()
		all += fmt.Sprint(ch.records)
		ch.mu.Unlock()
		is.True(t, !strings.Contains(all, "4815162342"), all)
	})

	t.Run("should log the event of a transaction whose end waits for a loop over its rows when the loop ends, with the error of its commit", func(t *testing.T) {
		h := &events{}
		i := newInstanceLogged(t, engine.Options{Logger: slog.New(h)}, nil)
		is.Equal(t, "", i.exec(t1).err)
		is.Equal(t, "", i.exec(insert1).err)
		is.NotError(t, i.c.Begin(t.Context()))
		rows, err := i.c.Query(t.Context(), selectT)
		is.NotError(t, err)
		for _, err := range rows.All() {
			if err != nil {
				break
			}
			is.Error(t, engine.ErrRowsOpen, i.c.Commit(t.Context()))
			e, _ := h.last(t)
			is.Equal(t, engine.EventStatement, e["msg"])
		}
		// The end of the rows ends the transaction, and then logs the event of the query.
		h.mu.Lock()
		e, level := h.records[len(h.records)-2], h.levels[len(h.levels)-2]
		h.mu.Unlock()
		is.Equal(t, slog.LevelDebug, level)
		is.Equal(t, engine.EventTransaction, e["msg"])
		is.Equal(t, "commit", e[engine.AttrTxEnd])
		is.Equal(t, "not_supported", e[engine.AttrOutcome])
		is.True(t, strings.HasSuffix(e[engine.AttrError], "so the transaction is rolled back and not committed"), e[engine.AttrError])
	})

	t.Run("should log a commit that is too large at Debug", func(t *testing.T) {
		h := &events{}
		hi := newInstanceOn(t, simstore.Options{MaxSize: 4 << 10})
		hi.e = engine.New(hi.d, hi.host, engine.Options{Logger: slog.New(h)})
		hi.c = hi.e.Conn()
		is.Equal(t, "", hi.exec(t1).err)
		is.NotError(t, hi.c.Begin(t.Context()))
		for v := range 100 {
			is.Equal(t, "", do(t, hi.c, insertP, int64(v)))
		}
		is.Error(t, engine.ErrTooLarge, hi.c.Commit(t.Context()))
		e, level := h.last(t)
		is.Equal(t, slog.LevelDebug, level)
		is.Equal(t, "too_large", e[engine.AttrOutcome])
		is.Equal(t, "100", e[engine.AttrTxWrites])
	})

	t.Run("should log each statement of a transaction, with its rows, the parent of a write transaction, and the refusals before and at the start of a query", func(t *testing.T) {
		h := &events{}
		i := newInstanceLogged(t, engine.Options{Logger: slog.New(h)}, nil)
		is.Equal(t, "", i.exec(t1).err)

		is.NotError(t, i.c.Begin(t.Context()))
		is.Equal(t, "", do(t, i.c, insert1))
		e, _ := h.last(t)
		is.Equal(t, engine.EventStatement, e["msg"])
		is.Equal(t, "1", e[engine.AttrRows])
		is.True(t, e[engine.AttrPosition] != "", "the insert has its position")
		is.Equal(t, e[engine.AttrPosition], e[engine.AttrParent])

		is.Equal(t, "1/1: no such table: t2", do(t, i.c, "select * from t2"))
		e, _ = h.last(t)
		is.Equal(t, engine.EventStatement, e["msg"])
		is.Equal(t, "1", e[engine.AttrCode])

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := i.c.Query(ctx, selectT)
		is.Error(t, context.Canceled, err)
		e, _ = h.last(t)
		is.Equal(t, engine.EventStatement, e["msg"])
		is.True(t, strings.Contains(e[engine.AttrError], "context canceled"), e[engine.AttrError])
		// A create checks no context of its own, so the refusal is the transaction's.
		_, err = i.c.Exec(ctx, "create table t2 (v int primary key) strict, without rowid")
		is.Error(t, context.Canceled, err)
		is.NotError(t, i.c.Commit(t.Context()))
		is.Equal(t, "1/1: no such table: t2", i.query("select * from t2").err)

		is.NotError(t, i.c.BeginReadOnly(t.Context()))
		is.Equal(t, "8/8: attempt to write a readonly database", do(t, i.c, insert2))
		e, _ = h.last(t)
		is.Equal(t, engine.EventStatement, e["msg"])
		is.True(t, e[engine.AttrPosition] != "", "the insert has its position")
		_, ok := e[engine.AttrParent]
		is.True(t, !ok, "a statement of a read-only transaction has no parent")
		is.NotError(t, i.c.Rollback(t.Context()))
	})

	t.Run("should log the duration of a transaction from its begin", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := &events{}
			i := newInstanceLogged(t, engine.Options{Logger: slog.New(h)}, nil)
			// The begin is an hour after the start, so that a duration from the start of the clock
			// would show.
			time.Sleep(time.Hour)
			is.NotError(t, i.c.BeginReadOnly(t.Context()))
			time.Sleep(time.Minute)
			is.NotError(t, i.c.Commit(t.Context()))
			e, _ := h.last(t)
			is.Equal(t, engine.EventTransaction, e["msg"])
			is.Equal(t, "1m0s", e[engine.AttrDuration])
		})
	})

	t.Run("should log the event of a read-only transaction whose commit comes inside a loop over its rows once, when the loop ends, and the cut of the loop in the event of its query", func(t *testing.T) {
		h := &events{}
		i := newInstanceLogged(t, engine.Options{Logger: slog.New(h)}, nil)
		is.Equal(t, "", i.exec(t1).err)
		is.Equal(t, "", i.exec(insert1).err)
		is.Equal(t, "", i.exec(insert2).err)
		is.NotError(t, i.c.BeginReadOnly(t.Context()))
		rows, err := i.c.Query(t.Context(), selectT)
		is.NotError(t, err)
		var cut error
		for _, err := range rows.All() {
			if err != nil {
				cut = err
				break
			}
			is.NotError(t, i.c.Commit(t.Context()))
			// No statement runs on the connection while the rows hold the end of the transaction, and
			// the refusal has its event.
			_, err := i.c.Query(t.Context(), selectT)
			is.Error(t, engine.ErrRowsOpen, err)
			e, _ := h.last(t)
			is.Equal(t, engine.EventStatement, e["msg"])
			is.Equal(t, "not_supported", e[engine.AttrOutcome])
		}
		is.Error(t, engine.ErrTxDone, cut)
		h.mu.Lock()
		var ends int
		for _, r := range h.records {
			if r["msg"] == engine.EventTransaction {
				ends++
			}
		}
		last := h.records[len(h.records)-1]
		h.mu.Unlock()
		is.Equal(t, 1, ends)
		is.Equal(t, engine.EventStatement, last["msg"])
		is.Equal(t, "tx_state", last[engine.AttrOutcome])
	})
}
