package drivercheck_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"maps"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-sqlite3"
	"maragu.dev/is"

	"maragu.dev/s4ql"
	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/expr"
	sqloracle "maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
	"maragu.dev/s4ql/internal/sql/token"
	"maragu.dev/s4ql/internal/store/s3test"
)

// call of database/sql that runs a text.
type call string

const (
	callExec         call = "exec"
	callQuery        call = "query"
	callPrepareExec  call = "prepare, exec"
	callPrepareQuery call = "prepare, query"
	callQueryRow     call = "query row"
	// callExecArgument is Exec with one argument, the integer 1, in the sequences written by hand and
	// in the fuzz target: a text gets SQLite's syntax error or the "not supported" of the text, as
	// without the argument, a statement that s4ql supports with no parameter gets s4ql's error of the
	// wrong number of arguments, and one with one parameter runs.
	callExecArgument call = "exec with an argument"
	// callBegin is BeginTx of a write transaction, which stays open: the calls after it run in it,
	// until [callCommit] or [callRollback]. Its text is the name of its isolation level, as
	// [sql.IsolationLevel.String] gives it, or "" for the default; see [pair.begin].
	callBegin call = "begin"
	// callQueryArgument is Query with one argument, the integer 1, as [callExecArgument] is Exec.
	callQueryArgument call = "query with an argument"
	// callPrepareExecRuns is Prepare, and then Exec of the statement once for each run of arguments:
	// in the sequences written by hand and in the fuzz target, the integers 1, 2, and 1, so that the
	// third run meets the key of the first.
	callPrepareExecRuns call = "prepare, exec several times"
	// callBeginReadOnly is [callBegin] of a read-only transaction.
	callBeginReadOnly call = "begin read-only"
	// callCommit and callRollback end the open transaction, and do nothing without one.
	callCommit   call = "commit"
	callRollback call = "rollback"
	// callStmtExec and callStmtQuery are Prepare where the transaction began, on the [sql.DB] or the
	// [sql.Conn], and Exec or Query of the statement in the transaction through [sql.Tx.StmtContext].
	// Without a transaction they are [callPrepareExec] and [callPrepareQuery].
	callStmtExec  call = "prepare outside, exec in the transaction"
	callStmtQuery call = "prepare outside, query in the transaction"
	// callQueryOpen is Query, and in a transaction, after the comparison, the query runs again on both
	// sides, reads its first row, and leaves its rows open for the next call; see [pair.openRows].
	callQueryOpen call = "query, rows left open"
	// callBeside is Query where the transaction began: on a pinned [sql.Conn], whose statements run
	// in its transaction, and through the pool on the [sql.DB], outside the transaction, so that it
	// must not see the transaction's changes. Through the pool it runs outside only a select, and any
	// other text in the transaction, because a write beside a write transaction waits for it.
	callBeside call = "query beside the transaction"
)

// calls in the order of the bytes of the fuzz target.
var calls = []call{
	callExec, callQuery, callPrepareExec, callPrepareQuery, callQueryRow, callExecArgument, callBegin, callQueryArgument, callPrepareExecRuns,
	callBeginReadOnly, callCommit, callRollback, callStmtExec, callStmtQuery, callQueryOpen, callBeside,
}

// levels that [callBegin] and [callBeginReadOnly] name by their texts in the fuzz target: the
// default, which is "", and every level that [sql.IsolationLevel] names, and one that it does not.
var levels = []sql.IsolationLevel{
	sql.LevelDefault, sql.LevelReadUncommitted, sql.LevelReadCommitted, sql.LevelWriteCommitted, sql.LevelRepeatableRead,
	sql.LevelSnapshot, sql.LevelSerializable, sql.LevelLinearizable, sql.IsolationLevel(99),
}

// levelName of l, as the text of a begin: "" for the default.
func levelName(l sql.IsolationLevel) string {
	if l == sql.LevelDefault {
		return ""
	}
	return l.String()
}

// fixedRuns are the arguments of each run of the call c in the sequences written by hand and in the
// fuzz target: one run with the integer 1 for a call with an argument, the runs of
// [callPrepareExecRuns], and one run without arguments for every other call.
func fixedRuns(c call) [][]any {
	switch c {
	case callExecArgument, callQueryArgument:
		return [][]any{{int64(1)}}
	case callPrepareExecRuns:
		return [][]any{{int64(1)}, {int64(2)}, {int64(1)}}
	}
	return [][]any{nil}
}

// outcome of a text through a call: what an application sees.
type outcome struct {
	call call
	// failed is the step of the call that failed: "prepare", "exec", "query", "rows", "scan",
	// "begin", "commit", or "rollback", or "" if none failed.
	failed string
	// err is the error of the failed step: "code/extended: message" for SQLite's, "no rows" for
	// [sql.ErrNoRows], and the message for s4ql's own errors.
	err string
	// affected and lastID are of the [sql.Result] of an exec, or "" where there is none or it is not
	// compared.
	affected, lastID string
	// columns of a query, by their names and by the names of their database types, and the rows, in
	// the order in which they came, each as [rowText] gives it.
	columns, types string
	rows           []string
	// scans of the rows of a query, one for each row and in the order of the rows, each the values of
	// a row as a scan into a sql.Null* value and into a pointer gives them, as [readRow] gives it,
	// which [outcome.full] has and its String does not.
	scans []string
	// null reports whether a value of the rows is NULL. It is not in the text of the outcome, which
	// holds each NULL.
	null bool
	// next are the outcomes of the runs after the first of [callPrepareExecRuns], without a call.
	next []outcome
}

// String of the outcome, with the rows of a query as a multiset. SQLite leaves the order of the rows
// of a select without ORDER BY unspecified, and s4ql varies it from one query to the next, so no
// comparison is by the order of the rows: the text holds the rows in the order of their texts, and
// the rows field keeps the order in which they came. The text of a row tells its values apart from
// those of every other row, as [rowText] says, and ends where its bracket closes, so the list of
// the texts in order is the same for two outcomes only if they have the same rows the same number of
// times: a row that is wrong changes an element of the list, and a row that is missing, or one too
// many, also one that is there twice, changes its length or another element.
func (o outcome) String() string {
	return o.text(false)
}

// full text of the outcome: its String, with each row beside its scan into sql.Null* values and
// pointers, as [outcome.pairs] gives them, in the order of their texts, which a comparison of two
// outcomes compares too. A row stays with its own scan, so a scan that belongs to another row
// changes two elements of the list.
func (o outcome) full() string {
	return o.text(true)
}

// text of the outcome: its [outcome.full] if scans, and its String otherwise.
func (o outcome) text(scans bool) string {
	var b strings.Builder
	b.WriteString(string(o.call))
	if o.failed != "" {
		fmt.Fprintf(&b, ": %v failed with %v", o.failed, o.err)
	} else if o.affected != "" || o.lastID != "" {
		fmt.Fprintf(&b, ": %v affected, last insert id %v", o.affected, o.lastID)
	}
	rows, what := o.rows, "rows"
	if scans && len(o.scans) > 0 {
		rows, what = o.pairs(), "rows, each with its scan into sql.Null* values and pointers,"
	}
	rows = slices.Sorted(slices.Values(rows))
	switch {
	case o.failed != "":
	case o.columns != "" || o.types != "":
		fmt.Fprintf(&b, ": columns %q of types %q, %v %v", o.columns, o.types, what, strings.Join(rows, " "))
	case len(rows) > 0:
		// A row of QueryRow, whose columns database/sql does not give.
		fmt.Fprintf(&b, ": row %v", strings.Join(rows, " "))
	}
	for _, n := range o.next {
		fmt.Fprintf(&b, "; then%v", n.text(scans))
	}
	return b.String()
}

// pairs of the outcome: each row of a query with its scan, as [pairText] gives them, in the order in
// which the rows came.
func (o outcome) pairs() []string {
	pairs := make([]string, len(o.rows))
	for i, row := range o.rows {
		pairs[i] = pairText(row, o.scans[i])
	}
	return pairs
}

// runs of the outcome: itself, and the outcomes of its later runs.
func (o outcome) runs() []outcome {
	return append([]outcome{o}, o.next...)
}

// notSupported reports whether the outcome is s4ql's "not supported".
func (o outcome) notSupported() bool {
	return strings.HasPrefix(o.err, "s4ql: not supported: ")
}

// runner is what [sql.DB], [sql.Conn], and [sql.Tx] have in common: where a text runs.
type runner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
}

// queryer is a [runner] that begins transactions: a [sql.DB] or a [sql.Conn].
type queryer interface {
	runner
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// inTx is a transaction, as a [runner], with the queryer that it began on, where [callStmtExec] and
// [callStmtQuery] prepare their statements.
type inTx struct {
	*sql.Tx
	outer queryer
}

// run src through the call c on r, with the arguments of each run, and with errText for the text of
// an error. Every call but [callPrepareExecRuns] makes one run, with the arguments of the first.
func run(tb testing.TB, r runner, c call, src string, runs [][]any, errText func(testing.TB, error) string) outcome {
	tb.Helper()
	ctx := tb.Context()
	o := outcome{call: c}
	failed := func(step string, err error) outcome {
		o.failed, o.err = step, errText(tb, err)
		return o
	}
	var args []any
	if len(runs) > 0 {
		args = runs[0]
	}
	switch c {
	case callExec, callExecArgument:
		res, err := r.ExecContext(ctx, src, args...)
		if err != nil {
			return failed("exec", err)
		}
		return result(tb, o, res)
	case callQuery, callQueryArgument, callQueryOpen, callBeside:
		rows, err := r.QueryContext(ctx, src, args...)
		if err != nil {
			return failed("query", err)
		}
		return read(tb, o, rows, errText)
	case callPrepareExec, callPrepareQuery, callPrepareExecRuns, callStmtExec, callStmtQuery:
		// A statement prepared where its transaction began runs in the transaction through
		// [sql.Tx.StmtContext], which database/sql closes when the transaction ends.
		where := r
		t, outside := r.(inTx)
		outside = outside && (c == callStmtExec || c == callStmtQuery)
		if outside {
			where = t.outer
		}
		prepared, err := where.PrepareContext(ctx, src)
		if err != nil {
			return failed("prepare", err)
		}
		defer func() { is.NotError(tb, prepared.Close()) }()
		s := prepared
		if outside {
			// Its Close gives the error of a prepare in the transaction that failed, which its run gives
			// too, and the end of the transaction closes it.
			s = t.StmtContext(ctx, prepared)
		}
		switch c {
		case callPrepareExec, callStmtExec:
			res, err := s.ExecContext(ctx, args...)
			if err != nil {
				return failed("exec", err)
			}
			return result(tb, o, res)
		case callPrepareExecRuns:
			for i, args := range runs {
				var r outcome
				if res, err := s.ExecContext(ctx, args...); err != nil {
					r.failed, r.err = "exec", errText(tb, err)
				} else {
					r = result(tb, r, res)
				}
				if i == 0 {
					r.call = c
					o = r
				} else {
					o.next = append(o.next, r)
				}
			}
			return o
		}
		rows, err := s.QueryContext(ctx, args...)
		if err != nil {
			return failed("query", err)
		}
		return read(tb, o, rows, errText)
	case callQueryRow:
		return queryRow(tb, r, src, args, 0, errText)
	}
	tb.Fatalf("no call %q", c)
	return o
}

// queryRow is [run] of src through [callQueryRow] on r, with the arguments args, into width
// destinations, or, for a width of 0, into as many as the row has. database/sql gives the error of
// the query at Scan, and gives no columns of a row: with a width of 0 the query runs with one
// destination, and again with as many as database/sql says that the row has, if it has more. A
// statement that writes, as one with RETURNING, must run once, so its width is given.
func queryRow(tb testing.TB, r runner, src string, args []any, width int, errText func(testing.TB, error) string) outcome {
	tb.Helper()
	ctx := tb.Context()
	o := outcome{call: callQueryRow}
	failed := func(step string, err error) outcome {
		o.failed, o.err = step, errText(tb, err)
		return o
	}
	{
		values := make([]any, max(width, 1))
		for {
			dest := make([]any, len(values))
			for i := range values {
				dest[i] = &values[i]
			}
			err := r.QueryRowContext(ctx, src, args...).Scan(dest...)
			var n int
			if _, scanned := fmt.Sscanf(fmt.Sprint(err), "sql: expected %d destination arguments in Scan, not 1", &n); width == 0 && err != nil && scanned == nil && len(values) == 1 {
				values = make([]any, n)
				continue
			}
			if err != nil {
				return failed("scan", err)
			}
			break
		}
		for _, v := range values {
			o.null = o.null || v == nil
		}
		o.rows = []string{rowText(values)}
		return o
	}
}

// runS4QL is [run] of src through the call c on s4ql's s, with the arguments of each run, where the
// call runs on SQLite's o. The QueryRow of an insert, an update, or a delete with RETURNING runs once,
// into as many destinations as the columns of its RETURNING, which it gives on the table as o has it,
// since a second run would change the database again.
func (p *pair) runS4QL(s, o runner, c call, src string, runs [][]any) outcome {
	tb := p.tb
	tb.Helper()
	if st, _ := stmt.Check(src); c == callQueryRow && returns(st) {
		var args []any
		if len(runs) > 0 {
			args = runs[0]
		}
		return queryRow(tb, s, src, args, p.returningWidth(st, o), s4qlText)
	}
	return run(tb, s, c, src, runs, s4qlText)
}

// returningWidth is the number of the columns of the RETURNING of s, an insert, an update, or a
// delete, on its table as o has it: those that it names, or every column of the table for *, and 1
// for * on a table that o does not have, whose statement fails.
func (p *pair) returningWidth(s stmt.Statement, o runner) int {
	var table stmt.Name
	var r stmt.Returning
	switch s := s.(type) {
	case *stmt.Insert:
		table, r = s.Table, s.Returning
	case *stmt.Update:
		table, r = s.Table, s.Returning
	case *stmt.Delete:
		table, r = s.Table, s.Returning
	}
	if r.Columns != nil {
		return len(r.Columns)
	}
	_, columns := p.columnsIn(o, table.Value)
	return max(len(columns), 1)
}

// result of an exec into o.
func result(tb testing.TB, o outcome, res sql.Result) outcome {
	tb.Helper()
	affected, err := res.RowsAffected()
	is.NotError(tb, err)
	id, err := res.LastInsertId()
	is.NotError(tb, err)
	o.affected, o.lastID = fmt.Sprint(affected), fmt.Sprint(id)
	return o
}

// read the rows of a query into o, and close them.
func read(tb testing.TB, o outcome, rows *sql.Rows, errText func(testing.TB, error) string) outcome {
	tb.Helper()
	defer func() { is.NotError(tb, rows.Close()) }()
	columns, err := rows.Columns()
	is.NotError(tb, err)
	types, err := rows.ColumnTypes()
	is.NotError(tb, err)
	var names []string
	for _, ct := range types {
		names = append(names, ct.DatabaseTypeName())
	}
	o.columns, o.types = strings.Join(columns, ","), strings.Join(names, ",")
	for rows.Next() {
		row, scan, null := readRow(tb, rows, names)
		o.rows, o.scans, o.null = append(o.rows, row), append(o.scans, scan), o.null || null
	}
	if err := rows.Err(); err != nil {
		o.failed, o.err = "rows", errText(tb, err)
		o.columns, o.types, o.rows, o.scans, o.null = "", "", nil, nil, false
	}
	return o
}

// readRow that the Next of rows found, whose columns have the database types of the names: its text,
// as [rowText] gives it, the text of its scan, and whether a value of it is NULL. The scan is the row
// read again into a sql.Null* value for each column, and into a pointer for each, so that the scan
// of NULL into each is compared too: the list of the texts of the destinations, as [typedText] gives
// them, or of the error of a scan that failed, each in quotes, so that two scans have one text only
// if they are the same.
func readRow(tb testing.TB, rows *sql.Rows, names []string) (row, scan string, null bool) {
	tb.Helper()
	values := make([]any, len(names))
	dest := make([]any, len(names))
	for i := range values {
		dest[i] = &values[i]
	}
	is.NotError(tb, rows.Scan(dest...))
	for _, v := range values {
		null = null || v == nil
	}
	var texts []string
	for _, pointer := range []bool{false, true} {
		typed := make([]any, len(names))
		for i, v := range values {
			typed[i] = typedDest(v, names[i], pointer)
		}
		if err := rows.Scan(typed...); err != nil {
			texts = append(texts, "scan: "+err.Error())
			continue
		}
		for _, d := range typed {
			texts = append(texts, typedText(d))
		}
	}
	return rowText(values), fmt.Sprintf("%q", texts), null
}

// rowText of the values of a row that database/sql scanned into an any each: the list of their
// texts, as [valueText] gives them, each in quotes, such as ["int64:1" "string:a"]. Two rows have
// one text only if they have the same values: a quote, a comma, a space, or a bracket inside text is
// inside the quotes of its value, where a quote is escaped, so no value runs into the next, and the
// text of each value tells it apart from every other value.
func rowText(values []any) string {
	texts := make([]string, len(values))
	for i, v := range values {
		texts[i] = valueText(v)
	}
	return fmt.Sprintf("%q", texts)
}

// pairText of a row and its scan, as [readRow] gives them. The text of the row ends where its
// bracket closes, so two pairs have one text only if both their rows and their scans do.
func pairText(row, scan string) string {
	return row + " as " + scan
}

// valueText of a value that database/sql scanned into an any: its Go type and its value. The type
// has no colon, and the value is all that follows the first colon, so two values have one text only
// if they are the same: NULL is <nil>:<nil> and the text "<nil>" is string:<nil>, -0.0 is float64:-0,
// and a []byte that is nil is []uint8:nil, where an empty one is []uint8:[].
func valueText(v any) string {
	if b, ok := v.([]byte); ok && b == nil {
		return "[]uint8:nil"
	}
	return fmt.Sprintf("%T:%v", v, v)
}

// typedDest is a destination of a scan for the value v that database/sql scanned into an any, of a
// column of the type of the database typ: a pointer to a pointer if pointer, and otherwise a pointer
// to a sql.Null* value, of the Go type of v, or for NULL of the Go type that a value of typ has.
func typedDest(v any, typ string, pointer bool) any {
	switch v.(type) {
	case nil:
		switch typ {
		case "INT", "INTEGER":
			v = int64(0)
		case "REAL":
			v = float64(0)
		case "BLOB":
			v = []byte{}
		default:
			v = ""
		}
		return typedDest(v, typ, pointer)
	case int64:
		if pointer {
			return new(*int64)
		}
		return new(sql.NullInt64)
	case float64:
		if pointer {
			return new(*float64)
		}
		return new(sql.NullFloat64)
	case []byte:
		if pointer {
			return new(*[]byte)
		}
		return new(sql.Null[[]byte])
	}
	if pointer {
		return new(*string)
	}
	return new(sql.NullString)
}

// typedText of a destination of [typedDest] after a scan: its Go type and its value as Go writes it,
// with text in quotes, or nil for a pointer that is nil, so that NULL in a *string is *string:nil and
// the text "nil" in one is *string:"nil".
func typedText(d any) string {
	v := reflect.ValueOf(d).Elem()
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return fmt.Sprintf("%v:nil", v.Type())
		}
		return fmt.Sprintf("%v:%#v", v.Type(), v.Elem().Interface())
	}
	return fmt.Sprintf("%#v", v.Interface())
}

// s4qlText of an error of s4ql's driver, or of database/sql around it. Any error that is not
// SQLite's, "not supported", or [sql.ErrNoRows] fails the test, and so does one that breaks what
// the package's documentation promises of these.
func s4qlText(tb testing.TB, err error) string {
	tb.Helper()
	if errors.Is(err, driver.ErrBadConn) {
		tb.Fatalf("s4ql gives an error that is driver.ErrBadConn: %v", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "no rows"
	}
	if e, ok := errors.AsType[*s4ql.Error](err); ok {
		if errors.Is(err, s4ql.ErrNotSupported) || err.Error() != e.Message {
			tb.Fatalf("s4ql gives SQLite's error %+v as %q, which matches ErrNotSupported: %v", e, err, errors.Is(err, s4ql.ErrNotSupported))
		}
		return fmt.Sprint(e.Code, "/", e.ExtendedCode, ": ", e.Message)
	}
	if errors.Is(err, s4ql.ErrNotSupported) {
		if !strings.HasPrefix(err.Error(), "s4ql: not supported: ") {
			tb.Fatalf("s4ql gives %q, which matches ErrNotSupported", err)
		}
		return err.Error()
	}
	if errors.Is(err, s4ql.ErrEvaluation) {
		// An evaluation error of arithmetic matches the kind that its message names, which is the kind
		// that [stmttest.Justify] judges, and no other kind and no other sentinel; its message names
		// the kind, the operator, and the offset of its node. The message of a kind starts with that
		// of its sentinel, as "s4ql: integer overflow in + (at offset 42)" with that of
		// [s4ql.ErrOverflow].
		if _, ok := stmttest.EvaluationOf(err.Error()); !ok || errors.Is(err, s4ql.ErrNotSupported) {
			tb.Fatalf("s4ql gives the evaluation error %q, whose message names no kind of evaluation error, or which matches ErrNotSupported", err)
		}
		for _, kind := range []error{s4ql.ErrOverflow, s4ql.ErrDivisionByZero, s4ql.ErrNotFinite} {
			if named := strings.HasPrefix(err.Error(), kind.Error()+" in "); errors.Is(err, kind) != named {
				tb.Fatalf("s4ql gives the evaluation error %q, which matches %v: %v, where its message names that kind: %v", err, kind, errors.Is(err, kind), named)
			}
		}
		return err.Error()
	}
	if strings.HasPrefix(err.Error(), wrongArguments) {
		// Another number of arguments than the statement has parameters, which is an error of s4ql's
		// own and matches none of its sentinels.
		noSentinel(tb, err)
		return err.Error()
	}
	tb.Fatalf("s4ql gives %v", err)
	return ""
}

// noSentinel fails the test if err, an error of s4ql's own, matches one of s4ql's sentinels.
func noSentinel(tb testing.TB, err error) {
	tb.Helper()
	for _, sentinel := range []error{s4ql.ErrNotSupported, s4ql.ErrNotServing, s4ql.ErrOutcomeUnknown, s4ql.ErrStopped, s4ql.ErrTooLarge, s4ql.ErrEvaluation} {
		if errors.Is(err, sentinel) {
			tb.Fatalf("s4ql gives %q, which matches %v", err, sentinel)
		}
	}
}

// mattnText of an error of mattn's driver, or of database/sql around it: SQLite's error, or
// [sql.ErrNoRows]. Any other error fails the test.
func mattnText(tb testing.TB, err error) string {
	tb.Helper()
	if errors.Is(err, sql.ErrNoRows) {
		return "no rows"
	}
	if e, ok := errors.AsType[sqlite3.Error](err); ok {
		return fmt.Sprint(int(e.Code), "/", int(e.ExtendedCode), ": ", e.Error())
	}
	tb.Fatalf("mattn/go-sqlite3 gives %v", err)
	return ""
}

// firstStatement of src: the text up to the end of its first semicolon, or all of it. It is what
// the oracle runs of a text that s4ql runs or answers with SQLite's error, which holds one statement
// and maybe semicolons, spaces, and comments after it: Query and Prepare refuse a second statement,
// and a text of several statements in an Exec is [pair.several]'s. mattn runs every statement of a
// text, and one that is only a comment too, and then gives
// the result of the last: 0 rows affected after `insert into t1 (v1) values (1); -- c`, and no rows
// for `select * from t1; -- c`. SQLite's sqlite3_prepare_v2 prepares the first statement, and so
// does the engine's oracle.
//
// Semicolons before the statement are kept: mattn's Prepare gives a statement that crashes the
// process in Exec for a text that holds no statement.
func firstStatement(src string) string {
	started := false
	for _, t := range token.Tokens(src) {
		switch {
		case t.Kind == token.Space || t.Kind == token.Comment:
		case t.Kind == token.Semi && started:
			return src[:t.Offset+t.Len]
		case t.Kind != token.Semi:
			started = true
		}
	}
	return src
}

// world of the instances of s4ql of a test: a fake S3 that holds a database for each prefix.
type world struct {
	s3 *fakeS3
	// prefixes counts the databases made.
	prefixes atomic.Int64
}

func newWorld(tb testing.TB) *world {
	tb.Helper()
	s3test.Isolate(tb)
	return &world{s3: newFakeS3(tb)}
}

// options of an instance of the database at prefix, with its cache in the directory cache.
func (w *world) options(prefix, cache string) s4ql.Options {
	return s4ql.Options{
		Bucket: "bucket", Prefix: prefix, Endpoint: w.s3.srv.URL, Region: "us-east-1", AccessKeyID: "test", SecretAccessKey: "test",
		PathStyle: true, CacheDir: cache, CloseTimeout: time.Minute,
	}
}

// open an instance of s4ql with the options, and wait until it serves. The test closes it, or the
// cleanup does when the test fails first.
func open(tb testing.TB, o s4ql.Options) *sql.DB {
	tb.Helper()
	db, err := s4ql.Open(tb.Context(), o)
	is.NotError(tb, err)
	tb.Cleanup(func() { _ = db.Close() })
	is.NotError(tb, db.PingContext(tb.Context()))
	return db
}

// tally of the outcomes of the texts of a test, which counts nothing if it is nil.
type tally map[string]int

// comparedWithSQLite is the kind under which a tally counts the calls that ran in SQLite, whose
// outcomes [pair.compare] compared with s4ql's.
const comparedWithSQLite = "compared with SQLite"

// compared counts a call that runs in SQLite, under [comparedWithSQLite].
func (y tally) compared() {
	if y != nil {
		y[comparedWithSQLite]++
	}
}

// add the outcome got of s4ql for src through its call with the arguments of each run: the call, with
// arguments if a run has any, and the outcome of each run, by the kind of the statement, with a
// parameter if it has one. Each is counted once by itself, and once more after scope and a colon if
// scope is not "", such as "write transaction" for a call in one.
func (y tally) add(scope, src string, got outcome, runs [][]any) {
	if y == nil {
		return
	}
	count := func(name string) {
		y[name]++
		if scope != "" {
			y[scope+": "+name]++
		}
	}
	count("call " + string(got.call))
	if slices.ContainsFunc(runs, func(args []any) bool { return len(args) > 0 }) {
		count("call " + string(got.call) + " with arguments")
	}
	kind := statementKind(src)
	for _, o := range got.runs() {
		switch {
		case o.failed == "":
			count(kind + " ok")
			if len(o.rows) > 0 && o.columns != "" {
				count(kind + " ok with rows")
			}
		case strings.HasPrefix(o.err, "s4ql: not supported: the named argument "):
			count(kind + " named argument")
		case strings.HasPrefix(o.err, "s4ql: not supported: argument "):
			count(kind + " argument of a type not supported")
		case o.err == rowsOpen:
			count(kind + " while rows are open")
		case o.notSupported():
			count(kind + " not supported")
		default:
			count(kind + " " + messageKind(o.err))
		}
	}
}

// statementKind of src: the kind of the statement of slices 1 and 2 that it is, in lower case and
// with a parameter if it has one, "several statements" for a text that [stmt.Split] reads as more
// than one, or "other".
func statementKind(src string) string {
	s, _ := stmt.Check(src)
	if parts, _ := stmt.Split(src); len(parts) > 1 {
		return "several statements"
	}
	if s == nil {
		return "other"
	}
	kind := strings.ToLower(strings.TrimPrefix(fmt.Sprintf("%T", s), "*stmt."))
	if s.Parameters() > 0 {
		kind += " with a parameter"
	}
	return kind
}

// reach counts in the tally what the text src with the arguments of each run, whose outcome of s4ql
// is got, reaches, under "reached: ", on the tables that SQLite has on o, where the call runs,
// before it runs there. For a text of several statements through a call that refuses it: the call
// and its outcome; [pair.reachSeveral] counts what one through Exec reaches. For an insert: each argument by its kind, as [argumentKind] names it, each
// literal by its kind, as [stmttest.LiteralKind] names it, and, for an insert into a table that
// SQLite has, the outcome of each run by the type of the key column; of each run that reaches the
// instance, by what it reaches of NULL, as [stmttest.NullCases] gives it, and of defaults, as
// [stmttest.DefaultCases] gives them; and for a table of several
// columns by the form of the insert, without a list or with one, and by its faults, as
// [stmttest.Faults] gives them, one or two. For a select of a table that SQLite has, the outcome of
// each run by its form: * by the number of columns, names, and a name that the table does not have;
// and of * and of names with NULL among the values of its rows, or, for a QueryRow, of the row that
// SQLite gives.
func (p *pair) reach(src string, runs [][]any, got outcome, o runner) {
	tb := p.tb
	tb.Helper()
	if p.y == nil {
		return
	}
	count := func(what string) { p.y["reached: "+what]++ }
	kind := func(o outcome) string {
		switch {
		case strings.HasPrefix(o.err, "s4ql: not supported: ") && (strings.Contains(o.err, " for the column ") || strings.Contains(o.err, " compared with the column ")):
			return "a value of another type"
		case o.notSupported():
			return "not supported"
		case o.failed != "":
			return messageKind(o.err)
		}
		return "ok"
	}
	if statementKind(src) == "several statements" {
		// Through every call but Exec, which [pair.reachSeveral] counts.
		count(fmt.Sprintf("several statements through %v %v", got.call, kind(got)))
	}
	s, _ := stmt.Check(src)
	// A value on the left, of slice 12, on a table that a create table if not exists, of slice 10,
	// found: a meeting of two areas that only the mixed profile has on.
	if table, where := tableWhere(s); p.found[table.Fold()] && slices.ContainsFunc(where, func(c stmt.Condition) bool { return c.ValueFirst }) {
		count("value on the left on a table that a create table if not exists found")
	}
	p.reachReturning(s, runs, got, o, kind, count)
	p.reachSubquery(src, s, runs, got, o, kind, count)
	switch s.(type) {
	case *stmt.Update, *stmt.Delete:
		p.reachChange(s, runs, got, o, kind, count)
		return
	}
	found := false
	p.reachExists(src, s, runs, got, o, kind, func(what string) {
		count(what)
		found = found || what == "create table if not exists did nothing: a table of the name exists"
	})
	switch s := s.(type) {
	case *stmt.CreateTable:
		if found {
			p.found[s.Table.Fold()] = true
		}
	case *stmt.DropTable:
		if outs := got.runs(); len(outs) > 0 && outs[0].failed == "" {
			delete(p.found, s.Table.Fold())
		}
	}
	if sel, ok := s.(*stmt.Select); ok {
		_, columns := p.columnsIn(o, sel.Table.Value)
		if columns == nil {
			return
		}
		p.reachWhere(sel, runs, got, o, kind, columns, count)
		p.reachOrder(sel, runs, got, o, kind, columns, count)
		for i, out := range got.runs() {
			// The rows of a QueryRow, and of a select with LIMIT, are those that s4ql's scan meets
			// first, among those that tie under an ORDER BY, which vary with the direction of the scan
			// that it draws, so NULL is counted by the rows that SQLite gives for the run, which do not.
			// The rows of every other select are SQLite's, as the comparison requires. A run that failed
			// or gave no row has none.
			null := out.failed == "" && out.null
			if out.failed == "" && len(out.rows) > 0 && (got.call == callQueryRow || sel.Limited()) && i < len(runs) {
				c := callQuery
				if got.call == callQueryRow {
					c = callQueryRow
				}
				null = run(tb, o, c, firstStatement(src), runs[i:i+1], mattnText).null
			}
			if null {
				count(map[bool]string{true: "select * with NULL ", false: "select of names with NULL "}[sel.Columns == nil] + kind(out))
			}
			switch {
			case sel.Columns == nil:
				count(fmt.Sprint("select * of ", len(columns), " columns ", kind(out)))
			case slices.ContainsFunc(sel.Columns, func(n stmt.Name) bool { return stmttest.ColumnOf(columns, n) < 0 }):
				count("select of a name that the table does not have " + kind(out))
			default:
				count("select of names " + kind(out))
			}
		}
		return
	}
	insert, ok := s.(*stmt.Insert)
	if !ok {
		return
	}
	if insert.Parameters() > 0 {
		for _, args := range runs {
			for _, a := range args {
				count("argument " + argumentKind(a))
			}
		}
	}
	for _, v := range insert.Values {
		if v.Parameter == 0 {
			count("literal " + stmttest.LiteralKind(v.Value))
		}
	}
	table, columns := p.columnsIn(o, insert.Table.Value)
	var typ string
	for _, c := range columns {
		if c.PrimaryKey {
			typ = c.Type
		}
	}
	if typ == "" {
		return
	}
	indexes := p.indexesIn(o, table)
	for i, o := range got.runs() {
		count("insert into " + typ + " " + kind(o))
		if i >= len(runs) || !reaches(src, runs[i]) {
			continue
		}
		var bound []any
		for _, a := range runs[i] {
			if v, ok := stmttest.Integers([]any{a}); ok {
				a = v[0]
			}
			bound = append(bound, converted(a))
		}
		for _, what := range stmttest.NullCases(columns, insert, bound) {
			count(what + " " + kind(o))
		}
		for _, what := range stmttest.DefaultCases(columns, insert, bound) {
			count(what + " " + kind(o))
		}
		// A default, of slice 11, that an index, of slice 9, holds: a meeting of two areas that only
		// the mixed profile has on.
		if len(bound) == insert.Parameters() && stmttest.LeavesOutIndexed(columns, indexes, insert) {
			count("insert that leaves out a column with a default that an index holds " + kind(o))
		}
		if len(columns) < 2 {
			continue
		}
		form := "insert without a list"
		if insert.Columns != nil {
			form = "insert with a list"
		}
		count(form + " " + kind(o))
		switch faults := stmttest.Faults(columns, insert, bound); len(faults) {
		case 0:
		case 1:
			count("insert with " + faults[0] + ": " + kind(o))
		default:
			count("insert with two faults: " + kind(o))
		}
	}
}

// reachReturning counts what the insert, the update, or the delete s with RETURNING, of slice 14,
// with the arguments of each run, whose outcome of s4ql is got, reaches on its table on o, as
// [pair.reach] says, with the kind of each outcome as kind gives it: for each run, the statement by
// its call and its outcome, and the form of its RETURNING, *, names, a name twice, or a name that
// the table does not have; for a query that reads its rows to their end and succeeds, whether it
// gives no row, one, or several; and goqite's two statements as far as s4ql takes them: its send, an
// insert that leaves out a key with a default and gives the key by RETURNING, and its receive by
// id, an update of the row of a key by an argument, of columns outside the key, that gives the row
// by RETURNING.
func (p *pair) reachReturning(s stmt.Statement, runs [][]any, got outcome, o runner, kind func(outcome) string, count func(string)) {
	r := returningOf(s)
	if !r.Present() {
		return
	}
	statement := map[bool]string{true: "insert", false: "update"}[isInsert(s)]
	if _, ok := s.(*stmt.Delete); ok {
		statement = "delete"
	}
	statement += " with RETURNING"
	table, _ := tableWhere(s)
	if insert, ok := s.(*stmt.Insert); ok {
		table = insert.Table
	}
	_, columns := p.columnsIn(o, table.Value)
	form := "of names"
	var positions []int
	for _, n := range r.Columns {
		position := stmttest.ColumnOf(columns, n)
		if position < 0 {
			form = "of a name that the table does not have"
			break
		}
		if slices.Contains(positions, position) {
			form = "of a name twice"
		}
		positions = append(positions, position)
	}
	if r.Columns == nil {
		form = "*"
		for i := range columns {
			positions = append(positions, i)
		}
	}
	query := slices.Contains([]call{callQuery, callPrepareQuery, callStmtQuery, callQueryArgument, callBeside}, got.call)
	for i, out := range got.runs() {
		count(statement + " through " + string(got.call) + " " + kind(out))
		if columns == nil {
			continue
		}
		count(statement + " " + form + " " + kind(out))
		if !query || out.failed != "" {
			continue
		}
		count(statement + " that gives " + map[bool]string{true: "no row", false: map[bool]string{true: "one row", false: "several rows"}[len(out.rows) == 1]}[len(out.rows) == 0])
		if len(out.rows) != 1 || i >= len(runs) {
			continue
		}
		key := slices.IndexFunc(columns, func(c stmttest.TableColumn) bool { return c.PrimaryKey })
		if key < 0 || !slices.Contains(positions, key) {
			continue
		}
		switch s := s.(type) {
		case *stmt.Insert:
			if s.Columns != nil && columns[key].Default != "" && !slices.ContainsFunc(s.Columns, func(n stmt.Name) bool { return stmttest.ColumnOf(columns, n) == key }) {
				count("goqite's send: an insert that leaves out a key with a default and gives the key by RETURNING")
			}
		case *stmt.Update:
			if len(s.Where) == 1 && s.Where[0].Operator == stmt.OpEqual && s.Where[0].Value.Parameter > 0 && stmttest.ColumnOf(columns, s.Where[0].Column) == key &&
				!slices.ContainsFunc(s.Set, func(a stmt.Assignment) bool { return stmttest.ColumnOf(columns, a.Column) == key }) && len(positions) > 1 {
				count("goqite's receive by id: an update of the row of a key by an argument that gives the row by RETURNING")
			}
		}
	}
}

// reachSubquery counts what the select, the update, or the delete s of the text src with scalar subqueries, of slice
// 15, with the arguments of each run, whose outcome of s4ql is got, reaches on the tables of o, as
// [pair.reach] says, with the kind of each outcome as kind gives it: for each run, the statement by
// its call and by its outcome, with two subqueries, and each subquery by its operator, on its side and
// on either, and by its table, that of the statement or another; for the first run, which o has the tables of,
// each subquery that s4ql computes, by its rows on o: none, LIMIT 0, a first row whose value is NULL
// or is not, several rows, and several whose first two tie under its ORDER BY but for its last
// column, the key, which [stmttest.Sequence.Decide] puts there; and goqite's receive, by its call,
// as [goqiteReceive] says.
func (p *pair) reachSubquery(src string, s stmt.Statement, runs [][]any, got outcome, o runner, kind func(outcome) string, count func(string)) {
	tb := p.tb
	tb.Helper()
	subs := subqueriesIn(s)
	if subs == nil {
		return
	}
	statement := strings.ToLower(strings.TrimPrefix(fmt.Sprintf("%T", s), "*stmt."))
	if returns(s) {
		statement += " with RETURNING"
	}
	statement += " with a subquery"
	table, where := tableWhere(s)
	_, columns := p.columnsIn(o, table.Value)
	for _, out := range got.runs() {
		outcome := kind(out)
		count(statement + " through " + string(got.call) + " " + outcome)
		if len(subs) > 1 {
			count(statement + " with two subqueries " + outcome)
		}
		for _, c := range where {
			if c.Value.Subquery == 0 {
				continue
			}
			sub := subs[c.Value.Subquery-1]
			count(fmt.Sprint("subquery compared by ", c.Operator, map[bool]string{true: " with its value on the left ", false: " "}[c.ValueFirst], outcome))
			count(fmt.Sprint("subquery compared by ", c.Operator, " on either side ", outcome))
			count("subquery of " + map[bool]string{true: "the table of its statement ", false: "another table "}[sub.Table.Fold() == table.Fold()] + outcome)
		}
		if columns != nil && goqiteReceive(s, columns, runs, out) {
			count("goqite's receive: an update with RETURNING of the row whose key a subquery of its own table gives, by an ORDER BY of two columns or more and LIMIT 1, beside a pin, that changes the row, through " + string(got.call))
			count("goqite's receive: an update with RETURNING of the row whose key a subquery of its own table gives, by an ORDER BY of two columns or more and LIMIT 1, beside a pin, that changes the row")
		}
	}
	if columns == nil || len(runs) == 0 || len(runs[0]) != s.Parameters() || argumentRefusal(runs[0]) != "" {
		return
	}
	var bound []any
	for _, a := range runs[0] {
		if v, ok := stmttest.Integers([]any{a}); ok {
			a = v[0]
		}
		bound = append(bound, converted(a))
	}
	if p.subqueryRefusal(src, s, bound, o) != "" {
		return
	}
	for _, sub := range subs {
		_, inner := p.columnsIn(o, sub.Table.Value)
		names := slices.Clone(sub.Columns)
		for _, c := range sub.Where {
			names = append(names, conditionNames(c)...)
		}
		for _, c := range sub.OrderBy {
			names = append(names, c.Name)
		}
		// A subquery with arithmetic, whose rows decide its errors, is not counted.
		if inner == nil || sub.Columns == nil && len(inner) != 1 || slices.ContainsFunc(names, func(n stmt.Name) bool { return stmttest.ColumnOf(inner, n) < 0 }) ||
			stmttest.HasExpression(&stmt.Select{Where: sub.Where}) || limitArgument(sub.Limit.Parameter, bound) != "" || stmttest.WhereRule("t", inner, sub.Where, bound) != "" {
			continue
		}
		var by []string
		for _, c := range sub.OrderBy {
			term := stmttest.Quote(c.Name.Value)
			if c.Descending {
				term += " desc"
			}
			by = append(by, term)
		}
		orderBy := ""
		if by != nil {
			orderBy = " order by " + strings.Join(by, ", ")
		}
		text, args := stmttest.SelectText(&stmt.Select{Table: sub.Table, Columns: sub.Columns, Where: sub.Where}, bound, orderBy)
		rows := run(tb, o, callQuery, text, [][]any{args}, mattnText)
		if rows.failed != "" {
			tb.Fatalf("%q: SQLite gives %v", text, rows)
		}
		limit := int64(-1)
		switch {
		case sub.Limit.Parameter > 0:
			limit = bound[sub.Limit.Parameter-1].(int64)
		case sub.Limited():
			limit = int64(sub.Limit.Value.(stmt.Integer))
		}
		var what string
		switch {
		case limit == 0:
			what = "subquery of LIMIT 0"
		case len(rows.rows) == 0:
			what = "subquery that finds no row"
		case rows.rows[0] == `["<nil>:<nil>"]`:
			what = "subquery whose first row has NULL"
		default:
			what = "subquery whose first row has a value"
		}
		count(what)
		count(statement + ": " + what)
		if limit == 0 || len(rows.rows) < 2 {
			continue
		}
		count(statement + ": subquery of several rows")
		if len(sub.OrderBy) < 2 {
			continue
		}
		// The columns of the ORDER BY but the key, which [stmttest.Sequence.Decide] puts in each, of the
		// first two rows.
		var leading []stmt.Name
		for _, c := range sub.OrderBy {
			if !inner[stmttest.ColumnOf(inner, c.Name)].PrimaryKey {
				leading = append(leading, c.Name)
			}
		}
		if leading == nil {
			continue
		}
		text, args = stmttest.SelectText(&stmt.Select{Table: sub.Table, Columns: leading, Where: sub.Where}, bound, orderBy+" limit 2")
		if first := run(tb, o, callQuery, text, [][]any{args}, mattnText); first.failed == "" && len(first.rows) == 2 && first.rows[0] == first.rows[1] {
			count("subquery whose first two rows tie under its ORDER BY but for the key")
			count(statement + ": subquery whose first two rows tie under its ORDER BY but for the key")
		}
	}
}

// goqiteReceive reports whether the update s of the table of the columns, of the run out with the
// arguments of the runs, is goqite's receive as slice 15 has it: an update with RETURNING whose WHERE
// has a column outside the key equal to a value, and the key equal to a scalar subquery of the table
// of the update, of the key, with a WHERE, an ORDER BY of two columns or more, and LIMIT 1, and that
// changes one row.
func goqiteReceive(s stmt.Statement, columns []stmttest.TableColumn, runs [][]any, out outcome) bool {
	u, ok := s.(*stmt.Update)
	if !ok || !u.Returning.Present() || out.failed != "" {
		return false
	}
	changed := out.affected == "1" || out.columns != "" && len(out.rows) == 1 || out.call == callQueryRow
	if !changed {
		return false
	}
	key := slices.IndexFunc(columns, func(c stmttest.TableColumn) bool { return c.PrimaryKey })
	pin, next := false, false
	for _, c := range u.Where {
		position := stmttest.ColumnOf(columns, c.Column)
		switch {
		case position < 0:
			return false
		case c.Value.Subquery > 0 && position == key && c.Operator == stmt.OpEqual:
			sub := u.Subqueries[c.Value.Subquery-1]
			limit := sub.Limit.Value == stmt.Value(stmt.Integer(1))
			if sub.Limit.Parameter > 0 && len(runs) > 0 && sub.Limit.Parameter <= len(runs[0]) {
				n, ok := stmttest.Integers([]any{runs[0][sub.Limit.Parameter-1]})
				limit = ok && n[0] == 1
			}
			next = next || sub.Table.Fold() == u.Table.Fold() && len(sub.Columns) == 1 && stmttest.ColumnOf(columns, sub.Columns[0]) == key &&
				sub.Where != nil && len(sub.OrderBy) >= 2 && limit
		case c.Value.Subquery == 0 && position != key && c.Operator == stmt.OpEqual:
			pin = true
		}
	}
	return pin && next
}

// isInsert reports whether s is an insert.
func isInsert(s stmt.Statement) bool {
	_, ok := s.(*stmt.Insert)
	return ok
}

// reachWhere counts what the select sel with the arguments of each run, whose outcome of s4ql is got,
// reaches of WHERE, on its table with the columns on o, as [pair.reach] says, with the kind of each
// outcome as kind gives it: for each run that s4ql takes to the instance, each case of
// [stmttest.WhereCases], and for a query that reads its rows to their end and succeeds on a table
// with rows, whether it gives none, some, or all of them; and for any other run, one with an argument
// that s4ql does not support or with another number of arguments than parameters.
func (p *pair) reachWhere(sel *stmt.Select, runs [][]any, got outcome, o runner, kind func(outcome) string, columns []stmttest.TableColumn, count func(string)) {
	tb := p.tb
	tb.Helper()
	if sel.Where == nil {
		return
	}
	for i, out := range got.runs() {
		if i >= len(runs) {
			break
		}
		query := got.call == callQuery || got.call == callPrepareQuery || got.call == callStmtQuery
		if !reachesSelect(sel, runs[i]) {
			count("WHERE with arguments that s4ql refuses " + kind(out))
			reachValueFirst(sel, nil, kind(out), -1, count)
			continue
		}
		var bound []any
		for _, a := range runs[i] {
			if v, ok := stmttest.Integers([]any{a}); ok {
				a = v[0]
			}
			bound = append(bound, converted(a))
		}
		for _, what := range stmttest.WhereCases(columns, sel, bound) {
			count(what + " " + kind(out))
		}
		reachValueFirst(sel, bound, kind(out), map[bool]int{true: len(out.rows), false: -1}[query], count)
		if out.failed != "" || !query {
			continue
		}
		table, _ := p.columnsIn(o, sel.Table.Value)
		var n int
		is.NotError(tb, o.QueryRowContext(tb.Context(), "select count(*) from "+stmttest.Quote(table)).Scan(&n))
		switch {
		case n == 0:
		case len(out.rows) == 0:
			count("WHERE that gives no row of a table with rows")
		case len(out.rows) == n:
			count("WHERE that gives every row of a table with rows")
		default:
			count("WHERE that gives some rows of a table")
		}
	}
}

// reachOrder counts what the select sel with ORDER BY or LIMIT, of slice 13, with the arguments of
// each run, whose outcome of s4ql is got, reaches on its table with the columns on o, as [pair.reach]
// says, with the kind of each outcome as kind gives it: for each run that s4ql takes to the instance,
// a LIMIT by an argument, and an argument of LIMIT that s4ql refuses, by what it is, as
// [limitRefusal] names it; and for a query that reads its rows to their end and succeeds, an ORDER
// BY that gives two rows or more, whose order the comparison with SQLite checks, and a LIMIT that
// keeps some of the rows of the select without it and not all.
func (p *pair) reachOrder(sel *stmt.Select, runs [][]any, got outcome, o runner, kind func(outcome) string, columns []stmttest.TableColumn, count func(string)) {
	tb := p.tb
	tb.Helper()
	if sel.OrderBy == nil && !sel.Limited() {
		return
	}
	for i, out := range got.runs() {
		if i >= len(runs) {
			break
		}
		if !reachesSelect(sel, runs[i]) {
			continue
		}
		var bound []any
		for _, a := range runs[i] {
			if v, ok := stmttest.Integers([]any{a}); ok {
				a = v[0]
			}
			bound = append(bound, converted(a))
		}
		if sel.Limit.Parameter > 0 {
			if refusal := limitRefusal(columns, sel, bound); refusal != "" {
				which, _, _ := strings.Cut(refusal[strings.Index(refusal, "which is ")+len("which is "):], ", for LIMIT")
				count("LIMIT with an argument that is " + which + " " + kind(out))
				continue
			}
			count("LIMIT by an argument " + kind(out))
		}
		query := got.call == callQuery || got.call == callPrepareQuery || got.call == callStmtQuery
		if out.failed != "" || !query {
			continue
		}
		if sel.OrderBy != nil && len(out.rows) >= 2 {
			count("ORDER BY that gives two rows or more")
		}
		if !sel.Limited() {
			continue
		}
		var limit int64
		if sel.Limit.Parameter > 0 {
			limit, _ = bound[sel.Limit.Parameter-1].(int64)
		} else {
			limit = int64(sel.Limit.Value.(stmt.Integer))
		}
		whole, args := stmttest.SelectText(&stmt.Select{Table: sel.Table, Where: sel.Where, Subqueries: sel.Subqueries}, bound, "")
		var n int64
		is.NotError(tb, o.QueryRowContext(tb.Context(), "select count(*) from ("+whole+")", args...).Scan(&n))
		if limit > 0 && limit < n {
			count("LIMIT that cuts")
		}
	}
}

// reachChange counts what the update or the delete s with the arguments of each run, whose outcome of
// s4ql is got, reaches on its table on o, as [pair.reach] says, with the kind of each outcome as kind
// gives it, where "ok" of an exec is "ok, rows changed" or "ok, no row found" by the rows that it
// affected: for each run, the kind of the statement, with or without WHERE, and its outcome; one with
// arguments that s4ql refuses; and otherwise, for an update, each case of [stmttest.SetCases], and
// for a WHERE each case of the WHERE as a whole of [stmttest.WhereCases], of the select of its
// conditions as [stmttest.SelectOf] gives it, after the kind of the statement.
func (p *pair) reachChange(s stmt.Statement, runs [][]any, got outcome, o runner, kind func(outcome) string, count func(string)) {
	statement, name, where := "update", stmt.Name{}, []stmt.Condition(nil)
	switch s := s.(type) {
	case *stmt.Update:
		name, where = s.Table, s.Where
	case *stmt.Delete:
		statement, name, where = "delete", s.Table, s.Where
	}
	_, columns := p.columnsIn(o, name.Value)
	if columns == nil {
		return
	}
	for i, out := range got.runs() {
		if i >= len(runs) {
			break
		}
		outcome := kind(out)
		affected := out.affected
		switch {
		case !returns(s) || affected != "" || out.failed != "":
		case out.columns != "":
			// The rows of RETURNING of a query, one for each row that the statement changed.
			affected = fmt.Sprint(len(out.rows))
		case got.call == callQueryRow:
			// The first of them, of one row changed or more.
			affected = "1"
		}
		switch {
		case outcome != "ok" || affected == "":
		case affected == "0":
			outcome = "ok, no row found"
		default:
			outcome = "ok, rows changed"
		}
		count(statement + map[bool]string{true: " with WHERE ", false: " without WHERE "}[where != nil] + outcome)
		if len(runs[i]) != s.Parameters() || argumentRefusal(runs[i]) != "" {
			count(statement + " with arguments that s4ql refuses " + outcome)
			reachValueFirst(&stmt.Select{Where: where, Subqueries: subqueriesIn(s)}, nil, kind(out), -1, count)
			continue
		}
		var bound []any
		for _, a := range runs[i] {
			if v, ok := stmttest.Integers([]any{a}); ok {
				a = v[0]
			}
			bound = append(bound, converted(a))
		}
		sel, selArgs := stmttest.SelectOf(s, bound)
		rows := -1
		if affected != "" {
			rows = map[bool]int{true: 0, false: 1}[affected == "0"]
		}
		// Also for a SET of a column that the table does not have, as the engine's checks count it.
		reachValueFirst(sel, selArgs, kind(out), rows, count)
		missing := false
		if u, ok := s.(*stmt.Update); ok {
			for _, what := range stmttest.SetCases(columns, u, bound) {
				count(what + " " + outcome)
				missing = missing || strings.HasPrefix(what, "SET of a column that the table does not have")
			}
		}
		if where == nil || missing {
			continue
		}
		for _, what := range stmttest.WhereCases(columns, sel, selArgs) {
			if strings.HasPrefix(what, "WHERE ") {
				count(statement + " " + what + " " + outcome)
			}
		}
	}
}

// reachValueFirst counts with count each case of [stmttest.ValueFirstCases] of the select sel, whose
// arguments are args, or nil where they are not known, after the outcome of its run: the comparisons
// with the value on the left of slice 12, by their operator as the text writes it, and those with
// NULL. An outcome "ok" is "finds rows" or "finds no row" by the number of rows, the rows of a query
// or those that an update or a delete changed, and "ok" where it is -1, for a run whose rows the
// check does not count.
func reachValueFirst(sel *stmt.Select, args []any, outcome string, rows int, count func(string)) {
	if outcome == "ok" && rows >= 0 {
		outcome = map[bool]string{true: "finds no row", false: "finds rows"}[rows == 0]
	}
	for _, what := range stmttest.ValueFirstCases(sel, args) {
		count(what + " " + outcome)
	}
}

// reachesSelect reports whether s4ql takes the select sel with args to the instance: with one
// argument for each parameter, each one that s4ql supports.
func reachesSelect(sel *stmt.Select, args []any) bool {
	return len(args) == sel.Parameters() && argumentRefusal(args) == ""
}

// argumentKind of the argument a, for a count of what a test reaches, as [stmttest.ArgumentKind]
// names it, and for a kind of nil that database/sql hands the driver as nil, or as a nil []byte,
// "NULL as" or "nil []byte as" and its Go type.
func argumentKind(a any) string {
	if a == nil {
		return stmttest.ArgumentKind(a)
	}
	c := converted(a)
	if c == nil {
		return fmt.Sprintf("NULL as %T", a)
	}
	if b, ok := c.([]byte); ok && b == nil {
		if _, ok := a.([]byte); !ok {
			return fmt.Sprintf("nil []byte as %T", a)
		}
	}
	return stmttest.ArgumentKind(a)
}

// excused counts a text of the kind that the comparison excuses.
func (y tally) excused(kind string) {
	if y != nil {
		y["excused: "+kind]++
	}
}

// wrongArguments starts the message of s4ql's error of another number of arguments than the
// statement has parameters.
const wrongArguments = "s4ql: wrong number of arguments: "

// messageKind of an error's text: its message without the names, and for an evaluation error of
// arithmetic its kind, without the operator and the offset.
func messageKind(err string) string {
	for _, kind := range []string{
		"already exists", "no such table", "has no column named", "no such column", "values were supplied", " values for ", "duplicate column name",
		"too many columns on", "UNIQUE constraint failed", "NOT NULL constraint failed", "syntax error", "incomplete input", "unrecognized token", "no rows",
		"wrong number of arguments", "attempt to write a readonly database", "there is already a table named", "there is already an index named",
		"no such index", "row value misused", "sub-select returns",
		"s4ql: integer overflow", "s4ql: division by zero", "s4ql: real result that is not finite",
	} {
		if strings.Contains(err, kind) {
			return kind
		}
	}
	return err
}

// prepareKinds are the kinds of [messageKind] of SQLite's errors that depend on the database and that
// SQLite gives when it prepares a statement, where s4ql gives them when the statement runs.
var prepareKinds = []string{
	"no such table", "already exists", "has no column named", "no such column", "values were supplied", " values for ", "duplicate column name", "too many columns on",
	"there is already a table named", "there is already an index named", "no such index", "row value misused", "sub-select returns",
}

// The kinds of the texts that the comparison excuses, under which [tally.excused] counts them.
const (
	// excusedPrepare is a text whose error SQLite gives when it prepares the text, and s4ql when the
	// statement runs, because s4ql's Prepare checks only the text: the errors of [prepareKinds]
	// through Prepare.
	excusedPrepare = "SQLite's error at prepare, s4ql's at the run of the statement"
	// excusedStartedTable is the one difference from SQLite that the engine's tests accept too; see
	// [stmttest.ExistsAfterStart].
	excusedStartedTable = "already exists after the start of a table, accepted"
	// knownGapSuffix ends the kind of a text of a [stmttest.KnownGap], after the gap.
	knownGapSuffix = ", a known gap"
	// excusedArguments is a call with another number of arguments than its statement has
	// parameters, which s4ql refuses by its rule, where SQLite binds NULL to a parameter without an
	// argument and database/sql refuses an argument too many for mattn: it is checked against the
	// rule by [pair.checkArguments], and not run in SQLite.
	excusedArguments = "the wrong number of arguments, checked against s4ql's rule"
	// excusedArgument is a call with an argument that s4ql does not support, a named one or one of
	// a type or a value that no column holds, which SQLite binds: it is checked against s4ql's rule
	// by [pair.checkRefused], and not run in SQLite.
	excusedArgument = "an argument that s4ql does not support, checked against s4ql's rule"
	// excusedType is an insert or a select with a value, a literal or an argument, of another type
	// than its column, which SQLite converts, compares as it is, or refuses with an error of its own:
	// it is checked against s4ql's rule by [pair.checkRefused], and not run in SQLite.
	excusedType = "a value of another type than its column, checked against s4ql's rule"
	// excusedUpsert is an upsert that s4ql refuses by the rule of exact types, of its proposed row or of
	// DO UPDATE, also of its arithmetic and its text, where SQLite converts by affinity or orders by
	// type: it is checked against s4ql's rules by [pair.checkRefused], as [stmttest.UpsertRefusals]
	// gives them, and not run in SQLite.
	excusedUpsert = "an upsert refused by the rules of its types, checked against s4ql's rules"
	// excusedLevel is a begin with another isolation level than the default and serializable, which
	// s4ql refuses by its rule, where mattn ignores the level: it is checked against the rule by
	// [pair.begin], and nothing is begun in SQLite.
	excusedLevel = "an isolation level that s4ql does not support, checked against s4ql's rule"
	// excusedRowsOpen is a call in a transaction while the rows of a query of the transaction are
	// open, of a statement that s4ql would run, which s4ql refuses by its rule, where SQLite runs it:
	// it is checked against the rule by [pair.whileRowsOpen], and not run in SQLite.
	excusedRowsOpen = "a statement while the rows of the transaction are open, checked against s4ql's rule"
	// excusedTxOpen is a begin on a pinned connection whose transaction is open, which s4ql refuses
	// with an error of its own, and SQLite with `cannot start a transaction within a transaction`:
	// [pair.beginAgain] requires both.
	excusedTxOpen = "a begin on a connection whose transaction is open, refused by both with their own errors"
	// excusedOutside is [callStmtExec] or [callStmtQuery] through the pool, whose text mattn's Prepare
	// on the [sql.DB] refuses with an error of the names of the state outside the transaction, where
	// s4ql's Prepare checks only the text: the statement is compared with mattn's prepared in the
	// transaction, which database/sql prepares it in, by [pair.compare].
	excusedOutside = "a statement prepared outside the transaction, whose names only the transaction has, compared as prepared in it"
	// excusedQuoted is a select, an update, or a delete with a name in double quotes that the table does not have, or a
	// create index whose first column in the order of the text that the table does not have is in double
	// quotes, which mattn's SQLite reads as a string, and which [pair.checkQuoted] checks against the
	// pinned build's error instead.
	excusedQuoted = "a select, an update, a delete, a create index, or an insert with RETURNING with a name in double quotes that is no column, which mattn's SQLite reads as a string, checked against the pinned build's error"
	// excusedQuotedSubquery is a select, an update, or a delete with a scalar subquery, of slice 15,
	// and a name in double quotes that is no column of the tables that SQLite looks it up in, which
	// mattn's SQLite reads as a string, where the pinned build refuses the statement when it prepares
	// it: [pair.checkQuotedSubquery] requires an error of the names of s4ql, when the statement runs,
	// and does not run it in SQLite.
	excusedQuotedSubquery = "a statement with a subquery with a name in double quotes that is no column, which mattn's SQLite reads as a string, checked to fail on its names"
	// excusedCorrelated is a statement with a correlated scalar subquery, which s4ql refuses after
	// SQLite's errors of the names, and SQLite runs: it is checked against s4ql's rule by
	// [pair.checkRefused], as [pair.subqueryRefusal] gives it, and not run in SQLite.
	excusedCorrelated = "a correlated subquery, checked against s4ql's rule"
	// excusedSubqueryType is a statement with a scalar subquery whose column is of another declared
	// type than the column that it is compared with, INT and INTEGER being one, which SQLite compares by
	// affinity: it is checked against s4ql's rule as [pair.subqueryRefusal] gives it.
	excusedSubqueryType = "a subquery of a column of another type than the column that it is compared with, checked against s4ql's rule"
	// excusedSubqueryLimit is a statement with a scalar subquery whose argument of LIMIT is no integer
	// from 0, which SQLite reads as LIMIT (X <> 0): it is checked against s4ql's rule as
	// [pair.subqueryRefusal] gives it.
	excusedSubqueryLimit = "an argument of the LIMIT of a subquery that is no integer from 0, checked against s4ql's rule"
	// excusedWideSubqueries is an update or a delete with two scalar subqueries of * of more than one
	// column, for which SQLite gives `sub-select returns N columns - expected 1` for the one that its
	// plan codes first, and s4ql refuses at the second: it is checked against s4ql's rule as
	// [pair.subqueryRefusal] gives it.
	excusedWideSubqueries = "two subqueries of * of more than one column, checked against s4ql's rule"
	// excusedArithmetic is a statement with arithmetic, or a column compared with or set from a column,
	// that s4ql refuses by the rule of exact types, where SQLite converts by affinity or reads text and
	// blobs as numbers: it is checked against s4ql's rules by [pair.checkRefused], as
	// [stmttest.ArithmeticRefusals] gives them, and not run in SQLite.
	excusedArithmetic = "arithmetic refused by the rules of its types, checked against s4ql's rules"
	// excusedEvaluation is a statement with arithmetic that fails with s4ql's evaluation error, an
	// integer overflow, a division by zero, or a real result that is not finite, where SQLite gives a
	// real, NULL, or an infinity: it is checked by [pair.checkEvaluation] against SQLite's own result
	// of the operator that failed, as [stmttest.Justify] says, and not run in SQLite.
	excusedEvaluation = "an evaluation error of arithmetic, which SQLite's result of its operator justifies"
	// excusedSpaces is a text of several statements with a space that Go's TrimSpace removes and
	// SQLite reads as part of a token, at the start of the text after a statement or at the end of the
	// text, which mattn trims and runs, and s4ql refuses as a whole: it is checked against the error of
	// [stmt.Split], and not run in SQLite.
	excusedSpaces = "spaces that mattn trims and SQLite does not take for spaces"
	// excusedSeveralArguments is a text of several statements with more arguments than its statements
	// have parameters, which mattn drops, or with fewer than a statement needs after those before it,
	// for which mattn gives an error of its own: s4ql refuses the first before any statement runs and
	// the second when the statement runs, by its rule, which [pair.several] checks, and SQLite runs the
	// statements before.
	excusedSeveralArguments = "the wrong number of arguments of a text of several statements, checked against s4ql's rule"
	// excusedSeveralEnd is a text of several statements whose last statement ends in a semicolon
	// after which an empty statement or a comment follows, for which mattn's result is 0 rows affected,
	// its fix of its issue 963, where s4ql gives SQLite's count of changes after the text, which
	// [pair.several] requires of it.
	excusedSeveralEnd = "mattn's result of 0 rows affected after a text of several statements that ends in an empty statement, where s4ql gives SQLite's count"
	// excusedSeveralReturning is a text of several statements whose last statement has RETURNING, for
	// which mattn's result is SQLite's count of changes before that statement, as
	// [severalBeforeLast] measures it, since it steps the statement once, where s4ql gives SQLite's
	// count of changes after the text, which [pair.several] requires of it.
	excusedSeveralReturning = "mattn's result of the statement before a last statement with RETURNING, where s4ql gives SQLite's count"
)

// rowsOpen is the error of s4ql for a statement in a transaction while the rows of a query of the
// transaction are open.
const rowsOpen = "s4ql: not supported: a statement while the rows of a query of a transaction on its connection are open; close the rows first"

// txOpen is the error of s4ql for a begin on a connection whose transaction is open.
const txOpen = "s4ql: a transaction is open on the connection"

// pair of a database of s4ql and one of SQLite through mattn, which run the same texts, on a
// connection of each that is pinned, or through the pool of each.
type pair struct {
	tb testing.TB
	w  *world
	// prefix and cache of the instance of s4ql.
	prefix, cache string
	s4ql          *sql.DB
	sqlite        *sql.DB
	// s and o are where the texts run: s4ql's and SQLite's, a [sql.Conn] of each if pinned, and the
	// [sql.DB] of each otherwise.
	s, o   queryer
	pinned bool
	// empty is SQLite on an empty database, on which a text that s4ql does not support is prepared.
	empty *sql.DB
	// supports is, for the text of several statements of a generated call that runs through Exec,
	// whether each of its statements, as SQLite reads them, is one that s4ql supports, as
	// [stmttest.Call.Supports] says, which [pair.severalWalk] judges each statement by; nil otherwise.
	supports []bool
	// created are the names of the tables that a text created, as their values, so that the
	// comparison of the tables also asks for those that SQLite no longer has.
	created map[string]bool
	// found holds the tables, folded, that a create table if not exists of the sequence found, and
	// that no drop table dropped since, for [pair.reach].
	found map[string]bool
	y     tally
	// tx is the transaction that is open on both sides, or nil.
	tx *transaction
}

// transaction that is open on both sides of a pair, which the calls run in.
type transaction struct {
	// s and o are s4ql's and SQLite's.
	s, o     *sql.Tx
	readOnly bool
	// sRows and oRows are the rows that [callQueryOpen] left open, or nil, opened is the text of their
	// query, and sFirst and oFirst are the first row of each, which is read, as [firstRow] gives it.
	sRows, oRows           *sql.Rows
	opened, sFirst, oFirst string
	// openedArgs are the arguments of the query of the rows that are left open.
	openedArgs []any
	// oAll are SQLite's rows, each with its scan, of an insert, an update, or a delete with RETURNING
	// whose rows s4ql left open, as [pair.openWrite] says, which ran once on each side; oRows is nil
	// then.
	oAll []string
	// wrote is set once an insert succeeded in the transaction, failed once a call failed in it,
	// created and dropped once a create table or a drop table succeeded in it, and returned once an
	// insert, an update, or a delete with RETURNING, of slice 14, succeeded in it.
	wrote, failed, created, dropped, returned bool
}

// kind of the transaction, as the tally names it.
func (t *transaction) kind() string {
	if t.readOnly {
		return "read-only transaction"
	}
	return "write transaction"
}

// newPair on a new database of the world w, on a pinned connection of each side if pinned, with the
// outcomes counted in y.
func newPair(tb testing.TB, w *world, pinned bool, y tally) *pair {
	tb.Helper()
	p := &pair{tb: tb, w: w, prefix: fmt.Sprint("db", w.prefixes.Add(1)), cache: filepath.Join(tb.TempDir(), "cache"), pinned: pinned, created: map[string]bool{}, found: map[string]bool{}, y: y}
	p.s4ql = open(tb, w.options(p.prefix, p.cache))
	var err error
	// A file, so that every connection of the pool reaches the same database. Its transactions begin
	// with `begin immediate`, so that they take the place of the writer when they begin, as those of
	// s4ql do; mattn's BeginTx ignores ReadOnly, and [pair.begin] sets `pragma query_only` instead.
	// The name is no URI, whose path would end at a '#' of the name of the test.
	p.sqlite, err = sql.Open("sqlite3", filepath.Join(tb.TempDir(), "oracle.db")+"?_txlock=immediate")
	is.NotError(tb, err)
	tb.Cleanup(func() { is.NotError(tb, p.sqlite.Close()) })
	p.empty, err = sql.Open("sqlite3", ":memory:")
	is.NotError(tb, err)
	tb.Cleanup(func() { is.NotError(tb, p.empty.Close()) })
	p.s, p.o = p.s4ql, p.sqlite
	if pinned {
		p.pin()
	}
	return p
}

// pin a connection of each side, which the texts run on, until the test ends or the instance of
// s4ql closes.
func (p *pair) pin() {
	tb := p.tb
	tb.Helper()
	s, err := p.s4ql.Conn(tb.Context())
	is.NotError(tb, err)
	tb.Cleanup(func() { _ = s.Close() })
	o, err := p.sqlite.Conn(tb.Context())
	is.NotError(tb, err)
	tb.Cleanup(func() { _ = o.Close() })
	p.s, p.o = s, o
}

// step runs src through the call c on both sides, with the arguments of c in [fixedRuns], and compares
// what each gives, as [pair.stepRuns] does.
func (p *pair) step(src string, c call, supported bool) (outcome, string) {
	p.tb.Helper()
	return p.stepRuns(src, c, supported, fixedRuns(c))
}

// stepRuns runs src through the call c on both sides, with the arguments of each run, and compares
// what each gives. A text that s4ql does not support is not run in SQLite: it must not be a statement
// of slices 1 and 2, which supported says it is, and SQLite must prepare it on an empty database
// without a syntax error. A call that s4ql refuses for its arguments by its rule is not run in SQLite
// either, and must be refused as the rule says. stepRuns gives s4ql's outcome, and what differs, or
// "".
//
// A begin, a commit, and a roll back are those of [pair.begin] and [pair.end]. In a transaction, the
// calls run in it on both sides, but [callBeside], and while rows of the transaction are open, as
// [pair.whileRowsOpen] says.
func (p *pair) stepRuns(src string, c call, supported bool, runs [][]any) (outcome, string) {
	tb := p.tb
	tb.Helper()
	switch c {
	case callBegin, callBeginReadOnly:
		return p.begin(src, c)
	case callCommit, callRollback:
		return p.end(c)
	}
	if c == callExec || c == callExecArgument {
		if got, failure, ok := p.several(src, c, supported, runs); ok {
			return got, failure
		}
	}
	s, o, scope := p.where(c, src)
	if p.tx != nil && p.tx.sRows != nil {
		return p.whileRowsOpen(src, c, supported, runs, s, o, scope)
	}
	if st, _ := stmt.Check(src); c == callQueryOpen && p.tx != nil && scope == p.tx.kind() && returns(st) &&
		!slices.ContainsFunc(runs, func(args []any) bool { return !reaches(src, args) }) {
		return p.openWrite(src, supported, runs, s, o, scope)
	}
	got := p.runS4QL(s, o, c, src, runs)
	p.y.add(scope, src, got, runs)
	p.reach(src, runs, got, o)
	failure := p.compare(src, c, supported, runs, got, o)
	if failure == "" {
		failure = p.compareChanged(src, got, s, o)
	}
	if failure == "" && p.tx != nil && scope == p.tx.kind() {
		p.note(src, got)
		if c == callQueryOpen && got.failed == "" && got.columns != "" {
			failure = p.openRows(src, runs[0], got)
		}
	}
	return got, failure
}

// where the call c of src runs: s4ql's and SQLite's, and the scope that the tally counts it in. It is
// in the open transaction, if there is one, but for [callBeside].
func (p *pair) where(c call, src string) (runner, runner, string) {
	t := p.tx
	switch {
	case t == nil:
		return p.s, p.o, ""
	case c != callBeside:
		return inTx{Tx: t.s, outer: p.s}, inTx{Tx: t.o, outer: p.o}, t.kind()
	case p.pinned:
		// The statements of a sql.Conn run in its transaction.
		return p.s, p.o, t.kind()
	}
	if s, _ := stmt.Check(src); s != nil {
		if _, ok := s.(*stmt.Select); ok {
			return p.s, p.o, "beside a " + t.kind()
		}
	}
	return inTx{Tx: t.s, outer: p.s}, inTx{Tx: t.o, outer: p.o}, t.kind()
}

// note what the outcome got of src in the open transaction did, for the tally: a write, a failure,
// a create table, a drop table, and a query that reads rows after a write of the transaction.
func (p *pair) note(src string, got outcome) {
	t := p.tx
	kind := statementKind(src)
	st, _ := stmt.Check(src)
	for _, o := range got.runs() {
		t.returned = t.returned || o.failed == "" && returns(st)
		switch {
		case o.failed != "":
			t.failed = true
		case strings.HasPrefix(kind, "insert"):
			t.wrote = true
		case kind == "createtable":
			t.created = true
		case kind == "droptable":
			t.dropped = true
		case kind == "select" && t.wrote && len(o.rows) > 0 && p.y != nil:
			p.y[t.kind()+": select ok with rows after a write of the transaction"]++
		}
	}
}

// openRows runs the query src, with args, again in the open transaction on both sides, reads the
// first row of each, and leaves the rows open for the next call. compared is s4ql's outcome of the
// query, which was compared with SQLite's before, so its rows are the rows that SQLite gives. The
// two first rows need not be the same row, since each side reads in an order of its own: both sides
// must have a first row or neither, and s4ql's, with its scan, must be one of the rows of compared.
// [pair.closeRows] reads the rest. s4ql's query may fail instead, before its first row or with it,
// as [pair.reopened] says, and is then not run in SQLite, and no rows are left open.
func (p *pair) openRows(src string, args []any, compared outcome) string {
	tb := p.tb
	tb.Helper()
	t := p.tx
	var err error
	t.sRows, err = t.s.QueryContext(tb.Context(), src, args...)
	if err != nil {
		t.sRows = nil
		return p.reopened(src, args, err, "before their first row")
	}
	got, found, err := nextRow(tb, t.sRows)
	if err != nil {
		// database/sql closes rows whose Next found no row.
		is.NotError(tb, t.sRows.Close())
		t.sRows = nil
		return p.reopened(src, args, err, "with their first row")
	}
	t.oRows, err = t.o.QueryContext(tb.Context(), firstStatement(src), args...)
	is.NotError(tb, err)
	want, wanted := firstRow(tb, t.oRows)
	t.opened, t.sFirst, t.oFirst = src, got, want
	if !found || !wanted {
		// database/sql closes rows whose Next found no row.
		is.NotError(tb, t.sRows.Close())
		is.NotError(tb, t.oRows.Close())
		t.sRows, t.oRows = nil, nil
		if found != wanted {
			return fmt.Sprintf("%q: s4ql's rows that are left open have a first row: %v, %v, and SQLite's: %v, %v", src, found, got, wanted, want)
		}
		return ""
	}
	t.openedArgs = args
	if sel := orderedSelect(src); sel == nil && !slices.Contains(compared.pairs(), got) {
		// The first row of a select with ORDER BY or LIMIT is checked with the rest, in its place, as
		// [pair.closeRows] does: one with LIMIT and without ORDER BY can give other rows each time.
		return fmt.Sprintf("%q: the first row of s4ql's rows that are left open is\n%v\nwhich is none of the rows that the query gave:\n%v", src, got, compared.full())
	}
	if p.y != nil {
		p.y[t.kind()+": rows left open"]++
	}
	return ""
}

// openWrite is [pair.stepRuns] of src, an insert, an update, or a delete with RETURNING, through
// [callQueryOpen] in the open transaction, with the arguments of its run, on s and o: it runs once on
// each side, since a second run would change the database again, where [pair.openRows] runs a
// select again. s4ql's rows of RETURNING are left open after their first row, as the rows of a
// select are, and SQLite's are read to their end: s4ql's first row, with its scan, must be one of
// SQLite's rows, with the same columns and types, and [pair.closeRows] compares the rest and the
// table after the statement. A statement that fails or gives no row on s4ql's side is compared as
// through [callQuery].
func (p *pair) openWrite(src string, supported bool, runs [][]any, s, o runner, scope string) (outcome, string) {
	tb := p.tb
	tb.Helper()
	t := p.tx
	got := outcome{call: callQueryOpen}
	rows, err := s.QueryContext(tb.Context(), src, runs[0]...)
	var first string
	if err != nil {
		got.failed, got.err = "query", s4qlText(tb, err)
	} else {
		columns, err := rows.Columns()
		is.NotError(tb, err)
		types, err := rows.ColumnTypes()
		is.NotError(tb, err)
		var names []string
		for _, ct := range types {
			names = append(names, ct.DatabaseTypeName())
		}
		got.columns, got.types = strings.Join(columns, ","), strings.Join(names, ",")
		if rows.Next() {
			row, scan, null := readRow(tb, rows, names)
			got.rows, got.scans, got.null, first = []string{row}, []string{scan}, null, pairText(row, scan)
		} else {
			// database/sql closes rows whose Next found no row.
			if err := rows.Err(); err != nil {
				got.failed, got.err = "rows", s4qlText(tb, err)
				got.columns, got.types = "", ""
			}
			is.NotError(tb, rows.Close())
			rows = nil
		}
	}
	p.y.add(scope, src, got, runs)
	p.reach(src, runs, got, o)
	if rows == nil {
		failure := p.compare(src, callQueryOpen, supported, runs, got, o)
		if failure == "" {
			failure = p.compareChanged(src, got, s, o)
		}
		if failure == "" {
			p.note(src, got)
		}
		return got, failure
	}
	p.y.compared()
	want := run(tb, o, callQueryOpen, firstStatement(src), runs, mattnText)
	all := want.pairs()
	t.sRows, t.oRows, t.opened, t.openedArgs, t.sFirst, t.oFirst = rows, nil, src, runs[0], first, first
	p.note(src, got)
	t.oAll = all
	if want.failed != "" || want.columns != got.columns || want.types != got.types || !slices.Contains(all, first) {
		return got, fmt.Sprintf("%q with the arguments %#v: the first row of s4ql's rows of RETURNING that are left open is\n%v\nof the columns %q of the types %q, which is none of the rows that SQLite gives:\n%v", src, runs, first, got.columns, got.types, want.full())
	}
	if p.y != nil {
		p.y[t.kind()+": rows of RETURNING left open"]++
	}
	return got, ""
}

// firstRow of rows, with its scan into sql.Null* values and into pointers, as [pairText] gives them
// and as [outcome.pairs] has them, and whether the rows have one.
func firstRow(tb testing.TB, rows *sql.Rows) (string, bool) {
	tb.Helper()
	row, found, err := nextRow(tb, rows)
	is.NotError(tb, err)
	return row, found
}

// nextRow of rows, as [firstRow] gives it, or the error of their Next.
func nextRow(tb testing.TB, rows *sql.Rows) (string, bool, error) {
	tb.Helper()
	if !rows.Next() {
		return "", false, rows.Err()
	}
	return pairText(typedRow(tb, rows)), true, nil
}

// reopened checks the error err of s4ql's query src with the arguments args, which [pair.openRows]
// ran again in the open transaction, before or with its first row, or of the rest of its rows, which
// [pair.closeRows] read, as where says. The run that was compared with SQLite gave rows, but the
// scans of the run again may read in other directions, which s4ql draws from its host: a select with
// LIMIT, or with a subquery, which reads up to its first row, may then meet a row whose arithmetic
// fails, where the compared run did not. So the error must be an evaluation error of such a select
// with arithmetic that SQLite's own result of its operator justifies, as [pair.justify] says, on the
// state of the transaction, which nothing changed since; any other error fails, and so does one of a
// select that reads every row in every direction. It counts each such error by where, and gives what
// is wrong, or "".
func (p *pair) reopened(src string, args []any, err error, where string) string {
	tb := p.tb
	tb.Helper()
	text := s4qlText(tb, err)
	s, _ := stmt.Check(src)
	sel, ok := s.(*stmt.Select)
	if !ok || !stmttest.HasExpression(s) || !sel.Limited() && sel.Subqueries == nil {
		return fmt.Sprintf("%q with the arguments %#v: s4ql's query run again in the transaction, whose rows are left open, fails with %q", src, args, text)
	}
	if why := p.justify(src, s, args, text, inTx{Tx: p.tx.o, outer: p.o}); why != "" {
		return fmt.Sprintf("%q with the arguments %#v: s4ql's query run again in the transaction, whose rows are left open, fails with %q, which %v", src, args, text, why)
	}
	if p.y != nil {
		p.y["reached: rows left open that fail "+where+" with an evaluation error justified by SQLite's result of its operator"]++
	}
	return ""
}

// partOf reports whether the rows got, each with its scan, that s4ql's query src with the arguments
// args gave before it failed, are rows of the select without its LIMIT on o, each no more often than
// that select gives it, which SQLite gives in its order: what differs, or "".
func (p *pair) partOf(src string, args []any, got []string, o runner) string {
	tb := p.tb
	tb.Helper()
	s, _ := stmt.Check(src)
	sel, ok := s.(*stmt.Select)
	if !ok {
		return fmt.Sprintf("%q: s4ql's rows that were left open fail, and it is no select", src)
	}
	all, bound := stmttest.SelectText(sel, args, "")
	want := run(tb, o, callQuery, all, [][]any{bound}, mattnText)
	if want.failed != "" {
		tb.Fatalf("%q: SQLite gives %v", all, want)
	}
	left := map[string]int{}
	for _, row := range want.pairs() {
		left[row]++
	}
	for _, row := range got {
		if left[row] == 0 {
			return fmt.Sprintf("%q with the arguments %#v: s4ql's rows that were left open, the first and then the rest until they failed, are\n%v\nwhose row\n%v\nis none of SQLite's rows of the select without its LIMIT, or one more time than they hold it:\n%v", src, args, strings.Join(got, " "), row, want.full())
		}
		left[row]--
	}
	return ""
}

// typedRow that the Next of rows found: its text and the text of its scan, as [readRow] gives them.
func typedRow(tb testing.TB, rows *sql.Rows) (row, scan string) {
	tb.Helper()
	types, err := rows.ColumnTypes()
	is.NotError(tb, err)
	names := make([]string, len(types))
	for i, ct := range types {
		names[i] = ct.DatabaseTypeName()
	}
	row, scan, _ = readRow(tb, rows, names)
	return row, scan
}

// closeRows that [callQueryOpen] left open in the open transaction, on both sides, after reading the
// rest of each. The first row of each side and its rest, each row with its scan, must be the same
// rows the same number of times, in any order, or, for a select with ORDER BY or LIMIT, as
// [pair.orderedDiffers] says: the calls between the first row and the rest changed nothing, since
// s4ql refuses a statement of the transaction while its rows are open. The rest of s4ql's rows of a
// select may fail instead, as [pair.reopened] says, and are then not compared. It gives what
// differs, or "", also where no rows are open.
func (p *pair) closeRows() string {
	tb := p.tb
	tb.Helper()
	t := p.tx
	if t == nil || t.sRows == nil {
		return ""
	}
	got, want := []string{t.sFirst}, []string{t.oFirst}
	for t.sRows.Next() {
		got = append(got, pairText(typedRow(tb, t.sRows)))
	}
	sErr := t.sRows.Err()
	write := t.oRows == nil
	if write {
		want = t.oAll
	} else {
		for t.oRows.Next() {
			want = append(want, pairText(typedRow(tb, t.oRows)))
		}
		is.NotError(tb, t.oRows.Err())
		is.NotError(tb, t.oRows.Close())
	}
	is.NotError(tb, t.sRows.Close())
	t.sRows, t.oRows, t.oAll = nil, nil, nil
	if sErr != nil && !write {
		// The rows of a select, which s4ql reads as they are read: those before the error must be
		// SQLite's.
		o := inTx{Tx: t.o, outer: p.o}
		if failure := p.partOf(t.opened, t.openedArgs, got, o); failure != "" {
			return failure
		}
		return p.reopened(t.opened, t.openedArgs, sErr, "in their rest")
	}
	if sErr != nil {
		return fmt.Sprintf("%q: the rest of s4ql's rows that were left open fails with %v", t.opened, sErr)
	}
	if write {
		// The rows of a statement with RETURNING, which is no select, and then its table, which s4ql
		// refused to read while the rows were open.
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			return fmt.Sprintf("%q: s4ql's rows of RETURNING that were left open, the first and then the rest, are\n%v\nand SQLite's\n%v", t.opened, strings.Join(got, " "), strings.Join(want, " "))
		}
		return p.compareChanged(t.opened, outcome{}, inTx{Tx: t.s, outer: p.s}, inTx{Tx: t.o, outer: p.o})
	}
	if sel := orderedSelect(t.opened); sel != nil {
		if failure := p.orderedDiffers(sel, t.openedArgs, got, want, true, inTx{Tx: t.o, outer: p.o}); failure != "" {
			return fmt.Sprintf("%q: s4ql's rows that were left open, the first and then the rest: %v", t.opened, failure)
		}
		return ""
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return fmt.Sprintf("%q: s4ql's rows that were left open, the first and then the rest, are\n%v\nand SQLite's\n%v", t.opened, strings.Join(got, " "), strings.Join(want, " "))
	}
	return ""
}

// whileRowsOpen is [pair.stepRuns] of src through the call c, with the arguments of each run, on s
// and o, while rows of the open transaction are open, which it reads to their end, compares, and
// closes after the call, as [pair.closeRows] does. Where s4ql would run the statement in every run,
// it must refuse each with [rowsOpen] when the statement runs, and the call is not run in SQLite,
// which would run it. The open rows come after the checks of the text and of the arguments, so every
// other call is compared as without them; so is a query beside the transaction through the pool,
// which runs on another connection.
func (p *pair) whileRowsOpen(src string, c call, supported bool, runs [][]any, s, o runner, scope string) (got outcome, failure string) {
	tb := p.tb
	tb.Helper()
	defer func() {
		if rest := p.closeRows(); failure == "" {
			failure = rest
		}
	}()
	got = p.runS4QL(s, o, c, src, runs)
	p.y.add(scope, src, got, runs)
	runsAll := !slices.ContainsFunc(runs, func(args []any) bool { return !reaches(src, args) })
	if !runsAll || scope != p.tx.kind() {
		return got, p.compare(src, c, supported, runs, got, o)
	}
	if len(got.runs()) != len(runs) {
		return got, fmt.Sprintf("%q with %v runs while rows are open: s4ql gives %v", src, len(runs), got)
	}
	for _, r := range got.runs() {
		if r.failed == "" || r.failed == "prepare" || r.err != rowsOpen {
			return got, fmt.Sprintf("%q with the arguments %#v while rows of the transaction are open: s4ql gives %v, where the rule gives %q", src, runs, got, rowsOpen)
		}
	}
	p.y.excused(excusedRowsOpen)
	return got, ""
}

// begin a transaction on both sides, by the call c, with the isolation level that src names, as
// [levelName] gives it. A level that s4ql does not support must be refused as its rule says, and
// nothing is begun on either side. A read-only transaction of SQLite runs under `pragma query_only`,
// which [pair.end] resets. On a pinned connection whose transaction is open, the begin is
// [pair.beginAgain]; through the pool, the open transaction commits first, because s4ql's second
// write transaction would wait for the first.
func (p *pair) begin(src string, c call) (outcome, string) {
	tb := p.tb
	tb.Helper()
	ctx := tb.Context()
	i := slices.IndexFunc(levels, func(l sql.IsolationLevel) bool { return levelName(l) == src })
	if i < 0 {
		tb.Fatalf("no isolation level is named %q", src)
	}
	level := levels[i]
	opts := &sql.TxOptions{Isolation: level, ReadOnly: c == callBeginReadOnly}
	got := outcome{call: c}
	if p.tx != nil && p.pinned {
		return p.beginAgain(c, opts)
	}
	if p.tx != nil {
		if _, failure := p.end(callCommit); failure != "" {
			return got, failure
		}
	}
	if p.y != nil {
		p.y["call "+string(c)]++
	}

	sTx, err := p.s.BeginTx(ctx, opts)
	if level != sql.LevelDefault && level != sql.LevelSerializable {
		// mattn ignores the level, and is not asked.
		if want := "s4ql: not supported: the isolation level " + level.String(); err == nil || err.Error() != want || !errors.Is(err, s4ql.ErrNotSupported) {
			if err == nil {
				_ = sTx.Rollback()
			}
			return got, fmt.Sprintf("BeginTx with %+v: s4ql gives %v, where the rule gives %q", opts, err, want)
		}
		got.failed, got.err = "begin", err.Error()
		p.y.excused(excusedLevel)
		return got, ""
	}
	if err != nil {
		return got, fmt.Sprintf("BeginTx with %+v: s4ql gives %v, and SQLite begins", opts, err)
	}
	oTx, err := p.o.BeginTx(ctx, opts)
	is.NotError(tb, err)
	if opts.ReadOnly {
		_, err := oTx.ExecContext(ctx, "pragma query_only = 1")
		is.NotError(tb, err)
	}
	p.tx = &transaction{s: sTx, o: oTx, readOnly: opts.ReadOnly}
	if p.y != nil {
		p.y[p.tx.kind()+" begin ok"]++
		p.y["begin ok"]++
	}
	return got, ""
}

// beginAgain is [pair.begin] on a pinned connection whose transaction is open, with opts: s4ql must
// refuse an isolation level that it does not support as without the transaction, and otherwise give
// [txOpen], which matches none of its sentinels, and SQLite must refuse it as a transaction within a
// transaction. The open transaction stays open on both sides, and rows that it left open are read to
// their end, compared, and closed first, as [pair.closeRows] does.
func (p *pair) beginAgain(c call, opts *sql.TxOptions) (outcome, string) {
	tb := p.tb
	tb.Helper()
	ctx := tb.Context()
	got := outcome{call: c}
	if failure := p.closeRows(); failure != "" {
		return got, failure
	}
	if p.y != nil {
		p.y["call "+string(c)+" in a transaction"]++
	}
	_, err := p.s.BeginTx(ctx, opts)
	if err == nil {
		return got, fmt.Sprintf("BeginTx with %+v in a transaction: s4ql begins", opts)
	}
	got.failed, got.err = "begin", err.Error()
	if opts.Isolation != sql.LevelDefault && opts.Isolation != sql.LevelSerializable {
		if want := "s4ql: not supported: the isolation level " + opts.Isolation.String(); err.Error() != want || !errors.Is(err, s4ql.ErrNotSupported) {
			return got, fmt.Sprintf("BeginTx with %+v in a transaction: s4ql gives %v, where the rule gives %q", opts, err, want)
		}
		p.y.excused(excusedLevel)
		return got, ""
	}
	if err.Error() != txOpen {
		return got, fmt.Sprintf("BeginTx with %+v in a transaction: s4ql gives %v, where the rule gives %q", opts, err, txOpen)
	}
	noSentinel(tb, err)
	if _, ok := errors.AsType[*s4ql.Error](err); ok {
		return got, fmt.Sprintf("BeginTx with %+v in a transaction: s4ql gives SQLite's error %v", opts, got.err)
	}
	// Under `pragma query_only`, which a read-only transaction of SQLite runs under, `begin immediate`
	// is refused as a write first.
	want := "cannot start a transaction within a transaction"
	if p.tx.readOnly {
		want = "attempt to write a readonly database"
	}
	if _, err := p.o.BeginTx(ctx, opts); err == nil || err.Error() != want {
		return got, fmt.Sprintf("BeginTx with %+v in a transaction: SQLite gives %v, and the test expects %q", opts, err, want)
	}
	p.y.excused(excusedTxOpen)
	return got, ""
}

// end the open transaction on both sides, by the call c, a commit or a roll back, and compare what
// each gives, and then every table. Rows that the transaction left open stay open on s4ql's side,
// which database/sql closes before the end, so their rest is not read: their first row was checked
// when they were opened, as [pair.openRows] says. Without a transaction it does nothing.
func (p *pair) end(c call) (outcome, string) {
	tb := p.tb
	tb.Helper()
	got, want := outcome{call: c}, outcome{call: c}
	t := p.tx
	if t == nil {
		return got, ""
	}
	if t.sRows != nil && t.oRows == nil {
		// The rows of RETURNING of a write, of which only the first was compared: the rest, and the
		// table after the statement, before the end of the transaction.
		if failure := p.closeRows(); failure != "" {
			return got, failure
		}
	}
	p.tx = nil
	open := t.sRows != nil
	if open && t.oRows != nil {
		is.NotError(tb, t.oRows.Close())
	}
	if t.readOnly {
		_, err := t.o.ExecContext(tb.Context(), "pragma query_only = 0")
		is.NotError(tb, err)
	}
	step, sEnd, oEnd := "commit", t.s.Commit, t.o.Commit
	if c == callRollback {
		step, sEnd, oEnd = "rollback", t.s.Rollback, t.o.Rollback
	}
	if err := sEnd(); err != nil {
		got.failed, got.err = step, s4qlText(tb, err)
	}
	if err := oEnd(); err != nil {
		want.failed, want.err = step, mattnText(tb, err)
	}
	if open {
		is.NotError(tb, t.sRows.Close())
	}
	if got.String() != want.String() {
		return got, fmt.Sprintf("the %v of the %v: s4ql gives\n%v\nand SQLite\n%v", step, t.kind(), got, want)
	}
	if y := p.y; y != nil {
		y["call "+string(c)]++
		name := t.kind() + " " + step
		if got.failed == "" {
			name += " ok"
		} else {
			name += " " + messageKind(got.err)
		}
		y[name]++
		where := " through the pool"
		if p.pinned {
			where = " on a pinned connection"
		}
		y[name+where]++
		for _, with := range []struct {
			ok   bool
			what string
		}{{t.failed, "after a failed call"}, {t.wrote, "after an insert"}, {t.created, "after a create table"}, {t.dropped, "after a drop table"}, {open, "while rows are open"}, {t.returned, "after a statement with RETURNING"}} {
			if with.ok {
				y[name+" "+with.what]++
			}
		}
	}
	return got, p.compareTables()
}

// finish the open transaction by a commit, if there is one, and compare.
func (p *pair) finish() string {
	p.tb.Helper()
	_, failure := p.end(callCommit)
	return failure
}

// compare s4ql's outcome got of src through the call c, with the arguments of each run, with
// SQLite's on o, as [pair.stepRuns] describes.
func (p *pair) compare(src string, c call, supported bool, runs [][]any, got outcome, o runner) string {
	tb := p.tb
	tb.Helper()
	if s, err := stmt.Check(src); err == nil && s != nil && (stmttest.HasExpression(s) || upsertOf(s) != nil) && slices.ContainsFunc(got.runs(), evaluated) {
		// Also where another run is refused.
		return p.checkEvaluation(src, c, s, runs, got, o)
	}
	if got.notSupported() {
		return p.checkNotSupported(src, c, supported, runs, got, o)
	}
	if strings.HasPrefix(got.err, wrongArguments) {
		return p.checkArguments(src, runs, got)
	}
	// Every other outcome is compared with SQLite's. It is the syntax error of a text, which SQLite
	// gives before it binds the arguments, so that it runs there without them, or the outcome of a
	// statement that s4ql ran, which must have had an argument that s4ql supports for each
	// parameter in each run, and which runs there with them.
	s, err := stmt.Check(src)
	byText := err != nil || s == nil
	mattnRuns := runs
	if byText {
		mattnRuns = make([][]any, len(runs))
	} else {
		for _, args := range runs {
			if !reaches(src, args) {
				return fmt.Sprintf("%q with the arguments %#v: s4ql gives %v, and must refuse the arguments", src, args, got)
			}
		}
	}
	if !byText {
		for i, r := range got.runs() {
			if i < len(runs) {
				if failure := p.dueRefusal(src, s, runs[i], r, o); failure != "" {
					return failure
				}
			}
		}
	}

	if subqueriesIn(s) != nil {
		if failure, ok := p.checkQuotedSubquery(src, s, got, o); ok {
			return failure
		}
	} else if failure, ok := p.checkQuoted(src, s, got, o); ok {
		return failure
	}

	first := firstStatement(src)
	p.y.compared()
	var want outcome
	_, isSelect := s.(*stmt.Select)
	if _, ok := o.(inTx); ok && c == callStmtExec && isSelect {
		// mattn's Exec of a query steps it once and does not reset it, and the statement that
		// [sql.Tx.StmtContext] gives for one prepared outside the transaction stays open until the
		// transaction ends, so that a later drop table of the query's table in the transaction would
		// fail with SQLite's `database table is locked`. On SQLite's side the query is prepared in the
		// transaction instead, and closed after its run. Its names are those of the transaction either
		// way, as below.
		want = run(tb, o, callPrepareExec, first, mattnRuns, mattnText)
		want.call = c
	} else if returns(s) {
		want = execByQuery(tb, o, c, first, mattnRuns)
	} else {
		want = run(tb, o, c, first, mattnRuns, mattnText)
	}
	if t, ok := o.(inTx); ok && !p.pinned && (c == callStmtExec && !isSelect || c == callStmtQuery) && want.failed == "prepare" &&
		slices.Contains(prepareKinds, messageKind(want.err)) {
		// Through the pool, mattn's Prepare on the sql.DB ran outside the transaction, on another
		// connection, and failed on the names of the state there, before database/sql prepared the
		// statement again in the transaction. s4ql's Prepare checks only the text.
		want = run(tb, t.Tx, c, first, mattnRuns, mattnText)
		p.y.excused(excusedOutside)
	}
	if !p.pinned {
		// Through the pool, the count of changes is that of the connection that database/sql takes,
		// so it is compared only for a statement that sets it: an insert, an update, and a delete.
		switch s.(type) {
		case *stmt.Insert, *stmt.Update, *stmt.Delete:
		default:
			got.affected, want.affected = "", ""
		}
	}
	if sel, ok := s.(*stmt.Select); ok && (sel.OrderBy != nil || sel.Limited()) && got.failed == "" && want.failed == "" &&
		got.columns == want.columns && got.types == want.types && len(got.rows) == len(want.rows) && len(got.rows) > 0 {
		// The rows of a select with ORDER BY are in an order, and those of one with LIMIT are a part of
		// the rows of the select without it, so they are compared as [pair.orderedDiffers] says, and the
		// rest of the outcome as for every other text. A QueryRow gives the first row of the select.
		gotRows, wantRows := got.rows, want.rows
		if len(got.scans) > 0 {
			gotRows, wantRows = got.pairs(), want.pairs()
		}
		if failure := p.orderedDiffers(sel, mattnRuns[0], gotRows, wantRows, len(got.scans) > 0, o); failure != "" {
			return fmt.Sprintf("%q with the arguments %#v: %v", src, runs, failure)
		}
		want.rows, want.scans = got.rows, got.scans
	}
	if c == callQueryRow && got.failed == "" && want.failed == "" && !slices.Equal(got.rows, want.rows) {
		// A QueryRow gives the row that comes first, and each side reads in an order of its own, so
		// the two rows need not be the same row: s4ql's must be one of the rows that SQLite gives for
		// the query when all of them are read.
		all := want
		if !returns(s) {
			all = run(tb, o, callQuery, first, mattnRuns, mattnText)
		}
		if all.failed != "" || len(got.rows) != 1 || !slices.Contains(all.rows, got.rows[0]) {
			return fmt.Sprintf("%q with the arguments %#v: s4ql's QueryRow gives\n%v\nwhose row is none of the rows that SQLite gives:\n%v", src, runs, got, all)
		}
		want.rows = got.rows
	}
	if got.full() == want.full() {
		if ct, ok := s.(*stmt.CreateTable); ok && got.failed == "" {
			p.created[ct.Table.Value] = true
		}
		return ""
	}

	switch {
	case (c == callPrepareExec || c == callPrepareExecRuns || c == callStmtExec) && got.failed == "exec" || (c == callPrepareQuery || c == callStmtQuery) && got.failed == "query":
		// SQLite gives the error when it prepares the text, and s4ql in each run of the statement.
		if want.failed == "prepare" && slices.Contains(prepareKinds, messageKind(got.err)) &&
			!slices.ContainsFunc(got.runs(), func(o outcome) bool { return o.failed != got.failed || o.err != want.err }) {
			p.y.excused(excusedPrepare)
			return ""
		}
	case p.startedTable(src, c, mattnRuns, got, want):
		p.y.excused(excusedStartedTable)
		return ""
	}
	if gap := knownGap(src, got, want); gap != "" {
		p.y.excused(gap + knownGapSuffix)
		return ""
	}
	return fmt.Sprintf("%q with the arguments %#v: s4ql gives\n%v\nand SQLite\n%v", src, runs, got.full(), want.full())
}

// returningOf the insert, the update, or the delete s: its RETURNING, or the zero Returning for
// another statement.
func returningOf(s stmt.Statement) stmt.Returning {
	switch s := s.(type) {
	case *stmt.Insert:
		return s.Returning
	case *stmt.Update:
		return s.Returning
	case *stmt.Delete:
		return s.Returning
	}
	return stmt.Returning{}
}

// returns reports whether s is an insert, an update, or a delete with RETURNING.
func returns(s stmt.Statement) bool {
	switch s := s.(type) {
	case *stmt.Insert:
		return s.Returning.Present()
	case *stmt.Update:
		return s.Returning.Present()
	case *stmt.Delete:
		return s.Returning.Present()
	}
	return false
}

// execByQuery is [run] on mattn's side of src, a statement with RETURNING, through the call c, with
// the arguments of each run. A QueryRow runs it once, with the arguments of the first run, through
// Query, and its outcome holds each row that the QueryRow may give. mattn's Exec steps such a
// statement once and gives the count of changes
// that SQLite has then, which is that of the statement before: SQLite runs every change of the
// statement in its first step, and counts them once the statement has run to its end. So a call of
// Exec runs the statement through Query, as the query call of c, and reads its rows, and its count of
// changes is the number of its rows, one for each row that the statement changed, which is the count
// that SQLite gives after it. Every other call is [run].
func execByQuery(tb testing.TB, o runner, c call, src string, runs [][]any) outcome {
	tb.Helper()
	if c == callQueryRow {
		// The rows of the statement, which runs once, as the outcome of a QueryRow, which gives every
		// error at Scan, and no rows as [sql.ErrNoRows], with each row that SQLite gives: the row of
		// s4ql's QueryRow must be one of them.
		r := run(tb, o, callQuery, src, runs[:min(len(runs), 1)], mattnText)
		r.call, r.columns, r.types, r.scans = c, "", "", nil
		switch {
		case r.failed != "":
			r.failed = "scan"
		case len(r.rows) == 0:
			r.failed, r.err = "scan", "no rows"
		}
		if r.failed != "" {
			r.rows, r.null = nil, false
		}
		return r
	}
	query := map[call]call{callExec: callQuery, callExecArgument: callQueryArgument, callPrepareExec: callPrepareQuery, callStmtExec: callStmtQuery, callPrepareExecRuns: callPrepareQuery}[c]
	if query == "" {
		return run(tb, o, c, src, runs, mattnText)
	}
	each := runs
	if c != callPrepareExecRuns && len(runs) > 1 {
		each = runs[:1]
	}
	var first outcome
	for i, args := range each {
		r := run(tb, o, query, src, [][]any{args}, mattnText)
		r.call = ""
		if r.failed == "prepare" {
			// A prepare that fails fails the call once, as in [run].
			r.call = c
			return r
		}
		if r.failed == "query" {
			r.failed = "exec"
		}
		if r.failed == "" {
			r.affected, r.lastID = fmt.Sprint(len(r.rows)), "0"
		}
		r.columns, r.types, r.rows, r.scans, r.null = "", "", nil, nil, false
		if i == 0 {
			r.call = c
			first = r
		} else {
			first.next = append(first.next, r)
		}
	}
	return first
}

// orderedSelect is the select of the text src if it is one with ORDER BY or LIMIT, and nil otherwise.
func orderedSelect(src string) *stmt.Select {
	s, _ := stmt.Check(src)
	if sel, ok := s.(*stmt.Select); ok && (sel.OrderBy != nil || sel.Limited()) {
		return sel
	}
	return nil
}

// orderedDiffers says how the rows got that s4ql gives for the select sel with ORDER BY or LIMIT,
// with the arguments args, in the order in which they came, differ from the rows want that SQLite
// gives for it on o, or "" if they do not. Each row is as [rowText] gives it, or, if scans, with its
// scan as [outcome.pairs] has it. Rows that tie under ORDER BY come in an order that is not
// specified, and a LIMIT without ORDER BY may give any of the rows, so the rows are compared with
// those of the select without its LIMIT, which SQLite gives in its order, each with its dense rank
// under the ORDER BY, from a second query, whose ranks at each place do not depend on the order in
// which SQLite gives rows that tie:
//
//   - The number of rows is SQLite's.
//   - With ORDER BY, the row of s4ql at each place must be a row of the rank that SQLite has at that
//     place: where no rows tie, that is the same row at the same place, and the rows of a rank are
//     compared as a multiset, of which a LIMIT that cuts a rank leaves any part.
//   - Without ORDER BY, the rows are rows of the select without its LIMIT, each no more often than
//     that select gives it.
func (p *pair) orderedDiffers(sel *stmt.Select, args []any, got, want []string, scans bool, o runner) string {
	tb := p.tb
	tb.Helper()
	if len(got) != len(want) {
		return fmt.Sprintf("s4ql gives %v rows, %q, and SQLite %v, %q", len(got), got, len(want), want)
	}
	var by []string
	for _, c := range sel.OrderBy {
		term := stmttest.Quote(c.Name.Value)
		if c.Descending {
			term += " desc"
		}
		by = append(by, term)
	}
	orderBy := ""
	if by != nil {
		orderBy = " order by " + strings.Join(by, ", ")
	}
	src, bound := stmttest.SelectText(sel, args, orderBy)
	all := run(tb, o, callQuery, src, [][]any{bound}, mattnText)
	if all.failed != "" {
		tb.Fatalf("%q: SQLite gives %v", src, all)
	}
	rows := all.rows
	if scans {
		rows = all.pairs()
	}
	ranks := make([]string, len(rows))
	if by != nil {
		text, _ := stmttest.SelectText(&stmt.Select{Table: sel.Table, Where: sel.Where, Subqueries: sel.Subqueries}, args, orderBy)
		rest, ok := strings.CutPrefix(text, "select *")
		if !ok {
			tb.Fatalf("%q does not start with select *", text)
		}
		rankSrc := "select dense_rank() over (" + strings.TrimPrefix(orderBy, " ") + ")" + rest
		ranked := run(tb, o, callQuery, rankSrc, [][]any{bound}, mattnText)
		if ranked.failed != "" || len(ranked.rows) != len(rows) {
			tb.Fatalf("%q: SQLite gives %v, for %v rows", rankSrc, ranked, len(rows))
		}
		ranks = ranked.rows
	}
	if len(got) > len(rows) {
		return fmt.Sprintf("s4ql gives %v rows, and the select without its LIMIT has %v in SQLite", len(got), len(rows))
	}
	for start := 0; start < len(got); {
		end := start
		left := map[string]int{}
		for end < len(rows) && ranks[end] == ranks[start] {
			left[rows[end]]++
			end++
		}
		for n := start; n < min(end, len(got)); n++ {
			if left[got[n]]--; left[got[n]] < 0 {
				return fmt.Sprintf("s4ql gives the row %v at the place %v, where SQLite has the rows %q; s4ql's rows are %q", got[n], n, rows[start:end], got)
			}
		}
		start = end
	}
	return ""
}

// checkQuoted s4ql's outcome got of src, which holds the statement s, run where SQLite runs on o,
// when s is a select whose name that SQLite's `no such column` names, as [stmttest.MissingColumn]
// gives it, is in double quotes, in the select list or in a condition, or, after those, in the
// RETURNING of an insert, an update, or a delete, of slice 14. mattn's SQLite reads such a
// name as a string, where the pinned build of SQLite, which the engine's tests compare with, gives
// `no such column`, because it takes double quotes for names only (DQS=0). So such a select is not
// run in mattn's SQLite: each run must fail when the statement runs, since Prepare checks only the
// text, with the pinned build's error, `no such column: "x" - should this be a string literal in
// single-quotes?`. The same holds for a name of a condition of an update, whose names of SET the
// table has, and of a delete. It reports whether s is such a statement, and gives what differs, or "".
func (p *pair) checkQuoted(src string, s stmt.Statement, got outcome, o runner) (string, bool) {
	p.tb.Helper()
	var sel *stmt.Select
	switch s := s.(type) {
	case *stmt.Select:
		sel = s
	case *stmt.Update:
		// A name of SET that the table does not have gets SQLite's error first, without the question, in
		// both builds, but for a name of the value of an assignment, which SQLite looks up before the
		// column of the assignment, in double quotes.
		_, columns := p.columnsIn(o, s.Table.Value)
		if columns == nil {
			return "", false
		}
		for _, a := range s.Set {
			// SQLite walks the names of the value as stmttest.MissingName does, also inside a call.
			if name, missing := stmttest.MissingName(columns, a.Value.Expression); missing {
				return p.quoted(src, got, name)
			}
			if stmttest.ColumnOf(columns, a.Column) < 0 {
				return "", false
			}
		}
		sel = &stmt.Select{Table: s.Table, Where: s.Where}
	case *stmt.Delete:
		sel = &stmt.Select{Table: s.Table, Where: s.Where}
	case *stmt.CreateIndex:
		sel = &stmt.Select{Table: s.Table}
	case *stmt.Insert:
		// The names of the list of an insert and the number of its values get SQLite's errors first, in
		// both builds, and then the names of its RETURNING.
		_, columns := p.columnsIn(o, s.Table.Value)
		if _, ok := stmttest.Positions(columns, s); !ok || !s.Returning.Present() {
			return "", false
		}
		sel = &stmt.Select{Table: s.Table}
	default:
		return "", false
	}
	_, columns := p.columnsIn(o, sel.Table.Value)
	if columns == nil {
		return "", false
	}
	name, missing := stmttest.MissingColumn(columns, sel)
	if r := returningOf(s); !missing {
		// The names of RETURNING, of slice 14, come after every other name of the statement, in the
		// order of the text, as the names of a select list do.
		if at := slices.IndexFunc(r.Columns, func(n stmt.Name) bool { return stmttest.ColumnOf(columns, n) < 0 }); at >= 0 {
			name, missing = r.Columns[at], true
		}
	}
	if ci, ok := s.(*stmt.CreateIndex); ok {
		// The first column of the index in the order of the text that the table does not have: the
		// build of SQLite with DQS on makes an index on a string literal of it, if it is in double
		// quotes, where the pinned build gives its error.
		at := slices.IndexFunc(ci.Columns, func(c stmt.IndexColumn) bool { return stmttest.ColumnOf(columns, c.Name) < 0 })
		if missing = at >= 0; missing {
			name = ci.Columns[at].Name
		}
	}
	if !missing {
		return "", false
	}
	want := fmt.Sprintf(`1/1: no such column: "%v" - should this be a string literal in single-quotes?`, name.Value)
	if _, ok := s.(*stmt.CreateIndex); ok && !slices.ContainsFunc(got.runs(), func(r outcome) bool { return r.err == want }) {
		// An error of the name of the index or of the table comes first in both builds, and the
		// comparison with mattn's SQLite checks it.
		return "", false
	}
	return p.quoted(src, got, name)
}

// quoted is [pair.checkQuoted] of the text src, whose outcome of s4ql is got, and whose first name
// that its table does not have is name: excused, if name is in double quotes and s4ql gives the
// pinned build's error of it in each run, failed if it is in double quotes and s4ql gives another,
// and left to the comparison with mattn's SQLite otherwise.
func (p *pair) quoted(src string, got outcome, name stmt.Name) (string, bool) {
	if name.Raw[0] != '"' {
		return "", false
	}
	want := fmt.Sprintf(`1/1: no such column: "%v" - should this be a string literal in single-quotes?`, name.Value)
	for _, r := range got.runs() {
		if r.failed == "" || r.failed == "prepare" || r.err != want {
			return fmt.Sprintf("%q: s4ql gives %v, and the pinned build of SQLite %q when the statement runs", src, got, want), true
		}
	}
	p.y.excused(excusedQuoted)
	return "", true
}

// checkNotSupported is [pair.stepRuns] of a text that s4ql answers with "not supported", in got, of
// the call c that runs on o in SQLite.
func (p *pair) checkNotSupported(src string, c call, supported bool, runs [][]any, got outcome, o runner) string {
	tb := p.tb
	tb.Helper()
	s, err := stmt.Check(src)
	if err == nil && s != nil {
		return p.checkRefused(src, s, runs, got, o)
	}
	// The text alone decides "not supported", before the arguments, with the "not supported" of its
	// check, and Prepare checks the text.
	want := "s4ql: not supported: a text that holds no statement"
	if err != nil {
		want = err.Error()
	}
	if got.err != want {
		return fmt.Sprintf("%q: s4ql gives %v, where its check of the text gives %v", src, got, want)
	}
	if (c == callPrepareExec || c == callPrepareQuery || c == callPrepareExecRuns || c == callStmtExec || c == callStmtQuery) && got.failed != "prepare" {
		return fmt.Sprintf("%q: s4ql gives %v, after the statement was prepared", src, got)
	}
	if supported {
		return fmt.Sprintf("%q: s4ql does not support a statement of slice 1: %v", src, got)
	}
	// A text with a NUL byte or a byte that is not UTF-8 is not supported, whatever else it holds.
	if !utf8.ValidString(src) || strings.ContainsRune(src, 0) {
		return ""
	}
	ps, err := p.empty.PrepareContext(tb.Context(), src)
	if err != nil {
		if m := mattnText(tb, err); sqloracle.IsSyntaxMessage(m[strings.Index(m, ": ")+2:]) {
			return fmt.Sprintf("%q: s4ql gives %v, and SQLite %v", src, got, m)
		}
		return ""
	}
	is.NotError(tb, ps.Close())
	return ""
}

// checkRefused is [pair.checkNotSupported] of the text src, which holds the statement s that s4ql
// supports, so that its "not supported" is of an argument or of the type of a value. Each run must
// have one argument for each parameter, and fail when the statement runs, since Prepare checks only
// the text, with the "not supported" of its first argument that s4ql does not support, as
// [argumentRefusal] gives it: an error of s4ql's own, where SQLite binds every type and value, and
// mattn takes a name. If it has none, it must fail with the "not supported" of a value of another
// type than its column, as [pair.typeRefusal] gives it, where SQLite converts the value or refuses it
// with an error of its own, on the tables that SQLite has on o, where the call runs; or, for a
// statement with arithmetic or a column alone, as [stmttest.HasExpression] reports it, with one of
// the refusals of [pair.arithmeticRefusals].
func (p *pair) checkRefused(src string, s stmt.Statement, runs [][]any, got outcome, o runner) string {
	if len(got.runs()) != len(runs) {
		return fmt.Sprintf("%q with %v runs: s4ql gives %v", src, len(runs), got)
	}
	kind := excusedType
	for i, r := range got.runs() {
		args := runs[i]
		want := argumentRefusal(args)
		switch {
		case want != "":
			kind = excusedArgument
		case len(args) == s.Parameters() && upsertOf(s) != nil:
			// The engine gives the refusals of an upsert in an order of its own too.
			refusals := p.upsertRefusals(src, upsertOf(s), boundArgs(args), o)
			want = fmt.Sprintf("one of %q", refusals)
			if slices.Contains(refusals, r.err) {
				want = r.err
			}
			if kind == excusedType {
				kind = excusedUpsert
			}
		case len(args) == s.Parameters() && stmttest.HasExpression(s):
			// The engine gives the refusals of arithmetic in an order of its own, so the rule is that the
			// refusal is one of them.
			refusals := p.arithmeticRefusals(src, s, boundArgs(args), o)
			want = fmt.Sprintf("one of %q", refusals)
			if slices.Contains(refusals, r.err) {
				want = r.err
			}
			if kind == excusedType {
				kind = excusedArithmetic
			}
		case len(args) == s.Parameters():
			want = p.typeRefusal(src, s, args, o)
			if kind == excusedType {
				// A refusal of an argument of an earlier run keeps its excuse.
				kind = refusalKind(s, want)
			}
		}
		if len(args) != s.Parameters() || want == "" || r.failed == "prepare" || r.failed == "" || r.err != want {
			return fmt.Sprintf("%q with the arguments %#v: s4ql gives %v, where the rule gives %q", src, args, got, want)
		}
	}
	p.y.excused(kind)
	return ""
}

// argumentRefusal is the "not supported" of the first argument of args that s4ql does not support,
// in order, its name before its type: one with a name, an unsigned integer above the largest int64,
// a real that is not a number or is infinite, text that is not valid UTF-8 or holds the character
// NUL, a []byte that is nil, and one of a type that no column holds, by its type as database/sql
// converts it. It is "" if s4ql supports every argument, nil for NULL among them.
func argumentRefusal(args []any) string {
	for i, a := range args {
		if n, ok := a.(sql.NamedArg); ok {
			return fmt.Sprintf("s4ql: not supported: the named argument %q", n.Name)
		}
		if _, ok := stmttest.Integers([]any{a}); ok {
			continue
		}
		var which string
		switch v := converted(a).(type) {
		case uint64:
			return fmt.Sprintf("s4ql: not supported: argument %v of type uint64, whose value does not fit a signed 64-bit integer", i+1)
		case nil:
			// NULL, which a column that takes NULL holds, and SQLite's NOT NULL refuses for any other.
		case float64:
			switch {
			case math.IsNaN(v):
				which = "is a real that is not a number"
			case math.IsInf(v, 0):
				which = "is an infinite real"
			}
		case string:
			switch {
			case !utf8.ValidString(v):
				which = "is text that is not valid UTF-8"
			case strings.ContainsRune(v, 0):
				which = "is text that holds the character NUL"
			}
		case []byte:
			if v == nil {
				return fmt.Sprintf("s4ql: not supported: argument %v of type []byte, which is nil", i+1)
			}
		default:
			return fmt.Sprintf("s4ql: not supported: argument %v of type %T", i+1, v)
		}
		if which != "" {
			return fmt.Sprintf("s4ql: not supported: argument %v, which %v", i+1, which)
		}
	}
	return ""
}

// converted argument a, as database/sql converts a float32 to a float64 for the driver, a nil
// pointer to nil, also one to a [driver.Valuer], a [driver.Valuer] to its value, and another pointer
// to what it points to. Every other argument of the tests is as database/sql hands it, or is refused
// by its type.
func converted(a any) any {
	if f, ok := a.(float32); ok {
		return float64(f)
	}
	rv := reflect.ValueOf(a)
	if rv.Kind() == reflect.Pointer && rv.IsNil() {
		return nil
	}
	if v, ok := a.(driver.Valuer); ok {
		if value, err := v.Value(); err == nil {
			return converted(value)
		}
		return a
	}
	if rv.Kind() == reflect.Pointer {
		return converted(rv.Elem().Interface())
	}
	return a
}

// reaches reports whether s4ql takes src with args to the instance: the text holds a statement that
// s4ql supports, with one argument for each parameter, each one that s4ql supports.
func reaches(src string, args []any) bool {
	s, err := stmt.Check(src)
	return err == nil && s != nil && len(args) == s.Parameters() && argumentRefusal(args) == ""
}

// typeRefusal is the "not supported" of the statement s of the text src, with the arguments args,
// which s4ql supports, when it is an insert or a select with WHERE that SQLite prepares on o, where
// the call runs, and its value of a column, a literal or an argument, is of another type than the
// column of the table on o. For an insert, outside a read-only transaction, with a value for each
// column, it is for the first such column in the order of the table, as [stmttest.TypeRule] gives
// it; for a select, in any transaction, for the first such condition in the order of the text, as
// [stmttest.ConditionRule] gives it. It is "" otherwise, also for an insert that leaves a column
// without a value, which gets SQLite's `NOT NULL constraint failed` first.
func (p *pair) typeRefusal(src string, s stmt.Statement, args []any, o runner) string {
	tb := p.tb
	tb.Helper()
	var table stmt.Name
	switch s := s.(type) {
	case *stmt.Insert:
		if p.tx != nil && p.tx.readOnly {
			return ""
		}
		table = s.Table
	case *stmt.Select:
		if s.Where == nil && s.Limit.Parameter == 0 {
			return ""
		}
		table = s.Table
	case *stmt.Update:
		table = s.Table
	case *stmt.Delete:
		table = s.Table
	default:
		return ""
	}
	bound := boundArgs(args)
	if subqueriesIn(s) != nil {
		return p.subqueryRefusal(src, s, bound, o)
	}
	ps, err := o.PrepareContext(tb.Context(), firstStatement(src))
	if err != nil {
		return ""
	}
	is.NotError(tb, ps.Close())
	name, columns := p.columnsIn(o, table.Value)
	switch s := s.(type) {
	case *stmt.Select:
		if rule := stmttest.ConditionRule(name, columns, s, bound); rule != "" {
			return rule
		}
		return limitRefusal(columns, s, bound)
	case *stmt.Delete:
		return stmttest.DeleteRule(name, columns, s, bound)
	case *stmt.Update:
		if p.tx != nil && p.tx.readOnly {
			// A read-only transaction refuses the update before it looks at a value of SET.
			return stmttest.WhereRule(name, columns, s.Where, bound)
		}
		return stmttest.UpdateRule(name, columns, s, bound, func() bool {
			q, args := stmttest.FoundQuery(s.Table, s.Where, s.Subqueries, bound)
			rows, err := o.QueryContext(tb.Context(), q, args...)
			is.NotError(tb, err)
			defer func() { is.NotError(tb, rows.Close()) }()
			if rows.Next() {
				return true
			}
			is.NotError(tb, rows.Err())
			return false
		})
	}
	return stmttest.TypeRule(name, columns, s.(*stmt.Insert), bound)
}

// conditionNames of the condition c: the column that it tests, or the columns of the expression that
// it tests, and those of its value.
func conditionNames(c stmt.Condition) []stmt.Name {
	var names []stmt.Name
	if !c.Expression.Present() {
		names = append(names, c.Column)
	}
	return append(append(names, expressionNames(c.Expression)...), expressionNames(c.Value.Expression)...)
}

// expressionNames of the columns of the expression e, in the order of the text.
func expressionNames(e stmt.Expression) []stmt.Name {
	if e.Kind == stmt.ExprColumn {
		return []stmt.Name{e.Column}
	}
	var names []stmt.Name
	for _, o := range e.Operands {
		names = append(names, expressionNames(o)...)
	}
	return names
}

// boundArgs are the arguments args as SQLite binds them: an integer of any Go type as an int64, and
// each other as [converted] gives it.
func boundArgs(args []any) []any {
	var bound []any
	for _, a := range args {
		if v, ok := stmttest.Integers([]any{a}); ok {
			a = v[0]
		}
		bound = append(bound, converted(a))
	}
	return bound
}

// arithmeticRefusals of the statement s with arithmetic or a column alone, as [stmttest.HasExpression]
// reports it, of the text src, with the arguments args as SQLite binds them, on o, where the call
// runs: where SQLite prepares the text, every "not supported" that s4ql's rules give for it, as
// [stmttest.ArithmeticRefusals] gives them, but those of SET in a read-only transaction, which
// refuses the write first; and where it does not, the refusal of two subqueries of * of more than
// one column, as [pair.subqueryRefusal] gives it, or none, for SQLite's own error.
func (p *pair) arithmeticRefusals(src string, s stmt.Statement, args []any, o runner) []string {
	tb := p.tb
	tb.Helper()
	ps, err := o.PrepareContext(tb.Context(), firstStatement(src))
	if err != nil {
		if subqueriesIn(s) != nil {
			if rule := p.subqueryRefusal(src, s, args, o); rule != "" {
				return []string{rule}
			}
		}
		return nil
	}
	is.NotError(tb, ps.Close())
	table, _ := tableWhere(s)
	name, columns := p.columnsIn(o, table.Value)
	return stmttest.ArithmeticRefusals(name, columns, s, args, p.tx != nil && p.tx.readOnly, func(name string) (string, []stmttest.TableColumn) { return p.columnsIn(o, name) })
}

// upsertOf the statement s: s, if it is an insert with ON CONFLICT, or nil.
func upsertOf(s stmt.Statement) *stmt.Insert {
	if i, ok := s.(*stmt.Insert); ok && i.Conflict.Present() {
		return i
	}
	return nil
}

// dueRefusal of the run r of the statement s of the text src, with the arguments args, on o, where
// the call runs: where s is an upsert and r gives no error, what is wrong if one of s4ql's refusals
// was due, as [pair.upsertRefusals] gives them, or "". SQLite's outcome cannot show such a refusal
// where it converts a value that s4ql refuses.
func (p *pair) dueRefusal(src string, s stmt.Statement, args []any, r outcome, o runner) string {
	u := upsertOf(s)
	if u == nil || r.failed != "" {
		return ""
	}
	if refusals := p.upsertRefusals(src, u, boundArgs(args), o); len(refusals) > 0 {
		return fmt.Sprintf("%q with the arguments %#v: s4ql gives %v, and the rules refuse it with one of %q", src, args, r, refusals)
	}
	return ""
}

// upsertRefusals of the upsert s of the text src, with the arguments args as SQLite binds them, on
// o, where the call runs: where SQLite prepares the text, every "not supported" that s4ql's rules
// give for it, as [stmttest.UpsertRefusals] gives them, but those of its types in a read-only
// transaction, which refuses the write first; and where it does not, none, for SQLite's own error.
func (p *pair) upsertRefusals(src string, s *stmt.Insert, args []any, o runner) []string {
	tb := p.tb
	tb.Helper()
	ps, err := o.PrepareContext(tb.Context(), firstStatement(src))
	if err != nil {
		return nil
	}
	is.NotError(tb, ps.Close())
	name, columns := p.columnsIn(o, s.Table.Value)
	return stmttest.UpsertRefusals(name, columns, s, args, p.tx != nil && p.tx.readOnly)
}

// evaluated reports whether the run r failed with s4ql's evaluation error of arithmetic.
func evaluated(r outcome) bool {
	_, ok := stmttest.EvaluationOf(r.err)
	return r.failed != "" && ok
}

// checkEvaluation of the call c of the text src, which holds the statement s with arithmetic, with
// the arguments of each run, whose outcome of s4ql is got, a run of which failed with an evaluation
// error, on o, where the call runs, as SQLite has the tables before the call. A run that failed so
// changed nothing, and must be justified by SQLite's own result of the operator that failed, as
// [stmttest.Justify] says, on the state of o, and not in a read-only transaction for an update or a
// delete, which computes nothing; it is not run in SQLite. Every other run, of a prepared statement
// run several times, is refused as [pair.checkRefused] says, since the engine gives its refusals and
// its evaluation errors in an order of its own and a run with other arguments may meet a refusal
// first, and is not run in SQLite either; or runs in SQLite, in its order, and must give what s4ql
// gave. It gives what differs, or "".
func (p *pair) checkEvaluation(src string, c call, s stmt.Statement, runs [][]any, got outcome, o runner) string {
	tb := p.tb
	tb.Helper()
	if len(got.runs()) != len(runs) {
		return fmt.Sprintf("%q with %v runs: s4ql gives %v", src, len(runs), got)
	}
	_, isSelect := s.(*stmt.Select)
	var outs []outcome
	for i, r := range got.runs() {
		r.call, r.next = "", nil
		if !evaluated(r) {
			if c != callPrepareExecRuns {
				return fmt.Sprintf("%q with the arguments %#v: s4ql gives %v", src, runs, got)
			}
			if r.notSupported() {
				if failure := p.checkRefused(src, s, runs[i:i+1], r, o); failure != "" {
					return failure
				}
				outs = append(outs, r)
				continue
			}
			if !reaches(src, runs[i]) {
				return fmt.Sprintf("%q with the arguments %#v: s4ql gives %v, and must refuse the arguments", src, runs[i], got)
			}
			if failure := p.dueRefusal(src, s, runs[i], r, o); failure != "" {
				return failure
			}
			var w outcome
			if res, err := o.ExecContext(tb.Context(), firstStatement(src), runs[i]...); err != nil {
				w.failed, w.err = "exec", mattnText(tb, err)
			} else {
				w = result(tb, w, res)
			}
			outs = append(outs, w)
			continue
		}
		if !reaches(src, runs[i]) {
			return fmt.Sprintf("%q with the arguments %#v: s4ql gives %v, and must refuse the arguments", src, runs[i], got)
		}
		if p.tx != nil && p.tx.readOnly && !isSelect {
			return fmt.Sprintf("%q with the arguments %#v: s4ql gives %v in a read-only transaction, which computes nothing for a write", src, runs[i], r)
		}
		if why := p.justify(src, s, runs[i], r.err, o); why != "" {
			return fmt.Sprintf("%q with the arguments %#v: s4ql gives %q, which %v", src, runs[i], r.err, why)
		}
		if p.y != nil {
			p.y["reached: evaluation error justified by SQLite's result of its operator: "+messageKind(r.err)]++
		}
		outs = append(outs, r)
	}
	want := outs[0]
	want.call, want.next = c, outs[1:]
	if want.full() != got.full() {
		return fmt.Sprintf("%q with the arguments %#v: s4ql gives\n%v\nand SQLite, but for the runs that failed with an evaluation error,\n%v", src, runs, got.full(), want.full())
	}
	p.y.excused(excusedEvaluation)
	return ""
}

// justify the error text of s4ql's run of the statement s with arithmetic, with the arguments args,
// on o, where the statement runs, as SQLite has the tables that the statement saw: what is wrong
// with it, or "" for an evaluation error that SQLite's own result of its operator justifies, as
// [stmttest.Justify] says.
func (p *pair) justify(src string, s stmt.Statement, args []any, text string, o runner) string {
	tb := p.tb
	tb.Helper()
	e, ok := stmttest.EvaluationOf(text)
	if !ok {
		return "is no evaluation error"
	}
	rows := func(q string, args []any) bool {
		rs, err := o.QueryContext(tb.Context(), q, args...)
		is.NotError(tb, err)
		defer func() { is.NotError(tb, rs.Close()) }()
		found := rs.Next()
		is.NotError(tb, rs.Err())
		return found
	}
	if u := upsertOf(s); u != nil {
		return p.justifyUpsert(src, u, args, e, o)
	}
	return stmttest.Justify(s, boundArgs(args), e.Kind, e.Op, e.Span.Start, nil, rows)
}

// justifyUpsert is [pair.justify] of the upsert s of the text src, with the arguments args, whose evaluation error is
// e, on o, as SQLite has the tables before the call: SQLite's result of the operator of e alone, on
// its operands, must show the quirk of its kind, for its one value where the operator has no column,
// and otherwise on the row of the conflict that the documented rule evaluates it on, as
// [stmttest.UpsertQuirkQuery] gives the query. The queries of a row write, so they run in a
// transaction of their own, or in a savepoint of the open one, that is rolled back.
func (p *pair) justifyUpsert(src string, s *stmt.Insert, args []any, e *expr.Error, o runner) string {
	tb := p.tb
	tb.Helper()
	ctx := tb.Context()
	bound := boundArgs(args)
	_, columns := p.columnsIn(o, s.Table.Value)
	var key string
	for _, c := range columns {
		if c.PrimaryKey {
			key = c.Name
		}
	}
	// probe runs q on o, rolled back, and gives whether it gave a row, how many it changed, and
	// SQLite's error, or "".
	probe := func(q string, args []any) (bool, int64, string) {
		r := o
		var end func()
		switch o := o.(type) {
		case queryer:
			tx, err := o.BeginTx(ctx, nil)
			is.NotError(tb, err)
			r, end = tx, func() { is.NotError(tb, tx.Rollback()) }
		default:
			_, err := o.ExecContext(ctx, "savepoint s4ql_quirk")
			is.NotError(tb, err)
			end = func() {
				for _, q := range []string{"rollback to s4ql_quirk", "release s4ql_quirk"} {
					_, err := o.ExecContext(ctx, q)
					is.NotError(tb, err)
				}
			}
		}
		defer end()
		rows, err := r.QueryContext(ctx, q, args...)
		if err != nil {
			return false, 0, mattnText(tb, err)
		}
		found := rows.Next()
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			is.NotError(tb, rows.Close())
			return false, 0, mattnText(tb, err)
		}
		is.NotError(tb, rows.Close())
		var changes int64
		is.NotError(tb, r.QueryRowContext(ctx, "select changes()").Scan(&changes))
		return found, changes, ""
	}
	// The rule evaluates an expression with a column only on a conflict, where SQLite takes the
	// proposed row, which the engine checks before it computes such an expression.
	q, qArgs := stmttest.UpsertConflictQuery(s, src, key, bound)
	_, inserted, failed := probe(q, qArgs)
	conflict := failed == "" && inserted == 0
	at := false
	for _, site := range stmttest.UpsertSites(s.Conflict) {
		for _, n := range stmttest.Operators(site.Root) {
			if n.Span.Start != e.Span.Start || n.Kind.String() != e.Op.String() {
				continue
			}
			at = true
			if !conflict && site.Root.HasColumn() {
				continue
			}
			q, qArgs := stmttest.UpsertQuirkQuery(s, src, key, site, n, e.Kind, bound)
			found, _, failed := probe(q, qArgs)
			if failed != "" {
				tb.Fatalf("%q: SQLite gives %v", q, failed)
			}
			if found {
				return ""
			}
		}
	}
	if !at {
		return "is at no operator of the statement"
	}
	return "SQLite's result of its operator justifies it for no value that the rule evaluates"
}

// subqueriesIn the select, the update, or the delete s: its scalar subqueries, of slice 15, in the
// order of the text, or nil.
func subqueriesIn(s stmt.Statement) []stmt.Select {
	switch s := s.(type) {
	case *stmt.Select:
		return s.Subqueries
	case *stmt.Update:
		return s.Subqueries
	case *stmt.Delete:
		return s.Subqueries
	}
	return nil
}

// refusalKind of the "not supported" want that [pair.typeRefusal] gives for the statement s: the
// kind of the excuse of its class, one of a scalar subquery, or [excusedType].
func refusalKind(s stmt.Statement, want string) string {
	switch {
	case strings.HasPrefix(want, "s4ql: not supported: correlated subquery "):
		return excusedCorrelated
	case strings.HasPrefix(want, "s4ql: not supported: second subquery of * of more than one column "):
		return excusedWideSubqueries
	case strings.HasPrefix(want, "s4ql: not supported: subquery "):
		return excusedSubqueryType
	case strings.Contains(want, ", for LIMIT, which takes an integer from 0"):
		for _, q := range subqueriesIn(s) {
			if q.Limit.Parameter > 0 && strings.HasPrefix(want, fmt.Sprintf("s4ql: not supported: argument %v, ", q.Limit.Parameter)) {
				return excusedSubqueryLimit
			}
		}
	}
	return excusedType
}

// scopeName is a name of a statement that SQLite looks up in the columns outer of the table of the
// statement, or, for a name of a scalar subquery, in the columns inner of its table and then in
// outer, where it takes a name that inner does not have, a correlated name.
type scopeName struct {
	name         stmt.Name
	inner, outer []stmttest.TableColumn
}

// missing reports whether no table of the scope of the name has it.
func (n scopeName) missing() bool {
	if n.inner != nil && stmttest.ColumnOf(n.inner, n.name) >= 0 {
		return false
	}
	return stmttest.ColumnOf(n.outer, n.name) < 0
}

// namesOf the select, the update, or the delete s with scalar subqueries, on the tables of o: every
// name that SQLite looks up, of the statement on its table and of each subquery whose table o has on
// its own, not in SQLite's order, and whether every table is there. It gives nil for a statement whose
// own table o does not have.
func (p *pair) namesOf(s stmt.Statement, o runner) ([]scopeName, bool) {
	table, where := tableWhere(s)
	_, outer := p.columnsIn(o, table.Value)
	if outer == nil {
		return nil, false
	}
	var names []stmt.Name
	switch s := s.(type) {
	case *stmt.Select:
		names = append(names, s.Columns...)
		for _, c := range s.OrderBy {
			names = append(names, c.Name)
		}
	case *stmt.Update:
		for _, a := range s.Set {
			names = append(names, expressionNames(a.Value.Expression)...)
			names = append(names, a.Column)
		}
		names = append(names, s.Returning.Columns...)
	case *stmt.Delete:
		names = append(names, s.Returning.Columns...)
	}
	for _, c := range where {
		names = append(names, conditionNames(c)...)
	}
	var scoped []scopeName
	for _, n := range names {
		scoped = append(scoped, scopeName{name: n, outer: outer})
	}
	tables := true
	for _, q := range subqueriesIn(s) {
		_, inner := p.columnsIn(o, q.Table.Value)
		if inner == nil {
			tables = false
			continue
		}
		names := slices.Clone(q.Columns)
		for _, c := range q.Where {
			names = append(names, conditionNames(c)...)
		}
		for _, c := range q.OrderBy {
			names = append(names, c.Name)
		}
		for _, n := range names {
			scoped = append(scoped, scopeName{name: n, inner: inner, outer: outer})
		}
	}
	return scoped, tables
}

// checkQuotedSubquery is [pair.checkQuoted] of a select, an update, or a delete s with scalar
// subqueries, of slice 15, whose names SQLite looks up in an order that differs between a select and
// a change, and around each subquery: if a name in double quotes is no column of the tables of its
// scope, as [scopeName.missing] says, the pinned build refuses the statement when it prepares it, for
// that name or for an error of the names before it, where mattn's SQLite may read the name as a
// string and run the statement, or give the error of a name after it, such as `no such table` of a
// subquery of an update or a delete, whose table SQLite finds where its walk over the WHERE reaches
// it. Each run of s4ql must then fail when the statement runs, since
// Prepare checks only the text, with an error of the names: `no such column` of a name that the
// tables of its scope do not have, `no such table`, `row value misused`, or `sub-select returns`.
// Which of them is the first is the engine's differential test's to check, against the pinned build.
// It reports whether s is such a statement, and gives what differs, or "".
func (p *pair) checkQuotedSubquery(src string, s stmt.Statement, got outcome, o runner) (string, bool) {
	p.tb.Helper()
	names, _ := p.namesOf(s, o)
	if !slices.ContainsFunc(names, func(n scopeName) bool { return n.name.Raw[0] == '"' && n.missing() }) {
		return "", false
	}
	for _, r := range got.runs() {
		ofNames := strings.HasPrefix(r.err, "1/1: no such table: ") || r.err == "1/1: row value misused" || strings.HasPrefix(r.err, "1/1: sub-select returns ")
		for _, n := range names {
			ofNames = ofNames || n.missing() && (r.err == "1/1: no such column: "+n.name.Value || r.err == fmt.Sprintf(`1/1: no such column: "%v" - should this be a string literal in single-quotes?`, n.name.Value))
		}
		if r.failed == "" || r.failed == "prepare" || !ofNames {
			return fmt.Sprintf("%q: s4ql gives %v, and the pinned build of SQLite refuses its names when it prepares the statement", src, got), true
		}
	}
	p.y.excused(excusedQuotedSubquery)
	return "", true
}

// subqueryRefusal is [pair.typeRefusal] of the select, the update, or the delete s of the text src
// with scalar subqueries, of slice 15, with the arguments args as SQLite binds them, on the tables of
// o, as the engine checks them, which its differential test pins against the pinned build. It is ""
// for a statement with a name that the tables of its scope do not have, as [scopeName.missing] says,
// whose error comes first, also for one in double quotes that mattn's SQLite reads as a string. For a
// statement that SQLite does not prepare on o, it is the refusal of a second subquery of * of more
// than one column of an update or a delete, whose error SQLite gives for the one that its plan codes
// first, and otherwise "". For one that SQLite prepares, it is the refusal of the first correlated
// name, in the order of the conditions; then, condition by condition in the order of the text, the
// rule of the type of a value, and for a subquery the rule of its conditions on its table, of its
// argument of LIMIT, as [limitRefusal] gives it, and of the type of its column, which must be the
// type of the column that it is compared with, INT and INTEGER being one; and then for a select the
// rule of its LIMIT and for an update the rule of its SET, but in a read-only transaction, which
// refuses the write first.
func (p *pair) subqueryRefusal(src string, s stmt.Statement, args []any, o runner) string {
	tb := p.tb
	tb.Helper()
	names, ok := p.namesOf(s, o)
	if !ok || slices.ContainsFunc(names, func(n scopeName) bool { return n.missing() }) {
		return ""
	}
	table, where := tableWhere(s)
	subs := subqueriesIn(s)
	ps, err := o.PrepareContext(tb.Context(), firstStatement(src))
	if err != nil {
		message := mattnText(tb, err)
		if _, isSelect := s.(*stmt.Select); isSelect || message != "1/1: row value misused" && !strings.HasPrefix(message, "1/1: sub-select returns ") {
			return ""
		}
		var wide []stmt.Term
		for _, c := range where {
			if q := c.Value.Subquery; q > 0 {
				if _, columns := p.columnsIn(o, subs[q-1].Table.Value); subs[q-1].Columns == nil && len(columns) > 1 {
					wide = append(wide, c.Value)
				}
			}
		}
		if len(wide) < 2 {
			return ""
		}
		return fmt.Sprintf("s4ql: not supported: second subquery of * of more than one column (at offset %v)", wide[1].Span.Start)
	}
	is.NotError(tb, ps.Close())
	name, columns := p.columnsIn(o, table.Value)
	if stmttest.HasExpression(s) {
		// The refusals of a statement with arithmetic come in the engine's own order: one of them, as
		// [pair.arithmeticRefusals] gives them.
		refusals := stmttest.ArithmeticRefusals(name, columns, s, args, p.tx != nil && p.tx.readOnly, func(name string) (string, []stmttest.TableColumn) { return p.columnsIn(o, name) })
		if len(refusals) == 0 {
			return ""
		}
		return refusals[0]
	}
	for _, c := range where {
		q := c.Value.Subquery
		if q == 0 {
			continue
		}
		_, inner := p.columnsIn(o, subs[q-1].Table.Value)
		names := slices.Clone(subs[q-1].Columns)
		for _, c := range subs[q-1].Where {
			names = append(names, c.Column)
		}
		for _, c := range subs[q-1].OrderBy {
			names = append(names, c.Name)
		}
		for _, n := range names {
			if stmttest.ColumnOf(inner, n) < 0 {
				return fmt.Sprintf("s4ql: not supported: correlated subquery (at offset %v)", n.Offset)
			}
		}
	}
	for _, c := range where {
		q := c.Value.Subquery
		if q == 0 {
			if rule := stmttest.WhereRule(name, columns, []stmt.Condition{c}, args); rule != "" {
				return rule
			}
			continue
		}
		sub := subs[q-1]
		innerName, inner := p.columnsIn(o, sub.Table.Value)
		if rule := stmttest.WhereRule(innerName, inner, sub.Where, args); rule != "" {
			return rule
		}
		if rule := limitArgument(sub.Limit.Parameter, args); rule != "" {
			return rule
		}
		position := 0
		if sub.Columns != nil {
			position = stmttest.ColumnOf(inner, sub.Columns[0])
		}
		in, out := inner[position], columns[stmttest.ColumnOf(columns, c.Column)]
		if storageClass(in.Type) != storageClass(out.Type) {
			return fmt.Sprintf("s4ql: not supported: subquery %v, of the column %v.%v of type %v, compared with the column %v.%v of type %v", q, innerName, in.Name, in.Type, name, out.Name, out.Type)
		}
	}
	switch s := s.(type) {
	case *stmt.Select:
		return limitRefusal(columns, s, args)
	case *stmt.Update:
		if p.tx != nil && p.tx.readOnly {
			return ""
		}
		set := &stmt.Update{Table: s.Table, Set: s.Set}
		return stmttest.UpdateRule(name, columns, set, args[:set.Parameters()], func() bool {
			q, bound := stmttest.FoundQuery(s.Table, s.Where, s.Subqueries, args)
			rows, err := o.QueryContext(tb.Context(), q, bound...)
			is.NotError(tb, err)
			defer func() { is.NotError(tb, rows.Close()) }()
			if rows.Next() {
				return true
			}
			is.NotError(tb, rows.Err())
			return false
		})
	}
	return ""
}

// storageClass of the declared type of a column of a STRICT table: the kind of its values, which is
// one for INT and INTEGER.
func storageClass(typ string) string {
	if typ == "INT" {
		return "INTEGER"
	}
	return typ
}

// limitRefusal of the select s of the table with the columns, in the order of the table, with the
// arguments args, each as SQLite binds it: the engine's "not supported" of the argument of its
// LIMIT, if that is a parameter whose argument is no integer from 0, which the engine gives after
// SQLite's errors of the names, also those of ORDER BY, or "". SQLite would convert a real or text
// that holds an integer, refuse the others with `datatype mismatch`, and read a negative integer as
// no limit.
func limitRefusal(columns []stmttest.TableColumn, s *stmt.Select, args []any) string {
	if s.Limit.Parameter == 0 || len(args) != s.Parameters() {
		return ""
	}
	if _, missing := stmttest.MissingColumn(columns, s); missing ||
		slices.ContainsFunc(s.OrderBy, func(c stmt.OrderColumn) bool { return stmttest.ColumnOf(columns, c.Name) < 0 }) {
		return ""
	}
	return limitArgument(s.Limit.Parameter, args)
}

// limitArgument is the engine's "not supported" of the argument of the parameter of the number
// parameter of a LIMIT, of a select or of a scalar subquery, among the arguments args of its
// statement, each as SQLite binds them, if it is no integer from 0, or "", also for a parameter of
// 0, a LIMIT without one.
func limitArgument(parameter int, args []any) string {
	if parameter == 0 {
		return ""
	}
	var which string
	switch v := args[parameter-1].(type) {
	case int64:
		if v >= 0 {
			return ""
		}
		which = "a negative integer"
	case nil:
		which = "NULL"
	case float64:
		which = "a real"
	case string:
		which = "text"
	case []byte:
		which = "a blob"
	default:
		return ""
	}
	return fmt.Sprintf("s4ql: not supported: argument %v, which is %v, for LIMIT, which takes an integer from 0", parameter, which)
}

// columnsIn of the table with the name, as SQLite compares names, in SQLite on o, in the order of
// the table, and the name of the table as the table stores it. They are nil and "" if SQLite has no
// such table there.
func (p *pair) columnsIn(o runner, name string) (string, []stmttest.TableColumn) {
	tb := p.tb
	tb.Helper()
	rows, err := o.QueryContext(tb.Context(), `select m.name, p.name, p.type, p.pk, p."notnull", coalesce(p.dflt_value, '') from sqlite_schema m, pragma_table_info(m.name) p where m.type = 'table' and m.name = ? collate nocase order by p.cid`, name)
	is.NotError(tb, err)
	defer func() { is.NotError(tb, rows.Close()) }()
	var table string
	var columns []stmttest.TableColumn
	for rows.Next() {
		var c stmttest.TableColumn
		var pk, notNull int
		// A column without DEFAULT has the dflt_value NULL, and the text of a literal is never empty.
		is.NotError(tb, rows.Scan(&table, &c.Name, &c.Type, &pk, &notNull, &c.Default))
		// The key of a table WITHOUT ROWID never takes NULL, also when SQLite reports it without NOT
		// NULL.
		c.PrimaryKey = pk == 1
		c.Nullable = !c.PrimaryKey && notNull == 0
		columns = append(columns, c)
	}
	is.NotError(tb, rows.Err())
	return table, columns
}

// indexesIn gives the indexes that a create index made on the table of the name on o, in the order in
// which they were created, each with the positions of its columns, or nil for a table that o does
// not have.
func (p *pair) indexesIn(o runner, table string) []stmttest.TableIndex {
	tb := p.tb
	tb.Helper()
	rows, err := o.QueryContext(tb.Context(), `select il.name, ii.cid from pragma_index_list(?) il, pragma_index_info(il.name) ii where il.origin = 'c' order by il.name, ii.seqno`, table)
	is.NotError(tb, err)
	defer func() { is.NotError(tb, rows.Close()) }()
	var indexes []stmttest.TableIndex
	for rows.Next() {
		var name string
		var position int
		is.NotError(tb, rows.Scan(&name, &position))
		if len(indexes) == 0 || indexes[len(indexes)-1].Name != name {
			indexes = append(indexes, stmttest.TableIndex{Name: name})
		}
		last := &indexes[len(indexes)-1]
		last.Columns = append(last.Columns, stmttest.IndexKeyColumn{Position: position})
	}
	is.NotError(tb, rows.Err())
	return indexes
}

// tableWhere gives the table and the WHERE of the select, the update, or the delete s.
func tableWhere(s stmt.Statement) (stmt.Name, []stmt.Condition) {
	switch s := s.(type) {
	case *stmt.Select:
		return s.Table, s.Where
	case *stmt.Update:
		return s.Table, s.Where
	case *stmt.Delete:
		return s.Table, s.Where
	}
	return stmt.Name{}, nil
}

// reachExists counts with count what the statement s of IF NOT EXISTS or IF EXISTS of slice 10 of
// the text src reaches, whose outcome of s4ql is got, on the tables and indexes that SQLite has on o
// before the call, by [stmttest.ExistsRule]: the kind of the statement, such as "drop table if
// exists", and, where the rule says that SQLite does nothing, "did nothing" and the rule's reason, and
// otherwise "did its work" for one that succeeded and the kind of the outcome, as kind gives it, for
// one that failed. Where the rule says that SQLite does nothing, s4ql must succeed. Only the first run
// counts, since a run after it meets what the first did, and nothing counts for a statement without
// the clause, or for runs with arguments, which s4ql refuses by its rule.
func (p *pair) reachExists(src string, s stmt.Statement, runs [][]any, got outcome, o runner, kind func(outcome) string, count func(string)) {
	tb := p.tb
	tb.Helper()
	var what string
	switch s := s.(type) {
	case *stmt.CreateTable:
		what = map[bool]string{true: "create table if not exists"}[s.IfNotExists]
	case *stmt.DropTable:
		what = map[bool]string{true: "drop table if exists"}[s.IfExists]
	case *stmt.CreateIndex:
		what = map[bool]string{true: "create index if not exists"}[s.IfNotExists]
	case *stmt.DropIndex:
		what = map[bool]string{true: "drop index if exists"}[s.IfExists]
	}
	outs := got.runs()
	if what == "" || len(outs) == 0 || slices.ContainsFunc(runs, func(args []any) bool { return len(args) > 0 }) {
		return
	}
	tables := map[string][]stmttest.TableColumn{}
	indexes := map[string]bool{}
	rows, err := o.QueryContext(tb.Context(), "select type, name from sqlite_schema where type = 'table' or type = 'index' and sql is not null")
	is.NotError(tb, err)
	var names [][2]string
	for rows.Next() {
		var typ, name string
		is.NotError(tb, rows.Scan(&typ, &name))
		names = append(names, [2]string{typ, name})
	}
	is.NotError(tb, rows.Err())
	is.NotError(tb, rows.Close())
	for _, n := range names {
		fold := stmt.Name{Value: n[1]}.Fold()
		if n[0] == "index" {
			indexes[fold] = true
			continue
		}
		_, tables[fold] = p.columnsIn(o, n[1])
	}
	// A QueryRow of a statement that gives no rows ends with the error of its scan, which is no failure
	// of the statement.
	outcome := kind(outs[0])
	if outcome == "ok" || outcome == "no rows" {
		outcome = "did its work"
	}
	if rule := stmttest.ExistsRule(tables, indexes, s); rule != "" {
		if outcome != "did its work" {
			tb.Errorf("%q: s4ql gives %v, and the rule does nothing: %v", src, got, rule)
		}
		count(what + " did nothing: " + rule)
		return
	}
	count(what + " " + outcome)
}

// checkArguments of the outcome got of src, with the arguments of each run, which is s4ql's error
// of another number of arguments than the statement has parameters: an error of s4ql's own, where
// SQLite binds NULL to a parameter without an argument and mattn refuses an argument too many itself.
// In each run, it must be the error that [engine.Check] gives for the text with the arguments of the
// run, and come when the statement runs, since Prepare checks only the text.
func (p *pair) checkArguments(src string, runs [][]any, got outcome) string {
	if len(got.runs()) != len(runs) {
		return fmt.Sprintf("%q with %v runs: s4ql gives %v", src, len(runs), got)
	}
	for i, o := range got.runs() {
		n := len(runs[i])
		want := engine.Check(src, n)
		if !errors.Is(want, engine.ErrArguments) || o.err != want.Error() || o.failed == "prepare" || o.failed == "" {
			return fmt.Sprintf("%q: s4ql gives %v, where its check of the text with %v arguments gives %v", src, got, n, want)
		}
	}
	p.y.excused(excusedArguments)
	return ""
}

// startedTable reports whether s4ql's outcome got and SQLite's outcome want for the text src through
// the call c, with the arguments of each run, differ in the one way that the tests accept, which
// [stmttest.ExistsAfterStart] describes: SQLite gives that a table exists, and s4ql gives what SQLite
// gives for src on an empty database.
func (p *pair) startedTable(src string, c call, runs [][]any, got, want outcome) bool {
	tb := p.tb
	tb.Helper()
	w, ok := strings.CutPrefix(want.err, "1/1: ")
	if !ok || !stmttest.ExistsAfterStart(src, w) {
		return false
	}
	// The text is a syntax error, so it changes nothing on the empty database.
	return got.String() == run(tb, p.empty, c, firstStatement(src), runs, mattnText).String()
}

// knownGap of the pass after the parser, as [stmttest.KnownGap] gives it, that s4ql's outcome got and
// SQLite's outcome want fall in, or "".
func knownGap(src string, got, want outcome) string {
	m, ok := strings.CutPrefix(got.err, "1/1: ")
	w, wok := strings.CutPrefix(want.err, "1/1: ")
	if !ok || !wok || got.failed != want.failed {
		return ""
	}
	return stmttest.KnownGap(src, m, w)
}

// tables of SQLite's database, by their names as SQLite holds them.
func (p *pair) tables() []string {
	tb := p.tb
	tb.Helper()
	rows, err := p.sqlite.QueryContext(tb.Context(), "select name from sqlite_schema where type = 'table' order by name collate binary")
	is.NotError(tb, err)
	var names []string
	for rows.Next() {
		var name string
		is.NotError(tb, rows.Scan(&name))
		names = append(names, name)
	}
	is.NotError(tb, rows.Err())
	is.NotError(tb, rows.Close())
	return names
}

// compareChanged reads back the table of src, if it holds an update or a delete, after the statement
// ran on s4ql's runner s and SQLite's o, with s4ql's outcome got, whose outcomes were compared, through
// a query of it on each, in the transaction where the statement ran, if it ran in one, and gives what
// differs, or "". A run that failed changed nothing, on either side, also one that s4ql failed with
// an evaluation error after it had changed rows, and that did not run in SQLite. It counts each such
// read under "reached: table read back after an update or a delete", and once more by a run that
// failed with an evaluation error, in a transaction or not.
func (p *pair) compareChanged(src string, got outcome, s, o runner) string {
	tb := p.tb
	tb.Helper()
	var table stmt.Name
	switch st, _ := stmt.Check(src); st := st.(type) {
	case *stmt.Update:
		table = st.Table
	case *stmt.Delete:
		table = st.Table
	default:
		return ""
	}
	if p.y != nil {
		p.y["reached: table read back after an update or a delete"]++
		if slices.ContainsFunc(got.runs(), evaluated) {
			// Every update and delete runs in the open transaction, if there is one, also beside it.
			p.y[map[bool]string{false: "reached: table read back after an evaluation error of an update or a delete, outside a transaction",
				true: "reached: table read back after an evaluation error of an update or a delete, in a transaction"}[p.tx != nil]]++
		}
	}
	q := "select * from " + stmttest.Quote(table.Value)
	held, want := run(tb, s, callQuery, q, nil, s4qlText), run(tb, o, callQuery, q, nil, mattnText)
	if held.full() != want.full() {
		return fmt.Sprintf("%q: after it, the table holds in s4ql\n%v\nand in SQLite\n%v", src, held.full(), want.full())
	}
	return ""
}

// compareTables of SQLite, and the tables that a text created, through a query of each on both
// sides, through the pool, so that a table that only s4ql has is found too. It gives what differs,
// or "".
func (p *pair) compareTables() string {
	tb := p.tb
	tb.Helper()
	names := p.tables()
	for name := range p.created {
		if !slices.ContainsFunc(names, func(n string) bool { return stmt.Name{Value: n}.Fold() == stmt.Name{Value: name}.Fold() }) {
			names = append(names, name)
		}
	}
	for _, name := range names {
		src := "select * from " + stmttest.Quote(name)
		got, want := run(tb, p.s4ql, callQuery, src, nil, s4qlText), run(tb, p.sqlite, callQuery, src, nil, mattnText)
		if got.full() != want.full() {
			return fmt.Sprintf("the table %q: s4ql holds\n%v\nand SQLite\n%v", name, got.full(), want.full())
		}
	}
	return ""
}

// restart the instance of s4ql: close it, and open a new one on the same database, with an empty
// cache if empty and with the cache of the one before otherwise. A pinned connection is pinned again.
func (p *pair) restart(empty bool) {
	tb := p.tb
	tb.Helper()
	if p.tx != nil {
		tb.Fatal("a transaction is open")
	}
	if c, ok := p.s.(*sql.Conn); ok {
		is.NotError(tb, c.Close())
	}
	is.NotError(tb, p.s4ql.Close())
	if empty {
		p.cache = filepath.Join(tb.TempDir(), "cache")
	}
	p.s4ql = open(tb, p.w.options(p.prefix, p.cache))
	p.s = p.s4ql
	if p.pinned {
		// SQLite's connection is pinned again too, so that both counts of changes start at 0.
		if c, ok := p.o.(*sql.Conn); ok {
			is.NotError(tb, c.Close())
		}
		p.pin()
	}
}

// close the instance of s4ql, and fail the test if the close fails.
func (p *pair) close() {
	tb := p.tb
	tb.Helper()
	if p.tx != nil {
		tb.Fatal("a transaction is open")
	}
	if c, ok := p.s.(*sql.Conn); ok {
		is.NotError(tb, c.Close())
	}
	is.NotError(tb, p.s4ql.Close())
}

// step of a sequence: a text and the call that runs it.
type step struct {
	call call
	src  string
	// want is the outcome that both sides must give, as [outcome.String] writes it, with the rows of
	// a query in the order of their texts, whatever the order in which they come, or "" to compare
	// the two sides only. For a QueryRow of a query with several rows, it is [oneOf] the outcomes with
	// each of them.
	want string
}

// oneOf the outcomes, as the want of a [step] whose outcome depends on the order of the rows, which
// SQLite leaves unspecified: a QueryRow of a query with several rows gives whichever comes first.
func oneOf(outcomes ...string) string {
	return strings.Join(outcomes, "\n")
}

const (
	t1 = "create table t1 (v1 int primary key) strict, without rowid"
	// t3 is the table of the example of slice 5: three columns of three types, with the key first.
	t3 = "create table t3 (v1 int primary key, v2 text not null, v3 real not null) strict, without rowid"
	// t4 is the table of the sequences of slice 9, with a column for each index: text and an integer
	// that take NULL, and a real.
	t4 = "create table t4 (v1 int primary key, v2 text, v3 real not null, v4 int) strict, without rowid"
	// t5 is the table of the sequences of slice 14: a column of each type, NULL in all but the key and
	// v3, and a default on v3.
	t5 = "create table t5 (v1 int primary key, v2 text, v3 real not null default 0.5, v4 blob, v5 integer) strict, without rowid"
)

// sequences written by hand, which cover every statement of slice 1, every error that an
// application can get from it, and each call.
var sequences = map[string][]step{
	// Arithmetic, and a column compared with and set from a column: what s4ql computes is SQLite's,
	// its refusals of types are those of its rules, and an evaluation error, which changes nothing, is
	// justified by SQLite's own result of the operator, also in one run of a prepared statement run
	// several times.
	"arithmetic, and a column compared with a column": {
		{callExec, "create table t2 (v2 int primary key, s2 text, i2 int, r2 real) strict, without rowid", "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t2 values (1, 'a', 1, 0.5)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t2 values (2, 'b', null, 1e308)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t2 values (3, 'c', 9223372036854775807, -2.5)", "exec: 1 affected, last insert id 0"},
		{callQuery, "select * from t2 where v2 = s2", "query: query failed with s4ql: not supported: the column t2.s2 of type TEXT compared with the column t2.v2 of type INT"},
		{callQueryRow, "select v2 from t2 where v2 = i2", `query row: row ["int64:1"]`},
		{callPrepareQuery, "select v2 from t2 where v2 + 1 = 2", `prepare, query: columns "v2" of types "INT", rows ["int64:1"]`},
		{callQuery, "select v2 from t2 where v2 / (v2 - v2) = 1", "query: rows failed with s4ql: division by zero in / (at offset 24)"},
		{callQuery, "select v2 from t2 where v2 = (select v2 from t2 where i2 * 2 = 2)",
			// The subquery reads up to its first row, in the direction that the engine draws, and the
			// row of the largest integer fails before the row of 1 where it comes first.
			oneOf(`query: columns "v2" of types "INT", rows ["int64:1"]`, "query: query failed with s4ql: integer overflow in * (at offset 54)")},
		{callQuery, "select v2 from t2 where s2 + 1 = 2", "query: query failed with s4ql: not supported: column of type TEXT in arithmetic (at offset 24)"},
		{callExec, "update t2 set i2 = i2 + 1", "exec: exec failed with s4ql: integer overflow in + (at offset 19)"},
		{callExec, "update t2 set r2 = r2 * 10", "exec: exec failed with s4ql: real result that is not finite in * (at offset 19)"},
		{callExec, "update t2 set i2 = s2", "exec: exec failed with s4ql: not supported: the column t2.s2 of type TEXT for the column t2.i2 of type INT"},
		{callExec, "update t2 set r2 = i2", "exec: exec failed with s4ql: not supported: the column t2.i2 of type INT for the column t2.r2 of type REAL"},
		// In a transaction, an update or a delete that fails with an evaluation error after it changed
		// a row, in the direction that the engine draws, changes nothing either, and the transaction
		// stays open: the table is read back after each.
		{callBegin, "", "begin"},
		{callExec, "update t2 set i2 = i2 + 1", "exec: exec failed with s4ql: integer overflow in + (at offset 19)"},
		{callExec, "update t2 set r2 = r2 * 10", "exec: exec failed with s4ql: real result that is not finite in * (at offset 19)"},
		{callExec, "delete from t2 where 10 / (v2 - 3) < 0", "exec: exec failed with s4ql: division by zero in / (at offset 21)"},
		{callQuery, "select * from t2", `query: columns "v2,s2,i2,r2" of types "INT,TEXT,INT,REAL", rows ["int64:1" "string:a" "int64:1" "float64:0.5"] ["int64:2" "string:b" "<nil>:<nil>" "float64:1e+308"] ["int64:3" "string:c" "int64:9223372036854775807" "float64:-2.5"]`},
		{callCommit, "", "commit"},
		{callExec, "update t2 set i2 = v2 * 2, v2 = v2 + 10 where v2 < 3", "exec: 2 affected, last insert id 0"},
		{callPrepareExecRuns, "update t2 set i2 = i2 + 4611686018427387904 * ? where v2 = 11", "prepare, exec several times: 1 affected, last insert id 0; then: exec failed with s4ql: integer overflow in * (at offset 24); then: exec failed with s4ql: integer overflow in + (at offset 19)"},
		{callExec, "delete from t2 where -i2 < -100", "exec: 2 affected, last insert id 0"},
		{callQuery, "select * from t2", `query: columns "v2,s2,i2,r2" of types "INT,TEXT,INT,REAL", rows ["int64:12" "string:b" "int64:4" "float64:1e+308"]`},
		{callExec, "drop table t2", "exec: 2 affected, last insert id 0"},
	},
	// Text in expressions: what s4ql computes is SQLite's, and its refusals of types are those of its
	// rules, also for a statement that is prepared and then run.
	"text in expressions": {
		{callExec, "create table t3 (v3 text primary key, w3 text, b3 blob, n3 int) strict, without rowid", "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t3 values ('m_00ff', 'A', x'00ff', 1)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t3 values ('b', 'Bc', x'', 2)", "exec: 1 affected, last insert id 0"},
		{callPrepareExec, "update t3 set w3 = 'a' where lower(w3) = 'a'", "prepare, exec: 1 affected, last insert id 0"},
		{callPrepareQuery, "select v3 from t3 where v3 = 'm_' || lower(hex(b3))", `prepare, query: columns "v3" of types "TEXT", rows ["string:m_00ff"]`},
		{callQueryRow, "select v3 from t3 where lower(w3) || v3 = 'bcb'", `query row: row ["string:b"]`},
		{callQuery, "select v3 from t3 where hex(b3) = ''", `query: columns "v3" of types "TEXT", rows ["string:b"]`},
		{callExecArgument, "update t3 set w3 = lower(w3) || 'x' where n3 = ?", "exec with an argument: 1 affected, last insert id 0"},
		{callQuery, "select v3 from t3 where n3 = lower(w3)", "query: query failed with s4ql: not supported: an expression of type text compared with the column t3.n3 of type INT"},
		{callExec, "update t3 set w3 = n3 || 'a'", "exec: exec failed with s4ql: not supported: column of type INT in a concatenation (at offset 19)"},
		{callPrepareExec, "update t3 set b3 = hex(b3)", "prepare, exec: exec failed with s4ql: not supported: an expression of type text for the column t3.b3 of type BLOB"},
		{callBegin, "", "begin"},
		{callExec, "update t3 set v3 = 'm_' || lower(hex(b3)) || v3 returning v3", ""},
		{callQuery, "select * from t3 where lower(w3) = 'ax'", ""},
		{callRollback, "", "rollback"},
		{callQuery, "select * from t3", ""},
		// SQLite walks on after a missing name inside a call, and a later one replaces it.
		{callExec, `update t3 set w3 = lower("nosuch") || "nosuch2"`, ""},
		{callExec, `update t3 set w3 = lower(nosuch) || "nosuch2"`, ""},
		{callExec, `update t3 set w3 = lower("nosuch") || w3`, ""},
		{callExec, "drop table t3", "exec: 2 affected, last insert id 0"},
	},
	"the example of slice 1": {
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows `},
		{callExec, "insert into t1 (v1) values (1)", "exec: 1 affected, last insert id 0"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows ["int64:1"]`},
		{callExec, "drop table t1", "exec: 1 affected, last insert id 0"},
	},
	"several rows, through each call": {
		{callPrepareExec, t1, "prepare, exec: 0 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (3)", "exec: 1 affected, last insert id 0"},
		{callPrepareExec, "insert into t1 (v1) values (-9223372036854775808)", "prepare, exec: 1 affected, last insert id 0"},
		{callQuery, "insert into t1 (v1) values (9223372036854775807)", "query"},
		{callQueryRow, "insert into t1 (v1) values (0)", "query row: scan failed with no rows"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows ["int64:-9223372036854775808"] ["int64:0"] ["int64:3"] ["int64:9223372036854775807"]`},
		{callPrepareQuery, "select * from t1", `prepare, query: columns "v1" of types "INT", rows ["int64:-9223372036854775808"] ["int64:0"] ["int64:3"] ["int64:9223372036854775807"]`},
		{callQueryRow, "select * from t1", oneOf(`query row: row ["int64:-9223372036854775808"]`, `query row: row ["int64:0"]`, `query row: row ["int64:3"]`, `query row: row ["int64:9223372036854775807"]`)},
		{callExec, "select * from t1", "exec: 1 affected, last insert id 0"},
		{callPrepareQuery, "drop table t1", "prepare, query"},
		{callQueryRow, "select * from t1", "query row: scan failed with 1/1: no such table: t1"},
	},
	"the count of changes, which a statement that sets none leaves": {
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (1)", "exec: 1 affected, last insert id 0"},
		{callExec, "create table t2 (v2 int primary key) strict, without rowid", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (1)", "exec: exec failed with 19/1555: UNIQUE constraint failed: t1.v1"},
		{callExec, "create table t3 (v3 int primary key) strict, without rowid", "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (2)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t9 (v1) values (2)", "exec: exec failed with 1/1: no such table: t9"},
		{callExec, "drop table t3", "exec: 1 affected, last insert id 0"},
		{callExec, "selec", `exec: exec failed with 1/1: near "selec": syntax error`},
		{callExec, "drop table t2", "exec: 1 affected, last insert id 0"},
	},
	"the errors of slice 1, through Exec and Query": {
		{callQuery, "select * from t1", "query: query failed with 1/1: no such table: t1"},
		{callExec, "insert into t1 (v1) values (1)", "exec: exec failed with 1/1: no such table: t1"},
		{callExec, "drop table t1", "exec: exec failed with 1/1: no such table: t1"},
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callQuery, t1, "query: query failed with 1/1: table t1 already exists"},
		{callExec, `create table "T1" (x int primary key) strict, without rowid`, `exec: exec failed with 1/1: table "T1" already exists`},
		{callExec, "insert into t1 (v2) values (1)", "exec: exec failed with 1/1: table t1 has no column named v2"},
		{callExec, "insert into t1 (v1) values (1)", "exec: 1 affected, last insert id 0"},
		{callQuery, "insert into T1 (V1) values (1)", "query: query failed with 19/1555: UNIQUE constraint failed: t1.v1"},
		{callQuery, "selec * from t1", `query: query failed with 1/1: near "selec": syntax error`},
		{callExec, "insert into t1 (v1) values (", "exec: exec failed with 1/1: incomplete input"},
		{callQueryRow, "insert into t1 (v1) values (1)", "query row: scan failed with 19/1555: UNIQUE constraint failed: t1.v1"},
	},
	"the errors of slice 1, through Prepare": {
		// SQLite gives these errors when it prepares the text, and s4ql when the statement runs.
		{callPrepareQuery, "select * from t1", "prepare, query: query failed with 1/1: no such table: t1"},
		{callPrepareExec, "insert into t1 (v1) values (1)", "prepare, exec: exec failed with 1/1: no such table: t1"},
		{callPrepareExec, "drop table t1", "prepare, exec: exec failed with 1/1: no such table: t1"},
		{callPrepareExec, t1, "prepare, exec: 0 affected, last insert id 0"},
		{callPrepareExec, t1, "prepare, exec: exec failed with 1/1: table t1 already exists"},
		{callPrepareExec, "insert into t1 (v2) values (1)", "prepare, exec: exec failed with 1/1: table t1 has no column named v2"},
		{callPrepareExec, "insert into t1 (v1) values (1)", "prepare, exec: 1 affected, last insert id 0"},
		// These both sides give at the same step.
		{callPrepareExec, "insert into t1 (v1) values (1)", "prepare, exec: exec failed with 19/1555: UNIQUE constraint failed: t1.v1"},
		{callPrepareQuery, "insert into t1 (v1) values (1)", "prepare, query: query failed with 19/1555: UNIQUE constraint failed: t1.v1"},
		{callPrepareExec, "select * frm t1", `prepare, exec: prepare failed with 1/1: near "frm": syntax error`},
		{callPrepareQuery, "drop table", "prepare, query: prepare failed with 1/1: incomplete input"},
	},
	"a syntax error after the start of a table that exists": {
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		// SQLite gives `table t1 already exists`, and s4ql what SQLite gives on an empty database.
		{callExec, "create table t1 (v1 int primary key) strict without rowid", ""},
	},
	"names in quotes and in other cases": {
		{callExec, `create table "Foo bar" ("Bar""x" int primary key) strict, without rowid`, "exec: 0 affected, last insert id 0"},
		{callExec, `insert into "foo BAR" ("bar""X") values (7)`, "exec: 1 affected, last insert id 0"},
		{callQuery, `select * from "FOO BAR"`, `query: columns "Bar\"x" of types "INT", rows ["int64:7"]`},
		{callExec, `insert into "Foo bar" ("Bar""x") values (7)`, `exec: exec failed with 19/1555: UNIQUE constraint failed: Foo bar.Bar"x`},
		{callExec, "create table Été (abort int primary key) strict, without rowid", "exec: 0 affected, last insert id 0"},
		{callQuery, "select * from Été", `query: columns "abort" of types "INT", rows `},
		{callExec, "select * from été", "exec: exec failed with 1/1: no such table: été"},
	},
	"statements and texts that are not supported": {
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (1), (2)", ""},
		{callExec, "insert into t1 (v1) values (9223372036854775808)", ""},
		{callQuery, "select v1 from t1", ""},
		{callExec, "insert into t1 (v1) values (?)", "exec: exec failed with s4ql: wrong number of arguments: 0 for a statement with 1 parameter"},
		{callExec, "begin", ""},
		{callQuery, "select * from t1; select * from t1", ""},
		{callExec, "create table t2 (v2 integer primary key) strict, without rowid", "exec: 0 affected, last insert id 0"},
		{callPrepareExec, "insert into t1 (v1) values (1.5)", "prepare, exec: exec failed with s4ql: not supported: a real literal for the column t1.v1 of type INT"},
		{callExecArgument, "insert into t2 (v2) values (?)", "exec with an argument: 1 affected, last insert id 0"},
		{callExec, "insert into t2 (v2) values ('1')", "exec: exec failed with s4ql: not supported: a string literal for the column t2.v2 of type INTEGER"},
		{callExec, "insert into t2 (v2) values (1e999)", "exec: exec failed with s4ql: not supported: infinite real number (at offset 28)"},
		{callExec, `insert into t1 (v1) values ("x")`, ""},
		{callPrepareExec, "drop view if exists t1", "prepare, exec: prepare failed with s4ql: not supported: DROP VIEW (at offset 5)"},
		{callExec, "", "exec: exec failed with s4ql: not supported: a text that holds no statement"},
		{callPrepareQuery, " ; -- nothing", "prepare, query: prepare failed with s4ql: not supported: a text that holds no statement"},
		{callExecArgument, "insert into t1 (v1) values (1)", "exec with an argument: exec failed with s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
		{callExec, "insert into t1 (v1) values (?1)", "exec: exec failed with s4ql: not supported: numbered parameter (at offset 28)"},
		{callExecArgument, "insert into t1 (v1) values (:v)", "exec with an argument: exec failed with s4ql: not supported: named parameter (at offset 28)"},
		{callExecArgument, "insert into t1 (v1) values (-?)", "exec with an argument: exec failed with s4ql: not supported: parameter after a sign (at offset 29)"},
		{callBegin, "", "begin"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows `},
	},
	"texts with an argument, each of which is judged before the argument": {
		// The spec: "Errors match SQLite's [...] for text that is a syntax error in SQLite, also when it
		// is SQL that s4ql does not support."
		{callExecArgument, "drop table", "exec with an argument: exec failed with 1/1: incomplete input"},
		{callExecArgument, "selec", `exec with an argument: exec failed with 1/1: near "selec": syntax error`},
		{callExecArgument, "select v1 from t1 where", "exec with an argument: exec failed with 1/1: incomplete input"},
		{callExecArgument, "select * from t1 group by v1", "exec with an argument: exec failed with s4ql: not supported: GROUP BY clause (at offset 17)"},
		{callExecArgument, "select * from t1 where v1 = 1 or v1 = 2", "exec with an argument: exec failed with s4ql: not supported: OR (at offset 30)"},
		{callExecArgument, "select * from t1 where v1 = 1", "exec with an argument: exec failed with s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
		{callExecArgument, "select * from t1 order by v1 limit 1", "exec with an argument: exec failed with s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
		{callExecArgument, "insert into t1 (v1) values (? ?)", `exec with an argument: exec failed with 1/1: near "?": syntax error`},
		{callExecArgument, "insert into t1 (v1) values (?)", "exec with an argument: exec failed with 1/1: no such table: t1"},
		{callExecArgument, " ; -- nothing", "exec with an argument: exec failed with s4ql: not supported: a text that holds no statement"},
		// A statement without a parameter that s4ql supports is not run, also where it would fail.
		{callExecArgument, t1, "exec with an argument: exec failed with s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callExecArgument, "insert into t1 (v1) values (1)", "exec with an argument: exec failed with s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
		{callExecArgument, "drop table t1", "exec with an argument: exec failed with s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
		{callExecArgument, "insert into t9 (v1) values (1)", "exec with an argument: exec failed with s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
		// SQLite gives `table t1 already exists`, and s4ql what SQLite gives on an empty database.
		{callExecArgument, "create table t1 (v1 int primary key) strict without rowid", `exec with an argument: exec failed with 1/1: near "without": syntax error`},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows `},
		// A statement with a parameter runs with the argument.
		{callExecArgument, "insert into t1 (v1) values (?)", "exec with an argument: 1 affected, last insert id 0"},
		{callExecArgument, "insert into T1 (V1) values ( ? );", "exec with an argument: exec failed with 19/1555: UNIQUE constraint failed: t1.v1"},
		{callExec, "insert into t1 (v1) values (?)", "exec: exec failed with s4ql: wrong number of arguments: 0 for a statement with 1 parameter"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows ["int64:1"]`},
	},
	"selects with WHERE": {
		{callExec, "select * from t1 where v1 = 1", "exec: exec failed with 1/1: no such table: t1"},
		{callExec, "create table t1 (v1 int primary key, v2 text) strict, without rowid", "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t1 values (1, 'a')", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t1 values (2, null)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t1 values (3, 'c')", "exec: 1 affected, last insert id 0"},
		{callQuery, "select * from t1 where v1 >= 2 and v1 < 10 and v2 is not null", `query: columns "v1,v2" of types "INT,TEXT", rows ["int64:3" "string:c"]`},
		{callQueryArgument, "select v2 from t1 where v1 = ?", `query with an argument: columns "v2" of types "TEXT", rows ["string:a"]`},
		{callPrepareQuery, "select v1 from t1 where v2 is null", `prepare, query: columns "v1" of types "INT", rows ["int64:2"]`},
		{callQueryArgument, "select v1 from t1 where v1 > ? and v2 != 'c'", `query with an argument: columns "v1" of types "INT", rows `},
		{callQuery, "select v1 from t1 where v2 = null", `query: columns "v1" of types "INT", rows `},
		{callQuery, "select v1 from t1 where x is null and y = 1", "query: query failed with 1/1: no such column: x"},
		{callQuery, "select v1 from t1 where x is null and y is null", "query: query failed with 1/1: no such column: y"},
		{callQuery, "select x from t1 where y = 1", "query: query failed with 1/1: no such column: x"},
		// s4ql's rule for a value of another type than its column, in the order of the text.
		{callQuery, "select * from t1 where v2 is null and v1 = '1' and v2 = 2", "query: query failed with s4ql: not supported: a string literal compared with the column t1.v1 of type INT"},
		{callQueryArgument, "select * from t1 where v2 = ?", "query with an argument: query failed with s4ql: not supported: argument 1, which is an integer, compared with the column t1.v2 of type TEXT"},
		{callPrepareQuery, "select * from t1 where v1 = 1 or v1 = 2", "prepare, query: prepare failed with s4ql: not supported: OR (at offset 30)"},
		{callBegin, "", "begin"},
		{callExec, "insert into t1 values (4, 'd')", "exec: 1 affected, last insert id 0"},
		{callQuery, "select v1 from t1 where v1 > 2", `query: columns "v1" of types "INT", rows ["int64:3"] ["int64:4"]`},
		{callRollback, "", "rollback"},
		{callQuery, "select v1 from t1 where v1 > 2", `query: columns "v1" of types "INT", rows ["int64:3"]`},
	},
	"selects with ORDER BY and LIMIT, with ties, through each call, in transactions, and with rows left open": {
		// The outcomes hold the rows of a query in the order of their texts, so "" leaves their order,
		// and which rows that tie a LIMIT keeps, to the comparison with SQLite.
		{callExec, t4, "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t4 values (1, 'a', 1.5, null)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t4 values (2, null, 1.5, 3)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t4 values (3, 'a', 0.5, 3)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t4 values (4, 'b', 2.5, null)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t4 values (5, null, 0.5, 1)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t4 values (6, 'a', -0.0, 1)", "exec: 1 affected, last insert id 0"},
		{callQuery, "select * from t4 order by v2, v3 desc", ""},
		{callQuery, "select v1, v2 from t4 order by v4 desc, v1", ""},
		{callQuery, "select v1 from t4 order by v2 limit 3", ""},
		{callQuery, "select v1 from t4 limit 2", ""},
		{callQuery, "select v1 from t4 order by v1 desc limit 0", `query: columns "v1" of types "INT", rows `},
		{callQueryRow, "select v1 from t4 order by v3 desc", `query row: row ["int64:4"]`},
		{callQueryRow, "select v2 from t4 order by v2 desc limit 1", `query row: row ["string:b"]`},
		{callQueryRow, "select v1 from t4 order by v4 limit 0", "query row: scan failed with no rows"},
		{callQueryRow, "select v1 from t4 order by v2", ""},
		{callQueryArgument, "select v1 from t4 order by v3 limit ?", `query with an argument: columns "v1" of types "INT", rows ["int64:6"]`},
		{callPrepareQuery, "select v1 from t4 where v4 is not null order by v4, v1 desc limit 100", ""},
		{callExecArgument, "select v1 from t4 limit ?", ""},
		{callPrepareExecRuns, "select v1 from t4 order by v2 limit ?", ""},
		{callQuery, "select v1 from t4 order by x", "query: query failed with 1/1: no such column: x"},
		{callPrepareQuery, "select v1 from t4 order by v2, nope limit 1", "prepare, query: query failed with 1/1: no such column: nope"},
		{callBegin, "", "begin"},
		{callExec, "insert into t4 values (7, 'a', 9.5, 2)", "exec: 1 affected, last insert id 0"},
		{callQuery, "select v1 from t4 order by v2 limit 4", ""},
		{callQueryOpen, "select v1 from t4 order by v2 limit 4", ""},
		{callQuery, "select * from t4 order by v3 desc limit 2", ""},
		{callQueryOpen, "select v1 from t4 limit 3", ""},
		{callStmtQuery, "select v1, v3 from t4 order by v3 limit 3", ""},
		{callCommit, "", "commit"},
		{callBeginReadOnly, "", "begin read-only"},
		{callQueryRow, "select v1 from t4 order by v3 desc, v1", `query row: row ["int64:7"]`},
		{callStmtQuery, "select v1, v3 from t4 order by v3 limit 3", ""},
		{callQueryOpen, "select v1 from t4 order by v2 desc limit 2", ""},
		{callBeside, "select v1 from t4 order by v4 desc limit 3", ""},
		{callRollback, "", "rollback"},
	},
	"a prepared statement with a parameter, run several times, and a query with an argument": {
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		// The runs have the arguments 1, 2, and 1.
		{callPrepareExecRuns, "insert into t1 (v1) values (?)", "prepare, exec several times: 1 affected, last insert id 0; then: 1 affected, last insert id 0; then: exec failed with 19/1555: UNIQUE constraint failed: t1.v1"},
		{callQueryArgument, "insert into T1 (V1) values ( ? )", "query with an argument: query failed with 19/1555: UNIQUE constraint failed: t1.v1"},
		{callQueryArgument, "select * from t1", "query with an argument: query failed with s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
		{callPrepareExecRuns, "insert into t1 (v1) values (3)", "prepare, exec several times: exec failed with s4ql: wrong number of arguments: 1 for a statement with 0 parameters; then: exec failed with s4ql: wrong number of arguments: 1 for a statement with 0 parameters; then: exec failed with s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
		// SQLite gives the error when it prepares the text, and s4ql in each run of the statement.
		{callPrepareExecRuns, "insert into t9 (v1) values (?)", "prepare, exec several times: exec failed with 1/1: no such table: t9; then: exec failed with 1/1: no such table: t9; then: exec failed with 1/1: no such table: t9"},
		{callPrepareExecRuns, "insert into t1 (v1) values (?1)", "prepare, exec several times: prepare failed with s4ql: not supported: numbered parameter (at offset 28)"},
		{callPrepareExecRuns, "insert into t1 (v1) values (? ?)", `prepare, exec several times: prepare failed with 1/1: near "?": syntax error`},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows ["int64:1"] ["int64:2"]`},
	},
	"a semicolon and a comment after the statement": {
		{callExec, t1 + "; -- c", "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (1);;", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (2); /* c */", "exec: 1 affected, last insert id 0"},
		{callQuery, "select * from t1 ; -- c", `query: columns "v1" of types "INT", rows ["int64:1"] ["int64:2"]`},
	},
	"a write transaction that commits, with arguments and prepared statements": {
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callCommit, "", "commit"},
		{callBegin, "", "begin"},
		{callExec, "insert into t1 (v1) values (1)", "exec: 1 affected, last insert id 0"},
		{callExecArgument, "insert into t1 (v1) values (?)", "exec with an argument: exec failed with 19/1555: UNIQUE constraint failed: t1.v1"},
		{callPrepareExecRuns, "insert into t1 (v1) values (?)", "prepare, exec several times: exec failed with 19/1555: UNIQUE constraint failed: t1.v1; then: 1 affected, last insert id 0; then: exec failed with 19/1555: UNIQUE constraint failed: t1.v1"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows ["int64:1"] ["int64:2"]`},
		{callQueryRow, "select * from t1", oneOf(`query row: row ["int64:1"]`, `query row: row ["int64:2"]`)},
		{callExec, "create table t2 (v2 int primary key) strict, without rowid", "exec: 0 affected, last insert id 0"},
		// Through the pool, mattn's Prepare outside the transaction does not find t2.
		{callStmtExec, "insert into t2 (v2) values (5)", "prepare outside, exec in the transaction: 1 affected, last insert id 0"},
		{callStmtQuery, "select * from t2", `prepare outside, query in the transaction: columns "v2" of types "INT", rows ["int64:5"]`},
		{callStmtExec, "insert into t2 (v2) values (?)", "prepare outside, exec in the transaction: exec failed with s4ql: wrong number of arguments: 0 for a statement with 1 parameter"},
		{callPrepareQuery, "select * from t2", `prepare, query: columns "v2" of types "INT", rows ["int64:5"]`},
		{callCommit, "", "commit"},
		{callQuery, "select * from t2", `query: columns "v2" of types "INT", rows ["int64:5"]`},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows ["int64:1"] ["int64:2"]`},
	},
	"a write transaction that rolls back, with create table and drop table": {
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (1)", "exec: 1 affected, last insert id 0"},
		{callBegin, "", "begin"},
		{callExec, "insert into t1 (v1) values (2)", "exec: 1 affected, last insert id 0"},
		{callExec, "create table t2 (v2 int primary key) strict, without rowid", "exec: 1 affected, last insert id 0"},
		{callExec, "drop table t1", "exec: 1 affected, last insert id 0"},
		{callQuery, "select * from t1", "query: query failed with 1/1: no such table: t1"},
		{callStmtExec, "insert into t1 (v1) values (3)", "prepare outside, exec in the transaction: exec failed with 1/1: no such table: t1"},
		{callExec, "insert into t2 (v2) values (3)", "exec: 1 affected, last insert id 0"},
		{callRollback, "", "rollback"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows ["int64:1"]`},
		{callQuery, "select * from t2", "query: query failed with 1/1: no such table: t2"},
	},
	"a read-only transaction": {
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (1)", "exec: 1 affected, last insert id 0"},
		{callBeginReadOnly, "", "begin read-only"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows ["int64:1"]`},
		{callExec, "insert into t1 (v1) values (2)", "exec: exec failed with 8/8: attempt to write a readonly database"},
		{callExec, "create table t2 (v2 int primary key) strict, without rowid", "exec: exec failed with 8/8: attempt to write a readonly database"},
		{callExec, "drop table t1", "exec: exec failed with 8/8: attempt to write a readonly database"},
		{callExec, "insert into t9 (v1) values (1)", "exec: exec failed with 1/1: no such table: t9"},
		{callExec, t1, "exec: exec failed with 1/1: table t1 already exists"},
		{callExecArgument, "insert into t1 (v1) values (?)", "exec with an argument: exec failed with 8/8: attempt to write a readonly database"},
		{callPrepareExecRuns, "insert into t1 (v1) values (?)", "prepare, exec several times: exec failed with 8/8: attempt to write a readonly database; then: exec failed with 8/8: attempt to write a readonly database; then: exec failed with 8/8: attempt to write a readonly database"},
		{callStmtQuery, "select * from t1", `prepare outside, query in the transaction: columns "v1" of types "INT", rows ["int64:1"]`},
		{callQueryRow, "select * from t1", `query row: row ["int64:1"]`},
		{callCommit, "", "commit"},
		{callExec, "insert into t1 (v1) values (2)", "exec: 1 affected, last insert id 0"},
		{callBeginReadOnly, "Serializable", "begin read-only"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows ["int64:1"] ["int64:2"]`},
		{callRollback, "", "rollback"},
	},
	"rows of a transaction left open": {
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (1)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (2)", "exec: 1 affected, last insert id 0"},
		{callBegin, "", "begin"},
		{callQueryOpen, "select * from t1", `query, rows left open: columns "v1" of types "INT", rows ["int64:1"] ["int64:2"]`},
		{callExec, "insert into t1 (v1) values (3)", "exec: exec failed with " + rowsOpen},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows ["int64:1"] ["int64:2"]`},
		{callQueryOpen, "select * from t1", ""},
		{callExec, "insert into t9 (v1) values (3)", "exec: exec failed with " + rowsOpen},
		{callQueryOpen, "select * from t1", ""},
		{callPrepareExecRuns, "insert into t1 (v1) values (?)", "prepare, exec several times: exec failed with " + rowsOpen + "; then: exec failed with " + rowsOpen + "; then: exec failed with " + rowsOpen},
		{callQueryOpen, "select * from t1", ""},
		{callQueryRow, "select * from t1", "query row: scan failed with " + rowsOpen},
		// The checks of the text and of the arguments come first.
		{callQueryOpen, "select * from t1", ""},
		{callExec, "selec", `exec: exec failed with 1/1: near "selec": syntax error`},
		{callQueryOpen, "select * from t1", ""},
		{callExecArgument, "insert into t1 (v1) values (1)", "exec with an argument: exec failed with s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
		{callQueryOpen, "select * from t1", ""},
		{callExec, "insert into t1 (v1) values (1), (2)", ""},
		{callQueryOpen, "select * from t1", ""},
		{callCommit, "", "commit"},
		{callBeginReadOnly, "", "begin read-only"},
		{callQueryOpen, "select * from t1", `query, rows left open: columns "v1" of types "INT", rows ["int64:1"] ["int64:2"]`},
		{callExec, "insert into t1 (v1) values (3)", "exec: exec failed with " + rowsOpen},
		{callQueryOpen, "select * from t1", ""},
		{callRollback, "", "rollback"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows ["int64:1"] ["int64:2"]`},
	},
	"isolation levels, and a begin in a transaction": {
		{callBegin, "Serializable", "begin"},
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callCommit, "", "commit"},
		{callBegin, "Read Committed", "begin: begin failed with s4ql: not supported: the isolation level Read Committed"},
		{callExec, "insert into t1 (v1) values (1)", "exec: 1 affected, last insert id 0"},
		{callBeginReadOnly, "Linearizable", "begin read-only: begin failed with s4ql: not supported: the isolation level Linearizable"},
		{callBeginReadOnly, "IsolationLevel(99)", "begin read-only: begin failed with s4ql: not supported: the isolation level IsolationLevel(99)"},
		{callBegin, "", "begin"},
		{callExec, "insert into t1 (v1) values (2)", "exec: 1 affected, last insert id 0"},
		// On a pinned connection, s4ql and SQLite refuse a second begin, each with an error of its own;
		// through the pool, the transaction commits first.
		{callBegin, "", "begin: begin failed with " + txOpen},
		{callBeginReadOnly, "Snapshot", "begin read-only: begin failed with s4ql: not supported: the isolation level Snapshot"},
		{callQuery, "select * from t1", ""},
		{callRollback, "", "rollback"},
		{callRollback, "", "rollback"},
		{callQuery, "select * from t1", ""},
	},
	"the example of slice 5, of several columns": {
		{callExec, t3, "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t3 values (1, 'a', 1.5)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t3 (v3, v1, v2) values (2.5, 2, 'b')", "exec: 1 affected, last insert id 0"},
		{callQuery, "select v2, v1, v2 from t3", `query: columns "v2,v1,v2" of types "TEXT,INT,TEXT", rows ["string:a" "int64:1" "string:a"] ["string:b" "int64:2" "string:b"]`},
		{callQuery, "select * from t3", `query: columns "v1,v2,v3" of types "INT,TEXT,REAL", rows ["int64:1" "string:a" "float64:1.5"] ["int64:2" "string:b" "float64:2.5"]`},
		{callPrepareQuery, "select V3, \"V1\" from T3", `prepare, query: columns "v3,v1" of types "REAL,INT", rows ["float64:1.5" "int64:1"] ["float64:2.5" "int64:2"]`},
		{callQueryRow, "select v2 from t3", oneOf(`query row: row ["string:a"]`, `query row: row ["string:b"]`)},
	},
	"the errors of slice 5, through Exec, Query, and Prepare": {
		{callExec, t3, "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t3 values (1, 'a', 1.5)", "exec: 1 affected, last insert id 0"},
		{callExec, "create table t3 (v int primary key, v text not null) strict, without rowid", "exec: exec failed with 1/1: table t3 already exists"},
		{callExec, "create table t4 (v int primary key, V text not null) strict, without rowid", "exec: exec failed with 1/1: duplicate column name: V"},
		{callExec, "insert into t3 values (2, 'b')", "exec: exec failed with 1/1: table t3 has 3 columns but 2 values were supplied"},
		{callQuery, "insert into t3 (v1, v2) values (2, 'b', 2.5)", "query: query failed with 1/1: 3 values for 2 columns"},
		{callExec, "insert into t3 (v1, x) values (2)", "exec: exec failed with 1/1: table t3 has no column named x"},
		{callExec, "insert into t3 (v1, v2) values (2, 'b')", "exec: exec failed with 19/1299: NOT NULL constraint failed: t3.v3"},
		{callExec, "select * from t3", "exec: 0 affected, last insert id 0"},
		{callQuery, "insert into t3 (v2, v3) values ('b', 2.5)", "query: query failed with 19/1299: NOT NULL constraint failed: t3.v1"},
		{callExec, "insert into t3 values (1, 'b', 2.5)", "exec: exec failed with 19/1555: UNIQUE constraint failed: t3.v1"},
		{callQuery, "select v1, x from t3", "query: query failed with 1/1: no such column: x"},
		// mattn's build reads a name in double quotes that is no column as a string, where the pinned
		// build of s4ql's tests gives `no such column`: [pair.checkQuoted] checks such a select against
		// the pinned build's error instead.
		{callQuery, `select v1, "x" from t3`, `query: query failed with 1/1: no such column: "x" - should this be a string literal in single-quotes?`},
		// The first value of another type than its column in the order of the table is refused, also
		// where a column before it in the list has a value of another type.
		{callExec, "insert into t3 (v3, v1, v2) values ('c', 'x', 'b')", "exec: exec failed with s4ql: not supported: a string literal for the column t3.v1 of type INT"},
		// A column without a value comes first.
		{callExec, "insert into t3 (v3, v2) values ('c', 2)", "exec: exec failed with 19/1299: NOT NULL constraint failed: t3.v1"},
		{callQueryRow, `select V1 from t3`, `query row: row ["int64:1"]`},
		{callQueryRow, `select V1, v3, v2 from t3`, `query row: row ["int64:1" "float64:1.5" "string:a"]`},
		// SQLite gives these errors when it prepares the text, and s4ql when the statement runs.
		{callPrepareExec, "insert into t3 values (2, 'b')", "prepare, exec: exec failed with 1/1: table t3 has 3 columns but 2 values were supplied"},
		{callPrepareExec, "insert into t3 (v1) values (2, 'b')", "prepare, exec: exec failed with 1/1: 2 values for 1 columns"},
		{callPrepareQuery, "select x from t3", "prepare, query: query failed with 1/1: no such column: x"},
		{callPrepareExec, "create table t4 (v int primary key, V text not null) strict, without rowid", "prepare, exec: exec failed with 1/1: duplicate column name: V"},
		// This both sides give at the same step.
		{callPrepareExec, "insert into t3 (v1) values (2)", "prepare, exec: exec failed with 19/1299: NOT NULL constraint failed: t3.v2"},
		{callQuery, "select * from t3", `query: columns "v1,v2,v3" of types "INT,TEXT,REAL", rows ["int64:1" "string:a" "float64:1.5"]`},
	},
	"parameters numbered across the values of an insert of several columns": {
		{callExec, "create table k (a text not null, b int primary key) strict, without rowid", "exec: 0 affected, last insert id 0"},
		{callExecArgument, "insert into k (a, b) values ('x', ?)", "exec with an argument: 1 affected, last insert id 0"},
		{callPrepareExecRuns, "insert into k values ('y', ?)", "prepare, exec several times: exec failed with 19/1555: UNIQUE constraint failed: k.b; then: 1 affected, last insert id 0; then: exec failed with 19/1555: UNIQUE constraint failed: k.b"},
		{callExecArgument, "insert into k (b, a) values (?, ?)", "exec with an argument: exec failed with s4ql: wrong number of arguments: 1 for a statement with 2 parameters"},
		{callExecArgument, "insert into k (b, a) values (3, ?)", "exec with an argument: exec failed with s4ql: not supported: argument 1, which is an integer, for the column k.a of type TEXT"},
		{callQuery, "select * from k", `query: columns "a,b" of types "TEXT,INT", rows ["string:x" "int64:1"] ["string:y" "int64:2"]`},
	},
	"a transaction of several columns, with a failed insert that changes nothing": {
		{callExec, t3, "exec: 0 affected, last insert id 0"},
		{callBegin, "", "begin"},
		{callExec, "insert into t3 values (1, 'a', 1.5)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t3 (v1, v2) values (2, 'b')", "exec: exec failed with 19/1299: NOT NULL constraint failed: t3.v3"},
		{callExec, "insert into t3 values (1, 'b', 2.5)", "exec: exec failed with 19/1555: UNIQUE constraint failed: t3.v1"},
		{callQuery, "select v2 from t3", `query: columns "v2" of types "TEXT", rows ["string:a"]`},
		{callCommit, "", "commit"},
		{callBeginReadOnly, "", "begin read-only"},
		{callExec, "insert into t3 values (2, 'b')", "exec: exec failed with 1/1: table t3 has 3 columns but 2 values were supplied"},
		{callExec, "insert into t3 (v1, v2) values (2, 'b')", "exec: exec failed with 8/8: attempt to write a readonly database"},
		{callExec, "insert into t3 values (2, 'b', 'c')", "exec: exec failed with 8/8: attempt to write a readonly database"},
		{callQuery, "select v3, v1 from t3", `query: columns "v3,v1" of types "REAL,INT", rows ["float64:1.5" "int64:1"]`},
		{callRollback, "", "rollback"},
	},
	"a query beside the transaction": {
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (1)", "exec: 1 affected, last insert id 0"},
		{callBegin, "", "begin"},
		{callExec, "insert into t1 (v1) values (2)", "exec: 1 affected, last insert id 0"},
		// On a pinned connection in the transaction, and through the pool outside it.
		{callBeside, "select * from t1", `query beside the transaction: columns "v1" of types "INT", rows ["int64:1"] ["int64:2"]`},
		{callBeside, "insert into t1 (v1) values (3)", "query beside the transaction"},
		{callQueryOpen, "select * from t1", ""},
		{callBeside, "select * from t1", ""},
		{callRollback, "", "rollback"},
		{callBeside, "select * from t1", `query beside the transaction: columns "v1" of types "INT", rows ["int64:1"]`},
	},
	"a query beside a transaction that made its table anew, with a value of another type than the column outside it": {
		{callExec, "create table T5 (v1 int primary key) strict, without rowid", "exec: 0 affected, last insert id 0"},
		{callBegin, "", "begin"},
		{callExec, "drop table t5", "exec: 0 affected, last insert id 0"},
		{callExec, "create table t5 (v1 text primary key) strict, without rowid", "exec: 0 affected, last insert id 0"},
		// On a pinned connection in the transaction, where the column takes the text, and through the
		// pool outside it, whose column of type INT refuses it, by the name that the table has there.
		{callBeside, "select * from t5 where v1 = 'a'", `query beside the transaction: columns "v1" of types "TEXT", rows `},
		{callRollback, "", "rollback"},
	},
	"the example of slice 9, with selects, updates, and deletes through indexes": {
		{callExec, t4, "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t4 values (1, 'a', 1.5, 1)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t4 values (2, 'b', 2.5, 2)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t4 values (3, 'a', 3.5, 1)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t4 values (4, null, 0.5, null)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t4 values (5, 'c', 2.5, 1)", "exec: 1 affected, last insert id 0"},
		// An index on one column, one on a descending column, and one on two columns, on a table with
		// rows.
		{callExec, "create index i1 on t4 (v2)", "exec: 1 affected, last insert id 0"},
		{callPrepareExec, "create index i2 on t4 (v3 desc)", "prepare, exec: 1 affected, last insert id 0"},
		{callQuery, "create index i3 on t4 (v4, v3 desc)", "query"},
		// With =, with a range, and with IS NULL, on the column of each index.
		{callQuery, "select v1 from t4 where v2 = 'a'", `query: columns "v1" of types "INT", rows ["int64:1"] ["int64:3"]`},
		{callQueryRow, "select v1 from t4 where v2 = 'a'", oneOf(`query row: row ["int64:1"]`, `query row: row ["int64:3"]`)},
		{callPrepareQuery, "select v1, v3 from t4 where v3 > 1.5 and v3 <= 3.5", `prepare, query: columns "v1,v3" of types "INT,REAL", rows ["int64:2" "float64:2.5"] ["int64:3" "float64:3.5"] ["int64:5" "float64:2.5"]`},
		{callQuery, "select * from t4 where v2 is null", `query: columns "v1,v2,v3,v4" of types "INT,TEXT,REAL,INT", rows ["int64:4" "<nil>:<nil>" "float64:0.5" "<nil>:<nil>"]`},
		{callQuery, "select v1 from t4 where v4 is null and v3 < 1.0", `query: columns "v1" of types "INT", rows ["int64:4"]`},
		{callQueryArgument, "select v1, v3 from t4 where v4 = ? and v3 < 3.0", `query with an argument: columns "v1,v3" of types "INT,REAL", rows ["int64:1" "float64:1.5"] ["int64:5" "float64:2.5"]`},
		{callQuery, "select v1 from t4 where v2 > 'a' and v2 <= 'c'", `query: columns "v1" of types "INT", rows ["int64:2"] ["int64:5"]`},
		// An update and a delete find their rows through an index, and change the columns of the
		// indexes.
		{callExec, "update t4 set v3 = 9.5 where v2 = 'a'", "exec: 2 affected, last insert id 0"},
		{callQuery, "select v1 from t4 where v3 >= 9.5", `query: columns "v1" of types "INT", rows ["int64:1"] ["int64:3"]`},
		{callExecArgument, "update t4 set v2 = 'd' where v4 = ?", "exec with an argument: 3 affected, last insert id 0"},
		{callQuery, "select v1, v3 from t4 where v2 = 'd'", `query: columns "v1,v3" of types "INT,REAL", rows ["int64:1" "float64:9.5"] ["int64:3" "float64:9.5"] ["int64:5" "float64:2.5"]`},
		{callExec, "delete from t4 where v2 is null", "exec: 1 affected, last insert id 0"},
		{callPrepareExec, "delete from t4 where v3 > 2.0 and v3 < 9.0", "prepare, exec: 2 affected, last insert id 0"},
		{callQuery, "select * from t4", `query: columns "v1,v2,v3,v4" of types "INT,TEXT,REAL,INT", rows ["int64:1" "string:d" "float64:9.5" "int64:1"] ["int64:3" "string:d" "float64:9.5" "int64:1"]`},
		{callExec, "insert into t4 values (6, 'd', 9.5, 1)", "exec: 1 affected, last insert id 0"},
		{callQuery, "select v1 from t4 where v2 = 'd' and v3 = 9.5", `query: columns "v1" of types "INT", rows ["int64:1"] ["int64:3"] ["int64:6"]`},
		// The same selects without the indexes.
		{callExec, "drop index i1", "exec: 1 affected, last insert id 0"},
		{callPrepareExec, "drop index i3", "prepare, exec: 1 affected, last insert id 0"},
		{callQuery, "select v1 from t4 where v2 = 'd'", `query: columns "v1" of types "INT", rows ["int64:1"] ["int64:3"] ["int64:6"]`},
		{callQueryArgument, "select v1 from t4 where v4 = ?", `query with an argument: columns "v1" of types "INT", rows ["int64:1"] ["int64:3"] ["int64:6"]`},
		{callQuery, "drop index i2", "query"},
		{callQuery, "select v1, v3 from t4 where v3 > 1.5", `query: columns "v1,v3" of types "INT,REAL", rows ["int64:1" "float64:9.5"] ["int64:3" "float64:9.5"] ["int64:6" "float64:9.5"]`},
		{callQueryRow, "select v1 from t4 where v3 > 1.5", oneOf(`query row: row ["int64:1"]`, `query row: row ["int64:3"]`, `query row: row ["int64:6"]`)},
	},
	"the errors of slice 9, through Exec, Query, and Prepare": {
		{callExec, "create index i1 on t4 (v2)", "exec: exec failed with 1/1: no such table: main.t4"},
		{callExec, "drop index i1", "exec: exec failed with 1/1: no such index: i1"},
		{callExec, t4, "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t4 values (1, 'a', 1.5, 1)", "exec: 1 affected, last insert id 0"},
		// SQLite's order: the table, a table of the name of the index, an index of the name, and then
		// the columns.
		{callExec, "create index t4 on t9 (x)", "exec: exec failed with 1/1: no such table: main.t9"},
		{callExec, "create index t4 on t4 (x)", "exec: exec failed with 1/1: there is already a table named t4"},
		{callQuery, "create index T4 on t4 (v2)", "query: query failed with 1/1: there is already a table named T4"},
		{callExec, "create index i1 on t4 (v2, x)", "exec: exec failed with 1/1: no such column: x"},
		{callExec, "create index i1 on t4 (v2)", "exec: 1 affected, last insert id 0"},
		{callExec, "create index i1 on t4 (x)", "exec: exec failed with 1/1: index i1 already exists"},
		{callExec, "create index I1 on t4 (v3 desc)", "exec: exec failed with 1/1: index I1 already exists"},
		{callQuery, "create index i1 on t4 (v2)", "query: query failed with 1/1: index i1 already exists"},
		{callExec, "create index i1 on t9 (v2)", "exec: exec failed with 1/1: no such table: main.t9"},
		{callExec, "create table i1 (v1 int primary key) strict, without rowid", "exec: exec failed with 1/1: there is already an index named i1"},
		{callExec, `create table "I1" (v1 int primary key) strict, without rowid`, "exec: exec failed with 1/1: there is already an index named I1"},
		{callExec, "drop index t4", "exec: exec failed with 1/1: no such index: t4"},
		{callExec, "drop index i9", "exec: exec failed with 1/1: no such index: i9"},
		{callQuery, "drop index i9", "query: query failed with 1/1: no such index: i9"},
		{callExec, "drop table i1", "exec: exec failed with 1/1: no such table: i1"},
		{callExecArgument, "create index i2 on t4 (v2)", "exec with an argument: exec failed with s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
		{callExecArgument, "drop index i1", "exec with an argument: exec failed with s4ql: wrong number of arguments: 1 for a statement with 0 parameters"},
		// The clauses of slice 10, which do nothing here and leave the count of changes as it was.
		{callExec, "create index if not exists i1 on t4 (v2)", "exec: 1 affected, last insert id 0"},
		{callExec, "create index if not exists I1 on t4 (x)", "exec: 1 affected, last insert id 0"},
		{callExec, "drop index if exists i9", "exec: 1 affected, last insert id 0"},
		{callExec, "drop index if exists t4", "exec: 1 affected, last insert id 0"},
		{callExec, "create index if not exists i1 on t9 (v2)", "exec: exec failed with 1/1: no such table: main.t9"},
		// What slice 9 does not support.
		{callExec, "create unique index i2 on t4 (v2)", "exec: exec failed with s4ql: not supported: UNIQUE INDEX (at offset 7)"},
		{callExec, "create index i2 on t4 (v2) where v2 is not null", "exec: exec failed with s4ql: not supported: partial index (at offset 27)"},
		{callExec, "create index i2 on t4 (v2, V2)", "exec: exec failed with s4ql: not supported: column named twice (at offset 27)"},
		{callExec, "create index i2 on t4 ()", `exec: exec failed with 1/1: near ")": syntax error`},
		// SQLite gives these errors when it prepares the text, and s4ql when the statement runs.
		{callPrepareExec, "create index i2 on t9 (v2)", "prepare, exec: exec failed with 1/1: no such table: main.t9"},
		{callPrepareExec, "create index t4 on t4 (v2)", "prepare, exec: exec failed with 1/1: there is already a table named t4"},
		{callPrepareExec, "create index i1 on t4 (v3)", "prepare, exec: exec failed with 1/1: index i1 already exists"},
		{callPrepareExec, "create index i2 on t4 (x)", "prepare, exec: exec failed with 1/1: no such column: x"},
		{callPrepareExec, "create table i1 (v1 int primary key) strict, without rowid", "prepare, exec: exec failed with 1/1: there is already an index named i1"},
		{callPrepareExec, "drop index i9", "prepare, exec: exec failed with 1/1: no such index: i9"},
		{callPrepareQuery, "drop index t4", "prepare, query: query failed with 1/1: no such index: t4"},
		// A drop table drops the indexes of its table, and the names are free again.
		{callExec, "drop table t4", "exec: 1 affected, last insert id 0"},
		{callExec, "drop index i1", "exec: exec failed with 1/1: no such index: i1"},
		{callExec, "create table i1 (v1 int primary key, v2 text) strict, without rowid", "exec: 1 affected, last insert id 0"},
		{callExec, "create index t4 on i1 (v2 desc, v1)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into i1 values (1, 'a')", "exec: 1 affected, last insert id 0"},
		{callQuery, "select * from i1 where v2 = 'a'", `query: columns "v1,v2" of types "INT,TEXT", rows ["int64:1" "string:a"]`},
		{callPrepareExec, "drop index t4", "prepare, exec: 1 affected, last insert id 0"},
	},
	"indexes in transactions, with a roll back that undoes a create index and a drop index": {
		{callExec, t4, "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t4 values (1, 'a', 1.5, 1)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t4 values (2, 'b', 2.5, 2)", "exec: 1 affected, last insert id 0"},
		{callExec, "create index i1 on t4 (v2)", "exec: 1 affected, last insert id 0"},
		{callBegin, "", "begin"},
		{callExec, "create index i2 on t4 (v3 desc, v4)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t4 values (3, 'a', 3.5, 1)", "exec: 1 affected, last insert id 0"},
		{callQuery, "select v1 from t4 where v3 > 2.0", `query: columns "v1" of types "INT", rows ["int64:2"] ["int64:3"]`},
		{callExec, "create index i2 on t4 (v2)", "exec: exec failed with 1/1: index i2 already exists"},
		{callExec, "drop index i1", "exec: 1 affected, last insert id 0"},
		{callExec, "drop index i1", "exec: exec failed with 1/1: no such index: i1"},
		{callQuery, "select v1 from t4 where v2 = 'a'", `query: columns "v1" of types "INT", rows ["int64:1"] ["int64:3"]`},
		// On a pinned connection in the transaction, and through the pool outside it, where i1 is and
		// the new row is not.
		{callBeside, "select v1 from t4 where v2 = 'a'", `query beside the transaction: columns "v1" of types "INT", rows ["int64:1"] ["int64:3"]`},
		{callExec, "update t4 set v2 = 'c' where v3 > 2.0", "exec: 2 affected, last insert id 0"},
		{callRollback, "", "rollback"},
		// The roll back undid the create index, the drop index, and the rows.
		{callExec, "drop index i2", "exec: exec failed with 1/1: no such index: i2"},
		{callExec, "create index i1 on t4 (v2)", "exec: exec failed with 1/1: index i1 already exists"},
		{callQuery, "select v1 from t4 where v2 = 'a'", `query: columns "v1" of types "INT", rows ["int64:1"]`},
		{callQuery, "select v1 from t4 where v3 > 2.0", `query: columns "v1" of types "INT", rows ["int64:2"]`},
		{callBegin, "", "begin"},
		// Through the pool, mattn's Prepare outside the transaction does not find i2 for the drop.
		{callStmtExec, "create index i2 on t4 (v3 desc, v4)", "prepare outside, exec in the transaction: 2 affected, last insert id 0"},
		{callStmtExec, "drop index i2", "prepare outside, exec in the transaction: 2 affected, last insert id 0"},
		{callPrepareExec, "create index i2 on t4 (v4, v2 desc)", "prepare, exec: 2 affected, last insert id 0"},
		{callExec, "insert into t4 values (3, 'c', 3.5, 1)", "exec: 1 affected, last insert id 0"},
		// Rows that are read through an index stay open, and s4ql refuses a drop index and a create
		// index while they are, as it refuses every statement of the transaction.
		{callQueryOpen, "select v1, v2 from t4 where v4 >= 1", `query, rows left open: columns "v1,v2" of types "INT,TEXT", rows ["int64:1" "string:a"] ["int64:2" "string:b"] ["int64:3" "string:c"]`},
		{callExec, "drop index i2", "exec: exec failed with " + rowsOpen},
		{callQueryOpen, "select * from t4 where v2 > 'a'", `query, rows left open: columns "v1,v2,v3,v4" of types "INT,TEXT,REAL,INT", rows ["int64:2" "string:b" "float64:2.5" "int64:2"] ["int64:3" "string:c" "float64:3.5" "int64:1"]`},
		{callExec, "create index i3 on t4 (v2)", "exec: exec failed with " + rowsOpen},
		{callExecArgument, "delete from t4 where v4 = ?", "exec with an argument: 2 affected, last insert id 0"},
		{callStmtExec, "drop index i1", "prepare outside, exec in the transaction: 2 affected, last insert id 0"},
		{callStmtQuery, "select v1, v2 from t4 where v4 = 2", `prepare outside, query in the transaction: columns "v1,v2" of types "INT,TEXT", rows ["int64:2" "string:b"]`},
		{callCommit, "", "commit"},
		{callExec, "create index i2 on t4 (v2)", "exec: exec failed with 1/1: index i2 already exists"},
		{callExec, "drop index i1", "exec: exec failed with 1/1: no such index: i1"},
		{callQuery, "select * from t4 where v4 = 2", `query: columns "v1,v2,v3,v4" of types "INT,TEXT,REAL,INT", rows ["int64:2" "string:b" "float64:2.5" "int64:2"]`},
		{callBeginReadOnly, "", "begin read-only"},
		{callExec, "create index i3 on t4 (v2)", "exec: exec failed with 8/8: attempt to write a readonly database"},
		{callExec, "drop index i2", "exec: exec failed with 8/8: attempt to write a readonly database"},
		{callExec, "create index i2 on t4 (v2)", "exec: exec failed with 1/1: index i2 already exists"},
		{callExec, "create index i3 on t4 (x)", "exec: exec failed with 1/1: no such column: x"},
		{callExec, "drop index i9", "exec: exec failed with 1/1: no such index: i9"},
		{callPrepareExec, "drop index i2", "prepare, exec: exec failed with 8/8: attempt to write a readonly database"},
		{callQuery, "select v1 from t4 where v4 > 1", `query: columns "v1" of types "INT", rows ["int64:2"]`},
		{callCommit, "", "commit"},
		{callBegin, "", "begin"},
		{callExec, "drop table t4", "exec: 2 affected, last insert id 0"},
		{callExec, "drop index i2", "exec: exec failed with 1/1: no such index: i2"},
		{callRollback, "", "rollback"},
		{callExec, "drop index i2", "exec: 2 affected, last insert id 0"},
		{callQuery, "select * from t4", `query: columns "v1,v2,v3,v4" of types "INT,TEXT,REAL,INT", rows ["int64:2" "string:b" "float64:2.5" "int64:2"]`},
	},
	"returning on an insert, an update, and a delete, through each call": {
		{callExec, t5, ""},
		{callExec, "create index i5 on t5 (v2)", ""},
		// An insert gives the row as the table stores it, with the default and NULL, and 0.0 for -0.0.
		{callQuery, "insert into t5 (v1, v2) values (1, 'a') returning *", `query: columns "v1,v2,v3,v4,v5" of types "INT,TEXT,REAL,BLOB,INTEGER", rows ["int64:1" "string:a" "float64:0.5" "<nil>:<nil>" "<nil>:<nil>"]`},
		{callExec, "insert into t5 (v1, v2, v4) values (2, 'b', x'00') returning v1", "exec: 1 affected, last insert id 0"},
		{callPrepareQuery, "insert into t5 (v1, v2, v3) values (3, 'a', -0.0) returning v3, V1, v3", `prepare, query: columns "v3,v1,v3" of types "REAL,INT,REAL", rows ["float64:0" "int64:3" "float64:0"]`},
		{callQueryRow, "insert into t5 (v1, v5) values (4, -9223372036854775808) returning v5", `query row: row ["int64:-9223372036854775808"]`},
		{callPrepareExec, "insert into t5 (v1, v4) values (5, x'') returning *", "prepare, exec: 1 affected, last insert id 0"},
		{callQueryArgument, "insert into t5 (v1, v2) values (6, 'c') returning v1", ""},
		// The errors of an insert, after those of the rest of the statement.
		{callQuery, "insert into t5 (v1) values (1) returning v1", "query: query failed with 19/1555: UNIQUE constraint failed: t5.v1"},
		{callQuery, "insert into t5 (v1, v3) values (7, null) returning v1", "query: query failed with 19/1299: NOT NULL constraint failed: t5.v3"},
		{callQuery, "insert into t5 (v1) values (7) returning nope", "query: query failed with 1/1: no such column: nope"},
		{callPrepareQuery, "insert into t5 (nope) values (7) returning nope2", ""},
		{callExec, "insert into t5 (v1, v2) values (7, 1) returning v1", ""},
		// An update of the rows of an index, with the values that they have, of the key, of one row by
		// an argument, and of no row.
		{callQuery, "update t5 set v2 = 'z' where v2 = 'a' returning v1, v2", `query: columns "v1,v2" of types "INT,TEXT", rows ["int64:1" "string:z"] ["int64:3" "string:z"]`},
		{callQuery, "update t5 set v2 = 'z' where v2 = 'z' returning v1", `query: columns "v1" of types "INT", rows ["int64:1"] ["int64:3"]`},
		{callExec, "update t5 set v2 = 'y' where v1 = 3 returning *", "exec: 1 affected, last insert id 0"},
		{callQuery, "update t5 set v1 = 30 where v1 = 3 returning v1, v2", `query: columns "v1,v2" of types "INT,TEXT", rows ["int64:30" "string:y"]`},
		{callQueryArgument, "update t5 set v3 = -0.0 where v1 = ? returning v3, v1", `query with an argument: columns "v3,v1" of types "REAL,INT", rows ["float64:0" "int64:1"]`},
		{callQuery, "update t5 set v3 = 1.5 where v1 > 100 returning *", `query: columns "v1,v2,v3,v4,v5" of types "INT,TEXT,REAL,BLOB,INTEGER", rows `},
		{callPrepareQuery, "update t5 set v5 = 7 where v2 is null returning v1, v5", ""},
		{callQuery, "update t5 set v1 = 1 where v1 = 2 returning v1", "query: query failed with 19/1555: UNIQUE constraint failed: t5.v1"},
		{callQuery, "update t5 set v3 = null returning v1", "query: query failed with 19/1299: NOT NULL constraint failed: t5.v3"},
		{callQuery, "update t5 set v2 = 'q' where nope = 1 returning nope2", "query: query failed with 1/1: no such column: nope"},
		{callQuery, "select * from t5", ""},
		// A delete gives the values that the row had.
		{callQuery, "delete from t5 where v2 = 'z' returning *", ""},
		{callPrepareExecRuns, "delete from t5 where v1 = ? returning v1", "prepare, exec several times: 0 affected, last insert id 0; then: 1 affected, last insert id 0; then: 0 affected, last insert id 0"},
		{callQuery, "delete from t5 where v1 = 1000 returning v1", `query: columns "v1" of types "INT", rows `},
		{callQuery, "delete from t5 returning v1, v1", ""},
		{callQuery, "select * from t5", `query: columns "v1,v2,v3,v4,v5" of types "INT,TEXT,REAL,BLOB,INTEGER", rows `},
	},
	"returning in transactions, with a commit, a roll back, and rows left open": {
		{callExec, t5, ""},
		{callExec, "insert into t5 (v1, v2) values (1, 'a')", ""},
		{callExec, "insert into t5 (v1, v2) values (2, 'b')", ""},
		{callExec, "insert into t5 (v1, v2) values (3, 'b')", ""},
		{callBegin, "", "begin"},
		{callQuery, "insert into t5 (v1, v2) values (4, 'd') returning v1, v2", `query: columns "v1,v2" of types "INT,TEXT", rows ["int64:4" "string:d"]`},
		{callStmtQuery, "update t5 set v2 = 'x' where v1 > 2 returning v1", `prepare outside, query in the transaction: columns "v1" of types "INT", rows ["int64:3"] ["int64:4"]`},
		// The statement runs once, and its outcome is the first row of its rows, which are left open, in
		// either order; the comparison of the rest with SQLite's follows the next call.
		{callQueryOpen, "update t5 set v3 = 2.5 where v2 = 'x' returning v1, v3", oneOf(`query, rows left open: columns "v1,v3" of types "INT,REAL", rows ["int64:3" "float64:2.5"]`, `query, rows left open: columns "v1,v3" of types "INT,REAL", rows ["int64:4" "float64:2.5"]`)},
		{callExec, "delete from t5 where v1 = 1", "exec: exec failed with " + rowsOpen},
		{callQueryOpen, "update t5 set v3 = 3.5 where v1 > 3 returning v1", `query, rows left open: columns "v1" of types "INT", rows ["int64:4"]`},
		{callQuery, "delete from t5 where v1 = 1 returning v1", "query: query failed with " + rowsOpen},
		{callRollback, "", "rollback"},
		{callQuery, "select v1, v2 from t5", `query: columns "v1,v2" of types "INT,TEXT", rows ["int64:1" "string:a"] ["int64:2" "string:b"] ["int64:3" "string:b"]`},
		{callBegin, "", "begin"},
		{callStmtExec, "delete from t5 where v1 >= 2 returning v1", "prepare outside, exec in the transaction: 2 affected, last insert id 0"},
		{callQuery, "update t5 set v2 = 'c' returning *", `query: columns "v1,v2,v3,v4,v5" of types "INT,TEXT,REAL,BLOB,INTEGER", rows ["int64:1" "string:c" "float64:0.5" "<nil>:<nil>" "<nil>:<nil>"]`},
		{callBeside, "select v1 from t5", ""},
		{callCommit, "", "commit"},
		{callQuery, "select v1, v2 from t5", `query: columns "v1,v2" of types "INT,TEXT", rows ["int64:1" "string:c"]`},
		{callBeginReadOnly, "", "begin read-only"},
		{callQuery, "insert into t5 (v1) values (5) returning v1", "query: query failed with 8/8: attempt to write a readonly database"},
		{callExec, "delete from t5 returning *", "exec: exec failed with 8/8: attempt to write a readonly database"},
		{callQuery, "update t5 set v2 = 'q' returning nope", "query: query failed with 1/1: no such column: nope"},
		{callRollback, "", "rollback"},
		{callQuery, "select v1, v2 from t5", `query: columns "v1,v2" of types "INT,TEXT", rows ["int64:1" "string:c"]`},
	},
}

// edges of each type of a column, each as a literal and as an argument of database/sql, in an order
// that is not the order of the keys.
var edges = map[string][]struct {
	literal string
	arg     any
}{
	"int": {
		{"0", 0}, {"9223372036854775807", int64(math.MaxInt64)}, {"-9223372036854775808", int64(math.MinInt64)},
		{"-1", int8(-1)}, {"1", uint16(1)}, {"9223372036854775806", uint64(math.MaxInt64 - 1)},
	},
	"real": {
		{"0.0", 0.0}, {"-0.0", math.Copysign(0, -1)}, {"5e-324", math.SmallestNonzeroFloat64}, {"-5e-324", -math.SmallestNonzeroFloat64},
		{"1.7976931348623157e308", math.MaxFloat64}, {"-1.7976931348623157e308", -math.MaxFloat64},
		{"2.2250738585072014e-308", 0x1p-1022}, {"2.225073858507201e-308", 0x1p-1022 - math.SmallestNonzeroFloat64},
		{"1.5", float32(1.5)}, {"-2.5", -2.5}, {"0.1", 0.1},
	},
	"text": {
		{"''", ""}, {"'a'", "a"}, {"'A'", "A"}, {"'é'", "é"}, {"'日本語'", "日本語"}, {"'🙂'", "🙂"}, {"'it''s'", "it's"},
		{"' '", " "}, {"'a '", "a "}, {"'\U0010ffff'", "\U0010ffff"}, {"'é'", "é"},
	},
	"blob": {
		{"x''", []byte{}}, {"x'00'", []byte{0}}, {"x'0000'", []byte{0, 0}}, {"x'ff'", []byte{0xff}}, {"x'0001'", []byte{0, 1}},
		{"x'6162'", []byte("ab")}, {"x'fffe00'", []byte{0xff, 0xfe, 0}},
	},
}

func TestTypes(t *testing.T) {
	w := newWorld(t)
	types := map[string][]string{"int": {"int", "integer"}, "real": {"real"}, "text": {"text"}, "blob": {"blob"}}
	for _, pinned := range []bool{true, false} {
		for _, kind := range slices.Sorted(maps.Keys(edges)) {
			for _, typ := range types[kind] {
				t.Run(fmt.Sprintf("should give what SQLite gives for the edges of %v, refuse the values of the other types, and replay them, on a pinned connection: %v", typ, pinned), func(t *testing.T) {
					p := newPair(t, w, pinned, nil)
					step := func(src string, c call, runs ...[]any) {
						t.Helper()
						if len(runs) == 0 {
							runs = [][]any{nil}
						}
						if _, failure := p.stepRuns(src, c, true, runs); failure != "" {
							t.Fatal(failure)
						}
					}
					for _, table := range []string{"t", "p"} {
						step(fmt.Sprintf("create table %v (v %v primary key) strict, without rowid", table, typ), callExec)
					}
					var runs [][]any
					// Each value twice: the second time it, or -0.0 after 0.0, is a key that exists.
					for range 2 {
						for _, e := range edges[kind] {
							step("insert into t (v) values ("+e.literal+")", callExec)
							step("insert into t (v) values (?)", callExecArgument, []any{e.arg})
							runs = append(runs, []any{e.arg})
						}
					}
					step("insert into p (v) values (?)", callPrepareExecRuns, runs...)
					for other, es := range edges {
						if other != kind {
							step("insert into t (v) values ("+es[1].literal+")", callExec)
							step("insert into t (v) values (?)", callExecArgument, []any{es[1].arg})
						}
					}
					step("select * from t", callQuery)
					step("select * from p", callPrepareQuery)
					is.Equal(t, "", p.compareTables())
					p.restart(true)
					is.Equal(t, "", p.compareTables())
					step("insert into p (v) values (?)", callPrepareExecRuns, runs[:2]...)
					step("select * from P", callQuery)
					p.close()
				})
			}
		}
	}
}

// literal of the type typ for the number k, and the argument of database/sql with its value: k as an
// integer, k and a half as a real, k as text, and the byte k as a blob.
func literal(typ string, k int) (string, any) {
	switch typ {
	case "int", "integer":
		return fmt.Sprint(k), int64(k)
	case "real":
		return fmt.Sprint(k, ".5"), float64(k) + 0.5
	case "text":
		return fmt.Sprintf("'%v'", k), fmt.Sprint(k)
	}
	return fmt.Sprintf("x'%02x'", k), []byte{byte(k)}
}

func TestColumns(t *testing.T) {
	w := newWorld(t)
	// The names of the columns by position: as the create table writes them, and in another spelling
	// of the same names, which the other statements write.
	created, spelled := []string{"a", `"B b"`, "C"}, []string{"A", `"b B"`, "c"}
	for _, types := range [][]string{{"int", "text"}, {"blob", "real"}, {"text", "integer", "real"}, {"real", "blob", "int"}, {"integer", "text", "blob"}} {
		for key := range types {
			// Every column NOT NULL, every column but the key taking NULL, and for three columns the
			// column after the key alone, so that a column that takes NULL is of each type at each
			// position, beside one that does not.
			nullables := [][]int{nil, slices.DeleteFunc([]int{0, 1, 2}[:len(types)], func(i int) bool { return i == key })}
			if len(types) == 3 {
				nullables = append(nullables, []int{(key + 1) % 3})
			}
			for _, nullable := range nullables {
				for _, pinned := range []bool{true, false} {
					t.Run(fmt.Sprintf("should give what SQLite gives for a table of %v with the key at %v and NULL at %v, on a pinned connection: %v", strings.Join(types, ", "), key, nullable, pinned), func(t *testing.T) {
						columnsSteps(t, w, types, key, nullable, pinned, created, spelled)
					})
				}
			}
		}
	}
}

// columnsSteps runs the steps of [TestColumns] for a table of the types, with the key at the position
// key, the columns at the positions nullable taking NULL, and the names created and spelled.
func columnsSteps(t *testing.T, w *world, types []string, key int, nullable []int, pinned bool, created, spelled []string) {
	t.Helper()
	n := len(types)
	p := newPair(t, w, pinned, nil)
	step := func(src string, c call, runs ...[]any) {
		t.Helper()
		if len(runs) == 0 {
			runs = [][]any{nil}
		}
		if _, failure := p.stepRuns(src, c, true, runs); failure != "" {
			t.Fatal(failure)
		}
	}
	var columns []string
	for i, typ := range types {
		c := created[i] + " " + typ + " not null"
		switch {
		case i == key:
			c = created[i] + " " + typ + " primary key"
		case slices.Contains(nullable, i):
			c = created[i] + " " + typ
		}
		columns = append(columns, c)
	}
	create := "create table t (" + strings.Join(columns, ", ") + ") strict, without rowid"
	// insert of the number k into the columns at the positions, through literals, or
	// through parameters with the arguments of the run.
	insert := func(positions []int, k int, parameters bool) (string, []any) {
		var names, vs []string
		var args []any
		for _, i := range positions {
			l, a := literal(types[i], k)
			names = append(names, spelled[i])
			if parameters {
				l = "?"
				args = append(args, a)
			}
			vs = append(vs, l)
		}
		return fmt.Sprintf("insert into t (%v) values (%v)", strings.Join(names, ", "), strings.Join(vs, ", ")), args
	}
	// insertNull is insert with NULL for the column at the position at, as the literal NULL or as the
	// argument null, and a value of another type than its column for the column at the position
	// wrong, if it is not -1.
	insertNull := func(positions []int, k, at, wrong int, null any, parameters bool) (string, []any) {
		var names, vs []string
		var args []any
		for _, i := range positions {
			l, a := literal(types[i], k)
			switch {
			case i == at:
				l, a = "NULL", null
			case i == wrong && types[i] == "text":
				l, a = literal("int", k)
			case i == wrong:
				l, a = literal("text", k)
			}
			names = append(names, spelled[i])
			if parameters {
				l = "?"
				args = append(args, a)
			}
			vs = append(vs, l)
		}
		return fmt.Sprintf("insert into t (%v) values (%v)", strings.Join(names, ", "), strings.Join(vs, ", ")), args
	}
	all, reversed := make([]int, n), make([]int, n)
	for i := range n {
		all[i], reversed[i] = i, n-1-i
	}
	// nulls inserts NULL into each column: as a literal and as the kinds of nil that database/sql
	// hands the driver, in the order of the table and in another, into a column that takes NULL or
	// one that does not, also with a key that exists, beside a value of another type, and beside a
	// column left out. Every k from base on is for a new key.
	nulls := func(base int) {
		for at := range n {
			k, other := base+10*at, (at+1)%n
			src, _ := insertNull(all, k, at, -1, nil, false)
			step(src, callExec)
			src, _ = insertNull(all, 1, at, -1, nil, false)
			step(src, callExec)
			src, _ = insertNull(all, k+1, at, other, nil, false)
			step(src, callPrepareExec)
			var runs [][]any
			for j, null := range []any{nil, sql.NullString{}, (*int64)(nil)} {
				_, args := insertNull(reversed, k+2+j, at, -1, null, true)
				runs = append(runs, args)
			}
			src, _ = insertNull(reversed, 0, at, -1, nil, true)
			step(src, callPrepareExecRuns, runs...)
			src, args := insertNull(all, k+5, at, other, sql.Null[[]byte]{}, true)
			step(src, callExecArgument, args)
			for q := range n {
				if q != at {
					src, _ := insertNull(slices.DeleteFunc(slices.Clone(all), func(i int) bool { return i == q }), k+6+q, at, -1, nil, false)
					step(src, callExec)
				}
			}
		}
	}

	step(create, callExec)
	step(create, callPrepareExec)
	src, _ := insert(all, 4, false)
	step(src, callExec)
	src, _ = insert(reversed, 1, false)
	step(src, callPrepareExec)
	src, args := insert(reversed, 3, true)
	var runs [][]any
	for _, k := range []int{2, 5, 2} {
		_, a := insert(reversed, k, true)
		runs = append(runs, a)
	}
	step(src, callExec, args)
	step(src, callPrepareExecRuns, runs...)
	step("select * from t", callQuery)
	step(fmt.Sprintf("select %v, %v, %v from T", spelled[n-1], created[0], created[n-1]), callPrepareQuery)
	step(fmt.Sprintf("select %v from t", spelled[key]), callQueryRow)
	step("select x from t", callQuery)
	step("select x from t", callPrepareQuery)
	step("insert into t values (1)", callExec)
	step("insert into t values (1)", callPrepareExec)
	src, _ = insert(all[1:], 6, false)
	step(strings.Replace(src, ") values (", ", "+spelled[0]+") values (", 1), callPrepareExec)
	step(strings.Replace(src, "insert into t (", "insert into t ("+spelled[0]+", ", 1), callExec)
	for left := range n {
		src, _ := insert(slices.DeleteFunc(slices.Clone(all), func(i int) bool { return i == left }), 7, false)
		step(src, callExec)
		step(src, callPrepareExec)
	}
	src, _ = insert(all, 1, false)
	step(src, callQuery)
	nulls(20)
	step("select * from t", callQuery)

	// In a transaction, a statement that fails changes nothing, also with a roll back of
	// the transaction after it.
	for _, end := range []call{callCommit, callRollback} {
		step("", callBegin)
		src, _ = insert(all, 8, false)
		step(src, callExec)
		src, _ = insert(all[:n-1], 9, false)
		step(src, callExec)
		src, _ = insert(all, 8, false)
		step(src, callExec)
		src, _ = insertNull(all, 60, n-1, -1, nil, false)
		step(src, callExec)
		step("select * from t", callQuery)
		step("", end)
	}
	step("", callBeginReadOnly)
	src, _ = insert(all, 10, false)
	step(src, callExec)
	src, _ = insert(all[:n-1], 10, false)
	step(src, callExec)
	step("insert into t values (1)", callExec)
	step("", callRollback)

	is.Equal(t, "", p.finish())
	is.Equal(t, "", p.compareTables())
	p.restart(true)
	is.Equal(t, "", p.compareTables())
	step(fmt.Sprintf("select %v from t", strings.Join(slices.Collect(func(yield func(string) bool) {
		for _, i := range reversed {
			if !yield(spelled[i]) {
				return
			}
		}
	}), ", ")), callQuery)
	// The table of the replay takes NULL where the table did, and nowhere else.
	nulls(120)
	step("select * from t", callQuery)
	is.Equal(t, "", p.compareTables())
	p.close()
}

func TestSequences(t *testing.T) {
	w := newWorld(t)
	for _, name := range slices.Sorted(maps.Keys(sequences)) {
		for _, pinned := range []bool{true, false} {
			t.Run(fmt.Sprintf("should give what SQLite gives for %v, on a pinned connection: %v", name, pinned), func(t *testing.T) {
				p := newPair(t, w, pinned, nil)
				for _, s := range sequences[name] {
					got, failure := p.step(s.src, s.call, false)
					if failure != "" {
						t.Fatal(failure)
					}
					// Through the pool, the count of changes is that of whichever connection database/sql
					// takes, so the outcomes written down hold on a pinned connection. A new line separates
					// the outcomes of [oneOf], and an outcome whose text held one would match none of them.
					if wants := strings.Split(s.want, "\n"); s.want != "" && pinned && !slices.Contains(wants, got.String()) {
						t.Fatalf("%q gives\n%v\nand the test expects\n%v", s.src, got, strings.Join(wants, "\nor\n"))
					}
				}
				is.Equal(t, "", p.finish())
				is.Equal(t, "", p.compareTables())
				p.close()
			})
		}
	}
}

func TestPair(t *testing.T) {
	t.Run("should begin a write transaction of SQLite as the writer, as s4ql does", func(t *testing.T) {
		p := newPair(t, newWorld(t), false, nil)
		_, err := p.sqlite.ExecContext(t.Context(), t1)
		is.NotError(t, err)
		tx, err := p.o.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		defer func() { is.NotError(t, tx.Rollback()) }()
		// The connection waits for no lock, and fails at once.
		c, err := p.sqlite.Conn(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, c.Close()) }()
		_, err = c.ExecContext(t.Context(), "pragma busy_timeout = 0")
		is.NotError(t, err)
		_, err = c.ExecContext(t.Context(), "insert into t1 (v1) values (1)")
		is.Equal(t, "5/5: database is locked", mattnText(t, err))
		p.close()
	})

	t.Run("should run goqite's send and receive with RETURNING as far as the slices take them, with arguments and prepared statements, and keep their changes after a restart with an empty cache", func(t *testing.T) {
		w := newWorld(t)
		for _, pinned := range []bool{true, false} {
			p := newPair(t, w, pinned, nil)
			goqite := "create table goqite (id text primary key, created text not null, updated text not null, queue text not null, " +
				"body blob not null, timeout text not null, received int not null default 0, priority int not null default 0) strict, without rowid"
			for _, src := range []string{goqite, "create index goqite_queue_priority_created_idx on goqite (queue, priority desc, created)"} {
				_, failure := p.step(src, callExec, false)
				is.Equal(t, "", failure)
			}
			now := "2026-11-23T12:00:00.000Z"
			// goqite's send, with the id from the caller, since the default of the id is an expression.
			send := "insert into goqite (id, created, updated, queue, body, timeout, priority) values (?, ?, ?, ?, ?, ?, ?) returning id"
			for n, id := range []string{"m_1", "m_2", "m_3"} {
				got, failure := p.stepRuns(send, []call{callQuery, callPrepareQuery, callQueryRow}[n], false, [][]any{{id, now, now, "default", []byte("body " + id), now, n % 2}})
				is.Equal(t, "", failure)
				is.True(t, len(got.rows) == 1 && strings.Contains(got.rows[0], id), got.String())
			}
			// goqite's receive, with the id that its select of the next message gives, since a scalar
			// subquery and arithmetic are not supported.
			next := "select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1"
			got, failure := p.stepRuns(next, callQuery, false, [][]any{{"default", now, 3}})
			is.Equal(t, "", failure)
			is.EqualSlice(t, []string{`["string:m_2"]`}, got.rows)
			receive := "update goqite set timeout = ?, received = ? where id = ? returning id, body"
			got, failure = p.stepRuns(receive, callPrepareQuery, false, [][]any{{"2026-11-23T12:01:00.000Z", 1, "m_2"}})
			is.Equal(t, "", failure)
			is.EqualSlice(t, []string{`["string:m_2" "[]uint8:[98 111 100 121 32 109 95 50]"]`}, got.rows)
			is.Equal(t, "", p.compareTables())
			p.restart(true)
			is.Equal(t, "", p.compareTables())
			p.close()
		}
	})

	t.Run("should take the engine's refusal of an argument of LIMIT that is no integer from 0 by the rule, after the errors of the names, and run one that is", func(t *testing.T) {
		w := newWorld(t)
		for _, pinned := range []bool{true, false} {
			p := newPair(t, w, pinned, nil)
			for _, src := range []string{t4, "insert into t4 values (1, 'a', 1.5, null)", "insert into t4 values (2, null, 0.5, 3)", "insert into t4 values (3, 'a', 0.5, 3)"} {
				_, failure := p.step(src, callExec, false)
				is.Equal(t, "", failure)
			}
			for _, c := range []struct {
				src  string
				args []any
				want string
			}{
				{src: "select v1 from t4 order by v2 limit ?", args: []any{nil}, want: "NULL"},
				{src: "select * from t4 limit ?", args: []any{2.0}, want: "a real"},
				{src: "select v1 from t4 order by v3 desc limit ?", args: []any{"2"}, want: "text"},
				{src: "select v1 from t4 order by v3 desc limit ?", args: []any{[]byte("2")}, want: "a blob"},
				{src: "select v1 from t4 where v1 > ? order by v4 limit ?", args: []any{int64(0), int64(-1)}, want: "a negative integer"},
				{src: "select v1 from t4 where v1 > ? order by v4 limit ?", args: []any{int64(0), int(-2)}, want: "a negative integer"},
				// The errors of the names and the type of a condition come first.
				{src: "select v1 from t4 order by x limit ?", args: []any{nil}},
				{src: "select x from t4 order by v1 limit ?", args: []any{2.0}},
				{src: "select v1 from t4 where v1 > ? limit ?", args: []any{"a", nil}},
				// An integer from 0 runs.
				{src: "select v1 from t4 order by v3, v1 desc limit ?", args: []any{int64(2)}},
				{src: "select v1 from t4 where v1 > ? order by v2 desc limit ?", args: []any{int64(1), uint8(0)}},
			} {
				got, failure := p.stepRuns(c.src, callQuery, false, [][]any{c.args})
				is.Equal(t, "", failure, c.src)
				if c.want != "" {
					is.Equal(t, fmt.Sprintf("s4ql: not supported: argument %v, which is %v, for LIMIT, which takes an integer from 0", len(c.args), c.want), got.err, c.src)
				} else {
					is.True(t, !strings.Contains(got.err, "for LIMIT"), c.src, got.err)
				}
			}
			p.close()
		}
	})

	t.Run("should run upserts as SQLite does, through Exec, Query with RETURNING, Prepare, in transactions, and among several statements, and judge their refusals and evaluation errors by s4ql's rules", func(t *testing.T) {
		w := newWorld(t)
		for _, pinned := range []bool{true, false} {
			p := newPair(t, w, pinned, nil)
			for _, src := range []string{t4, "insert into t4 values (1, 'a', 1.5, null)", "insert into t4 values (2, 'b', 2.5, 7)"} {
				_, failure := p.step(src, callExec, false)
				is.Equal(t, "", failure)
			}
			for _, c := range []struct {
				src  string
				call call
				runs [][]any
				want string
			}{
				{"insert into t4 values (3, 'c', 3.5, 3) on conflict (v1) do nothing", callExec, nil, "exec: 1 affected, last insert id 0"},
				{"insert into t4 values (3, 'd', 4.5, 4) on conflict (v1) do nothing", callExec, nil, "exec: 0 affected, last insert id 0"},
				{"insert into t4 (v1, v3) values (1, 0.5) on conflict (v1) do update set v2 = excluded.v2 || '', v4 = 1 returning *", callQuery, nil, `query: columns "v1,v2,v3,v4" of types "INT,TEXT,REAL,INT", rows ["int64:1" "<nil>:<nil>" "float64:1.5" "int64:1"]`},
				{"insert into t4 (v1, v2, v3) values (1, 'B', 0.5) on conflict (v1) do update set v2 = lower(excluded.v2) || v2 returning v2", callQuery, nil, `query: columns "v2" of types "TEXT", rows ["<nil>:<nil>"]`},
				{"insert into t4 (v1, v2, v3) values (1, 'B', 0.5) on conflict (v1) do update set v2 = lower(excluded.v2) returning v2", callQuery, nil, `query: columns "v2" of types "TEXT", rows ["string:b"]`},
				{"insert into t4 (v1, v2, v3) values (1, 'b', 0.5) on conflict (v1) do update set v2 = excluded.v2, v4 = 1 returning *", callQuery, nil, `query: columns "v1,v2,v3,v4" of types "INT,TEXT,REAL,INT", rows ["int64:1" "string:b" "float64:1.5" "int64:1"]`},
				{"insert into t4 (v1, v3, v4) values (?, ?, ?) on conflict (v1) do update set v4 = v4 + excluded.v4 where v4 < ? returning v1, v4", callPrepareQuery, [][]any{{int64(2), 0.5, int64(10), int64(100)}}, `prepare, query: columns "v1,v4" of types "INT,INT", rows ["int64:2" "int64:17"]`},
				{"insert into t4 (v1, v3, v4) values (?, ?, ?) on conflict (v1) do update set v4 = v4 + excluded.v4 where v4 < ?", callPrepareExecRuns, [][]any{{int64(2), 0.5, int64(10), int64(100)}, {int64(2), 0.5, int64(10), int64(20)}, {int64(5), 0.5, int64(1), int64(0)}, {int64(2), 0.5, int64(9223372036854775807), int64(100)}},
					"prepare, exec several times: 1 affected, last insert id 0; then: 0 affected, last insert id 0; then: 1 affected, last insert id 0; then: exec failed with s4ql: integer overflow in + (at offset 81)"},
				{"insert into t4 (v1, v3) values (1, 0.5) on conflict (v1) do update set v4 = excluded.v3", callExec, nil, "exec: exec failed with s4ql: not supported: the column excluded.v3 of type REAL for the column t4.v4 of type INT"},
				{"insert into t4 (v1, v3) values (1, 0.5) on conflict (v1) do update set v2 = lower(excluded.v2)", callPrepareExec, nil, "prepare, exec: 1 affected, last insert id 0"},
				{"insert into t4 (v1, v3) values (?, ?) on conflict (v1) do update set v2 = lower(excluded.v2)", callPrepareExec, [][]any{{int64(1), math.NaN()}}, "prepare, exec: exec failed with s4ql: not supported: argument 2, which is a real that is not a number"},
				// Text of both rows, prepared and run several times, with NULL, and its refusals.
				{"insert into t4 (v1, v2, v3) values (?, ?, 0.5) on conflict (v1) do update set v2 = 'm_' || lower(hex(?)) || v2 where excluded.v2 || '' = ? returning v2", callPrepareQuery, [][]any{{int64(2), "q", []byte{0xab}, "q"}}, `prepare, query: columns "v2" of types "TEXT", rows ["string:m_abb"]`},
				{"insert into t4 (v1, v2, v3) values (?, ?, 0.5) on conflict (v1) do update set v2 = v2 || excluded.v2", callPrepareExecRuns, [][]any{{int64(2), "c"}, {int64(2), nil}, {int64(1), "d"}, {int64(2), "e"}},
					"prepare, exec several times: 1 affected, last insert id 0; then: 1 affected, last insert id 0; then: 1 affected, last insert id 0; then: 1 affected, last insert id 0"},
				{"insert into t4 (v1, v3) values (2, 0.5) on conflict (v1) do update set v4 = lower(excluded.v2)", callExec, nil, "exec: exec failed with s4ql: not supported: an expression of type text for the column t4.v4 of type INT"},
				{"insert into t4 (v1, v3) values (2, 0.5) on conflict (v1) do update set v2 = 'a' where lower(v2) = excluded.v3", callExec, nil, "exec: exec failed with s4ql: not supported: an expression of type real compared with an expression of type text"},
				{"insert into t4 (v1, v3) values (2, 0.5) on conflict (v1) do update set v2 = ? || v2", callPrepareExecRuns, [][]any{{int64(1)}, {2.5}},
					"prepare, exec several times: exec failed with s4ql: not supported: argument 1 in a concatenation, which is an integer (at offset 76); then: exec failed with s4ql: not supported: argument 1 in a concatenation, which is a real (at offset 76)"},
				{"insert into t4 (v1, v3) values (?, ?) on conflict (v1) do update set v2 = lower(excluded.v2)", callExec, [][]any{{int64(1), math.NaN()}}, "exec: exec failed with s4ql: not supported: argument 2, which is a real that is not a number"},
				{"insert into t4 (v1) values (1) on conflict (v1) do update set v4 = 9223372036854775807 + 1, v3 = v3 * 2.0", callExec, nil, "exec: exec failed with s4ql: integer overflow in + (at offset 67)"},
				{"insert into t4 (v1, v3) values (1, 0.5) on conflict (v2) do nothing", callExec, nil, "exec: exec failed with 1/1: ON CONFLICT clause does not match any PRIMARY KEY or UNIQUE constraint"},
				{"insert into t4 (v1, v3) values (1, 0.5) on conflict (v1) do update set v4 = 9223372036854775807 + v1", callExec, nil, "exec: exec failed with s4ql: integer overflow in + (at offset 76)"},
				{"insert into t4 (v1, v3) values (1, null) on conflict (v1) do nothing", callExec, nil, "exec: exec failed with 19/1299: NOT NULL constraint failed: t4.v3"},
				{"", callBegin, nil, ""},
				{"insert into t4 (v1, v2, v3) values (1, 'q', 0.5) on conflict (v1) do update set v2 = excluded.v2 returning v2", callQuery, nil, `query: columns "v2" of types "TEXT", rows ["string:q"]`},
				{"insert into t4 (v1, v2, v3) values (9, 'q', 0.5) on conflict (v1) do nothing", callExec, nil, "exec: 1 affected, last insert id 0"},
				{"", callRollback, nil, ""},
				{"", callBeginReadOnly, nil, ""},
				{"insert into t4 (v1, v2, v3) values (1, 'q', 0.5) on conflict (v1) do nothing", callExec, nil, "exec: exec failed with 8/8: attempt to write a readonly database"},
				{"", callRollback, nil, ""},
				{"insert into t4 (v1, v2, v3) values (1, 'r', 0.5) on conflict (v1) do nothing; insert into t4 (v1, v2, v3) values (1, 's', 0.5) on conflict (v1) do update set v2 = excluded.v2 || excluded.v2", callExec, nil, "exec: 1 affected, last insert id 0"},
				{"insert into t4 (v1, v2, v3) values (1, 'r', 0.5) on conflict (v1) do nothing; insert into t4 (v1, v2, v3) values (1, 's', 0.5) on conflict (v1) do update set v2 = excluded.v2; insert into t4 (v1, v2, v3) values (8, 't', 0.5) on conflict (v1) do nothing", callExec, nil, "exec: 1 affected, last insert id 0"},
			} {
				runs := c.runs
				if runs == nil {
					runs = fixedRuns(c.call)
				}
				got, failure := p.stepRuns(c.src, c.call, false, runs)
				is.Equal(t, "", failure, c.src)
				if c.want != "" {
					is.Equal(t, c.want, got.String(), c.src)
				}
			}
			is.Equal(t, "", p.compareTables())
			p.restart(true)
			is.Equal(t, "", p.compareTables())
			p.close()
		}
	})

	t.Run("should fail an upsert's refusal that is none of the rules', a refusal that was due and not given, and an evaluation error that SQLite does not justify", func(t *testing.T) {
		p := newPair(t, newWorld(t), true, nil)
		for _, src := range []string{t4, "insert into t4 values (1, 'a', 1.5, null)", "insert into t4 values (2, 'b', 2.5, 7)"} {
			_, failure := p.step(src, callExec, false)
			is.Equal(t, "", failure)
		}
		// check compares s4ql's outcome got of src with the runs, and requires that it fails where fails.
		check := func(src string, runs [][]any, got outcome, fails bool) {
			t.Helper()
			failure := p.compare(src, callExec, false, runs, got, p.o)
			is.Equal(t, fails, failure != "", fmt.Sprint(src, " with the runs ", runs, " and the outcome ", got, ": ", failure))
		}
		const refused = "insert into t4 (v1, v3) values (1, 0.5) on conflict (v1) do update set v4 = excluded.v3"
		refusal := run(t, p.s, callExec, refused, [][]any{nil}, s4qlText)
		is.Equal(t, "exec: exec failed with s4ql: not supported: the column excluded.v3 of type REAL for the column t4.v4 of type INT", refusal.String())
		check(refused, [][]any{nil}, refusal, false)

		// A refusal that is none of the rules', of a statement that the rules refuse, and of one that
		// they take.
		wrong := refusal
		wrong.err = "s4ql: not supported: the column excluded.v2 of type TEXT for the column t4.v4 of type INT"
		check(refused, [][]any{nil}, wrong, true)
		check("insert into t4 (v1, v3) values (1, 0.5) on conflict (v1) do update set v4 = excluded.v4", [][]any{nil}, refusal, true)

		// An evaluation error of an expression with a column, which SQLite justifies on the row of the
		// conflict and not for another value or where the table has no row of the key, and of a constant.
		const onRow = "insert into t4 (v1, v3, v4) values (?, 0.5, ?) on conflict (v1) do update set v4 = v4 + excluded.v4"
		overflow := run(t, p.s, callExec, onRow, [][]any{{int64(2), int64(math.MaxInt64)}}, s4qlText)
		is.Equal(t, "exec: exec failed with s4ql: integer overflow in + (at offset 83)", overflow.String())
		check(onRow, [][]any{{int64(2), int64(math.MaxInt64)}}, overflow, false)
		check(onRow, [][]any{{int64(2), int64(1)}}, overflow, true)
		check(onRow, [][]any{{int64(9), int64(math.MaxInt64)}}, overflow, true)
		const constant = "insert into t4 (v1, v3, v4) values (9, 0.5, 1) on conflict (v1) do update set v4 = ? + 1"
		overflow = run(t, p.s, callExec, constant, [][]any{{int64(math.MaxInt64)}}, s4qlText)
		is.True(t, evaluated(overflow), overflow.String())
		check(constant, [][]any{{int64(math.MaxInt64)}}, overflow, false)
		check(constant, [][]any{{int64(1)}}, overflow, true)

		// A refusal of text, by the rules of text in an update, and one that the rules do not give.
		const text = "insert into t4 (v1, v3) values (1, 0.5) on conflict (v1) do update set v2 = excluded.v4 || v2"
		refusal = run(t, p.s, callExec, text, [][]any{nil}, s4qlText)
		is.Equal(t, "exec: exec failed with s4ql: not supported: column of type INT in a concatenation (at offset 76)", refusal.String())
		check(text, [][]any{nil}, refusal, false)
		wrong = refusal
		wrong.err = "s4ql: not supported: an expression of type text for the column t4.v2 of type TEXT"
		check(text, [][]any{nil}, wrong, true)

		// No error where a refusal was due, where SQLite converts the value that s4ql refuses, of DO
		// UPDATE and of the proposed row, also in a run of a prepared statement after one with an
		// evaluation error. They come last, since a rule that missed them would run them in SQLite.
		done := outcome{call: callExec, affected: "1", lastID: "0"}
		check(text, [][]any{nil}, done, true)
		check("insert into t4 (v1, v3) values (9, 0.5) on conflict (v1) do update set v4 = lower(excluded.v2)", [][]any{nil}, done, true)
		const due = "insert into t4 (v1, v3) values (1, 0.5) on conflict (v1) do update set v2 = 'a' where lower(v2) = ?"
		failure := p.compare(due, callExec, false, [][]any{{int64(1)}}, outcome{call: callExec, affected: "0", lastID: "0"}, p.o)
		is.True(t, strings.Contains(failure, "the rules refuse it"), failure)
		check("insert into t4 (v1, v3) values (1, 2.0) on conflict (v1) do update set v4 = excluded.v3", [][]any{nil}, done, true)
		check("insert into t4 (v1, v3, v4) values (9, 0.5, 2.0) on conflict (v1) do nothing", [][]any{nil}, done, true)
		const runsOf = "insert into t4 (v1, v3, v4) values (?, ?, ?) on conflict (v1) do update set v4 = v4 + excluded.v4"
		runs := [][]any{{int64(2), 0.5, int64(math.MaxInt64)}, {int64(1), 0.5, 2.0}}
		failed := run(t, p.s, callExec, runsOf, runs[:1], s4qlText)
		is.True(t, evaluated(failed), failed.String())
		failed.call, failed.next = callPrepareExecRuns, []outcome{{affected: "1", lastID: "0"}}
		failure = p.compare(runsOf, callPrepareExecRuns, false, runs, failed, p.o)
		is.True(t, strings.Contains(failure, "the rules refuse it"), failure)
	})

	t.Run("should run goqite's receive with its scalar subquery, through Query and a prepared statement run several times, with rows that tie broken by goqite's index, and none on an empty queue", func(t *testing.T) {
		w := newWorld(t)
		for _, pinned := range []bool{true, false} {
			p := newPair(t, w, pinned, nil)
			goqite := "create table goqite (id text primary key, created text not null, updated text not null, queue text not null, " +
				"body blob not null, timeout text not null, received int not null default 0, priority int not null default 0) strict, without rowid"
			for _, src := range []string{goqite, "create index goqite_queue_priority_created_idx on goqite (queue, priority desc, created)"} {
				_, failure := p.step(src, callExec, false)
				is.Equal(t, "", failure)
			}
			now := "2026-11-23T12:00:00.000Z"
			send := "insert into goqite (id, created, updated, queue, body, timeout, priority) values (?, ?, ?, ?, ?, ?, ?)"
			// m_2 and m_3 tie under the ORDER BY of the receive, which the index orders by their keys.
			for _, m := range []struct {
				id, created string
				priority    int
			}{{"m_1", now, 0}, {"m_3", now, 1}, {"m_2", now, 1}, {"m_4", "2026-11-23T11:00:00.000Z", 0}} {
				_, failure := p.stepRuns(send, callExec, false, [][]any{{m.id, m.created, now, "default", []byte("body " + m.id), now, m.priority}})
				is.Equal(t, "", failure)
			}
			// goqite's receive without its arithmetic, which slice 15 does not have.
			receive := "update goqite set timeout = ? where queue = ? and id = (select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1) returning id, body, received"
			later := "2026-11-23T12:01:00.000Z"
			got, failure := p.stepRuns(receive, callPrepareQuery, false, [][]any{{later, "default", "default", now, 3}})
			is.Equal(t, "", failure)
			is.EqualSlice(t, []string{`["string:m_2" "[]uint8:[98 111 100 121 32 109 95 50]" "int64:0"]`}, got.rows)
			// The timeout of m_2 is later now, so the next receives take m_3, m_4, and m_1, and then none.
			got, failure = p.stepRuns(receive, callPrepareExecRuns, false, [][]any{{later, "default", "default", now, 3}, {later, "default", "default", now, 3}, {later, "default", "default", now, 3}, {later, "default", "default", now, 3}})
			is.Equal(t, "", failure)
			is.Equal(t, "prepare, exec several times: 1 affected, last insert id 0; then: 1 affected, last insert id 0; then: 1 affected, last insert id 0; then: 0 affected, last insert id 0", got.String())
			is.Equal(t, "", p.compareTables())
			p.restart(true)
			is.Equal(t, "", p.compareTables())
			p.close()
		}
	})

	t.Run("should take rows left open in a transaction, of a select with LIMIT run again, that fail with a justified evaluation error in the direction that s4ql draws, before their first row or after it", func(t *testing.T) {
		w := newWorld(t)
		for _, pinned := range []bool{true, false} {
			y := tally{}
			p := newPair(t, w, pinned, y)
			// In t, the row of 1, whose division is by zero, is the second in the order of the key and
			// is not read in the reverse; in u, it is the first.
			for _, src := range []string{"create table t (k int primary key) strict, without rowid", "create table u (k int primary key) strict, without rowid",
				"insert into t values (0)", "insert into t values (1)", "insert into t values (2)", "insert into t values (3)",
				"insert into u values (1)", "insert into u values (2)", "insert into u values (3)"} {
				_, failure := p.step(src, callExec, false)
				is.Equal(t, "", failure, src)
			}
			// Each run draws the directions of the compared run and of the run again, and one in four
			// gives rows and then fails when it runs again: the runs go on until the rows of t have
			// failed in their rest, and those of u before or with their first row, which 400 runs of
			// each fail to reach about once in 10^50.
			reached := func(where ...string) bool {
				for _, at := range where {
					if y["reached: rows left open that fail "+at+" with an evaluation error justified by SQLite's result of its operator"] > 0 {
						return true
					}
				}
				return false
			}
			for n := 0; n < 400 && (!reached("in their rest") || !reached("before their first row", "with their first row")); n++ {
				for _, src := range []string{"select k from t where k != 10 / (k - 1) limit 2", "select k from u where k != 10 / (k - 1) limit 2"} {
					_, failure := p.step("", callBegin, false)
					is.Equal(t, "", failure)
					got, failure := p.step(src, callQueryOpen, false)
					is.Equal(t, "", failure, src)
					is.True(t, got.failed == "" || got.err == "s4ql: division by zero in / (at offset 27)", src, got.String())
					is.Equal(t, "", p.closeRows(), src)
					_, failure = p.step("", callCommit, false)
					is.Equal(t, "", failure)
				}
			}
			is.True(t, reached("in their rest"), y)
			is.True(t, reached("before their first row", "with their first row"), y)
			is.Equal(t, "", p.compareTables())
			p.close()
		}
	})

	t.Run("should take each refusal of a scalar subquery that SQLite runs by s4ql's rule, under the excuse of its class, after the errors of the names", func(t *testing.T) {
		w := newWorld(t)
		for _, pinned := range []bool{true, false} {
			y := tally{}
			p := newPair(t, w, pinned, y)
			for _, src := range []string{t1, t3, t4, t5, "insert into t1 values (1)", "insert into t4 values (1, 'a', 1.5, 1)", "insert into t3 values (1, 'b', 2.5)"} {
				_, failure := p.step(src, callExec, false)
				is.Equal(t, "", failure)
			}
			at := func(src, sub string) string { return fmt.Sprint(strings.Index(src, sub)) }
			for _, c := range []struct {
				src  string
				args []any
				// want is s4ql's error, kind the excuse, and want "" for one that the comparison takes.
				want, kind string
			}{
				{src: "select v1 from t4 where v1 = (select v2 from t1)", want: "s4ql: not supported: correlated subquery (at offset " + at("select v1 from t4 where v1 = (select v2 from t1)", "v2 from") + ")", kind: excusedCorrelated},
				{src: "delete from t4 where v1 = (select v1 from t1 order by v3)", want: "s4ql: not supported: correlated subquery (at offset " + at("delete from t4 where v1 = (select v1 from t1 order by v3)", "v3)") + ")", kind: excusedCorrelated},
				{src: "select v1 from t4 where v2 = (select v1 from t1)", want: "s4ql: not supported: subquery 1, of the column t1.v1 of type INT, compared with the column t4.v2 of type TEXT", kind: excusedSubqueryType},
				{src: "update t4 set v2 = 'z' where v4 = (select v1 from t1) and (select v3 from t3) > v1", want: "s4ql: not supported: subquery 2, of the column t3.v3 of type REAL, compared with the column t4.v1 of type INT", kind: excusedSubqueryType},
				{src: "select v1 from t4 where v1 = (select v1 from t1 limit ?)", args: []any{int64(-1)}, want: "s4ql: not supported: argument 1, which is a negative integer, for LIMIT, which takes an integer from 0", kind: excusedSubqueryLimit},
				{src: "delete from t4 where v1 = ? and v1 = (select v1 from t1 limit ?)", args: []any{int64(1), "1"}, want: "s4ql: not supported: argument 2, which is text, for LIMIT, which takes an integer from 0", kind: excusedSubqueryLimit},
				{src: "update t4 set v2 = 'z' where v1 = (select * from t5) and v4 = (select * from t3)", want: "s4ql: not supported: second subquery of * of more than one column (at offset " + at("update t4 set v2 = 'z' where v1 = (select * from t5) and v4 = (select * from t3)", "(select * from t3)") + ")", kind: excusedWideSubqueries},
				{src: "select v1 from t4 where v1 = (select v1 from t1 where v1 = 'a')", want: "s4ql: not supported: a string literal compared with the column t1.v1 of type INT", kind: excusedType},
				{src: `select v1 from t4 where v1 = (select v1 from t1 where "nope" = 1)`, want: `1/1: no such column: "nope" - should this be a string literal in single-quotes?`, kind: excusedQuotedSubquery},
				{src: `delete from t4 where "nope" = 1 and v1 = (select v1 from nope)`, want: `1/1: no such column: "nope" - should this be a string literal in single-quotes?`, kind: excusedQuotedSubquery},
				// The errors of the names come first, and are compared with SQLite's; a decided subquery runs.
				{src: "select v1 from t4 where nope = (select v2 from t1)", want: "1/1: no such column: nope"},
				{src: "select v1 from t4 where v1 = (select v1 from t1 order by v1) and v2 = (select v2 from t3 order by v1 limit 1)"},
				{src: "update t4 set v2 = 'z' where v1 = (select * from t5) returning v1", want: "1/1: sub-select returns 5 columns - expected 1"},
			} {
				before := maps.Clone(y)
				got, failure := p.stepRuns(c.src, callQuery, false, [][]any{c.args})
				is.Equal(t, "", failure, c.src)
				is.Equal(t, c.want, got.err, c.src)
				for kind, n := range y {
					if strings.HasPrefix(kind, "excused: ") && n != before[kind] {
						is.Equal(t, "excused: "+c.kind, kind, c.src)
						is.Equal(t, before[kind]+1, n, c.src)
					}
				}
				if c.kind != "" {
					is.Equal(t, before["excused: "+c.kind]+1, y["excused: "+c.kind], c.src)
				}
			}
			is.Equal(t, "", p.compareTables())
			p.close()
		}
	})
}

// pick the call of a text of a sequence, which is a query if query, from r, in a transaction if inTx:
// one call in begins outside a transaction a begin, of a read-only transaction one time in three, or
// in a transaction an end, one call in seven, a commit two times in three, and otherwise a call for a
// query or for a statement that is no query, of which one in eight is a prepared statement that runs
// several times. In a transaction, a call also prepares its statement outside the transaction, leaves
// the rows of its query open, which half of the queries do, or queries beside the transaction, and
// one call in forty is a begin. While rows of the transaction are open, if open, one call in four is
// an end, a commit or a roll back as often, since a statement and an end of a transaction while its
// rows are open are rare otherwise.
func pick(r stmttest.Rand, query, inTx, open bool, begins int) call {
	ends, rollbacks := 7, 3
	if open {
		ends, rollbacks = 4, 2
	}
	switch {
	case !inTx && r.IntN(begins) == 0, inTx && r.IntN(40) == 0:
		if r.IntN(3) == 0 {
			return callBeginReadOnly
		}
		return callBegin
	case inTx && r.IntN(ends) == 0:
		if r.IntN(rollbacks) == 0 {
			return callRollback
		}
		return callCommit
	}
	if query && inTx {
		return []call{callPrepareQuery, callQueryRow, callStmtQuery, callBeside, callQuery, callQueryOpen, callQueryOpen, callQueryOpen, callQueryOpen, callQueryOpen}[r.IntN(10)]
	}
	if query {
		switch r.IntN(4) {
		case 0:
			return callPrepareQuery
		case 1:
			return callQueryRow
		}
		return callQuery
	}
	switch r.IntN(8) {
	case 0, 1:
		return callPrepareExec
	case 2:
		return callPrepareExecRuns
	case 3:
		if inTx {
			return callStmtExec
		}
	}
	return callExec
}

// pickLevel of a begin from r: mostly the default, one time in eight serializable, and one time in
// eight another level, which s4ql does not support.
func pickLevel(r stmttest.Rand) string {
	switch r.IntN(8) {
	case 0:
		return levelName(sql.LevelSerializable)
	case 1:
		others := slices.DeleteFunc(slices.Clone(levels), func(l sql.IsolationLevel) bool {
			return l == sql.LevelDefault || l == sql.LevelSerializable
		})
		return levelName(others[r.IntN(len(others))])
	}
	return ""
}

// again are the arguments args of a call of q for another run of its prepared statement: each value
// replaced by another value of q of the same Go type, as [stmttest.Sequence.Like] gives it, so that
// the run has the same number and the same types of arguments. An argument of the LIMIT of the select
// src that is an integer stays, so that s4ql takes or refuses every run alike, as [pair.checkRefused]
// and the comparison with SQLite require: another integer could be negative where the first is one
// from 0, or the other way round, where every value of another type that s4ql refuses for LIMIT is
// refused again.
func again(q *stmttest.Sequence, src string, args []any) []any {
	var keep []int
	if s, err := stmt.Check(src); err == nil {
		var limits []stmt.Term
		if sel, ok := s.(*stmt.Select); ok {
			limits = append(limits, sel.Limit)
		}
		for _, q := range subqueriesIn(s) {
			limits = append(limits, q.Limit)
		}
		for _, l := range limits {
			if l.Parameter > 0 && l.Parameter <= len(args) {
				if _, ok := args[l.Parameter-1].(int64); ok {
					keep = append(keep, l.Parameter-1)
				}
			}
		}
	}
	next := slices.Clone(args)
	for i, a := range next {
		// Each argument draws, so that the stream draws as it would without the ones that stay.
		if like := q.Like(a); !slices.Contains(keep, i) {
			next[i] = like
		}
	}
	return next
}

// nils are kinds of nil that database/sql hands the driver as nil, which is NULL: nil, a nil
// pointer, also one to a [driver.Valuer] with a value receiver, and a sql.Null* value that is not
// valid.
var nils = []any{nil, (*int64)(nil), (*float64)(nil), (*string)(nil), (*[]byte)(nil), (*sql.NullString)(nil),
	sql.NullInt64{}, sql.NullFloat64{}, sql.NullString{}, sql.Null[[]byte]{}}

// nilBytes are kinds of a nil []byte, which database/sql hands the driver as a nil []byte, and s4ql
// refuses: one given directly, a pointer to one, and a valid sql.Null[[]byte] whose value is nil.
var nilBytes = []any{[]byte(nil), new([]byte), sql.Null[[]byte]{Valid: true}}

// withNils are the arguments args of a call with each nil one of [nils] and each nil []byte one of
// [nilBytes], at random from r.
func withNils(r stmttest.Rand, args []any) []any {
	args = slices.Clone(args)
	for i, a := range args {
		if b, ok := a.([]byte); ok && b == nil {
			args[i] = nilBytes[r.IntN(len(nilBytes))]
		} else if a == nil {
			args[i] = nils[r.IntN(len(nils))]
		}
	}
	return args
}

// generated runs the sequence of n texts from seed on a new pair in the world w, each with its
// arguments, and compares what each side gives for each, and at the end every table. A prepared
// statement that runs several times runs one to three more times, with other values. Transactions
// begin and end within the sequence, as [pick] chooses, the calls between run in them, and every
// table is compared after each; one that is open at the end commits or rolls back. Of the seeds of
// a profile, every second sequence runs on pinned connections, and every other one through the
// pool, and half of them then restart the instance of s4ql, with an empty cache or with its own, and
// compare every table again.
func generated(t *testing.T, w *world, seed uint64, n int, y tally) {
	t.Helper()
	q, err := stmttest.NewSequence(seed)
	is.NotError(t, err)
	// A subquery, of slice 15, whose ORDER BY does not decide its first row may give the value of any
	// row that a legal order puts first, and s4ql draws among them where SQLite takes the first of its
	// plan, which the test cannot see through database/sql: so every subquery of the sequence ends its
	// ORDER BY with the key of its table, and the two sides must give the same value.
	q.Decide()
	// What the test draws for itself, its calls, the number of runs of a prepared statement, the nils
	// of the arguments, and the end of a transaction, draws from a stream of its own, so that it moves
	// no statement. The values of the runs after the first come from [stmttest.Sequence.Like], which
	// draws them after those of the statement.
	r := q.Stream("calls")
	// The variant of the sequence, its pinned connections and its restart, comes from the number of
	// the seed among the seeds of its profile, so that each profile runs with every variant.
	k := seed / stmttest.Slots
	p := newPair(t, w, k%2 == 0, y)
	var log []string
	fail := func(failure string) {
		t.Helper()
		t.Fatalf("seed %v, profile %v, pinned %v, after\n%v\n%v", seed, q.Profile().Name, p.pinned, strings.Join(log, "\n"), failure)
	}
	// One call in begins outside a transaction is a begin: one in ten for the odds of most profiles,
	// [stmttest.Profile.Transactions] 4, and one in five for 2.
	begins := 5 * q.Profile().Transactions / 2
	for range n {
		next := q.Next()
		c := pick(r, next.Query, p.tx != nil, p.tx != nil && p.tx.sRows != nil, begins)
		if next.Statements > 0 && (c == callPrepareExec || c == callPrepareExecRuns || c == callStmtExec) && r.IntN(3) != 0 {
			// A text of several statements runs through Exec, as glue's migrations do, two times in three of
			// those that would run through a prepared statement, which s4ql refuses for its second statement.
			c = callExec
		}
		src, supported, runs := next.Src, next.Supported, [][]any{next.Args}
		p.supports = nil
		if next.Statements > 0 && c == callExec {
			// Each statement of a text of several statements in one Exec is judged as a statement alone, by
			// its own support, as [pair.several] says, and every other call refuses the text for its second
			// statement.
			supported, p.supports = next.Each, next.Supports
		}
		switch c {
		case callBegin, callBeginReadOnly:
			src, supported, runs = pickLevel(r), false, nil
		case callCommit, callRollback:
			src, supported, runs = "", false, nil
		case callPrepareExecRuns:
			for range 1 + r.IntN(3) {
				runs = append(runs, again(q, next.Src, next.Args))
			}
		}
		for i := range runs {
			runs[i] = withNils(r, runs[i])
		}
		log = append(log, fmt.Sprintf("%v: %v -- with %#v", c, src, runs))
		_, failure := p.stepRuns(src, c, supported, runs)
		p.supports = nil
		if failure != "" {
			fail(failure)
		}
	}
	if p.tx != nil {
		c := callCommit
		if r.IntN(3) == 0 {
			c = callRollback
		}
		log = append(log, string(c))
		if _, failure := p.end(c); failure != "" {
			fail(failure)
		}
	}
	if failure := p.compareTables(); failure != "" {
		fail(failure)
	}
	if k%4 >= 2 {
		empty := k%8 >= 4
		log = append(log, fmt.Sprint("-- restart, with an empty cache: ", empty))
		p.restart(empty)
		if failure := p.compareTables(); failure != "" {
			fail(failure)
		}
	}
	p.close()
}

// TestGenerated runs sequences of each profile of [stmttest.Profiles], of the generator that the
// engine's differential test uses, through each call, with their arguments, on both sides, and
// requires the same outcomes and the same tables.
//
// What the run of each profile must reach is in testdata/reached/TestGenerated.tsv: every kind of
// testdata/reached/TestGenerated.kinds that the tally counts three times or more in the profile, at
// least a third as often, and the texts that the comparison excuses, by kind, exactly, so that a
// change that excuses more cannot pass unseen. After a change of the generator or of the test,
// S4QL_RECOUNT=1 writes the file again. The profiles together must reach each kind at least as
// often as the number before it in TestGenerated.kinds, which is written by hand, so that a recount
// cannot lower a floor.
func TestGenerated(t *testing.T) {
	const length = 48
	w := newWorld(t)
	total := tally{}
	runs := map[string]map[string]int{}
	var names []string
	for _, p := range stmttest.Profiles {
		y := tally{}
		runs[p.Name], names = y, append(names, p.Name)
		n := generatedOf(p)
		for k := range n {
			generated(t, w, p.Seed(k), length, y)
		}
		t.Logf("%v, %v sequences of %v texts: %v", p.Name, n, length, y)
		for kind, count := range y {
			total[kind] += count
		}
	}
	// An update whose SET fails with an evaluation error on a row after it changed another is rare in
	// the profile of arithmetic: about 15 sequences in 1,000 write one outside a transaction and 7 in
	// one, the first at the 202nd and the 233rd. The 400 first sequences hold 4 and 3, so the profile
	// runs up to 400, and [pair.compareChanged] reads each table back. The sequences after those of
	// [generatedOf] are compared as every other, and not counted: s4ql draws the directions of its
	// scans from its host, a statement with arithmetic fails on a row or not by which rows a scan meets
	// first, and the rest of its sequence then differs, its calls and its tables, so that the counts
	// of 400 sequences, the excuses among them, differ by a few between two runs.
	for k := generatedOf(stmttest.Arithmetic); k < 400; k++ {
		generated(t, w, stmttest.Arithmetic.Seed(k), length, nil)
	}
	t.Logf("%v calls ran in SQLite, and were compared with it", total[comparedWithSQLite])
	// On the one sequence of all areas, before the profiles, 250 sequences ran 8,487 calls in SQLite.
	is.True(t, total[comparedWithSQLite] >= 8487, "the calls that run in SQLite are not fewer than before the profiles")
	// Kinds that the generator writes too rarely for a profile to reach three times in the sequences
	// that the test runs, which the run must reach once at least: a value on the left of != in a
	// select that finds rows, which the values on the left reach about 4 times in 100 sequences, and
	// a column that the table does not have under IS NULL or IS NOT NULL alone in a WHERE, which the
	// profile of the WHERE reaches about 3 times in 100; an insert of NULL beside a value of another
	// type in a column that does not take NULL, which the profiles reach 6 times in all, mixed twice;
	// and an insert with two faults, one a value of another type, which mixed, order, arithmetic, and
	// several reach twice each.
	for _, kind := range []string{
		"reached: value on the left of != finds rows",
		"reached: WHERE with a column that the table does not have under IS NULL or IS NOT NULL, alone no such column",
		"reached: insert of NULL beside a value of another type NOT NULL constraint failed",
		"reached: insert with two faults: a value of another type",
	} {
		if total[kind] == 0 {
			t.Errorf("no profile reaches %q", kind)
		}
	}

	// The excuses are pinned exactly, since they tell how much of the test escapes the comparison with
	// SQLite, but for that of an evaluation error: a scalar subquery reads up to its first row in the
	// direction that s4ql draws from its host, and its WHERE with arithmetic fails on a row or not by
	// which rows it reads, so its count may differ by a few between two runs.
	path := filepath.Join("testdata", "reached", t.Name())
	reached := stmttest.Reached{Kinds: path + ".kinds", Counts: path + ".tsv", Doc: "What each profile of TestGenerated of the driver check reaches.",
		Exact: func(kind string) bool { return strings.HasPrefix(kind, "excused: ") && kind != "excused: "+excusedEvaluation }}
	failures, err := reached.Check(names, runs)
	is.NotError(t, err)
	for _, failure := range failures {
		t.Error(failure)
	}
}

// generatedOf the profile p: the number of sequences of it that [TestGenerated] runs. The mixed
// profile runs 128, the most that the time of the package allows: it is the only one in which every
// area meets the others, such as a default with an index or a clause of slice 10 with a value on the
// left, and no tally of a focused profile counts what only such a meeting reaches. The 250 of the
// one sequence of all areas before the profiles each met them. A focused profile runs as many as its
// rarest kinds of outcome need: each reaches the least number of every row of its own that
// testdata/reached had when it ran more, though a row of a kind that it then reached three to five
// times can have gone, and the profiles of the clauses of slice 10, the defaults, and the values on
// the left reach a kind that no other profile reaches three times. Each runs every variant of
// [generated] once at least.
// The profile of the ORDER BY runs 50 for its rarest refusals of an argument of LIMIT, a blob and
// NULL, each about once in ten sequences.
// The defaults and the transactions also reach the statements of a transaction while its rows are
// open the most, of which a read-only transaction's insert is the rarest. The profile of RETURNING
// runs 50 for the rows of RETURNING left open in a transaction, and for goqite's receive by id, an
// update with RETURNING of several rows, and a delete with RETURNING through a prepared statement run
// several times, each reached three to five times. The profile of the subqueries runs 60 for goqite's
// receive with its subquery, and for the statements with subqueries through a prepared statement run
// several times. The profile of arithmetic runs 40 for its evaluation errors of each kind, in SET and
// in WHERE, and for its refusals of types, each reached in a few sequences in ten; [TestGenerated]
// runs more of it, which it does not count. The profile of the
// values on the left runs 70 for a value on the left of != in a select that finds rows, which
// [TestGenerated] requires once: since the fixes after the review of the workload of arithmetic of
// 2026-11-25, mixed no longer reaches it. The profile of several statements runs 80 for a text whose
// last statement has RETURNING and whose result in mattn is the count of the statement before, which
// [pair.several] excuses, and for an evaluation error after a statement ran, each reached about once
// in twenty sequences.
func generatedOf(p *stmttest.Profile) uint64 {
	switch p {
	case stmttest.Mixed:
		return 128
	case stmttest.Exists, stmttest.Defaults:
		return 56
	case stmttest.Arguments:
		return 40
	case stmttest.ValueFirst:
		return 70
	case stmttest.Transactions:
		return 33
	case stmttest.Where:
		return 20
	case stmttest.Indexes:
		return 16
	case stmttest.Order:
		return 50
	case stmttest.Subquery:
		return 60
	case stmttest.Arithmetic:
		return 40
	case stmttest.Returning:
		return 50
	case stmttest.Tables:
		return 14
	case stmttest.Several:
		return 80
	}
	return 13
}

// fuzzSeparator separates the texts of the input of [FuzzSequence]. SQLite reads it as no token, so
// a text with it is a syntax error.
const fuzzSeparator = "\x1e"

// FuzzSequence runs the texts of its input, each through the call that a byte of calls chooses, with
// the arguments of that call in [fixedRuns], on a new pair, pinned if pinned, and compares as
// [TestGenerated] does. A begin takes the isolation level that its text names, or, if it names none,
// the level of [levels] at the length of the text, and a transaction that is open at the end
// commits. Its seeds are the sequences of
// [TestSequences], and each text of [stmttest.Others].
func FuzzSequence(f *testing.F) {
	for _, name := range slices.Sorted(maps.Keys(sequences)) {
		var texts []string
		var cs []byte
		for _, s := range sequences[name] {
			texts = append(texts, s.src)
			cs = append(cs, byte(slices.Index(calls, s.call)))
		}
		f.Add(strings.Join(texts, fuzzSeparator), cs, true)
	}
	for i, form := range stmttest.Others {
		src := form
		if strings.Contains(form, "%") {
			src = fmt.Sprintf(form, "t1", "v1", 1)
		}
		f.Add(t1+fuzzSeparator+src, []byte{0, byte(i)}, i%2 == 0)
	}
	w := newWorld(f)
	f.Fuzz(func(t *testing.T, texts string, cs []byte, pinned bool) {
		p := newPair(t, w, pinned, nil)
		for i, src := range strings.SplitN(texts, fuzzSeparator, 17)[:min(16, strings.Count(texts, fuzzSeparator)+1)] {
			c := callExec
			if len(cs) > 0 {
				c = calls[int(cs[i%len(cs)])%len(calls)]
			}
			if (c == callBegin || c == callBeginReadOnly) && !slices.ContainsFunc(levels, func(l sql.IsolationLevel) bool { return levelName(l) == src }) {
				src = levelName(levels[len(src)%len(levels)])
			}
			if _, failure := p.step(src, c, false); failure != "" {
				t.Fatal(failure)
			}
		}
		if failure := p.finish(); failure != "" {
			t.Fatal(failure)
		}
		if failure := p.compareTables(); failure != "" {
			t.Fatal(failure)
		}
		p.close()
	})
}

// several runs src through c, an Exec, with the arguments of runs, one run, if [stmt.Split] reads the
// text as several statements, or refuses it as a whole for its spaces, or the oracle's rule of spaces
// [spaceRule] refuses it, and reports whether it did, with s4ql's outcome and what differs, or "".
// Open rows of the transaction are read to their end and closed first, as [pair.closeRows] does.
//
// The whole text runs in one Exec on s4ql's side. What SQLite's side runs depends on s4ql's outcome:
//
//   - The errors that come before any statement runs are checked against the oracle's rules, and
//     nothing runs in SQLite: the spaces that mattn trims and SQLite reads as part of a token, as
//     [spaceRule] gives the error, which s4ql must give exactly where the rule does, and nowhere
//     else; more arguments than the statements have parameters; and an argument of a Go type that
//     s4ql does not support, which the driver judges for the whole call, as [driverRefusal] gives
//     it.
//   - Every other text is walked in SQLite, as [pair.severalWalk] says, in a savepoint that is
//     rolled back after it: statement by statement, each that s4ql ran checked against the rules of
//     a statement alone, up to the statement at which s4ql stopped. Then mattn's own Exec runs on
//     the connection, as [pair.severalMattn] says: of the whole text, which must give s4ql's result
//     if s4ql ran the text to its end, and the error at which the walk stopped if SQLite gave it
//     there; or of the text up to the statement at which s4ql stopped with an error of its own,
//     which must run.
//
// Then every table is read back on both sides where the call ran, in the transaction if one is open,
// also after a call that failed.
func (p *pair) several(src string, c call, supported bool, runs [][]any) (outcome, string, bool) {
	tb := p.tb
	tb.Helper()
	p.reachForms(src)
	parts, failed := stmt.Split(src)
	ns, _ := errors.AsType[*stmt.NotSupportedError](failed)
	splitSpaces := ns != nil && len(parts) == 0 && strings.HasPrefix(ns.What, "space ")
	rule := spaceRule(src)
	if rule == "" && !splitSpaces && (len(parts) == 0 || len(parts) == 1 && failed == nil) {
		return outcome{}, "", false
	}
	if p.tx != nil && p.tx.sRows != nil {
		if failure := p.closeRows(); failure != "" {
			return outcome{}, failure, true
		}
	}
	var args []any
	if len(runs) > 0 {
		args = runs[0]
	}
	s, o, scope := p.where(c, src)
	got := run(tb, s, c, src, runs, s4qlText)
	p.y.add(scope, src, got, runs)
	switch {
	case rule != "":
		p.y.excused(excusedSpaces)
		if got.failed != "exec" || got.err != rule {
			return got, fmt.Sprintf("%q: s4ql gives %v, and must refuse it with %v", src, got, rule), true
		}
		return got, "", true
	case splitSpaces:
		return got, fmt.Sprintf("%q: s4ql gives %v, and mattn and SQLite read its spaces alike", src, got), true
	}
	if q, ok := o.(*sql.DB); ok {
		// mattn's count of changes is that of the connection that ran the text.
		conn, err := q.Conn(tb.Context())
		is.NotError(tb, err)
		defer func() { is.NotError(tb, conn.Close()) }()
		o = conn
	}
	var failure string
	var stopped int
	switch total := parameters(parts); {
	case failed == nil && len(args) > total:
		failure = p.severalArguments(src, got, fmt.Sprintf("%v%v for a text whose %v statements have %v %v", wrongArguments, len(args), len(parts), total, parameterNoun(total)))
		stopped = -1
	case driverRefusal(args) != "":
		if want := driverRefusal(args); got.failed != "exec" || got.err != want {
			failure = fmt.Sprintf("s4ql gives %v, where the rule gives %q", got, want)
		}
		p.y.excused(excusedArgument)
		stopped = -1
	default:
		savepoint := func(q string) {
			_, err := o.ExecContext(tb.Context(), q)
			is.NotError(tb, err)
		}
		savepoint("savepoint several")
		var at outcome
		stopped, at, failure = p.severalWalk(src, supported, parts, failed, args, got, o)
		savepoint("rollback to several")
		savepoint("release several")
		if failure == "" {
			failure = p.severalMattn(src, parts, stopped, at, args, got, o)
		}
	}
	p.reachSeveral(src, parts, args, got, stopped, scope)
	for i, part := range parts {
		if ct, ok := part.Statement.(*stmt.CreateTable); ok && i < stopped {
			p.created[ct.Table.Value] = true
		}
		if p.tx != nil && scope == p.tx.kind() && i <= stopped {
			ran := outcome{}
			if i == stopped {
				ran = got
			}
			p.note(src[part.Span.Start:part.Span.End], ran)
		}
	}
	if p.tx != nil && scope == p.tx.kind() && got.failed != "" && (stopped < 0 || stopped == len(parts)) {
		// A failure before any statement, or of the statement that Split refused.
		p.note(src, got)
	}
	if failure == "" {
		failure = p.compareIn(s, o)
	}
	if failure != "" {
		failure = fmt.Sprintf("the text of several statements %q with the arguments %#v:\n%v", src, args, failure)
	}
	return got, failure, true
}

// parameters of the statements of parts, together.
func parameters(parts []stmt.Part) int {
	n := 0
	for _, part := range parts {
		n += part.Statement.Parameters()
	}
	return n
}

// parameterNoun for n parameters.
func parameterNoun(n int) string {
	if n == 1 {
		return "parameter"
	}
	return "parameters"
}

// severalArguments checks s4ql's outcome got of the text src of several statements against want, its
// rule's error of the wrong number of arguments, which mattn does not give: it drops the arguments
// that the statements do not take, and gives an error of its own for too few after the statements
// before. It gives what differs, or "".
func (p *pair) severalArguments(src string, got outcome, want string) string {
	p.y.excused(excusedSeveralArguments)
	if got.failed != "exec" || got.err != want {
		return fmt.Sprintf("%q: s4ql gives %v, where the rule gives %q", src, got, want)
	}
	return ""
}

// driverRefusal is the "not supported" of the first argument of args that the driver refuses for the
// whole call, before any statement runs, by its number in the call, as [argumentRefusal] gives it:
// one with a name, a []byte that is nil, an unsigned integer above the largest int64, and one of a
// type that no column holds. It is "" if there is none. An argument whose value no column holds, a
// real that is not finite or text that is no valid UTF-8 or holds NUL, is its statement's to refuse.
func driverRefusal(args []any) string {
	for i, a := range args {
		switch v := converted(a).(type) {
		case int64, float64, string, nil:
			continue
		case []byte:
			if v != nil {
				continue
			}
		}
		if _, ok := stmttest.Integers([]any{a}); ok {
			continue
		}
		// The number of the argument in the call.
		return argumentRefusal(append(make([]any, i), a))
	}
	return ""
}

// severalMattn runs mattn's own Exec on o after [pair.severalWalk] of the text src of several
// statements, with the arguments args, whose walk stopped at the part of the number stopped of
// parts, with SQLite's outcome at of the statement there if the walk ran it in SQLite, and checks
// it against s4ql's outcome got. It gives what differs, or "":
//
//   - A text that s4ql ran to its end runs whole, as [pair.severalRan] says.
//   - A text at whose statement SQLite gave the error at which the walk stopped runs whole, and
//     mattn's Exec must fail with the same error.
//   - A text at whose statement s4ql stopped with an error of its own, which the walk judged
//     without SQLite, runs up to the end of the statement before it, with the arguments of the
//     statements there, and mattn's Exec must run it. Too few arguments for a statement is such an
//     error: mattn prepares the statement before it counts its arguments, and may fail on its names
//     where s4ql counts first.
func (p *pair) severalMattn(src string, parts []stmt.Part, stopped int, at outcome, args []any, got outcome, o runner) string {
	tb := p.tb
	tb.Helper()
	ctx := tb.Context()
	switch {
	case got.failed == "":
		return p.severalRan(src, parts, args, got, o)
	case at.failed != "":
		p.y.compared()
		if stopped == len(parts) {
			// The walk ran the statement after the parts that Split refused without arguments, and
			// mattn's Exec binds the arguments after those of the parts to it.
			args = args[:parameters(parts)]
		}
		_, err := o.ExecContext(ctx, src, args...)
		if err == nil {
			return fmt.Sprintf("s4ql gives %v, and mattn's Exec runs the text to its end", got)
		}
		text := severalMattnText(tb, err)
		if text != at.err {
			return fmt.Sprintf("s4ql gives %v, and mattn's Exec fails with %v, where its statements one after another fail with %v", got, text, at.err)
		}
		return ""
	case stopped > 0:
		p.y.compared()
		n := parameters(parts[:min(stopped, len(parts))])
		if _, err := o.ExecContext(ctx, src[:parts[stopped-1].Span.End], args[:n]...); err != nil {
			return fmt.Sprintf("s4ql gives %v, and mattn's Exec of the statements before the one at which s4ql stopped fails with %v", got, severalMattnText(tb, err))
		}
	}
	return ""
}

// severalRan compares the text src of several statements, with the arguments args, which s4ql ran to
// its end with the outcome got, with one Exec of it in mattn on o, as [pair.several] says, and gives
// what differs, or "".
func (p *pair) severalRan(src string, parts []stmt.Part, args []any, got outcome, o runner) string {
	tb := p.tb
	tb.Helper()
	ctx := tb.Context()
	p.y.compared()
	before := ""
	if returns(parts[len(parts)-1].Statement) {
		before = severalBeforeLast(tb, src, parts, args, o)
	}
	res, err := o.ExecContext(ctx, src, args...)
	if err != nil {
		return fmt.Sprintf("s4ql gives %v, and mattn fails with %v", got, severalMattnText(tb, err))
	}
	want := result(tb, outcome{}, res)
	var changes int64
	is.NotError(tb, o.QueryRowContext(ctx, "select changes()").Scan(&changes))
	sets := slices.ContainsFunc(parts, func(part stmt.Part) bool {
		switch part.Statement.(type) {
		case *stmt.Insert, *stmt.Update, *stmt.Delete:
			return true
		}
		return false
	})
	if !p.pinned && !sets {
		return ""
	}
	if got.affected != fmt.Sprint(changes) {
		return fmt.Sprintf("s4ql gives %v, and SQLite's count of changes after the text in mattn is %v", got, changes)
	}
	if want.affected != fmt.Sprint(changes) {
		excuse := severalExcuse(src, parts)
		if excuse == excusedSeveralEnd && want.affected != "0" || excuse == excusedSeveralReturning && want.affected != before {
			excuse = ""
		}
		if excuse == "" {
			return fmt.Sprintf("mattn's result has %v rows affected, and SQLite's count of changes after the text is %v", want.affected, changes)
		}
		p.y.excused(excuse)
	}
	return ""
}

// severalBeforeLast is SQLite's count of changes on o before the last statement of the text src of
// several statements runs, with the arguments args, which is mattn's result of a last statement with
// RETURNING that gives a row: mattn steps it once, and SQLite counts its changes when it ends. It
// runs the statements before the last in mattn, in a savepoint that it rolls back, which leaves the
// count of changes as they set it, or as it was if none sets one, as the whole text then sets it too.
func severalBeforeLast(tb testing.TB, src string, parts []stmt.Part, args []any, o runner) string {
	tb.Helper()
	ctx := tb.Context()
	savepoint := func(q string) {
		_, err := o.ExecContext(ctx, q)
		is.NotError(tb, err)
	}
	savepoint("savepoint before_last")
	defer savepoint("release before_last")
	defer savepoint("rollback to before_last")
	if len(parts) > 1 {
		if _, err := o.ExecContext(ctx, src[:parts[len(parts)-2].Span.End], args[:parameters(parts[:len(parts)-1])]...); err != nil {
			// The Exec of the whole text fails too, which is a failure of its own.
			return ""
		}
	}
	var changes int64
	is.NotError(tb, o.QueryRowContext(ctx, "select changes()").Scan(&changes))
	return fmt.Sprint(changes)
}

// severalExcuse of mattn's result of the text src of several statements, whose count of rows
// affected is not SQLite's count of changes after the text: [excusedSeveralEnd] for a text whose last
// statement ends in a semicolon after which a comment or an empty statement follows, for which mattn
// gives 0, and [excusedSeveralReturning] for a text whose last statement has RETURNING, for which
// mattn gives SQLite's count before that statement, as it steps the statement once; or "". The
// caller requires mattn's number of each.
func severalExcuse(src string, parts []stmt.Part) string {
	last := parts[len(parts)-1].Statement
	if end := last.Bounds().End; src[end-1] == ';' && strings.TrimSpace(src[end:]) != "" {
		return excusedSeveralEnd
	}
	if returns(last) {
		return excusedSeveralReturning
	}
	return ""
}

// severalMattnText of an error of mattn: SQLite's error, as [mattnText] gives it, or mattn's own,
// such as `not enough args to execute query: want 1 got 0`, as it is.
func severalMattnText(tb testing.TB, err error) string {
	tb.Helper()
	if _, ok := errors.AsType[sqlite3.Error](err); ok {
		return mattnText(tb, err)
	}
	return err.Error()
}

// severalWalk runs in SQLite on o the statements of the text src that s4ql, with the outcome got,
// ran, as mattn runs them, one after another, each with the arguments of its parameters of args, and
// checks the one at which s4ql failed, if it did. It walks the parts of the text in order, and before
// each part runs in SQLite it asks whether s4ql's error is the part's:
//
//   - If the part has more parameters than the arguments after those of the statements before, s4ql
//     must give its rule's error of the wrong number of arguments.
//   - An error of s4ql's own with an offset, "not supported" or an evaluation error, is the part's
//     whose span holds the offset, and is judged as an Exec of the text of the part alone, with the
//     offset in it, by [pair.compare]: against s4ql's rules, and not run in SQLite. Such an error
//     without an offset, of an argument or of the type of a value, is the first part's that the
//     rules give it for.
//   - SQLite's error of a name in double quotes that is no column, which the pinned build gives and
//     mattn's SQLite reads as a string, is the part's that [pair.checkQuoted] gives it for, and the
//     part does not run in SQLite.
//   - Every other error is SQLite's: the part runs in SQLite, and is the one at which s4ql stopped if
//     SQLite fails with the same error, or with an error that [knownGap] or [pair.startedTable]
//     accepts.
//
// A part that SQLite runs, s4ql ran too, and so it must be one that s4ql's rules of a statement alone
// do not refuse, as [pair.refusedAlone] gives them, on the state before it. A text that s4ql ran to
// its end is walked so to its end. A statement after the parts that Split refused gets the same
// judgement, without arguments. It gives the number of the part at which s4ql stopped, SQLite's
// outcome of that part if it ran there, and what differs, or "".
//
// An error without an offset that two parts give alike, as a statement of the text again does, is
// taken for the first, where s4ql stops if it follows its rules. An s4ql that ran the first and
// failed at the second changed the state that the tables read back after the text show, but for a
// first that changes nothing, such as a select, which the checks of a statement alone judge.
func (p *pair) severalWalk(src string, supported bool, parts []stmt.Part, failed error, args []any, got outcome, o runner) (int, outcome, string) {
	tb := p.tb
	tb.Helper()
	spans := make([]stmt.Span, len(parts))
	for i, part := range parts {
		spans[i] = part.Span
	}
	if failed != nil {
		spans = append(spans, stmt.Span{Start: parts[len(parts)-1].Span.End, End: len(src)})
	}
	used := 0
	for i, span := range spans {
		n := 0
		if i < len(parts) {
			n = parts[i].Statement.Parameters()
		}
		text := src[span.Start:span.End]
		if used+n > len(args) {
			if got.failed == "" {
				return i, outcome{}, fmt.Sprintf("s4ql gives %v, and the statement %q has %v %v without an argument", got, text, used+n-len(args), parameterNoun(used+n-len(args)))
			}
			return i, outcome{}, p.severalArguments(src, got, fmt.Sprintf("%v%v for a text whose statements up to the one at offset %v have %v %v", wrongArguments, len(args),
				parts[i].Statement.Bounds().Start, used+n, parameterNoun(used+n)))
		}
		a := args[used : used+n]
		used += n
		part := got
		part.err = shifted(got.err, span.Start)
		at, ok := errorOffset(got.err)
		switch {
		case got.failed == "":
		case ok && (at < span.Start || at >= span.End):
			// The offset is in another part.
		case ok || got.notSupported() || evaluated(got):
			alone := supported && i < len(parts)
			if p.supports != nil && i < len(parts) {
				if i >= len(p.supports) {
					return i, outcome{}, fmt.Sprintf("s4ql gives %v, and the walk reaches its statement %v of %v, of which the generator wrote %v, so the oracle cannot tell whether s4ql supports it", got, i+1, len(parts), len(p.supports))
				}
				// Each statement is judged by whether it itself is one that s4ql supports.
				alone = p.supports[i]
			}
			if failure := p.compare(text, callExec, alone, [][]any{a}, part, o); failure == "" || ok {
				return i, outcome{}, failure
			}
		default:
			st, _ := stmt.Check(text)
			if st == nil {
				break
			}
			check := p.checkQuoted
			if subqueriesIn(st) != nil {
				check = p.checkQuotedSubquery
			}
			if failure, ok := check(text, st, part, o); ok {
				return i, outcome{}, failure
			}
		}
		refusal := ""
		if i < len(parts) {
			if _, err := stmt.Check(text); err != nil {
				// Not run in SQLite, where a statement of a transaction, such as a COMMIT, would end the
				// savepoint of the walk.
				return i, outcome{}, fmt.Sprintf("s4ql gives %v, and runs the statement %q, whose check alone refuses it with %v", got, text, err)
			}
			refusal = p.refusedAlone(text, a, o)
		}
		want := run(tb, o, callExec, text, [][]any{a}, severalMattnText)
		p.y.compared()
		if want.failed == "" {
			if refusal != "" {
				return i, outcome{}, fmt.Sprintf("s4ql gives %v, and runs the statement %q with the arguments %#v, which its rules for a statement alone refuse with %v", got, text, a, refusal)
			}
			continue
		}
		switch {
		case got.failed == "" || got.notSupported() || evaluated(got) || strings.HasPrefix(got.err, wrongArguments):
		case part.err == want.err:
			return i, want, ""
		case knownGap(text, part, want) != "":
			p.y.excused(knownGap(text, part, want) + knownGapSuffix)
			return i, want, ""
		case p.startedTable(text, callExec, [][]any{a}, part, want):
			p.y.excused(excusedStartedTable)
			return i, want, ""
		}
		return i, outcome{}, fmt.Sprintf("s4ql gives %v, and SQLite fails at the statement %q with %v", got, text, want)
	}
	if got.failed == "" {
		return len(parts), outcome{}, ""
	}
	return len(parts), outcome{}, fmt.Sprintf("s4ql gives %v, and SQLite runs every statement", got)
}

// refusedAlone is the refusal that s4ql's rules of a statement alone give for the text src of a part
// of a text of several statements, with args, the arguments of its parameters, on o, as SQLite has
// the tables there before the part runs, or "" if they give none: the check of the text, an
// argument that s4ql does not support, as [argumentRefusal] gives it, and, as [pair.checkRefused]
// judges a statement alone, one of the refusals of its arithmetic, as [pair.arithmeticRefusals]
// gives them, or of the type of a value, as [pair.typeRefusal] gives it.
func (p *pair) refusedAlone(src string, args []any, o runner) string {
	s, err := stmt.Check(src)
	switch {
	case err != nil:
		return err.Error()
	case s == nil:
		return ""
	case argumentRefusal(args) != "":
		return argumentRefusal(args)
	case stmttest.HasExpression(s):
		if refusals := p.arithmeticRefusals(src, s, boundArgs(args), o); len(refusals) > 0 {
			return refusals[0]
		}
		return ""
	}
	return p.typeRefusal(src, s, args, o)
}

// sqliteStatement is a statement of a text as [sqliteStatements] reads it: its span, from its first
// token to the end of the semicolon that ends it, or of its last token, and whether a semicolon ends
// it.
type sqliteStatement struct {
	stmt.Span
	semi bool
}

// sqliteStatements of the text src, as SQLite's sqlite3_complete (complete.c) reads where a
// statement ends, on the tokens of SQLite's tokenizer: a semicolon ends a statement, but within the
// body of a CREATE TRIGGER, which a semicolon after END ends. Spaces, comments, and empty statements
// are in no statement. The oracle reads the statements of a text so, and not as [stmt.Split] does.
func sqliteStatements(src string) []sqliteStatement {
	// The classes of tokens of sqlite3_complete, and its table of states: invalid, start, normal,
	// explain, create, trigger, semi, and end.
	const (
		semi = iota
		ws
		other
		explain
		create
		temp
		trigger
		end
	)
	trans := [8][8]int{
		{1, 0, 2, 3, 4, 2, 2, 2},
		{1, 1, 2, 3, 4, 2, 2, 2},
		{1, 2, 2, 2, 2, 2, 2, 2},
		{1, 3, 3, 2, 4, 2, 2, 2},
		{1, 4, 2, 2, 2, 4, 5, 2},
		{6, 5, 5, 5, 5, 5, 5, 5},
		{6, 6, 5, 5, 5, 5, 5, 7},
		{1, 7, 5, 5, 5, 5, 5, 5},
	}
	var statements []sqliteStatement
	state, from, last := 0, 0, 0
	for _, t := range token.Tokens(src) {
		class := other
		switch t.Kind {
		case token.Semi:
			class = semi
		case token.Space, token.Comment:
			class = ws
		case token.Explain:
			class = explain
		case token.Create:
			class = create
		case token.Temp:
			class = temp
		case token.Trigger:
			class = trigger
		case token.End:
			class = end
		}
		next := trans[state][class]
		switch {
		case state <= 1 && class != semi && class != ws:
			from = t.Offset
		case next == 1 && state > 1:
			statements = append(statements, sqliteStatement{Span: stmt.Span{Start: from, End: t.Offset + t.Len}, semi: true})
		}
		if class != ws {
			last = t.Offset + t.Len
		}
		state = next
	}
	if state > 1 {
		statements = append(statements, sqliteStatement{Span: stmt.Span{Start: from, End: last}})
	}
	return statements
}

// spaceRule is the oracle's rule of the spaces of a text of several statements: the "not supported"
// with which s4ql must refuse the text src before any statement runs, or "". mattn hands SQLite each
// statement after the first as the text after the semicolon of the statement before it, with Go's
// strings.TrimSpace applied, so mattn and SQLite read the text differently where the trim removes a
// character that SQLite does not read as a space, or a space that SQLite reads as part of a token:
// at the start of the text after a statement, "space after a statement", and, if text follows the
// first statement, at the end of the text, as [trimmedEndAt] says. s4ql reads the statements, as
// [sqliteStatements] gives them, up to the first that its check refuses, and the rule looks at the
// end after the first statement, and then at the start of the text after each statement that s4ql
// reads.
func spaceRule(src string) string {
	if !utf8.ValidString(src) || strings.ContainsRune(src, 0) {
		// The text is refused as a whole, for its bytes.
		return ""
	}
	trimmed := len(strings.TrimRightFunc(src, unicode.IsSpace))
	for i, s := range sqliteStatements(src) {
		if st, err := stmt.Check(src[s.Start:s.End]); err != nil || st == nil || !s.semi {
			return ""
		}
		if i == 0 && s.End < len(src) {
			if at, ok := trimmedEndAt(src, trimmed); ok {
				return fmt.Sprintf("s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset %v)", at)
			}
		}
		if s.End >= trimmed {
			return ""
		}
		// SQLite's tokens of the spaces that the trim removes there: a token of SQLite's spaces starts
		// with a space, a tab, a line break, or a form feed, and may hold a vertical tab after it, which
		// starts no token.
		spaces := src[s.End : len(src)-len(strings.TrimLeftFunc(src[s.End:], unicode.IsSpace))]
		for _, t := range token.Tokens(spaces) {
			if t.Kind != token.Space {
				return fmt.Sprintf("s4ql: not supported: space after a statement that SQLite reads as part of a token (at offset %v)", s.End+t.Offset)
			}
		}
	}
	return ""
}

// trimmedEndAt is where SQLite reads the end of src from the offset trimmed on, which Go's TrimSpace
// removes, otherwise than it reads src without it, and true: at trimmed, if a token runs past it that
// is no comment that stays one without the end, such as a string without its closing quote, a name
// that ends in a no-break space, or a block comment of which only `/*` is left, which SQLite reads as
// a slash at the end of a text; or at the first token after it that is not of SQLite's spaces. It
// reports false if SQLite reads both alike.
func trimmedEndAt(src string, trimmed int) (int, bool) {
	for _, t := range token.Tokens(src) {
		switch {
		case t.Offset+t.Len <= trimmed:
		case t.Offset < trimmed && t.Kind == token.Comment:
			if src[t.Offset:trimmed] == "/*" {
				return trimmed, true
			}
		case t.Offset < trimmed:
			return trimmed, true
		case t.Kind != token.Space:
			return t.Offset, true
		}
	}
	return 0, false
}

// reachForms counts in the tally the forms of the text src, of several statements as
// [sqliteStatements] reads them, that a split must read as SQLite does, by the position of the
// statement in the text, first, in the middle, or last: a CREATE TRIGGER, whose body holds
// semicolons; a name in double quotes and a string that hold a semicolon; statements that depend on
// each other, a create table and a later statement of its table, by the first of them, or the last
// if the first is not the first of the text; and a comment that holds a semicolon before the
// statements, between two, and after them. It counts every such text of an Exec, also one that
// [pair.several] leaves to the checks of a statement alone because [stmt.Split] refuses its first
// statement, as it refuses a CREATE TRIGGER first: there s4ql's error must be the check's of the
// whole text, which a split that read the trigger otherwise would not give.
func (p *pair) reachForms(src string) {
	ss := sqliteStatements(src)
	if p.y == nil || len(ss) < 2 {
		return
	}
	count := func(what string, i int) {
		position := "in the middle"
		switch i {
		case 0:
			position = "first"
		case len(ss) - 1:
			position = "last"
		}
		p.y["reached: several statements: "+what+" "+position]++
	}
	created := map[string]int{}
	dependent := false
	for i, s := range ss {
		var kinds []token.Kind
		quoted, text := false, false
		for _, t := range token.Tokens(src[s.Start:s.End]) {
			value := src[s.Start+t.Offset : s.Start+t.Offset+t.Len]
			switch t.Kind {
			case token.Space, token.Comment:
				continue
			case token.ID:
				quoted = quoted || strings.HasPrefix(value, `"`) && strings.Contains(value, ";")
				if c, ok := created[strings.ToLower(value)]; ok && !dependent && len(kinds) > 0 && kinds[0] != token.Create {
					dependent = true
					if c == 0 {
						count("statements that depend on each other", 0)
					} else {
						count("statements that depend on each other", i)
					}
				}
				if len(kinds) >= 2 && kinds[0] == token.Create && kinds[1] == token.Table && !slices.Contains(kinds, token.ID) {
					created[strings.ToLower(value)] = i
				}
			case token.String:
				text = text || strings.Contains(value, ";")
			}
			kinds = append(kinds, t.Kind)
		}
		if len(kinds) >= 2 && kinds[0] == token.Create && (kinds[1] == token.Trigger || len(kinds) >= 3 && kinds[1] == token.Temp && kinds[2] == token.Trigger) {
			count("create trigger", i)
		}
		if quoted {
			count("name in double quotes with a semicolon", i)
		}
		if text {
			count("string with a semicolon", i)
		}
	}
	positions := map[string]bool{}
	for _, t := range token.Tokens(src) {
		if t.Kind != token.Comment || !strings.Contains(src[t.Offset:t.Offset+t.Len], ";") {
			continue
		}
		switch {
		case t.Offset < ss[0].Start:
			positions["before the first statement"] = true
		case t.Offset >= ss[len(ss)-1].End:
			positions["after the last statement"] = true
		case !slices.ContainsFunc(ss, func(s sqliteStatement) bool { return s.Start <= t.Offset && t.Offset < s.End }):
			positions["between statements"] = true
		}
	}
	for position := range positions {
		p.y["reached: several statements: comment with a semicolon "+position]++
	}
}

// errorOffset of the error text err: the offset that it ends with, as "(at offset 12)", and true, or
// false if it ends with none.
func errorOffset(err string) (int, bool) {
	i := strings.LastIndex(err, " (at offset ")
	if i < 0 || !strings.HasSuffix(err, ")") {
		return 0, false
	}
	var at int
	if _, scanned := fmt.Sscanf(err[i:], " (at offset %d)", &at); scanned != nil {
		return 0, false
	}
	return at, true
}

// shifted is the error text err with the offset that it ends with, if it has one, less start: the
// offset in the text of a part of the text that starts at start.
func shifted(err string, start int) string {
	at, ok := errorOffset(err)
	if !ok {
		return err
	}
	return fmt.Sprintf("%v (at offset %v)", err[:strings.LastIndex(err, " (at offset ")], at-start)
}

// compareIn compares every table of SQLite on o and every table that a text created, through a query
// of each on s4ql's s and on o, where the call ran, in the transaction if it ran in one, and gives
// what differs, or "".
func (p *pair) compareIn(s, o runner) string {
	tb := p.tb
	tb.Helper()
	rows, err := o.QueryContext(tb.Context(), "select name from sqlite_schema where type = 'table' order by name collate binary")
	is.NotError(tb, err)
	var names []string
	for rows.Next() {
		var name string
		is.NotError(tb, rows.Scan(&name))
		names = append(names, name)
	}
	is.NotError(tb, rows.Err())
	is.NotError(tb, rows.Close())
	for name := range p.created {
		if !slices.ContainsFunc(names, func(n string) bool { return stmt.Name{Value: n}.Fold() == stmt.Name{Value: name}.Fold() }) {
			names = append(names, name)
		}
	}
	for _, name := range names {
		q := "select * from " + stmttest.Quote(name)
		held, want := run(tb, s, callQuery, q, nil, s4qlText), run(tb, o, callQuery, q, nil, mattnText)
		if held.full() != want.full() {
			return fmt.Sprintf("after it, the table %q holds in s4ql\n%v\nand in SQLite\n%v", name, held.full(), want.full())
		}
	}
	return ""
}

// reachSeveral counts in the tally what the text src of several statements, with the parts that
// [stmt.Split] gives and the arguments args, reached with s4ql's outcome got, which stopped at the part
// of the number stopped, -1 before any part, and len(parts) after every part, in the scope of the
// call: the number of its statements, the kinds of the statements that ran, the arguments, too few
// and too many, a statement that failed after one ran and one that failed before the last, by its
// error, the empty statements and comments before, between, and after the statements, and a last
// statement with RETURNING that ran.
func (p *pair) reachSeveral(src string, parts []stmt.Part, args []any, got outcome, stopped int, scope string) {
	if p.y == nil {
		return
	}
	count := func(what string) {
		p.y["reached: several statements: "+what]++
		if scope != "" {
			p.y[scope+": reached: several statements: "+what]++
		}
	}
	outcome := "ok"
	if got.failed != "" {
		outcome = "failed"
	}
	count(outcome)
	count(fmt.Sprint(min(len(parts), 5), " statements ", outcome))
	if len(args) > 0 {
		count("with arguments " + outcome)
	}
	for i, part := range parts[:max(0, min(stopped, len(parts)))] {
		count(statementKind(src[part.Span.Start:part.Span.End]) + " that ran")
		if i == len(parts)-1 && returns(part.Statement) {
			count("last statement with RETURNING that ran")
		}
	}
	switch {
	case strings.HasPrefix(got.err, wrongArguments) && stopped < 0:
		count("more arguments than parameters")
	case strings.HasPrefix(got.err, wrongArguments):
		count("fewer arguments than parameters")
	}
	if got.failed != "" && stopped > 0 && stopped <= len(parts) {
		what := messageKind(got.err)
		switch {
		case got.notSupported():
			what = "not supported"
		case evaluated(got):
			what = "evaluation error"
		case what == got.err:
			what = "another error"
		}
		count("failed after a statement ran: " + what)
		if stopped < len(parts)-1 {
			count("failed before the last statement: " + what)
		}
	}
	first, last := parts[0].Statement.Bounds(), parts[len(parts)-1].Statement.Bounds()
	if fills(src[:first.Start]) {
		count("empty statement or comment before the first statement")
	}
	for i := 1; i < len(parts); i++ {
		if fills(src[parts[i-1].Statement.Bounds().End:parts[i].Statement.Bounds().Start]) {
			count("empty statement or comment between statements")
			break
		}
	}
	if fills(src[last.End:]) {
		count("empty statement or comment after the last statement " + outcome)
	}
}

// fills reports whether src holds a semicolon or a comment.
func fills(src string) bool {
	return slices.ContainsFunc(token.Tokens(src), func(t token.Token) bool { return t.Kind == token.Semi || t.Kind == token.Comment })
}

// severalText of the outcome of an Exec of a text of several statements on r: the rows affected of
// its result, or its error. If changes, it ends with the count of changes that SQLite has on the
// connection of r after it, as `select changes()` gives it, which only mattn's SQLite answers.
// mattn's own errors, such as `not enough args to execute query: want 1 got 0`, are given as they
// are.
func severalText(tb testing.TB, r runner, src string, args []any, changes bool) string {
	tb.Helper()
	ctx := tb.Context()
	res, err := r.ExecContext(ctx, src, args...)
	var text string
	switch {
	case err == nil:
		o := result(tb, outcome{}, res)
		text = "affected " + o.affected
	case changes:
		if _, ok := errors.AsType[sqlite3.Error](err); ok {
			text = "failed with " + mattnText(tb, err)
		} else {
			text = "failed with " + err.Error()
		}
	default:
		text = "failed with " + s4qlText(tb, err)
	}
	if changes {
		var n int64
		is.NotError(tb, r.QueryRowContext(ctx, "select changes()").Scan(&n))
		text += fmt.Sprint(", changes() ", n)
	}
	return text
}

// TestSeveral runs texts of several statements through Exec on both sides, on a pinned connection
// and in transactions, and compares what each gives and the tables after it. Where mattn's driver
// gives what s4ql does not copy, the test pins both outcomes and what s4ql's rule leaves of the
// tables, as the comment of the case says.
func TestSeveral(t *testing.T) {
	ins := func(v int) string { return fmt.Sprint("insert into t1 (v1) values (", v, ")") }
	tests := []struct {
		name  string
		setup []string
		src   string
		args  []any
		// s4ql and mattn are the outcomes of each, as severalText gives them. The count of changes of
		// s4ql's result must be the changes() of mattn's SQLite after the same text, unless the case
		// has an oracle.
		s4ql, mattn string
		// oracle, if set, is what mattn runs on the database that the tables are compared with, so
		// that it holds what s4ql's rule leaves of them, and "-" for nothing. The text itself then runs
		// in mattn on a database of its own, with the same set-up.
		oracle string
	}{
		{name: "statements with arguments, a comment, an empty statement, and a select", src: t1 + "; ;-- c\n" + "insert into t1 (v1) values (?);\nupdate t1 set v1 = ? where v1 = ?; select * from t1 where v1 = ?",
			args: []any{1, 2, 1, 2}, s4ql: "affected 1", mattn: "affected 1, changes() 1"},
		{name: "a create table and a select after an insert", setup: []string{t1}, src: ins(1) + "; " + ins(2) + "; delete from t1; create table t2 (v1 int primary key) strict, without rowid; select * from t2",
			s4ql: "affected 2", mattn: "affected 2, changes() 2"},
		{name: "a failed insert in the middle, which sets the count to 0", setup: []string{t1}, src: ins(1) + "; " + ins(1) + "; " + ins(2),
			s4ql: "failed with 19/1555: UNIQUE constraint failed: t1.v1", mattn: "failed with 19/1555: UNIQUE constraint failed: t1.v1, changes() 0"},
		{name: "a syntax error in the middle", setup: []string{t1}, src: ins(1) + "; insert into t1 (v1) valus (2); " + ins(3),
			s4ql: `failed with 1/1: near "valus": syntax error`, mattn: `failed with 1/1: near "valus": syntax error, changes() 1`},
		{name: "an error of the names in the middle", setup: []string{t1}, src: ins(1) + "; insert into t9 (v1) values (2); " + ins(3),
			s4ql: "failed with 1/1: no such table: t9", mattn: "failed with 1/1: no such table: t9, changes() 1"},
		{name: "an incomplete statement at the end", setup: []string{t1}, src: ins(1) + "; insert into",
			s4ql: "failed with 1/1: incomplete input", mattn: "failed with 1/1: incomplete input, changes() 1"},
		{name: "a RETURNING in the middle", setup: []string{t1}, src: ins(1) + " returning v1; " + ins(2) + "; select * from t1",
			s4ql: "affected 1", mattn: "affected 1, changes() 1"},
		{name: "too few arguments, which runs the statements before the one that lacks them", setup: []string{t1},
			src: "insert into t1 (v1) values (?); insert into t1 (v1) values (?)", args: []any{1},
			s4ql:  "failed with s4ql: wrong number of arguments: 1 for a text whose statements up to the one at offset 32 have 2 parameters",
			mattn: "failed with not enough args to execute query: want 1 got 0, changes() 1"},
		// The empty statement at the end makes mattn give a result of 0 rows affected, which its
		// issue 963 put there in place of no result; SQLite's count of changes is 1, which s4ql gives.
		{name: "a comment at the end", setup: []string{t1}, src: ins(1) + "; -- c", s4ql: "affected 1", mattn: "affected 0, changes() 1", oracle: ins(1)},
		{name: "a semicolon at the end", setup: []string{t1}, src: ins(1) + ";;", s4ql: "affected 1", mattn: "affected 0, changes() 1", oracle: ins(1)},
		{name: "a semicolon and a space at the end", setup: []string{t1}, src: ins(1) + "; \n", s4ql: "affected 1", mattn: "affected 1, changes() 1"},
		// mattn steps a statement with RETURNING once, and its result has the count of the statement
		// before; SQLite's count after it is that of the RETURNING, which s4ql gives.
		{name: "a RETURNING at the end", setup: []string{t1, ins(1), ins(2)}, src: ins(3) + "; delete from t1 returning v1",
			s4ql: "affected 3", mattn: "affected 1, changes() 3", oracle: ins(3) + "; delete from t1"},
		// It is SQLite's count before the last statement, which a call before sets where no statement
		// of the text before the last does.
		{name: "a RETURNING at the end after a statement that sets no count", setup: []string{t1, ins(1), ins(2), ins(3), "delete from t1 where v1 > 1"},
			src:  "create table t2 (v1 int primary key) strict, without rowid; " + ins(4) + " returning v1",
			s4ql: "affected 1", mattn: "affected 2, changes() 1", oracle: "create table t2 (v1 int primary key) strict, without rowid; " + ins(4)},
		// Each of these, s4ql refuses before any statement runs.
		{name: "more arguments than parameters, which mattn drops", setup: []string{t1}, src: "insert into t1 (v1) values (?); " + ins(2), args: []any{1, 9},
			s4ql:  "failed with s4ql: wrong number of arguments: 2 for a text whose 2 statements have 1 parameter",
			mattn: "affected 1, changes() 1", oracle: "-"},
		{name: "a no-break space after a semicolon, which mattn's TrimSpace removes and SQLite reads as a name", setup: []string{t1}, src: ins(1) + ";\u00a0" + ins(2),
			s4ql:  "failed with s4ql: not supported: space after a statement that SQLite reads as part of a token (at offset 31)",
			mattn: "affected 1, changes() 1", oracle: "-"},
		{name: "a vertical tab after a semicolon, which SQLite does not read as a space where a token starts", setup: []string{t1}, src: ins(1) + ";\v" + ins(2),
			s4ql:  "failed with s4ql: not supported: space after a statement that SQLite reads as part of a token (at offset 31)",
			mattn: "affected 1, changes() 1", oracle: "-"},
		{name: "a no-break space at the end", setup: []string{t1}, src: ins(1) + "; " + ins(2) + "\u00a0",
			s4ql:  "failed with s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 62)",
			mattn: "affected 1, changes() 1", oracle: "-"},
		{name: "a block comment that the trim makes a slash", setup: []string{t1}, src: ins(1) + "; /* ",
			s4ql:  "failed with s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 34)",
			mattn: `failed with 1/1: near "/": syntax error, changes() 1`, oracle: "-"},
		{name: "a string without its closing quote, whose spaces the trim takes from the message", setup: []string{t1}, src: ins(1) + "; select 'abc  ",
			s4ql:  "failed with s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 43)",
			mattn: `failed with 1/1: unrecognized token: "'abc", changes() 1`, oracle: "-"},
		// A statement that s4ql does not support ends the run after the statements before it.
		{name: "a statement that s4ql does not support in the middle", setup: []string{t1}, src: ins(1) + "; drop view if exists v; " + ins(2),
			s4ql:  "failed with s4ql: not supported: DROP VIEW (at offset 37)",
			mattn: "affected 1, changes() 1", oracle: ins(1)},
	}
	w := newWorld(t)
	for _, test := range tests {
		t.Run("should give what mattn gives for "+test.name, func(t *testing.T) {
			p := newPair(t, w, true, nil)
			defer p.close()
			for _, s := range test.setup {
				_, err := p.s.ExecContext(t.Context(), s)
				is.NotError(t, err)
				_, err = p.o.ExecContext(t.Context(), s)
				is.NotError(t, err)
			}
			got := severalText(t, p.s, test.src, test.args, false)
			is.Equal(t, test.s4ql, got)
			if test.oracle == "" {
				is.Equal(t, test.mattn, severalText(t, p.o, test.src, test.args, true))
				if !strings.HasPrefix(got, "failed") {
					is.Equal(t, "affected "+strings.TrimPrefix(test.mattn[strings.LastIndex(test.mattn, "changes() "):], "changes() "), got)
				}
			} else {
				scratch, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "scratch.db"))
				is.NotError(t, err)
				defer func() { is.NotError(t, scratch.Close()) }()
				scratch.SetMaxOpenConns(1)
				for _, s := range test.setup {
					_, err := scratch.ExecContext(t.Context(), s)
					is.NotError(t, err)
				}
				is.Equal(t, test.mattn, severalText(t, scratch, test.src, test.args, true))
				if test.oracle != "-" {
					_, err := p.o.ExecContext(t.Context(), test.oracle)
					is.NotError(t, err)
				}
			}
			is.Equal(t, "", p.compareTables())
		})
	}

	for _, commit := range []bool{true, false} {
		t.Run(fmt.Sprintf("should keep the statements before one that fails in a transaction, which goes on, and commits them: %v", commit), func(t *testing.T) {
			p := newPair(t, w, false, nil)
			defer p.close()
			for _, r := range []queryer{p.s, p.o} {
				_, err := r.ExecContext(t.Context(), t1)
				is.NotError(t, err)
			}
			var txs []*sql.Tx
			for _, r := range []queryer{p.s, p.o} {
				tx, err := r.BeginTx(t.Context(), nil)
				is.NotError(t, err)
				txs = append(txs, tx)
			}
			src := ins(1) + "; " + ins(2) + "; " + ins(1) + "; " + ins(3)
			is.Equal(t, "failed with 19/1555: UNIQUE constraint failed: t1.v1", severalText(t, txs[0], src, nil, false))
			is.Equal(t, "failed with 19/1555: UNIQUE constraint failed: t1.v1, changes() 0", severalText(t, txs[1], src, nil, true))
			src = ins(4) + "; " + ins(5)
			is.Equal(t, "affected 1", severalText(t, txs[0], src, nil, false))
			is.Equal(t, "affected 1, changes() 1", severalText(t, txs[1], src, nil, true))
			for _, tx := range txs {
				if commit {
					is.NotError(t, tx.Commit())
				} else {
					is.NotError(t, tx.Rollback())
				}
			}
			is.Equal(t, "", p.compareTables())
		})
	}
}
