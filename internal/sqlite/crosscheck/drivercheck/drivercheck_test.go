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
	"unicode/utf8"

	"github.com/mattn/go-sqlite3"
	"maragu.dev/is"

	"maragu.dev/s4ql"
	"maragu.dev/s4ql/internal/engine"
	sqloracle "maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
	"maragu.dev/s4ql/internal/sql/token"
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
		// database/sql gives the error of the query at Scan, and gives no columns of a row: the query
		// runs with one destination, and again with as many as database/sql says that the row has, if
		// it has more.
		values := make([]any, 1)
		for {
			dest := make([]any, len(values))
			for i := range values {
				dest[i] = &values[i]
			}
			err := r.QueryRowContext(ctx, src, args...).Scan(dest...)
			var n int
			if _, scanned := fmt.Sscanf(fmt.Sprint(err), "sql: expected %d destination arguments in Scan, not 1", &n); err != nil && scanned == nil && len(values) == 1 {
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
	tb.Fatalf("no call %q", c)
	return o
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
		o.columns, o.types, o.rows, o.scans = "", "", nil, nil
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
	for _, sentinel := range []error{s4ql.ErrNotSupported, s4ql.ErrNotServing, s4ql.ErrOutcomeUnknown, s4ql.ErrStopped, s4ql.ErrTooLarge} {
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
// and maybe semicolons, spaces, and comments after it, since s4ql does not support a second
// statement. mattn runs every statement of a text, and one that is only a comment too, and then gives
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
	isolate(tb)
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
// with a parameter if it has one, or "other".
func statementKind(src string) string {
	s, _ := stmt.Check(src)
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
// before it runs there. For an insert: each argument by its kind, as [argumentKind] names it, each
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
	s, _ := stmt.Check(src)
	// A value on the left, of slice 12, on a table that a create table if not exists, of slice 10,
	// found: a meeting of two areas that only the mixed profile has on.
	if table, where := tableWhere(s); p.found[table.Fold()] && slices.ContainsFunc(where, func(c stmt.Condition) bool { return c.ValueFirst }) {
		count("value on the left on a table that a create table if not exists found")
	}
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
		for _, out := range got.runs() {
			null := out.null
			if got.call == callQueryRow && out.failed == "" {
				// The row of a QueryRow is the one that s4ql's scan meets first, which varies with the
				// direction of the scan, so the count is by the row that SQLite gives, which does not.
				null = run(tb, o, callQueryRow, firstStatement(src), runs, mattnText).null
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
		whole, args := stmttest.SelectText(&stmt.Select{Table: sel.Table, Where: sel.Where}, bound, "")
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
		switch {
		case outcome != "ok" || out.affected == "":
		case out.affected == "0":
			outcome = "ok, no row found"
		default:
			outcome = "ok, rows changed"
		}
		count(statement + map[bool]string{true: " with WHERE ", false: " without WHERE "}[where != nil] + outcome)
		if len(runs[i]) != s.Parameters() || argumentRefusal(runs[i]) != "" {
			count(statement + " with arguments that s4ql refuses " + outcome)
			reachValueFirst(&stmt.Select{Where: where}, nil, kind(out), -1, count)
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
		if out.affected != "" {
			rows = map[bool]int{true: 0, false: 1}[out.affected == "0"]
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

// messageKind of an error's text: its message without the names.
func messageKind(err string) string {
	for _, kind := range []string{
		"already exists", "no such table", "has no column named", "no such column", "values were supplied", " values for ", "duplicate column name",
		"too many columns on", "UNIQUE constraint failed", "NOT NULL constraint failed", "syntax error", "incomplete input", "no rows",
		"wrong number of arguments", "attempt to write a readonly database", "there is already a table named", "there is already an index named",
		"no such index",
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
	"there is already a table named", "there is already an index named", "no such index",
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
	excusedQuoted = "a select, an update, a delete, or a create index with a name in double quotes that is no column, which mattn's SQLite reads as a string, checked against the pinned build's error"
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
	// wrote is set once an insert succeeded in the transaction, failed once a call failed in it, and
	// created and dropped once a create table or a drop table succeeded in it.
	wrote, failed, created, dropped bool
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
	s, o, scope := p.where(c, src)
	if p.tx != nil && p.tx.sRows != nil {
		return p.whileRowsOpen(src, c, supported, runs, s, o, scope)
	}
	got := run(tb, s, c, src, runs, s4qlText)
	p.y.add(scope, src, got, runs)
	p.reach(src, runs, got, o)
	failure := p.compare(src, c, supported, runs, got, o)
	if failure == "" && got.failed == "" {
		failure = p.compareChanged(src, s, o)
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
	for _, o := range got.runs() {
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
// [pair.closeRows] reads the rest.
func (p *pair) openRows(src string, args []any, compared outcome) string {
	tb := p.tb
	tb.Helper()
	t := p.tx
	var err error
	t.sRows, err = t.s.QueryContext(tb.Context(), src, args...)
	is.NotError(tb, err)
	t.oRows, err = t.o.QueryContext(tb.Context(), firstStatement(src), args...)
	is.NotError(tb, err)
	got, found := firstRow(tb, t.sRows)
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

// firstRow of rows, with its scan into sql.Null* values and into pointers, as [pairText] gives them
// and as [outcome.pairs] has them, and whether the rows have one.
func firstRow(tb testing.TB, rows *sql.Rows) (string, bool) {
	tb.Helper()
	if !rows.Next() {
		is.NotError(tb, rows.Err())
		return "", false
	}
	return pairText(typedRow(tb, rows)), true
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
// s4ql refuses a statement of the transaction while its rows are open. It gives what differs, or "",
// also where no rows are open.
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
	for t.oRows.Next() {
		want = append(want, pairText(typedRow(tb, t.oRows)))
	}
	is.NotError(tb, t.oRows.Err())
	is.NotError(tb, t.sRows.Close())
	is.NotError(tb, t.oRows.Close())
	t.sRows, t.oRows = nil, nil
	if sErr != nil {
		return fmt.Sprintf("%q: the rest of s4ql's rows that were left open fails with %v", t.opened, sErr)
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
	got = run(tb, s, c, src, runs, s4qlText)
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
	p.tx = nil
	open := t.sRows != nil
	if open {
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
		}{{t.failed, "after a failed call"}, {t.wrote, "after an insert"}, {t.created, "after a create table"}, {t.dropped, "after a drop table"}, {open, "while rows are open"}} {
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

	if failure, ok := p.checkQuoted(src, s, got, o); ok {
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
		all := run(tb, o, callQuery, first, mattnRuns, mattnText)
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
		text, _ := stmttest.SelectText(&stmt.Select{Table: sel.Table, Where: sel.Where}, args, orderBy)
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
// gives it, is in double quotes, in the select list or in a condition. mattn's SQLite reads such a
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
		// both builds.
		_, columns := p.columnsIn(o, s.Table.Value)
		if columns == nil || slices.ContainsFunc(s.Set, func(a stmt.Assignment) bool { return stmttest.ColumnOf(columns, a.Column) < 0 }) {
			return "", false
		}
		sel = &stmt.Select{Table: s.Table, Where: s.Where}
	case *stmt.Delete:
		sel = &stmt.Select{Table: s.Table, Where: s.Where}
	case *stmt.CreateIndex:
		sel = &stmt.Select{Table: s.Table}
	default:
		return "", false
	}
	_, columns := p.columnsIn(o, sel.Table.Value)
	if columns == nil {
		return "", false
	}
	name, missing := stmttest.MissingColumn(columns, sel)
	if ci, ok := s.(*stmt.CreateIndex); ok {
		// The first column of the index in the order of the text that the table does not have: the
		// build of SQLite with DQS on makes an index on a string literal of it, if it is in double
		// quotes, where the pinned build gives its error.
		at := slices.IndexFunc(ci.Columns, func(c stmt.IndexColumn) bool { return stmttest.ColumnOf(columns, c.Name) < 0 })
		if missing = at >= 0; missing {
			name = ci.Columns[at].Name
		}
	}
	if !missing || name.Raw[0] != '"' {
		return "", false
	}
	want := fmt.Sprintf(`1/1: no such column: "%v" - should this be a string literal in single-quotes?`, name.Value)
	if _, ok := s.(*stmt.CreateIndex); ok && !slices.ContainsFunc(got.runs(), func(r outcome) bool { return r.err == want }) {
		// An error of the name of the index or of the table comes first in both builds, and the
		// comparison with mattn's SQLite checks it.
		return "", false
	}
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
// with an error of its own, on the tables that SQLite has on o, where the call runs.
func (p *pair) checkRefused(src string, s stmt.Statement, runs [][]any, got outcome, o runner) string {
	if len(got.runs()) != len(runs) {
		return fmt.Sprintf("%q with %v runs: s4ql gives %v", src, len(runs), got)
	}
	kind := excusedType
	for i, r := range got.runs() {
		args := runs[i]
		want := argumentRefusal(args)
		if want != "" {
			kind = excusedArgument
		} else if len(args) == s.Parameters() {
			want = p.typeRefusal(src, s, args, o)
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
	ps, err := o.PrepareContext(tb.Context(), firstStatement(src))
	if err != nil {
		return ""
	}
	is.NotError(tb, ps.Close())
	name, columns := p.columnsIn(o, table.Value)
	var bound []any
	for _, a := range args {
		if v, ok := stmttest.Integers([]any{a}); ok {
			a = v[0]
		}
		bound = append(bound, converted(a))
	}
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
			q, args := stmttest.FoundQuery(s.Table, s.Where, bound)
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
	var which string
	switch v := args[s.Limit.Parameter-1].(type) {
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
	return fmt.Sprintf("s4ql: not supported: argument %v, which is %v, for LIMIT, which takes an integer from 0", s.Limit.Parameter, which)
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
// succeeded on s4ql's runner s and SQLite's o, through a query of it on each, in the transaction where
// the statement ran, if it ran in one, and gives what differs, or "". It counts each such read under
// "reached: table read back after an update or a delete".
func (p *pair) compareChanged(src string, s, o runner) string {
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
	}
	q := "select * from " + stmttest.Quote(table.Value)
	got, want := run(tb, s, callQuery, q, nil, s4qlText), run(tb, o, callQuery, q, nil, mattnText)
	if got.full() != want.full() {
		return fmt.Sprintf("%q: after it, the table holds in s4ql\n%v\nand in SQLite\n%v", src, got.full(), want.full())
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
)

// sequences written by hand, which cover every statement of slice 1, every error that an
// application can get from it, and each call.
var sequences = map[string][]step{
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
	keep := -1
	if s, err := stmt.Check(src); err == nil {
		if sel, ok := s.(*stmt.Select); ok && sel.Limit.Parameter > 0 && sel.Limit.Parameter <= len(args) {
			if _, ok := args[sel.Limit.Parameter-1].(int64); ok {
				keep = sel.Limit.Parameter - 1
			}
		}
	}
	next := slices.Clone(args)
	for i, a := range next {
		// Each argument draws, so that the stream draws as it would without the one that stays.
		if like := q.Like(a); i != keep {
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
		src, supported, runs := next.Src, next.Supported, [][]any{next.Args}
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
		if _, failure := p.stepRuns(src, c, supported, runs); failure != "" {
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
	t.Logf("%v calls ran in SQLite, and were compared with it", total[comparedWithSQLite])
	// On the one sequence of all areas, before the profiles, 250 sequences ran 8,487 calls in SQLite.
	is.True(t, total[comparedWithSQLite] >= 8487, "the calls that run in SQLite are not fewer than before the profiles")
	// Kinds that the generator writes too rarely for a profile to reach three times in the sequences
	// that the test runs, which the run must reach once at least: a value on the left of != in a
	// select that finds rows, which the values on the left reach about 4 times in 100 sequences, and
	// a column that the table does not have under IS NULL or IS NOT NULL alone in a WHERE, which the
	// profile of the WHERE reaches about 3 times in 100.
	for _, kind := range []string{
		"reached: value on the left of != finds rows",
		"reached: WHERE with a column that the table does not have under IS NULL or IS NOT NULL, alone no such column",
	} {
		if total[kind] == 0 {
			t.Errorf("no profile reaches %q", kind)
		}
	}

	path := filepath.Join("testdata", "reached", t.Name())
	reached := stmttest.Reached{Kinds: path + ".kinds", Counts: path + ".tsv", Doc: "What each profile of TestGenerated of the driver check reaches.",
		Exact: func(kind string) bool { return strings.HasPrefix(kind, "excused: ") }}
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
// open the most, of which a read-only transaction's insert is the rarest.
func generatedOf(p *stmttest.Profile) uint64 {
	switch p {
	case stmttest.Mixed:
		return 128
	case stmttest.Exists, stmttest.Defaults:
		return 56
	case stmttest.Arguments:
		return 40
	case stmttest.ValueFirst:
		return 35
	case stmttest.Transactions:
		return 33
	case stmttest.Where:
		return 20
	case stmttest.Indexes:
		return 16
	case stmttest.Order:
		return 50
	case stmttest.Tables:
		return 14
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
