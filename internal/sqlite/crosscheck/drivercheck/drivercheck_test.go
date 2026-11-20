package drivercheck_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"path/filepath"
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
	// columns of a query, by their names and by the names of their database types, and the rows,
	// each value as its Go type and its value.
	columns, types string
	rows           []string
	// next are the outcomes of the runs after the first of [callPrepareExecRuns], without a call.
	next []outcome
}

func (o outcome) String() string {
	var b strings.Builder
	b.WriteString(string(o.call))
	if o.failed != "" {
		fmt.Fprintf(&b, ": %v failed with %v", o.failed, o.err)
	} else if o.affected != "" || o.lastID != "" {
		fmt.Fprintf(&b, ": %v affected, last insert id %v", o.affected, o.lastID)
	}
	switch {
	case o.failed != "":
	case o.columns != "" || o.types != "":
		fmt.Fprintf(&b, ": columns %q of types %q, rows %v", o.columns, o.types, strings.Join(o.rows, " "))
	case len(o.rows) > 0:
		// A row of QueryRow, whose columns database/sql does not give.
		fmt.Fprintf(&b, ": row %v", strings.Join(o.rows, " "))
	}
	for _, n := range o.next {
		fmt.Fprintf(&b, "; then%v", n)
	}
	return b.String()
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
		var texts []string
		for _, v := range values {
			texts = append(texts, valueText(v))
		}
		o.rows = []string{strings.Join(texts, ",")}
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
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		is.NotError(tb, rows.Scan(dest...))
		var texts []string
		for _, v := range values {
			texts = append(texts, valueText(v))
		}
		o.rows = append(o.rows, strings.Join(texts, ","))
	}
	if err := rows.Err(); err != nil {
		o.failed, o.err = "rows", errText(tb, err)
		o.columns, o.types, o.rows = "", "", nil
	}
	return o
}

