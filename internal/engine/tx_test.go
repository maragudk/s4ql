package engine_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// txStep is a statement of a transaction in a pinned test, with its arguments and the answer that
// it must give: an error as "code/extended: message", "rows" and the rows for a query, "" for a
// statement that changes the database, or one of the engine's own answers, which SQLite does not
// run: "not supported", "no statement", or "wrong arguments".
type txStep struct {
	src  string
	args []int64
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
// writes it, and keeps SQLite's count of changes as its result says.
func runIn(t *testing.T, i *instance, s txStep) string {
	t.Helper()
	if s.want == "wrong arguments" {
		_, err := i.c.Exec(t.Context(), s.src, integers(s.args)...)
		is.True(t, errors.Is(err, engine.ErrArguments), fmt.Sprint(err))
		return s.want
	}
	if strings.Contains(strings.ToLower(s.src), "select") {
		a := i.query(s.src, s.args...)
		if a.err != "" {
			return a.err
		}
		return strings.TrimSpace("rows " + strings.Join(a.rows, " "))
	}
	return i.exec(s.src, s.args...).err
}

// pinTx runs the statements of setup, each a transaction of its own, and then the steps in one
// transaction, read-only if readOnly, which ends with a commit if commit and with a roll back
// otherwise, on the engine and in SQLite. Each step must give its answer and the same count of changes
// in both, and after the end, both must hold the state want, also after a replay of the log into an
// empty cache. It gives the instance.
func pinTx(t *testing.T, setup []string, readOnly bool, steps []txStep, commit bool, want string) *instance {
	t.Helper()
	i := newInstance(t, engine.Options{})
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
		got := runIn(t, i, s)
		is.Equal(t, s.want, got, s.src)
		if s.own() {
			continue
		}
		a := o.run(t, s.src, s.args...)
		sqlite := a.err
		if sqlite == "" && strings.Contains(strings.ToLower(s.src), "select") {
			sqlite = strings.TrimSpace("rows " + strings.Join(a.rows, " "))
		}
		is.Equal(t, sqlite, got, "SQLite and the engine: "+s.src)
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
	i.restart(true, engine.Options{})
	is.Equal(t, want, i.state(), "the engine after a replay of the log")
	return i
}

const (
	insert1 = "insert into t1 (v1) values (1)"
	insert2 = "insert into t1 (v1) values (2)"
	insertP = "insert into t1 (v1) values (?)"
	selectT = "select * from t1"
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
				{src: "select * from t1 where v1 = 1", want: "not supported"},
				{src: " ; ", want: "no statement"},
				{src: insertP, args: []int64{3, 4}, want: "wrong arguments"},
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
				{src: insertP, args: []int64{-9223372036854775808}},
				{src: insertP, args: []int64{9223372036854775807}},
				{src: insertP, args: []int64{9223372036854775807}, want: "19/1555: UNIQUE constraint failed: t1.v1"},
				{src: insertP, args: []int64{0}},
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
				{src: insertP, args: []int64{3}, want: "8/8: attempt to write a readonly database"},
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
		{name: "should write nothing at the roll back of a read-only transaction", setup: []string{t1, insert1}, readOnly: true,
			steps: []txStep{{src: selectT, want: "rows 1"}, {src: insert2, want: "8/8: attempt to write a readonly database"}},
			want:  "t1 (v1 INT): 1\n"},
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
}

// txDifferential runs n statements of the sequence of seed, in transactions, as
// [TestConn_differential] describes.
func txDifferential(t *testing.T, seed uint64, n int, y tally) {
	t.Helper()
	i := newInstance(t, engine.Options{})
	defer i.close()
	r := rand.New(rand.NewPCG(seed, 0x7e))
	q := newSequence(t, r)
	p := newPair(t, i, y)
	var log []string
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("seed %v, after\n%v\n%v", seed, strings.Join(log, "\n"), fmt.Sprintf(format, args...))
	}
	// open is the kind of the transaction that is open: "", "write", or "read only".
	open := ""
	end := func() {
		t.Helper()
		commit := r.IntN(3) > 0
		log = append(log, fmt.Sprint("-- end the ", open, " transaction, with a commit: ", commit))
		if commit {
			is.NotError(t, i.c.Commit(t.Context()))
		} else {
			is.NotError(t, i.c.Rollback(t.Context()))
		}
		for _, s := range oracleEnd(open == "read only", commit) {
			is.Equal(t, "", p.o.run(t, s).err, s)
		}
		open = ""
		if got, want := i.state(), p.o.state(t); got != want {
			fail("after the end of the transaction the engine holds\n%v\nand SQLite\n%v", got, want)
		}
	}
	for range n {
		switch {
		case open == "" && r.IntN(4) == 0:
			open = "write"
			if r.IntN(3) == 0 {
				open = "read only"
				is.NotError(t, i.c.BeginReadOnly(t.Context()))
			} else {
				is.NotError(t, i.c.Begin(t.Context()))
			}
			log = append(log, "-- begin a "+open+" transaction")
			for _, s := range oracleBegin(open == "read only") {
				is.Equal(t, "", p.o.run(t, s).err, s)
			}
		case open != "" && r.IntN(8) == 0:
			end()
		}
		src, query, supported := q.Next()
		log = append(log, src)
		if failure := p.step(src, query, supported); failure != "" {
			fail("%v", failure)
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
