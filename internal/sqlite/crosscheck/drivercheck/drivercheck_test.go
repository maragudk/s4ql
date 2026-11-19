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
	sqloracle "maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/sqlerr"
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
	// callExecArgument is Exec with one argument, which s4ql does not support.
	callExecArgument call = "exec with an argument"
	// callBegin is BeginTx, which runs no text, and which s4ql does not support.
	callBegin call = "begin"
)

// calls in the order of the bytes of the fuzz target.
var calls = []call{callExec, callQuery, callPrepareExec, callPrepareQuery, callQueryRow, callExecArgument, callBegin}

// outcome of a text through a call: what an application sees.
type outcome struct {
	call call
	// failed is the step of the call that failed: "prepare", "exec", "query", "rows", "scan", or
	// "begin", or "" if none failed.
	failed string
	// err is the error of the failed step: "code/extended: message" for SQLite's, "no rows" for
	// [sql.ErrNoRows], and the message for s4ql's "not supported".
	err string
	// affected and lastID are of the [sql.Result] of an exec, or "" where there is none or it is not
	// compared.
	affected, lastID string
	// columns of a query, by their names and by the names of their database types, and the rows,
	// each value as its Go type and its value.
	columns, types string
	rows           []string
}

func (o outcome) String() string {
	var b strings.Builder
	b.WriteString(string(o.call))
	if o.failed != "" {
		fmt.Fprintf(&b, ": %v failed with %v", o.failed, o.err)
		return b.String()
	}
	if o.affected != "" || o.lastID != "" {
		fmt.Fprintf(&b, ": %v affected, last insert id %v", o.affected, o.lastID)
	}
	switch {
	case o.columns != "" || o.types != "":
		fmt.Fprintf(&b, ": columns %q of types %q, rows %v", o.columns, o.types, strings.Join(o.rows, " "))
	case len(o.rows) > 0:
		// A row of QueryRow, whose columns database/sql does not give.
		fmt.Fprintf(&b, ": row %v", strings.Join(o.rows, " "))
	}
	return b.String()
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

// run src through the call c on q, with errText for the text of an error.
func run(tb testing.TB, q queryer, c call, src string, errText func(testing.TB, error) string) outcome {
	tb.Helper()
	ctx := tb.Context()
	o := outcome{call: c}
	failed := func(step string, err error) outcome {
		o.failed, o.err = step, errText(tb, err)
		return o
	}
	switch c {
	case callExec, callExecArgument:
		var args []any
		if c == callExecArgument {
			args = []any{int64(1)}
		}
		res, err := q.ExecContext(ctx, src, args...)
		if err != nil {
			return failed("exec", err)
		}
		return result(tb, o, res)
	case callQuery:
		rows, err := q.QueryContext(ctx, src)
		if err != nil {
			return failed("query", err)
		}
		return read(tb, o, rows, errText)
	case callPrepareExec, callPrepareQuery:
		s, err := q.PrepareContext(ctx, src)
		if err != nil {
			return failed("prepare", err)
		}
		defer func() { is.NotError(tb, s.Close()) }()
		if c == callPrepareExec {
			res, err := s.ExecContext(ctx)
			if err != nil {
				return failed("exec", err)
			}
			return result(tb, o, res)
		}
		rows, err := s.QueryContext(ctx)
		if err != nil {
			return failed("query", err)
		}
		return read(tb, o, rows, errText)
	case callQueryRow:
		// database/sql gives the error of the query at Scan, and one destination is enough for slice
		// 1, whose queries have one column.
		var v any
		if err := q.QueryRowContext(ctx, src).Scan(&v); err != nil {
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

// add the outcome got of s4ql for src.
func (y tally) add(src string, got outcome) {
	if y == nil {
		return
	}
	kind := "other"
	if s, _ := stmt.Check(src); s != nil && got.call != callBegin {
		kind = strings.ToLower(strings.TrimPrefix(fmt.Sprintf("%T", s), "*stmt."))
	}
	if got.call == callBegin {
		kind = "begin"
	}
	y["call "+string(got.call)]++
	switch {
	case got.failed == "":
		y[kind+" ok"]++
		if len(got.rows) > 0 && got.columns != "" {
			y[kind+" ok with rows"]++
		}
	case got.notSupported():
		y[kind+" not supported"]++
	default:
		y[kind+" "+messageKind(got.err)]++
	}
}

// excused counts a text of the kind that the comparison excuses.
func (y tally) excused(kind string) {
	if y != nil {
		y["excused: "+kind]++
	}
}

// messageKind of an error's text: its message without the names.
func messageKind(err string) string {
	for _, kind := range []string{"already exists", "no such table", "has no column named", "UNIQUE constraint failed", "syntax error", "incomplete input", "no rows"} {
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
	// excusedArgumentSyntax is a text with a syntax error that runs with an argument: SQLite gives
	// the syntax error, and s4ql "not supported: parameters", which breaks the spec's rule that a
	// text with a syntax error gets SQLite's error. It is a difference that this test found, and
	// that [TestExecArgument] pins; see docs/todo.md.
	excusedArgumentSyntax = "a syntax error with an argument, which s4ql answers with not supported, a known difference"
	// knownGapSuffix ends the kind of a text of a [stmttest.KnownGap], after the gap.
	knownGapSuffix = ", a known gap"
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
	y     tally
}

// newPair on a new database of the world w, on a pinned connection of each side if pinned, with the
// outcomes counted in y.
func newPair(tb testing.TB, w *world, pinned bool, y tally) *pair {
	tb.Helper()
	p := &pair{tb: tb, w: w, prefix: fmt.Sprint("db", w.prefixes.Add(1)), cache: filepath.Join(tb.TempDir(), "cache"), pinned: pinned, y: y}
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
	tb.Cleanup(func() { is.NotError(tb, o.Close()) })
	p.s, p.o = s, o
}

// step runs src through the call c on both sides, and compares what each gives. A text that s4ql
// does not support is not run in SQLite: it must not be a statement of slice 1, which supported says
// it is, and SQLite must prepare it on an empty database without a syntax error. step gives s4ql's
// outcome, and what differs, or "".
func (p *pair) step(src string, c call, supported bool) (outcome, string) {
	tb := p.tb
	tb.Helper()
	got := run(tb, p.s, c, src, s4qlText)
	p.y.add(src, got)
	return got, p.compare(src, c, supported, got)
}

// compare s4ql's outcome got of src through the call c with SQLite's, as [pair.step] describes.
func (p *pair) compare(src string, c call, supported bool, got outcome) string {
	tb := p.tb
	tb.Helper()
	if got.notSupported() {
		return p.checkNotSupported(src, c, supported, got)
	}
	if c == callExecArgument || c == callBegin {
		return fmt.Sprintf("%q: s4ql gives %v, and supports no %v", src, got, c)
	}

	first := firstStatement(src)
	want := run(tb, p.o, c, first, mattnText)
	if !p.pinned {
		// Through the pool, the count of changes is that of the connection that database/sql takes,
		// so it is compared only for a statement that sets it. In slice 1 that is an insert.
		if s, _ := stmt.Check(src); s == nil || fmt.Sprintf("%T", s) != "*stmt.Insert" {
			got.affected, want.affected = "", ""
		}
	}
	if got.String() == want.String() {
		return ""
	}

	switch {
	case got.failed != "prepare" && want.failed == "prepare" && (c == callPrepareExec || c == callPrepareQuery) && got.err == want.err:
		p.y.excused(excusedPrepare)
		return ""
	case p.startedTable(src, c, got, want):
		p.y.excused(excusedStartedTable)
		return ""
	}
	if gap := knownGap(src, got, want); gap != "" {
		p.y.excused(gap + knownGapSuffix)
		return ""
	}
	return fmt.Sprintf("%q: s4ql gives\n%v\nand SQLite\n%v", src, got, want)
}

// checkNotSupported is [pair.step] of a text that s4ql answers with "not supported", in got.
func (p *pair) checkNotSupported(src string, c call, supported bool, got outcome) string {
	tb := p.tb
	tb.Helper()
	switch c {
	case callBegin:
		if got.err != "s4ql: not supported: transactions" {
			return fmt.Sprintf("s4ql gives %v", got)
		}
		return ""
	case callExecArgument:
		if got.err != "s4ql: not supported: parameters" {
			return fmt.Sprintf("%q: s4ql gives %v", src, got)
		}
		if _, err := stmt.Check(src); errors.As(err, new(*sqlerr.Error)) {
			p.y.excused(excusedArgumentSyntax)
			return ""
		}
	case callPrepareExec, callPrepareQuery:
		// The text alone decides "not supported", and Prepare checks the text.
		if got.failed != "prepare" {
			return fmt.Sprintf("%q: s4ql gives %v, after the statement was prepared", src, got)
		}
		fallthrough
	default:
		if supported {
			return fmt.Sprintf("%q: s4ql does not support a statement of slice 1: %v", src, got)
		}
	}
	// A text with a NUL byte or a byte that is not UTF-8 is not supported, whatever else it holds.
	if !utf8.ValidString(src) || strings.ContainsRune(src, 0) {
		return ""
	}
	s, err := p.empty.PrepareContext(tb.Context(), src)
	if err != nil {
		if m := mattnText(tb, err); sqloracle.IsSyntaxMessage(m[strings.Index(m, ": ")+2:]) {
			return fmt.Sprintf("%q: s4ql gives %v, and SQLite %v", src, got, m)
		}
		return ""
	}
	is.NotError(tb, s.Close())
	return ""
}

// startedTable reports whether s4ql's outcome got and SQLite's outcome want for the text src through
// the call c differ in the one way that the tests accept, which [stmttest.ExistsAfterStart]
// describes: SQLite gives that a table exists, and s4ql gives what SQLite gives for src on an empty
// database.
func (p *pair) startedTable(src string, c call, got, want outcome) bool {
	tb := p.tb
	tb.Helper()
	w, ok := strings.CutPrefix(want.err, "1/1: ")
	if !ok || !stmttest.ExistsAfterStart(src, w) {
		return false
	}
	// The text is a syntax error, so it changes nothing on the empty database.
	return got.String() == run(tb, p.empty, c, firstStatement(src), mattnText).String()
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

// compareTables of SQLite through a query of each on both sides, through the pool. It gives what
// differs, or "".
func (p *pair) compareTables() string {
	tb := p.tb
	tb.Helper()
	for _, name := range p.tables() {
		src := "select * from " + stmttest.Quote(name)
		got, want := run(tb, p.s4ql, callQuery, src, s4qlText), run(tb, p.sqlite, callQuery, src, mattnText)
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
		s, err := p.s4ql.Conn(tb.Context())
		is.NotError(tb, err)
		tb.Cleanup(func() { _ = s.Close() })
		p.s = s
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
		{callExec, "insert into t1 (v1) values (?)", ""},
		{callExec, "begin", ""},
		{callQuery, "select * from t1; select * from t1", ""},
		{callExec, "create table t2 (v2 integer primary key) strict, without rowid", ""},
		{callExec, `insert into t1 (v1) values ("x")`, ""},
		{callPrepareExec, "drop table if exists t1", "prepare, exec: prepare failed with s4ql: not supported: IF EXISTS (at offset 11)"},
		{callExec, "", "exec: exec failed with s4ql: not supported: a text that holds no statement"},
		{callPrepareQuery, " ; -- nothing", "prepare, query: prepare failed with s4ql: not supported: a text that holds no statement"},
		{callExecArgument, "insert into t1 (v1) values (1)", "exec with an argument: exec failed with s4ql: not supported: parameters"},
		{callBegin, "", "begin: begin failed with s4ql: not supported: transactions"},
		{callQuery, "select * from t1", `query: columns "v1" of types "INT", rows `},
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

// pick the call of a text of a sequence, which is a query if query, from r: now and then BeginTx or
// an argument, which s4ql does not support, and otherwise a call for a query or for a statement that
// is no query.
func pick(r stmttest.Rand, query bool) call {
	switch n := r.IntN(48); {
	case n == 0:
		return callBegin
	case n < 3:
		return callExecArgument
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
	if r.IntN(4) == 0 {
		return callPrepareExec
	}
	return callExec
}

// generated runs the sequence of n texts from seed on a new pair in the world w, and compares what
// each side gives for each, and at the end every table. Every second sequence runs on pinned
// connections, and every other one through the pool. Half of the sequences then restart the instance
// of s4ql, with an empty cache or with its own, and compare every table again.
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
		src, query, supported := q.Next()
		c := pick(r, query)
		if c == callBegin {
			src, supported = "", false
		}
		log = append(log, fmt.Sprintf("%v: %v", c, src))
		if _, failure := p.step(src, c, supported); failure != "" {
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
// each call, on both sides, and requires the same outcomes and the same tables. It fails if a kind of
// outcome is never reached, and it pins how many texts each kind of excuse lets through, so that a
// change that excuses more cannot pass unseen.
func TestGenerated(t *testing.T) {
	const n, length = 200, 40
	w := newWorld(t)
	y := tally{}
	for seed := range uint64(n) {
		generated(t, w, seed, length, y)
	}
	t.Logf("%v sequences of %v texts: %v", n, length, y)

	for _, kind := range []string{
		"createtable ok", "createtable already exists", "createtable no rows",
		"insert ok", "insert UNIQUE constraint failed", "insert has no column named", "insert no such table", "insert no rows", "insert not supported",
		"select ok", "select ok with rows", "select no such table", "select no rows",
		"droptable ok", "droptable no such table",
		"other not supported", "other syntax error", "other incomplete input", "begin not supported",
		"call " + string(callExec), "call " + string(callQuery), "call " + string(callPrepareExec), "call " + string(callPrepareQuery),
		"call " + string(callQueryRow), "call " + string(callExecArgument), "call " + string(callBegin),
	} {
		if y[kind] == 0 {
			t.Errorf("no text has the outcome %q", kind)
		}
	}

	want := map[string]int{excusedPrepare: 816, excusedStartedTable: 12, excusedArgumentSyntax: 3}
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

// FuzzSequence runs the texts of its input, each through the call that a byte of calls chooses, on a
// new pair, pinned if pinned, and compares as [TestGenerated] does. Its seeds are the sequences of
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

// TestExecArgument pins a difference from SQLite that [TestGenerated] found, which docs/todo.md
// records: a text with a syntax error, run with an argument, gets "not supported: parameters" from
// s4ql, and the syntax error from SQLite. The spec: "Where SQLite gives an error, s4ql gives the
// same result code, extended result code, and message. This holds [...] for text that is a syntax
// error in SQLite, also when it is SQL that s4ql does not support." When the driver checks the text
// before the arguments, this test fails, and the excuse of [excusedArgumentSyntax] goes.
func TestExecArgument(t *testing.T) {
	t.Run("should give not supported for a syntax error with an argument, where SQLite gives the syntax error", func(t *testing.T) {
		p := newPair(t, newWorld(t), true, nil)
		for src, want := range map[string]string{"drop table": "1/1: incomplete input", "selec": `1/1: near "selec": syntax error`} {
			is.Equal(t, "exec with an argument: exec failed with s4ql: not supported: parameters", run(t, p.s, callExecArgument, src, s4qlText).String())
			is.Equal(t, "exec with an argument: exec failed with "+want, run(t, p.o, callExecArgument, src, mattnText).String())
		}
		p.close()
	})
}