// valueText of a value that database/sql scanned into an any: its Go type and its value.
func valueText(v any) string {
	return fmt.Sprintf("%T:%v", v, v)
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
// before it runs there. For an insert: each argument by its kind, as [stmttest.ArgumentKind] names it, each
// literal by its kind, as [stmttest.LiteralKind] names it, and, for an insert into a table that
// SQLite has, the outcome of each run by the type of the key column, and for a table of several
// columns by the form of the insert, without a list or with one, and by its faults, as
// [stmttest.Faults] gives them, one or two. For a select of a table that SQLite has, the outcome of
// each run by its form: * by the number of columns, names, and a name that the table does not have.
func (p *pair) reach(src string, runs [][]any, got outcome, o runner) {
	tb := p.tb
	tb.Helper()
	if p.y == nil {
		return
	}
	count := func(what string) { p.y["reached: "+what]++ }
	kind := func(o outcome) string {
		switch {
		case strings.HasPrefix(o.err, "s4ql: not supported: ") && strings.Contains(o.err, " for the column "):
			return "a value of another type"
		case o.notSupported():
			return "not supported"
		case o.failed != "":
			return messageKind(o.err)
		}
		return "ok"
	}
	s, _ := stmt.Check(src)
	if sel, ok := s.(*stmt.Select); ok {
		_, columns := p.columnsIn(o, sel.Table.Value)
		if columns == nil {
			return
		}
		for _, o := range got.runs() {
			switch {
			case sel.Columns == nil:
				count(fmt.Sprint("select * of ", len(columns), " columns ", kind(o)))
			case slices.ContainsFunc(sel.Columns, func(n stmt.Name) bool { return stmttest.ColumnOf(columns, n) < 0 }):
				count("select of a name that the table does not have " + kind(o))
			default:
				count("select of names " + kind(o))
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
				count("argument " + stmttest.ArgumentKind(a))
			}
		}
	}
	for _, v := range insert.Values {
		if v.Parameter == 0 {
			count("literal " + stmttest.LiteralKind(v.Value))
		}
	}
	_, columns := p.columnsIn(o, insert.Table.Value)
	var typ string
	for _, c := range columns {
		if c.PrimaryKey {
			typ = c.Type
		}
	}
	if typ == "" {
		return
	}
	for i, o := range got.runs() {
		count("insert into " + typ + " " + kind(o))
		if len(columns) < 2 || i >= len(runs) || !reaches(src, runs[i]) {
			continue
		}
		var bound []any
		for _, a := range runs[i] {
			if v, ok := stmttest.Integers([]any{a}); ok {
				a = v[0]
			}
			bound = append(bound, converted(a))
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
		"wrong number of arguments", "attempt to write a readonly database",
	} {
		if strings.Contains(err, kind) {
			return kind
		}
	}
	return err
}

// prepareKinds are the kinds of [messageKind] of SQLite's errors that depend on the database and that
// SQLite gives when it prepares a statement, where s4ql gives them when the statement runs.
var prepareKinds = []string{"no such table", "already exists", "has no column named", "no such column", "values were supplied", " values for ", "duplicate column name", "too many columns on"}

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
	// excusedType is an insert of a value, a literal or an argument, of another type than its column,
	// which SQLite converts or refuses with an error of its own: it is checked against s4ql's rule by
	// [pair.checkRefused], and not run in SQLite.
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
	// excusedQuoted is a select of a name in double quotes that the table does not have, which
	// mattn's SQLite reads as a string, and which [pair.checkQuoted] checks against the pinned build's
	// error instead.
	excusedQuoted = "a select of a name in double quotes that is no column, which mattn's SQLite reads as a string, checked against the pinned build's error"
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
	y       tally
	// tx is the transaction that is open on both sides, or nil.
	tx *transaction
}

// transaction that is open on both sides of a pair, which the calls run in.
type transaction struct {
	// s and o are s4ql's and SQLite's.
	s, o     *sql.Tx
	readOnly bool
	// sRows and oRows are the rows that [callQueryOpen] left open, or nil.
	sRows, oRows *sql.Rows
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
	p := &pair{tb: tb, w: w, prefix: fmt.Sprint("db", w.prefixes.Add(1)), cache: filepath.Join(tb.TempDir(), "cache"), pinned: pinned, created: map[string]bool{}, y: y}
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
	if failure == "" && p.tx != nil && scope == p.tx.kind() {
		p.note(src, got)
		if c == callQueryOpen && got.failed == "" && got.columns != "" {
			failure = p.openRows(src, runs[0])
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

// openRows runs the query src, with args, again in the open transaction on both sides, after its
// outcome was compared, reads the first row of each, and leaves the rows open for the next call.
func (p *pair) openRows(src string, args []any) string {
	tb := p.tb
	tb.Helper()
	t := p.tx
	var err error
	t.sRows, err = t.s.QueryContext(tb.Context(), src, args...)
	is.NotError(tb, err)
	t.oRows, err = t.o.QueryContext(tb.Context(), firstStatement(src), args...)
	is.NotError(tb, err)
	got, want := firstRow(tb, t.sRows), firstRow(tb, t.oRows)
	if got != want {
		return fmt.Sprintf("%q: the first row of s4ql's rows that are left open is %v, and of SQLite's %v", src, got, want)
	}
	if got == "none" {
		// database/sql closes rows whose Next found no row.
		p.closeRows()
		return ""
	}
	if p.y != nil {
		p.y[t.kind()+": rows left open"]++
	}
	return ""
}

// firstRow of rows, with its values as [valueText] gives them, or "none".
func firstRow(tb testing.TB, rows *sql.Rows) string {
	tb.Helper()
	if !rows.Next() {
		is.NotError(tb, rows.Err())
		return "none"
	}
	columns, err := rows.Columns()
	is.NotError(tb, err)
	values := make([]any, len(columns))
	dest := make([]any, len(columns))
	for i := range values {
		dest[i] = &values[i]
	}
	is.NotError(tb, rows.Scan(dest...))
	var texts []string
	for _, v := range values {
		texts = append(texts, valueText(v))
	}
	return strings.Join(texts, ",")
}

// closeRows that [callQueryOpen] left open in the open transaction, on both sides.
func (p *pair) closeRows() {
	tb := p.tb
	tb.Helper()
	if t := p.tx; t != nil && t.sRows != nil {
		is.NotError(tb, t.sRows.Close())
		is.NotError(tb, t.oRows.Close())
		t.sRows, t.oRows = nil, nil
	}
}

// whileRowsOpen is [pair.stepRuns] of src through the call c, with the arguments of each run, on s
// and o, while rows of the open transaction are open, which it closes after the call. Where s4ql
// would run the statement in every run, it must refuse each with [rowsOpen] when the statement runs,
// and the call is not run in SQLite, which would run it. The open rows come after the checks of the
// text and of the arguments, so every other call is compared as without them; so is a query beside
// the transaction through the pool, which runs on another connection.
func (p *pair) whileRowsOpen(src string, c call, supported bool, runs [][]any, s, o runner, scope string) (outcome, string) {
	tb := p.tb
	tb.Helper()
	defer p.closeRows()
	got := run(tb, s, c, src, runs, s4qlText)
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
// transaction. The open transaction stays open on both sides, and rows that it left open are closed
// first.
func (p *pair) beginAgain(c call, opts *sql.TxOptions) (outcome, string) {
	tb := p.tb
	tb.Helper()
	ctx := tb.Context()
	p.closeRows()
	got := outcome{call: c}
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
// which database/sql closes before the end. Without a transaction it does nothing.
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
		return p.checkNotSupported(src, c, supported, runs, got)
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
		// so it is compared only for a statement that sets it. In slice 1 that is an insert.
		if s, _ := stmt.Check(src); s == nil || fmt.Sprintf("%T", s) != "*stmt.Insert" {
			got.affected, want.affected = "", ""
		}
	}
	if got.String() == want.String() {
		if s, _ := stmt.Check(src); s != nil && got.failed == "" {
			if ct, ok := s.(*stmt.CreateTable); ok {
				p.created[ct.Table.Value] = true
			}
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
	return fmt.Sprintf("%q with the arguments %#v: s4ql gives\n%v\nand SQLite\n%v", src, runs, got, want)
}

// checkQuoted s4ql's outcome got of src, which holds the statement s, run where SQLite runs on o,
// when s is a select whose first name that the table does not have is in double quotes. mattn's
// SQLite reads such a name as a string, where the pinned build of SQLite, which the engine's tests
// compare with, gives `no such column`, because it takes double quotes for names only (DQS=0). So
// such a select is not run in mattn's SQLite: each run must fail when the statement runs, since
// Prepare checks only the text, with the pinned build's error, `no such column: "x" - should this be
// a string literal in single-quotes?`. It reports whether s is such a select, and gives what
// differs, or "".
func (p *pair) checkQuoted(src string, s stmt.Statement, got outcome, o runner) (string, bool) {
	p.tb.Helper()
	sel, ok := s.(*stmt.Select)
	if !ok || sel.Columns == nil {
		return "", false
	}
	_, columns := p.columnsIn(o, sel.Table.Value)
	if columns == nil {
		return "", false
	}
	at := slices.IndexFunc(sel.Columns, func(n stmt.Name) bool { return stmttest.ColumnOf(columns, n) < 0 })
	if at < 0 || sel.Columns[at].Raw[0] != '"' {
		return "", false
	}
	want := fmt.Sprintf(`1/1: no such column: "%v" - should this be a string literal in single-quotes?`, sel.Columns[at].Value)
	for _, r := range got.runs() {
		if r.failed == "" || r.failed == "prepare" || r.err != want {
			return fmt.Sprintf("%q: s4ql gives %v, and the pinned build of SQLite %q when the statement runs", src, got, want), true
		}
	}
	p.y.excused(excusedQuoted)
	return "", true
}

// checkNotSupported is [pair.stepRuns] of a text that s4ql answers with "not supported", in got.
func (p *pair) checkNotSupported(src string, c call, supported bool, runs [][]any, got outcome) string {
	tb := p.tb
	tb.Helper()
	s, err := stmt.Check(src)
	if err == nil && s != nil {
		return p.checkRefused(src, s, runs, got)
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
// with an error of its own.
func (p *pair) checkRefused(src string, s stmt.Statement, runs [][]any, got outcome) string {
	if len(got.runs()) != len(runs) {
		return fmt.Sprintf("%q with %v runs: s4ql gives %v", src, len(runs), got)
	}
	kind := excusedType
	for i, o := range got.runs() {
		args := runs[i]
		want := argumentRefusal(args)
		if want != "" {
			kind = excusedArgument
		} else if len(args) == s.Parameters() {
			want = p.typeRefusal(src, s, args)
		}
		if len(args) != s.Parameters() || want == "" || o.failed == "prepare" || o.failed == "" || o.err != want {
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

// converted argument a, as database/sql converts a float32 to a float64 for the driver. Every other
// argument of the tests is as database/sql hands it, or is refused by its type.
func converted(a any) any {
	if f, ok := a.(float32); ok {
		return float64(f)
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
// which s4ql supports, when it is an insert that SQLite prepares where the call runs, outside a
// read-only transaction, with a value for each column, and its value of a column, a literal or an
// argument, is of another type than the column: for the first such column in the order of the table,
// as [stmttest.TypeRule] gives it. It is "" otherwise, also for an insert that leaves a column
// without a value, which gets SQLite's `NOT NULL constraint failed` first.
func (p *pair) typeRefusal(src string, s stmt.Statement, args []any) string {
	tb := p.tb
	tb.Helper()
	insert, ok := s.(*stmt.Insert)
	if !ok || (p.tx != nil && p.tx.readOnly) {
		return ""
	}
	_, o, _ := p.where(callExec, src)
	ps, err := o.PrepareContext(tb.Context(), firstStatement(src))
	if err != nil {
		return ""
	}
	is.NotError(tb, ps.Close())
	table, columns := p.columns(insert.Table.Value)
	var bound []any
	for _, a := range args {
		if v, ok := stmttest.Integers([]any{a}); ok {
			a = v[0]
		}
		bound = append(bound, converted(a))
	}
	return stmttest.TypeRule(table, columns, insert, bound)
}

// columns of the table with the name, as SQLite compares names, in SQLite where a call runs, in the
// order of the table, and the name of the table as the table stores it. They are nil and "" if
// SQLite has no such table there.
func (p *pair) columns(name string) (string, []stmttest.TableColumn) {
	p.tb.Helper()
	_, o, _ := p.where(callExec, "")
	return p.columnsIn(o, name)
}

// columnsIn are the [pair.columns] of the table with the name in SQLite on o.
func (p *pair) columnsIn(o runner, name string) (string, []stmttest.TableColumn) {
	tb := p.tb
	tb.Helper()
	rows, err := o.QueryContext(tb.Context(), `select m.name, p.name, p.type, p.pk, p."notnull" from sqlite_schema m, pragma_table_info(m.name) p where m.type = 'table' and m.name = ? collate nocase order by p.cid`, name)
	is.NotError(tb, err)
	defer func() { is.NotError(tb, rows.Close()) }()
	var table string
	var columns []stmttest.TableColumn
	for rows.Next() {
		var c stmttest.TableColumn
		var pk, notNull int
		is.NotError(tb, rows.Scan(&table, &c.Name, &c.Type, &pk, &notNull))
		// The key of a table WITHOUT ROWID never takes NULL, also when SQLite reports it without NOT
		// NULL.
		c.PrimaryKey = pk == 1
		c.Nullable = !c.PrimaryKey && notNull == 0
		columns = append(columns, c)
	}
	is.NotError(tb, rows.Err())
	return table, columns
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
		if got.String() != want.String() {
			return fmt.Sprintf("the table %q: s4ql holds\n%v\nand SQLite\n%v", name, got, want)
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
	// want is the outcome that both sides must give, as [outcome.String] writes it, or "" to compare
	// the two sides only.
	want string
}

const (
	t1 = "create table t1 (v1 int primary key) strict, without rowid"
	// t3 is the table of the example of slice 5: three columns of three types, with the key first.
	t3 = "create table t3 (v1 int primary key, v2 text not null, v3 real not null) strict, without rowid"
)

// sequences written by hand, which cover every statement of slice 1, every error that an
// application can get from it, and each call.
var sequences = map[string][]step{
	"the example of slice 1": {
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows `},
		{callExec, "insert into t1 (v1) values (1)", "exec: 1 affected, last insert id 0"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows int64:1`},
		{callExec, "drop table t1", "exec: 1 affected, last insert id 0"},
	},
	"several rows in the order of the key, through each call": {
		{callPrepareExec, t1, "prepare, exec: 0 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (3)", "exec: 1 affected, last insert id 0"},
		{callPrepareExec, "insert into t1 (v1) values (-9223372036854775808)", "prepare, exec: 1 affected, last insert id 0"},
		{callQuery, "insert into t1 (v1) values (9223372036854775807)", "query"},
		{callQueryRow, "insert into t1 (v1) values (0)", "query row: scan failed with no rows"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows int64:-9223372036854775808 int64:0 int64:3 int64:9223372036854775807`},
		{callPrepareQuery, "select * from t1", `prepare, query: columns "v1" of types "INT", rows int64:-9223372036854775808 int64:0 int64:3 int64:9223372036854775807`},
		{callQueryRow, "select * from t1", "query row: row int64:-9223372036854775808"},
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
		{callQuery, `select * from "FOO BAR"`, `query: columns "Bar\"x" of types "INT", rows int64:7`},
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
		{callPrepareExec, "drop table if exists t1", "prepare, exec: prepare failed with s4ql: not supported: IF EXISTS (at offset 11)"},
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
		{callExecArgument, "select * from t1 where v1 = 1", "exec with an argument: exec failed with s4ql: not supported: WHERE clause (at offset 17)"},
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
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows int64:1`},
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
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows int64:1 int64:2`},
	},
	"a semicolon and a comment after the statement": {
		{callExec, t1 + "; -- c", "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (1);;", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (2); /* c */", "exec: 1 affected, last insert id 0"},
		{callQuery, "select * from t1 ; -- c", `query: columns "v1" of types "INT", rows int64:1 int64:2`},
	},
	"a write transaction that commits, with arguments and prepared statements": {
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callCommit, "", "commit"},
		{callBegin, "", "begin"},
		{callExec, "insert into t1 (v1) values (1)", "exec: 1 affected, last insert id 0"},
		{callExecArgument, "insert into t1 (v1) values (?)", "exec with an argument: exec failed with 19/1555: UNIQUE constraint failed: t1.v1"},
		{callPrepareExecRuns, "insert into t1 (v1) values (?)", "prepare, exec several times: exec failed with 19/1555: UNIQUE constraint failed: t1.v1; then: 1 affected, last insert id 0; then: exec failed with 19/1555: UNIQUE constraint failed: t1.v1"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows int64:1 int64:2`},
		{callQueryRow, "select * from t1", "query row: row int64:1"},
		{callExec, "create table t2 (v2 int primary key) strict, without rowid", "exec: 0 affected, last insert id 0"},
		// Through the pool, mattn's Prepare outside the transaction does not find t2.
		{callStmtExec, "insert into t2 (v2) values (5)", "prepare outside, exec in the transaction: 1 affected, last insert id 0"},
		{callStmtQuery, "select * from t2", `prepare outside, query in the transaction: columns "v2" of types "INT", rows int64:5`},
		{callStmtExec, "insert into t2 (v2) values (?)", "prepare outside, exec in the transaction: exec failed with s4ql: wrong number of arguments: 0 for a statement with 1 parameter"},
		{callPrepareQuery, "select * from t2", `prepare, query: columns "v2" of types "INT", rows int64:5`},
		{callCommit, "", "commit"},
		{callQuery, "select * from t2", `query: columns "v2" of types "INT", rows int64:5`},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows int64:1 int64:2`},
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
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows int64:1`},
		{callQuery, "select * from t2", "query: query failed with 1/1: no such table: t2"},
	},
	"a read-only transaction": {
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (1)", "exec: 1 affected, last insert id 0"},
		{callBeginReadOnly, "", "begin read-only"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows int64:1`},
		{callExec, "insert into t1 (v1) values (2)", "exec: exec failed with 8/8: attempt to write a readonly database"},
		{callExec, "create table t2 (v2 int primary key) strict, without rowid", "exec: exec failed with 8/8: attempt to write a readonly database"},
		{callExec, "drop table t1", "exec: exec failed with 8/8: attempt to write a readonly database"},
		{callExec, "insert into t9 (v1) values (1)", "exec: exec failed with 1/1: no such table: t9"},
		{callExec, t1, "exec: exec failed with 1/1: table t1 already exists"},
		{callExecArgument, "insert into t1 (v1) values (?)", "exec with an argument: exec failed with 8/8: attempt to write a readonly database"},
		{callPrepareExecRuns, "insert into t1 (v1) values (?)", "prepare, exec several times: exec failed with 8/8: attempt to write a readonly database; then: exec failed with 8/8: attempt to write a readonly database; then: exec failed with 8/8: attempt to write a readonly database"},
		{callStmtQuery, "select * from t1", `prepare outside, query in the transaction: columns "v1" of types "INT", rows int64:1`},
		{callQueryRow, "select * from t1", "query row: row int64:1"},
		{callCommit, "", "commit"},
		{callExec, "insert into t1 (v1) values (2)", "exec: 1 affected, last insert id 0"},
		{callBeginReadOnly, "Serializable", "begin read-only"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows int64:1 int64:2`},
		{callRollback, "", "rollback"},
	},
	"rows of a transaction left open": {
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (1)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (2)", "exec: 1 affected, last insert id 0"},
		{callBegin, "", "begin"},
		{callQueryOpen, "select * from t1", `query, rows left open: columns "v1" of types "INT", rows int64:1 int64:2`},
		{callExec, "insert into t1 (v1) values (3)", "exec: exec failed with " + rowsOpen},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows int64:1 int64:2`},
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
		{callQueryOpen, "select * from t1", `query, rows left open: columns "v1" of types "INT", rows int64:1 int64:2`},
		{callExec, "insert into t1 (v1) values (3)", "exec: exec failed with " + rowsOpen},
		{callQueryOpen, "select * from t1", ""},
		{callRollback, "", "rollback"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows int64:1 int64:2`},
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
		{callQuery, "select v2, v1, v2 from t3", `query: columns "v2,v1,v2" of types "TEXT,INT,TEXT", rows string:a,int64:1,string:a string:b,int64:2,string:b`},
		{callQuery, "select * from t3", `query: columns "v1,v2,v3" of types "INT,TEXT,REAL", rows int64:1,string:a,float64:1.5 int64:2,string:b,float64:2.5`},
		{callPrepareQuery, "select V3, \"V1\" from T3", `prepare, query: columns "v3,v1" of types "REAL,INT", rows float64:1.5,int64:1 float64:2.5,int64:2`},
		{callQueryRow, "select v2 from t3", "query row: row string:a"},
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
		{callQueryRow, `select V1 from t3`, "query row: row int64:1"},
		{callQueryRow, `select V1, v3, v2 from t3`, "query row: row int64:1,float64:1.5,string:a"},
		// SQLite gives these errors when it prepares the text, and s4ql when the statement runs.
		{callPrepareExec, "insert into t3 values (2, 'b')", "prepare, exec: exec failed with 1/1: table t3 has 3 columns but 2 values were supplied"},
		{callPrepareExec, "insert into t3 (v1) values (2, 'b')", "prepare, exec: exec failed with 1/1: 2 values for 1 columns"},
		{callPrepareQuery, "select x from t3", "prepare, query: query failed with 1/1: no such column: x"},
		{callPrepareExec, "create table t4 (v int primary key, V text not null) strict, without rowid", "prepare, exec: exec failed with 1/1: duplicate column name: V"},
		// This both sides give at the same step.
		{callPrepareExec, "insert into t3 (v1) values (2)", "prepare, exec: exec failed with 19/1299: NOT NULL constraint failed: t3.v2"},
		{callQuery, "select * from t3", `query: columns "v1,v2,v3" of types "INT,TEXT,REAL", rows int64:1,string:a,float64:1.5`},
	},
	"parameters numbered across the values of an insert of several columns": {
		{callExec, "create table k (a text not null, b int primary key) strict, without rowid", "exec: 0 affected, last insert id 0"},
		{callExecArgument, "insert into k (a, b) values ('x', ?)", "exec with an argument: 1 affected, last insert id 0"},
		{callPrepareExecRuns, "insert into k values ('y', ?)", "prepare, exec several times: exec failed with 19/1555: UNIQUE constraint failed: k.b; then: 1 affected, last insert id 0; then: exec failed with 19/1555: UNIQUE constraint failed: k.b"},
		{callExecArgument, "insert into k (b, a) values (?, ?)", "exec with an argument: exec failed with s4ql: wrong number of arguments: 1 for a statement with 2 parameters"},
		{callExecArgument, "insert into k (b, a) values (3, ?)", "exec with an argument: exec failed with s4ql: not supported: argument 1, which is an integer, for the column k.a of type TEXT"},
		{callQuery, "select * from k", `query: columns "a,b" of types "TEXT,INT", rows string:x,int64:1 string:y,int64:2`},
	},
	"a transaction of several columns, with a failed insert that changes nothing": {
		{callExec, t3, "exec: 0 affected, last insert id 0"},
		{callBegin, "", "begin"},
		{callExec, "insert into t3 values (1, 'a', 1.5)", "exec: 1 affected, last insert id 0"},
		{callExec, "insert into t3 (v1, v2) values (2, 'b')", "exec: exec failed with 19/1299: NOT NULL constraint failed: t3.v3"},
		{callExec, "insert into t3 values (1, 'b', 2.5)", "exec: exec failed with 19/1555: UNIQUE constraint failed: t3.v1"},
		{callQuery, "select v2 from t3", `query: columns "v2" of types "TEXT", rows string:a`},
		{callCommit, "", "commit"},
		{callBeginReadOnly, "", "begin read-only"},
		{callExec, "insert into t3 values (2, 'b')", "exec: exec failed with 1/1: table t3 has 3 columns but 2 values were supplied"},
		{callExec, "insert into t3 (v1, v2) values (2, 'b')", "exec: exec failed with 8/8: attempt to write a readonly database"},
		{callExec, "insert into t3 values (2, 'b', 'c')", "exec: exec failed with 8/8: attempt to write a readonly database"},
		{callQuery, "select v3, v1 from t3", `query: columns "v3,v1" of types "REAL,INT", rows float64:1.5,int64:1`},
		{callRollback, "", "rollback"},
	},
	"a query beside the transaction": {
		{callExec, t1, "exec: 0 affected, last insert id 0"},
		{callExec, "insert into t1 (v1) values (1)", "exec: 1 affected, last insert id 0"},
		{callBegin, "", "begin"},
		{callExec, "insert into t1 (v1) values (2)", "exec: 1 affected, last insert id 0"},
		// On a pinned connection in the transaction, and through the pool outside it.
		{callBeside, "select * from t1", `query beside the transaction: columns "v1" of types "INT", rows int64:1 int64:2`},
		{callBeside, "insert into t1 (v1) values (3)", "query beside the transaction"},
		{callQueryOpen, "select * from t1", ""},
		{callBeside, "select * from t1", ""},
		{callRollback, "", "rollback"},
		{callBeside, "select * from t1", `query beside the transaction: columns "v1" of types "INT", rows int64:1`},
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
			for _, pinned := range []bool{true, false} {
				t.Run(fmt.Sprintf("should give what SQLite gives for a table of %v with the key at %v, on a pinned connection: %v", strings.Join(types, ", "), key, pinned), func(t *testing.T) {
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
						if i == key {
							c = created[i] + " " + typ + " primary key"
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
					all, reversed := make([]int, n), make([]int, n)
					for i := range n {
						all[i], reversed[i] = i, n-1-i
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
					p.close()
				})
			}
		}
	}
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
					// takes, so the outcomes written down hold on a pinned connection.
					if s.want != "" && pinned {
						is.Equal(t, s.want, got.String(), s.src)
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
}

// pick the call of a text of a sequence, which is a query if query, from r, in a transaction if inTx:
// now and then a begin, of a read-only transaction one time in three, or in a transaction an end, a
// commit two times in three, and otherwise a call for a query or for a statement that is no query,
// of which one in eight is a prepared statement that runs several times. In a transaction, a call
// also prepares its statement outside the transaction, leaves the rows of its query open, or queries
// beside the transaction, and one call in forty is a begin.
func pick(r stmttest.Rand, query, inTx bool) call {
	switch {
	case !inTx && r.IntN(10) == 0, inTx && r.IntN(40) == 0:
		if r.IntN(3) == 0 {
			return callBeginReadOnly
		}
		return callBegin
	case inTx && r.IntN(7) == 0:
		if r.IntN(3) == 0 {
			return callRollback
		}
		return callCommit
	}
	if query && inTx {
		return []call{callPrepareQuery, callQueryRow, callStmtQuery, callQueryOpen, callQueryOpen, callQueryOpen, callBeside, callQuery}[r.IntN(8)]
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
// the run has the same number and the same types of arguments.
func again(q *stmttest.Sequence, args []any) []any {
	next := slices.Clone(args)
	for i, a := range next {
		next[i] = q.Like(a)
	}
	return next
}

// generated runs the sequence of n texts from seed on a new pair in the world w, each with its
// arguments, and compares what each side gives for each, and at the end every table. A prepared
// statement that runs several times runs one to three more times, with other values. Transactions
// begin and end within the sequence, as [pick] chooses, the calls between run in them, and every
// table is compared after each; one that is open at the end commits or rolls back. Every second
// sequence runs on pinned connections, and every other one through the pool. Half of the sequences
// then restart the instance of s4ql, with an empty cache or with its own, and compare every table
// again.
func generated(t *testing.T, w *world, seed uint64, n int, y tally) {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, 0xd51))
	q, err := stmttest.NewSequence(r)
	is.NotError(t, err)
	p := newPair(t, w, seed%2 == 0, y)
	var log []string
	fail := func(failure string) {
		t.Helper()
		t.Fatalf("seed %v, pinned %v, after\n%v\n%v", seed, p.pinned, strings.Join(log, "\n"), failure)
	}
	for range n {
		next := q.Next()
		c := pick(r, next.Query, p.tx != nil)
		src, supported, runs := next.Src, next.Supported, [][]any{next.Args}
		switch c {
		case callBegin, callBeginReadOnly:
			src, supported, runs = pickLevel(r), false, nil
		case callCommit, callRollback:
			src, supported, runs = "", false, nil
		case callPrepareExecRuns:
			for range 1 + r.IntN(3) {
				runs = append(runs, again(q, next.Args))
			}
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
	if seed%4 >= 2 {
		empty := seed%8 >= 4
		log = append(log, fmt.Sprint("-- restart, with an empty cache: ", empty))
		p.restart(empty)
		if failure := p.compareTables(); failure != "" {
			fail(failure)
		}
	}
	p.close()
}

// TestGenerated runs sequences of the generator that the engine's differential test uses, through
// each call, with their arguments, on both sides, and requires the same outcomes and the same tables.
// It fails if a kind of outcome is never reached, and it pins how many texts each kind of excuse lets
// through, so that a change that excuses more cannot pass unseen.
func TestGenerated(t *testing.T) {
	const n, length = 200, 40
	w := newWorld(t)
	y := tally{}
	for seed := range uint64(n) {
		generated(t, w, seed, length, y)
	}
	t.Logf("%v sequences of %v texts: %v", n, length, y)

	for _, kind := range []string{
		"createtable ok", "createtable already exists", "createtable no rows", "createtable wrong number of arguments",
		"insert ok", "insert UNIQUE constraint failed", "insert has no column named", "insert no such table", "insert no rows", "insert wrong number of arguments",
		"insert with a parameter ok", "insert with a parameter UNIQUE constraint failed", "insert with a parameter no such table",
		"insert with a parameter wrong number of arguments", "insert with a parameter argument of a type not supported", "insert with a parameter named argument",
		"select ok", "select ok with rows", "select no such table", "select no rows", "select wrong number of arguments",
		"droptable ok", "droptable no such table",
		"other not supported", "other syntax error", "other incomplete input", "begin ok",
		"call " + string(callExec), "call " + string(callQuery), "call " + string(callPrepareExec), "call " + string(callPrepareQuery),
		"call " + string(callQueryRow), "call " + string(callBegin), "call " + string(callPrepareExecRuns),
		"call " + string(callExec) + " with arguments", "call " + string(callQuery) + " with arguments",
		"call " + string(callPrepareExec) + " with arguments", "call " + string(callPrepareQuery) + " with arguments",
		"call " + string(callQueryRow) + " with arguments", "call " + string(callPrepareExecRuns) + " with arguments",

		// Write transactions that commit and that roll back, on a pinned connection and through the
		// pool, after a failed call, an insert, a create table, and a drop table, and while rows of
		// theirs are open.
		"write transaction begin ok", "call " + string(callBegin) + " in a transaction",
		"write transaction commit ok on a pinned connection", "write transaction commit ok through the pool",
		"write transaction rollback ok on a pinned connection", "write transaction rollback ok through the pool",
		"write transaction commit ok after a failed call", "write transaction rollback ok after a failed call",
		"write transaction commit ok after an insert", "write transaction rollback ok after an insert",
		"write transaction commit ok after a create table", "write transaction rollback ok after a create table",
		"write transaction commit ok after a drop table", "write transaction rollback ok after a drop table",
		"write transaction commit ok while rows are open", "write transaction rollback ok while rows are open",
		// Each call in a write transaction, with arguments, and what it gives.
		"write transaction: call " + string(callExec) + " with arguments", "write transaction: call " + string(callQuery) + " with arguments",
		"write transaction: call " + string(callQueryRow) + " with arguments", "write transaction: call " + string(callPrepareExec) + " with arguments",
		"write transaction: call " + string(callPrepareQuery) + " with arguments", "write transaction: call " + string(callPrepareExecRuns) + " with arguments",
		"write transaction: call " + string(callStmtExec) + " with arguments", "write transaction: call " + string(callStmtQuery) + " with arguments",
		"write transaction: call " + string(callQueryOpen), "write transaction: call " + string(callBeside),
		"write transaction: insert ok", "write transaction: insert with a parameter ok", "write transaction: insert UNIQUE constraint failed",
		"write transaction: insert no such table", "write transaction: createtable ok", "write transaction: createtable already exists",
		"write transaction: droptable ok", "write transaction: droptable no such table", "write transaction: other syntax error",
		"write transaction: insert with a parameter wrong number of arguments", "write transaction: insert with a parameter argument of a type not supported",
		"write transaction: select ok with rows after a write of the transaction", "write transaction: rows left open",
		"write transaction: insert while rows are open", "write transaction: select while rows are open",
		// Through the pool, a query outside a write transaction, which must not see its changes.
		"beside a write transaction: select ok with rows",
		// Read-only transactions: their queries, and their writes, which are refused.
		"read-only transaction begin ok",
		"read-only transaction commit ok on a pinned connection", "read-only transaction commit ok through the pool",
		"read-only transaction rollback ok on a pinned connection", "read-only transaction rollback ok through the pool",
		"read-only transaction: select ok with rows", "read-only transaction: rows left open",
		"read-only transaction: insert attempt to write a readonly database", "read-only transaction: insert with a parameter attempt to write a readonly database",
		"read-only transaction: createtable attempt to write a readonly database", "read-only transaction: droptable attempt to write a readonly database",
		"read-only transaction: insert no such table", "read-only transaction: createtable already exists",
		"read-only transaction: call " + string(callStmtExec), "read-only transaction: call " + string(callStmtQuery),
		"read-only transaction: call " + string(callPrepareExecRuns) + " with arguments",
		"read-only transaction: insert with a parameter while rows are open",
		"call " + string(callBeginReadOnly) + " in a transaction",
	} {
		if y[kind] == 0 {
			t.Errorf("no text has the outcome %q", kind)
		}
	}

	// What the run must reach, at least: each type as a key, with an insert that succeeds, meets a key
	// that exists, has a value of another type, and is refused in a read-only transaction; each kind of
	// literal and of argument, also those that no column holds. Each least number is about a third of
	// what the run gave on 2026-11-19, so that a generator that stops reaching a case fails the test.
	for kind, least := range map[string]int{
		"insert into INT ok": 50, "insert into INTEGER ok": 50, "insert into REAL ok": 50, "insert into TEXT ok": 50, "insert into BLOB ok": 50,
		"insert into INT UNIQUE constraint failed": 20, "insert into INTEGER UNIQUE constraint failed": 30, "insert into REAL UNIQUE constraint failed": 30,
		"insert into TEXT UNIQUE constraint failed": 24, "insert into BLOB UNIQUE constraint failed": 26,
		"insert into INT a value of another type": 8, "insert into INTEGER a value of another type": 20, "insert into REAL a value of another type": 15,
		"insert into TEXT a value of another type": 12, "insert into BLOB a value of another type": 12,
		"insert into INT attempt to write a readonly database": 10, "insert into INTEGER attempt to write a readonly database": 12,
		"insert into REAL attempt to write a readonly database": 6, "insert into TEXT attempt to write a readonly database": 13,
		"insert into BLOB attempt to write a readonly database": 14,
		"literal integer": 270, "literal real": 120, "literal real -0.0": 20, "literal text": 120, "literal empty text": 20, "literal blob": 100, "literal empty blob": 25,
		"argument int64": 150, "argument int": 1, "argument int32": 3, "argument uint64": 5, "argument float64": 70, "argument float64 -0.0": 10,
		"argument float32": 5, "argument string": 60, "argument empty string": 9, "argument []uint8": 50, "argument empty []byte": 12,
		"argument NaN": 2, "argument infinite float64": 2, "argument float32 that is not finite": 1, "argument string that is not valid UTF-8": 3,
		"argument string with NUL": 4, "argument nil": 1, "argument nil []byte": 6, "argument bool": 2, "argument time.Time": 2,
		"argument uint64 above int64": 1, "argument named": 23,

		// Slice 5, as the run gave it on 2026-11-20: inserts into tables of several columns, without a
		// list and with one, with each fault and with two, also in a read-only transaction, and selects of
		// * of each number of columns, of names, and of a name that the table does not have.
		"insert without a list ok": 64, "insert with a list ok": 100, "insert without a list UNIQUE constraint failed": 27, "insert with a list UNIQUE constraint failed": 52,
		"insert with a list attempt to write a readonly database": 22, "insert without a list attempt to write a readonly database": 10,
		"insert with a column that the table does not have: has no column named": 19, "insert with a column without a value: NOT NULL constraint failed": 31,
		"insert with the wrong number of values: values were supplied": 18, "insert with the wrong number of values:  values for ": 30,
		"insert with a value of another type in the key: a value of another type": 10, "insert with a value of another type in another column: a value of another type": 10,
		"insert with two faults: NOT NULL constraint failed": 5, "insert with two faults: a value of another type": 7, "insert with two faults: has no column named": 3,
		"insert with a column without a value: attempt to write a readonly database": 2,
		"select * of 1 columns ok": 49, "select * of 2 columns ok": 52, "select * of 3 columns ok": 40, "select of names ok": 130,
		"select of a name that the table does not have no such column": 24,
	} {
		if y["reached: "+kind] < least {
			t.Errorf("the run reaches %q %v times, and at least %v were expected: the generator stopped reaching it", kind, y["reached: "+kind], least)
		}
	}

	// The texts that each kind of excuse lets through, by kind: those that the comparison accepts
	// where SQLite and s4ql differ, and the calls that s4ql refuses by its rule, which are checked
	// against the rule instead of SQLite. Every other kind is pinned at 0. Each number counts calls:
	// a text that SQLite refuses at its prepare and s4ql when it runs ([excusedPrepare]); a create
	// table that SQLite finds started ([excusedStartedTable]); a call with another number of
	// arguments than parameters ([excusedArguments]), or with an argument that s4ql does not support
	// ([excusedArgument]); an insert of a value of another type than its column ([excusedType]); a
	// begin with an isolation level that s4ql refuses ([excusedLevel]); a
	// statement while rows of its transaction are open, which s4ql refuses ([excusedRowsOpen]); a
	// begin on a pinned connection whose transaction is open, which both refuse ([excusedTxOpen]); and
	// a statement prepared outside a transaction through the pool, whose names only the transaction
	// has ([excusedOutside]); and a select of a name in double quotes that is no column, which mattn's
	// SQLite reads as a string ([excusedQuoted]).
	// Before the sequence wrote tables of several columns, the counts were 1012, 12, 395, 126, 180,
	// 78, 38, 22, 49, and 0. The sequence draws other random numbers, which moves every count, the
	// calls and the transactions too; and of its own: the errors of slice 5 that SQLite gives at
	// prepare took excusedPrepare up, inserts of up to four values have more arguments, which took
	// excusedArgument up, one insert in ten has a value of any type, where one value in ten had,
	// which took excusedType down, and select lists name columns in double quotes, which
	// excusedQuoted counts. The engine and the driver of slice 6 took excusedPrepare from 1072 to
	// 1075 and excusedArgument from 189 to 184, with the generator drawing as before: a nil argument
	// is NULL, which s4ql takes, so 5 calls with one are no longer refused for it, and one of them
	// meets SQLite's error of a name at prepare; and two inserts of the literal NULL of
	// [stmttest.Others], which s4ql refused as not supported, meet such an error too.
	want := map[string]int{
		excusedPrepare: 1075, excusedStartedTable: 9, excusedArguments: 396, excusedArgument: 184, excusedType: 161,
		excusedLevel: 70, excusedRowsOpen: 29, excusedTxOpen: 30, excusedOutside: 68, excusedQuoted: 32,
	}
	kinds := slices.Collect(maps.Keys(want))
	for name := range y {
		if kind, ok := strings.CutPrefix(name, "excused: "); ok && !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	slices.Sort(kinds)
	for _, kind := range kinds {
		if got := y["excused: "+kind]; got != want[kind] {
			t.Errorf("texts excused as %v: %v, but the test pins %v. Change the pinned number only on purpose, and say why in the commit", kind, got, want[kind])
		}
	}
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
