package drivercheck_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"maps"
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
	// callBegin is BeginTx, which runs no text, and whose transaction is rolled back at once.
	callBegin call = "begin"
	// callQueryArgument is Query with one argument, the integer 1, as [callExecArgument] is Exec.
	callQueryArgument call = "query with an argument"
	// callPrepareExecRuns is Prepare, and then Exec of the statement once for each run of arguments:
	// in the sequences written by hand and in the fuzz target, the integers 1, 2, and 1, so that the
	// third run meets the key of the first.
	callPrepareExecRuns call = "prepare, exec several times"
)

// calls in the order of the bytes of the fuzz target.
var calls = []call{callExec, callQuery, callPrepareExec, callPrepareQuery, callQueryRow, callExecArgument, callBegin, callQueryArgument, callPrepareExecRuns}

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
	// failed is the step of the call that failed: "prepare", "exec", "query", "rows", "scan", or
	// "begin", or "" if none failed.
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

// queryer is what [sql.DB] and [sql.Conn] have in common: where a text runs.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// run src through the call c on q, with the arguments of each run, and with errText for the text of
// an error. Every call but [callPrepareExecRuns] makes one run, with the arguments of the first, and
// [callBegin] none.
func run(tb testing.TB, q queryer, c call, src string, runs [][]any, errText func(testing.TB, error) string) outcome {
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
		res, err := q.ExecContext(ctx, src, args...)
		if err != nil {
			return failed("exec", err)
		}
		return result(tb, o, res)
	case callQuery, callQueryArgument:
		rows, err := q.QueryContext(ctx, src, args...)
		if err != nil {
			return failed("query", err)
		}
		return read(tb, o, rows, errText)
	case callPrepareExec, callPrepareQuery, callPrepareExecRuns:
		s, err := q.PrepareContext(ctx, src)
		if err != nil {
			return failed("prepare", err)
		}
		defer func() { is.NotError(tb, s.Close()) }()
		switch c {
		case callPrepareExec:
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
		// database/sql gives the error of the query at Scan, and one destination is enough for slice
		// 1, whose queries have one column.
		var v any
		if err := q.QueryRowContext(ctx, src, args...).Scan(&v); err != nil {
			return failed("scan", err)
		}
		o.rows = []string{valueText(v)}
		return o
	case callBegin:
		tx, err := q.BeginTx(ctx, nil)
		if err != nil {
			return failed("begin", err)
		}
		is.NotError(tb, tx.Rollback())
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
		for _, sentinel := range []error{s4ql.ErrNotServing, s4ql.ErrOutcomeUnknown, s4ql.ErrStopped} {
			if errors.Is(err, sentinel) {
				tb.Fatalf("s4ql gives %q, which matches %v", err, sentinel)
			}
		}
		return err.Error()
	}
	tb.Fatalf("s4ql gives %v", err)
	return ""
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
// parameter if it has one.
func (y tally) add(src string, got outcome, runs [][]any) {
	if y == nil {
		return
	}
	kind := "other"
	if s, _ := stmt.Check(src); s != nil && got.call != callBegin {
		kind = strings.ToLower(strings.TrimPrefix(fmt.Sprintf("%T", s), "*stmt."))
		if s.Parameters() > 0 {
			kind += " with a parameter"
		}
	}
	if got.call == callBegin {
		kind = "begin"
	}
	y["call "+string(got.call)]++
	if slices.ContainsFunc(runs, func(args []any) bool { return len(args) > 0 }) && got.call != callBegin {
		y["call "+string(got.call)+" with arguments"]++
	}
	for _, o := range got.runs() {
		switch {
		case o.failed == "":
			y[kind+" ok"]++
			if len(o.rows) > 0 && o.columns != "" {
				y[kind+" ok with rows"]++
			}
		case strings.HasPrefix(o.err, "s4ql: not supported: the named argument "):
			y[kind+" named argument"]++
		case strings.HasPrefix(o.err, "s4ql: not supported: argument "):
			y[kind+" argument of a type not supported"]++
		case o.notSupported():
			y[kind+" not supported"]++
		default:
			y[kind+" "+messageKind(o.err)]++
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
	for _, kind := range []string{"already exists", "no such table", "has no column named", "UNIQUE constraint failed", "syntax error", "incomplete input", "no rows", "wrong number of arguments"} {
		if strings.Contains(err, kind) {
			return kind
		}
	}
	return err
}

// The kinds of the texts that the comparison excuses, under which [tally.excused] counts them.
const (
	// excusedPrepare is a text whose error SQLite gives when it prepares the text, and s4ql when the
	// statement runs, because s4ql's Prepare checks only the text: `no such table`, `already exists`,
	// and `has no column named` through Prepare.
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
	// another type, which SQLite binds: it is checked against s4ql's rule by
	// [pair.checkArgumentRefused], and not run in SQLite.
	excusedArgument = "an argument that s4ql does not support, checked against s4ql's rule"
)

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
}

// newPair on a new database of the world w, on a pinned connection of each side if pinned, with the
// outcomes counted in y.
func newPair(tb testing.TB, w *world, pinned bool, y tally) *pair {
	tb.Helper()
	p := &pair{tb: tb, w: w, prefix: fmt.Sprint("db", w.prefixes.Add(1)), cache: filepath.Join(tb.TempDir(), "cache"), pinned: pinned, created: map[string]bool{}, y: y}
	p.s4ql = open(tb, w.options(p.prefix, p.cache))
	var err error
	// A file, so that every connection of the pool reaches the same database.
	p.sqlite, err = sql.Open("sqlite3", filepath.Join(tb.TempDir(), "oracle.db"))
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
func (p *pair) stepRuns(src string, c call, supported bool, runs [][]any) (outcome, string) {
	tb := p.tb
	tb.Helper()
	got := run(tb, p.s, c, src, runs, s4qlText)
	p.y.add(src, got, runs)
	return got, p.compare(src, c, supported, runs, got)
}

// compare s4ql's outcome got of src through the call c, with the arguments of each run, with
// SQLite's, as [pair.stepRuns] describes.
func (p *pair) compare(src string, c call, supported bool, runs [][]any, got outcome) string {
	tb := p.tb
	tb.Helper()
	if c == callBegin {
		// Both sides begin a transaction, and roll it back.
		if want := run(tb, p.o, c, src, runs, mattnText); got.failed != "" || got.String() != want.String() {
			return fmt.Sprintf("s4ql gives\n%v\nand SQLite\n%v", got, want)
		}
		return ""
	}
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
			if !(stmttest.Call{Src: src, Args: args, Supported: true}).Runs() {
				return fmt.Sprintf("%q with the arguments %#v: s4ql gives %v, and must refuse the arguments", src, args, got)
			}
		}
	}

	first := firstStatement(src)
	want := run(tb, p.o, c, first, mattnRuns, mattnText)
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
	case (c == callPrepareExec || c == callPrepareExecRuns) && got.failed == "exec" || c == callPrepareQuery && got.failed == "query":
		// SQLite gives the error when it prepares the text, and s4ql in each run of the statement.
		if want.failed == "prepare" && slices.Contains([]string{"no such table", "already exists", "has no column named"}, messageKind(got.err)) &&
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

// checkNotSupported is [pair.stepRuns] of a text that s4ql answers with "not supported", in got.
func (p *pair) checkNotSupported(src string, c call, supported bool, runs [][]any, got outcome) string {
	tb := p.tb
	tb.Helper()
	s, err := stmt.Check(src)
	if err == nil && s != nil {
		return p.checkArgumentRefused(src, s, runs, got)
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
	if (c == callPrepareExec || c == callPrepareQuery || c == callPrepareExecRuns) && got.failed != "prepare" {
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

// checkArgumentRefused is [pair.checkNotSupported] of the text src, which holds the statement s that
// s4ql supports, so that its "not supported" is of an argument: an error of s4ql's own, where SQLite
// binds every type, and mattn takes a name. Each run must have one argument for each parameter, and
// fail when the statement runs, since Prepare checks only the text, with the "not supported" of its
// first argument that s4ql does not support, as [argumentRefusal] gives it.
func (p *pair) checkArgumentRefused(src string, s stmt.Statement, runs [][]any, got outcome) string {
	for i, o := range got.runs() {
		args := runs[min(i, len(runs)-1)]
		want := argumentRefusal(args)
		if len(args) != s.Parameters() || want == "" || o.failed == "prepare" || o.failed == "" || o.err != want {
			return fmt.Sprintf("%q with the arguments %#v: s4ql gives %v, where the rule gives %q", src, args, got, want)
		}
	}
	if len(got.runs()) != len(runs) {
		return fmt.Sprintf("%q with %v runs: s4ql gives %v", src, len(runs), got)
	}
	p.y.excused(excusedArgument)
	return ""
}

// argumentRefusal is the "not supported" of the first argument of args that s4ql does not support,
// in order, its name before its type: one with a name, an unsigned integer above the largest int64,
// and one of another type than an integer, by its type as database/sql converts it. It is "" if s4ql
// supports every argument.
func argumentRefusal(args []any) string {
	for i, a := range args {
		if n, ok := a.(sql.NamedArg); ok {
			return fmt.Sprintf("s4ql: not supported: the named argument %q", n.Name)
		}
		if _, ok := stmttest.Integers([]any{a}); ok {
			continue
		}
		switch v := a.(type) {
		case uint64:
			return fmt.Sprintf("s4ql: not supported: argument %v of type uint64, whose value does not fit a signed 64-bit integer", i+1)
		case nil:
			return fmt.Sprintf("s4ql: not supported: argument %v of type nil", i+1)
		default:
			return fmt.Sprintf("s4ql: not supported: argument %v of type %T", i+1, v)
		}
	}
	return ""
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

const t1 = "create table t1 (v1 int primary key) strict, without rowid"

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
		{callExec, "create table t2 (v2 integer primary key) strict, without rowid", ""},
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
				is.Equal(t, "", p.compareTables())
				p.close()
			})
		}
	}
}

// pick the call of a text of a sequence, which is a query if query, from r: now and then BeginTx,
// and otherwise a call for a query or for a statement that is no query, of which one in eight is a
// prepared statement that runs several times.
func pick(r stmttest.Rand, query bool) call {
	if r.IntN(48) == 0 {
		return callBegin
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
	}
	return callExec
}

// again are the arguments args of a call of q for another run of its prepared statement: each int64
// replaced by another value of q, and every other argument as it is, so that the run has the same
// number and the same types of arguments.
func again(q *stmttest.Sequence, args []any) []any {
	next := slices.Clone(args)
	for i, a := range next {
		if _, ok := a.(int64); ok {
			next[i] = q.Value()
		}
	}
	return next
}

// generated runs the sequence of n texts from seed on a new pair in the world w, each with its
// arguments, and compares what each side gives for each, and at the end every table. A prepared
// statement that runs several times runs one to three more times, with other values. Every second
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
		c := pick(r, next.Query)
		src, supported, runs := next.Src, next.Supported, [][]any{next.Args}
		switch c {
		case callBegin:
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
	} {
		if y[kind] == 0 {
			t.Errorf("no text has the outcome %q", kind)
		}
	}

	// The texts that each kind of excuse lets through, by kind: those that the comparison accepts
	// where SQLite and s4ql differ, and the calls that s4ql refuses for their arguments by its rule,
	// which are checked against the rule instead of SQLite. Every other kind is pinned at 0.
	want := map[string]int{excusedPrepare: 1120, excusedStartedTable: 13, excusedArguments: 412, excusedArgument: 103}
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
// [TestGenerated] does. Its seeds are the sequences of
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
			if c == callBegin {
				src = ""
			}
			if _, failure := p.step(src, c, false); failure != "" {
				t.Fatal(failure)
			}
		}
		if failure := p.compareTables(); failure != "" {
			t.Fatal(failure)
		}
		p.close()
	})
}
